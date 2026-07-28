from __future__ import annotations

import json
from pathlib import Path
from typing import Any
from urllib import parse

import pytest

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.acceptance.transactions import CandidateTransactionStore
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.pursuit import EvidencePursuitStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.scenes import SceneStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _project(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Missing evidence pursuit")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Acme", "model": "Widget", "model_year": 2026}
    )
    return project, target


def _provider(project: ProjectStore) -> dict:
    return SearchProviderStore(project).register(
        name="Pursuit fixture provider",
        configuration={
            "endpoint": "https://search.example.test/api",
            "provider_kind": "json_items",
            "maximum_queries_per_run": 10,
            "maximum_results_per_query": 5,
            "maximum_response_bytes": 8192,
            "allowed_provider_domains": ["search.example.test"],
            "allowed_result_domains": ["acme.example"],
            "official_domains": ["acme.example"],
            "authentication": {"mode": "none"},
            "access_policy": {
                "authentication_boundary": "none",
                "source_terms_review": "approved",
                "privacy_review": "not_applicable",
            },
        },
        reviewer="Provider reviewer",
        reason="Terms and privacy reviewed for the pursuit fixture.",
    )


def _failed_evaluation(project: ProjectStore, tmp_path: Path) -> dict:
    source = tmp_path / "blocked-candidate.blend"
    source.write_bytes(b"candidate")
    scenes = SceneStore(project)
    scene = scenes.import_blend(source)
    scenes.transition(scene["id"], "CANDIDATE", reviewer="Builder", reason="Evaluate")
    gates = [
        {
            "name": f"{category} gate",
            "category": category,
            "status": "BLOCKED" if category in {"measurement", "provenance"} else "PASS",
        }
        for category in (
            "camera",
            "measurement",
            "component",
            "topology",
            "material",
            "appearance",
            "provenance",
        )
    ]
    return CandidateTransactionStore(project).evaluate(scene["id"], gates=gates)


def _autonomy_facts(target_id: str) -> dict[str, Any]:
    return {
        "target_id": target_id,
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
        "comparison_count": 1,
        "comparison_coverage_complete": True,
        "passed_candidate_evaluation_count": 0,
        "promoted_scene_count": 0,
        "promoted_scene_id": None,
        "proposed_portfolio_candidate_count": 0,
    }


def _patch_governed_autonomy(
    monkeypatch: pytest.MonkeyPatch, executor: AutonomousWorkflowExecutor, target_id: str
) -> None:
    monkeypatch.setattr(executor, "_facts", lambda: _autonomy_facts(target_id))
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


class _Response:
    status = 200

    def __init__(self, payload: bytes):
        self.payload = payload

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        return None

    def geturl(self) -> str:
        return "https://search.example.test/api"

    def read(self, maximum: int) -> bytes:
        return self.payload[:maximum]


