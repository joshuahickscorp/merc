#!/usr/bin/env bash
# Provision a RunPod A100 running pinned vLLM, print its endpoint, and make sure
# it dies.
#
# This spends real money from a prepaid balance. The dangerous failure is not a
# bad deploy, it is a pod nobody remembers to stop: at $1.19/hr a forgotten A100
# quietly eats the balance. So teardown is wired to EXIT before the pod is ever
# created, and `--keep` is the only way to leave one running.
#
# Trap-based teardown cannot catch SIGKILL, host loss, or death between create
# and trap arming. Defence in depth beside the trap:
#   1. Intent marker written BEFORE create, bound with pod id the instant create
#      returns — so a death still leaves a local trail.
#   2. Entry reconcile before any create — live pods matched against intents,
#      receipts, and .merc-runpod.env; unrecognised billing is reported loudly
#      and the run refuses (never silent terminate by default).
#   3. Standalone `reconcile` for operators/cron — exit non-zero on orphans.
#
# `experiment` is the governed form and the one a paid lane should use. `up` bounds
# nothing but its own runtime; `experiment` converts a dollar cap into a lifetime
# through scripts/runpod-spend-guard.py, refuses to start while any other pod is
# billing, stops only its positively identified pod on exit, verifies teardown,
# emits a spend receipt that is INADMISSIBLE if the teardown was unverified, the
# lifetime bound did not hold, the image was a floating tag, or a pod was left
# behind.
#
#   bash scripts/runpod-vllm.sh experiment    # GOVERNED: cost cap, enforced lifetime,
#                                             # own-pod teardown, verified receipt
#   bash scripts/runpod-vllm.sh up            # provision, print endpoint, tear down on exit
#   bash scripts/runpod-vllm.sh up --keep     # leave it running (prints the stop command)
#   bash scripts/runpod-vllm.sh list          # what is running right now
#   bash scripts/runpod-vllm.sh reconcile     # detect orphans (exit 1 if any); no terminate
#   bash scripts/runpod-vllm.sh reconcile --terminate-orphans
#                                             # loud terminate of orphans only (explicit)
#   bash scripts/runpod-vllm.sh down <pod-id> # stop one
#   bash scripts/runpod-vllm.sh down-all      # stop everything (the panic button)

set -euo pipefail
# This script writes short-lived endpoint credentials and local lease tokens.
# New files must be private from the instant the shell opens them, not only
# after a later chmod succeeds.
umask 077
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

say() { printf '%s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
usage() {
  cat <<'USAGE'
usage: scripts/runpod-vllm.sh <command>

commands:
  experiment                 governed, cost-bounded run with receipt and teardown
  up [--keep]                provision explicitly; --keep is bounded
  list                       list current account pods
  reconcile [--terminate-orphans]
  renew-keep
  down <pod-id>
  down-all                   explicit account-wide emergency stop
USAGE
}

# Never make a missing command bill money. Help is deliberately available
# without credentials or profile validation so operators can inspect the
# command surface safely on a fresh host.
COMMAND="${1:-}"
case "$COMMAND" in
  ""|help|-h|--help) usage; exit 0 ;;
  list|reconcile|renew-keep|down|down-all|experiment|up) ;;
  *) usage >&2; die "unknown command $COMMAND" ;;
