# Runtime and performance

Merged runtime/perf surface (PLAN_300K L6).


<!-- source: docs/RUNTIME_MATRIX.md -->

# Runtime capability matrix

`control/runtime-authority.json` is the sole authority for admission,
advertisement, scheduling, the database model catalog, and agent dispatch. Go
embeds it and Rust includes the same bytes; both bind dispatch to its SHA-256.

Version: `2026-08-02.12`

| Workload | Model | Engine | Device | Hardware | Verification | Lifecycle / ordinary routable |
|---|---|---|---|---|---|---|
| `embed` | `all-minilm-l6-v2` | Candle | Metal | Apple Silicon base/pro/max/ultra | cosine | ACTIVE (routable — authority binds) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | ACTIVE (not ordinary-routable: receipt omits model artifact digest and cites profile `r3` while authority is `r9`) |
| `media_transcode` | `ffmpeg-transcode-v1` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | CANARY (not ordinary-routable: `merc_source_commit` is not a git object) |
| `media_rendering` | `svg-scene-render-v1` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | CANARY (not ordinary-routable: missing source commit and harness) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | vLLM | CUDA | nvidia 24/48/80gb | byte exact | DRAFT (not routable) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | SGLang | CUDA | nvidia 24/48/80gb | byte exact | DRAFT (not routable) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | TensorRT-LLM | CUDA | nvidia 24/48/80gb | byte exact | DRAFT (not routable) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | LMDeploy | CUDA | nvidia 24/48/80gb | byte exact | DRAFT (not routable) |
| `embed` | `all-minilm-l6-v2` | llama.cpp | Metal | Apple Silicon | cosine | REAL_RUNTIME_PROVEN (not ordinary-routable) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | MLX | Metal | Apple Silicon | byte exact | VALIDATED (not routable) |

These are exact cells, not a Cartesian product. Unknown job, model, engine,
device, or hardware values fail closed. A DRAFT/VALIDATED/REAL_RUNTIME_PROVEN
cell is visible and comparable but is not admitted to ordinary buyer placement
until the promotion ladder is cleared with measured evidence — a definition or
standalone benchmark never confers routability.

Ordinary buyer admission is a **singleton** today: exactly one advertised cell
per (job type, model) is frozen at classification time. Competing engines exist
as DRAFT or in the directed set; the shadow selector scores them but does not
route production traffic. Multi-candidate production selection requires the
engine tournament and is not this matrix's current behaviour.

A cell is routable only when its lifecycle is CANARY/ACTIVE **and** its
benchmark authority binds (receipt resolves, applicable identity fields are
present and valid — including `merc_source_commit` as a real git object — and
the authority has not been INVALIDATED, WITHDRAWN, or SUPERSEDED). Lifecycle
alone does not advertise a cell.

The serving-engine tournament harness lives in `control/serving_matrix.go`. It
runs the same corpus against any OpenAI-compatible engine under a documented
subset of the full concurrency × prompt × output × state × lane × precision
space, refuses mismatched model digests and sub-5× prompt counts as
incomparable, refuses unsupported points with a reason rather than skipping
them, and evaluates the budget gate at every claimed concurrency level.

CPU execution is a test fallback and is never advertised. Hardware identity is
currently self-declared; remote physical attestation is a named production
limitation.


<!-- source: docs/RUNTIME_CROSS_TEST_2026-07-30.md -->

# Four runtimes, one harness, two platforms — final

Measured 2026-07-30, re-run after fixing every defect found in the first pass.
All rows: `merc-agent bench-batch`, same prompt, `max_tokens=48`, **5 repetitions**,
batch 1/8/32/64. Receipts in `evidence/perf/runtime-benchmarks/`.

## Results

**Apple M3 Ultra (Metal)** — tokens/sec

| engine | serial | b8 | b32 | b64 | peak | scaling | byte-determinism |
|---|---:|---:|---:|---:|---:|---:|---|
| candle *(in-process, pinned q4)* | 225.9 | 439.6 | 502.6 | 509.9 | **509.9** | 2.26× | IDENTICAL |
| llama.cpp *(HTTP, np=64, pinned q4)* | 330.5 | 596.5 | 1538.7 | 2184.4 | **2184.4** | 6.61× | **DIVERGES b≥8** |
| MLX *(HTTP, mlx-community 4bit)* | 310.7 | 306.8 | 305.9 | 305.6 | **310.7** | 1.00× | IDENTICAL |

**CUDA (RunPod, vLLM, HF bf16)** — tokens/sec

| GPU | serial | b8 | b32 | b64 | peak | scaling | byte-determinism |
|---|---:|---:|---:|---:|---:|---:|---|
| NVIDIA L4 | 74.4 | 446.1 | 1746.6 | 3078.4 | **3078.4** | 41.40× | IDENTICAL |
| NVIDIA A40 | 151.6 | 989.0 | 3011.2 | 4677.2 | **4677.2** | 30.84× | IDENTICAL |

## Verdict

**Non-determinism under batching is an engine property, and llama.cpp is the
only engine of four that has it.**

The first pass raised the hypothesis that high throughput might be inherently
incompatible with `byte_exact` verification, which would have argued for
weakening the verification strategy. That is now refuted on **two independent
NVIDIA cards**: vLLM reached 41.4× and 30.8× serial with byte-identical output
at every batch size. candle batches 2.26× and stays identical. Only llama.cpp
diverges — and it is the fastest engine on Metal.

So `byte_exact` stays. The rule already enforced at load — a routable profile
serving a `byte_exact` cell whose receipt records non-determinism is refused —
is the right rule, and llama.cpp is exactly the case it was written for.

**On Metal, candle remains correct for byte_exact cells.** Not because it is
fastest (it is 4.3× slower than llama.cpp at peak) but because it is the only
Metal engine that both batches and stays deterministic. MLX does not batch at
all; llama.cpp batches and diverges.

**MLX does not continuous-batch through `mlx_lm.server`.** Flat at ~306 tok/s
from batch 1 to 64, CV under 0.6%. This is a measured negative, not a failure to
measure.

## What is comparable

**Clean:** candle vs llama.cpp. Both served the exact pinned artifact
(`b69aef11…`, sha256 `3f5a2242…`).

**Confounded, do not rank across these lines:**

- L4/A40 vs M3 Ultra — different silicon, different price.
- vLLM served HF **bf16**, not the pinned GGUF q4. **Unfixed** — see below.
- MLX cannot load the pinned GGUF and ran a different 4-bit build.
- candle runs **in-process**; the other three go over HTTP. Serial numbers carry
  loopback overhead the candle row does not.

Serial throughput predicted nothing about batched throughput on any engine. The
L4 is the slowest serial engine measured and the best scaler.

## Fixed since the first pass

1. **The agent could not start.** Schema v2 replaced the single `runtime` object
   with `runtimes[]`, and `agent/src/runtime_authority.rs` still expected v1 —
   it panicked at load with `missing field 'runtime'`. The Go suite never caught
   it because it does not build the Rust agent. The agent now projects only
   routable profiles, matching the control plane; 106 agent tests green. This was
   a latent production defect, not a benchmark issue.
2. **MLX batching, previously unmeasured.** The first diagnosis (single-threaded
   server) was wrong: `mlx_lm` already uses `ThreadingHTTPServer`. The real cause
   was the 5-connection listen backlog, so the 6th simultaneous connection was
   refused by the kernel. Raised to 256 and it measures cleanly.
3. **The harness asserted the answer.** It printed a hardcoded note claiming
   continuous batching is not byte-deterministic. vLLM disproved it. Determinism
   is now reported per backend from measurement.
4. **The RunPod wrapper reported success on a failed provision.** No capacity →
   it fell through to a stale `.merc-runpod.env`, benchmarked a dead pod, got
   404, exited 0. It now deletes that file and requires `up` to succeed.
5. **3 reps → 5** on every run.

## Transport confound — now bounded, not hand-waved

candle runs in-process; the other three go over HTTP loopback. Measured directly
against llama-server on this machine:

| measurement | median | share of a 48-token request |
|---|---:|---:|
| `GET /v1/models` round trip (n=200) | 0.223 ms | **0.13%** |
| 1-token completion (transport + prefill + 1 decode) | 6.53 ms | 3.7% |
| 48-token completion | 178.21 ms | — |

Pure HTTP transport is **0.13%** of a benchmark request. It does not move any
conclusion here, and the in-process candle row is comparable to the HTTP rows
within that bound.

## Not fixed: vLLM ran HF bf16, not the pinned GGUF q4

Attempted and failed. GGUF under vLLM needs `--model` to be a local file, so the
pod needs a download-then-serve command, which needs a `dockerEntrypoint`
override. Two attempts (A40, L4) served **HTTP 404 for 900 s** and were torn
down without ever answering. Cost $0.08 for nothing.

The cause was not isolated. Either the entrypoint override replaces RunPod's
in-container agent so the HTTP proxy never wires to port 8000 — which
`runpod-create-payload.py` explicitly warned about and which I dismissed on the
grounds that readiness no longer polls `pod.runtime` — or the download-then-serve
command failed inside the container. 404 rather than connection-refused says the
proxy was reachable with nothing behind it, which leans toward the first, and
toward the warning having been right.

Distinguishing them needs container logs the script cannot fetch, at the cost of
another pod. Stopped there rather than guessing at money per attempt. GGUF mode
is left in the code, disabled by default, with the failure recorded next to it.

The determinism finding is independent of quantization and reproduced on two
cards, so this gap does not touch the verdict.

## Cost

**$0.16 total**, balance $17.07 → $16.91, no pods left running.

| run | GPU | cost | outcome |
|---|---|---:|---|
| bf16 | L4 | $0.03 | measured |
| bf16 | A40 | $0.05 | measured |
| GGUF | A40 | ~$0.05 | 404, torn down |
| GGUF | L4 | ~$0.03 | 404, killed |

A GPU/cloud combination with no capacity bills nothing; A5000 and 4090 were
refused repeatedly.

### A teardown that lied

The GGUF run logged "pod torn down by the exit trap" and left the A40 **running
at $0.44/hr**, alongside a second pod. `terminate()` discarded the DELETE result
with `|| true` and printed "terminated" unconditionally, so a failed teardown
reported success. Both pods were killed manually by `down-all`.

`terminate()` now retries three times and **verifies the pod is gone** before
claiming it, and shouts the recovery command if it cannot. Announcing a teardown
that did not happen is worse than a noisy failure, because nobody goes looking.

## Lifecycle after this

Nothing moved.

| profile | lifecycle | why not routable |
|---|---|---|
| `candle_metal` | ACTIVE | — |
| `llama_cpp_metal` | VALIDATED | fails `byte_exact` under batching, confirmed at 5 reps |
| `mlx_metal` | VALIDATED | does not batch; different artifact |
| `vllm_cuda` | DRAFT | different artifact; no Merc chain; no CUDA supplier |

## Determinism sweep: is llama.cpp's divergence a policy or a kernel?

The previous receipt left this open: does a llama.cpp configuration exist on
Metal that is both batched and byte-deterministic? Swept, 3 reps each, same
harness and artifact.

| configuration | peak tok/s | × serial | byte-determinism |
|---|---:|---:|---|
| continuous batching, `-np 64` | 2184.4 | 6.61× | DIVERGES |
| continuous batching **disabled**, `-np 64` | 936.3 | 2.78× | DIVERGES |
| single-chunk prefill, `-ub 2048` | 1554.4 | 4.68× | DIVERGES |
| **serialised, `-np 1`** | 341.3 | 1.02× | **IDENTICAL** |

**No.** llama.cpp on Metal reproduces its own serial output only when serialised
to one slot. Disabling continuous batching does not restore determinism, and
neither does single-chunk prefill — so the divergence lives in the **batched
kernel path**, not in the scheduling policy. That is a stronger and more useful
statement than "continuous batching is non-deterministic", which is what the
first pass would have concluded.

### This inverts the second-runtime choice

For a `byte_exact` cell, `llama_cpp_metal` is **dominated** by `candle_metal`:

| | deterministic throughput | × serial |
|---|---:|---:|
| candle_metal | 509.9 | 2.26× |
| llama_cpp_metal (`-np 1`, its only deterministic setting) | 341.3 | 1.02× |

Not a speed-versus-verification trade-off. The challenger is **strictly worse at
the only setting where it is usable**.

The "least new code" heuristic picked llama.cpp because its binaries were already
installed and it loads the pinned artifact. That optimised the wrong thing: the
cheapest runtime to wire is one that cannot serve the cell it would be wired for.
On this evidence the second runtime for `byte_exact` work should be **vLLM**,
which is the only measured engine that is both fast and byte-deterministic —
at the cost of CUDA hardware and a supplier that does not exist yet.

llama.cpp is not disqualified everywhere: the `embed` cell verifies by `cosine`,
not byte identity, and nothing here speaks against it there.

## Can llama.cpp serve the *embed* cell instead?

`byte_exact` disqualifies llama.cpp on Metal. The `embed` cell verifies by
`cosine` against `embeddingCosineThreshold = 0.999`
(`control/verification.go:520`) — a threshold calibrated for two runs of the
same implementation. Whether a different engine clears it was open.

