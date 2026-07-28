from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import (
    evaluate_acceptance,
    export_receipt,
    verify_receipt,
)
from blender_vision.acceptance.transactions import (
    REQUIRED_GATE_CATEGORIES,
    CandidateTransactionStore,
)
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.cameras.landmarks import CameraLandmarkStore
from blender_vision.cameras.solver import CameraSolver
from blender_vision.comparison.metrics import compare_silhouettes
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.models import CameraSolution, EvidenceClass, RegistrationClass
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.review.service import ReviewService
from blender_vision.workflows.service import ReconstructionService


def _record_replayable_comparison(
    project: ProjectStore,
    reference: dict,
    render_path: Path,
    *,
    comparison_id: str,
    created_at: str,
) -> dict:
    artifacts = ArtifactStore(project)
    render = artifacts.ingest_file(render_path, media_type="image/png")
    residual_path = project.root / "comparisons" / f"{comparison_id}.png"
    metrics = compare_silhouettes(
        project.root / reference["relative_path"], render_path, residual_path
    )
    residual = artifacts.ingest_file(residual_path, media_type="image/png")
    return ComparisonStore(project).record(
        comparison_id,
        reference_id=reference["id"],
        render_digest=render.digest,
        residual_digest=residual.digest,
        metrics=metrics,
        engine="compare_silhouettes_v2",
        created_at=created_at,
    )


def test_camera_fallback_never_claims_metric_authority(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera Test")
    source = tmp_path / "front.png"
    Image.new("RGB", (100, 80), "white").save(source)
    ReferenceIngestor(project).import_file(source, rights_state="INTERNAL", viewpoint_label="front")
    result = CameraSolver(project).solve("turntable")
    assert result["backend"] == "turntable_fallback"
    assert result["cameras"][0]["registration_class"] == RegistrationClass.APPROXIMATE_VISUAL
    assert result["cameras"][0]["confidence"] < 0.5


def test_camera_undistortion_derives_pinhole_reference_without_authority_upgrade(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Undistortion")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Undistortion target"}
    )
    source = tmp_path / "distorted.png"
    image = Image.new("RGB", (160, 120), "gray")
    ImageDraw.Draw(image).rectangle((25, 20, 135, 105), outline="white", width=4)
    image.save(source)
    acquisition = EvidenceAcquisitionStore(project)
    governed_source = acquisition.register_source(
        target["id"],
        {
            "origin": "user://distorted-fixture",
            "publisher": "fixture",
            "page_title": "Distorted fixture",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={
            "status": "SYNTHETIC_OWNED",
            "internal_use": True,
            "redistribution": True,
        },
        reviewed_by="Fixture owner",
    )
    governed_source = acquisition.acquire_local(governed_source["id"], source)
    reference = governed_source["reference"]
    solver = CameraSolver(project)
    solution = solver._store_solution(
        "colmap",
        [
            CameraSolution(
                reference_id=reference["id"],
                model="SIMPLE_RADIAL",
                width=160,
                height=120,
                intrinsics={
                    "fx": 150.0,
                    "fy": 150.0,
                    "cx": 80.0,
                    "cy": 60.0,
                    "k1": 0.08,
                },
                world_from_camera=[
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 0.0],
                    [0.0, 0.0, 1.0, 500.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                confidence=0.75,
                registration_class=RegistrationClass.FEATURE_BASED.value,
                evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
            )
        ],
        {"registered_images": 1},
    )

    derived = solver.derive_undistorted_solution(solution["id"])

    assert derived["backend"] == "colmap_undistorted"
    assert derived["cameras"][0]["model"] == "PINHOLE"
    assert derived["cameras"][0]["registration_class"] == RegistrationClass.FEATURE_BASED
    assert derived["approved"] is False
    references = {item["id"]: item for item in ReferenceIngestor(project).list()}
    assert references[reference["id"]]["acceptance_eligible"] is False
    derived_reference = references[derived["cameras"][0]["reference_id"]]
    assert derived_reference["acceptance_eligible"] is True
    assert derived_reference["metadata"]["derived_from_reference_id"] == reference["id"]
    assert derived["reference_derivations"][0]["governed_source_id"] == governed_source["id"]
    acceptance = evaluate_acceptance(project)
    assert derived_reference["id"] in acceptance["metrics"]["reference_derivations"][
        "valid_governed_reference_ids"
    ]
    assert (
        "autonomous reconstruction has legacy references outside the governed source ledger"
        not in acceptance["blockers"]
    )
    with project.connection() as connection:
        row = connection.execute(
            "SELECT metadata_json FROM reference_items WHERE id=?",
            (derived_reference["id"],),
        ).fetchone()
        forged_metadata = json.loads(row["metadata_json"])
        forged_metadata["undistortion_roi"] = [1, 1, 10, 10]
        connection.execute(
            "UPDATE reference_items SET metadata_json=? WHERE id=?",
            (json.dumps(forged_metadata), derived_reference["id"]),
        )
    tampered = evaluate_acceptance(project)
    assert "derived acceptance references lack valid governance lineage receipts" in tampered[
        "blockers"
    ]
    assert tampered["metrics"]["reference_derivations"]["invalid_derivations"]


def test_reviewed_pnp_landmarks_recover_pending_metric_camera(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "PnP landmarks")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Calibration box"}
    )
    source = tmp_path / "pnp.png"
    Image.new("RGB", (640, 480), "gray").save(source)
    acquisition = EvidenceAcquisitionStore(project)
    image_source = acquisition.register_source(
        target["id"],
        {
            "origin": "user://pnp-image",
            "publisher": "fixture",
            "page_title": "PnP image",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "SYNTHETIC_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )
    image_source = acquisition.acquire_local(image_source["id"], source)
    reference = image_source["reference"]
    model_path = tmp_path / "fixture.glb"
    model_path.write_bytes(b"governed fixture model")
    model_source = acquisition.register_source(
        target["id"],
        {
            "origin": "user://pnp-model",
            "publisher": "fixture",
            "page_title": "PnP model",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "public 3D landmark reference",
            "quality_score": 1.0,
        },
        rights={"status": "SYNTHETIC_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )
    model_source = acquisition.acquire_local(model_source["id"], model_path)
    solver = CameraSolver(project)
    intrinsics = solver._store_solution(
        "synthetic_undistorted",
        [
            CameraSolution(
                reference_id=reference["id"],
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
                confidence=0.75,
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
    correspondences = []
    for index, (x, y, z) in enumerate(world_points):
        depth = z + 1000.0
        correspondences.append(
            {
                "landmark_id": f"corner-{index}",
                "world_mm": [x, y, z],
                "image_px": [800.0 * x / depth + 320.0, 800.0 * y / depth + 240.0],
                "confidence": 1.0,
                "method": "synthetic_projection_fixture",
            }
        )
    landmark_store = CameraLandmarkStore(project)
    stale_proposal = landmark_store.propose(
        target_id=target["id"],
        model_source_id=model_source["id"],
        intrinsics_solution_id=intrinsics["id"],
        evidence_binding_ids=bindings,
        views=[
            {"image_source_id": image_source["id"], "correspondences": correspondences}
        ],
        backend_identity={"name": "synthetic-fixture", "version": "1"},
        known_limitations=["synthetic unit-test proposal"],
    )
    proposal = landmark_store.propose(
        target_id=target["id"],
        model_source_id=model_source["id"],
        intrinsics_solution_id=intrinsics["id"],
        evidence_binding_ids=bindings,
        views=[
            {"image_source_id": image_source["id"], "correspondences": correspondences}
        ],
        backend_identity={"name": "synthetic-fixture-v2", "version": "2"},
        known_limitations=["synthetic unit-test replacement proposal"],
    )
    supersession = landmark_store.supersede_machine_proposal(
        stale_proposal["id"],
        replacement_id=proposal["id"],
        reason="Replace the unreviewed fixture output with deterministic v2 points",
    )
    superseded = landmark_store.get(stale_proposal["id"], verify=True)
    assert superseded["status"] == "SUPERSEDED"
    assert superseded["superseded_by_id"] == proposal["id"]
    assert superseded["supersession"]["id"] == supersession["id"]
    queued_landmarks = [
        item for item in ReviewService(project).review_queue() if item["kind"] == "landmark"
    ]
    assert [item["id"] for item in queued_landmarks] == [proposal["id"]]
    with pytest.raises(ValueError, match="only unreviewed"):
        landmark_store.supersede_machine_proposal(
            stale_proposal["id"],
            replacement_id=proposal["id"],
            reason="Cannot supersede the same proposal twice",
        )
    with pytest.raises(ValueError, match="decide every"):
        landmark_store.review(
            proposal["id"],
            reviewer="Synthetic fixture reviewer",
            reason="Projection is analytically known",
            decisions=[],
        )
    review = landmark_store.review(
        proposal["id"],
        reviewer="Synthetic fixture reviewer",
        reason="Projection is analytically known",
        decisions=[
            {
                "reference_id": reference["id"],
                "landmark_id": point["landmark_id"],
                "decision": "accept",
            }
            for point in correspondences
        ],
    )
    assert review["status"] == "READY_FOR_PNP"
    result = solver.solve_pnp_landmarks(
        landmark_proposal_id=proposal["id"],
    )

    camera = result["cameras"][0]
    assert result["backend"] == "opencv_pnp_landmarks"
    assert result["approved"] is False
    assert camera["registration_class"] == RegistrationClass.METRIC
    assert camera["diagnostics"]["quality"]["reprojection_rmse_px"] < 0.01
    assert camera["diagnostics"]["quality"]["registered_feature_count"] == 8
    assert camera["diagnostics"]["world_units"] == "millimetres"
    assert camera["diagnostics"]["landmark_review_id"] == review["id"]
    assert camera["diagnostics"]["landmark_review_digest"] == review["review_digest"]

    stored = landmark_store.get(proposal["id"])
    forged = dict(stored["review"])
    forged["decisions"] = [dict(item) for item in forged["decisions"]]
    forged["decisions"][0]["decision"] = "reject"
    forged_path = tmp_path / "forged-landmark-review.json"
    forged_path.write_text(json.dumps(forged), encoding="utf-8")
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.camera-landmark-review+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE camera_landmark_proposals SET review_json=?,review_digest=? WHERE id=?",
            (json.dumps(forged), forged_artifact.digest, proposal["id"]),
        )
    with pytest.raises(ValueError, match="landmark"):
        solver.solve_pnp_landmarks(landmark_proposal_id=proposal["id"])
    acceptance = evaluate_acceptance(project)
    assert result["id"] in acceptance["metrics"]["camera"][
        "invalid_pnp_landmark_solution_ids"
    ]


def test_camera_fallback_applies_governed_view_direction_and_roll(tmp_path: Path) -> None:
    project = ProjectStore.create(
        tmp_path / "project",
        "Hinted Camera Test",
        metadata={
            "camera_hints_by_viewpoint": {
                "top-edge": {"view_direction": [1.0, 0.0, 0.0], "roll_degrees": 90.0}
            }
        },
    )
    source = tmp_path / "top-edge.png"
    Image.new("RGB", (120, 80), "white").save(source)
    ReferenceIngestor(project).import_file(
        source, rights_state="INTERNAL", viewpoint_label="top-edge"
    )

    result = CameraSolver(project).solve("turntable")
    diagnostics = result["cameras"][0]["diagnostics"]

    assert diagnostics["view_direction"] == [1.0, 0.0, 0.0]
    assert diagnostics["camera_roll_degrees"] == 90.0
    assert diagnostics["manifest_camera_hint_applied"] is True
    assert result["cameras"][0]["registration_class"] == RegistrationClass.APPROXIMATE_VISUAL


def test_silhouette_comparison_writes_residual_and_metrics(tmp_path: Path) -> None:
    reference = Image.new("RGBA", (80, 80), (0, 0, 0, 0))
    render = Image.new("RGBA", (80, 80), (0, 0, 0, 0))
    ImageDraw.Draw(reference).rectangle((20, 20, 60, 60), fill=(200, 200, 200, 255))
    ImageDraw.Draw(render).rectangle((22, 20, 62, 60), fill=(200, 200, 200, 255))
    reference_path = tmp_path / "reference.png"
    render_path = tmp_path / "render.png"
    residual_path = tmp_path / "residual.png"
    reference.save(reference_path)
    render.save(render_path)
    metrics = compare_silhouettes(reference_path, render_path, residual_path)
    assert 0.8 < metrics["silhouette_iou"] < 1.0
    assert metrics["reference_segmentation"] == "embedded_alpha"
    assert metrics["reference_partial_object_crop"] is False
    assert residual_path.is_file()


def test_silhouette_comparison_marks_adjacent_edge_crop(tmp_path: Path) -> None:
    reference = Image.new("RGBA", (100, 80), (0, 0, 0, 0))
    render = Image.new("RGBA", (100, 80), (0, 0, 0, 0))
    ImageDraw.Draw(reference).rectangle((20, 10, 99, 79), fill=(200, 200, 200, 255))
    ImageDraw.Draw(render).rectangle((20, 10, 99, 79), fill=(200, 200, 200, 255))
    reference_path = tmp_path / "partial-reference.png"
    render_path = tmp_path / "partial-render.png"
    residual_path = tmp_path / "partial-residual.png"
    reference.save(reference_path)
    render.save(render_path)

    metrics = compare_silhouettes(reference_path, render_path, residual_path)

    assert metrics["silhouette_iou"] == 1.0
    assert metrics["reference_partial_object_crop"] is True


def test_local_render_comparison_recomputes_only_governed_crop(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Local crop comparison")
    reference_path = tmp_path / "reference.png"
    reference_image = Image.new("RGBA", (100, 100), (0, 0, 0, 0))
    ImageDraw.Draw(reference_image).rectangle((30, 30, 69, 69), fill=(220, 220, 220, 255))
    reference_image.save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    render_path = project.root / "renders" / "local-crop.png"
    reference_image.crop((25, 25, 75, 75)).save(render_path)
    artifact = ReconstructionService(project).artifacts.ingest_file(
        render_path, media_type="image/png"
    )

    result = ReconstructionService(project).compare_views(
        [
            {
                "reference_id": reference["id"],
                "relative_path": str(render_path.relative_to(project.root)),
                "artifact": artifact.to_dict(),
                "crop_roi_px": {"x": 25, "y": 25, "width": 50, "height": 50},
                "render": {"full_frame_width": 100, "full_frame_height": 100},
            }
        ]
    )

    metrics = result["comparisons"][0]["metrics"]
    assert metrics["silhouette_iou"] == 1.0
    assert metrics["locality"]["crop_roi_px"]["width"] == 50
    assert metrics["locality"]["full_frame_metric_recomputed"] is False
    assert len(metrics["locality"]["crop_artifact_digests"]["reference"]) == 64


def test_opaque_gradient_reference_uses_background_model_without_vision_extra(
    tmp_path: Path, monkeypatch
) -> None:
    from blender_vision.comparison import metrics as comparison_metrics

    monkeypatch.setattr(
        comparison_metrics,
        "_opencv_grabcut_mask",
        lambda _image, **_kwargs: None,
    )
    size = 96
    reference = Image.new("RGB", (size, size))
    pixels = reference.load()
    for y in range(size):
        for x in range(size):
            value = round(20 + 70 * x / (size - 1) + 15 * y / (size - 1))
            pixels[x, y] = (value, value + 1, value + 2)
    ImageDraw.Draw(reference).rectangle((24, 20, 72, 76), fill=(220, 225, 230))
    render = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    ImageDraw.Draw(render).rectangle((24, 20, 72, 76), fill=(220, 225, 230, 255))
    reference_path = tmp_path / "gradient-reference.png"
    render_path = tmp_path / "gradient-render.png"
    residual_path = tmp_path / "gradient-residual.png"
    reference.save(reference_path)
    render.save(render_path)

    result = compare_silhouettes(reference_path, render_path, residual_path)

    assert result["silhouette_iou"] > 0.99
    assert result["reference_segmentation"] == "bilinear_corner_background"
    assert result["reference_segmentation_confidence"] == "low"


def test_grabcut_comparison_is_byte_deterministic(tmp_path: Path) -> None:
    size = 160
    reference = Image.new("RGB", (size, size), (45, 55, 70))
    draw = ImageDraw.Draw(reference)
    draw.rounded_rectangle((25, 35, 135, 125), radius=12, fill=(180, 185, 190))
    draw.rectangle((55, 65, 105, 95), fill=(95, 105, 120))
    render = Image.new("RGBA", (size, size), (0, 0, 0, 0))
    ImageDraw.Draw(render).rounded_rectangle(
        (25, 35, 135, 125), radius=12, fill=(180, 185, 190, 255)
    )
    reference_path = tmp_path / "grabcut-reference.png"
    render_path = tmp_path / "grabcut-render.png"
    first_residual = tmp_path / "first.png"
    second_residual = tmp_path / "second.png"
    reference.save(reference_path)
    render.save(render_path)

    first = compare_silhouettes(reference_path, render_path, first_residual)
    second = compare_silhouettes(reference_path, render_path, second_residual)

    assert first["reference_segmentation"].startswith("opencv_grabcut")
    assert first == second
    assert first_residual.read_bytes() == second_residual.read_bytes()


def test_tall_foreground_shadow_wings_are_bounded_without_changing_horizontal_objects() -> None:
    import numpy as np

    from blender_vision.comparison.metrics import _suppress_horizontal_shadow_wings

    tall = np.zeros((100, 160), dtype=bool)
    tall[5:95, 70:90] = True
    tall[65:95, 10:150] = True
    edges = np.zeros_like(tall)
    edges[5, 70:90] = True
    edges[94, 70:90] = True
    bounded, applied = _suppress_horizontal_shadow_wings(tall, edge_map=edges)
    assert applied is True
    assert bounded[5:95, 70:90].all()
    assert not bounded[:, :50].any()
    assert not bounded[:, 110:].any()
    assert not bounded[98:, :].any()

    horizontal = np.zeros((100, 160), dtype=bool)
    horizontal[40:60, 10:150] = True
    unchanged, applied = _suppress_horizontal_shadow_wings(horizontal)
    assert applied is False
    assert np.array_equal(unchanged, horizontal)


def test_annotated_diagram_segmentation_suppresses_callouts() -> None:
    from blender_vision.comparison.metrics import _reference_mask

    diagram = Image.new("RGB", (400, 300), "white")
    draw = ImageDraw.Draw(diagram)
    draw.rectangle((30, 30, 370, 130), outline="black", width=7)
    for x in (90, 160, 230, 300):
        draw.line((x, 126, x, 245), fill="black", width=1)
        draw.rectangle((x - 22, 250, x + 22, 260), fill="black")

    mask, method, confidence = _reference_mask(diagram)

    assert method == "opencv_diagram_principal_outline_annotation_suppression"
    assert confidence == "medium"
    left, top, right, bottom = mask.getbbox() or (0, 0, 0, 0)
    assert (left, top, right) == (30, 30, 371)
    assert 131 <= bottom <= 132


def test_edge_validation_restores_wide_product_and_excludes_detached_object(
    monkeypatch,
) -> None:
    import cv2
    import numpy as np

    from blender_vision.comparison.metrics import _reference_mask

    image = Image.new("RGB", (400, 300), (55, 70, 90))
    draw = ImageDraw.Draw(image)
    draw.rounded_rectangle((20, 150, 380, 260), radius=10, fill=(120, 130, 140))
    draw.rectangle((20, 20, 60, 275), fill=(120, 130, 140))
    draw.rectangle((150, 150, 225, 180), fill=(55, 70, 90))
    draw.rectangle((85, 20, 150, 70), fill=(150, 155, 160))

    def segment_with_detached_component(
        _image, mask, _rect, _background_model, _foreground_model, _iterations, _mode
    ) -> None:
        mask.fill(cv2.GC_BGD)
        mask[150:261, 20:381] = cv2.GC_FGD
        mask[20:276, 20:61] = cv2.GC_FGD
        mask[20:71, 85:151] = cv2.GC_FGD

    monkeypatch.setattr(cv2, "grabCut", segment_with_detached_component)

    mask, method, confidence = _reference_mask(image)
    values = np.asarray(mask) >= 128

    assert method == "opencv_grabcut_principal_component_edge_validation"
    assert confidence == "medium"
    left, top, right, bottom = mask.getbbox() or (0, 0, 0, 0)
    assert left <= 21 and right >= 379
    assert 19 <= top <= 21 and 274 <= bottom <= 277
    assert not values[45, 110]
    assert values[220, 340]


def test_receipt_acceptance_uses_latest_comparison_per_reference(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Comparison Supersession")
    source = tmp_path / "front.png"
    source_image = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    ImageDraw.Draw(source_image).rectangle((16, 16, 47, 47), fill=(255, 255, 255, 255))
    source_image.save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="INTERNAL", viewpoint_label="front"
    )
    old_render = tmp_path / "old-render.png"
    new_render = tmp_path / "new-render.png"
    Image.new("RGBA", (64, 64), (255, 255, 255, 255)).save(old_render)
    source_image.save(new_render)
    _record_replayable_comparison(
        project,
        reference,
        old_render,
        comparison_id="old-failed-comparison",
        created_at="2026-01-01T00:00:00+00:00",
    )
    _record_replayable_comparison(
        project,
        reference,
        new_render,
        comparison_id="new-passing-comparison",
        created_at="2026-01-02T00:00:00+00:00",
    )

    receipt = export_receipt(project)

    blockers = receipt["acceptance"]["blockers"]
    assert "silhouette IoU is below the 0.95 L3 threshold" not in blockers
    assert "silhouette comparison requires high-confidence reference masks" not in blockers
    selection = receipt["acceptance"]["metrics"]["comparison_selection"]
    assert selection["policy"] == "latest_per_reference_for_authoritative_scene"
    assert selection["active_comparison_ids"] == ["new-passing-comparison"]
    assert selection["superseded_comparison_ids"] == ["old-failed-comparison"]


def test_receipt_keeps_partial_crop_comparison_diagnostic(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Partial Comparison")
    source = tmp_path / "detail.png"
    source_image = Image.new("RGBA", (64, 64), (0, 0, 0, 0))
    ImageDraw.Draw(source_image).rectangle((0, 0, 40, 40), fill=(255, 255, 255, 255))
    source_image.save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="INTERNAL", viewpoint_label="detail"
    )
    _record_replayable_comparison(
        project,
        reference,
        source,
        comparison_id="partial-comparison",
        created_at="2026-01-01T00:00:00+00:00",
    )

    receipt = export_receipt(project)

    blockers = receipt["acceptance"]["blockers"]
    assert "silhouette IoU is below the 0.95 L3 threshold" not in blockers
    assert any("partial-object crop comparisons are diagnostic" in item for item in blockers)
    selection = receipt["acceptance"]["metrics"]["comparison_selection"]
    assert selection["full_object_comparison_ids"] == []
    assert selection["diagnostic_partial_crop_comparison_ids"] == [
        "partial-comparison"
    ]


def test_receipt_does_not_reuse_comparison_from_superseded_scene(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Scene-bound comparisons")
    reference_path = tmp_path / "front.png"
    Image.new("RGB", (100, 80), "white").save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path,
        rights_state="INTERNAL",
        viewpoint_label="front",
    )
    comparison = _record_replayable_comparison(
        project,
        reference,
        reference_path,
        comparison_id="old-scene-passing-comparison",
        created_at="2026-01-01T00:01:00+00:00",
    )
    old_render_digest = comparison["render_digest"]
    camera = CameraSolver(project).import_manual(
        [
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": 100,
                "height": 80,
                "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
                "world_from_camera": [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 500.0],
                    [0.0, 0.0, 1.0, 50.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                "confidence": 0.7,
                "registration_class": "approximate_visual_registration",
                "evidence_class": "INFERRED_LOW_CONFIDENCE",
                "diagnostics": {},
            }
        ]
    )
    scenes = []
    for name in ("old", "new"):
        scene_path = tmp_path / f"{name}.blend"
        scene_path.write_bytes(name.encode())
        scenes.append(SceneStore(project).import_blend(scene_path))
    scene_store = SceneStore(project)
    scene_store.transition(
        scenes[1]["id"], "CANDIDATE", reviewer="Test builder", reason="Evaluate new scene"
    )
    evaluation = CandidateTransactionStore(project).evaluate(
        scenes[1]["id"],
        gates=[
            {"category": category, "name": f"{category} gate", "status": "PASS"}
            for category in sorted(REQUIRED_GATE_CATEGORIES)
        ],
    )
    scene_store.transition(
        scenes[1]["id"],
        "ACCEPTED",
        reviewer="Test QA",
        reason="All transaction gates passed",
        evaluation_id=evaluation["id"],
    )
    scene_store.transition(
        scenes[1]["id"],
        "PROMOTED",
        reviewer="Test owner",
        reason="Make the new scene authoritative",
        evaluation_id=evaluation["id"],
    )
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO render_runs"
            "(id,scene_id,camera_solution_id,config_json,outputs_json,created_at) "
            "VALUES(?,?,?,?,?,?)",
            (
                "old-render-run",
                scenes[0]["id"],
                camera["id"],
                "{}",
                json.dumps(
                    [
                        {
                            "reference_id": reference["id"],
                            "artifact_digest": old_render_digest,
                        }
                    ]
                ),
                "2026-01-01T00:00:00+00:00",
            ),
        )

    receipt = export_receipt(project)
    selection = receipt["acceptance"]["metrics"]["comparison_selection"]

    assert selection["authoritative_scene_id"] == scenes[1]["id"]
    assert selection["eligible_count"] == 0
    assert selection["active_comparison_ids"] == []
    assert "not every reference has a rendered comparison" in receipt["acceptance"][
        "blockers"
    ]


def test_receipt_uses_latest_camera_per_reference_instead_of_latest_document(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Camera supersession")
    references = []
    for viewpoint in ("front", "rear"):
        source = tmp_path / f"{viewpoint}.png"
        Image.new("RGB", (100, 80), "white").save(source)
        references.append(
            ReferenceIngestor(project).import_file(
                source,
                rights_state="INTERNAL",
                viewpoint_label=viewpoint,
            )
        )

    solution_ids = []
    for index, reference in enumerate(references):
        result = CameraSolver(project).import_manual(
            [
                {
                    "reference_id": reference["id"],
                    "model": "PINHOLE",
                    "width": 100,
                    "height": 80,
                    "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
                    "world_from_camera": [
                        [1.0, 0.0, 0.0, float(index)],
                        [0.0, 1.0, 0.0, 500.0],
                        [0.0, 0.0, 1.0, 50.0],
                        [0.0, 0.0, 0.0, 1.0],
                    ],
                    "confidence": 0.7,
                    "registration_class": "approximate_visual_registration",
                    "evidence_class": "INFERRED_LOW_CONFIDENCE",
                    "diagnostics": {},
                }
            ]
        )
        solution_ids.append(result["id"])

    receipt = export_receipt(project)
    camera = receipt["acceptance"]["metrics"]["camera"]

    assert camera["covered_reference_ids"] == sorted(item["id"] for item in references)
    assert camera["approved_reference_ids"] == []
    assert camera["active_solution_ids"] == sorted(solution_ids)
    assert camera["solution_ids_by_reference"] == {
        references[0]["id"]: solution_ids[0],
        references[1]["id"]: solution_ids[1],
    }
    assert "L3+ camera set does not cover every reference" not in receipt["acceptance"][
        "blockers"
    ]
    assert "L3+ approved camera set does not cover every reference" in receipt[
        "acceptance"
    ]["blockers"]


def test_receipt_prefers_camera_bound_to_active_comparison(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Validated camera selection")
    reference_path = tmp_path / "front.png"
    Image.new("RGBA", (64, 64), (0, 0, 0, 0)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="front"
    )
    camera_record = {
        "reference_id": reference["id"],
        "model": "PINHOLE",
        "width": 64,
        "height": 64,
        "intrinsics": {"fx": 80.0, "fy": 80.0, "cx": 32.0, "cy": 32.0},
        "world_from_camera": [
            [1.0, 0.0, 0.0, 0.0],
            [0.0, 1.0, 0.0, -1.0],
            [0.0, 0.0, 1.0, 1.0],
            [0.0, 0.0, 0.0, 1.0],
        ],
        "confidence": 0.4,
        "registration_class": "approximate_visual_registration",
        "evidence_class": "SINGLE_VIEW_OBSERVED",
        "diagnostics": {},
    }
    validated = CameraSolver(project).import_manual([camera_record])
    newer = CameraSolver(project).import_manual([{**camera_record, "confidence": 0.5}])
    blend = tmp_path / "scene.blend"
    blend.write_bytes(b"test scene")
    scene = SceneStore(project).import_blend(blend)
    comparison = _record_replayable_comparison(
        project,
        reference,
        reference_path,
        comparison_id="validated-comparison",
        created_at="2026-01-01T00:01:00+00:00",
    )
    render_digest = comparison["render_digest"]
    with project.connection() as connection:
        connection.execute(
            "INSERT INTO render_runs"
            "(id,scene_id,camera_solution_id,config_json,outputs_json,created_at) "
            "VALUES(?,?,?,?,?,?)",
            (
                "validated-render",
                scene["id"],
                validated["id"],
                "{}",
                json.dumps(
                    [{"reference_id": reference["id"], "artifact_digest": render_digest}]
                ),
                "2026-01-01T00:00:00+00:00",
            ),
        )

    receipt = export_receipt(project)
    camera_metrics = receipt["acceptance"]["metrics"]["camera"]

    assert camera_metrics["solution_ids_by_reference"] == {
        reference["id"]: validated["id"]
    }
    assert newer["id"] not in camera_metrics["active_solution_ids"]
    assert camera_metrics["selection_policy"] == (
        "active_authoritative_comparison_then_latest_per_reference"
    )


def test_receipt_integrity_is_separate_from_fidelity_acceptance(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Receipt Test")
    receipt = export_receipt(project)
    receipt_path = project.root / receipt["path"]
    verification = verify_receipt(receipt_path, project=project)
    assert verification["valid"] is True
    assert receipt["acceptance"]["accepted"] is False
    assert "no reference evidence" in receipt["acceptance"]["blockers"]
    envelope = json.loads(receipt_path.read_text(encoding="utf-8"))
    envelope["payload"]["project"]["name"] = "tampered"
    receipt_path.write_text(json.dumps(envelope), encoding="utf-8")
    assert verify_receipt(receipt_path)["valid"] is False


def test_receipt_uses_explicit_body_scope_instead_of_appendage_envelope(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(
        tmp_path / "project",
        "Scoped dimensions",
        metadata={
            "body_envelope": {
                "scope": "body_only",
                "object_patterns": ["body"],
            }
        },
    )
    scene_path = tmp_path / "scene.blend"
    scene_path.write_bytes(b"fixture")
    scene = SceneStore(project).import_blend(scene_path)
    SceneStore(project).set_inventory(
        scene["id"],
        {
            "canonical_transform": {"scale_to_millimetres": 1000.0},
            "canonical_bounds_mm": {"dimensions": [190.0, 153.0, 60.0]},
            "audit_findings": [],
            "objects": [
                {
                    "name": "body",
                    "type": "MESH",
                    "hidden_render": False,
                    "world_bounds": {
                        "minimum": [-0.075, -0.075, 0.0],
                        "maximum": [0.075, 0.075, 0.0505],
                    },
                },
                {
                    "name": "connector-appendage",
                    "type": "MESH",
                    "hidden_render": False,
                    "world_bounds": {
                        "minimum": [-0.095, -0.078, -0.005],
                        "maximum": [0.095, 0.075, 0.055],
                    },
                },
            ],
        },
    )
    for axis, millimetres in (("x", 150.0), ("y", 150.0), ("z", 50.5)):
        MeasurementStore(project).add(
            "known_overall_dimension",
            {
                "axis": axis,
                "millimetres": millimetres,
                "source": "https://example.invalid/spec",
                "retrieved_date": "2026-07-20",
                "scope": "body_only",
            },
            evidence_class=EvidenceClass.MANUFACTURER_SPEC,
            uncertainty={"millimetres": 0.25},
        )

    receipt = export_receipt(project)
    acceptance = receipt["acceptance"]

    assert "L3+ audited scene envelope exceeds authoritative tolerance" not in acceptance[
        "blockers"
    ]
    assert acceptance["metrics"]["dimension_scope"] == {
        "kind": "object_pattern_scope",
        "scope": "body_only",
        "object_patterns": ["body"],
        "object_count": 1,
        "objects": ["body"],
        "source": "project_metadata",
    }
    assert acceptance["metrics"]["dimension_residuals"]["z"][
        "audited_dimension_mm"
    ] == pytest.approx(50.5)


def test_metric_camera_import_needs_scale_and_explicit_approval(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Metric Camera")
    source = tmp_path / "rear.png"
    Image.new("RGB", (100, 80), "white").save(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="INTERNAL", viewpoint_label="rear"
    )
    scale = MeasurementStore(project).add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 197.0},
        evidence_class=EvidenceClass.MANUFACTURER_SPEC,
        uncertainty={"millimetres": 0.5},
    )
    quality = {
        "reprojection_rmse_px": 0.3,
        "registered_feature_count": 120,
        "view_coverage": 0.9,
        "baseline_diversity": 0.8,
        "scale_confidence": 0.95,
        "principal_point_confidence": 0.9,
        "distortion_confidence": 0.8,
    }
    result = CameraSolver(project).import_manual(
        [
            {
                "reference_id": reference["id"],
                "model": "PINHOLE",
                "width": 100,
                "height": 80,
                "intrinsics": {"fx": 90.0, "fy": 90.0, "cx": 50.0, "cy": 40.0},
                "world_from_camera": [
                    [1.0, 0.0, 0.0, 0.0],
                    [0.0, 1.0, 0.0, 500.0],
                    [0.0, 0.0, 1.0, 50.0],
                    [0.0, 0.0, 0.0, 1.0],
                ],
                "confidence": 0.95,
                "registration_class": "metric_camera_solution",
                "evidence_class": "MULTI_VIEW_OBSERVED",
                "diagnostics": {"quality": quality},
            }
        ],
        evidence_binding_ids=[scale["id"]],
    )
    assert result["approved"] is False
    receipt = export_receipt(project)
    assert "explicit human approval" in " ".join(receipt["acceptance"]["blockers"])
    approved = CameraSolver(project).approve(
        result["id"], reviewer="Camera QA", reason="Calibration residuals reviewed"
    )
    assert approved["approved"] is True
    assert approved["approval"]["reviewer"] == "Camera QA"
