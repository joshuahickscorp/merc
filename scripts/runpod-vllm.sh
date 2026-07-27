#!/usr/bin/env bash
# Provision a RunPod A100 running pinned vLLM, print its endpoint, and make sure
# it dies.
#
# This spends real money from a prepaid balance. The dangerous failure is not a
# bad deploy, it is a pod nobody remembers to stop: at $1.19/hr a forgotten A100
# quietly eats the balance. So teardown is wired to EXIT before the pod is ever
# created, and `--keep` is the only way to leave one running.
#
#   bash scripts/runpod-vllm.sh up            # provision, print endpoint, tear down on exit
#   bash scripts/runpod-vllm.sh up --keep     # leave it running (prints the stop command)
#   bash scripts/runpod-vllm.sh list          # what is running right now
#   bash scripts/runpod-vllm.sh down <pod-id> # stop one
#   bash scripts/runpod-vllm.sh down-all      # stop everything (the panic button)

set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

# shellcheck disable=SC1091
[ -f "$ROOT/.merc-credentials.env" ] && { set -a; . "$ROOT/.merc-credentials.env"; set +a; }
: "${RUNPOD_API_KEY:?RUNPOD_API_KEY is required (run scripts/merc-credentials.sh)}"

GPU_TYPE="${MERC_RUNPOD_GPU:-NVIDIA A100 80GB PCIe}"
# Pinned, not :latest. A floating tag means the runtime that served a receipt
# cannot be identified later, which is the whole point of a pinned profile.
IMAGE="${MERC_VLLM_IMAGE:-vllm/vllm-openai:v0.26.0-cu129-ubuntu2404}"
MODEL="${MERC_VLLM_MODEL:-Qwen/Qwen2.5-1.5B-Instruct}"
VLLM_KEY="${MERC_VLLM_API_KEY:-merc-canary-$RANDOM$RANDOM}"
POD_NAME="merc-canary-vllm"

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

gql() {
  printf 'header = "Authorization: Bearer %s"\n' "$RUNPOD_API_KEY" \
    | curl -sS --config - -H 'content-type: application/json' --max-time 60 \
        -X POST https://api.runpod.io/graphql -d "$1"
}

json() { python3 -c "import json,sys;print(json.dumps(json.load(sys.stdin)))"; }

terminate() {
  local id="$1"
  [ -z "$id" ] && return 0
  gql "$(printf '{"query":"mutation { podTerminate(input:{podId:\\"%s\\"}) }"}' "$id")" >/dev/null 2>&1 || true
  say "  terminated $id"
}

list_pods() {
  gql '{"query":"query { myself { clientBalance pods { id name desiredStatus costPerHr } } }"}' \
  | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(f\"  balance \${m.get('clientBalance'):.2f}\" if m.get('clientBalance') is not None else '  balance ?')
pods=m.get('pods') or []
if not pods: print('  no pods running')
for p in pods: print(f\"  {p['id']}  {p['name']}  {p['desiredStatus']}  \${p.get('costPerHr')}/hr\")
"
}

case "${1:-up}" in
list) list_pods; exit 0 ;;
down)
  [ -n "${2:-}" ] || die "usage: runpod-vllm.sh down <pod-id>"
  terminate "$2"; list_pods; exit 0 ;;
down-all)
  ids=$(gql '{"query":"query { myself { pods { id } } }"}' \
        | python3 -c "import json,sys;print(' '.join(p['id'] for p in ((json.load(sys.stdin).get('data') or {}).get('myself') or {}).get('pods') or []))")
  for id in $ids; do terminate "$id"; done
  list_pods; exit 0 ;;
up) ;;
*) die "unknown command ${1:-}" ;;
esac

KEEP=0
[ "${2:-}" = "--keep" ] && KEEP=1

POD_ID=""
cleanup() {
  local code=$?
  if [ -n "$POD_ID" ] && [ "$KEEP" -eq 0 ]; then
    say ""
    say "tearing down (exit $code)"
    terminate "$POD_ID"
  elif [ -n "$POD_ID" ]; then
    say ""
    say "pod LEFT RUNNING at your request. It bills until stopped:"
    say "  bash scripts/runpod-vllm.sh down $POD_ID"
  fi
}
# Armed BEFORE the pod exists, so an interrupt between creation and the next
# line still tears it down.
trap cleanup EXIT INT TERM

say "provisioning"
say "  gpu    $GPU_TYPE"
say "  image  $IMAGE"
say "  model  $MODEL"

