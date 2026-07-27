"""Editorial art director: generic composition, templates, over-explanation."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import composition_center_bias, text_volume_per_beat
from blender_vision.v2.records import CriticFinding


class EditorialArtDirectorCritic(Critic):
    role = CriticRole.EDITORIAL_ART_DIRECTOR

    # Low distance to thirds intersections is intentional; high center bias is generic.
    MAX_CENTER_BIAS = 0.22
    MAX_TEMPLATE_SCORE = 0.55
    MAX_TEXT_VOLUME = 40.0

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m for key in ("salient_xy", "template_similarity", "narrative_beats")
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        salient = m.get("salient_xy")
        if salient is not None:
            # Distance from image center: high means intentional off-center; low is generic.
            cx = abs(float(salient[0]) - 0.5)
            cy = abs(float(salient[1]) - 0.5)
            center_pull = 1.0 - min(1.0, (cx * cx + cy * cy) ** 0.5 * 2.0)
            thirds_dist = composition_center_bias(salient)
            # Generic if stuck at dead center (high center_pull) and far from thirds.
            generic_score = center_pull * min(1.0, thirds_dist / 0.3)
            if generic_score > self.MAX_CENTER_BIAS:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:generic-composition",
                        role=self.role,
                        diagnosis="composition is generic (dead-center, template-like framing)",
                        evidence=refs,
                        measured={
                            "generic_composition_score": float(generic_score),
                            "thirds_distance": float(thirds_dist),
                        },
                        severity="major",
                        likely_cause="default centered crop without editorial framing",
                        bounded_repair={
                            "parameters": ["salient_xy", "crop"],
                            "action": "reframe_to_thirds",
                        },
                        blast_radius=["composition", "layout"],
                        acceptance_test="generic_composition_score <= 0.22",
                    )
                )

        template = m.get("template_similarity")
        if template is not None and float(template) > self.MAX_TEMPLATE_SCORE:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:template-appearance",
                    role=self.role,
                    diagnosis="layout appearance is too close to a stock template",
                    evidence=refs,
                    measured={"template_similarity": float(template)},
                    severity="major",
                    likely_cause="unmodified template skeleton retained",
                    bounded_repair={
                        "parameters": ["template_similarity", "layout_offsets"],
                        "action": "break_template_axes",
                    },
                    blast_radius=["layout", "brand"],
                    acceptance_test="template_similarity <= 0.55",
                )
            )

        beats = m.get("narrative_beats")
        if beats is not None:
            volume = text_volume_per_beat(list(beats))
            if volume > self.MAX_TEXT_VOLUME:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:over-explanation",
                        role=self.role,
                        diagnosis="text volume per beat over-explains the narrative",
                        evidence=refs,
                        measured={"text_volume_per_beat": float(volume)},
                        severity="major",
                        likely_cause="copy blocks not paced to scroll beats",
                        bounded_repair={
                            "parameters": ["narrative_beats"],
                            "action": "cut_text_per_beat",
                        },
                        blast_radius=["copy", "narrative"],
                        acceptance_test="text_volume_per_beat <= 40",
                    )
                )
        return findings
