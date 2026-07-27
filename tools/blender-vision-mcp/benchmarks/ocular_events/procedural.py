"""Deterministic OpenCV sequences for the 9×4 ocular-event calibration matrix.

Authority is PROCEDURAL_GROUND_TRUTH. Frames are public; ground-truth labels
live only under sealed_gt/ and must never be opened on the detection path.

Four fixture classes per event type:
  true_positive  — the event unambiguously occurs
  true_negative  — static / irrelevant; firing is a false positive
  near_threshold — marginal magnitude of the real event
  confounder     — looks like the event but is a different phenomenon
"""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import cv2
import numpy as np

CALIBRATED_EVENTS: tuple[str, ...] = (
    "OBJECT_MOVED",
    "CAMERA_MOVED",
    "OBJECT_ENTERED",
    "OBJECT_LEFT",
    "OBJECT_OCCLUDED",
    "OBJECT_REAPPEARED",
    "NEW_UNKNOWN_REGION",
    "LIGHT_CHANGED",
    "SURFACE_CHANGED",
)
FIXTURE_CLASSES: tuple[str, ...] = (
    "true_positive",
    "true_negative",
    "near_threshold",
    "confounder",
)

Image = np.ndarray


def _textured(w: int = 320, h: int = 240, seed: int = 0) -> Image:
    """Strong spatial texture so Farneback has gradients to lock onto."""
    rng = np.random.default_rng(seed)
    base = rng.integers(35, 200, size=(h, w, 3), dtype=np.uint8)
    yy, xx = np.mgrid[0:h, 0:w]
    base = np.clip(
        base.astype(np.float32)
        + 35.0 * np.sin(xx / 14.0)[:, :, None]
        + 22.0 * np.cos(yy / 18.0)[:, :, None]
        + 12.0 * np.sin((xx + yy) / 25.0)[:, :, None],
        0,
        255,
    ).astype(np.uint8)
    return base


def _solid(w: int, h: int, color: tuple[int, int, int]) -> Image:
    img = np.zeros((h, w, 3), dtype=np.uint8)
    img[:] = color
    return img


