from __future__ import annotations

from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import export_receipt
from blender_vision.core.models import EvidenceClass
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.materials.store import MaterialStore
from blender_vision.projects.store import ProjectStore
from blender_vision.review.service import ReviewService


def test_material_profiles_are_appearance_only_and_named_reviewed(tmp_path: Path) -> None:
    project = ProjectStore.create(
        tmp_path / "project",
        "Materials",
        metadata={"required_material_regions": ["outer-shell"]},
    )
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (16, 16), (128, 132, 140)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="multi-light-left"
    )
    store = MaterialStore(project)
    profile = store.create(
        "outer-shell",
        {
            "base_color": [0.5, 0.52, 0.55, 1.0],
            "roughness": 0.32,
            "metallic": 0.88,
            "anisotropy": 0.15,
            "clearcoat": 0.08,
            "normal_detail": {"kind": "procedural_brush", "scale_mm": 0.04},
            "procedural_texture": {"kind": "brushed_aluminium"},
        },
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
        confidence=0.91,
        reference_ids=[reference["id"]],
        multi_light_reference_ids=[reference["id"]],
        color_calibration={"state": "calibrated", "space": "sRGB"},
        lighting_estimate={"state": "estimated", "method": "multi-light"},
    )
    assert profile["authority"]["may_establish_geometry"] is False
    assert profile["properties"]["normal_detail"]["scale_mm"] == 0.04
    assert ReviewService(project).review_queue()[0]["kind"] == "material"

    approved = store.review(
        profile["id"],
        approved=True,
        reviewer="Appearance Reviewer",
        reason="Calibrated multi-light evidence supports the recorded response",
    )
    assert approved["status"] == "approved"
    assert approved["approval"]["reviewer"] == "Appearance Reviewer"
    receipt = export_receipt(project)
    appearance = receipt["acceptance"]["metrics"]["appearance"]
    assert appearance["approved_regions"] == ["outer-shell"]
    assert appearance["geometry_separate_from_rgb"] is True


def test_material_approval_requires_bound_evidence(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Materials")
    store = MaterialStore(project)
    profile = store.create(
        "unseen-bottom",
        {"base_color": [0.1, 0.1, 0.1], "roughness": 0.6},
        evidence_class=EvidenceClass.UNSEEN,
        confidence=0.1,
    )
    with pytest.raises(ValueError, match="bound evidence"):
        store.review(profile["id"], approved=True, reviewer="Reviewer", reason="No source exists")


def test_translucent_material_requires_lighting_and_multilight_authority(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Translucent materials")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (16, 16), (180, 200, 220)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path,
        rights_state="SYNTHETIC_OWNED",
        viewpoint_label="front",
    )
    store = MaterialStore(project)
    profile = store.create(
        "frosted-core",
        {
            "material_class": "translucent",
            "base_color": [0.7, 0.8, 0.9, 0.72],
            "roughness": 0.38,
            "transmission": 0.64,
            "alpha": 0.72,
            "ior": 1.46,
            "reflectance_constraints": {
                "highlight_width": "broad",
                "background_response": "transmitted_and_blurred",
            },
        },
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
        confidence=0.9,
        reference_ids=[reference["id"]],
        uncertainty={"transmission": 0.08, "ior": 0.04},
        lighting_estimate={
            "state": "estimated",
            "method": "multi-light response",
            "confidence": 0.82,
            "uncertainty": {"intensity_fraction": 0.1},
        },
    )
    with pytest.raises(ValueError, match="multi-light"):
        store.review(
            profile["id"],
            approved=True,
            reviewer="Appearance Reviewer",
            reason="One view is insufficient for transmission",
        )

    governed = store.create(
        "frosted-core-governed",
        profile["properties"],
        evidence_class=EvidenceClass.MULTI_VIEW_OBSERVED,
        confidence=0.9,
        reference_ids=[reference["id"]],
        multi_light_reference_ids=[reference["id"]],
        uncertainty={"transmission": 0.08, "ior": 0.04},
        lighting_estimate={
            "state": "estimated",
            "method": "multi-light response",
            "confidence": 0.82,
        },
    )
    approved = store.review(
        governed["id"],
        approved=True,
        reviewer="Appearance Reviewer",
        reason="The bound multi-light evidence supports the declared class and uncertainty",
    )

    assert approved["properties"]["material_class"] == "translucent"
    assert approved["properties"]["transmission"] == 0.64
    assert approved["properties"]["ior"] == 1.46


def test_emissive_material_rejects_unreported_lighting(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Emissive materials")
    reference_path = tmp_path / "reference.png"
    Image.new("RGB", (16, 16), (255, 96, 16)).save(reference_path)
    reference = ReferenceIngestor(project).import_file(
        reference_path,
        rights_state="SYNTHETIC_OWNED",
    )
    profile = MaterialStore(project).create(
        "eclipse-disk",
        {
            "material_class": "emissive",
            "base_color": [0.1, 0.02, 0.0, 1.0],
            "emission_color": [1.0, 0.12, 0.01, 1.0],
            "emission_strength": 12.0,
        },
        evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
        confidence=0.7,
        reference_ids=[reference["id"]],
    )

    with pytest.raises(ValueError, match="lighting estimate"):
        MaterialStore(project).review(
            profile["id"],
            approved=True,
            reviewer="Appearance Reviewer",
            reason="Exposure authority is absent",
        )


def test_material_class_cannot_replace_required_physical_parameters(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Material class consistency")
    store = MaterialStore(project)

    with pytest.raises(ValueError, match="positive transmission"):
        store.create(
            "fake-glass",
            {
                "material_class": "transparent",
                "base_color": [0.8, 0.9, 1.0, 0.5],
                "transmission": 0.0,
            },
            evidence_class=EvidenceClass.UNSEEN,
            confidence=0.1,
        )
    with pytest.raises(ValueError, match="positive emission_strength"):
        store.create(
            "fake-emission",
            {
                "material_class": "emissive",
                "base_color": [1.0, 0.1, 0.0, 1.0],
                "emission_strength": 0.0,
            },
            evidence_class=EvidenceClass.UNSEEN,
            confidence=0.1,
        )
