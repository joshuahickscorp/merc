#!/usr/bin/env python3
"""Two-worker prefix routing: does sending a prompt to the worker that holds its
prefix beat sending it to one that does not?

The single-worker receipt (evidence/perf/prefix-kv-physical-metal-latest.json)
proved that llama.cpp reuses a prefix on the SAME server: a second request
carrying the same prompt skipped ~410 ms of prefill. That is a cache
measurement. It is not a routing result, and it must never be quoted as one --
one worker cannot demonstrate a placement decision, because there was no
alternative placement to lose.

This measures the routing question directly. Two llama-server processes, the
same model file by sha256, one holding prefix P_i and one that has never seen
it. The same request goes to both. Paired, interleaved, and repeated with a
fresh prefix each round so the cold arm stays genuinely cold instead of warming
itself up over the run.

What it can prove:

  - cached_tokens / cache_n on the warm worker and their absence on the cold one
  - prefill tokens avoided, prompt_ms and end-to-end wall deltas at p50/p95
  - GPU-domain joules per arm (IOReport, AGX domain only)
  - that warm and cold produce byte-identical text at temperature 0, so the
    saving is not being bought with a different answer
  - that a restart resets the belief: the engine reports an observed MISS
    (cached_tokens == 0, not absent), which is the signal the control plane
    needs to contradict a stale warm row instead of waiting out its TTL

What it cannot prove, and must not be read as proving:

  - a cross-supplier network advantage. Both processes are on this host. This
    is same-host, two-process placement.
  - a cost win. Supplier entitlement is units x price x share and does not
    depend on duration, so the only cost term that can differ here is energy.
    The receipt states that delta in dollars precisely so nobody has to guess
    whether it is material.
  - fleet behaviour, or anything about a second hardware class.
"""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
import statistics
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))


def _load(name: str, path: Path):
    spec = importlib.util.spec_from_file_location(name, path)
    mod = importlib.util.module_from_spec(spec)
    sys.modules[name] = mod
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


# Reuse the single-worker harness rather than growing a second copy of
# start_server / chat_completion / energy sampling that can drift from it.
_pk = _load("merc_prefix_physical", ROOT / "scripts" / "prefix-physical-kv-measure.py")

percentile = _pk.percentile
host_load = _pk.host_load
free_port = _pk.free_port
wait_http = _pk.wait_http
start_server = _pk.start_server
stop_server = _pk.stop_server
build_prefix = _pk.build_prefix
chat_completion = _pk.chat_completion
extract_signals = _pk.extract_signals
energy_around = _pk.energy_around
hardware_label = _pk.hardware_label
DEFAULT_MODEL = _pk.DEFAULT_MODEL

WRITE_ENV = "MERC_WRITE_PREFIX_TWO_WORKER"

# Same constants the control plane prices energy with. Duplicated here with the
# path so a reader can check them rather than trust them.
ELECTRICITY_USD_PER_KWH = 0.15  # control/pricing.go:defaultElectricityUSDPerKWh
ELECTRICITY_SOURCE = "control/pricing.go:defaultElectricityUSDPerKWh"


def _stat(xs: list[float]) -> dict:
    xs = [x for x in xs if x is not None]
    if not xs:
        return {"n": 0}
    return {
        "n": len(xs),
        "p50": percentile(xs, 50),
        "p95": percentile(xs, 95),
        "mean": statistics.fmean(xs),
        "min": min(xs),
        "max": max(xs),
    }


def summarize(samples: list[dict]) -> dict:
    def col(k):
        return [s[k] for s in samples if s.get(k) is not None]

    return {
        "n": len(samples),
        "prompt_ms": _stat(col("prompt_ms")),
        "wall_ms": _stat(col("wall_ms")),
        "cached_tokens": _stat(col("cached_tokens")),
        "cache_n": _stat(col("cache_n")),
        "prompt_n": _stat(col("prompt_n")),
        "cached_tokens_field_present_all": all(
            s.get("cached_tokens_field_present") for s in samples
        )
        if samples
        else False,
        "raw_samples": samples,
    }


