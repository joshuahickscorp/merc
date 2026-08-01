#!/usr/bin/env bash
# Provision a RunPod A100 running pinned vLLM, print its endpoint, and make sure
# it dies.
#
# This spends real money from a prepaid balance. The dangerous failure is not a
# bad deploy, it is a pod nobody remembers to stop: at $1.19/hr a forgotten A100
# quietly eats the balance. So teardown is wired to EXIT before the pod is ever
# created, and `--keep` is the only way to leave one running.
#
# `experiment` is the governed form and the one a paid lane should use. `up` bounds
# nothing but its own runtime; `experiment` converts a dollar cap into a lifetime
# through scripts/runpod-spend-guard.py, refuses to start while any other pod is
# billing, sweeps every pod on exit however it exits, verifies the teardown, and
# emits a spend receipt that is INADMISSIBLE if the teardown was unverified, the
# lifetime bound did not hold, the image was a floating tag, or a pod was left
# behind.
#
#   bash scripts/runpod-vllm.sh experiment    # GOVERNED: cost cap, enforced lifetime,
#                                             # orphan sweep, verified teardown, receipt
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

GPU_TYPE="${MERC_RUNPOD_GPU:-NVIDIA RTX A5000}"
# SECURE, not ALL. COMMUNITY had no capacity for any probed GPU class, and
# ALL resolves to community first, so ALL silently found nothing.
CLOUD="${MERC_RUNPOD_CLOUD:-SECURE}"
# The default is the only locally admitted vLLM profile.  Read every serving
# parameter from that one authority so the pod cannot silently use a model,
# alias, revision, context window, or sequence limit different from the offer
# it will register.  Overrides remain available for a separately governed
# profile, but every image still has to be an immutable manifest digest.
PROFILE_PATH="${MERC_VLLM_PROFILE_PATH:-$ROOT/control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json}"
[ -f "$PROFILE_PATH" ] || die "vLLM profile is missing: $PROFILE_PATH"
profile_value() { jq -er "$1" "$PROFILE_PATH"; }
IMAGE="${MERC_VLLM_IMAGE:-$(profile_value '.container_image')}"
MODEL="${MERC_VLLM_MODEL:-$(profile_value '.model_repository')}"
VLLM_SERVED_MODEL="${MERC_VLLM_SERVED_MODEL:-$(profile_value '.model_alias')}"
VLLM_MODEL_REVISION="${MERC_VLLM_MODEL_REVISION:-$(profile_value '.model_revision')}"
VLLM_TOKENIZER_REVISION="${MERC_VLLM_TOKENIZER_REVISION:-$(profile_value '.tokenizer_revision')}"
VLLM_MAX_MODEL_LEN="${MERC_VLLM_MAX_MODEL_LEN:-$(profile_value '.max_model_length')}"
VLLM_GPU_MEMORY_UTILIZATION="${MERC_VLLM_GPU_MEMORY_UTILIZATION:-$(profile_value '.gpu_memory_utilization')}"
VLLM_MAX_NUM_SEQS="${MERC_VLLM_MAX_NUM_SEQS:-$(profile_value '.max_active_sequences')}"
[[ "$IMAGE" =~ @sha256:[0-9a-f]{64}$ ]] || die "MERC_VLLM_IMAGE must be an immutable OCI digest"
[[ "$VLLM_MODEL_REVISION" =~ ^[0-9a-f]{40}$ ]] || die "MERC_VLLM_MODEL_REVISION must be an exact 40-character revision"
[[ "$VLLM_TOKENIZER_REVISION" =~ ^[0-9a-f]{40}$ ]] || die "MERC_VLLM_TOKENIZER_REVISION must be an exact 40-character revision"
[[ "$VLLM_SERVED_MODEL" =~ ^[A-Za-z0-9._:-]+$ ]] || die "MERC_VLLM_SERVED_MODEL contains unsupported characters"
[[ "$VLLM_MAX_MODEL_LEN" =~ ^[1-9][0-9]*$ && "$VLLM_MAX_NUM_SEQS" =~ ^[1-9][0-9]*$ ]] || die "vLLM limits must be positive integers"
VLLM_KEY="${MERC_VLLM_API_KEY:-cx_vllm_$(openssl rand -hex 24)}"
[[ "$VLLM_KEY" =~ ^cx_vllm_[A-Za-z0-9_-]{16,248}$ ]] || die "MERC_VLLM_API_KEY must be a cx_vllm_ credential suitable for realtime offer registration"
export MERC_VLLM_SERVED_MODEL="$VLLM_SERVED_MODEL"
export MERC_VLLM_MODEL_REVISION="$VLLM_MODEL_REVISION"
export MERC_VLLM_TOKENIZER_REVISION="$VLLM_TOKENIZER_REVISION"
export MERC_VLLM_MAX_MODEL_LEN="$VLLM_MAX_MODEL_LEN"
export MERC_VLLM_GPU_MEMORY_UTILIZATION="$VLLM_GPU_MEMORY_UTILIZATION"
export MERC_VLLM_MAX_NUM_SEQS="$VLLM_MAX_NUM_SEQS"
POD_NAME="merc-canary-vllm"

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

