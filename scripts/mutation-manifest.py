#!/usr/bin/env python3
"""Generate, validate, update, and weight the durable mutation manifest.

The manifest is deliberately data rather than an alternate source of truth for
mutants: the shell runner still declares the 84 actual transformations.  This
tool joins that inventory to the named invariant contracts, authority policy,
and observed runtimes, refusing gaps or duplicates before a scheduler can use
the data.
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
import subprocess
import sys
from pathlib import Path
from typing import Any


VALID_CLASSES = {"PURE", "DB", "LFS", "AGENT", "CROSS_PROCESS"}


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"mutation manifest input is unreadable ({path}): {exc}") from exc


def source_inventory(root: Path) -> list[tuple[int, str, str]]:
    result = subprocess.run(
        ["bash", "scripts/mutation-test.sh"],
        cwd=root,
        env={**__import__("os").environ, "MERC_MUTATION_LIST": "1", "MERC_MUTATION_LIST_DETAIL": "1"},
        check=False,
        text=True,
        capture_output=True,
    )
    if result.returncode:
        raise SystemExit(f"unable to read mutation inventory: {result.stderr.strip()}")
    entries: list[tuple[int, str, str]] = []
    for line in result.stdout.splitlines():
        parts = line.split("\t", 2)
        if len(parts) != 3 or not parts[0].isdigit() or not parts[1] or not parts[2]:
            raise SystemExit(f"invalid mutation inventory row: {line!r}")
        entries.append((int(parts[0]), parts[1], parts[2]))
    if [entry[0] for entry in entries] != list(range(1, len(entries) + 1)):
        raise SystemExit("mutation inventory IDs are not contiguous")
    return entries


def invariant_tests(root: Path, source: str) -> list[str]:
    result = subprocess.run(
        ["python3", "scripts/mutation-test-contracts.py", "--root", ".", "--source", source],
        cwd=root,
        check=False,
        text=True,
        capture_output=True,
    )
    if result.returncode:
        raise SystemExit(f"cannot resolve mutation contract for {source}: {result.stderr.strip()}")
    names = [line for line in result.stdout.splitlines() if line]
    if not names or len(names) != len(set(names)):
        raise SystemExit(f"invalid invariant contract set for {source}")
    return names


def percentile(values: list[float], fraction: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return round(ordered[0], 6)
    position = (len(ordered) - 1) * fraction
    lower = math.floor(position)
    upper = math.ceil(position)
    if lower == upper:
        return round(ordered[lower], 6)
    return round(ordered[lower] + (ordered[upper] - ordered[lower]) * (position - lower), 6)


def policy(root: Path) -> dict[str, dict[str, str]]:
    value = load_json(root / "scripts" / "mutation-source-policy.json")
    if value.get("version") != 1 or not isinstance(value.get("sources"), dict):
        raise SystemExit("mutation source policy must be version 1 with sources")
    source_policy: dict[str, dict[str, str]] = {}
    for source, item in value["sources"].items():
        if not isinstance(source, str) or not isinstance(item, dict):
            raise SystemExit("invalid mutation source policy entry")
        domain = item.get("authority_domain")
        classification = item.get("default_class")
        if not isinstance(domain, str) or not domain:
            raise SystemExit(f"mutation source policy {source} lacks authority_domain")
        if classification not in VALID_CLASSES:
            raise SystemExit(f"mutation source policy {source} has invalid default_class")
        source_policy[source] = {"authority_domain": domain, "default_class": classification}
    return source_policy


def validate(manifest: dict[str, Any], inventory: list[tuple[int, str, str]]) -> None:
    if manifest.get("version") != 1 or not isinstance(manifest.get("mutations"), list):
        raise SystemExit("mutation manifest must be version 1 with mutations")
    mutations = manifest["mutations"]
    expected = {(case_id, source, description) for case_id, source, description in inventory}
    actual: set[tuple[int, str, str]] = set()
    for item in mutations:
        if not isinstance(item, dict):
            raise SystemExit("mutation manifest contains a non-object entry")
        case_id = item.get("id")
        source = item.get("source_target")
        description = item.get("description")
        if not isinstance(case_id, int) or not isinstance(source, str) or not isinstance(description, str):
            raise SystemExit("mutation manifest entry lacks ID/source/description")
        key = (case_id, source.removeprefix("control/"), description)
        if key in actual:
            raise SystemExit(f"duplicate mutation manifest entry {case_id}")
        actual.add(key)
        if item.get("class") not in VALID_CLASSES:
            raise SystemExit(f"mutation {case_id} has invalid class")
        if not isinstance(item.get("authority_domain"), str) or not item["authority_domain"]:
            raise SystemExit(f"mutation {case_id} lacks authority_domain")
        contracts = item.get("required_invariant_contracts")
        if not isinstance(contracts, list) or not contracts or any(not isinstance(v, str) or not v for v in contracts):
            raise SystemExit(f"mutation {case_id} lacks required invariant contracts")
        historical = item.get("historical")
        if not isinstance(historical, dict):
            raise SystemExit(f"mutation {case_id} lacks historical timing")
        samples = historical.get("samples_seconds")
        if not isinstance(samples, list) or any(not isinstance(v, (int, float)) or v < 0 for v in samples):
            raise SystemExit(f"mutation {case_id} has invalid timing samples")
        for field in ("p50_seconds", "p90_seconds"):
            value = historical.get(field)
            if samples and not isinstance(value, (int, float)):
                raise SystemExit(f"mutation {case_id} has timing samples but no {field}")
            if not samples and value is not None:
                raise SystemExit(f"mutation {case_id} has {field} without timing samples")
        commit = item.get("last_validated_commit")
        if commit is not None and (not isinstance(commit, str) or len(commit) != 40):
            raise SystemExit(f"mutation {case_id} has invalid last_validated_commit")
    if actual != expected:
        missing = sorted(expected - actual)
        extra = sorted(actual - expected)
        raise SystemExit(f"mutation manifest drift: missing={missing!r} extra={extra!r}")


def build(root: Path, existing: dict[str, Any] | None) -> dict[str, Any]:
    inventory = source_inventory(root)
    source_policy = policy(root)
    sources = {source for _, source, _ in inventory}
    if set(source_policy) != sources:
        raise SystemExit(f"mutation source policy drift: missing={sorted(sources - set(source_policy))} extra={sorted(set(source_policy) - sources)}")
    prior = {item["id"]: item for item in (existing or {}).get("mutations", []) if isinstance(item, dict) and isinstance(item.get("id"), int)}
    mutations: list[dict[str, Any]] = []
    for case_id, source, description in inventory:
        old = prior.get(case_id)
        old_is_same = old and old.get("source_target") == f"control/{source}" and old.get("description") == description
        old_historical = old.get("historical") if old_is_same and isinstance(old.get("historical"), dict) else {}
        samples = old_historical.get("samples_seconds", [])
        if not isinstance(samples, list) or any(not isinstance(v, (int, float)) or v < 0 for v in samples):
            samples = []
        chosen_class = old.get("class") if old_is_same and old.get("class") in VALID_CLASSES else source_policy[source]["default_class"]
        mutations.append({
            "id": case_id,
            "source_target": f"control/{source}",
            "description": description,
            "authority_domain": source_policy[source]["authority_domain"],
            "class": chosen_class,
            "required_invariant_contracts": invariant_tests(root, source),
            "historical": {
                "samples_seconds": [round(float(value), 6) for value in samples],
                "p50_seconds": percentile([float(value) for value in samples], 0.50),
                "p90_seconds": percentile([float(value) for value in samples], 0.90),
            },
            "last_validated_commit": old.get("last_validated_commit") if old_is_same else None,
        })
    result = {"version": 1, "mutations": mutations}
    validate(result, inventory)
    return result


def ingest(manifest: dict[str, Any], timing_files: list[Path], commit: str | None, require_complete: bool) -> None:
    by_id = {item["id"]: item for item in manifest["mutations"]}
    seen: set[int] = set()
    candidate: str | None = None
    for timing_file in timing_files:
        for line_number, line in enumerate(timing_file.read_text(encoding="utf-8").splitlines(), 1):
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError as exc:
                raise SystemExit(f"invalid timing JSON {timing_file}:{line_number}: {exc}") from exc
            case_id = record.get("case_id")
            if case_id not in by_id or case_id in seen:
                raise SystemExit(f"timing records have a missing or duplicate case ID: {case_id!r}")
            if record.get("result") != "caught" or record.get("pathway") not in {"PURE", "DB"}:
                raise SystemExit(f"timing record {case_id} is not a legitimate catch")
            duration = record.get("duration_seconds")
            if not isinstance(duration, (int, float)) or duration < 0:
                raise SystemExit(f"timing record {case_id} has invalid duration")
            source = record.get("source")
            if source != by_id[case_id]["source_target"].removeprefix("control/"):
                raise SystemExit(f"timing record {case_id} has a source mismatch")
            current_candidate = record.get("candidate")
            if not isinstance(current_candidate, str) or len(current_candidate) != 40:
                raise SystemExit(f"timing record {case_id} lacks exact candidate")
            if candidate is None:
                candidate = current_candidate
            elif candidate != current_candidate:
                raise SystemExit("timing files mix candidate commits")
            seen.add(case_id)
            entry = by_id[case_id]
            samples = entry["historical"]["samples_seconds"]
            samples.append(round(float(duration), 6))
            entry["class"] = record["pathway"]
            entry["historical"]["p50_seconds"] = percentile(samples, 0.50)
            entry["historical"]["p90_seconds"] = percentile(samples, 0.90)
            entry["last_validated_commit"] = commit or candidate
    if require_complete and seen != set(by_id):
        raise SystemExit(f"timing records are not a complete mutation campaign: missing={sorted(set(by_id) - seen)}")


def write_json(path: Path, value: dict[str, Any]) -> None:
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", default=".")
    parser.add_argument("--manifest", type=Path)
    parser.add_argument("--write", action="store_true")
    parser.add_argument("--validate", action="store_true")
    parser.add_argument("--weights", action="store_true")
    parser.add_argument("--ingest", type=Path, action="append", default=[])
    parser.add_argument("--commit")
    parser.add_argument("--require-complete", action="store_true")
    args = parser.parse_args()
    root = Path(args.root).resolve()
    path = (args.manifest if args.manifest else root / "scripts" / "mutation-manifest.json").resolve()
    existing = load_json(path) if path.exists() else None
    manifest = build(root, existing)
    if args.ingest:
        ingest(manifest, args.ingest, args.commit, args.require_complete)
    validate(manifest, source_inventory(root))
    if args.weights:
        for item in manifest["mutations"]:
            weight = item["historical"]["p90_seconds"]
            if not isinstance(weight, (int, float)) or weight <= 0:
                weight = 60.0
            print(f"{item['id']}\t{float(weight):.6f}")
    if args.write:
        write_json(path, manifest)
    if args.validate:
        print(f"mutation-manifest: PASS {len(manifest['mutations'])} mutations")
    if not (args.write or args.validate or args.weights):
        parser.error("choose --write, --validate, or --weights")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
