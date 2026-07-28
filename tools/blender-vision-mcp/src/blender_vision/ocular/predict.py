"""Predictive loop and surprise detection for the Ocular world model.

A prediction carries an expected value, a declared tolerance, and a horizon.
When observation falls outside that tolerance a SurpriseEvent fires, the
contradicted belief is recorded, and entity uncertainty is raised. Confirming
evidence lowers uncertainty again.

Five predictions are produced per tracked entity (from that track's own
kinematics / appearance history — never from a scene description):

  1. expected position (pose)
  2. expected visibility
  3. expected reappearance (if occluded / left)
  4. expected camera result (what a continuing camera move reveals)
  5. expected frame region (which image region to look in)

Five surprises fire on contradictions:

  1. missing expected object
  2. unexpected object
  3. wrong motion
  4. wrong reappearance
  5. camera/object classification contradiction
"""

from __future__ import annotations

import math
import statistics
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
    REAPPEARANCE = "reappearance"
    FRAME_REGION = "frame_region"
    # Alias used in docs / calibration for the camera-result prediction.
    CAMERA_RESULT = "camera_result"


class SurpriseKind(StrEnum):
    """Contradiction classes that a fired surprise must map to."""

    MISSING_EXPECTED_OBJECT = "missing_expected_object"
    UNEXPECTED_OBJECT = "unexpected_object"
    WRONG_MOTION = "wrong_motion"
    WRONG_REAPPEARANCE = "wrong_reappearance"
    CAMERA_OBJECT_CONTRADICTION = "camera_object_classification_contradiction"


# Signal-derived defaults (not tuned on sealed labels).
_DEFAULT_POSE_TOLERANCE_M = 0.05
_DEFAULT_VISIBILITY_TOLERANCE = 0.5
_DEFAULT_FEATURE_TOLERANCE = 0.15
# Reappearance horizon grows with identity uncertainty; base frames of patience
# before the track is treated as "still expected somewhere".
_REAPPEAR_BASE_HORIZON = 4
# Fraction of image extent used when converting pose uncertainty to region pad.
_REGION_PAD_SIGMA_SCALE = 2.0
# Camera-coherence: if ≥ this fraction of live entities share the median
# velocity within envelope, treat motion as ego / camera.
_CAMERA_COHERENCE_FRACTION = 0.6
# Residual motion (object) relative to global that contradicts a camera label.
_CAMERA_RESIDUAL_RATIO = 1.5


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
    # Contradiction class (one of SurpriseKind). Empty when not fired / diagnostic.
    surprise_kind: str = ""
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


# ---------------------------------------------------------------------------
# Kinematics from track history (no scene description)
# ---------------------------------------------------------------------------


def _observed_trajectory(trajectory: list[dict[str, Any]]) -> list[dict[str, Any]]:
    return [item for item in trajectory if item.get("visible", True) and "pose_m" in item]


def _velocity_from_trajectory(trajectory: list[dict[str, Any]]) -> list[float]:
    """Constant-velocity estimate from the last two observed poses."""
    observed = _observed_trajectory(trajectory)
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


def _image_velocity_from_trajectory(trajectory: list[dict[str, Any]]) -> list[float] | None:
    """Pixel (or image-space) velocity from the last two observed image_xy samples.

    Returns None when image_xy is not present on the trajectory — callers must
    not mix pose-space velocity into pixel coordinates.
    """
    observed = _observed_trajectory(trajectory)
    with_xy = [item for item in observed if item.get("image_xy") is not None]
    if len(with_xy) < 2:
        return None
    a = with_xy[-2]
    b = with_xy[-1]
    fa = int(a.get("frame_index", 0))
    fb = int(b.get("frame_index", 1))
    dt = max(1, fb - fa)
    pa = a["image_xy"]
    pb = b["image_xy"]
    return [
        (float(pb[0]) - float(pa[0])) / dt,
        (float(pb[1]) - float(pa[1])) / dt,
    ]


def _velocity_variance(trajectory: list[dict[str, Any]]) -> float:
    """RMS speed residual of successive observed steps — uncertainty envelope."""
    observed = _observed_trajectory(trajectory)
    if len(observed) < 3:
        return 0.0
    speeds: list[float] = []
    for i in range(1, len(observed)):
        a = observed[i - 1]
        b = observed[i]
        fa = int(a.get("frame_index", 0))
        fb = int(b.get("frame_index", 1))
        dt = max(1, fb - fa)
        pa = a["pose_m"]
        pb = b["pose_m"]
        dx = [(float(pb[j]) - float(pa[j])) / dt for j in range(3)]
        speeds.append(math.sqrt(sum(v * v for v in dx)))
    if len(speeds) < 2:
        return 0.0
    mean = sum(speeds) / len(speeds)
    var = sum((s - mean) ** 2 for s in speeds) / max(1, len(speeds) - 1)
    return math.sqrt(var)


def _pose_tolerance_from_signal(
    entity_sigma: float | None,
    velocity_var: float,
    *,
    floor: float = _DEFAULT_POSE_TOLERANCE_M,
) -> float:
    """Tolerance from entity uncertainty and trajectory velocity variance."""
    sigma = float(entity_sigma) if entity_sigma is not None else floor
    # 2-sigma of residual speed as one-frame position envelope, floored.
    return max(floor, sigma, 2.0 * velocity_var)