gql() {
  printf 'header = "Authorization: Bearer %s"\n' "$RUNPOD_API_KEY" \
    | curl -sS --config - -H 'content-type: application/json' --max-time 60 \
        -X POST https://api.runpod.io/graphql -d "$1"
}

# REST, not GraphQL, for anything that creates or destroys. podFindAndDeployOnDemand
# returns a pod id even when no machine was allocated -- two A100s billed for
# 25 minutes each without ever starting because of that silent success. The REST
# API answers 500 "There are no instances currently available" for the same
# request.
rest() {
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    printf 'header = "Authorization: Bearer %s"\n' "$RUNPOD_API_KEY" \
      | curl -sS --config - -H 'content-type: application/json' --max-time 60 \
          -X "$method" "https://rest.runpod.io/v1$path" -d "$body"
  else
    printf 'header = "Authorization: Bearer %s"\n' "$RUNPOD_API_KEY" \
      | curl -sS --config - --max-time 60 -X "$method" "https://rest.runpod.io/v1$path"
  fi
}

json() { python3 -c "import json,sys;print(json.dumps(json.load(sys.stdin)))"; }

# Terminate and VERIFY. The previous version discarded the DELETE result with
# `|| true` and printed "terminated" unconditionally, so a failed teardown
# reported success while the pod kept billing -- observed on 2026-07-30, when a
# run logged "pod torn down by the exit trap" and left an A40 running at
# $0.44/hr alongside a second pod. Announcing a teardown that did not happen is
# worse than a noisy failure, because nobody goes looking.
pod_exists() {
  gql '{"query":"query { myself { pods { id } } }"}' \
  | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print('yes' if any(p['id']=='$1' for p in (m.get('pods') or [])) else 'no')" 2>/dev/null
}

