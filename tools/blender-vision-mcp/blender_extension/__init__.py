"""Thin Blender UI for the external Blender Vision MCP coordinator.

The extension deliberately owns no socket listener and performs no reconstruction work. It starts
allow-listed CLI commands in background threads and lets Blender's main thread consume their JSON
results through a timer.
"""

from __future__ import annotations

import json
import queue
import shutil
import subprocess
import threading
import time
from pathlib import Path
from typing import Any

import bpy
from bpy.props import EnumProperty, IntProperty, PointerProperty, StringProperty
from bpy.types import AddonPreferences, Operator, Panel, PropertyGroup

_RESULTS: queue.SimpleQueue[tuple[str, dict[str, Any] | None, str | None]] = queue.SimpleQueue()
_INFLIGHT: set[str] = set()
_REVIEW_PROCESS: subprocess.Popen[bytes] | None = None
_LAST_POLL = 0.0

_OPERATIONS = {
    "audit": ("project", "audit", "--async"),
    "solve_cameras": ("vision", "solve-cameras", "--async"),
    "compare": ("validate", "compare", "--async"),
    "render": ("blender", "render", "--async"),
    "receipt": ("receipt", "export"),
}


def _preferences(context: bpy.types.Context) -> BVMCPPreferences | None:
    addon = context.preferences.addons.get(__package__)
    return addon.preferences if addon else None


def _executable(context: bpy.types.Context) -> str:
    preferences = _preferences(context)
    return preferences.executable if preferences and preferences.executable else "bvmcp"


def _project(context: bpy.types.Context) -> Path:
    value = Path(bpy.path.abspath(context.scene.bvmcp_project_path)).expanduser().resolve()
    if not (value / "project.json").is_file():
        raise ValueError("Select a Blender Vision project containing project.json")
    return value


def _decode_json(stdout: str) -> dict[str, Any]:
    try:
        value = json.loads(stdout)
    except json.JSONDecodeError as error:
        raise ValueError("coordinator returned invalid JSON") from error
    if not isinstance(value, dict):
        raise ValueError("coordinator response must be a JSON object")
    if value.get("ok") is False:
        detail = value.get("error") or {}
        raise ValueError(str(detail.get("message", "coordinator operation failed")))
    return value


def _submit(tag: str, command: list[str]) -> bool:
    if tag in _INFLIGHT:
        return False
    _INFLIGHT.add(tag)

    def run() -> None:
        try:
            completed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                timeout=90,
            )
            if completed.returncode != 0:
                try:
                    _decode_json(completed.stdout)
                except ValueError as error:
                    message = str(error)
                else:
                    message = completed.stderr.strip() or (
                        f"coordinator exited {completed.returncode}"
                    )
                _RESULTS.put((tag, None, message))
                return
            _RESULTS.put((tag, _decode_json(completed.stdout), None))
        except (OSError, subprocess.SubprocessError, ValueError) as error:
            _RESULTS.put((tag, None, str(error)))

    threading.Thread(target=run, name=f"bvmcp-{tag}", daemon=True).start()
    return True


def _consume_result(tag: str, value: dict[str, Any] | None, error: str | None) -> None:
    runtime = getattr(bpy.context.window_manager, "bvmcp_runtime", None)
    if runtime is None:
        return
    if error:
        runtime.last_error = error
        runtime.status_text = f"{tag} failed"
        return
    runtime.last_error = ""
    runtime.last_result = json.dumps(value, indent=2, sort_keys=True)[:16000]
    if tag == "status" and value:
        project = value.get("project") or {}
        name = project.get("name", "Project")
        fidelity = project.get("target_fidelity", "—")
        runtime.status_text = f"{name} · target {fidelity}"
    elif tag == "jobs" and value:
        jobs = value.get("jobs") or []
        runtime.last_jobs = json.dumps(jobs, sort_keys=True)[:16000]
        active = next((job for job in jobs if job.get("id") == runtime.current_job_id), None)
        if active:
            runtime.job_status = str(active.get("status", "unknown"))
    elif value:
        job_id = value.get("job_id")
        if job_id:
            runtime.current_job_id = str(job_id)
            runtime.job_status = str(value.get("status", "queued"))
        runtime.status_text = f"{tag} submitted"


def _poll_timer() -> float:
    global _LAST_POLL
    while not _RESULTS.empty():
        tag, value, error = _RESULTS.get()
        _INFLIGHT.discard(tag)
        _consume_result(tag, value, error)
    context = bpy.context
    scene = getattr(context, "scene", None)
    project_value = getattr(scene, "bvmcp_project_path", "") if scene else ""
    if project_value and time.monotonic() - _LAST_POLL >= 3.0:
        try:
            project = _project(context)
        except (OSError, ValueError):
            pass
        else:
            executable = _executable(context)
            _submit("status", [executable, "status", "--project", str(project)])
            _submit("jobs", [executable, "jobs", "--project", str(project), "--limit", "20"])
            _LAST_POLL = time.monotonic()
    return 0.5


