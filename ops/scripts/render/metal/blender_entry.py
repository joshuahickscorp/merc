#!/usr/bin/env python3
"""Blender-side Cycles CPU / Metal GPU renderer for the Metal lane.

    blender -b -noaudio --factory-startup --python-exit-code 1 \\
        --python ops/scripts/render/metal/blender_entry.py -- \\
        --mode=probe|render --device=CPU|GPU --scene=trivial --out PATH

Refuses EEVEE. A GPU request that cannot enable a Metal device exits 2
rather than falling back to CPU.
"""

from __future__ import annotations

import argparse
import array
import hashlib
import json
import os
import struct
import sys
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parents[4]
SCRIPT_ROOT = REPO_ROOT / "ops" / "scripts"
if str(SCRIPT_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPT_ROOT))


def script_args() -> list[str]:
    if "--" in sys.argv:
        return sys.argv[sys.argv.index("--") + 1 :]
    return sys.argv[1:]


def emit(payload: dict) -> None:
    print("MERC_CYCLES_METAL " + json.dumps(payload, separators=(",", ":"), default=str), flush=True)


def linear_buffer(bpy) -> dict:
    img = bpy.data.images.get("Render Result")
    if img is None:
        return {"status": "UNAVAILABLE", "reason": "no Render Result image"}
    w, h = int(img.size[0]), int(img.size[1])
    ch = int(img.channels) if img.channels else 4
    n = w * h * ch
    if n <= 0:
        return {"status": "UNAVAILABLE", "reason": "empty Render Result", "size": [w, h, ch]}
    try:
        buf = array.array("f", [0.0]) * n
        img.pixels.foreach_get(buf)
    except Exception as exc:
        return {"status": "UNAVAILABLE", "reason": f"foreach_get: {type(exc).__name__}: {exc}"}
    le = b"".join(struct.pack("<f", v) for v in buf)
    return {
        "status": "MEASURED",
        "width": w,
        "height": h,
        "channels": ch,
        "n_floats": n,
        "linear_buffer_sha256": hashlib.sha256(le).hexdigest(),
        "note": "scene-referred float pixels from Render Result before PNG view transform",
    }


def blender_info(bpy) -> dict:
    def dec(v):
        if isinstance(v, bytes):
            return v.decode("utf-8", "replace")
        return str(v)

    info = {
        "blender_version": list(bpy.app.version),
        "blender_version_string": bpy.app.version_string,
        "blender_build_hash": dec(bpy.app.build_hash),
        "blender_build_branch": dec(bpy.app.build_branch),
        "blender_build_platform": dec(bpy.app.build_platform),
        "blender_binary": bpy.app.binary_path,
        "pid": os.getpid(),
    }
    try:
        import _cycles  # type: ignore

        info["cycles_module_file"] = getattr(_cycles, "__file__", None)
        info["cycles_with_osl"] = bool(getattr(_cycles, "with_osl", False))
        info["cycles_with_embree"] = bool(getattr(_cycles, "with_embree", False))
        if hasattr(_cycles, "available_devices"):
            try:
                info["available_devices"] = _cycles.available_devices()
            except TypeError:
                try:
                    info["available_devices_metal"] = _cycles.available_devices("METAL")
                except Exception as exc:
                    info["available_devices_error"] = f"{type(exc).__name__}:{exc}"
            except Exception as exc:
                info["available_devices_error"] = f"{type(exc).__name__}:{exc}"
    except Exception as exc:
        info["cycles_module_error"] = f"{type(exc).__name__}:{exc}"
    return info


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(prog="blender_entry")
    p.add_argument("--mode", required=True, choices=("probe", "render", "sweep"))
    p.add_argument("--device", default="GPU", choices=("CPU", "GPU"))
    p.add_argument("--metal-rt", default="OFF", choices=("OFF", "ON", "AUTO"))
    p.add_argument("--scene", default="trivial")
    p.add_argument("--out", default="")
    p.add_argument("--out-dir", default="")
    p.add_argument("--repeats", type=int, default=1)
    p.add_argument("--adaptive", type=int, default=0, choices=(0, 1))
    p.add_argument("--persistent", type=int, default=0, choices=(0, 1))
    p.add_argument("--generated", default="")
    p.add_argument("--dump-linear", type=int, default=1, choices=(0, 1))
    p.add_argument("--width", type=int, default=0, help="0 = use scene record")
    p.add_argument("--height", type=int, default=0, help="0 = use scene record")
    p.add_argument("--samples", type=int, default=0, help="0 = use scene record")
    p.add_argument(
        "--cells-json",
        default="",
        help="sweep: JSON list of {tag,width,height,samples} cells",
    )
    return p.parse_args(argv)


