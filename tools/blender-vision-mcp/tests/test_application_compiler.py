from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest
from test_application_specification import complete_packet_document

from blender_vision.app_build import (
    ApplicationReferencePacket,
    BoundedApplicationCompiler,
    CompilationError,
)
from blender_vision.cli.main import build_parser, dispatch


def _packet(**changes: object) -> ApplicationReferencePacket:
    document = complete_packet_document()
    document.update(changes)
    return ApplicationReferencePacket.model_validate(document)


def _verified_sources(packet: ApplicationReferencePacket) -> set[str]:
    return {source.id for source in packet.sources}


def _materialize_packet(tmp_path: Path) -> Path:
    document = complete_packet_document()
    for source in document["sources"]:
        payload = f"authoritative source bytes for {source['id']}\n".encode()
        locator = Path("packet") / f"{source['id']}.json"
        destination = tmp_path / locator
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(payload)
        source["locator"] = locator.as_posix()
        source["digest"] = hashlib.sha256(payload).hexdigest()
    packet_path = tmp_path / "packet.json"
    packet_path.write_text(json.dumps(document), encoding="utf-8")
    return packet_path


def test_compiler_materializes_receipt_bound_typescript_application(tmp_path: Path) -> None:
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")
    packet = _packet()

    receipt = compiler.compile(
        packet,
        candidate_id="owned-crud",
        mode="promotion_candidate",
        verified_source_ids=_verified_sources(packet),
    )
    candidate = tmp_path / "candidates" / "owned-crud"
    verification = compiler.verify_candidate(candidate)

    assert receipt.promotable is True
    assert receipt.verified_source_ids == sorted(_verified_sources(packet))
    assert receipt.packet_sha256 == packet.canonical_digest()
    assert verification.valid is True
    assert (candidate / "src" / "app.ts").is_file()
    assert (candidate / "frontend" / "src" / "app.ts").is_file()
    assert (candidate / "generated" / "migrations" / "001_up.sql").is_file()
    assert (candidate / "generated" / "migrations" / "001_down.sql").is_file()
    assert (candidate / "Dockerfile").is_file()
    assert (candidate / "compose.yaml").is_file()
    assert (candidate / "package-lock.json").is_file()
    package = json.loads((candidate / "package.json").read_text(encoding="utf-8"))
    lock = json.loads((candidate / "package-lock.json").read_text(encoding="utf-8"))
    assert package["engines"]["node"] == ">=20 <21"
    assert (
        lock["packages"]["node_modules/better-sqlite3"]["version"]
        == package["dependencies"]["better-sqlite3"]
    )
    migration = (candidate / "generated" / "migrations" / "001_up.sql").read_text(encoding="utf-8")
    assert 'CREATE TABLE IF NOT EXISTS "items"' in migration
    assert "idempotency_keys" in migration


def test_verifier_detects_source_tampering(tmp_path: Path) -> None:
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")
    compiler.compile(_packet(), candidate_id="tamper-probe")
    candidate = tmp_path / "candidates" / "tamper-probe"
    (candidate / "src" / "app.ts").write_text("tampered\n", encoding="utf-8")

    verification = compiler.verify_candidate(candidate)

    assert verification.valid is False
    assert verification.changed_files[0]["path"] == "src/app.ts"


def test_promotion_rejects_unverified_source_bytes(tmp_path: Path) -> None:
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")

    with pytest.raises(
        CompilationError,
        match="promotion candidate requires digest-verified source bytes",
    ):
        compiler.compile(
            _packet(),
            candidate_id="unverified-promotion",
            mode="promotion_candidate",
        )


def test_hypothesis_compiles_only_as_draft(tmp_path: Path) -> None:
    document = complete_packet_document()
    auth_source = next(item for item in document["sources"] if item["id"] == "auth")
    auth_source["authority"] = "HYPOTHESIS"
    packet = ApplicationReferencePacket.model_validate(document)
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")

    with pytest.raises(CompilationError, match="promotion candidate requires complete authority"):
        compiler.compile(
            packet,
            candidate_id="unpromotable",
            mode="promotion_candidate",
        )
    receipt = compiler.compile(packet, candidate_id="explicit-draft", mode="draft")
    assert receipt.promotable is False
    assert receipt.completeness_report.hypotheses


def test_invalid_identifier_failure_is_preserved(tmp_path: Path) -> None:
    document = complete_packet_document()
    document["data_model"]["entities"][0]["table_name"] = "items; DROP TABLE items"
    packet = ApplicationReferencePacket.model_validate(document)
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")

    with pytest.raises(CompilationError, match="attempt preserved"):
        compiler.compile(packet, candidate_id="unsafe-sql", mode="draft")

    failures = list((tmp_path / "candidates" / "failed").iterdir())
    assert len(failures) == 1
    failure = json.loads((failures[0] / ".visionmcp" / "failure.json").read_text(encoding="utf-8"))
    assert failure["error_type"] == "CompilationError"
    assert "unsafe SQL identifier" in failure["error"]


def test_compiler_rejects_unsupported_database_without_creating_candidate(
    tmp_path: Path,
) -> None:
    document = complete_packet_document()
    document["data_model"]["database_engine"] = "postgresql"
    packet = ApplicationReferencePacket.model_validate(document)
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")

    with pytest.raises(CompilationError, match="SQLite targets only"):
        compiler.compile(packet, candidate_id="postgres-candidate")

    assert not (tmp_path / "candidates" / "postgres-candidate").exists()
    assert not (tmp_path / "candidates" / "failed").exists()


def test_verified_candidate_can_be_copied_for_fresh_clone_test(tmp_path: Path) -> None:
    compiler = BoundedApplicationCompiler(tmp_path / "candidates")
    compiler.compile(_packet(), candidate_id="source-candidate")
    source = tmp_path / "candidates" / "source-candidate"
    destination = tmp_path / "fresh-clone"

    compiler.copy_candidate(source, destination)

    assert compiler.verify_candidate(destination).valid is True
    with pytest.raises(CompilationError, match="already exists"):
        compiler.copy_candidate(source, destination)


def test_cli_checks_compiles_and_verifies_packet(tmp_path: Path) -> None:
    packet_path = _materialize_packet(tmp_path)
    workspace = tmp_path / "workspace"
    parser = build_parser()

    checked = dispatch(parser.parse_args(["app", "check", str(packet_path)]))
    compiled = dispatch(
        parser.parse_args(
            [
                "app",
                "compile",
                str(packet_path),
                "--workspace",
                str(workspace),
                "--candidate-id",
                "cli-candidate",
                "--mode",
                "promotion_candidate",
            ]
        )
    )
    verified = dispatch(parser.parse_args(["app", "verify", str(workspace / "cli-candidate")]))

    assert checked["promotable"] is True
    assert checked["missing_authority"] == []
    assert compiled["candidate_id"] == "cli-candidate"
    assert compiled["promotable"] is True
    assert compiled["verified_source_ids"]
    assert verified["valid"] is True
