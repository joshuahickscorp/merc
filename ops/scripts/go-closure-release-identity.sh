#!/usr/bin/env bash
set -euo pipefail

# Compare the complete operator-input profile on the staging host without
# printing any value.  The outer command syncs only this reviewed non-secret
# harness before the remote host sources its separately provisioned 0600 file.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/go-closure-common.sh
. "$ROOT/ops/scripts/lib/go-closure-common.sh"

usage() {
  echo "usage: ops/scripts/go-closure-release-identity.sh --target local|ssh" >&2
  exit 2
}

profile_sha256() {
  {
    printf 'merc-level-b-remote-profile-v1\0'
    while IFS= read -r name; do
      printf '%s\0%s\0' "$name" "${!name}"
    done < <(jq -r '.inputs[].name' "$GC_INPUTS" | LC_ALL=C sort)
  } | if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  else
    sha256sum | awk '{print $1}'
  fi
}

host_identity() {
  gc_require_command jq
  gc_load_env
  gc_validate_host_config
  local profile
  profile="$(profile_sha256)"
  [[ "$profile" =~ ^[0-9a-f]{64}$ ]] || gc_die "profile digest is not SHA-256"
  jq -cn \
    --arg profile "$profile" \
    --arg commit "$MERC_CANDIDATE_COMMIT" \
    --arg image "$MERC_CANDIDATE_CONTROL_IMAGE" \
    '{schema_version:1,kind:"merc_level_b_remote_input_profile",status:"PASS",
      profile_sha256:$profile,candidate_commit:$commit,control_image:$image,
      secret_values_recorded:false}'
}

if [ "${MERC_RELEASE_IDENTITY_PROFILE_SELF_TEST:-}" = 1 ]; then
  gc_require_command jq
  profile_sha256
  exit
fi

if [ "${1:-}" = --host ]; then
  [ "$#" -eq 1 ] || usage
  host_identity
  exit
fi

target=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac

gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
# The deployment adapter deliberately does not transfer the environment file.
# This copy contains only reviewed ops/scripts/manifests and lets a first launch
# query the remote profile before it mutates a service.
gc_sync_bundle "$target"
gc_run_on_target "$target" ops/scripts/go-closure-release-identity.sh