esac

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
# State that decides paid-pod ownership belongs under Git's common directory,
# not a worktree: sibling worktrees of this clone must contend for one local
# provisioning lease. This does not claim cross-host/account serialization;
# that requires provider-side idempotency or an authorized durable coordinator.
GIT_COMMON_DIR="$(git -C "$ROOT" rev-parse --git-common-dir)"
if [[ "$GIT_COMMON_DIR" != /* ]]; then GIT_COMMON_DIR="$ROOT/$GIT_COMMON_DIR"; fi
RUNPOD_STATE_ROOT="${MERC_RUNPOD_STATE_ROOT:-$GIT_COMMON_DIR/merc-runpod}"
INTENT_DIR="${MERC_RUNPOD_INTENT_DIR:-$RUNPOD_STATE_ROOT/intent}"
SPEND_GUARD="$ROOT/scripts/runpod-spend-guard.py"
# Carries across a provision path so experiment can complete the same intent.
# It is never read from a global "current request" file: two concurrent callers
# could overwrite such a file and complete each other's intent.
REQUEST_ID="${MERC_RUNPOD_REQUEST_ID:-}"
# An owner token is a short-lived local lease capability.  It must not arrive
# through a caller environment, where every subprocess would inherit it.  The
# only cross-process provision handoff is the private descriptor below; local
# ready/pending records are sourced only by the narrow helper that needs them.
if [ -n "${MERC_RUNPOD_OWNER_TOKEN:-}" ]; then
  die "MERC_RUNPOD_OWNER_TOKEN must not be supplied through the environment; use the private owner-token FD handoff"
fi
OWNER_TOKEN=""
OWNER_PID="${MERC_RUNPOD_OWNER_PID:-}"
OWNER_PROCESS_START="${MERC_RUNPOD_OWNER_PROCESS_START:-}"
OWNER_BOOT_ID="${MERC_RUNPOD_OWNER_BOOT_ID:-}"
if [ -n "${MERC_RUNPOD_OWNER_TOKEN_FD:-}" ]; then
  [[ "$MERC_RUNPOD_OWNER_TOKEN_FD" =~ ^[3-9][0-9]*$ ]] \
    || die "MERC_RUNPOD_OWNER_TOKEN_FD must name a private inherited descriptor"
  IFS= read -r -u "$MERC_RUNPOD_OWNER_TOKEN_FD" OWNER_TOKEN \
    || die "could not read parent owner token from private descriptor"
fi

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
    # Keep both bearer token and JSON body off argv.  curl consumes auth from a
    # private process-substitution FD and the payload from stdin; neither value
    # appears in ps output as it did with `-d "$body"`.
    printf '%s' "$body" | curl -sS \
      --config <(printf 'header = "Authorization: Bearer %s"\n' "$RUNPOD_API_KEY") \
      -H 'content-type: application/json' --max-time 60 -X "$method" \
      "https://rest.runpod.io/v1$path" --data-binary @-
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

live_pods_json() {
  gql '{"query":"query { myself { pods { id name desiredStatus costPerHr } } }"}' \
  | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(json.dumps(m.get('pods') or []))
"
}

# Fetch live pods, classify against local trails. Prints the human report on
# stderr-via-stdout of the guard; returns the guard's exit code (1 = orphans).
# Does NOT terminate anything.
run_reconcile() {
  local live extra=()
  live=$(live_pods_json) || die "could not list live pods for reconcile"
  [ -n "${1:-}" ] && extra+=(--json)
  python3 "$SPEND_GUARD" reconcile \
    --live-pods-json "$live" \
    --intent-dir "$INTENT_DIR" \
    --receipts-dir "$ROOT/evidence/runpod" \
    "${extra[@]+"${extra[@]}"}"
}

# Entry gate: before any create. Loud refuse on orphans or any pre-existing
# billing unless the operator explicitly opts into terminating orphans only.
# Never silent. The trap for the currently identified pod remains armed.
entry_reconcile_or_refuse() {
  local live report_json orphan_ids preexisting
  live=$(live_pods_json) || die "could not list live pods for entry reconcile"
  report_json=$(python3 "$SPEND_GUARD" reconcile \
    --live-pods-json "$live" \
    --intent-dir "$INTENT_DIR" \
    --receipts-dir "$ROOT/evidence/runpod" \
    --json) || true
  printf '%s' "$report_json" | python3 -c "
import json,sys
d=json.load(sys.stdin)
if not d.get('live'):
    print('reconcile: no live pods')
else:
    print(f\"reconcile: {len(d['live'])} live pod(s)\")
    for p in d['live']:
        flag='ORPHAN' if p.get('orphan') else 'owned'
        rate=p.get('cost_per_hr')
        rate_s=f'\${rate}/hr' if rate is not None else '?/hr'
        print(f\"  [{flag}] {p.get('pod_id')}  {p.get('name') or '-'}  {p.get('desired_status') or '-'}  {rate_s}  class={p.get('classification')}\")
        print(f\"           owner: {p.get('owner')}\")
        print(f\"           {p.get('detail')}\")
if d.get('orphans'):
    print(f\"reconcile: {len(d['orphans'])} ORPHAN pod(s) billing with no living owner\")
    print('  refuse quiet success. Inspect with: bash scripts/runpod-vllm.sh reconcile')
for s in d.get('stale_intents') or []:
    print(f\"  stale intent: request={s.get('request_id')} pod={s.get('pod_id')}\")
for u in d.get('unbound_intents') or []:
    print(f\"  unbound intent: request={u.get('request_id')}\")
"

  orphan_ids=$(printf '%s' "$report_json" | python3 -c "
import json,sys
print(' '.join(json.load(sys.stdin).get('orphan_pod_ids') or []))
" 2>/dev/null || true)
  preexisting=$(printf '%s' "$live" | python3 -c "
import json,sys
print(' '.join(p.get('id','') for p in json.load(sys.stdin) if p.get('id')))
" 2>/dev/null || true)

  if [ -n "$orphan_ids" ]; then
    say ""
    say "!! ORPHAN POD(S) BILLING WITH NO LIVING OWNER: $orphan_ids"
    say "!! A prior process likely died (SIGKILL/host loss) without trap teardown."
    if [ "${MERC_RUNPOD_TERMINATE_ORPHANS:-0}" = "1" ]; then
      say "!! MERC_RUNPOD_TERMINATE_ORPHANS=1 set — terminating orphans loudly"
      local id
      for id in $orphan_ids; do
        say "!! terminating orphan $id"
        terminate "$id" || die "failed to terminate orphan $id; refusing to create another pod"
      done
    else
      die "refusing to create a pod while orphans are billing.
       Inspect:  bash scripts/runpod-vllm.sh reconcile
       Terminate orphans only (explicit):  MERC_RUNPOD_TERMINATE_ORPHANS=1 bash scripts/runpod-vllm.sh reconcile --terminate-orphans
       Panic button:  bash scripts/runpod-vllm.sh down-all"
    fi
  fi

  # Even owned / deliberate --keep pods mean something is already billing.
  # Governed and casual creates both refuse rather than share the account.
  if [ -n "$preexisting" ]; then
    preexisting=$(live_pods_json | python3 -c "
import json,sys
print(' '.join(p.get('id','') for p in json.load(sys.stdin) if p.get('id')))
" 2>/dev/null || true)
  fi
  if [ -n "$preexisting" ]; then
    die "pods are already running and billing: $preexisting
       Stop them first:  bash scripts/runpod-vllm.sh down-all
       Or reconcile:     bash scripts/runpod-vllm.sh reconcile"
  fi
}

ensure_lease_owner() {
  local identity
  if [ -z "$OWNER_PID" ]; then
    OWNER_PID="$$"
    identity=$(python3 "$SPEND_GUARD" process-identity --pid "$OWNER_PID") \
      || die "cannot establish a PID/start/boot lease identity"
    IFS='|' read -r OWNER_PID OWNER_PROCESS_START OWNER_BOOT_ID <<< "$identity"
  fi
  [ -n "$OWNER_PROCESS_START" ] && [ -n "$OWNER_BOOT_ID" ] \
    || die "lease owner needs PID, process-start, and boot identity together"
  if [ -z "$OWNER_TOKEN" ]; then
    OWNER_TOKEN=$(openssl rand -hex 24)
  fi
}

ensure_request_id() {
  [ -n "$REQUEST_ID" ] || REQUEST_ID="req-$(date +%s)-$(openssl rand -hex 12)"
}

renew_create_lease() {
  python3 "$SPEND_GUARD" intent-renew-create \
    --request-id "$REQUEST_ID" \
    --owner-token-fd 3 \
    --owner-pid "$OWNER_PID" \
    --owner-process-start "$OWNER_PROCESS_START" \
    --owner-boot-id "$OWNER_BOOT_ID" \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null
}

renew_bound_lease() {
  python3 "$SPEND_GUARD" intent-renew \
    --request-id "$REQUEST_ID" \
    --owner-token-fd 3 \
    --owner-pid "$OWNER_PID" \
    --owner-process-start "$OWNER_PROCESS_START" \
    --owner-boot-id "$OWNER_BOOT_ID" \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null
}

write_create_intent() {
  local purpose="$1"
  ensure_lease_owner
  ensure_request_id
  python3 "$SPEND_GUARD" intent-write \
    --request-id "$REQUEST_ID" \
    --purpose "$purpose" \
    --gpu "$GPU_TYPE" \
    --name "$POD_NAME" \
    --owner-token-fd 3 \
    --owner-pid "$OWNER_PID" \
    --owner-process-start "$OWNER_PROCESS_START" \
    --owner-boot-id "$OWNER_BOOT_ID" \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null
  say "  intent  $REQUEST_ID (account lease recorded before create)"
}

bind_create_intent() {
  local pod_id="$1"
  [ -n "$REQUEST_ID" ] || die "no request id to bind for pod $pod_id"
  python3 "$SPEND_GUARD" intent-bind \
    --request-id "$REQUEST_ID" \
    --pod-id "$pod_id" \
    --owner-token-fd 3 \
    --owner-pid "$OWNER_PID" \
    --owner-process-start "$OWNER_PROCESS_START" \
    --owner-boot-id "$OWNER_BOOT_ID" \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null
  say "  intent  bound $REQUEST_ID -> $pod_id (active provisioning lease)"
}

promote_operator_keep() {
  python3 "$SPEND_GUARD" intent-promote-operator-keep \
    --request-id "$REQUEST_ID" \
    --owner-token-fd 3 \
    --keep-seconds "${MERC_RUNPOD_KEEP_SECS:-90}" \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null
}

complete_create_intent() {
  local rid="${1:-$REQUEST_ID}"
  [ -n "$rid" ] || return 0
  [ -n "$OWNER_TOKEN" ] || return 0
  python3 "$SPEND_GUARD" intent-complete \
    --request-id "$rid" \
    --owner-token-fd 3 \
    --intent-dir "$INTENT_DIR" 3<<<"$OWNER_TOKEN" >/dev/null 2>&1 || true
}

complete_ready_intent_if_matching() {
  local pod_id="$1" ready_env="$ROOT/.merc-runpod.env"
  [ -f "$ready_env" ] || return 0
  (
    # shellcheck disable=SC1090
    . "$ready_env"
    [ "${MERC_RUNPOD_POD_ID:-}" = "$pod_id" ] || exit 0
    [ -n "${MERC_RUNPOD_REQUEST_ID:-}" ] || exit 0
    [ -n "${MERC_RUNPOD_OWNER_TOKEN:-}" ] || exit 0
    export -n MERC_RUNPOD_OWNER_TOKEN
    python3 "$SPEND_GUARD" intent-complete \
      --request-id "$MERC_RUNPOD_REQUEST_ID" \
      --owner-token-fd 3 \
      --intent-dir "$INTENT_DIR" 3<<<"$MERC_RUNPOD_OWNER_TOKEN" >/dev/null 2>&1 || true
  )
}

complete_pending_intent_if_matching() {
  # Pending state is never ownership proof. It is consulted here only *after*
  # terminate() has verified provider absence, to leave an honest terminal
  # tombstone for an operator who manually stopped a pre-ready pod.
  local pod_id="$1" pending_env="$ROOT/.merc-runpod-pending.env"
  [ -f "$pending_env" ] || return 0
  (
    # shellcheck disable=SC1090
    . "$pending_env"
    [ "${MERC_RUNPOD_POD_ID:-}" = "$pod_id" ] || exit 0
    [ -n "${MERC_RUNPOD_REQUEST_ID:-}" ] || exit 0
    [ -n "${MERC_RUNPOD_OWNER_TOKEN:-}" ] || exit 0
    export -n MERC_RUNPOD_OWNER_TOKEN
    python3 "$SPEND_GUARD" intent-complete \
      --request-id "$MERC_RUNPOD_REQUEST_ID" \
      --owner-token-fd 3 \
      --intent-dir "$INTENT_DIR" 3<<<"$MERC_RUNPOD_OWNER_TOKEN" >/dev/null 2>&1 || true
  )
}

case "$COMMAND" in
list) list_pods; exit 0 ;;
reconcile)
  TERMINATE_FLAG=0
  [ "${2:-}" = "--terminate-orphans" ] && TERMINATE_FLAG=1
  [ "${MERC_RUNPOD_TERMINATE_ORPHANS:-0}" = "1" ] && TERMINATE_FLAG=1
  say "reconciling live pods against local leases, intents, and receipts"
  live=$(live_pods_json) || die "could not list live pods"
  # Human report first (exit ignored); then JSON for decisions.
  python3 "$SPEND_GUARD" reconcile \
    --live-pods-json "$live" \
    --intent-dir "$INTENT_DIR" \
    --receipts-dir "$ROOT/evidence/runpod" || true
  report_json=$(python3 "$SPEND_GUARD" reconcile \
    --live-pods-json "$live" \
    --intent-dir "$INTENT_DIR" \
    --receipts-dir "$ROOT/evidence/runpod" \
    --json) || true
  orphan_ids=$(printf '%s' "$report_json" | python3 -c "
import json,sys
print(' '.join(json.load(sys.stdin).get('orphan_pod_ids') or []))
")
  if [ -n "$orphan_ids" ] && [ "$TERMINATE_FLAG" -eq 1 ]; then
    say ""
    say "!! --terminate-orphans: stopping orphan pods only (not silent; you asked)"
    for id in $orphan_ids; do
      say "!! terminating orphan $id"
      terminate "$id" || die "failed to terminate orphan $id"
    done
    list_pods
    # Re-run after terminate to set exit code honestly.
    live=$(live_pods_json)
    python3 "$SPEND_GUARD" reconcile \
      --live-pods-json "$live" \
      --intent-dir "$INTENT_DIR" \
      --receipts-dir "$ROOT/evidence/runpod"
    exit $?
  fi
  if [ -n "$orphan_ids" ]; then
    say ""
    say "orphans detected; not terminating (pass --terminate-orphans to stop them)"
    exit 1
  fi
  exit 0
  ;;
renew-keep)
  READY_ENV="$ROOT/.merc-runpod.env"
  [ -f "$READY_ENV" ] || die "no ready RunPod record to renew"
  # This file is private (0600) and already contains the local API credential;
  # it supplies only the token for an existing short operator_keep lease.
  # shellcheck disable=SC1090
  . "$READY_ENV"
  : "${MERC_RUNPOD_REQUEST_ID:?ready record lacks request id}"
  : "${MERC_RUNPOD_OWNER_TOKEN:?ready record lacks keep token}"
  export -n MERC_RUNPOD_OWNER_TOKEN
  python3 "$SPEND_GUARD" intent-renew-operator-keep \
    --request-id "$MERC_RUNPOD_REQUEST_ID" \
    --owner-token-fd 3 \
    --keep-seconds "${MERC_RUNPOD_KEEP_SECS:-90}" \
    --intent-dir "$INTENT_DIR" 3<<<"$MERC_RUNPOD_OWNER_TOKEN" >/dev/null
  say "operator keep renewed for at most ${MERC_RUNPOD_KEEP_SECS:-90}s; renew again or stop the pod"
  exit 0
  ;;
down)
  [ -n "${2:-}" ] || die "usage: runpod-vllm.sh down <pod-id>"
  if terminate "$2"; then
    complete_ready_intent_if_matching "$2"
    complete_pending_intent_if_matching "$2"
  fi
  list_pods; exit 0 ;;
down-all)
  ids=$(gql '{"query":"query { myself { pods { id } } }"}' \
        | python3 -c "import json,sys;print(' '.join(p['id'] for p in ((json.load(sys.stdin).get('data') or {}).get('myself') or {}).get('pods') or []))")
  for id in $ids; do
    if terminate "$id"; then
      complete_ready_intent_if_matching "$id"
      complete_pending_intent_if_matching "$id"
    fi
  done
  list_pods; exit 0 ;;
experiment)
  # A governed paid experiment: cost cap, enforced lifetime, orphan refusal before
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
  BUDGET_SECS=$(python3 "$SPEND_GUARD" budget \
                  --cost-per-hr "$COST_PER_HR" --cap-usd "$CAP_USD") \
    || die "the cap was refused by the spend guard"
  say "governed experiment"
  say "  cap       \$$CAP_USD at \$$COST_PER_HR/hr"
  say "  provider  observed secure-cloud rate \$$ADVERTISED_COST_PER_HR/hr"
  say "  lifetime  ${BUDGET_SECS}s (the rest of the cap is teardown headroom)"

  # Pre-flight: reconcile live pods against local ownership trails. Orphans from
  # a prior SIGKILL are reported loudly; never create while anything bills.
  say "entry reconcile (before any create)"
  entry_reconcile_or_refuse
  # The child that provisions must bind the pod to this *parent* identity before
  # it returns. That gives the experiment foreground ownership continuously;
  # a child PID that exits after readiness is never treated as a living owner.
  ensure_lease_owner
  ensure_request_id

  # Armed before anything is created.  A governed experiment may only stop the
  # pod it positively identified in its own pending/ready record.  Sweeping an
  # entire account here could kill another host's valid pod; an unknown pod is
  # instead left visible to reconcile until provider idempotency or a durable
  # account coordinator is available.
  terminate_experiment_pod() {
    local id="${1:-${MERC_RUNPOD_POD_ID:-}}"
    [ -n "$id" ] || return 0
    say "stopping governed experiment pod $id"
    terminate "$id"
  }
  trap 'say ""; terminate_experiment_pod' EXIT INT TERM

  # `up --keep` records the identity before readiness so a failed startup gets
  # a receipt with its own clock, cost bound, and teardown verification rather
  # than disappearing behind the parent exit trap. This ignored file is mode
  # 0600 and is removed before every governed experiment to rule out staleness.
  PENDING_ENV="$ROOT/.merc-runpod-pending.env"
  if [ -e "$PENDING_ENV" ]; then rm "$PENDING_ENV"; fi
  if [ -e "$ROOT/.merc-runpod.env" ]; then rm "$ROOT/.merc-runpod.env"; fi

  emit_receipt() {
    local pod_id="$1" ready="$2" teardown_verified="$3" stopped_at="$4" orphans="$5"
    local receipt="evidence/runpod/spend-${pod_id}.json"
    python3 "$SPEND_GUARD" receipt \
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
  # Parent already ran entry_reconcile_or_refuse; child must not re-list or refuse
  # the pod it is about to create under a second gate.
  MERC_RUNPOD_SKIP_ENTRY_RECONCILE=1 MERC_RUNPOD_HOLD_SECS=0 \
    MERC_RUNPOD_PURPOSE=experiment MERC_RUNPOD_PARENT_OWNER=1 \
    MERC_RUNPOD_REQUEST_ID="$REQUEST_ID" \
    MERC_RUNPOD_OWNER_TOKEN_FD=9 \
    MERC_RUNPOD_OWNER_PID="$OWNER_PID" \
    MERC_RUNPOD_OWNER_PROCESS_START="$OWNER_PROCESS_START" \
    MERC_RUNPOD_OWNER_BOOT_ID="$OWNER_BOOT_ID" \
    bash "$0" up --keep 9<<<"$OWNER_TOKEN" || UP_STATUS=$?
  if [ "$UP_STATUS" -ne 0 ]; then
    if [ ! -f "$PENDING_ENV" ]; then
      die "provisioning failed before a pod identity was recorded; reconcile will expose any unknown pod"
    fi
    # shellcheck disable=SC1090
    . "$PENDING_ENV"
    say "provisioning failed; verifying this experiment pod teardown before an inadmissible receipt"
    terminate_experiment_pod "$MERC_RUNPOD_POD_ID"
    TEARDOWN_VERIFIED=false
    [ "$(pod_exists "$MERC_RUNPOD_POD_ID")" = "no" ] && TEARDOWN_VERIFIED=true
    STOPPED_AT=$(date +%s)
    ORPHANS=$(gql '{"query":"query { myself { pods { id } } }"}' \
      | python3 -c "
import json,sys
d=json.load(sys.stdin); m=(d.get('data') or {}).get('myself') or {}
print(','.join(p['id'] for p in (m.get('pods') or [])))" 2>/dev/null)
    emit_receipt "$MERC_RUNPOD_POD_ID" false "$TEARDOWN_VERIFIED" "$STOPPED_AT" "$ORPHANS" || true
    # Only complete the intent when teardown is verified; otherwise leave it bound
    # so the next entry reconcile still classifies the pod as abandoned_intent.
    if [ "$TEARDOWN_VERIFIED" = "true" ]; then
      complete_create_intent
    fi
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
    renew_bound_lease || die "lost experiment ownership lease; exit cleanup will terminate this pod"
    sleep 2
    ELAPSED=$((ELAPSED + 2))
  done
  wait "$CMD_PID" 2>/dev/null || say "  experiment command exited non-zero"
  renew_bound_lease || die "lost experiment ownership lease before teardown"

  say ""
  say "tearing down"
  renew_bound_lease || die "lost experiment ownership lease during teardown"
  terminate "$MERC_RUNPOD_POD_ID"
  renew_bound_lease || true
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
  if [ "$TEARDOWN_VERIFIED" = "true" ]; then
    complete_create_intent
  fi
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
    renew_bound_lease >/dev/null 2>&1 || true
    if terminate "$POD_ID"; then
      complete_create_intent
    fi
  elif [ -n "$POD_ID" ]; then
    say ""
    say "pod LEFT RUNNING at your request under a bounded operator lease:"
    say "  bash scripts/runpod-vllm.sh down $POD_ID"
  fi
}
# Armed BEFORE the pod exists, so an interrupt between creation and the next
# line still tears it down. SIGKILL still bypasses this — intent + entry
# reconcile cover that class of leak.
trap cleanup EXIT INT TERM

# Entry reconcile for casual `up` as well as experiment. Skip only when nested
# under experiment (parent already reconciled) via MERC_RUNPOD_SKIP_ENTRY_RECONCILE.
if [ "${MERC_RUNPOD_SKIP_ENTRY_RECONCILE:-0}" != "1" ]; then
  say "entry reconcile (before any create)"
  entry_reconcile_or_refuse
fi

say "provisioning"
say "  gpu    $GPU_TYPE ($CLOUD)"
say "  image  $IMAGE"
say "  model  $MODEL"

# Intent BEFORE create. If we die between create and bind, reconcile still sees
# an unknown live pod; if we die after bind, it sees abandoned_intent.
write_create_intent "${MERC_RUNPOD_PURPOSE:-up}"

CREATE=$(printf '%s\n' "$VLLM_KEY" | python3 "$ROOT/scripts/runpod-create-payload.py" \
           --api-key-stdin "$GPU_TYPE" "$IMAGE" "$MODEL" "$POD_NAME" "$CLOUD")

# The account lease is refreshed around the provider's 60s-bounded POST. If
# the request somehow crosses its hard TTL, the trap terminates the returned
# pod rather than allowing an ambiguous second creator to proceed.
renew_create_lease
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
renew_create_lease || die "create lease expired after provider response; exit trap will terminate $POD_ID"

# Bind the intent immediately — the next most important line after create.
bind_create_intent "$POD_ID"

# REST creation can select a differently priced machine than the pre-flight
# lowest-price query.  Fail closed and let the armed trap terminate it rather
# than continue a run whose cap conversion is false.
renew_bound_lease
ACTUAL_COST_PER_HR=$(pod_hourly_rate "$POD_ID") || { KEEP=0; die "RunPod did not report the created pod's hourly rate"; }
renew_bound_lease || { KEEP=0; die "lost provisioning lease while reading provider rate"; }
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
  printf 'export MERC_RUNPOD_REQUEST_ID=%q\n' "$REQUEST_ID"
  # Deliberately not exported: the helper that sources this private record
  # forwards the token only through a one-shot file descriptor to the guard.
  printf 'MERC_RUNPOD_OWNER_TOKEN=%q\n' "$OWNER_TOKEN"
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
  renew_bound_lease || die "lost provisioning lease while waiting for readiness"
  code=$(printf 'header = "Authorization: Bearer %s"\n' "$VLLM_KEY" \
         | curl -sS --config - -o /dev/null -w '%{http_code}' --max-time 10 \
             "$ENDPOINT/models" 2>/dev/null || printf '000')
  renew_bound_lease || die "lost provisioning lease after readiness probe"
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
  if [ "${MERC_RUNPOD_PARENT_OWNER:-0}" = "1" ]; then
    if ! renew_bound_lease; then
      KEEP=0
      die "parent ownership lease was lost after readiness"
    fi
    say "ready under the experiment parent lease; the parent will renew and tear it down"
  else
    if ! promote_operator_keep; then
      KEEP=0
      die "could not record bounded operator keep; exit trap will tear down"
    fi
    say "left running for at most ${MERC_RUNPOD_KEEP_SECS:-90}s. Renew explicitly or stop it:"
    say "  bash scripts/runpod-vllm.sh renew-keep"
    say "  bash scripts/runpod-vllm.sh down $POD_ID"
  fi
else
  say ""
  say "press Ctrl-C or wait; this pod tears down when this script exits."
  HOLD_SECONDS="${MERC_RUNPOD_HOLD_SECS:-60}"
  HOLD_ELAPSED=0
  while [ "$HOLD_ELAPSED" -lt "$HOLD_SECONDS" ]; do
    renew_bound_lease || die "lost provisioning lease while holding the pod"
    HOLD_STEP=20
    [ $((HOLD_SECONDS - HOLD_ELAPSED)) -lt "$HOLD_STEP" ] && HOLD_STEP=$((HOLD_SECONDS - HOLD_ELAPSED))
    sleep "$HOLD_STEP"
    HOLD_ELAPSED=$((HOLD_ELAPSED + HOLD_STEP))
  done
fi
