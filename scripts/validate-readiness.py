#!/usr/bin/env python3
"""Fail-closed readiness: domain scores are derived from machine receipts only.

Hand-typed `earned` fields in ops/readiness.json are advisory and ignored.
Where a named receipt is missing or fails its content check, that receipt
contributes zero points. Live money / public launch must stay NO_GO_PROHIBITED.

Machine-reachable ceiling: with every currently wired local receipt present
and passing, the derived total is 84/100. The remaining 16 points have
receipt rows wired to external evidence paths under evidence/external/, but
those artifacts are absent today and their content checks refuse local or
paper substitutes — so the score stays 84 until real external work lands.
Operator steps for those points: docs/PROGRAMME.md § "Facet external action pack".
Do not loosen content checks to "make room".
"""

from __future__ import annotations

import datetime as dt
import hashlib
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any, Callable, Iterable

ROOT = Path(__file__).resolve().parents[1]

_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_COMMIT = re.compile(r"^[0-9a-f]{40}$")
_BACKUP_ID = re.compile(r"^[0-9]{8}T[0-9]{6}Z$")
_CONTAINER_ID = re.compile(r"^[0-9a-f]{64}$")
_IMAGE_ID = re.compile(r"^sha256:[0-9a-f]{64}$")
_IMMUTABLE_IMAGE = re.compile(
    r"^[A-Za-z0-9._:-]+(/[A-Za-z0-9._-]+)+@sha256:[0-9a-f]{64}$"
)
_RFC3339_Z = re.compile(r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")
_SECRET_SHAPED = re.compile(
    r"(sk_(?:test|live)_|rk_(?:test|live)_|pk_live_|whsec_|"
    r"AGE-SECRET-KEY-|AKIA[0-9A-Z]{12,})",
    re.IGNORECASE,
)
_PLACEHOLDER_TOKEN = re.compile(
    r"\b(example|placeholder|todo|tbd|n/?a|unknown|dummy|fake|sample|"
    r"lorem|test(?:er)?|self[- ]?approv|not executed|pending)\b",
    re.IGNORECASE,
)
_LOCAL_HOST_TOKEN = re.compile(
    r"(localhost|127\.0\.0\.1|0\.0\.0\.0|::1|example\.(com|org|net)|"
    r"invalid|harness|docker\.internal|nip\.io|sslip\.io|localtest)",
    re.IGNORECASE,
)


def fail(message: str) -> None:
    print(f"readiness: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def load_json(relative: str) -> Any:
    path = ROOT / relative
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None


def status_in(*allowed: str) -> Callable[[Any], bool]:
    allowed_set = {a.upper() for a in allowed}

    def check(doc: Any) -> bool:
        if not isinstance(doc, dict):
            return False
        status = str(doc.get("status", "")).upper()
        return status in allowed_set

    return check


def file_exists(_doc: Any) -> bool:
    return True


def auth_matrix_complete(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    routes = 0
    for route_class in doc.get("route_classes", []):
        routes += len(route_class.get("routes", []))
    # 126 after the two versioned UI composition reads (GET /v1/ui/v1/buy through
    # authBuyer, GET /v1/ui/v1/earn through authWorker) joined the reviewed matrix.
    # The count is a tripwire, not a fact about the world: it exists so that adding
    # a route forces someone to look at the matrix. It had gone stale at 118 while
    # two routes were already serving buyer traffic, which silently cost this
    # domain 11 readiness points and made `make ci` red. It went stale AGAIN at
    # 123 against a live 124, costing the same 11 points, and was only found by
    # re-deriving the score rather than trusting it — so if you are reading this
    # because the number moved, update BOTH this tripwire and
    # scripts/validate-authorization-matrix.py, and check they agree.
    return routes == 126 and doc.get("policy", {}).get("default") == "deny"


def technical_break_glass(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bg = doc.get("break_glass") if isinstance(doc, dict) else None
    return isinstance(bg, dict) and str(bg.get("status", "")).upper() == "PASS"


def technical_privacy(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("dsar", "deletion", "tombstone"):
        block = doc.get(section)
        if not isinstance(block, dict) or not any(block.values()):
            return False
    return True


def technical_tabletops(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    for section in ("support_tabletop", "security_tabletop"):
        block = doc.get(section)
        if not isinstance(block, dict) or str(block.get("status", "")).upper() != "PASS":
            return False
    return True


def soak_clean(doc: Any) -> bool:
    if not status_in("PASS")(doc):
        return False
    bounds = doc.get("observed_bounds") if isinstance(doc, dict) else None
    if not isinstance(bounds, dict):
        return False
    restarts = bounds.get("control_restart_count")
    if isinstance(restarts, dict):
        restart_max = restarts.get("max", 1)
    else:
        restart_max = restarts if isinstance(restarts, (int, float)) else 1
    oom = bounds.get("control_oom_samples", 1)
    return restart_max == 0 and oom == 0


def payment_simulated(doc: Any) -> bool:
    if not isinstance(doc, dict):
        return False
    status = str(doc.get("status", "")).upper()
    label = str(doc.get("evidence_label", "")).upper()
    return "SIMULATED" in status or label == "SIMULATED"


def alert_delivery_proven(doc: Any) -> bool:
    """Validate an observed Alertmanager fire/resolve delivery receipt.

    The receipt is deliberately stricter than the harness simulation: it must
    contain two observed sink payloads, matching alert fingerprints, and both
    Alertmanager states.  This earns the private technical-delivery point only;
    the external staffed paging receiver remains a separate release gate.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "alert_delivery" or str(doc.get("status", "")).upper() != "PASS":
        return False
    receiver = doc.get("receiver")
    delivery = doc.get("delivery")
    observations = doc.get("observations")
    if not isinstance(receiver, dict) or not isinstance(delivery, dict) or not isinstance(observations, dict):
        return False
    if receiver.get("transport") != "alertmanager_webhook" or receiver.get("secret_values_recorded") is not False:
        return False
    host = str(receiver.get("url_host", "")).strip().lower()
    if not host or "harness" in host or "example" in host:
        return False
    try:
        count = int(delivery.get("sink_event_count", 0))
    except (TypeError, ValueError):
        return False
    firing = observations.get("firing")
    resolved = observations.get("resolved")
    if not isinstance(firing, dict) or not isinstance(resolved, dict):
        return False
    firing_body = firing.get("body")
    resolved_body = resolved.get("body")
    if not isinstance(firing_body, dict) or not isinstance(resolved_body, dict):
        return False
    return (
        count >= 2
        and bool(str(delivery.get("firing_received_at", "")).strip())
        and bool(str(delivery.get("resolved_received_at", "")).strip())
        and str(delivery.get("firing_fingerprint", "")).strip()
        == str(delivery.get("resolved_fingerprint", "")).strip()
        and bool(str(delivery.get("firing_fingerprint", "")).strip())
        and str(firing_body.get("status", "")).lower() == "firing"
        and str(resolved_body.get("status", "")).lower() == "resolved"
    )


def _all_strings(value: Any) -> Iterable[str]:
    if isinstance(value, str):
        yield value
    elif isinstance(value, dict):
        for key, item in value.items():
            yield str(key)
            yield from _all_strings(item)
    elif isinstance(value, list):
        for item in value:
            yield from _all_strings(item)


def _has_secret_shaped(doc: Any) -> bool:
    return any(_SECRET_SHAPED.search(text) for text in _all_strings(doc))


def _is_rfc3339_z(value: Any) -> bool:
    if not isinstance(value, str) or not _RFC3339_Z.fullmatch(value):
        return False
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError:
        return False
    return parsed.utcoffset() == dt.timedelta(0)


def _parse_utc(value: str) -> dt.datetime | None:
    if not _is_rfc3339_z(value):
        return None
    try:
        return dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError:
        return None


def _nonempty_text(value: Any, minimum: int = 3) -> bool:
    if not isinstance(value, str):
        return False
    text = value.strip()
    # Reject leading/trailing whitespace and placeholder tokens.
    if len(text) < minimum or value != text:
        return False
    if _PLACEHOLDER_TOKEN.search(text):
        return False
    return True


def _is_public_staging_host(host: Any) -> bool:
    if not isinstance(host, str):
        return False
    host = host.strip().lower().rstrip(".")
    if not host or "." not in host or " " in host:
        return False
    if _LOCAL_HOST_TOKEN.search(host):
        return False
    if host.endswith(".local") or host.endswith(".internal"):
        return False
    labels = host.split(".")
    if any(not label or len(label) > 63 for label in labels):
        return False
    return True


def _is_s3_offsite_uri(value: Any) -> bool:
    if not isinstance(value, str) or not value.startswith("s3://"):
        return False
    if any(ch.isspace() for ch in value) or "?" in value or "#" in value:
        return False
    rest = value[len("s3://") :]
    if "/" not in rest:
        return False
    bucket, _, key = rest.partition("/")
    if not bucket or ".." in key.split("/"):
        return False
    if "@" in bucket or ":" in bucket:
        return False
    return bool(key)


def _truthy_map(block: Any, required: set[str]) -> bool:
    if not isinstance(block, dict):
        return False
    for key in required:
        if block.get(key) is not True:
            return False
    return True


def stripe_sandbox_matrix_proven(doc: Any) -> bool:
    """Stripe sandbox matrix with provider reconciliation (6 pts).

    Matches scripts/stripe-sandbox.sh matrix emission. Refuses simulated
    payment receipts, live mode, secret-shaped values, and incomplete scenario
    coverage. status:PASS alone is not enough.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "stripe_sandbox_matrix":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if str(doc.get("provider_mode", "")).lower() != "test":
        return False
    if str(doc.get("live_mode", "")).upper() != "PROHIBITED":
        return False
    if doc.get("secret_values_printed") is not False:
        return False
    if not _nonempty_text(doc.get("run_id"), minimum=8):
        return False
    currency = str(doc.get("settlement_currency", "")).strip().lower()
    if len(currency) != 3 or not currency.isalpha():
        return False

    payment_objects = doc.get("payment_objects")
    if not isinstance(payment_objects, dict):
        return False
    for key in (
        "authorization",
        "capture",
        "decline",
        "idempotency",
        "refunds",
        "transfer",
    ):
        if payment_objects.get(key) is not True:
            return False
    timeout = payment_objects.get("timeout")
    if not isinstance(timeout, dict):
        return False
    if timeout.get("client_deadline") is not True or timeout.get("idempotent_recovery") is not True:
        return False

    external = doc.get("external_scenarios")
    if not isinstance(external, dict):
        return False
    if str(external.get("status", "")).upper() != "PASS":
        return False
    if str(external.get("provider_mode", "")).lower() != "test":
        return False
    if external.get("secret_values_recorded") is not False:
        return False
    if str(external.get("live_mode", "")).upper() != "PROHIBITED":
        return False
    webhook = external.get("webhook")
    if not isinstance(webhook, dict):
        return False
    for key in (
        "endpoint_secrets_verified",
        "delivery",
        "staging_urls_exact",
        "application_outcomes_verified",
        "replay_idempotent",
        "out_of_order_safe",
        "distinct_endpoint_ids",
    ):
        if webhook.get(key) is not True:
            return False
    api_version = str(webhook.get("payload_api_version", "")).strip()
    if "2025-06-30" not in api_version:
        return False
    dispute = external.get("dispute")
    payout = external.get("payout")
    reconciliation = external.get("reconciliation")
    settlement = external.get("settlement")
    if not isinstance(dispute, dict) or not isinstance(payout, dict):
        return False
    if not isinstance(reconciliation, dict) or not isinstance(settlement, dict):
        return False
    if dispute.get("opened") is not True or dispute.get("resolved") is not True:
        return False
    for key in ("hold", "release", "failure", "reversal"):
        if payout.get(key) is not True:
            return False
    if reconciliation.get("clean") is not True:
        return False
    if str(settlement.get("currency", "")).strip().lower() != currency:
        return False
    # Real provider object identifiers from the matrix run (not inventable
    # as empty/true flags alone).
    for field, prefix in (
        ("payment_intent", "pi_"),
        ("charge", "ch_"),
        ("transfer", "tr_"),
        ("disputed_payment_intent", "pi_"),
        ("disputed_charge", "ch_"),
    ):
        value = str(external.get(field, "")).strip()
        if not value.startswith(prefix) or len(value) < len(prefix) + 8:
            return False
    if _has_secret_shaped(doc):
        return False
    # Refuse the local simulator shape even if someone renames its kind.
    if str(doc.get("evidence_label", "")).upper() == "SIMULATED":
        return False
    if "SIMULATED" in str(doc.get("status", "")).upper():
        return False
    return True


def qualifying_24h_soak_proven(doc: Any) -> bool:
    """Qualifying ≥24 h soak on persistent staging (3 pts).

    Requires the go_closure_soak schema, qualifying mode, wall-clock and
    requested duration ≥86400 s, immutable candidate binding, safety policy,
    and an independent re-validation of the retained raw sample stream via
    scripts/validate-go-closure-soak-receipt.py. Local 60 s / 300 s soaks and
    iteration mode cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if doc.get("schema_version") != 2:
        return False
    if str(doc.get("kind", "")) != "go_closure_soak":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if str(doc.get("mode", "")) != "qualifying":
        return False

    started = _parse_utc(str(doc.get("started_at", "")))
    finished = _parse_utc(str(doc.get("finished_at", "")))
    if started is None or finished is None or finished <= started:
        return False

    duration = doc.get("duration")
    if not isinstance(duration, dict):
        return False
    try:
        requested = int(duration["requested_seconds"])
        actual = int(duration["actual_seconds"])
        interval = int(duration["interval_seconds"])
        sample_count = int(duration["samples"])
    except (KeyError, TypeError, ValueError):
        return False
    if requested < 86400 or actual < 86400 or interval < 15 or interval > 900:
        return False
    if sample_count < 1 or actual < requested:
        return False
    wall = int((finished - started).total_seconds())
    if wall < actual or wall > actual + 300:
        return False

    qualification = doc.get("qualification")
    if not isinstance(qualification, dict):
        return False
    if qualification.get("qualifies_for_24h_gate") is not True:
        return False
    if qualification.get("reason") != "observed_at_least_86400_seconds":
        return False

    commit = str(doc.get("expected_commit", ""))
    image = str(doc.get("control_image", ""))
    if not _COMMIT.fullmatch(commit) or not _IMMUTABLE_IMAGE.fullmatch(image):
        return False

    runtime = doc.get("runtime")
    if not isinstance(runtime, dict):
        return False
    if not _CONTAINER_ID.fullmatch(str(runtime.get("container_id", ""))):
        return False
    if str(runtime.get("configured_image", "")) != image:
        return False
    if not _IMAGE_ID.fullmatch(str(runtime.get("image_id", ""))):
        return False
    try:
        restart_count = int(runtime.get("restart_count", -1))
    except (TypeError, ValueError):
        return False
    if restart_count != 0:
        return False

    assertions = doc.get("assertions")
    if not isinstance(assertions, dict) or not assertions:
        return False
    if not all(value is True for value in assertions.values()):
        return False
    for key in (
        "two_agents_continuously_present",
        "no_page_alerts",
        "no_webhook_dead_letters",
        "no_control_restarts_or_recreates",
        "no_stuck_terminal_jobs",
        "bounded_resource_growth",
        "raw_samples_independently_validated",
    ):
        if assertions.get(key) is not True:
            return False

    policy = doc.get("policy")
    if policy != {
        "stripe_test_mode": True,
        "stripe_live_mode": False,
        "real_value": False,
        "secret_values_recorded": False,
    }:
        return False

    samples = doc.get("samples")
    if not isinstance(samples, dict):
        return False
    samples_rel = str(samples.get("path", "")).strip()
    samples_sha = str(samples.get("sha256", "")).strip()
    if not samples_rel or not _SHA256.fullmatch(samples_sha):
        return False
    if samples_rel.startswith("/") or ".." in Path(samples_rel).parts:
        return False
    samples_path = ROOT / samples_rel
    if not samples_path.is_file() or samples_path.is_symlink():
        return False
    digest = hashlib.sha256()
    try:
        with samples_path.open("rb") as handle:
            for chunk in iter(lambda: handle.read(1 << 20), b""):
                digest.update(chunk)
    except OSError:
        return False
    if digest.hexdigest() != samples_sha:
        return False

    if _has_secret_shaped(doc):
        return False

    # Re-derive continuity / bounds from the sample stream. A hand-typed PASS
    # receipt without a corroborating 24 h JSONL cannot survive this.
    receipt_path = ROOT / "evidence/external/qualifying-soak-24h.json"
    if not receipt_path.is_file():
        return False
    validator = ROOT / "scripts/validate-go-closure-soak-receipt.py"
    if not validator.is_file():
        return False
    try:
        completed = subprocess.run(
            [
                sys.executable,
                str(validator),
                str(receipt_path),
                "--root",
                str(ROOT),
                "--commit",
                commit,
                "--image",
                image,
            ],
            capture_output=True,
            timeout=120,
            check=False,
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return completed.returncode == 0


def independent_offsite_backup_proven(doc: Any) -> bool:
    """Independent external offsite encrypted backup copy (2 pts).

    Matches scripts/backup.sh merc_offsite_backup_verification emission.
    Requires an s3:// offsite URI, independent re-download of both manifest
    and ciphertext, matching checksums, and encrypted-before-upload policy.
    Local-only restore receipts cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("kind", "")) != "merc_offsite_backup_verification":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    backup_id = str(doc.get("backup_id", ""))
    if not _BACKUP_ID.fullmatch(backup_id):
        return False
    offsite = doc.get("offsite_uri")
    if not _is_s3_offsite_uri(offsite):
        return False
    if not str(offsite).rstrip("/").endswith("/" + backup_id):
        return False
    if not _SHA256.fullmatch(str(doc.get("manifest_sha256", ""))):
        return False
    ciphertext = doc.get("ciphertext")
    if not isinstance(ciphertext, dict):
        return False
    manifest_sha = str(ciphertext.get("manifest_sha256", ""))
    downloaded_sha = str(ciphertext.get("downloaded_sha256", ""))
    if not _SHA256.fullmatch(manifest_sha) or not _SHA256.fullmatch(downloaded_sha):
        return False
    if manifest_sha != downloaded_sha:
        return False
    try:
        byte_count = int(ciphertext.get("bytes", 0))
    except (TypeError, ValueError):
        return False
    if byte_count <= 0:
        return False
    if not _is_rfc3339_z(doc.get("verified_at")):
        return False
    if not _truthy_map(
        doc.get("checks"),
        {
            "offsite_bundle_visible",
            "independent_manifest_download",
            "independent_ciphertext_download",
            "manifest_checksum_match",
            "ciphertext_checksum_match",
        },
    ):
        return False
    policy = doc.get("policy")
    if policy != {
        "encrypted_before_upload": True,
        "plaintext_uploaded": False,
        "secret_values_recorded": False,
    }:
        return False
    if _has_secret_shaped(doc):
        return False
    # Explicit refusal of local logical-restore stand-ins.
    if str(doc.get("external_offsite_restore", "")).upper() == "NOT EXECUTED":
        return False
    if str(doc.get("kind", "")) == "logical_independent_restore":
        return False
    return True


def external_offsite_restore_proven(doc: Any) -> bool:
    """External offsite independent restore (1 pt).

    Requires a restore from an independently downloaded s3:// offsite bundle
    into an isolated environment with observed integrity. The local
    logical-independent-restore receipt (external_offsite_restore:NOT EXECUTED)
    cannot pass.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "external_offsite_restore":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    backup_id = str(doc.get("backup_id", ""))
    if not _BACKUP_ID.fullmatch(backup_id):
        return False
    offsite = doc.get("offsite_uri")
    if not _is_s3_offsite_uri(offsite):
        return False
    if not str(offsite).rstrip("/").endswith("/" + backup_id):
        return False
    if not _SHA256.fullmatch(str(doc.get("ciphertext_sha256", ""))):
        return False
    if not _is_rfc3339_z(doc.get("completed_at")):
        return False

    for flag in (
        "independent_download",
        "ciphertext_checksum_verified",
        "decrypt_isolated",
        "source_environment_destroyed",
        "new_database_credentials",
        "new_object_credentials",
        "new_namespace",
    ):
        if doc.get(flag) is not True:
            return False

    integrity = doc.get("integrity")
    if not isinstance(integrity, dict):
        return False
    for flag in (
        "ciphertext_verified",
        "payload_checksums_verified",
        "corrupt_backup_rejected",
        "ledger_zero_sum",
        "artifact_sentinels_verified",
    ):
        if integrity.get(flag) is not True:
            return False
    semantics = integrity.get("database_semantics")
    if not isinstance(semantics, dict) or not semantics:
        return False
    try:
        object_count = int(integrity.get("object_count", -1))
    except (TypeError, ValueError):
        return False
    if object_count < 0:
        return False

    # Refuse any residual local-only marker if present.
    marker = str(doc.get("external_offsite_restore", "PASS")).upper()
    if marker in {"NOT EXECUTED", "NOT_EXECUTED", "LOCAL", "SIMULATED"}:
        return False
    if _has_secret_shaped(doc):
        return False
    # Cross-check the local restore ledger: it must no longer claim the
    # external offsite restore was skipped.
    local = load_json("evidence/autonomous/logical-independent-restore.json")
    if isinstance(local, dict):
        local_marker = str(local.get("external_offsite_restore", "")).upper()
        if local_marker in {"NOT EXECUTED", "NOT_EXECUTED", "PENDING", ""}:
            return False
        if local_marker not in {"PASS", "EXECUTED", "COMPLETE"}:
            return False
    return True


def external_staging_attack_proven(doc: Any) -> bool:
    """External staging attack rehearsal (1 pt).

    Hostile exercise against a real public TLS staging hostname — not the
    local technical security_tabletop already scored in technical-exercises.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "external_staging_attack_rehearsal":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if str(doc.get("surface", "")) != "persistent_staging_tls":
        return False

    target = doc.get("target")
    if not isinstance(target, dict):
        return False
    host = target.get("hostname")
    if not _is_public_staging_host(host):
        return False
    if str(target.get("scheme", "")).lower() != "https":
        return False
    url = str(target.get("url", "")).strip().lower()
    if not url.startswith("https://" + str(host).strip().lower()):
        return False
    if _LOCAL_HOST_TOKEN.search(url):
        return False

    findings = doc.get("findings")
    if not isinstance(findings, dict):
        return False
    # Critical negatives must be observed false; break-glass exercise true.
    if findings.get("cross_tenant_access") is not False:
        return False
    if findings.get("authz_bypass") is not False:
        return False
    if findings.get("break_glass_under_staging") is not True:
        return False

    observations = doc.get("observations")
    if not isinstance(observations, dict):
        return False
    started = _parse_utc(str(observations.get("started_at", "")))
    finished = _parse_utc(str(observations.get("finished_at", "")))
    if started is None or finished is None or finished <= started:
        return False
    try:
        request_count = int(observations.get("request_count", 0))
        routes = int(observations.get("distinct_routes_exercised", 0))
    except (TypeError, ValueError):
        return False
    if request_count < 5 or routes < 3:
        return False

    reviewer = doc.get("reviewer")
    if not isinstance(reviewer, dict):
        return False
    if not _nonempty_text(reviewer.get("name"), minimum=5):
        return False
    if not _nonempty_text(reviewer.get("organization"), minimum=3):
        return False

    # Local technical tabletop markers must not be accepted as external.
    if str(doc.get("qualification", "")).upper() in {"TECHNICAL", "LOCAL", "SIMULATED"}:
        return False
    if doc.get("technical_only") is True:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def _approval_record_qualified(record: Any) -> bool:
    if not isinstance(record, dict):
        return False
    if str(record.get("status", "")).upper() != "APPROVED":
        return False
    if not _nonempty_text(record.get("approver"), minimum=5):
        return False
    if not _nonempty_text(record.get("organization"), minimum=3):
        return False
    if not _nonempty_text(record.get("reviewed_scope"), minimum=12):
        return False
    evidence_uri = str(record.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    # Evidence must sit outside the repo's local technical receipts.
    lowered = evidence_uri.lower()
    if lowered.startswith("evidence/autonomous/"):
        return False
    if "technical-exercises" in lowered:
        return False
    if not _is_rfc3339_z(record.get("approved_at")):
        return False
    return True


def _exercise_record_pass(record: Any) -> bool:
    if not isinstance(record, dict):
        return False
    if str(record.get("status", "")).upper() != "PASS":
        return False
    evidence_uri = str(record.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    if evidence_uri.lower().startswith("evidence/autonomous/"):
        return False
    if not _is_rfc3339_z(record.get("completed_at")):
        return False
    return True


def privacy_qualified_approval_proven(doc: Any) -> bool:
    """Qualified privacy approval + external subprocessor deletion (1 pt).

    Local technical DSAR/deletion/tombstone already scores 3/4. This demands
    a named privacy authority approval and an executed external subprocessor
    deletion exercise — not technical-exercises.json.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "privacy_qualified_approval":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("scope", "")) != "supervised_stripe_test_mode_private_canary":
        return False
    commit = str(doc.get("candidate_commit", ""))
    if not _COMMIT.fullmatch(commit):
        return False
    if not _approval_record_qualified(doc.get("approval")):
        return False
    if not _exercise_record_pass(doc.get("dsar_export_deletion")):
        return False
    sub = doc.get("external_subprocessor_deletion")
    if not isinstance(sub, dict):
        return False
    if str(sub.get("status", "")).upper() != "PASS":
        return False
    if sub.get("executed") is not True:
        return False
    if not _nonempty_text(sub.get("subprocessor"), minimum=3):
        return False
    if not _is_rfc3339_z(sub.get("completed_at")):
        return False
    evidence_uri = str(sub.get("evidence_uri", "")).strip()
    if len(evidence_uri) < 12 or _PLACEHOLDER_TOKEN.search(evidence_uri):
        return False
    # Refuse the technical-exercises marker.
    if str(sub.get("status", "")).upper() in {"NOT EXECUTED", "NOT_EXECUTED"}:
        return False
    # Cross-check the live technical qualification ledger. The external receipt
    # alone must not award the point while technical-exercises still records
    # external_subprocessor_deletion as NOT EXECUTED.
    technical = load_json("evidence/autonomous/technical-exercises.json")
    if not isinstance(technical, dict):
        return False
    qualification = technical.get("qualification")
    if not isinstance(qualification, dict):
        return False
    live_sub = str(qualification.get("external_subprocessor_deletion", "")).upper()
    if live_sub in {"", "NOT EXECUTED", "NOT_EXECUTED", "PENDING"}:
        return False
    if live_sub not in {"PASS", "EXECUTED", "COMPLETE"}:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def licensing_provenance_approval_proven(doc: Any) -> bool:
    """License and asset/model provenance approval (1 pt).

    Requires a named licensing authority approval and a completed
    asset_and_model_provenance exercise with zero remaining BLOCKED rows.
    """
    if not isinstance(doc, dict):
        return False
    if str(doc.get("kind", "")) != "licensing_provenance_approval":
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("schema_version") != 1:
        return False
    if str(doc.get("scope", "")) != "supervised_stripe_test_mode_private_canary":
        return False
    commit = str(doc.get("candidate_commit", ""))
    if not _COMMIT.fullmatch(commit):
        return False
    if not _approval_record_qualified(doc.get("approval")):
        return False
    if not _exercise_record_pass(doc.get("asset_and_model_provenance")):
        return False
    register = doc.get("provenance_register")
    if not isinstance(register, dict):
        return False
    if str(register.get("status", "")).upper() != "APPROVED":
        return False
    try:
        blocked = int(register.get("blocked_rows_remaining", -1))
    except (TypeError, ValueError):
        return False
    if blocked != 0:
        return False
    if not _nonempty_text(register.get("register_uri"), minimum=12):
        return False
    # Refuse residual BLOCKED markers inside the receipt register summary.
    status_blob = " ".join(
        str(value) for value in _all_strings(register) if isinstance(value, str)
    ).upper()
    if "BLOCKED" in status_blob:
        return False
    # Cross-check the live in-repo provenance register. Planting only the
    # external receipt while ops/asset-provenance.json remains BLOCKED_* must
    # not award the point.
    live = load_json("ops/asset-provenance.json")
    if not isinstance(live, dict):
        return False
    live_status = str(live.get("status", "")).upper()
    if not live_status or live_status.startswith("BLOCKED") or live_status in {
        "PENDING",
        "NOT_EXECUTED",
        "NOT EXECUTED",
    }:
        return False
    if live_status not in {"APPROVED", "PASS", "COMPLETE", "CLEARED"}:
        return False
    if _has_secret_shaped(doc):
        return False
    return True


def staffed_abuse_route_or_tabletop_proven(doc: Any) -> bool:
    """Staffed human abuse route or qualified multi-role tabletop (1 pt).

    Local technical tabletops already score 1/2. This requires named humans
    on a real escalation path (or a timed multi-role tabletop) with
    non-placeholder contacts — not technical-exercises.json.
    """
    if not isinstance(doc, dict):
        return False
    kind = str(doc.get("kind", ""))
    if kind not in {"staffed_abuse_route", "qualified_human_abuse_tabletop"}:
        return False
    if str(doc.get("status", "")).upper() != "PASS":
        return False
    if doc.get("secret_values_recorded") is not False:
        return False
    if doc.get("technical_only") is True:
        return False
    if str(doc.get("qualification", "")).upper() in {
        "TECHNICAL",
        "LOCAL",
        "NOT EXECUTED",
        "NOT_EXECUTED",
    }:
        return False

    if kind == "staffed_abuse_route":
        route = doc.get("route")
        if not isinstance(route, dict):
            return False
        if not _nonempty_text(route.get("owner"), minimum=5):
            return False
        if not _nonempty_text(route.get("organization"), minimum=3):
            return False
        contact = str(route.get("contact", "")).strip()
        # Staffed contact: email-shaped, not a placeholder or example host.
        if "@" not in contact or contact.count("@") != 1:
            return False
        local, _, domain = contact.partition("@")
        if len(local) < 2 or not _is_public_staging_host(domain):
            return False
        if _PLACEHOLDER_TOKEN.search(contact):
            return False
        if not _nonempty_text(route.get("runbook_uri"), minimum=12):
            return False
        if not _is_rfc3339_z(route.get("staffed_since")):
            return False
        if route.get("human_on_call") is not True:
            return False
    else:
        tabletop = doc.get("tabletop")
        if not isinstance(tabletop, dict):
            return False
        started = _parse_utc(str(tabletop.get("started_at", "")))
        finished = _parse_utc(str(tabletop.get("finished_at", "")))
        if started is None or finished is None or finished <= started:
            return False
        # At least 30 minutes of wall clock — not a checkbox flip.
        if int((finished - started).total_seconds()) < 1800:
            return False
        roles = tabletop.get("roles")
        if not isinstance(roles, dict):
            return False
        required_roles = ("trust_and_safety", "support", "security")
        for role in required_roles:
            person = roles.get(role)
            if not isinstance(person, dict):
                return False
            if not _nonempty_text(person.get("name"), minimum=5):
                return False
            if not _nonempty_text(person.get("organization"), minimum=3):
                return False
        if tabletop.get("escalation_path_exercised") is not True:
            return False
        if tabletop.get("postmortem_required") is not True:
            return False
        if not _nonempty_text(tabletop.get("evidence_uri"), minimum=12):
            return False

    if _has_secret_shaped(doc):
        return False
    # Cross-check technical qualification ledger: a planted staffed-route
    # receipt must not score while technical-exercises still says the qualified
    # human tabletop was NOT EXECUTED.
    technical = load_json("evidence/autonomous/technical-exercises.json")
    if not isinstance(technical, dict):
        return False
    qualification = technical.get("qualification")
    if not isinstance(qualification, dict):
        return False
    live_human = str(qualification.get("qualified_human_tabletop", "")).upper()
    if live_human in {"", "NOT EXECUTED", "NOT_EXECUTED", "PENDING"}:
        return False
    if live_human not in {"PASS", "EXECUTED", "COMPLETE", "STAFFED"}:
        return False
    return True


# Each domain's possible points are fixed. earned is the sum of points for
# receipts that exist and pass their content check. Missing/failed => 0.
# External rows under evidence/external/ are wired so real operator artifacts
# can earn the reserved points; they evaluate to zero until those files exist
# and pass the content checks above.
DOMAIN_RECEIPTS: dict[str, dict[str, Any]] = {
    "source_and_ci": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/registry-verification.json", status_in("PASS"), 4),
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 3),
            ("ops/authorization-matrix.json", auth_matrix_complete, 3),
        ],
    },
    "security": {
        "possible": 15,
        "receipts": [
            ("ops/authorization-matrix.json", auth_matrix_complete, 8),
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 6),
            (
                "evidence/external/staging-attack-rehearsal.json",
                external_staging_attack_proven,
                1,
            ),
        ],
    },
    "money_and_reconciliation": {
        "possible": 15,
        "receipts": [
            ("evidence/autonomous/payment-simulator.json", payment_simulated, 9),
            (
                "evidence/external/stripe-sandbox-matrix.json",
                stripe_sandbox_matrix_proven,
                6,
            ),
        ],
    },
    "lifecycle_and_concurrency": {
        "possible": 10,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_break_glass, 5),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 5),
        ],
    },
    "artifacts_and_storage": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 6),
            (
                "evidence/external/offsite-backup-verification.json",
                independent_offsite_backup_proven,
                2,
            ),
        ],
    },
    "agent_and_sandbox": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/hardware-characterization.json", status_in("PASS"), 8),
        ],
    },
    "database_and_recovery": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/logical-independent-restore.json", status_in("PASS"), 4),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 3),
            (
                "evidence/external/offsite-independent-restore.json",
                external_offsite_restore_proven,
                1,
            ),
        ],
    },
    "deployment_and_rollback": {
        "possible": 8,
        "receipts": [
            ("evidence/autonomous/staging-validation.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-rollback.json", status_in("PASS"), 2),
            ("evidence/autonomous/local-restart-storm.json", status_in("PASS"), 1),
            ("evidence/autonomous/local-soak-60s.json", soak_clean, 0),
            (
                "evidence/external/qualifying-soak-24h.json",
                qualifying_24h_soak_proven,
                3,
            ),
        ],
    },
    "observability_and_alerting": {
        "possible": 6,
        "receipts": [
            ("evidence/autonomous/alert-pipeline-simulation.json", status_in("PASS"), 3),
            ("evidence/autonomous/alert-page-simulation.json", status_in("PASS"), 2),
            ("evidence/autonomous/alert-delivery-r1.json", alert_delivery_proven, 1),
        ],
    },
    "privacy_and_data_governance": {
        "possible": 4,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_privacy, 3),
            (
                "evidence/external/privacy-qualified-approval.json",
                privacy_qualified_approval_proven,
                1,
            ),
        ],
    },
    "licensing_and_supply_chain": {
        "possible": 3,
        "receipts": [
            ("evidence/autonomous/supply-chain.json", status_in("PASS"), 2),
            (
                "evidence/external/licensing-provenance-approval.json",
                licensing_provenance_approval_proven,
                1,
            ),
        ],
    },
    "abuse_and_trust": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
            (
                "evidence/external/staffed-abuse-route-or-tabletop.json",
                staffed_abuse_route_or_tabletop_proven,
                1,
            ),
        ],
    },
    "support_and_incident_response": {
        "possible": 1,
        "receipts": [
            ("evidence/autonomous/technical-exercises.json", technical_tabletops, 1),
        ],
    },
    "website_and_buyer_usability": {
        "possible": 2,
        "receipts": [
            ("evidence/autonomous/website-validation.json", status_in("PASS", "PASS_AUTOMATED_BROWSER"), 2),
        ],
    },
}


def derive_domain_score(domain_id: str) -> tuple[int, int, list[str]]:
    spec = DOMAIN_RECEIPTS[domain_id]
    possible = int(spec["possible"])
    earned = 0
    notes: list[str] = []
    for relative, checker, points in spec["receipts"]:
        path = ROOT / relative
        if not path.is_file():
            notes.append(f"{relative}: MISSING → 0/{points}")
            continue
        doc = load_json(relative) if relative.endswith(".json") else True
        if relative.endswith(".json") and doc is None:
            notes.append(f"{relative}: UNREADABLE → 0/{points}")
            continue
        if not checker(doc):
            notes.append(f"{relative}: CHECK_FAILED → 0/{points}")
            continue
        earned += int(points)
        notes.append(f"{relative}: OK → {points}/{points}")
    if earned > possible:
        fail(f"{domain_id}: derived earned {earned} exceeds possible {possible}")
    return earned, possible, notes


def main() -> None:
    readiness_path = ROOT / "ops" / "readiness.json"
    decision_path = ROOT / "ops" / "go-no-go.json"
    try:
        readiness = json.loads(readiness_path.read_text(encoding="utf-8"))
        decision = json.loads(decision_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        fail(f"cannot load readiness ledgers: {exc}")

    score = readiness.get("weighted_score") or {}
    domains = score.get("domains")
    if not isinstance(domains, list) or not domains:
        fail("weighted_score.domains must be a non-empty array")

    declared_ids = [item.get("id") for item in domains]
    expected_ids = list(DOMAIN_RECEIPTS.keys())
    if declared_ids != expected_ids:
        fail(
            "domain id axis mismatch "
            f"missing={sorted(set(expected_ids) - set(declared_ids))} "
            f"extra={sorted(set(declared_ids) - set(expected_ids))} "
            f"order_or_dup=declared_ids!=expected"
        )

    derived_total = 0
    possible_total = 0
    per_domain: list[str] = []
    for item in domains:
        domain_id = item["id"]
        declared_possible = int(item.get("possible", -1))
        earned, possible, notes = derive_domain_score(domain_id)
        if declared_possible != possible:
            fail(f"{domain_id}: possible {declared_possible} != receipt schedule {possible}")
        # Hand-typed earned is advisory only — never authoritative.
        advisory = item.get("earned")
        if advisory is not None and int(advisory) != earned:
            per_domain.append(
                f"{domain_id}: derived={earned}/{possible} "
                f"(advisory earned={advisory} ignored)"
            )
        else:
            per_domain.append(f"{domain_id}: derived={earned}/{possible}")
        for note in notes:
            if "MISSING" in note or "FAILED" in note or "UNREADABLE" in note:
                per_domain.append(f"  - {note}")
        derived_total += earned
        possible_total += possible

    if possible_total != 100:
        fail(f"domain possibles sum to {possible_total}, want 100")

    if decision.get("readiness_score") != derived_total:
        fail(
            f"decision readiness_score {decision.get('readiness_score')} "
            f"!= receipt-derived total {derived_total}"
        )

    open_p0 = decision.get("open_p0")
    open_p1 = decision.get("open_p1")
    if not isinstance(open_p0, list) or not isinstance(open_p1, list):
        fail("open_p0 and open_p1 must be arrays")

    severity = readiness.get("severity") or {}
    if severity.get("target_scope_open_p0") != len(open_p0):
        fail("open target-scope P0 count differs")
    if severity.get("target_scope_open_p1") != len(open_p1):
        fail("open target-scope P1 count differs")

    level_b = decision.get("decisions", {}).get("supervised_stripe_test_mode_private_canary")
    go_threshold = int(decision.get("go_threshold", score.get("go_threshold", 95)))
    if derived_total < go_threshold or open_p0 or open_p1:
        if level_b != "NO_GO":
            fail("an under-threshold or blocked Level B must be NO_GO")

    if decision.get("decisions", {}).get("live_money_or_public_launch") != "NO_GO_PROHIBITED":
        fail("live money/public launch must remain explicitly prohibited")

    if decision.get("machine_input_request") != "ops/go-closure-inputs.json":
        fail("decision must point to the single exact input request")

    print(
        f"readiness: PASS ({derived_total}/100 derived, P0={len(open_p0)}, "
        f"P1={len(open_p1)}, Level B {level_b})"
    )
    for line in per_domain:
        print(f"  {line}")


if __name__ == "__main__":
    main()
