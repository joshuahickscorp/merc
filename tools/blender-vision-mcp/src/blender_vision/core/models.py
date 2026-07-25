from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any


class EvidenceClass(StrEnum):
    MEASURED = "MEASURED"
    MANUFACTURER_SPEC = "MANUFACTURER_SPEC"
    MULTI_VIEW_OBSERVED = "MULTI_VIEW_OBSERVED"
    SINGLE_VIEW_OBSERVED = "SINGLE_VIEW_OBSERVED"
    TEARDOWN_OBSERVED = "TEARDOWN_OBSERVED"
    INFERRED_HIGH_CONFIDENCE = "INFERRED_HIGH_CONFIDENCE"
    INFERRED_LOW_CONFIDENCE = "INFERRED_LOW_CONFIDENCE"
    OCCLUDED = "OCCLUDED"
    UNSEEN = "UNSEEN"


class FidelityLevel(StrEnum):
    L0 = "L0"
    L1 = "L1"
    L2 = "L2"
    L3 = "L3"
    L4 = "L4"
    L5 = "L5"


class JobStatus(StrEnum):
    QUEUED = "queued"
    RUNNING = "running"
    SUCCEEDED = "succeeded"
    FAILED = "failed"
    CANCELLED = "cancelled"
    TIMED_OUT = "timed_out"


class BackendState(StrEnum):
    AVAILABLE = "AVAILABLE"
    UNAVAILABLE = "UNAVAILABLE"
    RESEARCH_ONLY = "RESEARCH_ONLY"
    LICENSE_REVIEW_REQUIRED = "LICENSE_REVIEW_REQUIRED"
    DOWNLOAD_REQUIRED = "DOWNLOAD_REQUIRED"
    INCOMPATIBLE_HARDWARE = "INCOMPATIBLE_HARDWARE"


class RegistrationClass(StrEnum):
    BODY_BOUNDING_BOX = "body_bounding_box_alignment"
    APPROXIMATE_VISUAL = "approximate_visual_registration"
    FEATURE_BASED = "feature_based_camera_solution"
    METRIC = "metric_camera_solution"


class SceneLifecycleState(StrEnum):
    DRAFT = "DRAFT"
    CANDIDATE = "CANDIDATE"
    REJECTED = "REJECTED"
    ACCEPTED = "ACCEPTED"
    PROMOTED = "PROMOTED"
    SUPERSEDED = "SUPERSEDED"


@dataclass(slots=True)
class BackendCapability:
    name: str
    version: str
    revision: str
    license: str
    commercial_use: bool
    redistribution: str
    state: BackendState
    outputs: list[str]
    hardware: list[str]
    quality_tier: str
    weight_hash: str | None = None
    vram_gb: float | None = None
    download_source: str | None = None
    precision: list[str] = field(default_factory=list)
    input_limits: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["state"] = self.state.value
        return value


@dataclass(slots=True)
class ArtifactRecord:
    digest: str
    size: int
    media_type: str
    path: str
    created_at: str
    source_name: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class CameraSolution:
    reference_id: str
    model: str
    width: int
    height: int
    intrinsics: dict[str, float]
    world_from_camera: list[list[float]]
    confidence: float
    registration_class: str
    evidence_class: EvidenceClass
    diagnostics: dict[str, Any] = field(default_factory=dict)
    distortion_model: dict[str, Any] | None = None
    sensor_model: dict[str, Any] | None = None
    crop: dict[str, Any] | None = None
    clipping: dict[str, float] | None = None
    coordinate_transform: dict[str, Any] | None = None
    camera_source_identity: dict[str, Any] | None = None
    solve_method: dict[str, Any] | None = None

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["evidence_class"] = self.evidence_class.value
        return value
