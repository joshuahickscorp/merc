"""TSDF volumetric fusion of posed depth maps."""

from __future__ import annotations

from dataclasses import dataclass

import numpy as np
from skimage import measure

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    DepthFrame,
    MeshGeometry,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import topology_report, write_ply_mesh
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate


@dataclass(slots=True)
class TSDFVolume:
    """Truncated signed-distance volume with per-voxel weights."""

    origin: np.ndarray
    voxel_size: float
    truncation: float
    resolution: tuple[int, int, int]
    tsdf: np.ndarray
    weight: np.ndarray

    @classmethod
    def create(
        cls,
        *,
        bounds_min: np.ndarray,
        bounds_max: np.ndarray,
        voxel_size: float,
        truncation: float | None = None,
    ) -> TSDFVolume:
        bounds_min = np.asarray(bounds_min, dtype=np.float64)
        bounds_max = np.asarray(bounds_max, dtype=np.float64)
        dims = np.ceil((bounds_max - bounds_min) / voxel_size).astype(int)
        dims = np.maximum(dims, 2)
        truncation = float(truncation if truncation is not None else 3.0 * voxel_size)
        shape = (int(dims[0]), int(dims[1]), int(dims[2]))
        return cls(
            origin=bounds_min.copy(),
            voxel_size=float(voxel_size),
            truncation=truncation,
            resolution=shape,
            tsdf=np.ones(shape, dtype=np.float32),
            weight=np.zeros(shape, dtype=np.float32),
        )

    def integrate(self, frame: DepthFrame) -> None:
        """Integrate one depth frame using OpenCV camera convention (+Z forward)."""
        depth = np.asarray(frame.depth, dtype=np.float64)
        camera = frame.camera
        cam_from_world = camera.camera_from_world()
        nx, ny, nz = self.resolution
        xs = self.origin[0] + (np.arange(nx) + 0.5) * self.voxel_size
        ys = self.origin[1] + (np.arange(ny) + 0.5) * self.voxel_size
        zs = self.origin[2] + (np.arange(nz) + 0.5) * self.voxel_size
        h, w = depth.shape[:2]
        for iz, z in enumerate(zs):
            xx, yy = np.meshgrid(xs, ys, indexing="ij")
            pts = np.stack([xx.ravel(), yy.ravel(), np.full(xx.size, z)], axis=1)
            homo = np.concatenate([pts, np.ones((len(pts), 1))], axis=1)
            cam = (cam_from_world @ homo.T).T
            z_cam = cam[:, 2]
            # OpenCV: positive depth along +Z. Blender-looking cameras yield -Z.
            if float(np.mean(z_cam)) < 0:
                optical = -z_cam
                u = camera.fx * (cam[:, 0] / np.maximum(optical, 1e-12)) + camera.cx
                v = camera.cy - camera.fy * (cam[:, 1] / np.maximum(optical, 1e-12))
            else:
                optical = z_cam
                u = camera.fx * (cam[:, 0] / np.maximum(optical, 1e-12)) + camera.cx
                v = camera.fy * (cam[:, 1] / np.maximum(optical, 1e-12)) + camera.cy
            col = np.rint(u).astype(np.int64)
            row = np.rint(v).astype(np.int64)
            in_img = (
                (optical > camera.near)
                & (col >= 0)
                & (row >= 0)
                & (col < w)
                & (row < h)
            )
            if not np.any(in_img):
                continue
            measured = np.zeros(len(pts), dtype=np.float64)
            measured[in_img] = depth[row[in_img], col[in_img]]
            if frame.valid_mask is not None:
                mask = np.asarray(frame.valid_mask)
                keep = in_img.copy()
                keep[in_img] &= mask[row[in_img], col[in_img]].astype(bool)
                in_img = keep
            sdf = measured - optical
            truncated = np.clip(sdf / self.truncation, -1.0, 1.0)
            use = in_img & (measured > 0) & (np.abs(sdf) <= self.truncation)
            if not np.any(use):
                continue
            idx = np.flatnonzero(use)
            # ravel order for meshgrid(..., indexing="ij") over (xs, ys) is ix-major.
            ix = idx // ny
            iy = idx % ny
            w_old = self.weight[ix, iy, iz]
            w_new = w_old + 1.0
            self.tsdf[ix, iy, iz] = (
                self.tsdf[ix, iy, iz] * w_old + truncated[use].astype(np.float32)
            ) / np.maximum(w_new, 1e-6)
            self.weight[ix, iy, iz] = w_new

    def extract_mesh(self, *, min_weight: float = 1.0) -> MeshGeometry:
        volume = self.tsdf.copy()
        volume[self.weight < min_weight] = 1.0
        if not np.any(self.weight >= min_weight):
            return MeshGeometry(
                vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=np.int64)
            )
        if not (volume.min() < 0 < volume.max()):
            # No zero-crossing: surface may sit entirely outside truncation.
            return MeshGeometry(
                vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=np.int64)
            )
        try:
            verts, faces, _n, _v = measure.marching_cubes(
                volume,
                level=0.0,
                spacing=(self.voxel_size, self.voxel_size, self.voxel_size),
                mask=self.weight >= min_weight,
            )
        except ValueError:
            return MeshGeometry(
                vertices=np.zeros((0, 3)), faces=np.zeros((0, 3), dtype=np.int64)
            )
        verts = verts + self.origin
        return MeshGeometry(vertices=verts.astype(np.float64), faces=faces.astype(np.int64))


