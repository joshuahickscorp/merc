"""Tests for the VisionMCP V2 full-runtime repair corpus."""

from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.benchmarks.repair_corpus import (
    get_drill,
    repair_corpus_drill_ids,
    repair_corpus_drills,
    run_repair_corpus,
    run_repair_drill,
)

REQUIRED_FAILURES = (
    # Geometry
    "wrong dimensions",
    "wrong hidden surface",
    "missing semantic part",
    "bad topology",
    "LOD identity mismatch",
    # Material
    "plastic metal",
    "wrong roughness",
    "flat fake foam",
    "texture scale error",
    "offline/browser mismatch",
    # Lighting
    "clipped hero",
    "floating contact",
    "wrong exposure",
    "flat corridor",
    "excessive glow",
    # Cinematic
    "delayed camera",
    "dead scroll",
    "text collision",
    "left-turn overshoot",
    "mobile crop",
    "reduced-motion regression",
    # Delivery
    "oversized shell",
    "decode long task",
    "memory growth",
    "blank first frame",
    "shader flash",
    "no-WebGL content loss",
)


def test_corpus_registers_every_named_failure() -> None:
    drills = repair_corpus_drills()
    classes = {d.failure_class for d in drills}
    missing = [name for name in REQUIRED_FAILURES if name not in classes]
    assert not missing, f"missing failure classes: {missing}"
    assert len(drills) == len(REQUIRED_FAILURES)
    ids = repair_corpus_drill_ids()
    assert len(ids) == len(set(ids))
    for drill in drills:
        assert drill.parameters
        assert drill.blast_radius
        assert drill.acceptance_test
        assert drill.finding_key
        assert drill.critic_role


@pytest.mark.parametrize("drill_id", list(repair_corpus_drill_ids()))
def test_each_drill_detector_fires_and_repair_accepts(drill_id: str, tmp_path: Path) -> None:
    """Prove inject → detector → bounded repair → acceptance without receipt replay.

    Uses force_measure_without_external so the live artifact+critic path is
    exercised even when Blender/browser cannot start in this environment. That
    does not claim those external runtimes succeeded.
    """
    drill = get_drill(drill_id)
    result = run_repair_drill(
        drill,
        tmp_path / "drills",
        force_measure_without_external=True,
    )
    assert result.detector_fired, (
        f"{drill_id}: detector did not fire on injection; "
        f"measured={result.measured_injected} notes={result.notes}"
    )
    assert result.acceptance_passed, (
        f"{drill_id}: acceptance failed after repair; "
        f"before={result.measured_injected} after={result.measured_repaired} "
        f"notes={result.notes}"
    )
    assert result.global_regression is False
    assert result.status == "PASS"
    assert result.failed_attempt_dir is not None
    failed = Path(result.failed_attempt_dir)
    assert failed.is_dir()
    assert (failed / "failing-measurement.json").is_file()
    assert (failed / "artifact.json").is_file()


def test_full_corpus_force_measure_passes(tmp_path: Path) -> None:
    receipt = run_repair_corpus(
        tmp_path / "corpus",
        force_measure_without_external=True,
    )
    assert receipt.failed_count == 0
    assert receipt.passed_count == len(REQUIRED_FAILURES)
    assert receipt.status == "PASS"
    assert (tmp_path / "corpus" / "repair-corpus.receipt.json").is_file()
    assert (tmp_path / "corpus" / "failed-attempts").is_dir()


def test_blocked_external_when_runtime_required(tmp_path: Path) -> None:
    """Without force-measure, external-required drills must not fake PASS."""
    # Pick a drill that requires Blender; in this sandbox Blender SIGSEGVs.
    drill = get_drill("geometry-wrong-dimensions")
    assert drill.requires_external and drill.external_kind == "blender"
    result = run_repair_drill(drill, tmp_path / "blocked")
    # Either PASS (if Blender works on the host) or honest BLOCKED_EXTERNAL.
    assert result.status in {"PASS", "BLOCKED_EXTERNAL"}
    if result.status == "BLOCKED_EXTERNAL":
        assert result.block_reason
        assert "PASS" not in result.block_reason
        # Injected failure is still preserved for the supervisor.
        assert result.failed_attempt_dir is not None
        assert Path(result.failed_attempt_dir).is_dir()


def test_run_repair_corpus_unknown_id_raises(tmp_path: Path) -> None:
    with pytest.raises(Exception, match="unknown drill"):
        run_repair_corpus(tmp_path, only=["not-a-real-drill"])
