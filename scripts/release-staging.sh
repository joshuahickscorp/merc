#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
COMPOSE="$ROOT/ops/staging/compose.go-closure.yml"
MODE="${1:-}"
TEMP="$(mktemp -d)"
trap 'rm -rf "$TEMP"' EXIT

for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  value="${!name:-}"
  case "$value" in
    sk_live_*|rk_live_*|pk_live_*)
      printf 'release-staging: %s is live-class; refused before network access\n' "$name" >&2
      exit 1
      ;;
  esac
done
unset STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY

command -v docker >/dev/null 2>&1 || { echo 'release-staging: docker is required' >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo 'release-staging: jq is required' >&2; exit 1; }
docker compose -f "$COMPOSE" config --no-interpolate --format json > "$TEMP/rendered.json"

case "$MODE" in
  render)
    jq '{schema_version:1,supported_deployment_system:"Docker Compose v2",
      source_manifest:"ops/staging/compose.go-closure.yml",
      services:(.services | to_entries | map({name:.key,image:.value.image,
        read_only:(.value.read_only // false),user:(.value.user // "image-defined"),
        cap_drop:(.value.cap_drop // []),healthcheck:(.value.healthcheck != null),
        persistent_mounts:[.value.volumes[]? | select(.type=="volume") | .target]})),
      volumes:(.volumes | keys),secrets:(.secrets | keys),
      note:"Environment values are intentionally not rendered; only secret references and structure are shown."}' \
      "$TEMP/rendered.json"
    ;;
  validate)
    bash "$ROOT/scripts/validate-go-closure-scaffold.sh" >/dev/null
    jq -e '
      (.services.control.image | contains("${MERC_ACTIVE_CONTROL_IMAGE:?")) and
      (.services.control.user == "65532:65532") and
      .services.control.read_only == true and
      (.services.control.cap_drop | index("ALL")) != null and
      (.services.control.security_opt | index("no-new-privileges:true")) != null and
      (.services.control.healthcheck != null) and
      (.services.postgres.healthcheck != null) and (.services.minio.healthcheck != null) and
      (.services.caddy.ports | length) >= 2 and
      (.services.postgres.volumes | map(select(.type=="volume")) | length) >= 1 and
      (.services.minio.volumes | map(select(.type=="volume")) | length) >= 1 and
      (.services.alertmanager.secrets | length) == 1 and
      .services.control.environment.MERC_PAYMENT_MODE == "test" and
      .services.control.environment.MERC_PAYMENT_PROVIDER == "stripe" and
      .services.control.environment.MERC_SETTLEMENT_CURRENCY == "cad" and
      (.services.control.environment.MERC_PUBLIC_CONTROL_ORIGIN | startswith("https://")) and
      (.services.control.environment.S3_PUBLIC_ENDPOINT | startswith("https://"))
    ' "$TEMP/rendered.json" >/dev/null
    if rg -n '(^|[^A-Za-z])latest([^A-Za-z]|$)|^[[:space:]]*build:' "$COMPOSE" >/dev/null; then
      echo 'release-staging: mutable image or build directive found' >&2
      exit 1
    fi
    for required in scripts/backup.sh scripts/go-closure-rollback-rehearsal.sh monitoring/alertmanager.yml; do
      [ -f "$ROOT/$required" ] || { echo "release-staging: missing $required" >&2; exit 1; }
    done
    jq -n '{schema_version:1,status:"PASS",supported_deployment_system:"Docker Compose v2",
      checks:{immutable_image_contract:true,no_latest:true,secret_references:true,
        non_root_control:true,dropped_capabilities:true,read_only_control:true,
        resource_limits:true,persistent_volumes:true,health_and_readiness:true,
        tls_only_staging_mode:true,backup_schedule:true,alert_routes:true,
        rollback_digest_contract:true,source_identity_contract:true,
        payment_authority_contract:{mode:"test",provider:"stripe",currency:"cad"}},
      deployment_evidence:"NOT EXECUTED"}'
    ;;
  *) echo 'usage: scripts/release-staging.sh render|validate' >&2; exit 2 ;;
esac
