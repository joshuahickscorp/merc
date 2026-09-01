#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1
ENV=.env
STRIPE_API_VERSION=2025-06-30.basil

die()  { echo "ERROR · $*" >&2; exit 1; }
info() { echo "·· $*" >&2; }
hr()   { echo >&2; echo "== $* ==" >&2; }

command -v jq   >/dev/null 2>&1 || die "jq not found (brew install jq)"
command -v curl >/dev/null 2>&1 || die "curl not found"

endpoint_payload_version_matches() {
  local endpoint_json="$1"
  [ "$(printf '%s' "$endpoint_json" | jq -r '.api_version // empty')" = "$STRIPE_API_VERSION" ]
}

select_endpoint_from_inventory() {
  local inventory_json="$1" url="$2" matches count
  if ! printf '%s' "$inventory_json" \
    | jq -e 'type == "object" and (.data | type == "array")' >/dev/null 2>&1; then
    echo "Stripe returned an invalid webhook endpoint inventory" >&2
    return 2
  fi
  if [ "$(printf '%s' "$inventory_json" | jq -r '.has_more // false')" = "true" ]; then
    echo "Stripe webhook endpoint inventory exceeds the guarded 100-endpoint scan; refusing an ambiguous create" >&2
    return 3
  fi
  matches="$(printf '%s' "$inventory_json" \
    | jq -c --arg u "$url" '[.data[] | select(.url == $u)]')"
  count="$(printf '%s' "$matches" | jq 'length')"
  if [ "$count" -gt 1 ]; then
    echo "Stripe has $count webhook endpoints with exact URL $url; resolve duplicates before activation" >&2
    return 4
  fi
  printf '%s' "$matches" | jq -c '.[0] // empty'
}

if [ "${MERC_STRIPE_WEBHOOK_VERSION_SELF_TEST:-0}" = "1" ]; then
  inventory='{"object":"list","has_more":false,"data":[
    {"id":"we_bill","url":"https://merc.invalid/v1/stripe/webhook","api_version":"2025-06-30.basil"},
    {"id":"we_connect","url":"https://merc.invalid/v1/stripe/connect-webhook","api_version":"2025-06-30.basil"}
  ]}'
  selected="$(select_endpoint_from_inventory "$inventory" \
    "https://merc.invalid/v1/stripe/connect-webhook")" \
    || die "self-test could not select an exact endpoint URL"
  [ "$(printf '%s' "$selected" | jq -r '.id')" = "we_connect" ] \
    || die "self-test confused billing and Connect endpoint URLs"
  duplicate_inventory='{"has_more":false,"data":[
    {"id":"we_one","url":"https://merc.invalid/v1/stripe/webhook"},
    {"id":"we_two","url":"https://merc.invalid/v1/stripe/webhook"}
  ]}'
  if select_endpoint_from_inventory "$duplicate_inventory" \
    "https://merc.invalid/v1/stripe/webhook" >/dev/null 2>&1; then
    die "self-test accepted duplicate exact-URL webhook endpoints"
  fi
  if select_endpoint_from_inventory \
    '{"has_more":true,"data":[]}' "https://merc.invalid/v1/stripe/webhook" \
    >/dev/null 2>&1; then
    die "self-test accepted a truncated webhook endpoint inventory"
  fi
  endpoint_payload_version_matches \
    '{"id":"we_exact","api_version":"2025-06-30.basil"}' \
    || die "self-test rejected the compiled webhook payload version"
  if endpoint_payload_version_matches '{"id":"we_default","api_version":null}'; then
    die "self-test accepted an account-default webhook payload version"
  fi
  if endpoint_payload_version_matches \
    '{"id":"we_drift","api_version":"2026-02-25.clover"}'; then
    die "self-test accepted a different webhook payload version"
  fi
  info "stripe-webhooks version self-test: PASS"
  exit 0
fi

envval() { [ -f "$ENV" ] && grep -E "^[[:space:]]*$1=" "$ENV" 2>/dev/null | tail -1 | cut -d= -f2- || true; }

set_env() {
  local key="$1" val="$2" tmp
  [ -f "$ENV" ] || { cp ops/configs/env.example "$ENV" 2>/dev/null || touch "$ENV"; }
  chmod 600 "$ENV"
  tmp="$(mktemp)"
  grep -vE "^[[:space:]]*#?[[:space:]]*${key}=" "$ENV" > "$tmp" || true
  printf '%s=%s\n' "$key" "$val" >> "$tmp"
  mv "$tmp" "$ENV"; chmod 600 "$ENV"
}

SK="${STRIPE_SECRET_KEY:-$(envval STRIPE_SECRET_KEY)}"
[ -n "$SK" ] || die "STRIPE_SECRET_KEY is not set in the environment or .env"
case "$SK" in
  sk_live_*) info "using a LIVE key  -  endpoints will be created on your real Stripe account" ;;
  sk_test_*) info "using a TEST key  -  endpoints created against Stripe test data (safe rehearsal)" ;;
  rk_*)      die "that is a RESTRICTED key (rk_…). Webhook management needs the standard secret key (sk_…)." ;;
  *)         die "STRIPE_SECRET_KEY does not look like an sk_ key" ;;
