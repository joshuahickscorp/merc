from __future__ import annotations

import json
import sqlite3
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
from pathlib import Path

import pytest
from PIL import Image

from blender_vision.acceptance.receipts import evaluate_acceptance
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.benchmarks.beast import BeastBenchmarkAuditor
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.acquisition import EvidenceAcquisitionStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.projects.store import ProjectStore
from blender_vision.workflows.progress import WorkflowProgressReporter


def test_target_resolution_binds_canonical_identity_and_search_plan(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Porsche benchmark")
    target = TargetResolver(project).resolve(
        "Make an L3 digital twin of a 2024 Porsche 911 GT3 RS.",
        requested_tier="L3",
        market="North America",
    )

    assert target["status"] == "RESOLVED"
    assert target["target"]["manufacturer"] == "Porsche"
    assert target["target"]["model"] == "911 GT3 RS"
    assert target["target"]["model_year"] == 2024
    assert target["target"]["output_classification"] == ("AUTONOMOUS EVIDENCE-BASED RECONSTRUCTION")
    assert project.project()["metadata"]["canonical_target_id"] == target["id"]
    assert TargetResolver(project).authority_status(target["id"])["valid"] is True
    assert project.status()["counts"]["target_resolution_events"] == 1

    plan = EvidenceAcquisitionStore(project).plan_search(target["id"], category="vehicles")
    assert any("underbody" in query for query in plan["queries"])
    assert any("wheelbase" in query for query in plan["queries"])


def test_material_variant_ambiguity_requires_one_clarification(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Variant")
    result = TargetResolver(project).resolve(
        {"manufacturer": "Porsche", "model": "911 GT3 RS"},
        alternatives=[
            {"name": "992.1", "geometry_changes": True},
            {"name": "991.2", "geometry_changes": True},
        ],
    )

    assert result["status"] == "NEEDS_CLARIFICATION"
    assert result["ambiguity"]["requires_clarification"] is True
    assert result["ambiguity"]["question"].count("?") == 1


def test_target_resolution_chain_is_append_only_and_revisioned(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Target chain")
    resolver = TargetResolver(project)
    first = resolver.resolve({"manufacturer": "Acme", "model": "Widget A"})
    second = resolver.resolve({"manufacturer": "Acme", "model": "Widget B"})

    assert first["target"]["revision"] == 1
    assert second["target"]["revision"] == 2
    assert resolver.get()["id"] == second["id"]
    assert resolver.authority_status(first["id"])["valid"] is True
    second_status = resolver.authority_status(second["id"])
    assert second_status["valid"] is True
    second_receipt = json.loads(
        resolver.artifacts.path_for(second_status["receipt_digest"]).read_text()
    )
    assert second_receipt["supersedes_receipt_digest"] == first["receipt_digest"]


def test_target_authority_rejects_hash_valid_semantic_forgery(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Forged target")
    resolver = TargetResolver(project)
    target = resolver.resolve({"manufacturer": "Acme", "model": "Widget"})
    status = resolver.authority_status(target["id"])
    receipt = json.loads(
        resolver.artifacts.path_for(status["receipt_digest"]).read_text()
    )
    receipt["target"]["model"] = None
    forged_path = project.root / "receipts" / "forged-target.json"
    atomic_write_json(forged_path, receipt)
    forged = resolver.artifacts.ingest_file(
        forged_path, media_type="application/vnd.bvmcp.target-resolution+json"
    )
    forged_target = dict(target["target"])
    forged_target["model"] = None
    with project.connection() as connection:
        connection.execute(
            "UPDATE target_resolutions SET target_json=? WHERE id=?",
            (json.dumps(forged_target), target["id"]),
        )
        connection.execute(
            "UPDATE target_resolution_events SET receipt_digest=? WHERE target_id=?",
            (forged.digest, target["id"]),
        )

    assert resolver.authority_status(target["id"])["valid"] is False
    acceptance = evaluate_acceptance(project)
    assert "canonical target resolution receipt is missing or invalid" in acceptance[
        "blockers"
    ]
    progress = WorkflowProgressReporter(project).report()
    stage = next(item for item in progress["stages"] if item["stage"] == "target_resolution")
    assert stage["status"] == "BLOCKED"
    benchmark = BeastBenchmarkAuditor(project).audit(3)
    assert benchmark["facts"]["resolved_target_count"] == 0
    target_check = next(
        item for item in benchmark["checks"] if item["name"] == "target variant resolved"
    )
    assert target_check["passed"] is False


def test_target_authority_rejects_hash_valid_skipped_supersession(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Target supersession")
    resolver = TargetResolver(project)
    first = resolver.resolve({"manufacturer": "Acme", "model": "Widget A"})
    resolver.resolve({"manufacturer": "Acme", "model": "Widget B"})
    third = resolver.resolve({"manufacturer": "Acme", "model": "Widget C"})
    status = resolver.authority_status(third["id"])
    receipt = json.loads(
        resolver.artifacts.path_for(status["receipt_digest"]).read_text()
    )
    receipt["supersedes_receipt_digest"] = first["receipt_digest"]
    forged_path = project.root / "receipts" / "skipped-target-supersession.json"
    atomic_write_json(forged_path, receipt)
    forged = resolver.artifacts.ingest_file(
        forged_path, media_type="application/vnd.bvmcp.target-resolution+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE target_resolution_events SET receipt_digest=?,"
            "supersedes_receipt_digest=? WHERE target_id=?",
            (forged.digest, first["receipt_digest"], third["id"]),
        )

    assert resolver.authority_status(third["id"])["valid"] is False


def test_concurrent_target_resolution_uses_compare_and_swap(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Target race")
    outer = TargetResolver(project)
    original_ingest = outer.artifacts.ingest_file

    def competing_resolution(path: Path, *, media_type: str | None = None):
        artifact = original_ingest(path, media_type=media_type)
        TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Winner"})
        return artifact

    monkeypatch.setattr(outer.artifacts, "ingest_file", competing_resolution)
    with pytest.raises(RuntimeError, match="changed during resolution"):
        outer.resolve({"manufacturer": "Acme", "model": "Loser"})

    assert project.status()["counts"]["target_resolutions"] == 1
    assert project.status()["counts"]["target_resolution_events"] == 1
    assert TargetResolver(project).get()["target"]["model"] == "Winner"


def test_target_resolution_ledger_failure_rolls_back_identity(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Target rollback")
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_target_resolution_event BEFORE INSERT "
            "ON target_resolution_events "
            "BEGIN SELECT RAISE(ABORT, 'simulated target event failure'); END"
        )

    with pytest.raises(sqlite3.IntegrityError, match="simulated target event failure"):
        TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})

    assert project.status()["counts"]["target_resolutions"] == 0
    assert project.status()["counts"]["target_resolution_events"] == 0
    assert "canonical_target_id" not in project.project().get("metadata", {})
    assert not (project.root / "target.json").exists()


def test_legacy_target_authority_migration_preserves_record_and_rejects_forgery(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Legacy target")
    resolver = TargetResolver(project)
    target = resolver.resolve({"manufacturer": "Acme", "model": "Legacy Widget"})
    before = resolver._record(target["id"])
    with project.connection() as connection:
        connection.execute(
            "DELETE FROM target_resolution_events WHERE target_id=?", (target["id"],)
        )

    migrated = resolver.migrate_legacy_authority(target["id"])
    assert migrated["migration"]["new_resolution_performed"] is False
    assert resolver._record(target["id"]) == before
    assert migrated["authority"]["valid"] is True

    receipt_path = resolver.artifacts.path_for(migrated["receipt_digest"])
    receipt = json.loads(receipt_path.read_text())
    receipt["migration"]["new_resolution_performed"] = True
    forged_path = project.root / "receipts" / "forged-target-migration.json"
    atomic_write_json(forged_path, receipt)
    forged = resolver.artifacts.ingest_file(
        forged_path, media_type="application/vnd.bvmcp.target-resolution+json"
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE target_resolution_events SET receipt_digest=? WHERE target_id=?",
            (forged.digest, target["id"]),
        )
    assert resolver.authority_status(target["id"])["valid"] is False


def test_evidence_source_rights_acquisition_and_coverage(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Evidence")
    target = TargetResolver(project).resolve(
        {"manufacturer": "NVIDIA", "model": "DGX Spark", "model_year": 2025}
    )
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "owned-front-photo",
            "publisher": "Project owner",
            "page_title": "Front reference",
            "authority_class": "user_owned",
            "target_variant": {"model": "DGX Spark"},
            "viewpoint": "front",
            "quality_score": 0.95,
        },
        rights={
            "status": "USER_OWNED",
            "internal_use": True,
            "redistribution": True,
        },
        reviewed_by="Owner",
    )
    image = tmp_path / "front.png"
    Image.new("RGB", (100, 80), "gray").save(image)
    acquired = store.acquire_local(source["id"], image)

    assert acquired["status"] == "ACQUIRED"
    assert acquired["source"]["media_hash"] == acquired["reference"]["artifact"]["digest"]
    authority = store.authority_status(source["id"])
    assert authority["governance_valid"] is True
    assert authority["acquisition_valid"] is True
    assert project.status()["counts"]["evidence_source_governance_reviews"] == 1
    assert project.status()["counts"]["evidence_source_acquisitions"] == 1
    coverage = store.analyze_coverage(target["id"])
    assert coverage["directions"]["front"] == ["owned-front-photo"]
    assert "rear" in coverage["missing_directions"]
    assert coverage["acquired_count"] == 1
    assert store.audit(target["id"])["governance_complete"] is True

    denied = store.register_source(
        target["id"],
        {
            "origin": "restricted-source",
            "publisher": "Publisher",
            "page_title": "Restricted",
            "authority_class": "diagnostic_third_party",
            "target_variant": {"model": "DGX Spark"},
            "viewpoint": "rear",
            "quality_score": 0.8,
        },
        rights={"status": "RESTRICTED", "internal_use": False, "redistribution": False},
    )
    with pytest.raises(PermissionError):
        store.acquire_local(denied["id"], image)


def test_source_authority_rejects_hash_valid_acquisition_forgery(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Forged source authority")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "user://front",
            "publisher": "Project owner",
            "page_title": "Front",
            "authority_class": "user_owned",
            "target_variant": {"manufacturer": "Acme", "model": "Widget"},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
        reviewed_by="Project owner",
    )
    image = tmp_path / "front.png"
    Image.new("RGB", (64, 48), "gray").save(image)
    acquired = store.acquire_local(source["id"], image)
    artifacts = ArtifactStore(project)
    forged = json.loads(
        artifacts.path_for(acquired["acquisition_receipt_digest"]).read_text(
            encoding="utf-8"
        )
    )
    forged["source"]["media_hash"] = "0" * 64
    forged["source"]["content_hash"] = "0" * 64
    forged_path = project.root / "receipts" / "forged-source-acquisition.json"
    atomic_write_json(forged_path, forged)
    forged_artifact = artifacts.ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.evidence-source-acquisition+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE evidence_sources SET source_json=? WHERE id=?",
            (json.dumps(forged["source"]), source["id"]),
        )
        connection.execute(
            "UPDATE evidence_source_acquisitions SET source_json=?,receipt_digest=? "
            "WHERE source_id=?",
            (json.dumps(forged["source"]), forged_artifact.digest, source["id"]),
        )

    status = store.authority_status(source["id"])
    assert status["governance_valid"] is True
    assert status["acquisition_valid"] is False
    assert store.analyze_coverage(target["id"])["eligible_acquired_count"] == 0
    acceptance = evaluate_acceptance(project)
    assert source["id"] in acceptance["metrics"]["source_governance"][
        "incomplete_source_ids"
    ]
    progress = WorkflowProgressReporter(project).report()
    assert progress["evidence"]["acquired_source_count"] == 0
    assert progress["evidence"]["eligible_acquired_source_count"] == 0


def test_source_authority_ledger_failures_roll_back_mutable_state(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Source authority rollback")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "user://front",
            "publisher": "Project owner",
            "page_title": "Front",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
    )
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_source_governance BEFORE INSERT "
            "ON evidence_source_governance_reviews "
            "BEGIN SELECT RAISE(ABORT, 'simulated source governance failure'); END"
        )
    with pytest.raises(sqlite3.IntegrityError, match="simulated source governance failure"):
        store.review_governance(
            source["id"],
            reviewed_by="Project owner",
            source_terms_review="user_owned",
            privacy_review="user_owned",
        )
    assert store.get(source["id"])["reviewed_by"] is None
    assert project.status()["counts"]["evidence_source_governance_reviews"] == 0

    with project.connection() as connection:
        connection.execute("DROP TRIGGER fail_source_governance")
    store.review_governance(
        source["id"],
        reviewed_by="Project owner",
        source_terms_review="user_owned",
        privacy_review="user_owned",
    )
    with project.connection() as connection:
        connection.execute(
            "CREATE TRIGGER fail_source_acquisition BEFORE INSERT "
            "ON evidence_source_acquisitions "
            "BEGIN SELECT RAISE(ABORT, 'simulated source acquisition failure'); END"
        )
    image = tmp_path / "front.png"
    Image.new("RGB", (32, 24), "gray").save(image)
    with pytest.raises(sqlite3.IntegrityError, match="simulated source acquisition failure"):
        store.acquire_local(source["id"], image)
    current = store.get(source["id"])
    assert current["status"] == "DISCOVERED"
    assert current["reference_id"] is None
    assert project.status()["counts"]["evidence_source_acquisitions"] == 0


def test_concurrent_source_governance_review_uses_compare_and_swap(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Source review race")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    outer = EvidenceAcquisitionStore(project)
    source = outer.register_source(
        target["id"],
        {
            "origin": "user://front",
            "publisher": "Project owner",
            "page_title": "Front",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
    )
    original_ingest = outer.artifacts.ingest_file

    def competing_review(path: Path, *, media_type: str | None = None):
        artifact = original_ingest(path, media_type=media_type)
        EvidenceAcquisitionStore(project).review_governance(
            source["id"],
            reviewed_by="Concurrent reviewer",
            source_terms_review="user_owned",
            privacy_review="user_owned",
        )
        return artifact

    monkeypatch.setattr(outer.artifacts, "ingest_file", competing_review)
    with pytest.raises(RuntimeError, match="changed during governance review"):
        outer.review_governance(
            source["id"],
            reviewed_by="Original reviewer",
            source_terms_review="user_owned",
            privacy_review="user_owned",
        )
    assert project.status()["counts"]["evidence_source_governance_reviews"] == 1
    assert outer.governance_status(source["id"])["valid"] is True
    assert outer.get(source["id"])["reviewed_by"] == "Concurrent reviewer"


def test_source_governance_rejects_hash_valid_skipped_supersession(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Source review chain")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "user://front",
            "publisher": "Project owner",
            "page_title": "Front",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
    )
    decisions = []
    for reviewer in ("First reviewer", "Second reviewer", "Third reviewer"):
        decisions.append(
            store.review_governance(
                source["id"],
                reviewed_by=reviewer,
                source_terms_review="user_owned",
                privacy_review="user_owned",
            )
        )
    latest_digest = decisions[-1]["governance_receipt_digest"]
    forged = json.loads(
        store.artifacts.path_for(latest_digest).read_text(encoding="utf-8")
    )
    forged["supersedes_receipt_digest"] = decisions[0]["governance_receipt_digest"]
    forged_path = project.root / "receipts" / "skipped-source-governance-link.json"
    atomic_write_json(forged_path, forged)
    forged_artifact = store.artifacts.ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.evidence-source-governance+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE evidence_source_governance_reviews SET receipt_digest=?,"
            "supersedes_receipt_digest=? WHERE id=?",
            (
                forged_artifact.digest,
                decisions[0]["governance_receipt_digest"],
                forged["id"],
            ),
        )

    status = store.governance_status(source["id"])
    assert status["valid"] is False
    assert "semantics are inconsistent" in status["error"]


def test_legacy_source_authority_migration_replays_named_review_and_bytes(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Legacy source migration")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "user://legacy-front",
            "publisher": "Project owner",
            "page_title": "Legacy front",
            "authority_class": "user_owned",
            "target_variant": {},
            "viewpoint": "front",
            "quality_score": 1.0,
        },
        rights={"status": "USER_OWNED", "internal_use": True, "redistribution": False},
    )
    image = tmp_path / "legacy-front.png"
    Image.new("RGB", (40, 30), "gray").save(image)
    reference = ReferenceIngestor(project).import_file(
        image, rights_state="USER_OWNED", viewpoint_label="front"
    )
    legacy_source = source["source"]
    legacy_source["access_policy"]["reviewed_by"] = "Legacy reviewer"
    legacy_source["access_policy"]["reviewed_at"] = "2026-07-21T00:00:00.000001+00:00"
    legacy_source["content_hash"] = reference["artifact"]["digest"]
    legacy_source["media_hash"] = reference["artifact"]["digest"]
    with project.connection() as connection:
        connection.execute(
            "UPDATE evidence_sources SET reference_id=?,source_json=?,status='ACQUIRED',"
            "updated_at=? WHERE id=?",
            (
                reference["id"],
                json.dumps(legacy_source),
                "2026-07-21T00:00:01+00:00",
                source["id"],
            ),
        )
        connection.execute(
            "UPDATE rights_ledger SET reviewed_by=?,reviewed_at=?,updated_at=? "
            "WHERE source_id=?",
            (
                "Legacy reviewer",
                "2026-07-21T00:00:00.000002+00:00",
                "2026-07-21T00:00:00.000002+00:00",
                source["id"],
            ),
        )

    migrated = store.migrate_legacy_authority(source["id"])
    assert migrated["migration"]["new_review_performed"] is False
    assert migrated["migration"]["normalized_fields"] == [
        "access_policy.reviewer_type",
        "access_policy.reviewed_at",
    ]
    assert migrated["authority"]["valid"] is True
    current = store.get(source["id"])
    assert "reviewer_type" not in current["source"]["access_policy"]
    assert current["source"]["access_policy"]["reviewed_at"] == (
        "2026-07-21T00:00:00.000001+00:00"
    )
    assert current["reviewed_at"] == "2026-07-21T00:00:00.000002+00:00"

    receipt_path = store.artifacts.path_for(migrated["governance_receipt_digest"])
    forged = json.loads(receipt_path.read_text(encoding="utf-8"))
    forged["migration"]["new_review_performed"] = True
    forged_path = project.root / "receipts" / "forged-source-migration.json"
    atomic_write_json(forged_path, forged)
    forged_artifact = store.artifacts.ingest_file(
        forged_path,
        media_type="application/vnd.bvmcp.evidence-source-governance+json",
    )
    with project.connection() as connection:
        connection.execute(
            "UPDATE evidence_source_governance_reviews SET receipt_digest=? WHERE source_id=?",
            (forged_artifact.digest, source["id"]),
        )
    assert store.governance_status(source["id"])["valid"] is False


def test_public_source_governance_requires_named_terms_review(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Governed public evidence")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "https://example.test/widget",
            "publisher": "Acme",
            "page_title": "Widget manual",
            "authority_class": "manufacturer_authoritative",
            "target_variant": {"manufacturer": "Acme", "model": "Widget"},
            "viewpoint": "technical drawing",
            "quality_score": 1.0,
        },
        rights={"status": "PUBLIC_FACTUAL", "internal_use": True, "redistribution": False},
    )
    assert store.audit(target["id"])["governance_complete"] is False

    reviewed = store.review_governance(
        source["id"],
        reviewed_by="Evidence auditor",
        source_terms_review="approved",
        privacy_review="not_applicable",
    )

    assert reviewed["reviewed_by"] == "Evidence auditor"
    assert store.audit(target["id"])["governance_complete"] is True


def test_policy_agent_review_discloses_terms_basis_and_cannot_redistribute(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Policy-reviewed evidence")
    target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
    store = EvidenceAcquisitionStore(project)
    source = store.register_source(
        target["id"],
        {
            "origin": "https://example.test/widget/specifications",
            "publisher": "Acme",
            "page_title": "Widget specifications",
            "authority_class": "manufacturer_authoritative",
            "target_variant": {"manufacturer": "Acme", "model": "Widget"},
            "viewpoint": "official dimensions",
            "quality_score": 1.0,
        },
        rights={"status": "INTERNAL", "internal_use": True, "redistribution": False},
    )
    basis = {
        "terms_urls": ["https://example.test/legal"],
        "terms_retrieved_at": "2026-07-21T00:00:00+00:00",
        "scope": "personal non-commercial internal evidence use",
        "decision": "internal_use_permitted",
        "redistribution_prohibited": True,
    }

    reviewed = store.review_governance(
        source["id"],
        reviewed_by="VisionMCP policy agent",
        reviewer_type="policy_agent",
        review_basis=basis,
        source_terms_review="approved",
        privacy_review="not_applicable",
    )

    access = reviewed["source"]["access_policy"]
    assert access["reviewer_type"] == "policy_agent"
    assert access["review_basis"] == basis
    with pytest.raises(ValueError, match="non-redistributed internal use"):
        store.review_governance(
            source["id"],
            reviewed_by="VisionMCP policy agent",
            reviewer_type="policy_agent",
            review_basis={**basis, "scope": ""},
            source_terms_review="approved",
            privacy_review="not_applicable",
        )
    with pytest.raises(ValueError, match="non-redistributed internal use"):
        store.review_governance(
            source["id"],
            reviewed_by="VisionMCP policy agent",
            reviewer_type="policy_agent",
            review_basis=basis,
            source_terms_review="approved",
            privacy_review="not_applicable",
            rights={"status": "PUBLIC", "internal_use": True, "redistribution": True},
        )


def test_governed_url_acquisition_enforces_robots_and_records_http_provenance(
    tmp_path: Path,
) -> None:
    buffer = BytesIO()
    Image.new("RGB", (64, 48), "gray").save(buffer, format="PNG")
    payload = buffer.getvalue()

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:  # noqa: N802
            if self.path == "/robots.txt":
                body = b"User-agent: *\nAllow: /\n"
                content_type = "text/plain"
            elif self.path == "/front.png":
                body = payload
                content_type = "image/png"
            else:
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("ETag", '"fixture-v1"')
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: object) -> None:
            return

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        project = ProjectStore.create(tmp_path / "project", "URL acquisition")
        target = TargetResolver(project).resolve({"manufacturer": "Acme", "model": "Widget"})
        store = EvidenceAcquisitionStore(project)
        origin = f"http://127.0.0.1:{server.server_port}/front.png"
        source = store.register_source(
            target["id"],
            {
                "origin": origin,
                "publisher": "Fixture server",
                "page_title": "Front reference",
                "authority_class": "user_owned",
                "target_variant": {"manufacturer": "Acme", "model": "Widget"},
                "viewpoint": "front",
                "quality_score": 1.0,
                "access_policy": {
                    "authentication_boundary": "user_authorized",
                    "private_network_authorized": True,
                    "maximum_download_bytes": 1024 * 1024,
                },
            },
            rights={"status": "USER_OWNED", "internal_use": True, "redistribution": True},
        )
        store.review_governance(
            source["id"],
            reviewed_by="Fixture owner",
            source_terms_review="user_owned",
            privacy_review="user_owned",
        )

        result = store.acquire_url(source["id"], timeout_seconds=5)

        assert result["status"] == "ACQUIRED"
        assert result["reference"]["quality"]["decode_ok"] is True
        assert result["retrieval"]["requested_url"] == origin
        assert result["retrieval"]["robots_allowed"] is True
        assert result["retrieval"]["content_type"] == "image/png"
        assert result["retrieval"]["bytes"] == len(payload)
        assert result["source"]["content_hash"] == result["reference"]["artifact"]["digest"]
        assert list((project.root / "references" / "downloads").iterdir()) == []
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
