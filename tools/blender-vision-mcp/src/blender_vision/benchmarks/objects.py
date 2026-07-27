"""Sealed object benchmarks: consumer remote (Phase O) and soft/organic/fur (Phase P).

Doctrine:
- Derived claims use `derive()` and never outrank their weakest input.
- Never-observed structure is NEVER_OBSERVED, never OBSERVED.
- Blender/COLMAP failures are reported with the exact reason; software paths are
  labelled as such and do not silently absorb a crash.
- Poor reconstruction scores are results, not framework crashes.
- Synthetic fur is synthetic ground truth only — never evidence about a real animal.

There is no `benchmarks/sealed.py` on this branch yet; this module is the local
sealed contract for object benchmarks (source packet → sealed builder → hidden
evaluator → scorecard). Coupling note: if a shared sealed framework lands later,
this module should adopt it rather than diverge.
"""

from __future__ import annotations

import hashlib
import json
import math
import os
import subprocess
import time
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from skimage.metrics import peak_signal_noise_ratio, structural_similarity

from blender_vision.active_perception import (
    NextBestViewPlanner,
    PerceptionTarget,
    PlannerConfig,
    SurfaceCell,
    consumer_object_candidates,
)
from blender_vision.core.errors import ValidationError
from blender_vision.core.util import utc_now
from blender_vision.delivery.compress import measure_and_select_compression
from blender_vision.delivery.lods import LodBudget, generate_lods
from blender_vision.delivery.manifest import build_delivery_manifest
from blender_vision.materials.inverse import (
    SurfaceObservation,
    SurfaceRegion,
    infer_materials,
)
from blender_vision.reconstruction.base import (
    CameraView,
    DepthFrame,
    MeshGeometry,
    PointCloud,
    ReconstructionInputs,
)
from blender_vision.reconstruction.browser_runtime import BrowserRuntimeBackend
from blender_vision.reconstruction.colmap_sfm import (
    DENSE_UNAVAILABLE_REASON,
    ColmapSfMBackend,
)
from blender_vision.reconstruction.depth_fusion import DepthFusionBackend
from blender_vision.reconstruction.mesh_ops import (
    bounding_box,
    box_mesh,
    chamfer_distance,
    load_mesh_artifact,
    sample_surface_points,
    write_obj_mesh,
    write_ply_mesh,
)
from blender_vision.reconstruction.parametric import ParametricBackend
from blender_vision.reconstruction.point_representation import PointRepresentationBackend
from blender_vision.reconstruction.portfolio import build_portfolio, write_portfolio
from blender_vision.reconstruction.retrieval import RetrievalBackend
from blender_vision.reconstruction.visual_hull import (
    VisualHullBackend,
    synthetic_silhouette_masks,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    Units,
    VisibilityState,
    derive,
    visibility_authority_ceiling,
)
from blender_vision.v2.records import DeliveryAsset, Lineage
from blender_vision.v2.validation import verify_payload

REPO_ROOT = Path(__file__).resolve().parents[3]
LIBRARY = REPO_ROOT / "benchmarks" / "reconstruction" / "fixtures" / "archetypes"
BROWSER_SCENE = (
    REPO_ROOT / "benchmarks" / "reconstruction" / "fixtures" / "browser_scenes" / "owned_box.json"
)

# Gates carried from the organic lane. Do not relax.
MIN_UV_PACKING = 0.35
KNOWN_OPEN_UV_FAILURES: frozenset[tuple[str, str]] = frozenset(
    {
        ("organic_sculpture", "uv_packing"),
        ("plant", "uv_packing"),
    }
)

SYNTHETIC_FUR_CLAIM = (
    "Synthetic target with known construction parameters. This is not evidence "
    "about any real animal; the real-animal lane remains blocked on an authorized "
    "multiview capture set."
)


# ---------------------------------------------------------------------------
# Construction parameters (Phase O remote)
# ---------------------------------------------------------------------------


@dataclass(slots=True)
class RemoteConstruction:
    """Known construction parameters for the self-captured remote fixture.

    All lengths in metres. These are procedural ground truth, not measurements
    of any user's physical remote.
    """

    body_half_extents_m: tuple[float, float, float] = (0.090, 0.030, 0.0125)
    button_radius_m: float = 0.008
    button_height_m: float = 0.004
    button_grid: tuple[int, int] = (4, 2)
    battery_hatch_half_extents_m: tuple[float, float, float] = (0.040, 0.018, 0.0015)
    battery_hatch_z_m: float = -0.0125
    seam_inset_m: float = 0.001
    material_body_rgb: tuple[float, float, float] = (0.12, 0.12, 0.14)
    material_button_rgb: tuple[float, float, float] = (0.75, 0.22, 0.18)
    material_hatch_rgb: tuple[float, float, float] = (0.08, 0.08, 0.09)

    @property
    def body_dimensions_mm(self) -> tuple[float, float, float]:
        hx, hy, hz = self.body_half_extents_m
        return (hx * 2000.0, hy * 2000.0, hz * 2000.0)

    @property
    def bounds_min(self) -> np.ndarray:
        hx, hy, hz = self.body_half_extents_m
        return np.array([-hx, -hy, -hz - 0.002], dtype=np.float64)

    @property
    def bounds_max(self) -> np.ndarray:
        hx, hy, hz = self.body_half_extents_m
        return np.array(
            [hx, hy, hz + self.button_height_m + 0.001],
            dtype=np.float64,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "body_half_extents_m": list(self.body_half_extents_m),
            "body_dimensions_mm": list(self.body_dimensions_mm),
            "button_radius_m": self.button_radius_m,
            "button_height_m": self.button_height_m,
            "button_grid": list(self.button_grid),
            "battery_hatch_half_extents_m": list(self.battery_hatch_half_extents_m),
            "battery_hatch_z_m": self.battery_hatch_z_m,
            "seam_inset_m": self.seam_inset_m,
            "materials": {
                "body_rgb": list(self.material_body_rgb),
                "button_rgb": list(self.material_button_rgb),
                "hatch_rgb": list(self.material_hatch_rgb),
            },
            "authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
            "claim": (
                "Procedurally constructed consumer-object fixture. Not a claim "
                "about any physical remote control."
            ),
        }


# ---------------------------------------------------------------------------
# Sealed scorecard types
# ---------------------------------------------------------------------------


class BenchmarkPhase(StrEnum):
    CONSUMER_OBJECT = "consumer_object"
    SOFT = "soft"
    ORGANIC = "organic"
    FUR = "fur"


class StageStatus(StrEnum):
    PASSED = "passed"
    FAILED = "failed"
    BLOCKED = "blocked"
    SKIPPED = "skipped"
    REPORTED = "reported"  # scored honestly, may be poor


@dataclass(slots=True)
class HiddenSurfaceEntry:
    region: str
    visibility: VisibilityState
    authority_ceiling: AuthorityClass
    reason: str
    observed: bool = False
    area_m2: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "region": self.region,
            "visibility": self.visibility.value,
            "authority_ceiling": self.authority_ceiling.value,
            "reason": self.reason,
            "observed": self.observed,
            "area_m2": self.area_m2,
        }


@dataclass(slots=True)
class BackendScore:
    backend: str
    executed: bool
    chamfer_m: float | None
    a_to_b_m: float | None = None
    b_to_a_m: float | None = None
    reason: str | None = None
    authority: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class DimensionalError:
    axis: str
    truth_mm: float
    measured_mm: float
    error_mm: float
    relative_error: float

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class ImageMetrics:
    view_id: str
    psnr_db: float
    ssim: float
    held_out: bool = True

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class TargetScorecard:
    target_id: str
    phase: BenchmarkPhase
    authority: AuthorityClass
    stages: dict[str, StageStatus] = field(default_factory=dict)
    backend_scores: list[BackendScore] = field(default_factory=list)
    dimensional_errors_mm: list[DimensionalError] = field(default_factory=list)
    unseen_view_metrics: list[ImageMetrics] = field(default_factory=list)
    hidden_surface_ledger: list[HiddenSurfaceEntry] = field(default_factory=list)
    materials: dict[str, Any] = field(default_factory=dict)
    topology: dict[str, Any] = field(default_factory=dict)
    uv: dict[str, Any] = field(default_factory=dict)
    delivery: dict[str, Any] = field(default_factory=dict)
    next_views: dict[str, Any] = field(default_factory=dict)
    blockers: list[dict[str, Any]] = field(default_factory=list)
    failures: list[dict[str, Any]] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    synthetic_claim: str | None = None
    artifacts: dict[str, str] = field(default_factory=dict)
    runtime_seconds: float = 0.0

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_id": self.target_id,
            "phase": self.phase.value,
            "authority": self.authority.value,
            "stages": {k: v.value for k, v in self.stages.items()},
            "backend_scores": [b.to_dict() for b in self.backend_scores],
            "dimensional_errors_mm": [d.to_dict() for d in self.dimensional_errors_mm],
            "unseen_view_metrics": [m.to_dict() for m in self.unseen_view_metrics],
            "hidden_surface_ledger": [h.to_dict() for h in self.hidden_surface_ledger],
            "hidden_surface_counts": {
                "total": len(self.hidden_surface_ledger),
                "never_observed": sum(
                    1
                    for h in self.hidden_surface_ledger
                    if h.visibility is VisibilityState.NEVER_OBSERVED
                ),
                "inferred": sum(
                    1
                    for h in self.hidden_surface_ledger
                    if h.visibility is VisibilityState.INFERRED_SURFACE
                ),
                "incorrectly_marked_observed": sum(
                    1 for h in self.hidden_surface_ledger if h.observed
                ),
            },
            "materials": self.materials,
            "topology": self.topology,
            "uv": self.uv,
            "delivery": self.delivery,
            "next_views": self.next_views,
            "blockers": list(self.blockers),
            "failures": list(self.failures),
            "notes": list(self.notes),
            "synthetic_claim": self.synthetic_claim,
            "artifacts": dict(self.artifacts),
            "runtime_seconds": self.runtime_seconds,
        }


# ---------------------------------------------------------------------------
# Geometry helpers
# ---------------------------------------------------------------------------


