#!/usr/bin/env python3
"""Independent LFS content-integrity authority for the merc corpus.

Fail-closed gate. Derives counts from the live tree and asserts them against
evidence/state/lfs-corpus-ledger.json. Computes sha256 of every resolved
payload itself — does not shell to `git lfs fsck` for the verdict. May run
fsck as a supplementary cross-check and report both.

    python3 scripts/verify-lfs-corpus.py
    python3 scripts/verify-lfs-corpus.py --root DIR
    python3 scripts/verify-lfs-corpus.py --json

Exit 0 only when every expected count matches and every object hashes clean.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
from collections import defaultdict
from pathlib import Path
from typing import Any


LFS_POINTER_PREFIX = "version https://git-lfs.github.com/spec/v1"
LEDGER_REL = "evidence/state/lfs-corpus-ledger.json"


class Fail(Exception):
    pass


def git(root: Path, *args: str, check: bool = True) -> str:
    proc = subprocess.run(
        ["git", "-C", str(root), *args],
        capture_output=True,
        text=True,
    )
    if check and proc.returncode != 0:
        detail = (proc.stderr or proc.stdout or "").strip()
        raise Fail(f"git {' '.join(args)} failed: {detail}")
    return proc.stdout


def git_common_dir(root: Path) -> Path:
    raw = git(root, "rev-parse", "--git-common-dir").strip()
    p = Path(raw)
    if not p.is_absolute():
        p = (root / p).resolve()
    return p


def parse_lfs_pointer(text: str) -> tuple[str, int] | None:
    if not text.startswith(LFS_POINTER_PREFIX):
        return None
    oid = ""
    size = -1
    for line in text.splitlines():
        if line.startswith("oid sha256:"):
            oid = line[len("oid sha256:") :].strip().lower()
        elif line.startswith("size "):
            try:
                size = int(line[len("size ") :].strip())
            except ValueError:
                size = -1
    if len(oid) == 64 and all(c in "0123456789abcdef" for c in oid) and size >= 0:
        return oid, size
    return None


def is_pointer_bytes(data: bytes) -> bool:
    if len(data) == 0 or len(data) > 1024:
        return False
    try:
        return parse_lfs_pointer(data.decode("utf-8")) is not None
    except UnicodeDecodeError:
        return False


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def lfs_object_path(common: Path, oid: str) -> Path:
    return common / "lfs" / "objects" / oid[:2] / oid[2:4] / oid


def load_ledger(root: Path) -> dict[str, Any]:
    path = root / LEDGER_REL
    if not path.is_file():
        raise Fail(f"missing corpus ledger: {LEDGER_REL}")
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise Fail(f"unreadable corpus ledger {LEDGER_REL}: {exc}") from exc
    expected = data.get("expected")
    if not isinstance(expected, dict):
        raise Fail(f"{LEDGER_REL}: missing expected counts object")
    required = (
        "tracked_pointer_files",
        "unique_payload_oids",
        "missing_objects",
        "corrupt_objects",
        "resolved_payload_mismatches",
    )
    for key in required:
        if key not in expected or not isinstance(expected[key], int):
            raise Fail(f"{LEDGER_REL}: expected.{key} must be an int")
    return data


def load_index(root: Path) -> list[tuple[str, str, int]]:
    """Return (path, oid, size) for every LFS-tracked path from the index pointer."""
    out = git(root, "lfs", "ls-files", "--long")
    entries: list[tuple[str, str, int]] = []
    for line in out.splitlines():
        line = line.strip()
        if not line:
            continue
        fields = line.split()
        if len(fields) < 3:
            continue
        oid = fields[0].lower()
        if len(oid) != 64:
            continue
        # path is everything after the status marker (* or -)
        marker = fields[1]
        path_start = line.find(marker)
        if path_start < 0:
            continue
        rel = line[path_start + len(marker) :].strip()
        if not rel:
            continue
        try:
            ptr = git(root, "cat-file", "blob", f":{rel}")
        except Fail as exc:
            raise Fail(f"LFS index pointer for {rel}: {exc}") from exc
        parsed = parse_lfs_pointer(ptr)
        if parsed is None:
            raise Fail(f"LFS path {rel}: index blob is not a valid pointer")
        ptr_oid, size = parsed
        if ptr_oid != oid:
            raise Fail(f"LFS path {rel}: ls-files oid {oid} != pointer oid {ptr_oid}")
        entries.append((rel, oid, size))
    return entries


def run_fsck_supplementary(root: Path) -> dict[str, Any]:
    proc = subprocess.run(
        ["git", "-C", str(root), "lfs", "fsck"],
        capture_output=True,
        text=True,
    )
    return {
        "exit_code": proc.returncode,
        "stdout": (proc.stdout or "").strip(),
        "stderr": (proc.stderr or "").strip(),
        "ok": proc.returncode == 0,
        "note": "supplementary only — not authority for this gate",
    }


def verify_corpus(root: Path) -> dict[str, Any]:
    ledger = load_ledger(root)
    expected = ledger["expected"]
    common = git_common_dir(root)
    entries = load_index(root)

    by_oid: dict[str, list[str]] = defaultdict(list)
    for rel, oid, _size in entries:
        by_oid[oid].append(rel)

    missing: list[dict[str, str]] = []
    corrupt: list[dict[str, str]] = []
    mismatches: list[dict[str, str]] = []
    verified_oids: list[str] = []

    # Unique-oid object-store integrity.
    for oid, paths in sorted(by_oid.items()):
        obj = lfs_object_path(common, oid)
        if not obj.is_file():
            missing.append({"oid": oid, "paths": ", ".join(paths), "object": str(obj)})
            continue
        got = sha256_file(obj)
        if got != oid:
            corrupt.append(
                {
                    "oid": oid,
                    "got_sha256": got,
                    "paths": ", ".join(paths),
                    "object": str(obj),
                }
            )
            continue
        # Size check against any one pointer (all aliases share size).
        size = next(s for r, o, s in entries if o == oid)
        actual_size = obj.stat().st_size
        if actual_size != size:
            corrupt.append(
                {
                    "oid": oid,
                    "got_sha256": got,
                    "paths": ", ".join(paths),
                    "object": str(obj),
                    "size_error": f"size {actual_size} != pointer size {size}",
                }
            )
            continue
        verified_oids.append(oid)

    # Worktree hydrated mismatch: on-disk non-pointer bytes that disagree with oid.
    for rel, oid, size in entries:
        abs_path = root / rel
        if not abs_path.is_file():
            continue
        try:
            raw = abs_path.read_bytes()
        except OSError:
            continue
        if is_pointer_bytes(raw):
            continue
        got = sha256_bytes(raw)
        if got != oid:
            mismatches.append(
                {
                    "path": rel,
                    "oid": oid,
                    "got_sha256": got,
                    "note": "hydrated worktree bytes disagree with index pointer oid",
                }
            )
        elif len(raw) != size:
            mismatches.append(
                {
                    "path": rel,
                    "oid": oid,
                    "got_sha256": got,
                    "note": f"hydrated size {len(raw)} != pointer size {size}",
                }
            )

    aliases = {
        oid: paths
        for oid, paths in sorted(by_oid.items())
        if len(paths) > 1
    }
    duplicate_pointer_count = sum(len(p) - 1 for p in aliases.values())

    observed = {
        "tracked_pointer_files": len(entries),
        "unique_payload_oids": len(by_oid),
        "missing_objects": len(missing),
        "corrupt_objects": len(corrupt),
        "resolved_payload_mismatches": len(mismatches),
    }

    failures: list[str] = []

    def assert_count(key: str) -> None:
        got = observed[key]
        want = expected[key]
        if got != want:
            if key == "tracked_pointer_files":
                failures.append(
                    f"{got} pointers, expected {want} — update the ledger "
                    f"({LEDGER_REL} expected.tracked_pointer_files)"
                )
            elif key == "unique_payload_oids":
                failures.append(
                    f"{got} unique payload OIDs, expected {want} — update the ledger "
                    f"({LEDGER_REL} expected.unique_payload_oids)"
                )
            else:
                failures.append(f"{key}: observed {got}, expected {want}")

    for key in (
        "tracked_pointer_files",
        "unique_payload_oids",
        "missing_objects",
        "corrupt_objects",
        "resolved_payload_mismatches",
    ):
        assert_count(key)

    for m in missing:
        failures.append(f"missing object oid={m['oid']} paths=[{m['paths']}]")
    for c in corrupt:
        detail = c.get("size_error", f"sha256={c['got_sha256']}")
        failures.append(f"corrupt object oid={c['oid']} {detail} paths=[{c['paths']}]")
    for m in mismatches:
        failures.append(
            f"resolved-payload mismatch path={m['path']} oid={m['oid']} "
            f"got={m['got_sha256']}: {m['note']}"
        )

    fsck = run_fsck_supplementary(root)

    result = {
        "kind": "lfs_corpus_verification",
        "root": str(root),
        "ledger": LEDGER_REL,
        "observed": observed,
        "expected": expected,
        "ok": len(failures) == 0,
        "failures": failures,
        "dedup": {
            "duplicate_pointer_count": duplicate_pointer_count,
            "shared_oid_count": len(aliases),
            "aliases": {oid: paths for oid, paths in aliases.items()},
        },
        "verified_unique_oids": len(verified_oids),
        "missing": missing,
        "corrupt": corrupt,
        "mismatches": mismatches,
        "git_lfs_fsck_supplementary": fsck,
    }
    return result


def format_human(result: dict[str, Any]) -> str:
    lines: list[str] = []
    obs = result["observed"]
    exp = result["expected"]
    lines.append("lfs-corpus: independent content-integrity verification")
    lines.append(
        f"  tracked LFS pointer files      == {obs['tracked_pointer_files']}"
        f"  (expected {exp['tracked_pointer_files']})"
    )
    lines.append(
        f"  unique payload OIDs            == {obs['unique_payload_oids']}"
        f"  (expected {exp['unique_payload_oids']})"
    )
    lines.append(
        f"  missing objects                == {obs['missing_objects']}"
        f"  (expected {exp['missing_objects']})"
    )
    lines.append(
        f"  corrupt objects                == {obs['corrupt_objects']}"
        f"  (expected {exp['corrupt_objects']})"
    )
    lines.append(
        f"  resolved-payload mismatches    == {obs['resolved_payload_mismatches']}"
        f"  (expected {exp['resolved_payload_mismatches']})"
    )
    dedup = result["dedup"]
    lines.append(
        f"  content-addressed aliases      == {dedup['duplicate_pointer_count']} "
        f"extra pointers across {dedup['shared_oid_count']} shared oids"
    )
    for oid, paths in dedup["aliases"].items():
        lines.append(f"    oid {oid}  n={len(paths)}")
        for p in paths:
            lines.append(f"      {p}")
    fsck = result["git_lfs_fsck_supplementary"]
    status = "OK" if fsck["ok"] else f"FAIL exit={fsck['exit_code']}"
    lines.append(f"  git lfs fsck (supplementary)  → {status}")
    if fsck["stdout"]:
        for line in fsck["stdout"].splitlines()[:5]:
            lines.append(f"    {line}")
    if result["ok"]:
        lines.append("lfs-corpus: OK — all counts match ledger; every payload sha256==oid")
    else:
        lines.append(f"lfs-corpus: FAIL ({len(result['failures'])} finding(s))")
        for f in result["failures"]:
            lines.append(f"  - {f}")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--root",
        type=Path,
        default=None,
        help="repository root (default: parent of scripts/)",
    )
    ap.add_argument("--json", action="store_true", help="emit machine-readable JSON")
    args = ap.parse_args(argv)

    if args.root is not None:
        root = args.root.resolve()
    else:
        root = Path(__file__).resolve().parents[1]

    try:
        top = git(root, "rev-parse", "--show-toplevel").strip()
        root = Path(top)
    except Fail as exc:
        print(f"lfs-corpus: FAIL not a git repository: {exc}", file=sys.stderr)
        return 2

    try:
        result = verify_corpus(root)
    except Fail as exc:
        print(f"lfs-corpus: FAIL {exc}", file=sys.stderr)
        return 1

    if args.json:
        print(json.dumps(result, indent=2, sort_keys=True))
    else:
        print(format_human(result))

    return 0 if result["ok"] else 1


if __name__ == "__main__":
    # Avoid accidental mutation of caller environment.
    os.environ.setdefault("GIT_LFS_SKIP_SMUDGE", "1")
    sys.exit(main())
