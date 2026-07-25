from __future__ import annotations

import hashlib
import hmac
import json
import math
import secrets
import uuid
from datetime import UTC, datetime, timedelta
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import JobStatus
from blender_vision.core.util import utc_now
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.projects.store import ProjectStore

WORKER_CLASSES = {
    "blender",
    "vision",
    "optimization",
    "training",
    "generative",
    "render",
    "review",
}
MAX_RESULT_BYTES = 16 * 1024 * 1024


def _hash_secret(secret: str) -> str:
    return hashlib.sha256(secret.encode()).hexdigest()


def _timestamp_after(seconds: int) -> str:
    return (datetime.now(UTC) + timedelta(seconds=seconds)).isoformat()


def _expired(value: str) -> bool:
    return datetime.fromisoformat(value) <= datetime.now(UTC)


def operation_requirements(
    operation: str,
    *,
    input_artifact_digests: list[str] | None = None,
    overrides: dict[str, Any] | None = None,
) -> dict[str, Any]:
    if operation.startswith("blender.") or operation in {
        "project.audit",
        "component.generate",
        "portfolio.execute_parametric_seed",
        "portfolio.execute_initial",
        "repair.apply",
        "benchmark.revise_rtx_5090_fe_candidate",
        "benchmark.refine_rtx_5090_fe_visual_candidate",
        "benchmark.refine_rtx_5090_fe_front_frame_candidate",
        "benchmark.refine_dgx_spark_visual_candidate",
        "benchmark.refine_dgx_spark_base_foot_candidate",
        "vision.refine_camera",
        "workflow.deliver_promoted",
    }:
        worker_classes = ["blender"]
        preferred_hardware = ["metal", "optix", "cuda"]
    elif operation.startswith(("vision.", "evidence.")):
        worker_classes = ["vision"]
        preferred_hardware = ["cuda", "mps", "cpu"]
    elif operation.startswith("optimization.") or operation == "component.fit":
        worker_classes = ["optimization"]
        preferred_hardware = ["cuda", "mps", "cpu"]
    elif operation.startswith("dataset."):
        worker_classes = ["training", "blender"]
        preferred_hardware = ["cuda", "metal", "mps"]
    elif operation.startswith("training."):
        worker_classes = ["training"]
        preferred_hardware = ["cuda", "mps"]
    elif operation.startswith("generative3d."):
        worker_classes = ["generative"]
        preferred_hardware = ["cuda", "mps"]
    elif operation.startswith("validation."):
        worker_classes = ["render", "blender", "vision"]
        preferred_hardware = ["optix", "metal", "cuda", "cpu"]
    else:
        worker_classes = ["review"]
        preferred_hardware = ["cpu"]
    requirements = {
        "worker_classes": worker_classes,
        "required_capabilities": [operation],
        "preferred_hardware": preferred_hardware,
        "required_models": [],
        "min_vram_gb": 0.0,
        "input_artifact_digests": sorted(set(input_artifact_digests or [])),
        "max_attempts": 3,
    }
    requirements.update(overrides or {})
    return requirements


