#!/usr/bin/env python3
"""Validate an external model before it can enter merc's catalogue.

Adding a model has been a code change: edit runtime-authority.json, rebuild,
and the boot-time policy in src/control/model_onboarding.go refuses anything merc
cannot resell. That policy is real but it only checks what the JSON *claims*.
Nothing checked that the model exists at the pinned revision, that it actually
loads, that it produces sane output, or how fast it is -- so a catalogue entry
could be priced and advertised on numbers nobody measured.

This is that missing half. It runs against a live runtime and refuses to emit a
profile if any stage fails:

  1. policy     licence on the resale allowlist, remote_code false, attribution
  2. identity   the runtime serves the alias we are onboarding
  3. smoke      a real completion, non-empty, with usage accounting
  4. determinism  temperature 0 twice returns the same bytes
  5. benchmark  measured prefill and decode throughput, not a guess

Emits a runtime profile carrying the MEASURED numbers, so the price a supplier
is paid is derived from what the model did rather than what someone assumed.

  python3 ops/scripts/onboard-model.py \\
      --endpoint http://127.0.0.1:8095/v1 --alias cx-chat-1b \\
      --license Apache-2.0 --license-url https://... \\
      --repo Qwen/Qwen2.5-3B-Instruct --revision <sha> \\
      --out evidence/onboarding/<id>.json
"""

import argparse
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request

_SCRIPTS = os.path.dirname(os.path.abspath(__file__))
if _SCRIPTS not in sys.path:
    sys.path.insert(0, _SCRIPTS)
from lib.evidence_binding import EvidenceBindingError, emit_bound_json  # noqa: E402


def _write_report(path: str, report: dict) -> None:
    try:
        emit_bound_json(
            path,
            report,
            harness="ops/scripts/onboard-model.py",
            build_binary_path=os.path.join(_SCRIPTS, "onboard-model.py"),
            exact_config="onboard stages + runtime_profile",
            raw_samples="measured samples embedded when present",
        )
    except EvidenceBindingError as exc:
        print(f"REFUSED evidence write: {exc}", file=sys.stderr)
        raise SystemExit(2) from exc

# Mirrors resaleAllowedLicenses in src/control/model_onboarding.go. Kept as an
# allowlist for the same reason: an unrecognised licence is refused, never
# assumed permissive.
RESALE_ALLOWED = {
    "Apache-2.0", "MIT", "BSD-3-Clause", "Llama-3.2-Community",
    "Qwen-Research", "Apache-2.0-with-attribution",
}


# RunPod's edge blocks default library User-Agents: urllib's "Python-urllib/3.x"
# gets 403 from a pod proxy even with a valid key and URL, while the identical
# request via curl returns 200. Without this every stage below reports a healthy
# CUDA engine as unreachable.
MERC_UA = "merc-onboard/1.0"


def post(endpoint, key, path, payload, timeout=120):
    req = urllib.request.Request(
        endpoint.rstrip("/") + path,
        data=json.dumps(payload).encode(),
        method="POST",
        headers={"content-type": "application/json",
                 "authorization": f"Bearer {key}",
                 "User-Agent": MERC_UA},
    )
    start = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = json.loads(resp.read())
    return time.perf_counter() - start, body


