"""Procedural (non-Blender) spatial fixture with the same schema as generate_fixture.py.

Used when Blender cannot start (e.g. Metal SIGSEGV in WM_init). Authority of the
geometry is PROCEDURAL_GROUND_TRUTH — not a substitute claim that Blender ran.
"""

from __future__ import annotations

import json
import math
from pathlib import Path
from typing import Any

import cv2
import numpy as np

BOARD_COLS = 8
BOARD_ROWS = 6
SQUARE_M = 0.025
IMAGE_W = 640
IMAGE_H = 480
FOCAL_MM = 35.0
SENSOR_W_MM = 36.0
N_VIEWS = 14


def _look_at(eye: np.ndarray, target: np.ndarray, up: np.ndarray | None = None) -> np.ndarray:
    up_vec = np.asarray(up if up is not None else (0.0, 0.0, 1.0), dtype=np.float64)
    forward = target - eye
    forward = forward / max(float(np.linalg.norm(forward)), 1e-12)
    z_axis = -forward
    x_axis = np.cross(up_vec, z_axis)
    n = float(np.linalg.norm(x_axis))
    if n < 1e-12:
        up_vec = np.array([0.0, 1.0, 0.0])
        x_axis = np.cross(up_vec, z_axis)
        n = float(np.linalg.norm(x_axis))
    x_axis = x_axis / n
    y_axis = np.cross(z_axis, x_axis)
    matrix = np.eye(4, dtype=np.float64)
    matrix[:3, 0] = x_axis
    matrix[:3, 1] = y_axis
    matrix[:3, 2] = z_axis
    matrix[:3, 3] = eye
    return matrix


def build_manifest_geometry() -> dict[str, Any]:
    n_sq_x = BOARD_COLS + 1
    n_sq_y = BOARD_ROWS + 1
    board_w = n_sq_x * SQUARE_M
    board_h = n_sq_y * SQUARE_M
    box_center = [board_w * 0.5, board_h * 0.5, 0.03]
    return {
        "board_cols": BOARD_COLS,
        "board_rows": BOARD_ROWS,
        "square_m": SQUARE_M,
        "board_width_m": board_w,
        "board_height_m": board_h,
        "box_size_m": [0.12, 0.08, 0.06],
        "box_center_m": box_center,
        "box_bounds_min": [
            box_center[0] - 0.06,
            box_center[1] - 0.04,
            0.0,
        ],
        "box_bounds_max": [
            box_center[0] + 0.06,
            box_center[1] + 0.04,
            0.06,
        ],
    }


def sensor_dict() -> dict[str, Any]:
    fx = FOCAL_MM / SENSOR_W_MM * IMAGE_W
    return {
        "width": IMAGE_W,
        "height": IMAGE_H,
        "focal_mm": FOCAL_MM,
        "sensor_width_mm": SENSOR_W_MM,
        "intrinsics": {
            "fx": fx,
            "fy": fx,
            "cx": IMAGE_W / 2.0,
            "cy": IMAGE_H / 2.0,
        },
    }


def view_poses(board: dict[str, Any]) -> list[dict[str, Any]]:
    # Aim at board centre; vary elevation strongly so fy is observable.
    bw, bh = board["board_width_m"], board["board_height_m"]
    target = np.array([bw * 0.5, bh * 0.5, 0.0], dtype=np.float64)
    poses = []
    elevations = [0.35, 0.55, 0.75, 1.05]
    for index in range(N_VIEWS):
        theta = 2.0 * math.pi * (index / N_VIEWS)
        elev = elevations[index % len(elevations)]
        radius = 0.42 + 0.08 * (index % 3)
        x = target[0] + radius * math.cos(theta) * math.cos(elev * 0.55)
        y = target[1] + radius * math.sin(theta) * math.cos(elev * 0.55)
        z = 0.05 + radius * math.sin(elev * 0.55) + 0.15 * (index % 2)
        eye = np.array([x, y, max(z, 0.12)], dtype=np.float64)
        wfc = _look_at(eye, target)
        poses.append(
            {
                "label": f"view_{index:02d}",
                "location": eye.tolist(),
                "target": target.tolist(),
                "timestamp": float(index),
                "world_from_camera": wfc.tolist(),
            }
        )
    return poses


def _board_object_points(board: dict[str, Any]) -> np.ndarray:
    cols, rows = board["board_cols"], board["board_rows"]
    square = board["square_m"]
    pts = []
    for j in range(rows + 1):
        for i in range(cols + 1):
            pts.append([i * square, j * square, 0.0])
    return np.asarray(pts, dtype=np.float64)


