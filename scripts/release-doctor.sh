#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib/stripe-sandbox-contract.sh
source "$ROOT/scripts/lib/stripe-sandbox-contract.sh"
GROUP="all"
if [ "${1:-}" = "--check" ]; then
  GROUP="${2:-}"
  case "$GROUP" in staging|backup|stripe|alert|canary|review|governance|all) ;; *)
    echo "usage: scripts/release-doctor.sh [--check staging|backup|stripe|alert|canary|review|governance|all]" >&2
    exit 2
  esac
fi

# Values may be supplied by the process or by a deliberately ignored operator
# file.  The doctor reports only validity classes and booleans.
if [ "${MERC_RELEASE_DOCTOR_NO_ENV_FILE:-}" != 1 ]; then
  for env_file in "$ROOT/.env" "${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"; do
    [ -f "$env_file" ] || continue
    set -a
    # shellcheck disable=SC1090
    . "$env_file"
    set +a
  done
fi

present() { [ -n "${!1:-}" ]; }
# shellcheck disable=SC2053 # callers deliberately supply an approved glob
matches() { local name="$1" pattern="$2"; [[ "${!name:-}" == $pattern ]]; }
readable_path() { present "$1" && [ -r "${!1}" ]; }
command_present() { command -v "$1" >/dev/null 2>&1; }

stripe_class() {
  local value="${STRIPE_SECRET_KEY:-}"
  case "$value" in
    '') echo missing ;;
    sk_test_*) echo sk_test ;;
    sk_live_*) echo sk_live ;;
    rk_test_*) echo rk_test ;;
    rk_live_*) echo rk_live ;;
    pk_test_*) echo publishable_test ;;
    pk_live_*) echo publishable_live ;;
    whsec_*) echo webhook_present ;;
    *) echo unknown ;;
  esac
}

live_alias_present=false
for stripe_name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
  STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
  case "${!stripe_name:-}" in sk_live_*|rk_live_*|pk_live_*) live_alias_present=true ;; esac
done

staging_ok=false
backup_ok=false
stripe_ok=false
alert_ok=false
canary_ok=false
review_ok=false
governance_ok=false

present STAGING_SSH_TARGET && present STAGING_TLS_HOSTNAME && \
  matches STAGING_DEPLOYMENT_ROOT '/*' && staging_ok=true

present MERC_BACKUP_OFFSITE && matches MERC_BACKUP_OFFSITE 's3://*' && \
  present AWS_ACCESS_KEY_ID && present AWS_SECRET_ACCESS_KEY && \
  matches MERC_BACKUP_ENCRYPTION_RECIPIENT 'age1*' && \
  readable_path MERC_BACKUP_DECRYPTION_IDENTITY_FILE && \
  command_present age && command_present aws && backup_ok=true

[ "$live_alias_present" = false ] && \
  (matches STRIPE_SECRET_KEY 'sk_test_*' || matches STRIPE_SECRET_KEY 'rk_test_*') && \
  matches STRIPE_WEBHOOK_SECRET 'whsec_*' && \
  matches MERC_CONNECT_WEBHOOK_SECRET 'whsec_*' && \
  [ "${STRIPE_WEBHOOK_SECRET:-}" != "${MERC_CONNECT_WEBHOOK_SECRET:-}" ] && \
  matches MERC_CONNECT_CLIENT_ID 'ca_*' && \
  matches STRIPE_TEST_CONNECTED_ACCOUNT_ID 'acct_*' && \
  matches STRIPE_BILLING_WEBHOOK_ENDPOINT_ID 'we_*' && \
  matches STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID 'we_*' && \
  merc_stripe_distinct_endpoint_ids \
    "${STRIPE_BILLING_WEBHOOK_ENDPOINT_ID:-}" \
    "${STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID:-}" && \
  merc_stripe_valid_staging_hostname "${STAGING_TLS_HOSTNAME:-}" && \
  [ "$(printf '%s' "${MERC_SETTLEMENT_CURRENCY:-$MERC_STRIPE_CANDIDATE_CURRENCY}" \
      | tr '[:upper:]' '[:lower:]')" = "$MERC_STRIPE_CANDIDATE_CURRENCY" ] && \
  command_present stripe && stripe_ok=true

