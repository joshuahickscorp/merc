#!/usr/bin/env bash
set -euo pipefail

# Run the strict, exact-path evidence-chain validator on the staging host. The
# caller supplies receipt paths captured from individual adapter PASS lines;
# this command never searches for a newest receipt or accepts a glob.

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=ops/scripts/lib/go-closure-common.sh
. "$ROOT/ops/scripts/lib/go-closure-common.sh"

usage() {
  cat >&2 <<'USAGE'
usage: ops/scripts/go-closure-evidence-proof.sh --target local|ssh \
  --deploy PATH --rollback PATH --restart PATH --canary PATH --soak PATH --governance PATH
USAGE
  exit 2
}

host_prove() {
  [ "$#" -eq 6 ] || usage
  gc_require_command python3
  gc_load_env
  gc_validate_host_config
  python3 "$ROOT/ops/scripts/validate-go-closure-evidence-chain.py" \
    --root "$GC_ROOT" \
    --commit "$MERC_CANDIDATE_COMMIT" \
    --image "$MERC_CANDIDATE_CONTROL_IMAGE" \
    --checked-at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --deploy "$1" --rollback "$2" --restart "$3" --canary "$4" --soak "$5" --governance "$6"
}

if [ "${1:-}" = --host ]; then
  shift
  host_prove "$@"
  exit
fi

target="" deploy="" rollback="" restart="" canary="" soak="" governance=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --target) shift; target="${1:-}" ;;
    --deploy) shift; deploy="${1:-}" ;;
    --rollback) shift; rollback="${1:-}" ;;
    --restart) shift; restart="${1:-}" ;;
    --canary) shift; canary="${1:-}" ;;
    --soak) shift; soak="${1:-}" ;;
    --governance) shift; governance="${1:-}" ;;
    *) usage ;;
  esac
  shift
done
case "$target" in local|ssh) ;; *) usage ;; esac
for path in "$deploy" "$rollback" "$restart" "$canary" "$soak" "$governance"; do
  gc_validate_safe_arg "$path"
done
gc_require_command jq
gc_load_env
gc_require_declared_inputs STAGING_DEPLOYMENT_ROOT
if [ "$target" = ssh ]; then gc_validate_ssh_target; fi
gc_sync_bundle "$target"
gc_run_on_target "$target" ops/scripts/go-closure-evidence-proof.sh \
  "$deploy" "$rollback" "$restart" "$canary" "$soak" "$governance"
