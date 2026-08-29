#!/usr/bin/env bash
# Shared fail-closed primitives for ops/scripts/alpha/*.
# Callers set `set -euo pipefail` before sourcing this file.

# shellcheck disable=SC2034
ALPHA_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
ALPHA_ROOT="$(cd "$ALPHA_LIB_DIR/../../.." && pwd -P)"
ALPHA_RECEIPT_DIR="${MERC_ALPHA_RECEIPT_DIR:-$ALPHA_ROOT/.artifacts/alpha}"
ALPHA_BOOT_RECEIPT="${MERC_ALPHA_BOOT_RECEIPT:-$ALPHA_ROOT/evidence/state/alpha-boot-green.json}"
ALPHA_GO_NO_GO="${MERC_ALPHA_GO_NO_GO:-$ALPHA_ROOT/ops/go-no-go.json}"

# Gate identifiers and P1-INDEPENDENT-APPROVAL state come from
# ops/go-no-go.json. This file does not independently drop or pass a gate.
# Order is enforced by ALPHA_PREREQS_* below.
ALPHA_GATES=(
  boot
  P1-STAGING
  P1-STRIPE-TEST
  P1-OFFSITE-RESTORE
  P1-ALERT-DELIVERY
  P1-CANARY-REHEARSAL
  P1-RECOVERY-SOAK
  P1-GOVERNANCE
)

# boot -> staging -> {stripe, offsite, alerts, canary} -> soak
ALPHA_PREREQS_boot=""
ALPHA_PREREQS_P1_STAGING="boot"
ALPHA_PREREQS_P1_STRIPE_TEST="boot P1-STAGING"
ALPHA_PREREQS_P1_OFFSITE_RESTORE="boot P1-STAGING"
ALPHA_PREREQS_P1_ALERT_DELIVERY="boot P1-STAGING"
ALPHA_PREREQS_P1_CANARY_REHEARSAL="boot P1-STAGING"
ALPHA_PREREQS_P1_RECOVERY_SOAK="boot P1-STAGING P1-STRIPE-TEST P1-OFFSITE-RESTORE P1-ALERT-DELIVERY P1-CANARY-REHEARSAL"
ALPHA_PREREQS_P1_GOVERNANCE="boot"

alpha_die() {
  printf 'alpha: FAIL %s\n' "$*" >&2
  exit 1
}

alpha_log() {
  printf 'alpha: %s\n' "$*" >&2
}

alpha_utc() {
  date -u +%Y-%m-%dT%H:%M:%SZ
}

alpha_require_command() {
  command -v "$1" >/dev/null 2>&1 || alpha_die "$1 is required"
}

alpha_gate_to_var() {
  printf '%s' "$1" | tr '-' '_'
}

alpha_prereqs_for() {
  local gate="$1" var
  var="ALPHA_PREREQS_$(alpha_gate_to_var "$gate")"
  printf '%s' "${!var-}"
}

alpha_receipt_path() {
  local gate="$1"
  if [ "$gate" = boot ]; then
    printf '%s' "$ALPHA_BOOT_RECEIPT"
    return
  fi
  printf '%s/%s.json' "$ALPHA_RECEIPT_DIR" "$gate"
}

# Classify a credential value without printing it.
alpha_stripe_class() {
  case "${1:-}" in
    '') printf 'missing' ;;
    sk_test_*|rk_test_*) printf 'test' ;;
    pk_test_*) printf 'publishable_test' ;;
    sk_live_*|rk_live_*|pk_live_*) printf 'live' ;;
    whsec_*) printf 'webhook' ;;
    *) printf 'unknown' ;;
  esac
}

# Refuse live Stripe before any network call. Safe to run with no env file.
alpha_reject_live_stripe() {
  local name value
  for name in STRIPE_SECRET_KEY STRIPE_LIVE_SECRET_KEY STRIPE_RESTRICTED_KEY \
    STRIPE_PUBLISHABLE_KEY NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY; do
    value="${!name:-}"
    if [ "$(alpha_stripe_class "$value")" = live ]; then
      alpha_die "$name has a live Stripe credential class; alpha scripts refuse it before any network access"
    fi
  done
  case "${MERC_PAYMENT_MODE:-}" in
    live)
      alpha_die "MERC_PAYMENT_MODE=live is prohibited in the alpha canary (test-mode only)"
      ;;
  esac
  case "${MERC_PAYOUT_EXPORT:-}" in
    ''|0|false|FALSE|no|NO) ;;
    *)
      alpha_die "MERC_PAYOUT_EXPORT is set; Level B test canary refuses payout export"
      ;;
  esac
}

alpha_env_mode() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then
    stat -f '%Lp' "$1"
  else
    stat -c '%a' "$1"
  fi
}

