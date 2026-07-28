from __future__ import annotations

import asyncio
import hmac
import json
import os
import threading
from pathlib import Path
from typing import Any

from mcp.server.fastmcp import FastMCP

from blender_vision.acceptance.regression import FixedCameraRegressionEvaluator
from blender_vision.acceptance.transactions import CandidateTransactionStore
from blender_vision.active_perception import (
    NextBestViewPlanner,
    PerceptionTarget,
    PlannerConfig,
    ProposedView,
    SurfaceCell,
    estimate_information_gain,
)
from blender_vision.app_build.benchmark import ApplicationBenchmarkRunner
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.artifacts.transfer import ArtifactTransfer
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.backends.registry import BackendRegistry
from blender_vision.benchmarks.beast import BeastBenchmarkAuditor
from blender_vision.benchmarks.calibration import bootstrap_calibration
from blender_vision.benchmarks.devices import bootstrap_device_benchmark
from blender_vision.benchmarks.external import bootstrap_external_benchmark
from blender_vision.benchmarks.reviews import BenchmarkReviewStore
from blender_vision.cameras.consensus import CameraConsensus
from blender_vision.cameras.landmark_matching import RenderLandmarkMatcher
from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.cameras.refinement import CameraRefiner
from blender_vision.cameras.solver import CameraSolver
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.capture.intelligence import VideoIntelligenceService
from blender_vision.capture.service import CaptureService
from blender_vision.cinematic.emit import bake_blender_camera, export_motion_table
from blender_vision.cinematic.path import (
    SolidGeometry,
    compose_camera_path,
    compose_flagship_datacentre_path,
)
from blender_vision.core.config import default_projects_root, doctor_report
from blender_vision.core.errors import BackendUnavailable, ValidationError
from blender_vision.core.models import EvidenceClass, FidelityLevel
from blender_vision.critics import CriticWorkspace, CritiqueEvidence, CritiqueSubject
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.delivery.compress import measure_and_select_compression
from blender_vision.delivery.lods import LodBudget
from blender_vision.delivery.lods import generate_lods as delivery_generate_lods
from blender_vision.delivery.manifest import build_delivery_manifest
from blender_vision.delivery.stream import build_streaming_plan
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.adoption import LegacyReferenceAdoptionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.discovery import SearchProviderStore
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.measurements import MeasurementGridStore, MeasurementStore
from blender_vision.evidence.pursuit import EvidencePursuitStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.features.detector import FeatureDetectionImporter
from blender_vision.features.store import FeatureStore
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.portfolio_executor import PortfolioExecutor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import SemanticTwinGraph
from blender_vision.geometry.synthetic_views import SyntheticViewStore
from blender_vision.grooming.fur import FurGroomer, GroomParameters
from blender_vision.intelligence.active_learning import ActiveLearningStore
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.lighting.joint import joint_solve
from blender_vision.lighting.solve import GeometryContext, LightingObservation, solve_lighting
from blender_vision.materials.inverse import SurfaceObservation, SurfaceRegion, infer_materials
from blender_vision.materials.store import MaterialStore
from blender_vision.materials.textures import generate_texture_set
from blender_vision.models.store import ModelStore
from blender_vision.ocular.calibration import calibrate_sensor as ocular_calibrate_sensor
from blender_vision.ocular.gaze import GazeController
from blender_vision.ocular.predict import list_surprises as ocular_list_surprises
from blender_vision.ocular.predict import predict_next as ocular_predict_next
from blender_vision.ocular.stream import (
    close_stream as ocular_close_stream,
)
from blender_vision.ocular.stream import (
    get_stream_state as ocular_get_stream_state,
)
from blender_vision.ocular.stream import (
    open_stream_or_attest,
)
from blender_vision.ocular.track import (
    Detection,
    KalmanCV2,
    TrackerState,
    TrackTargetKind,
    VisualTrack,
)
from blender_vision.ocular.track import (
    reidentify as ocular_reidentify,
)
from blender_vision.ocular.track import (
    track as ocular_track,
)
from blender_vision.ocular.world import (
    WorldState,
)
from blender_vision.ocular.world import (
    build_world_model as ocular_build_world_model,
)
from blender_vision.ocular.world import (
    compare_worlds as ocular_compare_worlds,
)
from blender_vision.ocular.world import (
    explain_belief as ocular_explain_belief,
)
from blender_vision.ocular.world import (
    list_uncertainties as ocular_list_uncertainties,
)
from blender_vision.ocular.world import (
    query_world as ocular_query_world,
)
from blender_vision.ocular.world import (
    update_world_model as ocular_update_world_model,
)
from blender_vision.optimization.engine import OptimizationEngine
from blender_vision.optimization.search import MultiviewSearchStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.orchestration.context import ContextPacketStore
from blender_vision.orchestration.locality import LocalityPlanner
from blender_vision.orchestration.resources import PROFILES, discover_resources
from blender_vision.orchestration.roles import RoleTaskStore
from blender_vision.orchestration.services import WarmServiceRegistry
from blender_vision.organic.topology import TopologyService
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.fitting import ComponentFitter
from blender_vision.parametric.store import ComponentStore
from blender_vision.perception import (
    CaptureBus,
    DesignIntelligenceService,
    ExperienceIRCompiler,
    FeatureCapsuleCompiler,
    FeatureCapsuleVerifier,
    FrontendComparisonService,
    FrontendRepairService,
    GraphicsRoundTripService,
    MediaReconstructionService,
    ObservationQueryService,
    PerceptionLearningService,
    PerceptionWorkspace,
    SourceIntelligenceService,
    default_adapter_registry,
    default_capture_bus,
)
from blender_vision.procedural.archetype import mesh_fingerprint
from blender_vision.procedural.emit import emit_archetype
from blender_vision.procedural.grammar import SceneProgram, datacenter_flagship_program
from blender_vision.procedural.library import default_library, list_archetypes
from blender_vision.procedural.scene import build_flagship_scene, compile_scene
from blender_vision.projects.store import ProjectStore, slugify
from blender_vision.reconstruction.base import (
    CameraView,
    ReconstructionInputs,
)
from blender_vision.reconstruction.base import (
    PointCloud as ReconstructionPointCloud,
)
from blender_vision.reconstruction.browser_runtime import BrowserRuntimeBackend
from blender_vision.reconstruction.colmap_sfm import ColmapSfMBackend
from blender_vision.reconstruction.compare import compare_all, compare_candidates
from blender_vision.reconstruction.depth_fusion import DepthFusionBackend
from blender_vision.reconstruction.parametric import ParametricBackend, fit_primitive
from blender_vision.reconstruction.point_representation import PointRepresentationBackend
from blender_vision.reconstruction.portfolio import build_portfolio
from blender_vision.reconstruction.retrieval import RetrievalBackend
from blender_vision.reconstruction.visual_hull import VisualHullBackend
from blender_vision.repairs.store import RepairStore
from blender_vision.review.service import ReviewService
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import DistributedScheduler
from blender_vision.spatial.capture_plan import plan_capture
from blender_vision.spatial.coverage import CoverageAtlas
from blender_vision.spatial.depth import DepthKind, DepthMap
from blender_vision.spatial.pointcloud import PointCloud as SpatialPointCloud
from blender_vision.v2.authority import (
    AuthorityClass,
    AuthorityPromotionError,
    Units,
)
from blender_vision.v2.records import (
    DeliveryAsset,
    MaterialHypothesis,
    NarrativeBeat,
    ReconstructionCandidate,
    load_record,
)
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.vision.store import GeometryEvidenceStore
from blender_vision.visual.oracle import VisualOracleStore
from blender_vision.visual_geometry.audit import ManufacturedFormAuditor
from blender_vision.visual_geometry.baseline import VisualBaselineStore
from blender_vision.visual_geometry.bindings import SemanticBindingStore
from blender_vision.visual_geometry.diagnosis import VisualDefectDiagnosisStore
from blender_vision.visual_geometry.packets import (
    ComponentTaskPacketStore,
    VisualFrequencyScoreStore,
)
from blender_vision.visual_geometry.store import VisualGeometryStore
from blender_vision.workflows.autonomous import (
    generate_original_asset,
    reconstruct_from_public_evidence,
    reconstruct_from_user_capture,
)
from blender_vision.workflows.executor import AutonomousWorkflowExecutor
from blender_vision.workflows.progress import WorkflowProgressReporter

_V2_RECONSTRUCTION_BACKENDS: dict[str, type] = {
    "colmap_sfm": ColmapSfMBackend,
    "visual_hull": VisualHullBackend,
    "depth_fusion": DepthFusionBackend,
    "parametric": ParametricBackend,
    "retrieval": RetrievalBackend,
    "browser_runtime": BrowserRuntimeBackend,
    "point_representation": PointRepresentationBackend,
}


