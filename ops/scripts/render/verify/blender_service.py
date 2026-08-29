#!/usr/bin/env python3
"""Resident Cycles-CPU renderer for the L1 verify-pipeline bench.

Must run inside Blender:

    Blender -b -noaudio --factory-startup --python-exit-code 1 \\
        --python ops/scripts/render/verify/blender_service.py -- \\
        --workdir DIR --frames N --width 1024 --height 1024 --samples 512

Speaks one JSON object per line on stdout, prefixed with MERC_VERIFY.
Never assigns EEVEE. Adaptive sampling and denoising stay OFF. Color
management is factory AgX (same contract as the locality 1024²/512spp
measurement). use_persistent_data is ON — that is the resident mechanism,
not a quality cut.

L1 is decoded 8-bit RGB, never the PNG container and never the scene-linear
Render Result float buffer. This process writes the PNG and returns; the
orchestrator hashes the decoded pixels and overlaps that with the next frame.
"""

from __future__ import annotations

import argparse
import json
import math
import sys
import time
from pathlib import Path

MARKER = "MERC_VERIFY "


def emit(payload: dict) -> None:
    print(MARKER + json.dumps(payload, separators=(",", ":")), flush=True)


def script_args() -> list[str]:
    if "--" in sys.argv:
        return sys.argv[sys.argv.index("--") + 1 :]
    return sys.argv[1:]


def require_bpy():
    try:
        import bpy  # type: ignore
    except ImportError as exc:
        sys.stderr.write("blender_service: must run inside Blender (bpy missing)\n")
        raise SystemExit(2) from exc
    return bpy


def refuse_if_eevee(scene) -> None:
    engine = scene.render.engine
    if engine in ("BLENDER_EEVEE", "BLENDER_EEVEE_NEXT") or "EEVEE" in str(engine):
        sys.stderr.write("REFUSE: engine=%r (background EEVEE aborts, exit 134)\n" % (engine,))
        raise SystemExit(2)


def pin_cycles_cpu(bpy, scene) -> None:
    scene.render.engine = "CYCLES"
    addon = bpy.context.preferences.addons.get("cycles")
    if addon is not None and hasattr(addon.preferences, "compute_device_type"):
        addon.preferences.compute_device_type = "NONE"
        if hasattr(addon.preferences, "get_devices"):
            try:
                addon.preferences.get_devices()
            except Exception:
                pass
        for dev in getattr(addon.preferences, "devices", []):
            try:
                dev.use = str(dev.type) == "CPU"
            except Exception:
                pass
    scene.cycles.device = "CPU"
    refuse_if_eevee(scene)
    if scene.cycles.device != "CPU":
        sys.stderr.write("REFUSE: cycles.device=%r\n" % (scene.cycles.device,))
        raise SystemExit(2)


def apply_settings(scene, *, width: int, height: int, samples: int, seed: int) -> None:
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
    # Resident mechanism: keep BVH / shaders / kernels across frames.
    scene.render.use_persistent_data = True
    scene.render.use_compositing = False
    scene.render.use_sequencer = False
    scene.render.use_border = False
    scene.render.use_crop_to_border = False

    scene.view_settings.view_transform = "AgX"
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
    cyc.max_bounces = 12
    cyc.diffuse_bounces = 4
    cyc.glossy_bounces = 4
    cyc.transmission_bounces = 12
    cyc.volume_bounces = 0
    cyc.transparent_max_bounces = 8
    cyc.filter_glossy = 1.0
    cyc.sample_clamp_direct = 0.0
    cyc.sample_clamp_indirect = 10.0
    cyc.caustics_reflective = False
    cyc.caustics_refractive = False
    cyc.use_fast_gi = False
    cyc.pixel_filter_type = "BLACKMAN_HARRIS"
    cyc.filter_width = 1.5
    if hasattr(cyc, "use_auto_tile"):
        cyc.use_auto_tile = False


def build_scene(bpy, scene) -> dict:
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
    scene.camera = cam

    world = bpy.data.worlds.new("World")
    world.use_nodes = True
    bg = world.node_tree.nodes["Background"]
    bg.inputs["Color"].default_value = (0.03, 0.03, 0.04, 1.0)
    bg.inputs["Strength"].default_value = 0.40
    scene.world = world
    return {"primitives": ["plane", "monkey", "area_light", "camera"], "assets_downloaded": False}


