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

# A dollar cap is only real if the hourly rate used to convert it came from the
# provider immediately before provisioning.  Do not accept a remembered price
# or a hand-typed approximation: either exactly match the currently advertised
# secure-cloud rate or refuse before creating anything billable.
advertised_secure_hourly_rate() {
  local payload
  payload=$(python3 - "$GPU_TYPE" <<'PY'
import json,sys
gpu = sys.argv[1]
print(json.dumps({
  "query": "query($id: String!) { gpuTypes(input: { id: $id }) { lowestPrice(input: { gpuCount: 1, secureCloud: true }) { stockStatus uninterruptablePrice } } }",
  "variables": {"id": gpu},
}))
PY
) || return 1
  gql "$payload" | python3 -c '
import json,sys
d=json.load(sys.stdin)
types=(d.get("data") or {}).get("gpuTypes") or []
price=(types[0].get("lowestPrice") or {}).get("uninterruptablePrice") if types else None
if price is None:
    raise SystemExit("RunPod did not advertise a secure-cloud hourly price for this GPU")
print(price)
'
}

require_exact_rate() {
  local configured="$1" observed="$2"
  python3 - "$configured" "$observed" <<'PY'
from decimal import Decimal, InvalidOperation
import sys
try:
    configured, observed = map(Decimal, sys.argv[1:])
except InvalidOperation as exc:
    raise SystemExit(f"invalid hourly rate: {exc}")
if configured <= 0 or observed <= 0:
    raise SystemExit("hourly rates must be positive")
if configured != observed:
    raise SystemExit(f"configured ${configured}/hr does not exactly match RunPod's advertised ${observed}/hr")
PY
}

pod_hourly_rate() {
  gql '{"query":"query { myself { pods { id costPerHr } } }"}' \
    | python3 -c '
import json,sys
pod_id=sys.argv[1]
d=json.load(sys.stdin); pods=((d.get("data") or {}).get("myself") or {}).get("pods") or []
for pod in pods:
    if pod.get("id") == pod_id and pod.get("costPerHr") is not None:
        print(pod["costPerHr"]); break
else:
    raise SystemExit("RunPod did not report a costPerHr for the created pod")
' "$1"
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
  [ "$CLOUD" = "SECURE" ] || die "governed experiments require MERC_RUNPOD_CLOUD=SECURE so the advertised rate is unambiguous"
  ADVERTISED_COST_PER_HR=$(advertised_secure_hourly_rate) \
    || die "could not obtain RunPod's advertised secure-cloud hourly price before provisioning"
  require_exact_rate "$COST_PER_HR" "$ADVERTISED_COST_PER_HR" \
    || die "refusing a cap whose hourly rate does not match RunPod"
  BUDGET_SECS=$(python3 "$ROOT/scripts/runpod-spend-guard.py" budget \
                  --cost-per-hr "$COST_PER_HR" --cap-usd "$CAP_USD") \
    || die "the cap was refused by the spend guard"
  say "governed experiment"
  say "  cap       \$$CAP_USD at \$$COST_PER_HR/hr"
  say "  provider  observed secure-cloud rate \$$ADVERTISED_COST_PER_HR/hr"
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

  # `up --keep` records the identity before readiness so a failed startup gets
  # a receipt with its own clock, cost bound, and teardown verification rather
  # than disappearing behind the parent exit trap. This ignored file is mode
  # 0600 and is removed before every governed experiment to rule out staleness.
  PENDING_ENV="$ROOT/.merc-runpod-pending.env"
  if [ -e "$PENDING_ENV" ]; then rm "$PENDING_ENV"; fi

  emit_receipt() {
    local pod_id="$1" ready="$2" teardown_verified="$3" stopped_at="$4" orphans="$5"
    local receipt="evidence/runpod/spend-${pod_id}.json"
    python3 "$ROOT/scripts/runpod-spend-guard.py" receipt \
      --pod-id "$pod_id" --gpu "$GPU_TYPE" --image "$IMAGE" --model "$MODEL" \
      --cost-per-hr "$COST_PER_HR" --cap-usd "$CAP_USD" \
      --started-at "$STARTED_AT" --stopped-at "$stopped_at" \
      --teardown-verified "$teardown_verified" --ready "$ready" \
      --orphans "$orphans" --out "$receipt"
    local guard=$?
    say "  receipt   $receipt"
    return "$guard"
  }

  STARTED_AT=$(date +%s)
  UP_STATUS=0
  MERC_RUNPOD_HOLD_SECS=0 bash "$0" up --keep || UP_STATUS=$?
  if [ "$UP_STATUS" -ne 0 ]; then
    if [ ! -f "$PENDING_ENV" ]; then
      die "provisioning failed before a pod identity was recorded; the exit sweep will run"
    fi
    # shellcheck disable=SC1090
    . "$PENDING_ENV"
    say "provisioning failed; verifying sweep before an inadmissible receipt"
    sweep_all
    TEARDOWN_VERIFIED=false
    [ "$(pod_exists "$MERC_RUNPOD_POD_ID")" = "no" ] && TEARDOWN_VERIFIED=true
    STOPPED_AT=$(date +%s)
    ORPHANS=$(gql '{"query":"query { myself { pods { id } } }"}' \
      | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(','.join(p['id'] for p in (m.get('pods') or [])))" 2>/dev/null)
    emit_receipt "$MERC_RUNPOD_POD_ID" false "$TEARDOWN_VERIFIED" "$STOPPED_AT" "$ORPHANS" || true
    exit 1
  fi
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

  emit_receipt "$MERC_RUNPOD_POD_ID" "$READY" "$TEARDOWN_VERIFIED" "$STOPPED_AT" "$ORPHANS"
  GUARD=$?
  exit "$GUARD" ;;
up) ;;
*) die "unknown command ${1:-}" ;;
esac

