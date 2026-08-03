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
