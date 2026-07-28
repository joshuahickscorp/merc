#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"

usage() {
  echo "usage: scripts/go-closure-restart-storm.sh --target local|ssh --check|--execute" >&2
  exit 2
}

wait_release() {
  local deadline=$(( $(date +%s) + 300 ))
  while ! gc_probe_release "$MERC_CANDIDATE_COMMIT" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || gc_die "candidate did not recover within 300s"
    sleep 3
  done
}

validate_agent_receipt() {
  local file="$1" checked_at="$2"
  python3 "$ROOT/scripts/validate-agent-restart-receipt.py" "$file" \
    --run-id "$MERC_RESTART_RUN_ID" \
    --commit "$MERC_RESTART_CANDIDATE_COMMIT" \
    --image "$MERC_RESTART_CONTROL_IMAGE" \
    --driver-sha256 "$MERC_RESTART_DRIVER_SHA256" \
    --approved-worker-ids "$MERC_CANARY_APPROVED_WORKER_IDS" \
    --run-started-at "$MERC_RESTART_RUN_STARTED_AT" \
    --checked-at "$checked_at" >/dev/null \
    || gc_die "agent restart driver receipt failed exact-run validation"
}

gc_restart_db_json() {
  local sql="$1"
  gc_compose exec -T postgres psql -X -qAt -v ON_ERROR_STOP=1 -U cx -d cx -c "$sql"
}

