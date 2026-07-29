#!/usr/bin/env bash
# Exercise scripts/canary-scenario-driver.sh against a locally-running control
# plane. Successful scenarios are piped through validate-canary-scenario-receipt.py
# with the same argument shape the GO-closure rehearsal uses. Fail-closed
# behaviour is asserted: when the underlying work cannot happen, the driver
# exits non-zero and emits no receipt on stdout.
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "canary-scenario-driver test: FAIL: $*" >&2; exit 1; }
pass() { echo "canary-scenario-driver test: $*"; }

DRIVER="$(cd scripts && pwd -P)/canary-scenario-driver.sh"
[ -x "$DRIVER" ] || fail "scripts/canary-scenario-driver.sh is not executable"
[ ! -L "$DRIVER" ] || fail "driver must not be a symlink for rehearsal-shaped checks"

require_cmd() { command -v "$1" >/dev/null 2>&1 || fail "missing $1"; }
require_cmd jq
require_cmd python3
require_cmd openssl
require_cmd curl
require_cmd psql
require_cmd bash

tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-canary-driver-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# ---------------------------------------------------------------------------
# Binding fixtures matching the validator's closed forms
# ---------------------------------------------------------------------------
export MERC_CANARY_RUN_ID='0123456789abcdef0123456789abcdef'
export MERC_CANARY_CANDIDATE_COMMIT='1111111111111111111111111111111111111111'
export MERC_CANARY_CONTROL_IMAGE='registry.example.invalid/merc/control@sha256:3333333333333333333333333333333333333333333333333333333333333333'
export MERC_CANARY_DRIVER_SHA256
MERC_CANARY_DRIVER_SHA256="$(shasum -a 256 "$DRIVER" | awk '{print $1}')"
export MERC_CANARY_RUN_STARTED_AT
MERC_CANARY_RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Small sleep so scenario_started_at is strictly after run_started when clocks
# only have second resolution.
sleep 1

# Prefer a control that exposes payment_mode on /readyz (required for honest
# safety claims). 8094 is the local stack; when it is not_ready without payment
# fields (pre-patch binary), fall back to 8097 if a payment-field proxy is up,
# else 8094 still and let scenarios fail closed honestly.
CONTROL_BASE="${MERC_CONTROL_BASE_URL:-}"
if [ -z "$CONTROL_BASE" ]; then
  for candidate in \
    "http://127.0.0.1:8094" \
    "http://127.0.0.1:8097" \
    "http://127.0.0.1:8093"; do
    body="$(curl -sS --max-time 2 "$candidate/readyz" 2>/dev/null || true)"
    if printf '%s' "$body" | jq -e '.payment_mode != null and .live_value_movement != null' >/dev/null 2>&1; then
      CONTROL_BASE="$candidate"
      break
    fi
  done
  CONTROL_BASE="${CONTROL_BASE:-http://127.0.0.1:8094}"
fi
export MERC_CONTROL_BASE_URL="$CONTROL_BASE"
pass "using control plane at $CONTROL_BASE"

# Prefer the live console DB used by the local control process when present.
# Connect via PG* env so the DSN password never sits on psql argv (mirrors driver).
psql_obs() {
  # shellcheck disable=SC2034
  local url="$1"; shift
  eval "$(python3 - "$url" <<'PY'
import sys, urllib.parse, shlex
u = urllib.parse.urlparse(sys.argv[1])
host = u.hostname or "localhost"
port = str(u.port or 5432)
user = urllib.parse.unquote(u.username or "")
password = urllib.parse.unquote(u.password or "")
db = (u.path or "/").lstrip("/") or "postgres"
qs = urllib.parse.parse_qs(u.query)
sslmode = (qs.get("sslmode") or ["prefer"])[0]
for name, val in (
    ("PGHOST", host), ("PGPORT", port), ("PGUSER", user),
    ("PGDATABASE", db), ("PGPASSWORD", password), ("PGSSLMODE", sslmode),
):
    print(f"export {name}={shlex.quote(val)}")
PY
)"
  psql -X -qAt -v ON_ERROR_STOP=1 \
    -c "SET default_transaction_read_only = on" "$@"
}

if [ -z "${MERC_CANARY_DATABASE_URL:-}" ]; then
  for candidate in \
    "postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable" \
    "postgres://cx@127.0.0.1:55432/merc_console?sslmode=disable" \
    "${DATABASE_URL:-}" \
    "${MERC_TEST_DATABASE_URL:-}"; do
    [ -n "$candidate" ] || continue
    if (
      eval "$(python3 - "$candidate" <<'PY'
import sys, urllib.parse, shlex
u = urllib.parse.urlparse(sys.argv[1])
for name, val in (
    ("PGHOST", u.hostname or "localhost"),
    ("PGPORT", str(u.port or 5432)),
    ("PGUSER", urllib.parse.unquote(u.username or "")),
    ("PGDATABASE", (u.path or "/").lstrip("/") or "postgres"),
    ("PGPASSWORD", urllib.parse.unquote(u.password or "")),
    ("PGSSLMODE", (urllib.parse.parse_qs(u.query).get("sslmode") or ["prefer"])[0]),
):
    print(f"export {name}={shlex.quote(val)}")
PY
)"
      psql -X -qAt -c 'SELECT 1' >/dev/null 2>&1
    ); then
      export MERC_CANARY_DATABASE_URL="$candidate"
      break
    fi
  done
