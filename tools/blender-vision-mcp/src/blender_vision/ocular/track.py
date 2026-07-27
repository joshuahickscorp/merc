"""Visual tracking and object permanence for the Ocular loop.

Association cost combines IoU, appearance distance, constant-velocity
predicted-position distance, motion consistency and scale continuity.
Assignment is solved with the Hungarian algorithm (not greedy) so crossing
paths do not produce ID switches from local greed.

Occluded tracks stay alive with growing identity uncertainty and an explicit
occluder reference. Re-identification requires appearance evidence **and**
kinematic plausibility; either alone is insufficient. An unknown entrant
spawns a new identity.

Ground truth never lives on runtime track structures — the sealed evaluator
scores predicted tracks against GT after the fact.
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

try:
    from scipy.optimize import linear_sum_assignment as _linear_sum_assignment
except ImportError:  # pragma: no cover - scipy is present in the project env
    _linear_sum_assignment = None  # type: ignore[assignment]


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


# Declared association weights (must sum to 1.0).
IOU_WEIGHT = 0.30
APPEARANCE_WEIGHT = 0.30
POSITION_WEIGHT = 0.20
MOTION_WEIGHT = 0.10
SCALE_WEIGHT = 0.10

ASSOCIATION_THRESHOLD = 0.28
REID_THRESHOLD_OCCLUDED = 0.70
REID_THRESHOLD_LOST = 0.92
# Re-id also requires kinematic plausibility above this soft score.
REID_KINEMATIC_THRESHOLD = 0.25
# Hard spatial gate: never associate a detection farther than this many bbox
# scales from the predicted centre. Prevents teleport re-id onto new objects.
MAX_ASSOCIATION_DIST_SCALES = 2.5
MAX_OCCLUDED_FRAMES = 45
MAX_LOST_FRAMES = 90
# Identity uncertainty grows by this amount each occluded/lost frame.
UNCERTAINTY_GROWTH_PER_FRAME = 0.04
UNCERTAINTY_FLOOR = 0.05
UNCERTAINTY_CEILING = 1.0
PROCESS_NOISE = 4.0
MEASUREMENT_NOISE = 2.0
# Large finite cost for infeasible pairs (Hungarian needs a dense matrix).
_INFEASIBLE_COST = 1.0e6


@dataclass(slots=True)
class Detection:
    """One observation to associate. Built from masks, points, or segments.

    ``appearance_embedding`` is the preferred multi-block descriptor when the
    perception detector is used. ``appearance_hist`` remains for colour-only
    callers and unit tests; association falls back to it when the embedding is
    empty. Ground-truth identity must never appear on this structure.
    """

    detection_id: str
    kind: TrackTargetKind
    bbox_xywh: tuple[float, float, float, float]
    centroid_xy: tuple[float, float]
    appearance_hist: list[float]
    area_px: float = 0.0
    frame_index: int = 0
    conf: float = 1.0
    appearance_embedding: list[float] = field(default_factory=list)
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
            "appearance_embedding": list(self.appearance_embedding),
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

    def velocity(self) -> tuple[float, float]:
        return (self.vx, self.vy)

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


def _embedding_cosine_sim(a: Sequence[float], b: Sequence[float]) -> float:
    va = np.asarray(a, dtype=np.float64).ravel()
    vb = np.asarray(b, dtype=np.float64).ravel()
    if va.size == 0 or vb.size == 0 or va.shape != vb.shape:
        return 0.0
    na = float(np.linalg.norm(va))
    nb = float(np.linalg.norm(vb))
    if na <= 1e-12 or nb <= 1e-12:
        return 0.0
    cos = float(np.dot(va, vb) / (na * nb))
    return float(np.clip(0.5 * (cos + 1.0), 0.0, 1.0))


def appearance_score(
    track_hist: Sequence[float],
    track_emb: Sequence[float],
    det_hist: Sequence[float],
    det_emb: Sequence[float],
) -> float:
    """Prefer multi-block embeddings; fall back to colour histograms."""
    if len(track_emb) > 0 and len(det_emb) > 0 and len(track_emb) == len(det_emb):
        return _embedding_cosine_sim(track_emb, det_emb)
    return histogram_correlation(list(track_hist), list(det_hist))


def _scale_score(
    track_bbox: tuple[float, float, float, float],
    det_bbox: tuple[float, float, float, float],
) -> float:
    tw = max(track_bbox[2], 1.0)
    th = max(track_bbox[3], 1.0)
    dw = max(det_bbox[2], 1.0)
    dh = max(det_bbox[3], 1.0)
    # Relative scale change; 1 when identical.
    rw = min(tw, dw) / max(tw, dw)
    rh = min(th, dh) / max(th, dh)
    return float(0.5 * (rw + rh))


def _motion_score(
    track_vel: tuple[float, float],
    prev_centroid: tuple[float, float],
    observed: tuple[float, float],
) -> float:
    """How consistent is the observed displacement with the track velocity?"""
    obs_vx = observed[0] - prev_centroid[0]
    obs_vy = observed[1] - prev_centroid[1]
    tv = float(np.hypot(track_vel[0], track_vel[1]))
    ov = float(np.hypot(obs_vx, obs_vy))
    # New / stationary tracks: no penalty.
    if tv < 0.5 and ov < 0.5:
        return 1.0
    # Directional agreement via cosine of velocity vectors.
    if tv < 1e-6 or ov < 1e-6:
        # One is nearly still — soft score from speed gap.
        gap = abs(tv - ov)
        return float(np.exp(-0.5 * (gap / 8.0) ** 2))
    cos = (track_vel[0] * obs_vx + track_vel[1] * obs_vy) / (tv * ov)
    return float(np.clip(0.5 * (cos + 1.0), 0.0, 1.0))


def association_score(
    track: VisualTrack,
    detection: Detection,
    *,
    predicted_xy: tuple[float, float],
    track_velocity: tuple[float, float] = (0.0, 0.0),
) -> float:
    iou = bbox_iou(track.bbox_xywh, detection.bbox_xywh)
    app = appearance_score(
        track.appearance_hist,
        track.appearance_embedding,
        detection.appearance_hist,
        detection.appearance_embedding,
    )
    scale = max(track.bbox_xywh[2], track.bbox_xywh[3], 8.0)
    pos = _position_score(predicted_xy, detection.centroid_xy, scale_px=scale)
    motion = _motion_score(track_velocity, track.centroid_xy, detection.centroid_xy)
    sc = _scale_score(track.bbox_xywh, detection.bbox_xywh)
    return (
        IOU_WEIGHT * iou
        + APPEARANCE_WEIGHT * app
        + POSITION_WEIGHT * pos
        + MOTION_WEIGHT * motion
        + SCALE_WEIGHT * sc
    )


def identity_confidence(track: VisualTrack) -> float:
    """Always-reported identity confidence = 1 − uncertainty."""
    return float(np.clip(1.0 - track.identity_uncertainty, 0.0, 1.0))


@dataclass(slots=True, kw_only=True)
class VisualTrack(V2Record):
    """One identity across time. Identity uncertainty is always reported.

    No ground-truth id field — GT lives only in the sealed evaluator.
    """

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
    appearance_embedding: list[float] = field(default_factory=list)
    identity_uncertainty: float = UNCERTAINTY_FLOOR
    association_score: float = 0.0
    identity_confidence: float = 1.0 - UNCERTAINTY_FLOOR
    occluder_track_id: str | None = None
    reappearance_prediction_xy: tuple[float, float] | None = None
    kalman: dict[str, float] = field(default_factory=dict)
    hit_streak: int = 0
    age: int = 0

    def __post_init__(self) -> None:
        if not self.track_id:
            self.track_id = self.id
        if not (0.0 <= self.identity_uncertainty <= 1.0):
            raise ValidationError("identity_uncertainty must be in [0, 1]")
        self.identity_confidence = float(
            np.clip(1.0 - self.identity_uncertainty, 0.0, 1.0)
        )

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
    kinematic_score: float = 0.0
    identity_uncertainty: float | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "matched": self.matched,
            "track_id": self.track_id,
            "score": self.score,
            "threshold": self.threshold,
            "reason": self.reason,
            "appearance_score": self.appearance_score,
            "kinematic_score": self.kinematic_score,
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


def _hungarian_assign(
    cost: ArrayF64,
) -> list[tuple[int, int]]:
    """Return (row, col) pairs from a cost matrix. Uses SciPy when available."""
    if cost.size == 0:
        return []
    n_rows, n_cols = cost.shape
    if n_rows == 0 or n_cols == 0:
        return []
    if _linear_sum_assignment is not None:
        rows, cols = _linear_sum_assignment(cost)
        return [(int(r), int(c)) for r, c in zip(rows, cols, strict=False)]
    # Pure-Python fallback for tiny problems (greedy-on-sorted is wrong for
    # crossings; use exhaustive search for N<=6, else greedy by cost).
    return _hungarian_fallback(cost)


def _hungarian_fallback(cost: ArrayF64) -> list[tuple[int, int]]:
    n_rows, n_cols = cost.shape
    n = max(n_rows, n_cols)
    # Pad to square.
    padded = np.full((n, n), _INFEASIBLE_COST, dtype=np.float64)
    padded[:n_rows, :n_cols] = cost
    if n <= 7:
        return _brute_force_assignment(padded, n_rows, n_cols)
    # Larger: iterative min-cost greedy with local repair is inadequate;
    # still better than pure greedy sort for moderate N via Jonker-like
    # successive shortest is heavy — use scipy-quality greedy with 2-opt.
    return _greedy_then_2opt(padded, n_rows, n_cols)


def _brute_force_assignment(
    padded: ArrayF64, n_rows: int, n_cols: int
) -> list[tuple[int, int]]:
    from itertools import permutations

    n = padded.shape[0]
    best_cost = float("inf")
    best_perm: tuple[int, ...] | None = None
    for perm in permutations(range(n)):
        c = sum(padded[i, perm[i]] for i in range(n))
        if c < best_cost:
            best_cost = c
            best_perm = perm
    if best_perm is None:
        return []
    return [
        (i, best_perm[i])
        for i in range(n)
        if i < n_rows and best_perm[i] < n_cols
    ]


def _greedy_then_2opt(
    padded: ArrayF64, n_rows: int, n_cols: int
) -> list[tuple[int, int]]:
    n = padded.shape[0]
    used_r: set[int] = set()
    used_c: set[int] = set()
    pairs: list[tuple[int, int]] = []
    flat = [
        (float(padded[r, c]), r, c)
        for r in range(n)
        for c in range(n)
    ]
    flat.sort(key=lambda t: t[0])
    assignment = [-1] * n
    for _, r, c in flat:
        if r in used_r or c in used_c:
            continue
        used_r.add(r)
        used_c.add(c)
        assignment[r] = c
        if len(used_r) == n:
            break
    # 2-opt swaps.
    improved = True
    while improved:
        improved = False
        for i in range(n):
            for j in range(i + 1, n):
                ci, cj = assignment[i], assignment[j]
                if ci < 0 or cj < 0:
                    continue
                before = padded[i, ci] + padded[j, cj]
                after = padded[i, cj] + padded[j, ci]
                if after + 1e-12 < before:
                    assignment[i], assignment[j] = cj, ci
                    improved = True
    for r, c in enumerate(assignment):
        if r < n_rows and 0 <= c < n_cols:
            pairs.append((r, c))
    return pairs


def associate_hungarian(
    tracks: Sequence[VisualTrack],
    detections: Sequence[Detection],
    *,
    predicted: dict[str, tuple[float, float]],
    velocities: dict[str, tuple[float, float]],
    association_threshold: float = ASSOCIATION_THRESHOLD,
) -> list[tuple[VisualTrack, Detection, float]]:
    """Global optimal assignment by cost = 1 − association_score.

    Pairs below ``association_threshold`` are treated as infeasible. Lost /
    occluded tracks additionally require their appearance re-id bar.
    """
    if not tracks or not detections:
        return []

    n_t = len(tracks)
    n_d = len(detections)
    scores = np.zeros((n_t, n_d), dtype=np.float64)
    feasible = np.zeros((n_t, n_d), dtype=bool)

    for i, trk in enumerate(tracks):
        pred = predicted.get(trk.track_id, trk.centroid_xy)
        vel = velocities.get(trk.track_id, (0.0, 0.0))
        scale = max(trk.bbox_xywh[2], trk.bbox_xywh[3], 8.0)
        max_dist = scale * MAX_ASSOCIATION_DIST_SCALES
        # LOST tracks get a slightly larger search radius for true reappearance.
        if trk.state is TrackState.LOST:
            max_dist = scale * MAX_ASSOCIATION_DIST_SCALES * 2.0
        for j, det in enumerate(detections):
            dist = float(
                np.hypot(pred[0] - det.centroid_xy[0], pred[1] - det.centroid_xy[1])
            )
            if dist > max_dist:
                scores[i, j] = 0.0
                continue
            score = association_score(
                trk, det, predicted_xy=pred, track_velocity=vel
            )
            scores[i, j] = score
            if score < association_threshold:
                continue
            # Stricter appearance gate for permanence recovery.
            app = appearance_score(
                trk.appearance_hist,
                trk.appearance_embedding,
                det.appearance_hist,
                det.appearance_embedding,
            )
            if trk.state is TrackState.LOST and app < REID_THRESHOLD_LOST:
                continue
            if trk.state is TrackState.OCCLUDED and app < REID_THRESHOLD_OCCLUDED:
                continue
            # LOST also needs kinematic plausibility (re-id dual gate).
            if trk.state is TrackState.LOST:
                kin = _position_score(pred, det.centroid_xy, scale_px=scale * 2.0)
                if kin < REID_KINEMATIC_THRESHOLD:
                    continue
            feasible[i, j] = True

    cost = np.where(feasible, 1.0 - scores, _INFEASIBLE_COST)
    pairs_idx = _hungarian_assign(cost)
    matches: list[tuple[VisualTrack, Detection, float]] = []
    for i, j in pairs_idx:
        if not feasible[i, j]:
            continue
        matches.append((tracks[i], detections[j], float(scores[i, j])))
    return matches


def associate_greedy(
    tracks: Sequence[VisualTrack],
    detections: Sequence[Detection],
    *,
    predicted: dict[str, tuple[float, float]],
    velocities: dict[str, tuple[float, float]],
    association_threshold: float = ASSOCIATION_THRESHOLD,
) -> list[tuple[VisualTrack, Detection, float]]:
    """Greedy association (exported for tests that prove Hungarian is better)."""
    pairs: list[tuple[float, VisualTrack, Detection]] = []
    for trk in tracks:
        pred = predicted.get(trk.track_id, trk.centroid_xy)
        vel = velocities.get(trk.track_id, (0.0, 0.0))
        for det in detections:
            score = association_score(
                trk, det, predicted_xy=pred, track_velocity=vel
            )
            if score >= association_threshold:
                pairs.append((score, trk, det))
    pairs.sort(key=lambda item: item[0], reverse=True)
    assigned_t: set[str] = set()
    assigned_d: set[str] = set()
    matches: list[tuple[VisualTrack, Detection, float]] = []
    for score, trk, det in pairs:
        if trk.track_id in assigned_t or det.detection_id in assigned_d:
            continue
        app = appearance_score(
            trk.appearance_hist,
            trk.appearance_embedding,
            det.appearance_hist,
            det.appearance_embedding,
        )
        if trk.state is TrackState.LOST and app < REID_THRESHOLD_LOST:
            continue
        if trk.state is TrackState.OCCLUDED and app < REID_THRESHOLD_OCCLUDED:
            continue
        assigned_t.add(trk.track_id)
        assigned_d.add(det.detection_id)
        matches.append((trk, det, score))
    return matches


def track(
    detections: Sequence[Detection],
    state: TrackerState | None = None,
    *,
    frame_index: int | None = None,
) -> TrackerState:
    """Associate detections to tracks for one frame; update permanence state.

    Unmatched tracks transition ACTIVE→OCCLUDED (with occluder if found) then
    LOST after MAX_OCCLUDED_FRAMES. Identity uncertainty grows monotonically
    while unseen. New detections spawn new tracks. Assignment is Hungarian.
    """
    tracker = state if state is not None else TrackerState()
    fi = tracker.frame_index + 1 if frame_index is None else frame_index
    tracker.frame_index = fi

    # Contract: no GT symbol may ride on a detection meta key.
    for det in detections:
        for banned in ("gt_id", "ground_truth_id", "ground_truth", "oracle_id"):
            if banned in det.meta:
                raise ValidationError(
                    f"ground truth key {banned!r} reached tracker on {det.detection_id}"
                )

    # Predict all filters.
    predicted: dict[str, tuple[float, float]] = {}
    velocities: dict[str, tuple[float, float]] = {}
    for tid, filt in tracker.filters.items():
        predicted[tid] = filt.predict(1.0)
        velocities[tid] = filt.velocity()

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

    matches = associate_hungarian(
        active_pool,
        list(detections),
        predicted=predicted,
        velocities=velocities,
    )
    assigned_dets = {det.detection_id for _, det, _ in matches}
    matched_ids = {trk.track_id for trk, _, _ in matches}

    updated: list[VisualTrack] = []

    for trk, det, score in matches:
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

        old_e = np.asarray(trk.appearance_embedding, dtype=np.float64)
        new_e = np.asarray(det.appearance_embedding, dtype=np.float64)
        if old_e.size == new_e.size and old_e.size > 0:
            blended_e = 0.7 * old_e + 0.3 * new_e
            nrm = float(np.linalg.norm(blended_e))
            emb = [float(v) for v in (blended_e / nrm if nrm > 1e-12 else blended_e)]
        else:
            emb = list(det.appearance_embedding)

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
            appearance_embedding=emb,
            identity_uncertainty=float(identity_u),
            association_score=float(score),
            occluder_track_id=None,
            reappearance_prediction_xy=None,
            kalman=filt.to_dict(),
            hit_streak=trk.hit_streak + 1,
            age=trk.age + 1,
            authority=AuthorityClass.SENSOR_DERIVED,
            lineage=Lineage(
                operation="ocular.track.associate",
                inputs=[trk.id, det.detection_id],
                input_authorities=[],
                parameters={
                    "score": score,
                    "prev_state": prev_state.value,
                    "assignment": "hungarian",
                    "source_authorities": [
                        SEGMENT_AUTHORITY_CEILING.value,
                        AuthorityClass.SENSOR_DERIVED.value,
                    ],
                    "thresholds": {
                        "association": ASSOCIATION_THRESHOLD,
                        "reid_occluded": REID_THRESHOLD_OCCLUDED,
                        "reid_lost": REID_THRESHOLD_LOST,
                        "reid_kinematic": REID_KINEMATIC_THRESHOLD,
                    },
                    "weights": {
                        "iou": IOU_WEIGHT,
                        "appearance": APPEARANCE_WEIGHT,
                        "position": POSITION_WEIGHT,
                        "motion": MOTION_WEIGHT,
                        "scale": SCALE_WEIGHT,
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
            others = [t for t in updated if t.track_id != trk.track_id]
            others = others + [
                t
                for t in active_pool
                if t.track_id != trk.track_id and t.track_id in matched_ids
            ]
            occluder = _detect_occluder(trk, others if others else active_pool, pred)
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
            appearance_embedding=list(trk.appearance_embedding),
            identity_uncertainty=float(identity_u),
            association_score=0.0,
            occluder_track_id=occluder,
            reappearance_prediction_xy=pred,
            kalman=filt.to_dict(),
            hit_streak=0,
            age=trk.age + 1,
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

    # New detections → new tracks (unknown entrants always get a fresh id).
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
            appearance_embedding=list(det.appearance_embedding),
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
    require_kinematics: bool = True,
) -> ReidentifyDecision:
    """Re-attach a detection only when appearance **and** kinematics both clear.

    Appearance-only matches (similar look, wrong place) and kinematics-only
    matches (right place, wrong look) are refused. Lost tracks use
    REID_THRESHOLD_LOST unless the caller overrides min_score.
    """
    if not tracks:
        return ReidentifyDecision(
            matched=False,
            track_id=None,
            score=0.0,
            threshold=min_score if min_score is not None else REID_THRESHOLD_OCCLUDED,
            reason="no-candidate-tracks",
            appearance_score=0.0,
            kinematic_score=0.0,
        )

    best: VisualTrack | None = None
    best_score = -1.0
    best_app = 0.0
    best_kin = 0.0
    best_threshold = REID_THRESHOLD_OCCLUDED

    for trk in tracks:
        eligible = {
            TrackState.OCCLUDED,
            TrackState.LOST,
            TrackState.REAPPEARED,
        }
        if require_lost_or_occluded and trk.state not in eligible:
            continue
        if trk.state is TrackState.LOST:
            threshold = REID_THRESHOLD_LOST if min_score is None else min_score
        else:
            threshold = REID_THRESHOLD_OCCLUDED if min_score is None else min_score

        app = appearance_score(
            trk.appearance_hist,
            trk.appearance_embedding,
            detection.appearance_hist,
            detection.appearance_embedding,
        )
        pred = trk.predicted_xy if trk.predicted_xy != (0.0, 0.0) else trk.centroid_xy
        scale = max(trk.bbox_xywh[2], trk.bbox_xywh[3], 8.0)
        # Slightly more lenient spatial gate for re-id after long absence.
        kin = _position_score(pred, detection.centroid_xy, scale_px=scale * 2.0)
        iou = bbox_iou(trk.bbox_xywh, detection.bbox_xywh)
        # Re-id emphasises appearance over IoU (bbox may have drifted).
        score = 0.55 * app + 0.30 * kin + 0.15 * iou
        if score > best_score:
            best_score = score
            best = trk
            best_app = app
            best_kin = kin
            best_threshold = threshold

    if best is None:
        return ReidentifyDecision(
            matched=False,
            track_id=None,
            score=0.0,
            threshold=best_threshold,
            reason="no-eligible-tracks",
            appearance_score=0.0,
            kinematic_score=0.0,
        )

    # Dual gate: appearance alone or kinematics alone is insufficient.
    # LOST re-id also requires the composite score to clear the same bar as
    # appearance — a soft 0.85× fudge previously let distractors through.
    if best_app < best_threshold:
        return ReidentifyDecision(
            matched=False,
            track_id=best.track_id,
            score=float(best_score),
            threshold=best_threshold,
            reason=(
                f"appearance-only-insufficient appearance={best_app:.4f} "
                f"kinematic={best_kin:.4f} required_app={best_threshold:.4f}"
            ),
            appearance_score=float(best_app),
            kinematic_score=float(best_kin),
            identity_uncertainty=best.identity_uncertainty,
        )
    if require_kinematics and best_kin < REID_KINEMATIC_THRESHOLD:
        return ReidentifyDecision(
            matched=False,
            track_id=best.track_id,
            score=float(best_score),
            threshold=best_threshold,
            reason=(
                f"kinematics-only-insufficient appearance={best_app:.4f} "
                f"kinematic={best_kin:.4f} required_kin={REID_KINEMATIC_THRESHOLD:.4f}"
            ),
            appearance_score=float(best_app),
            kinematic_score=float(best_kin),
            identity_uncertainty=best.identity_uncertainty,
        )
    composite_bar = best_threshold if best.state is TrackState.LOST else best_threshold * 0.85
    if best_score < composite_bar:
        return ReidentifyDecision(
            matched=False,
            track_id=best.track_id,
            score=float(best_score),
            threshold=best_threshold,
            reason=(
                f"below-threshold appearance={best_app:.4f} score={best_score:.4f} "
                f"required={composite_bar:.4f}"
            ),
            appearance_score=float(best_app),
            kinematic_score=float(best_kin),
            identity_uncertainty=best.identity_uncertainty,
        )

    return ReidentifyDecision(
        matched=True,
        track_id=best.track_id,
        score=float(best_score),
        threshold=best_threshold,
        reason=f"matched state={best.state.value}",
        appearance_score=float(best_app),
        kinematic_score=float(best_kin),
        identity_uncertainty=best.identity_uncertainty,
    )


def track_metrics(
    frame_assignments: list[dict[str, str | None]],
    ground_truth_ids: Sequence[str],
) -> dict[str, Any]:
    """Compute ID switches, MOTA-style accuracy, fragmentation, re-id stats.

    ``frame_assignments`` is a list of {gt_id: track_id_or_None} per frame.
    This is a **sealed evaluator** helper: GT ids are scoring labels only and
    never enter TrackerState.
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
    reid_tp = 0
    reid_fp = 0
    reid_fn = 0

    for frame in frame_assignments:
        for gid in ground_truth_ids:
            tid = frame.get(gid)
            if tid is None:
                misses += 1
                if gid in prev_map:
                    fragments += 1
                continue
            tid_s = str(tid)
            matches += 1
            if gid in prev_map and prev_map[gid] != tid_s:
                id_switches += 1
            prev_map[gid] = tid_s
            if not gt_to_tracks[gid] or gt_to_tracks[gid][-1] != tid_s:
                gt_to_tracks[gid].append(tid_s)
            for other_gid, other_tid in frame.items():
                if other_gid not in ground_truth_ids:
                    continue
                if other_tid == tid_s and other_gid != gid:
                    confusion[gid][str(other_gid)] += 1
            confusion[gid][gid] += 1

        false_positives += int(frame.get("_false_positives", 0) or 0)
        reid_tp += int(frame.get("_reid_tp", 0) or 0)
        reid_fp += int(frame.get("_reid_fp", 0) or 0)
        reid_fn += int(frame.get("_reid_fn", 0) or 0)

    total = matches + misses + false_positives
    mota = 1.0 - (misses + false_positives + id_switches) / total if total else 0.0
    # IDF1: identity F1 over the GT→track mapping consistency.
    idtp = matches  # simplified: each correct match counts as IDTP when no switch
    # More honest: identity precision/recall from fragment counts.
    n_id_pred = sum(len(tids) for tids in gt_to_tracks.values())
    n_gt = len(ground_truth_ids)
    idf1 = 0.0
    if n_id_pred > 0 and matches > 0:
        # Standard simplified IDF1 proxy: 2*IDTP / (n_gt_dets + n_pred_ids_used)
        id_precision = matches / max(1, matches + id_switches)
        id_recall = matches / max(1, matches + misses)
        idf1 = (
            2 * id_precision * id_recall / (id_precision + id_recall)
            if (id_precision + id_recall) > 0
            else 0.0
        )
    fragmentation = {
        gid: max(0, len(tids) - 1) for gid, tids in gt_to_tracks.items()
    }
    reid_precision = reid_tp / (reid_tp + reid_fp) if (reid_tp + reid_fp) else 1.0
    reid_recall = reid_tp / (reid_tp + reid_fn) if (reid_tp + reid_fn) else 1.0
    precision = matches / (matches + false_positives) if (matches + false_positives) else 0.0
    recall = matches / (matches + misses) if (matches + misses) else 0.0

    return {
        "id_switches": id_switches,
        "mota": mota,
        "idf1": idf1,
        "matches": matches,
        "misses": misses,
        "false_positives": false_positives,
        "precision": precision,
        "recall": recall,
        "fragmentation": fragmentation,
        "track_fragmentation_total": sum(fragmentation.values()),
        "confusion": confusion,
        "reid_precision": reid_precision,
        "reid_recall": reid_recall,
        "reid_tp": reid_tp,
        "reid_fp": reid_fp,
        "reid_fn": reid_fn,
        "gt_to_tracks": gt_to_tracks,
        "n_gt_identities": n_gt,
        "n_predicted_id_fragments": n_id_pred,
        "idtp_proxy": idtp,
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


def assert_no_ground_truth_on_tracks(tracks: Sequence[VisualTrack]) -> None:
    """Hard contract: runtime tracks must not carry GT identity fields."""
    for trk in tracks:
        if hasattr(trk, "ground_truth_id"):
            # slots=True dataclasses still expose class attributes; reject values.
            val = getattr(trk, "ground_truth_id", None)
            if val is not None:
                raise AssertionError(
                    f"track {trk.track_id} carries ground_truth_id={val!r}"
                )
        # Also reject if the field exists as an instance annotation with a value
        # via __dict__ (non-slots subclasses).
        data = getattr(trk, "__dict__", {})
        if "ground_truth_id" in data and data["ground_truth_id"] is not None:
            raise AssertionError(f"track {trk.track_id} has ground_truth_id in __dict__")
