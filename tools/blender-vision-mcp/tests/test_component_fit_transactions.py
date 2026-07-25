from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore


def _proposed_fit(tmp_path: Path) -> tuple[ProjectStore, ComponentFitter, dict]:
    project = ProjectStore.create(tmp_path / "project", "Atomic component fit")
    measurement = MeasurementStore(project).add(
        "line",
        {"role": "panel_width", "millimetres": 197.0},
        evidence_class=EvidenceClass.MEASURED,
        certainty="bounded",
        uncertainty={"millimetres": 0.2},
    )
    ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 190.0},
            evidence_bindings=[measurement["id"]],
        )
    )
    fitter = ComponentFitter(project)
    return project, fitter, fitter.propose("panel", {"width_mm": [measurement["id"]]})


def test_component_fit_review_is_artifact_bound_and_receipt_verified(tmp_path: Path) -> None:
    project, fitter, proposal = _proposed_fit(tmp_path)

    accepted = fitter.review(
        proposal["id"],
        accepted=True,
        reviewer="Component reviewer",
        reason="Authoritative measured width and constraints verified.",
    )

    assert accepted["status"] == "accepted"
    assert accepted["decision_digest"] == accepted["decision_artifact"]["digest"]
    assert accepted["decision_receipt"]["proposal_digest"] == proposal["artifact"]["digest"]
    assert accepted["decision_receipt"]["component_revision_before"] == 1
    assert accepted["decision_receipt"]["component_revision_after"] == 2
    assert (project.root / accepted["decision_path"]).is_file()
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 197.0
    receipt = export_receipt(project)
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_component_and_fit_decision_roll_back_together_on_write_failure(tmp_path: Path) -> None:
    project, fitter, proposal = _proposed_fit(tmp_path)
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER reject_fit_decision BEFORE UPDATE OF status ON component_fits "
            "WHEN NEW.status='accepted' BEGIN SELECT RAISE(ABORT,'fixture failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="fixture failure"):
        fitter.review(
            proposal["id"],
            accepted=True,
            reviewer="Component reviewer",
            reason="Simulate a failure after the component update statement.",
        )

    component = ComponentStore(project).get("panel")
    persisted = fitter.get(proposal["id"])
    assert component["revision"] == 1
    assert component["parameters"]["width_mm"] == 190.0
    assert persisted["status"] == "proposed"
    assert persisted["decision_digest"] is None
    assert persisted["applied_revision"] is None


def test_tampered_fit_state_or_component_snapshot_cannot_be_accepted(tmp_path: Path) -> None:
    project, fitter, proposal = _proposed_fit(tmp_path)
    with project.connection() as connection:
        row = connection.execute(
            "SELECT result_json FROM component_fits WHERE id=?", (proposal["id"],)
        ).fetchone()
        result = json.loads(row["result_json"])
        result["candidate_parameters"]["width_mm"] = 999.0
        connection.execute(
            "UPDATE component_fits SET result_json=? WHERE id=?",
            (json.dumps(result), proposal["id"]),
        )

    with pytest.raises(ProjectError, match="artifact is missing, corrupt, or inconsistent"):
        fitter.review(
            proposal["id"],
            accepted=True,
            reviewer="Component reviewer",
            reason="This forged proposal must not apply.",
        )
    assert ComponentStore(project).get("panel")["parameters"]["width_mm"] == 190.0


def test_acceptance_audit_rejects_fit_decision_that_disagrees_with_receipt(
    tmp_path: Path,
) -> None:
    project, fitter, proposal = _proposed_fit(tmp_path)
    fitter.review(
        proposal["id"],
        accepted=True,
        reviewer="Component reviewer",
        reason="Measured width verified.",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE component_fits SET reviewer='Forged reviewer' WHERE id=?",
            (proposal["id"],),
        )

    receipt = export_receipt(project)
    component_metrics = receipt["acceptance"]["metrics"]["component_fitting"]

    assert component_metrics["invalid_decision_receipt_ids"] == [proposal["id"]]
    assert "L3+ component fit decisions lack valid immutable receipts" in receipt[
        "acceptance"
    ]["blockers"]
    assert receipt["acceptance"]["accepted"] is False
