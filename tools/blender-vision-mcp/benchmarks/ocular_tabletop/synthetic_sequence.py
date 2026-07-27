"""Offline diagnostic tabletop sequence when Blender cannot WM_init.

Produces the same ground-truth contract as create_scene.py (ids, roles,
frame count, animation timeline) by drawing spheres into RGB frames with
OpenCV. Execution authority is DIAGNOSTIC_ONLY — never a physical Blender
claim. Coordinate frames are still declared explicitly.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import cv2
import numpy as np

FRAME_COUNT = 36
RESOLUTION = (320, 240)

# Near-identical BGR colours so appearance alone cannot separate primaries.
COLOR_PRIMARY = (122, 158, 184)  # beige-ish
COLOR_REPLACE = (118, 153, 178)  # subtly different
COLOR_OCCLUDER = (20, 20, 25)
COLOR_TABLE = (40, 36, 32)
COLOR_BG = (28, 28, 30)


def _project(x_m: float, y_m: float, z_m: float = 0.08) -> tuple[float, float, float]:
    """Simple pinhole from tabletop world (Blender-like +Z up) to OpenCV pixels."""
    # Camera at (0, -0.85, 0.75), looking roughly +Y / down.
    cam = np.array([0.0, -0.85, 0.75])
    pt = np.array([x_m, y_m, z_m])
    # Camera looks toward origin: forward ≈ normalize(-cam) with slight down.
    forward = -cam / np.linalg.norm(cam)
    world_up = np.array([0.0, 0.0, 1.0])
    right = np.cross(forward, world_up)
    right = right / (np.linalg.norm(right) + 1e-9)
    up = np.cross(right, forward)
    up = up / (np.linalg.norm(up) + 1e-9)
    rel = pt - cam
    x_c = float(np.dot(rel, right))
    y_c = float(np.dot(rel, -up))  # OpenCV +Y down
    z_c = float(np.dot(rel, forward))
    if z_c <= 0.05:
        return 0.0, 0.0, 0.0
    f = 280.0
    u = RESOLUTION[0] / 2.0 + f * x_c / z_c
    v = RESOLUTION[1] / 2.0 + f * y_c / z_c
    radius_px = max(6.0, 18.0 * (0.9 / z_c))
    return u, v, radius_px


def _pose_for_frame(frame: int) -> dict[str, dict[str, Any]]:
    """Match create_scene.py keyframe logic (1-indexed blender frames)."""
    t = (frame - 1) / (FRAME_COUNT - 1)
    poses: dict[str, dict[str, Any]] = {}

    poses["obj_move"] = {
        "xyz": (-0.25 + 0.45 * t, 0.0, 0.08),
        "hidden": False,
        "color": COLOR_PRIMARY,
    }
    poses["obj_occlude"] = {
        "xyz": (0.0, 0.05, 0.08),
        "hidden": False,
        "color": COLOR_PRIMARY,
    }

    if frame < 10:
        ox = 0.40
    elif frame < 14:
        ox = 0.40 - 0.40 * ((frame - 10) / 4.0)
    elif frame < 22:
        ox = 0.0
    elif frame < 26:
        ox = 0.0 + 0.40 * ((frame - 22) / 4.0)
    else:
        ox = 0.40
    poses["occluder_slab"] = {
        "xyz": (ox, 0.05, 0.12),
        "hidden": False,
        "color": COLOR_OCCLUDER,
        "shape": "box",
    }

    if frame <= 8:
        leave = (0.25, -0.05, 0.08)
        leave_hidden = False
    elif frame <= 12:
        u = (frame - 8) / 4.0
        leave = (0.25 + 0.5 * u, -0.05, 0.08)
        leave_hidden = False
    elif frame <= 24:
        leave = (0.90, -0.05, 0.08)
        leave_hidden = True
    elif frame <= 28:
        u = (frame - 24) / 4.0
        leave = (0.90 - 0.65 * u, -0.05, 0.08)
        leave_hidden = False
    else:
        leave = (0.25, -0.05, 0.08)
        leave_hidden = False
    poses["obj_leave"] = {"xyz": leave, "hidden": leave_hidden, "color": COLOR_PRIMARY}

    if frame <= 10:
        depart = (-0.20, 0.18, 0.08)
        depart_hidden = False
    elif frame <= 14:
        u = (frame - 10) / 4.0
        depart = (-0.20 - 0.5 * u, 0.18, 0.08)
        depart_hidden = False
    else:
        depart = (-0.90, 0.18, 0.08)
        depart_hidden = True
    poses["obj_depart"] = {"xyz": depart, "hidden": depart_hidden, "color": COLOR_PRIMARY}

    if frame < 20:
        replace = (0.90, 0.18, 0.08)
        replace_hidden = True
    elif frame <= 24:
        u = (frame - 20) / 4.0
        replace = (0.90 - 0.7 * u, 0.18, 0.08)
        replace_hidden = False
    else:
        replace = (-0.20, 0.18, 0.08)
        replace_hidden = False
    poses["obj_replace"] = {
        "xyz": replace,
        "hidden": replace_hidden,
        "color": COLOR_REPLACE,
    }
    return poses


def _draw_table(img: np.ndarray) -> None:
    # Approximate table as a filled trapezoid / rectangle in the lower mid image.
    h, w = img.shape[:2]
    pts = np.array(
        [[int(w * 0.08), int(h * 0.78)], [int(w * 0.92), int(h * 0.78)],
         [int(w * 0.85), int(h * 0.42)], [int(w * 0.15), int(h * 0.42)]],
        dtype=np.int32,
    )
    cv2.fillConvexPoly(img, pts, COLOR_TABLE)


def _draw_object(img: np.ndarray, pose: dict[str, Any]) -> tuple[float, float, list[float], bool]:
    x, y, z = pose["xyz"]
    u, v, radius = _project(x, y, z)
    h, w = img.shape[:2]
    in_view = 0 <= u < w and 0 <= v < h and radius > 0
    if pose["hidden"] or not in_view:
        return u, v, [u - radius, v - radius, 2 * radius, 2 * radius], False
    color = pose["color"]
    if pose.get("shape") == "box":
        x0 = int(u - radius * 1.2)
        y0 = int(v - radius * 0.7)
        x1 = int(u + radius * 1.2)
        y1 = int(v + radius * 0.7)
        cv2.rectangle(img, (x0, y0), (x1, y1), color, -1)
        bbox = [float(x0), float(y0), float(x1 - x0), float(y1 - y0)]
    else:
        cv2.circle(img, (int(u), int(v)), int(radius), color, -1)
        # Subtle highlight so textures differ slightly per object seed.
        cv2.circle(
            img,
            (int(u - radius * 0.3), int(v - radius * 0.3)),
            max(1, int(radius * 0.25)),
            tuple(min(255, c + 30) for c in color),
            -1,
        )
        bbox = [u - radius, v - radius, 2 * radius, 2 * radius]
    return u, v, bbox, True


def write_synthetic_sequence(output: Path) -> dict[str, Any]:
    """Write frames + GT + manifest under output (same layout as Blender path)."""
    output.mkdir(parents=True, exist_ok=True)
    frames_dir = output / "frames"
    gt_dir = output / "ground_truth"
    frames_dir.mkdir(exist_ok=True)
    gt_dir.mkdir(exist_ok=True)

    tracked = [
        "obj_move",
        "obj_occlude",
        "obj_leave",
        "obj_depart",
        "obj_replace",
        "occluder_slab",
    ]
    sequence: dict[str, Any] = {
        "frame_count": FRAME_COUNT,
        "resolution": list(RESOLUTION),
        "source": "synthetic_diagnostic",
        "authority": "DIAGNOSTIC_ONLY",
        "coordinate_frame": {
            "world": {
                "name": "blender-world",
                "up_axis": "+Z",
                "forward_axis": "-Y",
                "handedness": "right",
                "units": "m",
            },
            "image": {
                "name": "opencv-camera",
                "up_axis": "-Y",
                "forward_axis": "+Z",
                "units": "px",
            },
        },
        "objects": tracked,
        "roles": {
            "obj_move": "moves across table",
            "obj_occlude": "stationary; occluded mid-sequence",
            "obj_leave": "leaves and returns (same identity)",
            "obj_depart": "leaves permanently (negative case)",
            "obj_replace": "replacement similar object (must not re-id as depart)",
            "occluder_slab": "occluder for obj_occlude",
        },
        "frames": [],
    }

    for frame in range(1, FRAME_COUNT + 1):
        img = np.full((RESOLUTION[1], RESOLUTION[0], 3), COLOR_BG, dtype=np.uint8)
        _draw_table(img)
        poses = _pose_for_frame(frame)
        # Draw order: table objects then occluder on top.
        draw_order = [
            "obj_move",
            "obj_leave",
            "obj_depart",
            "obj_replace",
            "obj_occlude",
            "occluder_slab",
        ]
        records = []
        for name in draw_order:
            pose = poses[name]
            u, v, bbox, visible = _draw_object(img, pose)
            # When occluder covers obj_occlude centre, mark occlude not visible.
            if name == "obj_occlude" and not poses["occluder_slab"]["hidden"]:
                ou, ov, orad = _project(*poses["occluder_slab"]["xyz"])
                if abs(u - ou) < orad * 1.1 and abs(v - ov) < orad * 0.9:
                    visible = False
            records.append(
                {
                    "id": name,
                    "visible": bool(visible and not pose["hidden"]),
                    "hidden_flag": bool(pose["hidden"]),
                    "world_xyz_m": list(pose["xyz"]),
                    "image_uv_px": [float(u), float(v)],
                    "bbox_xywh_px": [float(c) for c in bbox],
                    "in_camera_view": bool(0 <= u < RESOLUTION[0] and 0 <= v < RESOLUTION[1]),
                }
            )
        # Restore draw-order independence for GT list order.
        by_id = {r["id"]: r for r in records}
        ordered = [by_id[n] for n in tracked]

        frame_path = frames_dir / f"frame_{frame:04d}.png"
        cv2.imwrite(str(frame_path), img)
        frame_gt = {
            "frame_index": frame - 1,
            "blender_frame": frame,
            "image": frame_path.name,
            "objects": ordered,
        }
        gt_path = gt_dir / f"frame_{frame:04d}.json"
        gt_path.write_text(json.dumps(frame_gt, indent=2), encoding="utf-8")
        sequence["frames"].append(
            {
                "frame_index": frame - 1,
                "image": str(frame_path.relative_to(output)),
                "ground_truth": str(gt_path.relative_to(output)),
            }
        )

    manifest = output / "sequence_manifest.json"
    manifest.write_text(json.dumps(sequence, indent=2), encoding="utf-8")
    return sequence


if __name__ == "__main__":
    import sys

    out = Path(sys.argv[1]) if len(sys.argv) > 1 else Path("artifacts/ocular/tracking/blender")
    write_synthetic_sequence(out)
    print(f"OCULAR_TABLETOP_SYNTHETIC_COMPLETE frames={FRAME_COUNT} output={out}")
