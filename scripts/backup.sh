#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for env_file in "$ROOT/.env" "${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"; do
  [ -f "$env_file" ] || continue
  set -a
  # shellcheck disable=SC1090
  . "$env_file"
  set +a
done

DB_ONLY=0
DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --db-only) DB_ONLY=1 ;;
    --dry-run) DRY_RUN=1 ;;
    *)
      echo "usage: scripts/backup.sh [--db-only] [--dry-run]" >&2
      exit 2
      ;;
  esac
done

RESULT_FILE="${MERC_BACKUP_RESULT_FILE:-}"
if [ -n "$RESULT_FILE" ]; then
  [[ "$RESULT_FILE" == /* ]] || {
    echo "[backup] ERROR: MERC_BACKUP_RESULT_FILE must be absolute" >&2
    exit 1
  }
  [ ! -e "$RESULT_FILE" ] || {
    echo "[backup] ERROR: MERC_BACKUP_RESULT_FILE already exists" >&2
    exit 1
  }
fi

die() { echo "[backup] ERROR: $*" >&2; exit 1; }
log() { echo "[backup] $*"; }

write_backup_status() {
  # Optional low-cardinality health input for the control-plane metrics endpoint.
  # Mount its parent directory read-only into the control container and set the
  # same MERC_BACKUP_STATUS_FILE path there.
  if [ -z "${MERC_BACKUP_STATUS_FILE:-}" ]; then
    return 0
  fi
  local status_dir status_tmp
  status_dir="$(dirname -- "$MERC_BACKUP_STATUS_FILE")"
  status_tmp="${MERC_BACKUP_STATUS_FILE}.tmp.$$"
  mkdir -p "$status_dir"
  umask 027
  date -u +%s > "$status_tmp"
  chmod 0640 "$status_tmp"
  mv -f -- "$status_tmp" "$MERC_BACKUP_STATUS_FILE"
  log "backup health timestamp updated: $MERC_BACKUP_STATUS_FILE"
}

OFFSITE="${MERC_BACKUP_OFFSITE:-}"
[ -n "$OFFSITE" ] || die "MERC_BACKUP_OFFSITE is unset. Set it (and the offsite \
S3 creds) in .env · see .env.example. Refusing to take a backup with nowhere \
offsite to put it."

COMPOSE_FILE="${MERC_COMPOSE_FILE:-$ROOT/docker-compose.prod.yml}"
PG_SERVICE="${MERC_PG_SERVICE:-postgres}"
PG_USER="${POSTGRES_USER:-cx}"
PG_DB="${POSTGRES_DB:-cx}"

AWS_ARGS=()
if [ -n "${MERC_BACKUP_S3_ENDPOINT:-}" ]; then
  AWS_ARGS+=(--endpoint-url "$MERC_BACKUP_S3_ENDPOINT")
fi

RECIPIENT="${MERC_BACKUP_ENCRYPTION_RECIPIENT:-}"
[[ "$RECIPIENT" == age1* ]] || die "MERC_BACKUP_ENCRYPTION_RECIPIENT must be an age1... public recipient"

if [ "$DRY_RUN" -eq 1 ]; then
  # Validates scheduler/metric wiring without docker, age, aws, or offsite I/O.
  # Writes MERC_BACKUP_STATUS_FILE so the control metrics path can be exercised in CI.
  # Do not run --dry-run against a production status path: it would falsely
  # reset merc_backup_age_seconds without an offsite backup.
  [ -n "${MERC_BACKUP_STATUS_FILE:-}" ] || die "--dry-run requires MERC_BACKUP_STATUS_FILE"
  [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
    || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY required even for dry-run (config presence check)"
  log "dry-run: config ok (offsite=$OFFSITE recipient=age1… status=$MERC_BACKUP_STATUS_FILE)"
  log "dry-run: WARNING writing status without offsite backup (test/wiring only)"
  write_backup_status
  log "dry-run: done (no dump, no upload)"
  exit 0
fi

RECIPIENT="${MERC_BACKUP_ENCRYPTION_RECIPIENT:-}"
[[ "$RECIPIENT" == age1* ]] || die "MERC_BACKUP_ENCRYPTION_RECIPIENT must be an age1... public recipient"

if [ "$DRY_RUN" -eq 1 ]; then
  # Validates scheduler/metric wiring without docker, age, aws, or offsite I/O.
  # Writes MERC_BACKUP_STATUS_FILE so the control metrics path can be exercised.
  # Do not run --dry-run against a production status path: it would falsely
  # reset merc_backup_age_seconds without an offsite backup.
  [ -n "${MERC_BACKUP_STATUS_FILE:-}" ] || die "--dry-run requires MERC_BACKUP_STATUS_FILE"
  [ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
    || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY required even for dry-run (config presence check)"
  log "dry-run: config ok (offsite=$OFFSITE recipient=age1… status=$MERC_BACKUP_STATUS_FILE)"
  log "dry-run: WARNING writing status without offsite backup (test/wiring only)"
  write_backup_status
  log "dry-run: done (no dump, no upload)"
  exit 0
fi

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v aws >/dev/null 2>&1 || die "aws CLI not found (install awscli; it \
speaks S3/Spaces/R2/B2). Offsite upload requires it."
command -v age >/dev/null 2>&1 || die "age not found; refusing to upload a plaintext backup"
command -v python3 >/dev/null 2>&1 || die "python3 not found; backup verification is mandatory"

[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
  || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (offsite bucket creds) not \
set. See .env.example. Refusing to back up with no way to authenticate offsite."

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

BACKUP_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
TS="$(date -u +%Y%m%dT%H%M%SZ)"
STAGE="${MERC_BACKUP_DIR:-$ROOT/.artifacts/backups}/$TS"
PAYLOAD="$STAGE/payload"
DEST="${OFFSITE%/}/$TS"
[ ! -e "$STAGE" ] || die "backup stage already exists for $TS; refusing to mix invocations"
mkdir -p "$PAYLOAD"

log "pg_dump (-Fc) $PG_DB -> encrypted bundle payload"
if ! dc exec -T "$PG_SERVICE" pg_dump -U "$PG_USER" -d "$PG_DB" -Fc > "$PAYLOAD/db.dump"; then
  die "pg_dump failed · see above. NO backup produced."
fi
[ -s "$PAYLOAD/db.dump" ] || die "pg_dump produced an empty file · refusing to ship a zero-byte backup."
log "db.dump $(du -h "$PAYLOAD/db.dump" | cut -f1)"

( cd "$PAYLOAD" && shasum -a 256 db.dump > db.dump.sha256 )

if [ "$DB_ONLY" -eq 0 ]; then
  S3_BUCKET="${S3_BUCKET:-cx-jobs}"
  log "object store: mirror minio/$S3_BUCKET -> encrypted bundle payload"
  if ! dc run --rm -T \
        -e MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@minio:9000" \
        --entrypoint sh minio/mc -c \
        "mc mirror --overwrite --remove local/${S3_BUCKET} /tmp/o && tar -C /tmp -cf - o" \
        > "$PAYLOAD/objects.tar"; then
    die "object-store mirror failed · see above."
  fi
  log "objects.tar $(du -h "$PAYLOAD/objects.tar" | cut -f1)"
  ( cd "$PAYLOAD" && shasum -a 256 objects.tar > objects.tar.sha256 )
fi

jq -nc --arg id "$TS" --arg database "$PG_DB" \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson objects "$([ "$DB_ONLY" -eq 0 ] && echo true || echo false)" \
  '{schema_version:1,backup_id:$id,database:$database,objects_included:$objects,created_at:$created}' \
  > "$PAYLOAD/backup-metadata.json"
( cd "$PAYLOAD" && tar -cf "$STAGE/backup.tar" . )
age -r "$RECIPIENT" -o "$STAGE/backup.tar.age" "$STAGE/backup.tar" \
  || die "age encryption failed; nothing was uploaded"
( cd "$STAGE" && shasum -a 256 backup.tar.age > backup.tar.age.sha256 )
rm -f "$STAGE/backup.tar"
rm -rf "$PAYLOAD"

jq -nc --arg id "$TS" \
  --arg sha "$(cut -d' ' -f1 "$STAGE/backup.tar.age.sha256")" \
  --arg created "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --arg database "$PG_DB" --arg offsite "$DEST" \
  --argjson objects "$([ "$DB_ONLY" -eq 0 ] && echo true || echo false)" \
  --argjson bytes "$(wc -c < "$STAGE/backup.tar.age" | tr -d ' ')" \
  '{schema_version:2,kind:"merc_encrypted_offsite_backup",
    backup_id:$id,cipher:"age-x25519",ciphertext_sha256:$sha,
    ciphertext_bytes:$bytes,created_at:$created,database:$database,
    objects_included:$objects,offsite_uri:$offsite}' \
  > "$STAGE/manifest.json"

log "ship -> $DEST"
if ! aws "${AWS_ARGS[@]}" s3 cp --only-show-errors --recursive "$STAGE" "$DEST"; then
  die "OFFSITE UPLOAD FAILED to $DEST. The local staging copy is at $STAGE but \
this backup is NOT safe (single host). Investigate creds/endpoint/network."
fi
aws "${AWS_ARGS[@]}" s3 ls "$DEST/backup.tar.age" >/dev/null \
  || die "post-upload verify failed: encrypted bundle not visible at $DEST."
VERIFY="$(mktemp -d "${TMPDIR:-/tmp}/merc-backup-verify.XXXXXX")"
trap 'rm -rf "$VERIFY"' EXIT
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$DEST/backup.tar.age" "$VERIFY/backup.tar.age" \
  || die "independent post-upload download failed"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$DEST/manifest.json" "$VERIFY/manifest.json" \
  || die "independent manifest download failed"
expected="$(cut -d' ' -f1 "$STAGE/backup.tar.age.sha256")"
actual="$(shasum -a 256 "$VERIFY/backup.tar.age" | cut -d' ' -f1)"
[ "$actual" = "$expected" ] || die "downloaded ciphertext checksum mismatch"
manifest_expected="$(shasum -a 256 "$STAGE/manifest.json" | cut -d' ' -f1)"
manifest_actual="$(shasum -a 256 "$VERIFY/manifest.json" | cut -d' ' -f1)"
[ "$manifest_actual" = "$manifest_expected" ] || die "downloaded manifest checksum mismatch"
verified_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
jq -nc \
  --arg id "$TS" --arg offsite "$DEST" \
  --arg manifest_sha "$manifest_expected" \
  --arg ciphertext_sha "$expected" --arg downloaded_sha "$actual" \
  --arg verified "$verified_at" \
  --argjson bytes "$(wc -c < "$STAGE/backup.tar.age" | tr -d ' ')" '
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
  }' > "$STAGE/verification.json"
python3 "$ROOT/scripts/validate-backup-verification-receipt.py" \
  "$STAGE/manifest.json" "$STAGE/verification.json" \
  --ciphertext "$STAGE/backup.tar.age" \
  --offsite-base "$OFFSITE" \
  --not-before "$BACKUP_STARTED_AT" \
  --checked-at "$verified_at" >/dev/null \
  || die "local backup verification receipt failed closed-schema validation"
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors \
  "$STAGE/verification.json" "$DEST/verification.json" \
  || die "verification receipt upload failed"
aws "${AWS_ARGS[@]}" s3 ls "$DEST/verification.json" >/dev/null \
  || die "verification receipt is not visible offsite"
log "offsite verified: $DEST/backup.tar.age sha256=$expected"

write_backup_status

KEEP="${MERC_BACKUP_KEEP_LOCAL:-7}"
BASE="$(dirname "$STAGE")"
ls -1dt "$BASE"/*/ 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  log "prune local $old"; rm -rf "$old"
done

if [ -n "$RESULT_FILE" ]; then
  result_tmp="${RESULT_FILE}.tmp.$$"
  mkdir -p "$(dirname "$RESULT_FILE")"
  umask 077
  jq -nc \
    --arg id "$TS" --arg offsite "$DEST" \
    --arg manifest_sha "$manifest_expected" \
    --arg verification_sha "$(shasum -a 256 "$STAGE/verification.json" | cut -d' ' -f1)" \
    --arg ciphertext_sha "$expected" \
    --arg completed "$(date -u +%Y-%m-%dT%H:%M:%SZ)" '
    {
      schema_version:1,
      kind:"merc_backup_invocation_result",
      status:"PASS",
      backup_id:$id,
      offsite_uri:$offsite,
      manifest_sha256:$manifest_sha,
      verification_sha256:$verification_sha,
      ciphertext_sha256:$ciphertext_sha,
      completed_at:$completed
    }' > "$result_tmp"
  chmod 600 "$result_tmp"
  mv -f -- "$result_tmp" "$RESULT_FILE"
fi

log "done: $TS (encrypted offsite $DEST, encrypted local $STAGE)"
log "restore: scripts/restore.sh $TS    (or --latest) · see docs/RUNBOOKS.md"
