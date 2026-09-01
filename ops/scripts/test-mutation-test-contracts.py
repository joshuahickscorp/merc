#!/usr/bin/env python3
"""Fail closed if a fast mutation contract can silently lose its test surface."""

from __future__ import annotations

import json
import os
import re
import subprocess
import sys
import importlib.util
from pathlib import Path


ROOT = Path(__file__).resolve().parent.parent.parent
CONTRACT_RESOLVER = ROOT / "ops/scripts" / "mutation-test-contracts.py"
spec = importlib.util.spec_from_file_location("mutation_test_contracts", CONTRACT_RESOLVER)
if spec is None or spec.loader is None:
    raise RuntimeError(f"cannot load {CONTRACT_RESOLVER}")
contracts = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = contracts
spec.loader.exec_module(contracts)


def mutation_sources() -> set[str]:
    env = os.environ.copy()
    env.update({"MERC_MUTATION_LIST": "1", "MERC_MUTATION_LIST_DETAIL": "1"})
    output = subprocess.check_output(
        ["bash", "ops/scripts/mutation-test.sh"], cwd=ROOT, env=env, text=True
    )
    sources: set[str] = set()
    for line in output.splitlines():
        parts = line.split("\t", 2)
        if len(parts) != 3 or not parts[0].isdigit() or not parts[1] or not parts[2]:
            raise AssertionError(f"invalid detailed mutation listing: {line!r}")
        sources.add(parts[1])
    if not sources:
        raise AssertionError("mutation listing has no source files")
    return sources


def listed_tests() -> set[str]:
    output = subprocess.check_output(
        ["go", "test", "-list", "^Test", "."], cwd=ROOT / "src/control", text=True
    )
    return {line.strip() for line in output.splitlines() if re.fullmatch(r"Test[A-Za-z0-9_]+", line.strip())}


def main() -> int:
    sources = mutation_sources()
    declared = contracts.load_contracts(ROOT)
    missing = sorted(sources - set(declared))
    extra = sorted(set(declared) - sources)
    if missing or extra:
        raise AssertionError(f"contract/source mismatch: missing={missing}, extra={extra}")

    available = listed_tests()
    selected: set[str] = set()
    source_contracts: dict[str, list[str]] = {}
    for source in sorted(sources):
        names = contracts.resolve(ROOT, source)
        source_contracts[source] = names
        absent = sorted(set(names) - available)
        if absent:
            raise AssertionError(f"contract for {source} names unavailable tests: {absent}")
        selected.update(names)

    if not selected:
        raise AssertionError("contracts resolve to no invariant tests")

    manifest = json.loads(
        (ROOT / "ops/scripts" / "mutation-manifest.json").read_text(encoding="utf-8")
    )
    by_id = {
        item["id"]: item
        for item in manifest["mutations"]
        if isinstance(item, dict) and isinstance(item.get("id"), int)
    }
    batch_ids = sorted(by_id)
    batch_output = subprocess.check_output(
        [sys.executable, str(CONTRACT_RESOLVER), "--root", str(ROOT), "--case-ids", *map(str, batch_ids)],
        text=True,
    )
    batch_rows = {}
    for line in batch_output.splitlines():
        parts = line.split("\t")
        if len(parts) != 4 or not parts[0].isdigit() or not parts[1] or not parts[2] or not parts[3]:
            raise AssertionError(f"invalid batch contract row: {line!r}")
        case_id = int(parts[0])
        names = parts[3].split(",")
        if parts[2] != "^(" + "|".join(names) + ")$":
            raise AssertionError(f"batch selector disagrees with names for case {case_id}")
        batch_rows[case_id] = (parts[1], names)
    if set(batch_rows) != set(batch_ids):
        raise AssertionError("batch contract resolution did not return every mutation case exactly once")
    narrowed = contracts.load_case_contracts(ROOT)
    for case_id, item in by_id.items():
        source = item["source_target"].removeprefix("src/control/")
        expected = narrowed.get(case_id, source_contracts[source])
        if batch_rows[case_id] != (source, expected):
            raise AssertionError(f"batch contract case {case_id} disagrees with manifest: {batch_rows[case_id]} != {(source, expected)}")
    for case_id, names in sorted(contracts.load_case_contracts(ROOT).items()):
        entry = by_id.get(case_id)
        if entry is None:
            raise AssertionError(f"case contract names unknown mutation {case_id}")
        source = entry["source_target"].removeprefix("src/control/")
        resolved = contracts.resolve_case(ROOT, case_id, source)
        if resolved != names:
            raise AssertionError(
                f"case contract {case_id} resolved differently: {resolved} != {names}"
            )
        absent = sorted(set(names) - available)
        if absent:
            raise AssertionError(f"case contract {case_id} names unavailable tests: {absent}")

    print(
        f"mutation contracts: PASS {len(sources)} source contracts / "
        f"{len(selected)} named invariant tests / {len(contracts.load_case_contracts(ROOT))} narrowed cases"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
