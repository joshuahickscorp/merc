from __future__ import annotations

from pathlib import Path

from PIL import Image

from blender_vision.cameras.solver import CameraSolver
from blender_vision.capture.service import CaptureService
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.scenes import SceneStore
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.orchestration.context import ContextPacketStore
from blender_vision.orchestration.roles import RoleTaskStore
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.autonomous import (
    generate_original_asset,
    reconstruct_from_public_evidence,
    reconstruct_from_user_capture,
)
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def test_campaign_enforces_full_self_correcting_state_machine(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Campaign")
    store = CampaignStore(project)
    campaign = store.start(
        "repair_existing_model",
        configuration={"requested_tier": "L3"},
        resource_profile="compact",
        budget={"maximum_iterations": 3, "minimum_expected_improvement": 0.01},
    )
    campaign_id = campaign["id"]
    assert campaign["controller_state"] == "OBSERVE"
    payloads = [
        {"coverage": 0.8, "current_metrics": {"silhouette": 0.9}},
        {"diagnosis": "silhouette mismatch", "supporting_evidence": ["comparison-1"]},
        {
            "affected_components": ["body"],
            "proposed_operation": "component.repair",
            "rollback_checkpoint": "scene-baseline",
        },
        {
            "expected_metric_changes": {"silhouette": 0.02},
            "risk": "low",
            "estimated_cost": {"seconds": 30},
        },
        {"execution_record": {"job_id": "job-1", "scene_id": "candidate-1"}},
        {"render_run_id": "render-1"},
        {"metrics": {"silhouette": 0.92}},
        {"accepted": False, "reason": "aggregate regression"},
    ]
    for payload in payloads:
        campaign = store.advance(campaign_id, payload)
    assert campaign["controller_state"] == "CONTINUE"
    assert campaign["result"]["rollback_required"] is True
    stopped = store.advance(campaign_id, {"expected_improvement": 0.001})
    assert stopped["status"] == "STOPPED"
    assert stopped["result"]["stop_reason"] == "expected improvement is below threshold"
    assert project.status()["counts"]["agent_proposals"] == 1


def test_context_packet_is_component_local_and_artifact_bound(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Context")
    ComponentStore(project).create(
        ComponentSpec(
            id="body",
            type=ComponentType.BODY,
            parameters={"width_mm": 100.0},
        )
    )
    packet = ContextPacketStore(project).create(
        target_component="body",
        allowed_operations=["component.repair", "component.fit"],
        desired_gate="silhouette",
    )
    assert packet["target_component"] == "body"
    assert packet["current_parameters"]["parameters"]["width_mm"] == 100.0
    assert packet["whole_project_included"] is False
    assert len(packet["artifact"]["digest"]) == 64


def test_role_tasks_are_persistent_cost_ranked_advisory_handoffs(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Role tasks")
    campaign = CampaignStore(project).start(
        "role_test", configuration={}, resource_profile="compact"
    )
    store = RoleTaskStore(project)
    first = store.assign(
        campaign["id"],
        "Review camera reprojection and PnP pose evidence",
        confidence=0.3,
        estimated_cost=0.5,
        inputs={"camera_solution_id": "pending"},
    )
    duplicate = store.assign(
        campaign["id"],
        "Review camera reprojection and PnP pose evidence",
        confidence=0.3,
        estimated_cost=0.5,
        inputs={"camera_solution_id": "pending"},
    )
    assert first["role"] == "Camera Analyst"
    assert duplicate["id"] == first["id"]
    waiting = store.set_waiting(first["id"], reason="reviewed landmarks are missing")
    assert waiting["status"] == "WAITING_INPUT"
    completed = store.complete(
        first["id"],
        output={"finding": "relative scale only"},
        artifact_digests=[],
        completed_by="Camera reviewer",
    )
    assert completed["status"] == "COMPLETED"
    assert completed["output"]["authority"].startswith("advisory_only")
    assert project.status()["counts"]["role_tasks"] == 1


def test_public_evidence_workflow_launches_truthful_campaign(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Public workflow")
    result = reconstruct_from_public_evidence(
        project,
        target="2024 Porsche 911 GT3 RS",
        requested_tier="L4",
        configuration="factory standard",
        evidence_policy="public_internal_use",
        resource_profile="compact",
    )
    assert result["target_resolution"]["status"] == "RESOLVED"
    assert result["category_pack"]["id"] == "vehicles"
    assert result["current_evidence_ceiling"] == "L0"
    assert result["current_status"] == "EVIDENCE_ACQUISITION_REQUIRED"
    assert result["campaign"]["status"] == "RUNNING"
    assert result["semantic_graph"]["operations_require_semantic_ids"] is True
    assert len(result["portfolio"]["candidates"]) == 8
    assert "No one-to-one claim" in result["accuracy_statement"]


def test_geforce_rtx_target_selects_computer_hardware_pack() -> None:
    pack = CategoryPackRegistry().select(
        {
            "manufacturer": "NVIDIA",
            "model": "GeForce RTX 5090 Founders Edition",
            "body_style": "dual-slot graphics card",
        }
    )

    assert pack["id"] == "computer_hardware"
    assert {"fan", "blade", "heat_sink", "bracket"} <= set(pack["ontology"])


def test_user_capture_and_generated_design_remain_distinct(tmp_path: Path) -> None:
    capture_project = ProjectStore.create(tmp_path / "capture", "Capture workflow")
    front = tmp_path / "front.png"
    Image.new("RGB", (80, 60), "gray").save(front)
    capture = reconstruct_from_user_capture(
        capture_project,
        target="NVIDIA DGX Spark",
        reference_paths=[front],
        category="computer_hardware",
        resource_profile="compact",
    )
    assert capture["target_resolution"]["target"]["output_classification"] == (
        "REFERENCE RECONSTRUCTION"
    )
    assert capture["rights_audit"]["internal_use_permitted"] == 1
    assert capture["current_evidence_ceiling"] == "L1"
    assert capture["current_status"] == "READY_FOR_CAMERA_AND_CANDIDATE_EXECUTION"

    generated_project = ProjectStore.create(tmp_path / "generated", "Generated workflow")
    generated = generate_original_asset(
        generated_project,
        description="futuristic unicorn",
        resource_profile="compact",
    )
    assert generated["target_resolution"]["target"]["output_classification"] == ("GENERATED DESIGN")
    assert generated["current_evidence_ceiling"] == "L0"
    assert generated["current_status"] == "READY_FOR_GENERATIVE_PROPOSALS"
    assert "no measured-target fidelity" in generated["accuracy_statement"]


def test_autonomous_executor_solves_cameras_then_stops_at_physical_authority(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "executor", "Executor workflow")
    front = tmp_path / "executor-front.png"
    Image.new("RGB", (96, 72), "gray").save(front)
    workflow = reconstruct_from_user_capture(
        project,
        target="NVIDIA DGX Spark",
        reference_paths=[front],
        category="consumer_electronics",
        resource_profile="compact",
    )
    executor = AutonomousWorkflowExecutor(project)

    camera_step = executor.continue_once(
        workflow["campaign"]["id"], camera_backend="turntable_fallback"
    )
    dimension_step = executor.continue_once(workflow["campaign"]["id"])

    assert camera_step["workflow_state"] == "CAMERA_ENSEMBLE_COMPLETE"
    assert camera_step["evidence"]["camera_solution"]["approved"] is False
    assert dimension_step["workflow_state"] == "PHYSICAL_DIMENSIONS_REQUIRED"
    assert dimension_step["campaign"]["status"] == "PAUSED"


def test_public_executor_does_not_use_unledgered_legacy_images(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "legacy-images", "Legacy image boundary")
    target = TargetResolver(project).resolve(
        {"manufacturer": "NVIDIA", "model": "GeForce RTX 5090 Founders Edition"}
    )
    source = tmp_path / "legacy-front.png"
    Image.new("RGB", (96, 72), "gray").save(source)
    ReferenceIngestor(project).import_file(
        source, rights_state="UNKNOWN", viewpoint_label="front"
    )
    campaign = CampaignStore(project).start(
        "public_evidence_reconstruction",
        configuration={"target_id": target["id"]},
        resource_profile="compact",
    )

    result = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])

    assert result["workflow_state"] == "EVIDENCE_ACQUISITION_REQUIRED"
    assert result["campaign"]["status"] == "PAUSED"
    assert result["evidence"]["evidence_source_count"] == 0
    assert project.status()["counts"]["camera_solutions"] == 0


def test_autonomous_executor_describes_metric_camera_evidence_boundary(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "metric-boundary", "Metric boundary")
    front = tmp_path / "metric-front.png"
    Image.new("RGB", (96, 72), "gray").save(front)
    workflow = reconstruct_from_user_capture(
        project,
        target="NVIDIA DGX Spark",
        reference_paths=[front],
        category="consumer_electronics",
        resource_profile="compact",
    )
    CameraSolver(project).solve("turntable_fallback")
    measurements = MeasurementStore(project)
    for axis, value in (("x", 150.0), ("y", 150.0), ("z", 50.5)):
        measurements.add(
            "known_overall_dimension",
            {"axis": axis, "millimetres": value},
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            certainty="exact",
        )
    blend = tmp_path / "candidate.blend"
    blend.write_bytes(b"fixture")
    SceneStore(project).import_blend(blend)

    portfolio_result = AutonomousWorkflowExecutor(project).continue_once(
        workflow["campaign"]["id"]
    )
    result = AutonomousWorkflowExecutor(project).continue_once(workflow["campaign"]["id"])

    assert portfolio_result["workflow_state"] == "PORTFOLIO_INITIAL_LANES_COMPLETE"
    assert portfolio_result["evidence"]["job"]["status"] == "succeeded"
    assert result["workflow_state"] == "METRIC_CAMERA_REVIEW_REQUIRED"
    request = result["evidence"]["next_best_evidence"]
    assert request["preferred"]["method"] == "reviewed_2d_3d_pnp_landmarks"
    assert "must not be relabeled metric" in request["prohibited_shortcut"]
    assert {task["role"] for task in result["evidence"]["role_tasks"]} == {
        "Camera Analyst",
        "Capture Planner",
        "Adversarial Reviewer",
        "Acceptance Auditor",
    }


def test_autonomous_executor_acquires_reviewed_direct_media(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "direct-media", "Direct media")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Autonomous image"}
    )
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "https://example.test/front.png",
            "publisher": "Fixture",
            "page_title": "Front",
            "authority_class": "manufacturer_authoritative",
            "target_variant": {"manufacturer": "Fixture", "model": "Autonomous image"},
            "viewpoint": "front",
            "quality_score": 0.9,
            "direct_media": True,
        },
        rights={"status": "FIXTURE", "internal_use": True, "redistribution": False},
    )
    store.review_governance(
        source["id"],
        reviewed_by="Fixture reviewer",
        source_terms_review="approved",
        privacy_review="not_applicable",
    )
    image = tmp_path / "autonomous-front.png"
    Image.new("RGB", (96, 72), "gray").save(image)

    def acquire_fixture(
        instance: EvidenceAcquisitionStore, source_id: str, *, timeout_seconds: float = 30.0
    ) -> dict:
        assert timeout_seconds == 30.0
        return instance.acquire_local(source_id, image)

    monkeypatch.setattr(EvidenceAcquisitionStore, "acquire_url", acquire_fixture)
    campaign = CampaignStore(project).start(
        "external_public_benchmark",
        configuration={"target_id": target["id"]},
        resource_profile="compact",
    )

    result = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])

    assert result["workflow_state"] == "EVIDENCE_SOURCE_ACQUIRED"
    assert result["evidence"]["acquired"]["status"] == "ACQUIRED"
    assert result["campaign"]["status"] == "RUNNING"


