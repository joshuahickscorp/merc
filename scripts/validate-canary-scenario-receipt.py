#!/usr/bin/env python3
"""Validate one externally driven GO-closure canary scenario receipt.

The scenario driver is an adapter, not an authority. Its receipt must be bound
to the exact run, candidate commit, immutable image, and driver bytes; carry
fresh observations from the expected source; and contain no secret-shaped
values. The host rehearsal independently corroborates database-backed subjects
after this structural/provenance gate passes.
"""

import argparse
import datetime as dt
import json
import math
import re
import sys
from pathlib import Path


EXPECTED_SOURCES = {
    "approved_buyer_identity": "merc_postgres.buyers",
    "distinct_metal_agent": "merc_postgres.workers",
    "embed_success": "merc_postgres.jobs",
    "batch_infer_success": "merc_postgres.jobs",
    "cancelled_job": "merc_postgres.jobs",
    "forced_retry": "merc_postgres.job_events",
    "stale_lease_recovery": "merc_postgres.job_events",
    "stale_attempt_commit_rejection": "merc_control.http",
    "buyer_webhook_retry_sequence": "merc_postgres.webhooks",
    "backup_independent_restore": "offsite_backup_provider",
    "stripe_test_matrix": "stripe_test_api",
    "real_alert_firing_resolution": "alert_receiver_api",
    "post_rehearsal_invariant_audit": "merc_postgres.invariant_audit",
    "bounded_retry_backoff_audit": "merc_prometheus",
}

UUID_SUBJECT_SCENARIOS = {
    "approved_buyer_identity",
    "distinct_metal_agent",
    "embed_success",
    "batch_infer_success",
    "cancelled_job",
    "forced_retry",
    "stale_lease_recovery",
    "stale_attempt_commit_rejection",
    "buyer_webhook_retry_sequence",
}

SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.:/@-]{7,199}$")
UUID = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-"
    r"[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
