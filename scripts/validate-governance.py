#!/usr/bin/env python3
"""Deterministically validate the release-blocking governance artifacts."""

from __future__ import annotations

import hashlib
import json
import sys
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


class ValidationError(Exception):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise ValidationError(message)


def read_json(path: str) -> dict:
    target = ROOT / path
    try:
        value = json.loads(target.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValidationError(f"{path}: invalid or unreadable JSON: {exc}") from exc
    require(isinstance(value, dict), f"{path}: root must be an object")
    return value


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def validate_documents() -> None:
    required = {
        "docs/CANARY_TERMS.md": ("DRAFT", "synthetic", "Stripe test-mode", "Built with Llama"),
        "docs/PRIVACY_NOTICE_DRAFT.md": ("DRAFT", "DO NOT PUBLISH", "supplier"),
        "docs/PRIVACY_DATA_GOVERNANCE.md": ("DRAFT", "Proposed", "backup"),
        "docs/DSAR_RUNBOOK.md": ("DRAFT", "not yet", "tombstone"),
        "docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md": ("DRAFT", "Prohibited use", "operator-controlled"),
        "docs/SUPPORT_AND_INCIDENT_RUNBOOK.md": ("DRAFT", "TABLETOP NOT EXECUTED", "Stripe"),
        "docs/THIRD_PARTY_LICENSES.md": ("INCOMPLETE", "RELEASE BLOCKING", "Llama"),
        "NOTICE": ("Built with Llama", "does not currently contain a project-level LICENSE", "Apache-2.0"),
    }
    for relative, needles in required.items():
        target = ROOT / relative
        require(target.is_file(), f"missing required document: {relative}")
        text = target.read_text(encoding="utf-8")
        for needle in needles:
            require(needle in text, f"{relative}: missing required marker {needle!r}")


def validate_legal() -> None:
    legal = read_json("ops/legal-review.json")
    require(legal.get("schema_version") == 1, "legal review schema_version must be 1")
    require(legal.get("status") == "NO_GO" and legal.get("decision") == "NO_GO",
            "legal review must remain NO_GO")
    require(legal.get("candidate_commit") is None,
            "legal review candidate must stay null until final clean-commit binding")
    require(legal.get("commercial", {}).get("real_money_allowed") is False,
            "legal review must prohibit real money")
    require(legal.get("commercial", {}).get("stripe_mode") == "test_only",
            "legal review must restrict Stripe to test mode")
    require(legal.get("commercial", {}).get("independently_owned_suppliers_allowed") is False,
            "legal review must prohibit independently owned suppliers")
    approvals = legal.get("approvals")
    require(isinstance(approvals, dict) and approvals, "legal approvals must be present")
    for name, approval in approvals.items():
        require(approval.get("status") == "PENDING", f"legal approval {name} must remain PENDING")
        require(approval.get("approver") is None and approval.get("approved_at") is None,
                f"legal approval {name} cannot imply a human approval")
    blockers = legal.get("open_blockers")
    require(isinstance(blockers, list) and len(blockers) >= 10,
            "legal review must retain the complete blocker set")
    require(any(item.get("severity") == "P0" for item in blockers),
            "legal review must retain the supplier-boundary P0")


def validate_economics() -> None:
    economics = read_json("ops/economics-readiness.json")
    require(economics.get("schema_version") == 1, "economics schema_version must be 1")
    require(economics.get("status") == "NO_GO" and economics.get("decision") == "NO_GO",
            "economics readiness must remain NO_GO")
    require(economics.get("candidate_commit") is None,
            "economics candidate must stay null until final clean-commit binding")
    limits = economics.get("cohort_limits", {})
    require(limits.get("supplier_ownership") == "operator_controlled_only",
            "economics must restrict supplier ownership")
    require(limits.get("max_real_cash_usd") == 0 and limits.get("max_supplier_liability_usd") == 0,
            "economics must prohibit real cash and supplier liability")
    require(economics.get("reconciliation", {}).get("stripe_mode") == "test_only",
            "economics must restrict reconciliation to Stripe test mode")
    require(economics.get("canary_evidence", {}).get("status") == "NO_CANARY_SAMPLE",
            "economics must not claim uncollected canary evidence")
    require(len(economics.get("stop_conditions", [])) >= 8,
            "economics must define fail-closed stop conditions")
    require(len(economics.get("open_blockers", [])) >= 8,
            "economics must retain the complete blocker set")
    for name, approval in economics.get("approvals", {}).items():
        require(approval.get("status") == "PENDING", f"economics approval {name} must remain PENDING")
        require(approval.get("approver") is None and approval.get("approved_at") is None,
                f"economics approval {name} cannot imply a human approval")


def validate_tabletop() -> None:
    tabletop = read_json("ops/support-incident-tabletop.json")
    require(tabletop.get("status") == "NOT_EXECUTED", "tabletop must remain NOT_EXECUTED")
    require(tabletop.get("candidate_commit") is None, "unexecuted tabletop cannot bind a candidate")
    require(not tabletop.get("participants"), "unexecuted tabletop cannot name participants")
    require(tabletop.get("approved_at") is None, "unexecuted tabletop cannot be approved")
    scenarios = tabletop.get("planned_scenarios", [])
    require(len(scenarios) >= 5, "tabletop must retain all planned scenarios")
    require(all(item.get("result") == "NOT_RUN" for item in scenarios),
            "unexecuted tabletop scenarios must remain NOT_RUN")


def validate_assets() -> None:
    provenance = read_json("ops/asset-provenance.json")
    require(provenance.get("status") == "BLOCKED_INCOMPLETE_PROVENANCE",
            "asset provenance must remain blocked")
    assets = provenance.get("assets", [])
    require(assets, "asset provenance must list tracked assets")
    seen: set[str] = set()
    for asset in assets:
        relative = asset.get("path")
        require(isinstance(relative, str) and relative not in seen,
                f"invalid or duplicate asset path: {relative!r}")
        seen.add(relative)
        target = ROOT / relative
        require(target.is_file(), f"asset missing: {relative}")
        observed = sha256(target)
        require(observed == asset.get("sha256"),
                f"asset hash mismatch for {relative}: expected {asset.get('sha256')}, got {observed}")
        require(str(asset.get("status", "")).startswith("BLOCKED_"),
                f"asset {relative} must retain a blocked provenance status")


def validate_models() -> None:
    provenance = read_json("ops/model-provenance.json")
    authority = read_json("control/runtime-authority.json")
    require(provenance.get("status") == "BLOCKED_LICENSE_AND_FINAL_CANDIDATE_BINDING",
            "model provenance must remain blocked for licensing and final binding")
    authority_models = {item["id"]: item for item in authority.get("models", [])}
    rust = (ROOT / "agent/src/models.rs").read_text(encoding="utf-8")
    for model in provenance.get("models", []):
        model_id = model.get("catalog_id")
        declared = authority_models.get(model_id)
        require(declared is not None, f"model missing from runtime authority: {model_id}")
        require(declared.get("hf_repo") == model.get("repository"),
                f"repository mismatch for {model_id}")
        require(declared.get("hf_revision") == model.get("declared_revision"),
                f"declared revision mismatch for {model_id}")
        require(model.get("enforced_revision") == model.get("declared_revision"),
                f"model {model_id} enforced revision must match runtime authority")
        require(model.get("review_status") == "BLOCKED",
                f"model {model_id} must remain blocked")
        require(model.get("repository") in rust,
                f"agent model repository not found in source for {model_id}")
        require(model.get("declared_revision") in rust,
                f"agent model revision not found in source for {model_id}")
        declared_artifacts = model.get("declared_artifacts", {})
        for artifact in declared.get("artifacts", []):
            if artifact.get("repo"):
                key = f"{artifact['path']}@{artifact['repo']}#{artifact['revision']}"
            else:
                key = artifact["path"]
            record = declared_artifacts.get(key)
            require(record is not None, f"provenance missing declared artifact {model_id}/{key}")
            require(record.get("sha256") == artifact.get("sha256") and
                    record.get("size_bytes") == artifact.get("bytes"),
                    f"declared artifact mismatch for {model_id}/{key}")
            require(record.get("sha256") in rust,
                    f"agent source does not enforce declared hash for {model_id}/{key}")
    require("Repo::with_revision" in rust and "verify_file" in rust,
            "agent source must select revisions and verify model files")


def main() -> int:
    checks = (
        ("documents", validate_documents),
        ("legal", validate_legal),
        ("economics", validate_economics),
        ("tabletop", validate_tabletop),
        ("assets", validate_assets),
        ("models", validate_models),
    )
    try:
        for name, check in checks:
            check()
            print(f"PASS {name}")
    except ValidationError as exc:
        print(f"FAIL {exc}", file=sys.stderr)
        return 1
    print("PASS governance artifacts are internally consistent and remain fail-closed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
