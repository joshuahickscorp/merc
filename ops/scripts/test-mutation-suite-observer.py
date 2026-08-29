#!/usr/bin/env python3
"""Hostile fixtures for the whole-suite mutation observer."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
OBSERVER = ROOT / "ops/scripts" / "mutation-suite-observer.py"


def event(action: str, test: str | None = None, output: str | None = None, package: str | None = None) -> str:
    value: dict[str, str] = {"Action": action}
    if test is not None:
        value["Test"] = test
    if output is not None:
        value["Output"] = output
    if package is not None:
        value["Package"] = package
    return json.dumps(value)


def check(
    lines: list[str],
    exit_code: int,
    want_code: int,
    want_prefix: str,
    *extra: str,
) -> None:
    with tempfile.TemporaryDirectory() as temporary:
        log = Path(temporary) / "go.json"
        log.write_text("\n".join(lines) + "\n", encoding="utf-8")
        result = subprocess.run(
            [sys.executable, str(OBSERVER), "--log", str(log), "--exit-code", str(exit_code), *extra],
            text=True,
            capture_output=True,
            check=False,
        )
    if result.returncode != want_code or not result.stdout.startswith(want_prefix):
        raise SystemExit(
            f"suite observer mismatch: code={result.returncode} output={result.stdout!r}; "
            f"want {want_code}/{want_prefix!r}"
        )


def main() -> int:
    # Green suite.
    check(
        [event("run", "TestA"), event("pass", "TestA"), event("run", "TestB"), event("pass", "TestB")],
        0,
        0,
        "pass:",
    )
    # Real test failure is a catch.
    check(
        [event("run", "TestA"), event("fail", "TestA"), event("run", "TestB"), event("pass", "TestB")],
        1,
        0,
        "caught:TestA",
    )
    # Timeout is infrastructure even if a test name appears nearby.
    check(
        [
            event("run", "TestA"),
            event("output", "TestA", "panic: test timed out after 2m0s"),
            event("fail", "TestA"),
        ],
        1,
        2,
        "infrastructure: timeout_or_kill",
    )
    # Package/setup failure without a test failure is infrastructure.
    check(
        [event("fail", package="merc/control", output="setup failed")],
        1,
        2,
        "infrastructure:",
    )
    # Non-zero without any test fail event is infrastructure.
    check(
        [event("output", output="connection refused while creating database")],
        1,
        2,
        "infrastructure: nonzero_without_test_failure",
    )
    # Baseline mode refuses a red clean suite.
    check(
        [event("run", "TestUnrelated"), event("fail", "TestUnrelated")],
        1,
        2,
        "caught:TestUnrelated",
        "--require-pass",
    )
    # Baseline mode accepts a green clean suite.
    check(
        [event("run", "TestA"), event("pass", "TestA")],
        0,
        0,
        "pass:",
        "--require-pass",
    )
    print("test-mutation-suite-observer: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