# Load ignored operator files if present. Never required for --print-runbook.
alpha_load_env_optional() {
  local env_file mode
  for env_file in \
    "${MERC_GO_CLOSURE_ENV_FILE:-$ALPHA_ROOT/.env.go-closure}" \
    "$ALPHA_ROOT/.env"; do
    [ -f "$env_file" ] || continue
    [ ! -L "$env_file" ] || alpha_die "$env_file must not be a symlink"
    mode="$(alpha_env_mode "$env_file")"
    [[ "$mode" =~ ^[0-7]*00$ ]] \
      || alpha_die "$env_file permissions are $mode; require chmod 600"
    set -a
    # shellcheck disable=SC1090
    . "$env_file"
    set +a
  done
  alpha_reject_live_stripe
}

alpha_boot_receipt_commit() {
  local file="${1:-$ALPHA_BOOT_RECEIPT}"
  [ -f "$file" ] || return 0
  jq -er '.commit // .source.commit // empty' "$file" 2>/dev/null || true
}

# Ledger state for a P1 id. open_p1 wins; dropped_p1 is the only way to
# de-scope a gate. lib.sh does not invent either list.
alpha_ledger_gate_state() {
  local gate="$1"
  if [ ! -f "$ALPHA_GO_NO_GO" ]; then
    printf 'unknown'
    return
  fi
  if ! command -v jq >/dev/null 2>&1; then
    printf 'unknown'
    return
  fi
  if jq -e --arg id "$gate" '.open_p1[]? | select(.id==$id)' "$ALPHA_GO_NO_GO" >/dev/null 2>&1; then
    printf 'open'
    return
  fi
  if jq -e --arg id "$gate" '
      ((.dropped_p1 // [])[])
      | if type=="object" then .id else . end
      | select(.==$id)
    ' "$ALPHA_GO_NO_GO" >/dev/null 2>&1; then
    printf 'dropped'
    return
  fi
  printf 'absent'
}

alpha_ledger_gate_owner() {
  local gate="$1"
  jq -er --arg id "$gate" '
    (.open_p1[]? | select(.id==$id) | .owner)
    // ((.dropped_p1 // [])[] | select(type=="object" and .id==$id) | .owner)
    // empty
  ' "$ALPHA_GO_NO_GO" 2>/dev/null || true
}

alpha_boot_status() {
  local file="$ALPHA_BOOT_RECEIPT" receipt_commit expected
  if [ ! -f "$file" ]; then
    printf 'missing'
    return
  fi
  if ! command -v jq >/dev/null 2>&1; then
    printf 'unknown'
    return
  fi
  if ! jq -e '
    .status == "PASS" and
    (.kind == "alpha_boot_green" or .kind == "release_image_boot") and
    .binding_status == "BOUND"
  ' "$file" >/dev/null 2>&1; then
    printf 'FAIL'
    return
  fi
  receipt_commit="$(alpha_boot_receipt_commit "$file")"
  expected="$(alpha_expected_commit)"
  if [ -z "$receipt_commit" ] || [ "$receipt_commit" != "$expected" ]; then
    printf 'FAIL'
    return
  fi
  printf 'PASS'
}

alpha_boot_is_green() {
  [ "$(alpha_boot_status)" = PASS ]
}

alpha_require_boot() {
  local status receipt_commit expected
  status="$(alpha_boot_status)"
  if [ "$status" = PASS ]; then
    return
  fi
  receipt_commit="$(alpha_boot_receipt_commit)"
  expected="$(alpha_expected_commit)"
  if [ -n "$receipt_commit" ] && [ "$receipt_commit" != "$expected" ]; then
    alpha_die "boot receipt commit $receipt_commit != candidate $expected (receipt $ALPHA_BOOT_RECEIPT). Re-seal at this HEAD after G070 is green, or refuse deploy. Deploy/execute is refused."
  fi
  alpha_die "boot is not green (receipt $ALPHA_BOOT_RECEIPT status=$status). Wait for the power-authority lane (VENDOR_WALL_UPPER_BOUND) to write a BOUND PASS receipt at this HEAD. Deploy/execute is refused."
}

alpha_receipt_status() {
  local gate="$1" file
  if [ "$gate" = boot ]; then
    alpha_boot_status
    return
  fi
  if [ "$(alpha_ledger_gate_state "$gate")" = dropped ]; then
    printf 'dropped'
    return
  fi
  file="$(alpha_receipt_path "$gate")"
  if [ ! -f "$file" ]; then
    printf 'missing'
    return
  fi
  jq -er '.status // "FAIL"' "$file" 2>/dev/null || printf 'FAIL'
}

alpha_require_gate() {
  local gate="$1" status
  status="$(alpha_receipt_status "$gate")"
  [ "$status" = PASS ] || alpha_die "gate $gate is not PASS (status=$status); refusing out-of-order execute"
}

alpha_require_prereqs() {
  local gate="$1" prereq
  # shellcheck disable=SC2086
  for prereq in $(alpha_prereqs_for "$gate"); do
    [ -n "$prereq" ] || continue
    alpha_require_gate "$prereq"
  done
}

alpha_require_execute_ready() {
  local gate="$1"
  alpha_reject_live_stripe
  alpha_require_boot
  alpha_require_prereqs "$gate"
}

# --check reports blockers and exits 1 if the gate cannot execute.
alpha_check_ready() {
  local gate="$1" prereq status ready=0
  alpha_reject_live_stripe
  printf 'gate: %s\n' "$gate"
  status="$(alpha_boot_status)"
  printf 'boot: %s (%s)\n' "$status" "$ALPHA_BOOT_RECEIPT"
  [ "$status" = PASS ] || ready=1
  # shellcheck disable=SC2086
  for prereq in $(alpha_prereqs_for "$gate"); do
    [ -n "$prereq" ] || continue
    status="$(alpha_receipt_status "$prereq")"
    printf 'prereq %s: %s\n' "$prereq" "$status"
    [ "$status" = PASS ] || ready=1
  done
  return "$ready"
}

alpha_write_receipt() {
  local gate="$1" status="$2" kind="${3:-alpha_gate_receipt}"
  local dest tmp
  dest="$(alpha_receipt_path "$gate")"
  mkdir -p "$(dirname "$dest")"
  chmod 700 "$(dirname "$dest")" 2>/dev/null || true
  tmp="${dest}.tmp.$$"
  umask 077
  jq -n \
    --arg gate "$gate" \
    --arg status "$status" \
    --arg kind "$kind" \
    --arg at "$(alpha_utc)" \
    --arg host "${STAGING_TLS_HOSTNAME:-${SITE_HOST:-mercmerc.net}}" \
    --arg commit "$(git -C "$ALPHA_ROOT" rev-parse HEAD 2>/dev/null || printf unknown)" \
    '{schema_version:1,kind:$kind,gate:$gate,status:$status,
      finished_at:$at,endpoint:$host,source_commit:$commit,
      policy:{stripe_live_mode:false,real_value:false,secret_values_recorded:false},
      note:"alpha progress ledger; not Level B authority by itself"}' > "$tmp"
  mv -f "$tmp" "$dest"
  chmod 600 "$dest"
  printf '%s\n' "$dest"
}

alpha_who_for() {
  local owner
  case "$1" in
    boot) printf 'power-authority lane (VENDOR_WALL_UPPER_BOUND)' ;;
    P1-STAGING) printf 'SUPERVISOR (ssh/deploy)' ;;
    P1-STRIPE-TEST) printf 'SUPERVISOR (Stripe TEST API)' ;;
    P1-OFFSITE-RESTORE) printf 'SUPERVISOR upload + SELF-CONTAINED restore' ;;
    P1-ALERT-DELIVERY) printf 'SUPERVISOR (external page)' ;;
    P1-CANARY-REHEARSAL) printf 'SUPERVISOR + 2 Metal devices' ;;
    P1-RECOVERY-SOAK) printf 'SUPERVISOR RUN-LAST' ;;
    P1-GOVERNANCE) printf 'operator (governance bundle)' ;;
    P1-INDEPENDENT-APPROVAL)
      if [ "$(alpha_ledger_gate_state "$1")" = dropped ]; then
        printf 'dropped (ops/go-no-go.json)'
      else
        owner="$(alpha_ledger_gate_owner "$1")"
        printf '%s' "${owner:-repository_owner}"
      fi
      ;;
    *) printf 'unknown' ;;
  esac
}

