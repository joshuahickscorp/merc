import shutil
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.capture.service import CaptureService
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore


def test_frame_selection_is_deterministic_and_quality_ranked(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Capture")
    flat = tmp_path / "flat.png"
    detailed = tmp_path / "detailed.png"
    Image.new("RGB", (64, 64), (128, 128, 128)).save(flat)
    image = Image.new("RGB", (128, 128), (128, 128, 128))
    draw = ImageDraw.Draw(image)
    for y in range(0, 128, 8):
        for x in range(0, 128, 8):
            if (x // 8 + y // 8) % 2:
                draw.rectangle((x, y, x + 7, y + 7), fill=(64, 64, 64))
            else:
                draw.rectangle((x, y, x + 7, y + 7), fill=(192, 192, 192))
    image.save(detailed)
    ingestor = ReferenceIngestor(project)
    flat_reference = ingestor.import_file(flat, rights_state="SYNTHETIC_OWNED")
    detailed_reference = ingestor.import_file(detailed, rights_state="SYNTHETIC_OWNED")

    first = CaptureService(project).select_frames(maximum_selected=1)
    second = CaptureService(project).select_frames(maximum_selected=1)

    assert first == second
    assert first["selected_reference_ids"] == [detailed_reference["id"]]
    assert first["rejected"][0]["reference_id"] == flat_reference["id"]


def test_measurement_linking_requires_named_provenance(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Links")
    image_path = tmp_path / "calibration.png"
    Image.new("RGB", (32, 32), "white").save(image_path)
    reference = ReferenceIngestor(project).import_file(image_path, rights_state="SYNTHETIC_OWNED")
    store = MeasurementStore(project)
    measurement = store.add(
        "known_overall_dimension",
        {"axis": "x", "millimetres": 100.0},
        evidence_class=EvidenceClass.MEASURED,
    )

    linked = store.link(
        measurement["id"],
        [reference["id"]],
        linked_by="Calibration QA",
        reason="Scale marker and object share the focal plane",
    )

    assert linked["value"]["reference_ids"] == [reference["id"]]
    assert linked["value"]["link_history"][0]["linked_by"] == "Calibration QA"


def test_video_only_evidence_has_a_specific_acceptance_blocker(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Video only")
    video = tmp_path / "capture.mp4"
    video.write_bytes(b"not-a-real-video")
    ReferenceIngestor(project).import_file(video, rights_state="SYNTHETIC_OWNED")

    receipt = export_receipt(project)

    assert "no renderable image reference evidence" in receipt["acceptance"]["blockers"]


@pytest.mark.skipif(shutil.which("pdftoppm") is None, reason="Poppler is not installed")
def test_pdf_pages_are_source_linked_without_rewriting_the_document(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "PDF")
    document = tmp_path / "technical-drawing.pdf"
    first = Image.new("RGB", (80, 60), "white")
    second = Image.new("RGB", (80, 60), "gray")
    first.save(document, save_all=True, append_images=[second])

    imported = ReferenceIngestor(project).import_pdf_pages(
        document,
        rights_state="SYNTHETIC_OWNED",
        maximum_pages=10,
        resolution_dpi=72,
    )

    assert imported["page_count"] == 2
    assert all(
        page["metadata"]["document_source_reference_id"]
        == imported["source_reference"]["id"]
        for page in imported["pages"]
    )
    assert document.is_file()
