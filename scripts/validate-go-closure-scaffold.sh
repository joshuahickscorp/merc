#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
COMPOSE="$ROOT/ops/staging/compose.go-closure.yml"
INPUTS="$ROOT/ops/go-closure-inputs.json"
ENV_EXAMPLE="$ROOT/ops/staging/env.go-closure.example"

die() { printf 'validate-go-closure: %s\n' "$*" >&2; exit 1; }
pass() { printf 'validate-go-closure: PASS %s\n' "$*"; }
skip() { printf 'validate-go-closure: SKIP %s\n' "$*"; }

command -v jq >/dev/null 2>&1 || die "jq is required"
[ -f "$COMPOSE" ] || die "missing compose manifest"
[ -f "$INPUTS" ] || die "missing operator-input declaration"
[ -f "$ENV_EXAMPLE" ] || die "missing environment template"

jq -e '
  .schema_version == 1 and .policy.stripe_live_mode == "refused" and
  ([.inputs[].name] | unique | length) == (.inputs | length) and
  all(.inputs[]; (.name | test("^[A-Z0-9_]+$")) and
                 (.minimum_scope | length > 0) and (.verification | length > 0) and
                 (.unblocks | type == "array" and length > 0))
' "$INPUTS" >/dev/null || die "operator-input JSON contract is invalid"
pass "operator-input JSON contract"

while IFS= read -r required_var; do
  # The host scripts derive this internal selector from the declared candidate
  # or prior digest; operators never supply it directly.
  [ "$required_var" != CX_ACTIVE_CONTROL_IMAGE ] || continue
  jq -e --arg name "$required_var" '.inputs | any(.name == $name)' "$INPUTS" >/dev/null \
    || die "required compose input $required_var is absent from ops/go-closure-inputs.json"
done < <(rg -o '\$\{[A-Z0-9_]+:\?' "$COMPOSE" | sed -E 's/^\$\{//; s/:\?$//' | sort -u)
pass "required compose inputs are declared in the operator contract"

if rg -n '^[[:space:]]*build:' "$COMPOSE" >/dev/null; then
  die "staging compose must not contain a build directive"
fi
if rg -n 'image:.*:-' "$COMPOSE" >/dev/null; then
  die "staging compose image variables must not have mutable defaults"
fi
while IFS= read -r image_line; do
  case "$image_line" in
    *'${CX_ACTIVE_CONTROL_IMAGE:?'*|*'${CX_PROMETHEUS_IMAGE:?'*|*'${CX_ALERTMANAGER_IMAGE:?'*|*'${CX_GRAFANA_IMAGE:?'*|*'${CX_NODE_EXPORTER_IMAGE:?'*) ;;
    *@sha256:*)
      [[ "$image_line" =~ @sha256:[0-9a-f]{64}$ ]] \
        || die "literal image is not pinned to 64 lowercase digest hex: $image_line"
      ;;
    *) die "unapproved image expression: $image_line" ;;
  esac
done < <(sed -n 's/^[[:space:]]*image:[[:space:]]*//p' "$COMPOSE")
pass "no build path and immutable image contract"

for required in \
  CX_CANARY_MODE CX_CANARY_APPROVED_BUYER_EMAILS CX_CANARY_APPROVED_WORKER_IDS \
  CX_CANARY_APPROVED_AGENT_VERSIONS CX_CANARY_APPROVED_BUILD_HASHES \
  CX_CANARY_MAX_ACTIVE_BUYERS CX_CANARY_MAX_ACTIVE_WORKERS \
  CX_CANARY_MAX_QUEUED_JOBS CX_CANARY_MAX_DAILY_JOBS; do
  rg -q "^[[:space:]]*$required:" "$COMPOSE" || die "compose omits $required"
done
rg -q '^\.env\.go-closure$' "$ROOT/.gitignore" || die ".env.go-closure is not ignored"
git -C "$ROOT" check-ignore -q .env.go-closure || die ".env.go-closure ignore rule is ineffective"
pass "canary envelope and ignored secret file"

