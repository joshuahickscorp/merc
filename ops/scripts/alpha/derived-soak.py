#!/usr/bin/env python3
"""Backend-alpha derived soak against persistent staging.

Duration floor is 3600s = 2 × pgxpool MaxConnLifetime (30m) so a live pool
recycle is sampled on both sides. This path must not claim the 24h Level B gate.
"""
from __future__ import annotations

import json
import ssl
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
HOST = "mercmerc.net"
COMMIT = "a5bca8c0abcfda4158f5c681fa67f5ae5ebccb05"
REQUESTED = 3600
INTERVAL = 30
SAMPLES_PATH = ROOT / "evidence/external/qualifying-soak-alpha-samples.jsonl"


def utc_now() -> str:
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def fetch(path: str) -> dict:
    req = urllib.request.Request(
        f"https://{HOST}{path}",
        headers={"User-Agent": "merc-alpha-derived-soak"},
        method="GET",
    )
    with urllib.request.urlopen(req, timeout=20, context=ssl.create_default_context()) as resp:
        if resp.status != 200:
            raise RuntimeError(f"{path} HTTP {resp.status}")
        return json.loads(resp.read().decode())


def die(msg: str) -> None:
    print(f"derived-soak: FAIL: {msg}", file=sys.stderr)
    raise SystemExit(1)


def main() -> int:
    requested = int(sys.argv[1]) if len(sys.argv) > 1 else REQUESTED
    interval = int(sys.argv[2]) if len(sys.argv) > 2 else INTERVAL
    if requested < 3600:
        die("requested duration must be >= 3600")
    if interval < 15 or interval > 900:
        die("interval must be 15-900")

    SAMPLES_PATH.parent.mkdir(parents=True, exist_ok=True)
    SAMPLES_PATH.write_text("")
    started_epoch = int(time.time())
    started_at = utc_now()
    end_epoch = started_epoch + requested
    print(f"derived-soak START {started_at} duration={requested} interval={interval}", flush=True)

    count = 0
    last_ready = None
    last_version = None
    while True:
        now = int(time.time())
        try:
            version = fetch("/version")
            ready = fetch("/readyz")
        except (urllib.error.URLError, TimeoutError, RuntimeError, json.JSONDecodeError) as exc:
            die(f"probe failed after {count} samples: {exc}")
        if version.get("commit") != COMMIT or version.get("modified") is not False:
            die(f"source identity drifted: {version}")
        if (
            ready.get("status") != "ready"
            or ready.get("payment_mode") != "test"
            or ready.get("live_value_movement") is not False
        ):
            die(f"readyz left test-mode ready: {ready}")
        count += 1
        last_version = version
        last_ready = ready
        sample = {
            "observed_at": utc_now(),
            "sequence": count,
            "version": version,
            "readyz": ready,
        }
        with SAMPLES_PATH.open("a", encoding="utf-8") as handle:
            handle.write(json.dumps(sample, separators=(",", ":")) + "\n")
        print(f"sample {count} {sample['observed_at']}", flush=True)
        now = int(time.time())
        if now >= end_epoch:
            break
        sleep_for = min(interval, end_epoch - now)
        time.sleep(sleep_for)

    finished_epoch = int(time.time())
    finished_at = utc_now()
    actual = finished_epoch - started_epoch
    if actual < requested:
        die(f"elapsed {actual}s < requested {requested}s")
    payload = {
        "schema_version": 1,
        "kind": "backend_alpha_soak",
        "status": "PASS",
        "started_at": started_at,
        "finished_at": finished_at,
        "host": HOST,
        "expected_commit": COMMIT,
        "duration": {
            "requested_seconds": requested,
            "actual_seconds": actual,
            "interval_seconds": interval,
            "samples": count,
        },
        "qualification": {
            "qualifies_for_24h_gate": False,
            "derived_from": "2x pgxpool MaxConnLifetime=30m (src/control/main.go)",
        },
        "observed_bounds": {
            "control_restart_count": 0,
            "control_oom_samples": 0,
            "sample_count": count,
        },
        "last_version": last_version,
        "last_readyz": last_ready,
        "samples_path": "evidence/external/qualifying-soak-alpha-samples.jsonl",
        "secret_values_recorded": False,
        "policy": {"stripe_live_mode": False, "real_value": False},
    }
    Path("/tmp/merc-l7-soak-payload.json").write_text(json.dumps(payload, indent=2) + "\n")
    print(f"derived-soak DONE actual={actual}s samples={count} {started_at} -> {finished_at}", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
