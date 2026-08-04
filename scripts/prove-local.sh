#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ART="${MERC_PROOF_ARTIFACT_DIR:-$ROOT/.artifacts/prove-local}"
PGDATA="$ART/pgdata"
LEDGER="$ART/ledger.jsonl"
PGPORT="${PGPORT:-55432}"
MINIO_PORT="${MINIO_PORT:-59000}"
CONTROL_PORT="${CONTROL_PORT:-18080}"
CONTROL_URL="http://127.0.0.1:$CONTROL_PORT"
KEEP="${KEEP:-0}"
SKIP_LIVE="${SKIP_LIVE:-0}"
USE_DOCKER="${USE_DOCKER:-0}"
PROVE_MEDIA="${MERC_PROVE_MEDIA:-0}"
PROVE_RENDERING="${MERC_PROVE_RENDERING:-0}"
MERC_PROOF_COMPOSE_PROJECT=""
TEST_DATABASE_URL=""
RACE_DATABASE_URL=""
CONTROL_PID=""
AGENT_PID=""
AGENT2_PID=""
MINIO_PID=""
PG_STARTED=0
SANDBOX_ROOT=""

export DATABASE_URL="postgres://cx@127.0.0.1:$PGPORT/cx?sslmode=disable"
export MERC_ENV="development"
export S3_ENDPOINT="http://127.0.0.1:$MINIO_PORT"
export S3_PUBLIC_ENDPOINT="$S3_ENDPOINT"
export S3_BUCKET="cx-jobs"
export S3_ACCESS_KEY="minioadmin"
export S3_SECRET_KEY="minioadmin"
export S3_REGION="us-east-1"
export LISTEN_ADDR="127.0.0.1:$CONTROL_PORT"
# The control plane refuses to start without an explicit settlement currency.
# This proof is hermetic and shadow-ledgered: its economic inputs below are all
# USD-denominated and the reference board is USD, so USD settles under identity
# FX and needs no operator-supplied rate. Staging and canary run CAD, which
# additionally requires a bound FX revision; override to exercise that path.
export MERC_SETTLEMENT_CURRENCY="${MERC_SETTLEMENT_CURRENCY:-usd}"
export MERC_ECON_SCHEDULE_VERSION="prove-v1"
export MERC_PROCESSOR_PERCENT_BPS="290"
export MERC_PROCESSOR_FIXED_USD="0.30"
export MERC_CONTROL_PLANE_PER_BATCH_USD="0.0001"
export MERC_MIN_CONTRIBUTION_PER_BATCH_USD="0.000001"
export MERC_TARGET_MARGIN_BPS="1000"
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$ROOT/.artifacts/prove-cache/cargo-target}"
export GOCACHE="${GOCACHE:-$ROOT/.artifacts/prove-cache/go-cache}"

DEV_ADMIN_AUTH='Authorization: Bearer dev-admin-key-0001' # gitleaks:allow -- seeded local proof credential
DEV_BUYER_AUTH='Authorization: Bearer dev-api-key-0001'   # gitleaks:allow -- seeded local proof credential

# A proof must be hermetic even when the caller has sourced a production or
# Stripe-enabled .env.  The live processor boundary is exercised by the Go
# webhook/financial model tests; this local E2E intentionally runs the shadow
# ledger and must never inherit credentials or contact Stripe.
unset STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET MERC_CONNECT_WEBHOOK_SECRET

record() {
  jq -nc --arg status "$1" --arg gate "$2" --arg detail "$3" \
    --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{ts:$ts,status:$status,gate:$gate,detail:$detail}' >>"$LEDGER"
  printf '%s %-24s %s\n' "$1" "$2" "$3"
}

cleanup() {
  code=$?
  for pid in "$AGENT_PID" "$AGENT2_PID" "$CONTROL_PID" "$MINIO_PID"; do
    [ -z "$pid" ] || kill "$pid" 2>/dev/null || true
  done
  if [ "$KEEP" != "1" ]; then
    if [ "$USE_DOCKER" = "1" ]; then
      if [[ "$MERC_PROOF_COMPOSE_PROJECT" =~ ^merc-proof-[0-9]+-[0-9]+$ ]]; then
        docker compose down --volumes --remove-orphans >/dev/null 2>&1 || true
      else
        echo "refusing to remove proof volumes for unexpected Compose project" >&2
      fi
    elif [ "$PG_STARTED" = "1" ]; then
      pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
    fi
    [ -z "$SANDBOX_ROOT" ] || rm -rf "$SANDBOX_ROOT"
  fi
  exit "$code"
}
trap cleanup EXIT INT TERM

