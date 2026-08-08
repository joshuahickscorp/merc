#!/usr/bin/env python3
"""Offline adversarial tests for RunPod lease-backed orphan reconciliation.

No test calls RunPod or reads credentials.  The fixtures model the dangerous
states around create/bind/readiness: only an exact schema-v2 lease, with a live
PID/start/boot identity and a fresh bounded heartbeat, is ownership proof.
"""

from __future__ import annotations

import copy
import importlib.util
import json
import os
import stat
import subprocess
import sys
import tempfile
import time

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
GUARD = os.path.join(ROOT, "scripts", "runpod-spend-guard.py")
VLLM_SCRIPT = os.path.join(ROOT, "scripts", "runpod-vllm.sh")
POD = "lnk2yta98ciwqv"
NOW = int(time.time())

spec = importlib.util.spec_from_file_location("runpod_spend_guard", GUARD)
guard = importlib.util.module_from_spec(spec)
assert spec.loader is not None
sys.modules[spec.name] = guard
spec.loader.exec_module(guard)


def live() -> list[dict]:
    return [{"id": POD, "name": "merc-canary-vllm", "desiredStatus": "RUNNING"}]


def owner(token: str = "a" * 48) -> dict:
    identity = guard.process_identity(os.getpid())
    return {
        "owner_token": token,
        "owner_pid": identity["pid"],
        "owner_process_start": identity["process_start"],
        "owner_boot_id": identity["boot_id"],
    }


def bound_record(*, active_lease: dict, token: str, schema_version: int = 2) -> dict:
    return {
        "schema_version": schema_version,
        "kind": guard.INTENT_KIND,
        "request_id": "req-lease-test",
        "pod_id": POD,
        "purpose": "experiment",
        "status": "bound",
        "created_at_unix": NOW - 2,
        "pod_bound_at_unix": NOW - 1,
        "completed_at_unix": None,
        "active_lease": active_lease,
        "lease_binding": {
            "request_id": "req-lease-test",
            "pod_id": POD,
            "token_sha256": guard._token_digest(token),
        },
        "terminal": None,
    }


def provisioning_record() -> dict:
    claim = owner()
    lease = guard._lease(
        owner={
            "token": claim["owner_token"],
            "owner_pid": claim["owner_pid"],
            "owner_process_start": claim["owner_process_start"],
            "owner_boot_id": claim["owner_boot_id"],
        },
        request_id="req-lease-test",
        pod_id=POD,
        purpose="experiment",
        state="provisioning",
        now=NOW,
        ttl_seconds=guard.DEFAULT_LEASE_TTL_SECONDS,
    )
    return bound_record(active_lease=lease, token=claim["owner_token"])


def classify(intent: dict, *, now: int = NOW) -> object:
    return guard.classify_live_pods(live(), intents=[intent], completed_by_id={}, now=now)


def assert_orphan(intent: dict, label: str, *, now: int = NOW) -> None:
    report = classify(intent, now=now)
    assert report.has_orphans, f"{label} must be an orphan"
    assert report.orphans[0].classification == "abandoned_intent", report.to_dict()


def test_fresh_matching_provisioning_lease_is_owned() -> None:
    report = classify(provisioning_record())
    assert not report.has_orphans, report.to_dict()
    assert report.owned[0].classification == "active_provisioning_lease"
    print("PASS: fresh PID/start/boot-bound provisioning lease is owned")


