#!/usr/bin/env python3
"""Wall-energy inference benchmark harness.

Records, for every run: runtime, model, batch, prompt length, output length,
prefix state, watts, tokens, errors and elapsed time -- one JSON object per run
on stdout, and a JSONL log.

Energy honesty: on macOS the only wall-power source is `powermetrics`, which
requires root. When it is unavailable every energy field is null and
`power_source` says why. The harness never estimates watts from a datasheet and
presents it as measurement -- an unmeasured number that looks measured is worse
than no number.

    # measured energy (asks for your password once)
    sudo python3 scripts/bench-harness.py --sweep quick

    # no energy, everything else still measured
    python3 scripts/bench-harness.py --sweep quick

Workload classes:
  INTERACTIVE         batch 1, latency-shaped: TTFT and inter-token latency
  BATCH               large batch, independent prompts, throughput-shaped
  SHARED_PREFIX_BATCH large batch sharing a system prompt, prefill computed once
"""

import argparse
import json
import os
import platform
import shutil
import statistics
import subprocess
import sys
import threading
import time
from dataclasses import dataclass, asdict, field
from pathlib import Path

SCHEMA_VERSION = 2

WORKLOAD_CLASSES = ("INTERACTIVE", "BATCH", "SHARED_PREFIX_BATCH")
PREFIX_STATES = ("NONE", "COLD", "SHARED")


# --------------------------------------------------------------------------- power


class PowerSampler:
    """Samples package power via powermetrics. Null-safe when not root."""

    def __init__(self, interval_ms: int = 200):
        self.interval_ms = interval_ms
        self.samples: list[float] = []
        self._proc = None
        self._thread = None
        self._stop = threading.Event()
        self.reason = None

        if platform.system() != "Darwin":
            self.reason = f"unsupported platform {platform.system()}"
        elif shutil.which("powermetrics") is None:
            self.reason = "powermetrics not found"
        elif os.geteuid() != 0:
            self.reason = "powermetrics requires root; re-run under sudo for energy"

    @property
    def available(self) -> bool:
        return self.reason is None

    def _read(self):
        for line in self._proc.stdout:
            if self._stop.is_set():
                return
            # "Combined Power (CPU + GPU + ANE): 12345 mW"
            if "Combined Power" in line and "mW" in line:
                try:
                    self.samples.append(float(line.rsplit(":", 1)[1].strip().split()[0]) / 1000.0)
                except (ValueError, IndexError):
                    pass

    def __enter__(self):
        if not self.available:
            return self
        self._proc = subprocess.Popen(
            ["powermetrics", "--samplers", "cpu_power,gpu_power",
             "-i", str(self.interval_ms), "-n", "0"],
            stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True,
        )
        self._thread = threading.Thread(target=self._read, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *exc):
        if self._proc is not None:
            self._stop.set()
            self._proc.terminate()
            try:
                self._proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._proc.kill()
        return False

    def result(self) -> dict:
        if not self.available:
            return {"power_source": "UNAVAILABLE", "power_unavailable_reason": self.reason,
                    "mean_watts": None, "peak_watts": None, "samples": 0}
        if not self.samples:
            return {"power_source": "powermetrics", "power_unavailable_reason": "no samples captured",
                    "mean_watts": None, "peak_watts": None, "samples": 0}
        return {"power_source": "powermetrics", "power_unavailable_reason": None,
                "mean_watts": round(statistics.fmean(self.samples), 3),
                "peak_watts": round(max(self.samples), 3),
                "samples": len(self.samples)}


# --------------------------------------------------------------------------- record


@dataclass
class RunRecord:
    schema_version: int
    runtime: str
    model: str
    workload_class: str
    batch: int
    prompt_tokens: int
    output_tokens: int
    prefix_state: str
    shared_prefix_tokens: int
    # measurement
    elapsed_s: float
    prefill_s: float
    decode_s: float
    tokens_prompt_total: int
    tokens_output_total: int
    tokens_billable_total: int
    # Accounting separation (goal 23). A shared prefix is computed ONCE and then
    # reused by every stream in the batch. Counting it per-stream inflates
    # throughput by the reuse factor and reports cache hits as inference.
    tokens_physical_total: int
    tokens_prefix_reused_total: int
    prefix_reuse_inflation: float
    ttft_ms: float | None
    itl_ms: float | None
    decode_tokens_per_s: float
    goodput_tokens_per_s: float
    physical_tokens_per_s: float
    # energy
    power_source: str
    power_unavailable_reason: str | None
    mean_watts: float | None
    peak_watts: float | None
    power_samples: int
    energy_joules: float | None
    joules_per_million_accepted_tokens: float | None
    # provenance
    hardware: str
    errors: list[str] = field(default_factory=list)
    started_at: str = ""