esac

HOST="${HOST:-$(envval SITE_HOST)}"
HOST="${HOST:-mercmerc.net}"
HOST="${HOST#http://}"; HOST="${HOST#https://}"; HOST="${HOST%%/*}"   # tolerate a pasted URL
info "target host: https://$HOST"

stripe_api() {
  local method="$1" path="$2"; shift 2
  local resp
  if ! resp="$(curl -fsS -X "$method" \
    -H "Stripe-Version: $STRIPE_API_VERSION" \
    "https://api.stripe.com/v1/$path" -u "$SK:" "$@" 2>/dev/null)"; then # gitleaks:allow -- credential comes only from the environment
    die "Stripe API $method /$path failed (network, or the key lacks permission)"
  fi
  if echo "$resp" | jq -e '.error' >/dev/null 2>&1; then
    die "Stripe API error: $(echo "$resp" | jq -r '.error.message')"
  fi
  echo "$resp"
}

find_endpoint_json() {
  local url="$1" inventory
  inventory="$(stripe_api GET "webhook_endpoints?limit=100")" || return
  select_endpoint_from_inventory "$inventory" "$url"
}

ensure_endpoint() {
  local url="$1" envkey="$2" events="$3" connect_scope="$4"
  hr "$url"
  local existing_json existing existing_version
  existing_json="$(find_endpoint_json "$url")" \
    || die "could not prove a unique existing webhook endpoint for $url"
  if [ -n "$existing_json" ]; then
    existing="$(printf '%s' "$existing_json" | jq -r '.id // empty')"
    existing_version="$(printf '%s' "$existing_json" | jq -r '.api_version // empty')"
    [ -n "$existing" ] || die "matching webhook endpoint has no id"
    endpoint_payload_version_matches "$existing_json" || die \
      "endpoint $existing renders webhook payloads with '${existing_version:-account default}', not $STRIPE_API_VERSION. Stripe cannot change an existing endpoint's api_version: create a replacement under the supervised activation procedure, rotate its signing secret, verify delivery, then disable the old endpoint."
    local update_args=() update_ev updated
    IFS=',' read -ra EVS <<< "$events"
    for update_ev in "${EVS[@]}"; do update_args+=(-d "enabled_events[]=$update_ev"); done
    updated="$(stripe_api POST "webhook_endpoints/$existing" "${update_args[@]}")"
    endpoint_payload_version_matches "$updated" \
      || die "Stripe update response for $existing lost the pinned webhook payload version"
    info "endpoint already exists ($existing)  -  payload version $STRIPE_API_VERSION; refreshed events: $events"
    if [ -n "$(envval "$envkey")" ]; then
      info "$envkey is already set in .env  -  leaving it."
    else
      info "Stripe will NOT re-reveal an existing endpoint's secret via API."
      info "  Get it from: Stripe dashboard -> Developers -> Webhooks -> $url -> Signing secret -> Reveal,"
      info "  then: set $envkey=whsec_… in .env (or delete the endpoint and re-run to mint a fresh one)."
    fi
    return 0
  fi
  local args=(-d "url=$url" -d "connect=$connect_scope" -d "api_version=$STRIPE_API_VERSION")
  local ev; IFS=',' read -ra EVS <<< "$events"
  for ev in "${EVS[@]}"; do args+=(-d "enabled_events[]=$ev"); done
  args+=(-d "description=computexchange $envkey")
  local resp secret id
  resp="$(stripe_api POST "webhook_endpoints" "${args[@]}")"
  id="$(echo "$resp" | jq -r '.id')"
  secret="$(echo "$resp" | jq -r '.secret // empty')"
  endpoint_payload_version_matches "$resp" \
    || die "created endpoint $id without pinned webhook payload version $STRIPE_API_VERSION"
  [ -n "$secret" ] || die "created endpoint $id but Stripe returned no signing secret (unexpected)  -  check the dashboard"
  info "created $id for events: $events (payload version $STRIPE_API_VERSION)"
  if [ "${WRITE_ENV:-1}" = "1" ]; then
    set_env "$envkey" "$secret"
    info "wrote $envkey -> .env (whsec_…${secret: -4})"
  else
    echo "$envkey=$secret"
  fi
}

ensure_endpoint "https://$HOST/v1/stripe/webhook"         "STRIPE_WEBHOOK_SECRET"     "setup_intent.succeeded,payment_method.attached,payment_intent.succeeded,payment_intent.payment_failed,charge.refunded,charge.dispute.created,charge.dispute.funds_withdrawn,charge.dispute.funds_reinstated,charge.dispute.closed,radar.early_fraud_warning.created,radar.early_fraud_warning.updated" false
ensure_endpoint "https://$HOST/v1/stripe/connect-webhook" "MERC_CONNECT_WEBHOOK_SECRET" "account.updated,capability.updated,payout.created,payout.updated,payout.paid,payout.failed,payout.canceled,payout.reconciliation_completed" true

hr "done"
info "If secrets were written to .env, restart the control plane so it loads them:"
info "  local:  make control     ·     prod:  cx reload   (or docker compose -f ops/deploy/docker-compose.prod.yml up -d control)"
