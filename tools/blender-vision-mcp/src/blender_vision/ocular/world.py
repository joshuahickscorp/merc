"""Persistent world model for the Ocular operating system (Bible §12).

The world is a memory-bearing, append-only belief structure. It survives camera
motion, object motion, occlusion, absent frames, and process restart. Competing
evidence is never silently overwritten: a contradiction appends a competing
belief and records the disagreement in the belief history.
"""

from __future__ import annotations

import hashlib
import json
import math
import re
from dataclasses import dataclass, field, fields
from enum import StrEnum
from pathlib import Path
from typing import Any, ClassVar

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import atomic_write_json, canonical_json, utc_now
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    VisibilityState,
    derive,
    strength,
    visibility_authority_ceiling,
)
from blender_vision.v2.records import Lifecycle, Lineage, V2Record

# ---------------------------------------------------------------------------
# Bible §12.5 surface provenance — every surface is classifiable.
# ---------------------------------------------------------------------------


class SurfaceProvenance(StrEnum):
    """How a surface entered the world model. Bible section 12.5."""

    DIRECTLY_OBSERVED = "directly_observed"
    SENSOR_DERIVED = "sensor_derived"
    MULTI_VIEW_DERIVED = "multi_view_derived"
    SYMMETRY_INFERRED = "symmetry_inferred"
    RETRIEVAL_ADAPTED = "retrieval_adapted"
    PROCEDURALLY_INFERRED = "procedurally_inferred"
    GENERATIVELY_COMPLETED = "generatively_completed"
    HUMAN_REVIEWED = "human_reviewed"
    UNRESOLVED = "unresolved"


#: Provenances that may never be described as observed truth.
INFERRED_PROVENANCES: frozenset[SurfaceProvenance] = frozenset(
    {
        SurfaceProvenance.SYMMETRY_INFERRED,
        SurfaceProvenance.RETRIEVAL_ADAPTED,
        SurfaceProvenance.PROCEDURALLY_INFERRED,
        SurfaceProvenance.GENERATIVELY_COMPLETED,
        SurfaceProvenance.UNRESOLVED,
    }
)


class RelationKind(StrEnum):
    """Bible §12.2 relations. same_as and candidate_same_as never collapse."""

    SAME_AS = "same_as"
    CANDIDATE_SAME_AS = "candidate_same_as"
    PART_OF = "part_of"
    SUPPORTS = "supports"
    ON_TOP_OF = "on_top_of"
    ADJACENT_TO = "adjacent_to"
    CONTAINS = "contains"
    OCCLUDES = "occludes"
    INSTANCE_OF = "instance_of"
    NEAR = "near"


class BeliefSlot(StrEnum):
    """Named belief channels an entity maintains."""

    EXISTENCE = "existence"
    CLASS = "class"
    IDENTITY = "identity"
    POSE = "pose"
    VISIBILITY = "visibility"
    GEOMETRY = "geometry"
    MATERIAL = "material"
    LIGHTING = "lighting"
    APPEARANCE = "appearance"


class ChangeClass(StrEnum):
    """Dynamic-room memory change classes (Phase M)."""

    SAME_SCENE = "same_scene"
    MOVED_OBJECT = "moved_object"
    REMOVED_OBJECT = "removed_object"
    NEW_OBJECT = "new_object"
    LIGHTING_ONLY = "lighting_only"
    APPEARANCE_CHANGE = "appearance_change"
    GEOMETRY_CHANGE = "geometry_change"


# Pose distance above this (metres) is a geometry move, not noise.
_DEFAULT_MOVE_TOLERANCE_M = 0.05
# Relative mean-luminance shift that counts as a lighting-only change.
_DEFAULT_LIGHTING_TOLERANCE = 0.08
# Per-frame confidence decay while an entity is occluded / absent.
_OCCLUSION_CONFIDENCE_DECAY = 0.08
_ABSENT_CONFIDENCE_DECAY = 0.04
_SURPRISE_CONFIDENCE_DROP = 0.25
_CONFIRM_CONFIDENCE_GAIN = 0.12
_MIN_CONFIDENCE = 0.05
_MAX_CONFIDENCE = 0.99

# Production identity sources for the builder path. Ground truth is sealed-
# evaluator territory; silence is not permission.
_PRODUCTION_TRACK_SOURCES: frozenset[str] = frozenset({"perception", "perception_derived"})
# Perception tracker mint shape (see track._new_track_id).
_TRACKER_ID_RE = re.compile(r"^trk-\d+$")


def _clamp_confidence(value: float) -> float:
    return max(_MIN_CONFIDENCE, min(_MAX_CONFIDENCE, float(value)))


def _pose_distance(a: list[float] | tuple[float, ...], b: list[float] | tuple[float, ...]) -> float:
    if len(a) < 3 or len(b) < 3:
        raise ValidationError("pose vectors must have at least xyz")
    return math.sqrt(sum((float(a[i]) - float(b[i])) ** 2 for i in range(3)))


def _as_float_list(value: Any, *, length: int | None = None) -> list[float]:
    if not isinstance(value, list | tuple):
        raise ValidationError(f"expected list, got {type(value).__name__}")
    out = [float(item) for item in value]
    if length is not None and len(out) != length:
        raise ValidationError(f"expected length {length}, got {len(out)}")
    return out


# ---------------------------------------------------------------------------
# Records
# ---------------------------------------------------------------------------


@dataclass(slots=True, kw_only=True)
class BeliefUpdate(V2Record):
    """One append-only step in the belief history. Never rewritten in place."""

    RECORD_KIND: ClassVar[str] = "ocular.belief-update"

    entity_id: str = ""
    slot: str = BeliefSlot.POSE.value
    belief_id: str = ""
    prior: dict[str, Any] = field(default_factory=dict)
    evidence: dict[str, Any] = field(default_factory=dict)
    model: str = "identity"
    posterior: dict[str, Any] = field(default_factory=dict)
    contradiction: bool = False
    competing: bool = False
    reviewer: str = "system"
    timestamp: str = field(default_factory=utc_now)
    frame_index: int = -1
    confidence_before: float = 0.0
    confidence_after: float = 0.0