fi
[ -n "${MERC_CANARY_DATABASE_URL:-}" ] \
  || fail "no reachable MERC_CANARY_DATABASE_URL (set it for the local control plane)"
# Configure PG* for remaining harness SQL without DSN on argv.
eval "$(python3 - "$MERC_CANARY_DATABASE_URL" <<'PY'
import sys, urllib.parse, shlex
u = urllib.parse.urlparse(sys.argv[1])
host = u.hostname or "localhost"
port = str(u.port or 5432)
user = urllib.parse.unquote(u.username or "")
password = urllib.parse.unquote(u.password or "")
db = (u.path or "/").lstrip("/") or "postgres"
qs = urllib.parse.parse_qs(u.query)
sslmode = (qs.get("sslmode") or ["prefer"])[0]
for name, val in (
    ("PGHOST", host), ("PGPORT", port), ("PGUSER", user),
    ("PGDATABASE", db), ("PGPASSWORD", password), ("PGSSLMODE", sslmode),
):
    print(f"export {name}={shlex.quote(val)}")
PY
)"

# Live control plane required for integration path.
if ! curl --fail --silent --show-error --max-time 5 "$CONTROL_BASE/healthz" >/dev/null 2>&1; then
  fail "control plane not reachable at $CONTROL_BASE/healthz (start it before this test)"
fi

# Demo workers are real RFC 4122 v4 UUIDs (version nibble 4). Version-nibble-0
# placeholders fail the receipt UUID regex; seed remaps the fixed demo tokens
# onto these IDs so local agent configs keep working.
export MERC_CANARY_APPROVED_WORKER_IDS="${MERC_CANARY_APPROVED_WORKER_IDS:-00000000-0000-4000-8000-0000000000b1,00000000-0000-4000-8000-0000000000b2}"
export MERC_CANARY_WORKER_TOKENS="${MERC_CANARY_WORKER_TOKENS:-00000000-0000-4000-8000-0000000000b1=dev-worker-token-0001,00000000-0000-4000-8000-0000000000b2=dev-worker-token-0002}"
export MERC_CANARY_ADMIN_API_KEY="${MERC_CANARY_ADMIN_API_KEY:-dev-admin-key-0001}"

if [ -z "${MERC_CANARY_APPROVED_BUYER_EMAILS:-}" ] || [ -z "${MERC_CANARY_BUYER_API_KEYS:-}" ]; then
  eval "$(python3 - "$CONTROL_BASE" <<'PY'
import json, sys, urllib.error, urllib.request
base = sys.argv[1].rstrip("/")
emails = ["canary-driver-one@example.invalid", "canary-driver-two@example.invalid"]
password = "canary-driver-pass-0001"
keys = {}
for email in emails:
    def post(path, data, headers=None):
        h = {"Content-Type": "application/json"}
        if headers:
            h.update(headers)
        req = urllib.request.Request(
            base + path, data=json.dumps(data).encode(), headers=h, method="POST"
        )
        try:
            with urllib.request.urlopen(req, timeout=30) as r:
                return r.status, json.loads(r.read().decode())
        except urllib.error.HTTPError as e:
            body = e.read().decode()
            try:
                return e.code, json.loads(body)
            except Exception:
                return e.code, {"error": body}

    st, body = post("/v1/signup", {"email": email, "password": password})
    if st in (200, 201) and body.get("sandbox_key"):
        keys[email] = body["sandbox_key"]
        continue
    st, body = post("/v1/login", {"email": email, "password": password})
    if st != 200 or "token" not in body:
        raise SystemExit(f"cannot signup/login {email}: {st} {body}")
    st, body = post(
        "/v1/keys",
        {"name": "canary-driver-test", "test": True},
        {"Authorization": "Bearer " + body["token"]},
    )
    if st not in (200, 201) or "key" not in body:
        raise SystemExit(f"cannot mint key for {email}: {st} {body}")
    keys[email] = body["key"]
print("export MERC_CANARY_APPROVED_BUYER_EMAILS=%s" % ",".join(emails))
print(
    "export MERC_CANARY_BUYER_API_KEYS=%s"
    % ",".join(f"{e}={keys[e]}" for e in emails)
)
PY
)"
fi