Measured: llama.cpp F16 GGUF embeddings against the pinned F32 safetensors
reference, 6 texts, 384-dim, normalized.

| | value |
|---|---:|
| mean cosine | **0.999999** |
| min cosine | 0.999999 |
| gate | 0.999 |

Clears it with roughly a thousandfold margin on the allowed tolerance. **The
picture is cell-specific, not engine-wide**: llama.cpp is disqualified for
`byte_exact` work on Metal and comfortably qualified for cosine-verified
embeddings.

### The next blocker is structural, not configuration

`wire_kind` is declared **globally per model**. `all-minilm-l6-v2` is `hf`
because candle serves safetensors; llama.cpp needs the GGUF of the same logical
model. A cell cannot currently say "this runtime serves this model from a
different artifact format", and `validateAdvertisedRuntimeCatalogRows` actively
refuses conflicting wire kinds for one model — correctly, under the current
single-runtime assumption.

So registering a llama.cpp embed cell requires artifact format to move from the
**model** to the **(runtime, model)** pair. That is a schema change to the
authority document, and it is the honest next step for Lane B rather than
something to work around.

Not established: embedding throughput, any Merc chain, and whether quantizations
below F16 still clear 0.999.


<!-- source: docs/SPEED_LANE_2026-07-27.md -->

# Speed lane, measured 2026-07-27

> **Historical planning snapshot — superseded.** These MLX / llama.cpp multiples
> rest on unbound bench logs and must not be labelled as today's physical or
> delivered performance under bound identity. The 145× accounting error retired
> in `docs/FRONTIER_300X.md` is the same chain. Use `docs/SHIPPABILITY_STATUS.md`
> and bound receipts under `evidence/` for current claims. This file is retained
> as an audit trail, not a live speed claim. (Banner pattern matches
> `docs/PATH_TO_TEN.md`.)

Target was ≥50× the 138.7 tok/s on record. The table below records what an
unbound 2026-07-27 harness reported on an M3 Ultra; those figures are historical
only.

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


<!-- source: docs/BENCH_PROFILES_2026-07-27.md -->

# Benchmark profiles

> **Historical planning snapshot — superseded.** These MLX profile tables rest
> on the same unbound bench chain as the retired speed-lane logs. They are not
> bound at this commit and must not be labelled as today's physical or delivered
> multiples. Use `docs/SHIPPABILITY_STATUS.md` and bound receipts under
> `evidence/` for current claims. This file is retained as an audit trail, not
> an operator performance reference. (Banner pattern matches `docs/PATH_TO_TEN.md`.)

- hardware: **Apple M3 Ultra / 96GB**
- runtime: **mlx**
- model: **mlx-community/Llama-3.2-1B-Instruct-4bit**
- quality: greedy argmax decoding, no sampling; outputs not scored for quality
- energy: **NOT MEASURED - powermetrics requires root; re-run under sudo for energy**
- market price: $0.00003600/1k units (confidence-weighted median x positioning multiplier)
- supplier share: 97%, electricity $0.15/kWh
- **energy columns are null by design**: this harness does not estimate watts and present them as measurement.

## INTERACTIVE

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 1 | 32 | 64 | COLD | 30 | 2.68 | 476 | 476 | 1.00x | n/a | $0.00000271 | $0.00000279 | $0.0553* | 0 |
| 1 | 128 | 64 | COLD | 62 | 2.98 | 760 | 760 | 1.00x | n/a | $0.00000169 | $0.00000175 | $0.0911* | 0 |
| 1 | 128 | 64 | COLD | 25 | 2.68 | 978 | 978 | 1.00x | n/a | $0.00000132 | $0.00000136 | $0.1184* | 0 |
| 1 | 512 | 64 | COLD | 81 | 2.85 | 2185 | 2185 | 1.00x | n/a | $0.00000059 | $0.00000061 | $0.2702* | 0 |
| 1 | 2048 | 64 | COLD | 312 | 3.27 | 4045 | 4045 | 1.00x | n/a | $0.00000032 | $0.00000033 | $0.5040* | 0 |

## BATCH

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 1 | 128 | 32 | COLD | 43 | 2.71 | 1235 | 1235 | 1.00x | n/a | $0.00000104 | $0.00000108 | $0.1508* | 0 |
| 8 | 128 | 32 | COLD | 153 | 5.22 | 3993 | 3993 | 1.00x | n/a | $0.00000032 | $0.00000033 | $0.4974* | 0 |
| 32 | 128 | 32 | COLD | 585 | 9.33 | 5797 | 5797 | 1.00x | n/a | $0.00000022 | $0.00000023 | $0.7243* | 0 |
| 64 | 64 | 16 | COLD | 603 | 13.82 | 6215 | 6215 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7768* | 0 |
| 64 | 64 | 64 | COLD | 584 | 13.74 | 5596 | 5596 | 1.00x | n/a | $0.00000023 | $0.00000024 | $0.6990* | 0 |
| 64 | 64 | 256 | COLD | 588 | 14.26 | 4832 | 4832 | 1.00x | n/a | $0.00000027 | $0.00000027 | $0.6029* | 0 |
| 64 | 128 | 32 | COLD | 1210 | 14.04 | 6171 | 6171 | 1.00x | n/a | $0.00000021 | $0.00000022 | $0.7713* | 0 |
| 64 | 128 | 32 | COLD | 1204 | 13.87 | 6216 | 6216 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7769* | 0 |
| 64 | 256 | 16 | COLD | 2412 | 16.30 | 6512 | 6512 | 1.00x | n/a | $0.00000020 | $0.00000020 | $0.8142* | 0 |
| 64 | 256 | 64 | COLD | 2341 | 14.99 | 6205 | 6205 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7755* | 0 |
| 64 | 256 | 256 | COLD | 2357 | 15.04 | 5279 | 5279 | 1.00x | n/a | $0.00000024 | $0.00000025 | $0.6592* | 0 |
| 64 | 1024 | 16 | COLD | 10041 | 30.72 | 6319 | 6319 | 1.00x | n/a | $0.00000020 | $0.00000021 | $0.7899* | 0 |
| 64 | 1024 | 64 | COLD | 9735 | 22.21 | 6241 | 6241 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7801* | 0 |
| 64 | 1024 | 256 | COLD | 9718 | 20.59 | 5465 | 5465 | 1.00x | n/a | $0.00000024 | $0.00000024 | $0.6825* | 0 |
| 128 | 128 | 32 | COLD | 2517 | 25.45 | 6148 | 6148 | 1.00x | n/a | $0.00000021 | $0.00000022 | $0.7684* | 0 |
| 256 | 128 | 32 | COLD | 5056 | 48.80 | 6189 | 6189 | 1.00x | n/a | $0.00000021 | $0.00000021 | $0.7736* | 0 |

## SHARED_PREFIX_BATCH

| batch | prompt | out | prefix | TTFT ms | ITL ms | PHYSICAL tok/s | delivered tok/s | reuse | J / M tok | supplier floor /1k | CX floor /1k | contribution $/hr | err |
|---:|---:|---:|:--|---:|---:|---:|---:|---:|---:|---:|---:|---:|:--|
| 64 | 192 | 32 | SHARED(128) | 622 | 14.26 | 5816 | 13294 | 2.29x | n/a | $0.00000022 | $0.00000023 | $0.7266* | 0 |
| 128 | 192 | 32 | COLD | 3669 | 25.95 | 6372 | 6372 | 1.00x | n/a | $0.00000020 | $0.00000021 | $0.7965* | 0 |
| 128 | 192 | 32 | SHARED(32) | 3085 | 26.02 | 6282 | 7319 | 1.17x | n/a | $0.00000021 | $0.00000021 | $0.7852* | 0 |
| 128 | 192 | 32 | SHARED(64) | 2506 | 26.35 | 6134 | 8561 | 1.40x | n/a | $0.00000021 | $0.00000022 | $0.7666* | 0 |
| 128 | 192 | 32 | SHARED(128) | 1415 | 26.54 | 5482 | 12660 | 2.31x | n/a | $0.00000024 | $0.00000024 | $0.6847* | 0 |
| 128 | 192 | 32 | SHARED(160) | 736 | 25.51 | 5380 | 18469 | 3.43x | n/a | $0.00000024 | $0.00000025 | $0.6718* | 0 |

`*` contribution derived from the documented 30 W figure, **not** a measurement. Re-run under `sudo` for real energy.

## Best observed profile

**6512 tok/s** PHYSICAL (6512 delivered, 1.00x reuse) - BATCH, batch 64, 256 prompt / 16 output, prefix COLD, mlx on Apple M3 Ultra / 96GB.

At the market price that is $0.8142/hr of supplier contribution after electricity (assumed power).


<!-- source: docs/FRONTIER_300X.md -->

# Merc efficiency frontier

> **The 145× claim remains retired.** It counted a shared prefix computed once
> as if inferred once per stream, inflating the figure 3.43×. The finding, the
> corrected benchmark and the accounting regression tests are preserved below
> and in `scripts/test-bench-accounting.py`, which runs in `make ci`.
>
> **Current physical and delivered multiples are unknown under bound identity.**
> Historical unbound MLX logs and the tables below must not be labelled as
> today's numbers. They are retained as the audit trail of a measurement error
> and its corrected accounting, not as a live performance claim at this commit.

## Three independent authorities

| authority | measures | today (bound identity) | ceiling (historical dense bound) |
|---|---|---:|---|
| **PHYSICAL EXACT-MODEL** | newly executed model work only | **unknown** — no bound receipt | ~116× dense roofline (historical) |
| **DELIVERED-TOKEN** | logical buyer tokens, reuse labelled | **unknown** — no bound receipt | 300× target (programme aspiration) |
| **OUTCOME** | verified task completion | not yet measured | cascades, specialists |

`300×` is retained only as `DELIVERED_EFFECTIVE_THROUGHPUT` on a fixed eligible
workload, or as `OUTCOME_EQUIVALENT_COST_EFFICIENCY`. Neither is physical model
execution and neither may be described as such.

The ~116× dense roofline is a *generous* bound for the current exact-model
device profile, not an ordinary target. Physical optimisation continues until
measurement shows the practical ceiling.

## Authoritative benchmark rebased on quality-approved 8-bit

`evidence/immutable-fixtures/shared-prefix-v2.json` (sha256 `95341e33…`) supersedes v1,
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

A historical unbound suite once reported 8-bit at **6,646 t/s** physical against
4-bit's 6,512 on that same unattested run. Those figures are not bound at this
commit and are not today's numbers; quality approval of 8-bit over 4-bit still
stands on the outcome table above, not on a live speed claim.

## Original analysis



**Headline: the 145× figure I reported was inflated 3.43×. The honest physical
number is 47×. And 300× physical inference is not reachable on this hardware —
it requires more arithmetic than the GPU can perform.**

Both claims are measured, and the arithmetic is below so you can check it.

Benchmark frozen at `evidence/immutable-fixtures/shared-prefix-v1.json`
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


<!-- source: docs/EXECUTION_FRONTIER_LANE.md -->

# Execution frontier lane

Branch: `perf/execution-frontier`. `release/rc1-go-closure` stays frozen.

This maps the ten-step faster/cheaper order onto what the tree actually
contains, so no step is started against a wall that was already visible.
Everything below was probed against the code on 2026-07-30, not inferred from
intent. `docs/FRONTIER_300X.md` and `docs/SPEED_LANE_2026-07-27.md` hold the
measured throughput authorities and are not restated here.

No global refactor. The boundaries the Execution Brain needs mostly exist
already under different names, listed per step.

## 1. Calibrated compute planning — MEASURING

Existing: `control/compute_plan.go` freezes the geometry; `eta_calibration` plus
`Store.ETABiasFactor` (`control/store.go:609`) close the loop for duration only,
one-sided and clamped so a fast window cannot shrink an SLA promise.

Added this lane: `control/plan_actuals.go` records predicted-versus-realized for
`output_tokens`, `task_count`, `task_attempts` and `compute_usd` at finalize,
scoped by (job_type, tier, model_ref, input_depth_band) plus the runtime and
hardware class that executed the job. `GET /admin/plan-accuracy` reports median
and p90 ratio and MAPE per bucket, untrusted below `driftMinSamples`.

Observed-only by construction. No money, reserve, pricing or admission path
reads it. Promoting an estimator from this evidence is a separate change that
must state its own quality and money effect.

Not recorded, and why:

| quantity | blocker |
|---|---|
| peak VRAM / KV growth | `worker_memory_samples` is host available/effective GB per worker. `minimum_memory_gb` is an admission floor, not a predicted usage. Needs worker-side per-job device telemetry. |
| storage / transfer bytes | `jobs.economic_output_bytes` is realized, but no predicted byte count is frozen in the plan to compare against. |
| duration | already owned by `eta_calibration`. Two learners over one quantity is a bug. |

Truth boundary (`control/plan_actuals.go`, `control/plan_calibration.go`):

- `observation_class` is `PRIMARY_EXECUTION`, `CACHE_HIT` or `SYNTHETIC_TEST`.
  Only the first trains ordinary planning; the others are labelled rather than
  dropped so reuse and fixture coverage stay visible.
