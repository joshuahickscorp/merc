#!/usr/bin/env bash
# P1-STRIPE-TEST matrix runner. Test-mode only. Never live.
#
# Wraps ops/scripts/stripe-sandbox.sh. --check never opens a Stripe connection.
# --execute is SUPERVISOR: it calls the existing test-mode matrix.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/alpha/stripe-test.sh --print-runbook|--check|--execute

--check    classify keys locally, refuse live, require boot+staging (no Stripe I/O)
--execute  SUPERVISOR: ops/scripts/stripe-sandbox.sh matrix against the deployed candidate
USAGE
  exit 2
}

print_runbook() {
  cat <<EOF
# P1-STRIPE-TEST — SUPERVISOR EXECUTES (this scaffold session does not)
# Exit: Execute every scenario with sk_test/whsec test credentials and distinct
# endpoint secrets; preserve redacted provider IDs and reconciliation receipts.
#
# Bind to the deployed candidate:
#   STAGING_TLS_HOSTNAME=$(alpha_staging_host)
#   MERC_SETTLEMENT_CURRENCY=cad
# Webhooks must already point at:
#   https://$(alpha_staging_host)/v1/stripe/webhook
#   https://$(alpha_staging_host)/v1/stripe/connect-webhook
#
# Matrix (existing authority):
#   payment success, decline, idempotency, timeout recovery
#   refund (partial + remaining; excess refused)
#   dispute open/resolve
#   Connect restriction
#   payout hold / release / failure / reversal
#   replay + out-of-order webhook
#   provider reconciliation
#
# Commands:
set -a; . ./.env.go-closure; set +a
ops/scripts/alpha/stripe-test.sh --check
make stripe-check
make stripe-matrix
# or:
ops/scripts/alpha/stripe-test.sh --execute
#
# Receipt lands under evidence/ (stripe-sandbox.sh stdout) and
# $ALPHA_RECEIPT_DIR/P1-STRIPE-TEST.json
# Never export sk_live_ / rk_live_ / pk_live_. Distinct billing vs Connect whsec.
EOF
}

check_only() {
  local secret_class pub_class
  alpha_require_command jq
  alpha_load_env_optional
  secret_class="$(alpha_stripe_class "${STRIPE_SECRET_KEY:-}")"
  pub_class="$(alpha_stripe_class "${STRIPE_PUBLISHABLE_KEY:-}")"
  printf 'stripe_secret_class: %s\n' "$secret_class"
  printf 'stripe_publishable_class: %s\n' "$pub_class"
  printf 'billing_webhook: %s\n' "$(alpha_stripe_class "${STRIPE_WEBHOOK_SECRET:-}")"
  printf 'connect_webhook: %s\n' "$(alpha_stripe_class "${MERC_CONNECT_WEBHOOK_SECRET:-}")"
  [ "$secret_class" != live ] || alpha_die "live secret classified"
  [ "$pub_class" != live ] || alpha_die "live publishable classified"
  if [ -n "${STRIPE_WEBHOOK_SECRET:-}" ] && [ -n "${MERC_CONNECT_WEBHOOK_SECRET:-}" ] \
    && [ "$STRIPE_WEBHOOK_SECRET" = "$MERC_CONNECT_WEBHOOK_SECRET" ]; then
    alpha_die "billing and Connect webhook secrets must be distinct"
  fi
  if ! alpha_check_ready P1-STRIPE-TEST; then
    alpha_die "P1-STRIPE-TEST is not execute-ready (boot/staging)"
  fi
  [ "$secret_class" = test ] || alpha_die "STRIPE_SECRET_KEY must be sk_test_* or rk_test_* for --execute"
  [ "${STRIPE_WEBHOOK_SECRET:-}" != "${STRIPE_WEBHOOK_SECRET#whsec_}" ] \
    || alpha_die "STRIPE_WEBHOOK_SECRET must be whsec_*"
  [ "${MERC_CONNECT_WEBHOOK_SECRET:-}" != "${MERC_CONNECT_WEBHOOK_SECRET#whsec_}" ] \
    || alpha_die "MERC_CONNECT_WEBHOOK_SECRET must be whsec_*"
  alpha_log "CHECK ok: test-class keys, distinct webhooks, boot+staging PASS (no Stripe I/O)"
}

execute_matrix() {
  alpha_require_command jq
  alpha_load_env_optional
  alpha_require_execute_ready P1-STRIPE-TEST
  [ -x "$ROOT/ops/scripts/stripe-sandbox.sh" ] || alpha_die "missing ops/scripts/stripe-sandbox.sh"
  local out
  out="$(mktemp "${TMPDIR:-/tmp}/merc-alpha-stripe.XXXXXX")"
  if ! bash "$ROOT/ops/scripts/stripe-sandbox.sh" matrix >"$out" 2>"${out}.err"; then
    alpha_log "stripe-sandbox.sh matrix failed: $(head -c 400 "${out}.err" 2>/dev/null || true)"
    rm -f "$out" "${out}.err"
    exit 1
  fi
  jq -e '.status == "PASS" and .provider_mode == "test" and .live_mode == "PROHIBITED"' \
    "$out" >/dev/null \
    || alpha_die "stripe matrix receipt is not PASS test-mode"
  mkdir -p "$ALPHA_RECEIPT_DIR"
  chmod 700 "$ALPHA_RECEIPT_DIR"
  umask 077
  jq --arg at "$(alpha_utc)" \
    '. + {alpha_gate:"P1-STRIPE-TEST",alpha_finished_at:$at}' "$out" \
    > "$ALPHA_RECEIPT_DIR/P1-STRIPE-TEST.json"
  chmod 600 "$ALPHA_RECEIPT_DIR/P1-STRIPE-TEST.json"
  rm -f "$out" "${out}.err"
  alpha_log "PASS receipt: $ALPHA_RECEIPT_DIR/P1-STRIPE-TEST.json"
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute) mode=execute ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_only ;;
  execute) execute_matrix ;;
  *) usage ;;
esac