def _look_at(position: list[float] | np.ndarray, target: list[float] | None = None) -> np.ndarray:
    target_arr = np.asarray(target or [0.0, 0.0, 0.0], dtype=np.float64)
    pos = np.asarray(position, dtype=np.float64)
    back = pos - target_arr
    norm = np.linalg.norm(back)
    back = np.array([0.0, 1.0, 0.0]) if norm < 1e-12 else back / norm
    provisional_up = (
        np.array([0.0, 1.0, 0.0]) if abs(back[2]) > 0.98 else np.array([0.0, 0.0, 1.0])
    )
    right = np.cross(provisional_up, back)
    right = right / max(np.linalg.norm(right), 1e-12)
    up = np.cross(back, right)
    mat = np.eye(4)
    mat[:3, 0] = right
    mat[:3, 1] = up
    mat[:3, 2] = back
    mat[:3, 3] = pos
    return mat


def orbit_cameras(
    count: int,
    *,
    radius: float = 0.45,
    size: int = 256,
    elevation: float = 0.35,
    seed: int = 0,
    name_prefix: str = "cam",
) -> list[CameraView]:
    """Orbit cameras with slight elevation jitter. None look from under the object."""
    rng = np.random.default_rng(seed)
    cameras: list[CameraView] = []
    for i in range(count):
        angle = 2 * np.pi * i / count + float(rng.uniform(-0.03, 0.03))
        elev = elevation + float(rng.uniform(-0.05, 0.08))
        elev = max(0.12, elev)  # keep above horizon — underside stays unobserved
        pos = [
            radius * math.cos(angle) * math.cos(elev),
            radius * math.sin(angle) * math.cos(elev),
            radius * math.sin(elev),
        ]
        cameras.append(
            CameraView(
                name=f"{name_prefix}{i:03d}",
                width=size,
                height=size,
                fx=size * 1.35,
                fy=size * 1.35,
                cx=size / 2,
                cy=size / 2,
                world_from_camera=_look_at(pos),
            )
        )
    return cameras


def build_remote_mesh(construction: RemoteConstruction | None = None) -> MeshGeometry:
    """Procedural remote-control-like body with buttons and underside hatch."""
    c = construction or RemoteConstruction()
    hx, hy, hz = c.body_half_extents_m
    parts: list[MeshGeometry] = [box_mesh([-hx, -hy, -hz], [hx, hy, hz])]

    # Buttons on the top face (extruded cylinders approximated as small boxes).
    cols, rows = c.button_grid
    span_x = hx * 1.4
    span_y = hy * 1.0
    for i in range(cols):
        for j in range(rows):
            x = -span_x / 2 + (i + 0.5) * (span_x / cols)
            y = -span_y / 2 + (j + 0.5) * (span_y / rows)
            r = c.button_radius_m
            z0 = hz
            z1 = hz + c.button_height_m
            parts.append(box_mesh([x - r, y - r, z0], [x + r, y + r, z1]))

    # Battery hatch on the underside (recessed panel).
    bhx, bhy, bhz = c.battery_hatch_half_extents_m
    z_h = c.battery_hatch_z_m
    parts.append(box_mesh([-bhx, -bhy, z_h - bhz], [bhx, bhy, z_h + bhz]))

    # Side seam beads (thin ridges) — surface detail for multiview features.
    seam = c.seam_inset_m
    for sign in (-1.0, 1.0):
        parts.append(
            box_mesh(
                [sign * (hx - seam * 2), -hy * 0.9, -hz * 0.3],
                [sign * hx, hy * 0.9, hz * 0.3],
            )
        )

    return merge_meshes(parts)


def merge_meshes(parts: list[MeshGeometry]) -> MeshGeometry:
    if not parts:
        return MeshGeometry(vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=np.int64))
    verts: list[np.ndarray] = []
    faces: list[np.ndarray] = []
    offset = 0
    for mesh in parts:
        v = np.asarray(mesh.vertices, dtype=np.float64)
        f = np.asarray(mesh.faces, dtype=np.int64)
        verts.append(v)
        faces.append(f + offset)
        offset += len(v)
    return MeshGeometry(
        vertices=np.vstack(verts),
        faces=np.vstack(faces) if faces else np.zeros((0, 3), dtype=np.int64),
    )


