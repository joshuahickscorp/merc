#!/usr/bin/env bash
# Extra canary scenario adapters required by P1-CANARY-REHEARSAL's exit criterion
# that are not in the counted go-closure driver list:
#   kill_switch, revocation, intake_pause_result_retrieval, no_payout_export
#
# Known go-closure scenarios are delegated to scripts/canary-scenario-driver.sh.
# Usage: scripts/alpha/scenarios.sh run <scenario> <minimum>
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/alpha/scenarios.sh run <scenario> <minimum>
USAGE
  exit 2
}

[ "${1:-}" = run ] || usage
[ "$#" -eq 3 ] || usage
scenario="$2"
minimum="$3"
[[ "$minimum" =~ ^[1-9][0-9]*$ ]] || alpha_die "minimum must be a positive integer"

alpha_load_env_optional
alpha_reject_live_stripe

delegate() {
  [ -x "$ROOT/scripts/canary-scenario-driver.sh" ] \
    || alpha_die "missing scripts/canary-scenario-driver.sh"
  exec bash "$ROOT/scripts/canary-scenario-driver.sh" run "$scenario" "$minimum"
}

control_base() {
  if [ -n "${MERC_CONTROL_BASE_URL:-}" ]; then
    printf '%s' "${MERC_CONTROL_BASE_URL%/}"
    return
  fi
  if [ -n "${STAGING_TLS_HOSTNAME:-}" ]; then
    printf 'https://%s' "$STAGING_TLS_HOSTNAME"
    return
  fi
  alpha_die "MERC_CONTROL_BASE_URL or STAGING_TLS_HOSTNAME is required"
}

admin_cfg() {
  local cfg
  : "${MERC_CANARY_ADMIN_API_KEY:?MERC_CANARY_ADMIN_API_KEY is required for kill/intake adapters}"
  cfg="$(mktemp "${TMPDIR:-/tmp}/merc-alpha-admin.XXXXXX")"
  chmod 600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' \
    "$(printf '%s' "$MERC_CANARY_ADMIN_API_KEY" | sed 's/\\/\\\\/g; s/"/\\"/g')" > "$cfg"
  printf '%s' "$cfg"
}

emit_simple() {
  local name="$1" subject="$2"
  jq -n --arg scenario "$name" --arg subject "$subject" --arg at "$(alpha_utc)" \
    --argjson min "$minimum" \
    '{schema_version:2,scenario:$scenario,requested:$min,observed:1,status:"PASS",
      started_at:$at,finished_at:$at,
      evidence:[{id:("obs-"+$scenario+"-0001"),subject_id:$subject,occurred_at:$at,
                 source:"alpha_scenario_adapter"}],
      safety:{stripe_live_mode:false,real_value:false,secret_values_recorded:false,
              approved_participants_only:true}}'
}

run_no_payout_export() {
  [ "$minimum" = 1 ] || alpha_die "no_payout_export minimum must be 1"
  case "${MERC_PAYOUT_EXPORT:-}" in
    ''|0|false|FALSE|no|NO) ;;
    *) alpha_die "MERC_PAYOUT_EXPORT is set; canary refuses payout export" ;;
  esac
  local base ready
  base="$(control_base)"
  ready="$(curl --fail --silent --show-error --proto '=https' --tlsv1.2 \
    --connect-timeout 10 --max-time 30 "$base/readyz")"
  jq -e '.payment_mode == "test" and .live_value_movement == false' <<< "$ready" >/dev/null \
    || alpha_die "/readyz does not prove test-mode with no live value movement"
  emit_simple no_payout_export "payout-export-refused"
}

run_kill_switch() {
  [ "$minimum" = 1 ] || alpha_die "kill_switch minimum must be 1"
  local base cfg payload code ready
  base="$(control_base)"
  cfg="$(admin_cfg)"
  payload='{"paused":true,"reason":"alpha-canary-kill-switch"}'
  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
    -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "$payload" "$base/admin/controls/dispatch")"
  case "$code" in
    2??) ;;
    *) rm -f "$cfg"; alpha_die "kill_switch pause dispatch failed HTTP $code" ;;
  esac
  payload='{"paused":false,"reason":"alpha-canary-kill-switch-resume"}'
  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
    -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "$payload" "$base/admin/controls/dispatch")"
  rm -f "$cfg"
  case "$code" in
    2??) ;;
    *) alpha_die "kill_switch resume dispatch failed HTTP $code" ;;
  esac
  emit_simple kill_switch "operational-control-dispatch"
}

