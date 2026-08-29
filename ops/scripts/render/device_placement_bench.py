#!/usr/bin/env python3
"""Cycles CPU vs Metal device-placement sweep.

    python3 ops/scripts/render/device_placement_bench.py --self-test
    MERC_RENDER_DEVICE_PLACEMENT=1 python3 ops/scripts/render/device_placement_bench.py --write-evidence

Finds the resolution × samples × complexity curve where Metal overtakes
CPU on this host, evaluates gpu_placement_license() against that curve,
quantifies L1 CPU-vs-GPU divergence, and measures kernel-compile cost
(first use vs cached across processes).

Same samples, seed, bounces, AgX, adaptive-OFF, denoise-OFF on BOTH
arms. A GPU request that cannot enable Metal is a hard failure.
Never launches EEVEE. Does not claim a 10,000x / 73,000x anything.
This axis moves per-worker speedup (GPU only where it wins).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[3]
SOURCE_ROOT = ROOT / "src"
if str(SOURCE_ROOT) not in sys.path:
    sys.path.insert(0, str(SOURCE_ROOT))
if str(ROOT / "ops/scripts") not in sys.path:
    sys.path.insert(0, str(ROOT / "ops/scripts"))

from lib.device_placement import (  # noqa: E402
    CORPUS_COMPLEXITY,
    GPU_KERNEL_COMPILE_COLD_S,
    L1_INTERCHANGEABLE,
    WIN_MARGIN,
    complexity_class,
    corpus_license,
    gpu_placement_license,
    pixel_samples,
    self_test as predicate_self_test,
)
from render.lib.pngutil import container_sha256, decode_png_rgb8, pixel_l1_stats  # noqa: E402
from render.metal.scenes import SCENE_RECORDS, validate_records  # noqa: E402

ENV_GATE = "MERC_RENDER_DEVICE_PLACEMENT"
EVIDENCE_REL = "evidence/perf/cycles-device-placement.json"
DEFAULT_BLENDER = "/Applications/Blender.app/Contents/MacOS/Blender"
ENTRY_REL = "src/render/metal/blender_entry.py"

SCENES = ("trivial", "dense_geometry", "many_instances", "principled_graph")

# Resident curve. 256²/64 is in both axes on purpose (one render).
SAMPLES_AT_256 = (16, 32, 64, 128, 256, 512)
RES_AT_64 = (128, 256, 384, 512, 768, 1024)
MID_CELLS = (
    (512, 128),
    (512, 256),
    (768, 128),
    (768, 256),
    (1024, 128),
    (1024, 256),
)
# Always remasure the published heavy win on trivial. Other scenes'
# 1024²/512 sits behind --include-expensive.
ALWAYS_HEAVY = (("trivial", 1024, 512),)

MARKER_RE = re.compile(r"(?m)^MERC_CYCLES_METAL\s+(\{.*\})\s*$")
TIME_RE = re.compile(r"(?m)^Time:\s*(\d+):(\d+\.\d+)")
RSS_RE = re.compile(r"(?m)^\s*(\d+)\s+maximum resident set size\s*$")
KERNEL_RE = re.compile(r"(\d+)\s*/\s*(\d+)\s+render kernels loaded")
FALLBACK_RE = re.compile(
    r"(?i)(falling back to cpu|fallback to cpu|no metal device|failed to create metal|using cpu device)"
)


def utc_now() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def git_source_commit(root: Path) -> str:
    head = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
    dirty = subprocess.run(
        ["git", "-C", str(root), "diff", "--name-only", "HEAD"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    untracked = subprocess.run(
        ["git", "-C", str(root), "ls-files", "--others", "--exclude-standard"],
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    return f"{head}-dirty" if dirty or untracked else head


def blender_version(bin_path: str) -> str:
    out = subprocess.check_output([bin_path, "--version"], text=True, stderr=subprocess.STDOUT)
    lines = [ln.strip() for ln in out.splitlines() if ln.strip()]
    first = lines[0] if lines else out.strip()
    h = ""
    for ln in lines:
        if ln.startswith("build hash:"):
            h = ln.split(":", 1)[1].strip()
            break
    return f"{first} hash {h}" if h else first


def parse_markers(text: str) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for m in MARKER_RE.finditer(text):
        try:
            out.append(json.loads(m.group(1)))
        except json.JSONDecodeError:
            continue
    return out


def parse_blender_times(text: str) -> list[float]:
    out: list[float] = []
    for minutes, seconds in TIME_RE.findall(text):
        out.append(int(minutes) * 60 + float(seconds))
    return out


def parse_rss(text: str) -> int:
    matches = RSS_RE.findall(text)
    return int(matches[-1]) if matches else 0


def parse_kernel_progress(text: str) -> dict[str, Any]:
    hits = KERNEL_RE.findall(text)
    last = None
    if hits:
        last = {"loaded": int(hits[-1][0]), "total": int(hits[-1][1])}
    return {
        "kernel_load_mentioned": (
            "Loading render kernels" in text or "render kernels loaded" in text
        ),
        "progress_hits": [{"loaded": int(a), "total": int(b)} for a, b in hits],
        "last": last,
        "first_time_warning": "may take a few minutes the first time" in text,
        "using_optimized": "Using optimized kernels" in text,
    }


def ratio(a: float | None, b: float | None) -> float | None:
    if a is None or b is None or b == 0:
        return None
    return a / b


def compare_pngs(a: str, b: str) -> dict[str, Any]:
    wa, ha, ra = decode_png_rgb8(a)
    wb, hb, rb = decode_png_rgb8(b)
    if (wa, ha) != (wb, hb):
        return {
            "pixel_exact": False,
            "comparable": False,
            "reason": f"size {wa}x{ha} vs {wb}x{hb}",
            "level": "L1_PIXEL_EXACT",
            "container_equal": container_sha256(a) == container_sha256(b),
            "container_note": "PNG file hashes are not an equivalence key (eXIf/tEXt)",
        }
    stats = pixel_l1_stats(ra, rb, wa, ha)
    stats["container_equal"] = container_sha256(a) == container_sha256(b)
    stats["container_note"] = "PNG file hashes are not an equivalence key (eXIf/tEXt)"
    stats["size_a"] = [wa, ha]
    stats["size_b"] = [wb, hb]
    return stats


def find_kernel_cache(cwd: Path) -> dict[str, Any]:
    homes = Path.home()
    candidates = [
        cwd / "cycles_metal_integrator_s",
        cwd / "cycles_metal_integrator_a",
        homes / "Library" / "Caches" / "blender",
        homes / "Library" / "Caches" / "com.apple.metal",
        homes / "Library" / "Caches" / "com.apple.metalfe",
        homes / "Library" / "Application Support" / "Blender" / "4.2" / "cache",
    ]
    found: list[dict[str, Any]] = []
    for base in candidates:
        if not base.exists():
            continue
        n_files = 0
        n_bytes = 0
        try:
            if base.is_file():
                n_files = 1
                n_bytes = base.stat().st_size
            else:
                for p in base.rglob("*"):
                    if p.is_file():
                        n_files += 1
                        n_bytes += p.stat().st_size
        except OSError:
            continue
        found.append(
            {
                "path": str(base),
                "exists": True,
                "files": n_files,
                "bytes": n_bytes,
                "cwd_relative": str(base).startswith(str(cwd)),
            }
        )
    return {
        "cwd": str(cwd),
        "entries": found,
        "cwd_integrator_present": any(
            e["cwd_relative"] and e["files"] > 0 for e in found
        ),
    }


def build_grid(*, include_expensive: bool) -> list[dict[str, Any]]:
    cells: list[dict[str, Any]] = []
    seen: set[tuple[str, int, int, int]] = set()

    def add(scene_id: str, width: int, height: int, samples: int, family: str) -> None:
        key = (scene_id, width, height, samples)
        if key in seen:
            return
        seen.add(key)
        spec = CORPUS_COMPLEXITY[scene_id]
        cells.append(
            {
                "scene_id": scene_id,
                "width": width,
                "height": height,
                "samples": samples,
                "family": family,
                "tag": f"{scene_id}_{width}x{height}_{samples}spp",
                "pixel_samples": pixel_samples(width, height, samples),
                "triangle_count": int(spec["triangle_count"]),
                "instance_count": int(spec["instance_count"]),
                "texture_bytes": int(spec["texture_bytes"]),
                "complexity_class": complexity_class(
                    int(spec["triangle_count"]),
                    int(spec["instance_count"]),
                    int(spec["texture_bytes"]),
                ),
            }
        )

    for scene_id in SCENES:
        for spp in SAMPLES_AT_256:
            add(scene_id, 256, 256, spp, "samples_at_256")
        for res in RES_AT_64:
            add(scene_id, res, res, 64, "res_at_64")
        for w, spp in MID_CELLS:
            add(scene_id, w, w, spp, "mid")
    for scene_id, w, spp in ALWAYS_HEAVY:
        add(scene_id, w, w, spp, "heavy_anchor")
    if include_expensive:
        for scene_id in SCENES:
            if scene_id == "trivial":
                continue
            add(scene_id, 1024, 1024, 512, "expensive")
    cells.sort(key=lambda c: (c["scene_id"], c["pixel_samples"], c["width"], c["samples"]))
    return cells


def run_blender(
    *,
    blender: str,
    mode: str,
    device: str,
    scene_id: str,
    cwd: Path,
    out: Path | None = None,
    out_dir: Path | None = None,
    cells: list[dict[str, Any]] | None = None,
    width: int = 0,
    height: int = 0,
    samples: int = 0,
    repeats: int = 1,
    persistent: int = 0,
    dump_linear: int = 0,
    generated: Path,
    timeout_s: float,
) -> dict[str, Any]:
    entry = ROOT / ENTRY_REL
    args = [
        "/usr/bin/time",
        "-l",
        blender,
        "-b",
        "-noaudio",
        "--factory-startup",
        "--python-exit-code",
        "1",
        "--python",
        str(entry),
        "--",
        f"--mode={mode}",
        f"--device={device}",
        "--metal-rt=OFF",
        f"--scene={scene_id}",
        f"--repeats={repeats}",
        "--adaptive=0",
        f"--persistent={persistent}",
        f"--generated={generated}",
        f"--dump-linear={dump_linear}",
    ]
    if width:
        args.append(f"--width={width}")
    if height:
        args.append(f"--height={height}")
    if samples:
        args.append(f"--samples={samples}")
    if out is not None:
        args.append(f"--out={out}")
    cells_path = None
    if mode == "sweep":
        if out_dir is None or not cells:
            raise RuntimeError("sweep requires out_dir and cells")
        out_dir.mkdir(parents=True, exist_ok=True)
        payload = [
            {"tag": c["tag"], "width": c["width"], "height": c["height"], "samples": c["samples"]}
            for c in cells
        ]
        cells_path = out_dir / f"_cells_{device}.json"
        cells_path.write_text(json.dumps(payload), encoding="utf-8")
        args.append(f"--out-dir={out_dir}")
        args.append(f"--cells-json=@{cells_path}")
    env = os.environ.copy()
    env["MERC_RENDER_GENERATED"] = str(generated)
    t0 = time.perf_counter()
    proc = subprocess.run(
        args,
        cwd=str(cwd),
        env=env,
        capture_output=True,
        text=True,
        timeout=timeout_s,
    )
    wall = time.perf_counter() - t0
    stdout = proc.stdout or ""
    stderr = proc.stderr or ""
    text = stdout + "\n" + stderr
    markers = parse_markers(stdout)
    fallback_hit = bool(FALLBACK_RE.search(text))
    kernels = parse_kernel_progress(text)
    times = parse_blender_times(text)
    rss = parse_rss(stderr)
    if proc.returncode == 0 and not markers:
        raise RuntimeError(
            f"no MERC_CYCLES_METAL marker\nstdout:\n{stdout[-4000:]}\nstderr:\n{stderr[-4000:]}"
        )
    assertion = {}
    for m in markers:
        if m.get("assertion"):
            assertion = m["assertion"]
    if device == "GPU" and proc.returncode == 0:
        if assertion.get("backend") != "METAL":
            raise RuntimeError(f"GPU arm did not assert METAL: {assertion}")
        if assertion.get("compute_device_type") != "METAL":
            raise RuntimeError(f"GPU arm compute_device_type={assertion.get('compute_device_type')!r}")
        if assertion.get("cycles_device") != "GPU":
            raise RuntimeError(f"GPU arm cycles.device={assertion.get('cycles_device')!r}")
        if fallback_hit:
            raise RuntimeError("GPU arm stdout/stderr mentions CPU fallback")
    if device == "CPU" and proc.returncode == 0:
        if assertion.get("backend") != "CPU" or assertion.get("cycles_device") != "CPU":
            raise RuntimeError(f"CPU arm did not assert CPU: {assertion}")
    return {
        "exit_code": proc.returncode,
        "wall_s": wall,
        "blender_times_s": times,
        "peak_rss_bytes": rss,
        "markers": markers,
        "kernels": kernels,
        "fallback_mentioned": fallback_hit,
        "stdout_tail": stdout[-2500:],
        "stderr_tail": stderr[-2500:],
        "cmd": args[2:],
        "assertion": assertion,
    }


def self_test() -> int:
    predicate_self_test()
    errs = validate_records()
    ids = {r["id"] for r in SCENE_RECORDS}
    for sid in SCENES:
        if sid not in ids:
            errs.append(f"sweep scene {sid} missing from SCENE_RECORDS")
        if sid not in CORPUS_COMPLEXITY:
            errs.append(f"sweep scene {sid} missing from CORPUS_COMPLEXITY")
    grid = build_grid(include_expensive=False)
    if len(grid) < 40:
        errs.append(f"default grid too small: {len(grid)}")
    tags = [c["tag"] for c in grid]
    if len(tags) != len(set(tags)):
        errs.append("duplicate cell tags")
    # 256²/64 present for every scene; 1024²/512 present for trivial.
    for sid in SCENES:
        if not any(c["scene_id"] == sid and c["width"] == 256 and c["samples"] == 64 for c in grid):
            errs.append(f"{sid} missing 256/64")
    if not any(c["scene_id"] == "trivial" and c["width"] == 1024 and c["samples"] == 512 for c in grid):
        errs.append("trivial missing 1024/512 anchor")
    entry = (ROOT / ENTRY_REL).read_text(encoding="utf-8")
    if 'scene.render.engine = "BLENDER_EEVEE"' in entry or 'scene.render.engine = "EEVEE"' in entry:
        errs.append("blender_entry must never assign EEVEE")
    if "--mode=sweep" not in entry and '"sweep"' not in entry:
        errs.append("blender_entry must accept sweep mode")
    device_src = (ROOT / "src/render" / "metal" / "device.py").read_text(encoding="utf-8")
    if 'prefs.compute_device_type = "METAL"' not in device_src:
        errs.append("device.py must pin METAL")
    if "silent CPU fallback" not in device_src and "refusing silent" not in device_src:
        errs.append("device.py must refuse silent CPU fallback")
    # L1 helper: identical vs one-byte delta
    a = bytes([10, 20, 30, 40, 50, 60, 70, 80, 90, 100, 110, 120])
    same = pixel_l1_stats(a, a, 2, 2)
    if not same["pixel_exact"]:
        errs.append("identical buffers must be L1 exact")
    b = bytearray(a)
    b[3] = (b[3] + 7) & 255
    diff = pixel_l1_stats(a, bytes(b), 2, 2)
    if diff["pixel_exact"] or diff["pixels_differing"] != 1 or diff["max_abs_error"] != 7:
        errs.append(f"one-pixel delta stats wrong: {diff}")
    if L1_INTERCHANGEABLE:
        errs.append("predicate must not claim L1 interchangeability")
    if errs:
        print("SELFTEST FAIL")
        for e in errs:
            print(" -", e)
        return 1
    print("self-test ok")
    print(f"scenes={list(SCENES)}")
    print(f"default_grid={len(grid)}")
    return 0


def derive_floors(paired: list[dict[str, Any]], metric: str) -> dict[str, dict[str, Any]]:
    """Per-class lose/win floors from paired cells.

    lose = max pixel_samples whose ratio < 1.0
    win  = min pixel_samples whose ratio >= WIN_MARGIN
    Unknown band is everything strictly between.
    """
    by_cls: dict[str, list[dict[str, Any]]] = {}
    for cell in paired:
        r = cell.get(metric)
        if r is None:
            continue
        by_cls.setdefault(cell["complexity_class"], []).append(cell)
    out: dict[str, dict[str, Any]] = {}
    for cls, rows in by_cls.items():
        loses = [c["pixel_samples"] for c in rows if c[metric] < 1.0]
        wins = [c["pixel_samples"] for c in rows if c[metric] >= WIN_MARGIN]
        lose_at = max(loses) if loses else None
        win_at = min(wins) if wins else None
        if lose_at is not None and win_at is not None and win_at <= lose_at:
            # Non-monotone: keep the highest lose and the highest win
            # that is still above every lose? Safer: win_at = min win
            # strictly above lose_at; else None.
            above = [p for p in wins if p > lose_at]
            win_at = min(above) if above else None
        out[cls] = {
            "lose_at_or_below": lose_at,
            "win_at_or_above": win_at,
            "n": len(rows),
            "n_lose": len(loses),
            "n_win": len(wins),
            "metric": metric,
        }
    return out


def measure(args: argparse.Namespace) -> dict[str, Any]:
    blender = args.blender
    if not Path(blender).is_file():
        raise SystemExit(f"blender binary missing: {blender}")
    version = blender_version(blender)
    host = platform.node()
    work = Path(args.workdir) if args.workdir else ROOT / "src/render" / "corpus" / "output" / "placement"
    work.mkdir(parents=True, exist_ok=True)
    generated = work / "generated"
    generated.mkdir(parents=True, exist_ok=True)

    started = utc_now()
    t0 = time.perf_counter()
    surprises: list[str] = []
    dropped: list[dict[str, Any]] = []
    grid = build_grid(include_expensive=bool(args.include_expensive))

    print("== kernel cache before ==", flush=True)
    cache_before = find_kernel_cache(ROOT)
    print(json.dumps(cache_before, indent=2), flush=True)

    print("== probe GPU ==", flush=True)
    probe = run_blender(
        blender=blender,
        mode="probe",
        device="GPU",
        scene_id="trivial",
        cwd=ROOT,
        generated=generated,
        timeout_s=args.timeout,
    )
    if probe["exit_code"] != 0:
        raise SystemExit(f"Metal probe failed: {probe['stderr_tail']}")
    print(
        f"  backend={probe['assertion'].get('backend')} "
        f"devices={probe['assertion'].get('metal_device_names')} "
        f"wall={probe['wall_s']:.3f}s",
        flush=True,
    )

    print("== kernel compile / cross-process cache ==", flush=True)
    kernel_runs: list[dict[str, Any]] = []
    tiny_dir = work / "kernel"
    tiny_dir.mkdir(parents=True, exist_ok=True)
    for i in range(3):
        print(f"  tiny GPU 32²/4spp process {i} (cwd=ROOT)", flush=True)
        run = run_blender(
            blender=blender,
            mode="render",
            device="GPU",
            scene_id="trivial",
            cwd=ROOT,
            out=tiny_dir / f"tiny_root_{i:02d}.png",
            width=32,
            height=32,
            samples=4,
            generated=generated,
            timeout_s=max(args.timeout, 180),
        )
        kernel_runs.append(
            {
                "i": i,
                "cwd": "ROOT",
                "wall_s": run["wall_s"],
                "blender_time_s": run["blender_times_s"][-1] if run["blender_times_s"] else None,
                "kernels": run["kernels"],
                "exit_code": run["exit_code"],
            }
        )
        print(
            f"    wall={run['wall_s']:.3f}s blender_time={kernel_runs[-1]['blender_time_s']} "
            f"first_time={run['kernels'].get('first_time_warning')} last={run['kernels'].get('last')}",
            flush=True,
        )
        if run["exit_code"] != 0:
            raise SystemExit(f"tiny GPU render failed: {run['stderr_tail']}")

    other_cwd = Path(tempfile.mkdtemp(prefix="merc-kcache-"))
    print(f"  tiny GPU 32²/4spp process in other cwd {other_cwd}", flush=True)
    other_run = run_blender(
        blender=blender,
        mode="render",
        device="GPU",
        scene_id="trivial",
        cwd=other_cwd,
        out=other_cwd / "tiny_other.png",
        width=32,
        height=32,
        samples=4,
        generated=generated,
        timeout_s=max(args.timeout, 180),
    )
    other_cache = find_kernel_cache(other_cwd)
    other_cell = {
        "cwd": str(other_cwd),
        "wall_s": other_run["wall_s"],
        "blender_time_s": other_run["blender_times_s"][-1] if other_run["blender_times_s"] else None,
        "kernels": other_run["kernels"],
        "exit_code": other_run["exit_code"],
        "cache_after": other_cache,
    }
    print(
        f"    wall={other_run['wall_s']:.3f}s first_time={other_run['kernels'].get('first_time_warning')} "
        f"cwd_integrator={other_cache.get('cwd_integrator_present')}",
        flush=True,
    )
    if other_run["exit_code"] != 0:
        surprises.append("other-cwd tiny GPU render failed; cache-locality cell dropped")
        dropped.append({"cell": "kernel_other_cwd", "reason": other_run["stderr_tail"][-400:]})

    cache_after_kernel = find_kernel_cache(ROOT)
    first_wall = kernel_runs[0]["wall_s"]
    later = [r["wall_s"] for r in kernel_runs[1:]]
    later_mean = statistics.fmean(later) if later else None
    # first_time_warning prints on every Metal process on this Blender
    # and is NOT a compile-cold detector. Compile-cold is a large first
    # vs later gap (tens of seconds vs <1s).
    compile_cold_observed = bool(later_mean and first_wall > max(later_mean * 3, 8.0))
    cached_across_processes = (later_mean is not None and later_mean < 5.0)
    other_recompiled = False
    if other_run["exit_code"] == 0 and later_mean:
        other_recompiled = other_run["wall_s"] > max(later_mean * 3, 8.0)

    kernel_block = {
        "classification": "MEASURED",
        "probe_gpu_wall_s": probe["wall_s"],
        "probe_assertion": probe["assertion"],
        "tiny_root_processes": kernel_runs,
        "tiny_other_cwd": other_cell,
        "cache_before": cache_before,
        "cache_after": cache_after_kernel,
        "first_process_wall_s": first_wall,
        "later_process_wall_s_mean": later_mean,
        "first_over_later": ratio(first_wall, later_mean),
        "cached_across_processes_in_same_cwd": cached_across_processes,
        "cache_is_cwd_local": other_recompiled,
        "compile_cold_s": first_wall if compile_cold_observed else None,
        "warm_process_s": later_mean,
        "note": (
            "GPU equivalent of the ~0.40s CPU startup constant. "
            "compile_cold_s is first Metal use when this CWD had no integrator cache. "
            "warm_process_s is a subsequent process in the same CWD. "
            "cache_is_cwd_local=true means a new working directory recompiles "
            "(the cache is not a user-global Cycles store)."
        ),
    }
    # Honesty: if the first process was already warm (prior session cache),
    # we did not measure compile-cold in this run.
    if kernel_block["compile_cold_s"] is None:
        kernel_block["compile_cold_s_prior"] = GPU_KERNEL_COMPILE_COLD_S
        kernel_block["compile_cold_source"] = (
            "not observed this run (cache already populated); "
            "32.75s is the GPU-lane first-use on this host / Blender 4.2.1"
        )
        surprises.append(
            "this CWD's first tiny GPU process was not compile-cold; "
            "citing the GPU-lane 32.75s first-use rather than pretending we re-measured it"
        )

    print("== resident sweep ==", flush=True)
    resident_runs: dict[str, dict[str, Any]] = {}
    identities: dict[str, Any] = {}
    for scene_id in SCENES:
        scene_cells = [c for c in grid if c["scene_id"] == scene_id]
        for device in ("CPU", "GPU"):
            out_dir = work / "resident" / scene_id / device.lower()
            print(
                f"  {scene_id} {device} n_cells={len(scene_cells)} -> {out_dir}",
                flush=True,
            )
            timeout = args.timeout
            if any(c["pixel_samples"] >= 200_000_000 for c in scene_cells):
                timeout = max(timeout, 900)
            run = run_blender(
                blender=blender,
                mode="sweep",
                device=device,
                scene_id=scene_id,
                cwd=ROOT,
                out_dir=out_dir,
                cells=scene_cells,
                persistent=1,
                dump_linear=0,
                generated=generated,
                timeout_s=timeout,
            )
            if run["exit_code"] != 0:
                dropped.append(
                    {
                        "cell": f"resident_{scene_id}_{device}",
                        "reason": run["stderr_tail"][-600:],
                    }
                )
                surprises.append(f"resident {scene_id} {device} failed (exit {run['exit_code']})")
                print(f"    FAIL exit={run['exit_code']}", flush=True)
                continue
            cell_markers = [m for m in run["markers"] if m.get("mode") == "sweep_cell"]
            begin = next((m for m in run["markers"] if m.get("mode") == "sweep_begin"), None)
            if begin and begin.get("identity"):
                identities[scene_id] = begin["identity"]
            by_tag = {m["tag"]: m for m in cell_markers}
            resident_runs[f"{scene_id}:{device}"] = {
                "process_wall_s": run["wall_s"],
                "peak_rss_bytes": run["peak_rss_bytes"],
                "n_markers": len(cell_markers),
                "identity": (begin or {}).get("identity"),
                "assertion": run["assertion"],
                "kernels": run["kernels"],
                "cells": by_tag,
            }
            print(
                f"    process_wall={run['wall_s']:.2f}s cells={len(cell_markers)} "
                f"rss={run['peak_rss_bytes']}",
                flush=True,
            )

    print("== pair resident cells + L1 ==", flush=True)
    paired: list[dict[str, Any]] = []
    for cell in grid:
        cpu = (resident_runs.get(f"{cell['scene_id']}:CPU") or {}).get("cells", {}).get(cell["tag"])
        gpu = (resident_runs.get(f"{cell['scene_id']}:GPU") or {}).get("cells", {}).get(cell["tag"])
        row = dict(cell)
        row["classification"] = "MEASURED"
        row["cpu_resident_wall_s"] = cpu.get("wall_s") if cpu else None
        row["gpu_resident_wall_s"] = gpu.get("wall_s") if gpu else None
        row["resident_cpu_over_gpu"] = ratio(row["cpu_resident_wall_s"], row["gpu_resident_wall_s"])
        row["cpu_out"] = cpu.get("out") if cpu else None
        row["gpu_out"] = gpu.get("out") if gpu else None
        row["parity_l1"] = None
        if cpu and gpu and cpu.get("out_exists") and gpu.get("out_exists"):
            row["parity_l1"] = compare_pngs(cpu["out"], gpu["out"])
            if not row["parity_l1"].get("pixel_exact"):
                surprises.append(
                    "%s L1 FAIL CPU vs Metal — differing=%s/%s max_abs=%s"
                    % (
                        cell["tag"],
                        row["parity_l1"].get("pixels_differing"),
                        row["parity_l1"].get("pixels_compared"),
                        row["parity_l1"].get("max_abs_error"),
                    )
                )
        else:
            dropped.append({"cell": cell["tag"], "reason": "missing resident CPU or GPU output"})
        # Predicate as-of this bench (first-assignment, kernels warm).
        pred = gpu_placement_license(
            cell["width"],
            cell["height"],
            cell["samples"],
            triangle_count=cell["triangle_count"],
            instance_count=cell["instance_count"],
            texture_bytes=cell["texture_bytes"],
            kernels_warm=True,
        )
        row["predicate"] = {
            "licensed": pred["licensed"],
            "gpu_faster": pred["gpu_faster"],
            "band": pred["band"],
            "complexity_class": pred["complexity_class"],
        }
        paired.append(row)
        r = row["resident_cpu_over_gpu"]
        print(
            f"  {cell['tag']:40s} class={cell['complexity_class']:9s} "
            f"ps={cell['pixel_samples']:12d} "
            f"cpu={row['cpu_resident_wall_s']!s:>8} gpu={row['gpu_resident_wall_s']!s:>8} "
            f"x={r if r is None else round(r, 3)!s:>6} "
            f"L1={'EXACT' if (row['parity_l1'] or {}).get('pixel_exact') else 'FAIL'} "
            f"pred={pred['band']}",
            flush=True,
        )

    resident_floors = derive_floors(paired, "resident_cpu_over_gpu")

    print("== cold-wall band (crossover neighbourhood) ==", flush=True)
    # Remeasure cells near the resident crossover as one-shot cold processes.
    cold_candidates: list[dict[str, Any]] = []
    for row in paired:
        r = row.get("resident_cpu_over_gpu")
        if r is None:
            continue
        if 0.70 <= r <= 2.50 or row["family"] == "heavy_anchor" or (
            row["width"] == 256 and row["samples"] == 64
        ):
            cold_candidates.append(row)
    # Cap: keep the 256/64 per scene, the heavy anchor, and the closest
    # to 1.0x per class.
    picked: list[dict[str, Any]] = []
    seen_pick: set[str] = set()

    def pick(row: dict[str, Any]) -> None:
        if row["tag"] in seen_pick:
            return
        seen_pick.add(row["tag"])
        picked.append(row)

    for row in paired:
        if row["width"] == 256 and row["samples"] == 64:
            pick(row)
        if row["family"] == "heavy_anchor":
            pick(row)
    by_cls: dict[str, list[dict[str, Any]]] = {}
    for row in cold_candidates:
        by_cls.setdefault(row["complexity_class"], []).append(row)
    for cls, rows in by_cls.items():
        rows = sorted(rows, key=lambda c: abs((c.get("resident_cpu_over_gpu") or 1) - 1.0))
        for row in rows[:3]:
            pick(row)
    picked = picked[:14]
    print(f"  cold remasure n={len(picked)}", flush=True)

    cold_rows: list[dict[str, Any]] = []
    for row in picked:
        cold: dict[str, Any] = {
            "tag": row["tag"],
            "scene_id": row["scene_id"],
            "width": row["width"],
            "height": row["height"],
            "samples": row["samples"],
            "pixel_samples": row["pixel_samples"],
            "complexity_class": row["complexity_class"],
        }
        for device in ("CPU", "GPU"):
            out = work / "cold" / f"{row['tag']}_{device.lower()}.png"
            timeout = args.timeout
            if row["pixel_samples"] >= 200_000_000:
                timeout = max(timeout, 900)
            print(f"  cold {device} {row['tag']}", flush=True)
            run = run_blender(
                blender=blender,
                mode="render",
                device=device,
                scene_id=row["scene_id"],
                cwd=ROOT,
                out=out,
                width=row["width"],
                height=row["height"],
                samples=row["samples"],
                generated=generated,
                timeout_s=timeout,
            )
            cold[f"{device.lower()}_wall_s"] = run["wall_s"]
            cold[f"{device.lower()}_blender_time_s"] = (
                run["blender_times_s"][-1] if run["blender_times_s"] else None
            )
            cold[f"{device.lower()}_kernels"] = run["kernels"]
            cold[f"{device.lower()}_exit"] = run["exit_code"]
            if run["exit_code"] != 0:
                dropped.append({"cell": f"cold_{row['tag']}_{device}", "reason": run["stderr_tail"][-400:]})
        cold["cold_wall_cpu_over_gpu"] = ratio(cold.get("cpu_wall_s"), cold.get("gpu_wall_s"))
        cold["cold_render_cpu_over_gpu"] = ratio(
            cold.get("cpu_blender_time_s"), cold.get("gpu_blender_time_s")
        )
        cold_rows.append(cold)
        print(
            f"    wall cpu={cold.get('cpu_wall_s')} gpu={cold.get('gpu_wall_s')} "
            f"x={cold.get('cold_wall_cpu_over_gpu')}",
            flush=True,
        )

    # Attach cold numbers onto the paired rows.
    cold_by_tag = {c["tag"]: c for c in cold_rows}
    for row in paired:
        c = cold_by_tag.get(row["tag"])
        if not c:
            continue
        row["cpu_cold_wall_s"] = c.get("cpu_wall_s")
        row["gpu_cold_wall_s"] = c.get("gpu_wall_s")
        row["cpu_cold_render_s"] = c.get("cpu_blender_time_s")
        row["gpu_cold_render_s"] = c.get("gpu_blender_time_s")
        row["cold_wall_cpu_over_gpu"] = c.get("cold_wall_cpu_over_gpu")
        row["cold_render_cpu_over_gpu"] = c.get("cold_render_cpu_over_gpu")

    cold_floors = derive_floors(paired, "cold_wall_cpu_over_gpu")

    # Intra-device L1 on one light + one heavy cell (CPU should be exact;
    # Metal is not, per the GPU lane).
    print("== intra-device L1 (trivial 256/64, two cold processes) ==", flush=True)
    intra: dict[str, Any] = {}
    for device in ("CPU", "GPU"):
        paths = []
        for i in range(2):
            out = work / "intra" / f"trivial_256_64_{device.lower()}_{i}.png"
            run = run_blender(
                blender=blender,
                mode="render",
                device=device,
                scene_id="trivial",
                cwd=ROOT,
                out=out,
                width=256,
                height=256,
                samples=64,
                generated=generated,
                timeout_s=args.timeout,
            )
            if run["exit_code"] != 0 or not out.is_file():
                dropped.append({"cell": f"intra_{device}_{i}", "reason": "render failed"})
                paths = []
                break
            paths.append(str(out))
        if len(paths) == 2:
            intra[device] = compare_pngs(paths[0], paths[1])
            print(
                f"  {device} intra L1 exact={intra[device].get('pixel_exact')} "
                f"diff={intra[device].get('pixels_differing')}",
                flush=True,
            )

    # Parity verdict across the whole curve.
    l1_rows = [r["parity_l1"] for r in paired if r.get("parity_l1") and r["parity_l1"].get("comparable")]
    worst = None
    for stats in l1_rows:
        if worst is None:
            worst = stats
            continue
        if (stats.get("pixels_differing") or 0) > (worst.get("pixels_differing") or 0):
            worst = stats
        elif (stats.get("pixels_differing") or 0) == (worst.get("pixels_differing") or 0):
            if (stats.get("max_abs_error") or 0) > (worst.get("max_abs_error") or 0):
                worst = stats
    n_exact = sum(1 for s in l1_rows if s.get("pixel_exact"))
    n_fail = sum(1 for s in l1_rows if not s.get("pixel_exact"))
    worst_paired = None
    if worst:
        for r in paired:
            if r.get("parity_l1") is worst:
                worst_paired = r
                break
    parity_verdict = {
        "status": "L1_FAILS" if n_fail else ("L1_HOLDS" if n_exact else "UNMEASURED"),
        "l1_pixel_exact": n_fail == 0 and n_exact > 0,
        "interchangeable_under_l1": False if n_fail else (n_exact > 0),
        "products": "different" if n_fail else ("same" if n_exact else "unknown"),
        "n_cells_compared": len(l1_rows),
        "n_l1_exact": n_exact,
        "n_l1_fail": n_fail,
        "worst_scene": (worst_paired or {}).get("scene_id"),
        "worst_tag": (worst_paired or {}).get("tag"),
        "worst_pixels_differing": (worst or {}).get("pixels_differing"),
        "worst_pixels_compared": (worst or {}).get("pixels_compared"),
        "worst_max_abs_error": (worst or {}).get("max_abs_error"),
        "worst_mean_abs_error": (worst or {}).get("mean_abs_error"),
        "worst_differing_fraction": (worst or {}).get("differing_fraction"),
        "cpu_intra_l1_exact": (intra.get("CPU") or {}).get("pixel_exact"),
        "gpu_intra_l1_exact": (intra.get("GPU") or {}).get("pixel_exact"),
        "reason": (
            "CPU and Metal produce different decoded pixels on at least one "
            "cell. Merc must not silently mix them for one project. Placement "
            "must treat device as part of the quality contract."
            if n_fail
            else "All compared cells were L1 PIXEL_EXACT. Unexpected on this host; re-check."
        ),
        "consequence": (
            "The planner must pin device on the project quality contract. "
            "gpu_placement_license() refuses a GPU dispatch when "
            "project_device='CPU' and stays on GPU when project_device='GPU', "
            "even if that frame is light work where CPU would be faster."
        ),
    }

    # Predicate agreement: first-assignment licensed iff measured cold (or
    # resident, if no cold) GPU win with margin. Conservative predicate
    # may refuse a measured win (unknown band) — that is allowed. It must
    # NOT license a measured lose.
    pred_errors: list[str] = []
    pred_unknown_wins = 0
    pred_correct_lose = 0
    pred_correct_win = 0
    for row in paired:
        r = row.get("cold_wall_cpu_over_gpu")
        if r is None:
            r = row.get("resident_cpu_over_gpu")
        if r is None:
            continue
        licensed = row["predicate"]["licensed"]
        if r < 1.0 and licensed:
            pred_errors.append(
                f"{row['tag']}: predicate licensed GPU but measured ratio {r:.3f} < 1"
            )
        elif r < 1.0 and not licensed:
            pred_correct_lose += 1
        elif r >= WIN_MARGIN and licensed:
            pred_correct_win += 1
        elif r >= WIN_MARGIN and not licensed:
            pred_unknown_wins += 1  # conservative refuse of a measured win

    finished = utc_now()
    wall = time.perf_counter() - t0

    # Curve tables for the evidence headline.
    samples_curve = [r for r in paired if r["width"] == 256 and r["height"] == 256]
    res_curve = [r for r in paired if r["samples"] == 64]
    samples_curve.sort(key=lambda r: (r["scene_id"], r["samples"]))
    res_curve.sort(key=lambda r: (r["scene_id"], r["width"]))

    recommended = {
        "resident": resident_floors,
        "cold_wall": cold_floors,
        "note": (
            "Recommended floors for a later tightening of gpu_placement_license. "
            "The shipped predicate stays conservative (unknown band = refuse) "
            "until these floors are copied into ops/scripts/lib/device_placement.py "
            "and the self-test is updated. A conservative refuse of a measured "
            "win is allowed; a license of a measured lose is not."
        ),
    }

    headline_rows = []
    for row in paired:
        if (row["width"] == 256 and row["samples"] == 64) or row["family"] == "heavy_anchor":
            headline_rows.append(
                {
                    "scene": row["scene_id"],
                    "resolution": [row["width"], row["height"]],
                    "samples": row["samples"],
                    "complexity_class": row["complexity_class"],
                    "pixel_samples": row["pixel_samples"],
                    "resident_cpu_over_gpu": row.get("resident_cpu_over_gpu"),
                    "cold_wall_cpu_over_gpu": row.get("cold_wall_cpu_over_gpu"),
                    "cold_render_cpu_over_gpu": row.get("cold_render_cpu_over_gpu"),
                    "l1_pixels_differing": (row.get("parity_l1") or {}).get("pixels_differing"),
                    "l1_max_abs": (row.get("parity_l1") or {}).get("max_abs_error"),
                    "predicate_band": row["predicate"]["band"],
                    "predicate_licensed": row["predicate"]["licensed"],
                }
            )

    report = {
        "classification": "MEASURED",
        "kind": "cycles_device_placement",
        "lane": "speed-device-placement",
        "generated_at": started,
        "finished_at": finished,
        "wall_clock_seconds": wall,
        "source_commit": git_source_commit(ROOT),
        "host": host,
        "host_note": "Mac Studio M3 Ultra, 28-core CPU / 60-core GPU / 96GB",
        "num_cpu": os.cpu_count(),
        "platform": platform.platform(),
        "invocation": {
            "env_gate": f"{ENV_GATE}=1",
            "excluded_from_normal_gate": True,
            "exclusion_proof": (
                "TestRenderDevicePlacementBench skips unless "
                f"{ENV_GATE}=1; listed in ops/scripts/allowed-test-skips.txt; "
                "make test / make ci never set the env var"
            ),
            "command": (
                f"cd src/control && {ENV_GATE}=1 go test -count=1 -timeout 180m "
                "-run '^TestRenderDevicePlacementBench$' ."
            ),
            "blender_bin": blender,
            "blender_version": version,
            "script": ENTRY_REL,
            "harness": "ops/scripts/render/device_placement_bench.py",
            "engine": "CYCLES",
            "gpu_backend_required": "METAL",
            "adaptive_sampling": False,
            "denoising": False,
            "seed": 1,
            "color_management": "AgX / sRGB / look=None / exposure=0 / gamma=1",
            "eevee_invoked": False,
            "include_expensive": bool(args.include_expensive),
            "scenes": list(SCENES),
            "both_arms": {
                "engine": "CYCLES",
                "samples": "per-cell, identical on CPU and GPU",
                "seed": 1,
                "bounces": "max=12 diffuse=4 glossy=4 transmission=12 volume=0 transparent=8",
                "view_transform": "AgX",
                "adaptive_sampling": False,
                "denoising": False,
                "metal_rt": "OFF",
            },
        },
        "honesty": {
            "what_this_proves": (
                "On this host, with this Blender 4.2.1 binary, the resolution × "
                "samples × scene-complexity curve where Cycles Metal overtakes "
                "Cycles CPU, a checkable-before-dispatch predicate matching "
                "that curve, the L1 PIXEL_EXACT verdict between the two "
                "devices, and the GPU kernel-compile / cross-process cache "
                "cost. Both arms used identical samples, seed, bounces, AgX, "
                "adaptive-OFF, denoise-OFF."
            ),
            "what_this_does_not_prove": (
                "This is one Mac Studio. It is not a Merc workload, not priced, "
                "and does not authorise silently mixing CPU and GPU frames. "
                "It does not prove a 10x or 100x or 73,000x project win. "
                "73,366x is an Amdahl CEILING from the locality lane (the "
                "point where verification stops binding), not a multiplier "
                "this lane achieved. A sampling-knob change is not an "
                "architecture win. Resident ratios are not cold-wall ratios."
            ),
            "axis": "per_worker_speedup",
            "axis_note": (
                "Device placement raises per-worker speedup on heavy frames "
                "and avoids the 0.69-0.90x penalty on light ones. It does not "
                "change the serial fraction (verification / transfer)."
            ),
            "guards": [
                "Cycles only; EEVEE is never selected (background EEVEE aborts with exit 134)",
                "GPU arm asserts Metal; silent CPU fallback is a hard failure",
                "same samples, seed, bounces, colour management, adaptive, denoise on BOTH arms",
                "L1 PIXEL_EXACT is decoded RGB pixels, never the PNG container (eXIf/tEXt)",
                "a cell that fails L1 is reported and is not tuned away",
                "scene is generated in-process from built-in primitives; no downloaded assets",
                "this harness is the only writer of evidence/perf/cycles-device-placement.json",
                "unknown band of the curve is REFUSED, not interpolated into a license",
                "a predicate license of a measured GPU-lose is a harness failure",
                "no 10,000x / 73,000x claim; Amdahl ceilings are not multipliers",
            ],
        },
        "probe": {
            "wall_s": probe["wall_s"],
            "assertion": probe["assertion"],
        },
        "kernel_cache": kernel_block,
        "identities": identities,
        "corpus_complexity_spec": CORPUS_COMPLEXITY,
        "grid": {
            "n": len(grid),
            "samples_at_256": list(SAMPLES_AT_256),
            "res_at_64": list(RES_AT_64),
            "mid": [list(x) for x in MID_CELLS],
            "include_expensive": bool(args.include_expensive),
        },
        "resident_process": {
            sid_dev: {
                "process_wall_s": v["process_wall_s"],
                "peak_rss_bytes": v["peak_rss_bytes"],
                "n_cells": v["n_markers"],
                "identity": v.get("identity"),
            }
            for sid_dev, v in resident_runs.items()
        },
        "cells": paired,
        "cold_cells": cold_rows,
        "intra_device_l1": intra,
        "floors": recommended,
        "predicate": {
            "module": "ops/scripts/lib/device_placement.py",
            "function": "gpu_placement_license",
            "win_margin": WIN_MARGIN,
            "agreement": {
                "licensed_a_measured_lose": pred_errors,
                "correct_lose": pred_correct_lose,
                "correct_win": pred_correct_win,
                "conservative_refuse_of_measured_win": pred_unknown_wins,
            },
            "rule": gpu_placement_license(256, 256, 64)["rule"],
        },
        "parity_verdict": parity_verdict,
        "dropped_cells": dropped,
        "surprises": surprises,
        "headline": {
            "adaptive_sampling": False,
            "denoising": False,
            "seed": 1,
            "view_transform": "AgX",
            "backend_asserted": (probe["assertion"] or {}).get("metal_device_names"),
            "anchor_rows": headline_rows,
            "samples_at_256_curve": [
                {
                    "scene": r["scene_id"],
                    "samples": r["samples"],
                    "pixel_samples": r["pixel_samples"],
                    "resident_cpu_over_gpu": r.get("resident_cpu_over_gpu"),
                    "cold_wall_cpu_over_gpu": r.get("cold_wall_cpu_over_gpu"),
                    "l1_pixels_differing": (r.get("parity_l1") or {}).get("pixels_differing"),
                }
                for r in samples_curve
            ],
            "res_at_64_curve": [
                {
                    "scene": r["scene_id"],
                    "resolution": r["width"],
                    "pixel_samples": r["pixel_samples"],
                    "resident_cpu_over_gpu": r.get("resident_cpu_over_gpu"),
                    "cold_wall_cpu_over_gpu": r.get("cold_wall_cpu_over_gpu"),
                    "l1_pixels_differing": (r.get("parity_l1") or {}).get("pixels_differing"),
                }
                for r in res_curve
            ],
            "not_claimed": (
                "100x. 73,366x is an Amdahl ceiling from the locality lane, "
                "not a multiplier. This lane's contribution is: GPU only "
                "where the published curve says it wins, and never mixed "
                "with CPU inside one project."
            ),
        },
    }
    if pred_errors:
        report["classification"] = "MEASURED_WITH_PREDICATE_ERROR"
        surprises.append("predicate licensed a measured GPU-lose: " + "; ".join(pred_errors))
        report["surprises"] = surprises
    return report


def main() -> int:
    p = argparse.ArgumentParser(prog="device_placement_bench")
    p.add_argument("--self-test", action="store_true")
    p.add_argument("--write-evidence", action="store_true")
    p.add_argument("--print-json", action="store_true")
    p.add_argument("--include-expensive", action="store_true")
    p.add_argument("--blender", default=DEFAULT_BLENDER)
    p.add_argument("--workdir", default="")
    p.add_argument("--timeout", type=float, default=600.0)
    args = p.parse_args()
    if args.self_test:
        return self_test()
    if os.environ.get(ENV_GATE) != "1":
        sys.stderr.write(f"REFUSE: set {ENV_GATE}=1 to run the live placement sweep\n")
        return 2
    report = measure(args)
    text = json.dumps(report, indent=2, default=str)
    if args.write_evidence:
        dest = ROOT / EVIDENCE_REL
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_text(text + "\n", encoding="utf-8")
        print(f"wrote {dest}", flush=True)
    if args.print_json or not args.write_evidence:
        print(text)
    # Fail the process if the predicate licensed a lose — that is a
    # contract bug, not a noisy cell.
    if report.get("predicate", {}).get("agreement", {}).get("licensed_a_measured_lose"):
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