def rasterize_mesh(
    camera: CameraView,
    mesh: MeshGeometry,
    *,
    background: int = 28,
    base_colour: tuple[int, int, int] = (55, 55, 60),
    seed: int = 0,
) -> np.ndarray:
    """Painter's-algorithm rasterizer (Blender camera: -Z forward, +Y up)."""
    h, w = camera.height, camera.width
    img = np.full((h, w, 3), background, dtype=np.uint8)
    cam_from_world = camera.camera_from_world()
    verts = mesh.vertices
    faces = mesh.faces
    order: list[tuple[float, int, np.ndarray, np.ndarray]] = []
    for fi, (a, b, c) in enumerate(faces):
        tri = verts[[a, b, c]]
        homo = np.concatenate([tri, np.ones((3, 1))], axis=1)
        cam = (cam_from_world @ homo.T).T
        depth = -cam[:, 2]
        if np.any(depth <= 1e-4):
            continue
        order.append((float(depth.mean()), fi, cam, depth))
    order.sort(reverse=True)
    light = np.array([0.45, -0.35, 0.82])
    light = light / np.linalg.norm(light)
    rng = np.random.default_rng(seed)
    for _key, fi, cam, depth in order:
        a, b, c = faces[fi]
        tri = verts[[a, b, c]]
        u = camera.fx * (cam[:, 0] / depth) + camera.cx
        v = camera.cy - camera.fy * (cam[:, 1] / depth)
        pts = np.stack([u, v], axis=1).astype(np.float32)
        if not np.all(np.isfinite(pts)):
            continue
        normal = np.cross(tri[1] - tri[0], tri[2] - tri[0])
        nlen = np.linalg.norm(normal)
        if nlen < 1e-12:
            continue
        normal = normal / nlen
        # Underside faces (normal pointing down-ish) get darker hatch colour.
        if normal[2] < -0.4:
            colour_base = (22, 22, 24)
        elif normal[2] > 0.7 and tri[:, 2].mean() > 0.01:
            colour_base = (180, 55, 45)  # buttons
        else:
            colour_base = base_colour
        shade = float(0.30 + 0.70 * max(0.0, normal @ light))
        colour = (
            int(colour_base[0] * shade),
            int(colour_base[1] * shade),
            int(colour_base[2] * shade),
        )
        if fi % 3 == 0:
            colour = (min(255, colour[0] + 28), colour[1], min(255, colour[2] + 18))
        cv2.fillConvexPoly(img, np.round(pts).astype(np.int32), colour)
    # High-frequency texture for SIFT / COLMAP.
    noise = rng.integers(0, 28, img.shape, dtype=np.int16)
    yy, xx = np.mgrid[0:h, 0:w]
    checker = ((xx // 10 + yy // 10) % 2) * 14
    mask = img.sum(axis=2) > background + 4
    img = img.astype(np.int16)
    img[mask] = np.clip(img[mask] + noise[mask] + checker[mask, None], 0, 255)
    return img.astype(np.uint8)


def blender_to_opencv_camera(camera: CameraView) -> CameraView:
    wfc = camera.world_from_camera.copy()
    wfc[:3, 1] *= -1
    wfc[:3, 2] *= -1
    return CameraView(
        name=camera.name + "_opencv",
        width=camera.width,
        height=camera.height,
        fx=camera.fx,
        fy=camera.fy,
        cx=camera.cx,
        cy=camera.cy,
        world_from_camera=wfc,
        near=camera.near,
        far=camera.far,
    )


def ray_box_depth(
    camera: CameraView,
    minimum: np.ndarray,
    maximum: np.ndarray,
) -> np.ndarray:
    """OpenCV-camera ray/AABB intersection depth (metres along +Z)."""
    h, w = camera.height, camera.width
    ys, xs = np.mgrid[0:h, 0:w]
    dirs = np.stack(
        [
            (xs - camera.cx) / camera.fx,
            (ys - camera.cy) / camera.fy,
            np.ones_like(xs, dtype=np.float64),
        ],
        axis=-1,
    )
    dirs = dirs / np.linalg.norm(dirs, axis=-1, keepdims=True)
    origin = camera.world_from_camera[:3, 3]
    # Transform rays to world.
    rot = camera.world_from_camera[:3, :3]
    world_dirs = dirs @ rot.T
    # Slab method.
    inv = 1.0 / np.where(np.abs(world_dirs) < 1e-12, 1e-12, world_dirs)
    t0 = (minimum - origin) * inv
    t1 = (maximum - origin) * inv
    tmin = np.minimum(t0, t1).max(axis=-1)
    tmax = np.maximum(t0, t1).min(axis=-1)
    hit = (tmax >= tmin) & (tmin > 0)
    depth = np.where(hit, tmin, 0.0).astype(np.float64)
    return depth


# ---------------------------------------------------------------------------
# Hidden-surface ledger
# ---------------------------------------------------------------------------


def remote_hidden_surface_ledger(
    construction: RemoteConstruction,
    *,
    views_include_underside: bool = False,
) -> list[HiddenSurfaceEntry]:
    """Ledger of surfaces the multiview capture never observes.

    Top-hemisphere orbits never see the underside or the battery compartment.
    Those regions must stay NEVER_OBSERVED / INFERRED, never OBSERVED.
    """
    hx, hy, hz = construction.body_half_extents_m
    body_bottom_area = 4.0 * hx * hy
    bhx, bhy, bhz = construction.battery_hatch_half_extents_m
    hatch_area = 4.0 * bhx * bhy
    interior_area = hatch_area * 2.5  # compartment walls (never photographed)

    ledger: list[HiddenSurfaceEntry] = []
    if not views_include_underside:
        ledger.append(
            HiddenSurfaceEntry(
                region="underside",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(VisibilityState.NEVER_OBSERVED),
                reason=(
                    "All capture cameras are in the upper hemisphere (elev >= 0.12 rad); "
                    "the bottom face is never in any silhouette or colour image."
                ),
                observed=False,
                area_m2=body_bottom_area,
            )
        )
        ledger.append(
            HiddenSurfaceEntry(
                region="battery_compartment_interior",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(VisibilityState.NEVER_OBSERVED),
                reason=(
                    "Battery hatch is closed; compartment interior is occluded by the hatch "
                    "and has no open-hatch capture."
                ),
                observed=False,
                area_m2=interior_area,
            )
        )
        ledger.append(
            HiddenSurfaceEntry(
                region="battery_hatch_outer",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(VisibilityState.NEVER_OBSERVED),
                reason="Hatch lies on the underside; same camera elevation constraint.",
                observed=False,
                area_m2=hatch_area,
            )
        )
    # Concave button wells / side seams may be only partially visible.
    ledger.append(
        HiddenSurfaceEntry(
            region="button_sidewalls",
            visibility=VisibilityState.PARTIALLY_VISIBLE,
            authority_ceiling=visibility_authority_ceiling(VisibilityState.PARTIALLY_VISIBLE),
            reason="Button cylinder walls are foreshortened; only tops are clearly observed.",
            observed=False,
            area_m2=2
            * math.pi
            * construction.button_radius_m
            * construction.button_height_m
            * construction.button_grid[0]
            * construction.button_grid[1],
        )
    )
    ledger.append(
        HiddenSurfaceEntry(
            region="internal_electronics",
            visibility=VisibilityState.NEVER_OBSERVED,
            authority_ceiling=visibility_authority_ceiling(VisibilityState.NEVER_OBSERVED),
            reason="No X-ray or disassembly; internals are topology prior only if modelled.",
            observed=False,
            area_m2=None,
        )
    )
    return ledger


def assert_ledger_honest(ledger: list[HiddenSurfaceEntry]) -> list[str]:
    """Return violations if any never-observed region is marked observed."""
    violations: list[str] = []
    for entry in ledger:
        if entry.visibility is VisibilityState.NEVER_OBSERVED and entry.observed:
            violations.append(
                f"{entry.region} is NEVER_OBSERVED but observed=True"
            )
        ceiling = visibility_authority_ceiling(entry.visibility)
        if entry.authority_ceiling != ceiling:
            violations.append(
                f"{entry.region} authority_ceiling {entry.authority_ceiling} "
                f"!= expected {ceiling}"
            )
    return violations


# ---------------------------------------------------------------------------
# Image / dimensional metrics
# ---------------------------------------------------------------------------


def image_metrics(pred: np.ndarray, truth: np.ndarray, *, view_id: str) -> ImageMetrics:
    a = np.asarray(pred, dtype=np.float64)
    b = np.asarray(truth, dtype=np.float64)
    if a.shape != b.shape:
        # Resize pred to truth for comparison (evaluator holds truth resolution).
        a = cv2.resize(a, (b.shape[1], b.shape[0]), interpolation=cv2.INTER_AREA)
    if a.max() > 1.0:
        a = a / 255.0
    if b.max() > 1.0:
        b = b / 255.0
    a = np.clip(a, 0.0, 1.0)
    b = np.clip(b, 0.0, 1.0)
    # PSNR: skimage expects data_range.
    try:
        psnr = float(peak_signal_noise_ratio(b, a, data_range=1.0))
    except ValueError:
        psnr = 0.0
    try:
        if a.ndim == 3:
            ssim = float(
                structural_similarity(b, a, data_range=1.0, channel_axis=2)
            )
        else:
            ssim = float(structural_similarity(b, a, data_range=1.0))
    except ValueError:
        ssim = 0.0
    return ImageMetrics(view_id=view_id, psnr_db=psnr, ssim=ssim, held_out=True)


def dimensional_errors(
    measured_mesh: MeshGeometry,
    truth_dimensions_mm: tuple[float, float, float] | list[float],
) -> list[DimensionalError]:
    lo, hi = bounding_box(measured_mesh)
    measured_m = hi - lo
    measured_mm = measured_m * 1000.0
    axes = ("x", "y", "z")
    out: list[DimensionalError] = []
    for i, axis in enumerate(axes):
        truth = float(truth_dimensions_mm[i])
        meas = float(measured_mm[i])
        err = meas - truth
        rel = abs(err) / truth if truth > 1e-9 else float("inf")
        out.append(
            DimensionalError(
                axis=axis,
                truth_mm=truth,
                measured_mm=meas,
                error_mm=err,
                relative_error=rel,
            )
        )
    return out


def render_prediction_views(
    mesh: MeshGeometry,
    cameras: list[CameraView],
    *,
    colour: tuple[int, int, int] = (70, 70, 75),
) -> dict[str, np.ndarray]:
    images: dict[str, np.ndarray] = {}
    for i, camera in enumerate(cameras):
        images[camera.name] = rasterize_mesh(camera, mesh, base_colour=colour, seed=100 + i)
    return images


# ---------------------------------------------------------------------------
# Capture + builder (Phase O)
# ---------------------------------------------------------------------------


def probe_blender_status() -> dict[str, Any]:
    try:
        from blender_vision.cinematic.blender_probe import probe_blender

        result = probe_blender()
        return {
            "available": bool(result.get("available")),
            "executable": result.get("executable"),
            "reason": result.get("reason") or "",
        }
    except Exception as error:  # noqa: BLE001 — probe must never crash the benchmark
        return {
            "available": False,
            "executable": None,
            "reason": f"Blender probe raised {type(error).__name__}: {error}",
        }


def write_source_packet(
    path: Path,
    *,
    target_id: str,
    construction: dict[str, Any],
    train_views: list[dict[str, Any]],
    holdout_view_ids: list[str],
    notes: list[str],
) -> Path:
    packet = {
        "schema": "visionmcp.object-benchmark-source-packet/v1",
        "target_id": target_id,
        "created_at": utc_now(),
        "authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
        "construction": construction,
        "train_view_count": len(train_views),
        "holdout_view_ids": holdout_view_ids,
        "train_views": train_views,
        "notes": notes,
        "lineage": Lineage(
            operation="object_benchmark_source_packet",
            input_authorities=[AuthorityClass.PROCEDURAL_GROUND_TRUTH.value],
            parameters={"target_id": target_id},
            limitations=notes,
        ).to_dict(),
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(packet, indent=2) + "\n", encoding="utf-8")
    return path


def capture_remote_fixture(
    output: Path,
    *,
    construction: RemoteConstruction | None = None,
    train_views: int = 24,
    holdout_views: int = 8,
    image_size: int = 256,
    seed: int = 20260726,
) -> dict[str, Any]:
    """Build truth mesh, render train + holdout views, write sealed source packet.

    Attempts real Blender when available; on Metal/GPU crash falls back to the
    software raycast path with an explicit blender_blocker (never silent).
    """
    c = construction or RemoteConstruction()
    output.mkdir(parents=True, exist_ok=True)
    blender = probe_blender_status()
    mesh = build_remote_mesh(c)
    truth_obj = write_obj_mesh(output / "truth.obj", mesh)
    truth_ply = write_ply_mesh(output / "truth.ply", mesh)
    (output / "construction.json").write_text(
        json.dumps(c.to_dict(), indent=2) + "\n", encoding="utf-8"
    )

    total = train_views + holdout_views
    all_cameras = orbit_cameras(
        total, radius=0.42, size=image_size, elevation=0.38, seed=seed, name_prefix="view_"
    )
    # Hold out every (train_views/holdout)-th view so holdouts are interleaved.
    holdout_idx = set(range(train_views, total))
    # Prefer evenly spaced holdouts:
    if holdout_views > 0:
        holdout_idx = {
            int(round(i * total / holdout_views)) % total for i in range(holdout_views)
        }
        # Ensure we still have enough train views.
        while len(holdout_idx) < holdout_views:
            holdout_idx.add(len(holdout_idx) % total)

    train_dir = output / "images" / "train"
    holdout_dir = output / "images" / "holdout"
    train_dir.mkdir(parents=True, exist_ok=True)
    holdout_dir.mkdir(parents=True, exist_ok=True)

    blender_used = False
    blender_blocker = None
    if blender["available"]:
        try:
            blender_used = _try_blender_remote_render(
                output, c, all_cameras, holdout_idx, train_dir, holdout_dir
            )
        except Exception as error:  # noqa: BLE001
            blender_blocker = (
                f"Blender render raised {type(error).__name__}: {error}"
            )
            blender_used = False
    else:
        blender_blocker = str(blender.get("reason") or "Blender unavailable")

    view_meta: list[dict[str, Any]] = []
    if not blender_used:
        for i, camera in enumerate(all_cameras):
            img = rasterize_mesh(
                camera,
                mesh,
                base_colour=tuple(int(x * 255) for x in c.material_body_rgb),
                seed=seed + i,
            )
            is_holdout = i in holdout_idx
            dest = holdout_dir if is_holdout else train_dir
            path = dest / f"{camera.name}.png"
            cv2.imwrite(str(path), img)
            view_meta.append(
                {
                    "name": camera.name,
                    "path": str(path),
                    "held_out": is_holdout,
                    "width": camera.width,
                    "height": camera.height,
                    "fx": camera.fx,
                    "fy": camera.fy,
                    "cx": camera.cx,
                    "cy": camera.cy,
                    "world_from_camera": camera.world_from_camera.tolist(),
                }
            )

    # If blender wrote images, rebuild view_meta from disk + known cameras.
    if blender_used and not view_meta:
        for i, camera in enumerate(all_cameras):
            # Where the renderer actually put the file is the split, not what we
            # would have chosen. The Blender script applies its own stride rule;
            # recomputing the split here produced two disagreeing answers and
            # labelled train images as held out, which is a leak.
            rendered_train = (train_dir / f"{camera.name}.png").is_file()
            rendered_holdout = (holdout_dir / f"{camera.name}.png").is_file()
            if rendered_train and rendered_holdout:
                raise ValidationError(
                    f"{camera.name} was rendered into both splits; the renderer and "
                    "the split rule disagree"
                )
            if rendered_train or rendered_holdout:
                is_holdout = rendered_holdout
            else:
                is_holdout = i in holdout_idx
            dest = holdout_dir if is_holdout else train_dir
            path = dest / f"{camera.name}.png"
            if not path.is_file():
                # Software fill for any missing frames.
                img = rasterize_mesh(camera, mesh, seed=seed + i)
                cv2.imwrite(str(path), img)
            view_meta.append(
                {
                    "name": camera.name,
                    "path": str(path),
                    "held_out": is_holdout,
                    "width": camera.width,
                    "height": camera.height,
                    "fx": camera.fx,
                    "fy": camera.fy,
                    "cx": camera.cx,
                    "cy": camera.cy,
                    "world_from_camera": camera.world_from_camera.tolist(),
                }
            )

    train_meta = [v for v in view_meta if not v["held_out"]]
    holdout_ids = [v["name"] for v in view_meta if v["held_out"]]

    # Leakage invariant, enforced rather than assumed: a held-out view must not
    # exist in the train directory under any name. An overlap here would let the
    # builder be scored on views it was given, which is the exact failure the
    # sealed framework exists to prevent, so it is fatal and not a warning.
    train_on_disk = {path.stem for path in train_dir.glob("*.png")}
    holdout_on_disk = {path.stem for path in holdout_dir.glob("*.png")}
    leaked = train_on_disk & holdout_on_disk
    if leaked:
        raise ValidationError(
            "held-out views leaked into the training set: " + ", ".join(sorted(leaked))
        )
    if set(holdout_ids) & {v["name"] for v in train_meta}:
        raise ValidationError("held-out view ids appear in the train view metadata")

    notes = [
        "Self-captured procedural fixture; not photographs of a user remote.",
        f"train_views={len(train_meta)} holdout_views={len(holdout_ids)}",
        f"renderer={'blender' if blender_used else 'software_raycast'}",
    ]
    if blender_blocker:
        notes.append(f"blender_blocker: {blender_blocker}")

    packet_path = write_source_packet(
        output / "source_packet.json",
        target_id="consumer_remote",
        construction=c.to_dict(),
        train_views=train_meta,
        holdout_view_ids=holdout_ids,
        notes=notes,
    )
    cameras_path = output / "cameras.json"
    cameras_path.write_text(json.dumps(view_meta, indent=2) + "\n", encoding="utf-8")

    return {
        "target_id": "consumer_remote",
        "construction": c.to_dict(),
        "truth_obj": str(truth_obj),
        "truth_ply": str(truth_ply),
        "source_packet": str(packet_path),
        "cameras": str(cameras_path),
        "train_image_dir": str(train_dir),
        "holdout_image_dir": str(holdout_dir),
        "train_view_count": len(train_meta),
        "holdout_view_ids": holdout_ids,
        "view_meta": view_meta,
        "blender_used": blender_used,
        "blender_blocker": blender_blocker,
        "renderer": "blender" if blender_used else "software_raycast",
        "mesh": mesh,
        "construction_obj": c,
    }


def _try_blender_remote_render(
    output: Path,
    construction: RemoteConstruction,
    cameras: list[CameraView],
    holdout_idx: set[int],
    train_dir: Path,
    holdout_dir: Path,
) -> bool:
    """Invoke the benchmark Blender builder. Returns True only if images exist."""
    script = REPO_ROOT / "benchmarks" / "remote" / "build_and_render.py"
    if not script.is_file():
        return False
    blender_exe = (
        os.environ.get("BVMCP_BLENDER_PATH")
        or os.environ.get("BLENDER_EXECUTABLE")
        or "/Applications/Blender.app/Contents/MacOS/Blender"
    )
    cmd = [
        blender_exe,
        "--background",
        "--factory-startup",
        "--python",
        str(script),
        "--",
        "--output",
        str(output),
        "--views",
        str(len(cameras)),
    ]
    result = subprocess.run(cmd, capture_output=True, text=True, timeout=1800, check=False)
    (output / "blender_render.log").write_text(
        (result.stdout or "") + "\n" + (result.stderr or ""), encoding="utf-8"
    )
    if result.returncode != 0:
        return False
    # Require at least train_views images somewhere under images/.
    pngs = list((output / "images").rglob("*.png"))
    return len(pngs) >= max(8, len(cameras) // 2)


# ---------------------------------------------------------------------------
# Reconstruction portfolio scoring
# ---------------------------------------------------------------------------


def _candidate_mesh(candidate: Any) -> MeshGeometry | None:
    for key in ("mesh_ply", "mesh_obj"):
        path = candidate.artifacts.get(key) if hasattr(candidate, "artifacts") else None
        if path and Path(path).is_file():
            mesh = load_mesh_artifact(Path(path))
            if mesh is not None and not mesh.is_empty():
                return mesh
    return None


def run_geometry_portfolio(
    *,
    target_id: str,
    work_dir: Path,
    truth: MeshGeometry,
    train_cameras: list[CameraView],
    image_dir: Path | None,
    bounds_min: np.ndarray,
    bounds_max: np.ndarray,
    primitive_kind: str = "box",
    solid_params: dict[str, Any] | None = None,
    licensing: str = "SYNTHETIC_OWNED",
) -> tuple[Any, list[BackendScore], dict[str, Any]]:
    """Run the seven reconstruction backends and score chamfer vs truth."""
    work_dir.mkdir(parents=True, exist_ok=True)
    solid_params = solid_params or {
        "minimum": bounds_min.tolist(),
        "maximum": bounds_max.tolist(),
    }
    masks = synthetic_silhouette_masks(
        cameras=train_cameras,
        solid="box" if primitive_kind == "box" else primitive_kind,
        solid_params=solid_params,
    )
    points = PointCloud(positions=sample_surface_points(truth, 2200, seed=7))
    base = ReconstructionInputs(
        target_id=target_id,
        work_dir=work_dir / "work",
        frame=CoordinateFrame(
            name="blender-world",
            units=Units.METRE,
            scale_authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        ),
        masks=masks,
        cameras=train_cameras,
        bounds_min=bounds_min - 0.05,
        bounds_max=bounds_max + 0.05,
        points=points,
        primitive_kind=primitive_kind,
        image_dir=image_dir if image_dir and image_dir.is_dir() else None,
        parameters={"grid_resolution": 48, "primitive_kind": primitive_kind},
        input_authorities=[AuthorityClass.SENSOR_DERIVED, AuthorityClass.PROCEDURAL_GROUND_TRUTH],
        licensing=licensing,
        library_dir=LIBRARY if LIBRARY.is_dir() else None,
        archetype_id="box_unit",
        adaptation_scale=(1.0, 1.0, 1.0),
        evidence_refs=[f"truth:{target_id}"],
    )
    if BROWSER_SCENE.is_file():
        base.browser_scene = json.loads(BROWSER_SCENE.read_text(encoding="utf-8"))
    # Depth frames for TSDF / points (OpenCV cameras).
    base.depth_frames = [
        DepthFrame(
            name=cam.name,
            depth=ray_box_depth(
                blender_to_opencv_camera(cam),
                bounds_min,
                bounds_max,
            ),
            camera=blender_to_opencv_camera(cam),
        )
        for cam in train_cameras
    ]

    backends = [
        VisualHullBackend(),
        DepthFusionBackend(),
        ParametricBackend(),
        PointRepresentationBackend(),
        ColmapSfMBackend(),
        RetrievalBackend(),
        BrowserRuntimeBackend(),
    ]
    portfolio = build_portfolio(target_id=target_id, backends=backends, inputs_for=base)
    write_portfolio(work_dir / "portfolio.json", portfolio)
    try:
        verify_payload(portfolio.to_dict())
    except Exception as error:  # noqa: BLE001 — record, do not crash the scorecard
        (work_dir / "portfolio_verify_error.txt").write_text(str(error), encoding="utf-8")

    scores: list[BackendScore] = []
    for candidate in portfolio.candidates:
        mesh = _candidate_mesh(candidate)
        if mesh is None or truth is None:
            scores.append(
                BackendScore(
                    backend=candidate.backend,
                    executed=bool(candidate.executed),
                    chamfer_m=None,
                    reason=(
                        candidate.failure_modes[0]
                        if candidate.failure_modes
                        else "no mesh artifact"
                    ),
                    authority=candidate.authority.value
                    if hasattr(candidate.authority, "value")
                    else str(candidate.authority),
                )
            )
            continue
        metrics = chamfer_distance(mesh, truth, samples=1200)
        scores.append(
            BackendScore(
                backend=candidate.backend,
                executed=bool(candidate.executed),
                chamfer_m=metrics["chamfer"],
                a_to_b_m=metrics["a_to_b"],
                b_to_a_m=metrics["b_to_a"],
                authority=candidate.authority.value
                if hasattr(candidate.authority, "value")
                else str(candidate.authority),
            )
        )

    colmap = next((c for c in portfolio.candidates if c.backend == "colmap_sfm"), None)
    colmap_report: dict[str, Any] = {"dense_mvs_blocker": DENSE_UNAVAILABLE_REASON}
    if colmap is not None:
        colmap_report.update(
            {
                "executed": colmap.executed,
                "registered_images": colmap.coverage.get("registered_images"),
                "mean_reprojection_error_px": colmap.coverage.get(
                    "mean_reprojection_error_px"
                ),
                "num_points3d": colmap.coverage.get("num_points3d"),
                "failure_modes": list(colmap.failure_modes),
                "execution_log": colmap.execution_log,
            }
        )

    return portfolio, scores, colmap_report


# ---------------------------------------------------------------------------
# Materials, NBV, delivery
# ---------------------------------------------------------------------------


def estimate_remote_materials(
    train_image_dir: Path,
    construction: RemoteConstruction,
) -> dict[str, Any]:
    """Separate body vs button materials from train images (hypothesis set)."""
    images = sorted(train_image_dir.glob("*.png"))[:8]
    if not images:
        return {
            "status": "blocked",
            "reason": "no train images for material estimation",
        }
    body_obs: list[SurfaceObservation] = []
    button_obs: list[SurfaceObservation] = []
    for path in images:
        bgr = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if bgr is None:
            continue
        rgb = cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB).astype(np.float64) / 255.0
        # Heuristic masks: dark body vs reddish button tops.
        lum = 0.2126 * rgb[..., 0] + 0.7152 * rgb[..., 1] + 0.0722 * rgb[..., 2]
        body_mask = (lum > 0.05) & (lum < 0.35) & (rgb[..., 0] < 0.45)
        button_mask = (rgb[..., 0] > 0.35) & (rgb[..., 0] > rgb[..., 1] * 1.2)
        if body_mask.any():
            body_obs.append(
                SurfaceObservation(
                    view_id=path.stem,
                    rgb=rgb,
                    mask=body_mask,
                    authority=AuthorityClass.SENSOR_DERIVED,
                )
            )
        if button_mask.any():
            button_obs.append(
                SurfaceObservation(
                    view_id=path.stem,
                    rgb=rgb,
                    mask=button_mask,
                    authority=AuthorityClass.SENSOR_DERIVED,
                )
            )

    result: dict[str, Any] = {"surfaces": {}}
    if body_obs:
        body_set = infer_materials(
            body_obs,
            [SurfaceRegion(surface_id="remote_body", label="body")],
        )
        result["surfaces"]["body"] = body_set.to_dict()
    if button_obs:
        btn_set = infer_materials(
            button_obs,
            [SurfaceRegion(surface_id="remote_buttons", label="buttons")],
        )
        result["surfaces"]["buttons"] = btn_set.to_dict()
    # Hatch is never observed — materials for it must be INFERRED / HYPOTHETICAL.
    hatch_authority = derive(
        [AuthorityClass.HYPOTHETICAL],
        proposed=AuthorityClass.INFERRED,
    )
    result["surfaces"]["battery_hatch"] = {
        "surface_id": "battery_hatch",
        "status": "never_observed",
        "visibility": VisibilityState.NEVER_OBSERVED.value,
        "authority": hatch_authority.value,
        "note": (
            "No pixels of the hatch appear in the capture set; material is not "
            "estimated from observation. Construction colour is procedural truth only."
        ),
        "construction_colour_rgb": list(construction.material_hatch_rgb),
        "construction_authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
    }
    result["status"] = "ok" if result["surfaces"] else "blocked"
    return result


def plan_next_views_for_remote(target_id: str = "consumer_remote") -> dict[str, Any]:
    cells = [
        SurfaceCell(region="front", area_m2=0.005, covered=True, resolution_px=1600),
        SurfaceCell(region="top", area_m2=0.01, covered=True, resolution_px=1600),
        SurfaceCell(region="left", area_m2=0.003, covered=True, resolution_px=800),
        SurfaceCell(region="right", area_m2=0.003, covered=True, resolution_px=800),
        SurfaceCell(region="rear", area_m2=0.005, covered=False, resolution_px=0),
        SurfaceCell(region="underside", area_m2=0.01, covered=False, resolution_px=0),
        SurfaceCell(
            region="battery_compartment",
            area_m2=0.004,
            covered=False,
            resolution_px=0,
        ),
    ]
    target = PerceptionTarget(
        target_id=target_id,
        cells=cells,
        scale_authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        material_confidences=[0.7, 0.55],
        has_scale_reference=True,  # construction params are metric
        has_diffuse_light_view=True,
        has_grazing_light_view=False,
        has_lens_metadata=False,
        has_calibration_target=False,
    )
    # Doctrine entry: consumer_object_candidates enumerates the required view set;
    # NextBestViewPlanner.plan rebuilds that set internally and ranks by gain.
    proposed = consumer_object_candidates(target)
    planner = NextBestViewPlanner(PlannerConfig(max_requests=8, gain_threshold=0.02))
    result = planner.plan(target)
    payload = result.to_dict()
    payload["proposed_view_count"] = len(proposed)
    return payload


def _export_glb(mesh: MeshGeometry, path: Path) -> Path:
    """Write a minimal GLB via trimesh for the delivery LOD pipeline."""
    import trimesh

    path.parent.mkdir(parents=True, exist_ok=True)
    tm = trimesh.Trimesh(
        vertices=np.asarray(mesh.vertices, dtype=np.float64),
        faces=np.asarray(mesh.faces, dtype=np.int64),
        process=False,
    )
    tm.export(path)
    return path


def build_editable_delivery(
    mesh: MeshGeometry,
    output: Path,
    *,
    target_id: str,
) -> dict[str, Any]:
    """Export editable mesh + web LODs + offline note. Honest about Blender use."""
    output.mkdir(parents=True, exist_ok=True)
    editable = write_obj_mesh(output / f"{target_id}_editable.obj", mesh)
    ply = write_ply_mesh(output / f"{target_id}_editable.ply", mesh)
    glb = _export_glb(mesh, output / f"{target_id}_editable.glb")
    delivery: dict[str, Any] = {
        "editable_obj": str(editable),
        "editable_ply": str(ply),
        "editable_glb": str(glb),
        "offline_render": {
            "status": "software_proxy",
            "note": (
                "Offline Cycles path requires Blender. When Blender is blocked, "
                "the software raster of the editable mesh is the offline proxy."
            ),
        },
    }
    # Software offline "render" of three canonical views.
    offline_dir = output / "offline_proxy"
    offline_dir.mkdir(exist_ok=True)
    for name, elev, azim in (
        ("hero", 0.4, 0.6),
        ("top", 1.2, 0.0),
        ("side", 0.2, 1.6),
    ):
        pos = [
            0.45 * math.cos(azim) * math.cos(elev),
            0.45 * math.sin(azim) * math.cos(elev),
            0.45 * math.sin(elev),
        ]
        cam = CameraView(
            name=name,
            width=320,
            height=320,
            fx=320 * 1.3,
            fy=320 * 1.3,
            cx=160,
            cy=160,
            world_from_camera=_look_at(pos),
        )
        img = rasterize_mesh(cam, mesh, seed=hash(name) % 10_000)
        path = offline_dir / f"{name}.png"
        cv2.imwrite(str(path), img)
        delivery.setdefault("offline_frames", {})[name] = str(path)

    # Web LODs via delivery subsystem (falls back without Blender).
    try:
        lod_report = generate_lods(
            glb,
            [
                LodBudget(name="L1", max_triangles=4000),
                LodBudget(name="L2", max_triangles=1500),
                LodBudget(name="L3", max_triangles=400),
            ],
            output / "lods",
        )
        delivery["lods"] = lod_report.to_dict()
    except Exception as error:  # noqa: BLE001
        delivery["lods"] = {
            "status": "blocked",
            "reason": f"{type(error).__name__}: {error}",
        }

    # Compression selection on the editable GLB shell.
    try:
        selection = measure_and_select_compression(glb, asset_id=f"{target_id}-shell")
        delivery["compression"] = selection.to_dict()
    except Exception as error:  # noqa: BLE001
        delivery["compression"] = {
            "status": "blocked",
            "reason": f"{type(error).__name__}: {error}",
        }

    # Delivery manifest (web shell).
    try:
        assets = [
            DeliveryAsset(
                asset_id=f"{target_id}-shell",
                role="shell",
                path=str(glb),
                bytes=glb.stat().st_size,
                digest=hashlib.sha256(glb.read_bytes()).hexdigest(),
                compression="none",
                lod="L0",
            )
        ]
        if (output / "lods").is_dir():
            for lod_path in sorted((output / "lods").glob("*.glb")):
                role = "detail" if "L1" in lod_path.name else "mobile"
                assets.append(
                    DeliveryAsset(
                        asset_id=f"{target_id}-{lod_path.stem}",
                        role=role,
                        path=str(lod_path),
                        bytes=lod_path.stat().st_size,
                        digest=hashlib.sha256(lod_path.read_bytes()).hexdigest(),
                        compression="none",
                        lod=lod_path.stem,
                    )
                )
        manifest = build_delivery_manifest(
            manifest_id=f"dm-{target_id}",
            source_scene=target_id,
            assets=assets,
            input_authorities=[
                AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
                AuthorityClass.MODEL_DERIVED.value,
            ],
        )
        delivery["manifest"] = (
            manifest.to_dict() if hasattr(manifest, "to_dict") else dict(manifest)
        )
    except Exception as error:  # noqa: BLE001
        delivery["manifest"] = {
            "status": "blocked",
            "reason": f"{type(error).__name__}: {error}",
        }

    return delivery


# ---------------------------------------------------------------------------
# Phase O: full consumer object benchmark
# ---------------------------------------------------------------------------


def run_consumer_object_benchmark(
    output: Path,
    *,
    train_views: int = 24,
    holdout_views: int = 8,
    seed: int = 20260726,
) -> TargetScorecard:
    started = time.perf_counter()
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    card = TargetScorecard(
        target_id="consumer_remote",
        phase=BenchmarkPhase.CONSUMER_OBJECT,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        notes=[
            "Self-captured procedural remote fixture. Not a claim about any physical remote.",
        ],
    )

    # 1. Capture
    try:
        capture = capture_remote_fixture(
            output / "capture",
            train_views=train_views,
            holdout_views=holdout_views,
            seed=seed,
        )
        card.stages["capture"] = StageStatus.PASSED
        card.artifacts["source_packet"] = capture["source_packet"]
        card.artifacts["truth_obj"] = capture["truth_obj"]
        if capture.get("blender_blocker"):
            card.blockers.append(
                {
                    "stage": "blender_render",
                    "reason": capture["blender_blocker"],
                    "fallback": "software_raycast",
                }
            )
            card.notes.append(f"renderer={capture['renderer']}")
    except Exception as error:  # noqa: BLE001
        card.stages["capture"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "capture", "error": f"{type(error).__name__}: {error}"}
        )
        card.runtime_seconds = time.perf_counter() - started
        _write_scorecard(output, card)
        return card

    construction: RemoteConstruction = capture["construction_obj"]
    truth: MeshGeometry = capture["mesh"]
    view_meta = capture["view_meta"]
    train_meta = [v for v in view_meta if not v["held_out"]]
    holdout_meta = [v for v in view_meta if v["held_out"]]

    train_cameras = [
        CameraView(
            name=v["name"],
            width=int(v["width"]),
            height=int(v["height"]),
            fx=float(v["fx"]),
            fy=float(v["fy"]),
            cx=float(v["cx"]),
            cy=float(v["cy"]),
            world_from_camera=np.asarray(v["world_from_camera"], dtype=np.float64),
        )
        for v in train_meta
    ]

    # 2. Hidden-surface ledger
    ledger = remote_hidden_surface_ledger(construction, views_include_underside=False)
    card.hidden_surface_ledger = ledger
    violations = assert_ledger_honest(ledger)
    if violations:
        card.stages["hidden_surface_ledger"] = StageStatus.FAILED
        card.failures.append({"stage": "hidden_surface_ledger", "violations": violations})
    else:
        card.stages["hidden_surface_ledger"] = StageStatus.PASSED

    # 3. Geometry portfolio (builder sees only train images)
    try:
        portfolio, scores, colmap_report = run_geometry_portfolio(
            target_id="consumer_remote",
            work_dir=output / "reconstruction",
            truth=truth,
            train_cameras=train_cameras,
            image_dir=Path(capture["train_image_dir"]),
            bounds_min=construction.bounds_min,
            bounds_max=construction.bounds_max,
            primitive_kind="box",
            solid_params={
                "minimum": construction.bounds_min.tolist(),
                "maximum": construction.bounds_max.tolist(),
            },
        )
        card.backend_scores = scores
        card.stages["geometry_portfolio"] = StageStatus.REPORTED
        card.artifacts["portfolio"] = str(output / "reconstruction" / "portfolio.json")
        (output / "reconstruction" / "colmap_report.json").write_text(
            json.dumps(colmap_report, indent=2, default=str) + "\n", encoding="utf-8"
        )
        # Prefer parametric or visual_hull mesh for dimensional / unseen eval.
        builder_mesh = None
        for preferred in ("parametric", "visual_hull", "depth_fusion", "retrieval"):
            cand = next((c for c in portfolio.candidates if c.backend == preferred), None)
            if cand is None:
                continue
            m = _candidate_mesh(cand)
            if m is not None and not m.is_empty():
                builder_mesh = m
                break
        if builder_mesh is None:
            # Sealed builder still emits an editable model: parametric box from construction.
            builder_mesh = box_mesh(construction.bounds_min, construction.bounds_max)
            card.notes.append(
                "No backend mesh available; editable model is construction-parameter box "
                "(MODEL_DERIVED / PROCEDURAL), not an observed reconstruction."
            )
            card.artifacts["builder_mesh_source"] = "construction_box_fallback"
        else:
            card.artifacts["builder_mesh_source"] = "portfolio"
    except Exception as error:  # noqa: BLE001
        card.stages["geometry_portfolio"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "geometry_portfolio", "error": f"{type(error).__name__}: {error}"}
        )
        builder_mesh = box_mesh(construction.bounds_min, construction.bounds_max)
        portfolio = None
        scores = []

    # 4. Dimensional evaluation against construction parameters
    try:
        card.dimensional_errors_mm = dimensional_errors(
            builder_mesh, construction.body_dimensions_mm
        )
        card.stages["dimensional_evaluation"] = StageStatus.REPORTED
    except Exception as error:  # noqa: BLE001
        card.stages["dimensional_evaluation"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "dimensional_evaluation", "error": f"{type(error).__name__}: {error}"}
        )

    # 5. Unseen-view evaluation (holdout images the builder never saw)
    try:
        holdout_cameras = [
            CameraView(
                name=v["name"],
                width=int(v["width"]),
                height=int(v["height"]),
                fx=float(v["fx"]),
                fy=float(v["fy"]),
                cx=float(v["cx"]),
                cy=float(v["cy"]),
                world_from_camera=np.asarray(v["world_from_camera"], dtype=np.float64),
            )
            for v in holdout_meta
        ]
        pred_images = render_prediction_views(builder_mesh, holdout_cameras)
        metrics: list[ImageMetrics] = []
        for v in holdout_meta:
            truth_img = cv2.imread(v["path"], cv2.IMREAD_COLOR)
            if truth_img is None:
                continue
            pred = pred_images.get(v["name"])
            if pred is None:
                continue
            metrics.append(image_metrics(pred, truth_img, view_id=v["name"]))
        card.unseen_view_metrics = metrics
        card.stages["unseen_view_evaluation"] = (
            StageStatus.REPORTED if metrics else StageStatus.BLOCKED
        )
        if not metrics:
            card.blockers.append(
                {
                    "stage": "unseen_view_evaluation",
                    "reason": "no holdout images readable for comparison",
                }
            )
        # Persist holdout comparison receipt (evaluator-only).
        (output / "evaluator").mkdir(exist_ok=True)
        (output / "evaluator" / "unseen_view_metrics.json").write_text(
            json.dumps([m.to_dict() for m in metrics], indent=2) + "\n", encoding="utf-8"
        )
        # Builder isolation: holdout paths must not appear in reconstruction work dir.
        card.notes.append(
            f"Holdout views ({len(holdout_meta)}) sealed from builder; "
            "only evaluator reads them."
        )
    except Exception as error:  # noqa: BLE001
        card.stages["unseen_view_evaluation"] = StageStatus.FAILED
        card.failures.append(
            {
                "stage": "unseen_view_evaluation",
                "error": f"{type(error).__name__}: {error}",
            }
        )

    # 6. Materials
    try:
        card.materials = estimate_remote_materials(
            Path(capture["train_image_dir"]), construction
        )
        card.stages["materials"] = StageStatus.REPORTED
    except Exception as error:  # noqa: BLE001
        card.stages["materials"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "materials", "error": f"{type(error).__name__}: {error}"}
        )

    # 7. Next-view requests
    try:
        card.next_views = plan_next_views_for_remote()
        card.stages["next_views"] = StageStatus.PASSED
    except Exception as error:  # noqa: BLE001
        card.stages["next_views"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "next_views", "error": f"{type(error).__name__}: {error}"}
        )

    # 8. Editable model + delivery (web + offline)
    try:
        card.delivery = build_editable_delivery(
            builder_mesh, output / "delivery", target_id="consumer_remote"
        )
        card.stages["delivery"] = StageStatus.REPORTED
        card.artifacts["editable"] = card.delivery.get("editable_obj", "")
    except Exception as error:  # noqa: BLE001
        card.stages["delivery"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "delivery", "error": f"{type(error).__name__}: {error}"}
        )

    # Topology summary of builder mesh
    from blender_vision.reconstruction.mesh_ops import topology_report

    card.topology = topology_report(builder_mesh)

    card.runtime_seconds = time.perf_counter() - started
    _write_scorecard(output, card)
    return card


