"""Tests for the VisionMCP V2 semantic procedural world engine."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from blender_vision.core.errors import ValidationError
from blender_vision.procedural.archetype import mesh_fingerprint
from blender_vision.procedural.datacenter import DATACENTER_ARCHETYPES, StatusLightMatrix
from blender_vision.procedural.grammar import (
    SceneProgram,
    empty_u_slots,
    evaluate_program,
    occupied_u_ranges,
)
from blender_vision.procedural.instancing import (
    assert_state_does_not_split_meshes,
    build_instancing_plan,
)
from blender_vision.procedural.library import default_library, list_archetypes
from blender_vision.procedural.lod import (
    check_lod_identity,
    generate_lods,
    intentionally_broken_far_lod,
)
from blender_vision.procedural.scene import build_flagship_scene, compile_scene
from blender_vision.procedural.standards import (
    FRAME_WIDTH_600_M,
    MOUNTING_WIDTH_M,
    U_HEIGHT_M,
    drawer_height_m,
    rack_height_m,
)
from blender_vision.v2.validation import validate_record, verify_payload

REQUIRED_ARCHETYPES = (
    "rack_shell",
    "rack_door",
    "server_drawer",
    "gpu_drawer",
    "switch",
    "blanking_panel",
    "pdu",
    "cable_tray",
    "cable_bundle",
    "cooling_face",
    "floor_tile",
    "ceiling_panel",
    "wall_rib",
    "column",
    "threshold",
    "aisle",
    "junction",
    "containment_door",
    "terminal_wall",
    "status_light_matrix",
)

BLENDER_GATE = pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender procedural emission",
)


def test_all_twenty_datacenter_archetypes_registered() -> None:
    names = set(list_archetypes())
    assert names == set(REQUIRED_ARCHETYPES)
    assert len(DATACENTER_ARCHETYPES) == 20


@pytest.mark.parametrize("name", REQUIRED_ARCHETYPES)
def test_parameter_validation_rejects_out_of_range(name: str) -> None:
    library = default_library()
    cls = library.get(name)
    assert cls.parameter_specs, f"{name} must declare parameters"
    # Build once with defaults — must succeed.
    library.create(name)
    # Out-of-range on first numeric parameter with bounds.
    for spec in cls.parameter_specs:
        if spec.kind == "int" and spec.maximum is not None:
            with pytest.raises(ValidationError):
                library.create(name, {spec.name: int(spec.maximum) + 1})
            with pytest.raises(ValidationError):
                low = int(spec.minimum) - 1 if spec.minimum is not None else -999
                library.create(name, {spec.name: low})
            return
        if spec.kind == "float" and spec.maximum is not None and not spec.choices:
            with pytest.raises(ValidationError):
                library.create(name, {spec.name: float(spec.maximum) + 1.0})
            return
        if spec.choices:
            with pytest.raises(ValidationError):
                library.create(name, {spec.name: "__invalid_choice__"})
            return
    pytest.fail(f"{name}: no ranged parameter found to validate")


@pytest.mark.parametrize("name", REQUIRED_ARCHETYPES)
def test_declared_vs_measured_dimensions_agree(name: str) -> None:
    arch = default_library().create(name)
    arch.assert_dimensions(tolerance_m=0.001)


def test_one_u_and_nineteen_inch_arithmetic() -> None:
    assert abs(U_HEIGHT_M - 0.04445) < 1e-12
    assert abs(MOUNTING_WIDTH_M - 0.4826) < 1e-12
    assert abs(rack_height_m(42) - 42 * U_HEIGHT_M) < 1e-12
    assert abs(rack_height_m(42) - 1.8669) < 1e-9
    assert abs(FRAME_WIDTH_600_M - 0.600) < 1e-12
    # Drawer height is U stack minus 0.5 mm clearance.
    assert abs(drawer_height_m(1) - (U_HEIGHT_M - 0.0005)) < 1e-12
    rack = default_library().create("rack_shell", {"u_count": 42, "frame_width_m": 0.6})
    dims = rack.declared_dimensions()
    assert abs(dims.height_m - rack_height_m(42)) < 1e-12
    assert abs(dims.width_m - 0.6) < 1e-12
    drawer = default_library().create("server_drawer", {"u_height": 1})
    assert abs(drawer.declared_dimensions().width_m - MOUNTING_WIDTH_M) < 1e-12


def test_leave_gap_empties_requested_u_ranges() -> None:
    program = SceneProgram(name="gap_test")
    program.place(
        "rack_shell",
        "rack_a",
        params={"u_count": 42, "frame_width_m": 0.6, "depth_m": 1.0},
        tags=["rack"],
    )
    program.leave_gap("rack_a", [(9, 10), (17, 18), (25, 26)])
    program.populate_rack(
        "rack_a",
        u_start=3,
        u_end=28,
        archetype="gpu_drawer",
        u_height=4,
        params={"u_height": 4, "gpu_count": 4, "depth_m": 0.85},
    )
    instances = evaluate_program(program)
    occupied = occupied_u_ranges(instances, "rack_a")
    for start, end in occupied:
        for gap in ((9, 10), (17, 18), (25, 26)):
            assert end < gap[0] or start > gap[1]
    empty = empty_u_slots(instances, "rack_a", u_count=42)
    for u in (9, 10, 17, 18, 25, 26):
        assert u in empty


def test_repeat_along_count_and_pitch() -> None:
    program = SceneProgram(name="repeat_test")
    program.place(
        "rack_shell",
        "seed",
        location=(0.0, 0.0, 0.0),
        params={"u_count": 42, "frame_width_m": 0.6, "depth_m": 1.0},
    )
    program.repeat_along("seed", axis="y", count=24, pitch_m=0.6, id_prefix="rack")
    instances = evaluate_program(program)
    racks = [inst for inst in instances if inst.archetype == "rack_shell"]
    assert len(racks) == 24
    ys = sorted(inst.transform.location[1] for inst in racks)
    for i in range(1, len(ys)):
        assert abs(ys[i] - ys[i - 1] - 0.6) < 1e-9


def test_vary_state_changes_state_not_mesh() -> None:
    ok = StatusLightMatrix({"cols": 4, "rows": 2, "status": "ok"})
    warn = StatusLightMatrix({"cols": 4, "rows": 2, "status": "warn"})
    assert ok.params["status"] != warn.params["status"]
    # Geometry fingerprint ignores material/state-only fields.
    assert mesh_fingerprint(ok.build()) == mesh_fingerprint(warn.build())
    assert mesh_fingerprint(ok.build(), include_state=True) != mesh_fingerprint(
        warn.build(), include_state=True
    )

    program = SceneProgram(name="state_test")
    program.place(
        "status_light_matrix",
        "s1",
        params={"cols": 4, "rows": 2, "status": "ok"},
        state={"status": "ok"},
        tags=["status"],
    )
    program.place(
        "status_light_matrix",
        "s2",
        location=(1.0, 0.0, 0.0),
        params={"cols": 4, "rows": 2, "status": "ok"},
        state={"status": "ok"},
        tags=["status"],
    )
    program.vary_state("s2", {"status": "fault"})
    instances = evaluate_program(program)
    by_id = {inst.instance_id: inst for inst in instances}
    assert by_id["s1"].state["status"] == "ok"
    assert by_id["s2"].state["status"] == "fault"
    # mesh_key must remain identical so instancing stays valid.
    assert by_id["s1"].mesh_key == by_id["s2"].mesh_key
    assert_state_does_not_split_meshes(instances, archetype="status_light_matrix")
    plan = build_instancing_plan(instances)
    assert plan.unique_mesh_count() == 1
    assert plan.instance_count() == 2


def test_lod_identity_catches_intentionally_broken_far() -> None:
    arch = default_library().create("server_drawer", {"u_height": 2})
    parts = arch.build()
    variants = generate_lods(parts)
    # Healthy LODs: bbox/silhouette pass; part loss on far may be reported.
    healthy = check_lod_identity("server_drawer", variants, require_all_parts=False)
    assert healthy.bbox_ok
    assert healthy.silhouette_ok

    # Inject a deliberately broken far LOD that shrinks geometry and drops parts.
    broken_parts = intentionally_broken_far_lod(parts)
    variants["far"].parts = broken_parts
    from blender_vision.procedural.archetype import measure_parts_bounds
    from blender_vision.procedural.lod import part_name_set

    variants["far"].dimensions = measure_parts_bounds(broken_parts)
    variants["far"].part_names = part_name_set(broken_parts)
    report = check_lod_identity(
        "server_drawer",
        variants,
        bbox_tolerance_m=0.02,
        require_all_parts=True,
    )
    assert not report.passed
    assert report.lost_parts or not report.bbox_ok
    assert any("lose" in note or "bbox" in note for note in report.notes) or report.lost_parts


def test_scene_graph_record_validates() -> None:
    compiled = build_flagship_scene(rack_count_per_side=4, aisle_length_m=6.0)
    record = compiled.record
    validate_record(record)
    verified = verify_payload(record.to_dict())
    assert verified.digest == record.digest
    assert record.scene_name == "datacenter_flagship"
    assert record.instance_count() >= 4 * 2  # racks both sides at minimum
    assert compiled.plan.unique_mesh_count() >= 1
    assert compiled.plan.instance_count() == len(compiled.instances)
    # Path grammar present.
    kinds = {op["kind"] for op in record.grammar}
    assert "place" in kinds
    assert "junction" in kinds or "turn" in kinds


def test_server_and_gpu_drawers_have_structural_parts() -> None:
    server = default_library().create("server_drawer", {"u_height": 2, "drive_bays": 8})
    names = set(server.part_names())
    for required in (
        "chassis",
        "front_bezel",
        "front_vent_field",
        "handle_left",
        "handle_right",
        "drive_bay_01",
        "rear_psu_1",
    ):
        assert required in names, f"server_drawer missing {required}"

    gpu = default_library().create("gpu_drawer", {"u_height": 4, "gpu_count": 8})
    gnames = set(gpu.part_names())
    for required in (
        "chassis",
        "front_bezel",
        "front_vent_field",
        "gpu_bay_01",
        "gpu_heatsink_01",
        "rear_psu_1",
    ):
        assert required in gnames, f"gpu_drawer missing {required}"


def test_compile_scene_from_json_roundtrip() -> None:
    program = SceneProgram(name="json_roundtrip")
    program.place("floor_tile", "t0", params={"size_m": 0.6})
    program.repeat_along("t0", axis="x", count=3, pitch_m=0.6)
    payload = program.to_dict()
    restored = SceneProgram.from_dict(payload)
    compiled = compile_scene(restored)
    assert compiled.plan.instance_count() == 3
    verify_payload(compiled.record.to_dict())


@BLENDER_GATE
def test_blender_emit_archetype_dimensions(tmp_path: Path) -> None:
    from blender_vision.procedural.emit import emit_archetype

    result = emit_archetype("rack_shell", tmp_path / "rack", params={"u_count": 42})
    assert result.blend_path.is_file()
    assert result.glb_path.is_file()
    metrics = result.metrics
    arch_metrics = metrics.get("archetypes", {})
    near = arch_metrics.get("rack_shell:near") or next(iter(arch_metrics.values()))
    size = near["bbox_size"]
    declared = default_library().create("rack_shell", {"u_count": 42}).declared_dimensions()
    assert abs(size[0] - declared.width_m) < 0.002
    assert abs(size[1] - declared.depth_m) < 0.002
    assert abs(size[2] - declared.height_m) < 0.002
    assert near["triangle_count"] > 0


@BLENDER_GATE
def test_blender_emit_flagship_scene(tmp_path: Path) -> None:
    from blender_vision.procedural.emit import emit_scene_plan

    compiled = build_flagship_scene(rack_count_per_side=3, aisle_length_m=4.0)
    result = emit_scene_plan(
        compiled.plan,
        tmp_path / "scene",
        renders=[
            {
                "filename": "view_entry.png",
                "location": [0.0, -4.0, 1.6],
                "target": [0.0, 2.0, 1.2],
            }
        ],
        timeout_seconds=600,
    )
    assert result.blend_path.is_file()
    assert result.glb_path.is_file()
    assert result.metrics["instances"] == compiled.plan.instance_count()
    assert result.metrics["unique_meshes"] == compiled.plan.unique_mesh_count()
    assert result.render_paths
    assert result.render_paths[0].is_file()
