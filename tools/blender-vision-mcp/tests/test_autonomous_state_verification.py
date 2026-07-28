from __future__ import annotations

import json
import uuid
from pathlib import Path

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.passes import GOVERNED_RENDER_PASSES
from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.metrics import compare_silhouettes
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import utc_now
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _metric_camera(reference_id: str) -> dict:
    return {
        "reference_id": reference_id,
        "model": "PINHOLE",
        "width": 96,
        "height": 64,
        "intrinsics": {"fx": 80.0, "fy": 80.0, "cx": 48.0, "cy": 32.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, 500.0],
            [0.0, 0.0, 1.0, 50.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.9,
        "registration_class": "metric_camera_solution",
        "evidence_class": "MULTI_VIEW_OBSERVED",
        "diagnostics": {
            "quality": {
                "reprojection_rmse_px": 0.25,
                "registered_feature_count": 120,
                "view_coverage": 1.0,
                "baseline_diversity": 0.8,
                "scale_confidence": 1.0,
                "principal_point_confidence": 0.9,
                "distortion_confidence": 0.9,
            }
        },
    }


def _project_with_metric_camera(tmp_path: Path, *, approve: bool) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Verified autonomy state")
    image_path = tmp_path / "reference.png"
    Image.new("RGBA", (96, 64), (80, 80, 80, 255)).save(image_path)
    reference = ReferenceIngestor(project).import_file(
        image_path, rights_state="SYNTHETIC_OWNED"
    )
    scale = MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 100.0},
        evidence_class=EvidenceClass.MEASURED,
        certainty="exact",
    )
    solution = CameraSolver(project).import_manual(
        [_metric_camera(reference["id"])], evidence_binding_ids=[scale["id"]]
    )
    if approve:
        solution = CameraSolver(project).approve(
            solution["id"],
            reviewer="Camera verifier",
            reason="Metric fixture camera reviewed",
        )
    return project, solution


def test_executor_facts_reject_forged_camera_evaluation_and_promotion(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_metric_camera(tmp_path, approve=False)
    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"candidate")
    scene = SceneStore(project).import_blend(scene_path)
    with project.connection() as connection:
        document = json.loads(
            connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?", (solution["id"],)
            ).fetchone()[0]
        )
        document["approved"] = True
        document["approval"] = {
            "state": "approved",
            "reviewer": "Forged reviewer",
            "reason": "Direct database edit",
            "reviewed_at": utc_now(),
        }
        connection.execute(
            "UPDATE camera_solutions SET approved=1,solution_json=? WHERE id=?",
            (json.dumps(document), solution["id"]),
        )
        evaluation_id = str(uuid.uuid4())
        connection.execute(
            "INSERT INTO candidate_evaluations(id,scene_id,baseline_scene_id,status,"
            "gates_json,metrics_json,regressions_json,receipt_digest,created_at) "
            "VALUES(?,?,?,?,?,?,?,?,?)",
            (
                evaluation_id,
                scene["id"],
                None,
                "PASSED",
                "[]",
                "{}",
                "[]",
                scene["artifact"]["digest"],
                utc_now(),
            ),
        )
        connection.execute(
            "UPDATE scene_assets SET state='PROMOTED',is_authoritative=1 WHERE id=?",
            (scene["id"],),
        )

    facts = AutonomousWorkflowExecutor(project)._facts()

    assert facts["approved_metric_camera_solution_count"] == 0
    assert facts["passed_candidate_evaluation_count"] == 0
    assert facts["promoted_scene_count"] == 0
    assert facts["promoted_scene_id"] is None
    assert facts["verification"]["invalid_evaluation_ids"] == [evaluation_id]


def test_executor_facts_require_complete_artifact_valid_render_and_comparison(
    tmp_path: Path,
) -> None:
    project, solution = _project_with_metric_camera(tmp_path, approve=True)
    scene_path = tmp_path / "candidate.blend"
    scene_path.write_bytes(b"candidate")
    scene = SceneStore(project).import_blend(scene_path)
    image_path = tmp_path / "render.png"
    Image.new("RGBA", (96, 64), (90, 90, 90, 255)).save(image_path)
    artifact = ArtifactStore(project).ingest_file(image_path, media_type="image/png")
    residual_path = tmp_path / "residual.png"
    metrics = compare_silhouettes(
        tmp_path / "reference.png", image_path, residual_path
    )
    residual = ArtifactStore(project).ingest_file(residual_path, media_type="image/png")
    reference_id = solution["cameras"][0]["reference_id"]
    render_id = str(uuid.uuid4())
    comparison_id = str(uuid.uuid4())
    outputs = [
        {
            "reference_id": reference_id,
            "artifact_digest": artifact.digest,
            "pass_artifact_digests": {
                name: artifact.digest for name in GOVERNED_RENDER_PASSES
            },
            "relative_path": "renders/missing.png",
        }
    ]
    ComparisonStore(project).record(
        comparison_id,
        reference_id=reference_id,
        render_digest=artifact.digest,
        residual_digest=residual.digest,
        metrics=metrics,
        engine="compare_silhouettes_v2",
    )
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO render_runs(id,scene_id,camera_solution_id,config_json,outputs_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            (render_id, scene["id"], solution["id"], "{}", json.dumps(outputs), utc_now()),
        )

    incomplete = AutonomousWorkflowExecutor(project)._facts()
    assert incomplete["mandatory_render_suite_complete"] is False
    assert incomplete["comparison_coverage_complete"] is False
    assert incomplete["render_run_count"] == 0
    assert incomplete["comparison_count"] == 0

    materialized = project.root / "renders" / "verified.png"
    ArtifactStore(project).materialize(artifact.digest, materialized)
    outputs[0]["relative_path"] = str(materialized.relative_to(project.root))
    with project.connection() as connection:
        connection.execute(
            "UPDATE render_runs SET outputs_json=? WHERE id=?",
            (json.dumps(outputs), render_id),
        )

    complete = AutonomousWorkflowExecutor(project)._facts()
    assert complete["mandatory_render_suite_complete"] is True
    assert complete["comparison_coverage_complete"] is True
    assert complete["verification"]["verified_render_run_ids"] == [render_id]
    assert complete["verification"]["valid_comparison_ids"] == [comparison_id]
