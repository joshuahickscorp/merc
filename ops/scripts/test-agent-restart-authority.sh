#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/../.."

: "${MERC_TEST_DATABASE_URL:?agent restart authority test needs MERC_TEST_DATABASE_URL}"
for command in createdb dropdb psql jq python3; do
  command -v "$command" >/dev/null 2>&1 || { echo "missing $command" >&2; exit 1; }
done

fail() { echo "agent restart authority test: FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-agent-restart-test.XXXXXX")"
db="merc_agent_restart_authority_$$"
cleanup() {
  dropdb --if-exists --maintenance-db="$MERC_TEST_DATABASE_URL" "$db" >/dev/null 2>&1 || true
  rm -rf "$tmp"
}
trap cleanup EXIT

run_id='0123456789abcdef0123456789abcdef'
commit='1111111111111111111111111111111111111111'
driver='2222222222222222222222222222222222222222222222222222222222222222'
image="registry.example.invalid/merc/control@sha256:3333333333333333333333333333333333333333333333333333333333333333"
worker1='10000000-0000-4000-8000-000000000001'
worker2='10000000-0000-4000-8000-000000000002'
before1='20000000-0000-4000-8000-000000000001'
before2='20000000-0000-4000-8000-000000000002'
after1='30000000-0000-4000-8000-000000000001'
after2='30000000-0000-4000-8000-000000000002'
run_started='2026-07-28T10:00:00Z'
checked='2026-07-28T10:05:00Z'

jq -n \
  --arg run "$run_id" --arg commit "$commit" --arg image "$image" \
  --arg driver "$driver" --arg worker1 "$worker1" --arg worker2 "$worker2" '
  {
    schema_version:2,
    kind:"merc_agent_restart_action",
    status:"PASS",
    requested:2,
    binding:{
      run_id:$run,
      candidate_commit:$commit,
      control_image:$image,
      driver_sha256:$driver
    },
    started_at:"2026-07-28T10:01:00Z",
    finished_at:"2026-07-28T10:02:00Z",
    safety:{
      stripe_live_mode:false,
      real_value:false,
      approved_participants_only:true,
      secret_values_recorded:false
    },
    actions:[
      {
        id:"restart-action-fixture-0001",
        worker_id:$worker1,
        occurred_at:"2026-07-28T10:01:20Z",
        source:"approved_agent_supervisor"
      },
      {
        id:"restart-action-fixture-0002",
        worker_id:$worker2,
        occurred_at:"2026-07-28T10:01:30Z",
        source:"approved_agent_supervisor"
      }
    ]
  }' > "$tmp/valid.json"

validator=(
  python3 ops/scripts/validate-agent-restart-receipt.py
  --run-id "$run_id"
  --commit "$commit"
  --image "$image"
  --driver-sha256 "$driver"
  --approved-worker-ids "$worker1,$worker2"
  --run-started-at "$run_started"
  --checked-at "$checked"
)
"${validator[@]}" "$tmp/valid.json" >/dev/null \
  || fail "valid exact-run action receipt was rejected"

expect_fail() {
  local name="$1" filter="$2"
  jq "$filter" "$tmp/valid.json" > "$tmp/$name.json"
  if "${validator[@]}" "$tmp/$name.json" >/dev/null 2>&1; then
    fail "$name mutation was accepted"
  fi
}

expect_fail replayed_run '.binding.run_id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
expect_fail wrong_candidate '.binding.candidate_commit = ("a" * 40)'
expect_fail wrong_driver '.binding.driver_sha256 = ("a" * 64)'
expect_fail wrong_worker '.actions[1].worker_id = "40000000-0000-4000-8000-000000000004"'
expect_fail wrong_source '.actions[0].source = "driver_says_so"'
expect_fail duplicate_action '.actions[1].id = .actions[0].id'
expect_fail before_run '.started_at = "2026-07-28T09:59:59Z"'
expect_fail after_check '.finished_at = "2026-07-28T10:05:01Z"'
expect_fail secret '.note = "whsec_not-a-real-secret"'
expect_fail count '.requested = 3'
printf '{"schema_version":2,"schema_version":2}\n' > "$tmp/duplicate-key.json"
if "${validator[@]}" "$tmp/duplicate-key.json" >/dev/null 2>&1; then
  fail "duplicate JSON key was accepted"
fi

