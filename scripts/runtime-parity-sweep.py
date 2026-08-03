#!/usr/bin/env python3
"""Measure one runtime across a concurrency sweep, honestly labelled.

merc's structural claim is heterogeneous routing: send a request to whichever
runtime is actually better for its shape. That claim needs numbers, and the
numbers only mean something if the comparison is labelled precisely enough that
nobody can quote them as a headline.

So this deliberately does NOT print a winner. It records one runtime's behaviour
across concurrency, with the full configuration attached, and leaves the
comparison to a caller who can state what differed. Metal/llama.cpp/GGUF-Q4 and
CUDA/vLLM/FP16 are different weights at different precision; a single "X is
faster" number across them would be dishonest.

What the sweep is for: batch-1 latency and batch-N throughput usually favour
different hardware. That crossover is the routing decision, and it is the thing
a homogeneous fleet cannot exploit.

  python3 scripts/runtime-parity-sweep.py \
      --endpoint https://<pod>-8000.proxy.runpod.net/v1 --model merc-vllm \
      --label "CUDA A5000 / vLLM / Qwen2.5-1.5B fp16" \
      --concurrency 1,4,16 --out evidence/perf/cuda-a5000.json
"""

import argparse
import json
import os
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request

# RunPod's edge blocks default library User-Agents (Python-urllib gets 403 with a
# valid key while curl gets 200), so every request here carries an explicit one.
MERC_UA = "merc-bench/1.0"

PROMPT = ("Write a short factual paragraph about the water cycle. "
          "Be specific and do not repeat yourself.")


def one_request(endpoint, key, model, max_tokens, timeout=180):
    """Returns (ttft_ms, total_s, completion_tokens) or raises."""
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "temperature": 0,
        "stream": True,
        "stream_options": {"include_usage": True},
    }).encode()
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/chat/completions", data=body, method="POST",
        headers={"content-type": "application/json",
                 "authorization": f"Bearer {key}", "User-Agent": MERC_UA})

    start = time.perf_counter()
    ttft = None
    completion_tokens = 0
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if not line.startswith("data: "):
                continue
            payload = line[6:]
            if payload == "[DONE]":
                break
            try:
                chunk = json.loads(payload)
            except json.JSONDecodeError:
                continue
            # First token out is what an interactive user actually waits for.
            if ttft is None:
                choices = chunk.get("choices") or []
                if choices and (choices[0].get("delta") or {}).get("content"):
                    ttft = (time.perf_counter() - start) * 1000
            usage = chunk.get("usage")
            if usage and usage.get("completion_tokens"):
                completion_tokens = usage["completion_tokens"]
    total = time.perf_counter() - start
    return ttft, total, completion_tokens


