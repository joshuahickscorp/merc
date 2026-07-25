from __future__ import annotations

import json
from pathlib import Path

import pytest
from mcp.server.fastmcp.exceptions import ToolError

from blender_vision.mcp.server import create_server
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator


def test_queued_job_can_be_cancelled_before_execution(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Jobs Test")
    coordinator = Coordinator(project)
    job_id = coordinator.enqueue("validation.coverage", {})
    project.request_cancel(job_id)
    result = coordinator.execute(job_id)
    assert result["status"] == "cancelled"
    assert [event["event"] for event in result["events"]] == [
        "queued",
        "cancel_requested",
        "cancelled",
    ]


def test_exact_work_reuses_cached_result(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Cache Test")
    coordinator = Coordinator(project)
    first = coordinator.run("validation.coverage", {})
    second = coordinator.run("validation.coverage", {})
    assert first["status"] == "succeeded"
    assert first["result"]["cache_hit"] is False
    assert second["result"]["cache_hit"] is True
    assert len(project.cache_entries()) == 1
    with project.connection() as connection:
        provenance = connection.execute(
            "SELECT input_hashes_json,execution_json,output_hashes_json "
            "FROM job_provenance ORDER BY updated_at"
        ).fetchall()
    assert len(provenance) == 2
    assert json.loads(provenance[0]["input_hashes_json"])
    assert json.loads(provenance[1]["execution_json"])["cache_hit"] is True
    assert json.loads(provenance[0]["output_hashes_json"])


@pytest.mark.asyncio
async def test_mcp_discovers_required_vertical_slice_tools(tmp_path: Path) -> None:
    server = create_server(tmp_path / "projects")
    tools = await server.list_tools()
    names = {tool.name for tool in tools}
    assert {
        "system.doctor",
        "system.resource_profiles",
        "system.warm_service_update",
        "system.warm_service_evict",
        "system.warm_service_list",
        "vision.observe",
        "vision.query",
        "vision.verify",
        "vision.adapters",
        "campaign.start",
        "campaign.advance",
        "campaign.progress",
        "campaign.pause",
        "campaign.resume",
        "campaign.status",
        "campaign.stop",
        "role.assign",
        "role.waiting",
        "role.complete",
        "role.list",
        "context.create_packet",
        "workflow.reconstruct_from_public_evidence",
        "workflow.reconstruct_from_user_capture",
        "workflow.continue_autonomous",
        "workflow.progress",
        "workflow.generate_original_asset",
        "workflow.reconstruct_vehicle",
        "workflow.reconstruct_hardware",
        "workflow.reconstruct_packaging",
        "workflow.reconstruct_organic_subject",
        "target.resolve",
        "category.list",
        "category.select",
        "semantic_model.bootstrap",
        "semantic_model.bind",
        "semantic_model.extend",
        "portfolio.generate",
        "portfolio.record_result",
        "portfolio.execute_parametric_seed",
        "portfolio.execute_initial",
        "portfolio.rank",
        "portfolio.fusion_plan",
        "generative3d.generate",
        "generative3d.import_result",
        "evidence.search",
        "evidence.acquire",
        "evidence.acquire_url",
        "evidence.audit",
        "evidence.propose_legacy_reference_adoption",
        "evidence.review_legacy_reference_adoption",
        "evidence.list_legacy_reference_adoptions",
        "evidence.review_governance",
        "evidence.deduplicate",
        "evidence.resolve_conflicts",
        "video.ingest",
        "video.extract_keyframes",
        "coverage.analyze",
        "coverage.observe_surface",
        "coverage.observe_governed_source",
        "coverage.acquire_missing",
        "camera.freeze",
        "camera.derive_undistorted",
        "candidate.evaluate_transaction",
        "candidate.rank",
        "candidate.reject",
        "candidate.accept",
        "candidate.promote",
        "benchmark.bootstrap_calibration",
        "benchmark.beast_audit",
        "benchmark.bootstrap_dgx_spark",
        "benchmark.bootstrap_rtx_5090_fe",
        "benchmark.bootstrap_external_perseverance",
        "benchmark.revise_rtx_5090_fe_candidate",
        "benchmark.refine_rtx_5090_fe_visual_candidate",
        "benchmark.refine_rtx_5090_fe_front_frame_candidate",
        "benchmark.refine_dgx_spark_visual_candidate",
        "benchmark.refine_dgx_spark_base_foot_candidate",
        "model.approve_source",
        "model.import_checkpoint",
        "model.list",
        "project.create",
        "project.open",
        "project.audit",
        "reference.import",
        "reference.import_reviewed_mask",
        "reference.propose_masks",
        "reference.review_mask_proposal",
        "reference.list_mask_proposals",
        "reference.list_masks",
        "reference.audit_derivations",
        "reference.extract_video",
        "reference.extract_pdf",
        "reference.select_frames",
        "reference.coverage",
        "measurement.calibrate",
        "measurement.add",
        "measurement.add_physical",
        "measurement.link",
        "measurement.bind_source_provenance",
        "measurement.correct",
        "measurement.grid_create",
        "measurement.grid_list",
        "feature.add",
        "feature.review",
        "feature.link",
        "material.create",
        "material.review",
        "material.list",
        "component.create",
        "component.fit",
        "component.review_fit",
        "component.generate",
        "component.reconstruct",
        "component.repair",
        "recon.bootstrap",
        "recon.solve",
        "recon.audit_existing",
        "recon.next_best_view",
        "workflow.fit_component",
        "blender.inspect",
        "blender.render",
        "blender.export",
        "blender.export_blend",
        "blender.generate_lod",
        "vision.solve_cameras",
        "vision.compare_camera_solutions",
        "vision.refine_camera",
        "vision.run",
        "photogrammetry.run",
        "geometry.run_ensemble",
        "vision.import_geometry_evidence",
        "vision.compare_backends",
        "vision.import_camera_solution",
        "vision.solve_calibration_board",
        "vision.propose_pnp_landmarks",
        "vision.propose_pnp_landmarks_from_renders",
        "vision.review_pnp_landmarks",
        "vision.solve_pnp_landmarks",
        "vision.solve_vanishing_points",
        "vision.review_camera_solution",
        "validation.plan_locality",
        "validation.compare",
        "visual_geometry.create_rig",
        "visual_geometry.freeze_baseline",
        "visual_geometry.bind_scene",
        "visual_geometry.review_binding",
        "visual_geometry.repropose_binding",
        "visual_geometry.relate_components",
        "visual_geometry.create_component_packet",
        "visual_geometry.score_frequencies",
        "visual_geometry.evaluate",
        "visual_geometry.diagnose_residuals",
        "visual_geometry.audit_manufactured_form",
        "visual_geometry.repair_degenerate_candidate",
        "visual_geometry.list",
        "visual_geometry.verify",
        "validation.recompute_legacy_comparison",
        "validation.supersede_duplicate_comparison",
        "appearance.compare",
        "material.compare",
        "geometry.compare",
        "uncertainty.audit",
        "validation.acceptance",
        "repair.propose_mac_studio_grille",
        "repair.approve",
        "repair.apply",
        "repair.review",
        "recon.repair",
        "receipt.export",
        "review.snapshot",
        "review.request_capture",
        "review.model_tier",
        "dataset.plan_synthetic",
        "dataset.generate",
        "dataset.train_feature_model",
        "dataset.import_training_result",
        "dataset.evaluate",
        "active_learning.start",
        "active_learning.record_corrections",
        "active_learning.plan_retraining",
        "active_learning.execute_retraining",
        "active_learning.compare",
        "active_learning.promote",
        "synthetic_view.register",
        "vision.import_feature_detections",
        "validation.register_visual_oracle",
        "visual_oracle.train",
        "recon.optimize",
        "recon.optimize_multiview",
        "recon.start_multiview_search",
        "recon.continue_multiview_search",
        "recon.list_multiview_searches",
        "recon.review_optimization",
        "job.status",
        "job.cancel",
        "worker.register",
        "worker.heartbeat",
        "worker.claim",
        "worker.renew",
        "worker.complete",
        "worker.fail",
        "worker.list",
        "artifact.describe",
        "artifact.read_chunk",
        "artifact.begin_upload",
        "artifact.upload_chunk",
        "artifact.complete_upload",
        "artifact.abort_upload",
        "artifact.reap_stale_uploads",
        "workflow.audit_reference_fidelity",
        "workflow.reconstruct_product",
        "workflow.repair_existing_model",
        "workflow.prepare_capture_session",
        "workflow.prepare_acceptance",
        "workflow.deliver_promoted",
    } <= names
    templates = await server.list_resource_templates()
    uris = {str(template.uriTemplate) for template in templates}
    assert "project://{project_id}/summary" in uris
    assert "project://{project_id}/scene" in uris
    assert "project://{project_id}/cameras" in uris
    assert "project://{project_id}/repairs" in uris
    assert "project://{project_id}/review-queue" in uris
    assert "project://{project_id}/workers" in uris
    assert "project://{project_id}/models" in uris
    assert "project://{project_id}/datasets" in uris
    assert "project://{project_id}/visual-oracles" in uris
    assert "project://{project_id}/optimization" in uris
    assert "project://{project_id}/geometry" in uris
    assert "project://{project_id}/fits" in uris
    assert "project://{project_id}/materials" in uris
    assert "project://{project_id}/residuals" in uris
    prompts = await server.list_prompts()
    prompt_names = {prompt.name for prompt in prompts}
    assert {
        "audit-existing-model",
        "prepare-acceptance",
        "repair-from-evidence",
        "reconstruct-product",
        "fit-technical-component",
        "plan-capture",
        "review-uncertainty",
    } <= prompt_names


@pytest.mark.asyncio
async def test_mcp_worker_enrollment_requires_coordinator_secret(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    projects_root = tmp_path / "projects"
    project = ProjectStore.create(projects_root / "distributed", "Distributed")
    server = create_server(projects_root)
    arguments = {
        "project_path": str(project.root),
        "name": "RTX 5090",
        "worker_class": "vision",
        "capabilities": {
            "hardware": ["cuda"],
            "vram_gb": 32,
            "system_memory_gb": 64,
            "supported_models": [],
            "render_devices": ["CUDA"],
            "capabilities": ["vision.*"],
        },
        "enrollment_token": "secret",
    }
    monkeypatch.delenv("BVMCP_WORKER_ENROLLMENT_TOKEN", raising=False)
    with pytest.raises(ToolError, match="enrollment is disabled"):
        await server.call_tool("worker.register", arguments)

    monkeypatch.setenv("BVMCP_WORKER_ENROLLMENT_TOKEN", "secret")
    _content, structured = await server.call_tool("worker.register", arguments)
    assert structured["name"] == "RTX 5090"
    assert structured["worker_token"]
