#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/ops/scripts/lib/stripe-sandbox-contract.sh"
MODE="${1:-}"
case "$MODE" in check|matrix|nonconnect|connect) ;; *) echo "usage: ops/scripts/stripe-sandbox.sh check|matrix|nonconnect|connect" >&2; exit 2 ;; esac

ENV_FILE="${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"
if [ -f "$ENV_FILE" ]; then
  [ ! -L "$ENV_FILE" ] || { echo "stripe-sandbox: environment file must not be a symlink" >&2; exit 1; }
  mode="$(stat -f '%Lp' "$ENV_FILE" 2>/dev/null || stat -c '%a' "$ENV_FILE")"
  case "$mode" in 600|400) ;; *) echo "stripe-sandbox: environment file must have mode 0600 or 0400" >&2; exit 1 ;; esac
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

classify() {
  case "$1" in
    '') echo missing ;;
    sk_test_*|rk_test_*) echo test ;;
    sk_live_*|rk_live_*|pk_live_*) echo live ;;
    whsec_*) echo webhook ;;
    *) echo unknown ;;
  esac
}

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  value="${!name:-}"
  if [ "$(classify "$value")" = live ]; then
    jq -nc --arg mode "$MODE" --arg variable "$name" \
      '{schema_version:1,command:$mode,status:"LIVE CREDENTIAL REFUSED",network_accessed:false,
        secret_values_printed:false,refused_variable:$variable}'
    exit 1
  fi
done

# Connect remainder creates the connected account. Do not require acct_/ca_
# as inputs; those are what this command produces after Connect signup.
if [ "$MODE" = connect ]; then
  shift
  exec "$ROOT/ops/scripts/stripe-sandbox-connect.sh" "$@"
fi

if [ "$MODE" = nonconnect ]; then
  exec "$ROOT/ops/scripts/stripe-sandbox-nonconnect.sh"
fi

