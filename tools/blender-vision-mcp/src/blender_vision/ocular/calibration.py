"""Sensor calibration: lens, principal point, timestamp skew, exposure, colour.

Authority is MEASURED only when a physical scale or calibrated target size is
supplied. Checkerboard geometry without a metric square size stays
SENSOR_DERIVED — pixels without metres are not a measurement.
"""

from __future__ import annotations

from collections.abc import Sequence
from contextlib import suppress
from pathlib import Path

import cv2
import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.records import SensorCalibration, default_lineage
from blender_vision.ocular.sensors import SensorRegistry, get_sensor
from blender_vision.v2.authority import AuthorityClass, CoordinateFrame, Units


def _load_gray(path: Path) -> np.ndarray:
    image = cv2.imread(str(path), cv2.IMREAD_GRAYSCALE)
    if image is None:
        raise ValidationError(f"could not read calibration image: {path}")
    return image


def _detect_board(
    gray: np.ndarray, board_size: tuple[int, int]
) -> tuple[bool, np.ndarray | None]:
    flags = cv2.CALIB_CB_ADAPTIVE_THRESH | cv2.CALIB_CB_NORMALIZE_IMAGE
    found, corners = cv2.findChessboardCorners(gray, board_size, flags)
    if not found or corners is None:
        return False, None
    criteria = (cv2.TERM_CRITERIA_EPS + cv2.TERM_CRITERIA_MAX_ITER, 30, 0.001)
    refined = cv2.cornerSubPix(gray, corners, (11, 11), (-1, -1), criteria)
    return True, refined


def _object_points(board_size: tuple[int, int], square_m: float) -> np.ndarray:
    cols, rows = board_size
    obj = np.zeros((cols * rows, 3), dtype=np.float32)
    grid = np.mgrid[0:cols, 0:rows].T.reshape(-1, 2)
    obj[:, :2] = grid * float(square_m)
    return obj


def _timestamp_skew_ms(timestamps: Sequence[float] | None) -> float | None:
    if not timestamps or len(timestamps) < 3:
        return None
    arr = np.asarray(timestamps, dtype=np.float64)
    diffs = np.diff(arr)
    if diffs.size == 0:
        return None
    # Skew proxy: deviation of inter-frame intervals from the median interval.
    median = float(np.median(diffs))
    if median <= 0:
        return None
    return float(np.median(np.abs(diffs - median)) * 1000.0)


def _exposure_pumping(means: Sequence[float]) -> bool:
    if len(means) < 6:
        return False
    arr = np.asarray(means, dtype=np.float64)
    # Pumping: alternating high/low mean luminance with large peak-to-peak.
    diffs = np.diff(arr)
    sign_changes = int(np.sum(diffs[:-1] * diffs[1:] < 0))
    peak_to_peak = float(arr.max() - arr.min())
    return sign_changes >= len(arr) // 2 and peak_to_peak > 12.0


def _colour_temperature_drift_k(images_bgr: Sequence[np.ndarray]) -> float | None:
    if len(images_bgr) < 2:
        return None
    temps: list[float] = []
    for image in images_bgr:
        if image.ndim != 3:
            continue
        b, g, r = [float(np.mean(image[:, :, c])) + 1e-3 for c in range(3)]
        # McCamy-ish chromaticity proxy in Kelvin; relative drift only.
        n = (r - g) / (r + g - 2.0 * b + 1e-6)
        cct = 449.0 * n**3 + 3525.0 * n**2 + 6823.3 * n + 5520.33
        temps.append(float(cct))
    if len(temps) < 2:
        return None
    return float(abs(temps[-1] - temps[0]))


