from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.conflicts import EvidenceConflictStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.orchestration.campaigns import CampaignStore
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.executor import AutonomousWorkflowExecutor


def _source(
    store: EvidenceAcquisitionStore,
    target_id: str,
    *,
    origin: str,
    variant: dict[str, Any],
    viewpoint: str = "front",
    **metadata: Any,
) -> dict[str, Any]:
    return store.register_source(
        target_id,
        {
            "origin": origin,
            "publisher": "Fixture owner",
            "page_title": metadata.pop("page_title", "Canonical product reference"),
            "authority_class": "user_owned",
            "target_variant": variant,
            "viewpoint": viewpoint,
            "quality_score": 0.9,
            **metadata,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": True},
        reviewed_by="Fixture owner",
    )


def _project(tmp_path: Path) -> tuple[ProjectStore, dict[str, Any]]:
    project = ProjectStore.create(tmp_path / "project", "Evidence conflicts")
    target = TargetResolver(project).resolve(
        {
            "manufacturer": "Acme",
            "model": "Widget",
            "model_year": 2026,
            "market": "North America",
            "regional_version": "US",
            "aero_package": "standard",
            "wheel_option": "19 inch",
            "factory_options": ["standard"],
        }
    )
    return project, target


def test_conflict_audit_classifies_target_and_image_incompatibilities(tmp_path: Path) -> None:
    project, target = _project(tmp_path)
    store = EvidenceAcquisitionStore(project)
    variant = target["target"]
    _source(
        store,
        target["id"],
        origin="different-variant",
        variant={**variant, "model": "Widget XL"},
    )
    _source(
        store,
        target["id"],
        origin="different-market",
        variant={**variant, "market": "Europe", "regional_version": "EU"},
    )
    _source(
        store,
        target["id"],
        origin="different-package",
        variant={**variant, "aero_package": "track"},
    )
    suspicious = _source(
        store,
        target["id"],
        origin="suspicious-image",
        variant=variant,
        page_title="Modified prototype with aftermarket body kit",
        mirrored=True,
        editing_suspicion="geometry_altered",
        cropping={"partial_object": True},
    )

    report = EvidenceConflictStore(project).audit(target["id"], record=True)
    categories = {item["category"] for item in report["findings"]}

    assert {
        "aftermarket_parts",
        "different_variant",
        "market_specific_components",
        "mirrored_image",
        "modified_product",
        "optional_package_mismatch",
        "partial_crop_without_scope",
        "prototype_vs_production",
        "severe_editing",
    } <= categories
    assert report["conflict_count"] == report["finding_count"]
    assert report["conflicts"] == report["findings"]
    assert report["merge_permitted"] is False
    assert report["canonical_merge_permitted"] is False
    assert report["source_eligibility"][suspicious["id"]]["coverage_eligible"] is False
    assert (project.root / report["path"]).is_file()


def test_blocking_conflict_is_excluded_from_coverage_and_pauses_autonomy(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    store = EvidenceAcquisitionStore(project)
    source = _source(
        store,
        target["id"],
        origin="wrong-front",
        variant={**target["target"], "model": "Widget XL"},
    )
    image = tmp_path / "front.png"
    Image.new("RGB", (120, 80), "gray").save(image)

    acquired = store.acquire_local(source["id"], image)
    coverage = store.analyze_coverage(target["id"])
    campaign = CampaignStore(project).start(
        "external_public_benchmark",
        configuration={"target_id": target["id"]},
        resource_profile="compact",
    )
    result = AutonomousWorkflowExecutor(project).continue_once(campaign["id"])

    assert acquired["conflict_audit"]["unresolved_blocking_count"] == 1
    assert coverage["acquired_count"] == 1
    assert coverage["eligible_acquired_count"] == 0
    assert coverage["directions"]["front"] == []
    assert result["workflow_state"] == "EVIDENCE_CONFLICT_REVIEW_REQUIRED"
    assert result["campaign"]["status"] == "PAUSED"
    reference = {item["id"]: item for item in ReferenceIngestor(project).list()}[
        acquired["reference"]["id"]
    ]
    assert reference["acceptance_eligible"] is False


def test_review_is_finding_bound_and_reactivation_requires_governed_acquisition(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    store = EvidenceAcquisitionStore(project)
    source = _source(
        store,
        target["id"],
        origin="confirmed-front",
        variant=target["target"],
        page_title="Modified product photograph",
    )
    image = tmp_path / "front.png"
    Image.new("RGB", (120, 80), "gray").save(image)
    acquired = store.acquire_local(source["id"], image)

    reviewed = EvidenceConflictStore(project).review(
        source["id"],
        "modified_product",
        decision="CANONICAL_MATCH_CONFIRMED",
        reviewer="Variant resolver",
        reason="Inspection confirms the word modified describes the article, not the unit.",
    )
    references = {item["id"]: item for item in ReferenceIngestor(project).list()}
    assert references[acquired["reference"]["id"]]["acceptance_eligible"] is True
    assert reviewed["updated_conflict_audit"]["unresolved_blocking_count"] == 0

    with project.connection() as connection:
        row = connection.execute(
            "SELECT source_json FROM evidence_sources WHERE id=?", (source["id"],)
        ).fetchone()
        changed = json.loads(row["source_json"])
        changed["page_title"] = "Customized product photograph"
        connection.execute(
            "UPDATE evidence_sources SET source_json=? WHERE id=?",
            (json.dumps(changed), source["id"]),
        )

    stale = EvidenceConflictStore(project).audit(target["id"], record=False)
    finding = next(
        item for item in stale["findings"] if item["category"] == "modified_product"
    )
    assert finding["review"] is None
    assert finding["status"] == "UNRESOLVED"
    assert stale["unresolved_blocking_count"] == 1


def test_configuration_branch_is_explicit_and_never_enters_canonical_merge(
    tmp_path: Path,
) -> None:
    project, target = _project(tmp_path)
    source = _source(
        EvidenceAcquisitionStore(project),
        target["id"],
        origin="track-package",
        variant={**target["target"], "aero_package": "track"},
    )
    conflicts = EvidenceConflictStore(project)

    with pytest.raises(ValueError, match="requires id, description, and target overrides"):
        conflicts.review(
            source["id"],
            "optional_package_mismatch",
            decision="CONFIGURATION_BRANCH",
            reviewer="Variant resolver",
            reason="Separate factory package.",
        )
    reviewed = conflicts.review(
        source["id"],
        "optional_package_mismatch",
        decision="CONFIGURATION_BRANCH",
        reviewer="Variant resolver",
        reason="Separate documented factory package.",
        configuration_model={
            "id": "track-package",
            "description": "Track aero package branch",
            "target_overrides": {"aero_package": "track"},
        },
    )
    state = reviewed["updated_conflict_audit"]["source_eligibility"][source["id"]]

    assert state["configuration_branch_id"] == "track-package"
    assert state["acceptance_eligible"] is False
    assert state["coverage_eligible"] is False
    assert reviewed["updated_conflict_audit"]["canonical_merge_permitted"] is True
    receipt = export_receipt(project)
    assert receipt["acceptance"]["metrics"]["evidence_conflicts"]["review_count"] == 1
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_camera_and_crop_inconsistencies_warn_without_claiming_authority(tmp_path: Path) -> None:
    project, target = _project(tmp_path)
    store = EvidenceAcquisitionStore(project)
    sources = [
        _source(store, target["id"], origin=f"front-{index}", variant=target["target"])
        for index in range(2)
    ]
    paths = [tmp_path / "wide.png", tmp_path / "square.png"]
    Image.new("RGB", (200, 100), "gray").save(paths[0])
    Image.new("RGB", (100, 100), "gray").save(paths[1])
    acquired = [
        store.acquire_local(source["id"], path)
        for source, path in zip(sources, paths, strict=True)
    ]
    with project.connection() as connection:
        for item, focal in zip(acquired, (10, 100), strict=True):
            row = connection.execute(
                "SELECT metadata_json FROM reference_items WHERE id=?",
                (item["reference"]["id"],),
            ).fetchone()
            metadata = json.loads(row["metadata_json"])
            metadata["lens"] = {"FocalLengthIn35mmFilm": focal}
            connection.execute(
                "UPDATE reference_items SET metadata_json=? WHERE id=?",
                (json.dumps(metadata), item["reference"]["id"]),
            )

    report = EvidenceConflictStore(project).audit(target["id"], record=False)
    warning_categories = {
        item["category"] for item in report["findings"] if item["severity"] == "WARNING"
    }

    assert {"crop_inconsistency", "lens_inconsistency", "perspective_distortion"} <= (
        warning_categories
    )
    assert report["unresolved_blocking_count"] == 0
    assert report["unresolved_warning_count"] >= 3
    assert report["canonical_merge_permitted"] is True
    assert report["policy"]["warnings_establish_camera_or_geometry_authority"] is False
