#!/usr/bin/env python3
"""One-command Stripe Connect remainder, up to the API refusal.

Test-mode only. Never prints secret values. Never reads .merc-secrets.env.
Never synthesizes a tr_, acct_, po_, or we_. Does not write PASS for a
scenario that did not run.

Today POST /v1/accounts is the first call that requires Connect signup.
Every earlier step still runs. The receipt stays BLOCKED until Connect is real.
"""
from __future__ import annotations

import base64
import json
import os
import re
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

API_VERSION = os.environ.get("MERC_STRIPE_API_VERSION", "2025-06-30.basil")
CURRENCY = (os.environ.get("MERC_STRIPE_CANDIDATE_CURRENCY") or "cad").strip().lower()
COUNTRY = (os.environ.get("MERC_STRIPE_CANDIDATE_CONNECTED_COUNTRY") or "CA").strip().upper()
ACCOUNT_TYPE = os.environ.get("MERC_STRIPE_CONNECT_ACCOUNT_TYPE", "custom")
PAYOUT_ROUTING = os.environ.get("MERC_STRIPE_CANDIDATE_PAYOUT_ROUTING", "11000-000")
PAYOUT_SUCCESS = os.environ.get("MERC_STRIPE_CANDIDATE_PAYOUT_SUCCESS_ACCOUNT", "000123456789")
PAYOUT_FAILURE = os.environ.get("MERC_STRIPE_CANDIDATE_PAYOUT_FAILURE_ACCOUNT", "000111111116")
CONNECT_EVENTS = [
    item
    for item in (
        os.environ.get(
            "MERC_STRIPE_CONNECT_WEBHOOK_EVENTS",
            "account.updated,payout.created,payout.paid,payout.failed",
        ).split(",")
    )
    if item
]
COMMAND = os.environ.get("MERC_STRIPE_CONNECT_REMAINDER_COMMAND", "scripts/stripe-sandbox-connect.sh")
PLATFORM_ACCOUNT = "acct_1TxbzMCwPLrR4vaY"
CONNECT_PATH = "/v1/stripe/connect-webhook"
API_ROOT = "https://api.stripe.com/v1"
CTX = ssl.create_default_context()
PLACEHOLDER_ACCOUNT = "<connected_account>"
BLOCKED_MESSAGE = "blocked: Connect not signed up"
PASS = "PASS"
BLOCKED = "BLOCKED-ON-CONNECT"
FAILED = "FAILED"

CONNECT_SCENARIOS: tuple[tuple[str, str], ...] = (
    (
        "connected_account_creation",
        "A project-controlled Canadian test connected account exists and is distinct from the platform.",
    ),
    (
        "transfer_to_connected_account",
        "A CAD tr_ from the platform to a connected account appears in the provider log and Merc ledger.",
    ),
    (
        "payout_hold",
        "Manual payout schedule plus payout.created on the connected account holds supplier cash.",
    ),
    (
        "payout_manual_release",
        "A po_ to the documented Canadian success bank reaches paid.",
    ),
    (
        "payout_failure",
        "A po_ to the documented Canadian failure bank reaches failed.",
    ),
    (
        "connect_restriction_capability_events",
        "account.updated / capability restriction events on a connected account update supplier payout readiness.",
    ),
    (
        "connect_true_webhook_delivery",
        "Connected-account events are delivered to the Connect endpoint with connect=true and the Connect signing secret.",
    ),
)


def log(msg: str) -> None:
    print(f"stripe-connect: {msg}", file=sys.stderr)


def classify(value: str) -> str:
    if not value:
        return "missing"
    if value.startswith(("sk_live_", "rk_live_", "pk_live_")):
        return "live"
    if value.startswith(("sk_test_", "rk_test_")):
        return "test"
    if value.startswith("whsec_"):
        return "webhook"
    return "unknown"


def redact(value: str) -> str:
    if not value:
        return ""
    if value.startswith(("sk_", "rk_", "pk_", "whsec_")):
        return value[:8] + "…"
    return value


def die_live(variable: str) -> None:
    print(
        json.dumps(
            {
                "schema_version": 1,
                "kind": "stripe_connect_remainder",
                "status": "LIVE CREDENTIAL REFUSED",
                "provider_mode": "refused",
                "live_mode": "PROHIBITED",
                "secret_values_printed": False,
                "network_accessed": False,
                "refused_variable": variable,
            }
        )
    )
    raise SystemExit(1)


def err_of(doc: dict[str, Any]) -> dict[str, Any]:
    err = doc.get("error")
    return err if isinstance(err, dict) else {}


def connect_signup_error(status: int, doc: dict[str, Any]) -> str | None:
    if status != 400:
        return None
    message = str(err_of(doc).get("message") or "")
    if "signed up for Connect" in message:
        return message
    return None


def contains_secret(value: Any) -> str | None:
    if isinstance(value, str):
        # Prefix-only redactions (sk_test_…, whsec_Ab…) are allowed. A raw
        # token is longer and has no ellipsis.
        if re.search(r"(sk|rk)_(test|live)_[A-Za-z0-9]{12,}", value):
            return "sk/rk"
        if re.search(r"whsec_[A-Za-z0-9]{12,}", value):
            return "whsec_"
        return None
    if isinstance(value, dict):
        for item in value.values():
            found = contains_secret(item)
            if found:
                return found
        return None
    if isinstance(value, list):
        for item in value:
            found = contains_secret(item)
            if found:
                return found
    return None