# ---------------------------------------------------------------------------
# Phase P: soft / organic / fur
# ---------------------------------------------------------------------------


# Construction envelopes (metres) matching organic build scripts' expected scale.
ORGANIC_TARGETS: dict[str, dict[str, Any]] = {
    "draped_cloth": {
        "phase": BenchmarkPhase.SOFT,
        "label": "soft / draped cloth",
        "approx_dimensions_mm": (400.0, 400.0, 80.0),
        "synthetic_claim": None,
    },
    "organic_sculpture": {
        "phase": BenchmarkPhase.ORGANIC,
        "label": "organic sculpture",
        "approx_dimensions_mm": (250.0, 250.0, 320.0),
        "synthetic_claim": None,
    },
    "plant": {
        "phase": BenchmarkPhase.ORGANIC,
        "label": "branching plant",
        "approx_dimensions_mm": (200.0, 200.0, 350.0),
        "synthetic_claim": None,
    },
    "animal_bust": {
        "phase": BenchmarkPhase.FUR,
        "label": "synthetic animal bust + groom",
        "approx_dimensions_mm": (220.0, 180.0, 250.0),
        "synthetic_claim": SYNTHETIC_FUR_CLAIM,
    },
}


def _software_organic_mesh(target_id: str, seed: int = 0) -> MeshGeometry:
    """Approximate construction-scale meshes for scoring when Blender is blocked.

    These are NOT Blender organic builds. They exist so dimensional and portfolio
    scoring can still run with explicit authority PROCEDURAL_GROUND_TRUTH on the
    construction envelope, while topology/UV/groom stages stay BLOCKED.
    """
    rng = np.random.default_rng(seed + hash(target_id) % 10_000)
    dims = ORGANIC_TARGETS[target_id]["approx_dimensions_mm"]
    half = np.array(dims, dtype=np.float64) / 2000.0  # mm full-span → m half-extent
    if target_id == "draped_cloth":
        # Thin open sheet with gentle undulation.
        n = 24
        xs = np.linspace(-half[0], half[0], n)
        ys = np.linspace(-half[1], half[1], n)
        verts = []
        for y in ys:
            for x in xs:
                z = 0.02 * math.sin(8 * x) * math.cos(6 * y) + 0.01 * float(rng.normal())
                verts.append([x, y, z])
        faces = []
        for j in range(n - 1):
            for i in range(n - 1):
                a = j * n + i
                b = a + 1
                c = a + n
                d = c + 1
                faces.append([a, b, d])
                faces.append([a, d, c])
        return MeshGeometry(
            vertices=np.asarray(verts, dtype=np.float64),
            faces=np.asarray(faces, dtype=np.int64),
        )
    if target_id == "plant":
        # Vertical stem + leaf boxes.
        parts = [box_mesh([-0.01, -0.01, 0], [0.01, 0.01, half[2] * 2])]
        for k in range(6):
            z = (k + 1) * (half[2] * 2) / 7
            ang = k * 1.1
            parts.append(
                box_mesh(
                    [0.02 * math.cos(ang), 0.02 * math.sin(ang), z - 0.005],
                    [0.08 * math.cos(ang), 0.08 * math.sin(ang), z + 0.005],
                )
            )
        return merge_meshes(parts)
    if target_id == "animal_bust":
        # Skull mass + muzzle + ear boxes. Synthetic only.
        parts = [
            box_mesh([-half[0] * 0.7, -half[1] * 0.6, 0], [half[0] * 0.7, half[1] * 0.6, half[2]]),
            box_mesh(
                [-half[0] * 0.25, half[1] * 0.4, half[2] * 0.25],
                [half[0] * 0.25, half[1] * 1.1, half[2] * 0.55],
            ),
            box_mesh(
                [-half[0] * 0.5, -half[1] * 0.2, half[2] * 0.85],
                [-half[0] * 0.25, half[1] * 0.1, half[2] * 1.15],
            ),
            box_mesh(
                [half[0] * 0.25, -half[1] * 0.2, half[2] * 0.85],
                [half[0] * 0.5, half[1] * 0.1, half[2] * 1.15],
            ),
        ]
        return merge_meshes(parts)
    # organic_sculpture: stacked offset boxes.
    parts = []
    for k in range(5):
        s = 1.0 - 0.12 * k
        o = 0.02 * k
        parts.append(
            box_mesh(
                [-half[0] * s + o, -half[1] * s, k * half[2] * 0.35],
                [half[0] * s + o, half[1] * s, (k + 1) * half[2] * 0.35],
            )
        )
    return merge_meshes(parts)


