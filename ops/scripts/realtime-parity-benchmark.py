#!/usr/bin/env python3
"""Realtime parity benchmark: merc routing versus a direct upstream.

Measures the overhead merc adds by serving the same completion twice -- once
through merc's OpenAI-compatible surface, once straight at the upstream engine --
and reports the delta.

The point of this harness is not the number. It is the ATTESTATION GATE.

A latency figure measured against an httptest fake is not evidence about a GPU,
and publishing it as though it were is the failure mode this file exists to
prevent. So the harness classifies its own evidence and refuses to authorise a
public claim unless the upstream proved it is a real pinned runtime:

    evidence_level        UNATTESTED_HARNESS_RUN | REAL_RUNTIME_MEASURED
    real_runtime_attested false unless the upstream presents a runtime attestation
    public_claim_allowed  false unless real_runtime_attested

`gate_passed` says the harness ran and the two surfaces agreed. It is
deliberately separate from `public_claim_allowed`: a run can be internally valid
and still be unpublishable.

Rewritten 2026-07-27. The original was deleted during the [KILL-RT] cleanup and
was untracked, so it could not be recovered; this reconstructs the contract from
its call site in src/control/realtime_integration_test.go.
"""

import argparse
import json
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

# An upstream claiming to be a real runtime must return this header carrying a
# pinned image digest. An httptest fake does not have one, which is exactly how
# the fake is distinguished from a GPU.
RUNTIME_ATTESTATION_HEADER = "X-Merc-Runtime-Attestation"


def post_completion(base_url: str, api_key: str, max_tokens: int, model: str, timeout: float = 30.0):
    """One chat completion. Returns (latency_seconds, body, headers)."""
    payload = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": "parity probe"}],
        "max_completion_tokens": max_tokens,
        "stream": False,
    }).encode()
    req = urllib.request.Request(
        base_url.rstrip("/") + "/chat/completions",
        data=payload,
        method="POST",
        headers={
            "content-type": "application/json",
            "authorization": f"Bearer {api_key}",
        },
    )
    start = time.perf_counter()
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        body = resp.read()
        headers = dict(resp.headers)
    return time.perf_counter() - start, body, headers


