#!/usr/bin/env python3
"""Measure merc gateway overhead against the same engine spoken to directly.

Question answered with numbers, not slogans:

    client -> engine directly            baseline
    client -> merc -> the same engine    measured

For identical traffic, report the delta on TTFT p50/p95/p99, inter-token
latency, total wall time, aggregate throughput at concurrency 1 / 8 / 32, and
bytes transferred. Means alone are not an answer; the tail is where a gateway
hurts.

Shape follows scripts/runtime-parity-sweep.py: concurrency sweep, full config
recorded in the receipt, and a hard refusal to print a comparison winner when
the two sides are not comparable (different model, precision, or max_tokens).

  python3 scripts/gateway-parity.py \\
      --merc-base-url http://127.0.0.1:8080/v1 \\
      --direct-base-url http://127.0.0.1:8095/v1 \\
      --model cx-chat-1b \\
      --out evidence/perf/gateway-parity.json

Required env:
  MERC_BENCHMARK_API_KEY     buyer key for merc
  MERC_DIRECT_VLLM_API_KEY   engine key for the direct path
"""

from __future__ import annotations

import argparse
import json
import os
import re
import statistics
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from datetime import datetime, timezone
from typing import Any

MERC_UA = "merc-gateway-parity/1.0"

# Same prompt as runtime-parity-sweep so a reader can put the two receipts
# next to each other without wondering whether the workload changed.
PROMPT = (
    "Write a short factual paragraph about the water cycle. "
    "Be specific and do not repeat yourself."
)

# Must match control/runtime_cell_cost.go:minCellCostSamples. Twenty samples is
# the promotion gate's floor; p95/p99 under nearest-rank need that floor too.
# Do not invent a parallel constant — drift would re-admit under-sampled receipts.
MIN_CELL_COST_SAMPLES = 20

GATE_VERSION = "gateway-parity-gate-v1"

# Policy ceiling, not a derived SLA. Calibrated to the 2026-07-28 measurement
# (+3.5 ms TTFT p50, +9.6 ms TTFT p95, −1.86% aggregate throughput at c=1) with
# modest headroom so the gate can fail a real regression without failing on
# ordinary thermal noise. See evaluate_budget / the receipt's budget.basis.
DEFAULT_BUDGET_TTFT_OVERHEAD_P95_MS = 15.0
DEFAULT_BUDGET_THROUGHPUT_LOSS_FRACTION = 0.05

SHA256_RE = re.compile(r"^[0-9a-f]{64}$")

# Features that are intentionally off for an overhead measurement. Exact reuse
# and coalescing only fire on non-streaming completions (control/realtime.go);
# a hit would replace the live engine path and the "overhead" would be a cache
# hit. Recorded in the receipt so a reader never has to infer this.
FEATURES_DISABLED = [
    "exact_reuse",
    "request_coalescing",
]


def percentile(xs: list[float], p: float) -> float | None:
    if not xs:
        return None
    ordered = sorted(xs)
    if len(ordered) == 1:
        return ordered[0]
    # Nearest-rank, matching runtime-parity-sweep.py.
    idx = min(len(ordered) - 1, int(len(ordered) * p))
    return ordered[idx]


def stats_ms(xs: list[float]) -> dict[str, Any]:
    """xs are already in milliseconds."""
    if not xs:
        return {"samples": 0}
    return {
        "samples": len(xs),
        "mean_ms": round(statistics.fmean(xs), 3),
        "p50_ms": round(percentile(xs, 0.50), 3),  # type: ignore[arg-type]
        "p95_ms": round(percentile(xs, 0.95), 3),  # type: ignore[arg-type]
        "p99_ms": round(percentile(xs, 0.99), 3),  # type: ignore[arg-type]
        "min_ms": round(min(xs), 3),
        "max_ms": round(max(xs), 3),
    }


def stats_s(xs: list[float]) -> dict[str, Any]:
    if not xs:
        return {"samples": 0}
    return {
        "samples": len(xs),
        "mean_s": round(statistics.fmean(xs), 4),
        "p50_s": round(percentile(xs, 0.50), 4),  # type: ignore[arg-type]
        "p95_s": round(percentile(xs, 0.95), 4),  # type: ignore[arg-type]
        "p99_s": round(percentile(xs, 0.99), 4),  # type: ignore[arg-type]
        "min_s": round(min(xs), 4),
        "max_s": round(max(xs), 4),
    }


def probe_models(endpoint: str, key: str, timeout: float = 10.0) -> dict[str, Any]:
    """Fetch /models so the receipt can pin what each side actually served."""
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/models",
        headers={
            "authorization": f"Bearer {key}",
            "User-Agent": MERC_UA,
        },
        method="GET",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            headers = {k.lower(): v for k, v in resp.headers.items()}
            try:
                payload = json.loads(body.decode("utf-8", "replace"))
            except json.JSONDecodeError:
                payload = None
            ids: list[str] = []
            if isinstance(payload, dict):
                for item in payload.get("data") or []:
                    if isinstance(item, dict) and item.get("id"):
                        ids.append(str(item["id"]))
            return {
                "reachable": True,
                "http_status": resp.status,
                "model_ids": ids,
                "bytes": len(body),
                "runtime_attestation": headers.get("x-merc-runtime-attestation", ""),
                "server": headers.get("server", ""),
            }
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, TimeoutError) as exc:
        return {
            "reachable": False,
            "error": f"{type(exc).__name__}: {exc}",
            "model_ids": [],
        }