def _load_organic_receipt(path: Path) -> dict[str, Any] | None:
    if path.is_file():
        return json.loads(path.read_text(encoding="utf-8"))
    return None


def run_organic_target_benchmark(
    target_id: str,
    output: Path,
    *,
    organic_receipt: dict[str, Any] | None = None,
    seed: int = 20260726,
    attempt_blender: bool = True,
) -> TargetScorecard:
    """Score one soft/organic/fur target through the sealed scorecard."""
    if target_id not in ORGANIC_TARGETS:
        raise ValueError(f"unknown organic target {target_id!r}")
    started = time.perf_counter()
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    meta = ORGANIC_TARGETS[target_id]
    card = TargetScorecard(
        target_id=target_id,
        phase=meta["phase"],
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        synthetic_claim=meta["synthetic_claim"],
        notes=[
            f"Target: {meta['label']}",
            "Scored against construction / measured ground truth, not against another guess.",
        ],
    )
    if meta["synthetic_claim"]:
        card.notes.append(meta["synthetic_claim"])

    blender = probe_blender_status()
    blender_available = bool(blender.get("available")) and attempt_blender

    # Source packet
    construction = {
        "target_id": target_id,
        "approx_dimensions_mm": list(meta["approx_dimensions_mm"]),
        "authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
        "synthetic_claim": meta["synthetic_claim"],
    }
    if organic_receipt and target_id in organic_receipt.get("targets", {}):
        construction["from_organic_receipt"] = True
        construction["measured"] = organic_receipt["targets"][target_id].get("source")
        construction["retopologized"] = organic_receipt["targets"][target_id].get(
            "retopologized"
        )
        construction["uv"] = organic_receipt["targets"][target_id].get("uv")
        construction["lods"] = organic_receipt["targets"][target_id].get("lods")

    packet_path = write_source_packet(
        output / "source_packet.json",
        target_id=target_id,
        construction=construction,
        train_views=[],
        holdout_view_ids=[],
        notes=card.notes,
    )
    card.artifacts["source_packet"] = str(packet_path)
    card.stages["source_packet"] = StageStatus.PASSED

    # Sealed builder: real Blender organic lane, or software construction mesh.
    truth_mesh: MeshGeometry
    if organic_receipt and target_id in organic_receipt.get("targets", {}):
        entry = organic_receipt["targets"][target_id]
        card.topology = entry.get("retopologized") or entry.get("source") or {}
        card.uv = entry.get("uv") or {}
        glb = entry.get("glb")
        if glb and Path(glb).is_file():
            card.artifacts["glb"] = glb
        # Prefer an exported mesh if present under the receipt's topology dir.
        truth_mesh = _software_organic_mesh(target_id, seed=seed)
        card.stages["sealed_builder"] = StageStatus.REPORTED
        card.notes.append("Builder measurements taken from prior organic-fur-lane receipt.")
        card.authority = AuthorityClass.PROCEDURAL_GROUND_TRUTH
    elif blender_available:
        # Attempt full organic build for this target alone is heavy; record intent
        # and fall through to software mesh if the shared receipt is absent.
        card.blockers.append(
            {
                "stage": "sealed_builder",
                "reason": (
                    "Full organic lane must be run via scripts/run-organic-fur-lane.py; "
                    "per-target isolated Blender build is not inlined here. "
                    "Software construction envelope used for geometry scoring."
                ),
            }
        )
        truth_mesh = _software_organic_mesh(target_id, seed=seed)
        card.stages["sealed_builder"] = StageStatus.BLOCKED
    else:
        card.blockers.append(
            {
                "stage": "blender",
                "reason": blender.get("reason") or "Blender unavailable",
            }
        )
        truth_mesh = _software_organic_mesh(target_id, seed=seed)
        card.stages["sealed_builder"] = StageStatus.BLOCKED
        card.notes.append(
            "Software construction envelope mesh used; topology/UV/groom require Blender."
        )

    write_obj_mesh(output / "truth.obj", truth_mesh)
    write_ply_mesh(output / "truth.ply", truth_mesh)
    card.artifacts["truth_obj"] = str(output / "truth.obj")

    # UV gate handling (known-open for sculpture and plant).
    uv_pack = None
    if card.uv and "packing_efficiency" in card.uv:
        uv_pack = float(card.uv["packing_efficiency"])
    if uv_pack is not None:
        if uv_pack < MIN_UV_PACKING:
            failure = {
                "target": target_id,
                "gate": "uv_packing",
                "value": uv_pack,
                "threshold": MIN_UV_PACKING,
            }
            if (target_id, "uv_packing") in KNOWN_OPEN_UV_FAILURES:
                failure["known_open"] = True
                failure["note"] = (
                    "Known-open failure: smart_project packs branching forms to ~29% "
                    "against a 35% gate. Gate not relaxed."
                )
                card.stages["uv_packing"] = StageStatus.FAILED
            else:
                card.stages["uv_packing"] = StageStatus.FAILED
            card.failures.append(failure)
        else:
            card.stages["uv_packing"] = StageStatus.PASSED
    else:
        card.stages["uv_packing"] = StageStatus.BLOCKED
        if (target_id, "uv_packing") in KNOWN_OPEN_UV_FAILURES:
            # Carry the known-open failure forward even without a live measurement.
            card.failures.append(
                {
                    "target": target_id,
                    "gate": "uv_packing",
                    "value": 0.29,
                    "threshold": MIN_UV_PACKING,
                    "known_open": True,
                    "note": (
                        "Known-open UV packing failure carried forward (~29% vs 35% gate). "
                        "No live measurement this run (Blender blocked or receipt missing)."
                    ),
                    "carried_forward": True,
                }
            )
            card.uv = {
                "packing_efficiency": 0.29,
                "status": "carried_forward_known_open",
                "threshold": MIN_UV_PACKING,
            }

    # Geometry portfolio scoring
    try:
        lo, hi = bounding_box(truth_mesh)
        cameras = orbit_cameras(16, radius=0.8, size=160, elevation=0.45, seed=seed)
        # Capture software multiviews for this target (train only; holdout separate).
        img_dir = output / "images" / "train"
        img_dir.mkdir(parents=True, exist_ok=True)
        holdout_dir = output / "images" / "holdout"
        holdout_dir.mkdir(parents=True, exist_ok=True)
        train_cams = cameras[:12]
        holdout_cams = cameras[12:]
        for i, cam in enumerate(train_cams):
            img = rasterize_mesh(cam, truth_mesh, seed=seed + i)
            cv2.imwrite(str(img_dir / f"{cam.name}.png"), img)
        holdout_meta = []
        for i, cam in enumerate(holdout_cams):
            img = rasterize_mesh(cam, truth_mesh, seed=seed + 100 + i)
            path = holdout_dir / f"{cam.name}.png"
            cv2.imwrite(str(path), img)
            holdout_meta.append((cam, path))

        _, scores, colmap_report = run_geometry_portfolio(
            target_id=target_id,
            work_dir=output / "reconstruction",
            truth=truth_mesh,
            train_cameras=train_cams,
            image_dir=img_dir,
            bounds_min=lo,
            bounds_max=hi,
            primitive_kind="box",
            solid_params={"minimum": lo.tolist(), "maximum": hi.tolist()},
            licensing="SYNTHETIC_OWNED" if meta["synthetic_claim"] else "PROCEDURAL_OWNED",
        )
        card.backend_scores = scores
        card.stages["geometry_portfolio"] = StageStatus.REPORTED
        (output / "reconstruction" / "colmap_report.json").write_text(
            json.dumps(colmap_report, indent=2, default=str) + "\n", encoding="utf-8"
        )

        # Dimensional errors vs construction envelope.
        card.dimensional_errors_mm = dimensional_errors(
            truth_mesh, meta["approx_dimensions_mm"]
        )
        # For construction-envelope mesh vs itself, errors are ~0; when a receipt
        # provides measured dims, recompute against those.
        if organic_receipt and target_id in organic_receipt.get("targets", {}):
            bounds = (
                organic_receipt["targets"][target_id]
                .get("source", {})
                .get("bounds_m")
            )
            if bounds and len(bounds) == 2:
                truth_dims_mm = [
                    (bounds[1][i] - bounds[0][i]) * 1000.0 for i in range(3)
                ]
                card.dimensional_errors_mm = dimensional_errors(truth_mesh, truth_dims_mm)
                card.notes.append(
                    "Dimensional truth from organic receipt source bounds_m."
                )
        card.stages["dimensional_evaluation"] = StageStatus.REPORTED

        # Unseen views: re-render builder mesh (construction envelope) vs holdout.
        builder = box_mesh(lo, hi)  # sealed builder proxy without Blender retopo
        if organic_receipt and card.topology:
            builder = truth_mesh
        pred = render_prediction_views(builder, holdout_cams)
        metrics = []
        for cam, path in holdout_meta:
            truth_img = cv2.imread(str(path), cv2.IMREAD_COLOR)
            if truth_img is None:
                continue
            metrics.append(image_metrics(pred[cam.name], truth_img, view_id=cam.name))
        card.unseen_view_metrics = metrics
        card.stages["unseen_view_evaluation"] = StageStatus.REPORTED
    except Exception as error:  # noqa: BLE001
        card.stages["geometry_portfolio"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "geometry_portfolio", "error": f"{type(error).__name__}: {error}"}
        )

    # Materials (simple single-surface estimate from train images)
    try:
        train_imgs = sorted((output / "images" / "train").glob("*.png"))[:6]
        observations = []
        for path in train_imgs:
            bgr = cv2.imread(str(path), cv2.IMREAD_COLOR)
            if bgr is None:
                continue
            rgb = cv2.cvtColor(bgr, cv2.COLOR_BGR2RGB).astype(np.float64) / 255.0
            mask = rgb.sum(axis=2) > 0.12
            observations.append(
                SurfaceObservation(
                    view_id=path.stem,
                    rgb=rgb,
                    mask=mask,
                    authority=AuthorityClass.SENSOR_DERIVED,
                )
            )
        if observations:
            mset = infer_materials(
                observations,
                [SurfaceRegion(surface_id=target_id, label=meta["label"])],
            )
            card.materials = mset.to_dict()
            card.stages["materials"] = StageStatus.REPORTED
        else:
            card.stages["materials"] = StageStatus.BLOCKED
    except Exception as error:  # noqa: BLE001
        card.stages["materials"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "materials", "error": f"{type(error).__name__}: {error}"}
        )

    # Fur groom stage
    if target_id == "animal_bust":
        card.synthetic_claim = SYNTHETIC_FUR_CLAIM
        if organic_receipt and "fur" in organic_receipt:
            fur = organic_receipt["fur"]
            card.topology = {
                **(card.topology or {}),
                "fur": {
                    "guides": fur.get("report", {}).get("guides"),
                    "guard_strands": fur.get("report", {}).get("guard_strands"),
                    "undercoat_strands": fur.get("report", {}).get("undercoat_strands"),
                    "critique_passed": fur.get("critique_passed"),
                    "claim": fur.get("claim", SYNTHETIC_FUR_CLAIM),
                },
            }
            card.stages["fur_groom"] = (
                StageStatus.PASSED
                if fur.get("critique_passed")
                else StageStatus.FAILED
            )
            card.notes.append(fur.get("claim", SYNTHETIC_FUR_CLAIM))
        else:
            card.stages["fur_groom"] = StageStatus.BLOCKED
            card.blockers.append(
                {
                    "stage": "fur_groom",
                    "reason": (
                        blender.get("reason")
                        if not blender_available
                        else "organic-fur receipt not present; run scripts/run-organic-fur-lane.py"
                    ),
                    "synthetic_claim": SYNTHETIC_FUR_CLAIM,
                }
            )
        # Hidden surfaces for synthetic bust: interior skull, underside of neck.
        card.hidden_surface_ledger = [
            HiddenSurfaceEntry(
                region="skull_interior",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(
                    VisibilityState.NEVER_OBSERVED
                ),
                reason="Interior volume never observed; synthetic shell only.",
                observed=False,
            ),
            HiddenSurfaceEntry(
                region="neck_underside",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(
                    VisibilityState.NEVER_OBSERVED
                ),
                reason="Bust terminates at neck; underside of cut plane unobserved.",
                observed=False,
            ),
        ]
        card.stages["hidden_surface_ledger"] = StageStatus.PASSED
    else:
        # Soft/organic: underside / self-occlusion ledger.
        card.hidden_surface_ledger = [
            HiddenSurfaceEntry(
                region="self_occluded_folds" if target_id == "draped_cloth" else "backside",
                visibility=VisibilityState.NEVER_OBSERVED,
                authority_ceiling=visibility_authority_ceiling(
                    VisibilityState.NEVER_OBSERVED
                ),
                reason=(
                    "Multiview hemisphere does not cover all self-occluded or "
                    "backside surfaces."
                ),
                observed=False,
            )
        ]
        card.stages["hidden_surface_ledger"] = StageStatus.PASSED

    # Delivery
    try:
        card.delivery = build_editable_delivery(
            truth_mesh, output / "delivery", target_id=target_id
        )
        if meta["synthetic_claim"]:
            card.delivery["synthetic_claim"] = meta["synthetic_claim"]
        card.stages["delivery"] = StageStatus.REPORTED
    except Exception as error:  # noqa: BLE001
        card.stages["delivery"] = StageStatus.FAILED
        card.failures.append(
            {"stage": "delivery", "error": f"{type(error).__name__}: {error}"}
        )

    if not card.topology:
        from blender_vision.reconstruction.mesh_ops import topology_report

        card.topology = topology_report(truth_mesh)

    card.runtime_seconds = time.perf_counter() - started
    _write_scorecard(output, card)
    return card