class DepthFusionBackend:
    """Fuse posed depth maps into a TSDF and extract a surface."""

    name = "depth_fusion"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="numpy TSDF + scikit-image marching cubes available",
            details={
                "spatial_package": False,
                "note": "no spatial package on branch; local depth loader defined here",
            },
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        if not inputs.depth_frames:
            return unavailable_candidate(
                backend=self.name,
                reason="depth_fusion requires depth_frames",
                inputs=inputs,
            )
        if inputs.bounds_min is None or inputs.bounds_max is None:
            bounds_min, bounds_max = estimate_bounds_from_depth(inputs.depth_frames)
        else:
            bounds_min = np.asarray(inputs.bounds_min, dtype=np.float64)
            bounds_max = np.asarray(inputs.bounds_max, dtype=np.float64)

        voxel_size = float(inputs.parameters.get("voxel_size", 0.01))
        truncation = float(inputs.parameters.get("truncation", 3.0 * voxel_size))
        with TimedRun() as timer:
            volume = TSDFVolume.create(
                bounds_min=bounds_min,
                bounds_max=bounds_max,
                voxel_size=voxel_size,
                truncation=truncation,
            )
            for frame in inputs.depth_frames:
                volume.integrate(frame)
            mesh = volume.extract_mesh()
            if mesh.is_empty():
                return unavailable_candidate(
                    backend=self.name,
                    reason="TSDF fusion produced no zero-crossing surface",
                    inputs=inputs,
                )
            work = inputs.ensure_work_dir()
            mesh_path = write_ply_mesh(work / "depth_fusion.ply", mesh)
            report = topology_report(mesh)
            report["voxel_size"] = voxel_size
            report["truncation"] = truncation
            report["weighted_voxels"] = int((volume.weight > 0).sum())

        authority = derive(
            inputs.input_authorities or [AuthorityClass.SENSOR_DERIVED],
            proposed=AuthorityClass.SENSOR_DERIVED,
        )
        scale_auth = (
            inputs.frame.scale_authority
            if inputs.frame.scale_authority is not AuthorityClass.UNRESOLVED
            else AuthorityClass.SENSOR_DERIVED
        )
        return finalize_candidate(
            backend=self.name,
            inputs=inputs,
            authority=authority,
            scale_authority=scale_auth,
            scale_state=(
                "metric"
                if scale_auth is not AuthorityClass.UNRESOLVED
                else "sensor-relative"
            ),
            coverage={
                "depth_frame_count": len(inputs.depth_frames),
                "voxel_size": voxel_size,
                "truncation": truncation,
                "volume_resolution": list(volume.resolution),
                "weighted_voxels": int((volume.weight > 0).sum()),
            },
            topology_state=report,
            editability="implicit-volume; remesh required for edit",
            hidden_surface_assumptions=[
                "unobserved voxels retain free-space prior (TSDF=+1)",
                "truncation limits the influence of each depth sample",
            ],
            artifacts={"mesh_ply": str(mesh_path)},
            runtime_seconds=timer.seconds,
            execution_log=(
                f"fused {len(inputs.depth_frames)} depth maps; "
                f"voxel_size={voxel_size:.5g} truncation={truncation:.5g}; "
                f"mesh V={report['vertex_count']} F={report['face_count']}"
            ),
            failure_modes=[
                "depth noise thicker than truncation yields soft surfaces",
                "missing views leave free-space holes or open boundaries",
            ],
            executed=True,
        )


