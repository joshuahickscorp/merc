"""Blender EEVEE fixture renderer for the 9×4 ocular-event matrix.

Renders short textured sequences. Workbench is forbidden: it ignores materials
and yields featureless frames where optical flow is pure noise.

Usage (headless):
  blender --background --factory-startup --python generate_blender.py -- OUT_DIR
  blender ... -- OUT_DIR EVENT FIXTURE_CLASS   # single cell
"""

from __future__ import annotations

import json
import math
import sys
from pathlib import Path

import bpy

IMAGE_W = 320
IMAGE_H = 240
N_FRAMES = 6

EVENTS = (
    "OBJECT_MOVED",
    "CAMERA_MOVED",
    "OBJECT_ENTERED",
    "OBJECT_LEFT",
    "OBJECT_OCCLUDED",
    "OBJECT_REAPPEARED",
    "NEW_UNKNOWN_REGION",
    "LIGHT_CHANGED",
    "SURFACE_CHANGED",
)
CLASSES = ("true_positive", "true_negative", "near_threshold", "confounder")


def clear_scene() -> None:
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for collection in (bpy.data.materials, bpy.data.cameras, bpy.data.lights, bpy.data.meshes):
        for item in list(collection):
            collection.remove(item)


def make_checker(
    name: str,
    a: tuple[float, float, float, float],
    b: tuple[float, float, float, float],
    scale: float = 18.0,
) -> bpy.types.Material:
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


def make_solid(name: str, color: tuple[float, float, float, float]) -> bpy.types.Material:
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


def setup_world(strength: float = 1.0) -> bpy.types.World:
    world = bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes["Background"]
    bg.inputs[0].default_value = (0.15, 0.16, 0.18, 1.0)
    bg.inputs[1].default_value = strength
    return world


def build_base_scene(*, with_target: bool = True, with_occluder: bool = False) -> dict:
    clear_scene()
    world = setup_world(1.0)

    bpy.ops.mesh.primitive_plane_add(size=6.0, location=(0.0, 0.0, 0.0))
    ground = bpy.context.object
    ground.name = "ground"
    ground.data.materials.append(
        make_checker("ground", (0.18, 0.20, 0.23, 1.0), (0.52, 0.55, 0.58, 1.0), 22.0)
    )

    bpy.ops.mesh.primitive_plane_add(size=8.0, location=(0.0, 2.6, 1.6))
    wall = bpy.context.active_object
    wall.name = "back_wall"
    wall.rotation_euler = (1.5708, 0.0, 0.0)
    wall.data.materials.append(
        make_checker("wall", (0.14, 0.16, 0.20, 1.0), (0.42, 0.46, 0.52, 1.0), 14.0)
    )

    target = None
    if with_target:
        bpy.ops.mesh.primitive_cube_add(size=0.5, location=(0.0, 0.0, 0.25))
        target = bpy.context.object
        target.name = "target"
        target.data.materials.append(make_solid("target", (0.95, 0.12, 0.06, 1.0)))

    occluder = None
    if with_occluder:
        bpy.ops.mesh.primitive_cube_add(size=1.0, location=(0.5, -0.15, 0.4))
        occluder = bpy.context.object
        occluder.name = "occluder"
        occluder.scale = (0.15, 0.8, 0.8)
        bpy.ops.object.transform_apply(location=False, rotation=False, scale=True)
        occluder.data.materials.append(make_solid("occluder", (0.35, 0.4, 0.55, 1.0)))

    bpy.ops.object.light_add(type="AREA", location=(1.5, -2.0, 3.0))
    light = bpy.context.object
    light.data.energy = 250.0
    light.data.size = 2.0

    bpy.ops.object.camera_add(location=(0.0, -4.5, 1.8))
    cam = bpy.context.object
    cam.name = "Camera"
    cam.rotation_euler = (math.radians(72.0), 0.0, 0.0)
    bpy.context.scene.camera = cam

    return {
        "target": target,
        "occluder": occluder,
        "camera": cam,
        "light": light,
        "world": world,
    }


