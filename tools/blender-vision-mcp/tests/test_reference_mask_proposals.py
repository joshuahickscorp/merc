from __future__ import annotations

import json
import sqlite3
import uuid
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.comparison.metrics import compare_silhouettes
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.review import ReviewService


def _project(tmp_path: Path) -> tuple[ProjectStore, dict]:
    project = ProjectStore.create(tmp_path / "project", "Mask proposal")
    reference_path = tmp_path / "reference.png"
    reference = Image.new("RGB", (160, 120), (40, 50, 65))
    ImageDraw.Draw(reference).rounded_rectangle(
        (25, 25, 135, 100), radius=12, fill=(185, 190, 195)
    )
    reference.save(reference_path)
    imported = ReferenceIngestor(project).import_file(
        reference_path, rights_state="SYNTHETIC_OWNED", viewpoint_label="front"
    )
    return project, imported


def test_automatic_mask_proposal_is_replayable_idempotent_and_review_queued(
    tmp_path: Path,
) -> None:
    project, reference = _project(tmp_path)
    store = ReferenceMaskStore(project)

    proposal = store.propose_automatic(reference["id"])
    repeated = store.propose_automatic(reference["id"])

    assert repeated["id"] == proposal["id"]
    assert proposal["proposal"]["authority"] == (
        "MACHINE_PROPOSAL_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY"
    )
    assert proposal["proposal_verification"]["valid"] is True
    assert store.list() == []
    acceptance = evaluate_acceptance(project)
    assert acceptance["metrics"]["reference_mask_proposals"] == {
        "count": 1,
        "pending_count": 1,
        "approved_count": 0,
        "rejected_count": 0,
        "invalid_proposal_ids": [],
    }
    queue = ReviewService(project).review_queue()
    assert [item["kind"] for item in queue] == ["reference_mask"]
    assert queue[0]["reference_image_url"].startswith("/artifact/")
    assert queue[0]["mask_image_url"].startswith("/artifact/")

    scoped = store.propose_automatic(
        reference["id"],
        visible_components=["outer-shell"],
        roi={"x": 1, "y": 2, "width": 150, "height": 110},
    )
    scoped_repeat = store.propose_automatic(
        reference["id"],
        visible_components=["outer-shell"],
        roi={"x": 1, "y": 2, "width": 150, "height": 110},
    )
    assert scoped["id"] != proposal["id"]
    assert scoped_repeat["id"] == scoped["id"]

    forged = dict(proposal["proposal"])
    forged["roi"] = {"x": 0, "y": 0, "width": 999, "height": 120}
    forged_path = project.root / "receipts" / "hash-valid-forged-mask-proposal.json"
    atomic_write_json(forged_path, forged)
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.reference-mask-proposal+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_mask_proposals SET record_json=?,proposal_digest=? WHERE id=?",
            (json.dumps(forged), forged_artifact.digest, proposal["id"]),
        )
    tampered = evaluate_acceptance(project)
    assert proposal["id"] in tampered["metrics"]["reference_mask_proposals"][
        "invalid_proposal_ids"
    ]
    assert "reference-mask proposal ledger contains invalid receipts" in tampered["blockers"]


def test_hash_valid_incomplete_approval_snapshot_has_no_mask_authority(
    tmp_path: Path,
) -> None:
    project, reference = _project(tmp_path)
    store = ReferenceMaskStore(project)
    proposal = store.propose_automatic(reference["id"])
    approved = store.review_proposal(
        proposal["id"],
        accepted=True,
        reviewer="Named mask reviewer",
        reason="The full-object outline was checked against the source pixels",
    )
    artifacts = ArtifactStore(project)
    forged_decision = json.loads(
        artifacts.path_for(approved["decision_digest"]).read_text(encoding="utf-8")
    )
    forged_decision["approved_mask"] = {}
    forged_path = project.root / "receipts" / "hash-valid-incomplete-mask-decision.json"
    atomic_write_json(forged_path, forged_decision)
    forged_artifact = artifacts.ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.reference-mask-decision+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_mask_proposals SET decision_digest=? WHERE id=?",
            (forged_artifact.digest, proposal["id"]),
        )
        connection.execute(
            "UPDATE reference_masks SET decision_digest=? WHERE id=?",
            (forged_artifact.digest, approved["approved_mask_id"]),
        )

    assert store.verify_decision(proposal["id"])["valid"] is False
    acceptance = evaluate_acceptance(project)
    assert proposal["id"] in acceptance["metrics"]["reference_mask_proposals"][
        "invalid_proposal_ids"
    ]
    assert "reference-mask proposal ledger contains invalid receipts" in acceptance["blockers"]