KEEP=0
[ "${2:-}" = "--keep" ] && KEEP=1

POD_ID=""
UP_READY=0
cleanup() {
  local code=$?
  if [ -n "$POD_ID" ] && { [ "$KEEP" -eq 0 ] || [ "$UP_READY" -eq 0 ]; }; then
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

# REST creation can select a differently priced machine than the pre-flight
# lowest-price query.  Fail closed and let the armed trap terminate it rather
# than continue a run whose cap conversion is false.
ACTUAL_COST_PER_HR=$(pod_hourly_rate "$POD_ID") || { KEEP=0; die "RunPod did not report the created pod's hourly rate"; }
if ! require_exact_rate "${MERC_RUNPOD_COST_PER_HR:-$ACTUAL_COST_PER_HR}" "$ACTUAL_COST_PER_HR"; then
  KEEP=0
  die "created pod rate differs from the governed experiment rate"
fi
say "  rate   \$$ACTUAL_COST_PER_HR/hr (provider verified)"

# Persist a private pending identity before the long image/model startup. The
# parent experiment uses it only to mint an inadmissible receipt if readiness
# fails; a ready endpoint is written to .merc-runpod.env only after HTTP 200.
PENDING_ENV="$ROOT/.merc-runpod-pending.env"
{
  printf 'export MERC_GPU_ENDPOINT=%q\n' "https://${POD_ID}-8000.proxy.runpod.net/v1"
  printf 'export MERC_GPU_API_KEY=%q\n' "$VLLM_KEY"
  printf 'export MERC_RUNPOD_POD_ID=%q\n' "$POD_ID"
  printf 'export MERC_RUNPOD_COST_PER_HR=%q\n' "$ACTUAL_COST_PER_HR"
} > "$PENDING_ENV"
chmod 600 "$PENDING_ENV"

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
  [ $((i % 10)) -eq 0 ] && say "  still starting ($((i * 6))s, last HTTP $code)"
  sleep 6
done
[ "$READY" -eq 1 ] || die "vLLM never became ready; pod torn down by the exit trap"

say ""
say "READY"
say "  endpoint  $ENDPOINT"
say "  api key   (in \$MERC_GPU_API_KEY, not printed)"
say ""
say "  export MERC_GPU_ENDPOINT=$ENDPOINT"
say "  export MERC_GPU_API_KEY=<key>"

# Promote the already-private pending record atomically only after the engine
# has answered readiness. A consumer can never mistake a starting pod for one
# it may send paid traffic to.
mv "$PENDING_ENV" "$ROOT/.merc-runpod.env"
UP_READY=1
say "  wrote .merc-runpod.env (chmod 600)"

if [ "$KEEP" -eq 1 ]; then
  say ""
  say "left running. Stop it when done:  bash scripts/runpod-vllm.sh down $POD_ID"
else
  say ""
  say "press Ctrl-C or wait; this pod tears down when this script exits."
  sleep "${MERC_RUNPOD_HOLD_SECS:-60}"
fi
