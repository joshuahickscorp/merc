#!/usr/bin/env python3
"""CI gate: unbound evidence must not exist unlabelled, and must not back claims.

Fails when any artifact under evidence/** is:
  (a) missing binding_status
  (b) UNBOUND or WITHDRAWN yet cited as authority by a doc or by code
  (c) claims BOUND while missing an applicable identity field
  (d) claims BOUND while depending on evidence that is not itself BOUND

Also fails on non-object evidence without a sidecar binding file
(path + ".binding.json").

    python3 scripts/validate-evidence-binding.py
    python3 scripts/validate-evidence-binding.py --summary-only

Exit 0 only when the tree is clean. Today's historical tree is expected to fail
loudly while UNBOUND artifacts remain cited.
"""

from __future__ import annotations

import argparse
import hashlib
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
    lfs_pointer_oid,
    missing_fields_for_object,
    path_holds_withdrawn,
    read_evidence_bytes,
    read_evidence_text,
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
    ROOT / "evidence" / "proof",
    ROOT / "README.md",
    ROOT / "RELEASE_READINESS.md",
    ROOT / "docs" / "PROGRAMME.md",
    ROOT / "docs" / "RUNTIME_AND_PERF.md",
    ROOT / "docs" / "ARCHITECTURE.md",
    ROOT / "Makefile",
    ROOT / "pricing",
    ROOT / "web",
)

# Census / summary artifacts are allowed to mention UNBOUND peers.
CITE_ALLOWLIST_PREFIXES = (
    "evidence/state/evidence-binding-census.json",
)

# These authority ids have been explicitly withdrawn. Their paths are sticky:
# a replacement must use a new id/path, not relabel the historical payload.
TERMINALLY_WITHDRAWN_PATHS = {
    "evidence/perf/selector/paired-cohort-embed.json",
}

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


# Trees relocated under evidence/ that are not measurement receipts.
# Formerly lived at repo root (proof/, census/, artifacts/) and were outside
# this scanner; they remain policy/census/fixture trees, not binding corpus.
EVIDENCE_SCAN_SKIP_PREFIXES = (
    "evidence/proof/",
    "evidence/census/",
    "evidence/artifacts/",
    "evidence/immutable-fixtures/",
    "evidence/workload-catalog/",
)

# Narrative companions sitting beside receipts. They are not measurement
# artifacts and do not carry binding_status; the receipt they describe does.
EVIDENCE_SCAN_SKIP_SUFFIXES = (".md",)


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
        rel = path.relative_to(ROOT).as_posix()
        if any(part.startswith(".") for part in path.relative_to(ROOT).parts):
            continue
        if any(rel.startswith(prefix) for prefix in EVIDENCE_SCAN_SKIP_PREFIXES):
            continue
        if path.suffix.lower() in EVIDENCE_SCAN_SKIP_SUFFIXES:
            continue
        out.append(path)
    return out


def _load_sidecar(path: Path, rel: str) -> dict[str, Any] | None:
    side = binding_sidecar_path(path)
    if not side.is_file():
        return None
    try:
        binding = json.loads(read_evidence_text(side, ROOT))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError, EvidenceBindingError) as exc:
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
            # Resolve git-lfs pointers to content (or fail with oid context).
            # When only the pointer is available and content cannot be fetched,
            # lfs_pointer_oid still identifies the payload by sha256.
            data = json.loads(read_evidence_text(path, ROOT))
        except (OSError, UnicodeDecodeError, json.JSONDecodeError, EvidenceBindingError) as exc:
            oid = lfs_pointer_oid(path)
            if oid:
                fail(
                    f"{rel}: unreadable JSON after LFS resolve "
                    f"(oid sha256:{oid}): {exc}"
                )
            else:
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


