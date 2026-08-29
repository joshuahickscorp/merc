#!/usr/bin/env bash
# Syntax + contract check for the alpha-gate scaffold. Does not ssh, deploy,
# call Stripe, or start the soak.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd -P)"
# shellcheck source=ops/scripts/alpha/lib.sh
. "$ROOT/ops/scripts/alpha/lib.sh"

die() { printf 'validate-alpha: %s\n' "$*" >&2; exit 1; }
pass() { printf 'validate-alpha: PASS %s\n' "$*"; }

alpha_require_command jq
[ -f "$ROOT/docs/archive/staging/ALPHA_GATE_PLAN.md" ] || die "missing archived alpha gate plan"
[ -f "$ROOT/ops/go-no-go.json" ] || die "missing ops/go-no-go.json"

scripts=(
  ops/scripts/alpha/lib.sh
  ops/scripts/alpha/status.sh
  ops/scripts/alpha/deploy.sh
  ops/scripts/alpha/probes.sh
  ops/scripts/alpha/stripe-test.sh
  ops/scripts/alpha/offsite-restore.sh
  ops/scripts/alpha/alert-sink.sh
  ops/scripts/alpha/enrol-worker.sh
  ops/scripts/alpha/canary-rehearsal.sh
  ops/scripts/alpha/scenarios.sh
  ops/scripts/alpha/soak.sh
  ops/scripts/alpha/validate-scaffold.sh
)
for script in "${scripts[@]}"; do
  [ -f "$ROOT/$script" ] || die "missing $script"
  bash -n "$ROOT/$script" || die "bash -n failed for $script"
done
pass "bash syntax"

# Plan quotes every currently open exit criterion. Satisfied historical gates
# belong in dropped_p1 and must not make the scaffold fail before it reaches
# the live decision ledger.
open_p1_ids="$(jq -er -r '.open_p1[]?.id // empty' "$ROOT/ops/go-no-go.json")" \
  || die "ops/go-no-go.json has no readable open_p1 list"
[ -n "$open_p1_ids" ] || die "ops/go-no-go.json has no open P1 gates"
while IFS= read -r id; do
  [ -n "$id" ] || continue
	grep -Fq "$id" "$ROOT/docs/archive/staging/ALPHA_GATE_PLAN.md" || die "plan omits $id"
	crit="$(jq -er --arg id "$id" '.open_p1[] | select(.id==$id) | .exit_criterion' "$ROOT/ops/go-no-go.json")"
	grep -Fq "$crit" "$ROOT/docs/archive/staging/ALPHA_GATE_PLAN.md" || die "plan does not quote $id exit_criterion"
done <<< "$open_p1_ids"
grep -Fq 'P1-INDEPENDENT-APPROVAL' "$ROOT/docs/archive/staging/ALPHA_GATE_PLAN.md" \
  || die "plan does not name P1-INDEPENDENT-APPROVAL"
grep -Fq 'alpha_ledger_gate_state' "$ROOT/ops/scripts/alpha/lib.sh" \
  || die "lib.sh must derive P1 state from ops/go-no-go.json"
grep -Fq 'ops/go-no-go.json' "$ROOT/ops/scripts/alpha/lib.sh" \
  || die "lib.sh must name the go-no-go ledger"
[ -f "$ROOT/docker-compose.canary.yml" ] \
  || die "missing docker-compose.canary.yml (launch review cites it)"
grep -Fq 'MERC_ENV: staging' "$ROOT/docker-compose.canary.yml" \
  || die "docker-compose.canary.yml must override MERC_ENV=staging"
pass "plan quotes in-scope exit criteria; P1 approval follows one ledger"

# Fail-closed contracts exist in the shared lib / soak driver.
for needle in alpha_reject_live_stripe alpha_require_boot alpha_require_prereqs \
  MERC_ALPHA_SOAK_I_AM_THE_SUPERVISOR VENDOR_WALL_UPPER_BOUND; do
  grep -Fq "$needle" "$ROOT/ops/scripts/alpha/lib.sh" "$ROOT/ops/scripts/alpha/soak.sh" \
    || die "scaffold missing fail-closed contract $needle"
done
grep -Fq 'sk_live_' "$ROOT/ops/scripts/alpha/lib.sh" || die "live Stripe prefix guard missing"
grep -Fq '86400' "$ROOT/ops/scripts/alpha/soak.sh" || die "soak missing 24h gate"
pass "fail-closed contracts"

# Two-device canary.
grep -Fq 'Mac Studio' "$ROOT/ops/scripts/alpha/canary-rehearsal.sh" \
  || die "canary rehearsal does not name the Mac Studio"
grep -Fq 'MacBook' "$ROOT/ops/scripts/alpha/canary-rehearsal.sh" \
  || die "canary rehearsal does not name the MacBook"
for adapter in kill_switch revocation intake_pause_result_retrieval no_payout_export; do
  grep -Fq "$adapter" "$ROOT/ops/scripts/alpha/scenarios.sh" \
    || die "missing extra adapter $adapter"
done
pass "two-device canary + extra adapters"

# Deploy runner must not invoke ssh/rsync. Runbook text may mention ssh
# as documentation; go-closure uses `ssh --` / `rsync -` as real calls.
if grep -nE 'ssh -- |rsync -[a-zA-Z]' "$ROOT/ops/scripts/alpha/deploy.sh"; then
  die "deploy.sh must not invoke ssh/rsync (runbook only)"
fi
pass "deploy runner does not ssh"

if command -v shellcheck >/dev/null 2>&1; then
  # shellcheck disable=SC2086
  shellcheck -x --severity=warning "${scripts[@]/#/$ROOT/}" || die "shellcheck failed"
  pass "shellcheck"
else
  printf 'validate-alpha: SKIP shellcheck (not installed)\n'
fi

pass "alpha-gate scaffold (this is not staging, Stripe, or soak evidence)"
