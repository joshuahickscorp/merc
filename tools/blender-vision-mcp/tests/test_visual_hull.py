from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.registry import BackendRegistry
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.errors import EvidenceUnavailable
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.portfolio_executor import PortfolioExecutor
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.vision.pipeline import GeometryPipeline

CAMERA_MATRICES = (
    [
        [1.0, 0.0, 0.0, 0.0],
        [0.0, 0.0, -1.0, -4.0],
        [0.0, 1.0, 0.0, 0.0],
        [0.0, 0.0, 0.0, 1.0],
    ],
    [
        [0.0, 0.0, -1.0, -4.0],
        [-1.0, 0.0, 0.0, 0.0],
        [0.0, 1.0, 0.0, 0.0],
        [0.0, 0.0, 0.0, 1.0],
    ],
    [
        [1.0, 0.0, 0.0, 0.0],
        [0.0, 1.0, 0.0, 0.0],
        [0.0, 0.0, 1.0, 4.0],
        [0.0, 0.0, 0.0, 1.0],
    ],
)


def test_visual_hull_is_discoverable_as_builtin_cpu_backend() -> None:
    capability = next(
        item for item in BackendRegistry().as_dict() if item["name"] == "visual_hull"
    )

    assert capability["state"] == "AVAILABLE"
    assert capability["hardware"] == ["cpu"]
    assert capability["quality_tier"] == "coarse-observed-topology"
    assert capability["input_limits"]["minimum_reviewed_full_object_masks"] == 2


def _governed_visual_hull_project(
    tmp_path: Path,
    *,
    partial_last_mask: bool = False,
    rights_state: str = "SYNTHETIC_OWNED",
) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Governed visual hull")
    references = []
    masks = ReferenceMaskStore(project)
    for index in range(3):
        reference_path = tmp_path / f"reference-{index}.png"
        Image.new("RGB", (64, 64), "gray").save(reference_path)
        reference = ReferenceIngestor(project).import_file(
            reference_path,
            rights_state=rights_state,
            viewpoint_label=f"view-{index}",
        )
        references.append(reference)
        mask_path = tmp_path / f"mask-{index}.png"
        mask = Image.new("L", (64, 64), 0)
        ImageDraw.Draw(mask).rectangle((14, 14, 50, 50), fill=255)
        mask.save(mask_path)
        masks.import_reviewed(
            reference["id"],
            mask_path,
            reviewer="Synthetic fixture reviewer",
            reason="Known full-object silhouette fixture",
            intended_use="visual_hull_reconstruction",
            visible_components=["front_panel"] if partial_last_mask and index == 2 else [],
        )
    solution = CameraSolver(project).import_manual(
        [
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": 64,
                "height": 64,
                "intrinsics": {"fx": 80.0, "fy": 80.0, "cx": 32.0, "cy": 32.0},
                "world_from_camera": CAMERA_MATRICES[index],
                "confidence": 0.8,
                "registration_class": "approximate_visual_registration",
                "evidence_class": "MULTI_VIEW_OBSERVED",
            }
            for index, reference in enumerate(references)
        ]
    )
    return project, solution


def test_visual_hull_carves_content_addressed_editable_mesh(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path)
    run = GeometryPipeline(project).run(
        "visual_hull",
        {
            "camera_solution_id": solution["id"],
            "grid_resolution": 20,
            "minimum_views": 3,
            "bounds": {"minimum": [-1.5, -1.5, -1.5], "maximum": [1.5, 1.5, 1.5]},
        },
    )

    evidence = run["evidence"]
    assert run["backend"] == "visual_hull"
    assert run["evidence_class"] == "MULTI_VIEW_OBSERVED"
    assert run["commercial_eligible"] is True
    assert evidence["scale_factor"] is None
    assert evidence["uncertainty"]["metric_authority"] is False
    assert len(evidence["mask_artifacts"]) == 3
    assert len(evidence["visual_hull_artifacts"]) == 1
    mesh_path = ArtifactStore(project).path_for(evidence["visual_hull_artifacts"][0])
    assert mesh_path.read_text(encoding="utf-8").startswith("ply\nformat ascii 1.0\n")
    report_path = ArtifactStore(project).path_for(evidence["occupancy_artifacts"][0])
    report = json.loads(report_path.read_text(encoding="utf-8"))
    assert 0 < report["voxel_count"] < report["total_voxel_count"]
    assert report["triangle_count"] > 0
    assert report["authority"]["concavity_authority"] is False


def test_visual_hull_rejects_partial_component_masks(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path, partial_last_mask=True)

    with pytest.raises(EvidenceUnavailable, match="reviewed full-object masks"):
        GeometryPipeline(project).run(
            "visual_hull",
            {
                "camera_solution_id": solution["id"],
                "grid_resolution": 16,
                "minimum_views": 3,
            },
        )


