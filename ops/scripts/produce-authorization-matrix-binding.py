#!/usr/bin/env python3
"""Bind ops/authorization-matrix.json to the commit it was validated against.

The matrix is a hand-maintained declaration, not a measurement, so it has no
natural producer. What makes it trustworthy is that it agrees with the routes
actually registered in src/control/api.go — and that agreement is only true of a
particular commit. ops/scripts/validate-authorization-matrix.py proves it; this
records which commit it was proven at.

The stamp is written ONLY when the validator exits zero. A matrix that does not
match the code it claims to describe must not carry a commit binding, because
that binding is the whole claim.
"""

from __future__ import annotations

import json
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.receipt_binding import candidate_commit, stamp  # noqa: E402

MATRIX = ROOT / "ops" / "authorization-matrix.json"
VALIDATOR = ROOT / "ops/scripts" / "validate-authorization-matrix.py"
PRODUCER = "ops/scripts/produce-authorization-matrix-binding.py"


def main() -> int:
    run = subprocess.run(
        [sys.executable, str(VALIDATOR)], cwd=ROOT, capture_output=True, text=True
    )
    sys.stdout.write(run.stdout)
    sys.stderr.write(run.stderr)
    if run.returncode != 0:
        print(f"{PRODUCER}: validator failed; refusing to stamp", file=sys.stderr)
        return run.returncode or 1

    doc = json.loads(MATRIX.read_text(encoding="utf-8"))
    commit = candidate_commit(str(ROOT))
    stamp(doc, commit, PRODUCER)
    doc["validated_by"] = "ops/scripts/validate-authorization-matrix.py"
    doc["validator_output"] = run.stdout.strip().splitlines()[-1] if run.stdout.strip() else ""
    MATRIX.write_text(json.dumps(doc, indent=2) + "\n", encoding="utf-8")
    print(f"stamped {MATRIX} source_commit={commit} binding_status={doc['binding_status']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
