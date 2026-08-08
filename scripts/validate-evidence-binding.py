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
import hashlib
import json
import re
import subprocess
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
    indexed_lfs_pointer_identity,
    is_job_contract_payload,
    lfs_pointer_oid,
    missing_fields_for_object,
    parse_lfs_pointer,
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

# A small number of historical receipts are immutable Git-LFS payloads that
# predate binding_status.  They cannot be repaired by editing the body: doing
# so would rewrite the historical measurement.  Their sibling envelopes are
# deliberately narrower than ordinary payload sidecars:
#
#   * they may apply only to a JSON receipt with no in-body binding_status;
#   * they are UNBOUND, never an upgrade to BOUND/SUPERSEDED/WITHDRAWN;
#   * they pin the logical path, indexed pointer oid+size, and hydrated bytes.
#
# Thus an envelope preserves an honest historical classification without being
# a loose sidecar that can bless a repointed or edited payload.
HISTORICAL_LFS_ENVELOPE_KIND = "historical_lfs_binding_envelope"
HISTORICAL_LFS_ENVELOPE_KEYS = frozenset(
    {
        "schema_version",
        "kind",
        "append_only",
        "historical_source_commit",
        "target_path",
        "indexed_lfs_oid_sha256",
        "indexed_lfs_size_bytes",
        "hydrated_body_sha256",
        "binding_status",
        "missing_identity_fields",
        "reason",
    }
)

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


def _historical_source_pointer(
    source_commit: Any, target_path: str
) -> tuple[str, int] | None:
    """Read the immutable historical pointer from a full ancestor commit.

    `append_only` is not a Git primitive.  Anchoring an envelope to a committed
    predecessor makes a coordinated working-tree pointer+sidecar rewrite fail:
    both the current index and the named historical tree must still name the
    identical OID and byte size.
    """
    if not isinstance(source_commit, str) or not re.fullmatch(r"[0-9a-f]{40}", source_commit):
        fail("historical_source_commit must be a full lowercase 40-hex commit")
        return None
    resolved = subprocess.run(
        ["git", "-C", str(ROOT), "rev-parse", "--verify", f"{source_commit}^{{commit}}"],
        capture_output=True,
        text=True,
        check=False,
    )
    if resolved.returncode != 0 or resolved.stdout.strip() != source_commit:
        fail("historical_source_commit is not an exact local commit")
        return None
    ancestry = subprocess.run(
        ["git", "-C", str(ROOT), "merge-base", "--is-ancestor", source_commit, "HEAD"],
        capture_output=True,
        text=True,
        check=False,
    )
    if ancestry.returncode != 0:
        fail("historical_source_commit is not an ancestor of current HEAD")
        return None
    shown = subprocess.run(
        ["git", "-C", str(ROOT), "show", f"{source_commit}:{target_path}"],
        capture_output=True,
        check=False,
    )
    if shown.returncode != 0:
        fail("historical_source_commit does not contain the target pointer")
        return None
    try:
        text = shown.stdout.decode("utf-8")
    except UnicodeDecodeError:
        fail("historical_source_commit target is not an LFS pointer")
        return None
    pointer = parse_lfs_pointer(text)
    if pointer is None:
        fail("historical_source_commit target is not an LFS pointer")
    return pointer


