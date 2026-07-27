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
  S3_PUBLIC_ENDPOINT MERC_PUBLIC_CONTROL_ORIGIN STRIPE_SECRET_KEY STRIPE_WEBHOOK_SECRET
  MERC_CONNECT_WEBHOOK_SECRET MERC_CONNECT_RETURN_URL MERC_CONNECT_REFRESH_URL MERC_TOKEN_KEY
  MERC_VERIFICATION_SAMPLE_SECRET MERC_ECON_SCHEDULE_VERSION MERC_PROCESSOR_PERCENT_BPS
  MERC_PROCESSOR_FIXED_USD MERC_CONTROL_PLANE_PER_TASK_USD MERC_TARGET_MARGIN_BPS
  MERC_ALERT_RECEIVER_URL_FILE GF_SECURITY_ADMIN_PASSWORD)
for name in "${required[@]}"; do [ -n "${!name:-}" ] || die "$name is required"; done
[[ "$STRIPE_SECRET_KEY" == sk_live_* ]] || die "production requires a live Stripe key"
[[ "$STRIPE_WEBHOOK_SECRET" == whsec_* ]] || die "invalid billing webhook secret"
[[ "$MERC_CONNECT_WEBHOOK_SECRET" == whsec_* ]] || die "invalid Connect webhook secret"
[ "$STRIPE_WEBHOOK_SECRET" != "$MERC_CONNECT_WEBHOOK_SECRET" ] || die "webhook secrets must differ"
[ "${#MERC_TOKEN_KEY}" -ge 32 ] || die "MERC_TOKEN_KEY is too short"
[ "${#MERC_VERIFICATION_SAMPLE_SECRET}" -ge 32 ] || die "verification secret is too short"
[ -f "$MERC_ALERT_RECEIVER_URL_FILE" ] && [ -s "$MERC_ALERT_RECEIVER_URL_FILE" ] \
  || die "MERC_ALERT_RECEIVER_URL_FILE must exist and contain the HTTPS alert webhook URL (see monitoring/README.md)"
docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml config -q \
  || die "invalid production+observability compose"

echo "Deploy ${SITE_HOST} from $(git rev-parse --short HEAD); backup=$((1-SKIP_BACKUP))"
if [ "$YES" -eq 0 ]; then read -r -p 'Type yes to continue: ' answer; [ "$answer" = yes ] || exit 1; fi
if [ "$PULL" -eq 1 ]; then git pull --ff-only; fi
if [ "$SKIP_BACKUP" -eq 0 ] && docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml ps -q postgres | grep -q .; then
  bash scripts/backup.sh
fi
bash scripts/deploy.sh
curl -fsS "https://${SITE_HOST}/readyz" >/dev/null
echo "bootstrap: deployment ready"
