"""Inverse material estimation from multi-view surface observations.

Solves base colour, roughness, metalness, specular/IOR, anisotropy, transmission,
and subsurface from observations of known surface regions. Underdetermined cases
emit competing hypotheses rather than a single averaged guess.
"""

from __future__ import annotations

import math
import uuid
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import Lineage, MaterialHypothesis, MaterialHypothesisSet


@dataclass(slots=True)
class SurfaceObservation:
    """One view of a surface region with optional highlight mask."""

    view_id: str
    rgb: np.ndarray
    mask: np.ndarray | None = None
    highlight_mask: np.ndarray | None = None
    view_direction: tuple[float, float, float] = (0.0, 0.0, 1.0)
    light_direction: tuple[float, float, float] | None = None
    authority: AuthorityClass = AuthorityClass.OBSERVED

    def __post_init__(self) -> None:
        arr = np.asarray(self.rgb, dtype=np.float64)
        if arr.ndim != 3 or arr.shape[2] < 3:
            raise ValueError("observation rgb must be HxWx3+")
        if arr.max() > 1.0 + 1e-6:
            arr = arr / 255.0
        self.rgb = np.clip(arr[..., :3], 0.0, 1.0)
        if self.mask is not None:
            self.mask = np.asarray(self.mask).astype(bool)
            if self.mask.shape[:2] != self.rgb.shape[:2]:
                raise ValueError("mask spatial shape must match rgb")
        if self.highlight_mask is not None:
            self.highlight_mask = np.asarray(self.highlight_mask).astype(bool)


@dataclass(slots=True)
class SurfaceRegion:
    """A surface patch with stable identity for material estimation."""

    surface_id: str
    label: str = ""
    normal: tuple[float, float, float] = (0.0, 0.0, 1.0)
    known_geometry: bool = True
    metadata: dict[str, Any] = field(default_factory=dict)


def _region_pixels(obs: SurfaceObservation) -> np.ndarray:
    if obs.mask is None:
        return obs.rgb.reshape(-1, 3)
    return obs.rgb[obs.mask]


def _highlight_pixels(obs: SurfaceObservation) -> np.ndarray:
    if obs.highlight_mask is not None:
        return obs.rgb[obs.highlight_mask]
    pixels = _region_pixels(obs)
    if pixels.size == 0:
        return pixels
    luminance = 0.2126 * pixels[:, 0] + 0.7152 * pixels[:, 1] + 0.0722 * pixels[:, 2]
    threshold = max(float(np.percentile(luminance, 92)), 0.55)
    return pixels[luminance >= threshold]


def _diffuse_pixels(obs: SurfaceObservation) -> np.ndarray:
    pixels = _region_pixels(obs)
    if pixels.size == 0:
        return pixels
    if obs.highlight_mask is not None:
        mask = obs.mask if obs.mask is not None else np.ones(obs.rgb.shape[:2], dtype=bool)
        keep = mask & ~obs.highlight_mask
        if keep.any():
            return obs.rgb[keep]
    luminance = 0.2126 * pixels[:, 0] + 0.7152 * pixels[:, 1] + 0.0722 * pixels[:, 2]
    threshold = float(np.percentile(luminance, 80))
    return pixels[luminance <= threshold]


def _median_chromaticity(pixels: np.ndarray) -> np.ndarray:
    if pixels.size == 0:
        return np.array([0.5, 0.5, 0.5], dtype=np.float64)
    rgb = np.median(pixels, axis=0)
    total = float(np.sum(rgb))
    if total < 1e-8:
        return np.array([0.0, 0.0, 0.0], dtype=np.float64)
    return rgb


def _chromaticity(rgb: np.ndarray) -> np.ndarray:
    total = float(np.sum(rgb)) + 1e-8
    return rgb / total


def _specular_tint_ratio(diffuse_rgb: np.ndarray, specular_rgb: np.ndarray) -> float:
    """Metals tint specular toward base colour; dielectrics keep neutral specular."""
    d = _chromaticity(diffuse_rgb)
    s = _chromaticity(specular_rgb)
    neutral = np.array([1.0 / 3.0, 1.0 / 3.0, 1.0 / 3.0])
    tint_distance = float(np.linalg.norm(s - neutral))
    base_distance = float(np.linalg.norm(d - neutral))
    if base_distance < 0.02:
        # Near-neutral base: use raw specular colour saturation.
        return float(np.clip(tint_distance * 4.0, 0.0, 1.0))
    alignment = float(np.dot(s - neutral, d - neutral) / (base_distance * max(tint_distance, 1e-6)))
    return float(np.clip(0.5 * (alignment + 1.0) * min(1.0, tint_distance / 0.08), 0.0, 1.0))


