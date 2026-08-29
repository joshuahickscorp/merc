#!/usr/bin/env bash
# Self-test for mutation campaign load precondition and suite-timeout derivation.
# Synthetic inputs only — does not start worktrees, Postgres, or go test.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
# shellcheck source=lib/mutation-load-preflight.sh
source "$ROOT/ops/scripts/lib/mutation-load-preflight.sh"

fail() {
  echo "test-mutation-load-preflight: $*" >&2
  exit 1
}

# Headroom: cpus - load1, clamped at 0.
headroom="$(mutation_cpu_headroom 28 10.5)"
[ "$headroom" = "17.50" ] || fail "expected headroom 17.50, got $headroom"
headroom="$(mutation_cpu_headroom 8 34.2)"
[ "$headroom" = "0.00" ] || fail "expected clamped headroom 0.00, got $headroom"

# Default threshold is workers + 2 (one core per worker plus coordinator slack).
[ "$(mutation_default_min_cpu_headroom 16)" = "18" ] || fail "default min headroom for 16 workers"
[ "$(mutation_default_min_cpu_headroom 1)" = "3" ] || fail "default min headroom for 1 worker"

# Refuse when headroom < threshold.
decision="$(mutation_load_precondition_decision 1.00 3 0)"
[ "$decision" = "refuse" ] || fail "expected refuse at headroom 1 < threshold 3, got $decision"
decision="$(mutation_load_precondition_decision 0.00 18 0)"
[ "$decision" = "refuse" ] || fail "expected refuse at headroom 0, got $decision"

# Proceed when headroom >= threshold.
decision="$(mutation_load_precondition_decision 5.00 3 0)"
[ "$decision" = "proceed" ] || fail "expected proceed at headroom 5 >= 3, got $decision"
decision="$(mutation_load_precondition_decision 18.00 18 0)"
[ "$decision" = "proceed" ] || fail "expected proceed at headroom == threshold, got $decision"

# Waiver proceeds even when headroom is below threshold.
decision="$(mutation_load_precondition_decision 0.00 18 1)"
[ "$decision" = "waive" ] || fail "expected waive under MERC_MUTATION_IGNORE_LOAD=1, got $decision"
decision="$(mutation_load_precondition_decision 1.00 3 1)"
[ "$decision" = "waive" ] || fail "expected waive at low headroom with ignore, got $decision"

# Suite timeout: max(120, ceil(3*B)). Integer B makes 3*B exact.
[ "$(mutation_derive_suite_timeout_seconds 76)" = "228" ] || fail "derive from B=76"
[ "$(mutation_derive_suite_timeout_seconds 30)" = "120" ] || fail "derive floor at B=30"
[ "$(mutation_derive_suite_timeout_seconds 40)" = "120" ] || fail "derive floor at B=40"
[ "$(mutation_derive_suite_timeout_seconds 41)" = "123" ] || fail "derive from B=41"
[ "$(mutation_derive_suite_timeout_seconds 0)" = "120" ] || fail "derive floor at B=0"

# Live readers return parseable values on this host (do not assert load magnitude).
cpus="$(mutation_read_cpu_count)"
[[ "$cpus" =~ ^[1-9][0-9]*$ ]] || fail "cpu count not a positive integer: $cpus"
load1="$(mutation_read_load1)"
[[ "$load1" =~ ^[0-9]+([.][0-9]+)?$ ]] || fail "load1 not numeric: $load1"
live_headroom="$(mutation_cpu_headroom "$cpus" "$load1")"
[[ "$live_headroom" =~ ^[0-9]+[.][0-9]{2}$ ]] || fail "live headroom shape: $live_headroom"

# Parallel orchestrator must source the helper, document the knobs, and no longer
# hard-code the historical 2m suite budget.
rg --fixed-strings 'source "$ROOT/ops/scripts/lib/mutation-load-preflight.sh"' \
  ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_SUITE_TIMEOUT' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_MIN_CPU_HEADROOM' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_IGNORE_LOAD' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'mutation_run_load_preflight' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'mutation_derive_suite_timeout_seconds' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'load_precondition_waived' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'measured_baseline_seconds' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'derived_suite_timeout_seconds' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'postmaster became multithreaded during startup' \
  ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'LC_ALL=C LANG=C pg_ctl' ops/scripts/mutation-test-parallel.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_SUITE_TIMEOUT' ops/scripts/mutation-test.sh >/dev/null
rg --fixed-strings 'MERC_MUTATION_SUITE_TIMEOUT_FLAG' ops/scripts/mutation-test.sh >/dev/null

if rg -n -F -- '-timeout=2m' ops/scripts/mutation-test-parallel.sh ops/scripts/mutation-test.sh; then
  fail "historical -timeout=2m literal still present; suite budget must be the measured variable"
fi

# A just-below-threshold refuse message must name the infrastructure class.
refuse_log="$(mktemp "${TMPDIR:-/tmp}/merc-mut-preflight-refuse.XXXXXX")"
if MERC_MUTATION_IGNORE_LOAD=0 MERC_MUTATION_MIN_CPU_HEADROOM=99 \
  mutation_run_load_preflight 1 "${TMPDIR:-/tmp}" >"$refuse_log" 2>&1; then
  rm -f "$refuse_log"
  fail "expected live refuse under min_cpu_headroom=99"
fi
rg --fixed-strings 'infrastructure precondition, not a mutation result' "$refuse_log" >/dev/null \
  || { cat "$refuse_log" >&2; rm -f "$refuse_log"; fail "refuse message must state infrastructure vs mutation"; }
rm -f "$refuse_log"

waive_log="$(mktemp "${TMPDIR:-/tmp}/merc-mut-preflight-waive.XXXXXX")"
if ! MERC_MUTATION_IGNORE_LOAD=1 MERC_MUTATION_MIN_CPU_HEADROOM=99 \
  mutation_run_load_preflight 1 "${TMPDIR:-/tmp}" >"$waive_log" 2>&1; then
  cat "$waive_log" >&2
  rm -f "$waive_log"
  fail "expected waive under IGNORE_LOAD=1"
fi
rg --fixed-strings 'decision=waive' "$waive_log" >/dev/null \
  || { cat "$waive_log" >&2; rm -f "$waive_log"; fail "waive path did not report decision=waive"; }
rm -f "$waive_log"
[ "$MUTATION_PREFLIGHT_WAIVED" = "1" ] || fail "MUTATION_PREFLIGHT_WAIVED not set on waive"

echo "test-mutation-load-preflight: PASS refuse/proceed/waive and suite-timeout derivation"
