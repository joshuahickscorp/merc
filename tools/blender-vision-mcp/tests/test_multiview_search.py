from __future__ import annotations

import json
import uuid
from pathlib import Path
from typing import Any

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.images import compare_project_renders
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import utc_now
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import SemanticTwinGraph
from blender_vision.optimization.search import MultiviewSearchStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _fixture(tmp_path: Path) -> tuple[ProjectStore, str, str, str]:
    project = ProjectStore.create(tmp_path / "project", "Multiview search")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    references = []
    for index, label in enumerate(("front", "rear")):
        image = Image.new("RGBA", (96, 72), (0, 0, 0, 0))
        ImageDraw.Draw(image).rectangle(
            (18 + index * 3, 14, 76 + index * 3, 60), fill=(180, 180, 180, 255)
        )
        path = tmp_path / f"{label}.png"
        image.save(path)
        references.append(
            ReferenceIngestor(project).import_file(
                path, rights_state="SYNTHETIC_OWNED", viewpoint_label=label
            )
        )
    camera = CameraSolver(project).solve("turntable_fallback")
    camera = CameraSolver(project).approve(
        camera["id"], reviewer="Fixture reviewer", reason="Fixed synthetic views."
    )
    blend = tmp_path / "baseline.blend"
    blend.write_bytes(b"authoritative baseline")
    scene = SceneStore(project).import_blend(blend)
    ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 10.0, "height_mm": 20.0},
        )
    )
    graph = SemanticTwinGraph(project).bootstrap(
        category="general_product", target_id=target["id"]
    )
    semantic_id = next(
        item["id"] for item in graph["nodes"] if item["type"] != "digital_twin_root"
    )
    SemanticTwinGraph(project).bind(
        semantic_id,
        scene_id=scene["id"],
        object_names=["Panel_Object"],
        component_ids=["panel"],
        reference_ids=[item["id"] for item in references],
        confidence=0.9,
    )
    return project, semantic_id, camera["id"], scene["id"]


def _fake_evaluator(project: ProjectStore):
    def evaluate(search: dict[str, Any], candidate: dict[str, Any]) -> dict[str, Any]:
        scene_path = (
            project.root / "scene" / f"search-{search['id']}-{candidate['candidate_index']}.blend"
        )
        scene_path.write_bytes(json.dumps(candidate["parameters"], sort_keys=True).encode())
        scene = SceneStore(project).register_generated(
            scene_path, original_name=scene_path.name
        )
        renders = []
        for reference_index, reference_id in enumerate(search["locality_plan"]["reference_ids"]):
            render_path = (
                project.root
                / "renders"
                / f"search-{candidate['id']}-{reference_index}.png"
            )
            image = Image.new("RGBA", (96, 72), (0, 0, 0, 0))
            inset = {0: 6, 1: 3, 2: 10}.get(candidate["candidate_index"], 12)
            ImageDraw.Draw(image).rectangle(
                (18 + inset, 14, 76, 60), fill=(180, 180, 180, 255)
            )
            image.save(render_path)
            renders.append(
                {
                    "reference_id": reference_id,
                    "path": str(render_path.relative_to(project.root)),
                }
            )
        comparisons = compare_project_renders(project, renders)["comparisons"]
        render_run_id = str(uuid.uuid4())
        outputs = [
            {
                "reference_id": item["reference_id"],
                "artifact_digest": item["render_digest"],
                "relative_path": item["render"],
            }
            for item in comparisons
        ]
        with project.connection() as connection:
            connection.execute(
                "INSERT INTO render_runs(id,scene_id,camera_solution_id,config_json,"
                "outputs_json,created_at) VALUES(?,?,?,?,?,?)",
                (
                    render_run_id,
                    scene["id"],
                    search["camera_solution_id"],
                    json.dumps({"search_id": search["id"]}),
                    json.dumps(outputs),
                    utc_now(),
                ),
            )
        return {
            "scene_id": scene["id"],
            "render_run_id": render_run_id,
            "comparison_ids": sorted(item["id"] for item in comparisons),
        }

    return evaluate


