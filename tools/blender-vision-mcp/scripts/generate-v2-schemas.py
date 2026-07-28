#!/usr/bin/env python3
"""Emit the frozen `schemas/v2/*.schema.json` contracts.

Structure comes from the dataclasses so the schema cannot silently drift from
the code; the constraint overlay below is hand-authored and is what actually
makes the schemas strict. `tests/test_v2_records.py` re-runs this generator and
fails when the committed bytes differ.
"""

from __future__ import annotations

import json
import sys
import types
import typing
from dataclasses import MISSING, fields, is_dataclass
from enum import StrEnum
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.v2.authority import (  # noqa: E402
    AuthorityClass,
    Handedness,
    Units,
    VisibilityState,
)
from blender_vision.v2.records import (  # noqa: E402
    RECORD_TYPES,
    CriticFinding,
    DeliveryAsset,
    Lifecycle,
    LightingHypothesis,
    Lineage,
    MaterialHypothesis,
    NarrativeBeat,
    ReconstructionCandidate,
)
from blender_vision.v2.records import (  # noqa: E402
    Uncertainty as UncertaintyRecord,
)

SHA256 = {"type": "string", "pattern": "^[0-9a-f]{64}$"}
UNIT_INTERVAL = {"type": "number", "minimum": 0.0, "maximum": 1.0}
VEC3 = {"type": "array", "items": {"type": "number"}, "minItems": 3, "maxItems": 3}
AXIS = {"type": "string", "enum": ["+X", "-X", "+Y", "-Y", "+Z", "-Z"]}

# Hand-authored constraints. Anything named here overrides the derived shape.
# Keyed by "<TypeName>.<field>" or "*.<field>" for every type.
CONSTRAINTS: dict[str, dict[str, Any]] = {
    "*.digest": SHA256,
    "*.authority": {"type": "string", "enum": [item.value for item in AuthorityClass]},
    "*.lifecycle": {"type": "string", "enum": [item.value for item in Lifecycle]},
    "*.scale_authority": {"type": "string", "enum": [item.value for item in AuthorityClass]},
    "*.schema_version": {"type": "string", "const": "2"},
    "*.created_at": {"type": "string", "minLength": 20},
    "*.id": {"type": "string", "minLength": 1, "maxLength": 256},
    "*.confidence": UNIT_INTERVAL,
    "*.base_colour": VEC3,
    "*.emission": VEC3,
    "*.subsurface_radius": VEC3,
    "CoordinateFrame.up_axis": AXIS,
    "CoordinateFrame.forward_axis": AXIS,
    "CoordinateFrame.handedness": {
        "type": "string",
        "enum": [item.value for item in Handedness],
    },
    "CoordinateFrame.units": {"type": "string", "enum": [item.value for item in Units]},
    "Uncertainty.units": {"type": "string", "enum": [item.value for item in Units]},
    "MaterialHypothesis.roughness": UNIT_INTERVAL,
    "MaterialHypothesis.metalness": UNIT_INTERVAL,
    "MaterialHypothesis.anisotropy": {"type": "number", "minimum": -1.0, "maximum": 1.0},
    "MaterialHypothesis.clearcoat": UNIT_INTERVAL,
    "MaterialHypothesis.clearcoat_roughness": UNIT_INTERVAL,
    "MaterialHypothesis.transmission": UNIT_INTERVAL,
    "MaterialHypothesis.subsurface": UNIT_INTERVAL,
    "MaterialHypothesis.occlusion": UNIT_INTERVAL,
    "MaterialHypothesis.specular_ior": {"type": "number", "minimum": 1.0, "maximum": 4.0},
    "MaterialHypothesis.texture_scale_m": {"type": "number", "exclusiveMinimum": 0.0},
    "LightingHypothesis.shadow_softness": UNIT_INTERVAL,
    "LightingHypothesis.white_balance_k": {
        "type": "number",
        "minimum": 1000.0,
        "maximum": 20000.0,
    },
    "LightingHypothesis.rig_class": {
        "type": "string",
        "enum": [
            "product_studio",
            "black_architectural",
            "neutral_documentation",
            "indoor_practical",
            "outdoor_daylight",
            "cinematic_corridor",
            "soft_organic",
        ],
    },
    "CriticFinding.severity": {
        "type": "string",
        "enum": ["info", "minor", "major", "critical"],
    },
    "CriticFinding.evidence": {
        "type": "array",
        "items": {"type": "string"},
        "minItems": 1,
    },
    "NextViewRequest.priority": {"type": "integer", "minimum": 0, "maximum": 10},
    "NextViewRequest.expected_reduction": UNIT_INTERVAL,
    "CameraPathGraph.damping": UNIT_INTERVAL,
    "CameraPathGraph.arc_length_m": {"type": "number", "minimum": 0.0},
    "CameraPathGraph.skip_points": {"type": "array", "items": UNIT_INTERVAL},
    "NarrativeBeat.scroll_start": UNIT_INTERVAL,
    "NarrativeBeat.scroll_end": UNIT_INTERVAL,
    "NarrativeBeat.dwell": {"type": "number", "minimum": 0.0},
    "DeliveryAsset.bytes": {"type": "integer", "minimum": 0},
    "DeliveryAsset.digest": SHA256,
    "DeliveryAsset.role": {
        "type": "string",
        "enum": ["poster", "shell", "detail", "mobile", "network", "terminal", "texture"],
    },
    "SceneEvidenceGraph.visibility": {
        "type": "object",
        "additionalProperties": {
            "type": "string",
            "enum": [item.value for item in VisibilityState],
        },
    },
}

