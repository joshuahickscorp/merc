from __future__ import annotations

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.critics import (
    ALL_ROLES,
    BoundedRepairRunner,
    CriticRole,
    CriticWorkspace,
    CritiqueEvidence,
    CritiqueSubject,
    default_critics,
    make_finding,
    plan_from_finding,
    require_measured,
)
from blender_vision.critics.fixtures import evidence_for, load_control_subject, load_fault_subject
from blender_vision.v2.records import CriticFinding
from blender_vision.v2.validation import validate_record


@pytest.mark.parametrize("role", list(ALL_ROLES), ids=lambda r: r.value)
def test_critic_catches_its_fault_fixture(role: CriticRole) -> None:
    critic = next(c for c in default_critics() if c.role is role)
    subject = load_fault_subject(role)
    assert critic.applies_to(subject)
    findings = critic.critique(subject, evidence_for(subject.subject_id))
    assert findings, f"{role.value} missed its fault fixture"
    for finding in findings:
        assert finding.evidence
        assert finding.measured
        numeric = [
            v
            for v in finding.measured.values()
            if isinstance(v, (int, float)) and not isinstance(v, bool)
        ]
        assert numeric, finding.measured


@pytest.mark.parametrize("role", list(ALL_ROLES), ids=lambda r: r.value)
def test_critic_does_not_fire_on_control(role: CriticRole) -> None:
    critic = next(c for c in default_critics() if c.role is role)
    subject = load_control_subject(role)
    assert critic.applies_to(subject)
    findings = critic.critique(subject, evidence_for(subject.subject_id))
    assert findings == [], f"{role.value} false positive: {findings}"


def test_finding_without_evidence_raises() -> None:
    with pytest.raises(ValidationError):
        CriticFinding(
            finding_id="x",
            critic_role="product_photographer",
            diagnosis="d",
            evidence=[],
            measured={"v": 1.0},
        )


def test_finding_without_measured_quantity_raises() -> None:
    with pytest.raises(ValidationError, match="no measured"):
        require_measured({}, finding_id="x")
    with pytest.raises(ValidationError, match="no numeric"):
        require_measured({"note": "opinion"}, finding_id="x")
    with pytest.raises(ValidationError, match="no measured"):
        make_finding(
            finding_id="x",
            role=CriticRole.PRODUCT_PHOTOGRAPHER,
            diagnosis="d",
            evidence=["e"],
            measured={},
        )


def test_workspace_seals_perceptual_critique() -> None:
    role = CriticRole.MATERIAL_ARTIST
    subject = load_fault_subject(role)
    evidence = evidence_for(subject.subject_id)
    record = CriticWorkspace().run(subject, evidence)
    record.verify()
    validate_record(record)
    assert record.passed is False
    assert role.value in record.critics_run
    assert record.blocking_findings()


def test_repair_plan_declares_blast_radius() -> None:
    role = CriticRole.MATERIAL_ARTIST
    subject = load_fault_subject(role)
    findings = next(c for c in default_critics() if c.role is role).critique(
        subject, evidence_for(subject.subject_id)
    )
    plan = plan_from_finding(findings[0])
    assert plan.blast_radius
    assert plan.parameters
    assert plan.acceptance_test


def test_bounded_repair_acceptance_and_no_unrelated_regression() -> None:
    role = CriticRole.MATERIAL_ARTIST
    subject = load_fault_subject(role)
    evidence = evidence_for(subject.subject_id)
    workspace = CriticWorkspace()
    before = workspace.run(subject, evidence)
    finding = before.blocking_findings()[0]
    plan = plan_from_finding(finding)
    control = load_control_subject(CriticRole.PRODUCT_PHOTOGRAPHER)
    result = BoundedRepairRunner(workspace).apply(
        subject,
        evidence,
        plan,
        baseline=before,
        unrelated_subjects=[(control, evidence_for(control.subject_id))],
    )
    assert result.acceptance_passed
    assert result.global_regression is False
    assert finding.finding_id not in {f.finding_id for f in result.after.blocking_findings()}


def test_thirteen_specialists_registered() -> None:
    critics = default_critics()
    assert len(critics) == 13
    assert {c.role for c in critics} == set(ALL_ROLES)


def test_performance_engineer_refuses_simulated_measurements() -> None:
    from blender_vision.critics.performance_engineer import PerformanceEngineerCritic

    critic = PerformanceEngineerCritic()
    subject = CritiqueSubject(
        subject_id="sim",
        kind="perf",
        metrics={
            "frame_times_ms": [50, 60, 70],
            "measurements_are_simulated": True,
        },
    )
    with pytest.raises(ValidationError, match="refuses simulated"):
        critic.critique(subject, CritiqueEvidence(references=["e"]))
