#!/usr/bin/env python3
"""Blender-side scene builder / Cycles-CPU renderer for the P4 baseline.

Executed inside Blender, never as system Python:

    /Applications/Blender.app/Contents/MacOS/Blender -b -noaudio --factory-startup \\
        --python scripts/blender-render-baseline.py -- \\
        --mode=build|full|tile --blend PATH [--out PATH] [--tile-x I --tile-y J --grid N]

Refuses to continue unless the engine is CYCLES and cycles.device is CPU.
Never assigns BLENDER_EEVEE / BLENDER_EEVEE_NEXT (background EEVEE aborts
the process with exit 134). Scene is built from built-in primitives only.
"""

from __future__ import annotations

import argparse
import json
import os
import sys


def script_args() -> list[str]:
    if "--" in sys.argv:
        return sys.argv[sys.argv.index("--") + 1 :]
    return sys.argv[1:]


def require_bpy():
    try:
        import bpy  # type: ignore
    except ImportError as exc:
        sys.stderr.write("blender-render-baseline: must run inside Blender (bpy missing)\n")
        raise SystemExit(2) from exc
    return bpy


def require_cycles_cpu(bpy, scene) -> None:
    engine = scene.render.engine
    if engine != "CYCLES":
        sys.stderr.write("REFUSE: engine=%r, want CYCLES\n" % (engine,))
        raise SystemExit(2)
    device = getattr(scene.cycles, "device", None)
    if device != "CPU":
        sys.stderr.write("REFUSE: cycles.device=%r, want CPU\n" % (device,))
        raise SystemExit(2)
    addon = bpy.context.preferences.addons.get("cycles")
    if addon is not None:
        compute = getattr(addon.preferences, "compute_device_type", None)
        if compute not in (None, "NONE", "CPU"):
            sys.stderr.write("REFUSE: cycles compute_device_type=%r, want NONE/CPU\n" % (compute,))
            raise SystemExit(2)


def pin_cycles_cpu(bpy, scene) -> None:
    scene.render.engine = "CYCLES"
    addon = bpy.context.preferences.addons.get("cycles")
    if addon is not None and hasattr(addon.preferences, "compute_device_type"):
        addon.preferences.compute_device_type = "NONE"
    scene.cycles.device = "CPU"
    require_cycles_cpu(bpy, scene)


def apply_render_settings(scene, *, width: int, height: int, samples: int, seed: int) -> None:
    scene.render.resolution_x = width
    scene.render.resolution_y = height
    scene.render.resolution_percentage = 100
    scene.render.use_file_extension = True
    scene.render.image_settings.file_format = "PNG"
    scene.render.image_settings.color_mode = "RGB"
    scene.render.image_settings.color_depth = "8"
    scene.render.image_settings.compression = 15
    scene.render.film_transparent = False
    scene.render.threads_mode = "AUTO"
    scene.render.use_persistent_data = False
    scene.render.use_compositing = False
    scene.render.use_sequencer = False

    scene.view_settings.view_transform = "Standard"
    scene.view_settings.look = "None"
    scene.view_settings.exposure = 0.0
    scene.view_settings.gamma = 1.0
    scene.display_settings.display_device = "sRGB"

    cyc = scene.cycles
    cyc.samples = samples
    cyc.use_adaptive_sampling = False
    cyc.use_denoising = False
    cyc.seed = seed
    cyc.use_animated_seed = False
    cyc.max_bounces = 4
    cyc.diffuse_bounces = 2
    cyc.glossy_bounces = 2
    cyc.transmission_bounces = 2
    cyc.transparent_max_bounces = 2
    cyc.pixel_filter_type = "BLACKMAN_HARRIS"
    cyc.filter_width = 1.5
    if hasattr(cyc, "use_auto_tile"):
        cyc.use_auto_tile = False


def build_procedural_scene(bpy, scene) -> dict:
    """Built-in Suzanne + plane + area light. No downloaded assets."""
    bpy.ops.mesh.primitive_plane_add(size=8.0, location=(0.0, 0.0, 0.0))
    ground = bpy.context.object
    ground.name = "Ground"
    mat_g = bpy.data.materials.new("Ground")
    mat_g.use_nodes = True
    bsdf_g = mat_g.node_tree.nodes["Principled BSDF"]
    bsdf_g.inputs["Base Color"].default_value = (0.15, 0.16, 0.18, 1.0)
    bsdf_g.inputs["Roughness"].default_value = 0.70
    ground.data.materials.append(mat_g)

    bpy.ops.mesh.primitive_monkey_add(size=2.0, location=(0.0, 0.0, 1.2))
    monkey = bpy.context.object
    monkey.name = "Suzanne"
    bpy.ops.object.shade_smooth()
    mat_m = bpy.data.materials.new("Suzanne")
    mat_m.use_nodes = True
    bsdf_m = mat_m.node_tree.nodes["Principled BSDF"]
    bsdf_m.inputs["Base Color"].default_value = (0.80, 0.25, 0.12, 1.0)
    bsdf_m.inputs["Roughness"].default_value = 0.35
    monkey.data.materials.append(mat_m)

    bpy.ops.object.light_add(type="AREA", location=(4.0, -3.5, 6.0))
    light = bpy.context.object
    light.name = "Key"
    light.data.energy = 800.0
    light.data.size = 3.0
    light.rotation_euler = (0.70, 0.20, 0.40)

    bpy.ops.object.camera_add(location=(6.5, -6.5, 4.2))
    cam = bpy.context.object
    cam.name = "Camera"
    cam.rotation_euler = (1.15, 0.0, 0.785)
    scene.camera = cam

    world = bpy.data.worlds.new("World")
    world.use_nodes = True
    bg = world.node_tree.nodes["Background"]
    bg.inputs["Color"].default_value = (0.03, 0.03, 0.04, 1.0)
    bg.inputs["Strength"].default_value = 0.40
    scene.world = world

    return {
        "primitives": ["plane", "monkey", "area_light", "camera"],
        "assets_downloaded": False,
        "rights": "built-in Blender primitives generated in-process; no third-party files",
    }


