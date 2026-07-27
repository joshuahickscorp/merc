"""Coordinate-frame conversion for the three V2 declared frames.

This is the single sanctioned place for BLENDER_WORLD / GLTF_WORLD /
OPENCV_CAMERA conversion. Round-trips are exact to floating-point
orthogonality (asserted at 1e-9 in tests).
"""

from __future__ import annotations

from typing import Any

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    GLTF_WORLD,
    OPENCV_CAMERA,
    CoordinateFrame,
    Handedness,
)

_FRAME_REGISTRY: dict[str, CoordinateFrame] = {
    BLENDER_WORLD.name: BLENDER_WORLD,
    GLTF_WORLD.name: GLTF_WORLD,
    OPENCV_CAMERA.name: OPENCV_CAMERA,
    "blender": BLENDER_WORLD,
    "gltf": GLTF_WORLD,
    "opencv": OPENCV_CAMERA,
    "opencv-camera": OPENCV_CAMERA,
    "blender-world": BLENDER_WORLD,
    "gltf-world": GLTF_WORLD,
}

# Canonical 3x3 maps. Columns are source basis axes expressed in the target frame.
# Derived from the physical axis contracts in authority.py:
#   Blender world: right=+X, up=+Z, forward=-Y
#   glTF world:    right=+X, up=+Y, forward=-Z
#   OpenCV camera: right=+X, up=-Y, forward=+Z
#
# Blender (x,y,z) → glTF (x, z, -y)
# Blender (x,y,z) → OpenCV (x, -z, y)   [world-axis remap matching up/forward]
# glTF (x,y,z)    → OpenCV (x, -y, -z)  [Y-up look-Z → Y-down look+Z]
_CANONICAL: dict[tuple[str, str], np.ndarray] = {}


def _register(src: str, dst: str, matrix: list[list[float]]) -> None:
    arr = np.asarray(matrix, dtype=np.float64)
    _CANONICAL[(src, dst)] = arr
    _CANONICAL[(dst, src)] = arr.T  # orthonormal → inverse is transpose


_register(
    BLENDER_WORLD.name,
    GLTF_WORLD.name,
    [
        [1.0, 0.0, 0.0],
        [0.0, 0.0, 1.0],
        [0.0, -1.0, 0.0],
    ],
)
_register(
    BLENDER_WORLD.name,
    OPENCV_CAMERA.name,
    [
        [1.0, 0.0, 0.0],
        [0.0, 0.0, -1.0],
        [0.0, 1.0, 0.0],
    ],
)
_register(
    GLTF_WORLD.name,
    OPENCV_CAMERA.name,
    [
        [1.0, 0.0, 0.0],
        [0.0, -1.0, 0.0],
        [0.0, 0.0, -1.0],
    ],
)


def _axis_vector(axis: str) -> np.ndarray:
    sign = 1.0 if axis[0] == "+" else -1.0
    index = "XYZ".index(axis[1])
    vector = np.zeros(3, dtype=np.float64)
    vector[index] = sign
    return vector


def frame_basis(frame: CoordinateFrame) -> np.ndarray:
    """Return a 3x3 matrix whose columns are (right, up, forward) in frame coords.

    For the three named frames this matches the hard-coded conversion tables.
    For custom frames, right is chosen so that det([R,U,F]) = +1 for right-handed
    frames when possible.
    """
    if frame.name == BLENDER_WORLD.name:
        return np.column_stack(
            [
                np.array([1.0, 0.0, 0.0]),
                np.array([0.0, 0.0, 1.0]),
                np.array([0.0, -1.0, 0.0]),
            ]
        )
    if frame.name == GLTF_WORLD.name:
        return np.column_stack(
            [
                np.array([1.0, 0.0, 0.0]),
                np.array([0.0, 1.0, 0.0]),
                np.array([0.0, 0.0, -1.0]),
            ]
        )
    if frame.name == OPENCV_CAMERA.name:
        return np.column_stack(
            [
                np.array([1.0, 0.0, 0.0]),
                np.array([0.0, -1.0, 0.0]),
                np.array([0.0, 0.0, 1.0]),
            ]
        )
    up = _axis_vector(frame.up_axis)
    forward = _axis_vector(frame.forward_axis)
    if frame.handedness is Handedness.RIGHT:
        right = np.cross(up, forward)
        # If that yields the wrong orientation for inverted-up frames, flip.
        if float(np.linalg.norm(right)) < 1e-12:
            raise ValidationError(f"degenerate frame basis for {frame.name}")
        # Prefer det([R U F]) = +1.
        basis = np.column_stack([right, up, forward])
        if np.linalg.det(basis) < 0:
            right = -right
            basis = np.column_stack([right, up, forward])
        right = right / float(np.linalg.norm(right))
        return np.column_stack([right, up, forward])
    right = np.cross(forward, up)
    right = right / float(np.linalg.norm(right))
    return np.column_stack([right, up, forward])


