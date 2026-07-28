#!/usr/bin/env python3
"""Measure builder-side detector quality against sealed hard-fixture GT.

Builder path (detection): pixels only — no sealed GT is opened inside the
detector. Sealed boxes are loaded only inside evaluator-role functions below
for scoring (IoU ≥ 0.5 match).

Writes artifacts/ocular/detection-quality.json
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path
from typing import Any

import cv2
import numpy as np

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from blender_vision.ocular.detect import (  # noqa: E402
    BackgroundModel,
    DetectionConfig,
    DetectionMethod,
    detect,
    detections_from_proposals,
)


HARD_ROOT = ROOT / "artifacts" / "ocular" / "tracking" / "hard"
OUT_PATH = ROOT / "artifacts" / "ocular" / "detection-quality.json"
IOU_MATCH = 0.5
# Background-blob indicator: detections covering more than this frame-area fraction.
LARGE_AREA_FRAC = 0.25


# ---------------------------------------------------------------------------
# Evaluator-role only — sealed GT. Never imported by the detection path.
# ---------------------------------------------------------------------------


def _load_sealed_gt_boxes(condition_dir: Path, frame_index: int) -> list[tuple[float, float, float, float]]:
    """Evaluator-role: load visible GT boxes for one frame. Not used by detection."""
    sealed = condition_dir / "sealed_gt" / f"frame_{frame_index + 1:04d}.json"
    if not sealed.exists():
        # Some fixtures index by frame_index itself.
        sealed = condition_dir / "sealed_gt" / f"frame_{frame_index:04d}.json"
    if not sealed.exists():
        return []
    data = json.loads(sealed.read_text(encoding="utf-8"))
    boxes: list[tuple[float, float, float, float]] = []
    for obj in data.get("objects") or []:
        if not obj.get("visible", False):
            continue
        bb = obj.get("bbox_xywh_px")
        if not bb or len(bb) != 4:
            continue
        x, y, w, h = (float(bb[0]), float(bb[1]), float(bb[2]), float(bb[3]))
        if w <= 0 or h <= 0:
            continue
        boxes.append((x, y, w, h))
    return boxes


def _bbox_iou(
    a: tuple[float, float, float, float], b: tuple[float, float, float, float]
) -> float:
    ax, ay, aw, ah = a
    bx, by, bw, bh = b
    ax2, ay2 = ax + aw, ay + ah
    bx2, by2 = bx + bw, by + bh
    ix0, iy0 = max(ax, bx), max(ay, by)
    ix1, iy1 = min(ax2, bx2), min(ay2, by2)
    iw, ih = max(0.0, ix1 - ix0), max(0.0, iy1 - iy0)
    inter = iw * ih
    if inter <= 0:
        return 0.0
    union = aw * ah + bw * bh - inter
    return float(inter / union) if union > 0 else 0.0


def score_detections_against_gt(
    det_boxes: list[tuple[float, float, float, float]],
    det_areas: list[float],
    gt_boxes: list[tuple[float, float, float, float]],
    *,
    frame_area: int,
    iou_threshold: float = IOU_MATCH,
) -> dict[str, Any]:
    """Evaluator-role greedy IoU match. Completely separate from detection code."""
    matched_gt: set[int] = set()
    matched_det: set[int] = set()
    ious: list[float] = []
    # Greedy by best IoU pairs.
    pairs: list[tuple[int, int, float]] = []
    for di, db in enumerate(det_boxes):
        for gi, gb in enumerate(gt_boxes):
            iou = _bbox_iou(db, gb)
            if iou >= iou_threshold:
                pairs.append((di, gi, iou))
    pairs.sort(key=lambda t: t[2], reverse=True)
    for di, gi, iou in pairs:
        if di in matched_det or gi in matched_gt:
            continue
        matched_det.add(di)
        matched_gt.add(gi)
        ious.append(iou)

    n_gt = len(gt_boxes)
    n_det = len(det_boxes)
    tp = len(matched_gt)
    recall = tp / n_gt if n_gt else (1.0 if n_det == 0 else 0.0)
    precision = tp / n_det if n_det else (1.0 if n_gt == 0 else 0.0)
    large = sum(1 for a in det_areas if frame_area > 0 and (a / frame_area) > LARGE_AREA_FRAC)
    return {
        "n_gt_visible": n_gt,
        "n_detections": n_det,
        "true_positives": tp,
        "recall": recall,
        "precision": precision,
        "mean_matched_iou": float(np.mean(ious)) if ious else 0.0,
        "n_large_area_dets": large,
    }


# ---------------------------------------------------------------------------
# Builder-side detection (no sealed GT)
# ---------------------------------------------------------------------------


def _list_conditions(hard_root: Path) -> list[Path]:
    return sorted(
        p
        for p in hard_root.iterdir()
        if p.is_dir() and (p / "frames").is_dir() and (p / "sequence_manifest.json").is_file()
    )


def _detect_frame(
    image: np.ndarray,
    *,
    frame_index: int,
    previous_image: np.ndarray | None,
    background_model: BackgroundModel | None,
    method: str,
) -> list[Any]:
    if method == "proposal_fusion":
        return detections_from_proposals(
            image,
            frame_index=frame_index,
            previous_image=previous_image,
            background_model=background_model,
        )
    cfg = DetectionConfig(
        method=DetectionMethod(method),
        min_area=35,
        max_regions=20,
    )
    return detect(
        image,
        frame_index=frame_index,
        config=cfg,
        previous_image=previous_image,
        background_model=background_model,
    )


def measure_condition(condition_dir: Path, *, method: str) -> dict[str, Any]:
    manifest = json.loads((condition_dir / "sequence_manifest.json").read_text(encoding="utf-8"))
    frames_meta = manifest["frames"]
    images: list[np.ndarray] = []
    for fm in frames_meta:
        path = condition_dir / fm["image"]
        img = cv2.imread(str(path), cv2.IMREAD_COLOR)
        if img is None:
            raise RuntimeError(f"failed to read {path}")
        images.append(img)

    bg: BackgroundModel | None = None
    if len(images) >= 3:
        bg = BackgroundModel(images, threshold=18)

    per_frame: list[dict[str, Any]] = []
    sum_recall = 0.0
    sum_precision = 0.0
    sum_dets = 0.0
    sum_iou = 0.0
    sum_large = 0
    sum_gt = 0
    n_scored = 0
    n_frames_with_gt = 0

    for fm, image in zip(frames_meta, images, strict=True):
        fi = int(fm["frame_index"])
        prev = images[fi - 1] if fi > 0 else None
        # Builder path — no GT.
        dets = _detect_frame(
            image,
            frame_index=fi,
            previous_image=prev,
            background_model=bg,
            method=method,
        )
        h, w = image.shape[:2]
        frame_area = h * w
        det_boxes = [tuple(d.bbox_xywh) for d in dets]
        det_areas = [float(d.area_px) for d in dets]

        # Evaluator path — sealed GT only here.
        gt_boxes = _load_sealed_gt_boxes(condition_dir, fi)
        score = score_detections_against_gt(
            det_boxes, det_areas, gt_boxes, frame_area=frame_area
        )
        per_frame.append({"frame_index": fi, **score})
        if score["n_gt_visible"] > 0 or score["n_detections"] > 0:
            n_scored += 1
            sum_recall += score["recall"]
            sum_precision += score["precision"]
            sum_dets += score["n_detections"]
            sum_iou += score["mean_matched_iou"]
            sum_large += score["n_large_area_dets"]
            sum_gt += score["n_gt_visible"]
            if score["n_gt_visible"] > 0:
                n_frames_with_gt += 1

    n = max(1, n_scored)
    return {
        "condition": condition_dir.name,
        "method": method,
        "n_frames": len(frames_meta),
        "n_frames_scored": n_scored,
        "n_frames_with_visible_gt": n_frames_with_gt,
        "detection_recall": sum_recall / n,
        "detection_precision": sum_precision / n,
        "mean_detections_per_frame": sum_dets / n,
        "mean_visible_gt_per_frame": sum_gt / n,
        "mean_matched_iou": sum_iou / n,
        "n_large_area_dets_total": sum_large,
        "mean_large_area_dets_per_frame": sum_large / n,
        "per_frame": per_frame,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--method",
        default="proposal_fusion",
        choices=[
            "proposal_fusion",
            "fused",
            "watershed",
            "background_model",
            "motion_components",
        ],
        help="Builder-side detection method (default: proposal_fusion)",
    )
    parser.add_argument(
        "--out",
        type=Path,
        default=OUT_PATH,
        help="Output JSON path",
    )
    parser.add_argument(
        "--hard-root",
        type=Path,
        default=HARD_ROOT,
    )
    args = parser.parse_args()

    conditions = _list_conditions(args.hard_root)
    if not conditions:
        print(f"no hard conditions under {args.hard_root}", file=sys.stderr)
        return 1

    results = []
    for cond in conditions:
        print(f"measuring {cond.name} ...", flush=True)
        row = measure_condition(cond, method=args.method)
        results.append(row)
        print(
            f"  recall={row['detection_recall']:.3f} "
            f"prec={row['detection_precision']:.3f} "
            f"dets/f={row['mean_detections_per_frame']:.2f} "
            f"gt/f={row['mean_visible_gt_per_frame']:.2f} "
            f"iou={row['mean_matched_iou']:.3f} "
            f"large/f={row['mean_large_area_dets_per_frame']:.2f}",
            flush=True,
        )

    # Aggregate
    def _mean(key: str) -> float:
        return float(np.mean([r[key] for r in results])) if results else 0.0

    report = {
        "method": args.method,
        "iou_match_threshold": IOU_MATCH,
        "large_area_fraction_threshold": LARGE_AREA_FRAC,
        "n_conditions": len(results),
        "aggregate": {
            "detection_recall": _mean("detection_recall"),
            "detection_precision": _mean("detection_precision"),
            "mean_detections_per_frame": _mean("mean_detections_per_frame"),
            "mean_visible_gt_per_frame": _mean("mean_visible_gt_per_frame"),
            "mean_matched_iou": _mean("mean_matched_iou"),
            "mean_large_area_dets_per_frame": _mean("mean_large_area_dets_per_frame"),
        },
        "conditions": [
            {k: v for k, v in r.items() if k != "per_frame"} for r in results
        ],
        "per_condition_frames": {r["condition"]: r["per_frame"] for r in results},
        "notes": [
            "Sealed GT is used only in evaluator-role scoring functions.",
            "Detection path never opens sealed_gt/.",
        ],
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    args.out.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"\nwrote {args.out}")
    print("aggregate:", json.dumps(report["aggregate"], indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
