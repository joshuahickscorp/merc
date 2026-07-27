"""Silhouette visual-hull carving with marching-cubes surface extraction."""

from __future__ import annotations

from typing import Any

import numpy as np
from skimage import measure

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    CameraView,
    MeshGeometry,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import topology_report, write_ply_mesh
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate


class VisualHullBackend:
    """Voxel silhouette carving. Does not invent unobserved concavities."""

    name = "visual_hull"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="numpy + scikit-image marching cubes available",
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        if not inputs.masks or not inputs.cameras:
            return unavailable_candidate(
                backend=self.name,
                reason="visual_hull requires masks and cameras",
                inputs=inputs,
            )
        if len(inputs.masks) != len(inputs.cameras):
            return unavailable_candidate(
                backend=self.name,
                reason="mask count must equal camera count",
                inputs=inputs,
            )
        if inputs.bounds_min is None or inputs.bounds_max is None:
            return unavailable_candidate(
                backend=self.name,
                reason="visual_hull requires bounds_min and bounds_max",
                inputs=inputs,
            )

        resolution = int(inputs.parameters.get("grid_resolution", 48))
        resolution = max(8, min(resolution, 128))
        with TimedRun() as timer:
            occupancy = carve_occupancy(
                masks=inputs.masks,
                cameras=inputs.cameras,
                bounds_min=np.asarray(inputs.bounds_min, dtype=np.float64),
                bounds_max=np.asarray(inputs.bounds_max, dtype=np.float64),
                resolution=resolution,
            )
            if not np.any(occupancy):
                return unavailable_candidate(
                    backend=self.name,
                    reason="visual hull intersection is empty",
                    inputs=inputs,
                )
            mesh = occupancy_to_mesh(
                occupancy,
                bounds_min=np.asarray(inputs.bounds_min, dtype=np.float64),
                bounds_max=np.asarray(inputs.bounds_max, dtype=np.float64),
            )
            work = inputs.ensure_work_dir()
            mesh_path = write_ply_mesh(work / "visual_hull.ply", mesh)
            report = topology_report(mesh)

        authority = derive(
            inputs.input_authorities or [AuthorityClass.SENSOR_DERIVED],
            proposed=AuthorityClass.SENSOR_DERIVED,
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=inputs.frame.scale_authority,
            scale_state=(
                "metric"
                if inputs.frame.scale_authority
                not in {AuthorityClass.UNRESOLVED, AuthorityClass.HYPOTHETICAL}
                else "unresolved"
            ),
            coverage={
                "view_count": len(inputs.cameras),
                "occupied_voxels": int(occupancy.sum()),
                "total_voxels": int(occupancy.size),
                "occupancy_fraction": float(occupancy.mean()),
                "grid_resolution": resolution,
                "method": "silhouette_intersection",
            },
            topology_state=report,
            editability="voxel-grid; not parametric",
            hidden_surface_assumptions=[
                "visual hull is the intersection of silhouette cones; "
                "unobserved concavities are filled as solid",
            ],
            artifacts={"mesh_ply": str(mesh_path)},
            runtime_seconds=timer.seconds,
            execution_log=(
                f"carved {int(occupancy.sum())}/{int(occupancy.size)} voxels "
                f"from {len(inputs.cameras)} views; "
                f"mesh V={report['vertex_count']} F={report['face_count']}"
            ),
            failure_modes=[
                "cannot recover concavities hidden from all silhouettes",
                "metric accuracy requires metric cameras and bounds",
            ],
            executed=True,
        )


def carve_occupancy(
    *,
    masks: list[np.ndarray],
    cameras: list[CameraView],
    bounds_min: np.ndarray,
    bounds_max: np.ndarray,
    resolution: int,
) -> np.ndarray:
    """Return a boolean occupancy grid (True = inside all silhouettes)."""
    xs = np.linspace(bounds_min[0], bounds_max[0], resolution, endpoint=False)
    ys = np.linspace(bounds_min[1], bounds_max[1], resolution, endpoint=False)
    zs = np.linspace(bounds_min[2], bounds_max[2], resolution, endpoint=False)
    # Sample at voxel centres.
    step = (bounds_max - bounds_min) / resolution
    xs = xs + 0.5 * step[0]
    ys = ys + 0.5 * step[1]
    zs = zs + 0.5 * step[2]
    grid = np.ones((resolution, resolution, resolution), dtype=bool)
    xx, yy, zz = np.meshgrid(xs, ys, zs, indexing="ij")
    points = np.stack([xx.ravel(), yy.ravel(), zz.ravel()], axis=1)
    for mask, camera in zip(masks, cameras, strict=True):
        inside = project_inside_mask(points, camera, mask)
        grid &= inside.reshape(resolution, resolution, resolution)
    return grid


