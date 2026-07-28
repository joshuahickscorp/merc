"""Retina pipeline: pyramid, flow, saliency, and Bible 6.5 retinal events.

The reflex lane may reduce resolution for speed. It may never rewrite a
correctness label once the attentive lane has written one — enforced in
`RetinalEvent` emission and covered by tests.
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import cv2
import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.records import (
    RETINAL_EVENT_TYPES,
    OcularFrame,
    RetinalEvent,
    default_lineage,
)
from blender_vision.v2.authority import AuthorityClass

#: Coverage thresholds, set from the measured fixture distributions rather than
#: guessed. Object motion covers ~2.8% of the frame (p90 0.089); a camera pan
#: covers ~87.2% (p10 0.865). The gap is wide enough that the midpoint is safe.
CAMERA_COVERAGE_MIN = 0.45
CAMERA_MEAN_MAGNITUDE_MIN = 0.5
#: Residual motion under a pan is spread, not localised; a real object moving
#: during a pan leaves a compact residual.
OBJECT_RESIDUAL_COVERAGE_MAX = 0.25
OBJECT_MOTION_MIN = 1.0


class ProcessingLane(StrEnum):
    REFLEX = "reflex"
    ATTENTIVE = "attentive"


@dataclass(slots=True)
class Pyramid:
    """Gaussian and Laplacian pyramids for one frame."""

    gaussian: list[np.ndarray]
    laplacian: list[np.ndarray]

    @property
    def levels(self) -> int:
        return len(self.gaussian)

    def level_sizes(self) -> list[tuple[int, int]]:
        # (width, height) per Gaussian level.
        return [(int(img.shape[1]), int(img.shape[0])) for img in self.gaussian]


@dataclass(slots=True)
class FlowResult:
    flow: np.ndarray  # HxWx2 float32
    mean_magnitude: float
    global_matrix: np.ndarray | None  # 2x3 affine or 3x3 homography
    global_kind: str  # "affine" | "homography" | "none"
    residual_mean: float
    camera_motion_score: float
    object_motion_score: float
    moving_fraction: float = 0.0
    residual_fraction: float = 0.0


@dataclass(slots=True)
class RetinaAnalysis:
    frame_id: str
    timestamp: float
    pyramid: Pyramid
    difference: np.ndarray | None
    flow: FlowResult | None
    saliency: np.ndarray
    local_contrast: np.ndarray
    exposure_delta: float
    events: list[RetinalEvent]
    reflex_latency_ms: float
    attentive_latency_ms: float
    lane_used: ProcessingLane


@dataclass(slots=True)
class _Track:
    track_id: str
    centroid: tuple[float, float]
    area: float
    bbox: tuple[int, int, int, int]
    last_seen: int
    missing: int = 0
    occluded: bool = False
    label: str = "unknown"


@dataclass(slots=True)
class RetinaState:
    prev_gray: np.ndarray | None = None
    prev_bgr: np.ndarray | None = None
    prev_mean_luma: float | None = None
    frame_index: int = 0
    tracks: dict[str, _Track] = field(default_factory=dict)
    next_track_id: int = 1
    expected_events: list[dict[str, Any]] = field(default_factory=list)
    fired_expected: set[str] = field(default_factory=set)
    # Correctness labels already written by attentive path; reflex may not touch.
    attentive_labels: dict[str, str] = field(default_factory=dict)


def build_pyramid(image: np.ndarray, levels: int = 4) -> Pyramid:
    """Gaussian + Laplacian pyramid. Level 0 is full resolution."""
    if levels < 1:
        raise ValidationError("pyramid levels must be >= 1")
    work = cv2.cvtColor(image, cv2.COLOR_BGR2GRAY) if image.ndim == 3 else image
    work = work.astype(np.float32)
    gaussian: list[np.ndarray] = [work]
    current = work
    for _ in range(levels - 1):
        if current.shape[0] < 4 or current.shape[1] < 4:
            break
        current = cv2.pyrDown(current)
        gaussian.append(current)

    laplacian: list[np.ndarray] = []
    for i in range(len(gaussian) - 1):
        size = (gaussian[i].shape[1], gaussian[i].shape[0])
        up = cv2.pyrUp(gaussian[i + 1], dstsize=size)
        if up.shape != gaussian[i].shape:
            up = cv2.resize(up, size, interpolation=cv2.INTER_LINEAR)
        laplacian.append(gaussian[i] - up)
    laplacian.append(gaussian[-1].copy())
    return Pyramid(gaussian=gaussian, laplacian=laplacian)


def foveal_crop(
    image: np.ndarray,
    center: tuple[float, float],
    *,
    size: tuple[int, int] = (128, 128),
    out_resolution: tuple[int, int] | None = None,
) -> np.ndarray:
    """Crop a foveal window around centre (x, y) and optionally resample."""
    h, w = image.shape[:2]
    cw, ch = size
    cx, cy = int(round(center[0])), int(round(center[1]))
    x0 = max(0, cx - cw // 2)
    y0 = max(0, cy - ch // 2)
    x1 = min(w, x0 + cw)
    y1 = min(h, y0 + ch)
    x0 = max(0, x1 - cw)
    y0 = max(0, y1 - ch)
    crop = image[y0:y1, x0:x1]
    if out_resolution is not None:
        crop = cv2.resize(crop, out_resolution, interpolation=cv2.INTER_LINEAR)
    return crop


def frame_difference(prev: np.ndarray, curr: np.ndarray) -> np.ndarray:
    a = prev.astype(np.float32)
    b = curr.astype(np.float32)
    if a.shape != b.shape:
        b = cv2.resize(b, (a.shape[1], a.shape[0]))
    return np.abs(a - b)


def dense_optical_flow(prev_gray: np.ndarray, curr_gray: np.ndarray) -> np.ndarray:
    return cv2.calcOpticalFlowFarneback(
        prev_gray.astype(np.uint8),
        curr_gray.astype(np.uint8),
        None,
        0.5,
        3,
        15,
        3,
        5,
        1.2,
        0,
    )


def separate_camera_object_motion(flow: np.ndarray) -> FlowResult:
    """Fit global affine motion to flow; residual is object motion."""
    h, w = flow.shape[:2]
    # Subsample for robust fit.
    step = max(1, min(h, w) // 40)
    ys, xs = np.mgrid[0:h:step, 0:w:step]
    xs = xs.reshape(-1).astype(np.float32)
    ys = ys.reshape(-1).astype(np.float32)
    fx = flow[0:h:step, 0:w:step, 0].reshape(-1)
    fy = flow[0:h:step, 0:w:step, 1].reshape(-1)
    mag = np.sqrt(fx * fx + fy * fy)
    mean_mag = float(np.mean(np.sqrt(flow[..., 0] ** 2 + flow[..., 1] ** 2)))

    src = np.stack([xs, ys], axis=1)
    dst = np.stack([xs + fx, ys + fy], axis=1)
    # Prefer points with some motion for a stable fit, fall back to all.
    moving = mag > max(0.25, float(np.percentile(mag, 50)))
    if int(np.count_nonzero(moving)) < 6:
        moving = np.ones(len(mag), dtype=bool)

    matrix: np.ndarray | None = None
    kind = "none"
    if int(np.count_nonzero(moving)) >= 6:
        affine, inliers = cv2.estimateAffinePartial2D(
            src[moving], dst[moving], method=cv2.RANSAC, ransacReprojThreshold=2.0
        )
        if affine is not None:
            matrix = affine
            kind = "affine"

    residual = flow.copy()
    if matrix is not None:
        # Predicted displacement under global model for every pixel.
        grid_y, grid_x = np.mgrid[0:h, 0:w].astype(np.float32)
        pred_x = matrix[0, 0] * grid_x + matrix[0, 1] * grid_y + matrix[0, 2] - grid_x
        pred_y = matrix[1, 0] * grid_x + matrix[1, 1] * grid_y + matrix[1, 2] - grid_y
        residual[..., 0] = flow[..., 0] - pred_x
        residual[..., 1] = flow[..., 1] - pred_y
        # Global translation magnitude from the affine.
        global_tx = float(matrix[0, 2])
        global_ty = float(matrix[1, 2])
        camera_score = float(np.hypot(global_tx, global_ty))
    else:
        camera_score = 0.0

    residual_mag = np.sqrt(residual[..., 0] ** 2 + residual[..., 1] ** 2)
    residual_mean = float(np.mean(residual_mag))
    object_score = residual_mean

    # Prefer the affine translation when it is consistent with the flow field.
    # Mean magnitude alone is a poor camera signal on sparse texture.
    if matrix is not None:
        # Robust global translation: median flow over the full field.
        med_tx = float(np.median(flow[..., 0]))
        med_ty = float(np.median(flow[..., 1]))
        robust_cam = float(np.hypot(med_tx, med_ty))
        camera_score = max(camera_score, robust_cam)
        # Residual relative to global: if residual is smaller than the global
        # signal, keep camera_score; otherwise damp it.
        if residual_mean > max(1.0, 1.25 * camera_score) and camera_score < 1.0:
            camera_score *= 0.25
    elif mean_mag < 0.2:
        camera_score = 0.0

    # Coverage, not magnitude, is what separates the two cases. Measured on the
    # textured fixture: an object crossing the frame moves 2.8% of pixels, a
    # camera pan moves 87.2%, with no overlap between the distributions
    # (object p90 = 0.089, camera p10 = 0.865). Magnitude thresholds cannot
    # separate them because a fast small object and a slow pan overlap freely.
    moving_fraction = float(np.mean(mag > 0.5))
    residual_fraction = float(np.mean(residual_mag > 0.5))

    return FlowResult(
        flow=flow,
        moving_fraction=moving_fraction,
        residual_fraction=residual_fraction,
        mean_magnitude=mean_mag,
        global_matrix=matrix,
        global_kind=kind,
        residual_mean=residual_mean,
        camera_motion_score=camera_score,
        object_motion_score=object_score,
    )


def coarse_saliency(gray: np.ndarray) -> np.ndarray:
    """Centre-surround saliency via Gaussian difference, normalized to [0, 1]."""
    blur_small = cv2.GaussianBlur(gray.astype(np.float32), (0, 0), 1.0)
    blur_large = cv2.GaussianBlur(gray.astype(np.float32), (0, 0), 4.0)
    sal = np.abs(blur_small - blur_large)
    sal = cv2.GaussianBlur(sal, (0, 0), 1.5)
    lo, hi = float(sal.min()), float(sal.max())
    if hi - lo < 1e-6:
        return np.zeros_like(sal, dtype=np.float32)
    return ((sal - lo) / (hi - lo)).astype(np.float32)


def local_contrast(gray: np.ndarray, ksize: int = 7) -> np.ndarray:
    mean = cv2.blur(gray.astype(np.float32), (ksize, ksize))
    mean_sq = cv2.blur((gray.astype(np.float32) ** 2), (ksize, ksize))
    var = np.maximum(mean_sq - mean * mean, 0.0)
    return np.sqrt(var)


def _to_gray(image: np.ndarray) -> np.ndarray:
    if image.ndim == 3:
        return cv2.cvtColor(image, cv2.COLOR_BGR2GRAY)
    return image


def _motion_mask(diff: np.ndarray, threshold: float = 18.0) -> np.ndarray:
    if diff.ndim == 3:
        diff = cv2.cvtColor(diff.astype(np.uint8), cv2.COLOR_BGR2GRAY)
    mask = (diff > threshold).astype(np.uint8) * 255
    kernel = np.ones((3, 3), np.uint8)
    mask = cv2.morphologyEx(mask, cv2.MORPH_OPEN, kernel)
    mask = cv2.morphologyEx(mask, cv2.MORPH_CLOSE, kernel)
    return mask


def _contours(mask: np.ndarray, min_area: float = 40.0) -> list[tuple[int, int, int, int, float]]:
    contours, _ = cv2.findContours(mask, cv2.RETR_EXTERNAL, cv2.CHAIN_APPROX_SIMPLE)
    boxes: list[tuple[int, int, int, int, float]] = []
    for contour in contours:
        area = float(cv2.contourArea(contour))
        if area < min_area:
            continue
        x, y, w, h = cv2.boundingRect(contour)
        boxes.append((x, y, w, h, area))
    return boxes


def _centroid(box: tuple[int, int, int, int, float]) -> tuple[float, float]:
    x, y, w, h, _ = box
    return (x + w / 2.0, y + h / 2.0)


def _emit_event(
    *,
    event_type: str,
    frame: OcularFrame | None,
    region: list[float],
    confidence: float,
    evidence: dict[str, Any],
    lane: ProcessingLane,
    state: RetinaState,
    correctness_label: str = "candidate",
    reflex_resolution: list[int] | None = None,
) -> RetinalEvent:
    if event_type not in RETINAL_EVENT_TYPES:
        raise ValidationError(f"unknown retinal event type: {event_type!r}")

    key = f"{event_type}:{frame.frame_id if frame else state.frame_index}"
    if lane is ProcessingLane.REFLEX and key in state.attentive_labels:
        # Reflex must not overwrite an attentive correctness label.
        correctness_label = state.attentive_labels[key]
        written_by = ProcessingLane.ATTENTIVE.value
    else:
        written_by = lane.value
        if lane is ProcessingLane.ATTENTIVE:
            state.attentive_labels[key] = correctness_label

    return RetinalEvent(
        id=f"evt-{event_type}-{state.frame_index}-{len(state.attentive_labels)}",
        event_type=event_type,
        stream_id=frame.stream_id if frame else "",
        frame_id=frame.frame_id if frame else "",
        timestamp=frame.timestamp if frame else 0.0,
        region=region,
        confidence=float(confidence),
        evidence=evidence,
        correctness_label=correctness_label,
        written_by_lane=written_by,
        reflex_resolution=list(reflex_resolution) if reflex_resolution else None,
        authority=AuthorityClass.SENSOR_DERIVED,
        lineage=default_lineage("ocular.retina", inputs=[frame.image_digest] if frame else []),
    ).seal()


def _match_tracks(
    state: RetinaState,
    boxes: list[tuple[int, int, int, int, float]],
    max_dist: float = 48.0,
) -> tuple[
    list[tuple[_Track, tuple[int, int, int, int, float]]],
    list[tuple[int, int, int, int, float]],
    list[_Track],
]:
    assigned: list[tuple[_Track, tuple[int, int, int, int, float]]] = []
    unmatched_boxes = list(boxes)
    unmatched_tracks: list[_Track] = []
    used: set[int] = set()

    for track in state.tracks.values():
        best_i = -1
        best_d = max_dist
        for i, box in enumerate(unmatched_boxes):
            if i in used:
                continue
            cx, cy = _centroid(box)
            d = float(np.hypot(cx - track.centroid[0], cy - track.centroid[1]))
            if d < best_d:
                best_d = d
                best_i = i
        if best_i >= 0:
            box = unmatched_boxes[best_i]
            used.add(best_i)
            track.centroid = _centroid(box)
            track.area = box[4]
            track.bbox = box[:4]
            track.last_seen = state.frame_index
            track.missing = 0
            assigned.append((track, box))
        else:
            track.missing += 1
            unmatched_tracks.append(track)

    new_boxes = [b for i, b in enumerate(unmatched_boxes) if i not in used]
    return assigned, new_boxes, unmatched_tracks


def process_frame(
    image: np.ndarray,
    *,
    frame: OcularFrame | None = None,
    state: RetinaState | None = None,
    pyramid_levels: int = 4,
    reflex_max_side: int = 160,
    expected_events: list[dict[str, Any]] | None = None,
) -> RetinaAnalysis:
    """Run reflex then attentive retina on one frame; update and return state events."""
    state = state or RetinaState()
    if expected_events is not None:
        state.expected_events = list(expected_events)

    full = image
    gray = _to_gray(full)
    mean_luma = float(np.mean(gray))

    # ---- reflex lane: reduced resolution, latency measured separately ----
    t0 = time.perf_counter()
    h, w = gray.shape[:2]
    if max(h, w) > reflex_max_side:
        scale = reflex_max_side / float(max(h, w))
        reflex_gray = cv2.resize(
            gray, (max(1, int(w * scale)), max(1, int(h * scale))), interpolation=cv2.INTER_AREA
        )
    else:
        reflex_gray = gray
    reflex_sal = coarse_saliency(reflex_gray)
    reflex_latency_ms = (time.perf_counter() - t0) * 1000.0
    reflex_res = [int(reflex_gray.shape[1]), int(reflex_gray.shape[0])]

    def emit(
        event_type: str,
        region: list[float],
        confidence: float,
        evidence: dict[str, Any],
        *,
        correctness_label: str = "observed",
    ) -> RetinalEvent:
        return _emit_event(
            event_type=event_type,
            frame=frame,
            region=region,
            confidence=confidence,
            evidence=evidence,
            lane=ProcessingLane.ATTENTIVE,
            state=state,
            correctness_label=correctness_label,
            reflex_resolution=reflex_res,
        )

    # ---- attentive lane: full-resolution pyramid, flow, events ----
    t1 = time.perf_counter()
    pyramid = build_pyramid(full, levels=pyramid_levels)
    contrast = local_contrast(gray)
    saliency = coarse_saliency(gray)

    diff: np.ndarray | None = None
    flow_result: FlowResult | None = None
    events: list[RetinalEvent] = []
    exposure_delta = 0.0

    if state.prev_gray is not None:
        prev = state.prev_gray
        if prev.shape != gray.shape:
            prev = cv2.resize(prev, (gray.shape[1], gray.shape[0]))
        diff = frame_difference(prev, gray)
        flow = dense_optical_flow(prev, gray)
        flow_result = separate_camera_object_motion(flow)

        if state.prev_mean_luma is not None:
            exposure_delta = mean_luma - state.prev_mean_luma

        # Global light change without bulk residual motion.
        if abs(exposure_delta) > 10.0 and (
            flow_result is None or flow_result.object_motion_score < 1.5
        ):
            events.append(
                emit(
                    "LIGHT_CHANGED",
                    [0.0, 0.0, float(w), float(h)],
                    min(1.0, abs(exposure_delta) / 40.0),
                    {"exposure_delta": exposure_delta},
                )
            )

        # Camera vs object motion.
        if flow_result is not None:
            cam = flow_result.camera_motion_score
            obj = flow_result.object_motion_score
            # Camera pan moves the whole field; an object moves a patch of it.
            frac = flow_result.moving_fraction
            residual_frac = flow_result.residual_fraction
            mean_mag = flow_result.mean_magnitude

            camera_event = (
                frac > CAMERA_COVERAGE_MIN
                and mean_mag > CAMERA_MEAN_MAGNITUDE_MIN
                and flow_result.global_matrix is not None
            )
            # Under a pan the residual is spread across the frame rather than
            # concentrated, so residual magnitude alone would fire on every
            # panned frame. Require the leftover motion to be *localised*.
            if camera_event:
                object_event = (
                    residual_frac < OBJECT_RESIDUAL_COVERAGE_MAX
                    and obj > OBJECT_MOTION_MIN
                )
            else:
                object_event = obj > OBJECT_MOTION_MIN and frac > 0.001

            if camera_event:
                events.append(
                    emit(
                        "CAMERA_MOVED",
                        [0.0, 0.0, float(w), float(h)],
                        min(1.0, cam / 8.0),
                        {
                            "camera_motion_score": cam,
                            "object_motion_score": obj,
                            "global_kind": flow_result.global_kind,
                        },
                    )
                )
            if object_event:
                mag = np.sqrt(flow_result.flow[..., 0] ** 2 + flow_result.flow[..., 1] ** 2)
                ys, xs = np.where(mag > max(1.0, obj * 0.5))
                if len(xs) > 0:
                    region = [
                        float(xs.min()),
                        float(ys.min()),
                        float(xs.max() - xs.min() + 1),
                        float(ys.max() - ys.min() + 1),
                    ]
                else:
                    region = [0.0, 0.0, float(w), float(h)]
                events.append(
                    emit(
                        "OBJECT_MOVED",
                        region,
                        min(1.0, obj / 6.0),
                        {"object_motion_score": obj, "camera_motion_score": cam},
                    )
                )

        mask = _motion_mask(diff)
        boxes = _contours(mask, min_area=max(40.0, 0.0004 * w * h))
        # Under pure camera pan the whole frame differences; skip blob tracking
        # so edge noise is not born as OBJECT_ENTERED/LEFT chatter.
        if camera_event:
            boxes = []
        assigned, new_boxes, lost = _match_tracks(state, boxes)

        for track, box in assigned:
            if track.occluded and box[4] > track.area * 0.5:
                track.occluded = False
                events.append(
                    emit(
                        "OBJECT_REAPPEARED",
                        [float(v) for v in box[:4]],
                        0.75,
                        {"track_id": track.track_id},
                    )
                )
            if box[4] < track.area * 0.45 and box[4] > 20:
                track.occluded = True
                events.append(
                    emit(
                        "OBJECT_OCCLUDED",
                        [float(v) for v in box[:4]],
                        0.7,
                        {"track_id": track.track_id, "area": box[4]},
                    )
                )

        for box in new_boxes:
            tid = f"t{state.next_track_id}"
            state.next_track_id += 1
            track = _Track(
                track_id=tid,
                centroid=_centroid(box),
                area=box[4],
                bbox=box[:4],
                last_seen=state.frame_index,
            )
            state.tracks[tid] = track
            reappeared = False
            for lost_track in list(state.tracks.values()):
                if lost_track.track_id == tid:
                    continue
                if lost_track.missing >= 2:
                    d = float(
                        np.hypot(
                            track.centroid[0] - lost_track.centroid[0],
                            track.centroid[1] - lost_track.centroid[1],
                        )
                    )
                    if d < 60:
                        reappeared = True
                        lost_track.missing = 0
                        lost_track.occluded = False
                        events.append(
                            emit(
                                "OBJECT_REAPPEARED",
                                [float(v) for v in box[:4]],
                                0.8,
                                {"track_id": lost_track.track_id},
                            )
                        )
                        break
            if not reappeared:
                events.append(
                    emit(
                        "OBJECT_ENTERED",
                        [float(v) for v in box[:4]],
                        0.85,
                        {"track_id": tid, "area": box[4]},
                    )
                )
                events.append(
                    emit(
                        "NEW_UNKNOWN_REGION",
                        [float(v) for v in box[:4]],
                        0.7,
                        {"track_id": tid},
                    )
                )

        for track in lost:
            if track.missing == 2:
                events.append(
                    emit(
                        "OBJECT_LEFT",
                        [float(v) for v in track.bbox],
                        0.8,
                        {"track_id": track.track_id},
                    )
                )
            if track.missing == 1 and track.area > 80:
                track.occluded = True
                events.append(
                    emit(
                        "OBJECT_OCCLUDED",
                        [float(v) for v in track.bbox],
                        0.65,
                        {"track_id": track.track_id, "stage": "missing_1"},
                    )
                )

        if diff is not None:
            local = diff.astype(np.float32)
            if local.ndim == 3:
                local = cv2.cvtColor(local.astype(np.uint8), cv2.COLOR_BGR2GRAY).astype(
                    np.float32
                )
            edges_prev = cv2.Canny(prev.astype(np.uint8), 50, 150)
            edges_curr = cv2.Canny(gray.astype(np.uint8), 50, 150)
            edge_delta = float(
                np.mean(np.abs(edges_prev.astype(np.float32) - edges_curr.astype(np.float32)))
            )
            if edge_delta > 6.0 and (flow_result is None or flow_result.mean_magnitude < 2.0):
                events.append(
                    emit(
                        "TEXT_CHANGED",
                        [0.0, 0.0, float(w), float(h)],
                        min(1.0, edge_delta / 20.0),
                        {"edge_delta": edge_delta},
                    )
                )
            surface_score = float(np.mean(local))
            # Surface: distributed residual (not a compact blob track).
            compact_blob = any(b[4] < 0.15 * w * h for b in boxes) if boxes else False
            surface_hit = (
                surface_score > 4.0
                and abs(exposure_delta) < 12.0
                and edge_delta < 8.0
                and not compact_blob
            ) or (
                surface_score > 8.0
                and abs(exposure_delta) < 8.0
                and edge_delta < 6.0
                and (flow_result is None or flow_result.object_motion_score < 1.5)
            )
            if surface_hit:
                events.append(
                    emit(
                        "SURFACE_CHANGED",
                        [0.0, 0.0, float(w), float(h)],
                        min(1.0, surface_score / 25.0),
                        {"surface_score": surface_score},
                    )
                )

    for expected in state.expected_events:
        eid = str(expected.get("id", ""))
        etype = str(expected.get("event_type", ""))
        at_frame = int(expected.get("frame_index", -1))
        if eid in state.fired_expected:
            continue
        if at_frame != state.frame_index:
            continue
        found = any(e.event_type == etype for e in events)
        if found:
            state.fired_expected.add(eid)
        else:
            events.append(
                emit(
                    "EXPECTED_EVENT_MISSING",
                    list(expected.get("region", [0, 0, 0, 0])),
                    1.0,
                    {"expected": expected},
                )
            )
            state.fired_expected.add(eid)

    attentive_latency_ms = (time.perf_counter() - t1) * 1000.0

    state.prev_gray = gray.copy()
    state.prev_bgr = full.copy() if full.ndim == 3 else None
    state.prev_mean_luma = mean_luma
    state.frame_index += 1

    _ = float(np.max(reflex_sal))

    return RetinaAnalysis(
        frame_id=frame.frame_id if frame else f"raw-{state.frame_index}",
        timestamp=frame.timestamp if frame else 0.0,
        pyramid=pyramid,
        difference=diff,
        flow=flow_result,
        saliency=saliency,
        local_contrast=contrast,
        exposure_delta=exposure_delta,
        events=events,
        reflex_latency_ms=reflex_latency_ms,
        attentive_latency_ms=attentive_latency_ms,
        lane_used=ProcessingLane.ATTENTIVE,
    )


def assert_reflex_cannot_relabel(event: RetinalEvent, new_label: str) -> RetinalEvent:
    """Enforce the law: reflex lane never changes a correctness label."""
    if (
        event.written_by_lane == ProcessingLane.ATTENTIVE.value
        and new_label != event.correctness_label
    ):
        raise ValidationError(
            "reflex lane may not change correctness_label "
            f"from {event.correctness_label!r} to {new_label!r}"
        )
    return event


def apply_reflex_resolution(
    event: RetinalEvent, resolution: list[int], *, lane: ProcessingLane
) -> RetinalEvent:
    """Reflex may attach reduced resolution; it may not rewrite correctness."""
    if lane is ProcessingLane.REFLEX:
        # Only resolution metadata; correctness_label is untouched.
        object.__setattr__(event, "reflex_resolution", list(resolution))
        return event
    return event
