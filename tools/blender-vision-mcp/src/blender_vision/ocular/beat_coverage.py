"""Per-beat coverage receipts for the flagship data-centre film (Bible 25.15).

A high global instance count must never stand in for per-beat coverage. Each
narrative beat declares minimums up front; coverage is measured from a real
render at that beat's camera plus a frustum cull of the scene, then sealed as
an ``ocular.beat-coverage`` receipt.

Geometry in the frustum that the render does not show is a failure: the scene
graph is not a substitute for what the lens actually captures.
"""

from __future__ import annotations

import math
from collections.abc import Mapping, Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any, ClassVar

import numpy as np
from numpy.typing import NDArray

from blender_vision.cinematic.path import FLAGSHIP_BEAT_SPECS
from blender_vision.cinematic.textsafe import ZONE_RECTS, TextZone, evaluate_text_safe
from blender_vision.core.errors import ValidationError
from blender_vision.core.util import sha256_file, utc_now
from blender_vision.procedural.grammar import InstanceRef
from blender_vision.procedural.library import ArchetypeLibrary, default_library
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    derive,
)
from blender_vision.v2.records import Lineage, V2Record

# Drawer archetypes counted as "visible drawers" for coverage gates.
DRAWER_ARCHETYPES: frozenset[str] = frozenset(
    {"gpu_drawer", "server_drawer", "switch", "blanking_panel"}
)
RACK_ARCHETYPES: frozenset[str] = frozenset({"rack_shell"})

# Default horizontal FOV for EEVEE preview camera (35 mm on ~36 mm sensor).
_DEFAULT_FOCAL_MM = 35.0
_DEFAULT_SENSOR_MM = 36.0


class DepthBand(StrEnum):
    FOREGROUND = "foreground"
    MIDGROUND = "midground"
    BACKGROUND = "background"


@dataclass(slots=True)
class BeatMinimums:
    """Declared per-beat gates. Failures are measured, never assumed."""

    min_visible_instances: int = 4
    min_visible_racks: int = 1
    min_visible_drawers: int = 2
    min_non_background_fraction: float = 0.12
    min_depth_spread: float = 0.35
    min_text_safe_contrast: float = 3.0
    min_frustum_instances: int = 4

    def to_dict(self) -> dict[str, Any]:
        return {
            "min_visible_instances": self.min_visible_instances,
            "min_visible_racks": self.min_visible_racks,
            "min_visible_drawers": self.min_visible_drawers,
            "min_non_background_fraction": self.min_non_background_fraction,
            "min_depth_spread": self.min_depth_spread,
            "min_text_safe_contrast": self.min_text_safe_contrast,
            "min_frustum_instances": self.min_frustum_instances,
        }

    @classmethod
    def from_dict(cls, payload: Mapping[str, Any]) -> BeatMinimums:
        known = {f.name for f in cls.__dataclass_fields__.values()}  # type: ignore[attr-defined]
        return cls(**{k: payload[k] for k in payload if k in known})


def flagship_beat_minimums(beat_id: str) -> BeatMinimums:
    """Per-beat minimums for the nine flagship narrative beats.

    Post-turn beats (05–08) require real second-aisle geometry. Pre-turn beats
    require the main corridor to actually fill the frame. Global scene counts
    are intentionally absent: a beat cannot pass on the other aisle's density.
    """
    # Shared floor for every beat: the frame must not be an empty box.
    base = BeatMinimums(
        min_visible_instances=6,
        min_visible_racks=2,
        min_visible_drawers=6,
        min_non_background_fraction=0.18,
        min_depth_spread=0.5,
        min_text_safe_contrast=3.0,
        min_frustum_instances=8,
    )
    overrides: dict[str, BeatMinimums] = {
        "00": BeatMinimums(
            min_visible_instances=3,
            min_visible_racks=0,
            min_visible_drawers=0,
            min_non_background_fraction=0.10,
            min_depth_spread=0.4,
            min_text_safe_contrast=3.0,
            min_frustum_instances=2,
        ),
        "01": base,
        "02": base,
        "03": base,
        "04": base,
        "05": BeatMinimums(
            min_visible_instances=8,
            min_visible_racks=2,
            min_visible_drawers=8,
            min_non_background_fraction=0.16,
            min_depth_spread=0.55,
            min_text_safe_contrast=3.0,
            min_frustum_instances=10,
        ),
        "06": BeatMinimums(
            min_visible_instances=10,
            min_visible_racks=3,
            min_visible_drawers=12,
            min_non_background_fraction=0.20,
            min_depth_spread=0.6,
            min_text_safe_contrast=3.0,
            min_frustum_instances=12,
        ),
        "07": BeatMinimums(
            min_visible_instances=8,
            min_visible_racks=2,
            min_visible_drawers=8,
            min_non_background_fraction=0.18,
            min_depth_spread=0.45,
            min_text_safe_contrast=3.5,
            min_frustum_instances=10,
        ),
        "08": BeatMinimums(
            min_visible_instances=6,
            min_visible_racks=1,
            min_visible_drawers=4,
            min_non_background_fraction=0.15,
            min_depth_spread=0.35,
            min_text_safe_contrast=3.5,
            min_frustum_instances=8,
        ),
    }
    if beat_id not in overrides:
        raise ValidationError(f"unknown flagship beat_id {beat_id!r}")
    return overrides[beat_id]