def test_named_mask_review_is_receipted_and_tamper_evident(tmp_path: Path) -> None:
    project, reference = _project(tmp_path)
    store = ReferenceMaskStore(project)
    proposal = store.propose_automatic(reference["id"])

    reviewed = ReviewService(project).action(
        "reference_mask.review",
        {
            "id": proposal["id"],
            "accepted": True,
            "reviewer": "Named mask reviewer",
            "reason": "The full-object outline was checked against the source pixels",
        },
    )

    assert reviewed["status"] == "APPROVED"
    assert reviewed["decision_verification"]["valid"] is True
    masks = store.list()
    assert len(masks) == 1
    assert masks[0]["method"] == "human_reviewed_machine_proposal"
    assert masks[0]["confidence"] == "high"
    assert store.verify_approved_mask(masks[0]) is True
    assert ReviewService(project).review_queue() == []

    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_masks SET reviewer='forged reviewer' WHERE id=?",
            (masks[0]["id"],),
        )
    assert store.verify_approved_mask(store.list()[0]) is False


def test_mask_review_transaction_rolls_back_approved_row(tmp_path: Path) -> None:
    project, reference = _project(tmp_path)
    store = ReferenceMaskStore(project)
    proposal = store.propose_automatic(reference["id"])
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_mask_proposal_review "
            "BEFORE UPDATE OF decision_digest ON reference_mask_proposals "
            "BEGIN SELECT RAISE(ABORT, 'simulated mask review failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="simulated mask review failure"):
        store.review_proposal(
            proposal["id"],
            accepted=True,
            reviewer="Named mask reviewer",
            reason="The outline was reviewed",
        )

    assert store.list() == []
    assert store.get(proposal["id"])["status"] == "PROPOSED"


def test_pending_mask_row_cannot_satisfy_l3_segmentation_gate(tmp_path: Path) -> None:
    project, reference = _project(tmp_path)
    store = ReferenceMaskStore(project)
    proposal = store.propose_automatic(reference["id"])
    approved = store.review_proposal(
        proposal["id"],
        accepted=True,
        reviewer="Named mask reviewer",
        reason="The outline was reviewed pixel by pixel",
    )
    mask = store._get_mask(approved["approved_mask_id"])
    reference_path = ArtifactStore(project).path_for(reference["artifact"]["digest"])
    render_path = tmp_path / "render.png"
    render = Image.new("RGBA", (160, 120), (0, 0, 0, 0))
    ImageDraw.Draw(render).rounded_rectangle(
        (25, 25, 135, 100), radius=12, fill=(185, 190, 195, 255)
    )
    render.save(render_path)
    residual_path = tmp_path / "residual.png"
    metrics = compare_silhouettes(
        reference_path,
        render_path,
        residual_path,
        reviewed_mask_path=ArtifactStore(project).path_for(mask["artifact_digest"]),
        reviewed_mask_record=mask,
    )
    artifacts = ArtifactStore(project)
    render_artifact = artifacts.ingest_file(render_path, media_type="image/png")
    residual_artifact = artifacts.ingest_file(residual_path, media_type="image/png")
    ComparisonStore(project).record(
        str(uuid.uuid4()),
        reference_id=reference["id"],
        render_digest=render_artifact.digest,
        residual_digest=residual_artifact.digest,
        metrics=metrics,
        engine="compare_silhouettes_v3",
    )
    before = evaluate_acceptance(project)
    assert metrics["reference_segmentation"] == "human_reviewed_machine_proposal"
    assert "silhouette comparison requires high-confidence reference masks" not in before[
        "blockers"
    ]

    with project.connection() as connection:
        connection.execute(
            "UPDATE reference_masks SET approval_state='pending' WHERE id=?", (mask["id"],)
        )
    after = evaluate_acceptance(project)
    assert "silhouette comparison requires high-confidence reference masks" in after["blockers"]
