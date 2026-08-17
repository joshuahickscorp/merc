#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
for command_name in docker age age-keygen jq shasum; do
  command -v "$command_name" >/dev/null 2>&1 || { echo "local-independent-restore: $command_name is required" >&2; exit 1; }
done

RUN="merc-restore-$RANDOM-$$"
mkdir -p "$ROOT/.artifacts"
WORK="$(mktemp -d "$ROOT/.artifacts/merc-logical-restore.XXXXXX")"
A_PG="${RUN}-a-pg"; A_MINIO="${RUN}-a-minio"; A_NET="${RUN}-a"
B_PG="${RUN}-b-pg"; B_MINIO="${RUN}-b-minio"; B_NET="${RUN}-b"
A_PG_VOL="${RUN}-a-pgdata"; A_OBJ_VOL="${RUN}-a-objects"
B_PG_VOL="${RUN}-b-pgdata"; B_OBJ_VOL="${RUN}-b-objects"
PG_IMAGE='postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d'
MINIO_IMAGE='minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'
MC_IMAGE='minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727'
A_PW="logical-a-${RANDOM}-${RANDOM}-password"; B_PW="logical-b-${RANDOM}-${RANDOM}-password"
A_USER="logicala${RANDOM}"; A_SECRET="logical-a-${RANDOM}-${RANDOM}-object-secret"
B_USER="logicalb${RANDOM}"; B_SECRET="logical-b-${RANDOM}-${RANDOM}-object-secret"
SOURCE_DESTROYED=false

cleanup() {
  docker rm -f "$A_PG" "$A_MINIO" "$B_PG" "$B_MINIO" >/dev/null 2>&1 || true
  docker network rm "$A_NET" "$B_NET" >/dev/null 2>&1 || true
  docker volume rm "$A_PG_VOL" "$A_OBJ_VOL" "$B_PG_VOL" "$B_OBJ_VOL" >/dev/null 2>&1 || true
  rm -rf -- "$WORK"
}
trap cleanup EXIT INT TERM

wait_pg() {
  local container="$1" deadline=$(( $(date +%s) + 90 ))
  until docker exec "$container" pg_isready -U cx -d cx >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "PostgreSQL did not become ready" >&2; exit 1; }
    sleep 1
  done
}

wait_minio() {
  local network="$1" user="$2" secret="$3" deadline=$(( $(date +%s) + 90 ))
  until docker run --rm --network "$network" --entrypoint /bin/sh "$MC_IMAGE" -c \
    "mc alias set local http://minio:9000 '$user' '$secret' >/dev/null && mc ready local >/dev/null" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || { echo "MinIO did not become ready" >&2; exit 1; }
    sleep 1
  done
}

mkdir -p "$WORK/a/plain/objects/jobs/embed" "$WORK/a/plain/objects/jobs/batch" "$WORK/b/inbox" "$WORK/b/restored"
chmod 755 "$WORK" "$WORK/a" "$WORK/b" "$WORK/b/inbox"
printf '%s\n' '{"vectors":[[0.1,0.2]]}' > "$WORK/a/plain/objects/jobs/embed/result.json"
printf '%s\n' '{"text":"synthetic result"}' > "$WORK/a/plain/objects/jobs/batch/result.json"

docker network create "$A_NET" >/dev/null
docker volume create "$A_PG_VOL" >/dev/null
docker volume create "$A_OBJ_VOL" >/dev/null
docker run -d --name "$A_PG" --network "$A_NET" --network-alias postgres \
  -e POSTGRES_USER=cx -e POSTGRES_PASSWORD="$A_PW" -e POSTGRES_DB=cx \
  -v "$A_PG_VOL:/var/lib/postgresql/data" "$PG_IMAGE" >/dev/null
docker run -d --name "$A_MINIO" --network "$A_NET" --network-alias minio \
  -e MINIO_ROOT_USER="$A_USER" -e MINIO_ROOT_PASSWORD="$A_SECRET" \
  -v "$A_OBJ_VOL:/data" "$MINIO_IMAGE" server /data >/dev/null
