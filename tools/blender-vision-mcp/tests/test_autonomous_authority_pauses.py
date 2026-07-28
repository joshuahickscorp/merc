from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.workflows.executor import AutonomousWorkflowExecutor
from blender_vision.workflows.progress import WorkflowProgressReporter


def _ready_facts(*, passed_evaluation: bool) -> dict:
    return {
        "target_id": "resolved-target",
        "target_status": "RESOLVED",
        "evidence_source_count": 1,
        "acquired_source_count": 1,
        "image_reference_count": 2,
        "video_analysis_count": 0,
        "camera_solution_count": 1,
        "approved_metric_camera_solution_count": 1,
        "approved_metric_camera_solution_ids": ["metric-camera"],
        "authoritative_dimension_axes": ["x", "y", "z"],
        "scene_count": 1,
        "render_run_count": 1,
        "mandatory_render_suite_complete": True,
        "comparison_count": 2,
        "comparison_coverage_complete": True,
        "passed_candidate_evaluation_count": int(passed_evaluation),
        "promoted_scene_count": 0,
        "promoted_scene_id": None,
        "proposed_portfolio_candidate_count": 0,
        "verification": {
            "verified_passed_evaluation_ids": (
                ["verified-evaluation"] if passed_evaluation else []
            )
        },
    }


def _executor(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    *,
    passed_evaluation: bool,
) -> tuple[AutonomousWorkflowExecutor, dict]:
    project = ProjectStore.create(tmp_path / "project", "Authority pause")
    campaign = CampaignStore(project).start(
        "authority_pause_fixture", configuration={}, resource_profile="compact"
    )
    executor = AutonomousWorkflowExecutor(project)
    monkeypatch.setattr(executor, "_facts", lambda: _ready_facts(
        passed_evaluation=passed_evaluation
    ))
    monkeypatch.setattr(executor, "_multiview_fit_targets", lambda: [])
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
    return executor, campaign


def test_candidate_transaction_boundary_pauses_with_persistent_review_tasks(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    executor, campaign = _executor(tmp_path, monkeypatch, passed_evaluation=False)

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "CANDIDATE_TRANSACTION_REQUIRED"
    assert result["campaign"]["status"] == "PAUSED"
    assert result["evidence"]["required_gate_categories"] == [
        "camera",
        "measurement",
        "component",
        "topology",
        "material",
        "appearance",
        "provenance",
    ]
    assert {task["role"] for task in result["evidence"]["role_tasks"]} == {
        "Geometry Analyst",
        "Material Analyst",
        "Adversarial Reviewer",
        "Acceptance Auditor",
    }
    progress = WorkflowProgressReporter(executor.project).report(campaign["id"])
    assert progress["next_action"]["action"] == "complete_named_review"
    assert set(progress["next_action"]["review_task_ids"]) == {
        task["id"] for task in result["evidence"]["role_tasks"]
    }
    assert {task["role"] for task in progress["review_tasks"]} == {
        "Geometry Analyst",
        "Material Analyst",
        "Adversarial Reviewer",
        "Acceptance Auditor",
    }
    stopped = executor.continue_once(campaign["id"])
    assert stopped["workflow_state"] == "CAMPAIGN_NOT_RUNNING"


def test_safe_promotion_boundary_pauses_with_verified_evaluation_task(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    executor, campaign = _executor(tmp_path, monkeypatch, passed_evaluation=True)

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "SAFE_PROMOTION_REQUIRED"
    assert result["campaign"]["status"] == "PAUSED"
    assert result["evidence"]["verified_passed_evaluation_ids"] == [
        "verified-evaluation"
    ]
    assert result["evidence"]["required_tools"] == [
        "candidate.accept",
        "candidate.promote",
    ]
    assert {task["role"] for task in result["evidence"]["role_tasks"]} == {
        "Adversarial Reviewer",
        "Acceptance Auditor",
    }


def test_reviewed_promotion_resume_continues_to_verified_delivery(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    executor, campaign = _executor(tmp_path, monkeypatch, passed_evaluation=True)
    paused = executor.continue_once(campaign["id"])
    assert paused["workflow_state"] == "SAFE_PROMOTION_REQUIRED"

    promoted = _ready_facts(passed_evaluation=True)
    promoted.update(
        {
            "promoted_scene_count": 1,
            "promoted_scene_id": "reviewed-promoted-scene",
        }
    )
    monkeypatch.setattr(executor, "_facts", lambda: promoted)

    def delivered(
        _self: Coordinator, operation: str, config: dict | None = None
    ) -> dict:
        assert operation == "workflow.deliver_promoted"
        assert config == {"scene_id": "reviewed-promoted-scene"}
        return {
            "id": "delivery-job",
            "status": "succeeded",
            "result": {
                "status": "DELIVERED",
                "accepted": True,
                "acceptance": {"accepted": True, "accepted_fidelity": "L3"},
            },
        }

    monkeypatch.setattr(Coordinator, "run", delivered)

    result = executor.continue_once(campaign["id"], resume_paused=True)

    assert result["workflow_state"] == "DELIVERY_COMPLETE"
    assert result["accepted"] is True
    assert result["campaign"]["status"] == "STOPPED"
    assert result["campaign"]["result"]["accepted_fidelity"] == "L3"