def _roughness_from_highlight_spread(observations: list[SurfaceObservation]) -> float:
    spreads: list[float] = []
    for obs in observations:
        hl = _highlight_pixels(obs)
        if hl.shape[0] < 4:
            continue
        luminance = 0.2126 * hl[:, 0] + 0.7152 * hl[:, 1] + 0.0722 * hl[:, 2]
        # Broader highlight support => higher roughness.
        support = float(hl.shape[0]) / max(1, _region_pixels(obs).shape[0])
        variance = float(np.var(luminance))
        spreads.append(float(np.clip(0.15 + 2.5 * support + 0.8 * variance, 0.02, 0.98)))
    if not spreads:
        return 0.5
    return float(np.median(spreads))


def _anisotropy_from_highlights(observations: list[SurfaceObservation]) -> float:
    elongations: list[float] = []
    for obs in observations:
        if obs.highlight_mask is None:
            continue
        ys, xs = np.nonzero(obs.highlight_mask)
        if len(xs) < 8:
            continue
        coords = np.column_stack([xs.astype(np.float64), ys.astype(np.float64)])
        centered = coords - coords.mean(axis=0)
        cov = centered.T @ centered / max(1, len(centered) - 1)
        evals = np.linalg.eigvalsh(cov)
        if evals[-1] < 1e-8:
            continue
        ratio = float(evals[-1] / max(evals[0], 1e-8))
        elongations.append(float(np.clip((math.sqrt(ratio) - 1.0) / 4.0, 0.0, 1.0)))
    if not elongations:
        return 0.0
    return float(np.median(elongations))


def _transmission_proxy(observations: list[SurfaceObservation]) -> float:
    # Bright, low-saturation regions with high min-channel suggest glass-like transmission.
    scores: list[float] = []
    for obs in observations:
        pixels = _region_pixels(obs)
        if pixels.size == 0:
            continue
        mins = pixels.min(axis=1)
        maxs = pixels.max(axis=1)
        sat = (maxs - mins) / (maxs + 1e-8)
        score = float(np.mean((mins > 0.35) & (sat < 0.15)))
        scores.append(score)
    if not scores:
        return 0.0
    return float(np.clip(np.mean(scores) * 1.4, 0.0, 1.0))


def _subsurface_proxy(observations: list[SurfaceObservation]) -> float:
    # Soft colour bleed into shadow side: elevated dark-region chromaticity vs midtones.
    scores: list[float] = []
    for obs in observations:
        pixels = _region_pixels(obs)
        if pixels.shape[0] < 16:
            continue
        lum = 0.2126 * pixels[:, 0] + 0.7152 * pixels[:, 1] + 0.0722 * pixels[:, 2]
        dark = pixels[lum < np.percentile(lum, 25)]
        mid = pixels[(lum >= np.percentile(lum, 35)) & (lum <= np.percentile(lum, 65))]
        if dark.size == 0 or mid.size == 0:
            continue
        dark_chroma = _chromaticity(np.median(dark, axis=0))
        mid_chroma = _chromaticity(np.median(mid, axis=0))
        scores.append(float(np.linalg.norm(dark_chroma - mid_chroma)))
    if not scores:
        return 0.0
    return float(np.clip(np.mean(scores) * 6.0, 0.0, 1.0))


def _estimate_metalness(
    diffuse_rgb: np.ndarray, specular_rgb: np.ndarray, confidence_boost: float = 0.0
) -> tuple[float, float]:
    tint = _specular_tint_ratio(diffuse_rgb, specular_rgb)
    # High specular luminance relative to diffuse also supports metal.
    d_lum = float(np.dot(diffuse_rgb, [0.2126, 0.7152, 0.0722]))
    s_lum = float(np.dot(specular_rgb, [0.2126, 0.7152, 0.0722]))
    ratio = s_lum / max(d_lum, 1e-4)
    metal_from_ratio = float(np.clip((ratio - 0.8) / 2.5, 0.0, 1.0))
    metalness = float(np.clip(0.65 * tint + 0.35 * metal_from_ratio, 0.0, 1.0))
    confidence = float(np.clip(0.35 + abs(metalness - 0.5) * 0.9 + confidence_boost, 0.0, 0.98))
    return metalness, confidence


