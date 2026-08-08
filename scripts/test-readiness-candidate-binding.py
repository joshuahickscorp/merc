#!/usr/bin/env python3
"""Prove external readiness approvals cannot self-select their candidate SHA.

The readiness validator is deliberately usable by local operators and CI.  Its
external receipts must nevertheless bind to an independently supplied
candidate identity.  These adversarial checks exercise the two approval paths
that used to accept any syntactically valid commit.
"""

from __future__ import annotations

import importlib.util
import os
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
VALIDATOR = ROOT / "scripts" / "validate-readiness.py"
EXPECTED = "a" * 40
FOREIGN = "b" * 40


def load_validator():
    spec = importlib.util.spec_from_file_location("merc_readiness", VALIDATOR)
    if spec is None or spec.loader is None:
        raise AssertionError("cannot load readiness validator")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def approval() -> dict[str, object]:
    return {
        "status": "APPROVED",
        "approver": "Riley Oren",
        "organization": "Merc Governance",
        "reviewed_scope": "supervised private canary approval",
        "evidence_uri": "https://records.merc.invalid/audits/20260808",
        "approved_at": "2026-08-08T12:00:00Z",
    }


def exercise() -> dict[str, object]:
    return {
        "status": "PASS",
        "evidence_uri": "https://records.merc.invalid/exercises/20260808",
        "completed_at": "2026-08-08T12:30:00Z",
    }


def privacy_receipt(candidate_commit: str) -> dict[str, object]:
    return {
        "kind": "privacy_qualified_approval",
        "status": "PASS",
        "secret_values_recorded": False,
        "schema_version": 1,
        "scope": "supervised_stripe_test_mode_private_canary",
        "candidate_commit": candidate_commit,
        "approval": approval(),
        "dsar_export_deletion": exercise(),
        "external_subprocessor_deletion": {
            "status": "PASS",
            "executed": True,
            "subprocessor": "Archive Authority",
            "completed_at": "2026-08-08T12:45:00Z",
            "evidence_uri": "https://records.merc.invalid/deletions/20260808",
        },
    }


def licensing_receipt(candidate_commit: str) -> dict[str, object]:
    return {
        "kind": "licensing_provenance_approval",
        "status": "PASS",
        "secret_values_recorded": False,
        "schema_version": 1,
        "scope": "supervised_stripe_test_mode_private_canary",
        "candidate_commit": candidate_commit,
        "approval": approval(),
        "asset_and_model_provenance": exercise(),
        "provenance_register": {
            "status": "APPROVED",
            "blocked_rows_remaining": 0,
            "register_uri": "https://records.merc.invalid/provenance/20260808",
        },
    }


def main() -> int:
    module = load_validator()
    previous_commit = os.environ.get("MERC_READINESS_EXPECTED_COMMIT")
    previous_candidate = os.environ.get("MERC_CANDIDATE_COMMIT")
    original_load_json = module.load_json
    try:
        os.environ["MERC_READINESS_EXPECTED_COMMIT"] = EXPECTED
        os.environ.pop("MERC_CANDIDATE_COMMIT", None)

        def controlled_ledger(path: str):
            if path == "evidence/autonomous/technical-exercises.json":
                return {"qualification": {"external_subprocessor_deletion": "PASS"}}
            if path == "ops/asset-provenance.json":
                return {"status": "APPROVED"}
            return None

        module.load_json = controlled_ledger

        if not module.privacy_qualified_approval_proven(privacy_receipt(EXPECTED)):
            raise AssertionError("matching privacy receipt was rejected")
        if module.privacy_qualified_approval_proven(privacy_receipt(FOREIGN)):
            raise AssertionError("foreign privacy candidate was accepted")
        if not module.licensing_provenance_approval_proven(licensing_receipt(EXPECTED)):
            raise AssertionError("matching licensing receipt was rejected")
        if module.licensing_provenance_approval_proven(licensing_receipt(FOREIGN)):
            raise AssertionError("foreign licensing candidate was accepted")
        blocked = licensing_receipt(EXPECTED)
        blocked["provenance_register"] = {
            "status": "APPROVED",
            "blocked_rows_remaining": 0,
            "register_uri": "https://records.merc.invalid/provenance/20260808",
            "finding": "BLOCKED supplier artifact",
        }
        if module.licensing_provenance_approval_proven(blocked):
            raise AssertionError("licensing receipt with a blocked finding was accepted")

        os.environ.pop("MERC_READINESS_EXPECTED_COMMIT", None)
        if module.privacy_qualified_approval_proven(privacy_receipt(EXPECTED)):
            raise AssertionError("self-selected privacy candidate was accepted")
        if module.licensing_provenance_approval_proven(licensing_receipt(EXPECTED)):
            raise AssertionError("self-selected licensing candidate was accepted")
    finally:
        module.load_json = original_load_json
        if previous_commit is None:
            os.environ.pop("MERC_READINESS_EXPECTED_COMMIT", None)
        else:
            os.environ["MERC_READINESS_EXPECTED_COMMIT"] = previous_commit
        if previous_candidate is None:
            os.environ.pop("MERC_CANDIDATE_COMMIT", None)
        else:
            os.environ["MERC_CANDIDATE_COMMIT"] = previous_candidate

    print("readiness-candidate-binding: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
