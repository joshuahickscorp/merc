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

die() { echo "[restore] ERROR: $*" >&2; exit 1; }
log() { echo "[restore] $*"; }

WHICH=""; DB_ONLY=0; TARGET_DB=""
while [ $# -gt 0 ]; do
  case "$1" in
    --latest)  WHICH="latest" ;;
    --db-only) DB_ONLY=1 ;;
    --to)      shift; TARGET_DB="${1:-}"; [ -n "$TARGET_DB" ] || die "--to needs a db name" ;;
    --*)       die "unknown flag $1" ;;
    *)         WHICH="$1" ;;
  esac
  shift
done
[ -n "$WHICH" ] || die "specify a timestamp or --latest. List: aws s3 ls \$CX_BACKUP_OFFSITE/"

OFFSITE="${CX_BACKUP_OFFSITE:-}"
[ -n "$OFFSITE" ] || die "CX_BACKUP_OFFSITE unset (see .env.example)."
[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
  || die "offsite creds (AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY) not set."

COMPOSE_FILE="${CX_COMPOSE_FILE:-$ROOT/docker-compose.prod.yml}"
PG_SERVICE="${CX_PG_SERVICE:-postgres}"
PG_USER="${POSTGRES_USER:-cx}"
PG_DB="${POSTGRES_DB:-cx}"
RESTORE_DB="${TARGET_DB:-$PG_DB}"
S3_BUCKET="${S3_BUCKET:-cx-jobs}"

AWS_ARGS=()
[ -n "${CX_BACKUP_S3_ENDPOINT:-}" ] && AWS_ARGS+=(--endpoint-url "$CX_BACKUP_S3_ENDPOINT")

command -v docker >/dev/null 2>&1 || die "docker not found"
command -v aws >/dev/null 2>&1 || die "aws CLI not found"
command -v age >/dev/null 2>&1 || die "age not found"
IDENTITY="${CX_BACKUP_DECRYPTION_IDENTITY_FILE:-}"
[ -n "$IDENTITY" ] && [ -r "$IDENTITY" ] \
  || die "CX_BACKUP_DECRYPTION_IDENTITY_FILE must name a readable age identity"

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

if [ "$WHICH" = "latest" ]; then
  WHICH="$(aws "${AWS_ARGS[@]}" s3 ls "${OFFSITE%/}/" \
    | awk '/PRE/ {print $2}' | sed 's#/$##' | sort | tail -1)"
  [ -n "$WHICH" ] || die "no backups found under $OFFSITE"
  log "latest resolved -> $WHICH"
fi

SRC="${OFFSITE%/}/$WHICH"
STAGE="$(mktemp -d "${TMPDIR:-/tmp}/cx-restore.XXXXXX")"
trap 'rm -rf "$STAGE"' EXIT

log "fetch $SRC -> $STAGE"
aws "${AWS_ARGS[@]}" s3 cp --recursive "$SRC" "$STAGE" \
  || die "fetch from $SRC failed."
[ -s "$STAGE/backup.tar.age" ] || die "no encrypted backup.tar.age in fetched backup $SRC."
[ -s "$STAGE/backup.tar.age.sha256" ] || die "no ciphertext checksum in fetched backup $SRC."

log "verify encrypted bundle checksum and decrypt"
( cd "$STAGE" && shasum -a 256 -c backup.tar.age.sha256 ) \
  || die "encrypted bundle checksum MISMATCH  -  corrupt/incomplete backup. Aborting."
age --decrypt -i "$IDENTITY" -o "$STAGE/backup.tar" "$STAGE/backup.tar.age" \
  || die "backup decryption failed"
if tar -tf "$STAGE/backup.tar" | awk 'BEGIN{bad=0} /^\// || /(^|\/)\.\.($|\/)/ {bad=1} END{exit bad?0:1}'; then
  die "backup archive contains an unsafe path"
fi
mkdir "$STAGE/payload"
tar -C "$STAGE/payload" -xf "$STAGE/backup.tar" || die "backup archive extraction failed"
rm -f "$STAGE/backup.tar"
PAYLOAD="$STAGE/payload"
[ -s "$PAYLOAD/db.dump" ] || die "decrypted bundle has no db.dump."
( cd "$PAYLOAD" && shasum -a 256 -c db.dump.sha256 ) \
  || die "db.dump checksum MISMATCH  -  corrupt/incomplete backup. Aborting."

if [ "$RESTORE_DB" != "$PG_DB" ]; then
  log "DR drill mode: restoring into scratch db '$RESTORE_DB' (live '$PG_DB' untouched)"
  dc exec -T "$PG_SERVICE" psql -U "$PG_USER" -d "$PG_DB" \
     -v ON_ERROR_STOP=1 -c "DROP DATABASE IF EXISTS \"$RESTORE_DB\"" \
     -c "CREATE DATABASE \"$RESTORE_DB\"" \
     || die "could not create scratch db $RESTORE_DB"
fi

log "pg_restore -> db '$RESTORE_DB'"
if ! dc exec -T "$PG_SERVICE" pg_restore -U "$PG_USER" -d "$RESTORE_DB" \
        --clean --if-exists --no-owner --no-privileges -1 < "$PAYLOAD/db.dump"; then
  die "pg_restore FAILED  -  db '$RESTORE_DB' rolled back (transaction). Nothing partially applied."
fi
log "DB restored into '$RESTORE_DB'"

if [ "$DB_ONLY" -eq 0 ] && [ "$RESTORE_DB" = "$PG_DB" ]; then
  if [ -s "$PAYLOAD/objects.tar" ]; then
    ( cd "$PAYLOAD" && shasum -a 256 -c objects.tar.sha256 ) \
      || die "objects.tar checksum MISMATCH. Aborting object restore."
    log "object store: mirror archive -> minio/$S3_BUCKET"
    if ! dc run --rm -T \
          -e MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@minio:9000" \
          -v "$PAYLOAD/objects.tar:/tmp/objects.tar:ro" \
          --entrypoint sh minio/mc -c \
          "mkdir -p /tmp/x && tar -C /tmp/x -xf /tmp/objects.tar && mc mb -p local/${S3_BUCKET} && mc mirror --overwrite /tmp/x/o local/${S3_BUCKET}"; then
      die "object-store restore failed."
    fi
    log "objects restored into minio/$S3_BUCKET"
  else
    log "no objects.tar in this backup (db-only backup?)  -  skipping object restore"
  fi
fi

log "restore complete from $SRC into db '$RESTORE_DB'"
if [ "$RESTORE_DB" = "$PG_DB" ]; then
  log "restart the control instances so they re-verify deps:"
  log "  docker compose -f $COMPOSE_FILE restart control"
else
  log "DR drill: inspect with  docker compose -f $COMPOSE_FILE exec $PG_SERVICE psql -U $PG_USER -d $RESTORE_DB -c 'SELECT count(*) FROM jobs;'"
fi
