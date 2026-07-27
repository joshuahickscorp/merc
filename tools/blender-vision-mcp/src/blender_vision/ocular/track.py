"""Visual tracking and object permanence for the Ocular loop.

Association uses IoU + appearance histogram + constant-velocity Kalman
prediction. Occluded tracks stay alive with growing identity uncertainty and
an explicit occluder reference. Re-identification requires appearance evidence
above a stated threshold; lost/departed tracks use a stricter threshold so a
similar replacement is not silently claimed as the original.
"""

from __future__ import annotations

from collections.abc import Sequence
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import numpy as np
from numpy.typing import NDArray

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.segment import (
    SEGMENT_AUTHORITY_CEILING,
    SegmentInstance,
    histogram_correlation,
)
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units
from blender_vision.v2.records import Lineage, V2Record

ArrayF64 = NDArray[np.float64]


class TrackState(StrEnum):
    ACTIVE = "ACTIVE"
    OCCLUDED = "OCCLUDED"
    LOST = "LOST"
    REAPPEARED = "REAPPEARED"


class TrackTargetKind(StrEnum):
    MASK = "mask"
    POINT = "point"
    OBJECT = "object"
    PART = "part"
    CAMERA = "camera"


# Declared up front: association and re-id thresholds (sensitivity-relevant).
IOU_WEIGHT = 0.45
APPEARANCE_WEIGHT = 0.35
POSITION_WEIGHT = 0.20
ASSOCIATION_THRESHOLD = 0.28
REID_THRESHOLD_OCCLUDED = 0.70
REID_THRESHOLD_LOST = 0.92
MAX_OCCLUDED_FRAMES = 45
MAX_LOST_FRAMES = 90
# Identity uncertainty grows by this amount each occluded/lost frame.
UNCERTAINTY_GROWTH_PER_FRAME = 0.04
UNCERTAINTY_FLOOR = 0.05
UNCERTAINTY_CEILING = 1.0
PROCESS_NOISE = 4.0
MEASUREMENT_NOISE = 2.0


@dataclass(slots=True)
class Detection:
    """One observation to associate. Built from masks, points, or segments."""

    detection_id: str
    kind: TrackTargetKind
    bbox_xywh: tuple[float, float, float, float]
    centroid_xy: tuple[float, float]
    appearance_hist: list[float]
    area_px: float = 0.0
    frame_index: int = 0
    conf: float = 1.0
    meta: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_segment(
        cls,
        instance: SegmentInstance,
        *,
        frame_index: int,
        kind: TrackTargetKind = TrackTargetKind.OBJECT,
    ) -> Detection:
        x, y, w, h = instance.bbox_xywh
        return cls(
            detection_id=instance.segment_id,
            kind=kind,
            bbox_xywh=(float(x), float(y), float(w), float(h)),
            centroid_xy=instance.centroid_xy,
            appearance_hist=list(instance.appearance_hist),
            area_px=float(instance.area_px),
            frame_index=frame_index,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "detection_id": self.detection_id,
            "kind": self.kind.value,
            "bbox_xywh": list(self.bbox_xywh),
            "centroid_xy": list(self.centroid_xy),
            "appearance_hist": list(self.appearance_hist),
            "area_px": self.area_px,
            "frame_index": self.frame_index,
            "conf": self.conf,
            "meta": dict(self.meta),
        }


