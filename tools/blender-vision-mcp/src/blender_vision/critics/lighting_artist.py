"""Lighting artist critic: flat corridor, overfilled shadows, clipping, floaters."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import highlight_clip_fraction
from blender_vision.v2.records import CriticFinding


class LightingArtistCritic(Critic):
    role = CriticRole.LIGHTING_ARTIST

    MIN_LUMINANCE_VARIANCE = 0.008
    MAX_SHADOW_FLOOR = 0.22
    MAX_CLIP_FRACTION = 0.03
    MIN_CONTACT_SHADOW = 0.08

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "luminance_variance",
                "shadow_floor",
                "image",
                "contact_shadow_strength",
            )
        ) or subject.get("image") is not None

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        variance = m.get("luminance_variance")
        if variance is not None and float(variance) < self.MIN_LUMINANCE_VARIANCE:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:flat-corridor",
                    role=self.role,
                    diagnosis="lighting is a flat corridor with insufficient contrast",
                    evidence=refs,
                    measured={"luminance_variance": float(variance)},
                    severity="major",
                    likely_cause="key/fill ratio too low; missing negative fill",
                    bounded_repair={
                        "parameters": ["key_intensity", "fill_intensity", "negative_fill"],
                        "delta": {"key_intensity": +0.4, "fill_intensity": -0.2},
                    },
                    blast_radius=["lighting"],
                    acceptance_test="luminance_variance >= 0.008",
                )
            )

        shadow_floor = m.get("shadow_floor")
        if shadow_floor is not None and float(shadow_floor) > self.MAX_SHADOW_FLOOR:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:overfilled-shadows",
                    role=self.role,
                    diagnosis="shadows are overfilled and lose contact grounding",
                    evidence=refs,
                    measured={"shadow_floor": float(shadow_floor)},
                    severity="major",
                    likely_cause="fill light too strong relative to key",
                    bounded_repair={
                        "parameters": ["fill_intensity", "shadow_floor"],
                        "delta": {"fill_intensity": -0.3},
                    },
                    blast_radius=["lighting"],
                    acceptance_test="shadow_floor <= 0.22",
                )
            )

        image = subject.get("image")
        clip = m.get("highlight_clip_fraction")
        if clip is None and image is not None:
            clip = highlight_clip_fraction(image)
        if clip is not None and float(clip) > self.MAX_CLIP_FRACTION:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:clipped-hero",
                    role=self.role,
                    diagnosis="hero highlight region is clipped",
                    evidence=refs,
                    measured={"highlight_clip_fraction": float(clip)},
                    severity="major",
                    likely_cause="exposure or key too high on hero surface",
                    bounded_repair={
                        "parameters": ["exposure"],
                        "delta": {"exposure": -0.4},
                    },
                    blast_radius=["lighting", "exposure"],
                    acceptance_test="highlight_clip_fraction <= 0.03",
                )
            )

        contact = m.get("contact_shadow_strength")
        if contact is not None and float(contact) < self.MIN_CONTACT_SHADOW:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:floating-object",
                    role=self.role,
                    diagnosis="object appears floating due to missing contact shadow",
                    evidence=refs,
                    measured={"contact_shadow_strength": float(contact)},
                    severity="major",
                    likely_cause="contact shadow disabled or ambient occlusion too weak",
                    bounded_repair={
                        "parameters": ["contact_shadow_strength"],
                        "delta": {"contact_shadow_strength": +0.2},
                    },
                    blast_radius=["lighting", "ground_contact"],
                    acceptance_test="contact_shadow_strength >= 0.08",
                )
            )
        return findings
