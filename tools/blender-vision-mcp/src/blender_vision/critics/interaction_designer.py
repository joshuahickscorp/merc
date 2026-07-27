"""Interaction designer critic: latency, dead zones, skip/get-app discoverability."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.v2.records import CriticFinding


class InteractionDesignerCritic(Critic):
    role = CriticRole.INTERACTION_DESIGNER

    MAX_RESPONSE_LATENCY_MS = 100.0
    MAX_DEAD_ZONE_FRACTION = 0.05
    MIN_DISCOVERABILITY = 0.6

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "response_latency_ms",
                "dead_zone_fraction",
                "skip_discoverability",
                "get_app_discoverability",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        latency = m.get("response_latency_ms")
        if latency is not None and float(latency) > self.MAX_RESPONSE_LATENCY_MS:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:response-latency",
                    role=self.role,
                    diagnosis="interaction response latency exceeds budget",
                    evidence=refs,
                    measured={"response_latency_ms": float(latency)},
                    severity="major",
                    likely_cause="main-thread work on input path",
                    bounded_repair={
                        "parameters": ["response_latency_ms"],
                        "action": "defer_noncritical_work",
                    },
                    blast_radius=["interaction", "main_thread"],
                    acceptance_test="response_latency_ms <= 100",
                )
            )

        dead = m.get("dead_zone_fraction")
        if dead is not None and float(dead) > self.MAX_DEAD_ZONE_FRACTION:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:dead-zones",
                    role=self.role,
                    diagnosis="interactive dead zones cover too much of the viewport",
                    evidence=refs,
                    measured={"dead_zone_fraction": float(dead)},
                    severity="major",
                    likely_cause="overlay intercepts pointer without handlers",
                    bounded_repair={
                        "parameters": ["dead_zone_fraction", "hit_targets"],
                        "action": "expand_hit_targets",
                    },
                    blast_radius=["interaction", "layout"],
                    acceptance_test="dead_zone_fraction <= 0.05",
                )
            )

        for key, label in (
            ("skip_discoverability", "skip"),
            ("get_app_discoverability", "get-app"),
        ):
            score = m.get(key)
            if score is not None and float(score) < self.MIN_DISCOVERABILITY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:{label}-discoverability",
                        role=self.role,
                        diagnosis=f"{label} control discoverability is too low",
                        evidence=refs,
                        measured={key: float(score)},
                        severity="major",
                        likely_cause=f"{label} control contrast/placement insufficient",
                        bounded_repair={
                            "parameters": [key, f"{label}_control"],
                            "action": "increase_affordance",
                        },
                        blast_radius=["interaction", "chrome"],
                        acceptance_test=f"{key} >= 0.6",
                    )
                )
        return findings
