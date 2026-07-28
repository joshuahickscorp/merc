#!/usr/bin/env python3
"""Perception-driven ocular tracking over hard conditions.

Pipeline:
  render (Blender EEVEE or diagnostic synthetic)
  → detect from pixels (no GT boxes)
  → associate (Hungarian) + track + permanence
  → sealed evaluator scores predicted tracks against sealed GT

Exit non-zero if:
  - a distractor is falsely re-identified as a departed original,
  - an unknown entering object is absorbed into an existing identity,
  - any ground-truth value reaches the tracker,
  - ID switches exceed ID_SWITCH_THRESHOLD.
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
if str(_ROOT / "benchmarks") not in sys.path:
    sys.path.insert(0, str(_ROOT / "benchmarks"))

from blender_vision.ocular.attestation import (  # noqa: E402
    ExecutionClass,
    FailureKind,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
    classify_failure,
    run_attested,
)
from blender_vision.ocular.detect import (  # noqa: E402
    DetectionConfig,
    DetectionMethod,
    assert_no_ground_truth_in_detections,
    detect,
)
from blender_vision.ocular.registry import default_registry  # noqa: E402
from blender_vision.ocular.track import (  # noqa: E402
    REID_THRESHOLD_LOST,
    Detection,
    TrackerState,
    TrackState,
    VisualTrack,
    assert_no_ground_truth_on_tracks,
    occlusion_survival_rate,
    reidentify,
    track,
    track_metrics,
)

# Declared up front — a gate that passes every value is worthless.
ID_SWITCH_THRESHOLD = 12
MIN_OCCLUSION_SURVIVAL = 0.3

HARD_CONDITIONS = [
    "visually_similar",
    "crossing_paths",
    "partial_occlusion",
    "full_occlusion",
    "lighting_change",
    "scale_change",
    "camera_motion",
    "leave_return",
    "distractor_replacement",
    "unknown_entering",
    "permanence",
]


def _blender() -> str | None:
    env = os.environ.get("BVMCP_BLENDER")
    if env and Path(env).is_file():
        return env
    candidate = Path("/Applications/Blender.app/Contents/MacOS/Blender")
    if candidate.is_file():
        return str(candidate)
    return None


def _write_diagnostic_hard(hard_out: Path) -> None:
    """OpenCV stand-in for the hard suite. Never a physical Blender claim."""
    from ocular_hard.synthetic import write_all_conditions  # type: ignore[import-not-found]

    write_all_conditions(hard_out)


def render_hard_suite(
    output: Path,
) -> tuple[RuntimeAttestation, Path, RuntimeAttestation | None]:
    """Render ten hard conditions + permanence in real Blender and attest.

    Returns (primary_attestation, hard_dir, substitute_attestation|None).
    """
    hard_out = output / "hard"
    hard_out.mkdir(parents=True, exist_ok=True)
    scene_script = _ROOT / "benchmarks" / "ocular_hard" / "create_scene.py"
    blender = _blender()
    if blender is None:
        _write_diagnostic_hard(hard_out)
        blocked = attest_blocked(
            "blender",
            "Blender executable not found on this host",
            substituted_by="synthetic_hard_sequence",
        )
        sub = attest_substitute(
            "blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason="Blender not installed; offline OpenCV hard suite",
            substitute="synthetic_hard_sequence",
            outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
        )
        return blocked, hard_out, sub

    attestation = run_attested(
        "blender",
        [
            blender,
            "--background",
            "--python",
            str(scene_script),
            "--",
            "--output",
            str(hard_out),
        ],
        cwd=_ROOT,
        timeout_seconds=2400,
        version_argv=["--version"],
        expect_marker="OCULAR_HARD_COMPLETE",
        outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
    )

    if attestation.is_physical and (hard_out / "OCULAR_HARD_COMPLETE").is_file():
        return attestation, hard_out, None

    hay_stdout = attestation.stdout_tail or ""
    hay_stderr = attestation.stderr_tail or ""
    if attestation.returncode not in (0, None):
        try:
            failure = classify_failure(
                hay_stdout, hay_stderr, int(attestation.returncode or 1)
            )
        except Exception:  # noqa: BLE001
            failure = FailureKind.UNCLASSIFIED
        crash_note = f"Blender exit {attestation.returncode} classified {failure.value}"
    else:
        crash_note = (
            attestation.blocked_reason
            or "Blender did not produce OCULAR_HARD_COMPLETE"
        )

    _write_diagnostic_hard(hard_out)
    sub = attest_substitute(
        "blender",
        execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
        reason=(
            f"{crash_note}. Synthetic OpenCV hard suite used for tracking "
            "diagnostics only — never promoted to PHYSICAL."
        ),
        substitute="synthetic_hard_sequence",
        outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
    )
    return attestation, hard_out, sub


# ---------------------------------------------------------------------------
# Tracker path — pixels only. No sealed GT is opened here.
# ---------------------------------------------------------------------------


def run_tracker_on_condition(condition_dir: Path) -> dict[str, Any]:
    """Detect → associate → track using only the builder-visible frames.

    Raises AssertionError if any ground-truth symbol reaches the tracker.
    """
    manifest_path = condition_dir / "sequence_manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

    # Contract: builder-visible frames must not embed GT paths.
    for frame_meta in manifest["frames"]:
        if "ground_truth" in frame_meta:
            raise AssertionError(
                "ground_truth path present in builder-visible sequence_manifest; "
                "tracker must never see sealed GT paths"
            )

    state = TrackerState()
    prev_image: np.ndarray | None = None
    track_history: list[VisualTrack] = []
    per_frame: list[dict[str, Any]] = []
    # PROPOSAL_FUSION, not FUSED: the two are different paths and the names do not
    # say so. FUSED maps to classical watershed, which returns the table as one
    # 57%-of-frame region and never sees a 36px object. PROPOSAL_FUSION carries
    # the support-surface rejection and objectness ranking.
    cfg = DetectionConfig(
        method=DetectionMethod.PROPOSAL_FUSION, min_area=35, max_regions=20
    )

    for frame_meta in manifest["frames"]:
        fi = int(frame_meta["frame_index"])
        image_path = condition_dir / frame_meta["image"]
        image = cv2.imread(str(image_path), cv2.IMREAD_COLOR)
        if image is None:
            raise RuntimeError(f"failed to read {image_path}")

        detections = detect(
            image, frame_index=fi, config=cfg, previous_image=prev_image
        )
        assert_no_ground_truth_in_detections(detections)
        # Belt-and-braces: scan detection meta recursively.
        _assert_payload_has_no_gt(
            [d.to_dict() for d in detections], context=f"detections frame {fi}"
        )

        state = track(detections, state, frame_index=fi)
        assert_no_ground_truth_on_tracks(state.tracks)
        _assert_payload_has_no_gt(
            [t.to_dict() for t in state.tracks], context=f"tracks frame {fi}"
        )

        track_history.extend(list(state.tracks))
        per_frame.append(
            {
                "frame_index": fi,
                "n_detections": len(detections),
                "tracks": [
                    {
                        "track_id": t.track_id,
                        "state": t.state.value,
                        "centroid_xy": list(t.centroid_xy),
                        "bbox_xywh": list(t.bbox_xywh),
                        "identity_uncertainty": t.identity_uncertainty,
                        "identity_confidence": t.identity_confidence,
                        "appearance_embedding_dim": len(t.appearance_embedding),
                    }
                    for t in state.tracks
                ],
            }
        )
        prev_image = image

    return {
        "condition": manifest.get("condition"),
        "source": manifest.get("source"),
        "n_frames": len(manifest["frames"]),
        "final_tracks": [
            {
                "track_id": t.track_id,
                "state": t.state.value,
                "identity_uncertainty": t.identity_uncertainty,
                "identity_confidence": t.identity_confidence,
                "last_seen_frame": t.last_seen_frame,
                "frames_since_seen": t.frames_since_seen,
                "first_frame": t.first_frame,
            }
            for t in state.tracks
        ],
        "per_frame": per_frame,
        "track_history": track_history,
        "tracker_state": state,
    }


def _assert_payload_has_no_gt(payload: Any, *, context: str) -> None:
    """Recursive scan for GT symbols in tracker-facing structures."""
    banned = {
        "ground_truth_id",
        "gt_id",
        "ground_truth",
        "oracle_id",
        "gt_bbox",
        "bbox_xywh_px",  # sealed GT field name
    }
    if isinstance(payload, dict):
        for k, v in payload.items():
            if k in banned:
                raise AssertionError(f"GT key {k!r} reached tracker ({context})")
            _assert_payload_has_no_gt(v, context=context)
    elif isinstance(payload, list):
        for item in payload:
            _assert_payload_has_no_gt(item, context=context)


# ---------------------------------------------------------------------------
# Sealed evaluator — the only place ground truth is opened.
# ---------------------------------------------------------------------------


def sealed_evaluate(
    condition_dir: Path,
    tracker_result: dict[str, Any],
) -> dict[str, Any]:
    """Score predicted tracks against sealed GT. Never mutates tracker state."""
    sealed_path = condition_dir / "sealed_manifest.json"
    if not sealed_path.is_file():
        return {
            "error": "sealed_manifest.json missing",
            "metrics": {},
            "scored": False,
        }
    sealed = json.loads(sealed_path.read_text(encoding="utf-8"))
    primary_ids: list[str] = list(sealed.get("primary_ids") or ["obj_a", "obj_b", "obj_c"])
    distractor_id = sealed.get("distractor_id", "obj_distractor")
    unknown_id = sealed.get("unknown_id", "obj_unknown")

    state: TrackerState = tracker_result["tracker_state"]
    track_history: list[VisualTrack] = tracker_result["track_history"]

    # Stable GT→track map via first-visible nearest-centroid (evaluator only).
    gt_to_track: dict[str, str] = {}
    frame_assignments: list[dict[str, Any]] = []
    occlusion_frames: list[int] = []
    permanence_log: list[dict[str, Any]] = []
    leave_reid_events: list[dict[str, Any]] = []
    distractor_result: dict[str, Any] = {
        "false_reid": False,
        "depart_track_id": None,
        "distractor_track_id": None,
        "decision": None,
    }
    unknown_result: dict[str, Any] = {
        "absorbed_into_existing": False,
        "unknown_track_id": None,
        "existing_ids_at_entry": [],
        "first_visible_frame": None,
    }
    # Pre-scan sealed GT for the first frame the unknown is visible.
    unknown_first_frame: int | None = None
    for frame_meta in sealed["frames"]:
        gt_path = condition_dir / frame_meta["ground_truth"]
        frame_gt = json.loads(gt_path.read_text(encoding="utf-8"))
        for obj in frame_gt["objects"]:
            if obj["id"] == unknown_id and obj.get("visible"):
                unknown_first_frame = int(frame_meta["frame_index"])
                break
        if unknown_first_frame is not None:
            break
    unknown_result["first_visible_frame"] = unknown_first_frame
    prior_ids_at_entry: set[str] = set()
    if unknown_first_frame is not None:
        for prev in tracker_result["per_frame"]:
            if int(prev["frame_index"]) >= unknown_first_frame:
                break
            for t in prev["tracks"]:
                prior_ids_at_entry.add(t["track_id"])
    unknown_result["existing_ids_at_entry"] = sorted(prior_ids_at_entry)

    # Build a frame→tracks snapshot from per_frame records for assignment.
    tracks_by_frame: dict[int, list[dict[str, Any]]] = {
        int(row["frame_index"]): row["tracks"] for row in tracker_result["per_frame"]
    }

    for frame_meta in sealed["frames"]:
        fi = int(frame_meta["frame_index"])
        gt_path = condition_dir / frame_meta["ground_truth"]
        frame_gt = json.loads(gt_path.read_text(encoding="utf-8"))
        gt_map = {obj["id"]: obj for obj in frame_gt["objects"]}
        live = tracks_by_frame.get(fi, [])

        assignment: dict[str, Any] = {}
        for gid in primary_ids + [distractor_id, unknown_id]:
            obj = gt_map.get(gid)
            if obj is None or not obj.get("visible"):
                assignment[gid] = None
                continue
            ux, uy = obj["image_uv_px"]
            best_tid = None
            best_d = 1e18
            for trk in live:
                if trk["state"] not in {
                    TrackState.ACTIVE.value,
                    TrackState.REAPPEARED.value,
                    TrackState.OCCLUDED.value,
                }:
                    continue
                cx, cy = trk["centroid_xy"]
                d = (cx - ux) ** 2 + (cy - uy) ** 2
                if d < best_d:
                    best_d = d
                    best_tid = trk["track_id"]
            # Generous radius relative to object scale.
            if best_tid is not None and best_d <= 50.0**2:
                assignment[gid] = best_tid
                if gid not in gt_to_track:
                    gt_to_track[gid] = best_tid
            else:
                assignment[gid] = None

        # Count unmatched active tracks as false positives for this frame.
        assigned_tids = {v for v in assignment.values() if v is not None}
        n_active = sum(
            1
            for t in live
            if t["state"] in {TrackState.ACTIVE.value, TrackState.REAPPEARED.value}
        )
        assignment["_false_positives"] = max(0, n_active - len(assigned_tids))

        # Occlusion survival for obj_b when GT says not visible mid-sequence.
        obj_b = gt_map.get("obj_b", {})
        if obj_b and not obj_b.get("visible") and 8 <= fi <= 24:
            tid = gt_to_track.get("obj_b") or assignment.get("obj_b")
            if tid:
                trk = next((t for t in state.tracks if t.track_id == tid), None)
                # Prefer history at this frame.
                hist = [
                    t
                    for t in track_history
                    if t.track_id == tid and t.frame_index == fi
                ]
                if hist:
                    trk = hist[-1]
                if trk is not None:
                    occlusion_frames.append(fi)
                    permanence_log.append(
                        {
                            "frame": fi,
                            "track_id": tid,
                            "state": trk.state.value
                            if hasattr(trk, "state")
                            else trk["state"],  # type: ignore[index]
                            "identity_uncertainty": (
                                trk.identity_uncertainty
                                if hasattr(trk, "identity_uncertainty")
                                else trk["identity_uncertainty"]  # type: ignore[index]
                            ),
                        }
                    )

        # Leave/return for obj_c (or obj_b on leave_return condition).
        leave_gid = "obj_c"
        if sealed.get("condition") == "leave_return":
            leave_gid = "obj_b"
        leave = gt_map.get(leave_gid, {})
        if leave and leave.get("visible") and fi >= 22:
            tid_expected = gt_to_track.get(leave_gid)
            tid_now = assignment.get(leave_gid)
            if tid_expected and tid_now == tid_expected:
                assignment["_reid_tp"] = 1
                leave_reid_events.append(
                    {"frame": fi, "result": "tp", "track_id": tid_now}
                )
            elif tid_now and tid_expected and tid_now != tid_expected:
                assignment["_reid_fp"] = 1
                leave_reid_events.append(
                    {
                        "frame": fi,
                        "result": "fp",
                        "track_id": tid_now,
                        "expected": tid_expected,
                    }
                )
            elif tid_expected and not tid_now:
                assignment["_reid_fn"] = 1
                leave_reid_events.append({"frame": fi, "result": "fn"})

        # Distractor must not share the departed object's track id.
        distractor = gt_map.get(distractor_id, {})
        if distractor and distractor.get("visible") and fi >= 16:
            # Departed id depends on condition.
            depart_gid = "obj_c" if sealed.get("condition") == "permanence" else "obj_b"
            depart_tid = gt_to_track.get(depart_gid)
            dist_tid = assignment.get(distractor_id)
            distractor_result["depart_track_id"] = depart_tid
            distractor_result["distractor_track_id"] = dist_tid
            if depart_tid and dist_tid and depart_tid == dist_tid:
                distractor_result["false_reid"] = True

            # Explicit reidentify API check against a LOST view of the departed id.
            if depart_tid and dist_tid:
                live_trk = next((t for t in state.tracks if t.track_id == depart_tid), None)
                dist_trk = next((t for t in state.tracks if t.track_id == dist_tid), None)
                if live_trk is not None and dist_trk is not None:
                    from blender_vision.v2.authority import AuthorityClass as _AC

                    lost_view = VisualTrack(
                        id=f"{depart_tid}-lost-view",
                        track_id=depart_tid,
                        kind=live_trk.kind,
                        state=TrackState.LOST,
                        frame_index=fi,
                        first_frame=live_trk.first_frame,
                        last_seen_frame=live_trk.last_seen_frame,
                        frames_since_seen=max(1, live_trk.frames_since_seen),
                        bbox_xywh=live_trk.bbox_xywh,
                        centroid_xy=live_trk.centroid_xy,
                        predicted_xy=live_trk.predicted_xy,
                        appearance_hist=list(live_trk.appearance_hist),
                        appearance_embedding=list(live_trk.appearance_embedding),
                        identity_uncertainty=live_trk.identity_uncertainty,
                        authority=_AC.INFERRED,
                    ).seal()
                    # Build a synthetic detection from the distractor's observed state.
                    det = Detection(
                        detection_id=f"eval-dist-{fi}",
                        kind=dist_trk.kind,
                        bbox_xywh=dist_trk.bbox_xywh,
                        centroid_xy=dist_trk.centroid_xy,
                        appearance_hist=list(dist_trk.appearance_hist),
                        appearance_embedding=list(dist_trk.appearance_embedding),
                        area_px=dist_trk.bbox_xywh[2] * dist_trk.bbox_xywh[3],
                        frame_index=fi,
                    )
                    decision = reidentify(
                        det, [lost_view], min_score=REID_THRESHOLD_LOST
                    )
                    distractor_result["decision"] = decision.to_dict()
                    if decision.matched and decision.track_id == depart_tid:
                        distractor_result["false_reid"] = True

        # Unknown entering must receive a new identity, not absorb into existing.
        # Definition: the track assigned on the unknown's first visible frame
        # already existed before that frame, or shares a track with a visible
        # primary in the same frame.
        unknown = gt_map.get(unknown_id, {})
        if unknown and unknown.get("visible") and fi >= 18:
            unk_tid = assignment.get(unknown_id)
            if unknown_result["unknown_track_id"] is None and unk_tid is not None:
                unknown_result["unknown_track_id"] = unk_tid
            # Same-frame collision: unknown and a visible primary share a track.
            for gid in primary_ids:
                if assignment.get(gid) is not None and assignment.get(gid) == unk_tid:
                    unknown_result["absorbed_into_existing"] = True
            if (
                fi == unknown_first_frame
                and unk_tid is not None
                and unk_tid in prior_ids_at_entry
            ):
                unknown_result["absorbed_into_existing"] = True

        frame_assignments.append(assignment)

    metrics = track_metrics(frame_assignments, primary_ids)
    occlude_tid = gt_to_track.get("obj_b", "")
    survival = occlusion_survival_rate(
        track_history, expected_track_id=occlude_tid, occlusion_frames=occlusion_frames
    )

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

    leave_return_ok = any(e.get("result") == "tp" for e in leave_reid_events)

    return {
        "scored": True,
        "gt_to_track": gt_to_track,
        "metrics": metrics,
        "confusion_primary_ids": metrics.get("confusion", {}),
        "occlusion_survival_rate": survival,
        "occlusion_frames": occlusion_frames,
        "permanence_log": permanence_log,
        "uncertainty_monotone_during_occlusion": unc_monotone,
        "uncertainty_grew_during_occlusion": unc_grew,
        "uncertainty_series_occluded": unc_series,
        "leave_return_same_id": leave_return_ok,
        "leave_reid_events": leave_reid_events,
        "distractor": distractor_result,
        "unknown": unknown_result,
    }


def run_hard_suite(hard_out: Path) -> dict[str, Any]:
    """Track + sealed-evaluate every condition under hard_out."""
    results: dict[str, Any] = {"conditions": {}, "permanence": None}
    for name in HARD_CONDITIONS:
        cond_dir = hard_out / name
        if not (cond_dir / "sequence_manifest.json").is_file():
            results["conditions"][name] = {"error": "missing sequence_manifest.json"}
            continue
        print(f"  tracking condition={name} ...")
        tracker_result = run_tracker_on_condition(cond_dir)
        # Drop non-serialisable objects before scoring uses them.
        eval_result = sealed_evaluate(cond_dir, tracker_result)
        serialisable = {
            "condition": name,
            "source": tracker_result.get("source"),
            "n_frames": tracker_result.get("n_frames"),
            "n_final_tracks": len(tracker_result.get("final_tracks", [])),
            "final_tracks": tracker_result.get("final_tracks"),
            "evaluation": {
                k: v
                for k, v in eval_result.items()
                if k != "permanence_log" or name == "permanence"
            },
        }
        # Always keep permanence log for the permanence condition.
        if name == "permanence":
            serialisable["evaluation"]["permanence_log"] = eval_result.get(
                "permanence_log", []
            )
            serialisable["evaluation"]["uncertainty_series_occluded"] = eval_result.get(
                "uncertainty_series_occluded", []
            )
            results["permanence"] = serialisable
        results["conditions"][name] = serialisable
    return results


def _print_condition_table(suite: dict[str, Any]) -> None:
    print("--- Per-condition metrics (honest; expect regression vs GT-seeded) ---")
    header = (
        f"{'condition':<24} {'IDS':>4} {'MOTA':>7} {'IDF1':>7} "
        f"{'P':>6} {'R':>6} {'frag':>5} {'surv':>6}"
    )
    print(header)
    print("-" * len(header))
    for name in HARD_CONDITIONS:
        row = suite["conditions"].get(name, {})
        if "error" in row:
            print(f"{name:<24} ERROR: {row['error']}")
            continue
        ev = row.get("evaluation", {})
        m = ev.get("metrics") or {}
        surv = ev.get("occlusion_survival_rate")
        surv_s = f"{surv:.3f}" if isinstance(surv, float) else "  n/a"
        print(
            f"{name:<24} "
            f"{m.get('id_switches', -1):>4} "
            f"{m.get('mota', float('nan')):>7.3f} "
            f"{m.get('idf1', float('nan')):>7.3f} "
            f"{m.get('precision', float('nan')):>6.3f} "
            f"{m.get('recall', float('nan')):>6.3f} "
            f"{m.get('track_fragmentation_total', -1):>5} "
            f"{surv_s:>6}"
        )


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
        help="reuse an existing hard/ tree under --output",
    )
    parser.add_argument(
        "--diagnostic-only",
        action="store_true",
        help="force OpenCV synthetic suite (skip Blender attempt)",
    )
    args = parser.parse_args(argv)
    output: Path = args.output
    output.mkdir(parents=True, exist_ok=True)

    registry = default_registry()
    intake = registry.intake_report()
    (output / "model_intake.json").write_text(
        json.dumps(intake, indent=2), encoding="utf-8"
    )
    print("=== Model intake (REVIEW_PENDING; nothing downloaded) ===")
    print(f"  entries: {len(intake['entries'])}")
    print(f"  physical_candidates: {intake['physical_candidates']}")
    print(f"  families: {intake['families_covered']}")

    substitute: RuntimeAttestation | None = None
    hard_out = output / "hard"
    if args.diagnostic_only:
        print("=== Diagnostic synthetic hard suite (no Blender attempt) ===")
        _write_diagnostic_hard(hard_out)
        attestation = attest_substitute(
            "blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason="--diagnostic-only: synthetic OpenCV hard suite",
            substitute="synthetic_hard_sequence",
            outputs={"marker": hard_out / "OCULAR_HARD_COMPLETE"},
        )
    elif args.skip_render and (hard_out / "OCULAR_HARD_COMPLETE").is_file():
        attestation = RuntimeAttestation(
            id="attest-blender-skipped-reuse",
            runtime="blender",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            blocked_reason="--skip-render reused existing frames",
            substituted_by="cached-frames",
        ).seal()
    else:
        print("=== Blender render hard suite (physical attempt) ===")
        attestation, hard_out, substitute = render_hard_suite(output)

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

    if not (hard_out / "OCULAR_HARD_COMPLETE").is_file():
        print("ERROR: OCULAR_HARD_COMPLETE missing; cannot track.")
        if attestation.stderr_tail:
            print(attestation.stderr_tail[-2000:])
        return 2

    if attestation.is_physical:
        try:
            attestation.require_physical("ocular hard suite render")
            print("  physical claim: PASS (RuntimeAttestation.require_physical)")
        except Exception as exc:  # noqa: BLE001
            print(f"  physical claim refused: {exc}")
    else:
        print(
            "  physical claim: NOT asserted "
            f"(execution_class={attestation.execution_class.value}; "
            "no fallback may emit a physical PASS)"
        )

    print("=== Detect + track (no ground truth) over hard conditions ===")
    try:
        suite = run_hard_suite(hard_out)
    except AssertionError as exc:
        print(f"=== FAIL (GT leakage or contract violation) ===\n  {exc}")
        return 1

    # Strip non-JSON objects.
    suite_json = json.loads(
        json.dumps(
            suite,
            default=lambda o: None,
        )
    )
    (output / "tracking_report.json").write_text(
        json.dumps(suite_json, indent=2), encoding="utf-8"
    )

    _print_condition_table(suite)

    # Permanence deep-dive.
    perm = suite.get("permanence") or suite["conditions"].get("permanence", {})
    ev = perm.get("evaluation") or {}
    print("=== Permanence sequence ===")
    print(f"  leave-return same id:   {ev.get('leave_return_same_id')}")
    print(
        f"  uncertainty monotone:   {ev.get('uncertainty_monotone_during_occlusion')} "
        f"(grew={ev.get('uncertainty_grew_during_occlusion')})"
    )
    series = ev.get("uncertainty_series_occluded") or []
    if series:
        print(f"  uncertainty trajectory: {[round(u, 3) for u in series]}")
    print("  permanence samples (occluded object):")
    for row in (ev.get("permanence_log") or [])[:10]:
        print(
            f"    frame={row['frame']} state={row['state']} "
            f"u={row['identity_uncertainty']:.4f}"
        )
    dist = ev.get("distractor") or {}
    print("--- Distractor refusal ---")
    print(f"  depart_track_id:     {dist.get('depart_track_id')}")
    print(f"  distractor_track_id: {dist.get('distractor_track_id')}")
    print(f"  false_reid:          {dist.get('false_reid')}")
    if dist.get("decision"):
        print(f"  reidentify():        {dist['decision']}")
    unk = ev.get("unknown") or {}
    print("--- Unknown entrant ---")
    print(f"  unknown_track_id:         {unk.get('unknown_track_id')}")
    print(f"  absorbed_into_existing:   {unk.get('absorbed_into_existing')}")

    # Aggregate failure gates.
    failures: list[str] = []
    if dist.get("false_reid"):
        failures.append(
            "distractor was falsely re-identified as the departed original"
        )
    if unk.get("absorbed_into_existing"):
        failures.append(
            "unknown entering object was absorbed into an existing identity"
        )

    # ID-switch gate on permanence + crossing (hardest).
    for gate_name in ("permanence", "crossing_paths"):
        row = suite["conditions"].get(gate_name, {})
        m = (row.get("evaluation") or {}).get("metrics") or {}
        ids = m.get("id_switches", 0)
        if ids > ID_SWITCH_THRESHOLD:
            failures.append(
                f"{gate_name}: ID switches {ids} exceed threshold {ID_SWITCH_THRESHOLD}"
            )

    surv = ev.get("occlusion_survival_rate")
    if (
        isinstance(surv, float)
        and surv < MIN_OCCLUSION_SURVIVAL
        and (ev.get("occlusion_frames") or [])
    ):
        failures.append(
            f"occlusion survival {surv:.3f} < {MIN_OCCLUSION_SURVIVAL}"
        )

    print("--- Honest regression note ---")
    print(
        "  Metrics above are perception-driven (no GT-seeded boxes). "
        "Expect worse numbers than the historical GT-seeded baseline; "
        "that is the point. Do not retune fixtures to flatter the tracker."
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
