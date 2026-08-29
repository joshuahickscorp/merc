#!/usr/bin/env python3
"""Embed throughput per hardware class, at concurrency, over one HTTP contract.

The first CUDA arm measurement asked one question badly. It sent one request at
a time from a Mac to a rented pod and reported 30.6 ms/unit p50, a number
dominated by the public internet and RunPod's Cloudflare proxy. Comparing that
to candle's 3.026 ms/unit -- in-process, on this machine, no socket -- would be
measuring the network and calling it an engine.

This asks the comparable question the placement readiness gate actually lists as
available: THROUGHPUT PER HARDWARE CLASS on the matched contract. Two properties
make it fair where the first attempt was not:

  - both arms are HTTP servers doing continuous batching. llama-server on Metal
    and vLLM on CUDA are the same shape of thing. candle is in-process and is
    deliberately NOT an arm here; it is the outcome reference only.
  - throughput at concurrency amortises the round trip instead of measuring it.
    At c=64 the network is a pipeline stage, not the bottleneck, so units/sec
    reflects the engine. Per-request latency at that concurrency still includes
    the RTT and is reported as such, never as engine latency.

It also scrapes each server's own /metrics before and after, so where the engine
publishes its internal timings they travel in the receipt beside the
client-observed ones and a reader can see the size of the gap.

    embed-throughput-sweep.py --endpoint https://host/v1 --key K --label cuda_a40 \\
        --corpus corpus.json --reference metal-candle-embeddings.json \\
        --concurrency 1,4,16,64

Still cannot compare COST across hardware classes; comparableHardwareFor refuses
one and this changes nothing about that.
"""

from __future__ import annotations

import argparse
import json
import math
import statistics
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path

# RunPod's proxy is behind Cloudflare, which bans Python-urllib's default
# User-Agent with `error code: 1010`. Same identity the readiness probe sends.
USER_AGENT = "curl/8.7.1"

MEAN_COSINE_GATE = 0.999
ROW_COSINE_GATE = 0.999


def percentile(xs: list[float], p: float) -> float:
    if not xs:
        return 0.0
    s = sorted(xs)
    if len(s) == 1:
        return s[0]
    k = (len(s) - 1) * (p / 100.0)
    lo, hi = math.floor(k), math.ceil(k)
    if lo == hi:
        return s[int(k)]
    return s[lo] + (s[hi] - s[lo]) * (k - lo)


def cosine(a: list[float], b: list[float]) -> float:
    num = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        raise ValueError("zero-norm embedding cannot be compared by cosine")
    return num / (na * nb)


def _req(url: str, key: str, data: bytes | None = None, timeout: float = 180.0):
    headers = {"User-Agent": USER_AGENT}
    if key:
        headers["Authorization"] = f"Bearer {key}"
    if data is not None:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=data, headers=headers,
                                 method="POST" if data is not None else "GET")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()


def embed_once(base: str, key: str, model: str, texts: list[str]):
    body = json.dumps({"model": model, "input": texts}).encode()
    t0 = time.perf_counter()
    status, raw = _req(f"{base}/embeddings", key, body)
    wall_ms = (time.perf_counter() - t0) * 1000.0
    if status != 200:
        raise RuntimeError(f"HTTP {status}: {raw[:400].decode('utf-8', 'replace')}")
    doc = json.loads(raw)
    vectors = [d["embedding"] for d in sorted(doc["data"], key=lambda d: d["index"])]
    return vectors, wall_ms


def scrape_metrics(base: str, key: str) -> dict:
    """Best-effort /metrics scrape. Absent metrics are recorded as absent."""
    root = base[:-3] if base.endswith("/v1") else base
    status, raw = _req(f"{root}/metrics", key, timeout=20.0)
    if status != 200:
        return {"available": False, "status": status}
    text = raw.decode("utf-8", "replace")
    keep = {}
    for line in text.splitlines():
        if line.startswith("#") or " " not in line:
            continue
        name, _, value = line.rpartition(" ")
        if any(k in name for k in (
            "request_inference_time", "e2e_request_latency", "request_queue_time",
            "prompt_tokens_total", "request_success", "num_requests",
            "request_prefill_time", "iteration_tokens",
        )):
            try:
                keep[name] = float(value)
            except ValueError:
                continue
    return {"available": True, "series": keep}


