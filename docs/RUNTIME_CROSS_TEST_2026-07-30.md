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
