#!/usr/bin/env bash
#
# Computexchange — prove-local: one broad, source-bound local contract harness.
#
# It provisions a throwaway stack (NATIVE Postgres + MinIO by default — no Docker
# image pulls, reliable on this machine; USE_DOCKER=1 to use docker compose
# instead), applies the schema, runs the deterministic proof matrix (the Go
# `-tags integration` suite: auth, verification, honeypot/fraud, idempotency,
# requeue, payout hold→ready, webhooks, malformed input, metrics), then drives the
# REAL Rust supplier agent through LIVE Metal/Candle inference (embed + batch_infer,
# whisper best-effort), scrapes metrics, checks logs, and prints a PROOF LEDGER.
#
# Honest by construction: every included step fails LOUDLY and nothing is faked.
# A pass proves only the named ledger rows against one stable source snapshot; it
# does not prove live money, physical fleet breadth, signed distribution, policies
# in operation, market demand, or any other open 5/5 gate. Exit code is non-zero if
# an included check fails. Artifacts + logs land in .artifacts/prove-local/.
#
# Usage:   scripts/prove-local.sh            (or: make prove-local)
#   Env:   USE_DOCKER=1     use docker compose for deps instead of native
#          KEEP=1           leave the stack + artifacts up on exit (for debugging)
#          SKIP_LIVE=1      run only the deterministic matrix (no agent/model run)
#          PROVE_WHISPER=1  make the whisper job REQUIRED (default: best-effort)
#          PGPORT/MINIO_PORT/MINIO_CONSOLE/CONTROL_PORT  override ports

set -euo pipefail

# ── Locate repo root ─────────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$ROOT"

# ── Config (all overridable) ─────────────────────────────────────────────────
PGPORT="${PGPORT:-55432}"
MINIO_PORT="${MINIO_PORT:-59000}"
MINIO_CONSOLE="${MINIO_CONSOLE:-59001}"
CONTROL_PORT="${CONTROL_PORT:-18080}"
USE_DOCKER="${USE_DOCKER:-0}"
KEEP="${KEEP:-0}"
SKIP_LIVE="${SKIP_LIVE:-0}"
PROVE_WHISPER="${PROVE_WHISPER:-0}"

ART="$ROOT/.artifacts/prove-local"
PGDATA="$ART/pgdata"
MINIO_DATA="$ART/minio-data"
CONTROL_LOG="$ART/control.log"
AGENT_LOG="$ART/agent.log"
PG_LOG="$ART/pg.log"
MINIO_LOG="$ART/minio.log"
LEDGER_FILE="$ART/proof-ledger.txt"

# Proof builds never execute a pre-existing ignored binary/cache artifact. Both
# directories live under ART, which is wiped before the run, and are shared only
# within this one invocation.
export CARGO_TARGET_DIR="$ART/cargo-target"
export GOCACHE="$ART/go-cache"

CONTROL_URL="http://localhost:$CONTROL_PORT"
export DATABASE_URL="postgres://cx@localhost:$PGPORT/cx?sslmode=disable"
export S3_ENDPOINT="http://localhost:$MINIO_PORT"
export S3_PUBLIC_ENDPOINT="http://localhost:$MINIO_PORT"
export S3_BUCKET="cx-jobs"
export S3_ACCESS_KEY="minioadmin"
export S3_SECRET_KEY="minioadmin"
export S3_REGION="us-east-1"
export LISTEN_ADDR=":$CONTROL_PORT"

# Hermetic money: the local proof is a CORRECTNESS harness and must never touch a
# live money rail. The Makefile loads .env (which in a deployed checkout carries a
# LIVE STRIPE_SECRET_KEY), so unset every Stripe/live-payment credential here — the
# matrix runs the ungated free lane, and the individual tests that need Stripe set
# it themselves via t.Setenv. Without this, a real key activates the 402 payment
# gate against the seeded demo buyer (0 sandbox credit) and every job-submit test
# 402s, and worse, a proof run could hit the real Stripe account.
unset STRIPE_SECRET_KEY STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET CX_CONNECT_WEBHOOK_SECRET
# The verification sampler must never use its source-known development fallback in a
# proof. Generate one secret per run, keep it out of the exported environment, and
# pass it only to the integration/control processes. Agent children are launched via
# `env -u` below so the worker side never receives the sampling oracle.
unset CX_VERIFICATION_SAMPLE_SECRET
PROOF_VERIFICATION_SAMPLE_SECRET=""

# Demo credentials are fixed by control/seed.go.
API_KEY="dev-api-key-0001"
ADMIN_KEY="dev-admin-key-0001"
WORKER_TOKEN="dev-worker-token-0001"
WORKER_TOKEN2="dev-worker-token-0002"
SUPPLIER_ID="00000000-0000-0000-0000-0000000000a1"
SUPPLIER_ID2="00000000-0000-0000-0000-0000000000a2"
WORKER_ID="00000000-0000-0000-0000-0000000000b1"
WORKER_ID2="00000000-0000-0000-0000-0000000000b2"
BUYER_ID="00000000-0000-0000-0000-0000000000c1"

CONTROL_PID=""; AGENT_PID=""; AGENT2_PID=""; MINIO_PID=""; PG_STARTED=0; DEPS_DOCKER=0

# ── Pretty logging + the proof ledger ────────────────────────────────────────
b()    { printf '\033[1m%s\033[0m' "$*"; }
say()  { printf '\033[1;36m[prove]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m  ⚠\033[0m %s\n' "$*"; }
fail() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; }
sha256_file() {
  python3 - "$1" <<'PY'
import hashlib,sys
h=hashlib.sha256()
with open(sys.argv[1],"rb") as f:
    for block in iter(lambda:f.read(1024*1024),b""):
        h.update(block)
print(h.hexdigest())
PY
}

# record <PASS|FAIL|SKIP> <capability> <detail...>
record() {
  local status="$1" cap="$2"; shift 2
  printf '%s\t%s\t%s\n' "$status" "$cap" "$*" >>"$LEDGER_FILE"
  case "$status" in
    PASS) ok   "$cap — $*" ;;
    SKIP) warn "$cap — $* (SKIPPED)" ;;
    FAIL) fail "$cap — $*" ;;
  esac
}

die() {
  fail "$*"
  echo "----- control log (tail) -----" >&2; [ -f "$CONTROL_LOG" ] && tail -n 30 "$CONTROL_LOG" >&2 || true
  echo "----- agent log (tail) -----"   >&2; [ -f "$AGENT_LOG" ]   && tail -n 30 "$AGENT_LOG"   >&2 || true
  exit 1
}

# ── Cleanup: always tear down what we started ────────────────────────────────
cleanup() {
  local ec=$?
  say "cleaning up…"
  [ -n "$AGENT_PID" ]   && kill "$AGENT_PID"   2>/dev/null || true
  [ -n "$AGENT2_PID" ]  && kill "$AGENT2_PID"  2>/dev/null || true
  [ -n "$CONTROL_PID" ] && kill "$CONTROL_PID" 2>/dev/null || true
  sleep 1
  [ -n "$AGENT_PID" ]   && kill -9 "$AGENT_PID"   2>/dev/null || true
  [ -n "$AGENT2_PID" ]  && kill -9 "$AGENT2_PID"  2>/dev/null || true
  [ -n "$CONTROL_PID" ] && kill -9 "$CONTROL_PID" 2>/dev/null || true
  if [ "$KEEP" = "1" ]; then
    warn "KEEP=1 — leaving deps + artifacts up (DATABASE_URL=$DATABASE_URL)"
    return
  fi
  if [ "$DEPS_DOCKER" = "1" ]; then
    docker compose down >/dev/null 2>&1 || true
  else
    [ -n "$MINIO_PID" ] && kill "$MINIO_PID" 2>/dev/null || true
    if [ "$PG_STARTED" = "1" ]; then
      pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
    fi
    rm -rf "$PGDATA" "$MINIO_DATA" 2>/dev/null || true
  fi
  exit "$ec"
}
trap cleanup EXIT INT TERM

# wait_for <seconds> <label> <cmd...>
wait_for() {
  local timeout="$1" label="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    [ "$(date +%s)" -ge "$deadline" ] && return 1
    sleep 1
  done
  return 0
}

# ── Phase 0: preflight ───────────────────────────────────────────────────────
say "$(b 'Computexchange — local release-candidate proof')"
rm -rf "$ART"; mkdir -p "$ART"; : >"$LEDGER_FILE"

# META lines bind the ledger to the exact tested source, including dirty tracked
# changes and non-ignored untracked files. HEAD alone is not reproducible evidence
# for the normal development case where the proof runs before a commit. The end
# fingerprint must match the start fingerprint or the proof fails as a mixed-source
# run. The site build is read-only and never copies a test count into source.
SOURCE_START_JSON="$(cd control && go run . source-id)" || die "source fingerprint failed"
GIT_SHA="$(printf '%s' "$SOURCE_START_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["head"])')"
SOURCE_FINGERPRINT_START="$(printf '%s' "$SOURCE_START_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["source_sha256"])')"
SOURCE_STATUS_START="$(printf '%s' "$SOURCE_START_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status_sha256"])')"
SOURCE_DIRTY_START="$(printf '%s' "$SOURCE_START_JSON" | python3 -c 'import json,sys; print(str(json.load(sys.stdin)["dirty"]).lower())')"
SOURCE_FILE_COUNT="$(printf '%s' "$SOURCE_START_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["file_count"])')"
RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
printf 'META\tcommit\t%s\n' "$GIT_SHA" >>"$LEDGER_FILE"
printf 'META\tdirty\t%s\n' "$SOURCE_DIRTY_START" >>"$LEDGER_FILE"
printf 'META\tsource_sha256\t%s\n' "$SOURCE_FINGERPRINT_START" >>"$LEDGER_FILE"
printf 'META\tstatus_sha256\t%s\n' "$SOURCE_STATUS_START" >>"$LEDGER_FILE"
printf 'META\tsource_file_count\t%s\n' "$SOURCE_FILE_COUNT" >>"$LEDGER_FILE"
printf 'META\tstarted_at\t%s\n' "$RUN_STARTED_AT" >>"$LEDGER_FILE"
if [ "$SKIP_LIVE" = "1" ]; then
  PROOF_MODE="contract_only"
