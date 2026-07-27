"""Predictive loop and surprise detection for the Ocular world model.

A prediction carries an expected value, a declared tolerance, and a horizon.
When observation falls outside that tolerance a SurpriseEvent fires, the
contradicted belief is recorded, and entity uncertainty is raised. Confirming
evidence lowers uncertainty again.
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
    raise_uncertainty,
)
from blender_vision.v2.authority import AuthorityClass, Uncertainty, Units
from blender_vision.v2.records import Lifecycle, Lineage, V2Record


class PredictionKind(StrEnum):
    POSE = "pose"
    VISIBILITY = "visibility"
    FRAME_FEATURES = "frame_features"
    CAMERA_PATH = "camera_path"
    MATERIAL_RESPONSE = "material_response"
    BROWSER_ANIMATION = "browser_animation"
    EXISTENCE = "existence"


@dataclass(slots=True, kw_only=True)
class Prediction(V2Record):
    """What the world expects to observe next, with a hard tolerance band."""

    RECORD_KIND: ClassVar[str] = "ocular.prediction"

    prediction_id: str = ""
    entity_id: str = ""
    kind: str = PredictionKind.POSE.value
    expected: dict[str, Any] = field(default_factory=dict)
    tolerance: float = 0.05
    tolerance_units: str = Units.METRE.value
    horizon_frames: int = 1
    valid_from_frame: int = 0
    valid_until_frame: int = 0
    model: str = "constant_velocity"
    # Belief this prediction was derived from (for contradiction linkage).
    source_belief_id: str = ""

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Prediction:
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


@dataclass(slots=True, kw_only=True)
class SurpriseEvent(V2Record):
    """Fired when observation leaves a prediction's declared tolerance."""

    RECORD_KIND: ClassVar[str] = "ocular.surprise"

    surprise_id: str = ""
    prediction_id: str = ""
    entity_id: str = ""
    kind: str = PredictionKind.POSE.value
    prediction: dict[str, Any] = field(default_factory=dict)
    observation: dict[str, Any] = field(default_factory=dict)
    magnitude: float = 0.0
    tolerance: float = 0.0
    contradicted_belief_id: str = ""
    frame_index: int = -1
    # True when the observation was inside tolerance (diagnostic non-event).
    fired: bool = True

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> SurpriseEvent:
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


def _velocity_from_trajectory(trajectory: list[dict[str, Any]]) -> list[float]:
    """Constant-velocity estimate from the last two observed poses."""
    observed = [item for item in trajectory if item.get("visible", True) and "pose_m" in item]
    if len(observed) < 2:
        return [0.0, 0.0, 0.0]
    a = observed[-2]
    b = observed[-1]
    fa = int(a.get("frame_index", 0))
    fb = int(b.get("frame_index", 1))
    dt = max(1, fb - fa)
    pa = a["pose_m"]
    pb = b["pose_m"]
    return [(float(pb[i]) - float(pa[i])) / dt for i in range(3)]


def _predict_pose(
    entity_id: str,
    pose_m: list[float],
    velocity: list[float],
    *,
    frame_index: int,
    horizon: int,
    tolerance: float,
    source_belief_id: str,
    prediction_index: int,
) -> Prediction:
    expected_pose = [
        float(pose_m[0]) + velocity[0] * horizon,
        float(pose_m[1]) + velocity[1] * horizon,
        float(pose_m[2]) + velocity[2] * horizon,
        *list(pose_m[3:7] if len(pose_m) >= 7 else [1.0, 0.0, 0.0, 0.0]),
    ]
    pid = f"pred-{entity_id}-pose-{frame_index}-{prediction_index}"
    return Prediction(
        id=pid,
        prediction_id=pid,
        entity_id=entity_id,
        kind=PredictionKind.POSE.value,
        expected={"pose_m": expected_pose, "velocity_m_per_frame": list(velocity)},
        tolerance=tolerance,
        tolerance_units=Units.METRE.value,
        horizon_frames=horizon,
        valid_from_frame=frame_index,
        valid_until_frame=frame_index + horizon,
        model="constant_velocity",
        source_belief_id=source_belief_id,
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="predict_pose",
            inputs=[entity_id, source_belief_id or entity_id],
            parameters={
                "horizon": horizon,
                "model": "constant_velocity",
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
            },
        ),
        uncertainty=Uncertainty(
            kind="pose",
            sigma=tolerance,
            units=Units.METRE,
            basis="constant-velocity-extrapolation",
            samples=0,
        ),
    ).seal()


