from __future__ import annotations

import json
import os
from pathlib import Path

import pytest

from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.benchmarks.calibration import bootstrap_calibration
from blender_vision.projects.store import ProjectStore


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender calibration",
)
def test_public_calibration_benchmark_accepts_every_gate(tmp_path: Path) -> None:
    result = bootstrap_calibration(
        tmp_path / "calibration",
        reviewer="Automated calibration integration reviewer",
        review_reason="Reviewed procedural ground truth and all deterministic validation gates",
    )
    assert result["accepted_fidelity"] == "L3"
    assert result["calibration"]["passed"] is True
    assert set(result["calibration"]["gates"]) == {
        "known_dimensions",
        "camera_recovery",
        "scale_recovery",
        "repeatability",
        "export_consistency",
    }
    assert all(gate["passed"] for gate in result["calibration"]["gates"].values())
    assert (
        min(
            comparison["metrics"]["silhouette_iou"]
            for comparison in result["comparison"]["comparisons"]
        )
        >= 0.95
    )
    assert result["coverage"]["comparison_coverage"] == 1.0
    assert len(result["exports"]) == 3
    assert result["exports"][0]["artifact"]["digest"] == result["exports"][1]["artifact"]["digest"]

    project = ProjectStore.open(Path(result["project"]))
    assert project.project()["accepted_fidelity"] == "L3"
    receipt = result["receipt"]
    assert receipt["acceptance"]["accepted"] is True
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True
    assert (project.root / receipt["human_path"]).is_file()
    envelope = json.loads((project.root / receipt["path"]).read_text(encoding="utf-8"))
    evidence = envelope["payload"]["evidence"]
    assert len(evidence["render_runs"]) == 2
    required_passes = {
        "beauty",
        "appearance",
        "exposure_minus_2",
        "exposure_0",
        "exposure_plus_2",
        "material_neutral",
        "silhouette",
        "depth",
        "normal",
        "object_id",
        "feature_id",
    }
    assert all(
        set(output["pass_artifact_digests"]) >= required_passes
        for run in evidence["render_runs"]
        for output in run["outputs"]
    )
    assert len(evidence["exports"]) == 3
    assert {item["format"] for item in evidence["exports"]} == {"blend", "glb"}
    assert evidence["calibration_runs"][-1]["gates"]["passed"] is True
