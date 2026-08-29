#!/usr/bin/env python3
"""Energy *deltas* nobody has measured: idle scale-to-zero and warm vs cold.

Builds on the GPU-domain IOReport authority (bench-harness.py sampler). Produces
a BOUND receipt when MERC_WRITE_ENERGY_DELTAS=1 is set.

Measures (all AGX GPU Energy domain, unprivileged):
  1. Unloaded idle GPU draw (no model process).
  2. Resident idle GPU draw (llama-server holds the model; no requests).
  3. Cold placement: llama-cli cold-start + generate N tokens (load included).
  4. Warm placement: HTTP completion against already-loaded llama-server.

Derives:
  - Idle-power-avoided curve vs duty cycle (scale-to-zero vs always-resident).
  - Warm vs cold energy (load included in cold).
  - Telemetry-coverage ceiling on this hardware.
  - Named baseline (or honest absence) for the −20–40% J/outcome target.

Never presents GPU-domain numbers as wall-plug. Never fabricates CUDA.
Does not edit src/control/pricing.go.
"""

from __future__ import annotations

import argparse
import json
import os
import platform
import signal
import socket
import statistics
import subprocess
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "ops/scripts"))

# Import the shared IOReport sampler (and helpers) from bench-harness without
# reimplementing the ctypes subscription path.
import importlib.util

_bh_path = ROOT / "ops/scripts" / "bench-harness.py"
_spec = importlib.util.spec_from_file_location("merc_bench_harness", _bh_path)
_bh = importlib.util.module_from_spec(_spec)
sys.modules["merc_bench_harness"] = _bh  # required for @dataclass on 3.14
assert _spec.loader is not None
_spec.loader.exec_module(_bh)

IOREPORT_BOUNDARY = _bh.IOREPORT_BOUNDARY
IOREPORT_MEASURES = _bh.IOREPORT_MEASURES
IOREPORT_EXCLUDES = _bh.IOREPORT_EXCLUDES
open_power_sampler = _bh.open_power_sampler
sample_window = _bh.sample_window
run_llama_cli_workload = _bh.run_llama_cli_workload
hardware_label = _bh.hardware_label
_default_gguf = _bh._default_gguf

WRITE_ENV = "MERC_WRITE_ENERGY_DELTAS"

DEFAULT_PROMPT = (
    "Write a short technical paragraph about measuring GPU energy on "
    "Apple Silicon using IOReport.\n\n"
)

# Realistic duty-cycle anchors for commentary (fraction of wall time under load).
DUTY_CYCLE_ANCHORS = {
    "batch_overnight": 0.70,
    "interactive_business_hours": 0.25,
    "bursty_canary": 0.05,
    "sparse_dev": 0.01,
}


def free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def wait_http(url: str, timeout_s: float = 120.0) -> None:
    deadline = time.time() + timeout_s
    last_err = None
    while time.time() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as resp:
                if resp.status < 500:
                    return
        except Exception as exc:  # noqa: BLE001
            last_err = exc
            time.sleep(0.25)
    raise RuntimeError(f"server not ready at {url}: {last_err}")


def start_llama_server(model_path: str, port: int, ngl: int = 99) -> subprocess.Popen:
    llama = (
        __import__("shutil").which("llama-server")
        or __import__("shutil").which("llama-server-cli")
    )
    if not llama:
        raise RuntimeError("llama-server not found on PATH")
    cmd = [
        llama,
        "-m",
        model_path,
        "--host",
        "127.0.0.1",
        "--port",
        str(port),
        "-ngl",
        str(ngl),
        "--ctx-size",
        "2048",
        # Keep idle quiet; no continuous batch churn.
        "--parallel",
        "1",
    ]
    log_path = Path(f"/tmp/merc-energy-deltas-llama-server-{port}.log")
    log_fh = open(log_path, "w")  # noqa: SIM115 — kept for process lifetime
    proc = subprocess.Popen(
        cmd,
        stdout=log_fh,
        stderr=subprocess.STDOUT,
        start_new_session=True,
    )
    proc._merc_log_fh = log_fh  # type: ignore[attr-defined]
    proc._merc_log_path = str(log_path)  # type: ignore[attr-defined]
    return proc


def stop_proc(proc: subprocess.Popen | None) -> None:
    if proc is None:
        return
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except (ProcessLookupError, PermissionError, OSError):
        try:
            proc.terminate()
        except Exception:  # noqa: BLE001
            pass
    try:
        proc.wait(timeout=15)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(proc.pid, signal.SIGKILL)
        except Exception:  # noqa: BLE001
            proc.kill()
        proc.wait(timeout=5)
    fh = getattr(proc, "_merc_log_fh", None)
    if fh is not None:
        try:
            fh.close()
        except Exception:  # noqa: BLE001
            pass


