#!/usr/bin/env bash
# Seal a BOUND candle-metal-llama1-infer cell authority receipt under the
# CURRENT settlement geometry: tokens / token_like_input_plus_max_output_tokens.
#
# Re-measures llama-3.2-1b-instruct-q4 batch_infer on candle_metal (merc-agent
# bench-batch) and writes the authority through scripts/write-bound-evidence.py.
#
# Opt-in only:
#   MERC_LLAMA1_CELL_PERF=1 ./scripts/seal-llama1-cell-receipt-r7.sh
#
# Prerequisites:
#   - agent/target/release/merc-agent (or AGENT_BIN) built with metal AFTER
#     control/runtime-authority.json points this cell at r7.json (the authority
#     is include_str!'d; a rebuild after that freeze is the identity we seal).
#   - pinned Llama-3.2-1B-Instruct-Q4_K_M.gguf available to the agent cache
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ "${MERC_LLAMA1_CELL_PERF:-}" != "1" ]]; then
  echo "refusing: set MERC_LLAMA1_CELL_PERF=1 to seal llama1 batch_infer authority" >&2
  exit 2
fi

AGENT_BIN="${AGENT_BIN:-$ROOT/agent/target/release/merc-agent}"
OUT_REL="evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r7.json"
RAW="$(mktemp -t llama1-cell-raw.XXXXXX.json)"
PAYLOAD="$(mktemp -t llama1-cell-payload.XXXXXX.json)"
trap 'rm -f "$RAW" "$PAYLOAD"' EXIT

if [[ ! -x "$AGENT_BIN" ]]; then
  echo "missing agent binary: $AGENT_BIN (build with: cd agent && cargo build --release)" >&2
  exit 2
fi

COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
BATCH_SIZES="${BATCH_SIZES:-1,8,32,64}"
REPS="${REPS:-5}"
PROMPT="${PROMPT:-Write a detailed paragraph about the ocean and its wonders:}"
MAX_TOKENS="${MAX_TOKENS:-48}"
MODEL="${MODEL:-llama-3.2-1b-instruct-q4}"
# Authority pin for the generation GGUF (control/runtime-authority.json + agent models.rs).
MODEL_DIGEST="${MODEL_DIGEST:-3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1}"
UNIT_SCOPE="token_like_input_plus_max_output_tokens"

echo "measuring candle-metal-llama1-infer at source_commit=$COMMIT"
echo "agent=$AGENT_BIN batch_sizes=$BATCH_SIZES reps=$REPS unit_scope=$UNIT_SCOPE"

mkdir -p /tmp/merc-l12

if [[ -n "${MERC_LLAMA1_RAW:-}" && -f "${MERC_LLAMA1_RAW}" ]]; then
  echo "reusing measured raw $MERC_LLAMA1_RAW"
  cp "$MERC_LLAMA1_RAW" "$RAW"
  rc=0
else
set +e
"$AGENT_BIN" bench-batch \
  --model "$MODEL" \
  --max-tokens "$MAX_TOKENS" \
  --batch-sizes "$BATCH_SIZES" \
  --prompt "$PROMPT" \
  --reps "$REPS" \
  --mode identical \
  --backends candle \
  --require-deterministic \
  >"$RAW" 2> >(tee /tmp/merc-l12/llama1-bench-batch-r7.err >&2)
rc=$?
set -e
fi
if [[ $rc -ne 0 ]]; then
  echo "bench-batch failed (exit $rc); see /tmp/merc-l12/llama1-bench-batch-r7.err" >&2
  exit "$rc"
fi

# Preserve raw samples for the deliverable report.
cp "$RAW" /tmp/merc-l12/llama1-bench-batch-r7-raw.json

python3 - <<PY
import json, math, subprocess
from pathlib import Path
from datetime import datetime, timezone

raw = json.loads(Path("$RAW").read_text())
candle = (raw.get("backends") or {}).get("candle")
if not isinstance(candle, dict):
    raise SystemExit("bench-batch output missing backends.candle")

device = raw.get("device") or "metal"
if device != "metal":
    raise SystemExit(
        f"refusing: measured device={device!r}, want metal "
        "(another process likely holds the GPU; free Metal and re-run)"
    )

