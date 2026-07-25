from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import evaluate_acceptance, export_receipt, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, sha256_file
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.adoption import LegacyReferenceAdoptionStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore
from blender_vision.review import ReviewService


def _fixture_project(tmp_path: Path) -> tuple[ProjectStore, dict, list[dict]]:
    project = ProjectStore.create(tmp_path / "project", "Legacy reference adoption")
    target = TargetResolver(project).resolve(
        {"manufacturer": "Fixture", "model": "Governed Twin"}
    )
    ingestor = ReferenceIngestor(project)
    references = []
    for index, color in enumerate(((180, 20, 40), (20, 80, 180))):
        path = tmp_path / f"legacy-{index}.png"
        Image.new("RGB", (96, 64), color).save(path)
        references.append(
            ingestor.import_file(
                path,
                rights_state="LEGACY_INTERNAL_LABEL",
                viewpoint_label=("front", "rear")[index],
            )
        )
    return project, target, references


def _source(reference: dict) -> dict:
    return {
        "origin": f"fixture archive/{reference['original_name']}",
        "publisher": "Fixture owner",
        "page_title": reference["original_name"],
        "authority_class": "user_owned_archive",
        "target_variant": {"manufacturer": "Fixture", "model": "Governed Twin"},
        "viewpoint": reference["viewpoint_label"],
        "quality_score": 0.9,
    }


def test_legacy_references_require_named_adoption_or_exclusion(tmp_path: Path) -> None:
    project, target, references = _fixture_project(tmp_path)
    store = LegacyReferenceAdoptionStore(project)

    proposed = store.propose_orphans(target["id"])
    repeated = store.propose_orphans(target["id"])
    assert proposed["proposal_count"] == 2
    assert [item["id"] for item in repeated["proposals"]] == [
        item["id"] for item in proposed["proposals"]
    ]
    assert all(item["proposal"]["reference"]["artifact_digest"] for item in proposed["proposals"])
    assert project.status()["counts"]["evidence_sources"] == 0
    queue = ReviewService(project).review_queue()
    assert [item["kind"] for item in queue] == [
        "reference_adoption",
        "reference_adoption",
    ]
    assert all(item["image_url"].startswith("/artifact/") for item in queue)
    assert all(item["canonical_target"]["model"] == "Governed Twin" for item in queue)

    first = proposed["proposals"][0]
    mismatched_source = _source(references[0])
    mismatched_source["target_variant"]["model"] = "Different product"
    with pytest.raises(ValueError, match="resolved manufacturer and model"):
        store.review(
            first["id"],
            decision="ADOPT",
            reviewer="Named evidence reviewer",
            reason="The archive ownership and target identity were checked",
            source=mismatched_source,
            rights={
                "status": "USER_OWNED",
                "internal_use": True,
                "redistribution": False,
            },
            source_terms_review="user_owned",
            privacy_review="not_applicable",
        )
    with pytest.raises(ValueError, match="terms and privacy"):
        store.review(
            first["id"],
            decision="ADOPT",
            reviewer="Named evidence reviewer",
            reason="The archive ownership and target identity were checked",
            source=_source(references[0]),
            rights={
                "status": "USER_OWNED",
                "internal_use": True,
                "redistribution": False,
            },
        )
    adopted = store.review(
        first["id"],
        decision="ADOPT",
        reviewer="Named evidence reviewer",
        reason="The archive ownership and target identity were checked",
        source=_source(references[0]),
        rights={
            "status": "USER_OWNED",
            "internal_use": True,
            "redistribution": False,
        },
        source_terms_review="user_owned",
        privacy_review="not_applicable",
    )
    assert adopted["status"] == "ADOPTED"
    assert store.get(first["id"], verify=True)["source_id"] == adopted["source_id"]
    assert EvidenceAcquisitionStore(project).authority_status(adopted["source_id"])[
        "valid"
    ] is True

    second = proposed["proposals"][1]
    excluded = ReviewService(project).action(
        "reference_adoption.review",
        {
            "id": second["id"],
            "decision": "EXCLUDE",
            "reviewer": "Named evidence reviewer",
            "reason": "Original source identity cannot be established",
        },
    )
    assert excluded["status"] == "EXCLUDED"
    assert store.get(second["id"], verify=True)["source_id"] is None

    with project.connection() as connection:
        source = connection.execute(
            "SELECT reference_id,source_json FROM evidence_sources WHERE id=?",
            (adopted["source_id"],),
        ).fetchone()
    source_record = json.loads(source["source_json"])
    assert source["reference_id"] == references[0]["id"]
    assert source_record["content_hash"] == references[0]["artifact"]["digest"]
    assert sha256_file(project.root / references[0]["relative_path"])[0] == source_record[
        "content_hash"
    ]
    persisted = {item["id"]: item for item in ReferenceIngestor(project).list()}
    assert len(persisted) == 2
    assert persisted[references[1]["id"]]["acceptance_eligible"] is False

    receipt = export_receipt(project)
    adoption_metrics = receipt["acceptance"]["metrics"]["legacy_reference_adoption"]
    assert adoption_metrics["status_counts"] == {
        "PROPOSED": 0,
        "ADOPTED": 1,
        "EXCLUDED": 1,
    }
    assert adoption_metrics["orphan_renderable_reference_ids"] == []
    assert verify_receipt(project.root / receipt["path"], project=project)["valid"] is True


