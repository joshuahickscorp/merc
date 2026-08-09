#!/usr/bin/env python3
"""Hostile fixtures for the mutation-contract observer."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
OBSERVER = ROOT / "scripts" / "mutation-contract-observer.py"


def event(action: str, test: str | None = None, output: str | None = None) -> str:
    value: dict[str, str] = {"Action": action}
    if test is not None:
        value["Test"] = test
    if output is not None:
        value["Output"] = output
    return json.dumps(value)


def check(
    lines: list[str],
    exit_code: int,
    want_code: int,
    want_prefix: str,
    *completion: str,
) -> None:
    with tempfile.TemporaryDirectory() as temporary:
        log = Path(temporary) / "go.json"
        log.write_text("\n".join(lines) + "\n", encoding="utf-8")
        result = subprocess.run(
            [sys.executable, str(OBSERVER), "--log", str(log), "--exit-code", str(exit_code),
             "--expected", "TestInvariant", "--expected", "TestOther", *completion],
            text=True,
            capture_output=True,
            check=False,
        )
    if result.returncode != want_code or not result.stdout.startswith(want_prefix):
        raise SystemExit(
            f"observer mismatch: code={result.returncode} output={result.stdout!r}; "
            f"want {want_code}/{want_prefix!r}"
        )


def main() -> int:
    check([event("run", "TestInvariant"), event("pass", "TestInvariant")], 0, 0, "pass:")
    check([event("run", "TestInvariant"), event("skip", "TestInvariant")], 0, 0, "skipped:")
    check(
        [event("run", "TestInvariant"), event("pass", "TestInvariant")],
        0,
        2,
        "infrastructure: declared_test_not_run:TestOther",
        "--require-all-run",
    )
    check(
        [
            event("run", "TestInvariant"),
            event("pass", "TestInvariant"),
            event("run", "TestOther"),
            event("skip", "TestOther"),
        ],
        0,
        0,
        "pass:",
        "--require-all-run",
    )
    check(
        [
            event("run", "TestInvariant"),
            event("pass", "TestInvariant"),
            event("run", "TestOther"),
            event("skip", "TestOther"),
        ],
        0,
        2,
        "infrastructure: declared_test_not_passed:TestOther",
        "--require-all-pass",
    )
    check([event("run", "TestInvariant"), event("fail", "TestInvariant")], 1, 0, "caught:")
    check([event("fail", output="build failed")], 1, 2, "infrastructure:")
    check([event("run", "TestInvariant"), event("fail", "TestInvariant", "panic: test timed out")], 1, 2, "infrastructure:")
    print("test-mutation-contract-observer: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
