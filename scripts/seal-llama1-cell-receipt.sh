#!/usr/bin/env bash
# Seal a BOUND candle-metal-llama1-infer cell authority receipt.
#
# Re-measures llama-3.2-1b-instruct-q4 batch_infer on candle_metal (merc-agent
# bench-batch) and writes the authority through the programme's bound-evidence
# writer so producer_identity is complete — same pattern as
# scripts/seal-embed-cell-receipt.sh.
#
# Opt-in only — verification suites must not dirty tracked evidence:
#   MERC_LLAMA1_CELL_PERF=1 ./scripts/seal-llama1-cell-receipt.sh
#
# Prerequisites:
#   - agent/target/release/merc-agent (or AGENT_BIN) built with metal
#   - pinned Llama-3.2-1B-Instruct-Q4_K_M.gguf available to the agent cache
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ "${MERC_LLAMA1_CELL_PERF:-}" != "1" ]]; then
  echo "refusing: set MERC_LLAMA1_CELL_PERF=1 to seal llama1 batch_infer authority" >&2
  exit 2
fi

AGENT_BIN="${AGENT_BIN:-$ROOT/agent/target/release/merc-agent}"
OUT_REL="evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r4.json"
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

echo "measuring candle-metal-llama1-infer at source_commit=$COMMIT"
echo "agent=$AGENT_BIN batch_sizes=$BATCH_SIZES reps=$REPS"

# Capture stdout JSON only; stderr is progress.
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
  >"$RAW" 2> >(tee /tmp/merc-llama1-bench-batch.err >&2)
rc=$?
set -e
if [[ $rc -ne 0 ]]; then
  echo "bench-batch failed (exit $rc); see /tmp/merc-llama1-bench-batch.err" >&2
  exit "$rc"
fi

python3 - <<PY
import json, subprocess
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

serial = float(candle["serial_baseline_tok_s"])
peak = float(candle["peak_tok_s"])
sweep = candle.get("sweep") or []
by_batch = {}
peak_batch = 1
for row in sweep:
    b = int(row["batch"])
    tps = float(row["tokens_per_s"])
    by_batch[str(b)] = round(tps, 1)
    if abs(tps - peak) < 1e-9 or tps >= peak:
        peak_batch = b
        peak = tps

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

# profile_revision is r9 because candle_metal is at r9 in control/runtime-authority.json.
# This is a re-measure under the CURRENT profile, not an edit of the r3 receipt.
payload = {
    "schema_version": 1,
    "merc_source_commit": commit,
    "harness": "merc-agent bench-batch (candle-metal-llama1-infer re-measure)",
    "harness_settings": {
        "prompt": """$PROMPT""",
        "max_tokens": int("$MAX_TOKENS"),
        "reps": int("$REPS"),
        "mode": "identical",
        "batch_sizes": [int(x) for x in "$BATCH_SIZES".split(",") if x.strip()],
        "backends": ["candle"],
    },
    "measured_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "runtime_profile_id": "candle_metal",
    "profile_revision": "r9",
    "engine": "candle",
    "transport": "in-process",
    "model_id": "$MODEL",
    "model_note": "the exact pinned GGUF q4 artifact",
    "model_artifact_sha256": "$MODEL_DIGEST",
    "hardware": {
        "gpu": "Apple M3 Ultra",
        "device": device,
        "hw_class": "apple_silicon_ultra",
    },
    "physical_throughput": {
        "serial_tokens_per_sec": round(serial, 1),
        "by_batch": by_batch,
        "peak_tokens_per_sec": round(peak, 1),
        "peak_batch": peak_batch,
        "peak_multiple_of_serial": round(peak / serial, 2) if serial > 0 else 0.0,
        "decode_tokens_per_sec": round(peak, 1),
    },
    "byte_determinism": {
        "vs_serial": "IDENTICAL",
        "diverges_at_batch": diverged,
        "satisfies_byte_exact_verification": True,
    },
    "benchmark_status": "PHYSICAL_THROUGHPUT_MEASURED",
    "raw_measurement": {
        "kind": raw.get("kind"),
        "build_hash": raw.get("build_hash"),
        "device": device,
        "backends": {"candle": candle},
        "peak_by_backend": raw.get("peak_by_backend"),
    },
    "supersedes": {
        "paths": [
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r3.json",
            "evidence/perf/runtime-benchmarks/candle-metal-llama1-q4-r2.json",
        ],
        "reasons": [
            "r3 was UNBOUND under the eight-field producer-identity bar (missing build_digest, model_artifact_digest, image_digest, corpus_digest, exact_config, raw_samples)",
            "r3 cites profile_revision r3 while candle_metal authority is at r9, so cellAuthorityBindable refuses the mismatch",
            "r3 omits model_artifact_sha256s that the catalogue pins for llama-3.2-1b-instruct-q4",
            "r4 re-ran merc-agent bench-batch on this host under the current r9 profile and sealed through scripts/write-bound-evidence.py",
        ],
    },
}
Path("$PAYLOAD").write_text(json.dumps(payload, indent=2) + "\n")
print("device", device)
print("serial_tok_s", round(serial, 1))
print("peak_tok_s", round(peak, 1), "at batch", peak_batch)
print("by_batch", by_batch)
print("byte_determinism IDENTICAL")
print("model_digest", "$MODEL_DIGEST")
PY

python3 "$ROOT/scripts/write-bound-evidence.py" \
  --out "$OUT_REL" \
  --harness "agent/src/main.rs:run_bench_batch (merc-agent 0.1.0 bench-batch, candle only)" \
  --payload-file "$PAYLOAD" \
  --build-binary "$AGENT_BIN" \
  --model-digest "$MODEL_DIGEST" \
  --image-na "in-process candle metal; no container image" \
  --corpus-na "no external corpus; harness prompt is embedded in harness_settings" \
  --exact-config "candle in-process metal; batch_sizes=${BATCH_SIZES} reps=${REPS} max_tokens=${MAX_TOKENS} mode=identical model=${MODEL} require_deterministic" \
  --raw-samples "embedded raw_measurement.backends.candle.sweep[] tokens_per_s / min_tok_s / batched_equals_serial; physical_throughput + byte_determinism"

echo "sealed $OUT_REL (BOUND)."
echo "Next: update control/evidence-manifest.json + control/runtime-authority.json to re-point candle-metal-llama1-infer authority at $OUT_REL (profile_revision r9, binding_status BOUND, model_artifact_sha256s including the GGUF pin)."
