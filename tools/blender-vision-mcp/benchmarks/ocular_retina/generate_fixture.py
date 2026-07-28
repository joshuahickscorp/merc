"""Blender headless fixture for ocular retina events.

Renders a short image sequence with ground-truth object positions.

Modes (argv after --):
  object_motion  — object enters, moves, is occluded, leaves, returns (camera fixed)
  camera_motion  — object static, camera pans (must not be reported as OBJECT_MOVED)

Usage:
  blender --background --factory-startup --python generate_fixture.py -- OUT_DIR MODE
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bpy

IMAGE_W = 320
IMAGE_H = 240
N_FRAMES = 24


def clear_scene() -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for collection in (bpy.data.materials, bpy.data.cameras, bpy.data.lights, bpy.data.meshes):
        for item in list(collection):
            collection.remove(item)


def make_checker_material(
    name: str,
    a: tuple[float, float, float, float],
    b: tuple[float, float, float, float],
    scale: float = 18.0,
) -> bpy.types.Material:
    """A textured material.

    Optical flow needs something to lock onto. A camera panning across an
    untextured plane produces no measurable flow at all — the aperture problem —
    so a featureless fixture cannot test camera/object separation no matter how
    far the camera travels. The checker gives the flow field real structure.
    """
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    tree = mat.node_tree
    bsdf = tree.nodes.get("Principled BSDF")
    checker = tree.nodes.new("ShaderNodeTexChecker")
    checker.inputs["Color1"].default_value = a
    checker.inputs["Color2"].default_value = b
    checker.inputs["Scale"].default_value = scale
    coords = tree.nodes.new("ShaderNodeTexCoord")
    tree.links.new(coords.outputs["Generated"], checker.inputs["Vector"])
    tree.links.new(checker.outputs["Color"], bsdf.inputs["Base Color"])
    bsdf.inputs["Roughness"].default_value = 0.85
    return mat


def make_material(name: str, color: tuple[float, float, float, float]) -> bpy.types.Material:
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    nodes = mat.node_tree.nodes
    nodes.clear()
    out = nodes.new("ShaderNodeOutputMaterial")
    bsdf = nodes.new("ShaderNodeBsdfPrincipled")
    bsdf.inputs["Base Color"].default_value = color
    bsdf.inputs["Roughness"].default_value = 0.4
    mat.node_tree.links.new(bsdf.outputs["BSDF"], out.inputs["Surface"])
    return mat


def setup_world() -> None:
    world = bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes["Background"]
    bg.inputs[0].default_value = (0.15, 0.16, 0.18, 1.0)
    bg.inputs[1].default_value = 1.0


def build_scene() -> dict:
    clear_scene()
    setup_world()

    # Ground plane.
    bpy.ops.mesh.primitive_plane_add(size=6.0, location=(0.0, 0.0, 0.0))
    ground = bpy.context.object
    ground.name = "ground"
    ground.data.materials.append(
        make_checker_material(
            "ground", (0.18, 0.20, 0.23, 1.0), (0.52, 0.55, 0.58, 1.0), scale=22.0
        )
    )

    # Back wall: a pan needs texture at depth as well as underfoot, otherwise
    # the upper half of frame carries no flow signal at all.
    bpy.ops.mesh.primitive_plane_add(size=8.0, location=(0.0, 2.6, 1.6))
    wall = bpy.context.active_object
    wall.name = "back_wall"
    wall.rotation_euler = (1.5708, 0.0, 0.0)
    wall.data.materials.append(
        make_checker_material(
            "wall", (0.14, 0.16, 0.20, 1.0), (0.42, 0.46, 0.52, 1.0), scale=14.0
        )
    )

    # Moving / static cube.
    bpy.ops.mesh.primitive_cube_add(size=0.5, location=(-2.0, 0.0, 0.25))
    cube = bpy.context.object
    cube.name = "target"
    cube.data.materials.append(make_material("target", (0.95, 0.12, 0.06, 1.0)))

    # Occluder slab (used in object_motion mode).
    bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0.5, -0.6, 0.4))
    occluder = bpy.context.object
    occluder.name = "occluder"
    occluder.scale = (0.15, 0.8, 0.8)
    bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
    occluder.data.materials.append(make_material("occluder", (0.35, 0.4, 0.55, 1.0)))

    # Light.
    bpy.ops.object.light_add(type="AREA", location=(1.5, -2.0, 3.0))
    light = bpy.context.object
    light.data.energy = 250.0
    light.data.size = 2.0

    # Camera looking at origin, Blender Z-up / -Y forward world.
    bpy.ops.object.camera_add(location=(0.0, -4.5, 1.8))
    cam = bpy.context.object
    cam.name = "Camera"
    cam.rotation_euler = (math.radians(72.0), 0.0, 0.0)
    bpy.context.scene.camera = cam

    return {"cube": cube, "occluder": occluder, "camera": cam}


def configure_render(scene: bpy.types.Scene, out_dir: Path) -> None:
    # EEVEE, not Workbench. Workbench is the solid-shading viewport renderer:
    # it ignores materials entirely and shades everything a single studio
    # colour. That produced a textureless fixture, and optical flow over a
    # featureless plane yields no signal at all — the camera-motion benchmark
    # was measuring noise. Verified on this host: EEVEE renders the checker,
    # Workbench does not.
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.eevee.taa_render_samples = 16
    scene.render.resolution_x = IMAGE_W
    scene.render.resolution_y = IMAGE_H
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGB"
    scene.render.filepath = str(out_dir / "frames" / "frame_")
    scene.frame_start = 1
    scene.frame_end = N_FRAMES
    scene.frame_step = 1


def animate_object_motion(cube, occluder, camera) -> list[dict]:
    """Object enters from left, moves, goes behind occluder, leaves, returns."""
    gt: list[dict] = []
    # Keyframe path for the cube (Blender world, Z-up).
    # frames 1-4: off-screen left (not visible)
    # 5-8: enter and move right
    # 9-12: behind occluder
    # 13-16: leave to the right
    # 17-20: gone
    # 21-24: re-enter from left
    path = {
        1: (-3.5, 0.0, 0.25),
        4: (-2.2, 0.0, 0.25),
        5: (-1.5, 0.0, 0.25),
        8: (-0.2, 0.0, 0.25),
        9: (0.4, 0.0, 0.25),
        12: (0.6, 0.0, 0.25),
        13: (1.5, 0.0, 0.25),
        16: (3.0, 0.0, 0.25),
        17: (4.0, 0.0, 0.25),
        20: (4.0, 0.0, 0.25),
        21: (-2.0, 0.0, 0.25),
        24: (-0.5, 0.0, 0.25),
    }
    for frame, loc in path.items():
        cube.location = loc
        cube.keyframe_insert(data_path="location", frame=frame)

    # Occluder static.
    occluder.location = (0.5, -0.15, 0.4)
    camera.location = (0.0, -4.5, 1.8)
    camera.keyframe_insert(data_path="location", frame=1)
    camera.keyframe_insert(data_path="location", frame=N_FRAMES)

    for f in range(1, N_FRAMES + 1):
        # Interpolate cube position for ground truth.
        keys = sorted(path)
        lo = max(k for k in keys if k <= f)
        hi = min(k for k in keys if k >= f)
        if lo == hi:
            pos = path[lo]
        else:
            t = (f - lo) / (hi - lo)
            a, b = path[lo], path[hi]
            pos = tuple(a[i] + t * (b[i] - a[i]) for i in range(3))
        visible = pos[0] > -2.5 and pos[0] < 2.8
        occluded = visible and 0.2 <= pos[0] <= 0.9
        events: list[str] = []
        if f == 5:
            events.append("OBJECT_ENTERED")
        if 6 <= f <= 8:
            events.append("OBJECT_MOVED")
        if f in {9, 10}:
            events.append("OBJECT_OCCLUDED")
        if f == 16:
            events.append("OBJECT_LEFT")
        if f == 21:
            events.append("OBJECT_REAPPEARED")
            events.append("OBJECT_ENTERED")
        if 22 <= f <= 24:
            events.append("OBJECT_MOVED")
        gt.append(
            {
                "frame": f,
                "object_location_blender_world": list(pos),
                "coordinate_frame": {
                    "name": "blender-world",
                    "up_axis": "+Z",
                    "forward_axis": "-Y",
                },
                "visible": visible,
                "occluded": occluded,
                "expected_events": events,
            }
        )
    return gt


def animate_camera_motion(cube, occluder, camera) -> list[dict]:
    """Static object; camera pans. Expected CAMERA_MOVED only."""
    cube.location = (0.0, 0.0, 0.25)
    cube.keyframe_insert(data_path="location", frame=1)
    cube.keyframe_insert(data_path="location", frame=N_FRAMES)
    occluder.location = (3.0, 0.0, 0.4)  # out of the way

    gt: list[dict] = []
    for f in range(1, N_FRAMES + 1):
        t = (f - 1) / max(N_FRAMES - 1, 1)
        # Pan camera along X.
        camera.location = (-1.2 + 2.4 * t, -4.5, 1.8)
        camera.keyframe_insert(data_path="location", frame=f)
        events = ["CAMERA_MOVED"] if f > 1 else []
        gt.append(
            {
                "frame": f,
                "object_location_blender_world": [0.0, 0.0, 0.25],
                "camera_location_blender_world": list(camera.location),
                "coordinate_frame": {
                    "name": "blender-world",
                    "up_axis": "+Z",
                    "forward_axis": "-Y",
                },
                "visible": True,
                "occluded": False,
                "expected_events": events,
            }
        )
    return gt


def main() -> None:
    argv = sys.argv
    if "--" in argv:
        argv = argv[argv.index("--") + 1 :]
    else:
        argv = []
    if len(argv) < 1:
        print("usage: generate_fixture.py OUT_DIR [object_motion|camera_motion]", file=sys.stderr)
        sys.exit(2)
    out_dir = Path(argv[0])
    mode = argv[1] if len(argv) > 1 else "object_motion"
    out_dir.mkdir(parents=True, exist_ok=True)
    frames_dir = out_dir / "frames"
    frames_dir.mkdir(parents=True, exist_ok=True)

    objects = build_scene()
    scene = bpy.context.scene
    configure_render(scene, out_dir)

    if mode == "camera_motion":
        ground_truth = animate_camera_motion(
            objects["cube"], objects["occluder"], objects["camera"]
        )
    else:
        ground_truth = animate_object_motion(
            objects["cube"], objects["occluder"], objects["camera"]
        )

    # Linear interpolation between keyframes.
    for action in (objects["cube"].animation_data, objects["camera"].animation_data):
        if action and action.action:
            for fcurve in action.action.fcurves:
                for kp in fcurve.keyframe_points:
                    kp.interpolation = "LINEAR"

    scene.render.filepath = str(frames_dir / "frame_")
    bpy.ops.render.render(animation=True)

    rendered = sorted(frames_dir.glob("frame_*.png"))
    manifest = {
        "mode": mode,
        "image_width": IMAGE_W,
        "image_height": IMAGE_H,
        "n_frames": N_FRAMES,
        "coordinate_frame": {
            "name": "blender-world",
            "up_axis": "+Z",
            "forward_axis": "-Y",
            "handedness": "right",
            "units": "m",
        },
        "frames": [
            {
                "index": i + 1,
                "path": f"frames/{p.name}",
                **ground_truth[i],
            }
            for i, p in enumerate(rendered)
        ],
        "ground_truth": ground_truth,
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"OCULAR_FIXTURE_OK mode={mode} frames={len(rendered)} out={out_dir}")


if __name__ == "__main__":
    main()
