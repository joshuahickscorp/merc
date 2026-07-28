from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bmesh
import bpy
from mathutils import Vector


REQUIRED = {
    "base",
    "outer_shell",
    "glass_core",
    "eclipse_disk",
    "acoustic_membrane",
    "thermal_grille",
    "rotary_control",
    "braided_cable",
    "internal_frame",
    "logic_board",
    "left_driver",
    "right_driver",
}
ANIMATED = {
    "outer_shell",
    "glass_core",
    "eclipse_disk",
    "acoustic_membrane",
    "internal_frame",
    "logic_board",
    "left_driver",
    "right_driver",
}
MATERIALS = {
    "MAT_BLACK_ANODIZED_ALUMINUM",
    "MAT_FROSTED_TRANSLUCENT_GLASS",
    "MAT_WARM_EMISSIVE_CERAMIC",
    "MAT_GRAPHITE_TENSIONED_TEXTILE",
    "MAT_MACHINED_ALUMINUM",
}
CAMERAS = {
    "front": {
        "lens": 58.0,
        "location": (0.0, -760.0, 184.0),
        "target": (0.0, 0.0, 176.0),
    },
    "rear": {
        "lens": 58.0,
        "location": (0.0, 760.0, 184.0),
        "target": (0.0, 0.0, 176.0),
    },
    "left": {
        "lens": 62.0,
        "location": (-650.0, -300.0, 190.0),
        "target": (0.0, 0.0, 176.0),
    },
    "right": {
        "lens": 62.0,
        "location": (650.0, -300.0, 190.0),
        "target": (0.0, 0.0, 176.0),
    },
    "top": {
        "lens": 64.0,
        "location": (0.0, -30.0, 820.0),
        "target": (0.0, 0.0, 145.0),
    },
    "hero": {
        "lens": 67.0,
        "location": (470.0, -650.0, 390.0),
        "target": (0.0, 0.0, 168.0),
    },
}


def arguments() -> tuple[Path, Path | None]:
    values = sys.argv[sys.argv.index("--") + 1 :] if "--" in sys.argv else []
    if not values:
        raise SystemExit("output JSON path is required")
    return Path(values[0]).resolve(), Path(values[1]).resolve() if len(values) > 1 else None


def semantic_objects() -> dict[str, bpy.types.Object]:
    return {
        str(obj.get("semantic_id") or obj.name): obj
        for obj in bpy.data.objects
        if str(obj.get("semantic_id") or obj.name) in REQUIRED
    }


def world_bounds(objects: list[bpy.types.Object]) -> tuple[Vector, Vector]:
    points = [
        obj.matrix_world @ Vector(corner)
        for obj in objects
        if obj.type == "MESH"
        for corner in obj.bound_box
    ]
    return (
        Vector(tuple(min(point[index] for point in points) for index in range(3))),
        Vector(tuple(max(point[index] for point in points) for index in range(3))),
    )


def look_at(camera: bpy.types.Object, target: tuple[float, float, float]) -> None:
    camera.rotation_euler = (
        Vector(target) - camera.location
    ).to_track_quat("-Z", "Y").to_euler()


def render_public(output: Path) -> None:
    output.mkdir(parents=True, exist_ok=True)
    scene = bpy.context.scene
    scene.frame_set(1)
    scene.render.engine = "BLENDER_WORKBENCH"
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGBA"
    scene.render.image_settings.color_depth = "8"
    scene.render.film_transparent = True
    scene.render.resolution_x = 512
    scene.render.resolution_y = 512
    scene.render.resolution_percentage = 100
    scene.display.shading.light = "FLAT"
    scene.display.shading.color_type = "SINGLE"
    scene.display.shading.single_color = (1.0, 1.0, 1.0)
    scene.display.shading.show_shadows = False
    scene.display.shading.show_cavity = False
    for label, record in CAMERAS.items():
        data = bpy.data.cameras.new(f"PUBLIC_CHECK_{label}")
        data.lens = record["lens"]
        data.sensor_width = 36.0
        data.clip_start = 1.0
        data.clip_end = 4000.0
        camera = bpy.data.objects.new(f"PUBLIC_CHECK_{label}", data)
        bpy.context.scene.collection.objects.link(camera)
        camera.location = record["location"]
        look_at(camera, record["target"])
        scene.camera = camera
        scene.render.filepath = str(output / f"{label}.png")
        bpy.ops.render.render(write_still=True)
        bpy.data.objects.remove(camera, do_unlink=True)
        bpy.data.cameras.remove(data)


def main() -> None:
    report_path, render_path = arguments()
    scene = bpy.context.scene
    scene.frame_set(1)
    semantic = semantic_objects()
    missing = sorted(REQUIRED - semantic.keys())
    mesh_issues: dict[str, dict[str, object]] = {}
    for name, obj in semantic.items():
        if obj.type != "MESH":
            mesh_issues[name] = {"type": obj.type}
            continue
        bm = bmesh.new()
        bm.from_mesh(obj.data)
        non_manifold = sum(not edge.is_manifold for edge in bm.edges)
        bm.free()
        issue = {
            "uv_layers": len(obj.data.uv_layers),
            "non_manifold_edges": non_manifold,
            "negative_scale": any(value < 0 for value in obj.scale),
            "finite_normals": all(
                all(math.isfinite(float(component)) for component in polygon.normal)
                for polygon in obj.data.polygons
            ),
        }
        if (
            issue["uv_layers"] == 0
            or issue["non_manifold_edges"]
            or issue["negative_scale"]
            or not issue["finite_normals"]
        ):
            mesh_issues[name] = issue
    primary = [
        semantic[name]
        for name in (
            "base",
            "outer_shell",
            "glass_core",
            "eclipse_disk",
            "acoustic_membrane",
            "thermal_grille",
            "rotary_control",
        )
        if name in semantic
    ]
    lower, upper = world_bounds(primary)
    dimensions = [float(value) for value in upper - lower]
    animated: list[str] = []
    for name in ANIMATED:
        obj = semantic.get(name)
        action = obj.animation_data.action if obj and obj.animation_data else None
        if action and any(curve.data_path == "location" for curve in action.fcurves):
            animated.append(name)
    root = bpy.data.objects.get("NOCTURNE_ONE_ROOT")
    material_names = {material.name for material in bpy.data.materials}
    passed = (
        not missing
        and not mesh_issues
        and root is not None
        and all(semantic[name].parent == root for name in REQUIRED)
        and set(animated) == ANIMATED
        and MATERIALS <= material_names
        and all(
            abs(observed - expected) / expected <= 0.01
            for observed, expected in zip(dimensions, (320.0, 180.0, 360.0), strict=True)
        )
    )
    report = {
        "schema_version": "1",
        "passed": passed,
        "required_parts": sorted(REQUIRED),
        "observed_parts": sorted(semantic),
        "missing_parts": missing,
        "mesh_issues": mesh_issues,
        "root_present": root is not None,
        "unparented_parts": (
            sorted(name for name in REQUIRED if semantic.get(name) and semantic[name].parent != root)
            if root
            else sorted(REQUIRED)
        ),
        "animated_parts": sorted(animated),
        "materials": sorted(material_names),
        "primary_dimensions_mm": dimensions,
    }
    report_path.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n")
    if render_path is not None:
        render_public(render_path)
    if not passed:
        raise SystemExit("scene contract check failed")


if __name__ == "__main__":
    main()