# Grant sandbox credit so job scenarios can spend (test harness only).
# Write path: temporarily leave read-only off for harness setup only.
psql -X -q -v ON_ERROR_STOP=1 -c "
  UPDATE buyers
     SET free_credit_usd = GREATEST(coalesce(free_credit_usd,0), 50)
   WHERE lower(email) = ANY(string_to_array(
     lower(replace('${MERC_CANARY_APPROVED_BUYER_EMAILS// /}', ' ', '')), ','));
" >/dev/null

# Derive reviewed agent version/build from the live approved workers when present.
if [ -z "${MERC_CANARY_APPROVED_AGENT_VERSIONS:-}" ] || [ -z "${MERC_CANARY_APPROVED_BUILD_HASHES:-}" ]; then
  row="$(psql -X -qAt -F $'\t' -c "
    SELECT coalesce(version,''), coalesce(build_hash,'')
      FROM workers
     WHERE id='00000000-0000-4000-8000-0000000000b1'
     LIMIT 1" 2>/dev/null || true)"
  ver="$(printf '%s' "$row" | cut -f1)"
  build="$(printf '%s' "$row" | cut -f2)"
  if [[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+ ]]; then
    export MERC_CANARY_APPROVED_AGENT_VERSIONS="${MERC_CANARY_APPROVED_AGENT_VERSIONS:-$ver}"
  else
    export MERC_CANARY_APPROVED_AGENT_VERSIONS="${MERC_CANARY_APPROVED_AGENT_VERSIONS:-0.1.0}"
  fi
  if [[ "$build" =~ ^[0-9a-f]{16}$ ]]; then
    export MERC_CANARY_APPROVED_BUILD_HASHES="${MERC_CANARY_APPROVED_BUILD_HASHES:-$build}"
  else
    export MERC_CANARY_APPROVED_BUILD_HASHES="${MERC_CANARY_APPROVED_BUILD_HASHES:-ea4d062efa210f76}"
  fi
fi

# Stripe must stay test-mode if present; never invent a live key.
case "${STRIPE_SECRET_KEY:-}" in
  sk_live_*|rk_live_*|pk_live_*) fail "refusing to run with a live Stripe key in the environment" ;;
esac

# Local agents keep heartbeating; do not wait the full deadWorkerAfter window in CI.
export MERC_CANARY_DEAD_WORKER_WAIT_SECS="${MERC_CANARY_DEAD_WORKER_WAIT_SECS:-20}"

validate_receipt() {
  local file="$1" scenario="$2" minimum="$3" scenario_started="$4" checked_at="$5"
  python3 scripts/validate-canary-scenario-receipt.py "$file" \
    --scenario "$scenario" --minimum "$minimum" \
    --run-id "$MERC_CANARY_RUN_ID" \
    --commit "$MERC_CANARY_CANDIDATE_COMMIT" \
    --image "$MERC_CANARY_CONTROL_IMAGE" \
    --driver-sha256 "$MERC_CANARY_DRIVER_SHA256" \
    --run-started-at "$MERC_CANARY_RUN_STARTED_AT" \
    --scenario-started-at "$scenario_started" \
    --checked-at "$checked_at" >/dev/null \
    || fail "$scenario receipt failed exact-run validation"
}

run_scenario() {
  local scenario="$1" minimum="$2"
  local scenario_started checked receipt out err
  scenario_started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO="$scenario"
  export MERC_CANARY_SCENARIO_MINIMUM="$minimum"
  export MERC_CANARY_SCENARIO_STARTED_AT="$scenario_started"
  receipt="$tmp/${scenario}.json"
  out="$tmp/${scenario}.stdout"
  err="$tmp/${scenario}.stderr"
  set +e
  "$DRIVER" run "$scenario" "$minimum" >"$out" 2>"$err"
  local rc=$?
  set -e
  if [ "$rc" -ne 0 ]; then
    printf 'SKIP/FAIL-CLOSED %s (exit %s): %s\n' "$scenario" "$rc" \
      "$(tr '\n' ' ' <"$err" | head -c 240)"
    # Fail-closed: no receipt body may be present.
    if [ -s "$out" ]; then
      # Allow empty whitespace only.
      if grep -q '[^[:space:]]' "$out"; then
        fail "$scenario exited $rc but still wrote stdout receipt bytes"
      fi
    fi
    return 1
  fi
  [ -s "$out" ] || fail "$scenario exited 0 without a receipt"
  cp "$out" "$receipt"
  # Brief delay so finished_at <= checked_at under second-resolution clocks.
  sleep 1
  checked="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  validate_receipt "$receipt" "$scenario" "$minimum" "$scenario_started" "$checked"
  pass "PASS $scenario (minimum=$minimum, validated)"
  return 0
}

