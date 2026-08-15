#!/usr/bin/env bash
# One-screen "what's left to alpha" checklist.
# Self-contained. Does not ssh, deploy, call Stripe, or upload backups.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

alpha_require_command jq
alpha_load_env_optional

commit="$(git -C "$ROOT" rev-parse --short=12 HEAD 2>/dev/null || printf unknown)"
full="$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || printf unknown)"
boot="$(alpha_boot_status)"
host="$(alpha_staging_host)"

printf 'ALPHA GATES    commit=%s    boot=%s    host=%s\n' "$commit" "$boot" "$host"
printf 'scope: supervised Stripe test-mode private canary    live Stripe: PROHIBITED\n'
printf '────────────────────────────────────────────────────────────────────────────\n'
printf '%-26s %-36s %s\n' 'GATE' 'STATE' 'WHO'
printf '%-26s %-36s %s\n' '----' '-----' '---'

for gate in boot P1-STAGING P1-STRIPE-TEST P1-OFFSITE-RESTORE \
  P1-ALERT-DELIVERY P1-CANARY-REHEARSAL P1-RECOVERY-SOAK P1-GOVERNANCE \
  P1-INDEPENDENT-APPROVAL; do
  printf '%-26s %-36s %s\n' "$gate" "$(alpha_state_label "$gate")" "$(alpha_who_for "$gate")"
done

printf '────────────────────────────────────────────────────────────────────────────\n'
printf 'graph: boot -> staging -> {stripe-test, offsite-restore, alerts, canary} -> soak-LAST\n'
printf 'receipts: %s\n' "$ALPHA_RECEIPT_DIR"
printf 'boot receipt: %s\n' "$ALPHA_BOOT_RECEIPT"
printf 'go-no-go: %s (candidate %s)\n' "$ALPHA_GO_NO_GO" "$full"

if ! alpha_boot_is_green; then
  printf 'next: wait for BOUND PASS at %s from VENDOR_WALL_UPPER_BOUND\n' "$ALPHA_BOOT_RECEIPT"
else
  if [ "$(alpha_receipt_status P1-STAGING)" != PASS ]; then
    printf 'next: SUPERVISOR runs scripts/alpha/deploy.sh --print-runbook then --check\n'
  elif [ "$(alpha_receipt_status P1-RECOVERY-SOAK)" = PASS ]; then
    printf 'next: operator stamps P1-GOVERNANCE; do not start a second 24h soak\n'
  else
    printf 'next: parallel batch (stripe/offsite/alerts/canary), then soak --print-start-command\n'
  fi
fi
