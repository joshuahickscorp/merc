from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.beast import BeastBenchmarkAuditor
from blender_vision.benchmarks.reviews import BenchmarkReviewStore
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.review import ReviewService


def _strategy() -> dict:
    return {
        "switch_policy": "screen_space_pixel_footprint",
        "tiers": [
            {
                "name": "hero",
                "representation": "physical_geometry",
                "minimum_screen_diameter_px": 24,
            },
            {
                "name": "mid",
                "representation": "geometry_nodes",
                "minimum_screen_diameter_px": 6,
            },
            {
                "name": "background",
                "representation": "normal_map",
                "minimum_screen_diameter_px": 1,
            },
        ],
        "validation_views": ["front-real", "front-diagram", "rear-real"],
        "crossfade": True,
    }


def test_dgx_foam_lod_requires_named_receipt_backed_review(tmp_path: Path) -> None:
    project = ProjectStore.create(
        tmp_path / "project", "DGX review", metadata={"benchmark": "dgx_spark"}
    )

    approval = BenchmarkReviewStore(project).approve_dgx_foam_lod(
        strategy=_strategy(),
        reviewer="DGX asset reviewer",
        reason="Screen-space transitions reviewed in the three governed views",
    )
    facts = BeastBenchmarkAuditor(project)._facts()
    checks = {
        item["name"]: item["passed"]
        for item in BeastBenchmarkAuditor._checks(2, facts)
    }

    assert approval["state"] == "approved"
    assert approval["artifact"]["digest"] == approval["receipt_digest"]
    assert facts["foam_lod_approval_valid"] is True
    assert checks["foam LOD strategy is explicitly approved"] is True
    assert BenchmarkReviewStore(project).dgx_foam_lod_status()["valid"] is True
    replacement = BenchmarkReviewStore(project).approve_dgx_foam_lod(
        strategy=_strategy(),
        reviewer="Second DGX asset reviewer",
        reason="The same governed transitions were independently reviewed",
    )
    assert replacement["supersedes_receipt_digest"] == approval["receipt_digest"]
    assert BenchmarkReviewStore(project).dgx_foam_lod_status()["valid"] is True
    latest = BenchmarkReviewStore(project).approve_dgx_foam_lod(
        strategy=_strategy(),
        reviewer="Third DGX asset reviewer",
        reason="The current policy and its immediate predecessor were reviewed",
    )
    artifacts = ArtifactStore(project)
    skipped = json.loads(
        artifacts.path_for(latest["receipt_digest"]).read_text(encoding="utf-8")
    )
    skipped["supersedes_receipt_digest"] = approval["receipt_digest"]
    skipped_path = project.root / "receipts" / "skipped-foam-policy-link.json"
    atomic_write_json(skipped_path, skipped)
    skipped_artifact = artifacts.ingest_file(
        skipped_path, media_type="application/vnd.bvmcp.benchmark-review+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE benchmark_policy_reviews SET receipt_digest=?,"
            "supersedes_receipt_digest=? WHERE id=?",
            (skipped_artifact.digest, approval["receipt_digest"], latest["id"]),
        )
    assert BenchmarkReviewStore(project).dgx_foam_lod_status()["valid"] is False
    assert project.status()["counts"]["benchmark_policy_reviews"] == 3