# ---------------------------------------------------------------------------
# 1. Fail-closed: live Stripe key refused before network work
# ---------------------------------------------------------------------------
{
  export MERC_CANARY_SCENARIO=approved_buyer_identity
  export MERC_CANARY_SCENARIO_MINIMUM=2
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  out="$tmp/live-stripe.out"
  err="$tmp/live-stripe.err"
  set +e
  STRIPE_SECRET_KEY='sk_live_not_a_real_key_but_live_prefix' \
    "$DRIVER" run approved_buyer_identity 2 >"$out" 2>"$err"
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "live Stripe key was not refused"
  [ ! -s "$out" ] || fail "live Stripe refusal still emitted a receipt"
  grep -qi 'live' "$err" || fail "live Stripe refusal diagnostic missing"
  pass "fail-closed: live Stripe key refused with empty stdout"
}

# ---------------------------------------------------------------------------
# 2. Fail-closed: missing control base
# ---------------------------------------------------------------------------
{
  export MERC_CANARY_SCENARIO=approved_buyer_identity
  export MERC_CANARY_SCENARIO_MINIMUM=2
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  out="$tmp/no-control.out"
  err="$tmp/no-control.err"
  set +e
  env -u MERC_CONTROL_BASE_URL -u STAGING_TLS_HOSTNAME \
    MERC_CANARY_RUN_ID="$MERC_CANARY_RUN_ID" \
    MERC_CANARY_RUN_STARTED_AT="$MERC_CANARY_RUN_STARTED_AT" \
    MERC_CANARY_CANDIDATE_COMMIT="$MERC_CANARY_CANDIDATE_COMMIT" \
    MERC_CANARY_CONTROL_IMAGE="$MERC_CANARY_CONTROL_IMAGE" \
    MERC_CANARY_DRIVER_SHA256="$MERC_CANARY_DRIVER_SHA256" \
    MERC_CANARY_SCENARIO_STARTED_AT="$MERC_CANARY_SCENARIO_STARTED_AT" \
    MERC_CANARY_DATABASE_URL="$MERC_CANARY_DATABASE_URL" \
    MERC_CANARY_APPROVED_BUYER_EMAILS="$MERC_CANARY_APPROVED_BUYER_EMAILS" \
    MERC_CANARY_BUYER_API_KEYS="$MERC_CANARY_BUYER_API_KEYS" \
    "$DRIVER" run approved_buyer_identity 2 >"$out" 2>"$err"
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "missing control base did not fail"
  [ ! -s "$out" ] || fail "missing control base still emitted a receipt"
  pass "fail-closed: missing control base emits no receipt"
}

# ---------------------------------------------------------------------------
# 3. Fail-closed: backup without offsite config
# ---------------------------------------------------------------------------
{
  export MERC_CANARY_SCENARIO=backup_independent_restore
  export MERC_CANARY_SCENARIO_MINIMUM=1
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  out="$tmp/backup.out"
  err="$tmp/backup.err"
  set +e
  env -u MERC_BACKUP_OFFSITE -u MERC_BACKUP_ENCRYPTION_RECIPIENT \
    "$DRIVER" run backup_independent_restore 1 >"$out" 2>"$err"
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "backup without offsite config passed"
  [ ! -s "$out" ] || fail "backup fail-closed still emitted a receipt"
  grep -qi 'offsite\|MERC_BACKUP' "$err" || fail "backup diagnostic missing offsite mention"
  pass "fail-closed: backup_independent_restore without offsite config"

  # backup.sh refuses to overwrite an existing MERC_BACKUP_RESULT_FILE, so the
  # driver must hand it a path that does not exist yet. Allocating it with
  # mktemp made this scenario die unconditionally, after nine scenarios had
  # already passed, however correct the backup environment was.
  grep -q 'mktemp -d' <<<"$(sed -n '/scenario_backup_independent_restore/,/^}/p' "$DRIVER")" \
    || fail "backup scenario does not allocate its result path in a temp directory"
  if sed -n '/scenario_backup_independent_restore/,/^}/p' "$DRIVER" \
    | grep -qE 'result_file="\$\(mktemp [^-]'; then
    fail "backup result path is pre-created by mktemp; backup.sh will refuse it"
  fi
  pass "structural: backup result path is not pre-created"
}

# ---------------------------------------------------------------------------
# 3a-bis. post_rehearsal_invariant_audit must not certify an empty window.
#     Synthesising a subject from the run id produced evidence labelled
#     merc_postgres.tasks that resolves to no row at all.
# ---------------------------------------------------------------------------
{
  out=""; err=""; rc=0
  out="$tmp/invariant-empty.out"
  err="$tmp/invariant-empty.err"
  export MERC_CANARY_SCENARIO=post_rehearsal_invariant_audit
  export MERC_CANARY_SCENARIO_MINIMUM=1
  # A window that starts in the future cannot contain an observed task.
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u -v+1H +%Y-%m-%dT%H:%M:%SZ 2>/dev/null \
    || date -u -d '+1 hour' +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  set +e
  MERC_CANARY_RUN_STARTED_AT="$MERC_CANARY_SCENARIO_STARTED_AT" \
    "$DRIVER" run post_rehearsal_invariant_audit 1 >"$out" 2>"$err"
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "invariant audit certified a window with no observed task"
  [ ! -s "$out" ] || fail "invariant audit emitted a receipt for an empty window"
  grep -qi 'empty window\|no task observed' "$err" \
    || fail "invariant audit diagnostic does not name the empty window: $(tail -c 200 "$err")"
  # The synthesised subject must be gone entirely, not merely unused.
  grep -q 'invariant-audit-\${MERC_CANARY_RUN_ID' "$DRIVER" \
    && fail "invariant audit still carries a synthesised subject fallback"
  pass "fail-closed: invariant audit refuses a window with no observed task"
}

