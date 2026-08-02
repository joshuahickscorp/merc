#!/usr/bin/env python3
"""Regression tests for benchmark token accounting.

These exist because merc published a 145x physical-inference figure that was
inflated 3.43x: a shared prefix was computed ONCE and counted once per stream.
The tests below make that specific error fail loudly rather than ship.

    python3 scripts/test-bench-accounting.py

No framework: this runs in CI next to the shell gates.

Evidence files are required. A gate that passes when it ran nothing is worse
than no gate. Opt out only with the greppable env var
MERC_BENCH_ACCOUNTING_ALLOW_MISSING=1 for a lane that genuinely has no
bench evidence (none of the current make ci paths set this).
"""

import json
import os
import sys
from pathlib import Path

FAILURES: list[str] = []

# Greppable opt-out for a CI lane that deliberately has no bench evidence.
# Main `make ci` does not set this; both evidence files are tracked.
ALLOW_MISSING_ENV = "MERC_BENCH_ACCOUNTING_ALLOW_MISSING"


def check(cond: bool, msg: str) -> None:
    if not cond:
        FAILURES.append(msg)


def allow_missing() -> bool:
    return os.environ.get(ALLOW_MISSING_ENV, "") == "1"


def physical_tokens(batch: int, prompt: int, output: int, shared: int) -> int:
    """The reference definition. Must match scripts/bench-harness.py."""
    unique = max(0, prompt - shared)
    return shared + batch * unique + batch * output


def billed_tokens(batch: int, prompt: int, output: int) -> int:
    return batch * (prompt + output)


# --------------------------------------------------------------- unit invariants

def test_definitions() -> None:
    # The exact profile that produced the retracted claim.
    b, p, o, s = 128, 192, 32, 160
    phys = physical_tokens(b, p, o, s)
    bill = billed_tokens(b, p, o)
    check(bill == 28672, f"billed should be 28672, got {bill}")
    check(phys == 8352, f"physical should be 8352, got {phys}")
    check(abs(bill / phys - 3.43) < 0.01,
          f"the retracted profile's inflation should be ~3.43x, got {bill/phys:.2f}")

    # No sharing means the two must be identical. If they ever diverge here the
    # accounting has picked up a phantom multiplier.
    for b, p, o in ((1, 128, 64), (64, 256, 16), (256, 128, 32)):
        check(physical_tokens(b, p, o, 0) == billed_tokens(b, p, o),
              f"without a shared prefix physical must equal billed at b={b} p={p} o={o}")

    # A shared prefix can never make physical work exceed billed work.
    for s in range(0, 193, 16):
        check(physical_tokens(128, 192, 32, s) <= billed_tokens(128, 192, 32),
              f"physical exceeded billed at shared={s}")

    # Sharing more prefix must not increase physical work.
    prev = None
    for s in range(0, 193, 16):
        cur = physical_tokens(128, 192, 32, s)
        if prev is not None:
            check(cur <= prev, f"physical work rose when sharing more prefix (shared={s})")
        prev = cur

    # The prefix is counted exactly once, never per stream.
    check(physical_tokens(1000, 200, 10, 200) == 200 + 1000 * 10,
          "a fully shared prompt must contribute its length once plus decode")


# --------------------------------------------------- recorded-evidence invariants

def test_recorded_runs(path: Path) -> None:
    if not path.exists():
        if allow_missing():
            print(f"note: {path} absent, {ALLOW_MISSING_ENV}=1 opt-out")
            return
        check(False,
              f"evidence input absent: {path} "
              f"(set {ALLOW_MISSING_ENV}=1 only for a lane with no bench evidence)")
        return
    rows = [json.loads(l) for l in path.read_text().splitlines() if l.strip()]
    check(bool(rows), f"{path} is empty")

    for r in rows:
        tag = (f"{r['workload_class']} b={r['batch']} p={r['prompt_tokens']} "
               f"o={r['output_tokens']} s={r['shared_prefix_tokens']}")

        expect_phys = physical_tokens(r["batch"], r["prompt_tokens"],
                                      r["output_tokens"], r["shared_prefix_tokens"])
        check(r["tokens_physical_total"] == expect_phys,
              f"{tag}: physical {r['tokens_physical_total']} != {expect_phys}")

        expect_bill = billed_tokens(r["batch"], r["prompt_tokens"], r["output_tokens"])
        check(r["tokens_billable_total"] == expect_bill,
              f"{tag}: billed {r['tokens_billable_total']} != {expect_bill}")

        check(r["tokens_physical_total"] <= r["tokens_billable_total"],
              f"{tag}: physical exceeds billed")

        check(r["tokens_prefix_reused_total"] ==
              r["tokens_billable_total"] - r["tokens_physical_total"],
              f"{tag}: reused tokens do not reconcile")

        # The headline number must never be the billed one.
        check(r["physical_tokens_per_s"] <= r["goodput_tokens_per_s"] + 1e-6,
              f"{tag}: physical rate exceeds delivered rate")

        if r["shared_prefix_tokens"] == 0:
            check(abs(r["prefix_reuse_inflation"] - 1.0) < 1e-6,
                  f"{tag}: no shared prefix but inflation is {r['prefix_reuse_inflation']}")
            check(abs(r["physical_tokens_per_s"] - r["goodput_tokens_per_s"]) < 1.0,
                  f"{tag}: no sharing, yet physical and delivered rates differ")

        # Energy must be charged against work actually done.
        if r.get("energy_joules") is not None:
            expect_jm = r["energy_joules"] / r["tokens_physical_total"] * 1e6
            got = r["joules_per_million_accepted_tokens"]
            check(abs(got - expect_jm) < 1.0,
                  f"{tag}: J/M token uses the wrong denominator ({got} vs {expect_jm:.1f})")


def test_frozen_benchmark(path: Path) -> None:
    if not path.exists():
        if allow_missing():
            print(f"note: {path} absent, {ALLOW_MISSING_ENV}=1 opt-out")
            return
        check(False,
              f"evidence input absent: {path} "
              f"(set {ALLOW_MISSING_ENV}=1 only for a lane with no bench evidence)")
        return
    d = json.loads(path.read_text())
    m = d.get("measured_2026_07_27", {})
    check(m.get("physical_tokens_per_s", 0) < m.get("billed_tokens_per_s", 0),
          "frozen benchmark must record physical below billed")
    forbidden = d.get("accounting", {}).get("forbidden", [])
    check(any("cache hits" in f for f in forbidden),
          "frozen benchmark must forbid counting cache hits as inference")


def main() -> int:
    test_definitions()
    test_recorded_runs(Path("evidence/bench/runs.jsonl"))
    test_frozen_benchmark(Path("bench/immutable/shared-prefix-v1.json"))

    if FAILURES:
        print(f"bench-accounting: FAIL ({len(FAILURES)})")
        for f in FAILURES:
            print(f"  - {f}")
        return 1
    print("bench-accounting: PASS (physical/billed separation holds; "
          "shared prefix counted once)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
