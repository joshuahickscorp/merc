from __future__ import annotations

import shutil
import subprocess
from pathlib import Path
from typing import Any

from blender_vision.core.errors import BackendUnavailable, BlenderVisionError
from blender_vision.security.paths import safe_filename


def extract_video_frames(
    source: Path,
    output_directory: Path,
    *,
    interval_seconds: float = 1.0,
    maximum_frames: int = 300,
) -> dict[str, Any]:
    """Extract time-ordered candidates without rewriting the source video."""
    executable = shutil.which("ffmpeg")
    if executable is None:
        raise BackendUnavailable("FFmpeg is required for video frame extraction")
    source = source.expanduser().resolve()
    if not source.is_file():
        raise FileNotFoundError(source)
    if interval_seconds <= 0 or maximum_frames < 1:
        raise ValueError("interval_seconds and maximum_frames must be positive")
    output_directory.mkdir(parents=True, exist_ok=True)
    stem = safe_filename(source.stem)
    pattern = output_directory / f"{stem}_%06d.png"
    command = [
        executable,
        "-hide_banner",
        "-loglevel",
        "error",
        "-i",
        str(source),
        "-vf",
        f"fps=1/{interval_seconds}",
        "-frames:v",
        str(maximum_frames),
        str(pattern),
    ]
    result = subprocess.run(command, capture_output=True, text=True, timeout=3600, check=False)
    if result.returncode != 0:
        raise BlenderVisionError(f"FFmpeg frame extraction failed: {result.stderr[-1000:]}")
    frames = sorted(output_directory.glob(f"{stem}_*.png"))
    frame_records = [
        {
            "path": str(frame),
            "extraction_index": index,
            "timecode_seconds": round((index - 1) * interval_seconds, 9),
        }
        for index, frame in enumerate(frames, start=1)
    ]
    return {
        "source": str(source),
        "interval_seconds": interval_seconds,
        "maximum_frames": maximum_frames,
        "frames": frame_records,
        "frame_count": len(frames),
        "timecode_policy": (
            "timecode is derived from the requested fixed extraction cadence; source remains "
            "immutable"
        ),
    }
