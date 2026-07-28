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
  [ "$required_var" != MERC_ACTIVE_CONTROL_IMAGE ] || continue
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
    *'${MERC_ACTIVE_CONTROL_IMAGE:?'*|*'${MERC_PROMETHEUS_IMAGE:?'*|*'${MERC_ALERTMANAGER_IMAGE:?'*|*'${MERC_GRAFANA_IMAGE:?'*|*'${MERC_NODE_EXPORTER_IMAGE:?'*) ;;
    *@sha256:*)
      [[ "$image_line" =~ @sha256:[0-9a-f]{64}$ ]] \
        || die "literal image is not pinned to 64 lowercase digest hex: $image_line"
      ;;
    *) die "unapproved image expression: $image_line" ;;
  esac
done < <(sed -n 's/^[[:space:]]*image:[[:space:]]*//p' "$COMPOSE")
pass "no build path and immutable image contract"

for required in \
  MERC_PAYMENT_MODE MERC_PAYMENT_PROVIDER MERC_SETTLEMENT_CURRENCY \
  MERC_CANARY_MODE MERC_CANARY_APPROVED_BUYER_EMAILS MERC_CANARY_APPROVED_WORKER_IDS \
  MERC_CANARY_APPROVED_AGENT_VERSIONS MERC_CANARY_APPROVED_BUILD_HASHES \
  MERC_CANARY_MAX_ACTIVE_BUYERS MERC_CANARY_MAX_ACTIVE_WORKERS \
  MERC_CANARY_MAX_QUEUED_JOBS MERC_CANARY_MAX_DAILY_JOBS; do
  rg -q "^[[:space:]]*$required:" "$COMPOSE" || die "compose omits $required"
done
rg -q '^[[:space:]]*MERC_SETTLEMENT_CURRENCY:[[:space:]]*"cad"$' "$COMPOSE" \
  || die "compose settlement currency must be the reviewed candidate authority cad"
rg -q '^[[:space:]]*MERC_PAYMENT_MODE:[[:space:]]*"test"$' "$COMPOSE" \
  || die "Level B compose must explicitly authorize test payment mode"
rg -q '^[[:space:]]*MERC_PAYMENT_PROVIDER:[[:space:]]*"stripe"$' "$COMPOSE" \
  || die "Level B compose must explicitly select the Stripe test provider"
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

[ -x "$ROOT/scripts/validate-canary-scenario-receipt.py" ] \
  || die "canary scenario receipt validator is missing or not executable"
[ -x "$ROOT/scripts/validate-agent-restart-receipt.py" ] \
  || die "agent restart receipt validator is missing or not executable"
[ -x "$ROOT/scripts/validate-go-closure-soak-receipt.py" ] \
  || die "GO-closure soak receipt validator is missing or not executable"
[ -x "$ROOT/scripts/validate-backup-verification-receipt.py" ] \
  || die "backup verification receipt validator is missing or not executable"
[ -x "$ROOT/scripts/validate-go-closure-evidence-chain.py" ] \
  || die "final evidence-chain validator is missing or not executable"
python3 -m py_compile "$ROOT/scripts/validate-canary-scenario-receipt.py" \
  || die "canary scenario receipt validator does not compile"
python3 -m py_compile "$ROOT/scripts/validate-agent-restart-receipt.py" \
  || die "agent restart receipt validator does not compile"
python3 -m py_compile "$ROOT/scripts/validate-go-closure-soak-receipt.py" \
  || die "GO-closure soak receipt validator does not compile"
python3 -m py_compile "$ROOT/scripts/validate-backup-verification-receipt.py" \
  || die "backup verification receipt validator does not compile"
python3 -m py_compile "$ROOT/scripts/validate-go-closure-evidence-chain.py" \
  || die "final evidence-chain validator does not compile"
