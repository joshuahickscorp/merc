#!/usr/bin/env python3
"""Run VisionMCP V2 object benchmarks: Phase O (consumer remote) and Phase P
(soft / organic / fur).

Prints, per target: backend-by-backend chamfer vs ground truth, dimensional
error in millimetres, unseen-view image metrics, hidden-surface ledger counts,
and the scorecard.

Exit code:
  0 — framework completed (poor reconstruction scores are reported results)
  1 — framework failure (unhandled crash in the harness itself)

Dense COLMAP MVS is reported blocked when COLMAP has no CUDA. Blender Metal
crashes are reported with the exact probe reason; software paths are labelled.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.benchmarks.objects import (  # noqa: E402
    run_object_benchmarks,
)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=ROOT / "artifacts" / "v2" / "object-benchmarks",
    )
    parser.add_argument("--train-views", type=int, default=24)
    parser.add_argument("--holdout-views", type=int, default=8)
    parser.add_argument("--seed", type=int, default=20260726)
    parser.add_argument("--skip-phase-o", action="store_true")
    parser.add_argument("--skip-phase-p", action="store_true")
    args = parser.parse_args()

    receipt = run_object_benchmarks(
        args.output,
        train_views=args.train_views,
        holdout_views=args.holdout_views,
        seed=args.seed,
        skip_phase_o=args.skip_phase_o,
        skip_phase_p=args.skip_phase_p,
    )

    print("\n======== OBJECT BENCHMARKS SUMMARY ========")
    print(f"output: {receipt.get('output')}")
    print(f"phases: {json.dumps(receipt.get('phases'), indent=2)}")
    print(f"dense_mvs_blocker: {receipt.get('dense_mvs_blocker')}")
    blender = receipt.get("blender") or {}
    print(
        f"blender: available={blender.get('available')} "
        f"reason={(blender.get('reason') or '')[:160]}"
    )
    for target_id, payload in (receipt.get("targets") or {}).items():
        print(f"\n-- {target_id} --")
        if payload.get("synthetic_claim"):
            print(f"  SYNTHETIC: {payload['synthetic_claim'][:100]}")
        scores = payload.get("backend_scores") or []
        for b in scores:
            ch = b.get("chamfer_m")
            ch_s = f"{ch:.6f}" if isinstance(ch, (int, float)) else "None"
            print(
                f"  chamfer[{b.get('backend')}]: {ch_s} m  "
                f"executed={b.get('executed')}"
            )
        for d in payload.get("dimensional_errors_mm") or []:
            print(
                f"  dim[{d.get('axis')}]: error={d.get('error_mm'):+.2f} mm "
                f"(truth={d.get('truth_mm'):.2f})"
            )
        metrics = payload.get("unseen_view_metrics") or []
        if metrics:
            psnr = sum(m["psnr_db"] for m in metrics) / len(metrics)
            ssim = sum(m["ssim"] for m in metrics) / len(metrics)
            print(f"  unseen-view: n={len(metrics)} mean PSNR={psnr:.2f} dB SSIM={ssim:.4f}")
        counts = payload.get("hidden_surface_counts") or {}
        print(f"  hidden-surface ledger: {counts}")
        print(f"  stages: {payload.get('stages')}")

    if receipt.get("framework_errors"):
        print("\nFRAMEWORK ERRORS:")
        print(json.dumps(receipt["framework_errors"], indent=2))
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
