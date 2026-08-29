#!/usr/bin/env python3
"""Inference benchmark harness with measured energy (when available).

Records, for every run: runtime, model, batch, prompt length, output length,
prefix state, watts, tokens, errors and elapsed time -- one JSON object per run
on stdout, and a JSONL log.

Energy honesty:
  - Prefer unprivileged IOReport ``GPU Energy`` (macOS Apple Silicon). That is
    GPU-domain energy only — not package energy, not wall energy.
  - Fall back to root ``powermetrics`` Combined Power (CPU+GPU+ANE package).
  - When neither works every energy field is null and ``power_source`` says why.
  - Never estimate watts from a datasheet and present it as measurement.

    # measured GPU energy, no sudo (IOReport)
    python3 ops/scripts/bench-harness.py --sweep quick

    # first real energy authority receipt (bound; env gate required)
    MERC_WRITE_ENERGY_AUTHORITY=1 python3 ops/scripts/bench-harness.py --energy-authority \\
        --receipt evidence/perf/ioreport-gpu-energy-authority.json

    # package energy under sudo (powermetrics)
    sudo python3 ops/scripts/bench-harness.py --sweep quick --power-backend powermetrics

Workload classes:
  INTERACTIVE         batch 1, latency-shaped: TTFT and inter-token latency
  BATCH               large batch, independent prompts, throughput-shaped
  SHARED_PREFIX_BATCH large batch sharing a system prompt, prefill computed once
"""

from __future__ import annotations

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

ROOT = Path(__file__).resolve().parents[2]
SCHEMA_VERSION = 2

WORKLOAD_CLASSES = ("INTERACTIVE", "BATCH", "SHARED_PREFIX_BATCH")
PREFIX_STATES = ("NONE", "COLD", "SHARED")

# Boundary copy for the IOReport GPU channel. Must travel with every number.
IOREPORT_BOUNDARY = (
    "IOReport Energy Model channel 'GPU Energy' (AGX on-chip GPU domain only). "
    "Not package energy (CPU/DRAM/ANE excluded). Not wall-plug / PSU energy."
)
IOREPORT_MEASURES = "AGX GPU Energy counter (integrated mJ over the sample window)"
IOREPORT_EXCLUDES = (
    "CPU cores/clusters, DRAM, GPU SRAM, ANE, display, PCIe, package total, "
    "wall AC draw, power-supply inefficiency"
)

# Gate tracked evidence writes so harness verification never dirties evidence/.
# Set MERC_WRITE_ENERGY_AUTHORITY=1 to emit the bound receipt under --receipt.
WRITE_ENERGY_AUTHORITY_ENV = "MERC_WRITE_ENERGY_AUTHORITY"

# ASSUMED figure in src/control/pricing.go for apple_silicon_ultra. This lane does
# not edit that table; the receipt records what measurement implies about it.
ASSUMED_APPLE_SILICON_ULTRA_WATTS = 65.0


# --------------------------------------------------------------------------- power: IOReport GPU Energy (~40 lines of ctypes)


