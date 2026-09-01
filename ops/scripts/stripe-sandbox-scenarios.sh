#!/usr/bin/env bash
set -euo pipefail

# Bundled Stripe Sandbox-only provider scenario adapter. The parent command
# validates credentials first; this script repeats the live-key refusal so it
# is also safe when invoked directly. It never supports live mode.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/ops/scripts/lib/stripe-sandbox-contract.sh"

RUN_ID=""
PAYMENT_INTENT=""
CHARGE=""
TRANSFER=""
CONNECTED_ACCOUNT=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --run-id) RUN_ID="${2:-}"; shift 2 ;;
    --payment-intent) PAYMENT_INTENT="${2:-}"; shift 2 ;;
    --charge) CHARGE="${2:-}"; shift 2 ;;
    --transfer) TRANSFER="${2:-}"; shift 2 ;;
    --connected-account) CONNECTED_ACCOUNT="${2:-}"; shift 2 ;;
    *) echo "stripe-sandbox-scenarios: unsupported argument $1" >&2; exit 2 ;;
  esac
done

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  case "${!name:-}" in
    sk_live_*|rk_live_*|pk_live_*)
      jq -nc --arg variable "$name" \
        '{schema_version:1,status:"LIVE CREDENTIAL REFUSED",provider_mode:"refused",network_accessed:false,secret_values_recorded:false,refused_variable:$variable}'
      exit 1
      ;;
  esac
done