@dataclass(slots=True)
class KalmanCV2:
    """Constant-velocity Kalman filter on (x, y, vx, vy). Explicit, no SciPy dependency."""

    x: float
    y: float
    vx: float = 0.0
    vy: float = 0.0
    p_pos: float = 25.0
    p_vel: float = 50.0

    def predict(self, dt: float = 1.0) -> tuple[float, float]:
        self.x = self.x + self.vx * dt
        self.y = self.y + self.vy * dt
        # Grow positional covariance while predicting.
        self.p_pos = self.p_pos + self.p_vel * dt * dt + PROCESS_NOISE
        self.p_vel = self.p_vel + PROCESS_NOISE * 0.5
        return (self.x, self.y)

    def update(self, mx: float, my: float) -> None:
        # Scalar Kalman gain on position; velocity from residual.
        k = self.p_pos / (self.p_pos + MEASUREMENT_NOISE)
        dx = mx - self.x
        dy = my - self.y
        self.x = self.x + k * dx
        self.y = self.y + k * dy
        # Velocity update toward residual (dt=1 frame).
        self.vx = (1.0 - 0.4) * self.vx + 0.4 * dx
        self.vy = (1.0 - 0.4) * self.vy + 0.4 * dy
        self.p_pos = (1.0 - k) * self.p_pos
        self.p_vel = max(1.0, self.p_vel * 0.9)

    def predicted_xy(self) -> tuple[float, float]:
        return (self.x, self.y)

    def copy(self) -> KalmanCV2:
        return KalmanCV2(
            x=self.x,
            y=self.y,
            vx=self.vx,
            vy=self.vy,
            p_pos=self.p_pos,
            p_vel=self.p_vel,
        )

    def to_dict(self) -> dict[str, Any]:
        return {
            "x": self.x,
            "y": self.y,
            "vx": self.vx,
            "vy": self.vy,
            "p_pos": self.p_pos,
            "p_vel": self.p_vel,
        }


def bbox_iou(
    a: tuple[float, float, float, float], b: tuple[float, float, float, float]
) -> float:
    ax, ay, aw, ah = a
    bx, by, bw, bh = b
    ax2, ay2 = ax + aw, ay + ah
    bx2, by2 = bx + bw, by + bh
    ix1, iy1 = max(ax, bx), max(ay, by)
    ix2, iy2 = min(ax2, bx2), min(ay2, by2)
    iw, ih = max(0.0, ix2 - ix1), max(0.0, iy2 - iy1)
    inter = iw * ih
    union = aw * ah + bw * bh - inter
    return float(inter / union) if union > 0 else 0.0


def _position_score(
    predicted: tuple[float, float],
    observed: tuple[float, float],
    *,
    scale_px: float,
) -> float:
    dx = predicted[0] - observed[0]
    dy = predicted[1] - observed[1]
    dist = float(np.hypot(dx, dy))
    # Soft score: 1 at 0 distance, ~0 beyond ~3*scale.
    denom = max(scale_px, 1.0) * 3.0
    return float(np.exp(-0.5 * (dist / denom) ** 2))


def association_score(
    track: VisualTrack,
    detection: Detection,
    *,
    predicted_xy: tuple[float, float],
) -> float:
    iou = bbox_iou(track.bbox_xywh, detection.bbox_xywh)
    app = histogram_correlation(track.appearance_hist, detection.appearance_hist)
    scale = max(track.bbox_xywh[2], track.bbox_xywh[3], 8.0)
    pos = _position_score(predicted_xy, detection.centroid_xy, scale_px=scale)
    return IOU_WEIGHT * iou + APPEARANCE_WEIGHT * app + POSITION_WEIGHT * pos


@dataclass(slots=True, kw_only=True)
class VisualTrack(V2Record):
    """One identity across time. Identity uncertainty is always reported."""

    RECORD_KIND = "ocular.visual-track"

    track_id: str = ""
    kind: TrackTargetKind = TrackTargetKind.OBJECT
    state: TrackState = TrackState.ACTIVE
    frame_index: int = 0
    first_frame: int = 0
    last_seen_frame: int = 0
    frames_since_seen: int = 0
    bbox_xywh: tuple[float, float, float, float] = (0.0, 0.0, 0.0, 0.0)
    centroid_xy: tuple[float, float] = (0.0, 0.0)
    predicted_xy: tuple[float, float] = (0.0, 0.0)
    appearance_hist: list[float] = field(default_factory=list)
    identity_uncertainty: float = UNCERTAINTY_FLOOR
    association_score: float = 0.0
    occluder_track_id: str | None = None
    reappearance_prediction_xy: tuple[float, float] | None = None
    kalman: dict[str, float] = field(default_factory=dict)
    hit_streak: int = 0
    age: int = 0
    ground_truth_id: str | None = None

    def __post_init__(self) -> None:
        if not self.track_id:
            self.track_id = self.id
        if not (0.0 <= self.identity_uncertainty <= 1.0):
            raise ValidationError("identity_uncertainty must be in [0, 1]")

    def to_dict(self) -> dict[str, Any]:
        value = V2Record.payload(self)
        value["kind"] = self.kind.value if isinstance(self.kind, StrEnum) else self.kind
        value["state"] = self.state.value if isinstance(self.state, StrEnum) else self.state
        value["bbox_xywh"] = list(self.bbox_xywh)
        value["centroid_xy"] = list(self.centroid_xy)
        value["predicted_xy"] = list(self.predicted_xy)
        if self.reappearance_prediction_xy is not None:
            value["reappearance_prediction_xy"] = list(self.reappearance_prediction_xy)
        value["digest"] = self.digest or self.compute_digest()
        return value


