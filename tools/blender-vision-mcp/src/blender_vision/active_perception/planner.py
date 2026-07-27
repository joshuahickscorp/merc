"""Next-best-view planner: ask only for non-redundant, high-gain evidence."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.active_perception.information_gain import (
    InformationGainEstimate,
    ProposedView,
    estimate_information_gain,
)
from blender_vision.active_perception.uncertainty import PerceptionTarget, quantify_uncertainty
from blender_vision.core.util import utc_now
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units, derive
from blender_vision.v2.records import Lineage, NextViewRequest


class StopReason(StrEnum):
    GATES_SATISFIED = "gates_satisfied"
    GAIN_TOO_LOW = "gain_too_low"
    USER_DECLINED = "user_declined"
    REQUESTS_EMITTED = "requests_emitted"


@dataclass(slots=True)
class PlannerConfig:
    gain_threshold: float = 0.02
    max_requests: int = 12
    min_priority: int = 1
    max_priority: int = 10


@dataclass(slots=True)
class PlannerResult:
    requests: list[NextViewRequest]
    stop_reason: StopReason
    uncertainty_before: float
    estimates: list[InformationGainEstimate] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "stop_reason": self.stop_reason.value,
            "uncertainty_before": self.uncertainty_before,
            "request_count": len(self.requests),
            "requests": [
                {
                    "id": r.id,
                    "missing_uncertainty": r.missing_uncertainty,
                    "expected_reduction": r.expected_reduction,
                    "priority": r.priority,
                    "reason": r.reason,
                    "human_instructions": r.human_instructions,
                    "capture_instructions": r.capture_instructions,
                }
                for r in self.requests
            ],
            "notes": list(self.notes),
        }


def consumer_object_candidates(target: PerceptionTarget) -> list[ProposedView]:
    """Canonical consumer-object view set required by the V2 doctrine."""
    uncovered = [c.region for c in target.cells if c.uncovered_area_m2 > 0]
    candidates: list[ProposedView] = []

    if "underside" in uncovered or any("under" in r for r in uncovered):
        regions = [r for r in uncovered if "under" in r or r == "underside"]
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:underside",
                kind="underside",
                regions=regions or ["underside"],
                capture_instructions={
                    "pose": "object inverted or camera under table glass",
                    "incidence_deg": 25,
                    "min_resolution_px": 1600,
                    "distance_m": 0.35,
                },
                human_instructions=(
                    "Photograph the underside: tip the object carefully or shoot through "
                    "a glass table. Fill the frame, sharp focus, 20–35° incidence."
                ),
                required_calibration=["color_checker_optional"],
                acceptable_alternatives=["mirror_underside_shot", "turntable_bottom_stop"],
                reason="underside surface is never observed",
            )
        )

    side_names = {"left", "right", "side", "rear"}
    side_regions = [r for r in uncovered if r in side_names]
    if side_regions:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:side",
                kind="side",
                regions=side_regions,
                capture_instructions={
                    "pose": "orbit 90° from primary hero",
                    "incidence_deg": 30,
                    "min_resolution_px": 1600,
                },
                human_instructions="Capture a clean side elevation at eye level.",
                required_calibration=[],
                acceptable_alternatives=["three_quarter_side"],
                reason="side surfaces lack coverage",
            )
        )

    if not target.has_scale_reference:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:scale-reference",
                kind="scale_reference",
                regions=[],
                covers_scale_reference=True,
                capture_instructions={
                    "include": "metric_ruler_or_known_credit_card",
                    "placement": "same plane as object base",
                    "min_resolution_px": 1200,
                },
                human_instructions=(
                    "Place a metric ruler or credit card in-frame on the same plane as "
                    "the object base and photograph."
                ),
                required_calibration=["scale_object_known_length_m"],
                acceptable_alternatives=["manufacturer_dimension_photo", "caliper_measurement"],
                reason="scale authority is unresolved without a metric reference",
            )
        )

    if not target.has_diffuse_light_view:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:diffuse-light",
                kind="diffuse_light",
                regions=[c.region for c in target.cells if c.covered][:1] or ["front"],
                covers_diffuse_light=True,
                capture_instructions={
                    "lighting": "overcast_or_softbox",
                    "avoid": "hard_specular_peaks",
                    "min_resolution_px": 1600,
                },
                human_instructions=(
                    "Reshoot the primary face under soft diffuse light (overcast sky or softbox) "
                    "to stabilize base colour and material read."
                ),
                required_calibration=["neutral_gray_card"],
                acceptable_alternatives=["light_tent"],
                reason="no diffuse-light observation for material base colour",
            )
        )

    if not target.has_grazing_light_view:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:grazing-light",
                kind="grazing_light",
                regions=[c.region for c in target.cells if c.covered][:1] or ["front"],
                covers_grazing_light=True,
                capture_instructions={
                    "lighting": "low_angle_grazing",
                    "incidence_deg": 75,
                    "min_resolution_px": 1600,
                },
                human_instructions=(
                    "Light from a low grazing angle to reveal surface microgeometry and texture."
                ),
                required_calibration=[],
                acceptable_alternatives=["raking_window_light"],
                reason="no grazing-light observation for microgeometry",
            )
        )

    if not target.has_lens_metadata:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:lens-metadata",
                kind="lens_metadata",
                regions=[],
                covers_lens_metadata=True,
                capture_instructions={
                    "required_exif": ["FocalLength", "FocalLengthIn35mmFilm", "BodySerialNumber"],
                    "or_manual": ["focal_length_mm", "sensor_width_mm"],
                },
                human_instructions=(
                    "Provide EXIF with focal length (and 35mm equivalent if available), "
                    "or manually record focal length and sensor width."
                ),
                required_calibration=["intrinsics_prior"],
                acceptable_alternatives=["manual_intrinsics_form"],
                reason="lens metadata missing; intrinsics underdetermined",
            )
        )

    if not target.has_calibration_target:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:calibration-target",
                kind="calibration_target",
                regions=[],
                covers_calibration_target=True,
                capture_instructions={
                    "target": "charuco_or_checkerboard",
                    "min_squares": 20,
                    "in_scene": True,
                },
                human_instructions=(
                    "Include a Charuco or checkerboard calibration target in at least one view."
                ),
                required_calibration=["charuco_board"],
                acceptable_alternatives=["aprilgrid", "known_metric_grid"],
                reason="no calibration target observed",
            )
        )

    # Any remaining uncovered regions not already proposed.
    covered_by_proposal = {r for view in candidates for r in view.regions}
    remaining = [r for r in uncovered if r not in covered_by_proposal]
    for region in remaining:
        candidates.append(
            ProposedView(
                view_id=f"{target.target_id}:region:{region}",
                kind="surface_region",
                regions=[region],
                capture_instructions={
                    "region": region,
                    "incidence_deg": 25,
                    "min_resolution_px": 1600,
                },
                human_instructions=f"Capture region '{region}' sharp and unobscured.",
                reason=f"region {region} is uncovered",
            )
        )
    return candidates


def _priority_from_gain(gain: float, *, cfg: PlannerConfig) -> int:
    # Map expected reduction [0,1] to priority 0..10 (higher = more urgent).
    raw = int(round(gain * 10))
    return int(max(cfg.min_priority, min(cfg.max_priority, raw)))


class NextBestViewPlanner:
    """Emit ordered NextViewRequest records or a stop reason."""

    def __init__(self, config: PlannerConfig | None = None):
        self.config = config or PlannerConfig()

    def ask_next_view(self, target: PerceptionTarget) -> list[NextViewRequest]:
        return self.plan(target).requests

    def plan(self, target: PerceptionTarget) -> PlannerResult:
        report = quantify_uncertainty(target)
        if target.user_declined:
            return PlannerResult(
                requests=[],
                stop_reason=StopReason.USER_DECLINED,
                uncertainty_before=report.total,
                notes=["user declined further capture"],
            )
        if target.gates_satisfied:
            return PlannerResult(
                requests=[],
                stop_reason=StopReason.GATES_SATISFIED,
                uncertainty_before=report.total,
                notes=["acceptance gates already satisfied"],
            )

        candidates = consumer_object_candidates(target)
        estimates = [
            estimate_information_gain(target, view, gain_threshold=self.config.gain_threshold)
            for view in candidates
        ]
        viable = [item for item in estimates if not item.redundant and item.expected_reduction > 0]
        viable.sort(key=lambda item: (-item.expected_reduction, item.view.view_id))
        viable = viable[: self.config.max_requests]

        if not viable:
            return PlannerResult(
                requests=[],
                stop_reason=StopReason.GAIN_TOO_LOW,
                uncertainty_before=report.total,
                estimates=estimates,
                notes=["all candidates redundant or below gain threshold"],
            )

        requests: list[NextViewRequest] = []
        for index, estimate in enumerate(viable):
            view = estimate.view
            missing = self._missing_label(target, view, estimate)
            priority = _priority_from_gain(estimate.expected_reduction, cfg=self.config)
            # Stable descending priority by rank when gains tie.
            priority = max(self.config.min_priority, min(self.config.max_priority, priority - 0))
            record = NextViewRequest(
                id=f"nvr-{view.view_id}",
                created_at=utc_now(),
                authority=derive(
                    [AuthorityClass.INFERRED.value],
                    proposed=AuthorityClass.INFERRED,
                ),
                lineage=Lineage(
                    operation="active_perception.ask_next_view",
                    inputs=[f"target:{target.target_id}"],
                    input_authorities=[AuthorityClass.INFERRED.value],
                    parameters={
                        "view_kind": view.kind,
                        "gain_threshold": self.config.gain_threshold,
                        "rank": index,
                    },
                ),
                uncertainty=Uncertainty(
                    kind="expected-reduction",
                    sigma=None,
                    interval=[0.0, estimate.expected_reduction],
                    units=Units.UNITLESS,
                    basis="coverage-atlas + hypothesis-disagreement estimator",
                    samples=1,
                ),
                target_id=target.target_id,
                missing_uncertainty=missing,
                expected_reduction=float(estimate.expected_reduction),
                capture_instructions=dict(view.capture_instructions),
                human_instructions=view.human_instructions,
                required_calibration=list(view.required_calibration),
                acceptable_alternatives=list(view.acceptable_alternatives),
                reason=view.reason or estimate.reason,
                priority=priority,
            ).seal()
            requests.append(record)

        # Ensure strict priority ordering by expected gain (desc).
        requests.sort(key=lambda r: (-r.expected_reduction, r.id))
        for index, request in enumerate(requests):
            # Re-seal if we adjust priority for rank clarity.
            new_priority = max(1, min(10, 10 - index))
            if request.priority != new_priority:
                request.priority = new_priority
                request.digest = ""
                request.seal()

        return PlannerResult(
            requests=requests,
            stop_reason=StopReason.REQUESTS_EMITTED,
            uncertainty_before=report.total,
            estimates=estimates,
        )

    @staticmethod
    def _missing_label(
        target: PerceptionTarget,
        view: ProposedView,
        estimate: InformationGainEstimate,
    ) -> str:
        if view.covers_scale_reference:
            return "scale_authority"
        if view.covers_diffuse_light or view.covers_grazing_light:
            return "lighting"
        if view.covers_lens_metadata:
            return "lens_metadata"
        if view.covers_calibration_target:
            return "calibration"
        if view.regions:
            return "surface_coverage"
        if estimate.disagreement_reduction > 0:
            return "hypothesis_disagreement"
        return "unspecified"

    def satisfy(
        self,
        target: PerceptionTarget,
        request: NextViewRequest,
        *,
        observation_id: str | None = None,
    ) -> PerceptionTarget:
        """Apply a satisfied request to the target (mutate coverage flags)."""
        obs = observation_id or f"satisfied:{request.id}"
        instructions = dict(request.capture_instructions or {})
        req_id = request.id
        # Infer kind from request id when not embedded.
        if "scale-reference" in req_id or request.missing_uncertainty == "scale_authority":
            target.has_scale_reference = True
            target.scale_authority = AuthorityClass.MEASURED
        if "diffuse-light" in req_id or instructions.get("lighting") == "overcast_or_softbox":
            target.has_diffuse_light_view = True
        if "grazing-light" in req_id or instructions.get("lighting") == "low_angle_grazing":
            target.has_grazing_light_view = True
        if "lens-metadata" in req_id or request.missing_uncertainty == "lens_metadata":
            target.has_lens_metadata = True
        if "calibration-target" in req_id or request.missing_uncertainty == "calibration":
            target.has_calibration_target = True

        regions = list(instructions.get("regions") or [])
        if ":underside" in req_id or req_id.endswith("underside"):
            regions = list(dict.fromkeys([*regions, "underside"]))
        elif req_id.endswith(":side") or ":side" in req_id and "underside" not in req_id:
            for name in ("left", "right", "side", "rear"):
                if any(c.region == name for c in target.cells):
                    regions.append(name)
        elif request.missing_uncertainty == "surface_coverage":
            for cell in target.cells:
                token = f":{cell.region}"
                if token in req_id or cell.region in request.reason:
                    regions.append(cell.region)
        regions = list(dict.fromkeys(regions))
        if regions:
            target.mark_covered(regions, view_id=obs)

        # Suppress exact request and canonical view signatures on the next plan.
        target.existing_view_signatures.add(obs)
        target.existing_view_signatures.add(req_id)
        # Strip nvr- prefix so candidate view_ids also match.
        if req_id.startswith("nvr-"):
            target.existing_view_signatures.add(req_id.removeprefix("nvr-"))
        request.satisfied_by = obs
        return target
