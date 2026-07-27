"""Beat coverage receipts and second-aisle population (Phase P art direction)."""

from __future__ import annotations

import math

import numpy as np
import pytest

from blender_vision.cinematic.path import compose_flagship_datacentre_path
from blender_vision.cinematic.replay import replay_camera_state
from blender_vision.cinematic.textsafe import ZONE_RECTS, TextZone, evaluate_text_safe
from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.ocular.beat_coverage import (
    DRAWER_ARCHETYPES,
    RACK_ARCHETYPES,
    BeatMinimums,
    flagship_beat_minimums,
    frustum_cull_instances,
    measure_beat_coverage,
    measure_frame_pixels,
    measure_text_safe_contrast,
    per_beat_instance_counts,
    synthetic_empty_frame,
    synthetic_populated_frame,
)
from blender_vision.procedural.grammar import InstanceRef, evaluate_program
from blender_vision.procedural.instancing import build_instancing_plan
from blender_vision.procedural.scene import build_flagship_scene
from blender_vision.v2.authority import BLENDER_WORLD
from blender_vision.v2.records import TamperError


def _rack_at(
    instance_id: str,
    location: tuple[float, float, float],
    *,
    tags: list[str] | None = None,
) -> InstanceRef:
    from blender_vision.procedural.archetype import Transform3D

    return InstanceRef(
        instance_id=instance_id,
        archetype="rack_shell",
        params={"u_count": 42, "frame_width_m": 0.6, "depth_m": 1.0},
        transform=Transform3D(location=location),
        tags=list(tags or ["rack"]),
    )


def _drawer_at(
    instance_id: str,
    location: tuple[float, float, float],
    *,
    parent_id: str,
) -> InstanceRef:
    from blender_vision.procedural.archetype import Transform3D

    return InstanceRef(
        instance_id=instance_id,
        archetype="gpu_drawer",
        params={"u_height": 4, "gpu_count": 8, "depth_m": 0.85},
        transform=Transform3D(location=location),
        parent_id=parent_id,
        tags=["equipment", "gpu"],
        u_start=3,
        u_end=6,
    )


def test_geometry_in_frustum_but_empty_render_fails() -> None:
    """Frustum full of racks + near-black render must fail coverage gates."""
    instances = [
        _rack_at("rack_a", ( -1.1, 4.0, 0.0)),
        _rack_at("rack_b", ( 1.1, 4.0, 0.0)),
        _drawer_at("gpu_a", (-1.1, 4.0, 0.5), parent_id="rack_a"),
        _drawer_at("gpu_b", (-1.1, 4.0, 0.7), parent_id="rack_a"),
        _drawer_at("gpu_c", (1.1, 4.0, 0.5), parent_id="rack_b"),
        _drawer_at("gpu_d", (1.1, 4.0, 0.7), parent_id="rack_b"),
        _drawer_at("gpu_e", (-1.1, 4.0, 0.9), parent_id="rack_a"),
        _drawer_at("gpu_f", (1.1, 4.0, 0.9), parent_id="rack_b"),
    ]
    cam = (0.0, 1.0, 1.5)
    target = (0.0, 6.0, 1.4)
    hits = frustum_cull_instances(instances, camera_position=cam, camera_target=target)
    assert len(hits) >= 4, "test setup requires geometry in frustum"

    empty = synthetic_empty_frame()
    receipt = measure_beat_coverage(
        beat_id="04",
        beat_label="EXECUTION",
        scroll=0.48,
        camera_position=cam,
        camera_target=target,
        instances=instances,
        frame=empty,
        text_zone="centre",
        minimums=flagship_beat_minimums("04"),
    )
    assert not receipt.passed
    assert receipt.frustum_instance_count >= 4
    assert receipt.non_background_pixel_fraction < 0.05
    assert receipt.visible_instances == 0
    assert any("frustum_render_mismatch" in f or "non_background" in f for f in receipt.failures)


def test_deliberately_emptied_beat_fails() -> None:
    """A beat with no instances and a blank frame must fail declared minimums."""
    empty = synthetic_empty_frame()
    receipt = measure_beat_coverage(
        beat_id="06",
        beat_label="VERIFY",
        scroll=0.74,
        camera_position=(3.0, 13.2, 1.5),
        camera_target=(7.0, 13.2, 1.4),
        instances=[],
        frame=empty,
        text_zone="right_upper",
        minimums=flagship_beat_minimums("06"),
    )
    assert not receipt.passed
    assert receipt.frustum_instance_count == 0
    assert receipt.visible_racks == 0
    assert receipt.visible_drawers == 0
    assert any("frustum_instances" in f or "visible_" in f for f in receipt.failures)