@dataclass(slots=True)
class ReidentifyDecision:
    """Explicit re-id outcome. Refusal is a first-class result."""

    matched: bool
    track_id: str | None
    score: float
    threshold: float
    reason: str
    appearance_score: float
    identity_uncertainty: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "matched": self.matched,
            "track_id": self.track_id,
            "score": self.score,
            "threshold": self.threshold,
            "reason": self.reason,
            "appearance_score": self.appearance_score,
            "identity_uncertainty": self.identity_uncertainty,
        }


@dataclass(slots=True)
class TrackerState:
    """Mutable multi-object tracker. Not a V2 record; holds live Kalman state."""

    tracks: list[VisualTrack] = field(default_factory=list)
    filters: dict[str, KalmanCV2] = field(default_factory=dict)
    next_id: int = 1
    frame_index: int = -1

    def live_tracks(self) -> list[VisualTrack]:
        return [
            t
            for t in self.tracks
            if t.state is not TrackState.LOST or t.frames_since_seen < MAX_LOST_FRAMES
        ]


def _new_track_id(state: TrackerState) -> str:
    tid = f"trk-{state.next_id:04d}"
    state.next_id += 1
    return tid


def _grow_uncertainty(current: float, frames: int) -> float:
    grown = UNCERTAINTY_FLOOR + frames * UNCERTAINTY_GROWTH_PER_FRAME
    return float(min(UNCERTAINTY_CEILING, max(current, grown)))


def _detect_occluder(
    track: VisualTrack,
    others: Sequence[VisualTrack],
    predicted_xy: tuple[float, float],
) -> str | None:
    """Heuristic: an ACTIVE track whose bbox covers the predicted centre is the occluder."""
    px, py = predicted_xy
    best: str | None = None
    best_area = 0.0
    for other in others:
        if other.track_id == track.track_id:
            continue
        if other.state not in {TrackState.ACTIVE, TrackState.REAPPEARED}:
            continue
        x, y, w, h = other.bbox_xywh
        if x <= px <= x + w and y <= py <= y + h:
            area = w * h
            if area > best_area:
                best_area = area
                best = other.track_id
    return best