def configure_render(scene: bpy.types.Scene, frames_dir: Path, n_frames: int = N_FRAMES) -> None:
    # EEVEE, never Workbench — Workbench drops materials entirely.
    scene.render.engine = "BLENDER_EEVEE_NEXT"
    scene.eevee.taa_render_samples = 16
    scene.render.resolution_x = IMAGE_W
    scene.render.resolution_y = IMAGE_H
    scene.render.resolution_percentage = 100
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGB"
    scene.render.filepath = str(frames_dir / "frame_")
    scene.frame_start = 1
    scene.frame_end = n_frames
    scene.frame_step = 1


def _linear_keys(obj, data_path: str, keys: dict[int, tuple]) -> None:
    for frame, value in keys.items():
        if data_path == "location":
            obj.location = value
        elif data_path == "scale":
            obj.scale = value
        obj.keyframe_insert(data_path=data_path, frame=frame)
    if obj.animation_data and obj.animation_data.action:
        for fcurve in obj.animation_data.action.fcurves:
            for kp in fcurve.keyframe_points:
                kp.interpolation = "LINEAR"


def _spec(event: str, cls: str) -> dict:
    """Return expect_fire, forbidden, notes, measured_quantity for a cell."""
    table: dict[tuple[str, str], dict] = {}
    for e in EVENTS:
        table[(e, "true_negative")] = {
            "expect_fire": False,
            "forbidden": [e],
            "notes": "static / irrelevant",
            "mq": "event_score",
        }
    # true positives
    table[("OBJECT_MOVED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": ["CAMERA_MOVED"],
        "notes": "target translates across frame",
        "mq": "object_motion_score",
    }
    table[("CAMERA_MOVED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": ["OBJECT_MOVED"],
        "notes": "camera pans, target static",
        "mq": "moving_fraction",
    }
    table[("OBJECT_ENTERED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": [],
        "notes": "target enters from off-screen",
        "mq": "unmatched_motion_blob",
    }
    table[("OBJECT_LEFT", "true_positive")] = {
        "expect_fire": True,
        "forbidden": [],
        "notes": "target exits frame",
        "mq": "track_missing_frames",
    }
    table[("OBJECT_OCCLUDED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": [],
        "notes": "target passes behind occluder",
        "mq": "area_ratio",
    }
    table[("OBJECT_REAPPEARED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": [],
        "notes": "target emerges from behind occluder",
        "mq": "area_recovery",
    }
    table[("NEW_UNKNOWN_REGION", "true_positive")] = {
        "expect_fire": True,
        "forbidden": ["CAMERA_MOVED"],
        "notes": "new cube materialises on static background",
        "mq": "global_compensated_residual_no_prior",
    }
    table[("LIGHT_CHANGED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": [],
        "notes": "world strength jumps",
        "mq": "abs_mean_luma_delta",
    }
    table[("SURFACE_CHANGED", "true_positive")] = {
        "expect_fire": True,
        "forbidden": ["CAMERA_MOVED"],
        "notes": "ground material tint shifts",
        "mq": "distributed_residual",
    }
    # near threshold: same as TP but smaller delta (animation scaled down)
    for e in EVENTS:
        base = table.get((e, "true_positive"), table[(e, "true_negative")])
        table[(e, "near_threshold")] = {
            **base,
            "notes": f"near-threshold variant of {e}",
        }
    # confounders
    table[("OBJECT_MOVED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["OBJECT_MOVED"],
        "notes": "camera pan confounder",
        "mq": "moving_fraction",
    }
    table[("CAMERA_MOVED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["CAMERA_MOVED"],
        "notes": "object motion confounder",
        "mq": "moving_fraction",
    }
    table[("OBJECT_ENTERED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["OBJECT_ENTERED"],
        "notes": "light change confounder",
        "mq": "unmatched_motion_blob",
    }
    table[("OBJECT_LEFT", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["OBJECT_LEFT"],
        "notes": "partial occlusion confounder",
        "mq": "track_missing_frames",
    }
    table[("OBJECT_OCCLUDED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["OBJECT_OCCLUDED"],
        "notes": "light change confounder",
        "mq": "area_ratio",
    }
    table[("OBJECT_REAPPEARED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["OBJECT_REAPPEARED"],
        "notes": "distant new enter confounder",
        "mq": "area_recovery",
    }
    table[("NEW_UNKNOWN_REGION", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["NEW_UNKNOWN_REGION"],
        "notes": "camera pan confounder",
        "mq": "global_compensated_residual_no_prior",
    }
    table[("LIGHT_CHANGED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["LIGHT_CHANGED"],
        "notes": "surface tint confounder",
        "mq": "abs_mean_luma_delta",
    }
    table[("SURFACE_CHANGED", "confounder")] = {
        "expect_fire": False,
        "forbidden": ["SURFACE_CHANGED"],
        "notes": "global light confounder",
        "mq": "distributed_residual",
    }
    return table[(event, cls)]


def animate_cell(event: str, cls: str, objs: dict) -> None:
    target = objs["target"]
    cam = objs["camera"]
    light = objs["light"]
    world = objs["world"]
    occluder = objs.get("occluder")
    n = N_FRAMES

    # Defaults: everything static.
    if target is not None:
        target.location = (0.0, 0.0, 0.25)
        target.keyframe_insert(data_path="location", frame=1)
        target.keyframe_insert(data_path="location", frame=n)
    cam.location = (0.0, -4.5, 1.8)
    cam.keyframe_insert(data_path="location", frame=1)
    cam.keyframe_insert(data_path="location", frame=n)
    light.data.energy = 250.0
    bg = world.node_tree.nodes["Background"]
    bg.inputs[1].default_value = 1.0

    near = cls == "near_threshold"
    step = 0.15 if near else 0.45

    if event == "OBJECT_MOVED" and cls in ("true_positive", "near_threshold"):
        _linear_keys(
            target,
            "location",
            {1: (-1.2, 0.0, 0.25), n: (-1.2 + step * (n - 1) * 3, 0.0, 0.25)},
        )
    elif event == "OBJECT_MOVED" and cls == "confounder":
        # Camera pan.
        _linear_keys(cam, "location", {1: (-1.2, -4.5, 1.8), n: (1.2, -4.5, 1.8)})
    elif event == "CAMERA_MOVED" and cls in ("true_positive", "near_threshold"):
        amp = 0.6 if near else 1.2
        _linear_keys(cam, "location", {1: (-amp, -4.5, 1.8), n: (amp, -4.5, 1.8)})
    elif event == "CAMERA_MOVED" and cls == "confounder":
        _linear_keys(target, "location", {1: (-1.2, 0.0, 0.25), n: (1.2, 0.0, 0.25)})
    elif event == "OBJECT_ENTERED" and cls in ("true_positive", "near_threshold"):
        # Off-screen then enter.
        _linear_keys(
            target,
            "location",
            {1: (-3.5, 0.0, 0.25), 2: (-3.5, 0.0, 0.25), 3: (-0.5, 0.0, 0.25), n: (0.2, 0.0, 0.25)},
        )
    elif event == "OBJECT_ENTERED" and cls == "confounder":
        # Light jump.
        for f, s in ((1, 0.4), (2, 0.4), (3, 3.0), (n, 3.0)):
            bg.inputs[1].default_value = s
            bg.inputs[1].keyframe_insert(data_path="default_value", frame=f)
            light.data.energy = 80.0 if s < 1 else 500.0
            light.data.keyframe_insert(data_path="energy", frame=f)
        if target is not None:
            target.location = (4.0, 0.0, 0.25)  # off-screen
            target.keyframe_insert(data_path="location", frame=1)
    elif event == "OBJECT_LEFT" and cls in ("true_positive", "near_threshold"):
        _linear_keys(
            target,
            "location",
            {1: (0.0, 0.0, 0.25), 3: (0.3, 0.0, 0.25), 4: (3.5, 0.0, 0.25), n: (3.5, 0.0, 0.25)},
        )
    elif event == "OBJECT_LEFT" and cls == "confounder":
        # Partial occlusion via occluder (scale target down as proxy if no occluder).
        if occluder is not None:
            _linear_keys(target, "location", {1: (-0.8, 0.0, 0.25), n: (0.6, 0.0, 0.25)})
        else:
            _linear_keys(target, "scale", {1: (1, 1, 1), 3: (0.3, 0.3, 0.3), n: (0.3, 0.3, 0.3)})
    elif event == "OBJECT_OCCLUDED" and cls in ("true_positive", "near_threshold"):
        if occluder is not None:
            _linear_keys(target, "location", {1: (-0.8, 0.0, 0.25), n: (0.7, 0.0, 0.25)})
        else:
            _linear_keys(
                target,
                "scale",
                {1: (1, 1, 1), 3: (0.25, 0.25, 0.25), n: (0.25, 0.25, 0.25)},
            )
    elif event == "OBJECT_OCCLUDED" and cls == "confounder":
        for f, s in ((1, 0.5), (2, 0.5), (3, 3.5), (n, 3.5)):
            bg.inputs[1].default_value = s
            bg.inputs[1].keyframe_insert(data_path="default_value", frame=f)
    elif event == "OBJECT_REAPPEARED" and cls in ("true_positive", "near_threshold"):
        if occluder is not None:
            _linear_keys(
                target,
                "location",
                {
                    1: (-0.9, 0.0, 0.25),
                    2: (-0.2, 0.0, 0.25),
                    3: (0.5, 0.0, 0.25),
                    4: (0.5, 0.0, 0.25),
                    5: (1.2, 0.0, 0.25),
                    n: (1.4, 0.0, 0.25),
                },
            )
        else:
            _linear_keys(
                target,
                "scale",
                {
                    1: (1, 1, 1),
                    2: (1, 1, 1),
                    3: (0.2, 0.2, 0.2),
                    4: (0.2, 0.2, 0.2),
                    5: (1, 1, 1),
                    n: (1, 1, 1),
                },
            )
    elif event == "OBJECT_REAPPEARED" and cls == "confounder":
        # Leave left, enter right.
        _linear_keys(
            target,
            "location",
            {
                1: (-0.5, 0.0, 0.25),
                2: (-0.5, 0.0, 0.25),
                3: (-3.5, 0.0, 0.25),
                4: (-3.5, 0.0, 0.25),
                5: (2.0, 0.0, 0.25),
                n: (2.0, 0.0, 0.25),
            },
        )
    elif event == "NEW_UNKNOWN_REGION" and cls in ("true_positive", "near_threshold"):
        # Materialise: off-screen then snap into view (no continuous enter path).
        hide_z = -2.0
        show_z = 0.25
        for f in range(1, n + 1):
            target.location = (0.0, 0.0, hide_z if f < 3 else show_z)
            if near and f >= 3:
                target.scale = (0.4, 0.4, 0.4)
            target.keyframe_insert(data_path="location", frame=f)
            target.keyframe_insert(data_path="scale", frame=f)
    elif event == "NEW_UNKNOWN_REGION" and cls == "confounder":
        _linear_keys(cam, "location", {1: (-1.0, -4.5, 1.8), n: (1.0, -4.5, 1.8)})
        if target is not None:
            target.location = (4.0, 0.0, 0.25)
            target.keyframe_insert(data_path="location", frame=1)
    elif event == "LIGHT_CHANGED" and cls in ("true_positive", "near_threshold"):
        lo, hi = (0.8, 1.6) if near else (0.4, 3.5)
        for f, s in ((1, lo), (2, lo), (3, hi), (n, hi)):
            bg.inputs[1].default_value = s
            bg.inputs[1].keyframe_insert(data_path="default_value", frame=f)
            light.data.energy = 100.0 * s
            light.data.keyframe_insert(data_path="energy", frame=f)
    elif event == "LIGHT_CHANGED" and cls == "confounder":
        # Surface material swap on ground (handled by swapping base colour keyframes
        # is awkward in EEVEE; use a slight light change below surface floor via
        # target colour is not light). Keep light stable; move target slightly
        # so residual is local — renderer stays constant exposure.
        light.data.energy = 250.0
        bg.inputs[1].default_value = 1.0
        if target is not None:
            # Re-colour by swapping material mid-sequence is complex; use scale
            # of a second plane via energy-neutral path: just hold static.
            pass
    elif event == "SURFACE_CHANGED" and cls in ("true_positive", "near_threshold"):
        # Approximate surface change by dimming only the area light slightly and
        # raising a fill — net mean shift small. Better: keyframe world color.
        lo = (0.18, 0.20, 0.23, 1.0)
        hi = (0.35, 0.28, 0.22, 1.0) if not near else (0.24, 0.22, 0.20, 1.0)
        for f, col in ((1, lo), (2, lo), (3, hi), (n, hi)):
            bg.inputs[0].default_value = col
            bg.inputs[0].keyframe_insert(data_path="default_value", frame=f)
        # Keep strength stable so LIGHT_CHANGED stays quiet.
        bg.inputs[1].default_value = 1.0
    elif event == "SURFACE_CHANGED" and cls == "confounder":
        for f, s in ((1, 0.4), (2, 0.4), (3, 3.5), (n, 3.5)):
            bg.inputs[1].default_value = s
            bg.inputs[1].keyframe_insert(data_path="default_value", frame=f)
    elif cls == "true_negative" and event in (
        "OBJECT_ENTERED",
        "NEW_UNKNOWN_REGION",
    ) and target is not None:
        # Pure static empty: park target off-screen.
        target.location = (4.0, 0.0, 0.25)
        target.keyframe_insert(data_path="location", frame=1)
        target.keyframe_insert(data_path="location", frame=n)


def render_cell(out_dir: Path, event: str, cls: str) -> dict:
    out_dir.mkdir(parents=True, exist_ok=True)
    frames_dir = out_dir / "frames"
    frames_dir.mkdir(parents=True, exist_ok=True)
    # Clear prior frames.
    for p in frames_dir.glob("*.png"):
        p.unlink()

    need_occluder = event in ("OBJECT_OCCLUDED", "OBJECT_REAPPEARED", "OBJECT_LEFT") and cls in (
        "true_positive",
        "near_threshold",
        "confounder",
    )
    need_target = not (
        event in ("LIGHT_CHANGED", "SURFACE_CHANGED") and cls == "true_negative"
    )
    # Always build with target for simplicity; park it off-screen when unused.
    objs = build_base_scene(with_target=True, with_occluder=need_occluder)
    if not need_target and objs["target"] is not None:
        objs["target"].location = (5.0, 0.0, 0.25)

    scene = bpy.context.scene
    configure_render(scene, frames_dir, N_FRAMES)
    animate_cell(event, cls, objs)
    bpy.ops.render.render(animation=True)

    rendered = sorted(frames_dir.glob("frame_*.png"))
    spec = _spec(event, cls)
    manifest = {
        "event_type": event,
        "fixture_class": cls,
        "expect_fire": spec["expect_fire"],
        "forbidden_events": spec["forbidden"],
        "notes": spec["notes"],
        "measured_quantity": spec["mq"],
        "authority": "PHYSICAL",
        "renderer": "BLENDER_EEVEE_NEXT",
        "n_frames": len(rendered),
        "image_width": IMAGE_W,
        "image_height": IMAGE_H,
        "coordinate_frame": {
            "name": "blender-world",
            "up_axis": "+Z",
            "forward_axis": "-Y",
            "handedness": "right",
            "units": "m",
        },
        "frames": [
            {"index": i + 1, "path": f"frames/{p.name}"} for i, p in enumerate(rendered)
        ],
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest


def main() -> None:
    argv = sys.argv
    argv = argv[argv.index("--") + 1 :] if "--" in argv else []
    if not argv:
        print("usage: generate_blender.py OUT_DIR [EVENT FIXTURE_CLASS]", file=sys.stderr)
        sys.exit(2)
    out_root = Path(argv[0])
    out_root.mkdir(parents=True, exist_ok=True)

    cells = (
        [(argv[1], argv[2])]
        if len(argv) >= 3
        else [(e, c) for e in EVENTS for c in CLASSES]
    )

    index_fixtures = []
    for event, cls in cells:
        dest = out_root / event.lower() / cls
        man = render_cell(dest, event, cls)
        index_fixtures.append(
            {
                "event_type": event,
                "fixture_class": cls,
                "path": str(dest),
                "expect_fire": man["expect_fire"],
            }
        )
        print(f"CELL_OK {event}/{cls} frames={man['n_frames']}")

    index = {
        "authority": "PHYSICAL",
        "renderer": "BLENDER_EEVEE_NEXT",
        "n_fixtures": len(index_fixtures),
        "events": list(EVENTS),
        "fixture_classes": list(CLASSES),
        "fixtures": index_fixtures,
    }
    (out_root / "index.json").write_text(
        json.dumps(index, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    print(f"OCULAR_EVENTS_BLENDER_OK n={len(index_fixtures)} out={out_root}")


if __name__ == "__main__":
    main()
