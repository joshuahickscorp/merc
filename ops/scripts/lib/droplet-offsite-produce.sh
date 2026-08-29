#!/usr/bin/env bash
# Produce an age-encrypted offsite bundle from the LIVE merc droplet volumes.
# Runs ON the droplet. Only ciphertext, sidecar hashes, the schema-v2 manifest,
# and count/hash observations are left in --work. Plaintext is shredded.
#
# This does not docker volume rm, docker compose down, or write into
# merc_pgdata / merc_miniodata. pg_dump is used; Postgres stays up.
set -euo pipefail

WORK=""
RECIPIENT=""
BACKUP_ID=""
BUCKET="cx-jobs"
PG_CONTAINER="merc-postgres-1"
MINIO_CONTAINER="merc-minio-1"
PG_VOLUME="merc_pgdata"
MINIO_VOLUME="merc_miniodata"
PG_USER="cx"
PG_DB="cx"
MC_IMAGE="${MERC_MC_IMAGE:-minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727}"

usage() {
  echo "usage: droplet-offsite-produce.sh --work DIR --recipient age1... --backup-id ID [--bucket NAME]" >&2
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --work) shift; WORK="${1:-}" ;;
    --recipient) shift; RECIPIENT="${1:-}" ;;
    --backup-id) shift; BACKUP_ID="${1:-}" ;;
    --bucket) shift; BUCKET="${1:-}" ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift || true
done

die() { echo "[droplet-produce] ERROR: $*" >&2; exit 1; }
log() { echo "[droplet-produce] $*"; }

[ -n "$WORK" ] && [ -n "$RECIPIENT" ] && [ -n "$BACKUP_ID" ] || usage
[[ "$RECIPIENT" == age1* ]] || die "recipient must be an age1 public key"
[[ "$BACKUP_ID" =~ ^[0-9]{8}T[0-9]{6}Z$ ]] || die "backup-id must be YYYYMMDDTHHMMSSZ"
[[ "$WORK" == /tmp/merc-offsite-* ]] || die "work dir must be under /tmp/merc-offsite-* (refusing unexpected path)"

for command_name in docker jq sha256sum tar; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

if ! command -v age >/dev/null 2>&1; then
  log "age missing; installing the Ubuntu package (no container restart)"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq age >/dev/null
  command -v age >/dev/null 2>&1 || die "age install failed"
fi

docker inspect "$PG_CONTAINER" >/dev/null 2>&1 || die "$PG_CONTAINER is not present"
docker inspect "$MINIO_CONTAINER" >/dev/null 2>&1 || die "$MINIO_CONTAINER is not present"
pg_running="$(docker inspect -f '{{.State.Running}}' "$PG_CONTAINER")"
minio_running="$(docker inspect -f '{{.State.Running}}' "$MINIO_CONTAINER")"
[ "$pg_running" = true ] || die "$PG_CONTAINER is not running"
[ "$minio_running" = true ] || die "$MINIO_CONTAINER is not running"

pg_mount="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/postgresql/data"}}{{.Name}}{{end}}{{end}}' "$PG_CONTAINER")"
minio_mount="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' "$MINIO_CONTAINER")"
[ "$pg_mount" = "$PG_VOLUME" ] || die "$PG_CONTAINER does not mount $PG_VOLUME (saw '$pg_mount'); refusing to dump the wrong volume"
[ "$minio_mount" = "$MINIO_VOLUME" ] || die "$MINIO_CONTAINER does not mount $MINIO_VOLUME (saw '$minio_mount'); refusing to copy the wrong volume"
docker volume inspect "$PG_VOLUME" >/dev/null 2>&1 || die "volume $PG_VOLUME missing"
docker volume inspect "$MINIO_VOLUME" >/dev/null 2>&1 || die "volume $MINIO_VOLUME missing"

mkdir -p "$WORK/plain/objects-export"
chmod 700 "$WORK" "$WORK/plain"
# If we are interrupted, shred plaintext. Never touch named live volumes.
cleanup_plain() {
  if [ -d "$WORK/plain" ]; then
    find "$WORK/plain" -type f -exec shred -u {} + 2>/dev/null \
      || rm -rf "$WORK/plain"
    rm -rf "$WORK/plain"
  fi
}
trap cleanup_plain EXIT INT TERM

