# Web Scene Compiler (Delivery)

Measured LOD, compression, streaming, and sealed `DeliveryManifest` production.

## Modules

| Module | Role |
| --- | --- |
| `blender_vision.delivery.lods` | Real Blender decimation LODs + silhouette identity IoU |
| `blender_vision.delivery.compress` | Measure raw / gzip / zlib / brotli / Draco / meshopt / quantize; select per asset |
| `blender_vision.delivery.stream` | poster → shell → first frame → detail → junction prefetch → terminal |
| `blender_vision.delivery.manifest` | Seal `v2.delivery-manifest` with budgets and honest violations |

## Frozen budgets

| Budget | Limit |
| --- | --- |
| Initial JS (compressed) | ≤ 300 KB |
| Shell GLB | ≤ 1.5 MB |
| Mobile shell | ≤ 650 KB |
| Poster before GLB | required |
| Detail chapter-gated | required |

A violated budget appears in `budget_violations`. The budget is never raised to
hide a breach.

## Compression doctrine

Codec choice is **measured**, not assumed:

1. Encode/decode each available option on the actual payload.
2. Record bytes, decode time, main-thread time, visual difference.
3. Score and pick per asset; record the reason and the runner-up.

Unavailable codecs are reported as blocked with an exact reason:

- **brotli** — Python package not installed.
- **Draco** — no standalone encoder; Blender extension not assumed.
- **meshopt** — no meshoptimizer binding installed.

## Streaming order

1. Poster (before any GLB)
2. Shell GLB + stable first frame
3. Detail enrichment (chapter-gated from CAPACITY)
4. Junction / second-corridor prefetch (before TURN)
5. Terminal assets (VERIFY+)
6. Mobile shell (parallel budget track)

Native scroll only. No custom scroll driver.

## Evidence

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic
BVMCP_RUN_BLENDER_TESTS=1 .venv/bin/python -m pytest -q tests/test_v2_delivery.py
```
