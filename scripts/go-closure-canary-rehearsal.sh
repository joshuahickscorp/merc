#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"

usage() {
  echo "usage: scripts/go-closure-canary-rehearsal.sh --target local|ssh --check|--execute" >&2
  exit 2
}

validate_scenario_receipt() {
  local file="$1" scenario="$2" minimum="$3"
  jq -e --arg scenario "$scenario" --argjson minimum "$minimum" '
    .schema_version == 1 and .scenario == $scenario and .requested == $minimum and
    .status == "PASS" and (.observed | type == "number" and . >= $minimum) and
    .safety.stripe_test_mode == true and .safety.real_value == false and
    .safety.approved_participants_only == true and
    (.evidence | type == "array") and (.evidence | length) >= $minimum and
    ([.evidence[].id] | unique | length) >= $minimum and
    all(.evidence[]; (.id | type == "string" and length > 0) and
                     (.occurred_at | type == "string" and length > 0) and
                     (.source | type == "string" and length > 0)) and
    ([.. | strings | select(test("sk_(test|live)_|whsec_|AGE-SECRET-KEY-|AKIA"; "i"))] | length) == 0
  ' "$file" >/dev/null || gc_die "$scenario driver receipt is invalid, incomplete, unsafe, or contains a secret-shaped value"

  case "$scenario" in
    stripe_test_matrix)
      jq -e '.provider_mode == "test" and .matrix_complete == true and .real_value == false' "$file" >/dev/null \
        || gc_die "Stripe matrix receipt is not complete test mode"
      ;;
    real_alert_firing_resolution)
      jq -e --arg receiver "$ALERT_RECEIVER_NAME" '
        .receiver_name == $receiver and
        (.receiver_event_ids.firing | type == "string" and length > 0) and
        (.receiver_event_ids.resolved | type == "string" and length > 0) and
        .receiver_event_ids.firing != .receiver_event_ids.resolved
      ' "$file" >/dev/null || gc_die "alert receipt lacks distinct firing/resolved receiver events"
      ;;
    backup_independent_restore)
      jq -e '
        .encrypted_offsite_upload == true and .independent_download == true and
        .ciphertext_checksum_verified == true and .isolated_restore == true and
        .postgres_semantic_checks == true and .object_checks == true
      ' "$file" >/dev/null || gc_die "backup receipt does not prove independent restore checks"
      ;;
    post_rehearsal_invariant_audit)
      jq -e '
        .invariants == {
          tenant_leak:false,missing_artifact:false,duplicate_effects:false,
          ledger_imbalance:false,stuck_terminal_jobs:false,stuck_payouts:false,
          unreconciled_state:false,silent_webhook_loss:false,unbounded_growth:false
        }
      ' "$file" >/dev/null || gc_die "post-rehearsal invariant audit is not clean"
      ;;
    bounded_retry_backoff_audit)
      jq -e '
        .max_attempts_within_policy == true and
        .backoff_schedule_within_policy == true and
        .unbounded_retry_growth == false
      ' "$file" >/dev/null || gc_die "retry/backoff audit is not bounded"
      ;;
  esac
}