def _project_world_to_image(
    points_w: np.ndarray,
    wfc: np.ndarray,
    intrinsics: dict[str, float],
) -> np.ndarray | None:
    """Project world points through Blender camera to OpenCV image pixels."""
    r_b = wfc[:3, :3]
    t_b = wfc[:3, 3]
    cam = (points_w - t_b) @ r_b
    if np.any(cam[:, 2] >= -1e-4):
        return None
    ocv = np.column_stack([cam[:, 0], -cam[:, 1], -cam[:, 2]])
    if np.any(ocv[:, 2] <= 1e-6):
        return None
    fx, fy, cx, cy = (
        intrinsics["fx"],
        intrinsics["fy"],
        intrinsics["cx"],
        intrinsics["cy"],
    )
    u = fx * (ocv[:, 0] / ocv[:, 2]) + cx
    v = fy * (ocv[:, 1] / ocv[:, 2]) + cy
    return np.stack([u, v], axis=1)


def render_checkerboard_view(
    wfc: np.ndarray,
    intrinsics: dict[str, float],
    board: dict[str, Any],
) -> np.ndarray:
    """Warp a perfect orthographic checkerboard via homography from 4 corners."""
    cols, rows = board["board_cols"], board["board_rows"]
    square = board["square_m"]
    # High-res source board: one square = 40 px.
    sq_px = 40
    src_w = (cols + 1) * sq_px
    src_h = (rows + 1) * sq_px
    src = np.zeros((src_h, src_w), dtype=np.uint8)
    for j in range(rows + 1):
        for i in range(cols + 1):
            color = 15 if (i + j) % 2 == 0 else 240
            src[j * sq_px : (j + 1) * sq_px, i * sq_px : (i + 1) * sq_px] = color
    # Four outer corners of the board in source pixels and world metres.
    # Use exact width/height (not width-1): the last square edge is at src_w pixels
    # from the origin. Mapping to width-1 systematically shrinks the board and
    # biases recovered focal length by ~1/N_squares.
    src_corners = np.array(
        [
            [0.0, 0.0],
            [float(src_w), 0.0],
            [float(src_w), float(src_h)],
            [0.0, float(src_h)],
        ],
        dtype=np.float64,
    )
    world_corners = np.array(
        [
            [0.0, 0.0, 0.0],
            [(cols + 1) * square, 0.0, 0.0],
            [(cols + 1) * square, (rows + 1) * square, 0.0],
            [0.0, (rows + 1) * square, 0.0],
        ],
        dtype=np.float64,
    )
    img_corners = _project_world_to_image(world_corners, wfc, intrinsics)
    if img_corners is None:
        return np.full((IMAGE_H, IMAGE_W), 200, dtype=np.uint8)
    H, _ = cv2.findHomography(src_corners, img_corners)
    if H is None:
        return np.full((IMAGE_H, IMAGE_W), 200, dtype=np.uint8)
    warped = cv2.warpPerspective(
        src,
        H,
        (IMAGE_W, IMAGE_H),
        flags=cv2.INTER_LINEAR,
        borderMode=cv2.BORDER_CONSTANT,
        borderValue=200,
    )
    return warped