def track(
    detections: Sequence[Detection],
    state: TrackerState | None = None,
    *,
    frame_index: int | None = None,
) -> TrackerState:
    """Associate detections to tracks for one frame; update permanence state.

    Unmatched tracks transition ACTIVE→OCCLUDED (with occluder if found) then
    LOST after MAX_OCCLUDED_FRAMES. Identity uncertainty grows monotonically
    while unseen. New detections spawn new tracks.
    """
    tracker = state if state is not None else TrackerState()
    fi = tracker.frame_index + 1 if frame_index is None else frame_index
    tracker.frame_index = fi

    # Predict all filters.
    predicted: dict[str, tuple[float, float]] = {}
    for tid, filt in tracker.filters.items():
        predicted[tid] = filt.predict(1.0)

    live_states = {
        TrackState.ACTIVE,
        TrackState.OCCLUDED,
        TrackState.REAPPEARED,
        TrackState.LOST,
    }
    active_pool = [
        t
        for t in tracker.tracks
        if t.state in live_states and t.frames_since_seen < MAX_LOST_FRAMES
    ]

    # Greedy association by score (stable for small N).
    pairs: list[tuple[float, VisualTrack, Detection]] = []
    for trk in active_pool:
        pred = predicted.get(trk.track_id, trk.centroid_xy)
        for det in detections:
            score = association_score(trk, det, predicted_xy=pred)
            if score >= ASSOCIATION_THRESHOLD:
                pairs.append((score, trk, det))
    pairs.sort(key=lambda item: item[0], reverse=True)

    assigned_tracks: set[str] = set()
    assigned_dets: set[str] = set()
    matches: list[tuple[VisualTrack, Detection, float]] = []
    for score, trk, det in pairs:
        if trk.track_id in assigned_tracks or det.detection_id in assigned_dets:
            continue
        # Lost tracks must clear the stricter re-id appearance bar.
        if trk.state is TrackState.LOST:
            app = histogram_correlation(trk.appearance_hist, det.appearance_hist)
            if app < REID_THRESHOLD_LOST:
                continue
        elif trk.state is TrackState.OCCLUDED:
            app = histogram_correlation(trk.appearance_hist, det.appearance_hist)
            if app < REID_THRESHOLD_OCCLUDED:
                continue
        assigned_tracks.add(trk.track_id)
        assigned_dets.add(det.detection_id)
        matches.append((trk, det, score))

    updated: list[VisualTrack] = []
    matched_ids: set[str] = set()

    for trk, det, score in matches:
        matched_ids.add(trk.track_id)
        filt = tracker.filters[trk.track_id]
        filt.update(det.centroid_xy[0], det.centroid_xy[1])
        prev_state = trk.state
        if prev_state in {TrackState.OCCLUDED, TrackState.LOST}:
            new_state = TrackState.REAPPEARED
            # Reappearance reduces uncertainty but does not zero it.
            identity_u = max(UNCERTAINTY_FLOOR, trk.identity_uncertainty * 0.5)
        else:
            new_state = TrackState.ACTIVE
            identity_u = max(UNCERTAINTY_FLOOR, trk.identity_uncertainty * 0.7)

        # EMA appearance update while visible.
        old = np.asarray(trk.appearance_hist, dtype=np.float64)
        new = np.asarray(det.appearance_hist, dtype=np.float64)
        if old.size == new.size and old.size > 0:
            blended = 0.7 * old + 0.3 * new
            blended = blended / (blended.sum() + 1e-12)
            hist = [float(v) for v in blended]
        else:
            hist = list(det.appearance_hist)

        sealed = VisualTrack(
            id=trk.id,
            track_id=trk.track_id,
            kind=det.kind,
            state=new_state,
            frame_index=fi,
            first_frame=trk.first_frame,
            last_seen_frame=fi,
            frames_since_seen=0,
            bbox_xywh=det.bbox_xywh,
            centroid_xy=det.centroid_xy,
            predicted_xy=filt.predicted_xy(),
            appearance_hist=hist,
            identity_uncertainty=float(identity_u),
            association_score=float(score),
            occluder_track_id=None,
            reappearance_prediction_xy=None,
            kalman=filt.to_dict(),
            hit_streak=trk.hit_streak + 1,
            age=trk.age + 1,
            ground_truth_id=trk.ground_truth_id,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=Lineage(
                operation="ocular.track.associate",
                inputs=[trk.id, det.detection_id],
                input_authorities=[],
                parameters={
                    "score": score,
                    "prev_state": prev_state.value,
                    "source_authorities": [
                        SEGMENT_AUTHORITY_CEILING.value,
                        AuthorityClass.SENSOR_DERIVED.value,
                    ],
                    "thresholds": {
                        "association": ASSOCIATION_THRESHOLD,
                        "reid_occluded": REID_THRESHOLD_OCCLUDED,
                        "reid_lost": REID_THRESHOLD_LOST,
                    },
                },
            ),
            uncertainty=Uncertainty(
                kind="identity",
                sigma=float(identity_u),
                units=Units.UNITLESS,
                basis="occlusion-growth+appearance",
                samples=trk.age + 1,
            ),
        ).seal()
        updated.append(sealed)

    # Unmatched existing tracks → occluded / lost with growing uncertainty.
    for trk in active_pool:
        if trk.track_id in matched_ids:
            continue
        filt = tracker.filters.get(trk.track_id)
        if filt is None:
            filt = KalmanCV2(x=trk.centroid_xy[0], y=trk.centroid_xy[1])
            tracker.filters[trk.track_id] = filt
        pred = predicted.get(trk.track_id, filt.predicted_xy())
        frames_unseen = trk.frames_since_seen + 1
        identity_u = _grow_uncertainty(trk.identity_uncertainty, frames_unseen)

        if frames_unseen > MAX_OCCLUDED_FRAMES:
            new_state = TrackState.LOST
            occluder = None
        else:
            # Prefer OCCLUDED over LOST while within the permanence window.
            others = [t for t in updated if t.track_id != trk.track_id]
            # Also consider still-active previous tracks for occluder search.
            others = others + [
                t for t in active_pool if t.track_id != trk.track_id and t.track_id in matched_ids
            ]
            occluder = _detect_occluder(trk, others if others else active_pool, pred)
            if frames_unseen > MAX_OCCLUDED_FRAMES:
                new_state = TrackState.LOST
            else:
                new_state = TrackState.OCCLUDED

        sealed = VisualTrack(
            id=trk.id,
            track_id=trk.track_id,
            kind=trk.kind,
            state=new_state,
            frame_index=fi,
            first_frame=trk.first_frame,
            last_seen_frame=trk.last_seen_frame,
            frames_since_seen=frames_unseen,
            bbox_xywh=trk.bbox_xywh,
            centroid_xy=trk.centroid_xy,
            predicted_xy=pred,
            appearance_hist=list(trk.appearance_hist),
            identity_uncertainty=float(identity_u),
            association_score=0.0,
            occluder_track_id=occluder,
            reappearance_prediction_xy=pred,
            kalman=filt.to_dict(),
            hit_streak=0,
            age=trk.age + 1,
            ground_truth_id=trk.ground_truth_id,
            authority=(
                AuthorityClass.INFERRED
                if new_state in {TrackState.OCCLUDED, TrackState.LOST}
                else AuthorityClass.SENSOR_DERIVED
            ),
            lineage=Lineage(
                operation="ocular.track.permanence",
                inputs=[trk.id],
                input_authorities=[],
                parameters={
                    "frames_since_seen": frames_unseen,
                    "identity_uncertainty": identity_u,
                    "occluder_track_id": occluder,
                    "uncertainty_growth_per_frame": UNCERTAINTY_GROWTH_PER_FRAME,
                    "source_authorities": [AuthorityClass.SENSOR_DERIVED.value],
                },
                limitations=["position predicted; identity not observed while occluded/lost"],
            ),
            uncertainty=Uncertainty(
                kind="identity",
                sigma=float(identity_u),
                units=Units.UNITLESS,
                basis="monotonic-occlusion-growth",
                samples=frames_unseen,
            ),
            notes=[
                f"state={new_state.value}",
                f"identity_uncertainty={identity_u:.4f}",
            ],
        ).seal()
        updated.append(sealed)

    # New detections → new tracks.
    for det in detections:
        if det.detection_id in assigned_dets:
            continue
        tid = _new_track_id(tracker)
        filt = KalmanCV2(x=det.centroid_xy[0], y=det.centroid_xy[1])
        tracker.filters[tid] = filt
        sealed = VisualTrack(
            id=tid,
            track_id=tid,
            kind=det.kind,
            state=TrackState.ACTIVE,
            frame_index=fi,
            first_frame=fi,
            last_seen_frame=fi,
            frames_since_seen=0,
            bbox_xywh=det.bbox_xywh,
            centroid_xy=det.centroid_xy,
            predicted_xy=det.centroid_xy,
            appearance_hist=list(det.appearance_hist),
            identity_uncertainty=UNCERTAINTY_FLOOR,
            association_score=1.0,
            kalman=filt.to_dict(),
            hit_streak=1,
            age=1,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=Lineage(
                operation="ocular.track.spawn",
                inputs=[det.detection_id],
                input_authorities=[],
                parameters={
                    "frame_index": fi,
                    "source_authorities": [AuthorityClass.SENSOR_DERIVED.value],
                },
            ),
            uncertainty=Uncertainty(
                kind="identity",
                sigma=UNCERTAINTY_FLOOR,
                units=Units.UNITLESS,
                basis="new-track",
                samples=1,
            ),
        ).seal()
        updated.append(sealed)

    # Keep long-LOST tracks that are still within the lost window already handled;
    # drop those that aged out entirely.
    tracker.tracks = [
        t
        for t in updated
        if not (t.state is TrackState.LOST and t.frames_since_seen >= MAX_LOST_FRAMES)
    ]
    return tracker


