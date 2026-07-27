"""Sensor registry (Bible section 5.1).

Every stream binds a sensor descriptor before frames are emitted. The registry
is process-local; it does not invent hardware that has not been registered.
"""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import utc_now
from blender_vision.v2.authority import AuthorityClass


class SourceType(StrEnum):
    VIDEO_FILE = "video_file"
    IMAGE_SEQUENCE = "image_sequence"
    SCREEN_CAPTURE = "screen_capture"
    BLENDER_RENDER = "blender_render"
    WEBCAM = "webcam"


class RightsState(StrEnum):
    UNREVIEWED = "unreviewed"
    OWNED = "owned"
    LICENSED = "licensed"
    PROHIBITED = "prohibited"
    SYNTHETIC = "synthetic"


class PrivacyState(StrEnum):
    UNKNOWN = "unknown"
    CLEARED = "cleared"
    CONTAINS_PII = "contains_pii"
    MASKED = "masked"
    SYNTHETIC = "synthetic"


class TimestampDomain(StrEnum):
    MONOTONIC = "monotonic"
    WALL_UTC = "wall_utc"
    MEDIA_PTS = "media_pts"
    FRAME_INDEX = "frame_index"


@dataclass(slots=True)
class SensorDescriptor:
    """Bible 5.1 sensor identity. One record per physical or synthetic source."""

    sensor_id: str
    source_type: SourceType
    hardware: str = ""
    colour_profile: str = "srgb"
    lens: str = "unknown"
    resolution: list[int] = field(default_factory=lambda: [0, 0])
    frame_rate: float = 0.0
    timestamp_domain: TimestampDomain = TimestampDomain.MONOTONIC
    rights_state: RightsState = RightsState.UNREVIEWED
    privacy_state: PrivacyState = PrivacyState.UNKNOWN
    last_calibration: str = ""
    known_limitations: list[str] = field(default_factory=list)
    authority: AuthorityClass = AuthorityClass.UNRESOLVED
    registered_at: str = field(default_factory=utc_now)
    metadata: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["source_type"] = self.source_type.value
        value["timestamp_domain"] = self.timestamp_domain.value
        value["rights_state"] = self.rights_state.value
        value["privacy_state"] = self.privacy_state.value
        value["authority"] = self.authority.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> SensorDescriptor:
        return cls(
            sensor_id=str(payload["sensor_id"]),
            source_type=SourceType(payload["source_type"]),
            hardware=str(payload.get("hardware", "")),
            colour_profile=str(payload.get("colour_profile", "srgb")),
            lens=str(payload.get("lens", "unknown")),
            resolution=[int(v) for v in payload.get("resolution", [0, 0])],
            frame_rate=float(payload.get("frame_rate", 0.0)),
            timestamp_domain=TimestampDomain(payload.get("timestamp_domain", "monotonic")),
            rights_state=RightsState(payload.get("rights_state", "unreviewed")),
            privacy_state=PrivacyState(payload.get("privacy_state", "unknown")),
            last_calibration=str(payload.get("last_calibration", "")),
            known_limitations=list(payload.get("known_limitations", [])),
            authority=AuthorityClass(payload.get("authority", "UNRESOLVED")),
            registered_at=str(payload.get("registered_at", utc_now())),
            metadata=dict(payload.get("metadata", {})),
        )


class SensorRegistry:
    """Process-local sensor book. Lookups fail closed when the id is unknown."""

    def __init__(self) -> None:
        self._sensors: dict[str, SensorDescriptor] = {}

    def register(self, sensor: SensorDescriptor) -> SensorDescriptor:
        if not sensor.sensor_id:
            raise ValidationError("sensor_id is required")
        if sensor.sensor_id in self._sensors:
            raise ValidationError(f"sensor {sensor.sensor_id!r} is already registered")
        self._sensors[sensor.sensor_id] = sensor
        return sensor

    def get(self, sensor_id: str) -> SensorDescriptor:
        if sensor_id not in self._sensors:
            raise ValidationError(f"unknown sensor_id: {sensor_id!r}")
        return self._sensors[sensor_id]

    def update_calibration(self, sensor_id: str, calibration_id: str) -> SensorDescriptor:
        sensor = self.get(sensor_id)
        sensor.last_calibration = calibration_id
        return sensor

    def list(self) -> list[SensorDescriptor]:
        return [self._sensors[key] for key in sorted(self._sensors)]

    def remove(self, sensor_id: str) -> None:
        self._sensors.pop(sensor_id, None)

    def clear(self) -> None:
        self._sensors.clear()


# Module-level default registry used by stream open helpers.
DEFAULT_REGISTRY = SensorRegistry()


def register_sensor(
    sensor: SensorDescriptor, *, registry: SensorRegistry | None = None
) -> SensorDescriptor:
    return (registry or DEFAULT_REGISTRY).register(sensor)


def get_sensor(
    sensor_id: str, *, registry: SensorRegistry | None = None
) -> SensorDescriptor:
    return (registry or DEFAULT_REGISTRY).get(sensor_id)