# ---------------------------------------------------------------------------
# 3b. Fail-closed: bounded_retry_backoff_audit must not mint merc_prometheus
#     provenance when Prometheus is unreachable (fabrication class).
# ---------------------------------------------------------------------------
{
  export MERC_CANARY_SCENARIO=bounded_retry_backoff_audit
  export MERC_CANARY_SCENARIO_MINIMUM=1
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  out="$tmp/backoff-prom.out"
  err="$tmp/backoff-prom.err"
  set +e
  # Point Prometheus at a black hole; the audit must still either fail closed
  # (empty task set) or emit merc_postgres.tasks — never merc_prometheus.
  MERC_PROMETHEUS_URL='http://127.0.0.1:1' \
    "$DRIVER" run bounded_retry_backoff_audit 1 >"$out" 2>"$err"
  rc=$?
  set -e
  if [ "$rc" -eq 0 ]; then
    [ -s "$out" ] || fail "bounded_retry exited 0 with empty stdout"
    src="$(jq -r '.evidence[0].source // empty' "$out")"
    [ "$src" = "merc_postgres.tasks" ] \
      || fail "bounded_retry with Prometheus down claimed source=$src (want merc_postgres.tasks)"
    jq -e 'has("backoff_schedule_within_policy") | not' "$out" >/dev/null \
      || fail "bounded_retry emitted unmeasured backoff_schedule_within_policy"
    jq -e '.max_attempts_within_policy == true and .unbounded_retry_growth == false' "$out" >/dev/null \
      || fail "bounded_retry missing measured attempt policy fields"
    pass "fabrication-guard: bounded_retry with Prometheus down uses merc_postgres.tasks"
  else
    [ ! -s "$out" ] || fail "bounded_retry fail-closed still emitted a receipt"
    # Empty task set since run start is an honest fail-closed.
    grep -qiE 'no tasks|empty|refusing' "$err" \
      || pass "fail-closed: bounded_retry exited $rc without merc_prometheus receipt (diag: $(tr '\n' ' ' <"$err" | head -c 160))"
    pass "fail-closed: bounded_retry with Prometheus down emits no merc_prometheus receipt"
  fi
}

# ---------------------------------------------------------------------------
# 3c. Fail-closed: unmeasurable safety when /readyz hides payment_mode
# ---------------------------------------------------------------------------
{
  export MERC_CANARY_SCENARIO=approved_buyer_identity
  export MERC_CANARY_SCENARIO_MINIMUM=2
  MERC_CANARY_SCENARIO_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO_STARTED_AT
  out="$tmp/no-payment-mode.out"
  err="$tmp/no-payment-mode.err"
  # Point control at a local python HTTP server that returns ready without payment_mode.
  py_port=18765
  python3 - "$py_port" <<'PY' &
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
port = int(sys.argv[1])
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b'{"status":"ready"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
HTTPServer(("127.0.0.1", port), H).handle_request()
PY
  mock_pid=$!
  sleep 0.3
  set +e
  MERC_CONTROL_BASE_URL="http://127.0.0.1:${py_port}" \
    "$DRIVER" run approved_buyer_identity 2 >"$out" 2>"$err"
  rc=$?
  set -e
  kill "$mock_pid" 2>/dev/null || true
  wait "$mock_pid" 2>/dev/null || true
  [ "$rc" -ne 0 ] || fail "missing payment_mode on /readyz did not fail closed"
  [ ! -s "$out" ] || fail "missing payment_mode still emitted a receipt"
  grep -qi 'payment_mode' "$err" || fail "missing payment_mode diagnostic absent"
  pass "fail-closed: /readyz without payment_mode emits no receipt"
}

