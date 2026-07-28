"""Cinematic compiler: scroll-bound camera paths with deterministic replay."""

from __future__ import annotations

from blender_vision.cinematic.emit import bake_blender_camera, export_motion_table
from blender_vision.cinematic.path import (
    CameraPathCompositionError,
    compose_camera_path,
    compose_flagship_datacentre_path,
    path_geometry_intersections,
)
from blender_vision.cinematic.replay import CameraState, replay_camera_state, replay_digest
from blender_vision.cinematic.spline import ArcLengthSpline, CatmullRomSpline, QuaternionCurve
from blender_vision.cinematic.textsafe import TextSafeResult, TextZone, evaluate_text_safe

__all__ = [
    "ArcLengthSpline",
    "CameraPathCompositionError",
    "CameraState",
    "CatmullRomSpline",
    "QuaternionCurve",
    "TextSafeResult",
    "TextZone",
    "bake_blender_camera",
    "compose_camera_path",
    "compose_flagship_datacentre_path",
    "evaluate_text_safe",
    "export_motion_table",
    "path_geometry_intersections",
    "replay_camera_state",
    "replay_digest",
]
