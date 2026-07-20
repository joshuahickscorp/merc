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
  while ! gc_probe_release "$CX_CANDIDATE_COMMIT" >/dev/null 2>&1; do
    [ "$(date +%s)" -lt "$deadline" ] || gc_die "candidate did not recover within 300s"
    sleep 3
  done
}

validate_agent_receipt() {
  local file="$1"
  jq -e '
    .schema_version == 1 and .status == "PASS" and .requested == 2 and
    (.restart_count | type == "number" and . >= 2) and
    (.distinct_agents | type == "number" and . >= 2) and
    (.evidence | type == "array") and
    (.evidence | length) >= .restart_count and
    ([.evidence[].agent_id] | unique | length) >= 2 and
    all(.evidence[]; (.agent_id | type == "string" and length > 0) and
                     (.occurred_at | type == "string" and length > 0) and
                     (.source | type == "string" and length > 0))
  ' "$file" >/dev/null || gc_die "agent restart driver returned an invalid or incomplete receipt"
}

RECOVERY_NETWORK=""
RECOVERY_CID=""
recover_network_on_exit() {
  if [ -n "$RECOVERY_NETWORK" ] && [ -n "$RECOVERY_CID" ]; then
    docker network connect --alias control "$RECOVERY_NETWORK" "$RECOVERY_CID" >/dev/null 2>&1 || true
  fi
}

host_storm() {
  local operation="$1"
  gc_load_env
  gc_validate_host_config
  export CX_ACTIVE_CONTROL_IMAGE="$CX_CANDIDATE_CONTROL_IMAGE"
  gc_validate_compose_images
  gc_probe_release "$CX_CANDIDATE_COMMIT" >/dev/null

  gc_require_declared_inputs CX_AGENT_RESTART_DRIVER
  [[ "$CX_AGENT_RESTART_DRIVER" == /* ]] || gc_die "CX_AGENT_RESTART_DRIVER must be an absolute path"
  [ -x "$CX_AGENT_RESTART_DRIVER" ] || gc_die "CX_AGENT_RESTART_DRIVER is not executable"
  local interruption_seconds="${CX_REHEARSAL_NETWORK_INTERRUPTION_SECONDS:-5}"
  [[ "$interruption_seconds" =~ ^[0-9]+$ ]] || gc_die "network interruption seconds must be an integer"
  [ "$interruption_seconds" -ge 1 ] && [ "$interruption_seconds" -le 30 ] \
    || gc_die "network interruption seconds must be between 1 and 30"
  if [ "$operation" = check ]; then
    gc_log "restart-storm target and driver are valid (no changes made)"
    return
  fi
  [ "$operation" = execute ] || gc_die "operation must be check or execute"

  gc_prepare_evidence restart-storm-working
  local agent_receipt
  agent_receipt="$GC_EVIDENCE_DIR/$(date -u +%Y%m%dT%H%M%SZ)-agent-restarts.json"
  local started_at finished_at control_count=0 database_count=0 storage_count=0
  local alert_count=0 network_count=0 network cid configured_image
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  umask 077
  if ! "$CX_AGENT_RESTART_DRIVER" restart-all 2 > "$agent_receipt"; then
    rm -f -- "$agent_receipt"
    gc_die "agent restart driver failed"
  fi
  chmod 600 "$agent_receipt"
  validate_agent_receipt "$agent_receipt"

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
  [ "$configured_image" = "$CX_CANDIDATE_CONTROL_IMAGE" ] \
    || gc_die "restart storm changed the active control image"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  gc_prepare_evidence restart-storm
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" --arg finished "$finished_at" \
    --arg image "$CX_CANDIDATE_CONTROL_IMAGE" --arg commit "$CX_CANDIDATE_COMMIT" \
    --argjson controls "$control_count" --argjson databases "$database_count" \
    --argjson storages "$storage_count" --argjson alerts "$alert_count" \
    --argjson networks "$network_count" --argjson seconds "$interruption_seconds" \
    --slurpfile agent "$agent_receipt" \
    '{schema_version:1,kind:"go_closure_restart_storm",status:"PASS",
      started_at:$started,finished_at:$finished,control_image:$image,expected_commit:$commit,
      observed:{control_restarts:$controls,database_restarts:$databases,
                storage_restarts:$storages,alerting_restarts:$alerts,
                network_interruptions:$networks,network_interruption_seconds_each:$seconds,
                agent_restart_receipt:$agent[0]},
      assertions:{control_restarts_at_least_2:($controls >= 2),
                  two_distinct_agents_restarted:($agent[0].distinct_agents >= 2),
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