def _resolve_frame(frame: CoordinateFrame | str) -> CoordinateFrame:
    if isinstance(frame, CoordinateFrame):
        if frame.name in _FRAME_REGISTRY:
            registered = _FRAME_REGISTRY[frame.name]
            if (
                frame.up_axis == registered.up_axis
                and frame.forward_axis == registered.forward_axis
                and frame.handedness == registered.handedness
            ):
                return registered
            return frame
        return frame
    key = frame.strip().lower()
    if key not in _FRAME_REGISTRY:
        raise ValidationError(
            f"unknown coordinate frame {frame!r}; "
            f"known: {sorted({f.name for f in (BLENDER_WORLD, GLTF_WORLD, OPENCV_CAMERA)})}"
        )
    return _FRAME_REGISTRY[key]


def transform_matrix(
    source: CoordinateFrame | str,
    target: CoordinateFrame | str,
) -> np.ndarray:
    """3x3 matrix mapping points from `source` coordinates into `target` coordinates.

    p_target = M @ p_source. Pure rotation/reflection — no translation.
    """
    src = _resolve_frame(source)
    dst = _resolve_frame(target)
    if src.name == dst.name and src.compatible_with(dst):
        return np.eye(3, dtype=np.float64)
    key = (src.name, dst.name)
    if key in _CANONICAL:
        return _CANONICAL[key].copy()
    # Custom frames: physical components via basis.
    basis_src = frame_basis(src)
    basis_dst = frame_basis(dst)
    return basis_dst @ basis_src.T


def convert_points(
    points: np.ndarray,
    source: CoordinateFrame | str,
    target: CoordinateFrame | str,
) -> np.ndarray:
    """Convert an (N, 3) or (3,) point array between frames."""
    array = np.asarray(points, dtype=np.float64)
    matrix = transform_matrix(source, target)
    if array.ndim == 1:
        if array.shape[0] != 3:
            raise ValidationError("point must have shape (3,)")
        return matrix @ array
    if array.ndim != 2 or array.shape[1] != 3:
        raise ValidationError("points must have shape (N, 3)")
    return array @ matrix.T


def convert_transform(
    transform: np.ndarray,
    source: CoordinateFrame | str,
    target: CoordinateFrame | str,
) -> np.ndarray:
    """Convert a 3x3 rotation or 4x4 rigid transform between frames.

    For a rigid body transform T that maps points in `source` coordinates,
    returns T' such that the same physical motion is expressed in `target`.
    """
    array = np.asarray(transform, dtype=np.float64)
    m = transform_matrix(source, target)
    m_inv = transform_matrix(target, source)
    if array.shape == (3, 3):
        return m @ array @ m_inv
    if array.shape == (4, 4):
        result = np.eye(4, dtype=np.float64)
        result[:3, :3] = m @ array[:3, :3] @ m_inv
        result[:3, 3] = m @ array[:3, 3]
        return result
    raise ValidationError("transform must be 3x3 or 4x4")


def known_frames() -> dict[str, CoordinateFrame]:
    return {
        BLENDER_WORLD.name: BLENDER_WORLD,
        GLTF_WORLD.name: GLTF_WORLD,
        OPENCV_CAMERA.name: OPENCV_CAMERA,
    }


def frame_to_dict(frame: CoordinateFrame) -> dict[str, Any]:
    return frame.to_dict()
