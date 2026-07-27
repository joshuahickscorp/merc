#!/usr/bin/env bash
# Blind-drop credentials for the merc private canary.
#
# Paste a RunPod key and a Stripe TEST key. This writes them to a chmod-600,
# gitignored file, checks each one actually works, and tells you which canary
# lanes it unblocked. It never prints a secret value, and it refuses a live
# Stripe key outright -- this path exists to unblock a local canary, and a key
# that can move real money has no business in it.
#
# Usage, any of:
#   bash scripts/merc-credentials.sh                    # prompts, input hidden
#   RUNPOD_API_KEY=... STRIPE_SECRET_KEY=... bash scripts/merc-credentials.sh
#   bash scripts/merc-credentials.sh --check            # re-verify what is stored
#
# Then:
#   source .merc-credentials.env && make private-canary

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ENV_FILE="$ROOT/.merc-credentials.env"
CHECK_ONLY=0
[ "${1:-}" = "--check" ] && CHECK_ONLY=1

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }
skip() { printf '  \033[33m--\033[0m    %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# A secret must never reach the terminal, a log, or the process table. Values
# are read into variables and written to a 600 file; nothing echoes them, and
# nothing passes them as a command-line argument where `ps` would show them.
redact() { local v="$1"; [ -z "$v" ] && { printf 'unset'; return; }
           printf '%s…%s (%d chars)' "${v:0:7}" "${v: -2}" "${#v}"; }

# ---------------------------------------------------------------- gitignore
# Checked BEFORE anything is written. A credentials file that is not ignored is
# a credentials file that gets committed by the next `git add -A`, which is
# exactly how a Stripe test key reached this repo's history once already.
ensure_ignored() {
  if ! grep -qxF '.merc-credentials.env' "$ROOT/.gitignore" 2>/dev/null; then
    printf '\n# Local canary credentials. Never commit.\n.merc-credentials.env\n' \
      >> "$ROOT/.gitignore"
    say "added .merc-credentials.env to .gitignore"
  fi
  ( cd "$ROOT" && git check-ignore -q .merc-credentials.env ) \
    || die ".merc-credentials.env is NOT gitignored; refusing to write a secret here"
}

# ------------------------------------------------------------------- input
read_secret() {
  local prompt="$1" varname="$2" existing="${3:-}"
  local value=""
  if [ -n "$existing" ]; then
    printf '%s [keep existing %s]: ' "$prompt" "$(redact "$existing")"
  else
    printf '%s (input hidden, blank to skip): ' "$prompt"
  fi
  if [ -t 0 ]; then read -rs value; printf '\n'; else read -r value || true; fi
  [ -z "$value" ] && value="$existing"
  printf -v "$varname" '%s' "$value"
}

# ------------------------------------------------------------------ checks
check_runpod() {
  local key="$1"
  [ -z "$key" ] && { skip "RunPod: no key supplied"; return 1; }
  # RunPod's GraphQL endpoint. Authorization goes in a header, never the URL:
  # a key in a query string lands in every proxy and access log in between.
  # The key goes in via --config on stdin, never as an argument. A header on the
  # command line is visible in `ps auxww` to every local user for the life of the
  # request -- verified empirically, not assumed.
  local code
  code=$(printf 'header = "Authorization: Bearer %s"\n' "$key" | curl -sS \
    --config - -o /dev/null -w '%{http_code}' --max-time 20 \
    -X POST https://api.runpod.io/graphql \
    -H 'content-type: application/json' \
    -d '{"query":"query { myself { id } }"}' 2>/dev/null) || {
      bad "RunPod: could not reach api.runpod.io"; return 1; }
  case "$code" in
    200) ok "RunPod: key accepted by api.runpod.io"; return 0 ;;
    401|403) bad "RunPod: key rejected (HTTP $code)"; return 1 ;;
    *) bad "RunPod: unexpected HTTP $code"; return 1 ;;
  esac
}

check_stripe() {
  local key="$1"
  [ -z "$key" ] && { skip "Stripe: no key supplied"; return 1; }
  case "$key" in
    sk_live_*|rk_live_*)
      die "that is a LIVE Stripe key. This script only accepts test-mode keys:
     it wires a local canary that creates charges and payouts, and a live key
     would move real money. Get the test key from the Stripe dashboard with
     'Test mode' enabled (it starts sk_test_)." ;;
    sk_test_*|rk_test_*) ;;
    *) bad "Stripe: key does not look like a Stripe secret key"; return 1 ;;
  esac
  # -u "$key:" would put the secret in argv. --config on stdin keeps it out.
  local code
  code=$(printf 'user = "%s:"\n' "$key" | curl -sS --config - \
    -o /dev/null -w '%{http_code}' --max-time 20 \
    https://api.stripe.com/v1/balance 2>/dev/null) || {
      bad "Stripe: could not reach api.stripe.com"; return 1; }
  case "$code" in
    200) ;;
    401) bad "Stripe: key rejected (HTTP 401)"; return 1 ;;
    *) bad "Stripe: unexpected HTTP $code"; return 1 ;;
  esac
  # merc settles in USD. A test account with no USD bucket fails at the first
  # payout with balance_insufficient, and that is worth knowing now rather than
  # in the middle of a canary.
  local body currencies
  body=$(printf 'user = "%s:"\n' "$key" | curl -sS --config - --max-time 20 \
    https://api.stripe.com/v1/balance 2>/dev/null)
  currencies=$(printf '%s' "$body" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print(",".join(sorted({b["currency"] for b in d.get("available",[])+d.get("pending",[])})))' 2>/dev/null || printf '')
  ok "Stripe: test key accepted; settlement currencies: ${currencies:-none reported}"
  case ",$currencies," in
    *,usd,*) ok "Stripe: usd is enabled"; return 0 ;;
    *) bad "Stripe: NO usd bucket. merc settles in USD, so payouts will fail with
        balance_insufficient. Enable USD on the test account before the payout lane
        can pass."
       # Returns FAILURE. Previously this printed the warning and then returned
       # 0, so the caller set stripe_ok=1 and the summary announced the payout
       # lane unblocked when the very thing that lane does would fail.
       return 1 ;;
  esac
}

