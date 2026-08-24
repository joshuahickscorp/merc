#!/usr/bin/env bash
# Produce evidence/autonomous/alert-delivery-r1.json without editing the shared
# harness scripts/test-alert-delivery.sh (that script's default output is
# evidence/autonomous/alert-delivery.json, which this lane does not own).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
OUT="$ROOT/evidence/autonomous/alert-delivery-r1.json"
export MERC_ALERT_DELIVERY_EVIDENCE="$OUT"
bash "$ROOT/scripts/test-alert-delivery.sh"

python3 - "$ROOT" "$OUT" <<'PY'
import json
import sys
from pathlib import Path

root = Path(sys.argv[1])
out = Path(sys.argv[2])
sys.path.insert(0, str(root / "scripts"))
from lib.receipt_binding import candidate_commit, stamp

doc = json.loads(out.read_text(encoding="utf-8"))
stamp(doc, candidate_commit(str(root)), "scripts/produce-alert-delivery-r1.sh")
out.write_text(json.dumps(doc, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
print(
    f"stamped {out} source_commit={doc['source_commit']} "
    f"binding_status={doc['binding_status']}"
)
PY
