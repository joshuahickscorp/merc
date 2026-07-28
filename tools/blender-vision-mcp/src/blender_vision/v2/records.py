"""The ten canonical VisionMCP V2 records (Bible section 19).

Every record is content-addressed over its own canonical JSON, carries an
authority class, a coordinate frame where spatial, an uncertainty budget, and an
explicit lineage. `digest` is computed over the payload with `digest` and
`signature` removed, so a record can always verify itself.
"""

from __future__ import annotations

import hashlib
from dataclasses import asdict, dataclass, field, fields, is_dataclass
from enum import StrEnum
from typing import Any, ClassVar, TypeVar

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import canonical_json, utc_now
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    VisibilityState,
    assert_no_promotion,
    derive,
)

SCHEMA_VERSION = "2"

T = TypeVar("T", bound="V2Record")


class TamperError(ValidationError):
    """Raised when a record's stored digest does not match its payload."""


class Lifecycle(StrEnum):
    DRAFT = "DRAFT"
    CANDIDATE = "CANDIDATE"
    REJECTED = "REJECTED"
    ACCEPTED = "ACCEPTED"
    PROMOTED = "PROMOTED"
    SUPERSEDED = "SUPERSEDED"


@dataclass(slots=True)
class Lineage:
    """Where a record came from, in enough detail to re-run it."""

    tool: str = "blender-vision-mcp"
    tool_version: str = "0.1.0"
    operation: str = "unspecified"
    inputs: list[str] = field(default_factory=list)
    input_authorities: list[str] = field(default_factory=list)
    parameters: dict[str, Any] = field(default_factory=dict)
    environment: dict[str, Any] = field(default_factory=dict)
    hardware: dict[str, Any] = field(default_factory=dict)
    model_versions: dict[str, str] = field(default_factory=dict)
    retrieved_at: str = ""
    rights_state: str = "unreviewed"
    limitations: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Lineage:
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in payload.items() if key in known})

    def authority_ceiling(self) -> AuthorityClass:
        return derive(self.input_authorities or [])


def _unpack(value: Any) -> Any:
    if isinstance(value, StrEnum):
        return value.value
    if is_dataclass(value) and not isinstance(value, type):
        if hasattr(value, "to_dict"):
            return value.to_dict()
        return {key: _unpack(item) for key, item in asdict(value).items()}
    if isinstance(value, dict):
        return {key: _unpack(item) for key, item in value.items()}
    if isinstance(value, list | tuple):
        return [_unpack(item) for item in value]
    return value


@dataclass(slots=True, kw_only=True)
class V2Record:
    """Base for every canonical record.

    Subclasses declare `RECORD_KIND` and their own payload fields.
    """

    RECORD_KIND: ClassVar[str] = "v2.record"

    id: str
    schema_version: str = SCHEMA_VERSION
    created_at: str = field(default_factory=utc_now)
    authority: AuthorityClass = AuthorityClass.HYPOTHETICAL
    lifecycle: Lifecycle = Lifecycle.DRAFT
    lineage: Lineage = field(default_factory=Lineage)
    uncertainty: Uncertainty = field(default_factory=Uncertainty)
    supersedes: list[str] = field(default_factory=list)
    superseded_by: str | None = None
    notes: list[str] = field(default_factory=list)
    digest: str = ""

    # ---------------------------------------------------------------- payload

    def payload(self) -> dict[str, Any]:
        """Canonical dictionary form, with `digest` excluded."""
        value: dict[str, Any] = {"record_kind": self.RECORD_KIND}
        for item in fields(self):
            if item.name == "digest":
                continue
            value[item.name] = _unpack(getattr(self, item.name))
        return value

    def compute_digest(self) -> str:
        return hashlib.sha256(canonical_json(self.payload())).hexdigest()

    def seal(self: T) -> T:
        """Stamp the record with its content digest. Idempotent."""
        self._enforce_authority_ceiling()
        self.digest = self.compute_digest()
        return self

    def verify(self) -> None:
        if not self.digest:
            raise TamperError(f"{self.RECORD_KIND} {self.id} is unsealed")
        if self.digest != self.compute_digest():
            raise TamperError(f"{self.RECORD_KIND} {self.id} failed digest verification")

    def to_dict(self) -> dict[str, Any]:
        value = self.payload()
        value["digest"] = self.digest or self.compute_digest()
        return value

    # -------------------------------------------------------------- authority

    def _enforce_authority_ceiling(self) -> None:
        ceiling = self.lineage.authority_ceiling()
        if not self.lineage.input_authorities:
            return
        from blender_vision.v2.authority import strength

        if strength(self.authority) > strength(ceiling):
            raise ValidationError(
                f"{self.RECORD_KIND} {self.id} claims {self.authority.value} but its "
                f"inputs only support {ceiling.value}"
            )

    def promote(
        self: T,
        target: AuthorityClass,
        *,
        reviewer: str,
        reason: str,
    ) -> T:
        self.authority = assert_no_promotion(
            self.authority, target, reviewer=reviewer, reason=reason
        )
        self.notes.append(f"authority -> {self.authority.value} by {reviewer}: {reason}")
        self.digest = ""
        return self.seal()

    def supersede(self: T, replacement_id: str) -> T:
        self.superseded_by = replacement_id
        self.lifecycle = Lifecycle.SUPERSEDED
        self.digest = ""
        return self.seal()

    # ------------------------------------------------------------ (de)serial

    @classmethod
    def from_dict(cls: type[T], payload: dict[str, Any]) -> T:
        return _base_from_dict(cls, payload)


