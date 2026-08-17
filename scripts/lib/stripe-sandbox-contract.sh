#!/usr/bin/env bash
# Shared, non-secret Stripe Sandbox authority for the reviewed Level B
# candidate. This file is sourced by the preflight and scenario driver so the
# runtime currency, endpoint identity, event schema, and payout fixture cannot
# drift into separate test contracts.

MERC_STRIPE_API_VERSION="2025-06-30.basil"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CANDIDATE_CURRENCY="cad"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY="CA"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING="11000-000"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CANDIDATE_PAYOUT_SUCCESS_ACCOUNT="000123456789"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CANDIDATE_PAYOUT_FAILURE_ACCOUNT="000111111116"
# Custom CA is the test-matrix path: one command can finish KYC and payouts
# via the API after Connect signup. Express onboarding is the product path.
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CONNECT_ACCOUNT_TYPE="custom"
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CONNECT_WEBHOOK_EVENTS="account.updated,payout.created,payout.paid,payout.failed"
# The operator command that drives every Connect-gated scenario after signup.
# shellcheck disable=SC2034 # consumed by scripts that source this authority
MERC_STRIPE_CONNECT_REMAINDER_COMMAND="scripts/stripe-sandbox-connect.sh"

merc_stripe_valid_staging_hostname() {
  local hostname="${1:-}" label
  local -a labels=()
  [ "${#hostname}" -le 253 ] || return 1
  [[ "$hostname" == *.* ]] || return 1
  [[ "$hostname" != *://* && "$hostname" != */* && "$hostname" != *:* ]] || return 1
  IFS='.' read -ra labels <<< "$hostname"
  [ "${#labels[@]}" -ge 2 ] || return 1
  for label in "${labels[@]}"; do
    [ "${#label}" -le 63 ] || return 1
    [[ "$label" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]] || return 1
  done
}

merc_stripe_expected_billing_url() {
  local hostname
  hostname="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  printf 'https://%s/v1/stripe/webhook' "$hostname"
}

merc_stripe_expected_connect_url() {
  local hostname
  hostname="$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
  printf 'https://%s/v1/stripe/connect-webhook' "$hostname"
}

merc_stripe_endpoint_contract() {
  local endpoint_json="$1" expected_id="$2" expected_url="$3" required_csv="$4"
  printf '%s' "$endpoint_json" | jq -e \
    --arg id "$expected_id" \
    --arg url "$expected_url" \
    --arg version "$MERC_STRIPE_API_VERSION" \
    --arg required "$required_csv" '
      . as $endpoint |
      .id == $id and
      .url == $url and
      .livemode == false and
      .status == "enabled" and
      .api_version == $version and
      (.enabled_events | type == "array") and
      ((.enabled_events | index("*")) != null or
       all($required | split(",")[];
         . as $event | ($endpoint.enabled_events | index($event)) != null))
    ' >/dev/null
}

merc_stripe_distinct_endpoint_ids() {
  local billing_id="${1:-}" connect_id="${2:-}"
  [[ "$billing_id" =~ ^we_[A-Za-z0-9]+$ ]] &&
    [[ "$connect_id" =~ ^we_[A-Za-z0-9]+$ ]] &&
    [ "$billing_id" != "$connect_id" ]
}