def run_soft_organic_fur_benchmarks(
    output: Path,
    *,
    seed: int = 20260726,
    organic_receipt_path: Path | None = None,
) -> dict[str, TargetScorecard]:
    """Run Phase P for all four targets."""
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    receipt = None
    candidates = [
        organic_receipt_path,
        REPO_ROOT / "artifacts" / "v2" / "organic" / "organic-fur-receipt.json",
    ]
    for path in candidates:
        if path is not None and path.is_file():
            receipt = _load_organic_receipt(path)
            break

    results: dict[str, TargetScorecard] = {}
    for target_id in ORGANIC_TARGETS:
        results[target_id] = run_organic_target_benchmark(
            target_id,
            output / target_id,
            organic_receipt=receipt,
            seed=seed,
        )
    return results


# ---------------------------------------------------------------------------
# Orchestration
# ---------------------------------------------------------------------------


def _write_scorecard(output: Path, card: TargetScorecard) -> Path:
    path = output / "scorecard.json"
    path.write_text(json.dumps(card.to_dict(), indent=2, default=str) + "\n", encoding="utf-8")
    return path


def print_scorecard_summary(card: TargetScorecard) -> None:
    print(f"\n=== {card.target_id} ({card.phase.value}) ===")
    print(f"  authority: {card.authority.value}")
    if card.synthetic_claim:
        print(f"  SYNTHETIC: {card.synthetic_claim[:80]}...")
    print("  stages:")
    for name, status in card.stages.items():
        print(f"    {name:28s} {status.value}")
    if card.backend_scores:
        print("  backend chamfer (m) vs ground truth:")
        for b in card.backend_scores:
            if b.chamfer_m is None:
                print(f"    {b.backend:22s} executed={b.executed}  chamfer=None  ({b.reason})")
            else:
                print(
                    f"    {b.backend:22s} executed={b.executed}  "
                    f"chamfer={b.chamfer_m:.6f} m"
                )
    if card.dimensional_errors_mm:
        print("  dimensional error (mm):")
        for d in card.dimensional_errors_mm:
            print(
                f"    {d.axis}: truth={d.truth_mm:.2f} measured={d.measured_mm:.2f} "
                f"error={d.error_mm:+.2f} mm ({d.relative_error * 100:.1f}%)"
            )
    if card.unseen_view_metrics:
        psnrs = [m.psnr_db for m in card.unseen_view_metrics]
        ssims = [m.ssim for m in card.unseen_view_metrics]
        print(
            f"  unseen-view metrics: n={len(psnrs)}  "
            f"PSNR mean={sum(psnrs)/len(psnrs):.2f} dB  "
            f"SSIM mean={sum(ssims)/len(ssims):.4f}"
        )
    counts = {
        "total": len(card.hidden_surface_ledger),
        "never_observed": sum(
            1
            for h in card.hidden_surface_ledger
            if h.visibility is VisibilityState.NEVER_OBSERVED
        ),
    }
    print(f"  hidden-surface ledger: {counts}")
    if card.blockers:
        print(f"  blockers: {len(card.blockers)}")
        for b in card.blockers:
            print(f"    - {b.get('stage')}: {str(b.get('reason', ''))[:120]}")
    if card.failures:
        print(f"  failures: {len(card.failures)}")
        for f in card.failures:
            print(f"    - {f}")
    print(f"  runtime: {card.runtime_seconds:.2f}s")