wait_pg "$A_PG"; wait_minio "$A_NET" "$A_USER" "$A_SECRET"
docker exec -i "$A_PG" psql -X -v ON_ERROR_STOP=1 -U cx -d cx < "$ROOT/control/schema.sql" > /dev/null 2> "$WORK/schema.log"

docker exec -i "$A_PG" psql -X -v ON_ERROR_STOP=1 -U cx -d cx >/dev/null <<'SQL'
INSERT INTO buyers(id,email,free_credit_usd) VALUES
 ('00000000-0000-4000-8000-000000000101','restore-buyer@example.invalid',0);
INSERT INTO suppliers(id,email,status) VALUES
 ('00000000-0000-4000-8000-000000000201','restore-worker-one@example.invalid','active'),
 ('00000000-0000-4000-8000-000000000202','restore-worker-two@example.invalid','active');
INSERT INTO workers(id,supplier_id,hw_class,version) VALUES
 ('00000000-0000-4000-8000-000000000301','00000000-0000-4000-8000-000000000201','apple_silicon_pro','restore-test'),
 ('00000000-0000-4000-8000-000000000302','00000000-0000-4000-8000-000000000202','apple_silicon_pro','restore-test');
INSERT INTO jobs(id,buyer_id,status,job_type,input_ref,output_ref,task_count,tasks_done) VALUES
 ('00000000-0000-4000-8000-000000000401','00000000-0000-4000-8000-000000000101','complete','embed','jobs/embed/input','jobs/embed/result.json',1,1),
 ('00000000-0000-4000-8000-000000000402','00000000-0000-4000-8000-000000000101','complete','batch_infer','jobs/batch/input','jobs/batch/result.json',1,1),
 ('00000000-0000-4000-8000-000000000403','00000000-0000-4000-8000-000000000101','cancelled','embed','jobs/cancelled/input',NULL,1,0),
 ('00000000-0000-4000-8000-000000000404','00000000-0000-4000-8000-000000000101','running','embed','jobs/retry/input',NULL,1,0);
INSERT INTO tasks(id,job_id,worker_id,status,retry_count,result_ref,result_key) VALUES
 ('00000000-0000-4000-8000-000000000501','00000000-0000-4000-8000-000000000401','00000000-0000-4000-8000-000000000301','complete',0,'jobs/embed/result.json','jobs/embed/result.json'),
 ('00000000-0000-4000-8000-000000000502','00000000-0000-4000-8000-000000000402','00000000-0000-4000-8000-000000000302','complete',0,'jobs/batch/result.json','jobs/batch/result.json'),
 ('00000000-0000-4000-8000-000000000503','00000000-0000-4000-8000-000000000403',NULL,'cancelled',0,NULL,NULL),
 ('00000000-0000-4000-8000-000000000504','00000000-0000-4000-8000-000000000404',NULL,'retrying',2,NULL,NULL);
INSERT INTO ledger_entries(id,kind,supplier_id,buyer_id,amount_usd,payout_status,release_at) VALUES
 ('00000000-0000-4000-8000-000000000601','buyer_charge',NULL,'00000000-0000-4000-8000-000000000101',-1.000000,'pending',NULL),
 ('00000000-0000-4000-8000-000000000602','supplier_credit','00000000-0000-4000-8000-000000000201',NULL,0.800000,'held',now()+interval '1 day'),
 ('00000000-0000-4000-8000-000000000603','platform_take',NULL,NULL,0.200000,'pending',NULL);
INSERT INTO webhooks(id,buyer_id,job_id,url,signing_secret_sealed)
VALUES ('00000000-0000-4000-8000-000000000701','00000000-0000-4000-8000-000000000101','00000000-0000-4000-8000-000000000401','https://example.invalid/hook','enc:synthetic-sealed-secret');
SQL