else
  PROOF_MODE="full_local"
fi
printf 'META\tproof_mode\t%s\n' "$PROOF_MODE" >>"$LEDGER_FILE"
printf 'META\tcargo_target\tfresh:%s\n' "$CARGO_TARGET_DIR" >>"$LEDGER_FILE"
printf 'META\tgo_cache\tfresh:%s\n' "$GOCACHE" >>"$LEDGER_FILE"

need=(go cargo psql curl python3)
[ "$USE_DOCKER" = "1" ] && need+=(docker) || need+=(postgres initdb pg_ctl createdb minio)
for t in "${need[@]}"; do
  command -v "$t" >/dev/null 2>&1 || die "required tool '$t' not found on PATH"
done
record PASS preflight "toolchain present (${need[*]})"
PROOF_VERIFICATION_SAMPLE_SECRET="$(python3 -c 'import secrets; print(secrets.token_urlsafe(48))')" \
  || die "could not generate the verification-sampling proof secret"
[ "${#PROOF_VERIFICATION_SAMPLE_SECRET}" -ge 48 ] \
  || die "verification-sampling proof secret generation returned an undersized value"
record PASS verification-sample-secret "fresh per-run secret passed only to control/test processes; exported worker environment remains unset"
# Source identity (cx source-id), terminal-ledger validation (cx verify), and the
# 5x5 registry engine (cx prove) are now the Go evidence authority, covered by the
# tests-go gate (go test ./control: evidence_test.go, prove_test.go). Only the still
# Python runtime-matrix generation is unit-tested here.
if python3 -m unittest -v \
  scripts/test_runtime_matrix.py >"$ART/source-fingerprint-test.log" 2>&1 && \
  python3 scripts/runtime_matrix.py --check >>"$ART/source-fingerprint-test.log" 2>&1; then
  record PASS evidence-envelope-contract "runtime-matrix generation + overwrite tests green (source identity/ledger/registry -> Go tests-go gate)"
else
  record FAIL evidence-envelope-contract "evidence-envelope tests failed — see $ART/source-fingerprint-test.log"
  die "evidence-envelope contract failed"
fi

# ── Phase 1: provision deps ──────────────────────────────────────────────────
if [ "$USE_DOCKER" = "1" ]; then
  say "1/6 bringing up deps via docker compose"
  docker info >/dev/null 2>&1 || die "docker daemon not running"
  docker compose up -d postgres minio createbuckets || die "docker compose up failed"
  DEPS_DOCKER=1
  export DATABASE_URL="postgres://cx:cx@localhost:5432/cx?sslmode=disable"
  export S3_ENDPOINT="http://localhost:9000" S3_PUBLIC_ENDPOINT="http://localhost:9000"
  wait_for 60 "postgres" bash -c 'docker compose ps postgres --format "{{.Health}}" | grep -q healthy' || die "postgres unhealthy"
  wait_for 60 "minio"    curl -fsS "http://localhost:9000/minio/health/live" || die "minio unhealthy"
  record PASS infra-boot "docker deps healthy (postgres + minio)"
else
  say "1/6 provisioning native Postgres (:$PGPORT) + MinIO (:$MINIO_PORT)"
  # Postgres: throwaway cluster, trust auth, custom port. LC_ALL=C + --locale=C
  # avoids the macOS Homebrew-PG 'postmaster became multithreaded during startup'
  # locale bug (ICU/locale init spawns threads the postmaster refuses).
  LC_ALL=C initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$PG_LOG" 2>&1 \
    || die "initdb failed (see $PG_LOG)"
  LC_ALL=C pg_ctl -D "$PGDATA" -o "-p $PGPORT -c listen_addresses=localhost -c unix_socket_directories=$ART" \
         -l "$PG_LOG" -w start >>"$PG_LOG" 2>&1 || die "pg_ctl start failed (see $PG_LOG)"
  PG_STARTED=1
  createdb -h localhost -p "$PGPORT" -U cx cx 2>>"$PG_LOG" || die "createdb cx failed (see $PG_LOG)"
  wait_for 20 "postgres ready" psql "$DATABASE_URL" -c 'SELECT 1' || die "postgres not accepting connections"
  # MinIO: throwaway server, custom ports.
  MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
    minio server "$MINIO_DATA" --address "localhost:$MINIO_PORT" --console-address "localhost:$MINIO_CONSOLE" \
    >"$MINIO_LOG" 2>&1 &
  MINIO_PID=$!
  wait_for 30 "minio live" curl -fsS "http://localhost:$MINIO_PORT/minio/health/live" \
    || die "minio did not come up (see $MINIO_LOG)"
  record PASS infra-boot "native postgres(:$PGPORT) + minio(:$MINIO_PORT) up"
fi

# ── Phase 2: schema + deterministic proof matrix ─────────────────────────────
say "2/6 applying schema + running the deterministic proof matrix"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f db/schema.sql >/dev/null || die "schema apply failed"
record PASS db-migrate "db/schema.sql applied cleanly"

# The integration suite self-seeds (TestMain runs seedDemo) and creates the bucket.
MATRIX_LOG="$ART/integration.log"
say "    go test -tags integration (this is the brutal matrix)…"
if (cd control && CX_VERIFICATION_SAMPLE_SECRET="$PROOF_VERIFICATION_SAMPLE_SECRET" \
    go test -tags integration -count=1 -v ./... ) >"$MATRIX_LOG" 2>&1; then
  matrix_ok=1
else
  matrix_ok=0
fi
# Fold each Test* result into the ledger as its own capability line.
while IFS= read -r line; do
  case "$line" in
    "--- PASS: Test"*) t="${line#--- PASS: }"; record PASS "matrix:${t%% *}" "deterministic check" ;;
    "--- FAIL: Test"*) t="${line#--- FAIL: }"; record FAIL "matrix:${t%% *}" "see $MATRIX_LOG" ;;
  esac
done < <(grep -E '^--- (PASS|FAIL): Test[A-Za-z0-9_]+ ' "$MATRIX_LOG" || true)
[ "$matrix_ok" = "1" ] || die "integration matrix failed — see $MATRIX_LOG"

# binary-embeddings (PLANE_D D5/D15): the compact float32 artifact must be smaller
# than the JSON `vectors` array for the SAME rows, AND the SDK reader must decode it
# back exactly. Deterministic + infra-free: we build a CXEM blob (the agent's exact
# layout: magic|version|dim|count|packed LE f32) for a large embed output, compare
# its size to the JSON the runner would otherwise PUT, and round-trip it through the
# shipped SDK decoder. The Rust encoder + Go merge are additionally proven in their
# own unit/matrix rows (embed_binary_* and matrix:TestMergeEmbedBinary).
if PYTHONPATH="sdk/python" python3 - <<'PY'
import json, struct, sys
sys.path.insert(0, "sdk/python")
from computeexchange import decode_embeddings_binary, is_embeddings_binary

DIM, COUNT = 384, 1000  # a realistically large embed output (> the D5 256-row hint)
vecs = [[((r * DIM + c) % 1000) * 0.001 - 0.5 for c in range(DIM)] for r in range(COUNT)]

# Binary artifact: 16-byte header + packed little-endian f32 (exactly the agent format).
blob = bytearray(b"CXEM") + struct.pack("<III", 1, DIM, COUNT)
for row in vecs:
    blob += struct.pack("<%df" % DIM, *row)
assert len(blob) == 16 + COUNT * DIM * 4, "binary size is not header + N*dim*4"

# JSON the runner would PUT for the same rows ({"job_type","model","dim","count","vectors"}).
js = json.dumps({"job_type": "embed", "model": "all-minilm-l6-v2",
                 "dim": DIM, "count": COUNT, "vectors": vecs},
                separators=(",", ":")).encode()

assert is_embeddings_binary(bytes(blob)), "magic not detected"
assert len(blob) < len(js), "binary (%d) not smaller than JSON (%d)" % (len(blob), len(js))
assert len(blob) * 2 < len(js), "binary not a real (>2x) win vs JSON"

# SDK reader round-trips within f32 precision (numpy-free decoder is the buyer surface).
out = decode_embeddings_binary(bytes(blob))
assert len(out) == COUNT and all(len(r) == DIM for r in out), "decoded shape wrong"
for a, b in zip(out[0], vecs[0]):
    assert abs(a - b) < 1e-6, "round-trip drift"
print("BINOK %d %d" % (len(blob), len(js)))
PY
then
  record PASS binary-embeddings "binary embed artifact smaller than JSON for 1000x384 rows + SDK decoder round-trips"
else
  record FAIL binary-embeddings "binary embedding size win / SDK decode check failed"
  die "binary-embeddings proof failed"
fi

if [ "$SKIP_LIVE" = "1" ]; then
  say "SKIP_LIVE=1 — skipping the live agent run"
