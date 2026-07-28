from __future__ import annotations

import math
from copy import deepcopy
from typing import Any

from blender_vision.core.util import canonical_json

REQUIRED_CAMERA_STATE_FIELDS = {
    "reference_id",
    "model",
    "width",
    "height",
    "intrinsics",
    "world_from_camera",
    "extrinsics",
    "distortion_model",
    "sensor_model",
    "crop",
    "resolution",
    "clipping",
    "coordinate_transform",
    "camera_source_identity",
    "solve_method",
    "approval_state",
    "immutable_sha256",
}


def _finite_number(value: Any, name: str) -> float:
    number = float(value)
    if not math.isfinite(number):
        raise ValueError(f"{name} must be finite")
    return number


def complete_camera_state(
    camera: dict[str, Any],
    *,
    backend: str,
    source: dict[str, Any] | None,
) -> dict[str, Any]:
    """Return one self-contained, hash-bound camera snapshot.

    Approval events intentionally live on the containing solution.  The snapshot hash
    therefore remains immutable when a reviewer approves or rejects that solution.
    """
    value = deepcopy(camera)
    width, height = int(value["width"]), int(value["height"])
    if width <= 0 or height <= 0:
        raise ValueError("camera resolution must be positive")
    intrinsics = {
        key: _finite_number(item, f"intrinsics.{key}") for key, item in value["intrinsics"].items()
    }
    if value["model"] != "ORTHOGRAPHIC" and (
        intrinsics.get("fx", 0.0) <= 0.0 or intrinsics.get("fy", 0.0) <= 0.0
    ):
        raise ValueError("perspective camera state requires positive fx and fy")
    matrix = [
        [_finite_number(item, "world_from_camera") for item in row]
        for row in value["world_from_camera"]
    ]
    if len(matrix) != 4 or any(len(row) != 4 for row in matrix):
        raise ValueError("camera extrinsics require a 4x4 world_from_camera matrix")
    distortion_parameters = {
        key: item for key, item in intrinsics.items() if key.startswith(("k", "p", "distortion_"))
    }
    value.update(
        {
            "intrinsics": intrinsics,
            "extrinsics": {"world_from_camera": matrix},
            "distortion_model": value.get("distortion_model")
            or {
                "type": value["model"],
                "parameters": distortion_parameters,
                "render_policy": (
                    "undistorted_input" if not distortion_parameters else "requires_distortion_pass"
                ),
            },
            "sensor_model": value.get("sensor_model")
            or {
                "type": "virtual_pinhole",
                "sensor_width_mm": 36.0,
                "pixel_aspect_x": 1.0,
                "pixel_aspect_y": 1.0,
            },
            "crop": value.get("crop")
            or {"x": 0, "y": 0, "width": width, "height": height, "source": "full_frame"},
            "resolution": {"width": width, "height": height},
            "clipping": value.get("clipping") or {"near": 0.01, "far": 1_000_000.0},
            "coordinate_transform": value.get("coordinate_transform")
            or {
                "matrix": matrix,
                "matrix_semantics": "world_from_camera",
                "world_handedness": "right",
                "world_up_axis": "Z",
                "camera_forward_axis": "-Z",
                "camera_up_axis": "Y",
            },
            "camera_source_identity": value.get("camera_source_identity")
            or {
                "reference_id": value["reference_id"],
                "artifact_digest": source.get("artifact_digest") if source else None,
                "original_name": source.get("original_name") if source else None,
            },
            "solve_method": value.get("solve_method")
            or {
                "backend": backend,
                "registration_class": value["registration_class"],
            },
            "approval_state": "pending",
        }
    )
    immutable = {key: item for key, item in value.items() if key != "immutable_sha256"}
    value["immutable_sha256"] = __import__("hashlib").sha256(canonical_json(immutable)).hexdigest()
    return value


