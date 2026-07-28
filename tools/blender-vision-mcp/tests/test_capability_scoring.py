from __future__ import annotations

import json
from pathlib import Path

from jsonschema import Draft202012Validator

from blender_vision.cli.main import build_parser, dispatch
from blender_vision.scoring import CapabilityAuthority, CapabilityEvidence, EvidenceRecord
from blender_vision.scoring.catalog import (
    canonical_json_bytes,
    sha256_bytes,
    sha256_path,
)

FACET_ID = "app.webpage_screenshot_capture"


def _record(
    artifact: Path,
    record_id: str,
    kind: str,
    **overrides: object,
) -> EvidenceRecord:
    values = {
        "id": record_id,
        "kind": kind,
        "artifact_path": str(artifact),
        "artifact_sha256": sha256_path(artifact),
    }
    values.update(overrides)
    return EvidenceRecord.model_validate(values)


def _evidence(
    authority: CapabilityAuthority,
    artifact: Path,
    **overrides: object,
) -> CapabilityEvidence:
    facet = authority.catalog.get(FACET_ID)
    values = {
        "facet_id": FACET_ID,
        "proposed_score": 100,
        "git_head": authority.catalog.git_head(),
        "catalog_sha256": authority.catalog.catalog_sha256,
        "registry_sha256": authority.catalog.registry_sha256,
        "builder_identity": "isolated-builder",
        "evaluator_identity": "sealed-evaluator",
        "metrics": {
            "acceptance_gate_pass_rate": 1.0,
            "p0_defects": 0,
            "p1_defects": 0,
        },
        "records": [
            _record(artifact, f"{FACET_ID}.implementation", "implementation"),
            _record(
                artifact,
                f"{FACET_ID}.runtime.chromium",
                "real_runtime",
                runtime="chromium",
            ),
            _record(
                artifact,
                f"{FACET_ID}.runtime.node",
                "real_runtime",
                runtime="node",
            ),
            _record(
                artifact,
                f"{FACET_ID}.holdout",
                "external_holdout",
                test_id=facet.required_external_or_holdout_tests[0],
                target_id="unseen-page-1",
                target_class="heldout-static-marketing",
            ),
            _record(
                artifact,
                facet.required_receipts[0],
                "receipt",
            ),
        ],
        "target_variants": ["unseen-page-1"],
        "reproduction_commands": ["uv run pytest tests/test_capability_scoring.py"],
        "unresolved_defects": {"P0": 0, "P1": 0},
    }
    values.update(overrides)
    return CapabilityEvidence.model_validate(values)


def test_catalog_preserves_every_original_facet_and_validates_schema() -> None:
    authority = CapabilityAuthority()
    document = json.loads(authority.catalog.facets_path.read_text(encoding="utf-8"))
    schema = json.loads(authority.catalog.schema_path.read_text(encoding="utf-8"))
    Draft202012Validator(schema).validate(document)

    assert len(authority.catalog.facets) == 139
    assert len({facet.id for facet in authority.catalog.facets}) == 139
    assert {facet.domain for facet in authority.catalog.facets} == {"app", "3d", "system"}
    assert all(facet.status == "PROVEN_BELOW_100" for facet in authority.catalog.facets)
    assert all(facet.current_score == facet.baseline_score for facet in authority.catalog.facets)