def test_dgx_foam_lod_decision_is_exposed_in_unified_review_queue(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(
        tmp_path / "project", "DGX queue", metadata={"benchmark": "dgx_spark"}
    )
    service = ReviewService(project)
    queue = service.review_queue()
    assert [item["kind"] for item in queue] == ["benchmark_policy"]
    assert queue[0]["strategy_template"]["switch_policy"] == (
        "screen_space_pixel_footprint"
    )

    result = service.action(
        "benchmark.review_dgx_foam_lod",
        {
            "id": "dgx_foam_lod_strategy",
            "strategy": _strategy(),
            "reviewer": "DGX asset reviewer",
            "reason": "Reviewed the transition policy against all named fixture views",
        },
    )
    assert result["state"] == "approved"
    assert service.review_queue() == []


def test_foam_lod_boolean_or_invalid_strategy_cannot_pass_audit(tmp_path: Path) -> None:
    project = ProjectStore.create(
        tmp_path / "project",
        "Weak DGX review",
        metadata={"benchmark": "dgx_spark", "foam_lod_approval": True},
    )

    assert BeastBenchmarkAuditor(project)._facts()["foam_lod_approval_valid"] is False
    forged_receipt = {
        "schema_version": 1,
        "receipt_type": "benchmark_named_review",
        "id": "forged-metadata-review",
        "benchmark": "dgx_spark",
        "review_kind": "dgx_foam_lod_strategy",
        "state": "approved",
        "reviewer": "Metadata-only reviewer",
        "reason": "This must not bypass the append-only policy ledger",
        "strategy": _strategy(),
        "supersedes_receipt_digest": None,
        "authority": "NAMED_BENCHMARK_POLICY_REVIEW",
        "reviewed_at": "2026-07-21T00:00:00+00:00",
    }
    forged_path = project.root / "receipts" / "metadata-only-foam-review.json"
    atomic_write_json(forged_path, forged_receipt)
    forged_artifact = ArtifactStore(project).ingest_file(
        forged_path, media_type="application/vnd.bvmcp.benchmark-review+json"
    )
    metadata = project.project()
    metadata["metadata"]["foam_lod_approval"] = {
        "state": "approved",
        "review_kind": "dgx_foam_lod_strategy",
        "reviewer": forged_receipt["reviewer"],
        "reason": forged_receipt["reason"],
        "strategy": forged_receipt["strategy"],
        "reviewed_at": forged_receipt["reviewed_at"],
        "receipt_digest": forged_artifact.digest,
    }
    atomic_write_json(project.project_file, metadata)
    metadata_only = BenchmarkReviewStore(project).dgx_foam_lod_status()
    assert metadata_only["valid"] is False
    assert "no authoritative policy-ledger row" in metadata_only["error"]
    invalid = _strategy()
    invalid["tiers"][0]["representation"] = "normal_map"
    with pytest.raises(ValueError, match="hero foam LOD"):
        BenchmarkReviewStore(project).approve_dgx_foam_lod(
            strategy=invalid,
            reviewer="DGX asset reviewer",
            reason="Invalid fixture",
        )


def test_foam_lod_rejects_hash_valid_semantic_forgery_and_missing_view(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(
        tmp_path / "project", "DGX strict review", metadata={"benchmark": "dgx_spark"}
    )
    for name, viewpoint in (("front.png", "front-real"), ("rear.png", "rear-real")):
        path = tmp_path / name
        Image.new("RGB", (32, 32), "gray").save(path)
        ReferenceIngestor(project).import_file(
            path, rights_state="SYNTHETIC_OWNED", viewpoint_label=viewpoint
        )
    incomplete = _strategy()
    incomplete["validation_views"] = ["front-real"]
    with pytest.raises(ValueError, match="omit acceptance references: rear-real"):
        BenchmarkReviewStore(project).approve_dgx_foam_lod(
            strategy=incomplete,
            reviewer="DGX asset reviewer",
            reason="Incomplete view fixture",
        )

    approval = BenchmarkReviewStore(project).approve_dgx_foam_lod(
        strategy=_strategy(),
        reviewer="DGX asset reviewer",
        reason="Every current acceptance view and transition was reviewed",
    )
    artifacts = ArtifactStore(project)
    forged = json.loads(
        artifacts.path_for(approval["receipt_digest"]).read_text(encoding="utf-8")
    )
    forged["strategy"]["tiers"][0]["unreviewed_claim"] = "approved"
    forged_path = project.root / "receipts" / "hash-valid-forged-foam-policy.json"
    atomic_write_json(forged_path, forged)
    forged_artifact = artifacts.ingest_file(
        forged_path, media_type="application/vnd.bvmcp.benchmark-review+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE benchmark_policy_reviews SET strategy_json=?,receipt_digest=? WHERE id=?",
            (json.dumps(forged["strategy"]), forged_artifact.digest, approval["id"]),
        )

    status = BenchmarkReviewStore(project).dgx_foam_lod_status()
    assert status["valid"] is False
    assert "invalid schema" in status["error"]
    assert BeastBenchmarkAuditor(project)._facts()["foam_lod_approval_valid"] is False


def test_foam_lod_ledger_write_failure_has_no_policy_authority(tmp_path: Path) -> None:
    project = ProjectStore.create(
        tmp_path / "project", "DGX rollback", metadata={"benchmark": "dgx_spark"}
    )
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_foam_policy_review "
            "BEFORE INSERT ON benchmark_policy_reviews "
            "BEGIN SELECT RAISE(ABORT, 'simulated foam policy failure'); END"
        )
    with pytest.raises(sqlite3.IntegrityError, match="simulated foam policy failure"):
        BenchmarkReviewStore(project).approve_dgx_foam_lod(
            strategy=_strategy(),
            reviewer="DGX asset reviewer",
            reason="Rollback fixture",
        )

    assert project.status()["counts"]["benchmark_policy_reviews"] == 0
    assert BenchmarkReviewStore(project).dgx_foam_lod_status()["valid"] is False
    assert "foam_lod_approval" not in project.project().get("metadata", {})


def test_concurrent_foam_policy_review_uses_compare_and_swap(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project = ProjectStore.create(
        tmp_path / "project", "DGX race", metadata={"benchmark": "dgx_spark"}
    )
    outer = BenchmarkReviewStore(project)
    original_ingest = outer.artifacts.ingest_file

    def insert_competing_review(path: Path, *, media_type: str | None = None):
        artifact = original_ingest(path, media_type=media_type)
        BenchmarkReviewStore(project).approve_dgx_foam_lod(
            strategy=_strategy(),
            reviewer="Concurrent DGX reviewer",
            reason="Concurrent review won the compare-and-swap race",
        )
        return artifact

    monkeypatch.setattr(outer.artifacts, "ingest_file", insert_competing_review)
    with pytest.raises(RuntimeError, match="changed during named review"):
        outer.approve_dgx_foam_lod(
            strategy=_strategy(),
            reviewer="Original DGX reviewer",
            reason="This review should lose the compare-and-swap race",
        )

    assert project.status()["counts"]["benchmark_policy_reviews"] == 1
    status = BenchmarkReviewStore(project).dgx_foam_lod_status()
    assert status["valid"] is True
    assert status["approval"]["reviewer"] == "Concurrent DGX reviewer"
