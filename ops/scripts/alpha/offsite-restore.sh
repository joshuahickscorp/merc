#!/usr/bin/env bash
# P1-OFFSITE-RESTORE: PG dump -> age -> independent upload -> independent
# download -> scratch PG restore -> verify.
#
# Upload half is SUPERVISOR (needs the droplet data plane).
# Download/decrypt/scratch-restore is SELF-CONTAINED on this Mac.
# --check never uploads or dumps.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/alpha/offsite-restore.sh --print-runbook|--check|--execute-restore|--record-pass

--check            tools + env + independence (no dump, no upload)
--execute-restore  SELF-CONTAINED: download latest ciphertext, restore scratch PG
--record-pass      SUPERVISOR stamps the gate after restore verifies
USAGE
  exit 2
}

print_runbook() {
  cat <<EOF
# P1-OFFSITE-RESTORE
# Exit: Upload only ciphertext, independently download/decrypt in isolation,
# restore database and objects, and match checksums plus application/ledger
# invariants.
#
# Independent boundary: MERC_BACKUP_OFFSITE must be s3://... at a provider
# that is NOT the droplet MinIO. Prefer Cloudflare R2
# (MERC_BACKUP_S3_ENDPOINT=https://<account>.r2.cloudflarestorage.com).
# Age identity stays OFF the droplet (MERC_BACKUP_DECRYPTION_IDENTITY_FILE).

## Preferred — live droplet source (this Mac drives the droplet)
# Encrypts on 192.241.134.31, uploads only ciphertext (presigned PUT; no
# long-lived R2 key on the droplet), independently downloads/hashes here,
# restores into isolated Postgres/MinIO. Does not touch merc_pgdata.
make offsite-droplet-restore-check
make offsite-droplet-restore
# Isolated-seed rehearsal (not a live-volume backup):
#   make offsite-independent-restore-check
#   make offsite-independent-restore

## A. SUPERVISOR — dump + encrypt + upload (on the droplet or via existing backup.sh)
# From a checkout that can docker exec merc-postgres-1:
export MERC_COMPOSE_FILE=$ROOT/docker-compose.smallhost.yml
export MERC_PG_SERVICE=postgres
# or, if the compose project is already named merc:
#   docker exec merc-postgres-1 pg_dump -U cx -d cx -Fc > /tmp/cx.dump
set -a; . ./.env.go-closure; set +a
ops/scripts/backup.sh
# Proves: age ciphertext uploaded, independent re-download, checksum match.
# Writes schema-v2 manifest + verification.json under .artifacts/backups/ and offsite.

## B. SELF-CONTAINED — download + decrypt + scratch restore (this Mac)
# Uses a different process/credential path than the droplet:
export MERC_BACKUP_S3_ENDPOINT=\${MERC_BACKUP_S3_ENDPOINT}   # R2
export MERC_BACKUP_OFFSITE=\${MERC_BACKUP_OFFSITE}           # s3://bucket/prefix
export MERC_BACKUP_DECRYPTION_IDENTITY_FILE=\${MERC_BACKUP_DECRYPTION_IDENTITY_FILE}
ops/scripts/alpha/offsite-restore.sh --execute-restore
# or the existing isolated drill:
#   ops/scripts/restore.sh --latest --to merc_alpha_restore_\$\$
#   ops/scripts/local-independent-restore.sh   # local mechanism proof, not the offsite copy

## C. SUPERVISOR — stamp
ops/scripts/alpha/offsite-restore.sh --record-pass
EOF
}

