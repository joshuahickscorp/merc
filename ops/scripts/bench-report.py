#!/usr/bin/env python3
"""Join benchmark runs to the economics and emit one table per profile.

Columns, per goal item 11:
  TTFT, ITL, total goodput, energy per million accepted tokens,
  supplier floor, CX floor, market price, contribution margin.

Every row is labelled with hardware, runtime, batch, prompt/output lengths,
prefix reuse and energy provenance (goal item 14) -- a speed number without
those labels is not a claim anyone can check.

    python3 ops/scripts/bench-report.py [--runs evidence/bench/runs.jsonl]
"""

import argparse
import json
import sys
from pathlib import Path

# Economics, kept in step with src/control/pricing.go and ops/configs/pricing/board.json.
SUPPLIER_SHARE = 0.97
ELECTRICITY_USD_PER_KWH = 0.15
# Sustained package power. Measured when powermetrics is available; otherwise
# this is the documented apple_silicon_pro figure src/control/pricing.go uses, and
# rows derived from it are marked ASSUMED rather than presented as measured.
ASSUMED_WATTS = 30.0
MARKET_PRICE_USD_PER_1K = 0.000036  # confidence-weighted median x positioning


def money(v, places=6):
    return f"${v:.{places}f}"


def load(path: Path):
    rows = []
    for line in path.read_text().splitlines():
        line = line.strip()
        if line:
            rows.append(json.loads(line))
    return rows


def economics(row):
    """Supplier floor, CX floor, and contribution at the market price."""
    # Physical work is what consumes energy and supplier time. Using billed
    # tokens here would make a bigger cache hit look like a more efficient
    # machine.
    goodput = row.get("physical_tokens_per_s") or row["goodput_tokens_per_s"]
    watts = row["mean_watts"]
    measured_power = watts is not None
    if not measured_power:
        watts = ASSUMED_WATTS

    elec_hr = watts / 1000.0 * ELECTRICITY_USD_PER_KWH
    units_hr = goodput * 3600.0

    # Supplier floor: the price per 1k units at which the supplier exactly
    # covers electricity, given this profile's throughput.
    supplier_floor = (elec_hr / (units_hr / 1000.0) / SUPPLIER_SHARE) if units_hr > 0 else float("inf")
    # CX floor: the price at which the platform's share covers nothing but is
    # non-negative -- i.e. the supplier floor grossed up by the platform share.
    cx_floor = supplier_floor / SUPPLIER_SHARE if units_hr > 0 else float("inf")

    revenue_hr = units_hr / 1000.0 * MARKET_PRICE_USD_PER_1K
    supplier_gross_hr = revenue_hr * SUPPLIER_SHARE
    contribution_hr = supplier_gross_hr - elec_hr
    return {
        "elec_hr": elec_hr,
        "supplier_floor": supplier_floor,
        "cx_floor": cx_floor,
        "contribution_hr": contribution_hr,
        "measured_power": measured_power,
    }


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--runs", default="evidence/bench/runs.jsonl")
    args = ap.parse_args()

    path = Path(args.runs)
    if not path.exists():
        print(f"no runs at {path}; run ops/scripts/bench-harness.py first", file=sys.stderr)
        return 2
    rows = load(path)
    if not rows:
        print("no runs recorded", file=sys.stderr)
        return 2

    hw = rows[0]["hardware"]
    runtime = rows[0]["runtime"]
    model = rows[0]["model"]
    powered = [r for r in rows if r["mean_watts"] is not None]
    power_note = (
        f"measured via powermetrics on {len(powered)}/{len(rows)} runs"
        if powered else
        f"NOT MEASURED - {rows[0]['power_unavailable_reason']}"
    )

    print(f"# Benchmark profiles\n")
    print(f"- hardware: **{hw}**")
    print(f"- runtime: **{runtime}**")
    print(f"- model: **{model}**")
    print(f"- quality: greedy argmax decoding, no sampling; outputs not scored for quality")
    print(f"- energy: **{power_note}**")
    print(f"- market price: {money(MARKET_PRICE_USD_PER_1K, 8)}/1k units "
          f"(confidence-weighted median x positioning multiplier)")
    print(f"- supplier share: {SUPPLIER_SHARE:.0%}, electricity ${ELECTRICITY_USD_PER_KWH}/kWh")
    if not powered:
        print(f"- **energy columns are null by design**: this harness does not "
              f"estimate watts and present them as measurement.")
    print()

    for wl in ("INTERACTIVE", "BATCH", "SHARED_PREFIX_BATCH"):
        sel = [r for r in rows if r["workload_class"] == wl]
        if not sel:
            continue
        print(f"## {wl}\n")
        print("| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | "
              "delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | "
              "contribution $/hr | err |")
        print("|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|")
        for r in sorted(sel, key=lambda x: (x["batch"], x["prompt_tokens"], x["output_tokens"])):
            e = economics(r)
            jm = r["joules_per_million_accepted_tokens"]
            prefix = r["prefix_state"]
            if r["shared_prefix_tokens"]:
                prefix += f"({r['shared_prefix_tokens']})"
            contribution = e["contribution_hr"]
            flag = "" if e["measured_power"] else "*"
            print(
                f"| {r['batch']} | {r['prompt_tokens']} | {r['output_tokens']} | {prefix} | "
                f"{r['ttft_ms']:.0f} | {r['itl_ms']:.2f} | "
                f"{r.get('physical_tokens_per_s', r['goodput_tokens_per_s']):.0f} | "
                f"{r['goodput_tokens_per_s']:.0f} | {r.get('prefix_reuse_inflation',1.0):.2f}x | "
                f"{jm if jm is not None else 'n/a'} | "
                f"{money(e['supplier_floor'], 8)} | {money(e['cx_floor'], 8)} | "
                f"{money(contribution, 4)}{flag} | {len(r['errors'])} |"
            )
        print()

    if not powered:
        print("`*` contribution derived from the documented "
              f"{ASSUMED_WATTS:.0f} W figure, **not** a measurement. "
              "Re-run under `sudo` for real energy.\n")

    best = max(rows, key=lambda r: r.get("physical_tokens_per_s") or r["goodput_tokens_per_s"])
    print("## Best observed profile\n")
    print(f"**{best.get('physical_tokens_per_s', best['goodput_tokens_per_s']):.0f} tok/s** "
          f"PHYSICAL ({best['goodput_tokens_per_s']:.0f} delivered, "
          f"{best.get('prefix_reuse_inflation',1.0):.2f}x reuse) - "
          f"{best['workload_class']}, batch {best['batch']}, "
          f"{best['prompt_tokens']} prompt / {best['output_tokens']} output, "
          f"prefix {best['prefix_state']}"
          + (f" ({best['shared_prefix_tokens']} shared)" if best["shared_prefix_tokens"] else "")
          + f", {runtime} on {hw}.")
    e = economics(best)
    print(f"\nAt the market price that is {money(e['contribution_hr'], 4)}/hr of supplier "
          f"contribution after electricity"
          + ("" if e["measured_power"] else " (assumed power)") + ".")
    return 0


if __name__ == "__main__":
    sys.exit(main())
