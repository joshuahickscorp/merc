"""Cinematographer critic: shot variety, camera lag, dead scroll, intentional turn."""

from __future__ import annotations

from blender_vision.critics.base import (
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
)
from blender_vision.critics.measures import dead_scroll_fraction
from blender_vision.v2.records import CriticFinding


class CinematographerCritic(Critic):
    role = CriticRole.CINEMATOGRAPHER

    MIN_SHOT_VARIETY = 0.45
    MAX_CAMERA_LAG = 0.12
    MAX_DEAD_SCROLL = 0.08
    MIN_TURN_INTENT = 0.55

    def applies_to(self, subject: CritiqueSubject) -> bool:
        m = subject.metrics
        return any(
            key in m
            for key in (
                "shot_positions",
                "camera_lag_vs_scroll",
                "dead_scroll_gaps",
                "turn_intent_score",
            )
        )

    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        refs = self._evidence_refs(evidence, subject)
        findings: list[CriticFinding] = []
        m = subject.metrics

        positions = m.get("shot_positions")
        if positions is not None:
            unique = {tuple(p) if not isinstance(p, tuple) else p for p in positions}
            variety = len(unique) / max(1, len(positions))
            if variety < self.MIN_SHOT_VARIETY:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:shot-variety",
                        role=self.role,
                        diagnosis="shot variety too low; path reads as a single locked framing",
                        evidence=refs,
                        measured={"shot_variety": float(variety), "unique_shots": len(unique)},
                        severity="major",
                        likely_cause="insufficient camera path control points",
                        bounded_repair={
                            "parameters": ["shot_positions"],
                            "action": "insert_orbit_beats",
                        },
                        blast_radius=["camera_path"],
                        acceptance_test="shot_variety >= 0.45",
                    )
                )

        lag = m.get("camera_lag_vs_scroll")
        if lag is not None and float(lag) > self.MAX_CAMERA_LAG:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:camera-lag",
                    role=self.role,
                    diagnosis="camera lag vs scroll exceeds cinematic damping budget",
                    evidence=refs,
                    measured={"camera_lag_vs_scroll": float(lag)},
                    severity="major",
                    likely_cause="damping too high or scroll-sample desync",
                    bounded_repair={
                        "parameters": ["damping", "camera_lag_vs_scroll"],
                        "delta": {"damping": -0.05},
                    },
                    blast_radius=["camera_path", "scroll_mapping"],
                    acceptance_test="camera_lag_vs_scroll <= 0.12",
                )
            )

        gaps = m.get("dead_scroll_gaps")
        if gaps is not None:
            dead = dead_scroll_fraction([(float(a), float(b)) for a, b in gaps])
            if dead > self.MAX_DEAD_SCROLL:
                findings.append(
                    make_finding(
                        finding_id=f"{subject.subject_id}:dead-scroll",
                        role=self.role,
                        diagnosis="dead scroll distance with no narrative beat",
                        evidence=refs,
                        measured={"dead_scroll_fraction": dead},
                        severity="major",
                        likely_cause="beat coverage gaps in camera path graph",
                        bounded_repair={
                            "parameters": ["beats"],
                            "action": "fill_dead_scroll_gaps",
                        },
                        blast_radius=["camera_path", "narrative"],
                        acceptance_test="dead_scroll_fraction <= 0.08",
                    )
                )

        turn = m.get("turn_intent_score")
        if turn is not None and float(turn) < self.MIN_TURN_INTENT:
            findings.append(
                make_finding(
                    finding_id=f"{subject.subject_id}:turn-intent",
                    role=self.role,
                    diagnosis="camera turn does not read as intentional",
                    evidence=refs,
                    measured={"turn_intent_score": float(turn)},
                    severity="minor",
                    likely_cause="angular velocity lacks clear accelerate/coast/decelerate profile",
                    bounded_repair={
                        "parameters": ["orientation_points"],
                        "action": "reshape_turn_easing",
                    },
                    blast_radius=["camera_path"],
                    acceptance_test="turn_intent_score >= 0.55",
                )
            )
        return findings
