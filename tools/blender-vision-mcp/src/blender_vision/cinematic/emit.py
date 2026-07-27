"""Bake a CameraPathGraph into Blender camera keys and a browser motion table."""

from __future__ import annotations

import json
import subprocess
import tempfile
import textwrap
from pathlib import Path
from typing import Any

from blender_vision.cinematic.blender_probe import require_blender
from blender_vision.cinematic.replay import replay_camera_state
from blender_vision.core.errors import BackendUnavailable
from blender_vision.core.util import atomic_write_json, sha256_file
from blender_vision.v2.records import CameraPathGraph

DEFAULT_SAMPLE_RATE_HZ = 30.0
DEFAULT_DURATION_S = 20.0
INTERPOLATION_RULE = "linear_hold_last"


def export_motion_table(
    graph: CameraPathGraph,
    output_path: Path,
    *,
    sample_rate_hz: float = DEFAULT_SAMPLE_RATE_HZ,
    duration_s: float = DEFAULT_DURATION_S,
) -> dict[str, Any]:
    """Write a compact JSON motion table for the browser runtime.

    Samples are declared at `sample_rate_hz` over `duration_s` of scroll 0..1.
    Consumers must use `interpolation` (linear between samples, hold last).
    """
    if sample_rate_hz <= 0.0 or duration_s <= 0.0:
        raise ValueError("sample_rate_hz and duration_s must be positive")
    sample_count = max(2, int(round(sample_rate_hz * duration_s)) + 1)
    samples = []
    for index in range(sample_count):
        scroll = index / (sample_count - 1)
        state = replay_camera_state(graph, scroll)
        samples.append(
            {
                "t": round(scroll * duration_s, 6),
                "scroll": state.scroll,
                "position": state.position,
                "orientation_wxyz": state.orientation_wxyz,
                "focal_length_mm": state.focal_length_mm,
                "exposure": state.exposure,
                "beat_id": state.beat_id,
            }
        )
    payload = {
        "schema": "visionmcp.motion-table/v1",
        "path_id": graph.id,
        "path_digest": graph.digest,
        "sample_rate_hz": sample_rate_hz,
        "duration_s": duration_s,
        "sample_count": sample_count,
        "interpolation": INTERPOLATION_RULE,
        "coordinate_frame": graph.frame.to_dict(),
        "arc_length_m": graph.arc_length_m,
        "samples": samples,
    }
    output_path = Path(output_path)
    output_path.parent.mkdir(parents=True, exist_ok=True)
    atomic_write_json(output_path, payload)
    digest, size = sha256_file(output_path)
    return {
        "path": str(output_path),
        "bytes": size,
        "digest": digest,
        "sample_count": sample_count,
        "sample_rate_hz": sample_rate_hz,
        "duration_s": duration_s,
        "interpolation": INTERPOLATION_RULE,
    }


def bake_blender_camera(
    graph: CameraPathGraph,
    output_blend: Path,
    *,
    sample_count: int = 120,
    frame_end: int = 120,
    blender_executable: str | None = None,
) -> dict[str, Any]:
    """Create a .blend with a camera keyed from the path via headless Blender."""
    executable = require_blender(blender_executable)
    output_blend = Path(output_blend)
    output_blend.parent.mkdir(parents=True, exist_ok=True)

    keys = []
    for index in range(sample_count):
        scroll = index / max(1, sample_count - 1)
        state = replay_camera_state(graph, scroll)
        keys.append(
            {
                "frame": 1 + int(round(scroll * (frame_end - 1))),
                "location": state.position,
                "rotation_quaternion": state.orientation_wxyz,
                "lens": state.focal_length_mm,
            }
        )

    with tempfile.TemporaryDirectory(prefix="bvmcp-cinematic-") as tmp:
        keys_path = Path(tmp) / "keys.json"
        script_path = Path(tmp) / "bake_camera.py"
        result_path = Path(tmp) / "result.json"
        keys_path.write_text(json.dumps(keys), encoding="utf-8")
        script = textwrap.dedent(
            f"""
            import json
            import bpy
            from mathutils import Quaternion

            keys = json.loads({keys_path.read_text(encoding="utf-8")!r})
            out = {str(output_blend.resolve())!r}
            result_path = {str(result_path)!r}
            frame_end = {frame_end}

            bpy.ops.wm.read_factory_settings(use_empty=True)
            scene = bpy.context.scene
            scene.frame_start = 1
            scene.frame_end = frame_end
            cam_data = bpy.data.cameras.new("VisionMCP_PathCam")
            cam_obj = bpy.data.objects.new("VisionMCP_PathCam", cam_data)
            scene.collection.objects.link(cam_obj)
            scene.camera = cam_obj
            cam_obj.rotation_mode = "QUATERNION"

            for key in keys:
                frame = int(key["frame"])
                scene.frame_set(frame)
                cam_obj.location = key["location"]
                cam_obj.rotation_quaternion = Quaternion(key["rotation_quaternion"])
                cam_data.lens = float(key["lens"])
                cam_obj.keyframe_insert(data_path="location", frame=frame)
                cam_obj.keyframe_insert(data_path="rotation_quaternion", frame=frame)
                cam_data.keyframe_insert(data_path="lens", frame=frame)

            bpy.ops.wm.save_as_mainfile(filepath=out)
            payload = {{
                "blend_path": out,
                "key_count": len(keys),
                "frame_end": frame_end,
                "camera": cam_obj.name,
            }}
            with open(result_path, "w", encoding="utf-8") as handle:
                json.dump(payload, handle)
            """
        )
        script_path.write_text(script, encoding="utf-8")
        completed = subprocess.run(
            [
                executable,
                "--background",
                "--factory-startup",
                "--python-exit-code",
                "1",
                "--python",
                str(script_path),
            ],
            capture_output=True,
            text=True,
            timeout=180,
            check=False,
        )
        if completed.returncode != 0:
            raise BackendUnavailable(
                "Blender camera bake failed: "
                + (completed.stderr or completed.stdout or "no output")[:2000]
            )
        if not result_path.is_file() or not output_blend.is_file():
            raise BackendUnavailable("Blender camera bake produced no blend file")
        result = json.loads(result_path.read_text(encoding="utf-8"))
        digest, size = sha256_file(output_blend)
        result.update({"bytes": size, "digest": digest, "path": str(output_blend)})
        return result
