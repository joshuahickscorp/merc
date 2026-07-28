#!/usr/bin/env bash
# The one credential still missing: the Cloudflare Global API Key.
#
# Everything else merc needs is already stored and verified -- Stripe, R2, the
# alert receiver, Grafana, and a scoped Cloudflare token. That token can list
# zones but cannot delete them or see the account, which is what blocks the
# teardown. The Global API Key has no such limits.
#
#   bash scripts/merc-cloudflare-key.sh
#
# The Global API Key is UNSCOPED. It can do anything the account owner can do,
# on every zone and every product, and unlike a token it cannot be narrowed or
# given an expiry. That is why it solves this instantly, and why the right move
# is to roll it in the dashboard once the teardown is finished.
#
# It also needs the account EMAIL: Cloudflare authenticates a global key with
# X-Auth-Email plus X-Auth-Key, not a bearer token. The email is not a secret,
# the key is.

set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ENV_FILE="$ROOT/.merc-secrets.env"

say()  { printf '%s\n' "$*"; }
ok()   { printf '  \033[32mOK\033[0m    %s\n' "$*"; }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; }
warn() { printf '  \033[33mWARN\033[0m  %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

[ -f "$ENV_FILE" ] || die "$ENV_FILE not found; run scripts/merc-secrets.sh first"
( cd "$ROOT" && git check-ignore -q .merc-secrets.env ) \
  || die ".merc-secrets.env is NOT gitignored; refusing to write a secret"

# shellcheck disable=SC1090
. "$ENV_FILE"

say "Cloudflare Global API Key"
say "  dash.cloudflare.com -> My Profile -> API Tokens -> Global API Key -> View"
say ""

printf '  Cloudflare account email: '
read -r CLOUDFLARE_EMAIL
[ -n "$CLOUDFLARE_EMAIL" ] || die "email is required (X-Auth-Email header)"

printf '  Global API Key (hidden): '
if [ -t 0 ]; then read -rs CLOUDFLARE_GLOBAL_API_KEY; printf '\n'; else read -r CLOUDFLARE_GLOBAL_API_KEY; fi
[ -n "$CLOUDFLARE_GLOBAL_API_KEY" ] || die "key is required"

# Verify BEFORE writing, so a mistyped key never lands on disk.
say ""
say "verifying"
resp=$(printf 'header = "X-Auth-Email: %s"\nheader = "X-Auth-Key: %s"\n' \
         "$CLOUDFLARE_EMAIL" "$CLOUDFLARE_GLOBAL_API_KEY" \
       | curl -sS --config - --max-time 30 \
           https://api.cloudflare.com/client/v4/accounts 2>/dev/null) || die "could not reach api.cloudflare.com"

if ! printf '%s' "$resp" | grep -q '"success":true'; then
  bad "rejected: $(printf '%s' "$resp" | head -c 200)"
  die "nothing written"
fi

n=$(printf '%s' "$resp" | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("result") or []))')
if [ "$n" -eq 0 ]; then
  bad "authenticated but sees 0 accounts - this is the same gap the scoped token had"
  die "nothing written"
fi
ok "authenticated, $n account(s) visible"
printf '%s' "$resp" | python3 -c '
import json,sys
for a in json.load(sys.stdin).get("result",[]): print("          ", a["name"], a["id"])'

# Append without disturbing what is already stored.
{
  printf 'export CLOUDFLARE_EMAIL=%q\n' "$CLOUDFLARE_EMAIL"
  printf 'export CLOUDFLARE_GLOBAL_API_KEY=%q\n' "$CLOUDFLARE_GLOBAL_API_KEY"
} >> "$ENV_FILE"
chmod 600 "$ENV_FILE"

say ""
ok "stored in $ENV_FILE"
warn "unscoped credential: roll it in the dashboard once the teardown is done"
say ""
say "next:"
say "  bash scripts/cloudflare-teardown.sh plan"