def project_inside_mask(points: np.ndarray, camera: CameraView, mask: np.ndarray) -> np.ndarray:
    """Project world points into a mask; True when foreground and in front of camera."""
    cam_from_world = camera.camera_from_world()
    homo = np.concatenate([points, np.ones((len(points), 1))], axis=1)
    cam = (cam_from_world @ homo.T).T
    # Blender camera looks along -Z; depth is -z.
    depth = -cam[:, 2]
    valid = (depth > camera.near) & (depth < camera.far)
    u = camera.fx * (cam[:, 0] / np.maximum(depth, 1e-12)) + camera.cx
    v = camera.cy - camera.fy * (cam[:, 1] / np.maximum(depth, 1e-12))
    # Mask may be downsampled relative to camera resolution.
    mask_arr = np.asarray(mask)
    if mask_arr.ndim == 3:
        mask_arr = mask_arr[..., 0]
    scale_u = mask_arr.shape[1] / camera.width
    scale_v = mask_arr.shape[0] / camera.height
    col = np.rint(u * scale_u).astype(np.int64)
    row = np.rint(v * scale_v).astype(np.int64)
    in_bounds = (
        valid
        & (col >= 0)
        & (row >= 0)
        & (col < mask_arr.shape[1])
        & (row < mask_arr.shape[0])
    )
    result = np.zeros(len(points), dtype=bool)
    if not np.any(in_bounds):
        return result
    samples = mask_arr[row[in_bounds], col[in_bounds]]
    result[in_bounds] = samples > 0
    return result


def occupancy_to_mesh(
    occupancy: np.ndarray,
    *,
    bounds_min: np.ndarray,
    bounds_max: np.ndarray,
) -> MeshGeometry:
    """Extract an isosurface from occupancy via marching cubes."""
    # Pad so the surface closes at the volume boundary when occupancy touches edges.
    padded = np.pad(occupancy.astype(np.float64), 1, mode="constant", constant_values=0.0)
    spacing = (bounds_max - bounds_min) / np.array(occupancy.shape, dtype=np.float64)
    verts, faces, _normals, _values = measure.marching_cubes(
        padded, level=0.5, spacing=tuple(spacing)
    )
    # Undo padding offset.
    verts = verts - spacing
    verts = verts + bounds_min
    return MeshGeometry(vertices=verts.astype(np.float64), faces=faces.astype(np.int64))


def synthetic_silhouette_masks(
    *,
    cameras: list[CameraView],
    solid: str,
    solid_params: dict[str, Any],
    image_size: tuple[int, int] | None = None,
) -> list[np.ndarray]:
    """Render analytic silhouettes for tests (sphere or axis-aligned box)."""
    masks: list[np.ndarray] = []
    for camera in cameras:
        h = image_size[1] if image_size else camera.height
        w = image_size[0] if image_size else camera.width
        ys, xs = np.mgrid[0:h, 0:w]
        # Pixel rays in Blender camera space: +X right, +Y up, -Z forward.
        x = (xs - camera.cx) / camera.fx
        y = (camera.cy - ys) / camera.fy
        dirs_cam = np.stack([x, y, -np.ones_like(x)], axis=-1)
        dirs_cam = dirs_cam / np.linalg.norm(dirs_cam, axis=-1, keepdims=True)
        R = camera.world_from_camera[:3, :3]
        origin = camera.world_from_camera[:3, 3]
        dirs_world = dirs_cam @ R.T
        if solid == "sphere":
            center = np.asarray(solid_params["center"], dtype=np.float64)
            radius = float(solid_params["radius"])
            oc = origin - center
            b = 2.0 * np.einsum("ijk,k->ij", dirs_world, oc)
            c = float(np.dot(oc, oc) - radius * radius)
            disc = b * b - 4.0 * c
            mask = disc >= 0
        elif solid == "box":
            minimum = np.asarray(solid_params["minimum"], dtype=np.float64)
            maximum = np.asarray(solid_params["maximum"], dtype=np.float64)
            mask = _ray_box_hits(origin, dirs_world, minimum, maximum)
        else:
            raise ValueError(f"unknown solid {solid!r}")
        masks.append(mask.astype(np.uint8) * 255)
    return masks


def _ray_box_hits(
    origin: np.ndarray,
    directions: np.ndarray,
    minimum: np.ndarray,
    maximum: np.ndarray,
) -> np.ndarray:
    inv = 1.0 / np.where(np.abs(directions) < 1e-12, 1e-12, directions)
    t0 = (minimum - origin) * inv
    t1 = (maximum - origin) * inv
    tmin = np.minimum(t0, t1).max(axis=-1)
    tmax = np.maximum(t0, t1).min(axis=-1)
    return (tmax >= tmin) & (tmax > 0)