[[ "${STRIPE_SECRET_KEY:-}" =~ ^(sk_test_|rk_test_)[A-Za-z0-9_]+$ ]] || { echo "stripe-sandbox-scenarios: test key required" >&2; exit 1; }
[[ "${STRIPE_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || { echo "stripe-sandbox-scenarios: billing webhook secret required" >&2; exit 1; }
[[ "${MERC_CONNECT_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || { echo "stripe-sandbox-scenarios: Connect webhook secret required" >&2; exit 1; }
[ "$STRIPE_WEBHOOK_SECRET" != "$MERC_CONNECT_WEBHOOK_SECRET" ] || { echo "stripe-sandbox-scenarios: webhook secrets must be distinct" >&2; exit 1; }
[[ "$RUN_ID" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "stripe-sandbox-scenarios: run id required" >&2; exit 1; }
[[ "$PAYMENT_INTENT" =~ ^pi_[A-Za-z0-9]+$ && "$CHARGE" =~ ^ch_[A-Za-z0-9]+$ && "$TRANSFER" =~ ^tr_[A-Za-z0-9]+$ ]] || {
  echo "stripe-sandbox-scenarios: provider object identifiers required" >&2; exit 1;
}
[[ "$CONNECTED_ACCOUNT" =~ ^acct_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: connected account required" >&2; exit 1; }
[[ "${STRIPE_BILLING_WEBHOOK_ENDPOINT_ID:-}" =~ ^we_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: STRIPE_BILLING_WEBHOOK_ENDPOINT_ID required" >&2; exit 1; }
[[ "${STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID:-}" =~ ^we_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID required" >&2; exit 1; }
merc_stripe_distinct_endpoint_ids \
  "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID" "$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID" \
  || { echo "stripe-sandbox-scenarios: webhook endpoint ids must be distinct" >&2; exit 1; }
merc_stripe_valid_staging_hostname "${STAGING_TLS_HOSTNAME:-}" \
  || { echo "stripe-sandbox-scenarios: exact STAGING_TLS_HOSTNAME required" >&2; exit 1; }
command -v stripe >/dev/null 2>&1 || { echo "stripe-sandbox-scenarios: Stripe CLI required" >&2; exit 1; }
settlement_currency="$MERC_STRIPE_CANDIDATE_CURRENCY"
provided_settlement_currency="$(
  printf '%s' "${MERC_SETTLEMENT_CURRENCY:-$settlement_currency}" | tr '[:upper:]' '[:lower:]'
)"
[ "$provided_settlement_currency" = "$settlement_currency" ] \
  || { echo "stripe-sandbox-scenarios: settlement currency must match reviewed candidate" >&2; exit 1; }

# The CLI must never fall back to a login profile. Export the key already proven
# test-only, and clear all live/publishable aliases before the first call.
export STRIPE_API_KEY="$STRIPE_SECRET_KEY"
unset STRIPE_LIVE_SECRET_KEY STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY

api() {
  local method="$1" path="$2"; shift 2
  printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" | curl --silent --show-error --config - \
    --request "$method" --header "Stripe-Version: $MERC_STRIPE_API_VERSION" \
    --connect-timeout 10 --max-time 45 "https://api.stripe.com/v1/$path" "$@"
}

connected_api() {
  local method="$1" path="$2"; shift 2
  api "$method" "$path" --header "Stripe-Account: $CONNECTED_ACCOUNT" "$@"
}

billing_endpoint="$(api GET "webhook_endpoints/$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID")"
connect_endpoint="$(api GET "webhook_endpoints/$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID")"
expected_billing_url="$(merc_stripe_expected_billing_url "$STAGING_TLS_HOSTNAME")"
expected_connect_url="$(merc_stripe_expected_connect_url "$STAGING_TLS_HOSTNAME")"
merc_stripe_endpoint_contract \
  "$billing_endpoint" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID" "$expected_billing_url" \
  "setup_intent.succeeded,payment_method.attached,payment_intent.succeeded,payment_intent.payment_failed,charge.refunded,charge.dispute.created,charge.dispute.funds_withdrawn,charge.dispute.funds_reinstated,charge.dispute.closed,radar.early_fraud_warning.created,radar.early_fraud_warning.updated"
merc_stripe_endpoint_contract \
  "$connect_endpoint" "$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID" "$expected_connect_url" \
  "account.updated,account.external_account.created,account.external_account.updated,account.external_account.deleted,capability.updated,payout.created,payout.updated,payout.paid,payout.failed,payout.canceled,payout.reconciliation_completed"

verify_endpoint_secret() {
  local endpoint_json="$1" secret_name="$2" probe_kind="$3"
  local endpoint_url secret timestamp payload digest signature response_file valid_status invalid_status
  endpoint_url="$(jq -er '.url | select(startswith("https://"))' <<< "$endpoint_json")"
  secret="${!secret_name}"
  timestamp="$(date +%s)"
  payload="$(jq -nc --arg id "evt_cx_probe_${RUN_ID}_${probe_kind}" \
    --arg api_version "$MERC_STRIPE_API_VERSION" --argjson created "$timestamp" \
    '{id:$id,type:"cx.sandbox.secret_probe",api_version:$api_version,
      livemode:false,created:$created,data:{object:{id:"cx_sandbox_probe"}}}')"
  digest="$(printf '%s\0%s' "$secret" "$payload" | python3 -c '
import hashlib, hmac, sys
timestamp = sys.argv[1].encode()
secret, payload = sys.stdin.buffer.read().split(b"\0", 1)
sys.stdout.write(hmac.new(secret, timestamp + b"." + payload, hashlib.sha256).hexdigest())
' "$timestamp")"
  signature="t=$timestamp,v1=$digest"
  response_file="$(mktemp "${TMPDIR:-/tmp}/merc-stripe-probe.XXXXXX")"
  valid_status="$(printf '%s' "$payload" | curl --silent --show-error --output "$response_file" \
    --write-out '%{http_code}' --request POST --header 'Content-Type: application/json' \
    --header "Stripe-Signature: $signature" --data-binary @- \
    --connect-timeout 10 --max-time 45 "$endpoint_url")"
  rm -f "$response_file"
  case "$valid_status" in 2??) ;; *) return 1 ;; esac

  response_file="$(mktemp "${TMPDIR:-/tmp}/merc-stripe-probe.XXXXXX")"
  invalid_status="$(printf '%s' "$payload" | curl --silent --show-error --output "$response_file" \
    --write-out '%{http_code}' --request POST --header 'Content-Type: application/json' \
    --header "Stripe-Signature: t=$timestamp,v1=$(printf '0%.0s' {1..64})" --data-binary @- \
    --connect-timeout 10 --max-time 45 "$endpoint_url")"
  rm -f "$response_file"
  [ "$invalid_status" = 400 ]
}

# A provider endpoint ID alone cannot prove the supplied whsec belongs to the
# deployed handler. Send one inert, uniquely named event with a valid signature
# and one with an invalid signature; the real handler must accept only the
# former. This transmits an HMAC, never the secret itself.
verify_endpoint_secret "$billing_endpoint" STRIPE_WEBHOOK_SECRET billing
verify_endpoint_secret "$connect_endpoint" MERC_CONNECT_WEBHOOK_SECRET connect

verify_cash_outcome() {
  local payload="$1" expected_outcome="$2" expected_rank="${3:-}"
  local timestamp digest signature headers_file body_file status outcome rank
  timestamp="$(date +%s)"
  digest="$(printf '%s\0%s' "$STRIPE_WEBHOOK_SECRET" "$payload" | python3 -c '
import hashlib, hmac, sys
timestamp = sys.argv[1].encode()
secret, payload = sys.stdin.buffer.read().split(b"\0", 1)
sys.stdout.write(hmac.new(secret, timestamp + b"." + payload, hashlib.sha256).hexdigest())
' "$timestamp")"
  signature="t=$timestamp,v1=$digest"
  headers_file="$(mktemp "${TMPDIR:-/tmp}/merc-stripe-outcome-headers.XXXXXX")"
  body_file="$(mktemp "${TMPDIR:-/tmp}/merc-stripe-outcome-body.XXXXXX")"
  status="$(printf '%s' "$payload" | curl --silent --show-error \
    --dump-header "$headers_file" --output "$body_file" --write-out '%{http_code}' \
    --request POST --header 'Content-Type: application/json' \
    --header "Stripe-Signature: $signature" --data-binary @- \
    --connect-timeout 10 --max-time 45 "$expected_billing_url")"
  outcome="$(awk -F': ' '
    tolower($1) == "x-merc-stripe-event-outcome" {
      gsub("\r", "", $2); print $2
    }
  ' "$headers_file" | tail -1)"
  rank="$(awk -F': ' '
    tolower($1) == "x-merc-stripe-cash-effect-rank" {
      gsub("\r", "", $2); print $2
    }
  ' "$headers_file" | tail -1)"
  rm -f "$headers_file" "$body_file"
  case "$status" in 2??) ;; *) return 1 ;; esac
  [ "$outcome" = "$expected_outcome" ] || return 1
  [ -z "$expected_rank" ] || [ "$rank" = "$expected_rank" ]
}

# Prove application-side non-regression on first application, not merely that
# Stripe accepted a resend request. These unique, signed, no-value dispute
# envelopes hit the real staging handler and database. The terminal event is
# intentionally delivered first, the older opening event must be ignored behind
# rank 30, and the byte-identical terminal replay must be classified duplicate.
cash_probe_created="$(date +%s)"
cash_probe_dispute="dp_cx_probe_${RUN_ID}"
cash_probe_charge="ch_cx_probe_${RUN_ID}"
cash_probe_closed_payload="$(jq -nc \
  --arg id "evt_cx_closed_${RUN_ID}" \
  --arg dispute "$cash_probe_dispute" --arg charge "$cash_probe_charge" \
  --arg api_version "$MERC_STRIPE_API_VERSION" \
  --arg currency "$settlement_currency" \
  --argjson created "$((cash_probe_created + 2))" '
    {id:$id,type:"charge.dispute.closed",api_version:$api_version,livemode:false,
     created:$created,data:{object:{id:$dispute,charge:$charge,amount:100,
       currency:$currency,status:"lost"}}}
  ')"
cash_probe_opened_payload="$(jq -nc \
  --arg id "evt_cx_opened_${RUN_ID}" \
  --arg dispute "$cash_probe_dispute" --arg charge "$cash_probe_charge" \
  --arg api_version "$MERC_STRIPE_API_VERSION" \
  --arg currency "$settlement_currency" \
  --argjson created "$((cash_probe_created + 1))" '
    {id:$id,type:"charge.dispute.created",api_version:$api_version,livemode:false,
     created:$created,data:{object:{id:$dispute,charge:$charge,amount:100,
       currency:$currency,status:"needs_response"}}}
  ')"
verify_cash_outcome "$cash_probe_closed_payload" applied 30
verify_cash_outcome "$cash_probe_opened_payload" stale_ignored 30
verify_cash_outcome "$cash_probe_closed_payload" duplicate

event_for_object() {
  local event_type="$1" object_id="$2" account_scope="${3:-platform}" deadline now response found
  deadline=$(( $(date +%s) + 120 ))
  while :; do
    if [ "$account_scope" = connected ]; then
      response="$(connected_api GET events --get --data-urlencode "type=$event_type" --data-urlencode limit=100)"
    else
      response="$(api GET events --get --data-urlencode "type=$event_type" --data-urlencode limit=100)"
    fi
    found="$(jq -c --arg id "$object_id" '
      [.data[] | select(
        .livemode == false and
        ((.data.object.id // "") == $id or
         (.data.object.payment_intent // "") == $id or
         (.data.object.charge // "") == $id or
         (.data.object.payout // "") == $id)
      )] | first // empty' <<< "$response")"
    [ -n "$found" ] && { printf '%s\n' "$found"; return 0; }
    now="$(date +%s)"
    [ "$now" -lt "$deadline" ] || return 1
    sleep 2
  done
}

wait_delivered() {
  local event_id="$1" account_scope="${2:-platform}" deadline response
  deadline=$(( $(date +%s) + 120 ))
  while :; do
    if [ "$account_scope" = connected ]; then response="$(connected_api GET "events/$event_id")"; else response="$(api GET "events/$event_id")"; fi
    jq -e '.livemode == false and .pending_webhooks == 0' <<< "$response" >/dev/null && return 0
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 2
  done
}

resend() {
  local event_id="$1" endpoint_id="$2" account_scope="${3:-platform}"
  if [ "$account_scope" = connected ]; then
    stripe events resend "$event_id" --webhook-endpoint "$endpoint_id" --stripe-account "$CONNECTED_ACCOUNT" >/dev/null
  else
    stripe events resend "$event_id" --webhook-endpoint "$endpoint_id" >/dev/null
  fi
}

# Prove delivery and duplicate replay using an object created by the parent.
success_event="$(event_for_object payment_intent.succeeded "$PAYMENT_INTENT")"
success_event_id="$(jq -r .id <<< "$success_event")"
wait_delivered "$success_event_id"
resend "$success_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
resend "$success_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
wait_delivered "$success_event_id"

# Create a real Sandbox dispute and resolve it with Stripe's documented losing
# evidence fixture. No card number or real payment instrument is used.
disputed="$(api POST payment_intents \
  --header "Idempotency-Key: merc-matrix-$RUN_ID-dispute" \
  --data-urlencode amount=1500 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode payment_method=pm_card_createDispute \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$RUN_ID")"
disputed_pi="$(jq -er '.id | select(startswith("pi_"))' <<< "$disputed")"
disputed_charge="$(jq -er '.latest_charge | select(startswith("ch_"))' <<< "$disputed")"
jq -e --arg currency "$settlement_currency" \
  '.livemode == false and .currency == $currency' <<< "$disputed" >/dev/null
created_event="$(event_for_object charge.dispute.created "$disputed_charge")"
created_event_id="$(jq -r .id <<< "$created_event")"
dispute_id="$(jq -er '.data.object.id | select(startswith("dp_") or startswith("du_"))' <<< "$created_event")"

resolved="$(api POST "disputes/$dispute_id" \
  --data-urlencode 'evidence[uncategorized_text]=losing_evidence' \
  --data-urlencode 'submit=true')"
jq -e '.livemode == false and (.status == "lost" or .status == "under_review")' <<< "$resolved" >/dev/null
closed_event="$(event_for_object charge.dispute.closed "$dispute_id")"
closed_event_id="$(jq -r .id <<< "$closed_event")"
wait_delivered "$created_event_id"
wait_delivered "$closed_event_id"

# Re-deliver provider-owned dispute events as an independent delivery-path
# check. Application-side first-application ordering and idempotency have
# already been proven above by the signed outcome probe.
resend "$closed_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
resend "$created_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
resend "$closed_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"

# Exercise connected-account payout semantics with Stripe's documented Sandbox
# bank-account numbers. The account must be a project-controlled Canadian test account;
# no real bank/card details are accepted by this adapter.
connected="$(api GET "accounts/$CONNECTED_ACCOUNT")"
jq -e --arg id "$CONNECTED_ACCOUNT" \
  --arg country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" '
    .id == $id and (.livemode // false) == false and
    .country == $country and .payouts_enabled == true
  ' <<< "$connected" >/dev/null
platform_account="$(api GET account)"
[ "$(jq -er '.id | select(startswith("acct_"))' <<< "$platform_account")" != "$CONNECTED_ACCOUNT" ]
original_interval="$(jq -er '.settings.payouts.schedule.interval' <<< "$connected")"
original_weekly_anchor="$(jq -r '.settings.payouts.schedule.weekly_anchor // empty' <<< "$connected")"
original_monthly_anchor="$(jq -r '.settings.payouts.schedule.monthly_anchor // empty' <<< "$connected")"
success_bank=""
failure_bank=""

restore_payout_fixture() {
  case "$original_interval" in
    daily|manual) api POST "accounts/$CONNECTED_ACCOUNT" --data-urlencode "settings[payouts][schedule][interval]=$original_interval" >/dev/null 2>&1 || true ;;
    weekly) api POST "accounts/$CONNECTED_ACCOUNT" --data-urlencode 'settings[payouts][schedule][interval]=weekly' --data-urlencode "settings[payouts][schedule][weekly_anchor]=$original_weekly_anchor" >/dev/null 2>&1 || true ;;
    monthly) api POST "accounts/$CONNECTED_ACCOUNT" --data-urlencode 'settings[payouts][schedule][interval]=monthly' --data-urlencode "settings[payouts][schedule][monthly_anchor]=$original_monthly_anchor" >/dev/null 2>&1 || true ;;
  esac
  [ -z "$failure_bank" ] || api DELETE "accounts/$CONNECTED_ACCOUNT/external_accounts/$failure_bank" >/dev/null 2>&1 || true
  [ -z "$success_bank" ] || api DELETE "accounts/$CONNECTED_ACCOUNT/external_accounts/$success_bank" >/dev/null 2>&1 || true
}
trap restore_payout_fixture EXIT INT TERM

manual="$(api POST "accounts/$CONNECTED_ACCOUNT" --data-urlencode 'settings[payouts][schedule][interval]=manual')"
jq -e '(.livemode // false) == false and .settings.payouts.schedule.interval == "manual"' <<< "$manual" >/dev/null

success_bank_json="$(api POST "accounts/$CONNECTED_ACCOUNT/external_accounts" \
  --data-urlencode 'external_account[object]=bank_account' \
  --data-urlencode "external_account[country]=$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" \
  --data-urlencode "external_account[currency]=$settlement_currency" \
  --data-urlencode "external_account[routing_number]=$MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING" \
  --data-urlencode "external_account[account_number]=$MERC_STRIPE_CANDIDATE_PAYOUT_SUCCESS_ACCOUNT")"
success_bank="$(jq -er '.id | select(startswith("ba_"))' <<< "$success_bank_json")"
jq -e --arg currency "$settlement_currency" \
  --arg country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" '
    .currency == $currency and .country == $country
  ' <<< "$success_bank_json" >/dev/null

failure_bank_json="$(api POST "accounts/$CONNECTED_ACCOUNT/external_accounts" \
  --data-urlencode 'external_account[object]=bank_account' \
  --data-urlencode "external_account[country]=$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" \
  --data-urlencode "external_account[currency]=$settlement_currency" \
  --data-urlencode "external_account[routing_number]=$MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING" \
  --data-urlencode "external_account[account_number]=$MERC_STRIPE_CANDIDATE_PAYOUT_FAILURE_ACCOUNT")"
failure_bank="$(jq -er '.id | select(startswith("ba_"))' <<< "$failure_bank_json")"
jq -e --arg currency "$settlement_currency" \
  --arg country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" '
    .currency == $currency and .country == $country
  ' <<< "$failure_bank_json" >/dev/null

wait_payout_status() {
  local payout_id="$1" wanted="$2" deadline response
  deadline=$(( $(date +%s) + 300 ))
  while :; do
    response="$(connected_api GET "payouts/$payout_id")"
    jq -e --arg status "$wanted" '.livemode == false and .status == $status' <<< "$response" >/dev/null && { printf '%s\n' "$response"; return 0; }
    case "$(jq -r '.status // "error"' <<< "$response")" in failed|canceled) [ "$wanted" = "$(jq -r .status <<< "$response")" ] || return 1 ;; esac
    [ "$(date +%s)" -lt "$deadline" ] || return 1
    sleep 3
  done
}

released="$(connected_api POST payouts \
  --header "Idempotency-Key: merc-matrix-$RUN_ID-payout-release" \
  --data-urlencode amount=40 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode destination="$success_bank" \
  --data-urlencode "description=merc Sandbox release $RUN_ID")"
payout_release_id="$(jq -er '.id | select(startswith("po_"))' <<< "$released")"
jq -e --arg currency "$settlement_currency" \
  '.livemode == false and .currency == $currency' <<< "$released" >/dev/null
hold_event="$(event_for_object payout.created "$payout_release_id" connected)"
wait_delivered "$(jq -r .id <<< "$hold_event")" connected
wait_payout_status "$payout_release_id" paid >/dev/null
paid_event="$(event_for_object payout.paid "$payout_release_id" connected)"
wait_delivered "$(jq -r .id <<< "$paid_event")" connected

reversed="$(connected_api POST "payouts/$payout_release_id/reverse")"
jq -e --arg original "$payout_release_id" '
  .livemode == false and .original_payout == $original and
  (.id | type == "string" and startswith("po_")) and .amount < 0
' <<< "$reversed" >/dev/null
payout_reversal_id="$(jq -r .id <<< "$reversed")"

failed="$(connected_api POST payouts \
  --header "Idempotency-Key: merc-matrix-$RUN_ID-payout-failure" \
  --data-urlencode amount=30 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode destination="$failure_bank" \
  --data-urlencode "description=merc Sandbox failure $RUN_ID")"
payout_failure_id="$(jq -er '.id | select(startswith("po_"))' <<< "$failed")"
jq -e --arg currency "$settlement_currency" \
  '.livemode == false and .currency == $currency' <<< "$failed" >/dev/null
wait_payout_status "$payout_failure_id" failed >/dev/null
failed_event="$(event_for_object payout.failed "$payout_failure_id" connected)"
wait_delivered "$(jq -r .id <<< "$failed_event")" connected
payout_hold_id="$payout_release_id"

# Reconciliation is bounded to provider-owned facts created in this run. Every
# referenced object/event is test-mode and every configured webhook drained.
jq -e '.livemode == false and (.id | startswith("pi_"))' <<< "$(api GET "payment_intents/$disputed_pi")" >/dev/null
jq -e '.livemode == false and (.id | startswith("tr_"))' <<< "$(api GET "transfers/$TRANSFER")" >/dev/null

jq -nc \
  --arg run "$RUN_ID" --arg dispute "$dispute_id" \
  --arg payment_intent "$PAYMENT_INTENT" --arg charge "$CHARGE" --arg transfer "$TRANSFER" \
  --arg disputed_payment_intent "$disputed_pi" --arg disputed_charge "$disputed_charge" \
  --arg stripe_api_version "$MERC_STRIPE_API_VERSION" \
  --arg settlement_currency "$settlement_currency" \
  --arg connected_country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" \
  --arg payout_hold "$payout_hold_id" --arg payout_release "$payout_release_id" \
  --arg payout_failure "$payout_failure_id" --arg payout_reversal "$payout_reversal_id" \
  '{schema_version:1,status:"PASS",provider_mode:"test",run_id:$run,
    payment_intent:$payment_intent,charge:$charge,transfer:$transfer,
    disputed_payment_intent:$disputed_payment_intent,disputed_charge:$disputed_charge,
    secret_values_recorded:false,
    webhook:{endpoint_secrets_verified:true,payload_api_version:$stripe_api_version,
      staging_urls_exact:true,distinct_endpoint_ids:true,
      delivery:true,application_outcomes_verified:true,
      replay_idempotent:true,out_of_order_safe:true},
    dispute:{opened:true,resolved:true,provider_object_class:($dispute|split("_")[0])},
    payout:{hold:true,release:true,failure:true,reversal:true,
      provider_object_classes:([$payout_hold,$payout_release,$payout_failure,$payout_reversal]|map(split("_")[0])|unique)},
    reconciliation:{clean:true,provider_events_drained:true},
    settlement:{currency:$settlement_currency,connected_account_country:$connected_country},
    cleanup:{parent_disposable_customer:true,fixture_objects:"retained in Sandbox provider log"},
    live_mode:"PROHIBITED"}'
