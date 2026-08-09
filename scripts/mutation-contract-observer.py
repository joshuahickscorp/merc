#!/usr/bin/env python3
"""Classify one JSON-mode Go mutation-contract run without trusting its exit code.

Mutation testing may only call a defect caught when one of the declared
invariant tests actually failed.  A timeout, package setup failure, compiler
failure, or a selector which skipped every named test is infrastructure, not a
catch.  The runner uses this small parser for both the fast unit contract and
the isolated-database fallback.
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


def root_test_name(name: str) -> str:
    return name.split("/", 1)[0]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--exit-code", type=int, required=True)
    parser.add_argument("--expected", action="append", default=[])
    completion = parser.add_mutually_exclusive_group()
    completion.add_argument(
        "--require-all-run",
        action="store_true",
        help="require every declared invariant to have emitted a run event",
    )
    completion.add_argument(
        "--require-all-pass",
        action="store_true",
        help="require every declared invariant to have passed",
    )
    args = parser.parse_args()

    expected = set(args.expected)
    if not expected:
        parser.error("at least one --expected test name is required")
    try:
        lines = args.log.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError as exc:
        print(f"infrastructure: unreadable test log: {exc}")
        return 2

    raw = "\n".join(lines).lower()
    if any(marker in raw for marker in TIMEOUT_MARKERS):
        print("infrastructure: timeout_or_kill")
        return 2

    actions: dict[str, set[str]] = {name: set() for name in expected}
    for line in lines:
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        name = event.get("Test")
        action = event.get("Action")
        if not isinstance(name, str) or not isinstance(action, str):
            continue
        root = root_test_name(name)
        if root in actions:
            actions[root].add(action)

    failed = sorted(name for name, seen in actions.items() if "fail" in seen)
    passed = sorted(name for name, seen in actions.items() if "pass" in seen)
    ran = sorted(name for name, seen in actions.items() if "run" in seen)
    if args.exit_code == 0:
        if args.require_all_run or args.require_all_pass:
            missing_runs = sorted(name for name, seen in actions.items() if "run" not in seen)
            if missing_runs:
                print("infrastructure: declared_test_not_run:" + ",".join(missing_runs))
                return 2
        if args.require_all_pass:
            missing_passes = sorted(name for name, seen in actions.items() if "pass" not in seen)
            if missing_passes:
                print("infrastructure: declared_test_not_passed:" + ",".join(missing_passes))
                return 2
        if passed:
            print("pass:" + ",".join(passed))
            return 0
        print("skipped:" + ",".join(ran))
        return 0
    if failed:
        print("caught:" + ",".join(failed))
        return 0
    print("infrastructure: nonzero_without_declared_test_failure")
    return 2


if __name__ == "__main__":
    raise SystemExit(main())