class BVMCPPreferences(AddonPreferences):
    bl_idname = __package__

    executable: StringProperty(
        name="Coordinator executable",
        description="Path to the bvmcp executable installed outside Blender",
        default=shutil.which("bvmcp") or "bvmcp",
        subtype="FILE_PATH",
    )
    review_port: IntProperty(name="Review port", default=8787, min=1024, max=65535)

    def draw(self, _context: bpy.types.Context) -> None:
        layout = self.layout
        layout.prop(self, "executable")
        layout.prop(self, "review_port")
        layout.label(text="The reviewer binds to loopback only; this extension opens no listener.")


class BVMCPRuntime(PropertyGroup):
    status_text: StringProperty(default="Select a project")
    current_job_id: StringProperty()
    job_status: StringProperty(default="idle")
    last_result: StringProperty()
    last_jobs: StringProperty()
    last_error: StringProperty()


class BVMCP_OT_select_project(Operator):
    bl_idname = "bvmcp.select_project"
    bl_label = "Select Blender Vision Project"
    bl_options = {"INTERNAL"}

    directory: StringProperty(subtype="DIR_PATH")

    def invoke(self, context: bpy.types.Context, _event: bpy.types.Event) -> set[str]:
        self.directory = context.scene.bvmcp_project_path
        context.window_manager.fileselect_add(self)
        return {"RUNNING_MODAL"}

    def execute(self, context: bpy.types.Context) -> set[str]:
        project = Path(bpy.path.abspath(self.directory)).expanduser().resolve()
        if not (project / "project.json").is_file():
            self.report({"ERROR"}, "Directory is not a Blender Vision project")
            return {"CANCELLED"}
        context.scene.bvmcp_project_path = str(project)
        _submit("status", [_executable(context), "status", "--project", str(project)])
        return {"FINISHED"}


class BVMCP_OT_select_artifact(Operator):
    bl_idname = "bvmcp.select_artifact"
    bl_label = "Select Project Artifact"
    bl_options = {"INTERNAL"}

    filepath: StringProperty(subtype="FILE_PATH")

    def invoke(self, context: bpy.types.Context, _event: bpy.types.Event) -> set[str]:
        try:
            self.filepath = str(_project(context) / "artifacts")
        except (OSError, ValueError):
            self.filepath = context.scene.bvmcp_artifact_path
        context.window_manager.fileselect_add(self)
        return {"RUNNING_MODAL"}

    def execute(self, context: bpy.types.Context) -> set[str]:
        try:
            project = _project(context)
            artifact = Path(bpy.path.abspath(self.filepath)).expanduser().resolve()
            if not artifact.is_file() or not artifact.is_relative_to(project):
                raise ValueError("Artifact must be a file inside the selected project")
        except (OSError, ValueError) as error:
            self.report({"ERROR"}, str(error))
            return {"CANCELLED"}
        context.scene.bvmcp_artifact_path = str(artifact)
        return {"FINISHED"}


class BVMCP_OT_submit_job(Operator):
    bl_idname = "bvmcp.submit_job"
    bl_label = "Submit Reconstruction Job"
    bl_options = {"INTERNAL"}

    def execute(self, context: bpy.types.Context) -> set[str]:
        try:
            project = _project(context)
        except (OSError, ValueError) as error:
            self.report({"ERROR"}, str(error))
            return {"CANCELLED"}
        operation = context.scene.bvmcp_operation
        command = [_executable(context), *_OPERATIONS[operation], "--project", str(project)]
        if not _submit(f"operation:{operation}", command):
            self.report({"WARNING"}, "That operation is already being submitted")
            return {"CANCELLED"}
        context.window_manager.bvmcp_runtime.status_text = f"Submitting {operation}…"
        return {"FINISHED"}


class BVMCP_OT_cancel_job(Operator):
    bl_idname = "bvmcp.cancel_job"
    bl_label = "Cancel Current Job"
    bl_options = {"INTERNAL"}

    def execute(self, context: bpy.types.Context) -> set[str]:
        runtime = context.window_manager.bvmcp_runtime
        if not runtime.current_job_id:
            self.report({"ERROR"}, "No current coordinator job")
            return {"CANCELLED"}
        try:
            project = _project(context)
        except (OSError, ValueError) as error:
            self.report({"ERROR"}, str(error))
            return {"CANCELLED"}
        _submit(
            "cancel",
            [
                _executable(context),
                "job",
                "cancel",
                runtime.current_job_id,
                "--project",
                str(project),
            ],
        )
        return {"FINISHED"}


