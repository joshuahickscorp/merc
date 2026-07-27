"""Accessibility reviewer: contrast, focus order, reduced motion, text equivalents."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import contrast_ratio
from blender_vision.v2.records import CriticFinding


class AccessibilityReviewerCritic(Critic):
    role = CriticRole.ACCESSIBILITY_REVIEWER

    MIN_CONTRAST = 4.5
    MIN_FOCUS_ORDER_COMPLETENESS = 1.0
    MIN_REDUCED_MOTION_EQUIVALENCE = 0.95
    MIN_TEXT_EQUIVALENT = 1.0

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "fg_luminance",
                "bg_luminance",
                "contrast_ratio",
                "focus_order_completeness",
                "reduced_motion_equivalence",
                "textual_equivalent_presence",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        ratio = m.get("contrast_ratio")
        if ratio is None and "fg_luminance" in m and "bg_luminance" in m:
            ratio = contrast_ratio(float(m["fg_luminance"]), float(m["bg_luminance"]))
        if ratio is not None and float(ratio) < self.MIN_CONTRAST:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:contrast-ratio",
                    role=self.role,
                    diagnosis="text/icon contrast ratio below WCAG AA 4.5:1",
                    evidence=refs,
                    measured={"contrast_ratio": float(ratio)},
                    severity="critical",
                    likely_cause="foreground colour too close to background",
                    bounded_repair={
                        "parameters": ["fg_luminance", "bg_luminance", "text_color"],
                        "action": "raise_contrast_to_aa",
                    },
                    blast_radius=["theme", "typography"],
                    acceptance_test="contrast_ratio >= 4.5",
                )
            )

        focus = m.get("focus_order_completeness")
        if focus is not None and float(focus) < self.MIN_FOCUS_ORDER_COMPLETENESS:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:focus-order",
                    role=self.role,
                    diagnosis="focus order is incomplete for interactive controls",
                    evidence=refs,
                    measured={"focus_order_completeness": float(focus)},
                    severity="major",
                    likely_cause="controls missing tabindex or aria ordering",
                    bounded_repair={
                        "parameters": ["focus_order_completeness", "tab_order"],
                        "action": "complete_focus_order",
                    },
                    blast_radius=["a11y", "interaction"],
                    acceptance_test="focus_order_completeness >= 1.0",
                )
            )

        reduced = m.get("reduced_motion_equivalence")
        if reduced is not None and float(reduced) < self.MIN_REDUCED_MOTION_EQUIVALENCE:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:reduced-motion",
                    role=self.role,
                    diagnosis="reduced-motion path loses content relative to animated path",
                    evidence=refs,
                    measured={"reduced_motion_equivalence": float(reduced)},
                    severity="major",
                    likely_cause="static fallback omits narrative beats",
                    bounded_repair={
                        "parameters": ["reduced_motion_views"],
                        "action": "restore_beat_coverage",
                    },
                    blast_radius=["a11y", "motion"],
                    acceptance_test="reduced_motion_equivalence >= 0.95",
                )
            )

        text_eq = m.get("textual_equivalent_presence")
        if text_eq is not None and float(text_eq) < self.MIN_TEXT_EQUIVALENT:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:textual-equivalent",
                    role=self.role,
                    diagnosis="visual content lacks a textual equivalent",
                    evidence=refs,
                    measured={"textual_equivalent_presence": float(text_eq)},
                    severity="major",
                    likely_cause="missing alt text or aria-label on informative media",
                    bounded_repair={
                        "parameters": ["textual_equivalent_presence", "alt_text"],
                        "action": "add_text_equivalents",
                    },
                    blast_radius=["a11y", "media"],
                    acceptance_test="textual_equivalent_presence >= 1.0",
                )
            )
        return findings
