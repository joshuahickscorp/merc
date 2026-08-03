#!/usr/bin/env python3
"""Quantization and speculative-decoding experiments (goal item 10).

Two questions, measured rather than assumed:

  1. What does quantization actually buy on this hardware, in throughput and in
     output agreement against the bf16 reference?
  2. Does speculative decoding help, and does it survive batching? The lane
     research claims it helps latency at batch 1 and *hurts* aggregate
     throughput at high batch. Aggregate throughput is what pays a supplier, so
     that claim decides whether it is worth anything here.

Emits JSONL alongside the main harness so every row carries the same labels.
"""

import argparse
import json
import sys
import time
from pathlib import Path

import mlx.core as mx

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "scripts"))
from lib.evidence_binding import (  # noqa: E402
    EvidenceBindingError,
    write_bound_jsonl_sidecar,
)
from mlx_lm import load
from mlx_lm.models.cache import make_prompt_cache

QUANTS = [
    ("mlx-community/Llama-3.2-1B-Instruct-4bit", "4bit"),
    ("mlx-community/Llama-3.2-1B-Instruct-8bit", "8bit"),
    ("mlx-community/Llama-3.2-1B-Instruct-bf16", "bf16"),
]

PROMPT = "Explain in one sentence why memory bandwidth limits transformer decoding."


def decode_throughput(model, batch: int, plen: int, gen: int) -> float:
    cache = make_prompt_cache(model)
    logits = model(mx.array([[1] * plen] * batch), cache=cache)
    mx.eval(logits)
    y = mx.argmax(logits[:, -1, :], axis=-1)[:, None]
    mx.eval(y)
    t0 = time.perf_counter()
    for _ in range(gen):
        logits = model(y, cache=cache)
        y = mx.argmax(logits[:, -1, :], axis=-1)[:, None]
    mx.eval(y)
    return batch * gen / (time.perf_counter() - t0)


def greedy_tokens(model, tok, prompt: str, n: int) -> list[int]:
    ids = mx.array([tok.encode(prompt)])
    cache = make_prompt_cache(model)
    logits = model(ids, cache=cache)
    mx.eval(logits)
    y = mx.argmax(logits[:, -1, :], axis=-1)[:, None]
    out = []
    for _ in range(n):
        out.append(int(y[0, 0]))
        logits = model(y, cache=cache)
        y = mx.argmax(logits[:, -1, :], axis=-1)[:, None]
    return out


def agreement(a: list[int], b: list[int]) -> float:
    if not a:
        return 0.0
    return sum(1 for x, y in zip(a, b) if x == y) / len(a)


def run_quantization(records: list, batches: tuple[int, ...]) -> None:
    reference = None
    for repo, label in QUANTS:
        try:
            model, tok = load(repo)
        except Exception as exc:  # noqa: BLE001
            records.append({"experiment": "quantization", "quant": label,
                            "error": f"{type(exc).__name__}: {exc}"})
            continue

        toks = greedy_tokens(model, tok, PROMPT, 48)
        if label == "bf16":
            reference = toks
        for b in batches:
            try:
                tps = decode_throughput(model, b, 128, 32)
                records.append({
                    "experiment": "quantization", "quant": label, "batch": b,
                    "decode_tokens_per_s": round(tps, 1),
                    "sample_tokens": toks[:12],
                })
            except Exception as exc:  # noqa: BLE001
                records.append({"experiment": "quantization", "quant": label,
                                "batch": b, "error": f"{type(exc).__name__}: {exc}"})
        del model

    # Agreement is only meaningful once bf16 has been seen.
    if reference is not None:
        for rec in records:
            if rec.get("experiment") == "quantization" and "sample_tokens" in rec:
                pass  # per-quant agreement computed in the second pass below


