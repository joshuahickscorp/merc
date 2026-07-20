#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/lib/go-closure-common.sh
. "$ROOT/scripts/lib/go-closure-common.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/go-closure-deploy.sh --target local|ssh --activate candidate|prior --check|--execute

--check validates an already provisioned target and makes no deployment change.
--execute syncs the non-secret bundle and activates the selected immutable image.
USAGE
  exit 2
}

host_deploy() {
  local operation="$1" activation="$2" active_image expected_commit
  gc_load_env
  gc_validate_host_config
  case "$activation" in
    candidate)
      active_image="$CX_CANDIDATE_CONTROL_IMAGE"
      expected_commit="$CX_CANDIDATE_COMMIT"
      ;;
    prior)
      active_image="$CX_PRIOR_CONTROL_IMAGE"
      expected_commit="$CX_PRIOR_COMMIT"
      ;;
    *) gc_die "activation must be candidate or prior" ;;
  esac
  export CX_ACTIVE_CONTROL_IMAGE="$active_image"
  gc_validate_compose_images
  if [ "$operation" = check ]; then
    gc_log "target configuration is valid for $activation (no changes made)"
    return
  fi
  [ "$operation" = execute ] || gc_die "host operation must be check or execute"

  gc_materialize_alert_secret
  local rendered_image
  while IFS= read -r rendered_image; do
    gc_pull_exact "$rendered_image"
  done < <(gc_compose config --images)

  local started_at finished_at version tls cid configured_image image_id
  started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  gc_compose up -d --remove-orphans
  local service
  for service in postgres minio control caddy alertmanager prometheus grafana node-exporter; do
    gc_wait_service "$service" 300
  done
  gc_wait_http http://127.0.0.1:9090/-/ready 180
  gc_wait_http http://127.0.0.1:9093/-/ready 180
  gc_wait_http http://127.0.0.1:3000/api/health 180
  version="$(gc_probe_release "$expected_commit")"
  tls="$(gc_tls_receipt)"
  cid="$(gc_compose ps -q control)"
  configured_image="$(docker inspect -f '{{.Config.Image}}' "$cid")"
  image_id="$(docker inspect -f '{{.Image}}' "$cid")"
  [ "$configured_image" = "$active_image" ] \
    || gc_die "running control is configured with $configured_image, not $active_image"
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  gc_prepare_evidence "deploy-$activation"
  gc_atomic_json "$GC_EVIDENCE_FILE" -n \
    --arg started "$started_at" \
    --arg finished "$finished_at" \
    --arg activation "$activation" \
    --arg host "$STAGING_TLS_HOSTNAME" \
    --arg image "$active_image" \
    --arg image_id "$image_id" \
    --arg commit "$expected_commit" \
    --argjson version "$version" \
    --arg tls "$tls" \
    --arg alert_receiver "$ALERT_RECEIVER_NAME" \
    '{schema_version:1,kind:"go_closure_deploy",status:"PASS",
      scope:"supervised_stripe_test_mode_private_canary",
      started_at:$started,finished_at:$finished,activation:$activation,
      endpoint:$host,control_image:$image,control_image_id:$image_id,
      expected_commit:$commit,reported_version:$version,
      tls_certificate_observation:$tls,
      observability:{prometheus_ready:true,alertmanager_ready:true,receiver_name:$alert_receiver},
      policy:{stripe_live_mode:false,real_value:false,unrestricted_public_access:false,
              secret_values_recorded:false}}'
  gc_log "PASS receipt: $GC_EVIDENCE_FILE"
}

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 3 ] || usage
  host_deploy "$2" "$3"
  exit
fi

target="" activation="" operation=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --activate) shift; activation="${1:-}" ;;
    --check) operation=check ;;
    --execute) operation=execute ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
case "$activation" in candidate|prior) ;; *) usage ;; esac
case "$operation" in check|execute) ;; *) usage ;; esac

gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_TLS_HOSTNAME STAGING_DEPLOYMENT_ROOT
gc_validate_absolute_path STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
if [ "$operation" = execute ]; then gc_sync_bundle "$target"; fi
gc_run_on_target "$target" scripts/go-closure-deploy.sh "$operation" "$activation"
