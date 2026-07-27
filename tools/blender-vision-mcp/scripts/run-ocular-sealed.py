#!/usr/bin/env python3
"""Phase S — sealed ocular benchmarks with single split authority.

Usage:
  .venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.sealed import TARGET_IDS, run_sealed_ocular  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "sealed",
    )
    parser.add_argument("--seed", type=int, default=20260727)
    args = parser.parse_args()

    receipt = run_sealed_ocular(args.output, seed=args.seed)
    print("======== OCULAR SEALED BENCHMARKS ========")
    print(f"targets ({receipt['target_count']}): {', '.join(TARGET_IDS)}")
    print()
    print("LEAKAGE MATRIX")
    print("-" * 72)
    for row in receipt["leakage_matrix"]:
        print(f"{row['probe']:<48} {row['result']:<6} {row['detail'][:60]}")
    print("-" * 72)
    print(f"failures: {receipt['failures']}")
    print(f"receipt: {args.output / 'sealed.receipt.json'}")
    print(f"STATUS: {receipt['status']}")
    return 0 if receipt["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
