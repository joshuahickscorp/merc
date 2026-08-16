#!/usr/bin/env bash
# One command: every deterministic recovery failure mode. Non-zero on any failure.
#
# Throwaway Docker PostgreSQL + MinIO only. Does not touch staging. Does not
# rewrite restore-drill or local-independent-restore; it runs them.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
RUN="merc-recovery-$$"
ART="${MERC_RECOVERY_DIR:-$ROOT/.artifacts/recovery-suite}/$RUN"
EVENTS="$ART/go-test.jsonl"
PG_IMAGE="${MERC_RECOVERY_PG_IMAGE:-postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d}"
MINIO_IMAGE="${MERC_RECOVERY_MINIO_IMAGE:-minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e}"
MC_IMAGE="${MERC_RECOVERY_MC_IMAGE:-minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727}"
PG_C="${RUN}-pg"
MINIO_C="${RUN}-minio"
NET="${RUN}-net"
PG_VOL="${RUN}-pgdata"
MINIO_VOL="${RUN}-minio"
PG_PORT="${MERC_RECOVERY_PG_PORT:-$((55460 + RANDOM % 80))}"
MINIO_PORT="${MERC_RECOVERY_MINIO_PORT:-$((9100 + RANDOM % 80))}"
PG_PW="recovery-${RANDOM}-${RANDOM}"
MINIO_USER="recovery${RANDOM}"
MINIO_SECRET="recovery-${RANDOM}-${RANDOM}-object"

die() { echo "[recovery-suite] ERROR: $*" >&2; exit 1; }
log() { echo "[recovery-suite] $*"; }

