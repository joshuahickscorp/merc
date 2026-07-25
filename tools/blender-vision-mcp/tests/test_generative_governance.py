from __future__ import annotations

from pathlib import Path

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def test_generative_backend_result_is_always_a_hypothesis(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Generative")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (32, 32), "gray").save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED"
    )
    mesh_path = tmp_path / "proposal.glb"
    mesh_path.write_bytes(b"proposal")
    mesh = ArtifactStore(project).ingest_file(mesh_path, media_type="model/gltf-binary")
    texture_path = tmp_path / "base-color.png"
    Image.new("RGB", (8, 8), "gray").save(texture_path)
    texture = ArtifactStore(project).ingest_file(texture_path, media_type="image/png")
    store = GenerativeProposalStore(project)
    request = store.request(
        "generate_shape_and_material",
        backend="approved-local-backend",
        inputs={"reference_ids": [reference["id"]], "prompt": "initialize topology"},
        checkpoint="approved-local-backend-v1",
        license_record={"license": "fixture", "commercial_use": True},
    )
    result = store.import_result(
        request["id"],
        mesh_digests=[mesh.digest],
        texture_digests=[texture.digest],
        image_digests=[],
        pbr_channels={"base_color": texture.digest},
        backend_identity="approved-local-backend",
        checkpoint="approved-local-backend-v1",
        input_reference_ids=[reference["id"]],
        generation_seed=42,
        confidence=0.7,
        known_limitations=["hidden surfaces are inferred"],
    )

    assert result["evidence_class"] == "SYNTHETIC_HYPOTHESIS"
    assert result["acceptance_eligible"] is False
    assert result["generation_seed"] == 42
    assert project.status()["counts"]["generative_requests"] == 1
    assert project.status()["counts"]["generative_results"] == 1
