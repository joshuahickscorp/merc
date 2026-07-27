"""Instance placement, de-duplication, and Blender collection-instance plans."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from blender_vision.procedural.grammar import InstanceRef


@dataclass(slots=True)
class MeshPrototype:
    """Unique mesh prototype shared by one or more instances."""

    prototype_id: str
    archetype: str
    params: dict[str, Any]
    mesh_key: str
    instance_ids: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "prototype_id": self.prototype_id,
            "archetype": self.archetype,
            "params": dict(self.params),
            "mesh_key": self.mesh_key,
            "instance_ids": list(self.instance_ids),
            "count": len(self.instance_ids),
        }


@dataclass(slots=True)
class InstancePlacement:
    instance_id: str
    prototype_id: str
    location: tuple[float, float, float]
    rotation_euler: tuple[float, float, float]
    scale: tuple[float, float, float]
    state: dict[str, Any] = field(default_factory=dict)
    tags: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "instance_id": self.instance_id,
            "prototype_id": self.prototype_id,
            "location": list(self.location),
            "rotation_euler": list(self.rotation_euler),
            "scale": list(self.scale),
            "state": dict(self.state),
            "tags": list(self.tags),
        }


@dataclass(slots=True)
class InstancingPlan:
    """De-duplicated mesh prototypes + per-instance placements.

    Emit uses collection instances / linked duplicates — never independent
    mesh copies for identical mesh_keys.
    """

    prototypes: list[MeshPrototype]
    placements: list[InstancePlacement]

    def unique_mesh_count(self) -> int:
        return len(self.prototypes)

    def instance_count(self) -> int:
        return len(self.placements)

    def to_dict(self) -> dict[str, Any]:
        return {
            "unique_mesh_count": self.unique_mesh_count(),
            "instance_count": self.instance_count(),
            "prototypes": [item.to_dict() for item in self.prototypes],
            "placements": [item.to_dict() for item in self.placements],
        }


def build_instancing_plan(instances: list[InstanceRef]) -> InstancingPlan:
    """Group instances by mesh_key (archetype + geometry params, not state)."""
    proto_by_key: dict[str, MeshPrototype] = {}
    placements: list[InstancePlacement] = []
    for inst in instances:
        key = inst.mesh_key or f"{inst.archetype}|{sorted(inst.params.items())}"
        if key not in proto_by_key:
            proto_id = f"proto_{len(proto_by_key):04d}_{inst.archetype}"
            proto_by_key[key] = MeshPrototype(
                prototype_id=proto_id,
                archetype=inst.archetype,
                params=dict(inst.params),
                mesh_key=key,
            )
        proto = proto_by_key[key]
        proto.instance_ids.append(inst.instance_id)
        placements.append(
            InstancePlacement(
                instance_id=inst.instance_id,
                prototype_id=proto.prototype_id,
                location=inst.transform.location,
                rotation_euler=inst.transform.rotation_euler,
                scale=inst.transform.scale,
                state=dict(inst.state),
                tags=list(inst.tags),
            )
        )
    return InstancingPlan(prototypes=list(proto_by_key.values()), placements=placements)


def assert_state_does_not_split_meshes(
    instances: list[InstanceRef],
    *,
    archetype: str,
) -> None:
    """Guard: same geometry params + different state must share one mesh_key."""
    by_params: dict[str, set[str]] = {}
    for inst in instances:
        if inst.archetype != archetype:
            continue
        param_key = repr(sorted(inst.params.items()))
        by_params.setdefault(param_key, set()).add(inst.mesh_key)
    for param_key, keys in by_params.items():
        if len(keys) > 1:
            raise AssertionError(
                f"state variation split meshes for {archetype} params {param_key}: {keys}"
            )