def estimate_bounds_from_depth(frames: list[DepthFrame]) -> tuple[np.ndarray, np.ndarray]:
    points: list[np.ndarray] = []
    for frame in frames:
        depth = np.asarray(frame.depth, dtype=np.float64)
        camera = frame.camera
        ys, xs = np.where(depth > 0)
        if len(xs) == 0:
            continue
        # Subsample for speed.
        step = max(1, len(xs) // 5000)
        xs = xs[::step]
        ys = ys[::step]
        z = depth[ys, xs]
        x = (xs - camera.cx) * z / camera.fx
        y = (ys - camera.cy) * z / camera.fy
        cam_pts = np.stack([x, y, z], axis=1)
        R = camera.world_from_camera[:3, :3]
        t = camera.world_from_camera[:3, 3]
        world = cam_pts @ R.T + t
        points.append(world)
    if not points:
        raise ValueError("no valid depth samples to estimate bounds")
    cloud = np.concatenate(points, axis=0)
    margin = 0.05 * (cloud.max(axis=0) - cloud.min(axis=0) + 1e-6)
    return cloud.min(axis=0) - margin, cloud.max(axis=0) + margin


def analytic_plane_depth(
    camera: object,
    *,
    plane_z: float = 0.0,
    plane_normal: np.ndarray | None = None,
) -> np.ndarray:
    """Depth map of an infinite plane for tests (OpenCV +Z camera looking at plane)."""
    from blender_vision.reconstruction.base import CameraView

    assert isinstance(camera, CameraView)
    normal = (
        np.asarray(plane_normal, dtype=np.float64)
        if plane_normal is not None
        else np.array([0.0, 0.0, 1.0])
    )
    normal = normal / np.linalg.norm(normal)
    # Plane: normal · x = plane_z when normal is +Z and plane is z=plane_z.
    plane_point = normal * float(plane_z)
    h, w = camera.height, camera.width
    ys, xs = np.mgrid[0:h, 0:w]
    # OpenCV camera rays.
    dirs = np.stack(
        [
            (xs - camera.cx) / camera.fx,
            (ys - camera.cy) / camera.fy,
            np.ones_like(xs, dtype=np.float64),
        ],
        axis=-1,
    )
    dirs = dirs / np.linalg.norm(dirs, axis=-1, keepdims=True)
    R = camera.world_from_camera[:3, :3]
    origin = camera.world_from_camera[:3, 3]
    dirs_w = dirs @ R.T
    denom = dirs_w @ normal
    numer = (plane_point - origin) @ normal
    t = numer / np.where(np.abs(denom) < 1e-9, np.nan, denom)
    # Depth along camera +Z = (origin + t*dir)_cam · z_axis.
    hit = origin + t[..., None] * dirs_w
    cam_from_world = camera.camera_from_world()
    homo = np.concatenate([hit, np.ones((*hit.shape[:-1], 1))], axis=-1)
    cam = np.einsum("ij,...j->...i", cam_from_world, homo)
    depth = cam[..., 2]
    depth = np.where(np.isfinite(t) & (t > 0) & (depth > 0), depth, 0.0)
    return depth.astype(np.float64)
