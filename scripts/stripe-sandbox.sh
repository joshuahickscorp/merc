#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
MODE="${1:-}"
case "$MODE" in check|matrix) ;; *) echo "usage: scripts/stripe-sandbox.sh check|matrix" >&2; exit 2 ;; esac

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

secret_class="$(classify "${STRIPE_SECRET_KEY:-}")"
billing_webhook_class="$(classify "${STRIPE_WEBHOOK_SECRET:-}")"
connect_webhook_class="$(classify "${MERC_CONNECT_WEBHOOK_SECRET:-}")"
connected_account_ready=false
[[ "${STRIPE_TEST_CONNECTED_ACCOUNT_ID:-}" =~ ^acct_[A-Za-z0-9]+$ ]] && connected_account_ready=true

ready=true
[ "$secret_class" = test ] || ready=false
[ "$billing_webhook_class" = webhook ] || ready=false
[ "$connect_webhook_class" = webhook ] || ready=false
[[ "${STRIPE_SECRET_KEY:-}" =~ ^(sk_test_|rk_test_)[A-Za-z0-9_]+$ ]] || ready=false
[[ "${STRIPE_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || ready=false
[[ "${MERC_CONNECT_WEBHOOK_SECRET:-}" =~ ^whsec_[A-Za-z0-9_]+$ ]] || ready=false
[ "${STRIPE_WEBHOOK_SECRET:-}" != "${MERC_CONNECT_WEBHOOK_SECRET:-}" ] || ready=false
[ "$connected_account_ready" = true ] || ready=false

if [ "$ready" != true ]; then
  jq -nc --arg mode "$MODE" --arg secret "$secret_class" \
    --arg billing "$billing_webhook_class" --arg connect "$connect_webhook_class" \
    --argjson account "$connected_account_ready" \
    '{schema_version:1,command:$mode,status:"EXTERNAL CREDENTIAL REQUIRED",network_accessed:false,
      secret_values_printed:false,classes:{api_key:$secret,billing_webhook:$billing,connect_webhook:$connect},
      connect_test_account_present:$account,live_mode:"PROHIBITED"}'
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
    --header 'Stripe-Version: 2025-06-30.basil' \
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
    --request "$method" --header 'Stripe-Version: 2025-06-30.basil' \
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
jq -e '.id | type == "string" and startswith("acct_")' <<< "$connect" >/dev/null
jq -e '(.livemode // false) == false' <<< "$connect" >/dev/null

# The ledger settles USD only. A platform with no USD bucket accepts the
# transfer request and fails it with balance_insufficient once money moves, so
# report it here rather than mid-matrix. Mirrors control's boot preflight.
balance="$(api GET balance)"
settles_usd=false
jq -e '[.available[].currency, .pending[].currency] | index("usd")' <<< "$balance" >/dev/null 2>&1 && settles_usd=true

if [ "$MODE" = check ]; then
  # A platform that cannot settle USD cannot complete the matrix, so this is a
  # blocking configuration fault rather than an advisory note.
  check_status=PASS
  [ "$settles_usd" = true ] || check_status=FAIL
  jq -nc --arg status "$check_status" --arg account_class "${api_account_id%%_*}" \
    --argjson settles_usd "$settles_usd" \
    --arg enabled "$(jq -r '[.available[].currency, .pending[].currency] | unique | join(" ")' <<< "$balance")" \
    '{schema_version:1,kind:"stripe_sandbox_check",status:$status,provider_mode:"test",
      network_accessed:true,secret_values_printed:false,account_identifier_class:$account_class,
      webhook_secrets:{billing:"present",connect:"present",distinct:true},
      connect:{sandbox_account_verified:true},
      settlement:{ledger_currency:"usd",platform_settles_usd:$settles_usd,
                  enabled:($enabled|split(" ")|map(select(length>0)))},
      live_mode:"PROHIBITED"}'
  [ "$check_status" = PASS ] || {
    printf 'stripe-check: FAIL - platform settles [%s], ledger requires usd; supplier payouts cannot execute\n' \
      "$(jq -r '[.available[].currency, .pending[].currency] | unique | join(",")' <<< "$balance")" >&2
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
  --data-urlencode amount=900 --data-urlencode currency=usd \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id"
timeout_recovery="$(api POST payment_intents \
  --header "Idempotency-Key: $timeout_idem" \
  --data-urlencode amount=900 --data-urlencode currency=usd \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e '.livemode == false and .status == "succeeded" and (.id | startswith("pi_"))' \
  <<< "$timeout_recovery" >/dev/null

idem="merc-matrix-$run_id-success"
success="$(api POST payment_intents \
  --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1200 --data-urlencode currency=usd \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
pi="$(jq -er '.id | select(startswith("pi_"))' <<< "$success")"
jq -e '.livemode == false and .status == "succeeded"' <<< "$success" >/dev/null

retry="$(api POST payment_intents \
  --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1200 --data-urlencode currency=usd \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_visa \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
[ "$(jq -r .id <<< "$retry")" = "$pi" ]

conflict="$(api POST payment_intents --header "Idempotency-Key: $idem" \
  --data-urlencode amount=1201 --data-urlencode currency=usd)"
jq -e '.error.type == "idempotency_error"' <<< "$conflict" >/dev/null

decline="$(api POST payment_intents \
  --header "Idempotency-Key: merc-matrix-$run_id-decline" \
  --data-urlencode amount=700 --data-urlencode currency=usd \
  --data-urlencode customer="$customer" --data-urlencode payment_method=pm_card_chargeDeclined \
  --data-urlencode 'payment_method_types[]=card' --data-urlencode confirm=true)"
jq -e '.error.decline_code != null or .error.code == "card_declined"' <<< "$decline" >/dev/null

charge="$(jq -er '.latest_charge | select(startswith("ch_"))' <<< "$success")"
partial="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=200 \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e '.status == "succeeded" and .amount == 200' <<< "$partial" >/dev/null
excess="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=1200)"
jq -e '.error != null' <<< "$excess" >/dev/null
remaining="$(api POST refunds --data-urlencode charge="$charge" --data-urlencode amount=1000 \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e '.status == "succeeded" and .amount == 1000' <<< "$remaining" >/dev/null

transfer="$(api POST transfers --data-urlencode amount=100 --data-urlencode currency=usd \
  --data-urlencode destination="$STRIPE_TEST_CONNECTED_ACCOUNT_ID" \
  --data-urlencode "metadata[cx_matrix_run]=$run_id")"
jq -e '.id | type == "string" and startswith("tr_")' <<< "$transfer" >/dev/null
jq -e '.livemode == false' <<< "$transfer" >/dev/null

# Provider-created webhook delivery, dispute lifecycle, and connected-account
# payout outcomes remain mandatory. The adapter is bundled so credentials and
# endpoint identifiers are the only missing inputs; no release-day code is
# supplied through an operator-controlled executable.
driver="$ROOT/scripts/stripe-sandbox-scenarios.sh"
[ -x "$driver" ] || { echo "stripe-sandbox: bundled scenario driver is not executable" >&2; exit 1; }

driver_receipt="$($driver --run-id "$run_id" --payment-intent "$pi" --charge "$charge" \
  --transfer "$(jq -r .id <<< "$transfer")" --connected-account "$STRIPE_TEST_CONNECTED_ACCOUNT_ID")"
jq -e '
  .schema_version == 1 and .status == "PASS" and .provider_mode == "test" and
  .secret_values_recorded == false and .webhook.endpoint_secrets_verified == true and
  .webhook.delivery == true and
  .webhook.replay_idempotent == true and .webhook.out_of_order_safe == true and
  .dispute.opened == true and .dispute.resolved == true and
  .payout.hold == true and .payout.release == true and .payout.failure == true and
  .payout.reversal == true and .reconciliation.clean == true and
  ([.. | strings | select(test("sk_(test|live)_|rk_(test|live)_|whsec_"))] | length) == 0
' <<< "$driver_receipt" >/dev/null

jq -nc --arg run "$run_id" --argjson driver "$driver_receipt" \
  '{schema_version:1,kind:"stripe_sandbox_matrix",status:"PASS",provider_mode:"test",
    run_id:$run,secret_values_printed:false,disposable_customer_cleanup:"attempted",
    payment_objects:{authorization:true,capture:true,decline:true,idempotency:true,refunds:true,transfer:true,
      timeout:{client_deadline:true,idempotent_recovery:true}},
    external_scenarios:$driver,live_mode:"PROHIBITED"}'