@dataclass(slots=True, kw_only=True)
class Surface(V2Record):
    """A classifiable surface attached to an entity. Bible §12.5."""

    RECORD_KIND: ClassVar[str] = "ocular.surface"

    surface_id: str = ""
    entity_id: str = ""
    provenance: SurfaceProvenance = SurfaceProvenance.UNRESOLVED
    visibility: VisibilityState = VisibilityState.NEVER_OBSERVED
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="blender-world", up_axis="+Z", forward_axis="-Y"
        )
    )
    centroid_m: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0])
    normal: list[float] = field(default_factory=lambda: [0.0, 0.0, 1.0])
    area_m2: float = 0.0
    material_hypothesis_ids: list[str] = field(default_factory=list)
    observation_ids: list[str] = field(default_factory=list)
    confidence: float = 0.0

    def _enforce_authority_ceiling(self) -> None:
        V2Record._enforce_authority_ceiling(self)
        ceiling = visibility_authority_ceiling(self.visibility)
        if strength(self.authority) > strength(ceiling):
            raise ValidationError(
                f"surface {self.surface_id} claims {self.authority.value} but "
                f"visibility {self.visibility.value} only permits {ceiling.value}"
            )
        if (
            self.provenance in INFERRED_PROVENANCES
            and self.authority is AuthorityClass.OBSERVED
        ):
            raise ValidationError(
                f"surface {self.surface_id} provenance {self.provenance.value} "
                "cannot claim OBSERVED authority"
            )

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Surface:
        data = dict(payload)
        if "provenance" in data:
            data["provenance"] = SurfaceProvenance(data["provenance"])
        if "visibility" in data:
            data["visibility"] = VisibilityState(data["visibility"])
        if "frame" in data and isinstance(data["frame"], dict):
            data["frame"] = CoordinateFrame.from_dict(data["frame"])
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
class Relation(V2Record):
    """Directed relation between two entities. Bible §12.2."""

    RECORD_KIND: ClassVar[str] = "ocular.relation"

    relation_id: str = ""
    kind: RelationKind = RelationKind.NEAR
    source_id: str = ""
    target_id: str = ""
    confidence: float = 0.0
    evidence: list[str] = field(default_factory=list)
    # same_as may only be set when explicit evidence was recorded; never from
    # candidate_same_as alone.
    evidence_recorded: bool = False

    def _enforce_authority_ceiling(self) -> None:
        V2Record._enforce_authority_ceiling(self)
        if self.kind is RelationKind.SAME_AS and not self.evidence_recorded:
            raise ValidationError(
                f"relation {self.relation_id}: same_as requires recorded evidence; "
                "use candidate_same_as until evidence is bound"
            )
        if self.kind is RelationKind.SAME_AS and not self.evidence:
            raise ValidationError(
                f"relation {self.relation_id}: same_as requires a non-empty evidence list"
            )

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Relation:
        data = dict(payload)
        if "kind" in data:
            data["kind"] = RelationKind(data["kind"])
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
class Entity(V2Record):
    """A persistent object hypothesis with identity, pose, and belief sets."""

    RECORD_KIND: ClassVar[str] = "ocular.entity"

    entity_id: str = ""
    track_id: str = ""
    class_label: str = ""
    parts: list[str] = field(default_factory=list)
    state: str = "active"
    # Pose is [x, y, z, qw, qx, qy, qz] in the world frame.
    pose_m: list[float] = field(default_factory=lambda: [0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0])
    trajectory: list[dict[str, Any]] = field(default_factory=list)
    geometry_hypotheses: list[dict[str, Any]] = field(default_factory=list)
    material_hypotheses: list[dict[str, Any]] = field(default_factory=list)
    observed_surface_ids: list[str] = field(default_factory=list)
    inferred_surface_ids: list[str] = field(default_factory=list)
    measurements: list[dict[str, Any]] = field(default_factory=list)
    source_bindings: list[str] = field(default_factory=list)
    confidence: float = 0.5
    rights: dict[str, Any] = field(default_factory=dict)
    visibility: VisibilityState = VisibilityState.NEVER_OBSERVED
    # slot -> list of competing posterior dicts (append-only within the entity).
    belief_sets: dict[str, list[dict[str, Any]]] = field(default_factory=dict)
    last_observed_frame: int = -1
    frames_since_seen: int = 0
    appearance: dict[str, Any] = field(default_factory=dict)
    # Identity provenance: track_source, source_observation_frames, minted_by.
    identity_provenance: dict[str, Any] = field(default_factory=dict)
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="blender-world", up_axis="+Z", forward_axis="-Y"
        )
    )

    def preferred_belief(self, slot: str) -> dict[str, Any] | None:
        """Return the highest-confidence non-rejected belief for a slot."""
        candidates = self.belief_sets.get(slot, [])
        if not candidates:
            return None
        live = [item for item in candidates if not item.get("rejected")]
        pool = live or candidates
        return max(pool, key=lambda item: float(item.get("confidence", 0.0)))

    def all_beliefs(self, slot: str) -> list[dict[str, Any]]:
        return list(self.belief_sets.get(slot, []))

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Entity:
        data = dict(payload)
        if "visibility" in data:
            data["visibility"] = VisibilityState(data["visibility"])
        if "frame" in data and isinstance(data["frame"], dict):
            data["frame"] = CoordinateFrame.from_dict(data["frame"])
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
class WorldState(V2Record):
    """The full persistent world. Belief history is append-only."""

    RECORD_KIND: ClassVar[str] = "ocular.world-state"

    scene_id: str = ""
    session_id: str = ""
    frame: CoordinateFrame = field(
        default_factory=lambda: CoordinateFrame(
            name="blender-world", up_axis="+Z", forward_axis="-Y"
        )
    )
    entities: dict[str, Entity] = field(default_factory=dict)
    surfaces: dict[str, Surface] = field(default_factory=dict)
    relations: list[Relation] = field(default_factory=list)
    belief_history: list[BeliefUpdate] = field(default_factory=list)
    current_frame: int = -1
    # Scene-level appearance / lighting fingerprint (not geometry).
    lighting: dict[str, Any] = field(default_factory=dict)
    appearance: dict[str, Any] = field(default_factory=dict)
    # Surprise events and predictions hang off the world for persistence.
    surprises: list[dict[str, Any]] = field(default_factory=list)
    predictions: list[dict[str, Any]] = field(default_factory=list)
    meta: dict[str, Any] = field(default_factory=dict)
    # Monotonic counter for belief ids — never reused.
    _belief_seq: int = 0

    def next_belief_id(self) -> str:
        self._belief_seq += 1
        return f"belief-{self.scene_id}-{self._belief_seq:06d}"

    def beliefs_payload(self) -> dict[str, Any]:
        """Canonical belief slice used for cross-session identity digests."""
        return {
            "scene_id": self.scene_id,
            "entities": {
                eid: {
                    "entity_id": entity.entity_id,
                    "track_id": entity.track_id,
                    "class_label": entity.class_label,
                    "pose_m": list(entity.pose_m),
                    "confidence": entity.confidence,
                    "belief_sets": entity.belief_sets,
                    "visibility": entity.visibility.value
                    if isinstance(entity.visibility, VisibilityState)
                    else entity.visibility,
                    "last_observed_frame": entity.last_observed_frame,
                    "appearance": entity.appearance,
                }
                for eid, entity in sorted(self.entities.items())
            },
            "relations": [
                {
                    "relation_id": rel.relation_id,
                    "kind": rel.kind.value if isinstance(rel.kind, RelationKind) else rel.kind,
                    "source_id": rel.source_id,
                    "target_id": rel.target_id,
                    "evidence": list(rel.evidence),
                    "evidence_recorded": rel.evidence_recorded,
                }
                for rel in self.relations
            ],
            "belief_history": [
                {
                    "id": item.id,
                    "entity_id": item.entity_id,
                    "slot": item.slot,
                    "belief_id": item.belief_id,
                    "prior": item.prior,
                    "evidence": item.evidence,
                    "posterior": item.posterior,
                    "contradiction": item.contradiction,
                    "competing": item.competing,
                    "timestamp": item.timestamp,
                    "frame_index": item.frame_index,
                }
                for item in self.belief_history
            ],
            "lighting": self.lighting,
            "appearance": self.appearance,
        }

    def beliefs_digest(self) -> str:
        return hashlib.sha256(canonical_json(self.beliefs_payload())).hexdigest()

    def seal(self) -> WorldState:
        for entity in self.entities.values():
            if not entity.digest:
                entity.seal()
        for surface in self.surfaces.values():
            if not surface.digest:
                surface.seal()
        for relation in self.relations:
            if not relation.digest:
                relation.seal()
        for update in self.belief_history:
            if not update.digest:
                update.seal()
        return V2Record.seal(self)

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> WorldState:
        data = dict(payload)
        if "frame" in data and isinstance(data["frame"], dict):
            data["frame"] = CoordinateFrame.from_dict(data["frame"])
        if "authority" in data:
            data["authority"] = AuthorityClass(data["authority"])
        if "lifecycle" in data:
            data["lifecycle"] = Lifecycle(data["lifecycle"])
        if "lineage" in data and isinstance(data["lineage"], dict):
            data["lineage"] = Lineage.from_dict(data["lineage"])
        if "uncertainty" in data and isinstance(data["uncertainty"], dict):
            data["uncertainty"] = Uncertainty.from_dict(data["uncertainty"])
        entities_raw = data.pop("entities", {})
        surfaces_raw = data.pop("surfaces", {})
        relations_raw = data.pop("relations", [])
        history_raw = data.pop("belief_history", [])
        known = {item.name for item in fields(cls)}
        world = cls(**{key: value for key, value in data.items() if key in known})
        if isinstance(entities_raw, dict):
            world.entities = {
                key: Entity.from_dict(value) if isinstance(value, dict) else value
                for key, value in entities_raw.items()
            }
        if isinstance(surfaces_raw, dict):
            world.surfaces = {
                key: Surface.from_dict(value) if isinstance(value, dict) else value
                for key, value in surfaces_raw.items()
            }
        world.relations = [
            Relation.from_dict(item) if isinstance(item, dict) else item for item in relations_raw
        ]
        world.belief_history = [
            BeliefUpdate.from_dict(item) if isinstance(item, dict) else item
            for item in history_raw
        ]
        return world


