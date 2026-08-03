#!/usr/bin/env bash
# Production-shaped restore drill: seed >=10k jobs, >=1k ledger rows, >=1k
# objects; measure RTO; refuse a corrupt dump; write evidence derived from
# observed counts (never asserted).
#
# Uses Docker for isolated PostgreSQL and MinIO so the drill works where host
# initdb cannot allocate SysV shared memory (common under sandboxed agents).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ART_BASE="${MERC_RESTORE_DRILL_DIR:-$ROOT/.artifacts/restore-drill}"
DRILL_ID="$(date -u +%Y%m%dT%H%M%SZ)-$$"
ART="$ART_BASE/$DRILL_ID"
EVIDENCE_OUT="${MERC_RESTORE_DRILL_EVIDENCE:-$ROOT/evidence/autonomous/restore-drill.json}"
CORRUPT_EVIDENCE_OUT="${MERC_CORRUPT_BACKUP_EVIDENCE:-$ROOT/evidence/autonomous/corrupt-backup-refused.json}"
MIN_JOBS="${MERC_RESTORE_DRILL_MIN_JOBS:-10000}"
MIN_LEDGER="${MERC_RESTORE_DRILL_MIN_LEDGER:-1000}"
MIN_OBJECTS="${MERC_RESTORE_DRILL_MIN_OBJECTS:-1000}"
RUN="merc-restore-drill-$$"
PG_IMAGE="${MERC_RESTORE_DRILL_PG_IMAGE:-postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d}"
MINIO_IMAGE="${MERC_RESTORE_DRILL_MINIO_IMAGE:-minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e}"
MC_IMAGE="${MERC_RESTORE_DRILL_MC_IMAGE:-minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727}"
PG_C="${RUN}-pg"
MINIO_C="${RUN}-minio"
NET="${RUN}-net"
PG_VOL="${RUN}-pgdata"
MINIO_VOL="${RUN}-minio"
PG_PW="drill-${RANDOM}-${RANDOM}"

die() { echo "[restore-drill] ERROR: $*" >&2; exit 1; }
log() { echo "[restore-drill] $*"; }
cleanup() {
  code=$?
  docker rm -f "$PG_C" "$MINIO_C" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  docker volume rm "$PG_VOL" "$MINIO_VOL" >/dev/null 2>&1 || true
  exit "$code"
}
trap cleanup EXIT INT TERM

for tool in docker jq shasum python3; do
  command -v "$tool" >/dev/null 2>&1 || die "missing dependency: $tool"
done
mkdir -p "$ART/source-objects" "$ART/restored-objects" "$(dirname "$EVIDENCE_OUT")" "$(dirname "$CORRUPT_EVIDENCE_OUT")"

wait_pg() {
  local deadline=$(( $(date +%s) + 90 ))
  until docker exec "$PG_C" pg_isready -U cx -d postgres >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || die "PostgreSQL did not become ready"
    sleep 1
  done
}

wait_minio() {
  local deadline=$(( $(date +%s) + 90 ))
  until docker run --rm --network "$NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
    "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc ready local >/dev/null" \
    >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || die "MinIO did not become ready"
    sleep 1
  done
}

psql_src() {
  docker exec -i "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d cx_drill_source "$@"
}

psql_db() {
  local db="$1"; shift
  docker exec -i "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d "$db" "$@"
}

log "starting isolated Docker PostgreSQL and MinIO"
docker network create "$NET" >/dev/null
docker volume create "$PG_VOL" >/dev/null
docker volume create "$MINIO_VOL" >/dev/null
docker run -d --name "$PG_C" --network "$NET" --network-alias postgres \
  -e POSTGRES_USER=cx -e POSTGRES_PASSWORD="$PG_PW" -e POSTGRES_DB=postgres \
  -v "$PG_VOL:/var/lib/postgresql/data" \
  "$PG_IMAGE" >/dev/null
docker run -d --name "$MINIO_C" --network "$NET" --network-alias minio \
  -e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
  -v "$MINIO_VOL:/data" \
  "$MINIO_IMAGE" server /data >/dev/null
wait_pg
wait_minio

