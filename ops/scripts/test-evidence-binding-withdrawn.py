#!/usr/bin/env python3
"""Focused adversarial checks for terminal evidence-binding citation policy."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import sys
import tempfile
from pathlib import Path


SCRIPT = Path(__file__).resolve().with_name("validate-evidence-binding.py")
SPEC = importlib.util.spec_from_file_location("validate_evidence_binding", SCRIPT)
if SPEC is None or SPEC.loader is None:
    raise SystemExit(f"cannot load {SCRIPT}")
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def run_fixture(
    *,
    binding_status: str,
    validity: str = "",
    citation: str = "",
    relative_path: str = "evidence/perf/cohort.json",
) -> tuple[int, str, list[str]]:
    with tempfile.TemporaryDirectory(prefix="merc-evidence-binding-") as tmp:
        root = Path(tmp)
        artifact = root / relative_path
        artifact.parent.mkdir(parents=True)
        payload = {"binding_status": binding_status}
        if validity:
            payload["validity"] = validity
        if binding_status == VALIDATOR.BINDING_UNBOUND:
            payload["missing_identity_fields"] = ["producer_identity"]
        artifact.write_text(json.dumps(payload), encoding="utf-8")

        docs = root / "docs"
        docs.mkdir()
        if citation:
            (docs / "claim.md").write_text(citation + "\n", encoding="utf-8")

        VALIDATOR.ROOT = root
        VALIDATOR.EVIDENCE = root / "evidence"
        VALIDATOR.CITE_ROOTS = (docs,)
        VALIDATOR.FAILURES.clear()
        old_argv = sys.argv
        sys.argv = [str(SCRIPT)]
        output = io.StringIO()
        try:
            with contextlib.redirect_stdout(output), contextlib.redirect_stderr(output):
                rc = VALIDATOR.main()
        finally:
            sys.argv = old_argv
        return rc, output.getvalue(), list(VALIDATOR.FAILURES)


def main() -> int:
    target = "evidence/perf/cohort.json"

    rc, output, failures = run_fixture(
        binding_status=VALIDATOR.BINDING_WITHDRAWN,
        validity="WITHDRAWN",
        citation=f"Production is faster, as proved by {target}.",
    )
    check(rc == 1, "undisclaimed WITHDRAWN citation must fail")
    check(
        any("WITHDRAWN yet cited as authority" in finding for finding in failures),
        f"WITHDRAWN citation failure missing: {output}",
    )

    rc, output, _ = run_fixture(
        binding_status=VALIDATOR.BINDING_WITHDRAWN,
        validity="WITHDRAWN",
        citation=f"{target} is WITHDRAWN and must not support a claim.",
    )
    check(rc == 0, f"explicit WITHDRAWN disclaimer should be citable: {output}")

    rc, output, failures = run_fixture(
        binding_status=VALIDATOR.BINDING_UNBOUND,
        citation=f"Production is faster, as proved by {target}.",
    )
    check(rc == 1, "undisclaimed UNBOUND citation must fail")
    check(
        any("UNBOUND yet cited as authority" in finding for finding in failures),
        f"UNBOUND citation failure missing: {output}",
    )

    rc, output, _ = run_fixture(
        binding_status=VALIDATOR.BINDING_UNBOUND,
        citation=f"{target} is UNBOUND and is cited only as a historical diagnostic.",
    )
    check(rc == 0, f"explicit UNBOUND disclaimer should be citable: {output}")

    rc, output, failures = run_fixture(
        binding_status=VALIDATOR.BINDING_UNBOUND,
        validity="INVALIDATED_PENDING_RERUN",
    )
    check(rc == 1, "withdrawn validity under a stale UNBOUND label must fail")
    check(
        any("expected WITHDRAWN" in finding for finding in failures),
        f"terminal validity mismatch failure missing: {output}",
    )

    rc, output, failures = run_fixture(
        binding_status=VALIDATOR.BINDING_BOUND,
        validity="VALID",
        relative_path="evidence/perf/selector/paired-cohort-embed.json",
    )
    check(rc == 1, "withdrawn cohort authority id must not be rebound in place")
    check(
        any("terminally withdrawn" in finding for finding in failures),
        f"sticky cohort withdrawal failure missing: {output}",
    )

    print("evidence-binding-withdrawn: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
