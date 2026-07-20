# Runtime capability matrix

`control/runtime-authority.json` is the sole authority for admission,
advertisement, scheduling, the database model catalog, and agent dispatch. Go
embeds it and Rust includes the same bytes; both bind dispatch to its SHA-256.

Version: `2026-07-19.2`

| Workload | Model | Engine | Device | Hardware | Verification |
|---|---|---|---|---|---|
| `embed` | `all-minilm-l6-v2` | Candle | Metal | Apple Silicon base/pro/max/ultra | cosine |
| `batch_infer` | `llama-3.2-1b-instruct-q4` | Candle | Metal | Apple Silicon base/pro/max/ultra | byte exact |

These are exact cells, not a Cartesian product. Unknown job, model, engine,
device, or hardware values fail closed. CPU execution is a test fallback and is
never advertised. Hardware identity is currently self-declared; remote physical
attestation is a named production limitation.
