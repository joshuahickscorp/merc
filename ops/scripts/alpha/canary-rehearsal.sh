#!/usr/bin/env bash
# P1-CANARY-REHEARSAL: 2 approved synthetic buyers + 2 operator-controlled
# Metal workers (Mac Studio + MacBook) + the strict scenario adapter set.
#
# Wraps ops/scripts/go-closure-canary-rehearsal.sh and ops/scripts/alpha/scenarios.sh.
# --check never enrols or submits jobs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/alpha/canary-rehearsal.sh --print-runbook|--check|--execute|--record-pass
USAGE
  exit 2
}

print_runbook() {
  cat <<EOF
# P1-CANARY-REHEARSAL
# Exit: Exercise the complete counted scenario matrix within every fail-closed
# limit, including kill switches, revocations, restore, alert, reconciliation,
# result retrieval during intake pause, and no payout export.

## Devices (exactly two, operator-controlled, Metal)
# 1. studio  — this Mac Studio (28/60 M3 Ultra)
# 2. laptop  — operator MacBook, merc-agent headless via screen-share
ops/scripts/alpha/enrol-worker.sh --device studio --print-runbook
ops/scripts/alpha/enrol-worker.sh --device laptop --print-runbook

## Counted matrix (existing go-closure driver)
#   approved_buyer_identity:2
#   distinct_metal_agent:2
#   embed_success:20
#   batch_infer_success:20
#   cancelled_job:5
#   forced_retry:5
#   stale_lease_recovery:3
#   stale_attempt_commit_rejection:3
#   buyer_webhook_retry_sequence:3
#   backup_independent_restore:1
#   stripe_test_matrix:1
#   real_alert_firing_resolution:1
#   post_rehearsal_invariant_audit:1
#   bounded_retry_backoff_audit:1

## Extra adapters (ops/scripts/alpha/scenarios.sh) required by the exit criterion
#   kill_switch
#   revocation
#   intake_pause_result_retrieval
#   no_payout_export

## Execute (SUPERVISOR, after both workers heartbeat)
# Pin the reviewed driver:
#   MERC_CANARY_SCENARIO_DRIVER=$ROOT/ops/scripts/canary-scenario-driver.sh
#   MERC_CANARY_APPROVED_DRIVER_SHA256=\$(shasum -a 256 \$MERC_CANARY_SCENARIO_DRIVER | awk '{print \$1}')
# Prefer the alpha wrapper so extra adapters run:
ops/scripts/alpha/canary-rehearsal.sh --execute
# Isolated go-closure path (second postgres — NOT this droplet):
#   ops/scripts/go-closure-canary-rehearsal.sh --target ssh --check
#   ops/scripts/go-closure-canary-rehearsal.sh --target ssh --execute
EOF
}

count_csv() {
  printf '%s' "$1" | awk -F',' '{n=0; for(i=1;i<=NF;i++){gsub(/^ +| +$/,"",$i); if($i!="")n++} print n}'
}

check_only() {
  alpha_require_command jq
  alpha_load_env_optional
  if ! alpha_check_ready P1-CANARY-REHEARSAL; then
    alpha_die "P1-CANARY-REHEARSAL is not execute-ready (boot/staging)"
  fi
  [ -n "${MERC_CANARY_APPROVED_BUYER_EMAILS:-}" ] \
    || alpha_die "MERC_CANARY_APPROVED_BUYER_EMAILS is required"
  [ -n "${MERC_CANARY_APPROVED_WORKER_IDS:-}" ] \
    || alpha_die "MERC_CANARY_APPROVED_WORKER_IDS is required"
  [ "$(count_csv "$MERC_CANARY_APPROVED_BUYER_EMAILS")" = 2 ] \
    || alpha_die "need exactly two approved buyer emails"
  [ "$(count_csv "$MERC_CANARY_APPROVED_WORKER_IDS")" = 2 ] \
    || alpha_die "need exactly two approved worker UUIDs"
  jq -en --arg v "$MERC_CANARY_APPROVED_WORKER_IDS" '
    ($v | split(",") | map(gsub("^\\s+|\\s+$";"")) | map(select(length>0))) as $ids |
    ($ids | length) == 2 and ($ids | unique | length) == 2 and
    all($ids[]; test("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$"))
  ' >/dev/null || alpha_die "worker IDs must be two distinct RFC UUIDs (version nibble 1-5; demo 0000- ids are refused)"
  [ -x "$ROOT/ops/scripts/canary-scenario-driver.sh" ] \
    || alpha_die "missing ops/scripts/canary-scenario-driver.sh"
  [ -x "$ROOT/ops/scripts/alpha/scenarios.sh" ] \
    || alpha_die "missing ops/scripts/alpha/scenarios.sh"
  [ -x "$ROOT/ops/scripts/alpha/enrol-worker.sh" ] \
    || alpha_die "missing ops/scripts/alpha/enrol-worker.sh"
  [ -z "${MERC_PAYOUT_EXPORT:-}" ] \
    || alpha_die "MERC_PAYOUT_EXPORT must be unset for the canary"
  alpha_log "CHECK ok: 2 buyers, 2 worker UUIDs, drivers present, no payout export"
}

execute_rehearsal() {
  alpha_require_command jq
  alpha_load_env_optional
  alpha_require_execute_ready P1-CANARY-REHEARSAL
  check_only
  local extras
  extras="kill_switch revocation intake_pause_result_retrieval no_payout_export"
  # Extra adapters first — they are fail-closed and cheap.
  local scenario
  for scenario in $extras; do
    alpha_log "adapter $scenario"
    bash "$ROOT/ops/scripts/alpha/scenarios.sh" run "$scenario" 1
  done
  if [ -n "${MERC_CANARY_SCENARIO_DRIVER:-}" ] && [ -x "${MERC_CANARY_SCENARIO_DRIVER}" ]; then
    alpha_log "delegating counted matrix to go-closure-canary-rehearsal.sh --target ${MERC_ALPHA_CANARY_TARGET:-local}"
    bash "$ROOT/ops/scripts/go-closure-canary-rehearsal.sh" \
      --target "${MERC_ALPHA_CANARY_TARGET:-local}" --execute
  else
    alpha_die "MERC_CANARY_SCENARIO_DRIVER is not set to a reviewed executable; counted matrix not run. Extra adapters completed. Pin the driver SHA and re-run."
  fi
  dest="$(alpha_write_receipt P1-CANARY-REHEARSAL PASS alpha_canary_rehearsal)"
  alpha_log "PASS receipt: $dest"
}

record_pass() {
  alpha_load_env_optional
  alpha_require_execute_ready P1-CANARY-REHEARSAL
  dest="$(alpha_write_receipt P1-CANARY-REHEARSAL PASS alpha_canary_rehearsal)"
  alpha_log "PASS receipt: $dest"
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute) mode=execute ;;
    --record-pass) mode=record ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_only ;;
  execute) execute_rehearsal ;;
  record) record_pass ;;
  *) usage ;;
esac
