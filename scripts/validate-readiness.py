#!/usr/bin/env python3
"""Fail-closed readiness: domain scores are derived from machine receipts only.

Hand-typed `earned` fields in ops/readiness.json are advisory and ignored.
Where a named receipt is missing or fails its content check, that receipt
contributes zero points. Live money / public launch must stay NO_GO_PROHIBITED.
"""

from __future__ import annotations

import json
import sys
from pathlib import Path
from typing import Any, Callable

ROOT = Path(__file__).resolve().parents[1]


def fail(message: str) -> None:
    print(f"readiness: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def load_json(relative: str) -> Any:
    path = ROOT / relative
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def status_in(*allowed: str) -> Callable[[Any], bool]:
    allowed_set = {a.upper() for a in allowed}

    def check(doc: Any) -> bool:
        if not isinstance(doc, dict):
            return False
        status = str(doc.get("status", "")).upper()
        return status in allowed_set

    return check


def file_exists(_doc: Any) -> bool:
    return True


def auth_matrix_complete(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    routes = 0
    for route_class in doc.get("route_classes", []):
        routes += len(route_class.get("routes", []))
    # 87 after the currently registered public and operator routes. The count is a tripwire,
    # not a fact about the world: it exists so that adding a route forces someone
    # to look at the matrix. It went stale when that route landed, which silently
    # cost this domain 11 readiness points and made `make ci` red.
    return routes == 87 and doc.get("policy", {}).get("default") == "deny"


def technical_break_glass(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bg = doc.get("break_glass") if isinstance(doc, dict) else None
    return isinstance(bg, dict) and str(bg.get("status", "")).upper() == "PASS"


def technical_privacy(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("dsar", "deletion", "tombstone"):
        block = doc.get(section)
        if not isinstance(block, dict) or not any(block.values()):
            return False
    return True


def technical_tabletops(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("support_tabletop", "security_tabletop"):
        block = doc.get(section)
        if not isinstance(block, dict) or str(block.get("status", "")).upper() != "PASS":
            return False
    return True


def soak_clean(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bounds = doc.get("observed_bounds") if isinstance(doc, dict) else None
    if not isinstance(bounds, dict):
        return False
    restarts = bounds.get("control_restart_count")
    if isinstance(restarts, dict):
        restart_max = restarts.get("max", 1)
    else:
        restart_max = restarts if isinstance(restarts, (int, float)) else 1
    oom = bounds.get("control_oom_samples", 1)
    return restart_max == 0 and oom == 0


def payment_simulated(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    status = str(doc.get("status", "")).upper()
    label = str(doc.get("evidence_label", "")).upper()
    return "SIMULATED" in status or label == "SIMULATED"


# Each domain's possible points are fixed. earned is the sum of points for
# receipts that exist and pass their content check. Missing/failed => 0.
DOMAIN_RECEIPTS: dict[str, dict[str, Any]] = {
    "source_and_ci": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/registry-verification.json", status_in("PASS"), 4),
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 3),
            ("ops/authorization-matrix.json", auth_matrix_complete, 3),
        ],
    },
    "security": {
        "possible": 15,
        "receipts": [
            ("ops/authorization-matrix.json", auth_matrix_complete, 8),
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 6),
            # external staging attack rehearsal has no receipt → remaining 1 point unearned
        ],
    },
    "money_and_reconciliation": {
        "possible": 15,
        "receipts": [
            ("evidence/autonomous/payment-simulator.json", payment_simulated, 9),
            # Stripe sandbox matrix / provider reconciliation: no receipt → 0
        ],
    },
    "lifecycle_and_concurrency": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 5),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 5),
        ],
    },
    "artifacts_and_storage": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 6),
            # independent external offsite copy: no receipt → 0
        ],
    },
    "agent_and_sandbox": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/hardware-characterization.json", status_in("PASS"), 8),
        ],
    },
    "database_and_recovery": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 4),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 3),
            # external offsite restore: no receipt → 0
        ],
    },
    "deployment_and_rollback": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/staging-validation.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 1),
            ("evidence/autonomous/local-soak-60s.json", soak_clean, 0),
            # qualifying 24h soak / persistent staging: no receipt → remaining unearned
        ],
    },
    "observability_and_alerting": {
        "possible": 6,
        "receipts": [
            ("evidence/autonomous/alert-pipeline-simulation.json", status_in("PASS"), 3),
            ("evidence/autonomous/alert-page-simulation.json", status_in("PASS"), 2),
            # genuine receiver delivery: no receipt → 0
        ],
    },
    "privacy_and_data_governance": {
        "possible": 4,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_privacy, 3),
            # qualified privacy approval / external subprocessor deletion: no receipt → 0
        ],
    },
    "licensing_and_supply_chain": {
        "possible": 3,
        "receipts": [
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 2),
            # license/asset provenance approval: no receipt → 0
        ],
    },
    "abuse_and_trust": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
            # staffed human route / qualified tabletop: no receipt → 0
        ],
    },
    "support_and_incident_response": {
        "possible": 1,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
        ],
    },
    "website_and_buyer_usability": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/website-validation.json", status_in("PASS", "PASS_AUTOMATED_BROWSER"), 2),
        ],
    },
}


