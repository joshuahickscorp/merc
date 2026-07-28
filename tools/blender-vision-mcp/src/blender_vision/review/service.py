from __future__ import annotations

import json
import threading
import uuid
from typing import Any

from blender_vision.acceptance.receipts import verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.reviews import BenchmarkReviewStore
from blender_vision.cameras.consensus import CameraConsensus
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import FidelityLevel, RegistrationClass
from blender_vision.core.util import utc_now
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.evidence.adoption import LegacyReferenceAdoptionStore
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.measurements import MeasurementGridStore, MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.features.store import FeatureStore
from blender_vision.materials.store import MaterialStore
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.orchestration.roles import RoleTaskStore
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.repairs.store import RepairStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import DistributedScheduler
from blender_vision.vision.store import GeometryEvidenceStore
from blender_vision.visual.oracle import VisualOracleStore
from blender_vision.workflows.service import ReconstructionService


class ReviewService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    @staticmethod
    def artifact_url(digest: str | None) -> str | None:
        return f"/artifact/{digest}" if digest else None

    def artifact(self, digest: str) -> tuple[Any, str]:
        path = self.artifacts.path_for(digest)
        if not path.is_file():
            raise FileNotFoundError(f"unknown artifact: {digest}")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT media_type FROM artifacts WHERE digest=?", (digest,)
            ).fetchone()
        if row is None:
            raise FileNotFoundError(f"unknown artifact: {digest}")
        return path, row["media_type"]

    def snapshot(self) -> dict[str, Any]:
        references = ReferenceIngestor(self.project).list()
        reference_by_id = {item["id"]: item for item in references}
        features = FeatureStore(self.project).list()
        repairs = RepairStore(self.project).list()
        fits = ComponentFitter(self.project).list()
        cameras = self._cameras()
        comparisons = self._comparisons(reference_by_id)
        coverage = self._coverage()
        receipt = self._latest_receipt()
        geometry = GeometryEvidenceStore(self.project)
        materials = MaterialStore(self.project).list()
        landmark_proposals = CameraLandmarkStore(self.project).list()
        optimization_runs = OptimizationEngine(self.project).list()
        role_tasks = RoleTaskStore(self.project).list()
        reference_adoptions = LegacyReferenceAdoptionStore(self.project).list()
        mask_proposals = ReferenceMaskStore(self.project).list_proposals()
        benchmark_review = BenchmarkReviewStore(self.project).dgx_foam_lod_status()
        status = self.project.status()
        return {
            "project": status["project"],
            "counts": status["counts"],
            "job_counts": status["jobs"],
            "references": [
                {
                    **item,
                    "image_url": self.artifact_url(item["artifact_digest"]),
                }
                for item in references
            ],
            "reference_masks": ReferenceMaskStore(self.project).list(),
            "comparisons": comparisons,
            "features": features,
            "measurements": MeasurementStore(self.project).list(),
            "measurement_grids": MeasurementGridStore(self.project).list(),
            "components": ComponentStore(self.project).list(),
            "material_profiles": materials,
            "cameras": cameras,
            "landmark_proposals": landmark_proposals,
            "camera_consensus": CameraConsensus(self.project).latest(),
            "repairs": repairs,
            "fits": fits,
            "geometry_runs": geometry.list(),
            "geometry_consensus": geometry.latest_consensus(),
            "datasets": DatasetStore(self.project).list(),
            "training_runs": TrainingStore(self.project).list(),
            "visual_oracles": VisualOracleStore(self.project).list(),
            "optimization_runs": optimization_runs,
            "role_tasks": role_tasks,
            "reference_adoptions": reference_adoptions,
            "reference_mask_proposals": mask_proposals,
            "benchmark_review": benchmark_review,
            "coverage": coverage,
            "capture_requests": self._capture_requests(),
            "tier_reviews": self._tier_reviews(),
            "latest_receipt": receipt,
            "jobs": self.project.list_jobs(limit=50),
            "workers": DistributedScheduler(self.project).list(),
            "review_queue": self.review_queue(
                features=features,
                cameras=cameras,
                repairs=repairs,
                fits=fits,
                materials=materials,
                landmark_proposals=landmark_proposals,
                optimization_runs=optimization_runs,
                role_tasks=role_tasks,
                reference_adoptions=reference_adoptions,
                mask_proposals=mask_proposals,
                benchmark_review=benchmark_review,
            ),
        }

    def review_queue(
        self,
        *,
        features: list[dict[str, Any]] | None = None,
        cameras: list[dict[str, Any]] | None = None,
        repairs: list[dict[str, Any]] | None = None,
        fits: list[dict[str, Any]] | None = None,
        materials: list[dict[str, Any]] | None = None,
        landmark_proposals: list[dict[str, Any]] | None = None,
        optimization_runs: list[dict[str, Any]] | None = None,
        role_tasks: list[dict[str, Any]] | None = None,
        reference_adoptions: list[dict[str, Any]] | None = None,
        mask_proposals: list[dict[str, Any]] | None = None,
        benchmark_review: dict[str, Any] | None = None,
    ) -> list[dict[str, Any]]:
        features = features if features is not None else FeatureStore(self.project).list()
        cameras = cameras if cameras is not None else self._cameras()
        repairs = repairs if repairs is not None else RepairStore(self.project).list()
        fits = fits if fits is not None else ComponentFitter(self.project).list()
        materials = materials if materials is not None else MaterialStore(self.project).list()
        landmark_proposals = (
            landmark_proposals
            if landmark_proposals is not None
            else CameraLandmarkStore(self.project).list()
        )
        optimization_runs = (
            optimization_runs
            if optimization_runs is not None
            else OptimizationEngine(self.project).list()
        )
        role_tasks = role_tasks if role_tasks is not None else RoleTaskStore(self.project).list()
        reference_adoptions = (
            reference_adoptions
            if reference_adoptions is not None
            else LegacyReferenceAdoptionStore(self.project).list()
        )
        mask_proposals = (
            mask_proposals
            if mask_proposals is not None
            else ReferenceMaskStore(self.project).list_proposals()
        )
        benchmark_review = (
            benchmark_review
            if benchmark_review is not None
            else BenchmarkReviewStore(self.project).dgx_foam_lod_status()
        )
        references = {
            item["id"]: item for item in ReferenceIngestor(self.project).list()
        }
        camera_reviews = self._prioritized_camera_reviews(cameras)
        queue = []
        queue.extend(
            {
                "kind": "role_task",
                "id": item["id"],
                "title": f"{item['role']}: {item['objective']}",
                "state": item["status"].lower(),
                "actionable": False,
                "priority": item["priority"],
                "confidence": item["confidence"],
                "estimated_cost": item["estimated_cost"],
                "waiting_reason": (item.get("output") or {}).get("waiting_reason"),
                "inputs": item["inputs"],
                "authority": item["authority"],
            }
            for item in role_tasks
            if item["status"] in {"ASSIGNED", "WAITING_INPUT"}
        )
        queue.extend(
            [
                {
                    "kind": "benchmark_policy",
                    "id": "dgx_foam_lod_strategy",
                    "title": "Review DGX foam level-of-detail policy",
                    "state": "invalid" if benchmark_review.get("approval") else "pending",
                    "actionable": True,
                    "authority": "NAMED_BENCHMARK_POLICY_REVIEW_REQUIRED",
                    "verification_error": benchmark_review.get("error"),
                    "strategy_template": {
                        "switch_policy": "screen_space_pixel_footprint",
                        "tiers": [
                            {
                                "name": "hero",
                                "representation": "physical_geometry",
                                "minimum_screen_diameter_px": 24,
                            },
                            {
                                "name": "mid",
                                "representation": "geometry_nodes",
                                "minimum_screen_diameter_px": 6,
                            },
                            {
                                "name": "background",
                                "representation": "normal_map",
                                "minimum_screen_diameter_px": 1,
                            },
                        ],
                        "validation_views": sorted(
                            {
                                str(item.get("viewpoint_label"))
                                for item in references.values()
                                if item.get("viewpoint_label")
                                and str(item.get("media_type", "")).startswith("image/")
                            }
                        ),
                        "crossfade": True,
                    },
                }
            ]
            if benchmark_review.get("required") and not benchmark_review.get("valid")
            else []
        )
        queue.extend(
            {
                "kind": "reference_adoption",
                "id": item["id"],
                "title": (
                    "Govern legacy reference: "
                    f"{item['proposal']['reference']['original_name']}"
                ),
                "state": item["status"].lower(),
                "actionable": True,
                "proposal_digest": item["proposal_digest"],
                "target_id": item["target_id"],
                "reference": item["proposal"]["reference"],
                "suggested_source": item["proposal"]["suggested_source"],
                "canonical_target": item["proposal"]["canonical_target"],
                "known_limitations": item["proposal"]["known_limitations"],
                "authority": item["proposal"]["authority"],
                "image_url": self.artifact_url(
                    item["proposal"]["reference"]["artifact_digest"]
                ),
            }
            for item in reference_adoptions
            if item["status"] == "PROPOSED"
        )
        queue.extend(
            {
                "kind": "reference_mask",
                "id": item["id"],
                "title": (
                    "Review proposed silhouette mask: "
                    + references.get(item["reference_id"], {}).get(
                        "original_name", item["reference_id"]
                    )
                ),
                "state": item["status"].lower(),
                "actionable": True,
                "proposal_digest": item["proposal_digest"],
                "reference_id": item["reference_id"],
                "reference_image_url": self.artifact_url(
                    item["proposal"]["reference_artifact_digest"]
                ),
                "mask_image_url": self.artifact_url(item["mask_artifact_digest"]),
                "method": item["proposal"]["method"],
                "confidence": item["proposal"]["confidence"],
                "intended_use": item["proposal"]["intended_use"],
                "roi": item["proposal"]["roi"],
                "authority": item["proposal"]["authority"],
            }
            for item in mask_proposals
            if item["status"] == "PROPOSED"
        )
        queue.extend(
            {
                "kind": "landmark",
                "id": item["id"],
                "title": "Review proposed 2D/3D camera landmarks",
                "state": item["status"].lower(),
                "actionable": True,
                "proposal_digest": item["proposal_digest"],
                "model_source_id": item["model_source_id"],
                "intrinsics_solution_id": item["intrinsics_solution_id"],
                "known_limitations": item["proposal"]["known_limitations"],
                "backend_identity": item["proposal"]["backend_identity"],
                "point_count": sum(
                    len(view["correspondences"])
                    for view in item["proposal"]["views"]
                ),
                "views": [
                    {
                        **view,
                        "image_url": self.artifact_url(
                            references.get(view["reference_id"], {}).get(
                                "artifact_digest"
                            )
                        ),
                        "image_width": references.get(view["reference_id"], {})
                        .get("metadata", {})
                        .get("width"),
                        "image_height": references.get(view["reference_id"], {})
                        .get("metadata", {})
                        .get("height"),
                    }
                    for view in item["proposal"]["views"]
                ],
                "authority": item["proposal"]["authority"],
            }
            for item in landmark_proposals
            if item["status"] == "PROPOSED"
        )
        queue.extend(
            {
                "kind": "optimization",
                "id": item["id"],
                "title": f"Optimization: {item['method']} for {item['component_id']}",
                "state": item["status"],
                "actionable": True,
                "best_parameters": item["result"]["best_parameters"],
                "best_total_loss": item["result"]["best_total_loss"],
                "baseline_total_loss": item["result"]["baseline_total_loss"],
                "expected_improvement": item["result"]["expected_improvement"],
                "authority": item["result"]["authority"],
            }
            for item in optimization_runs
            if item["status"] == "proposed"
        )
        queue.extend(
            {
                "kind": "feature",
                "id": item["id"],
                "title": f"Feature: {item['type']}",
                "state": item.get("approval", {}).get("state", "pending"),
                "confidence": item.get("confidence"),
                "evidence_class": item.get("evidence_class"),
                "hero_surface": bool(item.get("hero_surface")),
            }
            for item in features
            if item.get("lifecycle_state", "active") == "active"
            and item.get("approval", {}).get("state", "pending") == "pending"
        )
        queue.extend(
            {
                "kind": "camera",
                "id": item["id"],
                "title": f"Camera solution: {item['backend']}",
                "state": item["solution"].get("approval", {}).get("state", "pending"),
                "camera_count": len(item["solution"].get("cameras", [])),
                "registration_classes": sorted(
                    {
                        camera.get("registration_class", "unknown")
                        for camera in item["solution"].get("cameras", [])
                    }
                ),
                "evidence_classes": sorted(
                    {
                        str(camera.get("evidence_class", "unknown"))
                        for camera in item["solution"].get("cameras", [])
                    }
                ),
                "minimum_confidence": min(
                    (
                        float(camera.get("confidence", 0.0))
                        for camera in item["solution"].get("cameras", [])
                    ),
                    default=0.0,
                ),
                "views": [
                    {
                        "reference_id": camera.get("reference_id"),
                        "image_url": self.artifact_url(
                            references.get(str(camera.get("reference_id")), {}).get(
                                "artifact_digest"
                            )
                        ),
                        "original_name": references.get(
                            str(camera.get("reference_id")), {}
                        ).get("original_name"),
                        "viewpoint_label": references.get(
                            str(camera.get("reference_id")), {}
                        ).get("viewpoint_label"),
                        "registration_class": camera.get("registration_class"),
                        "evidence_class": camera.get("evidence_class"),
                        "confidence": float(camera.get("confidence", 0.0)),
                        "search_silhouette_iou": camera.get("diagnostics", {}).get(
                            "search_silhouette_iou"
                        ),
                        "authority": camera.get("diagnostics", {}).get("authority"),
                    }
                    for camera in item["solution"].get("cameras", [])
                ],
                "covers_acceptance_references": {
                    str(camera.get("reference_id"))
                    for camera in item["solution"].get("cameras", [])
                    if camera.get("reference_id")
                }
                == {
                    reference_id
                    for reference_id, reference in references.items()
                    if reference.get("acceptance_eligible")
                    and str(reference.get("media_type", "")).startswith("image/")
                },
                "metric_authority": bool(item["solution"].get("cameras"))
                and all(
                    camera.get("registration_class") == RegistrationClass.METRIC.value
                    for camera in item["solution"].get("cameras", [])
                ),
                "authority_warning": (
                    None
                    if item["solution"].get("cameras")
                    and all(
                        camera.get("registration_class") == RegistrationClass.METRIC.value
                        for camera in item["solution"].get("cameras", [])
                    )
                    else (
                        "Non-metric approval can freeze framing for comparison but cannot satisfy "
                        "L3 reconstruction or external Beast camera authority."
                    )
                ),
                "alternative_count": len(item["lower_priority_alternative_ids"]),
                "lower_priority_alternative_ids": item[
                    "lower_priority_alternative_ids"
                ],
                "prioritization_note": (
                    f"Summarizes {len(item['lower_priority_alternative_ids'])} lower-priority "
                    "camera proposal(s) with no stronger authority, reference coverage, or "
                    "per-reference confidence/fit evidence."
                    if item["lower_priority_alternative_ids"]
                    else None
                ),
            }
            for item in camera_reviews
        )
        queue.extend(
            self._repair_review_item(item)
            for item in repairs
            if item["status"] in {"proposed", "approved", "applied"}
        )
        queue.extend(
            {
                "kind": "fit",
                "id": item["id"],
                "title": f"Component fit: {item['component_id']}",
                "state": item["status"],
                "candidate_parameters": item["result"]["candidate_parameters"],
                "constraints_pass": item["result"]["constraints_pass"],
                "release_eligible_evidence": item["result"]["release_eligible_evidence"],
            }
            for item in fits
            if item["status"] == "proposed"
        )
        queue.extend(
            {
                "kind": "material",
                "id": item["id"],
                "title": f"Material: {item['region_id']}",
                "state": item["status"],
                "confidence": item["confidence"],
                "evidence_class": item["evidence"]["evidence_class"],
                "properties": item["properties"],
            }
            for item in materials
            if item["status"] == "proposed"
        )
        order = {
            "role_task": 0,
            "benchmark_policy": 1,
            "reference_adoption": 2,
            "reference_mask": 3,
            "landmark": 4,
            "camera": 5,
            "repair": 6,
            "optimization": 7,
            "feature": 8,
            "material": 9,
            "fit": 10,
        }
        return sorted(queue, key=lambda item: (order[item["kind"]], item["id"]))

    def _repair_review_item(self, item: dict[str, Any]) -> dict[str, Any]:
        result = item.get("result") or {}
        worker = result.get("worker") or {}
        inventory = (
            result.get("audit", {}).get("audit", {}).get("inventory", {})
        )
        return {
            "kind": "repair",
            "id": item["id"],
            "title": f"Repair: {item['kind']}",
            "state": item["status"],
            "expected": item["expected"],
            "render_url": self.artifact_url(
                result.get("rear_render", {}).get("artifact", {}).get("digest")
            ),
            "acceptance": result.get("acceptance"),
            "validation": {
                "source_scene_id": result.get("source_scene_id"),
                "generated_scene_id": result.get("generated_scene", {}).get("id"),
                "generated_hole_count": worker.get("generated_hole_count"),
                "ray_validation": worker.get("ray_validation"),
                "topology": worker.get("topology"),
                "dimensional_checks": worker.get("dimensional_checks"),
                "audit_findings": inventory.get("audit_findings", []),
            },
        }

    @staticmethod
    def _camera_authority_rank(item: dict[str, Any]) -> int:
        ranks = {
            "body_bounding_box_alignment": 0,
            "approximate_visual_registration": 1,
            "feature_based_camera_solution": 2,
            "metric_camera_solution": 3,
        }
        cameras = item.get("solution", {}).get("cameras", [])
        return min(
            (ranks.get(str(camera.get("registration_class")), 0) for camera in cameras),
            default=0,
        )

    @classmethod
    def _prioritized_camera_reviews(
        cls, cameras: list[dict[str, Any]]
    ) -> list[dict[str, Any]]:
        pending = [
            item
            for item in cameras
            if item.get("solution", {}).get("approval", {}).get("state", "pending")
            == "pending"
        ]

        def reference_ids(item: dict[str, Any]) -> set[str]:
            return {
                str(camera["reference_id"])
                for camera in item.get("solution", {}).get("cameras", [])
                if camera.get("reference_id")
            }

        def quality_by_reference(
            item: dict[str, Any]
        ) -> dict[str, tuple[float, float]]:
            quality: dict[str, tuple[float, float]] = {}
            for camera in item.get("solution", {}).get("cameras", []):
                reference_id = camera.get("reference_id")
                if not reference_id:
                    continue
                confidence = float(camera.get("confidence", 0.0))
                silhouette_iou = float(
                    camera.get("diagnostics", {}).get("search_silhouette_iou", -1.0)
                )
                candidate = (confidence, silhouette_iou)
                quality[str(reference_id)] = max(
                    candidate, quality.get(str(reference_id), (-1.0, -1.0))
                )
            return quality

        def dominates(other: dict[str, Any], item: dict[str, Any]) -> bool:
            if other["id"] == item["id"]:
                return False
            other_refs = reference_ids(other)
            item_refs = reference_ids(item)
            other_rank = cls._camera_authority_rank(other)
            item_rank = cls._camera_authority_rank(item)
            if not item_refs.issubset(other_refs) or other_rank < item_rank:
                return False
            other_quality = quality_by_reference(other)
            item_quality = quality_by_reference(item)
            if any(
                other_quality.get(reference_id, (-1.0, -1.0))[0]
                < item_quality.get(reference_id, (-1.0, -1.0))[0]
                or other_quality.get(reference_id, (-1.0, -1.0))[1]
                < item_quality.get(reference_id, (-1.0, -1.0))[1]
                for reference_id in item_refs
            ):
                return False
            return bool(
                other_refs != item_refs
                or other_rank > item_rank
                or any(
                    other_quality.get(reference_id) != item_quality.get(reference_id)
                    for reference_id in item_refs
                )
                or (other.get("created_at", ""), other["id"])
                > (item.get("created_at", ""), item["id"])
            )

        survivors = [
            item for item in pending if not any(dominates(other, item) for other in pending)
        ]
        return [
            {
                **item,
                "lower_priority_alternative_ids": sorted(
                    other["id"] for other in pending if dominates(item, other)
                ),
            }
            for item in survivors
        ]

    def action(self, action: str, payload: dict[str, Any]) -> dict[str, Any]:
        reviewer = str(payload.get("reviewer", ""))
        reason = str(payload.get("reason", ""))
        if action == "feature.review":
            return FeatureStore(self.project).review(
                str(payload["id"]),
                approved=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "camera.review":
            solver = CameraSolver(self.project)
            if bool(payload["accepted"]):
                return solver.approve(str(payload["id"]), reviewer=reviewer, reason=reason)
            return solver.reject(str(payload["id"]), reviewer=reviewer, reason=reason)
        if action == "landmarks.review":
            return CameraLandmarkStore(self.project).review(
                str(payload["id"]),
                reviewer=reviewer,
                reason=reason,
                decisions=list(payload.get("decisions") or []),
            )
        if action == "reference_adoption.review":
            return LegacyReferenceAdoptionStore(self.project).review(
                str(payload["id"]),
                decision=str(payload["decision"]),
                reviewer=reviewer,
                reason=reason,
                source=(dict(payload["source"]) if payload.get("source") else None),
                rights=(dict(payload["rights"]) if payload.get("rights") else None),
                source_terms_review=payload.get("source_terms_review"),
                privacy_review=payload.get("privacy_review"),
            )
        if action == "benchmark.review_dgx_foam_lod":
            return BenchmarkReviewStore(self.project).approve_dgx_foam_lod(
                strategy=dict(payload["strategy"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "repair.approve":
            return RepairStore(self.project).approve(str(payload["id"]), reviewer)
        if action == "repair.reject_proposal":
            return RepairStore(self.project).reject_proposed(
                str(payload["id"]), reviewer=reviewer, reason=reason
            )
        if action == "repair.review":
            return ReconstructionService(self.project).review_repair(
                str(payload["id"]),
                accepted=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
                receipt_id=payload.get("receipt_id"),
            )
        if action == "fit.review":
            return ComponentFitter(self.project).review(
                str(payload["id"]),
                accepted=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "optimization.review":
            return OptimizationEngine(self.project).review(
                str(payload["id"]),
                accepted=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "material.review":
            return MaterialStore(self.project).review(
                str(payload["id"]),
                approved=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "measurement.correct":
            return MeasurementStore(self.project).correct(
                str(payload["id"]),
                dict(payload["value"]),
                uncertainty=dict(payload.get("uncertainty") or {}),
                corrected_by=reviewer,
                reason=reason,
            )
        if action == "reference-mask.import":
            return ReferenceMaskStore(self.project).import_reviewed(
                str(payload["reference_id"]),
                self.project.root / str(payload["mask_path"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "reference_mask.review":
            return ReferenceMaskStore(self.project).review_proposal(
                str(payload["id"]),
                accepted=bool(payload["accepted"]),
                reviewer=reviewer,
                reason=reason,
            )
        if action == "feature.link":
            return FeatureStore(self.project).link_observation(
                str(payload["id"]),
                str(payload["reference_id"]),
                dict(payload["observation"]),
                linked_by=reviewer,
                reason=reason,
            )
        if action == "capture.request":
            return self._request_capture(payload, requester=reviewer, reason=reason)
        if action == "tier.review":
            return self._review_tier(payload, reviewer=reviewer, reason=reason)
        if action == "job.cancel":
            self.project.request_cancel(str(payload["id"]))
            return self.project.job(str(payload["id"]))
        if action in {"project.audit", "receipt.export", "repair.apply"}:
            configuration = dict(payload.get("configuration") or {})
            if action == "repair.apply":
                configuration["proposal_id"] = str(payload["id"])
            return self._enqueue(action, configuration)
        raise ValueError(f"unsupported review action: {action}")

    def _request_capture(
        self, payload: dict[str, Any], *, requester: str, reason: str
    ) -> dict[str, Any]:
        if not requester.strip() or not reason.strip():
            raise ValueError("capture request requires a named requester and reason")
        direction = str(payload.get("direction", "")).strip().lower()
        if direction not in {"front", "rear", "left", "right", "top", "bottom", "detail"}:
            raise ValueError("capture direction must name a standard surface or detail")
        now = utc_now()
        request_id = str(uuid.uuid4())
        record = {
            "id": request_id,
            "direction": direction,
            "region": str(payload.get("region", "")).strip() or None,
            "instructions": str(payload.get("instructions", "")).strip() or None,
            "requester": requester.strip(),
            "reason": reason.strip(),
            "status": "requested",
            "created_at": now,
            "updated_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO capture_requests(id,request_json,status,created_at,updated_at) "
                "VALUES(?,?,?,?,?)",
                (request_id, json.dumps(record), "requested", now, now),
            )
        return record

    def _review_tier(
        self, payload: dict[str, Any], *, reviewer: str, reason: str
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("tier review requires a named reviewer and reason")
        requested = FidelityLevel(str(payload["fidelity"]))
        receipt = self._latest_receipt()
        receipt_fidelity = (
            receipt["acceptance"].get("accepted_fidelity")
            if receipt and receipt["accepted"]
            else None
        )
        receipt_satisfies = bool(
            receipt_fidelity
            and int(str(receipt_fidelity)[1:]) >= int(requested.value[1:])
            and receipt["verification"].get("valid")
        )
        accepted = bool(payload.get("accepted")) and receipt_satisfies
        now = utc_now()
        review_id = str(uuid.uuid4())
        record = {
            "id": review_id,
            "requested_fidelity": requested.value,
            "accepted": accepted,
            "requested_decision": bool(payload.get("accepted")),
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "receipt_id": receipt["id"] if receipt else None,
            "receipt_fidelity": receipt_fidelity,
            "receipt_satisfies": receipt_satisfies,
            "state": "accepted" if accepted else "rejected_or_blocked",
            "reviewed_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO tier_reviews(id,requested_fidelity,decision_json,created_at) "
                "VALUES(?,?,?,?)",
                (review_id, requested.value, json.dumps(record), now),
            )
        return record

    def _capture_requests(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT request_json FROM capture_requests ORDER BY created_at"
            ).fetchall()
        return [json.loads(row["request_json"]) for row in rows]

    def _tier_reviews(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT decision_json FROM tier_reviews ORDER BY created_at"
            ).fetchall()
        return [json.loads(row["decision_json"]) for row in rows]

    def _enqueue(self, operation: str, configuration: dict[str, Any]) -> dict[str, Any]:
        coordinator = Coordinator(self.project)
        job_id = coordinator.enqueue(operation, configuration)
        thread = threading.Thread(target=coordinator.execute, args=(job_id,), daemon=True)
        thread.start()
        return {"job_id": job_id, "status": "queued"}

    def _cameras(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,backend,solution_json,diagnostics_json,created_at,approved,"
                "decision_id,decision_digest "
                "FROM camera_solutions ORDER BY created_at"
            ).fetchall()
        cameras = [
            {
                "id": row["id"],
                "backend": row["backend"],
                "solution": json.loads(row["solution_json"]),
                "diagnostics": json.loads(row["diagnostics_json"]),
                "created_at": row["created_at"],
                "approved": bool(row["approved"]),
            }
            for row in rows
        ]
        decision_store = CameraDecisionStore(self.project)
        for camera in cameras:
            verification = decision_store.verify_record(camera)
            camera["decision_receipt_valid"] = verification["valid"]
            camera["decision_receipt"] = verification["decision"]
        return cameras

    def _comparisons(self, references: dict[str, dict[str, Any]]) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute("SELECT * FROM comparisons ORDER BY created_at").fetchall()
        return [
            {
                "id": row["id"],
                "reference_id": row["reference_id"],
                "reference_url": self.artifact_url(
                    references.get(row["reference_id"], {}).get("artifact_digest")
                ),
                "render_url": self.artifact_url(row["render_digest"]),
                "residual_url": self.artifact_url(row["residual_digest"]),
                "metrics": json.loads(row["metrics_json"]),
                "created_at": row["created_at"],
            }
            for row in rows
        ]

    def _coverage(self) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT report_json FROM coverage_reports ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        return json.loads(row[0]) if row else None

    def _latest_receipt(self) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT id,digest,accepted,created_at FROM receipts "
                "ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        if row is None:
            return None
        path = self.artifacts.path_for(row["digest"])
        envelope = json.loads(path.read_text(encoding="utf-8"))
        return {
            "id": row["id"],
            "digest": row["digest"],
            "accepted": bool(row["accepted"]),
            "created_at": row["created_at"],
            "acceptance": envelope["payload"]["acceptance"],
            "verification": verify_receipt(path, project=self.project),
            "artifact_url": self.artifact_url(row["digest"]),
        }