class IOReportGPUEnergySampler:
    """Sample IOReport ``GPU Energy`` without privilege.

    Creates an Energy Model subscription, snapshots the cumulative GPU Energy
    counter, and integrates deltas into joules for the measurement window.
    Intermediate watt samples are derived from consecutive counter deltas so the
    signal can be shown idle vs load — joules come from the counter, not from
    mean(watts)*time.
    """

    power_source = "ioreport_gpu_energy"
    energy_boundary = IOREPORT_BOUNDARY
    energy_measures = IOREPORT_MEASURES
    energy_excludes = IOREPORT_EXCLUDES

    def __init__(self, interval_ms: int = 200):
        self.interval_ms = interval_ms
        self.samples_w: list[float] = []
        self.sample_mj_deltas: list[float] = []
        self.reason: str | None = None
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self._sub = None
        self._channels = None
        self._cf = None
        self._ior = None
        self._t0 = 0.0
        self._mj0: float | None = None
        self._mj_last: float | None = None
        self._t_last = 0.0
        self._window_joules: float | None = None

        if platform.system() != "Darwin":
            self.reason = f"unsupported platform {platform.system()}"
            return
        try:
            self._open()
        except Exception as exc:  # noqa: BLE001 — surface as unavailable
            self.reason = f"IOReport open failed: {type(exc).__name__}: {exc}"
            self._close()

    @property
    def available(self) -> bool:
        return self.reason is None and self._sub is not None

    def _open(self) -> None:
        import ctypes
        from ctypes import c_void_p, c_char_p, c_uint64, c_int64, c_int, POINTER, byref

        cf = ctypes.CDLL(
            "/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation"
        )
        ior = ctypes.CDLL("/usr/lib/libIOReport.dylib")
        kUTF8 = 0x08000100

        cf.CFStringCreateWithCString.restype = c_void_p
        cf.CFStringCreateWithCString.argtypes = [c_void_p, c_char_p, c_uint64]
        cf.CFRelease.argtypes = [c_void_p]
        cf.CFDictionaryCreateMutableCopy.restype = c_void_p
        cf.CFDictionaryCreateMutableCopy.argtypes = [c_void_p, c_int64, c_void_p]
        cf.CFDictionaryGetCount.restype = c_int64
        cf.CFDictionaryGetCount.argtypes = [c_void_p]
        cf.CFDictionaryGetValue.restype = c_void_p
        cf.CFDictionaryGetValue.argtypes = [c_void_p, c_void_p]
        cf.CFArrayGetCount.restype = c_int64
        cf.CFArrayGetCount.argtypes = [c_void_p]
        cf.CFArrayGetValueAtIndex.restype = c_void_p
        cf.CFArrayGetValueAtIndex.argtypes = [c_void_p, c_int64]
        cf.CFStringGetCString.restype = c_int
        cf.CFStringGetCString.argtypes = [c_void_p, c_char_p, c_int64, c_uint64]

        ior.IOReportCopyChannelsInGroup.restype = c_void_p
        ior.IOReportCopyChannelsInGroup.argtypes = [
            c_void_p, c_void_p, c_uint64, c_uint64, c_uint64
        ]
        ior.IOReportCreateSubscription.restype = c_void_p
        ior.IOReportCreateSubscription.argtypes = [
            c_void_p, c_void_p, POINTER(c_void_p), c_uint64, c_void_p
        ]
        ior.IOReportCreateSamples.restype = c_void_p
        ior.IOReportCreateSamples.argtypes = [c_void_p, c_void_p, c_void_p]
        ior.IOReportSimpleGetIntegerValue.restype = c_int64
        ior.IOReportSimpleGetIntegerValue.argtypes = [c_void_p, c_int]
        ior.IOReportChannelGetChannelName.restype = c_void_p
        ior.IOReportChannelGetChannelName.argtypes = [c_void_p]
        ior.IOReportChannelGetUnitLabel.restype = c_void_p
        ior.IOReportChannelGetUnitLabel.argtypes = [c_void_p]

        def cfstr(s: str):
            return cf.CFStringCreateWithCString(None, s.encode(), kUTF8)

        def to_str(ref) -> str:
            if not ref:
                return ""
            buf = ctypes.create_string_buffer(256)
            ok = cf.CFStringGetCString(ref, buf, 256, kUTF8)
            return buf.value.decode() if ok else ""

        group = cfstr("Energy Model")
        ch = ior.IOReportCopyChannelsInGroup(group, None, 0, 0, 0)
        cf.CFRelease(group)
        if not ch:
            raise RuntimeError("IOReportCopyChannelsInGroup returned null")
        n = cf.CFDictionaryGetCount(ch)
        ch_m = cf.CFDictionaryCreateMutableCopy(None, n, ch)
        cf.CFRelease(ch)
        upd = c_void_p()
        sub = ior.IOReportCreateSubscription(None, ch_m, byref(upd), 0, None)
        if not sub:
            cf.CFRelease(ch_m)
            raise RuntimeError(
                "IOReportCreateSubscription failed "
                "(blocked by sandbox, or Energy Model unavailable)"
            )

        self._cf = cf
        self._ior = ior
        self._cfstr = cfstr
        self._to_str = to_str
        self._sub = sub
        self._channels = ch_m
        # Probe once so a missing GPU Energy channel fails open, not mid-run.
        if self._read_gpu_mj() is None:
            self._close()
            raise RuntimeError("no 'GPU Energy' channel in Energy Model group")

    def _read_gpu_mj(self) -> float | None:
        """Cumulative GPU Energy in millijoules (sum of matching channels)."""
        import ctypes
        from ctypes import c_void_p

        cf, ior = self._cf, self._ior
        sample = ior.IOReportCreateSamples(self._sub, self._channels, None)
        if not sample:
            return None
        key = self._cfstr("IOReportChannels")
        arr = cf.CFDictionaryGetValue(sample, key)
        cf.CFRelease(key)
        total = 0.0
        found = False
        if arr:
            for i in range(cf.CFArrayGetCount(arr)):
                item = cf.CFArrayGetValueAtIndex(arr, i)
                name = self._to_str(ior.IOReportChannelGetChannelName(item))
                # Ultra: DIE_N_GPU Energy; base/pro/max: GPU Energy.
                base = name
                if name.startswith("DIE_") and "_" in name[4:]:
                    base = name.split("_", 2)[-1]
                if "GPU Energy" not in base:
                    continue
                unit = self._to_str(ior.IOReportChannelGetUnitLabel(item))
                raw = ior.IOReportSimpleGetIntegerValue(item, 0)
                if unit == "nJ":
                    mj = raw / 1_000_000.0
                elif unit in ("uJ", "µJ"):
                    mj = raw / 1_000.0
                elif unit == "mJ":
                    mj = float(raw)
                elif unit == "J":
                    mj = float(raw) * 1000.0
                else:
                    # Unknown unit: refuse rather than invent a scale.
                    cf.CFRelease(sample)
                    return None
                total += mj
                found = True
        cf.CFRelease(sample)
        return total if found else None

    def _close(self) -> None:
        if self._cf is not None and self._channels is not None:
            try:
                self._cf.CFRelease(self._channels)
            except Exception:  # noqa: BLE001
                pass
        # Subscription is an IOReport ref; CFRelease is accepted by the framework.
        if self._cf is not None and self._sub is not None:
            try:
                self._cf.CFRelease(self._sub)
            except Exception:  # noqa: BLE001
                pass
        self._sub = None
        self._channels = None

    def _loop(self) -> None:
        while not self._stop.wait(self.interval_ms / 1000.0):
            mj = self._read_gpu_mj()
            if mj is None or self._mj_last is None:
                continue
            now = time.perf_counter()
            dt = now - self._t_last
            if dt <= 0:
                continue
            dmj = mj - self._mj_last
            if dmj < 0:
                # Counter wrap / reset — skip interval, re-anchor.
                self._mj_last = mj
                self._t_last = now
                continue
            self.sample_mj_deltas.append(dmj)
            self.samples_w.append((dmj / 1000.0) / dt)
            self._mj_last = mj
            self._t_last = now

    def __enter__(self):
        if not self.available:
            return self
        self._mj0 = self._read_gpu_mj()
        self._mj_last = self._mj0
        self._t0 = time.perf_counter()
        self._t_last = self._t0
        self._stop.clear()
        self._thread = threading.Thread(target=self._loop, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *exc):
        if self._thread is not None:
            self._stop.set()
            self._thread.join(timeout=2)
        if self.available and self._mj0 is not None:
            mj1 = self._read_gpu_mj()
            if mj1 is not None and mj1 >= self._mj0:
                self._window_joules = (mj1 - self._mj0) / 1000.0
        self._close()
        return False

    def result(self) -> dict:
        if not self.available and self.reason:
            return {
                "power_source": "UNAVAILABLE",
                "power_unavailable_reason": self.reason,
                "mean_watts": None,
                "peak_watts": None,
                "samples": 0,
                "energy_joules": None,
                "energy_boundary": None,
                "energy_measures": None,
                "energy_excludes": None,
                "integration": None,
            }
        joules = self._window_joules
        mean_w = (
            round(statistics.fmean(self.samples_w), 3) if self.samples_w else None
        )
        peak_w = round(max(self.samples_w), 3) if self.samples_w else None
        # If the sampler thread got nothing but the counter delta exists, derive
        # mean watts from integrated joules / wall time.
        if mean_w is None and joules is not None:
            elapsed = max(1e-9, time.perf_counter() - self._t0) if self._t0 else None
            if elapsed:
                mean_w = round(joules / elapsed, 3)
                peak_w = mean_w
        return {
            "power_source": self.power_source,
            "power_unavailable_reason": None,
            "mean_watts": mean_w,
            "peak_watts": peak_w,
            "samples": len(self.samples_w),
            "energy_joules": round(joules, 6) if joules is not None else None,
            "energy_boundary": self.energy_boundary,
            "energy_measures": self.energy_measures,
            "energy_excludes": self.energy_excludes,
            "integration": "IOReport cumulative GPU Energy counter delta (mJ→J)",
            "raw_watts_samples": [round(w, 4) for w in self.samples_w],
        }