def predict_next(
    world: WorldState,
    *,
    frame_index: int | None = None,
    horizon: int = 1,
    pose_tolerance_m: float = 0.05,
    visibility_tolerance: float = 0.5,
    feature_tolerance: float = 0.15,
    include_frame_features: bool = True,
) -> list[Prediction]:
    """Predict pose, visibility, and optional frame features for the next horizon.

    Pose uses constant-velocity over the tracked trajectory. Visibility expects
    entities seen recently to remain visible unless already occluded for long.
    Frame features predict mean luminance from the world lighting fingerprint.
    """
    if horizon < 1:
        raise ValidationError("horizon must be >= 1")
    fi = world.current_frame if frame_index is None else int(frame_index)
    predictions: list[Prediction] = []
    index = 0
    for entity in world.entities.values():
        if entity.state == "removed":
            continue
        preferred = entity.preferred_belief("pose") or {}
        source_belief = str(preferred.get("belief_id", ""))
        velocity = _velocity_from_trajectory(entity.trajectory)
        pose_pred = _predict_pose(
            entity.entity_id,
            list(entity.pose_m),
            velocity,
            frame_index=fi,
            horizon=horizon,
            tolerance=pose_tolerance_m,
            source_belief_id=source_belief,
            prediction_index=index,
        )
        predictions.append(pose_pred)
        index += 1

        # Visibility: recently seen → visible; long occlusion → still occluded.
        expect_visible = entity.frames_since_seen <= 1
        vis_id = f"pred-{entity.entity_id}-vis-{fi}-{index}"
        predictions.append(
            Prediction(
                id=vis_id,
                prediction_id=vis_id,
                entity_id=entity.entity_id,
                kind=PredictionKind.VISIBILITY.value,
                expected={"visible": expect_visible, "frames_since_seen": entity.frames_since_seen},
                tolerance=visibility_tolerance,
                tolerance_units=Units.UNITLESS.value,
                horizon_frames=horizon,
                valid_from_frame=fi,
                valid_until_frame=fi + horizon,
                model="persistence",
                source_belief_id=source_belief,
                authority=AuthorityClass.MODEL_DERIVED,
                lineage=Lineage(
                    operation="predict_visibility",
                    inputs=[entity.entity_id],
                    parameters={"source_authority": AuthorityClass.INFERRED.value},
                ),
            ).seal()
        )
        index += 1

        # Existence: active entities are predicted to still exist.
        exist_id = f"pred-{entity.entity_id}-exist-{fi}-{index}"
        predictions.append(
            Prediction(
                id=exist_id,
                prediction_id=exist_id,
                entity_id=entity.entity_id,
                kind=PredictionKind.EXISTENCE.value,
                expected={"exists": True},
                tolerance=0.5,
                tolerance_units=Units.UNITLESS.value,
                horizon_frames=horizon,
                valid_from_frame=fi,
                valid_until_frame=fi + horizon,
                model="persistence",
                source_belief_id=source_belief,
                authority=AuthorityClass.MODEL_DERIVED,
                lineage=Lineage(
                    operation="predict_existence",
                    inputs=[entity.entity_id],
                    parameters={"source_authority": AuthorityClass.SENSOR_DERIVED.value},
                ),
            ).seal()
        )
        index += 1

    if include_frame_features:
        lum = world.lighting.get("mean_luminance", world.appearance.get("mean_luminance", 0.5))
        feat_id = f"pred-frame-features-{fi}-{index}"
        predictions.append(
            Prediction(
                id=feat_id,
                prediction_id=feat_id,
                entity_id="",
                kind=PredictionKind.FRAME_FEATURES.value,
                expected={"mean_luminance": float(lum) if lum is not None else 0.5},
                tolerance=feature_tolerance,
                tolerance_units=Units.UNITLESS.value,
                horizon_frames=horizon,
                valid_from_frame=fi,
                valid_until_frame=fi + horizon,
                model="lighting_persistence",
                authority=AuthorityClass.MODEL_DERIVED,
                lineage=Lineage(
                    operation="predict_frame_features",
                    inputs=[world.scene_id],
                    parameters={"source_authority": AuthorityClass.SENSOR_DERIVED.value},
                ),
            ).seal()
        )

    world.predictions.extend(pred.to_dict() for pred in predictions)
    world.digest = ""
    return predictions


def _pose_error(expected: dict[str, Any], observed_pose: list[float]) -> float:
    exp = expected.get("pose_m") or [0.0, 0.0, 0.0]
    return math.sqrt(sum((float(exp[i]) - float(observed_pose[i])) ** 2 for i in range(3)))


