from __future__ import annotations

import json
import math
from typing import Any

from blender_vision.acceptance.lifecycle import audit_scene_lifecycle
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.passes import GOVERNED_RENDER_PASSES
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.capture.service import CaptureService
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.models import RegistrationClass
from blender_vision.core.util import sha256_file
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.optimization.search import MultiviewSearchStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.orchestration.roles import RoleTaskStore
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator


class AutonomousWorkflowExecutor:
    """Advance one evidence-derived production action and stop at governed authority boundaries."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.campaigns = CampaignStore(project)

    def continue_once(
        self,
        campaign_id: str,
        *,
        camera_backend: str = "auto",
        resume_paused: bool = False,
    ) -> dict[str, Any]:
        campaign = self.campaigns.get(campaign_id)
        if campaign["status"] == "PAUSED" and resume_paused:
            campaign = self.campaigns.resume(campaign_id)
        if campaign["status"] != "RUNNING":
            return self._result(campaign, "CAMPAIGN_NOT_RUNNING", {})
        facts = self._facts()
        if facts["target_status"] != "RESOLVED":
            campaign = self.campaigns.pause(
                campaign_id, reason="material target ambiguity requires clarification"
            )
            return self._result(campaign, "TARGET_CLARIFICATION_REQUIRED", facts)

        if facts["evidence_source_count"] == 0:
            providers = SearchProviderStore(self.project).list()
            if providers:
                provider = providers[-1]
                try:
                    discovery = SearchProviderStore(self.project).discover(
                        provider["id"],
                        target_id=facts["target_id"],
                        category=campaign.get("configuration", {}).get(
                            "category", "general_product"
                        ),
                    )
                except Exception as error:
                    campaign = self.campaigns.pause(
                        campaign_id,
                        reason="governed source discovery failed and requires provider review",
                    )
                    return self._result(
                        campaign,
                        "EVIDENCE_DISCOVERY_FAILED",
                        {**facts, "error": f"{type(error).__name__}: {error}"},
                    )
                if discovery["registered_source_ids"]:
                    campaign = self.campaigns.progress(
                        campaign_id,
                        message="Governed search provider registered target-bound source leads",
                        details={
                            "provider_id": provider["id"],
                            "discovery_run_id": discovery["id"],
                            "registered_source_ids": discovery["registered_source_ids"],
                            "rights_auto_approved": False,
                        },
                    )
                    return self._result(
                        campaign,
                        "EVIDENCE_SOURCES_DISCOVERED",
                        {**facts, "discovery": discovery},
                    )
                if discovery["status"] == "FAILED":
                    campaign = self.campaigns.pause(
                        campaign_id,
                        reason="all governed provider queries failed and require review",
                    )
                    return self._result(
                        campaign,
                        "EVIDENCE_DISCOVERY_FAILED",
                        {**facts, "discovery": discovery},
                    )
            campaign = self.campaigns.pause(
                campaign_id,
                reason="no governed source records exist for the resolved target",
            )
            return self._result(
                campaign,
                "EVIDENCE_ACQUISITION_REQUIRED",
                {
                    **facts,
                    "next_best_evidence": EvidenceAcquisitionStore(
                        self.project
                    ).analyze_coverage(facts["target_id"]),
                },
            )

        governance = EvidenceAcquisitionStore(self.project).audit(facts["target_id"])
        if not governance["governance_complete"]:
            campaign = self.campaigns.pause(
                campaign_id, reason="source rights or access governance requires named review"
            )
            return self._result(
                campaign,
                "SOURCE_GOVERNANCE_REVIEW_REQUIRED",
                {**facts, "rights_audit": governance},
            )
        conflicts = EvidenceConflictStore(self.project).audit(facts["target_id"], record=True)
        if conflicts["unresolved_blocking_count"]:
            campaign = self.campaigns.pause(
                campaign_id,
                reason="target-incompatible evidence requires exclusion or configuration review",
            )
            return self._result(
                campaign,
                "EVIDENCE_CONFLICT_REVIEW_REQUIRED",
                {**facts, "conflict_audit": conflicts},
            )
        EvidenceDuplicateStore(self.project).audit(facts["target_id"], record=True)
        if facts["acquired_source_count"] == 0:
            candidates = [
                item
                for item in EvidenceAcquisitionStore(self.project).list(facts["target_id"])
                if item["status"] == "DISCOVERED"
                and item["source"].get("direct_media") is True
                and str(item["source"].get("origin", "")).startswith(("http://", "https://"))
            ]
            if candidates:
                candidate = max(
                    candidates, key=lambda item: float(item["source"].get("quality_score", 0.0))
                )
                try:
                    acquired = EvidenceAcquisitionStore(self.project).acquire_url(candidate["id"])
                except Exception as error:
                    campaign = self.campaigns.pause(
                        campaign_id,
                        reason="governed direct-media acquisition failed and requires review",
                    )
                    return self._result(
                        campaign,
                        "EVIDENCE_ACQUISITION_FAILED",
                        {
                            **facts,
                            "source_id": candidate["id"],
                            "error": f"{type(error).__name__}: {error}",
                        },
                    )
                campaign = self.campaigns.progress(
                    campaign_id,
                    message="Highest-value governed direct media acquired",
                    details={
                        "source_id": candidate["id"],
                        "reference_id": acquired["reference_id"],
                        "artifact_digest": acquired["reference"]["artifact"]["digest"],
                    },
                )
                return self._result(
                    campaign, "EVIDENCE_SOURCE_ACQUIRED", {**facts, "acquired": acquired}
                )
        if facts["image_reference_count"] == 0:
            references = {
                item["id"]: item for item in ReferenceIngestor(self.project).list()
            }
            video_source = next(
                (
                    references.get(item.get("reference_id"))
                    for item in EvidenceAcquisitionStore(self.project).list(facts["target_id"])
                    if item["status"] == "ACQUIRED" and item.get("reference_id")
                    and references.get(item.get("reference_id"), {}).get(
                        "media_type", ""
                    ).startswith("video/")
                ),
                None,
            )
            if video_source is not None and facts["video_analysis_count"] == 0:
                video = CaptureService(self.project).extract_video(
                    self.project.root / video_source["relative_path"],
                    rights_state=video_source["rights_state"],
                    interval_seconds=1.0,
                    maximum_frames=300,
                )
                campaign = self.campaigns.progress(
                    campaign_id,
                    message="Video frames extracted, shot-ranked, and governed",
                    details={
                        "source_reference_id": video["source_reference"]["id"],
                        "frame_count": len(video["frame_references"]),
                        "acceptance_reference_ids": video["intelligence"][
                            "acceptance_reference_ids"
                        ],
                    },
                )
                return self._result(
                    campaign, "VIDEO_KEYFRAMES_COMPLETE", {**facts, "video": video}
                )
            campaign = self.campaigns.pause(
                campaign_id, reason="unavailable critical image evidence requires acquisition"
            )
            return self._result(
                campaign,
                "EVIDENCE_ACQUISITION_REQUIRED",
                {
                    **facts,
                    "next_best_evidence": EvidenceAcquisitionStore(
                        self.project
                    ).analyze_coverage(facts["target_id"]),
                },
            )
        if facts["camera_solution_count"] == 0:
            solution = CameraSolver(self.project).solve(camera_backend)
            campaign = self.campaigns.progress(
                campaign_id,
                message="Camera ensemble completed",
                details={
                    "solution_id": solution["id"],
                    "backend": solution["backend"],
                    "approved": solution["approved"],
                    "camera_count": len(solution["cameras"]),
                },
            )
            return self._result(
                campaign,
                "CAMERA_ENSEMBLE_COMPLETE",
                {**facts, "camera_solution": solution},
            )
        if facts["scene_count"] == 0:
            if facts["authoritative_dimension_axes"] != ["x", "y", "z"]:
                campaign = self.campaigns.pause(
                    campaign_id,
                    reason="user-owned physical capture or authoritative dimensions are required",
                )
                return self._result(campaign, "PHYSICAL_DIMENSIONS_REQUIRED", facts)
            portfolio_id = campaign["configuration"].get("portfolio_id")
            if not portfolio_id:
                raise ValueError("autonomous campaign is missing its portfolio id")
            job = Coordinator(self.project).run(
                "portfolio.execute_parametric_seed", {"portfolio_id": portfolio_id}
            )
            if job["status"] != "succeeded":
                campaign = self.campaigns.pause(
                    campaign_id, reason="parametric seed worker failed or is unavailable"
                )
                return self._result(
                    campaign, "PARAMETRIC_SEED_FAILED", {**facts, "job": job}
                )
            campaign = self.campaigns.progress(
                campaign_id,
                message="Editable parametric seed generated",
                details={
                    "job_id": job["id"],
                    "scene_id": job["result"]["generated_scene"]["id"],
                    "accepted": False,
                },
            )
            return self._result(
                campaign, "PARAMETRIC_SEED_COMPLETE", {**facts, "job": job}
            )
        if facts["approved_metric_camera_solution_count"] == 0:
            portfolio_id = campaign["configuration"].get("portfolio_id")
            if portfolio_id and facts["proposed_portfolio_candidate_count"] > 0:
                job = Coordinator(self.project).run(
                    "portfolio.execute_initial", {"portfolio_id": portfolio_id}
                )
                campaign = self.campaigns.progress(
                    campaign_id,
                    message="Cheap reconstruction portfolio lanes evaluated",
                    details={
                        "job_id": job["id"],
                        "status": job["status"],
                        "acceptance_performed": False,
                    },
                )
                return self._result(
                    campaign, "PORTFOLIO_INITIAL_LANES_COMPLETE", {**facts, "job": job}
                )
            role_tasks = RoleTaskStore(self.project).ensure_metric_camera_boundary(
                campaign_id,
                evidence={
                    "camera_solution_count": facts["camera_solution_count"],
                    "approved_metric_camera_solution_count": facts[
                        "approved_metric_camera_solution_count"
                    ],
                    "image_reference_count": facts["image_reference_count"],
                    "authoritative_dimension_axes": facts["authoritative_dimension_axes"],
                },
            )
            campaign = self.campaigns.pause(
                campaign_id,
                reason="metric camera recovery and explicit human camera review are required",
            )
            return self._result(
                campaign,
                "METRIC_CAMERA_REVIEW_REQUIRED",
                {
                    **facts,
                    "next_best_evidence": {
                        "preferred": {
                            "method": "reviewed_2d_3d_pnp_landmarks",
                            "requirements": [
                                "undistorted pinhole intrinsics",
                                "at least six named non-coplanar correspondences per view",
                                "image coordinates in pixels",
                                "model coordinates in millimetres",
                                "authoritative x/y/z dimension measurement bindings",
                                "named landmark reviewer",
                                "separate final camera approval",
                            ],
                        },
                        "alternative": {
                            "method": "calibration_board_capture",
                            "requirements": [
                                "three or more board views",
                                "authoritative square-size measurement",
                                "explicit final camera approval",
                            ],
                        },
                        "prohibited_shortcut": (
                            "COLMAP relative translations must not be relabeled metric by "
                            "heuristic scale fitting"
                        ),
                    },
                    "role_tasks": role_tasks,
                },
            )
        if not facts["mandatory_render_suite_complete"]:
            job = Coordinator(self.project).run("blender.render", {})
            campaign = self.campaigns.progress(
                campaign_id,
                message="Mandatory governed render suite executed",
                details={"job_id": job["id"], "status": job["status"]},
            )
            return self._result(campaign, "RENDER_COMPLETE", {**facts, "job": job})
        if not facts["comparison_coverage_complete"]:
            job = Coordinator(self.project).run("validation.compare", {})
            campaign = self.campaigns.progress(
                campaign_id,
                message="All-view residual comparison executed",
                details={"job_id": job["id"], "status": job["status"]},
            )
            return self._result(campaign, "COMPARISON_COMPLETE", {**facts, "job": job})
        if facts["passed_candidate_evaluation_count"] == 0:
            blocked_evidence_terms = self._blocked_evidence_terms()
            if blocked_evidence_terms:
                pursuit_job = Coordinator(self.project).run(
                    "evidence.pursue_missing",
                    {
                        "target_id": facts["target_id"],
                        "category": campaign.get("configuration", {}).get(
                            "category", "general_product"
                        ),
                        "required_terms": blocked_evidence_terms,
                    },
                )
                if pursuit_job["status"] != "succeeded":
                    campaign = self.campaigns.pause(
                        campaign_id,
                        reason="governed missing-evidence pursuit failed",
                    )
                    return self._result(
                        campaign,
                        "MISSING_EVIDENCE_PURSUIT_FAILED",
                        {**facts, "pursuit_job": pursuit_job},
                    )
                pursuit = pursuit_job["result"]
                if pursuit["status"] == "SOURCES_DISCOVERED":
                    campaign = self.campaigns.progress(
                        campaign_id,
                        message="Targeted governed discovery found new evidence leads",
                        details={
                            "pursuit_id": pursuit["id"],
                            "focus_terms": pursuit["focus_terms"],
                            "acceptance_performed": False,
                        },
                    )
                    return self._result(
                        campaign,
                        "MISSING_EVIDENCE_DISCOVERED",
                        {**facts, "pursuit_job": pursuit_job, "pursuit": pursuit},
                    )
                if pursuit["status"] == "EVIDENCE_CEILING":
                    campaign = self.campaigns.stop(
                        campaign_id,
                        reason="remaining uncertainty requires external evidence",
                        result={
                            "evidence_ceiling": pursuit,
                            "blocked_evidence_terms": blocked_evidence_terms,
                        },
                    )
                    return self._result(
                        campaign,
                        "EVIDENCE_CEILING_REACHED",
                        {**facts, "pursuit_job": pursuit_job, "pursuit": pursuit},
                    )
            fit_targets = self._multiview_fit_targets()
            if fit_targets:
                target = fit_targets[0]
                component_id = target["component_ids"][0]
                searches = [
                    item
                    for item in MultiviewSearchStore(self.project).list()
                    if item["component_id"] == component_id
                    and target["semantic_id"] in item["semantic_ids"]
                    and item["status"] != "STALE"
                ]
                search = searches[-1] if searches else None
                if search is None:
                    parameter_bounds = self._automatic_parameter_bounds(component_id)
                    if parameter_bounds:
                        search = MultiviewSearchStore(self.project).start(
                            component_id,
                            semantic_ids=[target["semantic_id"]],
                            camera_solution_id=facts[
                                "approved_metric_camera_solution_ids"
                            ][0],
                            parameter_bounds=parameter_bounds,
                            maximum_candidates=7,
                        )
                if search is not None and search["status"] in {"PLANNED", "RUNNING"}:
                    job = Coordinator(self.project).run(
                        "optimization.execute_multiview_search",
                        {"search_id": search["id"]},
                    )
                    search = job.get("result") or MultiviewSearchStore(self.project).get(
                        search["id"]
                    )
                    if job["status"] != "succeeded" or search["status"] == "FAILED":
                        campaign = self.campaigns.pause(
                            campaign_id,
                            reason="bounded multiview search exhausted its governed attempts",
                        )
                        return self._result(
                            campaign,
                            "MULTIVIEW_SEARCH_FAILED",
                            {**facts, "job": job, "search": search},
                        )
                    if search["status"] != "COMPLETE":
                        campaign = self.campaigns.progress(
                            campaign_id,
                            message="Bounded fixed-camera multiview search made resumable progress",
                            details={
                                "search_id": search["id"],
                                "status": search["status"],
                                "acceptance_performed": False,
                            },
                        )
                        return self._result(
                            campaign,
                            "MULTIVIEW_SEARCH_PROGRESS",
                            {**facts, "job": job, "search": search},
                        )
                if search is not None and search["status"] == "COMPLETE":
                    with self.project.connection() as connection:
                        optimization = connection.execute(
                            "SELECT status,result_json,evaluations_json "
                            "FROM optimization_runs WHERE id=?",
                            (search["optimization_run_id"],),
                        ).fetchone()
                    optimization_status = optimization["status"] if optimization else None
                    if optimization_status == "accepted":
                        result = json.loads(optimization["result_json"])
                        evaluations = json.loads(optimization["evaluations_json"])
                        selected_candidate_id = next(
                            (
                                evaluation.get("diagnostics", {}).get("candidate_id")
                                for evaluation in evaluations
                                if evaluation["index"] == result["best_candidate_index"]
                            ),
                            None,
                        )
                        selected = next(
                            (
                                item
                                for item in search["candidates"]
                                if item["id"] == selected_candidate_id
                            ),
                            None,
                        )
                        transaction_evidence = {
                            **facts,
                            "search": search,
                            "selected_candidate": selected,
                            "required_gate_categories": [
                                "camera",
                                "measurement",
                                "component",
                                "topology",
                                "material",
                                "appearance",
                                "provenance",
                            ],
                        }
                        role_tasks = RoleTaskStore(
                            self.project
                        ).ensure_candidate_evaluation_boundary(
                            campaign_id, evidence=transaction_evidence
                        )
                        campaign = self.campaigns.pause(
                            campaign_id,
                            reason=(
                                "selected candidate requires a reviewed seven-category "
                                "transaction"
                            ),
                        )
                        return self._result(
                            campaign,
                            "CANDIDATE_TRANSACTION_REQUIRED",
                            {**transaction_evidence, "role_tasks": role_tasks},
                        )
                    fit_evidence = {
                        "camera_authority": "approved_metric",
                        "targets": fit_targets,
                        "comparison_count": facts["comparison_count"],
                        "search": search,
                        "optimization_status": optimization_status,
                        "acceptance_performed": False,
                    }
                    role_tasks = RoleTaskStore(
                        self.project
                    ).ensure_multiview_fit_boundary(campaign_id, evidence=fit_evidence)
                    campaign = self.campaigns.pause(
                        campaign_id,
                        reason="multiview optimization requires named review before mutation",
                    )
                    state = (
                        "MULTIVIEW_OPTIMIZATION_REVIEW_REQUIRED"
                        if optimization_status == "proposed"
                        else "MULTIVIEW_SEARCH_REVISION_REQUIRED"
                    )
                    return self._result(
                        campaign,
                        state,
                        {**facts, **fit_evidence, "role_tasks": role_tasks},
                    )
                fit_evidence = {
                    "camera_authority": "approved_metric",
                    "targets": fit_targets,
                    "comparison_count": facts["comparison_count"],
                    "required_tool": "recon.start_multiview_search",
                    "candidate_contract": {
                        "parameters": "bounded scalar semantic parameters",
                        "terms": "silhouette loss must equal stored comparison IoUs",
                        "diagnostics": "comparison_ids covering every locality-plan view",
                    },
                }
                role_tasks = RoleTaskStore(self.project).ensure_multiview_fit_boundary(
                    campaign_id, evidence=fit_evidence
                )
                return self._result(
                    campaign,
                    "MULTIVIEW_COMPONENT_FIT_REQUIRED",
                    {**facts, **fit_evidence, "role_tasks": role_tasks},
                )
            transaction_evidence = {
                **facts,
                "required_gate_categories": [
                    "camera",
                    "measurement",
                    "component",
                    "topology",
                    "material",
                    "appearance",
                    "provenance",
                ],
            }
            role_tasks = RoleTaskStore(
                self.project
            ).ensure_candidate_evaluation_boundary(
                campaign_id, evidence=transaction_evidence
            )
            campaign = self.campaigns.pause(
                campaign_id,
                reason="candidate requires a reviewed seven-category atomic transaction",
            )
            return self._result(
                campaign,
                "CANDIDATE_TRANSACTION_REQUIRED",
                {**transaction_evidence, "role_tasks": role_tasks},
            )
        if facts["promoted_scene_count"] == 0:
            promotion_evidence = {
                **facts,
                "verified_passed_evaluation_ids": facts.get("verification", {}).get(
                    "verified_passed_evaluation_ids", []
                ),
                "required_tools": ["candidate.accept", "candidate.promote"],
            }
            role_tasks = RoleTaskStore(self.project).ensure_safe_promotion_boundary(
                campaign_id, evidence=promotion_evidence
            )
            campaign = self.campaigns.pause(
                campaign_id,
                reason="passed candidate requires named acceptance and safe promotion",
            )
            return self._result(
                campaign,
                "SAFE_PROMOTION_REQUIRED",
                {**promotion_evidence, "role_tasks": role_tasks},
            )
        delivery_job = Coordinator(self.project).run(
            "workflow.deliver_promoted",
            {"scene_id": facts["promoted_scene_id"]},
        )
        if delivery_job["status"] != "succeeded":
            campaign = self.campaigns.pause(
                campaign_id, reason="promoted-scene delivery worker failed"
            )
            return self._result(
                campaign,
                "DELIVERY_FAILED",
                {**facts, "delivery_job": delivery_job},
            )
        delivery = delivery_job["result"]
        if not delivery["accepted"]:
            campaign = self.campaigns.pause(
                campaign_id,
                reason="final verified receipt still reports acceptance blockers",
            )
            return self._result(
                campaign,
                "DELIVERY_ACCEPTANCE_BLOCKED",
                {**facts, "delivery_job": delivery_job, "delivery": delivery},
            )
        campaign = self.campaigns.stop(
            campaign_id,
            reason="all requested gates pass and verified delivery completed",
            result={
                "delivery": delivery,
                "accepted_fidelity": delivery["acceptance"]["accepted_fidelity"],
            },
        )
        return self._result(
            campaign,
            "DELIVERY_COMPLETE",
            {**facts, "delivery_job": delivery_job, "delivery": delivery},
            accepted=True,
        )

    def _facts(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            target = connection.execute(
                "SELECT id,status FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            camera_rows = connection.execute(
                "SELECT id,backend,solution_json,approved,decision_id,decision_digest "
                "FROM camera_solutions"
            ).fetchall()
            axes = sorted(
                {
                    json.loads(row["value_json"]).get("axis")
                    for row in connection.execute(
                        "SELECT value_json FROM measurements "
                        "WHERE type='known_overall_dimension' "
                        "AND evidence_class IN ('MEASURED','MANUFACTURER_SPEC')"
                    )
                }
                - {None}
            )
            scene_rows = connection.execute(
                "SELECT id,state,is_authoritative,artifact_digest,relative_path FROM scene_assets"
            ).fetchall()
            render_rows = connection.execute(
                "SELECT id,scene_id,camera_solution_id,outputs_json FROM render_runs"
            ).fetchall()
            comparison_rows = connection.execute(
                "SELECT id,reference_id,render_digest,residual_digest,metrics_json,"
                "receipt_digest,created_at "
                "FROM comparisons"
            ).fetchall()
            artifact_rows = {
                row["digest"]: dict(row)
                for row in connection.execute(
                    "SELECT digest,size,relative_path FROM artifacts"
                ).fetchall()
            }
            source_rows = connection.execute(
                "SELECT id,status FROM evidence_sources"
            ).fetchall()
            image_references = connection.execute(
                "SELECT id FROM reference_items WHERE media_type LIKE 'image/%' "
                "AND acceptance_eligible=1"
            ).fetchall()
            video_analysis = connection.execute(
                "SELECT COUNT(*) FROM video_analysis_runs"
            ).fetchone()[0]
            proposed_portfolio = connection.execute(
                "SELECT COUNT(*) FROM reconstruction_candidates WHERE status='PROPOSED' "
                "AND lane IN ('classical_photogrammetry','learned_multiview_geometry',"
                "'generative_image_to_3d','visual_hull','gaussian_visual_oracle',"
                "'hybrid_semantic_reconstruction')"
            ).fetchone()[0]
        target_authority = (
            TargetResolver(self.project).authority_status(target["id"])
            if target
            else None
        )
        artifact_store = ArtifactStore(self.project)
        source_authority = EvidenceAcquisitionStore(self.project)
        evidence_sources = len(source_rows)
        acquired_sources = sum(
            row["status"] == "ACQUIRED"
            and source_authority.authority_status(row["id"])["acquisition_valid"]
            for row in source_rows
        )

        def verified_artifact(digest: Any, relative_path: Any = None) -> bool:
            if not isinstance(digest, str) or digest not in artifact_rows:
                return False
            try:
                row = artifact_rows[digest]
                artifact_path = artifact_store.path_for(digest).resolve()
                registered_path = (self.project.root / str(row["relative_path"])).resolve()
                registered_path.relative_to(self.project.root.resolve())
                if registered_path != artifact_path:
                    return False
                paths = [artifact_path]
                if relative_path is not None:
                    materialized = (self.project.root / str(relative_path)).resolve()
                    materialized.relative_to(self.project.root.resolve())
                    paths.append(materialized)
                for index, path in enumerate(paths):
                    if not path.is_file():
                        return False
                    actual_digest, actual_size = sha256_file(path)
                    if actual_digest != digest or (
                        index == 0 and actual_size != int(row["size"])
                    ):
                        return False
                return True
            except (OSError, TypeError, ValueError):
                return False

        eligible_reference_ids = {str(row["id"]) for row in image_references}
        valid_scene_ids = {
            str(row["id"])
            for row in scene_rows
            if verified_artifact(row["artifact_digest"], row["relative_path"])
        }
        decision_store = CameraDecisionStore(self.project)
        valid_camera_ids = []
        approved_metric_ids = []
        for row in camera_rows:
            document = json.loads(row["solution_json"])
            cameras = document.get("cameras", [])
            try:
                complete = bool(cameras) and all(
                    validate_complete_camera_state(camera) for camera in cameras
                )
            except (KeyError, TypeError, ValueError):
                complete = False
            record = {**dict(row), "solution": document}
            decision_valid = decision_store.verify_record(record)["valid"]
            covered = {str(camera.get("reference_id")) for camera in cameras}
            if complete and decision_valid:
                valid_camera_ids.append(str(row["id"]))
            if (
                complete
                and decision_valid
                and bool(row["approved"])
                and covered == eligible_reference_ids
                and len(cameras) == len(eligible_reference_ids)
                and all(
                    camera.get("registration_class") == RegistrationClass.METRIC.value
                    for camera in cameras
                )
            ):
                approved_metric_ids.append(str(row["id"]))

        verified_render_runs = []
        valid_render_digests: set[str] = set()
        for row in render_rows:
            if (
                str(row["scene_id"]) not in valid_scene_ids
                or str(row["camera_solution_id"]) not in approved_metric_ids
            ):
                continue
            try:
                outputs = json.loads(row["outputs_json"])
            except (json.JSONDecodeError, TypeError):
                continue
            if not isinstance(outputs, list) or not outputs:
                continue
            output_references = {str(output.get("reference_id")) for output in outputs}
            if output_references != eligible_reference_ids:
                continue
            run_valid = True
            run_digests: set[str] = set()
            for output in outputs:
                primary_digest = output.get("artifact_digest")
                pass_digests = output.get("pass_artifact_digests", {})
                if (
                    not isinstance(pass_digests, dict)
                    or set(pass_digests) < GOVERNED_RENDER_PASSES
                    or not verified_artifact(primary_digest, output.get("relative_path"))
                    or not all(
                        verified_artifact(pass_digests.get(name))
                        for name in GOVERNED_RENDER_PASSES
                    )
                ):
                    run_valid = False
                    break
                run_digests.add(str(primary_digest))
                run_digests.update(str(value) for value in pass_digests.values())
            if run_valid:
                verified_render_runs.append(str(row["id"]))
                valid_render_digests.update(run_digests)

        valid_comparison_ids = []
        compared_reference_ids: set[str] = set()
        comparison_store = ComparisonStore(self.project)
        for row in comparison_rows:
            try:
                metrics = json.loads(row["metrics_json"])
                silhouette_iou = float(metrics["silhouette_iou"])
            except (KeyError, TypeError, ValueError, json.JSONDecodeError):
                continue
            if (
                str(row["reference_id"]) in eligible_reference_ids
                and str(row["render_digest"]) in valid_render_digests
                and comparison_store.verify_record(row, replay=True)["valid"]
                and math.isfinite(silhouette_iou)
                and 0.0 <= silhouette_iou <= 1.0
            ):
                valid_comparison_ids.append(str(row["id"]))
                compared_reference_ids.add(str(row["reference_id"]))

        lifecycle = audit_scene_lifecycle(self.project)
        promoted_scene_id = (
            str(lifecycle["authoritative_scene_id"])
            if lifecycle["authoritative_promotion_chain_valid"]
            and lifecycle["authoritative_scene_id"]
            else None
        )
        return {
            "target_id": target["id"] if target else None,
            "target_status": (
                target["status"]
                if target and target_authority and target_authority["valid"]
                else "INVALID"
                if target
                else None
            ),
            "target_authority": target_authority,
            "evidence_source_count": evidence_sources,
            "acquired_source_count": acquired_sources,
            "image_reference_count": len(eligible_reference_ids),
            "video_analysis_count": video_analysis,
            "camera_solution_count": len(camera_rows),
            "approved_metric_camera_solution_count": len(approved_metric_ids),
            "approved_metric_camera_solution_ids": sorted(approved_metric_ids),
            "authoritative_dimension_axes": axes,
            "scene_count": len(valid_scene_ids),
            "render_run_count": len(verified_render_runs),
            "mandatory_render_suite_complete": bool(verified_render_runs),
            "comparison_count": len(valid_comparison_ids),
            "comparison_coverage_complete": bool(eligible_reference_ids)
            and compared_reference_ids == eligible_reference_ids,
            "passed_candidate_evaluation_count": len(
                lifecycle["verified_passed_evaluation_ids"]
            ),
            "promoted_scene_count": int(promoted_scene_id is not None),
            "promoted_scene_id": promoted_scene_id,
            "proposed_portfolio_candidate_count": proposed_portfolio,
            "verification": {
                "valid_scene_ids": sorted(valid_scene_ids),
                "valid_camera_solution_ids": sorted(valid_camera_ids),
                "verified_render_run_ids": sorted(verified_render_runs),
                "valid_comparison_ids": sorted(valid_comparison_ids),
                "compared_reference_ids": sorted(compared_reference_ids),
                "invalid_evaluation_ids": lifecycle["invalid_evaluation_ids"],
                "verified_passed_evaluation_ids": lifecycle[
                    "verified_passed_evaluation_ids"
                ],
                "invalid_transition_ids": lifecycle["invalid_transition_ids"],
            },
        }

    def _multiview_fit_targets(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            node_rows = connection.execute(
                "SELECT id,record_json FROM semantic_nodes ORDER BY created_at,id"
            ).fetchall()
            comparison_references = {
                row[0]
                for row in connection.execute(
                    "SELECT DISTINCT reference_id FROM comparisons"
                ).fetchall()
            }
        targets = []
        for row in node_rows:
            node = json.loads(row["record_json"])
            geometry = node.get("geometry") or {}
            component_ids = sorted(set(geometry.get("component_ids", [])))
            references = sorted(set(node.get("references", [])) & comparison_references)
            if component_ids and len(references) >= 2:
                targets.append(
                    {
                        "semantic_id": row["id"],
                        "component_ids": component_ids,
                        "reference_ids": references,
                    }
                )
        return targets

    def _automatic_parameter_bounds(self, component_id: str) -> dict[str, list[float]]:
        component = ComponentStore(self.project).get(component_id)
        bounds: dict[str, list[float]] = {}
        for name, raw in sorted(component["parameters"].items()):
            if isinstance(raw, bool) or not isinstance(raw, (int, float)):
                continue
            value = float(raw)
            if not math.isfinite(value):
                continue
            delta = max(abs(value) * 0.05, 0.1)
            lower = value - delta
            if value >= 0.0:
                lower = max(0.0, lower)
            bounds[name] = [lower, value + delta]
            if len(bounds) == 3:
                break
        return bounds

    def _blocked_evidence_terms(self) -> list[str]:
        with self.project.connection() as connection:
            evaluation = connection.execute(
                "SELECT gates_json,created_at FROM candidate_evaluations "
                "WHERE status='FAILED' ORDER BY created_at DESC,id DESC LIMIT 1"
            ).fetchone()
            latest_evidence = connection.execute(
                "SELECT MAX(timestamp) FROM ("
                "SELECT MAX(updated_at) AS timestamp FROM evidence_sources "
                "UNION ALL SELECT MAX(created_at) FROM reference_items "
                "UNION ALL SELECT MAX(created_at) FROM measurements "
                "UNION ALL SELECT MAX(created_at) FROM camera_solutions)"
            ).fetchone()[0]
        if evaluation is None or (
            latest_evidence and latest_evidence > evaluation["created_at"]
        ):
            return []
        category_terms = {
            "camera": ["calibration board", "reviewed camera landmarks"],
            "measurement": ["dimensions", "technical drawing"],
            "component": ["component close-up", "parts diagram"],
            "topology": ["teardown", "technical drawing"],
            "material": ["material close-up", "controlled lighting reference"],
            "appearance": ["controlled lighting reference"],
            "provenance": ["official manual", "manufacturer specifications"],
        }
        terms = []
        for gate in json.loads(evaluation["gates_json"]):
            if bool(gate.get("mandatory", True)) and gate.get("status") == "BLOCKED":
                terms.extend(category_terms.get(gate.get("category"), []))
        return list(dict.fromkeys(terms))

    @staticmethod
    def _result(
        campaign: dict[str, Any],
        state: str,
        evidence: dict[str, Any],
        *,
        accepted: bool = False,
    ) -> dict[str, Any]:
        return {
            "workflow_state": state,
            "campaign": campaign,
            "evidence": evidence,
            "accepted": accepted,
        }