class PowerMetricsSampler:
    """Samples package power via powermetrics. Null-safe when not root."""

    power_source = "powermetrics"
    energy_boundary = (
        "powermetrics Combined Power (CPU + GPU + ANE) as reported by the SMC/"
        "powermetrics samplers. Package-side, not wall-plug."
    )
    energy_measures = "Combined Power (CPU + GPU + ANE) instantaneous mW samples"
    energy_excludes = "wall AC draw, PSU inefficiency; not a per-domain split"

    def __init__(self, interval_ms: int = 200):
        self.interval_ms = interval_ms
        self.samples: list[float] = []
        self._proc = None
        self._thread = None
        self._stop = threading.Event()
        self.reason = None
        self._t0 = 0.0

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
            if "Combined Power" in line and "mW" in line:
                try:
                    self.samples.append(
                        float(line.rsplit(":", 1)[1].strip().split()[0]) / 1000.0
                    )
                except (ValueError, IndexError):
                    pass

    def __enter__(self):
        if not self.available:
            return self
        self._t0 = time.perf_counter()
        self._proc = subprocess.Popen(
            [
                "powermetrics",
                "--samplers",
                "cpu_power,gpu_power",
                "-i",
                str(self.interval_ms),
                "-n",
                "0",
            ],
            stdout=subprocess.PIPE,
            stderr=subprocess.DEVNULL,
            text=True,
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
            return {
                "power_source": "UNAVAILABLE",
                "power_unavailable_reason": self.reason,
                "mean_watts": None,
                "peak_watts": None,
                "samples": 0,
                "energy_joules": None,
                "energy_boundary": None,
                "energy_measures": None,
                "energy_excludes": None,
                "integration": None,
            }
        if not self.samples:
            return {
                "power_source": self.power_source,
                "power_unavailable_reason": "no samples captured",
                "mean_watts": None,
                "peak_watts": None,
                "samples": 0,
                "energy_joules": None,
                "energy_boundary": self.energy_boundary,
                "energy_measures": self.energy_measures,
                "energy_excludes": self.energy_excludes,
                "integration": None,
            }
        mean_w = round(statistics.fmean(self.samples), 3)
        elapsed = max(1e-9, time.perf_counter() - self._t0)
        return {
            "power_source": self.power_source,
            "power_unavailable_reason": None,
            "mean_watts": mean_w,
            "peak_watts": round(max(self.samples), 3),
            "samples": len(self.samples),
            "energy_joules": round(mean_w * elapsed, 6),
            "energy_boundary": self.energy_boundary,
            "energy_measures": self.energy_measures,
            "energy_excludes": self.energy_excludes,
            "integration": "mean(powermetrics Combined Power W) * wall_s",
            "raw_watts_samples": [round(w, 4) for w in self.samples],
        }


class _DisabledSampler:
    """Explicit no-energy backend (``--power-backend none``)."""

    reason = "disabled by --power-backend none"

    @property
    def available(self) -> bool:
        return False

    def __enter__(self):
        return self

    def __exit__(self, *a):
        return False

    def result(self) -> dict:
        return {
            "power_source": "UNAVAILABLE",
            "power_unavailable_reason": self.reason,
            "mean_watts": None,
            "peak_watts": None,
            "samples": 0,
            "energy_joules": None,
            "energy_boundary": None,
            "energy_measures": None,
            "energy_excludes": None,
            "integration": None,
        }


def open_power_sampler(backend: str = "auto", interval_ms: int = 200):
    """Select energy backend. ``auto``: IOReport GPU, else powermetrics if root."""
    if backend == "none":
        return _DisabledSampler()

    if backend in ("auto", "ioreport"):
        s = IOReportGPUEnergySampler(interval_ms=interval_ms)
        if s.available:
            return s
        if backend == "ioreport":
            return s  # unavailable, carries reason
        ioreport_reason = s.reason
    else:
        ioreport_reason = None

    if backend in ("auto", "powermetrics"):
        s = PowerMetricsSampler(interval_ms=interval_ms)
        if s.available:
            return s
        if backend == "powermetrics":
            return s
        # auto and both failed
        s.reason = (
            f"IOReport unavailable ({ioreport_reason}); "
            f"powermetrics unavailable ({s.reason})"
        )
        return s

    raise ValueError(f"unknown power backend: {backend}")


# Back-compat alias used by older call sites / tests.
PowerSampler = PowerMetricsSampler


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
    energy_boundary: str | None = None
    energy_measures: str | None = None
    energy_excludes: str | None = None
    energy_integration: str | None = None
    # provenance
    hardware: str = ""
    errors: list[str] = field(default_factory=list)
    started_at: str = ""


def hardware_label() -> str:
    try:
        cpu = subprocess.run(
            ["sysctl", "-n", "machdep.cpu.brand_string"],
            capture_output=True,
            text=True,
            timeout=5,
        ).stdout.strip()
    except Exception:
        cpu = platform.processor() or "unknown"
    try:
        mem = int(
            subprocess.run(
                ["sysctl", "-n", "hw.memsize"],
                capture_output=True,
                text=True,
                timeout=5,
            ).stdout.strip()
        )
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

    def run(
        self,
        batch: int,
        prompt_tokens: int,
        output_tokens: int,
        shared_prefix_tokens: int = 0,
    ):
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
                        (batch,) + layer.keys.shape[1:2] + (n, layer.keys.shape[3]),
                    )
                    kv.values = mx.broadcast_to(
                        layer.values[:, :, :n, :],
                        (batch,) + layer.values.shape[1:2] + (n, layer.values.shape[3]),
                    )
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


