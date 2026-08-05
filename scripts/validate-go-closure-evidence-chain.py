#!/usr/bin/env python3
"""Validate the complete external Level-B evidence chain.

Every operational receipt is independently closed and then bound to the same
commit, immutable image, execution order, freshness window, raw soak samples,
fresh backup bytes, and final governance approval. A PASS authorizes only
review of the supervised Stripe test-mode private canary; it never authorizes
live payments, public access, or unrestricted suppliers.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import importlib.util
import json
import math
import re
import sys
import unicodedata
from decimal import Decimal, InvalidOperation
from pathlib import Path, PurePosixPath
from types import SimpleNamespace
_SCRIPTS = Path(__file__).resolve().parent
if str(_SCRIPTS) not in sys.path:
    sys.path.insert(0, str(_SCRIPTS))

from lib.receipt_json import (
    all_strings,
    exact_keys,
    fail,
    finite_numbers,
    object_without_duplicate_keys,
    reject_constant,
    sha256_file,
)


COMMIT = re.compile(r"^[0-9a-f]{40}$")
SHA256 = re.compile(r"^[0-9a-f]{64}$")
IMAGE = re.compile(
    r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$"
)
IMAGE_ID = re.compile(r"^sha256:[0-9a-f]{64}$")
RUN_ID = re.compile(r"^[0-9a-f]{32}$")
UUID = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-"
    r"[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
HOSTNAME = re.compile(
    r"^(?=.{1,253}$)(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}"
    r"[A-Za-z0-9])?\.)*[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$"
)
BACKUP_ID = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
GOVERNANCE_TIMESTAMP = re.compile(
    r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"
)
SECRET = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)
MAX_JSON_BYTES = 16 * 1024 * 1024
MAX_CHAIN_AGE = dt.timedelta(days=7)
NOW_TOLERANCE = dt.timedelta(minutes=5)

APPROVAL_DOMAINS = {
    "security",
    "privacy",
    "legal",
    "licensing",
    "payments",
    "operations",
    "supplier_policy",
    "release_approval",
}
EXERCISES = {
    "support_tabletop",
    "security_tabletop",
    "dsar_export_deletion",
    "backup_tombstone",
    "asset_and_model_provenance",
}
SCENARIOS = (
    "approved_buyer_identity",
    "distinct_metal_agent",
    "embed_success",
    "batch_infer_success",
    "cancelled_job",
    "forced_retry",
    "stale_lease_recovery",
    "stale_attempt_commit_rejection",
    "buyer_webhook_retry_sequence",
    "backup_independent_restore",
    "stripe_test_matrix",
    "real_alert_firing_resolution",
    "post_rehearsal_invariant_audit",
    "bounded_retry_backoff_audit",
)
SCENARIO_MINIMUMS = dict(
    zip(SCENARIOS, (2, 2, 20, 20, 5, 5, 3, 3, 3, 1, 1, 1, 1, 1))
)
REQUIRED_COUNTS = {
    "approved_buyer_identities": 2,
    "distinct_metal_agents": 2,
    "successful_embed_jobs": 20,
    "successful_batch_infer_jobs": 20,
    "cancelled_jobs": 5,
    "forced_retries": 5,
    "stale_lease_recoveries": 3,
    "stale_attempt_commit_rejections": 3,
    "buyer_webhook_retry_sequences": 3,
    "backup_independent_restore": 1,
    "stripe_test_matrix": 1,
    "real_alert_firing_resolution": 1,
    "post_rehearsal_invariant_audit": 1,
    "bounded_retry_backoff_audit": 1,
}



def exact_integer(value, field: str, minimum: int = 0, maximum: int | None = None) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        fail(f"{field} must be an integer >= {minimum}")
    if maximum is not None and value > maximum:
        fail(f"{field} must be an integer <= {maximum}")
    return value


def finite_number(value, field: str, minimum: float = 0) -> float:
    if (
        isinstance(value, bool)
        or not isinstance(value, (int, float))
        or not math.isfinite(value)
        or value < minimum
    ):
        fail(f"{field} must be a finite number >= {minimum}")
    return float(value)


def parse_utc(value, field: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z") or len(value) > 64:
        fail(f"{field} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as exc:
        fail(f"{field} is not a valid RFC3339 timestamp: {exc}")
    if parsed.utcoffset() != dt.timedelta(0):
        fail(f"{field} must be UTC")
    return parsed


def bounded_text(value, field: str, maximum: int) -> str:
    if (
        not isinstance(value, str)
        or not value
        or value != value.strip()
        or len(value.encode("utf-8")) > maximum
        or any(unicodedata.category(character) == "Cc" for character in value)
    ):
        fail(f"{field} must be non-empty, trimmed, control-free, and <= {maximum} bytes")
    return value


def resolve_under_root(root: Path, supplied: str, field: str) -> Path:
    if not isinstance(supplied, str) or not supplied:
        fail(f"{field} path is missing")
    candidate = Path(supplied)
    if ".." in candidate.parts:
        fail(f"{field} path must not contain parent traversal")
    if not candidate.is_absolute():
        candidate = root / candidate
    try:
        relative = candidate.relative_to(root)
    except ValueError:
        fail(f"{field} resolves outside the evidence root")
    cursor = root
    for part in relative.parts:
        cursor = cursor / part
        if cursor.is_symlink():
            fail(f"{field} path must not contain symlinks")
    resolved = candidate.resolve(strict=True)
    try:
        resolved.relative_to(root)
    except ValueError:
        fail(f"{field} resolves outside the evidence root")
    if not resolved.is_file():
        fail(f"{field} must be a regular file")
    return resolved


def resolve_receipt_relative(root: Path, supplied: str, field: str) -> Path:
    if not isinstance(supplied, str) or not supplied:
        fail(f"{field} is missing")
    pure = PurePosixPath(supplied)
    if pure.is_absolute() or "." in pure.parts or ".." in pure.parts:
        fail(f"{field} must be a safe relative path")
    return resolve_under_root(root, supplied, field)


def load_json(path: Path, label: str, maximum: int = MAX_JSON_BYTES):
    size = path.stat().st_size
    if size <= 0 or size > maximum:
        fail(f"{label} is empty or exceeds its {maximum}-byte bound")
    try:
        value = json.loads(
            path.read_text(encoding="utf-8"),
            object_pairs_hook=object_without_duplicate_keys,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"{label} is not strict UTF-8 JSON: {exc}")
    if not isinstance(value, dict):
        fail(f"{label} root must be an object")
    if any(SECRET.search(item) for item in all_strings(value)):
        fail(f"{label} contains a secret-shaped value")
    if not finite_numbers(value):
        fail(f"{label} contains a non-finite number")
    return value


def load_validator(filename: str):
    path = Path(__file__).resolve().parent / filename
    spec = importlib.util.spec_from_file_location(filename.replace("-", "_"), path)
    if spec is None or spec.loader is None:
        fail(f"cannot load required validator {filename}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def validate_window(start_value, finish_value, label: str, checked_at: dt.datetime):
    started = parse_utc(start_value, f"{label}.started_at")
    finished = parse_utc(finish_value, f"{label}.finished_at")
    if finished < started or finished > checked_at:
        fail(f"{label} has an empty, reversed, or future time window")
    return started, finished


def validate_database_snapshot(value, field: str, require_zero_ledger: bool = False) -> None:
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
        exact_integer(value[key], f"{field}.{key}")
    if value["terminal_jobs_with_open_tasks"] != 0:
        fail(f"{field} contains a terminal job with open tasks")
    try:
        ledger = Decimal(value["ledger_sum_usd"])
    except (InvalidOperation, TypeError):
        fail(f"{field}.ledger_sum_usd is not a finite decimal")
    if not ledger.is_finite() or (require_zero_ledger and ledger != 0):
        fail(f"{field}.ledger_sum_usd does not satisfy the required invariant")


def validate_deploy(receipt, commit: str, image: str, checked_at: dt.datetime):
    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "scope",
            "started_at",
            "finished_at",
            "activation",
            "endpoint",
            "control_image",
            "control_image_id",
            "expected_commit",
            "reported_version",
            "tls_certificate_observation",
            "observability",
            "policy",
        },
        "deploy",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 1
        or receipt["kind"] != "go_closure_deploy"
        or receipt["status"] != "PASS"
        or receipt["scope"] != "supervised_stripe_test_mode_private_canary"
        or receipt["activation"] != "candidate"
        or receipt["control_image"] != image
        or receipt["expected_commit"] != commit
        or not IMAGE_ID.fullmatch(receipt["control_image_id"] or "")
        or not HOSTNAME.fullmatch(receipt["endpoint"] or "")
    ):
        fail("deploy identity or exact candidate binding is invalid")
    started, finished = validate_window(
        receipt["started_at"], receipt["finished_at"], "deploy", checked_at
    )
    exact_keys(
        receipt["reported_version"],
        {"version", "commit", "build_date", "go_version", "platform", "modified"},
        "deploy.reported_version",
    )
    if (
        receipt["reported_version"]["commit"] != commit
        or receipt["reported_version"]["modified"] is not False
        or receipt["reported_version"]["version"] in {"dev", "unknown"}
    ):
        fail("deploy reported version is not the exact clean candidate")
    for key in ("version", "build_date", "go_version", "platform"):
        bounded_text(receipt["reported_version"][key], f"deploy.reported_version.{key}", 256)
    build_date = parse_utc(
        receipt["reported_version"]["build_date"], "deploy.reported_version.build_date"
    )
    if (
        build_date > finished
        or not receipt["reported_version"]["go_version"].startswith("go")
        or "/" not in receipt["reported_version"]["platform"]
    ):
        fail("deploy reported build metadata is invalid")
    tls = receipt["tls_certificate_observation"]
    if (
        not isinstance(tls, str)
        or not tls
        or tls != tls.strip()
        or len(tls.encode("utf-8")) > 16_384
        or any(
            unicodedata.category(character) == "Cc" and character not in "\n\r\t"
            for character in tls
        )
    ):
        fail("deploy TLS observation is empty, oversized, or contains unsafe controls")
    if (
        not re.search(
            r"(?i)sha256 Fingerprint=(?:[0-9a-f]{2}:){31}[0-9a-f]{2}(?:\n|$)",
            tls,
        )
        or "notAfter=" not in tls
    ):
        fail("deploy TLS observation lacks certificate fingerprint or expiry")
    if receipt["observability"] != {
        "prometheus_ready": True,
        "alertmanager_ready": True,
        "receiver_name": receipt["observability"].get("receiver_name"),
    }:
        fail("deploy observability proof is incomplete")
    bounded_text(
        receipt["observability"]["receiver_name"],
        "deploy.observability.receiver_name",
        256,
    )
    if receipt["policy"] != {
        "stripe_live_mode": False,
        "real_value": False,
        "unrestricted_public_access": False,
        "secret_values_recorded": False,
    }:
        fail("deploy safety policy is invalid")
    return started, finished


def validate_backup_from_rollback(
    receipt, root: Path, rollback_started: dt.datetime, rollback_finished: dt.datetime
) -> None:
    backup = receipt["pre_upgrade_backup"]
    exact_keys(
        backup,
        {
            "backup_id",
            "ciphertext_sha256",
            "manifest_sha256",
            "local_manifest",
            "offsite_uri",
            "invocation_result",
            "verification_receipt",
        },
        "rollback.pre_upgrade_backup",
    )
    backup_id = backup["backup_id"]
    if not isinstance(backup_id, str) or not BACKUP_ID.fullmatch(backup_id):
        fail("rollback backup_id is invalid")
    manifest_path = resolve_receipt_relative(
        root, backup["local_manifest"], "rollback.pre_upgrade_backup.local_manifest"
    )
    verification_path = manifest_path.parent / "verification.json"
    ciphertext_path = manifest_path.parent / "backup.tar.age"
    for path, label in (
        (verification_path, "backup verification receipt"),
        (ciphertext_path, "backup ciphertext"),
    ):
        if not path.is_file() or path.is_symlink():
            fail(f"{label} must be a regular non-symlink file")
    manifest = load_json(manifest_path, "backup manifest", 1_048_576)
    verification = load_json(verification_path, "backup verification receipt", 1_048_576)
    if verification != backup["verification_receipt"]:
        fail("rollback does not embed the exact retained verification receipt")
    if manifest.get("backup_id") != backup_id or verification.get("backup_id") != backup_id:
        fail("rollback backup artifacts use different backup IDs")
    manifest_sha = sha256_file(manifest_path)
    verification_sha = sha256_file(verification_path)
    ciphertext_sha = sha256_file(ciphertext_path)
    if (
        backup["manifest_sha256"] != manifest_sha
        or backup["ciphertext_sha256"] != ciphertext_sha
        or manifest.get("ciphertext_sha256") != ciphertext_sha
    ):
        fail("rollback backup hashes do not bind the retained artifact bytes")

    invocation = backup["invocation_result"]
    exact_keys(
        invocation,
        {
            "schema_version",
            "kind",
            "status",
            "backup_id",
            "offsite_uri",
            "manifest_sha256",
            "verification_sha256",
            "ciphertext_sha256",
            "completed_at",
        },
        "rollback.pre_upgrade_backup.invocation_result",
    )
    if (
        type(invocation["schema_version"]) is not int
        or invocation["schema_version"] != 1
        or invocation["kind"] != "merc_backup_invocation_result"
        or invocation["status"] != "PASS"
        or invocation["backup_id"] != backup_id
        or invocation["offsite_uri"] != backup["offsite_uri"]
        or invocation["manifest_sha256"] != manifest_sha
        or invocation["verification_sha256"] != verification_sha
        or invocation["ciphertext_sha256"] != ciphertext_sha
    ):
        fail("rollback invocation result does not bind the exact completed backup")
    completed = parse_utc(invocation["completed_at"], "backup invocation completed_at")
    if completed < rollback_started or completed > rollback_finished:
        fail("backup invocation completed outside the rollback rehearsal")
    suffix = "/" + backup_id
    if (
        not isinstance(backup["offsite_uri"], str)
        or not backup["offsite_uri"].endswith(suffix)
    ):
        fail("rollback offsite URI is not bound to its backup ID")
    offsite_base = backup["offsite_uri"][: -len(suffix)]

    validator = load_validator("validate-backup-verification-receipt.py")
    validator.validate(
        manifest,
        verification,
        ciphertext_path,
        SimpleNamespace(
            manifest=str(manifest_path),
            offsite_base=offsite_base,
            not_before=rollback_started.strftime("%Y-%m-%dT%H:%M:%SZ"),
            checked_at=rollback_finished.strftime("%Y-%m-%dT%H:%M:%SZ"),
        ),
    )


def validate_rollback(
    receipt, root: Path, commit: str, image: str, checked_at: dt.datetime
):
    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "started_at",
            "finished_at",
            "candidate",
            "prior",
            "pre_upgrade_backup",
            "data_integrity",
            "policy",
        },
        "rollback",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 1
        or receipt["kind"] != "go_closure_rollback_forward"
        or receipt["status"] != "PASS"
    ):
        fail("rollback receipt identity is invalid")
    started, finished = validate_window(
        receipt["started_at"], receipt["finished_at"], "rollback", checked_at
    )
    exact_keys(
        receipt["candidate"],
        {"image", "commit", "forward_recoveries", "rto_seconds"},
        "rollback.candidate",
    )
    exact_keys(
        receipt["prior"],
        {"image", "commit", "rollbacks", "rto_seconds"},
        "rollback.prior",
    )
    if (
        receipt["candidate"]["image"] != image
        or receipt["candidate"]["commit"] != commit
        or receipt["candidate"]["forward_recoveries"] != 1
        or not IMAGE.fullmatch(receipt["prior"]["image"] or "")
        or not COMMIT.fullmatch(receipt["prior"]["commit"] or "")
        or receipt["prior"]["image"] == image
        or receipt["prior"]["commit"] == commit
        or receipt["prior"]["rollbacks"] != 1
    ):
        fail("rollback does not prove prior activation and exact candidate recovery")
    exact_integer(
        receipt["candidate"]["rto_seconds"], "rollback.candidate.rto_seconds", 0, 300
    )
    exact_integer(receipt["prior"]["rto_seconds"], "rollback.prior.rto_seconds", 0, 300)
    validate_backup_from_rollback(receipt, root, started, finished)

    integrity = receipt["data_integrity"]
    exact_keys(
        integrity,
        {"unchanged", "before_sha256", "after_sha256", "snapshot"},
        "rollback.data_integrity",
    )
    if (
        integrity["unchanged"] is not True
        or not SHA256.fullmatch(integrity["before_sha256"] or "")
        or integrity["before_sha256"] != integrity["after_sha256"]
    ):
        fail("rollback data-integrity proof is invalid")
    validate_database_snapshot(integrity["snapshot"], "rollback.data_integrity.snapshot")
    if receipt["policy"] != {
        "stripe_live_mode": False,
        "real_value": False,
        "secret_values_recorded": False,
    }:
        fail("rollback safety policy is invalid")
    return started, finished


def validate_session_set(value, field: str):
    if not isinstance(value, list) or len(value) != 2:
        fail(f"{field} must contain exactly two sessions")
    expected_keys = {
        "worker_id",
        "agent_session_id",
        "session_started_epoch",
        "last_seen_epoch",
        "agent_version",
        "build_hash",
    }
    result = {}
    for index, session in enumerate(value):
        exact_keys(session, expected_keys, f"{field}[{index}]")
        worker = session["worker_id"]
        session_id = session["agent_session_id"]
        if not UUID.fullmatch(worker or "") or not UUID.fullmatch(session_id or ""):
            fail(f"{field}[{index}] contains an invalid worker or session UUID")
        if worker in result:
            fail(f"{field} repeats a worker")
        exact_integer(session["session_started_epoch"], f"{field}[{index}].session_started_epoch", 1)
        exact_integer(session["last_seen_epoch"], f"{field}[{index}].last_seen_epoch", 1)
        bounded_text(session["agent_version"], f"{field}[{index}].agent_version", 128)
        if not re.fullmatch(r"[0-9a-f]{16}", session["build_hash"] or ""):
            fail(f"{field}[{index}].build_hash is invalid")
        result[worker] = session
    return result


def validate_restart(receipt, commit: str, image: str, checked_at: dt.datetime):
    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "run_id",
            "started_at",
            "finished_at",
            "control_image",
            "expected_commit",
            "agent_restart_driver",
            "observed",
            "assertions",
            "policy",
        },
        "restart",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 2
        or receipt["kind"] != "go_closure_restart_storm"
        or receipt["status"] != "PASS"
        or not RUN_ID.fullmatch(receipt["run_id"] or "")
        or receipt["control_image"] != image
        or receipt["expected_commit"] != commit
    ):
        fail("restart identity or exact candidate binding is invalid")
    started, finished = validate_window(
        receipt["started_at"], receipt["finished_at"], "restart", checked_at
    )
    driver = receipt["agent_restart_driver"]
    exact_keys(
        driver,
        {"path", "sha256", "matches_operator_reviewed_sha256", "unchanged_during_run"},
        "restart.agent_restart_driver",
    )
    if (
        not PurePosixPath(driver["path"]).is_absolute()
        or not SHA256.fullmatch(driver["sha256"] or "")
        or driver["matches_operator_reviewed_sha256"] is not True
        or driver["unchanged_during_run"] is not True
    ):
        fail("restart driver is not immutable reviewed authority")

    observed = receipt["observed"]
    exact_keys(
        observed,
        {
            "control_restarts",
            "database_restarts",
            "storage_restarts",
            "alerting_restarts",
            "network_interruptions",
            "network_interruption_seconds_each",
            "agent_supervisor_action_receipt",
            "agent_sessions_before",
            "agent_sessions_after_restart",
            "agent_sessions_final",
        },
        "restart.observed",
    )
    for key, minimum in (
        ("control_restarts", 2),
        ("database_restarts", 1),
        ("storage_restarts", 1),
        ("alerting_restarts", 1),
        ("network_interruptions", 2),
    ):
        exact_integer(observed[key], f"restart.observed.{key}", minimum)
    exact_integer(
        observed["network_interruption_seconds_each"],
        "restart.observed.network_interruption_seconds_each",
        1,
        30,
    )
    before = validate_session_set(observed["agent_sessions_before"], "restart.sessions_before")
    after = validate_session_set(
        observed["agent_sessions_after_restart"], "restart.sessions_after_restart"
    )
    final = validate_session_set(observed["agent_sessions_final"], "restart.sessions_final")
    if set(before) != set(after) or set(after) != set(final):
        fail("restart session sets do not name the same two workers")
    start_epoch = int(started.timestamp())
    finish_epoch = int(finished.timestamp())
    for worker in sorted(before):
        if (
            before[worker]["agent_session_id"] == after[worker]["agent_session_id"]
            or after[worker]["agent_session_id"] != final[worker]["agent_session_id"]
            or after[worker]["session_started_epoch"] < start_epoch
            or after[worker]["last_seen_epoch"] < start_epoch
            or final[worker]["last_seen_epoch"] < start_epoch
            or final[worker]["last_seen_epoch"] > finish_epoch + 300
        ):
            fail("restart sessions do not prove exactly one fresh process transition per worker")

    action = observed["agent_supervisor_action_receipt"]
    validator = load_validator("validate-agent-restart-receipt.py")
    validator.validate(
        action,
        SimpleNamespace(
            run_id=receipt["run_id"],
            commit=commit,
            image=image,
            driver_sha256=driver["sha256"],
            approved_worker_ids=",".join(sorted(before)),
            run_started_at=receipt["started_at"],
            checked_at=receipt["finished_at"],
        ),
    )
    if receipt["assertions"] != {
        "control_restarts_at_least_2": True,
        "two_distinct_agents_restarted_from_database_session_transitions": True,
        "restarted_agents_remained_current_without_an_extra_restart": True,
        "recovered_after_each_fault_within_300_seconds": True,
        "retry_backoff_requires_correlated_scenario_audit": True,
    }:
        fail("restart assertions are incomplete")
    if receipt["policy"] != {
        "stripe_live_mode": False,
        "real_value": False,
        "secret_values_recorded": False,
    }:
        fail("restart safety policy is invalid")
    return started, finished


def expected_scenario_keys(scenario: str) -> set[str]:
    keys = {
        "schema_version",
        "scenario",
        "requested",
        "observed",
        "status",
        "binding",
        "started_at",
        "finished_at",
        "safety",
        "evidence",
    }
    if scenario == "stripe_test_matrix":
        keys |= {
            "provider_mode",
            "matrix_complete",
            "real_value",
            "application_outcomes_verified",
        }
    elif scenario == "real_alert_firing_resolution":
        keys.add("receiver_event_ids")
    elif scenario == "backup_independent_restore":
        keys |= {
            "encrypted_offsite_upload",
            "independent_download",
            "ciphertext_checksum_verified",
            "isolated_restore",
            "postgres_semantic_checks",
            "object_checks",
        }
    elif scenario == "post_rehearsal_invariant_audit":
        keys.add("invariants")
    elif scenario == "bounded_retry_backoff_audit":
        # backoff_schedule_within_policy is not a load-bearing key: it must not
        # appear without a measured requeue-delay observation.
        keys |= {
            "max_attempts_within_policy",
            "unbounded_retry_growth",
            "observed_task_count",
            "observed_max_retry_count",
        }
    return keys


def validate_canary(receipt, commit: str, image: str, checked_at: dt.datetime):
    exact_keys(
        receipt,
        {
            "schema_version",
            "kind",
            "status",
            "run_id",
            "started_at",
            "finished_at",
            "control_image",
            "expected_commit",
            "scenario_driver",
            "required_counts",
            "observations",
            "policy",
            "qualification",
        },
        "canary",
    )
    if (
        type(receipt["schema_version"]) is not int
        or receipt["schema_version"] != 2
        or receipt["kind"] != "go_closure_canary_rehearsal"
        or receipt["status"] != "PASS"
        or not RUN_ID.fullmatch(receipt["run_id"] or "")
        or receipt["control_image"] != image
        or receipt["expected_commit"] != commit
    ):
        fail("canary identity or exact candidate binding is invalid")
    started, finished = validate_window(
        receipt["started_at"], receipt["finished_at"], "canary", checked_at
    )
    driver = receipt["scenario_driver"]
    exact_keys(
        driver,
        {"path", "sha256", "matches_operator_reviewed_sha256", "unchanged_during_run"},
        "canary.scenario_driver",
    )
    if (
        not PurePosixPath(driver["path"]).is_absolute()
        or not SHA256.fullmatch(driver["sha256"] or "")
        or driver["matches_operator_reviewed_sha256"] is not True
        or driver["unchanged_during_run"] is not True
    ):
        fail("canary driver is not immutable reviewed authority")
    if receipt["required_counts"] != REQUIRED_COUNTS:
        fail("canary required counts are not the frozen scenario matrix")
    observations = receipt["observations"]
    exact_keys(
        observations,
        {
            "active_workers",
            "page_alerts_firing_after_rehearsal",
            "database_before",
            "database_after",
            "scenario_receipts",
        },
        "canary.observations",
    )
    if finite_number(observations["active_workers"], "canary.active_workers") < 2:
        fail("canary observed fewer than two active workers")
    if finite_number(
        observations["page_alerts_firing_after_rehearsal"], "canary.page_alerts"
    ) != 0:
        fail("canary finished with a firing page alert")
    validate_database_snapshot(observations["database_before"], "canary.database_before")
    validate_database_snapshot(
        observations["database_after"], "canary.database_after", require_zero_ledger=True
    )
    scenario_receipts = observations["scenario_receipts"]
    if (
        not isinstance(scenario_receipts, list)
        or [item.get("scenario") for item in scenario_receipts] != list(SCENARIOS)
    ):
        fail("canary scenario receipts are missing, duplicated, or reordered")
    validator = load_validator("validate-canary-scenario-receipt.py")
    previous_finish = started
    for scenario, scenario_receipt in zip(SCENARIOS, scenario_receipts):
        exact_keys(
            scenario_receipt,
            expected_scenario_keys(scenario),
            f"canary.scenario_receipts.{scenario}",
        )
        scenario_started, scenario_finished = validate_window(
            scenario_receipt["started_at"],
            scenario_receipt["finished_at"],
            f"canary.scenario_receipts.{scenario}",
            finished,
        )
        if scenario_started < previous_finish or scenario_finished > finished:
            fail("canary scenarios overlap, are reordered, or fall outside the rehearsal")
        previous_finish = scenario_finished
        validator.validate(
            scenario_receipt,
            SimpleNamespace(
                scenario=scenario,
                minimum=SCENARIO_MINIMUMS[scenario],
                run_id=receipt["run_id"],
                commit=commit,
                image=image,
                driver_sha256=driver["sha256"],
                run_started_at=receipt["started_at"],
                scenario_started_at=scenario_receipt["started_at"],
                checked_at=receipt["finished_at"],
            ),
        )
    if receipt["policy"] != {
        "stripe_test_mode": True,
        "stripe_live_mode": False,
        "real_value": False,
        "approved_participants_only": True,
        "secret_values_recorded": False,
    }:
        fail("canary safety policy is invalid")
    if receipt["qualification"] != {
        "workload_and_external_scenarios": True,
        "exact_run_candidate_driver_binding": True,
        "database_backed_scenarios_corroborated": True,
        "restart_rollback_and_24h_soak_receipts_required_separately": True,
    }:
        fail("canary qualification is incomplete")
    return started, finished


def validate_soak(receipt, root: Path, commit: str, image: str):
    validator = load_validator("validate-go-closure-soak-receipt.py")
    samples_ref = receipt.get("samples")
    if not isinstance(samples_ref, dict):
        fail("soak samples reference is missing")
    sample_path = validator.resolve_sample_path(root, samples_ref.get("path"))
    samples, digest = validator.load_samples(sample_path)
    validator.validate(
        receipt,
        samples,
        digest,
        SimpleNamespace(commit=commit, image=image),
    )
    if receipt["mode"] != "qualifying":
        fail("final evidence chain requires a qualifying 24-hour soak")
    return (
        parse_utc(receipt["started_at"], "soak.started_at"),
        parse_utc(receipt["finished_at"], "soak.finished_at"),
    )


def validate_governance(
    bundle, commit: str, soak_finished: dt.datetime, checked_at: dt.datetime
) -> dt.datetime:
    exact_keys(
        bundle,
        {"schema_version", "candidate_commit", "scope", "approvals", "exercises"},
        "governance",
    )
    if (
        type(bundle["schema_version"]) is not int
        or bundle["schema_version"] != 1
        or bundle["candidate_commit"] != commit
        or bundle["scope"] != "supervised_stripe_test_mode_private_canary"
    ):
        fail("governance bundle is not bound to this exact Level-B candidate")
    if not isinstance(bundle["approvals"], dict) or set(bundle["approvals"]) != APPROVAL_DOMAINS:
        fail("governance bundle does not contain the exact eight approval domains")
    approval_times = {}
    approval_keys = {
        "status",
        "approver",
        "organization",
        "reviewed_scope",
        "evidence_uri",
        "approved_at",
    }
    for domain, approval in bundle["approvals"].items():
        exact_keys(approval, approval_keys, f"governance.approvals.{domain}")
        if approval["status"] != "APPROVED":
            fail(f"governance approval {domain} is not APPROVED")
        bounded_text(approval["approver"], f"governance.{domain}.approver", 256)
        bounded_text(approval["organization"], f"governance.{domain}.organization", 256)
        bounded_text(approval["reviewed_scope"], f"governance.{domain}.reviewed_scope", 4096)
        bounded_text(approval["evidence_uri"], f"governance.{domain}.evidence_uri", 2048)
        if not GOVERNANCE_TIMESTAMP.fullmatch(approval["approved_at"] or ""):
            fail(f"governance.{domain}.approved_at is not canonical UTC")
        approval_times[domain] = parse_utc(
            approval["approved_at"], f"governance.{domain}.approved_at"
        )
    if not isinstance(bundle["exercises"], dict) or set(bundle["exercises"]) != EXERCISES:
        fail("governance bundle does not contain the exact five exercises")
    exercise_times = {}
    for exercise, record in bundle["exercises"].items():
        exact_keys(
            record,
            {"status", "evidence_uri", "completed_at"},
            f"governance.exercises.{exercise}",
        )
        if record["status"] != "PASS":
            fail(f"governance exercise {exercise} is not PASS")
        bounded_text(
            record["evidence_uri"], f"governance.{exercise}.evidence_uri", 2048
        )
        if not GOVERNANCE_TIMESTAMP.fullmatch(record["completed_at"] or ""):
            fail(f"governance.{exercise}.completed_at is not canonical UTC")
        exercise_times[exercise] = parse_utc(
            record["completed_at"], f"governance.{exercise}.completed_at"
        )
    release_at = approval_times["release_approval"]
    if (
        any(value > release_at for value in approval_times.values())
        or any(value > release_at for value in exercise_times.values())
        or release_at < soak_finished
        or release_at > checked_at
    ):
        fail("release approval does not follow every approval, exercise, and qualifying soak")
    return release_at


def validate(args):
    if not COMMIT.fullmatch(args.commit or "") or not IMAGE.fullmatch(args.image or ""):
        fail("expected candidate must be an exact commit and immutable image digest")
    root = Path(args.root).resolve(strict=True)
    if not root.is_dir():
        fail("evidence root must be a directory")
    checked_at = parse_utc(args.checked_at, "checked_at")
    now = dt.datetime.now(dt.timezone.utc)
    if abs(now - checked_at) > NOW_TOLERANCE:
        fail("checked_at must be within five minutes of the validator clock")
    not_before = checked_at - MAX_CHAIN_AGE

    paths = {
        name: resolve_under_root(root, getattr(args, name), name)
        for name in ("deploy", "rollback", "restart", "canary", "soak", "governance")
    }
    documents = {
        name: load_json(
            path,
            name,
            1_048_576 if name in {"soak", "governance"} else MAX_JSON_BYTES,
        )
        for name, path in paths.items()
    }

    deploy_window = validate_deploy(documents["deploy"], args.commit, args.image, checked_at)
    rollback_window = validate_rollback(
        documents["rollback"], root, args.commit, args.image, checked_at
    )
    restart_window = validate_restart(
        documents["restart"], args.commit, args.image, checked_at
    )
    canary_window = validate_canary(
        documents["canary"], args.commit, args.image, checked_at
    )
    soak_window = validate_soak(
        documents["soak"], root, args.commit, args.image
    )
    if soak_window[1] > checked_at:
        fail("soak finishes after checked_at")
    windows = (deploy_window, rollback_window, restart_window, canary_window, soak_window)
    if windows[0][0] < not_before:
        fail("evidence chain is older than the seven-day freshness window")
    for previous, current in zip(windows, windows[1:]):
        if current[0] < previous[1]:
            fail("evidence chain operations overlap or are reordered")
    if (
        documents["deploy"]["control_image_id"]
        != documents["soak"]["runtime"]["image_id"]
    ):
        fail("deploy and qualifying soak observed different candidate image IDs")
    release_at = validate_governance(
        documents["governance"], args.commit, soak_window[1], checked_at
    )

    return {
        "schema_version": 1,
        "kind": "merc_go_closure_evidence_chain_validation",
        "status": "PASS",
        "decision": "ELIGIBLE_FOR_SUPERVISED_LEVEL_B_PRIVATE_CANARY_REVIEW",
        "scope": "supervised_stripe_test_mode_private_canary",
        "candidate_commit": args.commit,
        "control_image": args.image,
        "validated_at": args.checked_at,
        "freshness": {
            "maximum_chain_age_seconds": int(MAX_CHAIN_AGE.total_seconds()),
            "deploy_started_at": documents["deploy"]["started_at"],
            "soak_finished_at": documents["soak"]["finished_at"],
            "release_approved_at": release_at.strftime("%Y-%m-%dT%H:%M:%SZ"),
        },
        "evidence_sha256": {
            name: sha256_file(path) for name, path in sorted(paths.items())
        },
        "authority": {
            "level_b_private_canary_only": True,
            "stripe_test_mode_only": True,
            "live_payment_activation": False,
            "unrestricted_public_access": False,
            "unrestricted_supplier_access": False,
        },
        "secret_values_printed": False,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--root", required=True)
    parser.add_argument("--commit", required=True)
    parser.add_argument("--image", required=True)
    parser.add_argument("--checked-at", required=True)
    parser.add_argument("--deploy", required=True)
    parser.add_argument("--rollback", required=True)
    parser.add_argument("--restart", required=True)
    parser.add_argument("--canary", required=True)
    parser.add_argument("--soak", required=True)
    parser.add_argument("--governance", required=True)
    args = parser.parse_args()
    try:
        result = validate(args)
    except (OSError, TypeError, ValueError) as exc:
        print(f"go-closure-evidence-chain: FAIL: {exc}", file=sys.stderr)
        return 1
    print(json.dumps(result, sort_keys=True, separators=(",", ":")))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