wait_for() {
  deadline=$(( $(date +%s) + $1 ))
  shift
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 1
  done
}

workers_ready() {
  expected="$1"
  count="$(curl -fsS "$CONTROL_URL/admin/workers" \
    -H "$DEV_ADMIN_AUTH" | \
    jq '[.[] | select(
      (.id == "00000000-0000-4000-8000-0000000000b1" or
       .id == "00000000-0000-4000-8000-0000000000b2") and
      .version != "seed" and .last_seen_at != null
    )] | length')"
  [ "$count" -ge "$expected" ]
}

port_available() {
  python3 - "$1" <<'PY'
import socket
import sys

port = int(sys.argv[1])
with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 0)
    listener.bind(("127.0.0.1", port))
PY
}

for tool in go cargo psql curl jq openssl git node python3; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done
if [ "$PROVE_MEDIA" = "1" ]; then
  for tool in ffmpeg ffprobe base64; do
    command -v "$tool" >/dev/null || { echo "MERC_PROVE_MEDIA=1 requires $tool" >&2; exit 1; }
  done
fi
case "$PROVE_MEDIA" in
  0|1) ;;
  *) echo "MERC_PROVE_MEDIA must be 0 or 1" >&2; exit 1 ;;
esac
case "$PROVE_RENDERING" in
  0|1) ;;
  *) echo "MERC_PROVE_RENDERING must be 0 or 1" >&2; exit 1 ;;
esac
for port in "$PGPORT" "$MINIO_PORT" "$CONTROL_PORT"; do
  port_available "$port" || {
    echo "proof port $port is already occupied; refusing split-stack or cross-run reuse" >&2
    exit 1
  }
done

rm -rf "$ART"
mkdir -p "$ART"
: >"$LEDGER"

(cd control && go build -o "$ART/merc" .)
"$ART/merc" audit codebase --out census
SOURCE_START="$("$ART/merc" source-id --root "$ROOT" --field source_sha256)"
record PASS source-bound "source_sha256=$SOURCE_START"
record PASS census "authoritative census regenerated before source binding"

# Dependencies come up BEFORE the unit gates. S3_ENDPOINT is exported at the
# top of this script, so the object-storage tests in `go test ./...` connect
# to it for real; running them first meant they dialled a port nothing was
# listening on yet and failed the whole proof before it started.
if [ "$USE_DOCKER" = "1" ]; then
  # A proof must never inherit jobs, workers, object bytes, or idempotency keys
  # from a prior run. The unique project owns disposable volumes that cleanup
  # can remove without touching the developer's ordinary Compose project.
  MERC_PROOF_COMPOSE_PROJECT="merc-proof-$$-$(date +%s)"
  export COMPOSE_PROJECT_NAME="$MERC_PROOF_COMPOSE_PROJECT"
  # The ordinary Compose file exposes postgres/minio on their conventional
  # host ports, which may already belong to a developer stack. Bind this
  # proof's containers to the ports checked above and keep the override in the
  # proof artifact directory so teardown uses the identical isolated project.
  COMPOSE_OVERRIDE="$ART/docker-compose.proof.yml"
  cat >"$COMPOSE_OVERRIDE" <<EOF
services:
  postgres:
    ports:
      - "${PGPORT}:5432"
  minio:
    ports:
      - "${MINIO_PORT}:9000"
