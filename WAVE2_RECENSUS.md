# Wave 2 recensus (reconstruct/event-horizon, off main 60e20df)

<!-- CLAIM-SCOPE: internal-engineering-non-authoritative -->

`GLOBAL_OWNED_LOC = 212,701` (total 297,863 - unmodified vendored 85,162). Target `<= 100,000`.
Regenerate: `make audit`.

## Category breakdown (owned, approximate)

| category | ~LOC | notes |
|---|--:|---|
| production (Go control + Rust agent core + proto + db + sdk) | ~95,000 | control ~45k non-test, agent ~30k core, proto 2.4k, sdk 0.85k |
| tests | 40,570 | target 16k |
| proof | 22,364 | registry + claims + fixtures + go evidence; target compact |
| experimental / unrouted | ~40,000 | token-spec-poc 6.1k, spec-engine 3.7k, renderer 2.5k, render 4.1k, scripts/spec-lab 10.3k, docker/vllm 8.7k, agent spec/render/transcode ~4.6k |
| compatibility | ~0.9k | control/openai.go (OpenAI surface duplicating native API) |
| design | ~15,000 | web owned ~9.5k (three.js 53k is third_party), macapp 5.7k |
| generated | ~1,000 | runtime_matrix_generated.{go,rs}, contract JSON |
| documentation | 12,048 | target 5k |
| third_party (vendored, EXCLUDED from owned) | 85,162 | candle-metal-kernels ~32k, three.module.js 53k |

## Deletion targets to CP1 (<=160k, ~-53k)

- vLLM patches distill (this wave, next commit): -6.8k
- docker/vllm lane (launcher/lock, CUDA-pending): -1.9k
- token-spec-poc (POC): -6.1k
- spec-engine (standalone, if no prod consumer): -3.7k
- renderer + render generation source: -6.6k
- scripts/spec-lab (render/spec research): -10.3k
- agent render_preview + resident_spec_shadow + slot_speculation: ~-3.7k
- control openai/spec_receipt/render_spec_job/transcode: ~-1.9k
- macapp Swift duplication: -5.7k (design contract retained)
- web demo/admin consolidation: ~-3k

Each deletion is coordinated (impl + tests + CI/registry gates + docs) and verified green
(prove-local matrix + GitHub CI). The spec/render/transcode lane is the largest single
lever; it is "unrouted speculative execution" per the wave brief unless it earns CORE.
