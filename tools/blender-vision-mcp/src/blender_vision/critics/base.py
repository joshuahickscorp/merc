"""Shared critic contract: role, applicability, and evidence-bound findings."""

from __future__ import annotations

from abc import ABC, abstractmethod
from collections.abc import Mapping
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.v2.records import CriticFinding


class CriticRole(StrEnum):
    PRODUCT_PHOTOGRAPHER = "product_photographer"
    CINEMATOGRAPHER = "cinematographer"
    INDUSTRIAL_DESIGNER = "industrial_designer"
    ENVIRONMENT_ARTIST = "environment_artist"
    MATERIAL_ARTIST = "material_artist"
    LIGHTING_ARTIST = "lighting_artist"
    ORGANIC_ARTIST = "organic_artist"
    GROOM_ARTIST = "groom_artist"
    EDITORIAL_ART_DIRECTOR = "editorial_art_director"
    INTERACTION_DESIGNER = "interaction_designer"
    ACCESSIBILITY_REVIEWER = "accessibility_reviewer"
    PERFORMANCE_ENGINEER = "performance_engineer"
    ADVERSARIAL_ACCEPTANCE_REVIEWER = "adversarial_acceptance_reviewer"


ALL_ROLES: tuple[CriticRole, ...] = tuple(CriticRole)


@dataclass(slots=True)
class CritiqueSubject:
    """What a critic inspects. Measurements live in ``metrics``; optional media in ``media``."""

    subject_id: str
    kind: str
    metrics: dict[str, Any] = field(default_factory=dict)
    media: dict[str, Any] = field(default_factory=dict)
    tags: frozenset[str] = field(default_factory=frozenset)

    def get(self, key: str, default: Any = None) -> Any:
        if key in self.metrics:
            return self.metrics[key]
        return self.media.get(key, default)


@dataclass(slots=True)
class CritiqueEvidence:
    """Bound evidence references and optional raw payloads used for measurement."""

    references: list[str] = field(default_factory=list)
    payloads: dict[str, Any] = field(default_factory=dict)

    def require_references(self) -> list[str]:
        if not self.references:
            raise ValidationError("critique evidence must include at least one reference")
        return list(self.references)


def require_measured(measured: Mapping[str, Any], *, finding_id: str) -> dict[str, Any]:
    """Every finding must attach at least one numeric measured quantity."""
    if not measured:
        raise ValidationError(f"finding {finding_id} has no measured quantity")
    numeric = {
        key: value
        for key, value in measured.items()
        if isinstance(value, (int, float)) and not isinstance(value, bool)
    }
    if not numeric:
        raise ValidationError(
            f"finding {finding_id} measured payload has no numeric quantity: {dict(measured)}"
        )
    return dict(measured)


def make_finding(
    *,
    finding_id: str,
    role: CriticRole | str,
    diagnosis: str,
    evidence: list[str],
    measured: Mapping[str, Any],
    severity: str = "major",
    confidence: float = 0.85,
    likely_cause: str = "",
    bounded_repair: Mapping[str, Any] | None = None,
    blast_radius: list[str] | None = None,
    acceptance_test: str = "",
) -> CriticFinding:
    measured_payload = require_measured(measured, finding_id=finding_id)
    if not evidence:
        raise ValidationError(f"finding {finding_id} must bind evidence")
    return CriticFinding(
        finding_id=finding_id,
        critic_role=str(role),
        diagnosis=diagnosis,
        evidence=list(evidence),
        severity=severity,
        confidence=float(confidence),
        likely_cause=likely_cause,
        bounded_repair=dict(bounded_repair or {}),
        blast_radius=list(blast_radius or []),
        acceptance_test=acceptance_test,
        measured=measured_payload,
    )


class Critic(ABC):
    """Specialist perceptual critic with a real measurable detector."""

    role: CriticRole

    @abstractmethod
    def applies_to(self, subject: CritiqueSubject) -> bool:
        """Return True when this critic has enough subject data to run."""

    @abstractmethod
    def critique(
        self, subject: CritiqueSubject, evidence: CritiqueEvidence
    ) -> list[CriticFinding]:
        """Return findings. Empty list means no fault detected."""

    def _evidence_refs(self, evidence: CritiqueEvidence, subject: CritiqueSubject) -> list[str]:
        refs = list(evidence.references)
        if not refs:
            refs = [f"subject:{subject.subject_id}"]
        return refs
