#!/usr/bin/env bash
# Isolated encrypted backup to an independent S3-compatible provider, then
# independently download, hash, decrypt, and restore into a new environment.
#
# This is not `make restore-drill` (local tool proof) and not
# `make local-independent-restore` (same-host ciphertext handoff). Ciphertext
# must cross a provider/credential boundary. The strongest already-configured
# boundary on this machine is Cloudflare R2 via .merc-secrets.env.
#
# Source is an isolated rehearsal environment that is destroyed after upload.
# The live droplet volume and the local merc-postgres-1 / merc_pgdata volume
# are never touched.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
MODE="execute"
for arg in "$@"; do
  case "$arg" in
    --check) MODE="check" ;;
    --execute) MODE="execute" ;;
    -h|--help)
      cat <<'USAGE'
usage: scripts/offsite-independent-restore.sh [--check|--execute]

  --check    tools, env, and offsite independence (no dump, no upload)
  --execute  isolated seed → encrypt → upload ciphertext only → destroy
             source → independently download/hash → isolated decrypt/restore
USAGE
      exit 0
      ;;
    *)
      echo "usage: scripts/offsite-independent-restore.sh [--check|--execute]" >&2
      exit 2
      ;;
  esac
done

die() { echo "[offsite-restore] ERROR: $*" >&2; exit 1; }
log() { echo "[offsite-restore] $*"; }

for command_name in docker age age-keygen jq shasum python3 aws git; do
  command -v "$command_name" >/dev/null 2>&1 || die "$command_name is required"
done

source_env() {
  local path="$1"
  [ -n "$path" ] && [ -f "$path" ] || return 0
  set -a
  # shellcheck disable=SC1090
  . "$path"
  set +a
}

load_backup_env() {
  source_env "$ROOT/.merc-secrets.env"
  source_env "${MERC_SECRETS_ENV:-}"
  source_env "${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"
  source_env "$ROOT/.env"

  local common main
  common="$(git -C "$ROOT" rev-parse --git-common-dir 2>/dev/null || true)"
  if [ -n "$common" ]; then
    main="$(cd "$common/.." && pwd -P)"
    if [ -n "$main" ] && [ "$main" != "$ROOT" ]; then
      source_env "$main/.merc-secrets.env"
      source_env "$main/.env.go-closure"
    fi
  fi

  if [ -z "${MERC_BACKUP_S3_ENDPOINT:-}" ] && [ -n "${R2_ENDPOINT:-}" ]; then
    MERC_BACKUP_S3_ENDPOINT="$R2_ENDPOINT"
  fi
  if [ -z "${MERC_BACKUP_OFFSITE:-}" ] && [ -n "${R2_BUCKET:-}" ]; then
    MERC_BACKUP_OFFSITE="s3://${R2_BUCKET}/offsite-alpha"
  fi
  if [ -z "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${R2_ACCESS_KEY_ID:-}" ]; then
    AWS_ACCESS_KEY_ID="$R2_ACCESS_KEY_ID"
    AWS_SECRET_ACCESS_KEY="${R2_SECRET_ACCESS_KEY:-}"
  fi
  AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-auto}"
  export AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_DEFAULT_REGION
  export MERC_BACKUP_S3_ENDPOINT MERC_BACKUP_OFFSITE
}

load_backup_env