def derive_domain_score(domain_id: str) -> tuple[int, int, list[str]]:
    spec = DOMAIN_RECEIPTS[domain_id]
    possible = int(spec["possible"])
    earned = 0
    notes: list[str] = []
    for relative, checker, points in spec["receipts"]:
        path = ROOT / relative
        if not path.is_file():
            notes.append(f"{relative}: MISSING → 0/{points}")
            continue
        doc = load_json(relative) if relative.endswith(".json") else True
        if relative.endswith(".json") and doc is None:
            notes.append(f"{relative}: UNREADABLE → 0/{points}")
            continue
        if not checker(doc):
            notes.append(f"{relative}: CHECK_FAILED → 0/{points}")
            continue
        earned += int(points)
        notes.append(f"{relative}: OK → {points}/{points}")
    if earned > possible:
        fail(f"{domain_id}: derived earned {earned} exceeds possible {possible}")
    return earned, possible, notes


def main() -> None:
    readiness_path = ROOT / "ops" / "readiness.json"
    decision_path = ROOT / "ops" / "go-no-go.json"
    try:
        readiness = json.loads(readiness_path.read_text(encoding="utf-8"))
        decision = json.loads(decision_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot load readiness ledgers: {exc}")

    score = readiness.get("weighted_score") or {}
    domains = score.get("domains")
    if not isinstance(domains, list) or not domains:
        fail("weighted_score.domains must be a non-empty array")

    declared_ids = [item.get("id") for item in domains]
    expected_ids = list(DOMAIN_RECEIPTS.keys())
    if declared_ids != expected_ids:
        fail(
            "domain id axis mismatch "
            f"missing={sorted(set(expected_ids) - set(declared_ids))} "
            f"extra={sorted(set(declared_ids) - set(expected_ids))} "
            f"order_or_dup=declared_ids!=expected"
        )

    derived_total = 0
    possible_total = 0
    per_domain: list[str] = []
    for item in domains:
        domain_id = item["id"]
        declared_possible = int(item.get("possible", -1))
        earned, possible, notes = derive_domain_score(domain_id)
        if declared_possible != possible:
            fail(f"{domain_id}: possible {declared_possible} != receipt schedule {possible}")
        # Hand-typed earned is advisory only — never authoritative.
        advisory = item.get("earned")
        if advisory is not None and int(advisory) != earned:
            per_domain.append(
                f"{domain_id}: derived={earned}/{possible} "
                f"(advisory earned={advisory} ignored)"
            )
        else:
            per_domain.append(f"{domain_id}: derived={earned}/{possible}")
        for note in notes:
            if "MISSING" in note or "FAILED" in note or "UNREADABLE" in note:
                per_domain.append(f"  - {note}")
        derived_total += earned
        possible_total += possible

    if possible_total != 100:
        fail(f"domain possibles sum to {possible_total}, want 100")

    if decision.get("readiness_score") != derived_total:
        fail(
            f"decision readiness_score {decision.get('readiness_score')} "
            f"!= receipt-derived total {derived_total}"
        )

    open_p0 = decision.get("open_p0")
    open_p1 = decision.get("open_p1")
    if not isinstance(open_p0, list) or not isinstance(open_p1, list):
        fail("open_p0 and open_p1 must be arrays")

    severity = readiness.get("severity") or {}
    if severity.get("target_scope_open_p0") != len(open_p0):
        fail("open target-scope P0 count differs")
    if severity.get("target_scope_open_p1") != len(open_p1):
        fail("open target-scope P1 count differs")

    level_b = decision.get("decisions", {}).get("supervised_stripe_test_mode_private_canary")
    go_threshold = int(decision.get("go_threshold", score.get("go_threshold", 95)))
    if derived_total < go_threshold or open_p0 or open_p1:
        if level_b != "NO_GO":
            fail("an under-threshold or blocked Level B must be NO_GO")

    if decision.get("decisions", {}).get("live_money_or_public_launch") != "NO_GO_PROHIBITED":
        fail("live money/public launch must remain explicitly prohibited")

    if decision.get("machine_input_request") != "ops/go-closure-inputs.json":
        fail("decision must point to the single exact input request")

    print(
        f"readiness: PASS ({derived_total}/100 derived, P0={len(open_p0)}, "
        f"P1={len(open_p1)}, Level B {level_b})"
    )
    for line in per_domain:
        print(f"  {line}")


if __name__ == "__main__":
    main()
