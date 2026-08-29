#!/usr/bin/env python3
"""Validate a GO-closure soak receipt from its retained raw samples.

The soak runner is allowed to collect observations, but its PASS booleans are
not authority by themselves. This validator binds the receipt to the exact
candidate and independently re-derives continuity, health, and resource bounds
from the SHA-256-bound JSONL sample stream.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import re
import sys
from decimal import Decimal, InvalidOperation
from pathlib import Path, PurePosixPath
_SCRIPTS = Path(__file__).resolve().parent
if str(_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(_SCRIPTS))

from lib.receipt_json import (
    all_strings,
    exact_keys,
    fail,
    finite_numbers,
    object_without_duplicate_keys,
    parse_utc,
    reject_constant,
)


COMMIT = re.compile(r"^[0-9a-f]{40}$")
IMAGE = re.compile(
    r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$"
)
CONTAINER_ID = re.compile(r"^[0-9a-f]{64}$")
IMAGE_ID = re.compile(r"^sha256:[0-9a-f]{64}$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
SECRET = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)
MAX_RECEIPT_BYTES = 1_048_576
MAX_SAMPLES_BYTES = 268_435_456
MAX_SAMPLE_LINE_BYTES = 1_048_576



def load_json_bytes(raw: bytes, label: str):
    try:
        return json.loads(
            raw,
            object_pairs_hook=object_without_duplicate_keys,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"{label} is not strict UTF-8 JSON: {exc}")


def integer(value, field: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        fail(f"{field} must be an integer >= {minimum}")
    return value


def number(value, field: str, minimum: float = 0) -> float:
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
        or value < minimum
    ):
        fail(f"{field} must be a finite number >= {minimum}")
    return float(value)


def resolve_sample_path(root: Path, relative: str) -> Path:
    if not isinstance(relative, str) or not relative:
        fail("samples.path must be a non-empty relative path")
    pure = PurePosixPath(relative)
    if pure.is_absolute() or ".." in pure.parts or "." in pure.parts:
        fail("samples.path must stay beneath the evidence root")
    root = root.resolve(strict=True)
    unresolved = root / Path(*pure.parts)
    cursor = root
    for part in pure.parts:
        cursor = cursor / part
        if cursor.is_symlink():
            fail("samples.path must not contain symlinks")
    candidate = unresolved.resolve(strict=True)
    try:
        candidate.relative_to(root)
    except ValueError:
        fail("samples.path resolves outside the evidence root")
    if not candidate.is_file():
        fail("samples.path must resolve to a regular non-symlink file")
    return candidate


def load_samples(path: Path) -> tuple[list[dict], str]:
    size = path.stat().st_size
    if size <= 0 or size > MAX_SAMPLES_BYTES:
        fail("sample stream is empty or exceeds the 256 MiB bound")
    digest = hashlib.sha256()
    samples = []
    with path.open("rb") as handle:
        for line_number, raw in enumerate(handle, 1):
            digest.update(raw)
            if len(raw) > MAX_SAMPLE_LINE_BYTES:
                fail(f"sample line {line_number} exceeds the 1 MiB bound")
            if not raw.strip():
                fail(f"sample line {line_number} is blank")
            sample = load_json_bytes(raw, f"sample line {line_number}")
            if not isinstance(sample, dict):
                fail(f"sample line {line_number} is not an object")
            samples.append(sample)
    return samples, digest.hexdigest()


def same_number(actual, expected, field: str) -> None:
    a = number(actual, field)
    e = number(expected, f"derived {field}")
    if not math.isclose(a, e, rel_tol=0, abs_tol=1e-9):
        fail(f"{field}={a} does not match the sample-derived value {e}")


def validate_database_snapshot(value, field: str) -> None:
    exact_keys(
        value,
        {
            "buyers",
            "suppliers",
            "workers",
            "jobs",
            "tasks",
            "ledger_entries",
            "ledger_sum_usd",
            "terminal_jobs_with_open_tasks",
        },
        field,
    )
    for key in (
        "buyers",
        "suppliers",
        "workers",
        "jobs",
        "tasks",
        "ledger_entries",
        "terminal_jobs_with_open_tasks",
    ):
        integer(value[key], f"{field}.{key}")
    if value["terminal_jobs_with_open_tasks"] != 0:
        fail(f"{field} contains a terminal job with open tasks")
    try:
        ledger_sum = Decimal(value["ledger_sum_usd"])
    except (InvalidOperation, TypeError):
        fail(f"{field}.ledger_sum_usd is not a finite decimal")
    if not ledger_sum.is_finite():
        fail(f"{field}.ledger_sum_usd is not finite")


def validate_sample(
    sample,
    index: int,
    runtime: dict,
    started: dt.datetime,
    finished: dt.datetime,
    previous_time: dt.datetime | None,
) -> dt.datetime:
    field = f"samples[{index - 1}]"
    exact_keys(
        sample,
        {
            "observed_at",
            "sequence",
            "control_container_id",
            "control_configured_image",
            "control_image_id",
            "control_rss_kb",
            "host_disk_used_kb",
            "control_writable_layer_bytes",
            "control_restart_count",
            "active_workers",
            "db_connections_total",
            "db_connections_acquired",
            "firing_page_alerts",
            "webhook_dead_letters",
            "database",
        },
        field,
    )
    integer(sample["sequence"], f"{field}.sequence", 1)
    if sample["sequence"] != index:
        fail(f"{field}.sequence is not the contiguous expected value {index}")
    observed = parse_utc(sample["observed_at"], f"{field}.observed_at")
    if observed < started or observed > finished:
        fail(f"{field}.observed_at falls outside the receipt window")
    if previous_time is not None and observed <= previous_time:
        fail("sample timestamps must be strictly increasing")
    if (
        sample["control_container_id"] != runtime["container_id"]
        or sample["control_configured_image"] != runtime["configured_image"]
        or sample["control_image_id"] != runtime["image_id"]
        or sample["control_restart_count"] != runtime["restart_count"]
    ):
        fail(f"{field} does not preserve the exact candidate runtime identity")

    integer(sample["control_rss_kb"], f"{field}.control_rss_kb", 1)
    integer(sample["host_disk_used_kb"], f"{field}.host_disk_used_kb", 1)
    integer(
        sample["control_writable_layer_bytes"],
        f"{field}.control_writable_layer_bytes",
    )
    integer(sample["control_restart_count"], f"{field}.control_restart_count")
    if number(sample["active_workers"], f"{field}.active_workers") < 2:
        fail(f"{field} observed fewer than two active workers")
    number(sample["db_connections_total"], f"{field}.db_connections_total")
    number(sample["db_connections_acquired"], f"{field}.db_connections_acquired")
    if number(sample["firing_page_alerts"], f"{field}.firing_page_alerts") != 0:
        fail(f"{field} observed a firing page alert")
    if number(sample["webhook_dead_letters"], f"{field}.webhook_dead_letters") != 0:
        fail(f"{field} observed a webhook dead letter")
    validate_database_snapshot(sample["database"], f"{field}.database")
    return observed


def validate(receipt, samples: list[dict], sample_digest: str, args) -> None:
    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "started_at",
            "finished_at",
            "mode",
            "control_image",
            "expected_commit",
            "runtime",
            "duration",
            "samples",
            "bounds",
            "assertions",
            "qualification",
            "policy",
        },
        "receipt",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 2
        or receipt["kind"] != "go_closure_soak"
        or receipt["status"] != "PASS"
    ):
        fail("receipt identity or status is invalid")
    if not COMMIT.fullmatch(args.commit) or not IMAGE.fullmatch(args.image):
        fail("validator expectations are not an exact commit and immutable image")
    if (
        receipt["expected_commit"] != args.commit
        or receipt["control_image"] != args.image
    ):
        fail("receipt is not bound to the expected candidate")

    started = parse_utc(receipt["started_at"], "started_at")
    finished = parse_utc(receipt["finished_at"], "finished_at")
    if finished <= started:
        fail("receipt time window is empty or reversed")

    runtime = receipt["runtime"]
    exact_keys(
        runtime,
        {"container_id", "configured_image", "image_id", "restart_count"},
        "runtime",
    )
    if not CONTAINER_ID.fullmatch(runtime["container_id"] or ""):
        fail("runtime.container_id is not a full Docker container ID")
    if runtime["configured_image"] != args.image:
        fail("runtime.configured_image is not the exact candidate image")
    if not IMAGE_ID.fullmatch(runtime["image_id"] or ""):
        fail("runtime.image_id is not a content-addressed Docker image ID")
    integer(runtime["restart_count"], "runtime.restart_count")

    duration = receipt["duration"]
    exact_keys(
        duration,
        {"requested_seconds", "actual_seconds", "interval_seconds", "samples"},
        "duration",
    )
    requested = integer(duration["requested_seconds"], "duration.requested_seconds", 60)
    actual = integer(duration["actual_seconds"], "duration.actual_seconds", 60)
    interval = integer(duration["interval_seconds"], "duration.interval_seconds", 15)
    sample_count = integer(duration["samples"], "duration.samples", 1)
    if interval > 900:
        fail("duration.interval_seconds exceeds 900")
    if actual < requested:
        fail("duration.actual_seconds is shorter than requested_seconds")
    wall_seconds = int((finished - started).total_seconds())
    if wall_seconds < actual or wall_seconds > actual + 300:
        fail("receipt timestamps do not corroborate actual_seconds")
    expected_samples = max(1, requested // interval)
    minimum_samples = math.ceil(expected_samples * 0.95)
    if sample_count < minimum_samples:
        fail("receipt has fewer than 95% of the expected samples")
    if sample_count != len(samples):
        fail("duration.samples does not match the retained sample stream")

    samples_ref = receipt["samples"]
    exact_keys(samples_ref, {"path", "sha256"}, "samples")
    if not SHA256.fullmatch(samples_ref["sha256"] or ""):
        fail("samples.sha256 is invalid")
    if samples_ref["sha256"] != sample_digest:
        fail("sample stream SHA-256 does not match the receipt")

    previous = None
    for index, sample in enumerate(samples, 1):
        previous = validate_sample(sample, index, runtime, started, finished, previous)

    assertions = receipt["assertions"]
    exact_keys(
        assertions,
        {
            "two_agents_continuously_present",
            "no_page_alerts",
            "no_webhook_dead_letters",
            "no_control_restarts_or_recreates",
            "no_stuck_terminal_jobs",
            "bounded_resource_growth",
            "raw_samples_independently_validated",
        },
        "assertions",
    )
    if not all(value is True for value in assertions.values()):
        fail("every independently derived assertion must be true")

    bounds = receipt["bounds"]
    exact_keys(
        bounds,
        {"rss", "disk", "writable_layer", "db_connections"},
        "bounds",
    )
    expected_bound_keys = {
        "rss": {"baseline_kb", "max_kb", "final_kb", "observed_growth_bytes", "limit_growth_bytes"},
        "disk": {
            "baseline_used_kb",
            "max_used_kb",
            "final_used_kb",
            "observed_growth_kb",
            "limit_growth_kb",
        },
        "writable_layer": {
            "baseline_bytes",
            "max_bytes",
            "final_bytes",
            "observed_growth_bytes",
            "limit_growth_bytes",
        },
        "db_connections": {
            "baseline",
            "max",
            "final",
            "observed_growth",
            "limit_growth",
        },
    }
    for key, keys in expected_bound_keys.items():
        exact_keys(bounds[key], keys, f"bounds.{key}")

    rss = [integer(item["control_rss_kb"], "sample.control_rss_kb", 1) for item in samples]
    disk = [
        integer(item["host_disk_used_kb"], "sample.host_disk_used_kb", 1)
        for item in samples
    ]
    writable = [
        integer(item["control_writable_layer_bytes"], "sample.control_writable_layer_bytes")
        for item in samples
    ]
    connections = [
        number(item["db_connections_total"], "sample.db_connections_total")
        for item in samples
    ]

    rss_baseline = integer(bounds["rss"]["baseline_kb"], "bounds.rss.baseline_kb", 1)
    disk_baseline = integer(
        bounds["disk"]["baseline_used_kb"], "bounds.disk.baseline_used_kb", 1
    )
    writable_baseline = integer(
        bounds["writable_layer"]["baseline_bytes"],
        "bounds.writable_layer.baseline_bytes",
    )
    connection_baseline = number(
        bounds["db_connections"]["baseline"], "bounds.db_connections.baseline"
    )

    derived = {
        "rss": {
            "max": max([rss_baseline, *rss]),
            "final": rss[-1],
        },
        "disk": {
            "max": max([disk_baseline, *disk]),
            "final": disk[-1],
        },
        "writable": {
            "max": max([writable_baseline, *writable]),
            "final": writable[-1],
        },
        "connections": {
            "max": max([connection_baseline, *connections]),
            "final": connections[-1],
        },
    }
    derived["rss"]["growth"] = (derived["rss"]["max"] - rss_baseline) * 1024
    derived["disk"]["growth"] = derived["disk"]["max"] - disk_baseline
    derived["writable"]["growth"] = (
        derived["writable"]["max"] - writable_baseline
    )
    derived["connections"]["growth"] = (
        derived["connections"]["max"] - connection_baseline
    )

    comparisons = (
        (bounds["rss"]["max_kb"], derived["rss"]["max"], "bounds.rss.max_kb"),
        (bounds["rss"]["final_kb"], derived["rss"]["final"], "bounds.rss.final_kb"),
        (
            bounds["rss"]["observed_growth_bytes"],
            derived["rss"]["growth"],
            "bounds.rss.observed_growth_bytes",
        ),
        (
            bounds["disk"]["max_used_kb"],
            derived["disk"]["max"],
            "bounds.disk.max_used_kb",
        ),
        (
            bounds["disk"]["final_used_kb"],
            derived["disk"]["final"],
            "bounds.disk.final_used_kb",
        ),
        (
            bounds["disk"]["observed_growth_kb"],
            derived["disk"]["growth"],
            "bounds.disk.observed_growth_kb",
        ),
        (
            bounds["writable_layer"]["max_bytes"],
            derived["writable"]["max"],
            "bounds.writable_layer.max_bytes",
        ),
        (
            bounds["writable_layer"]["final_bytes"],
            derived["writable"]["final"],
            "bounds.writable_layer.final_bytes",
        ),
        (
            bounds["writable_layer"]["observed_growth_bytes"],
            derived["writable"]["growth"],
            "bounds.writable_layer.observed_growth_bytes",
        ),
        (
            bounds["db_connections"]["max"],
            derived["connections"]["max"],
            "bounds.db_connections.max",
        ),
        (
            bounds["db_connections"]["final"],
            derived["connections"]["final"],
            "bounds.db_connections.final",
        ),
        (
            bounds["db_connections"]["observed_growth"],
            derived["connections"]["growth"],
            "bounds.db_connections.observed_growth",
        ),
    )
    for actual_value, expected_value, field in comparisons:
        same_number(actual_value, expected_value, field)

    limits = (
        (
            derived["rss"]["growth"],
            bounds["rss"]["limit_growth_bytes"],
            "RSS",
        ),
        (
            derived["disk"]["growth"],
            bounds["disk"]["limit_growth_kb"],
            "disk",
        ),
        (
            derived["writable"]["growth"],
            bounds["writable_layer"]["limit_growth_bytes"],
            "writable layer",
        ),
        (
            derived["connections"]["growth"],
            bounds["db_connections"]["limit_growth"],
            "database connection",
        ),
    )
    for observed, limit, label in limits:
        if observed > number(limit, f"{label} growth limit"):
            fail(f"{label} growth exceeds the frozen receipt limit")

    qualification = receipt["qualification"]
    exact_keys(
        qualification,
        {"qualifies_for_24h_gate", "reason"},
        "qualification",
    )
    mode = receipt["mode"]
    qualifies = qualification["qualifies_for_24h_gate"]
    if mode == "qualifying":
        if requested < 86400 or actual < 86400 or qualifies is not True:
            fail("qualifying mode lacks a complete 24-hour observation")
        if qualification["reason"] != "observed_at_least_86400_seconds":
            fail("qualifying reason is invalid")
    elif mode == "iteration":
        if qualifies is not False or qualification["reason"] != "short_iteration_only":
            fail("iteration mode cannot qualify for the 24-hour gate")
    else:
        fail("mode must be qualifying or iteration")

    expected_policy = {
        "stripe_test_mode": True,
        "stripe_live_mode": False,
        "real_value": False,
        "secret_values_recorded": False,
    }
    if receipt["policy"] != expected_policy:
        fail("receipt safety policy is invalid")
    if any(SECRET.search(value) for value in all_strings(receipt)):
        fail("receipt contains a secret-shaped value")
    if not finite_numbers(receipt):
        fail("receipt contains a non-finite number")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("receipt")
    parser.add_argument("--root", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--image", required=True)
    args = parser.parse_args()
    try:
        receipt_path = Path(args.receipt)
        if receipt_path.stat().st_size > MAX_RECEIPT_BYTES:
            fail("receipt exceeds the 1 MiB bound")
        receipt = load_json_bytes(receipt_path.read_bytes(), "receipt")
        if not isinstance(receipt, dict):
            fail("receipt must be a JSON object")
        samples_ref = receipt.get("samples") if isinstance(receipt, dict) else None
        if not isinstance(samples_ref, dict):
            fail("receipt samples reference is missing")
        sample_path = resolve_sample_path(Path(args.root), samples_ref.get("path"))
        samples, digest = load_samples(sample_path)
        validate(receipt, samples, digest, args)
    except (OSError, TypeError, ValueError) as exc:
        print(f"go-closure-soak-receipt: FAIL: {exc}", file=sys.stderr)
        return 1
    print(
        "go-closure-soak-receipt: PASS "
        "(exact uninterrupted candidate and sample-derived bounds)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
