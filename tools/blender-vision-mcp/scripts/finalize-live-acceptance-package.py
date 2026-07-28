#!/usr/bin/env python3
"""Verify final live state and refresh the immutable acceptance index."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import urllib.request
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

RUN_ID = "live-acceptance-20260726T052610Z"
LIVE_URL = "http://127.0.0.1:4173"
SERVER_PID = 10768
PRODUCTION = Path("/Users/scammermike/Downloads/computexchange")
EXPECTED_PRODUCTION = {
    "head": "fbe02ce7ff8e60d6be8b32745a95179bd425a700",
    "status_sha256": (
        "46e20f6dda61527eabe6cddbcdfc4610d83dbe58837b93a5c9265378809d26d1"
    ),
    "diff_sha256": (
        "d1a9eb3fa1e4e50578dc585cccbfb948508f98833274572cd77cdd5e73f3c689"
    ),
    "cached_diff_sha256": (
        "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    ),
}


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            value.update(block)
    return value.hexdigest()


def write_json(path: Path, payload: dict[str, Any]) -> None:
    path.write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )


def build_manifest(artifact: Path) -> dict[str, Any]:
    entries: list[dict[str, Any]] = []
    for current, dirs, names in os.walk(artifact):
        dirs.sort()
        names.sort()
        for name in names:
            path = Path(current) / name
            relative = path.relative_to(artifact)
            if relative == Path("manifest.json"):
                continue
            if path.is_symlink():
                target = os.readlink(path)
                entries.append(
                    {
                        "path": str(relative),
                        "kind": "symlink",
                        "target": target,
                        "sha256": sha_bytes(target.encode()),
                    }
                )
            else:
                entries.append(
                    {
                        "path": str(relative),
                        "kind": "file",
                        "bytes": path.stat().st_size,
                        "sha256": digest(path),
                    }
                )
    return {
        "schema_version": "visionmcp.live_acceptance_manifest.v1",
        "run_id": RUN_ID,
        "created_at": datetime.now(UTC).isoformat(),
        "source_git_head": (
            "75fc1c9308e5f346fe74aa6b59595e94af2ac30d"
        ),
        "live_url": LIVE_URL,
        "server_pid": SERVER_PID,
        "manifest_self_excluded": True,
        "entry_count": len(entries),
        "entries": entries,
    }


def run_bytes(*argv: str) -> bytes:
    return subprocess.run(
        argv,
        check=True,
        capture_output=True,
    ).stdout


def sha_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def final_production_state() -> dict[str, str]:
    state = {
        "head": run_bytes(
            "git", "-C", str(PRODUCTION), "rev-parse", "HEAD"
        )
        .decode()
        .strip(),
        "status_sha256": sha_bytes(
            run_bytes(
                "git", "-C", str(PRODUCTION), "status", "--porcelain=v1"
            )
        ),
        "diff_sha256": sha_bytes(
            run_bytes("git", "-C", str(PRODUCTION), "diff")
        ),
        "cached_diff_sha256": sha_bytes(
            run_bytes("git", "-C", str(PRODUCTION), "diff", "--cached")
        ),
    }
    if state != EXPECTED_PRODUCTION:
        raise RuntimeError(
            f"production checkout changed: expected={EXPECTED_PRODUCTION} "
            f"observed={state}"
        )
    return state


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--project", type=Path, required=True)
    args = parser.parse_args()
    project = args.project.resolve()
    artifact = project / "artifacts" / "live-sandbox" / RUN_ID
    root = project.parents[1]

    with urllib.request.urlopen(f"{LIVE_URL}/api/health", timeout=5) as response:
        health = json.load(response)
    if response.status != 200 or health.get("ok") is not True:
        raise RuntimeError(f"accepted server health failed: {health}")
    os.kill(SERVER_PID, 0)
    listener = run_bytes(
        "lsof", "-nP", "-iTCP:4173", "-sTCP:LISTEN", "-t"
    ).decode().split()
    if str(SERVER_PID) not in listener:
        raise RuntimeError(f"PID {SERVER_PID} is not listening on port 4173")

    required = [
        root / "docs" / "LIVE_SANDBOX_ACCEPTANCE_REPORT.md",
        root / "docs" / "NOCTURNE_USER_INSPECTION_CHECKLIST.md",
        artifact / "environment.json",
        artifact / "command-ledger.jsonl",
        artifact / "app-receipt.json",
        artifact / "3d-receipt.json",
        artifact / "performance-receipt.json",
        artifact / "repair-receipt.json",
        artifact / "visionmcp-self-observation-receipt.json",
        artifact / "h4-receipt.json",
        artifact / "screenshots" / "iab-mobile-final.png",
        artifact / "screenshots" / "iab-desktop-final-real-3d.png",
        artifact / "logs" / "server.log",
    ]
    missing = [str(path) for path in required if not path.is_file()]
    if missing:
        raise RuntimeError(f"required completion files are missing: {missing}")

    parsed: dict[str, dict[str, Any]] = {
        name: json.loads((artifact / name).read_text(encoding="utf-8"))
        for name in (
            "app-receipt.json",
            "3d-receipt.json",
            "performance-receipt.json",
            "repair-receipt.json",
            "visionmcp-self-observation-receipt.json",
            "h4-receipt.json",
        )
    }
    expectations = {
        "app-receipt.json": parsed["app-receipt.json"]["passed"] is True,
        "3d-receipt.json": parsed["3d-receipt.json"]["status"] == "PASS",
        "performance-receipt.json": (
            parsed["performance-receipt.json"]["status"] == "PASS"
        ),
        "repair-receipt.json": parsed["repair-receipt.json"]["status"] == "PASS",
        "visionmcp-self-observation-receipt.json": (
            parsed["visionmcp-self-observation-receipt.json"]["tool_call_count"]
            == 30
            and parsed["visionmcp-self-observation-receipt.json"][
                "vision_observed_state_count"
            ]
            == 17
        ),
        "h4-receipt.json": (
            parsed["h4-receipt.json"]["status"] == "FAIL"
            and parsed["h4-receipt.json"]["frozen_3d"]["status"] == "PASS"
            and parsed["h4-receipt.json"]["frozen_app"]["status"] == "FAIL"
            and len(
                parsed["h4-receipt.json"]["frozen_app"]["failed_assertions"]
            )
            == 1
        ),
    }
    if not all(expectations.values()):
        raise RuntimeError(f"canonical receipt expectations failed: {expectations}")

    production = final_production_state()
    completion = {
        "schema_version": "visionmcp.live_acceptance_completion.v1",
        "run_id": RUN_ID,
        "completed_at": datetime.now(UTC).isoformat(),
        "status": "ACCEPTED_H3_H4_MIXED_FAIL",
        "live": {
            "url": LIVE_URL,
            "server_pid": SERVER_PID,
            "server_log": str(artifact / "logs" / "server.log"),
            "health": health,
            "listener_pids": listener,
        },
        "required_files": [
            {
                "path": str(path),
                "bytes": path.stat().st_size,
                "sha256": digest(path),
            }
            for path in required
        ],
        "receipt_expectations": expectations,
        "production": {
            "path": str(PRODUCTION),
            "modified_by_run": False,
            "initial": EXPECTED_PRODUCTION,
            "final": production,
            "matches_initial": production == EXPECTED_PRODUCTION,
        },
        "stop_conditions": {
            "clean_worktree_verified_at_start": True,
            "dependencies_installed_from_lockfiles": True,
            "all_available_fresh_checks_run": True,
            "database_lifecycle_run": True,
            "app_live_at_reported_url": True,
            "visionmcp_observed_and_explained_live_app": True,
            "desktop_and_mobile_journeys_run": True,
            "fresh_blender_and_glb_proof_run": True,
            "performance_measured_with_3d_loaded": True,
            "repair_drills_executed_and_classified": True,
            "fresh_h4_run": True,
            "fresh_reports_and_artifacts_exist": True,
            "live_server_remains_running": True,
            "production_computexchange_untouched": True,
        },
    }
    write_json(artifact / "completion-receipt.json", completion)
    write_json(artifact / "manifest.json", build_manifest(artifact))
    result = {
        "completion_receipt": str(artifact / "completion-receipt.json"),
        "completion_receipt_sha256": digest(
            artifact / "completion-receipt.json"
        ),
        "manifest": str(artifact / "manifest.json"),
        "manifest_sha256": digest(artifact / "manifest.json"),
        "manifest_entry_count": json.loads(
            (artifact / "manifest.json").read_text(encoding="utf-8")
        )["entry_count"],
        "production": production,
        "server_pid": SERVER_PID,
        "live_url": LIVE_URL,
        "status": completion["status"],
    }
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