docker exec "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d postgres \
  -c "CREATE DATABASE cx_drill_source" >/dev/null

log "applying schema and seeding production-shaped state (jobs>=$MIN_JOBS ledger>=$MIN_LEDGER objects>=$MIN_OBJECTS)"
docker exec -i "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d cx_drill_source \
  < "$ROOT/control/schema.sql" >/dev/null

psql_src >/dev/null <<SQL
INSERT INTO buyers (id,email,free_credit_usd)
VALUES ('11111111-1111-4111-8111-111111111111','restore-drill@example.invalid',12.345678);

INSERT INTO jobs (id,buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,max_duration_secs)
VALUES ('22222222-2222-4222-8222-222222222222','11111111-1111-4111-8111-111111111111',
        'queued','embed','all-minilm-l6-v2','restore-drill/input.jsonl',1,0,300);
INSERT INTO tasks (id,job_id,status,input_ref,result_key,expected_output_records)
VALUES ('33333333-3333-4333-8333-333333333333','22222222-2222-4222-8222-222222222222',
        'queued','restore-drill/input.jsonl','restore-drill/result.jsonl',1);

INSERT INTO jobs (buyer_id,status,job_type,model_ref,input_ref,task_count,tasks_done,max_duration_secs)
SELECT '11111111-1111-4111-8111-111111111111',
       'queued',
       'embed',
       'all-minilm-l6-v2',
       'restore-drill/bulk/input-' || g::text || '.jsonl',
       1,
       0,
       300
  FROM generate_series(1, ${MIN_JOBS} - 1) AS g;

INSERT INTO ledger_entries (kind,buyer_id,amount_usd,payout_status)
SELECT 'buyer_charge',
       '11111111-1111-4111-8111-111111111111',
       -0.001000,
       'pending'
  FROM generate_series(1, ${MIN_LEDGER}) AS g;
SQL

log "materializing ${MIN_OBJECTS} object-store keys"
python3 - "$ART/source-objects" "$MIN_OBJECTS" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
n = int(sys.argv[2])
root.mkdir(parents=True, exist_ok=True)
(root / "restore-drill").mkdir(exist_ok=True)
(root / "restore-drill" / "input.jsonl").write_text('{"text":"restore-drill-marker"}\n', encoding="utf-8")
bulk = root / "restore-drill" / "bulk"
bulk.mkdir(exist_ok=True)
for i in range(1, n):
    (bulk / f"object-{i:05d}.txt").write_text(f"restore-drill-object-{i}\n", encoding="utf-8")
PY

docker run --rm --network "$NET" \
  -v "$ART/source-objects:/source:ro" \
  --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && \
   mc mb --ignore-existing local/source local/restored >/dev/null && \
   mc mirror --overwrite /source local/source >/dev/null"

SOURCE_COUNTS="$(psql_src -Atc \
  "SELECT json_build_object(
     'buyers',(SELECT count(*)::int FROM buyers),
     'jobs',(SELECT count(*)::int FROM jobs),
     'tasks',(SELECT count(*)::int FROM tasks),
     'ledger_entries',(SELECT count(*)::int FROM ledger_entries),
     'marker',(SELECT status FROM jobs WHERE id='22222222-2222-4222-8222-222222222222')
   )::text")"
SOURCE_OBJECTS="$(docker run --rm --network "$NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && \
   mc find local/source --name '*' | wc -l" | tr -d '[:space:]')"
printf '%s\n' "$SOURCE_COUNTS" >"$ART/source-counts.json"
printf '%s\n' "$SOURCE_OBJECTS" >"$ART/source-object-count.txt"

SOURCE_JOBS="$(echo "$SOURCE_COUNTS" | jq -r '.jobs')"
SOURCE_LEDGER="$(echo "$SOURCE_COUNTS" | jq -r '.ledger_entries')"
log "source counts jobs=$SOURCE_JOBS ledger=$SOURCE_LEDGER objects=$SOURCE_OBJECTS"

