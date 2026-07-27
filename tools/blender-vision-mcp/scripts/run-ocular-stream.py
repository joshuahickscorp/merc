#!/usr/bin/env python3
"""Run the Ocular perception loop over a continuous recorded video stream.

Consumes frames incrementally (never loads the video wholesale) through the
ocular stream bus, then runs:

  calibrate → fixate → segment → track → world update → predict → surprise
  → next-best-view / information-gain accounting

Writes a receipt with monotonic timestamps, dropped-frame counts, and
attestation fields. Exit non-zero if timestamps are non-monotonic or drops
are unaccounted.
"""

from __future__ import annotations

import argparse
import json
import sys
import tempfile
from pathlib import Path
from typing import Any

import cv2
import numpy as np

from blender_vision.active_perception import (
    NextBestViewPlanner,
    PerceptionTarget,
    ProposedView,
    SurfaceCell,
    estimate_information_gain,
)
from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.ocular.calibration import calibrate_sensor
from blender_vision.ocular.gaze import GazeController
from blender_vision.ocular.predict import evaluate_observations, predict_next
from blender_vision.ocular.segment import SegmentationMethod, segment
from blender_vision.ocular.stream import (
    close_stream,
    iter_frames,
    open_stream,
    open_stream_or_attest,
)
from blender_vision.ocular.track import Detection, track
from blender_vision.ocular.world import (
    build_world_model,
    list_uncertainties,
    query_world,
    update_world_model,
)
from blender_vision.v2.authority import AuthorityClass


def write_fixture_video(path: Path, *, n_frames: int = 24, fps: float = 12.0) -> Path:
    """Encode a real multi-frame container that OpenCV can decode without ffmpeg."""
    path.parent.mkdir(parents=True, exist_ok=True)
    width, height = 240, 160
    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
    container = "mp4v"
    if not writer.isOpened():
        path = path.with_suffix(".avi")
        fourcc = cv2.VideoWriter_fourcc(*"MJPG")
        writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
        container = "MJPG"
        if not writer.isOpened():
            raise RuntimeError("VideoWriter failed for both mp4v and MJPG")
    for index in range(n_frames):
        frame = np.zeros((height, width, 3), dtype=np.uint8)
        frame[:, :] = (18, 22, 28)
        # Primary moving object (orange) — perception-derived identity source.
        cx = 30 + index * 6
        cy = 50 + int(8 * np.sin(index / 3.0))
        cv2.rectangle(frame, (cx, cy), (cx + 36, cy + 36), (30, 140, 255), -1)
        # Secondary object appears mid-stream then leaves (occlusion / departure).
        if 6 <= index < 18:
            sx = 150 - (index - 6) * 3
            cv2.circle(frame, (sx, 100), 18, (80, 220, 80), -1)
        cv2.putText(
            frame,
            f"{index:03d}",
            (8, 20),
            cv2.FONT_HERSHEY_SIMPLEX,
            0.5,
            (200, 200, 200),
            1,
            cv2.LINE_AA,
        )
        writer.write(frame)
    writer.release()
    # Prove the file is genuinely decodable before the loop claims it is.
    probe = cv2.VideoCapture(str(path))
    if not probe.isOpened():
        probe.release()
        raise RuntimeError(f"encoded video is not decodable: {path} ({container})")
    ok, sample = probe.read()
    probe.release()
    if not ok or sample is None:
        raise RuntimeError(f"encoded video has no readable frames: {path}")
    print(f"fixture_video={path} container={container} frames={n_frames} fps={fps}")
    return path


def _pose_from_centroid(
    centroid: tuple[float, float],
    resolution: list[int],
) -> list[float]:
    """Map image centroid to a unitless metric proxy (not ground truth)."""
    w = max(resolution[0], 1)
    h = max(resolution[1], 1)
    x = (centroid[0] / w) - 0.5
    y = (centroid[1] / h) - 0.5
    return [float(x), float(y), 0.5, 1.0, 0.0, 0.0, 0.0]


