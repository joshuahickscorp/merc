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
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from typing import Any

MERC_UA = "merc-gateway-parity/1.0"

# Same prompt as runtime-parity-sweep so a reader can put the two receipts
# next to each other without wondering whether the workload changed.
PROMPT = (
    "Write a short factual paragraph about the water cycle. "
    "Be specific and do not repeat yourself."
)


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


def assert_comparable(
    model: str,
    max_tokens: int,
    merc_probe: dict[str, Any],
    direct_probe: dict[str, Any],
    merc_smoke: dict[str, Any],
    direct_smoke: dict[str, Any],
    merc_label: str,
    direct_label: str,
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
    return reasons


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--merc-base-url", required=True,
                    help="merc OpenAI-compatible base URL (…/v1)")
    ap.add_argument("--direct-base-url", required=True,
                    help="engine base URL for the direct baseline (…/v1)")
    ap.add_argument("--model", default="cx-chat-1b")
    ap.add_argument("--max-tokens", type=int, default=64)
    ap.add_argument("--concurrency", default="1,8,32",
                    help="comma-separated concurrency levels")
    ap.add_argument("--requests-per-level", type=int, default=8,
                    help="completions collected at each concurrency level")
    ap.add_argument("--merc-label", default="",
                    help="exact merc-path description for the receipt "
                         "(routing, control-plane host, etc.)")
    ap.add_argument("--direct-label", default="",
                    help="exact direct-path description for the receipt "
                         "(engine/hardware/model/precision)")
    ap.add_argument("--out", default="evidence/perf/gateway-parity.json")
    ap.add_argument("--allow-incomparable", action="store_true",
                    help="write the receipt even when sides are not comparable "
                         "(comparison block will be null; exit 2)")
    args = ap.parse_args()

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
    }

    merc_levels: dict[str, Any] = {}
    direct_levels: dict[str, Any] = {}
    deltas: dict[str, Any] = {}
    measurement_errors: list[str] = list(incomparable)

    if incomparable and not args.allow_incomparable:
        report = {
            "schema_version": 1,
            "kind": "gateway_parity",
            "comparable": False,
            "gate_passed": False,
            "incomparable_reasons": incomparable,
            "config": config,
            "probes": {
                "merc": merc_probe,
                "direct": direct_probe,
                "merc_smoke": merc_smoke,
                "direct_smoke": direct_smoke,
            },
            "levels": {},
            "deltas": {},
            "comparability_warning": (
                "Refusing to report a comparison where the two sides are not "
                "comparable. A mismatched comparison is worse than no comparison."
            ),
        }
        os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
        with open(args.out, "w") as fh:
            json.dump(report, fh, indent=2)
            fh.write("\n")
        print("INCOMPARABLE:", "; ".join(incomparable), file=sys.stderr)
        print(f"wrote refusal receipt to {args.out}", file=sys.stderr)
        return 2

    # Run direct first so a broken merc does not burn engine warm-up, then merc.
    # Same prompts, same max_tokens, same concurrency schedule.
    for side, endpoint, key, bucket in (
        ("direct", args.direct_base_url, direct_key, direct_levels),
        ("merc", args.merc_base_url, merc_key, merc_levels),
    ):
        print(f"\n=== {side}: {endpoint} ({direct_label if side == 'direct' else merc_label}) ===")
        for level in levels:
            nreq = args.requests_per_level * max(1, level // max(levels[0], 1))
            # Keep total work proportional but cap so c=32 does not take forever
            # on a laptop engine: at least requests_per_level, at most 4x.
            nreq = min(max(args.requests_per_level, level), args.requests_per_level * 4)
            print(f"  concurrency {level:>3}: {nreq} requests…")
            result = run_level(endpoint, key, args.model, args.max_tokens, level, nreq)
            bucket[str(level)] = result
            print(
                f"    ok={result['requests_ok']}/{nreq} "
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
    for key, d in deltas.items():
        if (merc_levels[key]["requests_ok"] == 0) or (direct_levels[key]["requests_ok"] == 0):
            comparable = False
            measurement_errors.append(f"level {key}: one side recorded zero successful requests")

    # Summarize the concurrency=1 TTFT tail — the number people will quote.
    summary: dict[str, Any] = {}
    if "1" in deltas:
        summary = {
            "ttft_overhead_p50_ms": deltas["1"]["ttft_delta_ms"]["p50"],
            "ttft_overhead_p95_ms": deltas["1"]["ttft_delta_ms"]["p95"],
            "ttft_overhead_p99_ms": deltas["1"]["ttft_delta_ms"]["p99"],
            "itl_overhead_p50_ms": deltas["1"]["inter_token_latency_delta_ms"]["p50"],
            "itl_overhead_p95_ms": deltas["1"]["inter_token_latency_delta_ms"]["p95"],
            "wall_overhead_p50_s": deltas["1"]["total_wall_delta_s"]["p50_s"],
            "wall_overhead_p95_s": deltas["1"]["total_wall_delta_s"]["p95_s"],
        }

    report = {
        "schema_version": 1,
        "kind": "gateway_parity",
        "comparable": comparable and not incomparable,
        "gate_passed": comparable and not measurement_errors and not incomparable,
        "incomparable_reasons": incomparable,
        "measurement_errors": measurement_errors,
        "config": config,
        "probes": {
            "merc": merc_probe,
            "direct": direct_probe,
            "merc_smoke": merc_smoke,
            "direct_smoke": direct_smoke,
        },
        "levels": {
            "merc": merc_levels,
            "direct": direct_levels,
        },
        "deltas": deltas if (comparable or args.allow_incomparable) else {},
        "summary_concurrency_1": summary if comparable else {},
        "comparability_warning": (
            "Valid only when both sides used the SAME model, precision and "
            "max_tokens against the same engine. The harness refuses a "
            "comparison when probes disagree or labels are missing. Quoting "
            "one average across incomparable runs is not a measurement."
        ),
        "note": (
            "Deltas are merc − direct. Positive TTFT/ITL/wall means merc added "
            "latency. Throughput delta is merc − direct (negative means the "
            "gateway reduced aggregate tokens/s)."
        ),
    }

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
    if not report["gate_passed"]:
        print("gate_passed=false", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