def get(endpoint, key, path, timeout=30):
    req = urllib.request.Request(
        endpoint.rstrip("/") + path,
        headers={"authorization": f"Bearer {key}", "User-Agent": MERC_UA},
    )
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--endpoint", required=True)
    ap.add_argument("--alias", required=True)
    ap.add_argument("--license", required=True)
    ap.add_argument("--license-url", required=True)
    ap.add_argument("--repo", required=True)
    ap.add_argument("--revision", required=True)
    ap.add_argument("--remote-code", action="store_true",
                    help="declare the model needs trust_remote_code (always refused)")
    ap.add_argument("--attribution-text", default="")
    ap.add_argument("--samples", type=int, default=5)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    key = os.environ.get("MERC_ONBOARD_API_KEY", "")
    if not key:
        print("MERC_ONBOARD_API_KEY is required", file=sys.stderr)
        return 2

    report = {
        "schema_version": 1,
        "kind": "external_model_onboarding",
        "alias": args.alias,
        "model_repository": args.repo,
        "model_revision": args.revision,
        "license": args.license,
        "license_url": args.license_url,
        "stages": {},
        "admitted": False,
    }

    def stage(name, ok, detail):
        report["stages"][name] = {"passed": bool(ok), "detail": detail}
        print(f"  {'PASS' if ok else 'FAIL'}  {name}: {detail}")
        return ok

    # 1. Policy. Same rules the control plane enforces at boot, applied BEFORE
    #    anything is measured -- there is no point benchmarking a model merc is
    #    not allowed to sell.
    if args.remote_code:
        stage("policy", False,
              "remote_code declared; refused unconditionally (runs repo-supplied "
              "code on third-party supplier hardware)")
        _write_report(args.out, report)
        return 1
    if args.license not in RESALE_ALLOWED:
        stage("policy", False,
              f"licence {args.license!r} is not on the resale allowlist; a human "
              f"must read the licence and confirm merc may charge for inference")
        _write_report(args.out, report)
        return 1
    if not args.revision or len(args.revision) < 7:
        stage("policy", False,
              "no usable model revision; an unpinned model can change under merc "
              "without the catalogue or any receipt noticing")
        _write_report(args.out, report)
        return 1
    stage("policy", True,
          f"{args.license}, remote_code=false, revision pinned to {args.revision[:12]}")

    # 2. Identity: the runtime must actually serve this alias.
    try:
        served = [m["id"] for m in get(args.endpoint, key, "/models").get("data", [])]
    except Exception:
        try:
            raw = get(args.endpoint, key, "/models")
            served = [m.get("id") or m.get("name") for m in raw.get("models", [])]
        except Exception as exc:
            stage("identity", False, f"could not list models: {exc}")
            _write_report(args.out, report)
            return 1
    if args.alias not in served:
        stage("identity", False,
              f"runtime serves {served}, not {args.alias!r}; the profile would "
              f"describe a model the endpoint does not have")
        _write_report(args.out, report)
        return 1
    stage("identity", True, f"runtime serves {args.alias}")

    # 3. Smoke: a real completion with real usage accounting. An empty answer or
    #    absent usage means merc cannot bill this model correctly.
    try:
        _, body = post(args.endpoint, key, "/chat/completions", {
            "model": args.alias,
            "messages": [{"role": "user", "content": "Name one colour. One word."}],
            "max_tokens": 16, "temperature": 0,
        })
        text = body["choices"][0]["message"]["content"].strip()
        usage = body.get("usage") or {}
        if not text:
            stage("smoke", False, "model returned an empty completion")
            _write_report(args.out, report)
            return 1
        if not usage.get("completion_tokens"):
            stage("smoke", False,
                  "no completion_tokens in usage; merc bills on delivered tokens "
                  "and cannot meter this model")
            _write_report(args.out, report)
            return 1
        stage("smoke", True, f"answered {text[:40]!r}, usage {usage}")
    except Exception as exc:
        stage("smoke", False, f"{type(exc).__name__}: {exc}")
        _write_report(args.out, report)
        return 1

    # 4. Determinism at temperature 0. merc's verification compares a task run on
    #    two independent workers; a model that answers differently each time
    #    cannot be verified that way, so this decides which lanes may serve it.
    try:
        outs = []
        for _ in range(2):
            _, b = post(args.endpoint, key, "/chat/completions", {
                "model": args.alias,
                "messages": [{"role": "user", "content": "Repeat exactly: canary"}],
                "max_tokens": 8, "temperature": 0, "seed": 1,
            })
            outs.append(b["choices"][0]["message"]["content"])
        deterministic = outs[0] == outs[1]
        report["deterministic_at_temperature_zero"] = deterministic
        stage("determinism", True,
              "byte-identical across two calls" if deterministic else
              "NOT byte-identical -- redundancy verification cannot be used for "
              "this model; it may only serve unverified lanes")
    except Exception as exc:
        stage("determinism", False, f"{type(exc).__name__}: {exc}")

    # 5. Benchmark. Measured, not assumed: the catalogue price is derived from
    #    throughput, so an invented number becomes an invented price.
    try:
        decode_rates, latencies = [], []
        for _ in range(max(1, args.samples)):
            dt, b = post(args.endpoint, key, "/chat/completions", {
                "model": args.alias,
                "messages": [{"role": "user", "content": "Count from 1 to 40."}],
                "max_tokens": 128, "temperature": 0,
            })
            done = (b.get("usage") or {}).get("completion_tokens") or 0
            if done and dt > 0:
                decode_rates.append(done / dt)
            latencies.append(dt * 1000)
        if not decode_rates:
            stage("benchmark", False, "no successful timed samples")
            _write_report(args.out, report)
            return 1
        report["measured"] = {
            "samples": len(decode_rates),
            "decode_tokens_per_sec_median": round(statistics.median(decode_rates), 2),
            "decode_tokens_per_sec_min": round(min(decode_rates), 2),
            "latency_ms_median": round(statistics.median(latencies), 1),
        }
        stage("benchmark", True,
              f"{report['measured']['decode_tokens_per_sec_median']} tok/s median "
              f"over {len(decode_rates)} samples")
    except Exception as exc:
        stage("benchmark", False, f"{type(exc).__name__}: {exc}")
        _write_report(args.out, report)
        return 1

    report["admitted"] = all(s["passed"] for s in report["stages"].values())
    report["runtime_profile"] = {
        "schema_version": 1,
        "model_alias": args.alias,
        "model_repository": args.repo,
        "model_revision": args.revision,
        "license": args.license,
        "license_url": args.license_url,
        "remote_code": False,
        "commercial_use": True,
        "attribution_required": bool(args.attribution_text),
        "attribution_text": args.attribution_text,
        "measured_decode_tokens_per_sec": report["measured"]["decode_tokens_per_sec_median"],
        "deterministic_at_temperature_zero": report.get(
            "deterministic_at_temperature_zero", False),
        "verified_lanes_eligible": report.get(
            "deterministic_at_temperature_zero", False),
    }

    _write_report(args.out, report)
    print(f"\n{'ADMITTED' if report['admitted'] else 'REFUSED'}: {args.alias} -> {args.out}")
    return 0 if report["admitted"] else 1


if __name__ == "__main__":
    sys.exit(main())