def _magnitude_for(prediction: Prediction, observation: dict[str, Any]) -> float:
    kind = prediction.kind
    if kind == PredictionKind.POSE.value:
        pose = observation.get("pose_m")
        if pose is None:
            return float("inf")
        return _pose_error(prediction.expected, list(pose))
    if kind == PredictionKind.VISIBILITY.value:
        expected = bool(prediction.expected.get("visible", True))
        observed = bool(observation.get("visible", True))
        return 0.0 if expected == observed else 1.0
    if kind == PredictionKind.EXISTENCE.value:
        expected = bool(prediction.expected.get("exists", True))
        observed = bool(observation.get("exists", True))
        return 0.0 if expected == observed else 1.0
    if kind == PredictionKind.FRAME_FEATURES.value:
        exp = float(prediction.expected.get("mean_luminance", 0.5))
        obs = observation.get("mean_luminance")
        if obs is None and "lighting" in observation:
            obs = observation["lighting"].get("mean_luminance")
        if obs is None:
            return 0.0
        return abs(exp - float(obs))
    if kind == PredictionKind.CAMERA_PATH.value:
        exp = prediction.expected.get("camera_position") or [0.0, 0.0, 0.0]
        obs = observation.get("camera_position") or observation.get("pose_m") or exp
        return math.sqrt(sum((float(exp[i]) - float(obs[i])) ** 2 for i in range(min(3, len(exp)))))
    if kind == PredictionKind.MATERIAL_RESPONSE.value:
        exp = float(prediction.expected.get("specular_peak", 0.0))
        obs = float(observation.get("specular_peak", exp))
        return abs(exp - obs)
    if kind == PredictionKind.BROWSER_ANIMATION.value:
        exp = prediction.expected.get("animation_phase")
        obs = observation.get("animation_phase")
        if exp is None or obs is None:
            return 0.0
        return abs(float(exp) - float(obs))
    # Unknown kind: require exact equality of expected vs observation subset.
    return 0.0 if prediction.expected == observation else 1.0


def evaluate_prediction(
    world: WorldState,
    prediction: Prediction,
    observation: dict[str, Any],
    *,
    frame_index: int | None = None,
    update_uncertainty: bool = True,
) -> SurpriseEvent | None:
    """Compare one prediction to an observation.

    Returns a SurpriseEvent when magnitude exceeds tolerance (fired=True).
    Returns None when inside tolerance (no surprise). Callers that need the
    in-tolerance diagnostic can use `evaluate_prediction_detailed`.
    """
    event = evaluate_prediction_detailed(
        world,
        prediction,
        observation,
        frame_index=frame_index,
        update_uncertainty=update_uncertainty,
    )
    return event if event.fired else None


def evaluate_prediction_detailed(
    world: WorldState,
    prediction: Prediction,
    observation: dict[str, Any],
    *,
    frame_index: int | None = None,
    update_uncertainty: bool = True,
) -> SurpriseEvent:
    """Always returns a SurpriseEvent record; `fired` is False inside tolerance."""
    fi = world.current_frame if frame_index is None else int(frame_index)
    magnitude = _magnitude_for(prediction, observation)
    fired = magnitude > prediction.tolerance
    sid = f"surprise-{prediction.prediction_id}-{fi}"
    event = SurpriseEvent(
        id=sid,
        surprise_id=sid,
        prediction_id=prediction.prediction_id,
        entity_id=prediction.entity_id,
        kind=prediction.kind,
        prediction={
            "expected": prediction.expected,
            "tolerance": prediction.tolerance,
            "horizon_frames": prediction.horizon_frames,
            "model": prediction.model,
        },
        observation=dict(observation),
        magnitude=float(magnitude) if math.isfinite(magnitude) else 1e9,
        tolerance=prediction.tolerance,
        contradicted_belief_id=prediction.source_belief_id if fired else "",
        frame_index=fi,
        fired=fired,
        authority=AuthorityClass.SENSOR_DERIVED if fired else AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="evaluate_prediction",
            inputs=[prediction.prediction_id, prediction.entity_id or world.scene_id],
            parameters={
                "fired": fired,
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
            },
        ),
        created_at=utc_now(),
    ).seal()

    if fired:
        world.surprises.append(event.to_dict())
        if update_uncertainty and prediction.entity_id and prediction.entity_id in world.entities:
            raise_uncertainty(
                world,
                prediction.entity_id,
                reason=f"surprise:{prediction.kind}",
                magnitude=event.magnitude / max(prediction.tolerance, 1e-6),
                frame_index=fi,
                evidence={
                    "prediction_id": prediction.prediction_id,
                    "magnitude": event.magnitude,
                    "tolerance": prediction.tolerance,
                    "observation": observation,
                },
            )
    elif (
        update_uncertainty
        and prediction.entity_id
        and prediction.entity_id in world.entities
        and prediction.kind == PredictionKind.POSE.value
    ):
        lower_uncertainty(
            world,
            prediction.entity_id,
            reason="prediction_confirmed",
            frame_index=fi,
            evidence={
                "prediction_id": prediction.prediction_id,
                "magnitude": event.magnitude,
                "tolerance": prediction.tolerance,
            },
        )

    world.digest = ""
    return event