- A hedged chunk counts once. Summing an original and its dynamic copy inflated
  realized output by the hedge rate, which made the ceiling estimator look most
  accurate exactly when the fleet was struggling enough to hedge.
- Only terminal `complete` jobs are recorded.
- `ResolvePlanCalibration` walks exact → runtime+model+depth → model+depth →
  model → workload_class → global and names the level and sample count it landed
  on. An empty scope field is skipped, never matched as a wildcard.
- `CalibrationPromotable` gates any use: 100-sample floor, MAPE ≤ 35%,
  p90/median ≤ 2.0, no active drift alarm, named revision, shadow window that
  measurably improved, receipt digest. `AffectsMoney` is refused outright.
- `TestCalibrationIsUnreachableFromMoneyAndAdmissionPaths` enforces the
  separation in the build: 22 money/pricing/settlement/admission files may not
  reference calibration, and nothing outside a 6-file allowlist may either.

Next: enough finalized jobs to fill a trusted bucket, then argue the first
estimator change from the measured band through the promotion gate.

The project compiler now has a durable buyer-scoped evidence boundary in
addition to the upload response: `POST /v1/projects/compile` writes an
append-only proposal or bounded-probe receipt, and
`GET /v1/projects/compile/{id}` returns the same IR after revalidating the
project/artifact and IR digests. Probe receipts require a prior proposal
receipt from the same buyer; neither state creates a quote, reserves capacity,
or executes work. Calibration and execution remain explicitly refused.

Render decomposition is now a buyer-scoped production read rather than a
test-only helper: `GET /v1/projects/compile/{id}/render/{step}/units/{ordinal}`
expands one bounded frame/camera/tile/sample ordinal from that immutable receipt.
The response is `DECOMPOSITION_ONLY_NOT_EXECUTABLE` and carries the unresolved
asset-locality, runtime, worker, assembly, verification, and settlement refusal;
it does not create a task, reserve capacity, or move money. A real render runtime
and deterministic assembly/settlement path are still required before this lane
can claim execution capability. The buyer-scoped `POST
/v1/projects/compile/{id}/render/{step}/assembly` now records a complete
ordinal manifest with explicit failed-attempt replacement, and
`GET /v1/projects/render/assemblies/{id}` replays the immutable receipt. Its
status is `ASSEMBLY_MANIFEST_VERIFIED_NOT_EXECUTABLE`: it verifies coverage and
history only, with no worker assignment, asset transfer, pixel verification,
or money authority.

LoRA now has the corresponding independent-evaluation evidence boundary:
`POST /v1/worker/lora/evaluations` takes the evaluator worker from the
authenticated token, derives trainer/evaluator supplier and buyer-account
separation, and binds the held-out pin, baseline model, metric direction, and
required margin from a durable probed compile receipt. The append-only
`project_lora_evaluation_receipts` row is replayable by the buyer through
`GET /v1/projects/lora/evaluations/{id}` and records the normalized improvement
for either higher- or lower-is-better metrics. Its status remains
`EVALUATION_RECORDED_NOT_EXECUTABLE`: no trainer, adapter deployment, charge,
payable, or outcome settlement is implied until those independently governed
runtime paths exist.

## 2. Work elimination — LARGELY DONE

| item | state |
|---|---|
| exact-result cache | `control/exact_reuse.go` — content-addressed identity, own settlement path. **Now tenant-scoped**: it was shared across buyers, which is a cache-existence side channel. |
| in-flight coalescing | **WIRED** — `control/inflight_coalescing.go`, governed `inflight_executions` with lease, state machine, bounded re-election and expiry; called from the realtime lane after the exact-cache miss. The old unwired `ClaimInflightLeader`/`ReleaseInflight` and `inflight_requests` are deleted. |
| tokenized prefix trie | `control/prefix_routing.go:173` — `ComputePrefixChain` over token ids, `DeepestWarmPrefix`, value-ranked eviction (`EvictPrefixCacheToBudget`) |
| KV-hit-aware routing | prefix warmth feeds the scheduler; `prefixWarmTTL` is deliberately shorter than model warmth |
| prepared tools/schema identity cache | **WIRED_MEASURED** — `control/realtime_identity_cache.go`; bounded tenant/profile/policy-scoped cache is called by exact reuse, coalescing, and cache population. It caches semantic request identity only; no tokenizer exists. |
| control-plane tokenization cache | **DOES_NOT_APPLY** — Merc does not tokenize on the control plane (byte heuristics for admission/pricing; engine owns settlement token counts). Building one would cache nothing. Bound audit: `evidence/perf/five-cache-architecture-audit.json`. |
| image / audio preprocessing caches | **DOES_NOT_APPLY** — image generation returns 503 (no runtime); `media_rendering` is closed-scene byte-exact rasterisation, not preprocess; `media_transcode` probe is per-job validation. Exact full-result reuse is the correct elimination shape, not a preprocess tier. Same bound audit. |
| deterministic JSON / tool scaffolding | absent |

Coalescing proves the shape the milestone asks for: 128 concurrent callers elect
exactly one leader, the followers reconcile, one supplier payable, an independent
discounted receipt each, positive Merc contribution, and physical tokens that do
not grow with the followers. Cross-tenant sharing is deliberately not attempted —
`RequestIdentity` carries a tenant scope, so two tenants issuing byte-identical
requests never meet.

The control plane still has no model tokenizer, so token-ID caching remains absent by
design — and that absence is now recorded as **architecture non-applicability**, not
an unfinished checklist item (`evidence/perf/five-cache-architecture-audit.json`).
Prepared request identity has a bounded production cache; host microbench separates
the hit path (~0.4µs) from the miss/canonicalisation path (~12µs) so this small
optimisation cannot be mistaken for a tokenizer or pricing authority.

Realtime demand now also leaves a receipt-bound market observation. The atomic
offer reservation records candidate depth, selected rank, supplier ask in fixed
point nanos, and the selected worker/supplier identities beside the immutable
`PricingDecision`; `/v1/realtime/requests/{id}/receipt` returns the same
`market_clearing` object. The order-book crossing is production-wired and
tested through two live offers, including the database immutability refusal.
This raises the realtime liquidity evidence, not the local-fabric claim: region
authority, calibrated net costs, and a multi-site market remain absent.

The operator surface now also exposes `GET /admin/market/liquidity/network`, a
single bounded receipt that composes the realtime and warm-service lane reports
for the same requested retention window. It preserves each lane's own offer,
demand, fill, utilization, churn, and fixed-point price-depth evidence and
states that the scope is retained Merc lanes only. It does not infer a global
market, legal region, or a new price.

Fabric topology evaluations are also replayable through the authenticated
worker-scoped `GET /v1/worker/fabric/topologies/{id}` read. The response
reconstructs the exact persisted links, synthetic collective evidence, freshness
and planner refusal. It remains evidence only: the stored non-admissible status
cannot create a `LOCAL_CLUSTER` placement or a gang scheduler grant.

## 3. Continuous batching — PARTIAL

`agent/src/quantized_llama_batched.rs` has the primitives: padded-mask batched
prefill, `compact_kv_cache`, `truncate_kv_cache`, per-row KV byte accounting.
`agent/src/executor.rs:595` `generate_batch` sizes a batch against a memory
budget (`batch_width_cap`).

That is memory-budgeted **static** batching inside one task. Token-budget
batching across arriving requests, length bucketing, adaptive queue delay and
latency classes do not exist. Unblocked.

## 4. Runtime tournament — SCHEMA UNBLOCKED, PRODUCT STILL SINGLE-RUNTIME

**Update 2026-07-30.** The schema blocker below is resolved. `runtime-authority.json`
is now v2 with a `runtimes[]` registry, governed invariants replacing the
two-model/two-cell count check, and lifecycle states that decide routability.
Four profiles are registered:

| profile | lifecycle | routable | evidence |
|---|---|---|---|
| `candle_metal` | ACTIVE | yes | `evidence/canary/real-runtime-realtime.json` (unbound historical chain; not candidate-bound) |
| `mlx_metal` | VALIDATED | no | `docs/SPEED_LANE_2026-07-27.md` |
| `llama_cpp_metal` | VALIDATED | no | `docs/SPEED_LANE_2026-07-27.md` |
| `vllm_cuda` | DRAFT | no | `evidence/runpod/cuda-first-proof.json` (unbound provider proof; not Merc-chain canary) |

Only CANARY and ACTIVE profiles project into the advertised capability set, so
registration widened nothing that is sellable. A benchmark that ran outside the
product carries a profile to VALIDATED and no further.

**Merc has a multi-runtime schema, not a multi-runtime product.** The milestone
is one governed workload executed through two independently registered profiles
at the same declared quality tier, with complete execution and money receipts,
comparable benchmark evidence, and a shadow selector that predicted the better
one.

**Done since.** Governed identity now lives in PostgreSQL: `runtime_engines`,
`runtime_profiles`, `runtime_profile_models`, `runtime_profile_hardware`,
`runtime_profile_capabilities`, synced under the migration lock. Routability is
a derived column with a CHECK, not an assertable flag, and a partial unique
index refuses two routable profiles claiming one cell. Content immutability is
enforced at sync — an existing `(runtime_profile_id, revision)` whose digest
moved is a hard refusal. Worker admission validates engine, platform,
per-claimed-cell memory and device count against the profile; `device_count`
was pure documentation before this.

The migration is deliberately mid-transition: `runtime_profile_id` is nullable,
`workers.engine` and `workers_engine_valid` both remain, and a trigger
dual-validates. `ReconcileWorkerRuntimeProfiles` is the gate for the rest.

**Update 2026-07-30, later.** The second runtime is chosen, driven and measured.
`docs/runtime/SECOND_RUNTIME_CENSUS.md` records why it is `llama_cpp_metal` on
the **embed** cell and not MLX and not the infer cell: MLX is not installed and
has no agent code path at all, and llama.cpp's `batch_infer` cell is `byte_exact`
in the one configuration where llama.cpp is byte-deterministic on Metal and runs
at 1.02× serial there, so proving the chain on it would prove it on a
configuration Merc would never route to.

Done since:

- `RuntimeDriver` (`agent/src/runtime_driver.rs`) — validate, launch, health,
  embed, cancel, drain, metrics — implemented by `CandleDriver` and
  `LlamaCppDriver`, with `EmbedRunner` as a real caller rather than a parallel
  abstraction. `LlamaCppDriver::validate` refuses a `byte_exact` cell outright.
- Artifact format now reaches the artifact list, not just the cell:
  `all-minilm-l6-v2` declares the GGUF llama.cpp loads beside the safetensors
  candle loads, and a cell declaring a format with no bytes behind it is refused.
- The profile content digest binds each cell's **resolved artifacts**, so
  repointing which GGUF backs `gguf` can no longer change every executed byte
  while the revision and digest stand still.
- A receipt-bound benchmark authority exists for the embed cell:
  `evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json`,
  produced by `merc-agent bench-embed` — one harness, one corpus, both drivers,
  the quality gate evaluated before any timing. An unbound in-process harness
  once reported roughly 6.7× at batch 8 (and 1.5×/2.7×/1.2× at batch 1/32/128);
  that is not product throughput and it is not chain cost. The quality gate
  cleared at 0.999998 minimum cosine on that same harness.

Remaining to reach the milestone:

1. migration steps 6–7 — `NOT NULL`, then drop `workers_engine_valid`, gated on
   a clean reconciliation in a real deployment. Steps 6–8 are currently satisfied
   by the stronger dispatch-time invariant (`worker_capability_requires_profile`)
   rather than by `NOT NULL`, because enrollment legitimately creates a worker row
   before any profile is known;
2. a complete Merc chain — task, verification, money, payable, receipt — on
   `llama_cpp_metal`. The engine executes real work through the driver and clears
   the gate; what has not been driven is that output through submit → claim →
   commit → verify → settle → receipt. Until that runs, `llama_cpp_metal` stays
   `VALIDATED`: a benchmark is a prerequisite for `REAL_RUNTIME_PROVEN`, not a
   substitute for the chain;
3. `RuntimeSelector` in shadow mode plus regret measurement.

A governance note that falls out of the measurements and will matter at step 2:
`validateBenchmarkAuthorityBinding` refuses a **routable** profile that serves a
`byte_exact` cell on a benchmark authority recording non-byte-determinism.
`llama_cpp_metal` declares such a cell, so it cannot become routable while it
does — correctly. Reaching CANARY on the embed cell therefore means dropping the
infer cell from that profile, not arguing with the rule.

### The original blocker, for the record

MLX versus llama.cpp is already **measured** on this hardware
(`docs/SPEED_LANE_2026-07-27.md`). What does not exist is the authority to
select among runtimes:

- `control/runtime-authority.json` describes exactly one runtime
  (`candle_metal`), two models, two cells.
- `loadRuntimeAuthority` (`control/runtime_authority.go:82`) panics unless
  `len(Models) == 2 && len(Cells) == 2`, and `runtimeAuthorityDocument.Runtime`
  is a single object, not a list.
- `workers_engine_valid` (`control/schema.sql:1162`) constrains
  `engine = 'candle'`.
- `agent/src/vllm.rs` exists but the control plane cannot advertise it.