def server_completion(
    port: int,
    prompt: str,
    n_predict: int,
    temperature: float = 0.0,
) -> dict:
    """One completion against llama-server with ignore_eos so n_predict is exact.

    Prefers the native ``/completion`` endpoint (supports ignore_eos). Falls back
    to OpenAI chat completions only if /completion is missing — that path cannot
    force ignore_eos and must not be compared token-for-token to llama-cli cold.
    """
    url = f"http://127.0.0.1:{port}/completion"
    body = {
        "prompt": prompt,
        "n_predict": n_predict,
        "temperature": temperature,
        "stream": False,
        "ignore_eos": True,
    }
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            payload = json.loads(resp.read().decode())
            status = resp.status
    except urllib.error.HTTPError as exc:
        err_body = exc.read().decode(errors="replace")
        if exc.code in (404, 405):
            return _chat_completions_no_ignore_eos(
                port, prompt, n_predict, temperature
            )
        raise RuntimeError(f"completion HTTP {exc.code}: {err_body[:500]}") from exc
    elapsed = time.perf_counter() - t0
    # With ignore_eos, tokens_predicted should equal n_predict on success.
    predicted = payload.get("tokens_predicted")
    completion = int(predicted) if predicted is not None else int(n_predict)
    prompt_tok = int(payload.get("tokens_evaluated") or 0)
    return {
        "runtime": "llama_server_metal",
        "endpoint": url,
        "exit_code": 0 if status == 200 else status,
        "elapsed_s": round(elapsed, 4),
        "verified_outcomes": completion,
        "prompt_tokens": prompt_tok,
        "n_predict_requested": n_predict,
        "status": status,
        "ignore_eos": True,
        "stopped_eos": payload.get("stopped_eos"),
        "outcome_count_basis": (
            "tokens_predicted_with_ignore_eos"
            if predicted is not None
            else "n_predict_with_ignore_eos"
        ),
    }


def _chat_completions_no_ignore_eos(
    port: int, prompt: str, n_predict: int, temperature: float = 0.0
) -> dict:
    """Last-resort OpenAI path — EOS can stop early; flag that in the receipt."""
    url = f"http://127.0.0.1:{port}/v1/chat/completions"
    body = {
        "model": "local",
        "messages": [{"role": "user", "content": prompt}],
        "max_tokens": n_predict,
        "temperature": temperature,
        "stream": False,
    }
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.perf_counter()
    with urllib.request.urlopen(req, timeout=600) as resp:
        payload = json.loads(resp.read().decode())
        status = resp.status
    elapsed = time.perf_counter() - t0
    usage = payload.get("usage") or {}
    completion = int(usage.get("completion_tokens") or 0)
    prompt_tok = int(usage.get("prompt_tokens") or 0)
    return {
        "runtime": "llama_server_metal_chat_no_ignore_eos",
        "endpoint": url,
        "exit_code": 0 if status == 200 else status,
        "elapsed_s": round(elapsed, 4),
        "verified_outcomes": completion,
        "prompt_tokens": prompt_tok,
        "n_predict_requested": n_predict,
        "usage": usage,
        "status": status,
        "ignore_eos": False,
        "outcome_count_basis": "openai_usage_completion_tokens",
        "warning": (
            "OpenAI chat path cannot force ignore_eos; token counts may not "
            "match cold llama-cli. Prefer /completion."
        ),
    }


def measure_under_sampler(label: str, fn, interval_ms: int) -> tuple[dict, dict]:
    with open_power_sampler("ioreport", interval_ms=interval_ms) as power:
        detail = fn()
    r = power.result()
    r["label"] = label
    r["window_s"] = detail.get("elapsed_s")
    return r, detail


def duty_curve(
    w_unloaded: float,
    w_resident: float,
    w_load: float,
    points: list[float] | None = None,
) -> list[dict]:
    """Fraction of always-on energy avoided by scale-to-zero at duty cycle d.

    Always-on (model resident even when idle):
        E_on(d) = d * W_load + (1-d) * W_resident
    Scale-to-zero (unload when idle; load power assumed same as warm steady —
    cold-start amortization is reported separately, not folded into this curve):
        E_z(d)  = d * W_load + (1-d) * W_unloaded

    Idle-only avoided fraction (duty-independent, the pure residency delta):
        idle_avoided = (W_resident - W_unloaded) / W_resident

    Total energy avoided fraction at duty d:
        total_avoided(d) = (E_on - E_z) / E_on
                         = (1-d)*(W_resident - W_unloaded)
                           / (d*W_load + (1-d)*W_resident)
    """
    if points is None:
        points = [0.0, 0.01, 0.05, 0.10, 0.25, 0.50, 0.70, 0.90, 1.0]
    idle_delta = w_resident - w_unloaded
    idle_avoided = (
        idle_delta / w_resident if w_resident > 0 else None
    )
    out = []
    for d in points:
        e_on = d * w_load + (1.0 - d) * w_resident
        e_z = d * w_load + (1.0 - d) * w_unloaded
        saved = e_on - e_z
        total_avoided = saved / e_on if e_on > 0 else None
        out.append(
            {
                "duty_cycle": d,
                "always_on_mean_watts": round(e_on, 4),
                "scale_to_zero_mean_watts": round(e_z, 4),
                "saved_mean_watts": round(saved, 4),
                "total_energy_avoided_fraction": (
                    round(total_avoided, 6) if total_avoided is not None else None
                ),
                "idle_power_avoided_fraction": (
                    round(idle_avoided, 6) if idle_avoided is not None else None
                ),
                "note": (
                    "total_energy_avoided_fraction is vs always-resident at the "
                    "same W_load; cold-start energy is not amortized here"
                ),
            }
        )
    return out


def probe_package_energy_available() -> dict:
    """powermetrics package path requires root; report ceiling honestly."""
    reason = None
    if platform.system() != "Darwin":
        reason = f"unsupported platform {platform.system()}"
    elif os.geteuid() != 0:
        reason = "powermetrics package path requires root (sudo); not elevated here"
    else:
        # Even as root we do not drive a package measurement in this lane —
        # the existing authority is GPU-domain; package is a different boundary.
        reason = "root available but package measurement is out of this receipt's boundary"
    return {
        "backend": "powermetrics_combined_power",
        "available_this_run": False,
        "reason": reason,
        "boundary_if_available": (
            "powermetrics Combined Power (CPU+GPU+ANE package). Not wall-plug."
        ),
    }


