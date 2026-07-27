"""Surface coverage atlas: who saw what, and what was never observed."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.spatial.trajectory import CameraPose, look_at_matrix
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    VisibilityState,
    visibility_authority_ceiling,
)
from blender_vision.v2.records import Lineage, SceneEvidenceGraph


@dataclass(slots=True)
class SurfacePatch:
    """A small oriented surface sample on the target proxy."""

    patch_id: str
    position: np.ndarray
    normal: np.ndarray
    area_m2: float = 1.0
    visibility: VisibilityState = VisibilityState.NEVER_OBSERVED
    observing_cameras: list[str] = field(default_factory=list)
    best_incidence_deg: float | None = None
    hit_count: int = 0

    def to_dict(self) -> dict[str, Any]:
        return {
            "patch_id": self.patch_id,
            "position": self.position.tolist(),
            "normal": self.normal.tolist(),
            "area_m2": self.area_m2,
            "visibility": self.visibility.value,
            "observing_cameras": list(self.observing_cameras),
            "best_incidence_deg": self.best_incidence_deg,
            "hit_count": self.hit_count,
            "authority_ceiling": visibility_authority_ceiling(self.visibility).value,
        }


@dataclass(slots=True)
class CoverageReport:
    """Aggregate coverage fractions over a surface atlas."""

    patches: list[SurfacePatch]
    covered_fraction: float
    partially_covered_fraction: float
    never_observed_fraction: float
    frame: CoordinateFrame
    camera_count: int
    parameters: dict[str, Any] = field(default_factory=dict)

    def by_visibility(self) -> dict[str, list[SurfacePatch]]:
        buckets: dict[str, list[SurfacePatch]] = {}
        for patch in self.patches:
            buckets.setdefault(patch.visibility.value, []).append(patch)
        return buckets

    def to_dict(self) -> dict[str, Any]:
        return {
            "covered_fraction": self.covered_fraction,
            "partially_covered_fraction": self.partially_covered_fraction,
            "never_observed_fraction": self.never_observed_fraction,
            "camera_count": self.camera_count,
            "patch_count": len(self.patches),
            "frame": self.frame.to_dict(),
            "parameters": dict(self.parameters),
            "patches": [patch.to_dict() for patch in self.patches],
        }

    def seal_scene_evidence(
        self,
        *,
        target_id: str,
        observation_bundle_ids: list[str] | None = None,
        operation: str = "spatial.coverage.atlas",
    ) -> SceneEvidenceGraph:
        nodes = []
        visibility: dict[str, str] = {}
        for patch in self.patches:
            ceiling = visibility_authority_ceiling(patch.visibility)
            nodes.append(
                {
                    "id": patch.patch_id,
                    "kind": "surface_patch",
                    "authority": ceiling.value,
                    "position": patch.position.tolist(),
                    "normal": patch.normal.tolist(),
                }
            )
            visibility[patch.patch_id] = patch.visibility.value
        # Coverage claims are derived from the observing cameras' authority,
        # but never stronger than the visibility ceiling per patch.
        graph = SceneEvidenceGraph(
            id=f"cov-{uuid.uuid4().hex[:12]}",
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=Lineage(
                operation=operation,
                inputs=list(observation_bundle_ids or []),
                input_authorities=[],
                parameters={
                    "target_id": target_id,
                    "covered_fraction": self.covered_fraction,
                    "never_observed_fraction": self.never_observed_fraction,
                    "claimed_authority": AuthorityClass.SENSOR_DERIVED.value,
                    **self.parameters,
                },
                environment={"frame": self.frame.to_dict()},
                limitations=[
                    "proxy geometry only; does not invent unobserved structure",
                    "NEVER_OBSERVED patches must not be labelled OBSERVED",
                ],
            ),
            uncertainty=Uncertainty(
                kind="coverage-fraction",
                sigma=None,
                units=Units.NORMALIZED,
                basis="discrete surface-patch ray casting",
                samples=len(self.patches),
            ),
            frame=self.frame,
            nodes=nodes,
            visibility=visibility,
            observation_bundles=list(observation_bundle_ids or []),
            cameras=[],
        )
        return graph.seal()


@dataclass(slots=True)
class CoverageAtlas:
    """Ray-cast coverage of a target proxy against a camera set."""

    frame: CoordinateFrame = field(default_factory=lambda: BLENDER_WORLD)
    max_incidence_deg: float = 75.0
    partial_hit_threshold: int = 1
    covered_hit_threshold: int = 2

    def sample_box_patches(
        self,
        bounds_min: np.ndarray,
        bounds_max: np.ndarray,
        *,
        resolution: int = 8,
    ) -> list[SurfacePatch]:
        """Axis-aligned box surface patches (six faces)."""
        lo = np.asarray(bounds_min, dtype=np.float64)
        hi = np.asarray(bounds_max, dtype=np.float64)
        if np.any(hi <= lo):
            raise ValidationError("bounds_max must be strictly greater than bounds_min")
        if resolution < 1:
            raise ValidationError("resolution must be >= 1")
        patches: list[SurfacePatch] = []
        # Face specs: (axis, sign, constant_value_index setup)
        faces = [
            (0, -1.0, lo[0]),  # -X
            (0, +1.0, hi[0]),  # +X
            (1, -1.0, lo[1]),  # -Y
            (1, +1.0, hi[1]),  # +Y
            (2, -1.0, lo[2]),  # -Z underside
            (2, +1.0, hi[2]),  # +Z top
        ]
        dims = hi - lo
        for face_index, (axis, sign, constant) in enumerate(faces):
            other = [i for i in range(3) if i != axis]
            u_vals = np.linspace(lo[other[0]], hi[other[0]], resolution + 2)[1:-1]
            v_vals = np.linspace(lo[other[1]], hi[other[1]], resolution + 2)[1:-1]
            if u_vals.size == 0:
                u_vals = np.array([(lo[other[0]] + hi[other[0]]) * 0.5])
            if v_vals.size == 0:
                v_vals = np.array([(lo[other[1]] + hi[other[1]]) * 0.5])
            du = dims[other[0]] / max(1, resolution)
            dv = dims[other[1]] / max(1, resolution)
            area = float(du * dv)
            normal = np.zeros(3, dtype=np.float64)
            normal[axis] = sign
            for ui, u in enumerate(u_vals):
                for vi, v in enumerate(v_vals):
                    position = np.zeros(3, dtype=np.float64)
                    position[axis] = constant
                    position[other[0]] = u
                    position[other[1]] = v
                    patches.append(
                        SurfacePatch(
                            patch_id=f"face{face_index}-u{ui}-v{vi}",
                            position=position,
                            normal=normal.copy(),
                            area_m2=area,
                        )
                    )
        return patches

    def sample_sphere_patches(
        self,
        center: np.ndarray,
        radius: float,
        *,
        n_lat: int = 8,
        n_lon: int = 16,
    ) -> list[SurfacePatch]:
        """Fibonacci-ish latitude/longitude sphere samples."""
        center = np.asarray(center, dtype=np.float64)
        if radius <= 0:
            raise ValidationError("radius must be positive")
        patches: list[SurfacePatch] = []
        for lat_i in range(n_lat):
            # Exclude exact poles to keep area roughly uniform; include near-poles.
            lat = np.pi * (lat_i + 0.5) / n_lat - np.pi / 2  # -pi/2 .. pi/2
            for lon_i in range(n_lon):
                lon = 2 * np.pi * lon_i / n_lon
                normal = np.array(
                    [
                        np.cos(lat) * np.cos(lon),
                        np.cos(lat) * np.sin(lon),
                        np.sin(lat),
                    ],
                    dtype=np.float64,
                )
                position = center + radius * normal
                # dA ≈ R^2 cos(lat) dlat dlon
                area = float(
                    radius**2 * abs(np.cos(lat)) * (np.pi / n_lat) * (2 * np.pi / n_lon)
                )
                patches.append(
                    SurfacePatch(
                        patch_id=f"sph-lat{lat_i}-lon{lon_i}",
                        position=position,
                        normal=normal,
                        area_m2=area,
                    )
                )
        return patches

    def evaluate(
        self,
        patches: list[SurfacePatch],
        cameras: list[dict[str, Any] | CameraPose],
        *,
        proxy_triangles: np.ndarray | None = None,
        proxy_vertices: np.ndarray | None = None,
        max_range: float = 100.0,
    ) -> CoverageReport:
        """Classify each patch by ray visibility from the camera set.

        A camera observes a patch when:
        1. the patch is in front of the camera,
        2. the viewing ray hits the front-facing side (incidence within limit),
        3. no proxy triangle occludes the ray (if a mesh is supplied).

        Without a proxy mesh, only the geometric incidence test is applied —
        self-occlusion is not invented.
        """
        if not patches:
            raise ValidationError("coverage requires at least one surface patch")
        cam_records = [_normalize_camera(cam, index=i) for i, cam in enumerate(cameras)]
        # Work on copies so the caller's patches stay pristine.
        working = [
            SurfacePatch(
                patch_id=p.patch_id,
                position=p.position.copy(),
                normal=p.normal.copy(),
                area_m2=p.area_m2,
            )
            for p in patches
        ]

        for cam in cam_records:
            eye = cam["position"]
            # Camera look for frustum-ish front test.
            rotation = cam["rotation"]
            look = rotation @ np.array([0.0, 0.0, -1.0])
            for patch in working:
                to_patch = patch.position - eye
                distance = float(np.linalg.norm(to_patch))
                if distance < 1e-9 or distance > max_range:
                    continue
                view_dir = to_patch / distance
                # Must be roughly in front of the camera.
                if float(np.dot(view_dir, look)) <= 0.05:
                    continue
                # Incidence: angle between -view_dir (from surface to camera) and normal.
                to_camera = -view_dir
                cos_inc = float(np.dot(patch.normal, to_camera))
                if cos_inc <= 0.0:
                    # Back-face: this camera cannot observe the front of the patch.
                    continue
                incidence = float(np.degrees(np.arccos(np.clip(cos_inc, -1.0, 1.0))))
                if incidence > self.max_incidence_deg:
                    continue
                if (
                    proxy_vertices is not None
                    and proxy_triangles is not None
                    and _ray_occluded(
                        eye,
                        patch.position,
                        proxy_vertices,
                        proxy_triangles,
                        exclude_endpoint_eps=1e-4,
                    )
                ):
                    continue
                patch.hit_count += 1
                patch.observing_cameras.append(cam["label"])
                if patch.best_incidence_deg is None or incidence < patch.best_incidence_deg:
                    patch.best_incidence_deg = incidence

        for patch in working:
            if patch.hit_count >= self.covered_hit_threshold:
                patch.visibility = VisibilityState.DIRECTLY_VISIBLE
            elif patch.hit_count >= self.partial_hit_threshold:
                patch.visibility = VisibilityState.PARTIALLY_VISIBLE
            else:
                patch.visibility = VisibilityState.NEVER_OBSERVED

        total = len(working)
        covered = sum(
            1 for p in working if p.visibility is VisibilityState.DIRECTLY_VISIBLE
        )
        partial = sum(
            1 for p in working if p.visibility is VisibilityState.PARTIALLY_VISIBLE
        )
        never = sum(
            1 for p in working if p.visibility is VisibilityState.NEVER_OBSERVED
        )
        return CoverageReport(
            patches=working,
            covered_fraction=covered / total,
            partially_covered_fraction=partial / total,
            never_observed_fraction=never / total,
            frame=self.frame,
            camera_count=len(cam_records),
            parameters={
                "max_incidence_deg": self.max_incidence_deg,
                "partial_hit_threshold": self.partial_hit_threshold,
                "covered_hit_threshold": self.covered_hit_threshold,
                "max_range": max_range,
                "proxy_mesh": proxy_triangles is not None,
            },
        )


def _normalize_camera(camera: dict[str, Any] | CameraPose, *, index: int) -> dict[str, Any]:
    if isinstance(camera, CameraPose):
        return {
            "label": camera.label or f"cam-{index}",
            "position": camera.position,
            "rotation": camera.rotation,
            "world_from_camera": camera.world_from_camera,
        }
    if "world_from_camera" in camera:
        matrix = np.asarray(camera["world_from_camera"], dtype=np.float64)
        return {
            "label": str(camera.get("label", camera.get("id", f"cam-{index}"))),
            "position": matrix[:3, 3].copy(),
            "rotation": matrix[:3, :3].copy(),
            "world_from_camera": matrix,
        }
    if "position" in camera and "target" in camera:
        matrix = look_at_matrix(
            np.asarray(camera["position"], dtype=np.float64),
            np.asarray(camera["target"], dtype=np.float64),
            np.asarray(camera.get("up", (0.0, 0.0, 1.0)), dtype=np.float64),
        )
        return {
            "label": str(camera.get("label", f"cam-{index}")),
            "position": matrix[:3, 3].copy(),
            "rotation": matrix[:3, :3].copy(),
            "world_from_camera": matrix,
        }
    raise ValidationError(
        "camera must provide world_from_camera or position+target"
    )


def _ray_occluded(
    origin: np.ndarray,
    target: np.ndarray,
    vertices: np.ndarray,
    triangles: np.ndarray,
    *,
    exclude_endpoint_eps: float = 1e-4,
) -> bool:
    """True if any triangle intersects the open segment (origin, target)."""
    direction = target - origin
    length = float(np.linalg.norm(direction))
    if length < 1e-12:
        return False
    direction = direction / length
    for tri in triangles:
        hit_t = _ray_triangle_t(
            origin, direction, vertices[int(tri[0])], vertices[int(tri[1])], vertices[int(tri[2])]
        )
        if hit_t is None:
            continue
        # Occluder must be strictly between origin and target.
        if exclude_endpoint_eps < hit_t < length - exclude_endpoint_eps:
            return True
    return False


def _ray_triangle_t(
    origin: np.ndarray,
    direction: np.ndarray,
    v0: np.ndarray,
    v1: np.ndarray,
    v2: np.ndarray,
    *,
    eps: float = 1e-9,
) -> float | None:
    """Möller–Trumbore; returns distance t along ray or None."""
    edge1 = v1 - v0
    edge2 = v2 - v0
    pvec = np.cross(direction, edge2)
    det = float(np.dot(edge1, pvec))
    if abs(det) < eps:
        return None
    inv_det = 1.0 / det
    tvec = origin - v0
    u = float(np.dot(tvec, pvec)) * inv_det
    if u < 0.0 or u > 1.0:
        return None
    qvec = np.cross(tvec, edge1)
    v = float(np.dot(direction, qvec)) * inv_det
    if v < 0.0 or u + v > 1.0:
        return None
    t = float(np.dot(edge2, qvec)) * inv_det
    if t < 0.0:
        return None
    return t