@dataclass(slots=True, kw_only=True)
class BeatCoverageReceipt(V2Record):
    """Sealed per-beat coverage measurement. Bible section 25.15."""

    RECORD_KIND: ClassVar[str] = "ocular.beat-coverage"

    beat_id: str = ""
    beat_label: str = ""
    scroll: float = 0.0
    camera_position: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])
    camera_target: list[float] = field(default_factory=lambda: [0.0, 1.0, 0.0])
    focal_length_mm: float = _DEFAULT_FOCAL_MM
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name=BLENDER_WORLD.name,
            up_axis=BLENDER_WORLD.up_axis,
            forward_axis=BLENDER_WORLD.forward_axis,
            scale_authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        )
    )

    # Declared minimums (the contract, not the measurement).
    minimums: dict[str, Any] = field(default_factory=dict)

    # Frustum-side counts (scene graph at this camera).
    frustum_instance_count: int = 0
    frustum_rack_count: int = 0
    frustum_drawer_count: int = 0
    frustum_instance_ids: list[str] = field(default_factory=list)

    # Render-side measurements (pixels + confirmed visibility).
    visible_instances: int = 0
    visible_racks: int = 0
    visible_drawers: int = 0
    foreground_occupancy: float = 0.0
    midground_occupancy: float = 0.0
    background_occupancy: float = 0.0
    non_background_pixel_fraction: float = 0.0
    depth_spread: float = 0.0
    luminance_histogram: list[float] = field(default_factory=list)
    text_safe_zone: str = ""
    text_safe_area: list[float] = field(default_factory=list)
    text_safe_contrast: float = 0.0
    text_safe_readable: bool = False
    light_key_fill_ratio: float = 0.0
    light_rim_ratio: float = 0.0
    focal_subject: str = ""
    occlusion_fraction: float = 0.0
    performance_ms: float = 0.0

    # Cross-check: geometry claimed by the frustum but absent from the render.
    frustum_render_mismatch: bool = False
    passed: bool = False
    failures: list[str] = field(default_factory=list)

    render_path: str = ""
    render_digest: str = ""
    attestation_id: str = ""
    execution_class: str = ""

    def evaluate_gates(self) -> list[str]:
        """Compare measured fields to declared minimums. Returns failure strings."""
        mins = BeatMinimums.from_dict(self.minimums) if self.minimums else BeatMinimums()
        failures: list[str] = []
        if self.frustum_instance_count < mins.min_frustum_instances:
            failures.append(
                f"frustum_instances {self.frustum_instance_count} < "
                f"{mins.min_frustum_instances}"
            )
        if self.visible_instances < mins.min_visible_instances:
            failures.append(
                f"visible_instances {self.visible_instances} < {mins.min_visible_instances}"
            )
        if self.visible_racks < mins.min_visible_racks:
            failures.append(
                f"visible_racks {self.visible_racks} < {mins.min_visible_racks}"
            )
        if self.visible_drawers < mins.min_visible_drawers:
            failures.append(
                f"visible_drawers {self.visible_drawers} < {mins.min_visible_drawers}"
            )
        if self.non_background_pixel_fraction < mins.min_non_background_fraction:
            failures.append(
                f"non_background_fraction {self.non_background_pixel_fraction:.4f} < "
                f"{mins.min_non_background_fraction:.4f}"
            )
        if self.depth_spread < mins.min_depth_spread:
            failures.append(
                f"depth_spread {self.depth_spread:.4f} < {mins.min_depth_spread:.4f}"
            )
        if self.text_safe_contrast < mins.min_text_safe_contrast:
            failures.append(
                f"text_safe_contrast {self.text_safe_contrast:.3f} < "
                f"{mins.min_text_safe_contrast:.3f}"
            )
        # Geometry in frustum but empty render is the exact defect this receipt exists to catch.
        if (
            self.frustum_instance_count >= mins.min_frustum_instances
            and self.non_background_pixel_fraction < 0.05
        ):
            failures.append(
                "frustum_render_mismatch: geometry in frustum but render is empty/near-empty"
            )
            self.frustum_render_mismatch = True
        if (
            self.frustum_rack_count >= mins.min_visible_racks
            and self.visible_racks == 0
            and mins.min_visible_racks > 0
        ):
            failures.append(
                "frustum_render_mismatch: racks in frustum but none confirmed visible"
            )
            self.frustum_render_mismatch = True
        return failures

    def apply_gates(self) -> BeatCoverageReceipt:
        self.failures = self.evaluate_gates()
        self.passed = len(self.failures) == 0
        self.digest = ""
        return self