host_rehearsal() {
  local operation="$1"
  gc_load_env
  gc_validate_host_config
  gc_require_declared_inputs CX_BACKUP_OFFSITE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY \
    CX_BACKUP_ENCRYPTION_RECIPIENT CX_BACKUP_DECRYPTION_IDENTITY_FILE \
    CX_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID
  gc_require_declared_inputs CX_CANARY_SCENARIO_DRIVER
  [[ "$CX_CANARY_SCENARIO_DRIVER" == /* ]] \
    || gc_die "CX_CANARY_SCENARIO_DRIVER must be an absolute path"
  [ -x "$CX_CANARY_SCENARIO_DRIVER" ] \
    || gc_die "CX_CANARY_SCENARIO_DRIVER is not executable"
  export CX_ACTIVE_CONTROL_IMAGE="$CX_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$CX_CANDIDATE_COMMIT" >/dev/null
  if [ "$operation" = check ]; then
    gc_log "canary target and scenario driver are valid (no scenarios run)"
    return
  fi
  [ "$operation" = execute ] || gc_die "operation must be check or execute"

  local scenarios=(
    approved_buyer_identity
    distinct_metal_agent
    embed_success
    batch_infer_success
    cancelled_job
    forced_retry
    stale_lease_recovery
    stale_attempt_commit_rejection
    buyer_webhook_retry_sequence
    backup_independent_restore
    stripe_test_matrix
    real_alert_firing_resolution
    post_rehearsal_invariant_audit
    bounded_retry_backoff_audit
  )
  local minimums=(2 2 20 20 5 5 3 3 3 1 1 1 1 1)
  local started_at finished_at scenario minimum receipt index
  local receipt_files=()
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  gc_prepare_evidence canary-working

  for index in "${!scenarios[@]}"; do
    scenario="${scenarios[$index]}"
    minimum="${minimums[$index]}"
    receipt="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-scenario-$scenario.json"
    umask 077
    if ! "$CX_CANARY_SCENARIO_DRIVER" run "$scenario" "$minimum" > "$receipt"; then
      rm -f -- "$receipt"
      gc_die "scenario driver failed for $scenario"
    fi
    chmod 600 "$receipt"
    validate_scenario_receipt "$receipt" "$scenario" "$minimum"
    receipt_files+=("$receipt")
  done

  local active_workers page_alerts db_snapshot
  active_workers="$(gc_prometheus_scalar 'cx_active_workers')"
  awk -v n="$active_workers" 'BEGIN { exit !(n >= 2) }' \
    || gc_die "Prometheus reports fewer than two active Metal agents"
  page_alerts="$(gc_prometheus_scalar 'sum(ALERTS{alertstate="firing",severity="page"}) or vector(0)')"
  awk -v n="$page_alerts" 'BEGIN { exit !(n == 0) }' \
    || gc_die "page alerts remain firing after the rehearsal"
  db_snapshot="$(gc_database_snapshot)"
  jq -e '.terminal_jobs_with_open_tasks == 0' <<< "$db_snapshot" >/dev/null \
    || gc_die "terminal jobs still have open tasks"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  local combined="$GC_EVIDENCE_DIR/.scenario-receipts.$$"
  jq -s '.' "${receipt_files[@]}" > "$combined"
  gc_prepare_evidence canary-rehearsal
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" --arg finished "$finished_at" \
    --arg image "$CX_CANDIDATE_CONTROL_IMAGE" --arg commit "$CX_CANDIDATE_COMMIT" \
    --argjson workers "$active_workers" --argjson alerts "$page_alerts" \
    --argjson database "$db_snapshot" --slurpfile receipts "$combined" \
    '{schema_version:1,kind:"go_closure_canary_rehearsal",status:"PASS",
      started_at:$started,finished_at:$finished,control_image:$image,expected_commit:$commit,
      required_counts:{approved_buyer_identities:2,distinct_metal_agents:2,
                       successful_embed_jobs:20,successful_batch_infer_jobs:20,
                       cancelled_jobs:5,forced_retries:5,stale_lease_recoveries:3,
                       stale_attempt_commit_rejections:3,buyer_webhook_retry_sequences:3,
                       backup_independent_restore:1,stripe_test_matrix:1,
                       real_alert_firing_resolution:1,post_rehearsal_invariant_audit:1,
                       bounded_retry_backoff_audit:1},
      observations:{active_workers:$workers,page_alerts_firing_after_rehearsal:$alerts,
                    database:$database,scenario_receipts:$receipts[0]},
      policy:{stripe_test_mode:true,stripe_live_mode:false,real_value:false,
              approved_participants_only:true,secret_values_recorded:false},
      qualification:{workload_and_external_scenarios:true,
                     restart_rollback_and_24h_soak_receipts_required_separately:true}}'
  rm -f -- "$combined"
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 2 ] || usage
  host_rehearsal "$2"
  exit
fi

target="" operation=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --check) operation=check ;;
    --execute) operation=execute ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
case "$operation" in check|execute) ;; *) usage ;; esac
gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
gc_run_on_target "$target" scripts/go-closure-canary-rehearsal.sh "$operation"
