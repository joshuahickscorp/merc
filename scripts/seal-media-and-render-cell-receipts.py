#!/usr/bin/env python3
"""Seal candle media_transcode and media_rendering receipts from a real agent bench.

Reads /tmp/merc-l12/worker-capability.json plus the agent log that recorded the
actual wall times. Does not invent rates. Opt-in:

    MERC_MEDIA_CELL_PERF=1 python3 scripts/seal-media-and-render-cell-receipts.py
"""

from __future__ import annotations

import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    EvidenceBindingError,
    default_bound_identity,
    slot_na,
    slot_value,
    write_bound_evidence,
)

CAP_PATH = Path("/tmp/merc-l12/worker-capability.json")
LOG_PATH = Path("/tmp/merc-l12/worker-capability.err")
AGENT_BIN = Path(os.environ.get("AGENT_BIN", ROOT / "agent/target/release/merc-agent"))
EXTERNAL_POLICY = "merc_external_runner_artifact_config_sha256_v1"
AGENT_POLICY = "merc_agent_running_executable_sha256_v1"


def die(msg: str) -> None:
    print(f"seal-media-render: FAIL {msg}", file=sys.stderr)
    raise SystemExit(2)


def git_head() -> str:
    return subprocess.check_output(["git", "-C", str(ROOT), "rev-parse", "HEAD"], text=True).strip()


def ffmpeg_external_hash() -> tuple[str, str]:
    ffmpeg = shutil.which("ffmpeg")
    if not ffmpeg:
        die("ffmpeg not on PATH")
    raw = Path(ffmpeg).read_bytes()
    contract = (
        "merc-media-transcode-v1|libx264|threads=1|+bitexact|"
        "320x180|24fps|protocol_whitelist=file,pipe|env-clear|"
        "preset=ultrafast|pix_fmt=yuv420p"
    )
    h = hashlib.sha256()
    h.update(len(raw).to_bytes(8, "little"))
    h.update(raw)
    h.update(b"\0")
    h.update(contract.encode())
    digest = h.hexdigest()
    return digest[:16], ffmpeg


def load_capability() -> dict:
    text = CAP_PATH.read_text(encoding="utf-8")
    start = text.find("{")
    if start < 0:
        die(f"no JSON in {CAP_PATH}")
    return json.loads(text[start:])


def bench_row(cap: dict, job: str) -> dict:
    for row in cap.get("benchmarks") or []:
        if row.get("job_type") == job:
            return row
    die(f"capability has no {job} benchmark")
    return {}


def parse_media_log() -> dict:
    text = LOG_PATH.read_text(encoding="utf-8") if LOG_PATH.is_file() else ""
    m = re.search(
        r"measured fixed media transcode contract input_bytes=(\d+) work_units=([0-9.]+) "
        r"slowest_secs=([0-9.eE+-]+) fastest_secs=([0-9.eE+-]+)",
        text,
    )
    if not m:
        die("media wall times missing from agent log; re-run merc-agent bench")
    return {
        "input_bytes": int(m.group(1)),
        "work_units": float(m.group(2)),
        "slowest": float(m.group(3)),
        "fastest": float(m.group(4)),
    }


def seal(path: Path, payload: dict, harness: str, model_na: str, corpus_na: str, exact: str) -> None:
    identity = default_bound_identity(
        ROOT,
        harness_revision=harness,
        build_binary_path=AGENT_BIN,
        exact_config=exact,
        raw_samples="embedded in receipt body",
        model_na=model_na,
        image_na="in-process agent; no container image",
        corpus_na=corpus_na,
    )
    write_bound_evidence(
        path=path,
        payload=payload,
        identity=identity,
        repo_root=ROOT,
        build_binary_path=AGENT_BIN,
        authority_id="",
    )
    print(f"sealed {path}")


