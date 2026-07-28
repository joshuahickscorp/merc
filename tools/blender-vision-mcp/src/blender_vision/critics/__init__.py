"""Perceptual critic workspace — thirteen specialist roles with measured detectors."""

from __future__ import annotations

from blender_vision.critics.base import (
    ALL_ROLES,
    Critic,
    CriticRole,
    CritiqueEvidence,
    CritiqueSubject,
    make_finding,
    require_measured,
)
from blender_vision.critics.registry import critic_by_role, default_critics
from blender_vision.critics.repair import BoundedRepairPlan, BoundedRepairRunner, plan_from_finding
from blender_vision.critics.workspace import CriticWorkspace, critique_to_summary

__all__ = [
    "ALL_ROLES",
    "BoundedRepairPlan",
    "BoundedRepairRunner",
    "Critic",
    "CriticRole",
    "CriticWorkspace",
    "CritiqueEvidence",
    "CritiqueSubject",
    "critic_by_role",
    "critique_to_summary",
    "default_critics",
    "make_finding",
    "plan_from_finding",
    "require_measured",
]
