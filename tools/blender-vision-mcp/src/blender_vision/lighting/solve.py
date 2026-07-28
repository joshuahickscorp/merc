"""Inverse lighting estimation from observations of known geometry."""

from __future__ import annotations

import math
import uuid
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import LightingHypothesis, LightingHypothesisSet, Lineage


@dataclass(slots=True)
class LightingObservation:
    view_id: str
    rgb: np.ndarray
    mask: np.ndarray | None = None
    normals: np.ndarray | None = None  # HxWx3 unit normals in camera/world frame
    authority: AuthorityClass = AuthorityClass.OBSERVED


@dataclass(slots=True)
class GeometryContext:
    """Known geometry used to anchor the lighting solve."""

    scene_id: str
    shape: str = "sphere"
    center: tuple[float, float, float] = (0.0, 0.0, 0.0)
    radius: float = 1.0
    authority: AuthorityClass = AuthorityClass.PROCEDURAL_GROUND_TRUTH
    metadata: dict[str, Any] = field(default_factory=dict)


def _to_rgb(image: np.ndarray) -> np.ndarray:
    arr = np.asarray(image, dtype=np.float64)
    if arr.max() > 1.0 + 1e-6:
        arr = arr / 255.0
    return np.clip(arr[..., :3], 0.0, 1.0)


def _luminance(rgb: np.ndarray) -> np.ndarray:
    return 0.2126 * rgb[..., 0] + 0.7152 * rgb[..., 1] + 0.0722 * rgb[..., 2]


def _sphere_normals(height: int, width: int) -> np.ndarray:
    y, x = np.mgrid[0:height, 0:width].astype(np.float64)
    nx = (x + 0.5) / width * 2.0 - 1.0
    ny = 1.0 - (y + 0.5) / height * 2.0
    r2 = nx * nx + ny * ny
    nz = np.sqrt(np.clip(1.0 - r2, 0.0, 1.0))
    normals = np.stack([nx, ny, nz], axis=-1)
    mask = r2 <= 1.0
    normals[~mask] = 0.0
    return normals


def _estimate_key_direction(
    rgb: np.ndarray, normals: np.ndarray, mask: np.ndarray
) -> tuple[np.ndarray, float]:
    """Key direction from brightest shading gradient / Lambert fit on known normals."""
    lum = _luminance(rgb)
    valid = mask & (np.linalg.norm(normals, axis=-1) > 0.5)
    if not np.any(valid):
        return np.array([0.45, 0.65, 0.6]), 0.1
    # Weighted average of normals by luminance approximates light direction for Lambert.
    weights = np.clip(lum - np.median(lum[valid]), 0.0, None)
    weights = weights * valid
    if float(weights.sum()) < 1e-8:
        weights = valid.astype(np.float64)
    direction = (normals * weights[..., None]).sum(axis=(0, 1))
    norm = float(np.linalg.norm(direction))
    if norm < 1e-8:
        return np.array([0.45, 0.65, 0.6]), 0.15
    direction = direction / norm
    # Specular highlight peak as second cue when available.
    highlight = valid & (lum >= np.percentile(lum[valid], 97))
    if np.any(highlight):
        n_h = normals[highlight].mean(axis=0)
        n_h = n_h / (np.linalg.norm(n_h) + 1e-8)
        # For view ~ +Z, light ≈ reflect(view, n) ≈ 2(n·v)n - v
        view = np.array([0.0, 0.0, 1.0])
        light_from_spec = 2.0 * float(np.dot(n_h, view)) * n_h - view
        light_from_spec = light_from_spec / (np.linalg.norm(light_from_spec) + 1e-8)
        direction = 0.55 * direction + 0.45 * light_from_spec
        direction = direction / (np.linalg.norm(direction) + 1e-8)
    peak = weights.max() / (weights.mean() + 1e-8) / 8.0
    confidence = float(np.clip(0.4 + 0.5 * peak, 0.2, 0.95))
    return direction, confidence