class StripeClient:
    def __init__(self, key: str, version: str = API_VERSION) -> None:
        self.key = key
        self.version = version
        self.auth = "Basic " + base64.b64encode(key.encode() + b":").decode()

    def call(
        self,
        method: str,
        path: str,
        data: dict[str, Any] | None = None,
        *,
        stripe_account: str | None = None,
        idempotency: str | None = None,
        timeout: float = 45.0,
    ) -> tuple[int, dict[str, Any]]:
        url = f"{API_ROOT}/{path.lstrip('/')}"
        body = None
        headers = {
            "Authorization": self.auth,
            "Stripe-Version": self.version,
        }
        if stripe_account:
            headers["Stripe-Account"] = stripe_account
        if idempotency:
            headers["Idempotency-Key"] = idempotency
        if data is not None:
            body = urllib.parse.urlencode(data, doseq=True).encode()
            headers["Content-Type"] = "application/x-www-form-urlencoded"
        req = urllib.request.Request(url, data=body, method=method, headers=headers)
        try:
            with urllib.request.urlopen(req, context=CTX, timeout=timeout) as resp:
                raw = resp.read()
                parsed = json.loads(raw.decode() or "null")
                if not isinstance(parsed, dict):
                    parsed = {"_value": parsed}
                return resp.status, parsed
        except urllib.error.HTTPError as exc:
            raw = exc.read()
            try:
                parsed = json.loads(raw.decode() or "null")
            except json.JSONDecodeError:
                parsed = {"error": {"message": raw[:200].decode("utf-8", "replace")}}
            if not isinstance(parsed, dict):
                parsed = {"error": {"message": "unparsed provider error"}}
            return exc.code, parsed
        except TimeoutError:
            return 0, {"error": {"code": "client_timeout", "message": "client deadline exceeded"}}
        except urllib.error.URLError as exc:
            reason = str(getattr(exc, "reason", exc))
            if "timed out" in reason.lower():
                return 0, {"error": {"code": "client_timeout", "message": "client deadline exceeded"}}
            return 0, {"error": {"message": type(exc).__name__}}


def account_create_fields(run_id: str) -> dict[str, str]:
    return {
        "type": ACCOUNT_TYPE,
        "country": COUNTRY,
        "capabilities[card_payments][requested]": "true",
        "capabilities[transfers][requested]": "true",
        "metadata[cx_matrix_run]": run_id,
        "metadata[cx_purpose]": "connect_remainder",
    }


def dry_run_plans(hostname: str, run_id: str) -> list[dict[str, Any]]:
    connect_url = f"https://{hostname}{CONNECT_PATH}"
    return [
        {
            "id": "connected_account_creation",
            "method": "POST",
            "path": "/v1/accounts",
            "requires_connect": True,
            "fields": account_create_fields(run_id),
        },
        {
            "id": "transfer_to_connected_account",
            "method": "POST",
            "path": "/v1/transfers",
            "requires_connect": True,
            "fields": {
                "amount": "100",
                "currency": CURRENCY,
                "destination": PLACEHOLDER_ACCOUNT,
                "metadata[cx_matrix_run]": run_id,
            },
        },
        {
            "id": "payout_hold",
            "method": "POST",
            "path": f"/v1/accounts/{PLACEHOLDER_ACCOUNT}",
            "requires_connect": True,
            "fields": {"settings[payouts][schedule][interval]": "manual"},
        },
        {
            "id": "payout_manual_release",
            "method": "POST",
            "path": "/v1/payouts",
            "requires_connect": True,
            "stripe_account": PLACEHOLDER_ACCOUNT,
            "fields": {
                "amount": "40",
                "currency": CURRENCY,
                "external_account": {
                    "object": "bank_account",
                    "country": COUNTRY,
                    "currency": CURRENCY,
                    "routing_number": PAYOUT_ROUTING,
                    "account_number": PAYOUT_SUCCESS,
                },
            },
        },
        {
            "id": "payout_failure",
            "method": "POST",
            "path": "/v1/payouts",
            "requires_connect": True,
            "stripe_account": PLACEHOLDER_ACCOUNT,
            "fields": {
                "amount": "30",
                "currency": CURRENCY,
                "external_account": {
                    "object": "bank_account",
                    "country": COUNTRY,
                    "currency": CURRENCY,
                    "routing_number": PAYOUT_ROUTING,
                    "account_number": PAYOUT_FAILURE,
                },
            },
        },
        {
            "id": "connect_restriction_capability_events",
            "method": "GET",
            "path": f"/v1/accounts/{PLACEHOLDER_ACCOUNT}/capabilities",
            "requires_connect": True,
            "fields": {
                "follow_up_post": f"/v1/accounts/{PLACEHOLDER_ACCOUNT}",
                "business_profile[product_description]": f"merc connect remainder {run_id}",
            },
        },
        {
            "id": "connect_true_webhook_delivery",
            "method": "POST",
            "path": "/v1/webhook_endpoints",
            "requires_connect": False,
            "fields": {
                "url": connect_url,
                "connect": "true",
                "api_version": API_VERSION,
                "enabled_events": CONNECT_EVENTS,
                "description": f"merc connect remainder {run_id}",
            },
        },
    ]