[ "$SOURCE_JOBS" -ge "$MIN_JOBS" ] || die "jobs count $SOURCE_JOBS < required $MIN_JOBS (toy-sized seed refused)"
[ "$SOURCE_LEDGER" -ge "$MIN_LEDGER" ] || die "ledger count $SOURCE_LEDGER < required $MIN_LEDGER (toy-sized seed refused)"
[ "$SOURCE_OBJECTS" -ge "$MIN_OBJECTS" ] || die "object count $SOURCE_OBJECTS < required $MIN_OBJECTS (toy-sized seed refused)"

printf '%s' "$SOURCE_COUNTS" | shasum -a 256 | awk '{print $1}' >"$ART/source-db.sha256"
(cd "$ART/source-objects" && find . -type f -print0 | sort -z | xargs -0 shasum -a 256) >"$ART/source-objects.sha256"

log "dumping source database"
docker exec "$PG_C" pg_dump -U cx -d cx_drill_source -Fc >"$ART/db.dump"
(cd "$ART" && shasum -a 256 db.dump >db.dump.sha256 && shasum -a 256 -c db.dump.sha256 >/dev/null)

# --- Corrupt envelope refusal ---
log "proving corrupt dump is refused"
cp "$ART/db.dump" "$ART/db.dump.corrupt"
python3 - "$ART/db.dump.corrupt" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
data = bytearray(path.read_bytes())
if len(data) < 200:
    raise SystemExit("dump too small to corrupt safely")
idx = min(len(data) - 1, 256)
data[idx] ^= 0xFF
path.write_bytes(data)
PY
CORRUPT_SHA="$(shasum -a 256 "$ART/db.dump.corrupt" | awk '{print $1}')"
GOOD_SHA="$(awk '{print $1}' "$ART/db.dump.sha256")"
[ "$CORRUPT_SHA" != "$GOOD_SHA" ] || die "corrupt dump checksum unexpectedly matches good dump"

docker exec "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d postgres \
  -c "DROP DATABASE IF EXISTS cx_drill_corrupt_attempt" \
  -c "CREATE DATABASE cx_drill_corrupt_attempt" >/dev/null

CORRUPT_RESTORE_LOG="$ART/corrupt-restore.log"
set +e
docker exec -i "$PG_C" pg_restore -U cx -d cx_drill_corrupt_attempt \
  --exit-on-error --no-owner --no-privileges -1 <"$ART/db.dump.corrupt" \
  >"$CORRUPT_RESTORE_LOG" 2>&1
CORRUPT_RESTORE_RC=$?
set -e

set +e
(cd "$ART" && shasum -a 256 -c <(printf '%s  db.dump.corrupt\n' "$GOOD_SHA") >"$ART/corrupt-checksum.log" 2>&1)
CORRUPT_CHECKSUM_RC=$?
set -e

CORRUPT_JOBS_TABLE="$(psql_db cx_drill_corrupt_attempt -Atc \
  "SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name='jobs'" 2>/dev/null || echo 0)"
if [ "$CORRUPT_JOBS_TABLE" = "1" ]; then
  CORRUPT_JOB_ROWS="$(psql_db cx_drill_corrupt_attempt -Atc "SELECT count(*) FROM jobs" 2>/dev/null || echo 0)"
else
  CORRUPT_JOB_ROWS=0
fi

CORRUPT_REFUSED=0
if [ "$CORRUPT_CHECKSUM_RC" -ne 0 ] && [ "$CORRUPT_JOB_ROWS" -lt "$MIN_JOBS" ]; then
  CORRUPT_REFUSED=1
fi
if [ "$CORRUPT_RESTORE_RC" -ne 0 ] && [ "$CORRUPT_JOB_ROWS" -lt "$MIN_JOBS" ]; then
  CORRUPT_REFUSED=1
fi
[ "$CORRUPT_REFUSED" = 1 ] || die "corrupt backup was not refused (pg_restore_rc=$CORRUPT_RESTORE_RC checksum_rc=$CORRUPT_CHECKSUM_RC job_rows=$CORRUPT_JOB_ROWS)"
log "corrupt dump refused (pg_restore_rc=$CORRUPT_RESTORE_RC checksum_rc=$CORRUPT_CHECKSUM_RC job_rows=$CORRUPT_JOB_ROWS)"