SHA256 = re.compile(r"^[0-9a-f]{64}$")
COMMIT = re.compile(r"^[0-9a-f]{40}$")
IMAGE = re.compile(r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$")
RUN_ID = re.compile(r"^[0-9a-f]{32}$")
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


def require_exact_bool(mapping, name, expected):
    if mapping.get(name) is not expected:
        fail(f"{name} must be {str(expected).lower()}")


def validate_special(receipt, scenario):
    if scenario == "stripe_test_matrix":
        if (receipt.get("provider_mode") != "test" or
                receipt.get("matrix_complete") is not True or
                receipt.get("real_value") is not False or
                receipt.get("application_outcomes_verified") is not True):
            fail("Stripe matrix must prove complete test-mode application outcomes")
    elif scenario == "real_alert_firing_resolution":
        event_ids = receipt.get("receiver_event_ids", {})
        firing, resolved = event_ids.get("firing"), event_ids.get("resolved")
        if not SAFE_ID.fullmatch(firing or "") or not SAFE_ID.fullmatch(resolved or ""):
            fail("alert receipt requires safe firing and resolved receiver event IDs")
        if firing == resolved:
            fail("alert firing and resolved receiver event IDs must differ")
    elif scenario == "backup_independent_restore":
        for field in (
            "encrypted_offsite_upload",
            "independent_download",
            "ciphertext_checksum_verified",
            "isolated_restore",
            "postgres_semantic_checks",
            "object_checks",
        ):
            require_exact_bool(receipt, field, True)
    elif scenario == "post_rehearsal_invariant_audit":
        expected = {
            "tenant_leak": False,
            "missing_artifact": False,
            "duplicate_effects": False,
            "ledger_imbalance": False,
            "stuck_terminal_jobs": False,
            "stuck_payouts": False,
            "unreconciled_state": False,
            "silent_webhook_loss": False,
            "unbounded_growth": False,
        }
        if receipt.get("invariants") != expected:
            fail("post-rehearsal invariant map is incomplete or not clean")
    elif scenario == "bounded_retry_backoff_audit":
        require_exact_bool(receipt, "max_attempts_within_policy", True)
        require_exact_bool(receipt, "backoff_schedule_within_policy", True)
        require_exact_bool(receipt, "unbounded_retry_growth", False)
def validate(receipt, args):
    scenario = args.scenario
    if scenario not in EXPECTED_SOURCES:
        fail(f"unsupported scenario {scenario!r}")
    if not isinstance(receipt, dict):
        fail("receipt root must be an object")
    if receipt.get("schema_version") != 2 or receipt.get("scenario") != scenario:
        fail("receipt schema or scenario identity is invalid")
    if receipt.get("status") != "PASS" or receipt.get("requested") != args.minimum:
        fail("receipt status or requested count is invalid")

    observed = receipt.get("observed")
    evidence = receipt.get("evidence")
    if not isinstance(observed, int) or isinstance(observed, bool) or observed != args.minimum:
        fail("observed must equal the exact requested count")
    if not isinstance(evidence, list) or len(evidence) != observed:
        fail("observed must equal the exact evidence-array length")

    binding = receipt.get("binding")
    if not isinstance(binding, dict):
        fail("binding must be an object")
    expected_binding = {
        "run_id": args.run_id,
        "candidate_commit": args.commit,
        "control_image": args.image,
        "driver_sha256": args.driver_sha256,
    }
    if binding != expected_binding:
        fail("receipt binding does not match this exact run/candidate/driver")
    if (not RUN_ID.fullmatch(args.run_id) or not COMMIT.fullmatch(args.commit) or
            not IMAGE.fullmatch(args.image) or not SHA256.fullmatch(args.driver_sha256)):
        fail("validator expectations contain an invalid run, commit, image, or driver digest")

    safety = receipt.get("safety")
    if not isinstance(safety, dict):
        fail("safety must be an object")
    for field, expected in (
        ("stripe_test_mode", True),
        ("stripe_live_mode", False),
        ("real_value", False),
        ("approved_participants_only", True),
        ("secret_values_recorded", False),
    ):
        require_exact_bool(safety, field, expected)

    run_started = parse_utc(args.run_started_at, "expected run_started_at")
    scenario_started = parse_utc(
        args.scenario_started_at, "expected scenario_started_at"
    )
    checked_at = parse_utc(args.checked_at, "expected checked_at")
    started = parse_utc(receipt.get("started_at"), "started_at")
    finished = parse_utc(receipt.get("finished_at"), "finished_at")
    if scenario_started < run_started or scenario_started > checked_at:
        fail("validator scenario window falls outside this run")
    if started < scenario_started or finished < started or finished > checked_at:
        fail("receipt timestamps fall outside this invocation window")

    observation_ids = set()
    subject_ids = set()
    expected_source = EXPECTED_SOURCES[scenario]
    for index, item in enumerate(evidence):
        expected_keys = {"id", "subject_id", "occurred_at", "source"}
        if scenario == "stale_attempt_commit_rejection":
            expected_keys |= {
                "submitted_attempt",
                "current_attempt",
                "before_state_sha256",
                "after_state_sha256",
                "http_status",
                "response_sha256",
            }
        if not isinstance(item, dict) or set(item) != expected_keys:
            fail(f"evidence[{index}] fields do not match the closed {scenario} schema")
        observation_id = item["id"]
        subject_id = item["subject_id"]
        if not SAFE_ID.fullmatch(observation_id or ""):
            fail(f"evidence[{index}].id is unsafe or too short")
        if observation_id in observation_ids:
            fail("evidence observation IDs must be unique")
        observation_ids.add(observation_id)
        if scenario in UUID_SUBJECT_SCENARIOS:
            if not UUID.fullmatch(subject_id or ""):
                fail(f"evidence[{index}].subject_id must be a lowercase UUID")
        elif not SAFE_ID.fullmatch(subject_id or ""):
            fail(f"evidence[{index}].subject_id is unsafe or too short")
        if subject_id in subject_ids:
            fail("evidence subject IDs must be unique")
        subject_ids.add(subject_id)
        if item["source"] != expected_source:
            fail(f"evidence[{index}].source must be {expected_source}")
        occurred = parse_utc(item["occurred_at"], f"evidence[{index}].occurred_at")
        if occurred < started or occurred > finished:
            fail(f"evidence[{index}] occurred outside the scenario window")
        if scenario == "stale_attempt_commit_rejection":
            submitted = item["submitted_attempt"]
            current = item["current_attempt"]
            if (not isinstance(submitted, int) or isinstance(submitted, bool) or submitted < 0 or
                    not isinstance(current, int) or isinstance(current, bool) or current <= submitted):
                fail("stale-attempt evidence requires current_attempt > submitted_attempt >= 0")
            if item["http_status"] != 409:
                fail("stale attempt must be observed as HTTP 409")
            for field in ("before_state_sha256", "after_state_sha256", "response_sha256"):
                if not SHA256.fullmatch(item[field] or ""):
                    fail(f"stale-attempt {field} is missing or invalid")
            if item["before_state_sha256"] != item["after_state_sha256"]:
                fail("stale attempt changed the observed task/money state")

    validate_special(receipt, scenario)
    if any(SECRET.search(value) for value in strings(receipt)):
        fail("receipt contains a secret-shaped value")
    # JSON permits non-standard NaN/Infinity tokens in Python; exclude them even
    # though this schema currently expects no floating point values.
    def finite_numbers(value):
        if isinstance(value, float) and not math.isfinite(value):
            return False
        if isinstance(value, dict):
            return all(finite_numbers(v) for v in value.values())
        if isinstance(value, list):
            return all(finite_numbers(v) for v in value)
        return True
    if not finite_numbers(receipt):
        fail("receipt contains a non-finite number")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("receipt")
    parser.add_argument("--scenario", required=True)
    parser.add_argument("--minimum", required=True, type=int)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--driver-sha256", required=True)
    parser.add_argument("--run-started-at", required=True)
    parser.add_argument("--scenario-started-at", required=True)
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
        print(f"canary-scenario-receipt: FAIL: {exc}", file=sys.stderr)
        return 1
    print(f"canary-scenario-receipt: PASS {args.scenario} ({len(receipt['evidence'])} observations)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
