#!/usr/bin/env python3
"""Resolve the explicit fast mutation-test contract for one production file.

The complete test suite remains available through MERC_MUTATION_TEST_STRATEGY=oracle
(aliases: full, whole-suite).
The checkpoint path uses this resolver for each unit-suite survivor, so it must
then fail a named database invariant. A missing mapping, missing test file, or
mapping that resolves to no Test function is a hard failure--never a skip.

Most mutations use the complete contract for their production file. A small
number of expensive database mutations may additionally name a narrower guard
in mutation-case-contracts.json. Those guards are checked to be a subset of
the source contract, while the aggregate clean preflight still runs the full
source contract before any mutant is scored.
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
SAFE_TEST_NAME = re.compile(r"^Test[A-Za-z0-9_]+$")


def load_contracts(root: Path) -> dict[str, list[str]]:
    path = root / "ops" / "scripts" / "mutation-test-contracts.json"
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


def load_case_contracts(root: Path) -> dict[int, list[str]]:
    path = root / "ops" / "scripts" / "mutation-case-contracts.json"
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"mutation case contract manifest is unreadable: {exc}") from exc
    if payload.get("version") != 1 or not isinstance(payload.get("cases"), dict):
        raise SystemExit("mutation case contract manifest must be version 1 with cases")
    cases: dict[int, list[str]] = {}
    for raw_case_id, tests in payload["cases"].items():
        try:
            case_id = int(raw_case_id)
        except (TypeError, ValueError) as exc:
            raise SystemExit(f"invalid mutation case contract ID: {raw_case_id!r}") from exc
        if case_id <= 0 or str(case_id) != str(raw_case_id):
            raise SystemExit(f"invalid mutation case contract ID: {raw_case_id!r}")
        if not isinstance(tests, list) or not tests:
            raise SystemExit(f"mutation case contract {case_id} needs test names")
        checked: list[str] = []
        for test in tests:
            if not isinstance(test, str) or not SAFE_TEST_NAME.fullmatch(test):
                raise SystemExit(f"invalid test name for mutation case {case_id}: {test!r}")
            if test in checked:
                raise SystemExit(f"duplicate test name for mutation case {case_id}: {test}")
            checked.append(test)
        cases[case_id] = checked
    return cases


def test_names(root: Path, source: str, tests: list[str]) -> list[str]:
    names: list[str] = []
    for test in tests:
        path = root / "src" / "control" / test
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
        raise SystemExit(f"no fast mutation contract is declared for src/control/{source}")
    return test_names(root, source, tests)


def resolve_case(root: Path, case_id: int, source_hint: str | None = None) -> list[str]:
    manifest_path = root / "ops" / "scripts" / "mutation-manifest.json"
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"mutation manifest is unreadable: {exc}") from exc
    mutations = manifest.get("mutations") if isinstance(manifest, dict) else None
    if not isinstance(mutations, list):
        raise SystemExit("mutation manifest must contain mutations")
    entry = next((item for item in mutations if isinstance(item, dict) and item.get("id") == case_id), None)
    if entry is None:
        raise SystemExit(f"mutation case {case_id} is absent from the mutation manifest")
    source_target = entry.get("source_target")
    if not isinstance(source_target, str):
        raise SystemExit(f"mutation case {case_id} lacks a source target")
    source = source_target.removeprefix("src/control/")
    if source_hint is not None and source != source_hint:
        raise SystemExit(
            f"mutation case {case_id} source mismatch: manifest={source} requested={source_hint}"
        )
    source_names = resolve(root, source)
    case_contracts = load_case_contracts(root)
    manifest_ids = {
        item.get("id") for item in mutations if isinstance(item, dict) and isinstance(item.get("id"), int)
    }
    unknown = sorted(set(case_contracts) - manifest_ids)
    if unknown:
        raise SystemExit(f"mutation case contract manifest names unknown cases: {unknown}")
    selected = case_contracts.get(case_id)
    if selected is None:
        return source_names
    missing = sorted(set(selected) - set(source_names))
    if missing:
        raise SystemExit(
            f"mutation case {case_id} names tests outside the {source} contract: {missing}"
        )
    return selected


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=".")
    parser.add_argument("--source")
    parser.add_argument("--case-id", type=int)
    parser.add_argument("--selector", action="store_true")
    parser.add_argument("--validate-sources", type=Path)
    args = parser.parse_args()
    root = Path(args.root).resolve()

    if args.validate_sources is not None:
        if args.source or args.case_id is not None:
            parser.error("--validate-sources cannot be combined with --source or --case-id")
        sources = [line.strip() for line in args.validate_sources.read_text(encoding="utf-8").splitlines()]
        if not sources or any(not source for source in sources):
            raise SystemExit("mutation contract source list must contain non-empty lines")
        if len(set(sources)) != len(sources):
            raise SystemExit("mutation contract source list contains duplicates")
        for source in sorted(sources):
            names = resolve(root, source)
            print(f"{source}: {len(names)} invariant tests")
        return 0
    if args.case_id is not None:
        names = resolve_case(root, args.case_id, args.source)
        if args.selector:
            print("^(" + "|".join(names) + ")$")
        else:
            print("\n".join(names))
        return 0
    if not args.source:
        parser.error("provide one of --source, --case-id, or --validate-sources")
    if args.source:
        names = resolve(root, args.source)
        if args.selector:
            print("^(" + "|".join(names) + ")$")
        else:
            print("\n".join(names))
        return 0
    raise AssertionError("unreachable")


if __name__ == "__main__":
    raise SystemExit(main())