def measure(
    runtime,
    model_id: str,
    workload_class: str,
    batch: int,
    prompt_tokens: int,
    output_tokens: int,
    shared_prefix_tokens: int,
    power_backend: str = "auto",
    interval_ms: int = 200,
) -> RunRecord:
    prefix_state = (
        "SHARED"
        if shared_prefix_tokens > 0
        else ("COLD" if prompt_tokens else "NONE")
    )
    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    with open_power_sampler(power_backend, interval_ms=interval_ms) as power:
        wall0 = time.perf_counter()
        prefill_s, decode_s, ttft_ms, itl_ms, errors = runtime.run(
            batch, prompt_tokens, output_tokens, shared_prefix_tokens
        )
        elapsed = time.perf_counter() - wall0
    p = power.result()

    prompt_total = batch * prompt_tokens
    output_total = batch * output_tokens
    billable = prompt_total + output_total
    unique_prompt = max(0, prompt_tokens - shared_prefix_tokens)
    physical = shared_prefix_tokens + batch * unique_prompt + output_total
    prefix_reused = billable - physical
    inflation = (billable / physical) if physical > 0 else 0.0
    decode_tps = (output_total / decode_s) if decode_s > 0 else 0.0
    goodput = (billable / elapsed) if elapsed > 0 and not errors else 0.0
    physical_tps = (physical / elapsed) if elapsed > 0 and not errors else 0.0

    # Prefer counter-integrated joules when the backend provides them.
    joules = p.get("energy_joules")
    if joules is None and p.get("mean_watts") is not None:
        joules = round(p["mean_watts"] * elapsed, 6)
    j_per_m = None
    if joules is not None and physical > 0:
        # Energy is spent on physical work; dividing by billed tokens would make
        # a bigger cache hit look like better efficiency.
        j_per_m = round(joules / physical * 1_000_000, 3)

    return RunRecord(
        schema_version=SCHEMA_VERSION,
        runtime=runtime.name,
        model=model_id,
        workload_class=workload_class,
        batch=batch,
        prompt_tokens=prompt_tokens,
        output_tokens=output_tokens,
        prefix_state=prefix_state,
        shared_prefix_tokens=shared_prefix_tokens,
        elapsed_s=round(elapsed, 4),
        prefill_s=round(prefill_s, 4),
        decode_s=round(decode_s, 4),
        tokens_prompt_total=prompt_total,
        tokens_output_total=output_total,
        tokens_billable_total=billable,
        tokens_physical_total=physical,
        tokens_prefix_reused_total=prefix_reused,
        prefix_reuse_inflation=round(inflation, 3),
        ttft_ms=round(ttft_ms, 3) if ttft_ms is not None else None,
        itl_ms=round(itl_ms, 4) if itl_ms is not None else None,
        decode_tokens_per_s=round(decode_tps, 1),
        goodput_tokens_per_s=round(goodput, 1),
        physical_tokens_per_s=round(physical_tps, 1),
        power_source=p["power_source"],
        power_unavailable_reason=p["power_unavailable_reason"],
        mean_watts=p["mean_watts"],
        peak_watts=p["peak_watts"],
        power_samples=p["samples"],
        energy_joules=joules,
        joules_per_million_accepted_tokens=j_per_m,
        energy_boundary=p.get("energy_boundary"),
        energy_measures=p.get("energy_measures"),
        energy_excludes=p.get("energy_excludes"),
        energy_integration=p.get("integration"),
        hardware=hardware_label(),
        errors=errors,
        started_at=started,
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
    "length": [
        ("BATCH", 64, p, o, 0) for p in (64, 256, 1024) for o in (16, 64, 256)
    ],
    "prefix": [
        ("SHARED_PREFIX_BATCH", 128, 192, 32, s) for s in (0, 32, 64, 128, 160)
    ],
    "interactive": [("INTERACTIVE", 1, p, 64, 0) for p in (32, 128, 512, 2048)],
}