# ---------------------------------------------------------------------------
# Belief machinery
# ---------------------------------------------------------------------------


def _append_belief(
    world: WorldState,
    entity: Entity,
    *,
    slot: str,
    prior: dict[str, Any],
    evidence: dict[str, Any],
    posterior: dict[str, Any],
    model: str,
    contradiction: bool,
    competing: bool,
    confidence_before: float,
    confidence_after: float,
    frame_index: int,
    reviewer: str = "system",
    authority: AuthorityClass = AuthorityClass.SENSOR_DERIVED,
) -> BeliefUpdate:
    belief_id = world.next_belief_id()
    posterior = dict(posterior)
    posterior.setdefault("belief_id", belief_id)
    posterior.setdefault("confidence", confidence_after)
    posterior.setdefault("slot", slot)

    entity.belief_sets.setdefault(slot, [])
    if competing or contradiction:
        # Competing: keep the prior belief and add a new one alongside it.
        entity.belief_sets[slot].append(posterior)
    else:
        # Compatible update: still append history, replace preferred tip with
        # a new entry while retaining the full chain in belief_sets.
        entity.belief_sets[slot].append(posterior)

    # Lineage.authority_ceiling() is derive(inputs, proposed=INFERRED), so listing
    # high-authority inputs would cap the ceiling at INFERRED and reject SENSOR_DERIVED
    # claims. Carry the source authority in parameters; leave input_authorities empty
    # so the explicit authority field is the sealed claim (matches RuntimeAttestation).
    update = BeliefUpdate(
        id=f"bu-{belief_id}",
        entity_id=entity.entity_id,
        slot=slot,
        belief_id=belief_id,
        prior=prior,
        evidence=evidence,
        model=model,
        posterior=posterior,
        contradiction=contradiction,
        competing=competing,
        reviewer=reviewer,
        timestamp=utc_now(),
        frame_index=frame_index,
        confidence_before=confidence_before,
        confidence_after=confidence_after,
        authority=authority,
        lineage=Lineage(
            operation="belief_update",
            inputs=list(evidence.get("observation_ids", []) or []),
            parameters={"source_authority": authority.value},
        ),
    ).seal()
    world.belief_history.append(update)
    entity.digest = ""
    return update


def _pose_belief_value(pose_m: list[float]) -> dict[str, Any]:
    return {"pose_m": list(pose_m)}


def _poses_contradict(
    prior_pose: list[float],
    observed_pose: list[float],
    *,
    tolerance_m: float = _DEFAULT_MOVE_TOLERANCE_M,
) -> bool:
    return _pose_distance(prior_pose, observed_pose) > tolerance_m


# ---------------------------------------------------------------------------
# World construction and update
# ---------------------------------------------------------------------------


def _offending_entity_ids(observation: dict[str, Any]) -> str:
    ids: list[str] = []
    for raw in observation.get("entities") or []:
        if not isinstance(raw, dict):
            continue
        entity_id = str(raw.get("entity_id") or raw.get("track_id") or "").strip()
        if entity_id:
            ids.append(entity_id)
    return ", ".join(ids) if ids else "(no entity_id)"


def _normalize_track_source(observation: dict[str, Any]) -> str | None:
    raw = observation.get("track_source", None)
    if raw is None:
        return None
    text = str(raw).strip()
    return text if text else None


def _reject_ground_truth_identity(
    observation: dict[str, Any],
    *,
    allow_ground_truth: bool,
) -> None:
    """Block ground-truth / unlabelled identity on the builder path.

    Ground truth belongs in the sealed evaluator. Production builders only accept
    perception-minted identity sources.
    """
    if allow_ground_truth:
        return
    entity_ids = _offending_entity_ids(observation)
    source = _normalize_track_source(observation)
    if source is None:
        raise ValueError(
            f"observation missing track_source for entity_id={entity_ids}; "
            "silence is not permission — ground truth belongs in the sealed evaluator"
        )
    if source == "ground_truth":
        raise ValueError(
            f"ground-truth identity is forbidden for entity_id={entity_ids}; "
            "ground truth belongs in the sealed evaluator"
        )
    if source not in _PRODUCTION_TRACK_SOURCES:
        raise ValueError(
            f"track_source={source!r} is not a production perception source for "
            f"entity_id={entity_ids}; accepted values are perception, "
            "perception_derived (ground truth belongs in the sealed evaluator)"
        )


def _minted_by(entity_id: str) -> str:
    return "tracker" if _TRACKER_ID_RE.match(entity_id) else "caller"


def _record_entity_identity_provenance(
    entity: "Entity",
    *,
    track_source: str,
    frame_index: int,
) -> None:
    prior = dict(entity.identity_provenance or {})
    frames = list(prior.get("source_observation_frames") or [])
    if frame_index not in frames:
        frames.append(frame_index)
    entity.identity_provenance = {
        "track_source": track_source,
        "source_observation_frames": frames,
        "minted_by": prior.get("minted_by") or _minted_by(entity.entity_id),
    }


