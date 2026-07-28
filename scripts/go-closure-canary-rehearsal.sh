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
  local file="$1" scenario="$2" minimum="$3" scenario_started_at="$4" checked_at="$5"
  python3 "$ROOT/scripts/validate-canary-scenario-receipt.py" "$file" \
    --scenario "$scenario" --minimum "$minimum" \
    --run-id "$MERC_CANARY_RUN_ID" \
    --commit "$MERC_CANARY_CANDIDATE_COMMIT" \
    --image "$MERC_CANARY_CONTROL_IMAGE" \
    --driver-sha256 "$MERC_CANARY_DRIVER_SHA256" \
    --run-started-at "$MERC_CANARY_RUN_STARTED_AT" \
    --scenario-started-at "$scenario_started_at" \
    --checked-at "$checked_at" >/dev/null \
    || gc_die "$scenario driver receipt failed exact-run validation"

  if [ "$scenario" = real_alert_firing_resolution ]; then
    jq -e --arg receiver "$ALERT_RECEIVER_NAME" '.receiver_name == $receiver' "$file" >/dev/null \
      || gc_die "alert receipt receiver does not match the approved receiver"
  fi
}

gc_canary_db_scalar() {
  local sql="$1"
  gc_compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U cx -d cx -c "$sql"
}

