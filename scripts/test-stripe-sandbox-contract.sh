#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/scripts/lib/stripe-sandbox-contract.sh"

die() { printf 'test-stripe-sandbox-contract: FAIL %s\n' "$*" >&2; exit 1; }

# ripgrep is a hard dependency of the assertions below. Without this guard a
# missing rg makes every `rg -q ... || die` report the *opposite* of what the
# file says, which is how a correctly CAD-bound compose file was reported as
# unbound.
command -v rg >/dev/null 2>&1 || die "ripgrep (rg) is required by this contract test"

[ "$MERC_STRIPE_API_VERSION" = "2025-06-30.basil" ] \
  || die "unexpected Stripe API version"
[ "$MERC_STRIPE_CANDIDATE_CURRENCY" = "cad" ] \
  || die "candidate settlement currency is not cad"
[ "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" = "CA" ] \
  || die "candidate connected-account country is not CA"
[ "$MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING" = "11000-000" ] \
  || die "Canadian payout fixture routing drifted"

merc_stripe_valid_staging_hostname "canary.example.invalid" \
  || die "valid staging hostname rejected"
for invalid in \
  localhost https://canary.example.invalid canary..example.invalid \
  -canary.example.invalid canary-.example.invalid canary_example.invalid \
  canary.example.invalid:443 canary.example.invalid/path; do
  if merc_stripe_valid_staging_hostname "$invalid"; then
    die "invalid staging hostname accepted: $invalid"
  fi
done

billing_url="$(merc_stripe_expected_billing_url "CANARY.EXAMPLE.INVALID")"
connect_url="$(merc_stripe_expected_connect_url "CANARY.EXAMPLE.INVALID")"
[ "$billing_url" = "https://canary.example.invalid/v1/stripe/webhook" ] \
  || die "billing URL authority drifted"
[ "$connect_url" = "https://canary.example.invalid/v1/stripe/connect-webhook" ] \
  || die "Connect URL authority drifted"

billing_events="setup_intent.succeeded,payment_method.attached,payment_intent.succeeded,charge.refunded,charge.dispute.created,charge.dispute.funds_withdrawn,charge.dispute.funds_reinstated,charge.dispute.closed"
billing_json="$(jq -nc \
  --arg id we_billing \
  --arg url "$billing_url" \
  --arg version "$MERC_STRIPE_API_VERSION" \
  --arg events "$billing_events" '
    {id:$id,url:$url,api_version:$version,livemode:false,status:"enabled",
     enabled_events:($events|split(","))}
  ')"
merc_stripe_endpoint_contract "$billing_json" we_billing "$billing_url" "$billing_events" \
  || die "exact billing endpoint rejected"

for drifted in \
  "$(jq -c '.url="https://wrong.example.invalid/v1/stripe/webhook"' <<< "$billing_json")" \
  "$(jq -c '.api_version="2026-02-25.clover"' <<< "$billing_json")" \
  "$(jq -c '.livemode=true' <<< "$billing_json")" \
  "$(jq -c '.enabled_events -= ["charge.dispute.closed"]' <<< "$billing_json")"; do
  if merc_stripe_endpoint_contract "$drifted" we_billing "$billing_url" "$billing_events"; then
    die "drifted billing endpoint contract accepted"
  fi
done

wildcard_json="$(jq -nc \
  --arg id we_connect \
  --arg url "$connect_url" \
  --arg version "$MERC_STRIPE_API_VERSION" '
    {id:$id,url:$url,api_version:$version,livemode:false,status:"enabled",
     enabled_events:["*"]}
  ')"
merc_stripe_endpoint_contract \
  "$wildcard_json" we_connect "$connect_url" \
  "account.updated,payout.created,payout.paid,payout.failed" \
  || die "explicit wildcard endpoint rejected"

merc_stripe_distinct_endpoint_ids we_billing we_connect \
  || die "distinct webhook endpoint IDs rejected"
if merc_stripe_distinct_endpoint_ids we_same we_same; then
  die "aliased webhook endpoint IDs accepted"
