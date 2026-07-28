"""Blender headless renderer for reconstruction ensemble targets.

Run as:
  Blender --background --python render_targets.py -- --output DIR --target NAME
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path


def _parse_args(argv: list[str]) -> dict[str, str]:
    args = {"output": "artifacts/v2/reconstruction", "target": "all", "views": "24"}
    argv = argv[argv.index("--") + 1:] if "--" in argv else argv[1:]
    i = 0
    while i < len(argv):
        if argv[i] in {"--output", "--target", "--views"} and i + 1 < len(argv):
            args[argv[i].lstrip("-")] = argv[i + 1]
            i += 2
        else:
            i += 1
    return args


def clear_scene() -> None:
    import bpy

    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for block in bpy.data.meshes:
        bpy.data.meshes.remove(block)
    for block in bpy.data.materials:
        bpy.data.materials.remove(block)


def add_camera(name: str, location: tuple[float, float, float], target=(0.0, 0.0, 0.0)):
    import bpy
    from mathutils import Vector

    cam_data = bpy.data.cameras.new(name)
    cam_data.lens = 50
    cam_obj = bpy.data.objects.new(name, cam_data)
    bpy.context.scene.collection.objects.link(cam_obj)
    cam_obj.location = location
    direction = Vector(target) - Vector(location)
    cam_obj.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()
    return cam_obj


def make_material(name: str, colour: tuple[float, float, float]):
    import bpy

    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF")
    if bsdf:
        bsdf.inputs["Base Color"].default_value = (*colour, 1.0)
        bsdf.inputs["Roughness"].default_value = 0.45
    return mat


def build_calibration() -> dict:
    import bpy

    clear_scene()
    bpy.ops.mesh.primitive_uv_sphere_add(radius=0.5, location=(0, 0, 0), segments=48, ring_count=24)
    sphere = bpy.context.active_object
    sphere.name = "CalibrationSphere"
    sphere.data.materials.append(make_material("cal_red", (0.85, 0.15, 0.12)))
    # Checker-like second object for features: small cubes on the sphere equator.
    for i in range(8):
        ang = 2 * math.pi * i / 8
        bpy.ops.mesh.primitive_cube_add(
            size=0.12, location=(0.55 * math.cos(ang), 0.55 * math.sin(ang), 0.0)
        )
        cube = bpy.context.active_object
        cube.name = f"Marker{i}"
        colour = (0.1, 0.1, 0.1) if i % 2 == 0 else (0.95, 0.95, 0.9)
        cube.data.materials.append(make_material(f"m{i}", colour))
    return {
        "target_id": "calibration_sphere",
        "kind": "sphere",
        "radius_m": 0.5,
        "truth_mesh": "truth.obj",
    }


def build_consumer_remote() -> dict:
    import bpy

    clear_scene()
    # Body
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0, 0, 0))
    body = bpy.context.active_object
    body.name = "RemoteBody"
    body.scale = (0.18, 0.06, 0.02)
    bpy.ops.object.transform_apply(scale=True)
    body.data.materials.append(make_material("body", (0.12, 0.12, 0.14)))
    # Buttons
    for i, x in enumerate((-0.08, -0.03, 0.03, 0.08)):
        for j, y in enumerate((-0.02, 0.02)):
            bpy.ops.mesh.primitive_cylinder_add(
                radius=0.012, depth=0.01, location=(x, y, 0.025)
            )
            btn = bpy.context.active_object
            btn.name = f"Button{i}_{j}"
            btn.data.materials.append(
                make_material(f"btn{i}{j}", (0.8, 0.2 + 0.1 * i, 0.15 + 0.1 * j))
            )
    return {
        "target_id": "consumer_remote",
        "kind": "remote",
        "bounds_m": {"min": [-0.18, -0.06, -0.02], "max": [0.18, 0.06, 0.03]},
        "truth_mesh": "truth.obj",
    }


def build_rack_module() -> dict:
    import bpy

    clear_scene()
    # 1U-ish rack module shell
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0, 0, 0))
    shell = bpy.context.active_object
    shell.name = "RackShell"
    shell.scale = (0.225, 0.35, 0.022)
    bpy.ops.object.transform_apply(scale=True)
    shell.data.materials.append(make_material("rack", (0.25, 0.28, 0.32)))
    # Front vents / features
    for i in range(6):
        bpy.ops.mesh.primitive_cube_add(
            size=1.0, location=(-0.18 + i * 0.07, -0.34, 0.0)
        )
        vent = bpy.context.active_object
        vent.scale = (0.025, 0.01, 0.015)
        bpy.ops.object.transform_apply(scale=True)
        vent.data.materials.append(make_material(f"vent{i}", (0.05, 0.05, 0.05)))
    # Handle
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0.2, -0.36, 0.0))
    handle = bpy.context.active_object
    handle.scale = (0.015, 0.03, 0.01)
    bpy.ops.object.transform_apply(scale=True)
    handle.data.materials.append(make_material("handle", (0.7, 0.7, 0.72)))
    return {
        "target_id": "datacentre_rack_module",
        "kind": "rack",
        "bounds_m": {"min": [-0.225, -0.37, -0.022], "max": [0.225, 0.35, 0.022]},
        "truth_mesh": "truth.obj",
    }


def export_truth(path: Path) -> None:
    import bpy

    path.parent.mkdir(parents=True, exist_ok=True)
    # Join all meshes for a single truth export.
    bpy.ops.object.select_all(action="DESELECT")
    for obj in bpy.data.objects:
        if obj.type == "MESH":
            obj.select_set(True)
            bpy.context.view_layer.objects.active = obj
    if bpy.context.selected_objects:
        bpy.ops.object.duplicate()
        bpy.ops.object.join()
        joined = bpy.context.active_object
        joined.name = "TruthJoined"
    bpy.ops.wm.obj_export(
        filepath=str(path),
        export_selected_objects=True,
        export_materials=False,
        forward_axis="Y",
        up_axis="Z",
    )


def render_views(output: Path, view_count: int) -> list[dict]:
    import bpy

    scene = bpy.context.scene
    if hasattr(bpy.types, "BLENDER_EEVEE_NEXT"):
        scene.render.engine = "BLENDER_EEVEE_NEXT"
    else:
        scene.render.engine = "BLENDER_EEVEE"
    # Fall back safely.
    try:
        scene.render.engine = "BLENDER_EEVEE"
    except Exception:
        scene.render.engine = "CYCLES"
    scene.render.resolution_x = 480
    scene.render.resolution_y = 480
    scene.render.image_settings.file_format = "PNG"
    scene.render.film_transparent = False

    # Lighting
    light_data = bpy.data.lights.new(name="Key", type="AREA")
    light_data.energy = 200
    light_obj = bpy.data.objects.new("Key", light_data)
    scene.collection.objects.link(light_obj)
    light_obj.location = (1.5, -1.5, 2.0)
    fill = bpy.data.lights.new(name="Fill", type="AREA")
    fill.energy = 80
    fill_obj = bpy.data.objects.new("Fill", fill)
    scene.collection.objects.link(fill_obj)
    fill_obj.location = (-1.5, 1.0, 1.5)

    images_dir = output / "images"
    images_dir.mkdir(parents=True, exist_ok=True)
    meta = []
    radius = 1.6
    for i in range(view_count):
        elev = 0.25 + 0.15 * math.sin(i * 0.7)
        azim = 2 * math.pi * i / view_count
        loc = (
            radius * math.cos(azim) * math.cos(elev),
            radius * math.sin(azim) * math.cos(elev),
            radius * math.sin(elev) + 0.15,
        )
        cam = add_camera(f"Cam{i:03d}", loc)
        scene.camera = cam
        path = images_dir / f"view_{i:03d}.png"
        scene.render.filepath = str(path)
        bpy.ops.render.render(write_still=True)
        meta.append(
            {
                "name": path.name,
                "location": list(loc),
                "lens_mm": 50,
                "resolution": [480, 480],
            }
        )
        # Remove camera to keep scene clean
        bpy.data.objects.remove(cam, do_unlink=True)
    return meta


def main() -> None:
    args = _parse_args(sys.argv)
    output_root = Path(args["output"])
    view_count = int(args["views"])
    targets = {
        "calibration": build_calibration,
        "consumer": build_consumer_remote,
        "rack": build_rack_module,
    }
    selected = list(targets) if args["target"] == "all" else [args["target"]]
    receipt = {"targets": {}}
    for name in selected:
        builder = targets[name]
        meta = builder()
        out = output_root / name
        out.mkdir(parents=True, exist_ok=True)
        truth_path = out / "truth.obj"
        export_truth(truth_path)
        views = render_views(out, view_count)
        meta["views"] = views
        meta["image_dir"] = str(out / "images")
        meta["truth_mesh"] = str(truth_path)
        (out / "target.json").write_text(json.dumps(meta, indent=2) + "\n", encoding="utf-8")
        receipt["targets"][name] = meta
        print(f"RENDERED {name}: {len(views)} views -> {out}")
    (output_root / "render_receipt.json").write_text(
        json.dumps(receipt, indent=2) + "\n", encoding="utf-8"
    )


if __name__ == "__main__":
    main()
