from __future__ import annotations

import json
import math
import shutil
import subprocess
import uuid
from pathlib import Path
from typing import Any

from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.state import complete_camera_state, validate_complete_camera_state
from blender_vision.core.errors import BackendUnavailable, BlenderVisionError
from blender_vision.core.models import CameraSolution, EvidenceClass, RegistrationClass
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.evidence.derivations import ReferenceDerivationStore
from blender_vision.evidence.measurements import MeasurementGridStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore

CAMERA_MODELS = {
    "PINHOLE",
    "SIMPLE_PINHOLE",
    "SIMPLE_RADIAL",
    "RADIAL",
    "OPENCV",
    "FISHEYE",
    "ORTHOGRAPHIC",
}
CAMERA_QUALITY_FIELDS = {
    "reprojection_rmse_px",
    "registered_feature_count",
    "view_coverage",
    "baseline_diversity",
    "scale_confidence",
    "principal_point_confidence",
    "distortion_confidence",
}


def _validate_camera_quality(quality: dict[str, Any]) -> None:
    if not CAMERA_QUALITY_FIELDS.issubset(quality):
        raise ValueError("metric camera requires a complete camera quality record")
    count = quality["registered_feature_count"]
    if not isinstance(count, int) or count < 0:
        raise ValueError("registered_feature_count must be a non-negative integer")
    rmse = quality["reprojection_rmse_px"]
    if not isinstance(rmse, (int, float)) or not math.isfinite(float(rmse)) or rmse < 0:
        raise ValueError("reprojection_rmse_px must be finite and non-negative")
    for name in (
        "view_coverage",
        "baseline_diversity",
        "scale_confidence",
        "principal_point_confidence",
        "distortion_confidence",
    ):
        value = quality[name]
        if not isinstance(value, (int, float)) or not 0.0 <= float(value) <= 1.0:
            raise ValueError(f"{name} must be between zero and one")


def _validate_rigid_transform(matrix: list[list[float]]) -> None:
    if any(abs(matrix[3][index]) > 1e-8 for index in range(3)) or abs(matrix[3][3] - 1.0) > 1e-8:
        raise ValueError("world_from_camera must use a homogeneous [0,0,0,1] final row")
    rotation = [row[:3] for row in matrix[:3]]
    for row in rotation:
        if abs(sum(value * value for value in row) - 1.0) > 1e-3:
            raise ValueError("world_from_camera rotation rows must be unit length")
    for left in range(3):
        for right in range(left + 1, 3):
            dot = sum(rotation[left][axis] * rotation[right][axis] for axis in range(3))
            if abs(dot) > 1e-3:
                raise ValueError("world_from_camera rotation rows must be orthogonal")