def _historical_lfs_envelope_binding(
    path: Path, rel: str, body: dict[str, Any]
) -> dict[str, Any] | None:
    """Load one immutable-payload UNBOUND envelope, or refuse it precisely.

    This is intentionally not a generic fallback.  The caller invokes it only
    after establishing that the receipt body has *no* binding_status.  An
    in-body status, including an invalid one, stays authoritative and will be
    handled by the ordinary status checks below.
    """
    side = binding_sidecar_path(path)
    if not side.is_file():
        return None
    binding = _load_sidecar(path, rel)
    if binding is None:
        return None

    errors: list[str] = []
    if set(binding) != HISTORICAL_LFS_ENVELOPE_KEYS:
        unknown = sorted(set(binding) - HISTORICAL_LFS_ENVELOPE_KEYS)
        missing = sorted(HISTORICAL_LFS_ENVELOPE_KEYS - set(binding))
        detail: list[str] = []
        if unknown:
            detail.append("unknown keys=" + ",".join(unknown))
        if missing:
            detail.append("missing keys=" + ",".join(missing))
        errors.append("strict envelope schema required (" + "; ".join(detail) + ")")

    if binding.get("schema_version") != 1:
        errors.append("schema_version must be 1")
    if binding.get("kind") != HISTORICAL_LFS_ENVELOPE_KIND:
        errors.append(f"kind must be {HISTORICAL_LFS_ENVELOPE_KIND!r}")
    if binding.get("append_only") is not True:
        errors.append("append_only must be true")
    if binding.get("target_path") != rel:
        errors.append(f"target_path must equal {rel!r}")
    if str(binding.get("binding_status") or "").upper() != BINDING_UNBOUND:
        errors.append("historical LFS envelope may only declare binding_status=UNBOUND")
    if not isinstance(binding.get("reason"), str) or not binding["reason"].strip():
        errors.append("reason must be a non-empty string")

    historical_pointer = _historical_source_pointer(
        binding.get("historical_source_commit"), rel
    )
    if historical_pointer is not None:
        source_oid, source_size = historical_pointer
        if binding.get("indexed_lfs_oid_sha256") != source_oid:
            errors.append(
                "indexed_lfs_oid_sha256 does not match the historical source pointer"
            )
        if binding.get("indexed_lfs_size_bytes") != source_size:
            errors.append(
                "indexed_lfs_size_bytes does not match the historical source pointer"
            )

    indexed = indexed_lfs_pointer_identity(ROOT, path)
    if indexed is None:
        errors.append("target is not an indexed Git-LFS pointer")
    else:
        indexed_oid, indexed_size = indexed
        if binding.get("indexed_lfs_oid_sha256") != indexed_oid:
            errors.append("indexed_lfs_oid_sha256 does not match the indexed pointer")
        if (
            isinstance(binding.get("indexed_lfs_size_bytes"), bool)
            or binding.get("indexed_lfs_size_bytes") != indexed_size
        ):
            errors.append("indexed_lfs_size_bytes does not match the indexed pointer")

        try:
            hydrated = read_evidence_bytes(path, ROOT)
        except (OSError, EvidenceBindingError) as exc:
            errors.append(f"cannot verify hydrated body: {exc}")
        else:
            hydrated_sha = hashlib.sha256(hydrated).hexdigest()
            if len(hydrated) != indexed_size:
                errors.append(
                    "hydrated body size does not match the indexed LFS pointer"
                )
            if hydrated_sha != indexed_oid:
                errors.append(
                    "hydrated body sha256 does not match the indexed LFS pointer"
                )
            if binding.get("hydrated_body_sha256") != hydrated_sha:
                errors.append("hydrated_body_sha256 does not match the body")

    expected_missing = missing_fields_for_object(body, ROOT)
    if binding.get("missing_identity_fields") != expected_missing:
        errors.append(
            "missing_identity_fields must exactly name the fields absent from "
            "the immutable body"
        )

    if errors:
        fail(
            f"{rel}: historical LFS envelope refused: " + "; ".join(errors)
        )
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
            # An in-body status always wins.  In particular, an envelope cannot
            # downgrade or upgrade a body that now says BOUND, SUPERSEDED,
            # WITHDRAWN, or even an invalid status; status handling below will
            # fail an invalid claim rather than silently accepting the sidecar.
            if "binding_status" in data:
                return data, data

            # A historical LFS receipt with no in-body status may carry the
            # narrow immutable-payload envelope above.  Non-LFS receipts retain
            # the normal in-body-only rule, so this is never a broad sidecar
            # escape hatch for arbitrary JSON.
            envelope = _historical_lfs_envelope_binding(path, rel, data)
            if envelope is not None:
                return data, envelope

            # Receipt: binding normally lives in the object.
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