terminate() {
  local id="$1" attempt
  [ -z "$id" ] && return 0
  for attempt in 1 2 3; do
    rest DELETE "/pods/$id" >/dev/null 2>&1 || true
    sleep 3
    if [ "$(pod_exists "$id")" = "no" ]; then
      say "  terminated $id (verified)"
      return 0
    fi
    say "  teardown attempt $attempt did not take for $id; retrying"
  done
  say "  !! FAILED TO TERMINATE $id -- IT IS STILL BILLING"
  say "  !! run: bash scripts/runpod-vllm.sh down-all"
  return 1
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
experiment)
  # A governed paid experiment: cost cap, enforced lifetime, orphan sweep before
  # AND after, verified teardown, spend receipt.
  #
  # `up` already refuses to leave a pod running and verifies its own teardown.
  # What it does not have is a MONEY bound: nothing stops a run holding a pod for
  # as long as its own logic takes. The cap is that bound, converted to a
  # lifetime by scripts/runpod-spend-guard.py, and the conversion lives in Python
  # because it decides how long real money burns and shell cannot be unit tested
  # without spending it.
  CAP_USD="${MERC_RUNPOD_CAP_USD:-2.00}"
  COST_PER_HR="${MERC_RUNPOD_COST_PER_HR:?MERC_RUNPOD_COST_PER_HR is required: the cap cannot be converted to a lifetime without the advertised hourly rate for $GPU_TYPE}"
  BUDGET_SECS=$(python3 "$ROOT/scripts/runpod-spend-guard.py" budget \
                  --cost-per-hr "$COST_PER_HR" --cap-usd "$CAP_USD") \
    || die "the cap was refused by the spend guard"
  say "governed experiment"
  say "  cap       \$$CAP_USD at \$$COST_PER_HR/hr"
  say "  lifetime  ${BUDGET_SECS}s (the rest of the cap is teardown headroom)"

  # Pre-flight sweep. A pod that is already running is already billing, and
  # attributing its cost to this experiment's receipt would be a lie in either
  # direction — so the run refuses rather than guessing.
  PREEXISTING=$(gql '{"query":"query { myself { pods { id } } }"}' \
    | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(','.join(p['id'] for p in (m.get('pods') or [])))" 2>/dev/null)
  [ -z "$PREEXISTING" ] || die "pods are already running and billing: $PREEXISTING
       Stop them first:  bash scripts/runpod-vllm.sh down-all"

  # Armed before anything is created. `up --keep` deliberately leaves its pod
  # behind, so this is the only thing between a crash in the middle of the
  # experiment and a pod that bills until someone notices.
  sweep_all() {
    local ids
    ids=$(gql '{"query":"query { myself { pods { id } } }"}' \
      | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(' '.join(p['id'] for p in (m.get('pods') or [])))" 2>/dev/null)
    for id in $ids; do terminate "$id"; done
  }
  trap 'say ""; say "sweeping every pod (experiment exit)"; sweep_all' EXIT INT TERM

  STARTED_AT=$(date +%s)
  MERC_RUNPOD_HOLD_SECS=0 bash "$0" up --keep || die "provisioning failed; the sweep will run"
  # shellcheck disable=SC1091
  . "$ROOT/.merc-runpod.env"
  READY=true

  # The bounded workload. Default is a single completion against the pinned model
  # — enough to prove the engine serves — and MERC_RUNPOD_EXPERIMENT_CMD replaces
  # it with whatever the lane under test needs.
  CMD="${MERC_RUNPOD_EXPERIMENT_CMD:-}"
  if [ -z "$CMD" ]; then
    CMD="printf 'header = \"Authorization: Bearer %s\"\n' \"\$MERC_GPU_API_KEY\" | curl -sS --config - -H 'content-type: application/json' --max-time 60 -o /dev/null -w 'completion HTTP %{http_code}\n' \"\$MERC_GPU_ENDPOINT/chat/completions\" -d '{\"model\":\"$MODEL\",\"messages\":[{\"role\":\"user\",\"content\":\"one word\"}],\"max_tokens\":4}'"
  fi
  say ""
  say "running the experiment under a ${BUDGET_SECS}s lifetime bound"
  bash -c "$CMD" &
  CMD_PID=$!
  ELAPSED=0
  while kill -0 "$CMD_PID" 2>/dev/null; do
    if [ "$ELAPSED" -ge "$BUDGET_SECS" ]; then
      say "  lifetime bound reached; killing the experiment and tearing down"
      kill -TERM "$CMD_PID" 2>/dev/null || true
      break
    fi
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done
  wait "$CMD_PID" 2>/dev/null || say "  experiment command exited non-zero"

  say ""
  say "tearing down"
  terminate "$MERC_RUNPOD_POD_ID"
  TEARDOWN_VERIFIED=true
  [ "$(pod_exists "$MERC_RUNPOD_POD_ID")" = "no" ] || TEARDOWN_VERIFIED=false
  STOPPED_AT=$(date +%s)

  ORPHANS=$(gql '{"query":"query { myself { pods { id } } }"}' \
    | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(','.join(p['id'] for p in (m.get('pods') or [])))" 2>/dev/null)

  RECEIPT="evidence/runpod/spend-${MERC_RUNPOD_POD_ID}.json"
  python3 "$ROOT/scripts/runpod-spend-guard.py" receipt \
    --pod-id "$MERC_RUNPOD_POD_ID" --gpu "$GPU_TYPE" --image "$IMAGE" --model "$MODEL" \
    --cost-per-hr "$COST_PER_HR" --cap-usd "$CAP_USD" \
    --started-at "$STARTED_AT" --stopped-at "$STOPPED_AT" \
    --teardown-verified "$TEARDOWN_VERIFIED" --ready "$READY" \
    --orphans "$ORPHANS" --out "$RECEIPT"
  GUARD=$?
  say "  receipt   $RECEIPT"
  exit "$GUARD" ;;
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
say "  gpu    $GPU_TYPE ($CLOUD)"
say "  image  $IMAGE"
say "  model  $MODEL"

CREATE=$(python3 "$ROOT/scripts/runpod-create-payload.py" \
           "$GPU_TYPE" "$IMAGE" "$MODEL" "$VLLM_KEY" "$POD_NAME" "$CLOUD")

RESP=$(rest POST /pods "$CREATE")
POD_ID=$(printf '%s' "$RESP" | python3 -c "
import json,sys
d=json.load(sys.stdin)
if d.get('error'):
    sys.stderr.write('runpod: '+str(d['error'])[:200]+chr(10))
print(d.get('id') or '')
")
[ -n "$POD_ID" ] || die "pod was not created. RunPod reported no capacity for $GPU_TYPE on $CLOUD.
       Nothing is billing. Try another GPU (MERC_RUNPOD_GPU=) or cloud (MERC_RUNPOD_CLOUD=)."
say "  pod    $POD_ID"

ENDPOINT="https://${POD_ID}-8000.proxy.runpod.net/v1"
say ""
say "waiting for vLLM to serve (image pull + model download, 5-10 minutes)"
# Readiness is the PROXY answering 200, never pod.runtime. runtime is populated
# by RunPod's in-container agent; an image that does not run that agent leaves it
# null forever, so polling it waits out a perfectly healthy engine. Two A100s
# were torn down as "never started" for exactly that reason.
READY=0
for i in $(seq 1 90); do
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
