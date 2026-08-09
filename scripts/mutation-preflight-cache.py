#!/usr/bin/env python3
"""Create and verify an exact-candidate aggregate mutation-contract preflight.

The parallel mutation runner used to repeat clean unit and database preflights
for every worker/source pair.  This cache is deliberately not a trust shortcut:
it records one aggregate clean run, then every mutation worker re-hashes and
re-parses the two Go JSON logs for the exact source contracts it will mutate.
Any changed candidate, source, contract mapping, missing test event, skipped
database test, or altered log fails closed before source mutation begins.
"""

from __future__ import annotations

import argparse
import hashlib
import importlib.util
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


VERSION = 1
CACHE_NAME = "preflight-cache.json"
UNIT_LOG_NAME = "preflight-unit.json"
DB_LOG_NAME = "preflight-db.json"
HEX_SHA256 = re.compile(r"^[0-9a-f]{64}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")


def fail(message: str) -> "NoReturn":
    raise SystemExit(f"mutation-preflight-cache: {message}")


def sha256_file(path: Path) -> str:
    try:
        return hashlib.sha256(path.read_bytes()).hexdigest()
    except OSError as exc:
        fail(f"cannot read {path}: {exc}")


def load_contract_resolver(root: Path) -> Any:
    path = root / "scripts" / "mutation-test-contracts.py"
    spec = importlib.util.spec_from_file_location("mutation_test_contracts", path)
    if spec is None or spec.loader is None:
        fail(f"cannot load contract resolver {path}")
    # The validator must not make a clean frozen worktree look dirty merely by
    # importing the resolver from an isolated worker checkout.
    sys.dont_write_bytecode = True
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def git(root: Path, *args: str) -> str:
    result = subprocess.run(
        ["git", *args], cwd=root, text=True, capture_output=True, check=False
    )
    if result.returncode:
        fail(f"git {' '.join(args)} failed: {result.stderr.strip()}")
    return result.stdout.strip()


def exact_clean_candidate(root: Path) -> str:
    candidate = git(root, "rev-parse", "HEAD^{commit}")
    if not COMMIT.fullmatch(candidate):
        fail(f"candidate is not a full commit: {candidate!r}")
    if git(root, "status", "--porcelain"):
        fail("candidate worktree is dirty")
    return candidate


def load_sources(path: Path, resolver: Any) -> list[str]:
    try:
        raw = path.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        fail(f"cannot read source list {path}: {exc}")
    if not raw or any(not source for source in raw):
        fail("source list must contain only non-empty source names")
    if len(set(raw)) != len(raw):
        fail("source list contains duplicates")
    for source in raw:
        if not resolver.SAFE_SOURCE.fullmatch(source):
            fail(f"invalid source name {source!r}")
    return sorted(raw)


def selector(expected: list[str]) -> str:
    return "^(" + "|".join(expected) + ")$"


def source_records(root: Path, sources: list[str], resolver: Any) -> list[dict[str, Any]]:
    records: list[dict[str, Any]] = []
    for source in sources:
        path = root / "control" / source
        if not path.is_file():
            fail(f"mutation source is missing: {path}")
        expected = resolver.resolve(root, source)
        records.append(
            {
                "source": source,
                "source_sha256": sha256_file(path),
                "expected": expected,
                "selector": selector(expected),
            }
        )
    return records


def run_observer(
    root: Path, log: Path, expected: list[str], requirement: str
) -> str:
    command = [
        sys.executable,
        str(root / "scripts" / "mutation-contract-observer.py"),
        "--log",
        str(log),
        "--exit-code",
        "0",
        "--" + requirement,
    ]
    for name in expected:
        command.extend(("--expected", name))
    result = subprocess.run(command, text=True, capture_output=True, check=False)
    observation = result.stdout.strip()
    if result.returncode:
        detail = observation or result.stderr.strip() or "no observer output"
        fail(f"{log.name} does not prove its declared invariants: {detail}")
    return observation


def validate_logs(root: Path, cache_dir: Path, records: list[dict[str, Any]]) -> tuple[str, str, dict[str, tuple[str, str]]]:
    unit_log = cache_dir / UNIT_LOG_NAME
    db_log = cache_dir / DB_LOG_NAME
    if not unit_log.is_file() or not db_log.is_file():
        fail("aggregate preflight logs are missing")
    outcomes: dict[str, tuple[str, str]] = {}
    for record in records:
        expected = record["expected"]
        unit = run_observer(root, unit_log, expected, "require-all-run")
        if not (unit.startswith("pass:") or unit.startswith("skipped:")):
            fail(f"unit aggregate gave an invalid observation for {record['source']}: {unit}")
        db = run_observer(root, db_log, expected, "require-all-pass")
        if not db.startswith("pass:"):
            fail(f"database aggregate did not pass every invariant for {record['source']}: {db}")
        outcomes[record["source"]] = (unit, db)
    return sha256_file(unit_log), sha256_file(db_log), outcomes


