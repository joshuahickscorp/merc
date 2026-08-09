#!/usr/bin/env bash
# Behavioural proof that the oracle whole-suite strategy is sound.
#
# Decisive case: a no-op mutation on a deliberately red tree must abort on the
# clean baseline and must never report "caught". Companion cases prove a green
# baseline still scores a real mutant as caught, and that timeout/setup faults
# are infrastructure rather than catches.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

python3 scripts/test-mutation-suite-observer.py

# ---------------------------------------------------------------------------
# Structural: rename, aliases, safe bare default, deep tier uses oracle.
# ---------------------------------------------------------------------------
default_line="$(rg -n 'MERC_MUTATION_TEST_STRATEGY="\$\{MERC_MUTATION_TEST_STRATEGY:-' scripts/mutation-test.sh | head -n1)"
printf '%s\n' "$default_line" | rg --fixed-strings ':-adaptive}' >/dev/null || {
  echo "bare default is not adaptive: $default_line" >&2
  exit 1
}
rg --fixed-strings 'MERC_MUTATION_TEST_STRATEGY=oracle' scripts/mutation-test.sh >/dev/null
rg --fixed-strings 'full|whole-suite)' scripts/mutation-test.sh >/dev/null
rg --fixed-strings 'strategy=oracle' scripts/mutation-gate.sh >/dev/null
# The gate tier name "full" must remain a tier, not be confused with the strategy.
rg --fixed-strings 'full       all 104 mutations exactly once' scripts/mutation-gate.sh >/dev/null
# Deep is the only tier that selects the oracle strategy.
python3 - <<'PY'
from pathlib import Path
text = Path("scripts/mutation-gate.sh").read_text(encoding="utf-8")
# strategy defaults to adaptive, then deep alone assigns oracle.
if 'strategy="adaptive"' not in text:
    raise SystemExit("gate no longer defaults strategy to adaptive")
if "strategy=oracle" not in text:
    raise SystemExit("deep tier does not select oracle strategy")
# Ensure no remaining assignment of strategy=full (the unsound name collision).
for line in text.splitlines():
    stripped = line.strip()
    if stripped.startswith("strategy=full") or stripped.startswith('strategy="full"'):
        raise SystemExit(f"gate still assigns strategy=full: {line}")
print("gate strategy rename: PASS")
PY

# Alias acceptance without running a campaign.
alias_help="$(
  MERC_MUTATION_LIST=1 MERC_MUTATION_TEST_STRATEGY=full bash scripts/mutation-test.sh 2>&1 | head -n1 || true
)"
# Listing must succeed under the historical alias.
list_count="$(MERC_MUTATION_LIST=1 MERC_MUTATION_TEST_STRATEGY=full bash scripts/mutation-test.sh | wc -l | tr -d ' ')"
[ "$list_count" = "104" ] || {
  echo "full alias did not list 104 mutations (got $list_count)" >&2
  exit 1
}
list_count_oracle="$(MERC_MUTATION_LIST=1 MERC_MUTATION_TEST_STRATEGY=oracle bash scripts/mutation-test.sh | wc -l | tr -d ' ')"
[ "$list_count_oracle" = "104" ] || {
  echo "oracle strategy did not list 104 mutations (got $list_count_oracle)" >&2
  exit 1
}
list_count_ws="$(MERC_MUTATION_LIST=1 MERC_MUTATION_TEST_STRATEGY=whole-suite bash scripts/mutation-test.sh | wc -l | tr -d ' ')"
[ "$list_count_ws" = "104" ] || {
  echo "whole-suite alias did not list 104 mutations (got $list_count_ws)" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# Sandbox harness: tiny control package + one mutation case path.
# We do not run the real 104-case campaign. We exercise the oracle preflight and
# scoring path with the smallest package that still goes through mutation-test.sh.
# ---------------------------------------------------------------------------
PROOF="$(mktemp -d "${TMPDIR:-/tmp}/merc-oracle-proof.XXXXXX")"
cleanup_proof() {
  rm -rf "$PROOF"
}
trap cleanup_proof EXIT

mkdir -p "$PROOF/scripts" "$PROOF/control"
# Scripts the oracle path needs.
cp scripts/mutation-test.sh \
  scripts/mutation-suite-observer.py \
  "$PROOF/scripts/"
# Contracts/adaptive helpers are imported only on those strategies; still copy
# the minimal set so a mistaken path fails closed rather than with a missing file.
cp scripts/mutation-contract-observer.py \
  scripts/mutation-test-contracts.py \
  scripts/mutation-test-contracts.json \
  scripts/mutation-preflight-cache.py \
  scripts/with-isolated-test-db.sh \
  "$PROOF/scripts/" 2>/dev/null || true

# Strip the production mutation table down to a single controllable case so the
# harness stays small and deterministic. Keep the case shape file|desc|sed.
python3 - "$PROOF/scripts/mutation-test.sh" <<'PY'
from pathlib import Path
import sys
path = Path(sys.argv[1])
text = path.read_text(encoding="utf-8")
start = text.index("MUTATIONS=(")
end = text.index("\n)", start)  # original closing paren of the array
# One real behavioural mutant (changes return value) used for the green catch.
# A second no-op pattern is not needed: the red-baseline case aborts before sed.
# Keep the original "\n)" at end so we do not double-close the array.
replacement = '''MUTATIONS=(
"money_nanos.go|sandbox oracle multiplies instead of adding|s#return a + b#return a * b#"'''
path.write_text(text[:start] + replacement + text[end:], encoding="utf-8")
PY

# Mini package: green by default; a red test file is added only for the red proof.
cat >"$PROOF/control/go.mod" <<'EOF'
module merc/control

go 1.22
EOF

cat >"$PROOF/control/money_nanos.go" <<'EOF'
package control

func Combine(a, b int) int {
	return a + b
}
EOF

cat >"$PROOF/control/money_nanos_test.go" <<'EOF'
package control

import "testing"

func TestCombineAdds(t *testing.T) {
	if got := Combine(2, 3); got != 5 {
		t.Fatalf("Combine(2,3)=%d want 5", got)
	}
}
EOF

# ---------------------------------------------------------------------------
# Proof 1 — no-op mutation on a deliberately red tree aborts on baseline.
# ---------------------------------------------------------------------------
cat >"$PROOF/control/unrelated_red_test.go" <<'EOF'
package control

import "testing"

func TestUnrelatedDeliberateRed(t *testing.T) {
	t.Fatal("deliberately red baseline; unrelated to money_nanos.go")
}
EOF

set +e
red_out="$(
  cd "$PROOF" &&
    MERC_MUTATION_UNIT_ONLY=1 \
    MERC_MUTATION_FILTER="sandbox oracle" \
    MERC_MUTATION_TEST_STRATEGY=oracle \
    MERC_TEST_DATABASE_URL='postgres://unused' \
    bash scripts/mutation-test.sh 2>&1
)"
red_status=$?
set -e