def apply_cell(scene, width: int, height: int, samples: int) -> None:
    scene.render.resolution_x = int(width)
    scene.render.resolution_y = int(height)
    scene.render.resolution_percentage = 100
    scene.cycles.samples = int(samples)


def main() -> int:
    args = parse_args(script_args())
    t_start = time.perf_counter()
    try:
        import bpy  # type: ignore
    except ImportError:
        sys.stderr.write("blender_entry: must run inside Blender (bpy missing)\n")
        return 2

    from render.metal.device import assert_device, cycles_prefs, pin_cycles, refuse_eevee
    from render.metal.scenes import build, get_record, reset_scene, scene_identity

    bpy.ops.wm.read_factory_settings(use_empty=True)
    scene = bpy.context.scene
    # Factory-startup defaults to EEVEE. Switch before any render.
    scene.render.engine = "CYCLES"
    refuse_eevee(scene.render.engine)

    record = dict(get_record(args.scene))
    if args.width > 0 and args.height > 0:
        record["resolution"] = [int(args.width), int(args.height)]
    if args.samples > 0:
        record["samples"] = int(args.samples)
    generated = Path(args.generated) if args.generated else Path(os.environ.get("MERC_RENDER_GENERATED", "/tmp/merc-metal-generated"))
    generated.mkdir(parents=True, exist_ok=True)
    paths = {"generated": generated}

    t_pin0 = time.perf_counter()
    assertion = pin_cycles(
        scene,
        record,
        device=args.device,
        metal_rt=args.metal_rt,
        adaptive=bool(args.adaptive),
        persistent_data=bool(args.persistent),
    )
    t_pin1 = time.perf_counter()

    if args.mode == "probe":
        emit(
            {
                "mode": "probe",
                "ok": True,
                "blender": blender_info(bpy),
                "assertion": assertion,
                "pin_s": t_pin1 - t_pin0,
                "wall_s": time.perf_counter() - t_start,
                "scene_id": args.scene,
                "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
                "samples": int(scene.cycles.samples),
                "seed": int(scene.cycles.seed),
            }
        )
        return 0

    if args.mode == "render" and not args.out:
        sys.stderr.write("REFUSE: --out is required for render\n")
        return 2
    if args.mode == "sweep" and not args.out_dir:
        sys.stderr.write("REFUSE: --out-dir is required for sweep\n")
        return 2
    if args.repeats < 1:
        sys.stderr.write("REFUSE: --repeats must be >= 1\n")
        return 2

    t_build0 = time.perf_counter()
    reset_scene()
    build(record["builder"], record["spec"], paths)
    # Pin again after reset_scene wipes engine/device.
    assertion = pin_cycles(
        scene,
        record,
        device=args.device,
        metal_rt=args.metal_rt,
        adaptive=bool(args.adaptive),
        persistent_data=bool(args.persistent),
    )
    t_build1 = time.perf_counter()
    identity = scene_identity()

    if args.mode == "sweep":
        return run_sweep(
            bpy,
            scene,
            record,
            assertion,
            args,
            identity,
            t_start,
            t_pin0,
            t_pin1,
            t_build0,
            t_build1,
        )

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    repeats = []
    last_linear = None
    for i in range(args.repeats):
        dest = out_path if args.repeats == 1 else out_path.with_name(f"{out_path.stem}_r{i:02d}{out_path.suffix}")
        scene.render.filepath = str(dest)
        t0 = time.perf_counter()
        bpy.ops.render.render(write_still=True)
        t1 = time.perf_counter()
        written = scene.render.filepath
        if not os.path.isfile(written) and os.path.isfile(written + ".png"):
            written = written + ".png"
        # Read Render Result BEFORE re-pinning — pin_cycles rewrites settings
        # and can drop the linear buffer.
        linear = linear_buffer(bpy) if args.dump_linear else {"status": "SKIPPED"}
        last_linear = linear
        # Re-assert after render so a mid-render fallback cannot hide.
        assertion = pin_cycles(
            scene,
            record,
            device=args.device,
            metal_rt=args.metal_rt,
            adaptive=bool(args.adaptive),
            persistent_data=bool(args.persistent),
        )
        repeats.append(
            {
                "i": i,
                "wall_s": t1 - t0,
                "out": written,
                "out_exists": os.path.isfile(written),
                "out_bytes": os.path.getsize(written) if os.path.isfile(written) else 0,
                "linear": linear,
            }
        )
        if args.repeats > 1 and dest != out_path and os.path.isfile(written):
            # keep last frame also at --out for the harness
            if i == args.repeats - 1:
                try:
                    Path(out_path).write_bytes(Path(written).read_bytes())
                except OSError:
                    pass

    emit(
        {
            "mode": "render",
            "ok": True,
            "blender": blender_info(bpy),
            "assertion": assertion,
            "scene_id": record["id"],
            "scene_class": record["class"],
            "builder": record["builder"],
            "engine": scene.render.engine,
            "device": scene.cycles.device,
            "compute_device_type": assertion.get("compute_device_type"),
            "backend": assertion.get("backend"),
            "metal_rt_requested": args.metal_rt,
            "metalrt": assertion.get("metalrt"),
            "width": int(scene.render.resolution_x),
            "height": int(scene.render.resolution_y),
            "samples": int(scene.cycles.samples),
            "seed": int(scene.cycles.seed),
            "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
            "denoising": bool(scene.cycles.use_denoising),
            "persistent_data": bool(scene.render.use_persistent_data),
            "view_transform": scene.view_settings.view_transform,
            "display_device": scene.display_settings.display_device,
            "max_bounces": int(scene.cycles.max_bounces),
            "pin_s": t_pin1 - t_pin0,
            "build_s": t_build1 - t_build0,
            "identity": identity,
            "repeats": repeats,
            "last_linear": last_linear,
            "wall_s": time.perf_counter() - t_start,
        }
    )
    return 0