def _base_from_dict(cls: type[T], payload: dict[str, Any]) -> T:
    """Shared record constructor.

    A module function rather than `super().from_dict(...)`: `slots=True`
    dataclasses are rebuilt as new classes, which invalidates the `__class__`
    cell that zero-argument `super()` relies on.
    """
    kind = payload.get("record_kind")
    if kind is not None and kind != cls.RECORD_KIND:
        raise ValidationError(f"expected {cls.RECORD_KIND}, got {kind}")
    kwargs: dict[str, Any] = {}
    for item in fields(cls):
        if item.name not in payload:
            continue
        kwargs[item.name] = _rebuild(item.type, payload[item.name])
    return cls(**kwargs)


_REBUILDERS: dict[str, Any] = {
    "AuthorityClass": AuthorityClass,
    "Lifecycle": Lifecycle,
    "Units": Units,
    "VisibilityState": VisibilityState,
    "Lineage": lambda value: Lineage.from_dict(value),
    "Uncertainty": lambda value: Uncertainty.from_dict(value),
    "CoordinateFrame": lambda value: CoordinateFrame.from_dict(value),
}


def _rebuild(annotation: Any, value: Any) -> Any:
    if value is None:
        return None
    text = annotation if isinstance(annotation, str) else getattr(annotation, "__name__", "")
    for name, builder in _REBUILDERS.items():
        if text.startswith(name):
            return builder(value)
    return value


# --------------------------------------------------------------------------
# 19.1 ObservationBundle
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class ObservationBundle(V2Record):
    """Binds all raw evidence for a target."""

    RECORD_KIND: ClassVar[str] = "v2.observation-bundle"

    target_id: str = ""
    sensors: list[dict[str, Any]] = field(default_factory=list)
    artifacts: list[str] = field(default_factory=list)
    modalities: list[str] = field(default_factory=list)
    capture_environment: dict[str, Any] = field(default_factory=dict)
    coverage: dict[str, Any] = field(default_factory=dict)
    rights: dict[str, Any] = field(default_factory=dict)


# --------------------------------------------------------------------------
# 19.2 SceneEvidenceGraph
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class SceneEvidenceGraph(V2Record):
    """Unifies object, camera, geometry, material, light, visibility, uncertainty."""

    RECORD_KIND: ClassVar[str] = "v2.scene-evidence-graph"

    frame: CoordinateFrame = field(default_factory=lambda: CoordinateFrame(name="scene"))
    nodes: list[dict[str, Any]] = field(default_factory=list)
    edges: list[dict[str, Any]] = field(default_factory=list)
    cameras: list[dict[str, Any]] = field(default_factory=list)
    lights: list[dict[str, Any]] = field(default_factory=list)
    materials: list[dict[str, Any]] = field(default_factory=list)
    semantic_parts: list[dict[str, Any]] = field(default_factory=list)
    visibility: dict[str, str] = field(default_factory=dict)
    observation_bundles: list[str] = field(default_factory=list)

    def visibility_violations(self) -> list[str]:
        """Node ids claiming more authority than their visibility permits."""
        from blender_vision.v2.authority import strength, visibility_authority_ceiling

        violations: list[str] = []
        by_id = {str(node.get("id")): node for node in self.nodes}
        for node_id, state in self.visibility.items():
            node = by_id.get(node_id)
            if node is None:
                continue
            claimed = AuthorityClass(node.get("authority", AuthorityClass.INFERRED))
            if strength(claimed) > strength(visibility_authority_ceiling(state)):
                violations.append(node_id)
        return sorted(violations)


