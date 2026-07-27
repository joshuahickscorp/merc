#!/usr/bin/env bash
# Prove merc_backup_age_seconds moves when a backup status is written and is
# stale when the status is old. Status is derived only from measured ages.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
ART="${MERC_BACKUP_AGE_DIR:-$ROOT/.artifacts/backup-age-metric}/$(date -u +%Y%m%dT%H%M%SZ)-$$"
EVIDENCE_OUT="${MERC_BACKUP_AGE_EVIDENCE:-$ROOT/evidence/autonomous/backup-age-metric.json}"
STALE_THRESHOLD="${MERC_BACKUP_STALE_SECONDS:-93600}"

die() { echo "[test-backup-age-metric] ERROR: $*" >&2; exit 1; }
log() { echo "[test-backup-age-metric] $*"; }

for tool in go jq python3; do
  command -v "$tool" >/dev/null 2>&1 || die "missing dependency: $tool"
done
grep -Eq 'merc_backup_age_seconds\s*>\s*93600' "$ROOT/monitoring/alerts.yml" \
  || die "MercBackupStale threshold missing from monitoring/alerts.yml"

mkdir -p "$ART" "$(dirname "$EVIDENCE_OUT")"
STATUS_FILE="$ART/last-successful-offsite-backup.unixtime"

# --- Fresh backup signal (simulates scripts/backup.sh post-verify write) ---
FRESH_TS="$(date -u +%s)"
STATUS_TMP="${STATUS_FILE}.tmp.$$"
printf '%s\n' "$FRESH_TS" >"$STATUS_TMP"
chmod 0640 "$STATUS_TMP"
mv -f -- "$STATUS_TMP" "$STATUS_FILE"
log "wrote fresh backup status ts=$FRESH_TS path=$STATUS_FILE"

# Measure via the same reader the /metrics handler uses.
FRESH_JSON="$(
  cd "$ROOT/control"
  MERC_BACKUP_STATUS_FILE="$STATUS_FILE" go test -count=1 -run 'TestBackupAgeMetricObservation$' . -json 2>/dev/null \
    | jq -sr 'map(select(.Action=="output" and (.Output|test("BACKUP_AGE_OBSERVATION")))) | last | .Output' \
    | sed -n 's/^.*BACKUP_AGE_OBSERVATION //p' \
    | tr -d '\n'
)"
# Fallback: run helper binary-style via go test -v
if [ -z "$FRESH_JSON" ] || ! echo "$FRESH_JSON" | jq -e . >/dev/null 2>&1; then
  FRESH_JSON="$(
    cd "$ROOT/control"
    MERC_BACKUP_STATUS_FILE="$STATUS_FILE" go test -count=1 -v -run 'TestBackupAgeMetricObservation$' . 2>&1 \
      | sed -n 's/^.*BACKUP_AGE_OBSERVATION //p' | tail -1
  )"
fi
echo "$FRESH_JSON" | jq -e . >/dev/null 2>&1 || die "fresh age observation missing/unparseable: $FRESH_JSON"
printf '%s\n' "$FRESH_JSON" >"$ART/fresh-observation.json"

FRESH_AGE="$(echo "$FRESH_JSON" | jq -r '.age_seconds')"
FRESH_VALID="$(echo "$FRESH_JSON" | jq -r '.valid')"
FRESH_CONFIGURED="$(echo "$FRESH_JSON" | jq -r '.configured')"
log "fresh observation configured=$FRESH_CONFIGURED valid=$FRESH_VALID age_seconds=$FRESH_AGE"

# --- Stale backup signal (no new backup; old timestamp) ---
STALE_TS=$(( FRESH_TS - STALE_THRESHOLD - 3600 ))
STATUS_TMP="${STATUS_FILE}.tmp.$$"
printf '%s\n' "$STALE_TS" >"$STATUS_TMP"
chmod 0640 "$STATUS_TMP"
mv -f -- "$STATUS_TMP" "$STATUS_FILE"
log "wrote stale backup status ts=$STALE_TS (threshold=$STALE_THRESHOLD)"