def run_sweep(
    bpy,
    scene,
    record,
    assertion,
    args,
    identity,
    t_start,
    t_pin0,
    t_pin1,
    t_build0,
    t_build1,
) -> int:
    raw = args.cells_json
    if raw.startswith("@"):
        raw = Path(raw[1:]).read_text(encoding="utf-8")
    try:
        cells = json.loads(raw)
    except json.JSONDecodeError as exc:
        sys.stderr.write("REFUSE: --cells-json is not JSON: %s\n" % exc)
        return 2
    if not isinstance(cells, list) or not cells:
        sys.stderr.write("REFUSE: --cells-json must be a non-empty list\n")
        return 2

    from render.metal.device import assert_device, cycles_prefs

    out_dir = Path(args.out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    prefs = cycles_prefs(bpy)
    emit(
        {
            "mode": "sweep_begin",
            "ok": True,
            "blender": blender_info(bpy),
            "assertion": assertion,
            "scene_id": record["id"],
            "scene_class": record["class"],
            "builder": record["builder"],
            "identity": identity,
            "n_cells": len(cells),
            "device": args.device,
            "pin_s": t_pin1 - t_pin0,
            "build_s": t_build1 - t_build0,
        }
    )

    for i, cell in enumerate(cells):
        tag = str(cell.get("tag") or "cell_%03d" % i)
        width = int(cell["width"])
        height = int(cell["height"])
        samples = int(cell["samples"])
        if width < 1 or height < 1 or samples < 1:
            sys.stderr.write("REFUSE: cell %s has non-positive size/samples\n" % tag)
            return 2
        apply_cell(scene, width, height, samples)
        dest = out_dir / ("%s.png" % tag)
        scene.render.filepath = str(dest)
        t0 = time.perf_counter()
        bpy.ops.render.render(write_still=True)
        t1 = time.perf_counter()
        written = scene.render.filepath
        if not os.path.isfile(written) and os.path.isfile(written + ".png"):
            written = written + ".png"
        linear = linear_buffer(bpy) if args.dump_linear else {"status": "SKIPPED"}
        assertion = assert_device(scene, prefs, args.device.upper())
        emit(
            {
                "mode": "sweep_cell",
                "ok": True,
                "i": i,
                "tag": tag,
                "scene_id": record["id"],
                "device": scene.cycles.device,
                "backend": assertion.get("backend"),
                "compute_device_type": assertion.get("compute_device_type"),
                "width": int(scene.render.resolution_x),
                "height": int(scene.render.resolution_y),
                "samples": int(scene.cycles.samples),
                "seed": int(scene.cycles.seed),
                "adaptive_sampling": bool(scene.cycles.use_adaptive_sampling),
                "denoising": bool(scene.cycles.use_denoising),
                "wall_s": t1 - t0,
                "out": written,
                "out_exists": os.path.isfile(written),
                "out_bytes": os.path.getsize(written) if os.path.isfile(written) else 0,
                "linear": linear,
                "assertion": assertion,
            }
        )

    emit(
        {
            "mode": "sweep_end",
            "ok": True,
            "scene_id": record["id"],
            "device": args.device,
            "n_cells": len(cells),
            "identity": identity,
            "assertion": assertion,
            "wall_s": time.perf_counter() - t_start,
        }
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