def validate_complete_camera_state(camera: dict[str, Any]) -> dict[str, Any]:
    """Validate the full immutable camera snapshot and return its verified identity."""
    missing = sorted(REQUIRED_CAMERA_STATE_FIELDS - set(camera))
    if missing:
        raise ValueError(f"camera snapshot is incomplete: {missing}")
    width, height = int(camera["width"]), int(camera["height"])
    if width <= 0 or height <= 0:
        raise ValueError("camera snapshot resolution must be positive")
    resolution = camera["resolution"]
    if resolution != {"width": width, "height": height}:
        raise ValueError("camera snapshot resolution disagrees with width and height")
    intrinsics = camera["intrinsics"]
    if not isinstance(intrinsics, dict) or not intrinsics:
        raise ValueError("camera snapshot intrinsics are missing")
    for name, value in intrinsics.items():
        _finite_number(value, f"intrinsics.{name}")
    if camera["model"] != "ORTHOGRAPHIC" and (
        float(intrinsics.get("fx", 0.0)) <= 0.0 or float(intrinsics.get("fy", 0.0)) <= 0.0
    ):
        raise ValueError("camera snapshot perspective intrinsics require positive fx and fy")
    matrix = camera["world_from_camera"]
    if (
        not isinstance(matrix, list)
        or len(matrix) != 4
        or any(not isinstance(row, list) or len(row) != 4 for row in matrix)
    ):
        raise ValueError("camera snapshot world_from_camera must be a 4x4 matrix")
    for row in matrix:
        for value in row:
            _finite_number(value, "world_from_camera")
    if camera["extrinsics"].get("world_from_camera") != matrix:
        raise ValueError("camera snapshot extrinsics disagree with world_from_camera")
    transform = camera["coordinate_transform"]
    if transform.get("matrix") != matrix or transform.get("matrix_semantics") != (
        "world_from_camera"
    ):
        raise ValueError("camera snapshot coordinate transform disagrees with extrinsics")
    crop = camera["crop"]
    crop_values = {
        name: int(crop.get(name, -1)) for name in ("x", "y", "width", "height")
    }
    if (
        crop_values["x"] < 0
        or crop_values["y"] < 0
        or crop_values["width"] <= 0
        or crop_values["height"] <= 0
    ):
        raise ValueError("camera snapshot crop is invalid")
    clipping = camera["clipping"]
    near = _finite_number(clipping.get("near"), "clipping.near")
    far = _finite_number(clipping.get("far"), "clipping.far")
    if near <= 0.0 or far <= near:
        raise ValueError("camera snapshot clipping range is invalid")
    source = camera["camera_source_identity"]
    if source.get("reference_id") != camera["reference_id"]:
        raise ValueError("camera source identity references a different image")
    if not isinstance(camera["distortion_model"], dict) or not camera["distortion_model"].get(
        "type"
    ):
        raise ValueError("camera snapshot distortion model is invalid")
    if not isinstance(camera["sensor_model"], dict) or not camera["sensor_model"].get("type"):
        raise ValueError("camera snapshot sensor model is invalid")
    if not isinstance(camera["solve_method"], dict) or not camera["solve_method"].get("backend"):
        raise ValueError("camera snapshot solve method is invalid")
    expected = camera["immutable_sha256"]
    immutable = {key: value for key, value in camera.items() if key != "immutable_sha256"}
    actual = __import__("hashlib").sha256(canonical_json(immutable)).hexdigest()
    if expected != actual:
        raise ValueError("camera snapshot immutable SHA-256 does not match its content")
    return {
        "reference_id": camera["reference_id"],
        "immutable_sha256": actual,
        "complete": True,
    }


def scaled_camera_state(camera: dict[str, Any], maximum_dimension: int) -> dict[str, Any]:
    """Scale only raster coordinates; preserve the exact extrinsic matrix."""
    value = deepcopy(camera)
    width, height = int(value["width"]), int(value["height"])
    scale = min(1.0, maximum_dimension / max(width, height))
    scaled_width, scaled_height = max(64, round(width * scale)), max(64, round(height * scale))
    x_scale, y_scale = scaled_width / width, scaled_height / height
    intrinsics = dict(value["intrinsics"])
    for key in ("fx", "cx"):
        if key in intrinsics:
            intrinsics[key] = float(intrinsics[key]) * x_scale
    for key in ("fy", "cy"):
        if key in intrinsics:
            intrinsics[key] = float(intrinsics[key]) * y_scale
    value["intrinsics"] = intrinsics
    value["width"], value["height"] = scaled_width, scaled_height
    value["resolution"] = {"width": scaled_width, "height": scaled_height}
    crop = dict(value.get("crop", {}))
    if crop:
        crop.update(
            {
                "x": round(float(crop.get("x", 0)) * x_scale),
                "y": round(float(crop.get("y", 0)) * y_scale),
                "width": round(float(crop.get("width", width)) * x_scale),
                "height": round(float(crop.get("height", height)) * y_scale),
            }
        )
        value["crop"] = crop
    value["source_camera_sha256"] = camera.get("immutable_sha256")
    value.pop("immutable_sha256", None)
    return value