def test_tampered_adoption_review_is_an_acceptance_blocker(tmp_path: Path) -> None:
    project, target, references = _fixture_project(tmp_path)
    store = LegacyReferenceAdoptionStore(project)
    proposal = store.propose(target["id"], references[0]["id"])
    store.review(
        proposal["id"],
        decision="ADOPT",
        reviewer="Original reviewer",
        reason="Reviewed against the owner archive",
        source=_source(references[0]),
        rights={
            "status": "USER_OWNED",
            "internal_use": True,
            "redistribution": False,
        },
        source_terms_review="user_owned",
        privacy_review="not_applicable",
    )
    with project.connection() as connection:
        row = connection.execute(
            "SELECT decision_json FROM reference_adoption_proposals WHERE id=?",
            (proposal["id"],),
        ).fetchone()
        decision = json.loads(row["decision_json"])
        decision["reviewer"] = "Forged reviewer"
        connection.execute(
            "UPDATE reference_adoption_proposals SET decision_json=? WHERE id=?",
            (json.dumps(decision), proposal["id"]),
        )

    acceptance = export_receipt(project)["acceptance"]
    assert acceptance["metrics"]["legacy_reference_adoption"][
        "invalid_receipt_ids"
    ] == [proposal["id"]]
    assert "one or more legacy reference adoption receipts are invalid" in acceptance[
        "blockers"
    ]


def test_hash_valid_adoption_semantic_forgeries_are_acceptance_blockers(
    tmp_path: Path,
) -> None:
    project, target, references = _fixture_project(tmp_path)
    store = LegacyReferenceAdoptionStore(project)
    proposals = store.propose_orphans(target["id"])["proposals"]
    adopted = store.review(
        proposals[0]["id"],
        decision="ADOPT",
        reviewer="Named evidence reviewer",
        reason="The owner archive and exact target identity were checked",
        source=_source(references[0]),
        rights={
            "status": "USER_OWNED",
            "internal_use": True,
            "redistribution": False,
        },
        source_terms_review="user_owned",
        privacy_review="not_applicable",
    )
    store.review(
        proposals[1]["id"],
        decision="EXCLUDE",
        reviewer="Named evidence reviewer",
        reason="The original source identity cannot be established",
    )
    artifacts = ArtifactStore(project)

    adopted_record = store.get(proposals[0]["id"])
    forged_adoption = json.loads(json.dumps(adopted_record["decision"]))
    forged_adoption["source"]["target_variant"] = {}
    adopted_path = project.root / "receipts" / "hash-valid-forged-adoption.json"
    atomic_write_json(adopted_path, forged_adoption)
    adopted_artifact = artifacts.ingest_file(
        adopted_path,
        media_type="application/vnd.bvmcp.legacy-reference-adoption-review+json",
    )
    persisted_source = {
        **forged_adoption["source"],
        "adoption_proposal_id": proposals[0]["id"],
        "adoption_decision_digest": adopted_artifact.digest,
    }
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_adoption_proposals SET decision_json=?,decision_digest=? "
            "WHERE id=?",
            (
                json.dumps(forged_adoption),
                adopted_artifact.digest,
                proposals[0]["id"],
            ),
        )
        connection.execute(
            "UPDATE evidence_sources SET source_json=? WHERE id=?",
            (json.dumps(persisted_source), adopted["source_id"]),
        )

    excluded_record = store.get(proposals[1]["id"])
    forged_exclusion = json.loads(json.dumps(excluded_record["decision"]))
    forged_exclusion["source_id"] = "hidden-forged-source-id"
    excluded_path = project.root / "receipts" / "hash-valid-forged-exclusion.json"
    atomic_write_json(excluded_path, forged_exclusion)
    excluded_artifact = artifacts.ingest_file(
        excluded_path,
        media_type="application/vnd.bvmcp.legacy-reference-adoption-review+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_adoption_proposals SET decision_json=?,decision_digest=? "
            "WHERE id=?",
            (
                json.dumps(forged_exclusion),
                excluded_artifact.digest,
                proposals[1]["id"],
            ),
        )

    acceptance = evaluate_acceptance(project)
    assert sorted(
        acceptance["metrics"]["legacy_reference_adoption"]["invalid_receipt_ids"]
    ) == sorted([proposals[0]["id"], proposals[1]["id"]])
    assert "one or more legacy reference adoption receipts are invalid" in acceptance[
        "blockers"
    ]


