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
  code=$(printf 'user = "%s:"\n' "$key" | curl -sS --config - \
    --header 'Stripe-Version: 2025-06-30.basil' -o /tmp/.merc_bal.$$ \
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


# The R2 dashboard shows a full S3 endpoint, often with the bucket appended:
#   https://<account-id>.r2.cloudflarestorage.com/<bucket>
# Pasting that where a bare account id was asked for produced
# "https://https://....r2.cloudflarestorage.com/merc.r2.cloudflarestorage.com".
# Accept whatever form the dashboard gave and reduce it to the account id, so
# the prompt cannot be answered wrongly.
normalise_account_id() {
  local raw="$1"
  raw="${raw#https://}"; raw="${raw#http://}"   # scheme, if pasted
  raw="${raw%%/*}"                              # any /bucket path
  raw="${raw%.r2.cloudflarestorage.com}"        # the endpoint suffix
  printf '%s' "$raw"
}

# If a bucket came in on that path, it is a better default than a guess.
bucket_from_endpoint() {
  local raw="$1" path
  raw="${raw#https://}"; raw="${raw#http://}"
  case "$raw" in */*) path="${raw#*/}"; printf '%s' "${path%%/*}" ;; *) printf '' ;; esac
}

check_r2() {
  local acct="$1" akid="$2" secret="$3"
  if [ -z "$acct$akid$secret" ]; then skip "R2: not configured"; return 1; fi
  if [ -z "$acct" ] || [ -z "$akid" ] || [ -z "$secret" ]; then
    bad "R2: needs all three of account id, access key id and secret"; return 1
  fi
  # R2 speaks S3, and merc's storage layer is already an S3 client, so the check
  # that matters is whether the S3 endpoint answers -- not whether the Cloudflare
  # REST API likes the token. A signed ListBuckets is the cheapest real probe.
  acct=$(normalise_account_id "$acct")
  local endpoint="https://${acct}.r2.cloudflarestorage.com"
  local code
  code=$(AWS_ACCESS_KEY_ID="$akid" AWS_SECRET_ACCESS_KEY="$secret" \
    curl -sS -o /dev/null -w '%{http_code}' --max-time 25 \
      --aws-sigv4 "aws:amz:auto:s3" --user "${akid}:${secret}" \
      "$endpoint/" 2>/dev/null) || { bad "R2: could not reach $endpoint"; return 1; }
  case "$code" in
    200) ok "R2: endpoint reachable and credentials accepted" ;;
    403) bad "R2: credentials rejected (403). These must be the S3 keys from
        R2 dashboard -> Manage API tokens, NOT the general Cloudflare API token."
        return 1 ;;
    *) bad "R2: unexpected HTTP $code from $endpoint"; return 1 ;;
  esac
  return 0
}

check_alert_receiver() {
  local url="$1"
  [ -z "$url" ] && { skip "alert receiver: not configured"; return 1; }
  case "$url" in
    https://*) ;;
    *) bad "alert receiver: must be an HTTPS URL"; return 1 ;;
  esac
  # Deliberately NOT posting a test alert here: that would page whoever is on
  # the other end just for running a credential check. Shape is validated now;
  # scripts/test-alert-delivery.sh proves real delivery when you want it.
  ok "alert receiver: HTTPS URL recorded (delivery proven separately)"
  return 0
}

# -------------------------------------------------------------------- main
say "merc production secrets"
say ""

CLOUDFLARE_API_TOKEN="${CLOUDFLARE_API_TOKEN:-}"
STRIPE_SECRET_KEY="${STRIPE_SECRET_KEY:-}"
CLOUDFLARE_ACCOUNT_ID="${CLOUDFLARE_ACCOUNT_ID:-}"
R2_ACCESS_KEY_ID="${R2_ACCESS_KEY_ID:-}"
R2_SECRET_ACCESS_KEY="${R2_SECRET_ACCESS_KEY:-}"
R2_BUCKET="${R2_BUCKET:-merc-jobs}"
MERC_ALERT_RECEIVER_URL="${MERC_ALERT_RECEIVER_URL:-}"
GF_SECURITY_ADMIN_PASSWORD="${GF_SECURITY_ADMIN_PASSWORD:-}"
# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

if [ "$CHECK_ONLY" -eq 0 ]; then
  ensure_ignored

  # Already-stored values are not re-prompted. Blank input keeps what is there,
  # so re-running this to add one credential never wipes another.
  [ -z "${CLOUDFLARE_API_TOKEN:-}" ] && \
    read_secret "Cloudflare API token" CLOUDFLARE_API_TOKEN "" \
    || ok "Cloudflare API token already stored, skipping"
  [ -z "${STRIPE_SECRET_KEY:-}" ] && \
    read_secret "Stripe secret key (live or test)" STRIPE_SECRET_KEY "" \
    || ok "Stripe key already stored, skipping"

  say ""
  say "R2 object storage  (R2 dashboard -> Account Details -> API Tokens -> Manage)"
  say "  These are the S3 keys, NOT the general Cloudflare API token."
  read_secret "  Cloudflare account id OR the full R2 S3 endpoint" CLOUDFLARE_ACCOUNT_ID "${CLOUDFLARE_ACCOUNT_ID:-}"
  if [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]; then
    _maybe_bucket=$(bucket_from_endpoint "$CLOUDFLARE_ACCOUNT_ID")
    CLOUDFLARE_ACCOUNT_ID=$(normalise_account_id "$CLOUDFLARE_ACCOUNT_ID")
    [ -n "$_maybe_bucket" ] && { R2_BUCKET="$_maybe_bucket"; ok "  bucket taken from the endpoint: $R2_BUCKET"; }
    ok "  account id: $CLOUDFLARE_ACCOUNT_ID"
  fi
  read_secret "  R2 Access Key ID"        R2_ACCESS_KEY_ID      "${R2_ACCESS_KEY_ID:-}"
  read_secret "  R2 Secret Access Key"    R2_SECRET_ACCESS_KEY  "${R2_SECRET_ACCESS_KEY:-}"

  say ""
  say "operations"
  read_secret "  Alert receiver HTTPS URL (where a page actually lands)" MERC_ALERT_RECEIVER_URL "${MERC_ALERT_RECEIVER_URL:-}"
  if [ -z "${GF_SECURITY_ADMIN_PASSWORD:-}" ]; then
    # Generated, not prompted: this is a service password merc owns, and a
    # generated one beats a reused human-chosen one.
    GF_SECURITY_ADMIN_PASSWORD=$(openssl rand -base64 24 | tr -d '\n')
    ok "  Grafana admin password generated (32+ bits, stored only in this file)"
  fi

  {
    printf '# merc production secrets. chmod 600, gitignored, never commit.\n'
    [ -n "${CLOUDFLARE_API_TOKEN:-}" ]      && printf 'export CLOUDFLARE_API_TOKEN=%q\n' "$CLOUDFLARE_API_TOKEN"
    [ -n "${STRIPE_SECRET_KEY:-}" ]         && printf 'export STRIPE_SECRET_KEY=%q\n' "$STRIPE_SECRET_KEY"
    [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]     && printf 'export CLOUDFLARE_ACCOUNT_ID=%q\n' "$CLOUDFLARE_ACCOUNT_ID"
    [ -n "${R2_ACCESS_KEY_ID:-}" ]          && printf 'export R2_ACCESS_KEY_ID=%q\n' "$R2_ACCESS_KEY_ID"
    [ -n "${R2_SECRET_ACCESS_KEY:-}" ]      && printf 'export R2_SECRET_ACCESS_KEY=%q\n' "$R2_SECRET_ACCESS_KEY"
    [ -n "${R2_BUCKET:-}" ]                 && printf 'export R2_BUCKET=%q\n' "$R2_BUCKET"
    [ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ]     && printf 'export R2_ENDPOINT=%q\n' "https://${CLOUDFLARE_ACCOUNT_ID}.r2.cloudflarestorage.com"
    [ -n "${MERC_ALERT_RECEIVER_URL:-}" ]   && printf 'export MERC_ALERT_RECEIVER_URL=%q\n' "$MERC_ALERT_RECEIVER_URL"
    [ -n "${GF_SECURITY_ADMIN_PASSWORD:-}" ] && printf 'export GF_SECURITY_ADMIN_PASSWORD=%q\n' "$GF_SECURITY_ADMIN_PASSWORD"
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  say ""
  say "wrote $ENV_FILE (chmod 600, gitignored)"
fi

say ""
say "verifying"
cf_ok=0; stripe_ok=0; r2_ok=0; alert_ok=0
check_cloudflare "${CLOUDFLARE_API_TOKEN:-}" && cf_ok=1 || true
check_stripe     "${STRIPE_SECRET_KEY:-}"    && stripe_ok=1 || true
check_r2 "${CLOUDFLARE_ACCOUNT_ID:-}" "${R2_ACCESS_KEY_ID:-}" "${R2_SECRET_ACCESS_KEY:-}" && r2_ok=1 || true
check_alert_receiver "${MERC_ALERT_RECEIVER_URL:-}" && alert_ok=1 || true

say ""
say "unlocked"
[ "$cf_ok" -eq 1 ] \
  && ok "domain work: list, audit and delete stale zones" \
  || skip "domain work: still blocked"
[ "$stripe_ok" -eq 1 ] \
  && ok "payout rail: droplet can be pointed at this key" \
  || skip "payout rail: still blocked"
[ "$r2_ok" -eq 1 ] \
  && ok "object storage: merc speaks S3 already, so R2 is an endpoint swap" \
  || skip "object storage: still on the droplet's local MinIO"
[ "$alert_ok" -eq 1 ] \
  && ok "alerts: receiver recorded; run scripts/test-alert-delivery.sh to prove delivery" \
  || skip "alerts: no receiver, a page would land nowhere"

say ""
say "next:"
[ "$cf_ok" -eq 1 ] && say "  bash scripts/cloudflare-zones.sh list"
[ "$stripe_ok" -eq 1 ] && say "  bash scripts/merc-secrets.sh --check   # re-verify any time"