if [ "$MODE" = matrix ]; then
  # One freshly generated run_id covers every row — non-Connect and Connect.
  # The Python drivers write evidence/external/stripe-sandbox-matrix.json
  # and stamp it with ops/scripts/lib/receipt_binding.py. The older bash body
  # below is the `check` path plus a retained endpoint-contract authority;
  # it is not the matrix producer.
  unset STRIPE_LIVE_SECRET_KEY
  if [[ "${STRIPE_RESTRICTED_KEY:-}" == rk_live_* ]]; then unset STRIPE_RESTRICTED_KEY; fi
  if [[ "${STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset STRIPE_PUBLISHABLE_KEY; fi
  if [[ "${NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; fi
  export STAGING_TLS_HOSTNAME="${STAGING_TLS_HOSTNAME:-mercmerc.net}"
  export MERC_SETTLEMENT_CURRENCY="${MERC_SETTLEMENT_CURRENCY:-$MERC_STRIPE_CANDIDATE_CURRENCY}"
  export MERC_STRIPE_RUN_ID="${MERC_STRIPE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-full}"
  export MERC_STRIPE_MATRIX_OUT="${MERC_STRIPE_MATRIX_OUT:-$ROOT/evidence/external/stripe-sandbox-matrix.json}"
  export MERC_STRIPE_FULL_MATRIX=1
  export MERC_STRIPE_API_VERSION
  export MERC_STRIPE_CANDIDATE_CURRENCY
  export MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY
  export MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING
  export MERC_STRIPE_CANDIDATE_PAYOUT_SUCCESS_ACCOUNT
  export MERC_STRIPE_CANDIDATE_PAYOUT_FAILURE_ACCOUNT
  export MERC_STRIPE_CONNECT_ACCOUNT_TYPE
  export MERC_STRIPE_CONNECT_WEBHOOK_EVENTS
  export MERC_STRIPE_CONNECT_REMAINDER_COMMAND
  "$ROOT/ops/scripts/stripe-sandbox-nonconnect.sh"
  exec "$ROOT/ops/scripts/stripe-sandbox-connect.sh"
fi

secret_class="$(classify "${STRIPE_SECRET_KEY:-}")"
billing_webhook_class="$(classify "${STRIPE_WEBHOOK_SECRET:-}")"
connect_webhook_class="$(classify "${MERC_CONNECT_WEBHOOK_SECRET:-}")"
connected_account_ready=false
[[ "${STRIPE_TEST_CONNECTED_ACCOUNT_ID:-}" =~ ^acct_[A-Za-z0-9]+$ ]] && connected_account_ready=true
staging_hostname_ready=false
merc_stripe_valid_staging_hostname "${STAGING_TLS_HOSTNAME:-}" && staging_hostname_ready=true
endpoint_ids_distinct=false
merc_stripe_distinct_endpoint_ids \
  "${STRIPE_BILLING_WEBHOOK_ENDPOINT_ID:-}" \
  "${STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID:-}" && endpoint_ids_distinct=true
settlement_currency="$MERC_STRIPE_CANDIDATE_CURRENCY"
provided_settlement_currency="$(
  printf '%s' "${MERC_SETTLEMENT_CURRENCY:-$settlement_currency}" | tr '[:upper:]' '[:lower:]'
)"

ready=true
[ "$secret_class" = test ] || ready=false
[ "$billing_webhook_class" = webhook ] || ready=false
[ "$connect_webhook_class" = webhook ] || ready=false
[[ "${STRIPE_SECRET_KEY:-}" =~ ^(sk_test_|rk_test_)[A-Za-z0-9_]+$ ]] || ready=false
[[ "${STRIPE_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || ready=false
[[ "${MERC_CONNECT_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || ready=false
[ "${STRIPE_WEBHOOK_SECRET:-}" != "${MERC_CONNECT_WEBHOOK_SECRET:-}" ] || ready=false
[ "$connected_account_ready" = true ] || ready=false
[[ "${MERC_CONNECT_CLIENT_ID:-}" =~ ^ca_[A-Za-z0-9]+$ ]] || ready=false
[ "$staging_hostname_ready" = true ] || ready=false
[ "$endpoint_ids_distinct" = true ] || ready=false
[ "$provided_settlement_currency" = "$settlement_currency" ] || ready=false

if [ "$ready" != true ]; then
  # check stays a no-network preflight. matrix/nonconnect still drive every
  # scenario that is not gated on Connect signup when the test key and
  # reviewed currency/hostname are present.
  connect_only_gap=true
  [ "$secret_class" = test ] || connect_only_gap=false
  [ "$staging_hostname_ready" = true ] || connect_only_gap=false
  [ "$provided_settlement_currency" = "$settlement_currency" ] || connect_only_gap=false
  if { [ "$MODE" = matrix ] || [ "$MODE" = nonconnect ]; } && [ "$connect_only_gap" = true ]; then
    echo "stripe-sandbox: Connect inputs missing; driving non-Connect matrix" >&2
    exec "$ROOT/ops/scripts/stripe-sandbox-nonconnect.sh"
  fi
  jq -nc --arg mode "$MODE" --arg secret "$secret_class" \
    --arg billing "$billing_webhook_class" --arg connect "$connect_webhook_class" \
    --argjson account "$connected_account_ready" \
    --argjson staging_hostname "$staging_hostname_ready" \
    --argjson endpoint_ids_distinct "$endpoint_ids_distinct" \
    --arg settlement_currency "$settlement_currency" \
    '{schema_version:1,command:$mode,status:"EXTERNAL CREDENTIAL REQUIRED",network_accessed:false,
      secret_values_printed:false,classes:{api_key:$secret,billing_webhook:$billing,connect_webhook:$connect},
      connect_test_account_present:$account,staging_hostname_valid:$staging_hostname,
      endpoint_ids_distinct:$endpoint_ids_distinct,
      reviewed_settlement_currency:$settlement_currency,live_mode:"PROHIBITED"}'
  exit 1
fi

# Live aliases are removed from the environment inherited by every network
# subprocess. The generic key has already been proven test-class above.
unset STRIPE_LIVE_SECRET_KEY STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY
if [[ "${STRIPE_RESTRICTED_KEY:-}" == rk_live_* ]]; then unset STRIPE_RESTRICTED_KEY; fi

api() {
  local method="$1" path="$2"; shift 2
  # Keep even the test key out of argv/process listings. curl consumes this
  # one-line configuration from stdin before it opens the provider connection.
  printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" | curl --silent --show-error --config - --request "$method" \
    --header "Stripe-Version: $MERC_STRIPE_API_VERSION" \
    --connect-timeout 10 --max-time 45 \
    "https://api.stripe.com/v1/$path" "$@"
}

api_expect_timeout() {
  local method="$1" path="$2" status
  shift 2
  # An intentionally impossible client deadline proves the command's timeout
  # path without a proxy or operator-supplied driver. The exact idempotency key
  # is retried below, so the unknown provider-persistence outcome is recovered
  # without intentionally creating a second provider object.
  set +e
  printf 'user = "%s:"\n' "$STRIPE_SECRET_KEY" | curl --silent --show-error --config - \
    --request "$method" --header "Stripe-Version: $MERC_STRIPE_API_VERSION" \
    --connect-timeout 0.001 --max-time 0.001 \
    "https://api.stripe.com/v1/$path" "$@" >/dev/null
  status=$?
  set -e
  [ "$status" = 28 ]
}

account="$(api GET account)"
jq -e '.id | type == "string" and startswith("acct_")' <<< "$account" >/dev/null
# Stripe no longer returns `livemode` on the Account object (verified absent on
# both the platform and a connected account under the pinned 2025-06-30.basil
# version). Fail closed if it is ever present and true. The hard test-mode
# guarantee remains the sk_test_/rk_test_ key gate above, plus the strict
# `.livemode == false` assertions on PaymentIntent and transfer below, which
# Stripe does still populate.
jq -e '(.livemode // false) == false' <<< "$account" >/dev/null
api_account_id="$(jq -r .id <<< "$account")"

connect="$(api GET "accounts/${STRIPE_TEST_CONNECTED_ACCOUNT_ID}")"
jq -e \
  --arg id "$STRIPE_TEST_CONNECTED_ACCOUNT_ID" \
  --arg country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" '
    .id == $id and
    .country == $country and
    .payouts_enabled == true
  ' <<< "$connect" >/dev/null
jq -e '(.livemode // false) == false' <<< "$connect" >/dev/null
[ "$api_account_id" != "$STRIPE_TEST_CONNECTED_ACCOUNT_ID" ]

# The candidate settles in one reviewed currency. A platform with no matching
# bucket accepts the
# transfer request and fails it with balance_insufficient once money moves, so
# report it here rather than mid-matrix. Mirrors control's boot preflight.
balance="$(api GET balance)"
settles_currency=false
jq -e --arg currency "$settlement_currency" \
  '[.available[].currency, .pending[].currency] | index($currency)' \
  <<< "$balance" >/dev/null 2>&1 && settles_currency=true

billing_endpoint="$(api GET "webhook_endpoints/$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID")"
connect_endpoint="$(api GET "webhook_endpoints/$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID")"
expected_billing_url="$(merc_stripe_expected_billing_url "$STAGING_TLS_HOSTNAME")"
expected_connect_url="$(merc_stripe_expected_connect_url "$STAGING_TLS_HOSTNAME")"
merc_stripe_endpoint_contract \
  "$billing_endpoint" "$STRIPE_BILLING_WEBHOOK_ENDPOINT_ID" "$expected_billing_url" \
  "setup_intent.succeeded,payment_method.attached,payment_intent.succeeded,payment_intent.payment_failed,charge.refunded,charge.dispute.created,charge.dispute.funds_withdrawn,charge.dispute.funds_reinstated,charge.dispute.closed,radar.early_fraud_warning.created,radar.early_fraud_warning.updated"
merc_stripe_endpoint_contract \
  "$connect_endpoint" "$STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID" "$expected_connect_url" \
  "account.updated,payout.created,payout.paid,payout.failed"

if [ "$MODE" = check ]; then
  # A platform that cannot settle the reviewed runtime currency cannot complete
  # the matrix, so this is a blocking configuration fault.
  check_status=PASS
  [ "$settles_currency" = true ] || check_status=FAIL
  jq -nc --arg status "$check_status" --arg account_class "${api_account_id%%_*}" \
    --arg currency "$settlement_currency" \
    --arg connected_country "$MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY" \
    --argjson settles_currency "$settles_currency" \
    --arg enabled "$(jq -r '[.available[].currency, .pending[].currency] | unique | join(" ")' <<< "$balance")" \
    '{schema_version:1,kind:"stripe_sandbox_check",status:$status,provider_mode:"test",
      network_accessed:true,secret_values_printed:false,account_identifier_class:$account_class,
      webhook_secrets:{billing:"present",connect:"present",distinct:true},
      webhook_endpoints:{distinct:true,staging_urls_exact:true,payload_api_version_pinned:true},
      connect:{sandbox_account_verified:true,distinct_from_platform:true,
        country:$connected_country,payouts_enabled:true},
      settlement:{ledger_currency:$currency,platform_settles_currency:$settles_currency,
                  enabled:($enabled|split(" ")|map(select(length>0)))},
      live_mode:"PROHIBITED"}'
  [ "$check_status" = PASS ] || {
    printf 'stripe-check: FAIL - platform settles [%s], candidate requires %s; supplier payouts cannot execute\n' \
      "$(jq -r '[.available[].currency, .pending[].currency] | unique | join(",")' <<< "$balance")" \
      "$settlement_currency" >&2
    exit 1
  }
  exit 0
fi

run_id="$(date -u +%Y%m%dT%H%M%SZ)-$$"
customer=""
cleanup() {
  if [ -n "$customer" ]; then
    api DELETE "customers/$customer" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

customer_json="$(api POST customers \
  --data-urlencode "description=merc disposable Sandbox matrix $run_id" \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
customer="$(jq -er '.id | select(startswith("cus_"))' <<< "$customer_json")"

timeout_idem="merc-matrix-$run_id-timeout"
api_expect_timeout POST payment_intents \
  --header "Idempotency-Key: $timeout_idem" \
  --data-urlencode amount=900 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id"
timeout_recovery="$(api POST payment_intents \
  --header "Idempotency-Key: $timeout_idem" \
  --data-urlencode amount=900 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e --arg currency "$settlement_currency" '
  .livemode == false and .status == "succeeded" and .currency == $currency and
  (.id | startswith("pi_"))
' \
  <<< "$timeout_recovery" >/dev/null

idem="merc-matrix-$run_id-success"
success="$(api POST payment_intents \
  --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1200 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
pi="$(jq -er '.id | select(startswith("pi_"))' <<< "$success")"
jq -e --arg currency "$settlement_currency" \
  '.livemode == false and .status == "succeeded" and .currency == $currency' \
  <<< "$success" >/dev/null

retry="$(api POST payment_intents \
  --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1200 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
[ "$(jq -r .id <<< "$retry")" = "$pi" ]

conflict="$(api POST payment_intents --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1201 --data-urlencode "currency=$settlement_currency")"
jq -e '.error.type == "idempotency_error"' <<< "$conflict" >/dev/null

decline="$(api POST payment_intents \
  --header "Idempotency-Key: merc-matrix-$run_id-decline" \
  --data-urlencode amount=700 --data-urlencode "currency=$settlement_currency" \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_chargeDeclined \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true)"
jq -e '.error.decline_code != null or .error.code == "card_declined"' <<< "$decline" >/dev/null

charge="$(jq -er '.latest_charge | select(startswith("ch_"))' <<< "$success")"
partial="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=200 \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e --arg currency "$settlement_currency" \
  '.status == "succeeded" and .amount == 200 and .currency == $currency' \
  <<< "$partial" >/dev/null
excess="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=1200)"
jq -e '.error != null' <<< "$excess" >/dev/null
remaining="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=1000 \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e --arg currency "$settlement_currency" \
  '.status == "succeeded" and .amount == 1000 and .currency == $currency' \
  <<< "$remaining" >/dev/null

transfer="$(api POST transfers --data-urlencode amount=100 \
  --data-urlencode "currency=$settlement_currency" \
  --data-urlencode destination="$STRIPE_TEST_CONNECTED_ACCOUNT_ID" \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e '.id | type == "string" and startswith("tr_")' <<< "$transfer" >/dev/null
jq -e --arg currency "$settlement_currency" \
  '.livemode == false and .currency == $currency' <<< "$transfer" >/dev/null

# Provider-created webhook delivery, dispute lifecycle, and connected-account
# payout outcomes remain mandatory. The adapter is bundled so credentials and
# endpoint identifiers are the only missing inputs; no release-day code is
# supplied through an operator-controlled executable.
driver="$ROOT/ops/scripts/stripe-sandbox-scenarios.sh"
[ -x "$driver" ] || { echo "stripe-sandbox: bundled scenario driver is not executable" >&2; exit 1; }

driver_receipt="$($driver --run-id "$run_id" --payment-intent "$pi" --charge "$charge" \
  --transfer "$(jq -r .id <<< "$transfer")" --connected-account "$STRIPE_TEST_CONNECTED_ACCOUNT_ID")"
jq -e '
  .schema_version == 1 and .status == "PASS" and .provider_mode == "test" and
  .secret_values_recorded == false and .webhook.endpoint_secrets_verified == true and
  .webhook.delivery == true and .webhook.staging_urls_exact == true and
  .webhook.application_outcomes_verified == true and
  .webhook.replay_idempotent == true and .webhook.out_of_order_safe == true and
  .dispute.opened == true and .dispute.resolved == true and
  .payout.hold == true and .payout.release == true and .payout.failure == true and
  .payout.reversal == true and .reconciliation.clean == true and
  .settlement.currency == $currency and
  ([.. | strings | select(test("sk_(test|live)_|rk_(test|live)_|whsec_"))] | length) == 0
' --arg currency "$settlement_currency" <<< "$driver_receipt" >/dev/null

jq -nc --arg run "$run_id" --arg currency "$settlement_currency" --argjson driver "$driver_receipt" \
  '{schema_version:1,kind:"stripe_sandbox_matrix",status:"PASS",provider_mode:"test",
    run_id:$run,secret_values_printed:false,disposable_customer_cleanup:"attempted",
    settlement_currency:$currency,
    payment_objects:{authorization:true,capture:true,decline:true,idempotency:true,refunds:true,transfer:true,
      timeout:{client_deadline:true,idempotent_recovery:true}},
    external_scenarios:$driver,live_mode:"PROHIBITED"}'