for contract in \
  'MERC_CANARY_RUN_ID' 'MERC_CANARY_CANDIDATE_COMMIT' \
  'MERC_CANARY_CONTROL_IMAGE' 'MERC_CANARY_DRIVER_SHA256' \
  'MERC_CANARY_APPROVED_DRIVER_SHA256' '--scenario-started-at' \
  'corroborate_scenario' 'database_backed_scenarios_corroborated:true'; do
  rg -q -- "$contract" "$ROOT/scripts/go-closure-canary-rehearsal.sh" \
    || die "canary rehearsal lacks authority contract $contract"
done
rg -q 'scripts/validate-canary-scenario-receipt.py' "$ROOT/scripts/lib/go-closure-common.sh" \
  || die "deployment sync bundle omits the canary scenario receipt validator"
pass "exact-run canary receipt validator and database corroboration"

for contract in \
  'MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256' \
  'agent_sessions_transitioned' \
  'two_distinct_agents_restarted_from_database_session_transitions:true'; do
  rg -q -- "$contract" "$ROOT/scripts/go-closure-restart-storm.sh" \
    || die "restart storm lacks authority contract $contract"
done
rg -q 'scripts/validate-agent-restart-receipt.py' "$ROOT/scripts/lib/go-closure-common.sh" \
  || die "deployment sync bundle omits the agent restart receipt validator"
pass "reviewed restart driver and database process-session corroboration"

for contract in \
  'control container was recreated during soak' \
  'control_configured_image' \
  'no_control_restarts_or_recreates:true' \
  'raw_samples_independently_validated:true' \
  'scripts/validate-go-closure-soak-receipt.py'; do
  rg -q -- "$contract" "$ROOT/scripts/go-closure-soak.sh" \
    || die "soak lacks exact-candidate raw-sample authority contract $contract"
done
rg -q 'scripts/validate-go-closure-soak-receipt.py' "$ROOT/scripts/lib/go-closure-common.sh" \
  || die "deployment sync bundle omits the GO-closure soak receipt validator"
pass "uninterrupted candidate soak and raw-sample authority"

for contract in \
  'merc_offsite_backup_verification' \
  'merc_backup_invocation_result' \
  'independent_manifest_download:true' \
  'independent_ciphertext_download:true' \
  'scripts/validate-backup-verification-receipt.py'; do
  rg -q -- "$contract" "$ROOT/scripts/backup.sh" \
    || die "backup path lacks exact offsite verification contract $contract"
done
for contract in 'invocation_result' 'verification_receipt'; do
  rg -q -- "$contract" "$ROOT/scripts/go-closure-rollback-rehearsal.sh" \
    || die "rollback receipt does not embed the validated backup $contract"
done
rg -q 'scripts/validate-backup-verification-receipt.py' "$ROOT/scripts/lib/go-closure-common.sh" \
  || die "deployment sync bundle omits the backup verification validator"
pass "fresh encrypted offsite backup and independent-download authority"

for contract in \
  'ELIGIBLE_FOR_SUPERVISED_LEVEL_B_PRIVATE_CANARY_REVIEW' \
  'live_payment_activation.*False' \
  'validate-go-closure-soak-receipt.py' \
  'validate-backup-verification-receipt.py' \
  'validate-agent-restart-receipt.py' \
  'validate-canary-scenario-receipt.py'; do
  rg -q -- "$contract" "$ROOT/scripts/validate-go-closure-evidence-chain.py" \
    || die "final evidence-chain validator lacks authority contract $contract"
done
rg -q 'scripts/validate-go-closure-evidence-chain.py' "$ROOT/scripts/lib/go-closure-common.sh" \
  || die "deployment sync bundle omits the final evidence-chain validator"
pass "fresh ordered exact-candidate final evidence authority"

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
  export MERC_ACTIVE_CONTROL_IMAGE="${MERC_CANDIDATE_CONTROL_IMAGE:-}"
  docker compose --env-file "$ROOT/.env.go-closure" -f "$COMPOSE" config -q \
    || die "docker compose config failed"
  pass "docker compose config"
else
  skip "docker compose config (Compose v2 and an ignored .env.go-closure are both required)"
fi

pass "GO-closure staging scaffold (this is not deployment or canary evidence)"
