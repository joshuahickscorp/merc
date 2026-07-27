#!/usr/bin/env python3
"""Validate agent review notes: resolvable evidence, closed outcomes, real methods.

The artifact was renamed from independent-reviews.json to agent-review-notes.json
because the content is parallel-agent inspection, not independent human review.
"""

from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
PATH = ROOT / "ops" / "agent-review-notes.json"
LEGACY = ROOT / "ops" / "independent-reviews.json"

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

# Closed outcome vocabulary — free text like "SHIP IT" is rejected.
ALLOWED_OUTCOMES = {
    "PASS_LOCAL_WITH_EXTERNAL_RESIDUALS",
    "NO_GO_PENDING_QUALIFIED_APPROVAL_AND_EXTERNAL_PROCESS",
    "NO_GO_PENDING_QUALIFIED_APPROVAL",
    "NO_GO_PENDING_PROVENANCE_AND_LICENSE_APPROVAL",
    "NO_GO_PENDING_QUALIFIED_HUMAN_TABLETOP_AND_APPROVAL",
    "PASS_CODE_NO_GO_CANARY_EVIDENCE",
    "PASS_LOCAL_NO_GO_EXTERNAL_FLEET",
    "NO_GO_PENDING_PERSISTENT_EXTERNAL_EXECUTION_AND_24H_SOAK",
    "PASS_SIMULATED_NO_GO_REAL_RECEIVER",
    "NO_GO_PENDING_NAMED_PEOPLE_AND_QUALIFIED_HUMAN_TABLETOP",
    "PASS_REPOSITORY_NO_GO_FINAL_REVIEW",
    "PASS_PUBLICATION_NO_GO_LICENSE",
    "PASS_STATIC_AND_BROWSER",
    "NO_GO_PENDING_EXTERNAL_RESOURCES",
}

# Non-methods: fabricated process claims. Keep phrases specific so legitimate
# engineering notes (e.g. "placeholder expansion") are not rejected.
METHOD_DENYLIST = re.compile(
    r"(?i)("
    r"\bvibes\b|"
    r"i made all of this up|"
    r"made (this|it) all up|"
    r"made (this|it) up in a text editor|"
    r"\btrust me\b|"
    r"\bhand[- ]?waved?\b|"
    r"\bjust ship it\b|"
    r"\bmy cat\b"
    r")"
)

# Evidence must be a repo path, optionally with :line or :line-line.
EVIDENCE_RE = re.compile(
    r"^(?P<path>(?:[\w.-]+/)*[\w.-]+)(?::(?P<line>\d+)(?:-(?P<end>\d+))?)?$"
)


def fail(message: str) -> None:
    print(f"agent review notes: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def resolve_evidence(value: str, context: str) -> None:
    if not isinstance(value, str) or not value.strip():
        fail(f"{context}: evidence must be a non-empty string path")
    text = value.strip()
    # Reject free-prose evidence: must look like a path reference only.
    match = EVIDENCE_RE.match(text)
    if not match:
        fail(
            f"{context}: evidence must be a repo path or path:line, not free text "
            f"({text!r})"
        )
    rel = match.group("path")
    target = ROOT / rel
    if not target.exists():
        fail(f"{context}: evidence path does not exist: {rel}")
    line = match.group("line")
    if line is not None and target.is_file():
        try:
            n_lines = sum(1 for _ in target.open(encoding="utf-8", errors="replace"))
        except OSError as exc:
            fail(f"{context}: cannot read evidence file {rel}: {exc}")
        line_no = int(line)
        if line_no < 1 or line_no > n_lines:
            fail(f"{context}: evidence line {line_no} out of range for {rel} ({n_lines} lines)")
        end = match.group("end")
        if end is not None:
            end_no = int(end)
            if end_no < line_no or end_no > n_lines:
                fail(f"{context}: evidence line range {line_no}-{end_no} invalid for {rel}")


if LEGACY.is_file():
    fail(
        "ops/independent-reviews.json still present; rename to "
        "ops/agent-review-notes.json (independence is not earned)"
    )

if not PATH.is_file():
    fail(f"missing {PATH.relative_to(ROOT)}")

document = json.loads(PATH.read_text(encoding="utf-8"))
if document.get("schema_version") != 1:
    fail("schema_version must be 1")

method = document.get("method")
if not isinstance(method, str) or not method.strip():
    fail("top-level method must be a non-empty string")
if METHOD_DENYLIST.search(method):
    fail(f"method matches non-method denylist: {method!r}")

reviews = document.get("reviews")
if not isinstance(reviews, list):
    fail("reviews must be an array")
ids = [review.get("id") for review in reviews]
if len(ids) != len(set(ids)):
    fail("review ids must be unique")
if set(ids) != REQUIRED_DOMAINS:
    fail(
        f"domain mismatch missing={sorted(REQUIRED_DOMAINS - set(ids))} "
        f"extra={sorted(set(ids) - REQUIRED_DOMAINS)}"
    )

for review in reviews:
    rid = review.get("id", "<unknown>")
    missing = REQUIRED_FIELDS - set(review)
    if missing:
        fail(f"{rid} is missing {sorted(missing)}")
    for field in REQUIRED_FIELDS:
        if not isinstance(review[field], list) or not review[field]:
            fail(f"{rid}.{field} must be a non-empty array")
    track = review.get("reviewer_track")
    if not isinstance(track, str) or not track.strip():
        fail(f"{rid} must record reviewer_track")
    if METHOD_DENYLIST.search(track):
        fail(f"{rid}.reviewer_track matches non-method denylist: {track!r}")
    outcome = review.get("outcome")
    if outcome not in ALLOWED_OUTCOMES:
        fail(f"{rid}.outcome {outcome!r} is not in the closed outcome enum")
    for finding in review["findings"]:
        if not all(finding.get(key) for key in ("id", "severity", "status", "evidence")):
            fail(f"{rid} has an incomplete finding")
        resolve_evidence(finding["evidence"], f"{rid}.{finding.get('id')}")
    # verification entries that are pure path references must resolve.
    for item in review["verification"]:
        if not isinstance(item, str) or not item.strip():
            fail(f"{rid}.verification entries must be non-empty strings")
        stripped = item.strip()
        if EVIDENCE_RE.match(stripped) and (
            "/" in stripped
            or stripped.endswith((".json", ".go", ".rs", ".md", ".py", ".mjs", ".sh"))
        ):
            resolve_evidence(stripped, f"{rid}.verification")

print(
    f"agent review notes: PASS ({len(reviews)} domains, resolvable evidence, "
    f"closed outcomes)"
)