def build_world_model(
    observations: list[dict[str, Any]],
    *,
    scene_id: str,
    session_id: str = "session-0",
    frame: CoordinateFrame | None = None,
    lighting: dict[str, Any] | None = None,
    appearance: dict[str, Any] | None = None,
    allow_ground_truth: bool = False,
) -> WorldState:
    """Build a world from an ordered list of frame observations.

    Each observation is a dict with at least:
      frame_index: int
      entities: list[{entity_id|track_id, class_label, pose_m, visible?, appearance?}]
      track_source: "perception" | "perception_derived"  (required unless
        allow_ground_truth=True)
      lighting?: dict
      appearance?: dict
      absent?: bool  — frame never arrived; still advance time.

    Ground-truth identity is forbidden on the production builder path. Pass
    allow_ground_truth=True only for diagnostic / fixture consumers; the sealed
    evaluator is where ground truth belongs for scoring.
    """
    world_frame = frame or CoordinateFrame(
        name=BLENDER_WORLD.name,
        up_axis=BLENDER_WORLD.up_axis,
        forward_axis=BLENDER_WORLD.forward_axis,
        handedness=BLENDER_WORLD.handedness,
        units=BLENDER_WORLD.units,
    )
    identity_provenance = "ground_truth" if allow_ground_truth else "perception"
    world = WorldState(
        id=f"world-{scene_id}-{session_id}",
        scene_id=scene_id,
        session_id=session_id,
        frame=world_frame,
        lighting=dict(lighting or {}),
        appearance=dict(appearance or {}),
        authority=AuthorityClass.SENSOR_DERIVED,
        lineage=Lineage(
            operation="build_world_model",
            inputs=[scene_id],
            parameters={
                "n_observations": len(observations),
                "source_authority": AuthorityClass.SENSOR_DERIVED.value,
                "track_source": next(
                    (
                        str(item.get("track_source", "unknown"))
                        for item in observations
                        if item.get("track_source")
                    ),
                    "unknown",
                ),
                "identity_provenance": identity_provenance,
                "allow_ground_truth": allow_ground_truth,
            },
        ),
        meta={
            "track_source": "unknown",
            "identity_provenance": identity_provenance,
        },
    )
    for observation in observations:
        update_world_model(world, observation, allow_ground_truth=allow_ground_truth)
    return world.seal()


def update_world_model(
    world: WorldState,
    observation: dict[str, Any],
    *,
    allow_ground_truth: bool = False,
) -> WorldState:
    """Incorporate one frame. Append-only; contradictions become competing beliefs."""
    _reject_ground_truth_identity(observation, allow_ground_truth=allow_ground_truth)
    if observation.get("absent"):
        return _apply_absent_frame(world, observation)

    frame_index = int(observation.get("frame_index", world.current_frame + 1))
    world.current_frame = frame_index
    track_source = _normalize_track_source(observation)
    if track_source is not None:
        world.meta["track_source"] = track_source
    if allow_ground_truth:
        world.meta["identity_provenance"] = "ground_truth"

    if "lighting" in observation and observation["lighting"] is not None:
        world.lighting = dict(observation["lighting"])
    if "appearance" in observation and observation["appearance"] is not None:
        world.appearance = dict(observation["appearance"])

    observed_ids: set[str] = set()
    for raw in observation.get("entities", []) or []:
        entity_id = str(raw.get("entity_id") or raw.get("track_id") or "")
        if not entity_id:
            raise ValidationError("observation entity requires entity_id or track_id")
        observed_ids.add(entity_id)
        _upsert_entity(
            world,
            raw,
            frame_index=frame_index,
            track_source=track_source or str(world.meta.get("track_source") or "unknown"),
        )

    # Entities not seen this frame: occlusion / departure path.
    for entity_id, entity in world.entities.items():
        if entity_id in observed_ids:
            continue
        if entity.state == "removed":
            continue
        _mark_unobserved(world, entity, frame_index=frame_index, reason="occlusion")

    world.digest = ""
    return world


