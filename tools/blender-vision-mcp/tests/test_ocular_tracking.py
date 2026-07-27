"""Phases E/F: classical segmentation, tracking, object permanence, model intake."""

from __future__ import annotations

import numpy as np
import pytest

from blender_vision.core.models import BackendState
from blender_vision.ocular.registry import (
    ModelFamily,
    PhysicalModelSelectionError,
    ReviewState,
    default_registry,
)
from blender_vision.ocular.segment import (
    SEGMENT_AUTHORITY_CEILING,
    ConceptResolution,
    SegmentationMethod,
    appearance_histogram,
    histogram_correlation,
    segment,
    segment_concept,
)
from blender_vision.ocular.track import (
    REID_THRESHOLD_LOST,
    REID_THRESHOLD_OCCLUDED,
    UNCERTAINTY_GROWTH_PER_FRAME,
    Detection,
    TrackerState,
    TrackState,
    TrackTargetKind,
    reidentify,
    track,
)
from blender_vision.v2.authority import AuthorityClass


def _solid_scene() -> np.ndarray:
    """Two coloured squares on a dark background."""
    img = np.zeros((120, 180, 3), dtype=np.uint8)
    img[:] = (18, 18, 18)
    img[25:70, 25:70] = (40, 40, 210)  # red-ish in BGR
    img[25:70, 110:155] = (40, 200, 40)  # green
    return img


def _hist_unique(seed: int, bins: int = 48) -> list[float]:
    """Peaked histogram so distinct seeds are far under Bhattacharyya coeff."""
    h = np.zeros(bins, dtype=np.float64)
    # Two sharp peaks whose locations depend on seed — random flat hists
    # correlate ~0.9 and cannot exercise re-id thresholds.
    p0 = seed % bins
    p1 = (seed * 7 + 13) % bins
    h[p0] = 0.70
    h[p1] = 0.25
    h[(p0 + p1) % bins] = 0.05
    h = h / h.sum()
    return [float(v) for v in h]


def _det(
    frame: int,
    x: float,
    y: float,
    hist: list[float],
    *,
    det_id: str | None = None,
    w: float = 24.0,
    h: float = 24.0,
) -> Detection:
    return Detection(
        detection_id=det_id or f"d-{frame}-{int(x)}-{int(y)}",
        kind=TrackTargetKind.OBJECT,
        bbox_xywh=(x, y, w, h),
        centroid_xy=(x + w / 2.0, y + h / 2.0),
        appearance_hist=list(hist),
        area_px=w * h,
        frame_index=frame,
    )


def test_registry_entries_are_review_pending_and_not_physical() -> None:
    registry = default_registry()
    entries = registry.list_entries()
    assert len(entries) >= 8
    families = {e.family for e in entries}
    for required in (
        ModelFamily.DENSE_FEATURES,
        ModelFamily.PROMPTABLE_SEGMENTATION,
        ModelFamily.GEOMETRY,
        ModelFamily.POINT_TRACKING,
        ModelFamily.RADIANCE,
        ModelFamily.PREDICTION,
    ):
        assert required in families
    for entry in entries:
        assert entry.review_state is ReviewState.REVIEW_PENDING
        assert entry.checkpoint_path is None
        assert not entry.selectable_for_physical()
        assert entry.backend_state() in {
            BackendState.DOWNLOAD_REQUIRED,
            BackendState.UNAVAILABLE,
            BackendState.LICENSE_REVIEW_REQUIRED,
        }
    assert registry.physical_candidates() == []


def test_registry_no_local_checkpoint_unselectable_for_physical() -> None:
    registry = default_registry()
    entry = registry.get("sam2-hiera-large")
    assert entry.checkpoint_path is None
    assert entry.backend_state() is BackendState.DOWNLOAD_REQUIRED
    with pytest.raises(PhysicalModelSelectionError, match="not selectable for a physical claim"):
        registry.select_for_physical("sam2-hiera-large")
    with pytest.raises(PhysicalModelSelectionError):
        registry.select_for_physical("dino-v2-vitb14")
    with pytest.raises(PhysicalModelSelectionError):
        registry.select_for_physical("cotracker3-point")