printf '%s\n' "$red_out"
echo "--- red-baseline exit=$red_status ---"

if [ "$red_status" -eq 0 ]; then
  echo "oracle strategy accepted a red baseline (must refuse)" >&2
  exit 1
fi
printf '%s\n' "$red_out" | rg -q 'refusing to score mutants without a green oracle baseline|clean whole-suite oracle baseline' || {
  echo "red baseline did not abort with the oracle baseline refusal message" >&2
  exit 1
}
# Must not claim a catch for the mutant.
if printf '%s\n' "$red_out" | rg -q 'sandbox oracle multiplies instead of adding[[:space:]]+caught'; then
  echo "no-op/any mutant on a red tree was scored caught" >&2
  exit 1
fi
if printf '%s\n' "$red_out" | rg -q '^mutation-test: [1-9][0-9]* caught'; then
  echo "red baseline campaign reported a non-zero caught count" >&2
  exit 1
fi
echo "proof-red-baseline: PASS (aborted; not scored caught)"

# Remove the unrelated red so the remaining proofs see a green baseline.
rm -f "$PROOF/control/unrelated_red_test.go"

# ---------------------------------------------------------------------------
# Proof 2 — green baseline still scores a real mutant as caught.
# ---------------------------------------------------------------------------
set +e
green_out="$(
  cd "$PROOF" &&
    MERC_MUTATION_UNIT_ONLY=1 \
    MERC_MUTATION_FILTER="sandbox oracle" \
    MERC_MUTATION_TEST_STRATEGY=oracle \
    MERC_TEST_DATABASE_URL='postgres://unused' \
    bash scripts/mutation-test.sh 2>&1
)"
green_status=$?
set -e

printf '%s\n' "$green_out"
echo "--- green-catch exit=$green_status ---"

printf '%s\n' "$green_out" | rg -q 'sandbox oracle multiplies instead of adding[[:space:]]+caught' || {
  echo "green baseline did not score the real mutant as caught" >&2
  exit 1
}
printf '%s\n' "$green_out" | rg -q 'mutation-test: 1 caught, 0 survived, 0 stale, 0 infrastructure' || {
  echo "green catch summary is wrong: $green_out" >&2
  exit 1
}
[ "$green_status" -eq 0 ] || {
  echo "green catch campaign should exit 0 (all caught)" >&2
  exit 1
}
echo "proof-green-catch: PASS"

