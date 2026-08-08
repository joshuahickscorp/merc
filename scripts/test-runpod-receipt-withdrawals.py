#!/usr/bin/env python3
"""Hostile tests for append-only RunPod failed-startup receipt withdrawals.

No credential, provider, or repository evidence is touched.  The temporary Git
repository models the one property that matters: a withdrawal may retire an
inadmissible historical receipt, but it cannot follow a rewritten target.
"""

from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import os
import subprocess
import sys
import tempfile

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUARD = os.path.join(ROOT, "scripts", "runpod-spend-guard.py")

spec = importlib.util.spec_from_file_location("runpod_spend_guard", GUARD)
guard = importlib.util.module_from_spec(spec)
assert spec.loader is not None
sys.modules[spec.name] = guard
spec.loader.exec_module(guard)

TARGET_REL = "evidence/runpod/spend-failed.json"
REQUIRED_REFUSAL = (
    "vLLM never reached a verified ready state, so this is a failed startup "
    "receipt rather than usable CUDA-runtime evidence"
)


def run(*args: str, cwd: str) -> str:
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


def write_json(path: str, value: dict) -> None:
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(value, handle, indent=2, sort_keys=True)
        handle.write("\n")


def sha256(path: str) -> str:
    with open(path, "rb") as handle:
        return hashlib.sha256(handle.read()).hexdigest()


def failed_receipt() -> dict:
    return {
        "schema_version": 1,
        "kind": "runpod_spend_receipt",
        "binding_status": "BOUND",
        "admissible": False,
        "ready": False,
        "refusals": [REQUIRED_REFUSAL],
        "image": "vllm/vllm-openai@sha256:" + "a" * 64,
        "cost_per_hr_usd": 1.0,
        "cap_usd": 1.0,
        "lifetime_actual_secs": 60,
        "spend_usd": 0.02,
        "teardown_verified": True,
        "orphan_pods": [],
    }


def envelope(commit: str, target_sha: str) -> dict:
    return {
        "schema_version": 1,
        "kind": "runpod_spend_receipt_withdrawal",
        "binding_status": "WITHDRAWN",
        "status": "WITHDRAWN",
        "append_only": True,
        "target_path": TARGET_REL,
        "target_kind": "runpod_spend_receipt",
        "target_binding_status": "BOUND",
        "target_admissible": False,
        "target_ready": False,
        "target_sha256": target_sha,
        "historical_source_commit": commit,
        "required_refusal": REQUIRED_REFUSAL,
        "withdrawn_reason": ["Failed startup is retained history, never runtime evidence."],
    }


def assert_refused(path: str, receipt: dict, label: str) -> None:
    reason, error = guard.external_receipt_withdrawal(path, receipt)
    assert reason is None and error, f"{label}: expected refusal, got reason={reason!r} error={error!r}"


def main() -> int:
    original_root = guard.ROOT
    try:
        with tempfile.TemporaryDirectory(prefix="merc-runpod-withdrawal-") as tmp:
            evidence_dir = os.path.join(tmp, "evidence", "runpod")
            os.makedirs(evidence_dir)
            target = os.path.join(tmp, TARGET_REL)
            receipt = failed_receipt()
            write_json(target, receipt)
            run("git", "init", "-q", cwd=tmp)
            run("git", "add", TARGET_REL, cwd=tmp)
            run(
                "git",
                "-c",
                "user.name=Withdrawal Test",
                "-c",
                "user.email=withdrawal-test@example.invalid",
                "commit",
                "-qm",
                "retain failed receipt",
                cwd=tmp,
            )
            source_commit = run("git", "rev-parse", "HEAD", cwd=tmp)
            target_sha = sha256(target)
            sidecar = target[:-5] + ".withdrawal.json"
            valid = envelope(source_commit, target_sha)
            write_json(sidecar, valid)
            guard.ROOT = tmp

            reason, error = guard.external_receipt_withdrawal(target, receipt)
            assert error is None and reason, f"valid historical withdrawal rejected: {error}"
            assert guard.revalidate_retained_receipts() == 0

            forged_path = copy.deepcopy(valid)
            forged_path["target_path"] = "evidence/runpod/other.json"
            write_json(sidecar, forged_path)
            assert_refused(target, receipt, "forged target path")

            forged_hash = copy.deepcopy(valid)
            forged_hash["target_sha256"] = "0" * 64
            write_json(sidecar, forged_hash)
            assert_refused(target, receipt, "forged target hash")

            # A coordinated current-worktree target/sidecar rewrite still loses to
            # the historical commit pin.
            rewritten = copy.deepcopy(receipt)
            rewritten["refusals"] = [REQUIRED_REFUSAL, "tampered after commit"]
            write_json(target, rewritten)
            coordinated = copy.deepcopy(valid)
            coordinated["target_sha256"] = sha256(target)
            write_json(sidecar, coordinated)
            assert_refused(target, rewritten, "coordinated current target rewrite")

            # A valid receipt cannot be hidden behind this narrow failed-startup
            # withdrawal form.
            write_json(target, receipt)
            valid_receipt = copy.deepcopy(receipt)
            valid_receipt["admissible"] = True
            valid_receipt["ready"] = True
            write_json(sidecar, valid)
            assert_refused(target, valid_receipt, "attempted valid receipt withdrawal")
    finally:
        guard.ROOT = original_root
    print("test-runpod-receipt-withdrawals: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