def probe_cuda_available() -> dict:
    return {
        "backend": "nvidia_smi_or_nvml",
        "available_this_run": False,
        "reason": "no NVIDIA device / nvidia-smi on this host; CUDA figures are not fabricated",
        "devices": [],
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--gguf", default=None)
    ap.add_argument("--n-predict", type=int, default=512)
    ap.add_argument("--idle-s", type=float, default=8.0)
    ap.add_argument("--resident-idle-s", type=float, default=8.0)
    ap.add_argument("--interval-ms", type=int, default=200)
    ap.add_argument("--prompt", default=DEFAULT_PROMPT)
    ap.add_argument(
        "--receipt",
        default="evidence/perf/ioreport-energy-deltas-authority.json",
    )
    ap.add_argument(
        "--warm-repeats",
        type=int,
        default=1,
        help="number of warm requests to average (each measured separately)",
    )
    ap.add_argument(
        "--cold-repeats",
        type=int,
        default=1,
        help="number of cold llama-cli runs to average",
    )
    args = ap.parse_args()

    model_path = args.gguf or _default_gguf()
    if not model_path or not Path(model_path).is_file():
        print(json.dumps({"error": "GGUF model not found", "gguf": model_path}), file=sys.stderr)
        return 2

    probe = open_power_sampler("ioreport", interval_ms=args.interval_ms)
    if not probe.available:
        print(
            json.dumps(
                {
                    "error": "IOReport GPU Energy unavailable",
                    "reason": probe.reason,
                    "hint": "re-run unsandboxed; do not substitute sudo powermetrics",
                },
                indent=2,
            ),
            file=sys.stderr,
        )
        return 2

    port = free_port()
    server: subprocess.Popen | None = None
    errors: list[str] = []
    load_energy: dict = {}
    load_detail_clean: dict = {}
    cold_placement_runs: list[dict] = []
    warm_runs: list[dict] = []
    cold_cli_runs: list[dict] = []
    unloaded: dict = {}
    resident: dict = {}

    try:
        # ── 1) Unloaded idle ─────────────────────────────────────────────
        print("energy-deltas: unloaded idle window…", flush=True)
        unloaded = sample_window(
            "unloaded_idle", args.idle_s, interval_ms=args.interval_ms
        )

        # ── 2) Start server + measure pure model-load energy ─────────────
        print(f"energy-deltas: starting llama-server on :{port}…", flush=True)
        load_energy, load_detail = measure_under_sampler(
            "server_model_load",
            lambda: _start_and_ready(model_path, port),
            args.interval_ms,
        )
        server = load_detail["proc"]
        load_detail_clean = {k: v for k, v in load_detail.items() if k != "proc"}

        # ── 3) Resident idle (model held, no traffic) ────────────────────
        print("energy-deltas: resident idle window (model held)…", flush=True)
        time.sleep(2.0)
        resident = sample_window(
            "resident_idle", args.resident_idle_s, interval_ms=args.interval_ms
        )

        # ── 4) Warm request(s) — model already resident ──────────────────
        for i in range(max(1, args.warm_repeats)):
            print(
                f"energy-deltas: warm request {i+1}/{args.warm_repeats}…",
                flush=True,
            )
            energy, detail = measure_under_sampler(
                f"warm_request_{i}",
                lambda: server_completion(port, args.prompt, args.n_predict),
                args.interval_ms,
            )
            warm_runs.append({"energy": energy, "workload": detail})

        # ── 5) Cold placement: stop → start+first-request (load included) ─
        # This is the placement energy argument: a request that finds no warm
        # model pays load + inference. Measured as one integrated window so
        # load is included, not estimated.
        for i in range(max(1, args.cold_repeats)):
            print(
                f"energy-deltas: cold placement {i+1}/{args.cold_repeats} "
                f"(stop → load → first request)…",
                flush=True,
            )
            stop_proc(server)
            server = None
            time.sleep(1.5)
            port = free_port()

            def _cold_placement(p=port, i=i):
                started = _start_and_ready(model_path, p)
                # Keep the process handle on the outer scope.
                nonlocal_holder["proc"] = started["proc"]
                req = server_completion(p, args.prompt, args.n_predict)
                return {
                    "load_elapsed_s": started["elapsed_s"],
                    "load_log_path": started.get("log_path"),
                    "port": p,
                    "request": req,
                    "elapsed_s": round(
                        started["elapsed_s"] + float(req["elapsed_s"]), 4
                    ),
                    "verified_outcomes": req["verified_outcomes"],
                    "prompt_tokens": req.get("prompt_tokens"),
                    "n_predict_requested": args.n_predict,
                    "runtime": "llama_server_metal_cold_placement",
                    "outcome_count_basis": req.get("outcome_count_basis"),
                    "ignore_eos": req.get("ignore_eos"),
                }

            nonlocal_holder: dict = {"proc": None}
            energy, detail = measure_under_sampler(
                f"cold_placement_{i}", _cold_placement, args.interval_ms
            )
            server = nonlocal_holder["proc"]
            cold_placement_runs.append({"energy": energy, "workload": detail})

        # ── 6) Secondary: cold llama-cli process (load+gen in one process) ─
        print("energy-deltas: stopping server for llama-cli cold baseline…", flush=True)
        stop_proc(server)
        server = None
        time.sleep(1.5)
        for i in range(max(1, args.cold_repeats)):
            print(
                f"energy-deltas: cold llama-cli {i+1}/{args.cold_repeats}…",
                flush=True,
            )
            energy, detail = measure_under_sampler(
                f"cold_llama_cli_{i}",
                lambda: run_llama_cli_workload(
                    model_path, n_predict=args.n_predict, prompt=args.prompt
                ),
                args.interval_ms,
            )
            cold_cli_runs.append({"energy": energy, "workload": detail})

    finally:
        stop_proc(server)

    # ── Aggregate ────────────────────────────────────────────────────────
    w_unloaded = unloaded.get("mean_watts")
    w_resident = resident.get("mean_watts")
    # Load mean for the curve: warm steady mean (decode-only, model resident).
    warm_means = [
        r["energy"]["mean_watts"]
        for r in warm_runs
        if r["energy"].get("mean_watts") is not None
    ]
    cold_cli_means = [
        r["energy"]["mean_watts"]
        for r in cold_cli_runs
        if r["energy"].get("mean_watts") is not None
    ]
    w_load = (
        sum(warm_means) / len(warm_means)
        if warm_means
        else (sum(cold_cli_means) / len(cold_cli_means) if cold_cli_means else None)
    )

    if None in (w_unloaded, w_resident, w_load):
        errors.append("missing mean_watts for curve inputs")

    idle_delta_w = (
        round(float(w_resident) - float(w_unloaded), 4)
        if None not in (w_unloaded, w_resident)
        else None
    )

    def _sample_stdev(window: dict) -> float | None:
        xs = window.get("raw_watts_samples") or []
        if len(xs) < 2:
            return None
        return statistics.stdev(xs)

    unloaded_sd = _sample_stdev(unloaded)
    resident_sd = _sample_stdev(resident)
    # Combined uncertainty proxy for the mean difference (independent windows).
    idle_delta_noise = None
    if unloaded_sd is not None and resident_sd is not None:
        n_u = max(1, int(unloaded.get("samples") or 1))
        n_r = max(1, int(resident.get("samples") or 1))
        idle_delta_noise = (unloaded_sd**2 / n_u + resident_sd**2 / n_r) ** 0.5
    idle_delta_within_noise = (
        idle_delta_w is not None
        and idle_delta_noise is not None
        and abs(idle_delta_w) <= 2.0 * idle_delta_noise
    )
    idle_avoided_frac = (
        round(idle_delta_w / float(w_resident), 6)
        if idle_delta_w is not None and w_resident and float(w_resident) > 0
        else None
    )
    if idle_delta_within_noise and idle_avoided_frac is not None:
        # Do not claim a negative "avoided" fraction from noise; report ~0.
        idle_avoided_frac_raw = idle_avoided_frac
        idle_avoided_frac = 0.0
    else:
        idle_avoided_frac_raw = idle_avoided_frac

    # When idle delta is noise, force the curve's resident==unloaded so we do
    # not publish negative "scale-to-zero savings" from sampling jitter.
    if idle_delta_within_noise and None not in (w_unloaded, w_resident):
        w_unl_curve = w_res_curve = (float(w_unloaded) + float(w_resident)) / 2.0
    else:
        w_unl_curve = float(w_unloaded) if w_unloaded is not None else None
        w_res_curve = float(w_resident) if w_resident is not None else None

    curve = (
        duty_curve(float(w_unl_curve), float(w_res_curve), float(w_load))
        if None not in (w_unl_curve, w_res_curve, w_load)
        else []
    )

    def avg_j(runs: list[dict]) -> float | None:
        js = [
            r["energy"]["energy_joules"]
            for r in runs
            if r["energy"].get("energy_joules") is not None
        ]
        return sum(js) / len(js) if js else None

    def avg_outcomes(runs: list[dict]) -> float | None:
        os_ = [
            r["workload"]["verified_outcomes"]
            for r in runs
            if r["workload"].get("verified_outcomes")
        ]
        return sum(os_) / len(os_) if os_ else None

    # Primary cold = cold placement (load + first request). Secondary = llama-cli.
    cold_j = avg_j(cold_placement_runs)
    warm_j = avg_j(warm_runs)
    cold_cli_j = avg_j(cold_cli_runs)
    cold_out = avg_outcomes(cold_placement_runs)
    warm_out = avg_outcomes(warm_runs)
    cold_cli_out = avg_outcomes(cold_cli_runs)

    # Guard: unequal outcome counts make total-joule comparison misleading.
    if (
        cold_out is not None
        and warm_out is not None
        and abs(cold_out - warm_out) > max(1.0, 0.05 * max(cold_out, warm_out))
    ):
        errors.append(
            f"cold/warm verified_outcomes diverge "
            f"(cold={cold_out}, warm={warm_out}); prefer per-outcome figures"
        )

    cold_j_per = (
        cold_j / cold_out if cold_j is not None and cold_out and cold_out > 0 else None
    )
    warm_j_per = (
        warm_j / warm_out if warm_j is not None and warm_out and warm_out > 0 else None
    )
    cold_cli_j_per = (
        cold_cli_j / cold_cli_out
        if cold_cli_j is not None and cold_cli_out and cold_cli_out > 0
        else None
    )

    if cold_j is not None and warm_j is not None and cold_j > 0:
        warm_vs_cold_energy_delta_frac = round((warm_j - cold_j) / cold_j, 6)
        warm_energy_savings_frac = round((cold_j - warm_j) / cold_j, 6)
    else:
        warm_vs_cold_energy_delta_frac = None
        warm_energy_savings_frac = None

    if cold_j_per is not None and warm_j_per is not None and cold_j_per > 0:
        warm_vs_cold_j_per_outcome_savings = round(
            (cold_j_per - warm_j_per) / cold_j_per, 6
        )
    else:
        warm_vs_cold_j_per_outcome_savings = None

    # Amortization of fixed load energy over request size. Measured load cost is
    # the cold−warm joule gap (same n_predict); fall back to server_model_load
    # window if the gap is non-positive (noise).
    load_j_fixed = None
    if cold_j is not None and warm_j is not None and cold_j > warm_j:
        load_j_fixed = cold_j - warm_j
    elif load_energy.get("energy_joules") is not None:
        load_j_fixed = float(load_energy["energy_joules"])

    warm_j_per_for_curve = warm_j_per
    amortization_curve = []
    n_for_50pct = None
    if (
        load_j_fixed is not None
        and warm_j_per_for_curve is not None
        and warm_j_per_for_curve > 0
    ):
        n_for_50pct = load_j_fixed / warm_j_per_for_curve  # savings=50% when E_load = n*j
        for n in (1, 4, 8, 16, 32, 64, 128, 256, 512, 1024, 2048):
            e_warm = n * warm_j_per_for_curve
            e_cold = load_j_fixed + e_warm
            sav = (e_cold - e_warm) / e_cold if e_cold > 0 else None
            amortization_curve.append(
                {
                    "verified_outcomes": n,
                    "cold_joules_model": round(e_cold, 6),
                    "warm_joules_model": round(e_warm, 6),
                    "warm_energy_savings_fraction": (
                        round(sav, 6) if sav is not None else None
                    ),
                    "meets_minus_50pct": bool(sav is not None and sav >= 0.50),
                }
            )

    # Realistic duty-cycle placements on the curve
    anchors = {}
    for name, d in DUTY_CYCLE_ANCHORS.items():
        if curve and None not in (w_unl_curve, w_res_curve, w_load):
            # Same noise-clamped watts as duty_cycle_curve (not the raw means).
            row = duty_curve(
                float(w_unl_curve), float(w_res_curve), float(w_load), points=[d]
            )[0]
            anchors[name] = row

    package = probe_package_energy_available()
    cuda = probe_cuda_available()

    # Telemetry coverage: what fraction of routed volume can carry a *real*
    # energy reading on this host, for each boundary.
    telemetry = {
        "question": (
            "What fraction of a routed request's energy is currently observable "
            "on this hardware?"
        ),
        "host_has_nvidia": False,
        "boundaries": {
            "gpu_domain_ioreport": {
                "observable_unprivileged": True,
                "covers": "AGX on-chip GPU Energy for local Metal work",
                "coverage_of_gpu_domain_when_runtime_is_metal_local": 1.0,
                "coverage_of_package": "partial_lower_bound",
                "coverage_of_wall_plug": 0.0,
                "note": (
                    "Every local Metal-routed token's GPU-domain joules are "
                    "measurable without sudo. That is not package energy and "
                    "not wall energy."
                ),
            },
            "package_powermetrics": {
                "observable_unprivileged": False,
                "requires": "sudo / root",
                "available_this_run": package["available_this_run"],
                "reason": package["reason"],
                "coverage_if_elevated": (
                    "CPU+GPU+ANE Combined Power samples; still not wall-plug"
                ),
            },
            "cuda_nvml": {
                "observable_on_this_host": False,
                "reason": cuda["reason"],
                "coverage": 0.0,
            },
            "wall_plug": {
                "observable_on_this_host": False,
                "reason": "no external power meter instrumented in this lane",
                "coverage": 0.0,
            },
        },
        "honest_ceiling_this_hardware": {
            "gpu_domain_fraction_of_local_metal_volume": 1.0,
            "package_fraction_without_sudo": 0.0,
            "package_fraction_with_sudo": (
                "measurable but not measured in this receipt; different boundary"
            ),
            "wall_plug_fraction": 0.0,
            "cuda_fraction": 0.0,
            "target_gt_90pct_routed_volume_with_real_reading": {
                "reachable_for_gpu_domain_on_metal_local_volume": True,
                "reachable_for_package_without_privilege": False,
                "reachable_for_wall_plug": False,
                "reachable_for_cuda_volume_on_this_host": False,
                "note": (
                    "The >90% target is reachable only if the reading's boundary "
                    "is GPU-domain and the routed volume is local Metal. Expanding "
                    "the claim to package or wall needs privilege or hardware "
                    "Merc does not have in this lane."
                ),
            },
        },
    }

    j_per_baseline = {
        "target": "joules per verified outcome −20–40%",
        "existing_level": {
            "source": "evidence/perf/ioreport-gpu-energy-authority.json",
            "joules_per_verified_outcome": 0.121724572,
            "note": "a level, not a comparison; single point on one host",
        },
        "this_measurement": {
            "cold_joules_per_verified_outcome": (
                round(cold_j_per, 9) if cold_j_per is not None else None
            ),
            "warm_joules_per_verified_outcome": (
                round(warm_j_per, 9) if warm_j_per is not None else None
            ),
            "boundary": IOREPORT_BOUNDARY,
        },
        "required_baseline_for_delta_target": {
            "named": (
                "Same workload (model, prompt, n_predict, verified-outcome unit) "
                "on the same hardware, without Merc placement decisions — i.e. a "
                "direct always-on resident serving path vs Merc warm/locality-"
                "aware placement, or a non-Merc fixed-cloud baseline on identical "
                "silicon. The −20–40% is a *delta*, so two bound points are required."
            ),
            "fair_baseline_exists_on_this_host": False,
            "why_not": (
                "This host has one Metal path (llama.cpp). There is no second "
                "placement policy, no multi-node fabric under measurement, and no "
                "side-by-side non-Merc control on the same silicon in this lane. "
                "Warm-vs-cold is a *placement locality* delta (measured here) and "
                "must not be silently re-labeled as the programme −20–40% J/outcome "
                "target without naming that they are different comparisons."
            ),
            "what_would_make_it_known": (
                "A bound pair: (A) always-on cold-or-random placement energy per "
                "verified outcome, (B) Merc placement with prefix/model locality, "
                "same model and outcome unit, same host class, same boundary."
            ),
        },
        "do_not_confuse_with": (
            "warm_vs_cold energy savings measured in this receipt — that is the "
            "locality/placement energy argument, not the programme-level −20–40% "
            "against an external baseline."
        ),
    }

    reachability = {
        "idle_power_avoided_through_scale_to_zero_70_to_95pct": {
            "measured_idle_avoided_fraction_gpu_domain": idle_avoided_frac,
            "target_band": [0.70, 0.95],
            "reachable_here": (
                idle_avoided_frac is not None and idle_avoided_frac >= 0.70
            ),
            "view": (
                "On this Apple Silicon host the GPU-domain resident-vs-unloaded "
                "delta is the right measurement for the target *as stated in GPU "
                "terms*. If resident idle barely exceeds unloaded idle (weights "
                "sit in unified memory; GPU clocks drop), the 70–95% band is NOT "
                "reachable on the GPU domain — scale-to-zero's big idle win is a "
                "CUDA-card story (tens of watts idle with a model resident). "
                "Package or wall might show more residency cost (DRAM kept hot); "
                "those boundaries are not measured unprivileged here."
            ),
        },
        "warm_placement_vs_cold_loading_minus_50pct_plus_energy": {
            "measured_warm_energy_savings_fraction_at_n_predict": warm_energy_savings_frac,
            "n_predict": args.n_predict,
            "n_predict_for_50pct_savings_model": (
                round(n_for_50pct, 2) if n_for_50pct is not None else None
            ),
            "target": "≤ −50% energy for warm vs cold (load included in cold)",
            "reachable_here_at_measured_n": (
                warm_energy_savings_frac is not None
                and warm_energy_savings_frac >= 0.50
            ),
            "reachable_here_for_short_requests": (
                n_for_50pct is not None and n_for_50pct > 0
            ),
            "view": (
                "Measurable on this host. At n_predict=512 on a 1B Q4 model the "
                "GPU-domain load is ~1 J vs ~60 J decode, so savings are ~2%, not "
                "50%. The −50% band is reachable only for short requests "
                "(n ≲ E_load/J_per_outcome) or heavier models whose load cost is "
                "large relative to decode. Latency still favors warm; energy on "
                "the GPU domain does not automatically."
            ),
        },
        "joules_per_verified_outcome_minus_20_to_40pct": {
            "reachable_here": False,
            "view": (
                "Not reachable as a *delta* without a named baseline. Warm-vs-cold "
                "must not be substituted. Needs a fair non-Merc or non-locality "
                "control on the same silicon, or an explicit programme decision "
                "that warm-vs-cold *is* the baseline comparison."
            ),
        },
        "energy_readings_with_actual_telemetry_gt_90pct_routed_volume": {
            "reachable_here_gpu_domain_metal_local": True,
            "reachable_here_package_or_wall": False,
            "view": (
                "GPU-domain: yes, for local Metal volume, unprivileged. Package: "
                "needs sudo. Wall: needs a meter. CUDA: no device. The >90% target "
                "is boundary-dependent; claim it only with the boundary attached."
            ),
        },
    }

    payload = {
        "schema_version": 1,
        "kind": "measured_energy_deltas_authority",
        "title": (
            "GPU-domain energy deltas: idle scale-to-zero curve and warm vs cold "
            "placement (IOReport, unprivileged)"
        ),
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
            "sampling_interval_ms": args.interval_ms,
            "integration": "IOReport cumulative GPU Energy counter delta (mJ→J)",
            "boundary": IOREPORT_BOUNDARY,
            "measures": IOREPORT_MEASURES,
            "excludes": IOREPORT_EXCLUDES,
            "error_notes": [
                f"Sampling interval {args.interval_ms} ms; sub-interval spikes averaged into each sample.",
                "Joules are cumulative counter delta, not mean(watts)*time.",
                "Windows are sequential, not simultaneous — ambient GPU work from other processes is included.",
                "GPU-alone energy is a lower bound on package energy and is not comparable to wall-plug joules.",
                "Cold placement window is stop→llama-server start→first /completion (model load + decode); load is included.",
                "Warm window is a single /completion against an already-loaded llama-server (no model load).",
                "Secondary cold_llama_cli window is process lifetime (load + decode) as a shape cross-check.",
                "Both server paths use ignore_eos so verified_outcomes track n_predict.",
                "Resident idle is llama-server holding weights with no in-flight requests after a short settle.",
                "Scale-to-zero curve uses the same W_load for always-on and scale-to-zero; cold-start amortization is reported as warm-vs-cold, not folded into the duty curve.",
                "No NVIDIA device on this host; CUDA numbers are not fabricated.",
                "src/control/pricing.go ASSUMED apple_silicon_ultra 65 W is not edited and is package-boundary, not this receipt.",
            ],
        },
        "model": {
            "path": model_path,
            "n_predict": args.n_predict,
            "prompt_chars": len(args.prompt),
        },
        "windows": {
            "unloaded_idle": _window_view(unloaded),
            "server_model_load": {
                **_window_view(load_energy),
                "detail": load_detail_clean,
            },
            "resident_idle": _window_view(resident),
            "warm_runs": [
                {
                    "energy": _window_view(r["energy"]),
                    "workload": _workload_view(r["workload"]),
                }
                for r in warm_runs
            ],
            "cold_placement_runs": [
                {
                    "energy": _window_view(r["energy"]),
                    "workload": _workload_view(r["workload"]),
                }
                for r in cold_placement_runs
            ],
            "cold_llama_cli_runs": [
                {
                    "energy": _window_view(r["energy"]),
                    "workload": _workload_view(r["workload"]),
                }
                for r in cold_cli_runs
            ],
        },
        "idle_power_scale_to_zero": {
            "unloaded_mean_watts": w_unloaded,
            "resident_mean_watts": w_resident,
            "idle_delta_watts": idle_delta_w,
            "idle_delta_noise_stderr_watts": (
                round(idle_delta_noise, 4) if idle_delta_noise is not None else None
            ),
            "idle_delta_within_2sigma_noise": idle_delta_within_noise,
            "idle_power_avoided_fraction_raw": idle_avoided_frac_raw,
            "idle_power_avoided_fraction": idle_avoided_frac,
            "idle_power_avoided_fraction_note": (
                "When idle_delta_within_2sigma_noise is true, the resident and "
                "unloaded GPU means are indistinguishable; avoided fraction is "
                "reported as 0.0 (raw signed value kept in "
                "idle_power_avoided_fraction_raw)."
                if idle_delta_within_noise
                else "Resident mean exceeds unloaded beyond sample noise."
            ),
            "load_mean_watts_for_curve": (
                round(float(w_load), 4) if w_load is not None else None
            ),
            "load_mean_source": "warm_request_mean" if warm_means else "cold_request_mean",
            "formula": (
                "total_energy_avoided(d) = (1-d)*(W_resident - W_unloaded) "
                "/ (d*W_load + (1-d)*W_resident); "
                "idle_power_avoided = (W_resident - W_unloaded) / W_resident"
            ),
            "duty_cycle_curve": curve,
            "realistic_duty_cycle_anchors": anchors,
            "interpretation": (
                "The answer is the curve, not a single number. Read "
                "idle_power_avoided_fraction for the pure residency delta; read "
                "total_energy_avoided_fraction at each duty for share of always-on "
                "energy that scale-to-zero removes. Real traffic sits near the "
                "anchors (sparse_dev / bursty_canary / interactive_business_hours / "
                "batch_overnight)."
            ),
        },
        "warm_versus_cold": {
            "definition": (
                "cold_placement = energy of (model load + first request) when "
                "nothing was resident; warm = same request against already-loaded "
                "llama-server. Load is included in cold, not in warm. Both paths "
                "use ignore_eos so verified_outcomes match n_predict."
            ),
            "cold_mean_energy_joules": (
                round(cold_j, 6) if cold_j is not None else None
            ),
            "warm_mean_energy_joules": (
                round(warm_j, 6) if warm_j is not None else None
            ),
            "cold_mean_verified_outcomes": cold_out,
            "warm_mean_verified_outcomes": warm_out,
            "cold_joules_per_verified_outcome": (
                round(cold_j_per, 9) if cold_j_per is not None else None
            ),
            "warm_joules_per_verified_outcome": (
                round(warm_j_per, 9) if warm_j_per is not None else None
            ),
            "warm_minus_cold_over_cold": warm_vs_cold_energy_delta_frac,
            "warm_energy_savings_fraction": warm_energy_savings_frac,
            "warm_j_per_outcome_savings_fraction": warm_vs_cold_j_per_outcome_savings,
            "load_included_in_cold": True,
            "load_included_in_warm": False,
            "server_model_load_only_joules": load_energy.get("energy_joules"),
            "secondary_cold_llama_cli": {
                "mean_energy_joules": (
                    round(cold_cli_j, 6) if cold_cli_j is not None else None
                ),
                "mean_verified_outcomes": cold_cli_out,
                "joules_per_verified_outcome": (
                    round(cold_cli_j_per, 9) if cold_cli_j_per is not None else None
                ),
                "note": (
                    "llama-cli process lifetime (load+gen) as a process-shape "
                    "cross-check; primary comparison is cold_placement vs warm."
                ),
                "consistency_warning": (
                    (
                        "llama-cli joules_per_verified_outcome diverges >2x from "
                        "the warm server path and from "
                        "evidence/perf/ioreport-gpu-energy-authority.json "
                        "(~0.12 J/outcome). Treat as secondary only; do not use "
                        "for the warm-vs-cold headline."
                    )
                    if (
                        cold_cli_j_per is not None
                        and warm_j_per is not None
                        and warm_j_per > 0
                        and (
                            cold_cli_j_per < 0.5 * warm_j_per
                            or cold_cli_j_per > 2.0 * warm_j_per
                        )
                    )
                    else None
                ),
            },
            "target": "warm placement versus cold loading −50%+ energy",
            "meets_target_on_total_joules": (
                warm_energy_savings_frac is not None
                and warm_energy_savings_frac >= 0.50
            ),
            "meets_target_on_j_per_outcome": (
                warm_vs_cold_j_per_outcome_savings is not None
                and warm_vs_cold_j_per_outcome_savings >= 0.50
            ),
            "fixed_load_joules_estimate": (
                round(load_j_fixed, 6) if load_j_fixed is not None else None
            ),
            "fixed_load_source": (
                "cold_mean_joules - warm_mean_joules"
                if cold_j is not None
                and warm_j is not None
                and cold_j > warm_j
                else "server_model_load_window"
            ),
            "n_predict_for_50pct_savings": (
                round(n_for_50pct, 2) if n_for_50pct is not None else None
            ),
            "amortization_curve_vs_outcome_count": amortization_curve,
            "amortization_note": (
                "Model: E_cold(n)=E_load + n*J_warm_per_outcome, "
                "E_warm(n)=n*J_warm_per_outcome. The −50% target is a property of "
                "request size relative to fixed load cost on this model/host, not "
                "a single universal fraction. At n_predict=512 on Llama-3.2-1B-Q4 "
                "the fixed GPU-domain load is tiny vs decode."
            ),
        },
        "joules_per_verified_outcome_comparison": j_per_baseline,
        "telemetry_coverage": telemetry,
        "target_reachability": reachability,
        "errors": errors,
        "not_production_pricing_truth": (
            "GPU-domain energy deltas for one host and one model. Not package, "
            "not wall-plug. Does not replace sustainedWattsByHWClass. Must not be "
            "written into src/control/pricing.go without a separate pricing lane."
        ),
        "vs_assumed_sustained_watts": {
            "hw_class": "apple_silicon_ultra",
            "assumed_watts": 65.0,
            "assumed_kind": "ASSUMED",
            "assumed_source": "src/control/pricing.go sustainedWattsByHWClass (not edited)",
            "comparison_caveat": (
                "ASSUMED 65 W is whole-package. This receipt is GPU-domain only. "
                "A GPU mean below 65 W is not evidence the package assumption is high."
            ),
        },
        "related_authority": {
            "level_not_delta": "evidence/perf/ioreport-gpu-energy-authority.json",
            "note": (
                "Prior receipt is one J/outcome level. This receipt supplies the "
                "deltas (idle curve, warm vs cold) that level could not."
            ),
        },
    }

    summary = {
        "binding_status": None,
        "receipt": None,
        "unloaded_mean_watts": w_unloaded,
        "resident_mean_watts": w_resident,
        "idle_power_avoided_fraction": idle_avoided_frac,
        "warm_energy_joules": warm_j,
        "cold_energy_joules": cold_j,
        "warm_energy_savings_fraction": warm_energy_savings_frac,
        "warm_j_per_outcome": warm_j_per,
        "cold_j_per_outcome": cold_j_per,
        "boundary": IOREPORT_BOUNDARY,
        "anchors": {
            k: v.get("total_energy_avoided_fraction") for k, v in anchors.items()
        },
        "errors": errors,
    }

    if os.environ.get(WRITE_ENV) != "1":
        summary["binding_status"] = "NOT_WRITTEN"
        summary["write_gate"] = (
            f"set {WRITE_ENV}=1 to write the bound receipt to {args.receipt}"
        )
        print(
            f"energy-deltas: measurement ok; not writing "
            f"(export {WRITE_ENV}=1 to write)",
            flush=True,
        )
        print(json.dumps(summary, indent=2), flush=True)
        print(json.dumps({"payload_preview": payload}, indent=2), flush=True)
        return 0 if not errors else 2

    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        default_bound_identity,
        sha256_file,
        slot_value,
        write_bound_evidence,
    )

    receipt_path = Path(args.receipt)
    if not receipt_path.is_absolute():
        receipt_path = ROOT / receipt_path
    harness = "ops/scripts/energy-deltas-measure.py"
    build_path = Path(__file__).resolve()
    model_digest = sha256_file(model_path)
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=harness,
            build_binary_path=build_path,
            exact_config=(
                f"n_predict={args.n_predict} idle_s={args.idle_s} "
                f"resident_idle_s={args.resident_idle_s} "
                f"interval_ms={args.interval_ms} "
                f"warm_repeats={args.warm_repeats} cold_repeats={args.cold_repeats} "
                f"backend=ioreport_gpu_energy"
            ),
            raw_samples=(
                f"unloaded_samples={unloaded.get('samples')} "
                f"resident_samples={resident.get('samples')} "
                f"warm_runs={len(warm_runs)} "
                f"cold_placement_runs={len(cold_placement_runs)} "
                f"cold_cli_runs={len(cold_cli_runs)}"
            ),
            model_na="placeholder overwritten by model_artifact_digest value",
            image_na="no container image in this measurement",
            corpus_na="no external corpus; synthetic/local prompt only",
        )
        identity["model_artifact_digest"] = slot_value(model_digest)
        write_bound_evidence(
            path=receipt_path,
            payload=payload,
            identity=identity,
            repo_root=ROOT,
            build_binary_path=build_path,
        )
    except EvidenceBindingError as exc:
        print(f"energy-deltas: REFUSED by binding writer: {exc}", file=sys.stderr)
        return 2

    summary["binding_status"] = "BOUND"
    summary["receipt"] = str(receipt_path)
    print(f"energy-deltas: wrote {receipt_path}", flush=True)
    print(json.dumps(summary, indent=2), flush=True)
    return 0 if not errors else 2


