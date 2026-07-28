from __future__ import annotations

import os
import platform
import shutil
import subprocess
from dataclasses import asdict, dataclass
from pathlib import Path

from platformdirs import user_data_path

from blender_vision.security.paths import safe_mode


@dataclass(slots=True)
class ExecutableCapability:
    name: str
    path: str | None
    version: str | None
    available: bool


def _version(path: str, arguments: list[str]) -> str | None:
    try:
        result = subprocess.run(
            [path, *arguments], capture_output=True, text=True, timeout=10, check=False
        )
    except (OSError, subprocess.SubprocessError):
        return None
    output = (result.stdout or result.stderr).strip().splitlines()
    return output[0] if output else None


def discover_blender() -> ExecutableCapability:
    candidates = [
        os.environ.get("BVMCP_BLENDER_PATH"),
        shutil.which("blender"),
        "/Applications/Blender.app/Contents/MacOS/Blender",
        "/usr/bin/blender",
        "/snap/bin/blender",
    ]
    for candidate in candidates:
        if candidate and Path(candidate).is_file() and os.access(candidate, os.X_OK):
            return ExecutableCapability(
                "blender", str(Path(candidate).resolve()), _version(candidate, ["--version"]), True
            )
    return ExecutableCapability("blender", None, None, False)


def discover_executable(name: str, version_arguments: list[str]) -> ExecutableCapability:
    path = shutil.which(name)
    return ExecutableCapability(
        name, path, _version(path, version_arguments) if path else None, bool(path)
    )


def default_projects_root() -> Path:
    configured = os.environ.get("BVMCP_PROJECTS_ROOT")
    if configured:
        return Path(configured).expanduser().resolve()
    return user_data_path("blender-vision", "ComputExchange") / "projects"


def doctor_report() -> dict[str, object]:
    capabilities = [
        discover_blender(),
        discover_executable("colmap", ["-h"]),
        discover_executable("ffmpeg", ["-version"]),
        discover_executable("ffprobe", ["-version"]),
        discover_executable("pdftoppm", ["-v"]),
        discover_executable("git-lfs", ["version"]),
    ]
    return {
        "ok": capabilities[0].available,
        "safe_mode": safe_mode(),
        "platform": platform.platform(),
        "python": platform.python_version(),
        "projects_root": str(default_projects_root()),
        "capabilities": [asdict(capability) for capability in capabilities],
    }
