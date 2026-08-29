#!/usr/bin/env bash
# Interactive Stripe test-mode setup.
#
# Walks you through the Stripe Dashboard one value at a time, tells you exactly
# where to click, and writes the result into .env. Secrets are read blind: they
# are never echoed to the terminal, never printed back, and never written to
# your shell history.
#
#   bash ops/scripts/stripe-setup.sh
#
# Safe to re-run. Existing values are kept unless you choose to replace them,
# and .env is backed up before every write.
set -uo pipefail
cd "$(dirname "$0")/../.."

ENV_FILE=".env"
BOLD=$'\033[1m'; DIM=$'\033[2m'; RED=$'\033[31m'; GRN=$'\033[32m'; YEL=$'\033[33m'; OFF=$'\033[0m'

say()  { printf '%s\n' "$*"; }
head2() { printf '\n%s%s%s\n' "$BOLD" "$*" "$OFF"; }
dim()  { printf '%s%s%s\n' "$DIM" "$*" "$OFF"; }
ok()   { printf '%s  ✓ %s%s\n' "$GRN" "$*" "$OFF"; }
warn() { printf '%s  ! %s%s\n' "$YEL" "$*" "$OFF"; }
die()  { printf '%s  ✗ %s%s\n' "$RED" "$*" "$OFF" >&2; exit 1; }

# ---------------------------------------------------------------- current value
current() {
  [ -f "$ENV_FILE" ] || return 0
  sed -n "s/^$1=//p" "$ENV_FILE" | tail -1
}

# Redacted echo: enough to recognise a value, never enough to leak it.
mask() {
  local v="$1"
  [ -z "$v" ] && { printf '(unset)'; return; }
  if [ "${#v}" -le 12 ]; then printf '%s' "${v:0:4}…"; else printf '%s…%s' "${v:0:8}" "${v: -4}"; fi
}

# ------------------------------------------------------------------- validation
validate() {
  local name="$1" value="$2"
  case "$value" in
    sk_live_*|rk_live_*|pk_live_*)
      die "$name looks like a LIVE key. This script is test-mode only, and the
     control plane refuses live credentials outside an explicit live-money
     configuration. Switch the Dashboard to Test mode and copy again." ;;
  esac
  case "$name" in
    STRIPE_SECRET_KEY)
      [[ "$value" == sk_test_* ]] || return 1 ;;
    STRIPE_PUBLISHABLE_KEY)
      [[ "$value" == pk_test_* ]] || return 1 ;;
    STRIPE_WEBHOOK_SECRET|MERC_CONNECT_WEBHOOK_SECRET)
      [[ "$value" == whsec_* ]] || return 1 ;;
    STRIPE_BILLING_WEBHOOK_ENDPOINT_ID|STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID)
      [[ "$value" == we_* ]] || return 1 ;;
    STRIPE_TEST_CONNECTED_ACCOUNT_ID)
      [[ "$value" == acct_* ]] || return 1 ;;
  esac
  return 0
}

# --------------------------------------------------------------------- one value
declare -a PENDING_NAMES=() PENDING_VALUES=()

ask() {
  local name="$1" expect="$2" where="$3" why="$4"
  local existing; existing="$(current "$name")"

  head2 "$name"
  printf '%s\n' "$why" | fold -s -w 76 | sed 's/^/  /'
  say ""
  dim "  Where to find it:"
  printf '%s\n' "$where" | fold -s -w 74 | sed 's/^/    /'
  say ""
  dim "  Expected format: $expect"

  if [ -n "$existing" ]; then
    say ""
    case "$existing" in
      sk_live_*|rk_live_*|pk_live_*)
        # A live credential already on disk is a standing hazard, not a
        # convenience. make stripe-check refuses it, the control plane refuses to
        # start on it outside a live-money config, and it should not survive a
        # test-mode setup pass.
        printf '%s  ! %s is currently a LIVE credential (%s)%s\n' \
          "$RED" "$name" "$(mask "$existing")" "$OFF"
        say "    Replacing it with a test key is strongly recommended. If you keep"
        say "    it, make stripe-check will refuse to run."
        read -r -p "  Replace it with a test key? [Y/n] " ans </dev/tty
        case "$ans" in n|N) warn "keeping the live credential — test tooling will refuse"; return 0 ;; esac
        ;;
      *)
        ok "already set to $(mask "$existing")"
        read -r -p "  Replace it? [y/N] " ans </dev/tty
        case "$ans" in y|Y) ;; *) return 0 ;; esac
        ;;
    esac
  fi

  local value=""
  while :; do
    say ""
    read -r -s -p "  Paste $name (input hidden, Enter to skip): " value </dev/tty
    say ""
    [ -z "$value" ] && { warn "skipped — $name left unset"; return 0; }
    value="$(printf '%s' "$value" | tr -d '[:space:]')"
    if validate "$name" "$value"; then
      ok "accepted $(mask "$value")"
      PENDING_NAMES+=("$name"); PENDING_VALUES+=("$value")
      return 0
    fi
    warn "that does not look like $expect — check you copied the whole value"
  done
}