def test_pursuit_without_provider_records_precise_capture_ceiling_idempotently(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    store = EvidencePursuitStore(project)

    first = store.pursue(target["id"])
    repeated = store.pursue(target["id"])

    assert first["status"] == "EVIDENCE_CEILING"
    assert repeated["id"] == first["id"]
    assert len(first["capture_request_ids"]) == 7
    with project.connection() as connection:
        requests = connection.execute(
            "SELECT request_json FROM capture_requests ORDER BY created_at,id"
        ).fetchall()
    records = [json.loads(row["request_json"]) for row in requests]
    assert len(records) == 7
    assert {item["region"] for item in records} == {
        "front",
        "rear",
        "left",
        "right",
        "top",
        "bottom",
        "underbody",
    }
    assert all(item["requester"] == "VisionMCP Capture Planner" for item in records)
    assert all(item["request_type"] == "photograph" for item in records)
    assert first["report_digest"]
    receipt = export_receipt(project)
    metric = receipt["acceptance"]["metrics"]["missing_evidence_pursuit"]
    assert metric["invalid_receipt_ids"] == []
    assert metric["latest_status"] == "EVIDENCE_CEILING"
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_pursuit_executes_only_current_gap_queries_without_granting_rights(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    provider = _provider(project)
    monkeypatch.setattr(
        "blender_vision.evidence.acquisition.socket.getaddrinfo",
        lambda *_args, **_kwargs: [(2, 1, 6, "", ("93.184.216.34", 0))],
    )
    payload = json.dumps(
        {
            "items": [
                {
                    "url": "https://media.acme.example/widget-gap.png",
                    "title": "Official missing view",
                    "publisher": "Acme",
                    "score": 0.9,
                }
            ]
        }
    ).encode()
    queries = []

    def urlopen(search_request, *, timeout):
        assert timeout == 5.0
        queries.append(parse.parse_qs(parse.urlsplit(search_request.full_url).query)["q"][0])
        return _Response(payload)

    monkeypatch.setattr("blender_vision.evidence.discovery.request.urlopen", urlopen)

    result = EvidencePursuitStore(project).pursue(
        target["id"],
        provider_id=provider["id"],
        maximum_queries=3,
        timeout_seconds=5.0,
    )
    repeated = EvidencePursuitStore(project).pursue(
        target["id"],
        provider_id=provider["id"],
        maximum_queries=3,
        timeout_seconds=5.0,
    )

    assert result["status"] == "SOURCES_DISCOVERED"
    assert repeated["id"] == result["id"]
    assert len(queries) == 3
    assert all(query.split()[-1] in result["focus_terms"] for query in queries)
    assert not any(query.endswith("dimensions") for query in queries)
    assert result["capture_request_ids"] == []
    sources = EvidenceAcquisitionStore(project).list(target["id"])
    assert len(sources) == 1
    assert sources[0]["rights"]["status"] == "UNREVIEWED_DISCOVERY"
    assert sources[0]["rights"]["internal_use"] is False


def test_expired_pursuit_lease_resumes_without_duplicate_capture_requests(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    store = EvidencePursuitStore(project)
    first = store.pursue(target["id"])
    with project.connection() as connection:
        connection.execute(
            "UPDATE evidence_pursuit_runs SET status='RUNNING',report_digest=NULL,"
            "lease_token='abandoned',lease_expires_at='2020-01-01T00:00:00+00:00' "
            "WHERE id=?",
            (first["id"],),
        )

    resumed = store.pursue(target["id"])

    assert resumed["id"] == first["id"]
    assert resumed["status"] == "EVIDENCE_CEILING"
    assert resumed["attempt_count"] == 2
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM capture_requests").fetchone()[0] == 7


def test_focused_search_terms_are_bounded_and_normalized(tmp_path: Path) -> None:
    project, target = _project(tmp_path)
    plan = EvidenceAcquisitionStore(project).plan_search(
        target["id"], focus_terms=[" Rear Diffuser ", "rear diffuser", "UNDERBODY"]
    )
    assert plan["focus_terms"] == ["rear diffuser", "underbody"]
    assert len(plan["ranked_tasks"]) == 2
    with pytest.raises(ValueError, match="one to 32"):
        EvidenceAcquisitionStore(project).plan_search(target["id"], focus_terms=[])


def test_evidence_ceiling_emits_typed_document_and_measurement_requests(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)

    pursuit = EvidencePursuitStore(project).pursue(
        target["id"], required_terms=["dimensions", "official manual"]
    )
    by_region = {item["region"]: item for item in pursuit["report"]["capture_requests"]}

    assert by_region["dimensions"]["request_type"] == "physical_measurement"
    assert by_region["dimensions"]["direction"] == "measurement"
    assert "uncertainty" in by_region["dimensions"]["instructions"]
    assert by_region["official manual"]["request_type"] == "document_upload"
    assert by_region["official manual"]["direction"] == "document"
    assert "exact target variant" in by_region["official manual"]["instructions"]


def test_open_migrates_prelease_evidence_pursuit_table(tmp_path: Path) -> None:
    project, target = _project(tmp_path)
    with project.connection() as connection:
        connection.execute("DROP TABLE evidence_pursuit_runs")
        connection.execute(
            "CREATE TABLE evidence_pursuit_runs ("
            "id TEXT PRIMARY KEY,cache_key TEXT NOT NULL UNIQUE,target_id TEXT NOT NULL,"
            "provider_id TEXT,status TEXT NOT NULL,focus_terms_json TEXT NOT NULL,"
            "coverage_json TEXT NOT NULL,discovery_run_id TEXT,"
            "capture_request_ids_json TEXT NOT NULL,report_digest TEXT,"
            "created_at TEXT NOT NULL,updated_at TEXT NOT NULL)"
        )

    reopened = ProjectStore.open(project.root)
    with reopened.connection() as connection:
        columns = {
            row["name"]
            for row in connection.execute("PRAGMA table_info(evidence_pursuit_runs)")
        }
    assert {"lease_token", "lease_expires_at", "attempt_count"} <= columns
    assert EvidencePursuitStore(reopened).pursue(target["id"])["attempt_count"] == 1


def test_acceptance_detects_capture_request_tampering_after_pursuit(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    pursuit = EvidencePursuitStore(project).pursue(target["id"])
    request_id = pursuit["capture_request_ids"][0]
    with project.connection() as connection:
        row = connection.execute(
            "SELECT request_json FROM capture_requests WHERE id=?", (request_id,)
        ).fetchone()
        record = json.loads(row["request_json"])
        record["region"] = "forged-covered-region"
        connection.execute(
            "UPDATE capture_requests SET request_json=? WHERE id=?",
            (json.dumps(record), request_id),
        )

    receipt = export_receipt(project)

    metric = receipt["acceptance"]["metrics"]["missing_evidence_pursuit"]
    assert metric["invalid_receipt_ids"] == [pursuit["id"]]
    assert "one or more missing-evidence pursuit receipts are invalid" in receipt[
        "acceptance"
    ]["blockers"]


def test_autonomy_pursues_terms_from_blocked_candidate_gates(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    _failed_evaluation(project, tmp_path)
    campaign = CampaignStore(project).start(
        "missing_evidence_fixture",
        configuration={"target_id": target["id"], "category": "computer_hardware"},
        resource_profile="compact",
    )
    executor = AutonomousWorkflowExecutor(project)
    _patch_governed_autonomy(monkeypatch, executor, target["id"])

    def run(_self: Coordinator, operation: str, config: dict[str, Any] | None = None):
        assert operation == "evidence.pursue_missing"
        assert config == {
            "target_id": target["id"],
            "category": "computer_hardware",
            "required_terms": [
                "dimensions",
                "technical drawing",
                "official manual",
                "manufacturer specifications",
            ],
        }
        return {
            "id": "pursuit-job",
            "status": "succeeded",
            "result": {
                "id": "pursuit-run",
                "status": "SOURCES_DISCOVERED",
                "focus_terms": config["required_terms"],
                "capture_request_ids": [],
            },
        }

    monkeypatch.setattr(Coordinator, "run", run)

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "MISSING_EVIDENCE_DISCOVERED"
    assert result["campaign"]["status"] == "RUNNING"
    assert result["accepted"] is False


def test_autonomy_stops_at_evidence_ceiling_with_precise_capture_requests(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, target = _project(tmp_path)
    _failed_evaluation(project, tmp_path)
    campaign = CampaignStore(project).start(
        "missing_evidence_fixture",
        configuration={"target_id": target["id"]},
        resource_profile="compact",
    )
    executor = AutonomousWorkflowExecutor(project)
    _patch_governed_autonomy(monkeypatch, executor, target["id"])
    ceiling = {
        "id": "pursuit-run",
        "status": "EVIDENCE_CEILING",
        "focus_terms": ["dimensions", "official manual"],
        "capture_request_ids": ["capture-dimensions", "capture-manual"],
    }
    monkeypatch.setattr(
        Coordinator,
        "run",
        lambda _self, operation, config=None: {
            "id": "pursuit-job",
            "status": "succeeded",
            "result": ceiling,
        },
    )

    result = executor.continue_once(campaign["id"])

    assert result["workflow_state"] == "EVIDENCE_CEILING_REACHED"
    assert result["campaign"]["status"] == "STOPPED"
    assert result["campaign"]["result"]["evidence_ceiling"] == ceiling
    assert result["accepted"] is False


def test_new_evidence_makes_blocked_gate_diagnosis_stale(tmp_path: Path) -> None:
    project, _target = _project(tmp_path)
    evaluation = _failed_evaluation(project, tmp_path)
    with project.connection() as connection:
        connection.execute(
            "UPDATE candidate_evaluations SET created_at=? WHERE id=?",
            ("2020-01-01T00:00:00+00:00", evaluation["id"]),
        )
    MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 123.0},
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
        certainty="exact",
    )

    assert AutonomousWorkflowExecutor(project)._blocked_evidence_terms() == []