cleanup() {
  code=$?
  docker rm -f "$PG_C" "$MINIO_C" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm "$PG_VOL" "$MINIO_VOL" >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT INT TERM

for tool in docker go python3 jq; do
  command -v "$tool" >/dev/null 2>&1 || die "missing dependency: $tool"
done

mkdir -p "$ART" "$ROOT/evidence/recovery" "$ROOT/evidence/autonomous"

log "starting throwaway PostgreSQL and MinIO (ports $PG_PORT / $MINIO_PORT)"
docker network create "$NET" >/dev/null
docker volume create "$PG_VOL" >/dev/null
docker volume create "$MINIO_VOL" >/dev/null
docker run -d --name "$PG_C" --network "$NET" --network-alias postgres \
  -e POSTGRES_USER=cx -e POSTGRES_PASSWORD="$PG_PW" -e POSTGRES_DB=cx \
  -p "127.0.0.1:$PG_PORT:5432" \
  -v "$PG_VOL:/var/lib/postgresql/data" \
  "$PG_IMAGE" >/dev/null
docker run -d --name "$MINIO_C" --network "$NET" --network-alias minio \
  -e MINIO_ROOT_USER="$MINIO_USER" -e MINIO_ROOT_PASSWORD="$MINIO_SECRET" \
  -p "127.0.0.1:$MINIO_PORT:9000" \
  -v "$MINIO_VOL:/data" \
  "$MINIO_IMAGE" server /data >/dev/null

deadline=$(( $(date +%s) + 90 ))
until docker exec "$PG_C" pg_isready -U cx -d cx >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "PostgreSQL did not become ready"
  sleep 1
done
until docker run --rm --network "$NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$MINIO_USER' '$MINIO_SECRET' >/dev/null && mc ready local >/dev/null" \
  >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "MinIO did not become ready"
  sleep 1
done
docker run --rm --network "$NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$MINIO_USER' '$MINIO_SECRET' >/dev/null && mc mb --ignore-existing local/cx-jobs >/dev/null"

DATABASE="postgres://cx:${PG_PW}@127.0.0.1:${PG_PORT}/cx?sslmode=disable"
export MERC_TEST_DATABASE_URL="$DATABASE"
export MERC_TEST_S3_ENDPOINT="http://127.0.0.1:${MINIO_PORT}"
export MERC_TEST_S3_BUCKET="cx-jobs"
export MERC_TEST_S3_ACCESS_KEY="$MINIO_USER"
export MERC_TEST_S3_SECRET_KEY="$MINIO_SECRET"
export S3_ENDPOINT="$MERC_TEST_S3_ENDPOINT"
export S3_BUCKET="$MERC_TEST_S3_BUCKET"
export S3_ACCESS_KEY="$MERC_TEST_S3_ACCESS_KEY"
export S3_SECRET_KEY="$MERC_TEST_S3_SECRET_KEY"
export MERC_RECOVERY_SUITE=1
export MERC_RECOVERY_PG_CONTAINER="$PG_C"
export MERC_RECOVERY_MINIO_CONTAINER="$MINIO_C"
unset MERC_ALLOW_SKIPPING_DB_TESTS || true

log "running recovery-lane Go tests"
set +e
(
  cd "$ROOT/control"
  go test -count=1 -timeout 15m -json \
    -run '^(TestRecoveryLaneProcessRestart|TestRecoveryLaneControlPlaneRestartUnderLoad|TestRecoveryLanePostgresRestart|TestRecoveryLaneObjectStoreRestart|TestRecoveryLaneNetworkInterruption|TestRecoveryLaneStaleWorkerExpiry|TestRecoveryLaneInterruptedExecution|TestRecoveryLaneDuplicateStripeEvent|TestRecoveryLanePartialSettlement|TestRecoveryLaneRollbackAndForward)$' \
    .
) >"$EVENTS"
go_exit=$?
set -e
log "go test exit=$go_exit"

log "running restore-drill (production-shaped backup/restore)"
set +e
MERC_RESTORE_DRILL_EVIDENCE="$ROOT/evidence/autonomous/restore-drill.json" \
  MERC_CORRUPT_BACKUP_EVIDENCE="$ROOT/evidence/autonomous/corrupt-backup-refused.json" \
  bash "$ROOT/scripts/restore-drill.sh"
restore_exit=$?
set -e
log "restore-drill exit=$restore_exit"

log "running local-independent-restore (encrypted, isolated credentials)"
set +e
bash "$ROOT/scripts/local-independent-restore.sh"
independent_exit=$?
set -e
log "local-independent-restore exit=$independent_exit"

envelope_ok=false
if command -v age >/dev/null 2>&1 && command -v age-keygen >/dev/null 2>&1; then
  log "running backup envelope round-trip"
  if bash "$ROOT/scripts/test-backup-envelope.sh"; then
    envelope_ok=true
  fi
else
  log "age not installed; envelope round-trip not run"
fi

log "running backup-age metric (timestamp injection, not a 26h wait)"
set +e
# The age-metric harness parses a specific go-test line. Drop recovery-suite
# env so it cannot pick up peer-container tests or a dying throwaway DSN.
env -u MERC_RECOVERY_SUITE -u MERC_RECOVERY_PG_CONTAINER -u MERC_RECOVERY_MINIO_CONTAINER \
  -u MERC_TEST_DATABASE_URL -u MERC_TEST_S3_ENDPOINT -u MERC_TEST_S3_BUCKET \
  -u MERC_TEST_S3_ACCESS_KEY -u MERC_TEST_S3_SECRET_KEY \
  bash "$ROOT/scripts/test-backup-age-metric.sh"
age_metric_exit=$?
set -e
log "backup-age-metric exit=$age_metric_exit"

suite_exit=0
python3 "$ROOT/scripts/derive-recovery-receipts.py" \
  "$EVENTS" "$ROOT/evidence/recovery" \
  "$ROOT/evidence/autonomous/restore-drill.json" \
  "$ROOT/evidence/autonomous/logical-independent-restore.json" \
  "$envelope_ok" "$go_exit" || suite_exit=$?
if [ "$restore_exit" -ne 0 ] || [ "$independent_exit" -ne 0 ] || [ "$age_metric_exit" -ne 0 ]; then
  suite_exit=1
fi
if [ "$suite_exit" -eq 0 ]; then
  cp "$ROOT/evidence/recovery/soak-requirement-derivation.json" \
    "$ROOT/evidence/autonomous/soak-requirement-derivation.json"
fi

if [ "$suite_exit" -ne 0 ]; then
  die "recovery suite failed (go=$go_exit restore=$restore_exit independent=$independent_exit age_metric=$age_metric_exit derive=$suite_exit)"
fi
log "PASS recovery suite; receipts under evidence/recovery/"