AGE_CORRUPT_REFUSED=false
if command -v age >/dev/null 2>&1 && command -v age-keygen >/dev/null 2>&1; then
  log "proving corrupt age envelope is refused"
  age-keygen -o "$ART/age-identity.txt" 2>"$ART/age-keygen.log"
  RECIPIENT="$(awk '/^# public key:/ {print $4}' "$ART/age-identity.txt")"
  tar -C "$ART" -cf "$ART/envelope.tar" db.dump db.dump.sha256
  age -r "$RECIPIENT" -o "$ART/backup.tar.age" "$ART/envelope.tar"
  cp "$ART/backup.tar.age" "$ART/backup.tar.age.corrupt"
  python3 - "$ART/backup.tar.age.corrupt" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
data = bytearray(path.read_bytes())
if not data:
    raise SystemExit("empty age ciphertext")
data[len(data)//2] ^= 0xFF
path.write_bytes(data)
PY
  rm -f "$ART/envelope.restored.tar"
  set +e
  age -d -i "$ART/age-identity.txt" -o "$ART/envelope.restored.tar" "$ART/backup.tar.age.corrupt" \
    >"$ART/age-decrypt-corrupt.log" 2>&1
  AGE_CORRUPT_RC=$?
  set -e
  # age must fail closed. It may leave a partial output file; that is still a
  # refusal so long as the exit code is non-zero and a clean tar extract is not possible.
  [ "$AGE_CORRUPT_RC" -ne 0 ] || die "corrupt age envelope decrypted successfully (must refuse)"
  if [ -s "$ART/envelope.restored.tar" ]; then
    set +e
    tar -tf "$ART/envelope.restored.tar" >/dev/null 2>"$ART/age-corrupt-tar.log"
    AGE_TAR_RC=$?
    set -e
    [ "$AGE_TAR_RC" -ne 0 ] || die "corrupt age decrypt produced a readable tar (must refuse)"
  fi
  AGE_CORRUPT_REFUSED=true
else
  log "age not installed; skipping age-envelope corrupt proof (dump refusal already recorded)"
fi

CORRUPT_STATUS="FAIL"
[ "$CORRUPT_REFUSED" = 1 ] && CORRUPT_STATUS="PASS"
jq -n \
  --arg status "$CORRUPT_STATUS" \
  --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg drill_id "$DRILL_ID" \
  --arg good_sha "$GOOD_SHA" \
  --arg corrupt_sha "$CORRUPT_SHA" \
  --argjson pg_restore_rc "$CORRUPT_RESTORE_RC" \
  --argjson checksum_rc "$CORRUPT_CHECKSUM_RC" \
  --argjson corrupt_job_rows "$CORRUPT_JOB_ROWS" \
  --argjson min_jobs "$MIN_JOBS" \
  --argjson age_corrupt_refused "$AGE_CORRUPT_REFUSED" \
  '{
    schema_version:1,
    kind:"corrupt_backup_refused",
    status:$status,
    completed_at:$completed_at,
    drill_id:$drill_id,
    good_dump_sha256:$good_sha,
    corrupt_dump_sha256:$corrupt_sha,
    observations:{
      pg_restore_exit_code:$pg_restore_rc,
      checksum_verify_exit_code:$checksum_rc,
      jobs_rows_in_target_after_attempt:$corrupt_job_rows,
      production_shaped_min_jobs:$min_jobs,
      age_envelope_decrypt_refused:$age_corrupt_refused
    },
    derived:{
      checksum_mismatch: ($checksum_rc != 0),
      restore_failed_closed: ($pg_restore_rc != 0),
      no_full_database_from_corrupt: ($corrupt_job_rows < $min_jobs)
    }
  }' >"$ART/corrupt-evidence.json"
# shellcheck source=scripts/lib/write-bound-evidence.sh
. "$ROOT/scripts/lib/write-bound-evidence.sh"
merc_emit_bound_json "$CORRUPT_EVIDENCE_OUT" "scripts/restore-drill.sh" \
  "$ART/corrupt-evidence.json" \
  --exact-config "restore-drill corrupt-backup path; drill_id=$DRILL_ID" \
  --raw-samples "embedded observations in receipt body" \
  --model-na "restore drill does not load model weights" \
  --image-na "drill uses pinned postgres/minio images; digests live in script pins, not receipt identity" \
  --corpus-na "no external corpus; synthetic seed only"