EOF
  export COMPOSE_FILE="$ROOT/docker-compose.yml:$COMPOSE_OVERRIDE"
  docker compose up -d postgres minio createbuckets
  export DATABASE_URL="postgres://cx:cx@127.0.0.1:$PGPORT/cx?sslmode=disable"
  TEST_DATABASE_URL="postgres://cx:cx@127.0.0.1:$PGPORT/cx_prove_tests?sslmode=disable"
  export S3_ENDPOINT="http://127.0.0.1:$MINIO_PORT"
  export S3_PUBLIC_ENDPOINT="$S3_ENDPOINT"
  wait_for 60 psql "$DATABASE_URL" -c 'select 1'
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c 'CREATE DATABASE cx_prove_tests' >/dev/null
else
  for tool in initdb pg_ctl createdb minio mc; do
    command -v "$tool" >/dev/null || { echo "missing native dependency: $tool" >&2; exit 1; }
  done
  initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$ART/postgres.log"
  # The cluster is initdb'd with --locale=C, so start the postmaster in the same
  # locale. Inheriting an unset or invalid one makes PostgreSQL 17 on macOS die
  # with "postmaster became multithreaded during startup", which reads like a
  # crash but is only the caller's environment leaking into a proof that is
  # supposed to be hermetic.
  LC_ALL=C LANG=C pg_ctl -D "$PGDATA" -o "-p $PGPORT -c listen_addresses=127.0.0.1" -l "$ART/postgres.log" -w start
  PG_STARTED=1
  createdb -h 127.0.0.1 -p "$PGPORT" -U cx cx
  createdb -h 127.0.0.1 -p "$PGPORT" -U cx cx_prove_tests
  TEST_DATABASE_URL="postgres://cx@127.0.0.1:$PGPORT/cx_prove_tests?sslmode=disable"
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    minio server "$ART/minio" --address "127.0.0.1:$MINIO_PORT" >"$ART/minio.log" 2>&1 &
  MINIO_PID=$!
  wait_for 30 curl -fsS "$S3_ENDPOINT/minio/health/live"
  mc alias set cx-local "$S3_ENDPOINT" minioadmin minioadmin >/dev/null
  mc mb --ignore-existing cx-local/cx-jobs >/dev/null
fi
record PASS dependencies "PostgreSQL and MinIO are healthy"

# Full Go and race tests each get a separate disposable database in the proof's
# own fresh PostgreSQL cluster. Never inherit MERC_TEST_DATABASE_URL from the
# caller: doing so made a nominally hermetic proof query an unrelated
# developer/CI database, or fail outright when the variable was absent. The
# second database also prevents normal-suite fixtures from becoming race-suite
# state (for example, a fixed certificate used to test uniqueness).
psql "$TEST_DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
export MERC_TEST_DATABASE_URL="$TEST_DATABASE_URL"

test -z "$(gofmt -l control)"
# JSON emits package lifecycle events while retaining the test command's normal
# semantics.  The closure proof monitor consumes those events for progress
# heartbeats; a quiet race run is not by itself a hang.
(cd control && go vet ./... && go test -json ./... | tee "$ART/go-test.json")
if [ "$USE_DOCKER" = "1" ]; then
  psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -c 'CREATE DATABASE cx_prove_race' >/dev/null
  RACE_DATABASE_URL="postgres://cx:cx@127.0.0.1:$PGPORT/cx_prove_race?sslmode=disable"
else
  createdb --maintenance-db="$DATABASE_URL" cx_prove_race
  RACE_DATABASE_URL="postgres://cx@127.0.0.1:$PGPORT/cx_prove_race?sslmode=disable"
fi
psql "$RACE_DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
(cd control && MERC_TEST_DATABASE_URL="$RACE_DATABASE_URL" go test -json -race ./... | tee "$ART/go-race.json")
(cd agent && cargo fmt --all -- --check && cargo clippy --all-targets --no-default-features -- -D warnings && cargo test --no-default-features)
bash scripts/verify-python-sdk-package.sh
node scripts/site-build.mjs
record PASS local-gates "format, vet, race, Rust, SDK, and site gates passed"

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
(cd control && MERC_TEST_DATABASE_URL="$TEST_DATABASE_URL" go test ./... -run '^(TestBillingCustomerCanonicalSchema|TestListWorkersToleratesLegacyNullTelemetry|TestRuntimeCatalogPriceIsStableAcrossMigration|TestPrivilegedAdminMutationsHaveCompleteAtomicAudit|TestPrivilegedMutationIdempotentConcurrentReplay|TestConcurrentNamedOperatorsRetainIndependentAttribution|TestRevocationWinsRaceBeforePrivilegedMutation|TestAdminMutationRollsBackWhenAuditInsertFails|TestOperationalControlsAreDurableAndActorAudited|TestOperationalControlMutationFailsClosed|TestDisputeFilingAtomicallyFreezesAndTerminalResolutionControlsPayout|TestDisputeFilingOwnershipTerminalReasonAndWindowBoundaries|TestDisputeAPIUsesAuthenticatedOwnerAndStrictBoundedReason|TestConcurrentDisputeFilingsCreateOnlyOneActiveCase|TestDisputeFilingWinsQueuedPayoutClaimRace|TestDSARDeletionTombstoneAndRestoreReplay|TestSupportAndSecurityTechnicalTabletops)$' -count=1)
record PASS schema "canonical schema applies twice"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
DO $$
DECLARE probe UUID := gen_random_uuid(); rejected BOOLEAN := false;
BEGIN
  INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref)
  VALUES (probe,gen_random_uuid(),'queued','embed','all-minilm-l6-v2','proof/input');
  UPDATE jobs SET status='running' WHERE id=probe;
  BEGIN
    UPDATE jobs SET status='queued' WHERE id=probe;
  EXCEPTION WHEN OTHERS THEN
    rejected := position('illegal job lifecycle transition' in SQLERRM) > 0;
  END;
  IF NOT rejected THEN RAISE EXCEPTION 'illegal lifecycle transition was accepted'; END IF;
  DELETE FROM jobs WHERE id=probe;