def reidentify(
    detection: Detection,
    tracks: Sequence[VisualTrack],
    *,
    min_score: float | None = None,
    require_lost_or_occluded: bool = True,
) -> ReidentifyDecision:
    """Attempt to re-attach a detection to an existing track by appearance+position.

    Refuses below-threshold matches. Lost tracks use REID_THRESHOLD_LOST unless
    the caller overrides min_score. Identity uncertainty of the candidate is
    always reported.
    """
    if not tracks:
        return ReidentifyDecision(
            matched=False,
            track_id=None,
            score=0.0,
            threshold=min_score if min_score is not None else REID_THRESHOLD_OCCLUDED,
            reason="no-candidate-tracks",
            appearance_score=0.0,
        )

    best: VisualTrack | None = None
    best_score = -1.0
    best_app = 0.0
    best_threshold = REID_THRESHOLD_OCCLUDED

    for trk in tracks:
        eligible = {
            TrackState.OCCLUDED,
            TrackState.LOST,
            TrackState.REAPPEARED,
        }
        # ACTIVE is excluded when require_lost_or_occluded (default re-id pool).
        if require_lost_or_occluded and trk.state not in eligible:
            continue
        if trk.state is TrackState.LOST:
            threshold = REID_THRESHOLD_LOST if min_score is None else min_score
        else:
            threshold = REID_THRESHOLD_OCCLUDED if min_score is None else min_score

        app = histogram_correlation(trk.appearance_hist, detection.appearance_hist)
        pred = trk.predicted_xy if trk.predicted_xy != (0.0, 0.0) else trk.centroid_xy
        scale = max(trk.bbox_xywh[2], trk.bbox_xywh[3], 8.0)
        pos = _position_score(pred, detection.centroid_xy, scale_px=scale)
        iou = bbox_iou(trk.bbox_xywh, detection.bbox_xywh)
        # Re-id emphasises appearance over IoU (bbox may have drifted).
        score = 0.55 * app + 0.25 * pos + 0.20 * iou
        if score > best_score:
            best_score = score
            best = trk
            best_app = app
            best_threshold = threshold

    if best is None:
        return ReidentifyDecision(
            matched=False,
            track_id=None,
            score=0.0,
            threshold=best_threshold,
            reason="no-eligible-tracks",
            appearance_score=0.0,
        )

    # Hard appearance gate: even if composite score is high, appearance must clear threshold.
    if best_app < best_threshold or best_score < best_threshold * 0.85:
        return ReidentifyDecision(
            matched=False,
            track_id=best.track_id,
            score=float(best_score),
            threshold=best_threshold,
            reason=(
                f"below-threshold appearance={best_app:.4f} score={best_score:.4f} "
                f"required={best_threshold:.4f}"
            ),
            appearance_score=float(best_app),
            identity_uncertainty=best.identity_uncertainty,
        )

    return ReidentifyDecision(
        matched=True,
        track_id=best.track_id,
        score=float(best_score),
        threshold=best_threshold,
        reason=f"matched state={best.state.value}",
        appearance_score=float(best_app),
        identity_uncertainty=best.identity_uncertainty,
    )


