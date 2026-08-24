#!/usr/bin/env python3
"""One-command Stripe Connect remainder, up to the API refusal.

Test-mode only. Never prints secret values. Never reads .merc-secrets.env.
Never synthesizes a tr_, acct_, po_, or we_. Does not write PASS for a
scenario that did not run.

Connect signup and the platform-profile / Accounts-v1 dashboard walls are
cleared. The remainder is: supply the two currently_due KYC fields so
`transfers` goes active, transfer CAD, pay out to the documented Canadian
bank (standard method, not instant-to-card), observe connected-account
capability events on the connected event list, and keep a Connect-scoped
webhook. Basil omits the `connect` field on webhook_endpoint objects;
Connect scope is the `application=ca_*` value returned when connect=true
is accepted. A Stripe 400 whose message names a dashboard action is an
external gate, not a product defect. The receipt stays BLOCKED until the
remainder produces a tr_ plus payout hold/release/failure/reversal.
"""
from __future__ import annotations

import base64
import importlib.util
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
# capability.updated is how a restriction lift shows up. Compiled Connect
# events stay required; this is a superset, not a replacement.
WEBHOOK_EVENTS = list(dict.fromkeys([*CONNECT_EVENTS, "capability.updated"]))
PHONE_TOKEN = "0000000000"
RELATIONSHIP_TITLE = "Owner"
COMMAND = os.environ.get("MERC_STRIPE_CONNECT_REMAINDER_COMMAND", "scripts/stripe-sandbox-connect.sh")
PLATFORM_ACCOUNT = "acct_1TxbzMCwPLrR4vaY"
CONNECT_PATH = "/v1/stripe/connect-webhook"
API_ROOT = "https://api.stripe.com/v1"
CTX = ssl.create_default_context()
PLACEHOLDER_ACCOUNT = "<connected_account>"
BLOCKED_PREFIX = "blocked"
# The message must name the gate that actually refused. It used to be the
# constant "blocked: Connect not signed up", which kept asserting a wall the
# operator had already cleared and contradicted the blocker_id printed beside
# it on the same run.
def blocked_message(blocker_id: str = "") -> str:
    return f"{BLOCKED_PREFIX}: {blocker_id}" if blocker_id else f"{BLOCKED_PREFIX}: external gate"
PASS = "PASS"
BLOCKED = "BLOCKED-ON-CONNECT"
FAILED = "FAILED"
CONNECT_SIGNUP_URL = "https://dashboard.stripe.com/connect"
PLATFORM_PROFILE_URL = "https://dashboard.stripe.com/settings/connect/platform-profile"
V1_SUPPORT_URL = "https://dashboard.stripe.com/settings/features/feat_accounts_v1_support"
DASHBOARD_ACTION_URL_RE = re.compile(r"https://dashboard\.stripe\.com/[^\s\"'<>]+")

# Observed against acct_1TxbzMCwPLrR4vaY (test mode). Quoted verbatim so
# classification cannot silently drift off the walls that actually happened.
REFUSAL_CONNECT_SIGNUP = (
    "You can only create new accounts if you've signed up for Connect, "
    "which you can do at https://dashboard.stripe.com/connect."
)
REFUSAL_PLATFORM_PROFILE = (
    "Please review the responsibilities of collecting requirements for "
    "connected accounts at https://dashboard.stripe.com/settings/connect/platform-profile."
)
REFUSAL_ACCOUNTS_V1 = (
    "Stripe no longer recommends Accounts v1 for new Connect integrations. "
    "Create connected accounts with POST /v2/core/accounts instead: "
    "https://docs.stripe.com/api/v2/core/accounts. Read more about Accounts v2: "
    "https://docs.stripe.com/connect/accounts-v2/account-creation. If your "
    "integration requires v1 account creation for a supported compatibility "
    "scenario, enable Accounts v1 support in the Dashboard: "
    "https://dashboard.stripe.com/settings/features/feat_accounts_v1_support. "
    "For agent-based integrations, use Stripe's current best-practices skill: "
    "npx skills add stripe/ai."
)
REFUSAL_DEFECT_NO_DASHBOARD = (
    "No such destination: 'acct_NOTAREALACCOUNT99'"
)

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


def is_connect_scoped(endpoint: dict[str, Any] | None) -> bool:
    """Basil webhook_endpoint objects omit `connect` even when it was sent.

    A create with connect=true returns application=ca_* (the Connect
    application). A create with connect=false returns application=null.
    That is the field that distinguishes a genuine Connect-scoped endpoint
    under 2025-06-30.basil.
    """
    if not isinstance(endpoint, dict):
        return False
    if endpoint.get("connect") is True:
        return True
    application = endpoint.get("application")
    return isinstance(application, str) and application.startswith("ca_")


def pick_endpoint(
    listed: dict[str, Any] | None,
    url: str,
    *,
    connect_scoped: bool,
) -> dict[str, Any] | None:
    """Enabled endpoint at url, preferring the requested Connect scope.

    Listing by URL alone is how an older account-scoped we_ at the same
    Connect path outranked the Connect-scoped we_ that the remainder
    actually exercised.
    """
    matches: list[dict[str, Any]] = []
    for ep in (listed or {}).get("data") or []:
        if not isinstance(ep, dict):
            continue
        if ep.get("url") != url or ep.get("status") != "enabled":
            continue
        if ep.get("livemode") is True:
            continue
        matches.append(ep)
    if not matches:
        return None
    preferred = [ep for ep in matches if is_connect_scoped(ep) is connect_scoped]
    return (preferred or matches)[0]


def stamp_matrix(doc: dict[str, Any]) -> dict[str, Any]:
    """Last step before writing. Uses the committed primitive; refuses a non-sha."""
    path = Path(__file__).resolve().with_name("receipt_binding.py")
    spec = importlib.util.spec_from_file_location("receipt_binding", path)
    if spec is None or spec.loader is None:
        raise SystemExit("stripe-sandbox-connect: cannot load receipt_binding.py")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    root = str(Path(__file__).resolve().parents[2])
    return mod.stamp(doc, mod.candidate_commit(root), "scripts/stripe-sandbox.sh")


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


def error_message(doc: dict[str, Any]) -> str:
    err = err_of(doc)
    return str(err.get("message") or err.get("user_message") or "")


