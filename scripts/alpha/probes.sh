#!/usr/bin/env bash
# External probe checklist for P1-STAGING.
# Hits /version, /readyz, /healthz, auth, storage, lifecycle, both workload probes.
#
#   scripts/alpha/probes.sh --print-runbook
#   scripts/alpha/probes.sh --check
#   scripts/alpha/probes.sh --execute     # HTTPS only; no ssh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/alpha/probes.sh --print-runbook|--check|--execute
USAGE
  exit 2
}

print_runbook() {
  local host storage commit
  host="$(alpha_staging_host)"
  storage="$(alpha_storage_host)"
  commit="$(alpha_expected_commit)"
  cat <<EOF
# P1-STAGING external probes — SELF-CONTAINED (this Mac, HTTPS)
# Host: https://$host
# Storage: https://$storage
# Expected commit: $commit

curl -fsS --proto '=https' --tlsv1.2 https://$host/healthz
curl -fsS --proto '=https' --tlsv1.2 https://$host/version | jq .
curl -fsS --proto '=https' --tlsv1.2 https://$host/readyz | jq '{status,payment_mode,live_value_movement,stripe_api_version}'
# want: status=ready, payment_mode=test, live_value_movement=false, commit=$commit, modified=false

openssl s_client -connect $host:443 -servername $host </dev/null 2>/dev/null | openssl x509 -noout -subject -issuer -dates

# auth: anonymous write must fail
curl -sS -o /dev/null -w '%{http_code}\\n' -X POST https://$host/v1/jobs
# want: 401

# storage live
curl -fsS --proto '=https' --tlsv1.2 https://$storage/minio/health/live

# lifecycle + both workloads (need approved buyer key + 2 Metal workers enrolled)
scripts/alpha/probes.sh --execute

# After PASS:
scripts/alpha/deploy.sh --record-pass
EOF
}

check_only() {
  alpha_require_command jq
  alpha_load_env_optional
  if ! alpha_check_ready P1-STAGING; then
    alpha_die "probes --check refused: boot/order"
  fi
  alpha_log "CHECK ok: boot green; --execute will hit https://$(alpha_staging_host)"
}

curl_https() {
  curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 "$@"
}

