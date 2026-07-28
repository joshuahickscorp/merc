"""Phase T — ocular repair corpus tests."""

from __future__ import annotations

from pathlib import Path

from blender_vision.ocular.repair import (
    DrillStatus,
    repair_corpus_drill_ids,
    run_ocular_repair_corpus,
    run_repair_drill,
)

REQUIRED_DRILLS = [
    "sensor-wrong-intrinsics",
    "sensor-time-skew",
    "sensor-colour-mismatch",
    "sensor-axis-mismatch",
    "tracking-identity-swap",
    "tracking-occlusion-loss",
    "tracking-false-reid",
    "world-erased-object",
    "world-cross-session-mismatch",
    "geometry-scale-error",
    "geometry-hidden-surface-hallucination",
    "geometry-coordinate-frame-error",
    "material-roughness-error",
    "material-plastic-metal",
    "material-lighting-confusion",
    "browser-focus-trap",
    "browser-scroll-lag",
    "browser-blank-first-frame",
    "browser-dom-pixel-contradiction",
    "cinematic-empty-beat",
    "cinematic-text-collision",
    "cinematic-bad-turn",
    "cinematic-camera-freeze",
]


def test_all_required_drills_registered() -> None:
    ids = set(repair_corpus_drill_ids())
    for drill_id in REQUIRED_DRILLS:
        assert drill_id in ids, drill_id


def test_each_drill_detect_repair_verify_no_regression() -> None:
    for drill_id in REQUIRED_DRILLS:
        result = run_repair_drill(drill_id)
        assert result.detector_fired, f"{drill_id} detector silent"
        assert result.repaired, f"{drill_id} repair failed"
        assert result.acceptance_passed, f"{drill_id} acceptance failed"
        assert result.global_regression is False, f"{drill_id} regressed"
        assert result.status is DrillStatus.PASS, f"{drill_id} status={result.status}"


def test_corpus_receipt(tmp_path: Path) -> None:
    receipt = run_ocular_repair_corpus(tmp_path)
    assert receipt.failed_count == 0
    assert receipt.passed_count == len(REQUIRED_DRILLS)
    assert receipt.status == "PASS"
    assert (tmp_path / "repair.receipt.json").is_file()
    assert (tmp_path / "repair.matrix.txt").is_file()


def test_only_filter(tmp_path: Path) -> None:
    receipt = run_ocular_repair_corpus(tmp_path, only=["sensor-axis-mismatch"])
    assert receipt.drill_count == 1
    assert receipt.drills[0].drill_id == "sensor-axis-mismatch"
    assert receipt.status == "PASS"