def test_malformed_hash_valid_adoption_proposal_cannot_crash_acceptance(
    tmp_path: Path,
) -> None:
    project, target, references = _fixture_project(tmp_path)
    store = LegacyReferenceAdoptionStore(project)
    proposal = store.propose(target["id"], references[0]["id"])
    malformed_path = project.root / "receipts" / "hash-valid-malformed-adoption.json"
    atomic_write_json(malformed_path, ["not", "an", "object"])
    malformed_artifact = ArtifactStore(project).ingest_file(
        malformed_path,
        media_type="application/vnd.bvmcp.legacy-reference-adoption-proposal+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_adoption_proposals SET proposal_json=?,proposal_digest=? "
            "WHERE id=?",
            (
                json.dumps(["not", "an", "object"]),
                malformed_artifact.digest,
                proposal["id"],
            ),
        )

    acceptance = evaluate_acceptance(project)
    assert acceptance["metrics"]["legacy_reference_adoption"][
        "invalid_receipt_ids"
    ] == [proposal["id"]]


def test_adoption_review_transaction_rolls_back_all_authority_rows(tmp_path: Path) -> None:
    project, target, references = _fixture_project(tmp_path)
    store = LegacyReferenceAdoptionStore(project)
    proposal = store.propose(target["id"], references[0]["id"])
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_adoption_review "
            "BEFORE UPDATE OF decision_digest ON reference_adoption_proposals "
            "BEGIN SELECT RAISE(ABORT, 'simulated adoption failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="simulated adoption failure"):
        store.review(
            proposal["id"],
            decision="ADOPT",
            reviewer="Named evidence reviewer",
            reason="The owner archive and exact target identity were checked",
            source=_source(references[0]),
            rights={
                "status": "USER_OWNED",
                "internal_use": True,
                "redistribution": False,
            },
            source_terms_review="user_owned",
            privacy_review="not_applicable",
        )

    assert store.get(proposal["id"], verify=True)["status"] == "PROPOSED"
    assert project.status()["counts"]["evidence_sources"] == 0
    assert project.status()["counts"]["evidence_source_governance_reviews"] == 0
    assert project.status()["counts"]["evidence_source_acquisitions"] == 0
    with project.connection() as connection:
        reference = connection.execute(
            "SELECT rights_state,acceptance_eligible FROM reference_items WHERE id=?",
            (references[0]["id"],),
        ).fetchone()
        rights_count = connection.execute("SELECT COUNT(*) FROM rights_ledger").fetchone()[0]
    assert dict(reference) == {
        "rights_state": "LEGACY_INTERNAL_LABEL",
        "acceptance_eligible": 1,
    }
    assert rights_count == 0
