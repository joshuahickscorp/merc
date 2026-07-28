from __future__ import annotations

from pathlib import Path

import pytest
from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import EvidenceClass, RegistrationClass
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.portfolio_executor import PortfolioExecutor
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.base import GeometryEvidence
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.vision.store import GeometryEvidenceStore
from blender_vision.visual.oracle import VisualOracleStore


def test_configured_learned_lane_executes_governed_geometry_backend(
    tmp_path: Path, monkeypatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Configured learned lane")
    confidence_path = tmp_path / "confidence.bin"
    confidence_path.write_bytes(b"fixture-confidence")
    confidence = ArtifactStore(project).ingest_file(confidence_path)
    run = GeometryEvidenceStore(project).create(
        "vggt-original-research",
        "fixture",
        GeometryEvidence(
            camera_extrinsics=[
                {
                    "reference_id": "fixture-reference",
                    "registration_class": RegistrationClass.APPROXIMATE_VISUAL.value,
                    "confidence": 0.8,
                }
            ],
            confidence_artifacts=[confidence.digest],
            uncertainty={"depth_confidence_mean": 0.8, "scale": "unresolved"},
        ),
        evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
        license_record={"license": "fixture-research", "research_only": True},
        commercial_eligible=False,
    )
    calls: list[tuple[str, dict]] = []

    def execute(_pipeline: GeometryPipeline, backend: str, configuration: dict) -> dict:
        calls.append((backend, configuration))
        return run

    monkeypatch.setattr(GeometryPipeline, "run", execute)
    portfolio = ReconstructionPortfolioStore(project).generate(
        lanes=["learned_multiview_geometry"],
        resource_profile="compact",
        backend_configuration={
            "learned_multiview_geometry": {
                "backend": "vggt-original-research",
                "model_installation_id": "governed-installation",
                "device": "cpu",
            }
        },
    )

    result = PortfolioExecutor(project).execute_initial(portfolio["id"])
    candidate = ReconstructionPortfolioStore(project).list_candidates(portfolio["id"])[0]

    assert calls == [
        (
            "vggt-original-research",
            {
                "backend": "vggt-original-research",
                "model_installation_id": "governed-installation",
                "device": "cpu",
            },
        )
    ]
    assert candidate["status"] == "EVALUATED"
    assert candidate["geometry_run_id"] == run["id"]
    assert candidate["evidence_authority"] == "observed_initialization"
    assert candidate["acceptance_eligible"] is False
    assert candidate["metrics"]["learned_confidence"] == 0.8
    assert candidate["artifacts"] == [confidence.digest]
    assert result["acceptance_performed"] is False


def test_portfolio_backend_configuration_is_scoped_and_geometry_bound(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Portfolio boundaries")
    store = ReconstructionPortfolioStore(project)
    with pytest.raises(ValueError, match="unselected lanes"):
        store.generate(
            lanes=["visual_hull"],
            backend_configuration={"learned_multiview_geometry": {}},
        )
    portfolio = store.generate(lanes=["learned_multiview_geometry"])
    candidate = portfolio["candidates"][0]
    with pytest.raises(ValueError, match="unknown geometry run"):
        store.record_result(
            candidate["id"],
            metrics={"camera": 0.5},
            geometry_run_id="missing-run",
        )


def test_imported_generative_and_gaussian_results_remain_non_authoritative(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Proposal portfolio lanes")
    image_path = tmp_path / "reference.png"
    Image.new("RGB", (64, 48), "gray").save(image_path)
    reference = ReferenceIngestor(project).import_file(
        image_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    camera = CameraSolver(project).solve("turntable_fallback")

    mesh_path = tmp_path / "proposal.glb"
    mesh_path.write_bytes(b"generated-proposal")
    mesh = ArtifactStore(project).ingest_file(mesh_path, media_type="model/gltf-binary")
    proposals = GenerativeProposalStore(project)
    request = proposals.request(
        "generate_shape",
        backend="fixture-generator",
        inputs={"reference_ids": [reference["id"]]},
        checkpoint="fixture-generator-v1",
        license_record={"license": "fixture", "commercial_use": True},
    )
    proposal = proposals.import_result(
        request["id"],
        mesh_digests=[mesh.digest],
        texture_digests=[],
        image_digests=[],
        pbr_channels={},
        backend_identity="fixture-generator",
        checkpoint="fixture-generator-v1",
        input_reference_ids=[reference["id"]],
        generation_seed=7,
        confidence=0.6,
        known_limitations=["hidden surfaces inferred"],
    )

    oracle_path = project.root / "oracle.splat"
    oracle_path.write_bytes(b"gaussian-oracle")
    oracle = VisualOracleStore(project).register(
        oracle_path,
        kind="gaussian_splat",
        camera_solution_ids=[camera["id"]],
        training_configuration={"iterations": 10},
        license_record={"license": "fixture", "commercial_use": True},
    )
    portfolio = ReconstructionPortfolioStore(project).generate(
        lanes=["generative_image_to_3d", "gaussian_visual_oracle"],
        backend_configuration={
            "generative_image_to_3d": {"request_id": request["id"]},
            "gaussian_visual_oracle": {"oracle_id": oracle["id"]},
        },
    )

    result = PortfolioExecutor(project).execute_initial(portfolio["id"])
    candidates = {
        item["lane"]: item
        for item in ReconstructionPortfolioStore(project).list_candidates(portfolio["id"])
    }
    generated = candidates["generative_image_to_3d"]
    gaussian = candidates["gaussian_visual_oracle"]

    assert generated["status"] == "EVALUATED"
    assert generated["artifacts"] == sorted([mesh.digest, proposal["artifact"]["digest"]])
    assert generated["evidence_authority"] == "synthetic_hypothesis"
    assert generated["acceptance_eligible"] is False
    assert gaussian["status"] == "EVALUATED"
    assert gaussian["artifacts"] == [oracle["artifact_digest"]]
    assert gaussian["evidence_authority"] == "appearance_oracle_only"
    assert gaussian["acceptance_eligible"] is False
    assert result["fusion_plan"]["generated_inputs_labeled_as_inference"] is True