matches ALERT_RECEIVER_WEBHOOK_URL 'https://*' && present ALERT_RECEIVER_NAME && alert_ok=true

if present MERC_CANARY_APPROVED_BUYER_EMAILS && \
  present MERC_CANARY_APPROVED_WORKER_IDS && \
  present MERC_CANARY_APPROVED_AGENT_VERSIONS && \
  present MERC_CANARY_APPROVED_BUILD_HASHES && \
  present MERC_CANARY_SCENARIO_DRIVER && \
  present MERC_CANARY_APPROVED_DRIVER_SHA256 && \
  present MERC_AGENT_RESTART_DRIVER && \
  present MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256; then
  buyer_inputs_ok=false
  worker_inputs_ok=false
  runtime_inputs_ok=false
  jq -en --arg value "$MERC_CANARY_APPROVED_BUYER_EMAILS" '
    ($value | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
      map(select(length > 0))) as $items |
    ($items | length) == 2 and ($items | unique | length) == 2 and
    all($items[]; test("^[^@[:space:]]+@[^@[:space:]]+[.][^@[:space:]]+$"))
  ' >/dev/null && buyer_inputs_ok=true
  jq -en --arg value "$MERC_CANARY_APPROVED_WORKER_IDS" '
    ($value | split(",") | map(ascii_downcase | gsub("^\\s+|\\s+$"; "")) |
      map(select(length > 0))) as $items |
    ($items | length) == 2 and ($items | unique | length) == 2 and
    all($items[]; test("^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"))
  ' >/dev/null && worker_inputs_ok=true
  jq -en \
    --arg versions "$MERC_CANARY_APPROVED_AGENT_VERSIONS" \
    --arg builds "$MERC_CANARY_APPROVED_BUILD_HASHES" '
    ($versions | split(",") | map(gsub("^\\s+|\\s+$"; "")) |
      map(select(length > 0))) as $versions |
    ($builds | split(",") | map(gsub("^\\s+|\\s+$"; "")) |
      map(select(length > 0))) as $builds |
    ($versions | length) >= 1 and ($versions | length) <= 4 and
    ($versions | unique | length) == ($versions | length) and
    all($versions[]; test("^[0-9]+[.][0-9]+[.][0-9]+([+-][0-9A-Za-z.-]+)?$")) and
    ($builds | length) >= 1 and ($builds | length) <= 4 and
    ($builds | unique | length) == ($builds | length) and
    all($builds[]; test("^[0-9a-f]{16}$"))
  ' >/dev/null && runtime_inputs_ok=true
  [ "$buyer_inputs_ok" = true ] && [ "$worker_inputs_ok" = true ] && \
    [ "$runtime_inputs_ok" = true ] && \
    [[ "$MERC_CANARY_SCENARIO_DRIVER" == /* ]] && \
    [[ "$MERC_AGENT_RESTART_DRIVER" == /* ]] && \
    [[ "$MERC_CANARY_APPROVED_DRIVER_SHA256" =~ ^[0-9a-f]{64}$ ]] && \
    [[ "$MERC_AGENT_RESTART_APPROVED_DRIVER_SHA256" =~ ^[0-9a-f]{64}$ ]] && \
    canary_ok=true
fi

if present GITHUB_RELEASE_REVIEWER_LOGIN && \
  [ "$GITHUB_RELEASE_REVIEWER_LOGIN" != "joshuahickscorp" ] && command_present gh; then
  reviewer_permission="$(gh api \
    "repos/joshuahickscorp/merc/collaborators/$GITHUB_RELEASE_REVIEWER_LOGIN/permission" \
    --jq .permission 2>/dev/null || true)"
  case "$reviewer_permission" in admin|maintain|write) review_ok=true ;; esac
fi

candidate_commit="$(git -C "$ROOT" rev-parse HEAD)"
if readable_path GOVERNANCE_APPROVAL_BUNDLE_PATH; then
  approval_keys='["legal","licensing","operations","payments","privacy","release_approval","security","supplier_policy"]'
  exercise_keys='["asset_and_model_provenance","backup_tombstone","dsar_export_deletion","security_tabletop","support_tabletop"]'
  jq -e \
    --arg candidate "$candidate_commit" \
    --argjson approval_keys "$approval_keys" \
    --argjson exercise_keys "$exercise_keys" '
      def bounded_text($maximum):
        type == "string" and
        length > 0 and
        utf8bytelength <= $maximum and
        . == (gsub("^\\s+|\\s+$"; "")) and
        ([explode[] | select(. < 32 or . == 127)] | length == 0);
      .schema_version == 1 and
      .candidate_commit == $candidate and
      .scope == "supervised_stripe_test_mode_private_canary" and
      ((.approvals | keys) == $approval_keys) and
      (all(.approvals[];
        (keys == ["approved_at","approver","evidence_uri","organization","reviewed_scope","status"]) and
        .status == "APPROVED" and
        (.approver | bounded_text(256)) and
        (.organization | bounded_text(256)) and
        (.reviewed_scope | bounded_text(4096)) and
        (.evidence_uri | bounded_text(2048)) and
        (.approved_at | type == "string" and
          test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$") and
          (utf8bytelength <= 64) and
          (fromdateiso8601 <= (now + 300))))) and
      ((.exercises | keys) == $exercise_keys) and
      (all(.exercises[];
        (keys == ["completed_at","evidence_uri","status"]) and
        .status == "PASS" and
        (.evidence_uri | bounded_text(2048)) and
        (.completed_at | type == "string" and
          test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$") and
          (utf8bytelength <= 64) and
          (fromdateiso8601 <= (now + 300))))) and
      ((.approvals.release_approval.approved_at | fromdateiso8601) as $release_at |
        (all(.approvals | to_entries[];
          .key == "release_approval" or
          (.value.approved_at | fromdateiso8601) <= $release_at)) and
        (all(.exercises[];
          (.completed_at | fromdateiso8601) <= $release_at)))
    ' "$GOVERNANCE_APPROVAL_BUNDLE_PATH" >/dev/null 2>&1 && governance_ok=true
fi

json="$(jq -nc \
  --arg selected "$GROUP" \
  --argjson staging "$staging_ok" \
  --argjson backup "$backup_ok" \
  --argjson stripe "$stripe_ok" \
  --argjson alert "$alert_ok" \
  --argjson canary "$canary_ok" \
  --argjson review "$review_ok" \
  --argjson governance "$governance_ok" \
  --arg settlement_currency "$MERC_STRIPE_CANDIDATE_CURRENCY" \
  --arg stripe_class "$(if [ "$live_alias_present" = true ]; then echo live_refused; else stripe_class; fi)" \
  --argjson docker "$(command_present docker && echo true || echo false)" \
  --argjson compose "$(docker compose version >/dev/null 2>&1 && echo true || echo false)" \
  --argjson age "$(command_present age && echo true || echo false)" \
  --argjson aws "$(command_present aws && echo true || echo false)" \
  '{schema_version:1,selected:$selected,policy:{secret_values_printed:false,stripe_live_mode:"refused"},
    groups:{staging:{ready:$staging},backup:{ready:$backup},
      stripe:{ready:$stripe,secret_key_class:$stripe_class,
        settlement_currency:$settlement_currency,endpoint_ids_must_be_distinct:true,
        staging_url_binding_required:true},
      alert:{ready:$alert},canary:{ready:$canary},review:{ready:$review},governance:{ready:$governance}},
    tools:{docker_cli:$docker,docker_compose:$compose,age:$age,aws_cli:$aws},
    ready:($staging and $backup and $stripe and $alert and $canary and $review and $governance)}')"

if [ "$GROUP" = all ]; then
  printf '%s\n' "$json"
else
  printf '%s\n' "$json" | jq --arg group "$GROUP" \
    '{schema_version,selected,policy,group:.groups[$group],tools,ready:.groups[$group].ready}'
fi

printf '%s\n' "$json" | jq -e \
  "if \"$GROUP\" == \"all\" then .ready else .groups[\"$GROUP\"].ready end" >/dev/null
