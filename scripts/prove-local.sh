#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

ART="${CX_PROOF_ARTIFACT_DIR:-$ROOT/.artifacts/prove-local}"
PGDATA="$ART/pgdata"
LEDGER="$ART/ledger.jsonl"
PGPORT="${PGPORT:-55432}"
MINIO_PORT="${MINIO_PORT:-59000}"
CONTROL_PORT="${CONTROL_PORT:-18080}"
CONTROL_URL="http://127.0.0.1:$CONTROL_PORT"
KEEP="${KEEP:-0}"
SKIP_LIVE="${SKIP_LIVE:-0}"
USE_DOCKER="${USE_DOCKER:-0}"
CONTROL_PID=""
AGENT_PID=""
AGENT2_PID=""
MINIO_PID=""
PG_STARTED=0
SANDBOX_ROOT=""

export DATABASE_URL="postgres://cx@127.0.0.1:$PGPORT/cx?sslmode=disable"
export CX_ENV="development"
export S3_ENDPOINT="http://127.0.0.1:$MINIO_PORT"
export S3_PUBLIC_ENDPOINT="$S3_ENDPOINT"
export S3_BUCKET="cx-jobs"
export S3_ACCESS_KEY="minioadmin"
export S3_SECRET_KEY="minioadmin"
export S3_REGION="us-east-1"
export LISTEN_ADDR=":$CONTROL_PORT"
export CX_ECON_SCHEDULE_VERSION="prove-v1"
export CX_PROCESSOR_PERCENT_BPS="290"
export CX_PROCESSOR_FIXED_USD="0.30"
export CX_CONTROL_PLANE_PER_TASK_USD="0.0001"
export CX_TARGET_MARGIN_BPS="1000"
export CARGO_TARGET_DIR="${CARGO_TARGET_DIR:-$ROOT/.artifacts/prove-cache/cargo-target}"
export GOCACHE="${GOCACHE:-$ROOT/.artifacts/prove-cache/go-cache}"

DEV_ADMIN_AUTH='Authorization: Bearer dev-admin-key-0001' # gitleaks:allow -- seeded local proof credential
DEV_BUYER_AUTH='Authorization: Bearer dev-api-key-0001'   # gitleaks:allow -- seeded local proof credential

# A proof must be hermetic even when the caller has sourced a production or
# Stripe-enabled .env.  The live processor boundary is exercised by the Go
# webhook/financial model tests; this local E2E intentionally runs the shadow
# ledger and must never inherit credentials or contact Stripe.
unset STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET CX_CONNECT_WEBHOOK_SECRET

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
      docker compose down >/dev/null 2>&1 || true
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
    jq '[.[] | select(.version != "seed")] | length')"
  [ "$count" -ge "$expected" ]
}

for tool in go cargo psql curl jq openssl git node python3; do
  command -v "$tool" >/dev/null || { echo "missing tool: $tool" >&2; exit 1; }
done

rm -rf "$ART"
mkdir -p "$ART"
: >"$LEDGER"

(cd control && go build -o "$ART/cx" .)
"$ART/cx" audit codebase --out census
SOURCE_START="$("$ART/cx" source-id --root "$ROOT" --field source_sha256)"
record PASS source-bound "source_sha256=$SOURCE_START"
record PASS census "authoritative census regenerated before source binding"

test -z "$(gofmt -l control)"
(cd control && go vet ./... && go test ./... && go test -race ./...)
(cd agent && cargo fmt --all -- --check && cargo clippy --all-targets --no-default-features -- -D warnings && cargo test --no-default-features)
bash scripts/verify-python-sdk-package.sh
node scripts/site-build.mjs
record PASS local-gates "format, vet, race, Rust, SDK, and site gates passed"

if [ "$USE_DOCKER" = "1" ]; then
  docker compose up -d postgres minio createbuckets
  export DATABASE_URL="postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable"
  export S3_ENDPOINT="http://127.0.0.1:9000"
  export S3_PUBLIC_ENDPOINT="$S3_ENDPOINT"
  wait_for 60 psql "$DATABASE_URL" -c 'select 1'
else
  for tool in initdb pg_ctl createdb minio mc; do
    command -v "$tool" >/dev/null || { echo "missing native dependency: $tool" >&2; exit 1; }
  done
  initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$ART/postgres.log"
  pg_ctl -D "$PGDATA" -o "-p $PGPORT -c listen_addresses=127.0.0.1" -l "$ART/postgres.log" -w start
  PG_STARTED=1
  createdb -h 127.0.0.1 -p "$PGPORT" -U cx cx
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    minio server "$ART/minio" --address "127.0.0.1:$MINIO_PORT" >"$ART/minio.log" 2>&1 &
  MINIO_PID=$!
  wait_for 30 curl -fsS "$S3_ENDPOINT/minio/health/live"
  mc alias set cx-local "$S3_ENDPOINT" minioadmin minioadmin >/dev/null
  mc mb --ignore-existing cx-local/cx-jobs >/dev/null
