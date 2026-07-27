"""Expected information gain for proposed views and measurements."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.active_perception.uncertainty import (
    PerceptionTarget,
    SurfaceCell,
    quantify_uncertainty,
)


@dataclass(slots=True)
class ProposedView:
    """A candidate capture or measurement the planner may request."""

    view_id: str
    kind: str
    regions: list[str] = field(default_factory=list)
    signature: str = ""
    capture_instructions: dict[str, Any] = field(default_factory=dict)
    human_instructions: str = ""
    required_calibration: list[str] = field(default_factory=list)
    acceptable_alternatives: list[str] = field(default_factory=list)
    reason: str = ""
    covers_scale_reference: bool = False
    covers_diffuse_light: bool = False
    covers_grazing_light: bool = False
    covers_lens_metadata: bool = False
    covers_calibration_target: bool = False

    def __post_init__(self) -> None:
        if not self.signature:
            self.signature = f"{self.kind}:{','.join(sorted(self.regions))}"


@dataclass(slots=True)
class InformationGainEstimate:
    view: ProposedView
    expected_reduction: float
    newly_covered_area_m2: float
    disagreement_reduction: float
    component_reductions: dict[str, float] = field(default_factory=dict)
    redundant: bool = False
    reason: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "view_id": self.view.view_id,
            "kind": self.view.kind,
            "expected_reduction": self.expected_reduction,
            "newly_covered_area_m2": self.newly_covered_area_m2,
            "disagreement_reduction": self.disagreement_reduction,
            "component_reductions": dict(self.component_reductions),
            "redundant": self.redundant,
            "reason": self.reason,
        }


def _cells_by_region(target: PerceptionTarget) -> dict[str, SurfaceCell]:
    return {cell.region: cell for cell in target.cells}


def expected_newly_covered_area(target: PerceptionTarget, view: ProposedView) -> float:
    by_region = _cells_by_region(target)
    area = 0.0
    for region in view.regions:
        cell = by_region.get(region)
        if cell is None:
            continue
        area += cell.uncovered_area_m2
    return float(area)


def expected_disagreement_reduction(target: PerceptionTarget, view: ProposedView) -> float:
    """Variance of candidate predictions on newly visible surfaces (proxy for resolution)."""
    by_region = _cells_by_region(target)
    variances: list[float] = []
    for region in view.regions:
        cell = by_region.get(region)
        if cell is None or cell.uncovered_area_m2 <= 0:
            continue
        preds = cell.candidate_predictions
        if len(preds) >= 2:
            variances.append(float(np.var(np.asarray(preds, dtype=np.float64))))
        else:
            variances.append(0.0)
    if not variances:
        return 0.0
    # Expect full resolution of observed variance on newly covered surface.
    return float(np.mean(variances))


def estimate_information_gain(
    target: PerceptionTarget,
    view: ProposedView,
    *,
    gain_threshold: float = 0.02,
) -> InformationGainEstimate:
    known = target.existing_view_signatures
    if view.signature in known or view.view_id in known:
        return InformationGainEstimate(
            view=view,
            expected_reduction=0.0,
            newly_covered_area_m2=0.0,
            disagreement_reduction=0.0,
            redundant=True,
            reason="view signature already present in coverage",
        )

    before = quantify_uncertainty(target)
    new_area = expected_newly_covered_area(target, view)
    disagree = expected_disagreement_reduction(target, view)

    # Simulate applying the view to a shallow copy of coverage flags.
    simulated = PerceptionTarget(
        target_id=target.target_id,
        cells=[
            SurfaceCell(
                region=c.region,
                area_m2=c.area_m2,
                covered=c.covered,
                incidence_angle_deg=c.incidence_angle_deg,
                resolution_px=c.resolution_px,
                occlusion_fraction=c.occlusion_fraction,
                view_ids=list(c.view_ids),
                candidate_predictions=list(c.candidate_predictions),
            )
            for c in target.cells
        ],
        scale_authority=target.scale_authority,
        material_confidences=list(target.material_confidences),
        portfolio_predictions={k: list(v) for k, v in target.portfolio_predictions.items()},
        has_scale_reference=target.has_scale_reference or view.covers_scale_reference,
        has_diffuse_light_view=target.has_diffuse_light_view or view.covers_diffuse_light,
        has_grazing_light_view=target.has_grazing_light_view or view.covers_grazing_light,
        has_lens_metadata=target.has_lens_metadata or view.covers_lens_metadata,
        has_calibration_target=target.has_calibration_target or view.covers_calibration_target,
        gates_satisfied=target.gates_satisfied,
        user_declined=target.user_declined,
        existing_view_signatures=set(target.existing_view_signatures),
    )
    if view.regions:
        simulated.mark_covered(view.regions, view_id=view.view_id)
    if view.covers_scale_reference:
        from blender_vision.v2.authority import AuthorityClass

        simulated.has_scale_reference = True
        if strength_at_most_unresolved(simulated.scale_authority):
            simulated.scale_authority = AuthorityClass.MEASURED
    after = quantify_uncertainty(simulated)
    reduction = max(0.0, before.total - after.total)
    component_reductions = {
        key: max(0.0, before.components.get(key, 0.0) - after.components.get(key, 0.0))
        for key in before.components
    }

    # Area-based term reinforces the atlas estimator (normalized by total area).
    area_term = new_area / target.total_area()
    disagree_term = min(1.0, disagree)
    # Blend simulated total reduction with explicit atlas terms for defensibility.
    expected = float(min(1.0, 0.6 * reduction + 0.25 * area_term + 0.15 * disagree_term))

    redundant = expected < gain_threshold and new_area <= 0.0 and not any(
        [
            view.covers_scale_reference and not target.has_scale_reference,
            view.covers_diffuse_light and not target.has_diffuse_light_view,
            view.covers_grazing_light and not target.has_grazing_light_view,
            view.covers_lens_metadata and not target.has_lens_metadata,
            view.covers_calibration_target and not target.has_calibration_target,
        ]
    )
    reason = "expected uncertainty reduction"
    if redundant:
        reason = "expected gain below threshold or duplicate coverage"
    return InformationGainEstimate(
        view=view,
        expected_reduction=expected,
        newly_covered_area_m2=new_area,
        disagreement_reduction=disagree_term,
        component_reductions=component_reductions,
        redundant=redundant,
        reason=reason,
    )


def strength_at_most_unresolved(authority: Any) -> bool:
    from blender_vision.v2.authority import AuthorityClass, strength

    return strength(AuthorityClass(authority)) <= strength(AuthorityClass.UNRESOLVED)
