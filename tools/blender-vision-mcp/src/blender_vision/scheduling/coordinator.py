from __future__ import annotations

import json
import re
import time
from typing import Any

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.acceptance.receipts import evaluate_acceptance, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.consensus import CameraConsensus
from blender_vision.cameras.refinement import CameraRefiner
from blender_vision.core.errors import JobCancelled
from blender_vision.core.models import JobStatus
from blender_vision.core.util import canonical_json, hash_operation, runtime_revision, sha256_file
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.pursuit import EvidencePursuitStore
from blender_vision.geometry.portfolio_executor import PortfolioExecutor
from blender_vision.optimization.search import MultiviewSearchStore
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.distributed import DistributedScheduler, operation_requirements
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.workflows.service import ReconstructionService

CACHEABLE = {
    "project.audit",
    "blender.inspect",
    "blender.export",
    "blender.generate_lod",
    "vision.solve_cameras",
    "vision.run",
    "vision.compare_backends",
    "vision.compare_camera_solutions",
    "blender.render",
    "validation.compare",
    "validation.coverage",
}


def _recorded_hashes(value: Any) -> list[str]:
    hashes: set[str] = set()

    def visit(item: Any) -> None:
        if isinstance(item, dict):
            for nested in item.values():
                visit(nested)
        elif isinstance(item, list):
            for nested in item:
                visit(nested)
        elif isinstance(item, str) and re.fullmatch(r"[0-9a-f]{64}", item):
            hashes.add(item)

    visit(value)
    return sorted(hashes)


def _worker_records(value: Any) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []

    def visit(item: Any, key: str | None = None) -> None:
        if isinstance(item, dict):
            if key == "worker":
                records.append(item)
            for nested_key, nested in item.items():
                visit(nested, nested_key)
        elif isinstance(item, list):
            for nested in item:
                visit(nested, key)

    visit(value)
    return records


