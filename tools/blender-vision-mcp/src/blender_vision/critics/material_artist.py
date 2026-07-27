"""Material artist critic: plastic metal, pore scale, painted depth."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import (
    flat_depth_pretence,
    plastic_metal_score,
    pore_scale_ratio,
)
from blender_vision.v2.records import CriticFinding


class MaterialArtistCritic(Critic):
    role = CriticRole.MATERIAL_ARTIST

    MAX_PLASTIC_METAL = 0.18
    MAX_PORE_SCALE_RATIO = 4.0
    MIN_PORE_SCALE_RATIO = 0.25
    MAX_FLAT_DEPTH = 25.0

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "metalness",
                "roughness",
                "texture_scale_m",
                "albedo_variance",
                "normal_variance",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        if all(k in m for k in ("metalness", "roughness", "specular_peak")):
            score = plastic_metal_score(
                float(m["metalness"]), float(m["roughness"]), float(m["specular_peak"])
            )
            if score > self.MAX_PLASTIC_METAL:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:plastic-metal",
                        role=self.role,
                        diagnosis="surface reads as plastic metal (high metalness, soft specular)",
                        evidence=refs,
                        measured={"plastic_metal_score": float(score)},
                        severity="major",
                        likely_cause="metalness too high for dielectric look or roughness wrong",
                        bounded_repair={
                            "parameters": ["metalness", "roughness", "specular_peak"],
                            "delta": {"metalness": -0.4, "roughness": -0.2},
                        },
                        blast_radius=["materials"],
                        acceptance_test="plastic_metal_score <= 0.18",
                    )
                )

        if "texture_scale_m" in m and "feature_scale_m" in m:
            ratio = pore_scale_ratio(float(m["texture_scale_m"]), float(m["feature_scale_m"]))
            if ratio > self.MAX_PORE_SCALE_RATIO or ratio < self.MIN_PORE_SCALE_RATIO:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:wrong-pore-scale",
                        role=self.role,
                        diagnosis="pore/texture scale inconsistent with surface feature scale",
                        evidence=refs,
                        measured={
                            "pore_scale_ratio": float(ratio),
                            "texture_scale_m": float(m["texture_scale_m"]),
                            "feature_scale_m": float(m["feature_scale_m"]),
                        },
                        severity="major",
                        likely_cause="UV scale or texture_scale_m mis-set relative to object size",
                        bounded_repair={
                            "parameters": ["texture_scale_m"],
                            "target_ratio": 1.0,
                        },
                        blast_radius=["materials", "textures"],
                        acceptance_test="0.25 <= pore_scale_ratio <= 4.0",
                    )
                )

        if "albedo_variance" in m and "normal_variance" in m:
            pretence = flat_depth_pretence(
                float(m["albedo_variance"]), float(m["normal_variance"])
            )
            if pretence > self.MAX_FLAT_DEPTH:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:flat-texture-depth",
                        role=self.role,
                        diagnosis="flat texture is pretending to be geometric depth",
                        evidence=refs,
                        measured={"flat_depth_pretence": float(pretence)},
                        severity="major",
                        likely_cause="albedo baked AO/shadows with near-constant normals",
                        bounded_repair={
                            "parameters": ["normal_variance", "displacement"],
                            "action": "add_micro_normals",
                        },
                        blast_radius=["materials", "normals"],
                        acceptance_test="flat_depth_pretence <= 25.0",
                    )
                )
        return findings