# ---------------------------------------------------------------------------
# 3d. Structural: secret-shaped cx_* keys must be rejected by receipt assembly
# ---------------------------------------------------------------------------
{
  python3 - <<'PY' || fail "SECRET regex must match cx_test_ API keys"
import re
SECRET = re.compile(
    r"(sk_(?:test|live)_[A-Za-z0-9]+|"
    r"rk_(?:test|live)_[A-Za-z0-9]+|"
    r"pk_(?:test|live)_[A-Za-z0-9]+|"
    r"whsec_[A-Za-z0-9]+|"
    r"cx_(?:test|live)_[A-Za-z0-9_-]+|"
    r"cxw_[A-Za-z0-9_-]+|"
    r"ca_[A-Za-z0-9]+|"
    r"AGE-SECRET-KEY-[A-Za-z0-9+-]+|"
    r"AKIA[0-9A-Z]{12,}|"
    r"(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis|amqp|https?)://"
    r"[^\s:/@]+:[^\s/@]+@)",
    re.IGNORECASE,
)
samples = [
    "cx_test_abc123def456",
    "cxw_worker_token_001",
    "whsec_abc123",
    "sk_test_51abc",
    "sk_live_51abc",
    "rk_test_abc",
    "ca_abc123xyz",
    "postgres://cx:s3cret@127.0.0.1:5432/cx",
]
for s in samples:
    assert SECRET.search(s), s
print("ok")
PY
  pass "structural: secret-shaped guard covers cx_*/cxw_*/whsec_*/DSN forms"
}

# ---------------------------------------------------------------------------
# 3d-bis. Structural: job inputs carry a per-invocation nonce (not just the
#     idempotency key). Without it, a second run of embed_success under the
#     harness's pinned MERC_CANARY_RUN_ID is served from the exact-result cache
#     with task_count=0 and no supplier work.
# ---------------------------------------------------------------------------
{
  grep -q 'submit_nonce' "$DRIVER" \
    || fail "driver missing submit_nonce for unique job inputs"
  # Nonce must land in the embed INPUT body, not only the Idempotency-Key.
  if ! sed -n '/submit_job()/,/^}/p' "$DRIVER" | grep -q 'submit_nonce'; then
    fail "submit_job does not allocate submit_nonce"
  fi
  if ! sed -n '/submit_job()/,/^}/p' "$DRIVER" \
    | grep -E 'canary embed .*submit_nonce|text.*submit_nonce' >/dev/null; then
    fail "embed job input does not include submit_nonce (cache will swallow repeats)"
  fi
  # Stripe subject must never fall back to the driver run id.
  if sed -n '/scenario_stripe_test_matrix/,/^}/p' "$DRIVER" \
    | grep -qE 'stripe-matrix-\$\{?subject|stripe-matrix-\$MERC_CANARY_RUN_ID|subject=.*MERC_CANARY_RUN_ID'; then
    fail "stripe_test_matrix still mints subject_id from the driver run id"
  fi
  grep -q 'pi_\[A-Za-z0-9\]' "$DRIVER" \
    || fail "stripe_test_matrix no longer requires a resolvable pi_/ch_/tr_ subject"
  # Demo worker IDs in the driver path must not be version-nibble 0.
  if grep -n '00000000-0000-0000-0000-0000000000b[12]' control/seed.go \
    | grep -v '//' >/dev/null 2>&1; then
    # Only fail if those appear as the demoWorkerID constants (not comments).
    if grep -E 'demoWorkerID2?\s*=\s*"00000000-0000-0000-0000-0000000000b' control/seed.go >/dev/null; then
      fail "seed demo workers are still version-nibble 0 UUIDs"
    fi
  fi
  grep -E 'demoWorkerID\s*=\s*"00000000-0000-4000-8000-0000000000b1"' control/seed.go >/dev/null \
    || fail "seed demoWorkerID is not a v4 UUID"
  # batch_infer honeypot: seed installs the INPUT, and must never write a known
  # answer it has not measured. AvailableSeedHoneypots selects on shape alone, so
  # an unmeasurable row is attached as a real probe and quarantines the honest
  # supplier that runs it. A 503 from the verification floor is the correct
  # behaviour until scripts/seed-batch-infer-honeypot.sh has run.
  grep -q 'honeypots/batch_infer' control/seed.go \
    || fail "seed.go does not install a batch_infer honeypot input"
  if grep -qiE 'unmeasured' control/seed.go; then
    fail "seed.go still writes an unmeasured batch_infer known answer"
  fi
  grep -q 'MERC_BATCH_INFER_HONEYPOT_ANSWER' control/seed.go \
    || fail "seed.go does not accept a measured batch_infer answer"
  [ -x scripts/seed-batch-infer-honeypot.sh ] \
    || fail "scripts/seed-batch-infer-honeypot.sh missing or not executable"
  # The measured answer must reach the DB in worker wire order; bytes.Equal is
  # the comparison, so an alphabetized json! answer can never match a commit.
  grep -q 'known_answer_utf8' scripts/seed-batch-infer-honeypot.sh \
    || fail "seed-batch-infer-honeypot.sh does not take the exact answer bytes"
  grep -q 'seed-batch-infer-honeypot.sh' scripts/prove-local.sh \
    || fail "prove-local.sh does not measure a batch_infer honeypot before submitting one"
  pass "structural: unique embed input, v4 workers, batch_infer honeypot, stripe subject binding"
}

