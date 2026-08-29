#!/usr/bin/env bash
set -euo pipefail

# Controlled Level-B teardown. This stops only services named by the audited
# compose manifest. It deliberately does not use --volumes, prune images,
# remove evidence, or touch the separately provisioned operator environment.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/go-closure-common.sh
. "$ROOT/ops/scripts/lib/go-closure-common.sh"

usage() {
  echo "usage: ops/scripts/go-closure-destroy.sh --target local|ssh --execute" >&2
  exit 2
}

host_destroy() {
  gc_require_command jq
  gc_load_env
  gc_validate_host_config
  local started finished running
  started="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  gc_prepare_evidence destroy
  gc_compose down --remove-orphans
  running="$(gc_compose ps -q || true)"
  [ -z "$running" ] || gc_die "compose services remain after controlled teardown"
  finished="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started" --arg finished "$finished" \
    --arg commit "$MERC_CANDIDATE_COMMIT" --arg image "$MERC_CANDIDATE_CONTROL_IMAGE" \
    '{schema_version:1,kind:"go_closure_controlled_teardown",status:"PASS",
      started_at:$started,finished_at:$finished,candidate_commit:$commit,control_image:$image,
      assertions:{compose_services_stopped:true,persistent_volumes_preserved:true,
                  evidence_preserved:true,operator_environment_preserved:true},
      policy:{stripe_live_mode:false,real_value:false,secret_values_recorded:false}}'
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 1 ] || usage
  host_destroy
  exit
fi

target="" execute=false
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --execute) execute=true ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
[ "$execute" = true ] || usage
gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
gc_sync_bundle "$target"
gc_run_on_target "$target" ops/scripts/go-closure-destroy.sh