# -------------------------------------------------------------------- main
say "merc canary credentials"
say ""

RUNPOD_API_KEY="${RUNPOD_API_KEY:-}"
STRIPE_SECRET_KEY="${STRIPE_SECRET_KEY:-}"
MERC_GPU_ENDPOINT="${MERC_GPU_ENDPOINT:-}"

# shellcheck disable=SC1090
[ -f "$ENV_FILE" ] && . "$ENV_FILE"

if [ "$CHECK_ONLY" -eq 0 ]; then
  ensure_ignored
  if [ -z "${RUNPOD_API_KEY:-}" ] || [ -t 0 ]; then
    read_secret "RunPod API key" RUNPOD_API_KEY "${RUNPOD_API_KEY:-}"
  fi
  if [ -z "${STRIPE_SECRET_KEY:-}" ] || [ -t 0 ]; then
    read_secret "Stripe TEST secret key (sk_test_...)" STRIPE_SECRET_KEY "${STRIPE_SECRET_KEY:-}"
  fi
  if [ -t 0 ]; then
    _existing_endpoint="${MERC_GPU_ENDPOINT:-}"
    if [ -n "$_existing_endpoint" ]; then
      printf 'vLLM endpoint [keep %s]: ' "$_existing_endpoint"
    else
      printf 'vLLM endpoint URL, if a pod is already serving (blank to skip): '
    fi
    read -r MERC_GPU_ENDPOINT || true
    # Blank keeps what is stored. Without this, re-running the script to update
    # only the Stripe key silently erased a working endpoint.
    [ -z "$MERC_GPU_ENDPOINT" ] && MERC_GPU_ENDPOINT="$_existing_endpoint"
  fi

  # Validate the SHAPE before anything touches disk. The first version of this
  # script wrote the file and THEN verified, so a live Stripe key was persisted
  # before being refused: the refusal fired, but the secret was already on disk.
  # Refuse first, write second.
  case "${STRIPE_SECRET_KEY:-}" in
    sk_live_*|rk_live_*)
      die "that is a LIVE Stripe key. This script only accepts test-mode keys:
     it wires a local canary that creates charges and payouts, and a live key
     would move real money. Nothing was written to disk. Get the test key from
     the Stripe dashboard with 'Test mode' enabled (it starts sk_test_)." ;;
  esac

  {
    printf '# merc canary credentials. chmod 600, gitignored, never commit.\n'
    printf '# Written by scripts/merc-credentials.sh\n'
    [ -n "${RUNPOD_API_KEY:-}" ]     && printf 'export RUNPOD_API_KEY=%q\n' "$RUNPOD_API_KEY"
    [ -n "${STRIPE_SECRET_KEY:-}" ]  && printf 'export STRIPE_SECRET_KEY=%q\n' "$STRIPE_SECRET_KEY"
    [ -n "${MERC_GPU_ENDPOINT:-}" ]  && printf 'export MERC_GPU_ENDPOINT=%q\n' "$MERC_GPU_ENDPOINT"
    printf 'export MERC_GPU_API_KEY="${RUNPOD_API_KEY:-}"\n'
  } > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  say ""
  say "wrote $ENV_FILE (chmod 600, gitignored)"
fi

say ""
say "verifying"
runpod_ok=0; stripe_ok=0
check_runpod "${RUNPOD_API_KEY:-}"    && runpod_ok=1 || true
check_stripe "${STRIPE_SECRET_KEY:-}" && stripe_ok=1 || true

say ""
say "canary lanes"
pod_serving=0
if [ -n "${MERC_GPU_ENDPOINT:-}" ]; then
  # "a variable is set" is not "a pod is serving". The canary checks the endpoint
  # answers /v1/models with this key; claiming the lane unblocked without doing
  # the same check tells the operator something this script never verified.
  if printf 'header = "Authorization: Bearer %s"\n' "${RUNPOD_API_KEY:-}" | curl -sS \
       --config - -o /dev/null --max-time 15 -f \
       "${MERC_GPU_ENDPOINT%/}/models" 2>/dev/null; then
    pod_serving=1
  fi
fi
if [ "$runpod_ok" -eq 1 ] && [ "$pod_serving" -eq 1 ]; then
  ok "runpod_vllm, image_generation, lora, multi_gpu: pod is serving at $MERC_GPU_ENDPOINT"
elif [ "$runpod_ok" -eq 1 ] && [ -n "${MERC_GPU_ENDPOINT:-}" ]; then
  bad "MERC_GPU_ENDPOINT is set but did not answer /models with this key.
        Those lanes stay blocked until it does."
elif [ "$runpod_ok" -eq 1 ]; then
  skip "runpod_vllm, image_generation, lora, multi_gpu: key works, but no pod is
        serving yet. Start one and re-run with MERC_GPU_ENDPOINT=https://<pod>/v1"
else
  skip "runpod_vllm, image_generation, lora, multi_gpu: still blocked"
fi
[ "$stripe_ok" -eq 1 ] && ok "payouts: unblocked" || skip "payouts: still blocked"

say ""
say "next:"
say "  source .merc-credentials.env && make private-canary"