NESTED: dict[Any, str] = {
    Lineage: "Lineage",
    UncertaintyRecord: "Uncertainty",
    ReconstructionCandidate: "ReconstructionCandidate",
    MaterialHypothesis: "MaterialHypothesis",
    LightingHypothesis: "LightingHypothesis",
    NarrativeBeat: "NarrativeBeat",
    CriticFinding: "CriticFinding",
    DeliveryAsset: "DeliveryAsset",
}

RECORD_REQUIRED = ["record_kind", "id", "schema_version", "created_at", "authority", "digest"]

EXTRA_REQUIRED: dict[str, list[str]] = {
    "v2.observation-bundle": ["target_id"],
    "v2.reconstruction-portfolio": ["target_id", "candidates"],
    "v2.material-hypothesis-set": ["hypotheses"],
    "v2.lighting-hypothesis-set": ["hypotheses"],
    "v2.procedural-scene-graph": ["scene_name", "frame"],
    "v2.camera-path-graph": ["control_points", "beats"],
    "v2.perceptual-critique": ["subject_id", "findings", "passed"],
    "v2.delivery-manifest": ["source_scene", "assets"],
    "v2.next-view-request": ["target_id", "reason", "priority"],
    "v2.scene-evidence-graph": ["frame", "nodes"],
}


def _resolve(annotation: Any, owner: type) -> Any:
    if isinstance(annotation, str):
        namespace = dict(vars(sys.modules[owner.__module__]))
        return typing.ForwardRef(annotation)._evaluate(
            namespace, namespace, type_params=(), recursive_guard=frozenset()
        )
    return annotation


def _schema_for_type(annotation: Any, owner: type) -> dict[str, Any]:
    annotation = _resolve(annotation, owner)
    origin = typing.get_origin(annotation)
    if origin in (types.UnionType, typing.Union):
        args = [item for item in typing.get_args(annotation) if item is not type(None)]
        inner = _schema_for_type(args[0], owner)
        return {"anyOf": [inner, {"type": "null"}]}
    if origin in (list, typing.List):  # noqa: UP006
        (item,) = typing.get_args(annotation) or (Any,)
        return {"type": "array", "items": _schema_for_type(item, owner)}
    if origin in (dict, typing.Dict):  # noqa: UP006
        args = typing.get_args(annotation)
        value = _schema_for_type(args[1], owner) if len(args) == 2 else True
        return {"type": "object", "additionalProperties": value}
    if annotation in NESTED:
        return {"$ref": f"#/$defs/{NESTED[annotation]}"}
    if isinstance(annotation, type) and issubclass(annotation, StrEnum):
        return {"type": "string", "enum": [item.value for item in annotation]}
    if is_dataclass(annotation):
        return {"$ref": f"#/$defs/{annotation.__name__}"}
    return {
        bool: {"type": "boolean"},
        int: {"type": "integer"},
        float: {"type": "number"},
        str: {"type": "string"},
    }.get(annotation, True)


def _object_schema(cls: type, *, extra_properties: dict[str, Any] | None = None) -> dict[str, Any]:
    properties: dict[str, Any] = dict(extra_properties or {})
    required: list[str] = []
    for item in fields(cls):
        override = CONSTRAINTS.get(f"{cls.__name__}.{item.name}") or CONSTRAINTS.get(
            f"*.{item.name}"
        )
        properties[item.name] = override or _schema_for_type(item.type, cls)
        if item.default is MISSING and item.default_factory is MISSING:  # type: ignore[misc]
            required.append(item.name)
    schema: dict[str, Any] = {
        "type": "object",
        "additionalProperties": False,
        "properties": properties,
    }
    if required:
        schema["required"] = sorted(required)
    return schema


def _defs() -> dict[str, Any]:
    from blender_vision.v2.authority import CoordinateFrame

    defs: dict[str, Any] = {}
    for cls, name in list(NESTED.items()) + [(CoordinateFrame, "CoordinateFrame")]:
        defs[name] = _object_schema(cls)
    return defs


def build(kind: str, cls: type) -> dict[str, Any]:
    schema = _object_schema(
        cls,
        extra_properties={"record_kind": {"type": "string", "const": kind}},
    )
    schema["properties"]["digest"] = SHA256
    required = sorted(set(RECORD_REQUIRED) | set(EXTRA_REQUIRED.get(kind, [])))
    schema["required"] = required
    return {
        "$schema": "https://json-schema.org/draft/2020-12/schema",
        "$id": f"https://computexchange.dev/visionmcp/schemas/v2/{kind.removeprefix('v2.')}.json",
        "title": cls.__name__,
        "description": (cls.__doc__ or "").strip().splitlines()[0] if cls.__doc__ else kind,
        "$defs": _defs(),
        **schema,
    }


def main() -> int:
    out = ROOT / "schemas" / "v2"
    out.mkdir(parents=True, exist_ok=True)
    written = []
    for kind, cls in sorted(RECORD_TYPES.items()):
        path = out / f"{kind.removeprefix('v2.')}.schema.json"
        path.write_text(json.dumps(build(kind, cls), indent=2, sort_keys=True) + "\n")
        written.append(path.name)
    print(f"wrote {len(written)} schemas to {out}")
    for name in written:
        print(" ", name)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