STALE_JSON="$(
  cd "$ROOT/control"
  MERC_BACKUP_STATUS_FILE="$STATUS_FILE" go test -count=1 -v -run 'TestBackupAgeMetricObservation$' . 2>&1 \
    | sed -n 's/^.*BACKUP_AGE_OBSERVATION //p' | tail -1
)"
echo "$STALE_JSON" | jq -e . >/dev/null 2>&1 || die "stale age observation missing/unparseable: $STALE_JSON"
printf '%s\n' "$STALE_JSON" >"$ART/stale-observation.json"

STALE_AGE="$(echo "$STALE_JSON" | jq -r '.age_seconds')"
STALE_VALID="$(echo "$STALE_JSON" | jq -r '.valid')"
log "stale observation valid=$STALE_VALID age_seconds=$STALE_AGE"

# Optional: also exercise backup.sh status writer if --dry-run is available.
DRY_RUN_STATUS="$ART/dry-run-status.unixtime"
DRY_RUN_OK=false
if grep -q -- '--dry-run' "$ROOT/scripts/backup.sh"; then
  set +e
  MERC_BACKUP_STATUS_FILE="$DRY_RUN_STATUS" \
  MERC_BACKUP_OFFSITE="s3://cx-backup-age-metric-test/unused" \
  AWS_ACCESS_KEY_ID="test" \
  AWS_SECRET_ACCESS_KEY="test" \
  MERC_BACKUP_ENCRYPTION_RECIPIENT="age1dryrunqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqqq" \
    bash "$ROOT/scripts/backup.sh" --dry-run >"$ART/backup-dry-run.log" 2>&1
  DRY_RC=$?
  set -e
  if [ "$DRY_RC" -eq 0 ] && [ -s "$DRY_RUN_STATUS" ]; then
    DRY_RUN_OK=true
    log "backup.sh --dry-run wrote status file"
  else
    log "backup.sh --dry-run not usable (rc=$DRY_RC); metric path still proven via status file"
  fi
fi

# Derive PASS only from measurements.
STATUS="$(python3 - "$FRESH_AGE" "$STALE_AGE" "$FRESH_VALID" "$STALE_VALID" "$STALE_THRESHOLD" <<'PY'
import json, sys
fresh_age = float(sys.argv[1])
stale_age = float(sys.argv[2])
fresh_valid = sys.argv[3].lower() == "true"
stale_valid = sys.argv[4].lower() == "true"
threshold = float(sys.argv[5])
# Fresh must be young; stale must exceed the alert threshold; ages must move.
ok = (
    fresh_valid and stale_valid
    and fresh_age >= 0 and fresh_age < 120
    and stale_age > threshold
    and stale_age > fresh_age
)
print("PASS" if ok else "FAIL")
print(json.dumps({
    "fresh_age_below_120s": fresh_age < 120,
    "stale_age_above_threshold": stale_age > threshold,
    "age_increased": stale_age > fresh_age,
    "both_valid": fresh_valid and stale_valid,
}), file=sys.stderr)
PY
)"

jq -n \
  --arg status "$STATUS" \
  --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson fresh "$FRESH_JSON" \
  --argjson stale "$STALE_JSON" \
  --argjson stale_threshold "$STALE_THRESHOLD" \
  --argjson dry_run_status_written "$DRY_RUN_OK" \
  --arg alert "MercBackupStale" \
  --arg alert_expr "merc_backup_age_seconds > 93600" \
  '{
    schema_version:1,
    kind:"backup_age_metric",
    status:$status,
    completed_at:$completed_at,
    alert:{name:$alert,expr:$alert_expr,threshold_seconds:$stale_threshold},
    observations:{
      after_backup_signal:$fresh,
      after_no_backup_stale_signal:$stale
    },
    derived:{
      age_moved: (($stale.age_seconds|tonumber) > ($fresh.age_seconds|tonumber)),
      fresh_age_seconds:($fresh.age_seconds|tonumber),
      stale_age_seconds:($stale.age_seconds|tonumber),
      stale_would_fire_alert: (($stale.age_seconds|tonumber) > $stale_threshold),
      backup_sh_dry_run_status_written:$dry_run_status_written
    },
    secret_values_recorded:false
  }' >"$ART/evidence.json"

cp "$ART/evidence.json" "$EVIDENCE_OUT"
log "status=$STATUS evidence=$EVIDENCE_OUT"
[ "$STATUS" = "PASS" ] || die "derived status is $STATUS (metric did not move / did not go stale)"
log "PASS backup age metric moved on backup signal and exceeded stale threshold without a fresh backup"
