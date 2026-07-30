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
