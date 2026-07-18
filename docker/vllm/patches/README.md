<!-- CLAIM-SCOPE: internal-engineering-non-authoritative -->
# Vendored vLLM CX patches

The ComputExchange vLLM speculative-decoding lane no longer depends on the
external `joshuahickscorp/vllm` and `joshuahickscorp/vllm-metal` forks. Their
CX-unique deltas are vendored here as patches on top of the pinned **upstream**
commits, so the runtime is rebuildable from upstream + a local patch, with no
private fork required.

## `vllm-custom-proposer-context.patch`

- **Apply on:** `vllm-project/vllm` at v0.24.0, commit
  `ee0da84ab9e04ac7610e28580af62c365e898389` (the fork baseline was zero-delta
  from this upstream tag).
- **Adds:** the versioned full-context custom-proposer seam
  (`vllm/v1/spec_decode/custom_class_proposer.py`,
  `vllm/v1/worker/gpu_model_runner.py`) + its test and doc. ~280 line delta.

```
git clone https://github.com/vllm-project/vllm && cd vllm
git checkout ee0da84ab9e04ac7610e28580af62c365e898389
git apply /path/to/docker/vllm/patches/vllm-custom-proposer-context.patch
```

## `vllm-metal-spec-decode.patch`

- **Apply on:** `vllm-project/vllm-metal` at commit
  `4c18ee0e6e3ce2b594ab114d0a53ca24eafb1d58` ("Bump MLX to 0.32.0", an upstream
  commit; the fork baseline was zero-delta from it).
- **Adds:** the CX spec-decode paired parity gate, ngram/sonnet exercise fixture
  builders, and the historical C1 ngram divergence/endpoint reproductions
  (`tools/`, `tests/`) plus the `ngram_proposer.py` changes they exercise.

```
git clone https://github.com/vllm-project/vllm-metal && cd vllm-metal
git checkout 4c18ee0e6e3ce2b594ab114d0a53ca24eafb1d58
git apply /path/to/docker/vllm/patches/vllm-metal-spec-decode.patch
```

Regenerate a patch from an updated working tree with `git diff <base>..HEAD`.