def validate_dry_runs(plans: list[dict[str, Any]]) -> list[str]:
    errors: list[str] = []
    by_id = {item["id"]: item for item in plans}
    expected_ids = [item[0] for item in CONNECT_SCENARIOS]
    if [item["id"] for item in plans] != expected_ids:
        errors.append("dry-run plan order or ids drifted from CONNECT_SCENARIOS")

    create = by_id.get("connected_account_creation", {})
    fields = create.get("fields") if isinstance(create.get("fields"), dict) else {}
    if fields.get("type") != "custom":
        errors.append("account create type is not custom")
    if fields.get("country") != "CA":
        errors.append("account create country is not CA")
    if fields.get("capabilities[transfers][requested]") != "true":
        errors.append("account create does not request transfers")
    if fields.get("capabilities[card_payments][requested]") != "true":
        errors.append("account create does not request card_payments")

    transfer = by_id.get("transfer_to_connected_account", {})
    tfields = transfer.get("fields") if isinstance(transfer.get("fields"), dict) else {}
    if tfields.get("currency") != "cad":
        errors.append("transfer currency is not cad")
    if tfields.get("destination") != PLACEHOLDER_ACCOUNT:
        errors.append("transfer dry-run destination is not the placeholder")
    if isinstance(tfields.get("destination"), str) and tfields["destination"].startswith("acct_"):
        errors.append("transfer dry-run synthesized an acct_")

    release = by_id.get("payout_manual_release", {})
    rfields = release.get("fields") if isinstance(release.get("fields"), dict) else {}
    bank = rfields.get("external_account") if isinstance(rfields.get("external_account"), dict) else {}
    if bank.get("country") != "CA" or bank.get("currency") != "cad":
        errors.append("success bank is not CA/cad")
    if bank.get("routing_number") != "11000-000" or bank.get("account_number") != "000123456789":
        errors.append("success bank fixture drifted")

    failure = by_id.get("payout_failure", {})
    ffields = failure.get("fields") if isinstance(failure.get("fields"), dict) else {}
    fbank = ffields.get("external_account") if isinstance(ffields.get("external_account"), dict) else {}
    if fbank.get("account_number") != "000111111116":
        errors.append("failure bank fixture drifted")

    webhook = by_id.get("connect_true_webhook_delivery", {})
    wfields = webhook.get("fields") if isinstance(webhook.get("fields"), dict) else {}
    if wfields.get("connect") != "true":
        errors.append("webhook recreate does not pin connect=true")
    if wfields.get("api_version") != API_VERSION:
        errors.append("webhook recreate does not pin stripeAPIVersion")
    events = wfields.get("enabled_events")
    if not isinstance(events, list) or any(name not in events for name in CONNECT_EVENTS):
        errors.append("webhook recreate missing compiled Connect events")

    leaked = contains_secret(plans)
    if leaked:
        errors.append(f"dry-run plan leaked {leaked}")
    return errors


def wait_for(predicate, timeout: float, interval: float = 2.0):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval)
    return last


class ConnectRun:
    def __init__(self, run_id: str, hostname: str) -> None:
        self.run_id = run_id
        self.hostname = hostname
        self.currency = CURRENCY
        self.pre_connect: list[dict[str, Any]] = []
        self.scenarios: list[dict[str, Any]] = []
        self.notes: list[str] = []
        self.fixtures: dict[str, Any] = {
            "transfer": None,
            "payout_hold": None,
            "payout_release": None,
            "payout_failure": None,
            "payout_reversal": None,
            "connected_account": None,
        }
        self.stopped_at: dict[str, Any] | None = None
        self.webhook_probe: dict[str, Any] | None = None
        self.plans = dry_run_plans(hostname, run_id)

    def add(
        self,
        sid: str,
        status: str,
        *,
        fixture_id: str | None = None,
        detail: str = "",
        extra: dict[str, Any] | None = None,
    ) -> None:
        would = ""
        for name, prove in CONNECT_SCENARIOS:
            if name == sid:
                would = prove
                break
        row = {
            "id": sid,
            "status": status,
            "provider_mode": "test",
            "live_mode": "PROHIBITED",
            "fixture_id": fixture_id,
            "detail": detail,
            "would_prove": would,
        }
        if extra:
            row.update(extra)
        self.scenarios.append(row)
        log(f"{sid}: {status} fixture={fixture_id or '-'} {detail}")

    def record_pre(self, step: str, **fields: Any) -> None:
        row = {"step": step, **fields}
        self.pre_connect.append(row)
        log(f"pre {step}: {fields.get('detail') or fields.get('status') or 'ok'}")

    def stop_connect(self, method: str, path: str, status: int, doc: dict[str, Any], sent: dict[str, Any]) -> None:
        message = connect_signup_error(status, doc) or str(err_of(doc).get("message") or "")
        self.stopped_at = {
            "method": method,
            "path": path,
            "http": status,
            "error_type": err_of(doc).get("type"),
            "error_code": err_of(doc).get("code"),
            "error_message": message,
            "request_fields": sent,
        }
        print(BLOCKED_MESSAGE, file=sys.stderr)
        log(f"{BLOCKED_MESSAGE} at {method} {path} http={status} {message}")


def self_test() -> int:
    if API_VERSION != "2025-06-30.basil":
        log("self-test: unexpected API version")
        return 1
    if CURRENCY != "cad" or COUNTRY != "CA" or ACCOUNT_TYPE != "custom":
        log("self-test: candidate authority drifted")
        return 1
    plans = dry_run_plans("canary.example.invalid", "self-test")
    errors = validate_dry_runs(plans)
    if errors:
        for item in errors:
            log(f"self-test: {item}")
        return 1
    if redact("sk_test_abc123def") != "sk_test_…":
        log("self-test: redact drifted")
        return 1
    if PLACEHOLDER_ACCOUNT.startswith("acct_"):
        log("self-test: placeholder looks like an acct_")
        return 1
    fake = {"error": {"message": "You can only create new accounts if you've signed up for Connect, which you can do at https://dashboard.stripe.com/connect."}}
    if not connect_signup_error(400, fake):
        log("self-test: Connect signup error not recognized")
        return 1
    if connect_signup_error(200, {"id": "acct_x"}):
        log("self-test: false Connect wall")
        return 1
    if BLOCKED_MESSAGE != "blocked: Connect not signed up":
        log("self-test: blocked message drifted")
        return 1
    root = Path(__file__).resolve().parents[2]
    wrapper = (root / "scripts/stripe-sandbox-connect.sh").read_text()
    parent = (root / "scripts/stripe-sandbox.sh").read_text()
    if "scripts/lib/stripe-sandbox-contract.sh" not in wrapper:
        log("self-test: wrapper does not source the shared candidate authority")
        return 1
    if "refusing to read .merc-secrets.env" not in wrapper:
        log("self-test: wrapper does not refuse .merc-secrets.env")
        return 1
    if BLOCKED_MESSAGE not in wrapper:
        log("self-test: wrapper does not document the blocked exit")
        return 1
    if "check|matrix|nonconnect|connect" not in parent:
        log("self-test: stripe-sandbox.sh does not accept connect")
        return 1
    print("stripe-sandbox-connect: self-test PASS", file=sys.stderr)
    return 0


