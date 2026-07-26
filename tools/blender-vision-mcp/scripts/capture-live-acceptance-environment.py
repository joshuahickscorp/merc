#!/usr/bin/env python3
"""Capture a redacted environment fingerprint for live acceptance."""

from __future__ import annotations

import json
import os
import platform
import shutil
import subprocess
import sys
from datetime import UTC, datetime
from pathlib import Path


def command(*argv: str, cwd: Path | None = None) -> dict[str, object]:
    resolved = shutil.which(argv[0]) if "/" not in argv[0] else argv[0]
    if not resolved or not Path(resolved).exists():
        return {"argv": list(argv), "available": False}
    result = subprocess.run(
        argv,
        cwd=cwd,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        check=False,
    )
    return {
        "argv": list(argv),
        "available": True,
        "exit_status": result.returncode,
        "stdout": result.stdout.strip(),
        "stderr": result.stderr.strip(),
    }


def main() -> int:
    if len(sys.argv) != 3:
        raise SystemExit("usage: capture-live-acceptance-environment.py ROOT OUTPUT")
    root = Path(sys.argv[1]).resolve()
    output = Path(sys.argv[2]).resolve()
    safe_environment_names = {
        "CI",
        "LANG",
        "LC_ALL",
        "NO_COLOR",
        "SHELL",
        "TERM",
        "TZ",
    }
    environment = {
        name: value if name in safe_environment_names else "<redacted>"
        for name, value in sorted(os.environ.items())
    }
    blender = "/Applications/Blender.app/Contents/MacOS/Blender"
    chrome = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    payload = {
        "schema_version": "visionmcp.live_acceptance_environment.v1",
        "captured_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "root": str(root),
        "platform": {
            "system": platform.system(),
            "release": platform.release(),
            "machine": platform.machine(),
            "python_platform": platform.platform(),
        },
        "git": {
            "head": command("git", "rev-parse", "HEAD", cwd=root),
            "status": command("git", "status", "--porcelain=v1", cwd=root),
            "remote": command("git", "remote", "-v", cwd=root),
        },
        "runtime": {
            "sw_vers": command("sw_vers"),
            "uname": command("uname", "-a"),
            "arch": command("arch"),
            "node": command("node", "--version"),
            "npm": command("npm", "--version"),
            "python": command("python3", "--version"),
            "uv": command("uv", "--version"),
            "blender": command(blender, "--version"),
            "chrome": command(chrome, "--version"),
        },
        "graphics": {
            "system_profiler": command(
                "system_profiler", "SPDisplaysDataType", "-json"
            ),
        },
        "environment": environment,
        "environment_policy": (
            "Only an explicit non-secret allowlist is emitted; all other values are redacted."
        ),
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
