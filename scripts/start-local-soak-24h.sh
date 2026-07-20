#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"
MODE="${1:-start}"
RUN="$ROOT/.artifacts/local-soak-24h"
PID_FILE="$RUN/pid"
LOG="$RUN/run.log"
STATE="$RUN/status.json"
RECEIPT="$ROOT/evidence/autonomous/local-soak-86400s.json"

gc_reject_live_stripe_environment
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET \
  CX_CONNECT_WEBHOOK_SECRET CX_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID

running_pid() {
  [ -s "$PID_FILE" ] || return 1
  pid="$(tr -d '[:space:]' < "$PID_FILE")"
  [[ "$pid" =~ ^[0-9]+$ ]] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  ps -p "$pid" -o command= | grep -q 'local-resilience-rehearsal.sh soak'
}

case "$MODE" in
  status)
    if [ -s "$RECEIPT" ] && jq -e '.status=="PASS" and .qualification.qualifies_for_24h_gate==true' "$RECEIPT" >/dev/null; then
      jq -n --arg receipt "$RECEIPT" '{schema_version:1,status:"PASS",qualifies_for_24h_gate:true,receipt:$receipt}'
      exit 0
    fi
    if running_pid; then
      jq -n --argjson pid "$pid" --arg log "$LOG" '{schema_version:1,status:"IN_PROGRESS",pid:$pid,log:$log,qualifies_for_24h_gate:false}'
      exit 0
    fi
    jq -n '{schema_version:1,status:"NOT_RUNNING",qualifies_for_24h_gate:false}'
    exit 1
    ;;
  start) ;;
  *) echo 'usage: scripts/start-local-soak-24h.sh start|status' >&2; exit 2 ;;
esac

if running_pid; then
  echo "24-hour soak is already running as PID $pid" >&2
  exit 1
fi
mkdir -p "$RUN"
umask 077
nohup bash "$ROOT/scripts/local-resilience-rehearsal.sh" soak --duration 86400 --interval 60 \
  > "$LOG" 2>&1 < /dev/null &
pid=$!
printf '%s\n' "$pid" > "$PID_FILE"
jq -n --arg started "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --argjson pid "$pid" \
  --arg log "$LOG" --arg receipt "$RECEIPT" '
  {schema_version:1,status:"IN_PROGRESS",started_at:$started,pid:$pid,log:$log,
   expected_receipt:$receipt,requested_seconds:86400,interval_seconds:60,
   continuity:"single persistent background process; interruption invalidates the run",
   qualifies_for_24h_gate:false}' > "$STATE"
printf 'started persistent 24-hour soak as PID %s; status: make soak-24h-status\n' "$pid"
