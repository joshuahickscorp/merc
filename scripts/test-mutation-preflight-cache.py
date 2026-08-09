#!/usr/bin/env python3
"""Hostile fixtures for the aggregate mutation preflight cache."""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]


def run(root: Path, *args: str, expect: int = 0) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        [sys.executable, str(root / "scripts" / "mutation-preflight-cache.py"), *args],
        text=True,
        capture_output=True,
        check=False,
    )
    if result.returncode != expect:
        raise AssertionError(
            f"cache command {args!r} returned {result.returncode}, expected {expect}: "
            f"stdout={result.stdout!r} stderr={result.stderr!r}"
        )
    return result


def git(root: Path, *args: str) -> None:
    subprocess.run(["git", *args], cwd=root, check=True, stdout=subprocess.DEVNULL)


def write_log(path: Path, *, database: bool = False) -> None:
    events = []
    for name in ("TestPrice", "TestPriceDB", "TestStore"):
        events.append({"Action": "run", "Test": name})
        events.append(
            {
                "Action": "skip" if database and name == "TestPriceDB" else "pass",
                "Test": name,
            }
        )
    path.write_text("\n".join(json.dumps(event) for event in events) + "\n", encoding="utf-8")


def fixture(root: Path) -> tuple[Path, Path, Path]:
    (root / "scripts").mkdir()
    (root / "control").mkdir()
    for name in (
        "mutation-preflight-cache.py",
        "mutation-test-contracts.py",
        "mutation-contract-observer.py",
    ):
        shutil.copy2(ROOT / "scripts" / name, root / "scripts" / name)
    (root / "scripts" / "mutation-test-contracts.json").write_text(
        json.dumps(
            {
                "version": 1,
                "contracts": {"price.go": ["price_test.go"], "store.go": ["store_test.go"]},
            }
        ),
        encoding="utf-8",
    )
    (root / "control" / "price.go").write_text("package control\n", encoding="utf-8")
    (root / "control" / "store.go").write_text("package control\n", encoding="utf-8")
    (root / "control" / "price_test.go").write_text(
        "package control\nfunc TestPrice(t *testing.T) {}\nfunc TestPriceDB(t *testing.T) {}\n",
        encoding="utf-8",
    )
    (root / "control" / "store_test.go").write_text(
        "package control\nfunc TestStore(t *testing.T) {}\n", encoding="utf-8"
    )
    git(root, "init", "-q")
    git(root, "config", "user.email", "mutation@example.invalid")
    git(root, "config", "user.name", "Mutation Fixture")
    git(root, "add", ".")
    git(root, "commit", "-qm", "fixture")
    # The real runner keeps proof material outside its candidate worktree; an
    # untracked cache inside this fixture would correctly be refused as dirty.
    proof = root.parent / "proof"
    proof.mkdir()
    sources = proof / "sources"
    sources.write_text("price.go\nstore.go\n", encoding="utf-8")
    write_log(proof / "preflight-unit.json")
    write_log(proof / "preflight-db.json", database=True)
    return proof, sources, proof / "preflight-cache.json"


def selector_names(value: str) -> list[str]:
    value = value.strip()
    if not value.startswith("^(") or not value.endswith(")$"):
        raise AssertionError(f"selector is not exactly anchored: {value!r}")
    names = value[2:-2].split("|")
    if not names or any(not name for name in names):
        raise AssertionError(f"selector has an empty test name: {value!r}")
    return names


def main() -> int:
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary) / "candidate"
        root.mkdir()
        proof, sources, cache = fixture(root)
        selector_command = ("--root", str(root), "--sources", str(sources))
        whole = selector_names(run(root, *selector_command, "--selector").stdout)
        first_shards = run(
            root, *selector_command, "--selector-shards", "2"
        ).stdout
        second_shards = run(
            root, *selector_command, "--selector-shards", "2"
        ).stdout
        if first_shards != second_shards:
            raise AssertionError("selector sharding is not byte-for-byte deterministic")
        lanes: dict[int, list[str]] = {}
        for line in first_shards.splitlines():
            raw_lane, raw_selector = line.split("\t", 1)
            lane = int(raw_lane)
            if lane in lanes:
                raise AssertionError(f"selector sharding repeated lane {lane}")
            lanes[lane] = selector_names(raw_selector)
        if sorted(lanes) != [1, 2] or lanes[1] != whole[0::2] or lanes[2] != whole[1::2]:
            raise AssertionError(f"selector shards are not stable lexical round-robin: {lanes!r}")
        flattened = [name for lane in sorted(lanes) for name in lanes[lane]]
        if len(flattened) != len(set(flattened)) or set(flattened) != set(whole):
            raise AssertionError("selector shards do not form a disjoint complete union")
        run(root, *selector_command, "--selector-shards", "0", expect=1)
        run(root, *selector_command, "--selector-shards", "4", expect=1)

        command = ("--root", str(root), "--sources", str(sources), "--cache", str(cache))
        created = run(root, *command, "--create")
        if "CREATED 2 exact source contracts" not in created.stdout:
            raise AssertionError(f"unexpected create result: {created.stdout!r}")
        verified = run(root, *command, "--verify")
        if "PASS 2 exact source contracts" not in verified.stdout:
            raise AssertionError(f"unexpected verify result: {verified.stdout!r}")

        os.chmod(proof / "preflight-unit.json", 0o644)
        (proof / "preflight-unit.json").write_text("{}\n", encoding="utf-8")
        run(root, *command, "--verify", expect=1)
        write_log(proof / "preflight-unit.json")

        os.chmod(cache, 0o644)
        payload = json.loads(cache.read_text(encoding="utf-8"))
        payload["sources"][0]["source_sha256"] = "0" * 64
        cache.write_text(json.dumps(payload), encoding="utf-8")
        run(root, *command, "--verify", expect=1)

        with (root / "control" / "store_test.go").open("a", encoding="utf-8") as handle:
            handle.write("// newArtifactHarness( requires per-lane MinIO\n")
        storage = run(root, *selector_command, "--selector-shards", "2", expect=1)
        if "per-lane isolated object storage" not in storage.stderr:
            raise AssertionError(f"storage contract guard did not explain refusal: {storage.stderr!r}")
    print("test-mutation-preflight-cache: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
