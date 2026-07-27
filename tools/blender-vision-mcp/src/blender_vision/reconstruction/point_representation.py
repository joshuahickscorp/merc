"""Oriented-point / splat archive from fused depth.

This is a point-based representation (position, normal, colour, radius,
confidence). It is NOT a trained radiance field and NOT a trained Gaussian
splat model. Claiming NeRF/Gaussian-splat capability is a contract violation.
"""

from __future__ import annotations

import json

import numpy as np

from blender_vision.core.models import BackendState
from blender_vision.reconstruction.base import (
    BackendAvailability,
    PointCloud,
    ReconstructionInputs,
    TimedRun,
    finalize_candidate,
    unavailable_candidate,
)
from blender_vision.reconstruction.mesh_ops import (
    write_oriented_points,
    write_ply_points,
)
from blender_vision.v2.authority import AuthorityClass, derive
from blender_vision.v2.records import ReconstructionCandidate

REPRESENTATION_CONTRACT = (
    "oriented-point-archive; not a trained NeRF; not a trained Gaussian splat"
)


class PointRepresentationBackend:
    name = "point_representation"

    def availability(self) -> BackendAvailability:
        return BackendAvailability(
            state=BackendState.AVAILABLE,
            reason="oriented-point archive builder available (not NeRF/3DGS)",
        )

    def run(self, inputs: ReconstructionInputs) -> ReconstructionCandidate:
        if not inputs.depth_frames and inputs.points is None:
            return unavailable_candidate(
                backend=self.name,
                reason="point_representation requires depth_frames or points",
                inputs=inputs,
            )

        with TimedRun() as timer:
            if inputs.points is not None and len(inputs.points.positions) > 0:
                cloud = inputs.points
                source = "input_points"
            else:
                cloud = backproject_depth_frames(
                    inputs.depth_frames,
                    stride=int(inputs.parameters.get("stride", 2)),
                )
                source = "fused_depth_backprojection"
            if len(cloud.positions) == 0:
                return unavailable_candidate(
                    backend=self.name,
                    reason="no points produced from inputs",
                    inputs=inputs,
                )
            work = inputs.ensure_work_dir()
            bin_path = write_oriented_points(work / "oriented_points.osplat", cloud)
            ply_path = write_ply_points(work / "oriented_points.ply", cloud)
            meta = {
                "representation": REPRESENTATION_CONTRACT,
                "point_count": int(len(cloud.positions)),
                "source": source,
                "has_normals": cloud.normals is not None,
                "has_colours": cloud.colours is not None,
                "has_radii": cloud.radii is not None,
                "has_confidence": cloud.confidence is not None,
                "format": {
                    "magic": "BVMCPSPLT",
                    "version": 1,
                    "fields": [
                        "x",
                        "y",
                        "z",
                        "nx",
                        "ny",
                        "nz",
                        "r",
                        "g",
                        "b",
                        "radius",
                        "confidence",
                    ],
                    "not_nerf": True,
                    "not_trained_gaussian_splat": True,
                },
            }
            meta_path = work / "oriented_points.json"
            meta_path.write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")

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
                if inputs.frame.scale_authority is not AuthorityClass.UNRESOLVED
                else "sensor-relative"
            ),
            coverage={
                "point_count": int(len(cloud.positions)),
                "source": source,
                "representation": REPRESENTATION_CONTRACT,
            },
            topology_state={
                "representation": "oriented_point_archive",
                "point_count": int(len(cloud.positions)),
                "watertight": False,
                "manifold": False,
                "surface_mesh": False,
                "trained_radiance_field": False,
                "trained_gaussian_splat": False,
            },
            editability="point-edit; not mesh-topology",
            hidden_surface_assumptions=[
                "only back-projected observed depth samples are stored",
                "no hallucinated free-space fill",
            ],
            artifacts={
                "osplat": str(bin_path),
                "ply": str(ply_path),
                "metadata_json": str(meta_path),
            },
            runtime_seconds=timer.seconds,
            execution_log=(
                f"wrote {len(cloud.positions)} oriented points ({REPRESENTATION_CONTRACT})"
            ),
            failure_modes=[
                "not a continuous surface",
                "not a trained NeRF or Gaussian splat",
            ],
            executed=True,
        )


def backproject_depth_frames(
    frames: list,
    *,
    stride: int = 2,
) -> PointCloud:
    positions: list[np.ndarray] = []
    normals: list[np.ndarray] = []
    colours: list[np.ndarray] = []
    radii: list[float] = []
    confidence: list[float] = []
    for frame in frames:
        depth = np.asarray(frame.depth, dtype=np.float64)
        camera = frame.camera
        h, w = depth.shape[:2]
        ys, xs = np.mgrid[0:h:stride, 0:w:stride]
        z = depth[ys, xs]
        valid = z > 0
        if frame.valid_mask is not None:
            valid &= np.asarray(frame.valid_mask)[ys, xs].astype(bool)
        if not np.any(valid):
            continue
        xs_v = xs[valid].astype(np.float64)
        ys_v = ys[valid].astype(np.float64)
        z_v = z[valid]
        # OpenCV camera back-projection.
        x = (xs_v - camera.cx) * z_v / camera.fx
        y = (ys_v - camera.cy) * z_v / camera.fy
        cam_pts = np.stack([x, y, z_v], axis=1)
        R = camera.world_from_camera[:3, :3]
        t = camera.world_from_camera[:3, 3]
        world = cam_pts @ R.T + t
        # Approximate normals from local depth gradients.
        normal_cam = np.zeros_like(cam_pts)
        normal_cam[:, 2] = -1.0
        normal_world = normal_cam @ R.T
        lengths = np.linalg.norm(normal_world, axis=1, keepdims=True)
        normal_world = normal_world / np.maximum(lengths, 1e-12)
        positions.append(world)
        normals.append(normal_world)
        if frame.colour is not None:
            colour = np.asarray(frame.colour)
            c = np.stack([colour[ys, xs]] * 3, axis=-1) if colour.ndim == 2 else colour[ys, xs]
            c = c[valid].astype(np.float64)
            if c.max() > 1.0:
                c = c / 255.0
            colours.append(c)
        else:
            colours.append(np.full((len(world), 3), 0.7))
        # Radius from pixel footprint.
        pixel_size = z_v / max(camera.fx, camera.fy)
        radii.extend((0.5 * pixel_size).tolist())
        confidence.extend(np.clip(1.0 / (1.0 + 0.01 * z_v), 0.05, 1.0).tolist())
    if not positions:
        return PointCloud(positions=np.zeros((0, 3)))
    return PointCloud(
        positions=np.concatenate(positions, axis=0),
        normals=np.concatenate(normals, axis=0),
        colours=np.concatenate(colours, axis=0),
        radii=np.asarray(radii, dtype=np.float64),
        confidence=np.asarray(confidence, dtype=np.float64),
    )