# ------------------------------------------------------------------------ intro
cat <<'INTRO'

  Stripe test-mode setup
  ======================

  This asks for seven values and writes them to .env. Everything stays in TEST
  mode: no real money can move, and the scripts refuse a live key before they
  touch the network.

  Open the Stripe Dashboard and make sure the "Test mode" toggle (top right) is
  ON before you start. Every URL below assumes test mode.

  Nothing you paste is displayed or logged.

INTRO
read -r -p "  Press Enter when Test mode is on… " _ </dev/tty

# ------------------------------------------------------------------------- keys
ask STRIPE_SECRET_KEY "sk_test_…" \
"https://dashboard.stripe.com/test/apikeys
Developers → API keys → 'Secret key' → Reveal → copy." \
"The server-side key. Everything the control plane does with Stripe is signed with this."

ask STRIPE_PUBLISHABLE_KEY "pk_test_…" \
"Same page: https://dashboard.stripe.com/test/apikeys
'Publishable key' — it is shown in full, no reveal needed." \
"The browser-side key used when a buyer saves a card. Not secret, but it must match the account."

# ------------------------------------------------------------- billing webhook
cat <<'STEP'

  ──────────────────────────────────────────────────────────────────────────
  Next you create TWO webhook endpoints. They must be separate endpoints with
  DIFFERENT signing secrets — the validator rejects them being equal on
  purpose, so that a leaked billing secret cannot be used to forge Connect
  events like "a payout succeeded".
  ──────────────────────────────────────────────────────────────────────────
STEP

ask STRIPE_WEBHOOK_SECRET "whsec_…" \
"https://dashboard.stripe.com/test/webhooks → 'Add endpoint'
  URL:    https://YOUR-HOST/v1/stripe/webhook
  Events: payment_intent.succeeded, payment_intent.payment_failed,
          charge.refunded, charge.dispute.created, charge.dispute.closed
Create it, then 'Signing secret' → Reveal → copy." \
"Signs the BILLING webhook. Without it the control plane cannot trust that a payment actually succeeded, and it returns 503 rather than guessing."

