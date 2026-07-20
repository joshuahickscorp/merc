#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
COMPOSE="${CX_COMPOSE_FILE:-$ROOT/docker-compose.prod.yml}"
TARGET="${1:-}"

die() { echo "rollback: $*" >&2; exit 1; }
[ $# -eq 1 ] || die "usage: scripts/rollback.sh <40-character-commit>"
[[ "$TARGET" =~ ^[0-9a-f]{40}$ ]] || die "target must be a full lowercase commit hash"
command -v docker >/dev/null 2>&1 || die "docker is required"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
command -v jq >/dev/null 2>&1 || die "jq is required"
[ -f "$ROOT/.env" ] || die ".env is required"

IMAGE="computexchange/control:$TARGET"
docker image inspect "$IMAGE" >/dev/null 2>&1 || die "rollback image is not present locally: $IMAGE"
export CX_BUILD_COMMIT="$TARGET"
dc() { docker compose -f "$COMPOSE" "$@"; }
dc config -q || die "invalid compose configuration"

# Schema migrations are additive. Roll back only the application image; never
# reverse ledger rows or drop schema objects during an incident.
dc up -d --no-build control caddy || die "rollback rollout failed"
deadline=$(( $(date +%s) + 180 ))
while :; do
  cid="$(dc ps -q control)"
  state="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$cid" 2>/dev/null || true)"
  [ "$state" = healthy ] && break
  [ "$(date +%s)" -lt "$deadline" ] || die "control did not become healthy"
  sleep 3
done

set -a
. "$ROOT/.env"
set +a
SITE_HOST="${SITE_HOST:-computexchange.net}"
reported="$(curl -fsS "https://$SITE_HOST/version" | jq -r .commit)"
[ "$reported" = "$TARGET" ] || die "public version is $reported, expected $TARGET"
echo "rollback: $TARGET is healthy"