def _quaternion_matrix(w: float, x: float, y: float, z: float) -> list[list[float]]:
    scale = math.sqrt(w * w + x * x + y * y + z * z)
    w, x, y, z = w / scale, x / scale, y / scale, z / scale
    return [
        [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
        [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
        [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
    ]


def _transpose(matrix: list[list[float]]) -> list[list[float]]:
    return [list(row) for row in zip(*matrix, strict=True)]


def _matrix_vector(matrix: list[list[float]], vector: list[float]) -> list[float]:
    return [sum(row[index] * vector[index] for index in range(3)) for row in matrix]


def _dot(left: list[float], right: list[float]) -> float:
    return sum(left[index] * right[index] for index in range(3))


def _normalized(vector: list[float]) -> list[float]:
    length = math.sqrt(_dot(vector, vector))
    if length <= 1e-12:
        raise ValueError("camera direction is degenerate")
    return [value / length for value in vector]


def _cross(left: list[float], right: list[float]) -> list[float]:
    return [
        left[1] * right[2] - left[2] * right[1],
        left[2] * right[0] - left[0] * right[2],
        left[0] * right[1] - left[1] * right[0],
    ]


def _world_from_colmap(q: list[float], translation: list[float]) -> list[list[float]]:
    world_to_camera = _quaternion_matrix(*q)
    camera_to_world = _transpose(world_to_camera)
    center = [-value for value in _matrix_vector(camera_to_world, translation)]
    # COLMAP uses +X right, +Y down, +Z forward. Blender cameras use +X
    # right, +Y up, -Z forward, so negate the second and third basis columns.
    return [
        [camera_to_world[0][0], -camera_to_world[0][1], -camera_to_world[0][2], center[0]],
        [camera_to_world[1][0], -camera_to_world[1][1], -camera_to_world[1][2], center[1]],
        [camera_to_world[2][0], -camera_to_world[2][1], -camera_to_world[2][2], center[2]],
        [0.0, 0.0, 0.0, 1.0],
    ]


def _blender_world_from_look_at(
    position: list[float], target: list[float] | None = None, roll_degrees: float = 0.0
) -> list[list[float]]:
    target = target or [0.0, 0.0, 0.0]
    back = _normalized([position[index] - target[index] for index in range(3)])
    provisional_up = [0.0, 1.0, 0.0] if abs(back[2]) > 0.98 else [0.0, 0.0, 1.0]
    right = _normalized(_cross(provisional_up, back))
    up = _normalized(_cross(back, right))
    if roll_degrees:
        angle = math.radians(roll_degrees)
        cosine, sine = math.cos(angle), math.sin(angle)
        right, up = (
            [right[index] * cosine + up[index] * sine for index in range(3)],
            [up[index] * cosine - right[index] * sine for index in range(3)],
        )
    return [
        [right[0], up[0], back[0], position[0]],
        [right[1], up[1], back[1], position[1]],
        [right[2], up[2], back[2], position[2]],
        [0.0, 0.0, 0.0, 1.0],
    ]


def _intrinsics(model: str, parameters: list[float]) -> dict[str, float]:
    if model in {"SIMPLE_PINHOLE", "SIMPLE_RADIAL", "RADIAL"}:
        value = {"fx": parameters[0], "fy": parameters[0], "cx": parameters[1], "cy": parameters[2]}
        if len(parameters) > 3:
            value.update(
                {f"k{index + 1}": coefficient for index, coefficient in enumerate(parameters[3:])}
            )
        return value
    if model in {"PINHOLE", "OPENCV", "FULL_OPENCV"}:
        value = {"fx": parameters[0], "fy": parameters[1], "cx": parameters[2], "cy": parameters[3]}
        value.update(
            {f"distortion_{index}": coefficient for index, coefficient in enumerate(parameters[4:])}
        )
        return value
    return {f"parameter_{index}": parameter for index, parameter in enumerate(parameters)}


def _direction_for_label(label: str | None, index: int, count: int) -> tuple[float, float, float]:
    normalized = (label or "").lower()
    mappings = {
        "front": (0.0, -1.0, 0.12),
        "rear": (0.0, 1.0, 0.12),
        "back": (0.0, 1.0, 0.12),
        "left": (-1.0, 0.0, 0.12),
        "right": (1.0, 0.0, 0.12),
        "top": (0.0, -0.15, 1.0),
        "bottom": (0.0, -0.15, -1.0),
    }
    for name, direction in mappings.items():
        if name in normalized:
            return direction
    angle = 2.0 * math.pi * index / max(1, count)
    return (math.sin(angle), -math.cos(angle), 0.18)


class CameraSolver:
    def __init__(self, project: ProjectStore):
        self.project = project

    def _camera_hint(self, reference: dict[str, Any]) -> dict[str, Any] | None:
        metadata = self.project.project().get("metadata", {})
        hints = metadata.get("camera_hints_by_viewpoint", {})
        if not isinstance(hints, dict):
            raise ValueError("project camera hints must be keyed by viewpoint label")
        hint = hints.get(reference.get("viewpoint_label"))
        if hint is None:
            return None
        if not isinstance(hint, dict):
            raise ValueError("camera hint must be an object")
        direction = hint.get("view_direction")
        if (
            not isinstance(direction, list)
            or len(direction) != 3
            or not all(isinstance(value, (int, float)) for value in direction)
        ):
            raise ValueError("camera hint view_direction must contain three numbers")
        normalized_direction = _normalized([float(value) for value in direction])
        roll = float(hint.get("roll_degrees", 0.0))
        if not math.isfinite(roll) or not -360.0 <= roll <= 360.0:
            raise ValueError("camera hint roll_degrees must be finite and within +/-360")
        return {"view_direction": normalized_direction, "roll_degrees": roll}

    def _fallback_pose(
        self,
        reference: dict[str, Any],
        index: int,
        count: int,
    ) -> tuple[list[float], list[float], float, bool]:
        hint = self._camera_hint(reference)
        if hint is not None:
            view_direction = hint["view_direction"]
            direction = [-value for value in view_direction]
            return direction, view_direction, hint["roll_degrees"], True
        raw = _direction_for_label(
            reference.get("viewpoint_label") or reference["original_name"], index, count
        )
        direction = _normalized([float(value) for value in raw])
        return direction, [-value for value in direction], 0.0, False

    def solve(
        self, backend: str = "auto", *, reference_ids: list[str] | None = None
    ) -> dict[str, Any]:
        requested = set(reference_ids or [])
        references = [
            reference
            for reference in ReferenceIngestor(self.project).list()
            if reference["media_type"].startswith("image/")
            and reference["quality"].get("decode_ok")
            and (
                reference["id"] in requested
                if requested
                else reference.get("acceptance_eligible", True)
            )
        ]
        if requested - {reference["id"] for reference in references}:
            raise ValueError("camera solve references unknown, undecodable, or non-image evidence")
        if not references:
            raise BlenderVisionError("no decodable image references are available")
        diagnostics: dict[str, Any] = {}
        if backend == "exif":
            return self._solve_exif(references)
        if backend in {"auto", "colmap"}:
            try:
                return self._solve_colmap(references)
            except Exception as error:
                if backend == "colmap":
                    raise
                diagnostics["colmap_fallback_reason"] = f"{type(error).__name__}: {error}"
        return self._solve_turntable(references, diagnostics)

    def _solve_exif(self, references: list[dict[str, Any]]) -> dict[str, Any]:
        solutions = []
        initialized = 0
        for index, reference in enumerate(references):
            width = int(reference["metadata"].get("width", 1024))
            height = int(reference["metadata"].get("height", 1024))
            lens = reference["metadata"].get("lens", {})
            focal_35_raw = lens.get("FocalLengthIn35mmFilm")
            try:
                focal_35 = float(focal_35_raw) if focal_35_raw is not None else None
            except (TypeError, ValueError):
                focal_35 = None
            if focal_35 is not None and focal_35 > 0:
                fx = width * focal_35 / 36.0
                initialized += 1
                focal_source = "EXIF FocalLengthIn35mmFilm"
            else:
                fx = width * 1.25
                focal_source = "bounded default; EXIF 35mm equivalent unavailable"
            direction, view_direction, roll, hint_applied = self._fallback_pose(
                reference, index, len(references)
            )
            position = [component * 500.0 for component in direction]
            solutions.append(
                CameraSolution(
                    reference_id=reference["id"],
                    model="PINHOLE",
                    width=width,
                    height=height,
                    intrinsics={"fx": fx, "fy": fx, "cx": width / 2, "cy": height / 2},
                    world_from_camera=_blender_world_from_look_at(position, roll_degrees=roll),
                    confidence=0.35 if focal_35 else 0.2,
                    registration_class=RegistrationClass.APPROXIMATE_VISUAL.value,
                    evidence_class=EvidenceClass.INFERRED_LOW_CONFIDENCE,
                    diagnostics={
                        "direction": list(direction),
                        "view_direction": view_direction,
                        "camera_roll_degrees": roll,
                        "manifest_camera_hint_applied": hint_applied,
                        "focal_source": focal_source,
                        "lens_metadata": lens,
                        "rolling_shutter_warning": "unassessed_without_sensor_readout_metadata",
                    },
                )
            )
        diagnostics = {
            "registered_images": len(solutions),
            "exif_focal_initializations": initialized,
            "warning": (
                "EXIF initializes focal length only; poses remain approximate and cannot satisfy "
                "an L3 camera gate without feature/metric recovery and review."
            ),
        }
        return self._store_solution("exif", solutions, diagnostics)

    def solve_calibration_board(
        self,
        *,
        columns: int,
        rows: int,
        square_size_measurement_id: str,
    ) -> dict[str, Any]:
        """Recover metric intrinsics and poses from a reviewed planar chessboard measurement."""
        try:
            import cv2  # type: ignore[import-not-found]
            import numpy as np  # type: ignore[import-not-found]
        except ImportError as error:
            raise BackendUnavailable(
                "calibration-board solve requires the 'vision' extra (OpenCV and NumPy)"
            ) from error
        if columns < 3 or rows < 3:
            raise ValueError("calibration board requires at least 3x3 inner corners")
        with self.project.connection() as connection:
            measurement = connection.execute(
                "SELECT * FROM measurements WHERE id=?", (square_size_measurement_id,)
            ).fetchone()
        if measurement is None:
            raise ValueError("calibration board square-size measurement does not exist")
        measurement_value = json.loads(measurement["value_json"])
        square_mm = measurement_value.get("millimetres")
        if (
            measurement["evidence_class"] not in {"MEASURED", "MANUFACTURER_SPEC"}
            or measurement["type"] not in {"array_pitch", "known_overall_dimension", "line"}
            or not isinstance(square_mm, (int, float))
            or square_mm <= 0
        ):
            raise ValueError("calibration board requires authoritative square size in millimetres")
        references = [
            item
            for item in ReferenceIngestor(self.project).list()
            if item["media_type"].startswith("image/") and item["quality"].get("decode_ok")
        ]
        if not references:
            raise BlenderVisionError("no decodable calibration-board images are available")
        object_template = np.zeros((columns * rows, 3), np.float32)
        object_template[:, :2] = np.mgrid[0:columns, 0:rows].T.reshape(-1, 2)
        object_template *= float(square_mm)
        object_points = []
        image_points = []
        detected_references = []
        image_size: tuple[int, int] | None = None
        detection_diagnostics = []
        for reference in references:
            source = self.project.root / reference["relative_path"]
            image = cv2.imread(str(source), cv2.IMREAD_GRAYSCALE)
            if image is None:
                detection_diagnostics.append(
                    {"reference_id": reference["id"], "detected": False, "reason": "decode_failed"}
                )
                continue
            current_size = (int(image.shape[1]), int(image.shape[0]))
            if image_size is not None and current_size != image_size:
                raise ValueError("calibration-board images must share one image size")
            image_size = current_size
            if hasattr(cv2, "findChessboardCornersSB"):
                found, corners = cv2.findChessboardCornersSB(image, (columns, rows))
            else:
                found, corners = cv2.findChessboardCorners(image, (columns, rows))
            if not found or corners is None:
                detection_diagnostics.append(
                    {"reference_id": reference["id"], "detected": False, "reason": "not_found"}
                )
                continue
            corners = corners.reshape(-1, 1, 2).astype(np.float32)
            object_points.append(object_template.copy())
            image_points.append(corners)
            detected_references.append(reference)
            detection_diagnostics.append(
                {"reference_id": reference["id"], "detected": True, "corner_count": len(corners)}
            )
        if not object_points or image_size is None:
            raise BlenderVisionError("calibration board was not detected in any reference")
        rms, camera_matrix, distortion, rotations, translations = cv2.calibrateCamera(
            object_points, image_points, image_size, None, None
        )
        solutions = []
        camera_positions = []
        for reference, object_vertices, corners, rotation, translation in zip(
            detected_references,
            object_points,
            image_points,
            rotations,
            translations,
            strict=True,
        ):
            world_to_camera, _ = cv2.Rodrigues(rotation)
            camera_to_world = world_to_camera.T
            center = (-camera_to_world @ translation.reshape(3, 1)).reshape(3)
            camera_positions.append(center)
            projected, _ = cv2.projectPoints(
                object_vertices, rotation, translation, camera_matrix, distortion
            )
            pixel_deltas = projected.reshape(-1, 2) - corners.reshape(-1, 2)
            error = float(np.sqrt(np.mean(np.sum(pixel_deltas**2, axis=1))))
            points = corners.reshape(-1, 2)
            coverage = float(
                max(0.0, (points[:, 0].max() - points[:, 0].min()))
                * max(0.0, (points[:, 1].max() - points[:, 1].min()))
                / max(1, image_size[0] * image_size[1])
            )
            # OpenCV shares COLMAP's camera-axis convention; convert its
            # camera-to-world basis to Blender before persisting it.
            matrix = [
                [
                    float(camera_to_world[0, 0]),
                    -float(camera_to_world[0, 1]),
                    -float(camera_to_world[0, 2]),
                    float(center[0]),
                ],
                [
                    float(camera_to_world[1, 0]),
                    -float(camera_to_world[1, 1]),
                    -float(camera_to_world[1, 2]),
                    float(center[1]),
                ],
                [
                    float(camera_to_world[2, 0]),
                    -float(camera_to_world[2, 1]),
                    -float(camera_to_world[2, 2]),
                    float(center[2]),
                ],
                [0.0, 0.0, 0.0, 1.0],
            ]
            coefficients = distortion.reshape(-1).tolist()
            intrinsics = {
                "fx": float(camera_matrix[0, 0]),
                "fy": float(camera_matrix[1, 1]),
                "cx": float(camera_matrix[0, 2]),
                "cy": float(camera_matrix[1, 2]),
                **{f"distortion_{index}": float(value) for index, value in enumerate(coefficients)},
            }
            solutions.append(
                CameraSolution(
                    reference_id=reference["id"],
                    model="OPENCV",
                    width=image_size[0],
                    height=image_size[1],
                    intrinsics=intrinsics,
                    world_from_camera=matrix,
                    confidence=max(0.0, min(1.0, 1.0 - error / 5.0)),
                    registration_class=RegistrationClass.METRIC.value,
                    evidence_class=EvidenceClass.MEASURED,
                    diagnostics={
                        "quality": {
                            "reprojection_rmse_px": error,
                            "registered_feature_count": columns * rows,
                            "view_coverage": min(1.0, coverage),
                            "baseline_diversity": 0.0,
                            "scale_confidence": 0.95,
                            "principal_point_confidence": 0.85,
                            "distortion_confidence": 0.8,
                        },
                        "evidence_binding_ids": [square_size_measurement_id],
                        "rolling_shutter_warning": "unassessed_without_sensor_readout_metadata",
                        "view_direction": self._view_direction(matrix),
                    },
                )
            )
        if len(camera_positions) > 1:
            spread = float(np.linalg.norm(np.ptp(np.asarray(camera_positions), axis=0)))
            diversity = max(0.0, min(1.0, spread / (float(square_mm) * max(columns, rows) * 2.0)))
            for solution in solutions:
                solution.diagnostics["quality"]["baseline_diversity"] = diversity
        diagnostics = {
            "registered_images": len(solutions),
            "input_images": len(references),
            "board": {"columns": columns, "rows": rows, "square_size_mm": float(square_mm)},
            "global_reprojection_rms_px": float(rms),
            "evidence_binding_ids": [square_size_measurement_id],
            "detections": detection_diagnostics,
            "warning": (
                "Only detected board views are represented; approval still requires exact project "
                "reference coverage and named review."
            ),
        }
        return self._store_solution("opencv_calibration_board", solutions, diagnostics)

    def solve_pnp_landmarks(
        self,
        *,
        landmark_proposal_id: str,
        max_reprojection_rmse_px: float = 4.0,
    ) -> dict[str, Any]:
        """Recover pending metric poses from an immutable named landmark review."""
        from blender_vision.cameras.landmarks import CameraLandmarkStore

        reviewed = CameraLandmarkStore(self.project).reviewed_pnp_input(
            landmark_proposal_id
        )
        return self._solve_pnp_landmarks(
            intrinsics_solution_id=reviewed["intrinsics_solution_id"],
            views=reviewed["views"],
            evidence_binding_ids=reviewed["evidence_binding_ids"],
            reviewed_by=reviewed["reviewed_by"],
            landmark_review_id=reviewed["review_id"],
            landmark_review_digest=reviewed["review_digest"],
            max_reprojection_rmse_px=max_reprojection_rmse_px,
        )

    def _solve_pnp_landmarks(
        self,
        *,
        intrinsics_solution_id: str,
        views: list[dict[str, Any]],
        evidence_binding_ids: list[str],
        reviewed_by: str,
        landmark_review_id: str,
        landmark_review_digest: str,
        max_reprojection_rmse_px: float = 4.0,
    ) -> dict[str, Any]:
        """Recover metric poses from reviewed image-to-model landmark correspondences.

        The referenced camera solution supplies immutable pinhole intrinsics only. World
        points are interpreted in project millimetres and therefore require authoritative
        x/y/z dimension bindings. The resulting solution remains pending until a separate
        named camera review approves it.
        """
        try:
            import cv2  # type: ignore[import-not-found]
            import numpy as np  # type: ignore[import-not-found]
        except ImportError as error:
            raise BackendUnavailable(
                "PnP landmark solve requires the 'vision' extra (OpenCV and NumPy)"
            ) from error
        if not reviewed_by.strip():
            raise ValueError("PnP landmark correspondences require a named reviewer")
        if not views:
            raise ValueError("PnP landmark solve requires at least one view")
        if (
            not isinstance(max_reprojection_rmse_px, (int, float))
            or not math.isfinite(float(max_reprojection_rmse_px))
            or max_reprojection_rmse_px <= 0
        ):
            raise ValueError("maximum reprojection RMSE must be finite and positive")
        with self.project.connection() as connection:
            source_row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?",
                (intrinsics_solution_id,),
            ).fetchone()
            measurement_rows = connection.execute(
                "SELECT id,type,evidence_class,value_json FROM measurements"
            ).fetchall()
            reference_rows = connection.execute(
                "SELECT id,media_type,acceptance_eligible FROM reference_items"
            ).fetchall()
        if source_row is None:
            raise ValueError("PnP intrinsics solution does not exist")
        measurements = {row["id"]: row for row in measurement_rows}
        if not evidence_binding_ids or not set(evidence_binding_ids).issubset(measurements):
            raise ValueError("PnP evidence bindings must reference stored measurements")
        authoritative_axes = {
            json.loads(measurements[item]["value_json"]).get("axis")
            for item in evidence_binding_ids
            if measurements[item]["type"] == "known_overall_dimension"
            and measurements[item]["evidence_class"] in {"MEASURED", "MANUFACTURER_SPEC"}
        }
        if authoritative_axes != {"x", "y", "z"}:
            raise ValueError(
                "metric PnP requires authoritative known-overall-dimension bindings for x/y/z"
            )
        valid_references = {
            row["id"]
            for row in reference_rows
            if row["media_type"].startswith("image/") and row["acceptance_eligible"]
        }
        source_document = json.loads(source_row[0])
        source_cameras = {
            camera["reference_id"]: camera for camera in source_document.get("cameras", [])
        }
        solved: list[CameraSolution] = []
        camera_positions = []
        diagnostics_by_view = []
        seen: set[str] = set()
        for view in views:
            reference_id = str(view.get("reference_id", ""))
            if reference_id in seen:
                raise ValueError(f"PnP landmark views contain duplicate reference: {reference_id}")
            seen.add(reference_id)
            if reference_id not in valid_references:
                raise ValueError(
                    f"PnP view is not an acceptance-eligible image reference: {reference_id}"
                )
            source = source_cameras.get(reference_id)
            if source is None:
                raise ValueError(
                    f"PnP intrinsics solution does not cover reference: {reference_id}"
                )
            if source.get("model") not in {"PINHOLE", "SIMPLE_PINHOLE"}:
                raise ValueError("PnP requires an undistorted pinhole intrinsics solution")
            if source.get("distortion_model", {}).get("render_policy") not in {
                None,
                "undistorted_input",
            }:
                raise ValueError("PnP source camera still requires distortion handling")
            points = view.get("correspondences")
            if not isinstance(points, list) or len(points) < 6:
                raise ValueError("each PnP view requires at least six reviewed correspondences")
            landmark_ids: set[str] = set()
            world_points = []
            image_points = []
            for point in points:
                landmark_id = str(point.get("landmark_id", "")).strip()
                world = point.get("world_mm")
                image = point.get("image_px")
                if not landmark_id or landmark_id in landmark_ids:
                    raise ValueError("PnP landmarks require unique non-empty landmark ids per view")
                landmark_ids.add(landmark_id)
                if (
                    not isinstance(world, list)
                    or len(world) != 3
                    or not all(isinstance(value, (int, float)) for value in world)
                    or not isinstance(image, list)
                    or len(image) != 2
                    or not all(isinstance(value, (int, float)) for value in image)
                ):
                    raise ValueError("PnP correspondence coordinates must be numeric arrays")
                world_points.append([float(value) for value in world])
                image_points.append([float(value) for value in image])
            object_array = np.asarray(world_points, dtype=np.float64)
            pixel_array = np.asarray(image_points, dtype=np.float64)
            if not np.isfinite(object_array).all() or not np.isfinite(pixel_array).all():
                raise ValueError("PnP correspondences contain non-finite coordinates")
            if int(np.linalg.matrix_rank(object_array - object_array.mean(axis=0))) < 3:
                raise ValueError("PnP world landmarks must be non-coplanar")
            width, height = int(source["width"]), int(source["height"])
            if (
                (pixel_array[:, 0] < 0).any()
                or (pixel_array[:, 0] >= width).any()
                or (pixel_array[:, 1] < 0).any()
                or (pixel_array[:, 1] >= height).any()
            ):
                raise ValueError("PnP image landmarks must lie inside the source image")
            intrinsics = source["intrinsics"]
            camera_matrix = np.asarray(
                [
                    [float(intrinsics["fx"]), 0.0, float(intrinsics["cx"])],
                    [0.0, float(intrinsics["fy"]), float(intrinsics["cy"])],
                    [0.0, 0.0, 1.0],
                ],
                dtype=np.float64,
            )
            success, rotation, translation, inliers = cv2.solvePnPRansac(
                object_array,
                pixel_array,
                camera_matrix,
                None,
                iterationsCount=200,
                reprojectionError=float(max_reprojection_rmse_px) * 1.5,
                confidence=0.999,
                flags=cv2.SOLVEPNP_EPNP,
            )
            inlier_count = 0 if inliers is None else int(len(inliers))
            if not success or rotation is None or translation is None or inlier_count < 6:
                raise BlenderVisionError(
                    f"PnP failed to recover six inliers for reference {reference_id}"
                )
            if hasattr(cv2, "solvePnPRefineLM"):
                indices = inliers.reshape(-1)
                rotation, translation = cv2.solvePnPRefineLM(
                    object_array[indices],
                    pixel_array[indices],
                    camera_matrix,
                    None,
                    rotation,
                    translation,
                )
            projected, _ = cv2.projectPoints(
                object_array, rotation, translation, camera_matrix, None
            )
            deltas = projected.reshape(-1, 2) - pixel_array
            rmse = float(np.sqrt(np.mean(np.sum(deltas**2, axis=1))))
            if rmse > float(max_reprojection_rmse_px):
                raise BlenderVisionError(
                    f"PnP reprojection RMSE {rmse:.3f}px exceeds "
                    f"{float(max_reprojection_rmse_px):.3f}px for {reference_id}"
                )
            world_to_camera, _ = cv2.Rodrigues(rotation)
            camera_to_world = world_to_camera.T
            center = (-camera_to_world @ translation.reshape(3, 1)).reshape(3)
            camera_positions.append(center)
            matrix = [
                [
                    float(camera_to_world[0, 0]),
                    -float(camera_to_world[0, 1]),
                    -float(camera_to_world[0, 2]),
                    float(center[0]),
                ],
                [
                    float(camera_to_world[1, 0]),
                    -float(camera_to_world[1, 1]),
                    -float(camera_to_world[1, 2]),
                    float(center[1]),
                ],
                [
                    float(camera_to_world[2, 0]),
                    -float(camera_to_world[2, 1]),
                    -float(camera_to_world[2, 2]),
                    float(center[2]),
                ],
                [0.0, 0.0, 0.0, 1.0],
            ]
            coverage = float(
                max(0.0, pixel_array[:, 0].max() - pixel_array[:, 0].min())
                * max(0.0, pixel_array[:, 1].max() - pixel_array[:, 1].min())
                / max(1, width * height)
            )
            quality = {
                "reprojection_rmse_px": rmse,
                "registered_feature_count": inlier_count,
                "view_coverage": min(1.0, coverage),
                "baseline_diversity": 0.0,
                "scale_confidence": 0.95,
                "principal_point_confidence": float(
                    source.get("diagnostics", {})
                    .get("quality", {})
                    .get("principal_point_confidence", 0.8)
                ),
                "distortion_confidence": 1.0,
            }
            diagnostics = {
                "quality": quality,
                "evidence_binding_ids": list(evidence_binding_ids),
                "intrinsics_solution_id": intrinsics_solution_id,
                "landmark_reviewer": reviewed_by.strip(),
                "landmark_review_id": landmark_review_id,
                "landmark_review_digest": landmark_review_digest,
                "landmark_ids": sorted(landmark_ids),
                "correspondence_count": len(points),
                "inlier_count": inlier_count,
                "world_units": "millimetres",
                "rolling_shutter_warning": "unassessed_without_sensor_readout_metadata",
                "view_direction": self._view_direction(matrix),
            }
            diagnostics_by_view.append({"reference_id": reference_id, **diagnostics})
            solved.append(
                CameraSolution(
                    reference_id=reference_id,
                    model="PINHOLE",
                    width=width,
                    height=height,
                    intrinsics={
                        name: float(intrinsics[name])
                        for name in ("fx", "fy", "cx", "cy")
                    },
                    world_from_camera=matrix,
                    confidence=max(0.0, min(0.95, 1.0 - rmse / 10.0)),
                    registration_class=RegistrationClass.METRIC.value,
                    evidence_class=EvidenceClass.MEASURED,
                    diagnostics=diagnostics,
                    distortion_model={
                        "type": "PINHOLE",
                        "parameters": {},
                        "render_policy": "undistorted_input",
                    },
                    sensor_model=source.get("sensor_model"),
                    crop=source.get("crop"),
                    clipping=source.get("clipping"),
                )
            )
        if len(camera_positions) > 1:
            positions = np.asarray(camera_positions, dtype=np.float64)
            spread = float(np.linalg.norm(np.ptp(positions, axis=0)))
            world_extent = max(
                float(
                    json.loads(measurements[item]["value_json"]).get("millimetres", 0.0)
                )
                for item in evidence_binding_ids
            )
            diversity = max(0.0, min(1.0, spread / max(world_extent * 2.0, 1.0)))
            for camera in solved:
                camera.diagnostics["quality"]["baseline_diversity"] = diversity
            for item in diagnostics_by_view:
                item["quality"]["baseline_diversity"] = diversity
        return self._store_solution(
            "opencv_pnp_landmarks",
            solved,
            {
                "registered_images": len(solved),
                "intrinsics_solution_id": intrinsics_solution_id,
                "evidence_binding_ids": list(evidence_binding_ids),
                "landmark_reviewer": reviewed_by.strip(),
                "landmark_review_id": landmark_review_id,
                "landmark_review_digest": landmark_review_digest,
                "world_units": "millimetres",
                "views": diagnostics_by_view,
                "warning": (
                    "Metric classification is evidence-derived; the solution remains unapproved "
                    "until a separate named camera review covers the complete eligible set."
                ),
            },
        )

    def solve_vanishing_points(self, grid_ids: list[str] | None = None) -> dict[str, Any]:
        """Recover focal length and Manhattan orientation from reviewed perspective grids."""
        grid_store = MeasurementGridStore(self.project)
        grids = grid_store.list() if not grid_ids else [grid_store.get(item) for item in grid_ids]
        if not grids:
            raise BlenderVisionError("vanishing-point solve requires at least one measurement grid")
        solutions = []
        diagnostics_by_grid = []
        for index, grid in enumerate(grids):
            points = grid["definition"].get("vanishing_points", {})
            if set(points) != {"x", "y", "z"}:
                raise ValueError("vanishing-point solve requires x, y, and z vanishing points")
            width = int(grid["image_size"]["width"])
            height = int(grid["image_size"]["height"])
            if width <= 0 or height <= 0:
                raise ValueError("measurement grid is missing image dimensions")
            pixel_points = {
                axis: [float(point[0]) * width, float(point[1]) * height]
                for axis, point in points.items()
            }
            principal = [width / 2.0, height / 2.0]
            focal_squared = []
            for left, right in (("x", "y"), ("x", "z"), ("y", "z")):
                left_point, right_point = pixel_points[left], pixel_points[right]
                value = -(
                    (left_point[0] - principal[0]) * (right_point[0] - principal[0])
                    + (left_point[1] - principal[1]) * (right_point[1] - principal[1])
                )
                if value <= 0 or not math.isfinite(value):
                    raise ValueError("vanishing points do not support orthogonal Manhattan axes")
                focal_squared.append(value)
            focal = math.sqrt(sum(focal_squared) / len(focal_squared))
            directions = {
                axis: _normalized(
                    [
                        (point[0] - principal[0]) / focal,
                        (point[1] - principal[1]) / focal,
                        1.0,
                    ]
                )
                for axis, point in pixel_points.items()
            }
            x_axis = directions["x"]
            y_raw = directions["y"]
            y_axis = _normalized(
                [
                    y_raw[component] - _dot(y_raw, x_axis) * x_axis[component]
                    for component in range(3)
                ]
            )
            z_axis = _normalized(_cross(x_axis, y_axis))
            if _dot(z_axis, directions["z"]) < 0:
                z_axis = [-value for value in z_axis]
            camera_from_world = [
                [x_axis[0], y_axis[0], z_axis[0]],
                [x_axis[1], y_axis[1], z_axis[1]],
                [x_axis[2], y_axis[2], z_axis[2]],
            ]
            camera_to_world = _transpose(camera_from_world)
            direction = _direction_for_label(None, index, len(grids))
            position = [value * 500.0 for value in _normalized(list(direction))]
            matrix = [
                [*camera_to_world[0], position[0]],
                [*camera_to_world[1], position[1]],
                [*camera_to_world[2], position[2]],
                [0.0, 0.0, 0.0, 1.0],
            ]
            mean_f2 = sum(focal_squared) / len(focal_squared)
            consistency = max(
                0.0,
                1.0 - max(abs(value - mean_f2) for value in focal_squared) / max(mean_f2, 1.0),
            )
            diagnostics = {
                "grid_id": grid["id"],
                "vanishing_points_pixels": pixel_points,
                "orthogonality_consistency": consistency,
                "translation_authority": "approximate_only",
                "rolling_shutter_warning": "unassessed_without_sensor_readout_metadata",
                "view_direction": self._view_direction(matrix),
            }
            diagnostics_by_grid.append(diagnostics)
            solutions.append(
                CameraSolution(
                    reference_id=grid["reference_id"],
                    model="PINHOLE",
                    width=width,
                    height=height,
                    intrinsics={"fx": focal, "fy": focal, "cx": principal[0], "cy": principal[1]},
                    world_from_camera=matrix,
                    confidence=min(0.7, 0.35 + consistency * 0.35),
                    registration_class=RegistrationClass.APPROXIMATE_VISUAL.value,
                    evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
                    diagnostics=diagnostics,
                )
            )
        return self._store_solution(
            "vanishing_point",
            solutions,
            {
                "registered_images": len(solutions),
                "grids": diagnostics_by_grid,
                "warning": (
                    "Vanishing points constrain intrinsics and rotation, not metric translation; "
                    "this backend cannot satisfy the L3 metric-camera gate by itself."
                ),
            },
        )

    def import_manual(
        self,
        cameras: list[dict[str, Any]],
        *,
        diagnostics: dict[str, Any] | None = None,
        evidence_binding_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Import data-only manual/calibrated cameras; imported records start unapproved."""
        if not cameras:
            raise ValueError("manual camera import requires at least one camera")
        bindings = evidence_binding_ids or []
        with self.project.connection() as connection:
            valid_references = {
                row[0]
                for row in connection.execute(
                    "SELECT id FROM reference_items WHERE media_type LIKE 'image/%'"
                ).fetchall()
            }
            measurement_rows = connection.execute(
                "SELECT id,type,evidence_class FROM measurements"
            ).fetchall()
        valid_measurements = {row["id"] for row in measurement_rows}
        if not set(bindings).issubset(valid_measurements):
            raise ValueError("camera evidence bindings must reference stored measurements")
        has_authoritative_scale = any(
            row["id"] in bindings
            and row["type"] == "known_overall_dimension"
            and row["evidence_class"] in {"MEASURED", "MANUFACTURER_SPEC"}
            for row in measurement_rows
        )
        imported: list[CameraSolution] = []
        imported_reference_ids: set[str] = set()
        for camera in cameras:
            reference_id = str(camera.get("reference_id", ""))
            if reference_id not in valid_references:
                raise ValueError(f"manual camera references unknown evidence: {reference_id}")
            if reference_id in imported_reference_ids:
                raise ValueError(f"manual camera contains a duplicate reference: {reference_id}")
            imported_reference_ids.add(reference_id)
            model = str(camera.get("model", "")).upper()
            if model not in CAMERA_MODELS:
                raise ValueError(f"unsupported camera model: {model}")
            width, height = int(camera.get("width", 0)), int(camera.get("height", 0))
            intrinsics = camera.get("intrinsics", {})
            if width <= 0 or height <= 0 or not isinstance(intrinsics, dict):
                raise ValueError("manual camera requires positive dimensions and intrinsics")
            if model != "ORTHOGRAPHIC" and (
                float(intrinsics.get("fx", 0)) <= 0 or float(intrinsics.get("fy", 0)) <= 0
            ):
                raise ValueError("perspective camera intrinsics require positive fx and fy")
            matrix = camera.get("world_from_camera")
            if (
                not isinstance(matrix, list)
                or len(matrix) != 4
                or any(not isinstance(row, list) or len(row) != 4 for row in matrix)
            ):
                raise ValueError("world_from_camera must be a 4x4 matrix")
            numeric_matrix = [[float(value) for value in row] for row in matrix]
            if not all(math.isfinite(value) for row in numeric_matrix for value in row):
                raise ValueError("world_from_camera contains a non-finite value")
            _validate_rigid_transform(numeric_matrix)
            registration_class = RegistrationClass(camera["registration_class"])
            confidence = float(camera["confidence"])
            if not math.isfinite(confidence) or not 0.0 <= confidence <= 1.0:
                raise ValueError("camera confidence must be between zero and one")
            camera_diagnostics = dict(camera.get("diagnostics", {}))
            quality = camera_diagnostics.get("quality", {})
            if registration_class == RegistrationClass.METRIC:
                if not has_authoritative_scale:
                    raise ValueError("metric camera import requires authoritative scale evidence")
                _validate_camera_quality(quality)
            camera_diagnostics["evidence_binding_ids"] = bindings
            imported.append(
                CameraSolution(
                    reference_id=reference_id,
                    model=model,
                    width=width,
                    height=height,
                    intrinsics={key: float(value) for key, value in intrinsics.items()},
                    world_from_camera=numeric_matrix,
                    confidence=confidence,
                    registration_class=registration_class.value,
                    evidence_class=EvidenceClass(camera["evidence_class"]),
                    diagnostics=camera_diagnostics,
                    distortion_model=camera.get("distortion_model"),
                    sensor_model=camera.get("sensor_model"),
                    crop=camera.get("crop"),
                    clipping=camera.get("clipping"),
                    coordinate_transform=camera.get("coordinate_transform"),
                    camera_source_identity=camera.get("camera_source_identity"),
                    solve_method=camera.get("solve_method"),
                )
            )
        document_diagnostics = dict(diagnostics or {})
        document_diagnostics["evidence_binding_ids"] = bindings
        document_diagnostics["imported_manually"] = True
        return self._store_solution("manual", imported, document_diagnostics)

    def consolidate_solutions(
        self,
        solution_ids: list[str],
        *,
        require_all_acceptance_references: bool = True,
    ) -> dict[str, Any]:
        """Combine disjoint camera hypotheses without changing matrices or authority."""
        ordered_ids = list(dict.fromkeys(str(item).strip() for item in solution_ids))
        if not ordered_ids or any(not item for item in ordered_ids):
            raise ValueError("camera consolidation requires unique solution ids")
        if len(ordered_ids) != len(solution_ids):
            raise ValueError("camera consolidation contains duplicate solution ids")
        placeholders = ",".join("?" for _ in ordered_ids)
        with self.project.connection() as connection:
            rows = connection.execute(
                f"SELECT id,backend,solution_json,approved FROM camera_solutions "
                f"WHERE id IN ({placeholders})",
                ordered_ids,
            ).fetchall()
            acceptance_reference_ids = {
                row[0]
                for row in connection.execute(
                    "SELECT id FROM reference_items WHERE media_type LIKE 'image/%' "
                    "AND acceptance_eligible=1"
                )
            }
        by_id = {row["id"]: row for row in rows}
        missing = [item for item in ordered_ids if item not in by_id]
        if missing:
            raise KeyError(f"unknown camera solutions: {missing}")
        cameras: list[dict[str, Any]] = []
        reference_ids: set[str] = set()
        sources = []
        for solution_id in ordered_ids:
            row = by_id[solution_id]
            document = json.loads(row["solution_json"])
            source_cameras = document.get("cameras", [])
            if not source_cameras:
                raise ValueError(f"camera solution has no cameras: {solution_id}")
            sources.append(
                {
                    "solution_id": solution_id,
                    "backend": row["backend"],
                    "approved": bool(row["approved"]),
                    "camera_count": len(source_cameras),
                }
            )
            for camera in source_cameras:
                reference_id = camera["reference_id"]
                if reference_id in reference_ids:
                    raise ValueError(
                        f"camera consolidation duplicates reference: {reference_id}"
                    )
                reference_ids.add(reference_id)
                world_from_camera = camera.get("world_from_camera") or camera.get(
                    "extrinsics", {}
                ).get("world_from_camera")
                cameras.append(
                    {
                        "reference_id": reference_id,
                        "model": camera["model"],
                        "width": camera["width"],
                        "height": camera["height"],
                        "intrinsics": camera["intrinsics"],
                        "world_from_camera": world_from_camera,
                        "confidence": camera["confidence"],
                        "registration_class": camera["registration_class"],
                        "evidence_class": camera["evidence_class"],
                        "diagnostics": {
                            **dict(camera.get("diagnostics", {})),
                            "consolidated_from_solution_id": solution_id,
                            "consolidation_preserves_authority": True,
                        },
                        "distortion_model": camera.get("distortion_model"),
                        "sensor_model": camera.get("sensor_model"),
                        "crop": camera.get("crop"),
                        "clipping": camera.get("clipping"),
                        "coordinate_transform": camera.get("coordinate_transform"),
                        "camera_source_identity": camera.get("camera_source_identity"),
                        "solve_method": camera.get("solve_method"),
                    }
                )
        if require_all_acceptance_references and reference_ids != acceptance_reference_ids:
            missing_references = sorted(acceptance_reference_ids - reference_ids)
            extra_references = sorted(reference_ids - acceptance_reference_ids)
            raise ValueError(
                "camera consolidation must exactly cover acceptance references; "
                f"missing={missing_references}, extra={extra_references}"
            )
        result = self.import_manual(
            cameras,
            diagnostics={
                "authority": (
                    "consolidated camera hypotheses; matrices and registration classes "
                    "preserved; explicit approval still required"
                ),
                "consolidated_from": sources,
                "exact_acceptance_reference_coverage": reference_ids
                == acceptance_reference_ids,
            },
        )
        return {
            **result,
            "consolidation": {
                "source_solution_ids": ordered_ids,
                "camera_count": len(cameras),
                "reference_ids": sorted(reference_ids),
                "exact_acceptance_reference_coverage": reference_ids
                == acceptance_reference_ids,
                "authority_upgraded": False,
            },
        }

    def approve(self, solution_id: str, *, reviewer: str, reason: str) -> dict[str, Any]:
        if not reviewer.strip():
            raise ValueError("camera approval requires a named reviewer")
        if not reason.strip():
            raise ValueError("camera approval requires a reason")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
            reference_ids = {
                item[0]
                for item in connection.execute(
                    "SELECT id FROM reference_items WHERE media_type LIKE 'image/%' "
                    "AND acceptance_eligible=1"
                ).fetchall()
            }
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        decision_store = CameraDecisionStore(self.project)
        prepared = decision_store.prepare_for_decision(solution_id)
        document = prepared["document"]
        cameras = document.get("cameras", []) if isinstance(document, dict) else document
        for camera in cameras:
            validate_complete_camera_state(camera)
        covered = {camera.get("reference_id") for camera in cameras}
        if covered != reference_ids or len(cameras) != len(reference_ids):
            raise ValueError(
                "camera approval requires exactly one covered set of project references"
            )
        for camera in cameras:
            if camera.get("registration_class") == RegistrationClass.METRIC.value:
                quality = camera.get("diagnostics", {}).get("quality", {})
                _validate_camera_quality(quality)
        return decision_store.record(
            solution_id,
            document=document,
            state="approved",
            reviewer=reviewer,
            reason=reason,
            _migration=prepared["migration"],
            _expected_document=prepared["expected_document"],
        )

    def derive_undistorted_solution(self, solution_id: str) -> dict[str, Any]:
        """Create source-linked pinhole images without changing pose or camera authority."""
        try:
            import cv2  # type: ignore[import-not-found]
            import numpy as np  # type: ignore[import-not-found]
        except ImportError as error:
            raise BackendUnavailable(
                "camera undistortion requires the vision extra (OpenCV and NumPy)"
            ) from error
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        document = json.loads(row["solution_json"])
        cameras = document.get("cameras", [])
        if not cameras:
            raise ValueError("camera solution has no cameras to undistort")
        references = {item["id"]: item for item in ReferenceIngestor(self.project).list()}
        output_root = self.project.root / "references" / "derived" / f"undistorted-{solution_id}"
        output_root.mkdir(parents=True, exist_ok=True)
        derived_cameras: list[CameraSolution] = []
        derivations = []
        video_source_ids: set[str] = set()
        source_reference_ids: list[str] = []
        for camera in cameras:
            reference = references.get(camera["reference_id"])
            if reference is None or not reference["media_type"].startswith("image/"):
                raise ValueError("camera undistortion requires registered image references")
            source_path = self.project.root / reference["relative_path"]
            source_reference_ids.append(reference["id"])
            image = cv2.imread(str(source_path), cv2.IMREAD_UNCHANGED)
            if image is None:
                raise ValueError(f"undistortion could not decode reference: {reference['id']}")
            width, height = int(camera["width"]), int(camera["height"])
            intrinsics = camera["intrinsics"]
            matrix = np.array(
                [
                    [float(intrinsics["fx"]), 0.0, float(intrinsics["cx"])],
                    [0.0, float(intrinsics["fy"]), float(intrinsics["cy"])],
                    [0.0, 0.0, 1.0],
                ],
                dtype=np.float64,
            )
            distortion = self._opencv_distortion(intrinsics)
            new_matrix, roi = cv2.getOptimalNewCameraMatrix(
                matrix, distortion, (width, height), 0.0, (width, height)
            )
            undistorted = cv2.undistort(image, matrix, distortion, None, new_matrix)
            destination = output_root / f"{reference['id']}.png"
            if not cv2.imwrite(str(destination), undistorted):
                raise RuntimeError(f"failed to write undistorted reference: {destination}")
            derived = ReferenceIngestor(self.project).import_file(
                destination,
                rights_state=reference["rights_state"],
                viewpoint_label=reference.get("viewpoint_label"),
                evidence_role="acceptance_undistorted_reference",
                acceptance_eligible=True,
            )
            source_metadata = reference.get("metadata", {})
            video_source_id = source_metadata.get("video_source_reference_id")
            if video_source_id:
                video_source_ids.add(str(video_source_id))
            metadata = {
                **derived["metadata"],
                "derived_from_reference_id": reference["id"],
                "derived_from_artifact_digest": reference["artifact_digest"],
                "derivation": "OpenCV cv2.undistort from immutable stored camera intrinsics",
                "source_camera_solution_id": solution_id,
                "video_source_reference_id": video_source_id,
                "undistortion_roi": [int(value) for value in roi],
            }
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE reference_items SET metadata_json=? WHERE id=?",
                    (json.dumps(metadata), derived["id"]),
                )
            derived_cameras.append(
                CameraSolution(
                    reference_id=derived["id"],
                    model="PINHOLE",
                    width=width,
                    height=height,
                    intrinsics={
                        "fx": float(new_matrix[0, 0]),
                        "fy": float(new_matrix[1, 1]),
                        "cx": float(new_matrix[0, 2]),
                        "cy": float(new_matrix[1, 2]),
                    },
                    world_from_camera=camera["extrinsics"]["world_from_camera"],
                    confidence=float(camera["confidence"]),
                    registration_class=camera["registration_class"],
                    evidence_class=EvidenceClass(camera["evidence_class"]),
                    diagnostics={
                        **camera.get("diagnostics", {}),
                        "undistorted_from_reference_id": reference["id"],
                        "undistortion_source_solution_id": solution_id,
                        "scale_authority": "unchanged_from_source_solution",
                    },
                )
            )
            derivations.append(
                {
                    "source_reference_id": reference["id"],
                    "derived_reference_id": derived["id"],
                    "derived_artifact_digest": derived["artifact"]["digest"],
                    "roi": [int(value) for value in roi],
                }
            )
        with self.project.connection() as connection:
            connection.executemany(
                "UPDATE reference_items SET acceptance_eligible=0,"
                "evidence_role='diagnostic_distorted_source' WHERE id=?",
                [(reference_id,) for reference_id in source_reference_ids],
            )
            for video_source_id in video_source_ids:
                connection.execute(
                    "UPDATE reference_items SET acceptance_eligible=0,"
                    "evidence_role='diagnostic_distorted_or_unregistered_frame' "
                    "WHERE json_extract(metadata_json,'$.video_source_reference_id')=?",
                    (video_source_id,),
                )
            connection.executemany(
                "UPDATE reference_items SET acceptance_eligible=1,"
                "evidence_role='acceptance_undistorted_reference' WHERE id=?",
                [(camera.reference_id,) for camera in derived_cameras],
            )
        diagnostics = {
            "source_solution_id": solution_id,
            "source_backend": document.get("backend"),
            "derived_camera_count": len(derived_cameras),
            "derivations": derivations,
            "pose_changed": False,
            "scale_changed": False,
            "authority": (
                "undistortion removes the raster distortion model only; registration and scale "
                "authority remain unchanged"
            ),
        }
        result = self._store_solution("colmap_undistorted", derived_cameras, diagnostics)
        derivation_store = ReferenceDerivationStore(self.project)
        result["reference_derivations"] = [
            derivation_store.register_undistortion(item["derived_reference_id"])
            for item in derivations
        ]
        return result

    @staticmethod
    def _opencv_distortion(intrinsics: dict[str, Any]):
        import numpy as np  # type: ignore[import-not-found]

        return np.array(
            [
                float(intrinsics.get("k1", intrinsics.get("distortion_0", 0.0))),
                float(intrinsics.get("k2", intrinsics.get("distortion_1", 0.0))),
                float(intrinsics.get("p1", intrinsics.get("distortion_2", 0.0))),
                float(intrinsics.get("p2", intrinsics.get("distortion_3", 0.0))),
                float(intrinsics.get("k3", intrinsics.get("distortion_4", 0.0))),
            ],
            dtype=np.float64,
        )

    def reject(self, solution_id: str, *, reviewer: str, reason: str) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("camera rejection requires a named reviewer and reason")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        decision_store = CameraDecisionStore(self.project)
        prepared = decision_store.prepare_for_decision(solution_id)
        return decision_store.record(
            solution_id,
            document=prepared["document"],
            state="rejected",
            reviewer=reviewer,
            reason=reason,
            _migration=prepared["migration"],
            _expected_document=prepared["expected_document"],
        )

    def _solve_colmap(self, references: list[dict[str, Any]]) -> dict[str, Any]:
        executable = shutil.which("colmap")
        if executable is None:
            raise BackendUnavailable("COLMAP is not installed")
        if len(references) < 2:
            raise BlenderVisionError("COLMAP requires at least two distinct views")
        run_id = str(uuid.uuid4())
        root = self.project.root / "cameras" / "colmap" / run_id
        images = root / "images"
        sparse = root / "sparse"
        text_model = root / "text"
        images.mkdir(parents=True, exist_ok=True)
        sparse.mkdir(parents=True, exist_ok=True)
        name_to_reference: dict[str, dict[str, Any]] = {}
        for reference in references:
            suffix = Path(reference["original_name"]).suffix.lower() or ".png"
            filename = f"{reference['id']}{suffix}"
            source = self.project.root / reference["relative_path"]
            destination = images / filename
            try:
                destination.hardlink_to(source)
            except OSError:
                shutil.copy2(source, destination)
            name_to_reference[filename] = reference
        database = root / "database.db"
        feature_help = subprocess.run(
            [executable, "feature_extractor", "--help"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        matcher_help = subprocess.run(
            [executable, "exhaustive_matcher", "--help"],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        feature_help_text = feature_help.stdout + feature_help.stderr
        matcher_help_text = matcher_help.stdout + matcher_help.stderr
        feature_gpu_flag = (
            "--FeatureExtraction.use_gpu"
            if "--FeatureExtraction.use_gpu" in feature_help_text
            else "--SiftExtraction.use_gpu"
        )
        matcher_gpu_flag = (
            "--FeatureMatching.use_gpu"
            if "--FeatureMatching.use_gpu" in matcher_help_text
            else "--SiftMatching.use_gpu"
        )
        commands = [
            [
                executable,
                "feature_extractor",
                "--database_path",
                str(database),
                "--image_path",
                str(images),
                "--ImageReader.single_camera",
                "0",
                feature_gpu_flag,
                "0",
            ],
            [
                executable,
                "exhaustive_matcher",
                "--database_path",
                str(database),
                matcher_gpu_flag,
                "0",
            ],
            [
                executable,
                "mapper",
                "--database_path",
                str(database),
                "--image_path",
                str(images),
                "--output_path",
                str(sparse),
            ],
        ]
        command_logs = []
        for command in commands:
            result = subprocess.run(
                command, capture_output=True, text=True, timeout=900, check=False
            )
            command_logs.append(
                {
                    "command": Path(command[0]).name + " " + command[1],
                    "returncode": result.returncode,
                    "tail": (result.stdout + result.stderr)[-4000:],
                }
            )
            if result.returncode != 0:
                atomic_write_json(root / "commands.json", command_logs)
                raise BlenderVisionError(
                    f"COLMAP {command[1]} failed with exit {result.returncode}: "
                    f"{command_logs[-1]['tail'][-1000:]}"
                )
        models = sorted(path for path in sparse.iterdir() if path.is_dir())
        if not models:
            raise BlenderVisionError("COLMAP did not produce a registered sparse model")
        text_model.mkdir(parents=True, exist_ok=True)
        result = subprocess.run(
            [
                executable,
                "model_converter",
                "--input_path",
                str(models[0]),
                "--output_path",
                str(text_model),
                "--output_type",
                "TXT",
            ],
            capture_output=True,
            text=True,
            timeout=120,
            check=False,
        )
        if result.returncode != 0:
            raise BlenderVisionError("COLMAP model conversion failed")
        cameras = self._parse_colmap_cameras(text_model / "cameras.txt")
        solutions = self._parse_colmap_images(text_model / "images.txt", cameras, name_to_reference)
        if len(solutions) < 2:
            raise BlenderVisionError("COLMAP registered fewer than two reference views")
        diagnostics = {
            "registered_images": len(solutions),
            "input_images": len(references),
            "commands": command_logs,
            "workspace": str(root.relative_to(self.project.root)),
            "feature_gpu_flag": feature_gpu_flag,
            "matcher_gpu_flag": matcher_gpu_flag,
        }
        atomic_write_json(root / "commands.json", command_logs)
        return self._store_solution("colmap", solutions, diagnostics)

    @staticmethod
    def _parse_colmap_cameras(path: Path) -> dict[int, dict[str, Any]]:
        cameras = {}
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line or line.startswith("#"):
                continue
            fields = line.split()
            camera_id, model, width, height = (
                int(fields[0]),
                fields[1],
                int(fields[2]),
                int(fields[3]),
            )
            cameras[camera_id] = {
                "model": model,
                "width": width,
                "height": height,
                "intrinsics": _intrinsics(model, [float(value) for value in fields[4:]]),
            }
        return cameras

    @staticmethod
    def _parse_colmap_images(
        path: Path,
        cameras: dict[int, dict[str, Any]],
        name_to_reference: dict[str, dict[str, Any]],
    ) -> list[CameraSolution]:
        lines = [
            line
            for line in path.read_text(encoding="utf-8").splitlines()
            if line and not line.startswith("#")
        ]
        solutions = []
        for index in range(0, len(lines), 2):
            fields = lines[index].split()
            if len(fields) < 10:
                continue
            q = [float(value) for value in fields[1:5]]
            translation = [float(value) for value in fields[5:8]]
            camera = cameras[int(fields[8])]
            name = fields[9]
            reference = name_to_reference.get(name)
            if reference is None:
                continue
            solutions.append(
                CameraSolution(
                    reference_id=reference["id"],
                    model=camera["model"],
                    width=camera["width"],
                    height=camera["height"],
                    intrinsics=camera["intrinsics"],
                    world_from_camera=_world_from_colmap(q, translation),
                    confidence=0.75,
                    registration_class=RegistrationClass.FEATURE_BASED.value,
                    evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
                    diagnostics={
                        "source_name": reference["original_name"],
                        "view_direction": CameraSolver._view_direction(
                            _world_from_colmap(q, translation)
                        ),
                    },
                )
            )
        return solutions

    @staticmethod
    def _view_direction(world_from_camera: list[list[float]]) -> list[float]:
        center = [world_from_camera[index][3] for index in range(3)]
        length = math.sqrt(sum(component * component for component in center)) or 1.0
        return [-component / length for component in center]

    def _solve_turntable(
        self, references: list[dict[str, Any]], diagnostics: dict[str, Any]
    ) -> dict[str, Any]:
        solutions = []
        for index, reference in enumerate(references):
            width = int(reference["metadata"].get("width", 1024))
            height = int(reference["metadata"].get("height", 1024))
            direction, view_direction, roll, hint_applied = self._fallback_pose(
                reference, index, len(references)
            )
            position = [component * 500.0 for component in direction]
            solutions.append(
                CameraSolution(
                    reference_id=reference["id"],
                    model="PINHOLE",
                    width=width,
                    height=height,
                    intrinsics={
                        "fx": width * 1.25,
                        "fy": width * 1.25,
                        "cx": width / 2,
                        "cy": height / 2,
                    },
                    world_from_camera=_blender_world_from_look_at(position, roll_degrees=roll),
                    confidence=0.2,
                    registration_class=RegistrationClass.APPROXIMATE_VISUAL.value,
                    evidence_class=EvidenceClass.INFERRED_LOW_CONFIDENCE,
                    diagnostics={
                        "direction": list(direction),
                        "view_direction": view_direction,
                        "camera_roll_degrees": roll,
                        "manifest_camera_hint_applied": hint_applied,
                        "source_name": reference["original_name"],
                    },
                )
            )
        diagnostics.update(
            {
                "registered_images": len(solutions),
                "warning": "Turntable fallback is non-metric and cannot satisfy an L3 camera gate.",
            }
        )
        return self._store_solution("turntable_fallback", solutions, diagnostics)

    def _store_solution(
        self, backend: str, solutions: list[CameraSolution], diagnostics: dict[str, Any]
    ) -> dict[str, Any]:
        solution_id = str(uuid.uuid4())
        with self.project.connection() as connection:
            sources = {
                row["id"]: dict(row)
                for row in connection.execute(
                    "SELECT id,artifact_digest,original_name FROM reference_items"
                ).fetchall()
            }
        cameras = [
            complete_camera_state(
                solution.to_dict(), backend=backend, source=sources.get(solution.reference_id)
            )
            for solution in solutions
        ]
        for camera in cameras:
            validate_complete_camera_state(camera)
        value = {
            "id": solution_id,
            "backend": backend,
            "created_at": utc_now(),
            "cameras": cameras,
            "diagnostics": diagnostics,
            "approved": False,
            "approval": {"state": "pending", "reviewer": None, "reason": None},
        }
        destination = self.project.root / "cameras" / f"solution_{solution_id}.json"
        atomic_write_json(destination, value)
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO camera_solutions"
                "(id,backend,solution_json,diagnostics_json,created_at,approved) "
                "VALUES(?,?,?,?,?,0)",
                (
                    solution_id,
                    backend,
                    json.dumps(value),
                    json.dumps(diagnostics),
                    value["created_at"],
                ),
            )
        value["path"] = str(destination.relative_to(self.project.root))
        return value


def camera_specs_for_scene(
    camera_solution: dict[str, Any], scene_inventory: dict[str, Any]
) -> list[dict[str, Any]]:
    bounds = scene_inventory["scene"]["bounds"]
    minimum, maximum = bounds["minimum"], bounds["maximum"]
    center = [(minimum[index] + maximum[index]) / 2.0 for index in range(3)]
    diameter = max(bounds["dimensions"]) or 1.0
    radius = diameter * 2.8
    specifications = []
    for camera in camera_solution["cameras"]:
        direction = camera.get("diagnostics", {}).get("direction")
        if direction is None:
            center_from_solution = [camera["world_from_camera"][index][3] for index in range(3)]
            length = math.sqrt(sum(value * value for value in center_from_solution)) or 1.0
            direction = [value / length for value in center_from_solution]
        fx = camera["intrinsics"].get("fx", camera["width"] * 1.25)
        specifications.append(
            {
                "name": f"BVMCP_{camera['reference_id']}",
                "reference_id": camera["reference_id"],
                "position": [center[index] + direction[index] * radius for index in range(3)],
                "target": center,
                "lens_mm": fx / max(1, camera["width"]) * 36.0,
                "sensor_width_mm": 36.0,
                "width": camera["width"],
                "height": camera["height"],
                "camera_roll_degrees": float(
                    camera.get("diagnostics", {}).get("camera_roll_degrees", 0.0)
                ),
            }
        )
    return specifications