def one_stream(
    endpoint: str,
    key: str,
    model: str,
    max_tokens: int,
    timeout: float = 180.0,
) -> dict[str, Any]:
    """One streaming chat completion.

    Returns TTFT, inter-token latencies, wall time, token counts, and byte
    counts. Raises on transport / HTTP failure.
    """
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "temperature": 0,
        "stream": True,
        "stream_options": {"include_usage": True},
    }).encode()
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/chat/completions",
        data=body,
        method="POST",
        headers={
            "content-type": "application/json",
            "authorization": f"Bearer {key}",
            "User-Agent": MERC_UA,
        },
    )

    start = time.perf_counter()
    ttft_ms: float | None = None
    token_times: list[float] = []  # wall times of content-bearing chunks
    request_bytes = len(body)
    response_bytes = 0
    completion_tokens = 0
    prompt_tokens = 0
    content_chars = 0
    chunk_count = 0
    content_chunks = 0
    merc_headers: dict[str, str] = {}

    with urllib.request.urlopen(req, timeout=timeout) as resp:
        for name in ("X-Merc-Contract-ID", "X-Merc-Receipt", "X-Merc-Max-USD",
                     "X-Merc-Exact-Reuse", "X-Merc-Path-Timing"):
            if resp.headers.get(name):
                merc_headers[name] = resp.headers.get(name)
        for raw in resp:
            response_bytes += len(raw)
            now = time.perf_counter()
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
            chunk_count += 1
            choices = chunk.get("choices") or []
            delta = ""
            if choices:
                delta = (choices[0].get("delta") or {}).get("content") or ""
            if delta:
                content_chunks += 1
                content_chars += len(delta)
                token_times.append(now)
                if ttft_ms is None:
                    ttft_ms = (now - start) * 1000.0
            usage = chunk.get("usage")
            if usage:
                if usage.get("completion_tokens") is not None:
                    completion_tokens = int(usage["completion_tokens"])
                if usage.get("prompt_tokens") is not None:
                    prompt_tokens = int(usage["prompt_tokens"])

    total_s = time.perf_counter() - start

    # Inter-token latency: gap between successive content-bearing chunks.
    # Engines may emit multi-token deltas; this is still the client-visible
    # cadence and the right thing to compare gateway-vs-direct on.
    itl_ms: list[float] = []
    for i in range(1, len(token_times)):
        itl_ms.append((token_times[i] - token_times[i - 1]) * 1000.0)

    return {
        "ttft_ms": ttft_ms,
        "total_s": total_s,
        "itl_ms": itl_ms,
        "completion_tokens": completion_tokens,
        "prompt_tokens": prompt_tokens,
        "content_chars": content_chars,
        "content_chunks": content_chunks,
        "chunk_count": chunk_count,
        "request_bytes": request_bytes,
        "response_bytes": response_bytes,
        "merc_headers": merc_headers,
    }


def run_level(
    endpoint: str,
    key: str,
    model: str,
    max_tokens: int,
    concurrency: int,
    requests: int,
) -> dict[str, Any]:
    """Drive `requests` completions keeping `concurrency` in flight."""
    latencies: list[float] = []
    ttfts: list[float] = []
    itls: list[float] = []
    tokens: list[int] = []
    req_bytes = 0
    resp_bytes = 0
    errors: list[str] = []
    lock = threading.Lock()

    def worker() -> None:
        try:
            sample = one_stream(endpoint, key, model, max_tokens)
        except Exception as exc:  # noqa: BLE001 - recorded, not swallowed
            with lock:
                errors.append(f"{type(exc).__name__}: {exc}")
            return
        with lock:
            latencies.append(sample["total_s"])
            if sample["ttft_ms"] is not None:
                ttfts.append(sample["ttft_ms"])
            itls.extend(sample["itl_ms"])
            tokens.append(int(sample["completion_tokens"] or 0))
            nonlocal_req = sample["request_bytes"]
            nonlocal_resp = sample["response_bytes"]
            # rebind via list mutation below
            totals["req"] += nonlocal_req
            totals["resp"] += nonlocal_resp

    totals = {"req": 0, "resp": 0}

    # Warm the path so cold-start costs do not pollute the first level.
    try:
        one_stream(endpoint, key, model, min(8, max_tokens))
    except Exception:
        pass

    wall_start = time.perf_counter()
    threads: list[threading.Thread] = []
    for _ in range(requests):
        t = threading.Thread(target=worker)
        threads.append(t)
    running: list[threading.Thread] = []
    for t in threads:
        t.start()
        running.append(t)
        if len(running) >= concurrency:
            running.pop(0).join()
    for t in running:
        t.join()
    wall = time.perf_counter() - wall_start

    ok = len(latencies)
    total_tokens = sum(tokens)
    return {
        "concurrency": concurrency,
        "requests_attempted": requests,
        "requests_ok": ok,
        "errors": len(errors),
        "error_samples": errors[:5],
        "wall_seconds": round(wall, 3),
        "aggregate_tokens_per_sec": round(total_tokens / wall, 2) if wall > 0 else None,
        "aggregate_requests_per_sec": round(ok / wall, 3) if wall > 0 else None,
        "ttft": stats_ms(ttfts),
        "inter_token_latency": stats_ms(itls),
        "total_wall": stats_s(latencies),
        "completion_tokens_total": total_tokens,
        "bytes": {
            "request_total": totals["req"],
            "response_total": totals["resp"],
            "request_mean": round(totals["req"] / ok, 1) if ok else None,
            "response_mean": round(totals["resp"] / ok, 1) if ok else None,
        },
    }


def delta_ms(merc: dict[str, Any] | None, direct: dict[str, Any] | None, key: str) -> float | None:
    if not merc or not direct:
        return None
    if merc.get(key) is None or direct.get(key) is None:
        return None
    return round(float(merc[key]) - float(direct[key]), 3)


def delta_field(merc: dict[str, Any] | None, direct: dict[str, Any] | None, key: str, digits: int = 4) -> float | None:
    if not merc or not direct:
        return None
    if merc.get(key) is None or direct.get(key) is None:
        return None
    return round(float(merc[key]) - float(direct[key]), digits)


