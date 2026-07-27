"""Ocular stream and attention records (Bible sections 5–6).

Every frame is content-addressed and sealed. After seal, frames refuse field
mutation so downstream consumers can treat an OcularFrame as a fixed sample.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any, ClassVar

from blender_vision.core.errors import ValidationError
from blender_vision.v2.authority import CoordinateFrame
from blender_vision.v2.records import Lineage, V2Record


class ColourSpace(StrEnum):
    SRGB = "srgb"
    LINEAR_RGB = "linear_rgb"
    GRAY = "gray"
    BGR = "bgr"
    UNKNOWN = "unknown"


class FixationReason(StrEnum):
    SALIENCE = "salience"
    UNCERTAINTY = "uncertainty"
    MOTION = "motion"
    CRITIC = "critic"
    TASK = "task"
    EXPECTED_EVENT = "expected_event"
    MANUAL = "manual"


class FixationOutcome(StrEnum):
    PENDING = "pending"
    OBSERVED = "observed"
    REDUNDANT = "redundant"
    SUPPRESSED_IOR = "suppressed_ior"
    BUDGET_EXHAUSTED = "budget_exhausted"
    FAILED = "failed"


@dataclass(slots=True, kw_only=True)
class SensorCalibration(V2Record):
    """Lens, principal-point, timestamp, exposure, and colour calibration receipt."""

    RECORD_KIND: ClassVar[str] = "ocular.sensor-calibration"

    sensor_id: str = ""
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="opencv-camera", up_axis="-Y", forward_axis="+Z"
        )
    )
    image_size: list[int] = field(default_factory=lambda: [0, 0])
    camera_matrix: list[list[float]] = field(default_factory=list)
    distortion_coefficients: list[float] = field(default_factory=list)
    principal_point: list[float] = field(default_factory=lambda: [0.0, 0.0])
    reprojection_error_px: float | None = None
    timestamp_skew_ms: float | None = None
    exposure_pumping_detected: bool = False
    colour_temperature_drift_k: float | None = None
    physical_scale_m: float | None = None
    board_square_m: float | None = None
    samples_used: int = 0
    method: str = "unspecified"
    limitations: list[str] = field(default_factory=list)

    def seal(self: SensorCalibration) -> SensorCalibration:
        # Explicit base call: slots=True rebuilds the class and breaks zero-arg super().
        return V2Record.seal(self)


@dataclass(slots=True, kw_only=True)
class OcularFrame(V2Record):
    """One calibrated sample from an ocular stream. Immutable after seal."""

    RECORD_KIND: ClassVar[str] = "ocular.frame"

    frame_id: str = ""
    stream_id: str = ""
    timestamp: float = 0.0
    sensor_state: dict[str, Any] = field(default_factory=dict)
    image_digest: str = ""
    resolution: list[int] = field(default_factory=lambda: [0, 0])
    colour_space: ColourSpace = ColourSpace.SRGB
    exposure: float = 1.0
    camera_intrinsics: dict[str, Any] = field(default_factory=dict)
    camera_pose_if_known: dict[str, Any] | None = None
    depth_digest: str = ""
    motion_digest: str = ""
    privacy_mask_digest: str = ""
    calibration_receipt: str = ""
    coordinate_frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="opencv-camera", up_axis="-Y", forward_axis="+Z"
        )
    )
    sequence_index: int = 0
    dropped_before: int = 0
    # Instance lock (excluded from content digest via payload override).
    _locked: bool = field(default=False, init=False, repr=False, compare=False)

    def __setattr__(self, name: str, value: Any) -> None:
        if name != "_locked" and getattr(self, "_locked", False):
            raise AttributeError(
                f"OcularFrame {getattr(self, 'id', '?')} is immutable after seal "
                f"(attempted to set {name!r})"
            )
        object.__setattr__(self, name, value)

    def payload(self) -> dict[str, Any]:
        value = V2Record.payload(self)
        value.pop("_locked", None)
        return value

    def seal(self: OcularFrame) -> OcularFrame:
        # Unlock → write digest → re-lock. seal() is the only sanctioned writer.
        object.__setattr__(self, "_locked", False)
        V2Record.seal(self)
        object.__setattr__(self, "_locked", True)
        return self


@dataclass(slots=True, kw_only=True)
class AttentionBudget(V2Record):
    """Compute accounting for one fixation or saccade decision."""

    RECORD_KIND: ClassVar[str] = "ocular.attention-budget"

    compute_cost_ms: float = 0.0
    latency_ms: float = 0.0
    expected_gain: float = 0.0
    actual_gain: float = 0.0
    redundant_observations: int = 0
    resolution: list[int] = field(default_factory=lambda: [0, 0])
    models_requested: list[str] = field(default_factory=list)

    def seal(self: AttentionBudget) -> AttentionBudget:
        return V2Record.seal(self)


@dataclass(slots=True, kw_only=True)
class Fixation(V2Record):
    """A committed gaze hold on a region. Bible section 6.3."""

    RECORD_KIND: ClassVar[str] = "ocular.fixation"

    target: str = ""
    region: list[float] = field(default_factory=lambda: [0.0, 0.0, 1.0, 1.0])
    reason: FixationReason = FixationReason.SALIENCE
    expected_information: float = 0.0
    duration_ms: float = 0.0
    resolution: list[int] = field(default_factory=lambda: [0, 0])
    models_requested: list[str] = field(default_factory=list)
    outcome: FixationOutcome = FixationOutcome.PENDING
    stream_id: str = ""
    frame_id: str = ""
    budget: dict[str, Any] = field(default_factory=dict)
    uncertainty_at_start: float = 0.0
    uncertainty_at_end: float = 0.0
    evidence_changed: bool = False
    critic_requested: bool = False

    def seal(self: Fixation) -> Fixation:
        return V2Record.seal(self)


@dataclass(slots=True, kw_only=True)
class SaccadePlan(V2Record):
    """A planned gaze jump from one fixation to the next."""

    RECORD_KIND: ClassVar[str] = "ocular.saccade-plan"

    from_region: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0, 0.0])
    to_region: list[float] = field(default_factory=lambda: [0.0, 0.0, 1.0, 1.0])
    reason: FixationReason = FixationReason.SALIENCE
    expected_information: float = 0.0
    amplitude_norm: float = 0.0
    duration_ms: float = 0.0
    stream_id: str = ""
    from_fixation_id: str = ""
    to_fixation_id: str = ""
    inhibited: bool = False
    inhibition_reason: str = ""

    def seal(self: SaccadePlan) -> SaccadePlan:
        return V2Record.seal(self)


@dataclass(slots=True, kw_only=True)
class RetinalEvent(V2Record):
    """One Bible 6.5 retinal event bound to stream evidence."""

    RECORD_KIND: ClassVar[str] = "ocular.retinal-event"

    event_type: str = ""
    stream_id: str = ""
    frame_id: str = ""
    timestamp: float = 0.0
    region: list[float] = field(default_factory=list)
    confidence: float = 0.0
    evidence: dict[str, Any] = field(default_factory=dict)
    # Correctness labels are set by the attentive path; the reflex lane may
    # never rewrite them. The lane that wrote the label is recorded explicitly.
    correctness_label: str = ""
    written_by_lane: str = "attentive"
    reflex_resolution: list[int] | None = None

    def seal(self: RetinalEvent) -> RetinalEvent:
        if self.event_type and self.event_type not in RETINAL_EVENT_TYPES:
            raise ValidationError(f"unknown retinal event type: {self.event_type!r}")
        return V2Record.seal(self)


#: Bible section 6.5 complete event vocabulary.
RETINAL_EVENT_TYPES: frozenset[str] = frozenset(
    {
        "OBJECT_ENTERED",
        "OBJECT_LEFT",
        "OBJECT_MOVED",
        "OBJECT_OCCLUDED",
        "OBJECT_REAPPEARED",
        "SURFACE_CHANGED",
        "TEXT_CHANGED",
        "LIGHT_CHANGED",
        "CAMERA_MOVED",
        "NEW_UNKNOWN_REGION",
        "EXPECTED_EVENT_MISSING",
    }
)


def default_lineage(
    operation: str,
    *,
    inputs: list[str] | None = None,
    input_authorities: list[str] | None = None,
) -> Lineage:
    # Leave input_authorities empty unless the caller has real measured inputs.
    # Lineage.authority_ceiling() routes through derive(proposed=INFERRED), which
    # would otherwise cap every non-empty list at INFERRED and block SENSOR_DERIVED.
    return Lineage(
        tool="blender-vision-mcp",
        tool_version="0.1.0",
        operation=operation,
        inputs=list(inputs or []),
        input_authorities=list(input_authorities or []),
    )
