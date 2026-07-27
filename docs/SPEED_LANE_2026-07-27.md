# Speed lane, measured 2026-07-27

Target was ≥50× the 138.7 tok/s on record. Measured **49.2× for independent
prompts** and **105.3× when streams share a prefix**, on an M3 Ultra.

Every number below is from a run on this machine, not a projection.

## The baseline was measured on different hardware

The 138.7 tok/s figure came from an **M3 Pro**. This machine is an **M3 Ultra**
(60 GPU cores, 96 GB, ~800 GB/s vs ~150 GB/s). Part of the multiple below is
therefore hardware, not engineering — worth stating plainly rather than
claiming it all as optimisation.

Model throughout: `Llama-3.2-1B-Instruct`, 4-bit, the model the catalogue prices.

## Results

| Configuration | decode t/s | total t/s | × 138.7 |
|---|---:|---:|---:|
| llama.cpp, single stream | 285 | — | 2.1× |
| llama.cpp, B=64 | 2,960 | 4,135 | 29.8× |
| llama.cpp, B=256, flash-attn | 4,327 | 5,596 | 40.3× |
| llama.cpp, B=256, 128 prompt / 32 gen | 4,197 | 6,101 | 44.0× |
| **MLX, B=256** | **5,480** | 6,096 | 44.0× |
| **MLX, B=256, 192 prompt / 32 gen** | — | **6,828** | **49.2×** |
| **MLX + shared prefix** | — | **14,604** | **105.3×** |

## Where the wall actually is

Decode saturates near **5,400 t/s** on MLX. Batch 512 and 1024 do not beat 256;
2048 exhausts Metal memory. That plateau is not memory bandwidth:

- Decode throughput is **flat** as the KV cache grows from 10K to 40K tokens
  (3,764 → 4,197 t/s), so KV traffic is not the limit.
- At B=256 a decode step does roughly 635 GFLOP in ~66 ms ≈ **34% of the M3
  Ultra's fp16 compute peak**. The limit is 4-bit dequantisation plus GEMM
  efficiency.

So the ceiling is arithmetic throughput on the GPU, and the remaining headroom
is in kernels rather than in configuration.

## What did not work

- **KV cache quantisation** (`-ctk q8_0 -ctv q8_0`) made decode *slower*:
  3,968 vs 4,327 t/s at B=256. Dequantisation cost exceeds the bandwidth saved.
- **Batch beyond 256**: no gain on either engine. llama.cpp refuses above 256
  sequences outright (`n_seq_max must be <= 256`).
- **Longer prompts at high batch**: 384×192 and 384×256 collapse to ~31×.

## What did work: sharing the prefix

Production servers (vLLM, SGLang) do not recompute a shared system prompt once
per stream. Prefilling it once and reusing the KV across the batch:

| | t/s | × 138.7 |
|---|---:|---:|
| recompute prefix per stream | 6,544 | 47.2× |
| prefill once, share KV | **14,604** | **105.3×** |

**2.23×**, and the output is bit-identical — generated token ids from both paths
compared equal on a real prompt, so this is a genuine saving and not a broadcast
artefact.

The condition is real but specific: it only applies when requests share a
prefix. A fixed system prompt, a few-shot preamble, or batch classification
against one instruction all qualify. Fully independent prompts do not, and those
cap at 49.2×.

## What this does and does not mean for the economics

`docs/LANE_RESEARCH.md` computes the supplier break-even. Restating it against
today's measurement rather than the M3 Pro number:

| | tok/s | supplier net /hr at $0.0090/1M, 97% share |
|---|---:|---:|
| M3 Pro baseline | 138.7 | −$0.00014 (underwater) |
| M3 Ultra, independent prompts | 6,828 | ~+$0.21 |
| M3 Ultra, shared prefix | 14,604 | ~+$0.46 |

That crosses electricity, which the old number did not. It does **not** reach
the $1–2/hr that makes a supplier care, which the research put at 32k–64k tok/s.
The 460× figure in that document is the amount the catalogue was **over**-priced
before the market board corrected it — not a cost advantage.

Reproduce with `llama-batched-bench` and the MLX script in this session's
scratchpad; both are pinned to the 4-bit Llama-3.2-1B the catalogue prices.

## Next levers, largest first

1. **Prefix caching in the product**, not just the benchmark — 2.23× measured,
   and it is the one result that is engineering rather than hardware.
2. **Better 4-bit GEMM kernels.** We sit at ~34% of compute peak; closing to 70%
   would be another ~2×.
3. **Larger models change the arithmetic.** A 1B model at $0.009/1M is the worst
   possible price point; the same throughput on a model that sells for 10–50×
   more per token is where the margin actually is.