def test_expired_dead_reused_wrong_and_legacy_claims_are_orphans() -> None:
    expired = provisioning_record()
    expired["active_lease"]["heartbeat_at_unix"] = NOW - 91
    expired["active_lease"]["expires_at_unix"] = NOW - 1
    assert_orphan(expired, "expired heartbeat despite live owner")

    future = provisioning_record()
    future["active_lease"]["heartbeat_at_unix"] = NOW + 1
    future["active_lease"]["expires_at_unix"] = NOW + 1 + guard.DEFAULT_LEASE_TTL_SECONDS
    assert_orphan(future, "one-second future heartbeat")

    future_expiry = provisioning_record()
    future_expiry["active_lease"]["expires_at_unix"] = NOW + guard.DEFAULT_LEASE_TTL_SECONDS + 1
    assert_orphan(future_expiry, "one-second future expiry")

    dead = provisioning_record()
    dead["active_lease"]["owner_pid"] = 99999999
    assert_orphan(dead, "dead PID")

    reused = provisioning_record()
    reused["active_lease"]["owner_process_start"] = "0" * 64
    assert_orphan(reused, "PID reuse/start mismatch")

    wrong_pod = provisioning_record()
    wrong_pod["active_lease"]["pod_id"] = "different-pod"
    assert_orphan(wrong_pod, "wrong pod")

    wrong_token = provisioning_record()
    wrong_token["active_lease"]["token"] = "b" * 48
    assert_orphan(wrong_token, "wrong token")

    legacy = provisioning_record()
    legacy["schema_version"] = 1
    assert_orphan(legacy, "legacy schema")
    print("PASS: expired/future/dead/reused/wrong/legacy claims are refused")


def test_darwin_boot_identity_refuses_missing_or_malformed_sysctl() -> None:
    class Result:
        def __init__(self, returncode: int, stdout: str) -> None:
            self.returncode = returncode
            self.stdout = stdout

    original = guard.subprocess.run
    try:
        guard.subprocess.run = lambda *args, **kwargs: Result(1, "")
        try:
            guard._darwin_boot_identity()
        except ValueError:
            pass
        else:
            raise AssertionError("failed Darwin sysctl produced an ownership identity")
        guard.subprocess.run = lambda *args, **kwargs: Result(0, "not-a-kernel-boot-time")
        try:
            guard._darwin_boot_identity()
        except ValueError:
            pass
        else:
            raise AssertionError("malformed Darwin sysctl produced an ownership identity")
        guard.subprocess.run = lambda *args, **kwargs: Result(0, "{ sec = 123, usec = 456 } Wed")
        assert guard._darwin_boot_identity() == "123:456"
    finally:
        guard.subprocess.run = original
    print("PASS: Darwin boot identity refuses failed or malformed sysctl")


def test_pending_env_and_bound_without_lease_are_not_owners() -> None:
    killed = provisioning_record()
    killed["active_lease"] = None
    killed["lease_binding"] = None
    assert_orphan(killed, "bound killed run")
    # Passing the obsolete ready-env parameter cannot override the refusal.
    report = guard.classify_live_pods(
        live(), intents=[killed], active_pod_id=POD, completed_by_id={}, now=NOW
    )
    assert report.has_orphans
    timeout_after_create = {
        "schema_version": 2,
        "kind": guard.INTENT_KIND,
        "request_id": "req-timeout-after-create",
        "pod_id": None,
        "purpose": "experiment",
        "status": "requested",
        "active_lease": None,
        "lease_binding": None,
    }
    timeout_report = guard.classify_live_pods(
        live(), intents=[timeout_after_create], completed_by_id={}, now=NOW
    )
    assert timeout_report.has_orphans
    assert timeout_report.orphans[0].classification == "unknown"
    print("PASS: pending/ready env and create-timeout claims never mask an orphan")


def test_parent_handoff_and_operator_keep_expiry() -> None:
    record = provisioning_record()
    record["active_lease"]["purpose"] = "experiment"
    report = classify(record)
    assert not report.has_orphans
    assert report.owned[0].classification == "active_provisioning_lease"

    kept = copy.deepcopy(record)
    kept["active_lease"]["state"] = "operator_keep"
    kept["active_lease"]["expires_at_unix"] = NOW + 30
    report = classify(kept)
    assert not report.has_orphans and report.owned[0].classification == "operator_keep"
    assert_orphan(kept, "expired operator keep", now=NOW + 31)
    print("PASS: parent-owned handoff works and operator keep expires")