# Tree restored after mutant.
if ! grep -q 'return a + b' "$PROOF/control/money_nanos.go"; then
  echo "oracle runner did not restore money_nanos.go after the mutant" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Proof 3 — simulated timeout/setup is infrastructure, fails the campaign.
# Force the mutant run to emit a timeout marker via a PATH wrapper around go.
# Baseline stays green by delegating to the real go binary.
# ---------------------------------------------------------------------------
FAKE_BIN="$PROOF/fake-bin"
mkdir -p "$FAKE_BIN"
REAL_GO="$(command -v go)"
cat >"$FAKE_BIN/go" <<EOF
#!/usr/bin/env bash
set -euo pipefail
# Baseline logs are named mutation-oracle-baseline-*; mutant logs mutation-oracle-mutant-*.
# The suite observer classifies from the log the runner captures. We detect mutant
# runs by a marker file flipped after baseline completes.
if [ -f "$PROOF/.oracle_mutant_phase" ]; then
  if [[ "\$*" == *test* ]]; then
    # Emit JSON-ish timeout text the suite observer recognizes.
    echo '{"Action":"output","Output":"panic: test timed out after 2m0s"}'
    echo '{"Action":"fail","Test":"TestCombineAdds"}'
    exit 1
  fi
fi
if [[ "\$*" == *build* ]]; then
  exec "$REAL_GO" "\$@"
fi
# First suite invocation is baseline: run for real, then arm mutant phase.
if [[ "\$*" == *test* ]]; then
  "$REAL_GO" "\$@"
  status=\$?
  if [ "\$status" -eq 0 ]; then
    : >"$PROOF/.oracle_mutant_phase"
  fi
  exit "\$status"
fi
exec "$REAL_GO" "\$@"
EOF
chmod +x "$FAKE_BIN/go"

set +e
infra_out="$(
  cd "$PROOF" &&
    PATH="$FAKE_BIN:$PATH" \
    MERC_MUTATION_UNIT_ONLY=1 \
    MERC_MUTATION_FILTER="sandbox oracle" \
    MERC_MUTATION_TEST_STRATEGY=oracle \
    MERC_TEST_DATABASE_URL='postgres://unused' \
    bash scripts/mutation-test.sh 2>&1
)"
infra_status=$?
set -e

printf '%s\n' "$infra_out"
echo "--- infrastructure-timeout exit=$infra_status ---"

printf '%s\n' "$infra_out" | rg -q 'INFRASTRUCTURE|infrastructure' || {
  echo "timeout was not classified as infrastructure" >&2
  exit 1
}
if printf '%s\n' "$infra_out" | rg -q 'sandbox oracle multiplies instead of adding[[:space:]]+caught'; then
  echo "timeout was mis-scored as caught" >&2
  exit 1
fi
printf '%s\n' "$infra_out" | rg -q 'mutation-test: 0 caught, 0 survived, 0 stale, 1 infrastructure' || {
  echo "infrastructure summary is wrong" >&2
  exit 1
}
[ "$infra_status" -ne 0 ] || {
  echo "infrastructure > 0 must fail the campaign" >&2
  exit 1
}
echo "proof-infrastructure-timeout: PASS"

# Alias still reaches the same sound path (smoke: red baseline under full alias).
cat >"$PROOF/control/unrelated_red_test.go" <<'EOF'
package control

import "testing"

func TestUnrelatedDeliberateRed(t *testing.T) {
	t.Fatal("deliberately red baseline under full alias")
}
EOF
rm -f "$PROOF/.oracle_mutant_phase"
set +e
alias_red_out="$(
  cd "$PROOF" &&
    MERC_MUTATION_UNIT_ONLY=1 \
    MERC_MUTATION_FILTER="sandbox oracle" \
    MERC_MUTATION_TEST_STRATEGY=full \
    MERC_TEST_DATABASE_URL='postgres://unused' \
    bash scripts/mutation-test.sh 2>&1
)"
alias_red_status=$?
set -e
[ "$alias_red_status" -ne 0 ] || {
  echo "full alias did not refuse a red baseline" >&2
  exit 1
}
printf '%s\n' "$alias_red_out" | rg -q 'refusing to score mutants without a green oracle baseline|clean whole-suite oracle baseline' || {
  echo "full alias red baseline message missing" >&2
  exit 1
}
echo "proof-full-alias-red-baseline: PASS"

echo "test-mutation-oracle-strategy: PASS all oracle soundness proofs"
