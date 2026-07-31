#!/usr/bin/env bash
# A skipped test is an assertion nobody made.
#
# The control-plane suite skipped its dispute, payout, admin-authority and
# realtime settlement tests whenever MERC_TEST_DATABASE_URL was unset, so `make
# ci` reported ok in under a second while executing no ledger SQL at all.  This
# gate makes that visible: any skip in a CI run is a failure unless it is listed
# in the allowlist below with a reason, which a reviewer has to read.
set -euo pipefail
cd "$(dirname "$0")/.."

ALLOWLIST="scripts/allowed-test-skips.txt"

# Reads a `go test -json` log rather than running the suite.
#
# It used to run the whole suite a second time. That cost about fourteen extra
# minutes on a host with object storage and a local engine, and -- worse -- the
# second run inherited the first run's rows in the shared database, so it failed
# on tests that had just passed. `make ci` now records the suite once and hands
# the log to this gate.
#
# Running it directly with no argument still works, and still runs the suite,
# because a developer asking "which tests are skipping here" should not have to
# know about the log. Point MERC_TEST_DATABASE_URL at a fresh database when you
# do: a run against one the suite has already populated is not the same check.
LOG="${1:-}"
if [ -z "$LOG" ]; then
  # The suite needs the same budget `make ci` gives it. Without -timeout this
  # used the 10 minute default while the suite takes about fourteen, and under
  # `set -e` with `pipefail` the command substitution then failed with NO OUTPUT
  # AT ALL -- `make ci` printed nothing but "Error 1". A gate that fails silently
  # is indistinguishable from a gate that is broken.
  TEST_TIMEOUT="${MERC_TEST_TIMEOUT:-45m}"
  LOG="$(mktemp)"
  trap 'rm -f "$LOG"' EXIT
  if ! (cd control && MERC_TEST_DATABASE_URL="${MERC_TEST_DATABASE_URL:-}" \
          go test ./... -json -count=1 -timeout "$TEST_TIMEOUT" >"$LOG" 2>/dev/null); then
    echo "test skips: the suite did not pass, so its skip set is not meaningful" >&2
    echo "  (re-run \`make ci\` and fix the failures; this gate has nothing to add)" >&2
    exit 1
  fi
fi

report="$(python3 -c '
import json,sys
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("Action") == "skip" and e.get("Test"):
        print(e["Test"])
' < "$LOG" | sort -u)"

if [ -z "$report" ]; then
  echo "test skips: none"
  exit 0
fi

unexpected=""
while IFS= read -r name; do
  [ -z "$name" ] && continue
  if [ -f "$ALLOWLIST" ] && grep -qxF "$name" "$ALLOWLIST"; then
    continue
  fi
  unexpected="${unexpected}${name}"$'\n'
done <<< "$report"

if [ -n "$unexpected" ]; then
  echo "test skips: FAIL — these tests skipped and are not in $ALLOWLIST:" >&2
  printf '  %s\n' $unexpected >&2
  echo "Add a line to $ALLOWLIST with a reason, or make the test run." >&2
  exit 1
fi
echo "test skips: all allowlisted"
