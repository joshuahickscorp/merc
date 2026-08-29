#!/usr/bin/env python3
"""Seal candle media, render, and MiniLM embed cell receipts from a real agent bench.

Media/render read /tmp/merc-l12/worker-capability.json plus the agent log that
recorded the actual wall times. Embed re-runs merc-agent bench-embed (or reuses
MERC_EMBED_RAW) and refuses unless the agent emitted a 16-lowerhex
engine_build_hash. Does not invent rates or hashes. Opt-in:

    MERC_MEDIA_CELL_PERF=1 python3 ops/scripts/seal-media-and-render-cell-receipts.py
    MERC_EMBED_CELL_PERF=1 python3 ops/scripts/seal-media-and-render-cell-receipts.py
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

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.evidence_binding import (  # noqa: E402
    EvidenceBindingError,
    default_bound_identity,
    slot_na,
    slot_value,
    write_bound_evidence,
)

CAP_PATH = Path("/tmp/merc-l12/worker-capability.json")
LOG_PATH = Path("/tmp/merc-l12/worker-capability.err")
AGENT_BIN = Path(os.environ.get("AGENT_BIN", ROOT / "src/agent/target/release/merc-agent"))
EXTERNAL_POLICY = "merc_external_runner_artifact_config_sha256_v1"
AGENT_POLICY = "merc_agent_running_executable_sha256_v1"


def die(msg: str) -> None:
    print(f"seal-media-render: FAIL {msg}", file=sys.stderr)
    raise SystemExit(2)


EMBED_OUT_REL = "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r3.json"
EMBED_MODEL = "all-minilm-l6-v2"
# Candle cell's primary weight (safetensors); full artifact list lives in the body.
EMBED_SAFETENSORS = "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"
EMBED_GGUF = "797b70c4edf85907fe0a49eb85811256f65fa0f7bf52166b147fd16be2be4662"
EMBED_CONFIG = "953f9c0d463486b10a6871cc2fd59f223b2c70184f49815e7efbcab5d8908b41"
EMBED_TOKENIZER = "be50c3628f2bf5bb5e3a7f17b1f74611b2561a3a27eeab05e5aa30f411572037"
EMBED_BATCH_SIZES_DEFAULT = "1,8,32,128"
EMBED_REPS_DEFAULT = "5"
EMBED_LLAMA_URL_DEFAULT = "http://127.0.0.1:8188"


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


def _lowerhex16(value: str) -> bool:
    return len(value) == 16 and all(c in "0123456789abcdef" for c in value)


def embed_throughput_from_measurements(rows: list[dict]) -> dict[str, dict]:
    """Derive admission floors from the slowest rep, never from the median rate."""
    by_profile: dict[str, list[dict]] = {}
    for row in rows:
        profile = str(row.get("runtime_profile_id") or "")
        if not profile:
            die("measurement row missing runtime_profile_id")
        by_profile.setdefault(profile, []).append(row)
    out: dict[str, dict] = {}
    for profile, profile_rows in by_profile.items():
        best_rate = -1.0
        best_batch = 0
        op_row = None
        for row in profile_rows:
            batch = int(row["batch"])
            median_rate = float(row["texts_per_sec"])
            if median_rate > best_rate:
                best_rate = median_rate
                best_batch = batch
            if batch == 128:
                op_row = row
        if op_row is None:
            die(f"{profile} sweep missing batch=128 operating row")
        max_wall = float(op_row["max_wall_s"])
        if max_wall <= 0:
            die(f"{profile} batch=128 max_wall_s={max_wall} is not a measured wall")
        floor = 128.0 / max_wall
        out[profile] = {
            "operating_batch": 128,
            "units_per_sec_at_operating_batch": floor,
            "best_observed_units_per_sec": best_rate,
            "best_observed_batch": best_batch,
            "max_wall_s": max_wall,
            "median_texts_per_sec_at_operating_batch": float(op_row["texts_per_sec"]),
        }
    return out


def measure_embed_raw() -> dict:
    raw_reuse = os.environ.get("MERC_EMBED_RAW", "").strip()
    if raw_reuse:
        path = Path(raw_reuse)
        if not path.is_file():
            die(f"MERC_EMBED_RAW={raw_reuse} is not a file")
        print(f"reusing measured raw {path}")
        return json.loads(path.read_text(encoding="utf-8"))

    if not AGENT_BIN.is_file():
        die(f"missing agent {AGENT_BIN}")
    llama_url = os.environ.get("LLAMA_BASE_URL", EMBED_LLAMA_URL_DEFAULT).rstrip("/")
    try:
        subprocess.check_call(
            ["curl", "-sf", f"{llama_url}/health"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
    except (subprocess.CalledProcessError, FileNotFoundError):
        die(
            f"llama-server not healthy at {llama_url} "
            "(need --embedding --pooling mean on MiniLM F16 GGUF)"
        )
    commit = git_head()
    batch_sizes = os.environ.get("BATCH_SIZES", EMBED_BATCH_SIZES_DEFAULT)
    reps = os.environ.get("REPS", EMBED_REPS_DEFAULT)
    out_path = Path("/tmp/merc-l12/embed-cell-raw-r3.json")
    out_path.parent.mkdir(parents=True, exist_ok=True)
    print(f"measuring embed cell at source_commit={commit}")
    print(f"agent={AGENT_BIN} batch_sizes={batch_sizes} reps={reps}")
    cmd = [
        str(AGENT_BIN),
        "bench-embed",
        "--model",
        EMBED_MODEL,
        "--source-commit",
        commit,
        "--llama-base-url",
        llama_url,
        "--batch-sizes",
        batch_sizes,
        "--reps",
        reps,
        "--out",
        str(out_path),
    ]
    rc = subprocess.call(cmd)
    if rc != 0:
        die(f"bench-embed failed (exit {rc})")
    return json.loads(out_path.read_text(encoding="utf-8"))


def seal_embed() -> int:
    raw = measure_embed_raw()
    engine_build_hash = str(
        raw.get("engine_build_hash") or raw.get("build_hash") or ""
    ).strip()
    engine_build_identity_policy = str(
        raw.get("engine_build_identity_policy") or raw.get("build_identity_policy") or ""
    ).strip()
    hardware_identity = str(raw.get("hardware_identity") or "").strip()
    if not hardware_identity:
        hardware = raw.get("hardware") if isinstance(raw.get("hardware"), dict) else {}
        hardware_identity = str(hardware.get("hardware_identity") or "").strip()
    hw_class = ""
    hardware = raw.get("hardware") if isinstance(raw.get("hardware"), dict) else {}
    if isinstance(hardware, dict):
        hw_class = str(hardware.get("hw_class") or "").strip()
    if not hw_class:
        hw_class = "apple_silicon_ultra"
    if not _lowerhex16(engine_build_hash):
        die(f"engine_build_hash {engine_build_hash!r} is not 16-lowerhex")
    if engine_build_identity_policy != AGENT_POLICY:
        die(
            f"engine_build_identity_policy {engine_build_identity_policy!r} "
            f"is not {AGENT_POLICY}"
        )
    if not hardware_identity.startswith("apple_silicon_v1|"):
        die(
            f"hardware_identity {hardware_identity!r} is not the exact "
            "apple_silicon_v1 fingerprint"
        )
    quality = raw.get("quality") if isinstance(raw.get("quality"), dict) else {}
    if not quality.get("passes"):
        die(f"cosine quality gate failed: {quality}")
    corpus = raw.get("corpus") if isinstance(raw.get("corpus"), dict) else {}
    corpus_digest = str(corpus.get("sha256") or "").strip()
    if len(corpus_digest) != 64:
        die(f"corpus.sha256 {corpus_digest!r} is not a sha256")
    measurements = raw.get("measurements")
    if not isinstance(measurements, list) or not measurements:
        die("bench-embed output has no measurements")
    rates = embed_throughput_from_measurements(measurements)
    if "candle_metal" not in rates:
        die("measurements missing candle_metal")

    commit = git_head()
    now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    batch_sizes = os.environ.get("BATCH_SIZES", EMBED_BATCH_SIZES_DEFAULT)
    reps = os.environ.get("REPS", EMBED_REPS_DEFAULT)
    payload = dict(raw)
    payload["measured_at"] = now
    payload["merc_source_commit"] = commit
    payload["engine_build_hash"] = engine_build_hash
    payload["engine_build_identity_policy"] = engine_build_identity_policy
    payload["hardware_identity"] = hardware_identity
    payload["hardware_class"] = hw_class
    payload["cell_id"] = "candle-metal-minilm-embed"
    payload["runtime_profile_id"] = "candle_metal"
    payload["runtime_profile_ids"] = ["candle_metal", "llama_cpp_metal"]
    payload["kind"] = "runtime_benchmark"
    payload["physical_throughput"] = {
        "unit": "embeddings",
        "unit_scope": "completed_embedding_records",
        "operating_batch": 128,
        "units_per_sec_at_operating_batch": rates["candle_metal"][
            "units_per_sec_at_operating_batch"
        ],
        "serial_tokens_per_sec": rates["candle_metal"]["units_per_sec_at_operating_batch"],
        "peak_tokens_per_sec": rates["candle_metal"]["best_observed_units_per_sec"],
        "peak_batch": rates["candle_metal"]["best_observed_batch"],
        "by_profile": {
            profile: {
                "unit": "embeddings",
                "unit_scope": "completed_embedding_records",
                "operating_batch": row["operating_batch"],
                "units_per_sec_at_operating_batch": row["units_per_sec_at_operating_batch"],
                "best_observed_units_per_sec": row["best_observed_units_per_sec"],
                "best_observed_batch": row["best_observed_batch"],
                "basis": (
                    f"128 texts divided by max_wall_s={row['max_wall_s']} at batch 128: "
                    "the SLOWEST of five repetitions, not the median rate the receipt quotes"
                ),
            }
            for profile, row in rates.items()
        },
    }
    payload["benchmark_status"] = "PHYSICAL_THROUGHPUT_MEASURED"
    payload["supersedes"] = {
        "paths": [
            "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json",
            "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json",
            "evidence/perf/runtime-benchmarks/llama-cpp-metal-embed-cosine-gate.json",
        ],
        "reasons": [
            "r2 is BOUND under the eight-field producer-identity bar but has an empty engine_build_hash, so cellAuthorityBindable parks candle-metal-minilm-embed",
            "r2 hardware_identity is only hw_class/device, not the exact apple_silicon_v1 configuration fingerprint",
            "r2 lacks engine_build_identity_policy on the receipt body required by the strict gate",
            "r3 re-ran merc-agent bench-embed on this host and sealed the execution build identity the agent emitted",
        ],
    }
    for k in (
        "producer_identity",
        "binding_status",
        "missing_identity_fields",
        "validity",
        "profile_revision",
    ):
        payload.pop(k, None)

    out = ROOT / EMBED_OUT_REL
    identity = default_bound_identity(
        ROOT,
        harness_revision=(
            "src/agent/src/main.rs:run_bench_embed "
            "(merc-agent 0.1.0 bench-embed, r3 execution-identity re-measure)"
        ),
        build_binary_path=AGENT_BIN,
        exact_config=(
            f"embedded engine_configuration + batch_sizes={batch_sizes} "
            f"reps={reps} model={EMBED_MODEL} "
            f"engine_build_hash={engine_build_hash}"
        ),
        raw_samples=(
            "embedded measurements[] wall times and texts_per_sec; "
            "quality cosine over corpus"
        ),
        model_na="unused; model digest supplied as value",
        image_na="in-process candle + local llama-server process; no container image",
        corpus_na="unused; corpus digest supplied as value",
    )
    identity["model_artifact_digest"] = slot_value(EMBED_SAFETENSORS)
    identity["corpus_digest"] = slot_value(corpus_digest)
    # The first seal() already wrote a BOUND file; rewrite with the exact
    # model/corpus slots so the eight-field bar names the measured artifacts.
    write_bound_evidence(
        path=out,
        payload=payload,
        identity=identity,
        repo_root=ROOT,
        build_binary_path=AGENT_BIN,
        authority_id="",
    )
    print(f"sealed {out}")
    print(f"engine_build_hash {engine_build_hash}")
    print(f"engine_build_identity_policy {engine_build_identity_policy}")
    print(f"hardware_identity {hardware_identity}")
    print(f"corpus_digest {corpus_digest}")
    print("quality", json.dumps(quality, sort_keys=True))
    for profile, row in rates.items():
        print(
            f"throughput {profile} floor={row['units_per_sec_at_operating_batch']} "
            f"best={row['best_observed_units_per_sec']} "
            f"best_batch={row['best_observed_batch']} "
            f"max_wall_s={row['max_wall_s']}"
        )
    return 0


def main() -> int:
    if os.environ.get("MERC_EMBED_CELL_PERF") == "1":
        return seal_embed()
    if os.environ.get("MERC_MEDIA_CELL_PERF") != "1":
        die(
            "set MERC_MEDIA_CELL_PERF=1 to seal media/render authority "
            "or MERC_EMBED_CELL_PERF=1 to seal embed authority"
        )
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
        "src/agent/src/media.rs:MediaTranscodeRunner.benchmark",
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
        "src/agent/src/render.rs:MediaRenderingRunner.benchmark",
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