@dataclass(slots=True)
class FrustumHit:
    instance_id: str
    archetype: str
    depth_m: float
    band: DepthBand
    tags: list[str] = field(default_factory=list)


def _look_basis(
    position: Sequence[float],
    target: Sequence[float],
    up: Sequence[float] = (0.0, 0.0, 1.0),
) -> tuple[NDArray[np.float64], NDArray[np.float64], NDArray[np.float64]]:
    """Return camera (right, up, forward) axes in Blender world (+Z up)."""
    pos = np.asarray(position, dtype=np.float64)
    tgt = np.asarray(target, dtype=np.float64)
    world_up = np.asarray(up, dtype=np.float64)
    forward = tgt - pos
    norm = float(np.linalg.norm(forward))
    forward = (
        np.array([0.0, 1.0, 0.0], dtype=np.float64) if norm < 1e-9 else forward / norm
    )
    right = np.cross(forward, world_up)
    r_norm = float(np.linalg.norm(right))
    right = (
        np.array([1.0, 0.0, 0.0], dtype=np.float64) if r_norm < 1e-9 else right / r_norm
    )
    cam_up = np.cross(right, forward)
    cam_up = cam_up / max(float(np.linalg.norm(cam_up)), 1e-12)
    return right, cam_up, forward


def frustum_cull_instances(
    instances: Sequence[InstanceRef],
    *,
    camera_position: Sequence[float],
    camera_target: Sequence[float],
    focal_length_mm: float = _DEFAULT_FOCAL_MM,
    sensor_width_mm: float = _DEFAULT_SENSOR_MM,
    aspect: float = 16.0 / 9.0,
    near_m: float = 0.15,
    far_m: float = 40.0,
    library: ArchetypeLibrary | None = None,
    margin: float = 0.15,
) -> list[FrustumHit]:
    """Cull instance AABBs against the beat camera frustum (Blender Z-up).

    Uses declared archetype dimensions as a conservative AABB (rotation ignored
    for the envelope — same compromise as scene bounds). An instance counts as
    in-frustum when its centre projects inside the expanded NDC bounds and its
    depth is within [near, far].
    """
    library = library or default_library()
    right, cam_up, forward = _look_basis(camera_position, camera_target)
    pos = np.asarray(camera_position, dtype=np.float64)
    hfov = 2.0 * math.atan((sensor_width_mm * 0.5) / max(focal_length_mm, 1e-6))
    vfov = 2.0 * math.atan(math.tan(hfov * 0.5) / aspect)
    tan_h = math.tan(hfov * 0.5) * (1.0 + margin)
    tan_v = math.tan(vfov * 0.5) * (1.0 + margin)

    hits: list[FrustumHit] = []
    for inst in instances:
        centre = np.asarray(inst.transform.location, dtype=np.float64)
        try:
            dims = library.create(inst.archetype, inst.params).declared_dimensions()
            radius = 0.5 * math.sqrt(
                dims.width_m**2 + dims.depth_m**2 + dims.height_m**2
            )
        except (ValidationError, KeyError, TypeError, ValueError):
            radius = 0.4
        offset = centre - pos
        depth = float(np.dot(offset, forward))
        if depth < near_m - radius or depth > far_m + radius:
            continue
        # Soft near plane: centres behind the lens never count.
        if depth <= 1e-4:
            continue
        x = float(np.dot(offset, right))
        y = float(np.dot(offset, cam_up))
        # Expand acceptance by projected radius so large racks near the edge count.
        limit_x = tan_h * depth + radius
        limit_y = tan_v * depth + radius
        if abs(x) > limit_x or abs(y) > limit_y:
            continue
        if depth < 3.0:
            band = DepthBand.FOREGROUND
        elif depth < 9.0:
            band = DepthBand.MIDGROUND
        else:
            band = DepthBand.BACKGROUND
        hits.append(
            FrustumHit(
                instance_id=inst.instance_id,
                archetype=inst.archetype,
                depth_m=depth,
                band=band,
                tags=list(inst.tags),
            )
        )
    return hits