def dashboard_action_urls(message: str) -> list[str]:
    """Dashboard *action* URLs named in a Stripe error message.

    Every Stripe error also carries request_log_url pointing at the
    Dashboard workbench. That is a log, not an operator action, and it
    is not consulted here.
    """
    found: list[str] = []
    for match in DASHBOARD_ACTION_URL_RE.findall(message or ""):
        url = match.rstrip(".,;:)")
        if "/logs" in url or "/workbench/" in url:
            continue
        if url not in found:
            found.append(url)
    return found


def classify_external_gate(status: int, doc: dict[str, Any]) -> dict[str, Any] | None:
    """Classify a Stripe refusal as an external dashboard gate, or None.

    A 400 whose message names a dashboard action is blocked-on-external.
    A 400 that names no dashboard action is a defect (FAILED). Other
    HTTP statuses are never this gate.
    """
    if status != 400:
        return None
    message = error_message(doc)
    urls = dashboard_action_urls(message)
    if not urls:
        return None
    url = urls[0]
    lower = message.lower()
    joined = " ".join(urls).lower()
    if "signed up for connect" in lower:
        blocker_id = "connect_platform_not_signed_up"
        action = "Sign up for Connect"
        url = next((item for item in urls if item.rstrip("/").endswith("/connect")), url)
    elif "platform-profile" in joined or "collecting requirements" in lower:
        blocker_id = "connect_platform_profile_incomplete"
        action = "Complete the Connect platform profile (requirement-collection responsibilities)"
        url = next((item for item in urls if "platform-profile" in item), url)
    elif (
        "accounts v1" in lower
        or "feat_accounts_v1" in joined
        or "/v2/core/accounts" in lower
    ):
        blocker_id = "connect_accounts_v1_not_enabled"
        action = (
            "Create connected accounts with POST /v2/core/accounts, "
            "or enable Accounts v1 support"
        )
        url = next((item for item in urls if "feat_accounts_v1" in item), url)
    else:
        blocker_id = "connect_dashboard_action_required"
        action = "Complete the dashboard action named in the Stripe refusal"
    return {
        "kind": "external_gate",
        "id": blocker_id,
        "message": message,
        "dashboard_url": url,
        "dashboard_urls": urls,
        "dashboard_action": action,
    }


def stopped_at_record(
    method: str,
    path: str,
    status: int,
    doc: dict[str, Any],
    sent: dict[str, Any],
) -> dict[str, Any]:
    gate = classify_external_gate(status, doc)
    message = gate["message"] if gate else error_message(doc)
    return {
        "method": method,
        "path": path,
        "http": status,
        "error_type": err_of(doc).get("type"),
        "error_code": err_of(doc).get("code"),
        "error_message": message,
        "request_fields": sent,
        "classification": "external_gate" if gate else "defect",
        "blocker_id": gate["id"] if gate else None,
        "dashboard_url": gate["dashboard_url"] if gate else None,
        "dashboard_action": gate["dashboard_action"] if gate else None,
    }


def receipt_blocker(
    stopped_at: dict[str, Any] | None,
    *,
    platform_account: str = PLATFORM_ACCOUNT,
    command: str = COMMAND,
) -> dict[str, Any] | None:
    """Derive the matrix blocker from the refusal observed at run time.

    No refusal, or a 400 that names no dashboard action, means no blocker.
    The id is taken from classify_external_gate, never a constant.
    """
    if not isinstance(stopped_at, dict):
        return None
    if stopped_at.get("classification") != "external_gate":
        return None
    blocker_id = stopped_at.get("blocker_id")
    if not isinstance(blocker_id, str) or not blocker_id:
        return None
    dashboard_url = str(stopped_at.get("dashboard_url") or "")
    dashboard_action = str(stopped_at.get("dashboard_action") or "")
    refusal = str(stopped_at.get("error_message") or "").rstrip(".")
    method = stopped_at.get("method") or "POST"
    path = stopped_at.get("path") or "/v1/accounts"
    return {
        "id": blocker_id,
        "detail": (
            f"{method} {path} on {platform_account} "
            f"returns: {refusal}. "
            f"Dashboard action: {dashboard_action} {dashboard_url}. "
            f"After that action, {command} is the one command that finishes "
            "the Connect remainder."
        ),
        "unreachable_in_test_mode_api": True,
        "classification": "external_gate",
        "dashboard_url": dashboard_url or None,
        "dashboard_action": dashboard_action or None,
        "stopped_at": stopped_at,
        "exit_reason": blocked_message(blocker_id),
    }


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


def account_kyc_fields(hostname: str, run_id: str) -> dict[str, str]:
    """Custom CA individual KYC plus the two currently_due fields.

    Stripe test-mode phone token 0000000000; relationship.title is a
    free-form job title. Observed against acct_1U7npECeWJZCwOUN: supplying
    both flips transfers and card_payments from inactive to active.
    """
    return {
        "business_type": "individual",
        "business_profile[url]": f"https://{hostname}",
        "business_profile[mcc]": "5734",
        "business_profile[product_description]": (
            f"Compute marketplace supplier payouts for rendered jobs ({run_id})"
        ),
        "tos_acceptance[date]": str(int(time.time())),
        "tos_acceptance[ip]": "127.0.0.1",
        "individual[first_name]": "Jenny",
        "individual[last_name]": "Rosen",
        "individual[email]": f"connect-remainder-{run_id}@example.invalid",
        "individual[dob][day]": "1",
        "individual[dob][month]": "1",
        "individual[dob][year]": "1901",
        "individual[address][line1]": "address_full_match",
        "individual[address][city]": "Toronto",
        "individual[address][state]": "ON",
        "individual[address][postal_code]": "M4B 1B3",
        "individual[address][country]": COUNTRY,
        "individual[phone]": PHONE_TOKEN,
        "individual[relationship][title]": RELATIONSHIP_TITLE,
    }


def capability_snapshot(account: dict[str, Any]) -> dict[str, Any]:
    req = account.get("requirements") if isinstance(account.get("requirements"), dict) else {}
    caps = account.get("capabilities") if isinstance(account.get("capabilities"), dict) else {}
    return {
        "card_payments": caps.get("card_payments"),
        "transfers": caps.get("transfers"),
        "currently_due": req.get("currently_due"),
        "past_due": req.get("past_due"),
        "disabled_reason": req.get("disabled_reason"),
        "charges_enabled": account.get("charges_enabled"),
        "payouts_enabled": account.get("payouts_enabled"),
    }


