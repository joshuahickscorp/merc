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
command -v jq >/dev/null || die "jq is required"
command -v sha256sum >/dev/null || die "sha256sum is required"
[ -f .env ] || die "copy .env.example to .env and set production values"
set -a
. ./.env
set +a

required=(POSTGRES_PASSWORD MINIO_ROOT_USER MINIO_ROOT_PASSWORD ACME_EMAIL SITE_HOST
  S3_PUBLIC_ENDPOINT MERC_PUBLIC_CONTROL_ORIGIN MERC_TOKEN_KEY
  MERC_VERIFICATION_SAMPLE_SECRET MERC_SETTLEMENT_CURRENCY
  MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE MERC_PRICE_FX_REVISION
  MERC_ECON_SCHEDULE_VERSION MERC_PROCESSOR_PERCENT_BPS
  MERC_PROCESSOR_FIXED_USD MERC_CONTROL_PLANE_PER_BATCH_USD \
  MERC_MIN_CONTRIBUTION_PER_BATCH_USD MERC_TARGET_MARGIN_BPS
  MERC_ALERT_RECEIVER_URL_FILE GF_SECURITY_ADMIN_PASSWORD)
for name in "${required[@]}"; do [ -n "${!name:-}" ] || die "$name is required"; done

payment_mode="${MERC_PAYMENT_MODE:-sealed}"
case "$payment_mode" in
  sealed)
    for name in STRIPE_SECRET_KEY STRIPE_SECRET_KEY_SOURCE STRIPE_SECRET_KEY_FILE \
      STRIPE_SECRET_KEY_CONTAINER_FILE STRIPE_WEBHOOK_SECRET MERC_CONNECT_WEBHOOK_SECRET \
      MERC_CONNECT_CLIENT_ID MERC_PAYOUT_EXPORT MERC_LIVE_PAYMENT_ACTIVATION_SOURCE \
      MERC_LIVE_PAYMENT_ACTIVATION_SHA256 MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY \
      MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_SOURCE \
      MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_FILE \
      MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_CONTAINER_FILE; do
      [ -z "${!name:-}" ] || die "SEALED production forbids $name"
    done
    export MERC_PAYMENT_MODE=sealed
    export MERC_PAYMENT_PROVIDER=none
    export STRIPE_SECRET_KEY_CONTAINER_FILE=
    export MERC_LIVE_PAYMENT_ACTIVATION_CONTAINER_FILE=
    export MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_CONTAINER_FILE=
    ;;
  test)
    die "bootstrap-prod refuses TEST money; use the supervised staging/canary workflow"
    ;;
  live)
    [ -z "${STRIPE_SECRET_KEY:-}" ] \
      || die "LIVE mode forbids inline STRIPE_SECRET_KEY; use STRIPE_SECRET_KEY_SOURCE"
    stripe_secret_source="${STRIPE_SECRET_KEY_SOURCE:-}"
    [ "${stripe_secret_source#/}" != "$stripe_secret_source" ] \
      || die "STRIPE_SECRET_KEY_SOURCE must be an absolute path"
    [ -f "$stripe_secret_source" ] || die "Stripe secret key file does not exist"
    stripe_secret_mode="$(stat -c '%a' "$stripe_secret_source")"
    [ $(( 8#$stripe_secret_mode & 027 )) -eq 0 ] \
      || die "Stripe secret key file must not be group-writable or accessible to other users"
    stripe_secret="$(tr -d '\r\n' < "$stripe_secret_source")"
    [[ "$stripe_secret" == sk_live_* || "$stripe_secret" == rk_live_* ]] \
      || die "LIVE mode requires a live Stripe secret or restricted key file"
    [ "${#stripe_secret}" -le 16384 ] || die "Stripe secret key file is too large"
    [[ "${STRIPE_WEBHOOK_SECRET:-}" == whsec_* ]] || die "invalid billing webhook secret"
    [[ "${MERC_CONNECT_WEBHOOK_SECRET:-}" == whsec_* ]] || die "invalid Connect webhook secret"
    [ "$STRIPE_WEBHOOK_SECRET" != "$MERC_CONNECT_WEBHOOK_SECRET" ] || die "webhook secrets must differ"
    [[ "${MERC_CONNECT_CLIENT_ID:-}" == ca_* ]] || die "MERC_CONNECT_CLIENT_ID must be ca_*"
    connect_origin="https://${SITE_HOST%.}"
    for name in MERC_CONNECT_RETURN_URL MERC_CONNECT_REFRESH_URL; do
      case "${!name:-}" in
        "$connect_origin"|"$connect_origin"/*) ;;
        *) die "$name must use the SITE_HOST HTTPS origin" ;;
      esac
    done
    activation_source="${MERC_LIVE_PAYMENT_ACTIVATION_SOURCE:-}"
    [ "${activation_source#/}" != "$activation_source" ] \
      || die "MERC_LIVE_PAYMENT_ACTIVATION_SOURCE must be an absolute path"
    [ -f "$activation_source" ] || die "live payment activation file does not exist"
    activation_mode="$(stat -c '%a' "$activation_source")"
    [ $(( 8#$activation_mode & 027 )) -eq 0 ] \
      || die "live payment activation file must not be group-writable or accessible to other users"
    activation_digest="$(sha256sum "$activation_source" | awk '{print $1}')"
    [ "$activation_digest" = "${MERC_LIVE_PAYMENT_ACTIVATION_SHA256:-}" ] \
      || die "live payment activation digest does not match"
    [ -z "${MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY:-}" ] \
      || die "LIVE mode forbids inline MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY; use the key source file"
    activation_hmac_key_source="${MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_SOURCE:-}"
    [ "${activation_hmac_key_source#/}" != "$activation_hmac_key_source" ] \
      || die "MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_SOURCE must be an absolute path"
    [ -f "$activation_hmac_key_source" ] || die "live payment activation HMAC key file does not exist"
    activation_hmac_key_mode="$(stat -c '%a' "$activation_hmac_key_source")"
    [ $(( 8#$activation_hmac_key_mode & 027 )) -eq 0 ] \
      || die "live payment activation HMAC key file must not be group-writable or accessible to other users"
    activation_hmac_key="$(tr -d '\r\n' < "$activation_hmac_key_source")"
    [ "${#activation_hmac_key}" -ge 32 ] \
      || die "live payment activation HMAC key file is too short"
    [ "${#activation_hmac_key}" -le 4096 ] \
      || die "live payment activation HMAC key file is too large"
    candidate_commit="$(git rev-parse HEAD)"
    jq -e --arg commit "$candidate_commit" \
      --arg currency "$(printf '%s' "$MERC_SETTLEMENT_CURRENCY" | tr '[:upper:]' '[:lower:]')" '
        .schema_version == 1 and
        .activation.candidate_commit == $commit and
        .activation.environment == "production" and
        .activation.currency == $currency and
        (.activation.external_aggregate_cap_ref | type == "string" and length > 0) and
        (.activation.max_single_charge_minor | type == "number" and . > 0) and
        (.activation.max_single_payout_minor | type == "number" and . > 0) and
        (.activation.max_single_refund_minor | type == "number" and . > 0) and
        (.activation.max_single_reversal_minor | type == "number" and . > 0) and
        ([.activation.approvals[].role] | sort) == ["payments","release_manager","security"]
      ' "$activation_source" >/dev/null || die "live payment activation is not bound to this candidate/currency/cap/approval set"
    export MERC_PAYMENT_PROVIDER=stripe
    export STRIPE_SECRET_KEY_CONTAINER_FILE=/run/secrets/merc-stripe-secret-key
    export MERC_LIVE_PAYMENT_ACTIVATION_CONTAINER_FILE=/run/secrets/merc-live-payment-activation.json
    export MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_CONTAINER_FILE=/run/secrets/merc-live-payment-activation-hmac-key
    ;;
  *)
    die "MERC_PAYMENT_MODE must be sealed, test, or live"
    ;;
esac

[ "${#MERC_TOKEN_KEY}" -ge 32 ] || die "MERC_TOKEN_KEY is too short"
[ "${#MERC_VERIFICATION_SAMPLE_SECRET}" -ge 32 ] || die "verification secret is too short"
[ -f "$MERC_ALERT_RECEIVER_URL_FILE" ] && [ -s "$MERC_ALERT_RECEIVER_URL_FILE" ] \
  || die "MERC_ALERT_RECEIVER_URL_FILE must exist and contain the HTTPS alert webhook URL (see ops/monitoring/README.md)"
docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml config -q \
  || die "invalid production+observability compose"

echo "Deploy ${SITE_HOST} from $(git rev-parse --short HEAD); payment_mode=$payment_mode backup=$((1-SKIP_BACKUP))"
if [ "$YES" -eq 0 ]; then read -r -p 'Type yes to continue: ' answer; [ "$answer" = yes ] || exit 1; fi
if [ "$PULL" -eq 1 ]; then git pull --ff-only; fi
if [ "$SKIP_BACKUP" -eq 0 ] && docker compose -f docker-compose.prod.yml -f docker-compose.observability.yml ps -q postgres | grep -q .; then
  bash scripts/backup.sh
fi
bash scripts/deploy.sh
curl -fsS "https://${SITE_HOST}/readyz" >/dev/null
echo "bootstrap: deployment ready"