def test_valid_real_external_evidence_can_reach_100(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "executed-receipt.json"
    artifact.write_text('{"executed":true}\n', encoding="utf-8")

    evaluation = authority.evaluate(
        authority.catalog.get(FACET_ID),
        _evidence(authority, artifact),
    )

    assert evaluation.accepted is True
    assert evaluation.accepted_score == 100
    assert evaluation.status == "PROVEN_100_PLUS"
    assert evaluation.errors == []


def test_rejects_wrong_git_head_and_changed_thresholds(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")

    evidence = _evidence(
        authority,
        artifact,
        git_head="0" * 40,
        registry_sha256="1" * 64,
        thresholds_changed_after_run=True,
    )
    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert evaluation.accepted_score == 96
    assert any("Git head" in error for error in evaluation.errors)
    assert any("different acceptance thresholds" in error for error in evaluation.errors)
    assert any("thresholds changed" in error for error in evaluation.errors)


def test_rejects_fixture_simulation_adapter_and_stale_artifact(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")
    evidence = _evidence(authority, artifact)
    evidence.records[1].adapter_only = True
    evidence.records[2].simulated_hardware = True
    evidence.records[3].fixture_only = True
    evidence.records[0].artifact_sha256 = "f" * 64

    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert any("adapter presence" in error for error in evaluation.errors)
    assert any("simulated hardware" in error for error in evaluation.errors)
    assert any("fixture-only" in error for error in evaluation.errors)
    assert any("stale or substituted artifact" in error for error in evaluation.errors)


def test_rejects_evaluator_builder_overlap_and_hidden_edits(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")
    evidence = _evidence(
        authority,
        artifact,
        builder_identity="same-process",
        evaluator_identity="same-process",
        evaluator_had_builder_access=True,
        manual_edits_receipted=False,
    )

    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert any("identities must be isolated" in error for error in evaluation.errors)
    assert any("evaluator access" in error for error in evaluation.errors)
    assert any("manual edits" in error for error in evaluation.errors)


def test_rejects_unsupported_score_increase(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")
    evidence = _evidence(
        authority,
        artifact,
        proposed_score=97,
        records=[],
        reproduction_commands=[],
    )

    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert evaluation.accepted_score == 96
    assert any("unsupported score increase" in error for error in evaluation.errors)
    assert any("reproduction commands" in error for error in evaluation.errors)
    assert any("missing facet receipts" in error for error in evaluation.errors)


def test_105_requires_three_unseen_variants(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")
    evidence = _evidence(authority, artifact, proposed_score=105)

    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert any("three unseen target variants" in error for error in evaluation.errors)


def test_110_requires_recovery_repair_and_no_regression(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    artifact = tmp_path / "receipt.json"
    artifact.write_text("{}\n", encoding="utf-8")
    evidence = _evidence(
        authority,
        artifact,
        proposed_score=110,
        target_variants=["unseen-1", "unseen-2", "unseen-3"],
    )

    evaluation = authority.evaluate(authority.catalog.get(FACET_ID), evidence)

    assert evaluation.accepted is False
    assert any("zero global regressions" in error for error in evaluation.errors)
    assert any("adversarial recovery" in error for error in evaluation.errors)


def test_report_round_trip_detects_tampering(tmp_path: Path) -> None:
    authority = CapabilityAuthority()
    report_path = tmp_path / "scorecard.json"

    report = authority.report(output_path=report_path)
    verified = authority.verify_report(report_path)

    assert report.summary["facet_count"] == 139
    assert report.summary["facets_at_100_plus"] == 0
    assert verified["valid"] is True
    document = json.loads(report_path.read_text(encoding="utf-8"))
    document["summary"]["facets_at_100_plus"] = 139
    report_path.write_text(json.dumps(document), encoding="utf-8")
    tampered = authority.verify_report(report_path)
    assert tampered["valid"] is False
    assert any("report digest" in error for error in tampered["errors"])

    document = report.model_dump(mode="json")
    document["evaluations"][0]["accepted_score"] = 100
    unsigned = {key: value for key, value in document.items() if key != "report_sha256"}
    document["report_sha256"] = sha256_bytes(canonical_json_bytes(unsigned))
    report_path.write_text(json.dumps(document), encoding="utf-8")
    resigned_tamper = authority.verify_report(report_path)
    assert resigned_tamper["valid"] is False
    assert any(
        "do not reproduce from registered evidence" in error for error in resigned_tamper["errors"]
    )


def test_cli_exposes_required_capability_commands(tmp_path: Path) -> None:
    parser = build_parser()
    listed = dispatch(parser.parse_args(["capability", "list", "--domain", "system"]))
    shown = dispatch(parser.parse_args(["capability", "show", "system.evidence_traceability"]))
    evaluated = dispatch(parser.parse_args(["capability", "evaluate", "app"]))
    report_path = tmp_path / "report.json"
    reported = dispatch(parser.parse_args(["capability", "report", "--output", str(report_path)]))
    verified = dispatch(parser.parse_args(["capability", "verify-report", str(report_path)]))

    assert len(listed["facets"]) == 25
    assert shown["baseline_score"] == 97
    assert len(evaluated["evaluations"]) == 50
    assert reported["summary"]["facet_count"] == 139
    assert verified["valid"] is True