createdb --maintenance-db="$MERC_TEST_DATABASE_URL" "$db"
test_url="$(python3 - "$MERC_TEST_DATABASE_URL" "$db" <<'PY'
import sys,urllib.parse
u=urllib.parse.urlsplit(sys.argv[1])
print(urllib.parse.urlunsplit((u.scheme,u.netloc,"/"+sys.argv[2],u.query,u.fragment)))
PY
)"
PGOPTIONS='-c client_min_messages=warning' \
  psql "$test_url" -X -q -v ON_ERROR_STOP=1 -f src/control/schema.sql >/dev/null

supplier1='50000000-0000-4000-8000-000000000001'
supplier2='50000000-0000-4000-8000-000000000002'
psql "$test_url" -X -q -v ON_ERROR_STOP=1 <<SQL
INSERT INTO suppliers (id,email,status)
VALUES
  ('$supplier1','restart-one@example.invalid','active'),
  ('$supplier2','restart-two@example.invalid','active');
INSERT INTO workers
  (id,supplier_id,hw_class,last_seen_at,version,engine,build_hash,
   agent_session_id,agent_session_started_at)
VALUES
  ('$worker1','$supplier1','apple_silicon_max',now(),'1.2.3','candle',
   '1111111111111111','$after1',now()),
  ('$worker2','$supplier2','apple_silicon_ultra',now(),'1.2.3','candle',
   '2222222222222222','$after2',now());
SQL

run_started_epoch=$(( $(date +%s) - 5 ))
jq -n \
  --arg worker1 "$worker1" --arg worker2 "$worker2" \
  --arg before1 "$before1" --arg before2 "$before2" '
  [
    {worker_id:$worker1,agent_session_id:$before1},
    {worker_id:$worker2,agent_session_id:$before2}
  ]' > "$tmp/before.json"

MERC_TEST_DATABASE_URL="$test_url" \
MERC_AGENT_RESTART_CORROBORATION_SELF_TEST=1 \
MERC_RESTART_RUN_STARTED_EPOCH="$run_started_epoch" \
MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
MERC_CANARY_APPROVED_AGENT_VERSIONS='1.2.3' \
MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
  bash ops/scripts/go-closure-restart-storm.sh "$tmp/before.json" >/dev/null \
  || fail "two durable database session transitions were rejected"

jq --arg same "$after1" '.[0].agent_session_id=$same' \
  "$tmp/before.json" > "$tmp/one-unchanged.json"
if MERC_TEST_DATABASE_URL="$test_url" \
  MERC_AGENT_RESTART_CORROBORATION_SELF_TEST=1 \
  MERC_RESTART_RUN_STARTED_EPOCH="$run_started_epoch" \
  MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
  MERC_CANARY_APPROVED_AGENT_VERSIONS='1.2.3' \
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
    bash ops/scripts/go-closure-restart-storm.sh "$tmp/one-unchanged.json" >/dev/null 2>&1; then
  fail "one unchanged agent process session was accepted"
fi

if MERC_TEST_DATABASE_URL="$test_url" \
  MERC_AGENT_RESTART_CORROBORATION_SELF_TEST=1 \
  MERC_RESTART_RUN_STARTED_EPOCH="$run_started_epoch" \
  MERC_CANARY_APPROVED_WORKER_IDS="$worker1,$worker2" \
  MERC_CANARY_APPROVED_AGENT_VERSIONS='9.9.9' \
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222' \
    bash ops/scripts/go-closure-restart-storm.sh "$tmp/before.json" >/dev/null 2>&1; then
  fail "unreviewed agent versions were accepted as restart evidence"
fi

doctor_env=(
  env -i
  "PATH=$PATH"
  MERC_RELEASE_DOCTOR_NO_ENV_FILE=1
  MERC_CANARY_APPROVED_BUYER_EMAILS='one@example.invalid,two@example.invalid'
  "MERC_CANARY_APPROVED_WORKER_IDS=$worker1,$worker2"
  MERC_CANARY_APPROVED_AGENT_VERSIONS='1.2.3'
  MERC_CANARY_APPROVED_BUILD_HASHES='1111111111111111,2222222222222222'
  MERC_CANARY_SCENARIO_DRIVER='/srv/merc/canary-driver'
  MERC_CANARY_APPROVED_DRIVER_SHA256='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
  MERC_AGENT_RESTART_DRIVER='/srv/merc/restart-driver'
)
"${doctor_env[@]}" \
  MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' \
  bash ops/scripts/release-doctor.sh --check canary >/dev/null \
  || fail "release doctor rejected a complete canary authority input set"
if "${doctor_env[@]}" \
  bash ops/scripts/release-doctor.sh --check canary >/dev/null 2>&1; then
  fail "release doctor accepted canary inputs without an approved restart-driver digest"
fi

echo "agent-restart-authority: PASS (exact action receipt, durable process sessions, fail-closed doctor)"
