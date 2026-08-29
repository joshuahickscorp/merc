#!/usr/bin/env python3
"""Check a *candidate* live-payment activation. Not a signer. Not an authorization.

Live money remains NO_GO_PROHIBITED. This script never writes an activation
record, never prints an HMAC key, and never talks to Stripe.

    python3 ops/scripts/validate-live-activation.py --self-test
    python3 ops/scripts/validate-live-activation.py \\
        --activation /absolute/private/candidate.json \\
        --hmac-key-file /absolute/private/hmac-key \\
        --commit "$(git rev-parse HEAD)"

The HMAC body is Go encoding/json of {schema_version, activation} as defined
in src/control/payment_authority.go. The self-test signs only an EXAMPLE envelope
with an EXAMPLE key.
"""

from __future__ import annotations

import argparse
import copy
import datetime as dt
import hashlib
import hmac
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


ROOT = Path(__file__).resolve().parents[2]
SCHEMA_REL = "ops/live-payment-activation.schema.json"

# Copied from src/control/payment_authority.go. Do not loosen.
MAX_ACTIVATION_DURATION = dt.timedelta(hours=72)
MAX_RECOVERY_DURATION = dt.timedelta(days=30)
MAX_SIGNING_LEAD = dt.timedelta(days=7)
MIN_HMAC_KEY_LEN = 32
MAX_HMAC_KEY_LEN = 4 << 10
MAX_ACTIVATION_BYTES = 64 << 10

ACTIVATION_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
HMAC_RE = re.compile(r"^[0-9a-f]{64}$")
CURRENCIES = frozenset({"cad", "usd", "jpy"})
REQUIRED_ROLES = ("payments", "release_manager", "security")
ENVELOPE_KEYS = frozenset({"schema_version", "activation", "hmac_sha256"})
ACTIVATION_KEYS = frozenset(
    {
        "activation_id",
        "candidate_commit",
        "environment",
        "currency",
        "valid_from",
        "expires_at",
        "recovery_expires_at",
        "max_single_charge_minor",
        "max_single_payout_minor",
        "max_single_refund_minor",
        "max_single_reversal_minor",
        "external_aggregate_cap_ref",
        "approvals",
    }
)
CAP_FIELDS = (
    "max_single_charge_minor",
    "max_single_payout_minor",
    "max_single_refund_minor",
    "max_single_reversal_minor",
)
APPROVAL_KEYS = frozenset({"role", "approver", "reference"})
# Go struct field order for the HMAC body (src/control/payment_authority.go).
ACTIVATION_HMAC_FIELDS = (
    "activation_id",
    "candidate_commit",
    "environment",
    "currency",
    "valid_from",
    "expires_at",
    "recovery_expires_at",
    "max_single_charge_minor",
    "max_single_payout_minor",
    "max_single_refund_minor",
    "max_single_reversal_minor",
    "external_aggregate_cap_ref",
    "approvals",
)

EXAMPLE_ACTIVATION_ID = "EXAMPLE-NOT-AN-AUTHORIZATION-0001"
EXAMPLE_MARKER = "EXAMPLE-NOT-AN-AUTHORIZATION"
EXAMPLE_HMAC_KEY = "example-only-not-an-authorization-hmac-key-do-not-install"
EXAMPLE_CAP_REF = "example-only/not-an-authorization/aggregate-cap"
EXAMPLE_APPROVERS = {
    "payments": ("payments@example.invalid", "EXAMPLE-NOT-AN-AUTHORIZATION-PAYMENTS"),
    "release_manager": ("release@example.invalid", "EXAMPLE-NOT-AN-AUTHORIZATION-RELEASE"),
    "security": ("security@example.invalid", "EXAMPLE-NOT-AN-AUTHORIZATION-SECURITY"),
}