fi

rg -q '^[[:space:]]*MERC_SETTLEMENT_CURRENCY:[[:space:]]*"cad"$' \
  "$ROOT/ops/staging/compose.go-closure.yml" \
  || die "staging Compose is not bound to cad"
rg -q '^[[:space:]]*MERC_PAYMENT_MODE:[[:space:]]*"test"$' \
  "$ROOT/ops/staging/compose.go-closure.yml" \
  || die "staging Compose does not explicitly authorize test mode"
rg -q '^[[:space:]]*MERC_PAYMENT_PROVIDER:[[:space:]]*"stripe"$' \
  "$ROOT/ops/staging/compose.go-closure.yml" \
  || die "staging Compose does not select the Stripe test provider"
if rg -n 'currency=usd|country == "US"|US test account' \
  "$ROOT/scripts/stripe-sandbox.sh" "$ROOT/scripts/stripe-sandbox-scenarios.sh" \
  >/dev/null; then
  die "formal Stripe matrix still contains the obsolete USD/US authority"
fi
for script in scripts/stripe-sandbox.sh scripts/stripe-sandbox-scenarios.sh; do
  rg -q 'scripts/lib/stripe-sandbox-contract[.]sh' "$ROOT/$script" \
    || die "$script does not source the shared candidate authority"
  rg -q 'merc_stripe_endpoint_contract' "$ROOT/$script" \
    || die "$script does not enforce endpoint identity"
done
for required in \
  'verify_cash_outcome "$cash_probe_closed_payload" applied 30' \
  'verify_cash_outcome "$cash_probe_opened_payload" stale_ignored 30' \
  'verify_cash_outcome "$cash_probe_closed_payload" duplicate' \
  'application_outcomes_verified:true'; do
  rg -Fq "$required" "$ROOT/scripts/stripe-sandbox-scenarios.sh" \
    || die "scenario driver does not prove webhook outcome: $required"
done
rg -q '\[ "\$live_alias_present" = false \]' "$ROOT/scripts/release-doctor.sh" \
  || die "release doctor can report Stripe ready while a live alias is present"
rg -q 'merc_stripe_distinct_endpoint_ids' "$ROOT/scripts/release-doctor.sh" \
  || die "release doctor does not reject aliased endpoint IDs"
rg -q 'merc_stripe_valid_staging_hostname' "$ROOT/scripts/release-doctor.sh" \
  || die "release doctor does not require staging URL authority"

run_rejected_preflight() {
  local currency="$1" billing_id="$2" connect_id="$3" output status
  set +e
  output="$(env \
    MERC_GO_CLOSURE_ENV_FILE=/nonexistent/merc-go-closure-env \
    STRIPE_SECRET_KEY=sk_test_contract_shape \
    STRIPE_WEBHOOK_SECRET=whsec_billing_contract \
    MERC_CONNECT_WEBHOOK_SECRET=whsec_connect_contract \
    MERC_CONNECT_CLIENT_ID=ca_contract \
    STRIPE_TEST_CONNECTED_ACCOUNT_ID=acct_contract \
    STRIPE_BILLING_WEBHOOK_ENDPOINT_ID="$billing_id" \
    STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID="$connect_id" \
    STAGING_TLS_HOSTNAME=canary.example.invalid \
    MERC_SETTLEMENT_CURRENCY="$currency" \
    "$ROOT/scripts/stripe-sandbox.sh" check)"
  status=$?
  set -e
  [ "$status" -ne 0 ] || die "invalid preflight reached the provider boundary"
  jq -e '
    .status == "EXTERNAL CREDENTIAL REQUIRED" and
    .network_accessed == false and .secret_values_printed == false
  ' <<< "$output" >/dev/null || die "invalid preflight lacked a fail-before-network receipt"
}

run_rejected_preflight cad we_same we_same
run_rejected_preflight usd we_billing we_connect

printf 'test-stripe-sandbox-contract: PASS (currency, country, endpoint IDs, exact URLs, events, API version)\n'