def _upsert_entity(
    world: WorldState,
    raw: dict[str, Any],
    *,
    frame_index: int,
    track_source: str = "unknown",
) -> Entity:
    entity_id = str(raw.get("entity_id") or raw.get("track_id"))
    track_id = str(raw.get("track_id") or entity_id)
    class_label = str(raw.get("class_label") or raw.get("class") or "unknown")
    pose_m = _as_float_list(raw.get("pose_m") or [0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0])
    if len(pose_m) == 3:
        pose_m = pose_m + [1.0, 0.0, 0.0, 0.0]
    if len(pose_m) != 7:
        raise ValidationError(f"pose_m must be xyz or xyz+quat, got length {len(pose_m)}")

    visible = bool(raw.get("visible", True))
    visibility = (
        VisibilityState.DIRECTLY_VISIBLE if visible else VisibilityState.PARTIALLY_VISIBLE
    )
    appearance = dict(raw.get("appearance") or {})
    observation_ids = [str(item) for item in raw.get("observation_ids", [])] or [
        f"obs-{frame_index}-{entity_id}"
    ]
    input_auth = AuthorityClass(
        raw.get("authority", AuthorityClass.SENSOR_DERIVED.value)
    )

    entity = world.entities.get(entity_id)
    if entity is None:
        entity = Entity(
            id=f"entity-{entity_id}",
            entity_id=entity_id,
            track_id=track_id,
            class_label=class_label,
            pose_m=pose_m,
            confidence=_clamp_confidence(float(raw.get("confidence", 0.7))),
            visibility=visibility,
            appearance=appearance,
            frame=world.frame,
            authority=derive([input_auth], proposed=AuthorityClass.SENSOR_DERIVED),
            lineage=Lineage(
                operation="spawn_entity",
                inputs=observation_ids,
                parameters={"source_authority": input_auth.value},
            ),
            last_observed_frame=frame_index,
            frames_since_seen=0,
            state="active",
            source_bindings=observation_ids,
            uncertainty=Uncertainty(
                kind="pose",
                sigma=float(raw.get("pose_sigma", 0.02)),
                units=Units.METRE,
                basis="initial-observation",
                samples=1,
            ),
        )
        world.entities[entity_id] = entity
        _record_entity_identity_provenance(
            entity, track_source=track_source, frame_index=frame_index
        )
        conf = entity.confidence
        _append_belief(
            world,
            entity,
            slot=BeliefSlot.EXISTENCE.value,
            prior={},
            evidence={"observation_ids": observation_ids, "frame_index": frame_index},
            posterior={"exists": True, "confidence": conf},
            model="spawn",
            contradiction=False,
            competing=False,
            confidence_before=0.0,
            confidence_after=conf,
            frame_index=frame_index,
            authority=entity.authority,
        )
        _append_belief(
            world,
            entity,
            slot=BeliefSlot.POSE.value,
            prior={},
            evidence={"observation_ids": observation_ids, "pose_m": pose_m},
            posterior=_pose_belief_value(pose_m) | {"confidence": conf},
            model="direct_observation",
            contradiction=False,
            competing=False,
            confidence_before=0.0,
            confidence_after=conf,
            frame_index=frame_index,
            authority=entity.authority,
        )
        _append_belief(
            world,
            entity,
            slot=BeliefSlot.CLASS.value,
            prior={},
            evidence={"observation_ids": observation_ids, "class_label": class_label},
            posterior={"class_label": class_label, "confidence": conf},
            model="direct_observation",
            contradiction=False,
            competing=False,
            confidence_before=0.0,
            confidence_after=conf,
            frame_index=frame_index,
            authority=entity.authority,
        )
        if appearance:
            _append_belief(
                world,
                entity,
                slot=BeliefSlot.APPEARANCE.value,
                prior={},
                evidence={"observation_ids": observation_ids, "appearance": appearance},
                posterior={"appearance": appearance, "confidence": conf},
                model="direct_observation",
                contradiction=False,
                competing=False,
                confidence_before=0.0,
                confidence_after=conf,
                frame_index=frame_index,
                authority=entity.authority,
            )
        _maybe_add_surface(world, entity, raw, frame_index=frame_index, observed=True)
    else:
        _record_entity_identity_provenance(
            entity, track_source=track_source, frame_index=frame_index
        )
        prior_pose = list(entity.pose_m)
        prior_conf = entity.confidence
        conf_after = _clamp_confidence(prior_conf + _CONFIRM_CONFIDENCE_GAIN * 0.5)

        if _poses_contradict(prior_pose, pose_m):
            # Never overwrite: add a competing pose belief.
            conf_after = _clamp_confidence(prior_conf - 0.05)
            _append_belief(
                world,
                entity,
                slot=BeliefSlot.POSE.value,
                prior=_pose_belief_value(prior_pose),
                evidence={
                    "observation_ids": observation_ids,
                    "pose_m": pose_m,
                    "delta_m": _pose_distance(prior_pose, pose_m),
                },
                posterior=_pose_belief_value(pose_m) | {"confidence": conf_after},
                model="competing_observation",
                contradiction=True,
                competing=True,
                confidence_before=prior_conf,
                confidence_after=conf_after,
                frame_index=frame_index,
                authority=derive([input_auth, entity.authority]),
            )
            # Preferred pose follows the latest observation for tracking continuity,
            # but the prior remains in belief_sets and history.
            entity.pose_m = pose_m
        else:
            _append_belief(
                world,
                entity,
                slot=BeliefSlot.POSE.value,
                prior=_pose_belief_value(prior_pose),
                evidence={"observation_ids": observation_ids, "pose_m": pose_m},
                posterior=_pose_belief_value(pose_m) | {"confidence": conf_after},
                model="confirming_observation",
                contradiction=False,
                competing=False,
                confidence_before=prior_conf,
                confidence_after=conf_after,
                frame_index=frame_index,
                authority=derive([input_auth, entity.authority]),
            )
            entity.pose_m = pose_m

        if class_label and class_label != entity.class_label and class_label != "unknown":
            _append_belief(
                world,
                entity,
                slot=BeliefSlot.CLASS.value,
                prior={"class_label": entity.class_label},
                evidence={"observation_ids": observation_ids, "class_label": class_label},
                posterior={"class_label": class_label, "confidence": conf_after},
                model="competing_observation",
                contradiction=True,
                competing=True,
                confidence_before=prior_conf,
                confidence_after=conf_after,
                frame_index=frame_index,
                authority=derive([input_auth, entity.authority]),
            )
        elif class_label and class_label != "unknown":
            entity.class_label = class_label

        if appearance:
            prior_app = dict(entity.appearance)
            app_contradicts = bool(prior_app) and prior_app != appearance
            _append_belief(
                world,
                entity,
                slot=BeliefSlot.APPEARANCE.value,
                prior={"appearance": prior_app},
                evidence={"observation_ids": observation_ids, "appearance": appearance},
                posterior={"appearance": appearance, "confidence": conf_after},
                model="appearance_update",
                contradiction=app_contradicts,
                competing=app_contradicts,
                confidence_before=prior_conf,
                confidence_after=conf_after,
                frame_index=frame_index,
                authority=derive([input_auth, entity.authority]),
            )
            entity.appearance = appearance

        entity.confidence = conf_after
        entity.visibility = visibility
        entity.last_observed_frame = frame_index
        entity.frames_since_seen = 0
        entity.state = "active"
        entity.source_bindings = list(dict.fromkeys([*entity.source_bindings, *observation_ids]))
        sigma = entity.uncertainty.sigma or 0.02
        entity.uncertainty = Uncertainty(
            kind="pose",
            sigma=max(0.005, sigma * 0.85),
            units=Units.METRE,
            basis="confirming-observation",
            samples=(entity.uncertainty.samples or 0) + 1,
        )
        _maybe_add_surface(world, entity, raw, frame_index=frame_index, observed=True)

    entity.trajectory.append(
        {
            "frame_index": frame_index,
            "pose_m": list(entity.pose_m),
            "visible": visible,
            "confidence": entity.confidence,
        }
    )
    entity.digest = ""
    return entity


def _maybe_add_surface(
    world: WorldState,
    entity: Entity,
    raw: dict[str, Any],
    *,
    frame_index: int,
    observed: bool,
) -> None:
    surfaces = raw.get("surfaces")
    if not surfaces:
        # Default: one surface from the observed centroid.
        surface_id = f"surf-{entity.entity_id}-0"
        if surface_id in world.surfaces:
            return
        provenance = (
            SurfaceProvenance.DIRECTLY_OBSERVED
            if observed
            else SurfaceProvenance.PROCEDURALLY_INFERRED
        )
        visibility = (
            VisibilityState.DIRECTLY_VISIBLE if observed else VisibilityState.INFERRED_SURFACE
        )
        auth = (
            AuthorityClass.OBSERVED
            if provenance is SurfaceProvenance.DIRECTLY_OBSERVED
            else AuthorityClass.INFERRED
        )
        surface = Surface(
            id=surface_id,
            surface_id=surface_id,
            entity_id=entity.entity_id,
            provenance=provenance,
            visibility=visibility,
            frame=world.frame,
            centroid_m=list(entity.pose_m[:3]),
            confidence=entity.confidence,
            authority=auth,
            lineage=Lineage(
                operation="surface_from_entity",
                inputs=[entity.entity_id, f"frame-{frame_index}"],
                parameters={"source_authority": entity.authority.value},
            ),
            observation_ids=[f"obs-{frame_index}-{entity.entity_id}"],
        ).seal()
        world.surfaces[surface_id] = surface
        if observed:
            if surface_id not in entity.observed_surface_ids:
                entity.observed_surface_ids.append(surface_id)
        elif surface_id not in entity.inferred_surface_ids:
            entity.inferred_surface_ids.append(surface_id)
        return

    for index, raw_surf in enumerate(surfaces):
        surface_id = str(raw_surf.get("surface_id") or f"surf-{entity.entity_id}-{index}")
        provenance = SurfaceProvenance(
            raw_surf.get("provenance", SurfaceProvenance.DIRECTLY_OBSERVED.value)
        )
        visibility = VisibilityState(
            raw_surf.get("visibility", VisibilityState.DIRECTLY_VISIBLE.value)
        )
        auth = AuthorityClass(raw_surf.get("authority", AuthorityClass.SENSOR_DERIVED.value))
        ceiling = visibility_authority_ceiling(visibility)
        if strength(auth) > strength(ceiling):
            auth = ceiling
        if provenance in INFERRED_PROVENANCES and auth is AuthorityClass.OBSERVED:
            auth = AuthorityClass.INFERRED
        surface = Surface(
            id=surface_id,
            surface_id=surface_id,
            entity_id=entity.entity_id,
            provenance=provenance,
            visibility=visibility,
            frame=world.frame,
            centroid_m=_as_float_list(raw_surf.get("centroid_m") or entity.pose_m[:3]),
            normal=_as_float_list(raw_surf.get("normal") or [0.0, 0.0, 1.0]),
            area_m2=float(raw_surf.get("area_m2", 0.0)),
            confidence=float(raw_surf.get("confidence", entity.confidence)),
            authority=auth,
            lineage=Lineage(
                operation="surface_observation",
                inputs=[entity.entity_id],
                parameters={"source_authority": auth.value},
            ),
            observation_ids=list(raw_surf.get("observation_ids", [])),
        ).seal()
        world.surfaces[surface_id] = surface
        if provenance in INFERRED_PROVENANCES:
            if surface_id not in entity.inferred_surface_ids:
                entity.inferred_surface_ids.append(surface_id)
        elif surface_id not in entity.observed_surface_ids:
            entity.observed_surface_ids.append(surface_id)


