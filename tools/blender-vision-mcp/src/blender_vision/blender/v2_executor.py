"""Build-time Blender executor for V2 asset and benchmark generation.

This is deliberately *not* `BlenderRunner`. `BlenderRunner` is the governed
runtime gateway: an allowlist of named operations dispatched through
`blender_worker/entry.py`, reachable from MCP callers. V2 asset generation runs
repo-owned build scripts instead, so it gets its own entry point rather than
widening that allowlist with a general "run this script" operation.

The boundary it keeps: a script must already live inside the repository tree.
Caller-supplied source is never executed, the run is launched with
`--factory-startup --disable-autoexec`, proxies are stripped, and the executed
script's SHA-256 is returned so a receipt can bind exactly what ran.
"""

from __future__ import annotations

import json
import os
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

from blender_vision.core.config import discover_blender
from blender_vision.core.errors import BackendUnavailable, SecurityError
from blender_vision.core.util import sha256_file

#: The repository root that owns every executable build script.
PACKAGE_ROOT = Path(__file__).resolve().parents[3]


@dataclass(slots=True)
class BlenderRun:
    """Exactly what ran, and what it produced."""

    script: str
    script_sha256: str
    blender_version: str
    executable: str
    argv: list[str]
    returncode: int
    elapsed_seconds: float
    stdout: str
    stderr: str
    outputs: dict[str, Any] = field(default_factory=dict)

    @property
    def succeeded(self) -> bool:
        return self.returncode == 0

    def to_dict(self) -> dict[str, Any]:
        value = {
            "script": self.script,
            "script_sha256": self.script_sha256,
            "blender_version": self.blender_version,
            "executable": self.executable,
            "argv": self.argv,
            "returncode": self.returncode,
            "elapsed_seconds": round(self.elapsed_seconds, 3),
            "succeeded": self.succeeded,
            "outputs": self.outputs,
        }
        # Logs are large and noisy; keep the decisive tail only.
        value["stdout_tail"] = self.stdout[-4000:]
        value["stderr_tail"] = self.stderr[-4000:]
        return value


class BlenderExecutionError(RuntimeError):
    def __init__(self, run: BlenderRun) -> None:
        super().__init__(
            f"{Path(run.script).name} exited {run.returncode}. "
            f"stderr tail: {run.stderr[-800:] or '(empty)'}"
        )
        self.run = run


class V2BlenderExecutor:
    """Runs repo-owned Blender build scripts headlessly."""

    def __init__(self, executable: str | None = None) -> None:
        if executable:
            self.executable = executable
        else:
            capability = discover_blender()
            if not capability.available or not capability.path:
                raise BackendUnavailable(
                    "Blender was not found. Set BVMCP_BLENDER_BINARY or run `bvmcp doctor`."
                )
            self.executable = capability.path
        self._version = ""

    @property
    def version(self) -> str:
        if not self._version:
            completed = subprocess.run(  # noqa: S603 - fixed argv, discovered binary
                [self.executable, "--version"],
                capture_output=True,
                text=True,
                timeout=120,
            )
            self._version = completed.stdout.strip().splitlines()[0] if completed.stdout else ""
        return self._version

    def run(
        self,
        script: Path,
        payload: dict[str, Any] | None = None,
        *,
        blend_file: Path | None = None,
        timeout_seconds: int = 1800,
        expect_marker: str | None = None,
        outputs: dict[str, Any] | None = None,
    ) -> BlenderRun:
        script = script.resolve()
        if not script.is_file():
            raise FileNotFoundError(script)
        if not script.is_relative_to(PACKAGE_ROOT):
            raise SecurityError(
                f"refusing to execute a Blender script outside the repository: {script}"
            )
        if script.is_symlink():
            raise SecurityError("Blender build scripts cannot be symlinks")

        digest, _ = sha256_file(script)
        argv = [self.executable, "--background", "--factory-startup", "--disable-autoexec"]
        if blend_file is not None:
            argv.append(str(blend_file.resolve()))
        argv += ["--python-exit-code", "1", "--python", str(script)]
        if payload is not None:
            argv += ["--", json.dumps(payload)]

        environment = os.environ.copy()
        environment["BVMCP_NETWORK_DISABLED"] = "1"
        for name in (
            "HTTP_PROXY",
            "HTTPS_PROXY",
            "ALL_PROXY",
            "http_proxy",
            "https_proxy",
            "all_proxy",
        ):
            environment.pop(name, None)

        started = time.monotonic()
        completed = subprocess.run(  # noqa: S603 - argv is fixed, script is repo-owned
            argv,
            capture_output=True,
            text=True,
            timeout=timeout_seconds,
            env=environment,
            cwd=str(PACKAGE_ROOT),
        )
        run = BlenderRun(
            script=str(script),
            script_sha256=digest,
            blender_version=self.version,
            executable=self.executable,
            argv=argv,
            returncode=completed.returncode,
            elapsed_seconds=time.monotonic() - started,
            stdout=completed.stdout,
            stderr=completed.stderr,
            outputs=outputs or {},
        )
        if not run.succeeded:
            raise BlenderExecutionError(run)
        if expect_marker and expect_marker not in run.stdout:
            # A zero exit with no completion marker means the script bailed out
            # of its own logic before finishing. That is a failure, not a pass.
            raise BlenderExecutionError(run)
        return run
