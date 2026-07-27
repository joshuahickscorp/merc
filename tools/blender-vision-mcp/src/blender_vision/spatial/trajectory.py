"""Ordered camera trajectories with validation and frame conversion."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.core.errors import BlenderVisionError, ValidationError
from blender_vision.spatial.frames import convert_transform
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    OPENCV_CAMERA,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    derive,
)
from blender_vision.v2.records import Lineage, ObservationBundle


class TrajectoryError(BlenderVisionError):
    """Trajectory failed structural validation."""


@dataclass(slots=True)
class CameraPose:
    """One timestamped camera pose.

    `world_from_camera` is a 4x4 matrix mapping camera-local points into the
    trajectory's world frame. Rotation must be right-handed orthonormal.
    """

    timestamp: float
    world_from_camera: np.ndarray
    intrinsics: dict[str, float] = field(default_factory=dict)
    label: str = ""

    def __post_init__(self) -> None:
        self.world_from_camera = np.asarray(self.world_from_camera, dtype=np.float64)
        if self.world_from_camera.shape != (4, 4):
            raise ValidationError("world_from_camera must be 4x4")
        self.timestamp = float(self.timestamp)

    @property
    def position(self) -> np.ndarray:
        return self.world_from_camera[:3, 3].copy()

    @property
    def rotation(self) -> np.ndarray:
        return self.world_from_camera[:3, :3].copy()

    def to_dict(self) -> dict[str, Any]:
        return {
            "timestamp": self.timestamp,
            "world_from_camera": self.world_from_camera.tolist(),
            "intrinsics": dict(self.intrinsics),
            "label": self.label,
        }


@dataclass(slots=True)
class CameraTrajectory:
    """Ordered poses with shared frame and authority."""

    poses: list[CameraPose]
    frame: CoordinateFrame = field(default_factory=lambda: BLENDER_WORLD)
    authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED
    notes: list[str] = field(default_factory=list)

    def __post_init__(self) -> None:
        if not self.poses:
            raise TrajectoryError("trajectory requires at least one pose")

    def __len__(self) -> int:
        return len(self.poses)

    def validate(self, *, position_eps: float = 1e-9) -> None:
        """Reject non-orthonormal rotations, non-monotonic time, duplicate poses."""
        previous_t: float | None = None
        previous_matrix: np.ndarray | None = None
        for index, pose in enumerate(self.poses):
            _assert_orthonormal(pose.rotation, index=index)
            det = float(np.linalg.det(pose.rotation))
            if det < 0:
                raise TrajectoryError(
                    f"pose {index}: rotation is left-handed (det={det:.6f}); "
                    "trajectories must be right-handed"
                )
            if abs(pose.world_from_camera[3, 3] - 1.0) > 1e-6:
                raise TrajectoryError(f"pose {index}: homogeneous scale must be 1")
            if any(abs(pose.world_from_camera[3, col]) > 1e-6 for col in range(3)):
                raise TrajectoryError(f"pose {index}: bottom row must be [0,0,0,1]")
            if previous_t is not None and pose.timestamp <= previous_t:
                raise TrajectoryError(
                    f"pose {index}: timestamps must be strictly monotonic "
                    f"({previous_t} -> {pose.timestamp})"
                )
            if previous_matrix is not None and np.allclose(
                pose.world_from_camera, previous_matrix, atol=position_eps
            ):
                raise TrajectoryError(f"pose {index}: duplicate of pose {index - 1}")
            previous_t = pose.timestamp
            previous_matrix = pose.world_from_camera

    def arc_length(self) -> float:
        total = 0.0
        for index in range(1, len(self.poses)):
            delta = self.poses[index].position - self.poses[index - 1].position
            total += float(np.linalg.norm(delta))
        return total

    def relative_pose(self, i: int, j: int) -> np.ndarray:
        """Return 4x4 transform taking camera-i coords into camera-j coords."""
        wi = self.poses[i].world_from_camera
        wj = self.poses[j].world_from_camera
        return np.linalg.inv(wj) @ wi

    def resample(self, n: int) -> CameraTrajectory:
        """Uniform arc-length resampling of positions; slerp-free linear rot lerp.

        Rotations are re-orthonormalised via SVD after linear interpolation of
        matrix entries — adequate for short paths; not a replacement for
        quaternion SLERP on long cinematic paths.
        """
        if n < 2:
            raise ValidationError("resample requires n >= 2")
        if len(self.poses) == 1:
            return CameraTrajectory(
                poses=[
                    CameraPose(
                        timestamp=self.poses[0].timestamp,
                        world_from_camera=self.poses[0].world_from_camera.copy(),
                        intrinsics=dict(self.poses[0].intrinsics),
                        label=self.poses[0].label,
                    )
                    for _ in range(n)
                ],
                frame=self.frame,
                authority=self.authority,
                notes=[*self.notes, f"resampled to {n} (degenerate single pose)"],
            )
        self.validate()
        positions = np.array([pose.position for pose in self.poses], dtype=np.float64)
        rotations = np.array([pose.rotation for pose in self.poses], dtype=np.float64)
        timestamps = np.array([pose.timestamp for pose in self.poses], dtype=np.float64)
        seg_lengths = np.linalg.norm(np.diff(positions, axis=0), axis=1)
        cumulative = np.concatenate([[0.0], np.cumsum(seg_lengths)])
        total = float(cumulative[-1])
        if total < 1e-12:
            # Stationary camera: repeat first pose with interpolated times.
            t0, t1 = timestamps[0], timestamps[-1]
            times = np.linspace(t0, t1 if t1 > t0 else t0 + 1.0, n)
            poses = [
                CameraPose(
                    timestamp=float(times[k]),
                    world_from_camera=self.poses[0].world_from_camera.copy(),
                    intrinsics=dict(self.poses[0].intrinsics),
                )
                for k in range(n)
            ]
            return CameraTrajectory(
                poses=poses,
                frame=self.frame,
                authority=self.authority,
                notes=[*self.notes, f"resampled to {n} (zero arc length)"],
            )
        targets = np.linspace(0.0, total, n)
        new_poses: list[CameraPose] = []
        for s in targets:
            index = int(np.searchsorted(cumulative, s, side="right") - 1)
            index = max(0, min(index, len(self.poses) - 2))
            span = cumulative[index + 1] - cumulative[index]
            alpha = 0.0 if span < 1e-12 else (s - cumulative[index]) / span
            position = (1 - alpha) * positions[index] + alpha * positions[index + 1]
            rotation = (1 - alpha) * rotations[index] + alpha * rotations[index + 1]
            u, _, vt = np.linalg.svd(rotation)
            rotation = u @ vt
            if np.linalg.det(rotation) < 0:
                u[:, -1] *= -1
                rotation = u @ vt
            timestamp = (1 - alpha) * timestamps[index] + alpha * timestamps[index + 1]
            matrix = np.eye(4, dtype=np.float64)
            matrix[:3, :3] = rotation
            matrix[:3, 3] = position
            # Carry the nearer pose's intrinsics.
            donor = self.poses[index] if alpha < 0.5 else self.poses[index + 1]
            new_poses.append(
                CameraPose(
                    timestamp=float(timestamp),
                    world_from_camera=matrix,
                    intrinsics=dict(donor.intrinsics),
                )
            )
        return CameraTrajectory(
            poses=new_poses,
            frame=self.frame,
            authority=derive([self.authority], proposed=AuthorityClass.SENSOR_DERIVED),
            notes=[*self.notes, f"resampled to {n}"],
        )

    def to_blender(self) -> CameraTrajectory:
        """Convert world_from_camera matrices into BLENDER_WORLD if needed."""
        if self.frame.compatible_with(BLENDER_WORLD) and self.frame.name == BLENDER_WORLD.name:
            return self
        return self._convert_world_frame(BLENDER_WORLD)

    def from_blender(self, target: CoordinateFrame) -> CameraTrajectory:
        """Convert a BLENDER_WORLD trajectory into `target`."""
        if not self.frame.compatible_with(BLENDER_WORLD):
            raise TrajectoryError("from_blender requires a Blender-world source trajectory")
        return self._convert_world_frame(target)

    def _convert_world_frame(self, target: CoordinateFrame) -> CameraTrajectory:
        new_poses: list[CameraPose] = []
        for pose in self.poses:
            matrix = convert_transform(pose.world_from_camera, self.frame, target)
            new_poses.append(
                CameraPose(
                    timestamp=pose.timestamp,
                    world_from_camera=matrix,
                    intrinsics=dict(pose.intrinsics),
                    label=pose.label,
                )
            )
        return CameraTrajectory(
            poses=new_poses,
            frame=target,
            authority=self.authority,
            notes=[*self.notes, f"frame {self.frame.name} -> {target.name}"],
        )

    def camera_centers(self) -> np.ndarray:
        return np.array([pose.position for pose in self.poses], dtype=np.float64)

    def looking_directions(self) -> np.ndarray:
        """Unit vectors of camera look direction in the world frame.

        Blender cameras look along local -Z; OpenCV cameras look along +Z.
        """
        directions = []
        # Blender / glTF world trajectories: camera local -Z is look.
        # OpenCV camera-local trajectories look along +Z.
        opencv_local = (
            self.frame.name == OPENCV_CAMERA.name
            or self.frame.origin_semantics == "camera-centre"
        )
        local_look = (
            np.array([0.0, 0.0, 1.0]) if opencv_local else np.array([0.0, 0.0, -1.0])
        )
        for pose in self.poses:
            direction = pose.rotation @ local_look
            norm = float(np.linalg.norm(direction))
            directions.append(direction / max(norm, 1e-12))
        return np.array(directions, dtype=np.float64)

    def seal_observation_bundle(
        self,
        *,
        target_id: str,
        operation: str = "spatial.trajectory.ingest",
    ) -> ObservationBundle:
        self.validate()
        lineage = Lineage(
            operation=operation,
            inputs=[],
            input_authorities=[],
            parameters={
                "pose_count": len(self),
                "arc_length_m": self.arc_length(),
                "time_span_s": self.poses[-1].timestamp - self.poses[0].timestamp,
                "claimed_authority": self.authority.value,
            },
            environment={"frame": self.frame.to_dict()},
            limitations=list(self.notes),
        )
        return ObservationBundle(
            id=f"traj-{uuid.uuid4().hex[:12]}",
            target_id=target_id,
            authority=self.authority,
            lineage=lineage,
            uncertainty=Uncertainty(
                kind="trajectory",
                units=Units.METRE,
                basis="pose sigma not estimated",
                samples=len(self),
            ),
            sensors=[{"type": "camera-trajectory", "frame": self.frame.name}],
            modalities=["camera"],
            coverage={
                "pose_count": len(self),
                "arc_length_m": self.arc_length(),
            },
        ).seal()


def look_at_matrix(
    eye: np.ndarray,
    target: np.ndarray,
    up: np.ndarray | None = None,
) -> np.ndarray:
    """Blender-style camera matrix: local -Z looks at target, local +Y toward up."""
    eye = np.asarray(eye, dtype=np.float64)
    target = np.asarray(target, dtype=np.float64)
    up_vec = np.asarray(up if up is not None else (0.0, 0.0, 1.0), dtype=np.float64)
    forward = target - eye
    forward = forward / max(float(np.linalg.norm(forward)), 1e-12)
    # Camera looks along -Z, so camera -Z = forward ⇒ camera Z = -forward.
    z_axis = -forward
    x_axis = np.cross(up_vec, z_axis)
    x_norm = float(np.linalg.norm(x_axis))
    if x_norm < 1e-12:
        # up parallel to look; pick an alternate up.
        up_vec = np.array([0.0, 1.0, 0.0])
        x_axis = np.cross(up_vec, z_axis)
        x_norm = float(np.linalg.norm(x_axis))
    x_axis = x_axis / x_norm
    y_axis = np.cross(z_axis, x_axis)
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 0] = x_axis
    matrix[:3, 1] = y_axis
    matrix[:3, 2] = z_axis
    matrix[:3, 3] = eye
    return matrix


def _assert_orthonormal(rotation: np.ndarray, *, index: int, tol: float = 1e-3) -> None:
    if rotation.shape != (3, 3):
        raise TrajectoryError(f"pose {index}: rotation must be 3x3")
    should_be_i = rotation.T @ rotation
    if not np.allclose(should_be_i, np.eye(3), atol=tol):
        raise TrajectoryError(
            f"pose {index}: rotation is not orthonormal "
            f"(||R^T R - I||_max={np.max(np.abs(should_be_i - np.eye(3))):.6g})"
        )
    for row in range(3):
        if abs(float(np.linalg.norm(rotation[row])) - 1.0) > tol:
            raise TrajectoryError(f"pose {index}: rotation row {row} is not unit length")
