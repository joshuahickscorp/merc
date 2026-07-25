from __future__ import annotations

import json
import subprocess
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.config import discover_blender
from blender_vision.core.errors import BackendUnavailable, BlenderVisionError
from blender_vision.core.util import atomic_write_json, canonical_json
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path

ALLOWED_OPERATIONS = {"inspect_scene", "render_passes", "export_glb", "validate_scene"}


class BlenderGateway:
    def __init__(self, project: ProjectStore, blender_path: Path | None = None):
        self.project = project
        discovered = discover_blender()
        self.blender_path = blender_path or (Path(discovered.path) if discovered.path else None)
        self.worker_entry = Path(__file__).with_name("worker_entry.py")

    def import_scene(self, source: Path) -> dict[str, Any]:
        artifact = ArtifactStore(self.project).ingest_file(
            source, media_type="application/x-blender"
        )
        destination = self.project.root / "scene" / f"source_{artifact.digest[:12]}.blend"
        ArtifactStore(self.project).materialize(artifact.digest, destination)
        return {
            "artifact": artifact.to_dict(),
            "scene": str(destination.relative_to(self.project.root)),
        }

    def run(
        self,
        operation: str,
        *,
        scene: Path,
        parameters: dict[str, Any] | None = None,
        timeout_seconds: int = 300,
    ) -> dict[str, Any]:
        if operation not in ALLOWED_OPERATIONS:
            raise BlenderVisionError(f"Blender operation is not allowlisted: {operation}")
        if self.blender_path is None or not self.blender_path.is_file():
            raise BackendUnavailable("Blender executable was not discovered; run bvmcp doctor")
        scene = confined_path(self.project.root, scene, must_exist=True)
        job_id = str(uuid.uuid4())
        manifest_path = self.project.root / "jobs" / "manifests" / f"{job_id}.json"
        output_path = self.project.root / "jobs" / f"{job_id}.result.json"
        log_path = self.project.root / "jobs" / "logs" / f"{job_id}.log"
        manifest: dict[str, Any] = {
            "schema_version": 1,
            "job_id": job_id,
            "operation": operation,
            "project_root": str(self.project.root),
            "scene": str(scene),
            "output": str(output_path),
            "parameters": parameters or {},
            "safe_mode": True,
        }
        import hashlib

        manifest["manifest_hash"] = hashlib.sha256(canonical_json(manifest)).hexdigest()
        atomic_write_json(manifest_path, manifest)
        command = [
            str(self.blender_path),
            "--background",
            "--factory-startup",
            "--disable-autoexec",
            str(scene),
            "--python-exit-code",
            "1",
            "--python",
            str(self.worker_entry),
            "--",
            str(manifest_path),
        ]
        try:
            result = subprocess.run(
                command,
                cwd=self.project.root,
                capture_output=True,
                text=True,
                timeout=timeout_seconds,
                check=False,
            )
        except subprocess.TimeoutExpired as error:
            raise BlenderVisionError(
                f"Blender operation timed out after {timeout_seconds}s"
            ) from error
        log_path.write_text(
            json.dumps(
                {
                    "command": [command[0], *command[1:3], "<scene>", *command[4:]],
                    "returncode": result.returncode,
                    "stdout": result.stdout[-20000:],
                    "stderr": result.stderr[-20000:],
                },
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )
        if result.returncode != 0 or not output_path.is_file():
            raise BlenderVisionError(
                f"Blender operation failed with exit {result.returncode}; log: {log_path}"
            )
        value = json.loads(output_path.read_text(encoding="utf-8"))
        value["worker_log"] = str(log_path.relative_to(self.project.root))
        value["manifest"] = str(manifest_path.relative_to(self.project.root))
        return value

    def inspect(self, scene: Path) -> dict[str, Any]:
        return self.run("inspect_scene", scene=scene)