def look_at(obj, target: tuple[float, float, float]) -> None:
    from mathutils import Vector  # type: ignore

    direction = Vector(target) - obj.location
    obj.rotation_euler = direction.to_track_quat("-Z", "Y").to_euler()


def set_camera_frame(cam, frame: int, n_frames: int) -> None:
    # Orbit so the project is a real multi-frame animation, not N copies.
    t = 2.0 * math.pi * (frame / max(n_frames, 1))
    radius = 9.2
    cam.location = (radius * math.cos(t), radius * math.sin(t), 4.2)
    look_at(cam, (0.0, 0.0, 1.2))


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="blender_service")
    p.add_argument("--workdir", required=True)
    p.add_argument("--frames", type=int, default=8)
    p.add_argument("--width", type=int, default=1024)
    p.add_argument("--height", type=int, default=1024)
    p.add_argument("--samples", type=int, default=512)
    p.add_argument("--seed", type=int, default=1)
    p.add_argument("--start-frame", type=int, default=0)
    return p.parse_args(argv)


def main() -> int:
    args = parse_args(script_args())
    if args.frames < 1 or args.width < 16 or args.height < 16 or args.samples < 1:
        sys.stderr.write("REFUSE: invalid frames/width/height/samples\n")
        return 2
    workdir = Path(args.workdir)
    workdir.mkdir(parents=True, exist_ok=True)

    bpy = require_bpy()
    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    pin_cycles_cpu(bpy, scene)
    apply_settings(
        scene, width=args.width, height=args.height, samples=args.samples, seed=args.seed
    )
    meta = build_scene(bpy, scene)
    pin_cycles_cpu(bpy, scene)
    if scene.cycles.use_adaptive_sampling or scene.cycles.use_denoising:
        sys.stderr.write("REFUSE: adaptive or denoise turned on\n")
        return 2
    refuse_if_eevee(scene)

    emit(
        {
            "ok": True,
            "op": "BUILD",
            "engine": scene.render.engine,
            "device": scene.cycles.device,
            "width": args.width,
            "height": args.height,
            "samples": args.samples,
            "seed": args.seed,
            "frames": args.frames,
            "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
            "denoising": bool(scene.cycles.use_denoising),
            "view_transform": scene.view_settings.view_transform,
            "persistent_data": bool(scene.render.use_persistent_data),
            "eevee_invoked": False,
            "scene": meta,
        }
    )

    cam = scene.camera
    for i in range(args.frames):
        frame = args.start_frame + i
        set_camera_frame(cam, frame, args.frames)
        scene.frame_set(frame + 1)
        png = workdir / ("frame_%04d.png" % frame)
        scene.render.filepath = str(png.with_suffix(""))
        t0 = time.perf_counter()
        _ = bpy.context.evaluated_depsgraph_get()
        t_dg = time.perf_counter()
        bpy.ops.render.render(write_still=True)
        t_render = time.perf_counter()
        written = png if png.is_file() else Path(str(png) + ".png")
        if not written.is_file():
            candidate = Path(scene.render.filepath + ".png")
            if candidate.is_file():
                written = candidate
        if not written.is_file():
            emit({"ok": False, "op": "RENDER", "frame": frame, "error": "no PNG written"})
            return 2
        # Do not decode the PNG here. AgX 8-bit pixels exist in the encoder
        # buffer, but bpy exposes Render Result as scene-linear float (L2).
        # L1 is decoded 8-bit RGB; the orchestrator hashes that (Go, ~30 ms)
        # overlapped with the next frame. A Python decode here would add
        # ~0.27 s of serial worker tail per frame.
        emit(
            {
                "ok": True,
                "op": "RENDER",
                "frame": frame,
                "png": str(written),
                "width": int(scene.render.resolution_x),
                "height": int(scene.render.resolution_y),
                "depsgraph_s": t_dg - t0,
                "render_s": t_render - t_dg,
                "digest_s": 0.0,
                "wall_s": time.perf_counter() - t0,
                "engine": scene.render.engine,
                "device": scene.cycles.device,
                "samples": scene.cycles.samples,
                "seed": scene.cycles.seed,
                "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
                "denoising": bool(scene.cycles.use_denoising),
                "persistent_data": bool(scene.render.use_persistent_data),
            }
        )
    emit({"ok": True, "op": "DONE", "frames": args.frames})
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