def load_matrix(path: Path) -> dict[str, Any]:
    if not path.is_file():
        return {
            "schema_version": 1,
            "kind": "stripe_sandbox_matrix",
            "status": "BLOCKED",
            "provider_mode": "test",
            "live_mode": "PROHIBITED",
            "secret_values_printed": False,
            "scenarios": [],
            "connect_gated_remainder": [],
            "fixtures": {},
        }
    doc = json.loads(path.read_text())
    if not isinstance(doc, dict) or doc.get("kind") != "stripe_sandbox_matrix":
        raise SystemExit("stripe-sandbox-connect: existing matrix is not kind=stripe_sandbox_matrix")
    return doc


def public_plan(plan: dict[str, Any], sent: bool = False) -> dict[str, Any]:
    fields = plan.get("fields")
    return {
        "id": plan["id"],
        "method": plan["method"],
        "path": plan["path"],
        "requires_connect": plan.get("requires_connect"),
        "stripe_account": plan.get("stripe_account"),
        "fields": fields,
        "dry_run_verified": True,
        "sent": sent,
    }


def merge_matrix(existing: dict[str, Any], run: ConnectRun) -> dict[str, Any]:
    by_id = {row["id"]: row for row in run.scenarios}
    attempted = {row["id"] for row in run.scenarios if row.get("attempted")}
    remainder = []
    for sid, would in CONNECT_SCENARIOS:
        row = by_id.get(sid, {})
        remainder.append(
            {
                "id": sid,
                "status": row.get("status") or BLOCKED,
                "would_prove": would,
                "detail": row.get("detail") or "",
                "fixture_id": row.get("fixture_id"),
                "dry_run_verified": True,
                "request": next((public_plan(plan, sent=sid in attempted) for plan in run.plans if plan["id"] == sid), None),
            }
        )

    scenarios = list(existing.get("scenarios") or [])
    replaced = {item["id"] for item in remainder}
    out_scenarios = []
    for row in scenarios:
        if isinstance(row, dict) and row.get("id") in replaced:
            merged = dict(row)
            fresh = by_id.get(str(row["id"]), {})
            # Never promote a row to PASS unless this run produced a real fixture.
            merged["status"] = fresh.get("status") or row.get("status") or BLOCKED
            if fresh.get("detail"):
                merged["detail"] = fresh["detail"]
            if fresh.get("fixture_id"):
                merged["fixture_id"] = fresh["fixture_id"]
            merged["dry_run_verified"] = True
            if fresh.get("would_prove"):
                merged["would_prove"] = fresh["would_prove"]
            out_scenarios.append(merged)
        else:
            out_scenarios.append(row)
    seen = {row.get("id") for row in out_scenarios if isinstance(row, dict)}
    for row in run.scenarios:
        if row["id"] not in seen:
            out_scenarios.append(row)

    fixtures = dict(existing.get("fixtures") or {})
    for key, value in run.fixtures.items():
        if value:
            fixtures[key] = value
        elif key not in fixtures:
            fixtures[key] = None

    payment_objects = dict(existing.get("payment_objects") or {})
    transfer_id = fixtures.get("transfer")
    payment_objects["transfer"] = isinstance(transfer_id, str) and transfer_id.startswith("tr_")

    external = dict(existing.get("external_scenarios") or {})
    if payment_objects["transfer"]:
        external["transfer"] = transfer_id
    else:
        external["transfer"] = existing.get("external_scenarios", {}).get("transfer") or ""
        if not isinstance(external.get("payout"), dict):
            external["payout"] = {
                "hold": False,
                "release": False,
                "failure": False,
                "reversal": False,
            }
    payout = dict(external.get("payout") or {})
    payout["hold"] = isinstance(fixtures.get("payout_hold"), str) and str(fixtures["payout_hold"]).startswith("po_")
    payout["release"] = isinstance(fixtures.get("payout_release"), str) and str(fixtures["payout_release"]).startswith("po_")
    payout["failure"] = isinstance(fixtures.get("payout_failure"), str) and str(fixtures["payout_failure"]).startswith("po_")
    payout["reversal"] = isinstance(fixtures.get("payout_reversal"), str) and str(fixtures["payout_reversal"]).startswith("po_")
    external["payout"] = payout
    if not payment_objects["transfer"]:
        external["status"] = "BLOCKED"

    connect_pass = all(by_id.get(sid, {}).get("status") == PASS for sid, _ in CONNECT_SCENARIOS)
    top_status = "PASS" if connect_pass and payment_objects["transfer"] and all(
        payout.get(key) is True for key in ("hold", "release", "failure", "reversal")
    ) else "BLOCKED"
    if top_status == "PASS":
        external["status"] = "PASS"

    harness = dict(existing.get("harness") or {})
    harness["connect_remainder_command"] = COMMAND
    harness["connect_remainder_alias"] = "scripts/stripe-sandbox.sh connect"
    harness["connect_remainder_status"] = top_status if connect_pass else BLOCKED
    harness["live_mode"] = "PROHIBITED"

    blocker = existing.get("blocker") if isinstance(existing.get("blocker"), dict) else {}
    if run.stopped_at:
        blocker = {
            "id": "connect_platform_not_signed_up",
            "detail": (
                f"{run.stopped_at['method']} {run.stopped_at['path']} on {PLATFORM_ACCOUNT} "
                f"returns: {run.stopped_at.get('error_message')}. "
                f"After dashboard signup, {COMMAND} is the one command that finishes "
                "the Connect remainder."
            ),
            "unreachable_in_test_mode_api": True,
            "stopped_at": run.stopped_at,
            "exit_reason": BLOCKED_MESSAGE,
        }

    notes = list(existing.get("notes") or [])
    for note in run.notes:
        if note not in notes:
            notes.append(note)
    command_note = f"Connect remainder command: {COMMAND} (alias: scripts/stripe-sandbox.sh connect)"
    if command_note not in notes:
        notes.append(command_note)

    validator = dict(existing.get("validator") or {})
    validator.setdefault("path", "scripts/validate-readiness.py:stripe_sandbox_matrix_proven")
    validator.setdefault(
        "accepts_only",
        "status=PASS plus transfer tr_ and payout hold/release/failure/reversal",
    )
    validator["this_receipt"] = (
        "honest PASS; Connect remainder completed"
        if top_status == "PASS"
        else "honest BLOCKED; expected CHECK_FAILED until Connect signup"
    )

    merged = dict(existing)
    merged.update(
        {
            "schema_version": 1,
            "kind": "stripe_sandbox_matrix",
            "status": top_status,
            "provider_mode": "test",
            "live_mode": "PROHIBITED",
            "secret_values_printed": False,
            "settlement_currency": existing.get("settlement_currency") or CURRENCY,
            "platform_account": existing.get("platform_account") or PLATFORM_ACCOUNT,
            "blocker": blocker,
            "harness": harness,
            "payment_objects": payment_objects,
            "fixtures": fixtures,
            "external_scenarios": external,
            "scenarios": out_scenarios,
            "connect_gated_remainder": remainder,
            "connect_remainder": {
                "command": COMMAND,
                "alias": "scripts/stripe-sandbox.sh connect",
                "run_id": run.run_id,
                "status": "PASS" if connect_pass else "BLOCKED",
                "exit_reason": None if connect_pass else BLOCKED_MESSAGE,
                "stopped_at": run.stopped_at,
                "pre_connect": run.pre_connect,
                "dry_runs": [public_plan(plan, sent=plan["id"] in attempted) for plan in run.plans],
                "webhook_recreate_probe": run.webhook_probe,
                "scenarios": run.scenarios,
                "secret_values_printed": False,
                "live_mode": "PROHIBITED",
            },
            "notes": notes,
            "validator": validator,
        }
    )
    leaked = contains_secret(merged)
    if leaked:
        raise SystemExit(f"stripe-sandbox-connect: refusing to write a receipt containing {leaked}")
    return merged