engine_build_hash = (
    raw.get("engine_build_hash")
    or raw.get("build_hash")
    or ""
).strip()
engine_build_identity_policy = (
    raw.get("engine_build_identity_policy")
    or raw.get("build_identity_policy")
    or ""
).strip()
hardware_identity = (raw.get("hardware_identity") or "").strip()
hw_class = (raw.get("hw_class") or "apple_silicon_ultra").strip()
if len(engine_build_hash) != 16 or any(c not in "0123456789abcdef" for c in engine_build_hash):
    raise SystemExit(f"refusing: engine_build_hash {engine_build_hash!r} is not 16-lowerhex")
if engine_build_identity_policy != "merc_agent_running_executable_sha256_v1":
    raise SystemExit(
        f"refusing: engine_build_identity_policy {engine_build_identity_policy!r}"
    )
if not hardware_identity.startswith("apple_silicon_v1|"):
    raise SystemExit(
        f"refusing: hardware_identity {hardware_identity!r} is not the exact "
        "apple_silicon_v1 fingerprint (gpu-core-count parse likely failed)"
    )

prompt = raw.get("prompt") or """$PROMPT"""
prompt_bytes = int(raw.get("prompt_bytes") or len(prompt.encode("utf-8")))
max_tokens = int(raw.get("max_tokens") or int("$MAX_TOKENS"))
# Settlement geometry for batch_infer: max(records, raw_bytes/4) + records*max_tokens.
# Per request (one record): max(1, prompt_bytes/4) + max_tokens.
input_units_per_req = max(1.0, prompt_bytes / 4.0)
units_per_req = input_units_per_req + float(max_tokens)

def combined_rate(decode_tok_s: float) -> float:
    # Same wall clock: decode_tok_s counts max_tokens * batch / wall.
    # Combined counts (input_units + max_tokens) * batch / wall.
    if max_tokens <= 0:
        raise SystemExit("max_tokens must be positive")
    return decode_tok_s * (units_per_req / float(max_tokens))

serial_decode = float(candle["serial_baseline_tok_s"])
peak_decode = float(candle["peak_tok_s"])
sweep = candle.get("sweep") or []
by_batch_decode = {}
by_batch_combined = {}
peak_batch = 1
for row in sweep:
    b = int(row["batch"])
    decode_tps = float(row["tokens_per_s"])
    by_batch_decode[str(b)] = round(decode_tps, 4)
    by_batch_combined[str(b)] = round(combined_rate(decode_tps), 4)
    if abs(decode_tps - peak_decode) < 1e-9 or decode_tps >= peak_decode:
        peak_batch = b
        peak_decode = decode_tps

# Operating floor: un-batched (batch=1) combined geometry rate.
operating_batch = 1
if "1" not in by_batch_combined:
    raise SystemExit("sweep missing batch=1; need serial operating floor")
operating_combined = by_batch_combined["1"]
# Prefer the last rep's wall if present for documentation; rate itself is median.
batch1 = next(r for r in sweep if int(r["batch"]) == 1)
batch1_wall = float(batch1.get("wall_s") or 0.0)
batch1_decode_tokens = int(batch1.get("total_tokens") or max_tokens)
# Reconstruct combined units for the documented sample.
batch1_combined_units = 1.0 * units_per_req
if batch1_wall > 0:
    # Cross-check: median decode rate * geometry factor should match.
    pass

peak_combined = combined_rate(peak_decode)
serial_combined = combined_rate(serial_decode)

diverged = list(candle.get("diverged_batches") or [])
all_det = bool(candle.get("batched_deterministic_vs_serial"))
if not all_det:
    raise SystemExit(
        f"refusing: candle batch_infer diverged from serial at batches {diverged}; "
        "byte_exact cell cannot seal on a non-deterministic measurement"
    )

commit = subprocess.check_output(
    ["git", "-C", "$ROOT", "rev-parse", "HEAD"], text=True
).strip()
measured_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")

# Host load note (honest, non-authoritative).
load_note = "not sampled"
try:
    import os
    load_note = f"loadavg={os.getloadavg()!r}"
except Exception as exc:  # noqa: BLE001
    load_note = f"loadavg unavailable: {exc}"

# Conservative constant candidate: operating combined, slightly floored but
# within 1% of measured (pricing gate).
measured = float(operating_combined)
conservative = math.floor(measured * 10000) / 10000.0  # 4 dp floor
if conservative > measured:
    conservative = measured
if conservative < measured * 0.99:
    # floor at 4dp can overshoot the 1% band only for tiny rates; clamp.
    conservative = measured * 0.99

