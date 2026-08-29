#!/usr/bin/env bash
# Exercises the backup scheduler wiring that CI can cover without waiting 26h:
#   * systemd unit files exist and reference backup.sh
#   * backup.sh --dry-run writes MERC_BACKUP_STATUS_FILE
#   * control readBackupSignal interprets that file (via go test)
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
die() { echo "[test-backup-schedule] ERROR: $*" >&2; exit 1; }
log() { echo "[test-backup-schedule] $*"; }

[ -f "$ROOT/ops/systemd/merc-backup.service" ] || die "missing ops/systemd/merc-backup.service"
[ -f "$ROOT/ops/systemd/merc-backup.timer" ] || die "missing ops/systemd/merc-backup.timer"
grep -q 'ops/scripts/backup.sh' "$ROOT/ops/systemd/merc-backup.service" \
  || die "service unit does not invoke ops/scripts/backup.sh"
grep -q 'OnCalendar=' "$ROOT/ops/systemd/merc-backup.timer" \
  || die "timer unit missing OnCalendar="
grep -Eq 'merc_backup_age_seconds|93600' "$ROOT/ops/monitoring/alerts.yml" \
  || die "stale-backup alert threshold missing from ops/monitoring/alerts.yml"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/merc-backup-schedule.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT
STATUS="$WORK/last-successful-offsite-backup.unixtime"

# Minimal dry-run inputs: no docker/aws/offsite required.
export MERC_BACKUP_STATUS_FILE="$STATUS"
export MERC_BACKUP_OFFSITE="s3://merc-backup-schedule-test/unused"
export AWS_ACCESS_KEY_ID="test"
export AWS_SECRET_ACCESS_KEY="test"
# Prefix-only check in backup.sh; dry-run never encrypts.
export MERC_BACKUP_ENCRYPTION_RECIPIENT="age1dryrunqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq"

bash "$ROOT/ops/scripts/backup.sh" --dry-run
[ -f "$STATUS" ] || die "dry-run did not write status file $STATUS"
[ -s "$STATUS" ] || die "status file is empty"
ts="$(tr -d '[:space:]' <"$STATUS")"
[[ "$ts" =~ ^[0-9]+$ ]] || die "status file is not a unix timestamp: $ts"
now="$(date -u +%s)"
# Allow a few seconds of skew between write and read.
awk -v ts="$ts" -v now="$now" 'BEGIN { exit !(ts > 0 && ts <= now + 60 && now - ts < 120) }' \
  || die "status timestamp $ts is not a recent unix time (now=$now)"

log "status file ok: $STATUS=$ts"

# Metric path: existing unit test for readBackupSignal.
(
  cd "$ROOT/src/control"
  go test -count=1 -run 'TestReadBackupSignal$' .
)

log "PASS (timer units present; dry-run status file written; backup-age metric reader tested)"
log "NOTE: cannot wait 26h in CI for MercBackupStale to fire end-to-end"
