#!/usr/bin/env python3
"""Phase T — full-runtime ocular repair corpus.

Usage:
  .venv/bin/python scripts/run-ocular-repair.py --output artifacts/ocular/repair
  .venv/bin/python scripts/run-ocular-repair.py --list
  .venv/bin/python scripts/run-ocular-repair.py --only sensor-wrong-intrinsics
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.repair import (  # noqa: E402
    repair_corpus_drill_ids,
    run_ocular_repair_corpus,
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "repair",
    )
    parser.add_argument("--only", action="append", default=[])
    parser.add_argument("--list", action="store_true")
    args = parser.parse_args()

    if args.list:
        for drill_id in repair_corpus_drill_ids():
            print(drill_id)
        return 0

    receipt = run_ocular_repair_corpus(
        args.output,
        only=args.only or None,
    )
    matrix_path = args.output.expanduser().resolve() / "repair.matrix.txt"
    if matrix_path.is_file():
        print(matrix_path.read_text(encoding="utf-8"))
    print(f"receipt: {args.output / 'repair.receipt.json'}")
    print(
        f"STATUS: {receipt.status} "
        f"(passed={receipt.passed_count} failed={receipt.failed_count} "
        f"blocked={receipt.blocked_count})"
    )
    return 0 if receipt.failed_count == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