def test_populated_beat_passes() -> None:
    """A corridor of racks + structured render meets main-aisle minimums."""
    instances: list[InstanceRef] = []
    for i, y in enumerate((2.0, 3.0, 4.0, 5.0, 6.0, 7.0)):
        instances.append(_rack_at(f"rack_L_{i}", (-1.1, y, 0.0), tags=["rack", "left"]))
        instances.append(_rack_at(f"rack_R_{i}", (1.1, y, 0.0), tags=["rack", "right"]))
        for d in range(4):
            instances.append(
                _drawer_at(
                    f"gpu_L_{i}_{d}",
                    (-1.1, y, 0.3 + 0.15 * d),
                    parent_id=f"rack_L_{i}",
                )
            )
            instances.append(
                _drawer_at(
                    f"gpu_R_{i}_{d}",
                    (1.1, y, 0.3 + 0.15 * d),
                    parent_id=f"rack_R_{i}",
                )
            )
    frame = synthetic_populated_frame()
    # Darken declared text zone further for contrast gate.
    h, w = frame.shape[:2]
    zone = ZONE_RECTS[TextZone.CENTRE]
    x0 = int(zone[0] * (w - 1))
    y0 = int(zone[1] * (h - 1))
    x1 = int(zone[2] * (w - 1)) + 1
    y1 = int(zone[3] * (h - 1)) + 1
    frame[y0:y1, x0:x1, :] = 0.04

    receipt = measure_beat_coverage(
        beat_id="03",
        beat_label="DISPATCH",
        scroll=0.36,
        camera_position=(0.0, 1.5, 1.5),
        camera_target=(0.0, 8.0, 1.4),
        instances=instances,
        frame=frame,
        text_zone="edge",
        minimums=BeatMinimums(
            min_visible_instances=6,
            min_visible_racks=2,
            min_visible_drawers=6,
            min_non_background_fraction=0.12,
            min_depth_spread=0.3,
            min_text_safe_contrast=2.5,
            min_frustum_instances=6,
        ),
    )
    assert receipt.passed, receipt.failures
    assert receipt.visible_racks >= 2
    assert receipt.visible_drawers >= 6
    assert receipt.non_background_pixel_fraction >= 0.12
    assert receipt.digest
    receipt.verify()


def test_unique_mesh_count_stays_bounded_as_instances_rise() -> None:
    """Second-aisle population grows instances without exploding unique meshes."""
    small = build_flagship_scene(
        rack_count_per_side=4,
        aisle_length_m=6.0,
        second_rack_count_per_side=0,
    )
    full = build_flagship_scene()
    assert full.plan.instance_count() > small.plan.instance_count()
    # Unique meshes must stay near the archetype library size, not track instances.
    assert full.plan.unique_mesh_count() < 40
    assert full.plan.unique_mesh_count() <= small.plan.unique_mesh_count() + 8

    counts = per_beat_instance_counts(full.instances)
    assert counts["second_aisle"]["instances"] >= 100
    assert counts["second_aisle"]["racks"] >= 8
    assert counts["second_aisle"]["drawers"] >= 40
    # Global high count is not enough on its own — second aisle must carry its share.
    assert counts["second_aisle"]["instances"] > 8


def test_text_safe_contrast_measured_from_pixels_not_assumed() -> None:
    dark = np.full((90, 160, 3), 0.05, dtype=np.float64)
    bright = np.full((90, 160, 3), 0.85, dtype=np.float64)
    # Paint a noisy mid-tone into the centre zone of the bright frame so mean
    # contrast against white text collapses.
    zone = ZONE_RECTS[TextZone.CENTRE]
    h, w = bright.shape[:2]
    x0 = int(zone[0] * (w - 1))
    y0 = int(zone[1] * (h - 1))
    x1 = int(zone[2] * (w - 1)) + 1
    y1 = int(zone[3] * (h - 1)) + 1
    bright[y0:y1, x0:x1, :] = 0.92

    dark_m = measure_text_safe_contrast(dark, zone="centre", text_luminance=1.0)
    bright_m = measure_text_safe_contrast(bright, zone="centre", text_luminance=1.0)
    assert dark_m["contrast_ratio"] > bright_m["contrast_ratio"]
    assert dark_m["contrast_ratio"] >= 4.5
    assert bright_m["contrast_ratio"] < 2.0

    # evaluate_text_safe path agrees (same pixels, not a constant).
    via_zone = evaluate_text_safe(dark, zone=TextZone.CENTRE, text_luminance=1.0)
    assert abs(via_zone.contrast_ratio - dark_m["contrast_ratio"]) < 1e-9


