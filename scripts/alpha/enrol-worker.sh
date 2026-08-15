#!/usr/bin/env bash
# Per-device enrollment helper for the two operator-controlled Metal workers.
# Device-bound credential (containment). Does not enroll unless --execute.
#
#   scripts/alpha/enrol-worker.sh --device studio|laptop --print-runbook
#   scripts/alpha/enrol-worker.sh --device studio --check
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
# shellcheck source=scripts/alpha/lib.sh
. "$ROOT/scripts/alpha/lib.sh"

usage() {
  cat >&2 <<'USAGE'
usage: scripts/alpha/enrol-worker.sh --device studio|laptop --print-runbook|--check
USAGE
  exit 2
}

device=""
mode=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --device) shift; device="${1:-}" ;;
    --print-runbook) mode=print ;;
    --check) mode=check ;;
    -h|--help) usage ;;
    *) usage ;;
  esac
  shift
done
case "$device" in studio|laptop) ;; *) usage ;; esac
[ -n "$mode" ] || mode=print

host="$(alpha_staging_host)"
profile="$ROOT/clients/macapp/ComputeExchangeAgent/merc-agent.sb"

case "$device" in
  studio)
    label="Mac Studio (this machine, 28/60 M3 Ultra)"
    home_hint="\$HOME/.merc-studio"
    ;;
  laptop)
    label="operator MacBook (headless merc-agent via screen-share)"
    home_hint="\$HOME/.merc-laptop"
    ;;
esac

if [ "$mode" = print ]; then
  cat <<EOF
# Enrol $device — $label
# Containment: device-bound P-256 key under ~/.merc/enrollment (never copied).
# Each device MUST use its own HOME/state dir so credentials cannot be shared.
# Approved worker UUID is assigned by enroll complete (real v4). Seed 0000-… IDs
# are refused by the canary receipt validator.

export MERC_SANDBOX_PROFILE=${profile:-/path/to/seatbelt.sb}
# Do not set MERC_ALLOW_UNSANDBOXED.

# 1. Device generates a request (private key never leaves the device):
merc-agent enroll request --control-origin https://$host

# 2. SUPERVISOR approves in the supplier console → cxeb2_… bundle.

# 3. Device completes the exchange:
merc-agent enroll complete --bundle cxeb2_…

# 4. Start contained:
merc-agent run --config \$HOME/.merc/agent.toml

# 5. Record the printed worker_id into MERC_CANARY_APPROVED_WORKER_IDS
#    (exactly two, comma-separated, distinct RFC UUIDs version nibble 1–5).
#    Record version + 16-hex build_hash into
#    MERC_CANARY_APPROVED_AGENT_VERSIONS / MERC_CANARY_APPROVED_BUILD_HASHES.

# Isolation: $device state dir $home_hint if both agents ever share one login.
# hw_class must be apple_silicon_*.
EOF
  exit 0
fi

alpha_load_env_optional
if ! alpha_boot_is_green; then
  alpha_die "boot is not green; enrollment against staging is refused"
fi
[ -n "${STAGING_TLS_HOSTNAME:-}" ] || alpha_die "STAGING_TLS_HOSTNAME is required to enrol"
alpha_log "CHECK ok: $device enrollment runbook is ready for https://$host (containment required)"