def track_metrics(
    frame_assignments: list[dict[str, str | None]],
    ground_truth_ids: Sequence[str],
) -> dict[str, Any]:
    """Compute ID switches, MOTA-style accuracy, fragmentation, re-id stats.

    ``frame_assignments`` is a list of {gt_id: track_id_or_None} per frame.
    """
    id_switches = 0
    fragments = 0
    matches = 0
    misses = 0
    false_positives = 0
    prev_map: dict[str, str] = {}
    gt_to_tracks: dict[str, list[str]] = {gid: [] for gid in ground_truth_ids}
    confusion: dict[str, dict[str, int]] = {
        a: {b: 0 for b in ground_truth_ids} for a in ground_truth_ids
    }
    # For re-id precision/recall we count LOST→match events when provided via meta keys.
    reid_tp = 0
    reid_fp = 0
    reid_fn = 0

    for frame in frame_assignments:
        used_tracks: set[str] = set()
        for gid in ground_truth_ids:
            tid = frame.get(gid)
            if tid is None:
                misses += 1
                if gid in prev_map:
                    # Break in continuity.
                    fragments += 1
                continue
            tid_s = str(tid)
            matches += 1
            used_tracks.add(tid_s)
            if gid in prev_map and prev_map[gid] != tid_s:
                id_switches += 1
            prev_map[gid] = tid_s
            if not gt_to_tracks[gid] or gt_to_tracks[gid][-1] != tid_s:
                gt_to_tracks[gid].append(tid_s)
            # Confusion: which GT this track is currently claiming most — here we
            # count (gt, predicted_gt_of_same_track) when multiple GTs share tracks.
            for other_gid, other_tid in frame.items():
                if other_gid not in ground_truth_ids:
                    continue
                if other_tid == tid_s and other_gid != gid:
                    confusion[gid][str(other_gid)] += 1
            confusion[gid][gid] += 1

        # FP: tracks assigned that don't map to any GT in this simplified view
        # are counted externally; keep structural field.
        false_positives += int(frame.get("_false_positives", 0) or 0)
        reid_tp += int(frame.get("_reid_tp", 0) or 0)
        reid_fp += int(frame.get("_reid_fp", 0) or 0)
        reid_fn += int(frame.get("_reid_fn", 0) or 0)

    total = matches + misses + false_positives
    mota = 1.0 - (misses + false_positives + id_switches) / total if total else 0.0
    fragmentation = {
        gid: max(0, len(tids) - 1) for gid, tids in gt_to_tracks.items()
    }
    reid_precision = reid_tp / (reid_tp + reid_fp) if (reid_tp + reid_fp) else 1.0
    reid_recall = reid_tp / (reid_tp + reid_fn) if (reid_tp + reid_fn) else 1.0

    return {
        "id_switches": id_switches,
        "mota": mota,
        "matches": matches,
        "misses": misses,
        "false_positives": false_positives,
        "fragmentation": fragmentation,
        "track_fragmentation_total": sum(fragmentation.values()),
        "confusion": confusion,
        "reid_precision": reid_precision,
        "reid_recall": reid_recall,
        "reid_tp": reid_tp,
        "reid_fp": reid_fp,
        "reid_fn": reid_fn,
        "gt_to_tracks": gt_to_tracks,
    }


def occlusion_survival_rate(
    track_history: Sequence[VisualTrack],
    *,
    expected_track_id: str,
    occlusion_frames: Sequence[int],
) -> float:
    """Fraction of occlusion frames where the identity is still known."""
    if not occlusion_frames:
        return 1.0
    by_frame = {t.frame_index: t for t in track_history if t.track_id == expected_track_id}
    survived = 0
    for fi in occlusion_frames:
        trk = by_frame.get(fi)
        if trk is not None and trk.state in {
            TrackState.OCCLUDED,
            TrackState.REAPPEARED,
            TrackState.ACTIVE,
        }:
            survived += 1
    return survived / len(occlusion_frames)