def test_visual_hull_rejects_corrupt_reviewed_mask_artifact(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path)
    mask = ReferenceMaskStore(project).list()[0]
    ArtifactStore(project).path_for(mask["artifact_digest"]).write_bytes(b"corrupt")

    with pytest.raises(EvidenceUnavailable, match="reviewed full-object masks"):
        GeometryPipeline(project).run(
            "visual_hull",
            {
                "camera_solution_id": solution["id"],
                "grid_resolution": 16,
                "minimum_views": 3,
            },
        )


def test_visual_hull_preserves_nonredistributable_source_boundary(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path, rights_state="INTERNAL")
    run = GeometryPipeline(project).run(
        "visual_hull",
        {
            "camera_solution_id": solution["id"],
            "grid_resolution": 16,
            "minimum_views": 3,
            "bounds": {"minimum": [-1.5, -1.5, -1.5], "maximum": [1.5, 1.5, 1.5]},
        },
    )

    assert run["commercial_eligible"] is False
    assert run["license"]["commercial_use"] is False
    assert run["license"]["source_rights_states"] == ["INTERNAL"]


def test_portfolio_evaluates_visual_hull_only_after_governed_carving(tmp_path: Path) -> None:
    project, _solution = _governed_visual_hull_project(tmp_path)
    portfolio = ReconstructionPortfolioStore(project).generate(
        lanes=["visual_hull"], resource_profile="compact"
    )

    result = PortfolioExecutor(project).execute_initial(portfolio["id"])
    candidate = ReconstructionPortfolioStore(project).list_candidates(portfolio["id"])[0]

    assert candidate["status"] == "EVALUATED"
    assert candidate["geometry_run_id"]
    assert candidate["metrics"]["camera"] == 0.5
    assert candidate["metrics"]["silhouette"] == 0.0
    assert len(candidate["artifacts"]) == 3
    assert result["acceptance_performed"] is False


def test_visual_hull_job_cache_tracks_reviewed_mask_revisions(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path)
    configuration = {
        "backend": "visual_hull",
        "configuration": {
            "camera_solution_id": solution["id"],
            "grid_resolution": 16,
            "minimum_views": 3,
            "bounds": {"minimum": [-1.5, -1.5, -1.5], "maximum": [1.5, 1.5, 1.5]},
        },
    }
    coordinator = Coordinator(project)
    first = coordinator.run("vision.run", configuration)
    second = coordinator.run("vision.run", configuration)
    assert first["status"] == "succeeded"
    assert first["result"]["cache_hit"] is False
    assert second["result"]["cache_hit"] is True

    reference_id = ReferenceIngestor(project).list()[0]["id"]
    revised_path = tmp_path / "revised-mask.png"
    revised = Image.new("L", (64, 64), 0)
    ImageDraw.Draw(revised).ellipse((13, 13, 51, 51), fill=255)
    revised.save(revised_path)
    ReferenceMaskStore(project).import_reviewed(
        reference_id,
        revised_path,
        reviewer="Synthetic fixture reviewer",
        reason="Intentional governed mask revision",
        intended_use="visual_hull_reconstruction",
    )
    third = coordinator.run("vision.run", configuration)

    assert third["status"] == "succeeded"
    assert third["result"]["cache_hit"] is False
    assert third["result"]["id"] != first["result"]["id"]


def test_receipt_verifies_visual_hull_mesh_and_occupancy_artifacts(tmp_path: Path) -> None:
    project, solution = _governed_visual_hull_project(tmp_path)
    run = GeometryPipeline(project).run(
        "visual_hull",
        {
            "camera_solution_id": solution["id"],
            "grid_resolution": 16,
            "minimum_views": 3,
            "bounds": {"minimum": [-1.5, -1.5, -1.5], "maximum": [1.5, 1.5, 1.5]},
        },
    )
    receipt = export_receipt(project)
    receipt_path = project.root / receipt["path"]
    verification = verify_receipt(receipt_path, project=project)
    assert verification["valid"] is True
    assert receipt["acceptance"]["metrics"]["geometry_evidence"][
        "visual_hull_artifact_count"
    ] == 1
    assert receipt["acceptance"]["metrics"]["geometry_evidence"][
        "occupancy_artifact_count"
    ] == 1

    mesh_digest = run["evidence"]["visual_hull_artifacts"][0]
    ArtifactStore(project).path_for(mesh_digest).write_bytes(b"corrupt mesh")
    verification = verify_receipt(receipt_path, project=project)
    assert verification["valid"] is False
    assert mesh_digest in verification["missing_or_corrupt_artifacts"]