def _image_xy_from_item(item: dict[str, Any], pose_m: list[float]) -> list[float] | None:
    if "image_xy" in item and item["image_xy"] is not None:
        xy = item["image_xy"]
        return [float(xy[0]), float(xy[1])]
    appearance = item.get("appearance") or {}
    if "centroid_xy" in appearance:
        c = appearance["centroid_xy"]
        return [float(c[0]), float(c[1])]
    if "image_xy" in appearance:
        c = appearance["image_xy"]
        return [float(c[0]), float(c[1])]
    # Fallback: treat pose_m xy as already image-normalised (stream path).
    if len(pose_m) >= 2:
        return [float(pose_m[0]), float(pose_m[1])]
    return None


def _bbox_from_item(item: dict[str, Any], appearance: dict[str, Any]) -> list[float] | None:
    if item.get("bbox_xywh") is not None:
        b = item["bbox_xywh"]
        return [float(b[0]), float(b[1]), float(b[2]), float(b[3])]
    if appearance.get("bbox_xywh") is not None:
        b = appearance["bbox_xywh"]
        return [float(b[0]), float(b[1]), float(b[2]), float(b[3])]
    area = appearance.get("area_px")
    if area is not None and float(area) > 0:
        side = math.sqrt(float(area))
        return [-side / 2.0, -side / 2.0, side, side]
    return None


def _last_image_state(entity: Any) -> tuple[list[float] | None, list[float] | None]:
    """Return (image_xy, bbox_xywh) from the most recent trajectory / appearance."""
    appearance = dict(entity.appearance or {})
    for item in reversed(entity.trajectory):
        pose = item.get("pose_m") or entity.pose_m
        xy = _image_xy_from_item(item, list(pose))
        bbox = _bbox_from_item(item, appearance)
        if xy is not None:
            return xy, bbox
    xy = _image_xy_from_item({"appearance": appearance}, list(entity.pose_m))
    bbox = _bbox_from_item({}, appearance)
    return xy, bbox


def _entity_velocities(world: WorldState) -> dict[str, list[float]]:
    out: dict[str, list[float]] = {}
    for eid, entity in world.entities.items():
        if entity.state == "removed":
            continue
        if entity.frames_since_seen > 1:
            continue
        out[eid] = _velocity_from_trajectory(entity.trajectory)
    return out


def _global_camera_velocity(velocities: dict[str, list[float]]) -> list[float]:
    if not velocities:
        return [0.0, 0.0, 0.0]
    xs = [v[0] for v in velocities.values()]
    ys = [v[1] for v in velocities.values()]
    zs = [v[2] for v in velocities.values()]
    return [
        float(statistics.median(xs)),
        float(statistics.median(ys)),
        float(statistics.median(zs)),
    ]


def _velocity_norm(v: list[float]) -> float:
    return math.sqrt(sum(float(x) * float(x) for x in v[:3]))


def _camera_coherence(
    velocities: dict[str, list[float]],
    global_v: list[float],
    envelope: float,
) -> tuple[bool, float]:
    """Return (is_coherent_camera_motion, coherent_fraction)."""
    if not velocities:
        return False, 0.0
    if _velocity_norm(global_v) < envelope * 0.5:
        # Near-static scene: not a camera-move hypothesis.
        return False, 0.0
    coherent = 0
    for v in velocities.values():
        residual = [v[i] - global_v[i] for i in range(3)]
        if _velocity_norm(residual) <= max(envelope, _velocity_norm(global_v) * 0.5):
            coherent += 1
    frac = coherent / len(velocities)
    return frac >= _CAMERA_COHERENCE_FRACTION, frac


# ---------------------------------------------------------------------------
# Per-kind prediction builders
# ---------------------------------------------------------------------------


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
    image_xy: list[float] | None = None,
    image_velocity: list[float] | None = None,
) -> Prediction:
    expected_pose = [
        float(pose_m[0]) + velocity[0] * horizon,
        float(pose_m[1]) + velocity[1] * horizon,
        float(pose_m[2]) + velocity[2] * horizon,
        *list(pose_m[3:7] if len(pose_m) >= 7 else [1.0, 0.0, 0.0, 0.0]),
    ]
    expected: dict[str, Any] = {
        "pose_m": expected_pose,
        "velocity_m_per_frame": list(velocity),
    }
    if image_xy is not None and image_velocity is not None:
        expected["image_xy"] = [
            float(image_xy[0]) + image_velocity[0] * horizon,
            float(image_xy[1]) + image_velocity[1] * horizon,
        ]
        expected["image_velocity_per_frame"] = list(image_velocity)
    elif image_xy is not None:
        # No image-space velocity history yet — hold last observed pixel locus.
        expected["image_xy"] = [float(image_xy[0]), float(image_xy[1])]
    pid = f"pred-{entity_id}-pose-{frame_index}-{prediction_index}"
    return Prediction(
        id=pid,
        prediction_id=pid,
        entity_id=entity_id,
        kind=PredictionKind.POSE.value,
        expected=expected,
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


def _predict_visibility(
    entity_id: str,
    *,
    frames_since_seen: int,
    frame_index: int,
    horizon: int,
    tolerance: float,
    source_belief_id: str,
    prediction_index: int,
) -> Prediction:
    # Recently seen → visible; long occlusion → still occluded / not visible.
    expect_visible = frames_since_seen <= 1
    vis_id = f"pred-{entity_id}-vis-{frame_index}-{prediction_index}"
    return Prediction(
        id=vis_id,
        prediction_id=vis_id,
        entity_id=entity_id,
        kind=PredictionKind.VISIBILITY.value,
        expected={"visible": expect_visible, "frames_since_seen": frames_since_seen},
        tolerance=tolerance,
        tolerance_units=Units.UNITLESS.value,
        horizon_frames=horizon,
        valid_from_frame=frame_index,
        valid_until_frame=frame_index + horizon,
        model="persistence",
        source_belief_id=source_belief_id,
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="predict_visibility",
            inputs=[entity_id],
            parameters={"source_authority": AuthorityClass.INFERRED.value},
        ),
    ).seal()


