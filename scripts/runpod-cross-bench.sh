#!/usr/bin/env bash
# Provision a RunPod vLLM pod, benchmark it through the SAME merc-agent harness
# used for the Metal profiles, and guarantee teardown.
#
# runpod-vllm.sh already tears down on exit, but only while it is the running
# process — and it exits as soon as the endpoint is ready. Benchmarking
# therefore needs `--keep`, which moves the teardown obligation to the caller.
# This script IS that caller, and it traps EXIT before the pod exists so a
# failed benchmark still kills it. A forgotten pod is the expensive failure
# here, not a bad measurement.
set -uo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
OUT="${1:-$ROOT/evidence/perf/runtime-benchmarks/vllm-cuda-raw.txt}"

POD_ID=""
cleanup() {
  local code=$?
  if [ -n "$POD_ID" ]; then
    echo "== tearing down pod $POD_ID (exit $code) =="
    bash "$ROOT/scripts/runpod-vllm.sh" down "$POD_ID" || {
      echo "!! TEARDOWN FAILED for $POD_ID — run: bash scripts/runpod-vllm.sh down-all" >&2
    }
  fi
  exit $code
}
trap cleanup EXIT INT TERM

echo "== provisioning =="
# A stale .merc-runpod.env from an earlier session will happily point at a dead
# pod. The first version of this script did exactly that: provisioning failed
# for lack of capacity, it sourced the old file, and benchmarked a 404 while
# reporting success. Remove it first and require `up` to succeed.
rm -f "$ROOT/.merc-runpod.env"
set -o pipefail
if ! bash "$ROOT/scripts/runpod-vllm.sh" up --keep 2>&1 | tee /tmp/runpod-up.log; then
  echo "provisioning failed; nothing is billing" >&2
  exit 1
fi
[ -f "$ROOT/.merc-runpod.env" ] || { echo "no .merc-runpod.env written" >&2; exit 1; }
# shellcheck disable=SC1091
set -a; . "$ROOT/.merc-runpod.env"; set +a
POD_ID="${MERC_RUNPOD_POD_ID:-}"
[ -n "$POD_ID" ] || { echo "no pod id" >&2; exit 1; }
echo "== pod $POD_ID ready at $MERC_GPU_ENDPOINT =="

# The vLLM adapter serves the alias the runtime profile pins.
MODEL="$(curl -sS -H "Authorization: Bearer $MERC_GPU_API_KEY" \
          "$MERC_GPU_ENDPOINT/models" \
        | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d["data"][0]["id"])' 2>/dev/null)"
echo "== serving model: $MODEL =="

echo "== bench-batch through the same harness as the Metal profiles =="
"$ROOT/agent/target/release/merc-agent" bench-batch \
  --backends openai_http \
  --batch-sizes 1,8,32,64 \
  --max-tokens 48 --reps 3 \
  --openai-base-url "$MERC_GPU_ENDPOINT" \
  --openai-model "$MODEL" \
  --openai-api-key "$MERC_GPU_API_KEY" 2>&1 | tee "$OUT"

echo "== done; teardown follows =="