def calibrate_sensor(
    image_paths: Sequence[str | Path],
    *,
    sensor_id: str,
    board_size: tuple[int, int] = (9, 6),
    square_m: float | None = None,
    physical_scale_m: float | None = None,
    timestamps: Sequence[float] | None = None,
    coordinate_frame: CoordinateFrame | None = None,
    registry: SensorRegistry | None = None,
) -> SensorCalibration:
    """Calibrate from checkerboard images.

    MEASURED authority requires a physical scale (square_m or physical_scale_m).
    Without one, authority is capped at SENSOR_DERIVED even if corners resolve.
    """
    paths = [Path(p) for p in image_paths]
    if len(paths) < 1:
        raise ValidationError("calibrate_sensor requires at least one image")

    # Calibration may run before stream open; sensor absence is noted.
    if registry is not None or sensor_id:
        with suppress(ValidationError):
            get_sensor(sensor_id, registry=registry)

    obj_points: list[np.ndarray] = []
    img_points: list[np.ndarray] = []
    means: list[float] = []
    colour_images: list[np.ndarray] = []
    image_size: tuple[int, int] | None = None
    # Unit square when no metric size: geometry only, never MEASURED.
    metric = square_m if square_m is not None else 1.0
    has_physical = square_m is not None or physical_scale_m is not None

    for path in paths:
        gray = _load_gray(path)
        means.append(float(np.mean(gray)))
        colour = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if colour is not None:
            colour_images.append(colour)
        image_size = (int(gray.shape[1]), int(gray.shape[0]))
        found, corners = _detect_board(gray, board_size)
        if not found or corners is None:
            continue
        obj_points.append(_object_points(board_size, metric))
        img_points.append(corners.reshape(-1, 2).astype(np.float32))

    frame = coordinate_frame or CoordinateFrame(
        name="opencv-camera",
        up_axis="-Y",
        forward_axis="+Z",
        units=Units.METRE if has_physical else Units.PIXEL,
        origin_semantics="camera-centre",
        scale_authority=(
            AuthorityClass.MEASURED if has_physical else AuthorityClass.UNRESOLVED
        ),
    )

    camera_matrix: list[list[float]] = []
    dist: list[float] = []
    principal = [0.0, 0.0]
    reproj: float | None = None
    method = "none"
    limitations: list[str] = []

    if image_size is None:
        raise ValidationError("no readable calibration images")

    if len(obj_points) >= 3:
        # OpenCV expects object points as list of (N,3) float32.
        rms, mtx, dist_coeffs, _rvecs, _tvecs = cv2.calibrateCamera(
            obj_points,
            img_points,
            image_size,
            None,
            None,
        )
        camera_matrix = mtx.tolist()
        dist = dist_coeffs.reshape(-1).astype(float).tolist()
        principal = [float(mtx[0, 2]), float(mtx[1, 2])]
        reproj = float(rms)
        method = "opencv.calibrateCamera"
    elif len(obj_points) >= 1:
        # Single-view principal-point estimate from board centre of mass.
        pts = img_points[0]
        principal = [float(np.mean(pts[:, 0])), float(np.mean(pts[:, 1]))]
        fx = fy = float(max(image_size))
        camera_matrix = [
            [fx, 0.0, principal[0]],
            [0.0, fy, principal[1]],
            [0.0, 0.0, 1.0],
        ]
        method = "principal_point_from_board"
        limitations.append("fewer than 3 board detections; full lens model not fit")
    else:
        # No board: principal point defaults to image centre.
        principal = [image_size[0] / 2.0, image_size[1] / 2.0]
        fx = fy = float(max(image_size))
        camera_matrix = [
            [fx, 0.0, principal[0]],
            [0.0, fy, principal[1]],
            [0.0, 0.0, 1.0],
        ]
        method = "image_centre_fallback"
        limitations.append("no checkerboard corners found; principal point is image centre")

    if not has_physical:
        limitations.append(
            "no physical scale supplied (square_m / physical_scale_m); "
            "authority capped at SENSOR_DERIVED"
        )

    authority = AuthorityClass.MEASURED if has_physical and len(obj_points) >= 1 else (
        AuthorityClass.SENSOR_DERIVED if len(obj_points) >= 1 else AuthorityClass.INFERRED
    )
    # Hard law: MEASURED requires physical scale even if the board is perfect.
    if authority is AuthorityClass.MEASURED and not has_physical:
        authority = AuthorityClass.SENSOR_DERIVED

    calibration = SensorCalibration(
        id=f"calib-{sensor_id}-{method}",
        sensor_id=sensor_id,
        frame=frame,
        image_size=[image_size[0], image_size[1]],
        camera_matrix=camera_matrix,
        distortion_coefficients=dist,
        principal_point=principal,
        reprojection_error_px=reproj,
        timestamp_skew_ms=_timestamp_skew_ms(timestamps),
        exposure_pumping_detected=_exposure_pumping(means),
        colour_temperature_drift_k=_colour_temperature_drift_k(colour_images),
        physical_scale_m=physical_scale_m if physical_scale_m is not None else square_m,
        board_square_m=square_m,
        samples_used=len(obj_points),
        method=method,
        limitations=limitations,
        authority=authority,
        lineage=default_lineage(
            "ocular.calibrate_sensor",
            inputs=[str(p) for p in paths],
        ),
    )
    # Do not populate lineage.input_authorities with synthetic authorities:
    # Lineage.authority_ceiling() is derive(proposed=INFERRED) and would cap
    # SENSOR_DERIVED/MEASURED claims incorrectly. Scale evidence is recorded on
    # the record fields (physical_scale_m, board_square_m, limitations).
    return calibration.seal()
