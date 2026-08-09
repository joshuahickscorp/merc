#!/usr/bin/env python3
"""Focused adversarial checks for weakest-link evidence binding."""

from __future__ import annotations

import contextlib
import hashlib
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

IDENTITY_FIELDS = (
    "source_commit",
    "build_digest",
    "model_artifact_digest",
    "image_digest",
    "harness_revision",
    "corpus_digest",
    "exact_config",
    "raw_samples",
)


def bound_identity() -> dict[str, dict[str, str]]:
    return {field: {"na": "adversarial fixture"} for field in IDENTITY_FIELDS}


def check(condition: bool, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def run_fixture(
    *,
    dependency_status: str,
    dependency_path: str,
    dependency_digest_override: str = "",
) -> tuple[int, str, list[str]]:
    with tempfile.TemporaryDirectory(prefix="merc-evidence-dependency-") as tmp:
        root = Path(tmp)
        evidence = root / "evidence" / "perf"
        evidence.mkdir(parents=True)

        dependency_sha256 = "a" * 64
        if dependency_path == "evidence/perf/input.json":
            dependency: dict[str, object] = {"binding_status": dependency_status}
            if dependency_status == VALIDATOR.BINDING_BOUND:
                dependency["producer_identity"] = bound_identity()
            else:
                dependency["missing_identity_fields"] = ["raw_samples"]
            dependency_bytes = json.dumps(dependency).encode()
            (evidence / "input.json").write_bytes(dependency_bytes)
            dependency_sha256 = hashlib.sha256(dependency_bytes).hexdigest()
        if dependency_digest_override:
            dependency_sha256 = dependency_digest_override

        derived = {
            "kind": "derived_measurement_verdict",
            "binding_status": VALIDATOR.BINDING_BOUND,
            "producer_identity": bound_identity(),
            "evidence_dependencies": [{
                "path": dependency_path,
                "sha256": dependency_sha256,
            }],
        }
        (evidence / "derived.json").write_text(json.dumps(derived), encoding="utf-8")

        docs = root / "docs"
        docs.mkdir()
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
    rc, output, failures = run_fixture(
        dependency_status=VALIDATOR.BINDING_UNBOUND,
        dependency_path="evidence/perf/input.json",
    )
    check(rc == 1, "BOUND derived evidence must reject an UNBOUND input")
    check(
        any("binding_status=BOUND depends on" in finding for finding in failures),
        f"weakest-link finding missing: {output}",
    )

    rc, output, failures = run_fixture(
        dependency_status=VALIDATOR.BINDING_BOUND,
        dependency_path="evidence/perf/input.json",
    )
    check(rc == 0, f"BOUND dependency chain should pass: {output}; {failures}")

    rc, output, failures = run_fixture(
        dependency_status=VALIDATOR.BINDING_BOUND,
        dependency_path="evidence/perf/input.json",
        dependency_digest_override="b" * 64,
    )
    check(rc == 1, "changed dependency bytes must fail their frozen digest")
    check(
        any("dependency digest mismatch" in finding for finding in failures),
        f"dependency-digest finding absent: {output}",
    )

    rc, output, failures = run_fixture(
        dependency_status=VALIDATOR.BINDING_BOUND,
        dependency_path="evidence/perf/missing.json",
    )
    check(rc == 1, "missing dependency must fail closed")
    check(
        any("dependency does not exist" in finding for finding in failures),
        f"missing-dependency finding absent: {output}",
    )

    rc, output, failures = run_fixture(
        dependency_status=VALIDATOR.BINDING_BOUND,
        dependency_path="../outside.json",
    )
    check(rc == 1, "non-repo dependency path must fail closed")
    check(
        any("not a distinct repo-relative evidence path" in finding for finding in failures),
        f"dependency-path finding absent: {output}",
    )

    print("evidence-binding-dependencies: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