def _predict_reappearance(
    entity_id: str,
    pose_m: list[float],
    velocity: list[float],
    *,
    frames_since_seen: int,
    last_observed_frame: int,
    velocity_var: float,
    entity_sigma: float | None,
    frame_index: int,
    horizon: int,
    source_belief_id: str,
    prediction_index: int,
    image_xy: list[float] | None = None,
    image_velocity: list[float] | None = None,
) -> Prediction:
    """Where/when an occluded or departed entity should return.

    Derived solely from last kinematics. Horizon of patience grows with the
    residual velocity envelope (more uncertain motion → longer search window).
    """
    tol = _pose_tolerance_from_signal(entity_sigma, velocity_var)
    # Patience: base + scale by residual; still-visible entities are not
    # expected to "reappear" — they are expected present.
    patience = _REAPPEAR_BASE_HORIZON + int(max(0.0, velocity_var) * 20.0)
    occluded = frames_since_seen > 1
    # Extrapolate through the occlusion gap + horizon.
    steps = max(1, frames_since_seen + horizon)
    reappear_pose = [
        float(pose_m[0]) + velocity[0] * steps,
        float(pose_m[1]) + velocity[1] * steps,
        float(pose_m[2]) + velocity[2] * steps,
        *list(pose_m[3:7] if len(pose_m) >= 7 else [1.0, 0.0, 0.0, 0.0]),
    ]
    # Expected return frame: next frame if already occluded and still inside patience.
    if occluded and frames_since_seen <= patience:
        expect_reappear = True
        reappear_frame = frame_index + horizon
    elif occluded and frames_since_seen > patience:
        # Beyond patience: still predict a locus, but expect not-yet-returned.
        expect_reappear = False
        reappear_frame = last_observed_frame + patience
    else:
        expect_reappear = False
        reappear_frame = frame_index + horizon

    expected: dict[str, Any] = {
        "expect_reappear": expect_reappear,
        "reappear_frame": reappear_frame,
        "reappear_pose_m": reappear_pose,
        "frames_since_seen": frames_since_seen,
        "patience_frames": patience,
        "velocity_m_per_frame": list(velocity),
    }
    if image_xy is not None:
        if image_velocity is not None:
            expected["reappear_image_xy"] = [
                float(image_xy[0]) + image_velocity[0] * steps,
                float(image_xy[1]) + image_velocity[1] * steps,
            ]
            expected["image_velocity_per_frame"] = list(image_velocity)
        else:
            expected["reappear_image_xy"] = [float(image_xy[0]), float(image_xy[1])]
    # Position envelope grows with occlusion duration.
    growing_tol = tol * (1.0 + 0.15 * max(0, frames_since_seen - 1))
    rid = f"pred-{entity_id}-reappear-{frame_index}-{prediction_index}"
    return Prediction(
        id=rid,
        prediction_id=rid,
        entity_id=entity_id,
        kind=PredictionKind.REAPPEARANCE.value,
        expected=expected,
        tolerance=growing_tol,
        tolerance_units=Units.METRE.value,
        horizon_frames=horizon,
        valid_from_frame=frame_index,
        valid_until_frame=frame_index + max(horizon, patience),
        model="kinematic_reappearance",
        source_belief_id=source_belief_id,
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="predict_reappearance",
            inputs=[entity_id],
            parameters={
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
                "occluded": occluded,
            },
        ),
        uncertainty=Uncertainty(
            kind="pose",
            sigma=growing_tol,
            units=Units.METRE,
            basis="occlusion-extrapolation",
            samples=0,
        ),
    ).seal()