class Refusal(Exception):
    """A candidate activation is malformed. Not an authorization failure."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(message)
        self.code = code
        self.message = message


def fail(message: str) -> None:
    print(f"validate-live-activation: FAIL: {message}", file=sys.stderr)
    raise SystemExit(1)


def object_without_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Refusal("duplicate-key", f"duplicate JSON key {key!r}")
        result[key] = value
    return result


def reject_constant(value: str):
    raise Refusal("non-finite", f"non-finite JSON number {value}")


def load_schema() -> dict[str, Any]:
    path = ROOT / SCHEMA_REL
    if path.is_file():
        raw = path.read_bytes()
    else:
        completed = subprocess.run(
            ["git", "show", f"HEAD:{SCHEMA_REL}"],
            cwd=ROOT,
            capture_output=True,
            check=False,
        )
        if completed.returncode != 0:
            fail(f"{SCHEMA_REL} is not on disk and not in HEAD")
        raw = completed.stdout
    try:
        schema = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        fail(f"{SCHEMA_REL} is not JSON: {exc}")
    if not isinstance(schema, dict):
        fail(f"{SCHEMA_REL} is not an object")
    return schema


def git_head() -> str:
    completed = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=ROOT,
        capture_output=True,
        text=True,
        check=False,
    )
    if completed.returncode != 0:
        fail("git rev-parse HEAD failed; pass --commit")
    commit = completed.stdout.strip()
    if not COMMIT_RE.fullmatch(commit):
        fail(f"git HEAD is not a 40-character lowercase SHA-1: {commit!r}")
    return commit


def parse_rfc3339(value: Any, field: str) -> tuple[dt.datetime, str]:
    if not isinstance(value, str) or not value.strip():
        raise Refusal("timestamps", f"{field} must be an RFC3339 date-time")
    text = value.strip()
    try:
        if text.endswith("Z"):
            parsed = dt.datetime.fromisoformat(text[:-1] + "+00:00")
        else:
            parsed = dt.datetime.fromisoformat(text)
    except ValueError as exc:
        raise Refusal("timestamps", f"{field} is not RFC3339: {exc}") from exc
    if parsed.tzinfo is None:
        raise Refusal("timestamps", f"{field} must include a timezone")
    return parsed.astimezone(dt.timezone.utc), text


def go_json_string(value: str) -> str:
    dumped = json.dumps(value, ensure_ascii=True)
    return dumped.replace("&", r"\u0026").replace("<", r"\u003c").replace(">", r"\u003e")


def go_rfc3339nano(parsed: dt.datetime, original: str) -> str:
    """Remarshal a timestamp the way Go encoding/json formats time.Time."""
    if original.endswith("Z") or parsed.tzinfo is dt.timezone.utc:
        instant = parsed.astimezone(dt.timezone.utc)
        suffix = "Z"
        year, month, day = instant.year, instant.month, instant.day
        hour, minute, second = instant.hour, instant.minute, instant.second
        micro = instant.microsecond
    else:
        instant = parsed
        offset = instant.utcoffset()
        if offset is None:
            raise Refusal("timestamps", "timestamp tzinfo lost its offset")
        total = int(offset.total_seconds())
        sign = "+" if total >= 0 else "-"
        total = abs(total)
        hh, rem = divmod(total, 3600)
        mm, _ss = divmod(rem, 60)
        suffix = f"{sign}{hh:02d}:{mm:02d}"
        year, month, day = instant.year, instant.month, instant.day
        hour, minute, second = instant.hour, instant.minute, instant.second
        micro = instant.microsecond
    base = f"{year:04d}-{month:02d}-{day:02d}T{hour:02d}:{minute:02d}:{second:02d}"
    if micro:
        nano = f"{micro * 1000:09d}".rstrip("0")
        return f"{base}.{nano}{suffix}"
    return f"{base}{suffix}"


def go_marshal_signed_body(envelope: dict[str, Any]) -> bytes:
    """HMAC body: Go json.Marshal of {schema_version, activation}."""
    activation = envelope.get("activation")
    if not isinstance(activation, dict):
        raise Refusal("schema", "activation must be an object")
    chunks = [
        '{"schema_version":',
        str(int(envelope["schema_version"])),
        ',"activation":{',
    ]
    first = True
    for field in ACTIVATION_HMAC_FIELDS:
        if field not in activation:
            continue
        if not first:
            chunks.append(",")
        first = False
        chunks.append(go_json_string(field))
        chunks.append(":")
        value = activation[field]
        if field in {"valid_from", "expires_at", "recovery_expires_at"}:
            parsed, original = parse_rfc3339(value, field)
            chunks.append(go_json_string(go_rfc3339nano(parsed, original)))
        elif field == "approvals":
            if not isinstance(value, list):
                raise Refusal("approvals", "approvals must be an array")
            chunks.append("[")
            for i, approval in enumerate(value):
                if i:
                    chunks.append(",")
                if not isinstance(approval, dict):
                    raise Refusal("approvals", "each approval must be an object")
                chunks.append("{")
                chunks.append('"role":')
                chunks.append(go_json_string(str(approval.get("role", ""))))
                chunks.append(',"approver":')
                chunks.append(go_json_string(str(approval.get("approver", ""))))
                chunks.append(',"reference":')
                chunks.append(go_json_string(str(approval.get("reference", ""))))
                chunks.append("}")
            chunks.append("]")
        elif field in CAP_FIELDS:
            if isinstance(value, bool) or not isinstance(value, int):
                raise Refusal("absent-caps", f"{field} must be an integer")
            chunks.append(str(value))
        else:
            chunks.append(go_json_string(str(value)))
    chunks.append("}}")
    return "".join(chunks).encode("utf-8")


def compute_hmac(envelope: dict[str, Any], key: str) -> str:
    body = go_marshal_signed_body(envelope)
    return hmac.new(key.encode("utf-8"), body, hashlib.sha256).hexdigest()


def load_hmac_key(path: Path) -> str:
    if not path.is_file():
        fail(f"HMAC key file is not a regular file: {path}")
    mode = path.stat().st_mode & 0o777
    if mode & 0o027:
        fail(
            "HMAC key file must not be group-writable or accessible to other users "
            f"(mode {mode:03o})"
        )
    raw = path.read_bytes()
    key = raw.decode("utf-8", errors="strict").strip()
    if not (MIN_HMAC_KEY_LEN <= len(key) <= MAX_HMAC_KEY_LEN):
        fail(
            f"HMAC key must contain {MIN_HMAC_KEY_LEN}..{MAX_HMAC_KEY_LEN} bytes, "
            f"got {len(key)}"
        )
    return key


def load_envelope_bytes(raw: bytes, label: str) -> dict[str, Any]:
    if len(raw) == 0 or len(raw) > MAX_ACTIVATION_BYTES:
        raise Refusal("size", f"{label} has an invalid size ({len(raw)} bytes)")
    try:
        parsed = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=object_without_duplicate_keys,
            parse_constant=reject_constant,
        )
    except Refusal:
        raise
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise Refusal("json", f"{label} is not strict UTF-8 JSON: {exc}") from exc
    if not isinstance(parsed, dict):
        raise Refusal("schema", f"{label} must be a JSON object")
    return parsed


def require_exact_keys(value: dict[str, Any], expected: frozenset[str], field: str) -> None:
    extra = set(value) - expected
    missing = expected - set(value)
    if extra:
        raise Refusal("schema", f"{field} has unexpected keys: {sorted(extra)}")
    if missing:
        raise Refusal("schema", f"{field} is missing keys: {sorted(missing)}")


def check_schema_pin(schema: dict[str, Any]) -> None:
    required = set(schema.get("required") or [])
    if required != set(ENVELOPE_KEYS):
        fail(f"{SCHEMA_REL} required envelope keys changed: {sorted(required)}")
    activation = (schema.get("properties") or {}).get("activation") or {}
    act_required = set(activation.get("required") or [])
    if act_required != set(ACTIVATION_KEYS):
        fail(f"{SCHEMA_REL} required activation keys changed: {sorted(act_required)}")


def validate_schema_shape(envelope: dict[str, Any]) -> None:
    require_exact_keys(envelope, ENVELOPE_KEYS, "envelope")
    if envelope.get("schema_version") != 1:
        raise Refusal(
            "schema",
            f"unsupported live payment activation schema version {envelope.get('schema_version')!r}",
        )
    hmac_hex = envelope.get("hmac_sha256")
    if not isinstance(hmac_hex, str) or not HMAC_RE.fullmatch(hmac_hex):
        raise Refusal("bad-hmac", "live payment activation HMAC is invalid")
    activation = envelope.get("activation")
    if not isinstance(activation, dict):
        raise Refusal("schema", "activation must be an object")
    missing_caps = [name for name in CAP_FIELDS if name not in activation]
    if missing_caps:
        raise Refusal(
            "absent-caps",
            "absent per-transaction caps: " + ", ".join(missing_caps),
        )
    require_exact_keys(activation, ACTIVATION_KEYS, "activation")


def validate_activation_fields(
    envelope: dict[str, Any],
    *,
    now: dt.datetime,
    commit: str,
    modified: bool,
    settlement_currency: str,
) -> dict[str, Any]:
    activation = envelope["activation"]
    activation_id = activation.get("activation_id")
    if not isinstance(activation_id, str) or not ACTIVATION_ID_RE.fullmatch(activation_id):
        raise Refusal("activation-id", "live payment activation_id is invalid")

    candidate = activation.get("candidate_commit")
    if not isinstance(candidate, str) or not COMMIT_RE.fullmatch(candidate):
        raise Refusal(
            "wrong-commit",
            "live payment candidate_commit must be a full lowercase Git SHA-1",
        )
    if modified or not COMMIT_RE.fullmatch(commit) or candidate != commit:
        raise Refusal(
            "wrong-commit",
            "live payment activation is not bound to this clean candidate: "
            f"activation={candidate} build={commit} modified={str(modified).lower()}",
        )

    if activation.get("environment") != "production":
        raise Refusal("environment", "live payment activation environment must be production")

    currency = activation.get("currency")
    if currency not in CURRENCIES:
        raise Refusal("currency", f"live payment activation currency {currency!r} is not supported")
    if currency != settlement_currency:
        raise Refusal(
            "currency",
            f"live payment activation currency {currency!r} must exactly match "
            f"normalized settlement currency {settlement_currency!r}",
        )

    valid_from, _ = parse_rfc3339(activation.get("valid_from"), "valid_from")
    expires_at, _ = parse_rfc3339(activation.get("expires_at"), "expires_at")
    recovery_expires_at, _ = parse_rfc3339(
        activation.get("recovery_expires_at"), "recovery_expires_at"
    )
    if not valid_from < expires_at or (expires_at - valid_from) > MAX_ACTIVATION_DURATION:
        raise Refusal(
            "window",
            f"live payment activation window must be positive and at most {MAX_ACTIVATION_DURATION}",
        )
    if recovery_expires_at < expires_at or (
        recovery_expires_at - expires_at
    ) > MAX_RECOVERY_DURATION:
        raise Refusal(
            "window",
            "live payment recovery window must end from 0 to "
            f"{MAX_RECOVERY_DURATION} after activation expiry",
        )
    if valid_from - now > MAX_SIGNING_LEAD:
        raise Refusal(
            "window",
            "live payment activation cannot be signed more than "
            f"{MAX_SIGNING_LEAD} before valid_from",
        )
    if now >= recovery_expires_at:
        raise Refusal(
            "expired",
            "live payment activation and recovery windows have expired",
        )

    for name in CAP_FIELDS:
        amount = activation.get(name)
        if isinstance(amount, bool) or not isinstance(amount, int) or amount < 1:
            raise Refusal("absent-caps", f"{name} must be positive")

    cap_ref = activation.get("external_aggregate_cap_ref")
    if not isinstance(cap_ref, str) or not (1 <= len(cap_ref) <= 512) or not cap_ref.strip():
        raise Refusal(
            "aggregate-cap",
            "external_aggregate_cap_ref is required; per-operation limits are not an aggregate cap",
        )

    approvals = activation.get("approvals")
    if not isinstance(approvals, list) or not (len(approvals) == 3):
        present = []
        if isinstance(approvals, list):
            present = [
                str(item.get("role", "")).lower().strip()
                for item in approvals
                if isinstance(item, dict)
            ]
        missing = [role for role in REQUIRED_ROLES if role not in present]
        if missing:
            raise Refusal(
                "missing-approval-role",
                f"live payment activation is missing {missing[0]} approval",
            )
        raise Refusal("approvals", "live payment activation requires exactly three approvals")

    seen: dict[str, bool] = {}
    required = {role: False for role in REQUIRED_ROLES}
    for approval in approvals:
        if not isinstance(approval, dict):
            raise Refusal("approvals", "each approval must be an object")
        extra = set(approval) - APPROVAL_KEYS
        missing_keys = APPROVAL_KEYS - set(approval)
        if extra or missing_keys:
            raise Refusal("approvals", "each approval requires only role, approver, and reference")
        role = str(approval.get("role", "")).lower().strip()
        if role not in required:
            raise Refusal("approvals", f"unsupported live payment approval role {approval.get('role')!r}")
        if seen.get(role):
            raise Refusal("approvals", f"duplicate live payment approval role {role!r}")
        approver = approval.get("approver")
        reference = approval.get("reference")
        if not isinstance(approver, str) or not (1 <= len(approver) <= 256) or not approver.strip():
            raise Refusal("approvals", f"live payment approval {role!r} requires approver and reference")
        if not isinstance(reference, str) or not (1 <= len(reference) <= 512) or not reference.strip():
            raise Refusal("approvals", f"live payment approval {role!r} requires approver and reference")
        seen[role] = True
        required[role] = True
    for role, present in required.items():
        if not present:
            raise Refusal(
                "missing-approval-role",
                f"live payment activation is missing {role} approval",
            )

    value_movement_active = valid_from <= now < expires_at
    recovery_active = valid_from <= now < recovery_expires_at
    return {
        "activation_id": activation_id,
        "candidate_commit": candidate,
        "currency": currency,
        "valid_from": valid_from,
        "expires_at": expires_at,
        "recovery_expires_at": recovery_expires_at,
        "value_movement_active": value_movement_active,
        "recovery_active": recovery_active,
        "not_yet_valid": now < valid_from,
        "example": EXAMPLE_MARKER in activation_id,
    }


def validate_hmac(envelope: dict[str, Any], key: str) -> None:
    if not (MIN_HMAC_KEY_LEN <= len(key) <= MAX_HMAC_KEY_LEN):
        raise Refusal(
            "bad-hmac",
            f"HMAC key must contain {MIN_HMAC_KEY_LEN}..{MAX_HMAC_KEY_LEN} bytes",
        )
    want = compute_hmac(envelope, key)
    got = str(envelope.get("hmac_sha256", "")).lower()
    if not HMAC_RE.fullmatch(got) or not hmac.compare_digest(want, got):
        raise Refusal("bad-hmac", "live payment activation HMAC does not match")


def validate_envelope(
    envelope: dict[str, Any],
    *,
    now: dt.datetime,
    commit: str,
    modified: bool,
    settlement_currency: str,
    hmac_key: str,
) -> dict[str, Any]:
    validate_schema_shape(envelope)
    details = validate_activation_fields(
        envelope,
        now=now,
        commit=commit,
        modified=modified,
        settlement_currency=settlement_currency,
    )
    validate_hmac(envelope, hmac_key)
    return details


def example_approvals() -> list[dict[str, str]]:
    return [
        {
            "role": role,
            "approver": EXAMPLE_APPROVERS[role][0],
            "reference": EXAMPLE_APPROVERS[role][1],
        }
        for role in REQUIRED_ROLES
    ]


def make_example_envelope(
    *,
    commit: str,
    now: dt.datetime,
    hmac_key: str,
) -> dict[str, Any]:
    valid_from = now - dt.timedelta(minutes=1)
    expires_at = now + dt.timedelta(hours=1)
    recovery_expires_at = now + dt.timedelta(hours=24)
    envelope: dict[str, Any] = {
        "schema_version": 1,
        "hmac_sha256": "e" * 64,
        "activation": {
            "activation_id": EXAMPLE_ACTIVATION_ID,
            "candidate_commit": commit,
            "environment": "production",
            "currency": "cad",
            "valid_from": valid_from.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "expires_at": expires_at.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "recovery_expires_at": recovery_expires_at.strftime("%Y-%m-%dT%H:%M:%SZ"),
            "max_single_charge_minor": 50,
            "max_single_payout_minor": 25,
            "max_single_refund_minor": 50,
            "max_single_reversal_minor": 25,
            "external_aggregate_cap_ref": EXAMPLE_CAP_REF,
            "approvals": example_approvals(),
        },
    }
    envelope["hmac_sha256"] = compute_hmac(envelope, hmac_key)
    return envelope


def sign_envelope(envelope: dict[str, Any], hmac_key: str) -> dict[str, Any]:
    signed = copy.deepcopy(envelope)
    signed["hmac_sha256"] = compute_hmac(signed, hmac_key)
    return signed


def rfc3339(moment: dt.datetime) -> str:
    return moment.astimezone(dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def expect_refusal(
    envelope: dict[str, Any],
    *,
    code: str,
    hmac_key: str,
    now: dt.datetime,
    commit: str,
    settlement_currency: str,
    modified: bool = False,
) -> str:
    try:
        validate_envelope(
            envelope,
            now=now,
            commit=commit,
            modified=modified,
            settlement_currency=settlement_currency,
            hmac_key=hmac_key,
        )
    except Refusal as exc:
        if exc.code != code:
            raise AssertionError(
                f"expected refusal {code!r}, got {exc.code!r}: {exc.message}"
            ) from exc
        return f"{exc.code}: {exc.message}"
    raise AssertionError(f"expected refusal {code!r}, but the envelope was accepted")


def self_test() -> int:
    schema = load_schema()
    check_schema_pin(schema)
    commit = git_head()
    now = dt.datetime(2026, 8, 17, 15, 0, 0, tzinfo=dt.timezone.utc)
    key = EXAMPLE_HMAC_KEY
    example = make_example_envelope(commit=commit, now=now, hmac_key=key)
    if EXAMPLE_MARKER not in example["activation"]["activation_id"]:
        fail("self-test example lost its EXAMPLE marker")
    if key == "" or "example-only" not in key:
        fail("self-test HMAC key is not the example key")

    details = validate_envelope(
        example,
        now=now,
        commit=commit,
        modified=False,
        settlement_currency="cad",
        hmac_key=key,
    )
    if not details["example"]:
        fail("well-formed example was not classified as an example")
    if not details["value_movement_active"]:
        fail("well-formed example is not inside its value-movement window")

    print("validate-live-activation: EXAMPLE (not an authorization)")
    print(json.dumps(example, indent=2, sort_keys=False))
    print(
        "validate-live-activation: PASS example "
        f"candidate_commit={details['candidate_commit']} "
        "hmac=verified authorization=false"
    )

    wrong = copy.deepcopy(example)
    wrong["activation"]["candidate_commit"] = "b" * 40
    wrong = sign_envelope(wrong, key)
    wrong_msg = expect_refusal(
        wrong, code="wrong-commit", hmac_key=key, now=now, commit=commit, settlement_currency="cad"
    )
    print(f"validate-live-activation: REFUSE {wrong_msg}")

    expired = copy.deepcopy(example)
    expired["activation"]["valid_from"] = rfc3339(now - dt.timedelta(hours=5))
    expired["activation"]["expires_at"] = rfc3339(now - dt.timedelta(hours=3))
    expired["activation"]["recovery_expires_at"] = rfc3339(now - dt.timedelta(hours=1))
    expired = sign_envelope(expired, key)
    expired_msg = expect_refusal(
        expired, code="expired", hmac_key=key, now=now, commit=commit, settlement_currency="cad"
    )
    print(f"validate-live-activation: REFUSE {expired_msg}")

    missing_role = copy.deepcopy(example)
    missing_role["activation"]["approvals"] = example_approvals()[:2]
    missing_role = sign_envelope(missing_role, key)
    missing_msg = expect_refusal(
        missing_role,
        code="missing-approval-role",
        hmac_key=key,
        now=now,
        commit=commit,
        settlement_currency="cad",
    )
    print(f"validate-live-activation: REFUSE {missing_msg}")

    absent_caps = copy.deepcopy(example)
    for name in CAP_FIELDS:
        del absent_caps["activation"][name]
    # HMAC is not recomputed: the body is already malformed. Either a schema
    # refusal or an HMAC mismatch is a refuse; the required code is absent-caps.
    absent_msg = expect_refusal(
        absent_caps,
        code="absent-caps",
        hmac_key=key,
        now=now,
        commit=commit,
        settlement_currency="cad",
    )
    print(f"validate-live-activation: REFUSE {absent_msg}")

    bad_hmac = copy.deepcopy(example)
    digest = bad_hmac["hmac_sha256"]
    tail = "0" if digest[-1] != "0" else "1"
    bad_hmac["hmac_sha256"] = digest[:-1] + tail
    bad_msg = expect_refusal(
        bad_hmac, code="bad-hmac", hmac_key=key, now=now, commit=commit, settlement_currency="cad"
    )
    print(f"validate-live-activation: REFUSE {bad_msg}")

    # Extra pins so a later schema change cannot silently drop a refusal.
    zero_caps = copy.deepcopy(example)
    zero_caps["activation"]["max_single_charge_minor"] = 0
    zero_caps = sign_envelope(zero_caps, key)
    expect_refusal(
        zero_caps, code="absent-caps", hmac_key=key, now=now, commit=commit, settlement_currency="cad"
    )
    dirty = expect_refusal(
        example,
        code="wrong-commit",
        hmac_key=key,
        now=now,
        commit=commit,
        settlement_currency="cad",
        modified=True,
    )
    if "modified=true" not in dirty:
        fail("modified candidate was not reported as modified")

    staged = copy.deepcopy(example)
    staged["activation"]["valid_from"] = rfc3339(now + dt.timedelta(hours=2))
    staged["activation"]["expires_at"] = rfc3339(now + dt.timedelta(hours=26))
    staged["activation"]["recovery_expires_at"] = rfc3339(now + dt.timedelta(hours=50))
    staged = sign_envelope(staged, key)
    staged_details = validate_envelope(
        staged,
        now=now,
        commit=commit,
        modified=False,
        settlement_currency="cad",
        hmac_key=key,
    )
    if not staged_details["not_yet_valid"] or staged_details["value_movement_active"]:
        fail("pre-staged activation inside the 7-day lead was not classified as not-yet-valid")

    too_early = copy.deepcopy(example)
    too_early["activation"]["valid_from"] = rfc3339(now + MAX_SIGNING_LEAD + dt.timedelta(seconds=1))
    too_early["activation"]["expires_at"] = rfc3339(
        now + MAX_SIGNING_LEAD + dt.timedelta(hours=1)
    )
    too_early["activation"]["recovery_expires_at"] = rfc3339(
        now + MAX_SIGNING_LEAD + dt.timedelta(hours=2)
    )
    too_early = sign_envelope(too_early, key)
    expect_refusal(
        too_early, code="window", hmac_key=key, now=now, commit=commit, settlement_currency="cad"
    )

    print("validate-live-activation: self-test PASS")
    print("validate-live-activation: authorization=false live_money=NO_GO_PROHIBITED")
    return 0


def parse_now(raw: str | None) -> dt.datetime:
    if not raw:
        return dt.datetime.now(dt.timezone.utc)
    parsed, _original = parse_rfc3339(raw, "--now")
    return parsed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Validate a candidate live-payment activation against the schema "
            "and the running candidate. Not a signer. Not an authorization."
        )
    )
    parser.add_argument("--self-test", action="store_true", help="run the example and refusal cases")
    parser.add_argument("--activation", type=Path, help="candidate activation JSON")
    parser.add_argument("--hmac-key-file", type=Path, help="permission-restricted HMAC key file")
    parser.add_argument(
        "--commit",
        help="40-character running candidate (default: git rev-parse HEAD)",
    )
    parser.add_argument("--now", help="RFC3339 clock for window checks (default: now UTC)")
    parser.add_argument(
        "--settlement-currency",
        default="cad",
        help="normalized settlement currency the candidate must match (default: cad)",
    )
    parser.add_argument(
        "--modified",
        action="store_true",
        help="treat the running candidate as a dirty build stamp (the binary would refuse)",
    )
    args = parser.parse_args(argv)

    if args.self_test:
        if args.activation or args.hmac_key_file:
            fail("--self-test does not take --activation or --hmac-key-file")
        return self_test()

    if args.activation is None or args.hmac_key_file is None:
        fail("provide --activation and --hmac-key-file, or pass --self-test")

    schema = load_schema()
    check_schema_pin(schema)
    commit = args.commit or git_head()
    if not COMMIT_RE.fullmatch(commit):
        fail("--commit must be a 40-character lowercase Git SHA-1")
    settlement = args.settlement_currency.strip().lower()
    if settlement not in CURRENCIES:
        fail("--settlement-currency must be cad, usd, or jpy")

    if not args.activation.is_file():
        fail(f"activation file is not a regular file: {args.activation}")
    raw = args.activation.read_bytes()
    try:
        envelope = load_envelope_bytes(raw, str(args.activation))
        hmac_key = load_hmac_key(args.hmac_key_file)
        details = validate_envelope(
            envelope,
            now=parse_now(args.now),
            commit=commit,
            modified=args.modified,
            settlement_currency=settlement,
            hmac_key=hmac_key,
        )
    except Refusal as exc:
        print(f"validate-live-activation: REFUSE {exc.code}: {exc.message}")
        print("validate-live-activation: authorization=false")
        return 1

    kind = "example" if details["example"] else "candidate"
    if details["example"]:
        print(
            "validate-live-activation: PASS example "
            "(not an authorization; do not install this record)"
        )
    else:
        print(f"validate-live-activation: PASS {kind}")
    print(f"candidate_commit={details['candidate_commit']}")
    print(f"activation_id={details['activation_id']}")
    print(f"value_movement_active={str(details['value_movement_active']).lower()}")
    print(f"recovery_active={str(details['recovery_active']).lower()}")
    print(f"not_yet_valid={str(details['not_yet_valid']).lower()}")
    print("authorization=false")
    print("live_money=NO_GO_PROHIBITED")
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Refusal as exc:
        print(f"validate-live-activation: REFUSE {exc.code}: {exc.message}", file=sys.stderr)
        raise SystemExit(1)