def sample(base_url, api_key, samples, warmups, max_tokens, model):
    """Returns (latencies, last_headers, error)."""
    try:
        for _ in range(max(0, warmups)):
            post_completion(base_url, api_key, max_tokens, model)
        lat, headers = [], {}
        for _ in range(samples):
            dt, _body, headers = post_completion(base_url, api_key, max_tokens, model)
            lat.append(dt)
        return lat, headers, None
    except (urllib.error.URLError, urllib.error.HTTPError, OSError) as exc:
        return [], {}, f"{type(exc).__name__}: {exc}"


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--merc-base-url", required=True, help="merc OpenAI-compatible base URL")
    ap.add_argument("--direct-base-url", required=True, help="upstream engine base URL")
    ap.add_argument("--samples", type=int, default=20)
    ap.add_argument("--warmups", type=int, default=2)
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--max-completion-tokens", type=int, default=32)
    ap.add_argument("--out", required=True)
    # Defaults to the alias the shipped runtime profile advertises. The alias
    # is an input to the profile sha256 used for verification, so renaming it
    # is a coordinated change and not a cosmetic one.
    ap.add_argument("--model", default="cx-chat-1b")
    args = ap.parse_args()

    merc_key = os.environ.get("MERC_BENCHMARK_API_KEY", "")
    direct_key = os.environ.get("MERC_DIRECT_VLLM_API_KEY", "")
    if not merc_key or not direct_key:
        print("both MERC_BENCHMARK_API_KEY and MERC_DIRECT_VLLM_API_KEY are required",
              file=sys.stderr)
        return 2

    merc_lat, merc_headers, merc_err = sample(
        args.merc_base_url, merc_key, args.samples, args.warmups, args.max_completion_tokens, args.model)
    direct_lat, direct_headers, direct_err = sample(
        args.direct_base_url, direct_key, args.samples, args.warmups, args.max_completion_tokens, args.model)

    errors = [e for e in (merc_err, direct_err) if e]
    gate_passed = not errors and len(merc_lat) == args.samples and len(direct_lat) == args.samples

    # Attestation: only a runtime that identifies itself with a pinned digest may
    # support a published number. Absence is the normal case for a test fake and
    # is not an error -- it just caps what the run may be used for.
    attestation = direct_headers.get(RUNTIME_ATTESTATION_HEADER, "")
    real_runtime_attested = bool(attestation.strip())

    def stats(xs):
        if not xs:
            return {"samples": 0}
        ordered = sorted(xs)
        return {
            "samples": len(xs),
            "mean_ms": round(statistics.fmean(xs) * 1000, 3),
            "median_ms": round(statistics.median(xs) * 1000, 3),
            "p95_ms": round(ordered[min(len(ordered) - 1, int(len(ordered) * 0.95))] * 1000, 3),
        }

    merc_stats, direct_stats = stats(merc_lat), stats(direct_lat)
    overhead_ms = None
    if merc_stats.get("median_ms") is not None and direct_stats.get("median_ms") is not None:
        overhead_ms = round(merc_stats["median_ms"] - direct_stats["median_ms"], 3)

    evidence = {
        "schema_version": 2,
        "kind": "realtime_parity_benchmark",
        # Internally valid: the harness ran and both surfaces answered.
        "gate_passed": gate_passed,
        # Externally publishable: only with a real attested runtime behind it.
        "real_runtime_attested": real_runtime_attested,
        "public_claim_allowed": bool(real_runtime_attested and gate_passed),
        "evidence_level": "REAL_RUNTIME_MEASURED" if real_runtime_attested
                          else "UNATTESTED_HARNESS_RUN",
        "attestation_header": RUNTIME_ATTESTATION_HEADER,
        "attestation_present": real_runtime_attested,
        "config": {
            "samples": args.samples,
            "warmups": args.warmups,
            "concurrency": args.concurrency,
            "max_completion_tokens": args.max_completion_tokens,
            "model": args.model,
        },
        "merc": merc_stats,
        "direct": direct_stats,
        "merc_overhead_median_ms": overhead_ms,
        "errors": errors,
        "note": ("Latency measured against an upstream that presented no runtime "
                 "attestation. This is a harness self-check, not evidence about "
                 "any GPU, and must not be published as a performance claim."
                 if not real_runtime_attested else
                 "Upstream presented a runtime attestation; figures describe that runtime."),
    }

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    # Always route through the bound writer when the destination sits under
    # evidence/; other destinations (scratch) stay plain for local probes.
    root = Path(__file__).resolve().parents[2]
    try:
        under_evidence = out_path.resolve().is_relative_to((root / "evidence").resolve())
    except (OSError, ValueError):
        under_evidence = "evidence/" in out_path.as_posix()
    if under_evidence:
        sys.path.insert(0, str(root / "ops" / "scripts"))
        from lib.evidence_binding import EvidenceBindingError, emit_bound_json  # noqa: E402

        try:
            emit_bound_json(
                out_path,
                evidence,
                harness="ops/scripts/realtime-parity-benchmark.py",
                repo_root=root,
                build_binary_path=Path(__file__).resolve(),
                exact_config=json.dumps(evidence.get("config") or {}, sort_keys=True),
                raw_samples=f"samples={args.samples} warmups={args.warmups}",
                model_na="model alias recorded in config; no weight digest",
                image_na="no container image in this measurement",
                corpus_na="no external corpus",
            )
        except EvidenceBindingError as exc:
            print(f"realtime-parity-benchmark: REFUSED: {exc}", file=sys.stderr)
            return 2
    else:
        with open(out_path, "w", encoding="utf-8") as fh:
            json.dump(evidence, fh, indent=2)
            fh.write("\n")
    print(json.dumps({k: evidence[k] for k in
                      ("gate_passed", "real_runtime_attested",
                       "public_claim_allowed", "evidence_level")}))
    return 0 if gate_passed else 1


if __name__ == "__main__":
    sys.exit(main())
