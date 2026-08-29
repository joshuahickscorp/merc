#!/usr/bin/env python3
"""Scan secret locations without ever printing or persisting matched values."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
PATTERN = re.compile(rb"(?:sk|rk)_(?:live|test)_[A-Za-z0-9]{16,}|whsec_[A-Za-z0-9]{16,}")


def run(*args: str) -> bytes:
    return subprocess.run(args, cwd=ROOT, check=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL).stdout


def classify(data: bytes) -> tuple[int, int]:
    live = test = 0
    for match in PATTERN.finditer(data):
        prefix = match.group(0).split(b"_", 2)[:2]
        if b"live" in prefix:
            live += 1
        else:
            test += 1
    return live, test


def scan_paths(paths: list[Path]) -> tuple[int, int, int]:
    live = test = files = 0
    for path in paths:
        try:
            data = path.read_bytes()
        except OSError:
            continue
        found_live, found_test = classify(data)
        if found_live or found_test:
            files += 1
            live += found_live
            test += found_test
    return live, test, files


def rg_matching_files(root: Path, pattern: str) -> set[bytes]:
    if not root.exists():
        return set()
    result = subprocess.run(
        ["rg", "--files-with-matches", "--text", "--regexp", pattern, str(root)],
        cwd=ROOT, check=False, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    )
    if result.returncode not in (0, 1):
        raise subprocess.CalledProcessError(result.returncode, result.args)
    return set(result.stdout.splitlines())


def scan_artifacts(root: Path) -> tuple[int, int, int]:
    live = rg_matching_files(root, r"(?:sk|rk)_live_[A-Za-z0-9]{16,}")
    test = rg_matching_files(root, r"(?:sk|rk)_test_[A-Za-z0-9]{16,}|whsec_[A-Za-z0-9]{16,}")
    return len(live), len(test), len(live | test)


def scan_git_history() -> tuple[int, int, int]:
    objects: dict[bytes, bytes] = {}
    for line in run("git", "rev-list", "--objects", "--all").splitlines():
        oid, _, path = line.partition(b" ")
        if path:
            objects.setdefault(oid, path)
    if not objects:
        return 0, 0, 0
    oids = b"\n".join(objects) + b"\n"
    metadata = subprocess.run(
        ["git", "cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)"],
        cwd=ROOT, check=True, input=oids, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    ).stdout.splitlines()
    candidates: list[bytes] = []
    binary_suffixes = {b".png", b".jpg", b".jpeg", b".gif", b".ico", b".ttf", b".woff", b".woff2", b".pdf", b".zip", b".tar", b".gz", b".bin"}
    for line in metadata:
        oid, kind, raw_size = line.split()
        path = objects.get(oid, b"").lower()
        if kind == b"blob" and int(raw_size) <= 10 * 1024 * 1024 and not any(path.endswith(suffix) for suffix in binary_suffixes):
            candidates.append(oid)
    batch = subprocess.run(
        ["git", "cat-file", "--batch"], cwd=ROOT, check=True,
        input=b"\n".join(candidates) + b"\n", stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
    ).stdout
    live = test = files = 0
    cursor = 0
    for _ in candidates:
        header_end = batch.index(b"\n", cursor)
        _, kind, raw_size = batch[cursor:header_end].split()
        size = int(raw_size)
        start = header_end + 1
        data = batch[start:start + size]
        cursor = start + size + 1
        if kind != b"blob":
            continue
        found_live, found_test = classify(data)
        if found_live or found_test:
            files += 1
            live += found_live
            test += found_test
    return live, test, files


def record(category: str, tracked: str, live: int, test: int, files: int) -> dict:
    return {
        "location_category": category,
        "tracked": tracked,
        "classification": "live" if live else ("test" if test else "missing"),
        "exposure_detected": bool(live or test),
        "rotation_recommended": bool(live),
        "files_with_secret_shaped_values": files,
        "live_value_count": live,
        "test_value_count": test,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--ci", action="store_true", help="scan accessible recent GitHub Actions logs")
    args = parser.parse_args()

    tracked = [ROOT / item.decode() for item in run("git", "ls-files", "-z").split(b"\0") if item]
    untracked = [ROOT / item.decode() for item in run("git", "ls-files", "--others", "--exclude-standard", "-z").split(b"\0") if item]
    artifact_root = ROOT / ".artifacts"
    rows = [record("working_tree", "tracked", *scan_paths(tracked)),
            record("working_tree", "untracked", *scan_paths(untracked)),
            record("generated_artifacts", "ignored", *scan_artifacts(artifact_root))]
    local_live, local_test, local_files = scan_paths([ROOT / ".env", ROOT / ".env.go-closure", ROOT / ".env.local"])
    rows.append({"location_category":"local_secret_files","tracked":"ignored",
                 "classification":"live" if local_live else ("test" if local_test else "missing"),
                 "exposure_detected":False,"rotation_recommended":False,
                 "files_with_secret_shaped_values":local_files,
                 "live_value_count":local_live,"test_value_count":local_test})

    history_live = set(run("git", "log", "--all", "--format=", "--name-only", "-G(sk|rk)_live_[A-Za-z0-9]{16,}").splitlines())
    history_test = set(run("git", "log", "--all", "--format=", "--name-only", "-G(sk|rk)_test_[A-Za-z0-9]{16,}|whsec_[A-Za-z0-9]{16,}").splitlines())
    history_live.discard(b""); history_test.discard(b"")
    rows.append(record("git_history", "tracked", len(history_live), len(history_test), len(history_live | history_test)))

    if args.ci:
        live = test = files = 0
        try:
            runs = json.loads(run("gh", "run", "list", "--limit", "20", "--json", "databaseId"))
            for item in runs:
                log = run("gh", "run", "view", str(item["databaseId"]), "--log")
                found_live, found_test = classify(log)
                if found_live or found_test:
                    files += 1
                    live += found_live
                    test += found_test
        except (subprocess.CalledProcessError, json.JSONDecodeError, KeyError):
            rows.append({"location_category":"accessible_ci_logs","tracked":"external","classification":"unknown","exposure_detected":False,"rotation_recommended":False,"scan_status":"unavailable"})
        else:
            rows.append(record("accessible_ci_logs", "external", live, test, files))

    env_classes = []
    for name in ("STRIPE_SECRET_KEY", "STRIPE_LIVE_SECRET_KEY", "STRIPE_RESTRICTED_KEY", "STRIPE_PUBLISHABLE_KEY", "NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY"):
        value = os.environ.get(name, "")
        if not value:
            classification = "missing"
        elif value.startswith(("sk_live_", "rk_live_", "pk_live_")):
            classification = "live"
        elif value.startswith(("sk_test_", "rk_test_", "pk_test_")):
            classification = "test"
        else:
            classification = "unknown"
        env_classes.append({"variable":name,"classification":classification})
    live_env = any(item["classification"] == "live" for item in env_classes)
    rows.append({"location_category":"process_environment","tracked":"untracked","classification":"live" if live_env else "missing","exposure_detected":False,"rotation_recommended":False,"variables":env_classes})

    exposed_live = any(row.get("exposure_detected") and row.get("classification") == "live" for row in rows)
    report = {"schema_version":1,"kind":"stripe_secret_exposure_audit","secret_values_printed":False,
              "live_key_exists":"possible" if live_env or local_live else "unknown","live_key_exposure":"detected" if exposed_live else "not detected",
              "rotation_required_now":exposed_live,"live_key_use_during_closure":"prohibited","locations":rows}
    print(json.dumps(report, sort_keys=True, separators=(",", ":")))
    return 1 if exposed_live else 0


if __name__ == "__main__":
    raise SystemExit(main())
