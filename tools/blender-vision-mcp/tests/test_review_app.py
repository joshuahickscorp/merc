import json
import urllib.error
import urllib.request
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import CameraSolution, EvidenceClass, RegistrationClass
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.targets import TargetResolver
from blender_vision.features.store import FeatureStore
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.orchestration.roles import RoleTaskStore
from blender_vision.projects.store import ProjectStore
from blender_vision.repairs.store import RepairStore
from blender_vision.review import ReviewService, create_review_server
from blender_vision.review.server import run_review_server_in_thread


def _json_request(
    url: str,
    *,
    method: str = "GET",
    payload: dict[str, object] | None = None,
    token: str | None = None,
) -> tuple[int, dict[str, object]]:
    data = json.dumps(payload).encode() if payload is not None else None
    headers = {"Content-Type": "application/json"} if data is not None else {}
    if token is not None:
        headers["X-BVMCP-Review-Token"] = token
    request = urllib.request.Request(url, data=data, headers=headers, method=method)
    with urllib.request.urlopen(request, timeout=3) as response:
        return response.status, json.loads(response.read())


def test_review_snapshot_prioritizes_all_governed_checkpoints(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Review surface")
    feature = FeatureStore(project).add(
        "USB-C",
        parent_component="rear-panel",
        observations=[{"kind": "scene_object", "object_name": "rear-usbc"}],
        confidence=0.75,
        evidence_class=EvidenceClass.INFERRED_HIGH_CONFIDENCE,
        model_revision="scene-digest",
    )
    repair = RepairStore(project).propose(
        "checkpoint",
        {"minimum_open_fraction": 1.0},
        evidence_bindings=[{"kind": "feature", "id": feature["id"]}],
        expected_improvement={"feature_residual": "lower"},
    )
    RepairStore(project).approve(repair["id"], "A. Reviewer")

    snapshot = ReviewService(project).snapshot()

    assert snapshot["project"]["name"] == "Review surface"
    assert [item["kind"] for item in snapshot["review_queue"]] == ["repair", "feature"]
    assert snapshot["review_queue"][0]["state"] == "approved"
    assert snapshot["latest_receipt"] is None


def test_review_queue_collapses_dominated_camera_proposals_and_labels_metric_authority(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera review grouping")

    def camera(
        identifier: str,
        created_at: str,
        reference_ids: list[str],
        registration_class: str,
    ) -> dict:
        return {
            "id": identifier,
            "backend": "fixture",
            "created_at": created_at,
            "approved": False,
            "diagnostics": {},
            "solution": {
                "approval": {"state": "pending"},
                "cameras": [
                    {
                        "reference_id": reference_id,
                        "registration_class": registration_class,
                        "evidence_class": "SINGLE_VIEW_OBSERVED",
                        "confidence": 0.8,
                    }
                    for reference_id in reference_ids
                ],
            },
        }

    older = camera(
        "older", "2026-01-01T00:00:00Z", ["front"], "approximate_visual_registration"
    )
    newer = camera(
        "newer", "2026-01-02T00:00:00Z", ["front"], "approximate_visual_registration"
    )
    consolidated = camera(
        "consolidated",
        "2026-01-03T00:00:00Z",
        ["front", "rear"],
        "approximate_visual_registration",
    )
    metric = camera(
        "metric", "2026-01-04T00:00:00Z", ["detail"], "metric_camera_solution"
    )
    service = ReviewService(project)
    queue = service.review_queue(
        features=[],
        cameras=[older, newer, consolidated, metric],
        repairs=[],
        fits=[],
        materials=[],
        landmark_proposals=[],
        optimization_runs=[],
        role_tasks=[],
        reference_adoptions=[],
        benchmark_review={"required": False, "valid": False},
    )

    by_id = {item["id"]: item for item in queue}
    assert set(by_id) == {"consolidated", "metric"}
    assert by_id["consolidated"]["alternative_count"] == 2
    assert by_id["consolidated"]["lower_priority_alternative_ids"] == [
        "newer",
        "older",
    ]
    assert by_id["metric"]["metric_authority"] is True
    assert by_id["metric"]["authority_warning"] is None

    stronger_older = camera(
        "stronger-older",
        "2026-01-01T00:00:00Z",
        ["front"],
        "approximate_visual_registration",
    )
    weaker_newer = camera(
        "weaker-newer",
        "2026-01-05T00:00:00Z",
        ["front"],
        "approximate_visual_registration",
    )
    stronger_older["solution"]["cameras"][0]["confidence"] = 0.95
    stronger_older["solution"]["cameras"][0]["diagnostics"] = {
        "search_silhouette_iou": 0.98
    }
    weaker_newer["solution"]["cameras"][0]["confidence"] = 0.6
    weaker_newer["solution"]["cameras"][0]["diagnostics"] = {
        "search_silhouette_iou": 0.8
    }
    assert {
        item["id"]
        for item in ReviewService._prioritized_camera_reviews(
            [stronger_older, weaker_newer]
        )
    } == {"stronger-older"}


def test_applied_repair_queue_item_exposes_render_and_validation_evidence(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Repair evidence card")
    digest = "a" * 64
    item = ReviewService(project)._repair_review_item(
        {
            "id": "repair-id",
            "kind": "hero_surface",
            "status": "applied",
            "expected": {"hole_count": 2349},
            "result": {
                "source_scene_id": "source-scene",
                "generated_scene": {"id": "candidate-scene"},
                "rear_render": {"artifact": {"digest": digest}},
                "worker": {
                    "generated_hole_count": 2349,
                    "ray_validation": {"open_fraction": 1.0},
                    "topology": {"non_manifold_edges": 0},
                    "dimensional_checks": {"width_within_tolerance": True},
                },
                "audit": {
                    "audit": {
                        "inventory": {
                            "audit_findings": [{"code": "FIXTURE_WARNING"}]
                        }
                    }
                },
                "acceptance": {"accepted": False, "state": "awaiting_final_review"},
            },
        }
    )

    assert item["render_url"] == f"/artifact/{digest}"
    assert item["validation"]["generated_hole_count"] == 2349
    assert item["validation"]["ray_validation"]["open_fraction"] == 1.0
    assert item["validation"]["audit_findings"] == [{"code": "FIXTURE_WARNING"}]


def test_review_http_requires_token_for_mutations_and_serves_snapshot(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "HTTP review")
    feature = FeatureStore(project).add(
        "USB-C",
        parent_component="rear-panel",
        observations=[{"kind": "scene_object", "object_name": "rear-usbc"}],
        confidence=0.8,
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
        model_revision="scene-digest",
    )
    server = create_review_server(project, port=0)
    thread = run_review_server_in_thread(server)
    try:
        with urllib.request.urlopen(server.url, timeout=3) as response:
            index = response.read().decode()
            assert response.status == 200
            assert "__BVMCP_TOKEN__" not in index
            assert server.token in index
            assert response.headers["Content-Security-Policy"].startswith("default-src 'self'")

        status, snapshot = _json_request(f"{server.url}api/snapshot")
        assert status == 200
        assert snapshot["project"]["name"] == "HTTP review"

        with pytest.raises(urllib.error.HTTPError) as missing_token:
            _json_request(
                f"{server.url}api/action/feature.review",
                method="POST",
                payload={"id": feature["id"], "accepted": True},
            )
        assert missing_token.value.code == 403

        status, result = _json_request(
            f"{server.url}api/action/feature.review",
            method="POST",
            token=server.token,
            payload={
                "id": feature["id"],
                "accepted": True,
                "reviewer": "A. Reviewer",
                "reason": "Compared against the registered evidence",
            },
        )
        assert status == 200
        assert result["result"]["approval"]["state"] == "approved"
    finally:
        server.shutdown()
        thread.join(timeout=3)


def test_review_server_refuses_non_loopback_bind(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Bind safety")
    with pytest.raises(ValueError, match="loopback"):
        create_review_server(project, host="0.0.0.0", port=0)


def test_review_queue_exposes_complete_landmark_decisions_and_role_boundaries(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Landmark review surface")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Review target"}
    )
    acquisition = EvidenceAcquisitionStore(project)
    image_path = tmp_path / "reference.png"
    Image.new("RGB", (640, 480), "gray").save(image_path)
    image_source = acquisition.register_source(
        target["id"],
        {
            "origin": "user://review-image",
            "publisher": "fixture",
            "page_title": "Review image",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "SYNTHETIC_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )
    image_source = acquisition.acquire_local(image_source["id"], image_path)
    model_path = tmp_path / "model.glb"
    model_path.write_bytes(b"review fixture model")
    model_source = acquisition.register_source(
        target["id"],
        {
            "origin": "user://review-model",
            "publisher": "fixture",
            "page_title": "Review model",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "public 3D landmark reference",
            "quality_score": 1.0,
        },
        rights={"status": "SYNTHETIC_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )
    model_source = acquisition.acquire_local(model_source["id"], model_path)
    intrinsics = CameraSolver(project)._store_solution(
        "review_fixture",
        [
            CameraSolution(
                reference_id=image_source["reference"]["id"],
                model="PINHOLE",
                width=640,
                height=480,
                intrinsics={"fx": 800.0, "fy": 800.0, "cx": 320.0, "cy": 240.0},
                world_from_camera=[
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 0.0],
                    [0.0, 0.0, 1.0, -1000.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                confidence=0.6,
                registration_class=RegistrationClass.FEATURE_BASED.value,
                evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
                distortion_model={
                    "type": "PINHOLE",
                    "parameters": {},
                    "render_policy": "undistorted_input",
                },
            )
        ],
        {"registered_images": 1},
    )
    measurements = MeasurementStore(project)
    bindings = [
        measurements.add(
            "known_overall_dimension",
            {"axis": axis, "millimetres": value},
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            certainty="exact",
        )["id"]
        for axis, value in (("x", 400.0), ("y", 300.0), ("z", 200.0))
    ]
    world_points = [
        [-200.0, -150.0, -100.0],
        [200.0, -150.0, -100.0],
        [-200.0, 150.0, -100.0],
        [200.0, 150.0, -100.0],
        [-200.0, -150.0, 100.0],
        [200.0, -150.0, 100.0],
        [-200.0, 150.0, 100.0],
        [200.0, 150.0, 100.0],
    ]
    correspondences = [
        {
            "landmark_id": f"corner-{index}",
            "world_mm": point,
            "image_px": [float(80 + index * 60), float(100 + (index % 2) * 160)],
            "confidence": 0.8,
            "method": "review-surface-fixture",
        }
        for index, point in enumerate(world_points)
    ]
    proposal = CameraLandmarkStore(project).propose(
        target_id=target["id"],
        model_source_id=model_source["id"],
        intrinsics_solution_id=intrinsics["id"],
        evidence_binding_ids=bindings,
        views=[
            {
                "image_source_id": image_source["id"],
                "correspondences": correspondences,
            }
        ],
        backend_identity={"name": "review-surface-fixture", "version": 1},
        known_limitations=["synthetic review-surface test"],
    )
    campaign = CampaignStore(project).start("fixture", configuration={})
    role_task = RoleTaskStore(project).assign(
        campaign["id"],
        "Independently inspect the landmark proposal",
        role="Camera Analyst",
        confidence=0.4,
        estimated_cost=0.2,
        inputs={"proposal_id": proposal["id"]},
    )
    RoleTaskStore(project).set_waiting(
        role_task["id"], reason="Named point decisions are required"
    )

    queue = ReviewService(project).review_queue()

    assert queue[0]["kind"] == "role_task"
    assert queue[0]["actionable"] is False
    landmark = next(item for item in queue if item["kind"] == "landmark")
    assert landmark["authority"] == "MACHINE_PROPOSAL_NOT_REVIEWED"
    assert landmark["point_count"] == 8
    assert landmark["views"][0]["image_url"].startswith("/artifact/")
    assert landmark["views"][0]["image_width"] == 640
    camera_item = next(item for item in queue if item["kind"] == "camera")
    assert camera_item["metric_authority"] is False
    assert "cannot satisfy L3" in camera_item["authority_warning"]
    assert camera_item["views"][0]["image_url"].startswith("/artifact/")
    assert camera_item["views"][0]["reference_id"] == image_source["reference"]["id"]
    with pytest.raises(ValueError, match="decide every"):
        ReviewService(project).action(
            "landmarks.review",
            {
                "id": proposal["id"],
                "reviewer": "Named fixture reviewer",
                "reason": "Checked the displayed overlay",
                "decisions": [],
            },
        )
    reviewed = ReviewService(project).action(
        "landmarks.review",
        {
            "id": proposal["id"],
            "reviewer": "Named fixture reviewer",
            "reason": "Checked every displayed point against the fixture",
            "decisions": [
                {
                    "reference_id": image_source["reference"]["id"],
                    "landmark_id": point["landmark_id"],
                    "decision": "accept",
                }
                for point in correspondences
            ],
        },
    )
    assert reviewed["status"] == "READY_FOR_PNP"
    assert not any(
        item["kind"] == "landmark" and item["id"] == proposal["id"]
        for item in ReviewService(project).review_queue()
    )
