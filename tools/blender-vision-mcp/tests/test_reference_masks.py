from __future__ import annotations

import json
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.service import ReconstructionService


def test_named_reviewed_mask_becomes_hash_bound_high_confidence_evidence(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Reviewed mask")
    reference_path = tmp_path / "opaque-reference.png"
    Image.new("RGB", (80, 80), (45, 45, 45)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="INTERNAL", viewpoint_label="front"
    )
    mask_path = tmp_path / "reviewed-mask.png"
    mask = Image.new("L", (80, 80), 0)
    ImageDraw.Draw(mask).rectangle((20, 18, 60, 62), fill=255)
    mask.save(mask_path)
    reviewed = ReferenceMaskStore(project).import_reviewed(
        reference["id"],
        mask_path,
        reviewer="Silhouette QA",
        reason="Object boundary traced and checked against the opaque source",
    )
    render_path = project.root / "renders" / "reviewed-mask-render.png"
    render = Image.new("RGBA", (80, 80), (0, 0, 0, 0))
    ImageDraw.Draw(render).rectangle((20, 18, 60, 62), fill=(220, 220, 220, 255))
    render.save(render_path)
    render_artifact = ArtifactStore(project).ingest_file(render_path, media_type="image/png")

    result = ReconstructionService(project).compare_views(
        [
            {
                "reference_id": reference["id"],
                "relative_path": str(render_path.relative_to(project.root)),
                "artifact": render_artifact.to_dict(),
            }
        ]
    )["comparisons"][0]

    assert result["metrics"]["silhouette_iou"] == 1.0
    assert result["metrics"]["reference_segmentation"] == "reviewed_manual_mask"
    assert result["metrics"]["reference_segmentation_confidence"] == "high"
    assert result["metrics"]["reference_mask"]["id"] == reviewed["id"]
    receipt = export_receipt(project)
    assert "silhouette comparison requires high-confidence reference masks" not in receipt[
        "acceptance"
    ]["blockers"]
    envelope = json.loads((project.root / receipt["path"]).read_text(encoding="utf-8"))
    assert envelope["payload"]["evidence"]["reference_masks"][0]["reviewer"] == "Silhouette QA"


def test_reviewed_mask_must_match_reference_dimensions(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Mask dimensions")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (80, 80), "white").save(reference_path)
    reference = ReferenceIngestor(project).import_file(reference_path, rights_state="INTERNAL")
    mask_path = tmp_path / "wrong-size.png"
    Image.new("L", (79, 80), 0).save(mask_path)

    artifact_count = project.status()["counts"]["artifacts"]
    with pytest.raises(ValueError, match="dimensions"):
        ReferenceMaskStore(project).import_reviewed(
            reference["id"], mask_path, reviewer="QA", reason="Dimension validation fixture"
        )
    assert project.status()["counts"]["artifacts"] == artifact_count


def test_open_migrates_reference_mask_storage_for_existing_project(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Mask migration")
    mask_directory = project.root / "references" / "masks"
    mask_directory.rmdir()

    reopened = ProjectStore.open(project.root)

    assert (reopened.root / "references" / "masks").is_dir()
    assert reopened.status()["counts"]["reference_masks"] == 0