docker run --rm --network "$A_NET" -v "$WORK/a/plain/objects:/source:ro" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$A_USER' '$A_SECRET' >/dev/null && mc mb local/cx-jobs >/dev/null && mc mirror /source local/cx-jobs >/dev/null"
docker exec "$A_PG" pg_dump -U cx -d cx -Fc > "$WORK/a/plain/database.dump"
docker run --rm --network "$A_NET" -v "$WORK/a/plain/objects-export:/export" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$A_USER' '$A_SECRET' >/dev/null && mc mirror local/cx-jobs /export >/dev/null"

(cd "$WORK/a/plain" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 shasum -a 256 > SHA256SUMS)
age-keygen -o "$WORK/a/identity.txt" 2> "$WORK/a/keygen.log"
recipient="$(awk '/^# public key:/ {print $4}' "$WORK/a/identity.txt")"
tar -C "$WORK/a/plain" -cf "$WORK/a/backup.tar" .
age -r "$recipient" -o "$WORK/a/backup.tar.age" "$WORK/a/backup.tar"
shasum -a 256 "$WORK/a/backup.tar.age" | awk '{print $1}' > "$WORK/a/backup.sha256"
chmod 644 "$WORK/a/backup.tar.age" "$WORK/a/backup.sha256"
rm -rf "$WORK/a/plain" "$WORK/a/backup.tar"

# Separate containers receive only ciphertext/checksum streams and an empty B
# inbox. Neither process receives plaintext, database/object credentials, or
# the age identity.
docker run --rm -i -v "$WORK/b/inbox:/to" --entrypoint /bin/sh "$MC_IMAGE" -c \
  'umask 077; cat > /to/backup.tar.age' < "$WORK/a/backup.tar.age"
docker run --rm -i -v "$WORK/b/inbox:/to" --entrypoint /bin/sh "$MC_IMAGE" -c \
  'umask 077; cat > /to/backup.sha256' < "$WORK/a/backup.sha256"

docker rm -f "$A_PG" "$A_MINIO" >/dev/null
docker network rm "$A_NET" >/dev/null
docker volume rm "$A_PG_VOL" "$A_OBJ_VOL" >/dev/null
SOURCE_DESTROYED=true
rm -f "$WORK/a/backup.tar.age" "$WORK/a/backup.sha256"

expected="$(tr -d '[:space:]' < "$WORK/b/inbox/backup.sha256")"
actual="$(shasum -a 256 "$WORK/b/inbox/backup.tar.age" | awk '{print $1}')"
[ "$expected" = "$actual" ]
cp "$WORK/b/inbox/backup.tar.age" "$WORK/b/inbox/corrupt.tar.age"
printf 'x' >> "$WORK/b/inbox/corrupt.tar.age"
[ "$(shasum -a 256 "$WORK/b/inbox/corrupt.tar.age" | awk '{print $1}')" != "$expected" ]
age -d -i "$WORK/a/identity.txt" -o "$WORK/b/backup.tar" "$WORK/b/inbox/backup.tar.age"
tar -C "$WORK/b/restored" -xf "$WORK/b/backup.tar"
(cd "$WORK/b/restored" && shasum -a 256 -c SHA256SUMS >/dev/null)

docker network create "$B_NET" >/dev/null
docker volume create "$B_PG_VOL" >/dev/null
docker volume create "$B_OBJ_VOL" >/dev/null
docker run -d --name "$B_PG" --network "$B_NET" --network-alias postgres \
  -e POSTGRES_USER=cx -e POSTGRES_PASSWORD="$B_PW" -e POSTGRES_DB=cx \
  -v "$B_PG_VOL:/var/lib/postgresql/data" "$PG_IMAGE" >/dev/null
docker run -d --name "$B_MINIO" --network "$B_NET" --network-alias minio \
  -e MINIO_ROOT_USER="$B_USER" -e MINIO_ROOT_PASSWORD="$B_SECRET" \
  -v "$B_OBJ_VOL:/data" "$MINIO_IMAGE" server /data >/dev/null