def evaluate_observations(
    world: WorldState,
    predictions: list[Prediction],
    observation_frame: dict[str, Any],
    *,
    update_uncertainty: bool = True,
) -> list[SurpriseEvent]:
    """Evaluate a batch of predictions against one observation frame.

    `observation_frame` matches build/update observation shape plus optional
    frame-level keys (mean_luminance, camera_position, animation_phase, …).
    """
    fi = int(observation_frame.get("frame_index", world.current_frame + 1))
    by_entity: dict[str, dict[str, Any]] = {}
    for raw in observation_frame.get("entities", []) or []:
        eid = str(raw.get("entity_id") or raw.get("track_id") or "")
        if eid:
            by_entity[eid] = raw
    present = set(by_entity)

    surprises: list[SurpriseEvent] = []
    for prediction in predictions:
        if prediction.kind == PredictionKind.FRAME_FEATURES.value:
            obs = {
                "mean_luminance": observation_frame.get(
                    "mean_luminance",
                    (observation_frame.get("lighting") or {}).get("mean_luminance"),
                ),
                "lighting": observation_frame.get("lighting", {}),
            }
            event = evaluate_prediction_detailed(
                world, prediction, obs, frame_index=fi, update_uncertainty=update_uncertainty
            )
            if event.fired:
                surprises.append(event)
            continue

        if prediction.kind == PredictionKind.CAMERA_PATH.value:
            obs = {
                "camera_position": observation_frame.get("camera_position"),
                "pose_m": observation_frame.get("camera_position"),
            }
            event = evaluate_prediction_detailed(
                world, prediction, obs, frame_index=fi, update_uncertainty=update_uncertainty
            )
            if event.fired:
                surprises.append(event)
            continue

        if not prediction.entity_id:
            continue

        if prediction.entity_id in by_entity:
            raw = by_entity[prediction.entity_id]
            obs = {
                "pose_m": raw.get("pose_m"),
                "visible": raw.get("visible", True),
                "exists": True,
                "specular_peak": (raw.get("appearance") or {}).get("specular_peak"),
                "animation_phase": raw.get("animation_phase"),
                "mean_luminance": (raw.get("appearance") or {}).get("mean_luminance"),
            }
        else:
            # Missing entity at evaluation time.
            obs = {
                "pose_m": None,
                "visible": False,
                "exists": False,
            }

        if prediction.kind == PredictionKind.EXISTENCE.value:
            obs = {"exists": prediction.entity_id in present}

        event = evaluate_prediction_detailed(
            world, prediction, obs, frame_index=fi, update_uncertainty=update_uncertainty
        )
        if event.fired:
            surprises.append(event)
    return surprises


def list_surprises(world: WorldState, *, entity_id: str | None = None) -> list[dict[str, Any]]:
    """Return recorded surprise events, optionally filtered by entity."""
    rows = list(world.surprises)
    if entity_id is not None:
        rows = [item for item in rows if item.get("entity_id") == entity_id]
    return rows


def make_prediction(
    *,
    entity_id: str,
    kind: str,
    expected: dict[str, Any],
    tolerance: float,
    horizon_frames: int = 1,
    valid_from_frame: int = 0,
    model: str = "manual",
    source_belief_id: str = "",
    tolerance_units: str = Units.UNITLESS.value,
) -> Prediction:
    """Construct a sealed prediction for benchmark scenarios."""
    pid = f"pred-{kind}-{entity_id or 'scene'}-{valid_from_frame}"
    return Prediction(
        id=pid,
        prediction_id=pid,
        entity_id=entity_id,
        kind=kind,
        expected=expected,
        tolerance=tolerance,
        tolerance_units=tolerance_units,
        horizon_frames=horizon_frames,
        valid_from_frame=valid_from_frame,
        valid_until_frame=valid_from_frame + horizon_frames,
        model=model,
        source_belief_id=source_belief_id,
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="make_prediction",
            inputs=[entity_id or "scene"],
            parameters={"source_authority": AuthorityClass.MODEL_DERIVED.value},
        ),
    ).seal()


def uncertainty_trajectory(world: WorldState, entity_id: str) -> list[dict[str, Any]]:
    """Confidence / sigma samples from belief history for one entity."""
    rows: list[dict[str, Any]] = []
    for update in world.belief_history:
        if update.entity_id != entity_id:
            continue
        rows.append(
            {
                "frame_index": update.frame_index,
                "slot": update.slot,
                "model": update.model,
                "confidence_before": update.confidence_before,
                "confidence_after": update.confidence_after,
                "contradiction": update.contradiction,
                "belief_id": update.belief_id,
                "timestamp": update.timestamp,
            }
        )
    return rows
