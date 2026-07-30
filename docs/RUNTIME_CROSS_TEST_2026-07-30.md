# Four runtimes, one harness, two platforms

Measured 2026-07-30. Every row came from `merc-agent bench-batch`
(build `e9796eeb069ba4b0`), same prompt, `max_tokens=48`, 3 repetitions,
batch sizes 1/8/32/64. Receipts in `evidence/perf/runtime-benchmarks/`.

## Results

**Apple M3 Ultra (Metal)** — tokens/sec

| engine | serial | b8 | b32 | b64 | peak | vs serial | byte-determinism |
|---|---:|---:|---:|---:|---:|---:|---|
| candle | 214.2 | 427.8 | 501.0 | 495.6 | **501.0** | 2.34× | IDENTICAL everywhere |
| llama.cpp | 332.5 | 597.2 | 1553.5 | 2161.5 | **2161.5** | 6.50× | **DIVERGES at b≥8** |
| MLX | 311.7 | — | — | — | 311.7 | — | not measured |

**NVIDIA L4 (CUDA, RunPod)** — tokens/sec

| engine | serial | b8 | b32 | b64 | peak | vs serial | byte-determinism |
|---|---:|---:|---:|---:|---:|---:|---|
| vLLM | 74.4 | 446.1 | 1746.6 | 3078.4 | **3078.4** | 41.40× | IDENTICAL everywhere |

## What is and is not comparable

**Comparable:** engines within one platform on the same artifact. candle and
llama.cpp both served the exact pinned GGUF q4 (`b69aef11…`, sha256
`3f5a2242…`). That comparison is clean.

**Not comparable:** anything across the platform line, and MLX against either.

- L4 versus M3 Ultra is different silicon at a different price point.
- vLLM served HF bf16 weights, not the pinned GGUF q4. Different quantization.
- MLX cannot load the pinned GGUF, so it ran a different 4-bit build
  (`mlx-community/Llama-3.2-1B-Instruct-4bit`).

Reading "vLLM 3078 beats llama.cpp 2161" as a runtime result would be wrong
three times over. The cross-platform numbers are useful for *scaling shape*, not
for ranking engines.

## The finding that matters

**Non-determinism under batching is an engine property, not the price of
continuous batching.**

Last measurement raised the hypothesis that every high-throughput engine might be
incompatible with `byte_exact` verification, which would have argued for changing
the verification strategy. vLLM refutes it: **41.40× serial throughput with
byte-identical output at every batch size.** llama.cpp reached 6.50× and diverged
from its own serial output at every batch size tested.

So Merc should not relax `byte_exact`. It should prefer engines that stay
deterministic under batching, and that is now a governed refusal — a routable
profile serving a `byte_exact` cell whose receipt records non-determinism is
rejected at load.

Secondary: the L4 is the *slowest* serial engine measured (74.4 tok/s, a third of
Metal) and by far the best scaler (41.4× against Metal's best of 6.5×). Serial
throughput predicted nothing about batched throughput on any of the four.

## Two measurement artifacts, both mine, both recorded

1. **llama.cpp launched with `-np 8`** collapsed to 0.76× serial at batch 32 and
   looked far worse than candle. With `-np 64` it reached 6.50×. Server
   configuration, not engine behaviour.
2. **MLX batching is unmeasured, not absent.** `mlx_lm.server` is a
   single-threaded Python `http.server` and dropped the concurrent connections at
   batch 8; the failure was at the transport. Measuring MLX needs an in-process
   `batch_generate` harness or a concurrent server.

Both are the same lesson: the serving configuration dominated the apparent
engine result, twice, in opposite directions. Any future runtime tournament has
to pin and publish the server configuration alongside the number, or it is
measuring the operator.

Note: the harness prints a static line reading "openai_http is NOT marked
verified-work-capable: continuous batching is not byte-deterministic". vLLM
contradicts it. The note is a hardcoded assumption and should become a
per-measurement result.

## Cost

$0.03. NVIDIA L4 at $0.39/hr, torn down by the wrapper's own trap; balance
$17.07 → $17.04, no pods left running. RTX A5000 had no capacity on SECURE or
COMMUNITY, and 4090 none either — the wrapper walks a GPU list and bills nothing
for a combination that has no capacity.

## Lifecycle after this

Nothing moved. `candle_metal` stays ACTIVE, everything else stays non-routable.

| profile | lifecycle | why not routable |
|---|---|---|
| `candle_metal` | ACTIVE | — |
| `llama_cpp_metal` | VALIDATED | fails `byte_exact` under batching |
| `mlx_metal` | VALIDATED | batching unmeasured; different artifact |
| `vllm_cuda` | DRAFT | different artifact; no Merc chain; no CUDA supplier |

vLLM is the most interesting candidate on this evidence and the furthest from
routable: it needs the pinned artifact, a quality tier, and a complete
task→verification→money→receipt chain before any of this counts.