def hardware_label() -> str:
    try:
        cpu = subprocess.run(["sysctl", "-n", "machdep.cpu.brand_string"],
                             capture_output=True, text=True, timeout=5).stdout.strip()
    except Exception:
        cpu = platform.processor() or "unknown"
    try:
        mem = int(subprocess.run(["sysctl", "-n", "hw.memsize"],
                                 capture_output=True, text=True, timeout=5).stdout.strip())
        mem_gb = f"{mem // (1024**3)}GB"
    except Exception:
        mem_gb = "unknown"
    return f"{cpu} / {mem_gb}"


# --------------------------------------------------------------------------- runtime


class MLXRuntime:
    name = "mlx"

    def __init__(self, model_id: str):
        import mlx.core as mx
        from mlx_lm import load
        self.mx = mx
        self.model_id = model_id
        self.model, self.tok = load(model_id)

    def run(self, batch: int, prompt_tokens: int, output_tokens: int,
            shared_prefix_tokens: int = 0):
        """Returns (prefill_s, decode_s, ttft_ms, itl_ms, errors)."""
        from mlx_lm.models.cache import make_prompt_cache, KVCache
        mx = self.mx
        errors: list[str] = []

        unique = max(1, prompt_tokens - shared_prefix_tokens)
        t0 = time.perf_counter()
        try:
            if shared_prefix_tokens > 0:
                warm = make_prompt_cache(self.model)
                lg = self.model(mx.array([[1] * shared_prefix_tokens]), cache=warm)
                mx.eval(lg)
                cache = []
                for layer in warm:
                    n = layer.offset
                    kv = KVCache()
                    kv.keys = mx.broadcast_to(
                        layer.keys[:, :, :n, :],
                        (batch,) + layer.keys.shape[1:2] + (n, layer.keys.shape[3]))
                    kv.values = mx.broadcast_to(
                        layer.values[:, :, :n, :],
                        (batch,) + layer.values.shape[1:2] + (n, layer.values.shape[3]))
                    kv.offset = n
                    cache.append(kv)
                mx.eval([l.keys for l in cache] + [l.values for l in cache])
                lg = self.model(mx.array([[1] * unique] * batch), cache=cache)
            else:
                cache = make_prompt_cache(self.model)
                lg = self.model(mx.array([[1] * prompt_tokens] * batch), cache=cache)
            mx.eval(lg)
            y = mx.argmax(lg[:, -1, :], axis=-1)[:, None]
            mx.eval(y)
        except Exception as exc:  # noqa: BLE001 - recorded, not swallowed
            return 0.0, 0.0, None, None, [f"prefill: {type(exc).__name__}: {exc}"]
        prefill_s = time.perf_counter() - t0
        ttft_ms = prefill_s * 1000.0

        t1 = time.perf_counter()
        first_itl = None
        try:
            for i in range(output_tokens):
                step = time.perf_counter()
                lg = self.model(y, cache=cache)
                y = mx.argmax(lg[:, -1, :], axis=-1)[:, None]
                if i == 0:
                    mx.eval(y)
                    first_itl = (time.perf_counter() - step) * 1000.0
            mx.eval(y)
        except Exception as exc:  # noqa: BLE001
            errors.append(f"decode: {type(exc).__name__}: {exc}")
        decode_s = time.perf_counter() - t1
        itl_ms = (decode_s / output_tokens * 1000.0) if output_tokens else first_itl
        return prefill_s, decode_s, ttft_ms, itl_ms, errors


