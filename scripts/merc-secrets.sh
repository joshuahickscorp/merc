#!/usr/bin/env bash
# Blind-drop the credentials merc needs, verify each one, and say what it unlocked.
#
# Separate from scripts/merc-credentials.sh on purpose. That script serves the
# LOCAL canary and refuses live Stripe keys outright, which is correct for a
# throwaway loop on a laptop. This one handles the production droplet, where a
# live key is the point -- so it treats a live key as a deliberate, clearly
# labelled choice rather than an accident, and never mixes the two files.
#
#   bash scripts/merc-secrets.sh            # prompt for anything missing
#   bash scripts/merc-secrets.sh --check    # verify what is already stored
#
# Nothing is echoed. Values are read with hidden input, written to a chmod-600
# gitignored file, and passed to curl through --config on stdin so they never
# appear in argv where `ps` would show them to any local user.

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ENV_FILE="$ROOT/.merc-secrets.env"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
skip() { printf '  \033[33m--\033[0m    %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

redact() { local v="$1"; [ -z "$v" ] && { printf 'unset'; return; }
           printf '%s…%s (%d chars)' "${v:0:6}" "${v: -2}" "${#v}"; }

# Checked BEFORE writing. A credentials file that is not ignored is one
# `git add -A` from being committed.
ensure_ignored() {
  grep -qxF '.merc-secrets.env' "$ROOT/.gitignore" 2>/dev/null \
    || printf '\n# Production secrets. Never commit.\n.merc-secrets.env\n' >> "$ROOT/.gitignore"
  ( cd "$ROOT" && git check-ignore -q .merc-secrets.env ) \
    || die ".merc-secrets.env is NOT gitignored; refusing to write a secret here"
}

read_secret() {
  local prompt="$1" var="$2" existing="${3:-}" value=""
  if [ -n "$existing" ]; then printf '%s [keep %s]: ' "$prompt" "$(redact "$existing")"
  else printf '%s (hidden, blank to skip): ' "$prompt"; fi
  if [ -t 0 ]; then read -rs value; printf '\n'; else read -r value || true; fi
  [ -z "$value" ] && value="$existing"
  printf -v "$var" '%s' "$value"
}

# ------------------------------------------------------------------ checks
check_cloudflare() {
  local tok="$1"
  [ -z "$tok" ] && { skip "Cloudflare: no token supplied"; return 1; }
  local body
  body=$(printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sS --config - \
    --max-time 25 https://api.cloudflare.com/client/v4/user/tokens/verify 2>/dev/null) || {
      bad "Cloudflare: could not reach api.cloudflare.com"; return 1; }
  if ! printf '%s' "$body" | grep -q '"success":true'; then
    bad "Cloudflare: token rejected"; return 1
  fi
  ok "Cloudflare: token valid"
  # Zone listing is what the domain work needs; verifying it now beats
  # discovering a missing permission mid-delete.
  local zones
  zones=$(printf 'header = "Authorization: Bearer %s"\n' "$tok" | curl -sS --config - \
    --max-time 25 'https://api.cloudflare.com/client/v4/zones?per_page=50' 2>/dev/null \
    | python3 -c '
import json,sys
d=json.load(sys.stdin)
if not d.get("success"): print("ZONE_LIST_DENIED"); raise SystemExit
for z in d.get("result",[]): print(z["name"], z["id"], z["status"])' 2>/dev/null)
  if [ "$zones" = "ZONE_LIST_DENIED" ] || [ -z "$zones" ]; then
    bad "Cloudflare: token cannot list zones (needs Zone:Read); domain work blocked"
    return 1
  fi
  ok "Cloudflare: $(printf '%s' "$zones" | wc -l | tr -d ' ') zone(s) visible"
  printf '%s' "$zones" | sed 's/^/          /'
  return 0
}

check_stripe() {
  local key="$1"
  [ -z "$key" ] && { skip "Stripe: no key supplied"; return 1; }
  local mode
  case "$key" in
    sk_live_*|rk_live_*) mode=LIVE ;;
    sk_test_*|rk_test_*) mode=test ;;
    *) bad "Stripe: not a recognisable secret key"; return 1 ;;
  esac
  local code
  code=$(printf 'user = "%s:"\n' "$key" | curl -sS --config - -o /tmp/.merc_bal.$$ \
    -w '%{http_code}' --max-time 25 https://api.stripe.com/v1/balance 2>/dev/null) || {
      bad "Stripe: could not reach api.stripe.com"; rm -f /tmp/.merc_bal.$$; return 1; }
  case "$code" in
    200) ;;
    401) bad "Stripe: key rejected (401 - expired or revoked)"; rm -f /tmp/.merc_bal.$$; return 1 ;;
    *)   bad "Stripe: unexpected HTTP $code"; rm -f /tmp/.merc_bal.$$; return 1 ;;
  esac
  local currencies
  currencies=$(python3 -c '
import json,sys
d=json.load(open(sys.argv[1]))
print(",".join(sorted({b["currency"] for b in d.get("available",[])+d.get("pending",[])})))' \
    /tmp/.merc_bal.$$ 2>/dev/null || printf '')
  rm -f /tmp/.merc_bal.$$
  if [ "$mode" = LIVE ]; then
    warn "Stripe: LIVE key accepted. Charges and payouts move REAL money."
  else
    ok "Stripe: test key accepted"
  fi
  ok "Stripe: settlement currencies: ${currencies:-none reported}"
  # merc's settlement currency is configuration now; CAD is what the platform
  # holds, so a missing USD bucket is no longer a blocker.
  case ",$currencies," in
    *,cad,*) ok "Stripe: cad present, matches MERC_SETTLEMENT_CURRENCY=cad" ;;
    *) warn "Stripe: no cad bucket; MERC_SETTLEMENT_CURRENCY must match a currency this account holds" ;;
  esac
  return 0
}

