"""Compose sealed ProceduralSceneGraph V2 records from grammar programs."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any
from uuid import uuid4

from blender_vision.procedural.archetype import measure_parts_bounds
from blender_vision.procedural.grammar import (
    InstanceRef,
    SceneProgram,
    datacenter_flagship_program,
    evaluate_program,
)
from blender_vision.procedural.instancing import InstancingPlan, build_instancing_plan
from blender_vision.procedural.library import ArchetypeLibrary, default_library
from blender_vision.procedural.lod import DEFAULT_TRIANGLE_BUDGETS, generate_lods
from blender_vision.v2.authority import (
    BLENDER_WORLD,
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
)
from blender_vision.v2.records import Lifecycle, Lineage, ProceduralSceneGraph
from blender_vision.v2.validation import validate_record, verify_payload


@dataclass(slots=True)
class CompiledScene:
    program: SceneProgram
    instances: list[InstanceRef]
    plan: InstancingPlan
    record: ProceduralSceneGraph
    modules: list[dict[str, Any]] = field(default_factory=list)
    archetype_entries: list[dict[str, Any]] = field(default_factory=list)
    bounds_m: dict[str, Any] = field(default_factory=dict)


def _scene_bounds(instances: list[InstanceRef], library: ArchetypeLibrary) -> dict[str, Any]:
    mins = [float("inf")] * 3
    maxs = [float("-inf")] * 3
    for inst in instances:
        arch = library.create(inst.archetype, inst.params)
        dims = arch.declared_dimensions()
        loc = inst.transform.location
        half = (dims.width_m * 0.5, dims.depth_m * 0.5, dims.height_m * 0.5)
        # Conservative AABB ignoring rotation (good enough for scene envelope).
        for i, (c, h) in enumerate(zip(loc, half, strict=True)):
            mins[i] = min(mins[i], c - h)
            maxs[i] = max(maxs[i], c + h)
    if mins[0] == float("inf"):
        return {"min": [0.0, 0.0, 0.0], "max": [0.0, 0.0, 0.0]}
    return {
        "min": mins,
        "max": maxs,
        "size": [maxs[i] - mins[i] for i in range(3)],
    }


def compile_scene(
    program: SceneProgram,
    *,
    library: ArchetypeLibrary | None = None,
    scene_id: str | None = None,
    text_safe_zones: list[dict[str, Any]] | None = None,
) -> CompiledScene:
    library = library or default_library()
    instances = evaluate_program(program)
    plan = build_instancing_plan(instances)

    modules: list[dict[str, Any]] = []
    archetype_entries: list[dict[str, Any]] = []
    seen_arch: set[str] = set()
    for proto in plan.prototypes:
        arch = library.create(proto.archetype, proto.params)
        parts = arch.build()
        lods = generate_lods(parts)
        modules.append(
            {
                "module_id": proto.prototype_id,
                "archetype": proto.archetype,
                "params": dict(proto.params),
                "mesh_key": proto.mesh_key,
                "dimensions_m": arch.declared_dimensions().to_dict(),
                "part_names": arch.part_names(),
                "lod_levels": {
                    level: {
                        "triangle_budget": variant.triangle_budget,
                        "part_names": sorted(variant.part_names),
                        "dimensions_m": variant.dimensions.to_dict(),
                    }
                    for level, variant in lods.items()
                },
                "instance_ids": list(proto.instance_ids),
            }
        )
        if proto.archetype not in seen_arch:
            seen_arch.add(proto.archetype)
            archetype_entries.append(arch.to_manifest_entry())

    bounds = _scene_bounds(instances, library)
    instance_payloads = [inst.to_dict() for inst in instances]
    grammar_payload = [op.to_dict() for op in program.operations]

    # Empty input_authorities: PROCEDURAL_GROUND_TRUTH is an authored synthetic
    # source, not a derivation from weaker evidence (derive() would cap at INFERRED).
    lineage = Lineage(
        tool="blender-vision-mcp",
        tool_version="0.1.0",
        operation="procedural.compile_scene",
        inputs=[f"program:{program.name}"],
        input_authorities=[],
        parameters={
            "program_name": program.name,
            "instance_count": len(instances),
            "unique_mesh_count": plan.unique_mesh_count(),
            "dimensional_basis": "EIA-310/IEC-60297 manufacturer specifications",
        },
        rights_state="internal-synthetic",
        limitations=[
            "Scene is procedural synthetic ground truth for film/layout authoring.",
            "Rack dimensions follow EIA-310 / IEC 60297; layout is authored, not observed.",
        ],
    )

    record = ProceduralSceneGraph(
        id=scene_id or f"psg-{uuid4().hex[:12]}",
        scene_name=program.name,
        authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        lineage=lineage,
        uncertainty=Uncertainty(
            kind="procedural-exact",
            sigma=0.0,
            units=Units.METRE,
            basis="deterministic-grammar-expansion",
        ),
        frame=CoordinateFrame(
            name="blender-world",
            up_axis=BLENDER_WORLD.up_axis,
            forward_axis=BLENDER_WORLD.forward_axis,
            scale_authority=AuthorityClass.PROCEDURAL_GROUND_TRUTH,
        ),
        modules=modules,
        instances=instance_payloads,
        archetypes=archetype_entries,
        grammar=grammar_payload,
        lod_policy={
            "levels": list(DEFAULT_TRIANGLE_BUDGETS.keys()),
            "triangle_budgets": dict(DEFAULT_TRIANGLE_BUDGETS),
            "identity_check": {
                "bbox_tolerance_m": 0.02,
                "silhouette_tolerance": 0.05,
                "report_part_loss": True,
            },
        },
        text_safe_zones=list(text_safe_zones or []),
        bounds_m=bounds,
        lifecycle=Lifecycle.CANDIDATE,
    )
    record.seal()
    validate_record(record)
    verify_payload(record.to_dict())

    return CompiledScene(
        program=program,
        instances=instances,
        plan=plan,
        record=record,
        modules=modules,
        archetype_entries=archetype_entries,
        bounds_m=bounds,
    )


def build_flagship_scene(**kwargs: Any) -> CompiledScene:
    program = datacenter_flagship_program(**kwargs)
    return compile_scene(program)


def measure_instance_parts(instance: InstanceRef, library: ArchetypeLibrary | None = None):
    library = library or default_library()
    return measure_parts_bounds(library.create(instance.archetype, instance.params).build())
