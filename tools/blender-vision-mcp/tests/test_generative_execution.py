from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.core.errors import ProjectError
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import DistributedScheduler


def _project(tmp_path: Path) -> tuple[ProjectStore, dict, dict[str, str]]:
    project = ProjectStore.create(tmp_path / "project", "Generative execution")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (32, 32), "gray").save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED"
    )
    mesh_path = tmp_path / "proposal.glb"
    mesh_path.write_bytes(b"generated-mesh")
    mesh = ArtifactStore(project).ingest_file(mesh_path, media_type="model/gltf-binary")
    texture_path = tmp_path / "texture.png"
    Image.new("RGB", (8, 8), "white").save(texture_path)
    texture = ArtifactStore(project).ingest_file(texture_path, media_type="image/png")
    view_path = tmp_path / "view.png"
    Image.new("RGB", (16, 16), "black").save(view_path)
    view = ArtifactStore(project).ingest_file(view_path, media_type="image/png")
    return project, reference, {
        "mesh": mesh.digest,
        "texture": texture.digest,
        "view": view.digest,
    }


def _request(
    store: GenerativeProposalStore,
    operation: str,
    *,
    reference_id: str | None = None,
    artifact_digests: list[str] | None = None,
) -> dict:
    return store.request(
        operation,
        backend="fixture-generator@1",
        checkpoint="fixture-checkpoint-sha256",
        license_record={"license": "Apache-2.0", "commercial_use": True},
        backend_configuration={"device": "cuda", "maximum_steps": 20},
        inputs={
            "reference_ids": [reference_id] if reference_id else [],
            "artifact_digests": artifact_digests or [],
            "prompt": f"fixture {operation}",
        },
    )


def _import(
    store: GenerativeProposalStore,
    request: dict,
    artifacts: dict[str, str],
) -> dict:
    outputs = {
        "generate_shape": ([artifacts["mesh"]], [], [], {}),
        "generate_shape_and_material": (
            [artifacts["mesh"]],
            [artifacts["texture"]],
            [],
            {"base_color": artifacts["texture"]},
        ),
        "generate_multiview_images": ([], [], [artifacts["view"]], {}),
        "generate_texture": ([], [artifacts["texture"]], [], {}),
        "retopologize": ([artifacts["mesh"]], [], [], {}),
        "export_candidate": ([artifacts["mesh"]], [], [], {}),
    }
    meshes, textures, images, channels = outputs[request["operation"]]
    return store.import_result(
        request["id"],
        mesh_digests=meshes,
        texture_digests=textures,
        image_digests=images,
        pbr_channels=channels,
        backend_identity=request["backend"],
        checkpoint=request["checkpoint"],
        input_reference_ids=request["inputs"]["reference_ids"],
        generation_seed=17,
        confidence=0.65,
        known_limitations=["hidden geometry is inferred and unverified"],
    )


def test_all_required_generative_operations_have_governed_output_contracts(
    tmp_path: Path,
) -> None:
    project, reference, artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)

    for operation in (
        "generate_shape",
        "generate_shape_and_material",
        "generate_multiview_images",
        "generate_texture",
        "retopologize",
        "export_candidate",
    ):
        request = _request(
            store,
            operation,
            reference_id=reference["id"],
            artifact_digests=(
                [artifacts["mesh"]]
                if operation in {"retopologize", "export_candidate"}
                else []
            ),
        )
        result = _import(store, request, artifacts)
        assert result["operation"] == operation
        assert result["evidence_class"] == "SYNTHETIC_HYPOTHESIS"
        assert result["acceptance_eligible"] is False

    audit = store.audit()
    assert audit["invalid_request_ids"] == []
    assert audit["invalid_result_ids"] == []
    assert len(audit["results"]) == 6


