# Merc efficiency frontier

> **The 145× physical-inference claim is RETIRED PERMANENTLY.** It counted a
> shared prefix computed once as if inferred once per stream, inflating the
> figure 3.43×. The finding, the corrected benchmark and the accounting
> regression tests are preserved below and in `scripts/test-bench-accounting.py`,
> which runs in `make ci`.

## Three independent authorities

| authority | measures | today | ceiling |
|---|---|---:|---|
| **PHYSICAL EXACT-MODEL** | newly executed model work only | **48×** (6,646 t/s, 8-bit) | ~116× dense roofline |
| **DELIVERED-TOKEN** | logical buyer tokens, reuse labelled | 99× (13,749 t/s @ 2.29× reuse) | 300× target |
| **OUTCOME** | verified task completion | not yet measured | cascades, specialists |

`300×` is retained only as `DELIVERED_EFFECTIVE_THROUGHPUT` on a fixed eligible
workload, or as `OUTCOME_EQUIVALENT_COST_EFFICIENCY`. Neither is physical model
execution and neither may be described as such.

The ~116× dense roofline is a *generous* bound for the current exact-model
device profile, not an ordinary target. Physical optimisation continues until
measurement shows the practical ceiling.

## Authoritative benchmark rebased on quality-approved 8-bit

`bench/immutable/shared-prefix-v2.json` (sha256 `95341e33…`) supersedes v1,
which pinned 4-bit.

A representative suite — 12 prompts across 6 task classes, 64 greedy tokens each
(`evidence/bench/quality-suite.jsonl`) — replaces the single-prompt probe:

| quant | MODEL-EXACT agreement | OUTCOME correct | reference |
|---|---:|---:|---:|
| 4bit | 15.1% | 7/12 | 9/12 |
| **8bit** | 61.7% | **9/12** | 9/12 |

**8-bit is OUTCOME-EQUIVALENT**: it answers exactly what bf16 answers. 4-bit
loses 2 of 9 correct answers and is not approved. Neither is MODEL-EXACT — only
bf16 is, and any 8-bit claim must disclose the tier.

The single-prompt probe that showed 8-bit at 100% agreement was one lucky
prompt. It was not evidence.

There is also no speed/quality tradeoff to argue about: 8-bit measures **6,646
t/s** physical against 4-bit's 6,512. The approved model is the faster one.

## Original analysis



**Headline: the 145× figure I reported was inflated 3.43×. The honest physical
number is 47×. And 300× physical inference is not reachable on this hardware —
it requires more arithmetic than the GPU can perform.**

Both claims are measured, and the arithmetic is below so you can check it.

Benchmark frozen at `bench/immutable/shared-prefix-v1.json`
(sha256 `159ecb4b…`). Any speed claim must cite that id.

---

## 1. The 145× was an accounting error, and it was mine

The profile that produced 20,095 tok/s was batch 128, 192-token prompts of which
160 tokens were a shared prefix, 32 output tokens.

| | tokens |
|---|---:|
| billed (`batch × (prompt + output)`) | 28,672 |
| **physically computed** (`prefix×1 + batch×unique + batch×output`) | **8,352** |
| prefix tokens counted per-stream but computed **once** | 20,320 |

The shared prefix is prefilled a single time and its KV broadcast to all 128
streams. Counting it 128 times inflates throughput by **3.43×**. Your own
directive forbids exactly this — *"counting exact cache hits as newly inferred
tokens"* — and the harness I wrote was doing it.

Corrected, on the same recorded run: **5,854 tok/s physical = 42×**.

## 2. Prefix sharing does not make inference faster

Re-measuring all 27 profiles with the accounting separated makes this stark.
Physical throughput is essentially **flat** as prefix sharing increases:

| profile | billed t/s | **physical t/s** | inflation |
|---|---:|---:|---:|
| shared 160 / batch 128 | 18,469 | 5,380 | 3.43× |
| shared 128 / batch 64 | 13,294 | 5,816 | 2.29× |
| shared 64 / batch 128 | 8,561 | 6,134 | 1.40× |
| shared 32 / batch 128 | 7,319 | 6,282 | 1.17× |
| **no sharing, batch 64, 256-token prompt** | 6,512 | **6,512** | 1.00× |

The best *physical* result — **6,512 tok/s = 47×** — has **no prefix sharing at
all**.

This does not make prefix reuse worthless. It makes it a different thing than I
called it. Prefix reuse is **work elimination**: it delivers the same billed
tokens for one-third of the computation. That is real money — it cuts cost per
delivered token by 3.43× — but it is not speed, and it must never be reported as
speed.

## 3. 300× physical is beyond the hardware

Llama-3.2-1B forward pass ≈ 2 × 1.24e9 = 2.48 GFLOP per token.