class BVMCP_OT_open_review(Operator):
    bl_idname = "bvmcp.open_review"
    bl_label = "Open Local Reviewer"
    bl_options = {"INTERNAL"}

    def execute(self, context: bpy.types.Context) -> set[str]:
        global _REVIEW_PROCESS
        try:
            project = _project(context)
        except (OSError, ValueError) as error:
            self.report({"ERROR"}, str(error))
            return {"CANCELLED"}
        if _REVIEW_PROCESS is not None and _REVIEW_PROCESS.poll() is None:
            self.report({"INFO"}, "The local reviewer is already running")
            return {"FINISHED"}
        preferences = _preferences(context)
        port = preferences.review_port if preferences else 8787
        try:
            _REVIEW_PROCESS = subprocess.Popen(
                [
                    _executable(context),
                    "review",
                    "serve",
                    "--project",
                    str(project),
                    "--host",
                    "127.0.0.1",
                    "--port",
                    str(port),
                    "--open",
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        except OSError as error:
            self.report({"ERROR"}, str(error))
            return {"CANCELLED"}
        return {"FINISHED"}


class BVMCP_PT_coordinator(Panel):
    bl_label = "Blender Vision MCP"
    bl_idname = "BVMCP_PT_coordinator"
    bl_space_type = "VIEW_3D"
    bl_region_type = "UI"
    bl_category = "Blender Vision"

    def draw(self, context: bpy.types.Context) -> None:
        layout = self.layout
        scene = context.scene
        runtime = context.window_manager.bvmcp_runtime
        row = layout.row(align=True)
        row.prop(scene, "bvmcp_project_path", text="Project")
        row.operator("bvmcp.select_project", text="", icon="FILE_FOLDER")
        layout.label(text=runtime.status_text, icon="INFO")
        layout.separator()
        layout.prop(scene, "bvmcp_operation", text="Workflow")
        row = layout.row(align=True)
        row.operator("bvmcp.submit_job", icon="PLAY")
        row.operator("bvmcp.cancel_job", text="Cancel", icon="CANCEL")
        if runtime.current_job_id:
            layout.label(text=f"{runtime.job_status}: {runtime.current_job_id[:12]}")
        layout.operator("bvmcp.open_review", icon="URL")
        layout.separator()
        row = layout.row(align=True)
        row.prop(scene, "bvmcp_artifact_path", text="Artifact")
        row.operator("bvmcp.select_artifact", text="", icon="FILEBROWSER")
        if runtime.last_error:
            box = layout.box()
            box.alert = True
            box.label(text=runtime.last_error[:160], icon="ERROR")


_CLASSES = (
    BVMCPPreferences,
    BVMCPRuntime,
    BVMCP_OT_select_project,
    BVMCP_OT_select_artifact,
    BVMCP_OT_submit_job,
    BVMCP_OT_cancel_job,
    BVMCP_OT_open_review,
    BVMCP_PT_coordinator,
)


def register() -> None:
    for cls in _CLASSES:
        bpy.utils.register_class(cls)
    bpy.types.Scene.bvmcp_project_path = StringProperty(name="Project", subtype="DIR_PATH")
    bpy.types.Scene.bvmcp_artifact_path = StringProperty(name="Artifact", subtype="FILE_PATH")
    bpy.types.Scene.bvmcp_operation = EnumProperty(
        name="Workflow",
        items=(
            ("audit", "Audit project", "Run governed project audit"),
            ("solve_cameras", "Solve cameras", "Recover project camera hypotheses"),
            ("compare", "Compare references", "Render and compute reference residuals"),
            ("render", "Render", "Render approved project views"),
            ("receipt", "Export receipt", "Export a signed acceptance receipt"),
        ),
        default="audit",
    )
    bpy.types.WindowManager.bvmcp_runtime = PointerProperty(type=BVMCPRuntime)
    if not bpy.app.timers.is_registered(_poll_timer):
        bpy.app.timers.register(_poll_timer, first_interval=0.5, persistent=True)


def unregister() -> None:
    global _REVIEW_PROCESS
    if bpy.app.timers.is_registered(_poll_timer):
        bpy.app.timers.unregister(_poll_timer)
    if _REVIEW_PROCESS is not None and _REVIEW_PROCESS.poll() is None:
        _REVIEW_PROCESS.terminate()
    _REVIEW_PROCESS = None
    del bpy.types.WindowManager.bvmcp_runtime
    del bpy.types.Scene.bvmcp_operation
    del bpy.types.Scene.bvmcp_artifact_path
    del bpy.types.Scene.bvmcp_project_path
    for cls in reversed(_CLASSES):
        bpy.utils.unregister_class(cls)
