#!/usr/bin/env python3
"""Regenerate evidence/autonomous/payment-simulator.json from the Go simulator.

Owns only payment-simulator.json. Invoked by `make stripe-simulate`.
Stamps source_commit via ops/scripts/lib/receipt_binding.py as the last step.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.receipt_binding import candidate_commit, stamp  # noqa: E402

OUT = ROOT / "evidence" / "autonomous" / "payment-simulator.json"
CONTROL = ROOT / "src/control"
SIMULATOR = CONTROL / "stripe_simulator.go"
PRODUCER = "ops/scripts/produce-payment-simulator.py"


def main() -> int:
    if not SIMULATOR.is_file():
        print(f"{PRODUCER}: missing {SIMULATOR}", file=sys.stderr)
        return 2
    OUT.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.NamedTemporaryFile(
        prefix="merc-payment-sim.", suffix=".json", delete=False
    ) as handle:
        payload_path = Path(handle.name)
    try:
        run = subprocess.run(
            ["go", "run", ".", "release", "stripe-simulate", "--sequences", "4096"],
            cwd=CONTROL,
            check=False,
            capture_output=True,
            text=True,
        )
        if run.returncode != 0:
            sys.stderr.write(run.stderr)
            print(f"{PRODUCER}: go stripe-simulate exited {run.returncode}", file=sys.stderr)
            return run.returncode or 1
        payload_path.write_text(run.stdout, encoding="utf-8")
        writer = subprocess.run(
            [
                sys.executable,
                str(ROOT / "ops/scripts" / "write-bound-evidence.py"),
                "--out",
                str(OUT),
                "--harness",
                "src/control/stripe_simulator.go (release stripe-simulate)",
                "--payload-file",
                str(payload_path),
                "--build-binary",
                str(SIMULATOR),
                "--exact-config",
                "deterministic stripe simulator; sequences=4096",
                "--raw-samples",
                "embedded generated_sequences and scenario outcomes",
                "--model-na",
                "payment simulator does not load model weights",
                "--image-na",
                "no container image in this measurement",
                "--corpus-na",
                "no external corpus",
            ],
            check=False,
        )
        if writer.returncode != 0:
            return writer.returncode
        doc = json.loads(OUT.read_text(encoding="utf-8"))
        if not isinstance(doc, dict):
            print(f"{PRODUCER}: simulator payload was not a JSON object", file=sys.stderr)
            return 2
        stamp(doc, candidate_commit(str(ROOT)), PRODUCER)
        OUT.write_text(json.dumps(doc, indent=2, ensure_ascii=False) + "\n", encoding="utf-8")
        print(
            f"wrote {OUT} source_commit={doc['source_commit']} "
            f"binding_status={doc['binding_status']}"
        )
        return 0
    finally:
        payload_path.unlink(missing_ok=True)


if __name__ == "__main__":
    raise SystemExit(main())