def run_quantization_agreement(records: list) -> None:
    """Second pass: greedy-token agreement of each quant against bf16."""
    ref = None
    per_quant = {}
    for repo, label in QUANTS:
        try:
            model, tok = load(repo)
            per_quant[label] = greedy_tokens(model, tok, PROMPT, 48)
            del model
        except Exception as exc:  # noqa: BLE001
            records.append({"experiment": "quality", "quant": label,
                            "error": f"{type(exc).__name__}: {exc}"})
    ref = per_quant.get("bf16")
    if ref is None:
        return
    for label, toks in per_quant.items():
        records.append({
            "experiment": "quality", "quant": label,
            "greedy_agreement_vs_bf16": round(agreement(ref, toks), 4),
            "compared_tokens": len(ref),
        })


def run_speculative(records: list) -> None:
    """Draft 4-bit proposes, 8-bit target verifies."""
    from mlx_lm.generate import speculative_generate_step, generate_step

    target, tok = load("mlx-community/Llama-3.2-1B-Instruct-8bit")
    draft, _ = load("mlx-community/Llama-3.2-1B-Instruct-4bit")
    prompt = mx.array(tok.encode(PROMPT))
    MAX = 128

    # Baseline: the same target model, no draft.
    t0 = time.perf_counter()
    n = 0
    for _tokens, _logprobs in generate_step(prompt, target, max_tokens=MAX):
        n += 1
    base_dt = time.perf_counter() - t0
    base_tps = n / base_dt
    records.append({"experiment": "speculative", "mode": "baseline_no_draft",
                    "batch": 1, "tokens": n, "decode_tokens_per_s": round(base_tps, 1)})

    for k in (2, 4, 6):
        try:
            t0 = time.perf_counter()
            produced = 0
            steps = 0
            for tokens, _lp, _from_draft in speculative_generate_step(
                prompt, target, draft, num_draft_tokens=k, max_tokens=MAX
            ):
                produced += 1
                steps += 1
            dt = time.perf_counter() - t0
            tps = produced / dt
            records.append({
                "experiment": "speculative", "mode": f"draft_k{k}", "batch": 1,
                "tokens": produced, "decode_tokens_per_s": round(tps, 1),
                "speedup_vs_baseline": round(tps / base_tps, 3),
            })
        except Exception as exc:  # noqa: BLE001
            records.append({"experiment": "speculative", "mode": f"draft_k{k}",
                            "batch": 1, "error": f"{type(exc).__name__}: {exc}"})

    # The decisive question: does it batch? Aggregate throughput is what pays a
    # supplier, so a technique that cannot batch cannot help the economics.
    try:
        batched_prompt = mx.array([tok.encode(PROMPT)] * 8)
        next(speculative_generate_step(batched_prompt, target, draft,
                                       num_draft_tokens=4, max_tokens=8))
        records.append({"experiment": "speculative", "mode": "batched_probe",
                        "batch": 8, "supported": True})
    except Exception as exc:  # noqa: BLE001
        records.append({"experiment": "speculative", "mode": "batched_probe",
                        "batch": 8, "supported": False,
                        "error": f"{type(exc).__name__}: {str(exc)[:160]}"})


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="evidence/bench/quant-spec.jsonl")
    ap.add_argument("--batches", default="1,64,256")
    args = ap.parse_args()
    batches = tuple(int(b) for b in args.batches.split(","))

    records: list = []
    run_quantization(records, batches)
    run_quantization_agreement(records)
    run_speculative(records)

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w") as fh:
        for r in records:
            r.setdefault("hardware", "Apple M3 Ultra / 96GB")
            r.setdefault("runtime", "mlx")
            fh.write(json.dumps(r) + "\n")
            print(json.dumps(r), flush=True)
    try:
        write_bound_jsonl_sidecar(
            out,
            harness="scripts/bench-quant-spec.py",
            repo_root=ROOT,
            build_binary_path=Path(__file__).resolve(),
            exact_config=f"batches={args.batches}",
            raw_samples=f"JSONL rows at {out.as_posix()}",
            model_na="model ids recorded per JSONL row; no single artifact digest",
            image_na="no container image in this measurement",
            corpus_na="no external corpus",
        )
    except EvidenceBindingError as exc:
        print(f"bench-quant-spec: REFUSED binding sidecar: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
