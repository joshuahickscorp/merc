from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore


def _proposal(tmp_path: Path) -> tuple[ProjectStore, OptimizationEngine, dict]:
    project = ProjectStore.create(tmp_path / "project", "Atomic optimization")
    ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 10.0},
        )
    )
    measurement = MeasurementStore(project).add(
        "line",
        {"millimetres": 12.0},
        evidence_class=EvidenceClass.MEASURED,
        uncertainty={"millimetres": 0.1},
    )
    engine = OptimizationEngine(project)
    proposal = engine.propose(
        "panel",
        tier="black_box",
        method="bounded_candidate_search",
        candidates=[
            {
                "parameters": {"width_mm": 10.0},
                "terms": {"measurement": 2.0},
                "baseline": True,
            },
            {"parameters": {"width_mm": 12.0}, "terms": {"measurement": 0.0}},
        ],
        evidence_binding_ids=[measurement["id"]],
    )
    return project, engine, proposal


def test_optimization_review_is_atomic_and_receipt_bound(tmp_path: Path) -> None:
    project, engine, proposal = _proposal(tmp_path)

    accepted = engine.review(
        proposal["id"],
        accepted=True,
        reviewer="Optimization reviewer",
        reason="Bounded loss trace and evidence bindings verified.",
    )

    assert accepted["status"] == "accepted"
    assert accepted["decision_digest"] == accepted["decision_artifact"]["digest"]
    assert accepted["decision_receipt"]["proposal_digest"] == proposal["artifact"]["digest"]
    assert accepted["decision_receipt"]["component_revision_after"] == 2
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 12.0
    receipt = export_receipt(project)
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True
    assert receipt["acceptance"]["metrics"]["intelligence"][
        "accepted_optimization_with_valid_receipt_count"
    ] == 1


def test_optimization_update_and_decision_roll_back_together(tmp_path: Path) -> None:
    project, engine, proposal = _proposal(tmp_path)
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER reject_optimization_decision "
            "BEFORE UPDATE OF status ON optimization_runs "
            "WHEN NEW.status='accepted' BEGIN SELECT RAISE(ABORT,'fixture failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="fixture failure"):
        engine.review(
            proposal["id"],
            accepted=True,
            reviewer="Optimization reviewer",
            reason="Simulated decision-write failure.",
        )

    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 10.0
    persisted = engine.get(proposal["id"])
    assert persisted["status"] == "proposed"
    assert persisted["decision_digest"] is None


def test_optimization_tampering_fails_review_and_semantic_acceptance_audit(
    tmp_path: Path,
) -> None:
    project, engine, proposal = _proposal(tmp_path)
    with project.connection() as connection:
        row = connection.execute(
            "SELECT result_json FROM optimization_runs WHERE id=?", (proposal["id"],)
        ).fetchone()
        result = json.loads(row["result_json"])
        result["best_parameters"]["width_mm"] = 999.0
        connection.execute(
            "UPDATE optimization_runs SET result_json=? WHERE id=?",
            (json.dumps(result), proposal["id"]),
        )
    with pytest.raises(ProjectError, match="artifact is missing, corrupt, or inconsistent"):
        engine.review(
            proposal["id"],
            accepted=True,
            reviewer="Optimization reviewer",
            reason="Forged result must not apply.",
        )
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 10.0

    fresh_project, fresh_engine, fresh_proposal = _proposal(tmp_path / "fresh")
    fresh_engine.review(
        fresh_proposal["id"],
        accepted=True,
        reviewer="Optimization reviewer",
        reason="Valid decision before database tampering.",
    )
    with fresh_project.connection() as connection:
        row = connection.execute(
            "SELECT result_json FROM optimization_runs WHERE id=?", (fresh_proposal["id"],)
        ).fetchone()
        result = json.loads(row["result_json"])
        result["review"]["reviewer"] = "Forged reviewer"
        connection.execute(
            "UPDATE optimization_runs SET result_json=? WHERE id=?",
            (json.dumps(result), fresh_proposal["id"]),
        )
    receipt = export_receipt(fresh_project)
    assert receipt["acceptance"]["metrics"]["intelligence"][
        "invalid_optimization_decision_ids"
    ] == [fresh_proposal["id"]]
    assert "L3+ optimization decisions lack valid immutable receipts" in receipt[
        "acceptance"
    ]["blockers"]
