# Isolated vLLM 0.24.0 candidate provenance

This directory is a non-promoting candidate anchor. It is not connected to
Compute Exchange routing, the runtime matrix, scheduling, or production
advertising. A lock with `status: candidate` cannot be launched with
`--mode production`.

## Upstream identity

The following values were verified on 2026-07-10 and are locked by both the
schema and the dependency-free validator:

| Material | Locked identity | Authoritative source |
| --- | --- | --- |
| vLLM release | `0.24.0` | <https://github.com/vllm-project/vllm/releases/tag/v0.24.0> |
| Source tag commit | `ee0da84ab9e04ac7610e28580af62c365e898389` | <https://github.com/vllm-project/vllm/commit/ee0da84ab9e04ac7610e28580af62c365e898389> |
| Docker multi-platform index | `sha256:251eba5cc7c12fed0b75da22a9240e582b1c9e39f6fbc064f86781b963bd814f` | `vllm/vllm-openai:v0.24.0`, inspected with Docker Registry metadata |
| Docker Linux/AMD64 manifest | `sha256:f9de5cd9fa907fbf6dbba691eb7db095d48ad58ea283e3eba7142f9a91e186e8` | `vllm/vllm-openai:v0.24.0`, inspected with Docker Registry metadata |
| PyPI Linux x86-64 wheel | `sha256:2d2831aeba311292250df0132dbc4d8e9f42c654536eaec48e6fe58acb1822cf` | <https://pypi.org/pypi/vllm/0.24.0/json> |
| PyPI source distribution | `sha256:0862453adc1f3339f1a0c9dca1179c34d6ed6e118f87b6e5bddd120af614ac66` | <https://pypi.org/pypi/vllm/0.24.0/json> |
| Container CUDA runtime | `13.0.2` | Image configuration for the locked Linux/AMD64 manifest |
| Container PyTorch version | `2.11.0` | Image configuration for the locked Linux/AMD64 manifest |

The launcher requires the official image's `VLLM_BUILD_COMMIT` and an
orchestrator-supplied `CX_VLLM_IMAGE_DIGEST` to match the lock. A tag such as
`latest` or `v0.24.0` is never accepted as runtime identity.

## Model identity is locally resolved; CUDA behavior is not

The existing local Hugging Face cache resolved the catalog artifact without a
network request. The template now binds:

- model repository commit `b69aef112e9f895e6f98d7ae0949f72ff09aa401`;
- `Llama-3.2-1B-Instruct-Q4_K_M.gguf`, independently hashed as
  `3f5a22426976ab26cfe84dba63c1d08391717abb1af893e10f1b2968d862dcc1`;
- tokenizer repository `unsloth/Llama-3.2-1B-Instruct` at commit
  `5a8abab4a5d6f164389b1079fb721cfab8d7126c`; and
- the SHA-256 of the checked-in canary prompt.

Before this anchor can pass validation, a CUDA vLLM 0.24.0 soak must replace
the remaining `REQUIRED_*` values with:

1. the measured attention backend (`FLASH_ATTN`, `FLASHINFER`, or
   `TRITON_ATTN`);
2. the winning speculative method (`ngram` or `suffix` only for schema version
   1); and
3. the exact expected-completion SHA-256 produced by that fully pinned runtime.

The validator rejects missing keys, unknown keys, duplicate JSON keys,
placeholders, floating revisions, tagged container images, mismatched prompt
digests, and non-finite numbers. The launcher hashes the local GGUF before it
executes vLLM.

### Lock-to-runtime enforcement map

No `execution` field is merely descriptive:

| Lock field | Enforcement |
| --- | --- |
| `weight_format` | passed as `--load-format gguf` |
| `quantization` | `q4_k_m` is encoded in the hash-verified GGUF; it is intentionally not passed as vLLM's `--quantization`, whose values describe loader implementations rather than GGUF quant variants |
| `dtype` | passed as `--dtype` |
| all three parallel sizes | passed as tensor, pipeline, and data parallel CLI arguments |
| `resolved_kv_cache_dtype` | passed explicitly as `--kv-cache-dtype`; vLLM's `auto` default is never used |
| model, sequence, and batched-token limits | passed explicitly as CLI arguments |
| GPU-memory utilization and seed | passed explicitly as CLI arguments |
| `trust_remote_code: false` | enforced by a fixed command builder that never emits `--trust-remote-code` and accepts no free-form extra arguments |
| attention and compilation objects | serialized canonically and passed as `--attention-config` and `--compilation-config` |
| network host and port | passed explicitly; schema version 1 only permits loopback |

The exact GGUF digest also enforces the model revision, artifact filename, and
quantization bytes at the local-file boundary. The tokenizer repository and
commit are passed explicitly. The locked image manifest transitively fixes the
recorded CUDA and PyTorch builds; package version, image manifest, and upstream
build commit are checked again by the launcher before execution.

## Validation and candidate launch

The checked-in template is expected to fail until its operator-supplied
identity is complete:

```console
python3 docker/vllm/validate_runtime_lock.py \
  docker/vllm/v0.24.0-candidate.template.json
```

After copying and completing the template outside the repository:

```console
python3 docker/vllm/validate_runtime_lock.py /run/cx/vllm-runtime-lock.json
python3 docker/vllm/launch_pinned.py \
  --lock /run/cx/vllm-runtime-lock.json \
  --model-dir /models \
  --mode soak
```

The scheduler must start the image by its Linux/AMD64 manifest digest and set:

```text
CX_VLLM_IMAGE_DIGEST=sha256:f9de5cd9fa907fbf6dbba691eb7db095d48ad58ea283e3eba7142f9a91e186e8
```

`CX_VLLM_API_KEY` is optional for loopback candidate soaks and is required for
any future production launch. It is never stored in the lock or printed by a
dry-run.

## License boundary

vLLM is Apache-2.0 licensed. `LICENSE.vllm` is the verbatim license text from
the locked upstream commit:
<https://github.com/vllm-project/vllm/blob/ee0da84ab9e04ac7610e28580af62c365e898389/LICENSE>.
The upstream commit has no root `NOTICE` file. This anchor vendors no vLLM
source code; it records upstream identity and invokes the separately supplied
runtime. If code is later copied or modified, retain the license, source
attribution, and prominent modification notices for each copied file.

## Vendored CX patches (forks retired)

The former `joshuahickscorp/vllm` and `joshuahickscorp/vllm-metal` forks are
retired. Their CX-unique deltas are vendored under
[`patches/`](patches/README.md) as diffs on top of the pinned **upstream**
commits, so the speculative-decoding lane rebuilds from upstream vLLM + a local
patch with no private fork. See `patches/README.md` for the exact upstream base
commits and `git apply` recipe.
