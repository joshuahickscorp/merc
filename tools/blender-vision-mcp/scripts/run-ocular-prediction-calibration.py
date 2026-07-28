#!/usr/bin/env python3
"""Calibrate prediction + surprise against sealed hard-condition labels.

Pipeline (perception path only):
  detect → track → world update → predict_next → evaluate_observations

Scoring reads sealed GT **only** inside `sealed_evaluate`. The predictor never
sees ground truth. Thresholds are not tuned on sealed labels — they come from
track kinematics / uncertainty envelopes inside `predict.py`.

Usage:
  PYTHONPATH=src .venv/bin/python scripts/run-ocular-prediction-calibration.py
  PYTHONPATH=src .venv/bin/python scripts/run-ocular-prediction-calibration.py \\
      --hard-root artifacts/ocular/tracking/hard

Writes artifacts/ocular/prediction-calibration.json.
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any

import cv2
import numpy as np

REPO = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(REPO / "src"))

from blender_vision.ocular.detect import (  # noqa: E402
    DetectionConfig,
    DetectionMethod,
    assert_no_ground_truth_in_detections,
    detect,
)
from blender_vision.ocular.predict import (  # noqa: E402
    Prediction,
    PredictionKind,
    SurpriseKind,
    evaluate_observations,
    predict_next,
)
from blender_vision.ocular.track import (  # noqa: E402
    TrackerState,
    TrackState,
    assert_no_ground_truth_on_tracks,
    track,
)
from blender_vision.ocular.world import (  # noqa: E402
    build_world_model,
    update_world_model,
)

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

PREDICTION_KINDS = (
    PredictionKind.POSE.value,
    PredictionKind.VISIBILITY.value,
    PredictionKind.REAPPEARANCE.value,
    PredictionKind.CAMERA_RESULT.value,
    PredictionKind.FRAME_REGION.value,
)

SURPRISE_KINDS = (
    SurpriseKind.MISSING_EXPECTED_OBJECT.value,
    SurpriseKind.UNEXPECTED_OBJECT.value,
    SurpriseKind.WRONG_MOTION.value,
    SurpriseKind.WRONG_REAPPEARANCE.value,
    SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
)

DEFAULT_HARD_ROOT = REPO / "artifacts" / "ocular" / "tracking" / "hard"
DEFAULT_OUTPUT = REPO / "artifacts" / "ocular" / "prediction-calibration.json"

# Association radius (px) for track ↔ GT matching in the sealed evaluator only.
_ASSIGN_RADIUS_PX = 50.0
# Frame proximity for matching a fired surprise to a sealed true event.
_SURPRISE_FRAME_TOL = 2
# Pixel envelope for "GT constant-velocity would have been surprised".
# Derived from object scale on the fixtures (36px side → 0.5 * scale ≈ 18).
_GT_MOTION_SURPRISE_SCALE = 0.5


# ---------------------------------------------------------------------------
# Perception path — pixels only. Must never open sealed_gt.
# ---------------------------------------------------------------------------


def _pose_from_centroid(centroid: tuple[float, float], resolution: list[int]) -> list[float]:
    """Map image centroid to a unitless metric proxy (not ground truth)."""
    w = max(resolution[0], 1)
    h = max(resolution[1], 1)
    x = (float(centroid[0]) / w) - 0.5
    y = (float(centroid[1]) / h) - 0.5
    return [float(x), float(y), 0.5, 1.0, 0.0, 0.0, 0.0]


def _assert_no_gt(payload: Any, *, context: str) -> None:
    banned = {
        "ground_truth_id",
        "gt_id",
        "ground_truth",
        "oracle_id",
        "gt_bbox",
        "bbox_xywh_px",
        "world_xyz_m",
        "image_uv_px",
    }
    if isinstance(payload, dict):
        for k, v in payload.items():
            if k in banned:
                raise AssertionError(f"GT key {k!r} reached predictor ({context})")
            _assert_no_gt(v, context=context)
    elif isinstance(payload, list):
        for item in payload:
            _assert_no_gt(item, context=context)


def run_perception_predict(condition_dir: Path) -> dict[str, Any]:
    """detect → track → world → predict over builder-visible frames only."""
    if "sealed_gt" in str(condition_dir):
        raise RuntimeError("perception path must not receive a sealed_gt path")

    manifest_path = condition_dir / "sequence_manifest.json"
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    for frame_meta in manifest["frames"]:
        if "ground_truth" in frame_meta:
            raise AssertionError(
                "ground_truth path present in builder-visible sequence_manifest"
            )

    resolution = list(manifest.get("resolution") or [320, 240])
    cfg = DetectionConfig(
        method=DetectionMethod.PROPOSAL_FUSION, min_area=35, max_regions=20
    )
    state = TrackerState()
    prev_image: np.ndarray | None = None
    world = None
    per_frame: list[dict[str, Any]] = []
    all_predictions: list[dict[str, Any]] = []
    all_surprises: list[dict[str, Any]] = []
    pending_predictions: list[Prediction] = []

    for frame_meta in manifest["frames"]:
        fi = int(frame_meta["frame_index"])
        image_path = condition_dir / frame_meta["image"]
        image = cv2.imread(str(image_path), cv2.IMREAD_COLOR)
        if image is None:
            raise RuntimeError(f"failed to read {image_path}")
        h, w = image.shape[:2]
        resolution = [w, h]

        detections = detect(
            image, frame_index=fi, config=cfg, previous_image=prev_image
        )
        assert_no_ground_truth_in_detections(detections)
        state = track(detections, state, frame_index=fi)
        assert_no_ground_truth_on_tracks(state.tracks)

        live_tracks = [
            t
            for t in state.tracks
            if t.state in {TrackState.ACTIVE, TrackState.REAPPEARED}
            and t.frames_since_seen == 0
        ]
        entities: list[dict[str, Any]] = []
        for trk in live_tracks:
            cx, cy = trk.centroid_xy
            entities.append(
                {
                    "entity_id": trk.track_id,
                    "track_id": trk.track_id,
                    "class_label": "segment",
                    "pose_m": _pose_from_centroid(trk.centroid_xy, resolution),
                    "visible": True,
                    "image_xy": [float(cx), float(cy)],
                    "bbox_xywh": list(trk.bbox_xywh),
                    "centroid_xy": [float(cx), float(cy)],
                    "appearance": {
                        "centroid_xy": [float(cx), float(cy)],
                        "bbox_xywh": list(trk.bbox_xywh),
                        "area_px": float(trk.bbox_xywh[2] * trk.bbox_xywh[3]),
                        "hist_digest": f"{sum(trk.appearance_hist):.6f}",
                    },
                    "confidence": float(trk.identity_confidence),
                }
            )
        _assert_no_gt(entities, context=f"entities frame {fi}")

        observation = {
            "frame_index": fi,
            "track_source": "perception",
            "lighting": {"mean_luminance": float(np.mean(image) / 255.0)},
            "entities": entities,
        }

        # Evaluate predictions from the previous frame against this observation.
        frame_surprises: list[dict[str, Any]] = []
        if world is not None and pending_predictions:
            obs_for_eval = {
                "frame_index": fi,
                "mean_luminance": observation["lighting"]["mean_luminance"],
                "entities": [
                    {
                        "entity_id": e["entity_id"],
                        "pose_m": e["pose_m"],
                        "visible": True,
                        "centroid_xy": e["centroid_xy"],
                        "image_xy": e["image_xy"],
                        "bbox_xywh": e["bbox_xywh"],
                        "appearance": e["appearance"],
                    }
                    for e in entities
                ],
            }
            fired = evaluate_observations(
                world, pending_predictions, obs_for_eval, update_uncertainty=True
            )
            for event in fired:
                row = event.to_dict()
                frame_surprises.append(row)
                all_surprises.append(row)

        if world is None:
            world = build_world_model(
                [observation],
                scene_id=f"hard-{manifest.get('condition', condition_dir.name)}",
                session_id="prediction-calibration",
            )
        else:
            world = update_world_model(world, observation)

        # Predict next frame from the updated world (perception tracks only).
        preds = predict_next(world, horizon=1, include_frame_features=False)
        pred_rows = [p.to_dict() for p in preds]
        for row in pred_rows:
            row["_source_frame"] = fi
            all_predictions.append(row)
        pending_predictions = preds

        per_frame.append(
            {
                "frame_index": fi,
                "n_detections": len(detections),
                "n_live_tracks": len(live_tracks),
                "tracks": [
                    {
                        "track_id": t.track_id,
                        "state": t.state.value,
                        "centroid_xy": list(t.centroid_xy),
                        "bbox_xywh": list(t.bbox_xywh),
                        "identity_confidence": t.identity_confidence,
                    }
                    for t in state.tracks
                ],
                "n_predictions": len(preds),
                "prediction_kinds": sorted({p.kind for p in preds}),
                "n_surprises": len(frame_surprises),
                "surprise_kinds": sorted(
                    {
                        s.get("surprise_kind") or s.get("kind")
                        for s in frame_surprises
                        if s.get("fired", True)
                    }
                ),
            }
        )
        prev_image = image

    return {
        "condition": manifest.get("condition", condition_dir.name),
        "source": manifest.get("source"),
        "n_frames": len(manifest["frames"]),
        "resolution": resolution,
        "per_frame": per_frame,
        "predictions": all_predictions,
        "surprises": all_surprises,
        "n_predictions": len(all_predictions),
        "n_surprises": len(all_surprises),
        # Track snapshots for sealed assignment (no GT keys).
        "tracks_by_frame": {
            int(row["frame_index"]): row["tracks"] for row in per_frame
        },
    }


# ---------------------------------------------------------------------------
# Sealed evaluator — the only place ground truth is opened.
# ---------------------------------------------------------------------------


def _percentile(values: list[float], p: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return float(ordered[0])
    k = (len(ordered) - 1) * (p / 100.0)
    f = math.floor(k)
    c = math.ceil(k)
    if f == c:
        return float(ordered[int(k)])
    return float(ordered[f] * (c - k) + ordered[c] * (k - f))


def _read_sealed_frame(condition_dir: Path, frame_meta: dict[str, Any]) -> dict[str, Any]:
    gt_path = condition_dir / frame_meta["ground_truth"]
    return json.loads(gt_path.read_text(encoding="utf-8"))


def _assign_tracks_to_gt(
    live_tracks: list[dict[str, Any]],
    gt_objects: list[dict[str, Any]],
    *,
    radius_px: float = _ASSIGN_RADIUS_PX,
) -> dict[str, str | None]:
    """Nearest-centroid assignment of visible GT ids → track ids (evaluator only)."""
    assignment: dict[str, str | None] = {}
    used: set[str] = set()
    visible = [o for o in gt_objects if o.get("visible")]
    # Greedy by distance — fine for ≤ a few primaries.
    candidates: list[tuple[float, str, str]] = []
    for obj in visible:
        ux, uy = obj["image_uv_px"]
        gid = str(obj["id"])
        for trk in live_tracks:
            if trk["state"] not in {
                TrackState.ACTIVE.value,
                TrackState.REAPPEARED.value,
                TrackState.OCCLUDED.value,
            }:
                continue
            cx, cy = trk["centroid_xy"]
            d = math.hypot(float(cx) - float(ux), float(cy) - float(uy))
            candidates.append((d, gid, trk["track_id"]))
    candidates.sort()
    for d, gid, tid in candidates:
        if gid in assignment:
            continue
        if tid in used:
            continue
        if d > radius_px:
            continue
        assignment[gid] = tid
        used.add(tid)
    for obj in gt_objects:
        assignment.setdefault(str(obj["id"]), None)
    return assignment


def _gt_velocity(
    prev: dict[str, Any] | None, cur: dict[str, Any]
) -> tuple[float, float]:
    if prev is None:
        return 0.0, 0.0
    ax, ay = prev["image_uv_px"]
    bx, by = cur["image_uv_px"]
    fa = int(prev.get("frame_index", 0))
    fb = int(cur.get("frame_index", 1))
    # frame_index on object may be absent; caller passes frame-level.
    dt = 1.0
    return (float(bx) - float(ax)) / dt, (float(by) - float(ay)) / dt


def _derive_sealed_events(
    sealed: dict[str, Any],
    condition_dir: Path,
) -> list[dict[str, Any]]:
    """Derive true surprise events from sealed GT kinematics only.

    These are the sealed labels the evaluator scores against — never fed to the
    predictor. Definitions are kinematic / visibility flips, not tuned thresholds
    against detector output.
    """
    primary_ids: list[str] = list(sealed.get("primary_ids") or ["obj_a", "obj_b", "obj_c"])
    unknown_id = sealed.get("unknown_id", "obj_unknown")
    distractor_id = sealed.get("distractor_id", "obj_distractor")
    condition = str(sealed.get("condition") or condition_dir.name)

    # Load full GT trajectories.
    frames: list[tuple[int, dict[str, dict[str, Any]]]] = []
    for frame_meta in sealed["frames"]:
        fi = int(frame_meta["frame_index"])
        gt = _read_sealed_frame(condition_dir, frame_meta)
        by_id = {str(o["id"]): o for o in gt["objects"]}
        frames.append((fi, by_id))

    events: list[dict[str, Any]] = []
    # Per-object last visible sample for CV extrapolation.
    last_visible: dict[str, tuple[int, dict[str, Any]]] = {}
    prev_by_id: dict[str, dict[str, Any]] | None = None
    prev_fi: int | None = None

    for fi, by_id in frames:
        # Velocities of currently visible primaries.
        velocities: dict[str, tuple[float, float]] = {}
        for gid in primary_ids:
            obj = by_id.get(gid)
            if obj is None or not obj.get("visible"):
                continue
            if prev_by_id and gid in prev_by_id and prev_by_id[gid].get("visible"):
                ax, ay = prev_by_id[gid]["image_uv_px"]
                bx, by = obj["image_uv_px"]
                dt = max(1, fi - (prev_fi if prev_fi is not None else fi - 1))
                velocities[gid] = ((bx - ax) / dt, (by - ay) / dt)

        # Global (camera) median velocity among visible primaries.
        if velocities:
            gvx = float(statistics.median([v[0] for v in velocities.values()]))
            gvy = float(statistics.median([v[1] for v in velocities.values()]))
        else:
            gvx, gvy = 0.0, 0.0
        g_norm = math.hypot(gvx, gvy)

        # Object scale for motion-surprise envelope (from bbox).
        scales = []
        for gid in primary_ids:
            obj = by_id.get(gid)
            if obj is None or not obj.get("visible"):
                continue
            bb = obj.get("bbox_xywh_px") or [0, 0, 36, 36]
            scales.append(max(float(bb[2]), float(bb[3])))
        scale = float(statistics.median(scales)) if scales else 36.0
        motion_tol = _GT_MOTION_SURPRISE_SCALE * scale

        coherent = False
        if len(velocities) >= 2 and g_norm > motion_tol * 0.25:
            n_coh = 0
            for vx, vy in velocities.values():
                if math.hypot(vx - gvx, vy - gvy) <= max(motion_tol, 0.5 * g_norm):
                    n_coh += 1
            coherent = (n_coh / len(velocities)) >= 0.6

        for gid in primary_ids + [unknown_id, distractor_id]:
            obj = by_id.get(gid)
            if obj is None:
                continue
            visible = bool(obj.get("visible"))
            prev = prev_by_id.get(gid) if prev_by_id else None
            prev_vis = bool(prev.get("visible")) if prev else False

            # missing_expected_object: was visible, now not, and still in camera view
            # flag when provided (or simply a disappear mid-sequence).
            if prev_vis and not visible and prev_fi is not None:
                in_view = obj.get("in_camera_view", True)
                if in_view:
                    events.append(
                        {
                            "kind": SurpriseKind.MISSING_EXPECTED_OBJECT.value,
                            "frame_index": fi,
                            "entity_gt_id": gid,
                            "uv": list(obj.get("image_uv_px") or [0, 0]),
                            "condition": condition,
                        }
                    )

            # unexpected_object: unknown / distractor becomes visible.
            if visible and not prev_vis and gid in {unknown_id, distractor_id}:
                events.append(
                    {
                        "kind": SurpriseKind.UNEXPECTED_OBJECT.value,
                        "frame_index": fi,
                        "entity_gt_id": gid,
                        "uv": list(obj["image_uv_px"]),
                        "condition": condition,
                    }
                )
            # Also brand-new primary that was never visible before (rare).
            if visible and not prev_vis and gid in primary_ids and gid not in last_visible:
                if fi > 0:
                    events.append(
                        {
                            "kind": SurpriseKind.UNEXPECTED_OBJECT.value,
                            "frame_index": fi,
                            "entity_gt_id": gid,
                            "uv": list(obj["image_uv_px"]),
                            "condition": condition,
                        }
                    )

            # wrong_motion: CV from last two GT samples fails on this frame.
            if visible and prev_vis and prev is not None and prev_fi is not None:
                ax, ay = prev["image_uv_px"]
                dt = max(1, fi - prev_fi)
                # Prefer two-step history for velocity.
                if gid in last_visible and last_visible[gid][0] < prev_fi:
                    older_fi, older = last_visible[gid]
                    # Use prev as the most recent; velocity from older→prev.
                    oax, oay = older["image_uv_px"]
                    dt_v = max(1, prev_fi - older_fi)
                    vx = (ax - oax) / dt_v
                    vy = (ay - oay) / dt_v
                else:
                    vx, vy = 0.0, 0.0
                pred_x = ax + vx * dt
                pred_y = ay + vy * dt
                bx, by = obj["image_uv_px"]
                err = math.hypot(bx - pred_x, by - pred_y)
                if err > motion_tol and (abs(vx) + abs(vy) > 1e-6 or err > motion_tol * 1.5):
                    events.append(
                        {
                            "kind": SurpriseKind.WRONG_MOTION.value,
                            "frame_index": fi,
                            "entity_gt_id": gid,
                            "uv": [bx, by],
                            "error_px": err,
                            "condition": condition,
                        }
                    )

            # wrong_reappearance: becomes visible after absence, far from CV locus.
            if visible and not prev_vis and gid in last_visible:
                last_fi, last_obj = last_visible[gid]
                gap = fi - last_fi
                if gap >= 2:
                    # Velocity at departure if we have it stored.
                    lx, ly = last_obj["image_uv_px"]
                    # Hold last position (no image velocity across occlusion in GT
                    # derivation) — large jump is wrong reappearance.
                    bx, by = obj["image_uv_px"]
                    err = math.hypot(bx - lx, by - ly)
                    # Expect reappearance near last locus or continuing prior motion;
                    # anything beyond several object scales is wrong place.
                    if err > motion_tol * 2.0:
                        events.append(
                            {
                                "kind": SurpriseKind.WRONG_REAPPEARANCE.value,
                                "frame_index": fi,
                                "entity_gt_id": gid,
                                "uv": [bx, by],
                                "error_px": err,
                                "gap_frames": gap,
                                "condition": condition,
                            }
                        )
                    else:
                        # Reappearance itself is still a sealed event of interest
                        # for recall of reappearance-handling (even if on-locus).
                        # Tag as wrong_reappearance only when wrong; use a soft
                        # "expected reappearance" marker via missing→visible that
                        # is NOT counted as wrong. No event here.
                        pass

            # camera/object contradiction: coherent camera motion but one
            # primary has a large residual (object moved during camera move),
            # or vice versa — independent large motions while scene looks static
            # is not this class.
            if coherent and gid in velocities:
                vx, vy = velocities[gid]
                residual = math.hypot(vx - gvx, vy - gvy)
                if residual > max(motion_tol, 0.5 * g_norm):
                    events.append(
                        {
                            "kind": SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
                            "frame_index": fi,
                            "entity_gt_id": gid,
                            "uv": list(obj["image_uv_px"]),
                            "residual_px_per_frame": residual,
                            "global_px_per_frame": g_norm,
                            "condition": condition,
                        }
                    )

            if visible:
                last_visible[gid] = (fi, obj)

        # Scene-level camera_object: condition is camera_motion and motion is
        # coherent — if primaries move together, a system that attributes that
        # motion to objects is contradictory. Mark one event per such frame.
        if condition == "camera_motion" and coherent and g_norm > motion_tol * 0.25:
            events.append(
                {
                    "kind": SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value,
                    "frame_index": fi,
                    "entity_gt_id": "",
                    "uv": [0.0, 0.0],
                    "global_px_per_frame": g_norm,
                    "condition": condition,
                    "scene_level": True,
                }
            )

        prev_by_id = by_id
        prev_fi = fi

    return events


def sealed_evaluate(
    condition_dir: Path,
    perception_result: dict[str, Any],
) -> dict[str, Any]:
    """Score predictions and surprises against sealed GT. Never mutates inputs."""
    sealed_path = condition_dir / "sealed_manifest.json"
    if not sealed_path.is_file():
        return {"scored": False, "error": "sealed_manifest.json missing"}

    sealed = json.loads(sealed_path.read_text(encoding="utf-8"))
    primary_ids: list[str] = list(sealed.get("primary_ids") or ["obj_a", "obj_b", "obj_c"])
    resolution = list(
        perception_result.get("resolution")
        or sealed.get("resolution")
        or [320, 240]
    )
    w, h = int(resolution[0]), int(resolution[1])

    tracks_by_frame: dict[int, list[dict[str, Any]]] = {
        int(k): v for k, v in perception_result["tracks_by_frame"].items()
    }
    predictions = list(perception_result.get("predictions") or [])
    surprises = [s for s in perception_result.get("surprises") or [] if s.get("fired", True)]

    # Stable GT→track map via first-visible assignment, then frame-wise refresh.
    gt_to_track: dict[str, str] = {}
    frame_assignments: dict[int, dict[str, str | None]] = {}

    for frame_meta in sealed["frames"]:
        fi = int(frame_meta["frame_index"])
        gt = _read_sealed_frame(condition_dir, frame_meta)
        live = tracks_by_frame.get(fi, [])
        assignment = _assign_tracks_to_gt(live, gt["objects"])
        for gid, tid in assignment.items():
            if tid is not None and gid not in gt_to_track:
                gt_to_track[gid] = tid
        frame_assignments[fi] = assignment

    track_to_gt: dict[str, str] = {}
    for gid, tid in gt_to_track.items():
        # Prefer first mapping; collisions keep the first primary.
        track_to_gt.setdefault(tid, gid)

    # ---- Position / visibility prediction errors against sealed GT ----
    position_errors_px: list[float] = []
    position_errors_by_condition: dict[str, list[float]] = defaultdict(list)
    visibility_correct = 0
    visibility_total = 0
    reappearance_errors_px: list[float] = []
    region_errors_px: list[float] = []
    camera_velocity_errors: list[float] = []

    # Index pose / region / reappearance predictions by (source_frame, entity_id).
    pose_preds: dict[tuple[int, str], dict[str, Any]] = {}
    vis_preds: dict[tuple[int, str], dict[str, Any]] = {}
    reapp_preds: dict[tuple[int, str], dict[str, Any]] = {}
    region_preds: dict[tuple[int, str], dict[str, Any]] = {}
    camera_preds: dict[int, dict[str, Any]] = {}

    for pred in predictions:
        kind = pred.get("kind")
        src = int(pred.get("_source_frame", pred.get("valid_from_frame", -1)))
        eid = str(pred.get("entity_id") or "")
        if kind == PredictionKind.POSE.value and eid:
            pose_preds[(src, eid)] = pred
        elif kind == PredictionKind.VISIBILITY.value and eid:
            vis_preds[(src, eid)] = pred
        elif kind == PredictionKind.REAPPEARANCE.value and eid:
            reapp_preds[(src, eid)] = pred
        elif kind == PredictionKind.FRAME_REGION.value and eid:
            region_preds[(src, eid)] = pred
        elif kind in {
            PredictionKind.CAMERA_RESULT.value,
            PredictionKind.CAMERA_PATH.value,
        }:
            camera_preds[src] = pred

    prev_gt_by_id: dict[str, dict[str, Any]] | None = None
    prev_fi: int | None = None

    for frame_meta in sealed["frames"]:
        fi = int(frame_meta["frame_index"])
        gt = _read_sealed_frame(condition_dir, frame_meta)
        by_id = {str(o["id"]): o for o in gt["objects"]}
        assignment = frame_assignments.get(fi, {})
        # Predictions made at fi-1 target frame fi.
        src = fi - 1

        for gid in primary_ids:
            obj = by_id.get(gid)
            if obj is None:
                continue
            tid = assignment.get(gid) or gt_to_track.get(gid)
            if tid is None:
                continue

            # Position error: predicted image_xy vs GT uv.
            pred = pose_preds.get((src, tid))
            if pred is not None and obj.get("visible"):
                expected = pred.get("expected") or {}
                exp_xy = expected.get("image_xy")
                if exp_xy is None and expected.get("pose_m") is not None:
                    # Convert normalised pose proxy back to pixels.
                    pm = expected["pose_m"]
                    exp_xy = [(float(pm[0]) + 0.5) * w, (float(pm[1]) + 0.5) * h]
                if exp_xy is not None:
                    ux, uy = obj["image_uv_px"]
                    err = math.hypot(float(exp_xy[0]) - float(ux), float(exp_xy[1]) - float(uy))
                    position_errors_px.append(err)
                    position_errors_by_condition[
                        str(sealed.get("condition") or condition_dir.name)
                    ].append(err)

            # Visibility accuracy.
            vpred = vis_preds.get((src, tid))
            if vpred is not None:
                exp_vis = bool((vpred.get("expected") or {}).get("visible", True))
                obs_vis = bool(obj.get("visible"))
                visibility_total += 1
                if exp_vis == obs_vis:
                    visibility_correct += 1

            # Reappearance locus error when GT becomes visible after absence.
            rpred = reapp_preds.get((src, tid))
            if rpred is not None and obj.get("visible"):
                expected = rpred.get("expected") or {}
                if expected.get("expect_reappear") or (
                    prev_gt_by_id
                    and gid in prev_gt_by_id
                    and not prev_gt_by_id[gid].get("visible")
                ):
                    exp_xy = expected.get("reappear_image_xy")
                    if exp_xy is None and expected.get("reappear_pose_m") is not None:
                        pm = expected["reappear_pose_m"]
                        exp_xy = [(float(pm[0]) + 0.5) * w, (float(pm[1]) + 0.5) * h]
                    if exp_xy is not None:
                        ux, uy = obj["image_uv_px"]
                        reappearance_errors_px.append(
                            math.hypot(float(exp_xy[0]) - float(ux), float(exp_xy[1]) - float(uy))
                        )

            # Frame region centroid error.
            reg = region_preds.get((src, tid))
            if reg is not None and obj.get("visible"):
                exp_c = (reg.get("expected") or {}).get("centroid_xy")
                if exp_c is not None:
                    ux, uy = obj["image_uv_px"]
                    region_errors_px.append(
                        math.hypot(float(exp_c[0]) - float(ux), float(exp_c[1]) - float(uy))
                    )

        # Camera-result: predicted global velocity vs GT median primary velocity.
        cpred = camera_preds.get(src)
        if cpred is not None and prev_gt_by_id is not None and prev_fi is not None:
            gt_vels: list[tuple[float, float]] = []
            for gid in primary_ids:
                cur = by_id.get(gid)
                prev = prev_gt_by_id.get(gid)
                if cur is None or prev is None:
                    continue
                if not cur.get("visible") or not prev.get("visible"):
                    continue
                ax, ay = prev["image_uv_px"]
                bx, by = cur["image_uv_px"]
                dt = max(1, fi - prev_fi)
                gt_vels.append(((bx - ax) / dt, (by - ay) / dt))
            if gt_vels:
                gvx = float(statistics.median([v[0] for v in gt_vels]))
                gvy = float(statistics.median([v[1] for v in gt_vels]))
                # Predicted camera velocity is in pose-normalised units; convert.
                cam_v = (cpred.get("expected") or {}).get("camera_velocity") or [0, 0, 0]
                pred_vx = float(cam_v[0]) * w
                pred_vy = float(cam_v[1]) * h
                camera_velocity_errors.append(math.hypot(pred_vx - gvx, pred_vy - gvy))

        prev_gt_by_id = by_id
        prev_fi = fi

    # ---- Surprise precision / recall against sealed true events ----
    sealed_events = _derive_sealed_events(sealed, condition_dir)

    # Map fired surprises onto GT entities via track_to_gt when possible.
    fired_by_kind: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for s in surprises:
        sk = s.get("surprise_kind") or ""
        if not sk:
            # Fall back: map prediction kind → surprise class.
            pk = s.get("kind")
            if pk == PredictionKind.POSE.value:
                sk = SurpriseKind.WRONG_MOTION.value
            elif pk == PredictionKind.VISIBILITY.value:
                sk = SurpriseKind.MISSING_EXPECTED_OBJECT.value
            elif pk == PredictionKind.REAPPEARANCE.value:
                sk = SurpriseKind.WRONG_REAPPEARANCE.value
            elif pk in {
                PredictionKind.CAMERA_RESULT.value,
                PredictionKind.CAMERA_PATH.value,
            }:
                sk = SurpriseKind.CAMERA_OBJECT_CONTRADICTION.value
            else:
                sk = str(pk or "unknown")
        row = dict(s)
        row["surprise_kind"] = sk
        row["entity_gt_id"] = track_to_gt.get(str(s.get("entity_id") or ""), "")
        fired_by_kind[sk].append(row)

    sealed_by_kind: dict[str, list[dict[str, Any]]] = defaultdict(list)
    for e in sealed_events:
        sealed_by_kind[e["kind"]].append(e)

    surprise_metrics: dict[str, dict[str, Any]] = {}
    for kind in SURPRISE_KINDS:
        fired = fired_by_kind.get(kind, [])
        truth = sealed_by_kind.get(kind, [])
        matched_truth = [False] * len(truth)
        tp = 0
        for f in fired:
            f_fi = int(f.get("frame_index", -1))
            f_gid = str(f.get("entity_gt_id") or "")
            hit = False
            for i, t in enumerate(truth):
                if matched_truth[i]:
                    continue
                if abs(int(t["frame_index"]) - f_fi) > _SURPRISE_FRAME_TOL:
                    continue
                t_gid = str(t.get("entity_gt_id") or "")
                # Scene-level events match any entity (or empty).
                if t_gid and f_gid and t_gid != f_gid:
                    continue
                matched_truth[i] = True
                hit = True
                break
            if hit:
                tp += 1
        fp = len(fired) - tp
        fn = matched_truth.count(False)
        precision = tp / len(fired) if fired else None
        recall = tp / len(truth) if truth else None
        # FPR: FP / (FP + TN). TN approximated as frames without a true event
        # that also did not fire — use n_frames as denominator basis.
        n_frames = int(perception_result.get("n_frames") or len(sealed["frames"]))
        # Per-frame free rate: frames with a false fire and no true event.
        frames_with_truth = {int(t["frame_index"]) for t in truth}
        frames_with_fp = set()
        for f in fired:
            f_fi = int(f.get("frame_index", -1))
            # If this fire was not matched, count its frame as FP frame.
            # Approximate: if precision path counted it as FP.
            # Recompute match for this single fire.
            matched = False
            for t in truth:
                if abs(int(t["frame_index"]) - f_fi) > _SURPRISE_FRAME_TOL:
                    continue
                t_gid = str(t.get("entity_gt_id") or "")
                f_gid = str(f.get("entity_gt_id") or "")
                if t_gid and f_gid and t_gid != f_gid:
                    continue
                matched = True
                break
            if not matched:
                frames_with_fp.add(f_fi)
        # Negative frames = frames without a true event of this kind.
        negative_frames = n_frames - len(frames_with_truth)
        fp_frames = len(frames_with_fp - frames_with_truth)
        fpr = fp_frames / negative_frames if negative_frames > 0 else None
        surprise_metrics[kind] = {
            "n_fired": len(fired),
            "n_sealed_true": len(truth),
            "tp": tp,
            "fp": fp,
            "fn": fn,
            "precision": precision,
            "recall": recall,
            "false_positive_rate": fpr,
            "correspondence_rate": (tp / len(fired)) if fired else None,
        }

    def _err_summary(vals: list[float]) -> dict[str, Any]:
        if not vals:
            return {"n": 0, "mean_px": None, "p95_px": None, "median_px": None}
        return {
            "n": len(vals),
            "mean_px": float(sum(vals) / len(vals)),
            "p95_px": _percentile(vals, 95),
            "median_px": _percentile(vals, 50),
        }

    vis_acc = (visibility_correct / visibility_total) if visibility_total else None

    return {
        "scored": True,
        "condition": sealed.get("condition") or condition_dir.name,
        "gt_to_track": gt_to_track,
        "n_sealed_events": len(sealed_events),
        "sealed_events_by_kind": {k: len(v) for k, v in sealed_by_kind.items()},
        "prediction_metrics": {
            "position": _err_summary(position_errors_px),
            "visibility_accuracy": vis_acc,
            "visibility_n": visibility_total,
            "visibility_correct": visibility_correct,
            "reappearance_locus": _err_summary(reappearance_errors_px),
            "frame_region": _err_summary(region_errors_px),
            "camera_velocity_error_px_per_frame": _err_summary(camera_velocity_errors),
        },
        "surprise_metrics": surprise_metrics,
        "n_predictions": perception_result.get("n_predictions", 0),
        "n_surprises_fired": len(surprises),
        "surprise_kind_counts": {k: len(v) for k, v in fired_by_kind.items()},
        "prediction_kind_counts": _count_pred_kinds(predictions),
    }


def _count_pred_kinds(predictions: list[dict[str, Any]]) -> dict[str, int]:
    counts: dict[str, int] = defaultdict(int)
    for p in predictions:
        counts[str(p.get("kind"))] += 1
    return dict(counts)


# ---------------------------------------------------------------------------
# Aggregation + CLI
# ---------------------------------------------------------------------------


def _aggregate(condition_results: list[dict[str, Any]]) -> dict[str, Any]:
    pos_means = []
    pos_p95s = []
    pos_ns = 0
    vis_correct = 0
    vis_total = 0
    reapp_vals: list[float] = []
    region_vals: list[float] = []
    cam_vals: list[float] = []

    surprise_agg: dict[str, dict[str, float]] = {
        k: {"tp": 0, "fp": 0, "fn": 0, "n_fired": 0, "n_sealed_true": 0}
        for k in SURPRISE_KINDS
    }
    total_preds = 0
    total_surprises = 0
    kind_counts: dict[str, int] = defaultdict(int)

    for row in condition_results:
        if not row.get("scored"):
            continue
        pm = row["prediction_metrics"]
        pos = pm["position"]
        if pos["n"]:
            pos_means.append(pos["mean_px"])
            pos_p95s.append(pos["p95_px"])
            pos_ns += pos["n"]
        vis_total += int(pm.get("visibility_n") or 0)
        vis_correct += int(pm.get("visibility_correct") or 0)
        # We only have summaries per condition; re-sum n from summaries.
        total_preds += int(row.get("n_predictions") or 0)
        total_surprises += int(row.get("n_surprises_fired") or 0)
        for k, n in (row.get("prediction_kind_counts") or {}).items():
            kind_counts[k] += int(n)
        for sk, sm in (row.get("surprise_metrics") or {}).items():
            if sk not in surprise_agg:
                surprise_agg[sk] = {
                    "tp": 0,
                    "fp": 0,
                    "fn": 0,
                    "n_fired": 0,
                    "n_sealed_true": 0,
                }
            surprise_agg[sk]["tp"] += int(sm.get("tp") or 0)
            surprise_agg[sk]["fp"] += int(sm.get("fp") or 0)
            surprise_agg[sk]["fn"] += int(sm.get("fn") or 0)
            surprise_agg[sk]["n_fired"] += int(sm.get("n_fired") or 0)
            surprise_agg[sk]["n_sealed_true"] += int(sm.get("n_sealed_true") or 0)
        # Carry region/reapp/cam means weighted by n if present.
        for key, bucket in (
            ("reappearance_locus", reapp_vals),
            ("frame_region", region_vals),
            ("camera_velocity_error_px_per_frame", cam_vals),
        ):
            summary = pm.get(key) or {}
            n = int(summary.get("n") or 0)
            mean = summary.get("mean_px")
            if n and mean is not None:
                bucket.extend([float(mean)] * n)

    def _mean(xs: list[float]) -> float | None:
        return float(sum(xs) / len(xs)) if xs else None

    surprise_table: dict[str, dict[str, Any]] = {}
    for sk, agg in surprise_agg.items():
        tp, fp, fn = agg["tp"], agg["fp"], agg["fn"]
        n_fired = agg["n_fired"]
        n_true = agg["n_sealed_true"]
        surprise_table[sk] = {
            "n_fired": n_fired,
            "n_sealed_true": n_true,
            "tp": tp,
            "fp": fp,
            "fn": fn,
            "precision": (tp / n_fired) if n_fired else None,
            "recall": (tp / n_true) if n_true else None,
            "false_positive_rate": (fp / (fp + max(0, n_true - tp + 1))) if (fp or n_true) else None,
            "correspondence_rate": (tp / n_fired) if n_fired else None,
        }

    # Honest overall FP fraction among all fired surprises.
    total_tp = sum(surprise_agg[k]["tp"] for k in surprise_agg)
    total_fp = sum(surprise_agg[k]["fp"] for k in surprise_agg)
    overall_precision = (
        total_tp / (total_tp + total_fp) if (total_tp + total_fp) else None
    )

    return {
        "n_conditions_scored": sum(1 for r in condition_results if r.get("scored")),
        "total_predictions": total_preds,
        "total_surprises_fired": total_surprises,
        "prediction_kind_counts": dict(kind_counts),
        "position_error_px": {
            "n": pos_ns,
            "mean_of_condition_means": _mean(pos_means),
            "mean_of_condition_p95": _mean(pos_p95s),
        },
        "visibility_accuracy": (vis_correct / vis_total) if vis_total else None,
        "visibility_n": vis_total,
        "reappearance_locus_mean_px": _mean(reapp_vals),
        "frame_region_mean_px": _mean(region_vals),
        "camera_velocity_error_mean_px_per_frame": _mean(cam_vals),
        "surprise_by_kind": surprise_table,
        "overall_surprise_precision": overall_precision,
        "overall_surprise_tp": total_tp,
        "overall_surprise_fp": total_fp,
        "signal_or_noise": _judge_signal(overall_precision, total_surprises, total_preds),
    }


def _judge_signal(
    precision: float | None,
    n_surprises: int,
    n_predictions: int,
) -> dict[str, Any]:
    """Plain-language read of the 441/128-style headline numbers."""
    rate = (n_surprises / n_predictions) if n_predictions else None
    if precision is None:
        verdict = "unknown"
        note = "No sealed correspondence available."
    elif precision < 0.25:
        verdict = "noise"
        note = (
            "Most fired surprises do not correspond to sealed true events — "
            "high surprise counts are detector/predictor false positives, not "
            "a genuinely surprising sequence."
        )
    elif precision < 0.5:
        verdict = "mixed"
        note = (
            "Surprises are a mixture of real sealed events and false positives; "
            "headline counts overstate real surprise."
        )
    else:
        verdict = "signal"
        note = (
            "A majority of fired surprises correspond to sealed events; "
            "counts largely reflect real scene dynamics."
        )
    return {
        "verdict": verdict,
        "surprise_precision": precision,
        "surprise_per_prediction": rate,
        "note": note,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--hard-root",
        type=Path,
        default=DEFAULT_HARD_ROOT,
        help="Directory containing the eleven hard conditions",
    )
    parser.add_argument(
        "--output",
        type=Path,
        default=DEFAULT_OUTPUT,
        help="Path for prediction-calibration.json",
    )
    parser.add_argument(
        "--conditions",
        nargs="*",
        default=None,
        help="Optional subset of condition names",
    )
    args = parser.parse_args()

    hard_root: Path = args.hard_root
    if not hard_root.is_dir():
        print(f"FAIL: hard root missing: {hard_root}", file=sys.stderr)
        return 2

    conditions = list(args.conditions) if args.conditions else list(HARD_CONDITIONS)
    condition_results: list[dict[str, Any]] = []

    for name in conditions:
        condition_dir = hard_root / name
        print(f"== {name} ==")
        if not condition_dir.is_dir():
            print(f"  SKIP: missing {condition_dir}")
            condition_results.append(
                {
                    "condition": name,
                    "scored": False,
                    "error": "condition directory missing",
                }
            )
            continue
        try:
            perception = run_perception_predict(condition_dir)
            scored = sealed_evaluate(condition_dir, perception)
            row = {
                "condition": name,
                "n_frames": perception["n_frames"],
                "n_predictions": perception["n_predictions"],
                "n_surprises": perception["n_surprises"],
                "prediction_kind_counts": _count_pred_kinds(perception["predictions"]),
                "surprise_kind_counts": scored.get("surprise_kind_counts", {}),
                **{k: v for k, v in scored.items() if k != "condition"},
            }
            condition_results.append(row)
            pm = scored.get("prediction_metrics") or {}
            pos = pm.get("position") or {}
            print(
                f"  preds={perception['n_predictions']} "
                f"surprises={perception['n_surprises']} "
                f"pos_mean_px={pos.get('mean_px')} "
                f"vis_acc={pm.get('visibility_accuracy')}"
            )
        except Exception as exc:  # noqa: BLE001 — calibration must report all cells
            print(f"  ERROR: {exc}")
            condition_results.append(
                {
                    "condition": name,
                    "scored": False,
                    "error": f"{type(exc).__name__}: {exc}",
                }
            )

    aggregate = _aggregate(condition_results)
    payload = {
        "artifact": "ocular.prediction-calibration",
        "hard_root": str(hard_root),
        "conditions": conditions,
        "prediction_kinds_required": list(PREDICTION_KINDS),
        "surprise_kinds_required": list(SURPRISE_KINDS),
        "notes": {
            "thresholds": (
                "Derived from track uncertainty envelopes and trajectory velocity "
                "variance inside predict.py — not fit to sealed labels."
            ),
            "gt_boundary": (
                "Sealed GT is opened only inside sealed_evaluate / "
                "_derive_sealed_events. The perception path refuses GT keys."
            ),
            "stream_headline": (
                "run-ocular-stream.py --frames 48 previously reported 441 "
                "predictions and 128 surprises with no correctness evidence. "
                "This artifact supplies that evidence on the hard suite."
            ),
        },
        "aggregate": aggregate,
        "per_condition": condition_results,
    }

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print()
    print(f"Wrote {args.output}")
    print(json.dumps(aggregate, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
