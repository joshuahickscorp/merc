from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path
from typing import Any

import pytest

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.acceptance.transactions import (
    REQUIRED_GATE_CATEGORIES,
    CandidateTransactionStore,
)
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import canonical_json
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _promoted_project(tmp_path: Path) -> tuple[ProjectStore, str]:
    project = ProjectStore.create(tmp_path / "project", "Promoted delivery")
    baseline_path = tmp_path / "baseline.blend"
    baseline_path.write_bytes(b"baseline")
    SceneStore(project).import_blend(baseline_path)
    candidate_path = tmp_path / "candidate.blend"
    candidate_path.write_bytes(b"candidate")
    candidate = SceneStore(project).import_blend(candidate_path)
    scenes = SceneStore(project)
    scenes.transition(
        candidate["id"],
        "CANDIDATE",
        reviewer="Fixture builder",
        reason="Enter complete delivery fixture evaluation.",
    )
    evaluation = CandidateTransactionStore(project).evaluate(
        candidate["id"],
        gates=[
            {"category": category, "name": f"{category} gate", "status": "PASS"}
            for category in sorted(REQUIRED_GATE_CATEGORIES)
        ],
    )
    scenes.transition(
        candidate["id"],
        "ACCEPTED",
        reviewer="Fixture reviewer",
        reason="All mandatory gates passed.",
        evaluation_id=evaluation["id"],
    )
    scenes.transition(
        candidate["id"],
        "PROMOTED",
        reviewer="Fixture reviewer",
        reason="Promote the verified delivery fixture.",
        evaluation_id=evaluation["id"],
    )
    assert audit_scene_lifecycle(project)["authoritative_promotion_chain_valid"] is True
    return project, candidate["id"]


def _record_export(project: ProjectStore, scene_id: str, format_name: str) -> dict[str, Any]:
    output = project.root / "exports" / f"fixture.{format_name}"
    output.write_bytes(f"valid-{format_name}".encode())
    artifact = ArtifactStore(project).ingest_file(output)
    export_id = str(uuid.uuid4())
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO exports(id,scene_id,artifact_digest,format,relative_path,config_json,"
            "worker_json,created_at) VALUES(?,?,?,?,?,?,?,?)",
            (
                export_id,
                scene_id,
                artifact.digest,
                format_name,
                str(output.relative_to(project.root)),
                json.dumps({"format": format_name}),
                "{}",
                "2026-07-21T00:00:00+00:00",
            ),
        )
    return {"id": export_id, "artifact": artifact.to_dict()}


def _accepted_receipt() -> dict[str, Any]:
    return {
        "id": "accepted-delivery-receipt",
        "path": "receipts/accepted-delivery-receipt.json",
        "artifact": {"digest": "a" * 64},
        "acceptance": {
            "accepted": True,
            "accepted_fidelity": "L3",
            "blockers": [],
        },
        "payload_sha256": "b" * 64,
        "reused": True,
    }