class Coordinator:
    """A single-user coordinator backed by each portable project's SQLite database."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.service = ReconstructionService(project)

    def _input_hashes(self, operation: str, config: dict[str, Any]) -> list[str]:
        hashes: list[str] = []
        with self.project.connection() as connection:
            if (
                operation.startswith("blender.")
                or operation
                in {
                    "project.audit",
                    "workflow.audit_reference_fidelity",
                    "workflow.deliver_promoted",
                    "benchmark.revise_rtx_5090_fe_candidate",
                    "benchmark.refine_rtx_5090_fe_visual_candidate",
                    "benchmark.refine_rtx_5090_fe_front_frame_candidate",
                    "benchmark.refine_dgx_spark_visual_candidate",
                    "benchmark.refine_dgx_spark_base_foot_candidate",
                }
                or operation.startswith("dataset.")
            ):
                hashes.extend(
                    row[0] for row in connection.execute("SELECT artifact_digest FROM scene_assets")
                )
            if operation.startswith(("dataset.", "training.")):
                hashes.extend(
                    row[0]
                    for row in connection.execute(
                        "SELECT record_digest FROM datasets ORDER BY created_at"
                    )
                )
            if operation.startswith("generative3d."):
                request_id = config.get("request_id")
                if request_id:
                    row = connection.execute(
                        "SELECT request_digest FROM generative_requests WHERE id=?",
                        (request_id,),
                    ).fetchone()
                    if row and row["request_digest"]:
                        hashes.append(row["request_digest"])
            if operation.startswith(("vision.", "validation.", "receipt.")) or operation.startswith(
                ("blender.", "generative3d.")
            ):
                hashes.extend(
                    row[0]
                    for row in connection.execute("SELECT artifact_digest FROM reference_items")
                )
            if operation.startswith(("blender.render", "validation.", "receipt.")):
                hashes.extend(
                    __import__("hashlib").sha256(row[0].encode()).hexdigest()
                    for row in connection.execute("SELECT solution_json FROM camera_solutions")
                )
            if operation == "vision.compare_camera_solutions":
                hashes.extend(
                    __import__("hashlib").sha256(row[0].encode()).hexdigest()
                    for row in connection.execute(
                        "SELECT solution_json FROM camera_solutions ORDER BY created_at"
                    )
                )
            if operation == "vision.run":
                hashes.extend(
                    __import__("hashlib").sha256(row[0].encode()).hexdigest()
                    for row in connection.execute(
                        "SELECT solution_json FROM camera_solutions ORDER BY created_at,id"
                    )
                )
                hashes.extend(
                    __import__("hashlib").sha256(
                        canonical_json(
                            {
                                "id": row["id"],
                                "reference_id": row["reference_id"],
                                "artifact_digest": row["artifact_digest"],
                                "source_artifact_digest": row["source_artifact_digest"],
                                "method": row["method"],
                                "reviewer": row["reviewer"],
                                "reason": row["reason"],
                                "creator": row["creator"],
                                "backend": row["backend"],
                                "revision": row["revision"],
                                "approval_state": row["approval_state"],
                                "confidence": row["confidence"],
                                "intended_use": row["intended_use"],
                                "visible_components_json": row["visible_components_json"],
                                "excluded_components_json": row["excluded_components_json"],
                                "roi_json": row["roi_json"],
                            }
                        )
                    ).hexdigest()
                    for row in connection.execute(
                        "SELECT * FROM reference_masks ORDER BY created_at,id"
                    )
                )
            if operation.startswith(("validation.", "receipt.")):
                hashes.extend(
                    digest
                    for row in connection.execute(
                        "SELECT render_digest, residual_digest FROM comparisons"
                    )
                    for digest in row
                    if digest
                )
            if operation == "vision.compare_backends":
                hashes.extend(
                    row[0]
                    for row in connection.execute(
                        "SELECT record_digest FROM geometry_runs ORDER BY created_at"
                    )
                )
        explicit = config.get("input_hashes", [])
        if isinstance(explicit, list):
            hashes.extend(str(item) for item in explicit)
        hashes.append(__import__("hashlib").sha256(canonical_json(config)).hexdigest())
        return sorted(set(hashes))

    def enqueue(self, operation: str, config: dict[str, Any] | None = None) -> str:
        config = dict(config or {})
        requirement_overrides = config.pop("worker_requirements", None)
        input_hashes = self._input_hashes(operation, config)
        cache_key = None
        if operation in CACHEABLE:
            cache_key = hash_operation(
                operation,
                input_hashes,
                config,
                runtime_revision(),
            )
        job_id = self.project.add_job(operation, config, cache_key)
        with self.project.connection() as connection:
            known_artifacts = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
        input_artifacts = sorted(set(input_hashes) & known_artifacts)
        requirements = operation_requirements(
            operation,
            input_artifact_digests=input_artifacts,
            overrides=requirement_overrides,
        )
        DistributedScheduler(self.project).set_requirements(job_id, requirements)
        self.project.record_job_provenance(
            job_id,
            input_hashes=input_hashes,
            backend={
                "operation": operation,
                "backend": config.get("backend"),
                "model_revision": config.get("model_revision"),
            },
            execution={"status": "queued", "requirements": requirements},
        )
        return job_id

    def run(self, operation: str, config: dict[str, Any] | None = None) -> dict[str, Any]:
        job_id = self.enqueue(operation, config)
        self.execute(job_id)
        return self.project.job(job_id)

    def execute(self, job_id: str) -> dict[str, Any]:
        job = self.project.job(job_id)
        if job["cancel_requested"]:
            self.project.update_job(job_id, JobStatus.CANCELLED, error={"type": "JobCancelled"})
            self.project.record_job_provenance(
                job_id,
                execution={"status": "cancelled", "cache_hit": False},
                failure_class="JobCancelled",
            )
            return self.project.job(job_id)
        if job["status"] == JobStatus.QUEUED.value:
            self.project.update_job(job_id, JobStatus.RUNNING)
        elif job["status"] != JobStatus.RUNNING.value:
            return job
        cache_key = job.get("cache_key")
        if cache_key:
            cached = self.project.cached(cache_key)
            if cached is not None:
                result = {**cached, "cache_hit": True}
                self.project.update_job(job_id, JobStatus.SUCCEEDED, result=result)
                self._record_result(job_id, result, cache_hit=True)
                return self.project.job(job_id)
        try:
            result = self._dispatch(job["operation"], job["config"], job_id)
            result["cache_hit"] = False
            if cache_key:
                self.project.put_cache(cache_key, job["operation"], result)
            self.project.update_job(job_id, JobStatus.SUCCEEDED, result=result)
            self._record_result(job_id, result, cache_hit=False)
        except JobCancelled as error:
            self.project.update_job(
                job_id,
                JobStatus.CANCELLED,
                error={"type": type(error).__name__, "message": str(error)},
            )
            self.project.record_job_provenance(
                job_id,
                execution={"status": "cancelled", "cache_hit": False},
                failure_class=type(error).__name__,
            )
        except TimeoutError as error:
            self.project.update_job(
                job_id,
                JobStatus.TIMED_OUT,
                error={"type": type(error).__name__, "message": str(error)},
            )
            self.project.record_job_provenance(
                job_id,
                execution={"status": "timed_out", "cache_hit": False},
                failure_class=type(error).__name__,
            )
        except Exception as error:
            self.project.update_job(
                job_id,
                JobStatus.FAILED,
                error={"type": type(error).__name__, "message": str(error)},
            )
            self.project.record_job_provenance(
                job_id,
                execution={"status": "failed", "cache_hit": False},
                failure_class=type(error).__name__,
            )
        return self.project.job(job_id)

    def _record_result(self, job_id: str, result: dict[str, Any], *, cache_hit: bool) -> None:
        workers = _worker_records(result)
        logs = sorted(
            {
                str(worker["log"])
                for worker in workers
                if isinstance(worker.get("log"), str)
            }
        )
        metrics = {
            key: result[key]
            for key in ("metrics", "acceptance", "coverage", "summary")
            if isinstance(result.get(key), dict)
        }
        self.project.record_job_provenance(
            job_id,
            backend={
                "operation": self.project.job(job_id)["operation"],
                "backend": result.get("backend"),
                "backend_version": result.get("backend_version"),
                "model_revision": result.get("model_revision"),
            },
            execution={"status": "succeeded", "cache_hit": cache_hit, "workers": workers},
            output_hashes=_recorded_hashes(result),
            metrics=metrics,
            logs=logs,
            failure_class=None,
        )

    def dispatch_leased_job(
        self, operation: str, config: dict[str, Any], job_id: str
    ) -> dict[str, Any]:
        """Execute an already-leased allowlisted operation without changing job state."""
        return self._dispatch(operation, config, job_id)

    def record_leased_result(
        self, job_id: str, result: dict[str, Any], *, cache_hit: bool
    ) -> None:
        """Persist the same provenance record used by coordinator-local execution."""
        self._record_result(job_id, result, cache_hit=cache_hit)

    def _dispatch(self, operation: str, config: dict[str, Any], job_id: str) -> dict[str, Any]:
        if operation == "scene.import":
            return self.service.import_scene(config["source"])
        if operation == "reference.import":
            return self.service.import_reference(
                config["source"],
                rights_state=config.get("rights_state", "UNKNOWN"),
                viewpoint_label=config.get("viewpoint_label"),
            )
        if operation == "evidence.discover":
            return SearchProviderStore(self.project).discover(
                config["provider_id"],
                target_id=config.get("target_id"),
                category=config.get("category", "general_product"),
                focus_terms=config.get("focus_terms"),
                maximum_queries=config.get("maximum_queries"),
                maximum_results_per_query=config.get("maximum_results_per_query"),
                timeout_seconds=float(config.get("timeout_seconds", 20.0)),
            )
        if operation == "evidence.pursue_missing":
            return EvidencePursuitStore(self.project).pursue(
                config.get("target_id"),
                category=config.get("category", "general_product"),
                provider_id=config.get("provider_id"),
                required_terms=config.get("required_terms"),
                maximum_queries=int(config.get("maximum_queries", 5)),
                maximum_results_per_query=int(
                    config.get("maximum_results_per_query", 10)
                ),
                timeout_seconds=float(config.get("timeout_seconds", 20.0)),
            )
        if operation == "blender.inspect":
            return self.service.inspect_scene(config.get("scene_id"), job_id=job_id)
        if operation == "project.audit":
            return self.service.audit_scene(config.get("scene_id"), job_id=job_id)
        if operation == "benchmark.revise_rtx_5090_fe_candidate":
            return self.service.revise_rtx_5090_fe_candidate(
                config["source_revision"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "benchmark.refine_rtx_5090_fe_visual_candidate":
            return self.service.refine_rtx_5090_fe_visual_candidate(
                config["source_revision"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "benchmark.refine_rtx_5090_fe_front_frame_candidate":
            return self.service.refine_rtx_5090_fe_front_frame_candidate(
                config["source_revision"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "benchmark.refine_dgx_spark_visual_candidate":
            return self.service.refine_dgx_spark_visual_candidate(
                config["source_revision"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "benchmark.refine_dgx_spark_base_foot_candidate":
            return self.service.refine_dgx_spark_base_foot_candidate(
                config["source_revision"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "repair.propose_mac_studio_grille":
            return self.service.propose_mac_studio_grille()
        if operation == "repair.approve":
            return self.service.approve_repair(config["proposal_id"], config["approved_by"])
        if operation == "repair.review":
            return self.service.review_repair(
                config["proposal_id"],
                accepted=bool(config["accepted"]),
                reviewer=config["reviewer"],
                reason=config["reason"],
                receipt_id=config.get("receipt_id"),
            )
        if operation == "repair.apply":
            return self.service.apply_repair(
                config["proposal_id"], scene_id=config.get("scene_id"), job_id=job_id
            )
        if operation == "component.fit":
            return ComponentFitter(self.project).propose(
                config["component_id"],
                config["parameter_bindings"],
                huber_delta=float(config.get("huber_delta", 1.5)),
            )
        if operation == "component.review_fit":
            return ComponentFitter(self.project).review(
                config["fit_id"],
                accepted=bool(config["accepted"]),
                reviewer=config["reviewer"],
                reason=config["reason"],
            )
        if operation == "component.generate":
            return self.service.generate_components(
                config["component_ids"],
                scene_id=config.get("scene_id"),
                job_id=job_id,
            )
        if operation == "optimization.execute_multiview_search":
            return MultiviewSearchStore(self.project).execute(config["search_id"])
        if operation == "portfolio.execute_parametric_seed":
            return self.service.generate_parametric_seed(
                portfolio_id=config["portfolio_id"],
                job_id=job_id,
            )
        if operation == "portfolio.execute_initial":
            return PortfolioExecutor(self.project).execute_initial(config["portfolio_id"])
        if operation == "dataset.generate":
            return self.service.generate_synthetic_dataset(config["dataset_id"], job_id=job_id)
        if operation == "vision.solve_cameras":
            return self.service.solve_cameras(
                backend=config.get("backend", "heuristic-pinhole"),
                reference_ids=config.get("reference_ids"),
            )
        if operation == "vision.refine_camera":
            return CameraRefiner(self.project).refine(
                source_solution_id=config.get("source_solution_id"),
                reference_id=config.get("reference_id"),
                scene_id=config.get("scene_id"),
                maximum_dimension=int(config.get("maximum_dimension", 256)),
                stages=int(config.get("stages", 3)),
                evidence_binding_ids=config.get("evidence_binding_ids"),
                job_id=job_id,
            )
        if operation == "vision.run":
            return GeometryPipeline(self.project).run(
                config.get("backend", "auto"), config.get("configuration", {})
            )
        if operation == "vision.import_geometry_evidence":
            return GeometryPipeline(self.project).import_external(
                backend=config["backend"],
                backend_version=config["backend_version"],
                evidence=config["evidence"],
                evidence_class=config["evidence_class"],
                license_record=config["license_record"],
                configuration=config.get("configuration"),
            )
        if operation == "vision.compare_backends":
            return GeometryPipeline(self.project).compare(config.get("run_ids"))
        if operation == "vision.compare_camera_solutions":
            return CameraConsensus(self.project).compare(config.get("solution_ids"))
        if operation == "blender.render":
            return self.service.render_views(
                scene_id=config.get("scene_id"),
                solution_id=config.get("solution_id"),
                job_id=job_id,
                maximum_dimension=int(config.get("maximum_dimension", 1024)),
                reference_ids=config.get("reference_ids"),
                requested_passes=config.get("requested_passes"),
                regions_by_reference=config.get("regions_by_reference"),
            )
        if operation == "blender.export":
            return self.service.export_scene(
                scene_id=config.get("scene_id"),
                output_name=config.get("output_name", "model.glb"),
                job_id=job_id,
            )
        if operation == "blender.export_blend":
            return self.service.export_blend(
                scene_id=config.get("scene_id"),
                output_name=config.get("output_name", "model.blend"),
                job_id=job_id,
            )
        if operation == "blender.generate_lod":
            return self.service.generate_lod(
                scene_id=config.get("scene_id"),
                ratio=float(config.get("ratio", 0.5)),
                objects=config.get("objects"),
                job_id=job_id,
            )
        if operation == "visual_geometry.repair_degenerate_candidate":
            return self.service.repair_degenerate_geometry_candidate(
                scene_id=config["scene_id"],
                object_name=config["object_name"],
                expected_degenerate_faces=int(config["expected_degenerate_faces"]),
                area_epsilon=float(config.get("area_epsilon", 1e-14)),
                merge_distance=float(config.get("merge_distance", 1e-10)),
                job_id=job_id,
            )
        if operation == "validation.compare":
            renders = config.get("renders")
            if not isinstance(renders, list):
                render_job = self.run("blender.render", config)
                if render_job["status"] != JobStatus.SUCCEEDED.value:
                    raise RuntimeError("render prerequisite failed")
                renders = render_job["result"]["renders"]
            return self.service.compare_views(renders)
        if operation == "validation.coverage":
            return self.service.coverage_report()
        if operation == "receipt.export":
            return self.service.export_receipt()
        if operation == "workflow.deliver_promoted":
            return self._deliver_promoted(config, job_id)
        if operation == "workflow.audit_reference_fidelity":
            return self._audit_workflow(config, job_id)
        raise ValueError(f"unknown operation: {operation}")

    def _completed(
        self,
        operation: str,
        config: dict[str, Any] | None = None,
        *,
        parent_job_id: str | None = None,
    ) -> dict[str, Any]:
        if parent_job_id and self.project.cancellation_requested(parent_job_id):
            raise JobCancelled(f"workflow cancelled: {parent_job_id}")
        job = self.run(operation, config)
        if job["status"] != JobStatus.SUCCEEDED.value:
            raise RuntimeError(f"workflow stage {operation} failed: {json.dumps(job['error'])}")
        if parent_job_id and self.project.cancellation_requested(parent_job_id):
            raise JobCancelled(f"workflow cancelled: {parent_job_id}")
        return {"job_id": job["id"], "result": job["result"]}

    def _deliver_promoted(
        self, config: dict[str, Any], parent_job_id: str
    ) -> dict[str, Any]:
        lifecycle = audit_scene_lifecycle(self.project)
        scene_id = config.get("scene_id") or lifecycle.get("authoritative_scene_id")
        if (
            not scene_id
            or lifecycle.get("authoritative_scene_id") != scene_id
            or lifecycle.get("authoritative_promotion_chain_valid") is not True
        ):
            raise ValueError(
                "autonomous delivery requires the authoritative scene's verified promotion chain"
            )
        project = self.project.project()
        output_prefix = str(config.get("output_prefix") or project["slug"])
        stages: list[dict[str, Any]] = []
        exports = self._delivery_exports(scene_id)
        if "blend" not in exports:
            stages.append(
                self._completed(
                    "blender.export_blend",
                    {"scene_id": scene_id, "output_name": f"{output_prefix}.blend"},
                    parent_job_id=parent_job_id,
                )
            )
        if "glb" not in exports:
            stages.append(
                self._completed(
                    "blender.export",
                    {"scene_id": scene_id, "output_name": f"{output_prefix}.glb"},
                    parent_job_id=parent_job_id,
                )
            )
        exports = self._delivery_exports(scene_id)
        if set(exports) != {"blend", "glb"}:
            raise RuntimeError("delivery did not produce artifact-valid BLEND and GLB exports")
        receipt = self._current_delivery_receipt(scene_id, exports)
        reused_receipt = receipt is not None
        if receipt is None:
            receipt_stage = self._completed("receipt.export", parent_job_id=parent_job_id)
            stages.append(receipt_stage)
            receipt = receipt_stage["result"]
            verification = verify_receipt(
                self.project.root / receipt["path"], project=self.project
            )
            if not verification["valid"]:
                raise RuntimeError("final delivery receipt failed cryptographic verification")
            if receipt["acceptance"]["accepted"]:
                current = self._current_delivery_receipt(scene_id, exports)
                if current is None:
                    raise RuntimeError(
                        "accepted delivery receipt does not bind the promoted scene and exports"
                    )
                receipt = current
        accepted = bool(receipt["acceptance"]["accepted"])
        return {
            "workflow": "deliver_promoted",
            "status": "DELIVERED" if accepted else "NOT_ACCEPTED",
            "accepted": accepted,
            "scene_id": scene_id,
            "exports": exports,
            "receipt": receipt,
            "reused_receipt": reused_receipt,
            "stages": stages,
            "acceptance": receipt["acceptance"],
        }

    def _delivery_exports(self, scene_id: str) -> dict[str, dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT e.*,a.size,a.media_type,a.relative_path AS artifact_path "
                "FROM exports e JOIN artifacts a ON a.digest=e.artifact_digest "
                "WHERE e.scene_id=? AND e.format IN ('blend','glb') "
                "ORDER BY e.created_at DESC,e.id DESC",
                (scene_id,),
            ).fetchall()
        selected: dict[str, dict[str, Any]] = {}
        artifacts = ArtifactStore(self.project)
        for row in rows:
            value = dict(row)
            if value["format"] in selected:
                continue
            artifact_path = artifacts.path_for(value["artifact_digest"])
            materialized_path = self.project.root / value["relative_path"]
            try:
                artifact_digest, artifact_size = sha256_file(artifact_path)
                materialized_digest, materialized_size = sha256_file(materialized_path)
            except (FileNotFoundError, OSError):
                continue
            if (
                artifact_digest != value["artifact_digest"]
                or materialized_digest != value["artifact_digest"]
                or artifact_size != int(value["size"])
                or materialized_size != int(value["size"])
            ):
                continue
            value["configuration"] = json.loads(value.pop("config_json"))
            value["worker"] = json.loads(value.pop("worker_json"))
            value["artifact"] = {
                "digest": value["artifact_digest"],
                "size": value.pop("size"),
                "media_type": value.pop("media_type"),
                "path": value.pop("artifact_path"),
            }
            selected[value["format"]] = value
        return selected

    def _current_delivery_receipt(
        self, scene_id: str, exports: dict[str, dict[str, Any]]
    ) -> dict[str, Any] | None:
        required_export_ids = {item["id"] for item in exports.values()}
        current_acceptance = evaluate_acceptance(self.project)
        if not current_acceptance["accepted"]:
            return None
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT r.id,r.digest,r.created_at,a.size,a.media_type,a.relative_path "
                "FROM receipts r JOIN artifacts a ON a.digest=r.digest "
                "WHERE r.accepted=1 ORDER BY r.created_at DESC,r.id DESC"
            ).fetchall()
        artifacts = ArtifactStore(self.project)
        for row in rows:
            path = artifacts.path_for(row["digest"])
            verification = verify_receipt(path, project=self.project)
            if not verification["valid"]:
                continue
            try:
                payload = json.loads(path.read_text(encoding="utf-8"))["payload"]
            except (OSError, KeyError, TypeError, json.JSONDecodeError):
                continue
            scenes = payload.get("evidence", {}).get("scenes", [])
            receipt_exports = payload.get("evidence", {}).get("exports", [])
            scene_valid = any(
                item.get("id") == scene_id
                and item.get("state") == "PROMOTED"
                and bool(item.get("is_authoritative"))
                for item in scenes
            )
            export_ids = {
                item.get("id")
                for item in receipt_exports
                if item.get("scene_id") == scene_id
            }
            acceptance = payload.get("acceptance", {})
            if (
                scene_valid
                and required_export_ids.issubset(export_ids)
                and acceptance.get("accepted") is True
                and acceptance.get("accepted_fidelity")
                == self.project.project()["target_fidelity"]
                and acceptance.get("blockers") == current_acceptance["blockers"]
                and acceptance.get("accepted_fidelity")
                == current_acceptance["accepted_fidelity"]
            ):
                original_path = self.project.root / "receipts" / f"{row['id']}.json"
                try:
                    original_valid = sha256_file(original_path)[0] == row["digest"]
                except (FileNotFoundError, OSError):
                    original_valid = False
                delivery_path = original_path if original_valid else path
                return {
                    "id": row["id"],
                    "path": str(delivery_path.relative_to(self.project.root)),
                    "artifact": {
                        "digest": row["digest"],
                        "size": row["size"],
                        "media_type": row["media_type"],
                        "path": row["relative_path"],
                    },
                    "acceptance": acceptance,
                    "payload_sha256": verification["payload_sha256"],
                    "reused": True,
                }
        return None

    def _audit_workflow(self, config: dict[str, Any], parent_job_id: str) -> dict[str, Any]:
        stages: list[dict[str, Any]] = []
        if config.get("scene"):
            stages.append(
                self._completed(
                    "scene.import",
                    {"source": config["scene"]},
                    parent_job_id=parent_job_id,
                )
            )
        for reference in config.get("references", []):
            reference_config = {"source": reference} if isinstance(reference, str) else reference
            stages.append(
                self._completed("reference.import", reference_config, parent_job_id=parent_job_id)
            )
        stages.append(self._completed("blender.inspect", parent_job_id=parent_job_id))
        camera = self._completed(
            "vision.solve_cameras",
            {"backend": config.get("backend", "heuristic-pinhole")},
            parent_job_id=parent_job_id,
        )
        stages.append(camera)
        render = self._completed(
            "blender.render",
            {
                "solution_id": camera["result"]["id"],
                "maximum_dimension": int(config.get("maximum_dimension", 1024)),
            },
            parent_job_id=parent_job_id,
        )
        stages.append(render)
        stages.append(
            self._completed(
                "validation.compare",
                {"renders": render["result"]["renders"]},
                parent_job_id=parent_job_id,
            )
        )
        stages.append(self._completed("validation.coverage", parent_job_id=parent_job_id))
        stages.append(self._completed("receipt.export", parent_job_id=parent_job_id))
        return {
            "workflow": "audit_reference_fidelity",
            "stages": stages,
            "stage_count": len(stages),
        }

    def daemon(self, *, poll_seconds: float = 0.25, once: bool = False) -> None:
        while True:
            job_id = self.project.claim_next_job()
            if job_id:
                self.execute(job_id)
            elif once:
                return
            else:
                time.sleep(max(0.05, poll_seconds))
