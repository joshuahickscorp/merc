#!/usr/bin/env bash
# Interactive Cloudflare API token setup.
#
#   bash scripts/cloudflare-setup.sh
#
# Reads the token blind (never echoed, never in shell history), verifies it
# against Cloudflare before saving, shows exactly which zones and permissions it
# actually got, and writes it to .secrets/cloudflare.env (chmod 600, gitignored).
#
# A token that verifies but cannot see your zones is the common failure, so this
# checks reachability rather than just format.
set -uo pipefail
cd "$(dirname "$0")/.."

SECRETS_DIR=".secrets"
ENV_FILE="$SECRETS_DIR/cloudflare.env"
API="https://api.cloudflare.com/client/v4"
BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; OFF=$'\033[0m'

say()  { printf '%s\n' "$*"; }
head2(){ printf '\n%s%s%s\n' "$BOLD" "$*" "$OFF"; }
dim()  { printf '%s%s%s\n' "$DIM" "$*" "$OFF"; }
ok()   { printf '%s  ✓ %s%s\n' "$GRN" "$*" "$OFF"; }
warn() { printf '%s  ! %s%s\n' "$YEL" "$*" "$OFF"; }
bad()  { printf '%s  ✗ %s%s\n' "$RED" "$*" "$OFF"; }

command -v curl   >/dev/null || { bad "curl not found";   exit 1; }
command -v python3 >/dev/null || { bad "python3 not found"; exit 1; }

cat <<'INTRO'

  Cloudflare API token setup
  ==========================

  This does NOT need the OAuth flow. A scoped API token is enough and is easier
  to revoke later.

  1. Open:  https://dash.cloudflare.com/profile/api-tokens
     ^ this exact URL. The per-account page at
       dash.cloudflare.com/<account-id>/api-tokens offers ONLY account-scoped
       permissions and has no Zone section, so tokens made there cannot touch
       DNS records.
  2. "Create Token"  →  "Create Custom Token"  →  Get started
  3. Name it something you will recognise, e.g.  merc-agent-dns

  Permissions — add these four rows:

      Zone     │ DNS               │ Edit
      Zone     │ Zone Settings     │ Edit
      Zone     │ Zone              │ Read
      Account  │ Domain Registration │ Edit      ← only if you want auto-renew
                                                    or transfer-lock changed

  Zone Resources:     Include → All zones from an account → (your account)
  Account Resources:  Include → (your account)

  Leave Client IP Filtering empty. Set a TTL if you want it to self-expire.

  4. Continue to summary → Create Token → copy it. Cloudflare shows it ONCE.

INTRO
read -r -p "  Press Enter when you have the token on your clipboard… " _ </dev/tty

head2 "Paste the token"
dim "  Input is hidden. Nothing is echoed or logged."
say ""
read -r -s -p "  Token: " TOKEN </dev/tty
say ""
TOKEN="$(printf '%s' "$TOKEN" | tr -d '[:space:]')"
[ -n "$TOKEN" ] || { bad "no token entered — nothing written"; exit 1; }

# Confirm the paste actually landed before spending a round trip on it.
# Deliberately plain: no substring expansion, no non-ASCII inside an expansion.
TOKEN_LEN=$(printf '%s' "$TOKEN" | wc -c | tr -d ' ')
TOKEN_HEAD=$(printf '%s' "$TOKEN" | cut -c1-3)
TOKEN_TAIL=$(printf '%s' "$TOKEN" | rev | cut -c1-3 | rev)
ok "received $TOKEN_LEN characters [$TOKEN_HEAD...$TOKEN_TAIL]"
if [ "$TOKEN_LEN" -lt 20 ]; then
  warn "that is short for a Cloudflare API token (~40 chars) -- likely a truncated paste"
fi

AUTH_MODE="bearer"
AUTH_EMAIL=""

# A Global API Key is 37 hex characters and authenticates with X-Auth-Key +
# X-Auth-Email, NOT a Bearer header. Pasting one into a Bearer flow fails with a
# generic error, which is the single most common way this step goes wrong.
if printf '%s' "$TOKEN" | grep -qE '^[0-9a-f]{37}$'; then
  say ""
  warn "That looks like a Global API Key (37 hex chars), not a scoped API Token."
  say "    Global keys carry FULL account access and cannot be scoped, so a"
  say "    scoped token is both safer and easier to revoke."
  say ""
  read -r -p "  Use it anyway? [y/N] " ans </dev/tty
  case "$ans" in
    y|Y)
      read -r -p "  Cloudflare account email: " AUTH_EMAIL </dev/tty
      AUTH_MODE="globalkey" ;;
    *)
      say ""
      say "  Create a scoped token instead:"
      say "    https://dash.cloudflare.com/profile/api-tokens"
      say "    Create Token → Create Custom Token (NOT 'Global API Key')"
      exit 1 ;;
  esac