def _estimate_environment(rgb: np.ndarray, mask: np.ndarray) -> tuple[np.ndarray, float]:
    lum = _luminance(rgb)
    ambient_mask = mask & (lum <= np.percentile(lum[mask], 25)) if mask.any() else mask
    if not np.any(ambient_mask):
        ambient_mask = mask
    color = rgb[ambient_mask].mean(axis=0) if np.any(ambient_mask) else np.array([0.2, 0.2, 0.2])
    strength = float(np.clip(np.mean(lum[ambient_mask]) * 2.5, 0.02, 2.0))
    return color, strength


def _estimate_exposure(rgb: np.ndarray, mask: np.ndarray) -> float:
    lum = _luminance(rgb)[mask]
    if lum.size == 0:
        return 0.0
    mid = float(np.median(lum))
    # Map mid-grey ~0.18 to exposure EV.
    if mid < 1e-6:
        return -2.0
    return float(np.clip(math.log2(0.18 / mid), -3.0, 3.0))


def _estimate_white_balance_k(rgb: np.ndarray, mask: np.ndarray) -> float:
    pixels = rgb[mask]
    if pixels.size == 0:
        return 6500.0
    mean = pixels.mean(axis=0)
    # Simple CCT proxy from R/B ratio.
    rb = float(mean[0] / max(mean[2], 1e-6))
    # rb>1 warmer (lower K), rb<1 cooler (higher K)
    kelvin = 6500.0 - 1500.0 * (rb - 1.0)
    return float(np.clip(kelvin, 2500.0, 10000.0))


def _direction_to_spherical(direction: np.ndarray) -> dict[str, float]:
    x, y, z = (float(v) for v in direction)
    azimuth = math.degrees(math.atan2(x, z))
    elevation = math.degrees(math.asin(np.clip(y, -1.0, 1.0)))
    return {
        "direction": [x, y, z],
        "azimuth_deg": azimuth,
        "elevation_deg": elevation,
    }