scenario_subject_uuid_array() {
  local file="$1"
  jq -er '
    [.evidence[].subject_id] as $ids |
    if all($ids[]; test("^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
    then "{" + ($ids | join(",")) + "}"
    else error("non-UUID scenario subject")
    end
  ' "$file"
}

require_observed_count() {
  local scenario="$1" minimum="$2" observed="$3"
  [ "$observed" = "$minimum" ] \
    || gc_die "$scenario has $observed independently corroborated observation(s); require exactly $minimum"
}

corroborate_scenario() {
  local file="$1" scenario="$2" minimum="$3" ids observed buyers_b64 workers versions_b64 builds_b64
  local subject current current_count
  buyers_b64="$(printf '%s' "$MERC_CANARY_APPROVED_BUYER_EMAILS" | openssl base64 -A)"
  workers="$(jq -Rer '
    split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
    if length == 2 and (unique | length) == 2 and
       all(.[]; test("^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
    then "{" + join(",") + "}"
    else error("invalid approved worker UUID set")
    end
  ' <<< "$MERC_CANARY_APPROVED_WORKER_IDS")"
  versions_b64="$(printf '%s' "$MERC_CANARY_APPROVED_AGENT_VERSIONS" | openssl base64 -A)"
  builds_b64="$(printf '%s' "$MERC_CANARY_APPROVED_BUILD_HASHES" | openssl base64 -A)"
  case "$scenario" in
    approved_buyer_identity)
      jq -en --arg approved "$MERC_CANARY_APPROVED_BUYER_EMAILS" --slurpfile receipt "$file" '
        ($approved | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
          map(select(length > 0)) | unique | sort) as $want |
        ($receipt[0].evidence | map(.subject_id) | unique | sort) as $subjects |
        ($want | length) == 2 and ($subjects | length) == 2
      ' >/dev/null || gc_die "approved buyer receipt does not contain exactly two distinct subjects"
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*) FROM buyers
         WHERE id=ANY('$ids'::uuid[]) AND deleted_at IS NULL
           AND lower(email)=ANY(string_to_array(
             replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    distinct_metal_agent)
      jq -en --arg approved "$MERC_CANARY_APPROVED_WORKER_IDS" --slurpfile receipt "$file" '
        ($approved | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
          map(select(length > 0)) | unique | sort) ==
        ($receipt[0].evidence | map(.subject_id) | unique | sort)
      ' >/dev/null || gc_die "Metal-agent receipt subjects do not match the approved worker IDs"
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*) FROM workers
         WHERE id=ANY('$ids'::uuid[])
           AND hw_class LIKE 'apple_silicon_%'
           AND last_seen_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND version=ANY(string_to_array(
             replace(convert_from(decode('$versions_b64','base64'),'UTF8'),' ',''),','))
           AND build_hash=ANY(string_to_array(
             replace(convert_from(decode('$builds_b64','base64'),'UTF8'),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    embed_success|batch_infer_success)
      ids="$(scenario_subject_uuid_array "$file")"
      local job_type=embed
      [ "$scenario" != batch_infer_success ] || job_type=batch_infer
      observed="$(gc_canary_db_scalar "
        SELECT count(*)
          FROM jobs j
         WHERE j.id=ANY('$ids'::uuid[])
           AND j.job_type='$job_type' AND j.status='complete'
           AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND j.actual_usd > 0
           AND EXISTS (
             SELECT 1 FROM buyers b
              WHERE b.id=j.buyer_id AND b.deleted_at IS NULL
                AND lower(b.email)=ANY(string_to_array(
                  replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))
           )
           AND j.workload_decision_sha256 ~ '^[0-9a-f]{64}$'
           AND j.compute_plan_sha256 ~ '^[0-9a-f]{64}$'
           AND j.task_count > 0 AND j.tasks_done=j.task_count
           AND j.task_count=(SELECT count(*) FROM tasks t WHERE t.job_id=j.id)
           AND j.actual_usd >= (
             SELECT COALESCE(sum(t.economic_buyer_charge_usd),0)
               FROM tasks t WHERE t.job_id=j.id
           )
           AND NOT EXISTS (
             SELECT 1
               FROM tasks t
               LEFT JOIN workers w ON w.id=t.execution_worker_id
              WHERE t.job_id=j.id AND NOT COALESCE((
                t.status='complete'
                AND t.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
                AND t.execution_worker_id=ANY('$workers'::uuid[])
                AND w.supplier_id=t.execution_supplier_id
                AND w.hw_class LIKE 'apple_silicon_%'
                AND w.last_seen_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
                AND w.version=ANY(string_to_array(
                  replace(convert_from(decode('$versions_b64','base64'),'UTF8'),' ',''),','))
                AND w.build_hash=ANY(string_to_array(
                  replace(convert_from(decode('$builds_b64','base64'),'UTF8'),' ',''),','))
                AND t.execution_hw_class=w.hw_class
                AND btrim(COALESCE(t.execution_engine,'')) <> ''
                AND t.execution_build_hash=w.build_hash
                AND btrim(COALESCE(t.runtime_cell_id,'')) <> ''
                AND btrim(COALESCE(t.runtime_id,'')) <> ''
                AND t.runtime_matrix_sha256 ~ '^[0-9a-f]{64}$'
                AND t.result_sha256 ~ '^[0-9a-f]{64}$'
                AND t.verification_outcome IN ('pass','pass_with_penalty')
                AND t.verified_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
                AND t.economic_buyer_charge_usd > 0
                AND t.economic_supplier_payout_usd > 0
                AND (
                  SELECT count(*) FROM ledger_entries le
                   WHERE le.task_id=t.id
                ) = 3
                AND EXISTS (
                  SELECT 1 FROM ledger_entries le
                   WHERE le.task_id=t.id AND le.kind='buyer_charge'
                     AND le.buyer_id=j.buyer_id
                     AND le.supplier_id IS NULL
                     AND le.amount_usd=-t.economic_buyer_charge_usd
                )
                AND EXISTS (
                  SELECT 1 FROM ledger_entries le
                   WHERE le.task_id=t.id AND le.kind='supplier_credit'
                     AND le.buyer_id IS NULL
                     AND le.supplier_id=t.execution_supplier_id
                     AND le.amount_usd=t.economic_supplier_payout_usd
                )
                AND EXISTS (
                  SELECT 1 FROM ledger_entries le
                   WHERE le.task_id=t.id AND le.kind='platform_take'
                     AND le.buyer_id IS NULL AND le.supplier_id IS NULL
                     AND le.amount_usd=(
                       t.economic_buyer_charge_usd-t.economic_supplier_payout_usd
                     )
                )
                AND (
                  SELECT COALESCE(sum(le.amount_usd),0)
                    FROM ledger_entries le WHERE le.task_id=t.id
                ) = 0
              ),false)
           )")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    cancelled_job)
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*) FROM jobs j JOIN buyers b ON b.id=j.buyer_id
         WHERE j.id=ANY('$ids'::uuid[]) AND j.status='cancelled'
           AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND b.deleted_at IS NULL
           AND lower(b.email)=ANY(string_to_array(
             replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    forced_retry)
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*)
          FROM job_events e
          JOIN jobs j ON j.id=e.job_id
          JOIN buyers b ON b.id=j.buyer_id
          JOIN tasks t ON t.id=e.task_id AND t.job_id=j.id
         WHERE e.id=ANY('$ids'::uuid[]) AND e.event='task_requeued'
           AND e.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND t.retry_count >= 1 AND b.deleted_at IS NULL
           AND COALESCE(t.execution_worker_id,t.worker_id)=ANY('$workers'::uuid[])
           AND lower(b.email)=ANY(string_to_array(
             replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    stale_lease_recovery)
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*)
          FROM job_events e
          JOIN jobs j ON j.id=e.job_id
          JOIN buyers b ON b.id=j.buyer_id
         WHERE e.id=ANY('$ids'::uuid[])
           AND e.event = 'task_rescued_dead_worker'
           AND e.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND b.deleted_at IS NULL
           AND EXISTS (
             SELECT 1 FROM tasks t
              WHERE t.job_id=j.id
                AND COALESCE(t.execution_worker_id,t.worker_id)=ANY('$workers'::uuid[])
           )
           AND lower(b.email)=ANY(string_to_array(
             replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    buyer_webhook_retry_sequence)
      ids="$(scenario_subject_uuid_array "$file")"
      observed="$(gc_canary_db_scalar "
        SELECT count(*)
          FROM webhooks w
          JOIN jobs j ON j.id=w.job_id AND j.buyer_id=w.buyer_id
          JOIN buyers b ON b.id=w.buyer_id
         WHERE w.id=ANY('$ids'::uuid[]) AND w.attempts >= 2
           AND w.delivered_at IS NOT NULL
           AND w.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
           AND b.deleted_at IS NULL
           AND lower(b.email)=ANY(string_to_array(
             replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))")"
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    stale_attempt_commit_rejection)
      # Deduplicate subjects before counting (mirrors other branches' count(*) over PKs).
      jq -e --argjson min "$minimum" \
        '([.evidence[].subject_id] | unique | length) == $min' "$file" >/dev/null \
        || gc_die "stale_attempt_commit_rejection evidence subject_ids are not unique"
      observed=0
      # Read TSV on fd 3 so docker compose exec cannot drain the loop's stdin.
      while IFS=$'\t' read -r subject current <&3; do
        current_count="$(gc_canary_db_scalar "
          SELECT count(*)
            FROM tasks t
            JOIN jobs j ON j.id=t.job_id
            JOIN buyers b ON b.id=j.buyer_id
           WHERE t.id='$subject'::uuid AND t.retry_count=$current
             AND j.created_at >= '$MERC_CANARY_RUN_STARTED_AT'::timestamptz
             AND b.deleted_at IS NULL
             AND COALESCE(t.execution_worker_id,t.worker_id)=ANY('$workers'::uuid[])
             AND lower(b.email)=ANY(string_to_array(
               replace(lower(convert_from(decode('$buyers_b64','base64'),'UTF8')),' ',''),','))" </dev/null)"
        [ "$current_count" = 1 ] \
          || gc_die "stale-attempt subject $subject is not durably fenced at attempt $current"
        observed=$((observed + 1))
      done 3< <(jq -r '[.evidence[] | [.subject_id,.current_attempt]] | unique | .[] | @tsv' "$file")
      require_observed_count "$scenario" "$minimum" "$observed"
      ;;
    backup_independent_restore|stripe_test_matrix|real_alert_firing_resolution|\
      post_rehearsal_invariant_audit|bounded_retry_backoff_audit)
      # These observations cross provider or aggregate boundaries and are
      # checked by their strict source-specific receipt contracts plus final
      # database/Prometheus state below.
      ;;
    *) gc_die "no corroboration contract for $scenario" ;;
  esac
}

if [ "${MERC_CANARY_CORROBORATION_SELF_TEST:-}" = 1 ]; then
  [ "$#" -eq 3 ] || gc_die "corroboration self-test requires RECEIPT SCENARIO MINIMUM"
  : "${MERC_TEST_DATABASE_URL:?corroboration self-test requires MERC_TEST_DATABASE_URL}"
  : "${MERC_CANARY_RUN_STARTED_AT:?corroboration self-test requires a run start}"
  : "${MERC_CANARY_APPROVED_BUYER_EMAILS:?corroboration self-test requires approved buyers}"
  : "${MERC_CANARY_APPROVED_WORKER_IDS:?corroboration self-test requires approved workers}"
  : "${MERC_CANARY_APPROVED_AGENT_VERSIONS:?corroboration self-test requires approved versions}"
  : "${MERC_CANARY_APPROVED_BUILD_HASHES:?corroboration self-test requires approved builds}"
  gc_canary_db_scalar() {
    psql "$MERC_TEST_DATABASE_URL" -X -qAt -v ON_ERROR_STOP=1 -c "$1"
  }
  corroborate_scenario "$1" "$2" "$3"
  exit
fi

host_rehearsal() {
  local operation="$1"
  gc_require_command python3
  gc_require_command openssl
  gc_load_env
  gc_validate_host_config
  gc_require_declared_inputs MERC_BACKUP_OFFSITE AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY \
    MERC_BACKUP_ENCRYPTION_RECIPIENT MERC_BACKUP_DECRYPTION_IDENTITY_FILE \
    MERC_CONNECT_CLIENT_ID STRIPE_TEST_CONNECTED_ACCOUNT_ID
  gc_require_declared_inputs MERC_CANARY_SCENARIO_DRIVER MERC_CANARY_APPROVED_DRIVER_SHA256
  [[ "$MERC_CANARY_SCENARIO_DRIVER" == /* ]] \
    || gc_die "MERC_CANARY_SCENARIO_DRIVER must be an absolute path"
  [ -x "$MERC_CANARY_SCENARIO_DRIVER" ] \
    || gc_die "MERC_CANARY_SCENARIO_DRIVER is not executable"
  [ ! -L "$MERC_CANARY_SCENARIO_DRIVER" ] \
    || gc_die "MERC_CANARY_SCENARIO_DRIVER must not be a symlink"
  local driver_mode driver_dir_mode driver_physical
  driver_mode="$(gc_env_mode "$MERC_CANARY_SCENARIO_DRIVER")"
  [ $((8#$driver_mode & 8#022)) -eq 0 ] \
    || gc_die "MERC_CANARY_SCENARIO_DRIVER must not be group/world writable (mode $driver_mode)"
  driver_physical="$(cd "$(dirname "$MERC_CANARY_SCENARIO_DRIVER")" && pwd -P)/$(basename "$MERC_CANARY_SCENARIO_DRIVER")"
  [ "$driver_physical" = "$MERC_CANARY_SCENARIO_DRIVER" ] \
    || gc_die "MERC_CANARY_SCENARIO_DRIVER must be a physical canonical path"
  driver_dir_mode="$(gc_env_mode "$(dirname "$MERC_CANARY_SCENARIO_DRIVER")")"
  [ $((8#$driver_dir_mode & 8#022)) -eq 0 ] \
    || gc_die "scenario-driver directory must not be group/world writable (mode $driver_dir_mode)"
  MERC_CANARY_DRIVER_SHA256="$(gc_sha256 "$MERC_CANARY_SCENARIO_DRIVER")"
  [[ "$MERC_CANARY_APPROVED_DRIVER_SHA256" =~ ^[0-9a-f]{64}$ ]] \
    || gc_die "MERC_CANARY_APPROVED_DRIVER_SHA256 must be 64 lowercase hex"
  [ "$MERC_CANARY_DRIVER_SHA256" = "$MERC_CANARY_APPROVED_DRIVER_SHA256" ] \
    || gc_die "scenario driver does not match the reviewed SHA-256"
  export MERC_CANARY_DRIVER_SHA256
  export MERC_ACTIVE_CONTROL_IMAGE="$MERC_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$MERC_CANDIDATE_COMMIT" >/dev/null
  if [ "$operation" = check ]; then
    gc_log "canary target and scenario driver are valid (no scenarios run)"
    return
  fi
  [ "$operation" = execute ] || gc_die "operation must be check or execute"

  MERC_CANARY_RUN_ID="$(openssl rand -hex 16)"
  MERC_CANARY_RUN_STARTED_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  MERC_CANARY_CANDIDATE_COMMIT="$MERC_CANDIDATE_COMMIT"
  MERC_CANARY_CONTROL_IMAGE="$MERC_CANDIDATE_CONTROL_IMAGE"
  export MERC_CANARY_RUN_ID MERC_CANARY_RUN_STARTED_AT \
    MERC_CANARY_CANDIDATE_COMMIT MERC_CANARY_CONTROL_IMAGE

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
  local started_at finished_at scenario minimum receipt index scenario_started_at scenario_checked_at
  local receipt_files=()
  started_at="$MERC_CANARY_RUN_STARTED_AT"
  gc_prepare_evidence canary-working
  local db_before
  db_before="$(gc_database_snapshot)"

  for index in "${!scenarios[@]}"; do
    scenario="${scenarios[$index]}"
    minimum="${minimums[$index]}"
    receipt="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-scenario-$scenario.json"
    scenario_started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    export MERC_CANARY_SCENARIO="$scenario" MERC_CANARY_SCENARIO_MINIMUM="$minimum" \
      MERC_CANARY_SCENARIO_STARTED_AT="$scenario_started_at"
    umask 077
    [ "$(gc_sha256 "$MERC_CANARY_SCENARIO_DRIVER")" = "$MERC_CANARY_DRIVER_SHA256" ] \
      || gc_die "scenario driver bytes changed before $scenario"
    if ! "$MERC_CANARY_SCENARIO_DRIVER" run "$scenario" "$minimum" > "$receipt"; then
      rm -f -- "$receipt"
      gc_die "scenario driver failed for $scenario"
    fi
    [ "$(gc_sha256 "$MERC_CANARY_SCENARIO_DRIVER")" = "$MERC_CANARY_DRIVER_SHA256" ] \
      || gc_die "scenario driver bytes changed while running $scenario"
    chmod 600 "$receipt"
    scenario_checked_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    validate_scenario_receipt \
      "$receipt" "$scenario" "$minimum" "$scenario_started_at" "$scenario_checked_at"
    corroborate_scenario "$receipt" "$scenario" "$minimum"
    receipt_files+=("$receipt")
  done
  [ "$(gc_sha256 "$MERC_CANARY_SCENARIO_DRIVER")" = "$MERC_CANARY_DRIVER_SHA256" ] \
    || gc_die "scenario driver bytes changed during the canary run"

  local active_workers page_alerts db_snapshot
  active_workers="$(gc_prometheus_scalar 'merc_active_workers')"
  awk -v n="$active_workers" 'BEGIN { exit !(n >= 2) }' \
    || gc_die "Prometheus reports fewer than two active Metal agents"
  page_alerts="$(gc_prometheus_scalar 'sum(ALERTS{alertstate="firing",severity="page"}) or vector(0)')"
  awk -v n="$page_alerts" 'BEGIN { exit !(n == 0) }' \
    || gc_die "page alerts remain firing after the rehearsal"
  db_snapshot="$(gc_database_snapshot)"
  jq -e '
    .terminal_jobs_with_open_tasks == 0 and
    ((.ledger_sum_usd | tonumber) == 0)
  ' <<< "$db_snapshot" >/dev/null \
    || gc_die "post-canary database has open terminal tasks or a non-zero ledger sum"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  local combined="$GC_EVIDENCE_DIR/.scenario-receipts.$$"
  jq -s '.' "${receipt_files[@]}" > "$combined"
  gc_prepare_evidence canary-rehearsal
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" --arg finished "$finished_at" \
    --arg image "$MERC_CANDIDATE_CONTROL_IMAGE" --arg commit "$MERC_CANDIDATE_COMMIT" \
    --arg run_id "$MERC_CANARY_RUN_ID" --arg driver "$MERC_CANARY_SCENARIO_DRIVER" \
    --arg driver_sha256 "$MERC_CANARY_DRIVER_SHA256" \
    --argjson workers "$active_workers" --argjson alerts "$page_alerts" \
    --argjson database_before "$db_before" --argjson database "$db_snapshot" \
    --slurpfile receipts "$combined" \
    '{schema_version:2,kind:"go_closure_canary_rehearsal",status:"PASS",
      run_id:$run_id,started_at:$started,finished_at:$finished,
      control_image:$image,expected_commit:$commit,
      scenario_driver:{path:$driver,sha256:$driver_sha256,
                       matches_operator_reviewed_sha256:true,unchanged_during_run:true},
      required_counts:{approved_buyer_identities:2,distinct_metal_agents:2,
                       successful_embed_jobs:20,successful_batch_infer_jobs:20,
                       cancelled_jobs:5,forced_retries:5,stale_lease_recoveries:3,
                       stale_attempt_commit_rejections:3,buyer_webhook_retry_sequences:3,
                       backup_independent_restore:1,stripe_test_matrix:1,
                       real_alert_firing_resolution:1,post_rehearsal_invariant_audit:1,
                       bounded_retry_backoff_audit:1},
      observations:{active_workers:$workers,page_alerts_firing_after_rehearsal:$alerts,
                    database_before:$database_before,database_after:$database,
                    scenario_receipts:$receipts[0]},
      policy:{stripe_test_mode:true,stripe_live_mode:false,real_value:false,
              approved_participants_only:true,secret_values_recorded:false},
      qualification:{workload_and_external_scenarios:true,
                     exact_run_candidate_driver_binding:true,
                     database_backed_scenarios_corroborated:true,
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
