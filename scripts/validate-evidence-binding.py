#!/usr/bin/env python3
"""CI gate: unbound evidence must not exist unlabelled, and must not back claims.

Fails when any artifact under evidence/** is:
  (a) missing binding_status
  (b) UNBOUND yet cited by a doc or by code
  (c) claims BOUND while missing an applicable identity field

Also fails on non-object evidence without a sidecar binding file
(path + ".binding.json").

    python3 scripts/validate-evidence-binding.py
    python3 scripts/validate-evidence-binding.py --summary-only

Exit 0 only when the tree is clean. Today's historical tree is expected to fail
loudly while UNBOUND artifacts remain cited.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    BINDING_BOUND,
    BINDING_SUPERSEDED,
    BINDING_UNBOUND,
    BINDING_WITHDRAWN,
    BINDING_META_KEYS,
    binding_sidecar_path,
    incomplete_fields,
    is_job_contract_payload,
    missing_fields_for_object,
    validate_git_object,
    EvidenceBindingError,
)

EVIDENCE = ROOT / "evidence"
VALID_STATUSES = {
    BINDING_BOUND,
    BINDING_UNBOUND,
    BINDING_SUPERSEDED,
    BINDING_WITHDRAWN,
}

# Surfaces that may cite evidence paths. Citations in evidence/ itself are
# ignored (receipts may reference peers).
CITE_ROOTS = (
    ROOT / "docs",
    ROOT / "control",
    ROOT / "scripts",
    ROOT / "agent",
    ROOT / "ops",
    ROOT / "proof",
    ROOT / "README.md",
    ROOT / "RELEASE_READINESS.md",
    ROOT / "RELEASE_GATES.md",
    ROOT / "REQUIREMENT_PROOF_MATRIX.md",
    ROOT / "ROADMAP.md",
    ROOT / "RUNBOOK_ARTIFACTS.md",
    ROOT / "RUNBOOK_WORKER_FAILURE.md",
    ROOT / "EXECUTION_NETWORK_BIBLE.md",
    ROOT / "Makefile",
    ROOT / "pricing",
    ROOT / "web",
)

# Census / summary artifacts are allowed to mention UNBOUND peers.
CITE_ALLOWLIST_PREFIXES = (
    "evidence/state/evidence-binding-census.json",
)

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


def iter_evidence_files() -> list[Path]:
    if not EVIDENCE.is_dir():
        return []
    out: list[Path] = []
    for path in sorted(EVIDENCE.rglob("*")):
        if not path.is_file():
            continue
        if path.name.endswith(".binding.json"):
            continue
        # Skip hidden scratch.
        if any(part.startswith(".") for part in path.relative_to(ROOT).parts):
            continue
        out.append(path)
    return out


def _load_sidecar(path: Path, rel: str) -> dict[str, Any] | None:
    side = binding_sidecar_path(path)
    if not side.is_file():
        return None
    try:
        binding = json.loads(side.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"{side.relative_to(ROOT)}: unreadable: {exc}")
        return None
    if not isinstance(binding, dict):
        fail(f"{side.relative_to(ROOT)}: not a JSON object")
        return None
    return binding


def load_binding_for(path: Path) -> tuple[dict[str, Any] | None, dict[str, Any] | None]:
    """Return (object_or_none, binding_doc).

    Receipts (measurement descriptions) carry binding fields in the object.
    Job-result payloads and non-object evidence carry binding in
    path + '.binding.json' so the payload schema stays closed.
    """
    rel = path.relative_to(ROOT).as_posix()
    if path.suffix == ".json":
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            fail(f"{rel}: unreadable JSON: {exc}")
            return None, None
        if isinstance(data, dict):
            if is_job_contract_payload(data):
                # Payload bodies must stay free of binding meta. A prior bad
                # stamp that left binding_status inside is a hard failure.
                present = sorted(BINDING_META_KEYS.intersection(data.keys()))
                if present:
                    fail(
                        f"{rel}: job-result payload carries in-object binding "
                        f"fields {present}; move them to {path.name}.binding.json"
                    )
                    return data, None
                binding = _load_sidecar(path, rel)
                if binding is None:
                    fail(
                        f"{rel}: job-result payload without sidecar "
                        f"{path.name}.binding.json"
                    )
                    return data, None
                return data, binding
            # Receipt: binding lives in the object.
            return data, data
        # JSON non-object (array etc.)
        binding = _load_sidecar(path, rel)
        if binding is None:
            fail(f"{rel}: non-object JSON without sidecar {path.name}.binding.json")
            return None, None
        return None, binding

    # jsonl / txt / other
    binding = _load_sidecar(path, rel)
    if binding is None:
        fail(
            f"{rel}: non-JSON-object evidence without sidecar "
            f"{path.name}.binding.json"
        )
        return None, None
    return None, binding


def check_bound_claim(rel: str, data: dict[str, Any]) -> None:
    """Rule (c): BOUND requires complete formal producer_identity."""
    pi = data.get("producer_identity")
    if not isinstance(pi, dict):
        fail(f"{rel}: binding_status=BOUND but producer_identity missing")
        return
    missing = incomplete_fields(pi)
    sc = pi.get("source_commit") or {}
    if isinstance(sc, dict) and str(sc.get("value") or "").strip():
        try:
            validate_git_object(ROOT, str(sc["value"]))
        except EvidenceBindingError:
            missing = list(missing)
            if "source_commit" not in missing:
                missing.append("source_commit")
    # build_digest is not re-derived in CI (binary may not match the writer);
    # presence of value or na is enough for the BOUND structural check.
    if missing:
        fail(
            f"{rel}: binding_status=BOUND but incomplete identity: "
            + ", ".join(missing)
        )


def collect_citations() -> dict[str, list[str]]:
    """Map evidence-relative path → list of citing files."""
    # Build set of known evidence paths (posix relative).
    known: set[str] = set()
    for path in iter_evidence_files():
        known.add(path.relative_to(ROOT).as_posix())

    cites: dict[str, list[str]] = {k: [] for k in known}
    # Match evidence/... tokens including optional #fragment
    pattern = re.compile(r"(evidence/[A-Za-z0-9_./@+-]+\.(?:json|jsonl|txt))")

    def scan_file(path: Path) -> None:
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return
        rel = path.relative_to(ROOT).as_posix() if path.is_relative_to(ROOT) else str(path)
        for match in pattern.finditer(text):
            target = match.group(1)
            # Strip trailing punctuation leftovers
            target = target.rstrip(").,;:\"'")
            if target in cites and rel not in cites[target]:
                # Evidence files citing peers don't count as claim citations
                if rel.startswith("evidence/"):
                    continue
                cites[target].append(rel)

    for root in CITE_ROOTS:
        if root.is_file():
            scan_file(root)
            continue
        if not root.is_dir():
            continue
        for path in root.rglob("*"):
            if not path.is_file():
                continue
            if path.suffix.lower() not in {
                ".go",
                ".py",
                ".sh",
                ".md",
                ".mjs",
                ".js",
                ".ts",
                ".json",
                ".yml",
                ".yaml",
                ".toml",
                ".txt",
                ".html",
                "",
            } and path.name not in {"Makefile"}:
                # still scan extensionless and Makefile
                if path.name != "Makefile" and path.suffix:
                    continue
            # Skip large/generated binaries
            if path.suffix in {".png", ".woff2", ".ico", ".pdf"}:
                continue
            scan_file(path)
    return cites


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--summary-only",
        action="store_true",
        help="print status counts and exit 0 (no pass/fail)",
    )
    args = ap.parse_args()

    counts = {
        BINDING_BOUND: 0,
        BINDING_UNBOUND: 0,
        BINDING_SUPERSEDED: 0,
        BINDING_WITHDRAWN: 0,
        "MISSING_STATUS": 0,
        "OTHER": 0,
    }
    unbound_cited: list[tuple[str, list[str]]] = []
    artifacts: list[tuple[str, str, list[str]]] = []

    cites = collect_citations() if not args.summary_only else {}

    for path in iter_evidence_files():
        rel = path.relative_to(ROOT).as_posix()
        _obj, binding = load_binding_for(path)
        if binding is None:
            counts["OTHER"] += 1
            continue

        status = str(binding.get("binding_status") or "").upper()
        if status not in VALID_STATUSES:
            counts["MISSING_STATUS"] += 1
            fail(f"{rel}: missing or invalid binding_status ({binding.get('binding_status')!r})")
            artifacts.append((rel, "MISSING_STATUS", []))
            continue

        counts[status] = counts.get(status, 0) + 1
        missing = list(binding.get("missing_identity_fields") or [])

        if status == BINDING_BOUND:
            # For object files, check the object; for sidecars, check sidecar.
            check_bound_claim(rel, binding)

        if status == BINDING_UNBOUND:
            if not missing:
                # Derive for reporting if stamp omitted the list.
                if _obj is not None:
                    missing = missing_fields_for_object(_obj, ROOT)
                fail(f"{rel}: binding_status=UNBOUND but missing_identity_fields empty")
            citers = cites.get(rel) or []
            # Allow census to mention UNBOUND peers.
            citers = [
                c
                for c in citers
                if not any(c.startswith(p) or c == p for p in CITE_ALLOWLIST_PREFIXES)
            ]
            if citers:
                unbound_cited.append((rel, citers))
                preview = ", ".join(citers[:5])
                more = f" (+{len(citers)-5} more)" if len(citers) > 5 else ""
                fail(
                    f"{rel}: UNBOUND yet cited by {preview}{more} "
                    f"(missing: {', '.join(missing) if missing else 'unknown'})"
                )

        artifacts.append((rel, status, missing))

    print("evidence-binding: counts")
    for key in (
        BINDING_BOUND,
        BINDING_UNBOUND,
        BINDING_SUPERSEDED,
        BINDING_WITHDRAWN,
        "MISSING_STATUS",
        "OTHER",
    ):
        print(f"  {key}: {counts.get(key, 0)}")
    print(f"  total_files: {sum(counts.values())}")

    if unbound_cited:
        print(f"evidence-binding: UNBOUND citations: {len(unbound_cited)}")

    if args.summary_only:
        return 0

    if FAILURES:
        print(f"evidence-binding: FAIL ({len(FAILURES)} findings)", file=sys.stderr)
        for msg in FAILURES:
            print(f"  - {msg}", file=sys.stderr)
        return 1

    print("evidence-binding: PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