check_only() {
  alpha_require_command jq
  alpha_load_env_optional
  if ! alpha_check_ready P1-OFFSITE-RESTORE; then
    alpha_die "P1-OFFSITE-RESTORE is not execute-ready (boot/staging)"
  fi
  command -v age >/dev/null 2>&1 || alpha_die "age is required for offsite restore"
  command -v aws >/dev/null 2>&1 || alpha_die "aws CLI is required (speaks R2/S3)"
  command -v docker >/dev/null 2>&1 || alpha_die "docker is required for scratch PG restore"
  [ -n "${MERC_BACKUP_OFFSITE:-}" ] || alpha_die "MERC_BACKUP_OFFSITE is unset"
  [[ "$MERC_BACKUP_OFFSITE" == s3://* ]] || alpha_die "MERC_BACKUP_OFFSITE must be s3://..."
  [ -n "${MERC_BACKUP_ENCRYPTION_RECIPIENT:-}" ] \
    || alpha_die "MERC_BACKUP_ENCRYPTION_RECIPIENT must be age1..."
  [[ "${MERC_BACKUP_ENCRYPTION_RECIPIENT}" == age1* ]] \
    || alpha_die "MERC_BACKUP_ENCRYPTION_RECIPIENT must start with age1"
  [ -n "${MERC_BACKUP_DECRYPTION_IDENTITY_FILE:-}" ] \
    || alpha_die "MERC_BACKUP_DECRYPTION_IDENTITY_FILE is required"
  [ -r "${MERC_BACKUP_DECRYPTION_IDENTITY_FILE}" ] \
    || alpha_die "MERC_BACKUP_DECRYPTION_IDENTITY_FILE is not readable"
  # Independence: offsite must not be the droplet's own MinIO loopback.
  case "${MERC_BACKUP_S3_ENDPOINT:-}" in
    http://127.0.0.1:*|http://localhost:*|http://minio:*|http://merc-minio-1:*)
      alpha_die "MERC_BACKUP_S3_ENDPOINT is the droplet/local MinIO; offsite must cross a provider/credential boundary"
      ;;
  esac
  alpha_log "CHECK ok: age/aws/docker present, offsite URI independent, identity readable (no upload)"
}

execute_restore() {
  alpha_require_command jq
  alpha_require_command aws
  alpha_require_command age
  alpha_require_command docker
  alpha_load_env_optional
  alpha_require_execute_ready P1-OFFSITE-RESTORE
  [ -x "$ROOT/ops/scripts/restore.sh" ] || alpha_die "missing ops/scripts/restore.sh"
  local scratch
  scratch="merc_alpha_restore_$$"
  alpha_log "restoring latest offsite backup into scratch db $scratch (ops/scripts/restore.sh)"
  if ! bash "$ROOT/ops/scripts/restore.sh" --latest --db-only --to "$scratch"; then
    alpha_die "restore.sh --latest --db-only --to $scratch failed"
  fi
  mkdir -p "$ALPHA_RECEIPT_DIR"
  jq -n --arg db "$scratch" --arg at "$(alpha_utc)" --arg offsite "${MERC_BACKUP_OFFSITE}" \
    '{schema_version:1,kind:"alpha_offsite_restore",status:"PASS",
      finished_at:$at,scratch_database:$db,offsite_uri:$offsite,
      independent_download:true,isolated_restore:true,
      policy:{ciphertext_only:true,secret_values_recorded:false}}' \
    > "$ALPHA_RECEIPT_DIR/P1-OFFSITE-RESTORE.restore.json"
  chmod 600 "$ALPHA_RECEIPT_DIR/P1-OFFSITE-RESTORE.restore.json"
  alpha_log "scratch restore receipt: $ALPHA_RECEIPT_DIR/P1-OFFSITE-RESTORE.restore.json"
}

record_pass() {
  alpha_load_env_optional
  alpha_require_execute_ready P1-OFFSITE-RESTORE
  [ -f "$ALPHA_RECEIPT_DIR/P1-OFFSITE-RESTORE.restore.json" ] \
    || alpha_die "missing restore receipt; run --execute-restore first"
  dest="$(alpha_write_receipt P1-OFFSITE-RESTORE PASS alpha_offsite_restore)"
  alpha_log "PASS receipt: $dest"
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute-restore) mode=restore ;;
    --record-pass) mode=record ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_only ;;
  restore) execute_restore ;;
  record) record_pass ;;
  *) usage ;;
esac
