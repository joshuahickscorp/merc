"""Semantic procedural world engine for VisionMCP V2."""

from __future__ import annotations

from blender_vision.procedural.archetype import (
    Archetype,
    Dimensions,
    GeometryKind,
    GeometryRecipe,
    ParamSpec,
    PartRole,
    PartSpec,
    Transform3D,
    mesh_fingerprint,
)
from blender_vision.procedural.emit import emit_archetype, emit_scene_plan, emit_to_blender
from blender_vision.procedural.grammar import (
    SceneProgram,
    datacenter_flagship_program,
    evaluate_program,
)
from blender_vision.procedural.instancing import build_instancing_plan
from blender_vision.procedural.library import ArchetypeLibrary, default_library, list_archetypes
from blender_vision.procedural.lod import check_lod_identity, generate_lods
from blender_vision.procedural.scene import CompiledScene, build_flagship_scene, compile_scene

__all__ = [
    "Archetype",
    "ArchetypeLibrary",
    "CompiledScene",
    "Dimensions",
    "GeometryKind",
    "GeometryRecipe",
    "ParamSpec",
    "PartRole",
    "PartSpec",
    "SceneProgram",
    "Transform3D",
    "build_flagship_scene",
    "build_instancing_plan",
    "check_lod_identity",
    "compile_scene",
    "datacenter_flagship_program",
    "default_library",
    "emit_archetype",
    "emit_scene_plan",
    "emit_to_blender",
    "evaluate_program",
    "generate_lods",
    "list_archetypes",
    "mesh_fingerprint",
]
