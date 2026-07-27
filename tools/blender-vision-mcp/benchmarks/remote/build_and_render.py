"""Blender-side builder for the Phase O consumer remote fixture.

Run as:
  Blender --background --python build_and_render.py -- --output DIR --views 32

Builds a remote-control-like body with buttons, side seams, and a battery hatch
on the underside, then renders the requested orbit views. The underside is never
aimed at by the camera set (elev > 0), so hatch/compartment stay unobserved.
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path


def _parse_args(argv: list[str]) -> dict[str, str]:
    args = {"output": "artifacts/v2/object-benchmarks/remote/capture", "views": "32"}
    argv = argv[argv.index("--") + 1 :] if "--" in argv else argv[1:]
    i = 0
    while i < len(argv):
        if argv[i] in {"--output", "--views"} and i + 1 < len(argv):
            args[argv[i].lstrip("-")] = argv[i + 1]
            i += 2
        else:
            i += 1
    return args


def clear_scene() -> None:
    import bpy

    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for block in list(bpy.data.meshes):
        bpy.data.meshes.remove(block)
    for block in list(bpy.data.materials):
        bpy.data.materials.remove(block)


def make_material(name: str, colour: tuple[float, float, float], roughness: float = 0.45):
    import bpy

    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF")
    if bsdf:
        bsdf.inputs["Base Color"].default_value = (*colour, 1.0)
        bsdf.inputs["Roughness"].default_value = roughness
    return mat


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


def build_remote() -> dict:
    import bpy

    clear_scene()
    # Body 180 x 60 x 25 mm
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0, 0, 0))
    body = bpy.context.active_object
    body.name = "RemoteBody"
    body.scale = (0.090, 0.030, 0.0125)
    bpy.ops.object.transform_apply(scale=True)
    body.data.materials.append(make_material("body", (0.12, 0.12, 0.14), 0.55))

    # Buttons (4 x 2) on the top face
    for i, x in enumerate((-0.054, -0.018, 0.018, 0.054)):
        for j, y in enumerate((-0.012, 0.012)):
            bpy.ops.mesh.primitive_cylinder_add(
                radius=0.008, depth=0.004, location=(x, y, 0.0145)
            )
            btn = bpy.context.active_object
            btn.name = f"Button{i}_{j}"
            btn.data.materials.append(
                make_material(f"btn{i}{j}", (0.75, 0.20 + 0.05 * i, 0.16 + 0.04 * j), 0.35)
            )

    # Side seams (thin ridges)
    for sign in (-1.0, 1.0):
        bpy.ops.mesh.primitive_cube_add(size=1.0, location=(sign * 0.089, 0.0, 0.0))
        seam = bpy.context.active_object
        seam.name = f"Seam{'R' if sign > 0 else 'L'}"
        seam.scale = (0.0015, 0.027, 0.004)
        bpy.ops.object.transform_apply(scale=True)
        seam.data.materials.append(make_material(f"seam{sign}", (0.18, 0.18, 0.20), 0.6))

    # Battery hatch on the underside (never aimed at by the orbit)
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0.0, 0.0, -0.0125))
    hatch = bpy.context.active_object
    hatch.name = "BatteryHatch"
    hatch.scale = (0.040, 0.018, 0.0015)
    bpy.ops.object.transform_apply(scale=True)
    hatch.data.materials.append(make_material("hatch", (0.08, 0.08, 0.09), 0.7))

    return {
        "target_id": "consumer_remote",
        "kind": "remote",
        "bounds_m": {"min": [-0.090, -0.030, -0.014], "max": [0.090, 0.030, 0.0165]},
        "body_dimensions_mm": [180.0, 60.0, 25.0],
        "truth_mesh": "truth.obj",
        "hidden_surfaces": ["underside", "battery_hatch_outer", "battery_compartment_interior"],
        "claim": (
            "Procedurally constructed consumer-object fixture. Not a claim about "
            "any physical remote control."
        ),
    }


def export_truth(path: Path) -> None:
    import bpy

    path.parent.mkdir(parents=True, exist_ok=True)
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
    try:
        scene.render.engine = "BLENDER_EEVEE"
    except Exception:
        scene.render.engine = "CYCLES"
    scene.render.resolution_x = 256
    scene.render.resolution_y = 256
    scene.render.image_settings.file_format = "PNG"
    scene.render.film_transparent = False

    light_data = bpy.data.lights.new(name="Key", type="AREA")
    light_data.energy = 180
    light_obj = bpy.data.objects.new("Key", light_data)
    scene.collection.objects.link(light_obj)
    light_obj.location = (0.8, -0.8, 1.0)
    fill = bpy.data.lights.new(name="Fill", type="AREA")
    fill.energy = 70
    fill_obj = bpy.data.objects.new("Fill", fill)
    scene.collection.objects.link(fill_obj)
    fill_obj.location = (-0.7, 0.5, 0.7)

    # Interleaved holdouts: every 4th view is holdout when views == 32.
    holdout_stride = max(2, view_count // 8)
    train_dir = output / "images" / "train"
    holdout_dir = output / "images" / "holdout"
    train_dir.mkdir(parents=True, exist_ok=True)
    holdout_dir.mkdir(parents=True, exist_ok=True)

    meta = []
    radius = 0.42
    for i in range(view_count):
        elev = 0.30 + 0.12 * math.sin(i * 0.7)
        elev = max(0.14, elev)  # never under the object
        azim = 2 * math.pi * i / view_count
        loc = (
            radius * math.cos(azim) * math.cos(elev),
            radius * math.sin(azim) * math.cos(elev),
            radius * math.sin(elev),
        )
        cam = add_camera(f"view_{i:03d}", loc)
        scene.camera = cam
        is_holdout = (i % holdout_stride) == 0 and i > 0
        # Cap holdouts near 25%.
        dest = holdout_dir if is_holdout else train_dir
        # Rebalance if too many holdouts.
        path = dest / f"view_{i:03d}.png"
        scene.render.filepath = str(path)
        bpy.ops.render.render(write_still=True)
        meta.append(
            {
                "name": f"view_{i:03d}",
                "path": str(path),
                "held_out": is_holdout,
                "location": list(loc),
                "lens_mm": 50,
                "resolution": [256, 256],
            }
        )
        bpy.data.objects.remove(cam, do_unlink=True)
    return meta


def main() -> None:
    args = _parse_args(sys.argv)
    output = Path(args["output"])
    output.mkdir(parents=True, exist_ok=True)
    meta = build_remote()
    export_truth(output / "truth.obj")
    views = render_views(output, int(args["views"]))
    meta["views"] = views
    meta["image_dir"] = str(output / "images")
    meta["truth_mesh"] = str(output / "truth.obj")
    (output / "blender_target.json").write_text(
        json.dumps(meta, indent=2) + "\n", encoding="utf-8"
    )
    print(f"RENDERED consumer_remote: {len(views)} views -> {output}")
    print("V2_REMOTE_BUILD_OK")


if __name__ == "__main__":
    main()
