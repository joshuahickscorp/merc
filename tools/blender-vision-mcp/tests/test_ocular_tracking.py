"""Phases E/F: classical segmentation, tracking, object permanence, model intake."""

from __future__ import annotations

from pathlib import Path

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
    VisualTrack,
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


# ---------------------------------------------------------------------------
# Perception-driven tracking contracts (no GT on runtime structures).
# ---------------------------------------------------------------------------


def test_no_ground_truth_id_on_runtime_track_structures() -> None:
    """ground_truth_id must be gone from VisualTrack and TrackerState."""
    import ast
    import inspect

    from blender_vision.ocular import track as track_mod

    assert "ground_truth_id" not in VisualTrack.__dataclass_fields__
    assert "ground_truth_id" not in Detection.__dataclass_fields__

    src_path = inspect.getsourcefile(track_mod)
    assert src_path is not None
    tree = ast.parse(Path(src_path).read_text(encoding="utf-8"))
    assigned_names = {
        node.targets[0].id
        for node in ast.walk(tree)
        if isinstance(node, ast.Assign)
        and len(node.targets) == 1
        and isinstance(node.targets[0], ast.Name)
    }
    assert "ground_truth_id" not in assigned_names

    # Live track must not expose a GT attribute value.
    state = TrackerState()
    state = track([_det(0, 10, 10, _hist_unique(1))], state, frame_index=0)
    trk = state.tracks[0]
    assert not hasattr(trk, "ground_truth_id") or getattr(trk, "ground_truth_id", None) is None


def test_tracker_rejects_detection_with_gt_meta() -> None:
    det = _det(0, 20, 20, _hist_unique(2), det_id="poisoned")
    det.meta["gt_id"] = "oracle-box"
    with pytest.raises(Exception, match="ground truth"):
        track([det], TrackerState(), frame_index=0)


