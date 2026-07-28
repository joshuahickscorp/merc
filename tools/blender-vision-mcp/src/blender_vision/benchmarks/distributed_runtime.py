from __future__ import annotations

import hashlib
import json
import multiprocessing
import os
import platform
import subprocess
import time
import uuid
from datetime import UTC, datetime
from importlib import resources
from pathlib import Path
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator

from blender_vision.core.config import discover_blender
from blender_vision.core.util import atomic_write_json, code_revision, sha256_file
from blender_vision.perception.browser import BrowserAdapter
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import DistributedScheduler
from blender_vision.scheduling.worker import WorkerRuntime


class _StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class DistributedRuntimeManifest(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    operation: Literal["validation.coverage"]
    lease_seconds: int = Field(ge=15, le=60)
    crash_exit_code: int = Field(ge=1, le=255)
    maximum_process_start_seconds: int = Field(ge=5, le=60)
    required_real_runtimes: list[
        Literal[
            "installed_chromium",
            "installed_blender",
            "additional_browser_engine",
            "two_isolated_worker_processes",
            "device_loss_restart_recovery",
        ]
    ]
    required_additional_browser_engine: Literal["webkit"]
    external_requirements: list[
        Literal["second_physical_host", "webgpu_adapter"]
    ]

    @model_validator(mode="after")
    def unique_contract(self) -> DistributedRuntimeManifest:
        for values, label in (
            (self.required_real_runtimes, "real runtimes"),
            (self.external_requirements, "external requirements"),
        ):
            if len(values) != len(set(values)):
                raise ValueError(f"distributed benchmark {label} must be unique")
        return self


class RuntimeAssertion(_StrictModel):
    id: str
    expected: Any
    observed: Any
    passed: bool


class ExternalRuntimeBlocker(_StrictModel):
    id: Literal["second_physical_host", "webgpu_adapter"]
    reason: str
    exact_resumption_contract: str
    environment_variables: list[str]


class ProcessRuntimeReceipt(_StrictModel):
    role: Literal["crashed_worker", "restarted_worker"]
    pid: int = Field(gt=0)
    parent_pid: int = Field(gt=0)
    worker_id: str
    process_exit_code: int
    job_id: str
    attempt: int = Field(ge=1)
    status: str
    lease_expires_at: str | None = None


class DistributedRuntimeReceipt(_StrictModel):
    schema_version: Literal["1"] = "1"
    benchmark_id: str
    source_git_head: str = Field(pattern=r"^[0-9a-f]{40}$")
    manifest_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    started_at: str
    completed_at: str
    elapsed_seconds: float = Field(ge=0)
    status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"]
    functional_passed: bool
    complete: bool
    assertions: list[RuntimeAssertion]
    processes: list[ProcessRuntimeReceipt]
    recovery: dict[str, Any]
    runtimes: dict[str, Any]
    external_blockers: list[ExternalRuntimeBlocker]
    local_host_identity_sha256: str = Field(pattern=r"^[0-9a-f]{64}$")
    output_digests: dict[str, str]
    claim_boundary: list[str]
    workspace: str
    failure: str | None = None


class DistributedRuntimeError(ValueError):
    pass


def _benchmark_root() -> Path:
    development = (
        Path(__file__).resolve().parents[3]
        / "benchmarks"
        / "100_plus"
        / "distributed_runtime"
    )
    if development.is_dir():
        return development
    installed = resources.files("blender_vision").joinpath(
        "benchmarks", "data", "100_plus", "distributed_runtime"
    )
    return Path(str(installed))


def load_distributed_runtime_manifest(
    path: Path | None = None,
) -> tuple[DistributedRuntimeManifest, Path]:
    manifest_path = (path or (_benchmark_root() / "manifest.json")).expanduser().resolve()
    if not manifest_path.is_file() or manifest_path.is_symlink():
        raise DistributedRuntimeError(
            f"distributed runtime manifest is missing or linked: {manifest_path}"
        )
    return (
        DistributedRuntimeManifest.model_validate_json(
            manifest_path.read_text(encoding="utf-8")
        ),
        manifest_path,
    )


def _capabilities() -> dict[str, Any]:
    return {
        "hardware": ["cpu"],
        "vram_gb": 0,
        "system_memory_gb": 1,
        "supported_models": [],
        "blender_version": None,
        "render_devices": ["CPU"],
        "capabilities": ["validation.*"],
    }


def _crash_worker_entry(
    project_root: str,
    worker_id: str,
    worker_token: str,
    lease_seconds: int,
    crash_exit_code: int,
    connection: Any,
) -> None:
    """Claim a real lease, publish non-secret state, then emulate abrupt device loss."""
    project = ProjectStore.open(Path(project_root))
    scheduler = DistributedScheduler(project)
    scheduler.heartbeat(
        worker_id,
        worker_token,
        load={"current_jobs": 0, "queue_length": 0},
    )
    lease = scheduler.claim(
        worker_id,
        worker_token,
        lease_seconds=lease_seconds,
    )
    if lease is None:
        connection.send({"error": "crash worker could not claim a job", "pid": os.getpid()})
        connection.close()
        os._exit(74)
    connection.send(
        {
            "pid": os.getpid(),
            "job_id": lease["job_id"],
            "attempt": lease["attempt"],
            "lease_expires_at": lease["lease_expires_at"],
        }
    )
    connection.close()
    os._exit(crash_exit_code)


def _restart_worker_entry(
    project_root: str,
    worker_id: str,
    worker_token: str,
    connection: Any,
) -> None:
    project = ProjectStore.open(Path(project_root))
    report = WorkerRuntime(
        project,
        worker_id,
        worker_token,
        lease_seconds=30,
    ).run(once=True)
    connection.send({"pid": os.getpid(), "report": report})
    connection.close()


def _assertion(
    identifier: str, expected: Any, observed: Any, passed: bool
) -> RuntimeAssertion:
    return RuntimeAssertion(
        id=identifier,
        expected=expected,
        observed=observed,
        passed=passed,
    )


def _host_identity() -> str:
    value = {
        "hostname": platform.node(),
        "machine": platform.machine(),
        "platform": platform.platform(),
        "node": uuid.getnode(),
    }
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()


class DistributedRuntimeBenchmarkRunner:
    def __init__(self, manifest_path: Path | None = None):
        self.manifest, self.manifest_path = load_distributed_runtime_manifest(
            manifest_path
        )

    def run(self, output_root: Path) -> DistributedRuntimeReceipt:
        output_root = output_root.expanduser().resolve()
        if output_root.exists() and any(output_root.iterdir()):
            raise DistributedRuntimeError(
                f"distributed runtime output must be new or empty: {output_root}"
            )
        output_root.mkdir(parents=True, exist_ok=True)
        started_at = datetime.now(UTC).isoformat()
        started = time.monotonic()
        source_head = code_revision(Path(__file__).resolve().parents[3])
        if len(source_head) != 40:
            raise DistributedRuntimeError(
                "distributed runtime benchmark requires a full Git source revision"
            )
        manifest_sha256 = sha256_file(self.manifest_path)[0]
        workspace = output_root / "workspace"
        assertions: list[RuntimeAssertion] = []
        processes: list[ProcessRuntimeReceipt] = []
        recovery: dict[str, Any] = {}
        runtimes: dict[str, Any] = {}
        external_blockers: list[ExternalRuntimeBlocker] = []
        output_digests: dict[str, str] = {}
        failure: str | None = None
        try:
            project = ProjectStore.create(
                workspace / "project",
                "Distributed Physical Runtime Benchmark",
                metadata={"benchmark": self.manifest.benchmark_id},
            )
            processes, recovery, process_assertions = self._process_recovery(project)
            assertions.extend(process_assertions)
            runtimes["browsers"] = self._browser_runtimes()
            runtimes["blender"] = self._blender_runtime()
            runtime_assertions, runtime_blockers = self._runtime_assertions(
                runtimes, source_head
            )
            assertions.extend(runtime_assertions)
            external_blockers.extend(runtime_blockers)
            events_path = output_root / "job-events.json"
            with project.connection() as connection:
                events = [
                    {
                        "sequence": row["sequence"],
                        "event": row["event"],
                        "created_at": row["created_at"],
                        "payload": json.loads(row["payload_json"]),
                    }
                    for row in connection.execute(
                        "SELECT sequence,event,payload_json,created_at "
                        "FROM job_events WHERE job_id=? ORDER BY sequence",
                        (recovery["job_id"],),
                    ).fetchall()
                ]
            atomic_write_json(events_path, events)
            output_digests["job-events.json"] = sha256_file(events_path)[0]
        except Exception as error:
            failure = f"{type(error).__name__}: {error}"
            atomic_write_json(
                output_root / "distributed-runtime.failure.json",
                {
                    "schema_version": "1",
                    "source_git_head": source_head,
                    "manifest_sha256": manifest_sha256,
                    "failure": failure,
                    "completed_at": datetime.now(UTC).isoformat(),
                },
            )

        functional_passed = (
            bool(assertions)
            and all(item.passed for item in assertions)
            and len(processes) == 2
            and failure is None
        )
        complete = functional_passed and not external_blockers
        status: Literal["PASS", "FAIL", "BLOCKED_EXTERNAL"]
        if failure is not None or not functional_passed:
            status = "FAIL"
        elif external_blockers:
            status = "BLOCKED_EXTERNAL"
        else:
            status = "PASS"
        receipt = DistributedRuntimeReceipt(
            benchmark_id=self.manifest.benchmark_id,
            source_git_head=source_head,
            manifest_sha256=manifest_sha256,
            started_at=started_at,
            completed_at=datetime.now(UTC).isoformat(),
            elapsed_seconds=time.monotonic() - started,
            status=status,
            functional_passed=functional_passed,
            complete=complete,
            assertions=assertions,
            processes=processes,
            recovery=recovery,
            runtimes=runtimes,
            external_blockers=external_blockers,
            local_host_identity_sha256=_host_identity(),
            output_digests=output_digests,
            claim_boundary=[
                "Proves installed Chromium, WebKit, and Blender launches on this host.",
                "Proves a real OS worker process can die with an active lease and a "
                "different OS process can resume the requeued job.",
                "Distinct PIDs are process isolation, not distinct physical hosts.",
                "Absent second-host or WebGPU execution remains BLOCKED_EXTERNAL.",
            ],
            workspace=str(workspace),
            failure=failure,
        )
        atomic_write_json(
            output_root / "distributed-runtime.receipt.json",
            receipt.model_dump(mode="json"),
        )
        return receipt

    def _process_recovery(
        self, project: ProjectStore
    ) -> tuple[list[ProcessRuntimeReceipt], dict[str, Any], list[RuntimeAssertion]]:
        scheduler = DistributedScheduler(project)
        crash_worker = scheduler.register(
            "Crash probe worker", "vision", _capabilities()
        )
        restart_worker = scheduler.register(
            "Restart recovery worker", "vision", _capabilities()
        )
        job_id = Coordinator(project).enqueue(self.manifest.operation)
        context = multiprocessing.get_context("spawn")
        parent, child = context.Pipe(duplex=False)
        crash_process = context.Process(
            target=_crash_worker_entry,
            args=(
                str(project.root),
                crash_worker["id"],
                crash_worker["worker_token"],
                self.manifest.lease_seconds,
                self.manifest.crash_exit_code,
                child,
            ),
            name="visionmcp-crash-probe",
        )
        crash_process.start()
        child.close()
        if not parent.poll(self.manifest.maximum_process_start_seconds):
            crash_process.terminate()
            crash_process.join(timeout=5)
            raise TimeoutError("crash worker did not publish its lease")
        crashed = parent.recv()
        parent.close()
        crash_process.join(timeout=5)
        if crash_process.is_alive():
            crash_process.terminate()
            crash_process.join(timeout=5)
            raise TimeoutError("crash worker did not exit")
        if crashed.get("error"):
            raise RuntimeError(str(crashed["error"]))
        crash_exit_code = int(crash_process.exitcode or 0)
        lease_expiry = datetime.fromisoformat(crashed["lease_expires_at"])
        wait_seconds = max(
            0.0, (lease_expiry - datetime.now(UTC)).total_seconds() + 0.1
        )
        time.sleep(wait_seconds)
        reaped = scheduler.reap_expired()
        requeued_job = project.job(job_id)

        restart_parent, restart_child = context.Pipe(duplex=False)
        restart_process = context.Process(
            target=_restart_worker_entry,
            args=(
                str(project.root),
                restart_worker["id"],
                restart_worker["worker_token"],
                restart_child,
            ),
            name="visionmcp-restart-worker",
        )
        restart_process.start()
        restart_child.close()
        if not restart_parent.poll(self.manifest.maximum_process_start_seconds):
            restart_process.terminate()
            restart_process.join(timeout=5)
            raise TimeoutError("restart worker did not publish its result")
        restarted = restart_parent.recv()
        restart_parent.close()
        restart_process.join(timeout=5)
        if restart_process.is_alive():
            restart_process.terminate()
            restart_process.join(timeout=5)
            raise TimeoutError("restart worker did not exit")
        restart_exit_code = int(restart_process.exitcode or 0)
        report = restarted["report"]
        completed = project.job(job_id)
        with project.connection() as connection:
            provenance = connection.execute(
                "SELECT execution_json,failure_class FROM job_provenance WHERE job_id=?",
                (job_id,),
            ).fetchone()
        process_receipts = [
            ProcessRuntimeReceipt(
                role="crashed_worker",
                pid=int(crashed["pid"]),
                parent_pid=os.getpid(),
                worker_id=crash_worker["id"],
                process_exit_code=crash_exit_code,
                job_id=job_id,
                attempt=int(crashed["attempt"]),
                status="device_lost",
                lease_expires_at=crashed["lease_expires_at"],
            ),
            ProcessRuntimeReceipt(
                role="restarted_worker",
                pid=int(restarted["pid"]),
                parent_pid=os.getpid(),
                worker_id=restart_worker["id"],
                process_exit_code=restart_exit_code,
                job_id=job_id,
                attempt=int(report["attempt"]),
                status=str(report["status"]),
            ),
        ]
        recovery = {
            "job_id": job_id,
            "operation": self.manifest.operation,
            "reaper": reaped,
            "status_after_reap": requeued_job["status"],
            "final_status": completed["status"],
            "final_worker_id": completed.get("result", {}).get("worker", {}).get("id"),
            "final_attempt": completed.get("result", {}).get("worker", {}).get("attempt"),
            "provenance_execution": (
                json.loads(provenance["execution_json"]) if provenance else None
            ),
            "provenance_failure_class": (
                provenance["failure_class"] if provenance else None
            ),
        }
        pids = {item.pid for item in process_receipts}
        assertions = [
            _assertion(
                "crash_process_exit",
                self.manifest.crash_exit_code,
                crash_exit_code,
                crash_exit_code == self.manifest.crash_exit_code,
            ),
            _assertion(
                "isolated_worker_processes",
                {"minimum_distinct_child_pids": 2, "parent_excluded": True},
                {"child_pids": sorted(pids), "parent_pid": os.getpid()},
                len(pids) == 2 and os.getpid() not in pids,
            ),
            _assertion(
                "expired_lease_requeued",
                {"requeued": 1, "status": "queued"},
                {"reaper": reaped, "status": requeued_job["status"]},
                reaped == {"requeued": 1, "failed": 0}
                and requeued_job["status"] == "queued",
            ),
            _assertion(
                "restart_process_completed",
                {"exit_code": 0, "status": "succeeded", "attempt": 2},
                {
                    "exit_code": restart_exit_code,
                    "status": report.get("status"),
                    "attempt": report.get("attempt"),
                },
                restart_exit_code == 0
                and report.get("status") == "succeeded"
                and report.get("attempt") == 2,
            ),
            _assertion(
                "job_provenance_bound_to_restart_worker",
                restart_worker["id"],
                recovery["final_worker_id"],
                recovery["final_worker_id"] == restart_worker["id"]
                and recovery["final_attempt"] == 2
                and recovery["provenance_execution"] is not None,
            ),
        ]
        return process_receipts, recovery, assertions

    @staticmethod
    def _browser_runtimes() -> dict[str, Any]:
        from playwright.sync_api import sync_playwright

        with sync_playwright() as playwright:
            target_url = "data:text/html,<title>runtime</title>"
            # BrowserAdapter requires an allowlisted origin for governed HTTP capture.
            # These launch probes use a data document and perform no network request.
            chromium_path = Path(
                BrowserAdapter._resolve_browser_executable(
                    engine="chromium",
                    channel="chrome",
                    executable_path=None,
                )
                or ""
            )
            if not chromium_path.is_file():
                raise RuntimeError("installed Chromium executable is unavailable")
            chromium = playwright.chromium.launch(
                channel="chrome",
                headless=True,
                args=[
                    "--disable-background-networking",
                    "--disable-component-update",
                    "--no-first-run",
                ],
            )
            chromium_page = chromium.new_page()
            chromium_page.goto(target_url)
            chromium_value = chromium_page.evaluate("() => 6 * 7")
            webgpu = chromium_page.evaluate(
                """async () => {
                  if (!navigator.gpu) return {navigator: false, adapter: false};
                  const adapter = await Promise.race([
                    navigator.gpu.requestAdapter(),
                    new Promise(resolve => setTimeout(() => resolve(null), 3000)),
                  ]);
                  return {navigator: true, adapter: Boolean(adapter)};
                }"""
            )
            chromium_record = {
                "engine": "chromium",
                "version": chromium.version,
                "executable": str(chromium_path),
                "executable_sha256": sha256_file(chromium_path)[0],
                "launch_probe": chromium_value,
                "webgpu": webgpu,
            }
            chromium.close()

            webkit_path = Path(playwright.webkit.executable_path).resolve()
            if not webkit_path.is_file():
                raise RuntimeError("managed WebKit executable is unavailable")
            webkit = playwright.webkit.launch(headless=True)
            webkit_page = webkit.new_page()
            webkit_page.goto(target_url)
            webkit_value = webkit_page.evaluate("() => 6 * 7")
            webkit_record = {
                "engine": "webkit",
                "version": webkit.version,
                "executable": str(webkit_path),
                "executable_sha256": sha256_file(webkit_path)[0],
                "launch_probe": webkit_value,
            }
            webkit.close()
        return {"chromium": chromium_record, "webkit": webkit_record}

    @staticmethod
    def _blender_runtime() -> dict[str, Any]:
        capability = discover_blender()
        if not capability.available or not capability.path:
            raise RuntimeError("installed Blender executable is unavailable")
        executable = Path(capability.path)
        marker = "VISIONMCP_BLENDER_RUNTIME="
        expression = (
            "import bpy,json;print("
            f"{marker!r}+json.dumps("
            "{'version':bpy.app.version_string,'background':bpy.app.background}))"
        )
        process = subprocess.run(
            [
                str(executable),
                "--background",
                "--factory-startup",
                "--python-expr",
                expression,
            ],
            capture_output=True,
            text=True,
            timeout=60,
            check=False,
        )
        payload = None
        for line in process.stdout.splitlines():
            if line.startswith(marker):
                payload = json.loads(line[len(marker) :])
                break
        if process.returncode != 0 or payload is None or payload.get("background") is not True:
            raise RuntimeError(
                f"Blender runtime probe failed with exit code {process.returncode}"
            )
        return {
            "version": payload["version"],
            "executable": str(executable),
            "executable_sha256": sha256_file(executable)[0],
            "exit_code": process.returncode,
            "background": payload["background"],
        }

    def _runtime_assertions(
        self,
        runtimes: dict[str, Any],
        source_head: str,
    ) -> tuple[list[RuntimeAssertion], list[ExternalRuntimeBlocker]]:
        browsers = runtimes["browsers"]
        blender = runtimes["blender"]
        assertions = [
            _assertion(
                "installed_chromium",
                42,
                browsers["chromium"]["launch_probe"],
                browsers["chromium"]["launch_probe"] == 42,
            ),
            _assertion(
                "additional_webkit_engine",
                42,
                browsers["webkit"]["launch_probe"],
                browsers["webkit"]["launch_probe"] == 42,
            ),
            _assertion(
                "installed_blender",
                {"exit_code": 0, "background": True},
                {
                    "exit_code": blender["exit_code"],
                    "background": blender["background"],
                },
                blender["exit_code"] == 0 and blender["background"] is True,
            ),
        ]
        blockers: list[ExternalRuntimeBlocker] = []
        second_host = self._second_host_receipt(source_head)
        if second_host is None:
            blockers.append(
                ExternalRuntimeBlocker(
                    id="second_physical_host",
                    reason="No digest-bound receipt from a distinct physical host was supplied.",
                    exact_resumption_contract=(
                        "Run this benchmark from the same source Git SHA on a distinct "
                        "physical host, copy its distributed-runtime.receipt.json, set "
                        "BVMCP_SECOND_HOST_RECEIPT to that absolute path and "
                        "BVMCP_SECOND_HOST_RECEIPT_SHA256 to its SHA-256, then rerun."
                    ),
                    environment_variables=[
                        "BVMCP_SECOND_HOST_RECEIPT",
                        "BVMCP_SECOND_HOST_RECEIPT_SHA256",
                    ],
                )
            )
        else:
            assertions.append(
                _assertion(
                    "second_physical_host",
                    "distinct source-bound host receipt",
                    second_host,
                    True,
                )
            )
            runtimes["second_physical_host"] = second_host
        webgpu = browsers["chromium"]["webgpu"]
        if not webgpu.get("adapter"):
            blockers.append(
                ExternalRuntimeBlocker(
                    id="webgpu_adapter",
                    reason=(
                        "Installed headless Chromium did not expose a requestable WebGPU adapter."
                    ),
                    exact_resumption_contract=(
                        "Rerun on a host/browser configuration where navigator.gpu exists "
                        "and requestAdapter() returns an adapter without unsafe emulation flags; "
                        "preserve the resulting source-bound receipt."
                    ),
                    environment_variables=[],
                )
            )
        else:
            assertions.append(
                _assertion("webgpu_adapter", True, webgpu.get("adapter"), True)
            )
        return assertions, blockers

    @staticmethod
    def _second_host_receipt(source_head: str) -> dict[str, Any] | None:
        supplied = os.environ.get("BVMCP_SECOND_HOST_RECEIPT")
        expected_digest = os.environ.get("BVMCP_SECOND_HOST_RECEIPT_SHA256")
        if not supplied and not expected_digest:
            return None
        if not supplied or not expected_digest:
            raise DistributedRuntimeError(
                "second-host receipt path and SHA-256 must be supplied together"
            )
        path = Path(supplied).expanduser()
        if not path.is_absolute() or path.is_symlink() or not path.is_file():
            raise DistributedRuntimeError(
                "second-host receipt must be an absolute non-symlink file"
            )
        observed_digest = sha256_file(path)[0]
        if observed_digest != expected_digest:
            raise DistributedRuntimeError("second-host receipt digest mismatch")
        receipt = DistributedRuntimeReceipt.model_validate_json(
            path.read_text(encoding="utf-8")
        )
        if receipt.source_git_head != source_head:
            raise DistributedRuntimeError("second-host receipt source revision mismatch")
        if receipt.functional_passed is not True:
            raise DistributedRuntimeError("second-host receipt did not functionally pass")
        local_identity = _host_identity()
        if receipt.local_host_identity_sha256 == local_identity:
            raise DistributedRuntimeError("second-host receipt came from this physical host")
        return {
            "receipt_sha256": observed_digest,
            "host_identity_sha256": receipt.local_host_identity_sha256,
            "source_git_head": receipt.source_git_head,
            "functional_passed": receipt.functional_passed,
        }
