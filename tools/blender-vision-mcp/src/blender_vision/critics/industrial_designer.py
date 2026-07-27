"""Industrial designer critic: proportion, part count, fake interior geometry."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import depth_variance
from blender_vision.v2.records import CriticFinding


class IndustrialDesignerCritic(Critic):
    role = CriticRole.INDUSTRIAL_DESIGNER

    MAX_PROPORTION_ERROR = 0.08
    MIN_PART_COUNT = 1
    MIN_DRAWER_DEPTH_VARIANCE = 1e-4

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "declared_dimensions_m",
                "observed_dimensions_m",
                "part_count",
                "drawer_depth_samples_m",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        declared = m.get("declared_dimensions_m")
        observed = m.get("observed_dimensions_m")
        if declared is not None and observed is not None:
            errors = []
            for axis in ("x", "y", "z"):
                d = float(declared[axis])
                o = float(observed[axis])
                if d <= 0:
                    continue
                errors.append(abs(o - d) / d)
            if errors:
                max_err = max(errors)
                if max_err > self.MAX_PROPORTION_ERROR:
                    findings.append(
                        make_finding(
                            finding_id=f"{subject.subject_id}:proportion-scale",
                            role=self.role,
                            diagnosis="proportion/scale implausible against declared dimensions",
                            evidence=refs,
                            measured={"max_relative_dimension_error": float(max_err)},
                            severity="major",
                            likely_cause="scale authority unresolved or wrong unit conversion",
                            bounded_repair={
                                "parameters": ["observed_dimensions_m", "uniform_scale"],
                                "action": "rescale_to_declared",
                            },
                            blast_radius=["geometry", "scale"],
                            acceptance_test="max_relative_dimension_error <= 0.08",
                        )
                    )

        part_count = m.get("part_count")
        expected_min = int(m.get("expected_min_parts", self.MIN_PART_COUNT))
        if part_count is not None and int(part_count) < expected_min:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:part-count",
                    role=self.role,
                    diagnosis="part count below plausible assembly minimum",
                    evidence=refs,
                    measured={
                        "part_count": int(part_count),
                        "expected_min_parts": expected_min,
                    },
                    severity="major",
                    likely_cause="over-merged solid with no assembly structure",
                    bounded_repair={
                        "parameters": ["part_count"],
                        "action": "split_semantic_parts",
                    },
                    blast_radius=["geometry", "semantic_parts"],
                    acceptance_test=f"part_count >= {expected_min}",
                )
            )

        depths = m.get("drawer_depth_samples_m")
        if depths is not None:
            var = depth_variance(depths)
            if var < self.MIN_DRAWER_DEPTH_VARIANCE:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:fake-drawer",
                        role=self.role,
                        diagnosis="drawer-like part has no internal depth structure",
                        evidence=refs,
                        measured={"drawer_depth_variance": float(var)},
                        severity="critical",
                        likely_cause="decal or face-only geometry pretending to be a drawer",
                        bounded_repair={
                            "parameters": ["drawer_depth_samples_m"],
                            "action": "add_interior_cavity",
                        },
                        blast_radius=["geometry", "cabinetry"],
                        acceptance_test="drawer_depth_variance >= 1e-4",
                    )
                )
        return findings