def evidence_dependencies(rel: str, data: dict[str, Any]) -> list[tuple[str, str]]:
    """Return explicit authority dependencies after validating their paths.

    Evidence files may mention peers as historical context or as refusals.  The
    explicit list is reserved for artifacts whose claims actually depend on a
    peer, so weakest-link binding can be enforced without treating every JSON
    path in prose as an authority edge.
    """
    raw = data.get("evidence_dependencies")
    if raw is None:
        return []
    if not isinstance(raw, list):
        fail(f"{rel}: evidence_dependencies must be an array of evidence paths")
        return []

    out: list[tuple[str, str]] = []
    for index, value in enumerate(raw):
        if not isinstance(value, dict):
            fail(
                f"{rel}: evidence_dependencies[{index}] must bind path and sha256"
            )
            continue
        dependency = str(value.get("path") or "").strip()
        expected_sha256 = str(value.get("sha256") or "").strip().lower()
        parts = Path(dependency).parts
        if (
            not dependency.startswith("evidence/")
            or dependency.startswith("evidence//")
            or not dependency.endswith((".json", ".jsonl", ".txt"))
            or ".." in parts
            or dependency == rel
        ):
            fail(
                f"{rel}: evidence_dependencies[{index}] is not a distinct "
                f"repo-relative evidence path ({value!r})"
            )
            continue
        if len(expected_sha256) != 64 or any(
            char not in "0123456789abcdef" for char in expected_sha256
        ):
            fail(
                f"{rel}: evidence_dependencies[{index}] has invalid sha256 "
                f"({value.get('sha256')!r})"
            )
            continue
        if any(path == dependency for path, _digest in out):
            fail(f"{rel}: duplicate evidence dependency {dependency}")
            continue
        out.append((dependency, expected_sha256))
    return out


def effective_binding_status(rel: str, binding: dict[str, Any]) -> str:
    """Normalize terminal withdrawal without emitting duplicate findings."""
    status = str(binding.get("binding_status") or "").upper()
    if rel in TERMINALLY_WITHDRAWN_PATHS or path_holds_withdrawn(binding):
        return BINDING_WITHDRAWN
    return status