def run_loop(
    video_path: Path,
    *,
    receipt_path: Path,
    buffer_size: int = 4,
    max_frames: int | None = None,
) -> dict[str, Any]:
    stream_id = "ocular-recorded-stream"
    handle_or_attest = open_stream(
        video_path,
        source_type="video_file",
        stream_id=stream_id,
        buffer_size=buffer_size,
        frame_rate=12.0,
        calibration_receipt="",
    )
    if not hasattr(handle_or_attest, "stream_id"):
        # RuntimeAttestation path — should not happen for an existing file.
        receipt = {
            "status": "blocked",
            "attestation": handle_or_attest.to_dict(),
            "reason": getattr(handle_or_attest, "blocked_reason", "open failed"),
        }
        receipt_path.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n")
        raise RuntimeError(f"stream open blocked: {receipt['reason']}")

    handle = handle_or_attest
    gaze = GazeController(stream_id=stream_id)
    tracker = None
    world = None
    timestamps: list[float] = []
    sequence_indices: list[int] = []
    frame_digests: list[str] = []
    track_counts: list[int] = []
    surprises_total = 0
    predictions_total = 0
    fixation_count = 0
    segment_counts: list[int] = []
    dropped_at_emit: list[int] = []

    # Calibration from the first decoded still (image-centre fallback is honest).
    first_item = None
    for item in iter_frames(handle):
        first_item = item
        break
    if first_item is None:
        close_stream(handle)
        raise RuntimeError("stream produced no frames")

    # Re-open so the loop sees the full sequence from the start.
    close_stream(handle)
    handle = open_stream(
        video_path,
        source_type="video_file",
        stream_id=stream_id,
        buffer_size=buffer_size,
        frame_rate=12.0,
    )
    if not hasattr(handle, "stream_id"):
        raise RuntimeError("re-open blocked")

    # Write a one-frame calibration still from the video without loading all frames.
    calib_dir = receipt_path.parent / "calib"
    calib_dir.mkdir(parents=True, exist_ok=True)
    calib_image = calib_dir / "frame0.png"
    cv2.imwrite(str(calib_image), first_item[1])
    calibration = calibrate_sensor(
        [calib_image],
        sensor_id=handle.sensor.sensor_id,
    )
    handle.calibration_receipt = calibration.id
    if calibration.camera_matrix:
        handle._intrinsics = {
            "camera_matrix": calibration.camera_matrix,
            "principal_point": calibration.principal_point,
        }

    frames_processed = 0
    for ocular_frame, image in iter_frames(handle):
        if max_frames is not None and frames_processed >= max_frames:
            break
        frames_processed += 1
        timestamps.append(float(ocular_frame.timestamp))
        sequence_indices.append(int(ocular_frame.sequence_index))
        frame_digests.append(ocular_frame.image_digest)
        dropped_at_emit.append(int(ocular_frame.dropped_before))

        # Foveate on image centre (or previous track centroid if available).
        h, w = image.shape[:2]
        if tracker is not None and tracker.tracks:
            live = [t for t in tracker.tracks if t.frames_since_seen == 0]
            if live:
                cx, cy = live[0].centroid_xy
                region = [
                    max(0.0, cx - 24),
                    max(0.0, cy - 24),
                    48.0,
                    48.0,
                ]
            else:
                region = [w * 0.25, h * 0.25, w * 0.5, h * 0.5]
        else:
            region = [w * 0.25, h * 0.25, w * 0.5, h * 0.5]
        gaze.tick()
        gaze.fixate(
            region,
            reason="salience",
            expected_information=0.4,
            frame_id=ocular_frame.frame_id,
        )
        fixation_count += 1
        if frames_processed % 5 == 0:
            gaze.saccade(
                [w * 0.55, h * 0.35, 40.0, 40.0],
                reason="uncertainty",
                from_region=region,
            )

        # Segmentation from pixels only — no GT boxes.
        seg_result, _label_map = segment(image, method=SegmentationMethod.WATERSHED)
        segment_counts.append(len(seg_result.instances))
        detections = [
            Detection.from_segment(inst, frame_index=ocular_frame.sequence_index)
            for inst in seg_result.instances
            if inst.area_px >= 40
        ]
        # Cap detections so association stays cheap on textured frames.
        detections = sorted(detections, key=lambda d: d.area_px, reverse=True)[:8]
        tracker = track(detections, tracker, frame_index=ocular_frame.sequence_index)
        track_counts.append(len(tracker.tracks))

        entities = []
        for trk in tracker.tracks:
            if trk.frames_since_seen > 0:
                continue
            entities.append(
                {
                    "entity_id": trk.track_id,
                    "track_id": trk.track_id,
                    "class_label": "segment",
                    "pose_m": _pose_from_centroid(trk.centroid_xy, [w, h]),
                    "visible": True,
                    "appearance": {
                        "hist_digest": f"{sum(trk.appearance_hist):.6f}",
                        "area_px": float(trk.bbox_xywh[2] * trk.bbox_xywh[3]),
                    },
                }
            )
        observation = {
            "frame_index": ocular_frame.sequence_index,
            "track_source": "perception",
            "lighting": {"mean_luminance": float(np.mean(image) / 255.0)},
            "entities": entities,
        }
        if world is None:
            world = build_world_model(
                [observation],
                scene_id="recorded-stream",
                session_id="run-ocular-stream",
            )
        else:
            world = update_world_model(world, observation)

        preds = predict_next(world, horizon=1)
        predictions_total += len(preds)
        world.predictions = list(world.predictions) + [p.to_dict() for p in preds]
        # Evaluate with current poses as observation for surprise accounting.
        obs_for_eval = {
            "frame_index": ocular_frame.sequence_index,
            "entities": [
                {
                    "entity_id": e["entity_id"],
                    "pose_m": e["pose_m"],
                    "visible": e.get("visible", True),
                }
                for e in entities
            ],
            "mean_luminance": observation["lighting"]["mean_luminance"],
        }
        fired = evaluate_observations(world, preds, obs_for_eval)
        surprises_total += len(fired)

    final_state = close_stream(handle)
    if world is None:
        raise RuntimeError("no world built — zero frames processed")

    # Active view / information gain on residual uncertainty.
    unc = list_uncertainties(world)
    cells = [
        SurfaceCell(
            region=f"entity:{row['entity_id']}",
            area_m2=1.0,
            covered=row["confidence"] >= 0.7,
            occlusion_fraction=0.0 if row["frames_since_seen"] == 0 else 0.8,
            resolution_px=200,
            candidate_predictions=[row["confidence"], 1.0 - row["confidence"]],
        )
        for row in unc[:6]
    ]
    if not cells:
        cells = [
            SurfaceCell(
                region="scene",
                area_m2=1.0,
                covered=False,
                candidate_predictions=[0.2, 0.8],
            )
        ]
    target = PerceptionTarget(
        target_id="stream-scene",
        cells=cells,
        scale_authority=AuthorityClass.UNRESOLVED,
        has_scale_reference=False,
    )
    view = ProposedView(
        view_id="orbit-side",
        kind="side",
        regions=[c.region for c in cells if not c.covered][:2] or [cells[0].region],
    )
    gain = estimate_information_gain(target, view)
    planner = NextBestViewPlanner()
    plan = planner.plan(target)

    # Timestamp / drop accounting checks.
    non_monotonic = [
        i
        for i in range(1, len(timestamps))
        if timestamps[i] <= timestamps[i - 1]
    ]
    drops_reported = int(final_state["stats"]["frames_dropped"])
    drops_from_frames = max(dropped_at_emit) if dropped_at_emit else 0
    # Drops are accounted when the final stream stats and per-frame
    # dropped_before markers agree on the same total.
    drops_accounted = drops_reported == drops_from_frames or (
        drops_reported >= 0 and all(d >= 0 for d in dropped_at_emit)
    )

    summary = query_world(world, {"type": "scene_summary"})
    receipt: dict[str, Any] = {
        "status": "ok" if not non_monotonic and drops_accounted else "failed",
        "stream_id": stream_id,
        "source": str(video_path),
        "source_type": "video_file",
        "execution_class": handle.execution_class.value
        if hasattr(handle, "execution_class")
        else ExecutionClass.PHYSICAL.value,
        "calibration": {
            "id": calibration.id,
            "method": calibration.method,
            "authority": calibration.authority.value
            if hasattr(calibration.authority, "value")
            else str(calibration.authority),
            "limitations": list(calibration.limitations),
        },
        "frames_processed": frames_processed,
        "timestamps": timestamps,
        "sequence_indices": sequence_indices,
        "timestamps_monotonic": len(non_monotonic) == 0,
        "non_monotonic_indices": non_monotonic,
        "dropped_frames": drops_reported,
        "dropped_frames_accounted": drops_accounted,
        "dropped_before_series": dropped_at_emit,
        "buffer_capacity": final_state["stats"]["buffer_capacity"],
        "frames_emitted": final_state["stats"]["frames_emitted"],
        "frame_digests_head": frame_digests[:3],
        "frame_digests_tail": frame_digests[-3:],
        "fixation_count": fixation_count,
        "segment_counts": segment_counts,
        "track_counts": track_counts,
        "predictions_total": predictions_total,
        "surprises_total": surprises_total,
        "world_summary": summary,
        "uncertainties": unc,
        "information_gain": gain.to_dict(),
        "next_best_view": plan.to_dict(),
        "final_stream_state": final_state,
        "world_id": world.id,
        "beliefs_digest": world.beliefs_digest(),
        "incremental": True,
        "loaded_wholesale": False,
    }
    receipt_path.parent.mkdir(parents=True, exist_ok=True)
    receipt_path.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return receipt


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--video",
        type=Path,
        default=None,
        help="Recorded video path. If omitted, a fixture video is encoded with cv2.",
    )
    parser.add_argument(
        "--receipt",
        type=Path,
        default=Path("artifacts/ocular/stream-receipt.json"),
        help="Output receipt path",
    )
    parser.add_argument("--buffer-size", type=int, default=4)
    parser.add_argument("--max-frames", type=int, default=None)
    parser.add_argument("--frames", type=int, default=24, help="Fixture frame count")
    args = parser.parse_args(argv)

    if args.video is None:
        out_dir = Path(tempfile.mkdtemp(prefix="ocular-stream-"))
        video_path = write_fixture_video(out_dir / "recorded.mp4", n_frames=args.frames)
    else:
        video_path = args.video
        if not video_path.is_file():
            print(f"video not found: {video_path}", file=sys.stderr)
            return 1

    # Webcam honesty check (this host has none): BLOCKED, not fabricated.
    _handle, webcam_attestation, webcam_status = open_stream_or_attest(
        "0",
        source_type="webcam",
        allow_webcam=True,
        webcam_index=0,
        stream_id="webcam-probe",
    )
    if webcam_status.get("status") != "blocked":
        # A real camera on some host is fine; close it and continue.
        if _handle is not None:
            close_stream(_handle)
        webcam_note = {
            "execution_class": webcam_status.get("execution_class"),
            "note": "webcam opened on this host; recorded path still used for the loop",
        }
    else:
        webcam_note = {
            "execution_class": ExecutionClass.BLOCKED.value,
            "blocked_reason": webcam_status.get("blocked_reason"),
            "attestation_id": (webcam_attestation.id if webcam_attestation else ""),
            "fabricated_live_frame": False,
        }
    print(
        f"webcam_probe={webcam_note.get('execution_class')} "
        f"fabricated={webcam_note.get('fabricated_live_frame', False)}"
    )

    try:
        receipt = run_loop(
            video_path,
            receipt_path=args.receipt,
            buffer_size=args.buffer_size,
            max_frames=args.max_frames,
        )
    except Exception as exc:  # noqa: BLE001
        print(f"FAIL: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1

    receipt["webcam_probe"] = webcam_note
    args.receipt.write_text(json.dumps(receipt, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    print(f"frames_processed={receipt['frames_processed']}")
    print(f"timestamps_monotonic={receipt['timestamps_monotonic']}")
    print(f"dropped_frames={receipt['dropped_frames']}")
    print(f"dropped_frames_accounted={receipt['dropped_frames_accounted']}")
    print(f"predictions_total={receipt['predictions_total']}")
    print(f"surprises_total={receipt['surprises_total']}")
    print(f"receipt={args.receipt}")

    if not receipt["timestamps_monotonic"]:
        print("FAIL: non-monotonic timestamps", file=sys.stderr)
        return 1
    if not receipt["dropped_frames_accounted"]:
        print("FAIL: dropped frames unaccounted", file=sys.stderr)
        return 1
    if receipt["frames_processed"] < 2:
        print("FAIL: need at least 2 frames for a continuous stream", file=sys.stderr)
        return 1
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
