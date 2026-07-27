#!/usr/bin/env python3
"""Quantization quality suite.

A single prompt was enough to disqualify 4-bit; it is not enough to approve
8-bit. This runs a representative prompt set across the task shapes merc
actually sells and reports, per quantization:

  - exact greedy-token agreement against the bf16 reference
  - mean first-divergence position (how far in before it drifts)
  - per-task-class agreement, so a quant that is fine for extraction but
    wrong for reasoning cannot hide behind an average

Greedy decoding with a fixed token budget, so any disagreement is the
quantization and not sampling noise.

    python3 scripts/quality-suite.py --out evidence/bench/quality-suite.jsonl
"""

import argparse
import json
import statistics
import sys
from pathlib import Path

REFERENCE = "mlx-community/Llama-3.2-1B-Instruct-bf16"
CANDIDATES = [
    ("mlx-community/Llama-3.2-1B-Instruct-4bit", "4bit"),
    ("mlx-community/Llama-3.2-1B-Instruct-8bit", "8bit"),
]

# Task classes chosen to span what the catalogue actually serves. Kept short so
# the whole suite is cheap enough to run on every quantization change.
# (task_class, prompt, expected_substrings_any) -- an empty expectation means the
# prompt is only usable for MODEL-EXACT agreement, not for outcome scoring.
PROMPTS = [
    ("extraction", "Extract the year and city as JSON from: The summit was held in Lisbon in 2019.", ["2019"]),
    ("extraction", "List only the numbers in this sentence, comma separated: I bought 3 apples, 12 pears and 7 plums.", ["3, 12, 7", "3,12,7"]),
    ("classification", "Classify the sentiment as positive, negative or neutral: The battery life is disappointing.", ["negative", "Negative"]),
    ("classification", "Is this question about biology, physics or history? Why do cells divide?", ["biology", "Biology"]),
    ("summarization", "Summarize in one sentence: Transformers decode one token at a time, and each step must read the model weights, which makes decoding memory-bound at small batch sizes.", ["memory"]),
    ("reasoning", "If a train leaves at 14:05 and arrives at 16:20, how long is the journey? Answer with the duration only.", ["2 hours 15", "2h15", "2 hours and 15", "135 minutes"]),
    ("reasoning", "A shirt costs 40 dollars after a 20 percent discount. What was the original price?", ["50"]),
    ("code", "Write a Python one-liner that returns the sum of squares of a list xs.", ["sum(", "**2", "*x"]),
    ("code", "What does this return? sorted([3,1,2], reverse=True)", ["3, 2, 1", "3,2,1"]),
    ("instruction", "Reply with exactly the word ACKNOWLEDGED and nothing else.", ["ACKNOWLEDGED"]),
    ("instruction", "Answer in one short sentence: why is the sky blue?", ["scatter", "Rayleigh", "wavelength"]),
    ("factual", "What is the capital of Australia? Answer with the city name only.", ["Canberra"]),
]

MAX_TOKENS = 64


def greedy(model, tok, prompt: str, n: int) -> list[int]:
    import mlx.core as mx
    from mlx_lm.models.cache import make_prompt_cache

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


def scores_correct(text: str, expect: list) -> bool:
    """Outcome check: did the answer contain any acceptable form?

    Deliberately permissive on wording and strict on content -- two models that
    phrase an answer differently are still both right, which is exactly the
    distinction token agreement cannot make.
    """
    if not expect:
        return False
    low = text.lower()
    return any(e.lower() in low for e in expect)


def first_divergence(a: list[int], b: list[int]) -> int:
    """Index of the first differing token, or len(a) when identical."""
    for i, (x, y) in enumerate(zip(a, b)):
        if x != y:
            return i
    return min(len(a), len(b))


def agreement(a: list[int], b: list[int]) -> float:
    if not a:
        return 0.0
    return sum(1 for x, y in zip(a, b) if x == y) / len(a)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="evidence/bench/quality-suite.jsonl")
    ap.add_argument("--max-tokens", type=int, default=MAX_TOKENS)
    args = ap.parse_args()

    try:
        from mlx_lm import load
    except ImportError as exc:
        print(json.dumps({"error": f"mlx not installed: {exc}"}), file=sys.stderr)
        return 2

    print(f"reference: {REFERENCE}", file=sys.stderr)
    ref_model, ref_tok = load(REFERENCE)
    reference = {}
    ref_correct = 0
    for idx, (cls, prompt, expect) in enumerate(PROMPTS):
        reference[idx] = greedy(ref_model, ref_tok, prompt, args.max_tokens)
        if scores_correct(ref_tok.decode(reference[idx]), expect):
            ref_correct += 1
    del ref_model
    print(f"reference outcome score: {ref_correct}/{len(PROMPTS)}", file=sys.stderr)

    records = []
    for repo, label in CANDIDATES:
        print(f"candidate: {label}", file=sys.stderr)
        model, tok = load(repo)
        per_class: dict[str, list[float]] = {}
        divergences = []
        exact = 0
        correct = 0
        for idx, (cls, prompt, expect) in enumerate(PROMPTS):
            got = greedy(model, tok, prompt, args.max_tokens)
            ok = scores_correct(tok.decode(got), expect)
            correct += 1 if ok else 0
            a = agreement(reference[idx], got)
            d = first_divergence(reference[idx], got)
            per_class.setdefault(cls, []).append(a)
            divergences.append(d)
            if a == 1.0:
                exact += 1
            records.append({
                "kind": "prompt", "quant": label, "task_class": cls, "prompt_index": idx,
                "agreement": round(a, 4), "first_divergence_token": d,
                "tokens_compared": args.max_tokens, "outcome_correct": ok,
            })
        del model

        overall = statistics.fmean([r["agreement"] for r in records
                                    if r["kind"] == "prompt" and r["quant"] == label])
        records.append({
            "kind": "summary", "quant": label,
            "reference": REFERENCE,
            "prompts": len(PROMPTS),
            "tokens_per_prompt": args.max_tokens,
            "mean_agreement": round(overall, 4),
            "exact_match_prompts": exact,
            "mean_first_divergence": round(statistics.fmean(divergences), 2),
            "worst_class": min(((c, statistics.fmean(v)) for c, v in per_class.items()),
                               key=lambda kv: kv[1])[0],
            "per_class_agreement": {c: round(statistics.fmean(v), 4) for c, v in per_class.items()},
            "outcome_correct": correct,
            "outcome_score": round(correct / len(PROMPTS), 4),
            "reference_outcome_correct": ref_correct,
        })

    out = Path(args.out)
    out.parent.mkdir(parents=True, exist_ok=True)
    with out.open("w") as fh:
        for r in records:
            fh.write(json.dumps(r) + "\n")

    print(f"\n{'quant':>6} {'MODEL-EXACT':>12} {'exact/N':>9} {'1st div':>8} "
          f"{'OUTCOME':>9} {'vs ref':>8}")
    for r in records:
        if r["kind"] != "summary":
            continue
        print(f"{r['quant']:>6} {r['mean_agreement']*100:11.1f}% "
              f"{r['exact_match_prompts']:>4}/{r['prompts']:<4} "
              f"{r['mean_first_divergence']:>8.1f} "
              f"{r['outcome_correct']:>4}/{r['prompts']:<4} "
              f"{r['reference_outcome_correct']:>4}/{r['prompts']:<4}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
