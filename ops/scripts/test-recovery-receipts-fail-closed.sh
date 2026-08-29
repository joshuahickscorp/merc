#!/usr/bin/env bash
# Forced-fail observation must make derive-recovery-receipts.py exit non-zero.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/merc-recovery-fail.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

python3 - <<PY
import json
from pathlib import Path
# Duplicate-stripe test fails; others pass without observations so those modes
# also fail closed (missing observation).
lines = [
    json.dumps({"Action": "fail", "Test": "TestRecoveryLaneDuplicateStripeEvent", "Package": "merc/control"}),
    json.dumps({"Action": "pass", "Test": "TestRecoveryLaneProcessRestart", "Package": "merc/control"}),
]
Path("$TMP/events.jsonl").write_text("\\n".join(lines) + "\\n")
PY

set +e
output="$(python3 "$ROOT/ops/scripts/derive-recovery-receipts.py" \
  "$TMP/events.jsonl" "$TMP/out" \
  "$TMP/missing-restore.json" "$TMP/missing-independent.json" \
  false 1 2>&1)"
status=$?
set -e

if [ "$status" -eq 0 ]; then
  printf 'test-recovery-receipts-fail-closed: FAIL: deriver accepted a failing suite\n%s\n' "$output" >&2
  exit 1
fi
printf 'test-recovery-receipts-fail-closed: PASS (forced failure exits %s)\n' "$status"
