#!/usr/bin/env bash
# Force one required technical-exercise test to fail and assert the shared
# derivation path reports it with a non-zero exit. Binds to the same
# ops/scripts/derive-technical-exercises-receipt.py used by technical-exercises.sh.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/merc-tech-ex-fail.XXXXXX")"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

JSONL="$TMP/events.jsonl"
EVIDENCE="$TMP/technical-exercises.json"

# Synthetic go test -json stream: DSAR fails; the other six pass.
python3 - <<PY
import json
from pathlib import Path
required = [
    "TestDSARDeletionTombstoneAndRestoreReplay",
    "TestSupportAndSecurityTechnicalTabletops",
    "TestPrivilegedAdminMutationsHaveCompleteAtomicAudit",
    "TestPrivilegedMutationIdempotentConcurrentReplay",
    "TestConcurrentNamedOperatorsRetainIndependentAttribution",
    "TestRevocationWinsRaceBeforePrivilegedMutation",
    "TestAdminMutationRollsBackWhenAuditInsertFails",
]
lines = []
for name in required:
    action = "fail" if name == "TestDSARDeletionTombstoneAndRestoreReplay" else "pass"
    lines.append(json.dumps({"Action": action, "Test": name, "Package": "cx/control"}))
Path("$JSONL").write_text("\\n".join(lines) + "\\n")
PY

set +e
output="$(python3 "$ROOT/ops/scripts/derive-technical-exercises-receipt.py" "$JSONL" "$EVIDENCE" 1 2>&1)"
status=$?
set -e

if [ "$status" -eq 0 ]; then
  printf 'test-technical-exercises-fail-closed: FAIL: derivation accepted a failing test\n%s\n' "$output" >&2
  exit 1
fi

python3 - <<PY
import json
from pathlib import Path
receipt = json.loads(Path("$EVIDENCE").read_text())
assert receipt["status"] == "FAIL", receipt
assert receipt["test_results"]["TestDSARDeletionTombstoneAndRestoreReplay"] == "fail", receipt
assert receipt["dsar"]["export_complete"] is False, receipt
assert receipt["break_glass"]["immutable_audit"] is True, receipt
print("test-technical-exercises-fail-closed: PASS (forced failing test yields FAIL receipt + non-zero exit)")
PY