# --------------------------------------------------------------------------- energy authority (bound receipt)


def _default_gguf() -> str | None:
    candidates = [
        Path.home()
        / ".cache/huggingface/hub/models--unsloth--Llama-3.2-1B-Instruct-GGUF"
        / "snapshots",
    ]
    for base in candidates:
        if not base.is_dir():
            continue
        for gguf in base.rglob("*.gguf"):
            if "Q4_K_M" in gguf.name or "Q4_K" in gguf.name:
                return str(gguf)
        found = list(base.rglob("*.gguf"))
        if found:
            return str(found[0])
    return None


def run_llama_cli_workload(
    model_path: str,
    n_predict: int = 64,
    prompt: str = "The capital of France is",
    ngl: int = 99,
) -> dict:
    """Drive llama-cli on Metal; return verified outcome counts + wall time.

    Flags are pinned for non-interactive completion on current llama.cpp
    (b9430+): ``--single-turn`` exits after one turn; ``--ignore-eos`` forces
    exactly ``n_predict`` completion tokens so the outcome count is known;
    ``--simple-io`` avoids TTY assumptions when captured by a subprocess.
    """
    import re

    llama = shutil.which("llama-cli") or shutil.which("llama")
    if not llama:
        raise RuntimeError("llama-cli not found on PATH")
    if not Path(model_path).is_file():
        raise RuntimeError(f"model not found: {model_path}")
    cmd = [
        llama,
        "-m",
        model_path,
        "-p",
        prompt,
        "-n",
        str(n_predict),
        "-ngl",
        str(ngl),
        "--temp",
        "0",
        "--single-turn",
        "--simple-io",
        "--ignore-eos",
        "--perf",
    ]
    t0 = time.perf_counter()
    proc = subprocess.run(
        cmd, capture_output=True, text=True, timeout=600, check=False
    )
    elapsed = time.perf_counter() - t0
    text = (proc.stdout or "") + "\n" + (proc.stderr or "")

    def grab(pat):
        m = re.search(pat, text)
        return float(m.group(1)) if m else None

    # Classic llama.cpp timing table (older builds).
    n_prompt = grab(r"prompt eval time\s*=.*?/\s*(\d+)\s*tokens")
    n_eval = grab(r"eval time\s*=.*?/\s*(\d+)\s*runs")
    # With --ignore-eos and -n N the runtime produces exactly N completion
    # tokens when exit_code==0. Prefer that over missing timing lines.
    if proc.returncode == 0:
        outcomes = int(n_eval) if n_eval else int(n_predict)
    else:
        outcomes = int(n_eval) if n_eval else 0
    prompt_tok = int(n_prompt) if n_prompt else 0
    gen_tps = grab(r"Generation:\s*([0-9.]+)\s*t/s")
    prompt_tps = grab(r"Prompt:\s*([0-9.]+)\s*t/s")
    return {
        "runtime": "llama_cpp_metal",
        "model": model_path,
        "exit_code": proc.returncode,
        "elapsed_s": round(elapsed, 4),
        "verified_outcomes": outcomes,  # decoded tokens (completion tokens)
        "prompt_tokens": prompt_tok,
        "n_predict_requested": n_predict,
        "reported_generation_tps": gen_tps,
        "reported_prompt_tps": prompt_tps,
        "stdout_tail": "\n".join(text.splitlines()[-30:]),
        "cmd": cmd,
        "outcome_count_basis": (
            "eval_time_runs"
            if n_eval
            else (
                "n_predict_with_ignore_eos"
                if proc.returncode == 0
                else "unavailable"
            )
        ),
    }