def compare_levels(merc: dict[str, Any], direct: dict[str, Any]) -> dict[str, Any]:
    return {
        "ttft_delta_ms": {
            "p50": delta_ms(merc.get("ttft"), direct.get("ttft"), "p50_ms"),
            "p95": delta_ms(merc.get("ttft"), direct.get("ttft"), "p95_ms"),
            "p99": delta_ms(merc.get("ttft"), direct.get("ttft"), "p99_ms"),
            "mean": delta_ms(merc.get("ttft"), direct.get("ttft"), "mean_ms"),
        },
        "inter_token_latency_delta_ms": {
            "p50": delta_ms(merc.get("inter_token_latency"), direct.get("inter_token_latency"), "p50_ms"),
            "p95": delta_ms(merc.get("inter_token_latency"), direct.get("inter_token_latency"), "p95_ms"),
            "p99": delta_ms(merc.get("inter_token_latency"), direct.get("inter_token_latency"), "p99_ms"),
            "mean": delta_ms(merc.get("inter_token_latency"), direct.get("inter_token_latency"), "mean_ms"),
        },
        "total_wall_delta_s": {
            "p50_s": delta_field(merc.get("total_wall"), direct.get("total_wall"), "p50_s"),
            "p95_s": delta_field(merc.get("total_wall"), direct.get("total_wall"), "p95_s"),
            "p99_s": delta_field(merc.get("total_wall"), direct.get("total_wall"), "p99_s"),
            "mean_s": delta_field(merc.get("total_wall"), direct.get("total_wall"), "mean_s"),
        },
        "aggregate_tokens_per_sec": {
            "merc": merc.get("aggregate_tokens_per_sec"),
            "direct": direct.get("aggregate_tokens_per_sec"),
            "delta": (
                round(merc["aggregate_tokens_per_sec"] - direct["aggregate_tokens_per_sec"], 2)
                if merc.get("aggregate_tokens_per_sec") is not None
                and direct.get("aggregate_tokens_per_sec") is not None
                else None
            ),
        },
        "bytes": {
            "merc_response_total": (merc.get("bytes") or {}).get("response_total"),
            "direct_response_total": (direct.get("bytes") or {}).get("response_total"),
            "response_delta": (
                (merc.get("bytes") or {}).get("response_total", 0)
                - (direct.get("bytes") or {}).get("response_total", 0)
            ),
        },
        "errors": {
            "merc": merc.get("errors"),
            "direct": direct.get("errors"),
        },
    }


def smoke_accepts_model(endpoint: str, key: str, model: str, timeout: float = 60.0) -> dict[str, Any]:
    """One-token streaming probe: proves the side will serve `model`.

    merc's /models catalogue can list batch catalogue ids while the realtime
    surface accepts a runtime-profile alias (e.g. cx-chat-1b). Listing is not
    authority for comparability; a successful completion with the shared model
    and max_tokens is.
    """
    try:
        sample = one_stream(endpoint, key, model, max_tokens=1, timeout=timeout)
        return {
            "accepted": True,
            "ttft_ms": sample.get("ttft_ms"),
            "completion_tokens": sample.get("completion_tokens"),
        }
    except Exception as exc:  # noqa: BLE001
        return {"accepted": False, "error": f"{type(exc).__name__}: {exc}"}


def normalize_artifact_sha256(value: str | None, side: str) -> tuple[str, list[str]]:
    """Return (digest, reasons). Empty digest with reasons means unusable."""
    reasons: list[str] = []
    digest = (value or "").strip().lower()
    if not digest:
        reasons.append(
            f"{side} model artifact sha256 is required so same-weights is proved, "
            "not inferred from a smoke completion or /models listing"
        )
        return "", reasons
    if not SHA256_RE.match(digest):
        reasons.append(
            f"{side} model artifact sha256 must be 64 lowercase hex chars, got {value!r}"
        )
        return "", reasons
    return digest, reasons


def assert_comparable(
    model: str,
    max_tokens: int,
    merc_probe: dict[str, Any],
    direct_probe: dict[str, Any],
    merc_smoke: dict[str, Any],
    direct_smoke: dict[str, Any],
    merc_label: str,
    direct_label: str,
    merc_artifact_sha256: str = "",
    direct_artifact_sha256: str = "",
) -> list[str]:
    """Return reasons the two sides must not be compared. Empty = ok."""
    reasons: list[str] = []
    if not merc_probe.get("reachable"):
        reasons.append(f"merc unreachable: {merc_probe.get('error')}")
    if not direct_probe.get("reachable"):
        reasons.append(f"direct unreachable: {direct_probe.get('error')}")
    if max_tokens < 1:
        reasons.append("max_tokens must be >= 1")
    if not merc_label or not direct_label:
        reasons.append(
            "both --merc-label and --direct-label are required so the receipt "
            "pins precision/hardware (refuse to compare unlabeled runs)"
        )
    if not merc_smoke.get("accepted"):
        reasons.append(f"merc refused model {model!r}: {merc_smoke.get('error')}")
    if not direct_smoke.get("accepted"):
        reasons.append(f"direct refused model {model!r}: {direct_smoke.get('error')}")
    # Direct /models is still useful when the engine advertises ids: if the
    # list is non-empty and clearly missing the model, that is a mismatch.
    direct_ids = set(direct_probe.get("model_ids") or [])
    if direct_ids and model not in direct_ids:
        reasons.append(
            f"model {model!r} not advertised by direct engine /models ({sorted(direct_ids)})"
        )
    merc_digest, merc_reasons = normalize_artifact_sha256(merc_artifact_sha256, "merc")
    direct_digest, direct_reasons = normalize_artifact_sha256(
        direct_artifact_sha256, "direct"
    )
    reasons.extend(merc_reasons)
    reasons.extend(direct_reasons)
    if merc_digest and direct_digest and merc_digest != direct_digest:
        reasons.append(
            "model artifact sha256 mismatch between arms: "
            f"merc={merc_digest} direct={direct_digest} "
            "(same-weights must be proved by identical digests, not inferred)"
        )
    return reasons


