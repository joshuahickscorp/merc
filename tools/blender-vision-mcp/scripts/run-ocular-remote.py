#!/usr/bin/env python3
"""Phase K — real-remote perception through the ocular loop.

Usage:
  .venv/bin/python scripts/run-ocular-remote.py --output artifacts/ocular/remote
  .venv/bin/python scripts/run-ocular-remote.py --train-dir PATH --max-views 8

The governed self-captured fixture is procedural only — never the user's remote.
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.remote_loop import (  # noqa: E402
    capture_protocol_text,
    run_remote_loop,
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "ocular" / "remote",
    )
    parser.add_argument("--train-dir", type=Path, default=None)
    parser.add_argument("--max-views", type=int, default=None)
    args = parser.parse_args()
    output = args.output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)

    receipt = run_remote_loop(
        output,
        train_dir=args.train_dir,
        max_views=args.max_views,
    )

    (output / "USER_REMOTE_CAPTURE_PROTOCOL.md").write_text(
        capture_protocol_text(), encoding="utf-8"
    )

    print("======== OCULAR REMOTE LOOP ========")
    print(f"claim: {receipt.claim[:80]}...")
    print(f"execution_class: {receipt.execution_class}")
    print(f"train_images: {receipt.train_image_count}")
    print(f"views: {len(receipt.views)}")
    for view in receipt.views[:5]:
        print(
            f"  {view.view_id}: observed={len(view.observed)} "
            f"inferred={len(view.inferred)} next={len(view.next_view)} "
            f"segments={view.segment_count}"
        )
    if len(receipt.views) > 5:
        print(f"  ... {len(receipt.views) - 5} more views")
    print(f"world entities: {receipt.world_summary.get('entity_count')}")
    print(
        "geometry candidates: "
        + ", ".join(
            c.get("backend", "?")
            for c in receipt.geometry_portfolio.get("candidates", [])
        )
    )
    print(f"blockers: {len(receipt.blockers)}")
    for b in receipt.blockers:
        print(f"  BLOCKED {b.get('id')}: {str(b.get('reason', ''))[:100]}")
    print(f"receipt: {output / 'remote_loop.receipt.json'}")

    if not receipt.views:
        print("FAIL: no views processed", file=sys.stderr)
        return 1
    # Require every view to answer observed/inferred/next-view.
    for view in receipt.views:
        if "observed" not in view.to_dict() or "inferred" not in view.to_dict():
            print(f"FAIL: view {view.view_id} missing fields", file=sys.stderr)
            return 1
        if not view.next_view:
            print(f"FAIL: view {view.view_id} has empty next_view", file=sys.stderr)
            return 1
    print("STATUS: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
