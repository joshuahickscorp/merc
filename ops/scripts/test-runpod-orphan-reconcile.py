#!/usr/bin/env python3
"""Offline test: a SIGKILLed run's pending pod is detected as an orphan.

Simulates the failure mode that cost real money:

  1. A process writes/binds a create intent for pod lnk2yta98ciwqv.
  2. The process is SIGKILLed during startup — trap never runs, no receipt.
  3. The pod remains RUNNING with nobody watching.

This test never calls RunPod and never creates a pod. It feeds the reconcile
classifier a fixture live-pod list and a bound intent with no completion.

Expected: has_orphans is True and exit code of the CLI reconcile is 1.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
GUARD = os.path.join(ROOT, "ops/scripts", "runpod-spend-guard.py")

import importlib.util

spec = importlib.util.spec_from_file_location("runpod_spend_guard", GUARD)
guard = importlib.util.module_from_spec(spec)
assert spec.loader is not None
# dataclasses looks up the module in sys.modules while the class body runs.
sys.modules[spec.name] = guard
spec.loader.exec_module(guard)

KILLED_POD = "lnk2yta98ciwqv"


def test_classify_killed_run_is_orphan() -> None:
    live = [
        {
            "id": KILLED_POD,
            "name": "merc-canary-vllm",
            "desiredStatus": "RUNNING",
            "costPerHr": 0.44,
        }
    ]
    intents = [
        {
            "schema_version": 1,
            "kind": guard.INTENT_KIND,
            "request_id": "req-sigkill-sampler",
            "pod_id": KILLED_POD,
            "purpose": "board_power_remeasure",
            "status": "bound",
            "created_at_unix": 1,
            "pod_bound_at_unix": 2,
            "completed_at_unix": None,
        }
    ]
    report = guard.classify_live_pods(
        live, intents=intents, active_pod_id=None, completed_by_id={}
    )
    assert report.has_orphans, "killed run must surface as orphan"
    assert report.orphans[0].pod_id == KILLED_POD
    assert report.orphans[0].classification == "abandoned_intent"
    print("PASS: classify_live_pods detects SIGKILLed pending pod as orphan")


def test_cli_reconcile_exits_nonzero() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        receipts_dir = os.path.join(tmp, "receipts")
        os.makedirs(intent_dir)
        os.makedirs(receipts_dir)
        guard.write_intent(
            intent_dir=intent_dir,
            request_id="req-cli",
            purpose="board_power_remeasure",
            gpu="NVIDIA A40",
            name="merc-canary-vllm",
            now=10,
        )
        guard.bind_intent(
            intent_dir=intent_dir,
            request_id="req-cli",
            pod_id=KILLED_POD,
            now=11,
        )
        live = json.dumps(
            [
                {
                    "id": KILLED_POD,
                    "name": "merc-canary-vllm",
                    "desiredStatus": "RUNNING",
                    "costPerHr": 0.44,
                }
            ]
        )
        proc = subprocess.run(
            [
                sys.executable,
                GUARD,
                "reconcile",
                "--live-pods-json",
                live,
                "--intent-dir",
                intent_dir,
                "--receipts-dir",
                receipts_dir,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        assert proc.returncode == 1, (
            f"reconcile must exit 1 on orphan, got {proc.returncode}\n"
            f"stdout:\n{proc.stdout}\nstderr:\n{proc.stderr}"
        )
        assert "ORPHAN" in proc.stdout or "orphan" in proc.stdout.lower()
        assert KILLED_POD in proc.stdout
        print("PASS: CLI reconcile exits 1 for killed-run fixture")


def test_clean_account_exits_zero() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        receipts_dir = os.path.join(tmp, "receipts")
        os.makedirs(intent_dir)
        os.makedirs(receipts_dir)
        proc = subprocess.run(
            [
                sys.executable,
                GUARD,
                "reconcile",
                "--live-pods-json",
                "[]",
                "--intent-dir",
                intent_dir,
                "--receipts-dir",
                receipts_dir,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        assert proc.returncode == 0, proc.stdout + proc.stderr
        print("PASS: CLI reconcile exits 0 for empty live list")


def test_deliberate_keep_is_not_orphan() -> None:
    live = [{"id": KILLED_POD, "name": "merc-canary-vllm", "desiredStatus": "RUNNING"}]
    report = guard.classify_live_pods(
        live, intents=[], active_pod_id=KILLED_POD, completed_by_id={}
    )
    assert not report.has_orphans
    assert report.owned[0].classification == "active_owner"
    print("PASS: deliberate --keep (active env) is not an orphan")


def main() -> int:
    test_classify_killed_run_is_orphan()
    test_cli_reconcile_exits_nonzero()
    test_clean_account_exits_zero()
    test_deliberate_keep_is_not_orphan()
    print("test-runpod-orphan-reconcile: PASS")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