# --------------------------------------------------------------------------
# 19.3 ReconstructionPortfolio
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class ReconstructionCandidate:
    """One competing hypothesis about geometry. Bible section 8.1."""

    candidate_id: str
    backend: str
    inputs: list[str] = field(default_factory=list)
    frame: CoordinateFrame = field(default_factory=lambda: CoordinateFrame(name="candidate"))
    scale_state: str = "unresolved"
    scale_authority: AuthorityClass = AuthorityClass.UNRESOLVED
    coverage: dict[str, Any] = field(default_factory=dict)
    topology_state: dict[str, Any] = field(default_factory=dict)
    editability: str = "unknown"
    visual_score: float | None = None
    dimensional_score: float | None = None
    hidden_surface_assumptions: list[str] = field(default_factory=list)
    material_state: str = "none"
    licensing: str = "unreviewed"
    runtime_cost: dict[str, Any] = field(default_factory=dict)
    failure_modes: list[str] = field(default_factory=list)
    authority: AuthorityClass = AuthorityClass.MODEL_DERIVED
    artifacts: dict[str, str] = field(default_factory=dict)
    executed: bool = False
    execution_log: str = ""

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["frame"] = self.frame.to_dict()
        value["scale_authority"] = self.scale_authority.value
        value["authority"] = self.authority.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ReconstructionCandidate:
        data = dict(payload)
        data["frame"] = CoordinateFrame.from_dict(data.get("frame", {"name": "candidate"}))
        data["scale_authority"] = AuthorityClass(data.get("scale_authority", "UNRESOLVED"))
        data["authority"] = AuthorityClass(data.get("authority", "MODEL_DERIVED"))
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})


@dataclass(slots=True, kw_only=True)
class ReconstructionPortfolio(V2Record):
    """Competing geometry/world hypotheses. Never a single confident guess."""

    RECORD_KIND: ClassVar[str] = "v2.reconstruction-portfolio"

    target_id: str = ""
    candidates: list[ReconstructionCandidate] = field(default_factory=list)
    comparison: dict[str, Any] = field(default_factory=dict)
    fusion: list[dict[str, Any]] = field(default_factory=list)
    selected_candidate_id: str | None = None
    hidden_surface_ledger: list[dict[str, Any]] = field(default_factory=list)

    def executed_candidates(self) -> list[ReconstructionCandidate]:
        return [item for item in self.candidates if item.executed]

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> ReconstructionPortfolio:
        record = _base_from_dict(cls, payload)
        record.candidates = [
            ReconstructionCandidate.from_dict(item) for item in payload.get("candidates", [])
        ]
        return record


# --------------------------------------------------------------------------
# 19.4 MaterialHypothesisSet
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class MaterialHypothesis:
    """Bible section 10.1."""

    hypothesis_id: str
    label: str = ""
    base_colour: list[float] = field(default_factory=lambda: [0.5, 0.5, 0.5])
    roughness: float = 0.5
    metalness: float = 0.0
    specular_ior: float = 1.45
    anisotropy: float = 0.0
    clearcoat: float = 0.0
    clearcoat_roughness: float = 0.03
    transmission: float = 0.0
    subsurface: float = 0.0
    subsurface_radius: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])
    emission: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])
    normal_bands: dict[str, float] = field(default_factory=dict)
    displacement_bands: dict[str, float] = field(default_factory=dict)
    occlusion: float = 1.0
    texture_scale_m: float = 0.01
    texture_coordinate_hypothesis: str = "uv"
    confidence: float = 0.0
    evidence_views: list[str] = field(default_factory=list)
    authority: AuthorityClass = AuthorityClass.INFERRED
    renderer_parity: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["authority"] = self.authority.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> MaterialHypothesis:
        data = dict(payload)
        data["authority"] = AuthorityClass(data.get("authority", "INFERRED"))
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})


@dataclass(slots=True, kw_only=True)
class MaterialHypothesisSet(V2Record):
    RECORD_KIND: ClassVar[str] = "v2.material-hypothesis-set"

    surface_id: str = ""
    hypotheses: list[MaterialHypothesis] = field(default_factory=list)
    selected_hypothesis_id: str | None = None
    frequency_separation: dict[str, Any] = field(default_factory=dict)
    critic_findings: list[dict[str, Any]] = field(default_factory=list)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> MaterialHypothesisSet:
        record = _base_from_dict(cls, payload)
        record.hypotheses = [
            MaterialHypothesis.from_dict(item) for item in payload.get("hypotheses", [])
        ]
        return record


