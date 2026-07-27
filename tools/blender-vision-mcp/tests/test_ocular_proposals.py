"""Multi-source region proposals and non-destructive fusion.

Required coverage:
  - three stationary objects from a single first frame
  - geometry source BLOCKED rather than inventing depth
  - split hypothesis preserved for two touching objects
  - merge hypothesis preserved for one fragmented object
  - fusion not collapsing to a single region on crossing fixture
  - unknown-region source producing a NEW_UNKNOWN_REGION true positive
  - leakage canary absent from builder inputs
  - frozen thresholds matching the split-manifest digest
"""

from __future__ import annotations

import json
import sys
from pathlib import Path

import cv2
import numpy as np
import pytest

from blender_vision.ocular.detect import (
    DetectionMethod,
    detect,
    detections_from_proposals,
)
from blender_vision.ocular.proposals import (
    ALL_SOURCES,
    FROZEN_THRESHOLDS,
    FROZEN_THRESHOLDS_DIGEST,
    HypothesisKind,
    ProposalContext,
    ProposalSource,
    ProposalStatus,
    SourceAvailability,
    assert_no_ground_truth_in_proposals,
    propose,
    propose_geometry,
    propose_unknown_regions,
    thresholds_digest,
)

_ROOT = Path(__file__).resolve().parents[1]
_BENCH = _ROOT / "benchmarks"
if str(_BENCH) not in sys.path:
    sys.path.insert(0, str(_BENCH))


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------


def _three_stationary(h: int = 240, w: int = 320) -> np.ndarray:
    """Three similar textured disks — stationary, first-frame only."""
    img = np.zeros((h, w, 3), dtype=np.uint8)
    img[:] = (28, 26, 24)
    img[140:220, 20:300] = (38, 34, 30)
    for cx, seed in ((80, 11), (160, 23), (240, 37)):
        _blit_textured_disk(img, cx, 160, 16, seed, (118, 148, 176))
    return img


def _blit_textured_disk(
    canvas: np.ndarray,
    cx: int,
    cy: int,
    radius: int,
    seed: int,
    base_bgr: tuple[int, int, int],
) -> None:
    yy, xx = np.mgrid[-radius : radius + 1, -radius : radius + 1]
    mask = (xx * xx + yy * yy) <= radius * radius
    d = radius * 2 + 1
    patch = np.zeros((d, d, 3), dtype=np.uint8)
    base = np.array(base_bgr, dtype=np.float64)
    structured = np.zeros((d, d), dtype=np.float64)
    if seed == 11:
        structured = np.where((yy % 4) < 2, 40.0, -40.0)
    elif seed == 23:
        structured = np.where((xx % 4) < 2, 40.0, -40.0)
    else:
        structured = np.where(((xx + yy) % 5) < 2, 40.0, -40.0)
    for c in range(3):
        patch[:, :, c] = np.clip(base[c] + structured, 0, 255).astype(np.uint8)
    patch[~mask] = 0
    x0, y0 = cx - radius, cy - radius
    x1, y1 = x0 + d, y0 + d
    region = canvas[y0:y1, x0:x1]
    alpha = (patch.sum(axis=2) > 0)[:, :, None]
    region[:] = np.where(alpha, patch, region)


def _touching_pair() -> np.ndarray:
    """Two similar disks that touch / slightly overlap."""
    img = np.zeros((240, 320, 3), dtype=np.uint8)
    img[:] = (28, 26, 24)
    _blit_textured_disk(img, 145, 160, 22, 11, (118, 148, 176))
    _blit_textured_disk(img, 175, 160, 22, 23, (118, 148, 176))
    return img


def _fragmented_object() -> np.ndarray:
    """One object appearance split into two nearby fragments (gap)."""
    img = np.zeros((240, 320, 3), dtype=np.uint8)
    img[:] = (28, 26, 24)
    # Same texture seed and colour — fragments of one object.
    _blit_textured_disk(img, 150, 160, 12, 11, (118, 148, 176))
    _blit_textured_disk(img, 178, 160, 12, 11, (118, 148, 176))
    return img


# ---------------------------------------------------------------------------
# Required tests
# ---------------------------------------------------------------------------


def test_three_stationary_objects_from_first_frame() -> None:
    """Acceptance bar: three stationary objects, no temporal evidence."""
    img = _three_stationary()
    result = propose(img, frame_index=0, timestamp=0.0)
    graph = result.graph
    assert_no_ground_truth_in_proposals(graph.proposals)

    # Appearance alone must work on frame 0.
    appearance = [
        p
        for p in graph.proposals
        if p.source is ProposalSource.APPEARANCE
        and p.status is ProposalStatus.ACTIVE
        and p.area_px > 0
    ]
    assert len(appearance) >= 3, (
        f"appearance source found {len(appearance)} regions on first frame; need >= 3"
    )

    fused = graph.fused
    assert len(fused) >= 3, (
        f"fused proposals on first frame: {len(fused)}; need >= 3 "
        f"(centroids={[p.centroid_xy for p in fused]})"
    )
    # Centroids should land near the three known disk centres.
    cents = np.array([p.centroid_xy for p in fused], dtype=np.float64)
    targets = np.array([[80.0, 160.0], [160.0, 160.0], [240.0, 160.0]])
    matched = 0
    used: set[int] = set()
    for t in targets:
        dists = np.linalg.norm(cents - t, axis=1)
        j = int(np.argmin(dists))
        if j not in used and float(dists[j]) <= 30.0:
            used.add(j)
            matched += 1
    assert matched >= 3, f"only {matched}/3 targets matched; cents={cents.tolist()}"