def _predict_frame_region(
    entity_id: str,
    pose_m: list[float],
    velocity: list[float],
    *,
    frame_index: int,
    horizon: int,
    tolerance: float,
    source_belief_id: str,
    prediction_index: int,
    image_xy: list[float] | None,
    image_velocity: list[float] | None,
    bbox_xywh: list[float] | None,
    image_tolerance_px: float | None = None,
) -> Prediction:
    """Image region the entity is expected to occupy next frame."""
    if image_xy is not None and image_velocity is not None:
        cx = float(image_xy[0]) + image_velocity[0] * horizon
        cy = float(image_xy[1]) + image_velocity[1] * horizon
    elif image_xy is not None:
        cx = float(image_xy[0])
        cy = float(image_xy[1])
    else:
        # pose_m used as image-space proxy (stream / perception path).
        cx = float(pose_m[0]) + velocity[0] * horizon
        cy = float(pose_m[1]) + velocity[1] * horizon

    if bbox_xywh is not None and bbox_xywh[2] > 0 and bbox_xywh[3] > 0:
        w, h = float(bbox_xywh[2]), float(bbox_xywh[3])
    else:
        # Unitless fallback region around the predicted point.
        w, h = max(0.05, tolerance * 4.0), max(0.05, tolerance * 4.0)

    # Prefer a pixel envelope when image kinematics exist; else pose tolerance.
    region_tol = float(image_tolerance_px) if image_tolerance_px is not None else tolerance
    pad = _REGION_PAD_SIGMA_SCALE * region_tol
    region = [cx - w / 2.0 - pad, cy - h / 2.0 - pad, w + 2.0 * pad, h + 2.0 * pad]
    rid = f"pred-{entity_id}-region-{frame_index}-{prediction_index}"
    return Prediction(
        id=rid,
        prediction_id=rid,
        entity_id=entity_id,
        kind=PredictionKind.FRAME_REGION.value,
        expected={
            "region_xywh": region,
            "centroid_xy": [cx, cy],
            "bbox_xywh": [cx - w / 2.0, cy - h / 2.0, w, h],
            "pad": pad,
        },
        tolerance=max(region_tol, pad),
        tolerance_units=Units.PIXEL.value,
        horizon_frames=horizon,
        valid_from_frame=frame_index,
        valid_until_frame=frame_index + horizon,
        model="kinematic_region",
        source_belief_id=source_belief_id,
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="predict_frame_region",
            inputs=[entity_id],
            parameters={"source_authority": AuthorityClass.SENSOR_DERIVED.value},
        ),
        uncertainty=Uncertainty(
            kind="region",
            sigma=max(region_tol, pad),
            units=Units.PIXEL,
            basis="bbox-plus-velocity-envelope",
            samples=0,
        ),
    ).seal()


def _predict_camera_result(
    world: WorldState,
    *,
    frame_index: int,
    horizon: int,
    prediction_index: int,
) -> Prediction:
    """What continuing the estimated camera motion should reveal next.

    Global velocity = median of live entity velocities. Coherent motion ⇒
    camera / ego hypothesis; each entity is expected to shift by that global
    delta. Residuals (object motion) are recorded for contradiction checks.
    """
    velocities = _entity_velocities(world)
    envelopes = {
        eid: _pose_tolerance_from_signal(
            world.entities[eid].uncertainty.sigma if world.entities[eid].uncertainty else None,
            _velocity_variance(world.entities[eid].trajectory),
        )
        for eid in velocities
    }
    envelope = max(envelopes.values()) if envelopes else _DEFAULT_POSE_TOLERANCE_M
    global_v = _global_camera_velocity(velocities)
    coherent, frac = _camera_coherence(velocities, global_v, envelope)

    entity_shifts: dict[str, list[float]] = {}
    residual_motion: dict[str, list[float]] = {}
    for eid, v in velocities.items():
        entity_shifts[eid] = [global_v[i] * horizon for i in range(3)]
        residual_motion[eid] = [v[i] - global_v[i] for i in range(3)]

    # Entities currently occluded: under camera motion they should re-enter the
    # field of view near their extrapolated image locus (same kinematic model).
    revealed: list[dict[str, Any]] = []
    for eid, entity in world.entities.items():
        if entity.state == "removed":
            continue
        if entity.frames_since_seen <= 1:
            continue
        vel = _velocity_from_trajectory(entity.trajectory)
        # Under pure camera motion, occluded objects also shift by global_v.
        shift_v = global_v if coherent else vel
        steps = entity.frames_since_seen + horizon
        pose = list(entity.pose_m)
        predicted = [
            float(pose[0]) + shift_v[0] * steps,
            float(pose[1]) + shift_v[1] * steps,
            float(pose[2]) + shift_v[2] * steps,
        ]
        revealed.append(
            {
                "entity_id": eid,
                "expected_pose_m": predicted,
                "reason": "camera_continuation" if coherent else "object_kinematics",
            }
        )

    cid = f"pred-camera-result-{frame_index}-{prediction_index}"
    return Prediction(
        id=cid,
        prediction_id=cid,
        entity_id="",
        kind=PredictionKind.CAMERA_RESULT.value,
        expected={
            "camera_velocity": list(global_v),
            "hypothesis": "camera_motion" if coherent else "static_or_independent",
            "coherent_fraction": frac,
            "entity_shifts": entity_shifts,
            "residual_motion": residual_motion,
            "revealed_entities": revealed,
            "horizon_frames": horizon,
        },
        tolerance=envelope,
        tolerance_units=Units.METRE.value,
        horizon_frames=horizon,
        valid_from_frame=frame_index,
        valid_until_frame=frame_index + horizon,
        model="median_egomotion",
        source_belief_id="",
        authority=AuthorityClass.MODEL_DERIVED,
        lineage=Lineage(
            operation="predict_camera_result",
            inputs=[world.scene_id],
            parameters={
                "n_live_entities": len(velocities),
                "coherent": coherent,
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
            },
        ),
        uncertainty=Uncertainty(
            kind="camera",
            sigma=envelope,
            units=Units.METRE,
            basis="median-entity-velocity",
            samples=len(velocities),
        ),
    ).seal()