log "observing live postgres $PG_CONTAINER (read-only)"
semantic="$(docker exec "$PG_CONTAINER" psql -X -qAt -U "$PG_USER" -d "$PG_DB" -c \
  "SELECT json_build_object(
     'buyers',(SELECT count(*)::int FROM buyers),
     'suppliers',(SELECT count(*)::int FROM suppliers),
     'workers',(SELECT count(*)::int FROM workers),
     'jobs',(SELECT count(*)::int FROM jobs),
     'tasks',(SELECT count(*)::int FROM tasks),
     'ledger_entries',(SELECT count(*)::int FROM ledger_entries),
     'webhooks',(SELECT count(*)::int FROM webhooks),
     'ledger_sum',(SELECT COALESCE(sum(amount_usd),0)::numeric(20,6)::text FROM ledger_entries)
   )::text")"
[ -n "$semantic" ] || die "live semantic query returned empty"

ENVF="$(mktemp /tmp/merc-minio-env.XXXXXX)"
chmod 600 "$ENVF"
docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$MINIO_CONTAINER" >"$ENVF"
MINIO_USER="$(awk -F= '/^MINIO_ROOT_USER=/{print substr($0, index($0,"=")+1); exit}' "$ENVF")"
MINIO_SECRET="$(awk -F= '/^MINIO_ROOT_PASSWORD=/{print substr($0, index($0,"=")+1); exit}' "$ENVF")"
shred -u "$ENVF" 2>/dev/null || rm -f "$ENVF"
[ -n "$MINIO_USER" ] && [ -n "$MINIO_SECRET" ] || die "could not read MinIO credentials from $MINIO_CONTAINER env"

log "mirroring live minio bucket $BUCKET (read-only)"
docker run --rm --network merc_default \
  -v "$WORK/plain/objects-export:/export" \
  --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$MINIO_USER' '$MINIO_SECRET' >/dev/null && \
   mc mirror --overwrite local/${BUCKET} /export >/dev/null"

object_count="$(find "$WORK/plain/objects-export" -type f | wc -l | tr -d ' ')"
sentinels_json="$(
  if [ "$object_count" -eq 0 ]; then
    printf '[]'
  else
    (
      cd "$WORK/plain/objects-export"
      find . -type f -print | sed 's|^\./||' | sort | while IFS= read -r rel; do
        sha="$(sha256sum -- "$rel" | awk '{print $1}')"
        jq -nc --arg key "$rel" --arg sha "$sha" '{key:$key,sha256:$sha}'
      done
    ) | jq -s '.'
  fi
)"

pg_created="$(docker inspect -f '{{.Created}}' "$PG_CONTAINER")"
minio_created="$(docker inspect -f '{{.Created}}' "$MINIO_CONTAINER")"
pg_image="$(docker inspect -f '{{.Config.Image}}' "$PG_CONTAINER")"
minio_image="$(docker inspect -f '{{.Config.Image}}' "$MINIO_CONTAINER")"
pg_health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$PG_CONTAINER")"
minio_health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$MINIO_CONTAINER")"

jq -n \
  --argjson semantic "$semantic" \
  --argjson objects "$object_count" \
  --argjson sentinels "$sentinels_json" \
  --arg pg "$PG_CONTAINER" --arg minio "$MINIO_CONTAINER" \
  --arg pgvol "$PG_VOLUME" --arg miniovol "$MINIO_VOLUME" \
  --arg bucket "$BUCKET" --arg db "$PG_DB" \
  --arg pg_created "$pg_created" --arg minio_created "$minio_created" \
  --arg pg_image "$pg_image" --arg minio_image "$minio_image" \
  --arg pg_health "$pg_health" --arg minio_health "$minio_health" \
  --arg observed "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
  $semantic + {
    object_count:$objects,
    object_sentinels:$sentinels,
    object_bucket:$bucket,
    database:$db,
    postgres_container:$pg,
    minio_container:$minio,
    pgdata_volume:$pgvol,
    minio_volume:$miniovol,
    postgres_created:$pg_created,
    minio_created:$minio_created,
    postgres_image:$pg_image,
    minio_image:$minio_image,
    postgres_health:$pg_health,
    minio_health:$minio_health,
    observed_at:$observed
  }' > "$WORK/source-observations.json"