[ -n "${MERC_BACKUP_OFFSITE:-}" ] || die "MERC_BACKUP_OFFSITE is unset and no R2_BUCKET mapping is available"
[[ "$MERC_BACKUP_OFFSITE" == s3://* ]] || die "MERC_BACKUP_OFFSITE must be s3://..."
[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
  || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or R2_ACCESS_KEY_ID / R2_SECRET_ACCESS_KEY) are unset"
[ -n "${MERC_BACKUP_S3_ENDPOINT:-}" ] || die "MERC_BACKUP_S3_ENDPOINT is unset and no R2_ENDPOINT mapping is available"

case "${MERC_BACKUP_S3_ENDPOINT}" in
  http://127.0.0.1:*|https://127.0.0.1:*|http://localhost:*|https://localhost:*|http://minio:*|http://merc-minio-1:*|http://0.0.0.0:*)
    die "MERC_BACKUP_S3_ENDPOINT is the local/droplet MinIO; offsite must cross a provider/credential boundary"
    ;;
esac
if [[ "${MERC_BACKUP_S3_ENDPOINT}" == *docker.internal* ]] || [[ "${MERC_BACKUP_S3_ENDPOINT}" == *nip.io* ]]; then
  die "MERC_BACKUP_S3_ENDPOINT is a loopback/harness host; refusing local-only stand-in"
fi

AWS_ARGS=(--endpoint-url "$MERC_BACKUP_S3_ENDPOINT")
OFFSITE_BASE="${MERC_BACKUP_OFFSITE%/}"

endpoint_host="$(python3 - "$MERC_BACKUP_S3_ENDPOINT" <<'PY'
import sys
from urllib.parse import urlparse
print(urlparse(sys.argv[1]).hostname or "")
PY
)"
[ -n "$endpoint_host" ] || die "could not parse offsite endpoint host"
if [[ "$endpoint_host" == *r2.cloudflarestorage.com ]]; then
  PROVIDER="cloudflare_r2"
  BOUNDARY="cloudflare_r2_operator_controlled"
else
  PROVIDER="s3_compatible"
  BOUNDARY="s3_compatible_operator_controlled"
fi

log "offsite base=$OFFSITE_BASE endpoint_host=$endpoint_host boundary=$BOUNDARY"

log "probing offsite put/get independence (ciphertext prefix only)"
PROBE_KEY="${OFFSITE_BASE}/.probe/$(date -u +%Y%m%dT%H%M%SZ)-$$"
PROBE_BODY="$(mktemp "${TMPDIR:-/tmp}/merc-offsite-probe.XXXXXX")"
PROBE_DOWN="$(mktemp "${TMPDIR:-/tmp}/merc-offsite-probe-down.XXXXXX")"
printf 'offsite-probe %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$PROBE_BODY"
# R2/S3 list of a not-yet-created prefix is not a capability proof and often
# fails closed on an empty prefix. Put/get of a probe object is the check.
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$PROBE_BODY" "$PROBE_KEY" \
  || die "cannot put probe object to $PROBE_KEY"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$PROBE_KEY" "$PROBE_DOWN" \
  || die "cannot get probe object from $PROBE_KEY"
cmp -s "$PROBE_BODY" "$PROBE_DOWN" || die "offsite probe download does not match upload"
aws "${AWS_ARGS[@]}" s3 rm "$PROBE_KEY" --only-show-errors >/dev/null || true
rm -f "$PROBE_BODY" "$PROBE_DOWN"
log "offsite probe ok (put/get/delete)"

if [ "$MODE" = "check" ]; then
  log "CHECK ok: age/aws/docker present, offsite URI independent, credentials usable"
  exit 0
fi

PG_IMAGE='postgres:17@sha256:a426e44bac0b759c95894d68e1a0ac03ecc20b619f498a91aae373bf06d8508d'
MINIO_IMAGE='minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'
MC_IMAGE='minio/mc@sha256:a7fe349ef4bd8521fb8497f55c6042871b2ae640607cf99d9bede5e9bdf11727'

RUN="merc-offsite-$RANDOM-$$"
# Refuse any collision with the live local compose / droplet-shaped names.
case "$RUN" in
  merc-postgres-1|merc-minio-1|merc_pgdata) die "refusing to reuse a live stack name" ;;
esac