def predict_next(
    world: WorldState,
    *,
    frame_index: int | None = None,
    horizon: int = 1,
    pose_tolerance_m: float = _DEFAULT_POSE_TOLERANCE_M,
    visibility_tolerance: float = _DEFAULT_VISIBILITY_TOLERANCE,
    feature_tolerance: float = _DEFAULT_FEATURE_TOLERANCE,
    include_frame_features: bool = True,
    include_camera_result: bool = True,
) -> list[Prediction]:
    """Predict the five track-derived quantities for the next horizon.

    Pose uses constant-velocity over the tracked trajectory. Visibility expects
    entities seen recently to remain visible unless already occluded for long.
    Reappearance extrapolates occluded tracks. Camera result estimates what a
    continuing camera move would reveal from multi-track coherence. Frame region
    names the image locus to re-observe. Optional frame features keep lighting
    persistence for stream diagnostics.
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
        v_var = _velocity_variance(entity.trajectory)
        sigma = entity.uncertainty.sigma if entity.uncertainty else None
        # Caller floor can raise, never lower below signal envelope.
        pose_tol = max(
            pose_tolerance_m,
            _pose_tolerance_from_signal(sigma, v_var, floor=pose_tolerance_m),
        )
        image_xy, bbox = _last_image_state(entity)
        image_velocity = _image_velocity_from_trajectory(entity.trajectory)
        # Pixel envelope from image-velocity residual when available; else a
        # bbox-scale floor so region tolerance is not a bare pose metre value.
        if image_velocity is not None and bbox is not None:
            image_tol_px = max(2.0, 0.25 * max(float(bbox[2]), float(bbox[3])))
        elif bbox is not None:
            image_tol_px = max(2.0, 0.25 * max(float(bbox[2]), float(bbox[3])))
        else:
            image_tol_px = None

        predictions.append(
            _predict_pose(
                entity.entity_id,
                list(entity.pose_m),
                velocity,
                frame_index=fi,
                horizon=horizon,
                tolerance=pose_tol,
                source_belief_id=source_belief,
                prediction_index=index,
                image_xy=image_xy,
                image_velocity=image_velocity,
            )
        )
        index += 1

        predictions.append(
            _predict_visibility(
                entity.entity_id,
                frames_since_seen=entity.frames_since_seen,
                frame_index=fi,
                horizon=horizon,
                tolerance=visibility_tolerance,
                source_belief_id=source_belief,
                prediction_index=index,
            )
        )
        index += 1

        predictions.append(
            _predict_reappearance(
                entity.entity_id,
                list(entity.pose_m),
                velocity,
                frames_since_seen=entity.frames_since_seen,
                last_observed_frame=entity.last_observed_frame,
                velocity_var=v_var,
                entity_sigma=sigma,
                frame_index=fi,
                horizon=horizon,
                source_belief_id=source_belief,
                prediction_index=index,
                image_xy=image_xy,
                image_velocity=image_velocity,
            )
        )
        index += 1

        predictions.append(
            _predict_frame_region(
                entity.entity_id,
                list(entity.pose_m),
                velocity,
                frame_index=fi,
                horizon=horizon,
                tolerance=pose_tol,
                source_belief_id=source_belief,
                prediction_index=index,
                image_xy=image_xy,
                image_velocity=image_velocity,
                bbox_xywh=bbox,
                image_tolerance_px=image_tol_px,
            )
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

    if include_camera_result:
        predictions.append(
            _predict_camera_result(
                world, frame_index=fi, horizon=horizon, prediction_index=index
            )
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


# ---------------------------------------------------------------------------
# Magnitude / evaluation
# ---------------------------------------------------------------------------


def _pose_error(expected: dict[str, Any], observed_pose: list[float]) -> float:
    exp = expected.get("pose_m") or expected.get("reappear_pose_m") or [0.0, 0.0, 0.0]
    return math.sqrt(sum((float(exp[i]) - float(observed_pose[i])) ** 2 for i in range(3)))


def _point_in_region(xy: list[float], region_xywh: list[float]) -> bool:
    x, y, w, h = (float(region_xywh[i]) for i in range(4))
    return x <= xy[0] <= x + w and y <= xy[1] <= y + h


def _region_centroid_error(expected: dict[str, Any], observation: dict[str, Any]) -> float:
    exp_c = expected.get("centroid_xy")
    obs_c = observation.get("centroid_xy") or observation.get("image_xy")
    if exp_c is None:
        return 0.0
    if obs_c is None and observation.get("pose_m") is not None:
        pose = observation["pose_m"]
        obs_c = [float(pose[0]), float(pose[1])]
    if obs_c is None:
        # Invisible: infinite region error (missing).
        if observation.get("visible") is False:
            return float("inf")
        return 0.0
    return math.sqrt(
        (float(exp_c[0]) - float(obs_c[0])) ** 2 + (float(exp_c[1]) - float(obs_c[1])) ** 2
    )


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
    if kind in {PredictionKind.CAMERA_PATH.value, PredictionKind.CAMERA_RESULT.value}:
        # For legacy CAMERA_PATH: position residual. For CAMERA_RESULT: residual
        # of observed global velocity vs predicted camera velocity when provided.
        if "camera_velocity" in prediction.expected and "camera_velocity" in observation:
            exp = prediction.expected["camera_velocity"]
            obs = observation["camera_velocity"]
            return math.sqrt(
                sum((float(exp[i]) - float(obs[i])) ** 2 for i in range(min(3, len(exp))))
            )
        exp = prediction.expected.get("camera_position") or [0.0, 0.0, 0.0]
        obs = observation.get("camera_position") or observation.get("pose_m") or exp
        return math.sqrt(
            sum((float(exp[i]) - float(obs[i])) ** 2 for i in range(min(3, len(exp))))
        )
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
    if kind == PredictionKind.REAPPEARANCE.value:
        expect_reappear = bool(prediction.expected.get("expect_reappear", False))
        visible = bool(observation.get("visible", False))
        exists = bool(observation.get("exists", visible))
        if not expect_reappear:
            # Not expecting a reappearance this horizon: only surprise if a
            # claimed reappearance arrives far from the kinematic locus.
            if not visible:
                return 0.0
            pose = observation.get("pose_m")
            if pose is None:
                return 0.0
            return _pose_error(prediction.expected, list(pose))
        # Expecting reappearance: missing is a timing failure; wrong place is
        # position failure. Magnitude in pose units.
        if not visible and not exists:
            # Still absent — timing residual in frames, normalised by patience.
            patience = max(1, int(prediction.expected.get("patience_frames", 1)))
            since = int(
                observation.get(
                    "frames_since_seen",
                    prediction.expected.get("frames_since_seen", 0),
                )
            )
            # Inside patience → no surprise yet; beyond → magnitude grows.
            if since <= patience:
                return 0.0
            return float(since - patience) / float(patience)
        pose = observation.get("pose_m")
        if pose is None:
            return float("inf")
        return _pose_error(prediction.expected, list(pose))
    if kind == PredictionKind.FRAME_REGION.value:
        err = _region_centroid_error(prediction.expected, observation)
        if not math.isfinite(err):
            return err
        # Also check containment when a region is declared.
        region = prediction.expected.get("region_xywh")
        obs_c = observation.get("centroid_xy") or observation.get("image_xy")
        if obs_c is None and observation.get("pose_m") is not None:
            pose = observation["pose_m"]
            obs_c = [float(pose[0]), float(pose[1])]
        if region is not None and obs_c is not None and not _point_in_region(
            [float(obs_c[0]), float(obs_c[1])], list(region)
        ):
            return max(err, prediction.tolerance + err)
        return err
    # Unknown kind: require exact equality of expected vs observation subset.
    return 0.0 if prediction.expected == observation else 1.0


def _surprise_kind_for(
    prediction: Prediction,
    observation: dict[str, Any],
    *,
    fired: bool,
    magnitude: float,
) -> str:
    """Map a fired prediction failure onto one of the five contradiction classes."""
    if not fired:
        return ""
    kind = prediction.kind
    if kind == PredictionKind.VISIBILITY.value:
        expected = bool(prediction.expected.get("visible", True))
        observed = bool(observation.get("visible", True))
        if expected and not observed:
            return SurpriseKind.MISSING_EXPECTED_OBJECT.value
        if not expected and observed:
            # Reappearance of something predicted invisible — reappearance path.
            return SurpriseKind.WRONG_REAPPEARANCE.value
        return SurpriseKind.WRONG_MOTION.value
    if kind == PredictionKind.EXISTENCE.value:
        expected = bool(prediction.expected.get("exists", True))
        observed = bool(observation.get("exists", True))
        if expected and not observed:
            return SurpriseKind.MISSING_EXPECTED_OBJECT.value
        if not expected and observed:
            return SurpriseKind.UNEXPECTED_OBJECT.value
        return SurpriseKind.WRONG_MOTION.value
    if kind == PredictionKind.POSE.value:
        return SurpriseKind.WRONG_MOTION.value
    if kind == PredictionKind.REAPPEARANCE.value:
        return SurpriseKind.WRONG_REAPPEARANCE.value
    if kind == PredictionKind.FRAME_REGION.value:
        if observation.get("visible") is False or observation.get("pose_m") is None:
            return SurpriseKind.MISSING_EXPECTED_OBJECT.value
        return SurpriseKind.WRONG_MOTION.value
    if kind in {PredictionKind.CAMERA_PATH.value, PredictionKind.CAMERA_RESULT.value}:
        return SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value
    return SurpriseKind.WRONG_MOTION.value


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
    surprise_kind = _surprise_kind_for(
        prediction, observation, fired=fired, magnitude=magnitude
    )
    sid = f"surprise-{prediction.prediction_id}-{fi}"
    event = SurpriseEvent(
        id=sid,
        surprise_id=sid,
        prediction_id=prediction.prediction_id,
        entity_id=prediction.entity_id,
        kind=prediction.kind,
        surprise_kind=surprise_kind,
        prediction={
            "expected": prediction.expected,
            "tolerance": prediction.tolerance,
            "horizon_frames": prediction.horizon_frames,
            "model": prediction.model,
            "kind": prediction.kind,
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
                "surprise_kind": surprise_kind,
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
                reason=f"surprise:{surprise_kind or prediction.kind}",
                magnitude=event.magnitude / max(prediction.tolerance, 1e-6),
                frame_index=fi,
                evidence={
                    "prediction_id": prediction.prediction_id,
                    "magnitude": event.magnitude,
                    "tolerance": prediction.tolerance,
                    "surprise_kind": surprise_kind,
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


def _make_free_surprise(
    *,
    surprise_kind: str,
    entity_id: str,
    frame_index: int,
    observation: dict[str, Any],
    prediction: dict[str, Any],
    magnitude: float,
    tolerance: float,
    prediction_id: str = "",
    kind: str = "",
) -> SurpriseEvent:
    sid = f"surprise-{surprise_kind}-{entity_id or 'scene'}-{frame_index}"
    return SurpriseEvent(
        id=sid,
        surprise_id=sid,
        prediction_id=prediction_id,
        entity_id=entity_id,
        kind=kind or surprise_kind,
        surprise_kind=surprise_kind,
        prediction=prediction,
        observation=dict(observation),
        magnitude=magnitude,
        tolerance=tolerance,
        frame_index=frame_index,
        fired=True,
        authority=AuthorityClass.SENSOR_DERIVED,
        lineage=Lineage(
            operation="evaluate_observations",
            inputs=[entity_id or "scene"],
            parameters={
                "fired": True,
                "surprise_kind": surprise_kind,
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
            },
        ),
        created_at=utc_now(),
    ).seal()


def _evaluate_camera_object_contradiction(
    world: WorldState,
    camera_pred: Prediction | None,
    observation_frame: dict[str, Any],
    *,
    frame_index: int,
) -> list[SurpriseEvent]:
    """Detect motion attributed to camera that was object's, or vice versa.

    Uses the same median-egomotion signal as the camera-result prediction —
    no sealed labels. If the prediction claimed camera_motion but one entity's
    residual dominates, or claimed static/independent while motion is coherent,
    fire CAMERA_OBJECT_CONTRADICTION.
    """
    by_entity: dict[str, dict[str, Any]] = {}
    for raw in observation_frame.get("entities", []) or []:
        eid = str(raw.get("entity_id") or raw.get("track_id") or "")
        if eid:
            by_entity[eid] = raw

    # Observed velocities: prefer observation-provided; else entity pose delta
    # against world last pose.
    observed_v: dict[str, list[float]] = {}
    for eid, raw in by_entity.items():
        if "velocity_m_per_frame" in raw:
            observed_v[eid] = [float(x) for x in raw["velocity_m_per_frame"][:3]]
            continue
        pose = raw.get("pose_m")
        if pose is None or eid not in world.entities:
            continue
        prior = world.entities[eid].pose_m
        observed_v[eid] = [
            float(pose[0]) - float(prior[0]),
            float(pose[1]) - float(prior[1]),
            float(pose[2]) - float(prior[2]),
        ]
    if len(observed_v) < 2:
        return []

    global_v = _global_camera_velocity(observed_v)
    envelope = camera_pred.tolerance if camera_pred is not None else _DEFAULT_POSE_TOLERANCE_M
    coherent, frac = _camera_coherence(observed_v, global_v, envelope)
    predicted_hypothesis = (
        (camera_pred.expected.get("hypothesis") if camera_pred is not None else None)
        or "static_or_independent"
    )

    events: list[SurpriseEvent] = []
    # Case A: predicted camera motion, but a residual is large → object motion
    # was swallowed by the camera label.
    if predicted_hypothesis == "camera_motion":
        for eid, v in observed_v.items():
            residual = [v[i] - global_v[i] for i in range(3)]
            r_norm = _velocity_norm(residual)
            g_norm = max(_velocity_norm(global_v), 1e-9)
            if r_norm > max(envelope, _CAMERA_RESIDUAL_RATIO * envelope) and r_norm > 0.35 * g_norm:
                ev = _make_free_surprise(
                    surprise_kind=SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
                    entity_id=eid,
                    frame_index=frame_index,
                    observation={
                        "velocity": v,
                        "residual": residual,
                        "global_velocity": global_v,
                        "coherent_fraction": frac,
                    },
                    prediction={
                        "hypothesis": predicted_hypothesis,
                        "camera_velocity": (
                            camera_pred.expected.get("camera_velocity") if camera_pred else global_v
                        ),
                    },
                    magnitude=r_norm,
                    tolerance=envelope,
                    prediction_id=camera_pred.prediction_id if camera_pred else "",
                    kind=PredictionKind.CAMERA_RESULT.value,
                )
                world.surprises.append(ev.to_dict())
                events.append(ev)
        # Coherence collapsed entirely after a camera hypothesis.
        if not coherent and frac < _CAMERA_COHERENCE_FRACTION * 0.5:
            ev = _make_free_surprise(
                surprise_kind=SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
                entity_id="",
                frame_index=frame_index,
                observation={
                    "coherent_fraction": frac,
                    "global_velocity": global_v,
                    "n_entities": len(observed_v),
                },
                prediction={"hypothesis": predicted_hypothesis},
                magnitude=1.0 - frac,
                tolerance=1.0 - _CAMERA_COHERENCE_FRACTION,
                prediction_id=camera_pred.prediction_id if camera_pred else "",
                kind=PredictionKind.CAMERA_RESULT.value,
            )
            world.surprises.append(ev.to_dict())
            events.append(ev)

    # Case B: predicted independent object motion, but observation is coherent
    # camera motion → object motion was really the camera.
    if predicted_hypothesis != "camera_motion" and coherent and _velocity_norm(global_v) > envelope:
        # Only fire if individual pose predictions treated motion as object-owned
        # (i.e. residuals near zero under global). One scene-level event.
        ev = _make_free_surprise(
            surprise_kind=SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
            entity_id="",
            frame_index=frame_index,
            observation={
                "coherent_fraction": frac,
                "global_velocity": global_v,
                "n_entities": len(observed_v),
            },
            prediction={"hypothesis": predicted_hypothesis},
            magnitude=frac,
            tolerance=_CAMERA_COHERENCE_FRACTION,
            prediction_id=camera_pred.prediction_id if camera_pred else "",
            kind=PredictionKind.CAMERA_RESULT.value,
        )
        world.surprises.append(ev.to_dict())
        events.append(ev)

    return events


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

    Also fires free-standing surprises for unexpected objects and
    camera/object classification contradictions.
    """
    fi = int(observation_frame.get("frame_index", world.current_frame + 1))
    by_entity: dict[str, dict[str, Any]] = {}
    for raw in observation_frame.get("entities", []) or []:
        eid = str(raw.get("entity_id") or raw.get("track_id") or "")
        if eid:
            by_entity[eid] = raw
    present = set(by_entity)

    predicted_entity_ids = {p.entity_id for p in predictions if p.entity_id}
    camera_pred: Prediction | None = None
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

        if prediction.kind in {
            PredictionKind.CAMERA_PATH.value,
            PredictionKind.CAMERA_RESULT.value,
        }:
            camera_pred = prediction
            # Observed camera velocity from multi-entity median if not given.
            obs_velocities: dict[str, list[float]] = {}
            for eid, raw in by_entity.items():
                pose = raw.get("pose_m")
                if pose is None or eid not in world.entities:
                    continue
                prior = world.entities[eid].pose_m
                obs_velocities[eid] = [
                    float(pose[0]) - float(prior[0]),
                    float(pose[1]) - float(prior[1]),
                    float(pose[2]) - float(prior[2]),
                ]
            obs_cam_v = observation_frame.get("camera_velocity")
            if obs_cam_v is None and obs_velocities:
                obs_cam_v = _global_camera_velocity(obs_velocities)
            obs = {
                "camera_position": observation_frame.get("camera_position"),
                "pose_m": observation_frame.get("camera_position"),
                "camera_velocity": obs_cam_v,
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
                "centroid_xy": raw.get("centroid_xy")
                or (raw.get("appearance") or {}).get("centroid_xy"),
                "image_xy": raw.get("image_xy")
                or (raw.get("appearance") or {}).get("image_xy")
                or (raw.get("appearance") or {}).get("centroid_xy"),
                "bbox_xywh": raw.get("bbox_xywh")
                or (raw.get("appearance") or {}).get("bbox_xywh"),
                "frames_since_seen": 0,
            }
        else:
            # Missing entity at evaluation time.
            prior_entity = world.entities.get(prediction.entity_id)
            frames_since = (
                (prior_entity.frames_since_seen + 1) if prior_entity is not None else 1
            )
            obs = {
                "pose_m": None,
                "visible": False,
                "exists": False,
                "frames_since_seen": frames_since,
            }

        if prediction.kind == PredictionKind.EXISTENCE.value:
            obs = {"exists": prediction.entity_id in present}

        event = evaluate_prediction_detailed(
            world, prediction, obs, frame_index=fi, update_uncertainty=update_uncertainty
        )
        if event.fired:
            surprises.append(event)

    # Unexpected objects: observed identities the predictor did not know about.
    for eid, raw in by_entity.items():
        if eid in predicted_entity_ids:
            continue
        if eid in world.entities and world.entities[eid].state != "removed":
            # Known to the world but somehow not predicted this tick — skip
            # (predict_next should have covered it).
            continue
        ev = _make_free_surprise(
            surprise_kind=SurpriseKind.UNEXPECTED_OBJECT.value,
            entity_id=eid,
            frame_index=fi,
            observation={
                "pose_m": raw.get("pose_m"),
                "visible": raw.get("visible", True),
                "centroid_xy": raw.get("centroid_xy")
                or (raw.get("appearance") or {}).get("centroid_xy"),
            },
            prediction={"expected_entities": sorted(predicted_entity_ids)},
            magnitude=1.0,
            tolerance=0.5,
            kind=PredictionKind.EXISTENCE.value,
        )
        world.surprises.append(ev.to_dict())
        surprises.append(ev)

    # Camera / object classification contradiction (retina confounder class).
    cam_events = _evaluate_camera_object_contradiction(
        world, camera_pred, observation_frame, frame_index=fi
    )
    surprises.extend(cam_events)

    world.digest = ""
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
