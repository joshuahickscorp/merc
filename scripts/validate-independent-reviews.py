#!/usr/bin/env python3
"""Validate expanded review coverage and required review-record fields."""

import json
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PATH = ROOT / "ops" / "independent-reviews.json"
REQUIRED_DOMAINS = {
    "security_red_team",
    "privacy_data_governance",
    "legal_commercial",
    "licensing_models_ip",
    "abuse_trust",
    "economics",
    "supplier_quality",
    "reliability_load_soak",
    "observability",
    "support_incident_response",
    "change_management_governance",
    "dependency_registry",
    "website_buyer_ux",
    "operations_recovery",
}
REQUIRED_FIELDS = {
    "scope",
    "threat_or_failure_model",
    "findings",
    "repair",
    "verification",
    "residual_risk",
}


def fail(message: str) -> None:
    print(f"independent reviews: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


document = json.loads(PATH.read_text())
if document.get("schema_version") != 1:
    fail("schema_version must be 1")
reviews = document.get("reviews")
if not isinstance(reviews, list):
    fail("reviews must be an array")
ids = [review.get("id") for review in reviews]
if len(ids) != len(set(ids)):
    fail("review ids must be unique")
if set(ids) != REQUIRED_DOMAINS:
    fail(f"domain mismatch missing={sorted(REQUIRED_DOMAINS - set(ids))} extra={sorted(set(ids) - REQUIRED_DOMAINS)}")
for review in reviews:
    missing = REQUIRED_FIELDS - set(review)
    if missing:
        fail(f"{review['id']} is missing {sorted(missing)}")
    for field in REQUIRED_FIELDS:
        if not isinstance(review[field], list) or not review[field]:
            fail(f"{review['id']}.{field} must be a non-empty array")
    if not review.get("reviewer_track") or not review.get("outcome"):
        fail(f"{review['id']} must record reviewer_track and outcome")
    for finding in review["findings"]:
        if not all(finding.get(key) for key in ("id", "severity", "status", "evidence")):
            fail(f"{review['id']} has an incomplete finding")

print(f"independent reviews: PASS ({len(reviews)} domains, required fields complete)")
