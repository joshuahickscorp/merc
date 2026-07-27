#!/usr/bin/env python3
"""Physical execution of the ocular stream bus and retina pipeline.

Usage:
  .venv/bin/python scripts/run-ocular-retina.py --output artifacts/ocular/retina

Exit non-zero when:
  - OBJECT_ENTERED / OBJECT_LEFT / OBJECT_OCCLUDED recall is below threshold, or
  - camera-motion sequence reports OBJECT_MOVED (mis-separation).
"""

from __future__ import annotations

import argparse
import json
import os
import resource
import sys
import time
import traceback
from collections import Counter
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
from blender_vision.ocular.sensors import SourceType  # noqa: E402
from blender_vision.ocular.stream import (  # noqa: E402
    close_stream,
    open_stream,
    read_frame,
)

BLENDER_BIN = os.environ.get(
    "BVMCP_BLENDER",
    "/Applications/Blender.app/Contents/MacOS/Blender",
)

# Declared up front: event recall floor for the three hard lifecycle events.
RECALL_THRESHOLD = 0.50
HARD_EVENTS = ("OBJECT_ENTERED", "OBJECT_LEFT", "OBJECT_OCCLUDED")


def _percentile(values: list[float], p: float) -> float:
    if not values:
        return 0.0
    arr = np.sort(np.asarray(values, dtype=np.float64))
    idx = int(round((p / 100.0) * (len(arr) - 1)))
    return float(arr[idx])


def _peak_rss_mb() -> float:
    usage = resource.getrusage(resource.RUSAGE_SELF).ru_maxrss
    # macOS reports bytes; Linux reports kilobytes.
    if sys.platform == "darwin":
        return usage / (1024.0 * 1024.0)
    return usage / 1024.0


def render_blender_sequence(out_dir: Path, mode: str) -> dict[str, Any]:
    script = REPO / "benchmarks" / "ocular_retina" / "generate_fixture.py"
    out_dir.mkdir(parents=True, exist_ok=True)
    if not Path(BLENDER_BIN).is_file():
        att = attest_blocked("blender", f"Blender binary not found at {BLENDER_BIN}")
        return {"attestation": att.to_dict(), "status": "blocked", "mode": mode}

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
            mode,
        ],
        cwd=REPO,
        timeout_seconds=420,
        version_argv=["--version"],
        expect_marker="OCULAR_FIXTURE_OK",
        outputs={"manifest": out_dir / "manifest.json"},
    )
    status = "ok" if attestation.execution_class is ExecutionClass.PHYSICAL else "blocked"
    result: dict[str, Any] = {
        "attestation": attestation.to_dict(),
        "status": status,
        "mode": mode,
    }
    if status == "ok" and (out_dir / "manifest.json").is_file():
        result["manifest"] = json.loads(
            (out_dir / "manifest.json").read_text(encoding="utf-8")
        )
    return result


