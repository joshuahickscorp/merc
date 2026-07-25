from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any


class ConstraintType(StrEnum):
    SYMMETRY = "symmetry"
    CONCENTRICITY = "concentricity"
    EQUAL_SPACING = "equal_spacing"
    FIXED_OFFSET = "fixed_offset"
    KNOWN_DIMENSION = "known_dimension"
    COPLANARITY = "coplanarity"
    PARALLELISM = "parallelism"
    PERPENDICULARITY = "perpendicularity"
    SHARED_RADIUS = "shared_radius"
    SHARED_BEVEL = "shared_bevel"
    ARRAY_BOUNDARY = "array_boundary"
    CROSS_VIEW_ALIGNMENT = "cross_view_alignment"
    SURFACE_ATTACHMENT = "surface_attachment"


@dataclass(slots=True)
class Constraint:
    id: str
    type: ConstraintType
    subjects: list[str]
    parameters: dict[str, Any] = field(default_factory=dict)
    evidence_bindings: list[str] = field(default_factory=list)
    weight: float = 1.0
    enabled: bool = True

    def __post_init__(self) -> None:
        if not self.subjects:
            raise ValueError("constraint requires at least one subject")
        if self.weight < 0:
            raise ValueError("constraint weight cannot be negative")

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["type"] = self.type.value
        return value