ask STRIPE_BILLING_WEBHOOK_ENDPOINT_ID "we_… (skip if using \`stripe listen\`)" \
"Dashboard endpoints only: on that endpoint's page, the id in the URL.
If you are using \`stripe listen\` there is no we_ id — press Enter to skip.
See docs/ARCHITECTURE.md for which path you are on." \
"Identifies the billing endpoint in reconciliation receipts, so a later audit can tell which endpoint delivered what."

# ------------------------------------------------------------- connect webhook
ask MERC_CONNECT_WEBHOOK_SECRET "whsec_… (DIFFERENT from the billing one)" \
"https://dashboard.stripe.com/test/webhooks → 'Add endpoint' — a SECOND one
  URL:    https://YOUR-HOST/v1/stripe/connect-webhook
  Events: account.updated, transfer.created, transfer.reversed,
          payout.created, payout.paid, payout.failed
Create it, then reveal its own signing secret." \
"Signs the CONNECT webhook — supplier payouts and transfer reversals. This is the one that tells you money left the platform."

ask STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID "we_… (skip if using \`stripe listen\`)" \
"The second endpoint's id, same place. Skip if using \`stripe listen\`." \
"Identifies the Connect endpoint in payout reconciliation."

# -------------------------------------------------------------------- connect
ask STRIPE_TEST_CONNECTED_ACCOUNT_ID "acct_…" \
"https://dashboard.stripe.com/test/connect/accounts/overview
Enable Connect if you have not. Create a test connected account, open it, and
copy the acct_… id." \
"The test payout destination — it stands in for a supplier. Transfer reversal, the one path that has never met real Stripe, settles against this."

# ----------------------------------------------------------------------- write
if [ "${#PENDING_NAMES[@]}" -eq 0 ]; then
  head2 "Nothing to write"
  say "  No new values were entered. .env is unchanged."
  exit 0
fi

head2 "Writing ${#PENDING_NAMES[@]} value(s) to $ENV_FILE"
if [ -f "$ENV_FILE" ]; then
  backup="$ENV_FILE.bak.$(date -u +%Y%m%dT%H%M%SZ)"
  cp "$ENV_FILE" "$backup"; chmod 600 "$backup"
  dim "  backup: $backup"
else
  touch "$ENV_FILE"
fi
chmod 600 "$ENV_FILE"

tmp="$(mktemp "${TMPDIR:-/tmp}/merc-env.XXXXXX")"; chmod 600 "$tmp"
cp "$ENV_FILE" "$tmp"
for i in "${!PENDING_NAMES[@]}"; do
  n="${PENDING_NAMES[$i]}"; v="${PENDING_VALUES[$i]}"
  grep -v "^${n}=" "$tmp" > "$tmp.next" 2>/dev/null || : > "$tmp.next"
  printf '%s=%s\n' "$n" "$v" >> "$tmp.next"
  mv "$tmp.next" "$tmp"
  ok "$n"
done
mv "$tmp" "$ENV_FILE"; chmod 600 "$ENV_FILE"

# --------------------------------------------------------------------- verify
head2 "Checking what you have"
missing=()
for n in STRIPE_SECRET_KEY STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET \
         MERC_CONNECT_WEBHOOK_SECRET STRIPE_BILLING_WEBHOOK_ENDPOINT_ID \
         STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID; do
  v="$(current "$n")"
  if [ -n "$v" ]; then ok "$(printf '%-38s %s' "$n" "$(mask "$v")")"; else
    warn "$(printf '%-38s %s' "$n" "MISSING")"; missing+=("$n"); fi
done

for n in STRIPE_SECRET_KEY STRIPE_PUBLISHABLE_KEY STRIPE_RESTRICTED_KEY; do
  case "$(current "$n")" in
    sk_live_*|rk_live_*|pk_live_*)
      say ""
      warn "$n is still a LIVE credential. make stripe-check will refuse to run
     until it is replaced with a test key or removed from .env." ;;
  esac
done

billing="$(current STRIPE_WEBHOOK_SECRET)"; connect="$(current MERC_CONNECT_WEBHOOK_SECRET)"
if [ -n "$billing" ] && [ "$billing" = "$connect" ]; then
  say ""
  die "The billing and Connect signing secrets are identical. They must come from
     two SEPARATE endpoints — re-run and create the second one."
fi

head2 "Next"
if [ "${#missing[@]}" -gt 0 ]; then
  say "  ${#missing[@]} value(s) still missing."
  case " ${missing[*]} " in
    *ENDPOINT_ID*)
      say ""
      say "  The we_ endpoint ids are only needed for 'make stripe-matrix', which"
      say "  resends events to Dashboard-created endpoints. If you are on the"
      say "  'stripe listen' path that is expected — everything else still works."
      say "  See docs/STRIPE_CONNECT.md." ;;
  esac
  say ""
fi
cat <<'NEXT'
  When everything above is set:

      make stripe-check     # classifies credentials, refuses live keys, no network
      make stripe-matrix    # payment / refund / dispute / payout scenarios

  Watch transfer reversal specifically. It is implemented and simulator-tested,
  but it has never met real Stripe — it is the one place where a receipt has not
  yet been earned.

  Your .env is chmod 600 and gitignored. Nothing was printed in full.
NEXT
