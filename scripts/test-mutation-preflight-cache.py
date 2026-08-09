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


def write_log(path: Path) -> None:
    events = []
    for name in ("TestPrice", "TestStore"):
        events.extend(({"Action": "run", "Test": name}, {"Action": "pass", "Test": name}))
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
        "package control\nfunc TestPrice(t *testing.T) {}\n", encoding="utf-8"
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
    write_log(proof / "preflight-db.json")
    return proof, sources, proof / "preflight-cache.json"


def main() -> int:
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary) / "candidate"
        root.mkdir()
        proof, sources, cache = fixture(root)
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
    print("test-mutation-preflight-cache: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
