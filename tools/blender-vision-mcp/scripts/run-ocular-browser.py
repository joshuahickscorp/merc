#!/usr/bin/env python3
"""Phase N — browser and screen eyeball with contradiction detectors.

Usage:
  .venv/bin/python scripts/run-ocular-browser.py --output artifacts/ocular/browser
  scripts/with-one-browser.sh .venv/bin/python scripts/run-ocular-browser.py \\
      --output artifacts/ocular/browser --physical --url file://$PWD/tests/fixtures/web/static/index.html

Always runs the synthetic detector demonstration. Physical capture is optional
and must be serialized via with-one-browser.sh.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.browser_eyeball import run_browser_eyeball  # noqa: E402


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "browser",
    )
    parser.add_argument("--snapshot", type=Path, default=None)
    parser.add_argument("--physical", action="store_true")
    parser.add_argument("--url", type=str, default=None)
    args = parser.parse_args()

    receipt = run_browser_eyeball(
        args.output,
        url=args.url,
        snapshot_path=args.snapshot,
        physical=args.physical,
    )

    print("======== OCULAR BROWSER EYEBALL ========")
    print(f"demo contradictions: {receipt['demo']['contradiction_count']}")
    print(f"kinds fired: {receipt['demo']['kinds_fired']}")
    print(f"all kinds fired: {receipt['demo']['all_kinds_fired']}")
    print(f"existing tools: {receipt['existing_browser_tools']}")
    print(f"serialization: {receipt['serialization']}")
    if receipt.get("physical"):
        print(f"physical: {receipt['physical']}")
    print(f"receipt: {args.output / 'browser_eyeball.receipt.json'}")
    print(f"STATUS: {receipt['status']}")
    return 0 if receipt["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
