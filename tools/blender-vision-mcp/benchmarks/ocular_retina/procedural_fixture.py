"""Deterministic non-Blender image sequences for the ocular retina lane.

Used when Blender is BLOCKED, and as the always-available synthetic baseline
for precision/recall measurements. Authority is PROCEDURAL_GROUND_TRUTH —
never labelled PHYSICAL.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import cv2
import numpy as np


def _bg(w: int = 320, h: int = 240) -> np.ndarray:
    rng = np.random.default_rng(7)
    base = rng.integers(35, 55, size=(h, w, 3), dtype=np.uint8)
    # Soft gradient so global pan has texture for optical flow.
    xs = np.linspace(0, 30, w, dtype=np.float32)
    base = np.clip(base.astype(np.float32) + xs[None, :, None], 0, 255).astype(np.uint8)
    return base


def _draw_blob(
    img: np.ndarray,
    cx: int,
    cy: int,
    size: int = 28,
    color: tuple[int, int, int] = (40, 50, 220),
) -> np.ndarray:
    out = img.copy()
    cv2.rectangle(
        out,
        (cx - size // 2, cy - size // 2),
        (cx + size // 2, cy + size // 2),
        color,
        -1,
    )
    return out


def generate_object_sequence(out_dir: Path, n_frames: int = 24) -> dict[str, Any]:
    """Object enters, moves, is occluded, leaves, returns."""
    out_dir.mkdir(parents=True, exist_ok=True)
    frames_dir = out_dir / "frames"
    frames_dir.mkdir(exist_ok=True)
    w, h = 320, 240
    base = _bg(w, h)
    # Occluder strip.
    occluder = base.copy()
    cv2.rectangle(occluder, (150, 60), (190, 180), (90, 100, 120), -1)

    # Path of blob centre (x, y) per frame index 0..n-1
    path: list[tuple[int, int] | None] = []
    for i in range(n_frames):
        if i < 3:
            path.append(None)  # not yet entered
        elif i < 8:
            path.append((40 + (i - 3) * 18, 120))  # enter + move
        elif i < 12:
            path.append((150 + (i - 8) * 4, 120))  # under occluder
        elif i < 16:
            path.append((200 + (i - 12) * 20, 120))  # leave
        elif i < 19:
            path.append(None)  # gone
        else:
            path.append((50 + (i - 19) * 15, 120))  # reappear

    ground_truth: list[dict[str, Any]] = []
    for i, pos in enumerate(path):
        frame = occluder.copy()
        events: list[str] = []
        if pos is not None:
            # Hide blob when fully under occluder x-range for occlusion frames.
            under = 150 <= pos[0] <= 185
            if under and 8 <= i < 12:
                # Draw only a sliver so occlusion is visible as area collapse.
                frame = _draw_blob(frame, pos[0], pos[1], size=10)
                events.append("OBJECT_OCCLUDED")
            else:
                frame = _draw_blob(frame, pos[0], pos[1], size=28)
            if i == 3:
                events.append("OBJECT_ENTERED")
                events.append("NEW_UNKNOWN_REGION")
            if 4 <= i <= 7:
                events.append("OBJECT_MOVED")
            if i == 15:
                events.append("OBJECT_LEFT")
            if i == 19:
                events.append("OBJECT_REAPPEARED")
                events.append("OBJECT_ENTERED")
            if i >= 20:
                events.append("OBJECT_MOVED")
        elif i == 16:
            events.append("OBJECT_LEFT")

        path_i = frames_dir / f"frame_{i:04d}.png"
        cv2.imwrite(str(path_i), frame)
        ground_truth.append(
            {
                "frame": i,
                "position_px": list(pos) if pos else None,
                "expected_events": events,
                "coordinate_frame": {
                    "name": "image-pixel",
                    "up_axis": "-Y",
                    "forward_axis": "+Z",
                },
            }
        )

    manifest = {
        "mode": "object_motion",
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "n_frames": n_frames,
        "image_width": w,
        "image_height": h,
        "frames": [
            {"index": i, "path": f"frames/frame_{i:04d}.png", **ground_truth[i]}
            for i in range(n_frames)
        ],
        "ground_truth": ground_truth,
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest


def generate_camera_sequence(out_dir: Path, n_frames: int = 16) -> dict[str, Any]:
    """Global pan of a textured scene with a static object — CAMERA_MOVED only."""
    out_dir.mkdir(parents=True, exist_ok=True)
    frames_dir = out_dir / "frames"
    frames_dir.mkdir(exist_ok=True)
    w, h = 320, 240
    # Strong texture so Farneback locks onto a coherent global translation.
    rng = np.random.default_rng(11)
    canvas_w = w + n_frames * 8 + 16
    canvas = rng.integers(30, 220, size=(h, canvas_w, 3), dtype=np.uint8)
    # Soft low-frequency structure plus a fixed landmark blob.
    yy, xx = np.mgrid[0:h, 0:canvas_w]
    canvas = np.clip(
        canvas.astype(np.float32)
        + 40.0 * np.sin(xx / 18.0)[:, :, None]
        + 25.0 * np.cos(yy / 22.0)[:, :, None],
        0,
        255,
    ).astype(np.uint8)
    canvas = _draw_blob(canvas, canvas_w // 2, h // 2, size=36, color=(40, 50, 220))
    ground_truth: list[dict[str, Any]] = []
    pan_step = 8
    for i in range(n_frames):
        x0 = i * pan_step
        frame = canvas[:, x0 : x0 + w].copy()
        path_i = frames_dir / f"frame_{i:04d}.png"
        cv2.imwrite(str(path_i), frame)
        events = ["CAMERA_MOVED"] if i > 0 else []
        ground_truth.append(
            {
                "frame": i,
                "pan_px": x0,
                "expected_events": events,
                "forbidden_events": ["OBJECT_MOVED"],
            }
        )
    manifest = {
        "mode": "camera_motion",
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "n_frames": n_frames,
        "image_width": w,
        "image_height": h,
        "frames": [
            {"index": i, "path": f"frames/frame_{i:04d}.png", **ground_truth[i]}
            for i in range(n_frames)
        ],
        "ground_truth": ground_truth,
    }
    (out_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifest
