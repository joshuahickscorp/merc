from __future__ import annotations

import json
from pathlib import Path

from PIL import Image

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.cameras.solver import CameraSolver
from blender_vision.core.models import RegistrationClass
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore


def make_image(path: Path, color: tuple[int, int, int] = (240, 240, 240)) -> None:
    image = Image.new("RGB", (160, 120), color)
    for x in range(45, 115):
        for y in range(30, 95):
            image.putpixel((x, y), (20, 30, 40))
    image.save(path)


def test_project_reference_and_artifact_deduplication(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Mac Studio")
    source = tmp_path / "front.png"
    make_image(source)
    ingestor = ReferenceIngestor(project)
    first = ingestor.import_file(source, rights_state="TEST_FIXTURE", viewpoint_label="front")
    second = ingestor.import_file(source, rights_state="TEST_FIXTURE", viewpoint_label="front")
    assert first["artifact"]["digest"] == second["artifact"]["digest"]
    assert second["duplicate_of"] == first["id"]
    assert project.status()["counts"]["artifacts"] == 1
    assert project.status()["counts"]["reference_items"] == 2


def test_turntable_solution_is_explicitly_low_confidence(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Calibration")
    source = tmp_path / "rear.png"
    make_image(source)
    ReferenceIngestor(project).import_file(
        source, rights_state="TEST_FIXTURE", viewpoint_label="rear"
    )
    result = CameraSolver(project).solve("turntable_fallback")
    assert result["backend"] == "turntable_fallback"
    assert result["cameras"][0]["registration_class"] == RegistrationClass.APPROXIMATE_VISUAL
    assert result["cameras"][0]["evidence_class"] == "INFERRED_LOW_CONFIDENCE"


def test_camera_solver_uses_only_acceptance_eligible_references(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Selected camera evidence")
    selected_path = tmp_path / "selected.png"
    rejected_path = tmp_path / "rejected.png"
    make_image(selected_path)
    make_image(rejected_path, color=(100, 100, 100))
    ingestor = ReferenceIngestor(project)
    selected = ingestor.import_file(
        selected_path,
        rights_state="TEST_FIXTURE",
        viewpoint_label="front",
    )
    ingestor.import_file(
        rejected_path,
        rights_state="TEST_FIXTURE",
        viewpoint_label="diagnostic",
        evidence_role="diagnostic_rejected_frame",
        acceptance_eligible=False,
    )

    result = CameraSolver(project).solve("turntable_fallback")

    assert [camera["reference_id"] for camera in result["cameras"]] == [selected["id"]]


def test_exif_initializer_uses_35mm_focal_length_without_claiming_metric_pose(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "EXIF")
    source = tmp_path / "camera.jpg"
    image = Image.new("RGB", (360, 240), "gray")
    exif = Image.Exif()
    exif[41989] = 50
    image.save(source, exif=exif)
    ReferenceIngestor(project).import_file(
        source, rights_state="TEST_FIXTURE", viewpoint_label="front"
    )

    result = CameraSolver(project).solve("exif")

    assert result["backend"] == "exif"
    assert result["cameras"][0]["intrinsics"]["fx"] == 500.0
    assert result["cameras"][0]["registration_class"] == RegistrationClass.APPROXIMATE_VISUAL
    assert result["approved"] is False


def test_receipt_is_verifiable_and_does_not_overclaim_l3(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Receipt")
    scene = tmp_path / "scene.blend"
    scene.write_bytes(b"fixture")
    SceneStore(project).import_blend(scene)
    receipt = export_receipt(project)
    path = project.root / receipt["path"]
    human_path = project.root / receipt["human_path"]
    verification = verify_receipt(path, project=project)
    assert verification["valid"] is True
    assert verification["referenced_artifacts_valid"] is True
    assert human_path.is_file()
    human = human_path.read_text(encoding="utf-8")
    assert receipt["payload_sha256"] in human
    assert "NOT ACCEPTED" in human
    assert receipt["acceptance"]["accepted"] is False
    assert receipt["acceptance"]["accepted_fidelity"] is None


def test_receipt_verification_detects_corrupt_referenced_artifact(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Corrupt artifact")
    source = tmp_path / "scene.blend"
    source.write_bytes(b"original-scene")
    scene = SceneStore(project).import_blend(source)
    receipt = export_receipt(project)
    receipt_path = project.root / receipt["path"]
    with project.connection() as connection:
        relative_path = connection.execute(
            "SELECT relative_path FROM artifacts WHERE digest=?",
            (scene["artifact"]["digest"],),
        ).fetchone()["relative_path"]
    (project.root / relative_path).write_bytes(b"corrupt-scene")

    verification = verify_receipt(receipt_path, project=project)

    assert verification["payload_valid"] is True
    assert verification["registered_artifact"] is True
    assert verification["referenced_artifacts_valid"] is False
    assert verification["missing_or_corrupt_artifacts"] == [scene["artifact"]["digest"]]
    assert verification["valid"] is False


def test_receipt_compacts_only_superseded_scene_inventories(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Historical inventories")
    scene_store = SceneStore(project)
    for index in range(2):
        source = tmp_path / f"scene-{index}.blend"
        source.write_bytes(f"fixture-{index}".encode())
        scene = scene_store.import_blend(source)
        scene_store.set_inventory(
            scene["id"],
            {
                "canonical_transform": {"scale_to_millimetres": 1000.0},
                "canonical_bounds_mm": {"dimensions": [1.0, 2.0, 3.0]},
                "audit_findings": [],
                "objects": [
                    {
                        "name": f"mesh-{item}",
                        "type": "MESH",
                        "hidden_render": False,
                    }
                    for item in range(25)
                ],
            },
        )

    receipt = export_receipt(project)
    envelope = json.loads((project.root / receipt["path"]).read_text(encoding="utf-8"))
    scenes = envelope["payload"]["evidence"]["scenes"]

    assert len(scenes[0]["inventory"]["objects"]) == 25
    assert scenes[0]["is_authoritative"] == 1
    assert scenes[1]["inventory"]["record_kind"] == "historical_inventory_summary"
    assert scenes[1]["inventory"]["object_count"] == 25
    assert len(scenes[1]["inventory"]["sha256"]) == 64
    assert "objects" not in scenes[1]["inventory"]
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True