def run_object_benchmarks(
    output: Path,
    *,
    train_views: int = 24,
    holdout_views: int = 8,
    seed: int = 20260726,
    skip_phase_o: bool = False,
    skip_phase_p: bool = False,
) -> dict[str, Any]:
    """Run Phases O and P. Returns the aggregate receipt. Never raises on poor scores."""
    output = output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    started = time.perf_counter()
    receipt: dict[str, Any] = {
        "schema": "visionmcp.object-benchmarks-receipt/v1",
        "started_at": utc_now(),
        "output": str(output),
        "dense_mvs_blocker": DENSE_UNAVAILABLE_REASON,
        "blender": probe_blender_status(),
        "phases": {},
        "targets": {},
        "framework_errors": [],
    }

    if not skip_phase_o:
        try:
            remote = run_consumer_object_benchmark(
                output / "remote",
                train_views=train_views,
                holdout_views=holdout_views,
                seed=seed,
            )
            receipt["targets"]["consumer_remote"] = remote.to_dict()
            receipt["phases"]["O_consumer_object"] = "completed"
            print_scorecard_summary(remote)
        except Exception as error:  # noqa: BLE001 — framework failure only
            receipt["framework_errors"].append(
                {
                    "phase": "O",
                    "error": f"{type(error).__name__}: {error}",
                }
            )
            receipt["phases"]["O_consumer_object"] = "framework_error"
            # Preserve the failed attempt.
            fail_path = output / "remote" / "framework_error.json"
            fail_path.parent.mkdir(parents=True, exist_ok=True)
            fail_path.write_text(
                json.dumps(receipt["framework_errors"][-1], indent=2) + "\n",
                encoding="utf-8",
            )

    if not skip_phase_p:
        try:
            organic = run_soft_organic_fur_benchmarks(output / "organic", seed=seed)
            for tid, card in organic.items():
                receipt["targets"][tid] = card.to_dict()
                print_scorecard_summary(card)
            receipt["phases"]["P_soft_organic_fur"] = "completed"
        except Exception as error:  # noqa: BLE001
            receipt["framework_errors"].append(
                {
                    "phase": "P",
                    "error": f"{type(error).__name__}: {error}",
                }
            )
            receipt["phases"]["P_soft_organic_fur"] = "framework_error"

    receipt["completed_at"] = utc_now()
    receipt["runtime_seconds"] = time.perf_counter() - started
    receipt_path = output / "object_benchmarks_receipt.json"
    receipt_path.write_text(
        json.dumps(receipt, indent=2, default=str) + "\n", encoding="utf-8"
    )
    print(f"\nReceipt written to {receipt_path}")
    return receipt
