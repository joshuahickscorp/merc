from __future__ import annotations

from pathlib import Path

from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import FidelityLevel
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path, safe_mode


def make_project(tmp_path: Path) -> ProjectStore:
    return ProjectStore.create(
        tmp_path / "project", "Test Product", target_fidelity=FidelityLevel.L3
    )


def test_project_is_portable_and_uses_wal(tmp_path: Path) -> None:
    project = make_project(tmp_path)
    metadata = project.project()
    assert metadata["canonical_units"] == "millimetres"
    assert metadata["coordinate_system"] == {"handedness": "right", "up_axis": "Z"}
    with project.connection() as connection:
        assert connection.execute("PRAGMA journal_mode").fetchone()[0] == "wal"


def test_artifacts_are_content_addressed_and_deduplicated(tmp_path: Path) -> None:
    project = make_project(tmp_path)
    source = tmp_path / "evidence.bin"
    source.write_bytes(b"same evidence")
    store = ArtifactStore(project)
    first = store.ingest_file(source)
    second = store.ingest_file(source)
    assert first.digest == second.digest
    assert store.path_for(first.digest).read_bytes() == b"same evidence"
    assert project.status()["counts"]["artifacts"] == 1


def test_reference_ingestion_preserves_original_and_quality(tmp_path: Path) -> None:
    project = make_project(tmp_path)
    source = tmp_path / "front.png"
    Image.new("RGB", (80, 60), (120, 130, 140)).save(source)
    ingestor = ReferenceIngestor(project)
    first = ingestor.import_file(source, rights_state="INTERNAL", viewpoint_label="front")
    second = ingestor.import_file(source, rights_state="INTERNAL", viewpoint_label="front")
    assert first["metadata"]["width"] == 80
    assert first["quality"]["decode_ok"] is True
    assert second["duplicate_of"] == first["id"]
    assert (project.root / first["relative_path"]).read_bytes() == source.read_bytes()


def test_reference_ingestion_labels_public_3d_formats_as_non_acceptance_source_media(
    tmp_path: Path,
) -> None:
    project = make_project(tmp_path)
    source = tmp_path / "public-reference.glb"
    source.write_bytes(b"glTF fixture")

    imported = ReferenceIngestor(project).import_file(
        source,
        rights_state="PUBLIC_REUSABLE",
        viewpoint_label="public 3D landmark reference",
    )

    assert imported["media_type"] == "model/gltf-binary"
    assert imported["evidence_role"] == "source_media"
    assert imported["acceptance_eligible"] is False


def test_open_migrates_legacy_octet_stream_3d_reference_media_type(tmp_path: Path) -> None:
    project = make_project(tmp_path)
    source = tmp_path / "legacy.glb"
    source.write_bytes(b"legacy glTF fixture")
    imported = ReferenceIngestor(project).import_file(source, rights_state="PUBLIC_REUSABLE")
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_items SET media_type='application/octet-stream' WHERE id=?",
            (imported["id"],),
        )
        connection.execute(
            "UPDATE artifacts SET media_type='application/octet-stream' WHERE digest=?",
            (imported["artifact"]["digest"],),
        )

    reopened = ProjectStore.open(project.root)

    reference = next(
        item
        for item in ReferenceIngestor(reopened).list()
        if item["id"] == imported["id"]
    )
    with reopened.connection() as connection:
        artifact_type = connection.execute(
            "SELECT media_type FROM artifacts WHERE digest=?", (imported["artifact"]["digest"],)
        ).fetchone()[0]
    assert reference["media_type"] == "model/gltf-binary"
    assert artifact_type == "model/gltf-binary"


def test_safe_mode_and_path_confinement_are_default(tmp_path: Path) -> None:
    project = make_project(tmp_path)
    assert safe_mode() is True
    assert confined_path(project.root, project.root / "scene") == project.root / "scene"
