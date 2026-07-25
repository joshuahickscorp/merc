import base64
import hashlib
import json
from datetime import UTC, datetime, timedelta
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.artifacts.transfer import ArtifactTransfer
from blender_vision.datasets.store import DatasetStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.coordinator import Coordinator
from blender_vision.scheduling.distributed import DistributedScheduler
from blender_vision.scheduling.worker import WorkerRuntime


def _capabilities(worker_class: str) -> dict[str, object]:
    return {
        "hardware": ["metal" if worker_class == "blender" else "cuda"],
        "vram_gb": 32,
        "system_memory_gb": 64,
        "supported_models": [],
        "blender_version": "4.2.1" if worker_class == "blender" else None,
        "render_devices": ["METAL"] if worker_class == "blender" else ["CUDA"],
        "capabilities": [f"{worker_class}.*"],
    }


def test_hardware_worker_claim_is_authenticated_capability_routed_and_retried(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Distributed")
    scheduler = DistributedScheduler(project)
    blender = scheduler.register("Mac Studio", "blender", _capabilities("blender"))
    vision = scheduler.register("RTX 5090", "vision", _capabilities("vision"))
    job_id = Coordinator(project).enqueue("blender.render", {"maximum_dimension": 512})

    assert scheduler.claim(vision["id"], vision["worker_token"]) is None
    with pytest.raises(PermissionError, match="credentials"):
        scheduler.claim(blender["id"], "wrong-token")

    lease = scheduler.claim(blender["id"], blender["worker_token"])
    assert lease is not None
    assert lease["job_id"] == job_id
    assert lease["attempt"] == 1
    assert lease["requirements"]["worker_classes"] == ["blender"]

    retried = scheduler.fail(
        blender["id"],
        blender["worker_token"],
        job_id,
        lease["lease_token"],
        error={"type": "TransientDeviceLoss"},
    )
    assert retried["status"] == "queued"
    second = scheduler.claim(blender["id"], blender["worker_token"])
    assert second is not None and second["attempt"] == 2

    completed = scheduler.complete(
        blender["id"],
        blender["worker_token"],
        job_id,
        second["lease_token"],
        result={"renders": []},
    )
    assert completed["status"] == "succeeded"
    assert completed["result"]["worker"]["attempt"] == 2


def test_expired_lease_is_requeued_for_fault_recovery(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Lease recovery")
    scheduler = DistributedScheduler(project)
    worker = scheduler.register("Mac Studio", "blender", _capabilities("blender"))
    job_id = Coordinator(project).enqueue("blender.inspect", {})
    assert scheduler.claim(worker["id"], worker["worker_token"]) is not None
    expired = (datetime.now(UTC) - timedelta(seconds=1)).isoformat()
    with project.connection() as connection:
        connection.execute("UPDATE job_leases SET expires_at=? WHERE job_id=?", (expired, job_id))

    report = scheduler.reap_expired()

    assert report == {"requeued": 1, "failed": 0}
    assert project.job(job_id)["status"] == "queued"


def test_packaged_worker_runtime_claims_executes_and_records_provenance(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Worker runtime")
    scheduler = DistributedScheduler(project)
    capabilities = _capabilities("blender")
    capabilities["capabilities"] = ["validation.*"]
    worker = scheduler.register("Mac Studio", "blender", capabilities)
    job_id = Coordinator(project).enqueue("validation.coverage")

    report = WorkerRuntime(
        project, worker["id"], worker["worker_token"], lease_seconds=30
    ).run(once=True)

    assert report == {
        "claimed": True,
        "job_id": job_id,
        "operation": "validation.coverage",
        "attempt": 1,
        "status": "succeeded",
        "cache_hit": False,
        "processed": 1,
    }
    completed = project.job(job_id)
    assert completed["result"]["worker"] == {"id": worker["id"], "attempt": 1}
    with project.connection() as connection:
        provenance = connection.execute(
            "SELECT execution_json FROM job_provenance WHERE job_id=?", (job_id,)
        ).fetchone()
    assert provenance is not None
    assert json.loads(provenance["execution_json"])["status"] == "succeeded"

    cached_job_id = Coordinator(project).enqueue("validation.coverage")
    cached_report = WorkerRuntime(
        project, worker["id"], worker["worker_token"], lease_seconds=30
    ).run(once=True)
    assert cached_report["job_id"] == cached_job_id
    assert cached_report["status"] == "succeeded"
    assert cached_report["cache_hit"] is True


def test_artifact_transfer_is_chunked_and_digest_verified(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Artifact transfer")
    scheduler = DistributedScheduler(project)
    worker = scheduler.register("RTX 5090", "vision", _capabilities("vision"))
    source = tmp_path / "input.bin"
    source.write_bytes(b"reference-evidence")
    existing = ArtifactStore(project).ingest_file(source)
    transfer = ArtifactTransfer(project)

    downloaded = transfer.read_chunk(
        worker["id"], worker["worker_token"], existing.digest, offset=0, maximum_bytes=5
    )
    assert base64.b64decode(downloaded["data_base64"]) == b"refer"
    assert downloaded["eof"] is False

    output = b"remote-worker-output"
    digest = hashlib.sha256(output).hexdigest()
    upload = transfer.begin_upload(
        worker["id"],
        worker["worker_token"],
        expected_digest=digest,
        expected_size=len(output),
        media_type="application/octet-stream",
        source_name="depth.bin",
    )
    transfer.upload_chunk(
        worker["id"],
        worker["worker_token"],
        upload["transfer_id"],
        offset=0,
        data_base64=base64.b64encode(output[:8]).decode(),
    )
    transfer.upload_chunk(
        worker["id"],
        worker["worker_token"],
        upload["transfer_id"],
        offset=8,
        data_base64=base64.b64encode(output[8:]).decode(),
    )
    completed = transfer.complete_upload(
        worker["id"], worker["worker_token"], upload["transfer_id"]
    )
    assert completed["verified"] is True
    assert completed["artifact"]["digest"] == digest

    rejected = transfer.begin_upload(
        worker["id"],
        worker["worker_token"],
        expected_digest="0" * 64,
        expected_size=3,
        media_type="application/octet-stream",
        source_name="tampered.bin",
    )
    transfer.upload_chunk(
        worker["id"],
        worker["worker_token"],
        rejected["transfer_id"],
        offset=0,
        data_base64=base64.b64encode(b"bad").decode(),
    )
    with pytest.raises(ValueError, match="declared size and digest"):
        transfer.complete_upload(worker["id"], worker["worker_token"], rejected["transfer_id"])
    with project.connection() as connection:
        row = connection.execute(
            "SELECT status,relative_path FROM artifact_transfers WHERE id=?",
            (rejected["transfer_id"],),
        ).fetchone()
    assert row is not None and row["status"] == "rejected"
    assert not (project.root / row["relative_path"]).exists()


def test_claim_prefers_a_job_with_worker_local_inputs(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Locality")
    source = tmp_path / "cached.bin"
    source.write_bytes(b"cached evidence")
    cached = ArtifactStore(project).ingest_file(source)
    scheduler = DistributedScheduler(project)
    worker = scheduler.register("RTX 5090", "vision", _capabilities("vision"))
    scheduler.heartbeat(
        worker["id"],
        worker["worker_token"],
        load={"current_jobs": 0, "queue_length": 0},
        artifact_digests=[cached.digest],
    )
    Coordinator(project).enqueue("vision.run", {"backend": "silhouette"})
    local_job = Coordinator(project).enqueue(
        "vision.run",
        {"backend": "silhouette", "input_hashes": [cached.digest]},
    )

    lease = scheduler.claim(worker["id"], worker["worker_token"])

    assert lease is not None and lease["job_id"] == local_job


def test_worker_can_abort_only_its_active_partial_upload(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Transfer abort")
    scheduler = DistributedScheduler(project)
    worker = scheduler.register("RTX 5090", "vision", _capabilities("vision"))
    other = scheduler.register("Other worker", "vision", _capabilities("vision"))
    transfer = ArtifactTransfer(project)
    upload = transfer.begin_upload(
        worker["id"],
        worker["worker_token"],
        expected_digest=hashlib.sha256(b"partial").hexdigest(),
        expected_size=7,
        media_type="application/octet-stream",
        source_name="partial.bin",
    )
    transfer.upload_chunk(
        worker["id"],
        worker["worker_token"],
        upload["transfer_id"],
        offset=0,
        data_base64=base64.b64encode(b"par").decode(),
    )
    with pytest.raises(KeyError, match="unknown active"):
        transfer.abort_upload(other["id"], other["worker_token"], upload["transfer_id"])

    aborted = transfer.abort_upload(
        worker["id"], worker["worker_token"], upload["transfer_id"]
    )
    assert aborted["status"] == "aborted"
    with project.connection() as connection:
        row = connection.execute(
            "SELECT status,relative_path FROM artifact_transfers WHERE id=?",
            (upload["transfer_id"],),
        ).fetchone()
    assert row is not None and row["status"] == "aborted"
    assert not (project.root / row["relative_path"]).exists()

    stale = transfer.begin_upload(
        worker["id"],
        worker["worker_token"],
        expected_digest=hashlib.sha256(b"stale").hexdigest(),
        expected_size=5,
        media_type="application/octet-stream",
        source_name="stale.bin",
    )
    old = (datetime.now(UTC) - timedelta(minutes=2)).isoformat()
    with project.connection() as connection:
        connection.execute(
            "UPDATE artifact_transfers SET updated_at=? WHERE id=?",
            (old, stale["transfer_id"]),
        )
    reaped = transfer.reap_stale(maximum_age_seconds=60)
    assert reaped == {"expired": 1, "transfer_ids": [stale["transfer_id"]]}


def test_distributed_dataset_completion_updates_governed_dataset(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Remote synthetic data")
    scene_path = tmp_path / "source.blend"
    scene_path.write_bytes(b"test scene placeholder")
    scene = SceneStore(project).import_blend(scene_path)
    dataset = DatasetStore(project).plan_synthetic(
        "technical labels", sample_count=2, seed=7, scene_id=scene["id"]
    )
    plan_digest = dataset["artifact"]["digest"]
    scheduler = DistributedScheduler(project)
    capabilities = _capabilities("training")
    capabilities["capabilities"] = ["training.*", "dataset.*"]
    worker = scheduler.register("DGX training", "training", capabilities)
    scheduler.heartbeat(
        worker["id"],
        worker["worker_token"],
        load={"current_jobs": 0, "queue_length": 0},
        artifact_digests=[scene["artifact"]["digest"]],
    )
    job_id = Coordinator(project).enqueue("dataset.generate", {"dataset_id": dataset["id"]})
    lease = scheduler.claim(worker["id"], worker["worker_token"])
    assert lease is not None and scene["artifact"]["digest"] in lease["input_artifact_digests"]
    generated = tmp_path / "generated-index.json"
    generated.write_text('{"sample_count": 2}', encoding="utf-8")
    artifact = ArtifactStore(project).ingest_file(generated)

    completed = scheduler.complete(
        worker["id"],
        worker["worker_token"],
        job_id,
        lease["lease_token"],
        result={"sample_count": 2},
        output_artifact_digests=[artifact.digest],
    )

    assert completed["status"] == "succeeded"
    assert completed["result"]["governed_result"]["status"] == "generated"
    generated_dataset = DatasetStore(project).get(dataset["id"])
    assert generated_dataset["status"] == "generated"
    assert generated_dataset["artifact_digest"] != plan_digest
    assert generated_dataset["manifest"]["execution"]["plan_record_digest"] == plan_digest
    assert ArtifactStore(project).path_for(plan_digest).is_file()


def test_perception_capture_analysis_and_verification_route_across_workers(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Distributed perception")
    image_path = tmp_path / "owned.png"
    Image.new("RGB", (64, 48), "#125cd2").save(image_path)
    scheduler = DistributedScheduler(project)
    capture_capabilities = _capabilities("vision")
    capture_capabilities["capabilities"] = [
        "perception.capture",
        "adapter.image.file",
        "device.primary",
    ]
    analysis_capabilities = _capabilities("vision")
    analysis_capabilities["capabilities"] = ["perception.workspace"]
    verifier_capabilities = _capabilities("vision")
    verifier_capabilities["capabilities"] = ["perception.verify"]
    replica_capabilities = _capabilities("vision")
    replica_capabilities["capabilities"] = [
        "perception.capture",
        "adapter.image.file",
        "device.replica",
    ]
    capture_worker = scheduler.register(
        "Browser acquisition worker", "vision", capture_capabilities
    )
    analysis_worker = scheduler.register(
        "DGX analysis worker", "vision", analysis_capabilities
    )
    verifier_worker = scheduler.register(
        "Central verifier", "vision", verifier_capabilities
    )
    replica_worker = scheduler.register(
        "Replica acquisition worker", "vision", replica_capabilities
    )
    capture_config = {
        "adapter": "image.file",
        "target": {"path": str(image_path)},
        "configuration": {"ocr": False},
        "rights_decision": "SYNTHETIC_OWNED",
    }
    capture_job = Coordinator(project).enqueue(
        "perception.capture",
        {
            **capture_config,
            "worker_requirements": {
                "worker_classes": ["vision"],
                "required_capabilities": [
                    "perception.capture",
                    "adapter.image.file",
                    "device.primary",
                ],
                "preferred_hardware": ["cuda", "cpu"],
                "required_models": [],
                "min_vram_gb": 0,
                "max_attempts": 3,
            },
        },
    )
    capture_report = WorkerRuntime(
        project,
        capture_worker["id"],
        capture_worker["worker_token"],
        lease_seconds=30,
    ).run(once=True)
    assert capture_report["status"] == "succeeded", json.dumps(capture_report)
    capture = project.job(capture_job)["result"]
    capture_id = capture["capture_id"]

    workspace_job = Coordinator(project).enqueue(
        "perception.workspace",
        {
            "capture_ids": [capture_id],
            "compute_budget": 5,
            "worker_requirements": {
                "worker_classes": ["vision"],
                "required_capabilities": ["perception.workspace"],
                "preferred_hardware": ["cuda", "cpu"],
                "required_models": [],
                "min_vram_gb": 0,
                "max_attempts": 3,
            },
        },
    )
    analysis_report = WorkerRuntime(
        project,
        analysis_worker["id"],
        analysis_worker["worker_token"],
        lease_seconds=30,
    ).run(once=True)
    workspace = project.job(workspace_job)["result"]["governed_result"]

    verify_job = Coordinator(project).enqueue(
        "perception.verify",
        {
            "capture_id": capture_id,
            "worker_requirements": {
                "worker_classes": ["vision"],
                "required_capabilities": ["perception.verify"],
                "preferred_hardware": ["cpu"],
                "required_models": [],
                "min_vram_gb": 0,
                "max_attempts": 3,
            },
        },
    )
    verify_report = WorkerRuntime(
        project,
        verifier_worker["id"],
        verifier_worker["worker_token"],
        lease_seconds=30,
    ).run(once=True)

    replica_job = Coordinator(project).enqueue(
        "perception.capture",
        {
            **capture_config,
            "worker_requirements": {
                "worker_classes": ["vision"],
                "required_capabilities": [
                    "perception.capture",
                    "adapter.image.file",
                    "device.replica",
                ],
                "preferred_hardware": ["cuda", "cpu"],
                "required_models": [],
                "min_vram_gb": 0,
                "max_attempts": 3,
            },
        },
    )
    replica_report = WorkerRuntime(
        project,
        replica_worker["id"],
        replica_worker["worker_token"],
        lease_seconds=30,
    ).run(once=True)
    replica = project.job(replica_job)["result"]

    assert capture_report["status"] == "succeeded"
    assert analysis_report["status"] == "succeeded"
    assert verify_report["status"] == "succeeded"
    assert replica_report["status"] == "succeeded"
    assert capture["governed_result"]["verification"]["valid"] is True
    assert workspace["status"] == "COMPLETE"
    assert project.job(verify_job)["result"]["governed_result"]["verification"][
        "valid"
    ] is True
    assert capture["worker"]["id"] != project.job(workspace_job)["result"]["worker"]["id"]
    assert replica["worker"]["id"] != capture["worker"]["id"]
    assert replica["capture_id"] == capture_id
    assert replica["manifest_digest"] == capture["manifest_digest"]
