"""Build and render the ocular tabletop tracking fixture inside Blender.

Invoked as: blender --background --python create_scene.py -- --output DIR
Produces RGB frames, per-frame ground-truth JSON, and a sequence manifest.
Coordinate frame: Blender world (+Z up, -Y forward). Image space is OpenCV
(+X right, +Y down) via the projected 2D fields.
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bpy
from mathutils import Vector

FRAME_COUNT = 36
RESOLUTION = (320, 240)
# Similar-object colour: near-identical beige so colour alone cannot separate them.
OBJECT_COLOR = (0.72, 0.62, 0.48, 1.0)
OBJECT_COLOR_REPLACE = (0.70, 0.60, 0.46, 1.0)  # subtly different for negative case


def _material(
    name: str,
    color: tuple[float, float, float, float],
    roughness: float,
) -> bpy.types.Material:
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    nodes = mat.node_tree.nodes
    bsdf = nodes.get("Principled BSDF")
    if bsdf is not None:
        bsdf.inputs["Base Color"].default_value = color
        bsdf.inputs["Roughness"].default_value = roughness
    mat.diffuse_color = color
    return mat


def _uv_sphere(name: str, location: tuple[float, float, float], radius: float = 0.08):
    bpy.ops.mesh.primitive_uv_sphere_add(
        segments=24, ring_count=16, radius=radius, location=location
    )
    obj = bpy.context.object
    obj.name = name
    bpy.ops.object.shade_smooth()
    return obj


def _box(name: str, location: tuple[float, float, float], scale: tuple[float, float, float]):
    bpy.ops.mesh.primitive_cube_add(location=location)
    obj = bpy.context.object
    obj.name = name
    obj.scale = scale
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    return obj


def _project_world_to_camera(scene, cam, world_co: Vector) -> tuple[float, float, bool]:
    """Return OpenCV-style pixel (u, v) and whether the point is in front of the camera."""
    co_ndc = world_co.transformed_by(cam.matrix_world.inverted())
    # Camera space in Blender: -Z forward.
    if co_ndc.z >= 0:
        return 0.0, 0.0, False
    co_cam = cam.matrix_world.inverted() @ world_co
    # Use render projection.
    render = scene.render
    deps = bpy.context.evaluated_depsgraph_get()
    scene_eval = scene.evaluated_get(deps)
    cam_eval = cam.evaluated_get(deps)
    # Project with bpy_extras if available.
    try:
        from bpy_extras.object_utils import world_to_camera_view

        ndc = world_to_camera_view(scene_eval, cam_eval, world_co)
        u = ndc.x * render.resolution_x
        # OpenCV: +Y down — Blender NDC y=0 is bottom, so flip.
        v = (1.0 - ndc.y) * render.resolution_y
        visible = 0.0 <= ndc.x <= 1.0 and 0.0 <= ndc.y <= 1.0 and ndc.z > 0.0
        return float(u), float(v), bool(visible)
    except Exception:
        # Fallback pinhole.
        fl = cam.data.lens
        sensor = cam.data.sensor_width
        fx = (render.resolution_x * fl) / sensor
        fy = fx
        cx = render.resolution_x / 2.0
        cy = render.resolution_y / 2.0
        u = fx * (co_cam.x / -co_cam.z) + cx
        v = fy * (-co_cam.y / -co_cam.z) + cy
        return float(u), float(v), co_cam.z < 0


def build_scene() -> dict[str, bpy.types.Object]:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for coll in (bpy.data.materials, bpy.data.cameras, bpy.data.lights, bpy.data.meshes):
        for item in list(coll):
            coll.remove(item)

    scene = bpy.context.scene
    scene.unit_settings.system = "METRIC"
    scene.frame_start = 1
    scene.frame_end = FRAME_COUNT
    # Prefer EEVEE; fall back gracefully.
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
    scene.render.film_transparent = False
    scene.render.image_settings.file_format = "PNG"
    scene.render.fps = 12

    # Tabletop.
    table = _box("table", (0.0, 0.0, -0.02), (0.6, 0.4, 0.02))
    table.data.materials.append(_material("TableMat", (0.12, 0.14, 0.16, 1.0), 0.7))

    mat_a = _material("ObjMatA", OBJECT_COLOR, 0.35)
    mat_b = _material("ObjMatB", OBJECT_COLOR, 0.38)
    mat_c = _material("ObjMatC", OBJECT_COLOR, 0.41)
    mat_depart = _material("ObjMatDepart", OBJECT_COLOR, 0.36)
    mat_replace = _material("ObjMatReplace", OBJECT_COLOR_REPLACE, 0.55)

    # Three similar spheres.
    obj_move = _uv_sphere("obj_move", (-0.25, 0.0, 0.08))
    obj_move.data.materials.append(mat_a)
    obj_occlude = _uv_sphere("obj_occlude", (0.0, 0.05, 0.08))
    obj_occlude.data.materials.append(mat_b)
    obj_leave = _uv_sphere("obj_leave", (0.25, -0.05, 0.08))
    obj_leave.data.materials.append(mat_c)

    # Occluder slab (distinct dark colour so segmentation can see it separately).
    occluder = _box("occluder_slab", (0.45, 0.05, 0.10), (0.12, 0.08, 0.04))
    occluder.data.materials.append(_material("OccluderMat", (0.05, 0.05, 0.08, 1.0), 0.9))

    # Negative-case pair (start off-frame; animated later).
    obj_depart = _uv_sphere("obj_depart", (-0.45, 0.20, 0.08))
    obj_depart.data.materials.append(mat_depart)
    obj_replace = _uv_sphere("obj_replace", (0.55, 0.20, 0.08))
    obj_replace.data.materials.append(mat_replace)

    # Camera looking down at the table (Blender +Z up).
    bpy.ops.object.camera_add(location=(0.0, -0.85, 0.75))
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
        "obj_move": obj_move,
        "obj_occlude": obj_occlude,
        "obj_leave": obj_leave,
        "occluder_slab": occluder,
        "obj_depart": obj_depart,
        "obj_replace": obj_replace,
        "camera": cam,
    }
    _keyframe_motion(objects)
    return objects


def _keyframe_motion(objects: dict[str, bpy.types.Object]) -> None:
    move = objects["obj_move"]
    leave = objects["obj_leave"]
    occluder = objects["occluder_slab"]
    depart = objects["obj_depart"]
    replace = objects["obj_replace"]

    for frame in range(1, FRAME_COUNT + 1):
        t = (frame - 1) / (FRAME_COUNT - 1)

        # obj_move: slides +X across the table for the whole sequence.
        move.location = (-0.25 + 0.45 * t, 0.0, 0.08)
        move.keyframe_insert(data_path="location", frame=frame)

        # obj_occlude: stationary.
        objects["obj_occlude"].location = (0.0, 0.05, 0.08)
        objects["obj_occlude"].keyframe_insert(data_path="location", frame=frame)

        # occluder slides over obj_occlude mid-sequence (frames ~12-22).
        if frame < 10:
            ox = 0.40
        elif frame < 14:
            ox = 0.40 - 0.40 * ((frame - 10) / 4.0)
        elif frame < 22:
            ox = 0.0
        elif frame < 26:
            ox = 0.0 + 0.40 * ((frame - 22) / 4.0)
        else:
            ox = 0.40
        occluder.location = (ox, 0.05, 0.12)
        occluder.keyframe_insert(data_path="location", frame=frame)

        # obj_leave: present, exits right, gone, returns from right.
        if frame <= 8:
            leave.location = (0.25, -0.05, 0.08)
            leave.hide_render = False
            leave.hide_viewport = False
        elif frame <= 12:
            u = (frame - 8) / 4.0
            leave.location = (0.25 + 0.5 * u, -0.05, 0.08)
            leave.hide_render = False
            leave.hide_viewport = False
        elif frame <= 24:
            leave.location = (0.90, -0.05, 0.08)
            leave.hide_render = True
            leave.hide_viewport = True
        elif frame <= 28:
            u = (frame - 24) / 4.0
            leave.location = (0.90 - 0.65 * u, -0.05, 0.08)
            leave.hide_render = False
            leave.hide_viewport = False
        else:
            leave.location = (0.25, -0.05, 0.08)
            leave.hide_render = False
            leave.hide_viewport = False
        leave.keyframe_insert(data_path="location", frame=frame)
        leave.keyframe_insert(data_path="hide_render", frame=frame)
        leave.keyframe_insert(data_path="hide_viewport", frame=frame)

        # Negative case: obj_depart visible early frames, then leaves forever.
        # obj_replace appears late in a similar place — different identity.
        if frame <= 10:
            depart.location = (-0.20, 0.18, 0.08)
            depart.hide_render = False
            depart.hide_viewport = False
        elif frame <= 14:
            u = (frame - 10) / 4.0
            depart.location = (-0.20 - 0.5 * u, 0.18, 0.08)
            depart.hide_render = False
            depart.hide_viewport = False
        else:
            depart.location = (-0.90, 0.18, 0.08)
            depart.hide_render = True
            depart.hide_viewport = True
        depart.keyframe_insert(data_path="location", frame=frame)
        depart.keyframe_insert(data_path="hide_render", frame=frame)
        depart.keyframe_insert(data_path="hide_viewport", frame=frame)

        if frame < 20:
            replace.location = (0.90, 0.18, 0.08)
            replace.hide_render = True
            replace.hide_viewport = True
        elif frame <= 24:
            u = (frame - 20) / 4.0
            replace.location = (0.90 - 0.7 * u, 0.18, 0.08)
            replace.hide_render = False
            replace.hide_viewport = False
        else:
            replace.location = (-0.20, 0.18, 0.08)
            replace.hide_render = False
            replace.hide_viewport = False
        replace.keyframe_insert(data_path="location", frame=frame)
        replace.keyframe_insert(data_path="hide_render", frame=frame)
        replace.keyframe_insert(data_path="hide_viewport", frame=frame)


def render_sequence(output: Path, objects: dict[str, bpy.types.Object]) -> dict:
    output.mkdir(parents=True, exist_ok=True)
    frames_dir = output / "frames"
    frames_dir.mkdir(exist_ok=True)
    gt_dir = output / "ground_truth"
    gt_dir.mkdir(exist_ok=True)

    scene = bpy.context.scene
    cam = objects["camera"]
    tracked_names = [
        "obj_move",
        "obj_occlude",
        "obj_leave",
        "obj_depart",
        "obj_replace",
        "occluder_slab",
    ]
    sequence = {
        "frame_count": FRAME_COUNT,
        "resolution": list(RESOLUTION),
        "coordinate_frame": {
            "world": {
                "name": "blender-world",
                "up_axis": "+Z",
                "forward_axis": "-Y",
                "handedness": "right",
                "units": "m",
            },
            "image": {
                "name": "opencv-camera",
                "up_axis": "-Y",
                "forward_axis": "+Z",
                "units": "px",
            },
        },
        "objects": tracked_names,
        "roles": {
            "obj_move": "moves across table",
            "obj_occlude": "stationary; occluded mid-sequence",
            "obj_leave": "leaves and returns (same identity)",
            "obj_depart": "leaves permanently (negative case)",
            "obj_replace": "replacement similar object (must not re-id as depart)",
            "occluder_slab": "occluder for obj_occlude",
        },
        "frames": [],
    }

    for frame in range(1, FRAME_COUNT + 1):
        scene.frame_set(frame)
        frame_path = frames_dir / f"frame_{frame:04d}.png"
        scene.render.filepath = str(frame_path)
        bpy.ops.render.render(write_still=True)

        records = []
        for name in tracked_names:
            obj = objects[name]
            hidden = bool(obj.hide_render)
            loc = obj.matrix_world.translation
            u, v, in_view = _project_world_to_camera(scene, cam, loc)
            # Approximate projected radius for bbox (~sphere radius 0.08m).
            radius_px = 18.0
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
        frame_gt = {
            "frame_index": frame - 1,
            "blender_frame": frame,
            "image": str(frame_path.name),
            "objects": records,
        }
        gt_path = gt_dir / f"frame_{frame:04d}.json"
        gt_path.write_text(json.dumps(frame_gt, indent=2), encoding="utf-8")
        sequence["frames"].append(
            {
                "frame_index": frame - 1,
                "image": str(frame_path.relative_to(output)),
                "ground_truth": str(gt_path.relative_to(output)),
            }
        )

    manifest_path = output / "sequence_manifest.json"
    manifest_path.write_text(json.dumps(sequence, indent=2), encoding="utf-8")
    print(f"OCULAR_TABLETOP_COMPLETE frames={FRAME_COUNT} output={output}")
    return sequence


def main(argv: list[str]) -> int:
    output = Path("artifacts/ocular/tracking/blender")
    args = argv[argv.index("--") + 1 :] if "--" in argv else argv[1:]
    if "--output" in args:
        output = Path(args[args.index("--output") + 1])
    objects = build_scene()
    render_sequence(output, objects)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
