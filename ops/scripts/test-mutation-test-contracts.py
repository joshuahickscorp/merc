#!/usr/bin/env python3
"""Fail closed if a fast mutation contract can silently lose its test surface."""

from __future__ import annotations

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
    for source in sorted(sources):
        names = contracts.resolve(ROOT, source)
        absent = sorted(set(names) - available)
        if absent:
            raise AssertionError(f"contract for {source} names unavailable tests: {absent}")
        selected.update(names)

    if not selected:
        raise AssertionError("contracts resolve to no invariant tests")
    print(
        f"mutation contracts: PASS {len(sources)} source contracts / "
        f"{len(selected)} named invariant tests"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
