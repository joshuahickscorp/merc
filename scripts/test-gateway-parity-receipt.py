#!/usr/bin/env python3
"""CI gate for evidence/perf/gateway-parity.json.

Fails when the parity receipt is absent, undated, older than the revalidation
window (aligned with control/runtime_cell_performance.go
benchmarkRevalidationWindow = 180 days), or carries a non-empty refusals list.

Also runs offline unit checks that a receipt with no measured_at fails, that a
non-empty refusals list fails, and that budget evaluation produces refusals when
overhead exceeds the declared ceiling. Those unit checks import the harness so
a broken evaluate_budget cannot hide behind a green gate.

    python3 scripts/test-gateway-parity-receipt.py
    python3 scripts/test-gateway-parity-receipt.py --self-test-only

No live engine. No network.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import sys
import tempfile
from datetime import datetime, timedelta, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DEFAULT_RECEIPT = ROOT / "evidence" / "perf" / "gateway-parity.json"

# Keep aligned with control/runtime_cell_performance.go:benchmarkRevalidationWindow.
REVALIDATION_WINDOW = timedelta(days=180)

FAILURES: list[str] = []


def fail(msg: str) -> None:
    FAILURES.append(msg)


def check(cond: bool, msg: str) -> None:
    if not cond:
        fail(msg)


def load_harness():
    path = ROOT / "scripts" / "gateway-parity.py"
    spec = importlib.util.spec_from_file_location("gateway_parity", path)
    if spec is None or spec.loader is None:
        raise RuntimeError(f"cannot load {path}")
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def parse_measured_at(value: object) -> datetime | None:
    if not isinstance(value, str) or not value.strip():
        return None
    text = value.strip()
    try:
        if text.endswith("Z"):
            return datetime.fromisoformat(text[:-1] + "+00:00")
        return datetime.fromisoformat(text)
    except ValueError:
        return None


def validate_receipt(path: Path, *, now: datetime | None = None) -> list[str]:
    """Return a list of failure messages. Empty means the receipt clears the gate."""
    errors: list[str] = []
    if not path.exists():
        return [f"parity receipt absent: {path}"]

    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        return [f"parity receipt unreadable: {path}: {exc}"]

    if not isinstance(data, dict):
        return [f"parity receipt must be a JSON object: {path}"]

    measured_at = parse_measured_at(data.get("measured_at"))
    if measured_at is None:
        errors.append(
            f"{path}: measured_at missing or not RFC3339 "
            f"(got {data.get('measured_at')!r}); undated receipts are treated "
            "as no measurement at all"
        )
    else:
        at = now or datetime.now(timezone.utc)
        if measured_at.tzinfo is None:
            measured_at = measured_at.replace(tzinfo=timezone.utc)
        age = at - measured_at
        if age > REVALIDATION_WINDOW:
            errors.append(
                f"{path}: measured_at {data.get('measured_at')} is "
                f"{age.days} days old, past the {REVALIDATION_WINDOW.days}-day "
                "revalidation window"
            )

    commit = data.get("merc_source_commit")
    if not isinstance(commit, str) or not commit.strip():
        errors.append(f"{path}: merc_source_commit missing or empty")

    if data.get("gate_version") in (None, ""):
        errors.append(f"{path}: gate_version missing or empty")

    refusals = data.get("refusals")
    if refusals is None:
        errors.append(
            f"{path}: refusals field missing "
            "(empty list required when budget is met)"
        )
    elif not isinstance(refusals, list):
        errors.append(f"{path}: refusals must be a list, got {type(refusals).__name__}")
    elif refusals:
        errors.append(
            f"{path}: refusals is non-empty ({len(refusals)}): "
            + "; ".join(str(r) for r in refusals)
        )

    budget = data.get("budget")
    if not isinstance(budget, dict):
        errors.append(f"{path}: budget block missing")
    else:
        for key in ("ttft_overhead_p95_ms", "throughput_loss_fraction", "basis"):
            if key not in budget:
                errors.append(f"{path}: budget.{key} missing")

    scope = data.get("scope")
    if not isinstance(scope, dict):
        errors.append(f"{path}: scope block missing")
    else:
        for key in (
            "model_ref",
            "merc_model_artifact_sha256",
            "direct_model_artifact_sha256",
            "max_tokens",
            "temperature",
        ):
            if key not in scope:
                errors.append(f"{path}: scope.{key} missing")
        merc_d = scope.get("merc_model_artifact_sha256")
        direct_d = scope.get("direct_model_artifact_sha256")
        if (
            isinstance(merc_d, str)
            and isinstance(direct_d, str)
            and merc_d
            and direct_d
            and merc_d != direct_d
        ):
            errors.append(
                f"{path}: scope model artifact digests disagree "
                f"(merc={merc_d} direct={direct_d})"
            )

    if "features_disabled" not in data:
        errors.append(f"{path}: features_disabled missing")

    return errors


def run_unit_tests() -> None:
    """Demonstrate the five required behaviours against fixtures and harness logic."""
    harness = load_harness()

    with tempfile.TemporaryDirectory() as tmp:
        tmp_path = Path(tmp)

        # 1) Receipt with no measured_at fails the gate.
        undated = tmp_path / "undated.json"
        undated.write_text(
            json.dumps({
                "schema_version": 1,
                "kind": "gateway_parity",
                "gate_version": "gateway-parity-gate-v1",
                "merc_source_commit": "a" * 40,
                "refusals": [],
                "budget": {
                    "ttft_overhead_p95_ms": 15.0,
                    "throughput_loss_fraction": 0.05,
                    "basis": "test",
                },
                "scope": {
                    "model_ref": "cx-chat-1b",
                    "merc_model_artifact_sha256": "b" * 64,
                    "direct_model_artifact_sha256": "b" * 64,
                    "max_tokens": 32,
                    "temperature": 0,
                },
                "features_disabled": ["exact_reuse"],
            }),
            encoding="utf-8",
        )
        errs = validate_receipt(undated)
        check(any("measured_at" in e for e in errs),
              f"undated receipt must fail gate, got {errs!r}")

        # 2) Non-empty refusals fails the gate.
        refused = tmp_path / "refused.json"
        refused.write_text(
            json.dumps({
                "schema_version": 1,
                "kind": "gateway_parity",
                "gate_version": "gateway-parity-gate-v1",
                "measured_at": "2026-07-28T11:10:09Z",
                "merc_source_commit": "a" * 40,
                "refusals": ["ttft overhead p95 50.000 ms exceeds budget 15.000 ms"],
                "budget": {
                    "ttft_overhead_p95_ms": 15.0,
                    "throughput_loss_fraction": 0.05,
                    "basis": "test",
                },
                "scope": {
                    "model_ref": "cx-chat-1b",
                    "merc_model_artifact_sha256": "b" * 64,
                    "direct_model_artifact_sha256": "b" * 64,
                    "max_tokens": 32,
                    "temperature": 0,
                },
                "features_disabled": ["exact_reuse"],
            }),
            encoding="utf-8",
        )
        errs = validate_receipt(refused)
        check(any("refusals is non-empty" in e for e in errs),
              f"non-empty refusals must fail gate, got {errs!r}")

        # Absent receipt fails.
        missing = tmp_path / "no-such-receipt.json"
        errs = validate_receipt(missing)
        check(any("absent" in e for e in errs),
              f"absent receipt must fail, got {errs!r}")

        # Stale receipt fails.
        stale = tmp_path / "stale.json"
        old = (datetime.now(timezone.utc) - timedelta(days=200)).strftime(
            "%Y-%m-%dT%H:%M:%SZ"
        )
        stale.write_text(
            json.dumps({
                "schema_version": 1,
                "kind": "gateway_parity",
                "gate_version": "gateway-parity-gate-v1",
                "measured_at": old,
                "merc_source_commit": "a" * 40,
                "refusals": [],
                "budget": {
                    "ttft_overhead_p95_ms": 15.0,
                    "throughput_loss_fraction": 0.05,
                    "basis": "test",
                },
                "scope": {
                    "model_ref": "cx-chat-1b",
                    "merc_model_artifact_sha256": "b" * 64,
                    "direct_model_artifact_sha256": "b" * 64,
                    "max_tokens": 32,
                    "temperature": 0,
                },
                "features_disabled": ["exact_reuse"],
            }),
            encoding="utf-8",
        )
        errs = validate_receipt(stale)
        check(any("revalidation window" in e for e in errs),
              f"stale receipt must fail, got {errs!r}")

    # 3) Overhead above budget → non-empty refusals from the harness.
    budget = harness.default_budget()
    summary = {
        "ttft_overhead_p50_ms": 3.474,
        "ttft_overhead_p95_ms": 50.0,
        "ttft_overhead_p99_ms": 50.0,
    }
    deltas = {
        "1": {
            "aggregate_tokens_per_sec": {
                "merc": 144.85,
                "direct": 147.6,
                "delta": -2.75,
            }
        }
    }
    refusals = harness.evaluate_budget(summary, deltas, budget)
    check(len(refusals) > 0 and any("ttft overhead p95" in r for r in refusals),
          f"over-budget TTFT must produce refusals, got {refusals!r}")

    # 5) Scope round-trips; mismatched digests are incomparable.
    digest_a = "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
    digest_b = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
    scope = harness.build_scope(
        model="cx-chat-1b",
        merc_artifact_sha256=digest_a,
        direct_artifact_sha256=digest_a,
        precision="GGUF-Q4_K_M",
        engine="llama_cpp",
        engine_build="",
        hw_class="apple_silicon_ultra",
        max_tokens=32,
        temperature=0,
        stream=True,
        top_p=None,
        seed=None,
        prompt=harness.PROMPT,
        concurrency_levels=[1, 8, 32],
        requests_per_level=harness.MIN_CELL_COST_SAMPLES,
    )
    check(json.loads(json.dumps(scope)) == scope, "scope must round-trip")
    reasons = harness.assert_comparable(
        "cx-chat-1b", 32,
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"accepted": True}, {"accepted": True},
        "merc", "direct",
        merc_artifact_sha256=digest_a,
        direct_artifact_sha256=digest_b,
    )
    check(any("mismatch" in r for r in reasons),
          f"mismatched digests must be incomparable, got {reasons!r}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--receipt",
        type=Path,
        default=DEFAULT_RECEIPT,
        help="path to the gateway parity receipt",
    )
    ap.add_argument(
        "--self-test-only",
        action="store_true",
        help="run offline unit tests only; skip the committed receipt",
    )
    args = ap.parse_args()

    run_unit_tests()

    if not args.self_test_only:
        for err in validate_receipt(args.receipt):
            fail(err)

    if FAILURES:
        print(f"gateway-parity-receipt: FAIL ({len(FAILURES)})")
        for f in FAILURES:
            print(f"  - {f}")
        return 1
    print(
        "gateway-parity-receipt: PASS "
        "(dated, inside revalidation window, empty refusals)"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
