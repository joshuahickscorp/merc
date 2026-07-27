"""Build and render ocular_hard conditions inside Blender (EEVEE).

Invoked as:
  blender --background --python create_scene.py -- --output DIR

Produces per-condition RGB frames under DIR/<condition>/frames/ and sealed
ground truth under DIR/<condition>/sealed_gt/. The builder-visible
sequence_manifest.json intentionally omits ground-truth paths.

Objects share near-identical albedo; procedural textures differ so identity
cannot be solved by colour alone. World frame: Blender (+Z up). Image: OpenCV.
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bpy
from mathutils import Vector

# Inline catalogue so Blender's isolated Python does not need package imports.
FRAME_COUNT = 32
RESOLUTION = (320, 240)
CONDITIONS = [
    "visually_similar",
    "crossing_paths",
    "partial_occlusion",
    "full_occlusion",
    "lighting_change",
    "scale_change",
    "camera_motion",
    "leave_return",
    "distractor_replacement",
    "unknown_entering",
    "permanence",
]
PRIMARY = ("obj_a", "obj_b", "obj_c")
# Near-identical beige albedo; textures distinguish them.
ALBEDO = (0.72, 0.62, 0.48, 1.0)
ALBEDO_DISTRACTOR = (0.70, 0.60, 0.46, 1.0)
ALBEDO_UNKNOWN = (0.68, 0.64, 0.52, 1.0)


def _clear() -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for coll in (
        bpy.data.materials,
        bpy.data.cameras,
        bpy.data.lights,
        bpy.data.meshes,
        bpy.data.images,
    ):
        for item in list(coll):
            coll.remove(item)


def _noise_material(
    name: str, color: tuple[float, float, float, float], seed: int
) -> bpy.types.Material:
    """Principled BSDF with a procedural noise mix so textures differ."""
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    nodes = mat.node_tree.nodes
    links = mat.node_tree.links
    nodes.clear()
    out = nodes.new("ShaderNodeOutputMaterial")
    bsdf = nodes.new("ShaderNodeBsdfPrincipled")
    bsdf.inputs["Roughness"].default_value = 0.35 + 0.02 * (seed % 5)
    tex = nodes.new("ShaderNodeTexNoise")
    tex.inputs["Scale"].default_value = 8.0 + seed % 7
    tex.inputs["Detail"].default_value = 6.0
    tex.noise_dimensions = "3D"
    # Offset by seed via mapping.
    mapping = nodes.new("ShaderNodeMapping")
    mapping.inputs["Location"].default_value = (seed * 0.37, seed * 0.13, seed * 0.07)
    coord = nodes.new("ShaderNodeTexCoord")
    mix = nodes.new("ShaderNodeMixRGB")
    mix.blend_type = "MIX"
    mix.inputs["Fac"].default_value = 0.35
    mix.inputs["Color1"].default_value = color
    # Slightly different secondary colour per seed.
    mix.inputs["Color2"].default_value = (
        min(1.0, color[0] + 0.08 * ((seed % 3) - 1)),
        min(1.0, color[1] + 0.05 * ((seed % 5) - 2)),
        min(1.0, color[2] + 0.04 * ((seed % 4) - 1)),
        1.0,
    )
    links.new(coord.outputs["Object"], mapping.inputs["Vector"])
    links.new(mapping.outputs["Vector"], tex.inputs["Vector"])
    links.new(tex.outputs["Fac"], mix.inputs["Fac"])
    links.new(mix.outputs["Color"], bsdf.inputs["Base Color"])
    links.new(bsdf.outputs["BSDF"], out.inputs["Surface"])
    mat.diffuse_color = color
    return mat


def _solid_material(name: str, color: tuple[float, float, float, float]) -> bpy.types.Material:
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF")
    if bsdf is not None:
        bsdf.inputs["Base Color"].default_value = color
        bsdf.inputs["Roughness"].default_value = 0.85
    mat.diffuse_color = color
    return mat


def _sphere(name: str, loc: tuple[float, float, float], radius: float = 0.08):
    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=24, ring_count=16, radius=radius, location=loc
    )
    obj = bpy.context.object
    obj.name = name
    bpy.ops.object.shade_smooth()
    return obj


def _box(name: str, loc: tuple[float, float, float], scale: tuple[float, float, float]):
    bpy.ops.mesh.primitive_cube_add(location=loc)
    obj = bpy.context.object
    obj.name = name
    obj.scale = scale
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    return obj


def _project(scene, cam, world_co: Vector) -> tuple[float, float, bool]:
    try:
        from bpy_extras.object_utils import world_to_camera_view

        deps = bpy.context.evaluated_depsgraph_get()
        ndc = world_to_camera_view(scene.evaluated_get(deps), cam.evaluated_get(deps), world_co)
        u = ndc.x * scene.render.resolution_x
        v = (1.0 - ndc.y) * scene.render.resolution_y
        visible = 0.0 <= ndc.x <= 1.0 and 0.0 <= ndc.y <= 1.0 and ndc.z > 0.0
        return float(u), float(v), bool(visible)
    except Exception:
        return 0.0, 0.0, False


def _setup_render(scene: bpy.types.Scene) -> None:
    try:
        scene.render.engine = "BLENDER_EEVEE_NEXT"
    except Exception:
        try:
            scene.render.engine = "BLENDER_EEVEE"
        except Exception:
            scene.render.engine = "CYCLES"
            scene.cycles.samples = 16
    scene.render.resolution_x = RESOLUTION[0]
    scene.render.resolution_y = RESOLUTION[1]
    scene.render.image_settings.file_format = "PNG"
    scene.render.fps = 12
    scene.frame_start = 1
    scene.frame_end = FRAME_COUNT
    scene.unit_settings.system = "METRIC"


def build_condition(condition: str) -> dict[str, bpy.types.Object]:
    _clear()
    scene = bpy.context.scene
    _setup_render(scene)

    table = _box("table", (0.0, 0.0, -0.02), (0.7, 0.45, 0.02))
    table.data.materials.append(_solid_material("TableMat", (0.12, 0.14, 0.16, 1.0)))

    mat_a = _noise_material("MatA", ALBEDO, 11)
    mat_b = _noise_material("MatB", ALBEDO, 23)
    mat_c = _noise_material("MatC", ALBEDO, 37)
    mat_d = _noise_material("MatDistractor", ALBEDO_DISTRACTOR, 53)
    mat_u = _noise_material("MatUnknown", ALBEDO_UNKNOWN, 71)
    mat_occ = _solid_material("MatOcc", (0.05, 0.05, 0.08, 1.0))

    obj_a = _sphere("obj_a", (-0.22, 0.0, 0.08))
    obj_a.data.materials.append(mat_a)
    obj_b = _sphere("obj_b", (0.0, 0.05, 0.08))
    obj_b.data.materials.append(mat_b)
    obj_c = _sphere("obj_c", (0.22, -0.03, 0.08))
    obj_c.data.materials.append(mat_c)
    distractor = _sphere("obj_distractor", (0.7, 0.2, 0.08))
    distractor.data.materials.append(mat_d)
    distractor.hide_render = True
    unknown = _sphere("obj_unknown", (0.7, -0.2, 0.08), radius=0.07)
    unknown.data.materials.append(mat_u)
    unknown.hide_render = True
    occluder = _box("occluder_slab", (0.55, 0.05, 0.12), (0.12, 0.09, 0.05))
    occluder.data.materials.append(mat_occ)
    occluder.hide_render = True

    bpy.ops.object.camera_add(location=(0.0, -0.9, 0.8))
    cam = bpy.context.object
    cam.name = "TrackCam"
    cam.rotation_euler = (math.radians(55), 0.0, 0.0)
    cam.data.lens = 35
    scene.camera = cam

    bpy.ops.object.light_add(type="AREA", location=(0.3, -0.4, 1.2))
    light = bpy.context.object
    light.data.energy = 250
    light.data.size = 1.2
    bpy.ops.object.light_add(type="SUN", location=(0.0, 0.0, 2.0))
    sun = bpy.context.object
    sun.data.energy = 1.5

    objects = {
        "obj_a": obj_a,
        "obj_b": obj_b,
        "obj_c": obj_c,
        "obj_distractor": distractor,
        "obj_unknown": unknown,
        "occluder_slab": occluder,
        "camera": cam,
        "key_light": light,
        "sun": sun,
    }
    _keyframe(condition, objects)
    return objects


def _set_hide(obj: bpy.types.Object, frame: int, hidden: bool) -> None:
    obj.hide_render = hidden
    obj.hide_viewport = hidden
    obj.keyframe_insert(data_path="hide_render", frame=frame)
    obj.keyframe_insert(data_path="hide_viewport", frame=frame)


def _keyframe(condition: str, objects: dict[str, bpy.types.Object]) -> None:
    a, b, c = objects["obj_a"], objects["obj_b"], objects["obj_c"]
    d = objects["obj_distractor"]
    u = objects["obj_unknown"]
    occ = objects["occluder_slab"]
    cam = objects["camera"]
    light = objects["key_light"]

    for frame in range(1, FRAME_COUNT + 1):
        t = (frame - 1) / max(1, FRAME_COUNT - 1)
        # Defaults each frame.
        a.location = (-0.22, 0.0, 0.08)
        b.location = (0.0, 0.05, 0.08)
        c.location = (0.22, -0.03, 0.08)
        _set_hide(a, frame, False)
        _set_hide(b, frame, False)
        _set_hide(c, frame, False)
        _set_hide(d, frame, True)
        _set_hide(u, frame, True)
        _set_hide(occ, frame, True)
        cam.location = (0.0, -0.9, 0.8)
        light.data.energy = 250

        if condition == "visually_similar":
            a.location = (-0.22 + 0.15 * t, 0.0, 0.08)
            b.location = (0.0, 0.05 - 0.08 * t, 0.08)
            c.location = (0.22 - 0.1 * t, -0.03, 0.08)

        elif condition == "crossing_paths":
            _set_hide(c, frame, True)
            a.location = (-0.3 + 0.6 * t, 0.0, 0.08)
            b.location = (0.3 - 0.6 * t, 0.05, 0.08)

        elif condition == "partial_occlusion":
            _set_hide(c, frame, True)
            _set_hide(occ, frame, False)
            if frame < 8:
                ox = 0.40
            elif frame < 14:
                ox = 0.40 - 0.40 * ((frame - 8) / 6.0)
            elif frame < 20:
                ox = 0.0
            elif frame < 26:
                ox = 0.0 + 0.40 * ((frame - 20) / 6.0)
            else:
                ox = 0.40
            occ.location = (ox, 0.05, 0.12)
            occ.scale = (0.7, 1.0, 1.0)

        elif condition == "full_occlusion":
            _set_hide(c, frame, True)
            _set_hide(occ, frame, False)
            if frame < 8:
                ox = 0.45
            elif frame < 12:
                ox = 0.45 - 0.45 * ((frame - 8) / 4.0)
            elif frame < 22:
                ox = 0.0
                _set_hide(b, frame, True)
            elif frame < 26:
                ox = 0.0 + 0.45 * ((frame - 22) / 4.0)
            else:
                ox = 0.45
            occ.location = (ox, 0.05, 0.14)
            occ.scale = (1.2, 1.2, 1.0)

        elif condition == "lighting_change":
            light.data.energy = 250 if frame < 16 else 60

        elif condition == "scale_change":
            _set_hide(b, frame, True)
            _set_hide(c, frame, True)
            # Approach camera: +Y toward cam at -0.9.
            a.location = (0.0, -0.15 + 0.35 * t, 0.08)
            a.scale = (1.0 + 0.8 * t, 1.0 + 0.8 * t, 1.0 + 0.8 * t)

        elif condition == "camera_motion":
            pan = 0.25 * math.sin(t * math.pi)
            cam.location = (pan, -0.9, 0.8)

        elif condition == "leave_return":
            _set_hide(c, frame, True)
            if frame <= 8:
                b.location = (0.0, 0.05, 0.08)
            elif frame <= 12:
                u_t = (frame - 8) / 4.0
                b.location = (0.0 + 0.55 * u_t, 0.05, 0.08)
            elif frame <= 22:
                b.location = (0.7, 0.05, 0.08)
                _set_hide(b, frame, True)
            elif frame <= 26:
                u_t = (frame - 22) / 4.0
                b.location = (0.7 - 0.7 * u_t, 0.05, 0.08)
            else:
                b.location = (0.0, 0.05, 0.08)

        elif condition == "distractor_replacement":
            _set_hide(c, frame, True)
            if frame <= 10:
                b.location = (0.0, 0.05, 0.08)
            elif frame <= 14:
                u_t = (frame - 10) / 4.0
                b.location = (0.0 - 0.55 * u_t, 0.05, 0.08)
            else:
                b.location = (-0.7, 0.05, 0.08)
                _set_hide(b, frame, True)
            if frame < 18:
                d.location = (0.7, 0.05, 0.08)
                _set_hide(d, frame, True)
            elif frame <= 22:
                u_t = (frame - 18) / 4.0
                d.location = (0.7 - 0.7 * u_t, 0.05, 0.08)
                _set_hide(d, frame, False)
            else:
                d.location = (0.0, 0.05, 0.08)
                _set_hide(d, frame, False)

        elif condition == "unknown_entering":
            if frame >= 18:
                u_t = min(1.0, (frame - 18) / 6.0)
                u.location = (0.35 - 0.1 * u_t, -0.15, 0.08)
                _set_hide(u, frame, False)

        elif condition == "permanence":
            a.location = (-0.25 + 0.35 * t, 0.0, 0.08)
            # B full occlusion 10..20
            if 10 <= frame <= 20:
                _set_hide(b, frame, True)
                _set_hide(occ, frame, False)
                occ.location = (0.0, 0.05, 0.14)
                occ.scale = (1.3, 1.3, 1.0)
            else:
                b.location = (0.0, 0.05, 0.08)
            # C leave/return
            if frame <= 8:
                c.location = (0.25, -0.03, 0.08)
            elif frame <= 12:
                u_t = (frame - 8) / 4.0
                c.location = (0.25 + 0.5 * u_t, -0.03, 0.08)
            elif frame <= 24:
                c.location = (0.9, -0.03, 0.08)
                _set_hide(c, frame, True)
            elif frame <= 28:
                u_t = (frame - 24) / 4.0
                c.location = (0.9 - 0.65 * u_t, -0.03, 0.08)
            else:
                c.location = (0.25, -0.03, 0.08)
            # Distractor while C gone
            if 16 <= frame <= 26:
                d.location = (0.25, -0.03, 0.08)
                _set_hide(d, frame, False)
            if frame >= 28:
                u.location = (-0.15, -0.15, 0.08)
                _set_hide(u, frame, False)

        for obj in (a, b, c, d, u, occ, cam):
            obj.keyframe_insert(data_path="location", frame=frame)
        a.keyframe_insert(data_path="scale", frame=frame)
        light.data.keyframe_insert(data_path="energy", frame=frame)


def render_condition(output: Path, condition: str, objects: dict[str, bpy.types.Object]) -> None:
    cond_out = output / condition
    frames_dir = cond_out / "frames"
    sealed_dir = cond_out / "sealed_gt"
    frames_dir.mkdir(parents=True, exist_ok=True)
    sealed_dir.mkdir(parents=True, exist_ok=True)

    scene = bpy.context.scene
    cam = objects["camera"]
    tracked = list(PRIMARY) + ["obj_distractor", "obj_unknown", "occluder_slab"]
    builder_frames = []
    sealed_frames = []

    for frame in range(1, FRAME_COUNT + 1):
        scene.frame_set(frame)
        img_name = f"frame_{frame:04d}.png"
        scene.render.filepath = str(frames_dir / img_name)
        bpy.ops.render.render(write_still=True)

        records = []
        for name in tracked:
            obj = objects[name]
            hidden = bool(obj.hide_render)
            loc = obj.matrix_world.translation
            u, v, in_view = _project(scene, cam, loc)
            radius_px = 18.0 * float(max(obj.scale))
            bbox = [u - radius_px, v - radius_px, 2 * radius_px, 2 * radius_px]
            records.append(
                {
                    "id": name,
                    "visible": (not hidden) and in_view,
                    "hidden_flag": hidden,
                    "world_xyz_m": [float(loc.x), float(loc.y), float(loc.z)],
                    "image_uv_px": [float(u), float(v)],
                    "bbox_xywh_px": [float(c) for c in bbox],
                    "in_camera_view": bool(in_view),
                }
            )
        fi = frame - 1
        gt_name = f"frame_{frame:04d}.json"
        gt_payload = {
            "frame_index": fi,
            "blender_frame": frame,
            "image": img_name,
            "condition": condition,
            "objects": records,
        }
        (sealed_dir / gt_name).write_text(json.dumps(gt_payload, indent=2), encoding="utf-8")
        builder_frames.append({"frame_index": fi, "image": f"frames/{img_name}"})
        sealed_frames.append(
            {
                "frame_index": fi,
                "image": f"frames/{img_name}",
                "ground_truth": f"sealed_gt/{gt_name}",
            }
        )

    builder_manifest = {
        "condition": condition,
        "frame_count": FRAME_COUNT,
        "resolution": list(RESOLUTION),
        "source": "blender_eevee",
        "coordinate_frame": {
            "world": {"name": "blender-world", "up_axis": "+Z", "units": "m"},
            "image": {"name": "opencv-camera", "up_axis": "-Y", "units": "px"},
        },
        "frames": builder_frames,
        "sealed_manifest_relative": "sealed_manifest.json",
    }
    sealed_manifest = {
        **builder_manifest,
        "frames": sealed_frames,
        "primary_ids": list(PRIMARY),
        "distractor_id": "obj_distractor",
        "unknown_id": "obj_unknown",
        "occluder_id": "occluder_slab",
    }
    (cond_out / "sequence_manifest.json").write_text(
        json.dumps(builder_manifest, indent=2), encoding="utf-8"
    )
    (cond_out / "sealed_manifest.json").write_text(
        json.dumps(sealed_manifest, indent=2), encoding="utf-8"
    )


def main(argv: list[str]) -> int:
    output = Path("artifacts/ocular/tracking/hard")
    args = argv[argv.index("--") + 1 :] if "--" in argv else argv[1:]
    if "--output" in args:
        output = Path(args[args.index("--output") + 1])
    output.mkdir(parents=True, exist_ok=True)
    for condition in CONDITIONS:
        print(f"OCULAR_HARD_RENDER condition={condition}")
        objects = build_condition(condition)
        render_condition(output, condition, objects)
    (output / "OCULAR_HARD_COMPLETE").write_text(
        f"conditions={len(CONDITIONS)}\n", encoding="utf-8"
    )
    print(f"OCULAR_HARD_COMPLETE conditions={len(CONDITIONS)} output={output}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
