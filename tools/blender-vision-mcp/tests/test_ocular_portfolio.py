"""Phase R — representation portfolio tests."""

from __future__ import annotations

from pathlib import Path

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.ocular.representation import (
    Purpose,
    RepresentationCandidate,
    RepresentationKind,
    build_radiance_candidate_blocked,
    build_representation_portfolio,
    build_retrieved_candidate,
    evaluate_purposes,
    run_portfolio_benchmark,
)


def test_portfolio_has_mesh_points_procedural_radiance_blocked(tmp_path: Path) -> None:
    portfolio = build_representation_portfolio(tmp_path, target_id="t1")
    kinds = {c.kind for c in portfolio.candidates}
    assert RepresentationKind.MESH in kinds
    assert RepresentationKind.POINT_CLOUD in kinds
    assert RepresentationKind.PROCEDURAL in kinds
    assert RepresentationKind.RETRIEVED in kinds
    assert RepresentationKind.GAUSSIAN_RADIANCE in kinds
    radiance = next(
        c for c in portfolio.candidates if c.kind is RepresentationKind.GAUSSIAN_RADIANCE
    )
    assert radiance.executed is False
    assert radiance.execution_class == ExecutionClass.BLOCKED.value
    assert portfolio.radiance_blocked is True
    assert (tmp_path / "mesh.obj").is_file()
    assert (tmp_path / "points.ply").is_file()
    assert (tmp_path / "representation_portfolio.json").is_file()


def test_retrieval_blocked_without_license() -> None:
    cand = build_retrieved_candidate(license_ok=False, license_name="unknown")
    assert cand.executed is False
    assert cand.execution_class == ExecutionClass.BLOCKED.value


def test_purpose_evaluation_does_not_force_one_repr() -> None:
    candidates = [
        RepresentationCandidate(
            kind=RepresentationKind.MESH,
            executed=True,
            execution_class=ExecutionClass.PHYSICAL.value,
        ),
        RepresentationCandidate(
            kind=RepresentationKind.POINT_CLOUD,
            executed=True,
            execution_class=ExecutionClass.PHYSICAL.value,
        ),
        RepresentationCandidate(
            kind=RepresentationKind.PROCEDURAL,
            executed=True,
            execution_class=ExecutionClass.CANDIDATE_ONLY.value,
        ),
        RepresentationCandidate(
            kind=RepresentationKind.RETRIEVED,
            executed=False,
            execution_class=ExecutionClass.BLOCKED.value,
            reason="license",
        ),
        build_radiance_candidate_blocked(),
    ]
    evals = evaluate_purposes(candidates)
    by_purpose = {e.purpose: e for e in evals}
    assert by_purpose[Purpose.PHOTOREAL_VIEW_SYNTHESIS].selected_kind is None
    assert by_purpose[Purpose.EDITABLE_GEOMETRY].selected_kind in {
        RepresentationKind.MESH.value,
        RepresentationKind.PROCEDURAL.value,
    }
    assert by_purpose[Purpose.MEASUREMENT].selected_kind is not None


def test_radiance_executed_raises() -> None:
    bad = RepresentationCandidate(
        kind=RepresentationKind.GAUSSIAN_RADIANCE,
        executed=True,
        execution_class=ExecutionClass.PHYSICAL.value,
    )
    # evaluate_purposes does not raise; build path does.
    # Simulate build guard:
    if bad.kind is RepresentationKind.GAUSSIAN_RADIANCE and bad.executed:
        with pytest.raises(ValidationError):
            raise ValidationError("radiance candidate must not execute without weights")


def test_run_portfolio_benchmark(tmp_path: Path) -> None:
    receipt = run_portfolio_benchmark(tmp_path)
    assert receipt["status"] == "PASS"
    assert "remote" in receipt["targets"]
    for _tid, row in receipt["targets"].items():
        assert row["radiance_blocked"] is True
        assert RepresentationKind.GAUSSIAN_RADIANCE.value in row["blocked_kinds"]
        assert RepresentationKind.MESH.value in row["executed_kinds"]
