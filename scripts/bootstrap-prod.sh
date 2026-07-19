#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

PULL=0
SKIP_BACKUP=0
YES=0
for flag in "$@"; do
  case "$flag" in
    --pull) PULL=1 ;;
    --skip-backup) SKIP_BACKUP=1 ;;
    --yes|-y) YES=1 ;;
    *) echo "unknown flag: $flag" >&2; exit 2 ;;
  esac
done

die() { echo "bootstrap: $*" >&2; exit 1; }
command -v docker >/dev/null || die "docker is required"
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
[ -f .env ] || die "copy .env.example to .env and set production values"
set -a
. ./.env
set +a

required=(POSTGRES_PASSWORD MINIO_ROOT_USER MINIO_ROOT_PASSWORD ACME_EMAIL SITE_HOST
  S3_PUBLIC_ENDPOINT CX_PUBLIC_CONTROL_ORIGIN STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET
  CX_CONNECT_WEBHOOK_SECRET CX_CONNECT_RETURN_URL CX_CONNECT_REFRESH_URL CX_TOKEN_KEY
  CX_VERIFICATION_SAMPLE_SECRET CX_ECON_SCHEDULE_VERSION CX_PROCESSOR_PERCENT_BPS
  CX_PROCESSOR_FIXED_USD CX_CONTROL_PLANE_PER_TASK_USD CX_TARGET_MARGIN_BPS)
for name in "${required[@]}"; do [ -n "${!name:-}" ] || die "$name is required"; done
[[ "$STRIPE_SECRET_KEY" == sk_live_* ]] || die "production requires a live Stripe key"
[[ "$STRIPE_WEBHOOK_SECRET" == whsec_* ]] || die "invalid billing webhook secret"
[[ "$CX_CONNECT_WEBHOOK_SECRET" == whsec_* ]] || die "invalid Connect webhook secret"
[ "$STRIPE_WEBHOOK_SECRET" != "$CX_CONNECT_WEBHOOK_SECRET" ] || die "webhook secrets must differ"
[ "${#CX_TOKEN_KEY}" -ge 32 ] || die "CX_TOKEN_KEY is too short"
[ "${#CX_VERIFICATION_SAMPLE_SECRET}" -ge 32 ] || die "verification secret is too short"
docker compose -f docker-compose.prod.yml config -q || die "invalid production compose"

echo "Deploy ${SITE_HOST} from $(git rev-parse --short HEAD); backup=$((1-SKIP_BACKUP))"
if [ "$YES" -eq 0 ]; then read -r -p 'Type yes to continue: ' answer; [ "$answer" = yes ] || exit 1; fi
if [ "$PULL" -eq 1 ]; then git pull --ff-only; fi
if [ "$SKIP_BACKUP" -eq 0 ] && docker compose -f docker-compose.prod.yml ps -q postgres | grep -q .; then
  bash scripts/backup.sh
fi
bash scripts/deploy.sh
curl -fsS "https://${SITE_HOST}/readyz" >/dev/null
echo "bootstrap: deployment ready"