def _mark_unobserved(
    world: WorldState,
    entity: Entity,
    *,
    frame_index: int,
    reason: str,
) -> None:
    prior_conf = entity.confidence
    decay = _OCCLUSION_CONFIDENCE_DECAY if reason == "occlusion" else _ABSENT_CONFIDENCE_DECAY
    conf_after = _clamp_confidence(prior_conf - decay)
    entity.frames_since_seen += 1
    entity.confidence = conf_after
    entity.visibility = (
        VisibilityState.INFERRED_SURFACE
        if reason == "occlusion"
        else entity.visibility
    )
    if entity.visibility is VisibilityState.DIRECTLY_VISIBLE:
        entity.visibility = VisibilityState.PARTIALLY_VISIBLE
    sigma = entity.uncertainty.sigma or 0.02
    entity.uncertainty = Uncertainty(
        kind="pose",
        sigma=sigma + 0.01 * entity.frames_since_seen,
        units=Units.METRE,
        basis=f"unobserved-{reason}",
        samples=entity.uncertainty.samples or 0,
    )
    _append_belief(
        world,
        entity,
        slot=BeliefSlot.VISIBILITY.value,
        prior={"visibility": VisibilityState.DIRECTLY_VISIBLE.value, "confidence": prior_conf},
        evidence={"frame_index": frame_index, "reason": reason, "absent": reason == "absent"},
        posterior={
            "visibility": entity.visibility.value,
            "frames_since_seen": entity.frames_since_seen,
            "confidence": conf_after,
        },
        model=f"persistence_{reason}",
        contradiction=False,
        competing=False,
        confidence_before=prior_conf,
        confidence_after=conf_after,
        frame_index=frame_index,
        authority=AuthorityClass.INFERRED,
    )
    entity.trajectory.append(
        {
            "frame_index": frame_index,
            "pose_m": list(entity.pose_m),
            "visible": False,
            "confidence": entity.confidence,
            "reason": reason,
        }
    )
    entity.digest = ""


def _apply_absent_frame(world: WorldState, observation: dict[str, Any]) -> WorldState:
    frame_index = int(observation.get("frame_index", world.current_frame + 1))
    world.current_frame = frame_index
    for entity in world.entities.values():
        if entity.state == "removed":
            continue
        _mark_unobserved(world, entity, frame_index=frame_index, reason="absent")
    world.digest = ""
    return world


# ---------------------------------------------------------------------------
# Query / explain / uncertainty
# ---------------------------------------------------------------------------


def query_world(world: WorldState, query: dict[str, Any]) -> dict[str, Any]:
    """Query entities, relations, or change reports.

    Supported forms:
      {"type": "entity", "entity_id": "..."}
      {"type": "class", "class_label": "..."}
      {"type": "relations", "kind": "same_as"}
      {"type": "scene_summary"}
      {"type": "compare_sessions", "prior": WorldState|dict, "move_tol_m": 0.05}
    """
    qtype = str(query.get("type", "scene_summary"))
    if qtype == "entity":
        entity = world.entities.get(str(query["entity_id"]))
        return {"found": entity is not None, "entity": entity.to_dict() if entity else None}
    if qtype == "class":
        label = str(query["class_label"])
        hits = [e.to_dict() for e in world.entities.values() if e.class_label == label]
        return {"count": len(hits), "entities": hits}
    if qtype == "relations":
        kind = query.get("kind")
        rels = [
            rel.to_dict()
            for rel in world.relations
            if kind is None or rel.kind.value == kind or rel.kind == kind
        ]
        return {"count": len(rels), "relations": rels}
    if qtype == "compare_sessions":
        prior = query.get("prior")
        if isinstance(prior, WorldState):
            prior_world = prior
        elif isinstance(prior, dict):
            prior_world = WorldState.from_dict(prior)
        else:
            raise ValidationError("compare_sessions requires prior WorldState or dict")
        return compare_worlds(
            prior_world,
            world,
            move_tolerance_m=float(query.get("move_tol_m", _DEFAULT_MOVE_TOLERANCE_M)),
            lighting_tolerance=float(query.get("lighting_tol", _DEFAULT_LIGHTING_TOLERANCE)),
        )
    # scene_summary
    return {
        "scene_id": world.scene_id,
        "session_id": world.session_id,
        "current_frame": world.current_frame,
        "n_entities": len(world.entities),
        "n_relations": len(world.relations),
        "n_belief_updates": len(world.belief_history),
        "beliefs_digest": world.beliefs_digest(),
        "entity_ids": sorted(world.entities),
        "lighting": world.lighting,
        "track_source": world.meta.get("track_source", "unknown"),
        "identity_provenance": world.meta.get("identity_provenance", "unknown"),
    }


def explain_belief(world: WorldState, entity_id: str, slot: str) -> list[dict[str, Any]]:
    """Return the append-only belief history for one entity slot."""
    return [
        update.to_dict()
        for update in world.belief_history
        if update.entity_id == entity_id and update.slot == slot
    ]


def list_uncertainties(world: WorldState) -> list[dict[str, Any]]:
    """Entities ordered by uncertainty (lowest confidence / highest sigma first)."""
    rows: list[dict[str, Any]] = []
    for entity in world.entities.values():
        sigma = entity.uncertainty.sigma if entity.uncertainty.sigma is not None else 1.0
        rows.append(
            {
                "entity_id": entity.entity_id,
                "class_label": entity.class_label,
                "confidence": entity.confidence,
                "pose_sigma_m": sigma,
                "frames_since_seen": entity.frames_since_seen,
                "visibility": entity.visibility.value
                if isinstance(entity.visibility, VisibilityState)
                else entity.visibility,
                "n_competing_pose_beliefs": sum(
                    1
                    for item in entity.all_beliefs(BeliefSlot.POSE.value)
                    if item.get("belief_id")
                ),
                "state": entity.state,
            }
        )
    rows.sort(key=lambda item: (item["confidence"], -float(item["pose_sigma_m"])))
    return rows


# ---------------------------------------------------------------------------
# Relations: same_as vs candidate_same_as
# ---------------------------------------------------------------------------