def test_promoted_delivery_exports_both_formats_and_requires_verified_receipt(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, scene_id = _promoted_project(tmp_path)
    coordinator = Coordinator(project)
    completed_operations = []

    def completed(
        operation: str,
        config: dict[str, Any] | None = None,
        *,
        parent_job_id: str | None = None,
    ) -> dict[str, Any]:
        completed_operations.append(operation)
        if operation == "blender.export_blend":
            result = _record_export(project, scene_id, "blend")
        elif operation == "blender.export":
            result = _record_export(project, scene_id, "glb")
        elif operation == "receipt.export":
            path = project.root / "receipts" / "fixture-delivery.json"
            path.write_text("{}", encoding="utf-8")
            result = {
                "path": str(path.relative_to(project.root)),
                "acceptance": {
                    "accepted": True,
                    "accepted_fidelity": "L3",
                    "blockers": [],
                },
            }
        else:
            raise AssertionError(operation)
        return {"job_id": f"job-{operation}", "result": result}

    current_receipt_calls = 0

    def current_receipt(
        _scene_id: str, _exports: dict[str, dict[str, Any]]
    ) -> dict[str, Any] | None:
        nonlocal current_receipt_calls
        current_receipt_calls += 1
        return None if current_receipt_calls == 1 else _accepted_receipt()

    monkeypatch.setattr(coordinator, "_completed", completed)
    monkeypatch.setattr(coordinator, "_current_delivery_receipt", current_receipt)
    monkeypatch.setattr(
        "blender_vision.scheduling.coordinator.verify_receipt",
        lambda _path, *, project: {"valid": True},
    )

    delivered = coordinator._deliver_promoted({"scene_id": scene_id}, "parent-job")

    assert delivered["status"] == "DELIVERED"
    assert delivered["accepted"] is True
    assert set(delivered["exports"]) == {"blend", "glb"}
    assert completed_operations == [
        "blender.export_blend",
        "blender.export",
        "receipt.export",
    ]


def test_promoted_delivery_reuses_current_exports_and_receipt(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, scene_id = _promoted_project(tmp_path)
    _record_export(project, scene_id, "blend")
    _record_export(project, scene_id, "glb")
    coordinator = Coordinator(project)
    monkeypatch.setattr(
        coordinator, "_current_delivery_receipt", lambda _scene_id, _exports: _accepted_receipt()
    )
    monkeypatch.setattr(
        coordinator,
        "_completed",
        lambda *_args, **_kwargs: pytest.fail("idempotent delivery scheduled duplicate work"),
    )

    delivered = coordinator._deliver_promoted({"scene_id": scene_id}, "parent-job")

    assert delivered["status"] == "DELIVERED"
    assert delivered["reused_receipt"] is True
    assert delivered["stages"] == []


def test_delivery_emits_verified_not_accepted_receipt_when_gates_remain(
    tmp_path: Path,
) -> None:
    project, scene_id = _promoted_project(tmp_path)
    _record_export(project, scene_id, "blend")
    _record_export(project, scene_id, "glb")

    delivered = Coordinator(project)._deliver_promoted(
        {"scene_id": scene_id}, "fixture-parent"
    )

    assert delivered["status"] == "NOT_ACCEPTED"
    assert delivered["accepted"] is False
    assert delivered["acceptance"]["blockers"]
    assert [stage["result"]["id"] for stage in delivered["stages"]] == [
        delivered["receipt"]["id"]
    ]
    assert verify_receipt(
        project.root / delivered["receipt"]["path"], project=project
    )["valid"] is True


def test_delivery_refuses_unreceipted_promoted_state(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "forged", "Forged promotion")
    scene_path = tmp_path / "forged.blend"
    scene_path.write_bytes(b"forged")
    scene = SceneStore(project).import_blend(scene_path)
    with project.connection() as connection:
        connection.execute(
            "UPDATE scene_assets SET state='PROMOTED',is_authoritative=1 WHERE id=?",
            (scene["id"],),
        )

    with pytest.raises(ValueError, match="verified promotion chain"):
        Coordinator(project)._deliver_promoted({"scene_id": scene["id"]}, "parent-job")


def test_delivery_rejects_hash_valid_but_semantically_forged_acceptance_receipt(
    tmp_path: Path,
) -> None:
    project, scene_id = _promoted_project(tmp_path)
    _record_export(project, scene_id, "blend")
    _record_export(project, scene_id, "glb")
    exports = Coordinator(project)._delivery_exports(scene_id)
    receipt_id = str(uuid.uuid4())
    payload = {
        "schema_version": 1,
        "receipt_id": receipt_id,
        "evidence": {
            "scenes": [
                {
                    "id": scene_id,
                    "state": "PROMOTED",
                    "is_authoritative": 1,
                    "artifact_digest": SceneStore(project).get(scene_id)["artifact_digest"],
                }
            ],
            "exports": [
                {
                    "id": item["id"],
                    "scene_id": scene_id,
                    "format": item["format"],
                    "artifact_digest": item["artifact_digest"],
                }
                for item in exports.values()
            ],
        },
        "acceptance": {
            "accepted": True,
            "accepted_fidelity": "L3",
            "blockers": [],
        },
    }
    envelope = {
        "payload": payload,
        "payload_sha256": hashlib.sha256(canonical_json(payload)).hexdigest(),
    }
    path = project.root / "receipts" / f"{receipt_id}.json"
    path.write_text(json.dumps(envelope), encoding="utf-8")
    artifact = ArtifactStore(project).ingest_file(
        path, media_type="application/vnd.bvmcp.receipt+json"
    )
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO receipts(id,digest,fidelity_level,accepted,created_at) "
            "VALUES(?,?,?,?,?)",
            (receipt_id, artifact.digest, "L3", 1, "2026-07-21T00:00:00+00:00"),
        )

    assert verify_receipt(path, project=project)["valid"] is True
    assert Coordinator(project)._current_delivery_receipt(scene_id, exports) is None


def test_autonomous_executor_closes_campaign_only_after_verified_delivery(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, scene_id = _promoted_project(tmp_path)
    campaign = CampaignStore(project).start(
        "autonomous_delivery_fixture", configuration={}, resource_profile="compact"
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
        "approved_metric_camera_solution_ids": ["metric-camera"],
        "authoritative_dimension_axes": ["x", "y", "z"],
        "scene_count": 2,
        "render_run_count": 1,
        "mandatory_render_suite_complete": True,
        "comparison_count": 2,
        "comparison_coverage_complete": True,
        "passed_candidate_evaluation_count": 1,
        "promoted_scene_count": 1,
        "promoted_scene_id": scene_id,
        "proposed_portfolio_candidate_count": 0,
    }
    monkeypatch.setattr(executor, "_facts", lambda: facts)
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

    def run(_self: Coordinator, operation: str, config: dict[str, Any] | None = None):
        assert operation == "workflow.deliver_promoted"
        assert config == {"scene_id": scene_id}
        delivery = {
            "status": "DELIVERED",
            "accepted": True,
            "acceptance": {"accepted": True, "accepted_fidelity": "L3"},
        }
        return {"id": "delivery-job", "status": "succeeded", "result": delivery}

    monkeypatch.setattr(Coordinator, "run", run)

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "DELIVERY_COMPLETE"
    assert result["accepted"] is True
    assert result["campaign"]["status"] == "STOPPED"
    assert result["campaign"]["result"]["accepted_fidelity"] == "L3"