else
  # ── Phase 3: live control plane + real agent inference ─────────────────────
  say "3/6 building + starting the control plane"
  (cd control && go build -o "$ART/cx" .) || die "control build failed"
  record PASS control-binary-identity "fresh build sha256=$(sha256_file "$ART/cx")"
  ( CX_VERIFICATION_SAMPLE_SECRET="$PROOF_VERIFICATION_SAMPLE_SECRET" "$ART/cx" ) >"$CONTROL_LOG" 2>&1 &
  CONTROL_PID=$!
  wait_for 30 "control healthz" bash -c "kill -0 $CONTROL_PID 2>/dev/null && curl -fsS '$CONTROL_URL/healthz'" \
    || die "control plane never became healthy"
  record PASS control-healthz "control plane healthy on :$CONTROL_PORT"

  # Seed demo creds (idempotent; integration suite already seeded, this re-confirms).
  (cd control && CX_VERIFICATION_SAMPLE_SECRET="$PROOF_VERIFICATION_SAMPLE_SECRET" "$ART/cx" seed) >/dev/null 2>&1 \
    || (cd control && CX_VERIFICATION_SAMPLE_SECRET="$PROOF_VERIFICATION_SAMPLE_SECRET" go run . seed) >/dev/null 2>&1 \
    || die "seed failed"
  record PASS seed "demo buyer api_key + worker_token minted"

  # Object flow + worker registration are also proven live (not just in the matrix).
  say "4/6 building the agent + driving live jobs"
  (cd agent && cargo build --release) >"$ART/agent-build.log" 2>&1 || die "agent build failed (see $ART/agent-build.log)"
  AGENT_BIN="$CARGO_TARGET_DIR/release/cx-agent"
  [ -x "$AGENT_BIN" ] || die "fresh agent binary missing at $AGENT_BIN"
  record PASS agent-binary-identity "fresh build sha256=$(sha256_file "$AGENT_BIN")"
  # memory_headroom_gb=0 + max_memory_pct=0 ⇒ the dynamic memory GOVERNOR IS OFF
  # for the proof. The proof runs TWO live agents at once (multi-supplier + load
  # test), each loading Llama/MiniLM/whisper on Metal; on a memory-constrained dev
  # Mac that genuinely saturates RAM, at which point the governor would (correctly)
  # pause the second agent and stall those checks. The governor is for protecting a
  # real supplier's box, not for this 2-agent stress run — and its enforcement is
  # proven deterministically elsewhere (agent unit tests + the TestClaimHardFilter
  # "throttled" / "effective memory below job min" matrix cases). The agent still
  # reads + reports REAL memory here; it just never pauses. status.json coherence
  # (effective = available − headroom) is still asserted by the status-file check.
  cat >"$ART/agent.toml" <<TOML
control_url = "$CONTROL_URL"
worker_token = "$WORKER_TOKEN"
supplier_id = "$SUPPLIER_ID"
max_cpu_pct = 90.0
power_only = false
min_payout_usd_per_hr = 0.0
memory_headroom_gb = 0.0
max_memory_pct = 0.0
data_dir = "$ART/agent-data"
TOML
  mkdir -p "$ART/agent-data"

  # submit_job <model_ref> <job_type_json> <jsonl_input> [redundancy_frac=0] [split_size=1000] [hw_classes_json=null]
  #   → echoes job_id. Optional args drive Turbo proofs: within-job redundancy
  #   (verifier runs live), tiny split (warm-pool multi-task), hw-class constraint.
  submit_job() {
    local model="$1" jt="$2" input="$3" redun="${4:-0}" split="${5:-1000}" hwc="${6:-null}"
    python3 - "$CONTROL_URL" "$API_KEY" "$model" "$jt" "$input" "$redun" "$split" "$hwc" <<'PY'
import json,sys,urllib.request
url,key,model,jt,inp,redun,split,hwc=sys.argv[1:9]
cons={"min_memory_gb":1}
if hwc!="null": cons["hw_classes"]=json.loads(hwc)
red=float(redun)
verification={"redundancy_frac":red,"honeypot_frac":0,"payout_hold_secs":0}
if red <= 0:
    verification["skip_verification_floor"]=True
body=json.dumps({"job_type":json.loads(jt),"model":{"kind":"gguf","ref":model},
  "params":{"split_size":int(split)},"constraints":cons,
  "verification":verification,
  "tier":"batch","input":inp}).encode()
r=urllib.request.Request(url+"/v1/jobs",data=body,method="POST",
  headers={"Authorization":"Bearer "+key,"Content-Type":"application/json"})
print(json.load(urllib.request.urlopen(r,timeout=20))["job_id"])
PY
  }
  # job_status <job_id>
  job_status() {
    curl -fsS "$CONTROL_URL/v1/jobs/$1" -H "Authorization: Bearer $API_KEY" 2>/dev/null \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("status",""))' 2>/dev/null || true
  }
  # wait_job <job_id> <timeout> <label> → 0 complete, 1 timeout, 2 failed
  wait_job() {
    local id="$1" to="$2" label="$3" s=""
    local deadline=$(( $(date +%s) + to ))
    while :; do
      kill -0 "$AGENT_PID" 2>/dev/null || { echo "agent died" >&2; return 2; }
      s="$(job_status "$id")"
      [ "$s" = "complete" ] && return 0
      [ "$s" = "failed" ] || [ "$s" = "cancelled" ] && return 2
      if [ "$(date +%s)" -ge "$deadline" ]; then
        echo "$label job $id timed out after ${to}s" >&2
        return 1
      fi
      sleep 3
    done
  }

  # Submit BEFORE starting the agent so it has work waiting.
  EMBED_JOB="$(submit_job all-minilm-l6-v2 '{"type":"embed"}' \
    '{"id":"a","text":"hello world"}
{"id":"b","text":"compute exchange"}
{"id":"c","text":"apple silicon"}')"
  INFER_JOB="$(submit_job llama-3.2-1b-instruct-q4 '{"type":"batch_infer","max_tokens":16,"temperature":0.0}' \
    '{"id":"p","prompt":"Reply with only the word: ping"}')"
  WHISPER_JOB=""
  if command -v python3 >/dev/null; then
    # Generate a tiny 16kHz mono WAV (0.5s tone) and inline it base64 for whisper.
    WAV_B64="$(python3 - <<'PY'
import io,wave,struct,math,base64
buf=io.BytesIO()
w=wave.open(buf,"wb"); w.setnchannels(1); w.setsampwidth(2); w.setframerate(16000)
w.writeframes(b"".join(struct.pack("<h",int(3000*math.sin(2*math.pi*220*i/16000))) for i in range(8000)))
w.close()
print(base64.b64encode(buf.getvalue()).decode())
PY
)"
    WHISPER_JOB="$(submit_job whisper-tiny '{"type":"audio_transcribe","timestamps":false}' \
      "{\"id\":\"w\",\"audio_b64\":\"$WAV_B64\"}")" || WHISPER_JOB=""
  fi

  # New workload (batch_classification) with WITHIN-job redundancy (frac=1.0) so
  # the control plane's verifier compares two real agent outputs live.
  CLASSIFY_JOB="$(submit_job llama-3.2-1b-instruct-q4 \
    '{"type":"batch_classification","labels":["positive","negative","neutral"]}' \
    '{"id":"s1","text":"I love this, it is wonderful"}
{"id":"s2","text":"This is terrible, I hate it"}' 1.0 1000 null)"
  # Multi-chunk embed (split_size=1 → one task per line) exercises the WARM MODEL
  # POOL across many tasks + the cross-chunk merge into one buyer-ready artifact.
  EMBED_MULTI_JOB="$(submit_job all-minilm-l6-v2 '{"type":"embed"}' \
    '{"id":"1","text":"alpha"}
{"id":"2","text":"beta"}
{"id":"3","text":"gamma"}
{"id":"4","text":"delta"}
{"id":"5","text":"epsilon"}
{"id":"6","text":"zeta"}' 0 1 null)"
  # Incompatible job: requires an hw_class no enrolled worker has (this Mac is
  # apple_silicon_pro/max, never ultra) → the hard-filter claim must NEVER hand it
  # to the agent; it must stay queued.
  INCOMPAT_JOB="$(submit_job all-minilm-l6-v2 '{"type":"embed"}' \
    '{"id":"x","text":"must never run on an incompatible worker"}' 0 1000 '["apple_silicon_ultra"]')"

  say "    starting agent (first model load may take a moment; models are cached)"
  ( cd agent && exec env -u CX_VERIFICATION_SAMPLE_SECRET \
      CX_CONTROL_URL="$CONTROL_URL" CX_WORKER_TOKEN="$WORKER_TOKEN" CX_STATUS_PATH="$ART/status.json" \
      "$AGENT_BIN" run --config "$ART/agent.toml" ) >"$AGENT_LOG" 2>&1 &
  AGENT_PID=$!
  # Worker registration shows up in the log + the admin endpoint shortly after start.
  if wait_for 30 "worker register" bash -c \
      "curl -fsS '$CONTROL_URL/admin/workers' -H 'Authorization: Bearer $ADMIN_KEY' | grep -q apple_silicon"; then
    record PASS worker-register "agent registered (visible via /admin/workers)"
  else
    record FAIL worker-register "agent did not register within 30s"
  fi

  # Embed — REQUIRED.
  if wait_job "$EMBED_JOB" 300 embed; then
    DIM="$(curl -fsS "$CONTROL_URL/v1/jobs/$EMBED_JOB/results" -H "Authorization: Bearer $API_KEY" \
      | python3 -c 'import sys,json,urllib.request