def procedural_fallback(out_dir: Path, mode: str, blender_meta: dict[str, Any]) -> dict[str, Any]:
    # Load by path so we do not require benchmarks to be an installed package.
    import importlib.util

    fixture_path = REPO / "benchmarks" / "ocular_retina" / "procedural_fixture.py"
    spec = importlib.util.spec_from_file_location("ocular_procedural_fixture", fixture_path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load procedural fixture module from {fixture_path}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    generate_camera_sequence = module.generate_camera_sequence
    generate_object_sequence = module.generate_object_sequence

    if mode == "camera_motion":
        manifest = generate_camera_sequence(out_dir)
    else:
        manifest = generate_object_sequence(out_dir)
    manifest["blender_status"] = blender_meta
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return {
        "status": "procedural",
        "mode": mode,
        "manifest": manifest,
        "attestation": blender_meta.get("attestation"),
    }


def ensure_sequence(out_dir: Path, mode: str) -> dict[str, Any]:
    blender = render_blender_sequence(out_dir / "blender", mode)
    if blender.get("status") == "ok":
        # Copy frames into out_dir for uniform path layout.
        src = out_dir / "blender"
        manifest = blender["manifest"]
        # Prefer blender frames directory.
        return {
            "status": "blender",
            "mode": mode,
            "manifest": manifest,
            "frames_dir": src / "frames",
            "attestation": blender["attestation"],
            "root": src,
        }
    print(f"BLOCKED_BLENDER mode={mode}: falling back to procedural fixture")
    proc = procedural_fallback(out_dir / "procedural", mode, blender)
    return {
        "status": "procedural",
        "mode": mode,
        "manifest": proc["manifest"],
        "frames_dir": out_dir / "procedural" / "frames",
        "attestation": blender.get("attestation"),
        "root": out_dir / "procedural",
    }


def run_retina_on_sequence(
    frames_dir: Path,
    *,
    stream_id: str,
    source_type: SourceType = SourceType.IMAGE_SEQUENCE,
) -> dict[str, Any]:
    handle = open_stream(
        frames_dir,
        source_type=source_type,
        stream_id=stream_id,
        frame_rate=24.0,
    )
    if isinstance(handle, type(attest_blocked("x", "y"))):
        # RuntimeAttestation returned.
        return {"error": handle.to_dict()}

    from blender_vision.ocular.attestation import RuntimeAttestation

    if isinstance(handle, RuntimeAttestation):
        return {"error": handle.to_dict()}

    state = RetinaState()
    reflex: list[float] = []
    attentive: list[float] = []
    events: list[dict[str, Any]] = []
    n_frames = 0
    while True:
        item = read_frame(handle)
        if item is None:
            break
        frame, image = item
        analysis = process_frame(image, frame=frame, state=state)
        reflex.append(analysis.reflex_latency_ms)
        attentive.append(analysis.attentive_latency_ms)
        for event in analysis.events:
            events.append(
                {
                    "event_type": event.event_type,
                    "frame_id": event.frame_id,
                    "timestamp": event.timestamp,
                    "confidence": event.confidence,
                    "region": event.region,
                    "correctness_label": event.correctness_label,
                    "written_by_lane": event.written_by_lane,
                }
            )
        n_frames += 1
    closed = close_stream(handle)
    return {
        "n_frames": n_frames,
        "events": events,
        "event_counts": dict(Counter(e["event_type"] for e in events)),
        "reflex_latency_ms": {
            "p50": _percentile(reflex, 50),
            "p95": _percentile(reflex, 95),
            "n": len(reflex),
        },
        "attentive_latency_ms": {
            "p50": _percentile(attentive, 50),
            "p95": _percentile(attentive, 95),
            "n": len(attentive),
        },
        "dropped_frames": closed["stats"]["frames_dropped"],
        "stream_state": closed,
    }


def score_events(
    predicted: list[dict[str, Any]],
    ground_truth: list[dict[str, Any]],
) -> dict[str, Any]:
    """Per-type precision/recall against sequence-level expected event sets.

    A predicted type counts as a true positive if the ground truth ever expects
    that type. Confusion is reported as counts, not a single score.
    """
    expected_types: Counter[str] = Counter()
    for row in ground_truth:
        for et in row.get("expected_events", []):
            expected_types[et] += 1

    predicted_types: Counter[str] = Counter(e["event_type"] for e in predicted)
    all_types = sorted(set(expected_types) | set(predicted_types))

    per_type: dict[str, Any] = {}
    for et in all_types:
        exp = expected_types.get(et, 0)
        pred = predicted_types.get(et, 0)
        # Sequence-level presence matching: TP if both sides non-zero for hard
        # events that appear once; for multi-count use min.
        tp = min(exp, pred)
        fp = max(0, pred - exp)
        fn = max(0, exp - pred)
        precision = tp / pred if pred else (1.0 if exp == 0 else 0.0)
        recall = tp / exp if exp else (1.0 if pred == 0 else 0.0)
        per_type[et] = {
            "expected": exp,
            "predicted": pred,
            "tp": tp,
            "fp": fp,
            "fn": fn,
            "precision": precision,
            "recall": recall,
        }
    return {
        "per_type": per_type,
        "expected_totals": dict(expected_types),
        "predicted_totals": dict(predicted_types),
    }


def attempt_webcam(out_dir: Path, min_frames: int = 30) -> dict[str, Any]:
    handle = open_stream(
        0,
        source_type=SourceType.WEBCAM,
        allow_webcam=True,
        webcam_index=0,
        stream_id="webcam-live",
        buffer_size=64,
    )
    from blender_vision.ocular.attestation import RuntimeAttestation

    if isinstance(handle, RuntimeAttestation):
        path = out_dir / "webcam_attestation.json"
        path.write_text(json.dumps(handle.to_dict(), indent=2) + "\n", encoding="utf-8")
        print(f"WEBCAM BLOCKED: {handle.blocked_reason}")
        return {"status": "blocked", "attestation": handle.to_dict()}

    frames_dir = out_dir / "webcam_frames"
    frames_dir.mkdir(parents=True, exist_ok=True)
    captured = 0
    t0 = time.monotonic()
    while captured < min_frames:
        item = read_frame(handle)
        if item is None:
            break
        _frame, image = item
        cv2.imwrite(str(frames_dir / f"webcam_{captured:04d}.png"), image)
        captured += 1
    elapsed = time.monotonic() - t0
    closed = close_stream(handle)

    if captured >= min_frames:
        # Physical attestation: real frames left the device.
        from blender_vision.core.util import utc_now
        from blender_vision.ocular.attestation import RuntimeAttestation as RA
        from blender_vision.ocular.records import default_lineage
        from blender_vision.v2.authority import AuthorityClass

        att = RA(
            id=f"attest-webcam-physical-{int(time.time() * 1000)}",
            runtime="webcam",
            execution_class=ExecutionClass.PHYSICAL,
            executable="cv2.VideoCapture",
            command=["cv2.VideoCapture", "0"],
            started_at=utc_now(),
            ended_at=utc_now(),
            elapsed_seconds=elapsed,
            returncode=0,
            host={"platform": sys.platform, "frames": captured},
            authority=AuthorityClass.RUNTIME_OBSERVED,
            lineage=default_lineage("ocular.webcam.capture"),
        ).seal()
        (out_dir / "webcam_attestation.json").write_text(
            json.dumps(att.to_dict(), indent=2) + "\n", encoding="utf-8"
        )
        print(f"WEBCAM PHYSICAL: captured {captured} frames in {elapsed:.2f}s")
        return {
            "status": "physical",
            "frames": captured,
            "elapsed_seconds": elapsed,
            "attestation": att.to_dict(),
            "stream_state": closed,
        }

    att = attest_blocked(
        "webcam",
        f"webcam opened but only produced {captured} frames (need >= {min_frames})",
    )
    (out_dir / "webcam_attestation.json").write_text(
        json.dumps(att.to_dict(), indent=2) + "\n", encoding="utf-8"
    )
    print(f"WEBCAM BLOCKED: {att.blocked_reason}")
    return {"status": "blocked", "attestation": att.to_dict(), "frames": captured}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=REPO / "artifacts" / "ocular" / "retina",
    )
    args = parser.parse_args()
    out: Path = args.output
    out.mkdir(parents=True, exist_ok=True)

    print(f"RECALL_THRESHOLD for {HARD_EVENTS}: {RECALL_THRESHOLD}")
    print(f"output: {out}")

    # 1–2. Object-motion sequence (Blender preferred).
    obj_seq = ensure_sequence(out / "object_motion", "object_motion")
    obj_run = run_retina_on_sequence(
        obj_seq["frames_dir"],
        stream_id="object-motion",
        source_type=(
            SourceType.BLENDER_RENDER
            if obj_seq["status"] == "blender"
            else SourceType.IMAGE_SEQUENCE
        ),
    )
    obj_score = score_events(
        obj_run.get("events", []),
        obj_seq["manifest"].get("ground_truth", obj_seq["manifest"].get("frames", [])),
    )

    # 5. Camera-motion sequence.
    cam_seq = ensure_sequence(out / "camera_motion", "camera_motion")
    cam_run = run_retina_on_sequence(
        cam_seq["frames_dir"],
        stream_id="camera-motion",
        source_type=(
            SourceType.BLENDER_RENDER
            if cam_seq["status"] == "blender"
            else SourceType.IMAGE_SEQUENCE
        ),
    )
    cam_counts = cam_run.get("event_counts", {})
    obj_counts = obj_run.get("event_counts", {})

    print("\n=== Event tallies (side by side) ===")
    all_keys = sorted(set(obj_counts) | set(cam_counts))
    print(f"{'event':24} {'object_seq':>12} {'camera_seq':>12}")
    for key in all_keys:
        print(f"{key:24} {obj_counts.get(key, 0):12d} {cam_counts.get(key, 0):12d}")

    print("\n=== Per-type precision / recall (object sequence) ===")
    for et, row in sorted(obj_score["per_type"].items()):
        print(
            f"{et:24} P={row['precision']:.2f} R={row['recall']:.2f} "
            f"tp={row['tp']} fp={row['fp']} fn={row['fn']} "
            f"(exp={row['expected']} pred={row['predicted']})"
        )

    print("\n=== Latency / memory ===")
    print(f"reflex   p50={obj_run['reflex_latency_ms']['p50']:.3f} ms  "
          f"p95={obj_run['reflex_latency_ms']['p95']:.3f} ms")
    print(f"attentive p50={obj_run['attentive_latency_ms']['p50']:.3f} ms  "
          f"p95={obj_run['attentive_latency_ms']['p95']:.3f} ms")
    print(f"dropped_frames={obj_run.get('dropped_frames', 0)}")
    print(f"peak_rss_mb={_peak_rss_mb():.1f}")

    # 6. Webcam attempt.
    webcam = attempt_webcam(out)

    receipt = {
        "recall_threshold": RECALL_THRESHOLD,
        "hard_events": list(HARD_EVENTS),
        "object_sequence": {
            "source": obj_seq["status"],
            "attestation": obj_seq.get("attestation"),
            "run": {
                k: v
                for k, v in obj_run.items()
                if k != "events"
            },
            "events": obj_run.get("events", []),
            "score": obj_score,
        },
        "camera_sequence": {
            "source": cam_seq["status"],
            "attestation": cam_seq.get("attestation"),
            "run": {k: v for k, v in cam_run.items() if k != "events"},
            "events": cam_run.get("events", []),
            "event_counts": cam_counts,
        },
        "webcam": webcam,
        "peak_rss_mb": _peak_rss_mb(),
    }
    (out / "receipt.json").write_text(
        json.dumps(receipt, indent=2, sort_keys=True, default=str) + "\n",
        encoding="utf-8",
    )
    print(f"\nWrote {out / 'receipt.json'}")

    # Gates.
    failures: list[str] = []
    for et in HARD_EVENTS:
        row = obj_score["per_type"].get(et)
        if row is None:
            # If GT never expected it, skip; procedural GT always includes them.
            if obj_score["expected_totals"].get(et, 0) > 0:
                failures.append(f"{et}: missing from scores")
            continue
        if row["expected"] > 0 and row["recall"] < RECALL_THRESHOLD:
            failures.append(
                f"{et}: recall {row['recall']:.2f} < threshold {RECALL_THRESHOLD}"
            )

    if cam_counts.get("OBJECT_MOVED", 0) > 0 and cam_counts.get("CAMERA_MOVED", 0) == 0:
        failures.append(
            "camera sequence reported OBJECT_MOVED without CAMERA_MOVED "
            f"(counts={cam_counts})"
        )
    # Stronger: any OBJECT_MOVED on pure camera pan is a separation failure.
    if cam_seq["manifest"].get("mode") == "camera_motion":
        if cam_counts.get("OBJECT_MOVED", 0) > 0:
            failures.append(
                f"camera-motion misreported as object motion: OBJECT_MOVED="
                f"{cam_counts.get('OBJECT_MOVED')}"
            )
        if cam_counts.get("CAMERA_MOVED", 0) < 1:
            failures.append("camera sequence produced no CAMERA_MOVED events")

    if failures:
        print("\nFAIL:")
        for item in failures:
            print(f"  - {item}")
        return 1

    print("\nPASS: recall thresholds met; camera/object motion separated")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception:
        traceback.print_exc()
        raise SystemExit(2) from None
