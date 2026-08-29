#!/usr/bin/env bash
# Seal a BOUND embed-cell comparison receipt.
#
# Re-measures candle_metal vs llama_cpp_metal MiniLM embeddings on this host
# (merc-agent bench-embed) and writes the authority through the programme's
# bound-evidence writer so producer_identity is complete.
#
# Opt-in only — verification suites must not dirty tracked evidence:
#   MERC_EMBED_CELL_PERF=1 ./scripts/seal-embed-cell-receipt.sh
#
# Prerequisites:
#   - src/agent/target/release/merc-agent (or AGENT_BIN)
#   - llama-server listening with --embedding --pooling mean on the MiniLM F16 GGUF
#     (default http://127.0.0.1:8188; override with LLAMA_BASE_URL)
#   - pinned safetensors + GGUF artifacts available to the agent / llama-server
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

if [[ "${MERC_EMBED_CELL_PERF:-}" != "1" ]]; then
  echo "refusing: set MERC_EMBED_CELL_PERF=1 to seal embed-cell authority" >&2
  exit 2
fi

AGENT_BIN="${AGENT_BIN:-$ROOT/src/agent/target/release/merc-agent}"
LLAMA_BASE_URL="${LLAMA_BASE_URL:-http://127.0.0.1:8188}"
OUT_REL="evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r2.json"
RAW="$(mktemp -t embed-cell-raw.XXXXXX.json)"
PAYLOAD="$(mktemp -t embed-cell-payload.XXXXXX.json)"
trap 'rm -f "$RAW" "$PAYLOAD"' EXIT

if [[ ! -x "$AGENT_BIN" ]]; then
  echo "missing agent binary: $AGENT_BIN (build with: cd src/agent && cargo build --release)" >&2
  exit 2
fi

if ! curl -sf "${LLAMA_BASE_URL%/}/health" >/dev/null; then
  echo "llama-server not healthy at $LLAMA_BASE_URL (need --embedding --pooling mean on MiniLM F16 GGUF)" >&2
  exit 2
fi

COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
echo "measuring embed cell at source_commit=$COMMIT"

"$AGENT_BIN" bench-embed \
  --model all-minilm-l6-v2 \
  --source-commit "$COMMIT" \
  --llama-base-url "$LLAMA_BASE_URL" \
  --batch-sizes "${BATCH_SIZES:-1,8,32,128}" \
  --reps "${REPS:-5}" \
  --out "$RAW"

python3 - <<PY
import json, subprocess
from pathlib import Path
from datetime import datetime, timezone

raw = json.loads(Path("$RAW").read_text())
raw["measured_at"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
raw["merc_source_commit"] = subprocess.check_output(
    ["git", "-C", "$ROOT", "rev-parse", "HEAD"], text=True
).strip()
raw["supersedes"] = {
    "paths": [
        "evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json",
        "evidence/perf/runtime-benchmarks/llama-cpp-metal-embed-cosine-gate.json",
    ],
    "reasons": [
        "r1 (embed-cell comparison) was UNBOUND under the eight-field producer-identity bar",
        "cosine-gate receipt was UNBOUND; quality is re-measured in r2.quality",
        "r2 re-ran merc-agent bench-embed and sealed through ops/scripts/write-bound-evidence.py",
    ],
}
for k in ("producer_identity", "binding_status", "missing_identity_fields", "validity", "profile_revision"):
    raw.pop(k, None)
Path("$PAYLOAD").write_text(json.dumps(raw, indent=2) + "\n")
corpus = raw["corpus"]["sha256"]
print("corpus_digest", corpus)
print("quality", raw.get("quality"))
Path("/tmp/merc-embed-corpus-digest.txt").write_text(corpus)
PY

CORPUS_DIGEST="$(cat /tmp/merc-embed-corpus-digest.txt)"
# Candle cell's primary weight (safetensors); full artifact list lives in the body.
MODEL_DIGEST="53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db"

python3 "$ROOT/ops/scripts/write-bound-evidence.py" \
  --out "$OUT_REL" \
  --harness "src/agent/src/main.rs:run_bench_embed (merc-agent 0.1.0 bench-embed)" \
  --payload-file "$PAYLOAD" \
  --build-binary "$AGENT_BIN" \
  --model-digest "$MODEL_DIGEST" \
  --image-na "in-process candle + local llama-server process; no container image" \
  --corpus-digest "$CORPUS_DIGEST" \
  --exact-config "embedded engine_configuration + batch_sizes=${BATCH_SIZES:-1,8,32,128} reps=${REPS:-5} model=all-minilm-l6-v2" \
  --raw-samples "embedded measurements[] wall times and texts_per_sec; quality cosine over corpus"

echo "sealed $OUT_REL (BOUND). Update src/control/evidence-manifest.json + runtime-authority.json if re-pointing authority."