def test_generative_request_is_idempotent_and_rejects_secrets_or_unknown_inputs(
    tmp_path: Path,
) -> None:
    project, reference, _artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)
    first = _request(store, "generate_shape", reference_id=reference["id"])
    repeated = _request(store, "generate_shape", reference_id=reference["id"])

    assert repeated["id"] == first["id"]
    with pytest.raises(ValueError, match="credentials"):
        store.request(
            "generate_shape",
            backend="fixture",
            checkpoint="v1",
            license_record={"license": "fixture"},
            backend_configuration={"api_key": "must-not-persist"},
            inputs={"prompt": "shape"},
        )
    with pytest.raises(ValueError, match="unknown evidence"):
        store.request(
            "generate_shape",
            backend="fixture",
            checkpoint="v1",
            license_record={"license": "fixture"},
            inputs={"reference_ids": ["missing-reference"]},
        )


def test_output_contract_and_backend_lineage_cannot_be_bypassed(tmp_path: Path) -> None:
    project, reference, artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)
    multiview = _request(store, "generate_multiview_images", reference_id=reference["id"])

    with pytest.raises(ValueError, match="requires image artifacts"):
        store.import_result(
            multiview["id"],
            mesh_digests=[artifacts["mesh"]],
            texture_digests=[],
            image_digests=[],
            pbr_channels={},
            backend_identity=multiview["backend"],
            checkpoint=multiview["checkpoint"],
            input_reference_ids=[reference["id"]],
            generation_seed=1,
            confidence=0.5,
            known_limitations=["hypothesis"],
        )
    with pytest.raises(ValueError, match="backend or checkpoint"):
        store.import_result(
            multiview["id"],
            mesh_digests=[],
            texture_digests=[],
            image_digests=[artifacts["view"]],
            pbr_channels={},
            backend_identity="different-backend",
            checkpoint=multiview["checkpoint"],
            input_reference_ids=[reference["id"]],
            generation_seed=1,
            confidence=0.5,
            known_limitations=["hypothesis"],
        )


def test_distributed_generative_worker_completion_imports_governed_hypothesis(
    tmp_path: Path,
) -> None:
    project, reference, artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)
    request = _request(store, "generate_shape", reference_id=reference["id"])
    job_id = Coordinator(project).enqueue(
        "generative3d.execute",
        {
            "request_id": request["id"],
            "operation": request["operation"],
            "backend": request["backend"],
            "checkpoint": request["checkpoint"],
            "worker_requirements": {
                "required_models": [request["backend"]],
                "preferred_models": [request["backend"]],
            },
        },
    )
    store.bind_job(request["id"], job_id)
    scheduler = DistributedScheduler(project)
    wrong_model = scheduler.register(
        "Wrong generative model",
        "generative",
        {
            "hardware": ["cuda"],
            "vram_gb": 48,
            "system_memory_gb": 64,
            "supported_models": ["another-generator"],
            "render_devices": ["CUDA"],
            "capabilities": ["generative3d.*"],
        },
    )
    assert scheduler.claim(wrong_model["id"], wrong_model["worker_token"]) is None
    worker = scheduler.register(
        "Generative GPU",
        "generative",
        {
            "hardware": ["cuda"],
            "vram_gb": 32,
            "system_memory_gb": 64,
            "supported_models": [request["backend"]],
            "render_devices": ["CUDA"],
            "capabilities": ["generative3d.*"],
        },
    )
    lease = scheduler.claim(worker["id"], worker["worker_token"])

    assert lease is not None
    assert lease["requirements"]["worker_classes"] == ["generative"]
    assert request["request_digest"] in lease["input_artifact_digests"]
    completed = scheduler.complete(
        worker["id"],
        worker["worker_token"],
        job_id,
        lease["lease_token"],
        result={
            "mesh_digests": [artifacts["mesh"]],
            "texture_digests": [],
            "image_digests": [],
            "pbr_channels": {},
            "backend_identity": request["backend"],
            "checkpoint": request["checkpoint"],
            "input_reference_ids": [reference["id"]],
            "generation_seed": 91,
            "confidence": 0.72,
            "known_limitations": ["hidden geometry remains inferred"],
        },
        output_artifact_digests=[artifacts["mesh"]],
    )

    assert completed["status"] == "succeeded"
    governed = completed["result"]["governed_result"]
    assert governed["status"] == "HYPOTHESIS"
    assert store.get_request(request["id"])["status"] == "COMPLETED"
    receipt = export_receipt(project)
    assert receipt["acceptance"]["metrics"]["generative_hypotheses"][
        "invalid_result_ids"
    ] == []
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_terminal_worker_failure_marks_proposal_failed_without_invalidating_receipt(
    tmp_path: Path,
) -> None:
    project, reference, _artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)
    request = _request(store, "generate_shape", reference_id=reference["id"])
    job_id = Coordinator(project).enqueue(
        "generative3d.execute", {"request_id": request["id"]}
    )
    store.bind_job(request["id"], job_id)
    scheduler = DistributedScheduler(project)
    worker = scheduler.register(
        "Failing generator",
        "generative",
        {
            "hardware": ["cuda"],
            "vram_gb": 24,
            "system_memory_gb": 32,
            "supported_models": [],
            "render_devices": ["CUDA"],
            "capabilities": ["generative3d.*"],
        },
    )
    lease = scheduler.claim(worker["id"], worker["worker_token"])
    assert lease is not None

    failed = scheduler.fail(
        worker["id"],
        worker["worker_token"],
        job_id,
        lease["lease_token"],
        error={"type": "OutOfMemory"},
        retryable=False,
    )

    assert failed["status"] == "failed"
    assert store.get_request(request["id"])["status"] == "FAILED"
    assert store.audit(request["id"])["invalid_request_ids"] == []