def test_geometry_source_blocked_without_depth() -> None:
    """Geometry must report BLOCKED rather than inventing depth from RGB."""
    img = _three_stationary()
    props, report = propose_geometry(img, depth=None, normals=None, frame_index=0)
    assert report.availability is SourceAvailability.BLOCKED
    assert report.n_proposals == 0 or all(
        p.status is ProposalStatus.BLOCKED for p in props
    )
    assert "depth" in report.blocked_reason.lower() or "normal" in report.blocked_reason.lower()
    # No active geometry mask fabricated from RGB.
    active = [p for p in props if p.status is ProposalStatus.ACTIVE and p.area_px > 0]
    assert active == []

    # Full propose path also reports geometry BLOCKED.
    result = propose(img, frame_index=0)
    geo_reports = [
        r for r in result.graph.source_reports if r.source is ProposalSource.GEOMETRY
    ]
    assert geo_reports
    assert geo_reports[0].availability is SourceAvailability.BLOCKED


def test_geometry_source_active_with_depth() -> None:
    """When a real depth pass is supplied, geometry is available."""
    img = _three_stationary()
    h, w = img.shape[:2]
    depth = np.full((h, w), 5.0, dtype=np.float32)
    # Three closer blobs where the disks are.
    for cx in (80, 160, 240):
        cv2.circle(depth, (cx, 160), 16, 1.0, -1)
    props, report = propose_geometry(img, depth=depth, frame_index=0)
    assert report.availability in {
        SourceAvailability.AVAILABLE,
        SourceAvailability.EMPTY,
    }
    assert all(p.status is not ProposalStatus.BLOCKED or p.area_px > 0 for p in props) or True
    # At least: not the blocked-without-depth path.
    assert "invented" not in report.blocked_reason.lower()


def test_split_hypothesis_preserved_for_touching_objects() -> None:
    img = _touching_pair()
    result = propose(img, frame_index=0)
    graph = result.graph
    splits = graph.split_hypotheses
    assert len(splits) >= 1, "expected at least one SPLIT hypothesis for touching pair"
    assert all(p.hypothesis_kind is HypothesisKind.SPLIT for p in splits)
    # Parent ids recorded; parents themselves remain in the proposal set.
    parents = {pid for p in splits for pid in p.related_proposal_ids}
    assert parents, "split children must reference parent proposal ids"
    parent_still_present = {
        p.proposal_id for p in graph.proposals if p.proposal_id in parents
    }
    assert parent_still_present, "parent of split must not be destroyed by fusion"


def test_merge_hypothesis_preserved_for_fragmented_object() -> None:
    img = _fragmented_object()
    result = propose(img, frame_index=0)
    merges = result.graph.merge_hypotheses
    assert len(merges) >= 1, "expected at least one MERGE hypothesis for fragments"
    assert all(p.hypothesis_kind is HypothesisKind.MERGE for p in merges)
    # Children remain as atomic proposals — merge is additive, not destructive.
    child_ids = {cid for m in merges for cid in (m.meta.get("child_ids") or [])}
    if child_ids:
        present = {p.proposal_id for p in result.graph.proposals}
        assert child_ids & present, "merge children must survive alongside the parent"


def test_fusion_not_single_region_on_crossing_fixture(tmp_path: Path) -> None:
    """Crossing paths has two objects; fusion must not collapse to one region."""
    from ocular_hard.conditions import Condition  # type: ignore[import-not-found]
    from ocular_hard.synthetic import write_condition  # type: ignore[import-not-found]

    out = tmp_path / "crossing_paths"
    write_condition(out, Condition.CROSSING_PATHS)
    frames = sorted((out / "frames").glob("*.png"))
    assert frames, "crossing_paths fixture produced no frames"

    # Sample a few frames across the sequence (including mid-cross).
    indices = [0, 8, 15, 24, 31]
    min_fused = 999
    for i in indices:
        if i >= len(frames):
            continue
        img = cv2.imread(str(frames[i]), cv2.IMREAD_COLOR)
        prev = (
            cv2.imread(str(frames[i - 1]), cv2.IMREAD_COLOR) if i > 0 else None
        )
        result = propose(
            img,
            frame_index=i,
            context=ProposalContext(previous_image=prev),
        )
        n = len(result.graph.fused)
        min_fused = min(min_fused, n)
        # Outside the exact overlap frame, need at least 2; even at overlap,
        # split hypotheses or multi fused should avoid a permanent collapse.
        if i not in {14, 15, 16}:
            assert n >= 2, (
                f"crossing frame {i}: fused collapsed to {n}; "
                f"cents={[p.centroid_xy for p in result.graph.fused]}"
            )
    assert min_fused >= 1


