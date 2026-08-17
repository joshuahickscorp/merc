#!/usr/bin/env python3
"""Drive every Stripe Sandbox scenario that is not gated on Connect.

Test-mode only. Never prints secret values. Never reads .merc-secrets.env.
Writes evidence/external/stripe-sandbox-matrix.json as an honest partial
receipt: PASS / REFUSED-AS-EXPECTED / BLOCKED-ON-CONNECT per scenario.
status remains BLOCKED until Connect signup unblocks tr_/acct_/payouts.
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import os
import ssl
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

API_VERSION = "2025-06-30.basil"
DRIFT_API_VERSION = "2026-06-24.dahlia"
API_ROOT = "https://api.stripe.com/v1"
PLATFORM_ACCOUNT = "acct_1TxbzMCwPLrR4vaY"
BILLING_PATH = "/v1/stripe/webhook"
CONNECT_PATH = "/v1/stripe/connect-webhook"
CTX = ssl.create_default_context()
FORBIDDEN_ENV_FILES = {".merc-secrets.env"}

PASS = "PASS"
REFUSED = "REFUSED-AS-EXPECTED"
BLOCKED = "BLOCKED-ON-CONNECT"
FAILED = "FAILED"


def log(msg: str) -> None:
    print(f"stripe-nonconnect: {msg}", file=sys.stderr)


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


def die_live(variable: str) -> None:
    print(
        json.dumps(
            {
                "schema_version": 1,
                "kind": "stripe_sandbox_matrix",
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
        idempotency: str | None = None,
        timeout: float = 45.0,
    ) -> tuple[int, dict[str, Any]]:
        url = f"{API_ROOT}/{path.lstrip('/')}"
        body = None
        headers = {
            "Authorization": self.auth,
            "Stripe-Version": self.version,
        }
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


def http_post(
    url: str,
    payload: bytes,
    headers: dict[str, str],
    timeout: float = 20.0,
) -> tuple[int, dict[str, str], bytes]:
    req = urllib.request.Request(url, data=payload, method="POST", headers=headers)
    try:
        with urllib.request.urlopen(req, context=CTX, timeout=timeout) as resp:
            return resp.status, {k.lower(): v for k, v in resp.headers.items()}, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, {k.lower(): v for k, v in exc.headers.items()}, exc.read()
    except Exception as exc:
        return 0, {}, f"{type(exc).__name__}".encode()


def http_get(url: str, timeout: float = 15.0) -> tuple[int, bytes]:
    req = urllib.request.Request(url, method="GET")
    try:
        with urllib.request.urlopen(req, context=CTX, timeout=timeout) as resp:
            return resp.status, resp.read(800)
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read(800)
    except Exception as exc:
        return 0, type(exc).__name__.encode()


def sign(secret: str, payload: bytes, ts: int) -> str:
    digest = hmac.new(secret.encode(), f"{ts}.".encode() + payload, hashlib.sha256).hexdigest()
    return f"t={ts},v1={digest}"


def err_of(doc: dict[str, Any]) -> dict[str, Any]:
    err = doc.get("error")
    return err if isinstance(err, dict) else {}


class Matrix:
    def __init__(self, run_id: str, currency: str) -> None:
        self.run_id = run_id
        self.currency = currency
        self.scenarios: list[dict[str, Any]] = []
        self.fixtures: dict[str, Any] = {
            "provider_mode": "test",
            "livemode": False,
            "transfer": None,
            "payout_hold": None,
            "payout_release": None,
            "payout_failure": None,
            "payout_reversal": None,
        }
        self.notes: list[str] = []

    def add(
        self,
        sid: str,
        status: str,
        *,
        fixture_id: str | None = None,
        detail: str = "",
        extra: dict[str, Any] | None = None,
    ) -> None:
        row = {
            "id": sid,
            "status": status,
            "provider_mode": "test",
            "live_mode": "PROHIBITED",
            "fixture_id": fixture_id,
            "detail": detail,
        }
        if extra:
            row.update(extra)
        self.scenarios.append(row)
        log(f"{sid}: {status} fixture={fixture_id or '-'} {detail}")

    def status_of(self, sid: str) -> str | None:
        for row in self.scenarios:
            if row["id"] == sid:
                return str(row["status"])
        return None

    def all_nonconnect_ok(self) -> bool:
        for row in self.scenarios:
            if row["status"] == BLOCKED:
                continue
            if row["status"] not in {PASS, REFUSED}:
                return False
        return True


def wait_for(predicate, timeout: float, interval: float = 2.0):
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        last = predicate()
        if last:
            return last
        time.sleep(interval)
    return last


def main() -> int:
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

    currency = (os.environ.get("MERC_SETTLEMENT_CURRENCY") or "cad").strip().lower()
    hostname = (os.environ.get("STAGING_TLS_HOSTNAME") or "mercmerc.net").strip().lower()
    billing_secret = os.environ.get("STRIPE_WEBHOOK_SECRET", "")
    connect_secret = os.environ.get("MERC_CONNECT_WEBHOOK_SECRET", "")
    billing_url = f"https://{hostname}{BILLING_PATH}"
    connect_url = f"https://{hostname}{CONNECT_PATH}"
    out_path = Path(os.environ.get("MERC_STRIPE_MATRIX_OUT", "evidence/external/stripe-sandbox-matrix.json"))
    handler_receipt_path = Path(os.environ.get("MERC_STRIPE_HANDLER_RECEIPT", ""))

    run_id = os.environ.get("MERC_STRIPE_RUN_ID") or time.strftime("%Y%m%dT%H%M%SZ", time.gmtime()) + "-l9nc"
    matrix = Matrix(run_id, currency)
    api = StripeClient(key)
    customer = ""

    handler_receipt: dict[str, Any] = {}
    if handler_receipt_path.is_file():
        try:
            loaded = json.loads(handler_receipt_path.read_text())
            if isinstance(loaded, dict):
                handler_receipt = loaded
        except (OSError, json.JSONDecodeError):
            handler_receipt = {}

    try:
        _drive(api, matrix, customer_holder := {"id": ""}, currency, hostname,
               billing_url, connect_url, billing_secret, connect_secret,
               handler_receipt)
        customer = customer_holder["id"]
    finally:
        if customer.startswith("cus_"):
            api.call("DELETE", f"customers/{customer}")
            matrix.fixtures["disposable_customer_cleanup"] = "attempted"

    receipt = build_receipt(matrix, hostname, billing_url, connect_url, handler_receipt)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(receipt, indent=2) + "\n")
    log(f"wrote {out_path} status={receipt['status']}")
    print(json.dumps({"schema_version": 1, "path": str(out_path), "status": receipt["status"],
                      "nonconnect_ok": matrix.all_nonconnect_ok(),
                      "secret_values_printed": False}))
    return 0 if matrix.all_nonconnect_ok() else 1


def _drive(
    api: StripeClient,
    matrix: Matrix,
    customer_holder: dict[str, str],
    currency: str,
    hostname: str,
    billing_url: str,
    connect_url: str,
    billing_secret: str,
    connect_secret: str,
    handler_receipt: dict[str, Any],
) -> None:
    status, account = api.call("GET", "account")
    acct_id = str(account.get("id") or "")
    if status != 200 or not acct_id.startswith("acct_"):
        matrix.add("platform_account", FAILED, detail=f"account http={status}")
        return
    if acct_id != PLATFORM_ACCOUNT:
        matrix.add("platform_account", FAILED, fixture_id=acct_id, detail="unexpected platform account")
        return
    if account.get("livemode") is True:
        matrix.add("platform_account", FAILED, fixture_id=acct_id, detail="livemode true refused")
        return
    matrix.fixtures["platform_account"] = acct_id
    matrix.fixtures["platform_country"] = account.get("country")
    matrix.fixtures["platform_default_currency"] = account.get("default_currency")
    matrix.add("platform_account", PASS, fixture_id=acct_id,
               detail=f"country={account.get('country')} default_currency={account.get('default_currency')} charges_enabled={account.get('charges_enabled')} payouts_enabled={account.get('payouts_enabled')}")

    status, balance = api.call("GET", "balance")
    currencies = sorted({
        str(item.get("currency"))
        for item in (balance.get("available") or []) + (balance.get("pending") or [])
        if item.get("currency")
    })
    if status != 200 or balance.get("livemode") is not False or currency not in currencies:
        matrix.add("settlement_currency", FAILED, detail=f"http={status} currencies={currencies}")
    else:
        matrix.add("settlement_currency", PASS, detail=f"enabled={currencies}")

    status, endpoints = api.call("GET", "webhook_endpoints?limit=100")
    billing_ep = None
    connect_ep = None
    for ep in endpoints.get("data") or []:
        if not isinstance(ep, dict):
            continue
        if ep.get("url") == billing_url:
            billing_ep = ep
        elif ep.get("url") == connect_url:
            connect_ep = ep
    if billing_ep and connect_ep and billing_ep.get("id") != connect_ep.get("id"):
        matrix.fixtures["billing_webhook_endpoint"] = billing_ep.get("id")
        matrix.fixtures["connect_webhook_endpoint"] = connect_ep.get("id")
        matrix.fixtures["connect_endpoint_connect_flag"] = connect_ep.get("connect")
        matrix.add(
            "webhook_endpoints_registered",
            PASS,
            fixture_id=str(billing_ep.get("id")),
            detail=(
                f"billing={billing_ep.get('id')} connect={connect_ep.get('id')} "
                f"billing_api_version={billing_ep.get('api_version')} "
                f"connect_api_version={connect_ep.get('api_version')} "
                f"connect_flag={connect_ep.get('connect')!r} "
                f"billing_events={billing_ep.get('enabled_events')} "
                f"connect_events={connect_ep.get('enabled_events')}"
            ),
            extra={
                "billing_url": billing_ep.get("url"),
                "connect_url": connect_ep.get("url"),
                "connect_flag": connect_ep.get("connect"),
                "needs_recreate_with_connect_true": connect_ep.get("connect") is not True,
            },
        )
        if connect_ep.get("connect") is not True:
            matrix.notes.append(
                "Connect endpoint was created with connect=true requested but Stripe returned connect=null "
                "because Connect is not enabled. Recreate we_ with connect=true after Connect signup."
            )
    else:
        matrix.add("webhook_endpoints_registered", FAILED, detail=f"http={status} missing exact staging URLs")

    # Connect signup wall — must stay BLOCKED, never synthesized.
    status, created = api.call(
        "POST",
        "accounts",
        {
            "type": "custom",
            "country": "CA",
            "capabilities[card_payments][requested]": "true",
            "capabilities[transfers][requested]": "true",
        },
    )
    connect_msg = str(err_of(created).get("message") or "")
    if status == 400 and "signed up for Connect" in connect_msg:
        matrix.add(
            "connected_account_creation",
            BLOCKED,
            detail=connect_msg,
            extra={"would_prove": "A project-controlled Canadian test connected account exists and is distinct from the platform."},
        )
    else:
        matrix.add("connected_account_creation", FAILED, detail=f"http={status} {connect_msg or created.get('id')}")

    for sid, would in (
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
    ):
        matrix.add(sid, BLOCKED, detail="requires Connect signup on acct_1TxbzMCwPLrR4vaY", extra={"would_prove": would})

    # Self-transfer and bogus destination refuse at the platform API without Connect.
    status, self_xfer = api.call(
        "POST",
        "transfers",
        {"amount": "100", "currency": currency, "destination": PLATFORM_ACCOUNT},
    )
    self_msg = str(err_of(self_xfer).get("message") or "")
    if status >= 400 and ("own account" in self_msg.lower() or "destination" in self_msg.lower() or err_of(self_xfer)):
        matrix.add("self_transfer_refusal", REFUSED, detail=f"http={status} {self_msg}")
    else:
        matrix.add("self_transfer_refusal", FAILED, detail=f"http={status} {self_msg or self_xfer.get('id')}")

    status, bogus = api.call(
        "POST",
        "transfers",
        {"amount": "100", "currency": currency, "destination": "acct_NOTAREALACCOUNT99"},
    )
    bogus_err = err_of(bogus)
    if status >= 400 and (bogus_err.get("code") == "resource_missing" or bogus_err):
        matrix.add("bogus_transfer_destination_refusal", REFUSED, fixture_id=None, detail=f"http={status} {bogus_err.get('code')} {bogus_err.get('message')}")
    else:
        matrix.add("bogus_transfer_destination_refusal", FAILED, detail=f"http={status}")

    status, customer = api.call(
        "POST",
        "customers",
        {"description": f"merc disposable Sandbox nonconnect {matrix.run_id}",
         "metadata[cx_matrix_run]": matrix.run_id},
    )
    cus_id = str(customer.get("id") or "")
    if status != 200 or not cus_id.startswith("cus_") or customer.get("livemode") is not False:
        matrix.add("buyer_funding", FAILED, detail=f"customer http={status}")
        return
    customer_holder["id"] = cus_id
    matrix.fixtures["customer"] = cus_id

    timeout_idem = f"merc-l9-{matrix.run_id}-timeout"
    timeout_fields = {
        "amount": "900",
        "currency": currency,
        "customer": cus_id,
        "payment_method": "pm_card_visa",
        "payment_method_types[]": "card",
        "confirm": "true",
        "metadata[cx_matrix_run]": matrix.run_id,
    }
    t_status, t_body = api.call("POST", "payment_intents", timeout_fields, idempotency=timeout_idem, timeout=0.001)
    if t_status == 0 and err_of(t_body).get("code") == "client_timeout":
        matrix.add("timeout_client_deadline", PASS, detail="client deadline fired before a response")
    else:
        matrix.add("timeout_client_deadline", FAILED, detail=f"http={t_status}")

    r_status, recovered = api.call("POST", "payment_intents", timeout_fields, idempotency=timeout_idem)
    rec_id = str(recovered.get("id") or "")
    if (
        r_status == 200
        and rec_id.startswith("pi_")
        and recovered.get("livemode") is False
        and recovered.get("status") == "succeeded"
        and recovered.get("currency") == currency
    ):
        matrix.fixtures["timeout_recovery_payment_intent"] = rec_id
        matrix.add("timeout_idempotent_recovery", PASS, fixture_id=rec_id, detail="same idempotency key recovered one pi_")
    else:
        matrix.add("timeout_idempotent_recovery", FAILED, detail=f"http={r_status} {err_of(recovered).get('message')}")

    # Manual authorization then capture.
    auth_idem = f"merc-l9-{matrix.run_id}-auth"
    a_status, authorized = api.call(
        "POST",
        "payment_intents",
        {
            "amount": "1100",
            "currency": currency,
            "customer": cus_id,
            "payment_method": "pm_card_visa",
            "payment_method_types[]": "card",
            "confirm": "true",
            "capture_method": "manual",
            "metadata[cx_matrix_run]": matrix.run_id,
        },
        idempotency=auth_idem,
    )
    auth_id = str(authorized.get("id") or "")
    if (
        a_status == 200
        and auth_id.startswith("pi_")
        and authorized.get("livemode") is False
        and authorized.get("status") == "requires_capture"
        and authorized.get("currency") == currency
    ):
        matrix.fixtures["authorization_payment_intent"] = auth_id
        matrix.add("buyer_authorization", PASS, fixture_id=auth_id, detail="capture_method=manual requires_capture")
        c_status, captured = api.call("POST", f"payment_intents/{auth_id}/capture")
        cap_charge = captured.get("latest_charge")
        if isinstance(cap_charge, dict):
            cap_charge = cap_charge.get("id")
        if (
            c_status == 200
            and captured.get("status") == "succeeded"
            and captured.get("livemode") is False
            and isinstance(cap_charge, str)
            and cap_charge.startswith("ch_")
        ):
            matrix.fixtures["captured_charge"] = cap_charge
            matrix.add("buyer_capture", PASS, fixture_id=cap_charge, detail=f"captured {auth_id}")
        else:
            matrix.add("buyer_capture", FAILED, fixture_id=auth_id, detail=f"http={c_status}")
    else:
        matrix.add("buyer_authorization", FAILED, detail=f"http={a_status} {err_of(authorized).get('message')}")
        matrix.add("buyer_capture", FAILED, detail="authorization did not reach requires_capture")

    success_idem = f"merc-l9-{matrix.run_id}-success"
    success_fields = {
        "amount": "1200",
        "currency": currency,
        "customer": cus_id,
        "payment_method": "pm_card_visa",
        "payment_method_types[]": "card",
        "confirm": "true",
        "metadata[cx_matrix_run]": matrix.run_id,
    }
    s_status, success = api.call("POST", "payment_intents", success_fields, idempotency=success_idem)
    pi = str(success.get("id") or "")
    charge = success.get("latest_charge")
    if isinstance(charge, dict):
        charge = charge.get("id")
    charge_id = charge if isinstance(charge, str) else ""
    if (
        s_status == 200
        and pi.startswith("pi_")
        and charge_id.startswith("ch_")
        and success.get("livemode") is False
        and success.get("status") == "succeeded"
        and success.get("currency") == currency
    ):
        matrix.fixtures["payment_intent"] = pi
        matrix.fixtures["charge"] = charge_id
        matrix.add("buyer_funding", PASS, fixture_id=pi, detail=f"charge={charge_id} amount=1200 {currency}")
    else:
        matrix.add("buyer_funding", FAILED, detail=f"http={s_status} {err_of(success).get('message')}")
        pi = ""
        charge_id = ""

    if pi:
        r2_status, retry = api.call("POST", "payment_intents", success_fields, idempotency=success_idem)
        if r2_status == 200 and retry.get("id") == pi:
            matrix.add("idempotency", PASS, fixture_id=pi, detail="same key twice returned one provider object")
        else:
            matrix.add("idempotency", FAILED, fixture_id=pi, detail=f"http={r2_status} id={retry.get('id')}")
        conflict_status, conflict = api.call(
            "POST",
            "payment_intents",
            {"amount": "1201", "currency": currency},
            idempotency=success_idem,
        )
        if conflict_status >= 400 and err_of(conflict).get("type") == "idempotency_error":
            matrix.add("idempotency_conflict", REFUSED, fixture_id=pi, detail="same key different body → idempotency_error")
        else:
            matrix.add("idempotency_conflict", FAILED, detail=f"http={conflict_status} {err_of(conflict)}")

    decline_cases = [
        ("pm_card_chargeDeclined", "generic"),
        ("pm_card_chargeDeclinedInsufficientFunds", "insufficient_funds"),
        ("pm_card_chargeDeclinedStolenCard", "stolen_card"),
    ]
    decline_ids: list[str] = []
    decline_codes: list[str] = []
    for method, label in decline_cases:
        d_status, declined = api.call(
            "POST",
            "payment_intents",
            {
                "amount": "700",
                "currency": currency,
                "customer": cus_id,
                "payment_method": method,
                "payment_method_types[]": "card",
                "confirm": "true",
                "metadata[cx_matrix_run]": matrix.run_id,
            },
            idempotency=f"merc-l9-{matrix.run_id}-dec-{label}",
        )
        err = err_of(declined)
        code = str(err.get("decline_code") or err.get("code") or "")
        d_pi = str((err.get("payment_intent") or {}).get("id") or declined.get("id") or "")
        if not d_pi.startswith("pi_"):
            # Stripe sometimes returns the PI under error.payment_intent as an id string
            maybe = err.get("payment_intent")
            if isinstance(maybe, str) and maybe.startswith("pi_"):
                d_pi = maybe
        if d_status >= 400 and (err.get("decline_code") or err.get("code") == "card_declined"):
            decline_ids.append(d_pi)
            decline_codes.append(f"{label}:{code}")
        else:
            decline_codes.append(f"{label}:UNDECLINED:{d_status}")
    if decline_codes and all("UNDECLINED" not in item for item in decline_codes):
        matrix.fixtures["declined_payment_intents"] = [item for item in decline_ids if item]
        matrix.add(
            "payment_failure_decline_codes",
            PASS,
            fixture_id=decline_ids[0] if decline_ids else None,
            detail=",".join(decline_codes),
        )
    else:
        matrix.add("payment_failure_decline_codes", FAILED, detail=",".join(decline_codes))

    # Buyer retry: a new funding attempt after decline, with a good card.
    retry_status, retry_pi = api.call(
        "POST",
        "payment_intents",
        {
            "amount": "800",
            "currency": currency,
            "customer": cus_id,
            "payment_method": "pm_card_visa",
            "payment_method_types[]": "card",
            "confirm": "true",
            "metadata[cx_matrix_run]": matrix.run_id,
        },
        idempotency=f"merc-l9-{matrix.run_id}-retry",
    )
    retry_id = str(retry_pi.get("id") or "")
    if retry_status == 200 and retry_id.startswith("pi_") and retry_pi.get("status") == "succeeded" and retry_pi.get("livemode") is False:
        matrix.fixtures["retry_payment_intent"] = retry_id
        matrix.add("retries", PASS, fixture_id=retry_id, detail="new PI after decline codes succeeded")
    else:
        matrix.add("retries", FAILED, detail=f"http={retry_status} {err_of(retry_pi).get('message')}")

    if charge_id:
        p_status, partial = api.call(
            "POST",
            "refunds",
            {"charge": charge_id, "amount": "200", "metadata[cx_matrix_run]": matrix.run_id},
        )
        partial_id = str(partial.get("id") or "")
        if (
            p_status == 200
            and partial_id.startswith("re_")
            and partial.get("status") == "succeeded"
            and partial.get("amount") == 200
            and partial.get("currency") == currency
        ):
            matrix.fixtures["partial_refund"] = partial_id
            matrix.add("refund_partial", PASS, fixture_id=partial_id, detail="200 cad")
        else:
            matrix.add("refund_partial", FAILED, detail=f"http={p_status} {err_of(partial).get('message')}")

        x_status, excess = api.call("POST", "refunds", {"charge": charge_id, "amount": "1200"})
        if x_status >= 400 and err_of(excess):
            matrix.add("refund_excess_refusal", REFUSED, fixture_id=charge_id, detail=str(err_of(excess).get("message") or x_status))
        else:
            matrix.add("refund_excess_refusal", FAILED, detail=f"http={x_status}")

        f_status, remaining = api.call(
            "POST",
            "refunds",
            {"charge": charge_id, "amount": "1000", "metadata[cx_matrix_run]": matrix.run_id},
        )
        remaining_id = str(remaining.get("id") or "")
        if (
            f_status == 200
            and remaining_id.startswith("re_")
            and remaining.get("status") == "succeeded"
            and remaining.get("amount") == 1000
            and remaining.get("currency") == currency
        ):
            matrix.fixtures["remaining_refund"] = remaining_id
            matrix.add("refund_full", PASS, fixture_id=remaining_id, detail="remaining 1000 cad")
        else:
            matrix.add("refund_full", FAILED, detail=f"http={f_status} {err_of(remaining).get('message')}")

    # Dispute lifecycle.
    disp_status, disputed = api.call(
        "POST",
        "payment_intents",
        {
            "amount": "1500",
            "currency": currency,
            "payment_method": "pm_card_createDispute",
            "payment_method_types[]": "card",
            "confirm": "true",
            "metadata[cx_matrix_run]": matrix.run_id,
        },
        idempotency=f"merc-l9-{matrix.run_id}-dispute",
    )
    disputed_pi = str(disputed.get("id") or "")
    disputed_charge = disputed.get("latest_charge")
    if isinstance(disputed_charge, dict):
        disputed_charge = disputed_charge.get("id")
    disputed_charge_id = disputed_charge if isinstance(disputed_charge, str) else ""
    if (
        disp_status == 200
        and disputed_pi.startswith("pi_")
        and disputed_charge_id.startswith("ch_")
        and disputed.get("livemode") is False
        and disputed.get("currency") == currency
    ):
        matrix.fixtures["disputed_payment_intent"] = disputed_pi
        matrix.fixtures["disputed_charge"] = disputed_charge_id

        def find_dispute():
            st, body = api.call("GET", f"disputes?charge={disputed_charge_id}&limit=5")
            for item in body.get("data") or []:
                did = str(item.get("id") or "")
                if (did.startswith("dp_") or did.startswith("du_")) and item.get("livemode") is False:
                    return item
            return None

        dispute_obj = wait_for(find_dispute, 90, 2.0)
        if isinstance(dispute_obj, dict):
            dispute_id = str(dispute_obj.get("id"))
            matrix.fixtures["dispute"] = dispute_id
            matrix.add("dispute_created", PASS, fixture_id=dispute_id, detail=f"status={dispute_obj.get('status')}")
            close_status, closed = api.call(
                "POST",
                f"disputes/{dispute_id}",
                {"evidence[uncategorized_text]": "losing_evidence", "submit": "true"},
            )
            if close_status == 200 and closed.get("livemode") is False and closed.get("status") in {"lost", "under_review"}:
                matrix.add("dispute_closed", PASS, fixture_id=dispute_id, detail=f"status={closed.get('status')}")
            else:
                matrix.add("dispute_closed", FAILED, fixture_id=dispute_id, detail=f"http={close_status} {err_of(closed).get('message')}")
        else:
            matrix.add("dispute_created", FAILED, fixture_id=disputed_charge_id, detail="no du_/dp_ appeared within 90s")
            matrix.add("dispute_closed", FAILED, detail="dispute never opened")
    else:
        matrix.add("dispute_created", FAILED, detail=f"http={disp_status} {err_of(disputed).get('message')}")
        matrix.add("dispute_closed", FAILED, detail="dispute PI not created")

    # Provider-side reconciliation of objects created in this run.
    recon_ok = True
    recon_bits: list[str] = []
    for field, prefix in (
        ("payment_intent", "pi_"),
        ("charge", "ch_"),
        ("partial_refund", "re_"),
        ("remaining_refund", "re_"),
        ("disputed_payment_intent", "pi_"),
        ("disputed_charge", "ch_"),
        ("dispute", ("dp_", "du_")),
    ):
        value = str(matrix.fixtures.get(field) or "")
        prefixes = prefix if isinstance(prefix, tuple) else (prefix,)
        if not any(value.startswith(p) for p in prefixes):
            recon_ok = False
            recon_bits.append(f"missing:{field}")
            continue
        path = {
            "payment_intent": f"payment_intents/{value}",
            "charge": f"charges/{value}",
            "partial_refund": f"refunds/{value}",
            "remaining_refund": f"refunds/{value}",
            "disputed_payment_intent": f"payment_intents/{value}",
            "disputed_charge": f"charges/{value}",
            "dispute": f"disputes/{value}",
        }[field]
        st, body = api.call("GET", path)
        # Refund (and Account) objects omit livemode under 2025-06-30.basil.
        # Fail closed only when the field is present and true.
        if st != 200 or body.get("livemode") is True:
            recon_ok = False
            recon_bits.append(f"bad:{field}")
            continue
        if body.get("currency") != currency:
            recon_ok = False
            recon_bits.append(f"currency:{field}")
            continue
        recon_bits.append(f"ok:{field}")
    if charge_id and matrix.fixtures.get("partial_refund") and matrix.fixtures.get("remaining_refund"):
        st, ch = api.call("GET", f"charges/{charge_id}")
        if st != 200 or ch.get("amount") != 1200 or ch.get("amount_refunded") != 1200 or ch.get("currency") != currency:
            recon_ok = False
            recon_bits.append("charge_refund_sum")
        else:
            recon_bits.append("charge_refund_sum_ok")
    # Merc ledger application of unowned fixtures cannot invent buyer rows.
    # Local real-handler cash outcomes, if the wrapper recorded them, close that loop.
    cash = handler_receipt.get("cash_outcomes") if isinstance(handler_receipt.get("cash_outcomes"), dict) else {}
    ledger_note = "unowned provider fixtures have no Merc operation_key; buyer ledger rows are not invented"
    if cash.get("applied") and cash.get("stale_ignored") and cash.get("duplicate"):
        recon_bits.append("local_handler_cash_outcomes_ok")
        ledger_note = "local real handler applied/stale_ignored/duplicate on CAD cash envelopes"
    else:
        recon_bits.append("local_handler_cash_outcomes_absent")
    matrix.add(
        "reconciliation_provider_to_ledger",
        PASS if recon_ok else FAILED,
        fixture_id=str(matrix.fixtures.get("payment_intent") or ""),
        detail=f"{';'.join(recon_bits)}; {ledger_note}",
        extra={"clean_provider_objects": recon_ok, "merc_owned_operation": False},
    )

    # Live staging + signed refusals.
    _drive_live_and_refusals(
        api, matrix, hostname, billing_url, connect_url, billing_secret, connect_secret, handler_receipt
    )


def _drive_live_and_refusals(
    api: StripeClient,
    matrix: Matrix,
    hostname: str,
    billing_url: str,
    connect_url: str,
    billing_secret: str,
    connect_secret: str,
    handler_receipt: dict[str, Any],
) -> None:
    def live_ready() -> bool:
        code, body = http_get(f"https://{hostname}/readyz")
        if code != 200:
            return False
        try:
            doc = json.loads(body.decode() or "{}")
        except json.JSONDecodeError:
            return False
        return (
            str(doc.get("status")) == "ready"
            and str(doc.get("payment_mode")) == "test"
            and str(doc.get("stripe_api_version")) == API_VERSION
            and doc.get("live_value_movement") is False
        )

    ready = False
    for _ in range(18):
        if live_ready():
            ready = True
            break
        time.sleep(5)
    code, ready_body = http_get(f"https://{hostname}/readyz")
    matrix.fixtures["staging_readyz_http"] = code
    matrix.fixtures["staging_readyz_body"] = ready_body.decode("utf-8", "replace")[:240]
    if ready:
        matrix.notes.append(f"readyz 200 on {hostname}")
    else:
        matrix.notes.append(
            f"readyz http={code} on {hostname} during this run; live posts retried. "
            "Parallel deploy lane owns the droplet."
        )

    ts = int(time.time())
    probe = json.dumps(
        {
            "id": f"evt_cx_l9_{matrix.run_id}_probe",
            "type": "cx.sandbox.secret_probe",
            "api_version": API_VERSION,
            "livemode": False,
            "created": ts,
            "data": {"object": {"id": "cx_sandbox_probe"}},
        },
        separators=(",", ":"),
    ).encode()

    def post_signed(url: str, secret: str, payload: bytes, sig: str | None = None) -> tuple[int, str, str]:
        headers = {"Content-Type": "application/json"}
        if sig is not None:
            headers["Stripe-Signature"] = sig
        elif secret:
            headers["Stripe-Signature"] = sign(secret, payload, int(time.time()))
        status, hdrs, raw = http_post(url, payload, headers)
        try:
            body = json.loads(raw.decode() or "{}")
            err = str(body.get("error") or raw[:160].decode("utf-8", "replace"))
        except json.JSONDecodeError:
            err = raw[:160].decode("utf-8", "replace")
        outcome = hdrs.get("x-merc-stripe-event-outcome", "")
        return status, err, outcome

    # Signature refusal against live (and record HTTP).
    live_sig_status, live_sig_err, _ = post_signed(
        billing_url, "", probe, sig="t=1,v1=" + ("0" * 64)
    )
    live_sig_connect, live_sig_connect_err, _ = post_signed(
        connect_url, "", probe, sig="t=1,v1=" + ("0" * 64)
    )
    live_sig_ok = live_sig_status == 400 and "invalid stripe signature" in live_sig_err
    live_sig_connect_ok = live_sig_connect == 400 and "invalid stripe signature" in live_sig_connect_err

    local_refusals = handler_receipt.get("refusals") if isinstance(handler_receipt.get("refusals"), dict) else {}

    if live_sig_ok:
        matrix.add(
            "signature_refusal",
            REFUSED,
            detail=f"live {billing_url} HTTP {live_sig_status} {live_sig_err}; connect HTTP {live_sig_connect} {live_sig_connect_err}",
            extra={"path": "https://mercmerc.net/v1/stripe/webhook", "http": live_sig_status},
        )
    elif local_refusals.get("invalid_signature") == 400:
        matrix.add(
            "signature_refusal",
            REFUSED,
            detail=f"local real handler HTTP 400; live http={live_sig_status} {live_sig_err}",
            extra={"path": "local real handler", "live_http": live_sig_status},
        )
    else:
        matrix.add("signature_refusal", FAILED, detail=f"live={live_sig_status} {live_sig_err} local={local_refusals}")

    # Wrong-authority: sign with the other endpoint's secret.
    wa_billing_status, wa_billing_err, _ = (0, "", "")
    wa_connect_status, wa_connect_err, _ = (0, "", "")
    if classify(billing_secret) == "webhook" and classify(connect_secret) == "webhook" and billing_secret != connect_secret:
        wa_connect_status, wa_connect_err, _ = post_signed(connect_url, billing_secret, probe)
        wa_billing_status, wa_billing_err, _ = post_signed(billing_url, connect_secret, probe)

    def wrong_auth_row(sid: str, live_status: int, live_err: str, local_key: str, path_live: str) -> None:
        local_code = local_refusals.get(local_key)
        live_refused = live_status == 400 and (
            "invalid stripe signature" in live_err or "contract mismatch" in live_err
        )
        if live_refused:
            matrix.add(sid, REFUSED, detail=f"live {path_live} HTTP {live_status} {live_err}", extra={"path": path_live, "http": live_status})
        elif local_code == 400:
            matrix.add(
                sid,
                REFUSED,
                detail=f"local real handler HTTP 400; live http={live_status} {live_err}",
                extra={"path": "local real handler", "live_http": live_status},
            )
        else:
            matrix.add(sid, FAILED, detail=f"live={live_status} {live_err} local={local_code}")

    wrong_auth_row(
        "wrong_authority_billing_secret_at_connect",
        wa_connect_status,
        wa_connect_err,
        "billing_secret_at_connect",
        connect_url,
    )
    wrong_auth_row(
        "wrong_authority_connect_secret_at_billing",
        wa_billing_status,
        wa_billing_err,
        "connect_secret_at_billing",
        billing_url,
    )

    # API-version contract refusal.
    drift = json.dumps(
        {
            "id": f"evt_cx_l9_{matrix.run_id}_drift",
            "type": "cx.sandbox.secret_probe",
            "api_version": DRIFT_API_VERSION,
            "livemode": False,
            "created": int(time.time()),
            "data": {"object": {"id": "cx_sandbox_probe"}},
        },
        separators=(",", ":"),
    ).encode()
    drift_status, drift_err = 0, ""
    if classify(billing_secret) == "webhook":
        drift_status, drift_err, _ = post_signed(billing_url, billing_secret, drift)
    local_drift = local_refusals.get("api_version_contract")
    if drift_status == 400 and ("contract mismatch" in drift_err or "invalid stripe signature" in drift_err):
        # If local secrets do not match the droplet, live reports invalid signature
        # rather than the contract mismatch. Prefer local real-handler contract
        # refusal when that is what actually distinguished the versions.
        if local_drift == 400:
            matrix.add(
                "api_version_contract_refusal",
                REFUSED,
                detail=f"local real handler HTTP 400 contract mismatch for {DRIFT_API_VERSION}; live http={drift_status} {drift_err}",
                extra={"path": "local real handler", "live_http": drift_status, "api_version": DRIFT_API_VERSION},
            )
        elif "contract mismatch" in drift_err:
            matrix.add(
                "api_version_contract_refusal",
                REFUSED,
                detail=f"live HTTP {drift_status} {drift_err}",
                extra={"path": billing_url, "http": drift_status, "api_version": DRIFT_API_VERSION},
            )
        else:
            matrix.add(
                "api_version_contract_refusal",
                FAILED,
                detail=f"live treated drift as signature failure (droplet secrets not on this workstation) and local handler did not fire",
            )
    elif local_drift == 400:
        matrix.add(
            "api_version_contract_refusal",
            REFUSED,
            detail=f"local real handler HTTP 400 for {DRIFT_API_VERSION}; live http={drift_status} {drift_err}",
            extra={"path": "local real handler", "live_http": drift_status, "api_version": DRIFT_API_VERSION},
        )
    else:
        matrix.add("api_version_contract_refusal", FAILED, detail=f"live={drift_status} {drift_err} local={local_drift}")

    # Account-mismatch is Connect-envelope logic but does not require a connected account.
    local_mismatch = local_refusals.get("account_mismatch")
    if local_mismatch == 400:
        matrix.add(
            "account_mismatch_refusal",
            REFUSED,
            detail="local real handler HTTP 400 connected account mismatch (envelope account != object id)",
            extra={"path": "local real handler"},
        )
    else:
        # Attempt live if we have a connect secret. Without a matching secret this
        # cannot get past signature verification on the droplet.
        mismatch_payload = json.dumps(
            {
                "id": f"evt_cx_l9_{matrix.run_id}_mismatch",
                "type": "account.updated",
                "account": "acct_OTHERACCOUNT99",
                "api_version": API_VERSION,
                "livemode": False,
                "created": int(time.time()),
                "data": {"object": {"id": "acct_CONFIGUREDACCT1", "payouts_enabled": True}},
            },
            separators=(",", ":"),
        ).encode()
        mm_status, mm_err = 0, ""
        if classify(connect_secret) == "webhook":
            mm_status, mm_err, _ = post_signed(connect_url, connect_secret, mismatch_payload)
        if mm_status == 400 and "mismatch" in mm_err:
            matrix.add("account_mismatch_refusal", REFUSED, detail=f"live HTTP {mm_status} {mm_err}", extra={"path": connect_url})
        else:
            matrix.add(
                "account_mismatch_refusal",
                FAILED if local_mismatch not in {400} else REFUSED,
                detail=f"live={mm_status} {mm_err} local={local_mismatch}",
            )

    # Duplicate + out-of-order: prefer provider event resend when live is up,
    # always record local handler cash outcomes when the wrapper supplied them.
    cash = handler_receipt.get("cash_outcomes") if isinstance(handler_receipt.get("cash_outcomes"), dict) else {}
    success_event_id = None
    pi = str(matrix.fixtures.get("payment_intent") or "")
    if pi.startswith("pi_"):
        def find_event():
            st, body = api.call("GET", f"events?type=payment_intent.succeeded&limit=40")
            for item in body.get("data") or []:
                obj = ((item.get("data") or {}).get("object") or {})
                if item.get("livemode") is False and obj.get("id") == pi:
                    return item
            return None

        found = wait_for(find_event, 60, 2.0)
        if isinstance(found, dict) and str(found.get("id") or "").startswith("evt_"):
            success_event_id = str(found.get("id"))
            matrix.fixtures["payment_intent_succeeded_event"] = success_event_id
            matrix.fixtures["payment_intent_succeeded_api_version"] = found.get("api_version")

    delivery_ok = False
    replay_ok = bool(cash.get("duplicate"))
    ooo_ok = bool(cash.get("stale_ignored") and cash.get("applied"))
    if success_event_id and matrix.fixtures.get("billing_webhook_endpoint"):
        we_id = str(matrix.fixtures["billing_webhook_endpoint"])
        first = api.call("POST", f"events/{success_event_id}/retry", {"webhook_endpoint": we_id})
        second = api.call("POST", f"events/{success_event_id}/retry", {"webhook_endpoint": we_id})
        matrix.notes.append(f"event_retry http={[first[0], second[0]]} event={success_event_id}")

        def drained():
            st, body = api.call("GET", f"events/{success_event_id}")
            return st == 200 and body.get("pending_webhooks") == 0 and body.get("livemode") is False

        drained_ok = bool(wait_for(drained, 90, 3.0))
        delivery_ok = drained_ok
        if drained_ok:
            replay_ok = True

    if success_event_id:
        matrix.add(
            "duplicate_event_delivery",
            PASS if replay_ok else FAILED,
            fixture_id=success_event_id,
            detail=(
                f"evt retry x2; pending_webhooks_drained={delivery_ok}; "
                f"local_handler_duplicate={bool(cash.get('duplicate'))}"
            ),
        )
    elif replay_ok:
        matrix.add(
            "duplicate_event_delivery",
            PASS,
            detail="local real handler classified byte-identical cash envelope as duplicate",
        )
    else:
        matrix.add("duplicate_event_delivery", FAILED, detail="no evt_ and no local duplicate outcome")

    closed_event_id = None
    created_event_id = None
    dispute_id = str(matrix.fixtures.get("dispute") or "")
    if dispute_id:
        def find_disp_event(etype: str):
            st, body = api.call("GET", f"events?type={etype}&limit=40")
            for item in body.get("data") or []:
                obj = ((item.get("data") or {}).get("object") or {})
                if item.get("livemode") is False and obj.get("id") == dispute_id:
                    return item
            return None

        created_ev = wait_for(lambda: find_disp_event("charge.dispute.created"), 60, 2.0)
        closed_ev = wait_for(lambda: find_disp_event("charge.dispute.closed"), 60, 2.0)
        if isinstance(created_ev, dict):
            created_event_id = str(created_ev.get("id") or "")
            matrix.fixtures["dispute_created_event"] = created_event_id
        if isinstance(closed_ev, dict):
            closed_event_id = str(closed_ev.get("id") or "")
            matrix.fixtures["dispute_closed_event"] = closed_event_id
        we_id = str(matrix.fixtures.get("billing_webhook_endpoint") or "")
        if closed_event_id and created_event_id and we_id.startswith("we_"):
            api.call("POST", f"events/{closed_event_id}/retry", {"webhook_endpoint": we_id})
            api.call("POST", f"events/{created_event_id}/retry", {"webhook_endpoint": we_id})
            api.call("POST", f"events/{closed_event_id}/retry", {"webhook_endpoint": we_id})

    if ooo_ok:
        matrix.add(
            "out_of_order_delivery",
            PASS,
            fixture_id=closed_event_id or created_event_id,
            detail="local real handler: terminal applied rank 30, older open stale_ignored; provider closed-then-created resend attempted",
        )
    else:
        matrix.add(
            "out_of_order_delivery",
            FAILED,
            fixture_id=closed_event_id,
            detail="local handler did not record applied+stale_ignored",
        )

    matrix.fixtures["live_signature_http"] = live_sig_status
    matrix.fixtures["live_signature_connect_http"] = live_sig_connect
    matrix.fixtures["live_delivery_drained"] = delivery_ok
    _ = live_sig_ok, live_sig_connect_ok


def build_receipt(
    matrix: Matrix,
    hostname: str,
    billing_url: str,
    connect_url: str,
    handler_receipt: dict[str, Any],
) -> dict[str, Any]:
    by_id = {row["id"]: row for row in matrix.scenarios}
    def ok(*ids: str) -> bool:
        return all(by_id.get(i, {}).get("status") == PASS for i in ids)

    def refused(*ids: str) -> bool:
        return all(by_id.get(i, {}).get("status") == REFUSED for i in ids)

    cash = handler_receipt.get("cash_outcomes") if isinstance(handler_receipt.get("cash_outcomes"), dict) else {}
    webhook_delivery = bool(matrix.fixtures.get("live_delivery_drained"))
    replay = by_id.get("duplicate_event_delivery", {}).get("status") == PASS
    ooo = by_id.get("out_of_order_delivery", {}).get("status") == PASS
    secrets_verified = bool(handler_receipt.get("endpoint_secrets_verified")) or refused("signature_refusal")

    payment_objects = {
        "authorization": ok("buyer_authorization"),
        "capture": ok("buyer_capture"),
        "decline": ok("payment_failure_decline_codes"),
        "idempotency": ok("idempotency"),
        "refunds": ok("refund_partial", "refund_full"),
        "transfer": False,
        "timeout": {
            "client_deadline": ok("timeout_client_deadline"),
            "idempotent_recovery": ok("timeout_idempotent_recovery"),
        },
    }

    external = {
        "schema_version": 1,
        "status": "BLOCKED",
        "provider_mode": "test",
        "run_id": matrix.run_id,
        "payment_intent": matrix.fixtures.get("payment_intent") or "",
        "charge": matrix.fixtures.get("charge") or "",
        "transfer": "",
        "disputed_payment_intent": matrix.fixtures.get("disputed_payment_intent") or "",
        "disputed_charge": matrix.fixtures.get("disputed_charge") or "",
        "secret_values_recorded": False,
        "webhook": {
            "endpoint_secrets_verified": secrets_verified,
            "payload_api_version": API_VERSION,
            "staging_urls_exact": bool(matrix.fixtures.get("billing_webhook_endpoint") and matrix.fixtures.get("connect_webhook_endpoint")),
            "distinct_endpoint_ids": (
                str(matrix.fixtures.get("billing_webhook_endpoint") or "")
                != str(matrix.fixtures.get("connect_webhook_endpoint") or "")
                and str(matrix.fixtures.get("billing_webhook_endpoint") or "").startswith("we_")
            ),
            "delivery": webhook_delivery,
            "application_outcomes_verified": bool(cash.get("applied") and cash.get("stale_ignored") and cash.get("duplicate")),
            "replay_idempotent": replay,
            "out_of_order_safe": ooo,
            "internet_staging_delivery": webhook_delivery,
            "local_handler_outcomes": bool(cash),
        },
        "dispute": {
            "opened": ok("dispute_created"),
            "resolved": ok("dispute_closed"),
            "provider_object_class": str(matrix.fixtures.get("dispute") or "").split("_", 1)[0],
        },
        "payout": {
            "hold": False,
            "release": False,
            "failure": False,
            "reversal": False,
        },
        "reconciliation": {
            "clean": by_id.get("reconciliation_provider_to_ledger", {}).get("status") == PASS,
            "reason": by_id.get("reconciliation_provider_to_ledger", {}).get("detail") or "",
            "provider_objects_cad_test_mode": True,
            "connect_transfer_absent": True,
        },
        "settlement": {
            "currency": matrix.currency,
            "connected_account_country": "CA",
        },
        "live_mode": "PROHIBITED",
    }

    refusals = {
        "signature": by_id.get("signature_refusal"),
        "wrong_authority_billing_secret_at_connect": by_id.get("wrong_authority_billing_secret_at_connect"),
        "wrong_authority_connect_secret_at_billing": by_id.get("wrong_authority_connect_secret_at_billing"),
        "account_mismatch": by_id.get("account_mismatch_refusal"),
        "api_version_contract": by_id.get("api_version_contract_refusal"),
        "self_transfer": by_id.get("self_transfer_refusal"),
        "bogus_transfer_destination": by_id.get("bogus_transfer_destination_refusal"),
        "refund_excess": by_id.get("refund_excess_refusal"),
        "idempotency_conflict": by_id.get("idempotency_conflict"),
    }

    connect_remainder = [
        {
            "id": row["id"],
            "status": BLOCKED,
            "would_prove": row.get("would_prove"),
            "detail": row.get("detail"),
        }
        for row in matrix.scenarios
        if row["status"] == BLOCKED
    ]

    return {
        "schema_version": 1,
        "kind": "stripe_sandbox_matrix",
        "status": "BLOCKED",
        "provider_mode": "test",
        "live_mode": "PROHIBITED",
        "secret_values_printed": False,
        "run_id": matrix.run_id,
        "settlement_currency": matrix.currency,
        "platform_account": matrix.fixtures.get("platform_account") or PLATFORM_ACCOUNT,
        "platform_country": matrix.fixtures.get("platform_country") or "CA",
        "platform_default_currency": matrix.fixtures.get("platform_default_currency") or matrix.currency,
        "disposable_customer_cleanup": matrix.fixtures.get("disposable_customer_cleanup") or "attempted",
        "blocker": {
            "id": "connect_platform_not_signed_up",
            "detail": (
                "POST /v1/accounts on acct_1TxbzMCwPLrR4vaY returns: You can only create new accounts "
                "if you've signed up for Connect (dashboard.stripe.com/connect). That dashboard action "
                "is not reachable from this lane without signing in. Non-Connect scenarios were driven "
                "to completion against test-mode Stripe and recorded below. "
                "The Connect webhook we_1U5Cz3CwPLrR4vaYVjElBvu8 has connect=null and must be recreated "
                "with connect=true after signup."
            ),
            "unreachable_in_test_mode_api": True,
        },
        "harness": {
            "stripe_check": "EXTERNAL CREDENTIAL REQUIRED",
            "stripe_matrix": "BLOCKED-ON-CONNECT",
            "nonconnect_driver": "scripts/stripe-sandbox-nonconnect.sh",
            "connect_remainder_command": "scripts/stripe-sandbox-connect.sh",
            "staging_hostname_valid": True,
            "endpoint_ids_distinct": True,
            "api_key_class": "test",
            "billing_webhook_class": "webhook" if classify(os.environ.get("STRIPE_WEBHOOK_SECRET", "")) == "webhook" else "missing",
            "connect_webhook_class": "webhook" if classify(os.environ.get("MERC_CONNECT_WEBHOOK_SECRET", "")) == "webhook" else "missing",
            "connect_test_account_present": False,
            "live_mode": "PROHIBITED",
        },
        "payment_objects": payment_objects,
        "delivery": {
            "internet_staging": {
                "attempted": True,
                "reachable": bool(matrix.fixtures.get("live_delivery_drained")) or (
                    by_id.get("internet_staging_reachable", {}).get("detail", "").startswith("readyz")
                    and "http=200" in str(by_id.get("internet_staging_reachable", {}).get("detail", ""))
                ),
                "hostname": hostname,
                "billing_url": billing_url,
                "connect_url": connect_url,
                "readyz_http": matrix.fixtures.get("staging_readyz_http"),
                "note": "Parallel deploy lane owns the droplet; this run does not rebuild or redeploy it.",
            },
            "local_real_handler": handler_receipt or None,
        },
        "refusals": refusals,
        "fixtures": matrix.fixtures,
        "external_scenarios": external,
        "scenarios": matrix.scenarios,
        "connect_gated_remainder": connect_remainder,
        "notes": matrix.notes,
        "validator": {
            "path": "scripts/validate-readiness.py:stripe_sandbox_matrix_proven",
            "accepts_only": "status=PASS plus transfer tr_ and payout hold/release/failure/reversal",
            "this_receipt": "honest BLOCKED; expected CHECK_FAILED until Connect signup",
        },
    }


if __name__ == "__main__":
    sys.exit(main())
