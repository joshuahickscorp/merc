from __future__ import annotations

import threading
import time
from typing import Any

from blender_vision.core.errors import JobCancelled, ProjectError
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator, _recorded_hashes
from blender_vision.scheduling.distributed import DistributedScheduler


class WorkerRuntime:
    """Run authenticated pull leases against a locally mounted portable project.

    Network workers use the equivalent MCP methods and artifact-transfer protocol. This
    runtime is the packaged executable path for a trusted machine with project storage
    mounted locally; it never accepts an arbitrary command or Python entry point.
    """

    def __init__(
        self,
        project: ProjectStore,
        worker_id: str,
        worker_token: str,
        *,
        lease_seconds: int = 120,
    ) -> None:
        if not worker_id.strip() or not worker_token.strip():
            raise ValueError("worker runtime requires an id and token")
        if not 15 <= lease_seconds <= 3600:
            raise ValueError("worker lease must be between 15 and 3600 seconds")
        self.project = project
        self.worker_id = worker_id
        self.worker_token = worker_token
        self.lease_seconds = lease_seconds
        self.scheduler = DistributedScheduler(project)
        self.coordinator = Coordinator(project)

    def _artifact_digests(self) -> list[str]:
        with self.project.connection() as connection:
            return [row[0] for row in connection.execute("SELECT digest FROM artifacts")]

    def _heartbeat(self, current_jobs: int) -> None:
        self.scheduler.heartbeat(
            self.worker_id,
            self.worker_token,
            load={"current_jobs": current_jobs, "queue_length": 0, "warm_models": []},
            artifact_digests=self._artifact_digests(),
        )

    def _known_result_artifacts(self, result: dict[str, Any]) -> list[str]:
        candidates = _recorded_hashes(result)
        if not candidates:
            return []
        placeholders = ",".join("?" for _ in candidates)
        with self.project.connection() as connection:
            return sorted(
                row[0]
                for row in connection.execute(
                    f"SELECT digest FROM artifacts WHERE digest IN ({placeholders})", candidates
                ).fetchall()
            )

    def run_once(self) -> dict[str, Any]:
        self._heartbeat(0)
        lease = self.scheduler.claim(
            self.worker_id,
            self.worker_token,
            lease_seconds=self.lease_seconds,
        )
        if lease is None:
            return {"claimed": False, "worker_id": self.worker_id}
        self._heartbeat(1)
        stop_renewal = threading.Event()
        renewal_errors: list[Exception] = []

        def renew() -> None:
            interval = max(5.0, min(60.0, self.lease_seconds / 3.0))
            while not stop_renewal.wait(interval):
                try:
                    self.scheduler.renew(
                        self.worker_id,
                        self.worker_token,
                        lease["job_id"],
                        lease["lease_token"],
                        lease_seconds=self.lease_seconds,
                    )
                except Exception as error:  # the main thread surfaces the lease failure
                    renewal_errors.append(error)
                    stop_renewal.set()

        renewal_thread = threading.Thread(
            target=renew,
            name=f"bvmcp-lease-{lease['job_id']}",
            daemon=True,
        )
        renewal_thread.start()
        cache_hit = False
        try:
            job = self.project.job(lease["job_id"])
            cached = self.project.cached(job["cache_key"]) if job.get("cache_key") else None
            if cached is not None:
                result = cached
                cache_hit = True
            else:
                result = self.coordinator.dispatch_leased_job(
                    lease["operation"], lease["configuration"], lease["job_id"]
                )
            stop_renewal.set()
            renewal_thread.join(timeout=2.0)
            if renewal_errors:
                raise ProjectError(f"lease renewal failed: {renewal_errors[0]}")
            completed = self.scheduler.complete(
                self.worker_id,
                self.worker_token,
                lease["job_id"],
                lease["lease_token"],
                result=result,
                output_artifact_digests=self._known_result_artifacts(result),
                cache_hit=cache_hit,
            )
            self.coordinator.record_leased_result(
                lease["job_id"], completed["result"], cache_hit=cache_hit
            )
            return {
                "claimed": True,
                "job_id": lease["job_id"],
                "operation": lease["operation"],
                "attempt": lease["attempt"],
                "status": completed["status"],
                "cache_hit": cache_hit,
            }
        except Exception as error:
            stop_renewal.set()
            renewal_thread.join(timeout=2.0)
            retryable = not isinstance(
                error, (JobCancelled, KeyError, PermissionError, ValueError)
            )
            failed = self.scheduler.fail(
                self.worker_id,
                self.worker_token,
                lease["job_id"],
                lease["lease_token"],
                error={"type": type(error).__name__, "message": str(error)},
                retryable=retryable,
            )
            self.project.record_job_provenance(
                lease["job_id"],
                execution={
                    "status": failed["status"],
                    "worker_id": self.worker_id,
                    "attempt": lease["attempt"],
                },
                failure_class=type(error).__name__,
            )
            return {
                "claimed": True,
                "job_id": lease["job_id"],
                "operation": lease["operation"],
                "attempt": lease["attempt"],
                "status": failed["status"],
                "error": {"type": type(error).__name__, "message": str(error)},
            }
        finally:
            self._heartbeat(0)

    def run(self, *, once: bool = False, poll_seconds: float = 1.0) -> dict[str, Any]:
        if not 0.05 <= poll_seconds <= 60.0:
            raise ValueError("worker poll interval must be between 0.05 and 60 seconds")
        processed = 0
        while True:
            report = self.run_once()
            processed += int(report["claimed"])
            if once:
                return {**report, "processed": processed}
            if not report["claimed"]:
                time.sleep(poll_seconds)
