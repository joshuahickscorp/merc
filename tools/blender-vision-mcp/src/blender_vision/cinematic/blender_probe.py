"""Probe whether headless Blender can actually start in this environment."""

from __future__ import annotations

import os
import subprocess
import tempfile
from functools import cache
from pathlib import Path

from blender_vision.core.errors import BackendUnavailable


def discover_blender_executable() -> str:
    candidates = [
        os.environ.get("BLENDER_EXECUTABLE"),
        os.environ.get("BVMCP_BLENDER"),
        "/Applications/Blender.app/Contents/MacOS/Blender",
        "blender",
    ]
    for candidate in candidates:
        if not candidate:
            continue
        path = Path(candidate)
        if path.is_file() and os.access(path, os.X_OK):
            return str(path)
    raise BackendUnavailable(
        "Blender executable not found; set BLENDER_EXECUTABLE or install Blender 4.2 LTS"
    )


@cache
def probe_blender(executable: str | None = None) -> dict[str, str | bool]:
    """Return whether Blender can start; cache the first measurement.

    On this host Blender 4.2.1 has been observed to SIGSEGV inside
    `MTLBackend::metal_is_supported` during `WM_init` when the process cannot
    touch the Metal GPU stack. That is a real external blocker, not a silent skip.
    """
    exe = executable or discover_blender_executable()
    script = "import bpy\nprint('BVMCP_BLENDER_OK', bpy.app.version_string)\n"
    with tempfile.TemporaryDirectory(prefix="bvmcp-blender-probe-") as tmp:
        script_path = Path(tmp) / "probe.py"
        script_path.write_text(script, encoding="utf-8")
        try:
            completed = subprocess.run(
                [
                    exe,
                    "--background",
                    "--python-exit-code",
                    "1",
                    "--python",
                    str(script_path),
                ],
                capture_output=True,
                text=True,
                timeout=60,
                check=False,
            )
        except (OSError, subprocess.SubprocessError) as error:
            return {
                "available": False,
                "executable": exe,
                "reason": f"Blender probe failed to launch: {error}",
            }
    combined = (completed.stdout or "") + "\n" + (completed.stderr or "")
    if completed.returncode < 0 or completed.returncode in {139, 134, 132}:
        return {
            "available": False,
            "executable": exe,
            "reason": (
                f"Blender crashed during init (returncode={completed.returncode}). "
                "Observed SIGSEGV in MTLBackend::metal_is_supported / "
                "GPU_backend_type_selection_detect while WM_init loads the factory "
                "home file. Headless Metal GPU access is blocked in this environment. "
                f"Output tail: {combined[-500:]!r}"
            ),
        }
    if completed.returncode != 0 or "BVMCP_BLENDER_OK" not in combined:
        return {
            "available": False,
            "executable": exe,
            "reason": (
                f"Blender probe exited {completed.returncode} without success marker. "
                f"Output tail: {combined[-800:]!r}"
            ),
        }
    return {"available": True, "executable": exe, "reason": ""}


def require_blender(executable: str | None = None) -> str:
    status = probe_blender(executable)
    if not status["available"]:
        raise BackendUnavailable(str(status["reason"]))
    return str(status["executable"])
