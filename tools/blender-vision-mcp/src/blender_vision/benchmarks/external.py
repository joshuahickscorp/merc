from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from blender_vision.core.models import EvidenceClass, FidelityLevel
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.coverage_atlas import SurfaceCoverageAtlas
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.semantic_graph import SemanticTwinGraph
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore


def load_external_manifest(repository_root: Path) -> dict[str, Any]:
    repository_root = repository_root.expanduser().resolve()
    candidates = [
        repository_root / "tools" / "blender-vision-mcp" / "benchmarks"
        / "perseverance" / "benchmark.json",
        repository_root / "benchmarks" / "perseverance" / "benchmark.json",
    ]
    for path in candidates:
        if path.is_file():
            return json.loads(path.read_text(encoding="utf-8"))
    raise FileNotFoundError(f"external benchmark manifest is missing: {candidates}")


def bootstrap_external_benchmark(
    project_root: Path,
    repository_root: Path,
    *,
    reviewed_by: str,
    resource_profile: str = "auto",
) -> dict[str, Any]:
    """Bootstrap Stage 4 from governed public facts without importing a starting model."""
    if not reviewed_by.strip():
        raise ValueError("external benchmark bootstrap requires a named evidence reviewer")
    manifest = load_external_manifest(repository_root)
    project = ProjectStore.create(
        project_root,
        manifest["display_name"],
        target_fidelity=FidelityLevel(manifest["target_fidelity"]),
        metadata={
            "benchmark": manifest["benchmark"],
            "benchmark_schema_version": manifest["schema_version"],
            "benchmark_origin": "external_public",
            "private_starting_model": False,
            "dimension_scope": manifest["dimension_scope"],
            "release_policy": (
                "derived model, citations, receipts, and owned renders only unless each NASA "
                "media asset passes its final per-asset rights review"
            ),
        },
    )
    target = TargetResolver(project).resolve(
        manifest["target"],
        requested_tier=manifest["target_fidelity"],
        configuration=manifest["target"]["configuration"],
        request_class="AUTONOMOUS_PUBLIC_EVIDENCE",
    )
    acquisition = EvidenceAcquisitionStore(project)
    sources = []
    for definition in manifest["sources"]:
        source = acquisition.register_source(
            target["id"],
            {key: value for key, value in definition.items() if key != "rights"},
            rights=definition["rights"],
        )
        sources.append(
            acquisition.review_governance(
                source["id"],
                reviewed_by=reviewed_by,
                source_terms_review="approved",
                privacy_review="not_applicable",
            )
        )
    measurements = [
        MeasurementStore(project).add(
            "known_overall_dimension",
            {
                "axis": axis,
                "millimetres": millimetres,
                "source_url": manifest["sources"][0]["origin"],
                "scope": manifest["dimension_scope"],
            },
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            uncertainty={
                "millimetres": 50.0,
                "classification": "official rounded public dimension",
            },
            certainty="bounded",
        )
        for axis, millimetres in manifest["official_dimensions_mm"].items()
    ]
    pack = CategoryPackRegistry().get(manifest["category"])
    semantic = SemanticTwinGraph(project).bootstrap(
        category=pack["id"], target_id=target["id"]
    )
    surface_atlas = SurfaceCoverageAtlas(project).bootstrap(
        target_id=target["id"], regions=pack["ontology"], synchronize=True
    )
    portfolio = ReconstructionPortfolioStore(project).generate(
        category=pack["id"], resource_profile=resource_profile
    )
    campaign = CampaignStore(project).start(
        "external_public_benchmark",
        configuration={
            "target_id": target["id"],
            "requested_tier": manifest["target_fidelity"],
            "private_starting_model": False,
            "source_ids": [item["id"] for item in sources],
            "portfolio_id": portfolio["id"],
            "semantic_root_id": semantic["root_id"],
            "surface_atlas_cell_count": surface_atlas["cell_count"],
        },
        resource_profile=resource_profile,
    )
    return {
        "project": str(project.root),
        "benchmark": manifest["benchmark"],
        "status": "EVIDENCE_ACQUISITION_REQUIRED",
        "target": target,
        "sources": sources,
        "rights_audit": acquisition.audit(target["id"]),
        "measurements": measurements,
        "semantic_graph": semantic,
        "surface_atlas": surface_atlas,
        "portfolio": portfolio,
        "campaign": campaign,
        "next_actions": [
            "acquire and per-asset review reusable multiview stills, video, and official "
            "public 3D landmark reference",
            "extract and rank keyframes",
            "propose image-to-public-model landmarks, obtain named review, solve and "
            "freeze cameras",
            "generate and evaluate editable candidates",
        ],
    }