def drive(api: StripeClient, run: ConnectRun) -> str:
    errors = validate_dry_runs(run.plans)
    if errors:
        for item in errors:
            log(item)
        return FAILED

    status, account = api.call("GET", "account")
    acct_id = str(account.get("id") or "")
    if status != 200 or not acct_id.startswith("acct_"):
        run.record_pre("GET /v1/account", status="FAILED", http=status, detail="platform account unreadable")
        return FAILED
    if account.get("livemode") is True:
        run.record_pre("GET /v1/account", status="FAILED", http=status, detail="livemode true refused")
        return FAILED
    if acct_id != PLATFORM_ACCOUNT:
        run.record_pre(
            "GET /v1/account",
            status="FAILED",
            http=status,
            id=acct_id,
            detail="unexpected platform account",
        )
        return FAILED
    run.record_pre(
        "GET /v1/account",
        status="ok",
        http=status,
        id=acct_id,
        country=account.get("country"),
        default_currency=account.get("default_currency"),
        charges_enabled=account.get("charges_enabled"),
        payouts_enabled=account.get("payouts_enabled"),
        detail=f"country={account.get('country')} currency={account.get('default_currency')}",
    )

    status, balance = api.call("GET", "balance")
    currencies = sorted(
        {
            str(item.get("currency"))
            for item in (balance.get("available") or []) + (balance.get("pending") or [])
            if item.get("currency")
        }
    )
    if status != 200 or balance.get("livemode") is True or CURRENCY not in currencies:
        run.record_pre("GET /v1/balance", status="FAILED", http=status, currencies=currencies)
        return FAILED
    run.record_pre("GET /v1/balance", status="ok", http=status, currencies=currencies, detail=f"enabled={currencies}")

    status, endpoints = api.call("GET", "webhook_endpoints?limit=100")
    connect_url = f"https://{run.hostname}{CONNECT_PATH}"
    connect_ep = None
    billing_ep = None
    for ep in endpoints.get("data") or []:
        if not isinstance(ep, dict):
            continue
        if ep.get("url") == connect_url:
            connect_ep = ep
        elif str(ep.get("url") or "").endswith("/v1/stripe/webhook"):
            billing_ep = ep
    if status != 200:
        run.record_pre("GET /v1/webhook_endpoints", status="FAILED", http=status)
        return FAILED
    run.record_pre(
        "GET /v1/webhook_endpoints",
        status="ok",
        http=status,
        billing_id=billing_ep.get("id") if isinstance(billing_ep, dict) else None,
        connect_id=connect_ep.get("id") if isinstance(connect_ep, dict) else None,
        connect_flag=connect_ep.get("connect") if isinstance(connect_ep, dict) else None,
        connect_api_version=connect_ep.get("api_version") if isinstance(connect_ep, dict) else None,
        detail=(
            f"connect={connect_ep.get('id') if connect_ep else None} "
            f"flag={connect_ep.get('connect') if connect_ep else None} "
            f"api_version={connect_ep.get('api_version') if connect_ep else None}"
        ),
    )
    if isinstance(connect_ep, dict) and connect_ep.get("connect") is not True:
        run.notes.append(
            "Existing Connect endpoint still has connect!=true; "
            f"{COMMAND} will recreate it with connect=true after signup."
        )

    status, listed = api.call("GET", "accounts?limit=10")
    signup = connect_signup_error(status, listed)
    if signup:
        run.record_pre("GET /v1/accounts", status="blocked", http=status, detail=signup)
        run.stop_connect("GET", "/v1/accounts", status, listed, {"limit": "10"})
        _block_remaining(run, signup)
        return BLOCKED
    listed_ids = [
        str(item.get("id"))
        for item in (listed.get("data") or [])
        if isinstance(item, dict) and str(item.get("id") or "").startswith("acct_")
    ]
    run.record_pre(
        "GET /v1/accounts",
        status="ok",
        http=status,
        count=len(listed_ids),
        detail=f"listed {len(listed_ids)} connected account id(s); none synthesized",
    )

    probe = _probe_connect_webhook(api, run)
    run.webhook_probe = probe
    if probe.get("stopped"):
        return BLOCKED

    create_fields = account_create_fields(run.run_id)
    status, created = api.call("POST", "accounts", create_fields)
    signup = connect_signup_error(status, created)
    if signup:
        run.stop_connect("POST", "/v1/accounts", status, created, create_fields)
        run.add(
            "connected_account_creation",
            BLOCKED,
            detail=signup,
            extra={
                "attempted": True,
                "http": status,
                "path": "/v1/accounts",
                "method": "POST",
                "dry_run_verified": True,
            },
        )
        _block_remaining(run, f"requires Connect signup on {PLATFORM_ACCOUNT}")
        return BLOCKED

    created_id = str(created.get("id") or "")
    if status != 200 or not created_id.startswith("acct_") or created_id == PLATFORM_ACCOUNT:
        run.add(
            "connected_account_creation",
            FAILED,
            detail=f"http={status} {err_of(created).get('message') or created_id}",
        )
        _fail_remaining(run, "connected account was not created")
        return FAILED
    if created.get("country") != COUNTRY:
        run.add(
            "connected_account_creation",
            FAILED,
            fixture_id=created_id,
            detail=f"country={created.get('country')} wanted {COUNTRY}",
        )
        _fail_remaining(run, "connected account country is not CA")
        return FAILED
    run.fixtures["connected_account"] = created_id
    run.add(
        "connected_account_creation",
        PASS,
        fixture_id=created_id,
        detail=f"type={created.get('type')} country={created.get('country')}",
    )
    return _drive_remainder(api, run, created_id, connect_ep)