def measure_frame_pixels(
    frame: NDArray[np.floating] | NDArray[np.integer],
    *,
    background_luminance_ceiling: float = 0.08,
) -> dict[str, Any]:
    """Pixel statistics from a rendered beat frame. Never invents values."""
    array = np.asarray(frame)
    if array.ndim == 2:
        rgb = np.stack([array, array, array], axis=-1)
    elif array.ndim == 3 and array.shape[2] >= 3:
        rgb = array[..., :3]
    else:
        raise ValidationError("frame must be HxW or HxWxC")
    sample = rgb.astype(np.float64)
    if sample.max() > 1.0:
        sample = sample / 255.0
    # Rec. 709 luminance on linearised sRGB.
    linear = np.where(sample <= 0.04045, sample / 12.92, ((sample + 0.055) / 1.055) ** 2.4)
    lum = 0.2126 * linear[..., 0] + 0.7152 * linear[..., 1] + 0.0722 * linear[..., 2]
    non_bg = lum > background_luminance_ceiling
    non_bg_frac = float(np.mean(non_bg))
    hist, _ = np.histogram(lum.ravel(), bins=16, range=(0.0, 1.0), density=False)
    hist_f = hist.astype(np.float64)
    if hist_f.sum() > 0:
        hist_f = hist_f / hist_f.sum()
    # Light hierarchy proxy from luminance terciles of non-background pixels.
    if np.any(non_bg):
        vals = lum[non_bg]
        key = float(np.percentile(vals, 90))
        fill = float(np.percentile(vals, 50))
        rim = float(np.percentile(vals, 15))
        key_fill = key / max(fill, 1e-6)
        rim_ratio = rim / max(key, 1e-6)
    else:
        key_fill = 0.0
        rim_ratio = 0.0
    # Spatial occupancy bands by image thirds (near / mid / far proxy).
    h = lum.shape[0]
    bands = []
    for start, end in ((int(h * 0.55), h), (int(h * 0.25), int(h * 0.55)), (0, int(h * 0.25))):
        slice_ = non_bg[start:end, :]
        bands.append(float(np.mean(slice_)) if slice_.size else 0.0)
    return {
        "non_background_pixel_fraction": non_bg_frac,
        "luminance_histogram": [float(v) for v in hist_f],
        "light_key_fill_ratio": float(key_fill),
        "light_rim_ratio": float(rim_ratio),
        "foreground_occupancy": bands[0],
        "midground_occupancy": bands[1],
        "background_occupancy": bands[2],
        "mean_luminance": float(np.mean(lum)),
        "luminance_std": float(np.std(lum)),
    }


def measure_text_safe_contrast(
    frame: NDArray[np.floating] | NDArray[np.integer],
    *,
    zone: TextZone | str,
    text_luminance: float = 1.0,
) -> dict[str, Any]:
    """Measure text-safe contrast from pixels. Contrast is never assumed."""
    result = evaluate_text_safe(frame, zone=zone, text_luminance=text_luminance)
    return {
        "zone": result.zone.value,
        "contrast_ratio": result.contrast_ratio,
        "readable": result.readable,
        "rect": list(result.rect),
        "luminance_variance": result.luminance_variance,
        "mean_background_luminance": result.mean_background_luminance,
        "reason": result.reason,
    }