mkdir -p "$ROOT/.artifacts"
WORK="$(mktemp -d "$ROOT/.artifacts/merc-offsite-restore.XXXXXX")"
A_PG="${RUN}-a-pg"; A_MINIO="${RUN}-a-minio"; A_NET="${RUN}-a"
B_PG="${RUN}-b-pg"; B_MINIO="${RUN}-b-minio"; B_NET="${RUN}-b"
A_PG_VOL="${RUN}-a-pgdata"; A_OBJ_VOL="${RUN}-a-objects"
B_PG_VOL="${RUN}-b-pgdata"; B_OBJ_VOL="${RUN}-b-objects"
A_PW="offsite-a-${RANDOM}-${RANDOM}-password"
B_PW="offsite-b-${RANDOM}-${RANDOM}-password"
A_USER="offsitea${RANDOM}"; A_SECRET="offsite-a-${RANDOM}-${RANDOM}-object-secret"
B_USER="offsiteb${RANDOM}"; B_SECRET="offsite-b-${RANDOM}-${RANDOM}-object-secret"
SOURCE_DESTROYED=false
BACKUP_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
BACKUP_ID="$(date -u +%Y%m%dT%H%M%SZ)"
DEST="${OFFSITE_BASE}/${BACKUP_ID}"
SCHEMA_FILE=""

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
    [ "$(date +%s)" -lt "$deadline" ] || die "PostgreSQL $container did not become ready"
    sleep 1
  done
}

wait_minio() {
  local network="$1" user="$2" secret="$3" deadline=$(( $(date +%s) + 90 ))
  until docker run --rm --network "$network" --entrypoint /bin/sh "$MC_IMAGE" -c \
    "mc alias set local http://minio:9000 '$user' '$secret' >/dev/null && mc ready local >/dev/null" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || die "MinIO on $network did not become ready"
    sleep 1
  done
}

resolve_schema() {
  if [ -f "$ROOT/control/schema.sql" ]; then
    SCHEMA_FILE="$ROOT/control/schema.sql"
    return
  fi
  SCHEMA_FILE="$WORK/schema.sql"
  git -C "$ROOT" show HEAD:control/schema.sql >"$SCHEMA_FILE" \
    || die "control/schema.sql is not materialized and git show failed"
  [ -s "$SCHEMA_FILE" ] || die "git show HEAD:control/schema.sql produced an empty file"
}

mkdir -p "$WORK/a/plain/objects/jobs/embed" "$WORK/a/plain/objects/jobs/batch" \
  "$WORK/b/inbox" "$WORK/b/restored" "$WORK/verify"
chmod 700 "$WORK" "$WORK/a" "$WORK/b" "$WORK/verify"
resolve_schema

printf '%s\n' '{"vectors":[[0.1,0.2]]}' > "$WORK/a/plain/objects/jobs/embed/result.json"
printf '%s\n' '{"text":"synthetic result"}' > "$WORK/a/plain/objects/jobs/batch/result.json"

log "starting isolated source PostgreSQL and MinIO (live merc-postgres-1 / merc_pgdata untouched)"
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
docker exec -i "$A_PG" psql -X -v ON_ERROR_STOP=1 -U cx -d cx < "$SCHEMA_FILE" >/dev/null 2>"$WORK/schema.log"

docker exec -i "$A_PG" psql -X -v ON_ERROR_STOP=1 -U cx -d cx >/dev/null <<'SQL'
INSERT INTO buyers(id,email,free_credit_usd) VALUES
 ('00000000-0000-4000-8000-000000000101','restore-buyer@example.invalid',0);
INSERT INTO suppliers(id,email,status) VALUES
 ('00000000-0000-4000-8000-000000000201','restore-worker-one@example.invalid','active'),
 ('00000000-0000-4000-8000-000000000202','restore-worker-two@example.invalid','active');
INSERT INTO workers(id,supplier_id,hw_class,version) VALUES
 ('00000000-0000-4000-8000-000000000301','00000000-0000-4000-8000-000000000201','apple_silicon_pro','restore-offsite'),
 ('00000000-0000-4000-8000-000000000302','00000000-0000-4000-8000-000000000202','apple_silicon_pro','restore-offsite');
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
docker exec "$A_PG" pg_dump -U cx -d cx -Fc > "$WORK/a/plain/db.dump"
[ -s "$WORK/a/plain/db.dump" ] || die "pg_dump produced an empty file"
( cd "$WORK/a/plain" && shasum -a 256 db.dump > db.dump.sha256 )
docker run --rm --network "$A_NET" -v "$WORK/a/plain/objects-export:/export" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$A_USER' '$A_SECRET' >/dev/null && mc mirror local/cx-jobs /export >/dev/null"
tar -C "$WORK/a/plain/objects-export" -cf "$WORK/a/plain/objects.tar" .
( cd "$WORK/a/plain" && shasum -a 256 objects.tar > objects.tar.sha256 )
jq -nc --arg id "$BACKUP_ID" --arg database cx \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  '{schema_version:1,backup_id:$id,database:$database,objects_included:true,created_at:$created}' \
  > "$WORK/a/plain/backup-metadata.json"