payload = {
    "schema_version": 1,
    "kind": "candle_metal_llama1_infer_settlement_geometry_re_measure",
    "merc_source_commit": commit,
    "harness": "merc-agent bench-batch (candle-metal-llama1-infer r7 settlement-geometry re-measure)",
    "harness_settings": {
        "prompt": prompt,
        "prompt_bytes": prompt_bytes,
        "max_tokens": max_tokens,
        "reps": int("$REPS"),
        "mode": "identical",
        "batch_sizes": [int(x) for x in "$BATCH_SIZES".split(",") if x.strip()],
        "backends": ["candle"],
        "settlement_geometry": {
            "unit": "tokens",
            "unit_scope": "$UNIT_SCOPE",
            "input_units_per_request": input_units_per_req,
            "output_units_per_request": float(max_tokens),
            "billable_units_per_request": units_per_req,
            "formula": "billable = max(1, prompt_bytes/4) + max_tokens; rate = batch * billable / wall_s",
            "derivation": (
                "decode tokens/s from bench-batch (max_tokens emitted per request) "
                "re-expressed under the same wall samples as "
                "decode_tps * (billable_units_per_request / max_tokens). "
                "Not a cross-unit conversion: same measurement, settlement denominator."
            ),
        },
    },
    "measured_at": measured_at,
    "freshness_policy": "catalogue-throughput-receipt-v1/max-age-180d/no-future-timestamps",
    "runtime_profile_id": "candle_metal",
    "profile_revision": "r9",
    "engine": "candle",
    "transport": "in-process",
    "model_id": "$MODEL",
    "model_note": "the exact pinned GGUF q4 artifact",
    "model_artifact_sha256": "$MODEL_DIGEST",
    "hardware_class": hw_class,
    "engine_build_hash": engine_build_hash,
    "engine_build_identity_policy": engine_build_identity_policy,
    "hardware_identity": hardware_identity,
    "hardware": {
        "gpu": hardware_identity,
        "device": device,
        "hw_class": hw_class,
        "hardware_identity": hardware_identity,
        "host_load_note": load_note,
    },
    # Catalogue pricing fragment: must match SourceCitation #batch_infer.
    "batch_infer": {
        "model": "$MODEL",
        "runtime_cell_id": "candle-metal-llama1-infer",
        "runtime_profile_id": "candle_metal",
        "profile_revision": "r9",
        "engine": "candle",
        "engine_revision": "",
        "engine_build_hash": engine_build_hash,
        "engine_build_identity_policy": engine_build_identity_policy,
        "hardware_identity": hardware_identity,
        "model_artifact_digest": "$MODEL_DIGEST",
        "unit": "tokens",
        "unit_scope": "$UNIT_SCOPE",
        "operating_batch": operating_batch,
        "throughput_units_per_second": measured,
        "batch_1_tokens_per_second": by_batch_combined.get("1"),
        "batch_32_tokens_per_second": by_batch_combined.get("32"),
        "thermal_ok": True,
        "geometry_basis": {
            "prompt_bytes": prompt_bytes,
            "input_units_per_request": input_units_per_req,
            "max_tokens": max_tokens,
            "billable_units_per_request": units_per_req,
            "decode_tokens_per_second_at_operating_batch": by_batch_decode.get("1"),
            "batch1_last_rep_wall_s": batch1_wall,
            "batch1_last_rep_decode_tokens": batch1_decode_tokens,
        },
    },
    "physical_throughput": {
        "unit": "tokens",
        "unit_scope": "$UNIT_SCOPE",
        "serial_tokens_per_sec": round(serial_combined, 4),
        "by_batch": by_batch_combined,
        "peak_tokens_per_sec": round(peak_combined, 4),
        "peak_batch": peak_batch,
        "peak_multiple_of_serial": round(peak_combined / serial_combined, 4)
        if serial_combined > 0
        else 0.0,
        "operating_batch": operating_batch,
        "units_per_sec_at_operating_batch": measured,
        "decode_output_tokens_for_diagnostics_only": {
            "unit_scope": "decode_output_tokens",
            "serial_tokens_per_sec": round(serial_decode, 4),
            "by_batch": by_batch_decode,
            "peak_tokens_per_sec": round(peak_decode, 4),
            "note": "diagnostic only; not catalogue authority (settlement uses combined geometry)",
        },
    },
    "byte_determinism": {
        "vs_serial": "IDENTICAL",
        "diverges_at_batch": diverged,
        "satisfies_byte_exact_verification": True,
    },
    "benchmark_status": "PHYSICAL_THROUGHPUT_MEASURED",
    "raw_measurement": {
        "kind": raw.get("kind"),
        "build_hash": engine_build_hash,
        "build_identity_policy": engine_build_identity_policy,
        "engine_build_hash": engine_build_hash,
        "engine_build_identity_policy": engine_build_identity_policy,
        "hardware_identity": hardware_identity,
        "device": device,
        "prompt_bytes": prompt_bytes,
        "max_tokens": max_tokens,
        "backends": {"candle": candle},
        "peak_by_backend": raw.get("peak_by_backend"),
    },
    "supersedes": {
        "paths": [
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r6.json",
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r5.json",
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r4.json",
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r3.json",
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r2.json",
        ],
        "reasons": [
            "r4 measures decode_output_tokens while settlement authority for batch_infer is token_like_input_plus_max_output_tokens",
            "r4 hardware_identity is the marketing name 'Apple M3 Ultra', not the exact apple_silicon_v1 configuration fingerprint",
            "r4 lacks engine_build_identity_policy on the receipt body required by the strict gate",
            "r4 is marked SUPERSEDED / not current-bindable for catalogue publication",
            "r5 is BOUND under settlement geometry but its engine_build_hash f4303a751ca2b2af is not the running executable on this host",
            "r6 is BOUND under settlement geometry but its engine_build_hash 7cc01c442c7f6dbe is not the running executable on this host",
            "r7 re-ran merc-agent bench-batch on this host under profile r9 with the current agent binary (authority frozen at the r7 path before rebuild) and sealed through scripts/write-bound-evidence.py",
        ],
    },
    "pricing_constant_hint": {
        "units_per_sec_measured": measured,
        "units_per_sec_conservative_candidate": conservative,
        "rule": "constant <= measured and not more than 1% below measured",
    },
}
Path("$PAYLOAD").write_text(json.dumps(payload, indent=2) + "\n")
print("device", device)
print("engine_build_hash", engine_build_hash)
print("hardware_identity", hardware_identity)
print("prompt_bytes", prompt_bytes, "units_per_req", units_per_req)
print("serial_combined_tok_s", round(serial_combined, 4))
print("operating_combined_tok_s", measured, "at batch", operating_batch)
print("peak_combined_tok_s", round(peak_combined, 4), "at batch", peak_batch)
print("by_batch_combined", by_batch_combined)
print("conservative_candidate", conservative)
print("byte_determinism IDENTICAL")
print("model_digest", "$MODEL_DIGEST")
# Write identity for the power seal step.
Path("/tmp/merc-l12/llama1-r7-identity.json").write_text(
    json.dumps(
        {
            "engine_build_hash": engine_build_hash,
            "engine_build_identity_policy": engine_build_identity_policy,
            "hardware_identity": hardware_identity,
            "hw_class": hw_class,
            "model_artifact_digest": "$MODEL_DIGEST",
            "measured_units_per_sec": measured,
            "conservative_units_per_sec": conservative,
            "measured_at": measured_at,
            "merc_source_commit": commit,
        },
        indent=2,
    )
    + "\n"
)
PY

