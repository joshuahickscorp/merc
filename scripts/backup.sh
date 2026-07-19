#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [ -f "$ROOT/.env" ]; then set -a; . "$ROOT/.env"; set +a; fi

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

[ -n "${AWS_ACCESS_KEY_ID:-}" ] && [ -n "${AWS_SECRET_ACCESS_KEY:-}" ] \
  || die "AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (offsite bucket creds) not \
set. See .env.example. Refusing to back up with no way to authenticate offsite."

dc() { docker compose -f "$COMPOSE_FILE" "$@"; }

TS="$(date -u +%Y%m%dT%H%M%SZ)"
STAGE="${CX_BACKUP_DIR:-$ROOT/.artifacts/backups}/$TS"
mkdir -p "$STAGE"

log "pg_dump (-Fc) $PG_DB -> $STAGE/db.dump"
if ! dc exec -T "$PG_SERVICE" pg_dump -U "$PG_USER" -d "$PG_DB" -Fc > "$STAGE/db.dump"; then
  die "pg_dump failed · see above. NO backup produced."
fi
[ -s "$STAGE/db.dump" ] || die "pg_dump produced an empty file · refusing to ship a zero-byte backup."
log "db.dump $(du -h "$STAGE/db.dump" | cut -f1)"

( cd "$STAGE" && shasum -a 256 db.dump > db.dump.sha256 )

if [ "$DB_ONLY" -eq 0 ]; then
  S3_BUCKET="${S3_BUCKET:-cx-jobs}"
  log "object store: mirror minio/$S3_BUCKET -> $STAGE/objects"
  if ! dc run --rm -T \
        -e MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@minio:9000" \
        --entrypoint sh minio/mc -c \
        "mc mirror --overwrite --remove local/${S3_BUCKET} /tmp/o && tar -C /tmp -cf - o" \
        > "$STAGE/objects.tar"; then
    die "object-store mirror failed · see above."
  fi
  log "objects.tar $(du -h "$STAGE/objects.tar" | cut -f1)"
  ( cd "$STAGE" && shasum -a 256 objects.tar > objects.tar.sha256 )
fi

DEST="${OFFSITE%/}/$TS"
log "ship -> $DEST"
if ! aws "${AWS_ARGS[@]}" s3 cp --recursive "$STAGE" "$DEST"; then
  die "OFFSITE UPLOAD FAILED to $DEST. The local staging copy is at $STAGE but \
this backup is NOT safe (single host). Investigate creds/endpoint/network."
fi
aws "${AWS_ARGS[@]}" s3 ls "$DEST/db.dump" >/dev/null \
  || die "post-upload verify failed: db.dump not visible at $DEST."
log "offsite verified: $DEST/db.dump present"

KEEP="${CX_BACKUP_KEEP_LOCAL:-7}"
BASE="$(dirname "$STAGE")"
ls -1dt "$BASE"/*/ 2>/dev/null | tail -n +"$((KEEP + 1))" | while read -r old; do
  log "prune local $old"; rm -rf "$old"
done

log "done: $TS (offsite $DEST, local $STAGE)"
log "restore: scripts/restore.sh $TS    (or --latest) · see docs/RUNBOOKS.md"