fi

cf() {
  if [ "$AUTH_MODE" = "globalkey" ]; then
    curl -s --max-time 25 -H "X-Auth-Email: $AUTH_EMAIL" -H "X-Auth-Key: $TOKEN" "$@"
  else
    curl -s --max-time 25 -H "Authorization: Bearer $TOKEN" "$@"
  fi
}

# ------------------------------------------------------------------- verify it
head2 "Verifying against Cloudflare"
if [ "$AUTH_MODE" = "bearer" ]; then
  verify="$(cf "$API/user/tokens/verify")"
else
  verify="$(cf "$API/user")"
fi

if ! printf '%s' "$verify" | python3 -c 'import json,sys; sys.exit(0 if json.load(sys.stdin).get("success") else 1)' 2>/dev/null; then
  bad "Cloudflare rejected these credentials."
  printf '%s' "$verify" | python3 -c '
import json,sys
try:
    d=json.load(sys.stdin)
    for e in d.get("errors",[]):
        code=e.get("code"); msg=e.get("message","")
        print(f"     [{code}] {msg}")
        if code==6003:
            print("       -> malformed header: usually a truncated paste, or a")
            print("          Global API Key pasted into the scoped-token flow.")
        if code==9109: print("       -> token is valid but not permitted for this resource.")
        if code==1000: print("       -> wrong auth scheme for this credential type.")
except Exception:
    print("     (unparseable response)")
' 2>/dev/null
  say ""
  say "  Checklist:"
  say "    · Created via Create Token → Create Custom Token (not Global API Key)"
  say "    · Copied in full — they are ~40 chars, no spaces"
  say "    · Not already revoked, and TTL not expired"
  say "    · Client IP Filtering left empty"
  exit 1
fi
ok "credentials accepted"

# --------------------------------------------------------------- what it sees
head2 "What this token can actually see"
zones="$(cf "$API/zones?per_page=50")"
printf '%s' "$zones" | python3 -c '
import json,sys
d=json.load(sys.stdin)
rows=d.get("result") or []
if not rows:
    print("  \033[33m  ! no zones visible.\033[0m")
    print("    The usual cause is creating the token on the ACCOUNT tokens page")
    print("    (dash.cloudflare.com/<account-id>/api-tokens). That page only offers")
    print("    Account-scoped permissions -- it has no Zone section at all, so a")
    print("    token made there can never read or edit DNS records.")
    print("")
    print("    Zone permissions live on the USER tokens page:")
    print("      https://dash.cloudflare.com/profile/api-tokens")
    print("    Create Custom Token there and add Zone > Zone > Read and")
    print("    Zone > DNS > Edit.")
    sys.exit(0)
for z in rows:
    print(f"  \033[32m  ✓\033[0m {z[\"name\"]:<28} {z.get(\"status\",\"?\"):<10} id={z[\"id\"][:12]}…")
wanted={"computexchange.net","tailorapp.ai"}
seen={z["name"] for z in rows}
for w in sorted(wanted-seen):
    print(f"  \033[33m  ! {w} not visible to this token\033[0m")
'

# ------------------------------------------------------------------- write it
head2 "Saving"
mkdir -p "$SECRETS_DIR"; chmod 700 "$SECRETS_DIR"
if [ -f "$ENV_FILE" ]; then
  cp "$ENV_FILE" "$ENV_FILE.bak.$(date -u +%Y%m%dT%H%M%SZ)"
  chmod 600 "$ENV_FILE".bak.* 2>/dev/null || true
  dim "  previous token backed up"
fi
umask 077
if [ "$AUTH_MODE" = "globalkey" ]; then
  { printf 'CLOUDFLARE_API_KEY=%s\n' "$TOKEN"
    printf 'CLOUDFLARE_EMAIL=%s\n' "$AUTH_EMAIL"; } > "$ENV_FILE"
else
  printf 'CLOUDFLARE_API_TOKEN=%s\n' "$TOKEN" > "$ENV_FILE"
fi
chmod 600 "$ENV_FILE"
ok "$ENV_FILE  (chmod 600, .secrets/ is gitignored)"

head2 "Next"
cat <<'NEXT'
  Tell me it is done and I will take it from there.

  What I can do with this token:
    · list and audit DNS records on both zones
    · re-point or proxy computexchange.net
    · report registration state and expiry

  What I will still confirm with you first, because it is hard to undo:
    · changing DNS on a domain that is currently serving traffic
      (computexchange.net answers 200 from your droplet right now)
    · disabling auto-renew or unlocking a domain for transfer

  Revoke any time at https://dash.cloudflare.com/profile/api-tokens
NEXT