So there is nothing to hold a tournament between. This is the `RuntimeSelector`
boundary from the authorized targeted-refactor list, and it is a capability
blocker rather than a profiling result: a schema that admits one runtime cannot
be measured into admitting two. Doing it means making the authority document
multi-runtime while keeping every existing guarantee — per-cell minimum memory,
onboarding policy, matrix digest binding into task provenance, and the
`tasks_runtime_provenance_complete` invariant.

Order note: this is the first step in the list whose prerequisite is structural,
so it should be scheduled after steps 1–3 produce measurements, not before.

## 5. Quantization and kernels — PARTIAL

A governed quality suite already exists and already rejected a quantization:
`evidence/bench/quality-suite.jsonl` approved 8-bit as OUTCOME-EQUIVALENT and
refused 4-bit (`docs/FRONTIER_300X.md`). Mixed-bit profiles, dequant/GEMM
profiling, weight prepacking and graph/shape compilation do not exist.

## 6. Speculation portfolio — PRIMITIVE ONLY

`KvCacheSlot::truncate` is implemented and tested for the accept-k-of-n rollback
shape (`agent/src/quantized_llama_batched.rs:912`). No proposer of any kind —
no suffix, n-gram, MTP, EAGLE or draft model. Unblocked, and suffix/n-gram is
the cheapest entry.

## 7. Hardware and energy arbitrage — HONESTLY UNMODELED

`hwClassCostRank` (`control/scheduler.go:161`) is a static ordinal by marginal
cost to Merc, used to prefer cheaper hardware on claim. Energy is explicitly
declared unknown in both the quote and the pricing decision: "power draw and
provider-specific energy cost are not yet modeled" (`control/quote.go:660`,
`control/api.go:1162`). Selecting by *verified outcome cost* needs the step-1
actuals plus per-device power, neither of which is metered today.

## 8. Outcome cascades — ABSENT

No small-model-then-evaluator-then-large-model path exists. The
`OUTCOME_EQUIVALENT` label is already defined and defended in
`docs/FRONTIER_300X.md`, so a cascade must report under it and never as
model-exact.

## Promotion rule

Every promoted profile states quality, latency, throughput, energy, provider
cost, supplier contribution, Merc contribution and buyer-price effect, and must
improve at least one without degrading another beyond policy. The four
authorities stay separate: PHYSICAL EXACT-MODEL, DELIVERED REUSE, EXACT CACHE,
OUTCOME-EQUIVALENT.

No complete replacement for an upstream engine is built unless measurement shows
a stable, profitable bottleneck that upstream and a narrow extension cannot
solve.


<!-- source: docs/HOT_PATH_DURABLE_ADMISSION.md -->

# Can admission be durable without being synchronous on the first token?

Status: design conclusion with measured floors (2026-08-03).  
Probe: `control/hot_path_free_admit_probe_test.go` → `evidence/perf/hot-path-free-admit-latest.json`.
That receipt is **UNBOUND** — it carries no producer identity, so it records what a
probe observed on one machine and cannot be reproduced from it. Treat the numbers
below as a design investigation, not as evidence for a latency claim.  
Does not change production admit behaviour.

## Plain answer

**No — not if “not synchronous” means zero durable work before dispatch.**

Buyer ceiling under concurrent multi-writer admission requires a **durable**
decrement of available budget before two admits can both observe the same residual.
Supplier liability and crash recovery require a **durable** intent that survives
process death. Removing both from the first-token path reintroduces the shapes
that already produced money P0s (TTL holds that drop in-flight money) or unpaid
supplier work.

**Yes — if “nearly free” means amortized O(1) durable work still before dispatch.**

Pre-authorize funding (execution envelopes, already shipped) and pre-authorize
capacity (capacity leases, designed in `docs/CAPACITY_LEASES.md`, not shipped).
Per request then does:

1. O(1) envelope spend `UPDATE` (or O(1) committed-micros counter)
2. O(1) lease/offer slot `UPDATE`
3. Contract + RESERVED event insert (batched)
4. Commit

That path keeps every disqualifying invariant. It is still synchronous. Its
authorize floor is multi-statement Postgres, measured in the probe — not zero,
and not free enough to assume ≤1 ms Merc-added TTFT without an e2e proof.

**≤1 ms p50 local Merc-added TTFT with durability intact is not achievable by
optimising the legacy multi-aggregate authorize path** (floor ~1.0–1.4 ms
authorize alone; Metal merc-owned was ~3.0 ms). With full amortization it is
**borderline for authorize alone and unproven for merc-added**; renegotiate the
target against the measured O(1) floor rather than chase zero-durable admit.

## What admission does today

### Legacy (no envelope)

`AuthorizeRealtimeContract` one transaction:

| Step | Work | Why synchronous |
| --- | --- | --- |
| Buyer funding | advisory lock + multi-aggregate snapshot + `buyers FOR UPDATE` | ceiling under interleaving |
| Offer claim | ranking CTE + atomic `available_sequences` decrement | capacity truth |
| Pricing / market | CPU + JSON | rate freeze |
| Contract + event | inserts | durable intent + idempotency |
| Commit | WAL | durability |

Lock hierarchy (enforced by `TestAuthorizeSettlementDeadlockRepro`): **buyer
funding before offer capacity**. Do not reverse it.

Measured durable anatomy (segment probe, c=1): funding ~0.15–0.57 ms, offer
~0.22 ms, pricing ~0.04 ms, contract ~0.35 ms, event ~0.11 ms, commit ~0.15 ms
→ **~0.9–1.4 ms** authorize. Prior claimed floor ~1.0 ms (0.7–1.2).

### Envelope ACTIVE

Create already ran `evaluateRealtimeBuyerFunding` for the full cap. Admit:

| Step | Work |
| --- | --- |
| Envelope spend | single `UPDATE` + spend row insert (no buyer advisory lock) |
| Offer claim | **same** ranking CTE as legacy |
| Contract + event | same |
| Commit | same |

Funding multi-aggregate leaves the hot path. Capacity selection and contract
persistence remain. Envelope amortises money check latency, not the whole admit.

## Designs considered

### 1. Bounded pre-authorized execution envelopes (exist)

**Survives** for funding amortization. **Does not** make admit free: offer claim
+ contract insert remain. Does not alone hit ≤1 ms merc-added on the Metal path
where authorize is ~2.6 ms of ~3.0 ms merc-owned.

Rails when ACTIVE:

| Rail | Behaviour |
| --- | --- |
| `evaluateRealtimeBuyerFunding` | Holds `(cap−spent)` on ACTIVE envelopes; EXECUTING under ACTIVE envelope excluded from realtimeReserved; when envelope leaves ACTIVE, in-flight RESERVED spends fall back to EXECUTING hold |
| `prepaidOpenReservationMicros` | Same ACTIVE term + expired-envelope EXECUTING fallback |
| `BuyerFreeCreditRemaining` | Same ACTIVE `(cap−spent)` residual + ACTIVE-conditional EXECUTING exclusion as the other two rails. Used by job intake (not advisory-only) when free credit gates MaxUSD without a payment method |

### 2. Capacity leases (`docs/CAPACITY_LEASES.md`, not implemented)

**Survives** as design if Postgres remains authority and cost-class rules hold.
Hot path becomes O(1) slot `UPDATE` instead of ranking CTE. Composes with
envelopes (funding + supply both amortized). Still durable + sync before
dispatch. Probe stand-in: direct `UPDATE realtime_worker_offers … WHERE worker_id=$1`.

### 3. Speculative admit, durable settle before money moves

**Dies in the strict form** (dispatch with no durable hold):

| Attack | Outcome |
| --- | --- |
| Two concurrent admits, same buyer, residual covers one ceiling | Both pass in-memory check → **buyer ceiling broken** |
| Multi-replica control plane | In-memory counters diverge → over-admit |
| Kill after tokens, before durable record | No contract → **supplier unpaid** or invent orphan pay from evidence (operational TTL-hold shape; two prior P0s) |
| Replay without durable idempotency key | Double dispatch |

**Partially survives** only if “speculative” means: durable O(1) holds +
minimal intent **before** dispatch, and heavy enrichment/settlement later.
That is not removing durability from the first-token path; it is the amortized
design above. Exposure window if contract insert is deferred after dispatch:

| Name | Bound |
| --- | --- |
| **dispatch-to-contract gap** | From commit of spend+capacity hold to commit of `execution_contracts` row |
| Money | Covered by durable envelope spend / hold (not trust) |
| Capacity | Covered by durable lease/offer decrement |
| Supplier liability | **Broken** if work completes and process dies before contract unless orphan-evidence payout exists — same family as TTL holds; **disqualified for production without that machinery green** |

Recommendation: do **not** open the dispatch-to-contract gap. Keep contract
insert in the pre-dispatch transaction. Overlap only helps if engine TTFT hides
post-dispatch work; merc-added is dominated by **pre-engine** work.

### 4. Short-reserve + O(1) committed-micros (`3c6c5157`, not merged)

Two-phase legacy admit: TX1 materialises `realtime_funding_holds`, TX2 claims
offer without buyer funding lock. Measured **~1.33–1.37×** same-buyer p95 at
c=32; **c=1 slightly worse** (second begin/commit). Remaining cost is the
multi-aggregate re-aggregate under the lock, not the offer claim.

**Survives** correctness after KEY SHARE-before-offer deadlock fix. **Died on
risk/reward** (~30% one tail vs two money P0s + 40P01 history). Named next step
was O(1) `committed_micros` under TX1 — would cut funding snapshot, still leave
durable sync multi-statement admit, still not “free on first token”.

Worth reviving **only** with the O(1) counter **and** only for legacy
non-envelope buyers; envelope path already avoids the funding lock.

## Surviving design (recommended)

```
offline / create-time:
  CreateExecutionEnvelope(cap)     -- durable full-cap hold
  IssueCapacityLease(worker, slots) -- durable capacity (future)

hot path (still sync, still durable, O(1)+insert):
  BEGIN
    UPDATE envelope SET reserved += need WHERE residual >= need  -- funding token
    SELECT buyers.id FOR KEY SHARE WHERE id = buyer               -- hierarchy
    UPDATE lease/offer SET remaining -= 1 WHERE remaining > 0    -- capacity token
    INSERT execution_contracts … EXECUTING
    INSERT realtime_authorization_events RESERVED
    bind spend/lease → contract
  COMMIT
  → dispatch upstream
```

- **Exposure window:** none beyond today’s EXECUTING reservation (durable before
  first byte to the engine).
- **Not free:** still ~begin + 2 updates + insert batch + commit on local
  Postgres.
- **≤1 ms merc-added:** only if this authorize floor plus intake/admission/proxy
  sum ≤1 ms e2e against the real engine. Authorize-alone measurements in the
  probe are necessary but not sufficient.

## Attacks that survived (on the recommended design)

| Attack | Why it fails to break the design |
| --- | --- |
| Concurrent same-buyer overspend | Envelope `UPDATE … reserved+need ≤ cap` is atomic |
| Concurrent capacity overbook | Lease/offer `UPDATE … remaining > 0` is atomic |
| Envelope expiry mid-flight | Existing ACTIVE-conditional exclusion: in-flight falls back to EXECUTING hold on all three committed-money rails |
| Kill after commit before stream | EXECUTING + RESERVED spend reconcilable; finalize/void paths exist |
| Kill before commit | Rollback restores envelope residual and capacity |
| Replay | Spend idempotency key + contract idempotency key |
| Settlement vs admit deadlock | KEY SHARE (or funding lock) on buyer **before** offer; never reverse |
| Reverse lock hierarchy for speed | **Disqualified** — 40P01 already proven |

## Three committed-money rails under the recommended design

| Rail | Envelope residual | OPEN short-reserve hold | EXECUTING contract | Expired envelope in-flight |
| --- | --- | --- | --- | --- |
| `evaluateRealtimeBuyerFunding` | ACTIVE `(cap−spent)` | must sum OPEN holds if short-reserve ships | sum maxima; exclude only if spend’s envelope still ACTIVE | hold via EXECUTING (exclusion off) |
| `prepaidOpenReservationMicros` | same ACTIVE term | must include OPEN holds if shipped | expired-envelope RESERVED spend fallback | same |
| `BuyerFreeCreditRemaining` | ACTIVE `(cap−spent)` (fixed) | **must add OPEN holds** if shipped | sum maxima; exclude only if spend’s envelope still ACTIVE | hold via EXECUTING (exclusion off) |

Any design that drops a term on one rail while another rail still spends is a
P0. Envelopes already taught that lesson twice.

## What to renegotiate

| Target | Honest position after this analysis |
| --- | --- |
| ≤1 ms p50 local Merc-added TTFT | Not reachable on legacy path. Borderline under full amortization; **require e2e Metal remeasure** after envelope-default + lease, not authorize-only hope |
| Authorize 10× at c=1 | Floor branch already MET: ~1 ms durable work is irreducible without removing durability |
| “Architecture nearly free in the hot path” | Achievable as **amortized O(1) durable**, not as **async durable** |

## Measured floors (this worktree probe)

Machine: Mac Studio, 28 CPUs, load ~8 / 28 (quiet by probe heuristic).  
n=80 samples/cell, same buyer, one offer unless noted. Unbound probe receipt.

