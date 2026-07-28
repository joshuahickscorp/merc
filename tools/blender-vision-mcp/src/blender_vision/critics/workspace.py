"""Run applicable critics and seal a PerceptualCritique record."""

from __future__ import annotations

import hashlib
from typing import Any

from blender_vision.core.util import canonical_json, utc_now
from blender_vision.critics.base import Critic, CritiqueEvidence, CritiqueSubject
from blender_vision.critics.registry import default_critics
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import Lineage, PerceptualCritique


class CriticWorkspace:
    """Aggregate specialist findings into a sealed PerceptualCritique."""

    def __init__(self, critics: list[Critic] | None = None):
        self.critics = list(critics) if critics is not None else default_critics()

    def applicable(self, subject: CritiqueSubject) -> list[Critic]:
        return [critic for critic in self.critics if critic.applies_to(subject)]

    def run(
        self,
        subject: CritiqueSubject,
        evidence: CritiqueEvidence,
        *,
        critique_id: str | None = None,
        input_authorities: list[str] | None = None,
    ) -> PerceptualCritique:
        applicable = self.applicable(subject)
        findings = []
        critics_run: list[str] = []
        for critic in applicable:
            critics_run.append(critic.role.value)
            batch = critic.critique(subject, evidence)
            for finding in batch:
                if not finding.measured:
                    raise ValueError(
                        f"critic {critic.role.value} returned finding "
                        f"{finding.finding_id} without measured quantity"
                    )
                if not finding.evidence:
                    raise ValueError(
                        f"critic {critic.role.value} returned finding "
                        f"{finding.finding_id} without evidence"
                    )
                findings.append(finding)

        authorities = list(input_authorities or [AuthorityClass.OBSERVED.value])
        authority = derive(authorities, proposed=AuthorityClass.INFERRED)
        identity = {
            "subject_id": subject.subject_id,
            "subject_kind": subject.kind,
            "critics_run": critics_run,
            "finding_ids": [item.finding_id for item in findings],
        }
        digest_seed = hashlib.sha256(canonical_json(identity)).hexdigest()[:16]
        record_id = critique_id or f"critique-{subject.subject_id}-{digest_seed}"
        passed = not any(item.severity in {"major", "critical"} for item in findings)
        record = PerceptualCritique(
            id=record_id,
            created_at=utc_now(),
            authority=authority,
            lineage=Lineage(
                operation="critics.workspace.run",
                inputs=list(evidence.references) or [f"subject:{subject.subject_id}"],
                input_authorities=authorities,
                parameters={"subject_kind": subject.kind, "critics_run": critics_run},
            ),
            uncertainty=Uncertainty(
                kind="perceptual-findings",
                sigma=float(len([f for f in findings if f.severity in {"major", "critical"}])),
                units=Units.UNITLESS,
                basis="count of major/critical findings",
                samples=len(findings),
            ),
            subject_id=subject.subject_id,
            subject_kind=subject.kind,
            findings=findings,
            critics_run=critics_run,
            passed=passed,
        )
        return record.seal()

    def run_roles(
        self,
        subject: CritiqueSubject,
        evidence: CritiqueEvidence,
        roles: list[str],
    ) -> PerceptualCritique:
        selected = [c for c in self.critics if c.role.value in roles]
        return CriticWorkspace(selected).run(subject, evidence)


def critique_to_summary(record: PerceptualCritique) -> dict[str, Any]:
    return {
        "id": record.id,
        "subject_id": record.subject_id,
        "passed": record.passed,
        "critics_run": list(record.critics_run),
        "finding_count": len(record.findings),
        "blocking": [
            {
                "finding_id": item.finding_id,
                "role": item.critic_role,
                "severity": item.severity,
                "diagnosis": item.diagnosis,
                "measured": dict(item.measured),
            }
            for item in record.blocking_findings()
        ],
        "digest": record.digest,
    }
