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
report="$(cd control && MERC_TEST_DATABASE_URL="${MERC_TEST_DATABASE_URL:-}" go test ./... -json -count=1 2>/dev/null \
  | python3 -c '
import json,sys
for line in sys.stdin:
    try:
        e = json.loads(line)
    except ValueError:
        continue
    if e.get("Action") == "skip" and e.get("Test"):
        print(e["Test"])
' | sort -u)"

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
