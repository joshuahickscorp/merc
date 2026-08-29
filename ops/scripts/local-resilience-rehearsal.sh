#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/go-closure-common.sh
. "$ROOT/ops/scripts/lib/go-closure-common.sh"

MODE="${1:-}"
[ "$#" -gt 0 ] && shift
DURATION=900
INTERVAL=30
while [ "$#" -gt 0 ]; do
  case "$1" in
    --duration) shift; DURATION="${1:-}" ;;
    --interval) shift; INTERVAL="${1:-}" ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
case "$MODE" in rollback|restart-storm|soak) ;; *)
  echo "usage: ops/scripts/local-resilience-rehearsal.sh rollback|restart-storm|soak [--duration SECONDS] [--interval SECONDS]" >&2
  exit 2
esac
[[ "$DURATION" =~ ^[0-9]+$ ]] && [[ "$INTERVAL" =~ ^[0-9]+$ ]] || {
  echo "duration and interval must be integers" >&2; exit 2;
}
[ "$DURATION" -ge 60 ] && [ "$INTERVAL" -ge 5 ] || {
  echo "duration must be at least 60 seconds and interval at least 5 seconds" >&2; exit 2;
}

gc_reject_live_stripe_environment
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET \
  MERC_CONNECT_WEBHOOK_SECRET MERC_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID

for tool in docker curl jq git openssl cargo python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "$tool is required" >&2; exit 1; }
done

PROJECT="merc-local-resilience"
COMPOSE_FILE="$ROOT/ops/local/compose.rehearsal.yml"
ART="$ROOT/.artifacts/local-resilience"
TOPOLOGY_RECEIPT="$ART/topology.json"
EVIDENCE_DIR="$ROOT/evidence/autonomous"
mkdir -p "$ART" "$EVIDENCE_DIR"
LOCK_DIR="$ROOT/.artifacts/local-resilience.lock"
if ! mkdir "$LOCK_DIR" 2>/dev/null; then
  holder="$(tr -d '[:space:]' < "$LOCK_DIR/pid" 2>/dev/null || true)"
  if [[ "$holder" =~ ^[0-9]+$ ]] && ! kill -0 "$holder" 2>/dev/null; then
    rm -f "$LOCK_DIR/pid"
    rmdir "$LOCK_DIR" 2>/dev/null || {
      echo "stale local resilience lock could not be reclaimed safely" >&2
      exit 1
    }
    mkdir "$LOCK_DIR" 2>/dev/null || {
      echo "another local resilience proof acquired the lock" >&2
      exit 1
    }
  else
    echo "another local resilience proof is active${holder:+ as PID $holder}; refusing to share ports or volumes" >&2
    exit 1
  fi
fi
printf '%s\n' "$$" > "$LOCK_DIR/pid"
SOURCE_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
SOURCE_STATE="$(gc_source_state_sha256 "$ROOT")"
BUILD_EPOCH="$(git -C "$ROOT" show -s --format=%ct "$SOURCE_COMMIT")"
BUILD_DATE="$(date -u -r "$BUILD_EPOCH" +%Y-%m-%dT%H:%M:%SZ)"
HOST_ARCH="$(uname -m)"
case "$HOST_ARCH" in arm64|aarch64) PLATFORM=linux/arm64 ;; x86_64|amd64) PLATFORM=linux/amd64 ;;
  *) echo "unsupported host architecture: $HOST_ARCH" >&2; exit 1 ;;
esac

