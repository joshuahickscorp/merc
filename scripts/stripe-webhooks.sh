#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
ENV=.env

die()  { echo "ERROR · $*" >&2; exit 1; }
info() { echo "·· $*" >&2; }
hr()   { echo >&2; echo "== $* ==" >&2; }

command -v jq   >/dev/null 2>&1 || die "jq not found (brew install jq)"
command -v curl >/dev/null 2>&1 || die "curl not found"

envval() { [ -f "$ENV" ] && grep -E "^[[:space:]]*$1=" "$ENV" 2>/dev/null | tail -1 | cut -d= -f2- || true; }

set_env() {
  local key="$1" val="$2" tmp
  [ -f "$ENV" ] || { cp .env.example "$ENV" 2>/dev/null || touch "$ENV"; }
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
HOST="${HOST:-computexchange.net}"
HOST="${HOST#http://}"; HOST="${HOST#https://}"; HOST="${HOST%%/*}"   # tolerate a pasted URL
info "target host: https://$HOST"

stripe_api() {
  local method="$1" path="$2"; shift 2
  local resp
  resp="$(curl -fsS -X "$method" "https://api.stripe.com/v1/$path" -u "$SK:" "$@" 2>/dev/null)" \
    || die "Stripe API $method /$path failed (network, or the key lacks permission)"
  if echo "$resp" | jq -e '.error' >/dev/null 2>&1; then
    die "Stripe API error: $(echo "$resp" | jq -r '.error.message')"
  fi
  echo "$resp"
}

find_endpoint_id() {
  local url="$1" connect_scope="$2"
  stripe_api GET "webhook_endpoints?limit=100" \
    | jq -r --arg u "$url" --argjson c "$connect_scope" \
      '.data[] | select(.url==$u and ((.connect // false)==$c)) | .id' | head -1
}

ensure_endpoint() {
  local url="$1" envkey="$2" events="$3" connect_scope="$4"
  hr "$url"
  local existing
  existing="$(find_endpoint_id "$url" "$connect_scope")"
  if [ -n "$existing" ]; then
    local update_args=() update_ev
    IFS=',' read -ra EVS <<< "$events"
    for update_ev in "${EVS[@]}"; do update_args+=(-d "enabled_events[]=$update_ev"); done
    stripe_api POST "webhook_endpoints/$existing" "${update_args[@]}" >/dev/null
    info "endpoint already exists ($existing)  -  refreshed events: $events"
    if [ -n "$(envval "$envkey")" ]; then
      info "$envkey is already set in .env  -  leaving it."
    else
      info "Stripe will NOT re-reveal an existing endpoint's secret via API."
      info "  Get it from: Stripe dashboard -> Developers -> Webhooks -> $url -> Signing secret -> Reveal,"
      info "  then: set $envkey=whsec_… in .env (or delete the endpoint and re-run to mint a fresh one)."
    fi
    return 0
  fi
  local args=(-d "url=$url" -d "connect=$connect_scope")
  local ev; IFS=',' read -ra EVS <<< "$events"
  for ev in "${EVS[@]}"; do args+=(-d "enabled_events[]=$ev"); done
  args+=(-d "description=computexchange $envkey")
  local resp secret id
  resp="$(stripe_api POST "webhook_endpoints" "${args[@]}")"
  id="$(echo "$resp" | jq -r '.id')"
  secret="$(echo "$resp" | jq -r '.secret // empty')"
  [ -n "$secret" ] || die "created endpoint $id but Stripe returned no signing secret (unexpected)  -  check the dashboard"
  info "created $id for events: $events"
  if [ "${WRITE_ENV:-1}" = "1" ]; then
    set_env "$envkey" "$secret"
    info "wrote $envkey -> .env (whsec_…${secret: -4})"
  else
    echo "$envkey=$secret"
  fi
}

ensure_endpoint "https://$HOST/v1/stripe/webhook"         "STRIPE_WEBHOOK_SECRET"     "setup_intent.succeeded,payment_method.attached,payment_intent.succeeded,charge.refunded,charge.dispute.created,charge.dispute.funds_withdrawn,charge.dispute.funds_reinstated,charge.dispute.closed" false
ensure_endpoint "https://$HOST/v1/stripe/connect-webhook" "CX_CONNECT_WEBHOOK_SECRET" "account.updated" true

hr "done"
info "If secrets were written to .env, restart the control plane so it loads them:"
info "  local:  make control     ·     prod:  cx reload   (or docker compose -f docker-compose.prod.yml up -d control)"
