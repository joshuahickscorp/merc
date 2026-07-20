#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for env_file in "$ROOT/.env" "${CX_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"; do
  [ -f "$env_file" ] || continue
  set -a
  # shellcheck disable=SC1090
  . "$env_file"
  set +a
done

DB_ONLY=0
[ "${1:-}" = "--db-only" ] && DB_ONLY=1

die() { echo "[backup] ERROR: $*" >&2; exit 1; }
log() { echo "[backup] $*"; }

OFFSITE="${CX_BACKUP_OFFSITE:-}"
[ -n "$OFFSITE" ] || die "CX_BACKUP_OFFSITE is unset. Set it (and the offsite \
S3 creds) in .env · see .env.example. Refusing to take a backup with nowhere \
offsite to put it."

COMPOSE_FILE="${CX_COMPOSE_FILE:-$ROOT/docker-compose.prod.yml}"
PG_SERVICE="${CX_PG_SERVICE:-postgres}"
PG_USER="${POSTGRES_USER:-cx}"
PG_DB="${POSTGRES_DB:-cx}"

AWS_ARGS=()
if [ -n "${CX_BACKUP_S3_ENDPOINT:-}" ]; then
  AWS_ARGS+=(--endpoint-url "$CX_BACKUP_S3_ENDPOINT")
fi

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v aws >/dev/null 2>&1 || die "aws CLI not found (install awscli; it \
speaks S3/Spaces/R2/B2). Offsite upload requires it."
command -v age >/dev/null 2>&1 || die "age not found; refusing to upload a plaintext backup"

[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
  || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (offsite bucket creds) not \
set. See .env.example. Refusing to back up with no way to authenticate offsite."
RECIPIENT="${CX_BACKUP_ENCRYPTION_RECIPIENT:-}"
[[ "$RECIPIENT" == age1* ]] || die "CX_BACKUP_ENCRYPTION_RECIPIENT must be an age1... public recipient"

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

TS="$(date -u +%Y%m%dT%H%M%SZ)"
STAGE="${CX_BACKUP_DIR:-$ROOT/.artifacts/backups}/$TS"
PAYLOAD="$STAGE/payload"
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
  --argjson bytes "$(wc -c < "$STAGE/backup.tar.age" | tr -d ' ')" \
  '{schema_version:1,backup_id:$id,cipher:"age-x25519",ciphertext_sha256:$sha,ciphertext_bytes:$bytes,created_at:$created}' \
  > "$STAGE/manifest.json"

DEST="${OFFSITE%/}/$TS"
log "ship -> $DEST"
if ! aws "${AWS_ARGS[@]}" s3 cp --only-show-errors --recursive "$STAGE" "$DEST"; then
  die "OFFSITE UPLOAD FAILED to $DEST. The local staging copy is at $STAGE but \
this backup is NOT safe (single host). Investigate creds/endpoint/network."
fi
aws "${AWS_ARGS[@]}" s3 ls "$DEST/backup.tar.age" >/dev/null \
  || die "post-upload verify failed: encrypted bundle not visible at $DEST."
VERIFY="$(mktemp -d "${TMPDIR:-/tmp}/cx-backup-verify.XXXXXX")"
trap 'rm -rf "$VERIFY"' EXIT
aws "${AWS_ARGS[@]}" s3 cp --only-show-errors "$DEST/backup.tar.age" "$VERIFY/backup.tar.age" \
  || die "independent post-upload download failed"
expected="$(cut -d' ' -f1 "$STAGE/backup.tar.age.sha256")"
actual="$(shasum -a 256 "$VERIFY/backup.tar.age" | cut -d' ' -f1)"
[ "$actual" = "$expected" ] || die "downloaded ciphertext checksum mismatch"
log "offsite verified: $DEST/backup.tar.age sha256=$expected"

# Optional low-cardinality health input for the control-plane metrics endpoint.
# Mount its parent directory read-only into the control container and set the
# same CX_BACKUP_STATUS_FILE path there.
if [ -n "${CX_BACKUP_STATUS_FILE:-}" ]; then
  STATUS_DIR="$(dirname -- "$CX_BACKUP_STATUS_FILE")"
  STATUS_TMP="${CX_BACKUP_STATUS_FILE}.tmp.$$"
  mkdir -p "$STATUS_DIR"
  umask 027
  date -u +%s > "$STATUS_TMP"
  chmod 0640 "$STATUS_TMP"
  mv -f -- "$STATUS_TMP" "$CX_BACKUP_STATUS_FILE"
  log "backup health timestamp updated: $CX_BACKUP_STATUS_FILE"
fi

KEEP="${CX_BACKUP_KEEP_LOCAL:-7}"
BASE="$(dirname "$STAGE")"
ls -1dt "$BASE"/*/ 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  log "prune local $old"; rm -rf "$old"
done

log "done: $TS (encrypted offsite $DEST, encrypted local $STAGE)"
log "restore: scripts/restore.sh $TS    (or --latest) · see docs/RUNBOOKS.md"