def sample_window(label: str, duration_s: float, interval_ms: int = 200) -> dict:
    """Idle (or external-load) energy window with raw watt samples."""
    with open_power_sampler("ioreport", interval_ms=interval_ms) as power:
        time.sleep(duration_s)
    r = power.result()
    r["label"] = label
    r["window_s"] = duration_s
    return r


def run_energy_authority(args: argparse.Namespace) -> int:
    """End-to-end: measure GPU joules for a known number of verified outcomes."""
    sys.path.insert(0, str(ROOT / "ops/scripts"))
    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        default_bound_identity,
        sha256_file,
        slot_value,
        write_bound_evidence,
    )

    interval_ms = args.interval_ms
    # 1) Probe IOReport availability before doing heavy work.
    probe = open_power_sampler("ioreport", interval_ms=interval_ms)
    if not probe.available:
        print(
            json.dumps(
                {
                    "error": "IOReport GPU Energy unavailable",
                    "reason": probe.reason,
                    "hint": (
                        "IOReportCreateSubscription must succeed without sudo. "
                        "If this process is seatbelt-sandboxed, re-run under the "
                        "unsandboxed gate profile. Do not use powermetrics sudo "
                        "as a substitute for this receipt."
                    ),
                },
                indent=2,
            ),
            file=sys.stderr,
        )
        return 2

    # 2) Idle window — raw samples must show a low baseline.
    print("energy-authority: idle window…", flush=True)
    idle = sample_window("idle", args.idle_s, interval_ms=interval_ms)

    # 3) Load window — drive a real Metal workload under the sampler.
    model_path = args.gguf or _default_gguf()
    workload: dict
    load: dict
    if args.workload == "llama-cli":
        if not model_path:
            print("energy-authority: no GGUF model found", file=sys.stderr)
            return 2
        print(f"energy-authority: load via llama-cli ({model_path})…", flush=True)
        with open_power_sampler("ioreport", interval_ms=interval_ms) as power:
            workload = run_llama_cli_workload(
                model_path, n_predict=args.n_predict, prompt=args.prompt
            )
        load = power.result()
        load["label"] = "load_llama_cli"
        verified = int(workload["verified_outcomes"])
        physical_tokens = int(workload.get("prompt_tokens") or 0) + verified
    else:
        # Default: MLX INTERACTIVE measure path (same as --sweep interactive row).
        print("energy-authority: load via MLX…", flush=True)
        try:
            runtime = MLXRuntime(args.model)
        except Exception as exc:  # noqa: BLE001
            print(f"energy-authority: MLX unavailable: {exc}", file=sys.stderr)
            return 2
        rec = measure(
            runtime,
            args.model,
            "INTERACTIVE",
            batch=1,
            prompt_tokens=args.prompt_tokens,
            output_tokens=args.n_predict,
            shared_prefix_tokens=0,
            power_backend="ioreport",
            interval_ms=interval_ms,
        )
        workload = asdict(rec)
        load = {
            "label": "load_mlx",
            "power_source": rec.power_source,
            "mean_watts": rec.mean_watts,
            "peak_watts": rec.peak_watts,
            "samples": rec.power_samples,
            "energy_joules": rec.energy_joules,
            "energy_boundary": rec.energy_boundary,
            "energy_measures": rec.energy_measures,
            "energy_excludes": rec.energy_excludes,
            "integration": rec.energy_integration,
            "raw_watts_samples": [],  # on RunRecord path samples are aggregated
            "window_s": rec.elapsed_s,
        }
        verified = rec.tokens_output_total  # completion tokens produced
        physical_tokens = rec.tokens_physical_total
        model_path = args.model

    load_j = load.get("energy_joules")
    if load_j is None:
        print(
            json.dumps(
                {"error": "load window produced no energy_joules", "load": load},
                indent=2,
            ),
            file=sys.stderr,
        )
        return 2
    if verified <= 0:
        print(
            json.dumps(
                {"error": "no verified outcomes from workload", "workload": workload},
                indent=2,
            ),
            file=sys.stderr,
        )
        return 2

    j_per_outcome = load_j / verified
    j_per_physical = load_j / physical_tokens if physical_tokens > 0 else None

    model_digest = ""
    model_na = "no local weight file to hash (HF id / remote)"
    if model_path and Path(model_path).is_file():
        model_digest = sha256_file(model_path)
        model_na = ""

    payload = {
        "schema_version": 1,
        "kind": "measured_energy_authority",
        "title": "First measured GPU energy authority (IOReport, unprivileged)",
        "measured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "host": {
            "hardware": hardware_label(),
            "platform": platform.platform(),
            "uid": os.getuid(),
            "euid": os.geteuid(),
        },
        "measurement": {
            "backend": "ioreport_gpu_energy",
            "privilege": "none (uid-level IOReport subscription; no sudo)",
            "channel": "Energy Model / GPU Energy",
            "sampling_interval_ms": interval_ms,
            "integration": load.get("integration")
            or "IOReport cumulative GPU Energy counter delta (mJ→J)",
            "boundary": IOREPORT_BOUNDARY,
            "measures": IOREPORT_MEASURES,
            "excludes": IOREPORT_EXCLUDES,
            "error_notes": [
                f"Sampling interval {interval_ms} ms; sub-interval power spikes are averaged into each sample.",
                "Joules for the load window are the cumulative counter delta, not mean(watts)*time.",
                "Idle and load windows are sequential, not simultaneous — ambient GPU work from other processes is included.",
                "GPU-alone energy is a lower bound on package energy and is not comparable to wall-plug joules.",
                "The load window covers the entire llama-cli process, including model load / Metal graph compile, not decode-only steady state. Mean watts is therefore diluted by cold-start; peak watts and raw samples show the under-load signal.",
                "What this number excludes: CPU cores/clusters, DRAM, ANE, display, package total, wall AC, PSU inefficiency — typically a large fraction of machine draw under mixed load.",
            ],
        },
        "idle_window": {
            "window_s": idle.get("window_s"),
            "energy_joules": idle.get("energy_joules"),
            "mean_watts": idle.get("mean_watts"),
            "peak_watts": idle.get("peak_watts"),
            "samples": idle.get("samples"),
            "raw_watts_samples": idle.get("raw_watts_samples") or [],
        },
        "load_window": {
            "window_s": load.get("window_s") or workload.get("elapsed_s"),
            "energy_joules": load_j,
            "mean_watts": load.get("mean_watts"),
            "peak_watts": load.get("peak_watts"),
            "samples": load.get("samples"),
            "raw_watts_samples": load.get("raw_watts_samples") or [],
        },
        "workload": {
            "kind": args.workload,
            "runtime": workload.get("runtime"),
            "model": model_path,
            "verified_outcomes": verified,
            "verified_outcome_unit": "completion_tokens",
            "physical_tokens": physical_tokens,
            "detail": {
                k: workload[k]
                for k in workload
                if k
                not in {
                    "stdout_tail",
                    # keep receipt bounded; full run record fields live above
                }
            },
        },
        "joules_per_verified_outcome": round(j_per_outcome, 9),
        "joules_per_physical_token": (
            round(j_per_physical, 9) if j_per_physical is not None else None
        ),
        "signal_check": {
            "idle_mean_watts": idle.get("mean_watts"),
            "load_mean_watts": load.get("mean_watts"),
            "load_over_idle_ratio": (
                round(load["mean_watts"] / idle["mean_watts"], 3)
                if idle.get("mean_watts") and load.get("mean_watts") and idle["mean_watts"] > 0
                else None
            ),
            "note": "ratio must move under load; a flat signal is not authority",
        },
        "not_production_pricing_truth": (
            "This receipt is measured GPU-domain energy for one host and one "
            "workload. It does not replace sustainedWattsByHWClass and must not "
            "be written into src/control/pricing.go without a separate pricing lane."
        ),
        "vs_assumed_sustained_watts": {
            "hw_class": "apple_silicon_ultra",
            "assumed_watts": ASSUMED_APPLE_SILICON_ULTRA_WATTS,
            "assumed_kind": "ASSUMED",
            "assumed_source": "src/control/pricing.go sustainedWattsByHWClass (not edited by this lane)",
            "measured_load_mean_gpu_watts": load.get("mean_watts"),
            "measured_idle_mean_gpu_watts": idle.get("mean_watts"),
            "delta_load_minus_assumed_watts": (
                round(load["mean_watts"] - ASSUMED_APPLE_SILICON_ULTRA_WATTS, 3)
                if load.get("mean_watts") is not None
                else None
            ),
            "ratio_load_over_assumed": (
                round(load["mean_watts"] / ASSUMED_APPLE_SILICON_ULTRA_WATTS, 3)
                if load.get("mean_watts") and ASSUMED_APPLE_SILICON_ULTRA_WATTS > 0
                else None
            ),
            "comparison_caveat": (
                "The 65 W ASSUMED figure is whole-package sustained draw under "
                "inference-shaped load. This measurement is GPU-domain only, so "
                "a GPU-alone mean above 65 W already proves the package "
                "assumption understates reality; a GPU-alone mean below 65 W is "
                "not evidence the package assumption is high, because CPU/DRAM/"
                "ANE/package overhead are excluded here."
            ),
            "viability_gate_implication": (
                "Supplier viability and contribution margins price energy cost "
                "from sustainedWattsByHWClass. Understating watts understates "
                "energy cost and can make an unprofitable cell look viable. "
                "This receipt is the bound GPU-energy authority; promoting it "
                "into the package table is a separate pricing-lane decision."
            ),
        },
    }

    # Always print the measurement summary so a dry run is still useful.
    summary = {
        "binding_status": None,
        "receipt": None,
        "idle_mean_watts": idle.get("mean_watts"),
        "load_mean_watts": load.get("mean_watts"),
        "load_energy_joules": load_j,
        "verified_outcomes": verified,
        "joules_per_verified_outcome": round(j_per_outcome, 9),
        "boundary": IOREPORT_BOUNDARY,
        "vs_assumed_65W": payload["vs_assumed_sustained_watts"],
    }

    if os.environ.get(WRITE_ENERGY_AUTHORITY_ENV) != "1":
        summary["binding_status"] = "NOT_WRITTEN"
        summary["write_gate"] = (
            f"set {WRITE_ENERGY_AUTHORITY_ENV}=1 to write the bound receipt "
            f"to {args.receipt}; without it the harness measures but does not "
            "dirty tracked evidence/"
        )
        print(
            f"energy-authority: measurement ok; not writing "
            f"(export {WRITE_ENERGY_AUTHORITY_ENV}=1 to write)",
            flush=True,
        )
        print(json.dumps(summary, indent=2), flush=True)
        # Full payload on stdout-adjacent stream for inspection without a file.
        print(
            json.dumps({"payload_preview": payload}, indent=2),
            flush=True,
        )
        return 0

    receipt_path = Path(args.receipt)
    harness = "ops/scripts/bench-harness.py"
    build_path = Path(__file__).resolve()
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=harness,
            build_binary_path=build_path,
            exact_config=(
                f"workload={args.workload} interval_ms={interval_ms} "
                f"idle_s={args.idle_s} n_predict={args.n_predict} "
                f"backend=ioreport_gpu_energy"
            ),
            raw_samples=(
                f"idle_samples={idle.get('samples')} "
                f"load_samples={load.get('samples')} "
                f"verified_outcomes={verified}"
            ),
            model_na=model_na or "no model weights in this measurement",
            image_na="no container image in this measurement",
            corpus_na="no external corpus; synthetic/local prompt only",
        )
        if model_digest:
            identity["model_artifact_digest"] = slot_value(model_digest)
        write_bound_evidence(
            path=receipt_path,
            payload=payload,
            identity=identity,
            repo_root=ROOT,
            build_binary_path=build_path,
        )
    except EvidenceBindingError as exc:
        print(f"energy-authority: REFUSED by binding writer: {exc}", file=sys.stderr)
        return 2

    summary["binding_status"] = "BOUND"
    summary["receipt"] = str(receipt_path)
    print(f"energy-authority: wrote {receipt_path}", flush=True)
    print(json.dumps(summary, indent=2), flush=True)
    return 0