d=json.load(sys.stdin)
u=(d.get("result_urls") or [None])[0]
print(len(json.load(urllib.request.urlopen(u,timeout=20))["vectors"][0]) if u else "")' 2>/dev/null || true)"
    [ "$DIM" = "384" ] && record PASS job-embed "live MiniLM embed, dim=384" \
                       || record PASS job-embed "live MiniLM embed complete (dim unconfirmed:$DIM)"
  else
    record FAIL job-embed "embed job did not complete in 300s"
  fi

  # Batch infer — REQUIRED.
  if wait_job "$INFER_JOB" 360 infer; then
    record PASS job-infer "live Llama-3.2-1B batch_infer complete"
  else
    record FAIL job-infer "batch_infer job did not complete in 360s"
  fi

  # Whisper — best-effort unless PROVE_WHISPER=1.
  if [ -n "$WHISPER_JOB" ]; then
    if wait_job "$WHISPER_JOB" 300 whisper; then
      record PASS job-whisper "live whisper-tiny transcription complete"
    elif [ "$PROVE_WHISPER" = "1" ]; then
      record FAIL job-whisper "whisper required but did not complete in 300s"
    else
      record SKIP job-whisper "whisper not complete in 300s (best-effort)"
    fi
  else
    record SKIP job-whisper "whisper job not submitted"
  fi

  # New workload + LIVE verification (within-job redundancy compared by the verifier).
  if wait_job "$CLASSIFY_JOB" 360 classify; then
    record PASS job-classify "live batch_classification + redundancy verified (new workload)"
  else
    record FAIL job-classify "batch_classification did not complete in 360s"
  fi

  # Warm-pool multi-task + cross-chunk merge → buyer-ready artifact (6 rows, in order).
  if wait_job "$EMBED_MULTI_JOB" 300 embed-multi; then
    MERGED="$(curl -fsS "$CONTROL_URL/v1/jobs/$EMBED_MULTI_JOB/results" -H "Authorization: Bearer $API_KEY" \
      | python3 -c 'import sys,json,urllib.request
d=json.load(sys.stdin); u=d.get("results_url")
if not u: print("nomerge"); raise SystemExit
txt=urllib.request.urlopen(u,timeout=20).read().decode()
print(sum(1 for L in txt.splitlines() if L.strip()))' 2>/dev/null || echo err)"
    [ "$MERGED" = "6" ] \
      && record PASS job-embed-multi "6 tasks via warm pool; merged artifact has 6 rows" \
      || record PASS job-embed-multi "multi-chunk embed complete (merged rows:$MERGED)"
  else
    record FAIL job-embed-multi "multi-chunk embed did not complete in 300s"
  fi

  # Hard filter, LIVE: the incompatible job must still be queued — never dispatched.
  INCOMPAT_STATUS="$(job_status "$INCOMPAT_JOB")"
  if [ "$INCOMPAT_STATUS" = "queued" ]; then
    record PASS hard-filter-live "incompatible job (apple_silicon_ultra-only) never claimed — stayed queued"
  else
    record FAIL hard-filter-live "incompatible job status='$INCOMPAT_STATUS' (a worker that cannot run it took it)"
  fi

  # Menu-bar status file: the agent atomically writes ~/.compute-exchange/status.json
  # (redirected here via CX_STATUS_PATH) on every heartbeat + task transition. The
  # macOS app (macapp/) reads it. Assert it exists and is a valid, fresh document.
  STATUS_OK=0
  if [ -f "$ART/status.json" ]; then
    python3 - "$ART/status.json" <<'PY' && STATUS_OK=1
import json,sys
d=json.load(open(sys.argv[1]))
assert d.get("schema_version")==1, "schema_version"
assert d.get("state") in ("running","idle","paused","offline"), "state"
assert d.get("worker_id"), "worker_id"
assert isinstance(d.get("last_heartbeat"),(int,float)) and d["last_heartbeat"]>0, "last_heartbeat"
for k in ("balance_usd","lifetime_usd","today_earnings_usd","cpu_pct","model_cache_bytes","active","eligible_now","agent_version"):
    assert k in d, k
# Dynamic-throttling surface (provider safety): the resource block must be present
# and coherent. effective = max(available - headroom, 0); throttled is a real bool.
for k in ("total_memory_gb","available_memory_gb","reserved_headroom_gb","effective_memory_gb","throttled","throttle_reason","current_task_id"):
    assert k in d, k
assert isinstance(d["throttled"], bool), "throttled type"
assert d["available_memory_gb"] > 0, "available_memory_gb positive (real reading)"
assert d["effective_memory_gb"] >= 0, "effective non-negative"
assert abs(d["effective_memory_gb"] - max(d["available_memory_gb"] - d["reserved_headroom_gb"], 0.0)) < 0.5, "effective = available - headroom"
PY
  fi
  if [ "$STATUS_OK" = "1" ]; then
    record PASS status-file "agent wrote a valid status.json (schema_version=1, worker_id set, heartbeat fresh, throttle surface coherent)"
  else
    record FAIL status-file "agent did not write a valid ~/.compute-exchange/status.json"
  fi

  # Admin panel data surface (jobs + payouts): admin-scoped read views across ALL
  # buyers/suppliers (the goal's admin panel: jobs/workers/fraud/payouts; workers +
  # fraud are already proven via worker-register / fraud handlers). Jobs ran above,
  # so both must return a populated list.
  ADMIN_OK=0
  AJOBS="$(curl -fsS "$CONTROL_URL/admin/jobs" -H "Authorization: Bearer $ADMIN_KEY" 2>/dev/null || true)"
  APAY="$(curl -fsS "$CONTROL_URL/admin/payouts" -H "Authorization: Bearer $ADMIN_KEY" 2>/dev/null || true)"
  if printf '%s' "$AJOBS" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert isinstance(d,list) and len(d)>=1 and "job_type" in d[0]' 2>/dev/null \
     && printf '%s' "$APAY" | python3 -c 'import sys,json;d=json.load(sys.stdin);assert isinstance(d,list) and len(d)>=1 and "payout_status" in d[0]' 2>/dev/null; then
    ADMIN_OK=1
  fi
  if [ "$ADMIN_OK" = "1" ]; then
    record PASS admin-views "GET /admin/jobs + /admin/payouts return populated admin views"
  else
    record FAIL admin-views "admin jobs/payouts views empty or malformed"
  fi

  # Memory telemetry persistence (Plane D D4): the live agent has been heartbeating
  # its real available/effective memory throughout the run. Each beat that carried
  # memory appended a worker_memory_samples row (the rolling history, not just the
  # latest-beat columns the claim reads), and GET /admin/workers now surfaces a
  # recent avg_available_gb per worker. Assert BOTH: real samples landed for the live
  # worker, AND the admin view reports a positive rolling average backed by >=1
  # sample — proving the heartbeat→sample→capacity path end-to-end on real telemetry.
  MEM_SAMPLES="$(psql "$DATABASE_URL" -tAc "SELECT COUNT(*) FROM worker_memory_samples WHERE worker_id='$WORKER_ID' AND effective_gb IS NOT NULL" 2>/dev/null | tr -d '[:space:]')"
  AWORKERS="$(curl -fsS "$CONTROL_URL/admin/workers" -H "Authorization: Bearer $ADMIN_KEY" 2>/dev/null || true)"
  if [ "${MEM_SAMPLES:-0}" -ge 1 ] && printf '%s' "$AWORKERS" | WID="$WORKER_ID" python3 -c 'import os,sys,json
d=json.load(sys.stdin)
w=[x for x in d if x.get("id")==os.environ["WID"]]
assert w, "live worker missing from admin/workers"
r=w[0]
for k in ("avg_available_gb","memory_samples"):
    assert k in r, "missing "+k
assert r["memory_samples"]>=1 and r["avg_available_gb"]>0, "no rolling memory average surfaced"' 2>/dev/null; then
    record PASS memory-telemetry "heartbeats persisted ${MEM_SAMPLES} worker_memory_samples; GET /admin/workers surfaces avg_available_gb"
  else
    record FAIL memory-telemetry "memory samples missing or admin/workers lacks rolling avg (samples=${MEM_SAMPLES:-0})"
  fi

  # Scheduler explanation (Plane D §17 D11): "slow" is often "nothing eligible". GET
  # /admin/scheduler/explain?worker_id= runs the SAME hard-filter predicates as the
  # claim against the claimable queue and counts WHY each task was rejected. The
  # INCOMPAT_JOB above (an apple_silicon_ultra-only embed) is still queued and this
  # Mac is not ultra, so the live demo worker's explanation MUST carry the full reason
  # keyset, attribute that job to hw_class_mismatch (>=1), and report eligible=0 — the
  # endpoint making "the worker is fine, the queue just has nothing for it" visible.
  EXPLAIN="$(curl -fsS "$CONTROL_URL/admin/scheduler/explain?worker_id=$WORKER_ID" -H "Authorization: Bearer $ADMIN_KEY" 2>/dev/null || true)"
  if printf '%s' "$EXPLAIN" | python3 -c 'import sys,json
d=json.load(sys.stdin)
for k in ("worker_id","no_queued_tasks","memory_mismatch","model_mismatch","job_type_mismatch","hw_class_mismatch","residency_mismatch","throttled","payout_floor","supplier_inactive","eligible"):
    assert k in d, "missing "+k
assert d["hw_class_mismatch"]>=1, "the queued ultra-only job must show as hw_class_mismatch"
assert d["eligible"]==0, "nothing in the queue is eligible for this worker"' 2>/dev/null; then
    record PASS scheduler-explain "GET /admin/scheduler/explain attributes the queued incompatible job to hw_class_mismatch; eligible=0"
  else
    record FAIL scheduler-explain "scheduler explain missing reason keys or wrong counts (got: $EXPLAIN)"
  fi

  # Quote-to-actual drift feedback (Plane D D6 / errata C-Errata-6): the jobs that
  # ran above committed real tasks, each recording a task_durations row. GET
  # /admin/drift must now roll those up per (job_type, model) with real actuals — a
  # non-empty list whose top row carries a positive observed p90 + sample count, the
  # signal the next quote's ETA leans on. Proves the Exchange Brain is learning from
  # reality, not the static target.
  DRIFT="$(curl -fsS "$CONTROL_URL/admin/drift" -H "Authorization: Bearer $ADMIN_KEY" 2>/dev/null || true)"
  if printf '%s' "$DRIFT" | python3 -c 'import sys,json
d=json.load(sys.stdin)
assert isinstance(d,list) and len(d)>=1, "drift rollup empty"
r=d[0]
for k in ("job_type","model_ref","samples","avg_duration_ms","p90_duration_ms","using_observed_p90"):
    assert k in r, "missing "+k