def _block_remaining(run: ConnectRun, detail: str, attempted: str | None = None) -> None:
    for sid, _would in CONNECT_SCENARIOS:
        if any(row["id"] == sid for row in run.scenarios):
            continue
        extra = {"dry_run_verified": True, "not_sent": True}
        if attempted == sid:
            extra["attempted"] = True
        run.add(sid, BLOCKED, detail=detail, extra=extra)


def _fail_remaining(run: ConnectRun, detail: str) -> None:
    for sid, _would in CONNECT_SCENARIOS:
        if any(row["id"] == sid for row in run.scenarios):
            continue
        run.add(sid, FAILED, detail=detail, extra={"dry_run_verified": True})


def _probe_connect_webhook(api: StripeClient, run: ConnectRun) -> dict[str, Any]:
    probe_url = f"https://{run.hostname}{CONNECT_PATH}-probe/{run.run_id}"
    fields: dict[str, Any] = {
        "url": probe_url,
        "connect": "true",
        "api_version": API_VERSION,
        "description": f"merc connect remainder probe {run.run_id}",
        "enabled_events[]": list(CONNECT_EVENTS),
    }
    status, created = api.call("POST", "webhook_endpoints", fields)
    signup = connect_signup_error(status, created)
    if signup:
        run.stop_connect("POST", "/v1/webhook_endpoints", status, created, {"connect": "true", "api_version": API_VERSION})
        _block_remaining(run, signup)
        return {"stopped": True, "http": status, "error": signup}

    probe_id = str(created.get("id") or "")
    secret_present = bool(created.get("secret"))
    returned_connect = created.get("connect")
    returned_version = created.get("api_version")
    deleted = False
    if probe_id.startswith("we_"):
        del_status, _deleted = api.call("DELETE", f"webhook_endpoints/{probe_id}")
        deleted = del_status == 200 and (_deleted.get("deleted") is True or _deleted.get("id") == probe_id)
        if not deleted:
            run.notes.append(f"probe endpoint {probe_id} delete http={del_status}; operator should remove it")

    detail = (
        f"requested connect=true api_version={API_VERSION}; "
        f"returned connect={returned_connect!r} api_version={returned_version} "
        f"deleted={deleted} secret_present={secret_present}"
    )
    run.record_pre(
        "POST /v1/webhook_endpoints (connect=true probe)",
        status="ok" if status == 200 else "FAILED",
        http=status,
        id=probe_id if probe_id.startswith("we_") else None,
        connect_flag=returned_connect,
        api_version=returned_version,
        deleted=deleted,
        detail=detail,
    )
    return {
        "stopped": False,
        "http": status,
        "probe_url": probe_url,
        "probe_id": probe_id if probe_id.startswith("we_") else None,
        "requested_connect": True,
        "requested_api_version": API_VERSION,
        "returned_connect": returned_connect,
        "returned_api_version": returned_version,
        "secret_present": secret_present,
        "deleted": deleted,
        "detail": detail,
    }


