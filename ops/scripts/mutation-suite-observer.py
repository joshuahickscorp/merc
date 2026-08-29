#!/usr/bin/env python3
"""Classify one whole-suite Go mutation run without trusting its exit code alone.

The oracle (whole-package) mutation strategy may only call a defect caught when
at least one real test failed. A timeout, package/setup/build failure, missing
LFS content, harness fault, or non-zero exit without a declared test failure is
infrastructure — the same vocabulary the contract observer and campaign
summaries already use. Unrelated red on a clean baseline is rejected by the
serial preflight before any mutant is scored.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


TIMEOUT_MARKERS = (
    "panic: test timed out",
    "test timed out after",
    "signal: killed",
)

# Non-test failure text that must never inflate the caught count.
INFRASTRUCTURE_MARKERS = (
    "build failed",
    "setup failed",
    "# ",
    "cannot find package",
    "no required module provides",
    "error obtaining vcs status",
    "lfs",
    "git-lfs",
    "no space left on device",
    "resource temporarily unavailable",
    "too many open files",
    "connection refused",
    "could not connect",
    "password authentication failed",
    "database",
    "pq:",
    "FATAL:",
)


def root_test_name(name: str) -> str:
    return name.split("/", 1)[0]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--exit-code", type=int, required=True)
    parser.add_argument(
        "--require-pass",
        action="store_true",
        help="baseline mode: any non-pass observation is a hard failure",
    )
    args = parser.parse_args()

    try:
        lines = args.log.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError as exc:
        print(f"infrastructure: unreadable test log: {exc}")
        return 2

    raw = "\n".join(lines)
    raw_lower = raw.lower()
    if any(marker in raw_lower for marker in TIMEOUT_MARKERS):
        print("infrastructure: timeout_or_kill")
        return 2

    failed: set[str] = set()
    passed: set[str] = set()
    package_failed = False
    saw_json = False
    for line in lines:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        saw_json = True
        action = event.get("Action")
        name = event.get("Test")
        if isinstance(name, str) and isinstance(action, str):
            root = root_test_name(name)
            if action == "fail":
                failed.add(root)
            elif action == "pass":
                passed.add(root)
            continue
        # Package-level fail without a Test field is setup/build, not a catch.
        if action == "fail" and not name:
            package_failed = True

    if failed:
        print("caught:" + ",".join(sorted(failed)))
        # Baseline must be green; a catch observation on the clean tree is failure.
        if args.require_pass:
            return 2
        return 0

    if args.exit_code == 0:
        if passed or saw_json:
            print("pass:" + ",".join(sorted(passed)) if passed else "pass:")
            return 0
        # Empty successful log is suspicious for a whole-suite run.
        print("infrastructure: empty_suite_log")
        return 2

    if package_failed:
        print("infrastructure: package_or_setup_failure")
        return 2

    if any(marker in raw_lower for marker in INFRASTRUCTURE_MARKERS):
        print("infrastructure: nonzero_without_test_failure")
        return 2

    print("infrastructure: nonzero_without_test_failure")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