def sweep(endpoint, key, model, concurrency, requests_per_level, max_tokens):
    results = {}
    for level in concurrency:
        latencies, ttfts, tokens, errors = [], [], [], []
        lock = threading.Lock()

        def worker():
            try:
                ttft, total, ntok = one_request(endpoint, key, model, max_tokens)
            except Exception as exc:  # noqa: BLE001 - recorded, not swallowed
                with lock:
                    errors.append(f"{type(exc).__name__}: {exc}")
                return
            with lock:
                latencies.append(total)
                tokens.append(ntok)
                if ttft is not None:
                    ttfts.append(ttft)

        # Warm the engine so the first level does not pay one-off costs that the
        # later levels avoid, which would fake a throughput curve.
        try:
            one_request(endpoint, key, model, 8)
        except Exception:
            pass

        wall_start = time.perf_counter()
        threads = []
        for _ in range(level * requests_per_level):
            t = threading.Thread(target=worker)
            threads.append(t)
        # Keep `level` in flight at a time.
        running = []
        for t in threads:
            t.start()
            running.append(t)
            if len(running) >= level:
                running.pop(0).join()
        for t in running:
            t.join()
        wall = time.perf_counter() - wall_start

        ok = len(latencies)
        total_tokens = sum(tokens)
        results[str(level)] = {
            "requests": ok,
            "errors": len(errors),
            "error_samples": errors[:3],
            "wall_seconds": round(wall, 3),
            # Aggregate throughput is the number that matters at high
            # concurrency; per-request rate is the number that matters at 1.
            "aggregate_tokens_per_sec": round(total_tokens / wall, 2) if wall > 0 else None,
            "ttft_ms_p50": round(statistics.median(ttfts), 1) if ttfts else None,
            "ttft_ms_p95": round(sorted(ttfts)[min(len(ttfts) - 1, int(len(ttfts) * 0.95))], 1) if ttfts else None,
            "latency_s_p50": round(statistics.median(latencies), 3) if latencies else None,
            "latency_s_p95": round(sorted(latencies)[min(len(latencies) - 1, int(len(latencies) * 0.95))], 3) if latencies else None,
            "completion_tokens_total": total_tokens,
        }
        print(f"  concurrency {level:>3}: "
              f"{results[str(level)]['aggregate_tokens_per_sec']} tok/s aggregate, "
              f"TTFT p50 {results[str(level)]['ttft_ms_p50']}ms, "
              f"{ok} ok / {len(errors)} err")
    return results


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", required=True)
    ap.add_argument("--model", required=True)
    ap.add_argument("--label", required=True,
                    help="exact runtime/hardware/model/precision, for the receipt")
    ap.add_argument("--concurrency", default="1,4,16")
    ap.add_argument("--requests-per-level", type=int, default=3)
    ap.add_argument("--max-tokens", type=int, default=128)
    ap.add_argument("--cost-per-hour-usd", type=float, default=0.0)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    key = os.environ.get("MERC_BENCH_API_KEY", "")
    if not key:
        print("MERC_BENCH_API_KEY is required", file=sys.stderr)
        return 2

    levels = [int(x) for x in args.concurrency.split(",") if x.strip()]
    print(f"sweeping {args.label}")
    results = sweep(args.endpoint, key, args.model, levels,
                    args.requests_per_level, args.max_tokens)

    peak = max((r["aggregate_tokens_per_sec"] or 0) for r in results.values())
    report = {
        "schema_version": 1,
        "kind": "runtime_concurrency_sweep",
        "label": args.label,
        "model": args.model,
        "max_tokens": args.max_tokens,
        "cost_per_hour_usd": args.cost_per_hour_usd,
        "levels": results,
        "peak_aggregate_tokens_per_sec": peak,
        # The number that decides whether a lane is worth running at all.
        "usd_per_million_tokens_at_peak": (
            round(args.cost_per_hour_usd / (peak * 3600) * 1_000_000, 4)
            if peak > 0 and args.cost_per_hour_usd > 0 else None),
        "comparability_warning": (
            "Valid only against another sweep with the SAME model, precision and "
            "max_tokens. Metal/GGUF-Q4 and CUDA/fp16 are different weights at "
            "different precision; quoting one number across them is not a "
            "measurement, it is a slogan."),
    }
    from pathlib import Path as _Path
    import sys as _sys
    _scripts = str(_Path(__file__).resolve().parent)
    if _scripts not in _sys.path:
        _sys.path.insert(0, _scripts)
    from lib.evidence_binding import EvidenceBindingError, emit_bound_json
    try:
        emit_bound_json(
            args.out,
            report,
            harness="scripts/runtime-parity-sweep.py",
            build_binary_path=str(_Path(__file__).resolve()),
            exact_config="embedded concurrency levels + model/precision",
            raw_samples="embedded levels results",
        )
    except EvidenceBindingError as exc:
        print(f"REFUSED evidence write: {exc}", file=sys.stderr)
        return 2
    print(f"\npeak {peak} tok/s aggregate"
          + (f", ${report['usd_per_million_tokens_at_peak']}/M tokens"
             if report["usd_per_million_tokens_at_peak"] else ""))
    print(f"wrote {args.out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