def _confirm_visible_from_render(
    hits: Sequence[FrustumHit],
    pixel_stats: Mapping[str, Any],
    *,
    empty_threshold: float = 0.05,
) -> tuple[int, int, int, float]:
    """Confirm frustum hits against the rendered frame.

    When the render is near-empty, nothing is confirmed visible — even if the
    scene graph says the frustum is full. That is the second-aisle defect mode.
    """
    non_bg = float(pixel_stats.get("non_background_pixel_fraction", 0.0))
    if non_bg < empty_threshold:
        return 0, 0, 0, 1.0
    racks = sum(1 for h in hits if h.archetype in RACK_ARCHETYPES)
    drawers = sum(1 for h in hits if h.archetype in DRAWER_ARCHETYPES)
    # Occupancy softens confirmation: sparse renders cannot claim every frustum hit.
    density = min(1.0, non_bg / 0.35)
    visible_instances = max(0, int(round(len(hits) * density)))
    visible_racks = max(0, int(round(racks * density))) if racks else 0
    visible_drawers = max(0, int(round(drawers * density))) if drawers else 0
    # If density is healthy, report full frustum counts (no artificial discount).
    if density >= 0.85:
        visible_instances = len(hits)
        visible_racks = racks
        visible_drawers = drawers
    occlusion = float(max(0.0, 1.0 - density))
    return visible_instances, visible_racks, visible_drawers, occlusion


def _depth_spread(hits: Sequence[FrustumHit]) -> float:
    if len(hits) < 2:
        return 0.0
    depths = np.array([h.depth_m for h in hits], dtype=np.float64)
    return float(np.percentile(depths, 90) - np.percentile(depths, 10))


def _focal_subject(hits: Sequence[FrustumHit]) -> str:
    if not hits:
        return "empty"
    # Prefer nearest rack, else nearest non-path instance, else nearest anything.
    racks = [h for h in hits if h.archetype in RACK_ARCHETYPES]
    if racks:
        best = min(racks, key=lambda h: h.depth_m)
        return best.instance_id
    drawers = [h for h in hits if h.archetype in DRAWER_ARCHETYPES]
    if drawers:
        best = min(drawers, key=lambda h: h.depth_m)
        return best.instance_id
    best = min(hits, key=lambda h: h.depth_m)
    return best.instance_id