def check_bound_dependencies(
    rel: str,
    data: dict[str, Any],
    binding_index: dict[str, tuple[dict[str, Any] | None, dict[str, Any] | None]],
) -> None:
    """Rule (d): every declared input to a BOUND claim is also BOUND."""
    for dependency, expected_sha256 in evidence_dependencies(rel, data):
        record = binding_index.get(dependency)
        if record is None:
            fail(f"{rel}: BOUND evidence dependency does not exist: {dependency}")
            continue
        _dependency_object, dependency_binding = record
        if dependency_binding is None:
            fail(f"{rel}: BOUND evidence dependency has no binding: {dependency}")
            continue
        dependency_status = effective_binding_status(dependency, dependency_binding)
        if dependency_status != BINDING_BOUND:
            fail(
                f"{rel}: binding_status=BOUND depends on {dependency} "
                f"with binding_status={dependency_status or 'MISSING'}"
            )
            continue
        try:
            actual_sha256 = hashlib.sha256(
                read_evidence_bytes(ROOT / dependency, ROOT)
            ).hexdigest()
        except (OSError, EvidenceBindingError) as exc:
            fail(f"{rel}: cannot hash BOUND evidence dependency {dependency}: {exc}")
            continue
        if actual_sha256 != expected_sha256:
            fail(
                f"{rel}: BOUND evidence dependency digest mismatch for "
                f"{dependency}: got {actual_sha256}, want {expected_sha256}"
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

    def is_claim_surface(rel: str) -> bool:
        """True when a reference here asserts something a reader would believe.

        The gate exists so no CLAIM rests on unbound evidence. That is not the
        same as no code touching an evidence path. A writer naming the file it
        produces, a scorer naming the receipt it reads, and a sealing script
        naming its output are all machinery -- upstream or downstream of claims,
        not claims themselves. Failing them means the gate reports the writer for
        writing, which is noise that buries the real finding.

        Documentation is different: a doc citing an unbound artifact is exactly a
        reader being told a number is backed when it is not.
        """
        if rel.endswith(".md"):
            return True
        # Go source may cite a receipt as pricing authority; that IS a claim, and
        # pricing_citation_authority.go additionally resolves and verifies it.
        return rel.endswith(".go") and not rel.endswith("_test.go")

    def is_build_artifact(path: Path, rel: str) -> bool:
        """Compiled output is never a citation.

        control/control and agent/target/release/merc-agent embed evidence path
        strings from the source they were built from. Treating those as citations
        made a stale binary on someone's disk fail the build, and counted 17
        phantom citations that no reader could ever see.
        """
        if "/target/" in rel or rel.endswith(("/control", "/merc-agent")):
            return True
        try:
            with open(path, "rb") as fh:
                return b"\x00" in fh.read(8192)
        except OSError:
            return True

    def scan_file(path: Path) -> None:
        rel_probe = path.relative_to(ROOT).as_posix() if path.is_relative_to(ROOT) else str(path)
        if is_build_artifact(path, rel_probe):
            return
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            return
        rel = path.relative_to(ROOT).as_posix() if path.is_relative_to(ROOT) else str(path)
        if not is_claim_surface(rel):
            return
        lines = text.splitlines()
        for idx, line in enumerate(lines):
            for match in pattern.finditer(line):
                target = match.group(1)
                # Strip trailing punctuation leftovers
                target = target.rstrip(").,;:\"'")
                if target not in cites or rel in cites[target]:
                    continue
                # Evidence files citing peers don't count as claim citations
                if rel.startswith("evidence/"):
                    continue
                # A citation that DISCLAIMS the artifact is the honest use, not a
                # violation. "these figures are unbound and must not be quoted"
                # is precisely what this programme wants a doc to say; failing it
                # would pressure someone to delete the warning and leave the
                # number, which is the trust regression the gate exists to stop.
                window = " ".join(lines[max(0, idx - 2):idx + 3]).lower()
                if any(
                    marker in window
                    for marker in (
                        "unbound", "not bound", "must not be quoted", "do not quote",
                        "no bound receipt", "superseded", "withdrawn", "invalidated",
                        "does not prove", "not evidence", "historical",
                    )
                ):
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
    withdrawn_cited: list[tuple[str, list[str]]] = []
    artifacts: list[tuple[str, str, list[str]]] = []

    cites = collect_citations() if not args.summary_only else {}
    records: list[
        tuple[Path, str, dict[str, Any] | None, dict[str, Any] | None]
    ] = []
    binding_index: dict[
        str, tuple[dict[str, Any] | None, dict[str, Any] | None]
    ] = {}
    for path in iter_evidence_files():
        rel = path.relative_to(ROOT).as_posix()
        _obj, binding = load_binding_for(path)
        records.append((path, rel, _obj, binding))
        binding_index[rel] = (_obj, binding)

    for path, rel, _obj, binding in records:
        if binding is None:
            counts["OTHER"] += 1
            continue

        status = str(binding.get("binding_status") or "").upper()
        if status not in VALID_STATUSES:
            counts["MISSING_STATUS"] += 1
            fail(f"{rel}: missing or invalid binding_status ({binding.get('binding_status')!r})")
            artifacts.append((rel, "MISSING_STATUS", []))
            continue

        # An explicitly withdrawn authority id stays withdrawn even if someone
        # edits both current labels. Other withdrawn/invalidated validity markers
        # are terminal even when a stale binding_status says UNBOUND or BOUND.
        if rel in TERMINALLY_WITHDRAWN_PATHS and status != BINDING_WITHDRAWN:
            fail(
                f"{rel}: authority id is terminally withdrawn but "
                f"binding_status={status}; replacement requires a new path"
            )
            status = BINDING_WITHDRAWN
        elif path_holds_withdrawn(binding) and status != BINDING_WITHDRAWN:
            fail(
                f"{rel}: validity marks receipt withdrawn/invalidated but "
                f"binding_status={status}; expected {BINDING_WITHDRAWN}"
            )
            status = BINDING_WITHDRAWN

        counts[status] = counts.get(status, 0) + 1
        missing = list(binding.get("missing_identity_fields") or [])

        if status == BINDING_BOUND:
            # For object files, check the object; for sidecars, check sidecar.
            check_bound_claim(rel, binding)
            dependency_doc = (
                _obj
                if isinstance(_obj, dict) and "evidence_dependencies" in _obj
                else binding
            )
            check_bound_dependencies(rel, dependency_doc, binding_index)

        if status == BINDING_UNBOUND:
            if not missing:
                # Derive for reporting if stamp omitted the list.
                if _obj is not None:
                    missing = missing_fields_for_object(_obj, ROOT)
                fail(f"{rel}: binding_status=UNBOUND but missing_identity_fields empty")

        if status in (BINDING_UNBOUND, BINDING_WITHDRAWN):
            citers = cites.get(rel) or []
            # Allow the binding census to enumerate non-authoritative peers.
            citers = [
                c
                for c in citers
                if not any(c.startswith(p) or c == p for p in CITE_ALLOWLIST_PREFIXES)
            ]
            if citers:
                if status == BINDING_UNBOUND:
                    unbound_cited.append((rel, citers))
                else:
                    withdrawn_cited.append((rel, citers))
                preview = ", ".join(citers[:5])
                more = f" (+{len(citers)-5} more)" if len(citers) > 5 else ""
                fail(
                    f"{rel}: {status} yet cited as authority by {preview}{more} "
                    + (
                        f"(missing: {', '.join(missing) if missing else 'unknown'})"
                        if status == BINDING_UNBOUND
                        else "(withdrawn evidence is terminal and may only be cited with an explicit disclaimer)"
                    )
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
    if withdrawn_cited:
        print(f"evidence-binding: WITHDRAWN citations: {len(withdrawn_cited)}")

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