def cache_path(value: str) -> Path:
    path = Path(value).resolve()
    if path.name != CACHE_NAME:
        fail(f"cache file must be named {CACHE_NAME}")
    return path


def create(root: Path, cache: Path, sources: list[str], resolver: Any) -> None:
    candidate = exact_clean_candidate(root)
    records = source_records(root, sources, resolver)
    unit_sha, db_sha, outcomes = validate_logs(root, cache.parent, records)
    for record in records:
        unit, db = outcomes[record["source"]]
        record["unit_observation"] = unit
        record["db_observation"] = db
    payload = {
        "version": VERSION,
        "candidate": candidate,
        "contracts_sha256": sha256_file(root / "scripts" / "mutation-test-contracts.json"),
        "unit_log_sha256": unit_sha,
        "db_log_sha256": db_sha,
        "sources": records,
    }
    cache.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temporary = cache.with_name(cache.name + ".tmp")
    temporary.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    os.chmod(temporary, 0o444)
    os.replace(temporary, cache)
    print(f"mutation-preflight-cache: CREATED {len(records)} exact source contracts")


def require_exact_string(payload: dict[str, Any], name: str, pattern: re.Pattern[str]) -> str:
    value = payload.get(name)
    if not isinstance(value, str) or not pattern.fullmatch(value):
        fail(f"cache has invalid {name}")
    return value


def verify(root: Path, cache: Path, sources: list[str], resolver: Any) -> None:
    try:
        payload = json.loads(cache.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cache is unreadable: {exc}")
    if not isinstance(payload, dict) or payload.get("version") != VERSION:
        fail("cache has an unsupported schema")
    if payload.get("candidate") != exact_clean_candidate(root):
        fail("cache candidate does not match this worktree")
    if payload.get("contracts_sha256") != sha256_file(root / "scripts" / "mutation-test-contracts.json"):
        fail("cache contract manifest does not match this worktree")
    unit_sha = require_exact_string(payload, "unit_log_sha256", HEX_SHA256)
    db_sha = require_exact_string(payload, "db_log_sha256", HEX_SHA256)
    raw_records = payload.get("sources")
    if not isinstance(raw_records, list) or not raw_records:
        fail("cache has no source records")
    by_source: dict[str, dict[str, Any]] = {}
    for record in raw_records:
        if not isinstance(record, dict) or not isinstance(record.get("source"), str):
            fail("cache has an invalid source record")
        source = record["source"]
        if source in by_source:
            fail(f"cache repeats source {source}")
        by_source[source] = record
    current = source_records(root, sources, resolver)
    selected: list[dict[str, Any]] = []
    for record in current:
        cached = by_source.get(record["source"])
        if cached is None:
            fail(f"cache is missing selected source {record['source']}")
        for field in ("source", "source_sha256", "expected", "selector"):
            if cached.get(field) != record[field]:
                fail(f"cache source record is stale for {record['source']}: {field}")
        selected.append(record)
    if sha256_file(cache.parent / UNIT_LOG_NAME) != unit_sha:
        fail("aggregate unit log hash does not match cache")
    if sha256_file(cache.parent / DB_LOG_NAME) != db_sha:
        fail("aggregate database log hash does not match cache")
    _, _, outcomes = validate_logs(root, cache.parent, selected)
    for record in selected:
        cached = by_source[record["source"]]
        if cached.get("unit_observation") != outcomes[record["source"]][0] or cached.get("db_observation") != outcomes[record["source"]][1]:
            fail(f"cache observations do not match logs for {record['source']}")
    print(f"mutation-preflight-cache: PASS {len(selected)} exact source contracts")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=".")
    parser.add_argument("--sources", type=Path, required=True)
    parser.add_argument("--cache")
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--selector", action="store_true")
    mode.add_argument("--create", action="store_true")
    mode.add_argument("--verify", action="store_true")
    args = parser.parse_args()
    root = Path(args.root).resolve()
    resolver = load_contract_resolver(root)
    sources = load_sources(args.sources.resolve(), resolver)
    if args.selector:
        names = sorted({name for source in sources for name in resolver.resolve(root, source)})
        if not names:
            fail("selected sources resolve to no invariant tests")
        print(selector(names))
        return 0
    if not args.cache:
        parser.error("--cache is required with --create or --verify")
    cache = cache_path(args.cache)
    if args.create:
        create(root, cache, sources, resolver)
    else:
        verify(root, cache, sources, resolver)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