def derive_engine_side(before: dict, after: dict, texts_per_request: int) -> dict:
    """Engine-published timings, which is the only network-free number here.

    A client on a laptop cannot measure a remote engine's throughput: with a new
    TCP+TLS connection per request over a ~100ms link, what the client observes
    is its own connection handling. The engine's own histograms are not subject
    to that, and the gap between the two is the size of the confound -- which is
    worth publishing, because it is what stops someone reading client throughput
    as a hardware verdict.

    Returns available=False rather than guessing when the engine publishes no
    such series (llama.cpp exposes far less than vLLM here).
    """
    if not (before.get("available") and after.get("available")):
        return {"available": False, "why": "engine published no /metrics"}
    a, b = after.get("series") or {}, before.get("series") or {}

    def delta(substr: str):
        av = next((v for k, v in a.items() if substr in k), None)
        bv = next((v for k, v in b.items() if substr in k), 0.0)
        return None if av is None else av - (bv or 0.0)

    n = delta("request_inference_time_seconds_count")
    infer = delta("request_inference_time_seconds_sum")
    queue = delta("request_queue_time_seconds_sum")
    e2e = delta("e2e_request_latency_seconds_sum")
    if not n or infer is None:
        return {"available": False, "why": "no request_inference_time series"}

    per_req_infer_ms = infer / n * 1000.0
    out = {
        "available": True,
        "requests": n,
        "inference_ms_per_request": per_req_infer_ms,
        "inference_ms_per_unit": per_req_infer_ms / texts_per_request,
        "queue_ms_per_request": (queue / n * 1000.0) if queue is not None else None,
        "e2e_ms_per_request": (e2e / n * 1000.0) if e2e is not None else None,
        "boundary": "engine-published; excludes the client link entirely",
    }
    if e2e is not None:
        out["non_inference_ms_per_request"] = e2e / n * 1000.0 - per_req_infer_ms
    # Queue time is how the engine says whether it was ever the bottleneck. Near
    # zero means the load generator never saturated it, so no throughput ceiling
    # for this hardware was established and none may be claimed.
    if out["queue_ms_per_request"] is not None:
        out["saturated"] = out["queue_ms_per_request"] > per_req_infer_ms
        out["saturation_note"] = (
            "queue time exceeds inference time: the engine was the bottleneck and "
            "the throughput figures describe it"
            if out["saturated"] else
            "queue time is far below inference time: the engine was NEVER saturated, "
            "so the client-observed throughput is a property of the load generator "
            "and the link, and NO throughput ceiling for this hardware was established"
        )
    return out