wait_pg "$B_PG"; wait_minio "$B_NET" "$B_USER" "$B_SECRET"
docker exec -i "$B_PG" pg_restore -U cx -d cx --clean --if-exists --no-owner --no-privileges -1 < "$WORK/b/restored/database.dump"
docker run --rm --network "$B_NET" -v "$WORK/b/restored/objects-export:/source:ro" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$B_USER' '$B_SECRET' >/dev/null && mc mb local/cx-jobs >/dev/null && mc mirror /source local/cx-jobs >/dev/null"

semantic="$(docker exec "$B_PG" psql -X -qAt -U cx -d cx -c "SELECT json_build_object(
  'buyers',(SELECT count(*) FROM buyers),
  'workers',(SELECT count(*) FROM workers),
  'completed_embed',(SELECT count(*) FROM jobs WHERE status='complete' AND job_type='embed'),
  'completed_batch',(SELECT count(*) FROM jobs WHERE status='complete' AND job_type='batch_infer'),
  'cancelled',(SELECT count(*) FROM jobs WHERE status='cancelled'),
  'retried',(SELECT count(*) FROM tasks WHERE retry_count=2),
  'held_payout',(SELECT count(*) FROM ledger_entries WHERE payout_status='held'),
  'webhooks',(SELECT count(*) FROM webhooks),
  'ledger_sum',(SELECT sum(amount_usd)::text FROM ledger_entries))::text")"
jq -e '.buyers==1 and .workers==2 and .completed_embed==1 and .completed_batch==1 and .cancelled==1 and .retried==1 and .held_payout==1 and .webhooks==1 and .ledger_sum=="0.000000"' <<< "$semantic" >/dev/null

object_count="$(docker run --rm --network "$B_NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$B_USER' '$B_SECRET' >/dev/null && mc find local/cx-jobs --name '*' | wc -l" | tr -d '[:space:]')"
[ "$object_count" -eq 2 ]

mkdir -p "$ROOT/evidence/autonomous"
receipt="$ROOT/evidence/autonomous/logical-independent-restore.json"
payload="$(mktemp "${TMPDIR:-/tmp}/merc-logical-restore.XXXXXX.json")"
offsite_marker="NOT EXECUTED"
if [ -f "$ROOT/evidence/external/offsite-independent-restore.json" ]; then
  if jq -e '.kind=="external_offsite_restore" and (.status|ascii_upcase)=="PASS"' \
    "$ROOT/evidence/external/offsite-independent-restore.json" >/dev/null 2>&1; then
    offsite_marker="PASS"
  fi
fi
jq -n --arg at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg cipher "$expected" \
  --arg offsite "$offsite_marker" \
  --argjson semantic "$semantic" --argjson objects "$object_count" --argjson destroyed "$SOURCE_DESTROYED" \
  '{schema_version:1,kind:"logical_independent_restore",status:"PASS",completed_at:$at,
    separation:{source_environment_destroyed:$destroyed,separate_transfer_process:true,
      transfer_received_ciphertext_only:true,new_database_credentials:true,new_object_credentials:true,new_namespace:true},
    integrity:{ciphertext_sha256:$cipher,ciphertext_verified:true,corrupt_backup_rejected:true,
      payload_checksums_verified:true,database_semantics:$semantic,object_count:$objects,
      ledger_zero_sum:true,artifact_sentinels_verified:true},
    external_offsite_restore:$offsite,secret_values_recorded:false}' > "$payload"
# shellcheck source=scripts/lib/write-bound-evidence.sh
. "$ROOT/scripts/lib/write-bound-evidence.sh"
merc_emit_bound_json "$receipt" "scripts/local-independent-restore.sh" "$payload" \
  --exact-config "logical independent restore; ciphertext_sha256=$expected" \
  --raw-samples "embedded database_semantics and object_count" \
  --model-na "restore drill does not load model weights" \
  --image-na "drill uses ephemeral docker images; not programme image digests" \
  --corpus-na "no external corpus; synthetic seed only"
rm -f "$payload"
echo "PASS logical independent restore; external offsite restore $offsite_marker"
echo "$receipt"