def _drive_remainder(api: StripeClient, run: ConnectRun, connected: str, connect_ep: dict[str, Any] | None) -> str:
    # Recreate the real Connect webhook first so later events can land on it.
    webhook_status = _recreate_connect_webhook(api, run, connect_ep)

    tos = api.call(
        "POST",
        f"accounts/{connected}",
        {
            "business_type": "individual",
            "business_profile[url]": f"https://{run.hostname}",
            "business_profile[mcc]": "5734",
            "tos_acceptance[date]": str(int(time.time())),
            "tos_acceptance[ip]": "127.0.0.1",
            "individual[first_name]": "Jenny",
            "individual[last_name]": "Rosen",
            "individual[email]": f"connect-remainder-{run.run_id}@example.invalid",
            "individual[dob][day]": "1",
            "individual[dob][month]": "1",
            "individual[dob][year]": "1901",
            "individual[address][line1]": "address_full_match",
            "individual[address][city]": "Toronto",
            "individual[address][state]": "ON",
            "individual[address][postal_code]": "M4B 1B3",
            "individual[address][country]": COUNTRY,
        },
    )
    log(f"tos/kyc http={tos[0]} payouts_enabled={tos[1].get('payouts_enabled')}")

    status, transfer = api.call(
        "POST",
        "transfers",
        {
            "amount": "100",
            "currency": CURRENCY,
            "destination": connected,
            "metadata[cx_matrix_run]": run.run_id,
        },
        idempotency=f"merc-connect-{run.run_id}-transfer",
    )
    transfer_id = str(transfer.get("id") or "")
    if (
        status == 200
        and transfer_id.startswith("tr_")
        and transfer.get("livemode") is False
        and transfer.get("currency") == CURRENCY
        and transfer.get("destination") == connected
    ):
        run.fixtures["transfer"] = transfer_id
        run.add("transfer_to_connected_account", PASS, fixture_id=transfer_id, detail=f"100 {CURRENCY} -> {connected}")
    else:
        run.add(
            "transfer_to_connected_account",
            FAILED,
            detail=f"http={status} {err_of(transfer).get('message')}",
        )

    status, manual = api.call(
        "POST",
        f"accounts/{connected}",
        {"settings[payouts][schedule][interval]": "manual"},
    )
    interval = ((manual.get("settings") or {}).get("payouts") or {}).get("schedule", {}).get("interval")
    if status != 200 or interval != "manual":
        run.add("payout_hold", FAILED, fixture_id=connected, detail=f"http={status} interval={interval}")
    else:
        success_bank = _add_bank(api, connected, PAYOUT_SUCCESS)
        failure_bank = _add_bank(api, connected, PAYOUT_FAILURE)
        released = None
        if success_bank:
            released = _create_payout(api, connected, 40, success_bank, f"merc-connect-{run.run_id}-release")
        if isinstance(released, dict) and str(released.get("id") or "").startswith("po_"):
            payout_id = str(released["id"])
            run.fixtures["payout_hold"] = payout_id
            run.fixtures["payout_release"] = payout_id
            run.add("payout_hold", PASS, fixture_id=payout_id, detail="manual schedule; payout.created object created")
            paid = _wait_payout(api, connected, payout_id, "paid")
            if paid:
                run.add("payout_manual_release", PASS, fixture_id=payout_id, detail="status=paid")
                rev_status, reversed = api.call("POST", f"payouts/{payout_id}/reverse", stripe_account=connected)
                rev_id = str(reversed.get("id") or "")
                if rev_status == 200 and rev_id.startswith("po_") and reversed.get("amount", 0) < 0:
                    run.fixtures["payout_reversal"] = rev_id
                else:
                    run.notes.append(f"payout reverse http={rev_status} {err_of(reversed).get('message')}")
            else:
                run.add("payout_manual_release", FAILED, fixture_id=payout_id, detail="did not reach paid")
        else:
            run.add("payout_hold", FAILED, detail="no po_ from success-bank payout")
            run.add("payout_manual_release", FAILED, detail="hold never created a po_")

        if failure_bank:
            failed = _create_payout(api, connected, 30, failure_bank, f"merc-connect-{run.run_id}-failure")
            fail_id = str((failed or {}).get("id") or "")
            if fail_id.startswith("po_"):
                reached = _wait_payout(api, connected, fail_id, "failed")
                if reached:
                    run.fixtures["payout_failure"] = fail_id
                    run.add("payout_failure", PASS, fixture_id=fail_id, detail="status=failed")
                else:
                    run.add("payout_failure", FAILED, fixture_id=fail_id, detail="did not reach failed")
            else:
                run.add("payout_failure", FAILED, detail=f"no po_ {err_of(failed or {}).get('message')}")
        else:
            run.add("payout_failure", FAILED, detail="failure bank was not created")

    cap_status, caps = api.call("GET", f"accounts/{connected}/capabilities")
    upd_status, updated = api.call(
        "POST",
        f"accounts/{connected}",
        {"business_profile[product_description]": f"merc connect remainder {run.run_id}"},
    )

    def find_updated():
        st, body = api.call("GET", "events?type=account.updated&limit=40")
        for item in body.get("data") or []:
            obj = ((item.get("data") or {}).get("object") or {})
            if item.get("livemode") is False and obj.get("id") == connected:
                return item
        return None

    event = wait_for(find_updated, 60, 2.0)
    event_id = str(event.get("id") or "") if isinstance(event, dict) else ""
    if cap_status == 200 and upd_status == 200 and event_id.startswith("evt_"):
        run.add(
            "connect_restriction_capability_events",
            PASS,
            fixture_id=event_id,
            detail=f"capabilities http={cap_status}; account.updated {event_id}",
        )
    else:
        run.add(
            "connect_restriction_capability_events",
            FAILED,
            detail=f"capabilities http={cap_status} update http={upd_status} event={event_id or '-'}",
        )

    if webhook_status != PASS and not any(row["id"] == "connect_true_webhook_delivery" for row in run.scenarios):
        run.add("connect_true_webhook_delivery", webhook_status, detail="recreate did not pin connect=true")

    if all(any(row["id"] == sid and row["status"] == PASS for row in run.scenarios) for sid, _ in CONNECT_SCENARIOS):
        return PASS
    return FAILED