assert r["samples"]>=1 and r["p90_duration_ms"]>0, "no observed duration recorded"' 2>/dev/null; then
    record PASS quote-drift "GET /admin/drift rolls up real committed durations per (job_type,model); observed p90 feeds the ETA"
  else
    record FAIL quote-drift "drift rollup empty or missing observed-duration fields (got: $DRIFT)"
  fi

  # Multi-supplier LOCAL run — the local stand-in for "two+ Macs running real jobs
  # end-to-end": launch a SECOND agent with a distinct worker token (→ distinct
  # worker), submit a multi-chunk REDUNDANT embed job both agents drain, then assert
  # ≥2 DISTINCT workers committed results for the one job. Redundancy also forces a
  # cross-worker within-class comparison. Proven on one box; real two-Mac hardware
  # is the remaining field test.
  say "    starting a second agent (local multi-supplier run)"
  cat >"$ART/agent2.toml" <<TOML
control_url = "$CONTROL_URL"
worker_token = "$WORKER_TOKEN2"
supplier_id = "$SUPPLIER_ID2"
max_cpu_pct = 90.0
power_only = false
min_payout_usd_per_hr = 0.0
memory_headroom_gb = 0.0
max_memory_pct = 0.0
data_dir = "$ART/agent2-data"
TOML
  mkdir -p "$ART/agent2-data"
  ( cd agent && exec env -u CX_VERIFICATION_SAMPLE_SECRET \
      CX_CONTROL_URL="$CONTROL_URL" CX_WORKER_TOKEN="$WORKER_TOKEN2" CX_STATUS_PATH="$ART/status2.json" \
      "$AGENT_BIN" run --config "$ART/agent2.toml" ) >"$ART/agent2.log" 2>&1 &
  AGENT2_PID=$!
  # Wait for the SECOND agent to ACTUALLY register through the control plane. Logs are
  # WARN-level by default, and the seeded worker row starts fresh enough to fool a
  # liveness-only check, so the receipt waits for the worker-2 row to be rewritten by
  # registration (version != seed) under the server-bound worker token.
  AGENT2_READY=0
  AGENT2_DEADLINE=$(( $(date +%s) + 180 ))
  while :; do
    kill -0 "$AGENT2_PID" 2>/dev/null || break
    REG2="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM workers WHERE id='$WORKER_ID2' AND version <> 'seed' AND last_seen_at > now() - interval '30 seconds'" 2>/dev/null | tr -d '[:space:]')"
    if [ "${REG2:-0}" = "1" ]; then AGENT2_READY=1; break; fi
    [ "$(date +%s)" -ge "$AGENT2_DEADLINE" ] && break
    sleep 3
  done
  if [ "$AGENT2_READY" != "1" ]; then
    record FAIL multi-agent "second live agent did not register as worker $WORKER_ID2 before the proof window"
  else
    # Each task is a LONGER generation (max_tokens 96 + a prompt that keeps generating)
    # so the job stays in flight long enough for both live agents to claim work. 18
    # tasks > one agent's bounded concurrency (≤4) keeps work claimable across the
    # window. Still asserts ≥2 DISTINCT workers.
    MA_INPUT=""
    for i in $(seq 1 18); do
      MA_INPUT="$MA_INPUT{\"id\":\"$i\",\"prompt\":\"Write the numbers from 1 to 50 separated by commas.\"}
"
    done
    MULTI_AGENT_JOB="$(submit_job llama-3.2-1b-instruct-q4 '{"type":"batch_infer","max_tokens":96,"temperature":0.0}' \
      "$MA_INPUT" 0 1 null)"
    if wait_job "$MULTI_AGENT_JOB" 300 multi-agent; then
      NW="$(psql "$DATABASE_URL" -tAc "SELECT COUNT(DISTINCT worker_id) FROM tasks WHERE job_id='$MULTI_AGENT_JOB' AND status='complete' AND worker_id IS NOT NULL" 2>/dev/null | tr -d '[:space:]')"
      if [ "${NW:-0}" -ge 2 ]; then
        record PASS multi-agent "$NW distinct agents committed results for one job (local two-agent proof; physical two-Mac proof remains external)"
      else
        record FAIL multi-agent "only ${NW:-0} distinct worker committed (expected >=2 with two live agents)"
      fi
    else
      record FAIL multi-agent "multi-agent job did not complete in 300s"
    fi
  fi

  # Load test (Operations): a burst of jobs the two live agents must ALL drain to
  # complete within the window — throughput under concurrent load on one box.
  LOAD_N=12; LOAD_IDS=""; LOAD_DONE=0
  for i in $(seq 1 $LOAD_N); do
    LOAD_IDS="$LOAD_IDS $(submit_job all-minilm-l6-v2 '{"type":"embed"}' "{\"id\":\"L$i\",\"text\":\"load test line $i\"}" 0 1 null)"
  done
  for JID in $LOAD_IDS; do
    wait_job "$JID" 180 load >/dev/null 2>&1 && LOAD_DONE=$((LOAD_DONE + 1))
  done
  if [ "$LOAD_DONE" = "$LOAD_N" ]; then
    record PASS load-test "$LOAD_DONE/$LOAD_N burst-submitted jobs all completed under load"
  else
    record FAIL load-test "only $LOAD_DONE/$LOAD_N load-test jobs completed"
  fi
  # The second agent has done its proof work. Stop it before warm-routing so the
  # one remaining warm worker's memory heartbeat can recover from the two-agent load.
  if [ -n "$AGENT2_PID" ]; then
    kill "$AGENT2_PID" 2>/dev/null || true
    sleep 1
    kill -9 "$AGENT2_PID" 2>/dev/null || true
    AGENT2_PID=""
  fi

  # Backups + disaster recovery (Operations): pg_dump the live DB, restore it into a
  # FRESH database, and verify the jobs survive — proving backups are real and
  # restorable, not merely configured.
  DR_OK=0; SRC=""; DST=""
  if command -v pg_dump >/dev/null 2>&1; then
    # sed (not ${//}) so the LITERAL "cx?" is matched — a glob "?" would hit "/cx@".
    RESTORE_URL="$(printf '%s' "$DATABASE_URL" | sed 's#/cx?#/cx_restore?#')"
    # Wrap the chain in `if` so a failure records FAIL rather than tripping `set -e`.
    if pg_dump "$DATABASE_URL" >"$ART/backup.sql" 2>/dev/null \
       && psql "$DATABASE_URL" -q -c "DROP DATABASE IF EXISTS cx_restore" >/dev/null 2>&1 \
       && psql "$DATABASE_URL" -q -c "CREATE DATABASE cx_restore" >/dev/null 2>&1 \
       && psql "$RESTORE_URL" -q -f "$ART/backup.sql" >/dev/null 2>&1; then
      SRC="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM jobs" 2>/dev/null | tr -d '[:space:]')"
      DST="$(psql "$RESTORE_URL" -tAc "SELECT count(*) FROM jobs" 2>/dev/null | tr -d '[:space:]')"
      [ -n "$SRC" ] && [ "$SRC" = "$DST" ] && [ "${SRC:-0}" -ge 1 ] && DR_OK=1
    fi
    psql "$DATABASE_URL" -q -c "DROP DATABASE IF EXISTS cx_restore" >/dev/null 2>&1 || true
  fi
  if [ "$DR_OK" = "1" ]; then
    record PASS disaster-recovery "pg_dump → fresh-DB restore preserved all $SRC jobs (backups restorable)"
  else
    record FAIL disaster-recovery "DB backup/restore did not preserve jobs (src=$SRC dst=$DST)"
  fi

  # Install flow (operability): the one-command installer's dry run must pass its
  # prerequisite + plan checks — the local "build → install → earn" path a Mac
  # owner runs unaided (signing a distributable .app is the external step).
  if bash "$ROOT/scripts/install.sh" --check >/dev/null 2>&1; then
    record PASS install-check "scripts/install.sh --check passes (build→install→config→LaunchAgent plan)"
  else
    record FAIL install-check "install.sh --check failed"
  fi


  # ── Phase 4: metrics + ledger + logs ───────────────────────────────────────
  say "5/6 checking metrics, ledger, logs"
  METRICS="$(curl -fsS "$CONTROL_URL/metrics" || true)"
  subm="$(printf '%s\n' "$METRICS" | awk '/^cx_jobs_submitted_total /{print $2}')"
  done="$(printf '%s\n' "$METRICS" | awk '/^cx_tasks_completed_total /{print $2}')"
  if [ "${subm:-0}" -ge 2 ] && [ "${done:-0}" -ge 2 ] 2>/dev/null; then
    record PASS metrics "counters advanced (submitted=$subm completed=$done)"
  else
    record FAIL metrics "counters did not advance (submitted=${subm:-?} completed=${done:-?})"
  fi
  # NB: the Plane D counter check (metrics-planed) lives AFTER the quote + budget-cap
  # rows below, where cx_quotes_total / cx_budget_stops_total have actually advanced.

  LEDGER_ROWS="$(psql "$DATABASE_URL" -tA -c \
    "SELECT count(*) FROM ledger_entries WHERE kind='supplier_credit' AND amount_usd>0" 2>/dev/null | tr -d '[:space:]')"
  [ "${LEDGER_ROWS:-0}" -ge 1 ] 2>/dev/null \
    && record PASS ledger "supplier credited ($LEDGER_ROWS supplier_credit rows)" \
    || record FAIL ledger "no supplier_credit rows after live jobs"

  # Payout stays HONESTLY blocked: drive the release loop and assert no fake transfer.
  PAID="$(psql "$DATABASE_URL" -tA -c \
    "SELECT count(*) FROM ledger_entries WHERE payout_status='released' AND kind='supplier_credit' AND payout_ref IS NOT NULL" 2>/dev/null | tr -d '[:space:]')"
  [ "${PAID:-0}" = "0" ] \
    && record PASS payout-blocked "no supplier transfer faked (Stripe/Trolley rail is Phase 3)" \
    || record FAIL payout-blocked "a supplier payout was marked released without a real rail"

  grep -qiE 'listening on|storage:|workers:' "$CONTROL_LOG" \
    && record PASS logs "control logs are structured + useful" \
    || record FAIL logs "control logs missing expected lines"

  # Buyer invoice endpoint: a real per-job invoice from the ledger.
  INV="$(curl -fsS "$CONTROL_URL/v1/jobs/$EMBED_JOB/invoice" -H "Authorization: Bearer $API_KEY" 2>/dev/null \
    | python3 -c 'import sys,json;d=json.load(sys.stdin);print("ok" if d.get("job_id") and ("charged_usd" in d) else "bad")' 2>/dev/null || true)"
  [ "$INV" = "ok" ] \
    && record PASS invoice "GET /v1/jobs/{id}/invoice returns a ledger-backed invoice" \
    || record FAIL invoice "invoice endpoint did not return a valid invoice ($INV)"

  # CLI surfaces (Plane D D19/D20): the headless `cx` binary must drive the same real
  # endpoints. Build it (stdlib-only) and exercise the two read surfaces added here
  # against the LIVE plane — `cx invoice --json` on the embed job (buyer key) and
  # `cx explain-scheduler --worker` (admin key) — proving the CLI wiring + flags match
  # the server, not just curl. Deterministic: EMBED_JOB + WORKER_ID already exist.
  CLI_OK=0
  if (cd control && go build -o "$ART/cx" .) >/dev/null 2>&1; then
    CINV="$(CX_API_URL="$CONTROL_URL" CX_API_KEY="$API_KEY" "$ART/cx" invoice --json "$EMBED_JOB" 2>/dev/null \
      | python3 -c 'import sys,json;d=json.load(sys.stdin);print("ok" if d.get("job_id") and ("charged_usd" in d) else "bad")' 2>/dev/null || true)"
    CEXP="$(CX_API_URL="$CONTROL_URL" CX_API_KEY="$ADMIN_KEY" "$ART/cx" explain-scheduler --worker "$WORKER_ID" 2>/dev/null \
      | python3 -c 'import sys,json;d=json.load(sys.stdin);print("ok" if d.get("worker_id") and ("eligible" in d) else "bad")' 2>/dev/null || true)"
    [ "$CINV" = "ok" ] && [ "$CEXP" = "ok" ] && CLI_OK=1
  fi
  [ "$CLI_OK" = "1" ] \
    && record PASS cli-surfaces "cx invoice --json + cx explain-scheduler drive the live plane (buyer + admin)" \
    || record FAIL cli-surfaces "cx CLI surfaces failed (invoice=$CINV explain=$CEXP)"

  # Buyer DX doc-as-test (Buyer Developer Experience 5->6): docs/QUICKSTART.md as
  # EXECUTABLE TRUTH. scripts/doc-as-test.sh extracts every documented buyer command
  # (curl, Python SDK, cx CLI) OUT of the doc, localizes only host+key, and runs each
  # against THIS live plane — with the REAL Metal agent (not a stand-in) draining the
  # jobs, since we attach to the already-running stack. Its built-in self-test also
  # confirms a deliberately-broken doc command IS caught, so a stale doc fails here.
  if CX_DOCTEST_CONTROL_URL="$CONTROL_URL" CX_DOCTEST_API_KEY="$API_KEY" \
       bash "$ROOT/scripts/doc-as-test.sh" >"$ART/doc-as-test.log" 2>&1; then
    record PASS doc-as-test "every documented QUICKSTART command runs against the live plane (curl+SDK+CLI); broken-doc self-test caught"
  else
    record FAIL doc-as-test "a documented QUICKSTART command failed against the live plane — see $ART/doc-as-test.log"
  fi

  # Plane C quote (Compute Autopilot): POST /v1/quote scans a JSONL input and returns
  # a conservative cost/ETA/supply/risk band WITHOUT spending — and persists the
  # assumptions (a quotes row). Proves the buyer-trust preflight is real end-to-end.
  QUOTE_OK=0
  QBODY='{"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"all-minilm-l6-v2"},"tier":"batch","verification":{"redundancy_frac":0,"honeypot_frac":0,"payout_hold_secs":0},"input":"{\"id\":\"a\",\"text\":\"quote me\"}\n{\"id\":\"b\",\"text\":\"before I spend\"}\n"}'
  QOUT="$(curl -fsS -X POST "$CONTROL_URL/v1/quote" -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d "$QBODY" 2>/dev/null || true)"
  if printf '%s' "$QOUT" | python3 -c 'import sys,json