def test_segmentation_stable_across_static_frame_pair() -> None:
    img = _solid_scene()
    first, labels1 = segment(img, method=SegmentationMethod.WATERSHED, min_area=40)
    second, labels2 = segment(
        img,
        method=SegmentationMethod.WATERSHED,
        min_area=40,
        previous_result=first,
    )
    assert first.authority is SEGMENT_AUTHORITY_CEILING
    assert second.authority is SEGMENT_AUTHORITY_CEILING
    assert len(first.instances) >= 2
    assert len(second.instances) >= 2
    # Stable IDs: every second-frame instance should reuse a first-frame id when static.
    first_ids = {inst.segment_id for inst in first.instances}
    second_ids = {inst.segment_id for inst in second.instances}
    assert first_ids == second_ids or first_ids.issubset(second_ids) or second_ids.issubset(
        first_ids
    )
    # Identity links should confirm stabilisation when previous is supplied.
    assert second.previous_result_id == first.id
    # Label maps should agree on covered area for static scene.
    assert int((labels1 > 0).sum()) > 0
    overlap = int(np.logical_and(labels1 > 0, labels2 > 0).sum())
    union = int(np.logical_or(labels1 > 0, labels2 > 0).sum())
    assert overlap / union >= 0.85


def test_segment_concept_unresolved_for_unknown_prompt() -> None:
    img = _solid_scene()
    result, labels = segment_concept(img, "a purple quasicrystal spaceship")
    assert result.concept_resolution is ConceptResolution.UNRESOLVED
    assert result.authority is AuthorityClass.UNRESOLVED
    assert result.instances == []
    assert labels is None
    assert any("UNRESOLVED" in note for note in result.notes)


def test_segment_concept_resolves_local_colour() -> None:
    img = _solid_scene()
    result, labels = segment_concept(img, "red")
    assert result.concept_resolution is ConceptResolution.RESOLVED
    assert labels is not None
    assert int((labels > 0).sum()) > 0
    assert result.authority is SEGMENT_AUTHORITY_CEILING


def test_identity_preserved_through_synthetic_occlusion() -> None:
    hist = _hist_unique(7)
    state = TrackerState()
    # Appear for three frames.
    for fi in range(3):
        state = track([_det(fi, 40 + fi, 50, hist, det_id=f"a-{fi}")], state, frame_index=fi)
    track_id = state.tracks[0].track_id
    # Occlusion: no detection for several frames.
    for fi in range(3, 10):
        state = track([], state, frame_index=fi)
        trk = next(t for t in state.tracks if t.track_id == track_id)
        assert trk.state in {TrackState.OCCLUDED, TrackState.LOST}
    # Reappear near prediction.
    state = track([_det(10, 48, 50, hist, det_id="a-back")], state, frame_index=10)
    recovered = [t for t in state.tracks if t.track_id == track_id]
    assert len(recovered) == 1
    assert recovered[0].state in {TrackState.REAPPEARED, TrackState.ACTIVE}
    # No second identity for the same object.
    assert sum(1 for t in state.tracks if t.state is not TrackState.LOST) == 1


def test_reidentify_refuses_below_threshold_match() -> None:
    original = _hist_unique(11)
    different = _hist_unique(99)
    # Ensure they are not accidentally similar.
    assert histogram_correlation(original, different) < REID_THRESHOLD_OCCLUDED

    state = TrackerState()
    state = track([_det(0, 30, 30, original)], state, frame_index=0)
    tid = state.tracks[0].track_id
    # Force LOST with empty frames past occlusion window.
    for fi in range(1, 50):
        state = track([], state, frame_index=fi)
    trk = next(t for t in state.tracks if t.track_id == tid)
    assert trk.state is TrackState.LOST
    assert trk.identity_uncertainty > UNCERTAINTY_GROWTH_PER_FRAME

    decision = reidentify(
        _det(50, 30, 30, different, det_id="impostor"),
        state.tracks,
        min_score=REID_THRESHOLD_LOST,
    )
    assert decision.matched is False
    assert decision.appearance_score < REID_THRESHOLD_LOST
    refused = (
        "below-threshold" in decision.reason
        or "no-eligible" in decision.reason
        or decision.score < decision.threshold
    )
    assert refused


