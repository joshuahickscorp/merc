#!/usr/bin/env python3
"""Validate an agent-supervisor action receipt without treating it as proof.

The reviewed restart adapter can request a supervisor action. Only PostgreSQL
agent-session transitions observed by go-closure-restart-storm.sh prove that
the two approved cx-agent processes actually restarted.
"""

import argparse
import datetime as dt
import json
import math
import re
import sys
from pathlib import Path


UUID = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-"
    r"[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
RUN_ID = re.compile(r"^[0-9a-f]{32}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
IMAGE = re.compile(r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$")
SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:/@-]{7,199}$")
SECRET = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)


def fail(message):
    raise ValueError(message)


def object_without_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            fail(f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def parse_utc(value, field):
    if not isinstance(value, str) or not value.endswith("Z"):
        fail(f"{field} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        fail(f"{field} is not a valid RFC3339 timestamp: {exc}")
    if parsed.utcoffset() != dt.timedelta(0):
        fail(f"{field} must be UTC")
    return parsed


def strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield str(key)
            yield from strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from strings(item)


def finite_numbers(value):
    if isinstance(value, float) and not math.isfinite(value):
        return False
    if isinstance(value, dict):
        return all(finite_numbers(item) for item in value.values())
    if isinstance(value, list):
        return all(finite_numbers(item) for item in value)
    return True


def validate(receipt, args):
    expected_root = {
        "schema_version",
        "kind",
        "status",
        "requested",
        "binding",
        "started_at",
        "finished_at",
        "safety",
        "actions",
    }
    if not isinstance(receipt, dict) or set(receipt) != expected_root:
        fail("receipt root does not match the closed restart-action schema")
    if (
        receipt["schema_version"] != 2
        or receipt["kind"] != "merc_agent_restart_action"
        or receipt["status"] != "PASS"
        or receipt["requested"] != 2
    ):
        fail("receipt identity, status, or requested count is invalid")

    expected_binding = {
        "run_id": args.run_id,
        "candidate_commit": args.commit,
        "control_image": args.image,
        "driver_sha256": args.driver_sha256,
    }
    if receipt["binding"] != expected_binding:
        fail("receipt does not bind this exact run/candidate/driver")
    if (
        not RUN_ID.fullmatch(args.run_id)
        or not COMMIT.fullmatch(args.commit)
        or not IMAGE.fullmatch(args.image)
        or not SHA256.fullmatch(args.driver_sha256)
    ):
        fail("validator expectations contain an invalid binding value")

    safety = receipt["safety"]
    expected_safety = {
        "stripe_live_mode": False,
        "real_value": False,
        "approved_participants_only": True,
        "secret_values_recorded": False,
    }
    if safety != expected_safety:
        fail("restart action safety envelope is invalid")

    expected_workers = sorted(
        item.strip().lower()
        for item in args.approved_worker_ids.split(",")
        if item.strip()
    )
    if (
        len(expected_workers) != 2
        or len(set(expected_workers)) != 2
        or not all(UUID.fullmatch(item) for item in expected_workers)
    ):
        fail("approved worker set must contain exactly two lowercase UUIDs")

    run_started = parse_utc(args.run_started_at, "expected run_started_at")
    checked_at = parse_utc(args.checked_at, "expected checked_at")
    started = parse_utc(receipt["started_at"], "started_at")
    finished = parse_utc(receipt["finished_at"], "finished_at")
    if started < run_started or finished < started or finished > checked_at:
        fail("receipt timestamps fall outside this restart invocation")

    actions = receipt["actions"]
    expected_action_keys = {"id", "worker_id", "occurred_at", "source"}
    if not isinstance(actions, list) or len(actions) != 2:
        fail("receipt must contain exactly two restart actions")
    action_ids = set()
    workers = []
    for index, action in enumerate(actions):
        if not isinstance(action, dict) or set(action) != expected_action_keys:
            fail(f"actions[{index}] does not match the closed action schema")
        if not SAFE_ID.fullmatch(action["id"] or ""):
            fail(f"actions[{index}].id is unsafe or too short")
        if action["id"] in action_ids:
            fail("restart action IDs must be unique")
        action_ids.add(action["id"])
        worker_id = action["worker_id"]
        if not UUID.fullmatch(worker_id or ""):
            fail(f"actions[{index}].worker_id must be a lowercase UUID")
        workers.append(worker_id)
        if action["source"] != "approved_agent_supervisor":
            fail("restart action source is not the approved supervisor")
        occurred = parse_utc(action["occurred_at"], f"actions[{index}].occurred_at")
        if occurred < started or occurred > finished:
            fail(f"actions[{index}] occurred outside the invocation window")
    if sorted(workers) != expected_workers:
        fail("restart actions do not target the exact approved worker set")

    if any(SECRET.search(value) for value in strings(receipt)):
        fail("receipt contains a secret-shaped value")
    if not finite_numbers(receipt):
        fail("receipt contains a non-finite number")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("receipt")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--driver-sha256", required=True)
    parser.add_argument("--approved-worker-ids", required=True)
    parser.add_argument("--run-started-at", required=True)
    parser.add_argument("--checked-at", required=True)
    args = parser.parse_args()
    try:
        path = Path(args.receipt)
        if path.stat().st_size > 1_048_576:
            fail("receipt exceeds the 1 MiB bound")
        receipt = json.loads(
            path.read_text(),
            object_pairs_hook=object_without_duplicate_keys,
        )
        validate(receipt, args)
    except (OSError, json.JSONDecodeError, ValueError) as exc:
        print(f"agent-restart-receipt: FAIL: {exc}", file=sys.stderr)
        return 1
    print("agent-restart-receipt: PASS (two exact approved supervisor actions)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