def resolve_source_commit() -> str:
    """Pin the merc tree that produced this receipt. Empty if not in a git tree."""
    env = os.environ.get("MERC_SOURCE_COMMIT", "").strip()
    if env:
        return env
    try:
        out = subprocess.check_output(
            ["git", "rev-parse", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        )
        return out.strip()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        return ""


def build_scope(
    *,
    model: str,
    merc_artifact_sha256: str,
    direct_artifact_sha256: str,
    precision: str,
    engine: str,
    engine_build: str,
    hw_class: str,
    max_tokens: int,
    temperature: float,
    stream: bool,
    top_p: float | None,
    seed: int | None,
    prompt: str,
    concurrency_levels: list[int],
    requests_per_level: int,
) -> dict[str, Any]:
    """Scope block: every decode setting a revalidation must reproduce."""
    return {
        "model_ref": model,
        "merc_model_artifact_sha256": merc_artifact_sha256,
        "direct_model_artifact_sha256": direct_artifact_sha256,
        "precision": precision,
        "engine": engine,
        "engine_build": engine_build,
        "hw_class": hw_class,
        "max_tokens": max_tokens,
        "temperature": temperature,
        "stream": stream,
        "top_p": top_p,
        "seed": seed,
        "prompt": prompt,
        "concurrency_levels": list(concurrency_levels),
        "requests_per_level": requests_per_level,
        "min_samples_required": MIN_CELL_COST_SAMPLES,
    }


def default_budget() -> dict[str, Any]:
    return {
        "ttft_overhead_p95_ms": DEFAULT_BUDGET_TTFT_OVERHEAD_P95_MS,
        "throughput_loss_fraction": DEFAULT_BUDGET_THROUGHPUT_LOSS_FRACTION,
        "basis": (
            "policy ceiling calibrated to the 2026-07-28 gateway-parity measurement "
            "on Metal/cx-chat-1b (+3.5 ms TTFT p50, +9.6 ms TTFT p95, −1.86% "
            "aggregate throughput at c=1), with modest headroom; not a derived SLA"
        ),
    }


def evaluate_budget(
    summary: dict[str, Any],
    deltas: dict[str, Any],
    budget: dict[str, Any],
) -> list[str]:
    """Return refusals. Empty exactly when the declared budget was met.

    Shape matches CellPromotionEvidence.Refusals: a list of every failing rule
    rather than a single reason, so one evaluation reports the full set.
    Sample-count policy is enforced by the harness default (minCellCostSamples)
    and is not a budget refusal — budget is the overhead ceiling only.
    """
    refusals: list[str] = []
    ttft_p95 = summary.get("ttft_overhead_p95_ms")
    ceiling = budget.get("ttft_overhead_p95_ms")
    if ttft_p95 is None:
        refusals.append("summary_concurrency_1.ttft_overhead_p95_ms is missing")
    elif ceiling is None:
        refusals.append("budget.ttft_overhead_p95_ms is missing")
    elif float(ttft_p95) > float(ceiling):
        refusals.append(
            f"ttft overhead p95 {float(ttft_p95):.3f} ms exceeds budget "
            f"{float(ceiling):.3f} ms at concurrency=1"
        )

    level1 = deltas.get("1") or {}
    agg = level1.get("aggregate_tokens_per_sec") or {}
    direct_tps = agg.get("direct")
    merc_tps = agg.get("merc")
    loss_ceiling = budget.get("throughput_loss_fraction")
    if direct_tps is None or merc_tps is None:
        refusals.append(
            "concurrency=1 aggregate_tokens_per_sec missing on one or both arms"
        )
    elif float(direct_tps) <= 0:
        refusals.append("direct aggregate_tokens_per_sec at c=1 is not positive")
    elif loss_ceiling is None:
        refusals.append("budget.throughput_loss_fraction is missing")
    else:
        # Loss fraction is (direct − merc) / direct. Negative means merc was
        # faster; that always clears the loss budget.
        loss = (float(direct_tps) - float(merc_tps)) / float(direct_tps)
        if loss > float(loss_ceiling):
            refusals.append(
                f"throughput loss fraction {loss:.4f} exceeds budget "
                f"{float(loss_ceiling):.4f} at concurrency=1 "
                f"(merc={merc_tps} direct={direct_tps} tok/s)"
            )
    return refusals


def throughput_loss_fraction(deltas: dict[str, Any]) -> float | None:
    level1 = deltas.get("1") or {}
    agg = level1.get("aggregate_tokens_per_sec") or {}
    direct_tps = agg.get("direct")
    merc_tps = agg.get("merc")
    if direct_tps is None or merc_tps is None or float(direct_tps) <= 0:
        return None
    return round((float(direct_tps) - float(merc_tps)) / float(direct_tps), 6)


def _empty_level_accum(concurrency: int, requests: int) -> dict[str, Any]:
    return {
        "concurrency": concurrency,
        "requests_attempted": requests,
        "latencies": [],
        "ttfts": [],
        "itls": [],
        "tokens": [],
        "req_bytes": 0,
        "resp_bytes": 0,
        "errors": [],
        "wall_seconds": 0.0,
    }


def _finalize_level(accum: dict[str, Any]) -> dict[str, Any]:
    latencies: list[float] = accum["latencies"]
    ttfts: list[float] = accum["ttfts"]
    itls: list[float] = accum["itls"]
    tokens: list[int] = accum["tokens"]
    ok = len(latencies)
    wall = float(accum["wall_seconds"])
    total_tokens = sum(tokens)
    errors = accum["errors"]
    return {
        "concurrency": accum["concurrency"],
        "requests_attempted": accum["requests_attempted"],
        "requests_ok": ok,
        "errors": len(errors),
        "error_samples": errors[:5],
        "wall_seconds": round(wall, 3),
        "aggregate_tokens_per_sec": round(total_tokens / wall, 2) if wall > 0 else None,
        "aggregate_requests_per_sec": round(ok / wall, 3) if wall > 0 else None,
        "ttft": stats_ms(ttfts),
        "inter_token_latency": stats_ms(itls),
        "total_wall": stats_s(latencies),
        "completion_tokens_total": total_tokens,
        "bytes": {
            "request_total": accum["req_bytes"],
            "response_total": accum["resp_bytes"],
            "request_mean": round(accum["req_bytes"] / ok, 1) if ok else None,
            "response_mean": round(accum["resp_bytes"] / ok, 1) if ok else None,
        },
    }


def _run_batch(
    endpoint: str,
    key: str,
    model: str,
    max_tokens: int,
    concurrency: int,
    batch_size: int,
    accum: dict[str, Any],
) -> None:
    """Run batch_size completions keeping up to concurrency in flight; append to accum."""
    lock = threading.Lock()

    def worker() -> None:
        try:
            sample = one_stream(endpoint, key, model, max_tokens)
        except Exception as exc:  # noqa: BLE001 - recorded, not swallowed
            with lock:
                accum["errors"].append(f"{type(exc).__name__}: {exc}")
            return
        with lock:
            accum["latencies"].append(sample["total_s"])
            if sample["ttft_ms"] is not None:
                accum["ttfts"].append(sample["ttft_ms"])
            accum["itls"].extend(sample["itl_ms"])
            accum["tokens"].append(int(sample["completion_tokens"] or 0))
            accum["req_bytes"] += sample["request_bytes"]
            accum["resp_bytes"] += sample["response_bytes"]

    wall_start = time.perf_counter()
    threads: list[threading.Thread] = []
    for _ in range(batch_size):
        threads.append(threading.Thread(target=worker))
    running: list[threading.Thread] = []
    for t in threads:
        t.start()
        running.append(t)
        if len(running) >= concurrency:
            running.pop(0).join()
    for t in running:
        t.join()
    accum["wall_seconds"] += time.perf_counter() - wall_start


def run_interleaved_level(
    merc_endpoint: str,
    merc_key: str,
    direct_endpoint: str,
    direct_key: str,
    model: str,
    max_tokens: int,
    concurrency: int,
    requests: int,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Collect `requests` samples per arm, interleaving arms over time.

    Thermal and page-cache drift hit both sides equally: each round fires a
    concurrent batch on one arm then the other, alternating which arm leads.
    Concurrency semantics stay per-arm (never dual-load the engine).
    """
    merc_accum = _empty_level_accum(concurrency, requests)
    direct_accum = _empty_level_accum(concurrency, requests)

    # Warm both paths so cold-start does not land on whichever arm goes first.
    for endpoint, key in (
        (direct_endpoint, direct_key),
        (merc_endpoint, merc_key),
    ):
        try:
            one_stream(endpoint, key, model, min(8, max_tokens))
        except Exception:
            pass

    remaining = requests
    round_idx = 0
    while remaining > 0:
        batch = min(concurrency, remaining)
        order = (
            ("direct", direct_endpoint, direct_key, direct_accum),
            ("merc", merc_endpoint, merc_key, merc_accum),
        )
        if round_idx % 2 == 1:
            order = tuple(reversed(order))
        for _side, endpoint, key, accum in order:
            _run_batch(endpoint, key, model, max_tokens, concurrency, batch, accum)
        remaining -= batch
        round_idx += 1

    return _finalize_level(merc_accum), _finalize_level(direct_accum)


def build_receipt(
    *,
    comparable: bool,
    gate_passed: bool,
    incomparable_reasons: list[str],
    measurement_errors: list[str],
    refusals: list[str],
    config: dict[str, Any],
    scope: dict[str, Any],
    budget: dict[str, Any],
    probes: dict[str, Any],
    levels: dict[str, Any],
    deltas: dict[str, Any],
    summary: dict[str, Any],
    measured_at: str,
    merc_source_commit: str,
    features_disabled: list[str],
    allow_incomparable: bool,
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    report: dict[str, Any] = {
        "schema_version": 1,
        "kind": "gateway_parity",
        "gate_version": GATE_VERSION,
        "measured_at": measured_at,
        "merc_source_commit": merc_source_commit,
        "comparable": comparable,
        "gate_passed": gate_passed,
        "incomparable_reasons": incomparable_reasons,
        "measurement_errors": measurement_errors,
        "refusals": refusals,
        "budget": budget,
        "scope": scope,
        "features_disabled": list(features_disabled),
        "config": config,
        "probes": probes,
        "levels": levels,
        "deltas": deltas if (comparable or allow_incomparable) else {},
        "summary_concurrency_1": summary if comparable else {},
        "comparability_warning": (
            "Valid only when both sides used the SAME model artifact (sha256), "
            "precision and max_tokens against the same engine. The harness "
            "refuses a comparison when digests disagree, probes disagree or "
            "labels are missing. Quoting one average across incomparable runs "
            "is not a measurement."
        ),
        "note": (
            "Deltas are merc − direct. Positive TTFT/ITL/wall means merc added "
            "latency. Throughput delta is merc − direct (negative means the "
            "gateway reduced aggregate tokens/s). Arms are interleaved per "
            f"batch so thermal drift is shared. n>={MIN_CELL_COST_SAMPLES} "
            "(minCellCostSamples)."
        ),
    }
    if extra:
        report.update(extra)
    return report


def self_test() -> int:
    """Offline unit tests for budget, digests, and scope. No live engine."""
    failures: list[str] = []

    def check(cond: bool, msg: str) -> None:
        if not cond:
            failures.append(msg)

    # 1) Mismatched artifact digests → incomparable.
    digest_a = "3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1"
    digest_b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    reasons = assert_comparable(
        "cx-chat-1b", 32,
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"accepted": True}, {"accepted": True},
        "merc label", "direct label",
        merc_artifact_sha256=digest_a,
        direct_artifact_sha256=digest_b,
    )
    check(any("mismatch" in r for r in reasons),
          f"mismatched digests must be incomparable, got {reasons!r}")

    # 2) Matching digests + healthy probes → comparable (no reasons).
    reasons_ok = assert_comparable(
        "cx-chat-1b", 32,
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"accepted": True}, {"accepted": True},
        "merc label", "direct label",
        merc_artifact_sha256=digest_a,
        direct_artifact_sha256=digest_a,
    )
    check(reasons_ok == [], f"matching digests should compare, got {reasons_ok!r}")

    # 3) Missing digests → incomparable (proved, not inferred).
    reasons_missing = assert_comparable(
        "cx-chat-1b", 32,
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"reachable": True, "model_ids": ["cx-chat-1b"]},
        {"accepted": True}, {"accepted": True},
        "merc label", "direct label",
        merc_artifact_sha256="",
        direct_artifact_sha256="",
    )
    check(any("artifact sha256 is required" in r for r in reasons_missing),
          f"missing digests must refuse, got {reasons_missing!r}")

    # 4) Scope round-trip.
    scope = build_scope(
        model="cx-chat-1b",
        merc_artifact_sha256=digest_a,
        direct_artifact_sha256=digest_a,
        precision="GGUF-Q4_K_M",
        engine="llama_cpp",
        engine_build="d48a56eff",
        hw_class="apple_silicon_ultra",
        max_tokens=32,
        temperature=0,
        stream=True,
        top_p=None,
        seed=None,
        prompt=PROMPT,
        concurrency_levels=[1, 8, 32],
        requests_per_level=MIN_CELL_COST_SAMPLES,
    )
    encoded = json.dumps(scope)
    decoded = json.loads(encoded)
    check(decoded == scope, "scope must JSON round-trip identically")
    check(decoded["merc_model_artifact_sha256"] == digest_a, "scope merc digest")
    check(decoded["direct_model_artifact_sha256"] == digest_a, "scope direct digest")
    check(decoded["min_samples_required"] == MIN_CELL_COST_SAMPLES,
          "scope must pin minCellCostSamples")

    # 5) Budget pass with headroom on today's numbers.
    summary_ok = {
        "ttft_overhead_p50_ms": 3.474,
        "ttft_overhead_p95_ms": 9.561,
        "ttft_overhead_p99_ms": 9.561,
    }
    deltas_ok = {
        "1": {
            "aggregate_tokens_per_sec": {
                "merc": 144.85,
                "direct": 147.6,
                "delta": -2.75,
            }
        }
    }
    budget = default_budget()
    refusals_ok = evaluate_budget(summary_ok, deltas_ok, budget)
    check(refusals_ok == [], f"today's numbers must clear budget, got {refusals_ok!r}")
    loss = throughput_loss_fraction(deltas_ok)
    check(loss is not None and abs(loss - 0.01863) < 1e-4,
          f"throughput loss should be ~1.863%, got {loss}")

    # 6) Overhead above budget → non-empty refusals.
    summary_bad = dict(summary_ok, ttft_overhead_p95_ms=50.0)
    refusals_ttft = evaluate_budget(summary_bad, deltas_ok, budget)
    check(any("ttft overhead p95" in r for r in refusals_ttft),
          f"over-budget TTFT must refuse, got {refusals_ttft!r}")

    deltas_slow = {
        "1": {
            "aggregate_tokens_per_sec": {
                "merc": 100.0,
                "direct": 147.6,
                "delta": -47.6,
            }
        }
    }
    refusals_tps = evaluate_budget(summary_ok, deltas_slow, budget)
    check(any("throughput loss" in r for r in refusals_tps),
          f"over-budget throughput must refuse, got {refusals_tps!r}")

    # 7) features_disabled names what stream:true turns off.
    check("exact_reuse" in FEATURES_DISABLED, "exact_reuse must be listed disabled")
    check("request_coalescing" in FEATURES_DISABLED,
          "request_coalescing must be listed disabled")

    if failures:
        print(f"gateway-parity self-test: FAIL ({len(failures)})")
        for f in failures:
            print(f"  - {f}")
        return 1
    print("gateway-parity self-test: PASS")
    return 0


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--self-test", action="store_true",
                    help="run offline unit tests (budget, digests, scope) and exit")
    ap.add_argument("--merc-base-url",
                    help="merc OpenAI-compatible base URL (…/v1)")
    ap.add_argument("--direct-base-url",
                    help="engine base URL for the direct baseline (…/v1)")
    ap.add_argument("--model", default="cx-chat-1b")
    ap.add_argument("--max-tokens", type=int, default=64)
    ap.add_argument("--concurrency", default="1,8,32",
                    help="comma-separated concurrency levels")
    ap.add_argument("--requests-per-level", type=int, default=MIN_CELL_COST_SAMPLES,
                    help=f"completions per arm at each concurrency level "
                         f"(default {MIN_CELL_COST_SAMPLES} = minCellCostSamples)")
    ap.add_argument("--merc-label", default="",
                    help="exact merc-path description for the receipt "
                         "(routing, control-plane host, etc.)")
    ap.add_argument("--direct-label", default="",
                    help="exact direct-path description for the receipt "
                         "(engine/hardware/model/precision)")
    ap.add_argument("--merc-model-artifact-sha256", default="",
                    help="sha256 of the model weights behind the merc arm")
    ap.add_argument("--direct-model-artifact-sha256", default="",
                    help="sha256 of the model weights behind the direct arm")
    ap.add_argument("--model-artifact-sha256", default="",
                    help="shared sha256 applied to both arms when per-arm "
                         "flags are not set")
    ap.add_argument("--precision", default="",
                    help="precision label for the scope block (e.g. GGUF-Q4_K_M)")
    ap.add_argument("--engine", default="llama_cpp",
                    help="engine name for the scope block")
    ap.add_argument("--engine-build", default="",
                    help="engine build id/commit for the scope block")
    ap.add_argument("--hw-class", default="",
                    help="hardware class for the scope block "
                         "(e.g. apple_silicon_ultra)")
    ap.add_argument("--budget-ttft-overhead-p95-ms", type=float,
                    default=DEFAULT_BUDGET_TTFT_OVERHEAD_P95_MS)
    ap.add_argument("--budget-throughput-loss-fraction", type=float,
                    default=DEFAULT_BUDGET_THROUGHPUT_LOSS_FRACTION)
    ap.add_argument("--out", default="evidence/perf/gateway-parity.json")
    ap.add_argument("--allow-incomparable", action="store_true",
                    help="write the receipt even when sides are not comparable "
                         "(comparison block will be null; exit 2)")
    args = ap.parse_args()

    if args.self_test:
        return self_test()

    if not args.merc_base_url or not args.direct_base_url:
        print("--merc-base-url and --direct-base-url are required "
              "(or pass --self-test)", file=sys.stderr)
        return 2

    merc_key = os.environ.get("MERC_BENCHMARK_API_KEY", "")
    direct_key = os.environ.get("MERC_DIRECT_VLLM_API_KEY", "")
    if not merc_key or not direct_key:
        print("both MERC_BENCHMARK_API_KEY and MERC_DIRECT_VLLM_API_KEY are required",
              file=sys.stderr)
        return 2

    levels = [int(x) for x in args.concurrency.split(",") if x.strip()]
    if not levels:
        print("no concurrency levels", file=sys.stderr)
        return 2

    # Defaults that still pin the comparison in the receipt when the operator
    # did not pass labels. Empty labels fail the comparability gate.
    merc_label = args.merc_label or os.environ.get("MERC_GATEWAY_PARITY_MERC_LABEL", "")
    direct_label = args.direct_label or os.environ.get("MERC_GATEWAY_PARITY_DIRECT_LABEL", "")

    shared_digest = (
        args.model_artifact_sha256
        or os.environ.get("MERC_GATEWAY_PARITY_MODEL_ARTIFACT_SHA256", "")
    )
    merc_digest = (
        args.merc_model_artifact_sha256
        or os.environ.get("MERC_GATEWAY_PARITY_MERC_MODEL_ARTIFACT_SHA256", "")
        or shared_digest
    )
    direct_digest = (
        args.direct_model_artifact_sha256
        or os.environ.get("MERC_GATEWAY_PARITY_DIRECT_MODEL_ARTIFACT_SHA256", "")
        or shared_digest
    )
    precision = args.precision or os.environ.get("MERC_GATEWAY_PARITY_PRECISION", "")
    engine = args.engine or os.environ.get("MERC_GATEWAY_PARITY_ENGINE", "llama_cpp")
    engine_build = args.engine_build or os.environ.get(
        "MERC_GATEWAY_PARITY_ENGINE_BUILD", ""
    )
    hw_class = args.hw_class or os.environ.get("MERC_GATEWAY_PARITY_HW_CLASS", "")

    measured_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    merc_source_commit = resolve_source_commit()
    budget = {
        "ttft_overhead_p95_ms": args.budget_ttft_overhead_p95_ms,
        "throughput_loss_fraction": args.budget_throughput_loss_fraction,
        "basis": default_budget()["basis"],
    }
    scope = build_scope(
        model=args.model,
        merc_artifact_sha256=(merc_digest or "").strip().lower(),
        direct_artifact_sha256=(direct_digest or "").strip().lower(),
        precision=precision,
        engine=engine,
        engine_build=engine_build,
        hw_class=hw_class,
        max_tokens=args.max_tokens,
        temperature=0,
        stream=True,
        top_p=None,
        seed=None,
        prompt=PROMPT,
        concurrency_levels=levels,
        requests_per_level=args.requests_per_level,
    )

    print("probing both sides for comparability…")
    merc_probe = probe_models(args.merc_base_url, merc_key)
    direct_probe = probe_models(args.direct_base_url, direct_key)
    print("  smoke-accepting shared model on both sides…")
    merc_smoke = smoke_accepts_model(args.merc_base_url, merc_key, args.model)
    direct_smoke = smoke_accepts_model(args.direct_base_url, direct_key, args.model)
    incomparable = assert_comparable(
        args.model, args.max_tokens,
        merc_probe, direct_probe, merc_smoke, direct_smoke,
        merc_label, direct_label,
        merc_artifact_sha256=merc_digest,
        direct_artifact_sha256=direct_digest,
    )

    config = {
        "model": args.model,
        "max_tokens": args.max_tokens,
        "temperature": 0,
        "stream": True,
        "prompt": PROMPT,
        "concurrency_levels": levels,
        "requests_per_level": args.requests_per_level,
        "merc_base_url": args.merc_base_url,
        "direct_base_url": args.direct_base_url,
        "merc_label": merc_label,
        "direct_label": direct_label,
        "interleaved_arms": True,
        "min_samples_required": MIN_CELL_COST_SAMPLES,
    }

    merc_levels: dict[str, Any] = {}
    direct_levels: dict[str, Any] = {}
    deltas: dict[str, Any] = {}
    measurement_errors: list[str] = list(incomparable)
    probes = {
        "merc": merc_probe,
        "direct": direct_probe,
        "merc_smoke": merc_smoke,
        "direct_smoke": direct_smoke,
    }

    if incomparable and not args.allow_incomparable:
        report = build_receipt(
            comparable=False,
            gate_passed=False,
            incomparable_reasons=incomparable,
            measurement_errors=measurement_errors,
            refusals=list(incomparable),
            config=config,
            scope=scope,
            budget=budget,
            probes=probes,
            levels={},
            deltas={},
            summary={},
            measured_at=measured_at,
            merc_source_commit=merc_source_commit,
            features_disabled=FEATURES_DISABLED,
            allow_incomparable=args.allow_incomparable,
            extra={
                "comparability_warning": (
                    "Refusing to report a comparison where the two sides are not "
                    "comparable. A mismatched comparison is worse than no comparison."
                ),
            },
        )
        os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
        with open(args.out, "w") as fh:
            json.dump(report, fh, indent=2)
            fh.write("\n")
        print("INCOMPARABLE:", "; ".join(incomparable), file=sys.stderr)
        print(f"wrote refusal receipt to {args.out}", file=sys.stderr)
        return 2

    # Interleave arms at each concurrency level so thermal / page-cache drift
    # is shared rather than sequential (all direct, then all merc).
    for level in levels:
        # Keep total work proportional but cap so c=32 does not take forever
        # on a laptop engine: at least requests_per_level, at most 4x.
        nreq = min(max(args.requests_per_level, level), args.requests_per_level * 4)
        print(f"\n=== interleaved c={level}: {nreq} requests per arm ===")
        print(f"  merc={args.merc_base_url} ({merc_label})")
        print(f"  direct={args.direct_base_url} ({direct_label})")
        merc_result, direct_result = run_interleaved_level(
            args.merc_base_url, merc_key,
            args.direct_base_url, direct_key,
            args.model, args.max_tokens, level, nreq,
        )
        merc_levels[str(level)] = merc_result
        direct_levels[str(level)] = direct_result
        for side, result in (("merc", merc_result), ("direct", direct_result)):
            print(
                f"  {side}: ok={result['requests_ok']}/{nreq} "
                f"agg={result['aggregate_tokens_per_sec']} tok/s "
                f"TTFT p50={result['ttft'].get('p50_ms')} "
                f"p95={result['ttft'].get('p95_ms')} "
                f"p99={result['ttft'].get('p99_ms')} ms "
                f"err={result['errors']}"
            )
            if result["errors"]:
                measurement_errors.extend(
                    f"{side}@c={level}: {e}" for e in result["error_samples"]
                )

    for level in levels:
        key = str(level)
        if key in merc_levels and key in direct_levels:
            deltas[key] = compare_levels(merc_levels[key], direct_levels[key])

    # Hard gate: any level where one side fully failed is not a comparison.
    comparable = not incomparable
    for key, _d in deltas.items():
        if (merc_levels[key]["requests_ok"] == 0) or (direct_levels[key]["requests_ok"] == 0):
            comparable = False
            measurement_errors.append(f"level {key}: one side recorded zero successful requests")

    # Summarize the concurrency=1 TTFT tail — the number people will quote.
    summary: dict[str, Any] = {}
    if "1" in deltas:
        loss = throughput_loss_fraction(deltas)
        summary = {
            "ttft_overhead_p50_ms": deltas["1"]["ttft_delta_ms"]["p50"],
            "ttft_overhead_p95_ms": deltas["1"]["ttft_delta_ms"]["p95"],
            "ttft_overhead_p99_ms": deltas["1"]["ttft_delta_ms"]["p99"],
            "itl_overhead_p50_ms": deltas["1"]["inter_token_latency_delta_ms"]["p50"],
            "itl_overhead_p95_ms": deltas["1"]["inter_token_latency_delta_ms"]["p95"],
            "wall_overhead_p50_s": deltas["1"]["total_wall_delta_s"]["p50_s"],
            "wall_overhead_p95_s": deltas["1"]["total_wall_delta_s"]["p95_s"],
            "throughput_loss_fraction": loss,
        }

    if args.requests_per_level < MIN_CELL_COST_SAMPLES:
        measurement_errors.append(
            f"requests_per_level={args.requests_per_level} is below "
            f"minCellCostSamples={MIN_CELL_COST_SAMPLES}; p95/p99 under "
            "nearest-rank are not receipt-grade at this n"
        )

    refusals: list[str] = []
    if comparable and not incomparable:
        refusals = evaluate_budget(summary, deltas, budget)
    else:
        # Incomparable runs have not met a budget; surface the reasons as
        # refusals so the CI receipt gate fails closed rather than reading an
        # empty list as "budget cleared".
        refusals = list(incomparable) + [
            e for e in measurement_errors if e not in incomparable
        ]

    gate_passed = (
        comparable
        and not measurement_errors
        and not incomparable
        and not refusals
    )

    report = build_receipt(
        comparable=comparable and not incomparable,
        gate_passed=gate_passed,
        incomparable_reasons=incomparable,
        measurement_errors=measurement_errors,
        refusals=refusals,
        config=config,
        scope=scope,
        budget=budget,
        probes=probes,
        levels={"merc": merc_levels, "direct": direct_levels},
        deltas=deltas,
        summary=summary,
        measured_at=measured_at,
        merc_source_commit=merc_source_commit,
        features_disabled=FEATURES_DISABLED,
        allow_incomparable=args.allow_incomparable,
    )

    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    with open(args.out, "w") as fh:
        json.dump(report, fh, indent=2)
        fh.write("\n")

    print(f"\nwrote {args.out}")
    if summary:
        print("concurrency=1 overhead (merc − direct):")
        print(f"  TTFT  p50={summary.get('ttft_overhead_p50_ms')} "
              f"p95={summary.get('ttft_overhead_p95_ms')} "
              f"p99={summary.get('ttft_overhead_p99_ms')} ms")
        print(f"  ITL   p50={summary.get('itl_overhead_p50_ms')} "
              f"p95={summary.get('itl_overhead_p95_ms')} ms")
        print(f"  wall  p50={summary.get('wall_overhead_p50_s')} "
              f"p95={summary.get('wall_overhead_p95_s')} s")
        print(f"  throughput loss fraction={summary.get('throughput_loss_fraction')}")
    print(f"  refusals={refusals or '[]'}")
    if not report["gate_passed"]:
        print("gate_passed=false", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