END $$;
SQL
record PASS lifecycle "central reducer accepted a legal transition and rejected regression"

SAMPLE_SECRET="$(openssl rand -hex 32)"
TOKEN_KEY="$(openssl rand -hex 32)"
MERC_VERIFICATION_SAMPLE_SECRET="$SAMPLE_SECRET" MERC_TOKEN_KEY="$TOKEN_KEY" "$ART/merc" seed >"$ART/seed.log"
MERC_VERIFICATION_SAMPLE_SECRET="$SAMPLE_SECRET" MERC_TOKEN_KEY="$TOKEN_KEY" "$ART/merc" >"$ART/control.log" 2>&1 &
CONTROL_PID=$!
wait_for 30 curl -fsS "$CONTROL_URL/readyz"
record PASS control "ready endpoint passed"

if [ "$SKIP_LIVE" != "1" ]; then
  cargo build --release --manifest-path agent/Cargo.toml
  SANDBOX_ROOT="$(mktemp -d /private/tmp/cx-prove.XXXXXX)"
  mkdir -p "$SANDBOX_ROOT/home/.merc"
  cp "$CARGO_TARGET_DIR/release/merc-agent" "$SANDBOX_ROOT/merc-agent"
  cp clients/macapp/ComputeExchangeAgent/merc-agent.sb "$SANDBOX_ROOT/merc-agent.sb"
  AGENT_BIN="$SANDBOX_ROOT/merc-agent"
  MODEL_CACHE="${MERC_MODEL_CACHE:-${HF_HOME:-$HOME/.cache/huggingface}}"

  # The batch_infer submit below asks for honeypot_frac 0.5, and createJob
  # refuses (503) rather than admit a job whose only correctness check would be
  # a record count. `merc seed` deliberately will not write a known answer it
  # has not measured, because a probe no honest worker can pass quarantines the
  # supplier that runs it. Measure one against the binary that is about to
  # execute it -- byte-exact honeypots are bound to engine|build_hash.
  MERC_AGENT_BIN="$AGENT_BIN" MERC_MODEL_CACHE="$MODEL_CACHE" \
    DATABASE_URL="$DATABASE_URL" \
    scripts/seed-batch-infer-honeypot.sh >"$ART/batch-infer-honeypot.json" 2>"$ART/batch-infer-honeypot.log"
  jq -e '.status=="PASS" and (.known_answer_utf8|startswith("{\"job_type\":\"batch_infer\","))' \
    "$ART/batch-infer-honeypot.json" >/dev/null \
    || { echo "batch_infer honeypot measurement failed" >&2; tail -5 "$ART/batch-infer-honeypot.log" >&2; exit 1; }
  record PASS honeypot "batch_infer known answer measured against this agent build"
  for n in 1 2; do
    token="dev-worker-token-000$n"
    supplier="00000000-0000-0000-0000-0000000000a$n"
    mkdir -p "$SANDBOX_ROOT/home/.merc/agent$n"
    {
      printf 'control_url = "%s"\n' "$CONTROL_URL"
      printf 'worker_token = "%s"\n' "$token"
      printf 'supplier_id = "%s"\n' "$supplier"
      printf 'max_cpu_pct = 100.0\npower_only = false\nmin_payout_usd_per_hr = 0.0\n'
      printf 'memory_headroom_gb = 0.0\nmax_memory_pct = 0.0\n'
      printf 'data_dir = "%s"\n' "$SANDBOX_ROOT/home/.merc/agent$n"
    } >"$SANDBOX_ROOT/agent$n.toml"
  done

  HOME="$SANDBOX_ROOT/home" MERC_MODEL_CACHE="$MODEL_CACHE" \
    MERC_SANDBOX_PROFILE="$SANDBOX_ROOT/merc-agent.sb" MERC_REQUIRE_SANDBOX=1 \
    MERC_CONTROL_URL="$CONTROL_URL" MERC_WORKER_TOKEN=dev-worker-token-0001 \
    "$AGENT_BIN" run --config "$SANDBOX_ROOT/agent1.toml" >"$ART/agent1.log" 2>&1 &
  AGENT_PID=$!
  wait_for 300 workers_ready 1

  HOME="$SANDBOX_ROOT/home" MERC_MODEL_CACHE="$MODEL_CACHE" \
    MERC_SANDBOX_PROFILE="$SANDBOX_ROOT/merc-agent.sb" MERC_REQUIRE_SANDBOX=1 \
    MERC_CONTROL_URL="$CONTROL_URL" MERC_WORKER_TOKEN=dev-worker-token-0002 \
    "$AGENT_BIN" run --config "$SANDBOX_ROOT/agent2.toml" >"$ART/agent2.log" 2>&1 &
  AGENT2_PID=$!
  wait_for 300 workers_ready 2
  record PASS two-agents "two distinct supplier agents registered"

  submit() {
    body="$(jq -nc --arg model "$1" --argjson job_type "$2" --arg input "$3" \
      '{job_type:$job_type,model:{ref:$model},params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:1.0,honeypot_frac:0.5,payout_hold_secs:0},
        tier:"batch",input:$input}')"
    response="$(curl -sS "$CONTROL_URL/v1/jobs" -H "$DEV_BUYER_AUTH" \
      -H "Idempotency-Key: proof-$1" \
      -H 'Content-Type: application/json' -d "$body")"
    job_id="$(jq -r '.job_id // empty' <<<"$response")"
    [ -n "$job_id" ] || { printf 'job submission failed: %s\n' "$response" >&2; return 1; }
    printf '%s\n' "$job_id"
  }
  wait_job() {
    deadline=$(( $(date +%s) + 420 ))
    while [ "$(date +%s)" -lt "$deadline" ]; do
      state="$(curl -fsS "$CONTROL_URL/v1/jobs/$1" -H "$DEV_BUYER_AUTH" | jq -r .status)"
      [ "$state" = complete ] && return 0
      [ "$state" = failed ] || [ "$state" = cancelled ] && return 1
      sleep 3
    done
    return 1
  }

  submit_media() {
    local input_file="$1" body response quote_id job_id
    body="$(jq -nc --arg b64 "$MEDIA_B64" \
      '{job_type:{type:"media_transcode",input_format:"mp4",max_width:320,max_height:180,fps:30,video_bitrate_kbps:400},
        model:{ref:"ffmpeg-transcode-v1"},params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:1.0,honeypot_frac:0,payout_hold_secs:0},
        tier:"batch",input:{base64:$b64}}')"
    response="$(curl -sS "$CONTROL_URL/v1/quote" -H "$DEV_BUYER_AUTH" \
      -H 'Content-Type: application/json' -d "$body")"
    quote_id="$(jq -r '.quote_id // empty' <<<"$response")"
    [ -n "$quote_id" ] || { printf 'media quote failed: %s\n' "$response" >&2; return 1; }
    body="$(jq --arg quote "$quote_id" '. + {quote_id:$quote,firm_quote:true,max_usd:1.0}' <<<"$body")"
    response="$(curl -sS "$CONTROL_URL/v1/jobs" -H "$DEV_BUYER_AUTH" \
      -H 'Idempotency-Key: proof-media-transcode' \
      -H 'Content-Type: application/json' -d "$body")"
    job_id="$(jq -r '.job_id // empty' <<<"$response")"
    [ -n "$job_id" ] || { printf 'media submission failed: %s\n' "$response" >&2; return 1; }
    printf '%s\n' "$job_id"
  }
  submit_rendering() {
    local scene body response quote_id job_id
    scene='{"background":[8,16,32],"rectangles":[{"x":4,"y":4,"width":24,"height":24,"color":[220,80,40]}]}'
    body="$(jq -nc --arg input "$scene" \
      '{job_type:{type:"media_rendering",render_width:64,render_height:64},
        model:{ref:"svg-scene-render-v1"},params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:1.0,honeypot_frac:0,payout_hold_secs:0},
        tier:"batch",input:$input}')"
    response="$(curl -sS "$CONTROL_URL/v1/quote" -H "$DEV_BUYER_AUTH" \
      -H 'Content-Type: application/json' -d "$body")"
    quote_id="$(jq -r '.quote_id // empty' <<<"$response")"
    [ -n "$quote_id" ] || { printf 'rendering quote failed: %s\n' "$response" >&2; return 1; }
    # The physical rendering unit is the declared output pixel geometry. Keep
    # the buyer-visible quote and fail the live proof if a future refactor
    # drifts back to charging scene-document bytes.
    printf '%s\n' "$response" > "$ART/render-quote.json"
    jq -e '
      .pricing_decision.billable_units == 4096 and
      .compute_plan.settlement_input_units == 4096 and
      .pricing_decision.fixed_point.currency == "cad"
    ' "$ART/render-quote.json" >/dev/null || {
      printf 'rendering quote did not bind CAD pixel geometry: %s\n' "$response" >&2
      return 1
    }
    body="$(jq --arg quote "$quote_id" '. + {quote_id:$quote,firm_quote:true,max_usd:1.0}' <<<"$body")"
    response="$(curl -sS "$CONTROL_URL/v1/jobs" -H "$DEV_BUYER_AUTH" \
      -H 'Idempotency-Key: proof-media-rendering' \
      -H 'Content-Type: application/json' -d "$body")"
    job_id="$(jq -r '.job_id // empty' <<<"$response")"
    [ -n "$job_id" ] || { printf 'rendering submission failed: %s\n' "$response" >&2; return 1; }
    printf '%s\n' "$job_id"
  }

  EMBED_JOB="$(submit all-minilm-l6-v2 '{"type":"embed","batch_size":8}' $'{"text":"apple silicon"}\n{"text":"compute market"}\n')"
  INFER_JOB="$(submit llama-3.2-1b-instruct-q4 '{"type":"batch_infer","max_tokens":12,"temperature":0}' $'{"prompt":"Reply with only: ping"}\n')"
	if [ "$PROVE_MEDIA" = "1" ]; then
	  # The media path is a real binary caller, not a JSONL-shaped test. Generate a
	  # tiny deterministic MP4 locally, quote that exact base64 commitment, and let
	  # both enrolled agents run the bounded FFmpeg/ffprobe contract.
	  ffmpeg -hide_banner -loglevel error -f lavfi -i color=c=blue:s=320x180:r=24:d=0.25 \
	    -an -c:v libx264 -threads 1 -pix_fmt yuv420p -metadata comment=merc-proof \
	    -y "$ART/media-input.mp4"
	  MEDIA_B64="$(base64 < "$ART/media-input.mp4" | tr -d '\n')"
	  MEDIA_JOB="$(submit_media "$ART/media-input.mp4")"
	fi
	if [ "$PROVE_RENDERING" = "1" ]; then
	  RENDER_JOB="$(submit_rendering)"
	fi
	EMBED_REPLAY="$(submit all-minilm-l6-v2 '{"type":"embed","batch_size":8}' $'{"text":"apple silicon"}\n{"text":"compute market"}\n')"
	[ "$EMBED_REPLAY" = "$EMBED_JOB" ]
	if submit all-minilm-l6-v2 '{"type":"embed","batch_size":8}' $'{"text":"different request"}\n' >/dev/null 2>&1; then
	  echo "idempotency conflict was accepted" >&2
	  exit 1
	fi
	record PASS idempotency "identical submit replay returned one job; conflicting reuse was rejected"
  wait_job "$EMBED_JOB"
  wait_job "$INFER_JOB"
  record PASS customer-path "embed and batch_infer completed through live Candle agents"
	if [ "$PROVE_MEDIA" = "1" ]; then
	  wait_job "$MEDIA_JOB"
	  MEDIA_RESULTS="$(curl -fsS "$CONTROL_URL/v1/jobs/$MEDIA_JOB/results" -H "$DEV_BUYER_AUTH")"
	  MEDIA_RESULTS_URL="$(jq -r '.results_url // empty' <<<"$MEDIA_RESULTS")"
	  [ -n "$MEDIA_RESULTS_URL" ] || { echo "media result did not expose a merged results_url" >&2; exit 1; }
	  curl -fsS "$MEDIA_RESULTS_URL" -o "$ART/media-output.mp4"
	  ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height \
	    -of json "$ART/media-output.mp4" >"$ART/media-output-probe.json"
	  jq -e '.streams|length==1 and .[0].codec_name=="h264" and .[0].width==320 and .[0].height==180' \
	    "$ART/media-output-probe.json" >/dev/null
	  MEDIA_DB="$(psql "$DATABASE_URL" -Atc "select j.job_type || '|' || j.status || '|' || count(*) filter (where not t.is_redundancy and not t.is_honeypot) || '|' || count(*) from jobs j join tasks t on t.job_id=j.id where j.id='$MEDIA_JOB' group by j.job_type,j.status")"
	  [ "$MEDIA_DB" = "media_transcode|complete|1|2" ] || {
	    echo "media database receipt shape was not one primary plus one independent result: $MEDIA_DB" >&2
	    exit 1
	  }
	  MEDIA_MS="$(psql "$DATABASE_URL" -Atc "select round(extract(epoch from (max(t.completed_at)-j.created_at))*1000) from jobs j join tasks t on t.job_id=j.id where j.id='$MEDIA_JOB' group by j.created_at")"
	  record PASS media-customer-path "media quote, firm submit, two real FFmpeg agents, byte-exact merge, and receipt passed (ms=$MEDIA_MS)"
	fi
	if [ "$PROVE_RENDERING" = "1" ]; then
	  wait_job "$RENDER_JOB"
	  RENDER_RESULTS="$(curl -fsS "$CONTROL_URL/v1/jobs/$RENDER_JOB/results" -H "$DEV_BUYER_AUTH")"
	  RENDER_RESULTS_URL="$(jq -r '.results_url // empty' <<<"$RENDER_RESULTS")"
	  [ -n "$RENDER_RESULTS_URL" ] || { echo "rendering result did not expose a merged results_url" >&2; exit 1; }
	  curl -fsS "$RENDER_RESULTS_URL" -o "$ART/render-output.ppm"
	  python3 - "$ART/render-output.ppm" <<'PY'