### Authorize p50 / p95 (ms)

| Path | c=1 p50 | c=1 p95 | c=8 p50 | c=8 p95 | c=32 p50 | c=32 p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Legacy multi-aggregate | 1.13 | 1.45 | 13.4 | 38.8 | 59.1 | 117 |
| Envelope ACTIVE (prod path) | 1.25 | 1.61 | 3.19 | 5.12 | 3.21 | 33.3 |
| Envelope + direct offer claim (lease stand-in) | **0.96** | 1.33 | **2.51** | 5.71 | **2.88** | 24.0 |

### Direct-claim serial decomp (c=1, p50 ms)

| begin | envelope spend | direct offer | pricing | contract+event batch | commit | total |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| (in total) | 0.41 | 0.22 | (in total) | 0.39 | 0.15 | **1.30** |

### Reading

1. **Envelope does not win at c=1.** Prod envelope path was *slightly slower* than
   legacy at c=1 (1.25 vs 1.13): spend UPDATE+row insert costs more than a quiet
   funding snapshot. The envelope win is **concurrency**: same-buyer c=32 drops
   from 59 ms → 3.2 ms p50 because the buyer funding advisory lock leaves the path.
2. **Lease stand-in helps a little more** (0.96 ms c=1; 2.88 ms c=32 p50) by
   replacing the ranking CTE with a PK update. Contract insert + commit remain
   ~0.5 ms combined — the durable floor of “intent must exist before dispatch”.
3. **≤1 ms merc-added TTFT is still not shown.** Best authorize p50 is 0.96 ms
   *before* intake/admission/proxy. Prior mock merc-added had ~0.4 ms of those;
   Metal merc-owned was ~3.0 ms. Authorize-alone under 1 ms ≠ target met.
4. **c=32 p95** on envelope paths (24–33 ms) is still offer-row serialisation on
   a one-offer book — multiplicity / leases address the tail, not zero-durable.

## Probe how-to

```bash
MERC_HOT_PATH_FREE_PROBE=1 \
MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
go test -count=1 -run TestHotPathFreeAdmitProbe -timeout 30m ./control
```


<!-- source: docs/runtime/ADAPTIVE_EXECUTION.md -->

# Adaptive execution — capability, activation, verification classes, checkpoint gate

Branch `perf/execution-frontier`, continuing from `579edc0e`.
`release/rc1-go-closure` untouched.

This records what is done and what is not. Every "done" row has a test or a
receipt behind it; every "not done" row says so plainly rather than being
described as partial.

## 0. Capability is now separate from activation policy

**The defect.** Promoting llama.cpp's embed cell from `VALIDATED` to
`REAL_RUNTIME_PROVEN` broke every agent at once. `merc-agent` embeds
`control/runtime-authority.json` at compile time and hashed its **raw bytes**;
the control plane did the same. A lifecycle edit moves those bytes, so the two
started describing different matrices and every enrolment and dispatch was
refused until the whole fleet was rebuilt. A lifecycle promotion had become a
coordinated binary deploy — the exact thing a lifecycle is supposed to avoid.

**The split.**

| Immutable capability manifest | Mutable activation policy |
|---|---|
| profile id, revision, engine + revision, adapter | lifecycle |
| tokenizer revision, chat template, source identity | ordinary routability |
| device, hardware platforms, device count | directed-test eligibility |
| parallelism, runtime features | canary allowlist, traffic percentage |
| per cell: workload, model, runner, wire format, memory floor, verification contract, measured parallelism limits | promotion receipt |
| the exact artifact bytes each cell resolves to | rollback target, policy revision, effective time |

`control/capability_manifest.go` computes the digest over an **explicit**
projection rather than over the struct with a few fields blanked. That direction
is the point: with blanking, every field added later is capability by default and
silently welds itself to the agent binary. That is precisely how the per-cell
lifecycle got into the old digest without anyone deciding it should be there.

The canonical form is line-oriented, not JSON, because the two implementations
are in different languages: Go marshals `float64(2)` as `2` and Rust marshals
`2.0f64` as `2.0`, so a JSON canonicalisation would produce two different digests
for one document and the disagreement would surface as **every agent being
refused**. Numbers are formatted explicitly on both sides and the digest is
pinned in both test suites.

```
capability matrix digest  ea158343b64ea0967b832757c97777b8b2c6f00e8ee27549888e1f25c7bbb2a4
document byte digest      92e6343c16346172ecc6ff32a841ba5607e2d36d8f8c9f452ea780fffff406a6
```

**The last compile-time coupling.** Enrolment used to validate a worker's
declaration against the DIRECTED set, so an agent whose embedded document called
a cell `DRAFT` could not declare it — and promoting that cell by policy therefore
*still* needed the fleet rebuilt first. A worker now declares **capability** and
the control plane authorizes against **policy**. `TestPromotingOutOfDraftNeedsNoNewAgentBuild`
drives exactly that: the same worker is refused before the policy write and
authorized after it, with the capability digest unchanged.

The agent's own filter narrowed to the terminal states only. A
`REJECTED_FOR_CONTRACT` cell stays undeclarable, because measurement decided that
the runtime cannot honour the contract and no policy write reverses a
measurement.

**Activation policy** lives in `runtime_activation_policies`: append-only, one
revision per decision, keyed by capability identity. A rollback **writes
forward** — an undo that erased the intervening revision would leave no record
that the decision was ever taken. The document still declares the *default*
policy for a capability revision, and the sync **seeds** rather than overwrites,
so an operator promotion survives a redeploy.

Three rules are in the database, not only in Go:

- routability is derived from lifecycle, never independently asserted;
- nothing becomes routable without naming the promotion receipt that authorised it;
- policy rows cannot be updated or deleted.

A statement whose capability digest or profile revision no longer matches the
document is **refused at write** (the operator is still there to be told) and
**dropped at read** with the drop recorded — the promotion was granted to a
runtime that no longer exists in that form.

**Migration.** The digest changed meaning, not content, so it must not be
reported as drift. `runtime_profiles.capability_manifest_version` recognises a
row written under v1 and rewrites it; `runtime_profile_digest_history` retains
the old value so anything that recorded it still resolves to the revision it
named. No profile took a new revision for this.

### Found while doing it

- **A cell cannot outrank its profile**, so a cell-only `CANARY` statement is
  floored straight back to `REAL_RUNTIME_PROVEN` and changes nothing. A promotion
  addresses both. The test asserts it rather than working around it.
- **`effective_at <= now()` versus a Go timestamp.** PostgreSQL's `now()` is the
  *transaction* start, so a wall-clock time taken in Go is always fractionally
  later — and policy written and projected in one transaction projected nothing,
  silently. The insert uses `COALESCE($13, now())`.
- **The worker/profile trigger read an arbitrary revision.** `SELECT * INTO p
  FROM runtime_profiles WHERE runtime_profile_id = …` was written when one row
  existed per profile; with history retained, PL/pgSQL's non-STRICT `SELECT INTO`
  takes the first row it finds, so the lifecycle and engine checks were made
  against whichever revision the planner returned. Now scoped to the revision the
  worker binds.
- **A cell-id-keyed projection panics at process start.** The document validator
  deliberately permits two non-routable profiles to describe the same cell id —
  that is how a challenger is registered against an incumbent's cell — and the
  lifecycle-blind capability projection is the first filter that admits both.
  Keyed by `(runtime, cell)` now.

## 1. Verification classes are governed, not sampled

Ordinary buyer traffic is unchanged: reputation-weighted, HMAC-selected per task
id. What did not exist was a way to say *this execution will be verified* without
touching the sampling secret.

```
SAMPLED    ordinary buyer traffic; the only class a buyer may have
REQUIRED   always checked; system and operator work
HONEYPOT   known-answer probe; always checked
REDUNDANT  second execution of a primary chunk; always checked
REPLAY     recorded input against recorded output; always checked
```

The class is bound to the **task**, the **compute plan**, the **verification work
row** and the **receipt**. It is expressed as a probability of `1.0` rather than
as a branch around the sampling machinery, so the pinned sampling row still
records what actually decided the task; the class is stored beside it, so a
reader can tell *certain because governed* from *certain because the supplier is
new*.

A buyer cannot select one. The field does not exist on the wire, and submit
decodes with `DisallowUnknownFields` — a stronger guarantee than a validator that
has to remember to refuse it. `MERC_VERIFICATION_SAMPLE_SECRET` is untouched.

Two rules are in the database: the class may not contradict the probe/redundancy
flags on the task it labels, and a non-ordinary class may not be pinned
`selected = false`.

## 2. `merc dev checkpoint`

This exists because a knowingly unverified checkpoint was pushed: the commit was
chained after a test run with `&&` in a way that did not gate on the result, the
suite was red, and the push went out. That is not a discipline problem — a shell
pipeline whose failure mode is "push anyway" will be typed again.

Sequential, as a program rather than a habit:

1. worktree validation (clean tree, not a frozen branch, HEAD recorded);
2. targeted authority tests, first, so a regression is reported in seconds;
3. mutation suite — which modifies the tree;
4. **proof** that the mutation runner restored it, by content digest over every
   tracked file. `git status --porcelain` would usually catch it; hashing the
   bytes answers the question actually being asked — *is this the source that was
   tested*;
5. full CI, only now that the tree is proved restored;
6. race suite over the concurrency the money path has;
7. HEAD and worktree digest unchanged;
8. receipt at `evidence/checkpoint/<sha>.json`.

A receipt with a skipped or failed step **authorizes nothing**. Recording the
skip and treating the receipt as complete would be the same mistake with better
paperwork. `.githooks/pre-push` calls `merc dev checkpoint-verify`; the logic is
in the CLI so the same rule can be run by hand or by CI without depending on
anyone's local hook configuration. Remote CI remains authoritative.

Receipts are **not** committed. A receipt is bound to a commit hash, so
committing one would create a commit that itself has no receipt, and the hook
would refuse the very commit carrying the proof.

### What the gate found on its first run

`make ci` was already red at `579edc0e`, in five places, none of them noticed
because nothing ran the whole target:

- **the route-count tripwire was stale.** An earlier `validate-authorization-matrix.py`
  revision asserted exactly 81 reviewed routes while 82 had been registered since
  `GET /admin/plan-accuracy` landed in `b0004f00`. The route WAS added to the
  matrix — only the constant was missed. The current reviewed inventory is 110 and
  its validator and receipt scorer share that same tripwire. Because `validate-readiness.py` scores
  `auth_matrix_complete` for 3 points under source-and-CI and 8 under security,
  the stale constant silently cost **11 readiness points**, and the declared score
  of 83 had been reading as a receipt-derived 72 ever since.
- **the MiniLM GGUF was never recorded in `ops/model-provenance.json`.** It was
  added to the runtime authority and pinned in `agent/src/models.rs`, and the
  governance validator wants all three to agree.
- **`cargo clippy -- -D warnings` was red** on five dead-code items and a
  `mod tests` with public items after it. The dead code is the `RuntimeDriver`
  boundary's unused half (`validate`, `cancel`, `drain`), the GGUF embed spec, and
  the supervised-launch arm of `LlamaServerSupervision` — all deliberate shape
  rather than leftovers, so each now carries an explicit allow and a reason
  instead of a warning nobody reads.
- **`assert-no-test-skips.sh` was red** because every test that needs object
  storage or a local engine skips, and the allowlist had two entries. Those
  skips are now named — 36 of them — which is the honest cost of a gate that
  lists every skip rather than accepting a category. `make ci` also now passes
  `MERC_TEST_S3_*` and `MERC_LLAMA_EMBED_URL` through when the environment has
  them, so a machine with MinIO and a llama-server actually runs those tests.

A sixth appeared once the first five were fixed and the gate got far enough to
run `make ci` at all: **`go test ./...` has no `-timeout`, so it used the 10
minute default**, and the suite is about fourteen minutes on a host with object
storage and a local engine. It died mid-run with a timeout panic naming
`TestBothAgentsExecuteADirectedJobEndToEnd`, which reads as a hung test rather
than as a budget. `make ci` could not have passed on such a host since the
agent-process tests landed.

A seventh appeared behind it, and it is the worse of the two.
`assert-no-test-skips.sh` runs the suite **again** with `-json`, also without a
timeout, inside a `$(...)` under `set -euo pipefail`. When that timed out, the
command substitution failed and the script exited **with no output whatsoever** —
`make ci` printed nothing but `Error 1`. A gate that fails silently is
indistinguishable from a gate that is broken. It now takes the same budget and
says so when the suite it depends on did not pass.

All seven are fixed. The point is not the fixes; it is that "full suite green" at
the last checkpoint meant `go test ./...` with a hand-typed `-timeout`, and the
gate is what makes the difference visible.

The mutation suite came back **32 caught, 1 survived**, and the survivor is a
real hole:

> `exact reuse hashes request shape but not runtime authority` — **SURVIVED**

