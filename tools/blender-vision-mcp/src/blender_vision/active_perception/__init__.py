"""Active perception: uncertainty, information gain, next-best-view planner."""

from __future__ import annotations

from blender_vision.active_perception.information_gain import (
    InformationGainEstimate,
    ProposedView,
    estimate_information_gain,
    expected_disagreement_reduction,
    expected_newly_covered_area,
)
from blender_vision.active_perception.planner import (
    NextBestViewPlanner,
    PlannerConfig,
    PlannerResult,
    StopReason,
    consumer_object_candidates,
)
from blender_vision.active_perception.uncertainty import (
    PerceptionTarget,
    SurfaceCell,
    UncertaintyKind,
    UncertaintyReport,
    hypothesis_disagreement,
    material_confidence_spread,
    quantify_uncertainty,
    scale_authority_uncertainty,
)

__all__ = [
    "InformationGainEstimate",
    "NextBestViewPlanner",
    "PerceptionTarget",
    "PlannerConfig",
    "PlannerResult",
    "ProposedView",
    "StopReason",
    "SurfaceCell",
    "UncertaintyKind",
    "UncertaintyReport",
    "consumer_object_candidates",
    "estimate_information_gain",
    "expected_disagreement_reduction",
    "expected_newly_covered_area",
    "hypothesis_disagreement",
    "material_confidence_spread",
    "quantify_uncertainty",
    "scale_authority_uncertainty",
]