| | tok/s | implied TFLOP/s |
|---|---:|---:|
| measured physical | 6,512 | **16.1** |
| 300× target | 41,610 | **103.2** |

Against plausible device peaks for a 60-core M3 Ultra:

| assumed peak | measured is | 300× would need |
|---|---:|---:|
| 21 TFLOPS | 77% of peak | **4.91× the whole device** |
| 28 TFLOPS | 58% of peak | **3.69× the whole device** |
| 40 TFLOPS (generous) | 40% of peak | **2.58× the whole device** |

Even at 40 TFLOPS with a physically impossible 100% utilisation, the ceiling is
**16,129 tok/s = 116×**.

So the remaining kernel headroom — fusion, mixed-bit, graph compilation, reduced
synchronisation, everything in §10 — is worth somewhere between **1.3× and
2.5×**, taking 47× to perhaps **60–116×**. That is a real and worthwhile
programme. It is not 300×.

This is the §24 exit condition (b): *a measured physical/economic ceiling is
proven*.

## 4. What 300× can honestly mean

Three separate numbers, never merged (§23):

| lane | today | ceiling | mechanism |
|---|---:|---:|---|
| **physical inference** | 47× | ~116× | kernels, quantization, batching |
| **delivered tokens** (billed) | 133× | 300×+ | prefix reuse, exact cache, dedup |
| **outcome cost** | — | unbounded | cascades, task-specific models |

**300× delivered tokens per second is reachable.** It comes from computing less,
not from computing faster: deeper prefix tries, exact-result reuse, in-flight
coalescing, cascades. The buyer pays for delivered tokens either way, so this is
economically real — it simply must be labelled `PREFIX_REUSED` /
`EXACT_RESULT_REUSE` rather than presented as inference throughput.

**300× physical requires different hardware.** Attributing that honestly means
reporting the hardware's contribution separately, which your directive already
requires.

## 5. Quality blocks the current profile regardless

The frozen benchmark pins the **4-bit** model, and 4-bit agrees with bf16 on
**2.1% of greedy tokens** (48-token probe, `evidence/bench/quant-spec.jsonl`).
8-bit agrees on 100% and costs 4.5% of batch-256 throughput.

Under §25 the 300× claim requires a *quality-approved model tier*. The current
benchmark is therefore **not quality-approved**, and the ladder should be
re-based on 8-bit before any figure is published. That costs ~4.5% and removes
the largest disclosure risk in the whole programme.

One prompt is a weak probe. It is enough to disqualify a headline number; it is
not a substitute for the §21 Team G evaluation suite.

## 6. Recommended re-basing

```text
Physical inference ladder (8-bit, quality-approved, no reuse counted)
  now      47×      6,512 tok/s      measured
  next     60×      8,300 tok/s      fused dequant+GEMM, compiled buckets
  stretch  90×     12,500 tok/s      mixed-bit + reduced synchronisation
  ceiling ~116×    16,100 tok/s      100% of a generous device peak

Delivered-token ladder (labelled as reuse, not speed)
  now     133×     18,469 tok/s      160/192 shared prefix
  next    200×     27,700 tok/s      prefix trie, canonicalised prompts
  target  300×     41,610 tok/s      + exact-result reuse, in-flight coalescing
```

The second ladder reaches 300×. The first cannot, on this box, and saying so now
is cheaper than discovering it after building the autotuner around a target that
the silicon forbids.

## 7. What was already built toward this directive

Landed and verified this session:

- **§4.5 prefix-aware routing** — `control/prefix_routing.go`, prefix identity as
  a domain-separated hash, `worker_prefix_state` warmth with a 90s TTL, ordered
  above model warmth in the claim query. Routing is advisory only: it can never
  override model compatibility, authorisation, price ceiling, region or deadline.
- **§2 benchmark classes** — INTERACTIVE / BATCH / SHARED_PREFIX_BATCH in
  `scripts/bench-harness.py`, now with physical/billed separation.
- **§16.1 energy** — harness samples `powermetrics` when run as root and records
  `power_source: UNAVAILABLE` with a reason otherwise. It never estimates watts.
- **§22 economic gates** — negative supplier or platform contribution is
  unpublishable. The former free-form environment “receipt” bypass was removed;
  durable payout subsidy funds may fund an existing liability but cannot create
  a loss-making catalogue promise.
- **§10 quantization + speculation evidence** — `evidence/bench/quant-spec.jsonl`.

Not built: the autotuner (§3), prefix trie (§4.2), multi-level cache (§4.3),
suffix/n-gram lanes (§5), batched MLX speculation (§7), trained speculators (§8),
runtime tournament (§9), kernel fusion (§10.2), disaggregation (§12), exact-work
elimination (§13), cascades (§14).
