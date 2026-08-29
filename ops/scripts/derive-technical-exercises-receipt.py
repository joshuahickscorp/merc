#!/usr/bin/env python3
"""Derive technical-exercises.json booleans from go test -json output.

No field may be true unless the owning test reported Action=pass.
A failing or missing required test yields status FAIL and exit 1.
"""

from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))
from lib.evidence_binding import EvidenceBindingError, emit_bound_json  # noqa: E402
from lib.receipt_binding import candidate_commit, stamp  # noqa: E402

REQUIRED_TESTS = [
    "TestBuyerObjectDeletionQueueAndSweep",
    "TestDSARDeletionTombstoneAndRestoreReplay",
    "TestSupportAndSecurityTechnicalTabletops",
    "TestPrivilegedAdminMutationsHaveCompleteAtomicAudit",
    "TestPrivilegedMutationIdempotentConcurrentReplay",
    "TestConcurrentNamedOperatorsRetainIndependentAttribution",
    "TestRevocationWinsRaceBeforePrivilegedMutation",
    "TestAdminMutationRollsBackWhenAuditInsertFails",
]


def parse_results(events_path: Path) -> dict[str, str]:
    results: dict[str, str] = {}
    text = events_path.read_text(encoding="utf-8", errors="replace")
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        test = event.get("Test")
        action = event.get("Action")
        if not test or action not in {"pass", "fail", "skip"}:
            continue
        if "/" in test:
            continue
        results[test] = action
    return results


def passed(results: dict[str, str], name: str) -> bool:
    return results.get(name) == "pass"


def flags(ok: bool, names: list[str]) -> dict[str, bool]:
    return {name: bool(ok) for name in names}


def build_receipt(results: dict[str, str], go_exit: int, events_path: Path) -> dict:
    missing = [name for name in REQUIRED_TESTS if name not in results]
    dsar_ok = passed(results, "TestDSARDeletionTombstoneAndRestoreReplay")
    # Object erasure is a strictly stronger claim than the DSAR replay: it needs
    # the bytes gone from object storage, not just the database pointers nulled.
    # It therefore has its own test and its own flag, because the previous
    # receipt asserted deletable_data_removed while src/control/storage.go had no
    # delete method at all.
    erasure_ok = passed(results, "TestBuyerObjectDeletionQueueAndSweep")
    tabletop_ok = passed(results, "TestSupportAndSecurityTechnicalTabletops")
    audit_ok = passed(results, "TestPrivilegedAdminMutationsHaveCompleteAtomicAudit")
    idempotent_ok = passed(results, "TestPrivilegedMutationIdempotentConcurrentReplay")
    concurrent_ok = passed(results, "TestConcurrentNamedOperatorsRetainIndependentAttribution")
    revocation_ok = passed(results, "TestRevocationWinsRaceBeforePrivilegedMutation")
    audit_fail_aborts_ok = passed(results, "TestAdminMutationRollsBackWhenAuditInsertFails")
    break_glass_all = all(
        (audit_ok, idempotent_ok, concurrent_ok, revocation_ok, audit_fail_aborts_ok)
    )
    all_required_pass = not missing and all(passed(results, n) for n in REQUIRED_TESTS)
    status = "PASS" if go_exit == 0 and all_required_pass else "FAIL"

    return {
        "schema_version": 1,
        "status": status,
        "completed_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "go_test_exit_code": go_exit,
        "go_test_jsonl": str(events_path),
        "test_results": {name: results.get(name, "missing") for name in REQUIRED_TESTS},
        "dsar": flags(dsar_ok, [
            "export_complete", "foreign_data_absent", "secrets_redacted",
            "artifacts_in_manifest", "payment_records", "provenance",
        ]),
        "deletion": {
            **flags(dsar_ok, [
                "minimal_financial_retention", "artifact_expiry_harness",
                "authentication_revoked", "foreign_keys_valid",
            ]),
            "deletable_data_removed": erasure_ok,
        },
        "tombstone": flags(dsar_ok, [
            "authentication_denied", "submission_denied", "private_retrieval_denied",
            "silent_recreation_denied", "minimal_identity_retained",
            "pre_deletion_restore_replayed",
        ]),
        "provenance": flags(dsar_ok, [
            "request", "input", "quote", "submission", "tasks", "agent_identity",
            "model_revision", "attempt", "result", "verification", "finalization",
            "ledger", "webhook", "retrieval",
        ]),
        "support_tabletop": {
            "status": "PASS" if tabletop_ok else "FAIL",
            **flags(tabletop_ok, [
                "settlement_mismatch_detected", "reconciled_to_immutable_ledger",
                "safe_correction_proposal", "communication_draft", "postmortem_required",
            ]),
        },
        "security_tabletop": {
            "status": "PASS" if tabletop_ok else "FAIL",
            "cross_tenant_access": False if tabletop_ok else True,
            "intake_pause_procedure": tabletop_ok,
            "token_revocation": tabletop_ok,
            "leaked_url_issued": False if tabletop_ok else True,
            "url_presign_denied_after_owner_check": tabletop_ok,
            "canonical_artifact_reference_revoked": tabletop_ok,
            "audit_export": tabletop_ok,
            "containment": tabletop_ok,
            "recovery_intake_resumed": tabletop_ok,
            "severity_if_access_succeeded": "P0",
        },
        "break_glass": {
            "status": "PASS" if break_glass_all else "FAIL",
            "named_operator": audit_ok,
            "reason": audit_ok,
            "incident_reference": audit_ok,
            "before_after": audit_ok,
            "immutable_audit": audit_ok,
            "unauthorized_rejected": audit_ok,
            "audit_failure_aborts": audit_fail_aborts_ok,
            "concurrent_operators": concurrent_ok,
            "idempotent_retries": idempotent_ok,
            "revocation_wins_race": revocation_ok,
        },
        "qualification": {
            "technical_tabletop": "PASS" if tabletop_ok else "FAIL",
            "qualified_human_tabletop": "NOT EXECUTED",
            "external_subprocessor_deletion": "NOT EXECUTED",
        },
    }


def main() -> int:
    if len(sys.argv) != 4:
        print(
            "usage: derive-technical-exercises-receipt.py EVENTS.jsonl OUT.json GO_EXIT",
            file=sys.stderr,
        )
        return 2
    events_path = Path(sys.argv[1])
    evidence_path = Path(sys.argv[2])
    go_exit = int(sys.argv[3])
    results = parse_results(events_path)
    receipt = build_receipt(results, go_exit, events_path)
    stamp(
        receipt,
        candidate_commit(str(ROOT)),
        "ops/scripts/derive-technical-exercises-receipt.py",
    )
    try:
        emit_bound_json(
            evidence_path,
            receipt,
            harness="ops/scripts/derive-technical-exercises-receipt.py",
            repo_root=ROOT,
            build_binary_path=Path(__file__).resolve(),
            exact_config=f"go test -json events at {events_path.name}; exit={go_exit}",
            raw_samples="embedded test_results from go test -json",
            model_na="technical exercises do not load model weights",
            image_na="no container image in this measurement",
            corpus_na="no external corpus",
        )
    except EvidenceBindingError as exc:
        print(f"derive-technical-exercises-receipt: REFUSED: {exc}", file=sys.stderr)
        return 2
    summary = {
        "status": receipt["status"],
        "go_test_exit_code": go_exit,
        "test_results": receipt["test_results"],
    }
    print(json.dumps(summary, indent=2))
    return 0 if receipt["status"] == "PASS" else 1


if __name__ == "__main__":
    raise SystemExit(main())
