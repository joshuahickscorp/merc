from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw, ImageOps

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.duplicates import EvidenceDuplicateStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore


def _register(
    store: EvidenceAcquisitionStore,
    target_id: str,
    *,
    origin: str,
    viewpoint: str,
    quality_score: float,
) -> dict:
    return store.register_source(
        target_id,
        {
            "origin": origin,
            "publisher": "Fixture owner",
            "page_title": "Canonical Widget",
            "authority_class": "user_owned",
            "target_variant": {"manufacturer": "Acme", "model": "Widget"},
            "viewpoint": viewpoint,
            "quality_score": quality_score,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )


def _asymmetric_image() -> Image.Image:
    image = Image.new("RGB", (160, 100), "black")
    draw = ImageDraw.Draw(image)
    draw.rectangle((5, 10, 55, 35), fill="white")
    draw.rectangle((100, 50, 150, 90), fill="gray")
    draw.polygon(((70, 5), (95, 30), (60, 80)), fill="white")
    return image


def test_exact_perceptual_and_mirrored_copies_form_one_canonical_group(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Duplicate evidence")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    sources = [
        _register(
            store,
            target["id"],
            origin="canonical-front",
            viewpoint="front",
            quality_score=1.0,
        ),
        _register(
            store,
            target["id"],
            origin="recompressed-rear-label",
            viewpoint="rear",
            quality_score=0.8,
        ),
        _register(
            store,
            target["id"],
            origin="mirrored-right-label",
            viewpoint="right",
            quality_score=0.7,
        ),
        _register(
            store,
            target["id"],
            origin="exact-copy-left-label",
            viewpoint="left",
            quality_score=0.6,
        ),
    ]
    original = _asymmetric_image()
    paths = [
        tmp_path / "original.png",
        tmp_path / "recompressed.jpg",
        tmp_path / "mirrored.png",
        tmp_path / "exact-copy.png",
    ]
    original.save(paths[0])
    original.resize((120, 75)).save(paths[1], quality=90)
    ImageOps.mirror(original).save(paths[2])
    original.save(paths[3])
    acquired = [
        store.acquire_local(source["id"], path)
        for source, path in zip(sources, paths, strict=True)
    ]

    report = EvidenceDuplicateStore(project).audit(target["id"], record=True)
    group = report["duplicate_groups"][0]
    canonical_id = sources[0]["id"]

    assert report["source_count"] == 4
    assert report["unique_media_count"] == 1
    assert report["duplicate_group_count"] == 1
    assert group["canonical_source_id"] == canonical_id
    assert set(group["relationships"]) == {
        "EXACT_DUPLICATE",
        "MIRRORED_DUPLICATE",
        "PERCEPTUAL_DUPLICATE",
    }
    assert sum(
        state["independent_evidence_eligible"]
        for state in report["source_eligibility"].values()
    ) == 1
    coverage = store.analyze_coverage(target["id"])
    assert coverage["eligible_acquired_count"] == 1
    assert coverage["directions"]["front"] == ["canonical-front"]
    assert coverage["directions"]["rear"] == []
    assert coverage["directions"]["right"] == []
    assert coverage["directions"]["left"] == []

    references = {item["id"]: item for item in ReferenceIngestor(project).list()}
    assert references[acquired[0]["reference"]["id"]]["acceptance_eligible"] is True
    for item in acquired[1:]:
        assert references[item["reference"]["id"]]["acceptance_eligible"] is False
    assert (project.root / report["path"]).is_file()

    receipt = export_receipt(project)
    metrics = receipt["acceptance"]["metrics"]["evidence_duplicates"]
    assert metrics["duplicate_group_count"] == 1
    assert metrics["run_count"] >= 1
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_distinct_images_remain_independent_and_threshold_is_bounded(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Distinct evidence")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    first = _register(
        store,
        target["id"],
        origin="front",
        viewpoint="front",
        quality_score=1.0,
    )
    second = _register(
        store,
        target["id"],
        origin="rear",
        viewpoint="rear",
        quality_score=1.0,
    )
    first_path, second_path = tmp_path / "first.png", tmp_path / "second.png"
    _asymmetric_image().save(first_path)
    rotated = _asymmetric_image().rotate(90, expand=True)
    rotated.save(second_path)
    store.acquire_local(first["id"], first_path)
    store.acquire_local(second["id"], second_path)

    report = EvidenceDuplicateStore(project).audit(
        target["id"], record=False, maximum_hamming_distance=0
    )

    assert report["duplicate_group_count"] == 0
    assert report["unique_media_count"] == 2