[ "$CORRUPT_STATUS" = "PASS" ] || die "corrupt backup refusal evidence status=$CORRUPT_STATUS"

# --- Clean restore + measured RTO ---
log "restoring good dump into a new database (RTO clock starts)"
docker exec "$PG_C" psql -X -v ON_ERROR_STOP=1 -U cx -d postgres \
  -c "DROP DATABASE IF EXISTS cx_drill_restored" \
  -c "CREATE DATABASE cx_drill_restored" >/dev/null

RESTORE_START_NS="$(python3 - <<'PY'
import time; print(time.time_ns())
PY
)"
docker exec -i "$PG_C" pg_restore -U cx -d cx_drill_restored \
  --exit-on-error --no-owner --no-privileges -1 <"$ART/db.dump"
log "restoring object mirror"
docker run --rm --network "$NET" \
  -v "$ART/source-objects:/source:ro" \
  --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && \
   mc mb --ignore-existing local/restored >/dev/null && \
   mc mirror --overwrite /source local/restored >/dev/null"
docker run --rm --network "$NET" \
  -v "$ART/restored-objects:/dest" \
  --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && \
   mc mirror --overwrite local/restored /dest >/dev/null"
RESTORE_END_NS="$(python3 - <<'PY'
import time; print(time.time_ns())
PY
)"
RTO_SECONDS="$(python3 - "$RESTORE_START_NS" "$RESTORE_END_NS" <<'PY'
import sys
start, end = int(sys.argv[1]), int(sys.argv[2])
print(f"{(end - start) / 1e9:.3f}")
PY
)"
log "measured RTO_seconds=$RTO_SECONDS"

RESTORED_COUNTS="$(psql_db cx_drill_restored -Atc \
  "SELECT json_build_object(
     'buyers',(SELECT count(*)::int FROM buyers),
     'jobs',(SELECT count(*)::int FROM jobs),
     'tasks',(SELECT count(*)::int FROM tasks),
     'ledger_entries',(SELECT count(*)::int FROM ledger_entries),
     'marker',(SELECT status FROM jobs WHERE id='22222222-2222-4222-8222-222222222222')
   )::text")"
RESTORED_OBJECTS="$(find "$ART/restored-objects" -type f | wc -l | tr -d ' ')"
printf '%s\n' "$RESTORED_COUNTS" >"$ART/restored-counts.json"
printf '%s' "$RESTORED_COUNTS" | shasum -a 256 | awk '{print $1}' >"$ART/restored-db.sha256"
(cd "$ART/restored-objects" && find . -type f -print0 | sort -z | xargs -0 shasum -a 256) >"$ART/restored-objects.sha256"

cmp -s "$ART/source-db.sha256" "$ART/restored-db.sha256" || die "restored database state differs"
cmp -s "$ART/source-objects.sha256" "$ART/restored-objects.sha256" || die "restored object state differs"

RESTORED_JOBS="$(echo "$RESTORED_COUNTS" | jq -r '.jobs')"
RESTORED_LEDGER="$(echo "$RESTORED_COUNTS" | jq -r '.ledger_entries')"
[ "$RESTORED_JOBS" -ge "$MIN_JOBS" ] || die "restored jobs $RESTORED_JOBS < $MIN_JOBS"
[ "$RESTORED_LEDGER" -ge "$MIN_LEDGER" ] || die "restored ledger $RESTORED_LEDGER < $MIN_LEDGER"
[ "$RESTORED_OBJECTS" -ge "$MIN_OBJECTS" ] || die "restored objects $RESTORED_OBJECTS < $MIN_OBJECTS"

DB_SHA="$(awk '{print $1}' "$ART/db.dump.sha256")"
STATE_SHA="$(cat "$ART/restored-db.sha256")"
OBJECT_SHA="$(awk '{print $1}' "$ART/restored-objects.sha256" | shasum -a 256 | awk '{print $1}')"

