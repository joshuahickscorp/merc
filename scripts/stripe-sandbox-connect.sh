#!/usr/bin/env bash
# One-command Stripe Connect remainder.
#
#   scripts/stripe-sandbox-connect.sh
#   scripts/stripe-sandbox.sh connect
#
# After Connect is signed up on test platform acct_1TxbzMCwPLrR4vaY, this
# command creates the CA/CAD connected account, supplies the currently_due
# KYC fields (individual.phone, individual.relationship.title) so transfers
# goes active, transfers CAD, holds / manually releases / fails a standard
# bank-account payout (not instant-to-card), observes connected-account
# capability events on the connected event list, and keeps a Connect-scoped
# webhook. Basil omits the connect field on webhook_endpoint objects;
# Connect scope is application=ca_* when connect=true is accepted.
#
# Test-mode only. Never prints secret values. Never reads .merc-secrets.env.
# Never synthesizes a tr_ / acct_ / po_ / we_. Receipt stays BLOCKED until
# the remainder produces those objects. External dashboard gates still
# print "blocked: <id>" and exit 3.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/scripts/lib/stripe-sandbox-contract.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat >&2 <<'EOF'
usage: scripts/stripe-sandbox-connect.sh [--self-test]

One-command Connect remainder (CA/CAD account, KYC so transfers is
active, transfer, standard bank payout hold/release/failure, capability
events, Connect-scoped webhook).

Also accepted as: scripts/stripe-sandbox.sh connect
EOF
  exit 2
fi

# Refuse the live-secret file by name even if an operator points
# MERC_GO_CLOSURE_ENV_FILE at it.
ENV_FILE="${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"
case "$(basename -- "$ENV_FILE")" in
  .merc-secrets.env)
    echo "stripe-sandbox-connect: refusing to read .merc-secrets.env" >&2
    exit 1
    ;;
esac
if [ -f "$ENV_FILE" ]; then
  [ ! -L "$ENV_FILE" ] || { echo "stripe-sandbox-connect: environment file must not be a symlink" >&2; exit 1; }
  mode="$(stat -f '%Lp' "$ENV_FILE" 2>/dev/null || stat -c '%a' "$ENV_FILE")"
  case "$mode" in 600|400) ;; *) echo "stripe-sandbox-connect: environment file must have mode 0600 or 0400" >&2; exit 1 ;; esac
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

CREDS="$ROOT/.merc-credentials.env"
if [ -z "${STRIPE_SECRET_KEY:-}" ] && [ -f "$CREDS" ]; then
  case "$(basename -- "$CREDS")" in .merc-secrets.env) exit 1 ;; esac
  [ ! -L "$CREDS" ] || { echo "stripe-sandbox-connect: credentials file must not be a symlink" >&2; exit 1; }
  set -a
  # shellcheck disable=SC1090
  . "$CREDS"
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

# Live credentials may sit BESIDE the test ones. They may never be the operative
# key here.
#
# The operative slot is STRIPE_SECRET_KEY: it is what every request in this
# matrix authenticates with, so a live value there would create real connected
# accounts, real transfers and real payouts. That stays a hard refusal and it is
# not configurable.
#
# The parking slots below are a different thing. They exist so an operator can
# keep test and live credentials in one environment without this script becoming
# unrunnable — which is what happened before: any live value in any of these
# names exited 1 even though the script never read them, so adding live keys
# "in tandem" silently disabled the test matrix. They are recorded and then
# unset before any network call, so a later line cannot pick one up by accident.
#
# Live money remains NO_GO_PROHIBITED. Enabling it is not a matter of putting a
# key here: it needs the signed activation in ops/live-payment-activation.schema.json
# — HMAC, candidate commit, per-transaction caps, expiry, and three named
# approvals. See docs/LIVE_MONEY_TRANSITION.md.
if [ "$(classify "${STRIPE_SECRET_KEY:-}")" = live ]; then
  jq -nc --arg variable STRIPE_SECRET_KEY \
    '{schema_version:1,kind:"stripe_connect_remainder",status:"LIVE CREDENTIAL REFUSED",
      provider_mode:"refused",network_accessed:false,secret_values_printed:false,
      live_mode:"PROHIBITED",refused_variable:$variable,
      reason:"the operative key must be test-class; a live key here would move real money"}'
  exit 1
fi

parked_live=()
for name in STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  if [ "$(classify "${!name:-}")" = live ]; then
    parked_live+=("$name")
    unset "$name"
  fi
done
if [ "${#parked_live[@]}" -gt 0 ]; then
  printf 'stripe-sandbox-connect: live credential present and NOT used (scrubbed): %s\n' \
    "${parked_live[*]}" >&2
fi
export MERC_STRIPE_PARKED_LIVE_VARIABLES="${parked_live[*]:-}"

if [ "${1:-}" = "--self-test" ]; then
  python3 "$ROOT/scripts/lib/stripe-sandbox-connect.py" --self-test
  exit $?
fi

[ "$(classify "${STRIPE_SECRET_KEY:-}")" = test ] || {
  echo "stripe-sandbox-connect: test-class STRIPE_SECRET_KEY required" >&2
  exit 2
}

export STAGING_TLS_HOSTNAME="${STAGING_TLS_HOSTNAME:-mercmerc.net}"
export MERC_SETTLEMENT_CURRENCY="${MERC_SETTLEMENT_CURRENCY:-$MERC_STRIPE_CANDIDATE_CURRENCY}"
merc_stripe_valid_staging_hostname "$STAGING_TLS_HOSTNAME" \
  || { echo "stripe-sandbox-connect: STAGING_TLS_HOSTNAME is not a valid public hostname" >&2; exit 2; }
provided_settlement_currency="$(printf '%s' "$MERC_SETTLEMENT_CURRENCY" | tr '[:upper:]' '[:lower:]')"
[ "$provided_settlement_currency" = "$MERC_STRIPE_CANDIDATE_CURRENCY" ] \
  || { echo "stripe-sandbox-connect: settlement currency must be $MERC_STRIPE_CANDIDATE_CURRENCY" >&2; exit 2; }

unset STRIPE_LIVE_SECRET_KEY
if [[ "${STRIPE_RESTRICTED_KEY:-}" == rk_live_* ]]; then unset STRIPE_RESTRICTED_KEY; fi
if [[ "${STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset STRIPE_PUBLISHABLE_KEY; fi
if [[ "${NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; fi

export MERC_STRIPE_API_VERSION
export MERC_STRIPE_CANDIDATE_CURRENCY
export MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY
export MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING
export MERC_STRIPE_CANDIDATE_PAYOUT_SUCCESS_ACCOUNT
export MERC_STRIPE_CANDIDATE_PAYOUT_FAILURE_ACCOUNT
export MERC_STRIPE_CONNECT_ACCOUNT_TYPE
export MERC_STRIPE_CONNECT_WEBHOOK_EVENTS
export MERC_STRIPE_CONNECT_REMAINDER_COMMAND
# Full-matrix orchestration (scripts/stripe-sandbox.sh matrix) sets
# MERC_STRIPE_RUN_ID and MERC_STRIPE_FULL_MATRIX so this remainder shares
# the non-Connect run_id instead of minting a second identity.
export MERC_STRIPE_RUN_ID="${MERC_STRIPE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-l9cn}"
export MERC_STRIPE_MATRIX_OUT="${MERC_STRIPE_MATRIX_OUT:-$ROOT/evidence/external/stripe-sandbox-matrix.json}"

python3 "$ROOT/scripts/lib/stripe-sandbox-connect.py"