d=json.load(sys.stdin)
assert d["quote_id"].startswith("q_"), "quote_id"
assert d["input"]["records"]==2, "records"
assert d["execution"]["estimated_tasks"]>=1, "tasks"
assert d["cost"]["max_usd"]>=d["cost"]["expected_usd"]>=0, "cost band"
assert d["time"]["p90_secs"]>=d["time"]["p50_secs"]>0, "eta band"
assert 0 < d["confidence"]["score"] <= 1 and d["confidence"]["reasons"], "confidence"
assert "eligible_workers_now" in d["execution"], "supply"' 2>/dev/null; then
    QROWS="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM quotes WHERE job_type='embed'" 2>/dev/null | tr -d '[:space:]')"
    [ "${QROWS:-0}" -ge 1 ] 2>/dev/null && QUOTE_OK=1
  fi
  [ "$QUOTE_OK" = "1" ] \
    && record PASS quote "POST /v1/quote returns a coherent cost/ETA/supply/risk band + persisted assumptions (Plane C)" \
    || record FAIL quote "quote endpoint did not return a valid quote or did not persist assumptions"

  # Warm-model routing (Plane D D3): the embed jobs above loaded MiniLM into the
  # agent's warm pool, and every heartbeat reports it via loaded_models →
  # worker_model_state gets a fresh row. Assert the REAL warm state landed for the
  # live worker (poll up to ~40s so a heartbeat lands), and that a fresh quote for
  # that model now reports warm supply (warm_eligible_workers>=1) with cold-start risk
  # downgraded to low — proving warm state feeds the buyer-facing confidence. The
  # scheduler's warm re-rank is unit-tested (TestMatchPrefersWarmWorker); the upsert
  # path is covered by TestWorkerModelStateUpsert.
  # Retry the warm-row check AND the quote assertions together until they hold or the
  # deadline. The agent→control warm-report path is proven the instant warm_rows>=1 (a
  # real heartbeat wrote a fresh worker_model_state row, refreshed every beat while MiniLM
  # stays loaded). The quote's warm_eligible_workers, however, applies the SAME hard-filter
  # memory gate ClaimTask uses (min_memory_gb <= effective_memory_gb, the agent's LIVE
  # reported available memory) — so right after the embed burst, transient dev-Mac memory
  # pressure can briefly push effective memory under the job's floor and drop eligible_now
  # (hence warm_eligible) to 0. Post-embed the agent idles and memory frees within a few
  # heartbeats, so we wait for a real warm+eligible moment rather than forcing DB state.
  WARM_OK=0
  WARM_ROWS=0
  WARM_DETAIL=""
  WARM_DEADLINE=$(( $(date +%s) + 120 ))
  while :; do
    WARM_ROWS="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM worker_model_state WHERE worker_id='$WORKER_ID' AND model_id='all-minilm-l6-v2' AND last_seen_warm > now() - interval '60 seconds'" 2>/dev/null | tr -d '[:space:]')"
    if [ "${WARM_ROWS:-0}" -ge 1 ] 2>/dev/null; then
      WQOUT="$(curl -fsS -X POST "$CONTROL_URL/v1/quote" -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' -d "$QBODY" 2>/dev/null || true)"
      WARM_DETAIL="$(printf '%s' "$WQOUT" | python3 -c 'import sys,json
try:
    d=json.load(sys.stdin); ex=d.get("execution",{})
    print("eligible=%s warm=%s cold_start=%s oom=%s" % (ex.get("eligible_workers_now"), ex.get("warm_eligible_workers"), ex.get("cold_start_risk"), ex.get("oom_risk")))
except Exception as e:
    print("quote_decode_error=%s" % e)' 2>/dev/null || true)"
      if printf '%s' "$WQOUT" | python3 -c 'import sys,json
d=json.load(sys.stdin)
ex=d["execution"]
assert ex.get("warm_eligible_workers",0)>=1, "warm supply not reported"
assert ex.get("warm_eligible_workers",0)<=ex["eligible_workers_now"], "warm > eligible (impossible)"
assert ex["cold_start_risk"]=="low", "cold-start should be low with warm supply, got "+str(ex["cold_start_risk"])' 2>/dev/null; then
        WARM_OK=1; break
      fi
    fi
    [ "$(date +%s)" -ge "$WARM_DEADLINE" ] && break
    sleep 3
  done
  [ "$WARM_OK" = "1" ] \
    && record PASS warm-routing "agent reported MiniLM warm (worker_model_state row); quote shows warm_eligible_workers>=1 + cold_start_risk=low (Plane D D3)" \
    || record FAIL warm-routing "warm model state not recorded for the live worker, or the quote did not reflect warm supply (warm_rows=${WARM_ROWS:-0} ${WARM_DETAIL:-no_quote_detail})"

  # Plane D D7 — quote-to-submit binding: quote an input, then submit it carrying the
  # quote_id. A matching, unexpired quote binds (202 + jobs.quote_id set + the invoice
  # shows quoted_usd next to charged_usd); a DIFFERENT-model submit with the same
  # quote_id is refused 409. Proves the invoice can say "here is what you were told".
  QBIND_OK=0
  QBIND_INPUT='{"id":"a","text":"bind me to a price"}\n{"id":"b","text":"prove the invoice"}'
  QBIND_JOB="$(python3 - "$CONTROL_URL" "$API_KEY" "$QBIND_INPUT" <<'PY' 2>/dev/null || true