`batchRequestIdentity` derives the batch reuse key from the whole frozen workload
decision. Swapping that for the binding alone changes nothing any test can see,
because `batchExactReuseEnabled` is a compile-time `false` and the function
returns before it reaches the digest. A surviving mutation on disabled code is
not a passing grade; it is a hole waiting for the day the flag is flipped back —
two jobs frozen onto different runtime cells would share a reuse key, and the
second buyer would be served the first runtime's bytes at the reuse price with a
receipt naming a cell that never ran. Now asserted at source, with the property
itself (identical bindings, different decision digests) proved first so the source
check is anchored to a real difference rather than to a spelling.

An earlier run of the same suite reported **33 caught, 0 survived**. That number
is void: it was measured while a stray mutation runner was concurrently rewriting
the tree, which makes tests fail — and therefore mutations read as "caught" — for
entirely the wrong reason. It is recorded here rather than quietly replaced,
because a corrupted green is exactly the kind of result this whole gate exists to
stop being believed.

### And one the gate did not catch, because I broke its rule by hand

A checkpoint was killed mid-mutation. Its `mutation-test.sh` **survived the
kill**, and I removed its lock directory by hand on the assumption it was stale.
It then went on rewriting source files for the next hour — through a later
checkpoint whose restoration digest happened to be taken in a clean moment, and
into a `make ci` run that failed on four tests that were exercising mutated code.
Thirteen files were still mutated when I finally looked.

"Never run CI while mutation tooling modifies the same tree" was the one rule the
directive stated outright, and a content digest at a single instant does not
enforce it: between two mutations the tree can match by luck. The checkpoint now
refuses to **start** while `scripts/mutation-test.sh` holds its lock, and refuses
to **trust** the restoration digest if the lock is still held after the suite
returns. `TestCheckpointReadsTheMutationScriptsOwnLock` pins the two derivations
of the lock path together, because if they ever diverge the guard stops guarding
silently.

## 3. The llama.cpp failure matrix

`REAL_RUNTIME_PROVEN` was recorded with an explicitly incomplete failure matrix
and the promotion receipt said so. A happy path proves a runtime can earn money;
it says nothing about what happens when the agent dies holding a claim, when an
upload never lands, when the verifier restarts mid-decision, or when settlement
is retried. Every one of those is a place money can be created or destroyed.

Every case asserts the same nine properties through one helper, so a case cannot
quietly check less than its neighbours: one authoritative state from a closed set,
no duplicate buyer debit, no duplicate supplier payable, **nothing standing as
payment** for undelivered or rejected work, no leaked task lease, no leaked
verification lease, no artifact authority without a digest, bounded retry, and an
actionable diagnostic.

| Case | Driven by | Result |
|---|---|---|
| agent death before claim | real process, killed | queued, claimable, 0 USD |
| agent death after claim | real process, killed mid-claim | requeued once, 0 USD |
| runtime unreachable | real agent against a dead port | no commit, 0 USD |
| duplicate commit | second identical commit | refused, one payable |
| output upload interrupted | commit naming an absent object | no verification, 0 USD |
| result digest mismatch | well-formed digest of other bytes | work outcome `fail`, 0 USD |
| verifier restart | two passes over one attempt | one terminal outcome |
| finalizer + settlement retry | three finalizations | one debit, one payable |
| expired lease | seven expiries | requeued, 0 USD |
| input download failure | input object removed | requeued, 0 USD |
| cancellation before execution | queued job cancelled | cancelled, 0 USD |
| cancellation during execution | running job | refused, 0 USD |
| database restart | every backend terminated | recovers, no double count |
| receipt-generation failure | plan removal attempted | refused by the schema |

The **supplier invariant is the net, not the row count**. The supplier is credited
when verification settles and the grade can arrive afterwards, so a clawback
legitimately leaves two rows summing to nothing; asserting "exactly one row" would
call correct behaviour wrong in one direction and miss a real double-payment in
the other.

Three things the matrix found about the harness rather than the product, all of
which are the schema defending itself and were adopted rather than worked around:

- writing the execution identity columns directly is refused — *"task execution
  identity is immutable outside claim transition"* — so a claim has to come from
  the claim path;
- a job cannot go `running -> queued`, so a cancellation-before-execution case has
  to submit a job rather than rewind one;
- a frozen compute plan cannot be removed at all, which is a stronger answer to
  "what if the receipt authority is missing" than any handling would be.

Not driven: runtime **crash** mid-execution (as opposed to unreachable), model
artifact missing and model digest mismatch at the agent's download step, and
stale-attempt/tiebreak interactions beyond the existing
`TestStaleAttemptOutputIsNotVerified`. Those need the agent's download path
instrumented, not the control plane.

## State reconciliation, 2026-07-31

Run against HEAD `e4fd8993` because the accumulated progress history contained
incompatible snapshots. Every checkpoint the history named — `0bf2ee20`,
`2d62ee06`, `41d7c768`, `579edc0e`, `c7506e6d`, `a0b52cb8`, `e4fd8993` — is an
ancestor of HEAD; nothing was missing and nothing needed rebuilding. The branch is
clean and level with its upstream.

Six subsystems were classified from source by independent readers and each
classification was then adversarially re-checked.
`evidence/state/current-execution-frontier.json` (unbound frontier inventory)
carries the machine-readable result; the runtime digests in it come from
`merc dev authority`, which calls the same functions admission and dispatch call,
so the receipt cannot restate a number a previous report chose.

| Subsystem | Classification |
|---|---|
| runtime authority in PostgreSQL | PRODUCTION_WIRED |
| activation policy | PRODUCTION_WIRED |
| second runtime (llama.cpp) | REAL_RUNTIME_PROVEN |
| exact-result reuse cache | PRODUCTION_WIRED |
| in-flight coalescing | PRODUCTION_WIRED |
| execution overhead actuals | PRODUCTION_WIRED |
| token-budget batching | IMPLEMENTED_UNWIRED |
| prepared tools/schema identity cache | PRODUCTION_WIRED (control-plane tokenizer DOES_NOT_APPLY — see evidence/perf/five-cache-architecture-audit.json) |
| media preprocessing cache | DOES_NOT_APPLY (no image-gen preprocess; media_rendering is full-result byte-exact work) |
| RuntimeSelector | ABSENT |

**The second runtime is REAL_RUNTIME_PROVEN, not ECONOMICALLY_PROVEN.** Real agent
processes executed real llama-server embeddings through the real driver, and the
real verification and settlement transactions wrote real ledger rows that
conserve. What did not happen is a buyer request: `buildWorkloadDecisionDirected`
has zero production callers, so every chain proof submits a test-constructed job
row rather than going through `POST /v1/jobs`. The money is real and its origin is
a fixture. That is exactly the rung `llama_cpp_metal` already sits at, so the
authority and the classification agree.

### Claims the reconciliation had to withdraw

Recorded in full in
`evidence/state/correction-2026-07-31-coalescing-and-directed-routing.json`
(unbound correction record; lists withdrawn claims, not live proofs).

- **Coalescing is wired but not economically proven.** The 128-way test drives
  `Store.ClaimInflightExecution` directly — it executes nothing and settles
  nothing — and the money test is arithmetic against no database. The two halves
  have never been joined.
- **`RenewInflightLease` has zero production callers.** `inflightLeaseTTL` is 30
  seconds, so a leader whose execution runs longer can be taken over mid-flight.
- **`sweepExpiredInflight` has zero production callers** and is not in the workers
  ticker table. Expired `inflight_executions` rows accumulate.
- **`ClassCoalescedDelivery` is never written.** Followers settle through
  `SettleRealtimeExactReuse` and are recorded as `exact_reuse`, so coalesced
  revenue cannot be counted separately.
- **`MERC_SHAPE_AWARE_ROUTING=1` is inert.** `ClaimTaskSQL` passes
  `shapeNoPreference` unconditionally; `preferenceForTier` has no production
  caller.
- **`EvictPrefixCacheToBudget` and `DeepestWarmPrefix` have zero production
  callers.** The scheduler uses its own inline warm-depth SQL, so two definitions
  of warm depth exist and only one is live.
- **`SelectBatch` and `TokenBudgetFor` have zero production callers**, and one
  latency class is defined where the directive names four.

None of these were introduced by this tranche. They are claims that outran their
wiring, and a caller census is the only thing that finds them.

## Against the directive's stop conditions

| # | Condition | State |
|---|---|---|
| 1 | Agent capability identity decoupled from lifecycle policy | **done** |
| 2 | Proof/canary verification deterministic and production-governed | **done** |
| 3 | llama.cpp failure matrix complete | **substantially done** — 14 cases driven, 3 named as not driven |
| 4 | RuntimeSelector runs in shadow mode | **not done** |
| 5 | Prediction errors and regret populated from paired evidence | **not done** |
| 6 | A narrow selector promotion receipt exists | **not done** |
| 7 | llama.cpp embedding enters bounded CANARY or is honestly blocked | **honestly blocked** — blocked by 4-6, not by measurement |
| 8 | 128 eligible requests produce one physical execution and one payable | **bound against a double upstream** — `evidence/reuse/public-path-coalescing-128-to-1.json` (commit `4ef1922a`); not a GPU performance measurement |
| 9 | Coalesced buyer charges and Merc contribution reconcile | **bound on the public money path against a double upstream** — same receipts; not a live supplier-runtime claim |
| 10 | Tokenization and schema caches with real callers | **partial, closed honestly** — prepared request identity cache is live and bounded; control-plane tokenization cache **DOES_NOT_APPLY** (no model tokenizer on the control plane). Bound audit: `evidence/perf/five-cache-architecture-audit.json` |
| 11 | Token-budget policies measured per traffic class | **not done** |
| 12 | Full suite green and every pushed checkpoint receipt-bound | **done** — and `make ci` is green for the first time on this branch |

Four of twelve, plus most of a fifth. The four that are done are the ones the
directive listed as corrections to make **before** the selector work, and the
capability/activation split is what the rest of the programme has to be built on:
a selector promotion is an activation-policy write with a receipt and an instant
rollback, and none of that was expressible before.

## Not done

| Section | State |
|---|---|
| 4. RuntimeSelector in shadow mode | **not done** |
| 5. Verified-outcome scoring | **not done** |
| 6. Paired selector evidence | **not done** |
| 7. Selector promotion gate | **not done** |
| 8. Coalescing proved through real money | **bound money-path proofs against a double upstream** — `evidence/reuse/public-path-coalescing-128-to-1.json` (commit `4ef1922a`) shows 128 deliveries to 1 upstream call through the real public handler; it does not measure GPU performance or a live supplier runtime |
| 9. Prepared tools/schema identity cache | **done for identity; tokenization DOES_NOT_APPLY** — `realtime_identity_cache.go` has production callers, tenant/profile/policy invalidation, bounded LRU/TTL, and hit/miss metrics; control-plane token-ID caching is not an unfinished item (architecture has no control-plane tokenizer). See `evidence/perf/five-cache-architecture-audit.json` |
| 10. Token-budget batching sweep | **not done** |
| 11. vLLM CUDA as a governed cell | **not done** |

`llama_cpp_metal`'s embed cell remains `REAL_RUNTIME_PROVEN` and directed-only.
Candle remains `ACTIVE`. llama.cpp byte-exact generation remains
`REJECTED_FOR_CONTRACT`. Ordinary routing has not changed.

## Grok

`DEFERRED_USAGE_LIMIT`. Not auth-blocked and not awaiting an external action. The
five queued audits keep their contracts and are re-runnable when usage returns.
The direct adversarial work here is marked `NOT_GROK_INDEPENDENT`: the
capability/activation split was reviewed by writing the mutation tests that fail
if either half leaks into the other, and by pinning the cross-language digest on
both sides so a divergence is a test failure rather than a production refusal.


<!-- source: docs/runtime/RUNTIME_GGUF_CLOSURE_BASELINE.md -->

# Runtime GGUF closure — baseline before modification

Recorded before any edit in this pass, so every later claim has a fixed point to
be measured against.

```
branch      perf/execution-frontier
sha         160afc5f9ab0b78eef26f064ec3bb9ef2fb3876b
tree        clean
```

## Test baseline

| Suite | Command | Result |
| --- | --- | --- |
| Go control plane | `MERC_ALLOW_SKIPPING_DB_TESTS=1 go test ./...` | `ok merc/control` |
| Rust agent | `cargo test` | 106 passed, 0 failed, 0 ignored |

## Authority document

`control/runtime-authority.json`, `schema_version: 2`, matrix `2026-07-30.3`,
carries `runtimes[]`. Lifecycle at baseline:

| engine | lifecycle |
| --- | --- |
| candle | ACTIVE |
| mlx | VALIDATED |
| llama_cpp | VALIDATED |
| vllm | DRAFT |

## Claimed fixes — verified, not assumed

The instruction listed ten fixes to confirm are really present. Checked against
source rather than against the previous report:

| Claim | Status | Evidence |
| --- | --- | --- |
| Schema v2 uses `runtimes[]` | **PRESENT** | `schema_version: 2` and a `runtimes[]` array |
| `terminate()` retries and verifies absence | **PRESENT** | `scripts/runpod-vllm.sh` loops `1 2 3`, calls `pod_exists`, prints `(verified)` only when the pod is gone, and prints a loud `!! FAILED TO TERMINATE` otherwise |
| `down-all` iterates then re-lists | **PRESENT** | terminates every id, then `list_pods` |
| MLX listen backlog ≥ 256 | **NOT FOUND** | no `backlog` setting exists on any MLX path; the only matches in the tree are unrelated `webhook_backlog` metrics in `scripts/local-resilience-rehearsal.sh` |
| RunPod stale env files deleted before provisioning | **NOT FOUND** | no `rm`/`unlink` of stale pod-id, URL, port, status or env files in `scripts/runpod-vllm.sh` |

