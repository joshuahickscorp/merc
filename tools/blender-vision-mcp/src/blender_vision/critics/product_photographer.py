"""Product photographer critic: silhouette readability, clipping, background separation."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import (
    background_separation,
    highlight_clip_fraction,
    silhouette_edge_strength,
)
from blender_vision.v2.records import CriticFinding


class ProductPhotographerCritic(Critic):
    role = CriticRole.PRODUCT_PHOTOGRAPHER

    # Tuned so deliberate fault fixtures fail and the clean control passes.
    MIN_EDGE_STRENGTH = 0.35
    MIN_BACKGROUND_SEPARATION = 0.18
    MAX_CLIP_FRACTION = 0.02

    def applies_to(self, subject: CritiqueSubject) -> bool:
        return subject.get("image") is not None and subject.get("mask") is not None

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        image = subject.get("image")
        mask = subject.get("mask")
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []

        edge = silhouette_edge_strength(mask)
        if edge < self.MIN_EDGE_STRENGTH:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:silhouette-readability",
                    role=self.role,
                    diagnosis="silhouette readability is too low for product photography",
                    evidence=refs,
                    measured={"silhouette_edge_strength": edge},
                    severity="major",
                    likely_cause="soft matte or low contrast against background",
                    bounded_repair={
                        "parameters": ["background_luminance", "edge_contrast"],
                        "delta": {"edge_contrast": +0.4},
                    },
                    blast_radius=["product_render", "hero_still"],
                    acceptance_test="silhouette_edge_strength >= 0.35",
                )
            )

        separation = background_separation(image, mask)
        if separation < self.MIN_BACKGROUND_SEPARATION:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:background-separation",
                    role=self.role,
                    diagnosis="insufficient background separation for hero product still",
                    evidence=refs,
                    measured={"background_separation": separation},
                    severity="major",
                    likely_cause="background luminance too close to subject",
                    bounded_repair={
                        "parameters": ["background_luminance"],
                        "delta": {"background_luminance": -0.25},
                    },
                    blast_radius=["product_render"],
                    acceptance_test="background_separation >= 0.18",
                )
            )

        clip = highlight_clip_fraction(image)
        if clip > self.MAX_CLIP_FRACTION:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:hero-surface-clipping",
                    role=self.role,
                    diagnosis="hero surface highlight clipping",
                    evidence=refs,
                    measured={"highlight_clip_fraction": clip},
                    severity="major",
                    likely_cause="exposure too high on specular hero surface",
                    bounded_repair={
                        "parameters": ["exposure"],
                        "delta": {"exposure": -0.5},
                    },
                    blast_radius=["product_render", "lighting"],
                    acceptance_test="highlight_clip_fraction <= 0.02",
                )
            )
        return findings