alpha_state_label() {
  local gate="$1" status prereq
  if [ "$(alpha_ledger_gate_state "$gate")" = dropped ]; then
    printf 'dropped'
    return
  fi
  status="$(alpha_receipt_status "$gate")"
  case "$status" in
    PASS) printf 'done' ; return ;;
    FAIL) printf 'failed' ; return ;;
    dropped) printf 'dropped' ; return ;;
  esac
  if [ "$gate" = P1-GOVERNANCE ]; then
    printf 'needs-supervisor'
    return
  fi
  if [ "$gate" = boot ]; then
    printf 'blocked'
    return
  fi
  if ! alpha_boot_is_green; then
    printf 'blocked (boot)'
    return
  fi
  # shellcheck disable=SC2086
  for prereq in $(alpha_prereqs_for "$gate"); do
    [ -n "$prereq" ] || continue
    [ "$prereq" = boot ] && continue
    if [ "$(alpha_receipt_status "$prereq")" != PASS ]; then
      printf 'blocked (%s)' "$prereq"
      return
    fi
  done
  if [ "$gate" = P1-RECOVERY-SOAK ]; then
    printf 'blocked (RUN-LAST; priors incomplete or not started)'
    return
  fi
  printf 'needs-supervisor'
}

alpha_staging_host() {
  printf '%s' "${STAGING_TLS_HOSTNAME:-${SITE_HOST:-mercmerc.net}}"
}

alpha_storage_host() {
  printf '%s' "${STAGING_STORAGE_TLS_HOSTNAME:-${STORAGE_HOST:-storage.mercmerc.net}}"
}

alpha_expected_commit() {
  if [ -n "${MERC_CANDIDATE_COMMIT:-}" ]; then
    printf '%s' "$MERC_CANDIDATE_COMMIT"
    return
  fi
  git -C "$ALPHA_ROOT" rev-parse HEAD
}
