#!/usr/bin/env python3
"""Run one live-acceptance command and append a hash-bound ledger entry."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
import time
from datetime import UTC, datetime
from pathlib import Path


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--ledger", type=Path, required=True)
    parser.add_argument("--log-dir", type=Path, required=True)
    parser.add_argument("--id", required=True)
    parser.add_argument("--cwd", type=Path, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command
    if command and command[0] == "--":
        command = command[1:]
    if not command:
        parser.error("a command is required after --")

    args.ledger.parent.mkdir(parents=True, exist_ok=True)
    args.log_dir.mkdir(parents=True, exist_ok=True)
    stdout_path = args.log_dir / f"{args.id}.stdout.log"
    stderr_path = args.log_dir / f"{args.id}.stderr.log"
    started = datetime.now(UTC)
    started_monotonic = time.monotonic()
    result = subprocess.run(
        command,
        cwd=args.cwd,
        env=os.environ.copy(),
        stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    elapsed = time.monotonic() - started_monotonic
    stdout_path.write_bytes(result.stdout)
    stderr_path.write_bytes(result.stderr)
    sys.stdout.buffer.write(result.stdout)
    sys.stderr.buffer.write(result.stderr)

    entry = {
        "schema_version": "visionmcp.live_acceptance_command.v1",
        "id": args.id,
        "started_at": started.isoformat().replace("+00:00", "Z"),
        "ended_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "elapsed_seconds": round(elapsed, 6),
        "cwd": str(args.cwd.resolve()),
        "command": command,
        "exit_status": result.returncode,
        "stdout": {
            "path": str(stdout_path.resolve()),
            "bytes": stdout_path.stat().st_size,
            "sha256": sha256(stdout_path),
        },
        "stderr": {
            "path": str(stderr_path.resolve()),
            "bytes": stderr_path.stat().st_size,
            "sha256": sha256(stderr_path),
        },
    }
    with args.ledger.open("a", encoding="utf-8") as handle:
        handle.write(json.dumps(entry, sort_keys=True, separators=(",", ":")) + "\n")
        handle.flush()
        os.fsync(handle.fileno())
    print(json.dumps(entry, indent=2), file=sys.stderr)
    return result.returncode


if __name__ == "__main__":
    raise SystemExit(main())
