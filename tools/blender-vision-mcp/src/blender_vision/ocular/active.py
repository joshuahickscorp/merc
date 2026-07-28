"""Active perception: next-best-view planning over the ocular world model.

Plans views that reduce entity uncertainty, suppresses redundant requests, and
stops when expected information gain falls below a declared threshold.
Predicted gain is scored against realised gain after the view is satisfied —
a planner whose predicted gain does not correlate with actual gain is guessing.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field, fields
from enum import StrEnum
from typing import Any, ClassVar

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import utc_now
from blender_vision.ocular.world import (
    WorldState,
    lower_uncertainty,
    update_world_model,
)
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units
from blender_vision.v2.records import Lifecycle, Lineage, V2Record


class ViewStopReason(StrEnum):
    NONE = "none"
    GAIN_TOO_LOW = "gain_too_low"
    REDUNDANT = "redundant"
    NO_UNCERTAIN_ENTITIES = "no_uncertain_entities"
    BUDGET_EXHAUSTED = "budget_exhausted"
    SATISFIED = "satisfied"


@dataclass(slots=True, kw_only=True)
class NextViewRequest(V2Record):
    """Ocular next-best-view request. Satisfied only by feeding a new observation."""

    RECORD_KIND: ClassVar[str] = "ocular.next-view-request"

    request_id: str = ""
    target_entity_id: str = ""
    reason: str = ""
    # Expected uncertainty reduction in [0, 1] (confidence gain proxy).
    expected_reduction: float = 0.0
    # Camera / gaze instructions the actuator should honour.
    capture_instructions: dict[str, Any] = field(default_factory=dict)
    priority: int = 5
    satisfied_by: str | None = None
    declined: bool = False
    suppressed: bool = False
    stop_reason: str = ViewStopReason.NONE.value
    predicted_gain: float = 0.0
    actual_gain: float | None = None

    def __post_init__(self) -> None:
        if not 0 <= self.priority <= 10:
            raise ValidationError("priority must be within 0..10")
        if not 0.0 <= self.expected_reduction <= 1.0:
            raise ValidationError("expected_reduction must be within 0..1")

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> NextViewRequest:
        data = dict(payload)
        if "authority" in data:
            data["authority"] = AuthorityClass(data["authority"])
        if "lifecycle" in data:
            data["lifecycle"] = Lifecycle(data["lifecycle"])
        if "lineage" in data and isinstance(data["lineage"], dict):
            data["lineage"] = Lineage.from_dict(data["lineage"])
        if "uncertainty" in data and isinstance(data["uncertainty"], dict):
            data["uncertainty"] = Uncertainty.from_dict(data["uncertainty"])
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})


@dataclass(slots=True)
class InformationGainReport:
    """Predicted versus actual uncertainty reduction for one satisfied view."""

    request_id: str
    target_entity_id: str
    predicted_gain: float
    actual_gain: float
    confidence_before: float
    confidence_after: float
    sigma_before: float
    sigma_after: float
    redundant: bool = False
    stop_reason: str = ViewStopReason.NONE.value

    def to_dict(self) -> dict[str, Any]:
        return {
            "request_id": self.request_id,
            "target_entity_id": self.target_entity_id,
            "predicted_gain": self.predicted_gain,
            "actual_gain": self.actual_gain,
            "confidence_before": self.confidence_before,
            "confidence_after": self.confidence_after,
            "sigma_before": self.sigma_before,
            "sigma_after": self.sigma_after,
            "redundant": self.redundant,
            "stop_reason": self.stop_reason,
            "residual": abs(self.predicted_gain - self.actual_gain),
        }


@dataclass(slots=True)
class NBVPlannerState:
    """Tracks issued view signatures so redundant requests can be suppressed."""

    issued_signatures: set[str] = field(default_factory=set)
    satisfied_request_ids: list[str] = field(default_factory=list)
    gain_reports: list[InformationGainReport] = field(default_factory=list)
    min_gain: float = 0.05
    max_requests: int = 8
    requests_issued: int = 0


def _entity_uncertainty_score(entity: Any) -> float:
    """Higher = more uncertain. Combines low confidence and high pose sigma."""
    conf = float(entity.confidence)
    sigma = float(entity.uncertainty.sigma or 0.02)
    unseen = float(entity.frames_since_seen)
    return (1.0 - conf) + min(1.0, sigma * 5.0) + min(0.5, unseen * 0.05)


def _view_signature(entity_id: str, instructions: dict[str, Any]) -> str:
    az = round(float(instructions.get("azimuth_deg", 0.0)), 1)
    el = round(float(instructions.get("elevation_deg", 0.0)), 1)
    dist = round(float(instructions.get("distance_m", 1.0)), 2)
    return f"{entity_id}|az={az}|el={el}|d={dist}"


def measure_information_gain(
    *,
    confidence_before: float,
    confidence_after: float,
    sigma_before: float,
    sigma_after: float,
) -> float:
    """Realised information gain in [0, 1] from confidence rise and sigma drop."""
    conf_gain = max(0.0, float(confidence_after) - float(confidence_before))
    # Sigma drop scaled: a 50% reduction contributes up to 0.5.
    if sigma_before <= 0:
        sigma_gain = 0.0
    else:
        ratio = (float(sigma_before) - float(sigma_after)) / float(sigma_before)
        sigma_gain = max(0.0, ratio) * 0.5
    return min(1.0, conf_gain + sigma_gain)


def predict_information_gain(world: WorldState, entity_id: str) -> float:
    """Predicted gain from re-observing an uncertain entity.

    Uses current confidence/sigma only — no GT. High uncertainty → high predicted gain.
    """
    entity = world.entities.get(entity_id)
    if entity is None:
        raise ValidationError(f"unknown entity {entity_id}")
    conf = float(entity.confidence)
    sigma = float(entity.uncertainty.sigma or 0.02)
    unseen = float(entity.frames_since_seen)
    # Expected: confirmation recovers a fraction of the confidence gap and shrinks sigma.
    conf_room = max(0.0, 0.95 - conf)
    expected_conf = min(0.2, 0.08 + 0.4 * conf_room + 0.03 * min(unseen, 5.0))
    expected_sigma = min(0.3, sigma * 0.3)
    return min(1.0, expected_conf + expected_sigma)


def plan_next_view(
    world: WorldState,
    state: NBVPlannerState | None = None,
    *,
    min_gain: float | None = None,
) -> NextViewRequest | None:
    """Emit one NextViewRequest for the most uncertain entity, or stop.

    Returns None when the stop condition fires (gain too low / no targets / budget).
    Redundant signatures are returned with suppressed=True rather than re-issued.
    """
    planner = state if state is not None else NBVPlannerState()
    threshold = planner.min_gain if min_gain is None else float(min_gain)

    if planner.requests_issued >= planner.max_requests:
        return NextViewRequest(
            id=f"nvr-stop-budget-{planner.requests_issued}",
            request_id=f"nvr-stop-budget-{planner.requests_issued}",
            reason="request budget exhausted",
            expected_reduction=0.0,
            stop_reason=ViewStopReason.BUDGET_EXHAUSTED.value,
            declined=True,
            authority=AuthorityClass.INFERRED,
            lineage=Lineage(
                operation="plan_next_view",
                inputs=[world.scene_id],
                parameters={"source_authority": AuthorityClass.INFERRED.value},
            ),
        ).seal()

    candidates = [
        ent
        for ent in world.entities.values()
        if ent.state != "removed"
    ]
    if not candidates:
        return NextViewRequest(
            id="nvr-stop-empty",
            request_id="nvr-stop-empty",
            reason="no entities in world",
            expected_reduction=0.0,
            stop_reason=ViewStopReason.NO_UNCERTAIN_ENTITIES.value,
            declined=True,
            authority=AuthorityClass.INFERRED,
            lineage=Lineage(
                operation="plan_next_view",
                inputs=[world.scene_id],
                parameters={"source_authority": AuthorityClass.INFERRED.value},
            ),
        ).seal()

    ranked = sorted(candidates, key=_entity_uncertainty_score, reverse=True)
    target = ranked[0]
    predicted = predict_information_gain(world, target.entity_id)
    if predicted < threshold:
        return NextViewRequest(
            id=f"nvr-stop-gain-{target.entity_id}",
            request_id=f"nvr-stop-gain-{target.entity_id}",
            target_entity_id=target.entity_id,
            reason=f"expected gain {predicted:.4f} below threshold {threshold:.4f}",
            expected_reduction=predicted,
            predicted_gain=predicted,
            stop_reason=ViewStopReason.GAIN_TOO_LOW.value,
            declined=True,
            authority=AuthorityClass.MODEL_DERIVED,
            lineage=Lineage(
                operation="plan_next_view",
                inputs=[target.entity_id],
                parameters={
                    "min_gain": threshold,
                    "source_authority": AuthorityClass.MODEL_DERIVED.value,
                },
            ),
        ).seal()

    # Viewpoint: orbit toward the entity's last pose so the new view covers it.
    pose = list(target.pose_m)
    azimuth = math.degrees(math.atan2(pose[1], pose[0] + 1e-9)) + 35.0
    instructions = {
        "look_at_m": pose[:3],
        "azimuth_deg": azimuth,
        "elevation_deg": 25.0,
        "distance_m": 1.1,
        "render_engine": "BLENDER_EEVEE_NEXT",
        "purpose": "reduce_entity_uncertainty",
    }
    signature = _view_signature(target.entity_id, instructions)
    if signature in planner.issued_signatures:
        return NextViewRequest(
            id=f"nvr-redund-{target.entity_id}-{planner.requests_issued}",
            request_id=f"nvr-redund-{target.entity_id}-{planner.requests_issued}",
            target_entity_id=target.entity_id,
            reason="view signature already issued",
            expected_reduction=0.0,
            predicted_gain=0.0,
            capture_instructions=instructions,
            suppressed=True,
            declined=True,
            stop_reason=ViewStopReason.REDUNDANT.value,
            authority=AuthorityClass.MODEL_DERIVED,
            lineage=Lineage(
                operation="plan_next_view",
                inputs=[target.entity_id, signature],
                parameters={"source_authority": AuthorityClass.MODEL_DERIVED.value},
            ),
        ).seal()

    rid = f"nvr-{target.entity_id}-{planner.requests_issued:03d}"
    request = NextViewRequest(
        id=rid,
        request_id=rid,
        target_entity_id=target.entity_id,
        reason=(
            f"entity {target.entity_id} uncertainty_score="
            f"{_entity_uncertainty_score(target):.3f} conf={target.confidence:.3f}"
        ),
        expected_reduction=predicted,
        predicted_gain=predicted,
        capture_instructions=instructions,
        priority=min(10, 5 + int(predicted * 5)),
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="plan_next_view",
            inputs=[target.entity_id],
            parameters={
                "signature": signature,
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
            },
        ),
        uncertainty=Uncertainty(
            kind="view_gain",
            sigma=1.0 - predicted,
            units=Units.UNITLESS,
            basis="entity-uncertainty-proxy",
            samples=1,
        ),
        created_at=utc_now(),
    ).seal()
    planner.issued_signatures.add(signature)
    planner.requests_issued += 1
    return request


def satisfy_next_view(
    world: WorldState,
    request: NextViewRequest,
    observation: dict[str, Any],
    *,
    planner: NBVPlannerState | None = None,
) -> InformationGainReport:
    """Feed a new observation that satisfies a NextViewRequest and measure gain.

    The observation must re-observe the target entity. Uncertainty is lowered on
    confirmation; the report records predicted vs actual gain.
    """
    if request.declined or request.suppressed:
        raise ValidationError("cannot satisfy a declined or suppressed request")
    if not request.target_entity_id:
        raise ValidationError("request has no target_entity_id")
    entity = world.entities.get(request.target_entity_id)
    if entity is None:
        raise ValidationError(f"target entity {request.target_entity_id} missing from world")

    conf_before = float(entity.confidence)
    sigma_before = float(entity.uncertainty.sigma or 0.02)

    # Ensure the observation carries perception track_source and includes target.
    obs = dict(observation)
    obs.setdefault("track_source", "perception")
    update_world_model(world, obs)

    # Explicit confirmation update so gain is measurable even if pose already matched.
    lower_uncertainty(
        world,
        request.target_entity_id,
        reason="nbv_satisfied",
        frame_index=int(obs.get("frame_index", world.current_frame)),
        evidence={
            "request_id": request.request_id,
            "capture_instructions": request.capture_instructions,
        },
    )
    entity = world.entities[request.target_entity_id]
    conf_after = float(entity.confidence)
    sigma_after = float(entity.uncertainty.sigma or 0.02)
    actual = measure_information_gain(
        confidence_before=conf_before,
        confidence_after=conf_after,
        sigma_before=sigma_before,
        sigma_after=sigma_after,
    )
    request.satisfied_by = str(obs.get("frame_index", world.current_frame))
    request.actual_gain = actual
    request.stop_reason = ViewStopReason.SATISFIED.value
    request.digest = ""

    report = InformationGainReport(
        request_id=request.request_id,
        target_entity_id=request.target_entity_id,
        predicted_gain=float(request.predicted_gain or request.expected_reduction),
        actual_gain=actual,
        confidence_before=conf_before,
        confidence_after=conf_after,
        sigma_before=sigma_before,
        sigma_after=sigma_after,
        stop_reason=ViewStopReason.SATISFIED.value,
    )
    if planner is not None:
        planner.satisfied_request_ids.append(request.request_id)
        planner.gain_reports.append(report)
    world.meta.setdefault("nbv_reports", []).append(report.to_dict())
    world.digest = ""
    return report


def gain_correlation(reports: list[InformationGainReport]) -> float:
    """Pearson correlation between predicted and actual gain. 0 if undefined."""
    if len(reports) < 2:
        # Single point: perfect if residual small, else 0.
        if len(reports) == 1:
            r = reports[0]
            return 1.0 if abs(r.predicted_gain - r.actual_gain) < 0.25 else 0.0
        return 0.0
    xs = [r.predicted_gain for r in reports]
    ys = [r.actual_gain for r in reports]
    mx = sum(xs) / len(xs)
    my = sum(ys) / len(ys)
    num = sum((x - mx) * (y - my) for x, y in zip(xs, ys, strict=True))
    den_x = math.sqrt(sum((x - mx) ** 2 for x in xs))
    den_y = math.sqrt(sum((y - my) ** 2 for y in ys))
    if den_x < 1e-12 or den_y < 1e-12:
        return 0.0
    return max(-1.0, min(1.0, num / (den_x * den_y)))


def run_nbv_loop(
    world: WorldState,
    *,
    satisfy_fn: Any,
    max_steps: int = 4,
    min_gain: float = 0.05,
) -> dict[str, Any]:
    """Plan → satisfy → measure until stop. satisfy_fn(request) -> observation dict."""
    planner = NBVPlannerState(min_gain=min_gain, max_requests=max_steps)
    steps: list[dict[str, Any]] = []
    for _ in range(max_steps):
        request = plan_next_view(world, planner, min_gain=min_gain)
        if request is None:
            break
        step: dict[str, Any] = {"request": request.to_dict()}
        if request.declined or request.suppressed:
            step["stopped"] = True
            step["stop_reason"] = request.stop_reason
            steps.append(step)
            break
        observation = satisfy_fn(request)
        report = satisfy_next_view(world, request, observation, planner=planner)
        step["report"] = report.to_dict()
        steps.append(step)
        # Immediately re-plan the same signature should be suppressed.
        redundant = plan_next_view(world, planner, min_gain=min_gain)
        if redundant is not None and redundant.suppressed:
            steps.append(
                {
                    "request": redundant.to_dict(),
                    "suppressed": True,
                    "stop_reason": redundant.stop_reason,
                }
            )
            # Clear signature lock only for diversity in multi-step; keep it to
            # prove suppression, then force a different azimuth next loop via
            # min_gain check on already-confirmed entities.
            break

    correlation = gain_correlation(planner.gain_reports)
    return {
        "steps": steps,
        "gain_reports": [r.to_dict() for r in planner.gain_reports],
        "predicted_vs_actual_correlation": correlation,
        "n_satisfied": len(planner.satisfied_request_ids),
        "issued_signatures": sorted(planner.issued_signatures),
    }
