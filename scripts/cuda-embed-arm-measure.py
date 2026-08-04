#!/usr/bin/env python3
"""The CUDA embed arm: is vLLM on a rented GPU the same product as Metal, and
how fast is it on the one contract all three engines can be held to?

This is the third arm of the matched embed contract. Candle (safetensors, Metal)
and llama.cpp (F16 GGUF, Metal) were compared in
evidence/perf/selector/engine-parity-metal-embed-latest.json. vLLM/CUDA loads
the SAME safetensors bytes candle does -- sha256 53aa5117... pinned in
ops/placement-readiness-contract.json -- so the outcome comparison against
candle is a same-file comparison, not a same-family one.

Run ONLY as the workload of scripts/runpod-vllm.sh experiment, which owns the
cap, the lifetime bound, the teardown and the spend receipt. It reads
MERC_GPU_ENDPOINT / MERC_GPU_API_KEY from that parent and creates nothing.

The quality gate is the contract, not a footnote: cosine against the Metal
reference must clear 0.999 mean and 0.999 per row. A failed gate VOIDS the
timings rather than publishing them, because a faster engine that returns
different vectors is a different product and its latency is not comparable.

What it cannot do, by construction:

  - compare COST between Metal and CUDA. comparableHardwareFor refuses to mix
    hardware classes, and the readiness contract records that refusal as
    EXPLICIT_REFUSAL. The difference between an M3 Ultra and a rented A5000 is
    the machine, not the placement, and no receipt from this harness may imply
    otherwise.
  - promote anything. vllm-cuda-minilm-embed stays DRAFT and non-routable.
  - make a fleet claim. One pod, one GPU, one run.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import statistics
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))

WRITE_ENV = "MERC_WRITE_CUDA_EMBED_ARM"

# The verification contract, from control/embedding_comparator.go.
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
    if len(a) != len(b):
        raise ValueError(f"dimension mismatch: {len(a)} vs {len(b)}")
    num = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0 or nb == 0:
        raise ValueError("zero-norm embedding cannot be compared by cosine")
    return num / (na * nb)


def _get(base: str, key: str, path: str, timeout: float = 30.0) -> tuple[int, str]:
    req = urllib.request.Request(
        f"{base}{path}", headers={"Authorization": f"Bearer {key}"}, method="GET"
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")[:800]
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8", "replace")[:800]
    except Exception as exc:  # noqa: BLE001
        return 0, f"{type(exc).__name__}: {exc}"


def embed(base: str, key: str, model: str, texts: list[str], timeout: float = 120.0):
    body = json.dumps({"model": model, "input": texts}).encode()
    req = urllib.request.Request(
        f"{base}/embeddings",
        data=body,
        headers={"Content-Type": "application/json", "Authorization": f"Bearer {key}"},
        method="POST",
    )
    t0 = time.perf_counter()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as exc:
        # The server's own explanation is the diagnosis. Swallowing it turns a
        # one-line answer into another rented GPU: the first 403 from this
        # harness reported only "HTTP Error 403: Forbidden" and the pod was torn
        # down before anyone could ask why.
        detail = exc.read().decode("utf-8", "replace")[:1000]
        raise RuntimeError(
            f"POST {base}/embeddings failed HTTP {exc.code}: {detail}"
        ) from exc
    wall_ms = (time.perf_counter() - t0) * 1000.0
    doc = json.loads(raw)
    vectors = [d["embedding"] for d in sorted(doc["data"], key=lambda d: d["index"])]
    return vectors, wall_ms, doc.get("usage") or {}


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--reference", required=True, help="Metal candle embed artifact JSON")
    ap.add_argument("--corpus", required=True, help="JSON array of the corpus texts")
    ap.add_argument("--model", default="all-minilm-l6-v2")
    ap.add_argument("--warmup", type=int, default=5)
    ap.add_argument("--reps", type=int, default=40)
    ap.add_argument(
        "--out", default=str(ROOT / "evidence" / "perf" / "selector" / "cuda-embed-arm-latest.json")
    )
    args = ap.parse_args()

    base = os.environ.get("MERC_GPU_ENDPOINT", "")
    key = os.environ.get("MERC_GPU_API_KEY", "")
    pod_id = os.environ.get("MERC_RUNPOD_POD_ID", "")
    if not base or not key:
        print(
            "refusing: MERC_GPU_ENDPOINT / MERC_GPU_API_KEY are unset. This harness "
            "must run as the workload of scripts/runpod-vllm.sh experiment, which owns "
            "the cap, the teardown and the receipt.",
            file=sys.stderr,
        )
        return 2

    corpus = json.loads(Path(args.corpus).read_text())
    ref_doc = json.loads(Path(args.reference).read_text())
    reference = ref_doc["vectors"]
    if len(reference) != len(corpus):
        print("refusing: reference vector count does not match the corpus", file=sys.stderr)
        return 2

    started = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    # Say what the server thinks it is serving before asking it for anything.
    # Pod time is the expensive place to discover a name mismatch, and /v1/models
    # answers that in one request.
    models_status, models_body = _get(base, key, "/models")
    print(f"# GET /v1/models -> HTTP {models_status}: {models_body}", file=sys.stderr)
    served_names: list[str] = []
    if models_status == 200:
        try:
            served_names = [m["id"] for m in json.loads(models_body).get("data", [])]
        except Exception:  # noqa: BLE001
            pass
    if served_names and args.model not in served_names:
        print(
            f"# NOTE: requested model {args.model!r} is not in {served_names}; "
            f"using {served_names[0]!r} as the served name",
            file=sys.stderr,
        )
        args.model = served_names[0]

    # Warmup is not measured. A cold CUDA graph is not the steady state either
    # engine is being asked about.
    for _ in range(args.warmup):
        embed(base, key, args.model, corpus)

    samples: list[dict] = []
    last_vectors = None
    for i in range(args.reps):
        vectors, wall_ms, usage = embed(base, key, args.model, corpus)
        last_vectors = vectors
        samples.append(
            {
                "rep": i,
                "wall_ms": wall_ms,
                "ms_per_unit": wall_ms / len(corpus),
                "prompt_tokens": usage.get("prompt_tokens"),
            }
        )

    # Quality gate on the LAST measured response, against the Metal reference.
    per_row = [cosine(last_vectors[i], reference[i]) for i in range(len(corpus))]
    mean_cos = statistics.fmean(per_row)
    min_cos = min(per_row)
    passes = mean_cos >= MEAN_COSINE_GATE and min_cos >= ROW_COSINE_GATE

    ms = [s["ms_per_unit"] for s in samples]
    timing = {
        "n": len(ms),
        "ms_per_unit_p50": percentile(ms, 50),
        "ms_per_unit_p95": percentile(ms, 95),
        "ms_per_unit_p99": percentile(ms, 99),
        "ms_per_unit_mean": statistics.fmean(ms),
        "units_per_sec_p50": (1000.0 / percentile(ms, 50)) if percentile(ms, 50) > 0 else None,
    }

    art = {
        "schema_version": 1,
        "kind": "cuda_embed_arm_measurement",
        "label": "vLLM CUDA embed arm on the matched all-minilm-l6-v2 contract",
        "measured_at": started,
        "question": (
            "On the one contract candle, llama.cpp and vLLM can all be held to, does the "
            "CUDA arm return the same vectors as Metal, and at what latency?"
        ),
        "arm": {
            "cell_id": "vllm-cuda-minilm-embed",
            "runtime_id": "vllm_cuda",
            "engine": "vllm",
            "pod_id": pod_id,
            "endpoint_host": base.split("//")[-1].split("/")[0],
            "served_model": args.model,
            "lifecycle": "DRAFT — identity only, never routable, never advertised",
        },
        "model_identity": {
            "hf_repo": "sentence-transformers/all-MiniLM-L6-v2",
            "hf_revision": "1110a243fdf4706b3f48f1d95db1a4f5529b4d41",
            "model_artifact_path": "model.safetensors",
            "model_artifact_sha256": "53aa51172d142c89d9012cce15ae4d6cc0ca6895895114379cacb4fab128d9db",
            "same_file_as_candle_metal": True,
            "note": (
                "vLLM loads the same safetensors bytes candle does, so this is a same-file "
                "outcome comparison. llama.cpp's Metal arm loads the F16 GGUF and is a "
                "same-family, different-wire arm — see the placement readiness contract."
            ),
        },
        "corpus": {
            "sha256": "01fcad50dcc300f12f31f91d3ae356746382ef9961594715563958829bad59f9",
            "source": "EMBED_BENCH_CORPUS in agent/src/main.rs",
            "texts": len(corpus),
        },
        "reference": {
            "runtime": "candle_metal",
            "artifact": args.reference,
            "artifact_sha256": ref_doc.get("artifact_sha256"),
            "dim": ref_doc.get("dim"),
        },
        "quality": {
            "verification": "cosine",
            "mean_threshold": MEAN_COSINE_GATE,
            "row_threshold": ROW_COSINE_GATE,
            "mean_cosine": mean_cos,
            "min_cosine": min_cos,
            "per_text_cosine": per_row,
            "passes": passes,
            "consequence": (
                "a failed gate VOIDS the timings: a faster engine returning different "
                "vectors is a different product and its latency is not comparable"
            ),
        },
        "timing": timing if passes else None,
        "timing_void_reason": None
        if passes
        else f"cosine gate failed (mean {mean_cos:.9f}, min {min_cos:.9f} against {MEAN_COSINE_GATE})",
        "raw_samples": samples,
        "warmup_reps": args.warmup,
        "cost_comparison": {
            "status": "REFUSED_CROSS_HARDWARE",
            "authority": "control/runtime_cell_cost.go:comparableHardwareFor",
            "test": "control/runtime_cell_cost_test.go:TestComparableHardwareRefusesToMixMachines",
            "why": (
                "cost may not be compared across hardware classes: the difference between an "
                "M3 Ultra and a rented CUDA card is the machine, not the placement. The "
                "readiness contract records this as EXPLICIT_REFUSAL and this receipt does "
                "not work around it."
            ),
        },
        "can_prove": [
            "that vLLM/CUDA serves the matched embed contract at all, on the pinned image and revision",
            "cosine agreement (or disagreement) between the CUDA arm and the Metal candle reference on the same safetensors bytes",
            "CUDA ms/unit at p50/p95/p99 on this corpus and batch, when the quality gate passes",
        ],
        "does_not_prove": [
            "a cost comparison between Metal and CUDA — cross-hardware cost authority explicitly refuses one",
            "that vllm-cuda-minilm-embed may be routed or advertised; it stays DRAFT and non-routable",
            "a fleet claim: one pod, one GPU type, one run",
            "anything at another batch size, sequence length, model or precision",
            "that Merc selects between Metal and CUDA in production; ordinary admission remains a singleton",
        ],
        "limitations": [
            "Latency includes the RunPod HTTPS proxy and the public internet round trip; it is not a device-local kernel time and is not comparable to an in-process Metal measurement without saying so.",
            "One GPU type on secure cloud; no board-power measurement is taken here.",
            "The quality gate is evaluated on the final measured response, not on every rep.",
        ],
    }

    summary = {k: v for k, v in art.items() if k != "raw_samples"}
    print(json.dumps(summary, indent=2))

    out_path = Path(args.out)
    out_path.parent.mkdir(parents=True, exist_ok=True)
    if os.environ.get(WRITE_ENV, "") != "1":
        draft = out_path.with_suffix(".draft.json")
        draft.write_text(json.dumps(art, indent=2) + "\n")
        print(f"\n# not written (set {WRITE_ENV}=1 to seal)", file=sys.stderr)
        print(f"# draft: {draft}", file=sys.stderr)
        return 0 if passes else 5

    from lib.evidence_binding import (  # noqa: E402
        EvidenceBindingError,
        default_bound_identity,
        sha256_file,
        slot_value,
        write_bound_evidence,
    )

    harness_sha = sha256_file(Path(__file__))
    image = os.environ.get("MERC_VLLM_IMAGE", "")
    try:
        identity = default_bound_identity(
            ROOT,
            harness_revision=f"scripts/cuda-embed-arm-measure.py@{harness_sha[:16]}",
            build_binary_path=str(Path(__file__)),
            exact_config=(
                f"{WRITE_ENV}=1; reps={args.reps}; warmup={args.warmup}; "
                f"model={args.model}; pod={pod_id}; image={image}"
            ),
            raw_samples=f"reps={len(samples)}",
            model_na="model safetensors sha256 in receipt body model_identity",
            corpus_na="EMBED_BENCH_CORPUS sha256 in receipt body corpus",
        )
        identity["model_artifact_digest"] = slot_value(
            art["model_identity"]["model_artifact_sha256"]
        )
        identity["corpus_digest"] = slot_value(art["corpus"]["sha256"])
        if image:
            identity["image_digest"] = slot_value(image.split("@")[-1])
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
            build_binary_path=str(Path(__file__)),
        )
    except EvidenceBindingError as exc:
        print(f"write refused: {exc}", file=sys.stderr)
        return 4

    dated = out_path.with_name(
        f"cuda-embed-arm-{time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())}.json"
    )
    try:
        dated.write_text(out_path.read_text())
    except Exception:  # noqa: BLE001
        pass
    print(f"\n# BOUND: {out_path}", file=sys.stderr)
    return 0 if passes else 5


if __name__ == "__main__":
    raise SystemExit(main())