# Last step before a receipt is left on disk: name the commit and producer.
# Matches the restore/staging producers; do not invent a second shape.
stamp_receipt() {
  python3 - "$ROOT" "$1" "ops/scripts/local-resilience-rehearsal.sh" <<'PY'
import json, sys
from pathlib import Path

root, path, producer = sys.argv[1], sys.argv[2], sys.argv[3]
sys.path.insert(0, str(Path(root) / "ops" / "scripts"))
from lib.receipt_binding import candidate_commit, stamp

p = Path(path)
doc = json.loads(p.read_text(encoding="utf-8"))
stamp(doc, candidate_commit(root), producer)
p.write_text(json.dumps(doc, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
PY
}

AGENT_ONE=""
AGENT_TWO=""
DRIVER_PID=""
compose() { docker compose -p "$PROJECT" -f "$COMPOSE_FILE" "$@"; }
cleanup() {
  code=$?
  [ -z "$DRIVER_PID" ] || kill "$DRIVER_PID" >/dev/null 2>&1 || true
  for pid in "$AGENT_ONE" "$AGENT_TWO"; do
    [ -z "$pid" ] || kill "$pid" >/dev/null 2>&1 || true
  done
  if [[ "$PROJECT" =~ ^merc-local-resilience$ ]]; then
    compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  fi
  rm -f "$LOCK_DIR/pid"
  rmdir "$LOCK_DIR" 2>/dev/null || true
  exit "$code"
}
trap cleanup EXIT INT TERM

if [ -n "${MERC_LOCAL_PREBUILT_IMAGE_ID:-}" ]; then
  [[ "$MERC_LOCAL_PREBUILT_IMAGE_ID" =~ ^sha256:[0-9a-f]{64}$ ]] \
    || { echo "prebuilt local proof image must be an immutable image ID" >&2; exit 1; }
  [ "${MERC_LOCAL_PREBUILT_SOURCE_STATE_SHA256:-}" = "$SOURCE_STATE" ] \
    || { echo "prebuilt local proof image is not bound to the current source state" >&2; exit 1; }
  docker image inspect "$MERC_LOCAL_PREBUILT_IMAGE_ID" >/dev/null
  LOCAL_IMAGE="$MERC_LOCAL_PREBUILT_IMAGE_ID"
else
  LOCAL_TAG="cx-control-local-proof:${SOURCE_COMMIT:0:12}-${SOURCE_STATE:0:12}"
  docker build --platform "$PLATFORM" --provenance=false -f "$ROOT/Dockerfile.control" \
    --build-arg "MERC_BUILD_VERSION=local-proof" \
    --build-arg "MERC_BUILD_COMMIT=$SOURCE_COMMIT" \
    --build-arg "MERC_BUILD_DATE=$BUILD_DATE" -t "$LOCAL_TAG" "$ROOT" >/dev/null
  LOCAL_IMAGE="$(docker image inspect "$LOCAL_TAG" --format '{{.Id}}')"
fi
[[ "$LOCAL_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "local build lacks immutable image ID" >&2; exit 1; }

# Current control refuses to boot without MERC_SETTLEMENT_CURRENCY.
# ops/local/compose.rehearsal.yml does not inject it (prod/smallhost do).
# usd matches the catalogue board's reference currency, so FX identity applies
# and no invented conversion rate is required. The image_id in the receipt is
# this process-configured artifact.
CURRENCY_BASE="cx-control-local-proof:${SOURCE_COMMIT:0:12}-${SOURCE_STATE:0:12}-base"
CURRENCY_TAG="cx-control-local-proof:${SOURCE_COMMIT:0:12}-${SOURCE_STATE:0:12}-currency"
docker tag "$LOCAL_IMAGE" "$CURRENCY_BASE"
printf 'FROM %s\nENV MERC_SETTLEMENT_CURRENCY=usd\n' "$CURRENCY_BASE" | \
  docker build --platform "$PLATFORM" --provenance=false --pull=false -t "$CURRENCY_TAG" - >/dev/null
LOCAL_IMAGE="$(docker image inspect "$CURRENCY_TAG" --format '{{.Id}}')"
[[ "$LOCAL_IMAGE" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "currency-configured local image lacks immutable image ID" >&2; exit 1; }

# Keep fast startup checks, then use a production-shaped post-start cadence for
# soaks. HTTPS readiness is also asserted on every sample and Prometheus
# independently scrapes the control every five seconds.
CONTROL_HEALTH_INTERVAL=5s
[ "$MODE" != soak ] || CONTROL_HEALTH_INTERVAL=30s

# Seatbelt re-exec happens before config load. The deny-default profile only
# reads HOME/DATADIR/MODELCACHE/BINDIR/TMPDIR; the topology config and TLS CA
# live under the artifact dir, so TMPDIR must cover that tree.
export TMPDIR="$ART/topology"
mkdir -p "$TMPDIR"
# Pin the HuggingFace cache to the host tree. Topology start_agent rewrites
# HOME into the artifact dir; without this, hf-hub looks under the sandbox
# home, misses the already-fetched weights, and the egress proxy refuses the
# HuggingFace CDN host.
export HF_HOME="${HF_HOME:-$HOME/.cache/huggingface}"
# The agent passes MERC_MODEL_CACHE to hf-hub Cache::new VERBATIM, and hf-hub
# resolves <root>/models--org--name/. HF_HOME is the parent; the models live in
# $HF_HOME/hub (which is exactly why HUGGINGFACE_HUB_CACHE below appends it).
# Passing the parent made every model miss, the agent fall back to the network,
# the egress proxy refuse the CDN, and the benchmarks come back unavailable — so
# the agent advertised an identity that did not match the sealed cell and
# registration was rejected.
export MERC_MODEL_CACHE="${MERC_MODEL_CACHE:-$HF_HOME/hub}"
export HUGGINGFACE_HUB_CACHE="${HUGGINGFACE_HUB_CACHE:-$HF_HOME/hub}"

KEEP=1 KEEP_AGENTS=1 MERC_LOCAL_SOURCE_PROOF=1 \
  MERC_LOCAL_PROJECT="$PROJECT" MERC_LOCAL_ARTIFACT_DIR="$ART/topology" \
  MERC_LOCAL_EVIDENCE_FILE="$TOPOLOGY_RECEIPT" MERC_LOCAL_CONTROL_IMAGE="$LOCAL_IMAGE" \
  MERC_LOCAL_CONTROL_HEALTHCHECK=/merc-healthcheck \
  MERC_LOCAL_CONTROL_HEALTH_INTERVAL="$CONTROL_HEALTH_INTERVAL" \
  MERC_LOCAL_CONTROL_PLATFORM="$PLATFORM" TMPDIR="$TMPDIR" \
  bash "$ROOT/ops/scripts/local-production-rehearsal.sh"

# The topology setup intentionally leaves these two local sandboxed agent
# processes and its exact runtime environment for the fault exercise.
# shellcheck disable=SC1091
. "$ART/topology/runtime.env"
AGENT_ONE="$(tr -d '[:space:]' < "$ART/topology/agent1.pid")"
AGENT_TWO="$(tr -d '[:space:]' < "$ART/topology/agent2.pid")"
CONTROL_CONTAINER="$(compose ps -q control)"
CONTROL_RESTART_BASE="$(docker inspect --format '{{.RestartCount}}' "$CONTROL_CONTAINER")"
[ "$CONTROL_RESTART_BASE" = 0 ] || {
  echo "control restarted during topology setup; refusing to begin fault/soak assertions" >&2
  exit 1
}
CURL=(curl --noproxy '*' --silent --show-error --fail-with-body \
  --cacert "$ART/topology/tls/ca.crt" --resolve cx.localhost:18443:127.0.0.1)

wait_control() {
  local deadline=$(( $(date +%s) + 240 ))
  until "${CURL[@]}" https://cx.localhost:18443/readyz >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 2
  done
}
wait_service() {
  local service="$1" deadline=$(( $(date +%s) + 240 )) cid state
  while :; do
    cid="$(compose ps -q "$service")"
    state="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
    [ "$state" = healthy ] && return 0
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 2
  done
}
psql_value() {
  compose exec -T postgres psql -X -qAt -U cx -d cx -c "$1"
}
submit_job() {
  local kind="$1" sequence="${2:-single}" response job_id deadline body
  if [ "$kind" = embed ]; then
    body='{"job_type":{"type":"embed","batch_size":8},"model":{"ref":"all-minilm-l6-v2"},"params":{"split_size":1},"constraints":{"min_memory_gb":0,"hw_classes":null,"data_residency":null},"verification":{"redundancy_frac":0,"honeypot_frac":0,"payout_hold_secs":0},"tier":"batch","input":"{\"text\":\"resilience proof\"}\n"}'
  else
    body='{"job_type":{"type":"batch_infer","max_tokens":12,"temperature":0},"model":{"ref":"llama-3.2-1b-instruct-q4"},"params":{"split_size":1},"constraints":{"min_memory_gb":0,"hw_classes":null,"data_residency":null},"verification":{"redundancy_frac":0,"honeypot_frac":0,"payout_hold_secs":0},"tier":"batch","input":"{\"prompt\":\"Reply with only: resilient\"}\n"}'
  fi
  deadline=$(( $(date +%s) + 180 ))
  while :; do
    response="$("${CURL[@]}" https://cx.localhost:18443/v1/jobs \
      -H 'Authorization: Bearer dev-api-key-0001' \
      -H "Idempotency-Key: local-resilience-${MODE}-${kind}-${sequence}" \
      -H 'Content-Type: application/json' -d "$body" 2>/dev/null || true)"
    job_id="$(jq -r '.job_id // empty' <<< "$response" 2>/dev/null || true)"
    [ -n "$job_id" ] && { printf '%s\n' "$job_id"; return 0; }
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 2
  done
}
wait_job() {
  local id="$1" deadline=$(( $(date +%s) + 480 )) status
  while [ "$(date +%s)" -lt "$deadline" ]; do
    status="$("${CURL[@]}" "https://cx.localhost:18443/v1/jobs/$id" \
      -H 'Authorization: Bearer dev-api-key-0001' 2>/dev/null | jq -r '.status // empty' 2>/dev/null || true)"
    [ "$status" = complete ] && return 0
    [ "$status" = failed ] && return 1
    sleep 3
  done
  return 1
}
cancel_job() {
  "${CURL[@]}" -X DELETE "https://cx.localhost:18443/v1/jobs/$1" \
    -H 'Authorization: Bearer dev-api-key-0001' >/dev/null 2>&1 || true
}
semantic_snapshot() {
  psql_value "SELECT json_build_object(
    'jobs',(SELECT count(*) FROM jobs),
    'complete',(SELECT count(*) FROM jobs WHERE status='complete'),
    'cancelled',(SELECT count(*) FROM jobs WHERE status='cancelled'),
    'failed',(SELECT count(*) FROM jobs WHERE status='failed'),
    'retry_count',(SELECT COALESCE(sum(retry_count),0) FROM tasks),
    'terminal_open_tasks',(SELECT count(*) FROM tasks t JOIN jobs j ON j.id=t.job_id WHERE j.status IN ('complete','cancelled','failed') AND t.status NOT IN ('complete','cancelled','failed')),
    'ledger_sum',(SELECT COALESCE(sum(amount_usd),0)::text FROM ledger_entries),
    'duplicate_money',(SELECT count(*) FROM (SELECT task_id,kind,count(*) FROM ledger_entries WHERE task_id IS NOT NULL GROUP BY task_id,kind HAVING count(*)>1) d),
    'webhook_dead_letters',(SELECT count(*) FROM webhooks WHERE dead_lettered_at IS NOT NULL))::text"
}

run_simulator() {
  (cd "$ROOT/src/control" && go run . release stripe-simulate --sequences 4096) > "$ART/payment-simulator.json"
  jq -e '.status == "SIMULATED PASS" and .evidence_label == "SIMULATED" and
    .generated_sequences.count == 4096' "$ART/payment-simulator.json" >/dev/null
}

start_agent() {
  local n="$1"
  local output="$ART/restarted-agent$n.log"
  local model_cache="${MERC_MODEL_CACHE:-${HF_HOME:-$HOME/.cache/huggingface}/hub}"
  HOME="$ART/topology/home" MERC_MODEL_CACHE="$model_cache" \
    MERC_TLS_CA_FILE="$ART/topology/tls/ca.crt" MERC_REQUIRE_SANDBOX=1 \
    TMPDIR="$ART/topology" \
    MERC_SANDBOX_PROFILE="$ROOT/clients/macapp/ComputeExchangeAgent/merc-agent.sb" \
    "$ROOT/.artifacts/local-production-cargo-target/release/merc-agent" run \
    --config "$ART/topology/agent$n/config.toml" > "$output" 2>&1 &
  STARTED_PID=$!
}

if [ "$MODE" = rollback ]; then
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  run_simulator
  first_job="$(submit_job embed before)"; wait_job "$first_job"
  before="$(semantic_snapshot)"

  # PostgreSQL must roll back the entire deliberately interrupted migration.
  if compose exec -T postgres psql -X -v ON_ERROR_STOP=1 -U cx -d cx >/dev/null <<'SQL'
BEGIN;
CREATE TABLE cx_interrupted_migration_probe(id integer);
SELECT 1/0;
COMMIT;
SQL
  then
    echo "interrupted migration unexpectedly committed" >&2; exit 1
  fi
  [ "$(psql_value "SELECT to_regclass('public.cx_interrupted_migration_probe') IS NULL")" = t ]

  control_cid="$(compose ps -q control)"
  docker kill "$control_cid" >/dev/null
  compose start control >/dev/null
  wait_service control; wait_control
  compose restart postgres >/dev/null; wait_service postgres; wait_control
  compose restart minio >/dev/null; wait_service minio; wait_control
  if docker pull 'ghcr.io/joshuahickscorp/computexchange-control@sha256:0000000000000000000000000000000000000000000000000000000000000000' >/dev/null 2>&1; then
    echo "unavailable image injection unexpectedly pulled" >&2; exit 1
  fi

  prior='ghcr.io/joshuahickscorp/computexchange-control@sha256:098edaa7f97892724b9e62a7008f1b3aecae452a9e08c11e60525c05cc6fdacf'
  docker pull "$prior" >/dev/null
  export MERC_LOCAL_CONTROL_IMAGE="$prior" MERC_LOCAL_CONTROL_PLATFORM=linux/amd64 \
    MERC_LOCAL_CONTROL_HEALTHCHECK=/cx
  rollback_started="$(date +%s)"
  compose up -d --no-deps --force-recreate control >/dev/null
  wait_service control; wait_control
  rollback_rto=$(( $(date +%s) - rollback_started ))
  prior_version="$("${CURL[@]}" https://cx.localhost:18443/version)"
  [ "$(jq -r .commit <<< "$prior_version")" = 0387766c5d0e8f9e5b64e8cbef215edcd07784bd ]

  export MERC_LOCAL_CONTROL_IMAGE="$LOCAL_IMAGE" MERC_LOCAL_CONTROL_PLATFORM="$PLATFORM" \
    MERC_LOCAL_CONTROL_HEALTHCHECK=/merc-healthcheck
  forward_started="$(date +%s)"
  compose up -d --no-deps --force-recreate control >/dev/null
  wait_service control; wait_control
  forward_rto=$(( $(date +%s) - forward_started ))
  second_job="$(submit_job embed after)"; wait_job "$second_job"
  [ "$(psql_value "SELECT count(*) FROM jobs WHERE id='$first_job' AND status='complete'")" = 1 ]
  after="$(semantic_snapshot)"
  jq -e '.terminal_open_tasks == 0 and (.ledger_sum|tonumber) > -0.000001 and (.ledger_sum|tonumber) < 0.000001 and .duplicate_money == 0' <<< "$after" >/dev/null
  corrupt_rejected="$(jq -r '.integrity.corrupt_backup_rejected' "$EVIDENCE_DIR/logical-independent-restore.json")"
  [ "$corrupt_rejected" = true ]
  payload="$ART/local-rollback.payload.json"
  jq -n --arg started "$started" --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg source_commit "$SOURCE_COMMIT" --arg source_state "$SOURCE_STATE" \
    --arg local_image "$LOCAL_IMAGE" --arg prior "$prior" --argjson before "$before" \
    --argjson after "$after" --argjson rollback_rto "$rollback_rto" \
    --argjson forward_rto "$forward_rto" '
    {schema_version:1,kind:"local_immutable_rollback",status:"PASS",started_at:$started,finished_at:$finished,
     candidate:{source_commit:$source_commit,dirty_state_sha256:$source_state,image_id:$local_image,workloads_before_and_after:true},
     prior:{image:$prior,smoke:true},recovery:{rollback_rto_seconds:$rollback_rto,forward_rto_seconds:$forward_rto,
       measured_data_loss_jobs:0,before:$before,after:$after},
     injections:{migration_interruption:"PASS",control_death:"PASS",database_restart:"PASS",storage_restart:"PASS",
       unavailable_image:"PASS",corrupt_backup_rejection:"PASS"},
     payments:{deterministic_provider_events:"PASS",label:"SIMULATED",stripe_sandbox:"NOT EXECUTED",stripe_live:"PROHIBITED"},
     limitation:"Published amd64 candidate workload execution is not claimed on this arm64 host; current source was exercised as an immutable native image ID."}' \
    > "$payload"
  # shellcheck source=ops/scripts/lib/write-bound-evidence.sh
  . "$ROOT/ops/scripts/lib/write-bound-evidence.sh"
  merc_emit_bound_json "$EVIDENCE_DIR/local-rollback.json" \
    "ops/scripts/local-resilience-rehearsal.sh" "$payload" \
    --exact-config "local immutable rollback; image_id=$LOCAL_IMAGE" \
    --raw-samples "embedded before/after snapshots" \
    --model-na "rollback rehearsal does not load model weights" \
    --image-na "image_id recorded in receipt body; not a content digest slot" \
    --corpus-na "no external corpus"
  stamp_receipt "$EVIDENCE_DIR/local-rollback.json"
  echo "PASS local rollback and forward recovery"
  exit 0
fi

if [ "$MODE" = restart-storm ]; then
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; seed=20260719
  run_simulator
  jobs_file="$ART/restart-jobs.txt"; : > "$jobs_file"
  workload_driver() {
    local i id kind
    for i in $(seq 1 12); do
      kind='embed'; [ $((i % 5)) -ne 0 ] || kind='batch'
      id="$(submit_job "$kind" "$i")" || continue
      printf '%s\n' "$id" >> "$jobs_file"
      [ $((i % 4)) -ne 0 ] || cancel_job "$id"
      sleep 2
    done
  }
  workload_driver & DRIVER_PID=$!
  fault_order="$(python3 -c 'import random; v=["control","postgres","minio","caddy","prometheus","alertmanager","backup-worker","agent1","agent2"]; random.Random(20260719).shuffle(v); print(" ".join(v))')"
  for fault in $fault_order; do
    case "$fault" in
      agent1) kill "$AGENT_ONE" >/dev/null 2>&1 || true; wait "$AGENT_ONE" 2>/dev/null || true; start_agent 1; AGENT_ONE="$STARTED_PID" ;;
      agent2) kill "$AGENT_TWO" >/dev/null 2>&1 || true; wait "$AGENT_TWO" 2>/dev/null || true; start_agent 2; AGENT_TWO="$STARTED_PID" ;;
      *) compose restart "$fault" >/dev/null; wait_service "$fault" ;;
    esac
    wait_control
  done
  wait "$DRIVER_PID"; DRIVER_PID=""
  while IFS= read -r id; do
    status="$("${CURL[@]}" "https://cx.localhost:18443/v1/jobs/$id" -H 'Authorization: Bearer dev-api-key-0001' | jq -r .status)"
    [ "$status" = cancelled ] || wait_job "$id"
  done < "$jobs_file"
  stale_job="$(submit_job batch stale-recovery)"
  deadline=$(( $(date +%s) + 240 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    [ "$(psql_value "SELECT count(*) FROM tasks WHERE job_id='$stale_job' AND status='running'")" -gt 0 ] && break
    sleep 1
  done
  [ "$(psql_value "SELECT count(*) FROM tasks WHERE job_id='$stale_job' AND status='running'")" -gt 0 ]
  kill "$AGENT_ONE" "$AGENT_TWO" >/dev/null 2>&1 || true
  wait "$AGENT_ONE" 2>/dev/null || true; wait "$AGENT_TWO" 2>/dev/null || true
  psql_value "UPDATE tasks SET claimed_at=now()-interval '31 minutes' WHERE job_id='$stale_job' AND status='running'" >/dev/null
  start_agent 1; AGENT_ONE="$STARTED_PID"
  start_agent 2; AGENT_TWO="$STARTED_PID"
  wait_job "$stale_job"
  [ "$(psql_value "SELECT COALESCE(max(retry_count),0) FROM tasks WHERE job_id='$stale_job'")" -ge 1 ]
  final="$(semantic_snapshot)"
  jq -e '.terminal_open_tasks == 0 and (.ledger_sum|tonumber) > -0.000001 and (.ledger_sum|tonumber) < 0.000001 and .duplicate_money == 0 and .webhook_dead_letters == 0' <<< "$final" >/dev/null
  payload="$ART/local-restart-storm.payload.json"
  jq -n --arg started "$started" --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg source_commit "$SOURCE_COMMIT" --arg source_state "$SOURCE_STATE" \
    --arg image "$LOCAL_IMAGE" --argjson seed "$seed" \
    --arg order "$fault_order" --argjson final "$final" --argjson submitted "$(wc -l < "$jobs_file" | tr -d ' ')" '
    {schema_version:1,kind:"local_restart_storm",status:"PASS",started_at:$started,finished_at:$finished,
     source_commit:$source_commit,dirty_state_sha256:$source_state,
     immutable_local_image_id:$image,random_seed:$seed,fault_order:($order|split(" ")),
     restarted:{control:1,postgresql:1,object_storage:1,reverse_proxy:1,agents:2,monitoring:2,backup_worker:1},
     workload:{submitted:$submitted,types:["embed","batch_infer","cancel","SIMULATED payment","SIMULATED dispute/webhook ordering"]},
     assertions:{terminal_jobs_uncorrupted:($final.terminal_open_tasks==0),no_duplicate_financial_effects:($final.duplicate_money==0),
       ledger_zero_sum:true,stale_lease_recovered:true,agents_reconnected:true,finalization_resumed:true,webhook_delivery_resumed:($final.webhook_dead_letters==0),
       reconciliation_fault_detection:"covered by deterministic simulator"},final_snapshot:$final,
     external:{stripe_sandbox:"NOT EXECUTED",real_receiver:"NOT EXECUTED"}}' \
    > "$payload"
  # shellcheck source=ops/scripts/lib/write-bound-evidence.sh
  . "$ROOT/ops/scripts/lib/write-bound-evidence.sh"
  merc_emit_bound_json "$EVIDENCE_DIR/local-restart-storm.json" \
    "ops/scripts/local-resilience-rehearsal.sh" "$payload" \
    --exact-config "local restart storm seed=$seed" \
    --raw-samples "embedded final_snapshot and fault_order" \
    --model-na "restart-storm rehearsal does not load model weights" \
    --image-na "immutable_local_image_id recorded in receipt body" \
    --corpus-na "no external corpus"
  stamp_receipt "$EVIDENCE_DIR/local-restart-storm.json"
  echo "PASS local restart storm seed=$seed"
  exit 0
fi

started_epoch="$(date +%s)"; end_epoch=$((started_epoch + DURATION))
samples="$ART/soak-samples.jsonl"; : > "$samples"; sequence=0
run_simulator
while [ "$(date +%s)" -lt "$end_epoch" ]; do
  sequence=$((sequence + 1))
  wait_control
  id="$(submit_job embed "embed-$sequence")"; wait_job "$id"
  [ $((sequence % 5)) -ne 0 ] || { infer="$(submit_job batch "batch-$sequence")"; wait_job "$infer"; }
  metrics="$(curl --silent --get http://127.0.0.1:19090/api/v1/query \
    --data-urlencode 'query={__name__=~"merc_process_resident_memory_bytes|merc_process_open_file_descriptors|cx_go_.*|merc_db_pool_connections|merc_queue_age_seconds|merc_webhook_backlog|merc_reconcile_drift_total"}' | jq '.data.result')"
  database="$(psql_value "SELECT json_build_object(
    'connections',(SELECT count(*) FROM pg_stat_activity WHERE datname='cx'),
    'queue_age_seconds',(SELECT COALESCE(max(EXTRACT(EPOCH FROM now()-created_at)),0)::float8 FROM tasks WHERE status IN ('queued','retrying')),
    'retry_count',(SELECT COALESCE(sum(retry_count),0) FROM tasks),
    'webhook_backlog',(SELECT count(*) FROM webhooks WHERE delivered_at IS NULL AND dead_lettered_at IS NULL),
    'failed_finalizations',(SELECT count(*) FROM jobs WHERE status='failed'),
    'reconciliation_mismatches',(SELECT count(*) FROM supplier_payout_operations WHERE status='outcome_unknown' OR outcome_unknown))::text")"
  artifact_bytes="$(compose run --rm --no-deps --entrypoint /bin/sh createbuckets -c \
    'mc alias set local http://minio:9000 "$MINIO_ROOT_USER" "$MINIO_ROOT_PASSWORD" >/dev/null; mc du --json local/cx-jobs | tail -1' 2>/dev/null | jq -r '.size // 0')"
  model_cache_kb="$(du -sk "${MERC_MODEL_CACHE:-${HF_HOME:-$HOME/.cache/huggingface}}" 2>/dev/null | awk '{print $1}' || echo 0)"
  backup_health="$(docker inspect --format '{{.State.Health.Status}}' "$(compose ps -q backup-worker)")"
  control_restarts="$(docker inspect --format '{{.RestartCount}}' "$CONTROL_CONTAINER")"
  control_oom="$(docker inspect --format '{{.State.OOMKilled}}' "$CONTROL_CONTAINER")"
  jq -cn --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg job "$id" \
    --argjson sequence "$sequence" --argjson metrics "$metrics" --argjson database "$database" \
    --argjson artifact_bytes "${artifact_bytes:-0}" --argjson model_cache_kb "${model_cache_kb:-0}" \
    --arg backup "$backup_health" --argjson control_restarts "$control_restarts" \
    --argjson control_oom "$control_oom" '{observed_at:$at,sequence:$sequence,job_id:$job,
      process_and_service_metrics:$metrics,database:$database,artifact_bytes:$artifact_bytes,
      model_cache_kb:$model_cache_kb,backup_status:$backup,
      control_restart_count:$control_restarts,control_oom_killed:$control_oom}' >> "$samples"
  now="$(date +%s)"; [ "$now" -lt "$end_epoch" ] || break
  sleep_for="$INTERVAL"; [ $((end_epoch - now)) -ge "$sleep_for" ] || sleep_for=$((end_epoch - now))
  sleep "$sleep_for"
done
finished_epoch="$(date +%s)"
actual=$((finished_epoch - started_epoch))
[ "$actual" -ge "$DURATION" ]
final="$(semantic_snapshot)"
jq -e '.terminal_open_tasks == 0 and .duplicate_money == 0 and (.ledger_sum|tonumber) > -0.000001 and (.ledger_sum|tonumber) < 0.000001' <<< "$final" >/dev/null
summary="$(jq -s '
  def metric_values($name):
    [.[] | .process_and_service_metrics[]? |
      select(.metric.__name__ == $name) | .value[1] | tonumber];
  def bounds($values):
    {first:$values[0],last:$values[-1],min:($values|min),max:($values|max),
     delta:($values[-1]-$values[0])};
  . as $samples |
  (metric_values("merc_process_resident_memory_bytes")) as $rss |
  (metric_values("merc_process_open_file_descriptors")) as $fds |
  (metric_values("merc_go_heap_alloc_bytes")) as $heap |
  (metric_values("merc_go_sys_bytes")) as $go_sys |
  (metric_values("merc_go_memory_limit_bytes")) as $go_limit |
  (metric_values("merc_go_goroutines")) as $goroutines |
  (metric_values("merc_go_gc_cycles_total")) as $gc_cycles |
  {sample_count:length,
   rss_bytes:bounds($rss),
   open_file_descriptors:bounds($fds),
   go_heap_alloc_bytes:bounds($heap),
   go_sys_bytes:bounds($go_sys),
   go_memory_limit_bytes:{min:($go_limit|min),max:($go_limit|max)},
   go_goroutines:bounds($goroutines),
   go_gc_cycles_total:bounds($gc_cycles),
   database_connections:{max:([$samples[].database.connections]|max)},
   queue_age_seconds:{max:([$samples[].database.queue_age_seconds]|max)},
   retry_count:{max:([$samples[].database.retry_count]|max)},
   artifact_bytes:bounds([$samples[].artifact_bytes]),
   model_cache_kb:bounds([$samples[].model_cache_kb]),
   webhook_backlog:{max:([$samples[].database.webhook_backlog]|max)},
   failed_finalizations:{max:([$samples[].database.failed_finalizations]|max)},
   reconciliation_mismatches:{max:([$samples[].database.reconciliation_mismatches]|max)},
   backup_unhealthy_samples:([$samples[]|select(.backup_status != "healthy")]|length),
   control_restart_count:{max:([$samples[].control_restart_count]|max)},
   control_oom_samples:([$samples[]|select(.control_oom_killed == true)]|length)}
' "$samples")"
jq -e '.sample_count > 0 and .backup_unhealthy_samples == 0 and
  .webhook_backlog.max == 0 and .failed_finalizations.max == 0 and
  .reconciliation_mismatches.max == 0 and .control_restart_count.max == 0 and
  .control_oom_samples == 0 and .rss_bytes.max < 536870912 and
  .go_heap_alloc_bytes.max < 268435456 and
  .go_heap_alloc_bytes.max < .go_memory_limit_bytes.min' <<< "$summary" >/dev/null
qualifies=false; [ "$actual" -ge 86400 ] && qualifies=true
receipt="$EVIDENCE_DIR/local-soak-${DURATION}s.json"
payload="$ART/local-soak.payload.json"
samples_sha="$(shasum -a 256 "$samples" | awk '{print $1}')"
jq -n --arg started "$(date -u -r "$started_epoch" +%Y-%m-%dT%H:%M:%SZ)" \
  --arg finished "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg source_commit "$SOURCE_COMMIT" \
  --arg source_state "$SOURCE_STATE" \
  --arg image "$LOCAL_IMAGE" --arg samples_sha "$samples_sha" \
  --arg docker_health_interval "$CONTROL_HEALTH_INTERVAL" \
  --argjson requested "$DURATION" --argjson actual "$actual" --argjson interval "$INTERVAL" \
  --argjson count "$sequence" --argjson qualifies "$qualifies" --argjson final "$final" \
  --argjson summary "$summary" '
  {schema_version:1,kind:"local_resilience_soak",status:"PASS",started_at:$started,finished_at:$finished,
   source_commit:$source_commit,dirty_state_sha256:$source_state,immutable_local_image_id:$image,
   duration:{requested_seconds:$requested,actual_seconds:$actual,interval_seconds:$interval,samples:$count},
   health_monitoring:{docker_control_interval:$docker_health_interval,
     startup_interval:"2s",https_readiness_each_sample:true,prometheus_scrape_interval:"5s"},
   tracked:["RSS","file descriptors","Go heap/runtime/goroutines/GC","database connections","queue age","retry count","artifact growth",
     "model-cache growth","webhook backlog","failed finalizations","reconciliation","backup status",
     "control restarts","control OOM kills"],
   samples:{sha256:$samples_sha,retained_in_ignored_artifact_directory:true},observed_bounds:$summary,
   assertions:{backup_healthy:($summary.backup_unhealthy_samples==0),
     control_never_restarted:($summary.control_restart_count.max==0),
     control_never_oom_killed:($summary.control_oom_samples==0),
     bounded_rss:($summary.rss_bytes.max < 536870912),
     bounded_go_heap:($summary.go_heap_alloc_bytes.max < 268435456 and
       $summary.go_heap_alloc_bytes.max < $summary.go_memory_limit_bytes.min),
     webhook_backlog_zero:($summary.webhook_backlog.max==0),
     failed_finalizations_zero:($summary.failed_finalizations.max==0),
     reconciliation_mismatches_zero:($summary.reconciliation_mismatches.max==0)},
   final_snapshot:$final,
   qualification:{qualifies_for_24h_gate:$qualifies,status:(if $qualifies then "PASS" else "NOT EXECUTED" end)},
   external:{stripe_sandbox:"NOT EXECUTED",live_money:"PROHIBITED"}}' > "$payload"
# shellcheck source=ops/scripts/lib/write-bound-evidence.sh
. "$ROOT/ops/scripts/lib/write-bound-evidence.sh"
merc_emit_bound_json "$receipt" "ops/scripts/local-resilience-rehearsal.sh" "$payload" \
  --exact-config "local soak duration=${DURATION}s interval=${INTERVAL}s" \
  --raw-samples "samples sha256=$samples_sha (artifact dir)" \
  --model-na "soak rehearsal does not load model weights" \
  --image-na "immutable_local_image_id recorded in receipt body" \
  --corpus-na "no external corpus"
stamp_receipt "$receipt"
echo "PASS local soak ${actual}s; 24-hour qualification=$qualifies"
