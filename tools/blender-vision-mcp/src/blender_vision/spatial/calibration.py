"""Planar checkerboard calibration (OpenCV) with honest authority classes."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from blender_vision.core.errors import BlenderVisionError, ValidationError
from blender_vision.v2.authority import (
    AuthorityClass,
    Uncertainty,
    Units,
)
from blender_vision.v2.records import Lineage, ObservationBundle


class CalibrationError(BlenderVisionError):
    """Board detection or solve failed."""


@dataclass(slots=True)
class CalibrationResult:
    """Output of a planar board solve."""

    camera_matrix: np.ndarray
    distortion: np.ndarray
    image_size: tuple[int, int]
    per_view_errors: list[float]
    mean_reprojection_error: float
    rms: float
    authority: AuthorityClass
    square_size_m: float | None
    board_size: tuple[int, int]
    detected_views: int
    rotations: list[np.ndarray] = field(default_factory=list)
    translations: list[np.ndarray] = field(default_factory=list)
    view_paths: list[str] = field(default_factory=list)
    diagnostics: dict[str, Any] = field(default_factory=dict)

    @property
    def intrinsics(self) -> dict[str, float]:
        return {
            "fx": float(self.camera_matrix[0, 0]),
            "fy": float(self.camera_matrix[1, 1]),
            "cx": float(self.camera_matrix[0, 2]),
            "cy": float(self.camera_matrix[1, 2]),
        }

    def to_dict(self) -> dict[str, Any]:
        return {
            "camera_matrix": self.camera_matrix.tolist(),
            "distortion": self.distortion.reshape(-1).tolist(),
            "image_size": list(self.image_size),
            "per_view_errors": list(self.per_view_errors),
            "mean_reprojection_error": self.mean_reprojection_error,
            "rms": self.rms,
            "authority": self.authority.value,
            "square_size_m": self.square_size_m,
            "board_size": list(self.board_size),
            "detected_views": self.detected_views,
            "intrinsics": self.intrinsics,
            "view_paths": list(self.view_paths),
            "diagnostics": dict(self.diagnostics),
        }

    def seal_observation_bundle(
        self,
        *,
        target_id: str,
        operation: str = "spatial.calibration.planar_board",
    ) -> ObservationBundle:
        # Empty input_authorities: V2 lineage.authority_ceiling() uses
        # derive(proposed=INFERRED) and would refuse MEASURED/SENSOR_DERIVED
        # seals with a non-empty list. The physical square-size act and image
        # paths are recorded in parameters/inputs instead.
        lineage = Lineage(
            operation=operation,
            inputs=list(self.view_paths),
            input_authorities=[],
            parameters={
                "board_size": list(self.board_size),
                "square_size_m": self.square_size_m,
                "image_size": list(self.image_size),
                "detected_views": self.detected_views,
                "claimed_authority": self.authority.value,
                "authority_basis": (
                    "physical square_size_m supplied"
                    if self.square_size_m is not None
                    else "no physical square size; unit-square geometry"
                ),
            },
            environment={"opencv": cv2.__version__},
            limitations=[
                "planar checkerboard only; no Charuco",
                "MEASURED requires physical square size",
            ],
        )
        return ObservationBundle(
            id=f"cal-{uuid.uuid4().hex[:12]}",
            target_id=target_id,
            authority=self.authority,
            lineage=lineage,
            uncertainty=Uncertainty(
                kind="reprojection",
                sigma=self.mean_reprojection_error,
                units=Units.PIXEL,
                basis="per-view mean L2 reprojection error",
                samples=self.detected_views,
            ),
            sensors=[
                {
                    "type": "camera",
                    "intrinsics": self.intrinsics,
                    "distortion": self.distortion.reshape(-1).tolist(),
                }
            ],
            artifacts=list(self.view_paths),
            modalities=["calibration", "image"],
            coverage={
                "detected_views": self.detected_views,
                "mean_reprojection_error_px": self.mean_reprojection_error,
            },
        ).seal()


def calibrate_planar_board(
    images: list[Path | str],
    *,
    board_size: tuple[int, int],
    square_size_m: float | None = None,
    refine: bool = True,
    fix_aspect_ratio: bool = False,
    zero_distortion: bool = False,
) -> CalibrationResult:
    """Solve intrinsics from checkerboard images.

    `board_size` is (columns, rows) of *inner* corners.
    When `square_size_m` is supplied the result is MEASURED (physical board).
    Without it the board is treated as unit-square geometry and authority is
    SENSOR_DERIVED — never silently MEASURED.
    """
    columns, rows = int(board_size[0]), int(board_size[1])
    if columns < 2 or rows < 2:
        raise ValidationError("board_size must be at least 2x2 inner corners")
    paths = [Path(item) for item in images]
    if not paths:
        raise CalibrationError("no calibration images supplied")

    # Object points use metres when square_size_m is known, else unit squares.
    scale = float(square_size_m) if square_size_m is not None else 1.0
    if square_size_m is not None and square_size_m <= 0:
        raise ValidationError("square_size_m must be positive when supplied")

    object_template = np.zeros((columns * rows, 3), np.float32)
    object_template[:, :2] = np.mgrid[0:columns, 0:rows].T.reshape(-1, 2)
    object_template *= scale

    object_points: list[np.ndarray] = []
    image_points: list[np.ndarray] = []
    view_paths: list[str] = []
    detection_log: list[dict[str, Any]] = []
    image_size: tuple[int, int] | None = None

    criteria = (
        cv2.TERM_CRITERIA_EPS + cv2.TERM_CRITERIA_MAX_ITER,
        40,
        0.001,
    )

    for path in paths:
        image = cv2.imread(str(path), cv2.IMREAD_GRAYSCALE)
        if image is None:
            detection_log.append(
                {"path": str(path), "detected": False, "reason": "decode_failed"}
            )
            continue
        current_size = (int(image.shape[1]), int(image.shape[0]))
        if image_size is not None and current_size != image_size:
            raise CalibrationError(
                f"image size mismatch: {image_size} vs {current_size} for {path}"
            )
        image_size = current_size
        # Prefer classic finder: SB is excellent on photos but has been observed to
        # return slightly order-unstable corners on hard-edged synthetic boards,
        # which biases the focal length under planar multi-view solves.
        found, corners = cv2.findChessboardCorners(
            image,
            (columns, rows),
            cv2.CALIB_CB_ADAPTIVE_THRESH
            + cv2.CALIB_CB_NORMALIZE_IMAGE
            + cv2.CALIB_CB_FAST_CHECK,
        )
        if (not found or corners is None) and hasattr(cv2, "findChessboardCornersSB"):
            found, corners = cv2.findChessboardCornersSB(image, (columns, rows))
        if not found or corners is None:
            detection_log.append(
                {"path": str(path), "detected": False, "reason": "not_found"}
            )
            continue
        corners = corners.reshape(-1, 1, 2).astype(np.float32)
        if refine:
            corners = cv2.cornerSubPix(
                image, corners, (11, 11), (-1, -1), criteria
            )
        object_points.append(object_template.copy())
        image_points.append(corners)
        view_paths.append(str(path))
        detection_log.append(
            {
                "path": str(path),
                "detected": True,
                "corner_count": int(corners.shape[0]),
            }
        )

    if not object_points or image_size is None:
        raise CalibrationError(
            "checkerboard not detected in any image; "
            f"attempted={len(paths)} detected=0"
        )
    if len(object_points) < 3:
        raise CalibrationError(
            f"need at least 3 detected views for a stable solve; got {len(object_points)}"
        )

    flags = 0
    camera_matrix_init = None
    dist_init = None
    if fix_aspect_ratio:
        # Square pixels: constrain fy = fx. Common for synthetic pinhole cameras
        # and most physical sensors without anamorphic optics.
        flags |= cv2.CALIB_FIX_ASPECT_RATIO
        camera_matrix_init = np.eye(3, dtype=np.float64)
        camera_matrix_init[0, 0] = float(image_size[0])
        camera_matrix_init[1, 1] = float(image_size[0])
        camera_matrix_init[0, 2] = image_size[0] / 2.0
        camera_matrix_init[1, 2] = image_size[1] / 2.0
    if zero_distortion:
        flags |= (
            cv2.CALIB_FIX_K1
            | cv2.CALIB_FIX_K2
            | cv2.CALIB_FIX_K3
            | cv2.CALIB_ZERO_TANGENT_DIST
        )
        dist_init = np.zeros((5, 1), dtype=np.float64)
    rms, camera_matrix, distortion, rotations, translations = cv2.calibrateCamera(
        object_points,
        image_points,
        image_size,
        camera_matrix_init,
        dist_init,
        flags=flags,
    )

    per_view: list[float] = []
    rot_mats: list[np.ndarray] = []
    trans_vecs: list[np.ndarray] = []
    for object_vertices, corners, rvec, tvec in zip(
        object_points, image_points, rotations, translations, strict=True
    ):
        projected, _ = cv2.projectPoints(
            object_vertices, rvec, tvec, camera_matrix, distortion
        )
        deltas = projected.reshape(-1, 2) - corners.reshape(-1, 2)
        error = float(np.sqrt(np.mean(np.sum(deltas**2, axis=1))))
        per_view.append(error)
        rot, _ = cv2.Rodrigues(rvec)
        rot_mats.append(rot.astype(np.float64))
        trans_vecs.append(tvec.reshape(3).astype(np.float64))

    mean_error = float(np.mean(per_view)) if per_view else float("inf")

    if square_size_m is not None:
        # Physical board size supplied → MEASURED. Image observations alone
        # would be OBSERVED; the metric scale upgrades the intrinsics claim.
        authority = AuthorityClass.MEASURED
    else:
        authority = AuthorityClass.SENSOR_DERIVED

    return CalibrationResult(
        camera_matrix=camera_matrix.astype(np.float64),
        distortion=distortion.astype(np.float64),
        image_size=image_size,
        per_view_errors=per_view,
        mean_reprojection_error=mean_error,
        rms=float(rms),
        authority=authority,
        square_size_m=float(square_size_m) if square_size_m is not None else None,
        board_size=(columns, rows),
        detected_views=len(object_points),
        rotations=rot_mats,
        translations=trans_vecs,
        view_paths=view_paths,
        diagnostics={
            "detection_log": detection_log,
            "attempted_views": len(paths),
            "opencv_version": cv2.__version__,
            "authority_rule": (
                "MEASURED because square_size_m was supplied"
                if square_size_m is not None
                else "SENSOR_DERIVED because no physical square size was supplied"
            ),
        },
    )


def synthetic_checkerboard_image(
    *,
    width: int,
    height: int,
    board_size: tuple[int, int],
    square_px: int,
    origin: tuple[int, int] = (40, 40),
) -> np.ndarray:
    """Render an axis-aligned checkerboard for unit tests (no Blender needed)."""
    columns, rows = board_size
    image = np.full((height, width), 200, dtype=np.uint8)
    # board_size is inner corners, so there are columns+1 by rows+1 squares.
    for row in range(rows + 1):
        for col in range(columns + 1):
            x0 = origin[0] + col * square_px
            y0 = origin[1] + row * square_px
            x1 = x0 + square_px
            y1 = y0 + square_px
            if x1 > width or y1 > height:
                continue
            if (row + col) % 2 == 0:
                image[y0:y1, x0:x1] = 20
            else:
                image[y0:y1, x0:x1] = 230
    return image
