"""Environment artist critic: procedural sameness and empty-box scenes."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import occupied_volume_fraction, shannon_entropy
from blender_vision.v2.records import CriticFinding


class EnvironmentArtistCritic(Critic):
    role = CriticRole.ENVIRONMENT_ARTIST

    MIN_INSTANCE_ENTROPY = 1.2
    MIN_OCCUPIED_FRACTION = 0.12
    MIN_DEPTH_COMPLEXITY = 1.5

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in ("instance_variations", "occupancy_grid", "depth_complexity_samples")
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        variations = m.get("instance_variations")
        if variations is not None:
            entropy = shannon_entropy(variations)
            if entropy < self.MIN_INSTANCE_ENTROPY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:procedural-sameness",
                        role=self.role,
                        diagnosis="repeated procedural sameness across instances",
                        evidence=refs,
                        measured={"instance_variation_entropy": float(entropy)},
                        severity="major",
                        likely_cause="instance seed collapsed or variation parameters zeroed",
                        bounded_repair={
                            "parameters": ["instance_variations", "instance_seed_spread"],
                            "action": "increase_instance_entropy",
                        },
                        blast_radius=["procedural_instances"],
                        acceptance_test="instance_variation_entropy >= 1.2",
                    )
                )

        occupancy = m.get("occupancy_grid")
        if occupancy is not None:
            fraction = occupied_volume_fraction(occupancy)
            if fraction < self.MIN_OCCUPIED_FRACTION:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:empty-box",
                        role=self.role,
                        diagnosis="scene reads as an empty box",
                        evidence=refs,
                        measured={"occupied_volume_fraction": float(fraction)},
                        severity="major",
                        likely_cause="missing mid-ground set dressing or props",
                        bounded_repair={
                            "parameters": ["occupancy_grid"],
                            "action": "populate_midground",
                        },
                        blast_radius=["scene_layout", "props"],
                        acceptance_test="occupied_volume_fraction >= 0.12",
                    )
                )

        depths = m.get("depth_complexity_samples")
        if depths is not None and depths:
            complexity = float(sum(depths) / len(depths))
            if complexity < self.MIN_DEPTH_COMPLEXITY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:depth-complexity",
                        role=self.role,
                        diagnosis="depth complexity along camera path is too flat",
                        evidence=refs,
                        measured={"mean_depth_complexity": complexity},
                        severity="minor",
                        likely_cause="single-plane set with no layered occlusion",
                        bounded_repair={
                            "parameters": ["depth_complexity_samples"],
                            "action": "add_occlusion_layers",
                        },
                        blast_radius=["scene_layout"],
                        acceptance_test="mean_depth_complexity >= 1.5",
                    )
                )
        return findings