def test_duplicate_creator_and_terminal_tombstone() -> None:
    claim = owner()
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        guard.write_intent(
            intent_dir=intent_dir,
            request_id="req-first",
            purpose="experiment",
            now=NOW,
            **claim,
        )
        try:
            guard.write_intent(
                intent_dir=intent_dir,
                request_id="req-second",
                purpose="experiment",
                now=NOW + 1,
                **claim,
            )
        except ValueError as exc:
            assert "provisioning lease" in str(exc)
        else:
            raise AssertionError("second concurrent creator acquired the account lock")

        guard.bind_intent(
            intent_dir=intent_dir,
            request_id="req-first",
            pod_id=POD,
            now=NOW + 2,
            **claim,
        )
        path = guard._intent_path(intent_dir, "req-first")
        malformed = guard._read_json(path)
        assert malformed is not None
        malformed["lease_binding"] = None
        guard._atomic_write_json(path, malformed)
        try:
            guard.renew_intent_lease(
                intent_dir=intent_dir,
                request_id="req-first",
                now=NOW + 3,
                **claim,
            )
        except ValueError:
            pass
        else:
            raise AssertionError("malformed token binding advanced through renewal")
        # Restore a valid bind for the terminal-tombstone mutation test.
        malformed["lease_binding"] = {
            "request_id": "req-first",
            "pod_id": POD,
            "token_sha256": guard._token_digest(claim["owner_token"]),
        }
        guard._atomic_write_json(path, malformed)
        guard.complete_intent(
            intent_dir=intent_dir,
            request_id="req-first",
            owner_token=claim["owner_token"],
            now=NOW + 4,
        )
        terminal = guard.classify_live_pods(
            live(), intents=guard.load_intents(intent_dir), completed_by_id={}, now=NOW + 5
        )
        assert terminal.has_orphans
        assert terminal.orphans[0].classification == "terminal_intent_alive"
        try:
            guard.renew_intent_lease(
                intent_dir=intent_dir,
                request_id="req-first",
                now=NOW + 5,
                **claim,
            )
        except ValueError:
            pass
        else:
            raise AssertionError("old heartbeat revived a terminal intent")
    print("PASS: duplicate creator is refused and terminal lease cannot revive")


def test_legacy_requested_intent_cannot_renew_or_bind() -> None:
    claim = owner()
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        guard.write_intent(
            intent_dir=intent_dir,
            request_id="req-legacy-requested",
            purpose="experiment",
            now=NOW,
            **claim,
        )
        path = guard._intent_path(intent_dir, "req-legacy-requested")
        record = guard._read_json(path)
        assert record is not None
        record["schema_version"] = 1
        guard._atomic_write_json(path, record)
        for label, action in (
            (
                "renew",
                lambda: guard.renew_create_lease(
                    intent_dir=intent_dir, request_id="req-legacy-requested", now=NOW + 1, **claim
                ),
            ),
            (
                "bind",
                lambda: guard.bind_intent(
                    intent_dir=intent_dir,
                    request_id="req-legacy-requested",
                    pod_id=POD,
                    now=NOW + 1,
                    **claim,
                ),
            ),
        ):
            try:
                action()
            except ValueError as exc:
                assert "schema-v2" in str(exc), (label, exc)
            else:
                raise AssertionError(f"legacy requested intent was allowed to {label}")
    print("PASS: legacy requested intents cannot reach create or bind transitions")


