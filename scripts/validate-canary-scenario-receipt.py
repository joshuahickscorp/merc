#!/usr/bin/env python3
"""Validate one externally driven GO-closure canary scenario receipt.

The scenario driver is an adapter, not an authority. Its receipt must be bound
to the exact run, candidate commit, immutable image, and driver bytes; carry
fresh observations from the expected source; and contain no secret-shaped
values. The host rehearsal independently corroborates database-backed subjects
after this structural/provenance gate passes.

Every receipt field must be derivable from an observation. This validator does
not re-assert hardcoded booleans the driver could invent; it checks structure,
binding, windows, closed key sets, and that claimed sources match the scenario
contract. Unmeasurable claims (e.g. backoff_schedule without PromQL) must not
appear.
"""

import argparse
import datetime as dt
import json
import math
import re
import sys
from pathlib import Path
_SCRIPTS = Path(__file__).resolve().parent
if str(_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(_SCRIPTS))

from lib.receipt_json import (
    fail,
    object_without_duplicate_keys,
    parse_utc,
    strings,
)


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
    # Aggregate SQL audit over tasks/jobs — not a non-existent invariant_audit table.
    "post_rehearsal_invariant_audit": "merc_postgres.tasks",
    # Retry ceiling is measured in PostgreSQL, not Prometheus, unless PromQL is used.
    "bounded_retry_backoff_audit": "merc_postgres.tasks",
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
# Keep aligned with scripts/canary-scenario-driver.sh SECRET / redact_secrets.
SECRET = re.compile(
    r"(sk_(?:test|live)_[A-Za-z0-9]+|"
    r"rk_(?:test|live)_[A-Za-z0-9]+|"
    r"pk_(?:test|live)_[A-Za-z0-9]+|"
    r"whsec_[A-Za-z0-9]+|"
    r"cx_(?:test|live)_[A-Za-z0-9_-]+|"
    r"cxw_[A-Za-z0-9_-]+|"
    r"ca_[A-Za-z0-9]+|"
    r"AGE-SECRET-KEY-[A-Za-z0-9+-]+|"
    r"AKIA[0-9A-Z]{12,}|"
    r"(?:postgres|postgresql|mysql|mongodb(?:\+srv)?|redis|amqp|https?)://"
    r"[^\s:/@]+:[^\s/@]+@)",
    re.IGNORECASE,
)



def require_exact_bool(mapping, name, expected):
    if mapping.get(name) is not expected:
        fail(f"{name} must be {str(expected).lower()}")


def require_bool(mapping, name):
    value = mapping.get(name)
    if value is not True and value is not False:
        fail(f"{name} must be a boolean")
    return value


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
        # Critical path must be observed true. object_checks is measured and may
        # be false on an honest --db-only restore drill.
        for field in (
            "encrypted_offsite_upload",
            "independent_download",
            "ciphertext_checksum_verified",
            "isolated_restore",
            "postgres_semantic_checks",
        ):
            require_exact_bool(receipt, field, True)
        require_bool(receipt, "object_checks")
    elif scenario == "post_rehearsal_invariant_audit":
        # Only invariants the driver actually queries. unreconciled_state is not
        # published because no reconciliation observation exists.
        expected_keys = {
            "tenant_leak",
            "missing_artifact",
            "duplicate_effects",
            "ledger_imbalance",
            "stuck_terminal_jobs",
            "stuck_payouts",
            "silent_webhook_loss",
            "unbounded_growth",
        }
        invariants = receipt.get("invariants")
        if not isinstance(invariants, dict) or set(invariants) != expected_keys:
            fail("post-rehearsal invariant map keys do not match the measured set")
        for key in expected_keys:
            if invariants[key] is not False:
                fail(f"invariant {key} must be false (clean)")
        if "unreconciled_state" in receipt.get("invariants", {}):
            fail("unreconciled_state must not appear without a reconciliation query")
        if "unreconciled_state" in receipt:
            fail("unreconciled_state must not appear without a reconciliation query")
    elif scenario == "bounded_retry_backoff_audit":
        # backoff_schedule_within_policy is forbidden unless measured (it is not).
        if "backoff_schedule_within_policy" in receipt:
            fail(
                "backoff_schedule_within_policy must not appear without a measured "
                "requeue-delay observation"
            )
        require_exact_bool(receipt, "max_attempts_within_policy", True)
        require_exact_bool(receipt, "unbounded_retry_growth", False)
        if not isinstance(receipt.get("observed_task_count"), int) \
                or isinstance(receipt.get("observed_task_count"), bool) \
                or receipt["observed_task_count"] < 1:
            fail("bounded_retry_backoff_audit requires observed_task_count >= 1")


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
    # Live value movement and live Stripe must never be certified by a canary.
    require_exact_bool(safety, "stripe_live_mode", False)
    require_exact_bool(safety, "real_value", False)
    require_exact_bool(safety, "approved_participants_only", True)
    require_exact_bool(safety, "secret_values_recorded", False)
    # stripe_test_mode is derived from observed payment_mode (true only for test).
    require_bool(safety, "stripe_test_mode")
    payment_mode = safety.get("payment_mode")
    if payment_mode not in ("sealed", "test"):
        fail("safety.payment_mode must be sealed or test (observed from control plane)")
    if payment_mode == "test" and safety.get("stripe_test_mode") is not True:
        fail("safety.stripe_test_mode must be true when payment_mode is test")
    if payment_mode == "sealed" and safety.get("stripe_test_mode") is not False:
        fail("safety.stripe_test_mode must be false when payment_mode is sealed")
    require_exact_bool(safety, "live_value_movement", False)

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