def funded_source_type(balance: dict[str, Any], amount: int) -> str | None:
    """Pick the balance rail that actually has CAD available.

    Platform CAD sits in source_types.card (charges), not bank_account.
    Transfers land on the connected account in the same rail. Forcing
    source_type=bank_account against a card-funded balance is the empty-rail
    failure this matrix used to report as a payout defect.
    """
    for item in balance.get("available") or []:
        if not isinstance(item, dict) or item.get("currency") != CURRENCY:
            continue
        sources = item.get("source_types") if isinstance(item.get("source_types"), dict) else {}
        for name, value in sources.items():
            try:
                available = int(value)
            except (TypeError, ValueError):
                continue
            if available >= amount:
                return str(name)
        try:
            if int(item.get("amount") or 0) >= amount:
                return "card"
        except (TypeError, ValueError):
            continue
    return None


def dry_run_plans(hostname: str, run_id: str) -> list[dict[str, Any]]:
    connect_url = f"https://{hostname}{CONNECT_PATH}"
    kyc = account_kyc_fields(hostname, run_id)
    return [
        {
            "id": "connected_account_creation",
            "method": "POST",
            "path": "/v1/accounts",
            "requires_connect": True,
            "fields": account_create_fields(run_id),
            "kyc_fields": kyc,
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
                "method": "standard",
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
                "method": "standard",
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
            "stripe_account": PLACEHOLDER_ACCOUNT,
            "fields": {
                "events_list": "GET /v1/events?type=capability.updated with Stripe-Account",
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
                "enabled_events": list(WEBHOOK_EVENTS),
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
    kyc = create.get("kyc_fields") if isinstance(create.get("kyc_fields"), dict) else {}
    if kyc.get("individual[phone]") != PHONE_TOKEN:
        errors.append("kyc update missing documented test phone token")
    if kyc.get("individual[relationship][title]") != RELATIONSHIP_TITLE:
        errors.append("kyc update missing individual.relationship.title")
    if not kyc.get("business_profile[product_description]"):
        errors.append("kyc update missing business_profile.product_description")

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
    if rfields.get("method") != "standard":
        errors.append("success payout is not method=standard")

    failure = by_id.get("payout_failure", {})
    ffields = failure.get("fields") if isinstance(failure.get("fields"), dict) else {}
    fbank = ffields.get("external_account") if isinstance(ffields.get("external_account"), dict) else {}
    if fbank.get("account_number") != "000111111116":
        errors.append("failure bank fixture drifted")
    if ffields.get("method") != "standard":
        errors.append("failure payout is not method=standard")

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
            "connect_webhook_endpoint": None,
            "connect_endpoint_connect_flag": None,
            "connect_endpoint_application": None,
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

    def patch(self, sid: str, **fields: Any) -> None:
        for row in self.scenarios:
            if row.get("id") == sid:
                row.update(fields)
                return

    def record_pre(self, step: str, **fields: Any) -> None:
        row = {"step": step, **fields}
        self.pre_connect.append(row)
        log(f"pre {step}: {fields.get('detail') or fields.get('status') or 'ok'}")

    def stop_connect(self, method: str, path: str, status: int, doc: dict[str, Any], sent: dict[str, Any]) -> None:
        self.stopped_at = stopped_at_record(method, path, status, doc, sent)
        message = str(self.stopped_at.get("error_message") or "")
        gate_id = str(self.stopped_at.get("blocker_id") or "")
        dashboard_url = self.stopped_at.get("dashboard_url")
        # One line, naming the gate that actually refused. This used to print a
        # constant "Connect not signed up" and then the real gate id underneath,
        # so the tool contradicted itself on every run.
        if gate_id:
            print(f"{blocked_message(gate_id)} {dashboard_url}", file=sys.stderr)
        else:
            print(blocked_message(""), file=sys.stderr)
        log(f"{blocked_message(gate_id)} at {method} {path} http={status} {message}")


def _self_test_gate_vs_defect() -> str | None:
    """Prove the blocker id is derived from the refusal, not a constant.

    The three known dashboard walls must produce three distinct ids. A 400
    that names no dashboard action must stay a defect (no blocker).
    """
    signup = classify_external_gate(400, {"error": {"message": REFUSAL_CONNECT_SIGNUP}})
    profile = classify_external_gate(400, {"error": {"message": REFUSAL_PLATFORM_PROFILE}})
    accounts_v1 = classify_external_gate(400, {"error": {"message": REFUSAL_ACCOUNTS_V1}})
    defect = classify_external_gate(
        400,
        {
            "error": {
                "message": REFUSAL_DEFECT_NO_DASHBOARD,
                "code": "resource_missing",
                "type": "invalid_request_error",
                "request_log_url": (
                    "https://dashboard.stripe.com/acct_1TxbzMCwPLrR4vaY/"
                    "test/workbench/logs?object=req_NOT_A_GATE"
                ),
            }
        },
    )
    if not signup or signup["id"] != "connect_platform_not_signed_up":
        return "signup refusal not classified as connect_platform_not_signed_up"
    if signup["dashboard_url"] != CONNECT_SIGNUP_URL:
        return "signup refusal lost the dashboard URL"
    if not profile or profile["id"] != "connect_platform_profile_incomplete":
        return "platform-profile refusal not classified as connect_platform_profile_incomplete"
    if profile["dashboard_url"] != PLATFORM_PROFILE_URL:
        return "platform-profile refusal lost the dashboard URL"
    if not accounts_v1 or accounts_v1["id"] != "connect_accounts_v1_not_enabled":
        return "Accounts v1 refusal not classified as connect_accounts_v1_not_enabled"
    if accounts_v1["dashboard_url"] != V1_SUPPORT_URL:
        return "Accounts v1 refusal lost the dashboard URL"
    ids = {signup["id"], profile["id"], accounts_v1["id"]}
    if len(ids) != 3:
        return "classifier produced a constant id across known refusals"
    if defect is not None:
        return "400 without a dashboard action was classified as a gate (must stay FAILED)"
    if classify_external_gate(200, {"id": "acct_x"}):
        return "HTTP 200 classified as a Connect wall"
    if classify_external_gate(404, {"error": {"message": REFUSAL_PLATFORM_PROFILE}}):
        return "non-400 classified as a Connect wall"

    signup_stop = stopped_at_record(
        "POST", "/v1/accounts", 400, {"error": {"message": REFUSAL_CONNECT_SIGNUP}}, {}
    )
    profile_stop = stopped_at_record(
        "POST", "/v1/accounts", 400, {"error": {"message": REFUSAL_PLATFORM_PROFILE}}, {}
    )
    v1_stop = stopped_at_record(
        "POST", "/v1/accounts", 400, {"error": {"message": REFUSAL_ACCOUNTS_V1}}, {}
    )
    defect_stop = stopped_at_record(
        "POST", "/v1/accounts", 400, {"error": {"message": REFUSAL_DEFECT_NO_DASHBOARD}}, {}
    )
    b_signup = receipt_blocker(signup_stop)
    b_profile = receipt_blocker(profile_stop)
    b_v1 = receipt_blocker(v1_stop)
    if not b_signup or not b_profile or not b_v1:
        return "gate refusal produced no receipt blocker"
    blocker_ids = {b_signup["id"], b_profile["id"], b_v1["id"]}
    if len(blocker_ids) != 3:
        return "receipt blocker.id is constant across known refusals"
    if PLATFORM_PROFILE_URL not in (b_profile.get("detail") or ""):
        return "platform-profile blocker detail lost the dashboard URL"
    if PLATFORM_PROFILE_URL not in (b_profile.get("dashboard_url") or ""):
        return "platform-profile blocker lost dashboard_url"
    if receipt_blocker(defect_stop) is not None:
        return "defect 400 produced a receipt blocker"
    if receipt_blocker(None) is not None:
        return "no refusal produced a receipt blocker"
    return None


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
    if not is_connect_scoped({"connect": True, "application": None}):
        log("self-test: connect=true endpoint not treated as Connect-scoped")
        return 1
    if not is_connect_scoped({"connect": None, "application": "ca_V7Df0n9ofZqlquTrJTDBS1gZf3NXHEie"}):
        log("self-test: basil application=ca_ endpoint not treated as Connect-scoped")
        return 1
    if is_connect_scoped({"connect": None, "application": None}):
        log("self-test: account-scoped endpoint treated as Connect-scoped")
        return 1
    listed = {
        "data": [
            {
                "id": "we_old",
                "url": "https://canary.example.invalid/v1/stripe/connect-webhook",
                "status": "enabled",
                "livemode": False,
                "connect": None,
                "application": None,
            },
            {
                "id": "we_new",
                "url": "https://canary.example.invalid/v1/stripe/connect-webhook",
                "status": "enabled",
                "livemode": False,
                "connect": None,
                "application": "ca_TESTAPP",
            },
        ]
    }
    picked = pick_endpoint(
        listed,
        "https://canary.example.invalid/v1/stripe/connect-webhook",
        connect_scoped=True,
    )
    if not picked or picked.get("id") != "we_new":
        log("self-test: pick_endpoint did not prefer Connect-scoped we_")
        return 1
    kyc = account_kyc_fields("canary.example.invalid", "self-test")
    if kyc.get("individual[phone]") != PHONE_TOKEN or kyc.get("individual[relationship][title]") != RELATIONSHIP_TITLE:
        log("self-test: kyc fields drifted")
        return 1
    if funded_source_type({"available": [{"currency": "cad", "amount": 100, "source_types": {"card": 100}}]}, 40) != "card":
        log("self-test: funded_source_type missed the card rail")
        return 1
    if funded_source_type({"available": [{"currency": "cad", "amount": 0, "source_types": {"card": 0}}]}, 40) is not None:
        log("self-test: funded_source_type invented a rail")
        return 1
    class_error = _self_test_gate_vs_defect()
    if class_error:
        log(f"self-test: {class_error}")
        return 1
    if blocked_message("connect_platform_profile_incomplete") != "blocked: connect_platform_profile_incomplete":
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
    if BLOCKED_PREFIX not in wrapper:
        log("self-test: wrapper does not document the blocked exit")
        return 1
    if "check|matrix|nonconnect|connect" not in parent:
        log("self-test: stripe-sandbox.sh does not accept connect")
        return 1
    import importlib.util

    nc_spec = importlib.util.spec_from_file_location(
        "stripe_sandbox_nonconnect_self_test",
        root / "scripts/lib/stripe-sandbox-nonconnect.py",
    )
    if nc_spec is None or nc_spec.loader is None:
        log("self-test: cannot load stripe-sandbox-nonconnect.py")
        return 1
    nc = importlib.util.module_from_spec(nc_spec)
    nc_spec.loader.exec_module(nc)
    for message, expected in (
        (REFUSAL_CONNECT_SIGNUP, "connect_platform_not_signed_up"),
        (REFUSAL_PLATFORM_PROFILE, "connect_platform_profile_incomplete"),
        (REFUSAL_ACCOUNTS_V1, "connect_accounts_v1_not_enabled"),
    ):
        doc = {"error": {"message": message}}
        here = classify_external_gate(400, doc)
        there = nc.classify_external_gate(400, doc)
        if (
            not here
            or not there
            or here["id"] != expected
            or there["id"] != expected
            or here["id"] != there["id"]
            or here.get("dashboard_url") != there.get("dashboard_url")
        ):
            log("self-test: nonconnect classifier disagrees with connect remainder")
            return 1
    if nc.classify_external_gate(400, {"error": {"message": REFUSAL_DEFECT_NO_DASHBOARD}}) is not None:
        log("self-test: nonconnect classified a defect 400 as a gate")
        return 1
    if nc.receipt_blocker(None) is not None:
        log("self-test: nonconnect receipt_blocker invented a wall")
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
    out = {
        "id": plan["id"],
        "method": plan["method"],
        "path": plan["path"],
        "requires_connect": plan.get("requires_connect"),
        "stripe_account": plan.get("stripe_account"),
        "fields": fields,
        "dry_run_verified": True,
        "sent": sent,
    }
    if plan.get("kyc_fields"):
        out["kyc_fields"] = plan["kyc_fields"]
    return out


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

    # Derive from this run. Do not keep a previously observed wall: that is
    # how a cleared Connect-signup blocker outlived the refusal that replaced it.
    if top_status == "PASS":
        blocker = None
    else:
        blocker = receipt_blocker(run.stopped_at)

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
    wall = (blocker or {}).get("id") if isinstance(blocker, dict) else None
    validator["this_receipt"] = (
        "honest PASS; Connect remainder completed"
        if top_status == "PASS"
        else (
            f"honest BLOCKED; expected CHECK_FAILED until {wall}"
            if wall
            else "honest BLOCKED; expected CHECK_FAILED until Connect remainder completes"
        )
    )

    merged_run_id = existing.get("run_id") or run.run_id
    if os.environ.get("MERC_STRIPE_FULL_MATRIX") == "1":
        merged_run_id = run.run_id
    elif existing.get("run_id") and existing.get("run_id") != run.run_id:
        mismatch = (
            f"Connect remainder run_id {run.run_id} was merged into matrix "
            f"run_id {existing.get('run_id')}. Re-run scripts/stripe-sandbox.sh "
            "matrix so one run_id covers every row."
        )
        if mismatch not in notes:
            notes.append(mismatch)
    external["run_id"] = merged_run_id

    merged = dict(existing)
    merged.update(
        {
            "schema_version": 1,
            "kind": "stripe_sandbox_matrix",
            "status": top_status,
            "run_id": merged_run_id,
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
                "exit_reason": None if connect_pass else blocked_message(
                    str((run.stopped_at or {}).get("blocker_id") or "")
                ),
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
    billing_url = f"https://{run.hostname}/v1/stripe/webhook"
    if status != 200:
        run.record_pre("GET /v1/webhook_endpoints", status="FAILED", http=status)
        return FAILED
    connect_ep = pick_endpoint(endpoints, connect_url, connect_scoped=True)
    billing_ep = pick_endpoint(endpoints, billing_url, connect_scoped=False)
    run.record_pre(
        "GET /v1/webhook_endpoints",
        status="ok",
        http=status,
        billing_id=billing_ep.get("id") if isinstance(billing_ep, dict) else None,
        connect_id=connect_ep.get("id") if isinstance(connect_ep, dict) else None,
        connect_flag=connect_ep.get("connect") if isinstance(connect_ep, dict) else None,
        connect_application=connect_ep.get("application") if isinstance(connect_ep, dict) else None,
        connect_scoped=is_connect_scoped(connect_ep) if isinstance(connect_ep, dict) else False,
        connect_api_version=connect_ep.get("api_version") if isinstance(connect_ep, dict) else None,
        detail=(
            f"connect={connect_ep.get('id') if connect_ep else None} "
            f"flag={connect_ep.get('connect') if connect_ep else None} "
            f"application={connect_ep.get('application') if connect_ep else None} "
            f"api_version={connect_ep.get('api_version') if connect_ep else None}"
        ),
    )
    if isinstance(connect_ep, dict) and not is_connect_scoped(connect_ep):
        run.notes.append(
            "Existing Connect URL endpoint is not Connect-scoped (no application=ca_*); "
            f"{COMMAND} will recreate it with connect=true."
        )

    status, listed = api.call("GET", "accounts?limit=10")
    gate = classify_external_gate(status, listed)
    if gate:
        run.record_pre("GET /v1/accounts", status="blocked", http=status, detail=gate["message"])
        run.stop_connect("GET", "/v1/accounts", status, listed, {"limit": "10"})
        _block_remaining(run, f"requires {gate['id']} on {PLATFORM_ACCOUNT}")
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
    gate = classify_external_gate(status, created)
    if gate:
        run.stop_connect("POST", "/v1/accounts", status, created, create_fields)
        run.add(
            "connected_account_creation",
            BLOCKED,
            detail=gate["message"],
            extra={
                "attempted": True,
                "http": status,
                "path": "/v1/accounts",
                "method": "POST",
                "dry_run_verified": True,
                "classification": "external_gate",
                "blocker_id": gate["id"],
                "dashboard_url": gate["dashboard_url"],
            },
        )
        _block_remaining(run, f"requires {gate['id']} on {PLATFORM_ACCOUNT}")
        return BLOCKED

    created_id = str(created.get("id") or "")
    if status != 200 or not created_id.startswith("acct_") or created_id == PLATFORM_ACCOUNT:
        run.add(
            "connected_account_creation",
            FAILED,
            detail=f"http={status} {error_message(created) or created_id}",
            extra={"attempted": True, "http": status, "path": "/v1/accounts", "method": "POST"},
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
        "enabled_events[]": list(WEBHOOK_EVENTS),
    }
    status, created = api.call("POST", "webhook_endpoints", fields)
    gate = classify_external_gate(status, created)
    if gate:
        run.stop_connect("POST", "/v1/webhook_endpoints", status, created, {"connect": "true", "api_version": API_VERSION})
        _block_remaining(run, f"requires {gate['id']} on {PLATFORM_ACCOUNT}")
        return {"stopped": True, "http": status, "error": gate["message"]}

    probe_id = str(created.get("id") or "")
    secret_present = bool(created.get("secret"))
    returned_connect = created.get("connect")
    returned_version = created.get("api_version")
    returned_application = created.get("application")
    scoped = is_connect_scoped(created)
    deleted = False
    if probe_id.startswith("we_"):
        del_status, _deleted = api.call("DELETE", f"webhook_endpoints/{probe_id}")
        deleted = del_status == 200 and (_deleted.get("deleted") is True or _deleted.get("id") == probe_id)
        if not deleted:
            run.notes.append(f"probe endpoint {probe_id} delete http={del_status}; operator should remove it")

    detail = (
        f"requested connect=true api_version={API_VERSION}; "
        f"returned connect={returned_connect!r} application={returned_application!r} "
        f"connect_scoped={scoped} api_version={returned_version} "
        f"deleted={deleted} secret_present={secret_present}"
    )
    run.record_pre(
        "POST /v1/webhook_endpoints (connect=true probe)",
        status="ok" if status == 200 else "FAILED",
        http=status,
        id=probe_id if probe_id.startswith("we_") else None,
        connect_flag=returned_connect,
        application=returned_application if isinstance(returned_application, str) else None,
        connect_scoped=scoped,
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
        "returned_application": returned_application,
        "connect_scoped": scoped,
        "returned_api_version": returned_version,
        "secret_present": secret_present,
        "deleted": deleted,
        "detail": detail,
    }


def _drive_remainder(api: StripeClient, run: ConnectRun, connected: str, connect_ep: dict[str, Any] | None) -> str:
    # Recreate (or reuse) the Connect webhook first so later events can land on it.
    webhook_status = _recreate_connect_webhook(api, run, connect_ep)

    _st, before_acct = api.call("GET", f"accounts/{connected}")
    before_caps = capability_snapshot(before_acct)
    log(
        f"capabilities before kyc transfers={before_caps.get('transfers')} "
        f"currently_due={before_caps.get('currently_due')} "
        f"disabled_reason={before_caps.get('disabled_reason')}"
    )

    kyc_fields = account_kyc_fields(run.hostname, run.run_id)
    kyc_started = int(time.time())
    tos_status, tos_body = api.call("POST", f"accounts/{connected}", kyc_fields)
    log(
        f"tos/kyc http={tos_status} payouts_enabled={tos_body.get('payouts_enabled')} "
        f"currently_due={(tos_body.get('requirements') or {}).get('currently_due')} "
        f"transfers={(tos_body.get('capabilities') or {}).get('transfers')}"
    )
    # external_account is currently_due on a fresh Custom CA account and
    # blocks payouts_enabled until a bank is attached. Attach both banks
    # before waiting on transfers so the remainder is not racing KYC.
    success_bank = _add_bank(api, connected, PAYOUT_SUCCESS)
    failure_bank = _add_bank(api, connected, PAYOUT_FAILURE)
    log(f"external_accounts success={success_bank or '-'} failure={failure_bank or '-'}")
    _wait_transfers_active(api, connected)
    _st, after_acct = api.call("GET", f"accounts/{connected}")
    after_caps = capability_snapshot(after_acct)
    log(
        f"capabilities after kyc transfers={after_caps.get('transfers')} "
        f"currently_due={after_caps.get('currently_due')} "
        f"disabled_reason={after_caps.get('disabled_reason')}"
    )
    run.patch(
        "connected_account_creation",
        detail=(
            f"type={after_acct.get('type')} country={after_acct.get('country')}; "
            f"transfers {before_caps.get('transfers')}->{after_caps.get('transfers')}; "
            f"currently_due before={before_caps.get('currently_due')} after={after_caps.get('currently_due')}"
        ),
        capabilities_before=before_caps,
        capabilities_after=after_caps,
        kyc_fields_sent=[
            "individual[phone]",
            "individual[relationship][title]",
            "business_profile[product_description]",
            "business_type",
            "tos_acceptance[date]",
            "tos_acceptance[ip]",
            "external_account",
        ],
        kyc_http=tos_status,
    )

    transfer_fields = {
        "amount": "100",
        "currency": CURRENCY,
        "destination": connected,
        "metadata[cx_matrix_run]": run.run_id,
    }
    if after_caps.get("transfers") != "active":
        run.add(
            "transfer_to_connected_account",
            FAILED,
            detail=(
                f"transfers={after_caps.get('transfers')} "
                f"currently_due={after_caps.get('currently_due')} "
                f"disabled_reason={after_caps.get('disabled_reason')}"
            ),
            extra={"attempted": False, "capabilities_after": after_caps},
        )
    else:
        status, transfer = api.call(
            "POST",
            "transfers",
            transfer_fields,
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
            run.add(
                "transfer_to_connected_account",
                PASS,
                fixture_id=transfer_id,
                detail=f"100 {CURRENCY} -> {connected} source_type={transfer.get('source_type')}",
                extra={"attempted": True, "http": status, "request_fields": {**transfer_fields, "destination": connected}},
            )
        else:
            run.add(
                "transfer_to_connected_account",
                FAILED,
                detail=f"http={status} {err_of(transfer).get('message')}",
                extra={"attempted": True, "http": status, "request_fields": {"amount": "100", "currency": CURRENCY}},
            )

    funded = None
    source_type = None
    connected_balance: dict[str, Any] = {}
    if run.fixtures.get("transfer"):
        funded = _wait_available(api, connected, 40)
        if isinstance(funded, tuple):
            connected_balance, source_type = funded
        log(f"connected available source_type={source_type} balance={connected_balance.get('available')}")

    status, manual = api.call(
        "POST",
        f"accounts/{connected}",
        {"settings[payouts][schedule][interval]": "manual"},
    )
    interval = ((manual.get("settings") or {}).get("payouts") or {}).get("schedule", {}).get("interval")
    if status != 200 or interval != "manual":
        run.add("payout_hold", FAILED, fixture_id=connected, detail=f"http={status} interval={interval}")
        if not any(row["id"] == "payout_manual_release" for row in run.scenarios):
            run.add("payout_manual_release", FAILED, detail="manual schedule was not set")
        if not any(row["id"] == "payout_failure" for row in run.scenarios):
            run.add("payout_failure", FAILED, detail="manual schedule was not set")
    elif run.fixtures.get("transfer") is None:
        run.add("payout_hold", FAILED, detail="no transfer landed; connected card/bank balance was not funded")
        run.add("payout_manual_release", FAILED, detail="hold skipped; no funded transfer")
        run.add("payout_failure", FAILED, detail="failure skipped; no funded transfer")
    elif not source_type:
        run.add(
            "payout_hold",
            FAILED,
            detail=f"connected account has no funded CAD rail for a 40-cent standard payout; available={connected_balance.get('available')}",
        )
        run.add("payout_manual_release", FAILED, detail="no funded CAD rail")
        run.add("payout_failure", FAILED, detail="no funded CAD rail")
    else:
        if not success_bank:
            success_bank = _add_bank(api, connected, PAYOUT_SUCCESS)
        if not failure_bank:
            failure_bank = _add_bank(api, connected, PAYOUT_FAILURE)
        released = None
        if success_bank:
            released = _create_payout(
                api,
                connected,
                40,
                success_bank,
                f"merc-connect-{run.run_id}-release",
                source_type=source_type,
            )
        if isinstance(released, dict) and str(released.get("id") or "").startswith("po_"):
            payout_id = str(released["id"])
            run.fixtures["payout_hold"] = payout_id
            run.fixtures["payout_release"] = payout_id
            run.add(
                "payout_hold",
                PASS,
                fixture_id=payout_id,
                detail=(
                    f"manual schedule; standard bank payout method={released.get('method')} "
                    f"type={released.get('type')} source_type={released.get('source_type')} "
                    f"destination={success_bank}"
                ),
                extra={"attempted": True, "method": released.get("method"), "source_type": released.get("source_type")},
            )
            paid = _wait_payout(api, connected, payout_id, "paid")
            if paid:
                run.add(
                    "payout_manual_release",
                    PASS,
                    fixture_id=payout_id,
                    detail=f"status=paid method={released.get('method')} type={released.get('type')}",
                )
                rev_status, reversed = api.call("POST", f"payouts/{payout_id}/reverse", stripe_account=connected)
                rev_id = str(reversed.get("id") or "")
                if rev_status == 200 and rev_id.startswith("po_") and reversed.get("amount", 0) < 0:
                    run.fixtures["payout_reversal"] = rev_id
                else:
                    run.notes.append(f"payout reverse http={rev_status} {err_of(reversed).get('message')}")
            else:
                run.add("payout_manual_release", FAILED, fixture_id=payout_id, detail="did not reach paid")
        else:
            run.add(
                "payout_hold",
                FAILED,
                detail=(
                    f"no po_ from standard bank payout source_type={source_type} "
                    f"{err_of(released or {}).get('message') or ''}"
                ).strip(),
            )
            run.add("payout_manual_release", FAILED, detail="hold never created a po_")

        fail_funded = _wait_available(api, connected, 30)
        fail_source = fail_funded[1] if isinstance(fail_funded, tuple) else source_type
        if failure_bank:
            failed = _create_payout(
                api,
                connected,
                30,
                failure_bank,
                f"merc-connect-{run.run_id}-failure",
                source_type=fail_source,
            )
            fail_id = str((failed or {}).get("id") or "")
            if fail_id.startswith("po_"):
                reached = _wait_payout(api, connected, fail_id, "failed")
                if reached:
                    run.fixtures["payout_failure"] = fail_id
                    run.add(
                        "payout_failure",
                        PASS,
                        fixture_id=fail_id,
                        detail=(
                            f"status=failed method={(failed or {}).get('method')} "
                            f"type={(failed or {}).get('type')} source_type={(failed or {}).get('source_type')}"
                        ),
                    )
                else:
                    run.add("payout_failure", FAILED, fixture_id=fail_id, detail="did not reach failed")
            else:
                run.add(
                    "payout_failure",
                    FAILED,
                    detail=f"no po_ source_type={fail_source} {err_of(failed or {}).get('message')}",
                )
        else:
            run.add("payout_failure", FAILED, detail="failure bank was not created")

    cap_status, _caps = api.call("GET", f"accounts/{connected}/capabilities")
    upd_status, _updated = api.call(
        "POST",
        f"accounts/{connected}",
        {"business_profile[product_description]": f"merc connect remainder {run.run_id}"},
    )

    def find_capability():
        return _find_connected_event(
            api,
            connected,
            "capability.updated",
            object_id="transfers",
            since=kyc_started - 30,
            status_value="active",
        )

    def find_updated():
        return _find_connected_event(
            api,
            connected,
            "account.updated",
            object_id=connected,
            since=kyc_started - 30,
        )

    cap_event = wait_for(find_capability, 90, 2.0)
    acct_event = wait_for(find_updated, 90, 2.0)
    cap_event_id = str(cap_event.get("id") or "") if isinstance(cap_event, dict) else ""
    acct_event_id = str(acct_event.get("id") or "") if isinstance(acct_event, dict) else ""
    event_id = cap_event_id if cap_event_id.startswith("evt_") else acct_event_id
    if cap_status == 200 and upd_status == 200 and event_id.startswith("evt_"):
        pending = None
        chosen = cap_event if cap_event_id.startswith("evt_") else acct_event
        if isinstance(chosen, dict):
            pending = chosen.get("pending_webhooks")
        run.add(
            "connect_restriction_capability_events",
            PASS,
            fixture_id=event_id,
            detail=(
                f"capabilities http={cap_status}; update http={upd_status}; "
                f"capability.updated={cap_event_id or '-'} account.updated={acct_event_id or '-'} "
                f"(listed with Stripe-Account={connected})"
            ),
            extra={
                "attempted": True,
                "capability_event": cap_event_id or None,
                "account_event": acct_event_id or None,
                "pending_webhooks": pending,
            },
        )
        if webhook_status == PASS and isinstance(pending, int) and pending >= 0:
            for row in run.scenarios:
                if row.get("id") == "connect_true_webhook_delivery":
                    row["detail"] = (
                        f"{row.get('detail')}; connected event {event_id} "
                        f"pending_webhooks={pending}"
                    )
                    break
    else:
        platform_count = 0
        st_p, platform_events = api.call("GET", "events?type=account.updated&limit=10")
        if st_p == 200:
            platform_count = len(platform_events.get("data") or [])
        run.add(
            "connect_restriction_capability_events",
            FAILED,
            detail=(
                f"capabilities http={cap_status} update http={upd_status} "
                f"capability.updated={cap_event_id or '-'} account.updated={acct_event_id or '-'} "
                f"platform_account.updated_count={platform_count}"
            ),
        )

    if webhook_status != PASS and not any(row["id"] == "connect_true_webhook_delivery" for row in run.scenarios):
        run.add("connect_true_webhook_delivery", webhook_status, detail="recreate did not pin Connect scope")

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
    message = str(err_of(body).get("message") or "")
    log(f"external_account http={status} {message}")
    if "already exists" in message.lower() or status == 400:
        st, listed = api.call("GET", f"accounts/{connected}/external_accounts?limit=20")
        want_last4 = account_number[-4:]
        for item in listed.get("data") or []:
            if (
                isinstance(item, dict)
                and str(item.get("id") or "").startswith("ba_")
                and str(item.get("last4") or "") == want_last4
            ):
                return str(item["id"])
    return None


def _create_payout(
    api: StripeClient,
    connected: str,
    amount: int,
    destination: str,
    idem: str,
    *,
    source_type: str | None,
) -> dict[str, Any] | None:
    fields: dict[str, str] = {
        "amount": str(amount),
        "currency": CURRENCY,
        "destination": destination,
        "method": "standard",
        "description": idem,
    }
    if source_type:
        fields["source_type"] = source_type
    status, body = api.call(
        "POST",
        "payouts",
        fields,
        stripe_account=connected,
        idempotency=idem,
    )
    if status == 200 and str(body.get("id") or "").startswith("po_"):
        return body
    log(
        f"payout http={status} method=standard source_type={source_type} "
        f"destination={destination} {err_of(body).get('message')}"
    )
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


def _wait_transfers_active(api: StripeClient, connected: str, timeout: float = 90.0) -> dict[str, Any] | None:
    def check():
        st, body = api.call("GET", f"accounts/{connected}/capabilities/transfers")
        if st == 200 and body.get("status") == "active":
            return body
        return None

    last = wait_for(check, timeout, 2.0)
    return last if isinstance(last, dict) else None


def _wait_available(api: StripeClient, connected: str, amount: int, timeout: float = 60.0):
    def check():
        st, body = api.call("GET", "balance", stripe_account=connected)
        if st != 200:
            return None
        source = funded_source_type(body, amount)
        if source:
            return (body, source)
        return None

    return wait_for(check, timeout, 2.0)


def _find_connected_event(
    api: StripeClient,
    connected: str,
    event_type: str,
    *,
    object_id: str,
    since: int = 0,
    status_value: str | None = None,
) -> dict[str, Any] | None:
    st, body = api.call("GET", f"events?type={event_type}&limit=40", stripe_account=connected)
    if st != 200:
        return None
    for item in body.get("data") or []:
        if not isinstance(item, dict) or item.get("livemode") is True:
            continue
        if int(item.get("created") or 0) < since:
            continue
        obj = (item.get("data") or {}).get("object") or {}
        if obj.get("id") != object_id:
            continue
        if status_value and obj.get("status") != status_value:
            continue
        if str(item.get("id") or "").startswith("evt_"):
            return item
    return None


def _recreate_connect_webhook(api: StripeClient, run: ConnectRun, existing: dict[str, Any] | None) -> str:
    connect_url = f"https://{run.hostname}{CONNECT_PATH}"
    st, listed = api.call("GET", "webhook_endpoints?limit=100")
    reusable = None
    if st == 200:
        for ep in listed.get("data") or []:
            if not isinstance(ep, dict):
                continue
            if ep.get("url") != connect_url or ep.get("status") != "enabled":
                continue
            if ep.get("api_version") != API_VERSION or ep.get("livemode") is True:
                continue
            if not is_connect_scoped(ep):
                continue
            events = ep.get("enabled_events") or []
            if "*" in events or all(name in events for name in CONNECT_EVENTS):
                reusable = ep
                break
    if isinstance(reusable, dict) and str(reusable.get("id") or "").startswith("we_"):
        rid = str(reusable["id"])
        events = list(reusable.get("enabled_events") or [])
        if any(name not in events and "*" not in events for name in WEBHOOK_EVENTS):
            upd_status, updated = api.call(
                "POST",
                f"webhook_endpoints/{rid}",
                {"enabled_events[]": list(WEBHOOK_EVENTS)},
            )
            if upd_status == 200 and isinstance(updated, dict):
                reusable = updated
        run.fixtures["connect_webhook_endpoint"] = rid
        run.fixtures["connect_endpoint_connect_flag"] = reusable.get("connect")
        run.fixtures["connect_endpoint_application"] = reusable.get("application")
        run.add(
            "connect_true_webhook_delivery",
            PASS,
            fixture_id=rid,
            detail=(
                f"reused Connect-scoped {rid}; requested connect=true; "
                f"basil connect={reusable.get('connect')!r} "
                f"application={reusable.get('application')}; "
                f"api_version={reusable.get('api_version')}"
            ),
            extra={
                "attempted": True,
                "application": reusable.get("application"),
                "connect_flag": reusable.get("connect"),
                "connect_scoped": True,
            },
        )
        return PASS

    fields: dict[str, Any] = {
        "url": connect_url,
        "connect": "true",
        "api_version": API_VERSION,
        "description": f"merc connect remainder {run.run_id}",
        "enabled_events[]": list(WEBHOOK_EVENTS),
    }
    status, created = api.call("POST", "webhook_endpoints", fields)
    created_id = str(created.get("id") or "")
    scoped = is_connect_scoped(created)
    if (
        status == 200
        and created_id.startswith("we_")
        and scoped
        and created.get("api_version") == API_VERSION
        and created.get("url") == connect_url
        and created.get("livemode") is False
    ):
        run.fixtures["connect_webhook_endpoint"] = created_id
        run.fixtures["connect_endpoint_connect_flag"] = created.get("connect")
        run.fixtures["connect_endpoint_application"] = created.get("application")
        run.add(
            "connect_true_webhook_delivery",
            PASS,
            fixture_id=created_id,
            detail=(
                f"created connect=true api_version={API_VERSION}; "
                f"basil connect={created.get('connect')!r} "
                f"application={created.get('application')}; "
                f"previous={existing.get('id') if existing else None}"
            ),
            extra={
                "attempted": True,
                "application": created.get("application"),
                "connect_flag": created.get("connect"),
                "connect_scoped": True,
                "secret_present": bool(created.get("secret")),
            },
        )
        run.notes.append(
            f"Recreated Connect webhook {created_id} (application={created.get('application')}). "
            "Rotate MERC_CONNECT_WEBHOOK_SECRET from the dashboard reveal; this command does not print it."
        )
        return PASS
    run.add(
        "connect_true_webhook_delivery",
        FAILED,
        fixture_id=created_id if created_id.startswith("we_") else None,
        detail=(
            f"http={status} connect={created.get('connect')!r} "
            f"application={created.get('application')!r} "
            f"api_version={created.get('api_version')} {err_of(created).get('message') or ''}"
        ),
        extra={"attempted": True, "connect_scoped": scoped},
    )
    if created_id.startswith("we_") and not scoped:
        api.call("DELETE", f"webhook_endpoints/{created_id}")
        run.notes.append(f"deleted unusable non-Connect recreate {created_id}")
    return FAILED


def write_receipt(path: Path, run: ConnectRun, outcome: str) -> dict[str, Any]:
    existing = load_matrix(path)
    receipt = stamp_matrix(merge_matrix(existing, run))
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
        "blocked": (
            blocked_message(str((run.stopped_at or {}).get("blocker_id") or ""))
            if outcome == BLOCKED
            else None
        ),
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
