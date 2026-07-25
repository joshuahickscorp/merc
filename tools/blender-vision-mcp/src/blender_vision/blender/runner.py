from __future__ import annotations

import json
import os
import subprocess
import threading
import time
import uuid
from collections import deque
from collections.abc import Callable
from pathlib import Path
from typing import Any

from blender_vision.core.config import discover_blender
from blender_vision.core.errors import BackendUnavailable, JobCancelled, SecurityError
from blender_vision.core.util import atomic_write_json, canonical_json
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path, safe_mode

ALLOWED_OPERATIONS = {
    "inspect_scene",
    "validate_scene",
    "import_asset",
    "create_component",
    "update_component",
    "apply_constraints",
    "create_camera",
    "apply_camera_solution",
    "render_passes",
    "evaluate_camera_candidates",
    "export_glb",
    "export_blend",
    "generate_lod",
    "save_checkpoint",
    "repair_degenerate_geometry_candidate",
    "repair_mac_studio_grille",
    "revise_rtx_5090_fe_candidate",
    "refine_rtx_5090_fe_visual_candidate",
    "refine_rtx_5090_fe_front_frame_candidate",
    "refine_dgx_spark_visual_candidate",
    "refine_dgx_spark_base_foot_candidate",
    "generate_components",
    "generate_semantic_seed",
    "generate_synthetic_dataset",
    "generate_calibration_benchmark",
}


class BlenderRunner:
    def __init__(self, project: ProjectStore):
        self.project = project
        capability = discover_blender()
        if not capability.available or not capability.path:
            raise BackendUnavailable("Blender executable was not found; run `bvmcp doctor`")
        self.executable = capability.path
        source_worker = Path(__file__).resolve().parents[3] / "blender_worker" / "entry.py"
        packaged_worker = Path(__file__).with_name("standalone_worker.py")
        self.worker_entry = source_worker if source_worker.is_file() else packaged_worker
        if not self.worker_entry.is_file():
            raise BackendUnavailable(f"Blender worker entry point is missing: {self.worker_entry}")

    def run(
        self,
        operation: str,
        scene_path: Path,
        parameters: dict[str, Any],
        *,
        job_id: str | None = None,
        timeout_seconds: int = 300,
        cancelled: Callable[[], bool] | None = None,
    ) -> dict[str, Any]:
        if operation not in ALLOWED_OPERATIONS:
            raise SecurityError(f"Blender operation is not allowlisted: {operation}")
        scene_path = confined_path(self.project.root, scene_path, must_exist=True)
        job_token = job_id or str(uuid.uuid4())
        manifest_path = self.project.root / "jobs" / "manifests" / f"{job_token}.json"
        result_path = self.project.root / "jobs" / "manifests" / f"{job_token}.result.json"
        log_path = self.project.root / "jobs" / "logs" / f"{job_token}.log"
        manifest = {
            "schema_version": 1,
            "operation": operation,
            "project_root": str(self.project.root),
            "scene_path": str(scene_path),
            "result_path": str(result_path),
            "safe_mode": safe_mode(),
            "limits": {"max_output_bytes": 512 * 1024 * 1024},
            "parameters": parameters,
        }
        manifest["manifest_hash"] = (
            __import__("hashlib").sha256(canonical_json(manifest)).hexdigest()
        )
        atomic_write_json(manifest_path, manifest)
        command = [
            self.executable,
            "--background",
            "--factory-startup",
            "--disable-autoexec",
        ]
        if operation not in {"generate_calibration_benchmark", "generate_semantic_seed"}:
            command.append(str(scene_path))
        command.extend(
            [
                "--python-exit-code",
                "1",
                "--python",
                str(self.worker_entry),
                "--",
                str(manifest_path),
            ]
        )
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
        process = subprocess.Popen(
            command,
            cwd=self.project.root,
            env=environment,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        output_chunks: deque[str] = deque()
        output_size = 0
        output_lock = threading.Lock()

        def drain_output() -> None:
            nonlocal output_size
            if process.stdout is None:
                return
            while chunk := process.stdout.read(64 * 1024):
                with output_lock:
                    output_chunks.append(chunk)
                    output_size += len(chunk)
                    while output_size > 2_000_000 and len(output_chunks) > 1:
                        output_size -= len(output_chunks.popleft())

        output_thread = threading.Thread(
            target=drain_output,
            name=f"bvmcp-blender-log-{job_token}",
            daemon=True,
        )
        output_thread.start()
        try:
            while process.poll() is None:
                if cancelled and cancelled():
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                    raise JobCancelled(f"Blender job cancelled: {job_token}")
                if time.monotonic() - started > timeout_seconds:
                    process.terminate()
                    try:
                        process.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        process.kill()
                    raise TimeoutError(f"Blender operation timed out after {timeout_seconds}s")
                time.sleep(0.1)
        finally:
            output_thread.join(timeout=5)
            with output_lock:
                output = "".join(output_chunks)[-2_000_000:]
            sanitized = output.replace(str(self.project.root), "$PROJECT")
            log_path.write_text(sanitized, encoding="utf-8")
            if process.stdout:
                process.stdout.close()
        if process.returncode != 0:
            raise RuntimeError(
                f"Blender operation {operation} failed with exit code {process.returncode}; "
                f"see {log_path}"
            )
        if not result_path.is_file():
            raise RuntimeError("Blender worker completed without a result manifest")
        result = json.loads(result_path.read_text(encoding="utf-8"))
        result["worker"] = {
            "executable": self.executable,
            "safe_mode": safe_mode(),
            "log": str(log_path.relative_to(self.project.root)),
            "duration_seconds": round(time.monotonic() - started, 6),
        }
        return result