# --------------------------------------------------------------------------- main


def main() -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument("--sweep", default="quick", choices=sorted(SWEEPS) + ["all"])
    ap.add_argument("--model", default="mlx-community/Llama-3.2-1B-Instruct-4bit")
    ap.add_argument("--out", default="evidence/bench/runs.jsonl")
    ap.add_argument(
        "--power-backend",
        default="auto",
        choices=("auto", "ioreport", "powermetrics", "none"),
        help="auto: IOReport GPU Energy, else powermetrics if root",
    )
    ap.add_argument("--interval-ms", type=int, default=200)
    # Energy authority receipt mode
    ap.add_argument(
        "--energy-authority",
        action="store_true",
        help="measure GPU joules for a real workload and write a BOUND receipt",
    )
    ap.add_argument(
        "--receipt",
        default="evidence/perf/ioreport-gpu-energy-authority.json",
    )
    ap.add_argument(
        "--workload",
        default="llama-cli",
        choices=("llama-cli", "mlx"),
        help="real workload driven under the energy sampler",
    )
    ap.add_argument("--gguf", default=None, help="path to GGUF for llama-cli workload")
    ap.add_argument("--n-predict", type=int, default=512)
    ap.add_argument("--prompt-tokens", type=int, default=128)
    ap.add_argument("--idle-s", type=float, default=3.0)
    ap.add_argument(
        "--prompt",
        default=(
            "Write a detailed technical essay about measuring GPU energy on "
            "Apple Silicon using IOReport. Cover channels, units, sampling, "
            "and the difference between GPU-domain and wall-plug energy.\n\n"
        ),
    )
    args = ap.parse_args()

    if args.energy_authority:
        return run_energy_authority(args)

    try:
        runtime = MLXRuntime(args.model)
    except ImportError as exc:
        print(json.dumps({"error": f"mlx not installed: {exc}"}), file=sys.stderr)
        return 2

    plans = []
    for name in sorted(SWEEPS) if args.sweep == "all" else [args.sweep]:
        plans.extend(SWEEPS[name])

    backend = args.power_backend
    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("a") as fh:
        for wl, batch, prompt, output, shared in plans:
            rec = measure(
                runtime,
                args.model,
                wl,
                batch,
                prompt,
                output,
                shared,
                power_backend=backend,
                interval_ms=args.interval_ms,
            )
            line = json.dumps(asdict(rec))
            fh.write(line + "\n")
            fh.flush()
            print(line, flush=True)
    # Refresh the sidecar after every append so re-runs do not leave a missing
    # or stale UNBOUND stamp on a file whose rows just changed.
    sys.path.insert(0, str(ROOT / "ops/scripts"))
    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        write_bound_jsonl_sidecar,
    )

    try:
        write_bound_jsonl_sidecar(
            out,
            harness="ops/scripts/bench-harness.py",
            repo_root=ROOT,
            build_binary_path=Path(__file__).resolve(),
            exact_config=f"sweep={args.sweep} model={args.model} power_backend={backend}",
            raw_samples=f"JSONL rows appended at {out.as_posix()}",
            model_na="model id recorded per JSONL row; no single artifact digest",
            image_na="no container image in this measurement",
            corpus_na="no external corpus",
        )
    except EvidenceBindingError as exc:
        print(f"bench-harness: REFUSED binding sidecar: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
