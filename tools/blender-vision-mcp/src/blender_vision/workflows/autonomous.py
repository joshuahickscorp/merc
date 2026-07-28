from __future__ import annotations

from pathlib import Path
from typing import Any

from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import SemanticTwinGraph
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.progress import WorkflowProgressReporter


def reconstruct_from_public_evidence(
    project: ProjectStore,
    *,
    target: str | dict[str, Any],
    requested_tier: str,
    configuration: str,
    evidence_policy: str,
    existing_model: Path | None = None,
    sources: list[dict[str, Any]] | None = None,
    resource_profile: str = "auto",
    request_class: str = "AUTONOMOUS_PUBLIC_EVIDENCE",
    category_override: str | None = None,
) -> dict[str, Any]:
    resolution = TargetResolver(project).resolve(
        target,
        requested_tier=requested_tier,
        configuration=configuration,
        request_class=request_class,
    )
    pack = (
        CategoryPackRegistry().get(category_override)
        if category_override
        else CategoryPackRegistry().select(resolution["target"])
    )
    acquisition = EvidenceAcquisitionStore(project)
    acquired = []
    for item in sources or []:
        record = acquisition.register_source(
            resolution["id"],
            item["source"],
            rights=item["rights"],
            reviewed_by=item.get("reviewed_by"),
        )
        if item.get("local_path"):
            record = acquisition.acquire_local(record["id"], Path(item["local_path"]))
        acquired.append(record)
    scene = SceneStore(project).import_blend(existing_model) if existing_model else None
    semantic_graph = SemanticTwinGraph(project).bootstrap(
        category=pack["id"], target_id=resolution["id"]
    )
    surface_atlas = SurfaceCoverageAtlas(project).bootstrap(
        target_id=resolution["id"], regions=pack["ontology"]
    )
    portfolio = ReconstructionPortfolioStore(project).generate(
        category=pack["id"], resource_profile=resource_profile
    )
    campaign = CampaignStore(project).start(
        "public_evidence_reconstruction",
        configuration={
            "target_id": resolution["id"],
            "requested_tier": requested_tier,
            "configuration": configuration,
            "evidence_policy": evidence_policy,
            "category": pack["id"],
            "portfolio_id": portfolio["id"],
            "semantic_root_id": semantic_graph["root_id"],
            "existing_scene_id": scene["id"] if scene else None,
        },
        resource_profile=resource_profile,
    )
    campaign_store = CampaignStore(project)
    progress_records = [
        ("Target resolved", {"target_id": resolution["id"], "status": resolution["status"]}),
        ("Category intelligence selected", {"category": pack["id"]}),
        ("Rights ledger initialized", {"registered_sources": len(acquired)}),
        (
            "Surface atlas initialized",
            {"surface_cell_count": surface_atlas["cell_count"]},
        ),
        (
            "Candidate portfolio initialized",
            {"portfolio_id": portfolio["id"], "candidate_count": len(portfolio["candidates"])},
        ),
    ]
    for message, details in progress_records:
        campaign = campaign_store.progress(campaign["id"], message=message, details=details)
    if resolution["status"] == "NEEDS_CLARIFICATION":
        campaign = campaign_store.pause(
            campaign["id"], reason="material target ambiguity requires one clarification"
        )
    audit = acquisition.audit(resolution["id"])
    coverage = acquisition.analyze_coverage(resolution["id"])
    with project.connection() as connection:
        authoritative_dimensions = connection.execute(
            "SELECT COUNT(DISTINCT json_extract(value_json,'$.axis')) FROM measurements "
            "WHERE type='known_overall_dimension' AND evidence_class IN ('MEASURED',"
            "'MANUFACTURER_SPEC')"
        ).fetchone()[0]
    if request_class == "GENERATIVE_DESIGN":
        supported_ceiling = "L0"
    elif authoritative_dimensions >= 3 and acquired:
        supported_ceiling = "L3"
    elif acquired and coverage["directional_coverage"] >= 0.5:
        supported_ceiling = "L2"
    elif acquired:
        supported_ceiling = "L1"
    else:
        supported_ceiling = "L0"
    result = {
        "workflow": "workflow.reconstruct_from_public_evidence",
        "project": str(project.root),
        "target_resolution": resolution,
        "category_pack": pack,
        "search_plan": acquisition.plan_search(resolution["id"], category=pack["id"]),
        "acquired_sources": acquired,
        "rights_audit": audit,
        "coverage": coverage,
        "surface_atlas": surface_atlas,
        "existing_scene": scene,
        "semantic_graph": semantic_graph,
        "portfolio": portfolio,
        "campaign": campaign,
        "requested_tier": requested_tier,
        "current_evidence_ceiling": supported_ceiling,
        "current_status": (
            "TARGET_CLARIFICATION_REQUIRED"
            if resolution["status"] == "NEEDS_CLARIFICATION"
            else "READY_FOR_GENERATIVE_PROPOSALS"
            if request_class == "GENERATIVE_DESIGN"
            else "EVIDENCE_ACQUISITION_REQUIRED"
            if not acquired
            else "READY_FOR_CAMERA_AND_CANDIDATE_EXECUTION"
        ),
        "accuracy_statement": (
            "Generated design: no measured-target fidelity is claimed."
            if request_class == "GENERATIVE_DESIGN"
            else "No one-to-one claim is made. The final receipt will distinguish observed, "
            "measured, inferred, and unseen geometry."
        ),
    }
    result["progress"] = WorkflowProgressReporter(project).report(campaign["id"])
    return result


def reconstruct_from_user_capture(
    project: ProjectStore,
    *,
    target: str,
    reference_paths: list[Path],
    requested_tier: str = "L3",
    configuration: str = "as captured",
    category: str | None = None,
    resource_profile: str = "auto",
) -> dict[str, Any]:
    sources = [
        {
            "source": {
                "origin": str(path),
                "publisher": "project owner",
                "page_title": path.name,
                "authority_class": "user_owned",
                "target_variant": {},
                "viewpoint": path.stem,
                "quality_score": 1.0,
            },
            "rights": {
                "status": "USER_OWNED",
                "internal_use": True,
                "redistribution": True,
            },
            "reviewed_by": "project owner",
            "local_path": str(path),
        }
        for path in reference_paths
    ]
    return reconstruct_from_public_evidence(
        project,
        target=target,
        requested_tier=requested_tier,
        configuration=configuration,
        evidence_policy="user_owned",
        sources=sources,
        resource_profile=resource_profile,
        request_class="REFERENCE_RECONSTRUCTION",
        category_override=category,
    )


def generate_original_asset(
    project: ProjectStore,
    *,
    description: str,
    category: str = "organic_creatures",
    resource_profile: str = "auto",
) -> dict[str, Any]:
    return reconstruct_from_public_evidence(
        project,
        target={"manufacturer": "original", "model": description},
        requested_tier="L0",
        configuration="original generated design",
        evidence_policy="generated_owned_outputs",
        resource_profile=resource_profile,
        request_class="GENERATIVE_DESIGN",
        category_override=category,
    )