# --------------------------------------------------------------------------
# 19.5 LightingHypothesisSet
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class LightingHypothesis:
    hypothesis_id: str
    rig_class: str = "neutral_documentation"
    key: dict[str, Any] = field(default_factory=dict)
    fill: dict[str, Any] = field(default_factory=dict)
    negative_fill: dict[str, Any] = field(default_factory=dict)
    rim: dict[str, Any] = field(default_factory=dict)
    environment: dict[str, Any] = field(default_factory=dict)
    reflection_cards: list[dict[str, Any]] = field(default_factory=list)
    shadow_softness: float = 0.25
    contact_shadow: dict[str, Any] = field(default_factory=dict)
    exposure: float = 0.0
    white_balance_k: float = 6500.0
    tone_map: str = "AgX"
    bloom: float = 0.0
    atmosphere: dict[str, Any] = field(default_factory=dict)
    depth_falloff: float = 0.0
    confidence: float = 0.0
    authority: AuthorityClass = AuthorityClass.INFERRED

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["authority"] = self.authority.value
        return value

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> LightingHypothesis:
        data = dict(payload)
        data["authority"] = AuthorityClass(data.get("authority", "INFERRED"))
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in data.items() if key in known})


@dataclass(slots=True, kw_only=True)
class LightingHypothesisSet(V2Record):
    RECORD_KIND: ClassVar[str] = "v2.lighting-hypothesis-set"

    scene_id: str = ""
    hypotheses: list[LightingHypothesis] = field(default_factory=list)
    selected_hypothesis_id: str | None = None
    perceptual_checks: list[dict[str, Any]] = field(default_factory=list)
    joint_solve: dict[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> LightingHypothesisSet:
        record = _base_from_dict(cls, payload)
        record.hypotheses = [
            LightingHypothesis.from_dict(item) for item in payload.get("hypotheses", [])
        ]
        return record


# --------------------------------------------------------------------------
# 19.6 ProceduralSceneGraph
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class ProceduralSceneGraph(V2Record):
    """Semantic architecture and instancing rules."""

    RECORD_KIND: ClassVar[str] = "v2.procedural-scene-graph"

    scene_name: str = ""
    frame: CoordinateFrame = field(default_factory=lambda: CoordinateFrame(name="scene"))
    modules: list[dict[str, Any]] = field(default_factory=list)
    instances: list[dict[str, Any]] = field(default_factory=list)
    archetypes: list[dict[str, Any]] = field(default_factory=list)
    grammar: list[dict[str, Any]] = field(default_factory=list)
    lod_policy: dict[str, Any] = field(default_factory=dict)
    text_safe_zones: list[dict[str, Any]] = field(default_factory=list)
    bounds_m: dict[str, Any] = field(default_factory=dict)

    def instance_count(self) -> int:
        return sum(int(item.get("count", 1)) for item in self.instances)


# --------------------------------------------------------------------------
# 19.7 CameraPathGraph
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class NarrativeBeat:
    beat_id: str
    label: str
    scroll_start: float
    scroll_end: float
    text_zone: str = "centre"
    dwell: float = 0.0
    skippable: bool = True
    reduced_motion_view: dict[str, Any] = field(default_factory=dict)
    text: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> NarrativeBeat:
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in payload.items() if key in known})


@dataclass(slots=True, kw_only=True)
class CameraPathGraph(V2Record):
    """Cinematic path plus scroll mapping. Bible section 13.1."""

    RECORD_KIND: ClassVar[str] = "v2.camera-path-graph"

    frame: CoordinateFrame = field(default_factory=lambda: CoordinateFrame(name="scene"))
    control_points: list[list[float]] = field(default_factory=list)
    orientation_points: list[list[float]] = field(default_factory=list)
    focus_targets: list[dict[str, Any]] = field(default_factory=list)
    focal_length_mm: list[list[float]] = field(default_factory=list)
    exposure_curve: list[list[float]] = field(default_factory=list)
    light_state_transitions: list[dict[str, Any]] = field(default_factory=list)
    beats: list[NarrativeBeat] = field(default_factory=list)
    arc_length_m: float = 0.0
    samples: list[dict[str, Any]] = field(default_factory=list)
    damping: float = 0.12
    reduced_motion_views: list[dict[str, Any]] = field(default_factory=list)
    skip_points: list[float] = field(default_factory=list)

    def beat_coverage_gaps(self) -> list[tuple[float, float]]:
        """Scroll intervals no beat claims. A gap is dead scroll distance."""
        ordered = sorted(self.beats, key=lambda item: item.scroll_start)
        gaps: list[tuple[float, float]] = []
        cursor = 0.0
        for beat in ordered:
            if beat.scroll_start > cursor + 1e-6:
                gaps.append((cursor, beat.scroll_start))
            cursor = max(cursor, beat.scroll_end)
        if cursor < 1.0 - 1e-6:
            gaps.append((cursor, 1.0))
        return gaps

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> CameraPathGraph:
        record = _base_from_dict(cls, payload)
        record.beats = [NarrativeBeat.from_dict(item) for item in payload.get("beats", [])]
        return record