def run(args: argparse.Namespace) -> dict:
    model = Path(args.model).expanduser()
    if not model.exists():
        raise RuntimeError(f"model not found: {model}")

    from lib.evidence_binding import sha256_file  # noqa: E402

    model_sha = sha256_file(model)

    port_a, port_b = free_port(), free_port()
    log_a = Path(f"/tmp/llama-two-worker-A-{port_a}.log")
    log_b = Path(f"/tmp/llama-two-worker-B-{port_b}.log")
    base_a, base_b = f"http://127.0.0.1:{port_a}", f"http://127.0.0.1:{port_b}"

    proc_a = proc_b = None
    warm_samples: list[dict] = []
    cold_samples: list[dict] = []
    warm_energy: list[dict] = []
    cold_energy: list[dict] = []
    quality: list[dict] = []
    started_at = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    try:
        proc_a = start_server(model, port_a, args.ctx, log_a)
        proc_b = start_server(model, port_b, args.ctx, log_b)
        wait_http(f"{base_a}/health")
        wait_http(f"{base_b}/health")

        # Establish that BOTH workers expose the signal before measuring, so a
        # missing field is a setup failure rather than a silent zero later.
        probe = build_prefix(args.paragraphs, salt="probe")
        establish = {}
        for name, base in (("worker_a", base_a), ("worker_b", base_b)):
            sig = extract_signals(chat_completion(base, probe, "State one fact.", 8))
            establish[name] = sig
            if not sig.get("cached_tokens_field_present"):
                raise RuntimeError(
                    f"{name} did not report usage.prompt_tokens_details.cached_tokens; "
                    "this measurement needs the engine signal, not Merc's belief"
                )

        for i in range(args.reps):
            # A fresh prefix each round. Without this the cold worker warms
            # itself up after round one and the arms converge.
            prefix = build_prefix(args.paragraphs, salt=f"round{i:03d}")
            question = f"Name paragraph {i + 1} in one word."

            # Warm ONLY worker A on this round's prefix. Worker B never sees it.
            chat_completion(base_a, prefix, "Prime.", 4)

            # Interleave which arm goes first so host drift does not land on one
            # arm; same discipline as the engine parity harness.
            order = ("warm", "cold") if i % 2 == 0 else ("cold", "warm")
            per_round = {}
            for arm in order:
                base = base_a if arm == "warm" else base_b
                res, energy = energy_around(
                    lambda b=base: chat_completion(b, prefix, question, args.max_tokens),
                    interval_ms=args.energy_interval_ms,
                )
                sig = extract_signals(res)
                sig["round"] = i
                sig["order_in_round"] = order.index(arm)
                per_round[arm] = sig
                if arm == "warm":
                    warm_samples.append(sig)
                    warm_energy.append(energy)
                else:
                    cold_samples.append(sig)
                    cold_energy.append(energy)

            quality.append(
                {
                    "round": i,
                    "identical_text": per_round["warm"]["content"]
                    == per_round["cold"]["content"],
                    "warm_text": per_round["warm"]["content"],
                    "cold_text": per_round["cold"]["content"],
                }
            )

        # Restart fallback: the warm worker loses its cache, and the engine must
        # report an observed MISS rather than simply omitting the field. That
        # distinction is the whole contract on the control side.
        restart_prefix = build_prefix(args.paragraphs, salt="restart")
        chat_completion(base_a, restart_prefix, "Prime.", 4)
        before_restart = extract_signals(
            chat_completion(base_a, restart_prefix, "One word.", args.max_tokens)
        )
        stop_server(proc_a)
        proc_a = start_server(model, port_a, args.ctx, log_a)
        wait_http(f"{base_a}/health")
        after_restart = extract_signals(
            chat_completion(base_a, restart_prefix, "One word.", args.max_tokens)
        )
    finally:
        stop_server(proc_a)
        stop_server(proc_b)

    warm = summarize(warm_samples)
    cold = summarize(cold_samples)

    def joules(runs: list[dict]) -> list[float]:
        # The sampler's key is energy_joules; reading a name it does not publish
        # silently produced a null energy delta on the first run of this harness.
        return [
            r["energy_joules"]
            for r in runs
            if r.get("available") and r.get("energy_joules") is not None
        ]

    warm_j, cold_j = joules(warm_energy), joules(cold_energy)
    warm_j_p50 = percentile(warm_j, 50) if warm_j else None
    cold_j_p50 = percentile(cold_j, 50) if cold_j else None
    joules_delta = (
        (warm_j_p50 - cold_j_p50)
        if (warm_j_p50 is not None and cold_j_p50 is not None)
        else None
    )
    energy_usd_delta = (
        joules_delta / 3.6e6 * ELECTRICITY_USD_PER_KWH
        if joules_delta is not None
        else None
    )

    def d(a, b, key, stat="p50"):
        av, bv = a.get(key, {}).get(stat), b.get(key, {}).get(stat)
        return (av - bv) if (av is not None and bv is not None) else None

    all_identical = all(q["identical_text"] for q in quality) if quality else False
    warm_hit = (warm.get("cached_tokens", {}) or {}).get("p50")
    cold_hit = (cold.get("cached_tokens", {}) or {}).get("p50")

    return {
        "schema_version": 1,
        "kind": "prefix_two_worker_routing_measurement",
        "label": (
            "llama.cpp Metal, two workers, one holding the prefix and one not: "
            "cache hit, prefill avoided, TTFT and energy deltas, and the restart miss"
        ),
        "measured_at": started_at,
        "question": (
            "Does routing a request to the worker that already holds its prefix beat "
            "routing it to an equally capable worker that does not?"
        ),
        "host": {"hardware": hardware_label(), "load": host_load()},
        "model": {
            "path": str(model),
            "sha256": model_sha,
            "note": "one file, both workers; warmness is bound to the worker AND this artifact digest",
        },
        "setup": {
            "workers": 2,
            "worker_a_port": port_a,
            "worker_b_port": port_b,
            "rounds": args.reps,
            "paragraphs_per_prefix": args.paragraphs,
            "max_tokens": args.max_tokens,
            "ctx": args.ctx,
            "fresh_prefix_per_round": True,
            "interleaved": "warm/cold order alternates per round",
            "logs": [str(log_a), str(log_b)],
        },
        "signal_establishment": establish,
        "arms": {"warm_worker": warm, "cold_worker": cold},
        "deltas": {
            "definition": "warm minus cold; negative means the warm worker was cheaper/faster",
            "prompt_ms_p50": d(warm, cold, "prompt_ms"),
            "prompt_ms_p95": d(warm, cold, "prompt_ms", "p95"),
            "wall_ms_p50": d(warm, cold, "wall_ms"),
            "wall_ms_p95": d(warm, cold, "wall_ms", "p95"),
            "prefill_tokens_avoided_p50": d(cold, warm, "prompt_n"),
            "cached_tokens_p50_warm": warm_hit,
            "cached_tokens_p50_cold": cold_hit,
            "cold_worker_residual_cache_note": (
                "the cold worker's cached_tokens is small but NOT zero: it has served "
                "other prompts, so the chat template preamble every request shares is "
                "already resident. That residue is the common prefix, not this round's "
                "prefix, and calling the arm 'cold' means 'has never seen THIS prefix' "
                "rather than 'holds an empty cache'. The prefill-avoided figure is the "
                "difference between the arms, so the residue is already netted out."
            ),
            "gpu_joules_p50_warm": warm_j_p50,
            "gpu_joules_p50_cold": cold_j_p50,
            "gpu_joules_delta_p50": joules_delta,
        },
        "verified_outcome_cost_delta": {
            "energy_usd_per_request_delta": energy_usd_delta,
            "electricity_rate_usd_per_kwh": ELECTRICITY_USD_PER_KWH,
            "electricity_source": ELECTRICITY_SOURCE,
            "supplier_entitlement_delta_usd": 0.0,
            "why_supplier_is_zero": (
                "supplier entitlement is units/1000 x price x share and the catalogue is "
                "keyed by (model, job_type), never by worker or duration; a faster serve "
                "of the same units earns the same payout. Energy is therefore the only "
                "verified-outcome cost term that can differ between these two arms."
            ),
            "energy_knowledge": (
                "IOReport AGX GPU-domain joules are MEASURED; the electricity rate is a "
                "policy default, so the dollar figure is DEFAULTED-grade and is not a "
                "metered invoice"
            ),
        },
        "quality": {
            "identical_text_all_rounds": all_identical,
            "contract": "temperature 0, same model artifact, same prompt; warm and cold must agree",
            "rounds": quality,
        },
        "restart_fallback": {
            "before_restart": before_restart,
            "after_restart": after_restart,
            "observed_miss_after_restart": after_restart.get("cached_tokens") == 0,
            "signal_present_after_restart": after_restart.get(
                "cached_tokens_field_present"
            ),
            "why_it_matters": (
                "after a restart the engine reports cached_tokens == 0 -- an observed MISS, "
                "not an absent signal. CorrectPrefixBeliefFromObservation invalidates the "
                "stale warm rows on that observation instead of waiting out the 90s TTL. "
                "An absent field would have to be treated as no observation at all."
            ),
        },
        "selector_relationship": {
            "ranking": "control/prefix_placement.go:RankByCostThenPrefixAffinity",
            "order": [
                "CostRank ASC",
                "AskUSDHr ASC",
                "WarmPrefixDepth DESC",
                "WarmModel DESC",
                "WorkerID ASC",
            ],
            "consequence": (
                "prefix depth is strictly below cost rank and supplier ask, so warmth can "
                "only decide inside a cost class -- it can never move a request onto a more "
                "expensive class to chase a cache hit"
            ),
            "test": "control/prefix_placement_test.go",
        },
        "can_prove": [
            "a worker holding the prefix reports cached_tokens > 0 while an equally capable worker that has not seen it does not",
            "prefill tokens avoided, prompt_ms and wall deltas at p50/p95 between two workers on the same request",
            "GPU-domain joules per arm and the resulting energy dollar delta",
            "warm and cold produce identical text at temperature 0 on the same model artifact",
            "a restart produces an observed MISS (cached_tokens == 0), not an absent signal",
        ],
        "does_not_prove": [
            "a cross-supplier or cross-network advantage: both workers are processes on this host",
            "a supplier-entitlement cost win: duration cancels from the catalogue form, so only energy can differ",
            "fleet behaviour, a second hardware class, or concurrency beyond one in-flight request per worker",
            "that production routing changed: nothing here promotes a cell or alters admission",
            "that Merc's belief equals the engine's cache state at any instant; the engine signal is what corrects the belief",
        ],
        "limitations": [
            "One host, two processes; contention between them is real and is why arms are interleaved.",
            "IOReport measures the AGX GPU domain only -- not package, not wall-plug.",
            "Electricity is a policy default, so the dollar delta is defaulted-grade.",
            "Non-streaming wall time is an upper bound on TTFT; timings.prompt_ms is the engine-authoritative prefill clock.",
        ],
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--model", default=str(DEFAULT_MODEL))
    ap.add_argument("--ctx", type=int, default=4096)
    ap.add_argument("--paragraphs", type=int, default=45)
    ap.add_argument("--reps", type=int, default=12, help="paired rounds")
    ap.add_argument("--max-tokens", type=int, default=16)
    ap.add_argument("--energy-interval-ms", type=int, default=100)
    ap.add_argument(
        "--out",
        default=str(ROOT / "evidence" / "perf" / "prefix-two-worker-latest.json"),
    )
    args = ap.parse_args()

    try:
        art = run(args)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": str(exc)}, indent=2), file=sys.stderr)
        return 2

    summary = json.loads(json.dumps(art))
    for arm in summary.get("arms", {}).values():
        if isinstance(arm, dict):
            arm.pop("raw_samples", None)
    summary.get("quality", {}).pop("rounds", None)
    print(json.dumps(summary, indent=2))

    out_path = Path(args.out)
    if os.environ.get(WRITE_ENV, "") != "1":
        draft = out_path.with_suffix(".draft.json")
        draft.parent.mkdir(parents=True, exist_ok=True)
        draft.write_text(json.dumps(art, indent=2) + "\n")
        print(f"\n# not written (set {WRITE_ENV}=1 to seal)", file=sys.stderr)
        print(f"# draft: {draft}", file=sys.stderr)
        return 0

    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        default_bound_identity,
        sha256_file,
        slot_value,
        write_bound_evidence,
    )

    import shutil

    llama_bin = shutil.which("llama-server") or "/opt/homebrew/bin/llama-server"
    harness_sha = sha256_file(Path(__file__))
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=f"scripts/prefix-two-worker-measure.py@{harness_sha[:16]}",
            build_binary_path=llama_bin,
            exact_config=(
                f"{WRITE_ENV}=1; rounds={args.reps}; paragraphs={args.paragraphs}; "
                f"ctx={args.ctx}; max_tokens={args.max_tokens}; "
                f"energy_interval_ms={args.energy_interval_ms}"
            ),
            raw_samples=(
                f"warm={art['arms']['warm_worker']['n']} "
                f"cold={art['arms']['cold_worker']['n']}"
            ),
            model_na=f"model sha256 in receipt body model.sha256={art['model']['sha256'][:16]}…",
            corpus_na=(
                f"synthetic per-round prefixes paragraphs={args.paragraphs} "
                f"rounds={args.reps}"
            ),
        )
        identity["model_artifact_digest"] = slot_value(art["model"]["sha256"])
    except EvidenceBindingError as exc:
        print(f"identity refused: {exc}", file=sys.stderr)
        return 3

    art["binding_status"] = "BOUND"
    try:
        write_bound_evidence(
            path=out_path,
            payload=art,
            identity=identity,
            repo_root=ROOT,
            build_binary_path=llama_bin,
        )
    except EvidenceBindingError as exc:
        print(f"write refused: {exc}", file=sys.stderr)
        return 4

    dated = out_path.with_name(
        f"prefix-two-worker-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    )
    try:
        dated.write_text(out_path.read_text())
    except Exception:  # noqa: BLE001
        pass
    print(f"\n# BOUND: {out_path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