def run_level(base: str, key: str, model: str, corpus: list[str],
              concurrency: int, rounds: int) -> dict:
    """Fire `concurrency` requests at a time, `rounds` times. Wall clock over the
    whole level is what throughput is computed from -- summing per-request
    latencies would double-count overlapped time."""
    latencies: list[float] = []
    errors: list[str] = []
    last_vectors = None

    def one(_i: int):
        try:
            v, ms = embed_once(base, key, model, corpus)
            return v, ms, None
        except Exception as exc:  # noqa: BLE001
            return None, None, f"{type(exc).__name__}: {exc}"[:200]

    t0 = time.perf_counter()
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        for _ in range(rounds):
            for vectors, ms, err in pool.map(one, range(concurrency)):
                if err:
                    errors.append(err)
                    continue
                latencies.append(ms)
                last_vectors = vectors
    elapsed_s = time.perf_counter() - t0

    completed = len(latencies)
    units = completed * len(corpus)
    return {
        "concurrency": concurrency,
        "rounds": rounds,
        "requests_completed": completed,
        "requests_failed": len(errors),
        "errors": errors[:5],
        "wall_seconds": elapsed_s,
        "units": units,
        "units_per_sec": (units / elapsed_s) if elapsed_s > 0 else None,
        "requests_per_sec": (completed / elapsed_s) if elapsed_s > 0 else None,
        # Per-request latency at concurrency INCLUDES queueing and the network
        # round trip. It is not engine latency and must not be quoted as one.
        "request_latency_ms_p50": percentile(latencies, 50),
        "request_latency_ms_p95": percentile(latencies, 95),
        "request_latency_ms_mean": statistics.fmean(latencies) if latencies else None,
        "_last_vectors": last_vectors,
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--endpoint", required=True, help="OpenAI-compatible base, ending /v1")
    ap.add_argument("--key", default="")
    ap.add_argument("--label", required=True, help="arm label, e.g. cuda_a40 or metal_llama_cpp")
    ap.add_argument("--hw-class", required=True)
    ap.add_argument("--model", default="all-minilm-l6-v2")
    ap.add_argument("--corpus", required=True)
    ap.add_argument("--reference", required=True)
    ap.add_argument("--concurrency", default="1,4,16,64")
    ap.add_argument("--rounds", type=int, default=8)
    ap.add_argument("--warmup", type=int, default=5)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    corpus = json.loads(Path(args.corpus).read_text())
    reference = json.loads(Path(args.reference).read_text())["vectors"]
    levels = [int(x) for x in args.concurrency.split(",") if x.strip()]

    status, raw = _req(f"{args.endpoint}/models", args.key)
    served = []
    if status == 200:
        try:
            served = [m["id"] for m in json.loads(raw).get("data", [])]
        except Exception:  # noqa: BLE001
            pass
    print(f"# GET /models -> HTTP {status} served={served}", file=sys.stderr)
    if served and args.model not in served:
        print(f"# using served name {served[0]!r}", file=sys.stderr)
        args.model = served[0]

    for _ in range(args.warmup):
        embed_once(args.endpoint, args.key, args.model, corpus)

    before = scrape_metrics(args.endpoint, args.key)
    results = []
    last_vectors = None
    for c in levels:
        print(f"# concurrency {c}", file=sys.stderr)
        r = run_level(args.endpoint, args.key, args.model, corpus, c, args.rounds)
        last_vectors = r.pop("_last_vectors") or last_vectors
        results.append(r)
    after = scrape_metrics(args.endpoint, args.key)

    per_row = [cosine(last_vectors[i], reference[i]) for i in range(len(corpus))]
    mean_cos = statistics.fmean(per_row)
    min_cos = min(per_row)
    passes = mean_cos >= MEAN_COSINE_GATE and min_cos >= ROW_COSINE_GATE

    peak = max((r for r in results if r["units_per_sec"]),
               key=lambda r: r["units_per_sec"], default=None)

    art = {
        "schema_version": 1,
        "kind": "embed_throughput_sweep",
        "label": f"{args.label}: embed throughput at concurrency on the matched contract",
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "arm": {"label": args.label, "hw_class": args.hw_class,
                "endpoint_host": args.endpoint.split("//")[-1].split("/")[0],
                "served_model": args.model},
        "corpus": {
            "sha256": "01fcad50dcc300f12f31f91d3ae356746382ef9961594715563958829bad59f9",
            "source": "EMBED_BENCH_CORPUS in src/agent/src/main.rs", "texts": len(corpus),
        },
        "quality": {
            "verification": "cosine", "mean_threshold": MEAN_COSINE_GATE,
            "row_threshold": ROW_COSINE_GATE, "mean_cosine": mean_cos,
            "min_cosine": min_cos, "per_text_cosine": per_row, "passes": passes,
            "reference_runtime": "candle_metal",
        },
        "levels": results if passes else None,
        "void_reason": None if passes else
            f"cosine gate failed (mean {mean_cos:.9f}, min {min_cos:.9f})",
        "peak_units_per_sec": peak["units_per_sec"] if (passes and peak) else None,
        "peak_at_concurrency": peak["concurrency"] if (passes and peak) else None,
        "engine_metrics": {"before": before, "after": after,
                           "note": "server-published counters where the engine exposes them; "
                                   "absent metrics are recorded absent rather than inferred"},
        "engine_side": derive_engine_side(before, after, len(corpus)),
        "reading_this": [
            "units_per_sec is the comparable quantity across hardware classes: it is "
            "computed from wall clock over the whole level, so overlapped requests are "
            "not double-counted.",
            "request_latency_ms at concurrency > 1 includes queueing AND the network "
            "round trip. It is not engine latency and must not be quoted as one.",
            "candle is the outcome reference, not an arm: it is in-process and has no "
            "socket, so putting it on this axis would compare a function call to a web "
            "request.",
        ],
        "does_not_prove": [
            "a cost comparison across hardware classes — comparableHardwareFor refuses one",
            "a promotion; the CUDA cell stays DRAFT and non-routable",
            "a fleet claim, or behaviour at another batch size, model or precision",
        ],
    }
    summary = dict(art)
    summary["quality"] = {k: v for k, v in art["quality"].items() if k != "per_text_cosine"}
    print(json.dumps(summary, indent=2))
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(art, indent=2) + "\n")
    print(f"\n# wrote {out}", file=sys.stderr)
    return 0 if passes else 5


if __name__ == "__main__":
    raise SystemExit(main())