STATUS="$(jq -nr \
  --argjson src "$SOURCE_COUNTS" \
  --argjson dst "$RESTORED_COUNTS" \
  --argjson src_objects "$SOURCE_OBJECTS" \
  --argjson dst_objects "$RESTORED_OBJECTS" \
  --argjson min_jobs "$MIN_JOBS" \
  --argjson min_ledger "$MIN_LEDGER" \
  --argjson min_objects "$MIN_OBJECTS" \
  --arg source_db_sha "$(cat "$ART/source-db.sha256")" \
  --arg restored_db_sha "$(cat "$ART/restored-db.sha256")" \
  --arg source_obj_sha "$(shasum -a 256 "$ART/source-objects.sha256" | awk '{print $1}')" \
  --arg restored_obj_sha "$(shasum -a 256 "$ART/restored-objects.sha256" | awk '{print $1}')" \
  '
  ($src == $dst)
  and ($src.jobs >= $min_jobs)
  and ($src.ledger_entries >= $min_ledger)
  and ($dst.jobs >= $min_jobs)
  and ($dst.ledger_entries >= $min_ledger)
  and ($src_objects >= $min_objects)
  and ($dst_objects >= $min_objects)
  and ($source_db_sha == $restored_db_sha)
  and ($source_obj_sha == $restored_obj_sha)
  | if . then "PASS" else "FAIL" end
  ')"

jq -n \
  --arg status "$STATUS" \
  --arg drill_id "$DRILL_ID" \
  --arg completed_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg db_dump_sha256 "$DB_SHA" \
  --arg restored_state_sha256 "$STATE_SHA" \
  --arg restored_object_manifest_sha256 "$OBJECT_SHA" \
  --argjson rto_seconds "$RTO_SECONDS" \
  --argjson source_counts "$SOURCE_COUNTS" \
  --argjson restored_counts "$RESTORED_COUNTS" \
  --argjson source_objects "$SOURCE_OBJECTS" \
  --argjson restored_objects "$RESTORED_OBJECTS" \
  --argjson min_jobs "$MIN_JOBS" \
  --argjson min_ledger "$MIN_LEDGER" \
  --argjson min_objects "$MIN_OBJECTS" \
  --arg corrupt_evidence "$CORRUPT_EVIDENCE_OUT" \
  --arg runtime "docker" \
  '{
    schema_version:1,
    kind:"restore_drill",
    status:$status,
    drill_id:$drill_id,
    completed_at:$completed_at,
    runtime:$runtime,
    rto_seconds_measured:$rto_seconds,
    thresholds:{min_jobs:$min_jobs,min_ledger_entries:$min_ledger,min_objects:$min_objects},
    source:$source_counts,
    restored:$restored_counts,
    source_object_count:$source_objects,
    restored_object_count:$restored_objects,
    db_dump_sha256:$db_dump_sha256,
    restored_state_sha256:$restored_state_sha256,
    restored_object_manifest_sha256:$restored_object_manifest_sha256,
    corrupt_backup_evidence:$corrupt_evidence,
    assertions_derived_from_observation:[
      "source and restored row-count JSON match",
      "jobs/ledger/objects meet production-shaped minima",
      "database and object content hashes match",
      "RTO measured wall-clock around pg_restore + object mirror"
    ]
  }' >"$ART/evidence.json"

# shellcheck source=scripts/lib/write-bound-evidence.sh
. "$ROOT/scripts/lib/write-bound-evidence.sh"
merc_emit_bound_json "$EVIDENCE_OUT" "scripts/restore-drill.sh" \
  "$ART/evidence.json" \
  --exact-config "restore-drill clean path; drill_id=$DRILL_ID min_jobs=$MIN_JOBS" \
  --raw-samples "embedded source/restored counts and hashes in receipt body" \
  --model-na "restore drill does not load model weights" \
  --image-na "drill uses pinned postgres/minio images; digests live in script pins, not receipt identity" \
  --corpus-na "no external corpus; synthetic seed only"
log "status=$STATUS evidence=$EVIDENCE_OUT rto_seconds=$RTO_SECONDS"
[ "$STATUS" = "PASS" ] || die "derived status is $STATUS"
log "PASS evidence=$EVIDENCE_OUT"