CREATE=$(python3 - "$GPU_TYPE" "$IMAGE" "$MODEL" "$VLLM_KEY" "$POD_NAME" <<'PY'
import json,sys
gpu,image,model,key,name = sys.argv[1:6]
args = (f"--model {model} --host 0.0.0.0 --port 8000 "
        f"--api-key {key} --max-model-len 8192 --served-model-name merc-vllm")
q = ("mutation { podFindAndDeployOnDemand(input:{"
     f'cloudType: ALL, gpuCount: 1, volumeInGb: 40, containerDiskInGb: 40, '
     f'minVcpuCount: 8, minMemoryInGb: 32, gpuTypeId: {json.dumps(gpu)}, '
     f'name: {json.dumps(name)}, imageName: {json.dumps(image)}, '
     f'ports: "8000/http", dockerArgs: {json.dumps(args)}'
     "}) { id imageName machineId } }")
print(json.dumps({"query": q}))
PY
)

RESP=$(gql "$CREATE")
POD_ID=$(printf '%s' "$RESP" | python3 -c "
import json,sys
d=json.load(sys.stdin)
if d.get('errors'):
    print('', end=''); sys.stderr.write('runpod: '+json.dumps(d['errors'])[:400]+'\n'); raise SystemExit
print(((d.get('data') or {}).get('podFindAndDeployOnDemand') or {}).get('id') or '')
")
[ -n "$POD_ID" ] || die "pod was not created (no capacity for $GPU_TYPE, or the API refused). Nothing is billing."
say "  pod    $POD_ID"

ENDPOINT="https://${POD_ID}-8000.proxy.runpod.net/v1"
say ""
say "waiting for vLLM to serve (model download + load, usually 3-8 minutes)"
# A pod whose runtime stays null is not starting -- the image pull stalled or the
# container died on launch. Catch that in ~3 minutes instead of burning 25 on a
# poll loop that only ever sees 404 from the proxy.
say "  checking the container actually starts"
STARTED=0
for i in $(seq 1 18); do
  rt=$(gql "$(printf '{"query":"query { pod(input:{podId:\"%s\"}) { runtime { uptimeInSeconds } } }"}' "$POD_ID")" \
       | python3 -c "
import json,sys
d=json.load(sys.stdin)
r=(((d.get('data') or {}).get('pod') or {}).get('runtime') or {})
print(r.get('uptimeInSeconds') if r.get('uptimeInSeconds') is not None else '')
" 2>/dev/null)
  [ -n "$rt" ] && { STARTED=1; say "  container up (${rt}s)"; break; }
  sleep 10
done
[ "$STARTED" -eq 1 ] || die "container never started after 3 minutes (runtime stayed null).
       The image probably failed to pull. Pod torn down; nothing is billing.
       Try MERC_VLLM_IMAGE=<a tag RunPod caches> and re-run."

READY=0
for i in $(seq 1 150); do
  code=$(printf 'header = "Authorization: Bearer %s"\n' "$VLLM_KEY" \
         | curl -sS --config - -o /dev/null -w '%{http_code}' --max-time 10 \
             "$ENDPOINT/models" 2>/dev/null || printf '000')
  if [ "$code" = "200" ]; then READY=1; break; fi
  [ $((i % 10)) -eq 0 ] && say "  still starting (${i}0s, last HTTP $code)"
  sleep 6
done
[ "$READY" -eq 1 ] || die "vLLM never became ready; pod torn down by the exit trap"

say ""
say "READY"
say "  endpoint  $ENDPOINT"
say "  api key   (in \$MERC_VLLM_API_KEY, not printed)"
say ""
say "  export MERC_GPU_ENDPOINT=$ENDPOINT"
say "  export MERC_GPU_API_KEY=<key>"

# Written so the caller can source the endpoint without the key touching a log.
{
  printf 'export MERC_GPU_ENDPOINT=%q\n' "$ENDPOINT"
  printf 'export MERC_GPU_API_KEY=%q\n' "$VLLM_KEY"
  printf 'export MERC_RUNPOD_POD_ID=%q\n' "$POD_ID"
} > "$ROOT/.merc-runpod.env"
chmod 600 "$ROOT/.merc-runpod.env"
say "  wrote .merc-runpod.env (chmod 600)"

if [ "$KEEP" -eq 1 ]; then
  say ""
  say "left running. Stop it when done:  bash scripts/runpod-vllm.sh down $POD_ID"
else
  say ""
  say "press Ctrl-C or wait; this pod tears down when this script exits."
  sleep "${MERC_RUNPOD_HOLD_SECS:-60}"
fi