# --------------------------------------------------------------------------
# 19.8 PerceptualCritique
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class CriticFinding:
    finding_id: str
    critic_role: str
    diagnosis: str
    evidence: list[str] = field(default_factory=list)
    severity: str = "minor"
    confidence: float = 0.5
    likely_cause: str = ""
    bounded_repair: dict[str, Any] = field(default_factory=dict)
    blast_radius: list[str] = field(default_factory=list)
    acceptance_test: str = ""
    measured: dict[str, Any] = field(default_factory=dict)

    _SEVERITIES = ("info", "minor", "major", "critical")

    def __post_init__(self) -> None:
        if self.severity not in self._SEVERITIES:
            raise ValidationError(f"severity must be one of {self._SEVERITIES}")
        if not self.evidence:
            raise ValidationError(f"finding {self.finding_id} must bind evidence")

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> CriticFinding:
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in payload.items() if key in known})


@dataclass(slots=True, kw_only=True)
class PerceptualCritique(V2Record):
    RECORD_KIND: ClassVar[str] = "v2.perceptual-critique"

    subject_id: str = ""
    subject_kind: str = "scene"
    findings: list[CriticFinding] = field(default_factory=list)
    critics_run: list[str] = field(default_factory=list)
    passed: bool = False

    def blocking_findings(self) -> list[CriticFinding]:
        return [item for item in self.findings if item.severity in {"major", "critical"}]

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> PerceptualCritique:
        record = _base_from_dict(cls, payload)
        record.findings = [CriticFinding.from_dict(item) for item in payload.get("findings", [])]
        return record


# --------------------------------------------------------------------------
# 19.9 DeliveryManifest
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class DeliveryAsset:
    asset_id: str
    role: str
    path: str
    digest: str
    bytes: int
    compression: str = "none"
    lod: str = "L0"
    decode_ms: float | None = None
    main_thread_ms: float | None = None
    visual_difference: float | None = None
    chapter: str | None = None

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> DeliveryAsset:
        known = {item.name for item in fields(cls)}
        return cls(**{key: value for key, value in payload.items() if key in known})


@dataclass(slots=True, kw_only=True)
class DeliveryManifest(V2Record):
    RECORD_KIND: ClassVar[str] = "v2.delivery-manifest"

    source_scene: str = ""
    assets: list[DeliveryAsset] = field(default_factory=list)
    budgets: dict[str, Any] = field(default_factory=dict)
    measured: dict[str, Any] = field(default_factory=dict)
    budget_violations: list[dict[str, Any]] = field(default_factory=list)
    receipts: list[str] = field(default_factory=list)

    def total_bytes(self, role: str | None = None) -> int:
        return sum(item.bytes for item in self.assets if role is None or item.role == role)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> DeliveryManifest:
        record = _base_from_dict(cls, payload)
        record.assets = [DeliveryAsset.from_dict(item) for item in payload.get("assets", [])]
        return record


# --------------------------------------------------------------------------
# 19.10 NextViewRequest
# --------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class NextViewRequest(V2Record):
    """Active perception: what evidence would help most, and why."""

    RECORD_KIND: ClassVar[str] = "v2.next-view-request"

    target_id: str = ""
    missing_uncertainty: str = ""
    expected_reduction: float = 0.0
    capture_instructions: dict[str, Any] = field(default_factory=dict)
    human_instructions: str = ""
    required_calibration: list[str] = field(default_factory=list)
    acceptable_alternatives: list[str] = field(default_factory=list)
    reason: str = ""
    priority: int = 5
    satisfied_by: str | None = None
    declined: bool = False

    def __post_init__(self) -> None:
        if not 0 <= self.priority <= 10:
            raise ValidationError("priority must be within 0..10")


RECORD_TYPES: dict[str, type[V2Record]] = {
    cls.RECORD_KIND: cls
    for cls in (
        ObservationBundle,
        SceneEvidenceGraph,
        ReconstructionPortfolio,
        MaterialHypothesisSet,
        LightingHypothesisSet,
        ProceduralSceneGraph,
        CameraPathGraph,
        PerceptualCritique,
        DeliveryManifest,
        NextViewRequest,
    )
}


def load_record(payload: dict[str, Any]) -> V2Record:
    kind = payload.get("record_kind")
    if kind not in RECORD_TYPES:
        raise ValidationError(f"unknown V2 record kind: {kind!r}")
    return RECORD_TYPES[kind].from_dict(payload)
