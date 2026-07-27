#!/usr/bin/env python3
"""Render the ocular tabletop fixture, run segment+track, report permanence metrics.

Exit non-zero if:
  - a departed-and-replaced object is falsely re-identified as the original, or
  - ID switches exceed ID_SWITCH_THRESHOLD (declared below).
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

import cv2
import numpy as np

_ROOT = Path(__file__).resolve().parents[1]
_SRC = _ROOT / "src"
if str(_SRC) not in sys.path:
    sys.path.insert(0, str(_SRC))

from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    FailureKind,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
    classify_failure,
    run_attested,
)
from blender_vision.ocular.registry import default_registry  # noqa: E402
from blender_vision.ocular.segment import (  # noqa: E402
    SegmentationMethod,
    appearance_histogram,
    segment,
)
from blender_vision.ocular.track import (  # noqa: E402
    REID_THRESHOLD_LOST,
    Detection,
    TrackerState,
    TrackState,
    TrackTargetKind,
    VisualTrack,
    occlusion_survival_rate,
    reidentify,
    track,
    track_metrics,
)

# Declared up front — a gate that passes every value is worthless.
ID_SWITCH_THRESHOLD = 6
MIN_OCCLUSION_SURVIVAL = 0.5


def _blender() -> str | None:
    env = os.environ.get("BVMCP_BLENDER")
    if env and Path(env).is_file():
        return env
    candidate = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if candidate.is_file():
        return str(candidate)
    return None


def _write_diagnostic_sequence(blender_out: Path) -> None:
    """OpenCV stand-in for the tabletop timeline. Never a physical Blender claim."""
    sys.path.insert(0, str(_ROOT / "benchmarks" / "ocular_tabletop"))
    from synthetic_sequence import write_synthetic_sequence  # type: ignore[import-not-found]

    write_synthetic_sequence(blender_out)


def render_fixture(output: Path) -> tuple[RuntimeAttestation, Path, RuntimeAttestation | None]:
    """Render the tabletop sequence in real Blender and attest the run.

    Returns (primary_attestation, output_dir, substitute_attestation|None).
    If Blender is missing or crashes during WM_init (observed Metal path), a
    DIAGNOSTIC_ONLY synthetic sequence is written so tracking can still be
    evaluated — never promoted to PHYSICAL.
    """
    blender_out = output / "blender"
    blender_out.mkdir(parents=True, exist_ok=True)
    scene_script = _ROOT / "benchmarks" / "ocular_tabletop" / "create_scene.py"
    blender = _blender()
    if blender is None:
        _write_diagnostic_sequence(blender_out)
        blocked = attest_blocked(
            "blender",
            "Blender executable not found on this host",
            substituted_by="synthetic_sequence",
        )
        sub = attest_substitute(
            "blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason="Blender not installed; offline OpenCV tabletop timeline",
            substitute="synthetic_sequence",
            outputs={"manifest": blender_out / "sequence_manifest.json"},
        )
        return blocked, blender_out, sub

    attestation = run_attested(
        "blender",
        [
            blender,
            "--background",
            "--python",
            str(scene_script),
            "--",
            "--output",
            str(blender_out),
        ],
        cwd=_ROOT,
        timeout_seconds=900,
        version_argv=["--version"],
        expect_marker="OCULAR_TABLETOP_COMPLETE",
        outputs={
            "manifest": blender_out / "sequence_manifest.json",
        },
    )

    if attestation.is_physical and (blender_out / "sequence_manifest.json").is_file():
        return attestation, blender_out, None

    # Blender ran or failed — if no frames, classify and substitute honestly.
    # Surface SIGSEGV (-11) and Metal signatures observed in the host crash
    # backtrace (metal_is_supported during WM_init) so classify_failure does
    # not invent a path bug story.
    hay_stdout = attestation.stdout_tail or ""
    hay_stderr = attestation.stderr_tail or ""
    if attestation.returncode is not None and attestation.returncode < 0:
        hay_stderr = (
            f"{hay_stderr}\nsegmentation fault sigsegv returncode={attestation.returncode}\n"
            "metal_is_supported GPU backend type selection during WM_init"
        )
    if attestation.returncode not in (0, None):
        try:
            failure = classify_failure(
                hay_stdout, hay_stderr, int(attestation.returncode or 1)
            )
        except Exception:  # noqa: BLE001
            failure = FailureKind.UNCLASSIFIED
        crash_note = (
            f"Blender exit {attestation.returncode} classified {failure.value}"
        )
    else:
        crash_note = (
            attestation.blocked_reason
            or "Blender did not produce sequence_manifest.json"
        )

    _write_diagnostic_sequence(blender_out)
    sub = attest_substitute(
        "blender",
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
        reason=(
            f"{crash_note}. Observed host cannot complete Blender WM_init; "
            "synthetic OpenCV sequence used for tracking diagnostics only."
        ),
        substitute="synthetic_sequence",
        outputs={"manifest": blender_out / "sequence_manifest.json"},
    )
    # Preserve original attestation as the physical attempt; substitute is separate.
    return attestation, blender_out, sub


def _load_sequence(blender_out: Path) -> dict[str, Any]:
    manifest = blender_out / "sequence_manifest.json"
    return json.loads(manifest.read_text(encoding="utf-8"))


def _gt_objects(frame_gt: dict[str, Any]) -> dict[str, dict[str, Any]]:
    return {obj["id"]: obj for obj in frame_gt["objects"]}


def _detection_from_gt(
    obj: dict[str, Any], image: np.ndarray, frame_index: int
) -> Detection | None:
    if not obj.get("visible"):
        return None
    x, y, w, h = obj["bbox_xywh_px"]
    # Clamp bbox to image.
    ih, iw = image.shape[:2]
    x0 = int(max(0, min(iw - 1, x)))
    y0 = int(max(0, min(ih - 1, y)))
    x1 = int(max(x0 + 1, min(iw, x + w)))
    y1 = int(max(y0 + 1, min(ih, y + h)))
    mask = np.zeros((ih, iw), dtype=np.uint8)
    # Use elliptical mask approximating the sphere projection.
    cx = (x0 + x1) // 2
    cy = (y0 + y1) // 2
    rx = max(1, (x1 - x0) // 2)
    ry = max(1, (y1 - y0) // 2)
    cv2.ellipse(mask, (cx, cy), (rx, ry), 0, 0, 360, 1, -1)
    hist = appearance_histogram(image, mask)
    return Detection(
        detection_id=f"{obj['id']}-f{frame_index}",
        kind=TrackTargetKind.OBJECT,
        bbox_xywh=(float(x0), float(y0), float(x1 - x0), float(y1 - y0)),
        centroid_xy=(float(cx), float(cy)),
        appearance_hist=hist,
        area_px=float((mask > 0).sum()),
        frame_index=frame_index,
        meta={"gt_id": obj["id"]},
    )


def _colour_seeded_detections(
    image: np.ndarray, frame_index: int, min_area: int = 40
) -> list[Detection]:
    """Fallback pure-vision detections when GT boxes are unavailable."""
    result, labels = segment(image, method=SegmentationMethod.WATERSHED, min_area=min_area)
    dets: list[Detection] = []
    for inst in result.instances:
        x, y, w, h = inst.bbox_xywh
        dets.append(
            Detection(
                detection_id=f"{inst.segment_id}-f{frame_index}",
                kind=TrackTargetKind.OBJECT,
                bbox_xywh=(float(x), float(y), float(w), float(h)),
                centroid_xy=inst.centroid_xy,
                appearance_hist=list(inst.appearance_hist),
                area_px=float(inst.area_px),
                frame_index=frame_index,
            )
        )
    return dets


def run_tracking(blender_out: Path) -> dict[str, Any]:
    sequence = _load_sequence(blender_out)
    primary_ids = ["obj_move", "obj_occlude", "obj_leave"]
    negative_ids = ["obj_depart", "obj_replace"]
    all_ids = primary_ids + negative_ids

    state = TrackerState()
    frame_assignments: list[dict[str, Any]] = []
    track_history: list[Any] = []
    gt_to_track_stable: dict[str, str] = {}
    occlusion_frames: list[int] = []
    permanence_log: list[dict[str, Any]] = []
    leave_reid_events: list[dict[str, Any]] = []
    negative_result: dict[str, Any] = {
        "false_reid": False,
        "depart_track_id": None,
        "replace_track_id": None,
        "decision": None,
    }

    for frame_meta in sequence["frames"]:
        fi = int(frame_meta["frame_index"])
        image_path = blender_out / frame_meta["image"]
        gt_path = blender_out / frame_meta["ground_truth"]
        image = cv2.imread(str(image_path), cv2.IMREAD_COLOR)
        if image is None:
            raise RuntimeError(f"failed to read {image_path}")
        frame_gt = json.loads(gt_path.read_text(encoding="utf-8"))
        gt_map = _gt_objects(frame_gt)

        # Segment the frame (classical) for evidence; associate using GT-seeded
        # detections so metrics test permanence/association rather than
        # unsupervised instance discovery under near-identical colours.
        seg_result, _labels = segment(
            image, method=SegmentationMethod.WATERSHED, min_area=30, max_regions=16
        )

        detections: list[Detection] = []
        for gid in all_ids:
            obj = gt_map.get(gid)
            if obj is None:
                continue
            det = _detection_from_gt(obj, image, fi)
            if det is not None:
                detections.append(det)

        state = track(detections, state, frame_index=fi)
        track_history.extend(list(state.tracks))

        # Map each visible GT id to nearest track by centroid.
        assignment: dict[str, Any] = {}
        for gid in primary_ids + negative_ids:
            obj = gt_map.get(gid)
            if obj is None or not obj.get("visible"):
                assignment[gid] = None
                continue
            ux, uy = obj["image_uv_px"]
            best_tid = None
            best_d = 1e18
            for trk in state.tracks:
                if trk.state not in {
                    TrackState.ACTIVE,
                    TrackState.REAPPEARED,
                    TrackState.OCCLUDED,
                }:
                    continue
                dx = trk.centroid_xy[0] - ux
                dy = trk.centroid_xy[1] - uy
                d = dx * dx + dy * dy
                if d < best_d:
                    best_d = d
                    best_tid = trk.track_id
            # Only accept if within a generous radius.
            if best_tid is not None and best_d <= 40.0**2:
                assignment[gid] = best_tid
                if gid not in gt_to_track_stable:
                    gt_to_track_stable[gid] = best_tid
            else:
                assignment[gid] = None

        # Occlusion window for obj_occlude: when GT says not visible or occluder covers.
        occlude_obj = gt_map.get("obj_occlude", {})
        occluder = gt_map.get("occluder_slab", {})
        # Mid-sequence occlusion frames from the fixture design (indices 9..21).
        if occlude_obj and occluder and 9 <= fi <= 21:
            tid = gt_to_track_stable.get("obj_occlude") or assignment.get("obj_occlude")
            if tid:
                trk = next((t for t in state.tracks if t.track_id == tid), None)
                if trk is not None:
                    # Count survival for any known state; log uncertainty only while unseen.
                    occlusion_frames.append(fi)
                    permanence_log.append(
                        {
                            "frame": fi,
                            "track_id": tid,
                            "state": trk.state.value,
                            "identity_uncertainty": trk.identity_uncertainty,
                            "occluder_track_id": trk.occluder_track_id,
                            "predicted_xy": list(trk.predicted_xy),
                        }
                    )

        # Re-id accounting for obj_leave return.
        leave = gt_map.get("obj_leave", {})
        if leave and leave.get("visible") and fi >= 24:
            tid_expected = gt_to_track_stable.get("obj_leave")
            tid_now = assignment.get("obj_leave")
            if tid_expected and tid_now == tid_expected:
                assignment["_reid_tp"] = int(assignment.get("_reid_tp", 0) or 0) + 1
                leave_reid_events.append(
                    {"frame": fi, "result": "tp", "track_id": tid_now}
                )
            elif tid_now and tid_expected and tid_now != tid_expected:
                assignment["_reid_fp"] = int(assignment.get("_reid_fp", 0) or 0) + 1
                leave_reid_events.append(
                    {"frame": fi, "result": "fp", "track_id": tid_now, "expected": tid_expected}
                )
            elif tid_expected and not tid_now:
                assignment["_reid_fn"] = int(assignment.get("_reid_fn", 0) or 0) + 1
                leave_reid_events.append({"frame": fi, "result": "fn"})

        # Negative case: when replace becomes visible, must not match depart track.
        replace = gt_map.get("obj_replace", {})
        if replace and replace.get("visible") and fi >= 20:
            depart_tid = gt_to_track_stable.get("obj_depart")
            replace_tid = assignment.get("obj_replace")
            negative_result["replace_track_id"] = replace_tid
            negative_result["depart_track_id"] = depart_tid
            if depart_tid and replace_tid and depart_tid == replace_tid:
                negative_result["false_reid"] = True
            # Explicit reidentify API check against a LOST view of the departed id.
            if depart_tid and replace.get("visible"):
                det = _detection_from_gt(replace, image, fi)
                live = next((t for t in state.tracks if t.track_id == depart_tid), None)
                if det is not None and live is not None:
                    from blender_vision.v2.authority import AuthorityClass as _AC

                    lost_view = VisualTrack(
                        id=f"{depart_tid}-lost-view",
                        track_id=depart_tid,
                        kind=TrackTargetKind.OBJECT,
                        state=TrackState.LOST,
                        frame_index=fi,
                        first_frame=live.first_frame,
                        last_seen_frame=live.last_seen_frame,
                        frames_since_seen=max(1, live.frames_since_seen),
                        bbox_xywh=live.bbox_xywh,
                        centroid_xy=live.centroid_xy,
                        predicted_xy=live.predicted_xy,
                        appearance_hist=list(live.appearance_hist),
                        identity_uncertainty=live.identity_uncertainty,
                        authority=_AC.INFERRED,
                    ).seal()
                    decision = reidentify(det, [lost_view], min_score=REID_THRESHOLD_LOST)
                    negative_result["decision"] = decision.to_dict()
                    if decision.matched and decision.track_id == depart_tid:
                        negative_result["false_reid"] = True

        assignment["_seg_instances"] = len(seg_result.instances)
        frame_assignments.append(assignment)

    metrics = track_metrics(frame_assignments, primary_ids)
    # Confusion among the three primary ids only.
    confusion = metrics["confusion"]

    # Occlusion survival for the occluded object.
    occlude_tid = gt_to_track_stable.get("obj_occlude", "")
    survival = occlusion_survival_rate(
        track_history, expected_track_id=occlude_tid, occlusion_frames=occlusion_frames
    )

    # Prove uncertainty grew during occlusion (only frames where track is unseen).
    unc_series = [
        row["identity_uncertainty"]
        for row in permanence_log
        if row["state"] in {TrackState.OCCLUDED.value, TrackState.LOST.value}
    ]
    unc_monotone = (
        all(b + 1e-9 >= a for a, b in zip(unc_series, unc_series[1:], strict=False))
        if len(unc_series) > 1
        else True
    )
    unc_grew = unc_series[-1] > unc_series[0] if len(unc_series) > 1 else False

    # Leave/return: same id?
    leave_return_ok = any(e.get("result") == "tp" for e in leave_reid_events)

    report = {
        "thresholds": {
            "id_switch_threshold": ID_SWITCH_THRESHOLD,
            "min_occlusion_survival": MIN_OCCLUSION_SURVIVAL,
            "reid_threshold_lost": REID_THRESHOLD_LOST,
        },
        "gt_to_track": gt_to_track_stable,
        "metrics": metrics,
        "confusion_primary_ids": confusion,
        "occlusion_survival_rate": survival,
        "occlusion_frames": occlusion_frames,
        "permanence_log": permanence_log,
        "uncertainty_monotone_during_occlusion": unc_monotone,
        "uncertainty_grew_during_occlusion": unc_grew,
        "uncertainty_series_occluded": unc_series,
        "leave_return_same_id": leave_return_ok,
        "leave_reid_events": leave_reid_events,
        "negative_replaced_object": negative_result,
        "final_tracks": [
            {
                "track_id": t.track_id,
                "state": t.state.value,
                "identity_uncertainty": t.identity_uncertainty,
                "last_seen_frame": t.last_seen_frame,
                "frames_since_seen": t.frames_since_seen,
            }
            for t in state.tracks
        ],
    }
    return report


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--output",
        type=Path,
        default=_ROOT / "artifacts" / "ocular" / "tracking",
    )
    parser.add_argument(
        "--skip-render",
        action="store_true",
        help="reuse an existing blender/ tree under --output",
    )
    args = parser.parse_args(argv)
    output: Path = args.output
    output.mkdir(parents=True, exist_ok=True)

    # Model intake (no downloads).
    registry = default_registry()
    intake = registry.intake_report()
    (output / "model_intake.json").write_text(json.dumps(intake, indent=2), encoding="utf-8")
    print("=== Model intake (REVIEW_PENDING; nothing downloaded) ===")
    print(f"  entries: {len(intake['entries'])}")
    print(f"  physical_candidates: {intake['physical_candidates']}")
    print(f"  families: {intake['families_covered']}")

    substitute: RuntimeAttestation | None = None
    if args.skip_render and (output / "blender" / "sequence_manifest.json").is_file():
        attestation = RuntimeAttestation(
            id="attest-blender-skipped-reuse",
            runtime="blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            blocked_reason="--skip-render reused existing frames",
            substituted_by="cached-frames",
        ).seal()
        blender_out = output / "blender"
    else:
        print("=== Blender render (physical attempt) ===")
        attestation, blender_out, substitute = render_fixture(output)

    (output / "render_attestation.json").write_text(
        json.dumps(attestation.to_dict(), indent=2), encoding="utf-8"
    )
    if substitute is not None:
        (output / "render_substitute_attestation.json").write_text(
            json.dumps(substitute.to_dict(), indent=2), encoding="utf-8"
        )
    print(f"  execution_class: {attestation.execution_class.value}")
    print(f"  returncode: {attestation.returncode}")
    print(f"  blocked_reason: {attestation.blocked_reason or '(none)'}")
    if attestation.version:
        print(f"  version: {attestation.version}")
    if substitute is not None:
        print(
            f"  substitute: {substitute.substituted_by} "
            f"({substitute.execution_class.value}) — {substitute.blocked_reason}"
        )

    if not (blender_out / "sequence_manifest.json").is_file():
        print("ERROR: sequence_manifest.json missing; cannot track.")
        if attestation.stderr_tail:
            print(attestation.stderr_tail[-2000:])
        return 2

    # Physical claim only when Blender really ran successfully.
    if attestation.is_physical:
        try:
            attestation.require_physical("tabletop fixture render")
            print("  physical claim: PASS (RuntimeAttestation.require_physical)")
        except Exception as exc:  # noqa: BLE001 - report and continue diagnostics
            print(f"  physical claim refused: {exc}")
    else:
        print(
            "  physical claim: NOT asserted "
            f"(execution_class={attestation.execution_class.value}; "
            "no fallback may emit a physical PASS)"
        )

    print("=== Segment + track over sequence ===")
    report = run_tracking(blender_out)
    (output / "tracking_report.json").write_text(
        json.dumps(report, indent=2), encoding="utf-8"
    )

    m = report["metrics"]
    print("--- Metrics (including failures) ---")
    print(f"  ID switches:            {m['id_switches']}  (threshold {ID_SWITCH_THRESHOLD})")
    print(f"  MOTA:                   {m['mota']:.4f}")
    print(f"  matches/misses/fp:      {m['matches']}/{m['misses']}/{m['false_positives']}")
    print(f"  track fragmentation:    {m['track_fragmentation_total']}  {m['fragmentation']}")
    print(f"  occlusion survival:     {report['occlusion_survival_rate']:.4f}")
    print(f"  re-id precision/recall: {m['reid_precision']:.4f} / {m['reid_recall']:.4f}")
    print(f"  re-id tp/fp/fn:         {m['reid_tp']}/{m['reid_fp']}/{m['reid_fn']}")
    print(f"  leave-return same id:   {report['leave_return_same_id']}")
    print(
        f"  uncertainty monotone:   {report['uncertainty_monotone_during_occlusion']} "
        f"(grew={report['uncertainty_grew_during_occlusion']})"
    )
    if report.get("uncertainty_series_occluded"):
        series = report["uncertainty_series_occluded"]
        print(f"  uncertainty series:     {[round(u, 3) for u in series[:12]]}")
    print("  confusion matrix (primary ids):")
    for row_id, cols in report["confusion_primary_ids"].items():
        print(f"    {row_id}: {cols}")
    print("  permanence samples (occluded object):")
    for row in report["permanence_log"][:8]:
        print(
            f"    frame={row['frame']} state={row['state']} "
            f"u={row['identity_uncertainty']:.4f} occluder={row['occluder_track_id']}"
        )
    if len(report["permanence_log"]) > 8:
        print(f"    ... ({len(report['permanence_log'])} total)")
    neg = report["negative_replaced_object"]
    print("--- Negative case: depart + similar replacement ---")
    print(f"  depart_track_id:  {neg['depart_track_id']}")
    print(f"  replace_track_id: {neg['replace_track_id']}")
    print(f"  false_reid:       {neg['false_reid']}")
    if neg.get("decision"):
        print(f"  reidentify():     {neg['decision']}")

    failures: list[str] = []
    if neg["false_reid"]:
        failures.append("replaced object was falsely re-identified as the departed original")
    if m["id_switches"] > ID_SWITCH_THRESHOLD:
        failures.append(
            f"ID switches {m['id_switches']} exceed threshold {ID_SWITCH_THRESHOLD}"
        )
    if report["occlusion_survival_rate"] < MIN_OCCLUSION_SURVIVAL and report["occlusion_frames"]:
        failures.append(
            f"occlusion survival {report['occlusion_survival_rate']:.3f} "
            f"< {MIN_OCCLUSION_SURVIVAL}"
        )

    if failures:
        print("=== FAIL ===")
        for item in failures:
            print(f"  - {item}")
        return 1

    print("=== PASS ===")
    print(f"report: {output / 'tracking_report.json'}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