# Hash only the packed payload members. The seed tree under objects/ is
# already inside objects.tar / objects-export and must not appear as
# extra SHA256SUMS paths the restore side does not unpack.
( cd "$WORK/a/plain" && shasum -a 256 \
    db.dump db.dump.sha256 objects.tar objects.tar.sha256 backup-metadata.json \
    > SHA256SUMS )

age-keygen -o "$WORK/a/identity.txt" 2>"$WORK/a/keygen.log"
chmod 600 "$WORK/a/identity.txt"
recipient="$(awk '/^# public key:/ {print $4}' "$WORK/a/identity.txt")"
[[ "$recipient" == age1* ]] || die "age-keygen did not emit an age1 recipient"
tar -C "$WORK/a/plain" -cf "$WORK/a/backup.tar" \
  db.dump db.dump.sha256 objects.tar objects.tar.sha256 backup-metadata.json SHA256SUMS objects-export
age -r "$recipient" -o "$WORK/a/backup.tar.age" "$WORK/a/backup.tar" \
  || die "age encryption failed; nothing was uploaded"
( cd "$WORK/a" && shasum -a 256 backup.tar.age > backup.tar.age.sha256 )
rm -f "$WORK/a/backup.tar"
rm -rf "$WORK/a/plain"
CIPHER_BYTES="$(wc -c < "$WORK/a/backup.tar.age" | tr -d ' ')"
PRODUCER_CIPHER_SHA="$(awk '{print $1}' "$WORK/a/backup.tar.age.sha256")"

CREATED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
jq -nc --arg id "$BACKUP_ID" --arg sha "$PRODUCER_CIPHER_SHA" \
  --arg created "$CREATED_AT" --arg offsite "$DEST" --argjson bytes "$CIPHER_BYTES" \
  '{schema_version:2,kind:"merc_encrypted_offsite_backup",
    backup_id:$id,cipher:"age-x25519",ciphertext_sha256:$sha,
    ciphertext_bytes:$bytes,created_at:$created,database:"cx",
    objects_included:true,offsite_uri:$offsite}' \
  > "$WORK/a/manifest.json"
PRODUCER_MANIFEST_SHA="$(shasum -a 256 "$WORK/a/manifest.json" | awk '{print $1}')"

log "upload ciphertext only → $DEST"
mkdir -p "$WORK/a/upload"
cp "$WORK/a/backup.tar.age" "$WORK/a/upload/backup.tar.age"
cp "$WORK/a/backup.tar.age.sha256" "$WORK/a/upload/backup.tar.age.sha256"
cp "$WORK/a/manifest.json" "$WORK/a/upload/manifest.json"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors --recursive "$WORK/a/upload" "$DEST" \
  || die "OFFSITE UPLOAD FAILED to $DEST"
# Visibility is proven by the independent download below. Do not use `s3 ls`
# on an R2 prefix as a gate: empty-prefix list is not a capability signal.

log "destroying source environment and producer plaintext/ciphertext copies"
docker rm -f "$A_PG" "$A_MINIO" >/dev/null
docker network rm "$A_NET" >/dev/null
docker volume rm "$A_PG_VOL" "$A_OBJ_VOL" >/dev/null
SOURCE_DESTROYED=true
rm -f "$WORK/a/backup.tar.age" "$WORK/a/backup.tar.age.sha256" \
  "$WORK/a/upload/backup.tar.age" "$WORK/a/upload/backup.tar.age.sha256"
rm -rf "$WORK/a/upload" "$WORK/a/plain"
# Keep identity + producer manifest sha for later comparison; identity stays off-box.

log "independent download of manifest and ciphertext (new directory, computed hashes)"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$DEST/backup.tar.age" "$WORK/verify/backup.tar.age" \
  || die "independent ciphertext download failed"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$DEST/manifest.json" "$WORK/verify/manifest.json" \
  || die "independent manifest download failed"