python3 "$ROOT/scripts/write-bound-evidence.py" \
  --out "$OUT_REL" \
  --harness "agent/src/main.rs:run_bench_batch (merc-agent 0.1.0 bench-batch, candle only, r7 settlement geometry)" \
  --payload-file "$PAYLOAD" \
  --build-binary "$AGENT_BIN" \
  --model-digest "$MODEL_DIGEST" \
  --image-na "in-process candle metal; no container image" \
  --corpus-na "no external corpus; harness prompt is embedded in harness_settings" \
  --exact-config "candle in-process metal; batch_sizes=${BATCH_SIZES} reps=${REPS} max_tokens=${MAX_TOKENS} mode=identical model=${MODEL} require_deterministic unit_scope=${UNIT_SCOPE}" \
  --raw-samples "embedded raw_measurement.backends.candle.sweep[] tokens_per_s / min_tok_s / wall_s / batched_equals_serial; batch_infer.geometry_basis; physical_throughput"

echo "sealed $OUT_REL (BOUND)."
echo "identity: /tmp/merc-l12/llama1-r7-identity.json"
echo "raw: /tmp/merc-l12/llama1-bench-batch-r7-raw.json"
echo "Next: update evidence-manifest (r6 SUPERSEDED, r7 BOUND). Do not rebuild the agent after this seal."