def _competing_metal_hypotheses(
    diffuse_rgb: np.ndarray,
    specular_rgb: np.ndarray,
    base: MaterialHypothesis,
    view_ids: list[str],
    authority: AuthorityClass,
) -> list[MaterialHypothesis]:
    metalness, conf = _estimate_metalness(diffuse_rgb, specular_rgb)
    # Ambiguous mid-range: emit metal and dielectric competitors.
    if 0.25 <= metalness <= 0.75 and conf < 0.85:
        metal = MaterialHypothesis(
            hypothesis_id=f"{base.hypothesis_id}-metal",
            label=f"{base.label}-metal",
            base_colour=list(base.base_colour),
            roughness=base.roughness,
            metalness=float(np.clip(max(metalness, 0.75), 0.0, 1.0)),
            specular_ior=base.specular_ior,
            anisotropy=base.anisotropy,
            transmission=0.0,
            subsurface=base.subsurface,
            confidence=conf * 0.9,
            evidence_views=list(view_ids),
            authority=authority,
        )
        dielectric = MaterialHypothesis(
            hypothesis_id=f"{base.hypothesis_id}-dielectric",
            label=f"{base.label}-dielectric",
            base_colour=list(base.base_colour),
            roughness=base.roughness,
            metalness=float(np.clip(min(metalness, 0.15), 0.0, 1.0)),
            specular_ior=base.specular_ior,
            anisotropy=base.anisotropy,
            transmission=base.transmission,
            subsurface=base.subsurface,
            confidence=conf * 0.85,
            evidence_views=list(view_ids),
            authority=authority,
        )
        return [metal, dielectric]
    base.metalness = metalness
    base.confidence = conf
    return [base]


def infer_materials(
    observations: list[SurfaceObservation],
    surfaces: list[SurfaceRegion],
    *,
    surface_id: str | None = None,
) -> MaterialHypothesisSet:
    """Estimate material parameters for each surface region.

    Returns a sealed MaterialHypothesisSet. Authority is derived from observation
    authorities and never hand-promoted above the weakest input.
    """
    if not observations:
        raise ValueError("infer_materials requires at least one observation")
    if not surfaces:
        raise ValueError("infer_materials requires at least one surface region")

    target = surfaces[0] if surface_id is None else next(
        (item for item in surfaces if item.surface_id == surface_id), surfaces[0]
    )
    input_authorities = [obs.authority for obs in observations]
    authority = derive(input_authorities, proposed=AuthorityClass.INFERRED)
    view_ids = [obs.view_id for obs in observations]

    diffuse_stack = [_median_chromaticity(_diffuse_pixels(obs)) for obs in observations]
    specular_stack = [_median_chromaticity(_highlight_pixels(obs)) for obs in observations]
    diffuse_rgb = np.median(np.stack(diffuse_stack, axis=0), axis=0)
    specular_rgb = np.median(np.stack(specular_stack, axis=0), axis=0)

    roughness = _roughness_from_highlight_spread(observations)
    anisotropy = _anisotropy_from_highlights(observations)
    transmission = _transmission_proxy(observations)
    subsurface = _subsurface_proxy(observations)

    # Specular IOR: dielectrics cluster near 1.45; metals use conductor-like 1.0 response.
    specular_ior = 1.45
    if float(np.dot(specular_rgb, [0.2126, 0.7152, 0.0722])) > 0.65:
        specular_ior = 1.33 + 0.6 * float(np.clip(specular_rgb.mean(), 0.0, 1.0))

    base = MaterialHypothesis(
        hypothesis_id=f"mat-{uuid.uuid4().hex[:10]}",
        label=target.label or target.surface_id,
        base_colour=[float(x) for x in diffuse_rgb],
        roughness=roughness,
        metalness=0.0,
        specular_ior=float(specular_ior),
        anisotropy=anisotropy,
        transmission=transmission,
        subsurface=subsurface,
        confidence=0.5,
        evidence_views=list(view_ids),
        authority=authority,
    )
    hypotheses = _competing_metal_hypotheses(
        diffuse_rgb, specular_rgb, base, view_ids, authority
    )
    if len(hypotheses) == 1:
        metalness, conf = _estimate_metalness(diffuse_rgb, specular_rgb)
        hypotheses[0].metalness = metalness
        hypotheses[0].confidence = conf

    selected = max(hypotheses, key=lambda item: item.confidence)
    record = MaterialHypothesisSet(
        id=f"mhs-{uuid.uuid4().hex[:12]}",
        surface_id=target.surface_id,
        hypotheses=hypotheses,
        selected_hypothesis_id=selected.hypothesis_id,
        authority=authority,
        lineage=Lineage(
            operation="infer_materials",
            inputs=view_ids,
            input_authorities=[item.value for item in input_authorities],
            parameters={"surface_id": target.surface_id, "view_count": len(observations)},
            limitations=[
                "Classical multi-view dichromatic inversion; not a full path-traced inverse.",
            ],
        ),
        uncertainty=Uncertainty(
            kind="material-parameter",
            sigma=float(1.0 - selected.confidence),
            units=Units.UNITLESS,
            basis="per-parameter multi-view dispersion",
            samples=len(observations),
        ),
    )
    return record.seal()