def solve_lighting(
    observations: list[LightingObservation],
    geometry: GeometryContext,
) -> LightingHypothesisSet:
    """Estimate key direction/size, environment, exposure, and white balance.

    Emits competing hypotheses when multi-view cues disagree.
    """
    if not observations:
        raise ValueError("solve_lighting requires observations")

    input_authorities = [obs.authority for obs in observations] + [geometry.authority]
    authority = derive(input_authorities, proposed=AuthorityClass.INFERRED)

    directions: list[np.ndarray] = []
    confidences: list[float] = []
    env_colors: list[np.ndarray] = []
    env_strengths: list[float] = []
    exposures: list[float] = []
    kelvins: list[float] = []

    for obs in observations:
        rgb = _to_rgb(obs.rgb)
        h, w = rgb.shape[:2]
        if obs.normals is not None:
            normals = np.asarray(obs.normals, dtype=np.float64)
        else:
            normals = _sphere_normals(h, w)
        if obs.mask is not None:
            mask = np.asarray(obs.mask).astype(bool)
        else:
            mask = np.linalg.norm(normals, axis=-1) > 0.5
        direction, conf = _estimate_key_direction(rgb, normals, mask)
        directions.append(direction)
        confidences.append(conf)
        color, strength = _estimate_environment(rgb, mask)
        env_colors.append(color)
        env_strengths.append(strength)
        exposures.append(_estimate_exposure(rgb, mask))
        kelvins.append(_estimate_white_balance_k(rgb, mask))

    mean_dir = np.mean(np.stack(directions, axis=0), axis=0)
    mean_dir = mean_dir / (np.linalg.norm(mean_dir) + 1e-8)
    dir_dispersion = float(
        np.mean(
            [
                math.degrees(math.acos(np.clip(float(np.dot(d, mean_dir)), -1.0, 1.0)))
                for d in directions
            ]
        )
    )
    dispersion_factor = float(np.clip(1.0 - dir_dispersion / 60.0, 0.2, 1.0))
    primary_conf = float(np.mean(confidences)) * dispersion_factor

    env_color = np.mean(np.stack(env_colors, axis=0), axis=0)
    env_strength = float(np.mean(env_strengths))
    exposure = float(np.mean(exposures))
    white_balance = float(np.mean(kelvins))
    key_size = float(np.clip(0.2 + dir_dispersion / 90.0, 0.15, 1.5))

    primary = LightingHypothesis(
        hypothesis_id=f"light-{uuid.uuid4().hex[:10]}",
        rig_class="solved",
        key={
            **_direction_to_spherical(mean_dir),
            "size": key_size,
            "intensity": float(np.clip(1.5 - exposure, 0.2, 5.0)),
            "color": [1.0, 0.98, 0.95],
        },
        fill={"intensity": 0.35, "color": list(env_color)},
        negative_fill={"intensity": 0.1},
        rim={"intensity": 0.25},
        environment={
            "color": [float(x) for x in env_color],
            "strength": env_strength,
        },
        shadow_softness=float(np.clip(key_size * 0.4, 0.1, 0.8)),
        exposure=exposure,
        white_balance_k=white_balance,
        tone_map="AgX",
        confidence=primary_conf,
        authority=authority,
    )
    hypotheses = [primary]

    # Competing hypothesis when direction is underdetermined.
    if dir_dispersion > 18.0 or primary_conf < 0.55:
        alt_dir = directions[int(np.argmax(confidences))]
        alt = LightingHypothesis(
            hypothesis_id=f"light-{uuid.uuid4().hex[:10]}",
            rig_class="solved-alternate",
            key={
                **_direction_to_spherical(alt_dir),
                "size": key_size * 1.2,
                "intensity": float(primary.key["intensity"]) * 0.9,
                "color": [1.0, 0.98, 0.95],
            },
            fill=dict(primary.fill),
            negative_fill=dict(primary.negative_fill),
            rim=dict(primary.rim),
            environment=dict(primary.environment),
            shadow_softness=primary.shadow_softness,
            exposure=exposure,
            white_balance_k=white_balance,
            tone_map="AgX",
            confidence=primary_conf * 0.85,
            authority=authority,
        )
        hypotheses.append(alt)

    selected = max(hypotheses, key=lambda item: item.confidence)
    record = LightingHypothesisSet(
        id=f"lhs-{uuid.uuid4().hex[:12]}",
        scene_id=geometry.scene_id,
        hypotheses=hypotheses,
        selected_hypothesis_id=selected.hypothesis_id,
        authority=authority,
        lineage=Lineage(
            operation="solve_lighting",
            inputs=[obs.view_id for obs in observations] + [geometry.scene_id],
            input_authorities=[item.value for item in input_authorities],
            parameters={
                "shape": geometry.shape,
                "view_count": len(observations),
                "direction_dispersion_deg": dir_dispersion,
            },
            limitations=[
                "Lambert + specular-highlight cue on known normals; not full inverse path tracing.",
            ],
        ),
        uncertainty=Uncertainty(
            kind="lighting-direction",
            sigma=dir_dispersion,
            units=Units.DEGREE,
            basis="multi-view key-direction dispersion",
            samples=len(observations),
        ),
    )
    return record.seal()


def angular_error_deg(estimated: list[float] | np.ndarray, true: list[float] | np.ndarray) -> float:
    a = np.asarray(estimated, dtype=np.float64)
    b = np.asarray(true, dtype=np.float64)
    na = float(np.linalg.norm(a))
    nb = float(np.linalg.norm(b))
    if na < 1e-12 or nb < 1e-12:
        return 180.0
    a = a / na
    b = b / nb
    return float(math.degrees(math.acos(np.clip(float(np.dot(a, b)), -1.0, 1.0))))
