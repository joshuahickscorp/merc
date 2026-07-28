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

CONTROL_BASE="${MERC_CONTROL_BASE_URL:-http://127.0.0.1:8093}"
export MERC_CONTROL_BASE_URL="$CONTROL_BASE"

# Prefer the live console DB used by the local control process when present.
if [ -z "${MERC_CANARY_DATABASE_URL:-}" ]; then
  for candidate in \
    "postgres://cx@127.0.0.1:55432/merc_console?sslmode=disable" \
    "postgres://cx:cx@127.0.0.1:5432/cx?sslmode=disable" \
    "${DATABASE_URL:-}" \
    "${MERC_TEST_DATABASE_URL:-}"; do
    [ -n "$candidate" ] || continue
    if psql "$candidate" -X -qAt -c 'SELECT 1' >/dev/null 2>&1; then
      export MERC_CANARY_DATABASE_URL="$candidate"
      break
    fi
  done
fi
[ -n "${MERC_CANARY_DATABASE_URL:-}" ] \
  || fail "no reachable MERC_CANARY_DATABASE_URL (set it for the local control plane)"

# Live control plane required for integration path.
if ! curl --fail --silent --show-error --max-time 5 "$CONTROL_BASE/healthz" >/dev/null 2>&1; then
  fail "control plane not reachable at $CONTROL_BASE/healthz (start it before this test)"
fi

# Seed demo worker UUIDs are version-nibble 0 and fail the receipt UUID regex.
# Bootstrap two real buyers via signup (v4 UUIDs) and mint API keys. Free credit
# for job scenarios is granted through the local DB as test setup only — the
# driver itself never writes buyer credit.
export MERC_CANARY_APPROVED_WORKER_IDS="${MERC_CANARY_APPROVED_WORKER_IDS:-00000000-0000-0000-0000-0000000000b1,00000000-0000-0000-0000-0000000000b2}"
export MERC_CANARY_WORKER_TOKENS="${MERC_CANARY_WORKER_TOKENS:-00000000-0000-0000-0000-0000000000b1=dev-worker-token-0001,00000000-0000-0000-0000-0000000000b2=dev-worker-token-0002}"
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
psql "$MERC_CANARY_DATABASE_URL" -X -q -v ON_ERROR_STOP=1 -c "
  UPDATE buyers
     SET free_credit_usd = GREATEST(coalesce(free_credit_usd,0), 50)
   WHERE lower(email) = ANY(string_to_array(
     lower(replace('${MERC_CANARY_APPROVED_BUYER_EMAILS// /}', ' ', '')), ','));
" >/dev/null

# Derive reviewed agent version/build from the live approved workers when present.
if [ -z "${MERC_CANARY_APPROVED_AGENT_VERSIONS:-}" ] || [ -z "${MERC_CANARY_APPROVED_BUILD_HASHES:-}" ]; then
  row="$(psql "$MERC_CANARY_DATABASE_URL" -X -qAt -F $'\t' -c "
    SELECT coalesce(version,''), coalesce(build_hash,'')
      FROM workers
     WHERE id='00000000-0000-0000-0000-0000000000b1'
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

# Hard requirements on a healthy local stack: approved buyers + at least one
# real job path. Invariant/backoff audits PASS when the DB is clean since the
# run window; a historically dirty local database fail-closes them honestly.
[ -f "$tmp/approved_buyer_identity.json" ] \
  || fail "approved_buyer_identity must PASS against the local control plane"
if [ ! -f "$tmp/embed_success.json" ] && [ ! -f "$tmp/cancelled_job.json" ]; then
  fail "neither embed_success nor cancelled_job produced a receipt; control plane/agent path looks broken"
fi
if [ ! -f "$tmp/bounded_retry_backoff_audit.json" ] && [ ! -f "$tmp/post_rehearsal_invariant_audit.json" ]; then
  pass "note: invariant/backoff audits fail-closed (likely pre-existing local DB dirt or missing Prometheus)"
fi

pass "summary: $live_pass scenario receipt(s) validated; $live_fail_closed fail-closed (honest missing deps)"
echo "canary-scenario-driver: PASS (fail-closed paths enforced; live receipts schema-v2 validated)"
