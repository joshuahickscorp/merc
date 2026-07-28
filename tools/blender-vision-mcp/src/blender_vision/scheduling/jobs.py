from __future__ import annotations

import asyncio
import inspect
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, TypeVar

from blender_vision.core.errors import JobCancelled
from blender_vision.core.models import JobStatus
from blender_vision.projects.store import ProjectStore

Result = TypeVar("Result", bound=dict[str, Any])
JobOperation = Callable[["JobContext"], Result | Awaitable[Result]]


@dataclass(slots=True)
class JobContext:
    project: ProjectStore
    job_id: str

    def check_cancelled(self) -> None:
        if self.project.cancellation_requested(self.job_id):
            raise JobCancelled(f"job cancelled: {self.job_id}")

    def progress(self, stage: str, **payload: Any) -> None:
        self.check_cancelled()
        self.project.add_job_event(self.job_id, "progress", {"stage": stage, **payload})


class JobManager:
    def __init__(self, project: ProjectStore):
        self.project = project
        self._tasks: dict[str, asyncio.Task[dict[str, Any]]] = {}

    def submit(
        self,
        operation_name: str,
        operation: JobOperation,
        *,
        config: dict[str, Any],
        cache_key: str | None = None,
        timeout_seconds: float | None = None,
    ) -> str:
        job_id = self.project.add_job(operation_name, config, cache_key)
        task = asyncio.create_task(
            self._run(job_id, operation_name, operation, cache_key, timeout_seconds),
            name=f"bvmcp:{operation_name}:{job_id}",
        )
        self._tasks[job_id] = task
        task.add_done_callback(lambda _task: self._tasks.pop(job_id, None))
        return job_id

    async def _run(
        self,
        job_id: str,
        operation_name: str,
        operation: JobOperation,
        cache_key: str | None,
        timeout_seconds: float | None,
    ) -> dict[str, Any]:
        if cache_key and (cached := self.project.cached(cache_key)) is not None:
            result = {**cached, "cache_hit": True}
            self.project.update_job(job_id, JobStatus.SUCCEEDED, result=result)
            return result
        self.project.update_job(job_id, JobStatus.RUNNING)
        context = JobContext(self.project, job_id)
        try:
            if inspect.iscoroutinefunction(operation):
                awaitable = operation(context)
            else:
                awaitable = asyncio.to_thread(operation, context)
            result = await asyncio.wait_for(awaitable, timeout=timeout_seconds)
            context.check_cancelled()
            if cache_key:
                self.project.put_cache(cache_key, operation_name, result)
            self.project.update_job(job_id, JobStatus.SUCCEEDED, result=result)
            return result
        except TimeoutError as error:
            detail = {"type": type(error).__name__, "message": "job timed out"}
            self.project.update_job(job_id, JobStatus.TIMED_OUT, error=detail)
            raise
        except (JobCancelled, asyncio.CancelledError) as error:
            detail = {"type": type(error).__name__, "message": str(error)}
            self.project.update_job(job_id, JobStatus.CANCELLED, error=detail)
            raise
        except Exception as error:
            detail = {"type": type(error).__name__, "message": str(error)}
            self.project.update_job(job_id, JobStatus.FAILED, error=detail)
            raise

    async def result(self, job_id: str) -> dict[str, Any]:
        task = self._tasks.get(job_id)
        if task is not None:
            return await task
        job = self.project.job(job_id)
        if job["status"] == JobStatus.SUCCEEDED.value:
            return job["result"]
        raise RuntimeError(f"job {job_id} finished with {job['status']}: {job['error']}")

    def cancel(self, job_id: str) -> None:
        self.project.request_cancel(job_id)
        task = self._tasks.get(job_id)
        if task is not None:
            task.cancel()
