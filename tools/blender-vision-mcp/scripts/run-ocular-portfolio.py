#!/usr/bin/env python3
"""Phase R — representation portfolio runner.

Usage:
  .venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.representation import run_portfolio_benchmark  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "portfolio",
    )
    args = parser.parse_args()

    receipt = run_portfolio_benchmark(args.output)
    print("======== OCULAR REPRESENTATION PORTFOLIO ========")
    for tid, row in receipt["targets"].items():
        print(
            f"{tid}: executed={row['executed_kinds']} blocked={row['blocked_kinds']} "
            f"radiance_blocked={row['radiance_blocked']}"
        )
        print(f"  purpose selections: {json.dumps(row['purpose_selections'])}")
        print(f"  radiance: {row['radiance_block_reason'][:100]}")
    print(f"receipt: {args.output / 'portfolio.receipt.json'}")
    print(f"STATUS: {receipt['status']}")
    return 0 if receipt["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
