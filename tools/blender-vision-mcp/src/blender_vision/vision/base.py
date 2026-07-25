from __future__ import annotations

from dataclasses import asdict, dataclass, field
from typing import Any, Protocol


@dataclass(slots=True)
class GeometryEvidence:
    """Normalized output contract for classical and learned geometry backends."""

    camera_intrinsics: list[dict[str, Any]] = field(default_factory=list)
    camera_extrinsics: list[dict[str, Any]] = field(default_factory=list)
    depth_artifacts: list[str] = field(default_factory=list)
    point_artifacts: list[str] = field(default_factory=list)
    normal_artifacts: list[str] = field(default_factory=list)
    correspondence_artifacts: list[str] = field(default_factory=list)
    visibility_artifacts: list[str] = field(default_factory=list)
    confidence_artifacts: list[str] = field(default_factory=list)
    mask_artifacts: list[str] = field(default_factory=list)
    occupancy_artifacts: list[str] = field(default_factory=list)
    silhouette_volume_artifacts: list[str] = field(default_factory=list)
    visual_hull_artifacts: list[str] = field(default_factory=list)
    diagnostics: dict[str, Any] = field(default_factory=dict)
    source_frame: str = "backend"
    transform_to_canonical: list[list[float]] | None = None
    scale_factor: float | None = None
    uncertainty: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


class GeometryBackend(Protocol):
    name: str
    version: str

    def solve(self, reference_ids: list[str], configuration: dict[str, Any]) -> GeometryEvidence:
        """Return evidence without silently asserting metric scale."""
        ...