def test_receipt_seals_and_verifies() -> None:
    frame = synthetic_populated_frame()
    # Spread instances in depth so depth_spread is non-trivial.
    instances = [
        _rack_at("r1", (-1.1, 3.0, 0.0)),
        _rack_at("r2", (1.1, 5.5, 0.0)),
        _drawer_at("d1", (-1.1, 3.0, 0.5), parent_id="r1"),
        _drawer_at("d2", (-1.1, 3.5, 0.7), parent_id="r1"),
        _drawer_at("d3", (1.1, 5.5, 0.5), parent_id="r2"),
        _drawer_at("d4", (1.1, 6.0, 0.7), parent_id="r2"),
        _drawer_at("d5", (-1.1, 4.0, 0.9), parent_id="r1"),
        _drawer_at("d6", (1.1, 7.0, 0.9), parent_id="r2"),
    ]
    receipt = measure_beat_coverage(
        beat_id="02",
        beat_label="INFERENCE",
        scroll=0.24,
        camera_position=(0.0, 1.0, 1.5),
        camera_target=(0.0, 6.0, 1.4),
        instances=instances,
        frame=frame,
        text_zone="right_upper",
        minimums=BeatMinimums(
            min_visible_instances=2,
            min_visible_racks=1,
            min_visible_drawers=2,
            min_non_background_fraction=0.08,
            min_depth_spread=0.1,
            min_text_safe_contrast=1.5,
            min_frustum_instances=2,
        ),
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY.value,
    )
    assert receipt.RECORD_KIND == "ocular.beat-coverage"
    assert receipt.digest
    assert receipt.frame.up_axis == BLENDER_WORLD.up_axis
    receipt.verify()

    # Tamper detection.
    receipt.visible_racks = 999
    with pytest.raises(TamperError):
        receipt.verify()


def test_flagship_second_aisle_populated_and_path_clears_solids() -> None:
    scene = build_flagship_scene()
    counts = per_beat_instance_counts(scene.instances)
    assert counts["second_aisle"]["racks"] >= 8
    assert counts["total"]["instances"] > 464  # was 464 when second aisle was empty

    path = compose_flagship_datacentre_path()
    assert len(path.beats) == 9
    assert path.beat_coverage_gaps() == []
    # Mid-second-aisle camera must sit in the clear volume (not inside a rack).
    state = replay_camera_state(path, 0.80)
    x, y, z = state.position
    assert abs(y - 13.2) < 0.35
    assert 1.5 < x < 8.0
    assert 1.2 < z < 1.7


def test_measure_frame_pixels_rejects_assumed_constants() -> None:
    empty = synthetic_empty_frame(luminance=0.02)
    filled = synthetic_populated_frame()
    e = measure_frame_pixels(empty)
    f = measure_frame_pixels(filled)
    assert e["non_background_pixel_fraction"] < 0.05
    assert f["non_background_pixel_fraction"] > e["non_background_pixel_fraction"]
    assert len(f["luminance_histogram"]) == 16
    assert math.isclose(sum(f["luminance_histogram"]), 1.0, rel_tol=1e-6)


def test_no_beat_passes_on_global_counts_alone() -> None:
    """Even with a fully populated scene graph, an empty render fails the beat."""
    scene = build_flagship_scene()
    assert len(scene.instances) > 400
    empty = synthetic_empty_frame()
    path = compose_flagship_datacentre_path()
    state = replay_camera_state(path, 0.74)
    target = state.focus_target or [8.0, 13.2, 1.4]
    receipt = measure_beat_coverage(
        beat_id="06",
        beat_label="VERIFY",
        scroll=0.74,
        camera_position=state.position,
        camera_target=target,
        instances=scene.instances,
        frame=empty,
        text_zone="right_upper",
        minimums=flagship_beat_minimums("06"),
    )
    assert not receipt.passed
    assert any(
        "non_background" in f or "frustum_render_mismatch" in f or "visible_" in f
        for f in receipt.failures
    )


def test_instancing_plan_unique_meshes_stable_under_second_aisle() -> None:
    program = build_flagship_scene().program
    instances = evaluate_program(program)
    plan = build_instancing_plan(instances)
    assert plan.instance_count() == len(instances)
    assert plan.unique_mesh_count() < 40
    # Drawer archetypes must still instance (one mesh key each).
    drawer_keys = {
        inst.mesh_key
        for inst in instances
        if inst.archetype in DRAWER_ARCHETYPES
    }
    assert len(drawer_keys) <= 6
    rack_keys = {inst.mesh_key for inst in instances if inst.archetype in RACK_ARCHETYPES}
    assert len(rack_keys) == 1