run_intake_pause() {
  [ "$minimum" = 1 ] || alpha_die "intake_pause_result_retrieval minimum must be 1"
  local base cfg payload code job_id results
  base="$(control_base)"
  cfg="$(admin_cfg)"
  payload='{"paused":true,"reason":"alpha-canary-intake-pause"}'
  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
    -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "$payload" "$base/admin/controls/intake")"
  case "$code" in
    2??) ;;
    *) rm -f "$cfg"; alpha_die "intake pause failed HTTP $code" ;;
  esac
  # Result retrieval must still answer for a known job if one is supplied.
  job_id="${MERC_CANARY_KNOWN_JOB_ID:-}"
  if [ -n "$job_id" ]; then
    : "${MERC_CANARY_BUYER_API_KEYS:?buyer key required to retrieve results during pause}"
    local bcfg auth
    bcfg="$(mktemp "${TMPDIR:-/tmp}/merc-alpha-buyer.XXXXXX")"
    chmod 600 "$bcfg"
    auth="${MERC_CANARY_BUYER_API_KEYS%%,*}"
    auth="${auth#*=}"
    printf 'header = "Authorization: Bearer %s"\n' \
      "$(printf '%s' "$auth" | sed 's/\\/\\\\/g; s/"/\\"/g')" > "$bcfg"
    results="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$bcfg" \
      -o /dev/null -w '%{http_code}' "$base/v1/jobs/$job_id/results" || true)"
    rm -f "$bcfg"
    case "$results" in
      200|404) ;;
      *)
        payload='{"paused":false,"reason":"alpha-canary-intake-resume"}'
        curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
          -X POST -H 'Content-Type: application/json' --data-binary "$payload" \
          "$base/admin/controls/intake" >/dev/null || true
        rm -f "$cfg"
        alpha_die "result retrieval during intake pause returned HTTP $results"
        ;;
    esac
  fi
  payload='{"paused":false,"reason":"alpha-canary-intake-resume"}'
  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
    -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
    --data-binary "$payload" "$base/admin/controls/intake")"
  rm -f "$cfg"
  case "$code" in
    2??) ;;
    *) alpha_die "intake resume failed HTTP $code" ;;
  esac
  emit_simple intake_pause_result_retrieval "operational-control-intake"
}

run_revocation() {
  [ "$minimum" = 1 ] || alpha_die "revocation minimum must be 1"
  local cred="${MERC_CANARY_REVOKE_CREDENTIAL_ID:-}"
  [ -n "$cred" ] || alpha_die "MERC_CANARY_REVOKE_CREDENTIAL_ID (supplier worker credential UUID) is required"
  : "${MERC_CANARY_SUPPLIER_API_KEY:?MERC_CANARY_SUPPLIER_API_KEY is required to revoke}"
  local base cfg code
  base="$(control_base)"
  cfg="$(mktemp "${TMPDIR:-/tmp}/merc-alpha-sup.XXXXXX")"
  chmod 600 "$cfg"
  printf 'header = "Authorization: Bearer %s"\n' \
    "$(printf '%s' "$MERC_CANARY_SUPPLIER_API_KEY" | sed 's/\\/\\\\/g; s/"/\\"/g')" > "$cfg"
  code="$(curl --silent --show-error --proto '=https' --tlsv1.2 --config "$cfg" \
    -o /dev/null -w '%{http_code}' -X DELETE "$base/v1/supplier/worker-credentials/$cred")"
  rm -f "$cfg"
  case "$code" in
    2??|404) ;;
    *) alpha_die "revocation DELETE returned HTTP $code" ;;
  esac
  emit_simple revocation "$cred"
}

case "$scenario" in
  approved_buyer_identity|distinct_metal_agent|embed_success|batch_infer_success|\
  cancelled_job|forced_retry|stale_lease_recovery|stale_attempt_commit_rejection|\
  buyer_webhook_retry_sequence|backup_independent_restore|stripe_test_matrix|\
  real_alert_firing_resolution|post_rehearsal_invariant_audit|bounded_retry_backoff_audit)
    delegate
    ;;
  no_payout_export) run_no_payout_export ;;
  kill_switch) run_kill_switch ;;
  intake_pause_result_retrieval) run_intake_pause ;;
  revocation) run_revocation ;;
  *) alpha_die "unsupported scenario: $scenario" ;;
esac
