"""Calibration tests for the 9×4 ocular-event detector matrix.

Each of the nine detectors must fire on its true positive, stay silent on its
true negative and confounder, keep camera/object separation, define
NEW_UNKNOWN_REGION with a real true positive, and expose monotone confidence.
"""

from __future__ import annotations

import importlib.util
from pathlib import Path

import cv2
import numpy as np
import pytest

from blender_vision.ocular.retina import (
    CAMERA_COVERAGE_MIN,
    RetinaState,
    dense_optical_flow,
    process_frame,
    separate_camera_object_motion,
)

REPO = Path(__file__).resolve().parents[1]
PROCEDURAL = REPO / "benchmarks" / "ocular_events" / "procedural.py"

CALIBRATED_EVENTS = (
    "OBJECT_MOVED",
    "CAMERA_MOVED",
    "OBJECT_ENTERED",
    "OBJECT_LEFT",
    "OBJECT_OCCLUDED",
    "OBJECT_REAPPEARED",
    "NEW_UNKNOWN_REGION",
    "LIGHT_CHANGED",
    "SURFACE_CHANGED",
)


def _load_procedural():
    spec = importlib.util.spec_from_file_location("ocular_events_procedural", PROCEDURAL)
    assert spec is not None and spec.loader is not None
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture(scope="module")
def matrix(tmp_path_factory: pytest.TempPathFactory) -> Path:
    root = tmp_path_factory.mktemp("ocular_events")
    mod = _load_procedural()
    mod.generate_all(root)
    return root


def _run(frames_dir: Path) -> set[str]:
    state = RetinaState()
    types: set[str] = set()
    confidences: list[tuple[str, float]] = []
    for path in sorted(frames_dir.glob("frame_*.png")):
        image = cv2.imread(str(path), cv2.IMREAD_COLOR)
        assert image is not None
        analysis = process_frame(image, state=state)
        for event in analysis.events:
            types.add(event.event_type)
            confidences.append((event.event_type, event.confidence))
    return types


def _run_with_conf(frames_dir: Path) -> list[tuple[str, float]]:
    state = RetinaState()
    out: list[tuple[str, float]] = []
    for path in sorted(frames_dir.glob("frame_*.png")):
        image = cv2.imread(str(path), cv2.IMREAD_COLOR)
        analysis = process_frame(image, state=state)
        for event in analysis.events:
            out.append((event.event_type, float(event.confidence)))
    return out


@pytest.mark.parametrize("event", CALIBRATED_EVENTS)
def test_true_positive_fires(matrix: Path, event: str) -> None:
    types = _run(matrix / event.lower() / "true_positive" / "frames")
    assert event in types, f"{event} silent on true_positive; saw {sorted(types)}"


@pytest.mark.parametrize("event", CALIBRATED_EVENTS)
def test_true_negative_silent(matrix: Path, event: str) -> None:
    types = _run(matrix / event.lower() / "true_negative" / "frames")
    assert event not in types, f"{event} fired on true_negative; saw {sorted(types)}"


@pytest.mark.parametrize("event", CALIBRATED_EVENTS)
def test_confounder_silent(matrix: Path, event: str) -> None:
    types = _run(matrix / event.lower() / "confounder" / "frames")
    assert event not in types, f"{event} fired on confounder; saw {sorted(types)}"


def test_new_unknown_region_true_positive(matrix: Path) -> None:
    types = _run(matrix / "new_unknown_region" / "true_positive" / "frames")
    assert "NEW_UNKNOWN_REGION" in types
    # Must be residual-based novelty, not merely a camera pan byproduct.
    pan_types = _run(matrix / "new_unknown_region" / "confounder" / "frames")
    assert "NEW_UNKNOWN_REGION" not in pan_types


def test_camera_object_separation_preserved(matrix: Path) -> None:
    """Coverage rule must not regress: pan → CAMERA, not OBJECT; object → reverse."""
    cam_types = _run(matrix / "camera_moved" / "true_positive" / "frames")
    assert "CAMERA_MOVED" in cam_types
    assert "OBJECT_MOVED" not in cam_types

    obj_types = _run(matrix / "object_moved" / "true_positive" / "frames")
    assert "OBJECT_MOVED" in obj_types
    assert "CAMERA_MOVED" not in obj_types

    # Direct flow measurement still separates by coverage.
    rng = np.random.default_rng(2)
    base = rng.integers(40, 100, size=(160, 200, 3), dtype=np.uint8)
    M = np.float32([[1, 0, 8], [0, 1, 0]])
    pan = cv2.warpAffine(base, M, (base.shape[1], base.shape[0]), borderMode=cv2.BORDER_REFLECT)
    prev = cv2.cvtColor(base, cv2.COLOR_BGR2GRAY)
    curr = cv2.cvtColor(pan, cv2.COLOR_BGR2GRAY)
    result = separate_camera_object_motion(dense_optical_flow(prev, curr))
    assert result.moving_fraction > CAMERA_COVERAGE_MIN


def test_confidence_calibration_monotone(matrix: Path) -> None:
    """Higher confidence should not be systematically less accurate.

    Build (confidence, correct) pairs from the matrix: a fire on TP/near is
    correct; a fire on TN/confounder is incorrect. Group by coarse bins and
    require non-decreasing accuracy across non-empty bins.
    """
    samples: list[tuple[float, bool]] = []
    for event in CALIBRATED_EVENTS:
        for cls, correct_if_fire in (
            ("true_positive", True),
            ("near_threshold", True),
            ("true_negative", False),
            ("confounder", False),
        ):
            for etype, conf in _run_with_conf(matrix / event.lower() / cls / "frames"):
                if etype != event:
                    continue
                samples.append((conf, correct_if_fire))

    assert samples, "no confidence samples collected"
    bins = [(0.0, 0.4), (0.4, 0.7), (0.7, 1.01)]
    accs: list[float] = []
    for lo, hi in bins:
        in_bin = [ok for c, ok in samples if lo <= c < hi]
        if not in_bin:
            continue
        accs.append(sum(1 for ok in in_bin if ok) / len(in_bin))
    # Allow flat; forbid a clear inversion of more than 0.25.
    for i in range(len(accs) - 1):
        assert accs[i + 1] + 0.25 >= accs[i], (
            f"confidence calibration inverted: {accs}"
        )


def test_near_threshold_cells_exist(matrix: Path) -> None:
    for event in CALIBRATED_EVENTS:
        path = matrix / event.lower() / "near_threshold" / "manifest.json"
        assert path.is_file(), f"missing near_threshold fixture for {event}"


def test_matrix_is_complete(matrix: Path) -> None:
    for event in CALIBRATED_EVENTS:
        for cls in ("true_positive", "true_negative", "near_threshold", "confounder"):
            frames = list((matrix / event.lower() / cls / "frames").glob("frame_*.png"))
            assert len(frames) >= 2, f"{event}/{cls} has too few frames"