log "pg_dump -Fc from live $PG_CONTAINER (hot dump; process not stopped)"
docker exec "$PG_CONTAINER" pg_dump -U "$PG_USER" -d "$PG_DB" -Fc > "$WORK/plain/db.dump"
[ -s "$WORK/plain/db.dump" ] || die "pg_dump produced an empty file"
( cd "$WORK/plain" && sha256sum db.dump > db.dump.sha256 )

tar -C "$WORK/plain/objects-export" -cf "$WORK/plain/objects.tar" .
( cd "$WORK/plain" && sha256sum objects.tar > objects.tar.sha256 )

jq -nc --arg id "$BACKUP_ID" --arg database "$PG_DB" \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{schema_version:1,backup_id:$id,database:$database,objects_included:true,created_at:$created,source:"live_droplet_volume"}' \
  > "$WORK/plain/backup-metadata.json"
cp "$WORK/source-observations.json" "$WORK/plain/source-observations.json"

( cd "$WORK/plain" && sha256sum \
    db.dump db.dump.sha256 objects.tar objects.tar.sha256 \
    backup-metadata.json source-observations.json \
    > SHA256SUMS )

tar -C "$WORK/plain" -cf "$WORK/backup.tar" \
  db.dump db.dump.sha256 objects.tar objects.tar.sha256 \
  backup-metadata.json source-observations.json SHA256SUMS objects-export
age -r "$RECIPIENT" -o "$WORK/backup.tar.age" "$WORK/backup.tar" \
  || die "age encryption failed; nothing left to upload"
rm -f "$WORK/backup.tar"
cleanup_plain
trap - EXIT INT TERM

( cd "$WORK" && sha256sum backup.tar.age > backup.tar.age.sha256 )
CIPHER_BYTES="$(wc -c < "$WORK/backup.tar.age" | tr -d ' ')"
CIPHER_SHA="$(awk '{print $1}' "$WORK/backup.tar.age.sha256")"
CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# offsite_uri is filled by the verifying/upload side; produce writes a placeholder
# that the uploader rewrites to the exact s3 URI before the independent download.
jq -nc --arg id "$BACKUP_ID" --arg sha "$CIPHER_SHA" \
  --arg created "$CREATED_AT" --argjson bytes "$CIPHER_BYTES" \
  '{schema_version:2,kind:"merc_encrypted_offsite_backup",
    backup_id:$id,cipher:"age-x25519",ciphertext_sha256:$sha,
    ciphertext_bytes:$bytes,created_at:$created,database:"cx",
    objects_included:true,offsite_uri:("s3://pending/"+$id)}' \
  > "$WORK/manifest.partial.json"

jq -n \
  --arg id "$BACKUP_ID" \
  --arg cipher "$CIPHER_SHA" \
  --argjson bytes "$CIPHER_BYTES" \
  --arg age "$(age --version 2>&1 | head -1)" \
  --slurpfile obs "$WORK/source-observations.json" \
  '{backup_id:$id,ciphertext_sha256:$cipher,ciphertext_bytes:$bytes,
    age_version:$age,observations:$obs[0],
    live_volumes_untouched:true,plaintext_shredded:true}' \
  > "$WORK/produce-result.json"

# Confirm live volumes still exist and containers still run.
docker inspect -f '{{.State.Running}}' "$PG_CONTAINER" | grep -qx true \
  || die "postgres stopped during produce"
docker inspect -f '{{.State.Running}}' "$MINIO_CONTAINER" | grep -qx true \
  || die "minio stopped during produce"
docker volume inspect "$PG_VOLUME" >/dev/null
docker volume inspect "$MINIO_VOLUME" >/dev/null

log "ciphertext ready bytes=$CIPHER_BYTES sha256=$CIPHER_SHA"
log "observations buyers=$(jq -r '.buyers' "$WORK/source-observations.json") objects=$object_count"