def render_depth(
    wfc: np.ndarray,
    intrinsics: dict[str, float],
    board: dict[str, Any],
) -> np.ndarray:
    """Camera-space positive depth of board plane Z=0 and the metric box AABB."""
    fx, fy, cx, cy = (
        intrinsics["fx"],
        intrinsics["fy"],
        intrinsics["cx"],
        intrinsics["cy"],
    )
    r_b = wfc[:3, :3]
    eye = wfc[:3, 3]
    depth = np.zeros((IMAGE_H, IMAGE_W), dtype=np.float32)
    us = np.arange(IMAGE_W)
    vs = np.arange(IMAGE_H)
    uu, vv = np.meshgrid(us, vs)
    # Blender camera rays: X right, Y up, -Z look
    x = (uu - cx) / fx
    y = -((vv - cy) / fy)
    dirs_local = np.stack([x, y, -np.ones_like(x)], axis=-1)
    dirs_local /= np.maximum(np.linalg.norm(dirs_local, axis=-1, keepdims=True), 1e-12)
    dirs_world = dirs_local @ r_b.T

    # Plane Z=0
    denom = dirs_world[..., 2]
    with np.errstate(divide="ignore", invalid="ignore"):
        t_plane = (0.0 - eye[2]) / denom
    valid_plane = (denom < -1e-6) & (t_plane > 0)
    hit_plane = eye + dirs_world * t_plane[..., None]
    # Restrict to board extent
    bw, bh = board["board_width_m"], board["board_height_m"]
    on_board = (
        valid_plane
        & (hit_plane[..., 0] >= 0)
        & (hit_plane[..., 0] <= bw)
        & (hit_plane[..., 1] >= 0)
        & (hit_plane[..., 1] <= bh)
    )
    hit_cam = (hit_plane - eye) @ r_b
    z_plane = (-hit_cam[..., 2]).astype(np.float32)
    depth = np.where(on_board, z_plane, depth)

    # Box AABB ray intersection (slab method), overwrite if closer.
    bmin = np.asarray(board["box_bounds_min"], dtype=np.float64)
    bmax = np.asarray(board["box_bounds_max"], dtype=np.float64)
    tmin = np.full((IMAGE_H, IMAGE_W), -np.inf)
    tmax = np.full((IMAGE_H, IMAGE_W), np.inf)
    for axis in range(3):
        d = dirs_world[..., axis]
        o = eye[axis]
        with np.errstate(divide="ignore", invalid="ignore"):
            t1 = (bmin[axis] - o) / d
            t2 = (bmax[axis] - o) / d
        t_near = np.minimum(t1, t2)
        t_far = np.maximum(t1, t2)
        # Parallel rays
        parallel = np.abs(d) < 1e-12
        outside = parallel & ((o < bmin[axis]) | (o > bmax[axis]))
        tmin = np.where(parallel, tmin, np.maximum(tmin, t_near))
        tmax = np.where(parallel, tmax, np.minimum(tmax, t_far))
        tmin = np.where(outside, np.inf, tmin)
        tmax = np.where(outside, -np.inf, tmax)
    hit_box = (tmax >= tmin) & (tmin > 1e-6)
    t_box = np.where(hit_box, tmin, np.nan)
    hit_b = eye + dirs_world * t_box[..., None]
    hit_cam_b = (hit_b - eye) @ r_b
    z_box = (-hit_cam_b[..., 2]).astype(np.float32)
    use_box = hit_box & np.isfinite(z_box) & ((depth <= 0) | (z_box < depth))
    depth = np.where(use_box, z_box, depth)
    return depth


def export_box_surface_points(board: dict[str, Any]) -> list[list[float]]:
    cx, cy, cz = board["box_center_m"]
    hx, hy, hz = 0.06, 0.04, 0.03
    pts: list[list[float]] = []
    for face_axis, sign in [(0, -1), (0, 1), (1, -1), (1, 1), (2, -1), (2, 1)]:
        for u in range(5):
            for v in range(5):
                p = [cx, cy, cz]
                others = [i for i in range(3) if i != face_axis]
                half = [hx, hy, hz]
                p[face_axis] = [cx, cy, cz][face_axis] + sign * half[face_axis]
                p[others[0]] = [cx, cy, cz][others[0]] + (u / 4 - 0.5) * 2 * half[others[0]]
                p[others[1]] = [cx, cy, cz][others[1]] + (v / 4 - 0.5) * 2 * half[others[1]]
                pts.append(p)
    return pts


def generate(out_dir: Path) -> dict[str, Any]:
    out_dir = Path(out_dir)
    out_dir.mkdir(parents=True, exist_ok=True)
    board = build_manifest_geometry()
    sensor = sensor_dict()
    rgb_dir = out_dir / "rgb"
    depth_dir = out_dir / "depth"
    rgb_dir.mkdir(exist_ok=True)
    depth_dir.mkdir(exist_ok=True)
    views = []
    for pose in view_poses(board):
        wfc = np.asarray(pose["world_from_camera"], dtype=np.float64)
        rgb = render_checkerboard_view(wfc, sensor["intrinsics"], board)
        depth = render_depth(wfc, sensor["intrinsics"], board)
        rgb_path = rgb_dir / f"{pose['label']}.png"
        depth_path = depth_dir / f"{pose['label']}_depth.npy"
        cv2.imwrite(str(rgb_path), rgb)
        np.save(depth_path, depth)
        views.append(
            {
                **pose,
                "rgb": str(rgb_path.relative_to(out_dir)),
                "depth_exr": str(depth_path.relative_to(out_dir)),
                "depth_format": "npy",
                "intrinsics": sensor["intrinsics"],
                "width": sensor["width"],
                "height": sensor["height"],
            }
        )
    surface = export_box_surface_points(board)
    surface_path = out_dir / "box_surface_points.json"
    surface_path.write_text(json.dumps(surface), encoding="utf-8")
    manifest = {
        "board": board,
        "sensor": sensor,
        "views": views,
        "box_surface_points": str(surface_path.relative_to(out_dir)),
        "generator": "benchmarks/spatial/procedural_fixture.py",
        "blender_version": None,
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "blender_status": "not_used",
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest


if __name__ == "__main__":
    import sys

    generate(Path(sys.argv[1] if len(sys.argv) > 1 else "artifacts/v2/spatial/fixture"))