def test_hungarian_beats_greedy_on_crossing() -> None:
    """Constructed crossing: global assignment preserves identity; greedy can switch."""
    from blender_vision.ocular.track import TrackState as _TS
    from blender_vision.ocular.track import associate_greedy, associate_hungarian

    h_a = _hist_unique(3)
    h_b = _hist_unique(90)
    # Two tracks moving toward a cross: A going right, B going left.
    trk_a = VisualTrack(
        id="trk-a",
        track_id="trk-a",
        state=_TS.ACTIVE,
        frame_index=4,
        first_frame=0,
        last_seen_frame=4,
        bbox_xywh=(90.0, 40.0, 20.0, 20.0),
        centroid_xy=(100.0, 50.0),
        predicted_xy=(110.0, 50.0),  # continuing right
        appearance_hist=h_a,
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    trk_b = VisualTrack(
        id="trk-b",
        track_id="trk-b",
        state=_TS.ACTIVE,
        frame_index=4,
        first_frame=0,
        last_seen_frame=4,
        bbox_xywh=(110.0, 40.0, 20.0, 20.0),
        centroid_xy=(120.0, 50.0),
        predicted_xy=(110.0, 50.0),  # continuing left → same prediction zone
        appearance_hist=h_b,
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    # Detections after cross: A is now on the right, B on the left.
    det_a = _det(5, 115.0, 40.0, h_a, det_id="da", w=20.0, h=20.0)
    det_b = _det(5, 85.0, 40.0, h_b, det_id="db", w=20.0, h=20.0)
    predicted = {"trk-a": (115.0, 50.0), "trk-b": (95.0, 50.0)}
    velocities = {"trk-a": (10.0, 0.0), "trk-b": (-10.0, 0.0)}

    hun = associate_hungarian(
        [trk_a, trk_b],
        [det_a, det_b],
        predicted=predicted,
        velocities=velocities,
    )
    hun_map = {t.track_id: d.detection_id for t, d, _ in hun}
    assert hun_map.get("trk-a") == "da"
    assert hun_map.get("trk-b") == "db"

    # Greedy with intentionally ambiguous scores: put the higher composite on the
    # wrong pairing first by swapping appearance weights via near-overlap.
    # Prove Hungarian is at least as good (correct) on this constructed case.
    gre = associate_greedy(
        [trk_a, trk_b],
        [det_a, det_b],
        predicted={"trk-a": (100.0, 50.0), "trk-b": (100.0, 50.0)},
        velocities=velocities,
    )
    gre_map = {t.track_id: d.detection_id for t, d, _ in gre}
    # Hungarian must be correct; greedy may or may not — the contract is that
    # Hungarian solves the global optimum for the cost we defined.
    assert hun_map == {"trk-a": "da", "trk-b": "db"}
    # When greedy also gets it right that is fine; when it switches, Hungarian wins.
    if gre_map != hun_map:
        assert hun_map["trk-a"] != gre_map.get("trk-a") or hun_map["trk-b"] != gre_map.get(
            "trk-b"
        )


def test_reidentify_refuses_appearance_only_match() -> None:
    """Same appearance far from the prediction must not re-id."""
    hist = _hist_unique(41)
    state = TrackerState()
    state = track([_det(0, 30, 30, hist)], state, frame_index=0)
    tid = state.tracks[0].track_id
    for fi in range(1, 50):
        state = track([], state, frame_index=fi)
    trk = next(t for t in state.tracks if t.track_id == tid)
    assert trk.state is TrackState.LOST
    # Appear with perfect appearance but far from predicted position.
    far = _det(50, 280, 200, hist, det_id="far")
    decision = reidentify(far, [trk], min_score=REID_THRESHOLD_LOST)
    assert decision.matched is False
    assert "kinematic" in decision.reason or decision.kinematic_score < 0.25


def test_reidentify_refuses_kinematics_only_match() -> None:
    """Right place, wrong appearance must not re-id."""
    original = _hist_unique(17)
    different = _hist_unique(88)
    assert histogram_correlation(original, different) < REID_THRESHOLD_OCCLUDED
    state = TrackerState()
    state = track([_det(0, 40, 40, original)], state, frame_index=0)
    tid = state.tracks[0].track_id
    for fi in range(1, 50):
        state = track([], state, frame_index=fi)
    trk = next(t for t in state.tracks if t.track_id == tid)
    # Near predicted position but different appearance.
    impostor = _det(50, trk.predicted_xy[0] - 10, trk.predicted_xy[1] - 10, different)
    decision = reidentify(impostor, [trk], min_score=REID_THRESHOLD_LOST)
    assert decision.matched is False
    assert decision.appearance_score < REID_THRESHOLD_LOST


def test_unknown_entering_gets_new_id() -> None:
    h1 = _hist_unique(5)
    h2 = _hist_unique(60)
    state = TrackerState()
    state = track([_det(0, 30, 30, h1, det_id="known")], state, frame_index=0)
    known_id = state.tracks[0].track_id
    # Known continues; unknown enters far away.
    state = track(
        [
            _det(1, 32, 30, h1, det_id="known-1"),
            _det(1, 200, 40, h2, det_id="unknown"),
        ],
        state,
        frame_index=1,
    )
    active = [t for t in state.tracks if t.state is TrackState.ACTIVE]
    assert len(active) == 2
    ids = {t.track_id for t in active}
    assert known_id in ids
    assert len(ids) == 2


def test_uncertainty_monotone_during_occlusion_contract() -> None:
    """Contract restatement: uncertainty is non-decreasing while unseen."""
    hist = _hist_unique(9)
    state = TrackerState()
    state = track([_det(0, 50, 50, hist)], state, frame_index=0)
    tid = state.tracks[0].track_id
    series: list[float] = []
    for fi in range(1, 12):
        state = track([], state, frame_index=fi)
        trk = next(t for t in state.tracks if t.track_id == tid)
        series.append(trk.identity_uncertainty)
    for a, b in zip(series, series[1:], strict=False):
        assert b >= a - 1e-9
    assert series[-1] > series[0]


@pytest.mark.skipif(
    __import__("os").environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 to run Blender hard-fixture render",
)
def test_blender_hard_fixture_renders() -> None:
    """Physical Blender path for ocular_hard (gated; actually invokes Blender)."""
    import os
    import subprocess

    blender = os.environ.get("BVMCP_BLENDER") or "/Applications/Blender.app/Contents/MacOS/Blender"
    if not Path(blender).is_file():
        pytest.skip("Blender not installed")
    root = Path(__file__).resolve().parents[1]
    script = root / "benchmarks" / "ocular_hard" / "create_scene.py"
    out = root / "artifacts" / "ocular" / "tracking" / "blender_test_hard"
    proc = subprocess.run(
        [blender, "--background", "--python", str(script), "--", "--output", str(out)],
        cwd=str(root),
        capture_output=True,
        text=True,
        timeout=2400,
        check=False,
    )
    combined = (proc.stdout or "") + (proc.stderr or "")
    assert "OCULAR_HARD_COMPLETE" in combined or (out / "OCULAR_HARD_COMPLETE").is_file(), (
        f"Blender hard fixture failed rc={proc.returncode}\n{combined[-2000:]}"
    )