def _add_bank(api: StripeClient, connected: str, account_number: str) -> str | None:
    status, body = api.call(
        "POST",
        f"accounts/{connected}/external_accounts",
        {
            "external_account[object]": "bank_account",
            "external_account[country]": COUNTRY,
            "external_account[currency]": CURRENCY,
            "external_account[routing_number]": PAYOUT_ROUTING,
            "external_account[account_number]": account_number,
        },
    )
    bank_id = str(body.get("id") or "")
    if status == 200 and bank_id.startswith("ba_"):
        return bank_id
    log(f"external_account http={status} {err_of(body).get('message')}")
    return None


def _create_payout(api: StripeClient, connected: str, amount: int, destination: str, idem: str) -> dict[str, Any] | None:
    status, body = api.call(
        "POST",
        "payouts",
        {
            "amount": str(amount),
            "currency": CURRENCY,
            "destination": destination,
            "description": idem,
        },
        stripe_account=connected,
        idempotency=idem,
    )
    if status == 200 and str(body.get("id") or "").startswith("po_"):
        return body
    log(f"payout http={status} {err_of(body).get('message')}")
    return body if isinstance(body, dict) else None


def _wait_payout(api: StripeClient, connected: str, payout_id: str, wanted: str) -> bool:
    def check():
        st, body = api.call("GET", f"payouts/{payout_id}", stripe_account=connected)
        if st == 200 and body.get("livemode") is False and body.get("status") == wanted:
            return True
        if st == 200 and body.get("status") in {"failed", "canceled"} and wanted != body.get("status"):
            return False
        return None

    result = wait_for(check, 180, 3.0)
    return result is True


def _recreate_connect_webhook(api: StripeClient, run: ConnectRun, existing: dict[str, Any] | None) -> str:
    connect_url = f"https://{run.hostname}{CONNECT_PATH}"
    fields: dict[str, Any] = {
        "url": connect_url,
        "connect": "true",
        "api_version": API_VERSION,
        "description": f"merc connect remainder {run.run_id}",
        "enabled_events[]": list(CONNECT_EVENTS),
    }
    status, created = api.call("POST", "webhook_endpoints", fields)
    created_id = str(created.get("id") or "")
    if (
        status == 200
        and created_id.startswith("we_")
        and created.get("connect") is True
        and created.get("api_version") == API_VERSION
        and created.get("url") == connect_url
        and created.get("livemode") is False
    ):
        run.add(
            "connect_true_webhook_delivery",
            PASS,
            fixture_id=created_id,
            detail=f"connect=true api_version={API_VERSION}; previous={existing.get('id') if existing else None}",
        )
        run.notes.append(
            f"Recreated Connect webhook {created_id} with connect=true. "
            "Rotate MERC_CONNECT_WEBHOOK_SECRET from the dashboard reveal; this command does not print it."
        )
        return PASS
    run.add(
        "connect_true_webhook_delivery",
        FAILED if status == 200 else FAILED,
        fixture_id=created_id if created_id.startswith("we_") else None,
        detail=(
            f"http={status} connect={created.get('connect')!r} "
            f"api_version={created.get('api_version')} {err_of(created).get('message') or ''}"
        ),
    )
    if created_id.startswith("we_") and created.get("connect") is not True:
        api.call("DELETE", f"webhook_endpoints/{created_id}")
        run.notes.append(f"deleted unusable connect!=true recreate {created_id}")
    return FAILED


def write_receipt(path: Path, run: ConnectRun, outcome: str) -> dict[str, Any]:
    existing = load_matrix(path)
    receipt = merge_matrix(existing, run)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(receipt, indent=2) + "\n")
    log(f"wrote {path} status={receipt['status']} connect={outcome}")
    return receipt


def main() -> int:
    if "--self-test" in sys.argv:
        return self_test()

    for name in (
        "STRIPE_SECRET_KEY",
        "STRIPE_LIVE_SECRET_KEY",
        "STRIPE_RESTRICTED_KEY",
        "STRIPE_PUBLISHABLE_KEY",
        "NEXT_PUBLIC_STRIPE_PUBLISHABLE_KEY",
    ):
        if classify(os.environ.get(name, "")) == "live":
            die_live(name)

    key = os.environ.get("STRIPE_SECRET_KEY", "")
    if classify(key) != "test":
        log("test-class STRIPE_SECRET_KEY required")
        return 2

    hostname = (os.environ.get("STAGING_TLS_HOSTNAME") or "mercmerc.net").strip().lower()
    currency = (os.environ.get("MERC_SETTLEMENT_CURRENCY") or CURRENCY).strip().lower()
    if currency != CURRENCY:
        log(f"settlement currency must be {CURRENCY}")
        return 2

    run_id = os.environ.get("MERC_STRIPE_RUN_ID") or time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-l9cn"
    out_path = Path(os.environ.get("MERC_STRIPE_MATRIX_OUT", "evidence/external/stripe-sandbox-matrix.json"))
    run = ConnectRun(run_id, hostname)
    api = StripeClient(key)
    outcome = FAILED
    try:
        outcome = drive(api, run)
    except Exception as exc:  # noqa: BLE001 — receipt must still be written
        log(f"driver exception: {type(exc).__name__}: {exc}")
        run.notes.append(f"driver exception: {type(exc).__name__}")
        outcome = FAILED
    receipt = write_receipt(out_path, run, outcome)

    summary = {
        "schema_version": 1,
        "kind": "stripe_connect_remainder",
        "command": COMMAND,
        "status": "BLOCKED" if outcome == BLOCKED else ("PASS" if outcome == PASS else "FAILED"),
        "blocked": BLOCKED_MESSAGE if outcome == BLOCKED else None,
        "stopped_at": run.stopped_at,
        "path": str(out_path),
        "secret_values_printed": False,
        "live_mode": "PROHIBITED",
        "matrix_status": receipt.get("status"),
    }
    print(json.dumps(summary, indent=2))
    if outcome == PASS:
        return 0
    if outcome == BLOCKED:
        return 3
    return 1


if __name__ == "__main__":
    sys.exit(main())
