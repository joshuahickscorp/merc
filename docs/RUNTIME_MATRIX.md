# Runtime capability matrix

`control/runtime-authority.json` is the sole authority for admission,
advertisement, scheduling, the database model catalog, and agent dispatch. Go
embeds it and Rust includes the same bytes; both bind dispatch to its SHA-256.

Version: `2026-08-02.12`

| Workload | Model | Engine | Device | Hardware | Verification | Lifecycle |
|---|---|---|---|---|---|---|
| `embed` | `all-minilm-l6-v2` | Candle | Metal | Apple Silicon base/pro/max/ultra | cosine | ACTIVE (routable) |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | ACTIVE (routable) |
| `media_transcode` | `ffmpeg-transcode-v1` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | CANARY (routable) |
| `media_rendering` | `svg-scene-render-v1` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact | CANARY (routable) |
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

The serving-engine tournament harness lives in `control/serving_matrix.go`. It
runs the same corpus against any OpenAI-compatible engine under a documented
subset of the full concurrency × prompt × output × state × lane × precision
space, refuses mismatched model digests and sub-5× prompt counts as
incomparable, refuses unsupported points with a reason rather than skipping
them, and evaluates the budget gate at every claimed concurrency level.

CPU execution is a test fallback and is never advertised. Hardware identity is
currently self-declared; remote physical attestation is a named production
limitation.