def _blob(
    base: Image,
    cx: int,
    cy: int,
    size: int = 28,
    color: tuple[int, int, int] = (40, 50, 220),
) -> Image:
    out = base.copy()
    half = size // 2
    cv2.rectangle(
        out,
        (cx - half, cy - half),
        (cx + half, cy + half),
        color,
        -1,
    )
    for iy in range(cy - half, cy + half, 4):
        for ix in range(cx - half, cx + half, 4):
            if 0 <= iy < out.shape[0] and 0 <= ix < out.shape[1] and ((ix + iy) // 4) % 2 == 0:
                out[iy : min(iy + 2, out.shape[0]), ix : min(ix + 2, out.shape[1])] = (
                    color[0] // 2,
                    color[1] // 2,
                    min(255, color[2] + 40),
                )
    return out


def _occluder_over(
    base: Image,
    cx: int,
    cy: int,
    *,
    obj_size: int = 28,
    color: tuple[int, int, int] = (20, 20, 20),
) -> Image:
    """Cover an object with a larger opaque rectangle (full occlusion)."""
    out = base.copy()
    half = obj_size // 2 + 10
    cv2.rectangle(
        out,
        (cx - half, cy - half),
        (cx + half, cy + half),
        color,
        -1,
    )
    return out


def _write_frames(frames_dir: Path, frames: list[Image]) -> list[str]:
    frames_dir.mkdir(parents=True, exist_ok=True)
    paths: list[str] = []
    for i, frame in enumerate(frames):
        name = f"frame_{i:04d}.png"
        assert cv2.imwrite(str(frames_dir / name), frame)
        paths.append(f"frames/{name}")
    return paths


def _finalise(
    out_dir: Path,
    *,
    event_type: str,
    fixture_class: str,
    frames: list[Image],
    expect_fire: bool,
    onset_frame: int | None,
    forbidden: list[str] | None = None,
    notes: str = "",
    measured_quantity: str = "",
    authority: str = "PROCEDURAL_GROUND_TRUTH",
) -> dict[str, Any]:
    """Write public frames + sequence_manifest; labels only under sealed_gt/."""
    out_dir.mkdir(parents=True, exist_ok=True)
    paths = _write_frames(out_dir / "frames", frames)
    h, w = frames[0].shape[:2]

    # Public sequence manifest — detector path may read this. No labels.
    sequence = {
        "event_type": event_type,
        "fixture_class": fixture_class,
        "n_frames": len(frames),
        "image_width": w,
        "image_height": h,
        "authority": authority,
        "coordinate_frame": {
            "name": "image-pixel",
            "up_axis": "-Y",
            "forward_axis": "+Z",
        },
        "frames": [
            {"index": i, "path": paths[i], "image": paths[i]} for i in range(len(frames))
        ],
    }
    (out_dir / "sequence_manifest.json").write_text(
        json.dumps(sequence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    # Sealed ground truth — evaluator only. Detection must not open this.
    sealed_dir = out_dir / "sealed_gt"
    sealed_dir.mkdir(parents=True, exist_ok=True)
    sealed = {
        "event_type": event_type,
        "fixture_class": fixture_class,
        "expect_fire": expect_fire,
        "onset_frame": onset_frame,
        "forbidden_events": list(forbidden or []),
        "notes": notes,
        "measured_quantity": measured_quantity,
        "authority": authority,
        "n_frames": len(frames),
        "image_width": w,
        "image_height": h,
    }
    (sealed_dir / "labels.json").write_text(
        json.dumps(sealed, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    (out_dir / "sealed_manifest.json").write_text(
        json.dumps(
            {
                "event_type": event_type,
                "fixture_class": fixture_class,
                "ground_truth": "sealed_gt/labels.json",
                "n_frames": len(frames),
            },
            indent=2,
            sort_keys=True,
        )
        + "\n",
        encoding="utf-8",
    )

    # Backward-compatible co-located manifest used by older runners/tests.
    # The calibration evaluator still prefers sealed_gt/labels.json.
    legacy = dict(sealed)
    legacy["frames"] = [{"index": i, "path": paths[i]} for i in range(len(frames))]
    (out_dir / "manifest.json").write_text(
        json.dumps(legacy, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )

    out = dict(sealed)
    out["root"] = str(out_dir)
    out["frames"] = legacy["frames"]
    return out


# ---------------------------------------------------------------------------
# Per-event builders
# ---------------------------------------------------------------------------


def _object_moved(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=11)
    if fixture_class == "true_positive":
        frames = [_blob(bg, 40 + i * 18, 120) for i in range(6)]
        expect, notes = True, "blob translates ~18px/frame"
        onset = 1
        forbidden = ["CAMERA_MOVED"]
        mq = "object_motion_score"
    elif fixture_class == "true_negative":
        frames = [_blob(bg, 100, 120) for _ in range(6)]
        expect, notes = False, "static blob, no residual motion"
        onset = None
        forbidden = ["OBJECT_MOVED"]
        mq = "object_motion_score"
    elif fixture_class == "near_threshold":
        frames = [_blob(bg, 100 + i * 2, 120) for i in range(6)]
        expect, notes = True, "2px/frame translation near the residual floor"
        onset = 1
        forbidden = ["CAMERA_MOVED"]
        mq = "object_motion_score"
    else:
        # Confounder: camera pans; object is world-static (nothing in frame to move).
        frames = [bg]
        for dx in (4, 8, 12, 16, 20):
            M = np.float32([[1, 0, dx], [0, 1, 0]])
            frames.append(
                cv2.warpAffine(bg, M, (bg.shape[1], bg.shape[0]), borderMode=cv2.BORDER_REFLECT)
            )
        expect, notes = False, "camera pan with world-static scene is CAMERA_MOVED, not OBJECT_MOVED"
        onset = None
        forbidden = ["OBJECT_MOVED"]
        mq = "moving_fraction"
    return _finalise(
        out_dir,
        event_type="OBJECT_MOVED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _camera_moved(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=22)
    if fixture_class == "true_positive":
        frames = [bg]
        for dx in (5, 10, 15, 20, 25):
            M = np.float32([[1, 0, dx], [0, 1, 0]])
            frames.append(
                cv2.warpAffine(bg, M, (bg.shape[1], bg.shape[0]), borderMode=cv2.BORDER_REFLECT)
            )
        expect, notes = True, "global pan of textured scene"
        onset = 1
        forbidden = ["OBJECT_MOVED"]
        mq = "moving_fraction"
    elif fixture_class == "true_negative":
        frames = [bg for _ in range(6)]
        expect, notes = False, "static textured scene"
        onset = None
        forbidden = ["CAMERA_MOVED"]
        mq = "moving_fraction"
    elif fixture_class == "near_threshold":
        frames = [bg]
        for dx in (1, 2, 3, 4, 5):
            M = np.float32([[1, 0, dx], [0, 1, 0]])
            frames.append(
                cv2.warpAffine(bg, M, (bg.shape[1], bg.shape[0]), borderMode=cv2.BORDER_REFLECT)
            )
        expect, notes = True, "slow pan just above coverage floor"
        onset = 1
        forbidden = ["OBJECT_MOVED"]
        mq = "moving_fraction"
    else:
        # Confounder: large object moves across frame; camera static.
        frames = [_blob(bg, 30 + i * 35, 120, size=56) for i in range(6)]
        expect, notes = False, "large object transit is not a camera pan"
        onset = None
        forbidden = ["CAMERA_MOVED"]
        mq = "moving_fraction"
    return _finalise(
        out_dir,
        event_type="CAMERA_MOVED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _object_entered(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=33)
    if fixture_class == "true_positive":
        frames = [bg, bg, _blob(bg, 100, 120), _blob(bg, 100, 120), _blob(bg, 100, 120)]
        expect, notes = True, "blob appears after empty frames"
        onset = 2
        forbidden: list[str] = []
        mq = "unmatched_motion_blob"
    elif fixture_class == "true_negative":
        frames = [bg, bg, bg, bg, bg]
        expect, notes = False, "empty scene stays empty"
        onset = None
        forbidden = ["OBJECT_ENTERED"]
        mq = "unmatched_motion_blob"
    elif fixture_class == "near_threshold":
        frames = [bg, bg, _blob(bg, 100, 120, size=12), _blob(bg, 100, 120, size=12)]
        expect, notes = True, "small but above min-area blob partial entry"
        onset = 2
        forbidden = []
        mq = "unmatched_motion_blob"
    else:
        # Confounder: already-present object reappears from behind an occluder
        # — that is OBJECT_REAPPEARED, not ENTERED.
        cx, cy, sz = 100, 120, 32
        present = _blob(bg, cx, cy, size=sz)
        covered = _occluder_over(present, cx, cy, obj_size=sz)
        frames = [present, present, covered, covered, present, present]
        expect, notes = (
            False,
            "object reappearing from behind occluder is REAPPEARED, not ENTERED",
        )
        onset = None
        forbidden = ["OBJECT_ENTERED"]
        mq = "unmatched_motion_blob"
    return _finalise(
        out_dir,
        event_type="OBJECT_ENTERED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _object_left(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=44)
    if fixture_class == "true_positive":
        frames = [
            _blob(bg, 100, 120),
            _blob(bg, 100, 120),
            _blob(bg, 100, 120),
            bg,
            bg,
            bg,
        ]
        expect, notes = True, "blob present then gone for >=2 frames (left frame)"
        onset = 3
        forbidden = []
        mq = "track_missing_frames"
    elif fixture_class == "true_negative":
        frames = [_blob(bg, 100, 120) for _ in range(6)]
        expect, notes = False, "blob stays"
        onset = None
        forbidden = ["OBJECT_LEFT"]
        mq = "track_missing_frames"
    elif fixture_class == "near_threshold":
        frames = [
            _blob(bg, 100, 120),
            _blob(bg, 100, 120),
            _blob(bg, 100, 120),
            bg,
            bg,
            bg,
        ]
        expect, notes = True, "exactly two frames of absence (LEFT threshold)"
        onset = 3
        forbidden = []
        mq = "track_missing_frames"
    else:
        # Confounder: fully occluded but has not left the scene.
        cx, cy, sz = 100, 120, 32
        present = _blob(bg, cx, cy, size=sz)
        covered = _occluder_over(present, cx, cy, obj_size=sz)
        frames = [present, present, present, covered, covered, covered]
        expect, notes = False, "full occlusion is not a leave — object remains in scene"
        onset = None
        forbidden = ["OBJECT_LEFT"]
        mq = "track_missing_frames"
    return _finalise(
        out_dir,
        event_type="OBJECT_LEFT",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _object_occluded(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=55)
    if fixture_class == "true_positive":
        full = _blob(bg, 100, 120, size=40)
        small = _blob(bg, 100, 120, size=12)
        frames = [full, full, small, small, small]
        expect, notes = True, "area collapses below occlusion ratio (partial occlude)"
        onset = 2
        forbidden = []
        mq = "area_ratio"
    elif fixture_class == "true_negative":
        frames = [_blob(bg, 100, 120, size=36) for _ in range(6)]
        expect, notes = False, "steady area"
        onset = None
        forbidden = ["OBJECT_OCCLUDED"]
        mq = "area_ratio"
    elif fixture_class == "near_threshold":
        full = _blob(bg, 100, 120, size=40)
        mid = _blob(bg, 100, 120, size=16)
        frames = [full, full, mid, mid]
        expect, notes = True, "area ratio just under the collapse floor"
        onset = 2
        forbidden = []
        mq = "area_ratio"
    else:
        # Confounder: object leaves frame entirely — that is LEFT, not OCCLUDED.
        frames = [
            _blob(bg, 100, 120, size=36),
            _blob(bg, 100, 120, size=36),
            _blob(bg, 100, 120, size=36),
            bg,
            bg,
            bg,
        ]
        expect, notes = False, "complete leave is OBJECT_LEFT, not OCCLUDED"
        onset = None
        forbidden = ["OBJECT_OCCLUDED"]
        mq = "area_ratio"
    return _finalise(
        out_dir,
        event_type="OBJECT_OCCLUDED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _object_reappeared(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    bg = _textured(seed=66)
    if fixture_class == "true_positive":
        full = _blob(bg, 100, 120, size=40)
        small = _blob(bg, 100, 120, size=12)
        frames = [full, full, small, small, bg, bg, full, full]
        expect, notes = True, "area collapse then same object recovers"
        onset = 6
        forbidden = []
        mq = "area_recovery"
    elif fixture_class == "true_negative":
        frames = [_blob(bg, 100, 120, size=36) for _ in range(6)]
        expect, notes = False, "never occluded"
        onset = None
        forbidden = ["OBJECT_REAPPEARED"]
        mq = "area_recovery"
    elif fixture_class == "near_threshold":
        full = _blob(bg, 100, 120, size=40)
        small = _blob(bg, 100, 120, size=12)
        frames = [full, full, small, small, full, full]
        expect, notes = True, "brief occlusion then recovery"
        onset = 4
        forbidden = []
        mq = "area_recovery"
    else:
        # Confounder: different, similar object appears where the original was.
        original = (40, 50, 220)  # red-ish
        impostor = (40, 200, 50)  # green, same size/shape, same place
        frames = [
            _blob(bg, 100, 120, size=32, color=original),
            _blob(bg, 100, 120, size=32, color=original),
            _blob(bg, 100, 120, size=32, color=original),
            bg,
            bg,
            bg,
            _blob(bg, 100, 120, size=32, color=impostor),
            _blob(bg, 100, 120, size=32, color=impostor),
        ]
        expect, notes = (
            False,
            "similar impostor at same locus must not count as reappearance",
        )
        onset = None
        forbidden = ["OBJECT_REAPPEARED"]
        mq = "area_recovery"
    return _finalise(
        out_dir,
        event_type="OBJECT_REAPPEARED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _new_unknown_region(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    """Novel connected residual after global compensation, no prior correspondence."""
    bg = _textured(seed=77)
    if fixture_class == "true_positive":
        novel = _blob(bg, 160, 120, size=48, color=(20, 200, 40))
        frames = [bg, bg, novel, novel, novel]
        expect, notes = True, "materialised region, no prior correspondence"
        onset = 2
        forbidden = ["CAMERA_MOVED"]
        mq = "global_compensated_residual_no_prior"
    elif fixture_class == "true_negative":
        frames = [bg, bg, bg, bg, bg]
        expect, notes = False, "static scene — no residual regions"
        onset = None
        forbidden = ["NEW_UNKNOWN_REGION"]
        mq = "global_compensated_residual_no_prior"
    elif fixture_class == "near_threshold":
        # Area floor is ~0.5% of frame (~384 px). size=22 → ~484 px box.
        novel = _blob(bg, 160, 120, size=22, color=(30, 180, 50))
        frames = [bg, bg, novel, novel, novel]
        expect, notes = True, "small novel region just above area floor"
        onset = 2
        forbidden = []
        mq = "global_compensated_residual_no_prior"
    else:
        # Confounder: known object changes pose / lighting — not a new region.
        base_blob = _blob(bg, 100, 120, size=36, color=(40, 50, 220))
        lit = np.clip(base_blob.astype(np.int16) + 35, 0, 255).astype(np.uint8)
        posed = _blob(bg, 108, 124, size=36, color=(40, 50, 220))  # small pose shift
        frames = [base_blob, base_blob, lit, lit, posed, posed]
        expect, notes = False, "known object pose/lighting change is not NEW_UNKNOWN_REGION"
        onset = None
        forbidden = ["NEW_UNKNOWN_REGION"]
        mq = "global_compensated_residual_no_prior"
    return _finalise(
        out_dir,
        event_type="NEW_UNKNOWN_REGION",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _light_changed(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    if fixture_class == "true_positive":
        dark = _textured(seed=88)
        bright = np.clip(dark.astype(np.int16) + 90, 0, 255).astype(np.uint8)
        frames = [dark, dark, bright, bright]
        expect, notes = True, "global +90 luma step"
        onset = 2
        forbidden = []
        mq = "abs_mean_luma_delta"
    elif fixture_class == "true_negative":
        dark = _textured(seed=88)
        frames = [dark, dark, dark, dark]
        expect, notes = False, "stable exposure"
        onset = None
        forbidden = ["LIGHT_CHANGED"]
        mq = "abs_mean_luma_delta"
    elif fixture_class == "near_threshold":
        dark = _textured(seed=88)
        mid = np.clip(dark.astype(np.int16) + 22, 0, 255).astype(np.uint8)
        frames = [dark, dark, mid, mid]
        expect, notes = True, "+22 luma just above LIGHT_DELTA_MIN"
        onset = 2
        forbidden = []
        mq = "abs_mean_luma_delta"
    else:
        # Confounder: large bright object enters — not a global illumination change.
        bg = _textured(seed=88)
        frames = [
            bg,
            bg,
            _blob(bg, 80, 120, size=90, color=(240, 240, 240)),
            _blob(bg, 100, 120, size=90, color=(240, 240, 240)),
            _blob(bg, 120, 120, size=90, color=(240, 240, 240)),
        ]
        expect, notes = False, "large bright entrant is not a global light change"
        onset = None
        forbidden = ["LIGHT_CHANGED"]
        mq = "abs_mean_luma_delta"
    return _finalise(
        out_dir,
        event_type="LIGHT_CHANGED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


def _surface_changed(out_dir: Path, fixture_class: str) -> dict[str, Any]:
    if fixture_class == "true_positive":
        base = _textured(seed=99)
        surface = base.copy()
        surface = np.clip(surface.astype(np.int16) + (14, 10, 6), 0, 255).astype(np.uint8)
        frames = [base, base, surface, surface]
        expect, notes = True, "distributed residual tint, |Δluma| under light floor"
        onset = 2
        forbidden = ["CAMERA_MOVED"]
        mq = "distributed_residual"
    elif fixture_class == "true_negative":
        base = _textured(seed=99)
        frames = [base, base, base, base]
        expect, notes = False, "static surface"
        onset = None
        forbidden = ["SURFACE_CHANGED"]
        mq = "distributed_residual"
    elif fixture_class == "near_threshold":
        base = _textured(seed=99)
        surface = np.clip(base.astype(np.int16) + (10, 8, 5), 0, 255).astype(np.uint8)
        frames = [base, base, surface, surface]
        expect, notes = True, "subtle tint near SURFACE_SCORE_MIN"
        onset = 2
        forbidden = []
        mq = "distributed_residual"
    else:
        # Confounder: specular highlight moves across a static surface.
        base = _textured(seed=99)
        frames = [base]
        for i, cx in enumerate((60, 100, 140, 180, 220)):
            frame = base.copy()
            # Soft elliptical highlight — not a material change.
            overlay = frame.copy()
            cv2.ellipse(overlay, (cx, 120), (28, 16), 0, 0, 360, (255, 255, 255), -1)
            frame = cv2.addWeighted(frame, 0.72, overlay, 0.28, 0)
            frames.append(frame)
        expect, notes = False, "moving specular on static surface is not SURFACE_CHANGED"
        onset = None
        forbidden = ["SURFACE_CHANGED"]
        mq = "distributed_residual"
    return _finalise(
        out_dir,
        event_type="SURFACE_CHANGED",
        fixture_class=fixture_class,
        frames=frames,
        expect_fire=expect,
        onset_frame=onset,
        forbidden=forbidden,
        notes=notes,
        measured_quantity=mq,
    )


_BUILDERS: dict[str, Callable[[Path, str], dict[str, Any]]] = {
    "OBJECT_MOVED": _object_moved,
    "CAMERA_MOVED": _camera_moved,
    "OBJECT_ENTERED": _object_entered,
    "OBJECT_LEFT": _object_left,
    "OBJECT_OCCLUDED": _object_occluded,
    "OBJECT_REAPPEARED": _object_reappeared,
    "NEW_UNKNOWN_REGION": _new_unknown_region,
    "LIGHT_CHANGED": _light_changed,
    "SURFACE_CHANGED": _surface_changed,
}


def generate_fixture(
    out_dir: Path, event_type: str, fixture_class: str
) -> dict[str, Any]:
    if event_type not in _BUILDERS:
        raise ValueError(f"unknown event type: {event_type}")
    if fixture_class not in FIXTURE_CLASSES:
        raise ValueError(f"unknown fixture class: {fixture_class}")
    return _BUILDERS[event_type](out_dir, fixture_class)


def generate_all(root: Path) -> list[dict[str, Any]]:
    """Write the full 9×4 matrix under root/<event>/<class>/ with sealed_gt/."""
    manifests: list[dict[str, Any]] = []
    for event in CALIBRATED_EVENTS:
        for cls in FIXTURE_CLASSES:
            dest = root / event.lower() / cls
            man = generate_fixture(dest, event, cls)
            man["root"] = str(dest)
            manifests.append(man)
    index = {
        "authority": "PROCEDURAL_GROUND_TRUTH",
        "n_fixtures": len(manifests),
        "events": list(CALIBRATED_EVENTS),
        "fixture_classes": list(FIXTURE_CLASSES),
        "fixtures": [
            {
                "event_type": m["event_type"],
                "fixture_class": m["fixture_class"],
                "path": m["root"],
                "expect_fire": m["expect_fire"],
                "sealed_gt": f"{m['root']}/sealed_gt/labels.json",
            }
            for m in manifests
        ],
    }
    root.mkdir(parents=True, exist_ok=True)
    (root / "index.json").write_text(
        json.dumps(index, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    return manifests


if __name__ == "__main__":
    import sys

    target = (
        Path(sys.argv[1])
        if len(sys.argv) > 1
        else Path(__file__).resolve().parent / "matrix"
    )
    generate_all(target)
    print(f"OCULAR_EVENTS_PROCEDURAL_OK n=36 out={target}")
