#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"
MODE="${1:-start}"
RUN="$ROOT/.artifacts/local-soak-24h"
LOG="$RUN/run.log"
STATE="$RUN/status.json"
RECEIPT="$ROOT/evidence/autonomous/local-soak-86400s.json"
SESSION="${CX_SOAK_TMUX_SESSION:-cx-soak-24h}"

gc_reject_live_stripe_environment
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY STRIPE_WEBHOOK_SECRET \
  CX_CONNECT_WEBHOOK_SECRET CX_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID

tmux_value() {
  tmux list-panes -t "$SESSION" -F "$1" 2>/dev/null | head -1
}

receipt_passes() {
  [ -s "$RECEIPT" ] && [ -s "$STATE" ] || return 1
  jq -e --slurpfile state "$STATE" '
    .status == "PASS" and .qualification.qualifies_for_24h_gate == true and
    .duration.actual_seconds >= 86400 and
    .started_at >= $state[0].started_at and .finished_at >= $state[0].started_at and
    .source_commit == $state[0].source_commit and
    .dirty_state_sha256 == $state[0].source_state_sha256
  ' "$RECEIPT" >/dev/null
}

case "$MODE" in
  run)
    mkdir -p "$RUN"
    exec bash "$ROOT/scripts/local-resilience-rehearsal.sh" soak --duration 86400 --interval 60 \
      > "$LOG" 2>&1
    ;;
  status)
    if receipt_passes; then
      jq -n --arg receipt "$RECEIPT" --arg session "$SESSION" \
        '{schema_version:1,status:"PASS",qualifies_for_24h_gate:true,receipt:$receipt,tmux_session:$session}'
      exit 0
    fi
    if tmux has-session -t "$SESSION" 2>/dev/null; then
      dead="$(tmux_value '#{pane_dead}')"
      pane_pid="$(tmux_value '#{pane_pid}')"
      if [ "$dead" = 0 ]; then
        jq -n --argjson pid "$pane_pid" --arg log "$LOG" --arg session "$SESSION" \
          '{schema_version:1,status:"IN_PROGRESS",pid:$pid,log:$log,tmux_session:$session,qualifies_for_24h_gate:false}'
        exit 0
      fi
      exit_status="$(tmux_value '#{pane_dead_status}')"
      jq -n --arg status "${exit_status:-unknown}" --arg log "$LOG" --arg session "$SESSION" \
        '{schema_version:1,status:"FAILED",process_exit_status:$status,log:$log,tmux_session:$session,qualifies_for_24h_gate:false}'
      exit 1
    fi
    jq -n '{schema_version:1,status:"NOT_RUNNING",qualifies_for_24h_gate:false}'
    exit 1
    ;;
  start) ;;
  *) echo 'usage: scripts/start-local-soak-24h.sh start|status' >&2; exit 2 ;;
esac

command -v tmux >/dev/null 2>&1 || {
  echo "start-local-soak-24h: tmux is required for process continuity" >&2
  exit 1
}
if tmux has-session -t "$SESSION" 2>/dev/null; then
  if [ "$(tmux_value '#{pane_dead}')" = 0 ]; then
    echo "24-hour soak is already running in tmux session $SESSION" >&2
    exit 1
  fi
  tmux kill-session -t "$SESSION"
fi

mkdir -p "$RUN"
umask 077
source_commit="$(git -C "$ROOT" rev-parse HEAD)"
source_state="$(gc_source_state_sha256 "$ROOT")"
jq -n --arg started "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg session "$SESSION" \
  --arg log "$LOG" --arg receipt "$RECEIPT" --arg source_commit "$source_commit" \
  --arg source_state "$source_state" '
  {schema_version:1,status:"IN_PROGRESS",started_at:$started,tmux_session:$session,log:$log,
   expected_receipt:$receipt,source_commit:$source_commit,source_state_sha256:$source_state,
   requested_seconds:86400,interval_seconds:60,
   continuity:"single tmux-owned process; shell/tool disconnects do not terminate the run",
   qualifies_for_24h_gate:false}' > "$STATE"

tmux new-session -d -s "$SESSION"
tmux set-window-option -t "$SESSION" remain-on-exit on
tmux respawn-pane -k -t "$SESSION":0.0 "exec '$ROOT/scripts/start-local-soak-24h.sh' run"
pane_pid="$(tmux_value '#{pane_pid}')"
printf 'started persistent 24-hour soak in tmux session %s (PID %s); status: make soak-24h-status\n' \
  "$SESSION" "$pane_pid"
