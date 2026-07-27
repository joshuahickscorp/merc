"""Groom artist critic: fur clump scale and density plausibility."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.v2.records import CriticFinding


class GroomArtistCritic(Critic):
    role = CriticRole.GROOM_ARTIST

    MAX_CLUMP_TO_BODY = 0.35
    MIN_DENSITY = 20.0
    MAX_DENSITY = 800.0

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(key in m for key in ("fur_clump_scale_m", "fur_density_per_m2", "body_scale_m"))

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        clump = m.get("fur_clump_scale_m")
        body = m.get("body_scale_m")
        if clump is not None and body is not None and float(body) > 0:
            ratio = float(clump) / float(body)
            if ratio > self.MAX_CLUMP_TO_BODY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:fur-clump-scale",
                        role=self.role,
                        diagnosis="fur clump scale is too large relative to body scale",
                        evidence=refs,
                        measured={
                            "clump_to_body_ratio": float(ratio),
                            "fur_clump_scale_m": float(clump),
                            "body_scale_m": float(body),
                        },
                        severity="major",
                        likely_cause="clump radius authored in wrong units",
                        bounded_repair={
                            "parameters": ["fur_clump_scale_m"],
                            "delta": {"fur_clump_scale_m": "body_scale_m * 0.12"},
                        },
                        blast_radius=["groom"],
                        acceptance_test="clump_to_body_ratio <= 0.35",
                    )
                )

        density = m.get("fur_density_per_m2")
        if density is not None:
            d = float(density)
            if d < self.MIN_DENSITY or d > self.MAX_DENSITY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:fur-density",
                        role=self.role,
                        diagnosis="fur density is outside plausible groom range",
                        evidence=refs,
                        measured={"fur_density_per_m2": d},
                        severity="major",
                        likely_cause="density map scaled incorrectly or missing",
                        bounded_repair={
                            "parameters": ["fur_density_per_m2"],
                            "clamp": [self.MIN_DENSITY, self.MAX_DENSITY],
                        },
                        blast_radius=["groom"],
                        acceptance_test="20 <= fur_density_per_m2 <= 800",
                    )
                )
        return findings
