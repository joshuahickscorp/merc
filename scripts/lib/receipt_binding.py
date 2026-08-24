"""One way to say which commit a receipt was produced at.

Readiness receipts were being written by a dozen producers that each invented
their own provenance shape, or none. `scripts/validate-readiness.py` then scored
them without ever asking when they were made, so an artifact produced months and
many commits ago counted as present-tense readiness. This module is the single
answer to "which commit does this receipt attest to", used by both the producers
that stamp it and the checker that demands it.

A receipt is BOUND when it names the commit it was produced at and that commit
is the candidate under evaluation. Anything else is history.
"""

from __future__ import annotations

import subprocess
from typing import Any

# Producers already in the tree use several names for the same fact. Read all of
# them; write only the first.
COMMIT_FIELDS = ("source_commit", "expected_commit", "candidate_commit", "commit")
# Some receipts nest provenance instead of putting it at the top level.
NESTS = ("binding", "producer_identity", "provenance", "identity")

_HEX40 = set("0123456789abcdef")


def _unwrap(value: Any) -> Any:
    """Some producers write {"value": sha, "source": ...} instead of a bare sha."""
    if isinstance(value, dict) and "value" in value:
        return value["value"]
    return value


def _looks_like_commit(value: Any) -> bool:
    text = str(_unwrap(value) or "").strip().lower()
    return len(text) == 40 and set(text) <= _HEX40


def _as_commit(value: Any) -> str:
    return str(_unwrap(value)).strip().lower()


def head_commit(root: str = ".") -> str:
    """The commit under evaluation. Raises if this is not a git tree."""
    out = subprocess.run(
        ["git", "-C", root, "rev-parse", "HEAD"],
        capture_output=True,
        text=True,
        check=True,
    )
    return out.stdout.strip()


def candidate_commit(root: str = ".") -> str:
    """The commit receipts must attest to: the declared candidate, else HEAD.

    Producers stamp this rather than HEAD. Binding against a moving HEAD never
    settles — committing a regenerated receipt moves HEAD and invalidates the
    receipt just written, so a full regeneration pass could never converge. The
    candidate is declared once in ops/candidate.json and every producer in the
    pass stamps that same commit.

    This is only honest because scripts/validate-readiness.py separately asserts
    no CODE changed after the candidate. Without that assertion this function
    would let a receipt claim a commit whose code it never ran.
    """
    import json
    from pathlib import Path

    path = Path(root) / "ops" / "candidate.json"
    try:
        declared = json.loads(path.read_text(encoding="utf-8")).get("commit")
    except (OSError, ValueError, AttributeError):
        return head_commit(root)
    if not _looks_like_commit(declared):
        return head_commit(root)
    return _as_commit(declared)


def receipt_commit(doc: Any) -> str | None:
    """The commit a receipt claims, or None when it claims none.

    Only a full 40-hex sha counts. A short sha, a branch name, or an empty
    string is not a binding — it is a receipt that declines to say.
    """
    if not isinstance(doc, dict):
        return None
    for field in COMMIT_FIELDS:
        if _looks_like_commit(doc.get(field)):
            return _as_commit(doc[field])
    for nest in NESTS:
        inner = doc.get(nest)
        if isinstance(inner, dict):
            for field in COMMIT_FIELDS:
                if _looks_like_commit(inner.get(field)):
                    return _as_commit(inner[field])
    return None


def bound_to(doc: Any, candidate: str) -> bool:
    """True only when the receipt names exactly the candidate commit."""
    if not _looks_like_commit(candidate):
        return False
    return receipt_commit(doc) == str(candidate).strip().lower()


def stamp(doc: dict, commit: str, producer: str) -> dict:
    """Record on a receipt the commit and producer that made it.

    Producers call this as the last step before writing. It refuses to stamp a
    commit that is not a real sha rather than writing a placeholder that would
    later read as provenance.
    """
    if not _looks_like_commit(commit):
        raise ValueError(f"refusing to stamp non-commit provenance: {commit!r}")
    if not str(producer or "").strip():
        raise ValueError("refusing to stamp a receipt with no named producer")
    doc["source_commit"] = str(commit).strip().lower()
    doc["produced_by"] = str(producer).strip()
    doc["binding_status"] = "BOUND"
    return doc


def _selftest() -> None:
    real = "a" * 40
    other = "b" * 40

    assert receipt_commit(None) is None
    assert receipt_commit({}) is None
    assert receipt_commit({"source_commit": ""}) is None
    assert receipt_commit({"source_commit": "7c05e7f0"}) is None, "short sha is not a binding"
    assert receipt_commit({"source_commit": real}) == real
    assert receipt_commit({"expected_commit": real.upper()}) == real
    assert receipt_commit({"binding": {"commit": real}}) == real
    assert receipt_commit({"producer_identity": {"candidate_commit": real}}) == real
    # alert-delivery-r1 wraps the sha: producer_identity.source_commit.value
    assert receipt_commit({"producer_identity": {"source_commit": {"value": real}}}) == real
    assert receipt_commit({"producer_identity": {"source_commit": {"value": ""}}}) is None

    assert bound_to({"source_commit": real}, real)
    assert not bound_to({"source_commit": other}, real)
    assert not bound_to({}, real)
    assert not bound_to({"source_commit": real}, "not-a-sha")

    import os, tempfile, json as _json
    with tempfile.TemporaryDirectory() as d:
        os.makedirs(os.path.join(d, "ops"))
        # No candidate declared and not a git tree -> head_commit raises, so the
        # only thing asserted here is that a declared candidate is preferred.
        with open(os.path.join(d, "ops", "candidate.json"), "w") as fh:
            _json.dump({"schema_version": 1, "commit": other}, fh)
        assert candidate_commit(d) == other, "declared candidate must win over HEAD"

    doc = stamp({"kind": "x"}, real, "scripts/thing.py")
    assert doc["source_commit"] == real
    assert doc["binding_status"] == "BOUND"
    assert bound_to(doc, real)

    for bad in ("", "HEAD", "7c05e7f0", None):
        try:
            stamp({}, bad, "p")
        except ValueError:
            pass
        else:  # pragma: no cover
            raise AssertionError(f"stamp accepted non-commit {bad!r}")
    try:
        stamp({}, real, "")
    except ValueError:
        pass
    else:  # pragma: no cover
        raise AssertionError("stamp accepted an unnamed producer")

    print("receipt_binding: self-test PASS")


if __name__ == "__main__":
    _selftest()