def set_full_frame(scene) -> None:
    scene.render.use_border = False
    scene.render.use_crop_to_border = False


def set_tile_border(scene, tile_x: int, tile_y: int, grid: int) -> None:
    """(0,0) is the top-left of the output image. Blender border Y is bottom-up."""
    if grid < 1 or tile_x < 0 or tile_y < 0 or tile_x >= grid or tile_y >= grid:
        sys.stderr.write("REFUSE: tile (%d,%d) outside grid %d\n" % (tile_x, tile_y, grid))
        raise SystemExit(2)
    min_x = tile_x / grid
    max_x = (tile_x + 1) / grid
    min_y = (grid - 1 - tile_y) / grid
    max_y = (grid - tile_y) / grid
    scene.render.use_border = True
    scene.render.use_crop_to_border = True
    scene.render.border_min_x = min_x
    scene.render.border_max_x = max_x
    scene.render.border_min_y = min_y
    scene.render.border_max_y = max_y


def emit_marker(payload: dict) -> None:
    print("MERC_BLENDER_RENDER " + json.dumps(payload, separators=(",", ":")), flush=True)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="blender-render-baseline")
    p.add_argument("--mode", required=True, choices=("build", "full", "tile"))
    p.add_argument("--blend", required=True)
    p.add_argument("--out", default="")
    p.add_argument("--width", type=int, default=256)
    p.add_argument("--height", type=int, default=256)
    p.add_argument("--samples", type=int, default=32)
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--tile-x", type=int, default=0)
    p.add_argument("--tile-y", type=int, default=0)
    p.add_argument("--grid", type=int, default=2)
    return p.parse_args(argv)


def main() -> int:
    args = parse_args(script_args())
    if args.width < 16 or args.height < 16 or args.samples < 1 or args.seed < 0:
        sys.stderr.write("REFUSE: invalid width/height/samples/seed\n")
        return 2
    if args.mode != "build" and not args.out:
        sys.stderr.write("REFUSE: --out is required for render modes\n")
        return 2

    bpy = require_bpy()
    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    pin_cycles_cpu(bpy, scene)

    if args.mode == "build":
        apply_render_settings(
            scene, width=args.width, height=args.height, samples=args.samples, seed=args.seed
        )
        meta = build_procedural_scene(bpy, scene)
        pin_cycles_cpu(bpy, scene)
        os.makedirs(os.path.dirname(os.path.abspath(args.blend)) or ".", exist_ok=True)
        bpy.ops.wm.save_as_mainfile(filepath=os.path.abspath(args.blend))
        emit_marker(
            {
                "mode": "build",
                "engine": scene.render.engine,
                "device": scene.cycles.device,
                "samples": int(scene.cycles.samples),
                "seed": int(scene.cycles.seed),
                "width": int(scene.render.resolution_x),
                "height": int(scene.render.resolution_y),
                "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
                "denoising": bool(scene.cycles.use_denoising),
                "blend": os.path.abspath(args.blend),
                "scene": meta,
            }
        )
        return 0

    blend = os.path.abspath(args.blend)
    if not os.path.isfile(blend):
        sys.stderr.write("REFUSE: blend missing: %s\n" % blend)
        return 2
    bpy.ops.wm.open_mainfile(filepath=blend)
    scene = bpy.context.scene
    pin_cycles_cpu(bpy, scene)
    apply_render_settings(
        scene, width=args.width, height=args.height, samples=args.samples, seed=args.seed
    )
    pin_cycles_cpu(bpy, scene)

    tile_x = tile_y = grid = 0
    if args.mode == "tile":
        tile_x, tile_y, grid = args.tile_x, args.tile_y, args.grid
        set_tile_border(scene, tile_x, tile_y, grid)
    else:
        set_full_frame(scene)

    out = os.path.abspath(args.out)
    os.makedirs(os.path.dirname(out) or ".", exist_ok=True)
    scene.render.filepath = out
    require_cycles_cpu(bpy, scene)
    bpy.ops.render.render(write_still=True)
    written = scene.render.filepath
    if not os.path.isfile(out) and os.path.isfile(out + ".png"):
        written = out + ".png"
    emit_marker(
        {
            "mode": args.mode,
            "engine": scene.render.engine,
            "device": scene.cycles.device,
            "samples": int(scene.cycles.samples),
            "seed": int(scene.cycles.seed),
            "width": int(scene.render.resolution_x),
            "height": int(scene.render.resolution_y),
            "use_border": bool(scene.render.use_border),
            "crop_to_border": bool(scene.render.use_crop_to_border),
            "tile_x": tile_x,
            "tile_y": tile_y,
            "grid": grid,
            "out": written,
        }
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