def test_bound_lease_blocks_new_creator_after_create_lock_is_lost() -> None:
    claim = owner()
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        guard.write_intent(
            intent_dir=intent_dir,
            request_id="req-bound-owner",
            purpose="experiment",
            now=NOW,
            **claim,
        )
        guard.bind_intent(
            intent_dir=intent_dir,
            request_id="req-bound-owner",
            pod_id=POD,
            now=NOW + 1,
            **claim,
        )
        os.unlink(guard._lock_path(intent_dir))
        try:
            guard.write_intent(
                intent_dir=intent_dir,
                request_id="req-second-creator",
                purpose="experiment",
                now=NOW + 2,
                **owner(token="b" * 48),
            )
        except ValueError as exc:
            assert "bound intent" in str(exc), exc
        else:
            raise AssertionError("lost create lock hid a live bound provisioning lease")
    print("PASS: live bound lease blocks a new creator after lock loss")


def test_owner_token_fd_keeps_lease_capability_out_of_argv() -> None:
    claim = owner(token="f" * 48)
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        read_fd, write_fd = os.pipe()
        try:
            os.write(write_fd, claim["owner_token"].encode("utf-8"))
            os.close(write_fd)
            write_fd = -1
            command = [
                sys.executable,
                GUARD,
                "intent-write",
                "--request-id",
                "req-owner-fd",
                "--purpose",
                "offline-test",
                "--owner-token-fd",
                str(read_fd),
                "--owner-pid",
                str(claim["owner_pid"]),
                "--owner-process-start",
                claim["owner_process_start"],
                "--owner-boot-id",
                claim["owner_boot_id"],
                "--intent-dir",
                intent_dir,
            ]
            assert claim["owner_token"] not in " ".join(command)
            result = subprocess.run(
                command,
                capture_output=True,
                text=True,
                pass_fds=(read_fd,),
                check=False,
            )
        finally:
            os.close(read_fd)
            if write_fd >= 0:
                os.close(write_fd)
        assert result.returncode == 0, result.stderr
        record = guard._read_json(guard._intent_path(intent_dir, "req-owner-fd"))
        assert record and record["request_id"] == "req-owner-fd"
        assert stat.S_IMODE(os.stat(intent_dir).st_mode) == 0o700
        assert stat.S_IMODE(os.stat(os.path.dirname(intent_dir)).st_mode) == 0o700
    print("PASS: lease token is accepted from a private FD, not argv")


def test_owner_token_is_not_exported_from_private_state() -> None:
    """The shell contract must not silently regress to ambient token transport."""
    with open(VLLM_SCRIPT, encoding="utf-8") as handle:
        source = handle.read()
    assert 'OWNER_TOKEN="${MERC_RUNPOD_OWNER_TOKEN:-}"' not in source
    assert "MERC_RUNPOD_OWNER_TOKEN must not be supplied through the environment" in source
    assert "printf 'export MERC_RUNPOD_OWNER_TOKEN=%q\\n'" not in source
    assert "printf 'MERC_RUNPOD_OWNER_TOKEN=%q\\n'" in source
    assert "export -n MERC_RUNPOD_OWNER_TOKEN" in source
    print("PASS: owner token stays out of inherited environment state")