import pathlib
import sys

body = pathlib.Path(sys.argv[1]).read_bytes()
if not body.startswith(b"P6\n64 64\n255\n"):
    raise SystemExit("rendered artifact header is not the closed 64x64 PPM contract")
if len(body) != len(b"P6\n64 64\n255\n") + 64 * 64 * 3:
    raise SystemExit("rendered artifact payload length is not byte-exact")
PY
  RENDER_DB="$(psql "$DATABASE_URL" -Atc "select j.job_type || '|' || j.status || '|' || count(*) filter (where not t.is_redundancy and not t.is_honeypot) || '|' || count(*) from jobs j join tasks t on t.job_id=j.id where j.id='$RENDER_JOB' group by j.job_type,j.status")"
  [ "$RENDER_DB" = "media_rendering|complete|1|2" ] || {
    echo "rendering database receipt shape was not one primary plus one independent result: $RENDER_DB" >&2
    exit 1
  }
  RENDER_MS="$(psql "$DATABASE_URL" -Atc "select round(extract(epoch from (max(t.completed_at)-j.created_at))*1000) from jobs j join tasks t on t.job_id=j.id where j.id='$RENDER_JOB' group by j.created_at")"
  record PASS rendering-customer-path "rendering quote, firm submit, two builtin agents, byte-exact PPM merge, and receipt passed (ms=$RENDER_MS)"
	fi

  # Re-delivery of the exact terminal commit is harmless, while a delayed
  # process claiming a different attempt epoch must be rejected. This is the
  # execution-side counterpart to submit idempotency.
  COMMIT_ROW="$(psql "$DATABASE_URL" -Atc "
    select json_build_object(
      'task_id',id,'attempt',retry_count,'result_key',result_key,
      'duration_ms',reported_duration_ms,'tokens_used',reported_tokens_used,
      'result_sha256',result_sha256,'hardware_temp_c',reported_hardware_temp_c,
      'supplier_id',execution_supplier_id)::text
      from tasks
     where job_id='$EMBED_JOB' and not is_redundancy and not is_honeypot
     order by chunk_index,id limit 1")"
  COMMIT_TASK_ID="$(jq -r .task_id <<<"$COMMIT_ROW")"
  COMMIT_ATTEMPT="$(jq -r .attempt <<<"$COMMIT_ROW")"
  COMMIT_SUPPLIER="$(jq -r .supplier_id <<<"$COMMIT_ROW")"
  COMMIT_BODY="$(jq 'del(.supplier_id)' <<<"$COMMIT_ROW")"
  case "$COMMIT_SUPPLIER" in
    *a1) COMMIT_TOKEN=dev-worker-token-0001 ;;
    *a2) COMMIT_TOKEN=dev-worker-token-0002 ;;
    *) echo "unexpected proof execution supplier: $COMMIT_SUPPLIER" >&2; exit 1 ;;
  esac
  LEDGER_BEFORE="$(psql "$DATABASE_URL" -Atc 'select count(*) from ledger_entries')"
  REPLAY_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' \
    "$CONTROL_URL/v1/worker/task/$COMMIT_TASK_ID/commit" \
    -H "X-Worker-Token: $COMMIT_TOKEN" -H 'Content-Type: application/json' -d "$COMMIT_BODY")"
  [ "$REPLAY_STATUS" = 204 ]
  STALE_BODY="$(jq --argjson attempt "$((COMMIT_ATTEMPT + 1))" '.attempt=$attempt' <<<"$COMMIT_BODY")"
  STALE_STATUS="$(curl -sS -o /dev/null -w '%{http_code}' \
    "$CONTROL_URL/v1/worker/task/$COMMIT_TASK_ID/commit" \
    -H "X-Worker-Token: $COMMIT_TOKEN" -H 'Content-Type: application/json' -d "$STALE_BODY")"
  [ "$STALE_STATUS" = 409 ]
  LEDGER_AFTER="$(psql "$DATABASE_URL" -Atc 'select count(*) from ledger_entries')"
  [ "$LEDGER_BEFORE" = "$LEDGER_AFTER" ]
  record PASS attempt-fencing "exact terminal replay was inert; wrong-attempt late commit was rejected without money effects"

  ZERO_SUM="$(psql "$DATABASE_URL" -Atc "select abs(coalesce(sum(amount_usd),0)) < 0.000001 from ledger_entries")"
  DUPES="$(psql "$DATABASE_URL" -Atc "select count(*) from (select task_id,kind,count(*) from ledger_entries where task_id is not null group by task_id,kind having count(*)>1) x")"
  [ "$ZERO_SUM" = t ] && [ "$DUPES" = 0 ]
  record PASS money-invariants "ledger is zero-sum and has no duplicate task effects"

  CONTROL_RSS_KB="$(ps -o rss= -p "$CONTROL_PID" | tr -d ' ')"
  AGENT_RSS_KB="$(( $(ps -o rss= -p "$AGENT_PID") + $(ps -o rss= -p "$AGENT2_PID") ))"
  EMBED_MS="$(psql "$DATABASE_URL" -Atc "select round(extract(epoch from (max(t.completed_at)-j.created_at))*1000) from jobs j join tasks t on t.job_id=j.id where j.id='$EMBED_JOB' group by j.created_at")"
  INFER_MS="$(psql "$DATABASE_URL" -Atc "select round(extract(epoch from (max(t.completed_at)-j.created_at))*1000) from jobs j join tasks t on t.job_id=j.id where j.id='$INFER_JOB' group by j.created_at")"
  record PASS performance "control_rss_kb=$CONTROL_RSS_KB two_agent_rss_kb=$AGENT_RSS_KB embed_ms=$EMBED_MS batch_infer_ms=$INFER_MS"
else
  record SKIP customer-path "SKIP_LIVE=1"
fi

SOURCE_END="$("$ART/merc" source-id --root "$ROOT" --field source_sha256)"
[ "$SOURCE_START" = "$SOURCE_END" ]
record PASS source-stable "source fingerprint unchanged"
