"""Diagnostic hard-sequence generator (OpenCV) when Blender cannot WM_init.

Produces RGB frames with subtle textures on near-identical albedo so colour
histograms alone cannot solve identity. Ground truth is written under a
``sealed_gt/`` tree that the tracker path must never open.

Execution authority is DIAGNOSTIC_ONLY — never a physical Blender claim.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import cv2
import numpy as np

try:
    from .conditions import (
        BASE_BGR,
        BG_BGR,
        CONDITION_DESCRIPTIONS,
        DISTRACTOR_BGR,
        DISTRACTOR_ID,
        FRAME_COUNT,
        OCCLUDER_BGR,
        OCCLUDER_ID,
        PRIMARY_IDS,
        RESOLUTION,
        TABLE_BGR,
        UNKNOWN_BGR,
        UNKNOWN_ID,
        Condition,
    )
except ImportError:  # script-style execution inside Blender or bare path
    from conditions import (  # type: ignore[no-redef, import-not-found]
        BASE_BGR,
        BG_BGR,
        CONDITION_DESCRIPTIONS,
        DISTRACTOR_BGR,
        DISTRACTOR_ID,
        FRAME_COUNT,
        OCCLUDER_BGR,
        OCCLUDER_ID,
        PRIMARY_IDS,
        RESOLUTION,
        TABLE_BGR,
        UNKNOWN_BGR,
        UNKNOWN_ID,
        Condition,
    )

# Texture patterns that survive near-identical mean colour.
_TEXTURE_SEEDS = {
    "obj_a": 11,  # fine horizontal stripes
    "obj_b": 23,  # fine vertical stripes
    "obj_c": 37,  # diagonal hatch
    DISTRACTOR_ID: 53,  # sparse dots
    UNKNOWN_ID: 71,  # checker
}


def _texture_disk(
    radius: int,
    seed: int,
    base_bgr: tuple[int, int, int],
    *,
    brightness: float = 1.0,
) -> np.ndarray:
    """Disk with confusable albedo but distinct structure for embeddings.

    Mean colour stays near-identical across primaries so colour histograms
    alone fail; oriented-gradient + shape cues must carry identity. The
    distractor uses a clearly different spatial frequency so re-id refuses it.
    """
    d = radius * 2 + 1
    yy, xx = np.mgrid[-radius : radius + 1, -radius : radius + 1]
    mask = (xx * xx + yy * yy) <= radius * radius
    img = np.zeros((d, d, 3), dtype=np.uint8)
    rng = np.random.default_rng(seed)
    base = np.array(base_bgr, dtype=np.float64) * brightness
    # High-contrast structured texture (still averages near base).
    structured = np.zeros((d, d), dtype=np.float64)
    if seed == 11:  # obj_a — dense horizontal bars
        structured = np.where((yy % 4) < 2, 40.0, -40.0)
    elif seed == 23:  # obj_b — dense vertical bars
        structured = np.where((xx % 4) < 2, 40.0, -40.0)
    elif seed == 37:  # obj_c — diagonal hatch
        structured = np.where(((xx + yy) % 5) < 2, 40.0, -40.0)
    elif seed == 53:  # distractor — sparse large dots (different spatial freq)
        structured = np.zeros((d, d), dtype=np.float64)
        for cy in range(-radius + 3, radius - 2, 7):
            for cx in range(-radius + 3, radius - 2, 7):
                blob = (xx - cx) ** 2 + (yy - cy) ** 2 <= 4
                structured[blob] = 55.0
        structured -= 12.0
    else:  # unknown — 4×4 checker
        structured = np.where(((xx // 4) + (yy // 4)) % 2 == 0, 45.0, -45.0)

    noise = rng.normal(0.0, 2.0, size=(d, d, 3))
    for c in range(3):
        ch = base[c] + noise[:, :, c] + structured
        img[:, :, c] = np.clip(ch, 0, 255).astype(np.uint8)
    img[~mask] = 0
    return img


def _blit_disk(
    canvas: np.ndarray,
    cx: float,
    cy: float,
    radius: int,
    seed: int,
    base_bgr: tuple[int, int, int],
    *,
    brightness: float = 1.0,
) -> tuple[float, float, float, float] | None:
    disk = _texture_disk(radius, seed, base_bgr, brightness=brightness)
    h, w = canvas.shape[:2]
    x0 = int(round(cx)) - radius
    y0 = int(round(cy)) - radius
    x1, y1 = x0 + disk.shape[1], y0 + disk.shape[0]
    # Clip to canvas.
    sx0 = max(0, -x0)
    sy0 = max(0, -y0)
    sx1 = disk.shape[1] - max(0, x1 - w)
    sy1 = disk.shape[0] - max(0, y1 - h)
    dx0, dy0 = max(0, x0), max(0, y0)
    dx1, dy1 = min(w, x1), min(h, y1)
    if dx0 >= dx1 or dy0 >= dy1:
        return None
    patch = disk[sy0:sy1, sx0:sx1]
    region = canvas[dy0:dy1, dx0:dx1]
    alpha = (patch.sum(axis=2) > 0)[:, :, None]
    region[:] = np.where(alpha, patch, region)
    return (float(dx0), float(dy0), float(dx1 - dx0), float(dy1 - dy0))


def _draw_box(
    canvas: np.ndarray,
    cx: float,
    cy: float,
    half_w: float,
    half_h: float,
    color: tuple[int, int, int],
) -> tuple[float, float, float, float] | None:
    h, w = canvas.shape[:2]
    x0 = int(max(0, cx - half_w))
    y0 = int(max(0, cy - half_h))
    x1 = int(min(w, cx + half_w))
    y1 = int(min(h, cy + half_h))
    if x0 >= x1 or y0 >= y1:
        return None
    canvas[y0:y1, x0:x1] = color
    return (float(x0), float(y0), float(x1 - x0), float(y1 - y0))


def _base_canvas(*, brightness: float = 1.0) -> np.ndarray:
    img = np.zeros((RESOLUTION[1], RESOLUTION[0], 3), dtype=np.uint8)
    img[:] = tuple(int(c * brightness) for c in BG_BGR)
    # Table region.
    img[140:220, 20:300] = tuple(int(c * brightness) for c in TABLE_BGR)
    return img


def _object_pose(
    condition: Condition, frame: int
) -> dict[str, dict[str, Any]]:
    """Return per-object pose for 1-indexed frame index matching Blender."""
    t = (frame - 1) / max(1, FRAME_COUNT - 1)
    poses: dict[str, dict[str, Any]] = {}

    # Defaults: three primaries on the table, not always all used.
    base_positions = {
        "obj_a": (80.0, 160.0),
        "obj_b": (160.0, 160.0),
        "obj_c": (240.0, 160.0),
    }
    for oid, (x, y) in base_positions.items():
        poses[oid] = {
            "xy": (x, y),
            "radius": 16,
            "visible": True,
            "color": BASE_BGR,
            "seed": _TEXTURE_SEEDS[oid],
            "brightness": 1.0,
        }

    poses[OCCLUDER_ID] = {
        "xy": (400.0, 160.0),
        "half_wh": (22.0, 18.0),
        "visible": False,
        "color": OCCLUDER_BGR,
        "shape": "box",
    }
    poses[DISTRACTOR_ID] = {
        "xy": (400.0, 160.0),
        "radius": 16,
        "visible": False,
        "color": DISTRACTOR_BGR,
        "seed": _TEXTURE_SEEDS[DISTRACTOR_ID],
        "brightness": 1.0,
    }
    poses[UNKNOWN_ID] = {
        "xy": (400.0, 100.0),
        "radius": 14,
        "visible": False,
        "color": UNKNOWN_BGR,
        "seed": _TEXTURE_SEEDS[UNKNOWN_ID],
        "brightness": 1.0,
    }

    if condition is Condition.VISUALLY_SIMILAR:
        # Mild independent drift.
        poses["obj_a"]["xy"] = (80 + 20 * t, 160)
        poses["obj_b"]["xy"] = (160, 160 - 10 * t)
        poses["obj_c"]["xy"] = (240 - 15 * t, 160)

    elif condition is Condition.CROSSING_PATHS:
        poses["obj_c"]["visible"] = False
        # A left→right, B right→left, cross mid-sequence.
        poses["obj_a"]["xy"] = (60 + 200 * t, 160)
        poses["obj_b"]["xy"] = (260 - 200 * t, 160)

    elif condition is Condition.PARTIAL_OCCLUSION:
        poses["obj_c"]["visible"] = False
        # Occluder slides over B covering ~half.
        if frame < 8:
            ox = 280.0
        elif frame < 14:
            ox = 280.0 - 120.0 * ((frame - 8) / 6.0)
        elif frame < 20:
            ox = 160.0
        elif frame < 26:
            ox = 160.0 + 120.0 * ((frame - 20) / 6.0)
        else:
            ox = 280.0
        poses[OCCLUDER_ID]["visible"] = True
        poses[OCCLUDER_ID]["xy"] = (ox, 160.0)
        poses[OCCLUDER_ID]["half_wh"] = (18.0, 20.0)

    elif condition is Condition.FULL_OCCLUSION:
        poses["obj_c"]["visible"] = False
        if frame < 8:
            ox = 300.0
        elif frame < 12:
            ox = 300.0 - 140.0 * ((frame - 8) / 4.0)
        elif frame < 22:
            ox = 160.0
            # Fully cover B: mark B not visible for scoring while occluded.
            poses["obj_b"]["visible"] = False
        elif frame < 26:
            ox = 160.0 + 140.0 * ((frame - 22) / 4.0)
            poses["obj_b"]["visible"] = True
        else:
            ox = 300.0
        poses[OCCLUDER_ID]["visible"] = True
        poses[OCCLUDER_ID]["xy"] = (ox, 160.0)
        poses[OCCLUDER_ID]["half_wh"] = (28.0, 26.0)

    elif condition is Condition.LIGHTING_CHANGE:
        bright = 1.0 if frame < 16 else 0.55
        for oid in PRIMARY_IDS:
            poses[oid]["brightness"] = bright
        poses["_global_brightness"] = bright

    elif condition is Condition.SCALE_CHANGE:
        poses["obj_c"]["visible"] = False
        poses["obj_b"]["visible"] = False
        # A approaches: radius grows.
        poses["obj_a"]["radius"] = int(12 + 14 * t)
        poses["obj_a"]["xy"] = (160.0, 160.0)

    elif condition is Condition.CAMERA_MOTION:
        # Simulate pan: shift all x by camera offset.
        pan = 40.0 * np.sin(t * np.pi)
        for oid in PRIMARY_IDS:
            x, y = poses[oid]["xy"]
            poses[oid]["xy"] = (x + pan, y)

    elif condition is Condition.LEAVE_RETURN:
        poses["obj_c"]["visible"] = False
        if frame <= 8:
            poses["obj_b"]["xy"] = (160.0, 160.0)
        elif frame <= 12:
            u = (frame - 8) / 4.0
            poses["obj_b"]["xy"] = (160.0 + 180.0 * u, 160.0)
        elif frame <= 22:
            poses["obj_b"]["visible"] = False
            poses["obj_b"]["xy"] = (360.0, 160.0)
        elif frame <= 26:
            u = (frame - 22) / 4.0
            poses["obj_b"]["visible"] = True
            poses["obj_b"]["xy"] = (360.0 - 200.0 * u, 160.0)
        else:
            poses["obj_b"]["xy"] = (160.0, 160.0)

    elif condition is Condition.DISTRACTOR_REPLACEMENT:
        poses["obj_c"]["visible"] = False
        if frame <= 10:
            poses["obj_b"]["xy"] = (160.0, 160.0)
        elif frame <= 14:
            u = (frame - 10) / 4.0
            poses["obj_b"]["xy"] = (160.0 - 180.0 * u, 160.0)
        else:
            poses["obj_b"]["visible"] = False
            poses["obj_b"]["xy"] = (-40.0, 160.0)
        if frame < 18:
            poses[DISTRACTOR_ID]["visible"] = False
        elif frame <= 22:
            u = (frame - 18) / 4.0
            poses[DISTRACTOR_ID]["visible"] = True
            poses[DISTRACTOR_ID]["xy"] = (340.0 - 180.0 * u, 160.0)
        else:
            poses[DISTRACTOR_ID]["visible"] = True
            poses[DISTRACTOR_ID]["xy"] = (160.0, 160.0)

    elif condition is Condition.UNKNOWN_ENTERING:
        # Unknown appears late on the right.
        if frame >= 18:
            poses[UNKNOWN_ID]["visible"] = True
            u = min(1.0, (frame - 18) / 6.0)
            poses[UNKNOWN_ID]["xy"] = (300.0 - 40.0 * u, 110.0)

    elif condition is Condition.PERMANENCE:
        # Three similar; A moves; B fully occluded mid; C leaves and returns;
        # distractor enters while C is gone; original C returns.
        poses["obj_a"]["xy"] = (70.0 + 80.0 * t, 150.0)
        # B occlusion window frames 10..20
        if 10 <= frame <= 20:
            poses["obj_b"]["visible"] = False
            poses[OCCLUDER_ID]["visible"] = True
            poses[OCCLUDER_ID]["xy"] = (160.0, 170.0)
            poses[OCCLUDER_ID]["half_wh"] = (30.0, 28.0)
        else:
            poses["obj_b"]["xy"] = (160.0, 170.0)
            if 8 <= frame < 10 or 20 < frame <= 22:
                poses[OCCLUDER_ID]["visible"] = True
                ox = 160.0 + (40.0 if frame < 10 else -40.0)
                poses[OCCLUDER_ID]["xy"] = (ox, 170.0)
                poses[OCCLUDER_ID]["half_wh"] = (30.0, 28.0)
        # C leave / return
        if frame <= 8:
            poses["obj_c"]["xy"] = (250.0, 155.0)
        elif frame <= 12:
            u = (frame - 8) / 4.0
            poses["obj_c"]["xy"] = (250.0 + 120.0 * u, 155.0)
        elif frame <= 24:
            poses["obj_c"]["visible"] = False
            poses["obj_c"]["xy"] = (400.0, 155.0)
        elif frame <= 28:
            u = (frame - 24) / 4.0
            poses["obj_c"]["visible"] = True
            poses["obj_c"]["xy"] = (400.0 - 150.0 * u, 155.0)
        else:
            poses["obj_c"]["xy"] = (250.0, 155.0)
        # Distractor enters mid while C is gone (must not re-id as C).
        if 16 <= frame <= 26:
            poses[DISTRACTOR_ID]["visible"] = True
            poses[DISTRACTOR_ID]["xy"] = (250.0, 155.0)
        # Unknown enters late in a free corner — must not re-use any prior id.
        if frame >= 28:
            poses[UNKNOWN_ID]["visible"] = True
            poses[UNKNOWN_ID]["xy"] = (50.0, 50.0)
            poses[UNKNOWN_ID]["radius"] = 14

    return poses


def write_condition(output: Path, condition: Condition) -> dict[str, Any]:
    """Write frames + sealed GT for one condition. Returns builder-visible manifest."""
    frames_dir = output / "frames"
    sealed_dir = output / "sealed_gt"
    frames_dir.mkdir(parents=True, exist_ok=True)
    sealed_dir.mkdir(parents=True, exist_ok=True)

    builder_frames: list[dict[str, Any]] = []
    sealed_frames: list[dict[str, Any]] = []

    for frame in range(1, FRAME_COUNT + 1):
        poses = _object_pose(condition, frame)
        global_b = float(poses.pop("_global_brightness", 1.0))  # type: ignore[arg-type]
        canvas = _base_canvas(brightness=global_b)

        objects_gt: list[dict[str, Any]] = []
        for oid, pose in poses.items():
            if oid.startswith("_"):
                continue
            visible = bool(pose.get("visible", True))
            if pose.get("shape") == "box":
                if not visible:
                    objects_gt.append(
                        {
                            "id": oid,
                            "visible": False,
                            "image_uv_px": list(pose["xy"]),
                            "bbox_xywh_px": [0, 0, 0, 0],
                            "role": "occluder",
                        }
                    )
                    continue
                cx, cy = pose["xy"]
                hw, hh = pose["half_wh"]
                bbox = _draw_box(canvas, cx, cy, hw, hh, pose["color"])
                objects_gt.append(
                    {
                        "id": oid,
                        "visible": bbox is not None,
                        "image_uv_px": [float(cx), float(cy)],
                        "bbox_xywh_px": list(bbox) if bbox else [0, 0, 0, 0],
                        "role": "occluder",
                    }
                )
                continue

            if not visible:
                objects_gt.append(
                    {
                        "id": oid,
                        "visible": False,
                        "image_uv_px": list(pose["xy"]),
                        "bbox_xywh_px": [0, 0, 0, 0],
                        "role": _role_for(oid),
                        "texture_seed": pose.get("seed"),
                    }
                )
                continue
            cx, cy = pose["xy"]
            radius = int(pose.get("radius", 16))
            bbox = _blit_disk(
                canvas,
                cx,
                cy,
                radius,
                int(pose["seed"]),
                tuple(pose["color"]),
                brightness=float(pose.get("brightness", 1.0)) * global_b,
            )
            objects_gt.append(
                {
                    "id": oid,
                    "visible": bbox is not None,
                    "image_uv_px": [float(cx), float(cy)],
                    "bbox_xywh_px": list(bbox) if bbox else [0, 0, 0, 0],
                    "role": _role_for(oid),
                    "texture_seed": pose.get("seed"),
                }
            )

        fi = frame - 1
        img_name = f"frame_{frame:04d}.png"
        img_path = frames_dir / img_name
        cv2.imwrite(str(img_path), canvas)

        gt_name = f"frame_{frame:04d}.json"
        gt_payload = {
            "frame_index": fi,
            "blender_frame": frame,
            "image": img_name,
            "condition": condition.value,
            "objects": objects_gt,
        }
        (sealed_dir / gt_name).write_text(
            json.dumps(gt_payload, indent=2), encoding="utf-8"
        )
        builder_frames.append(
            {
                "frame_index": fi,
                "image": f"frames/{img_name}",
                # Deliberately no ground_truth path in the builder-visible manifest.
            }
        )
        sealed_frames.append(
            {
                "frame_index": fi,
                "image": f"frames/{img_name}",
                "ground_truth": f"sealed_gt/{gt_name}",
            }
        )

    builder_manifest = {
        "condition": condition.value,
        "description": CONDITION_DESCRIPTIONS[condition],
        "frame_count": FRAME_COUNT,
        "resolution": list(RESOLUTION),
        "source": "synthetic_opencv",
        "execution_class_hint": "DIAGNOSTIC_ONLY",
        "coordinate_frame": {
            "image": {
                "name": "opencv-camera",
                "up_axis": "-Y",
                "units": "px",
            }
        },
        "frames": builder_frames,
        # Tracker must not use this; sealed evaluator loads sealed_manifest.
        "sealed_manifest_relative": "sealed_manifest.json",
    }
    sealed_manifest = {
        **builder_manifest,
        "frames": sealed_frames,
        "primary_ids": list(PRIMARY_IDS),
        "distractor_id": DISTRACTOR_ID,
        "unknown_id": UNKNOWN_ID,
        "occluder_id": OCCLUDER_ID,
    }
    (output / "sequence_manifest.json").write_text(
        json.dumps(builder_manifest, indent=2), encoding="utf-8"
    )
    (output / "sealed_manifest.json").write_text(
        json.dumps(sealed_manifest, indent=2), encoding="utf-8"
    )
    return builder_manifest


def _role_for(oid: str) -> str:
    if oid == DISTRACTOR_ID:
        return "distractor"
    if oid == UNKNOWN_ID:
        return "unknown"
    if oid == OCCLUDER_ID:
        return "occluder"
    return "primary"


def write_all_conditions(root: Path) -> list[str]:
    """Write every hard condition under root/<condition>/."""
    written: list[str] = []
    for cond in Condition:
        out = root / cond.value
        write_condition(out, cond)
        written.append(cond.value)
    marker = root / "OCULAR_HARD_COMPLETE"
    marker.write_text(
        f"conditions={len(written)}\n", encoding="utf-8"
    )
    print(f"OCULAR_HARD_COMPLETE conditions={len(written)} output={root}")
    return written


if __name__ == "__main__":
    import sys

    out = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("artifacts/ocular/tracking/hard")
    write_all_conditions(out)