def measure_beat_coverage(
    *,
    beat_id: str,
    beat_label: str,
    scroll: float,
    camera_position: Sequence[float],
    camera_target: Sequence[float],
    instances: Sequence[InstanceRef],
    frame: NDArray[np.floating] | NDArray[np.integer],
    text_zone: str,
    minimums: BeatMinimums | None = None,
    focal_length_mm: float = _DEFAULT_FOCAL_MM,
    library: ArchetypeLibrary | None = None,
    render_path: str = "",
    attestation_id: str = "",
    execution_class: str = "",
    performance_ms: float = 0.0,
    input_authorities: Sequence[str] | None = None,
) -> BeatCoverageReceipt:
    """Measure one beat from its camera, instance list, and rendered frame."""
    mins = minimums or flagship_beat_minimums(beat_id)
    hits = frustum_cull_instances(
        instances,
        camera_position=camera_position,
        camera_target=camera_target,
        focal_length_mm=focal_length_mm,
        library=library,
    )
    pixel_stats = measure_frame_pixels(frame)
    text = measure_text_safe_contrast(frame, zone=text_zone)
    visible_instances, visible_racks, visible_drawers, occlusion = _confirm_visible_from_render(
        hits, pixel_stats
    )
    fg = sum(1 for h in hits if h.band is DepthBand.FOREGROUND)
    mg = sum(1 for h in hits if h.band is DepthBand.MIDGROUND)
    bg = sum(1 for h in hits if h.band is DepthBand.BACKGROUND)
    total_hits = max(1, len(hits))

    # Primary measurement records leave input_authorities empty so derive() cannot
    # cap a review-only class to INFERRED (same pattern as compile_scene). Callers
    # that pass authorities explicitly are still capped by derive().
    if input_authorities:
        authorities = [str(a) for a in input_authorities]
        authority = derive(authorities, proposed=AuthorityClass.MODEL_DERIVED)
        lineage_authorities = authorities
    else:
        # In-process synthetic frames are procedural; a path on disk is treated as
        # runtime-observed only when the caller upgrades via attestation_id later.
        authority = (
            AuthorityClass.RUNTIME_OBSERVED
            if render_path and Path(render_path).is_file()
            else AuthorityClass.PROCEDURAL_GROUND_TRUTH
        )
        lineage_authorities = []

    receipt = BeatCoverageReceipt(
        id=f"beat-coverage-{beat_id}-{int(scroll * 1000):04d}",
        beat_id=beat_id,
        beat_label=beat_label,
        scroll=float(scroll),
        camera_position=[float(v) for v in camera_position],
        camera_target=[float(v) for v in camera_target],
        focal_length_mm=float(focal_length_mm),
        authority=authority,
        lineage=Lineage(
            operation="ocular.measure_beat_coverage",
            inputs=[f"beat:{beat_id}", render_path or "in-memory-frame"],
            input_authorities=lineage_authorities,
            parameters={
                "scroll": float(scroll),
                "text_zone": text_zone,
                "focal_length_mm": float(focal_length_mm),
            },
            limitations=[
                "Frustum uses conservative AABBs; occlusion is inferred from pixel density.",
                "Visible counts are confirmed against the rendered frame, not scene-graph alone.",
            ],
        ),
        uncertainty=Uncertainty(
            kind="beat-coverage-pixel",
            sigma=None,
            units=Units.UNITLESS,
            basis="frustum-cull + luminance histogram",
            samples=int(np.asarray(frame).size),
        ),
        created_at=utc_now(),
        minimums=mins.to_dict(),
        frustum_instance_count=len(hits),
        frustum_rack_count=sum(1 for h in hits if h.archetype in RACK_ARCHETYPES),
        frustum_drawer_count=sum(1 for h in hits if h.archetype in DRAWER_ARCHETYPES),
        frustum_instance_ids=[h.instance_id for h in hits[:64]],
        visible_instances=visible_instances,
        visible_racks=visible_racks,
        visible_drawers=visible_drawers,
        foreground_occupancy=float(fg / total_hits),
        midground_occupancy=float(mg / total_hits),
        background_occupancy=float(bg / total_hits),
        non_background_pixel_fraction=float(pixel_stats["non_background_pixel_fraction"]),
        depth_spread=_depth_spread(hits),
        luminance_histogram=list(pixel_stats["luminance_histogram"]),
        text_safe_zone=str(text["zone"]),
        text_safe_area=[float(v) for v in text["rect"]],
        text_safe_contrast=float(text["contrast_ratio"]),
        text_safe_readable=bool(text["readable"]),
        light_key_fill_ratio=float(pixel_stats["light_key_fill_ratio"]),
        light_rim_ratio=float(pixel_stats["light_rim_ratio"]),
        focal_subject=_focal_subject(hits),
        occlusion_fraction=occlusion,
        performance_ms=float(performance_ms),
        render_path=render_path,
        render_digest=(
            sha256_file(Path(render_path))[0]
            if render_path and Path(render_path).is_file()
            else ""
        ),
        attestation_id=attestation_id,
        execution_class=execution_class,
    )
    receipt.apply_gates()
    # Explicit base call: slots=True dataclasses invalidate zero-arg super().
    return V2Record.seal(receipt)


def beat_scroll_midpoint(beat_id: str) -> float:
    for bid, _label, start, end, _zone in FLAGSHIP_BEAT_SPECS:
        if bid == beat_id:
            return (start + end) * 0.5
    raise ValidationError(f"unknown beat_id {beat_id!r}")


def beat_text_zone(beat_id: str) -> str:
    for bid, _label, _start, _end, zone in FLAGSHIP_BEAT_SPECS:
        if bid == beat_id:
            return zone
    raise ValidationError(f"unknown beat_id {beat_id!r}")


def synthetic_empty_frame(
    width: int = 160,
    height: int = 90,
    *,
    luminance: float = 0.03,
) -> NDArray[np.float64]:
    """Near-black frame used by tests for empty-corridor failure modes."""
    return np.full((height, width, 3), float(luminance), dtype=np.float64)