def propose_candidate_same_as(
    world: WorldState,
    source_id: str,
    target_id: str,
    *,
    confidence: float,
    evidence: list[str] | None = None,
) -> Relation:
    """Record a candidate identity link. Never promotes to same_as by itself."""
    if source_id not in world.entities or target_id not in world.entities:
        raise ValidationError("both entities must exist for candidate_same_as")
    rel = Relation(
        id=f"rel-cand-{source_id}-{target_id}",
        relation_id=f"rel-cand-{source_id}-{target_id}",
        kind=RelationKind.CANDIDATE_SAME_AS,
        source_id=source_id,
        target_id=target_id,
        confidence=float(confidence),
        evidence=list(evidence or []),
        evidence_recorded=bool(evidence),
        authority=AuthorityClass.INFERRED,
        lineage=Lineage(
            operation="propose_candidate_same_as",
            inputs=[source_id, target_id],
            parameters={"source_authority": AuthorityClass.INFERRED.value},
        ),
    ).seal()
    world.relations.append(rel)
    world.digest = ""
    return rel


def promote_same_as(
    world: WorldState,
    source_id: str,
    target_id: str,
    *,
    evidence: list[str],
    reviewer: str,
    confidence: float = 0.9,
) -> Relation:
    """Promote identity only with recorded evidence. candidate_same_as is not enough."""
    if not evidence:
        raise ValidationError("same_as requires recorded evidence")
    if not reviewer or reviewer.strip().lower() in {"system", "auto", "automatic", "visionmcp"}:
        # Identity is high-stakes; still allow system when evidence is explicit and
        # non-empty, but authority stays SENSOR_DERIVED rather than HUMAN_REVIEWED.
        authority = AuthorityClass.SENSOR_DERIVED
    else:
        authority = AuthorityClass.HUMAN_REVIEWED
    rel = Relation(
        id=f"rel-same-{source_id}-{target_id}",
        relation_id=f"rel-same-{source_id}-{target_id}",
        kind=RelationKind.SAME_AS,
        source_id=source_id,
        target_id=target_id,
        confidence=float(confidence),
        evidence=list(evidence),
        evidence_recorded=True,
        authority=authority,
        lineage=Lineage(
            operation="promote_same_as",
            inputs=[source_id, target_id, *evidence],
            parameters={"reviewer": reviewer, "source_authority": authority.value},
        ),
    ).seal()
    world.relations.append(rel)
    world.digest = ""
    return rel


def same_as_from_candidate_alone(
    world: WorldState,
    candidate: Relation,
) -> Relation:
    """Refused path: candidate_same_as never becomes same_as without new evidence."""
    if candidate.kind is not RelationKind.CANDIDATE_SAME_AS:
        raise ValidationError("expected candidate_same_as relation")
    raise ValidationError(
        "same_as cannot be inferred from candidate_same_as without recorded evidence"
    )


# ---------------------------------------------------------------------------
# Dynamic-room comparison (Phase M)
# ---------------------------------------------------------------------------


def _mean_luminance(lighting: dict[str, Any], appearance: dict[str, Any]) -> float | None:
    for key in ("mean_luminance", "luminance", "exposure", "intensity"):
        if key in lighting and lighting[key] is not None:
            return float(lighting[key])
        if key in appearance and appearance[key] is not None:
            return float(appearance[key])
    colour = lighting.get("rgb") or appearance.get("rgb")
    if isinstance(colour, list | tuple) and colour:
        return sum(float(c) for c in colour) / len(colour)
    return None


def compare_worlds(
    prior: WorldState,
    current: WorldState,
    *,
    move_tolerance_m: float = _DEFAULT_MOVE_TOLERANCE_M,
    lighting_tolerance: float = _DEFAULT_LIGHTING_TOLERANCE,
) -> dict[str, Any]:
    """Classify session-to-session changes with evidence and confidence.

    Returns the five required change classes for the dynamic-room benchmark:
    same_scene, moved_object, removed_object, new_object, lighting_only.
    Lighting-only is reported separately and never as geometry_change.
    """
    prior_ids = set(prior.entities)
    current_ids = set(current.entities)
    shared = prior_ids & current_ids
    removed = sorted(prior_ids - current_ids)
    added = sorted(current_ids - prior_ids)

    moved: list[dict[str, Any]] = []
    stable: list[dict[str, Any]] = []
    appearance_only: list[dict[str, Any]] = []

    for entity_id in sorted(shared):
        a = prior.entities[entity_id]
        b = current.entities[entity_id]
        delta = _pose_distance(a.pose_m, b.pose_m)
        geom_changed = delta > move_tolerance_m
        app_changed = a.appearance != b.appearance and bool(a.appearance or b.appearance)
        if geom_changed:
            moved.append(
                {
                    "entity_id": entity_id,
                    "class_label": b.class_label,
                    "delta_m": delta,
                    "prior_pose_m": list(a.pose_m),
                    "current_pose_m": list(b.pose_m),
                    "confidence": min(a.confidence, b.confidence),
                    "change_class": ChangeClass.MOVED_OBJECT.value,
                    "evidence": [
                        f"pose_delta_m={delta:.4f}",
                        f"tolerance_m={move_tolerance_m}",
                        f"prior_frame={a.last_observed_frame}",
                        f"current_frame={b.last_observed_frame}",
                    ],
                }
            )
        else:
            stable.append(
                {
                    "entity_id": entity_id,
                    "class_label": b.class_label,
                    "delta_m": delta,
                    "confidence": min(a.confidence, b.confidence),
                }
            )
            if app_changed:
                appearance_only.append(
                    {
                        "entity_id": entity_id,
                        "prior_appearance": a.appearance,
                        "current_appearance": b.appearance,
                        "change_class": ChangeClass.APPEARANCE_CHANGE.value,
                        "geometry_change": False,
                        "evidence": ["appearance_dict_diff", f"pose_delta_m={delta:.4f}<tol"],
                    }
                )

    prior_lum = _mean_luminance(prior.lighting, prior.appearance)
    curr_lum = _mean_luminance(current.lighting, current.appearance)
    lighting_delta = None
    lighting_changed = False
    lighting_evidence: list[str] = []
    if prior_lum is not None and curr_lum is not None:
        lighting_delta = abs(curr_lum - prior_lum)
        # The lighting-only class names a change *channel*: luminance/appearance
        # shifted. It is reported separately from geometry and is never itself a
        # geometry_change, even when other entities also moved.
        lighting_changed = lighting_delta > lighting_tolerance
        lighting_evidence = [
            f"prior_luminance={prior_lum}",
            f"current_luminance={curr_lum}",
            f"delta={lighting_delta}",
            f"tolerance={lighting_tolerance}",
            "channel=lighting_not_geometry",
            f"concurrent_geometry_moves={len(moved)}",
        ]
    elif prior.lighting != current.lighting and (prior.lighting or current.lighting):
        lighting_changed = True
        lighting_delta = 1.0
        lighting_evidence = [
            f"prior_lighting={prior.lighting}",
            f"current_lighting={current.lighting}",
            "structured_lighting_diff",
            "channel=lighting_not_geometry",
            f"concurrent_geometry_moves={len(moved)}",
        ]

    # same_scene: enough shared identity that this is the same room, even if
    # objects moved / lighting changed.
    n_prior = max(1, len(prior_ids))
    overlap = len(shared) / n_prior
    same_scene = overlap >= 0.5 or (len(shared) >= 1 and len(prior_ids) <= 2)
    same_scene_confidence = min(0.99, 0.5 + 0.5 * overlap)

    report = {
        "change_classes": {
            ChangeClass.SAME_SCENE.value: {
                "detected": same_scene,
                "confidence": same_scene_confidence,
                "evidence": [
                    f"shared_entities={sorted(shared)}",
                    f"overlap={overlap:.3f}",
                    f"prior_n={len(prior_ids)}",
                    f"current_n={len(current_ids)}",
                ],
            },
            ChangeClass.MOVED_OBJECT.value: {
                "detected": bool(moved),
                "confidence": min((item["confidence"] for item in moved), default=0.0),
                "items": moved,
                "evidence": [e for item in moved for e in item["evidence"]],
            },
            ChangeClass.REMOVED_OBJECT.value: {
                "detected": bool(removed),
                "confidence": 0.9 if removed else 0.0,
                "items": [
                    {
                        "entity_id": eid,
                        "class_label": prior.entities[eid].class_label,
                        "last_pose_m": list(prior.entities[eid].pose_m),
                        "evidence": [f"absent_from_session={current.session_id}"],
                    }
                    for eid in removed
                ],
                "evidence": [f"removed={removed}"],
            },
            ChangeClass.NEW_OBJECT.value: {
                "detected": bool(added),
                "confidence": 0.9 if added else 0.0,
                "items": [
                    {
                        "entity_id": eid,
                        "class_label": current.entities[eid].class_label,
                        "pose_m": list(current.entities[eid].pose_m),
                        "evidence": [f"absent_from_session={prior.session_id}"],
                    }
                    for eid in added
                ],
                "evidence": [f"added={added}"],
            },
            ChangeClass.LIGHTING_ONLY.value: {
                "detected": lighting_changed,
                "confidence": 0.85 if lighting_changed else 0.0,
                "lighting_delta": lighting_delta,
                # Hard law: a lighting-channel change is never a geometry change.
                "geometry_change": False,
                "evidence": lighting_evidence
                or ["no_luminance_signal", f"geometry_moves={len(moved)}"],
            },
        },
        "appearance_only_entities": appearance_only,
        "stable_entities": stable,
        "geometry_change": bool(moved),
        "lighting_reported_as_geometry": False,
    }
    lighting_block = report["change_classes"][ChangeClass.LIGHTING_ONLY.value]
    if lighting_block["detected"] and lighting_block["geometry_change"]:
        raise ValidationError("lighting-only change incorrectly marked as geometry")
    return report


