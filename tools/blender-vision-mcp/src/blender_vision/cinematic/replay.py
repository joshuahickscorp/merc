"""Deterministic camera-state replay from a CameraPathGraph + scroll value."""

from __future__ import annotations

import hashlib
from dataclasses import asdict, dataclass
from typing import Any

import numpy as np

from blender_vision.cinematic.path import _interpolate_scalar_curve, arc_length_tables
from blender_vision.core.util import canonical_json
from blender_vision.v2.records import CameraPathGraph, NarrativeBeat


@dataclass(slots=True)
class CameraState:
    scroll: float
    position: list[float]
    orientation_wxyz: list[float]
    focal_length_mm: float
    exposure: float
    focus_target: list[float] | None
    light_state: dict[str, Any] | None
    beat_id: str | None
    beat_label: str | None
    text_zone: str | None
    arc_length_m: float
    distance_along_m: float

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


def _nearest_focus(graph: CameraPathGraph, scroll: float) -> list[float] | None:
    if not graph.focus_targets:
        return None
    best = min(
        graph.focus_targets,
        key=lambda item: abs(float(item.get("scroll", 0.0)) - scroll),
    )
    target = best.get("target")
    if not isinstance(target, list | tuple) or len(target) != 3:
        return None
    return [float(v) for v in target]


def _active_light(graph: CameraPathGraph, scroll: float) -> dict[str, Any] | None:
    if not graph.light_state_transitions:
        return None
    ordered = sorted(
        graph.light_state_transitions, key=lambda item: float(item.get("scroll", 0.0))
    )
    chosen = ordered[0]
    for item in ordered:
        if float(item.get("scroll", 0.0)) <= scroll + 1e-12:
            chosen = item
        else:
            break
    return dict(chosen)


def _active_beat(graph: CameraPathGraph, scroll: float) -> NarrativeBeat | None:
    for beat in graph.beats:
        if beat.scroll_start <= scroll <= beat.scroll_end or (
            beat.scroll_end == 1.0 and abs(scroll - 1.0) < 1e-12
        ):
            # Prefer the latest-starting beat that still covers scroll (handles endpoints).
            covering = [
                item
                for item in graph.beats
                if item.scroll_start - 1e-12 <= scroll <= item.scroll_end + 1e-12
            ]
            if not covering:
                return None
            return max(covering, key=lambda item: item.scroll_start)
    return None


def replay_camera_state(graph: CameraPathGraph, scroll: float) -> CameraState:
    """Return the exact camera state at normalised scroll in [0, 1].

    Evaluation uses only sealed graph fields and pure math, so the same inputs
    produce byte-identical serialised output across processes.
    """
    s = float(np.clip(scroll, 0.0, 1.0))
    # Round to a fixed quantum so float noise cannot break cross-process digests.
    s = round(s, 12)
    arc, orientation = arc_length_tables(graph)
    position = arc.evaluate(s)
    quat = orientation.evaluate(s)
    # Force a stable sign for the quaternion (w >= 0) without introducing flips
    # along the path: only flip the evaluated sample, never the control data.
    if float(quat[0]) < 0.0:
        quat = -quat
    beat = _active_beat(graph, s)
    return CameraState(
        scroll=s,
        position=[round(float(v), 12) for v in position],
        orientation_wxyz=[round(float(v), 12) for v in quat],
        focal_length_mm=round(_interpolate_scalar_curve(graph.focal_length_mm, s), 12),
        exposure=round(_interpolate_scalar_curve(graph.exposure_curve, s), 12),
        focus_target=_nearest_focus(graph, s),
        light_state=_active_light(graph, s),
        beat_id=None if beat is None else beat.beat_id,
        beat_label=None if beat is None else beat.label,
        text_zone=None if beat is None else beat.text_zone,
        arc_length_m=round(float(graph.arc_length_m), 12),
        distance_along_m=round(float(graph.arc_length_m) * s, 12),
    )


def replay_digest(graph: CameraPathGraph, scroll: float) -> str:
    state = replay_camera_state(graph, scroll)
    return hashlib.sha256(canonical_json(state.to_dict())).hexdigest()


def replay_table(graph: CameraPathGraph, count: int = 64) -> list[dict[str, Any]]:
    if count < 2:
        raise ValueError("count must be at least 2")
    scrolls = np.linspace(0.0, 1.0, count)
    return [replay_camera_state(graph, float(s)).to_dict() for s in scrolls]