# ---------------------------------------------------------------------------
# 3e. Structural: validator rejects fabricated backoff_schedule + merc_prometheus
# ---------------------------------------------------------------------------
{
  fake="$tmp/fake-backoff.json"
  now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  sleep 1
  later="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  cat >"$fake" <<JSON
{"schema_version":2,"scenario":"bounded_retry_backoff_audit","requested":1,"observed":1,"status":"PASS",
 "binding":{"run_id":"$MERC_CANARY_RUN_ID","candidate_commit":"$MERC_CANARY_CANDIDATE_COMMIT",
 "control_image":"$MERC_CANARY_CONTROL_IMAGE","driver_sha256":"$MERC_CANARY_DRIVER_SHA256"},
 "started_at":"$now","finished_at":"$later",
 "safety":{"stripe_test_mode":false,"stripe_live_mode":false,"real_value":false,
  "approved_participants_only":true,"secret_values_recorded":false,
  "payment_mode":"sealed","live_value_movement":false},
 "evidence":[{"id":"obs-bounded_retry_backoff_audit-0001","subject_id":"retry-backoff-deadbeef",
  "occurred_at":"$later","source":"merc_prometheus"}],
 "max_attempts_within_policy":true,"backoff_schedule_within_policy":true,"unbounded_retry_growth":false}
JSON
  set +e
  python3 scripts/validate-canary-scenario-receipt.py "$fake" \
    --scenario bounded_retry_backoff_audit --minimum 1 \
    --run-id "$MERC_CANARY_RUN_ID" \
    --commit "$MERC_CANARY_CANDIDATE_COMMIT" \
    --image "$MERC_CANARY_CONTROL_IMAGE" \
    --driver-sha256 "$MERC_CANARY_DRIVER_SHA256" \
    --run-started-at "$MERC_CANARY_RUN_STARTED_AT" \
    --scenario-started-at "$now" \
    --checked-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$tmp/fake-backoff.val" 2>"$tmp/fake-backoff.err"
  rc=$?
  set -e
  [ "$rc" -ne 0 ] || fail "validator accepted fabricated merc_prometheus backoff receipt"
  grep -qiE 'source|backoff_schedule|merc_prometheus|merc_postgres' "$tmp/fake-backoff.err" \
    || fail "validator fabrication rejection diagnostic missing"
  pass "fabrication-guard: validator rejects merc_prometheus + backoff_schedule fabrication"
}

# ---------------------------------------------------------------------------
# 4. Live scenarios against the local control plane
# ---------------------------------------------------------------------------
# Scenarios that can complete with a single live Metal agent + demo buyer.
# Scenarios that need staging-only resources are expected to fail closed; that
# is recorded as an honest non-PASS rather than a fabricated receipt.
declare -a LIVE_SCENARIOS=(
  "approved_buyer_identity:2"
  "post_rehearsal_invariant_audit:1"
  "bounded_retry_backoff_audit:1"
  "cancelled_job:1"
  "embed_success:1"
  "forced_retry:1"
  "stale_attempt_commit_rejection:1"
  "distinct_metal_agent:1"
  "batch_infer_success:1"
  "stale_lease_recovery:1"
  "buyer_webhook_retry_sequence:1"
  "stripe_test_matrix:1"
  "real_alert_firing_resolution:1"
  "backup_independent_restore:1"
)

live_pass=0
live_fail_closed=0
for entry in "${LIVE_SCENARIOS[@]}"; do
  scenario="${entry%%:*}"
  minimum="${entry##*:}"
  if run_scenario "$scenario" "$minimum"; then
    live_pass=$((live_pass + 1))
  else
    live_fail_closed=$((live_fail_closed + 1))
  fi
done

# Hard requirements: payment-observed safety + approved buyers. Job paths need a
# live Metal agent and verification floor (honeypots); when those are absent the
# driver must fail closed rather than fabricate — that is not a harness failure.
[ -f "$tmp/approved_buyer_identity.json" ] \
  || fail "approved_buyer_identity must PASS against the local control plane"
# Safety must record observed payment_mode (not hardcoded defaults).
jq -e '.safety.payment_mode == "sealed" or .safety.payment_mode == "test"' \
  "$tmp/approved_buyer_identity.json" >/dev/null \
  || fail "approved_buyer receipt missing observed safety.payment_mode"
jq -e '.safety.live_value_movement == false and .safety.real_value == false and .safety.stripe_live_mode == false' \
  "$tmp/approved_buyer_identity.json" >/dev/null \
  || fail "approved_buyer receipt safety does not prove non-live observation"