def _start_and_ready(model_path: str, port: int) -> dict:
    t0 = time.perf_counter()
    proc = start_llama_server(model_path, port)
    try:
        wait_http(f"http://127.0.0.1:{port}/health", timeout_s=180.0)
    except RuntimeError:
        # Some builds use /v1/models instead of /health
        try:
            wait_http(f"http://127.0.0.1:{port}/v1/models", timeout_s=60.0)
        except RuntimeError:
            # Last resort: root
            wait_http(f"http://127.0.0.1:{port}/", timeout_s=30.0)
    elapsed = time.perf_counter() - t0
    return {
        "proc": proc,
        "elapsed_s": round(elapsed, 4),
        "port": port,
        "log_path": getattr(proc, "_merc_log_path", None),
        "ready": True,
    }


def _window_view(w: dict) -> dict:
    return {
        "label": w.get("label"),
        "window_s": w.get("window_s"),
        "energy_joules": w.get("energy_joules"),
        "mean_watts": w.get("mean_watts"),
        "peak_watts": w.get("peak_watts"),
        "samples": w.get("samples"),
        "power_source": w.get("power_source"),
        "raw_watts_samples": w.get("raw_watts_samples") or [],
        "energy_boundary": w.get("energy_boundary"),
    }


def _workload_view(w: dict) -> dict:
    skip = {"stdout_tail", "proc"}
    return {k: v for k, v in w.items() if k not in skip}


if __name__ == "__main__":
    raise SystemExit(main())
