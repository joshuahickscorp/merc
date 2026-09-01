#!/usr/bin/env python3
"""Refuse a Stripe webhook endpoint that is not subscribed to what the code handles.

This exists because of a real, silent money defect found on the live staging
plane during alpha closeout. The billing endpoint had been created with a
plausible-looking event list that omitted `setup_intent.succeeded` and both
`charge.dispute.funds_*` events. Those are cash events: `stripe_cash_events.go`
gives them a non-zero EffectRank and applies them to the ledger. Stripe was
never going to send them, the handler was never going to run, and nothing was
red. Money would simply not have moved, and no test on this machine would have
noticed, because every local test constructs its own event.

The failure mode is subscription drift, so the check compares two things that
drift independently: the event types the Go handlers actually switch on, and
the event types the live endpoint is registered for.

It reads the handled set out of the source rather than from a hand-maintained
list here, because a second hand-maintained list is the same defect again.

Network access and a test-mode key are required to check the live side. With no
key present the script reports the compiled expectation and exits 0 — it must
not fail a CI run that has no Stripe credential, and it must not pretend to have
checked something it did not.

    python3 ops/scripts/validate-stripe-endpoint-subscriptions.py

Never prints a secret. Never accepts a live key.
"""

from __future__ import annotations

import json
import os
import re
import sys
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent.parent
BILLING_SOURCES = ("src/control/billing.go", "src/control/stripe_cash_events.go", "src/control/stripe_risk_events.go")
CONNECT_SOURCES = ("src/control/suppliers.go", "src/control/stripe_connect_events.go")

EVENT_RE = re.compile(r'"((?:payment_intent|charge|customer|payment_method|setup_intent|radar)\.[a-z_.]+)"')
CONNECT_EVENT_RE = re.compile(r'"((?:account|transfer|payout|application)\.[a-z_.]+)"')


def handled_events(sources: tuple[str, ...], pattern: re.Pattern[str]) -> set[str]:
    found: set[str] = set()
    for rel in sources:
        path = ROOT / rel
        if not path.exists():
            continue
        found |= set(pattern.findall(path.read_text(encoding="utf-8")))
    return found


def stripe_get(key: str, path: str) -> dict:
    req = urllib.request.Request(f"https://api.stripe.com/v1/{path}")
    # Basic auth with the secret as username and an empty password, which is
    # what Stripe expects. The key never reaches argv or a log line.
    import base64

    token = base64.b64encode(f"{key}:".encode()).decode()
    req.add_header("Authorization", f"Basic {token}")
    with urllib.request.urlopen(req, timeout=30) as resp:
        return json.load(resp)


def read_test_key() -> str | None:
    """Find a test-mode key without ever touching a live one."""
    for name in ("STRIPE_SECRET_KEY",):
        value = os.environ.get(name, "")
        if value.startswith(("sk_test_", "rk_test_")):
            return value
        if value.startswith(("sk_live_", "rk_live_")):
            print("stripe-endpoint-subscriptions: refusing a live key", file=sys.stderr)
            return None
    for candidate in (".merc-credentials.env", ".env", ".env.go-closure"):
        path = ROOT / candidate
        if not path.exists():
            continue
        for line in path.read_text(encoding="utf-8").splitlines():
            line = line.strip().removeprefix("export ").strip()
            if not line.startswith("STRIPE_SECRET_KEY="):
                continue
            value = line.split("=", 1)[1].strip()
            if value.startswith(("sk_test_", "rk_test_")):
                return value
    return None


def main() -> int:
    billing_expected = handled_events(BILLING_SOURCES, EVENT_RE)
    connect_expected = handled_events(CONNECT_SOURCES, CONNECT_EVENT_RE)
    if not billing_expected:
        print("stripe-endpoint-subscriptions: FAIL no handled billing events found in source")
        return 1

    print(f"compiled billing handlers: {len(billing_expected)} event types")
    for name in sorted(billing_expected):
        print(f"  {name}")
    print(f"compiled connect handlers: {len(connect_expected)} event types")
    for name in sorted(connect_expected):
        print(f"  {name}")

    key = read_test_key()
    if not key:
        print("stripe-endpoint-subscriptions: SKIP live comparison (no test-mode key present)")
        return 0

    try:
        endpoints = stripe_get(key, "webhook_endpoints?limit=100").get("data", [])
    except (urllib.error.URLError, urllib.error.HTTPError, TimeoutError) as exc:
        print(f"stripe-endpoint-subscriptions: SKIP live comparison (Stripe unreachable: {exc})")
        return 0

    failures = 0
    for endpoint in endpoints:
        url = endpoint.get("url", "")
        registered = set(endpoint.get("enabled_events") or [])
        if "*" in registered:
            continue  # a wildcard subscription cannot drop an event
        if url.endswith("/v1/stripe/webhook"):
            expected, label = billing_expected, "billing"
        elif url.endswith("/v1/stripe/connect-webhook"):
            expected, label = connect_expected, "connect"
        else:
            continue
        missing = sorted(expected - registered)
        if missing:
            failures += 1
            print(
                f"stripe-endpoint-subscriptions: FAIL {label} endpoint {endpoint.get('id')} "
                f"is not subscribed to handled events: {', '.join(missing)}"
            )

    if failures:
        print(
            "stripe-endpoint-subscriptions: an unsubscribed cash event is a silent ledger gap, "
            "not a warning"
        )
        return 1

    print("stripe-endpoint-subscriptions: PASS live endpoints cover every handled event")
    return 0


if __name__ == "__main__":
    sys.exit(main())