fi
record PASS dependencies "PostgreSQL and MinIO are healthy"

psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f control/schema.sql >/dev/null
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
CX_VERIFICATION_SAMPLE_SECRET="$SAMPLE_SECRET" CX_TOKEN_KEY="$TOKEN_KEY" "$ART/cx" seed >"$ART/seed.log"
CX_VERIFICATION_SAMPLE_SECRET="$SAMPLE_SECRET" CX_TOKEN_KEY="$TOKEN_KEY" "$ART/cx" >"$ART/control.log" 2>&1 &
CONTROL_PID=$!
wait_for 30 curl -fsS "$CONTROL_URL/readyz"
record PASS control "ready endpoint passed"

if [ "$SKIP_LIVE" != "1" ]; then
  cargo build --release --manifest-path agent/Cargo.toml
  SANDBOX_ROOT="$(mktemp -d /private/tmp/cx-prove.XXXXXX)"
  mkdir -p "$SANDBOX_ROOT/home/.compute-exchange"
  cp "$CARGO_TARGET_DIR/release/cx-agent" "$SANDBOX_ROOT/cx-agent"
  cp macapp/ComputeExchangeAgent/cx-agent.sb "$SANDBOX_ROOT/cx-agent.sb"
  AGENT_BIN="$SANDBOX_ROOT/cx-agent"
  MODEL_CACHE="${CX_MODEL_CACHE:-${HF_HOME:-$HOME/.cache/huggingface}}"
  for n in 1 2; do
    token="dev-worker-token-000$n"
    supplier="00000000-0000-0000-0000-0000000000a$n"
    mkdir -p "$SANDBOX_ROOT/home/.compute-exchange/agent$n"
    {
      printf 'control_url = "%s"\n' "$CONTROL_URL"
      printf 'worker_token = "%s"\n' "$token"
      printf 'supplier_id = "%s"\n' "$supplier"
      printf 'max_cpu_pct = 100.0\npower_only = false\nmin_payout_usd_per_hr = 0.0\n'
      printf 'memory_headroom_gb = 0.0\nmax_memory_pct = 0.0\n'
      printf 'data_dir = "%s"\n' "$SANDBOX_ROOT/home/.compute-exchange/agent$n"
    } >"$SANDBOX_ROOT/agent$n.toml"
  done

  HOME="$SANDBOX_ROOT/home" CX_MODEL_CACHE="$MODEL_CACHE" \
    CX_SANDBOX_PROFILE="$SANDBOX_ROOT/cx-agent.sb" CX_REQUIRE_SANDBOX=1 \
    CX_CONTROL_URL="$CONTROL_URL" CX_WORKER_TOKEN=dev-worker-token-0001 \
    "$AGENT_BIN" run --config "$SANDBOX_ROOT/agent1.toml" >"$ART/agent1.log" 2>&1 &
  AGENT_PID=$!
  wait_for 300 workers_ready 1

  HOME="$SANDBOX_ROOT/home" CX_MODEL_CACHE="$MODEL_CACHE" \
    CX_SANDBOX_PROFILE="$SANDBOX_ROOT/cx-agent.sb" CX_REQUIRE_SANDBOX=1 \
    CX_CONTROL_URL="$CONTROL_URL" CX_WORKER_TOKEN=dev-worker-token-0002 \
    "$AGENT_BIN" run --config "$SANDBOX_ROOT/agent2.toml" >"$ART/agent2.log" 2>&1 &
  AGENT2_PID=$!
  wait_for 300 workers_ready 2
  record PASS two-agents "two distinct supplier agents registered"

  submit() {
    body="$(jq -nc --arg model "$1" --argjson job_type "$2" --arg input "$3" \
      '{job_type:$job_type,model:{ref:$model},params:{split_size:1},
        constraints:{min_memory_gb:0,hw_classes:null,data_residency:null},
        verification:{redundancy_frac:0,honeypot_frac:0,payout_hold_secs:0,skip_verification_floor:true},
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

  EMBED_JOB="$(submit all-minilm-l6-v2 '{"type":"embed","batch_size":8}' $'{"text":"apple silicon"}\n{"text":"compute market"}\n')"
  INFER_JOB="$(submit llama-3.2-1b-instruct-q4 '{"type":"batch_infer","max_tokens":12,"temperature":0}' $'{"prompt":"Reply with only: ping"}\n')"
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

SOURCE_END="$("$ART/cx" source-id --root "$ROOT" --field source_sha256)"
[ "$SOURCE_START" = "$SOURCE_END" ]
record PASS source-stable "source fingerprint unchanged"
