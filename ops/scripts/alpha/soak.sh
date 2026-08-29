#!/usr/bin/env bash
# P1-RECOVERY-SOAK — RUN LAST. Do not start from the scaffold session.
#
# Default: print the exact start command and exit 0.
# --check: fail closed unless every prior gate is PASS.
# --execute: 24h external sampler. Refused unless priors PASS and
#            MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/alpha/soak.sh --print-start-command|--check|--execute [--duration N] [--interval N]

RUN-LAST. Durations below 86400 cannot produce qualifying evidence.
USAGE
  exit 2
}

DURATION="${MERC_ALPHA_SOAK_DURATION:-86400}"
INTERVAL="${MERC_ALPHA_SOAK_INTERVAL:-60}"
mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-start-command|--print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute) mode=execute ;;
    --duration) shift; DURATION="${1:-}" ;;
    --interval) shift; INTERVAL="${1:-}" ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

print_start() {
  cat <<EOF
# P1-RECOVERY-SOAK — RUN LAST. Do not start until every prior gate is PASS.
# Exit: Run the exact published candidate and retained prior digest against
# persistent staging, preserve source/state/restart receipts, and complete the
# configured 24-hour invariant and SLO window on two distinct external devices.
#
# Devices that must stay enrolled for the whole window:
#   studio  — this Mac Studio (28/60 M3 Ultra)
#   laptop  — operator MacBook (headless merc-agent)

## SUPERVISOR — before the 24h window (still part of this gate, not a skip)
# 1. Rollback / forward (prior digest then candidate) on the droplet.
#    Keep merc-postgres-1 / merc-minio-1. Only swap the control image.
#    ops/scripts/rollback.sh <prior-full-commit>
#    then redeploy the candidate (ops/scripts/alpha/deploy.sh --print-runbook §4)
#    and re-run ops/scripts/alpha/probes.sh --execute
# 2. Restart-storm the two Metal agents (durable session transition):
#    ops/scripts/go-closure-restart-storm.sh --target ssh --check
#    ops/scripts/go-closure-restart-storm.sh --target ssh --execute
#    If not using go-closure compose: kill -TERM each merc-agent, wait for
#    re-enrol/heartbeat, confirm agent_session_id changed in postgres.

## EXACT START COMMAND (qualifying 24h)
export MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1
ops/scripts/alpha/soak.sh --execute --duration 86400 --interval 60

## Isolated go-closure alternative (NOT this droplet's existing PG/MinIO):
# ops/scripts/go-closure-soak.sh --target ssh --duration 86400 --interval 60 --execute

## Status
# ops/scripts/alpha/status.sh
EOF
}

check_only() {
  alpha_require_command jq
  alpha_load_env_optional
  if ! alpha_check_ready P1-RECOVERY-SOAK; then
    alpha_die "P1-RECOVERY-SOAK is not execute-ready. RUN-LAST: close staging + parallel batch first."
  fi
  [[ "$DURATION" =~ ^[0-9]+$ ]] && [ "$DURATION" -ge 86400 ] \
    || alpha_die "qualifying soak duration must be >= 86400 (got $DURATION)"
  [[ "$INTERVAL" =~ ^[0-9]+$ ]] && [ "$INTERVAL" -ge 15 ] && [ "$INTERVAL" -le 900 ] \
    || alpha_die "interval must be 15-900 seconds"
  alpha_log "CHECK ok: all priors PASS. Start only with MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1"
  print_start
}

execute_soak() {
  alpha_require_command jq
  alpha_require_command curl
  alpha_load_env_optional
  alpha_require_execute_ready P1-RECOVERY-SOAK
  [ "${MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR:-}" = 1 ] \
    || alpha_die "refusing to start the 24h soak. Set MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR=1 after reading the runbook. This scaffold session must not start it."
  [[ "$DURATION" =~ ^[0-9]+$ ]] && [ "$DURATION" -ge 86400 ] \
    || alpha_die "qualifying soak duration must be >= 86400"
  [[ "$INTERVAL" =~ ^[0-9]+$ ]] && [ "$INTERVAL" -ge 15 ] && [ "$INTERVAL" -le 900 ] \
    || alpha_die "interval must be 15-900 seconds"

  local host commit samples started_epoch end_epoch now count=0 version ready
  host="$(alpha_staging_host)"
  commit="$(alpha_expected_commit)"
  mkdir -p "$ALPHA_RECEIPT_DIR"
  chmod 700 "$ALPHA_RECEIPT_DIR"
  samples="$ALPHA_RECEIPT_DIR/soak-samples.jsonl"
  : > "$samples"
  chmod 600 "$samples"
  started_epoch="$(date +%s)"
  end_epoch=$((started_epoch + DURATION))
  alpha_log "START soak host=$host duration=$DURATION interval=$INTERVAL (RUN-LAST)"

  while [ "$(date +%s)" -lt "$end_epoch" ]; do
    version="$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 30 "https://$host/version")" \
      || alpha_die "soak /version failed"
    ready="$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
      --connect-timeout 10 --max-time 30 "https://$host/readyz")" \
      || alpha_die "soak /readyz failed"
    jq -e --arg commit "$commit" '.commit == $commit and .modified == false' \
      <<< "$version" >/dev/null \
      || alpha_die "soak source identity drifted"
    jq -e '.status == "ready" and .payment_mode == "test" and .live_value_movement == false' \
      <<< "$ready" >/dev/null \
      || alpha_die "soak /readyz left test-mode ready"
    count=$((count + 1))
    jq -cn --arg at "$(alpha_utc)" --argjson n "$count" \
      --argjson version "$version" --argjson ready "$ready" \
      '{observed_at:$at,sequence:$n,version:$version,readyz:$ready}' >> "$samples"
    now="$(date +%s)"
    [ "$now" -lt "$end_epoch" ] || break
    if [ $((end_epoch - now)) -lt "$INTERVAL" ]; then
      sleep $((end_epoch - now))
    else
      sleep "$INTERVAL"
    fi
  done

  local actual expected min
  actual=$(( $(date +%s) - started_epoch ))
  expected=$((DURATION / INTERVAL))
  [ "$expected" -ge 1 ] || expected=1
  min=$(( (expected * 95 + 99) / 100 ))
  [ "$count" -ge "$min" ] || alpha_die "captured $count samples, below 95% of $expected"
  [ "$actual" -ge 86400 ] || alpha_die "elapsed ${actual}s < 86400; does not qualify"
  jq -n --arg host "$host" --arg commit "$commit" --arg samples "$samples" \
    --arg at "$(alpha_utc)" --argjson duration "$actual" --argjson n "$count" \
    '{schema_version:1,kind:"alpha_recovery_soak",status:"PASS",
      finished_at:$at,endpoint:$host,expected_commit:$commit,
      duration_seconds:$duration,samples:$n,samples_path:$samples,
      devices:["studio-mac-m3-ultra","operator-macbook"],
      qualification:{qualifies_for_24h_gate:true},
      policy:{stripe_live_mode:false,secret_values_recorded:false}}' \
    > "$ALPHA_RECEIPT_DIR/P1-RECOVERY-SOAK.samples.json"
  dest="$(alpha_write_receipt P1-RECOVERY-SOAK PASS alpha_recovery_soak)"
  alpha_log "PASS receipt: $dest"
}

case "$mode" in
  print) print_start ;;
  check) check_only ;;
  execute) execute_soak ;;
  *) usage ;;
esac