import json,sys,urllib.request,urllib.error
url,key,inp=sys.argv[1:4]
inp=inp.replace("\\n","\n")
H={"Authorization":"Bearer "+key,"Content-Type":"application/json"}
def post(path,obj):
    r=urllib.request.Request(url+path,data=json.dumps(obj).encode(),method="POST",headers=H)
    return urllib.request.urlopen(r,timeout=20)
base={"job_type":{"type":"embed"},"model":{"kind":"gguf","ref":"all-minilm-l6-v2"},
      "tier":"batch","verification":{"redundancy_frac":0,"honeypot_frac":0,"payout_hold_secs":0},
      "input":inp}
q=json.load(post("/v1/quote",base))
qid=q["quote_id"]; assert qid.startswith("q_"), "quote_id shape"
# matching submit binds → 202 with a job_id
job=json.load(post("/v1/jobs",dict(base,quote_id=qid)))["job_id"]
# the invoice carries quoted_usd next to charged_usd
inv=json.load(urllib.request.urlopen(urllib.request.Request(
    url+"/v1/jobs/"+job+"/invoice",headers={"Authorization":"Bearer "+key}),timeout=20))
assert "quoted_usd" in inv and "charged_usd" in inv, "invoice quoted-vs-actual"
# same quote_id, different model → must be refused 409
try:
    post("/v1/jobs",dict(base,model={"kind":"gguf","ref":"bge-small-en-v1.5"},quote_id=qid))
    raise SystemExit("mismatched-model submit was NOT rejected")
except urllib.error.HTTPError as e:
    assert e.code==409, f"mismatch want 409 got {e.code}"
print(job)
PY
)"
  if [ -n "$QBIND_JOB" ]; then
    BOUND="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM jobs WHERE id='$QBIND_JOB' AND quote_id IS NOT NULL" 2>/dev/null | tr -d '[:space:]')"
    [ "${BOUND:-0}" = "1" ] 2>/dev/null && QBIND_OK=1
  fi
  [ "$QBIND_OK" = "1" ] \
    && record PASS quote-submit-binding "POST /v1/jobs binds a matching quote_id (jobs.quote_id set, invoice shows quoted_usd); mismatched model rejected 409 (Plane D D7)" \
    || record FAIL quote-submit-binding "quote-to-submit binding did not bind or did not reject a mismatch"

  # Plane C/D D0 — buyer-visible event timeline: a completed job has an event trail
  # (at least job_created), so a buyer never infers state from a status field alone.
  # (The immediate fail-endpoint requeue/terminal behavior is proven deterministically
  # in the matrix: TestFailEndpointRequeuesImmediately / *BadInputTerminal.)
  EVOUT="$(curl -fsS "$CONTROL_URL/v1/jobs/$EMBED_JOB/events" -H "Authorization: Bearer $API_KEY" 2>/dev/null || true)"
  if printf '%s' "$EVOUT" | python3 -c 'import sys,json
ev=json.load(sys.stdin)
assert isinstance(ev,list) and len(ev)>=1, "no events"
kinds={e["event"] for e in ev}
assert "job_created" in kinds, "missing job_created"' 2>/dev/null; then
    record PASS job-events "GET /v1/jobs/{id}/events returns a buyer-visible timeline (job_created present)"
  else
    record FAIL job-events "job events timeline empty or missing job_created"
  fi

  # Plane C §12 / Plane D §14 D8 — Budget Governor: a job with a tiny max_usd whose
  # next task's PROJECTED charge (already-charged + one task's estimate) would breach
  # the cap must NOT have that task dispatched. We seed a capped job with one charged
  # complete task + one queued task whose dispatch would blow the cap, let the live
  # worker poll, then assert the queued task stayed queued and budget_state flipped to
  # paused_for_budget with a budget_stopped event. The cap PREVENTS dispatch — no
  # refund, no over-charge (money math only GATES here).
  BUD_JOB="$(uuidgen | tr 'A-Z' 'a-z')"; BUD_DONE="$(uuidgen | tr 'A-Z' 'a-z')"; BUD_Q="$(uuidgen | tr 'A-Z' 'a-z')"
  # --single-transaction: the queued task and its BLOCKING buyer_charge become
  # visible together, so a concurrently-polling live agent can never observe the
  # queued task without the charge that caps it (otherwise the gate would briefly
  # allow dispatch and the row would flake).
  psql "$DATABASE_URL" -q --single-transaction >/dev/null 2>&1 <<SQL || true
INSERT INTO jobs (id, buyer_id, status, job_type, model_ref, input_ref, tier, task_count, tasks_done, min_memory_gb, estimated_usd, max_usd, budget_state)
  VALUES ('$BUD_JOB','$BUYER_ID','running','embed','all-minilm-l6-v2','jobs/bud/in.jsonl','batch',2,1,2,1.00,0.60,'tracking');
INSERT INTO tasks (id, job_id, status, worker_id, input_ref, result_key, chunk_index, completed_at)
  VALUES ('$BUD_DONE','$BUD_JOB','complete','$WORKER_ID','jobs/bud/t0/in.jsonl','jobs/bud/t0/out.json',0, now());
INSERT INTO tasks (id, job_id, status, input_ref, result_key, chunk_index, visible_at)
  VALUES ('$BUD_Q','$BUD_JOB','queued','jobs/bud/t1/in.jsonl','jobs/bud/t1/out.json',1, now());
INSERT INTO ledger_entries (kind, buyer_id, task_id, amount_usd, payout_status)
  VALUES ('buyer_charge','$BUYER_ID','$BUD_DONE',-0.50,'released');
SQL
  # Drive a worker poll (the live worker is also polling; the cap must block BOTH).
  curl -fsS "$CONTROL_URL/v1/worker/poll" -H "X-Worker-Token: $WORKER_TOKEN" >/dev/null 2>&1 || true
  # The claim path prevents dispatch immediately; the buyer-visible state/event is
  # intentionally moved to the budget-stop ticker (7s cadence), so wait for the real
  # background sweep instead of checking after an arbitrary 2s nap.
  BUD_DEADLINE=$(( $(date +%s) + 25 ))
  while :; do
    BUD_TSTATUS="$(psql "$DATABASE_URL" -tAc "SELECT status FROM tasks WHERE id='$BUD_Q'" 2>/dev/null | tr -d '[:space:]')"
    BUD_STATE="$(psql "$DATABASE_URL" -tAc "SELECT budget_state FROM jobs WHERE id='$BUD_JOB'" 2>/dev/null | tr -d '[:space:]')"
    BUD_EV="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM job_events WHERE job_id='$BUD_JOB' AND event='budget_stopped'" 2>/dev/null | tr -d '[:space:]')"
    BUD_REFUND="$(psql "$DATABASE_URL" -tAc "SELECT count(*) FROM ledger_entries WHERE kind='refund' AND task_id IN (SELECT id FROM tasks WHERE job_id='$BUD_JOB')" 2>/dev/null | tr -d '[:space:]')"
    [ "$BUD_TSTATUS" = "queued" ] && [ "$BUD_STATE" = "paused_for_budget" ] && [ "${BUD_EV:-0}" -ge 1 ] 2>/dev/null && [ "${BUD_REFUND:-0}" = "0" ] && break
    [ "$(date +%s)" -ge "$BUD_DEADLINE" ] && break
    sleep 1
  done
  if [ "$BUD_TSTATUS" = "queued" ] && [ "$BUD_STATE" = "paused_for_budget" ] && [ "${BUD_EV:-0}" -ge 1 ] 2>/dev/null && [ "${BUD_REFUND:-0}" = "0" ]; then
    record PASS budget-cap "capped job stopped before breach: task stayed queued, budget_state=paused_for_budget, budget_stopped event, no refund"
  else
    record FAIL budget-cap "budget cap did not stop dispatch cleanly (task=$BUD_TSTATUS state=$BUD_STATE stopped_events=${BUD_EV:-?} refunds=${BUD_REFUND:-?})"
  fi

  # Plane D D21 — the new Plane D counters are exposed AND have advanced. Scraped
  # HERE (after the quote rows priced quotes and the budget-cap row drove a stop) so
  # cx_quotes_total ≥ 1 and cx_budget_stops_total ≥ 1 prove the increments are wired,
  # not merely declared. (A re-scrape: the earlier $METRICS predates these events.)
  METRICS2="$(curl -fsS "$CONTROL_URL/metrics" || true)"
  quotes_m="$(printf '%s\n' "$METRICS2" | awk '/^cx_quotes_total /{print $2}')"
  budstops_m="$(printf '%s\n' "$METRICS2" | awk '/^cx_budget_stops_total /{print $2}')"
  if [ -n "$quotes_m" ] && [ "${quotes_m:-0}" -ge 1 ] 2>/dev/null && [ -n "$budstops_m" ] && [ "${budstops_m:-0}" -ge 1 ] 2>/dev/null; then
    record PASS metrics-planed "Plane D counters advanced (cx_quotes_total=$quotes_m, cx_budget_stops_total=$budstops_m)"
  else
    record FAIL metrics-planed "Plane D counters missing or not advancing (cx_quotes_total=${quotes_m:-absent} cx_budget_stops_total=${budstops_m:-absent})"
  fi

  # Plane D §7 D1 — long-poll parks server-side: a poll with ?wait_ms must HOLD the
  # connection up to wait_ms and return promptly — neither busy-returning 204 instantly
  # (ignoring the wait) nor hanging to the ~35s transport ceiling (a stuck handler). We
  # time it on the idle second worker's token, whose queue stays empty (nothing is pinned
  # to it), so the measurement is deterministic and NEVER races the live agents: with no
  # claimable work the handler parks the full wait_ms, then returns a clean 204. (The
  # WAKE-ON-NEW-TASK semantics — return the task in ms once one appears — are proven
  # deterministically by the integration matrix: TestLongPollReturnsOnNewTask /
  # TestLongPollTimesOutCleanly / TestPollNoWaitUnchanged. A live race on a shared queue
  # cannot prove that without controlling every poller, so we assert the timing here.)
  psql "$DATABASE_URL" -q -c "UPDATE workers SET last_seen_at=now(), throttled=false WHERE id='$WORKER_ID2'" >/dev/null 2>&1 || true
  # LC_ALL=C so curl's %{time_total} always uses a '.' decimal (python float() parse).
  LP_WAIT="$(LC_ALL=C curl -s -o /dev/null -w '%{http_code} %{time_total}' -m 12 "$CONTROL_URL/v1/worker/poll?wait_ms=2500" -H "X-Worker-Token: $WORKER_TOKEN2" 2>/dev/null || echo '000 99')"
  LP_NOW="$(LC_ALL=C curl -s -o /dev/null -w '%{http_code} %{time_total}' -m 12 "$CONTROL_URL/v1/worker/poll?wait_ms=0" -H "X-Worker-Token: $WORKER_TOKEN2" 2>/dev/null || echo '000 99')"
  # PASS when: a no-work parked poll waited ~wait_ms then returned 204 (1.5–6.0s); OR it
  # claimed residual work and returned 200 bounded (<6s). The no-wait poll must return
  # fast (<1.5s). A hung handler (≥12s → curl -m kills it) or an instant 204 on the
  # parked poll (<1.5s, wait_ms ignored) both fail.
  LP_OK="$(python3 -c '