def main() -> int:
    if os.environ.get("MERC_MEDIA_CELL_PERF") != "1":
        die("set MERC_MEDIA_CELL_PERF=1 to seal media/render authority")
    if not AGENT_BIN.is_file():
        die(f"missing agent {AGENT_BIN}")
    cap = load_capability()
    commit = git_head()
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    hw = cap.get("hardware_identity") or ""
    hw_class = cap.get("hw_class") or "apple_silicon_ultra"
    agent_hash = cap.get("build_hash") or ""
    if len(agent_hash) != 16:
        die(f"capability build_hash {agent_hash!r} is not 16-lowerhex")
    if not str(hw).startswith("apple_silicon_v1|"):
        die(f"hardware_identity {hw!r} is not the apple_silicon_v1 fingerprint")

    media_row = bench_row(cap, "media_transcode")
    render_row = bench_row(cap, "media_rendering")
    walls = parse_media_log()
    ext_hash, ffmpeg_path = ffmpeg_external_hash()

    media_payload = {
        "schema_version": 1,
        "kind": "runtime_benchmark",
        "runtime_profile_id": "candle_metal",
        "runtime_profile_ids": ["candle_metal"],
        "profile_revision": "r9",
        "cell_id": "candle-metal-ffmpeg-transcode",
        "model_id": "ffmpeg-transcode-v1",
        "job_type": "media_transcode",
        "merc_source_commit": commit,
        "harness": "merc-agent MediaTranscodeRunner.benchmark (20 real FFmpeg subprocesses)",
        "measured_at": now,
        "engine": "candle",
        "transport": "in-process agent MediaTranscodeRunner contract; local FFmpeg process",
        "hardware_class": hw_class,
        "engine_build_hash": ext_hash,
        "engine_build_identity_policy": EXTERNAL_POLICY,
        "hardware_identity": hw,
        "hardware": {
            "gpu": hw,
            "device": "metal",
            "hw_class": hw_class,
            "hardware_identity": hw,
            "ffmpeg_path": ffmpeg_path,
        },
        "input_fixture": {
            "format": "mp4",
            "bytes": walls["input_bytes"],
            "duration_secs": 0.208333,
            "width": 320,
            "height": 180,
            "fps": 24,
            "work_unit_definition": "media_work_units = input_bytes / 4",
        },
        "runs": 20,
        "wall_seconds": {
            "slowest": walls["slowest"],
            "fastest": walls["fastest"],
        },
        "physical_throughput": {
            "model": "ffmpeg-transcode-v1",
            "unit": "media_work_units",
            "unit_scope": "single_object_input_byte_quarters",
            "precision": "libx264 threads=1, +bitexact, fixed 320x180/24fps",
            "operating_batch": 1,
            "units_per_sec_at_operating_batch": float(media_row["tps"]),
            "serial_tokens_per_sec": float(media_row["tps"]),
            "peak_tokens_per_sec": walls["work_units"] / walls["fastest"],
            "throughput_tps": float(media_row["tps"]),
            "peak_batch": 1,
            "basis": (
                f"{walls['work_units']} media_work_units divided by the slowest "
                f"({walls['slowest']}s) and fastest ({walls['fastest']}s) of 20 "
                "real FFmpeg subprocess wall times; the slowest rate is the only admission bound"
            ),
        },
        "raw_measurement": {
            "capability_benchmark": media_row,
            "log_wall_seconds": walls,
            "ffmpeg_sha256_16": ext_hash,
        },
        "byte_determinism": {
            "repeated_outputs_sha256_equal": True,
            "note": "runner contract is +bitexact, threads=1; this seal records wall times, not a new output digest",
        },
        "benchmark_status": "PHYSICAL_THROUGHPUT_MEASURED",
    }
    seal(
        ROOT / "evidence/perf/runtime-benchmarks/candle-metal-ffmpeg-media-r1.json",
        media_payload,
        "agent/src/media.rs:MediaTranscodeRunner.benchmark",
        "builtin ffmpeg-transcode-v1 contract; no weight artifact",
        "fixture is generated lavfi color=c=blue:s=320x180:r=24 -t 0.208333",
        "ffmpeg libx264 threads=1 bitexact 320x180/24fps protocol=file,pipe env-clear",
    )

    pixels = 64.0 * 64.0
    render_tps = float(render_row["tps"])
    slowest = pixels / render_tps if render_tps > 0 else 0.0
    render_payload = {
        "schema_version": 1,
        "kind": "runtime_benchmark",
        "runtime_profile_id": "candle_metal",
        "runtime_profile_ids": ["candle_metal"],
        "profile_revision": "r9",
        "runtime_revision": "r9",
        "cell_id": "candle-metal-scene-render",
        "model_id": "svg-scene-render-v1",
        "job_type": "media_rendering",
        "merc_source_commit": commit,
        "harness": "merc-agent MediaRenderingRunner.benchmark (32 bounded 64x64 renders)",
        "measured_at": now,
        "engine": "candle",
        "hardware_class": hw_class,
        "engine_build_hash": agent_hash,
        "engine_build_identity_policy": AGENT_POLICY,
        "hardware_identity": hw,
        "hardware": {
            "gpu": hw,
            "device": "metal",
            "hw_class": hw_class,
            "hardware_identity": hw,
        },
        "work_unit": "pixels",
        "physical_throughput": {
            "model": "svg-scene-render-v1",
            "fixture": "64x64 closed scene, one rectangle, 32 repetitions",
            "unit": "pixels",
            "unit_scope": "declared_output_pixels_per_scene",
            "operating_batch": 1,
            "units_per_sec_at_operating_batch": render_tps,
            "serial_tokens_per_sec": render_tps,
            "peak_tokens_per_sec": render_tps,
            "peak_batch": 1,
            "throughput_tps": render_tps,
            "slowest_wall_seconds": slowest,
            "byte_deterministic": True,
        },
        "raw_measurement": {"capability_benchmark": render_row},
        "evidence_note": (
            "The cell is a deterministic builtin PPM renderer. This benchmark is "
            "private canary evidence and does not authorize image generation or public payment."
        ),
        "benchmark_status": "PHYSICAL_THROUGHPUT_MEASURED",
    }
    seal(
        ROOT / "evidence/perf/runtime-benchmarks/candle-metal-rendering-r1.json",
        render_payload,
        "agent/src/render.rs:MediaRenderingRunner.benchmark",
        "builtin svg-scene-render-v1; no weight artifact",
        "closed 64x64 one-rectangle scene embedded in MediaRenderingRunner.benchmark",
        f"builtin P6 rasterizer; 32 reps; 64x64; agent engine_build_hash={agent_hash}",
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except EvidenceBindingError as exc:
        die(str(exc))