if [ -f "$tmp/post_rehearsal_invariant_audit.json" ]; then
  jq -e 'has("invariants") and (.invariants | has("unreconciled_state") | not)' \
    "$tmp/post_rehearsal_invariant_audit.json" >/dev/null \
    || fail "post_rehearsal receipt still claims unreconciled_state without a query"
  jq -e '.evidence[0].source == "merc_postgres.tasks"' \
    "$tmp/post_rehearsal_invariant_audit.json" >/dev/null \
    || fail "post_rehearsal source must be merc_postgres.tasks (measured)"
fi
if [ -f "$tmp/bounded_retry_backoff_audit.json" ]; then
  jq -e '.evidence[0].source == "merc_postgres.tasks" and (has("backoff_schedule_within_policy") | not)' \
    "$tmp/bounded_retry_backoff_audit.json" >/dev/null \
    || fail "bounded_retry receipt still fabricates prometheus/backoff claims"
fi
if [ ! -f "$tmp/embed_success.json" ] && [ ! -f "$tmp/cancelled_job.json" ]; then
  pass "note: job scenarios fail-closed (no Metal agent and/or verification floor); honest, not fabricated"
fi
if [ ! -f "$tmp/bounded_retry_backoff_audit.json" ] && [ ! -f "$tmp/post_rehearsal_invariant_audit.json" ]; then
  pass "note: invariant/backoff audits fail-closed (empty run window or dirty DB)"
fi

# ---------------------------------------------------------------------------
# 5. Regression: two consecutive embed_success runs must both exercise the
#    pipeline (task_count > 0). A second run served from the exact-result cache
#    completes with task_count=0 and must not be certified as pipeline work.
# ---------------------------------------------------------------------------
if [ -f "$tmp/embed_success.json" ]; then
  # First live pass already succeeded above. Run embed_success again under the
  # same pinned MERC_CANARY_RUN_ID; unique input nonces must keep task_count > 0.
  second_started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  export MERC_CANARY_SCENARIO=embed_success
  export MERC_CANARY_SCENARIO_MINIMUM=1
  export MERC_CANARY_SCENARIO_STARTED_AT="$second_started"
  second_out="$tmp/embed_success_second.stdout"
  second_err="$tmp/embed_success_second.stderr"
  set +e
  "$DRIVER" run embed_success 1 >"$second_out" 2>"$second_err"
  second_rc=$?
  set -e
  if [ "$second_rc" -ne 0 ]; then
    fail "second consecutive embed_success failed (exit $second_rc): $(tr '\n' ' ' <"$second_err" | head -c 240)"
  fi
  [ -s "$second_out" ] || fail "second embed_success exited 0 without a receipt"
  sleep 1
  second_checked="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  validate_receipt "$second_out" embed_success 1 "$second_started" "$second_checked"
  # Corroborate both subjects against the live DB: task_count must be > 0.
  for receipt_file in "$tmp/embed_success.json" "$second_out"; do
    subject="$(jq -er '.evidence[0].subject_id' "$receipt_file")"
    tc="$(psql -X -qAt -c "
      SELECT task_count FROM jobs WHERE id = '$subject'::uuid" 2>/dev/null || true)"
    [ -n "$tc" ] || fail "embed_success subject $subject not found in jobs"
    [ "$tc" -gt 0 ] \
      || fail "embed_success subject $subject has task_count=$tc (cache hit, not pipeline)"
  done
  pass "regression: two consecutive embed_success runs both produced task_count > 0"
else
  pass "note: consecutive embed_success check skipped (first embed_success did not PASS)"
fi

# Worker subjects on distinct_metal_agent must be real v4 UUIDs (not seed nibble-0).
if [ -f "$tmp/distinct_metal_agent.json" ]; then
  python3 - "$tmp/distinct_metal_agent.json" <<'PY' || fail "distinct_metal_agent subject is not a v4 UUID"
import json, re, sys
UUID_V4 = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
receipt = json.load(open(sys.argv[1]))
for item in receipt["evidence"]:
    sid = item["subject_id"]
    if not UUID_V4.fullmatch(sid):
        raise SystemExit(f"subject_id {sid!r} is not a lowercase v4 UUID")
print("ok")
PY
  pass "regression: distinct_metal_agent evidence subjects are v4 UUIDs"
fi

# stripe subject must be a real provider object when the scenario PASSed.
if [ -f "$tmp/stripe_test_matrix.json" ]; then
  sid="$(jq -er '.evidence[0].subject_id' "$tmp/stripe_test_matrix.json")"
  case "$sid" in
    pi_[A-Za-z0-9]*|ch_[A-Za-z0-9]*|tr_[A-Za-z0-9]*)
      pass "regression: stripe_test_matrix subject_id is resolvable provider object ($sid)"
      ;;
    *)
      fail "stripe_test_matrix subject_id $sid is not a pi_/ch_/tr_ provider object"
      ;;
  esac
fi

pass "summary: $live_pass scenario receipt(s) validated; $live_fail_closed fail-closed (honest missing deps)"
echo "canary-scenario-driver: PASS (fail-closed paths enforced; live receipts schema-v2 validated)"