Two of the ten claimed fixes are not in the tree. They are recorded here as
absent rather than carried forward as done; the remaining five claims
(agent schema-v2 projection, Go-exercises-Rust-contract, harness measures
determinism, failed provision cannot report success, transport overhead
measured) are still to be checked in this pass.

## Provider state

```
bash scripts/runpod-vllm.sh list
  balance $16.87
  no pods running
```

Zero pods at baseline, verified against the provider rather than against a local
state file. Nothing is billing.

## Artifact confound carried in

This is the gap the closure exists to fix:

| engine | artifact at baseline |
| --- | --- |
| candle | pinned q4 GGUF |
| llama.cpp | pinned q4 GGUF |
| MLX | a different 4-bit conversion |
| vLLM | **HF bf16, not the pinned GGUF** |

The vLLM rows in the current report are therefore not exact-artifact results and
must not be cited as closing the CUDA gap.

## Claim boundary

Baseline only. No throughput figure in the previous report is re-measured in
this document; the reported history is carried forward as **unverified** until
it is re-run against the canonical manifest frozen later in this pass.


<!-- source: docs/runtime/SECOND_RUNTIME_CENSUS.md -->

# Which runtime becomes the second one — wiring census

Lane B says: *choose the runtime requiring the least code to complete the full
Merc product path, and do not choose from raw benchmark speed alone.* This is
that comparison. Every row was probed against this host and this tree on
2026-07-30, not carried forward from a report.

The candidates are the two profiles already registered as `VALIDATED`:
`mlx_metal` (r2) and `llama_cpp_metal` (r5). `vllm_cuda` is `DRAFT` and needs
hardware that is not attached, so it is not in the running for this lane.

## The comparison

| axis | `mlx_metal` | `llama_cpp_metal` |
|---|---|---|
| existing installation | **absent.** `import mlx.core` and `import mlx_lm` both fail on this host. A cached `mlx-community/Llama-3.2-1B-Instruct-4bit` exists with no runtime to load it. | **present.** `/opt/homebrew/bin/llama-server` and `llama-cli` are installed. |
| model compatibility | one cell, `mlx-metal-llama1-infer`, on a *different* 4-bit conversion from the pinned artifact — the artifact confound the GGUF closure pass exists to remove. | both cells registered. `llama-cpp-metal-llama1-infer` runs the pinned Q4_K_M GGUF; `llama-cpp-metal-minilm-embed` runs the F16 GGUF of the same logical MiniLM. |
| agent integration effort | **no in-agent path at all.** Nothing in `agent/src` references MLX. Reaching the product path means a process supervisor, a wire protocol and a driver, all new. | `agent/src/inference.rs` already fronts `llama-server` as `openai_http` for `/v1/chat/completions`. The embed verb is the only missing call. |
| cancellation | unbuilt. | request drop plus `TaskDeadline` — the same shape `openai_http` already uses. |
| output contract | unbuilt. | `/v1/embeddings` returns float arrays, which is what `EmbedResult` and `encode_embeddings_binary` already consume. |
| determinism | never measured. | measured, and the answer is *cell-specific*. Byte-exact: **diverges** in every batched configuration on Metal (`llama-cpp-metal-determinism-sweep.json`); byte-identical only serialised at 1.02× serial. Cosine: **0.999999** mean against a 0.999 gate. |
| metrics | unbuilt. | usage fields already parsed on the `openai_http` path. |
| lifecycle | `VALIDATED`, `benchmark_authority` **empty** — and both the document validator and a DB CHECK refuse routability without one. | `VALIDATED` with `benchmark_authority` populated. |
| quality tier | `UNPROVEN` | `UNPROVEN` |
| current admitted workloads | `batch_infer` only — the verification class where a Metal llama.cpp-style engine is dominated anyway. | `batch_infer` (dominated) **and** `embed` (admissible under cosine). |

## Decision

**`llama_cpp_metal`, on the `embed` cell.**

Two axes decide it and neither is speed. MLX is not installed and has no agent
code path, so it starts from a supervisor and a protocol. llama.cpp is installed
and already reachable through an existing backend trait.

The *cell* choice matters as much as the runtime choice. `llama_cpp_metal`'s
`batch_infer` cell is verified `byte_exact`, and the determinism sweep settled
that llama.cpp on Metal cannot be both batched and byte-deterministic: its only
reproducible setting runs at 1.02× serial while candle batches to 2.26× *and*
stays byte-identical. Driving the milestone through that cell would mean proving
the chain on a configuration Merc would never route to. The `embed` cell is
verified `cosine`, clears its gate by roughly three orders of magnitude, and is
a workload the product actually admits.

So the governed workload for the two-runtime milestone is **embed on
all-minilm-l6-v2**, executed by `candle_metal` and by `llama_cpp_metal`, at the
same declared quality tier, verification policy and money policy.

## Two gaps this census exposed

Both are real and both block the lane; neither was visible from the profile
document alone.

1. **The GGUF embed artifact is not declared.** The
   `llama-cpp-metal-minilm-embed` cell declares `wire_kind: "gguf"`, but the
   `all-minilm-l6-v2` model lists only the safetensors artifacts candle loads.
   An agent asked to serve that cell has nothing to fetch. Artifact format moved
   from the model to the (runtime, model) pair last commit; the artifact *list*
   did not move with it.

2. **The agent still reads `wire_kind` globally.** `agent/src/runtime_authority.rs`
   projects `model_kind` from `model.wire_kind` and never looks at
   `cell.wire_kind`. The Go side made format a per-cell property; the Rust
   projection did not follow. Today it is latent — no non-candle profile is
   routable — and it would have surfaced as a worker advertising `hf` for a cell
   that serves GGUF the moment one was.

## What this census does not establish

- Embedding **throughput** on either runtime. Not measured here; the benchmark
  receipt for the second profile is a separate artifact with its own contract.
- That the F16 GGUF is the right quantization. Only F16 has been compared
  against the gate; Q8 and below are unmeasured and may not clear 0.999.
- Anything about MLX's actual performance. This census rejects MLX on
  integration cost, not on speed — it was never run on this host.


<!-- source: docs/runtime/TRANCHE_STATUS.md -->

# Adaptive execution frontier — tranche status

Branch `perf/execution-frontier`, as of 2026-07-30. `release/rc1-go-closure`
untouched.

> Superseded in part by [ADAPTIVE_EXECUTION.md](ADAPTIVE_EXECUTION.md), which
> records the capability/activation split, governed verification classes, the
> checkpoint gate and the llama.cpp failure matrix. In particular, the "Not
> established" note below about the failure matrix is now largely closed and the
> claim that a lifecycle promotion is a coordinated agent deploy is no longer
> true — that was the defect the split removed.

This is what is done, what is not, and what the not-done items are blocked on.
Nothing here is projected: every "done" row has a test or a receipt behind it,
and every "not done" row says so plainly rather than being described as partial.

## Completion checklist

| # | Condition | State |
|---|---|---|
| 1 | PostgreSQL governs immutable runtime profiles | **done** — `(runtime_profile_id, revision)` key, history retained, delete refused by trigger, content drift refused at sync |
| 2 | Every worker binds profile ID, revision and digest | **done** — three-column identity with a NULL-safe CHECK and a composite FK; dispatch capability refused without it |
| 3 | Candle remains active and backward compatible | **done** — full suite green, `candle_metal` still the only routable profile |
| 4 | A second runtime is REAL_RUNTIME_PROVEN through the full Merc chain | **NOT done** — engine proven, routing mechanism built, chain not driven. See below |
| 5 | RuntimeSelector produces shadow decisions and regret measurements | **NOT done** — not started |
| 6 | Routing has not changed without promotion evidence | **done** — `llama_cpp_metal` is still `VALIDATED`; the advertised projection is still exactly the two candle cells, asserted by test |
| 7 | In-flight coalescing works with one payable and independent discounted receipts | **Bound money-path proofs against a double upstream (commit `4ef1922a`).** `evidence/reuse/public-path-coalescing-128-to-1.json` and `evidence/reuse/public-path-128-to-1.json` show 128 deliveries to 1 upstream call through the real public handler; they prove control-plane money/receipt paths and do not measure GPU performance. The 2026-07-31 correction still records earlier store-level gaps; see `evidence/state/correction-2026-07-31-coalescing-and-directed-routing.json` (unbound correction record of withdrawn claims) |
| 8 | Tokenization / tool-schema caches with real callers and measured savings | **partial, closed honestly** — tool/schema identity cache is production-wired; control-plane tokenization **DOES_NOT_APPLY** (no tokenizer on control plane). Bound audit: `evidence/perf/five-cache-architecture-audit.json`. Do not build an empty tokenizer cache |
| 9 | Token-budget batching with measured policies per latency class | **NOT done** — not started |
| 10 | No calibration or overhead authority can affect money | **done** — call-graph gate, mutation-verified |
| 11 | Full suite green | **done** — on an isolated database; see the caveat below |
| 12 | Branch pushed | **done** |

## Runtime authority is cell-specific

Added after the profile-level lifecycle was identified as too coarse. The proof
boundary is finer than the profile: llama.cpp's embed cell is proven and its
byte_exact generation cell is measured unsuitable, so one lifecycle would let the
first promote the second.

Cells now carry their own lifecycle, benchmark authority, quality tier, rejection
reason and measured parallelism limits, and the CELL is the routable unit in both
the Go and Rust projections. A profile cannot inflate a cell and a cell cannot
outrank its profile. `REJECTED_FOR_CONTRACT` is a decision with a required stated
reason, not a synonym for unfinished. `REAL_RUNTIME_PROVEN` is evidence rather
than permission — reachable by directed routing, never by ordinary buyer traffic.

Receipts declare which models they measured, so a MiniLM comparison can no longer
be cited as evidence about Llama generation on the same engine.

## Directed routing

> **Corrected 2026-07-31.** `buildWorkloadDecisionDirected` has zero production
> callers. The mechanism is real and is reached only from tests; the operator
> half described below does not exist as an entry point. Every llama.cpp chain
> proof therefore submits through a test-constructed job row rather than through
> `POST /v1/jobs`.

An operator or a test can force a governed job onto a named cell. The name is a
server-side argument, never read from the buyer wire, and is frozen into the
decision so the choice is auditable and the stored decision reconstructs as
itself. The reachable set is VALIDATED and above with terminal states excluded —
the floor is VALIDATED because a cell reaches REAL_RUNTIME_PROVEN *by* being
driven through the chain, so requiring it first would make the state unreachable.

Building it surfaced a coupling that would have blocked any second runtime:
worker matching used the BUYER's declared model kind, so a request naming
`all-minilm-l6-v2` as `hf` could never reach llama.cpp's GGUF cell whatever the
evidence said. The frozen cell now supplies the kind and the scheduler compares
against that.

## What item 4 is actually missing

The engine half is proven and measured:

- llama.cpp executes the embed cell through `RuntimeDriver` and agrees with
  candle at **0.999998 minimum cosine** against the 0.999 gate the control plane
  applies to a `cosine`-verified cell — reproduced here, not cited;
- `evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json` binds
  source commit, both profiles' id and revision, the exact artifacts per wire
  kind, hardware, engine configuration, and the quality result;
- `EmbedRunner` dispatches by driver, so a llama.cpp worker can serve the cell it
  is registered for.

What has not been driven is those bytes through submit → claim → start → commit →
verify → settle → payable → receipt on `llama_cpp_metal`. That is the whole of
what stands between `VALIDATED` and `REAL_RUNTIME_PROVEN`, and the lifecycle has
deliberately not been moved without it.

The concrete blocker is a fixture, not a design gap. Verification settles against
a stored result artifact, and every verification test in this tree constructs its
processor with a `nil` Storage — they exercise leasing and drain mechanics, never
an artifact round trip. Driving the chain end to end needs an object-storage
fixture (the compose MinIO is available to `make dev-up` but no test uses it).
That fixture is the next piece of work, and it unblocks the chain for both cells
at once.

## Autonomous two-agent product chain

Two real `merc-agent` processes, two suppliers, two engines, against a control
plane running the production `Routes()`. Every assertion reads what the control
plane stored.

| Stop condition | State |
|---|---|
| 1. Two enrolled agents claim autonomously | **done** |
| 2. Both valid jobs verify and finalize | **done** — `task=complete`, `verification=pass` |
| 3. Both buyer debits reconcile | **done** — ledger conserves |
| 4. Both supplier payables correctly attributed | **done** — one row each, distinct suppliers |
| 5. Both Merc contributions positive | **done** — 0.393166 USD each |
| 6. v2 rejection creates no success payment | **done** — outcome `fail`, 0.000000000 USD net, reputation 0.95→0.801 |
| 7. Both final receipts verify | **done** — authority names the executing cell; ten tamper mutations refused |
| 8. llama.cpp embed REAL_RUNTIME_PROVEN | **done** — profile r7→r8, cell promoted, routable=false |
| 9. Candle remains ACTIVE | **done** |
| 10. llama.cpp generation remains rejected | **done** |
| 11. Full suite green | **done** |
| 12. Branch pushed | **done** |