def test_hash_valid_semantically_forged_generative_result_blocks_acceptance(
    tmp_path: Path,
) -> None:
    project, reference, artifacts = _project(tmp_path)
    store = GenerativeProposalStore(project)
    request = _request(store, "generate_shape", reference_id=reference["id"])
    result = _import(store, request, artifacts)
    forged = {key: value for key, value in result.items() if key not in {"artifact", "path"}}
    forged["acceptance_eligible"] = True
    forged_path = project.root / "receipts" / "forged-generative-result.json"
    forged_path.write_text(json.dumps(forged), encoding="utf-8")
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path, media_type="application/vnd.bvmcp.generative-result+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE generative_results SET result_json=?,record_digest=? WHERE id=?",
            (json.dumps(forged), forged_artifact.digest, result["id"]),
        )

    audit = store.audit(request["id"])
    assert audit["invalid_result_ids"] == [result["id"]]
    with pytest.raises(ProjectError, match="lineage is invalid"):
        store.get_result(request["id"])
    acceptance = export_receipt(project)["acceptance"]
    assert "one or more generative result receipts are invalid" in acceptance["blockers"]


def test_open_migrates_legacy_generative_proposal_tables(tmp_path: Path) -> None:
    project, _reference, _artifacts = _project(tmp_path)
    with project.connection() as connection:
        connection.execute("DROP TABLE generative_results")
        connection.execute("DROP TABLE generative_requests")
        connection.execute(
            "CREATE TABLE generative_requests ("
            "id TEXT PRIMARY KEY,operation TEXT NOT NULL,backend TEXT NOT NULL,"
            "request_json TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL,"
            "updated_at TEXT NOT NULL)"
        )
        connection.execute(
            "CREATE TABLE generative_results ("
            "id TEXT PRIMARY KEY,request_id TEXT NOT NULL REFERENCES generative_requests(id),"
            "result_json TEXT NOT NULL,status TEXT NOT NULL,created_at TEXT NOT NULL)"
        )

    reopened = ProjectStore.open(project.root)
    with reopened.connection() as connection:
        request_columns = {
            row["name"]
            for row in connection.execute("PRAGMA table_info(generative_requests)")
        }
        result_columns = {
            row["name"]
            for row in connection.execute("PRAGMA table_info(generative_results)")
        }
        indexes = {
            row["name"]
            for row in connection.execute("PRAGMA index_list(generative_requests)")
        }
    assert {"request_digest", "license_json", "cache_key", "job_id"} <= request_columns
    assert "record_digest" in result_columns
    assert "generative_request_cache_key" in indexes