def test_autonomous_executor_extracts_acquired_video_keyframes(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "video-media", "Video media")
    target = TargetResolver(project).resolve({"manufacturer": "Fixture", "model": "Video"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "owned-video.mp4",
            "publisher": "Fixture owner",
            "page_title": "Walkaround",
            "authority_class": "user_owned",
            "target_variant": {"manufacturer": "Fixture", "model": "Video"},
            "viewpoint": "walkaround video",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": True},
    )
    store.review_governance(
        source["id"],
        reviewed_by="Fixture owner",
        source_terms_review="user_owned",
        privacy_review="user_owned",
    )
    video_path = tmp_path / "walkaround.mp4"
    video_path.write_bytes(b"fixture-video")
    acquired = store.acquire_local(source["id"], video_path)

    def extract_fixture(
        _instance: CaptureService,
        path: Path,
        *,
        rights_state: str,
        interval_seconds: float = 1.0,
        maximum_frames: int = 300,
    ) -> dict:
        assert path == project.root / acquired["reference"]["relative_path"]
        assert rights_state == "USER_OWNED"
        assert interval_seconds == 1.0 and maximum_frames == 300
        return {
            "source_reference": acquired["reference"],
            "frame_references": [{"id": "frame-1"}],
            "intelligence": {"acceptance_reference_ids": ["frame-1"]},
        }

    monkeypatch.setattr(CaptureService, "extract_video", extract_fixture)
    campaign = CampaignStore(project).start(
        "external_public_benchmark",
        configuration={"target_id": target["id"]},
        resource_profile="compact",
    )

    result = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])

    assert result["workflow_state"] == "VIDEO_KEYFRAMES_COMPLETE"
    assert result["evidence"]["video"]["intelligence"]["acceptance_reference_ids"] == [
        "frame-1"
    ]