def test_replaced_object_negative_case() -> None:
    """Departed object replaced by a different similar object must not re-id."""
    original = _hist_unique(21)
    # Similar-class object: nearby peaks (confusable) but below LOST re-id bar.
    replacement = _hist_unique(29)
    corr = histogram_correlation(original, replacement)
    assert corr < REID_THRESHOLD_LOST

    state = TrackerState()
    state = track([_det(0, 20, 40, original, det_id="orig")], state, frame_index=0)
    tid = state.tracks[0].track_id
    for fi in range(1, 50):
        state = track([], state, frame_index=fi)
    trk = next(t for t in state.tracks if t.track_id == tid)
    assert trk.state is TrackState.LOST

    # Replacement appears in a different place with different appearance.
    state = track(
        [_det(50, 140, 40, replacement, det_id="replacement")],
        state,
        frame_index=50,
    )
    # The original must remain LOST (or aged out); replacement must be a new id.
    by_id = {t.track_id: t for t in state.tracks}
    assert tid in by_id
    assert by_id[tid].state is TrackState.LOST
    new_tracks = [t for t in state.tracks if t.track_id != tid and t.state is TrackState.ACTIVE]
    assert len(new_tracks) == 1
    assert new_tracks[0].track_id != tid

    decision = reidentify(
        _det(50, 140, 40, replacement, det_id="replacement-check"),
        [by_id[tid]],
        min_score=REID_THRESHOLD_LOST,
    )
    assert decision.matched is False


def test_identity_uncertainty_grows_monotonically_while_occluded() -> None:
    hist = _hist_unique(3)
    state = TrackerState()
    state = track([_det(0, 50, 50, hist)], state, frame_index=0)
    tid = state.tracks[0].track_id
    uncertainties: list[float] = []
    for fi in range(1, 16):
        state = track([], state, frame_index=fi)
        trk = next(t for t in state.tracks if t.track_id == tid)
        assert trk.state in {TrackState.OCCLUDED, TrackState.LOST}
        uncertainties.append(trk.identity_uncertainty)
    # Strictly non-decreasing, and actually increased over the window.
    for a, b in zip(uncertainties, uncertainties[1:], strict=False):
        assert b >= a - 1e-9
    assert uncertainties[-1] > uncertainties[0]
    assert uncertainties[-1] >= UNCERTAINTY_GROWTH_PER_FRAME * 2


def test_similar_objects_not_merged() -> None:
    """Three near-identical colours keep separate track ids via position."""
    # Near-identical histograms (same shape class), separated in space.
    base = _hist_unique(5)
    hists = []
    for i in range(3):
        noisy = [max(0.0, v + 0.01 * (i - 1)) for v in base]
        s = sum(noisy)
        hists.append([v / s for v in noisy])

    state = TrackerState()
    positions = [(20.0, 30.0), (80.0, 30.0), (140.0, 30.0)]
    for fi in range(8):
        dets = [
            _det(fi, x + fi * (0.5 if i == 1 else 0.0), y, hists[i], det_id=f"o{i}-{fi}")
            for i, (x, y) in enumerate(positions)
        ]
        state = track(dets, state, frame_index=fi)

    active = [t for t in state.tracks if t.state in {TrackState.ACTIVE, TrackState.REAPPEARED}]
    assert len(active) == 3
    ids = {t.track_id for t in active}
    assert len(ids) == 3


def test_grabcut_and_region_grow_and_motion() -> None:
    img = _solid_scene()
    seeds = [(40, 40), (130, 40)]
    grown, labels = segment(
        img, method=SegmentationMethod.REGION_GROW, seeds=seeds, min_area=30, colour_radius=40
    )
    assert grown.method is SegmentationMethod.REGION_GROW
    assert len(grown.instances) >= 1
    assert int((labels > 0).sum()) > 0

    gc, gc_labels = segment(
        img, method=SegmentationMethod.GRABCUT, box=(20, 20, 55, 55), min_area=20
    )
    assert gc.method is SegmentationMethod.GRABCUT
    assert int((gc_labels > 0).sum()) > 0

    moved = img.copy()
    moved[25:70, 25:70] = (18, 18, 18)
    moved[25:70, 70:115] = (40, 40, 210)
    motion, mlabels = segment(
        moved,
        method=SegmentationMethod.MOTION_COMPONENTS,
        previous_image=img,
        min_area=20,
        residual_threshold=10,
    )
    assert motion.method is SegmentationMethod.MOTION_COMPONENTS
    assert int((mlabels > 0).sum()) > 0


def test_appearance_histogram_self_correlation() -> None:
    img = _solid_scene()
    mask = np.zeros(img.shape[:2], dtype=np.uint8)
    mask[25:70, 25:70] = 1
    hist = appearance_histogram(img, mask)
    assert abs(sum(hist) - 1.0) < 1e-6
    assert histogram_correlation(hist, hist) == pytest.approx(1.0, abs=1e-6)