scripts=(
  scripts/go-closure-deploy.sh
  scripts/go-closure-rollback-rehearsal.sh
  scripts/go-closure-restart-storm.sh
  scripts/go-closure-canary-rehearsal.sh
  scripts/go-closure-soak.sh
  scripts/validate-go-closure-scaffold.sh
  scripts/lib/go-closure-common.sh
)
for script in "${scripts[@]}"; do
  [ -f "$ROOT/$script" ] || die "missing $script"
  bash -n "$ROOT/$script" || die "bash syntax failed for $script"
done
pass "bash syntax for staging harness"

while IFS= read -r documented_script; do
  [ -f "$ROOT/$documented_script" ] || die "README references missing $documented_script"
done < <(rg -o 'scripts/[a-z0-9-]+[.]sh' "$ROOT/ops/staging/README.md" | sort -u)
pass "all README script references exist"

for script in \
  scripts/go-closure-deploy.sh scripts/go-closure-rollback-rehearsal.sh \
  scripts/go-closure-restart-storm.sh scripts/go-closure-canary-rehearsal.sh \
  scripts/go-closure-soak.sh; do
  rg -q -- '--execute' "$ROOT/$script" || die "$script lacks explicit execution gate"
done
rg -q 'duration.*86400|86400.*duration' "$ROOT/scripts/go-closure-soak.sh" \
  || die "soak script lacks 24-hour qualification gate"
pass "explicit mutation and 24-hour qualification gates"

for count in \
  'approved_buyer_identity:2' 'distinct_metal_agent:2' 'embed_success:20' \
  'batch_infer_success:20' 'cancelled_job:5' 'forced_retry:5' \
  'stale_lease_recovery:3' 'stale_attempt_commit_rejection:3' \
  'buyer_webhook_retry_sequence:3'; do
  scenario="${count%:*}"
  minimum="${count#*:}"
  rg -q "^[[:space:]]+$scenario$" "$ROOT/scripts/go-closure-canary-rehearsal.sh" \
    || die "missing canary scenario $scenario"
  rg -q "required_counts:.*" "$ROOT/scripts/go-closure-canary-rehearsal.sh" \
    || die "missing canary count ledger"
  case "$scenario" in
    approved_buyer_identity) label=approved_buyer_identities ;;
    distinct_metal_agent) label=distinct_metal_agents ;;
    embed_success) label=successful_embed_jobs ;;
    batch_infer_success) label=successful_batch_infer_jobs ;;
    cancelled_job) label=cancelled_jobs ;;
    forced_retry) label=forced_retries ;;
    stale_lease_recovery) label=stale_lease_recoveries ;;
    stale_attempt_commit_rejection) label=stale_attempt_commit_rejections ;;
    buyer_webhook_retry_sequence) label=buyer_webhook_retry_sequences ;;
  esac
  rg -q "$label:$minimum" "$ROOT/scripts/go-closure-canary-rehearsal.sh" \
    || die "wrong minimum for $scenario"
done
pass "mandatory canary workload counts"

if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -x --severity=warning "${scripts[@]/#/$ROOT/}" || die "shellcheck failed"
  pass "shellcheck"
else
  skip "shellcheck is not installed"
fi

if docker compose version >/dev/null 2>&1 && [ -f "$ROOT/.env.go-closure" ]; then
  # Never render the config to stdout: it contains substituted secret values.
  set -a
  # shellcheck disable=SC1091
  . "$ROOT/.env.go-closure"
  set +a
  export CX_ACTIVE_CONTROL_IMAGE="${CX_CANDIDATE_CONTROL_IMAGE:-}"
  docker compose --env-file "$ROOT/.env.go-closure" -f "$COMPOSE" config -q \
    || die "docker compose config failed"
  pass "docker compose config"
else
  skip "docker compose config (Compose v2 and an ignored .env.go-closure are both required)"
fi

pass "GO-closure staging scaffold (this is not deployment or canary evidence)"