DOWNLOADED_CIPHER_SHA="$(shasum -a 256 "$WORK/verify/backup.tar.age" | awk '{print $1}')"
DOWNLOADED_MANIFEST_SHA="$(shasum -a 256 "$WORK/verify/manifest.json" | awk '{print $1}')"
DOWNLOADED_BYTES="$(wc -c < "$WORK/verify/backup.tar.age" | tr -d ' ')"
[ "$DOWNLOADED_CIPHER_SHA" = "$PRODUCER_CIPHER_SHA" ] || die "downloaded ciphertext SHA-256 does not match producer ciphertext"
[ "$DOWNLOADED_MANIFEST_SHA" = "$PRODUCER_MANIFEST_SHA" ] || die "downloaded manifest SHA-256 does not match producer manifest"
[ "$DOWNLOADED_BYTES" = "$CIPHER_BYTES" ] || die "downloaded ciphertext size does not match producer size"
MANIFEST_OFFSITE="$(jq -r '.offsite_uri' "$WORK/verify/manifest.json")"
MANIFEST_CIPHER="$(jq -r '.ciphertext_sha256' "$WORK/verify/manifest.json")"
[ "$MANIFEST_OFFSITE" = "$DEST" ] || die "downloaded manifest.offsite_uri is not $DEST"
[ "$MANIFEST_CIPHER" = "$DOWNLOADED_CIPHER_SHA" ] || die "downloaded manifest.ciphertext_sha256 does not match independently hashed ciphertext"

VERIFIED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
VERIFY_EXACT="$WORK/verify/verification.exact.json"
VERIFY_PAYLOAD="$WORK/verify/verification.json"
jq -n \
  --arg id "$BACKUP_ID" --arg offsite "$DEST" \
  --arg manifest_sha "$DOWNLOADED_MANIFEST_SHA" \
  --arg ciphertext_sha "$DOWNLOADED_CIPHER_SHA" \
  --arg downloaded_sha "$DOWNLOADED_CIPHER_SHA" \
  --arg verified "$VERIFIED_AT" \
  --argjson bytes "$DOWNLOADED_BYTES" '
  {
    schema_version:1,
    kind:"merc_offsite_backup_verification",
    status:"PASS",
    backup_id:$id,
    offsite_uri:$offsite,
    manifest_sha256:$manifest_sha,
    ciphertext:{
      manifest_sha256:$ciphertext_sha,
      downloaded_sha256:$downloaded_sha,
      bytes:$bytes
    },
    verified_at:$verified,
    checks:{
      offsite_bundle_visible:true,
      independent_manifest_download:true,
      independent_ciphertext_download:true,
      manifest_checksum_match:true,
      ciphertext_checksum_match:true
    },
    policy:{
      encrypted_before_upload:true,
      plaintext_uploaded:false,
      secret_values_recorded:false
    }
  }' > "$VERIFY_EXACT"

python3 "$ROOT/scripts/validate-backup-verification-receipt.py" \
  "$WORK/verify/manifest.json" "$VERIFY_EXACT" \
  --ciphertext "$WORK/verify/backup.tar.age" \
  --offsite-base "$OFFSITE_BASE" \
  --not-before "$BACKUP_STARTED_AT" \
  --checked-at "$VERIFIED_AT" >/dev/null \
  || die "schema-exact verification receipt failed"

jq --arg boundary "$BOUNDARY" --arg provider "$PROVIDER" \
  --arg endpoint_host "$endpoint_host" \
  '. + {independence:{
      boundary:$boundary,
      provider:$provider,
      endpoint_host:$endpoint_host,
      operator_controlled:true,
      source_kind:"isolated_rehearsal_environment",
      live_droplet_volume_not_the_source:true,
      offsite_credential_distinct_from_source_object_store:true,
      ciphertext_only_crossed_the_boundary:true,
      verifying_side_hashed_its_own_download:true
    }}' "$VERIFY_EXACT" > "$VERIFY_PAYLOAD"

