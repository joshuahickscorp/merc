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
for env_file in "$ROOT/.env" "${MERC_GO_CLOSURE_ENV_FILE:-$ROOT/.env.go-closure}"; do
  [ -f "$env_file" ] || continue
  set -a
  # shellcheck disable=SC1090
  . "$env_file"
  set +a
done

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

if present MERC_CANARY_APPROVED_BUYER_EMAILS && present MERC_CANARY_APPROVED_WORKER_IDS; then
  buyer_count="$(printf '%s' "$MERC_CANARY_APPROVED_BUYER_EMAILS" | awk -F, '{print NF}')"
  worker_count="$(printf '%s' "$MERC_CANARY_APPROVED_WORKER_IDS" | awk -F, '{print NF}')"
  worker_valid_count="$(printf '%s' "$MERC_CANARY_APPROVED_WORKER_IDS" | tr ',' '\n' | \
    awk '/^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-5][0-9a-fA-F]{3}-[89aAbB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$/ {n++} END {print n+0}')"
  [ "$buyer_count" = 2 ] && [ "$worker_count" = 2 ] && [ "$worker_valid_count" = 2 ] && canary_ok=true
fi

if present GITHUB_RELEASE_REVIEWER_LOGIN && \
  [ "$GITHUB_RELEASE_REVIEWER_LOGIN" != "joshuahickscorp" ] && command_present gh; then
  reviewer_permission="$(gh api \
    "repos/joshuahickscorp/computexchange/collaborators/$GITHUB_RELEASE_REVIEWER_LOGIN/permission" \
    --jq .permission 2>/dev/null || true)"
  case "$reviewer_permission" in admin|maintain|write) review_ok=true ;; esac
fi

candidate_commit="$(git -C "$ROOT" rev-parse HEAD)"
if readable_path GOVERNANCE_APPROVAL_BUNDLE_PATH; then
  approval_keys='["legal","license","payments","privacy","security","support","tax","trust_safety"]'
  exercise_keys='["asset_and_model_provenance","backup_tombstone","dsar_export_deletion","security_tabletop","support_tabletop"]'
  jq -e \
    --arg candidate "$candidate_commit" \
    --argjson approval_keys "$approval_keys" \
    --argjson exercise_keys "$exercise_keys" '
      .schema_version == 1 and
      .candidate_commit == $candidate and
      .scope == "supervised_stripe_test_mode_private_canary" and
      ((.approvals | keys) == $approval_keys) and
      (all(.approvals[];
        .status == "APPROVED" and
        (.approver | type == "string" and length > 0) and
        (.organization | type == "string" and length > 0) and
        (.reviewed_scope | type == "string" and length > 0) and
        (.evidence_uri | type == "string" and length > 0) and
        (.approved_at | type == "string" and length > 0))) and
      ((.exercises | keys) == $exercise_keys) and
      (all(.exercises[];
        .status == "PASS" and
        (.evidence_uri | type == "string" and length > 0) and
        (.completed_at | type == "string" and length > 0)))
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