restart_worker_uuid_array() {
  jq -Rer '
    split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
    if length == 2 and (unique | length) == 2 and
       all(.[]; test("^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
    then "{" + join(",") + "}"
    else error("invalid approved worker UUID set")
    end
  ' <<< "$MERC_CANARY_APPROVED_WORKER_IDS"
}

approved_worker_json() {
  jq -Rcer '
    split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
    map(select(length > 0)) | unique | sort
  ' <<< "$MERC_CANARY_APPROVED_WORKER_IDS"
}

observe_agent_sessions() {
  local workers versions_b64 builds_b64
  workers="$(restart_worker_uuid_array)"
  versions_b64="$(printf '%s' "$MERC_CANARY_APPROVED_AGENT_VERSIONS" | openssl base64 -A)"
  builds_b64="$(printf '%s' "$MERC_CANARY_APPROVED_BUILD_HASHES" | openssl base64 -A)"
  gc_restart_db_json "
    SELECT COALESCE(json_agg(json_build_object(
             'worker_id',w.id::text,
             'agent_session_id',w.agent_session_id::text,
             'session_started_epoch',floor(extract(epoch FROM w.agent_session_started_at))::bigint,
             'last_seen_epoch',floor(extract(epoch FROM w.last_seen_at))::bigint,
             'agent_version',w.version,
             'build_hash',w.build_hash
           ) ORDER BY w.id::text),'[]'::json)::text
      FROM workers w
     WHERE w.id=ANY('$workers'::uuid[])
       AND w.agent_session_id IS NOT NULL
       AND w.agent_session_started_at IS NOT NULL
       AND w.last_seen_at IS NOT NULL
       AND w.version=ANY(string_to_array(
         replace(convert_from(decode('$versions_b64','base64'),'UTF8'),' ',''),','))
       AND w.build_hash=ANY(string_to_array(
         replace(convert_from(decode('$builds_b64','base64'),'UTF8'),' ',''),','))"
}

validate_agent_session_set() {
  local sessions="$1" active_floor="$2" approved
  approved="$(approved_worker_json)"
  jq -en --argjson sessions "$sessions" --argjson approved "$approved" \
    --argjson active_floor "$active_floor" '
    ($sessions | length) == 2 and
    ($sessions | map(.worker_id) | unique | sort) == $approved and
    all($sessions[];
      (.agent_session_id | type == "string" and
        test("^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$")) and
      (.session_started_epoch | type == "number" and . > 0) and
      (.last_seen_epoch | type == "number" and . >= $active_floor) and
      (.agent_version | type == "string" and length > 0) and
      (.build_hash | type == "string" and length == 16)
    )
  ' >/dev/null || gc_die "approved agents lack two current reviewed process-session observations"
}

agent_sessions_transitioned() {
  local before="$1" after="$2" run_started_epoch="$3"
  jq -en --argjson before "$before" --argjson after "$after" \
    --argjson run_started "$run_started_epoch" '
    ($before | length) == 2 and ($after | length) == 2 and
    all($after[];
      . as $current |
      ($current.session_started_epoch >= $run_started) and
      ($current.last_seen_epoch >= $run_started) and
      any($before[];
        .worker_id == $current.worker_id and
        .agent_session_id != $current.agent_session_id
      )
    )
  ' >/dev/null
}

wait_agent_session_transitions() {
  local before="$1" run_started_epoch="$2" deadline after
  deadline=$(( $(date +%s) + 300 ))
  while :; do
    after="$(observe_agent_sessions)"
    if agent_sessions_transitioned "$before" "$after" "$run_started_epoch"; then
      printf '%s\n' "$after"
      return
    fi
    [ "$(date +%s)" -lt "$deadline" ] \
      || gc_die "two approved agent session transitions were not observed within 300s"
    sleep 3
  done
}

if [ "${MERC_AGENT_RESTART_CORROBORATION_SELF_TEST:-}" = 1 ]; then
  [ "$#" -eq 1 ] || gc_die "restart corroboration self-test requires BEFORE_SESSIONS_JSON"
  : "${MERC_TEST_DATABASE_URL:?restart corroboration self-test requires MERC_TEST_DATABASE_URL}"
  : "${MERC_RESTART_RUN_STARTED_EPOCH:?restart corroboration self-test requires a run epoch}"
  : "${MERC_CANARY_APPROVED_WORKER_IDS:?restart corroboration self-test requires approved workers}"
  : "${MERC_CANARY_APPROVED_AGENT_VERSIONS:?restart corroboration self-test requires approved versions}"
  : "${MERC_CANARY_APPROVED_BUILD_HASHES:?restart corroboration self-test requires approved builds}"
  gc_restart_db_json() {
    psql "$MERC_TEST_DATABASE_URL" -X -qAt -v ON_ERROR_STOP=1 -c "$1"
  }
  before_sessions="$(jq -cer . "$1")"
  after_sessions="$(observe_agent_sessions)"
  validate_agent_session_set "$after_sessions" "$MERC_RESTART_RUN_STARTED_EPOCH"
  agent_sessions_transitioned \
    "$before_sessions" "$after_sessions" "$MERC_RESTART_RUN_STARTED_EPOCH" \
    || gc_die "database does not prove both agent process sessions changed"
  printf '%s\n' "$after_sessions"
  exit
fi

RECOVERY_NETWORK=""
RECOVERY_CID=""
recover_network_on_exit() {
  if [ -n "$RECOVERY_NETWORK" ] && [ -n "$RECOVERY_CID" ]; then
    docker network connect --alias control "$RECOVERY_NETWORK" "$RECOVERY_CID" >/dev/null 2>&1 || true
  fi
}

host_storm() {
  local operation="$1"
  gc_require_command python3
  gc_require_command openssl
  gc_load_env
  gc_validate_host_config
  export MERC_ACTIVE_CONTROL_IMAGE="$MERC_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$MERC_CANDIDATE_COMMIT" >/dev/null

  gc_require_declared_inputs \
    MERC_AGENT_RESTART_DRIVER MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256
  [[ "$MERC_AGENT_RESTART_DRIVER" == /* ]] || gc_die "MERC_AGENT_RESTART_DRIVER must be an absolute path"
  [ -x "$MERC_AGENT_RESTART_DRIVER" ] || gc_die "MERC_AGENT_RESTART_DRIVER is not executable"
  [ ! -L "$MERC_AGENT_RESTART_DRIVER" ] || gc_die "MERC_AGENT_RESTART_DRIVER must not be a symlink"
  local driver_mode driver_dir_mode driver_physical
  driver_mode="$(gc_env_mode "$MERC_AGENT_RESTART_DRIVER")"
  [ $((8#$driver_mode & 8#022)) -eq 0 ] \
    || gc_die "MERC_AGENT_RESTART_DRIVER must not be group/world writable (mode $driver_mode)"
  driver_physical="$(cd "$(dirname "$MERC_AGENT_RESTART_DRIVER")" && pwd -P)/$(basename "$MERC_AGENT_RESTART_DRIVER")"
  [ "$driver_physical" = "$MERC_AGENT_RESTART_DRIVER" ] \
    || gc_die "MERC_AGENT_RESTART_DRIVER must be a physical canonical path"
  driver_dir_mode="$(gc_env_mode "$(dirname "$MERC_AGENT_RESTART_DRIVER")")"
  [ $((8#$driver_dir_mode & 8#022)) -eq 0 ] \
    || gc_die "agent-restart driver directory must not be group/world writable (mode $driver_dir_mode)"
  MERC_RESTART_DRIVER_SHA256="$(gc_sha256 "$MERC_AGENT_RESTART_DRIVER")"
  [[ "$MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256" =~ ^[0-9a-f]{64}$ ]] \
    || gc_die "MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256 must be 64 lowercase hex"
  [ "$MERC_RESTART_DRIVER_SHA256" = "$MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256" ] \
    || gc_die "agent-restart driver does not match the reviewed SHA-256"
  export MERC_RESTART_DRIVER_SHA256
  local interruption_seconds="${MERC_REHEARSAL_NETWORK_INTERRUPTION_SECONDS:-5}"
  [[ "$interruption_seconds" =~ ^[0-9]+$ ]] || gc_die "network interruption seconds must be an integer"
  [ "$interruption_seconds" -ge 1 ] && [ "$interruption_seconds" -le 30 ] \
    || gc_die "network interruption seconds must be between 1 and 30"
  local preflight_sessions preflight_active_floor
  preflight_active_floor=$(( $(date +%s) - 60 ))
  preflight_sessions="$(observe_agent_sessions)"
  validate_agent_session_set "$preflight_sessions" "$preflight_active_floor"
  if [ "$operation" = check ]; then
    gc_log "restart-storm target, reviewed driver, and two agent process sessions are valid (no changes made)"
    return
  fi
  [ "$operation" = execute ] || gc_die "operation must be check or execute"

  gc_prepare_evidence restart-storm-working
  local agent_receipt
  agent_receipt="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-agent-restarts.json"
  local started_at finished_at control_count=0 database_count=0 storage_count=0
  local alert_count=0 network_count=0 network cid configured_image
  local run_started_epoch before_sessions after_sessions final_sessions checked_at
  MERC_RESTART_RUN_ID="$(openssl rand -hex 16)"
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  run_started_epoch="$(date +%s)"
  MERC_RESTART_RUN_STARTED_AT="$started_at"
  MERC_RESTART_RUN_STARTED_EPOCH="$run_started_epoch"
  MERC_RESTART_CANDIDATE_COMMIT="$MERC_CANDIDATE_COMMIT"
  MERC_RESTART_CONTROL_IMAGE="$MERC_CANDIDATE_CONTROL_IMAGE"
  export MERC_RESTART_RUN_ID MERC_RESTART_RUN_STARTED_AT \
    MERC_RESTART_RUN_STARTED_EPOCH MERC_RESTART_CANDIDATE_COMMIT \
    MERC_RESTART_CONTROL_IMAGE
  before_sessions="$(observe_agent_sessions)"
  validate_agent_session_set "$before_sessions" "$((run_started_epoch - 60))"

  umask 077
  [ "$(gc_sha256 "$MERC_AGENT_RESTART_DRIVER")" = "$MERC_RESTART_DRIVER_SHA256" ] \
    || gc_die "agent-restart driver bytes changed before execution"
  if ! "$MERC_AGENT_RESTART_DRIVER" restart-all 2 > "$agent_receipt"; then
    rm -f -- "$agent_receipt"
    gc_die "agent restart driver failed"
  fi
  [ "$(gc_sha256 "$MERC_AGENT_RESTART_DRIVER")" = "$MERC_RESTART_DRIVER_SHA256" ] \
    || gc_die "agent-restart driver bytes changed during execution"
  chmod 600 "$agent_receipt"
  checked_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  validate_agent_receipt "$agent_receipt" "$checked_at"
  after_sessions="$(wait_agent_session_transitions "$before_sessions" "$run_started_epoch")"
  validate_agent_session_set "$after_sessions" "$run_started_epoch"

  for _ in 1 2; do
    gc_compose restart control >/dev/null
    gc_wait_service control 300
    wait_release
    control_count=$((control_count + 1))
  done

  gc_compose restart postgres >/dev/null
  gc_wait_service postgres 300
  wait_release
  database_count=$((database_count + 1))

  gc_compose restart minio >/dev/null
  gc_wait_service minio 300
  wait_release
  storage_count=$((storage_count + 1))

  gc_compose restart alertmanager >/dev/null
  gc_wait_service alertmanager 300
  gc_wait_http http://127.0.0.1:9093/-/ready 180
  alert_count=$((alert_count + 1))

  cid="$(gc_compose ps -q control)"
  network="$(docker inspect "$cid" | jq -er '.[0].NetworkSettings.Networks | keys[] | select(endswith("_default"))' | head -1)"
  [ -n "$network" ] || gc_die "could not resolve the compose default network"
  trap recover_network_on_exit EXIT INT TERM
  for _ in 1 2; do
    RECOVERY_NETWORK="$network"
    RECOVERY_CID="$cid"
    docker network disconnect "$network" "$cid"
    sleep "$interruption_seconds"
    docker network connect --alias control "$network" "$cid"
    RECOVERY_NETWORK=""
    RECOVERY_CID=""
    gc_wait_service control 300
    wait_release
    network_count=$((network_count + 1))
  done
  trap - EXIT INT TERM

  configured_image="$(docker inspect -f '{{.Config.Image}}' "$cid")"
  [ "$configured_image" = "$MERC_CANDIDATE_CONTROL_IMAGE" ] \
    || gc_die "restart storm changed the active control image"
  [ "$(gc_sha256 "$MERC_AGENT_RESTART_DRIVER")" = "$MERC_RESTART_DRIVER_SHA256" ] \
    || gc_die "agent-restart driver bytes changed during the restart storm"
  final_sessions="$(observe_agent_sessions)"
  validate_agent_session_set "$final_sessions" "$(( $(date +%s) - 60 ))"
  jq -en --argjson after "$after_sessions" --argjson final "$final_sessions" '
    ($after | map({worker_id,agent_session_id}) | sort_by(.worker_id)) ==
    ($final | map({worker_id,agent_session_id}) | sort_by(.worker_id))
  ' >/dev/null || gc_die "an approved agent restarted again or lost its observed process session"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  gc_prepare_evidence restart-storm
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" --arg finished "$finished_at" \
    --arg image "$MERC_CANDIDATE_CONTROL_IMAGE" --arg commit "$MERC_CANDIDATE_COMMIT" \
    --arg run_id "$MERC_RESTART_RUN_ID" --arg driver "$MERC_AGENT_RESTART_DRIVER" \
    --arg driver_sha256 "$MERC_RESTART_DRIVER_SHA256" \
    --argjson controls "$control_count" --argjson databases "$database_count" \
    --argjson storages "$storage_count" --argjson alerts "$alert_count" \
    --argjson networks "$network_count" --argjson seconds "$interruption_seconds" \
    --argjson before_sessions "$before_sessions" --argjson after_sessions "$after_sessions" \
    --argjson final_sessions "$final_sessions" --slurpfile agent "$agent_receipt" \
    '{schema_version:2,kind:"go_closure_restart_storm",status:"PASS",
      run_id:$run_id,started_at:$started,finished_at:$finished,
      control_image:$image,expected_commit:$commit,
      agent_restart_driver:{path:$driver,sha256:$driver_sha256,
        matches_operator_reviewed_sha256:true,unchanged_during_run:true},
      observed:{control_restarts:$controls,database_restarts:$databases,
                storage_restarts:$storages,alerting_restarts:$alerts,
                network_interruptions:$networks,network_interruption_seconds_each:$seconds,
                agent_supervisor_action_receipt:$agent[0],
                agent_sessions_before:$before_sessions,
                agent_sessions_after_restart:$after_sessions,
                agent_sessions_final:$final_sessions},
      assertions:{control_restarts_at_least_2:($controls >= 2),
                  two_distinct_agents_restarted_from_database_session_transitions:true,
                  restarted_agents_remained_current_without_an_extra_restart:true,
                  recovered_after_each_fault_within_300_seconds:true,
                  retry_backoff_requires_correlated_scenario_audit:true},
      policy:{stripe_live_mode:false,real_value:false,secret_values_recorded:false}}'
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 2 ] || usage
  host_storm "$2"
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
gc_run_on_target "$target" scripts/go-closure-restart-storm.sh "$operation"
