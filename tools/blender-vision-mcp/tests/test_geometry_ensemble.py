from __future__ import annotations

from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.vision.store import GeometryEvidenceStore


def _reference(path: Path) -> None:
    image = Image.new("RGBA", (96, 72), (255, 255, 255, 0))
    ImageDraw.Draw(image).rounded_rectangle((18, 12, 78, 62), radius=8, fill=(30, 30, 30, 255))
    image.save(path)


def test_silhouette_geometry_run_preserves_unresolved_scale(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Geometry")
    source = tmp_path / "reference.png"
    _reference(source)
    reference = ReferenceIngestor(project).import_file(
        source, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )

    run = GeometryPipeline(project).run("silhouette")

    assert run["backend"] == "silhouette"
    assert run["commercial_eligible"] is True
    assert run["evidence"]["scale_factor"] is None
    assert run["evidence"]["uncertainty"]["metric_authority"] is False
    assert len(run["evidence"]["mask_artifacts"]) == 1
    assert run["evidence"]["diagnostics"]["masks"][0]["reference_id"] == reference["id"]
    assert (
        GeometryEvidenceStore(project).get(run["id"])["artifact_digest"]
        == run["artifact"]["digest"]
    )


def test_backend_consensus_excludes_research_run_from_release_authority(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Consensus")
    source = tmp_path / "reference.png"
    _reference(source)
    ReferenceIngestor(project).import_file(source, rights_state="SYNTHETIC_OWNED")
    commercial = GeometryPipeline(project).run("silhouette")

    points = tmp_path / "points.ply"
    points.write_text("ply\nformat ascii 1.0\nelement vertex 0\nend_header\n", encoding="utf-8")
    point_artifact = ArtifactStore(project).ingest_file(points, media_type="model/ply")
    research = GeometryPipeline(project).import_external(
        backend="research-depth",
        backend_version="checkpoint-1",
        evidence={
            "point_artifacts": [point_artifact.digest],
            "diagnostics": {"fixture": True},
            "source_frame": "research_backend",
            "uncertainty": {"scale": "unresolved"},
        },
        evidence_class="MULTI_VIEW_OBSERVED",
        license_record={
            "license": "research-only-test",
            "commercial_use": False,
            "research_only": True,
            "checkpoint_required": True,
            "weight_hash": "a" * 64,
        },
    )

    consensus = GeometryPipeline(project).compare([commercial["id"], research["id"]])

    assert consensus["report"]["averaging_performed"] is False
    assert consensus["report"]["selected_authority_run_id"] == commercial["id"]
    assert consensus["report"]["commercial_release_exclusions"] == [
        {
            "run_id": research["id"],
            "reason": "backend evidence is research-only or lacks commercial-use clearance",
        }
    ]


def test_external_geometry_rejects_unregistered_artifacts(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Evidence binding")
    with pytest.raises(ValueError, match="unknown artifact"):
        GeometryPipeline(project).import_external(
            backend="external",
            backend_version="1",
            evidence={"depth_artifacts": ["f" * 64]},
            evidence_class="SINGLE_VIEW_OBSERVED",
            license_record={"license": "Apache-2.0", "commercial_use": True},
        )
