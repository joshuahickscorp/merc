#!/usr/bin/env bash
# Run the complete local proof in one disposable namespace and retain enough
# observation to distinguish a slow race detector from an actual hang.  This is
# proof instrumentation only: it never changes product code or test selection.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$ROOT"

RUN_ID="${MERC_FULL_PROOF_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$}"
case "$RUN_ID" in *[!A-Za-z0-9._-]*|'') echo "invalid MERC_FULL_PROOF_RUN_ID" >&2; exit 2;; esac
BASE="$ROOT/.artifacts/full-proof-$RUN_ID"
PROOF_ART="$BASE/prove"
LOG="$BASE/full-proof.log"
OBS="$BASE/observations"
HOME_DIR="$BASE/home"
mkdir -p "$OBS" "$HOME_DIR"
chmod 700 "$BASE" "$HOME_DIR"

# Defaults are derived from the PID but may be explicitly overridden by a
# caller that has already reserved ports.  Refuse reuse rather than sharing a
# database, object namespace, or control listener with another test run.
port_free() {
  python3 - "$1" <<'PY'
import socket, sys
with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 0)
    s.bind(("127.0.0.1", int(sys.argv[1])))
PY
}
PORT_SEED=$(( ( $$ % 1000 ) + 47000 ))
PGPORT="${PGPORT:-$PORT_SEED}"
MINIO_PORT="${MINIO_PORT:-$((PORT_SEED + 1000))}"
CONTROL_PORT="${CONTROL_PORT:-$((PORT_SEED + 2000))}"
for port in "$PGPORT" "$MINIO_PORT" "$CONTROL_PORT"; do
  port_free "$port" || { echo "proof port $port is occupied" >&2; exit 2; }
done

if pgrep -af '(prove-local\.sh|go test .*race|mutation-test)' >"$OBS/preexisting-test-processes.txt" 2>&1; then
  echo "another proof, race test, or mutation run is active; refusing concurrent proof" >&2
  exit 2
fi

start_epoch="$(date +%s)"
deadline=$((start_epoch + ${MERC_FULL_PROOF_TIMEOUT_SECONDS:-2700}))
printf '%s\n' "run_id=$RUN_ID" "started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  "timeout_seconds=$((deadline-start_epoch))" "pgport=$PGPORT" "minio_port=$MINIO_PORT" "control_port=$CONTROL_PORT" >"$BASE/metadata.txt"

HOME="$HOME_DIR" MERC_PROOF_ARTIFACT_DIR="$PROOF_ART" PGPORT="$PGPORT" MINIO_PORT="$MINIO_PORT" CONTROL_PORT="$CONTROL_PORT" \
  bash ops/scripts/prove-local.sh >"$LOG" 2>&1 &
PROOF_PID=$!
printf '%s\n' "$PROOF_PID" >"$BASE/proof.pid"

snapshot() {
  local label="$1"
  date -u +%Y-%m-%dT%H:%M:%SZ >"$OBS/$label.timestamp"
  ps -axo pid,ppid,stat,etime,%cpu,%mem,command >"$OBS/$label.processes.txt" || true
  pstree -p "$PROOF_PID" >"$OBS/$label.process-tree.txt" 2>/dev/null || true
  lsof -p "$PROOF_PID" >"$OBS/$label.open-files.txt" 2>/dev/null || true
  if [ -d "$PROOF_ART/pgdata" ]; then
    psql "postgres://cx@127.0.0.1:$PGPORT/postgres?sslmode=disable" -Atqc \
      "select pid,wait_event_type,wait_event,state,left(query,160) from pg_stat_activity order by pid" \
      >"$OBS/$label.pg-activity.txt" 2>&1 || true
    psql "postgres://cx@127.0.0.1:$PGPORT/postgres?sslmode=disable" -Atqc \
      "select pid,locktype,mode,granted from pg_locks order by pid,locktype,mode" \
      >"$OBS/$label.pg-locks.txt" 2>&1 || true
  fi
}

timeout_capture() {
  snapshot timeout
  # SIGQUIT requests Go goroutine dumps. The proof script owns this PID and its
  # descendants; never signal a broad process match.
  kill -QUIT "$PROOF_PID" 2>/dev/null || true
  sleep 5
  snapshot timeout-post-quit
  kill -TERM "$PROOF_PID" 2>/dev/null || true
  sleep 5
  kill -KILL "$PROOF_PID" 2>/dev/null || true
}

interrupted() {
  echo "INTERRUPTED; retaining diagnostics at $BASE" >&2
  timeout_capture
  exit 130
}
trap interrupted INT TERM

next_heartbeat=$((start_epoch + 30))
while kill -0 "$PROOF_PID" 2>/dev/null; do
  now="$(date +%s)"
  if [ "$now" -ge "$deadline" ]; then
    echo "TIMEOUT elapsed_seconds=$((now-start_epoch))" | tee -a "$LOG"
    timeout_capture
    echo "ENVIRONMENT_FAILURE timeout diagnostics retained at $BASE" >&2
    exit 124
  fi
  if [ "$now" -ge "$next_heartbeat" ]; then
    elapsed=$((now-start_epoch))
    printf 'HEARTBEAT elapsed_seconds=%s proof_pid=%s log_bytes=%s\n' "$elapsed" "$PROOF_PID" "$(wc -c <"$LOG")" | tee -a "$LOG"
    snapshot "heartbeat-$elapsed"
    next_heartbeat=$((now + 30))
  fi
  sleep 2
done

if wait "$PROOF_PID"; then
  status=0
else
  status=$?
fi
snapshot final
if [ "$status" -eq 0 ]; then
  echo "PASS full isolated proof completed; evidence=$BASE"
else
  echo "FAIL full isolated proof exited=$status; evidence=$BASE" >&2
fi
exit "$status"
