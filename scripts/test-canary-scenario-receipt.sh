#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

fail() { echo "canary scenario receipt test: FAIL: $*" >&2; exit 1; }
tmp="$(mktemp -d "${TMPDIR:-/tmp}/merc-canary-receipt-test.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

run_id='0123456789abcdef0123456789abcdef'
commit='1111111111111111111111111111111111111111'
driver='2222222222222222222222222222222222222222222222222222222222222222'
image="registry.example.invalid/merc/control@sha256:3333333333333333333333333333333333333333333333333333333333333333"
run_started='2026-07-28T10:00:00Z'
checked='2026-07-28T10:05:00Z'
subject='44444444-4444-4444-8444-444444444444'

validator=(
  python3 scripts/validate-canary-scenario-receipt.py
  --scenario stale_attempt_commit_rejection
  --minimum 1
  --run-id "$run_id"
  --commit "$commit"
  --image "$image"
  --driver-sha256 "$driver"
  --run-started-at "$run_started"
  --scenario-started-at "2026-07-28T10:00:30Z"
  --checked-at "$checked"
)

jq -n \
  --arg run "$run_id" --arg commit "$commit" --arg image "$image" \
  --arg driver "$driver" --arg subject "$subject" '
  {
    schema_version:2,
    scenario:"stale_attempt_commit_rejection",
    requested:1,
    observed:1,
    status:"PASS",
    binding:{run_id:$run,candidate_commit:$commit,control_image:$image,driver_sha256:$driver},
    started_at:"2026-07-28T10:01:00Z",
    finished_at:"2026-07-28T10:02:00Z",
    safety:{
      stripe_test_mode:true,
      stripe_live_mode:false,
      real_value:false,
      approved_participants_only:true,
      secret_values_recorded:false
    },
    evidence:[{
      id:"observation-stale-attempt-0001",
      subject_id:$subject,
      occurred_at:"2026-07-28T10:01:30Z",
      source:"merc_control.http",
      submitted_attempt:1,
      current_attempt:2,
      before_state_sha256:"4444444444444444444444444444444444444444444444444444444444444444",
      after_state_sha256:"4444444444444444444444444444444444444444444444444444444444444444",
      http_status:409,
      response_sha256:"5555555555555555555555555555555555555555555555555555555555555555"
    }]
  }' > "$tmp/valid.json"

"${validator[@]}" "$tmp/valid.json" >/dev/null \
  || fail "valid exact-run receipt was rejected"

expect_fail() {
  local name="$1" filter="$2"
  jq "$filter" "$tmp/valid.json" > "$tmp/$name.json"
  if "${validator[@]}" "$tmp/$name.json" >/dev/null 2>&1; then
    fail "$name mutation was accepted"
  fi
}

expect_fail replayed_run '.binding.run_id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
expect_fail wrong_candidate '.binding.candidate_commit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"'
expect_fail wrong_driver '.binding.driver_sha256 = ("a" * 64)'
expect_fail wrong_source '.evidence[0].source = "scenario_driver"'
expect_fail stale_observation '.evidence[0].occurred_at = "2026-07-28T09:59:59Z"'
expect_fail before_invocation '.started_at = "2026-07-28T10:00:15Z"'
expect_fail future_finish '.finished_at = "2026-07-28T10:06:00Z"'
expect_fail missing_subject 'del(.evidence[0].subject_id)'
expect_fail fake_status '.evidence[0].http_status = 200'
expect_fail current_not_newer '.evidence[0].current_attempt = 1'
expect_fail state_changed '.evidence[0].after_state_sha256 = ("6" * 64)'
expect_fail secret_in_receipt '.note = "whsec_not-a-real-secret"'
expect_fail count_mismatch '.observed = 2'
expect_fail nonfinite '.observed = nan'
printf '{"schema_version":2,"schema_version":2}\n' > "$tmp/duplicate-key.json"
if "${validator[@]}" "$tmp/duplicate-key.json" >/dev/null 2>&1; then
  fail "duplicate JSON key was accepted"
fi

# Provider-specific Stripe evidence must prove application outcomes, not only
# say that the provider accepted a resend.
jq '
  .scenario = "stripe_test_matrix" |
  .evidence[0].source = "stripe_test_api" |
  .evidence[0].subject_id = "evt_test_provider_0001" |
  .provider_mode = "test" |
  .matrix_complete = true |
  .real_value = false |
  .application_outcomes_verified = true |
  .evidence[0] = {
    id:.evidence[0].id,
    subject_id:"evt_test_provider_0001",
    occurred_at:.evidence[0].occurred_at,
    source:"stripe_test_api"
  }
' "$tmp/valid.json" > "$tmp/stripe.json"
stripe_validator=(
  python3 scripts/validate-canary-scenario-receipt.py
  --scenario stripe_test_matrix --minimum 1
  --run-id "$run_id" --commit "$commit" --image "$image"
  --driver-sha256 "$driver" --run-started-at "$run_started"
  --scenario-started-at "2026-07-28T10:00:30Z" --checked-at "$checked"
)
"${stripe_validator[@]}" "$tmp/stripe.json" >/dev/null \
  || fail "valid Stripe matrix receipt was rejected"
jq 'del(.application_outcomes_verified)' "$tmp/stripe.json" > "$tmp/stripe-self-asserted.json"
if "${stripe_validator[@]}" "$tmp/stripe-self-asserted.json" >/dev/null 2>&1; then
  fail "Stripe receipt without observed application outcomes was accepted"
fi

echo "canary-scenario-receipt: PASS (run/candidate/driver/time/source/count/secret/provider attacks rejected)"