def measure(runtime, model_id: str, workload_class: str, batch: int,
            prompt_tokens: int, output_tokens: int, shared_prefix_tokens: int) -> RunRecord:
    prefix_state = "SHARED" if shared_prefix_tokens > 0 else ("COLD" if prompt_tokens else "NONE")
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    with PowerSampler() as power:
        wall0 = time.perf_counter()
        prefill_s, decode_s, ttft_ms, itl_ms, errors = runtime.run(
            batch, prompt_tokens, output_tokens, shared_prefix_tokens)
        elapsed = time.perf_counter() - wall0
    p = power.result()

    prompt_total = batch * prompt_tokens
    output_total = batch * output_tokens
    billable = prompt_total + output_total
    # Physically computed: the shared prefix once, each stream's unique prompt,
    # and every decoded token.
    unique_prompt = max(0, prompt_tokens - shared_prefix_tokens)
    physical = shared_prefix_tokens + batch * unique_prompt + output_total
    prefix_reused = billable - physical
    inflation = (billable / physical) if physical > 0 else 0.0
    decode_tps = (output_total / decode_s) if decode_s > 0 else 0.0
    goodput = (billable / elapsed) if elapsed > 0 and not errors else 0.0
    physical_tps = (physical / elapsed) if elapsed > 0 and not errors else 0.0

    joules = round(p["mean_watts"] * elapsed, 3) if p["mean_watts"] is not None else None
    j_per_m = None
    if joules is not None and physical > 0:
        # Energy is spent on physical work; dividing by billed tokens would make
        # a bigger cache hit look like better efficiency.
        j_per_m = round(joules / physical * 1_000_000, 3)

    return RunRecord(
        schema_version=SCHEMA_VERSION, runtime=runtime.name, model=model_id,
        workload_class=workload_class, batch=batch, prompt_tokens=prompt_tokens,
        output_tokens=output_tokens, prefix_state=prefix_state,
        shared_prefix_tokens=shared_prefix_tokens,
        elapsed_s=round(elapsed, 4), prefill_s=round(prefill_s, 4), decode_s=round(decode_s, 4),
        tokens_prompt_total=prompt_total, tokens_output_total=output_total,
        tokens_billable_total=billable,
        tokens_physical_total=physical, tokens_prefix_reused_total=prefix_reused,
        prefix_reuse_inflation=round(inflation, 3),
        ttft_ms=round(ttft_ms, 3) if ttft_ms is not None else None,
        itl_ms=round(itl_ms, 4) if itl_ms is not None else None,
        decode_tokens_per_s=round(decode_tps, 1),
        goodput_tokens_per_s=round(goodput, 1),
        physical_tokens_per_s=round(physical_tps, 1),
        power_source=p["power_source"], power_unavailable_reason=p["power_unavailable_reason"],
        mean_watts=p["mean_watts"], peak_watts=p["peak_watts"], power_samples=p["samples"],
        energy_joules=joules, joules_per_million_accepted_tokens=j_per_m,
        hardware=hardware_label(), errors=errors, started_at=started,
    )


# --------------------------------------------------------------------------- sweeps

SWEEPS = {
    # (workload_class, batch, prompt, output, shared_prefix)
    "quick": [
        ("INTERACTIVE", 1, 128, 64, 0),
        ("BATCH", 64, 128, 32, 0),
        ("SHARED_PREFIX_BATCH", 64, 192, 32, 128),
    ],
    "batch": [("BATCH", b, 128, 32, 0) for b in (1, 8, 32, 64, 128, 256)],
    "length": [("BATCH", 64, p, o, 0)
               for p in (64, 256, 1024) for o in (16, 64, 256)],
    "prefix": [("SHARED_PREFIX_BATCH", 128, 192, 32, s) for s in (0, 32, 64, 128, 160)],
    "interactive": [("INTERACTIVE", 1, p, 64, 0) for p in (32, 128, 512, 2048)],
}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--sweep", default="quick", choices=sorted(SWEEPS) + ["all"])
    ap.add_argument("--model", default="mlx-community/Llama-3.2-1B-Instruct-4bit")
    ap.add_argument("--out", default="evidence/bench/runs.jsonl")
    args = ap.parse_args()

    try:
        runtime = MLXRuntime(args.model)
    except ImportError as exc:
        print(json.dumps({"error": f"mlx not installed: {exc}"}), file=sys.stderr)
        return 2

    plans = []
    for name in (sorted(SWEEPS) if args.sweep == "all" else [args.sweep]):
        plans.extend(SWEEPS[name])

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("a") as fh:
        for wl, batch, prompt, output, shared in plans:
            rec = measure(runtime, args.model, wl, batch, prompt, output, shared)
            line = json.dumps(asdict(rec))
            fh.write(line + "\n")
            fh.flush()
            print(line, flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
