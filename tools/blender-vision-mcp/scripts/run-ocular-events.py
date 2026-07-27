#!/usr/bin/env python3
"""Calibrate retina event detectors against the 9×4 fixture matrix.

Usage:
  .venv/bin/python scripts/run-ocular-events.py --output artifacts/ocular/events

Publishes per event type: precision, recall, FPR, latency p50/p95, and
confidence calibration (reliability across confidence bins).

Exit non-zero when:
  - any detector fires on its true-negative fixture
  - NEW_UNKNOWN_REGION has no true positives
  - declared minimum precision is missed
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import sys
import time
import traceback
from collections import defaultdict
from pathlib import Path
from typing import Any

import cv2
import numpy as np

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "src"))

from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    attest_blocked,
    run_attested,
)
from blender_vision.ocular.retina import RetinaState, process_frame  # noqa: E402

BLENDER_BIN = os.environ.get(
    "BVMCP_BLENDER",
    "/Applications/Blender.app/Contents/MacOS/Blender",
)

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

# Declared up front — fail below these. Not aspirational.
MIN_PRECISION: dict[str, float] = {
    "OBJECT_MOVED": 0.70,
    "CAMERA_MOVED": 0.80,
    "OBJECT_ENTERED": 0.70,
    "OBJECT_LEFT": 0.70,
    "OBJECT_OCCLUDED": 0.60,
    "OBJECT_REAPPEARED": 0.60,
    "NEW_UNKNOWN_REGION": 0.60,
    "LIGHT_CHANGED": 0.80,
    "SURFACE_CHANGED": 0.60,
}

CONF_BINS = [(0.0, 0.2), (0.2, 0.4), (0.4, 0.6), (0.6, 0.8), (0.8, 1.01)]


def _percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    arr = np.sort(np.asarray(values, dtype=np.float64))
    idx = int(round((p / 100.0) * (len(arr) - 1)))
    return float(arr[idx])


def _load_procedural():
    path = REPO / "benchmarks" / "ocular_events" / "procedural.py"
    spec = importlib.util.spec_from_file_location("ocular_events_procedural", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def render_blender_matrix(out_dir: Path) -> dict[str, Any]:
    script = REPO / "benchmarks" / "ocular_events" / "generate_blender.py"
    out_dir.mkdir(parents=True, exist_ok=True)
    if not Path(BLENDER_BIN).is_file():
        att = attest_blocked("blender", f"Blender binary not found at {BLENDER_BIN}")
        return {"attestation": att.to_dict(), "status": "blocked"}

    attestation = run_attested(
        "blender",
        [
            BLENDER_BIN,
            "--background",
            "--factory-startup",
            "--python-exit-code",
            "1",
            "--python",
            str(script),
            "--",
            str(out_dir),
        ],
        cwd=REPO,
        timeout_seconds=900,
        version_argv=["--version"],
        expect_marker="OCULAR_EVENTS_BLENDER_OK",
        outputs={"index": out_dir / "index.json"},
    )
    status = "ok" if attestation.execution_class is ExecutionClass.PHYSICAL else "blocked"
    result: dict[str, Any] = {
        "attestation": attestation.to_dict(),
        "status": status,
    }
    if status == "ok" and (out_dir / "index.json").is_file():
        result["index"] = json.loads((out_dir / "index.json").read_text(encoding="utf-8"))
    return result


def ensure_matrix(out: Path) -> dict[str, Any]:
    """Prefer EEVEE physical fixtures; fall back to procedural (DIAGNOSTIC)."""
    blender_dir = out / "blender"
    blender = render_blender_matrix(blender_dir)
    if blender.get("status") == "ok":
        return {
            "status": "blender",
            "root": blender_dir,
            "authority": "PHYSICAL",
            "attestation": blender.get("attestation"),
            "index": blender.get("index"),
        }

    print("BLOCKED_BLENDER: falling back to procedural 9×4 matrix")
    proc_dir = out / "procedural"
    mod = _load_procedural()
    manifests = mod.generate_all(proc_dir)
    return {
        "status": "procedural",
        "root": proc_dir,
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "attestation": blender.get("attestation"),
        "index": {
            "n_fixtures": len(manifests),
            "fixtures": [
                {
                    "event_type": m["event_type"],
                    "fixture_class": m["fixture_class"],
                    "path": m["root"],
                    "expect_fire": m["expect_fire"],
                }
                for m in manifests
            ],
        },
    }


def run_fixture(frames_dir: Path) -> dict[str, Any]:
    state = RetinaState()
    events: list[dict[str, Any]] = []
    latencies: list[float] = []
    n_frames = 0
    quantities: list[dict[str, Any]] = []
    for path in sorted(frames_dir.glob("frame_*.png")):
        image = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if image is None:
            continue
        t0 = time.perf_counter()
        analysis = process_frame(image, state=state)
        latencies.append((time.perf_counter() - t0) * 1000.0)
        for event in analysis.events:
            events.append(
                {
                    "event_type": event.event_type,
                    "confidence": event.confidence,
                    "region": event.region,
                    "evidence": event.evidence,
                    "frame_index": state.frame_index - 1,
                }
            )
            quantities.append(
                {
                    "event_type": event.event_type,
                    "confidence": event.confidence,
                    "evidence": event.evidence,
                }
            )
        n_frames += 1
    types = {e["event_type"] for e in events}
    return {
        "n_frames": n_frames,
        "events": events,
        "event_types": sorted(types),
        "latencies_ms": latencies,
        "quantities": quantities,
    }


def score_matrix(matrix: dict[str, Any]) -> dict[str, Any]:
    root = Path(matrix["root"])
    cells: list[dict[str, Any]] = []
    per_event: dict[str, dict[str, int]] = {
        e: {"tp": 0, "fp": 0, "fn": 0, "tn": 0} for e in CALIBRATED_EVENTS
    }
    conf_samples: dict[str, list[tuple[float, bool]]] = defaultdict(list)
    all_latencies: list[float] = []
    event_latencies: dict[str, list[float]] = defaultdict(list)

    for event in CALIBRATED_EVENTS:
        for cls in FIXTURE_CLASSES:
            cell_dir = root / event.lower() / cls
            man_path = cell_dir / "manifest.json"
            if not man_path.is_file():
                cells.append(
                    {
                        "event_type": event,
                        "fixture_class": cls,
                        "status": "missing",
                        "fired": False,
                    }
                )
                continue
            man = json.loads(man_path.read_text(encoding="utf-8"))
            run = run_fixture(cell_dir / "frames")
            all_latencies.extend(run["latencies_ms"])
            fired = event in run["event_types"]
            expect = bool(man.get("expect_fire"))
            # Primary measured quantity from first matching event, else None.
            measured = None
            conf = None
            for q in run["quantities"]:
                if q["event_type"] == event:
                    measured = q["evidence"]
                    conf = q["confidence"]
                    break

            # Classification for the event under test.
            if expect and fired:
                per_event[event]["tp"] += 1
                outcome = "tp"
            elif expect and not fired:
                per_event[event]["fn"] += 1
                outcome = "fn"
            elif (not expect) and fired:
                per_event[event]["fp"] += 1
                outcome = "fp"
            else:
                per_event[event]["tn"] += 1
                outcome = "tn"

            if conf is not None:
                conf_samples[event].append((float(conf), outcome in ("tp",)))

            # True-negative hard gate.
            tn_violation = cls == "true_negative" and fired
            # Confounder: firing the event under test is a false positive.
            conf_violation = cls == "confounder" and fired

            for lat in run["latencies_ms"]:
                event_latencies[event].append(lat)

            cells.append(
                {
                    "event_type": event,
                    "fixture_class": cls,
                    "expect_fire": expect,
                    "fired": fired,
                    "outcome": outcome,
                    "confidence": conf,
                    "measured": measured,
                    "measured_quantity": man.get("measured_quantity"),
                    "notes": man.get("notes", ""),
                    "event_types_seen": run["event_types"],
                    "n_frames": run["n_frames"],
                    "tn_violation": tn_violation,
                    "confounder_violation": conf_violation,
                    "latency_ms": {
                        "p50": _percentile(run["latencies_ms"], 50),
                        "p95": _percentile(run["latencies_ms"], 95),
                    },
                }
            )

    metrics: dict[str, Any] = {}
    for event in CALIBRATED_EVENTS:
        c = per_event[event]
        tp, fp, fn, tn = c["tp"], c["fp"], c["fn"], c["tn"]
        precision = tp / (tp + fp) if (tp + fp) else (1.0 if fn == 0 else 0.0)
        recall = tp / (tp + fn) if (tp + fn) else (1.0 if fp == 0 else 0.0)
        fpr = fp / (fp + tn) if (fp + tn) else 0.0
        f1 = (
            2 * precision * recall / (precision + recall)
            if (precision + recall) > 0
            else 0.0
        )
        lats = event_latencies[event]
        metrics[event] = {
            "tp": tp,
            "fp": fp,
            "fn": fn,
            "tn": tn,
            "precision": precision,
            "recall": recall,
            "fpr": fpr,
            "f1": f1,
            "min_precision": MIN_PRECISION[event],
            "latency_ms": {
                "p50": _percentile(lats, 50),
                "p95": _percentile(lats, 95),
                "n": len(lats),
            },
            "confidence_calibration": _calibrate(conf_samples[event]),
        }

    return {
        "cells": cells,
        "per_event": metrics,
        "latency_ms": {
            "p50": _percentile(all_latencies, 50),
            "p95": _percentile(all_latencies, 95),
            "n": len(all_latencies),
        },
    }


def _calibrate(samples: list[tuple[float, bool]]) -> dict[str, Any]:
    """Reliability diagram: accuracy per confidence bin + monotone check."""
    bins: list[dict[str, Any]] = []
    accs: list[float] = []
    for lo, hi in CONF_BINS:
        in_bin = [(c, ok) for c, ok in samples if lo <= c < hi]
        n = len(in_bin)
        if n == 0:
            bins.append({"lo": lo, "hi": hi, "n": 0, "accuracy": None, "mean_conf": None})
            continue
        acc = sum(1 for _, ok in in_bin if ok) / n
        mean_c = sum(c for c, _ in in_bin) / n
        bins.append({"lo": lo, "hi": hi, "n": n, "accuracy": acc, "mean_conf": mean_c})
        accs.append(acc)
    # Monotone: non-decreasing accuracy across non-empty bins.
    monotone = all(accs[i] <= accs[i + 1] + 1e-9 for i in range(len(accs) - 1))
    return {"bins": bins, "monotone": monotone, "n_samples": len(samples)}


def print_matrix(score: dict[str, Any]) -> None:
    print("\n=== 9 × 4 matrix (event × fixture class → fired / quantity) ===")
    header = f"{'event':22} " + " ".join(f"{c:>16}" for c in FIXTURE_CLASSES)
    print(header)
    print("-" * len(header))
    by_key = {(c["event_type"], c["fixture_class"]): c for c in score["cells"]}
    for event in CALIBRATED_EVENTS:
        cells = []
        for cls in FIXTURE_CLASSES:
            cell = by_key.get((event, cls), {})
            fired = cell.get("fired")
            mark = "FIRE" if fired else "----"
            exp = "Y" if cell.get("expect_fire") else "n"
            conf = cell.get("confidence")
            conf_s = f"{conf:.2f}" if isinstance(conf, float) else "  - "
            cells.append(f"{mark}/{exp} c={conf_s}")
        print(f"{event:22} " + " ".join(f"{s:>16}" for s in cells))
    print("legend: FIRE|---- / expect(Y|n) c=confidence")


def print_metrics(score: dict[str, Any]) -> None:
    print("\n=== Per-event precision / recall / FPR / F1 / latency ===")
    print(
        f"{'event':22} {'P':>5} {'R':>5} {'FPR':>5} {'F1':>5} "
        f"{'minP':>5} {'p50ms':>7} {'p95ms':>7}"
    )
    for event in CALIBRATED_EVENTS:
        m = score["per_event"][event]
        print(
            f"{event:22} {m['precision']:5.2f} {m['recall']:5.2f} "
            f"{m['fpr']:5.2f} {m['f1']:5.2f} {m['min_precision']:5.2f} "
            f"{m['latency_ms']['p50']:7.2f} {m['latency_ms']['p95']:7.2f}"
        )
    print("\n=== Confidence calibration (reliability bins) ===")
    for event in CALIBRATED_EVENTS:
        cal = score["per_event"][event]["confidence_calibration"]
        parts = []
        for b in cal["bins"]:
            if b["n"] == 0:
                continue
            parts.append(f"[{b['lo']:.1f}-{b['hi']:.1f}) n={b['n']} acc={b['accuracy']:.2f}")
        mono = "monotone" if cal["monotone"] else "NON-MONOTONE"
        print(f"{event:22} {mono:12} {'; '.join(parts) if parts else '(no fires)'}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=REPO / "artifacts" / "ocular" / "events",
    )
    parser.add_argument(
        "--procedural-only",
        action="store_true",
        help="Skip Blender; use procedural fixtures only (DIAGNOSTIC).",
    )
    args = parser.parse_args()
    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)

    print("MIN_PRECISION declared:")
    for event, floor in MIN_PRECISION.items():
        print(f"  {event:22} {floor:.2f}")
    print(f"output: {out}")

    if args.procedural_only:
        proc_dir = out / "procedural"
        mod = _load_procedural()
        manifests = mod.generate_all(proc_dir)
        matrix = {
            "status": "procedural",
            "root": proc_dir,
            "authority": "PROCEDURAL_GROUND_TRUTH",
            "attestation": None,
            "index": {"n_fixtures": len(manifests)},
        }
    else:
        matrix = ensure_matrix(out)

    print(f"\nfixture matrix: status={matrix['status']} authority={matrix['authority']}")
    score = score_matrix(matrix)
    print_matrix(score)
    print_metrics(score)

    receipt = {
        "min_precision": MIN_PRECISION,
        "matrix": {
            "status": matrix["status"],
            "authority": matrix["authority"],
            "root": str(matrix["root"]),
            "attestation": matrix.get("attestation"),
        },
        "cells": score["cells"],
        "per_event": score["per_event"],
        "latency_ms": score["latency_ms"],
    }
    receipt_path = out / "receipt.json"
    receipt_path.write_text(
        json.dumps(receipt, indent=2, sort_keys=True, default=str) + "\n",
        encoding="utf-8",
    )
    print(f"\nWrote {receipt_path}")

    failures: list[str] = []
    # True-negative hard gate.
    for cell in score["cells"]:
        if cell.get("tn_violation"):
            failures.append(
                f"{cell['event_type']}/{cell['fixture_class']}: fired on true_negative"
            )
    # NEW_UNKNOWN_REGION must have true positives.
    nur = score["per_event"]["NEW_UNKNOWN_REGION"]
    if nur["tp"] < 1:
        failures.append(
            f"NEW_UNKNOWN_REGION has no true positives (tp={nur['tp']} fn={nur['fn']})"
        )
    # Precision floors (only when the event was evaluated with positives).
    for event, m in score["per_event"].items():
        if m["tp"] + m["fp"] == 0 and m["fn"] > 0:
            failures.append(f"{event}: never fired (recall 0) — not a precision dodge")
            continue
        if m["tp"] + m["fp"] > 0 and m["precision"] < m["min_precision"]:
            failures.append(
                f"{event}: precision {m['precision']:.2f} < min {m['min_precision']:.2f} "
                f"(recall={m['recall']:.2f} f1={m['f1']:.2f})"
            )
        # Do not accept zero recall as a way to meet precision.
        if m["fn"] > 0 and m["tp"] == 0:
            failures.append(f"{event}: recall suppressed to zero (fn={m['fn']})")

    if failures:
        print("\nFAIL:")
        for item in failures:
            print(f"  - {item}")
        return 1

    print("\nPASS: precision floors met; no true-negative fires; NEW_UNKNOWN has TPs")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        traceback.print_exc()
        raise SystemExit(2) from None
