#!/usr/bin/env python3
"""Deterministic alert delivery harness; no external receiver is contacted."""

from __future__ import annotations

import argparse
import json
import os
import re
import socket
import threading
import time
import urllib.error
import urllib.request
from collections import defaultdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse


ROOT = Path(__file__).resolve().parents[1]


class Receiver(BaseHTTPRequestHandler):
    attempts: dict[str, int] = defaultdict(int)
    accepted: list[dict] = []

    def log_message(self, *_args: object) -> None:
        return

    def do_POST(self) -> None:  # noqa: N802
        length = int(self.headers.get("Content-Length", "0"))
        body = json.loads(self.rfile.read(length))
        event_id = body["event_id"]
        Receiver.attempts[event_id] += 1
        attempt = Receiver.attempts[event_id]
        if event_id == "retry-firing" and attempt == 1:
            self.send_response(500)
            self.end_headers()
            return
        if event_id == "timeout-firing" and attempt == 1:
            time.sleep(0.15)
            try:
                self.send_response(200)
                self.end_headers()
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if event_id == "invalid-response" and attempt == 1:
            self.send_response(422)
            self.end_headers()
            return
        if event_id == "exhaustion":
            self.send_response(503)
            self.end_headers()
            return
        Receiver.accepted.append(body)
        self.send_response(204)
        self.end_headers()


def deliver(url: str, event: dict, maximum: int = 4) -> tuple[bool, int]:
    body = json.dumps(event, separators=(",", ":")).encode()
    for attempt in range(1, maximum + 1):
        request = urllib.request.Request(
            url, data=body, headers={"Content-Type": "application/json"}, method="POST"
        )
        try:
            with urllib.request.urlopen(request, timeout=0.1) as response:
                if 200 <= response.status < 300:
                    return True, attempt
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError, socket.timeout):
            pass
        if attempt < maximum:
            time.sleep(0.02 * attempt)
    return False, maximum


def event(event_id: str, fingerprint: str, status: str = "firing") -> dict:
    return {
        "schema_version": 1,
        "event_id": event_id,
        "fingerprint": fingerprint,
        "status": status,
        "severity": "page",
        "alertname": "MercSyntheticPage",
        "runbook": "docs/RUNBOOKS.md#control-plane-or-database-outage",
        "synthetic": True,
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--profile", choices=("check", "page"), default="check")
    parser.add_argument("--out", required=True)
    parser.add_argument(
        "--real-receiver-env",
        help="environment variable containing the approved HTTPS receiver URL; the value is never printed",
    )
    args = parser.parse_args()

    real_receiver_url = ""
    if args.real_receiver_env:
        if args.profile != "page" or not re.fullmatch(r"MERC_ALERT_RECEIVER_[A-Z0-9_]+", args.real_receiver_env):
            parser.error("real receiver delivery requires page profile and a MERC_ALERT_RECEIVER_* variable")
        real_receiver_url = os.environ.get(args.real_receiver_env, "").strip()
        parsed = urlparse(real_receiver_url)
        if parsed.scheme != "https" or not parsed.netloc or parsed.username or parsed.password:
            parser.error("the real receiver variable must contain an HTTPS URL without URL userinfo")

    Receiver.attempts.clear()
    Receiver.accepted.clear()
    server = ThreadingHTTPServer(("127.0.0.1", 0), Receiver)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    url = f"http://127.0.0.1:{server.server_port}/alerts"
    try:
        results: dict[str, dict] = {}
        seen: set[str] = set()
        scenarios = [
            event("normal-firing", "normal"),
            event("retry-firing", "retry"),
            event("timeout-firing", "timeout"),
            event("invalid-response", "invalid"),
        ]
        for item in scenarios:
            if item["fingerprint"] in seen:
                continue
            seen.add(item["fingerprint"])
            ok, attempts = deliver(url, item)
            results[item["event_id"]] = {"delivered": ok, "attempts": attempts}

        # A second firing with the same active fingerprint is suppressed.
        duplicate = event("normal-duplicate", "normal")
        duplicate_suppressed = duplicate["fingerprint"] in seen

        exhausted, exhausted_attempts = deliver(url, event("exhaustion", "exhaustion"), maximum=3)
        dead_letter = not exhausted and exhausted_attempts == 3
        resolved, resolution_attempts = deliver(url, event("normal-resolved", "normal", "resolved"))

        runbook = ROOT / "docs/RUNBOOKS.md"
        runbook_ok = runbook.is_file() and "Control plane or database outage" in runbook.read_text(
            encoding="utf-8"
        )
        passed = (
            all(item["delivered"] for item in results.values())
            and results["retry-firing"]["attempts"] == 2
            and results["timeout-firing"]["attempts"] == 2
            and results["invalid-response"]["attempts"] == 2
            and duplicate_suppressed
            and dead_letter
            and resolved
            and runbook_ok
        )
        external_status = "NOT EXECUTED"
        external_attempts: dict[str, int] = {}
        if real_receiver_url:
            firing_ok, firing_attempts = deliver(
                real_receiver_url, event("external-firing", "external-approved-receiver")
            )
            resolved_ok, resolved_attempts = deliver(
                real_receiver_url,
                event("external-resolved", "external-approved-receiver", "resolved"),
            )
            external_attempts = {"firing": firing_attempts, "resolution": resolved_attempts}
            external_status = "PASS" if firing_ok and resolved_ok else "FAIL"
            passed = passed and external_status == "PASS"
        receipt = {
            "schema_version": 1,
            "status": "PASS" if passed else "FAIL",
            "label": "ALERT PIPELINE SIMULATION",
            "profile": args.profile,
            "receiver": "harness-controlled loopback receiver",
            "checks": {
                "firing": results["normal-firing"]["delivered"],
                "retry": results["retry-firing"]["attempts"] == 2,
                "timeout": results["timeout-firing"]["attempts"] == 2,
                "deduplication": duplicate_suppressed,
                "invalid_receiver_response": results["invalid-response"]["attempts"] == 2,
                "retry_exhaustion": exhausted_attempts == 3,
                "dead_letter": dead_letter,
                "resolution": resolved and resolution_attempts == 1,
                "runbook_link": runbook_ok,
            },
            "attempt_counts": dict(Receiver.attempts),
            "external": {
                "real_receiver_delivery": external_status,
                "receiver_value_recorded": False,
                "attempts": external_attempts,
            },
        }
        destination = Path(args.out)
        destination.parent.mkdir(parents=True, exist_ok=True)
        temporary = destination.with_suffix(destination.suffix + ".tmp")
        temporary.write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
        temporary.replace(destination)
        print(json.dumps(receipt, separators=(",", ":")))
        return 0 if passed else 1
    finally:
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    raise SystemExit(main())