# ---------------------------------------------------------------------------
# Persistence
# ---------------------------------------------------------------------------


def save_world(world: WorldState, path: Path | str) -> str:
    """Serialize the sealed world. Returns the beliefs digest."""
    path = Path(path)
    if not world.digest:
        world.seal()
    world.verify()
    payload = world.to_dict()
    atomic_write_json(path, payload)
    # Also write a pure-beliefs slice for cross-process byte checks.
    beliefs_path = path.with_suffix(path.suffix + ".beliefs.json")
    atomic_write_json(beliefs_path, world.beliefs_payload())
    return world.beliefs_digest()


def load_world(path: Path | str) -> WorldState:
    """Reload a world from disk and verify digests."""
    path = Path(path)
    payload = json.loads(path.read_text(encoding="utf-8"))
    world = WorldState.from_dict(payload)
    stored = payload.get("digest", "")
    if stored:
        world.digest = stored
        world.verify()
    return world


def world_bytes(world: WorldState) -> bytes:
    """Canonical byte representation of the sealed world payload."""
    if not world.digest:
        world.seal()
    return canonical_json(world.to_dict())


def beliefs_bytes(world: WorldState) -> bytes:
    return canonical_json(world.beliefs_payload())


# ---------------------------------------------------------------------------
# Confidence adjustments used by the predictive loop
# ---------------------------------------------------------------------------


def raise_uncertainty(
    world: WorldState,
    entity_id: str,
    *,
    reason: str,
    magnitude: float,
    frame_index: int | None = None,
    evidence: dict[str, Any] | None = None,
) -> Entity:
    """Drop confidence / raise sigma after a surprise or failed prediction."""
    entity = world.entities.get(entity_id)
    if entity is None:
        raise ValidationError(f"unknown entity {entity_id}")
    prior = entity.confidence
    drop = _SURPRISE_CONFIDENCE_DROP * max(0.25, min(2.0, float(magnitude)))
    after = _clamp_confidence(prior - drop)
    entity.confidence = after
    sigma = entity.uncertainty.sigma or 0.02
    entity.uncertainty = Uncertainty(
        kind="pose",
        sigma=sigma + 0.02 * max(1.0, float(magnitude)),
        units=Units.METRE,
        basis=f"surprise:{reason}",
        samples=entity.uncertainty.samples or 0,
    )
    _append_belief(
        world,
        entity,
        slot=BeliefSlot.POSE.value,
        prior={"confidence": prior, "pose_m": list(entity.pose_m)},
        evidence=evidence or {"reason": reason, "magnitude": magnitude},
        posterior={
            "confidence": after,
            "pose_m": list(entity.pose_m),
            "uncertainty_sigma": entity.uncertainty.sigma,
        },
        model="surprise_update",
        contradiction=True,
        competing=True,
        confidence_before=prior,
        confidence_after=after,
        frame_index=frame_index if frame_index is not None else world.current_frame,
        authority=AuthorityClass.INFERRED,
    )
    entity.digest = ""
    world.digest = ""
    return entity


def lower_uncertainty(
    world: WorldState,
    entity_id: str,
    *,
    reason: str = "confirming_evidence",
    frame_index: int | None = None,
    evidence: dict[str, Any] | None = None,
) -> Entity:
    """Raise confidence / lower sigma after confirming evidence."""
    entity = world.entities.get(entity_id)
    if entity is None:
        raise ValidationError(f"unknown entity {entity_id}")
    prior = entity.confidence
    after = _clamp_confidence(prior + _CONFIRM_CONFIDENCE_GAIN)
    entity.confidence = after
    sigma = entity.uncertainty.sigma or 0.02
    entity.uncertainty = Uncertainty(
        kind="pose",
        sigma=max(0.005, sigma * 0.7),
        units=Units.METRE,
        basis=reason,
        samples=(entity.uncertainty.samples or 0) + 1,
    )
    _append_belief(
        world,
        entity,
        slot=BeliefSlot.POSE.value,
        prior={"confidence": prior, "pose_m": list(entity.pose_m)},
        evidence=evidence or {"reason": reason},
        posterior={
            "confidence": after,
            "pose_m": list(entity.pose_m),
            "uncertainty_sigma": entity.uncertainty.sigma,
        },
        model="confirmation_update",
        contradiction=False,
        competing=False,
        confidence_before=prior,
        confidence_after=after,
        frame_index=frame_index if frame_index is not None else world.current_frame,
        authority=AuthorityClass.SENSOR_DERIVED,
    )
    entity.digest = ""
    world.digest = ""
    return entity