aws "${AWS_ARGS[@]}" s3 cp --only-show-errors \
  "$VERIFY_EXACT" "$DEST/verification.json" \
  || die "verification receipt upload failed"

log "proving a corrupt independently-downloaded envelope is refused"
cp "$WORK/verify/backup.tar.age" "$WORK/verify/backup.tar.age.corrupt"
python3 - "$WORK/verify/backup.tar.age.corrupt" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
data = bytearray(path.read_bytes())
if not data:
    raise SystemExit("empty age ciphertext")
data[len(data)//2] ^= 0xFF
path.write_bytes(data)
PY
set +e
age -d -i "$WORK/a/identity.txt" -o "$WORK/verify/corrupt.tar" \
  "$WORK/verify/backup.tar.age.corrupt" >"$WORK/verify/age-decrypt-corrupt.log" 2>&1
AGE_CORRUPT_RC=$?
set -e
[ "$AGE_CORRUPT_RC" -ne 0 ] || die "corrupt age envelope decrypted successfully"
CORRUPT_REJECTED=true

log "decrypt independently downloaded ciphertext and restore into a new isolated environment"
age --decrypt -i "$WORK/a/identity.txt" -o "$WORK/b/backup.tar" "$WORK/verify/backup.tar.age" \
  || die "backup decryption failed"
if tar -tf "$WORK/b/backup.tar" | awk 'BEGIN{bad=0} /^\// || /(^|\/)\.\.($|\/)/ {bad=1} END{exit bad?0:1}'; then
  die "backup archive contains an unsafe path"
fi
mkdir -p "$WORK/b/restored"
tar -C "$WORK/b/restored" -xf "$WORK/b/backup.tar"
rm -f "$WORK/b/backup.tar"
[ -s "$WORK/b/restored/db.dump" ] || die "decrypted bundle has no db.dump"
( cd "$WORK/b/restored" && shasum -a 256 -c db.dump.sha256 >/dev/null ) \
  || die "db.dump checksum mismatch after independent decrypt"
( cd "$WORK/b/restored" && shasum -a 256 -c objects.tar.sha256 >/dev/null ) \
  || die "objects.tar checksum mismatch after independent decrypt"
( cd "$WORK/b/restored" && shasum -a 256 -c SHA256SUMS >/dev/null ) \
  || die "payload SHA256SUMS mismatch after independent decrypt"
mkdir -p "$WORK/b/restored/objects-export"
tar -C "$WORK/b/restored/objects-export" -xf "$WORK/b/restored/objects.tar"

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
docker exec -i "$B_PG" pg_restore -U cx -d cx --clean --if-exists --no-owner --no-privileges -1 \
  < "$WORK/b/restored/db.dump"
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
jq -e '.buyers==1 and .workers==2 and .completed_embed==1 and .completed_batch==1 and .cancelled==1 and .retried==1 and .held_payout==1 and .webhooks==1 and .ledger_sum=="0.000000"' <<< "$semantic" >/dev/null \
  || die "restored database semantics do not match the seeded application invariants"

object_count="$(docker run --rm --network "$B_NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$B_USER' '$B_SECRET' >/dev/null && mc find local/cx-jobs --name '*' | wc -l" | tr -d '[:space:]')"
[ "$object_count" -eq 2 ] || die "restored object count $object_count != 2"
embed_sentinel="$(docker run --rm --network "$B_NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$B_USER' '$B_SECRET' >/dev/null && mc cat local/cx-jobs/jobs/embed/result.json")"
batch_sentinel="$(docker run --rm --network "$B_NET" --entrypoint /bin/sh "$MC_IMAGE" -c \
  "mc alias set local http://minio:9000 '$B_USER' '$B_SECRET' >/dev/null && mc cat local/cx-jobs/jobs/batch/result.json")"
printf '%s\n' "$embed_sentinel" | jq -e '.vectors[0][0]==0.1' >/dev/null \
  || die "embed artifact sentinel mismatch"
printf '%s\n' "$batch_sentinel" | jq -e '.text=="synthetic result"' >/dev/null \
  || die "batch artifact sentinel mismatch"

COMPLETED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
RESTORE_PAYLOAD="$WORK/verify/restore.json"
jq -n \
  --arg id "$BACKUP_ID" --arg offsite "$DEST" \
  --arg cipher "$DOWNLOADED_CIPHER_SHA" --arg at "$COMPLETED_AT" \
  --arg boundary "$BOUNDARY" --arg provider "$PROVIDER" \
  --arg endpoint_host "$endpoint_host" \
  --argjson semantic "$semantic" --argjson objects "$object_count" \
  --argjson destroyed "$SOURCE_DESTROYED" --argjson corrupt "$CORRUPT_REJECTED" '
  {
    schema_version:1,
    kind:"external_offsite_restore",
    status:"PASS",
    backup_id:$id,
    offsite_uri:$offsite,
    ciphertext_sha256:$cipher,
    completed_at:$at,
    secret_values_recorded:false,
    independent_download:true,
    ciphertext_checksum_verified:true,
    decrypt_isolated:true,
    source_environment_destroyed:$destroyed,
    new_database_credentials:true,
    new_object_credentials:true,
    new_namespace:true,
    integrity:{
      ciphertext_verified:true,
      payload_checksums_verified:true,
      corrupt_backup_rejected:$corrupt,
      ledger_zero_sum:true,
      artifact_sentinels_verified:true,
      database_semantics:$semantic,
      object_count:$objects
    },
    independence:{
      boundary:$boundary,
      provider:$provider,
      endpoint_host:$endpoint_host,
      operator_controlled:true,
      source_kind:"isolated_rehearsal_environment",
      live_droplet_volume_not_the_source:true,
      offsite_credential_distinct_from_source_object_store:true,
      ciphertext_only_crossed_the_boundary:true,
      verifying_side_hashed_its_own_download:true
    }
  }' > "$RESTORE_PAYLOAD"

# shellcheck source=scripts/lib/write-bound-evidence.sh
. "$ROOT/scripts/lib/write-bound-evidence.sh"
mkdir -p "$ROOT/evidence/external"
merc_emit_bound_json \
  "$ROOT/evidence/external/offsite-backup-verification.json" \
  "scripts/offsite-independent-restore.sh" \
  "$VERIFY_PAYLOAD" \
  --exact-config "offsite backup verification backup_id=$BACKUP_ID offsite=$DEST boundary=$BOUNDARY" \
  --raw-samples "independently downloaded manifest and ciphertext SHA-256" \
  --model-na "offsite backup does not load model weights" \
  --image-na "isolated postgres/minio pins live in the harness, not this measurement" \
  --corpus-na "no external corpus; isolated seed only"

merc_emit_bound_json \
  "$ROOT/evidence/external/offsite-independent-restore.json" \
  "scripts/offsite-independent-restore.sh" \
  "$RESTORE_PAYLOAD" \
  --exact-config "offsite independent restore backup_id=$BACKUP_ID offsite=$DEST boundary=$BOUNDARY" \
  --raw-samples "embedded database_semantics and object_count" \
  --model-na "offsite restore does not load model weights" \
  --image-na "isolated postgres/minio pins live in the harness, not this measurement" \
  --corpus-na "no external corpus; isolated seed only"

# Mark the local logical restore ledger so the external restore content check
# no longer sees external_offsite_restore:NOT EXECUTED.
LOCAL_RECEIPT="$ROOT/evidence/autonomous/logical-independent-restore.json"
if [ -f "$LOCAL_RECEIPT" ]; then
  python3 - "$LOCAL_RECEIPT" <<'PY'
import json, sys
path = sys.argv[1]
doc = json.loads(open(path, encoding="utf-8").read())
doc["external_offsite_restore"] = "PASS"
open(path, "w", encoding="utf-8").write(json.dumps(doc, indent=2) + "\n")
PY
fi

# Identity never leaves this host and is shredded with WORK on EXIT.
log "PASS backup_id=$BACKUP_ID offsite=$DEST ciphertext_sha256=$DOWNLOADED_CIPHER_SHA"
log "receipts: evidence/external/offsite-backup-verification.json evidence/external/offsite-independent-restore.json"
log "independence boundary=$BOUNDARY (operator-controlled; live droplet volume was not the source)"