class DistributedScheduler:
    """Authenticated worker registry, hardware routing, leases, and fault recovery."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def register(
        self, name: str, worker_class: str, capabilities: dict[str, Any]
    ) -> dict[str, Any]:
        if not name.strip() or worker_class not in WORKER_CLASSES:
            raise ValueError("worker requires a name and supported worker class")
        normalized = self._validate_capabilities(capabilities)
        worker_id = str(uuid.uuid4())
        worker_token = secrets.token_urlsafe(32)
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO workers(id,name,worker_class,auth_token_hash,capabilities_json,"
                "load_json,status,registered_at,last_heartbeat) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    worker_id,
                    name.strip(),
                    worker_class,
                    _hash_secret(worker_token),
                    json.dumps(normalized),
                    json.dumps({"current_jobs": 0, "queue_length": 0}),
                    "available",
                    now,
                    now,
                ),
            )
        return {**self.get(worker_id), "worker_token": worker_token}

    def authenticate(self, worker_id: str, worker_token: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM workers WHERE id=?", (worker_id,)).fetchone()
        if row is None or not hmac.compare_digest(
            row["auth_token_hash"], _hash_secret(worker_token)
        ):
            raise PermissionError("invalid worker credentials")
        return self._row(row)

    def heartbeat(
        self,
        worker_id: str,
        worker_token: str,
        *,
        load: dict[str, Any],
        artifact_digests: list[str] | None = None,
    ) -> dict[str, Any]:
        self.authenticate(worker_id, worker_token)
        normalized_load = self._validate_load(load)
        digests = sorted(set(artifact_digests or []))
        with self.project.connection() as connection:
            known = set()
            if digests:
                placeholders = ",".join("?" for _ in digests)
                known = {
                    row[0]
                    for row in connection.execute(
                        f"SELECT digest FROM artifacts WHERE digest IN ({placeholders})",
                        digests,
                    ).fetchall()
                }
            now = utc_now()
            connection.execute(
                "UPDATE workers SET load_json=?,status='available',last_heartbeat=? WHERE id=?",
                (json.dumps(normalized_load), now, worker_id),
            )
            connection.execute("DELETE FROM artifact_locations WHERE worker_id=?", (worker_id,))
            connection.executemany(
                "INSERT OR REPLACE INTO artifact_locations(digest,worker_id,updated_at) "
                "VALUES(?,?,?)",
                [(digest, worker_id, now) for digest in sorted(known)],
            )
        return self.get(worker_id)

    def set_requirements(self, job_id: str, requirements: dict[str, Any]) -> None:
        self._validate_requirements(requirements)
        with self.project.connection() as connection:
            if connection.execute("SELECT 1 FROM jobs WHERE id=?", (job_id,)).fetchone() is None:
                raise KeyError(f"unknown job: {job_id}")
            connection.execute(
                "INSERT OR REPLACE INTO job_requirements(job_id,requirements_json) VALUES(?,?)",
                (job_id, json.dumps(requirements)),
            )

    def claim(
        self,
        worker_id: str,
        worker_token: str,
        *,
        lease_seconds: int = 120,
    ) -> dict[str, Any] | None:
        worker = self.authenticate(worker_id, worker_token)
        if not 15 <= lease_seconds <= 3600:
            raise ValueError("worker lease must be between 15 and 3600 seconds")
        self.reap_expired()
        if self._worker_offline(worker):
            raise ProjectError("worker heartbeat is stale")
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            candidates = connection.execute(
                "SELECT j.id,j.operation,j.config_json,j.created_at,r.requirements_json "
                "FROM jobs j LEFT JOIN job_requirements r ON r.job_id=j.id "
                "WHERE j.status=? AND j.cancel_requested=0 ORDER BY j.created_at LIMIT 128",
                (JobStatus.QUEUED.value,),
            ).fetchall()
            locations = {
                row[0]
                for row in connection.execute(
                    "SELECT digest FROM artifact_locations WHERE worker_id=?", (worker_id,)
                )
            }
            ranked: list[tuple[float, int, Any, dict[str, Any]]] = []
            for ordinal, candidate in enumerate(candidates):
                requirements = (
                    json.loads(candidate["requirements_json"])
                    if candidate["requirements_json"]
                    else operation_requirements(candidate["operation"])
                )
                self._validate_requirements(requirements)
                if self._eligible(worker, requirements):
                    ranked.append(
                        (
                            self._score(worker, requirements, locations),
                            -ordinal,
                            candidate,
                            requirements,
                        )
                    )
            if not ranked:
                return None
            _score, _ordinal, selected, requirements = max(ranked, key=lambda item: item[:2])
            previous = connection.execute(
                "SELECT attempt FROM job_leases WHERE job_id=?", (selected["id"],)
            ).fetchone()
            attempt = int(previous[0]) + 1 if previous else 1
            lease_token = secrets.token_urlsafe(32)
            now = utc_now()
            expires_at = _timestamp_after(lease_seconds)
            updated = connection.execute(
                "UPDATE jobs SET status=?,started_at=? WHERE id=? AND status=?",
                (JobStatus.RUNNING.value, now, selected["id"], JobStatus.QUEUED.value),
            )
            if updated.rowcount != 1:
                return None
            connection.execute(
                "INSERT OR REPLACE INTO job_leases(job_id,worker_id,lease_token_hash,attempt,"
                "leased_at,expires_at) VALUES(?,?,?,?,?,?)",
                (
                    selected["id"],
                    worker_id,
                    _hash_secret(lease_token),
                    attempt,
                    now,
                    expires_at,
                ),
            )
        self.project.add_job_event(
            selected["id"],
            "leased",
            {"worker_id": worker_id, "attempt": attempt, "expires_at": expires_at},
        )
        return {
            "schema_version": 1,
            "job_id": selected["id"],
            "operation": selected["operation"],
            "configuration": json.loads(selected["config_json"]),
            "requirements": requirements,
            "input_artifact_digests": requirements.get("input_artifact_digests", []),
            "lease_token": lease_token,
            "lease_expires_at": expires_at,
            "attempt": attempt,
            "project_id": self.project.project()["id"],
        }

    def renew(
        self,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        *,
        lease_seconds: int = 120,
    ) -> dict[str, Any]:
        self.authenticate(worker_id, worker_token)
        if not 15 <= lease_seconds <= 3600:
            raise ValueError("worker lease must be between 15 and 3600 seconds")
        lease = self._lease(worker_id, job_id, lease_token)
        if _expired(lease["expires_at"]):
            raise ProjectError("job lease has expired")
        expires_at = _timestamp_after(lease_seconds)
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE job_leases SET expires_at=? WHERE job_id=?", (expires_at, job_id)
            )
        self.project.add_job_event(job_id, "lease_renewed", {"expires_at": expires_at})
        return {"job_id": job_id, "lease_expires_at": expires_at}

    def complete(
        self,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        *,
        result: dict[str, Any],
        output_artifact_digests: list[str] | None = None,
        cache_hit: bool = False,
    ) -> dict[str, Any]:
        self.authenticate(worker_id, worker_token)
        lease = self._lease(worker_id, job_id, lease_token)
        if _expired(lease["expires_at"]):
            raise ProjectError("job lease has expired")
        if len(json.dumps(result, sort_keys=True).encode()) > MAX_RESULT_BYTES:
            raise ValueError("worker result exceeds the 16 MiB coordinator limit")
        outputs = sorted(set(output_artifact_digests or []))
        with self.project.connection() as connection:
            known = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
            if not set(outputs).issubset(known):
                raise ValueError("worker completion references unknown output artifacts")
            job = connection.execute(
                "SELECT operation,config_json,status,cache_key FROM jobs WHERE id=?", (job_id,)
            ).fetchone()
        if job is None or job["status"] != JobStatus.RUNNING.value:
            raise ProjectError("job is not running under this lease")
        if cache_hit:
            cached = self.project.cached(job["cache_key"]) if job["cache_key"] else None
            if cached is None or cached != result:
                raise ValueError("worker cache-hit completion does not match coordinator cache")
            governed_result = None
        else:
            governed_result = self._completion_hook(
                job["operation"], json.loads(job["config_json"]), result, outputs
            )
        with self.project.connection() as connection:
            final_result = {
                **result,
                "cache_hit": cache_hit,
                "worker": {"id": worker_id, "attempt": lease["attempt"]},
                "output_artifact_digests": outputs,
            }
            if governed_result is not None:
                final_result["governed_result"] = governed_result
            updated = connection.execute(
                "UPDATE jobs SET status=?,result_json=?,error_json=NULL,finished_at=? "
                "WHERE id=? AND status=?",
                (
                    JobStatus.SUCCEEDED.value,
                    json.dumps(final_result),
                    utc_now(),
                    job_id,
                    JobStatus.RUNNING.value,
                ),
            )
            if updated.rowcount != 1:
                raise ProjectError("job is not running under this lease")
            connection.execute("DELETE FROM job_leases WHERE job_id=?", (job_id,))
            job = connection.execute(
                "SELECT cache_key,operation FROM jobs WHERE id=?", (job_id,)
            ).fetchone()
            if job and job["cache_key"]:
                connection.execute(
                    "INSERT OR REPLACE INTO cache_entries(cache_key,operation,result_json,"
                    "created_at) VALUES(?,?,?,?)",
                    (job["cache_key"], job["operation"], json.dumps(final_result), utc_now()),
                )
        self.project.add_job_event(job_id, "succeeded", final_result)
        return self.project.job(job_id)

    def _completion_hook(
        self,
        operation: str,
        configuration: dict[str, Any],
        result: dict[str, Any],
        output_digests: list[str],
    ) -> dict[str, Any] | None:
        if operation == "dataset.generate":
            sample_count = int(result.get("sample_count", 0))
            return DatasetStore(self.project).mark_generated(
                configuration["dataset_id"],
                artifact_digests=output_digests,
                sample_count=sample_count,
            )
        if operation == "training.execute":
            checkpoint_digest = str(result.get("checkpoint_digest", ""))
            if checkpoint_digest not in output_digests:
                raise ValueError("training completion must declare its checkpoint output digest")
            checkpoint = ArtifactStore(self.project).path_for(checkpoint_digest)
            return TrainingStore(self.project).import_result(
                configuration["training_run_id"],
                checkpoint,
                metrics=dict(result.get("metrics") or {}),
                license_record=dict(result.get("license_record") or {}),
                model_revision=str(result.get("model_revision", "")),
            )
        if operation == "generative3d.execute":
            declared = {
                *result.get("mesh_digests", []),
                *result.get("texture_digests", []),
                *result.get("image_digests", []),
                *dict(result.get("pbr_channels") or {}).values(),
            }
            if not declared or not declared.issubset(output_digests):
                raise ValueError(
                    "generative completion outputs must be declared worker artifacts"
                )
            return GenerativeProposalStore(self.project).import_result(
                configuration["request_id"],
                mesh_digests=list(result.get("mesh_digests") or []),
                texture_digests=list(result.get("texture_digests") or []),
                image_digests=list(result.get("image_digests") or []),
                pbr_channels=dict(result.get("pbr_channels") or {}),
                backend_identity=str(result.get("backend_identity", "")),
                checkpoint=str(result.get("checkpoint", "")),
                input_reference_ids=list(result.get("input_reference_ids") or []),
                generation_seed=result.get("generation_seed"),
                confidence=result.get("confidence"),
                known_limitations=list(result.get("known_limitations") or []),
            )
        return None

    def fail(
        self,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        *,
        error: dict[str, Any],
        retryable: bool = True,
    ) -> dict[str, Any]:
        self.authenticate(worker_id, worker_token)
        lease = self._lease(worker_id, job_id, lease_token)
        requirements = self.requirements(job_id)
        retry = retryable and lease["attempt"] < int(requirements.get("max_attempts", 3))
        status = JobStatus.QUEUED if retry else JobStatus.FAILED
        with self.project.connection() as connection:
            job = connection.execute(
                "SELECT operation,config_json FROM jobs WHERE id=?", (job_id,)
            ).fetchone()
            updated = connection.execute(
                "UPDATE jobs SET status=?,error_json=?,"
                "started_at=CASE WHEN ? THEN NULL ELSE started_at END,finished_at=? "
                "WHERE id=? AND status=?",
                (
                    status.value,
                    json.dumps(error),
                    retry,
                    None if retry else utc_now(),
                    job_id,
                    JobStatus.RUNNING.value,
                ),
            )
            if updated.rowcount != 1:
                raise ProjectError("job is not running under this lease")
            if not retry:
                connection.execute("DELETE FROM job_leases WHERE job_id=?", (job_id,))
        if not retry and job and job["operation"] == "generative3d.execute":
            GenerativeProposalStore(self.project).mark_failed(
                json.loads(job["config_json"])["request_id"], error=error
            )
        self.project.add_job_event(
            job_id,
            "retry_queued" if retry else "failed",
            {"worker_id": worker_id, "attempt": lease["attempt"], "error": error},
        )
        return self.project.job(job_id)

    def reap_expired(self) -> dict[str, int]:
        requeued = 0
        failed = 0
        events: list[tuple[str, str, dict[str, Any]]] = []
        failed_generative_requests: list[str] = []
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT l.*,r.requirements_json,j.operation,j.config_json FROM job_leases l "
                "JOIN jobs j ON j.id=l.job_id AND j.status=? "
                "LEFT JOIN job_requirements r ON r.job_id=l.job_id",
                (JobStatus.RUNNING.value,),
            ).fetchall()
            for row in rows:
                if not _expired(row["expires_at"]):
                    continue
                requirements = (
                    json.loads(row["requirements_json"])
                    if row["requirements_json"]
                    else {"max_attempts": 3}
                )
                retry = row["attempt"] < int(requirements.get("max_attempts", 3))
                status = JobStatus.QUEUED if retry else JobStatus.FAILED
                connection.execute(
                    "UPDATE jobs SET status=?,error_json=?,"
                    "started_at=CASE WHEN ? THEN NULL ELSE started_at END,finished_at=? "
                    "WHERE id=?",
                    (
                        status.value,
                        json.dumps({"type": "LeaseExpired", "attempt": row["attempt"]}),
                        retry,
                        None if retry else utc_now(),
                        row["job_id"],
                    ),
                )
                events.append(
                    (
                        row["job_id"],
                        "lease_expired_retry" if retry else "lease_expired_failed",
                        {"worker_id": row["worker_id"], "attempt": row["attempt"]},
                    )
                )
                if retry:
                    requeued += 1
                else:
                    connection.execute("DELETE FROM job_leases WHERE job_id=?", (row["job_id"],))
                    if row["operation"] == "generative3d.execute":
                        failed_generative_requests.append(
                            json.loads(row["config_json"])["request_id"]
                        )
                    failed += 1
        for request_id in failed_generative_requests:
            GenerativeProposalStore(self.project).mark_failed(
                request_id, error={"type": "LeaseExpired"}
            )
        for job_id, event, payload in events:
            self.project.add_job_event(job_id, event, payload)
        return {"requeued": requeued, "failed": failed}

    def requirements(self, job_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT j.operation,r.requirements_json FROM jobs j "
                "LEFT JOIN job_requirements r ON r.job_id=j.id WHERE j.id=?",
                (job_id,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown job: {job_id}")
        return (
            json.loads(row["requirements_json"])
            if row["requirements_json"]
            else operation_requirements(row["operation"])
        )

    def get(self, worker_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM workers WHERE id=?", (worker_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown worker: {worker_id}")
        return self._row(row)

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute("SELECT * FROM workers ORDER BY registered_at").fetchall()
        return [self._row(row) for row in rows]

    def snapshot(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            leases = [
                dict(row)
                for row in connection.execute(
                    "SELECT l.job_id,l.worker_id,l.attempt,l.leased_at,l.expires_at,j.operation "
                    "FROM job_leases l JOIN jobs j ON j.id=l.job_id "
                    "WHERE j.status=? ORDER BY l.leased_at",
                    (JobStatus.RUNNING.value,),
                )
            ]
            queued = connection.execute(
                "SELECT COUNT(*) FROM jobs WHERE status=?", (JobStatus.QUEUED.value,)
            ).fetchone()[0]
            locations = connection.execute("SELECT COUNT(*) FROM artifact_locations").fetchone()[0]
        return {
            "workers": self.list(),
            "active_leases": leases,
            "queued_jobs": queued,
            "artifact_locations": locations,
        }

    def _lease(self, worker_id: str, job_id: str, lease_token: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM job_leases WHERE job_id=? AND worker_id=?",
                (job_id, worker_id),
            ).fetchone()
        if row is None or not hmac.compare_digest(
            row["lease_token_hash"], _hash_secret(lease_token)
        ):
            raise PermissionError("invalid job lease")
        return dict(row)

    @staticmethod
    def _validate_capabilities(value: dict[str, Any]) -> dict[str, Any]:
        required = {
            "hardware",
            "vram_gb",
            "system_memory_gb",
            "supported_models",
            "render_devices",
            "capabilities",
        }
        if not required.issubset(value):
            raise ValueError("worker capabilities are incomplete")
        for field in ("hardware", "supported_models", "render_devices", "capabilities"):
            if not isinstance(value[field], list) or not all(
                isinstance(item, str) and item for item in value[field]
            ):
                raise ValueError(f"worker {field} must be a string list")
        for field in ("vram_gb", "system_memory_gb"):
            number = float(value[field])
            if not math.isfinite(number) or number < 0:
                raise ValueError(f"worker {field} must be finite and non-negative")
        return {
            **value,
            "vram_gb": float(value["vram_gb"]),
            "system_memory_gb": float(value["system_memory_gb"]),
            "blender_version": value.get("blender_version"),
        }

    @staticmethod
    def _validate_load(value: dict[str, Any]) -> dict[str, Any]:
        current_jobs = int(value.get("current_jobs", 0))
        queue_length = int(value.get("queue_length", 0))
        if current_jobs < 0 or queue_length < 0:
            raise ValueError("worker load counters cannot be negative")
        return {
            **value,
            "current_jobs": current_jobs,
            "queue_length": queue_length,
            "warm_models": [str(item) for item in value.get("warm_models", [])],
        }

    @staticmethod
    def _validate_requirements(value: dict[str, Any]) -> None:
        classes = value.get("worker_classes")
        valid = isinstance(classes, list) and classes and set(classes).issubset(WORKER_CLASSES)
        if not valid:
            raise ValueError("job requirements contain invalid worker classes")
        for field in (
            "required_capabilities",
            "preferred_hardware",
            "preferred_models",
            "required_models",
        ):
            items = value.get(field, [])
            if not isinstance(items, list) or not all(
                isinstance(item, str) and item for item in items
            ):
                raise ValueError(f"job {field} must be a string list")
        digests = value.get("input_artifact_digests", [])
        if not isinstance(digests, list) or not all(
            isinstance(item, str)
            and len(item) == 64
            and all(character in "0123456789abcdef" for character in item)
            for item in digests
        ):
            raise ValueError("job input artifacts must be SHA-256 digests")
        min_vram = float(value.get("min_vram_gb", 0.0))
        if not math.isfinite(min_vram) or min_vram < 0:
            raise ValueError("min_vram_gb must be finite and non-negative")
        if int(value.get("max_attempts", 3)) not in range(1, 11):
            raise ValueError("max_attempts must be between 1 and 10")

    @staticmethod
    def _supports(capabilities: list[str], required: str) -> bool:
        return required in capabilities or any(
            item.endswith(".*") and required.startswith(item[:-1]) for item in capabilities
        )

    def _eligible(self, worker: dict[str, Any], requirements: dict[str, Any]) -> bool:
        capabilities = worker["capabilities"]
        return bool(
            worker["worker_class"] in requirements["worker_classes"]
            and capabilities["vram_gb"] >= float(requirements.get("min_vram_gb", 0.0))
            and all(
                self._supports(capabilities["capabilities"], item)
                for item in requirements.get("required_capabilities", [])
            )
            and set(requirements.get("required_models", [])).issubset(
                capabilities["supported_models"]
            )
            and worker["status"] == "available"
        )

    @staticmethod
    def _score(worker: dict[str, Any], requirements: dict[str, Any], locations: set[str]) -> float:
        capabilities = worker["capabilities"]
        load = worker["load"]
        locality = len(set(requirements.get("input_artifact_digests", [])) & locations)
        preferred = requirements.get("preferred_hardware", [])
        hardware_score = max(
            (
                len(preferred) - preferred.index(item)
                for item in capabilities["hardware"]
                if item in preferred
            ),
            default=0,
        )
        warm = len(set(requirements.get("preferred_models", [])) & set(load.get("warm_models", [])))
        return (
            locality * 20.0
            + hardware_score * 5.0
            + warm * 8.0
            + capabilities["vram_gb"] * 0.05
            - float(load.get("current_jobs", 0)) * 10.0
            - float(load.get("queue_length", 0)) * 2.0
        )

    @staticmethod
    def _worker_offline(worker: dict[str, Any]) -> bool:
        return datetime.fromisoformat(worker["last_heartbeat"]) < datetime.now(UTC) - timedelta(
            minutes=2
        )

    def _row(self, row: Any) -> dict[str, Any]:
        value = dict(row)
        value.pop("auth_token_hash")
        value["capabilities"] = json.loads(value.pop("capabilities_json"))
        value["load"] = json.loads(value.pop("load_json"))
        if self._worker_offline(value):
            value["status"] = "offline"
        return value
