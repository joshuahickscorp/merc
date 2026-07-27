"""Organic artist critic: silhouette naturalness and over-perfect symmetry."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import curvature_irregularity, left_right_symmetry
from blender_vision.v2.records import CriticFinding


class OrganicArtistCritic(Critic):
    role = CriticRole.ORGANIC_ARTIST

    MIN_CURVATURE_VARIANCE = 0.008
    MAX_SYMMETRY = 0.94

    def applies_to(self, subject: CritiqueSubject) -> bool:
        return subject.get("mask") is not None or subject.get("contour_xy") is not None

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []

        contour = subject.get("contour_xy")
        if contour is not None:
            irreg = curvature_irregularity(contour)
            if irreg < self.MIN_CURVATURE_VARIANCE:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:silhouette-naturalness",
                        role=self.role,
                        diagnosis="organic silhouette is unnaturally smooth",
                        evidence=refs,
                        measured={"curvature_variance": float(irreg)},
                        severity="major",
                        likely_cause="over-smoothed mesh or pure primitive silhouette",
                        bounded_repair={
                            "parameters": ["contour_xy", "surface_noise"],
                            "action": "add_organic_irregularity",
                        },
                        blast_radius=["organic_geometry"],
                        acceptance_test="curvature_variance >= 0.008",
                    )
                )

        mask = subject.get("mask")
        if mask is not None:
            sym = left_right_symmetry(mask)
            if sym > self.MAX_SYMMETRY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:over-symmetry",
                        role=self.role,
                        diagnosis="symmetry is too perfect for a natural organic form",
                        evidence=refs,
                        measured={"left_right_symmetry": float(sym)},
                        severity="major",
                        likely_cause="mirrored half without asymmetry or micro-variation",
                        bounded_repair={
                            "parameters": ["mask", "asymmetry_amplitude"],
                            "action": "break_perfect_symmetry",
                        },
                        blast_radius=["organic_geometry"],
                        acceptance_test="left_right_symmetry <= 0.94",
                    )
                )
        return findings