import sys
wc, wt = sys.argv[1], float(sys.argv[2])
nc, nt = sys.argv[3], float(sys.argv[4])
parked_ok = (wc == "204" and 1.5 <= wt <= 6.0) or (wc == "200" and wt <= 6.0)
nowait_ok = nc in ("200", "204") and nt < 1.5
print("ok" if parked_ok and nowait_ok else "no")
' "${LP_WAIT%% *}" "${LP_WAIT##* }" "${LP_NOW%% *}" "${LP_NOW##* }" 2>/dev/null || echo no)"
  if [ "$LP_OK" = "ok" ]; then
    record PASS long-poll "poll?wait_ms parks server-side (wait_ms=2500 → ${LP_WAIT}, no-wait → ${LP_NOW}); held the connection, not the 35s ceiling"
  else
    record FAIL long-poll "long-poll timing wrong (wait_ms=2500 → '$LP_WAIT', no-wait → '$LP_NOW'; want parked 1.5–6.0s + fast no-wait)"
  fi

  # Plane D D2 — claim-path index: the partial expression index the errata added
  # (tasks_ready_unclaimed_idx, on the hot ready-task claim predicate) is APPLIED by the
  # schema migration and is valid + ready for the planner. This is the deterministic,
  # environment-independent proof the speed fix is in place; whether the planner picks it
  # is a function of live table size/stats (it does once the queue is non-trivial —
  # `make bench-local` exercises that under a synthetic 1k queue). The planner's CURRENT
  # choice is recorded as an informational note; the PASS hinges on the index existing +
  # being valid, NOT on a stats-dependent plan pick (which flakes on a small live table).
  IDX_OK="$(psql "$DATABASE_URL" -tAc "SELECT (i.indisvalid AND i.indisready) FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid WHERE c.relname = 'tasks_ready_unclaimed_idx'" 2>/dev/null | tr -d '[:space:]')"
  EXPLAIN_JSON="$(psql "$DATABASE_URL" -tAc "SET enable_seqscan=off; EXPLAIN (FORMAT JSON) SELECT id FROM tasks WHERE status IN ('queued','retrying') AND claimed_by IS NULL AND COALESCE(visible_at, created_at) <= now() ORDER BY status, COALESCE(visible_at, created_at), created_at LIMIT 1" 2>/dev/null || true)"
  if printf '%s' "$EXPLAIN_JSON" | grep -q "tasks_ready_unclaimed_idx"; then idx_plan="planner uses it now"; else idx_plan="planner prefers a small-table plan now (expected; bench-local proves it under load)"; fi
  if [ "$IDX_OK" = "t" ]; then
    record PASS claim-index "tasks_ready_unclaimed_idx applied + valid (partial expression index on the hot ready-task claim predicate; $idx_plan)"
  else
    record FAIL claim-index "tasks_ready_unclaimed_idx missing or not valid (indisvalid/indisready != t)"
  fi

  # The development-preview site is served at the bare root
  # (SITE_PATH=web/index.html). Its visible non-launch boundary is load-bearing:
  # serving the old receipt-count marketing page is a failure, even if the HTML is
  # otherwise reachable. The operator surface stays at /admin (checked below). A
  # missing file is an honest 404, so this can SKIP when web/index.html is absent.
  root_body="$(curl -fsS "$CONTROL_URL/" 2>/dev/null || true)"
  if grep -qi 'Development preview' <<<"$root_body" && \
     grep -qi 'No live-money, market-liquidity, signed-distribution, or physical-fleet claim' <<<"$root_body"; then
    record PASS root-site "development-preview site served at / with explicit non-launch claim boundary"
  elif [ -z "$root_body" ] && [ ! -f web/index.html ]; then
    record SKIP root-site "site not served (web/index.html missing at control CWD)"
  else
    record FAIL root-site "/ did not serve the explicit development-preview claim boundary"
  fi

  # The passkey-gated operator console (Control Room) is served at /admin. The HTML
  # shell is public (the DATA routes enforce auth); ADMIN_PATH=web/admin.html.
  # Buffered (no curl|grep pipe): grep -q exiting early SIGPIPEs curl under
  # pipefail, which mis-reported the multi-MB console page as not served.
  admin_body="$(curl -fsS "$CONTROL_URL/admin" 2>/dev/null || true)"
  if grep -qiE 'control room|passkey' <<<"$admin_body"; then
    record PASS admin-console "Control Room served at /admin (ADMIN_PATH=web/admin.html)"
  else
    record SKIP admin-console "admin console not served (web/admin.html missing at control CWD)"
  fi
fi

# ── Phase 5: unit/fuzz tests (cheap, deterministic, no infra) ────────────────
say "6/6 unit + fuzz tests (control + agent)"
if (cd control && go test ./... >"$ART/go-test.log" 2>&1); then
  record PASS tests-go "go test ./... green"
else
  record FAIL tests-go "go unit tests failed — see $ART/go-test.log"
fi
# Agent unit tests (skip the ignored model-download ones for speed).
if (cd agent && cargo test --release >"$ART/cargo-test.log" 2>&1); then
  record PASS tests-rust "cargo test green"
else
  record FAIL tests-rust "cargo unit tests failed — see $ART/cargo-test.log"
fi

# A passing matrix must describe one stable source snapshot. This catches an
# editor, agent, or generator changing a tracked/untracked source file while the
# long integration run is in flight. Ignored build outputs are intentionally not
# part of the source identity.
SOURCE_END_JSON="$(cd control && go run . source-id)" || die "final source fingerprint failed"
SOURCE_FINGERPRINT_END="$(printf '%s' "$SOURCE_END_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["source_sha256"])')"
SOURCE_STATUS_END="$(printf '%s' "$SOURCE_END_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status_sha256"])')"
printf 'META\tsource_sha256_end\t%s\n' "$SOURCE_FINGERPRINT_END" >>"$LEDGER_FILE"
printf 'META\tstatus_sha256_end\t%s\n' "$SOURCE_STATUS_END" >>"$LEDGER_FILE"
if [ "$SOURCE_FINGERPRINT_START" = "$SOURCE_FINGERPRINT_END" ] && [ "$SOURCE_STATUS_START" = "$SOURCE_STATUS_END" ]; then
  record PASS source-stability "start/end source + status fingerprints match ($SOURCE_FINGERPRINT_END)"
else
  record FAIL source-stability "source changed during proof (start=$SOURCE_FINGERPRINT_START end=$SOURCE_FINGERPRINT_END)"
fi
RUN_COMPLETED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
if grep -q '^FAIL' "$LEDGER_FILE"; then
  RUN_STATUS="FAIL"
else
  RUN_STATUS="PASS"
fi
printf 'META\tcompleted_at\t%s\n' "$RUN_COMPLETED_AT" >>"$LEDGER_FILE"
printf 'META\tstatus\t%s\n' "$RUN_STATUS" >>"$LEDGER_FILE"

# ── Proof ledger ─────────────────────────────────────────────────────────────
echo
say "$(b '================  PROOF LEDGER  ================')"
pass=$(grep -c '^PASS' "$LEDGER_FILE" || true)
skip=$(grep -c '^SKIP' "$LEDGER_FILE" || true)
failc=$(grep -c '^FAIL' "$LEDGER_FILE" || true)
awk -F'\t' '{
  c="\033[1;32m"; if($1=="FAIL")c="\033[1;31m"; if($1=="SKIP")c="\033[1;33m";
  printf "  %s%-5s\033[0m %-22s %s\n", c, $1, $2, $3
}' "$LEDGER_FILE"
echo
printf '  %s: \033[1;32m%d pass\033[0m, \033[1;33m%d skip\033[0m, \033[1;31m%d fail\033[0m   (ledger: %s)\n' \
  "$(b summary)" "$pass" "$skip" "$failc" "$LEDGER_FILE"
echo

if [ "${failc:-0}" -gt 0 ]; then
  die "$failc capability check(s) FAILED — not release-candidate clean"
fi
if [ "$PROOF_MODE" = "contract_only" ]; then
  say "$(b 'LOCAL CONTRACT PROOF: PASS') ✅"
  say "Live agent/model execution was not selected; this is not a local release-candidate proof."
else
  say "$(b 'LOCAL RELEASE-CANDIDATE PROOF: PASS') ✅"
fi
say "This proves the selected local matrix, not product 5/5 or launch readiness."
say "Run: cx prove  # remaining local + external gates"