def _start(tmp_path: Path) -> tuple[ProjectStore, MultiviewSearchStore, dict[str, Any]]:
    project, semantic_id, camera_id, scene_id = _fixture(tmp_path)
    store = MultiviewSearchStore(project)
    search = store.start(
        "panel",
        semantic_ids=[semantic_id],
        camera_solution_id=camera_id,
        baseline_scene_id=scene_id,
        parameter_bounds={"width_mm": [8.0, 12.0]},
    )
    return project, store, search


def test_search_planning_is_bounded_and_idempotent(tmp_path: Path) -> None:
    project, store, search = _start(tmp_path)

    repeated = store.start(
        "panel",
        semantic_ids=search["semantic_ids"],
        camera_solution_id=search["camera_solution_id"],
        baseline_scene_id=search["baseline_scene_id"],
        parameter_bounds={"width_mm": [8.0, 12.0]},
    )

    assert repeated["id"] == search["id"]
    assert [item["parameters"] for item in search["candidates"]] == [
        {"width_mm": 10.0},
        {"width_mm": 8.0},
        {"width_mm": 12.0},
    ]
    assert ComponentStore(project).get("panel")["revision"] == 1


def test_search_executes_isolated_candidates_and_emits_receipt(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, search = _start(tmp_path)
    monkeypatch.setattr(store, "_evaluate_candidate", _fake_evaluator(project))

    completed = store.execute(search["id"])
    repeated = store.execute(search["id"])

    assert completed["status"] == "COMPLETE"
    assert completed["receipt_digest"]
    assert completed["optimization_run_id"] == repeated["optimization_run_id"]
    assert ComponentStore(project).get("panel")["revision"] == 1
    assert all(item["status"] == "EVALUATED" for item in completed["candidates"])
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM optimization_runs").fetchone()[0] == 1
        scene_states = [
            connection.execute(
                "SELECT state FROM scene_assets WHERE id=?", (item["scene_id"],)
            ).fetchone()[0]
            for item in completed["candidates"]
        ]
        assert scene_states.count("CANDIDATE") == 1
        assert scene_states.count("REJECTED") == 2
        assert connection.execute(
            "SELECT COUNT(*) FROM scene_transitions "
            "WHERE reviewer='VisionMCP multiview search policy'"
        ).fetchone()[0] == 2
    receipt = export_receipt(project)
    metric = receipt["acceptance"]["metrics"]["multiview_parameter_search"]
    assert metric["completed_with_valid_receipt_count"] == 1
    assert metric["invalid_completed_receipt_ids"] == []
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_search_retries_failure_and_preserves_error_history(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, search = _start(tmp_path)
    evaluator = _fake_evaluator(project)
    failed_once = False

    def flaky(search_record: dict[str, Any], candidate: dict[str, Any]) -> dict[str, Any]:
        nonlocal failed_once
        if candidate["candidate_index"] == 1 and not failed_once:
            failed_once = True
            raise RuntimeError("transient render failure")
        return evaluator(search_record, candidate)

    monkeypatch.setattr(store, "_evaluate_candidate", flaky)
    first = store.execute(search["id"])
    second = store.execute(search["id"])

    assert first["status"] == "RUNNING"
    assert second["status"] == "COMPLETE"
    retried = second["candidates"][1]
    assert retried["attempt_count"] == 2
    assert retried["errors"][0]["error"].endswith("transient render failure")


def test_failed_attempt_scene_is_rejected_before_retry(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, search = _start(tmp_path)
    evaluator = _fake_evaluator(project)
    failed_scene_id = None

    def fail_after_generation(
        search_record: dict[str, Any], candidate: dict[str, Any]
    ) -> dict[str, Any]:
        nonlocal failed_scene_id
        if candidate["candidate_index"] == 1 and failed_scene_id is None:
            path = project.root / "scene" / "failed-attempt.blend"
            path.write_bytes(b"failed attempt")
            scene = SceneStore(project).register_generated(path, original_name=path.name)
            failed_scene_id = scene["id"]
            store._record_candidate_scene(candidate["id"], scene["id"])
            raise RuntimeError("render failed after generation")
        return evaluator(search_record, candidate)

    monkeypatch.setattr(store, "_evaluate_candidate", fail_after_generation)
    first = store.execute(search["id"])

    assert first["status"] == "RUNNING"
    assert first["candidates"][1]["scene_id"] is None
    assert SceneStore(project).get(failed_scene_id)["state"] == "REJECTED"
    assert first["candidates"][1]["errors"][0]["scene_id"] == failed_scene_id

    assert store.execute(search["id"])["status"] == "COMPLETE"


def test_search_detects_stale_component_snapshot(tmp_path: Path) -> None:
    project, store, search = _start(tmp_path)
    ComponentStore(project).update_parameters("panel", {"width_mm": 11.0})

    with pytest.raises(ProjectError, match="component changed"):
        store.execute(search["id"])

    assert store.get(search["id"])["status"] == "STALE"


def test_acceptance_detects_candidate_record_tampering(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, search = _start(tmp_path)
    monkeypatch.setattr(store, "_evaluate_candidate", _fake_evaluator(project))
    completed = store.execute(search["id"])
    candidate_id = completed["candidates"][1]["id"]
    with project.connection() as connection:
        connection.execute(
            "UPDATE multiview_search_candidates SET parameters_json=? WHERE id=?",
            (json.dumps({"width_mm": 99.0}), candidate_id),
        )

    receipt = export_receipt(project)

    metric = receipt["acceptance"]["metrics"]["multiview_parameter_search"]
    assert metric["invalid_completed_receipt_ids"] == [search["id"]]
    assert "L3+ multiview searches lack valid immutable receipts" in receipt["acceptance"][
        "blockers"
    ]


def test_acceptance_detects_candidate_render_lineage_tampering(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, store, search = _start(tmp_path)
    monkeypatch.setattr(store, "_evaluate_candidate", _fake_evaluator(project))
    completed = store.execute(search["id"])
    with project.connection() as connection:
        connection.execute(
            "UPDATE render_runs SET scene_id=? WHERE id=?",
            (search["baseline_scene_id"], completed["candidates"][0]["render_run_id"]),
        )

    metric = export_receipt(project)["acceptance"]["metrics"][
        "multiview_parameter_search"
    ]

    assert metric["invalid_completed_receipt_ids"] == [search["id"]]


def test_autonomy_executes_search_then_stops_at_named_review(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, semantic_id, camera_id, _scene_id = _fixture(tmp_path)
    campaign = CampaignStore(project).start(
        "fixture_reconstruction", configuration={}, resource_profile="compact"
    )
    executor = AutonomousWorkflowExecutor(project)
    facts = {
        "target_id": "fixture-target",
        "target_status": "RESOLVED",
        "evidence_source_count": 1,
        "acquired_source_count": 1,
        "image_reference_count": 2,
        "video_analysis_count": 0,
        "camera_solution_count": 1,
        "approved_metric_camera_solution_count": 1,
        "approved_metric_camera_solution_ids": [camera_id],
        "authoritative_dimension_axes": ["x", "y", "z"],
        "scene_count": 1,
        "render_run_count": 1,
        "mandatory_render_suite_complete": True,
        "comparison_count": 2,
        "comparison_coverage_complete": True,
        "passed_candidate_evaluation_count": 0,
        "promoted_scene_count": 0,
        "proposed_portfolio_candidate_count": 0,
    }
    evaluator = _fake_evaluator(project)
    monkeypatch.setattr(executor, "_facts", lambda: facts)
    monkeypatch.setattr(
        executor,
        "_multiview_fit_targets",
        lambda: [
            {
                "semantic_id": semantic_id,
                "component_ids": ["panel"],
                "reference_ids": [],
            }
        ],
    )
    monkeypatch.setattr(
        EvidenceAcquisitionStore,
        "audit",
        lambda _self, _target_id: {"governance_complete": True},
    )
    monkeypatch.setattr(
        EvidenceConflictStore,
        "audit",
        lambda _self, _target_id, *, record=True: {"unresolved_blocking_count": 0},
    )
    monkeypatch.setattr(
        EvidenceDuplicateStore,
        "audit",
        lambda _self, _target_id, *, record=True: {},
    )
    monkeypatch.setattr(
        MultiviewSearchStore,
        "_evaluate_candidate",
        lambda _self, search, candidate: evaluator(search, candidate),
    )

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "MULTIVIEW_OPTIMIZATION_REVIEW_REQUIRED"
    assert result["campaign"]["status"] == "PAUSED"
    assert result["evidence"]["search"]["status"] == "COMPLETE"
    assert result["evidence"]["optimization_status"] == "proposed"
    assert result["accepted"] is False
    assert ComponentStore(project).get("panel")["revision"] == 1
