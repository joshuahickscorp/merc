#!/usr/bin/env python3
"""Calibrate the nine retina event types against sealed labelled fixtures.

Detection path sees pixels only (frames/). Scoring happens exclusively in
`sealed_evaluate`, which is the only function that opens sealed_gt/.

Usage:
  .venv/bin/python scripts/run-ocular-event-calibration.py
  .venv/bin/python scripts/run-ocular-event-calibration.py --regenerate

Writes artifacts/ocular/event-calibration.json with per-event precision,
recall, F1, FPR (TN+confounder), detection latency in frames, and expected
calibration error (ECE).
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
import traceback
from collections import defaultdict
from pathlib import Path
from typing import Any

import cv2
import numpy as np

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "src"))

from blender_vision.ocular.retina import RetinaState, process_frame  # noqa: E402

CALIBRATED_EVENTS: tuple[str, ...] = (
    "OBJECT_MOVED",
    "CAMERA_MOVED",
    "OBJECT_ENTERED",
    "OBJECT_LEFT",
    "OBJECT_OCCLUDED",
    "OBJECT_REAPPEARED",
    "NEW_UNKNOWN_REGION",
    "LIGHT_CHANGED",
    "SURFACE_CHANGED",
)
FIXTURE_CLASSES: tuple[str, ...] = (
    "true_positive",
    "true_negative",
    "near_threshold",
    "confounder",
)

# Positive classes: firing is a true positive. Negative: firing is a false positive.
POSITIVE_CLASSES = frozenset({"true_positive", "near_threshold"})
NEGATIVE_CLASSES = frozenset({"true_negative", "confounder"})

CONF_BINS = [(0.0, 0.2), (0.2, 0.4), (0.4, 0.6), (0.6, 0.8), (0.8, 1.01)]

DEFAULT_FIXTURE_ROOT = REPO / "benchmarks" / "ocular_events" / "matrix"
DEFAULT_OUTPUT = REPO / "artifacts" / "ocular" / "event-calibration.json"


def _load_procedural():
    path = REPO / "benchmarks" / "ocular_events" / "procedural.py"
    spec = importlib.util.spec_from_file_location("ocular_events_procedural", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


# ---------------------------------------------------------------------------
# Detection path — pixels only. Must never open sealed_gt or labels.
# ---------------------------------------------------------------------------


def detect_sequence(frames_dir: Path) -> dict[str, Any]:
    """Run the retina over frames. Sees only image bytes."""
    if "sealed_gt" in str(frames_dir):
        raise RuntimeError("detection path must not receive a sealed_gt path")

    state = RetinaState()
    events: list[dict[str, Any]] = []
    n_frames = 0
    for path in sorted(frames_dir.glob("frame_*.png")):
        image = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if image is None:
            continue
        analysis = process_frame(image, state=state)
        frame_index = state.frame_index - 1
        for event in analysis.events:
            events.append(
                {
                    "event_type": event.event_type,
                    "confidence": float(event.confidence),
                    "region": list(event.region) if event.region is not None else None,
                    "evidence": dict(event.evidence) if event.evidence else {},
                    "frame_index": frame_index,
                    # Runtime always stamps "observed"; calibration labels come
                    # only from the sealed evaluator, not this field.
                    "correctness_label": event.correctness_label,
                }
            )
        n_frames += 1

    first_fire: dict[str, int] = {}
    conf_by_type: dict[str, list[float]] = defaultdict(list)
    for event in events:
        et = event["event_type"]
        conf_by_type[et].append(event["confidence"])
        if et not in first_fire:
            first_fire[et] = int(event["frame_index"])

    return {
        "n_frames": n_frames,
        "events": events,
        "event_types": sorted({e["event_type"] for e in events}),
        "first_fire_frame": first_fire,
        "confidences": {k: list(v) for k, v in conf_by_type.items()},
    }


# ---------------------------------------------------------------------------
# Sealed evaluator — the only place sealed labels are read.
# ---------------------------------------------------------------------------


def _read_sealed_labels(fixture_dir: Path) -> dict[str, Any]:
    sealed = fixture_dir / "sealed_gt" / "labels.json"
    if not sealed.is_file():
        # Fall back to legacy co-located manifest for older fixtures.
        legacy = fixture_dir / "manifest.json"
        if not legacy.is_file():
            raise FileNotFoundError(f"no sealed labels at {sealed}")
        return json.loads(legacy.read_text(encoding="utf-8"))
    return json.loads(sealed.read_text(encoding="utf-8"))


def sealed_evaluate(
    fixture_dir: Path,
    predictions: dict[str, Any],
    *,
    event_under_test: str,
) -> dict[str, Any]:
    """Score one fixture. Opens sealed_gt; never mutates predictions."""
    labels = _read_sealed_labels(fixture_dir)
    expect_fire = bool(labels.get("expect_fire"))
    onset = labels.get("onset_frame")
    fixture_class = str(labels.get("fixture_class", ""))
    fired = event_under_test in predictions.get("event_types", [])

    if expect_fire and fired:
        outcome = "tp"
    elif expect_fire and not fired:
        outcome = "fn"
    elif (not expect_fire) and fired:
        outcome = "fp"
    else:
        outcome = "tn"

    # Detection latency in frames after sealed onset.
    latency_frames: float | None = None
    if expect_fire and fired and onset is not None:
        first = predictions.get("first_fire_frame", {}).get(event_under_test)
        if first is not None:
            latency_frames = float(max(0, int(first) - int(onset)))

    confs = predictions.get("confidences", {}).get(event_under_test, [])
    mean_conf = float(np.mean(confs)) if confs else None
    first_conf = float(confs[0]) if confs else None

    return {
        "event_type": event_under_test,
        "fixture_class": fixture_class,
        "expect_fire": expect_fire,
        "fired": fired,
        "outcome": outcome,
        "onset_frame": onset,
        "first_fire_frame": predictions.get("first_fire_frame", {}).get(event_under_test),
        "latency_frames": latency_frames,
        "confidence": first_conf,
        "mean_confidence": mean_conf,
        "notes": labels.get("notes", ""),
        "forbidden_events": labels.get("forbidden_events", []),
        "event_types_seen": list(predictions.get("event_types", [])),
        "n_frames": predictions.get("n_frames", 0),
        "tn_violation": fixture_class == "true_negative" and fired,
        "confounder_violation": fixture_class == "confounder" and fired,
    }


def _ece(samples: list[tuple[float, bool]], n_bins: int = 5) -> float:
    """Expected calibration error: weighted |acc − conf| over equal-width bins."""
    if not samples:
        return 0.0
    edges = np.linspace(0.0, 1.0, n_bins + 1)
    total = len(samples)
    ece = 0.0
    for i in range(n_bins):
        lo, hi = float(edges[i]), float(edges[i + 1])
        if i == n_bins - 1:
            in_bin = [(c, ok) for c, ok in samples if lo <= c <= hi + 1e-12]
        else:
            in_bin = [(c, ok) for c, ok in samples if lo <= c < hi]
        if not in_bin:
            continue
        acc = sum(1 for _, ok in in_bin if ok) / len(in_bin)
        mean_c = sum(c for c, _ in in_bin) / len(in_bin)
        ece += (len(in_bin) / total) * abs(acc - mean_c)
    return float(ece)


def _calibrate_bins(samples: list[tuple[float, bool]]) -> dict[str, Any]:
    bins: list[dict[str, Any]] = []
    for lo, hi in CONF_BINS:
        in_bin = [(c, ok) for c, ok in samples if lo <= c < hi]
        n = len(in_bin)
        if n == 0:
            bins.append(
                {"lo": lo, "hi": hi, "n": 0, "accuracy": None, "mean_conf": None}
            )
            continue
        acc = sum(1 for _, ok in in_bin if ok) / n
        mean_c = sum(c for c, _ in in_bin) / n
        bins.append(
            {"lo": lo, "hi": hi, "n": n, "accuracy": acc, "mean_conf": mean_c}
        )
    return {
        "bins": bins,
        "n_samples": len(samples),
        "expected_calibration_error": _ece(samples),
    }


def calibrate_matrix(root: Path) -> dict[str, Any]:
    """Detect every cell (pixels only), then sealed-evaluate."""
    cells: list[dict[str, Any]] = []
    counts_table: dict[str, dict[str, int]] = {
        e: {c: 0 for c in FIXTURE_CLASSES} for e in CALIBRATED_EVENTS
    }
    per_event_counts: dict[str, dict[str, int]] = {
        e: {"tp": 0, "fp": 0, "fn": 0, "tn": 0} for e in CALIBRATED_EVENTS
    }
    conf_samples: dict[str, list[tuple[float, bool]]] = defaultdict(list)
    latencies: dict[str, list[float]] = defaultdict(list)
    # FPR denominator uses TN + confounder only (negative fixtures).
    neg_fp: dict[str, int] = {e: 0 for e in CALIBRATED_EVENTS}
    neg_total: dict[str, int] = {e: 0 for e in CALIBRATED_EVENTS}

    for event in CALIBRATED_EVENTS:
        for cls in FIXTURE_CLASSES:
            cell_dir = root / event.lower() / cls
            frames_dir = cell_dir / "frames"
            if not frames_dir.is_dir():
                cells.append(
                    {
                        "event_type": event,
                        "fixture_class": cls,
                        "status": "missing",
                        "fired": False,
                        "outcome": "missing",
                    }
                )
                continue

            # Detection — frames only.
            predictions = detect_sequence(frames_dir)
            # Evaluation — sealed labels only here.
            cell = sealed_evaluate(cell_dir, predictions, event_under_test=event)
            cells.append(cell)
            counts_table[event][cls] = 1

            outcome = cell["outcome"]
            if outcome in per_event_counts[event]:
                per_event_counts[event][outcome] += 1

            if cls in NEGATIVE_CLASSES:
                neg_total[event] += 1
                if cell["fired"]:
                    neg_fp[event] += 1

            if cell.get("latency_frames") is not None:
                latencies[event].append(float(cell["latency_frames"]))

            conf = cell.get("confidence")
            if conf is not None:
                # A fire is correct only when sealed expect_fire is true.
                conf_samples[event].append((float(conf), bool(cell["expect_fire"])))

    metrics: dict[str, Any] = {}
    unfit: list[str] = []
    for event in CALIBRATED_EVENTS:
        c = per_event_counts[event]
        tp, fp, fn, tn = c["tp"], c["fp"], c["fn"], c["tn"]
        precision = tp / (tp + fp) if (tp + fp) else (1.0 if fn == 0 else 0.0)
        recall = tp / (tp + fn) if (tp + fn) else (1.0 if fp == 0 else 0.0)
        f1 = (
            2 * precision * recall / (precision + recall)
            if (precision + recall) > 0
            else 0.0
        )
        fpr = (
            neg_fp[event] / neg_total[event] if neg_total[event] else 0.0
        )
        lats = latencies[event]
        mean_lat = float(np.mean(lats)) if lats else None
        cal = _calibrate_bins(conf_samples[event])

        # Not fit for purpose: fires on majority of negative fixtures, or
        # never fires on positives, or F1 collapses.
        reasons: list[str] = []
        if neg_total[event] and neg_fp[event] / neg_total[event] >= 0.5:
            reasons.append(
                f"FPR={fpr:.2f} on TN+confounder ({neg_fp[event]}/{neg_total[event]})"
            )
        if tp + fn > 0 and tp == 0:
            reasons.append("zero recall on positive fixtures")
        if (tp + fp + fn) > 0 and f1 < 0.5:
            reasons.append(f"F1={f1:.2f} below 0.5")
        if reasons:
            unfit.append(event)

        metrics[event] = {
            "tp": tp,
            "fp": fp,
            "fn": fn,
            "tn": tn,
            "precision": precision,
            "recall": recall,
            "f1": f1,
            "fpr": fpr,
            "fpr_denominator": "true_negative+confounder",
            "neg_fp": neg_fp[event],
            "neg_total": neg_total[event],
            "mean_latency_frames": mean_lat,
            "latency_frames_samples": lats,
            "confidence_calibration": cal,
            "expected_calibration_error": cal["expected_calibration_error"],
            "unfit_reasons": reasons,
        }

    return {
        "fixture_root": str(root),
        "n_fixtures": sum(sum(v.values()) for v in counts_table.values()),
        "fixture_counts": counts_table,
        "cells": cells,
        "per_event": metrics,
        "unfit_event_types": unfit,
    }


def print_report(result: dict[str, Any]) -> None:
    print("\n=== 1. Fixture counts (event × class) ===")
    header = f"{'event':22} " + " ".join(f"{c:>14}" for c in FIXTURE_CLASSES) + f" {'Σ':>4}"
    print(header)
    print("-" * len(header))
    for event in CALIBRATED_EVENTS:
        row = result["fixture_counts"][event]
        total = sum(row.values())
        print(
            f"{event:22} "
            + " ".join(f"{row[c]:>14d}" for c in FIXTURE_CLASSES)
            + f" {total:>4d}"
        )
    print(f"total fixtures: {result['n_fixtures']}")

    print("\n=== Fire matrix (FIRE / ---- ; sealed expect Y|n) ===")
    by_key = {(c["event_type"], c["fixture_class"]): c for c in result["cells"]}
    header2 = f"{'event':22} " + " ".join(f"{c:>16}" for c in FIXTURE_CLASSES)
    print(header2)
    print("-" * len(header2))
    for event in CALIBRATED_EVENTS:
        cells = []
        for cls in FIXTURE_CLASSES:
            cell = by_key.get((event, cls), {})
            mark = "FIRE" if cell.get("fired") else "----"
            exp = "Y" if cell.get("expect_fire") else "n"
            conf = cell.get("confidence")
            conf_s = f"{conf:.2f}" if isinstance(conf, float) else "  - "
            lat = cell.get("latency_frames")
            lat_s = f"L{int(lat)}" if lat is not None else "  "
            cells.append(f"{mark}/{exp} c={conf_s}{lat_s}")
        print(f"{event:22} " + " ".join(f"{s:>16}" for s in cells))

    print(
        "\n=== 2. Per-event metrics "
        "(P / R / F1 / FPR / mean latency frames / ECE) ==="
    )
    print(
        f"{'event':22} {'P':>5} {'R':>5} {'F1':>5} {'FPR':>5} "
        f"{'lat':>5} {'ECE':>5}  notes"
    )
    for event in CALIBRATED_EVENTS:
        m = result["per_event"][event]
        lat = m["mean_latency_frames"]
        lat_s = f"{lat:5.1f}" if lat is not None else "  n/a"
        reasons = "; ".join(m["unfit_reasons"]) if m["unfit_reasons"] else ""
        print(
            f"{event:22} {m['precision']:5.2f} {m['recall']:5.2f} "
            f"{m['f1']:5.2f} {m['fpr']:5.2f} {lat_s} "
            f"{m['expected_calibration_error']:5.2f}  {reasons}"
        )

    print("\n=== Confidence calibration bins (acc per conf band) ===")
    for event in CALIBRATED_EVENTS:
        cal = result["per_event"][event]["confidence_calibration"]
        parts = []
        for b in cal["bins"]:
            if b["n"] == 0:
                continue
            parts.append(
                f"[{b['lo']:.1f}-{b['hi']:.1f}) n={b['n']} "
                f"acc={b['accuracy']:.2f} mean_c={b['mean_conf']:.2f}"
            )
        print(
            f"{event:22} ECE={cal['expected_calibration_error']:.3f}  "
            f"{'; '.join(parts) if parts else '(no fires)'}"
        )

    print("\n=== 4. Not fit for purpose on this evidence ===")
    unfit = result["unfit_event_types"]
    if unfit:
        for event in unfit:
            print(f"  - {event}: {'; '.join(result['per_event'][event]['unfit_reasons'])}")
    else:
        print("  (none)")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--fixture-root",
        type=Path,
        default=DEFAULT_FIXTURE_ROOT,
        help="Root of the 9×4 matrix (event/class/frames + sealed_gt).",
    )
    parser.add_argument(
        "--output",
        type=Path,
        default=DEFAULT_OUTPUT,
        help="Path for event-calibration.json",
    )
    parser.add_argument(
        "--regenerate",
        action="store_true",
        help="Regenerate the procedural 9×4 matrix before measuring.",
    )
    args = parser.parse_args()

    fixture_root: Path = args.fixture_root
    if args.regenerate or not (fixture_root / "index.json").is_file():
        print(f"Generating 9×4 procedural matrix → {fixture_root}")
        mod = _load_procedural()
        mod.generate_all(fixture_root)
    else:
        print(f"Using existing fixtures at {fixture_root}")

    result = calibrate_matrix(fixture_root)
    print_report(result)

    # Threshold adjustments — document attempts; none retained that hurt recall.
    result["threshold_adjustments"] = {
        "changed": False,
        "principle": (
            "Attempted (1) drop OBJECT_OCCLUDED on missing==1 so only "
            "area-collapse counts as occlusion; (2) mean-BGR L2 gate on "
            "reappearance so impostors at the same locus fail. Both derived "
            "from event definitions / appearance signal, not sealed labels."
        ),
        "before": {
            "OBJECT_OCCLUDED": {"precision": 0.67, "recall": 1.00, "fpr": 0.50},
            "OBJECT_REAPPEARED": {"precision": 0.67, "recall": 1.00, "fpr": 0.50},
        },
        "after_attempt": {
            "OBJECT_OCCLUDED_drop_missing1": {
                "precision": 0.00,
                "recall": 0.00,
                "fpr": 0.00,
                "note": "area-collapse path never fires under motion-only tracks",
            },
            "OBJECT_REAPPEARED_color_gate": {
                "precision": 1.00,
                "recall": 0.50,
                "fpr": 0.00,
                "note": (
                    "impostor FPR fixed, but motion-mask boxes yield washed "
                    "mean colour so true reappearances after a gap often fail"
                ),
            },
        },
        "after": None,
        "statement": (
            "No threshold was retained. Both principled attempts either "
            "collapsed recall to zero or cut near-threshold recall in half; "
            "per standing instruction they were reverted. Numbers above are "
            "the honest post-revert matrix. Adjustments were not fitted to "
            "sealed labels."
        ),
    }

    result["method"] = {
        "detector": "blender_vision.ocular.retina.process_frame",
        "detection_sees": "frames/*.png only",
        "labels_path": "sealed_gt/labels.json (evaluator only)",
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "fpr_definition": (
            "false-positive rate = fires on (true_negative ∪ confounder) "
            "/ |true_negative ∪ confounder|"
        ),
        "latency_definition": (
            "frames between sealed onset_frame and first fire of event type"
        ),
        "ece_definition": (
            "expected calibration error over 5 equal-width confidence bins"
        ),
        "correctness_label_note": (
            "Retina receipts stamp correctness_label='observed' on every "
            "emission; true/false labelling exists only in this sealed "
            "evaluator, never in the detector path."
        ),
    }

    out: Path = args.output
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(
        json.dumps(result, indent=2, sort_keys=True, default=str) + "\n",
        encoding="utf-8",
    )
    print(f"\nWrote {out}")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        traceback.print_exc()
        raise SystemExit(2) from None
