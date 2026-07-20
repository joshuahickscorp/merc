#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="${CX_COMPOSE_FILE:-$ROOT/docker-compose.prod.yml}"
PULL=0
[ "${1:-}" != --pull ] || PULL=1
[ $# -le 1 ] || { echo "usage: scripts/deploy.sh [--pull]" >&2; exit 2; }

die() { echo "deploy: $*" >&2; exit 1; }
command -v docker >/dev/null || die "docker is required"
command -v jq >/dev/null || die "jq is required"
[ -f "$ROOT/.env" ] || die ".env is required"
[ -z "$(git -C "$ROOT" status --porcelain)" ] || die "worktree must be clean"
[ "$PULL" -eq 0 ] || git -C "$ROOT" pull --ff-only

export CX_BUILD_COMMIT="$(git -C "$ROOT" rev-parse HEAD)"
export CX_BUILD_VERSION="${CX_BUILD_VERSION:-$(git -C "$ROOT" describe --tags --always)}"
export CX_BUILD_DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
dc() { docker compose -f "$COMPOSE" "$@"; }
dc config -q || die "invalid compose configuration"
dc build || die "image build failed"
dc up -d --remove-orphans || die "compose rollout failed"

deadline=$(( $(date +%s) + 180 ))
for service in postgres minio control caddy; do
  while :; do
    cid="$(dc ps -q "$service")"
    state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
    case "$state" in healthy|running) break;; esac
    [ "$(date +%s)" -lt "$deadline" ] || die "$service did not become healthy"
    sleep 3
  done
done

set -a
. "$ROOT/.env"
set +a
SITE_HOST="${SITE_HOST:-computexchange.net}"
curl -fsS "https://$SITE_HOST/healthz" >/dev/null || die "public health check failed"
reported="$(curl -fsS "https://$SITE_HOST/version" | jq -r .commit)"
[ "$reported" = "$CX_BUILD_COMMIT" ] || die "deployed commit $reported does not match $CX_BUILD_COMMIT"
echo "deploy: $CX_BUILD_COMMIT is healthy"
dc ps
