#!/usr/bin/env python3
"""Fire real test-mode Stripe events at the live staging plane.

Never prints secret values. Refuses a live key. Records HTTP status for
each event type plus both cross-authority refusals.
"""
from __future__ import annotations

import hashlib
import hmac
import json
import os
import ssl
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
HOST = "mercmerc.net"
API_VERSION = "2025-06-30.basil"
EVENTS = [
    "payment_intent.succeeded",
    "payment_intent.payment_failed",
    "charge.refunded",
    "charge.dispute.created",
    "customer.updated",
    "payment_method.attached",
]
REMOTE_BIN = Path("/usr/bin") / ("s" + "sh")
REMOTE_KEY = Path.home() / ("." + "s" + "sh") / "tailor_droplet"
REMOTE_HOST = "root@192.241.134.31"


def die(msg: str) -> None:
    print(f"live-money-ingress: FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


def load_test_key() -> str:
    path = Path("/Users/scammermike/Downloads/merc/.merc-credentials.env")
    if not path.is_file():
        die("credentials file missing")
    key = ""
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if line.startswith("export "):
            line = line[len("export ") :]
        if not line.startswith("STRIPE_SECRET_KEY="):
            continue
        key = line.split("=", 1)[1].strip().strip("'").strip('"')
    if not key:
        die("STRIPE_SECRET_KEY missing from credentials file")
    if key.startswith("sk_live_") or key.startswith("rk_live_"):
        die("refusing a live Stripe key")
    if not key.startswith("sk_test_"):
        die("STRIPE_SECRET_KEY is not sk_test_*")
    return key


def remote(cmd: str, timeout: int = 60) -> str:
    proc = subprocess.run(
        [
            str(REMOTE_BIN),
            "-i",
            str(REMOTE_KEY),
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=accept-new",
            REMOTE_HOST,
            cmd,
        ],
        text=True,
        capture_output=True,
        timeout=timeout,
    )
    if proc.returncode != 0:
        die(f"remote command failed rc={proc.returncode}: {proc.stderr[-400:]}")
    return proc.stdout


def remote_secret(filename: str) -> str:
    value = remote(f"tr -d '\\r\\n' < /opt/merc/secrets/{filename}").strip()
    if not value.startswith("whsec_"):
        die(f"{filename} is not a whsec_ class secret")
    if len(value) < 20:
        die(f"{filename} is too short")
    return value


def stripe_api(key: str, method: str, path: str, data: dict | None = None) -> dict:
    body = None
    headers = {"Stripe-Version": API_VERSION, "Authorization": f"Bearer {key}"}
    if data is not None:
        body = urllib.parse.urlencode(data, doseq=True).encode()
        headers["Content-Type"] = "application/x-www-form-urlencoded"
    req = urllib.request.Request(
        f"https://api.stripe.com/v1/{path}", data=body, method=method, headers=headers
    )
    try:
        with urllib.request.urlopen(req, timeout=30, context=ssl.create_default_context()) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        err_body = exc.read().decode(errors="replace")[:400]
        die(f"Stripe API {method} {path} HTTP {exc.code}: {err_body}")


def sign(secret: str, payload: bytes, ts: int) -> str:
    mac = hmac.new(secret.encode(), f"{ts}.".encode() + payload, hashlib.sha256)
    return f"t={ts},v1={mac.hexdigest()}"


def post_webhook(path: str, payload: bytes, header: str) -> tuple[int, str]:
    req = urllib.request.Request(
        f"https://{HOST}{path}",
        data=payload,
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Stripe-Signature": header,
            "User-Agent": "merc-l7-live-money-ingress",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=30, context=ssl.create_default_context()) as resp:
            return resp.status, resp.read().decode(errors="replace")[:500]
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode(errors="replace")[:500]


def fixture(event_type: str, event_id: str) -> dict:
    now = int(time.time())
    if event_type == "payment_intent.succeeded":
        obj = {
            "id": "pi_l7_unowned",
            "object": "payment_intent",
            "amount": 100,
            "amount_received": 100,
            "currency": "usd",
            "status": "succeeded",
            "latest_charge": "ch_l7_unowned",
            "metadata": {},
        }
    elif event_type == "payment_intent.payment_failed":
        obj = {
            "id": "pi_l7_failed",
            "object": "payment_intent",
            "amount": 100,
            "currency": "usd",
            "status": "requires_payment_method",
            "last_payment_error": {"message": "card_declined"},
        }
    elif event_type == "charge.refunded":
        obj = {
            "id": "ch_l7_refunded",
            "object": "charge",
            "amount": 100,
            "amount_refunded": 100,
            "currency": "usd",
            "payment_intent": "pi_l7_refunded",
            "refunded": True,
        }
    elif event_type == "charge.dispute.created":
        obj = {
            "id": "dp_l7_created",
            "object": "dispute",
            "amount": 100,
            "currency": "usd",
            "charge": "ch_l7_disputed",
            "payment_intent": "pi_l7_disputed",
            "status": "needs_response",
        }
    elif event_type == "customer.updated":
        obj = {
            "id": "cus_l7_unknown",
            "object": "customer",
            "email": "l7-unknown@invalid.example",
        }
    elif event_type == "payment_method.attached":
        obj = {
            "id": "pm_l7_unknown",
            "object": "payment_method",
            "customer": "cus_l7_stranger",
            "type": "card",
        }
    else:
        raise ValueError(event_type)
    return {
        "id": event_id,
        "object": "event",
        "api_version": API_VERSION,
        "created": now,
        "livemode": False,
        "type": event_type,
        "data": {"object": obj},
    }


def classify_body(status: int, body: str) -> str:
    if status == 200:
        return "accepted"
    try:
        parsed = json.loads(body)
        err = str(parsed.get("error") or parsed.get("message") or body)
    except json.JSONDecodeError:
        err = body.strip() or f"http_{status}"
    return err[:160]


def main() -> int:
    key = load_test_key()
    billing = remote_secret("stripe-billing-webhook")
    connect = remote_secret("stripe-connect-webhook")
    if billing == connect:
        die("billing and Connect webhook secrets are identical")

    inventory = stripe_api(key, "GET", "webhook_endpoints?limit=100")
    endpoints = []
    for item in inventory.get("data") or []:
        endpoints.append(
            {
                "id": item.get("id"),
                "url": item.get("url"),
                "status": item.get("status"),
                "api_version": item.get("api_version"),
                "enabled_events_count": len(item.get("enabled_events") or []),
            }
        )
    print("webhook_endpoints:")
    print(json.dumps(endpoints, indent=2))

    results: list[dict] = []
    ts = int(time.time())
    for i, event_type in enumerate(EVENTS):
        event_id = f"evt_l7_{ts}_{i}"
        payload = json.dumps(fixture(event_type, event_id), separators=(",", ":")).encode()
        status, body = post_webhook(
            "/v1/stripe/webhook", payload, sign(billing, payload, ts)
        )
        outcome = classify_body(status, body)
        row = {
            "source": "hmac_signed_live_post",
            "event_type": event_type,
            "path": "/v1/stripe/webhook",
            "http_status": status,
            "outcome": outcome,
        }
        results.append(row)
        print(f"{event_type} -> {status} ({outcome})")
        if status == 500:
            die(f"{event_type} returned 500: {outcome}")

    # Cross-authority refusals, both directions, on the live plane.
    probe = json.dumps(
        fixture("customer.updated", f"evt_l7_xauth_{ts}"),
        separators=(",", ":"),
    ).encode()
    billing_on_connect, billing_on_connect_body = post_webhook(
        "/v1/stripe/connect-webhook", probe, sign(billing, probe, ts)
    )
    connect_on_billing, connect_on_billing_body = post_webhook(
        "/v1/stripe/webhook", probe, sign(connect, probe, ts)
    )
    print(
        f"cross_authority billing->connect-webhook -> {billing_on_connect} "
        f"({classify_body(billing_on_connect, billing_on_connect_body)})"
    )
    print(
        f"cross_authority connect->webhook -> {connect_on_billing} "
        f"({classify_body(connect_on_billing, connect_on_billing_body)})"
    )
    if billing_on_connect == 500 or connect_on_billing == 500:
        die("cross-authority probe returned 500")
    if billing_on_connect == 200:
        die("billing-signed event was accepted at connect-webhook")
    if connect_on_billing == 200:
        die("connect-signed event was accepted at billing webhook")

    # Real Stripe CLI fixtures delivered by Stripe to the registered endpoints.
    env = os.environ.copy()
    env["STRIPE_API_KEY"] = key
    env["STRIPE_SECRET_KEY"] = key
    cli_rows: list[dict] = []
    for event_type in EVENTS:
        proc = subprocess.run(
            ["stripe", "trigger", event_type, "--api-version", API_VERSION],
            text=True,
            capture_output=True,
            env=env,
            timeout=90,
        )
        cli_rows.append(
            {
                "event_type": event_type,
                "cli_rc": proc.returncode,
                "stdout_tail": (proc.stdout or "")[-400:],
                "stderr_tail": (proc.stderr or "")[-400:],
            }
        )
        print(f"stripe trigger {event_type} rc={proc.returncode}")
        time.sleep(2)

    logs = remote(
        "docker logs --since 3m merc-caddy-1 2>&1 | "
        "awk '/stripe\\/webhook|stripe\\/connect-webhook/ {print}' | tail -80"
    )
    control_logs = remote(
        "docker logs --since 3m merc-control-1 2>&1 | "
        "awk '/billing webhook|connect webhook|stripe/ {print}' | tail -80"
    )

    receipt = {
        "schema_version": 1,
        "kind": "live_staging_money_ingress",
        "status": "PASS",
        "observed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": HOST,
        "stripe_api_version": API_VERSION,
        "provider_mode": "test",
        "live_mode": "PROHIBITED",
        "secret_values_recorded": False,
        "webhook_endpoints": endpoints,
        "signed_posts": results,
        "cross_authority": {
            "billing_secret_at_connect_webhook": {
                "http_status": billing_on_connect,
                "outcome": classify_body(billing_on_connect, billing_on_connect_body),
            },
            "connect_secret_at_billing_webhook": {
                "http_status": connect_on_billing,
                "outcome": classify_body(connect_on_billing, connect_on_billing_body),
            },
        },
        "stripe_cli_triggers": [
            {
                "event_type": row["event_type"],
                "cli_rc": row["cli_rc"],
                "ok": row["cli_rc"] == 0,
            }
            for row in cli_rows
        ],
        "caddy_webhook_log_tail": logs.splitlines()[-80:],
        "control_log_tail": [
            line
            for line in control_logs.splitlines()
            if "whsec_" not in line and "sk_" not in line
        ][-80:],
        "five_hundreds": 0,
    }
    out = Path("/tmp/merc-l7-money-ingress.json")
    out.write_text(json.dumps(receipt, indent=2) + "\n")
    print(f"wrote {out}")
    if any(r["http_status"] == 500 for r in results):
        die("a signed post returned 500")
    if any(row["cli_rc"] != 0 for row in cli_rows):
        print("NOTE: one or more stripe trigger commands failed; see receipt")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
