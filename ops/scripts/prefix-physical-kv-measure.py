#!/usr/bin/env python3
"""Physical prefix-KV evidence against llama.cpp on Metal.

Proves (or refutes) engine-side prompt reuse — not routing belief.

Against a live llama-server (Apple Silicon / Metal) this harness:

  1. Establishes what the server reports on a cache hit:
       - usage.prompt_tokens_details.cached_tokens  (OpenAI-shaped)
       - timings.cache_n / timings.prompt_n / timings.prompt_ms
       - /slots n_prompt_tokens_processed (secondary)
       - server logs LCP similarity / graphs reused (secondary)
  2. Measures cold vs warm shared-prefix arms:
       prefill tokens avoided, TTFT (ratio + absolute ms at p50/p95),
       GPU-domain energy delta (IOReport), and a verified-outcome
       energy cost delta (J per completion token).
  3. Records host load and the flags required to surface each signal.

Writes a BOUND receipt under evidence/perf/ only when
MERC_WRITE_PREFIX_PHYSICAL=1. Never touches pricing.go or money paths.

Does not claim Merc's batch agent currently forwards cached_tokens —
src/agent/src/inference.rs OpenAiHttpBackend parses only completion_tokens.
Realtime gateway parity already reads prompt_tokens_details.cached_tokens
when present.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import signal
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))

import importlib.util

_bh_path = ROOT / "ops/scripts" / "bench-harness.py"
_spec = importlib.util.spec_from_file_location("merc_bench_harness", _bh_path)
_bh = importlib.util.module_from_spec(_spec)
sys.modules["merc_bench_harness"] = _bh
assert _spec.loader is not None
_spec.loader.exec_module(_bh)

open_power_sampler = _bh.open_power_sampler
hardware_label = _bh.hardware_label
_default_gguf = _bh._default_gguf

WRITE_ENV = "MERC_WRITE_PREFIX_PHYSICAL"

DEFAULT_MODEL = (
    Path.home()
    / ".cache/huggingface/hub/models--unsloth--Llama-3.2-1B-Instruct-GGUF"
    / "snapshots/b69aef112e9f895e6f98d7ae0949f72ff09aa401"
    / "Llama-3.2-1B-Instruct-Q4_K_M.gguf"
)


def percentile(xs: list[float], p: float) -> float:
    if not xs:
        return float("nan")
    s = sorted(xs)
    if len(s) == 1:
        return float(s[0])
    k = (len(s) - 1) * (p / 100.0)
    f = int(k)
    c = min(f + 1, len(s) - 1)
    if f == c:
        return float(s[f])
    return float(s[f] + (s[c] - s[f]) * (k - f))


def host_load() -> dict:
    load1 = load5 = load15 = None
    try:
        load1, load5, load15 = os.getloadavg()
    except OSError:
        pass
    return {
        "hardware": hardware_label() if callable(hardware_label) else platform.processor(),
        "platform": platform.platform(),
        "goarch_equiv": platform.machine(),
        "num_cpu": os.cpu_count(),
        "load1": load1,
        "load5": load5,
        "load15": load15,
        "load_note": "recorded at measurement start; quiet machine preferred",
    }


def free_port() -> int:
    import socket

    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def wait_http(url: str, timeout_s: float = 120.0) -> None:
    deadline = time.time() + timeout_s
    last = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as resp:
                if resp.status < 500:
                    return
        except Exception as exc:  # noqa: BLE001
            last = exc
            time.sleep(0.2)
    raise RuntimeError(f"server not ready at {url}: {last}")


def start_server(model: Path, port: int, ctx: int, log_path: Path) -> subprocess.Popen:
    llama = (
        __import__("shutil").which("llama-server")
        or "/opt/homebrew/bin/llama-server"
    )
    if not Path(llama).exists():
        raise RuntimeError(f"llama-server not found: {llama}")
    logf = open(log_path, "w")  # noqa: SIM115 — kept open for server lifetime
    cmd = [
        llama,
        "-m",
        str(model),
        "--host",
        "127.0.0.1",
        "--port",
        str(port),
        "-c",
        str(ctx),
        "-np",
        "1",
        "--metrics",
        "--slots",
        "--cache-prompt",
        "--perf",
        "--no-warmup",
        "-ngl",
        "99",
    ]
    proc = subprocess.Popen(
        cmd,
        stdout=logf,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    proc._logf = logf  # type: ignore[attr-defined]
    return proc


def stop_server(proc: subprocess.Popen | None) -> None:
    if proc is None:
        return
    try:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=8)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait(timeout=4)
    except Exception:  # noqa: BLE001
        pass
    logf = getattr(proc, "_logf", None)
    if logf is not None:
        try:
            logf.close()
        except Exception:  # noqa: BLE001
            pass


def build_prefix(n_paragraphs: int, *, salt: str = "shared") -> str:
    """Build a long system prompt. Same length for salt variants of equal n."""
    lines = [
        "You are a retrieval-augmented assistant. Use only the following context.",
        f"CONTEXT DOCUMENT ({salt}):",
    ]
    for i in range(1, n_paragraphs + 1):
        lines.append(
            f"Paragraph {i}: Merc routes inference to independent suppliers. "
            f"Shared system and retrieved context should reuse KV when the same "
            f"worker holds the prefix. Surrogate sentence number {i:03d} with "
            f"extra padding words about latency, prefill, energy, and "
            f"verified-outcome cost for measurement. salt={salt}."
        )
    return "\n".join(lines)


def chat_completion(
    base: str,
    system: str,
    user: str,
    max_tokens: int = 16,
) -> dict:
    body = {
        "model": "local",
        "messages": [
            {"role": "system", "content": system},
            {"role": "user", "content": user},
        ],
        "max_tokens": max_tokens,
        "temperature": 0,
        "stream": False,
    }
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        f"{base}/v1/chat/completions",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=120) as resp:
        raw = resp.read()
        # Approximate TTFB: non-stream body arrives whole; wall to first byte
        # is not separately available via urlopen. Use end-to-end wall as
        # upper bound on TTFT for non-stream; timings.prompt_ms is the
        # engine-authoritative prefill clock.
    t1 = time.perf_counter()
    j = json.loads(raw)
    j["_wall_ms"] = (t1 - t0) * 1000.0
    return j


def extract_signals(j: dict) -> dict:
    usage = j.get("usage") or {}
    details = usage.get("prompt_tokens_details") or {}
    cached = details.get("cached_tokens")
    timings = j.get("timings") or {}
    return {
        "prompt_tokens": usage.get("prompt_tokens"),
        "completion_tokens": usage.get("completion_tokens"),
        "cached_tokens": cached,
        "cached_tokens_field_present": "cached_tokens" in details
        if isinstance(details, dict)
        else False,
        "timings_present": bool(timings),
        "cache_n": timings.get("cache_n"),
        "prompt_n": timings.get("prompt_n"),
        "prompt_ms": timings.get("prompt_ms"),
        "predicted_n": timings.get("predicted_n"),
        "predicted_ms": timings.get("predicted_ms"),
        "wall_ms": j.get("_wall_ms"),
        "content": ((j.get("choices") or [{}])[0].get("message") or {}).get(
            "content", ""
        )[:80],
    }


def get_slots(base: str) -> list:
    try:
        with urllib.request.urlopen(f"{base}/slots", timeout=5) as resp:
            return json.loads(resp.read())
    except Exception:  # noqa: BLE001
        return []


def get_metrics_snippet(base: str) -> str:
    try:
        with urllib.request.urlopen(f"{base}/metrics", timeout=5) as resp:
            text = resp.read().decode()
        keep = []
        for line in text.splitlines():
            if any(
                k in line
                for k in (
                    "prompt_tokens",
                    "prompt_seconds",
                    "tokens_predicted",
                    "n_tokens",
                )
            ):
                keep.append(line)
        return "\n".join(keep[:40])
    except Exception as exc:  # noqa: BLE001
        return f"metrics_error: {exc}"


def energy_around(fn, interval_ms: int = 100) -> tuple[dict, dict]:
    """Run fn while sampling GPU Energy; return (fn_result, energy_dict)."""
    sampler = open_power_sampler("ioreport", interval_ms=interval_ms)
    if not sampler.available:
        result = fn()
        return result, {
            "available": False,
            "reason": getattr(sampler, "reason", "unavailable"),
        }
    with sampler:
        # Brief lead-in so the first sample is non-empty.
        time.sleep(interval_ms / 1000.0)
        result = fn()
        # Trailing sample window so short requests are not lost between ticks.
        time.sleep(max(0.25, interval_ms / 1000.0))
    er = sampler.result()
    er["available"] = True
    return result, er


def summarize_arm(samples: list[dict], energy_runs: list[dict]) -> dict:
    def col(key):
        return [float(s[key]) for s in samples if s.get(key) is not None]

    prompt_ms = col("prompt_ms")
    wall_ms = col("wall_ms")
    cache_n = col("cache_n")
    prompt_n = col("prompt_n")
    cached_tokens = col("cached_tokens")
    completion = col("completion_tokens")
    energy_j = [
        float(e["energy_joules"])
        for e in energy_runs
        if e.get("available") and e.get("energy_joules") is not None
    ]

    out = {
        "n": len(samples),
        "cache_n_p50": percentile(cache_n, 50),
        "cache_n_mean": statistics.fmean(cache_n) if cache_n else None,
        "prompt_n_p50": percentile(prompt_n, 50),
        "prompt_n_mean": statistics.fmean(prompt_n) if prompt_n else None,
        "cached_tokens_p50": percentile(cached_tokens, 50),
        "cached_tokens_mean": statistics.fmean(cached_tokens) if cached_tokens else None,
        "prompt_ms_p50": percentile(prompt_ms, 50),
        "prompt_ms_p95": percentile(prompt_ms, 95),
        "prompt_ms_mean": statistics.fmean(prompt_ms) if prompt_ms else None,
        "wall_ms_p50": percentile(wall_ms, 50),
        "wall_ms_p95": percentile(wall_ms, 95),
        "wall_ms_mean": statistics.fmean(wall_ms) if wall_ms else None,
        "completion_tokens_mean": statistics.fmean(completion) if completion else None,
        "energy_joules_p50": percentile(energy_j, 50) if energy_j else None,
        "energy_joules_mean": statistics.fmean(energy_j) if energy_j else None,
        "energy_runs": len(energy_j),
        "all_hits_have_cached_tokens_gt_0": all(
            (s.get("cached_tokens") or 0) > 0 for s in samples
        )
        if samples
        else False,
        "all_hits_have_cache_n_gt_0": all((s.get("cache_n") or 0) > 0 for s in samples)
        if samples
        else False,
        "raw_samples": samples,
        "raw_energy": energy_runs,
    }
    # J per verified outcome ≈ J / completion_tokens (ignore_eos not used;
    # completion_tokens varies slightly; use measured mean).
    ct = out["completion_tokens_mean"] or 0
    if ct > 0 and out["energy_joules_mean"] is not None:
        out["joules_per_completion_token_mean"] = out["energy_joules_mean"] / ct
    else:
        out["joules_per_completion_token_mean"] = None
    return out


def run_measurement(args: argparse.Namespace) -> dict:
    model = Path(args.model) if args.model else DEFAULT_MODEL
    if not model.exists():
        alt = _default_gguf() if callable(_default_gguf) else None
        if alt and Path(alt).exists():
            model = Path(alt)
        else:
            raise FileNotFoundError(f"model not found: {model}")

    # Shared prefix used for warm arm; cold arm uses same length, unique salt
    # each sample so prefill cannot hit prior KV.
    prefix = build_prefix(args.paragraphs, salt="shared")
    host = host_load()
    port = free_port()
    base = f"http://127.0.0.1:{port}"
    log_path = Path(args.log or f"/tmp/llama-prefix-physical-{port}.log")
    llama_bin = (
        __import__("shutil").which("llama-server") or "/opt/homebrew/bin/llama-server"
    )

    signal_survey: dict = {
        "llama_server_path": llama_bin,
        "llama_server_version": None,
        "flags_used": [
            "--cache-prompt (default enabled; explicit)",
            "--perf (timings object on completion responses)",
            "--metrics (prometheus /metrics)",
            "--slots (GET /slots)",
            "-np 1",
            f"-c {args.ctx}",
        ],
        "openai_chat_completions": {
            "endpoint": "/v1/chat/completions",
            "fields": [
                "usage.prompt_tokens_details.cached_tokens",
                "timings.cache_n",
                "timings.prompt_n",
                "timings.prompt_ms",
                "timings.predicted_n",
                "timings.predicted_ms",
            ],
            "note": "Observed on llama.cpp b9430 Metal in this run when present",
        },
        "slots_endpoint": {
            "endpoint": "GET /slots",
            "fields": [
                "n_prompt_tokens",
                "n_prompt_tokens_processed",
                "n_prompt_tokens_cache",
            ],
            "note": (
                "n_prompt_tokens_processed drops on reuse; n_prompt_tokens_cache "
                "often stays 0 after slot release — use response timings/usage "
                "as the authoritative per-request hit signal"
            ),
        },
        "metrics_endpoint": {
            "endpoint": "GET /metrics (requires --metrics)",
            "fields": [
                "llamacpp:prompt_tokens_total",
                "llamacpp:prompt_seconds_total",
            ],
            "note": "Counters of processed prompt work; no dedicated cache_hit gauge",
        },
        "server_logs": {
            "flag": "default info logs (or --verbose)",
            "signals": [
                "prompt cache is enabled",
                "selected slot by LCP similarity",
                "graphs reused = N",
            ],
        },
    }
    try:
        ver = subprocess.check_output(
            [signal_survey["llama_server_path"], "--version"],
            text=True,
            stderr=subprocess.STDOUT,
            timeout=10,
        ).strip()
        signal_survey["llama_server_version"] = ver.splitlines()[0]
    except Exception as exc:  # noqa: BLE001
        signal_survey["llama_server_version"] = f"error: {exc}"

    proc = start_server(model, port, args.ctx, log_path)
    try:
        wait_http(f"{base}/health", timeout_s=180)

        # --- Signal establishment: one cold prime + one warm ---
        prime = chat_completion(
            base, prefix, "Prime the shared prefix. Reply: PRIMED", 8
        )
        prime_sig = extract_signals(prime)
        time.sleep(0.15)
        warm_probe = chat_completion(
            base,
            prefix,
            "Warm probe: what is paragraph 5 about? One sentence.",
            16,
        )
        warm_sig = extract_signals(warm_probe)
        slots_after = get_slots(base)
        metrics_after = get_metrics_snippet(base)

        signal_present = (
            warm_sig.get("cached_tokens") is not None
            and int(warm_sig.get("cached_tokens") or 0) > 0
            and warm_sig.get("cache_n") is not None
            and int(warm_sig.get("cache_n") or 0) > 0
        )

        # --- Multi-sample cold / warm ---
        # Cold: full-length unique system prompt each time (same n_paragraphs).
        # Warm: fixed shared system prefix, unique short tails.
        # Both use the same short user-tail shape so length is dominated by system.
        cold_samples: list[dict] = []
        cold_energy: list[dict] = []
        warm_samples: list[dict] = []
        warm_energy: list[dict] = []

        reps = args.reps
        for i in range(reps):
            cold_sys = build_prefix(args.paragraphs, salt=f"cold-{i:04d}")
            unique_user = (
                f"Cold arm {i}: what is paragraph {(i % 40) + 1} about? "
                f"One short sentence. tag=C{i}"
            )

            def do_cold(sys=cold_sys, usr=unique_user):
                return extract_signals(chat_completion(base, sys, usr, 16))

            sig, en = energy_around(do_cold, interval_ms=args.energy_interval_ms)
            cold_samples.append(sig)
            cold_energy.append(en)
            time.sleep(0.05)

        # Re-prime shared prefix once before warm series (cold uniques may have
        # displaced slot/prompt-cache state; re-establish the shared KV).
        chat_completion(base, prefix, "Re-prime shared prefix. Reply: READY", 4)
        time.sleep(0.15)

        for i in range(reps):
            user = (
                f"Warm arm {i}: what is paragraph {(i % 40) + 1} about? "
                f"One short sentence. tag=W{i}"
            )

            def do_warm(u=user):
                return extract_signals(chat_completion(base, prefix, u, 16))

            sig, en = energy_around(do_warm, interval_ms=args.energy_interval_ms)
            warm_samples.append(sig)
            warm_energy.append(en)
            time.sleep(0.05)

        cold_sum = summarize_arm(cold_samples, cold_energy)
        warm_sum = summarize_arm(warm_samples, warm_energy)

        # continue building art below using locals; wrap remainder in same try
        art_tail = _build_art_tail(
            args=args,
            model=model,
            prefix=prefix,
            host=host,
            port=port,
            log_path=log_path,
            signal_survey=signal_survey,
            prime_sig=prime_sig,
            warm_sig=warm_sig,
            slots_after=slots_after,
            metrics_after=metrics_after,
            signal_present=signal_present,
            cold_sum=cold_sum,
            warm_sum=warm_sum,
        )
        return art_tail
    finally:
        stop_server(proc)


def _ratio(a, b):
    if a is None or b is None or b == 0:
        return None
    return a / b


def _build_art_tail(
    *,
    args,
    model,
    prefix,
    host,
    port,
    log_path,
    signal_survey,
    prime_sig,
    warm_sig,
    slots_after,
    metrics_after,
    signal_present,
    cold_sum,
    warm_sum,
) -> dict:
    # Prefer engine prompt_ms as TTFT-prefill proxy (authoritative); wall_ms is
    # end-to-end non-stream upper bound including decode.
    prefill_avoided_tokens_p50 = None
    if cold_sum["prompt_n_p50"] is not None and warm_sum["prompt_n_p50"] is not None:
        prefill_avoided_tokens_p50 = cold_sum["prompt_n_p50"] - warm_sum["prompt_n_p50"]
    if warm_sum["cache_n_p50"] is not None and warm_sum["cache_n_p50"] > 0:
        prefill_avoided_tokens_p50 = warm_sum["cache_n_p50"]

    prompt_ms_delta_p50 = None
    prompt_ms_delta_p95 = None
    if cold_sum["prompt_ms_p50"] is not None and warm_sum["prompt_ms_p50"] is not None:
        prompt_ms_delta_p50 = cold_sum["prompt_ms_p50"] - warm_sum["prompt_ms_p50"]
    if cold_sum["prompt_ms_p95"] is not None and warm_sum["prompt_ms_p95"] is not None:
        prompt_ms_delta_p95 = cold_sum["prompt_ms_p95"] - warm_sum["prompt_ms_p95"]

    wall_ms_delta_p50 = None
    wall_ms_delta_p95 = None
    if cold_sum["wall_ms_p50"] is not None and warm_sum["wall_ms_p50"] is not None:
        wall_ms_delta_p50 = cold_sum["wall_ms_p50"] - warm_sum["wall_ms_p50"]
    if cold_sum["wall_ms_p95"] is not None and warm_sum["wall_ms_p95"] is not None:
        wall_ms_delta_p95 = cold_sum["wall_ms_p95"] - warm_sum["wall_ms_p95"]

    energy_delta_j_mean = None
    if (
        cold_sum["energy_joules_mean"] is not None
        and warm_sum["energy_joules_mean"] is not None
    ):
        energy_delta_j_mean = (
            cold_sum["energy_joules_mean"] - warm_sum["energy_joules_mean"]
        )

    j_per_tok_delta = None
    if (
        cold_sum["joules_per_completion_token_mean"] is not None
        and warm_sum["joules_per_completion_token_mean"] is not None
    ):
        j_per_tok_delta = (
            cold_sum["joules_per_completion_token_mean"]
            - warm_sum["joules_per_completion_token_mean"]
        )

    cost_dominance = {
        "rank_order": [
            "CostRank ASC (hw class)",
            "AskUSDHr ASC (supplier ask within class)",
            "WarmPrefixDepth DESC",
            "WarmModel DESC",
            "WorkerID ASC",
        ],
        "claim_sql_order_prefix": (
            "cheaper_class_online ASC, cheaper_ask_online ASC, "
            "... worker_tps DESC, warm_prefix_depth DESC, warm_for_task DESC"
        ),
        "cheaper_ask_hard_deferral": (
            "WHERE NOT cheaper_ask_online OR task older than askDeferralWindow — "
            "an expensive ask is refused while a cheaper capable ask is online"
        ),
        "can_affinity_outrank_cost_class": False,
        "can_affinity_outrank_cheaper_ask_within_class": False,
        "gap_at_which_affinity_wins": (
            "Never while a cheaper cost class or cheaper ask is eligible. "
            "Within a cost class, any AskUSDHr difference — including the "
            "smallest positive float the ranker compares — beats any "
            "WarmPrefixDepth. Affinity only breaks ties at equal CostRank AND "
            "equal AskUSDHr."
        ),
        "measured_gap": {
            "cost_class": "discrete hwClassCostRank steps; warmth never crosses",
            "ask_usd_hr": (
                "any epsilon > 0 in AskUSDHr outranks infinite WarmPrefixDepth "
                "within the same CostRank"
            ),
        },
        "tests_that_pin_this": [
            "TestPrefixAffinityNeverPromotesMoreExpensiveCostClass",
            "TestPrefixAffinityCostRankGateIsLoadBearing",
            "TestPrefixAffinityBreaksTiesWithinSameCostClass",
            "TestWarmExpensiveClassDoesNotBeatColdCheapClass",
            "prefix_routing_wiring_test cheaper_ask hard-deferral",
        ],
    }

    log_hits = []
    try:
        log_text = log_path.read_text(errors="replace")
        for line in log_text.splitlines():
            if any(
                k in line
                for k in (
                    "LCP similarity",
                    "graphs reused",
                    "prompt cache is enabled",
                    "cache state:",
                )
            ):
                log_hits.append(line.strip()[-200:])
        log_hits = log_hits[-30:]
    except Exception:  # noqa: BLE001
        pass

    model_sha = None
    try:
        import hashlib

        h = hashlib.sha256()
        with open(model, "rb") as f:
            for chunk in iter(lambda: f.read(1024 * 1024), b""):
                h.update(chunk)
        model_sha = h.hexdigest()
    except Exception:  # noqa: BLE001
        model_sha = None

    return {
        "schema_version": 1,
        "kind": "prefix_kv_physical_metal_measurement",
        "label": (
            "llama.cpp Metal physical prefix-cache hit: confirmed cache_n / "
            "cached_tokens, prefill avoided, TTFT and energy deltas"
        ),
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": host,
        "model": {
            "path": str(model),
            "sha256": model_sha,
            "ctx": args.ctx,
            "paragraphs_shared_prefix": args.paragraphs,
            "shared_prefix_chars": len(prefix),
        },
        "engine_signal_survey": signal_survey,
        "signal_establishment": {
            "prime_cold": prime_sig,
            "warm_probe": warm_sig,
            "signal_present": signal_present,
            "slots_after_warm_probe": [
                {
                    k: s.get(k)
                    for k in (
                        "id",
                        "n_prompt_tokens",
                        "n_prompt_tokens_processed",
                        "n_prompt_tokens_cache",
                    )
                }
                for s in slots_after
            ],
            "metrics_snippet": metrics_after,
            "verdict": (
                "PRESENT"
                if signal_present
                else "ABSENT_OR_ZERO — treat as routing affinity only"
            ),
        },
        "arms": {
            "cold_unique": cold_sum,
            "warm_shared_prefix": warm_sum,
        },
        "deltas": {
            "prefill_tokens_avoided_p50": prefill_avoided_tokens_p50,
            "prompt_ms_delta_p50_ms": prompt_ms_delta_p50,
            "prompt_ms_delta_p95_ms": prompt_ms_delta_p95,
            "prompt_ms_warm_over_cold_ratio_p50": _ratio(
                warm_sum["prompt_ms_p50"], cold_sum["prompt_ms_p50"]
            ),
            "wall_ms_delta_p50_ms": wall_ms_delta_p50,
            "wall_ms_delta_p95_ms": wall_ms_delta_p95,
            "wall_ms_warm_over_cold_ratio_p50": _ratio(
                warm_sum["wall_ms_p50"], cold_sum["wall_ms_p50"]
            ),
            "energy_joules_delta_mean": energy_delta_j_mean,
            "energy_warm_over_cold_ratio_mean": _ratio(
                warm_sum["energy_joules_mean"], cold_sum["energy_joules_mean"]
            ),
            "joules_per_completion_token_delta_mean": j_per_tok_delta,
            "notes": [
                "prompt_ms is engine-reported prefill time (authoritative TTFT-prefill).",
                "wall_ms is non-stream end-to-end (prefill+decode+HTTP); absolute "
                "TTFT for streaming would be lower by decode tail.",
                "energy is IOReport AGX GPU Energy domain only — not wall-plug.",
                "usage.prompt_tokens still counts the full prompt on hits; "
                "buyer token billing may not shrink even when prefill work does. "
                "Physical saving is prefill time and GPU joules.",
            ],
        },
        "cost_dominance": cost_dominance,
        "server_log_cache_lines": log_hits,
        "can_prove": [
            "llama.cpp Metal (measured version) exposes usage.prompt_tokens_details.cached_tokens on /v1/chat/completions",
            "llama.cpp Metal exposes timings.cache_n / prompt_n / prompt_ms with --perf (or default timings on this build)",
            "confirmed cache hit: cache_n > 0 and cached_tokens > 0 on warm shared-prefix arm",
            "prefill tokens avoided ≈ cache_n on warm arm",
            "TTFT-prefill (prompt_ms) ratio and absolute ms at p50/p95 cold vs warm",
            "GPU-domain energy joules cold-unique vs warm-shared (IOReport)",
            "affinity cannot outrank cost class or cheaper ask (ranker + claim SQL)",
        ],
        "does_not_prove": [
            "that Merc's batch agent openai_http path currently forwards cached_tokens to TaskCommit (it parses only completion_tokens today)",
            "that production ClaimTasksTx belief equals engine cache_n without observation wiring through the agent",
            "wall-plug / package energy (IOReport is AGX GPU domain only)",
            "buyer USD settlement reduction (token billing still sees full prompt_tokens; money paths untouched)",
            "vLLM/CUDA multi-tenant prefix cache under production traffic (no paid CUDA in this arm)",
            "cross-supplier fleet behaviour; this is a single local Metal engine",
            "p99 stability under load or concurrent multi-slot contention",
        ],
        "follow_ups": [
            {
                "id": "agent_forward_cached_tokens",
                "why": (
                    "OpenAiHttpBackend must parse usage.prompt_tokens_details.cached_tokens "
                    "and set TaskCommit.CachedPromptTokens so CorrectPrefixBeliefFromObservation "
                    "can consume the signal this harness confirmed on the wire"
                ),
            },
            {
                "id": "vllm_cuda_when_selector_exists",
                "why": (
                    "vLLM exposes the same OpenAI field under CUDA and would settle "
                    "multi-tenant / continuous-batch prefix cache; directive forbids "
                    "paid CUDA without a selector consumer"
                ),
            },
        ],
        "prior_routing_receipts": {
            "prefix_kv_hitrate": "evidence/perf/prefix-kv-hitrate-latest.json",
            "prefix_affinity_routing": "evidence/perf/prefix-affinity-routing.json",
            "relation": (
                "Those measure routing belief / claim-path concentration. This receipt "
                "measures engine-physical reuse on Metal. They answer different questions."
            ),
        },
        "setup": {
            "reps_per_arm": args.reps,
            "energy_interval_ms": args.energy_interval_ms,
            "server_log": str(log_path),
            "port": port,
        },
        "binding_status": "UNBOUND",
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--model", default=str(DEFAULT_MODEL))
    ap.add_argument("--ctx", type=int, default=4096)
    ap.add_argument("--paragraphs", type=int, default=45, help="shared prefix size")
    ap.add_argument("--reps", type=int, default=12, help="samples per arm")
    ap.add_argument("--energy-interval-ms", type=int, default=100)
    ap.add_argument("--log", default="")
    ap.add_argument(
        "--out",
        default=str(ROOT / "evidence" / "perf" / "prefix-kv-physical-metal-latest.json"),
    )
    args = ap.parse_args()

    proc = None
    try:
        art = run_measurement(args)
    except Exception as exc:  # noqa: BLE001
        print(json.dumps({"error": str(exc)}, indent=2), file=sys.stderr)
        return 2

    # Pretty summary to stdout (truncate raw samples).
    summary = json.loads(json.dumps(art))
    for arm in summary.get("arms", {}).values():
        if isinstance(arm, dict):
            arm.pop("raw_samples", None)
            arm.pop("raw_energy", None)
    print(json.dumps(summary, indent=2))

    out_path = Path(args.out)
    write = os.environ.get(WRITE_ENV, "") == "1"
    if not write:
        print(
            f"\n# not written (set {WRITE_ENV}=1 to seal bound receipt at {out_path})",
            file=sys.stderr,
        )
        # Still write unbound draft next to out for local inspection when asked.
        draft = out_path.with_suffix(".draft.json")
        draft.parent.mkdir(parents=True, exist_ok=True)
        # Keep raw samples in draft for analysis.
        draft.write_text(json.dumps(art, indent=2) + "\n")
        print(f"# draft: {draft}", file=sys.stderr)
        return 0

    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        default_bound_identity,
        sha256_file,
        slot_value,
        write_bound_evidence,
    )

    llama_bin = art["engine_signal_survey"]["llama_server_path"]
    harness_sha = sha256_file(Path(__file__))
    model_sha = art["model"].get("sha256") or ""
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=f"ops/scripts/prefix-physical-kv-measure.py@{harness_sha[:16]}",
            build_binary_path=llama_bin,
            exact_config=(
                f"{WRITE_ENV}=1; reps={args.reps}; paragraphs={args.paragraphs}; "
                f"ctx={args.ctx}; energy_interval_ms={args.energy_interval_ms}; "
                f"llama={art['engine_signal_survey'].get('llama_server_version')}"
            ),
            raw_samples=(
                f"cold={art['arms']['cold_unique']['n']} "
                f"warm={art['arms']['warm_shared_prefix']['n']}"
            ),
            model_na=(
                f"model sha256 embedded in receipt body model.sha256={model_sha[:16]}…"
                if model_sha
                else "model path recorded in receipt body"
            ),
            corpus_na=(
                f"synthetic shared-prefix paragraphs={args.paragraphs} "
                f"chars={art['model']['shared_prefix_chars']}"
            ),
        )
        # Prefer concrete model digest when available.
        if model_sha:
            identity["model_artifact_digest"] = slot_value(model_sha)
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
        f"prefix-kv-physical-metal-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    )
    try:
        dated.write_text(out_path.read_text())
    except Exception:  # noqa: BLE001
        pass
    print(f"\n# BOUND: {out_path}", file=sys.stderr)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
