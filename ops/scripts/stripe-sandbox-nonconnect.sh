#!/usr/bin/env bash
# Drive every Stripe Sandbox scenario that is not gated on Connect signup.
# Test-mode only. Never prints secret values. Never reads .merc-secrets.env.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/ops/scripts/lib/stripe-sandbox-contract.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "usage: ops/scripts/stripe-sandbox-nonconnect.sh" >&2
  exit 2
fi

# Refuse the live-secret file by name even if an operator points MERC_GO_CLOSURE_ENV_FILE at it.
ENV_FILE="${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"
case "$(basename -- "$ENV_FILE")" in
  .merc-secrets.env)
    echo "stripe-sandbox-nonconnect: refusing to read .merc-secrets.env" >&2
    exit 1
    ;;
esac
if [ -f "$ENV_FILE" ]; then
  [ ! -L "$ENV_FILE" ] || { echo "stripe-sandbox-nonconnect: environment file must not be a symlink" >&2; exit 1; }
  mode="$(stat -f '%Lp' "$ENV_FILE" 2>/dev/null || stat -c '%a' "$ENV_FILE")"
  case "$mode" in 600|400) ;; *) echo "stripe-sandbox-nonconnect: environment file must have mode 0600 or 0400" >&2; exit 1 ;; esac
  set -a
  # shellcheck disable=SC1090
  . "$ENV_FILE"
  set +a
fi

# A credentials-only file can supply the test key when .env.go-closure is absent.
CREDS="$ROOT/.merc-credentials.env"
if [ -z "${STRIPE_SECRET_KEY:-}" ] && [ -f "$CREDS" ]; then
  case "$(basename -- "$CREDS")" in .merc-secrets.env) exit 1 ;; esac
  [ ! -L "$CREDS" ] || { echo "stripe-sandbox-nonconnect: credentials file must not be a symlink" >&2; exit 1; }
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

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  value="${!name:-}"
  if [ "$(classify "$value")" = live ]; then
    jq -nc --arg variable "$name" \
      '{schema_version:1,kind:"stripe_sandbox_matrix",status:"LIVE CREDENTIAL REFUSED",
        provider_mode:"refused",network_accessed:false,secret_values_printed:false,
        live_mode:"PROHIBITED",refused_variable:$variable}'
    exit 1
  fi
done

[ "$(classify "${STRIPE_SECRET_KEY:-}")" = test ] || {
  echo "stripe-sandbox-nonconnect: test-class STRIPE_SECRET_KEY required" >&2
  exit 1
}

export STAGING_TLS_HOSTNAME="${STAGING_TLS_HOSTNAME:-mercmerc.net}"
export MERC_SETTLEMENT_CURRENCY="${MERC_SETTLEMENT_CURRENCY:-$MERC_STRIPE_CANDIDATE_CURRENCY}"
merc_stripe_valid_staging_hostname "$STAGING_TLS_HOSTNAME" \
  || { echo "stripe-sandbox-nonconnect: STAGING_TLS_HOSTNAME is not a valid public hostname" >&2; exit 1; }
provided_settlement_currency="$(printf '%s' "$MERC_SETTLEMENT_CURRENCY" | tr '[:upper:]' '[:lower:]')"
[ "$provided_settlement_currency" = "$MERC_STRIPE_CANDIDATE_CURRENCY" ] \
  || { echo "stripe-sandbox-nonconnect: settlement currency must be $MERC_STRIPE_CANDIDATE_CURRENCY" >&2; exit 1; }

unset STRIPE_LIVE_SECRET_KEY
if [[ "${STRIPE_RESTRICTED_KEY:-}" == rk_live_* ]]; then unset STRIPE_RESTRICTED_KEY; fi
if [[ "${STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset STRIPE_PUBLISHABLE_KEY; fi
if [[ "${NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY:-}" == pk_live_* ]]; then unset NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; fi

export MERC_STRIPE_RUN_ID="${MERC_STRIPE_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-l9nc}"
export MERC_STRIPE_MATRIX_OUT="${MERC_STRIPE_MATRIX_OUT:-$ROOT/evidence/external/stripe-sandbox-matrix.json}"
handler_receipt="$(mktemp "${TMPDIR:-/tmp}/merc-stripe-handler.XXXXXX")"
trap 'rm -f "$handler_receipt"' EXIT INT TERM
export MERC_STRIPE_HANDLER_RECEIPT="$handler_receipt"

# Isolated local handler: signature / wrong-authority / api-version / account-mismatch
# plus CAD cash apply/stale/duplicate when a test database is reachable.
if [ -z "${MERC_TEST_DATABASE_URL:-}" ]; then
  export MERC_TEST_DATABASE_URL="${DATABASE_URL:-postgres://cx:cx@localhost:5432/cx?sslmode=disable}"
fi
if command -v go >/dev/null 2>&1; then
  (
    cd "$ROOT/src/control"
    # src/control/go.mod pins a newer toolchain than the host default.
    GOTOOLCHAIN=auto go test -count=1 -timeout 180s -run TestNonconnectWebhookRefusals
  ) || {
    echo "stripe-sandbox-nonconnect: local real-handler refusal test failed" >&2
    exit 1
  }
else
  echo "stripe-sandbox-nonconnect: go toolchain missing; refusals will not have a local handler receipt" >&2
fi

python3 "$ROOT/ops/scripts/lib/stripe-sandbox-nonconnect.py"
