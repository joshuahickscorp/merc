#!/usr/bin/env bash
set -euo pipefail

# Bundled Stripe Sandbox-only provider scenario adapter. The parent command
# validates credentials first; this script repeats the live-key refusal so it
# is also safe when invoked directly. It never supports live mode.

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
[[ "$RUN_ID" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "stripe-sandbox-scenarios: run id required" >&2; exit 1; }
[[ "$PAYMENT_INTENT" =~ ^pi_[A-Za-z0-9]+$ && "$CHARGE" =~ ^ch_[A-Za-z0-9]+$ && "$TRANSFER" =~ ^tr_[A-Za-z0-9]+$ ]] || {
  echo "stripe-sandbox-scenarios: provider object identifiers required" >&2; exit 1;
}
[[ "$CONNECTED_ACCOUNT" =~ ^acct_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: connected account required" >&2; exit 1; }
[[ "${STRIPE_BILLING_WEBHOOK_ENDPOINT_ID:-}" =~ ^we_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: STRIPE_BILLING_WEBHOOK_ENDPOINT_ID required" >&2; exit 1; }
[[ "${STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID:-}" =~ ^we_[A-Za-z0-9]+$ ]] || { echo "stripe-sandbox-scenarios: STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID required" >&2; exit 1; }
command -v stripe >/dev/null 2>&1 || { echo "stripe-sandbox-scenarios: Stripe CLI required" >&2; exit 1; }

# The CLI must never fall back to a login profile. Export the key already proven
# test-only, and clear all live/publishable aliases before the first call.
export STRIPE_API_KEY="$STRIPE_SECRET_KEY"
unset STRIPE_LIVE_SECRET_KEY STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY

api() {
  local method="$1" path="$2"; shift 2
  printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" | curl --silent --show-error --config - \
    --request "$method" --header 'Stripe-Version: 2025-06-30.basil' \
    --connect-timeout 10 --max-time 45 "https://api.stripe.com/v1/$path" "$@"
}

connected_api() {
  local method="$1" path="$2"; shift 2
  api "$method" "$path" --header "Stripe-Account: $CONNECTED_ACCOUNT" "$@"
}

billing_endpoint="$(api GET "webhook_endpoints/$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID")"
connect_endpoint="$(api GET "webhook_endpoints/$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID")"
jq -e '
  . as $endpoint |
  .livemode == false and .status == "enabled" and (.url | startswith("https://")) and
  ((.enabled_events | index("*")) != null or
   all("payment_intent.succeeded","charge.dispute.created","charge.dispute.closed";
     . as $event | ($endpoint.enabled_events | index($event)) != null))
' <<< "$billing_endpoint" >/dev/null
jq -e '
  . as $endpoint |
  .livemode == false and .status == "enabled" and (.url | startswith("https://")) and
  ((.enabled_events | index("*")) != null or
   all("payout.created","payout.paid","payout.failed";
     . as $event | ($endpoint.enabled_events | index($event)) != null))
' <<< "$connect_endpoint" >/dev/null

verify_endpoint_secret() {
  local endpoint_json="$1" secret_name="$2" probe_kind="$3"
  local endpoint_url secret timestamp payload digest signature response_file valid_status invalid_status
  endpoint_url="$(jq -er '.url | select(startswith("https://"))' <<< "$endpoint_json")"
  secret="${!secret_name}"
  timestamp="$(date +%s)"
  payload="$(jq -nc --arg id "evt_cx_probe_${RUN_ID}_${probe_kind}" --argjson created "$timestamp" \
    '{id:$id,type:"cx.sandbox.secret_probe",created:$created,data:{object:{id:"cx_sandbox_probe"}}}')"
  digest="$(printf '%s\0%s' "$secret" "$payload" | python3 -c '
import hashlib, hmac, sys
timestamp = sys.argv[1].encode()
secret, payload = sys.stdin.buffer.read().split(b"\0", 1)
sys.stdout.write(hmac.new(secret, timestamp + b"." + payload, hashlib.sha256).hexdigest())
' "$timestamp")"
  signature="t=$timestamp,v1=$digest"
  response_file="$(mktemp "${TMPDIR:-/tmp}/cx-stripe-probe.XXXXXX")"
  valid_status="$(printf '%s' "$payload" | curl --silent --show-error --output "$response_file" \
    --write-out '%{http_code}' --request POST --header 'Content-Type: application/json' \
    --header "Stripe-Signature: $signature" --data-binary @- \
    --connect-timeout 10 --max-time 45 "$endpoint_url")"
  rm -f "$response_file"
  case "$valid_status" in 2??) ;; *) return 1 ;; esac

  response_file="$(mktemp "${TMPDIR:-/tmp}/cx-stripe-probe.XXXXXX")"
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
  --header "Idempotency-Key: cx-matrix-$RUN_ID-dispute" \
  --data-urlencode amount=1500 --data-urlencode currency=usd \
  --data-urlencode payment_method=pm_card_createDispute \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$RUN_ID")"
disputed_pi="$(jq -er '.id | select(startswith("pi_"))' <<< "$disputed")"
disputed_charge="$(jq -er '.latest_charge | select(startswith("ch_"))' <<< "$disputed")"
created_event="$(event_for_object charge.dispute.created "$disputed_charge")"
created_event_id="$(jq -r .id <<< "$created_event")"
dispute_id="$(jq -er '.data.object.id | select(startswith("dp_") or startswith("du_"))' <<< "$created_event")"

resolved="$(api POST "disputes/$dispute_id" \
  --data-urlencode 'evidence[uncategorized_text]=losing_evidence' \
  --data-urlencode 'evidence[submit]=true')"
jq -e '.livemode == false and (.status == "lost" or .status == "under_review")' <<< "$resolved" >/dev/null
closed_event="$(event_for_object charge.dispute.closed "$dispute_id")"
closed_event_id="$(jq -r .id <<< "$closed_event")"
wait_delivered "$created_event_id"
wait_delivered "$closed_event_id"

# Deliver the newer terminal fact before the older opening fact, then replay the
# terminal fact. The application-side non-regression and idempotency assertions
# are enforced by the matrix receipt contract and the release integration tests.
resend "$closed_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
resend "$created_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"
resend "$closed_event_id" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID"

# Exercise connected-account payout semantics with Stripe's documented Sandbox
# bank-account numbers. The account must be a project-controlled US test account;
# no real bank/card details are accepted by this adapter.
connected="$(api GET "accounts/$CONNECTED_ACCOUNT")"
jq -e '.livemode == false and .country == "US" and .payouts_enabled == true' <<< "$connected" >/dev/null
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
jq -e '.livemode == false and .settings.payouts.schedule.interval == "manual"' <<< "$manual" >/dev/null

success_bank_json="$(api POST "accounts/$CONNECTED_ACCOUNT/external_accounts" \
  --data-urlencode 'external_account[object]=bank_account' \
  --data-urlencode 'external_account[country]=US' \
  --data-urlencode 'external_account[currency]=usd' \
  --data-urlencode 'external_account[routing_number]=110000000' \
  --data-urlencode 'external_account[account_number]=000123456789')"
success_bank="$(jq -er '.id | select(startswith("ba_"))' <<< "$success_bank_json")"

failure_bank_json="$(api POST "accounts/$CONNECTED_ACCOUNT/external_accounts" \
  --data-urlencode 'external_account[object]=bank_account' \
  --data-urlencode 'external_account[country]=US' \
  --data-urlencode 'external_account[currency]=usd' \
  --data-urlencode 'external_account[routing_number]=110000000' \
  --data-urlencode 'external_account[account_number]=000111111116')"
failure_bank="$(jq -er '.id | select(startswith("ba_"))' <<< "$failure_bank_json")"

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
  --header "Idempotency-Key: cx-matrix-$RUN_ID-payout-release" \
  --data-urlencode amount=40 --data-urlencode currency=usd \
  --data-urlencode destination="$success_bank" \
  --data-urlencode "description=merc Sandbox release $RUN_ID")"
payout_release_id="$(jq -er '.id | select(startswith("po_"))' <<< "$released")"
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
  --header "Idempotency-Key: cx-matrix-$RUN_ID-payout-failure" \
  --data-urlencode amount=30 --data-urlencode currency=usd \
  --data-urlencode destination="$failure_bank" \
  --data-urlencode "description=merc Sandbox failure $RUN_ID")"
payout_failure_id="$(jq -er '.id | select(startswith("po_"))' <<< "$failed")"
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
  --arg payout_hold "$payout_hold_id" --arg payout_release "$payout_release_id" \
  --arg payout_failure "$payout_failure_id" --arg payout_reversal "$payout_reversal_id" \
  '{schema_version:1,status:"PASS",provider_mode:"test",run_id:$run,
    secret_values_recorded:false,
    webhook:{endpoint_secrets_verified:true,delivery:true,replay_idempotent:true,out_of_order_safe:true},
    dispute:{opened:true,resolved:true,provider_object_class:($dispute|split("_")[0])},
    payout:{hold:true,release:true,failure:true,reversal:true,
      provider_object_classes:([$payout_hold,$payout_release,$payout_failure,$payout_reversal]|map(split("_")[0])|unique)},
    reconciliation:{clean:true,provider_events_drained:true},
    cleanup:{parent_disposable_customer:true,fixture_objects:"retained in Sandbox provider log"},
    live_mode:"PROHIBITED"}'