def test_parallel_process_creators_contend_for_one_lock() -> None:
    program = r'''
import importlib.util, os, sys, time
spec = importlib.util.spec_from_file_location("guard_child", sys.argv[1])
guard = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = guard
spec.loader.exec_module(guard)
identity = guard.process_identity(os.getpid())
intent_dir, request_id, token, barrier_dir = sys.argv[2:6]
ready = os.path.join(barrier_dir, "ready-" + request_id)
go = os.path.join(barrier_dir, "go")
owner = os.path.join(barrier_dir, "owner")
release = os.path.join(barrier_dir, "release")
open(ready, "x").close()
deadline = time.monotonic() + 5
while not os.path.exists(go):
    if time.monotonic() >= deadline:
        raise SystemExit(2)
    time.sleep(0.01)
try:
    guard.write_intent(
        intent_dir=intent_dir, request_id=request_id, purpose="parallel-test",
        owner_token=token, owner_pid=identity["pid"],
        owner_process_start=identity["process_start"], owner_boot_id=identity["boot_id"],
        now=int(time.time()),
    )
except ValueError:
    raise SystemExit(1)
open(owner, "x").close()
while not os.path.exists(release):
    if time.monotonic() >= deadline:
        raise SystemExit(3)
    time.sleep(0.01)
raise SystemExit(0)
'''
    with tempfile.TemporaryDirectory() as tmp:
        intent_dir = os.path.join(tmp, "intent")
        barrier_dir = os.path.join(tmp, "barrier")
        os.mkdir(barrier_dir)
        first = subprocess.Popen(
            [
                sys.executable,
                "-c",
                program,
                GUARD,
                intent_dir,
                "req-parallel-a",
                "c" * 48,
                barrier_dir,
            ]
        )
        second = subprocess.Popen(
            [
                sys.executable,
                "-c",
                program,
                GUARD,
                intent_dir,
                "req-parallel-b",
                "d" * 48,
                barrier_dir,
            ]
        )
        deadline = time.monotonic() + 5
        while not (
            os.path.exists(os.path.join(barrier_dir, "ready-req-parallel-a"))
            and os.path.exists(os.path.join(barrier_dir, "ready-req-parallel-b"))
        ):
            if time.monotonic() >= deadline:
                raise AssertionError("parallel creators did not reach the start barrier")
            time.sleep(0.01)
        open(os.path.join(barrier_dir, "go"), "x").close()
        while not os.path.exists(os.path.join(barrier_dir, "owner")):
            if time.monotonic() >= deadline:
                raise AssertionError("no parallel creator acquired the held lease")
            time.sleep(0.01)
        # The winning process remains alive while the loser attempts its own
        # creation, so a stale-PID race cannot turn this into two successes.
        open(os.path.join(barrier_dir, "release"), "x").close()
        results = sorted([first.wait(timeout=10), second.wait(timeout=10)])
        assert results == [0, 1], f"parallel creators did not serialize: {results}"
    print("PASS: independent creator processes contend for one local lease")


def test_cli_reconcile_exits_nonzero_for_killed_fixture() -> None:
    with tempfile.TemporaryDirectory() as tmp:
        intent_path = os.path.join(tmp, "req-killed.json")
        killed = provisioning_record()
        killed["active_lease"] = None
        killed["lease_binding"] = None
        with open(intent_path, "w", encoding="utf-8") as handle:
            json.dump(killed, handle)
        proc = subprocess.run(
            [
                sys.executable,
                GUARD,
                "reconcile",
                "--live-pods-json",
                json.dumps(live()),
                "--intent-dir",
                tmp,
                "--receipts-dir",
                tmp,
            ],
            capture_output=True,
            text=True,
            check=False,
        )
        assert proc.returncode == 1, proc.stdout + proc.stderr
        assert "abandoned_intent" in proc.stdout
    print("PASS: CLI refuses a live pod after SIGKILL-like bound state")


def main() -> int:
    test_fresh_matching_provisioning_lease_is_owned()
    test_expired_dead_reused_wrong_and_legacy_claims_are_orphans()
    test_darwin_boot_identity_refuses_missing_or_malformed_sysctl()
    test_pending_env_and_bound_without_lease_are_not_owners()
    test_parent_handoff_and_operator_keep_expiry()
    test_duplicate_creator_and_terminal_tombstone()
    test_legacy_requested_intent_cannot_renew_or_bind()
    test_bound_lease_blocks_new_creator_after_create_lock_is_lost()
    test_owner_token_fd_keeps_lease_capability_out_of_argv()
    test_owner_token_is_not_exported_from_private_state()
    test_parallel_process_creators_contend_for_one_lock()
    test_cli_reconcile_exits_nonzero_for_killed_fixture()
    print("test-runpod-orphan-reconcile: PASS")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except AssertionError as exc:
        print(f"FAIL: {exc}", file=sys.stderr)
        raise SystemExit(1)