def create_server(projects_root: Path | None = None) -> FastMCP:
    root = (projects_root or default_projects_root()).expanduser().resolve()
    root.mkdir(parents=True, exist_ok=True)
    mcp = FastMCP(
        "blender-vision",
        instructions=(
            "Evidence-bound reference-to-3D reconstruction. Never promote approximate camera or "
            "inferred geometry evidence to metric/manufacturing authority. Prefer workflow tools."
        ),
    )

    def open_project(path: str) -> ProjectStore:
        return ProjectStore.open(Path(path))

    def perception_bus(project: ProjectStore) -> CaptureBus:
        return default_capture_bus(project)

    def by_id(project_id: str) -> ProjectStore:
        for metadata in root.glob("*/project.json"):
            project = ProjectStore.open(metadata.parent)
            if project.project()["id"] == project_id:
                return project
        raise FileNotFoundError(f"unknown project id: {project_id}")

    def enqueue_background(
        project: ProjectStore, operation: str, config: dict[str, Any]
    ) -> dict[str, Any]:
        coordinator = Coordinator(project)
        job_id = coordinator.enqueue(operation, config)
        thread = threading.Thread(target=coordinator.execute, args=(job_id,), daemon=True)
        thread.start()
        return {"job_id": job_id, "status": "queued", "project": str(project.root)}

    def enqueue_remote(
        project: ProjectStore, operation: str, config: dict[str, Any]
    ) -> dict[str, Any]:
        job_id = Coordinator(project).enqueue(operation, config)
        return {
            "job_id": job_id,
            "status": "queued",
            "execution": "distributed",
            "project": str(project.root),
        }

    def require_worker_enrollment(token: str) -> None:
        expected = os.environ.get("BVMCP_WORKER_ENROLLMENT_TOKEN")
        if not expected:
            raise PermissionError(
                "remote worker enrollment is disabled; set BVMCP_WORKER_ENROLLMENT_TOKEN"
            )
        if not hmac.compare_digest(expected, token):
            raise PermissionError("invalid worker enrollment token")

    @mcp.tool(name="system.doctor")
    def system_doctor() -> dict[str, Any]:
        """Discover pinned local Blender, media, and geometry capabilities."""
        return doctor_report()

    @mcp.tool(name="system.capabilities")
    def system_capabilities() -> dict[str, Any]:
        """List backend states, licenses, hardware, and outputs without downloading weights."""
        return {"backends": BackendRegistry().as_dict()}

    @mcp.tool(name="system.resource_profiles")
    def system_resource_profiles() -> dict[str, Any]:
        """Discover hardware and publish Compact through Distributed Beast profiles."""
        return {"discovery": discover_resources(), "profiles": PROFILES}

    @mcp.tool(name="system.warm_service_update")
    def system_warm_service_update(
        project_path: str,
        name: str,
        status: str,
        memory_gb: float,
        expected_reuse: float,
        backend: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Record optional warm-service state for resource-aware reuse and eviction."""
        return WarmServiceRegistry(open_project(project_path)).update(
            name,
            status=status,
            memory_gb=memory_gb,
            expected_reuse=expected_reuse,
            backend=backend,
        )

    @mcp.tool(name="system.warm_service_evict")
    def system_warm_service_evict(project_path: str, required_free_gb: float) -> dict[str, Any]:
        """Evict lowest-reuse warm services until the requested memory is available."""
        return WarmServiceRegistry(open_project(project_path)).evict_for_pressure(
            required_free_gb=required_free_gb
        )

    @mcp.tool(name="system.warm_service_list")
    def system_warm_service_list(project_path: str) -> dict[str, Any]:
        """List persistent optional worker state without starting any process."""
        return {"services": WarmServiceRegistry(open_project(project_path)).list()}

    @mcp.tool(name="campaign.start")
    def campaign_start(
        project_path: str,
        kind: str,
        configuration: dict[str, Any],
        resource_profile: str = "auto",
        budget: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Start a persistent evidence-governed reconstruction controller."""
        return CampaignStore(open_project(project_path)).start(
            kind,
            configuration=configuration,
            resource_profile=resource_profile,
            budget=budget,
        )

    @mcp.tool(name="campaign.advance")
    def campaign_advance(
        project_path: str, campaign_id: str, payload: dict[str, Any]
    ) -> dict[str, Any]:
        """Advance exactly one validated OBSERVE-to-ROLLBACK controller state."""
        return CampaignStore(open_project(project_path)).advance(campaign_id, payload)

    @mcp.tool(name="campaign.pause")
    def campaign_pause(project_path: str, campaign_id: str, reason: str) -> dict[str, Any]:
        """Pause a campaign while preserving its exact state and evidence."""
        return CampaignStore(open_project(project_path)).pause(campaign_id, reason=reason)

    @mcp.tool(name="campaign.progress")
    def campaign_progress(
        project_path: str,
        campaign_id: str,
        message: str,
        details: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Persist host-visible progress without advancing the controller state."""
        return CampaignStore(open_project(project_path)).progress(
            campaign_id, message=message, details=details
        )

    @mcp.tool(name="campaign.resume")
    def campaign_resume(project_path: str, campaign_id: str) -> dict[str, Any]:
        """Resume a paused campaign at its preserved controller state."""
        return CampaignStore(open_project(project_path)).resume(campaign_id)

    @mcp.tool(name="campaign.status")
    def campaign_status(project_path: str, campaign_id: str) -> dict[str, Any]:
        """Return controller state, iteration, budget, events, and stopping evidence."""
        return CampaignStore(open_project(project_path)).get(campaign_id)

    @mcp.tool(name="campaign.stop")
    def campaign_stop(project_path: str, campaign_id: str, reason: str) -> dict[str, Any]:
        """Stop a campaign explicitly and preserve its terminal reason."""
        return CampaignStore(open_project(project_path)).stop(campaign_id, reason=reason)

    @mcp.tool(name="role.assign")
    def role_assign(
        project_path: str,
        campaign_id: str,
        objective: str,
        confidence: float,
        estimated_cost: float,
        inputs: dict[str, Any],
        role: str | None = None,
    ) -> dict[str, Any]:
        """Assign one persistent advisory role task by uncertainty, cost, and objective."""
        return RoleTaskStore(open_project(project_path)).assign(
            campaign_id,
            objective,
            confidence=confidence,
            estimated_cost=estimated_cost,
            inputs=inputs,
            role=role,
        )

    @mcp.tool(name="role.waiting")
    def role_waiting(project_path: str, task_id: str, reason: str) -> dict[str, Any]:
        """Mark an advisory role task as waiting for explicit external input."""
        return RoleTaskStore(open_project(project_path)).set_waiting(task_id, reason=reason)

    @mcp.tool(name="role.complete")
    def role_complete(
        project_path: str,
        task_id: str,
        output: dict[str, Any],
        artifact_digests: list[str],
        completed_by: str,
    ) -> dict[str, Any]:
        """Complete a role handoff as advisory evidence without accepting or promoting scenes."""
        return RoleTaskStore(open_project(project_path)).complete(
            task_id,
            output=output,
            artifact_digests=artifact_digests,
            completed_by=completed_by,
        )

    @mcp.tool(name="role.list")
    def role_list(project_path: str, campaign_id: str | None = None) -> dict[str, Any]:
        """List persistent role-task handoffs in priority order."""
        return {"tasks": RoleTaskStore(open_project(project_path)).list(campaign_id)}

    @mcp.tool(name="context.create_packet")
    def context_create_packet(
        project_path: str,
        target_component: str,
        allowed_operations: list[str],
        desired_gate: str,
        campaign_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a compact component-local decision packet instead of sending the project."""
        return ContextPacketStore(open_project(project_path)).create(
            target_component=target_component,
            allowed_operations=allowed_operations,
            desired_gate=desired_gate,
            campaign_id=campaign_id,
        )

    @mcp.tool(name="workflow.reconstruct_from_public_evidence")
    def workflow_reconstruct_from_public_evidence(
        target: str,
        requested_tier: str = "L3",
        configuration: str = "factory standard",
        evidence_policy: str = "public_internal_use",
        existing_model: str | None = None,
        project_name: str | None = None,
        sources: list[dict[str, Any]] | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Create a project and launch the autonomous public-evidence twin workflow."""
        name = project_name or target
        project = ProjectStore.create(
            root / slugify(name),
            name,
            target_fidelity=FidelityLevel(requested_tier),
        )
        return reconstruct_from_public_evidence(
            project,
            target=target,
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=Path(existing_model) if existing_model else None,
            sources=sources,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="workflow.reconstruct_from_user_capture")
    def workflow_reconstruct_from_user_capture(
        target: str,
        reference_paths: list[str],
        requested_tier: str = "L3",
        configuration: str = "as captured",
        category: str | None = None,
        project_name: str | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Create a project from owner-supplied evidence and launch reference reconstruction."""
        name = project_name or f"{target} user capture"
        project = ProjectStore.create(
            root / slugify(name), name, target_fidelity=FidelityLevel(requested_tier)
        )
        return reconstruct_from_user_capture(
            project,
            target=target,
            reference_paths=[Path(path) for path in reference_paths],
            requested_tier=requested_tier,
            configuration=configuration,
            category=category,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="workflow.continue_autonomous")
    def workflow_continue_autonomous(
        project_path: str,
        campaign_id: str,
        camera_backend: str = "auto",
        resume_paused: bool = False,
    ) -> dict[str, Any]:
        """Execute the next safe evidence-derived action or stop at an authority boundary."""
        project = open_project(project_path)
        result = AutonomousWorkflowExecutor(project).continue_once(
            campaign_id,
            camera_backend=camera_backend,
            resume_paused=resume_paused,
        )
        return {**result, "progress": WorkflowProgressReporter(project).report(campaign_id)}

    @mcp.tool(name="workflow.progress")
    def workflow_progress(project_path: str, campaign_id: str | None = None) -> dict[str, Any]:
        """Return compact recomputed progress without changing evidence or acceptance state."""
        return WorkflowProgressReporter(open_project(project_path)).report(campaign_id)

    @mcp.tool(name="workflow.generate_original_asset")
    def workflow_generate_original_asset(
        description: str,
        category: str = "organic_creatures",
        project_name: str | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Launch an explicitly L0 generated-design portfolio without measured-target claims."""
        name = project_name or description
        project = ProjectStore.create(root / slugify(name), name, target_fidelity=FidelityLevel.L0)
        return generate_original_asset(
            project,
            description=description,
            category=category,
            resource_profile=resource_profile,
        )

    def reconstruct_category(
        *,
        target: str,
        category: str,
        requested_tier: str,
        configuration: str,
        evidence_policy: str,
        existing_model: str | None,
        project_name: str | None,
        sources: list[dict[str, Any]] | None,
        resource_profile: str,
    ) -> dict[str, Any]:
        name = project_name or target
        project = ProjectStore.create(
            root / slugify(name), name, target_fidelity=FidelityLevel(requested_tier)
        )
        return reconstruct_from_public_evidence(
            project,
            target=target,
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=Path(existing_model) if existing_model else None,
            sources=sources,
            resource_profile=resource_profile,
            category_override=category,
        )

    @mcp.tool(name="workflow.reconstruct_vehicle")
    def workflow_reconstruct_vehicle(
        target: str,
        requested_tier: str = "L3",
        configuration: str = "factory standard",
        evidence_policy: str = "public_internal_use",
        existing_model: str | None = None,
        project_name: str | None = None,
        sources: list[dict[str, Any]] | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Launch the vehicle ontology, constraints, search plan, and candidate portfolio."""
        return reconstruct_category(
            target=target,
            category="vehicles",
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=existing_model,
            project_name=project_name,
            sources=sources,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="workflow.reconstruct_hardware")
    def workflow_reconstruct_hardware(
        target: str,
        requested_tier: str = "L3",
        configuration: str = "factory standard",
        evidence_policy: str = "public_internal_use",
        existing_model: str | None = None,
        project_name: str | None = None,
        sources: list[dict[str, Any]] | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Launch the computer-hardware ontology and reconstruction portfolio."""
        return reconstruct_category(
            target=target,
            category="computer_hardware",
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=existing_model,
            project_name=project_name,
            sources=sources,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="workflow.reconstruct_packaging")
    def workflow_reconstruct_packaging(
        target: str,
        requested_tier: str = "L3",
        configuration: str = "as evidenced",
        evidence_policy: str = "public_internal_use",
        existing_model: str | None = None,
        project_name: str | None = None,
        sources: list[dict[str, Any]] | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Launch packaging folds, layers, print regions, and reconstruction hypotheses."""
        return reconstruct_category(
            target=target,
            category="packaging",
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=existing_model,
            project_name=project_name,
            sources=sources,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="workflow.reconstruct_organic_subject")
    def workflow_reconstruct_organic_subject(
        target: str,
        requested_tier: str = "L2",
        configuration: str = "as evidenced",
        evidence_policy: str = "public_internal_use",
        existing_model: str | None = None,
        project_name: str | None = None,
        sources: list[dict[str, Any]] | None = None,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Launch anatomical landmarks and distinguish real reconstruction from generation."""
        return reconstruct_category(
            target=target,
            category="organic_creatures",
            requested_tier=requested_tier,
            configuration=configuration,
            evidence_policy=evidence_policy,
            existing_model=existing_model,
            project_name=project_name,
            sources=sources,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="target.resolve")
    def target_resolve(
        project_path: str,
        target: str,
        requested_tier: str = "L3",
        request_class: str = "AUTONOMOUS_PUBLIC_EVIDENCE",
        configuration: str = "factory standard unless evidence specifies otherwise",
        market: str = "unspecified",
        structured_target: dict[str, Any] | None = None,
        alternatives: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Resolve and persist one canonical variant without merging incompatible evidence."""
        return TargetResolver(open_project(project_path)).resolve(
            structured_target or target,
            request_class=request_class,
            requested_tier=requested_tier,
            configuration=configuration,
            market=market,
            alternatives=alternatives,
        )

    @mcp.tool(name="vision.resolve_target")
    def vision_resolve_target(
        project_path: str,
        target: str,
        requested_tier: str = "L3",
        request_class: str = "AUTONOMOUS_PUBLIC_EVIDENCE",
        configuration: str = "factory standard unless evidence specifies otherwise",
        market: str = "unspecified",
        structured_target: dict[str, Any] | None = None,
        alternatives: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Resolve the canonical governed target through the stable vision surface."""
        return TargetResolver(open_project(project_path)).resolve(
            structured_target or target,
            request_class=request_class,
            requested_tier=requested_tier,
            configuration=configuration,
            market=market,
            alternatives=alternatives,
        )

    @mcp.tool(name="category.list")
    def category_list() -> dict[str, Any]:
        """List built-in domain ontologies, constraints, and parametric constructs."""
        return {"packs": CategoryPackRegistry().list()}

    @mcp.tool(name="category.select")
    def category_select(project_path: str, target_id: str | None = None) -> dict[str, Any]:
        """Select the strongest category pack for the canonical target."""
        target = TargetResolver(open_project(project_path)).get(target_id)
        return CategoryPackRegistry().select(target["target"])

    @mcp.tool(name="semantic_model.bootstrap")
    def semantic_model_bootstrap(
        project_path: str,
        category: str | None = None,
        target_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a semantic digital-twin graph whose operations use stable semantic IDs."""
        return SemanticTwinGraph(open_project(project_path)).bootstrap(
            category=category, target_id=target_id
        )

    @mcp.tool(name="semantic_model.bind")
    def semantic_model_bind(
        project_path: str,
        semantic_id: str,
        scene_id: str,
        object_names: list[str],
        confidence: float,
        reference_ids: list[str] | None = None,
        component_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Bind Blender objects and references to one stable semantic component ID."""
        return SemanticTwinGraph(open_project(project_path)).bind(
            semantic_id,
            scene_id=scene_id,
            object_names=object_names,
            reference_ids=reference_ids,
            component_ids=component_ids,
            confidence=confidence,
        )

    @mcp.tool(name="semantic_model.extend")
    def semantic_model_extend(
        project_path: str, root_id: str, component_types: list[str]
    ) -> dict[str, Any]:
        """Idempotently add target-specific component types to an existing semantic twin."""
        return SemanticTwinGraph(open_project(project_path)).ensure_component_nodes(
            root_id, component_types
        )

    @mcp.tool(name="semantic_model.review")
    def semantic_model_review(
        project_path: str,
        semantic_id: str,
        acceptance_state: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Accept, reject, or declare a semantic component inapplicable with named review."""
        return SemanticTwinGraph(open_project(project_path)).review(
            semantic_id,
            acceptance_state=acceptance_state,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="portfolio.generate")
    def portfolio_generate(
        project_path: str,
        category: str = "general_product",
        lanes: list[str] | None = None,
        resource_profile: str = "standard",
        backend_configuration: dict[str, dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Create bounded classical, learned, generative, and semantic hypotheses."""
        return ReconstructionPortfolioStore(open_project(project_path)).generate(
            category=category,
            lanes=lanes,
            resource_profile=resource_profile,
            backend_configuration=backend_configuration,
        )

    @mcp.tool(name="portfolio.record_result")
    def portfolio_record_result(
        project_path: str,
        candidate_id: str,
        metrics: dict[str, float],
        artifacts: list[str] | None = None,
        scene_id: str | None = None,
        geometry_run_id: str | None = None,
    ) -> dict[str, Any]:
        """Attach evidence outputs and scores to one isolated portfolio candidate."""
        return ReconstructionPortfolioStore(open_project(project_path)).record_result(
            candidate_id,
            metrics=metrics,
            artifacts=artifacts,
            scene_id=scene_id,
            geometry_run_id=geometry_run_id,
        )

    @mcp.tool(name="portfolio.execute_parametric_seed")
    def portfolio_execute_parametric_seed(
        project_path: str,
        portfolio_id: str,
        asynchronous: bool = True,
    ) -> dict[str, Any]:
        """Build a fresh editable dimension-bound seed with no private starting model."""
        project = open_project(project_path)
        if asynchronous:
            return enqueue_background(
                project,
                "portfolio.execute_parametric_seed",
                {"portfolio_id": portfolio_id},
            )
        return Coordinator(project).run(
            "portfolio.execute_parametric_seed", {"portfolio_id": portfolio_id}
        )

    @mcp.tool(name="portfolio.rank")
    def portfolio_rank(project_path: str, portfolio_id: str) -> dict[str, Any]:
        """Rank evaluated hypotheses while preferring an editable semantic target."""
        return ReconstructionPortfolioStore(open_project(project_path)).rank(portfolio_id)

    @mcp.tool(name="portfolio.execute_initial")
    def portfolio_execute_initial(project_path: str, portfolio_id: str) -> dict[str, Any]:
        """Execute cheap local portfolio lanes and record unavailable workers truthfully."""
        return PortfolioExecutor(open_project(project_path)).execute_initial(portfolio_id)

    @mcp.tool(name="portfolio.fusion_plan")
    def portfolio_fusion_plan(project_path: str, portfolio_id: str) -> dict[str, Any]:
        """Plan evidence-safe candidate fusion without treating generated topology as measured."""
        return ReconstructionPortfolioStore(open_project(project_path)).fusion_plan(portfolio_id)

    @mcp.tool(name="generative3d.generate")
    def generative3d_generate(
        project_path: str,
        operation: str,
        backend: str,
        inputs: dict[str, Any],
        checkpoint: str,
        license_record: dict[str, Any],
        backend_configuration: dict[str, Any] | None = None,
        execution: str = "distributed",
    ) -> dict[str, Any]:
        """Create and optionally queue a licensed artifact-bound generative proposal."""
        project = open_project(project_path)
        store = GenerativeProposalStore(project)
        proposal = store.request(
            operation,
            backend=backend,
            inputs=inputs,
            checkpoint=checkpoint,
            license_record=license_record,
            backend_configuration=backend_configuration,
        )
        if execution == "record":
            return {"request": proposal, "job": None}
        if execution != "distributed":
            raise ValueError("generative execution must be record or distributed")
        current = store.get_request(proposal["id"])
        if current["status"] == "COMPLETED":
            return {
                "request": current,
                "result": store.get_result(proposal["id"]),
                "job": None,
            }
        if current["status"] == "QUEUED" and current.get("job_id"):
            return {
                "request": current,
                "job": project.job(current["job_id"]),
            }
        if current["status"] == "FAILED":
            raise ValueError(
                "generative request exhausted its worker attempts; revise its governed "
                "configuration before creating a new request"
            )
        job = enqueue_remote(
            project,
            "generative3d.execute",
            {
                "request_id": proposal["id"],
                "operation": proposal["operation"],
                "backend": proposal["backend"],
                "checkpoint": proposal["checkpoint"],
                "worker_requirements": {
                    "required_models": [proposal["backend"]],
                    "preferred_models": [proposal["backend"]],
                },
            },
        )
        return {
            "request": store.bind_job(proposal["id"], job["job_id"]),
            "job": job,
        }

    @mcp.tool(name="generative3d.import_result")
    def generative3d_import_result(
        project_path: str,
        request_id: str,
        mesh_digests: list[str],
        texture_digests: list[str],
        image_digests: list[str],
        pbr_channels: dict[str, str],
        backend_identity: str,
        checkpoint: str,
        input_reference_ids: list[str],
        generation_seed: int,
        confidence: float,
        known_limitations: list[str],
    ) -> dict[str, Any]:
        """Import hash-bound generative outputs as non-acceptance-eligible hypotheses."""
        return GenerativeProposalStore(open_project(project_path)).import_result(
            request_id,
            mesh_digests=mesh_digests,
            texture_digests=texture_digests,
            image_digests=image_digests,
            pbr_channels=pbr_channels,
            backend_identity=backend_identity,
            checkpoint=checkpoint,
            input_reference_ids=input_reference_ids,
            generation_seed=generation_seed,
            confidence=confidence,
            known_limitations=known_limitations,
        )

    @mcp.tool(name="evidence.search")
    def evidence_search(
        project_path: str,
        target_id: str | None = None,
        category: str = "general_product",
    ) -> dict[str, Any]:
        """Generate a category-aware, rights-governed acquisition query plan."""
        return EvidenceAcquisitionStore(open_project(project_path)).plan_search(
            target_id, category=category
        )

    @mcp.tool(name="evidence.acquire")
    def evidence_acquire(
        project_path: str,
        target_id: str,
        source_record: dict[str, Any],
        rights_record: dict[str, Any],
        local_path: str | None = None,
        reviewed_by: str | None = None,
    ) -> dict[str, Any]:
        """Register provenance and rights, then ingest a permitted local working reference."""
        store = EvidenceAcquisitionStore(open_project(project_path))
        source = store.register_source(
            target_id, source_record, rights=rights_record, reviewed_by=reviewed_by
        )
        return store.acquire_local(source["id"], Path(local_path)) if local_path else source

    @mcp.tool(name="evidence.audit")
    def evidence_audit(project_path: str, target_id: str | None = None) -> dict[str, Any]:
        """Audit source provenance and rights-ledger completeness."""
        return EvidenceAcquisitionStore(open_project(project_path)).audit(target_id)

    @mcp.tool(name="evidence.propose_legacy_reference_adoption")
    def evidence_propose_legacy_reference_adoption(
        project_path: str,
        target_id: str,
        reference_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Propose artifact-bound migration of legacy references without inferring rights."""
        return LegacyReferenceAdoptionStore(open_project(project_path)).propose_orphans(
            target_id, reference_ids
        )

    @mcp.tool(name="evidence.review_legacy_reference_adoption")
    def evidence_review_legacy_reference_adoption(
        project_path: str,
        proposal_id: str,
        decision: str,
        reviewer: str,
        reason: str,
        source: dict[str, Any] | None = None,
        rights: dict[str, Any] | None = None,
        source_terms_review: str | None = None,
        privacy_review: str | None = None,
    ) -> dict[str, Any]:
        """Adopt or exclude one legacy reference through a named immutable review."""
        return LegacyReferenceAdoptionStore(open_project(project_path)).review(
            proposal_id,
            decision=decision,
            reviewer=reviewer,
            reason=reason,
            source=source,
            rights=rights,
            source_terms_review=source_terms_review,
            privacy_review=privacy_review,
        )

    @mcp.tool(name="evidence.list_legacy_reference_adoptions")
    def evidence_list_legacy_reference_adoptions(
        project_path: str, target_id: str | None = None
    ) -> dict[str, Any]:
        """List legacy-reference adoption proposals and their review state."""
        proposals = LegacyReferenceAdoptionStore(open_project(project_path)).list(target_id)
        return {"proposals": proposals, "count": len(proposals)}

    @mcp.tool(name="evidence.register_search_provider")
    def evidence_register_search_provider(
        project_path: str,
        name: str,
        configuration: dict[str, Any],
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Register a bounded search API after named terms, privacy, and secret review."""
        return SearchProviderStore(open_project(project_path)).register(
            name=name,
            configuration=configuration,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="evidence.discover")
    def evidence_discover(
        project_path: str,
        provider_id: str,
        target_id: str | None = None,
        category: str = "general_product",
        focus_terms: list[str] | None = None,
        maximum_queries: int | None = None,
        maximum_results_per_query: int | None = None,
        timeout_seconds: float = 20.0,
        asynchronous: bool = True,
    ) -> dict[str, Any]:
        """Execute reviewed search queries and register results with unresolved source rights."""
        project = open_project(project_path)
        discovery = {
            "provider_id": provider_id,
            "target_id": target_id,
            "category": category,
            "focus_terms": focus_terms,
            "maximum_queries": maximum_queries,
            "maximum_results_per_query": maximum_results_per_query,
            "timeout_seconds": timeout_seconds,
        }
        if asynchronous:
            return enqueue_background(project, "evidence.discover", discovery)
        return SearchProviderStore(project).discover(
            provider_id,
            target_id=target_id,
            category=category,
            focus_terms=focus_terms,
            maximum_queries=maximum_queries,
            maximum_results_per_query=maximum_results_per_query,
            timeout_seconds=timeout_seconds,
        )

    @mcp.tool(name="evidence.acquire_url")
    def evidence_acquire_url(
        project_path: str, source_id: str, timeout_seconds: float = 30.0
    ) -> dict[str, Any]:
        """Acquire one pre-reviewed URL with robots, network, size, and provenance guardrails."""
        return EvidenceAcquisitionStore(open_project(project_path)).acquire_url(
            source_id, timeout_seconds=timeout_seconds
        )

    @mcp.tool(name="evidence.review_governance")
    def evidence_review_governance(
        project_path: str,
        source_id: str,
        reviewed_by: str,
        source_terms_review: str,
        privacy_review: str,
        rights: dict[str, Any] | None = None,
        reviewer_type: str = "human",
        review_basis: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Complete a named source-terms, privacy, and rights review."""
        return EvidenceAcquisitionStore(open_project(project_path)).review_governance(
            source_id,
            reviewed_by=reviewed_by,
            source_terms_review=source_terms_review,
            privacy_review=privacy_review,
            rights=rights,
            reviewer_type=reviewer_type,
            review_basis=review_basis,
        )

    @mcp.tool(name="evidence.deduplicate")
    def evidence_deduplicate(project_path: str, target_id: str | None = None) -> dict[str, Any]:
        """Audit exact, perceptual, and mirrored duplicates without deleting evidence."""
        return EvidenceAcquisitionStore(open_project(project_path)).deduplicate(target_id)

    @mcp.tool(name="evidence.resolve_conflicts")
    def evidence_resolve_conflicts(
        project_path: str, target_id: str | None = None
    ) -> dict[str, Any]:
        """Detect source variants that conflict with the canonical project identity."""
        return EvidenceAcquisitionStore(open_project(project_path)).resolve_conflicts(target_id)

    @mcp.tool(name="evidence.review_conflict")
    def evidence_review_conflict(
        project_path: str,
        source_id: str,
        category: str,
        decision: str,
        reviewer: str,
        reason: str,
        configuration_model: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Exclude, branch, or explicitly resolve one classified evidence conflict."""
        return EvidenceConflictStore(open_project(project_path)).review(
            source_id,
            category,
            decision=decision,
            reviewer=reviewer,
            reason=reason,
            configuration_model=configuration_model,
        )

    @mcp.tool(name="coverage.analyze")
    def coverage_analyze(project_path: str, target_id: str | None = None) -> dict[str, Any]:
        """Analyze acquired directional evidence and identify missing surfaces."""
        project = open_project(project_path)
        return {
            "directional": EvidenceAcquisitionStore(project).analyze_coverage(target_id),
            "surface_atlas": SurfaceCoverageAtlas(project).analyze(target_id),
        }

    @mcp.tool(name="coverage.observe_surface")
    def coverage_observe_surface(
        project_path: str,
        cell_id: str,
        observation_id: str,
        incidence_angle_degrees: float,
        resolution_pixels: int,
        occlusion_fraction: float,
        reflection_risk: str,
        evidence_class: str,
        uncertainty: dict[str, Any],
    ) -> dict[str, Any]:
        """Bind one governed observation to a canonical surface-atlas cell."""
        return SurfaceCoverageAtlas(open_project(project_path)).observe(
            cell_id,
            observation_id=observation_id,
            incidence_angle_degrees=incidence_angle_degrees,
            resolution_pixels=resolution_pixels,
            occlusion_fraction=occlusion_fraction,
            reflection_risk=reflection_risk,
            evidence_class=evidence_class,
            uncertainty=uncertainty,
        )

    @mcp.tool(name="coverage.observe_governed_source")
    def coverage_observe_governed_source(
        project_path: str,
        source_id: str,
        observations: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """Credit inspected surface regions to one acquired, named-reviewed source."""
        return SurfaceCoverageAtlas(open_project(project_path)).observe_governed_source(
            source_id, observations=observations
        )

    @mcp.tool(name="coverage.acquire_missing")
    def coverage_acquire_missing(
        project_path: str,
        target_id: str | None = None,
        category: str = "general_product",
        provider_id: str | None = None,
        required_terms: list[str] | None = None,
        maximum_queries: int = 5,
        maximum_results_per_query: int = 10,
        timeout_seconds: float = 20.0,
    ) -> dict[str, Any]:
        """Execute a bounded governed search for gaps, or issue precise capture requests."""
        project = open_project(project_path)
        store = EvidenceAcquisitionStore(project)
        pursuit = EvidencePursuitStore(project).pursue(
            target_id,
            category=category,
            provider_id=provider_id,
            required_terms=required_terms,
            maximum_queries=maximum_queries,
            maximum_results_per_query=maximum_results_per_query,
            timeout_seconds=timeout_seconds,
        )
        return {
            "coverage": store.analyze_coverage(target_id),
            "surface_atlas": SurfaceCoverageAtlas(store.project).analyze(target_id),
            "search_plan": store.plan_search(target_id, category=category),
            "pursuit": pursuit,
        }

    @mcp.tool(name="camera.freeze")
    def camera_freeze(project_path: str, solution_id: str) -> dict[str, Any]:
        """Verify that every camera in a stored solution is a complete immutable snapshot."""
        project = open_project(project_path)
        with project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?", (solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {solution_id}")
        document = __import__("json").loads(row[0])
        invalid = []
        for camera in document["cameras"]:
            try:
                validate_complete_camera_state(camera)
            except (KeyError, TypeError, ValueError) as error:
                invalid.append(
                    {
                        "reference_id": camera.get("reference_id"),
                        "error": str(error),
                    }
                )
        if invalid:
            raise ValueError(f"camera solution contains invalid immutable records: {invalid}")
        return {
            "solution_id": solution_id,
            "frozen": True,
            "camera_hashes": [camera["immutable_sha256"] for camera in document["cameras"]],
            "framing_policy": "exact matrices; scene-bound auto-fit prohibited",
        }

    @mcp.tool(name="camera.derive_undistorted")
    def camera_derive_undistorted(project_path: str, solution_id: str) -> dict[str, Any]:
        """Derive source-linked pinhole frames while preserving pose and scale authority."""
        return CameraSolver(open_project(project_path)).derive_undistorted_solution(solution_id)

    @mcp.tool(name="candidate.evaluate_transaction")
    def candidate_evaluate_transaction(
        project_path: str,
        scene_id: str,
        gates: list[dict[str, Any]],
        baseline_scene_id: str | None = None,
        metrics: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Evaluate every mandatory gate atomically and auto-reject any regression."""
        return CandidateTransactionStore(open_project(project_path)).evaluate(
            scene_id,
            gates=gates,
            baseline_scene_id=baseline_scene_id,
            metrics=metrics,
        )

    @mcp.tool(name="candidate.evaluate_fixed_camera_regression")
    def candidate_evaluate_fixed_camera_regression(
        project_path: str,
        baseline_scene_id: str,
        candidate_scene_id: str,
        minimum_views: int = 2,
        regression_tolerance: float = 0.0,
    ) -> dict[str, Any]:
        """Verify paired fixed-view artifacts and reject an aggregate silhouette regression."""
        return FixedCameraRegressionEvaluator(open_project(project_path)).evaluate(
            baseline_scene_id=baseline_scene_id,
            candidate_scene_id=candidate_scene_id,
            minimum_views=minimum_views,
            regression_tolerance=regression_tolerance,
        )

    @mcp.tool(name="benchmark.review_dgx_foam_lod")
    def benchmark_review_dgx_foam_lod(
        project_path: str,
        strategy: dict[str, Any],
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Record a named, receipt-backed DGX foam LOD policy approval."""
        return BenchmarkReviewStore(open_project(project_path)).approve_dgx_foam_lod(
            strategy=strategy,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="candidate.rank")
    def candidate_rank(project_path: str) -> dict[str, Any]:
        """Rank stored candidate transactions with passed non-regressing results first."""
        evaluations = CandidateTransactionStore(open_project(project_path)).list()
        evaluations.sort(
            key=lambda item: (
                item["status"] == "PASSED",
                -len(item["regressions"]),
                item["created_at"],
            ),
            reverse=True,
        )
        return {"evaluations": evaluations}

    @mcp.tool(name="candidate.reject")
    def candidate_reject(
        project_path: str, scene_id: str, reviewer: str, reason: str
    ) -> dict[str, Any]:
        """Reject a recoverable candidate with a named transition receipt."""
        return SceneStore(open_project(project_path)).transition(
            scene_id, "REJECTED", reviewer=reviewer, reason=reason
        )

    @mcp.tool(name="candidate.accept")
    def candidate_accept(
        project_path: str,
        scene_id: str,
        evaluation_id: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Accept only a candidate backed by its own passed transaction receipt."""
        return SceneStore(open_project(project_path)).transition(
            scene_id,
            "ACCEPTED",
            reviewer=reviewer,
            reason=reason,
            evaluation_id=evaluation_id,
        )

    @mcp.tool(name="candidate.promote")
    def candidate_promote(
        project_path: str,
        scene_id: str,
        evaluation_id: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Promote an accepted scene using its passed evaluation and supersede the baseline."""
        return SceneStore(open_project(project_path)).transition(
            scene_id,
            "PROMOTED",
            reviewer=reviewer,
            reason=reason,
            evaluation_id=evaluation_id,
        )

    @mcp.tool(name="model.approve_source")
    def model_approve_source(
        project_path: str,
        name: str,
        source_url: str,
        expected_sha256: str,
        license_record: dict[str, Any],
        approved_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Approve a model URL, license, and expected digest without downloading it."""
        return ModelStore(open_project(project_path)).approve_source(
            name,
            source_url,
            expected_sha256,
            license_record=license_record,
            approved_by=approved_by,
            reason=reason,
        )

    @mcp.tool(name="model.import_checkpoint")
    def model_import_checkpoint(
        project_path: str,
        approval_id: str,
        checkpoint_path: str,
        revision: str,
    ) -> dict[str, Any]:
        """Import a manually acquired checkpoint only when its approved digest matches."""
        return ModelStore(open_project(project_path)).import_checkpoint(
            approval_id, Path(checkpoint_path), revision=revision
        )

    @mcp.tool(name="model.list")
    def model_list(project_path: str) -> dict[str, Any]:
        """List model approvals/installations and the no-silent-download policy."""
        return ModelStore(open_project(project_path)).list()

    @mcp.tool(name="project.create")
    def project_create(name: str, target_fidelity: str = "L3") -> dict[str, Any]:
        """Create a portable project using millimetres, right-handed coordinates, and Z-up."""
        project = ProjectStore.create(
            root / slugify(name), name, target_fidelity=FidelityLevel(target_fidelity)
        )
        return project.status()

    @mcp.tool(name="vision.observe")
    async def vision_observe(
        project_path: str,
        rights_decision: str,
        source_id: str | None = None,
        adapter: str = "browser.chromium",
        target: dict[str, Any] | None = None,
        configuration: dict[str, Any] | None = None,
        target_url: str | None = None,
        allowed_origins: list[str] | None = None,
        viewport_width: int = 1280,
        viewport_height: int = 720,
        device_scale_factor: float = 1.0,
        browser_engine: str = "chromium",
        is_mobile: bool = False,
        has_touch: bool = False,
        orientation: str | None = None,
        color_scheme: str = "light",
        reduced_motion: str = "no-preference",
        forced_colors: str = "none",
        contrast: str = "no-preference",
        offline: bool = False,
        network_profile: str = "online",
        locale: str = "en-US",
        timezone_id: str = "UTC",
        wait_until: str = "networkidle",
        timeout_ms: int = 30_000,
        allow_private_network: bool = False,
        browser_channel: str = "chrome",
        browser_executable_path: str | None = None,
        headless: bool = True,
        full_page: bool = True,
        execution: str = "local",
    ) -> dict[str, Any]:
        """Capture any installed sensor into a durable OBSERVED evidence envelope."""
        resolved_target = dict(target or {})
        resolved_configuration = dict(configuration or {})
        if adapter == "browser.chromium":
            if target_url is not None:
                resolved_target.setdefault("url", target_url)
            if "url" not in resolved_target:
                raise ValueError("browser.chromium requires target.url or target_url")
            browser_defaults = {
                "viewport": {"width": viewport_width, "height": viewport_height},
                "device_scale_factor": device_scale_factor,
                "engine": browser_engine,
                "is_mobile": is_mobile,
                "has_touch": has_touch,
                "color_scheme": color_scheme,
                "reduced_motion": reduced_motion,
                "forced_colors": forced_colors,
                "contrast": contrast,
                "offline": offline,
                "network_profile": network_profile,
                "locale": locale,
                "timezone_id": timezone_id,
                "wait_until": wait_until,
                "timeout_ms": timeout_ms,
                "allowed_origins": allowed_origins or [],
                "allow_private_network": allow_private_network,
                "channel": browser_channel,
                "executable_path": browser_executable_path,
                "headless": headless,
                "full_page": full_page,
            }
            if orientation is not None:
                browser_defaults["orientation"] = orientation
            for key, value in browser_defaults.items():
                resolved_configuration.setdefault(key, value)
        elif not resolved_target:
            raise ValueError(f"{adapter} requires a typed target")
        project = open_project(project_path)
        if execution == "distributed":
            return enqueue_remote(
                project,
                "perception.capture",
                {
                    "adapter": adapter,
                    "target": resolved_target,
                    "configuration": resolved_configuration,
                    "rights_decision": rights_decision,
                    "source_id": source_id,
                    "worker_requirements": {
                        "worker_classes": ["vision"],
                        "required_capabilities": [
                            "perception.capture",
                            f"adapter.{adapter}",
                        ],
                        "preferred_hardware": ["cuda", "mps", "cpu"],
                        "required_models": [],
                        "min_vram_gb": 0.0,
                        "max_attempts": 3,
                    },
                },
            )
        if execution != "local":
            raise ValueError("vision.observe execution must be local or distributed")
        return await asyncio.to_thread(
            perception_bus(project).observe,
            adapter,
            resolved_target,
            resolved_configuration,
            rights_decision=rights_decision,
            source_id=source_id,
        )

    @mcp.tool(name="vision.query")
    def vision_query(
        project_path: str,
        capture_id: str,
        query: dict[str, Any],
    ) -> dict[str, Any]:
        """Query an observed perceptual graph with an exact artifact citation."""
        project = open_project(project_path)
        if query.get("operation") == "visual_blast_radius":
            return SourceIntelligenceService(project).visual_blast_radius(
                capture_id,
                [str(path) for path in query.get("changed_paths", [])],
                [str(value) for value in query.get("linked_capture_ids", [])] or None,
            )
        return ObservationQueryService(project).query(capture_id, query)

    @mcp.tool(name="vision.discover_states")
    async def vision_discover_states(
        project_path: str,
        target_url: str,
        rights_decision: str,
        allowed_origins: list[str],
        source_id: str | None = None,
        viewport_width: int = 1280,
        viewport_height: int = 720,
        device_scale_factor: float = 1.0,
        browser_engine: str = "chromium",
        is_mobile: bool = False,
        has_touch: bool = True,
        orientation: str | None = None,
        color_scheme: str = "light",
        reduced_motion: str = "no-preference",
        forced_colors: str = "none",
        contrast: str = "no-preference",
        offline: bool = False,
        network_profile: str = "online",
        responsive_viewports: list[dict[str, int]] | None = None,
        input_modes: list[str] | None = None,
        action_limit: int = 24,
        timeline_duration_ms: int = 1200,
        timeline_step_ms: int = 100,
        scroll_steps: int = 5,
        allow_private_network: bool = False,
        browser_channel: str = "chrome",
        browser_executable_path: str | None = None,
        headless: bool = True,
    ) -> dict[str, Any]:
        """Discover only actually observed state, interaction, responsive, and motion evidence."""
        configuration = {
            "viewport": {"width": viewport_width, "height": viewport_height},
            "device_scale_factor": device_scale_factor,
            "engine": browser_engine,
            "is_mobile": is_mobile,
            "has_touch": has_touch,
            "color_scheme": color_scheme,
            "reduced_motion": reduced_motion,
            "forced_colors": forced_colors,
            "contrast": contrast,
            "offline": offline,
            "network_profile": network_profile,
            "responsive_viewports": responsive_viewports
            or [
                {"width": 360, "height": 800},
                {"width": 768, "height": 800},
                {"width": 1280, "height": 800},
            ],
            "input_modes": input_modes or ["pointer", "keyboard", "touch"],
            "action_limit": action_limit,
            "timeline_duration_ms": timeline_duration_ms,
            "timeline_step_ms": timeline_step_ms,
            "scroll_steps": scroll_steps,
            "allowed_origins": allowed_origins,
            "allow_private_network": allow_private_network,
            "channel": browser_channel,
            "executable_path": browser_executable_path,
            "headless": headless,
        }
        if orientation is not None:
            configuration["orientation"] = orientation
        return await asyncio.to_thread(
            perception_bus(open_project(project_path)).observe,
            "browser.experience",
            {"url": target_url},
            configuration,
            rights_decision=rights_decision,
            source_id=source_id,
        )

    @mcp.tool(name="vision.capture_state")
    def vision_capture_state(
        project_path: str,
        capture_id: str,
        state_id: str,
    ) -> dict[str, Any]:
        """Return one actually observed state and its exact screenshot citations."""
        graph = ObservationQueryService(open_project(project_path)).graph(
            capture_id, "StateGraph"
        )
        node = next((item for item in graph["nodes"] if item["id"] == state_id), None)
        if node is None:
            raise KeyError(f"unknown observed state: {state_id}")
        return {"state": node, "citation": graph["citation"]}

    @mcp.tool(name="vision.trace_behavior")
    def vision_trace_behavior(
        project_path: str,
        capture_id: str,
        selector: str | None = None,
        input_mode: str | None = None,
    ) -> dict[str, Any]:
        """Trace observed input-to-visual-effect causality for an experience capture."""
        graph = ObservationQueryService(open_project(project_path)).graph(
            capture_id, "InteractionGraph"
        )
        edges = graph["edges"]
        if selector is not None:
            edges = [
                edge
                for edge in edges
                if selector in {edge.get("source"), edge.get("event_target")}
            ]
        if input_mode is not None:
            edges = [
                edge for edge in edges if edge.get("input", {}).get("mode") == input_mode
            ]
        return {
            "capture_id": capture_id,
            "selector": selector,
            "input_mode": input_mode,
            "transitions": edges,
            "citation": graph["citation"],
        }

    @mcp.tool(name="vision.analyze_motion")
    def vision_analyze_motion(
        project_path: str,
        capture_id: str,
        selector: str | None = None,
    ) -> dict[str, Any]:
        """Return observed motion tracks, compiled curves, and replay bounds."""
        graph = ObservationQueryService(open_project(project_path)).graph(
            capture_id, "MotionGraph"
        )
        tracks = graph["nodes"]
        if selector is not None:
            tracks = [track for track in tracks if track["selector"] == selector]
        return {
            "capture_id": capture_id,
            "selector": selector,
            "tracks": tracks,
            "inference": graph["inference"],
            "reduced_motion_variant": graph["reduced_motion_variant"],
            "replay_contract": graph["replay_contract"],
            "citation": graph["citation"],
        }

    @mcp.tool(name="vision.inspect_graphics")
    async def vision_inspect_graphics(
        project_path: str,
        capture_id: str | None = None,
        target_url: str | None = None,
        rights_decision: str | None = None,
        allowed_origins: list[str] | None = None,
        source_id: str | None = None,
        frame_timestamps_ms: list[int] | None = None,
        require_runtime_scene_hook: bool = False,
        materialize_gltf: bool = True,
        allow_private_network: bool = False,
        browser_channel: str = "chrome",
        headless: bool = True,
    ) -> dict[str, Any]:
        """Capture or progressively disclose governed canvas and graphics-runtime evidence."""
        project = open_project(project_path)
        if capture_id is not None:
            return ObservationQueryService(project).graph(
                capture_id, "GraphicsFrameGraph"
            )
        if not target_url or not rights_decision:
            raise ValueError(
                "graphics capture requires target_url and a non-empty rights_decision"
            )
        return await asyncio.to_thread(
            perception_bus(project).observe,
            "browser.graphics",
            {"url": target_url},
            {
                "allowed_origins": allowed_origins or [],
                "allow_private_network": allow_private_network,
                "channel": browser_channel,
                "headless": headless,
                "frame_timestamps_ms": frame_timestamps_ms or [0, 500, 1000],
                "require_runtime_scene_hook": require_runtime_scene_hook,
                "materialize_gltf": materialize_gltf,
            },
            rights_decision=rights_decision,
            source_id=source_id,
        )

    @mcp.tool(name="vision.reconstruct")
    async def vision_reconstruct(
        project_path: str,
        capture_id: str,
        mode: str = "graphics_to_blender",
    ) -> dict[str, Any]:
        """Materialize an editable candidate without promoting it to accepted authority."""
        if mode == "media_to_interface":
            return MediaReconstructionService(
                open_project(project_path)
            ).reconstruct_interface(capture_id)
        if mode != "graphics_to_blender":
            raise ValueError(f"unsupported reconstruction mode: {mode}")
        return await asyncio.to_thread(
            GraphicsRoundTripService(open_project(project_path)).round_trip,
            capture_id,
        )

    @mcp.tool(name="vision.compare")
    def vision_compare(
        project_path: str,
        capture_a: str,
        capture_b: str,
        bindings: dict[str, str] | None = None,
        selectors: list[str] | None = None,
        thresholds: dict[str, float] | None = None,
    ) -> dict[str, Any]:
        """Compare two governed observations with a domain-specific evidence evaluator."""
        project = open_project(project_path)
        service = ObservationQueryService(project)
        try:
            left = service.graph(capture_a, "DesignSystemGraph")
            right = service.graph(capture_b, "DesignSystemGraph")
        except KeyError:
            try:
                service.graph(capture_a, "LayoutGraph")
                service.graph(capture_b, "LayoutGraph")
            except KeyError as error:
                raise ValueError(
                    "no compatible comparison evaluator is installed for these captures"
                ) from error
            return FrontendComparisonService(project).compare(
                capture_a,
                capture_b,
                selectors=selectors,
                thresholds=thresholds,
            )
        else:
            if {left.get("source_kind"), right.get("source_kind")} != {
                "figma",
                "storybook",
            }:
                raise ValueError(
                    "design comparison requires one Figma and one Storybook capture"
                )
            if left["source_kind"] == "figma":
                figma_capture, storybook_capture = capture_a, capture_b
            else:
                figma_capture, storybook_capture = capture_b, capture_a
            return DesignIntelligenceService(project).analyze_drift(
                figma_capture,
                storybook_capture,
                bindings=bindings,
            )

    @mcp.tool(name="vision.transplant_feature")
    def vision_transplant_feature(
        project_path: str,
        capture_ids: list[str],
        semantic_purpose: str,
        kind: str,
        framework: str,
        owned_asset_mappings: list[dict[str, Any]] | None = None,
        implementation_interface: dict[str, Any] | None = None,
        performance_budget: dict[str, Any] | None = None,
        verification_thresholds: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Compile and verify a clean-room behavior capsule without copying source assets."""
        project = open_project(project_path)
        experience = ExperienceIRCompiler(project).compile(capture_ids)
        capsule = FeatureCapsuleCompiler(project).compile(
            experience["id"],
            semantic_purpose=semantic_purpose,
            kind=kind,
            framework=framework,
            owned_asset_mappings=owned_asset_mappings,
            implementation_interface=implementation_interface,
            performance_budget=performance_budget,
            verification_thresholds=verification_thresholds,
        )
        evaluation = FeatureCapsuleVerifier(project).verify(capsule["id"])
        return {
            "experience_ir": experience,
            "capsule": capsule,
            "evaluation": evaluation,
        }

    @mcp.tool(name="vision.evaluate")
    def vision_evaluate(
        project_path: str,
        capsule_id: str | None = None,
        portfolio_id: str | None = None,
    ) -> dict[str, Any]:
        """Run a capsule evaluation or mandatory global frontend non-regression gate."""
        project = open_project(project_path)
        if capsule_id is not None:
            return FeatureCapsuleVerifier(project).verify(capsule_id)
        if portfolio_id is not None:
            return FrontendRepairService(project).run_global_gate(portfolio_id)
        raise ValueError("vision.evaluate requires capsule_id or portfolio_id")

    @mcp.tool(name="vision.repair")
    def vision_repair(
        project_path: str,
        action: str,
        target_capture_id: str | None = None,
        candidate_capture_id: str | None = None,
        candidates: list[dict[str, Any]] | None = None,
        selectors: list[str] | None = None,
        thresholds: dict[str, float] | None = None,
        target_file: str | None = None,
        portfolio_id: str | None = None,
        proposal_id: str | None = None,
        accepted: bool | None = None,
        reviewer: str | None = None,
        reason: str | None = None,
    ) -> dict[str, Any]:
        """Propose, review, apply, and globally gate bounded frontend repairs."""
        service = FrontendRepairService(open_project(project_path))
        if action == "portfolio":
            if not target_capture_id:
                raise ValueError("portfolio action requires target_capture_id")
            return service.create_portfolio(
                target_capture_id,
                candidates or [],
                locality_selectors=selectors or [],
                thresholds=thresholds,
            )
        if action == "global_gate":
            if not portfolio_id:
                raise ValueError("global_gate action requires portfolio_id")
            return service.run_global_gate(portfolio_id)
        if action == "propose_css":
            if not target_capture_id or not candidate_capture_id or not target_file:
                raise ValueError(
                    "propose_css requires target/candidate captures and target_file"
                )
            return service.propose_css_patch(
                target_capture_id,
                candidate_capture_id,
                target_file=target_file,
                selectors=selectors or [],
            )
        if action == "review":
            if (
                not proposal_id
                or accepted is None
                or reviewer is None
                or reason is None
            ):
                raise ValueError(
                    "review requires proposal_id, accepted, reviewer, and reason"
                )
            return service.review_patch(
                proposal_id,
                accepted=accepted,
                reviewer=reviewer,
                reason=reason,
            )
        if action == "apply":
            if not proposal_id:
                raise ValueError("apply requires proposal_id")
            return service.apply_patch(proposal_id)
        raise ValueError(f"unknown frontend repair action: {action}")

    @mcp.tool(name="vision.review_queue")
    def vision_review_queue(project_path: str) -> dict[str, Any]:
        """List perception decisions that still require named human or policy review."""
        project = open_project(project_path)
        with project.connection() as connection:
            frontend = connection.execute(
                "SELECT id,target_file,created_at FROM frontend_patch_proposals "
                "WHERE status='PROPOSED' ORDER BY created_at"
            ).fetchall()
        return {
            "frontend_patch_proposals": [dict(row) for row in frontend],
            "existing_project_review_queue": ReviewService(project).review_queue(),
        }

    @mcp.tool(name="vision.verify")
    def vision_verify(
        project_path: str,
        capture_id: str | None = None,
        capsule_id: str | None = None,
    ) -> dict[str, Any]:
        """Verify an observation or Feature Capsule by immutable evidence digest."""
        project = open_project(project_path)
        if capsule_id is not None:
            return FeatureCapsuleVerifier(project).verify(capsule_id)
        if capture_id is None:
            raise ValueError("vision.verify requires capture_id or capsule_id")
        bus = perception_bus(project)
        return ObservationQueryService(project, bus).verify(capture_id)

    @mcp.tool(name="vision.explain_region")
    def vision_explain_region(
        project_path: str,
        capture_id: str,
        x: float,
        y: float,
        graph_type: str | None = None,
        source_capture_id: str | None = None,
    ) -> dict[str, Any]:
        """Explain the evidence, authority, and uncertainty at an observed pixel."""
        query: dict[str, Any] = {"point": {"x": x, "y": y}, "limit": 100}
        if graph_type is not None:
            query["graph_type"] = graph_type
        result = ObservationQueryService(open_project(project_path)).query(
            capture_id, query
        )
        response = {
            **result,
            "explanations": [
                {
                    "node_id": node["id"],
                    "domain_type": node.get("domain_type"),
                    "authority": node.get("authority", "UNKNOWN"),
                    "confidence": node.get("confidence"),
                    "uncertainty": node.get("uncertainty", []),
                    "evidence_references": node.get("evidence_references", []),
                    "source_restrictions": node.get("source_restrictions", []),
                }
                for node in result["matches"]
            ],
        }
        if source_capture_id is not None:
            response["source_trace"] = SourceIntelligenceService(
                open_project(project_path)
            ).explain_bindings(result["matches"], source_capture_id)
        return response

    @mcp.tool(name="vision.progress")
    def vision_progress(
        project_path: str,
        capture_ids: list[str] | None = None,
        compute_budget: float = 8.0,
        router_benchmark_cases: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Return compact perception progress, evidence counts, and unresolved blockers."""
        project = open_project(project_path)
        overview = ObservationQueryService(project).overview()
        workspace_service = PerceptionWorkspace(project)
        workspace_run = (
            workspace_service.run(capture_ids, compute_budget=compute_budget)
            if capture_ids
            else None
        )
        router_benchmark = (
            workspace_service.benchmark_router(router_benchmark_cases)
            if router_benchmark_cases
            else None
        )
        with project.connection() as connection:
            counts = {
                table: connection.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
                for table in (
                    "experience_ir_records",
                    "feature_capsules",
                    "capsule_evaluations",
                    "design_drift_runs",
                    "graphics_roundtrips",
                    "media_reconstructions",
                    "perception_workspace_runs",
                    "perception_findings",
                    "perception_contradictions",
                )
            }
            interrupted = connection.execute(
                "SELECT id,adapter FROM observation_captures "
                "WHERE status='INTERRUPTED' ORDER BY updated_at"
            ).fetchall()
            rejected_capsules = connection.execute(
                "SELECT id,status FROM feature_capsules "
                "WHERE status='REJECTED' ORDER BY created_at"
            ).fetchall()
            rejected_portfolios = connection.execute(
                "SELECT id,status FROM frontend_candidate_portfolios "
                "WHERE status='GLOBAL_REJECTED' ORDER BY created_at"
            ).fetchall()
        return {
            "project_id": project.project()["id"],
            "observation_count": len(overview["captures"]),
            "latest_observation": (
                overview["captures"][0] if overview["captures"] else None
            ),
            "counts": counts,
            "workspace_run": workspace_run,
            "router_benchmark": router_benchmark,
            "workspace_progress": workspace_service.progress(),
            "blockers": [
                {"kind": "interrupted_capture", **dict(row)} for row in interrupted
            ]
            + [
                {"kind": "rejected_capsule", **dict(row)}
                for row in rejected_capsules
            ]
            + [
                {"kind": "frontend_global_regression", **dict(row)}
                for row in rejected_portfolios
            ],
        }

    @mcp.tool(name="vision.adapters")
    def vision_adapters() -> dict[str, Any]:
        """List installed sensor adapters without launching a browser."""
        return {"adapters": default_adapter_registry().list()}

    @mcp.tool(name="benchmark.beast_audit")
    def benchmark_beast_audit(project_path: str, stage: int) -> dict[str, Any]:
        """Audit a Beast Mode stage from persisted evidence and record incomplete gates."""
        return BeastBenchmarkAuditor(open_project(project_path)).audit(stage)

    @mcp.tool(name="benchmark.bootstrap_calibration")
    def benchmark_bootstrap_calibration(
        project_path: str, reviewer: str, review_reason: str
    ) -> dict[str, Any]:
        """Generate, review, and require a verified accepted L3 synthetic calibration project."""
        return bootstrap_calibration(
            Path(project_path), reviewer=reviewer, review_reason=review_reason
        )

    @mcp.tool(name="benchmark.bootstrap_dgx_spark")
    def benchmark_bootstrap_dgx_spark(
        project_path: str,
        scene_path: str,
        source_revision: str,
        repository_root: str,
        reference_root: str | None = None,
        source_artifact_paths: list[str] | None = None,
    ) -> dict[str, Any]:
        """Bootstrap an unaccepted, measured DGX Spark reconstruction candidate."""
        return bootstrap_device_benchmark(
            Path(project_path),
            Path(repository_root),
            benchmark="dgx_spark",
            scene_path=Path(scene_path),
            source_revision=source_revision,
            reference_root=Path(reference_root) if reference_root else None,
            source_artifacts=[Path(path) for path in source_artifact_paths or []],
        )

    @mcp.tool(name="benchmark.bootstrap_rtx_5090_fe")
    def benchmark_bootstrap_rtx_5090_fe(
        project_path: str,
        scene_path: str,
        source_revision: str,
        repository_root: str,
        reference_root: str | None = None,
        source_artifact_paths: list[str] | None = None,
    ) -> dict[str, Any]:
        """Bootstrap an unaccepted, measured RTX 5090 FE reconstruction candidate."""
        return bootstrap_device_benchmark(
            Path(project_path),
            Path(repository_root),
            benchmark="rtx_5090_fe",
            scene_path=Path(scene_path),
            source_revision=source_revision,
            reference_root=Path(reference_root) if reference_root else None,
            source_artifacts=[Path(path) for path in source_artifact_paths or []],
        )

    @mcp.tool(name="benchmark.bootstrap_external_perseverance")
    def benchmark_bootstrap_external_perseverance(
        project_path: str,
        repository_root: str,
        reviewed_by: str,
        resource_profile: str = "auto",
    ) -> dict[str, Any]:
        """Bootstrap Stage 4 from reviewed NASA sources and no private starting model."""
        return bootstrap_external_benchmark(
            Path(project_path),
            Path(repository_root),
            reviewed_by=reviewed_by,
            resource_profile=resource_profile,
        )

    @mcp.tool(name="benchmark.revise_rtx_5090_fe_candidate")
    def benchmark_revise_rtx_5090_fe_candidate(
        project_path: str,
        source_revision: str,
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue a distinct strict-v2 RTX candidate with a measured 137 × 40 × 304 mm envelope."""
        return enqueue_background(
            open_project(project_path),
            "benchmark.revise_rtx_5090_fe_candidate",
            {"scene_id": scene_id, "source_revision": source_revision},
        )

    @mcp.tool(name="benchmark.refine_rtx_5090_fe_visual_candidate")
    def benchmark_refine_rtx_5090_fe_visual_candidate(
        project_path: str,
        source_revision: str,
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue a seven-blade RTX visual candidate while retaining strict dimensions."""
        return enqueue_background(
            open_project(project_path),
            "benchmark.refine_rtx_5090_fe_visual_candidate",
            {"scene_id": scene_id, "source_revision": source_revision},
        )

    @mcp.tool(name="benchmark.refine_rtx_5090_fe_front_frame_candidate")
    def benchmark_refine_rtx_5090_fe_front_frame_candidate(
        project_path: str,
        source_revision: str,
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue an evidence-bound RTX front X-frame refinement at strict dimensions."""
        return enqueue_background(
            open_project(project_path),
            "benchmark.refine_rtx_5090_fe_front_frame_candidate",
            {"scene_id": scene_id, "source_revision": source_revision},
        )

    @mcp.tool(name="benchmark.refine_dgx_spark_visual_candidate")
    def benchmark_refine_dgx_spark_visual_candidate(
        project_path: str,
        source_revision: str,
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue a material and rear-I/O DGX candidate while retaining exact body dimensions."""
        return enqueue_background(
            open_project(project_path),
            "benchmark.refine_dgx_spark_visual_candidate",
            {"scene_id": scene_id, "source_revision": source_revision},
        )

    @mcp.tool(name="benchmark.refine_dgx_spark_base_foot_candidate")
    def benchmark_refine_dgx_spark_base_foot_candidate(
        project_path: str,
        source_revision: str,
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue a recessed DGX foot refinement without changing the 150 mm body envelope."""
        return enqueue_background(
            open_project(project_path),
            "benchmark.refine_dgx_spark_base_foot_candidate",
            {"scene_id": scene_id, "source_revision": source_revision},
        )

    @mcp.tool(name="project.status")
    def project_status(project_path: str) -> dict[str, Any]:
        """Return project metadata, evidence counts, and job states."""
        return open_project(project_path).status()

    @mcp.tool(name="project.open")
    def project_open(project_path: str) -> dict[str, Any]:
        """Open and migrate a portable project, returning its canonical resolved path."""
        project = open_project(project_path)
        return {"project_path": str(project.root), **project.status()}

    @mcp.tool(name="project.audit")
    def project_audit(project_path: str) -> dict[str, Any]:
        """Queue an authoritative scene audit with canonical-unit and safe-mode findings."""
        return enqueue_background(open_project(project_path), "project.audit", {})

    @mcp.tool(name="reference.import")
    def reference_import(
        project_path: str,
        source_path: str,
        rights_state: str = "UNKNOWN",
        viewpoint_label: str | None = None,
    ) -> dict[str, Any]:
        """Queue immutable reference ingestion with metadata and quality inspection."""
        return enqueue_background(
            open_project(project_path),
            "reference.import",
            {
                "source": source_path,
                "rights_state": rights_state,
                "viewpoint_label": viewpoint_label,
            },
        )

    @mcp.tool(name="reference.extract_video")
    def reference_extract_video(
        project_path: str,
        source_path: str,
        rights_state: str,
        interval_seconds: float = 1.0,
        maximum_frames: int = 300,
    ) -> dict[str, Any]:
        """Preserve a source video, extract ordered image evidence, and rank frame candidates."""
        return CaptureService(open_project(project_path)).extract_video(
            Path(source_path),
            rights_state=rights_state,
            interval_seconds=interval_seconds,
            maximum_frames=maximum_frames,
        )

    @mcp.tool(name="video.ingest")
    def video_ingest(
        project_path: str,
        source: str,
        rights_state: str,
        interval_seconds: float = 1.0,
        maximum_frames: int = 300,
    ) -> dict[str, Any]:
        """Ingest video, extract frames, classify motion, and select evidence keyframes."""
        return CaptureService(open_project(project_path)).extract_video(
            Path(source),
            rights_state=rights_state,
            interval_seconds=interval_seconds,
            maximum_frames=maximum_frames,
        )

    @mcp.tool(name="video.extract_keyframes")
    def video_extract_keyframes(
        project_path: str,
        source_reference_id: str,
        maximum_selected: int = 36,
    ) -> dict[str, Any]:
        """Reanalyze extracted frames into governed keyframes, motion classes, and tracks."""
        return VideoIntelligenceService(open_project(project_path)).analyze(
            source_reference_id, maximum_selected=maximum_selected
        )

    @mcp.tool(name="reference.select_frames")
    def reference_select_frames(
        project_path: str,
        reference_ids: list[str] | None = None,
        maximum_selected: int = 24,
    ) -> dict[str, Any]:
        """Rank decodable image references by deterministic quality signals for human review."""
        return CaptureService(open_project(project_path)).select_frames(
            reference_ids, maximum_selected=maximum_selected
        )

    @mcp.tool(name="reference.extract_pdf")
    def reference_extract_pdf(
        project_path: str,
        source_path: str,
        rights_state: str,
        maximum_pages: int = 200,
        resolution_dpi: int = 200,
    ) -> dict[str, Any]:
        """Preserve a PDF and import bounded, source-linked page images through Poppler."""
        return ReferenceIngestor(open_project(project_path)).import_pdf_pages(
            Path(source_path),
            rights_state=rights_state,
            maximum_pages=maximum_pages,
            resolution_dpi=resolution_dpi,
        )

    @mcp.tool(name="reference.coverage")
    def reference_coverage(project_path: str) -> dict[str, Any]:
        """Create a coverage and next-best-view report for current image references."""
        return Coordinator(open_project(project_path)).run("validation.coverage", {})["result"]

    @mcp.tool(name="reference.import_reviewed_mask")
    def reference_import_reviewed_mask(
        project_path: str,
        reference_id: str,
        mask_path: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Bind a named, reviewed binary silhouette mask to an immutable image reference."""
        return ReferenceMaskStore(open_project(project_path)).import_reviewed(
            reference_id,
            Path(mask_path),
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="reference.propose_masks")
    def reference_propose_masks(
        project_path: str,
        reference_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Generate replayable automatic mask proposals with no review authority."""
        project = open_project(project_path)
        store = ReferenceMaskStore(project)
        if reference_ids is None:
            reference_ids = [
                item["id"]
                for item in ReferenceIngestor(project).list()
                if item["acceptance_eligible"] and str(item["media_type"]).startswith("image/")
            ]
        proposals = [store.propose_automatic(reference_id) for reference_id in reference_ids]
        return {"proposals": proposals, "count": len(proposals)}

    @mcp.tool(name="reference.review_mask_proposal")
    def reference_review_mask_proposal(
        project_path: str,
        proposal_id: str,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Accept or reject an automatic mask through a named immutable decision."""
        return ReferenceMaskStore(open_project(project_path)).review_proposal(
            proposal_id,
            accepted=accepted,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="reference.list_mask_proposals")
    def reference_list_mask_proposals(
        project_path: str, reference_id: str | None = None
    ) -> dict[str, Any]:
        """List automatic mask proposals and their review state."""
        proposals = ReferenceMaskStore(open_project(project_path)).list_proposals(reference_id)
        return {"proposals": proposals, "count": len(proposals)}

    @mcp.tool(name="reference.list_masks")
    def reference_list_masks(project_path: str, reference_id: str | None = None) -> dict[str, Any]:
        """List approved, hash-bound reference masks and their named review provenance."""
        masks = ReferenceMaskStore(open_project(project_path)).list(reference_id)
        return {"reference_masks": masks}

    @mcp.tool(name="reference.audit_derivations")
    def reference_audit_derivations(project_path: str) -> dict[str, Any]:
        """Audit immutable derived-reference lineage and inherited source governance."""
        from blender_vision.evidence.derivations import ReferenceDerivationStore

        return ReferenceDerivationStore(open_project(project_path)).audit()

    @mcp.tool(name="measurement.add")
    def measurement_add(
        project_path: str,
        measurement_type: str,
        value: dict[str, Any],
        coordinate_frame: str = "canonical",
        certainty: str = "estimated",
        evidence_class: str = "INFERRED_LOW_CONFIDENCE",
        uncertainty: dict[str, Any] | None = None,
        reference_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Record a typed measurement with evidence class, uncertainty, and references."""
        return MeasurementStore(open_project(project_path)).add(
            measurement_type,
            value,
            coordinate_frame=coordinate_frame,
            evidence_class=EvidenceClass(evidence_class),
            qualifier=certainty,
            uncertainty=uncertainty,
            reference_ids=reference_ids,
        )

    @mcp.tool(name="measurement.add_physical")
    def measurement_add_physical(
        project_path: str,
        source_kind: str,
        value: dict[str, Any],
        uncertainty: dict[str, Any],
        calibration_state: dict[str, Any],
        evidence_class: str = "MEASURED",
        reference_ids: list[str] | None = None,
        coordinate_frame: str = "canonical_mm",
        certainty: str = "bounded",
    ) -> dict[str, Any]:
        """Register calibrated physical inputs while retaining uncertainty and calibration."""
        return MeasurementStore(open_project(project_path)).add_physical(
            source_kind,
            value,
            evidence_class=EvidenceClass(evidence_class),
            uncertainty=uncertainty,
            calibration_state=calibration_state,
            reference_ids=reference_ids,
            coordinate_frame=coordinate_frame,
            certainty=certainty,
        )

    @mcp.tool(name="measurement.calibrate")
    def measurement_calibrate(
        project_path: str,
        dimensions_mm: dict[str, float],
        reference_ids: list[str],
        measured_by: str,
        uncertainty_mm: float = 0.1,
    ) -> dict[str, Any]:
        """Record a reviewed x/y/z calibration envelope as authoritative measured evidence."""
        if set(dimensions_mm) != {"x", "y", "z"} or not measured_by.strip():
            raise ValueError("calibration requires x/y/z dimensions and a named measurer")
        store = MeasurementStore(open_project(project_path))
        measurements = [
            store.add(
                "known_overall_dimension",
                {
                    "axis": axis,
                    "millimetres": float(dimensions_mm[axis]),
                    "measurement_method": "calibration_object",
                    "measured_by": measured_by.strip(),
                },
                coordinate_frame="canonical_mm",
                evidence_class=EvidenceClass.MEASURED,
                qualifier="bounded",
                uncertainty={"millimetres": uncertainty_mm},
                reference_ids=reference_ids,
            )
            for axis in ("x", "y", "z")
        ]
        return {"measurements": measurements, "scale_authority": "MEASURED"}

    @mcp.tool(name="measurement.link")
    def measurement_link(
        project_path: str,
        measurement_id: str,
        reference_ids: list[str],
        linked_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Link a stored measurement to reference evidence with named audit provenance."""
        return MeasurementStore(open_project(project_path)).link(
            measurement_id,
            reference_ids,
            linked_by=linked_by,
            reason=reason,
        )

    @mcp.tool(name="measurement.bind_source_provenance")
    def measurement_bind_source_provenance(
        project_path: str,
        measurement_id: str,
        source_id: str,
        claim_locator: str,
    ) -> dict[str, Any]:
        """Bind a manufacturer measurement to a governed source without reviewing its value."""
        return MeasurementStore(open_project(project_path)).bind_source_provenance(
            measurement_id,
            source_id=source_id,
            claim_locator=claim_locator,
        )

    @mcp.tool(name="measurement.correct")
    def measurement_correct(
        project_path: str,
        measurement_id: str,
        value: dict[str, Any],
        uncertainty: dict[str, Any],
        corrected_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Correct metric evidence while retaining the prior value and named audit rationale."""
        return MeasurementStore(open_project(project_path)).correct(
            measurement_id,
            value,
            uncertainty=uncertainty,
            corrected_by=corrected_by,
            reason=reason,
        )

    @mcp.tool(name="measurement.grid_create")
    def measurement_grid_create(
        project_path: str,
        reference_id: str,
        definition: dict[str, Any],
        created_by: str,
        uncertainty: dict[str, Any] | None = None,
        scale_measurement_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a normalized perspective grid with rulers, targets, snapping, and links."""
        return MeasurementGridStore(open_project(project_path)).create(
            reference_id,
            definition,
            created_by=created_by,
            uncertainty=uncertainty,
            scale_measurement_id=scale_measurement_id,
        )

    @mcp.tool(name="measurement.grid_list")
    def measurement_grid_list(project_path: str) -> dict[str, Any]:
        """List perspective-grid records and their uncertainty and scale evidence."""
        return {"measurement_grids": MeasurementGridStore(open_project(project_path)).list()}

    @mcp.tool(name="feature.add")
    def feature_add(
        project_path: str,
        feature_type: str,
        coordinate_frame: str,
        observations: list[dict[str, Any]],
        reference_ids: list[str],
        confidence: float,
        evidence_class: str,
        parent_component: str,
        uncertainty: dict[str, Any] | None = None,
        dimensions: dict[str, float] | None = None,
        model_revision: str = "manual",
    ) -> dict[str, object]:
        """Add an evidence-bound technical feature to the project graph."""
        return FeatureStore(open_project(project_path)).add(
            feature_type,
            parent_component=parent_component,
            dimensions=dimensions or {},
            coordinate_frame=coordinate_frame,
            observations=observations,
            reference_ids=reference_ids,
            confidence=confidence,
            evidence_class=EvidenceClass(evidence_class),
            uncertainty=uncertainty or {},
            model_revision=model_revision,
        )

    @mcp.tool(name="feature.review")
    def feature_review(
        project_path: str,
        feature_id: str,
        approved: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Record an explicit named approval or rejection for one technical feature."""
        return FeatureStore(open_project(project_path)).review(
            feature_id,
            approved=approved,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="feature.link")
    def feature_link(
        project_path: str,
        feature_id: str,
        reference_id: str,
        observation: dict[str, Any],
        linked_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Link a cross-view observation to a technical feature with named provenance."""
        return FeatureStore(open_project(project_path)).link_observation(
            feature_id,
            reference_id,
            observation,
            linked_by=linked_by,
            reason=reason,
        )

    @mcp.tool(name="material.create")
    def material_create(
        project_path: str,
        region_id: str,
        properties: dict[str, Any],
        evidence_class: str,
        confidence: float,
        reference_ids: list[str] | None = None,
        artifact_digests: list[str] | None = None,
        component_id: str | None = None,
        material_slot: str | None = None,
        uncertainty: dict[str, Any] | None = None,
        color_calibration: dict[str, Any] | None = None,
        lighting_estimate: dict[str, Any] | None = None,
        reflective_region_masks: list[str] | None = None,
        multi_light_reference_ids: list[str] | None = None,
        supersedes_id: str | None = None,
        notes: str | None = None,
    ) -> dict[str, Any]:
        """Propose an evidence-bound appearance profile with no geometry authority."""
        return MaterialStore(open_project(project_path)).create(
            region_id,
            properties,
            evidence_class=EvidenceClass(evidence_class),
            confidence=confidence,
            reference_ids=reference_ids,
            artifact_digests=artifact_digests,
            component_id=component_id,
            material_slot=material_slot,
            uncertainty=uncertainty,
            color_calibration=color_calibration,
            lighting_estimate=lighting_estimate,
            reflective_region_masks=reflective_region_masks,
            multi_light_reference_ids=multi_light_reference_ids,
            supersedes_id=supersedes_id,
            notes=notes,
        )

    @mcp.tool(name="material.review")
    def material_review(
        project_path: str,
        profile_id: str,
        approved: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Record a named material-profile approval or rejection."""
        return MaterialStore(open_project(project_path)).review(
            profile_id, approved=approved, reviewer=reviewer, reason=reason
        )

    @mcp.tool(name="material.list")
    def material_list(project_path: str) -> dict[str, Any]:
        """List material properties, appearance evidence, confidence, and review state."""
        return {"material_profiles": MaterialStore(open_project(project_path)).list()}

    @mcp.tool(name="component.create")
    def component_create(
        project_path: str,
        component_id: str,
        component_type: str,
        parameters: dict[str, Any],
        evidence_bindings: list[str] | None = None,
        material_slots: list[str] | None = None,
        lod_rules: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create a typed parametric component bound to project evidence."""
        component = ComponentSpec(
            id=component_id,
            type=ComponentType(component_type),
            parameters=parameters,
            evidence_bindings=evidence_bindings or [],
            material_slots=material_slots or [],
            lod_rules=lod_rules or {},
        )
        return ComponentStore(open_project(project_path)).create(component)

    @mcp.tool(name="component.update")
    def component_update(
        project_path: str, component_id: str, parameters: dict[str, Any]
    ) -> dict[str, Any]:
        """Create a new component revision by updating named parameters."""
        return ComponentStore(open_project(project_path)).update_parameters(
            component_id, parameters
        )

    @mcp.tool(name="component.fit")
    def component_fit(
        project_path: str,
        component_id: str,
        parameter_bindings: dict[str, list[str]],
        huber_delta: float = 1.5,
    ) -> dict[str, Any]:
        """Queue a robust evidence-bound scalar parameter fit without applying it."""
        return enqueue_background(
            open_project(project_path),
            "component.fit",
            {
                "component_id": component_id,
                "parameter_bindings": parameter_bindings,
                "huber_delta": huber_delta,
            },
        )

    @mcp.tool(name="component.review_fit")
    def component_review_fit(
        project_path: str,
        fit_id: str,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Apply or reject a proposed fit through a named, revision-checked decision."""
        return Coordinator(open_project(project_path)).run(
            "component.review_fit",
            {
                "fit_id": fit_id,
                "accepted": accepted,
                "reviewer": reviewer,
                "reason": reason,
            },
        )["result"]

    @mcp.tool(name="component.generate")
    def component_generate(
        project_path: str,
        component_ids: list[str],
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue allowlisted Blender generation into a new audited checkpoint."""
        return enqueue_background(
            open_project(project_path),
            "component.generate",
            {"component_ids": component_ids, "scene_id": scene_id},
        )

    @mcp.tool(name="component.reconstruct")
    def component_reconstruct(
        project_path: str,
        component_ids: list[str],
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Generate semantic parametric components into an isolated audited candidate."""
        return enqueue_background(
            open_project(project_path),
            "component.generate",
            {"component_ids": component_ids, "scene_id": scene_id},
        )

    @mcp.tool(name="component.repair")
    def component_repair(
        project_path: str,
        component_ids: list[str],
        evidence_bindings: list[dict[str, Any]],
        expected_improvement: dict[str, Any],
    ) -> dict[str, Any]:
        """Create a governed component-local repair proposal without mutating the baseline."""
        return RepairStore(open_project(project_path)).propose(
            "semantic_component_repair",
            {"affected_components": component_ids},
            evidence_bindings=evidence_bindings,
            expected_improvement=expected_improvement,
        )

    @mcp.tool(name="recon.optimize")
    def recon_optimize(
        project_path: str,
        component_id: str,
        tier: str,
        method: str,
        candidates: list[dict[str, Any]],
        weights: dict[str, float] | None = None,
        configuration: dict[str, Any] | None = None,
        evidence_binding_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Create a multi-objective optimization proposal without mutating the component."""
        return OptimizationEngine(open_project(project_path)).propose(
            component_id,
            tier=tier,
            method=method,
            candidates=candidates,
            weights=weights,
            configuration=configuration,
            evidence_binding_ids=evidence_binding_ids,
        )

    @mcp.tool(name="recon.optimize_multiview")
    def recon_optimize_multiview(
        project_path: str,
        component_id: str,
        semantic_ids: list[str],
        camera_solution_id: str,
        candidates: list[dict[str, Any]],
        method: str = "bounded_candidate_search",
        weights: dict[str, float] | None = None,
        hierarchy_stage: str = "component_geometry",
    ) -> dict[str, Any]:
        """Propose a fixed-camera component fit from artifact-bound multiview residuals."""
        return OptimizationEngine(open_project(project_path)).propose_multiview(
            component_id,
            semantic_ids=semantic_ids,
            camera_solution_id=camera_solution_id,
            candidates=candidates,
            method=method,
            weights=weights,
            hierarchy_stage=hierarchy_stage,
        )

    @mcp.tool(name="recon.start_multiview_search")
    def recon_start_multiview_search(
        project_path: str,
        component_id: str,
        semantic_ids: list[str],
        camera_solution_id: str,
        parameter_bounds: dict[str, list[float]],
        baseline_scene_id: str | None = None,
        maximum_dimension: int = 512,
        maximum_candidates: int = 17,
        maximum_attempts: int = 2,
    ) -> dict[str, Any]:
        """Plan an idempotent bounded multiview search without mutating the component."""
        return MultiviewSearchStore(open_project(project_path)).start(
            component_id,
            semantic_ids=semantic_ids,
            camera_solution_id=camera_solution_id,
            parameter_bounds=parameter_bounds,
            baseline_scene_id=baseline_scene_id,
            maximum_dimension=maximum_dimension,
            maximum_candidates=maximum_candidates,
            maximum_attempts=maximum_attempts,
        )

    @mcp.tool(name="recon.continue_multiview_search")
    def recon_continue_multiview_search(project_path: str, search_id: str) -> dict[str, Any]:
        """Queue or resume isolated render-and-compare evaluation for a planned search."""
        return enqueue_background(
            open_project(project_path),
            "optimization.execute_multiview_search",
            {"search_id": search_id},
        )

    @mcp.tool(name="recon.list_multiview_searches")
    def recon_list_multiview_searches(project_path: str) -> list[dict[str, Any]]:
        """List persistent multiview searches, attempts, results, and receipt state."""
        return MultiviewSearchStore(open_project(project_path)).list()

    @mcp.tool(name="recon.bootstrap")
    def recon_bootstrap(
        project_path: str,
        scene_path: str | None = None,
        reference_paths: list[str] | None = None,
        rights_state: str = "UNKNOWN",
        camera_backend: str = "heuristic-pinhole",
    ) -> dict[str, Any]:
        """Queue the evidence-through-render bootstrap without claiming final fidelity."""
        references = [
            {"source": path, "rights_state": rights_state} for path in (reference_paths or [])
        ]
        return enqueue_background(
            open_project(project_path),
            "workflow.audit_reference_fidelity",
            {"scene": scene_path, "references": references, "backend": camera_backend},
        )

    @mcp.tool(name="recon.solve")
    def recon_solve(
        project_path: str,
        camera_backend: str = "heuristic-pinhole",
        geometry_backend: str = "auto",
    ) -> dict[str, Any]:
        """Queue independent camera and normalized geometry hypotheses for later consensus."""
        project = open_project(project_path)
        return {
            "camera_job": enqueue_background(
                project, "vision.solve_cameras", {"backend": camera_backend}
            ),
            "geometry_job": enqueue_background(
                project,
                "vision.run",
                {"backend": geometry_backend, "configuration": {}},
            ),
        }

    @mcp.tool(name="recon.audit_existing")
    def recon_audit_existing(project_path: str, scene_id: str | None = None) -> dict[str, Any]:
        """Queue an authoritative audit of the current imported or generated scene."""
        return enqueue_background(
            open_project(project_path), "project.audit", {"scene_id": scene_id}
        )

    @mcp.tool(name="recon.next_best_view")
    def recon_next_best_view(project_path: str) -> dict[str, Any]:
        """Return current uncertainty coverage and actionable missing-view guidance."""
        return Coordinator(open_project(project_path)).run("validation.coverage", {})["result"]

    @mcp.tool(name="recon.review_optimization")
    def recon_review_optimization(
        project_path: str,
        run_id: str,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Apply or reject an optimization proposal through named revision-checked review."""
        return OptimizationEngine(open_project(project_path)).review(
            run_id, accepted=accepted, reviewer=reviewer, reason=reason
        )

    @mcp.tool(name="workflow.fit_component")
    def workflow_fit_component(
        project_path: str,
        component_id: str,
        parameter_bindings: dict[str, list[str]],
        huber_delta: float = 1.5,
    ) -> dict[str, Any]:
        """Queue a robust fit proposal; named review is required before parameters change."""
        return enqueue_background(
            open_project(project_path),
            "component.fit",
            {
                "component_id": component_id,
                "parameter_bindings": parameter_bindings,
                "huber_delta": huber_delta,
            },
        )

    @mcp.tool(name="blender.inspect")
    def blender_inspect(project_path: str) -> dict[str, Any]:
        """Queue safe headless inventory of the latest imported Blender scene."""
        return enqueue_background(open_project(project_path), "blender.inspect", {})

    @mcp.tool(name="blender.render")
    def blender_render(
        project_path: str,
        scene_id: str | None = None,
        solution_id: str | None = None,
        maximum_dimension: int = 1024,
        reference_ids: list[str] | None = None,
        requested_passes: list[str] | None = None,
        regions_by_reference: dict[str, dict[str, int]] | None = None,
    ) -> dict[str, Any]:
        """Queue safe headless renders for the current evidence-bound camera solution."""
        return enqueue_background(
            open_project(project_path),
            "blender.render",
            {
                "scene_id": scene_id,
                "solution_id": solution_id,
                "maximum_dimension": maximum_dimension,
                "reference_ids": reference_ids,
                "requested_passes": requested_passes,
                "regions_by_reference": regions_by_reference,
            },
        )

    @mcp.tool(name="validation.plan_locality")
    def validation_plan_locality(
        project_path: str,
        semantic_ids: list[str],
        change_kind: str,
        camera_solution_id: str | None = None,
    ) -> dict[str, Any]:
        """Plan only affected views, passes, regions, and metrics for a semantic change."""
        return LocalityPlanner(open_project(project_path)).plan(
            semantic_ids,
            change_kind=change_kind,
            camera_solution_id=camera_solution_id,
        )

    @mcp.tool(name="vision.solve_cameras")
    def vision_solve_cameras(
        project_path: str,
        backend: str = "heuristic-pinhole",
        reference_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Queue camera solving while preserving backend authority and uncertainty labels."""
        return enqueue_background(
            open_project(project_path),
            "vision.solve_cameras",
            {"backend": backend, "reference_ids": reference_ids},
        )

    @mcp.tool(name="vision.refine_camera")
    def vision_refine_camera(
        project_path: str,
        source_solution_id: str | None = None,
        reference_id: str | None = None,
        scene_id: str | None = None,
        maximum_dimension: int = 256,
        stages: int = 3,
        evidence_binding_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Queue bounded silhouette camera refinement as an unapproved non-metric proposal."""
        return enqueue_background(
            open_project(project_path),
            "vision.refine_camera",
            {
                "source_solution_id": source_solution_id,
                "reference_id": reference_id,
                "scene_id": scene_id,
                "maximum_dimension": maximum_dimension,
                "stages": stages,
                "evidence_binding_ids": evidence_binding_ids or [],
            },
        )

    @mcp.tool(name="vision.run")
    def vision_run(
        project_path: str,
        backend: str = "auto",
        configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Queue normalized geometry evidence generation without promoting unresolved scale."""
        return enqueue_background(
            open_project(project_path),
            "vision.run",
            {"backend": backend, "configuration": configuration or {}},
        )

    @mcp.tool(name="photogrammetry.run")
    def photogrammetry_run(
        project_path: str,
        backend: str = "colmap",
        configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Queue an evidence-bound photogrammetry lane without promoting unresolved scale."""
        return enqueue_background(
            open_project(project_path),
            "vision.run",
            {"backend": backend, "configuration": configuration or {}},
        )

    @mcp.tool(name="geometry.run_ensemble")
    def geometry_run_ensemble(
        project_path: str,
        backends: list[str],
        configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Queue independent geometry hypotheses followed by license-aware consensus."""
        project = open_project(project_path)
        jobs = [
            enqueue_background(
                project,
                "vision.run",
                {"backend": backend, "configuration": configuration or {}},
            )
            for backend in backends
        ]
        return {"jobs": jobs, "consensus_operation": "vision.compare_backends"}

    @mcp.tool(name="vision.import_geometry_evidence")
    def vision_import_geometry_evidence(
        project_path: str,
        backend: str,
        backend_version: str,
        evidence: dict[str, Any],
        evidence_class: str,
        license_record: dict[str, Any],
        configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Import artifact-bound output from an approved external geometry worker."""
        return GeometryPipeline(open_project(project_path)).import_external(
            backend=backend,
            backend_version=backend_version,
            evidence=evidence,
            evidence_class=evidence_class,
            license_record=license_record,
            configuration=configuration,
        )

    @mcp.tool(name="vision.import_feature_detections")
    def vision_import_feature_detections(
        project_path: str,
        reference_id: str,
        detections: list[dict[str, Any]],
        model_revision: str,
        license_record: dict[str, Any],
    ) -> dict[str, Any]:
        """Import model detections as unapproved evidence-bound technical features."""
        return FeatureDetectionImporter(open_project(project_path)).import_detections(
            reference_id,
            detections,
            model_revision=model_revision,
            license_record=license_record,
        )

    @mcp.tool(name="dataset.plan_synthetic")
    def dataset_plan_synthetic(
        project_path: str,
        name: str,
        sample_count: int,
        seed: int = 0,
        scene_id: str | None = None,
        component_ids: list[str] | None = None,
        domain_randomization: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Plan a deterministic synthetic technical-product dataset with perfect-label outputs."""
        return DatasetStore(open_project(project_path)).plan_synthetic(
            name,
            sample_count=sample_count,
            seed=seed,
            scene_id=scene_id,
            component_ids=component_ids,
            domain_randomization=domain_randomization,
        )

    @mcp.tool(name="dataset.generate")
    def dataset_generate(
        project_path: str, dataset_id: str, execution: str = "local"
    ) -> dict[str, Any]:
        """Queue safe headless Blender rendering for a planned synthetic dataset."""
        project = open_project(project_path)
        if execution == "local":
            return enqueue_background(project, "dataset.generate", {"dataset_id": dataset_id})
        if execution == "distributed":
            return enqueue_remote(project, "dataset.generate", {"dataset_id": dataset_id})
        raise ValueError("dataset execution must be local or distributed")

    @mcp.tool(name="dataset.train_feature_model")
    def dataset_train_feature_model(
        project_path: str,
        dataset_id: str,
        backend: str,
        configuration: dict[str, Any],
    ) -> dict[str, Any]:
        """Plan and queue offline feature-model training; no weights are downloaded."""
        project = open_project(project_path)
        run = TrainingStore(project).plan(dataset_id, backend=backend, configuration=configuration)
        job = enqueue_remote(
            project,
            "training.execute",
            {"training_run_id": run["id"], "dataset_id": dataset_id, "backend": backend},
        )
        return {"training_run": run, "job": job}

    @mcp.tool(name="dataset.import_training_result")
    def dataset_import_training_result(
        project_path: str,
        run_id: str,
        checkpoint_path: str,
        metrics: dict[str, float],
        license_record: dict[str, Any],
        model_revision: str,
    ) -> dict[str, Any]:
        """Import a hash- and license-bound checkpoint produced by an approved worker."""
        return TrainingStore(open_project(project_path)).import_result(
            run_id,
            Path(checkpoint_path),
            metrics=metrics,
            license_record=license_record,
            model_revision=model_revision,
        )

    @mcp.tool(name="dataset.evaluate")
    def dataset_evaluate(
        project_path: str,
        dataset_id: str,
        predictions_path: str,
        training_run_id: str | None = None,
    ) -> dict[str, Any]:
        """Evaluate stored feature predictions with precision, recall, F1, and mask IoU."""
        return TrainingStore(open_project(project_path)).evaluate(
            dataset_id,
            Path(predictions_path),
            training_run_id=training_run_id,
        )

    @mcp.tool(name="active_learning.start")
    def active_learning_start(
        project_path: str,
        model_level: str,
        model_identity: dict[str, Any],
        predictions: list[dict[str, Any]],
        correction_budget: int = 32,
    ) -> dict[str, Any]:
        """Rank low-confidence, high-impact predictions for governed correction."""
        return ActiveLearningStore(open_project(project_path)).start(
            model_level=model_level,
            model_identity=model_identity,
            predictions=predictions,
            correction_budget=correction_budget,
        )

    @mcp.tool(name="active_learning.start_from_workspace")
    def active_learning_start_from_workspace(
        project_path: str,
        workspace_id: str,
        model_level: str,
        model_identity: dict[str, Any],
        correction_budget: int = 32,
    ) -> dict[str, Any]:
        """Rank evidence-bound workspace uncertainty for named correction."""
        return PerceptionLearningService(open_project(project_path)).start_from_workspace(
            workspace_id,
            model_level=model_level,
            model_identity=model_identity,
            correction_budget=correction_budget,
        )

    @mcp.tool(name="active_learning.record_corrections")
    def active_learning_record_corrections(
        project_path: str,
        cycle_id: str,
        corrections: list[dict[str, Any]],
        corrected_by: str,
    ) -> dict[str, Any]:
        """Bind named human corrections to requested predictions."""
        return ActiveLearningStore(open_project(project_path)).record_corrections(
            cycle_id, corrections, corrected_by=corrected_by
        )

    @mcp.tool(name="active_learning.plan_retraining")
    def active_learning_plan_retraining(
        project_path: str,
        cycle_id: str,
        backend: str,
        benchmark_dataset_id: str,
        required_metrics: list[str] | None = None,
        training_configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create a correction dataset and offline training run against a fixed benchmark."""
        return ActiveLearningStore(open_project(project_path)).plan_retraining(
            cycle_id,
            backend=backend,
            benchmark_dataset_id=benchmark_dataset_id,
            required_metrics=required_metrics,
            training_configuration=training_configuration,
        )

    @mcp.tool(name="active_learning.execute_retraining")
    def active_learning_execute_retraining(project_path: str, cycle_id: str) -> dict[str, Any]:
        """Queue the exact training run created by an artifact-bound active-learning plan."""
        project = open_project(project_path)
        cycle = ActiveLearningStore(project).get(cycle_id)
        if cycle["status"] != "RETRAINING_PLANNED":
            raise ValueError("active-learning cycle has no executable retraining plan")
        plan = cycle["retraining_plan"]
        job = enqueue_remote(
            project,
            "training.execute",
            {
                "training_run_id": plan["training_run_id"],
                "dataset_id": plan["correction_dataset_id"],
                "backend": plan["backend"],
                "active_learning_cycle_id": cycle_id,
            },
        )
        return {"cycle": cycle, "job": job}

    @mcp.tool(name="active_learning.compare")
    def active_learning_compare(
        project_path: str,
        cycle_id: str,
        baseline_evaluation_id: str,
        candidate_evaluation_id: str,
    ) -> dict[str, Any]:
        """Recompute non-regression from two stored fixed-benchmark evaluations."""
        return ActiveLearningStore(open_project(project_path)).compare_evaluations(
            cycle_id,
            baseline_evaluation_id=baseline_evaluation_id,
            candidate_evaluation_id=candidate_evaluation_id,
        )

    @mcp.tool(name="active_learning.promote")
    def active_learning_promote(
        project_path: str,
        cycle_id: str,
        reviewed_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Activate a commercially eligible non-regressing checkpoint after named review."""
        return ActiveLearningStore(open_project(project_path)).promote(
            cycle_id, reviewed_by=reviewed_by, reason=reason
        )

    @mcp.tool(name="active_learning.rollback")
    def active_learning_rollback(
        project_path: str,
        active_revision_id: str,
        reviewed_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Receipt-roll back the current revision and restore its predecessor."""
        return ActiveLearningStore(open_project(project_path)).rollback(
            active_revision_id,
            reviewed_by=reviewed_by,
            reason=reason,
        )

    @mcp.tool(name="synthetic_view.register")
    def synthetic_view_register(
        project_path: str,
        artifact_digest: str,
        source_kind: str,
        generator: dict[str, Any],
        input_reference_ids: list[str],
        view_identity: dict[str, Any],
        consistency: dict[str, float],
    ) -> dict[str, Any]:
        """Register a consistency-scored hypothetical view that cannot establish acceptance."""
        return SyntheticViewStore(open_project(project_path)).register(
            artifact_digest,
            source_kind=source_kind,
            generator=generator,
            input_reference_ids=input_reference_ids,
            view_identity=view_identity,
            consistency=consistency,
        )

    @mcp.tool(name="validation.register_visual_oracle")
    def validation_register_visual_oracle(
        project_path: str,
        source_path: str,
        kind: str,
        camera_solution_ids: list[str],
        training_configuration: dict[str, Any],
        license_record: dict[str, Any],
    ) -> dict[str, Any]:
        """Register a Gaussian/neural appearance oracle with hashed camera/training config."""
        return VisualOracleStore(open_project(project_path)).register(
            Path(source_path),
            kind=kind,
            camera_solution_ids=camera_solution_ids,
            training_configuration=training_configuration,
            license_record=license_record,
        )

    @mcp.tool(name="visual_oracle.train")
    def visual_oracle_train(
        project_path: str,
        dataset_id: str,
        kind: str,
        camera_solution_ids: list[str],
        backend: str,
        training_configuration: dict[str, Any],
    ) -> dict[str, Any]:
        """Plan and queue camera-bound offline appearance-oracle training."""
        project = open_project(project_path)
        run = VisualOracleStore(project).plan_training(
            dataset_id,
            kind=kind,
            camera_solution_ids=camera_solution_ids,
            backend=backend,
            training_configuration=training_configuration,
        )
        job = enqueue_remote(
            project,
            "training.execute",
            {
                "training_run_id": run["id"],
                "dataset_id": dataset_id,
                "backend": backend,
                "purpose": "visual_oracle",
                "oracle_kind": kind,
            },
        )
        return {"training_run": run, "job": job}

    @mcp.tool(name="vision.compare_backends")
    def vision_compare_backends(
        project_path: str, run_ids: list[str] | None = None
    ) -> dict[str, Any]:
        """Queue license-aware backend consensus that retains incompatible hypotheses."""
        return enqueue_background(
            open_project(project_path), "vision.compare_backends", {"run_ids": run_ids}
        )

    @mcp.tool(name="vision.compare_camera_solutions")
    def vision_compare_camera_solutions(
        project_path: str, solution_ids: list[str] | None = None
    ) -> dict[str, Any]:
        """Queue camera consensus while retaining hypotheses with incompatible scale."""
        return enqueue_background(
            open_project(project_path),
            "vision.compare_camera_solutions",
            {"solution_ids": solution_ids},
        )

    @mcp.tool(name="vision.import_camera_solution")
    def vision_import_camera_solution(
        project_path: str,
        cameras: list[dict[str, Any]],
        diagnostics: dict[str, Any] | None = None,
        evidence_binding_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Import a validated data-only manual or calibrated camera solution as unapproved."""
        return CameraSolver(open_project(project_path)).import_manual(
            cameras,
            diagnostics=diagnostics,
            evidence_binding_ids=evidence_binding_ids,
        )

    @mcp.tool(name="vision.consolidate_camera_solutions")
    def vision_consolidate_camera_solutions(
        project_path: str,
        solution_ids: list[str],
        require_all_acceptance_references: bool = True,
    ) -> dict[str, Any]:
        """Consolidate disjoint hypotheses into one immutable, still-unapproved camera set."""
        return CameraSolver(open_project(project_path)).consolidate_solutions(
            solution_ids,
            require_all_acceptance_references=require_all_acceptance_references,
        )

    @mcp.tool(name="vision.solve_calibration_board")
    def vision_solve_calibration_board(
        project_path: str,
        columns: int,
        rows: int,
        square_size_measurement_id: str,
    ) -> dict[str, Any]:
        """Recover metric OpenCV cameras from an authoritative measured chessboard."""
        return CameraSolver(open_project(project_path)).solve_calibration_board(
            columns=columns,
            rows=rows,
            square_size_measurement_id=square_size_measurement_id,
        )

    @mcp.tool(name="vision.solve_pnp_landmarks")
    def vision_solve_pnp_landmarks(
        project_path: str,
        landmark_proposal_id: str,
        max_reprojection_rmse_px: float = 4.0,
    ) -> dict[str, Any]:
        """Recover pending metric cameras from an immutable named landmark review."""
        return CameraSolver(open_project(project_path)).solve_pnp_landmarks(
            landmark_proposal_id=landmark_proposal_id,
            max_reprojection_rmse_px=max_reprojection_rmse_px,
        )

    @mcp.tool(name="vision.propose_pnp_landmarks")
    def vision_propose_pnp_landmarks(
        project_path: str,
        target_id: str,
        model_source_id: str,
        intrinsics_solution_id: str,
        evidence_binding_ids: list[str],
        views: list[dict[str, Any]],
        backend_identity: dict[str, Any],
        known_limitations: list[str],
    ) -> dict[str, Any]:
        """Persist machine-proposed image/model landmarks without granting authority."""
        return CameraLandmarkStore(open_project(project_path)).propose(
            target_id=target_id,
            model_source_id=model_source_id,
            intrinsics_solution_id=intrinsics_solution_id,
            evidence_binding_ids=evidence_binding_ids,
            views=views,
            backend_identity=backend_identity,
            known_limitations=known_limitations,
        )

    @mcp.tool(name="vision.propose_pnp_landmarks_from_renders")
    def vision_propose_pnp_landmarks_from_renders(
        project_path: str,
        target_id: str,
        model_source_id: str,
        image_source_ids: list[str],
        intrinsics_solution_id: str,
        evidence_binding_ids: list[str],
        render_manifest_path: str,
        model_to_world_mm: list[list[float]],
        known_limitations: list[str],
        config: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Propose hash-bound render/image landmarks or persist an explicit refusal."""
        return RenderLandmarkMatcher(open_project(project_path)).propose(
            target_id=target_id,
            model_source_id=model_source_id,
            image_source_ids=image_source_ids,
            intrinsics_solution_id=intrinsics_solution_id,
            evidence_binding_ids=evidence_binding_ids,
            render_manifest_path=Path(render_manifest_path),
            model_to_world_mm=model_to_world_mm,
            known_limitations=known_limitations,
            config=config,
        )

    @mcp.tool(name="vision.review_pnp_landmarks")
    def vision_review_pnp_landmarks(
        project_path: str,
        proposal_id: str,
        reviewer: str,
        reason: str,
        decisions: list[dict[str, Any]],
    ) -> dict[str, Any]:
        """Create an immutable named decision for every proposed correspondence."""
        return CameraLandmarkStore(open_project(project_path)).review(
            proposal_id,
            reviewer=reviewer,
            reason=reason,
            decisions=decisions,
        )

    @mcp.tool(name="vision.solve_vanishing_points")
    def vision_solve_vanishing_points(
        project_path: str, grid_ids: list[str] | None = None
    ) -> dict[str, Any]:
        """Recover focal length and orientation from x/y/z perspective-grid vanishing points."""
        return CameraSolver(open_project(project_path)).solve_vanishing_points(grid_ids)

    @mcp.tool(name="vision.review_camera_solution")
    def vision_review_camera_solution(
        project_path: str,
        solution_id: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Explicitly approve a camera solution after its quality evidence has been reviewed."""
        return CameraSolver(open_project(project_path)).approve(
            solution_id, reviewer=reviewer, reason=reason
        )

    @mcp.tool(name="geometry.validate_glb")
    def geometry_validate_glb(
        project_path: str,
        glb_path: str,
        required_node_names: list[str] | None = None,
        required_mesh_names: list[str] | None = None,
    ) -> dict[str, Any]:
        """Validate one project-confined GLB without fetching external resources."""
        from blender_vision.geometry import GlbValidator

        project = open_project(project_path)
        supplied = Path(glb_path).expanduser()
        if not supplied.is_absolute():
            supplied = project.root / supplied
        absolute = supplied.absolute()
        if absolute != project.root and project.root not in absolute.parents:
            raise ValueError("GLB validation path must remain inside the project")
        return GlbValidator().validate(
            absolute,
            required_node_names=required_node_names,
            required_mesh_names=required_mesh_names,
        ).to_dict()

    @mcp.tool(name="blender.export")
    def blender_export(project_path: str, output_name: str = "model.glb") -> dict[str, Any]:
        """Queue safe headless GLB export of the authoritative imported Blender scene."""
        return enqueue_background(
            open_project(project_path), "blender.export", {"output_name": output_name}
        )

    @mcp.tool(name="blender.export_blend")
    def blender_export_blend(project_path: str, output_name: str = "model.blend") -> dict[str, Any]:
        """Queue a portable editable Blender export of the authoritative accepted scene."""
        return enqueue_background(
            open_project(project_path), "blender.export_blend", {"output_name": output_name}
        )

    @mcp.tool(name="blender.generate_lod")
    def blender_generate_lod(
        project_path: str,
        ratio: float = 0.5,
        objects: list[str] | None = None,
    ) -> dict[str, Any]:
        """Queue a non-destructive, audited Blender LOD checkpoint for named mesh objects."""
        return enqueue_background(
            open_project(project_path),
            "blender.generate_lod",
            {"ratio": ratio, "objects": objects or []},
        )

    @mcp.tool(name="blender.prepare_asset")
    def blender_prepare_asset(
        project_path: str,
        targets: list[dict[str, Any]],
        scene_id: str | None = None,
    ) -> dict[str, Any]:
        """Queue bounded retopo, UV, PBR, bake, rig, animation, LOD, collision, and repair."""
        return enqueue_background(
            open_project(project_path),
            "blender.prepare_asset",
            {"scene_id": scene_id, "targets": targets},
        )

    @mcp.tool(name="validation.compare")
    def validation_compare(
        project_path: str,
        scene_id: str | None = None,
        solution_id: str | None = None,
        maximum_dimension: int = 1024,
    ) -> dict[str, Any]:
        """Queue render, silhouette residual, and metric comparison for imported references."""
        return enqueue_background(
            open_project(project_path),
            "validation.compare",
            {
                "scene_id": scene_id,
                "solution_id": solution_id,
                "maximum_dimension": maximum_dimension,
            },
        )

    @mcp.tool(name="visual_geometry.create_rig")
    def visual_geometry_create_rig(
        project_path: str,
        scene_id: str,
        camera_solution_id: str,
        maximum_dimension: int = 1024,
    ) -> dict[str, Any]:
        """Freeze a receipt-bound review rig; unapproved cameras remain diagnostic."""
        return VisualGeometryStore(open_project(project_path)).create_rig(
            scene_id=scene_id,
            camera_solution_id=camera_solution_id,
            maximum_dimension=maximum_dimension,
        )

    @mcp.tool(name="visual_geometry.freeze_baseline")
    def visual_geometry_freeze_baseline(
        project_path: str,
        label: str,
        scene_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Receipt the immutable scene, rig, render, scorecard, export, and benchmark baseline."""
        return VisualBaselineStore(open_project(project_path)).freeze(
            label=label,
            scene_ids=scene_ids,
        )

    @mcp.tool(name="visual_geometry.bind_scene")
    def visual_geometry_bind_scene(
        project_path: str,
        scene_id: str,
        reference_ids: list[str] | None = None,
        classifications: dict[str, dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Create object-level provisional semantic bindings and PART_OF assembly edges."""
        return SemanticBindingStore(open_project(project_path)).propose_scene(
            scene_id,
            reference_ids=reference_ids,
            classifications=classifications,
        )

    @mcp.tool(name="visual_geometry.review_binding")
    def visual_geometry_review_binding(
        project_path: str,
        binding_id: str,
        state: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Named review or acceptance of one receipt-bound visible-object binding."""
        return SemanticBindingStore(open_project(project_path)).review(
            binding_id,
            state=state,
            reviewer=reviewer,
            reason=reason,
        )

    @mcp.tool(name="visual_geometry.repropose_binding")
    def visual_geometry_repropose_binding(
        project_path: str,
        binding_id: str,
        reason: str,
        classification: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Supersede only an unreviewed machine binding after a classifier correction."""
        return SemanticBindingStore(open_project(project_path)).repropose(
            binding_id,
            reason=reason,
            classification=classification,
        )

    @mcp.tool(name="visual_geometry.relate_components")
    def visual_geometry_relate_components(
        project_path: str,
        source_binding_id: str,
        target_binding_id: str,
        relation: str,
        confidence: float,
        evidence: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Add a governed provisional mechanical relationship between bound objects."""
        return SemanticBindingStore(open_project(project_path)).relate(
            source_binding_id,
            target_binding_id,
            relation=relation,
            confidence=confidence,
            evidence=evidence,
        )

    @mcp.tool(name="visual_geometry.create_component_packet")
    def visual_geometry_create_component_packet(
        project_path: str,
        binding_id: str,
        rig_id: str,
        render_run_id: str,
        baseline_render_run_id: str | None = None,
        padding_fraction: float = 0.2,
    ) -> dict[str, Any]:
        """Create native-reference and diagnostic-render crops for one bound component."""
        return ComponentTaskPacketStore(open_project(project_path)).create(
            binding_id=binding_id,
            rig_id=rig_id,
            render_run_id=render_run_id,
            baseline_render_run_id=baseline_render_run_id,
            padding_fraction=padding_fraction,
        )

    @mcp.tool(name="visual_geometry.score_frequencies")
    def visual_geometry_score_frequencies(
        project_path: str,
        scene_id: str,
        rig_id: str,
        packet_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Aggregate primary, secondary, and tertiary component scores without area masking."""
        return VisualFrequencyScoreStore(open_project(project_path)).create(
            scene_id=scene_id,
            rig_id=rig_id,
            packet_ids=packet_ids,
        )

    @mcp.tool(name="visual_geometry.evaluate")
    def visual_geometry_evaluate(
        project_path: str,
        rig_id: str,
        reference_id: str,
        render_run_id: str,
        mask_id: str | None = None,
        mask_proposal_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a replayable projection, edge, local, and perceptual scorecard."""
        return VisualGeometryStore(open_project(project_path)).evaluate(
            rig_id=rig_id,
            reference_id=reference_id,
            render_run_id=render_run_id,
            mask_id=mask_id,
            mask_proposal_id=mask_proposal_id,
        )

    @mcp.tool(name="visual_geometry.audit_manufactured_form")
    def visual_geometry_audit_manufactured_form(project_path: str, scene_id: str) -> dict[str, Any]:
        """Audit malformed meshes and manufactured-form consistency from scene evidence."""
        return ManufacturedFormAuditor(open_project(project_path)).audit(scene_id)

    @mcp.tool(name="visual_geometry.diagnose_residuals")
    def visual_geometry_diagnose_residuals(
        project_path: str,
        scorecard_id: str,
        rollback_scene_id: str,
        packet_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        """Translate replayable residuals into bounded diagnostic repair hypotheses."""
        return VisualDefectDiagnosisStore(open_project(project_path)).create(
            scorecard_id=scorecard_id,
            rollback_scene_id=rollback_scene_id,
            packet_ids=packet_ids,
        )

    @mcp.tool(name="visual_geometry.repair_degenerate_candidate")
    def visual_geometry_repair_degenerate_candidate(
        project_path: str,
        scene_id: str,
        object_name: str,
        expected_degenerate_faces: int,
        area_epsilon: float = 1e-14,
        merge_distance: float = 1e-10,
    ) -> dict[str, Any]:
        """Create a guarded topology-repair candidate without accepting or promoting it."""
        return enqueue_background(
            open_project(project_path),
            "visual_geometry.repair_degenerate_candidate",
            {
                "scene_id": scene_id,
                "object_name": object_name,
                "expected_degenerate_faces": expected_degenerate_faces,
                "area_epsilon": area_epsilon,
                "merge_distance": merge_distance,
            },
        )

    @mcp.tool(name="visual_geometry.list")
    def visual_geometry_list(project_path: str, scene_id: str | None = None) -> dict[str, Any]:
        """List fixed rigs, scorecards, and manufactured-form audit records."""
        project = open_project(project_path)
        store = VisualGeometryStore(project)
        return {
            "baseline_freezes": VisualBaselineStore(project).list(),
            "rigs": store.list_rigs(),
            "scorecards": store.list_scorecards(scene_id),
            "manufactured_form_audits": ManufacturedFormAuditor(project).list(scene_id),
            "semantic_bindings": SemanticBindingStore(project).list(scene_id),
            "component_packets": ComponentTaskPacketStore(project).list(),
            "visual_frequency_scorecards": VisualFrequencyScoreStore(project).list(scene_id),
            "visual_defect_diagnoses": VisualDefectDiagnosisStore(project).list(scene_id),
        }

    @mcp.tool(name="visual_geometry.verify")
    def visual_geometry_verify(
        project_path: str,
        record_type: str,
        record_id: str,
    ) -> dict[str, Any]:
        """Replay and verify a fixed rig, scorecard, or manufactured-form audit receipt."""
        project = open_project(project_path)
        if record_type == "rig":
            return VisualGeometryStore(project).verify_rig(record_id)
        if record_type == "scorecard":
            return VisualGeometryStore(project).verify_scorecard(record_id, replay=True)
        if record_type == "manufactured_form_audit":
            return ManufacturedFormAuditor(project).verify(record_id)
        if record_type == "baseline_freeze":
            return VisualBaselineStore(project).verify(record_id)
        if record_type == "semantic_binding":
            return SemanticBindingStore(project).verify(record_id)
        if record_type == "component_packet":
            return ComponentTaskPacketStore(project).verify(record_id)
        if record_type == "visual_frequency_scorecard":
            return VisualFrequencyScoreStore(project).verify(record_id)
        if record_type == "visual_defect_diagnosis":
            return VisualDefectDiagnosisStore(project).verify(record_id)
        raise ValueError(
            "record_type must be rig, scorecard, manufactured_form_audit, "
            "baseline_freeze, semantic_binding, component_packet, or "
            "visual_frequency_scorecard, or visual_defect_diagnosis"
        )

    @mcp.tool(name="validation.recompute_legacy_comparison")
    def validation_recompute_legacy_comparison(
        project_path: str, comparison_id: str
    ) -> dict[str, Any]:
        """Recompute immutable inputs and supersede unreproducible legacy comparison metrics."""
        from blender_vision.comparison.store import ComparisonStore

        return ComparisonStore(open_project(project_path)).recompute_and_supersede(comparison_id)

    @mcp.tool(name="validation.supersede_duplicate_comparison")
    def validation_supersede_duplicate_comparison(
        project_path: str, comparison_id: str, canonical_comparison_id: str
    ) -> dict[str, Any]:
        """Receipt-supersede a replay-identical duplicate comparison without deleting history."""
        from blender_vision.comparison.store import ComparisonStore

        return ComparisonStore(open_project(project_path)).supersede_duplicate(
            comparison_id, canonical_comparison_id=canonical_comparison_id
        )

    def acceptance_metrics(project_path: str) -> dict[str, Any]:
        return Coordinator(open_project(project_path)).run("receipt.export", {})["result"][
            "acceptance"
        ]

    @mcp.tool(name="appearance.compare")
    def appearance_compare(project_path: str) -> dict[str, Any]:
        """Report governed appearance and full-object comparison gates."""
        acceptance = acceptance_metrics(project_path)
        return {
            "accepted": acceptance["accepted"],
            "appearance": acceptance["metrics"].get("appearance", {}),
            "comparisons": acceptance["metrics"].get("comparison_selection", {}),
            "blockers": [
                item
                for item in acceptance["blockers"]
                if any(term in item for term in ("material", "appearance", "silhouette"))
            ],
        }

    @mcp.tool(name="material.compare")
    def material_compare(project_path: str) -> dict[str, Any]:
        """Report material-region evidence, calibration, confidence, and review gates."""
        acceptance = acceptance_metrics(project_path)
        return {
            "appearance": acceptance["metrics"].get("appearance", {}),
            "blockers": [item for item in acceptance["blockers"] if "material" in item],
        }

    @mcp.tool(name="geometry.compare")
    def geometry_compare(project_path: str) -> dict[str, Any]:
        """Report dimensional, semantic, topology, and candidate-transaction geometry gates."""
        acceptance = acceptance_metrics(project_path)
        metrics = acceptance["metrics"]
        return {
            "scene_lifecycle": metrics.get("scene_lifecycle", {}),
            "candidate_transactions": metrics.get("candidate_transactions", {}),
            "dimension_residuals": metrics.get("dimension_residuals", {}),
            "semantic_twin": metrics.get("semantic_twin", {}),
            "geometry_evidence": metrics.get("geometry_evidence", {}),
            "blockers": [
                item
                for item in acceptance["blockers"]
                if any(
                    term in item
                    for term in ("scene", "dimension", "topology", "component", "geometry")
                )
            ],
        }

    @mcp.tool(name="uncertainty.audit")
    def uncertainty_audit(project_path: str) -> dict[str, Any]:
        """Distinguish observed, measured, inferred, unseen, and synthetic project claims."""
        acceptance = acceptance_metrics(project_path)
        return {
            "project_status": acceptance["project_status"],
            "requested_tier": acceptance["target_fidelity"],
            "passed_tier": acceptance["accepted_fidelity"],
            "evidence_authority": acceptance["metrics"].get("evidence_authority", {}),
            "coverage": acceptance["metrics"].get("coverage", {}),
            "blockers": acceptance["blockers"],
        }

    @mcp.tool(name="validation.coverage")
    def validation_coverage(project_path: str) -> dict[str, Any]:
        """Queue a coverage and next-best-view report."""
        return enqueue_background(open_project(project_path), "validation.coverage", {})

    @mcp.tool(name="validation.acceptance")
    def validation_acceptance(project_path: str) -> dict[str, Any]:
        """Evaluate every current fidelity gate and emit a verifiable acceptance receipt."""
        return Coordinator(open_project(project_path)).run("receipt.export", {})["result"]

    @mcp.tool(name="repair.propose_mac_studio_grille")
    def repair_propose_mac_studio_grille(project_path: str) -> dict[str, Any]:
        """Create an evidence-bound Mac Studio rear-grille proposal without editing geometry."""
        project = open_project(project_path)
        return Coordinator(project).run("repair.propose_mac_studio_grille", {})["result"]

    @mcp.tool(name="recon.repair")
    def recon_repair(
        project_path: str,
        repair_kind: str = "generic_semantic_component_repair",
        affected_components: list[str] | None = None,
        evidence_bindings: list[dict[str, Any]] | None = None,
        expected_improvement: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Propose a supported repair; a separate named approval is required before application."""
        project = open_project(project_path)
        if repair_kind == "mac_studio_rear_hero_grille":
            return Coordinator(project).run("repair.propose_mac_studio_grille", {})["result"]
        return RepairStore(project).propose(
            repair_kind,
            {"affected_components": affected_components or []},
            evidence_bindings=evidence_bindings or [],
            expected_improvement=expected_improvement or {},
        )

    @mcp.tool(name="repair.approve")
    def repair_approve(project_path: str, proposal_id: str, approved_by: str) -> dict[str, Any]:
        """Authorize safe checkpoint evaluation; this does not accept generated geometry."""
        return RepairStore(open_project(project_path)).approve(proposal_id, approved_by)

    @mcp.tool(name="repair.apply")
    def repair_apply(
        project_path: str, proposal_id: str, scene_id: str | None = None
    ) -> dict[str, Any]:
        """Queue an approved repair, checkpoint audit, topology/ray validation, and rear render."""
        return enqueue_background(
            open_project(project_path),
            "repair.apply",
            {"proposal_id": proposal_id, "scene_id": scene_id},
        )

    @mcp.tool(name="repair.review")
    def repair_review(
        project_path: str,
        proposal_id: str,
        accepted: bool,
        reviewer: str,
        reason: str,
        receipt_id: str | None = None,
    ) -> dict[str, Any]:
        """Record final repair acceptance or rejection; acceptance requires a clean receipt."""
        return Coordinator(open_project(project_path)).run(
            "repair.review",
            {
                "proposal_id": proposal_id,
                "accepted": accepted,
                "reviewer": reviewer,
                "reason": reason,
                "receipt_id": receipt_id,
            },
        )["result"]

    @mcp.tool(name="receipt.export")
    def receipt_export(project_path: str) -> dict[str, Any]:
        """Queue a machine-verifiable acceptance receipt without overstating fidelity."""
        return enqueue_background(open_project(project_path), "receipt.export", {})

    @mcp.tool(name="review.snapshot")
    def review_snapshot(project_path: str) -> dict[str, Any]:
        """Return the complete read-only review model used by the local browser application."""
        return ReviewService(open_project(project_path)).snapshot()

    @mcp.tool(name="review.request_capture")
    def review_request_capture(
        project_path: str,
        direction: str,
        requester: str,
        reason: str,
        region: str | None = None,
        instructions: str | None = None,
    ) -> dict[str, Any]:
        """Record a named, coverage-driven request for an additional capture view."""
        return ReviewService(open_project(project_path)).action(
            "capture.request",
            {
                "direction": direction,
                "region": region,
                "instructions": instructions,
                "reviewer": requester,
                "reason": reason,
            },
        )

    @mcp.tool(name="review.model_tier")
    def review_model_tier(
        project_path: str,
        fidelity: str,
        accepted: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Record a tier decision; acceptance cannot exceed a valid receipt's fidelity."""
        return ReviewService(open_project(project_path)).action(
            "tier.review",
            {
                "fidelity": fidelity,
                "accepted": accepted,
                "reviewer": reviewer,
                "reason": reason,
            },
        )

    @mcp.tool(name="job.status")
    def job_status(project_path: str, job_id: str) -> dict[str, Any]:
        """Get structured job state, result, error, and event history."""
        return open_project(project_path).job(job_id)

    @mcp.tool(name="job.cancel")
    def job_cancel(project_path: str, job_id: str) -> dict[str, Any]:
        """Request cooperative cancellation of a queued or running job."""
        project = open_project(project_path)
        project.request_cancel(job_id)
        return project.job(job_id)

    @mcp.tool(name="worker.register")
    def worker_register(
        project_path: str,
        name: str,
        worker_class: str,
        capabilities: dict[str, Any],
        enrollment_token: str,
    ) -> dict[str, Any]:
        """Enroll a capability-advertising worker and return its token exactly once."""
        require_worker_enrollment(enrollment_token)
        return DistributedScheduler(open_project(project_path)).register(
            name, worker_class, capabilities
        )

    @mcp.tool(name="worker.heartbeat")
    def worker_heartbeat(
        project_path: str,
        worker_id: str,
        worker_token: str,
        load: dict[str, Any],
        artifact_digests: list[str] | None = None,
    ) -> dict[str, Any]:
        """Refresh worker load, warm-model state, and content-addressed artifact locality."""
        return DistributedScheduler(open_project(project_path)).heartbeat(
            worker_id,
            worker_token,
            load=load,
            artifact_digests=artifact_digests,
        )

    @mcp.tool(name="worker.claim")
    def worker_claim(
        project_path: str,
        worker_id: str,
        worker_token: str,
        lease_seconds: int = 120,
    ) -> dict[str, Any] | None:
        """Claim the best hardware-, load-, model-, and locality-matched queued job."""
        return DistributedScheduler(open_project(project_path)).claim(
            worker_id, worker_token, lease_seconds=lease_seconds
        )

    @mcp.tool(name="worker.renew")
    def worker_renew(
        project_path: str,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        lease_seconds: int = 120,
    ) -> dict[str, Any]:
        """Renew an authenticated live job lease while a long operation is progressing."""
        return DistributedScheduler(open_project(project_path)).renew(
            worker_id,
            worker_token,
            job_id,
            lease_token,
            lease_seconds=lease_seconds,
        )

    @mcp.tool(name="worker.complete")
    def worker_complete(
        project_path: str,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        result: dict[str, Any],
        output_artifact_digests: list[str] | None = None,
    ) -> dict[str, Any]:
        """Complete a leased job only after every declared output artifact is registered."""
        return DistributedScheduler(open_project(project_path)).complete(
            worker_id,
            worker_token,
            job_id,
            lease_token,
            result=result,
            output_artifact_digests=output_artifact_digests,
        )

    @mcp.tool(name="worker.fail")
    def worker_fail(
        project_path: str,
        worker_id: str,
        worker_token: str,
        job_id: str,
        lease_token: str,
        error: dict[str, Any],
        retryable: bool = True,
    ) -> dict[str, Any]:
        """Report a structured failure and retry up to the job's configured attempt limit."""
        return DistributedScheduler(open_project(project_path)).fail(
            worker_id,
            worker_token,
            job_id,
            lease_token,
            error=error,
            retryable=retryable,
        )

    @mcp.tool(name="worker.list")
    def worker_list(project_path: str) -> dict[str, Any]:
        """List registered workers, advertised hardware, current load, and liveness."""
        return DistributedScheduler(open_project(project_path)).snapshot()

    @mcp.tool(name="artifact.describe")
    def artifact_describe(
        project_path: str,
        worker_id: str,
        worker_token: str,
        digest: str,
    ) -> dict[str, Any]:
        """Describe an input artifact after authenticating the requesting worker."""
        project = open_project(project_path)
        DistributedScheduler(project).authenticate(worker_id, worker_token)
        return ArtifactTransfer(project).describe(digest)

    @mcp.tool(name="artifact.read_chunk")
    def artifact_read_chunk(
        project_path: str,
        worker_id: str,
        worker_token: str,
        digest: str,
        offset: int,
        maximum_bytes: int = 1024 * 1024,
    ) -> dict[str, Any]:
        """Read at most 1 MiB from an immutable input artifact for remote execution."""
        return ArtifactTransfer(open_project(project_path)).read_chunk(
            worker_id,
            worker_token,
            digest,
            offset=offset,
            maximum_bytes=maximum_bytes,
        )

    @mcp.tool(name="artifact.begin_upload")
    def artifact_begin_upload(
        project_path: str,
        worker_id: str,
        worker_token: str,
        expected_digest: str,
        expected_size: int,
        media_type: str,
        source_name: str,
    ) -> dict[str, Any]:
        """Begin a sequential digest-declared worker output upload."""
        return ArtifactTransfer(open_project(project_path)).begin_upload(
            worker_id,
            worker_token,
            expected_digest=expected_digest,
            expected_size=expected_size,
            media_type=media_type,
            source_name=source_name,
        )

    @mcp.tool(name="artifact.upload_chunk")
    def artifact_upload_chunk(
        project_path: str,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
        offset: int,
        data_base64: str,
    ) -> dict[str, Any]:
        """Append one authenticated base64 chunk to a declared output transfer."""
        return ArtifactTransfer(open_project(project_path)).upload_chunk(
            worker_id,
            worker_token,
            transfer_id,
            offset=offset,
            data_base64=data_base64,
        )

    @mcp.tool(name="artifact.complete_upload")
    def artifact_complete_upload(
        project_path: str,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
    ) -> dict[str, Any]:
        """Verify declared size/SHA-256 and atomically register a worker output artifact."""
        return ArtifactTransfer(open_project(project_path)).complete_upload(
            worker_id, worker_token, transfer_id
        )

    @mcp.tool(name="artifact.abort_upload")
    def artifact_abort_upload(
        project_path: str,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
    ) -> dict[str, Any]:
        """Abort the authenticated worker's incomplete artifact transfer."""
        return ArtifactTransfer(open_project(project_path)).abort_upload(
            worker_id, worker_token, transfer_id
        )

    @mcp.tool(name="artifact.reap_stale_uploads")
    def artifact_reap_stale_uploads(
        project_path: str, maximum_age_seconds: int = 3600
    ) -> dict[str, Any]:
        """Expire abandoned partial uploads while preserving registered artifacts."""
        return ArtifactTransfer(open_project(project_path)).reap_stale(
            maximum_age_seconds=maximum_age_seconds
        )

    @mcp.tool(name="workflow.audit_reference_fidelity")
    def workflow_audit_reference_fidelity(
        project_path: str,
        scene_path: str | None = None,
        reference_paths: list[str] | None = None,
        rights_state: str = "UNKNOWN",
        backend: str = "heuristic-pinhole",
        maximum_dimension: int = 1024,
    ) -> dict[str, Any]:
        """Run the first complete audit slice asynchronously from evidence through receipt."""
        references = [
            {"source": path, "rights_state": rights_state} for path in (reference_paths or [])
        ]
        return enqueue_background(
            open_project(project_path),
            "workflow.audit_reference_fidelity",
            {
                "scene": scene_path,
                "references": references,
                "backend": backend,
                "maximum_dimension": maximum_dimension,
            },
        )

    @mcp.tool(name="workflow.reconstruct_product")
    def workflow_reconstruct_product(
        project_path: str,
        scene_path: str | None = None,
        reference_paths: list[str] | None = None,
        rights_state: str = "UNKNOWN",
        backend: str = "auto",
        maximum_dimension: int = 1024,
    ) -> dict[str, Any]:
        """Queue the complete staged reconstruction/audit workflow and retain all blockers."""
        references = [
            {"source": path, "rights_state": rights_state} for path in (reference_paths or [])
        ]
        return enqueue_background(
            open_project(project_path),
            "workflow.audit_reference_fidelity",
            {
                "scene": scene_path,
                "references": references,
                "backend": backend,
                "maximum_dimension": maximum_dimension,
            },
        )

    @mcp.tool(name="workflow.repair_existing_model")
    def workflow_repair_existing_model(
        project_path: str,
        repair_kind: str = "generic_semantic_component_repair",
        affected_components: list[str] | None = None,
        evidence_bindings: list[dict[str, Any]] | None = None,
        expected_improvement: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Create an evidence-bound repair proposal; authorization and review remain separate."""
        project = open_project(project_path)
        if repair_kind == "mac_studio_rear_hero_grille":
            return Coordinator(project).run("repair.propose_mac_studio_grille", {})["result"]
        proposal = RepairStore(project).propose(
            repair_kind,
            {"affected_components": affected_components or []},
            evidence_bindings=evidence_bindings or [],
            expected_improvement=expected_improvement or {},
        )
        campaign = CampaignStore(project).start(
            "existing_model_repair",
            configuration={
                "repair_proposal_id": proposal["id"],
                "affected_components": affected_components or [],
            },
        )
        return {"repair_proposal": proposal, "campaign": campaign}

    @mcp.tool(name="workflow.prepare_capture_session")
    def workflow_prepare_capture_session(
        project_path: str, maximum_selected_frames: int = 24
    ) -> dict[str, Any]:
        """Combine frame triage, coverage, and concrete capture guidance."""
        project = open_project(project_path)
        coverage = Coordinator(project).run("validation.coverage", {})["result"]
        selection = CaptureService(project).select_frames(maximum_selected=maximum_selected_frames)
        return {
            "coverage": coverage,
            "frame_selection": selection,
            "capture_checklist": [
                "lock focus and exposure where possible",
                "capture front, rear, left, right, top, and bottom with overlap",
                "include a measured calibration object in the same focal plane",
                "avoid clipped highlights, motion blur, and changing digital zoom",
                "record rights and source provenance before import",
            ],
        }

    @mcp.tool(name="workflow.prepare_acceptance")
    def workflow_prepare_acceptance(project_path: str) -> dict[str, Any]:
        """Refresh coverage and receipt gates, then return the named human-review queue."""
        project = open_project(project_path)
        coverage = Coordinator(project).run("validation.coverage", {})["result"]
        receipt = Coordinator(project).run("receipt.export", {})["result"]
        return {
            "coverage": coverage,
            "receipt": receipt,
            "review_queue": ReviewService(project).review_queue(),
            "ready": bool(receipt["acceptance"]["accepted"]),
        }

    @mcp.tool(name="workflow.deliver_promoted")
    def workflow_deliver_promoted(
        project_path: str,
        scene_id: str | None = None,
        output_prefix: str | None = None,
    ) -> dict[str, Any]:
        """Queue verified BLEND, GLB, and receipt delivery for a safely promoted scene."""
        return enqueue_background(
            open_project(project_path),
            "workflow.deliver_promoted",
            {"scene_id": scene_id, "output_prefix": output_prefix},
        )

    @mcp.resource("project://{project_id}/summary")
    def project_summary(project_id: str) -> dict[str, Any]:
        return by_id(project_id).status()

    @mcp.resource("vision://project/{project_id}/overview")
    def vision_project_overview(project_id: str) -> dict[str, Any]:
        return ObservationQueryService(by_id(project_id)).overview()

    @mcp.resource("vision://project/{project_id}/graph/{graph_type}")
    def vision_project_graph(project_id: str, graph_type: str) -> dict[str, Any]:
        service = ObservationQueryService(by_id(project_id))
        return service.graph(service.latest_capture_id(graph_type), graph_type)

    @mcp.resource("vision://project/{project_id}/node/{node_id}")
    def vision_project_node(project_id: str, node_id: str) -> dict[str, Any]:
        service = ObservationQueryService(by_id(project_id))
        with service.project.connection() as connection:
            rows = connection.execute(
                "SELECT capture_id,graph_type FROM perceptual_graphs "
                "ORDER BY created_at DESC,id DESC"
            ).fetchall()
        for row in rows:
            graph = service.graph(row["capture_id"], row["graph_type"])
            node = next(
                (item for item in graph.get("nodes", []) if item["id"] == node_id),
                None,
            )
            if node is not None:
                return {
                    "capture_id": row["capture_id"],
                    "graph_type": row["graph_type"],
                    "node": node,
                    "citation": graph["citation"],
                }
        raise KeyError(f"unknown perceptual node: {node_id}")

    @mcp.resource("vision://project/{project_id}/timeline/{timeline_id}")
    def vision_project_timeline(project_id: str, timeline_id: str) -> dict[str, Any]:
        service = ObservationQueryService(by_id(project_id))
        capture_id = service.latest_capture_id("MotionGraph")
        graph = service.graph(capture_id, "MotionGraph")
        timeline = next(
            (item for item in graph.get("timelines", []) if item["id"] == timeline_id),
            None,
        )
        if timeline is None:
            raise KeyError(f"unknown timeline in latest observation: {timeline_id}")
        return {
            "capture_id": capture_id,
            "timeline": timeline,
            "citation": graph["citation"],
        }

    @mcp.resource("vision://project/{project_id}/artifact/{digest}")
    def vision_project_artifact(project_id: str, digest: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            artifact = connection.execute(
                "SELECT digest,size,media_type,relative_path,source_name,created_at "
                "FROM artifacts WHERE digest=?",
                (digest,),
            ).fetchone()
            roles = connection.execute(
                "SELECT capture_id,role,metadata_json FROM observation_capture_artifacts "
                "WHERE artifact_digest=? ORDER BY capture_id,role",
                (digest,),
            ).fetchall()
        if artifact is None:
            raise KeyError(f"unknown artifact: {digest}")
        return {
            **dict(artifact),
            "exists": (project.root / artifact["relative_path"]).is_file(),
            "capture_roles": [
                {
                    "capture_id": row["capture_id"],
                    "role": row["role"],
                    "metadata": json.loads(row["metadata_json"]),
                }
                for row in roles
            ],
            "content_disclosure": "use the governed artifact path only when raw evidence is needed",
        }

    @mcp.resource("vision://project/{project_id}/candidate/{candidate_id}")
    def vision_project_candidate(project_id: str, candidate_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        try:
            return FeatureCapsuleCompiler(project).get(candidate_id)
        except KeyError:
            with project.connection() as connection:
                row = connection.execute(
                    "SELECT * FROM frontend_candidates WHERE id=?",
                    (candidate_id,),
                ).fetchone()
            if row is None:
                raise KeyError(
                    f"unknown perception candidate: {candidate_id}"
                ) from None
            value = dict(row)
            value["parameters"] = __import__("json").loads(
                value.pop("parameters_json")
            )
            return value

    @mcp.resource("vision://project/{project_id}/evaluation/{evaluation_id}")
    def vision_project_evaluation(project_id: str, evaluation_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            row = connection.execute(
                "SELECT report_digest FROM capsule_evaluations WHERE id=?",
                (evaluation_id,),
            ).fetchone()
            if row is None:
                row = connection.execute(
                    "SELECT report_digest FROM frontend_global_gate_runs WHERE id=?",
                    (evaluation_id,),
                ).fetchone()
        if row is None:
            raise KeyError(f"unknown capsule evaluation: {evaluation_id}")
        return __import__("json").loads(
            ArtifactStore(project)
            .path_for(row["report_digest"])
            .read_text(encoding="utf-8")
        )

    @mcp.resource("project://{project_id}/references")
    def project_references(project_id: str) -> dict[str, Any]:
        return {"references": ReferenceIngestor(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/scene")
    def project_scene(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        store = SceneStore(project)
        return {"scenes": [store.get(row["id"]) for row in store.list()]}

    @mcp.resource("project://{project_id}/jobs")
    def project_jobs(project_id: str) -> dict[str, Any]:
        return {"jobs": by_id(project_id).list_jobs()}

    @mcp.resource("project://{project_id}/workers")
    def project_workers(project_id: str) -> dict[str, Any]:
        return DistributedScheduler(by_id(project_id)).snapshot()

    @mcp.resource("project://{project_id}/models")
    def project_models(project_id: str) -> dict[str, Any]:
        return ModelStore(by_id(project_id)).list()

    @mcp.resource("project://{project_id}/measurements")
    def project_measurements(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        return {
            "measurements": MeasurementStore(project).list(),
            "measurement_grids": MeasurementGridStore(project).list(),
        }

    @mcp.resource("project://{project_id}/features")
    def project_features(project_id: str) -> dict[str, Any]:
        return {"features": FeatureStore(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/cameras")
    def project_cameras(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            rows = connection.execute(
                "SELECT id,backend,solution_json,diagnostics_json,created_at,approved "
                "FROM camera_solutions ORDER BY created_at"
            ).fetchall()
        return {
            "camera_solutions": [
                {
                    **dict(row),
                    "solution": __import__("json").loads(row["solution_json"]),
                    "diagnostics": __import__("json").loads(row["diagnostics_json"]),
                    "approved": bool(row["approved"]),
                }
                for row in rows
            ],
            "refinement_runs": CameraRefiner(project).list(),
            "latest_consensus": CameraConsensus(project).latest(),
        }

    @mcp.resource("project://{project_id}/components")
    def project_components(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        return {
            "components": ComponentStore(project).list(),
            "fits": ComponentFitter(project).list(),
        }

    @mcp.resource("project://{project_id}/materials")
    def project_materials(project_id: str) -> dict[str, Any]:
        return {"material_profiles": MaterialStore(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/repairs")
    def project_repairs(project_id: str) -> dict[str, Any]:
        return {"repairs": RepairStore(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/geometry")
    def project_geometry(project_id: str) -> dict[str, Any]:
        store = GeometryEvidenceStore(by_id(project_id))
        return {"runs": store.list(), "latest_consensus": store.latest_consensus()}

    @mcp.resource("project://{project_id}/fits")
    def project_fits(project_id: str) -> dict[str, Any]:
        return {"fits": ComponentFitter(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/datasets")
    def project_datasets(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        return {
            "datasets": DatasetStore(project).list(),
            "training_runs": TrainingStore(project).list(),
        }

    @mcp.resource("project://{project_id}/visual-oracles")
    def project_visual_oracles(project_id: str) -> dict[str, Any]:
        return {"visual_oracles": VisualOracleStore(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/optimization")
    def project_optimization(project_id: str) -> dict[str, Any]:
        return {"optimization_runs": OptimizationEngine(by_id(project_id)).list()}

    @mcp.resource("project://{project_id}/coverage")
    def project_coverage(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            row = connection.execute(
                "SELECT report_json FROM coverage_reports ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        return {"coverage": __import__("json").loads(row[0]) if row else None}

    @mcp.resource("project://{project_id}/residuals")
    def project_residuals(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            rows = connection.execute(
                "SELECT id,reference_id,render_digest,residual_digest,metrics_json,created_at "
                "FROM comparisons ORDER BY created_at"
            ).fetchall()
        return {
            "residuals": [
                {
                    **dict(row),
                    "metrics": __import__("json").loads(row["metrics_json"]),
                }
                for row in rows
            ]
        }

    @mcp.resource("project://{project_id}/latest-receipt")
    def project_latest_receipt(project_id: str) -> dict[str, Any]:
        project = by_id(project_id)
        with project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM receipts ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        return dict(row) if row else {"receipt": None}

    @mcp.resource("project://{project_id}/review-queue")
    def project_review_queue(project_id: str) -> dict[str, Any]:
        return {"review_queue": ReviewService(by_id(project_id)).review_queue()}

    @mcp.prompt(name="audit-existing-model")
    def audit_existing_model_prompt(project_path: str) -> str:
        return (
            f"Audit the Blender Vision project at {project_path}. Start with project.status, then "
            "call "
            "workflow.audit_reference_fidelity. Monitor job.status until terminal. Report every "
            "acceptance blocker and never upgrade inferred evidence to measured evidence."
        )

    @mcp.prompt(name="prepare-acceptance")
    def prepare_acceptance_prompt(project_path: str) -> str:
        return (
            f"Prepare acceptance for {project_path}. Inspect coverage and the latest receipt. "
            "Distinguish "
            "verified receipt integrity from accepted fidelity, and recommend missing evidence."
        )

    @mcp.prompt(name="repair-from-evidence")
    def repair_from_evidence_prompt(project_path: str) -> str:
        return (
            f"Inspect evidence and repair proposals for {project_path}. Use recon.repair only to "
            "create an evidence-bound proposal. Explain its evidence and expected improvement, "
            "obtain named authorization for checkpoint evaluation through repair.approve, then "
            "call repair.apply. "
            "Export a receipt, resolve every evidence blocker, and use repair.review for the final "
            "named decision. Report topology, ray, render, and acceptance evidence without "
            "claiming L3 prematurely."
        )

    @mcp.prompt(name="reconstruct-product")
    def reconstruct_product_prompt(project_path: str) -> str:
        return (
            f"Reconstruct the product in {project_path} with workflow.reconstruct_product. "
            "Preserve every source hash and evidence class, monitor the job, review camera and "
            "feature uncertainty, and report receipt blockers instead of inferring acceptance."
        )

    @mcp.prompt(name="fit-technical-component")
    def fit_technical_component_prompt(project_path: str, component_id: str) -> str:
        return (
            f"Fit component {component_id} in {project_path}. Inspect its evidence bindings, use "
            "workflow.fit_component to propose a robust fit, verify constraints, and require a "
            "named component.review_fit decision before applying parameters."
        )

    @mcp.prompt(name="plan-capture")
    def plan_capture_prompt(project_path: str) -> str:
        return (
            f"Prepare a capture session for {project_path} using "
            "workflow.prepare_capture_session. Prioritize missing directions and weak evidence, "
            "include a calibration object, and record rights before importing new frames."
        )

    @mcp.prompt(name="review-uncertainty")
    def review_uncertainty_prompt(project_path: str) -> str:
        return (
            f"Review uncertainty in {project_path}. Inspect cameras, geometry hypotheses, "
            "residuals, coverage, and the review queue. Keep incompatible scale hypotheses "
            "separate and request new evidence where acceptance authority is insufficient."
        )

    # ------------------------------------------------------------------ V2 MCP surface
    # Bible 18.2 tools. Each delegates to an existing V2 subsystem; none reimplements
    # authority rules. Sealed V2 records are returned via to_dict() so digests travel.

    def _v2_project_dir(project: ProjectStore, *parts: str) -> Path:
        path = project.root / "v2" / Path(*parts)
        path.mkdir(parents=True, exist_ok=True)
        return path

    def _authority(value: str | None, default: AuthorityClass) -> AuthorityClass:
        if value is None or value == "":
            return default
        return AuthorityClass(value)

    def _surface_cell(payload: dict[str, Any]) -> SurfaceCell:
        return SurfaceCell(
            region=str(payload["region"]),
            area_m2=float(payload.get("area_m2", 1.0)),
            covered=bool(payload.get("covered", False)),
            incidence_angle_deg=payload.get("incidence_angle_deg"),
            resolution_px=int(payload.get("resolution_px", 0)),
            occlusion_fraction=float(payload.get("occlusion_fraction", 1.0)),
            view_ids=list(payload.get("view_ids") or []),
            candidate_predictions=[
                float(item) for item in (payload.get("candidate_predictions") or [])
            ],
        )

    def _perception_target(payload: dict[str, Any]) -> PerceptionTarget:
        cells = [_surface_cell(item) for item in payload.get("cells") or []]
        signatures = payload.get("existing_view_signatures") or []
        return PerceptionTarget(
            target_id=str(payload["target_id"]),
            cells=cells,
            scale_authority=_authority(
                payload.get("scale_authority"), AuthorityClass.UNRESOLVED
            ),
            material_confidences=[
                float(item) for item in (payload.get("material_confidences") or [])
            ],
            portfolio_predictions=dict(payload.get("portfolio_predictions") or {}),
            has_scale_reference=bool(payload.get("has_scale_reference", False)),
            has_diffuse_light_view=bool(payload.get("has_diffuse_light_view", False)),
            has_grazing_light_view=bool(payload.get("has_grazing_light_view", False)),
            has_lens_metadata=bool(payload.get("has_lens_metadata", False)),
            has_calibration_target=bool(payload.get("has_calibration_target", False)),
            gates_satisfied=bool(payload.get("gates_satisfied", False)),
            user_declined=bool(payload.get("user_declined", False)),
            existing_view_signatures=set(str(item) for item in signatures),
        )

    def _surface_observation(payload: dict[str, Any]) -> SurfaceObservation:
        import numpy as np

        return SurfaceObservation(
            view_id=str(payload["view_id"]),
            rgb=np.asarray(payload["rgb"], dtype=np.float64),
            mask=None if payload.get("mask") is None else np.asarray(payload["mask"]),
            highlight_mask=(
                None
                if payload.get("highlight_mask") is None
                else np.asarray(payload["highlight_mask"])
            ),
            view_direction=tuple(payload.get("view_direction") or (0.0, 0.0, 1.0)),
            light_direction=(
                None
                if payload.get("light_direction") is None
                else tuple(payload["light_direction"])
            ),
            authority=_authority(payload.get("authority"), AuthorityClass.OBSERVED),
        )

    def _lighting_observation(payload: dict[str, Any]) -> LightingObservation:
        import numpy as np

        return LightingObservation(
            view_id=str(payload["view_id"]),
            rgb=np.asarray(payload["rgb"], dtype=np.float64),
            mask=None if payload.get("mask") is None else np.asarray(payload["mask"]),
            normals=(
                None if payload.get("normals") is None else np.asarray(payload["normals"])
            ),
            authority=_authority(payload.get("authority"), AuthorityClass.OBSERVED),
        )

    def _reconstruction_candidate(payload: dict[str, Any]) -> ReconstructionCandidate:
        return ReconstructionCandidate.from_dict(payload)

    def _camera_view(payload: dict[str, Any]) -> CameraView:
        import numpy as np

        return CameraView(
            name=str(payload.get("name", "cam")),
            width=int(payload["width"]),
            height=int(payload["height"]),
            fx=float(payload["fx"]),
            fy=float(payload["fy"]),
            cx=float(payload["cx"]),
            cy=float(payload["cy"]),
            world_from_camera=np.asarray(payload["world_from_camera"], dtype=np.float64),
            near=float(payload.get("near", 0.01)),
            far=float(payload.get("far", 100.0)),
        )

    def _blocked(reason: str, **extra: Any) -> dict[str, Any]:
        return {"status": "blocked", "reason": reason, **extra}

    @mcp.tool(name="vision.capture_world")
    def vision_capture_world(
        project_path: str,
        target_id: str,
        target: dict[str, Any],
        cameras: list[dict[str, Any]] | None = None,
        observation_bundle_ids: list[str] | None = None,
        resolution: int = 6,
    ) -> dict[str, Any]:
        """Evaluate coverage and seal a SceneEvidenceGraph with visibility-capped authority."""
        import numpy as np

        project = open_project(project_path)
        cameras = list(cameras or [])
        atlas = CoverageAtlas()
        if "bounds_min" in target and "bounds_max" in target:
            patches = atlas.sample_box_patches(
                np.asarray(target["bounds_min"], dtype=np.float64),
                np.asarray(target["bounds_max"], dtype=np.float64),
                resolution=resolution,
            )
        elif "center" in target and "radius" in target:
            patches = atlas.sample_sphere_patches(
                np.asarray(target["center"], dtype=np.float64),
                float(target["radius"]),
                n_lat=max(4, resolution),
                n_lon=max(8, resolution * 2),
            )
        else:
            raise ValidationError(
                "target must provide bounds_min/bounds_max or center/radius"
            )
        report = atlas.evaluate(patches, cameras)
        graph = report.seal_scene_evidence(
            target_id=target_id,
            observation_bundle_ids=observation_bundle_ids,
            operation="vision.capture_world",
        )
        graph.cameras = list(cameras)
        graph.digest = ""
        graph.seal()
        out = _v2_project_dir(project, "spatial")
        path = out / f"{graph.id}.json"
        path.write_text(
            json.dumps(graph.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "coverage": report.to_dict(),
            "record": graph.to_dict(),
            "path": str(path),
        }

    @mcp.tool(name="vision.import_depth")
    def vision_import_depth(
        project_path: str,
        target_id: str,
        path: str,
        intrinsics: dict[str, float],
        format: str = "npy",
        authority: str = "SENSOR_DERIVED",
        kind: str = "METRIC",
        depth_scale: float = 0.001,
        units: str = "m",
    ) -> dict[str, Any]:
        """Ingest a depth map and seal an ObservationBundle at the declared authority."""
        project = open_project(project_path)
        auth = _authority(authority, AuthorityClass.SENSOR_DERIVED)
        depth_kind = DepthKind(kind)
        unit = Units(units)
        source = Path(path)
        if format == "npy":
            depth_map = DepthMap.from_npy(
                source,
                intrinsics=intrinsics,
                kind=depth_kind,
                units=unit,
                authority=auth,
            )
        elif format in {"png16", "png"}:
            depth_map = DepthMap.from_16bit_png(
                source,
                intrinsics=intrinsics,
                depth_scale=depth_scale,
                kind=depth_kind,
                units=unit,
                authority=auth,
            )
        else:
            raise ValidationError(f"unsupported depth format: {format!r}")
        bundle = depth_map.seal_observation_bundle(
            target_id=target_id, operation="vision.import_depth"
        )
        out = _v2_project_dir(project, "spatial", "depth")
        record_path = out / f"{bundle.id}.json"
        record_path.write_text(
            json.dumps(bundle.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "summary": depth_map.summary(),
            "record": bundle.to_dict(),
            "path": str(record_path),
        }

    @mcp.tool(name="vision.import_point_cloud")
    def vision_import_point_cloud(
        project_path: str,
        target_id: str,
        path: str,
        authority: str = "SENSOR_DERIVED",
    ) -> dict[str, Any]:
        """Load a PLY point cloud and seal an ObservationBundle preserving source authority."""
        project = open_project(project_path)
        cloud = SpatialPointCloud.read_ply(
            Path(path),
            authority=_authority(authority, AuthorityClass.SENSOR_DERIVED),
        )
        cloud.source_path = str(Path(path).resolve())
        bundle = cloud.seal_observation_bundle(
            target_id=target_id, operation="vision.import_point_cloud"
        )
        out = _v2_project_dir(project, "spatial", "pointclouds")
        record_path = out / f"{bundle.id}.json"
        record_path.write_text(
            json.dumps(bundle.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "point_count": len(cloud),
            "bounds": cloud.bounds(),
            "record": bundle.to_dict(),
            "path": str(record_path),
        }

    @mcp.tool(name="vision.plan_capture")
    def vision_plan_capture(
        project_path: str,
        target_id: str,
        target: dict[str, Any],
        budget: int,
        existing_views: list[dict[str, Any]] | None = None,
        candidate_views: list[dict[str, Any]] | None = None,
        resolution: int = 6,
        n_candidates: int = 24,
    ) -> dict[str, Any]:
        """Propose coverage-maximising views; sealed plan carries HYPOTHETICAL authority."""
        project = open_project(project_path)
        plan = plan_capture(
            target,
            list(existing_views or []),
            budget,
            candidate_views=candidate_views,
            resolution=resolution,
            n_candidates=n_candidates,
        )
        bundle = plan.seal_observation_bundle(
            target_id=target_id, operation="vision.plan_capture"
        )
        return {
            "project": str(project.root),
            "plan": plan.to_dict(),
            "record": bundle.to_dict(),
        }

    @mcp.tool(name="vision.ask_next_view")
    def vision_ask_next_view(
        project_path: str,
        target: dict[str, Any],
        gain_threshold: float | None = None,
        max_requests: int | None = None,
    ) -> dict[str, Any]:
        """Return ordered NextViewRequest records ranked by expected information gain."""
        project = open_project(project_path)
        cfg_kwargs: dict[str, Any] = {}
        if gain_threshold is not None:
            cfg_kwargs["gain_threshold"] = float(gain_threshold)
        if max_requests is not None:
            cfg_kwargs["max_requests"] = int(max_requests)
        planner = NextBestViewPlanner(PlannerConfig(**cfg_kwargs) if cfg_kwargs else None)
        result = planner.plan(_perception_target(target))
        return {
            "project": str(project.root),
            "stop_reason": result.stop_reason.value if result.stop_reason else None,
            "uncertainty_before": result.uncertainty_before,
            "notes": list(result.notes),
            "requests": [item.to_dict() for item in result.requests],
        }

    @mcp.tool(name="vision.build_reconstruction_portfolio")
    def vision_build_reconstruction_portfolio(
        project_path: str,
        target_id: str,
        backends: list[str] | None = None,
        image_dir: str | None = None,
        points: list[list[float]] | None = None,
        cameras: list[dict[str, Any]] | None = None,
        bounds_min: list[float] | None = None,
        bounds_max: list[float] | None = None,
        primitive_kind: str | None = None,
        library_dir: str | None = None,
        archetype_id: str | None = None,
        browser_scene: dict[str, Any] | None = None,
        metric_anchor_m: float | None = None,
        selected_candidate_id: str | None = None,
    ) -> dict[str, Any]:
        """Run requested reconstruction backends and seal a ReconstructionPortfolio."""
        import numpy as np

        project = open_project(project_path)
        names = list(backends or list(_V2_RECONSTRUCTION_BACKENDS))
        unknown = [name for name in names if name not in _V2_RECONSTRUCTION_BACKENDS]
        if unknown:
            raise ValidationError(f"unknown reconstruction backends: {unknown}")
        backend_objs = [_V2_RECONSTRUCTION_BACKENDS[name]() for name in names]
        work_dir = _v2_project_dir(project, "reconstruction", target_id)
        recon_points = None
        if points is not None:
            arr = np.asarray(points, dtype=np.float64)
            recon_points = ReconstructionPointCloud(positions=arr)
        inputs = ReconstructionInputs(
            target_id=target_id,
            work_dir=work_dir,
            image_dir=Path(image_dir) if image_dir else None,
            cameras=[_camera_view(item) for item in (cameras or [])],
            bounds_min=None if bounds_min is None else np.asarray(bounds_min, dtype=np.float64),
            bounds_max=None if bounds_max is None else np.asarray(bounds_max, dtype=np.float64),
            points=recon_points,
            primitive_kind=primitive_kind,
            library_dir=Path(library_dir) if library_dir else None,
            archetype_id=archetype_id,
            browser_scene=browser_scene,
            metric_anchor_m=metric_anchor_m,
        )
        portfolio = build_portfolio(
            target_id=target_id,
            backends=backend_objs,
            inputs_for=inputs,
            selected_candidate_id=selected_candidate_id,
        )
        path = work_dir / f"{portfolio.id}.json"
        path.write_text(
            json.dumps(portfolio.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "record": portfolio.to_dict(),
            "path": str(path),
        }

    @mcp.tool(name="vision.compare_reconstruction_backends")
    def vision_compare_reconstruction_backends(
        project_path: str,
        candidates: list[dict[str, Any]],
        samples: int = 1500,
    ) -> dict[str, Any]:
        """Compare reconstruction candidates metric-by-metric without a single fused score."""
        project = open_project(project_path)
        parsed = [_reconstruction_candidate(item) for item in candidates]
        if len(parsed) < 2:
            raise ValidationError(
                "compare_reconstruction_backends requires at least two candidates"
            )
        if len(parsed) == 2:
            comparison = compare_candidates(parsed[0], parsed[1], samples=samples)
        else:
            comparison = compare_all(parsed)
        return {"project": str(project.root), "comparison": comparison}

    @mcp.tool(name="vision.fit_parametric_model")
    def vision_fit_parametric_model(
        project_path: str,
        points: list[list[float]],
        kind: str = "auto",
        iterations: int = 400,
        threshold: float = 0.01,
        seed: int = 0,
    ) -> dict[str, Any]:
        """Fit a RANSAC plane/box/cylinder/sphere; result authority is MODEL_DERIVED."""
        import numpy as np

        project = open_project(project_path)
        fit = fit_primitive(
            np.asarray(points, dtype=np.float64),
            kind=kind,
            iterations=iterations,
            threshold=threshold,
            seed=seed,
        )
        return {
            "project": str(project.root),
            "fit": {
                "kind": fit.kind,
                "parameters": fit.parameters,
                "inlier_ratio": fit.inlier_ratio,
                "residual_rmse": fit.residual_rmse,
                "inlier_count": fit.inlier_count,
                "sample_count": fit.sample_count,
            },
            "authority": AuthorityClass.MODEL_DERIVED.value,
        }

    @mcp.tool(name="vision.generate_procedural_scene")
    def vision_generate_procedural_scene(
        project_path: str,
        scene_id: str | None = None,
        flagship: bool = True,
        program: dict[str, Any] | None = None,
        text_safe_zones: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Compile a sealed ProceduralSceneGraph at PROCEDURAL_GROUND_TRUTH authority."""
        project = open_project(project_path)
        if flagship and program is None:
            compiled = build_flagship_scene(scene_id=scene_id)
        else:
            scene_program = (
                SceneProgram.from_dict(program)
                if program is not None
                else datacenter_flagship_program()
            )
            compiled = compile_scene(
                scene_program,
                scene_id=scene_id,
                text_safe_zones=text_safe_zones,
            )
        out = _v2_project_dir(project, "procedural")
        path = out / f"{compiled.record.id}.json"
        path.write_text(
            json.dumps(compiled.record.to_dict(), indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        return {
            "project": str(project.root),
            "instance_count": len(compiled.instances),
            "bounds_m": compiled.bounds_m,
            "record": compiled.record.to_dict(),
            "path": str(path),
        }

    @mcp.tool(name="vision.generate_archetype")
    def vision_generate_archetype(
        project_path: str,
        name: str,
        params: dict[str, Any] | None = None,
        emit_blender: bool = False,
        output_name: str | None = None,
    ) -> dict[str, Any]:
        """Instantiate a procedural archetype at PROCEDURAL_GROUND_TRUTH; optional Blender emit."""
        project = open_project(project_path)
        library = default_library()
        if name not in library:
            raise ValidationError(
                f"unknown archetype {name!r}; known: {list_archetypes()}"
            )
        arch = library.create(name, params)
        arch.assert_dimensions(tolerance_m=0.001)
        parts = arch.build()
        payload: dict[str, Any] = {
            "project": str(project.root),
            "archetype": arch.to_manifest_entry(),
            "fingerprint": mesh_fingerprint(parts),
            "declared_dimensions": arch.declared_dimensions().to_dict(),
            "measured_dimensions": arch.measured_dimensions().to_dict(),
            "dimension_deltas_m": arch.dimension_deltas_m(),
            "authority": AuthorityClass.PROCEDURAL_GROUND_TRUTH.value,
            "known_archetypes": list_archetypes(),
        }
        if emit_blender:
            out = _v2_project_dir(project, "procedural", "archetypes", output_name or name)
            try:
                emit_result = emit_archetype(name, out, params=params, library=library)
                payload["emit"] = emit_result.to_dict()
            except BackendUnavailable as error:
                payload["emit"] = _blocked(str(error))
            except Exception as error:  # noqa: BLE001 — report exact failure, never fake success
                payload["emit"] = _blocked(
                    f"{type(error).__name__}: {error}",
                    note="Python/runtime failure, not attributed to hardware",
                )
        return payload

    @mcp.tool(name="vision.infer_materials")
    def vision_infer_materials(
        project_path: str,
        observations: list[dict[str, Any]],
        surfaces: list[dict[str, Any]],
        surface_id: str | None = None,
    ) -> dict[str, Any]:
        """Infer a MaterialHypothesisSet capped by derive() over observation authorities."""
        project = open_project(project_path)
        obs = [_surface_observation(item) for item in observations]
        regions = [
            SurfaceRegion(
                surface_id=str(item["surface_id"]),
                label=str(item.get("label", "")),
                normal=tuple(item.get("normal") or (0.0, 0.0, 1.0)),
                known_geometry=bool(item.get("known_geometry", True)),
            )
            for item in surfaces
        ]
        record = infer_materials(obs, regions, surface_id=surface_id)
        out = _v2_project_dir(project, "materials")
        path = out / f"{record.id}.json"
        path.write_text(
            json.dumps(record.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {"project": str(project.root), "record": record.to_dict(), "path": str(path)}

    @mcp.tool(name="vision.solve_lighting")
    def vision_solve_lighting(
        project_path: str,
        observations: list[dict[str, Any]],
        geometry: dict[str, Any],
    ) -> dict[str, Any]:
        """Solve a LightingHypothesisSet capped by derive() over inputs."""
        project = open_project(project_path)
        obs = [_lighting_observation(item) for item in observations]
        geom = GeometryContext(
            scene_id=str(geometry.get("scene_id", "scene")),
            shape=str(geometry.get("shape", "sphere")),
            center=tuple(geometry.get("center") or (0.0, 0.0, 0.0)),
            radius=float(geometry.get("radius", 1.0)),
            authority=_authority(
                geometry.get("authority"), AuthorityClass.PROCEDURAL_GROUND_TRUTH
            ),
            metadata=dict(geometry.get("metadata") or {}),
        )
        record = solve_lighting(obs, geom)
        out = _v2_project_dir(project, "lighting")
        path = out / f"{record.id}.json"
        path.write_text(
            json.dumps(record.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {"project": str(project.root), "record": record.to_dict(), "path": str(path)}

    @mcp.tool(name="vision.optimize_inverse_render")
    def vision_optimize_inverse_render(
        project_path: str,
        observations: list[dict[str, Any]],
        surfaces: list[dict[str, Any]],
        geometry: dict[str, Any],
        max_iterations: int = 4,
        lighting_observations: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        """Joint material/light solve; records stay separate and never share authority."""
        project = open_project(project_path)
        mat_obs = [_surface_observation(item) for item in observations]
        regions = [
            SurfaceRegion(
                surface_id=str(item["surface_id"]),
                label=str(item.get("label", "")),
                normal=tuple(item.get("normal") or (0.0, 0.0, 1.0)),
                known_geometry=bool(item.get("known_geometry", True)),
            )
            for item in surfaces
        ]
        geom = GeometryContext(
            scene_id=str(geometry.get("scene_id", "scene")),
            shape=str(geometry.get("shape", "sphere")),
            center=tuple(geometry.get("center") or (0.0, 0.0, 0.0)),
            radius=float(geometry.get("radius", 1.0)),
            authority=_authority(
                geometry.get("authority"), AuthorityClass.PROCEDURAL_GROUND_TRUTH
            ),
            metadata=dict(geometry.get("metadata") or {}),
        )
        light_obs = (
            [_lighting_observation(item) for item in lighting_observations]
            if lighting_observations is not None
            else None
        )
        result = joint_solve(
            mat_obs,
            regions,
            geom,
            max_iterations=max_iterations,
            lighting_observations=light_obs,
        )
        return {
            "project": str(project.root),
            "material_record": result.material_record.to_dict(),
            "lighting_record": result.lighting_record.to_dict(),
            "residual_history": list(result.residual_history),
            "iterations": result.iterations,
            "joint_metadata": dict(result.joint_metadata),
        }

    @mcp.tool(name="vision.generate_texture_set")
    def vision_generate_texture_set(
        project_path: str,
        hypothesis: dict[str, Any],
        size: int = 256,
        seed: int = 0,
        output_subdir: str = "textures",
    ) -> dict[str, Any]:
        """Generate tileable PBR maps from a MaterialHypothesis (not observed textures)."""
        project = open_project(project_path)
        hyp = MaterialHypothesis.from_dict(hypothesis)
        out = _v2_project_dir(project, "materials", output_subdir, hyp.hypothesis_id)
        texture_set = generate_texture_set(hyp, size=size, output_dir=out, seed=seed)
        return {
            "project": str(project.root),
            "texture_set": texture_set.to_dict(),
            "authority": hyp.authority.value,
        }

    @mcp.tool(name="vision.retopologize")
    def vision_retopologize(
        project_path: str,
        source_blend: str,
        object_name: str,
        remesh: dict[str, Any] | None = None,
        export_glb: bool = True,
    ) -> dict[str, Any]:
        """Retopologize via real Blender; blocked state is reported if Blender cannot run."""
        project = open_project(project_path)
        out = _v2_project_dir(project, "organic", "retopo", object_name)
        try:
            result = TopologyService().process(
                Path(source_blend),
                object_name,
                output_dir=out,
                remesh=remesh or {"mode": "voxel", "voxel_size": 0.01},
                unwrap=None,
                lods=None,
                export_glb=export_glb,
            )
        except BackendUnavailable as error:
            return _blocked(str(error), project=str(project.root), operation="vision.retopologize")
        except Exception as error:  # noqa: BLE001
            return _blocked(
                f"{type(error).__name__}: {error}",
                project=str(project.root),
                operation="vision.retopologize",
                note="Python/runtime failure, not attributed to hardware",
            )
        return {
            "project": str(project.root),
            "source": result["source"].to_dict(),
            "retopologized": (
                result["retopologized"].to_dict() if result["retopologized"] else None
            ),
            "blend_path": str(result["blend_path"]),
            "glb_path": str(result["glb_path"]) if result["glb_path"] else None,
            "report_path": str(result["report_path"]),
        }

    @mcp.tool(name="vision.generate_uvs")
    def vision_generate_uvs(
        project_path: str,
        source_blend: str,
        object_name: str,
        unwrap: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Generate UVs in Blender; return measured quality or a blocked state."""
        project = open_project(project_path)
        out = _v2_project_dir(project, "organic", "uvs", object_name)
        try:
            result = TopologyService().process(
                Path(source_blend),
                object_name,
                output_dir=out,
                remesh=None,
                unwrap=unwrap or {"method": "smart_project", "angle_limit": 66.0},
                lods=None,
                export_glb=False,
            )
        except BackendUnavailable as error:
            return _blocked(str(error), project=str(project.root), operation="vision.generate_uvs")
        except Exception as error:  # noqa: BLE001
            return _blocked(
                f"{type(error).__name__}: {error}",
                project=str(project.root),
                operation="vision.generate_uvs",
                note="Python/runtime failure, not attributed to hardware",
            )
        return {
            "project": str(project.root),
            "uv": result["uv"].to_dict() if result["uv"] else None,
            "blend_path": str(result["blend_path"]),
            "report_path": str(result["report_path"]),
        }

    @mcp.tool(name="vision.generate_lods")
    def vision_generate_lods(
        project_path: str,
        mesh_path: str,
        budgets: list[dict[str, Any]],
        min_silhouette_iou: float = 0.85,
        allow_python_fallback: bool = True,
    ) -> dict[str, Any]:
        """Generate measured LODs; report blender_used=false when Blender is blocked."""
        project = open_project(project_path)
        out = _v2_project_dir(project, "delivery", "lods")
        try:
            report = delivery_generate_lods(
                Path(mesh_path),
                [LodBudget(**item) if "name" in item else item for item in budgets],
                out,
                min_silhouette_iou=min_silhouette_iou,
                allow_python_fallback=allow_python_fallback,
            )
        except BackendUnavailable as error:
            return _blocked(str(error), project=str(project.root), operation="vision.generate_lods")
        except Exception as error:  # noqa: BLE001
            return _blocked(
                f"{type(error).__name__}: {error}",
                project=str(project.root),
                operation="vision.generate_lods",
                note="Python/runtime failure, not attributed to hardware",
            )
        return {"project": str(project.root), "report": report.to_dict()}

    @mcp.tool(name="vision.generate_fur")
    def vision_generate_fur(
        project_path: str,
        source_blend: str,
        object_name: str,
        parameters: dict[str, Any] | None = None,
        seed: int = 20260726,
    ) -> dict[str, Any]:
        """Groom fur in Blender; returns measured groom report or an explicit blocked state."""
        project = open_project(project_path)
        out = _v2_project_dir(project, "grooming", object_name)
        params = GroomParameters(**parameters) if parameters else GroomParameters()
        try:
            result = FurGroomer().groom(
                Path(source_blend),
                object_name,
                out,
                parameters=params,
                seed=seed,
            )
        except BackendUnavailable as error:
            return _blocked(str(error), project=str(project.root), operation="vision.generate_fur")
        except Exception as error:  # noqa: BLE001
            return _blocked(
                f"{type(error).__name__}: {error}",
                project=str(project.root),
                operation="vision.generate_fur",
                note="Python/runtime failure, not attributed to hardware",
            )
        return {"project": str(project.root), "groom": result.to_dict()}

    @mcp.tool(name="vision.compose_camera_path")
    def vision_compose_camera_path(
        project_path: str,
        path_id: str,
        control_points: list[list[float]] | None = None,
        beats: list[dict[str, Any]] | None = None,
        flagship: bool = False,
        focus_targets: list[dict[str, Any]] | None = None,
        focal_length_mm: list[list[float]] | None = None,
        exposure_curve: list[list[float]] | None = None,
        solids: list[dict[str, Any]] | None = None,
        input_authorities: list[str] | None = None,
        damping: float = 0.12,
        sample_count: int = 256,
    ) -> dict[str, Any]:
        """Compose a sealed CameraPathGraph; authority is derive() of declared inputs."""
        project = open_project(project_path)
        if flagship or (control_points is None and beats is None):
            graph = compose_flagship_datacentre_path(path_id=path_id)
        else:
            if control_points is None or beats is None:
                raise ValidationError(
                    "compose_camera_path requires control_points and beats unless flagship=True"
                )
            solid_objs = [
                SolidGeometry(
                    name=str(item["name"]),
                    minimum=tuple(item["minimum"]),
                    maximum=tuple(item["maximum"]),
                )
                for item in (solids or [])
            ]
            graph = compose_camera_path(
                path_id=path_id,
                control_points=control_points,
                focus_targets=focus_targets,
                focal_length_mm=focal_length_mm,
                exposure_curve=exposure_curve,
                beats=[NarrativeBeat.from_dict(item) for item in beats],
                solids=solid_objs,
                damping=damping,
                sample_count=sample_count,
                input_authorities=input_authorities
                or [AuthorityClass.PROCEDURAL_GROUND_TRUTH.value],
            )
        out = _v2_project_dir(project, "cinematic")
        path = out / f"{graph.id}.json"
        path.write_text(
            json.dumps(graph.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "record": graph.to_dict(),
            "path": str(path),
        }

    @mcp.tool(name="vision.compile_cinematic_scene")
    def vision_compile_cinematic_scene(
        project_path: str,
        path_id: str = "flagship-datacentre-path",
        camera_path: dict[str, Any] | None = None,
        bake_blender: bool = False,
        sample_rate_hz: float = 30.0,
        duration_s: float = 20.0,
    ) -> dict[str, Any]:
        """Export browser motion table from a CameraPathGraph; optional Blender bake is honest."""
        from blender_vision.v2.records import CameraPathGraph

        project = open_project(project_path)
        if camera_path is not None:
            graph = CameraPathGraph.from_dict(camera_path)
            if not graph.digest:
                graph.seal()
        else:
            graph = compose_flagship_datacentre_path(path_id=path_id)
        out = _v2_project_dir(project, "cinematic", graph.id)
        motion = export_motion_table(
            graph,
            out / "motion-table.json",
            sample_rate_hz=sample_rate_hz,
            duration_s=duration_s,
        )
        path_file = out / "camera_path_graph.json"
        path_file.write_text(
            json.dumps(graph.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        payload: dict[str, Any] = {
            "project": str(project.root),
            "record": graph.to_dict(),
            "motion_table": motion,
            "path": str(path_file),
        }
        if bake_blender:
            try:
                payload["blender_bake"] = bake_blender_camera(graph, out / "camera-bake.blend")
            except BackendUnavailable as error:
                payload["blender_bake"] = _blocked(str(error))
            except Exception as error:  # noqa: BLE001
                payload["blender_bake"] = _blocked(
                    f"{type(error).__name__}: {error}",
                    note="Python/runtime failure, not attributed to hardware",
                )
        return payload

    @mcp.tool(name="vision.compile_web_scene")
    def vision_compile_web_scene(
        project_path: str,
        manifest_id: str,
        source_scene: str,
        assets: list[dict[str, Any]],
        camera_path: dict[str, Any] | None = None,
        measure_compression: bool = False,
        initial_js_compressed_bytes: int | None = None,
        input_authorities: list[str] | None = None,
    ) -> dict[str, Any]:
        """Seal a DeliveryManifest with measured budgets; violations are recorded, never raised."""
        from blender_vision.v2.records import CameraPathGraph

        project = open_project(project_path)
        asset_objs = [DeliveryAsset.from_dict(item) for item in assets]
        streaming = None
        if camera_path is not None:
            graph = CameraPathGraph.from_dict(camera_path)
            streaming = build_streaming_plan(graph)
        compression_selections = []
        if measure_compression:
            for asset in asset_objs:
                asset_path = Path(asset.path)
                if asset_path.is_file():
                    compression_selections.append(
                        measure_and_select_compression(asset_path, asset_id=asset.asset_id)
                    )
        record = build_delivery_manifest(
            manifest_id=manifest_id,
            source_scene=source_scene,
            assets=asset_objs,
            compression_selections=compression_selections or None,
            streaming_plan=streaming,
            initial_js_compressed_bytes=initial_js_compressed_bytes,
            input_authorities=input_authorities,
        )
        out = _v2_project_dir(project, "delivery")
        path = out / f"{record.id}.json"
        path.write_text(
            json.dumps(record.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "record": record.to_dict(),
            "streaming_plan": streaming.to_dict() if streaming else None,
            "path": str(path),
        }

    @mcp.tool(name="vision.stream_scene_assets")
    def vision_stream_scene_assets(
        project_path: str,
        camera_path: dict[str, Any] | None = None,
        path_id: str = "flagship-datacentre-path",
        asset_ids_by_role: dict[str, list[str]] | None = None,
    ) -> dict[str, Any]:
        """Build a scroll-gated streaming plan (poster→shell→detail→prefetch→terminal)."""
        from blender_vision.v2.records import CameraPathGraph

        project = open_project(project_path)
        if camera_path is not None:
            graph = CameraPathGraph.from_dict(camera_path)
        else:
            graph = compose_flagship_datacentre_path(path_id=path_id)
        plan = build_streaming_plan(graph, asset_ids_by_role=asset_ids_by_role)
        return {
            "project": str(project.root),
            "path_id": graph.id,
            "path_digest": graph.digest,
            "plan": plan.to_dict(),
        }

    @mcp.tool(name="vision.run_perceptual_critics")
    def vision_run_perceptual_critics(
        project_path: str,
        subject: dict[str, Any],
        evidence_references: list[str],
        evidence_payloads: dict[str, Any] | None = None,
        roles: list[str] | None = None,
        input_authorities: list[str] | None = None,
        critique_id: str | None = None,
    ) -> dict[str, Any]:
        """Run specialist critics and seal a PerceptualCritique with measured findings."""
        project = open_project(project_path)
        critique_subject = CritiqueSubject(
            subject_id=str(subject["subject_id"]),
            kind=str(subject.get("kind", "scene")),
            metrics=dict(subject.get("metrics") or {}),
            media=dict(subject.get("media") or {}),
            tags=frozenset(subject.get("tags") or []),
        )
        evidence = CritiqueEvidence(
            references=list(evidence_references),
            payloads=dict(evidence_payloads or {}),
        )
        workspace = CriticWorkspace()
        if roles:
            record = workspace.run_roles(critique_subject, evidence, roles)
        else:
            record = workspace.run(
                critique_subject,
                evidence,
                critique_id=critique_id,
                input_authorities=input_authorities,
            )
        out = _v2_project_dir(project, "critics")
        path = out / f"{record.id}.json"
        path.write_text(
            json.dumps(record.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {"project": str(project.root), "record": record.to_dict(), "path": str(path)}

    @mcp.tool(name="vision.benchmark_target")
    def vision_benchmark_target(
        project_path: str,
        output_subdir: str = "benchmarks",
        case_ids: list[str] | None = None,
        manifest_path: str | None = None,
    ) -> dict[str, Any]:
        """Run the owned application benchmark; external gates report blocked rather than pass."""
        project = open_project(project_path)
        out = _v2_project_dir(project, output_subdir)
        runner_kwargs: dict[str, Any] = {}
        if manifest_path is not None:
            runner_kwargs["manifest_path"] = Path(manifest_path)
        try:
            runner = ApplicationBenchmarkRunner(**runner_kwargs)
            receipt = runner.run(
                out,
                case_ids=set(case_ids) if case_ids is not None else None,
            )
        except Exception as error:  # noqa: BLE001 — benchmark may require node/npm/docker
            return _blocked(
                f"{type(error).__name__}: {error}",
                project=str(project.root),
                operation="vision.benchmark_target",
                note="Benchmark dependency or environment failure; not reclassified as hardware",
            )
        payload = receipt.model_dump() if hasattr(receipt, "model_dump") else dict(receipt)
        return {"project": str(project.root), "receipt": payload, "output": str(out)}

    @mcp.tool(name="vision.promote_candidate")
    def vision_promote_candidate(
        project_path: str,
        record: dict[str, Any],
        target_authority: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Promote a V2 record via V2Record.promote(); refuses system/auto reviewers."""
        project = open_project(project_path)
        try:
            candidate = load_record(record)
            if record.get("digest"):
                candidate.digest = str(record["digest"])
                candidate.verify()
            promoted = candidate.promote(
                AuthorityClass(target_authority),
                reviewer=reviewer,
                reason=reason,
            )
        except AuthorityPromotionError as error:
            return {
                "project": str(project.root),
                "status": "refused",
                "reason": str(error),
                "reviewer": reviewer,
                "target_authority": target_authority,
            }
        out = _v2_project_dir(project, "promotions")
        path = out / f"{promoted.id}.json"
        path.write_text(
            json.dumps(promoted.to_dict(), indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        return {
            "project": str(project.root),
            "status": "promoted",
            "record": promoted.to_dict(),
            "path": str(path),
        }

    # ------------------------------------------------------------------
    # Ocular OS surface (V2.1 continuous perception loop)
    # Additive only: existing V1/V2 tool names above stay untouched.
    # ------------------------------------------------------------------

    _ocular_gaze: dict[str, GazeController] = {}
    _ocular_trackers: dict[str, TrackerState] = {}
    _ocular_worlds: dict[str, WorldState] = {}

    def _ocular_gaze_for(stream_id: str) -> GazeController:
        key = stream_id or "default"
        controller = _ocular_gaze.get(key)
        if controller is None:
            controller = GazeController(stream_id=key)
            _ocular_gaze[key] = controller
        return controller

    def _detection_from_dict(payload: dict[str, Any]) -> Detection:
        kind_raw = payload.get("kind", TrackTargetKind.OBJECT.value)
        bbox = payload.get("bbox_xywh") or payload.get("bbox") or [0.0, 0.0, 1.0, 1.0]
        if len(bbox) < 4:
            raise ValidationError("detection.bbox_xywh must be [x, y, w, h]")
        cx = payload.get("centroid_xy")
        if cx is None:
            centroid = (
                float(bbox[0]) + float(bbox[2]) / 2.0,
                float(bbox[1]) + float(bbox[3]) / 2.0,
            )
        else:
            centroid = (float(cx[0]), float(cx[1]))
        hist = [float(v) for v in (payload.get("appearance_hist") or [1.0])]
        return Detection(
            detection_id=str(payload.get("detection_id") or payload.get("id") or "det-0"),
            kind=TrackTargetKind(str(kind_raw)),
            bbox_xywh=(float(bbox[0]), float(bbox[1]), float(bbox[2]), float(bbox[3])),
            centroid_xy=centroid,
            appearance_hist=hist,
            area_px=float(payload.get("area_px", float(bbox[2]) * float(bbox[3]))),
            frame_index=int(payload.get("frame_index", 0)),
            conf=float(payload.get("conf", 1.0)),
            meta=dict(payload.get("meta") or {}),
        )

    def _tracker_to_dict(state: TrackerState) -> dict[str, Any]:
        return {
            "tracks": [track.to_dict() for track in state.tracks],
            "filters": {tid: filt.to_dict() for tid, filt in state.filters.items()},
            "next_id": state.next_id,
            "frame_index": state.frame_index,
        }

    def _tracker_from_dict(payload: dict[str, Any] | None) -> TrackerState | None:
        if not payload:
            return None
        tracks = [
            VisualTrack.from_dict(item) if isinstance(item, dict) else item
            for item in (payload.get("tracks") or [])
        ]
        filters: dict[str, KalmanCV2] = {}
        for tid, raw in (payload.get("filters") or {}).items():
            if isinstance(raw, dict):
                filters[str(tid)] = KalmanCV2(
                    x=float(raw.get("x", 0.0)),
                    y=float(raw.get("y", 0.0)),
                    vx=float(raw.get("vx", 0.0)),
                    vy=float(raw.get("vy", 0.0)),
                    p_pos=float(raw.get("p_pos", 25.0)),
                    p_vel=float(raw.get("p_vel", 50.0)),
                )
        return TrackerState(
            tracks=list(tracks),
            filters=filters,
            next_id=int(payload.get("next_id", 1)),
            frame_index=int(payload.get("frame_index", -1)),
        )

    def _world_from_payload(payload: dict[str, Any] | str) -> WorldState:
        if isinstance(payload, str):
            if payload not in _ocular_worlds:
                raise ValidationError(f"unknown world_id: {payload}")
            return _ocular_worlds[payload]
        world = WorldState.from_dict(payload)
        # Cache by sealed id so subsequent tools can pass world_id alone.
        if world.id:
            _ocular_worlds[world.id] = world
        return world

    def _cache_world(world: WorldState) -> WorldState:
        sealed = world if world.digest else world.seal()
        _ocular_worlds[sealed.id] = sealed
        return sealed

    def _proposed_view(payload: dict[str, Any]) -> ProposedView:
        return ProposedView(
            view_id=str(payload.get("view_id") or payload.get("id") or "view-0"),
            kind=str(payload.get("kind", "side")),
            regions=list(payload.get("regions") or []),
            signature=str(payload.get("signature") or ""),
            capture_instructions=dict(payload.get("capture_instructions") or {}),
            human_instructions=str(payload.get("human_instructions") or ""),
            required_calibration=list(payload.get("required_calibration") or []),
            acceptable_alternatives=list(payload.get("acceptable_alternatives") or []),
            reason=str(payload.get("reason") or ""),
            covers_scale_reference=bool(payload.get("covers_scale_reference", False)),
            covers_diffuse_light=bool(payload.get("covers_diffuse_light", False)),
            covers_grazing_light=bool(payload.get("covers_grazing_light", False)),
            covers_lens_metadata=bool(payload.get("covers_lens_metadata", False)),
            covers_calibration_target=bool(payload.get("covers_calibration_target", False)),
        )

    @mcp.tool(name="vision.open_stream")
    def vision_open_stream(
        source: str,
        source_type: str = "video_file",
        stream_id: str | None = None,
        sensor_id: str | None = None,
        buffer_size: int = 32,
        allow_webcam: bool = False,
        webcam_index: int = 0,
        frame_rate: float = 30.0,
        calibration_receipt: str = "",
    ) -> dict[str, Any]:
        """Open a calibrated ocular stream (video, sequence, screen, render, or opt-in webcam).

        Webcam opens require allow_webcam=True. Missing hardware returns BLOCKED
        with an attestation — live frames are never fabricated.
        """
        _handle, _attestation, status = open_stream_or_attest(
            source,
            source_type=source_type,
            stream_id=stream_id,
            sensor_id=sensor_id,
            buffer_size=buffer_size,
            allow_webcam=allow_webcam,
            webcam_index=webcam_index,
            frame_rate=frame_rate,
            calibration_receipt=calibration_receipt,
        )
        if status.get("status") == "open" and status.get("stream_id"):
            sid = str(status["stream_id"])
            _ocular_gaze[sid] = GazeController(stream_id=sid)
            _ocular_trackers[sid] = TrackerState()
        return status

    @mcp.tool(name="vision.close_stream")
    def vision_close_stream(stream_id: str) -> dict[str, Any]:
        """Close an open ocular stream and release capture resources."""
        state = ocular_close_stream(stream_id)
        _ocular_gaze.pop(stream_id, None)
        _ocular_trackers.pop(stream_id, None)
        return {"status": "closed", **state}

    @mcp.tool(name="vision.get_stream_state")
    def vision_get_stream_state(stream_id: str) -> dict[str, Any]:
        """Return buffer occupancy, timestamps, drops, and execution class for a stream."""
        return ocular_get_stream_state(stream_id)

    @mcp.tool(name="vision.calibrate_sensor")
    def vision_calibrate_sensor(
        image_paths: list[str],
        sensor_id: str,
        board_cols: int = 9,
        board_rows: int = 6,
        square_m: float | None = None,
        physical_scale_m: float | None = None,
        timestamps: list[float] | None = None,
        stream_id: str | None = None,
    ) -> dict[str, Any]:
        """Calibrate lens/principal-point from checkerboard images.

        MEASURED authority requires square_m or physical_scale_m. Without a
        physical scale the receipt stays SENSOR_DERIVED / INFERRED.
        """
        calibration = ocular_calibrate_sensor(
            image_paths,
            sensor_id=sensor_id,
            board_size=(int(board_cols), int(board_rows)),
            square_m=square_m,
            physical_scale_m=physical_scale_m,
            timestamps=timestamps,
        )
        sealed = calibration.seal() if not calibration.digest else calibration
        if stream_id:
            try:
                state = ocular_get_stream_state(stream_id)
                state_id = str(state.get("stream_id") or stream_id)
                # Soft-bind calibration receipt id onto stream handle if open.
                from blender_vision.ocular.stream import get_stream

                handle = get_stream(state_id)
                handle.calibration_receipt = sealed.id
                if sealed.camera_matrix:
                    handle._intrinsics = {
                        "camera_matrix": sealed.camera_matrix,
                        "distortion_coefficients": sealed.distortion_coefficients,
                        "principal_point": sealed.principal_point,
                        "image_size": sealed.image_size,
                    }
            except ValidationError:
                pass
        return {
            "status": "calibrated",
            "calibration": sealed.to_dict(),
            "authority": sealed.authority.value
            if hasattr(sealed.authority, "value")
            else str(sealed.authority),
            "method": sealed.method,
            "limitations": list(sealed.limitations),
        }

    @mcp.tool(name="vision.fixate")
    def vision_fixate(
        region: list[float],
        stream_id: str = "",
        reason: str = "salience",
        expected_information: float = 0.5,
        duration_ms: float = 100.0,
        target: str = "",
        frame_id: str = "",
        uncertainty: float = 0.0,
        evidence_changed: bool = False,
        critic_requested: bool = False,
    ) -> dict[str, Any]:
        """Fixate gaze on a region with inhibition-of-return and attention budget."""
        controller = _ocular_gaze_for(stream_id)
        controller.tick()
        fixation = controller.fixate(
            region,
            reason=reason,
            expected_information=expected_information,
            duration_ms=duration_ms,
            target=target,
            frame_id=frame_id,
            uncertainty=uncertainty,
            evidence_changed=evidence_changed,
            critic_requested=critic_requested,
        )
        return {
            "status": "ok",
            "fixation": fixation.to_dict(),
            "outcome": fixation.outcome.value
            if hasattr(fixation.outcome, "value")
            else str(fixation.outcome),
            "stream_id": stream_id,
        }

    @mcp.tool(name="vision.saccade")
    def vision_saccade(
        to_region: list[float],
        stream_id: str = "",
        reason: str = "salience",
        expected_information: float = 0.5,
        from_region: list[float] | None = None,
        uncertainty: float = 0.0,
        evidence_changed: bool = False,
        critic_requested: bool = False,
        duration_ms: float = 40.0,
    ) -> dict[str, Any]:
        """Plan and commit a saccade to a new region (IOR-aware)."""
        controller = _ocular_gaze_for(stream_id)
        controller.tick()
        plan = controller.saccade(
            to_region,
            reason=reason,
            expected_information=expected_information,
            from_region=from_region,
            uncertainty=uncertainty,
            evidence_changed=evidence_changed,
            critic_requested=critic_requested,
            duration_ms=duration_ms,
        )
        return {
            "status": "ok",
            "saccade": plan.to_dict(),
            "inhibited": bool(plan.inhibited),
            "stream_id": stream_id,
        }

    @mcp.tool(name="vision.track")
    def vision_track(
        detections: list[dict[str, Any]],
        tracker_state: dict[str, Any] | None = None,
        frame_index: int | None = None,
        stream_id: str = "",
    ) -> dict[str, Any]:
        """Associate detections to tracks for one frame (IoU + appearance + Kalman).

        Identity is derived from image evidence alone. Pass tracker_state to
        continue a multi-frame association, or stream_id to use the server cache.
        """
        state = _tracker_from_dict(tracker_state)
        if state is None and stream_id and stream_id in _ocular_trackers:
            state = _ocular_trackers[stream_id]
        dets = [_detection_from_dict(item) for item in detections]
        updated = ocular_track(dets, state, frame_index=frame_index)
        if stream_id:
            _ocular_trackers[stream_id] = updated
        return {
            "status": "ok",
            "frame_index": updated.frame_index,
            "n_tracks": len(updated.tracks),
            "tracks": [track.to_dict() for track in updated.tracks],
            "tracker_state": _tracker_to_dict(updated),
            "stream_id": stream_id,
        }

    @mcp.tool(name="vision.reidentify")
    def vision_reidentify(
        detection: dict[str, Any],
        tracks: list[dict[str, Any]] | None = None,
        tracker_state: dict[str, Any] | None = None,
        stream_id: str = "",
        min_score: float | None = None,
        require_lost_or_occluded: bool = True,
    ) -> dict[str, Any]:
        """Attempt re-identification of a detection against lost/occluded tracks.

        Refusal below the appearance threshold is a first-class result, not an error.
        """
        det = _detection_from_dict(detection)
        pool: list[VisualTrack] = []
        if tracks:
            pool = [
                VisualTrack.from_dict(item) if isinstance(item, dict) else item for item in tracks
            ]
        else:
            state = _tracker_from_dict(tracker_state)
            if state is None and stream_id and stream_id in _ocular_trackers:
                state = _ocular_trackers[stream_id]
            if state is not None:
                pool = list(state.tracks)
        decision = ocular_reidentify(
            det,
            pool,
            min_score=min_score,
            require_lost_or_occluded=require_lost_or_occluded,
        )
        return {"status": "ok", "decision": decision.to_dict(), "stream_id": stream_id}

    @mcp.tool(name="vision.observe_change")
    def vision_observe_change(
        prior_world: dict[str, Any],
        current_world: dict[str, Any],
        move_tol_m: float = 0.05,
        lighting_tol: float = 0.08,
    ) -> dict[str, Any]:
        """Classify session-to-session world changes (moved/removed/new/lighting)."""
        prior = _world_from_payload(prior_world)
        current = _world_from_payload(current_world)
        report = ocular_compare_worlds(
            prior,
            current,
            move_tolerance_m=move_tol_m,
            lighting_tolerance=lighting_tol,
        )
        return {"status": "ok", "report": report}

    @mcp.tool(name="vision.build_world_model")
    def vision_build_world_model(
        observations: list[dict[str, Any]],
        scene_id: str,
        session_id: str = "session-0",
    ) -> dict[str, Any]:
        """Build a persistent world model from ordered frame observations.

        Observations must carry perception-derived entity ids (track_id/entity_id)
        with track_source of "perception" or "perception_derived". Sealed
        evaluator ground truth is rejected; allow_ground_truth is not exposed
        over MCP and remains False.
        """
        # allow_ground_truth is intentionally not a tool parameter — over MCP
        # the builder path is always perception-backed.
        world = ocular_build_world_model(
            observations,
            scene_id=scene_id,
            session_id=session_id,
            allow_ground_truth=False,
        )
        sealed = _cache_world(world)
        return {
            "status": "ok",
            "world_id": sealed.id,
            "world": sealed.to_dict(),
            "n_entities": len(sealed.entities),
            "beliefs_digest": sealed.beliefs_digest(),
            "identity_provenance": sealed.meta.get("identity_provenance", "unknown"),
        }

    @mcp.tool(name="vision.update_world_model")
    def vision_update_world_model(
        world: dict[str, Any],
        observation: dict[str, Any],
    ) -> dict[str, Any]:
        """Incorporate one frame observation into an existing world (append-only)."""
        current = _world_from_payload(world)
        updated = ocular_update_world_model(current, observation)
        sealed = _cache_world(updated)
        return {
            "status": "ok",
            "world_id": sealed.id,
            "world": sealed.to_dict(),
            "current_frame": sealed.current_frame,
            "n_entities": len(sealed.entities),
        }

    @mcp.tool(name="vision.query_world")
    def vision_query_world(
        world: dict[str, Any],
        query: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Query entities, relations, or a scene summary from a world model."""
        current = _world_from_payload(world)
        result = ocular_query_world(current, dict(query or {"type": "scene_summary"}))
        return {"status": "ok", "result": result}

    @mcp.tool(name="vision.explain_belief")
    def vision_explain_belief(
        world: dict[str, Any],
        entity_id: str,
        slot: str = "pose",
    ) -> dict[str, Any]:
        """Return the append-only belief history for one entity slot."""
        current = _world_from_payload(world)
        history = ocular_explain_belief(current, entity_id, slot)
        return {
            "status": "ok",
            "entity_id": entity_id,
            "slot": slot,
            "history": history,
            "count": len(history),
        }

    @mcp.tool(name="vision.list_uncertainties")
    def vision_list_uncertainties(world: dict[str, Any]) -> dict[str, Any]:
        """List entities ordered by uncertainty (lowest confidence first)."""
        current = _world_from_payload(world)
        rows = ocular_list_uncertainties(current)
        return {"status": "ok", "uncertainties": rows, "count": len(rows)}

    @mcp.tool(name="vision.predict_next")
    def vision_predict_next(
        world: dict[str, Any],
        frame_index: int | None = None,
        horizon: int = 1,
        pose_tolerance_m: float = 0.05,
        store_on_world: bool = True,
    ) -> dict[str, Any]:
        """Predict pose/visibility/existence for the next horizon frames."""
        current = _world_from_payload(world)
        predictions = ocular_predict_next(
            current,
            frame_index=frame_index,
            horizon=horizon,
            pose_tolerance_m=pose_tolerance_m,
        )
        payloads = [item.to_dict() for item in predictions]
        if store_on_world:
            current.predictions = list(current.predictions) + payloads
            sealed = _cache_world(current)
        else:
            sealed = current
        return {
            "status": "ok",
            "world_id": sealed.id,
            "predictions": payloads,
            "count": len(payloads),
            "world": sealed.to_dict() if store_on_world else None,
        }

    @mcp.tool(name="vision.list_surprises")
    def vision_list_surprises(
        world: dict[str, Any],
        entity_id: str | None = None,
    ) -> dict[str, Any]:
        """List recorded surprise events for a world (optionally filtered)."""
        current = _world_from_payload(world)
        rows = ocular_list_surprises(current, entity_id=entity_id)
        return {"status": "ok", "surprises": rows, "count": len(rows)}

    @mcp.tool(name="vision.measure_information_gain")
    def vision_measure_information_gain(
        target: dict[str, Any],
        view: dict[str, Any],
        gain_threshold: float = 0.02,
    ) -> dict[str, Any]:
        """Estimate expected information gain for a proposed view on a target."""
        perception_target = _perception_target(target)
        proposed = _proposed_view(view)
        estimate = estimate_information_gain(
            perception_target,
            proposed,
            gain_threshold=gain_threshold,
        )
        return {
            "status": "ok",
            "estimate": estimate.to_dict(),
            "expected_reduction": estimate.expected_reduction,
            "redundant": estimate.redundant,
            "reason": estimate.reason,
        }

    return mcp


def run_server(projects_root: Path | None = None, *, transport: str = "stdio") -> None:
    create_server(projects_root).run(transport=transport)


def main() -> None:
    configured = os.environ.get("BVMCP_PROJECTS_ROOT")
    run_server(Path(configured) if configured else None)


if __name__ == "__main__":
    main()
