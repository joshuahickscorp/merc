from __future__ import annotations

from pathlib import Path

from PIL import Image

from blender_vision.cameras.consensus import CameraConsensus
from blender_vision.cameras.solver import CameraSolver
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def _camera(reference_id: str, x: float) -> dict[str, object]:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 100,
        "height": 80,
        "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, x],
            [0.0, 1.0, 0.0, 500.0],
            [0.0, 0.0, 1.0, 50.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.4,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "INFERRED_LOW_CONFIDENCE",
        "diagnostics": {},
    }


def test_camera_consensus_ranks_but_does_not_average_approximate_hypotheses(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera consensus")
    source = tmp_path / "view.png"
    Image.new("RGB", (100, 80), "white").save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    first = CameraSolver(project).import_manual([_camera(reference["id"], 0.0)])
    second = CameraSolver(project).import_manual([_camera(reference["id"], 10.0)])
    CameraSolver(project).approve(
        second["id"], reviewer="Calibration reviewer", reason="Preferred initialization"
    )

    result = CameraConsensus(project).compare([first["id"], second["id"]])

    assert result["report"]["selected_solution_id"] == second["id"]
    assert result["report"]["selected_is_approved_metric"] is False
    assert result["report"]["averaging_performed"] is False
    assert result["report"]["pairwise"][0]["metric_frame_compatible"] is False
    assert result["report"]["pairwise"][0]["camera_center_rmse_mm"] is None
    assert CameraConsensus(project).latest()["id"] == result["id"]
