"""Generate the CC0 synthetic technical calibration scene in Blender 4.2+."""

from __future__ import annotations

import math
import sys
from pathlib import Path

import bpy


def material(name: str, color: tuple[float, float, float, float], metallic: float = 0.0):
    value = bpy.data.materials.new(name)
    value.diffuse_color = color
    value.metallic = metallic
    value.roughness = 0.34
    return value


def cube(name: str, location: tuple[float, float, float], dimensions: tuple[float, float, float]):
    bpy.ops.mesh.primitive_cube_add(location=location)
    value = bpy.context.object
    value.name = name
    value.dimensions = dimensions
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    return value


def cylinder(
    name: str,
    location: tuple[float, float, float],
    radius: float,
    depth: float,
    rotation: tuple[float, float, float] = (math.pi / 2.0, 0.0, 0.0),
):
    bpy.ops.mesh.primitive_cylinder_add(
        vertices=32, radius=radius, depth=depth, location=location, rotation=rotation
    )
    value = bpy.context.object
    value.name = name
    return value


def build(output: Path) -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for collection in (bpy.data.materials, bpy.data.cameras, bpy.data.lights):
        for item in list(collection):
            collection.remove(item)
    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.unit_settings.length_unit = "MILLIMETERS"
    scene.unit_settings.scale_length = 0.001
    scene["bvmcp_benchmark_id"] = "synthetic-technical-calibration-v1"
    scene["bvmcp_ground_truth_dimensions_mm"] = [120.0, 80.0, 40.0]

    aluminum = material("Calibration Aluminum", (0.22, 0.25, 0.29, 1.0), metallic=0.85)
    dark = material("Calibration Recess", (0.015, 0.018, 0.022, 1.0))
    accent = material("Calibration Accent", (0.05, 0.24, 0.42, 1.0), metallic=0.35)
    body = cube("calibration-body", (0.0, 0.0, 20.0), (120.0, 80.0, 40.0))
    body.data.materials.append(aluminum)
    bevel = body.modifiers.new("ground-truth-4mm-bevel", "BEVEL")
    bevel.width = 4.0
    bevel.segments = 4
    body["bvmcp_component_id"] = "body"
    body["bvmcp_component_type"] = "Body"

    port_specs = [(-32.0, 12.0, 14.0, 8.0), (-5.0, 10.0, 18.0, 7.0), (28.0, 9.0, 12.0, 9.0)]
    for index, (x, z, width, height) in enumerate(port_specs, start=1):
        port = cube(f"front-port-{index}", (x, -40.6, z), (width, 1.2, height))
        port.data.materials.append(dark)
        port["bvmcp_feature_type"] = "USB-C" if index != 2 else "HDMI"
        port["bvmcp_component_id"] = "front-panel"

    fan = cylinder("rear-fan-ring", (0.0, 40.7, 20.0), 17.0, 1.4)
    fan.data.materials.append(dark)
    fan["bvmcp_feature_type"] = "fan ring"
    fan["bvmcp_ground_truth_diameter_mm"] = 34.0

    for row in range(5):
        for column in range(7):
            hole = cylinder(
                f"grille-hole-{row:02d}-{column:02d}",
                (-45.0 + column * 15.0, 40.8, 5.0 + row * 7.0),
                1.8,
                1.6,
            )
            hole.data.materials.append(dark)
            hole["bvmcp_feature_type"] = "hole"

    for index, (x, z) in enumerate(((-52.0, 4.0), (52.0, 4.0), (-52.0, 36.0), (52.0, 36.0))):
        screw = cylinder(f"rear-screw-{index + 1}", (x, 40.9, z), 2.2, 1.8)
        screw.data.materials.append(accent)
        screw["bvmcp_feature_type"] = "screw"

    output.parent.mkdir(parents=True, exist_ok=True)
    bpy.ops.wm.save_as_mainfile(filepath=str(output), check_existing=False)


if __name__ == "__main__":
    arguments = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    if len(arguments) != 1:
        raise SystemExit("usage: blender --background --python create_scene.py -- OUTPUT.blend")
    build(Path(arguments[0]).expanduser().resolve())