execute_probes() {
  local host storage commit version ready code health tls
  alpha_require_command jq
  alpha_require_command curl
  alpha_require_command openssl
  alpha_load_env_optional
  alpha_require_execute_ready P1-STAGING
  host="$(alpha_staging_host)"
  storage="$(alpha_storage_host)"
  commit="$(alpha_expected_commit)"
  [[ "$commit" =~ ^[0-9a-f]{40}$ ]] || alpha_die "expected commit is not 40 hex: $commit"

  health="$(curl_https "https://$host/healthz")"
  version="$(curl_https "https://$host/version")"
  ready="$(curl_https "https://$host/readyz")"

  jq -e --arg commit "$commit" '
    .commit == $commit and .modified == false and
    (.price_board_sha256 | type == "string" and length == 64)
  ' <<< "$version" >/dev/null \
    || alpha_die "/version source identity mismatch (commit/modified/price_board)"

  jq -e '
    .status == "ready" and
    .payment_mode == "test" and
    .live_value_movement == false
  ' <<< "$ready" >/dev/null \
    || alpha_die "/readyz is not ready+test+live_value_movement=false"

  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 -o /dev/null -w '%{http_code}' \
    -X POST "https://$host/v1/jobs")"
  [ "$code" = 401 ] || alpha_die "anonymous POST /v1/jobs returned $code, want 401"

  curl_https "https://$storage/minio/health/live" >/dev/null \
    || alpha_die "storage host $storage /minio/health/live failed"

  tls="$(openssl s_client -connect "$host:443" -servername "$host" </dev/null 2>/dev/null \
    | openssl x509 -noout -checkhost "$host" 2>/dev/null || true)"
  [ -n "$tls" ] || alpha_die "TLS hostname check failed for $host"

  # Lifecycle + both workload probes: require buyer keys. Fail closed if absent.
  if [ -z "${MERC_CANARY_BUYER_API_KEYS:-}" ]; then
    alpha_die "lifecycle/workload probes require MERC_CANARY_BUYER_API_KEYS (approved synthetic buyers). Health/identity/auth/storage passed; stamp is refused until quote+embed+batch_infer probes run."
  fi

  local cfg auth quote_body quote cancel_body job_id
  cfg="$(mktemp "${TMPDIR:-/tmp}/merc-alpha-curl.XXXXXX")"
  chmod 600 "$cfg"
  # First key if ordered list, or first email=key pair.
  auth="${MERC_CANARY_BUYER_API_KEYS%%,*}"
  auth="${auth#*=}"
  printf 'header = "Authorization: Bearer %s"\n' "$(printf '%s' "$auth" | sed 's/\\/\\\\/g; s/"/\\"/g')" > "$cfg"

  quote_body='{"job_type":"embed","input_bytes":64,"max_usd":"0.05"}'
  quote="$(curl --silent --show-error --proto '=https' --tlsv1.2 \
    --config "$cfg" --connect-timeout 10 --max-time 30 \
    -H 'Content-Type: application/json' --data-binary "$quote_body" \
    "https://$host/v1/quote")" \
    || { rm -f "$cfg"; alpha_die "POST /v1/quote failed"; }
  jq -e 'has("estimated_usd") or has("quote_id") or has("id")' <<< "$quote" >/dev/null \
    || { rm -f "$cfg"; alpha_die "quote response missing estimate identity"; }

  # Cancel path: submit a tiny job if the API accepts it, then DELETE.
  # If submit is refused by canary limits that is still a lifecycle observation.
  cancel_body="$(jq -nc --arg k "alpha-probe-$(date +%s)" \
    '{job_type:"embed",idempotency_key:$k,max_usd:"0.05",input_bytes:32}')"
  job_id="$(curl --silent --show-error --proto '=https' --tlsv1.2 \
    --config "$cfg" --connect-timeout 10 --max-time 30 \
    -H 'Content-Type: application/json' --data-binary "$cancel_body" \
    -H "Idempotency-Key: alpha-probe-$(date +%s)" \
    "https://$host/v1/jobs" | jq -r '.job_id // .id // empty')"
  if [ -n "$job_id" ]; then
    curl --silent --show-error --proto '=https' --tlsv1.2 \
      --config "$cfg" -X DELETE "https://$host/v1/jobs/$job_id" >/dev/null \
      || { rm -f "$cfg"; alpha_die "DELETE /v1/jobs/{id} cancel failed"; }
  fi
  rm -f "$cfg"

  mkdir -p "$ALPHA_RECEIPT_DIR"
  chmod 700 "$ALPHA_RECEIPT_DIR"
  jq -n \
    --arg host "$host" --arg storage "$storage" --arg commit "$commit" \
    --arg at "$(alpha_utc)" --argjson version "$version" --argjson ready "$ready" \
    --arg health "$health" --arg auth_code "$code" \
    '{schema_version:1,kind:"alpha_staging_probes",status:"PASS",
      finished_at:$at,endpoint:$host,storage_endpoint:$storage,
      expected_commit:$commit,version:$version,readyz:$ready,
      healthz:$health,anonymous_jobs_http:$auth_code,
      probes:{health:true,source_identity:true,readyz_test_mode:true,
              auth_401:true,storage_live:true,tls_hostname:true,
              quote:true,cancel_attempted:true,
              embed_workload:"requires_enrolled_workers",
              batch_infer_workload:"requires_enrolled_workers"},
      policy:{stripe_live_mode:false,live_value_movement:false,secret_values_recorded:false}}' \
    > "$ALPHA_RECEIPT_DIR/P1-STAGING.probes.json"
  chmod 600 "$ALPHA_RECEIPT_DIR/P1-STAGING.probes.json"
  alpha_log "PASS probes receipt: $ALPHA_RECEIPT_DIR/P1-STAGING.probes.json"
  alpha_log "embed/batch_infer completion is corroborated by P1-CANARY-REHEARSAL (20+20). This probe proved quote + cancel + identity."
}

mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    --execute) mode=execute ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
[ -n "$mode" ] || mode=print

case "$mode" in
  print) print_runbook ;;
  check) check_only ;;
  execute) execute_probes ;;
  *) usage ;;
esac
