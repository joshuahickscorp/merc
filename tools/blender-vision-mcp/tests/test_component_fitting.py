from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.constraints.models import Constraint, ConstraintType
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore


def test_component_fit_is_robust_revisioned_and_review_gated(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Fitting")
    component = ComponentStore(project).create(
        ComponentSpec(
            id="panel",
            type=ComponentType.PANEL,
            parameters={"width_mm": 40.0, "height_mm": 20.0},
            constraints=[
                Constraint(
                    id="width-target",
                    type=ConstraintType.KNOWN_DIMENSION,
                    subjects=["panel"],
                    parameters={"parameter": "width_mm", "millimetres": 50.0, "tolerance_mm": 1.0},
                )
            ],
        )
    )
    measurements = MeasurementStore(project)
    ids = [
        measurements.add(
            "line",
            {"millimetres": value},
            evidence_class=EvidenceClass.MEASURED,
            uncertainty={"millimetres": sigma},
        )["id"]
        for value, sigma in ((49.9, 0.2), (50.1, 0.2), (85.0, 10.0))
    ]

    fit = ComponentFitter(project).propose("panel", {"width_mm": ids})

    assert fit["status"] == "proposed"
    assert fit["result"]["constraints_pass"] is True
    assert abs(fit["result"]["candidate_parameters"]["width_mm"] - 50.0) < 0.2
    accepted = ComponentFitter(project).review(
        fit["id"], accepted=True, reviewer="Metrology QA", reason="Bound measurements reviewed"
    )
    assert accepted["status"] == "accepted"
    assert accepted["applied_revision"] == component["revision"] + 1
    assert abs(ComponentStore(project).get("panel")["parameters"]["width_mm"] - 50.0) < 0.2


def test_component_fit_refuses_stale_application(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Stale fit")
    store = ComponentStore(project)
    store.create(ComponentSpec(id="body", type=ComponentType.BODY, parameters={"width_mm": 10.0}))
    measurement = MeasurementStore(project).add(
        "line",
        {"millimetres": 12.0},
        evidence_class=EvidenceClass.MEASURED,
        uncertainty={"millimetres": 0.1},
    )
    fit = ComponentFitter(project).propose("body", {"width_mm": [measurement["id"]]})
    store.update_parameters("body", {"width_mm": 11.0})
    with pytest.raises(ProjectError, match="changed after fit"):
        ComponentFitter(project).review(
            fit["id"], accepted=True, reviewer="Reviewer", reason="Stale on purpose"
        )
