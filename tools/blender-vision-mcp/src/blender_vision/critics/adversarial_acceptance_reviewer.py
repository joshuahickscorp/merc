"""Adversarial acceptance reviewer: cheapest dishonest paths to a green result."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.v2.records import CriticFinding


class AdversarialAcceptanceReviewerCritic(Critic):
    role = CriticRole.ADVERSARIAL_ACCEPTANCE_REVIEWER

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "reference_class_declared",
                "reference_class_actual",
                "hidden_view_score_delta",
                "threshold_original",
                "threshold_applied",
                "detail_removed_fraction",
                "budget_claim_met",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        declared = m.get("reference_class_declared")
        actual = m.get("reference_class_actual")
        if declared is not None and actual is not None and str(declared) != str(actual):
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:reference-class-redefinition",
                    role=self.role,
                    diagnosis="acceptance redefined the reference class to a weaker cohort",
                    evidence=refs,
                    measured={
                        "reference_class_mismatch": 1.0,
                        "declared_class_len": float(len(str(declared))),
                        "actual_class_len": float(len(str(actual))),
                    },
                    severity="critical",
                    likely_cause="benchmark cherry-picking via reference redefinition",
                    bounded_repair={
                        "parameters": ["reference_class_actual"],
                        "action": "restore_declared_reference_class",
                    },
                    blast_radius=["acceptance", "benchmark"],
                    acceptance_test="reference_class_declared == reference_class_actual",
                )
            )

        leakage = m.get("hidden_view_score_delta")
        if leakage is not None and float(leakage) > 0.05:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:hidden-view-leakage",
                    role=self.role,
                    diagnosis="held-out view quality collapses relative to scored views",
                    evidence=refs,
                    measured={"hidden_view_score_delta": float(leakage)},
                    severity="critical",
                    likely_cause="overfit to scored cameras; hidden view not constrained",
                    bounded_repair={
                        "parameters": ["camera_set", "loss_views"],
                        "action": "include_held_out_views",
                    },
                    blast_radius=["acceptance", "reconstruction"],
                    acceptance_test="hidden_view_score_delta <= 0.05",
                )
            )

        original = m.get("threshold_original")
        applied = m.get("threshold_applied")
        if original is not None and applied is not None:
            orig = float(original)
            app = float(applied)
            # Relaxation means the applied gate is easier (higher allowed error, etc.).
            relaxation = app - orig if m.get("threshold_higher_is_easier", True) else orig - app
            if relaxation > 1e-9:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:threshold-relaxation",
                        role=self.role,
                        diagnosis="acceptance threshold was relaxed to pass",
                        evidence=refs,
                        measured={
                            "threshold_relaxation": float(relaxation),
                            "threshold_original": orig,
                            "threshold_applied": app,
                        },
                        severity="critical",
                        likely_cause="gate moved after seeing the result",
                        bounded_repair={
                            "parameters": ["threshold_applied"],
                            "action": "restore_original_threshold",
                        },
                        blast_radius=["acceptance"],
                        acceptance_test="threshold_applied == threshold_original",
                    )
                )

        removed = m.get("detail_removed_fraction")
        budget_met = m.get("budget_claim_met")
        if removed is not None and budget_met is True and float(removed) > 0.15:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:detail-removed-for-budget",
                    role=self.role,
                    diagnosis="detail was removed primarily to hit a performance budget",
                    evidence=refs,
                    measured={
                        "detail_removed_fraction": float(removed),
                        "budget_claim_met": 1.0,
                    },
                    severity="major",
                    likely_cause="LOD collapse without visual-equivalence gate",
                    bounded_repair={
                        "parameters": ["detail_removed_fraction", "lod"],
                        "action": "restore_detail_with_budgeted_lod",
                    },
                    blast_radius=["delivery", "lod", "acceptance"],
                    acceptance_test="detail_removed_fraction <= 0.15 or visual_equivalence_pass",
                )
            )
        return findings