# -------------------------------------------------------------------- main
say "merc production secrets"
say ""

CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}"
STRIPE_SECRET_KEY="${STRIPE_SECRET_KEY:-}"
# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

if [ "$CHECK_ONLY" -eq 0 ]; then
  ensure_ignored
  read_secret "Cloudflare API token"        CLOUDFLARE_API_TOKEN "${CLOUDFLARE_API_TOKEN:-}"
  read_secret "Stripe secret key (live or test)" STRIPE_SECRET_KEY "${STRIPE_SECRET_KEY:-}"

  {
    printf '# merc production secrets. chmod 600, gitignored, never commit.\n'
    [ -n "${CLOUDFLARE_API_TOKEN:-}" ] && printf 'export CLOUDFLARE_API_TOKEN=%q\n' "$CLOUDFLARE_API_TOKEN"
    [ -n "${STRIPE_SECRET_KEY:-}" ]    && printf 'export STRIPE_SECRET_KEY=%q\n' "$STRIPE_SECRET_KEY"
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  say ""
  say "wrote $ENV_FILE (chmod 600, gitignored)"
fi

say ""
say "verifying"
cf_ok=0; stripe_ok=0
check_cloudflare "${CLOUDFLARE_API_TOKEN:-}" && cf_ok=1 || true
check_stripe     "${STRIPE_SECRET_KEY:-}"    && stripe_ok=1 || true

say ""
say "unlocked"
[ "$cf_ok" -eq 1 ] \
  && ok "domain work: list, audit and delete stale zones" \
  || skip "domain work: still blocked"
[ "$stripe_ok" -eq 1 ] \
  && ok "payout rail: droplet can be pointed at this key" \
  || skip "payout rail: still blocked"

say ""
say "next:"
[ "$cf_ok" -eq 1 ] && say "  bash scripts/cloudflare-zones.sh list"
[ "$stripe_ok" -eq 1 ] && say "  bash scripts/merc-secrets.sh --check   # re-verify any time"