Measured, identically on both cells:

```
candle    task=complete verification=pass  cell=candle-metal-minilm-embed    kind=hf
llama_cpp task=complete verification=pass  cell=llama-cpp-metal-minilm-embed kind=gguf

buyer_charge    -0.587166000
platform_take   +0.393166000
supplier_credit +0.194000000
                ------------
residual                   0

cross-cell equivalence: mean=0.999999 min_row=0.999999 revision=embed-cosine-v2
```

Six server-side defects were found by running real agents, none of which code
reading had surfaced: the advertised engine was hardcoded to candle; the agent's
capability projection was routable-only; worker profile resolution was
routable-only; `ValidateWorkerAgainstProfile` required a routable lifecycle;
`validEngines` was a hand-written `{candle, vllm}` literal that refused
`llama_cpp` at the door; and `agent.toml` requires `power_only` with no serde
default.

### Rejection economics

A honeypot submitted beside a primary, with a compute plan declaring both, graded
against the approved answer for DIFFERENT text:

```
honeypot verification outcome: "fail"
0 supplier rows netting 0.000000000 USD
reputation 0.95 -> 0.801, honeypot_fail event recorded
task requeued (status=retrying)
```

Two assumptions were corrected against what the policy actually does. Quarantine
is NOT the consequence of a wrong answer — it is reserved for an answer-class
mismatch, where the supplier's engine identity does not match what it claimed,
which is a different accusation. And "pays nobody" means the credit does not
STAND: the supplier is credited on commit and the grade arrives afterwards, so
the assertion is on the NET across credit and clawback.

A claim was also retired along the way. The settlement commit said the llama.cpp
task was graded against candle's approved output; it was not. The seeding ran an
`UPDATE` before `SubmitJobTx` inserted the row, so it matched nothing and the task
stayed ordinary. Both artifacts genuinely agree, so a passing comparison and an
unrun one were indistinguishable there — a check that could not fail. Withdrawn
in the tree and in the history.

### Promotion

`llama_cpp_metal` r7 → r8, profile and embed cell VALIDATED →
REAL_RUNTIME_PROVEN, receipt at `evidence/chain/two-agent-product-chain.json`
(unbound historical chain receipt; directed/test routing only, not candidate-bound).

REAL_RUNTIME_PROVEN is reachable by directed operator or test routing only. The
cell is **not** routable, Candle remains ACTIVE with both cells routable, and
`llama-cpp-metal-llama1-infer` remains REJECTED_FOR_CONTRACT.

The lifecycle guard that gated this used to hardcode "only candle_metal has a
Merc-chain receipt". That was true when written and became false the moment
llama.cpp completed the chain; naming a profile made the rule un-passable by
design. It now requires an existing chain or canary receipt, so the next runtime
can satisfy it and a profile claiming the state with nothing behind it still
fails.

### Not established

The failure matrix — agent death before and after claim, runtime unavailability,
input download failure, upload interruption, verifier restart, finalizer restart,
settlement retry — is not exercised. No CANARY or ACTIVE promotion follows from
this receipt.

## Caveats that are not caveats about this branch

**The shared development database is polluted.** `postgres://cx@localhost/cx`,
which `make ci` defaults to, holds 82 orphaned test-fixture jobs dating to
2026-07-28 with `workload_decision IS NULL`. The scheduler's frozen-runtime
filter has a legacy `workload_decision IS NULL OR …` escape hatch, so those
orphans are claimable by any worker, and two claim-ordering tests fail there
while passing against a freshly applied database. Every result in this tranche
was taken on an isolated database, as the tranche requires. The leak predates
this branch.

## Two findings worth carrying forward

**A revision bump had never been exercised against a populated database.** The
first real one — forced by widening the content digest — failed three separate
ways, each invisible against an empty table: insert-before-demote against a
partial unique index, a superseded revision still holding its cells routable, and
a child-row backfill that collapsed two revisions onto one key.
`TestRevisionBumpSucceedsAgainstAPopulatedRegistry` now drives it the way an
upgrade does.

**The exact-result cache was shared across tenants.** Not a bug in coalescing —
a pre-existing side channel that wiring coalescing forced into view. The bytes
were identical either way, so no correctness test could see it; the leak was that
buyer B could learn buyer A had run a request by watching it return at the reuse
price. `RequestIdentity` is tenant-scoped now and refuses an empty scope rather
than hashing it as the empty string.

## Grok

`DEFERRED_USAGE_LIMIT`. Not auth-blocked, not awaiting an external action. The
queued audits keep their contracts and are re-runnable when usage returns:

| audit | contract |
|---|---|
| runtime authority | is `(id, revision)` immutability actually enforced end to end, including the digest's coverage of resolved artifacts |
| plan actuals | can any money or admission path consume a calibration read — adversarially, against the new call-graph gate |
| selector promotion | not yet applicable; no selector exists to audit |
| coalescing privacy/money | can any caller obtain another tenant's result, or a second supplier payable, through the in-flight path |
| batching benchmark integrity | not yet applicable; no batching sweep exists to audit |

No acceptance condition in this tranche depended on Grok-specific evidence. The
direct adversarial work was done inline instead: the call-graph gate was
mutation-tested rather than inspected, and the coalescing money and tenant
properties are asserted by tests that fail if either is broken.


<!-- source: docs/OFFER_MULTIPLICITY.md -->

# One capacity row is one capacity row

**Verdict (2026-08-03): the multi-buyer single-offer authorize tail
(~50–75 ms p95 at c=32) is a thin-book / fixture characterisation, not a
production defect to re-architect capacity around.**

Do not reopen slot-row splitting, counter redesign, or lock-hierarchy
reordering against this number without first measuring a live multi-supplier
book for the same profile. The multi-offer path is already fixed.

## What one offer row is

| Fact | Authority |
| --- | --- |
| One row = one `(worker_id, runtime_profile_id)` | `control/schema.sql` `PRIMARY KEY (worker_id,runtime_profile_id)` on `realtime_worker_offers` |
| Upsert replaces that worker's live capacity for that profile | `Store.UpsertRealtimeOffer` in `control/realtime_store.go` |
| Agent registers **one** offer per vLLM session/profile | `agent/src/vllm.rs` (`register_realtime` after healthy pin) |
| `max_active_sequences` / `available_sequences` is a **counter of concurrent sequence slots on that worker**, default 128 | `agent/vllm.example.toml`, heartbeat clamp in `HeartbeatRealtimeOffer` |
| Not one GPU, not one supplier, not one slot row | Placement (`gpu_count`, `hw_class`) is metadata on the same offer row |

So "more rows" means **more workers advertising the same runtime profile**, not
finer-grained accounting of one worker's sequences. Multiplicity is a **supply
fact** (how many independent workers registered), not a modelling bug in the
counter.

Heartbeat does not invent free capacity: it clamps the advertised value by
in-flight `EXECUTING` contracts. Admit decrements atomically; finalize /
failure / stale recovery release via `releaseRealtimeCapacity`.

## Offers per (model, job type) in every catalogue examined

Realtime routing keys on **runtime profile** (and its model alias), not batch
job type. Counts below are **active offer rows that can clear a chat request
for that profile**.

| Environment | Offers per realtime profile | Source |
| --- | ---: | --- |
| Seeded dev (`make seed` / `control/seed.go`) | **0** until an agent registers | Seed inserts 2 batch workers (`demoWorkerID`, `demoWorkerID2`) for embed/batch_infer only. **No** `realtime_worker_offers` rows. |
| Local agent after register | **1** per agent process × profile | One `register_realtime` per pinned profile (`vllm.rs`). Typical laptop: one worker → one offer for `cx-chat-1b`. |
| VLLM profile catalogue in-repo | **1 profile** today | `control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json` → `cx-chat-1b` |
| Level-B / go-closure canary design | **2** distinct approved workers | `docs/LEVEL_B_OPERATOR_HANDOFF.md`, `ops/go-closure-inputs.json` ("two distinct operator-controlled Metal worker identities") — still a thin book, not a marketplace. |
| Tail probe 1-offer cell | **1** (deliberate) | `TestAuthorizeTailCharacterize` drains all other offers. |
| Tail probe N-offer cell | **N = concurrency** equal-rank offers | Same test; SKIP LOCKED fans out. |
| Intended production marketplace | **N independent suppliers/workers** | Order book ranking, liquidity slices (`current_active_offers`), multi-offer SKIP LOCKED path. |

There is **no production multi-tenant offer census** in-repo (no live fleet
receipt with offer multiplicity). What the repo knows is: seed and local
single-agent paths are thin by construction; canary is two workers; the product
code and probes assume a multi-offer book is the healthy shape.

## Why the ~50–75 ms number appears

Bound evidence: `evidence/perf/authorize-auth-tails-latest.json`
(phase `after_skip_gated_by_offer_count`).

At max_conns=64, c=32 (multi-buyer):

| Cell | p50 ms | p95 ms |
| --- | ---: | ---: |
| 1 offer | ~22 | **~76** |
| N=32 equal-rank offers | ~5 | **~10** |

Same-buyer 1-offer is higher still (~100–200 ms p95) and is owned by the
**buyer funding lock** lane, not this one.

Mechanism (unchanged hierarchy: buyer funding → offer capacity → insert →
commit):

1. Admit takes `FOR UPDATE` on the chosen `realtime_worker_offers` row.
2. PostgreSQL holds that row lock until **transaction commit**, not just the
   UPDATE statement.
3. `available_sequences > 0` decrement remains atomic and correct.
4. With one candidate, every concurrent admit queues on that one row.
5. With many candidates, `FOR UPDATE SKIP LOCKED` (gated by
   `activeOfferCount > 1` in `AuthorizeRealtimeContract`) lets contenders take
   other free offers — already measured ~3–8× better on multi-offer cells.

**One capacity row is one capacity row.** That is physical lock semantics, not
an accidental missing index.

## Is `available_sequences` the wrong granularity?

| Option | Effect on single-offer multi-buyer | Capacity truth | Status |
| --- | --- | --- | --- |
| Keep one counter row per worker×profile | Serialises concurrent admits on that worker for the TX duration | Strong: decrement + contract insert in one TX; release on terminal | **Current** |
| Row-per-slot | Parallel claims on distinct free slots of the same worker | Feasible but multiplies bookkeeping; orphan-slot recovery must match finalize/void/stale paths | Not justified while multi-offer is the production shape |
| Decrement without holding lock through insert | Would shrink hold time only if claim and contract insert split | Splitting risks oversubscribe or stranded capacity across commit boundaries | Rejected for capacity truth |
| Capacity leases (many slots pre-claimed) | Amortises re-ranking; does not remove concurrent multi-buyer wait on one worker's remaining slots | Design only — `docs/CAPACITY_LEASES.md` | Future, after envelopes prove out |

Capacity truth is non-negotiable: a supplier must never be oversubscribed, and
an abandoned claim must not strand sequences. The current counter + lock
through insert satisfies that. Weakening it for a thin-book tail is the wrong
trade.

## What not to do

- Do **not** reverse buyer-funding-before-offer-capacity (deadlock 40P01;
  `TestAuthorizeSettlementDeadlockRepro`).
- Do **not** treat the 1-offer p95 as a standing Merc defect when N-offer p95
  is already ~10 ms at c=32 under the same probe.
- Do **not** invent slot rows to "fix" local `make seed` + one agent.
- Do re-open only if a **bound production or staging liquidity receipt** shows
  sustained single-offer books for a hot profile under multi-buyer load *and*
  that shape is intentional product reality rather than under-supply.

## Operational read

| Observation | Meaning |
| --- | --- |
| Multi-buyer 1-offer p95 high, N-offer low | Thin book / fixture. Grow supply or accept single-worker serialization. |
| Multi-buyer N-offer still high | Real defect (pool, funding, ranking CTE, etc.) — investigate. |
| Same-buyer ≫ multi-buyer 1-offer | Buyer funding lock lane (separate). |

## Related

- Tail factorial: `control/authorize_tail_characterize_test.go`
- Claim SQL + SKIP LOCKED notes: `control/realtime_supplier_outcome_stats.go`,
  `control/realtime_store.go` (`AuthorizeRealtimeContract`)
- Bound numbers: `evidence/perf/authorize-auth-tails-latest.json`
- Lease design (not implemented): `docs/CAPACITY_LEASES.md`


<!-- contradiction-ledger: mlx-throughput -->

## Contradiction ledger (unresolved)

These figures are intentionally left side-by-side. Merging is not resolution.

| Claim | Source | Notes |
|---|---|---|
| MLX peak **6,828 t/s** | `docs/SPEED_LANE_2026-07-27.md` | Speed-lane harness |
| MLX **310.7 t/s** | `docs/RUNTIME_CROSS_TEST_2026-07-30.md` | Cross-test harness; different setup |


