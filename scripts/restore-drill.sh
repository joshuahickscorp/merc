#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ART_BASE="${CX_RESTORE_DRILL_DIR:-$ROOT/.artifacts/restore-drill}"
DRILL_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"
ART="$ART_BASE/$DRILL_ID"
PGPORT="${CX_RESTORE_DRILL_PGPORT:-56432}"
MINIO_PORT="${CX_RESTORE_DRILL_MINIO_PORT:-59100}"
RUNTIME="$(mktemp -d "${TMPDIR:-/tmp}/cx-restore-drill.XXXXXX")"
PGDATA="$RUNTIME/pgdata"
MINIO_PID=""
PG_STARTED=0

die() { echo "[restore-drill] ERROR: $*" >&2; exit 1; }
log() { echo "[restore-drill] $*"; }
cleanup() {
  code=$?
  [ -z "$MINIO_PID" ] || kill "$MINIO_PID" 2>/dev/null || true
  [ "$PG_STARTED" = 0 ] || pg_ctl -D "$PGDATA" -m fast stop >/dev/null 2>&1 || true
  case "$RUNTIME" in "${TMPDIR:-/tmp}"/cx-restore-drill.*) rm -rf "$RUNTIME" ;; esac
  exit "$code"
}
trap cleanup EXIT INT TERM

for tool in initdb pg_ctl createdb dropdb pg_dump pg_restore psql minio mc jq shasum; do
  command -v "$tool" >/dev/null 2>&1 || die "missing dependency: $tool"
done
mkdir -p "$ART/source-objects" "$ART/restored-objects"

log "starting isolated PostgreSQL and MinIO"
initdb -D "$PGDATA" -U cx --auth=trust -E UTF8 --locale=C >"$ART/postgres.log"
pg_ctl -D "$PGDATA" -o "-p $PGPORT -c listen_addresses=127.0.0.1" \
  -l "$ART/postgres.log" -w start
PG_STARTED=1
createdb -h 127.0.0.1 -p "$PGPORT" -U cx cx_drill_source
DATABASE_URL="postgres://cx@127.0.0.1:$PGPORT/cx_drill_source?sslmode=disable"

MINIO_ROOT_USER=minioadmin MINIO_ROOT_PASSWORD=minioadmin \
  minio server "$RUNTIME/minio" --address "127.0.0.1:$MINIO_PORT" >"$ART/minio.log" 2>&1 &
MINIO_PID=$!
for _ in $(seq 1 30); do
  mc alias set cx-restore-drill "http://127.0.0.1:$MINIO_PORT" minioadmin minioadmin >/dev/null 2>&1 && break
  sleep 1
done
mc alias set cx-restore-drill "http://127.0.0.1:$MINIO_PORT" minioadmin minioadmin >/dev/null
mc mb --ignore-existing cx-restore-drill/source cx-restore-drill/restored >/dev/null

log "creating representative control-plane and artifact state"
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 --single-transaction -f "$ROOT/control/schema.sql" >/dev/null
psql "$DATABASE_URL" -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
INSERT INTO buyers (id,email,free_credit_usd)
VALUES ('11111111-1111-4111-8111-111111111111','restore-drill@example.invalid',12.345678);
INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,max_duration_secs)
VALUES ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111',
        'queued','embed','all-minilm-l6-v2','restore-drill/input.jsonl',1,0,300);
INSERT INTO tasks (id,job_id,status,input_ref,result_key,expected_output_records)
VALUES ('33333333-3333-4333-8333-333333333333','22222222-2222-4222-8222-222222222222',
        'queued','restore-drill/input.jsonl','restore-drill/result.jsonl',1);
SQL
printf '%s\n' '{"text":"restore-drill-marker"}' | \
  mc pipe cx-restore-drill/source/restore-drill/input.jsonl >/dev/null

SOURCE_STATE="$(psql "$DATABASE_URL" -Atc \
  "SELECT json_build_object('buyers',(SELECT count(*) FROM buyers),'jobs',(SELECT count(*) FROM jobs),'tasks',(SELECT count(*) FROM tasks),'marker',(SELECT status FROM jobs WHERE id='22222222-2222-4222-8222-222222222222'))::text")"
printf '%s' "$SOURCE_STATE" | shasum -a 256 | awk '{print $1}' >"$ART/source-db.sha256"
mc mirror --overwrite cx-restore-drill/source "$ART/source-objects" >/dev/null
(cd "$ART/source-objects" && find . -type f -print0 | sort -z | xargs -0 shasum -a 256) >"$ART/source-objects.sha256"

log "dumping, checksumming, and restoring into a new database"
pg_dump "$DATABASE_URL" -Fc >"$ART/db.dump"
(cd "$ART" && shasum -a 256 db.dump >db.dump.sha256 && shasum -a 256 -c db.dump.sha256 >/dev/null)
createdb -h 127.0.0.1 -p "$PGPORT" -U cx cx_drill_restored
pg_restore -d "postgres://cx@127.0.0.1:$PGPORT/cx_drill_restored?sslmode=disable" \
  --exit-on-error --no-owner --no-privileges -1 "$ART/db.dump"
RESTORED_STATE="$(psql "postgres://cx@127.0.0.1:$PGPORT/cx_drill_restored?sslmode=disable" -Atc \
  "SELECT json_build_object('buyers',(SELECT count(*) FROM buyers),'jobs',(SELECT count(*) FROM jobs),'tasks',(SELECT count(*) FROM tasks),'marker',(SELECT status FROM jobs WHERE id='22222222-2222-4222-8222-222222222222'))::text")"
printf '%s' "$RESTORED_STATE" | shasum -a 256 | awk '{print $1}' >"$ART/restored-db.sha256"
cmp -s "$ART/source-db.sha256" "$ART/restored-db.sha256" || die "restored database state differs"

log "restoring object mirror and comparing content hashes"
mc mirror --overwrite "$ART/source-objects" cx-restore-drill/restored >/dev/null
mc mirror --overwrite cx-restore-drill/restored "$ART/restored-objects" >/dev/null
(cd "$ART/restored-objects" && find . -type f -print0 | sort -z | xargs -0 shasum -a 256) >"$ART/restored-objects.sha256"
cmp -s "$ART/source-objects.sha256" "$ART/restored-objects.sha256" || die "restored object state differs"

DB_SHA="$(awk '{print $1}' "$ART/db.dump.sha256")"
STATE_SHA="$(cat "$ART/restored-db.sha256")"
OBJECT_SHA="$(awk '{print $1}' "$ART/restored-objects.sha256" | shasum -a 256 | awk '{print $1}')"
jq -n \
  --arg status PASS --arg drill_id "$DRILL_ID" --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg db_dump_sha256 "$DB_SHA" --arg restored_state_sha256 "$STATE_SHA" \
  --arg restored_object_manifest_sha256 "$OBJECT_SHA" \
  '{status:$status,drill_id:$drill_id,completed_at:$completed_at,db_dump_sha256:$db_dump_sha256,
    restored_state_sha256:$restored_state_sha256,restored_object_manifest_sha256:$restored_object_manifest_sha256,
    assertions:["transactional PostgreSQL restore completed","representative row counts and lifecycle marker match","artifact mirror content hashes match"]}' \
  >"$ART/evidence.json"
log "PASS evidence=$ART/evidence.json"
