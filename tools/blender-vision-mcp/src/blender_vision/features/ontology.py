from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.models import EvidenceClass


class FeatureType(StrEnum):
    USB_A = "USB-A"
    USB_C = "USB-C"
    HDMI = "HDMI"
    DISPLAYPORT = "DisplayPort"
    ETHERNET = "Ethernet"
    AUDIO_JACK = "audio jack"
    POWER_CONNECTOR = "power connector"
    BUTTON = "button"
    LED = "LED"
    FAN_HUB = "fan hub"
    FAN_RING = "fan ring"
    FAN_BLADE = "fan blade"
    VENT = "vent"
    GRILLE = "grille"
    HOLE = "hole"
    SCREW = "screw"
    SEAM = "seam"
    PANEL = "panel"
    FOOT = "foot"
    LOGO = "logo"
    IO_BRACKET = "I/O bracket"
    HEAT_SINK_FIN = "heat-sink fin"
    PCB_EDGE = "PCB edge"
    CABLE_OPENING = "cable opening"
    SD_CARD_SLOT = "SD card slot"


@dataclass(slots=True)
class TechnicalFeature:
    id: str
    type: FeatureType
    parent_component: str | None
    coordinate_frame: str
    observations: list[dict[str, Any]]
    reference_ids: list[str]
    confidence: float
    evidence_class: EvidenceClass
    uncertainty: dict[str, Any] = field(default_factory=dict)
    dimensions: dict[str, Any] = field(default_factory=dict)
    model_revision: str | None = None
    human_approval: bool = False
    coverage_group: str | None = None
    hero_surface: bool = False
    provenance: list[dict[str, Any]] = field(default_factory=list)
    approval: dict[str, Any] = field(
        default_factory=lambda: {"state": "pending", "reviewer": None, "reason": None}
    )
    lifecycle_state: str = "active"
    superseded_by: str | None = None

    def __post_init__(self) -> None:
        if not 0.0 <= self.confidence <= 1.0:
            raise ValueError("feature confidence must be between zero and one")
        if not self.coordinate_frame:
            raise ValueError("feature coordinate frame is required")
        if self.human_approval and self.approval.get("state") != "approved":
            raise ValueError("approved features require an approved review record")
        if self.lifecycle_state not in {"active", "superseded", "rejected"}:
            raise ValueError("invalid feature lifecycle state")

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["type"] = self.type.value
        value["evidence_class"] = self.evidence_class.value
        return value
