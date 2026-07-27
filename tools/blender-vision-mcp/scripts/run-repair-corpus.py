#!/usr/bin/env python3
"""Run the VisionMCP V2 full-runtime repair corpus and print the matrix.

Usage:
  scripts/run-repair-corpus.py --output artifacts/v2/repair
  scripts/run-repair-corpus.py --output /tmp/repair --only geometry-wrong-dimensions
  scripts/run-repair-corpus.py --output /tmp/repair --force-measure

Exit non-zero only when a runnable drill fails detection or acceptance.
BLOCKED_EXTERNAL drills (missing Blender/browser) do not fail the process —
the supervisor re-runs those on hardware that can start the runtime.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT / "src") not in sys.path:
    sys.path.insert(0, str(ROOT / "src"))

from blender_vision.benchmarks.repair_corpus import (  # noqa: E402
    format_matrix,
    repair_corpus_drill_ids,
    run_repair_corpus,
)
from blender_vision.core.util import atomic_write_json  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "repair",
        help="Directory for receipts, failed-attempts/, and per-drill workspaces",
    )
    parser.add_argument(
        "--only",
        action="append",
        default=[],
        help="Run only the named drill id (repeatable)",
    )
    parser.add_argument(
        "--force-measure",
        action="store_true",
        help=(
            "Prove inject/detect/repair on the live artifact+critic path even when "
            "Blender/browser are unavailable. Does not invent external runtime success."
        ),
    )
    parser.add_argument(
        "--list",
        action="store_true",
        help="List drill ids and exit",
    )
    args = parser.parse_args()

    if args.list:
        for drill_id in repair_corpus_drill_ids():
            print(drill_id)
        return 0

    only = args.only or None
    receipt = run_repair_corpus(
        args.output,
        only=only,
        force_measure_without_external=bool(args.force_measure),
    )

    matrix = format_matrix(receipt)
    print(matrix)
    matrix_path = args.output.expanduser().resolve() / "repair-corpus.matrix.txt"
    matrix_path.write_text(matrix + "\n", encoding="utf-8")
    atomic_write_json(
        args.output.expanduser().resolve() / "repair-corpus.summary.json",
        {
            "status": receipt.status,
            "drill_count": receipt.drill_count,
            "passed_count": receipt.passed_count,
            "failed_count": receipt.failed_count,
            "blocked_count": receipt.blocked_count,
            "drills": [
                {
                    "drill_id": d.drill_id,
                    "detector_fired": d.detector_fired,
                    "repaired": d.repaired,
                    "acceptance_passed": d.acceptance_passed,
                    "global_regression": d.global_regression,
                    "runtime_used": d.runtime_used,
                    "status": d.status,
                    "block_reason": d.block_reason,
                    "measured_injected": d.measured_injected,
                    "measured_repaired": d.measured_repaired,
                }
                for d in receipt.drills
            ],
        },
    )

    if receipt.failed_count > 0:
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
