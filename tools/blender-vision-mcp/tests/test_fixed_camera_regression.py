from __future__ import annotations

import json
import uuid
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.regression import FixedCameraRegressionEvaluator
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.util import utc_now
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore


def _manual_cameras(reference_ids: list[str]) -> list[dict]:
    return [
        {
            "reference_id": reference_id,
            "model": "PINHOLE",
            "width": 96,
            "height": 72,
            "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 48.0, "cy": 36.0},
            "world_from_camera": [
                [1.0, 0.0, 0.0, float(index * 100)],
                [0.0, 1.0, 0.0, 500.0],
                [0.0, 0.0, 1.0, 50.0],
                [0.0, 0.0, 0.0, 1.0],
            ],
            "confidence": 0.6,
            "registration_class": "approximate_visual_registration",
            "evidence_class": "MULTI_VIEW_OBSERVED",
        }
        for index, reference_id in enumerate(reference_ids)
    ]


def _regression_project(
    tmp_path: Path, *, changed_camera: bool = False
) -> tuple[ProjectStore, dict, dict]:
    project = ProjectStore.create(tmp_path / "project", "Fixed-camera regression")
    scenes = SceneStore(project)
    baseline_path = tmp_path / "baseline.blend"
    candidate_path = tmp_path / "candidate.blend"
    baseline_path.write_bytes(b"baseline scene")
    candidate_path.write_bytes(b"candidate scene")
    baseline = scenes.import_blend(baseline_path)
    candidate = scenes.import_blend(candidate_path)
    references = []
    for index in range(2):
        source = tmp_path / f"reference-{index}.png"
        Image.new("RGB", (96, 72), (60 + index * 20, 60, 60)).save(source)
        references.append(
            ReferenceIngestor(project).import_file(
                source,
                rights_state="SYNTHETIC_OWNED",
                viewpoint_label=f"view-{index}",
            )
        )
    reference_ids = [item["id"] for item in references]
    camera = CameraSolver(project).import_manual(_manual_cameras(reference_ids))
    candidate_camera = (
        CameraSolver(project).import_manual(_manual_cameras(reference_ids))
        if changed_camera
        else camera
    )
    artifacts = ArtifactStore(project)
    render_outputs: dict[str, list[dict]] = {baseline["id"]: [], candidate["id"]: []}
    comparison_rows = []
    for index, reference in enumerate(references):
        for scene, score, shade in (
            (baseline, 0.92 - index * 0.01, 130),
            (candidate, 0.84 - index * 0.01, 100),
        ):
            render_path = tmp_path / f"{scene['id']}-{index}-render.png"
            residual_path = tmp_path / f"{scene['id']}-{index}-residual.png"
            Image.new("RGB", (96, 72), (shade, shade, shade)).save(render_path)
            Image.new("RGB", (96, 72), (20, 20, 20)).save(residual_path)
            render = artifacts.ingest_file(render_path, media_type="image/png")
            residual = artifacts.ingest_file(residual_path, media_type="image/png")
            render_outputs[scene["id"]].append(
                {
                    "reference_id": reference["id"],
                    "artifact": render.to_dict(),
                    "pass_artifact_digests": {"beauty": render.digest},
                }
            )
            comparison_rows.append(
                (
                    str(uuid.uuid4()),
                    reference["id"],
                    render.digest,
                    residual.digest,
                    json.dumps(
                        {
                            "silhouette_iou": score,
                            "reference_partial_object_crop": False,
                        }
                    ),
                    utc_now(),
                )
            )
    now = utc_now()
    with project.connection() as connection:
        for scene, solution in ((baseline, camera), (candidate, candidate_camera)):
            connection.execute(
                "INSERT INTO render_runs(id,scene_id,camera_solution_id,config_json,"
                "outputs_json,created_at) VALUES(?,?,?,?,?,?)",
                (
                    str(uuid.uuid4()),
                    scene["id"],
                    solution["id"],
                    "{}",
                    json.dumps(render_outputs[scene["id"]]),
                    now,
                ),
            )
        connection.executemany(
            "INSERT INTO comparisons(id,reference_id,render_digest,residual_digest,metrics_json,"
            "created_at) VALUES(?,?,?,?,?,?)",
            comparison_rows,
        )
    return project, baseline, candidate


def test_fixed_camera_regression_evidence_auto_rejects_without_acceptance_claim(
    tmp_path: Path,
) -> None:
    project, baseline, candidate = _regression_project(tmp_path)

    result = FixedCameraRegressionEvaluator(project).evaluate(
        baseline_scene_id=baseline["id"],
        candidate_scene_id=candidate["id"],
        minimum_views=2,
    )

    assert result["candidate_state"] == "REJECTED"
    assert result["accepted"] is False
    assert result["report"]["view_count"] == 2
    assert result["report"]["regressed_view_count"] == 2
    assert result["report"]["candidate_mean"] < result["report"]["baseline_mean"]
    assert result["transaction"]["status"] == "FAILED"
    assert result["transaction"]["baseline_scene_id"] is None
    assert result["transaction"]["automatic_rejection"]["to_state"] == "REJECTED"
    assert any(
        item["category"] == "appearance" and item["status"] == "FAIL"
        for item in result["transaction"]["gates"]
    )


def test_fixed_camera_regression_refuses_changed_camera_solution(tmp_path: Path) -> None:
    project, baseline, candidate = _regression_project(tmp_path, changed_camera=True)

    with pytest.raises(ValueError, match="camera solution changed"):
        FixedCameraRegressionEvaluator(project).evaluate(
            baseline_scene_id=baseline["id"],
            candidate_scene_id=candidate["id"],
        )

    assert SceneStore(project).get(candidate["id"])["state"] == "DRAFT"
