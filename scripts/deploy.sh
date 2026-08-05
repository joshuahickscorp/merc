#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/compose-prod.sh
. "$ROOT/scripts/lib/compose-prod.sh"
PULL=0
[ "${1:-}" != --pull ] || PULL=1
[ $# -le 1 ] || { echo "usage: scripts/deploy.sh [--pull]" >&2; exit 2; }

die() { echo "deploy: $*" >&2; exit 1; }
command -v docker >/dev/null || die "docker is required"
command -v jq >/dev/null || die "jq is required"
[ -f "$ROOT/.env" ] || die ".env is required"
[ -z "$(git -C "$ROOT" status --porcelain)" ] || die "worktree must be clean"
[ "$PULL" -eq 0 ] || git -C "$ROOT" pull --ff-only

set -a
# shellcheck disable=SC1091
. "$ROOT/.env"
set +a

cx_require_alert_receiver_secret || exit 1
[ -n "${GF_SECURITY_ADMIN_PASSWORD:-}" ] || die "GF_SECURITY_ADMIN_PASSWORD is required for the observability stack (see ops/monitoring/README.md)"

export MERC_BUILD_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
export MERC_BUILD_VERSION="${MERC_BUILD_VERSION:-$(git -C "$ROOT" describe --tags --always)}"
export MERC_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
# Keep backup health dir consistent with scripts/backup.sh / systemd timer.
export MERC_BACKUP_STATUS_FILE="${MERC_BACKUP_STATUS_FILE:-$ROOT/.artifacts/backup-health/last-successful-offsite-backup.unixtime}"
export MERC_BACKUP_HEALTH_DIR="${MERC_BACKUP_HEALTH_DIR:-$(dirname -- "$MERC_BACKUP_STATUS_FILE")}"
mkdir -p "$MERC_BACKUP_HEALTH_DIR"

dc() { cx_prod_compose "$@"; }
dc config -q || die "invalid compose configuration (prod + observability)"
dc build || die "image build failed"
dc up -d --remove-orphans || die "compose rollout failed"

deadline=$(( $(date +%s) + 300 ))
for service in postgres minio control caddy alertmanager prometheus grafana node-exporter; do
  while :; do
    cid="$(dc ps -q "$service")"
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
    case "$state" in healthy|running) break;; esac
    [ "$(date +%s)" -lt "$deadline" ] || die "$service did not become healthy"
    sleep 3
  done
done

SITE_HOST="${SITE_HOST:-mercmerc.net}"
curl -fsS "https://$SITE_HOST/healthz" >/dev/null || die "public health check failed"
reported="$(curl -fsS "https://$SITE_HOST/version" | jq -r .commit)"
[ "$reported" = "$MERC_BUILD_COMMIT" ] || die "deployed commit $reported does not match $MERC_BUILD_COMMIT"
echo "deploy: $MERC_BUILD_COMMIT is healthy"
dc ps
