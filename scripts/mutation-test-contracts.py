#!/usr/bin/env python3
"""Resolve the explicit fast mutation-test contract for one production file.

The complete test suite remains available through MERC_MUTATION_TEST_STRATEGY=oracle
(aliases: full, whole-suite).
The checkpoint path uses this resolver for each unit-suite survivor, so it must
then fail a named database invariant. A missing mapping, missing test file, or
mapping that resolves to no Test function is a hard failure--never a skip.
"""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


TEST_DECLARATION = re.compile(r"^func\s+(Test[A-Za-z0-9_]+)\s*\(", re.MULTILINE)
SAFE_SOURCE = re.compile(r"^[A-Za-z0-9_]+\.go$")
SAFE_TEST_FILE = re.compile(r"^[A-Za-z0-9_]+_test\.go$")


def load_contracts(root: Path) -> dict[str, list[str]]:
    path = root / "scripts" / "mutation-test-contracts.json"
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"mutation contract manifest is unreadable: {exc}") from exc
    if payload.get("version") != 1 or not isinstance(payload.get("contracts"), dict):
        raise SystemExit("mutation contract manifest must be version 1 with contracts")
    contracts: dict[str, list[str]] = {}
    for source, tests in payload["contracts"].items():
        if not isinstance(source, str) or not SAFE_SOURCE.fullmatch(source):
            raise SystemExit(f"invalid mutation contract source: {source!r}")
        if not isinstance(tests, list) or not tests:
            raise SystemExit(f"mutation contract for {source} needs test files")
        checked: list[str] = []
        for test in tests:
            if not isinstance(test, str) or not SAFE_TEST_FILE.fullmatch(test):
                raise SystemExit(f"invalid test file for {source}: {test!r}")
            if test in checked:
                raise SystemExit(f"duplicate test file for {source}: {test}")
            checked.append(test)
        contracts[source] = checked
    return contracts


def test_names(root: Path, source: str, tests: list[str]) -> list[str]:
    names: list[str] = []
    for test in tests:
        path = root / "control" / test
        if not path.is_file():
            raise SystemExit(f"mutation contract for {source} names missing {path}")
        found = TEST_DECLARATION.findall(path.read_text(encoding="utf-8"))
        if not found:
            raise SystemExit(f"mutation contract for {source} names no Test functions in {path}")
        names.extend(found)
    unique = sorted(set(names))
    if len(unique) != len(names):
        duplicates = sorted(name for name in set(names) if names.count(name) > 1)
        raise SystemExit(f"mutation contract for {source} repeats test functions: {', '.join(duplicates)}")
    return unique


def resolve(root: Path, source: str) -> list[str]:
    if not SAFE_SOURCE.fullmatch(source):
        raise SystemExit(f"invalid source name: {source!r}")
    contracts = load_contracts(root)
    tests = contracts.get(source)
    if tests is None:
        raise SystemExit(f"no fast mutation contract is declared for control/{source}")
    return test_names(root, source, tests)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=".")
    parser.add_argument("--source")
    parser.add_argument("--selector", action="store_true")
    parser.add_argument("--validate-sources", type=Path)
    args = parser.parse_args()
    root = Path(args.root).resolve()

    if bool(args.source) == bool(args.validate_sources):
        parser.error("provide exactly one of --source or --validate-sources")
    if args.source:
        names = resolve(root, args.source)
        if args.selector:
            print("^(" + "|".join(names) + ")$")
        else:
            print("\n".join(names))
        return 0

    sources = [line.strip() for line in args.validate_sources.read_text(encoding="utf-8").splitlines()]
    if not sources or any(not source for source in sources):
        raise SystemExit("mutation contract source list must contain non-empty lines")
    if len(set(sources)) != len(sources):
        raise SystemExit("mutation contract source list contains duplicates")
    for source in sorted(sources):
        names = resolve(root, source)
        print(f"{source}: {len(names)} invariant tests")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