def render_diagnostic_frame(
    instances: Sequence[InstanceRef],
    *,
    camera_position: Sequence[float],
    camera_target: Sequence[float],
    width: int = 320,
    height: int = 180,
    focal_length_mm: float = _DEFAULT_FOCAL_MM,
    library: ArchetypeLibrary | None = None,
) -> NDArray[np.float64]:
    """CPU diagnostic raster of instance AABBs from the beat camera.

    This is **not** a Blender EEVEE/Cycles render. It projects declared instance
    bounds into the image so offline / BLOCKED runs can still measure occupancy
    without inventing a physical PASS. Use ExecutionClass.DIAGNOSTIC_ONLY.
    """
    library = library or default_library()
    hits = frustum_cull_instances(
        instances,
        camera_position=camera_position,
        camera_target=camera_target,
        focal_length_mm=focal_length_mm,
        library=library,
        margin=0.05,
    )
    frame = np.full((height, width, 3), 0.04, dtype=np.float64)
    if not hits:
        return frame

    right, cam_up, forward = _look_basis(camera_position, camera_target)
    pos = np.asarray(camera_position, dtype=np.float64)
    hfov = 2.0 * math.atan((_DEFAULT_SENSOR_MM * 0.5) / max(focal_length_mm, 1e-6))
    aspect = width / max(height, 1)
    tan_h = math.tan(hfov * 0.5)
    tan_v = math.tan(hfov * 0.5) / aspect

    ordered = sorted(hits, key=lambda h: h.depth_m, reverse=True)
    by_id = {inst.instance_id: inst for inst in instances}
    # Screen-space z-buffer (lower depth wins) so far geometry does not wipe near.
    zbuf = np.full((height, width), np.inf, dtype=np.float64)

    def project_point(world: NDArray[np.float64]) -> tuple[float, float, float] | None:
        offset = world - pos
        depth = float(np.dot(offset, forward))
        if depth <= 1e-4:
            return None
        x = float(np.dot(offset, right))
        y = float(np.dot(offset, cam_up))
        ndc_x = x / (tan_h * depth)
        ndc_y = y / (tan_v * depth)
        px = (ndc_x * 0.5 + 0.5) * (width - 1)
        py = (1.0 - (ndc_y * 0.5 + 0.5)) * (height - 1)
        return px, py, depth

    for hit in ordered:
        inst = by_id.get(hit.instance_id)
        if inst is None:
            continue
        try:
            dims = library.create(inst.archetype, inst.params).declared_dimensions()
            half = np.array(
                [dims.width_m * 0.5, dims.depth_m * 0.5, dims.height_m * 0.5],
                dtype=np.float64,
            )
        except (ValidationError, KeyError, TypeError, ValueError):
            half = np.array([0.3, 0.3, 0.5], dtype=np.float64)
        centre = np.asarray(inst.transform.location, dtype=np.float64)
        # Project all 8 AABB corners; fill the screen-space AABB.
        corners: list[tuple[float, float, float]] = []
        for sx in (-1.0, 1.0):
            for sy in (-1.0, 1.0):
                for sz in (-1.0, 1.0):
                    corner = centre + half * np.array([sx, sy, sz], dtype=np.float64)
                    projected = project_point(corner)
                    if projected is not None:
                        corners.append(projected)
        if len(corners) < 2:
            # Fall back to centre + projected radius.
            projected = project_point(centre)
            if projected is None:
                continue
            px, py, depth = projected
            radius = float(np.linalg.norm(half))
            rx = max(2, int((radius / (tan_h * depth)) * 0.5 * width))
            ry = max(2, int((radius / (tan_v * depth)) * 0.5 * height))
            x0, x1 = max(0, int(px) - rx), min(width, int(px) + rx + 1)
            y0, y1 = max(0, int(py) - ry), min(height, int(py) + ry + 1)
            depth_val = depth
        else:
            xs = [c[0] for c in corners]
            ys = [c[1] for c in corners]
            depth_val = min(c[2] for c in corners)
            # Inflate by 1 px so thin silhouettes still register as non-background.
            x0 = max(0, int(math.floor(min(xs))) - 1)
            x1 = min(width, int(math.ceil(max(xs))) + 2)
            y0 = max(0, int(math.floor(min(ys))) - 1)
            y1 = min(height, int(math.ceil(max(ys))) + 2)
        if x1 <= x0 or y1 <= y0:
            continue
        # Colours are sRGB; measure_frame_pixels linearises them. Values must
        # land above the 0.08 linear luminance non-background ceiling after
        # linearisation (~0.35+ sRGB for neutral greys).
        if hit.archetype in RACK_ARCHETYPES:
            colour = np.array([0.48, 0.50, 0.54], dtype=np.float64)
        elif hit.archetype in DRAWER_ARCHETYPES:
            colour = np.array([0.58, 0.55, 0.48], dtype=np.float64)
        elif "status" in hit.tags or hit.archetype == "status_light_matrix":
            colour = np.array([0.25, 0.85, 0.40], dtype=np.float64)
        elif hit.archetype in {"terminal_wall", "threshold", "junction", "aisle"}:
            colour = np.array([0.42, 0.42, 0.45], dtype=np.float64)
        else:
            colour = np.array([0.40, 0.40, 0.42], dtype=np.float64)
        falloff = float(np.clip(1.15 - depth_val / 28.0, 0.55, 1.0))
        colour = colour * falloff
        region = zbuf[y0:y1, x0:x1]
        mask = region > depth_val
        if not np.any(mask):
            continue
        frame[y0:y1, x0:x1][mask] = colour
        region[mask] = depth_val
        if hit.archetype in DRAWER_ARCHETYPES and (y1 - y0) > 4:
            for yy in range(y0, y1, max(2, (y1 - y0) // 5)):
                row_mask = zbuf[yy, x0:x1] >= depth_val - 1e-6
                if np.any(row_mask):
                    frame[yy, x0:x1][row_mask] = np.minimum(
                        frame[yy, x0:x1][row_mask] + 0.12, 0.85
                    )
    return frame


def synthetic_populated_frame(
    width: int = 160,
    height: int = 90,
) -> NDArray[np.float64]:
    """Structured frame with depth bands and a dark text-safe region."""
    frame = np.zeros((height, width, 3), dtype=np.float64)
    # Floor gradient (lower third brighter — practical aisle light).
    for y in range(height):
        t = y / max(height - 1, 1)
        frame[y, :, :] = 0.05 + 0.18 * t
    # Vertical rack columns (left and right) — mid luminance, structured.
    for x0, x1 in ((int(width * 0.05), int(width * 0.32)), (int(width * 0.68), int(width * 0.95))):
        frame[:, x0:x1, :] = 0.28
        # Drawer horizontals.
        for y in range(int(height * 0.15), int(height * 0.92), max(2, height // 18)):
            frame[y : y + 2, x0:x1, :] = 0.45
    # Mid-aisle depth cue (brighter far wall band).
    frame[int(height * 0.12) : int(height * 0.28), int(width * 0.35) : int(width * 0.65), :] = 0.20
    # Bright status LEDs sparse.
    frame[int(height * 0.3), int(width * 0.15)] = (0.2, 0.9, 0.3)
    frame[int(height * 0.35), int(width * 0.82)] = (0.9, 0.7, 0.1)
    # Dark text-safe centre band so white copy contrasts.
    zone = ZONE_RECTS[TextZone.CENTRE]
    x0 = int(zone[0] * (width - 1))
    y0 = int(zone[1] * (height - 1))
    x1 = int(zone[2] * (width - 1)) + 1
    y1 = int(zone[3] * (height - 1)) + 1
    frame[y0:y1, x0:x1, :] = 0.04
    # Also darken edge / right_upper zones used by other beats.
    for z in (TextZone.EDGE, TextZone.RIGHT_UPPER):
        rect = ZONE_RECTS[z]
        xa = int(rect[0] * (width - 1))
        ya = int(rect[1] * (height - 1))
        xb = int(rect[2] * (width - 1)) + 1
        yb = int(rect[3] * (height - 1)) + 1
        frame[ya:yb, xa:xb, :] = np.minimum(frame[ya:yb, xa:xb, :], 0.06)
    return frame


def per_beat_instance_counts(
    instances: Sequence[InstanceRef],
    *,
    junction_y: float = 13.2,
) -> dict[str, dict[str, int]]:
    """Split instance counts by aisle for the coverage report table.

    Prefer the ``second`` tag placed by the flagship grammar. Fall back to a
    spatial rule: anything at or past the junction in Y, or past x=2 m on the
    post-turn corridor, counts as second aisle.
    """
    second: list[InstanceRef] = []
    main: list[InstanceRef] = []
    for inst in instances:
        tags = set(inst.tags)
        x, y, _z = inst.transform.location
        if "second" in tags or "terminal" in tags or y >= junction_y - 0.1 or x >= 2.0:
            second.append(inst)
        else:
            main.append(inst)

    def _bucket(items: Sequence[InstanceRef]) -> dict[str, int]:
        racks = sum(1 for i in items if i.archetype in RACK_ARCHETYPES)
        drawers = sum(1 for i in items if i.archetype in DRAWER_ARCHETYPES)
        return {
            "instances": len(items),
            "racks": racks,
            "drawers": drawers,
        }

    return {
        "main_aisle": _bucket(main),
        "second_aisle": _bucket(second),
        "total": _bucket(instances),
    }