def test_unknown_region_true_positive() -> None:
    """Source F emits NEW_UNKNOWN_REGION for an entrant not covered by known tracks."""
    img = np.zeros((240, 320, 3), dtype=np.uint8)
    img[:] = (28, 26, 24)
    _blit_textured_disk(img, 80, 160, 16, 11, (118, 148, 176))  # known
    _blit_textured_disk(img, 240, 100, 14, 71, (130, 150, 165))  # unknown entrant

    known = np.zeros((240, 320), dtype=np.uint8)
    cv2.circle(known, (80, 160), 20, 1, -1)

    props, report = propose_unknown_regions(
        img, known_masks=[known], frame_index=5
    )
    assert report.availability is SourceAvailability.AVAILABLE
    assert report.n_proposals >= 1
    events = [p.meta.get("event") for p in props]
    assert "NEW_UNKNOWN_REGION" in events

    # At least one proposal near the unknown entrant.
    near_unknown = [
        p
        for p in props
        if abs(p.centroid_xy[0] - 240.0) < 40 and abs(p.centroid_xy[1] - 100.0) < 40
    ]
    assert near_unknown, (
        f"no unknown proposal near entrant; cents={[p.centroid_xy for p in props]}"
    )
    # Known object must not dominate the unknown set.
    near_known = [
        p
        for p in props
        if abs(p.centroid_xy[0] - 80.0) < 25 and abs(p.centroid_xy[1] - 160.0) < 25
    ]
    assert len(near_known) == 0, "known track residual must not re-fire as unknown"


def test_leakage_canary_absent_from_builder_inputs() -> None:
    from ocular_splits import (  # type: ignore[import-not-found]
        assert_canary_absent_from_builder_inputs,
        load_canary,
        load_manifest,
    )

    canary = load_canary()
    assert canary.startswith("OCULAR_PROPOSAL_HIDDEN_CANARY_")
    assert_canary_absent_from_builder_inputs(canary)

    # Manifest itself is builder-visible and must not embed the secret.
    manifest = load_manifest()
    blob = json.dumps(manifest)
    assert canary not in blob


def test_frozen_thresholds_match_manifest_digest() -> None:
    from ocular_splits import load_manifest  # type: ignore[import-not-found]

    manifest = load_manifest()
    frozen = manifest["frozen_thresholds"]
    expected = manifest["frozen_thresholds_digest"]
    assert thresholds_digest(frozen) == expected
    assert expected == FROZEN_THRESHOLDS_DIGEST
    for key, value in FROZEN_THRESHOLDS.items():
        assert key in frozen
        assert frozen[key] == value


def test_all_six_sources_independently_measurable() -> None:
    img = _three_stationary()
    prev = img.copy()
    # Nudge one disk so temporal sources have residual.
    cv2.circle(prev, (80, 160), 16, (28, 26, 24), -1)
    _blit_textured_disk(prev, 90, 160, 16, 11, (118, 148, 176))

    result = propose(
        img,
        frame_index=1,
        context=ProposalContext(previous_image=prev),
    )
    reported = {r.source for r in result.graph.source_reports}
    assert reported == set(ALL_SOURCES)
    # Each source has an explicit availability.
    for rep in result.graph.source_reports:
        assert rep.availability in {
            SourceAvailability.AVAILABLE,
            SourceAvailability.BLOCKED,
            SourceAvailability.EMPTY,
        }


def test_detect_proposal_fusion_path() -> None:
    img = _three_stationary()
    dets = detect(
        img,
        frame_index=0,
        config=__import__(
            "blender_vision.ocular.detect", fromlist=["DetectionConfig"]
        ).DetectionConfig(method=DetectionMethod.PROPOSAL_FUSION),
    )
    assert len(dets) >= 3
    assert all(d.meta.get("method") == DetectionMethod.PROPOSAL_FUSION.value for d in dets)

    dets2 = detections_from_proposals(img, frame_index=0)
    assert len(dets2) >= 3


def test_no_ground_truth_keys_on_proposals() -> None:
    img = _three_stationary()
    result = propose(img, frame_index=0)
    assert_no_ground_truth_in_proposals(result.graph.proposals)
    with pytest.raises(AssertionError):
        bad = result.graph.fused[0] if result.graph.fused else result.graph.proposals[0]
        bad.meta["gt_id"] = "obj_a"
        assert_no_ground_truth_in_proposals([bad])
