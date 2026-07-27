# Organic geometry and fur (VisionMCP V2)

Companion to `SOFT_ORGANIC_FUR_REPORT.md`. This document names the organic and
grooming subsystem surface, the synthetic claim boundary, and the measured
evidence that exists at the current head.

## Packages

| Package | Role |
| --- | --- |
| `blender_vision.organic` | Retopology, topology metrics, UV measurement, organic build scripts |
| `blender_vision.grooming` | Guide / guard / undercoat groom generation and critique hooks |
| `blender_vision.benchmarks.objects` | Soft / organic / fur object-benchmark phases |

## Targets

| Target | Class | Authority | Notes |
| --- | --- | --- | --- |
| `draped_cloth` | soft | `PROCEDURAL_GROUND_TRUTH` | Open sheet topology |
| `organic_sculpture` | organic | `PROCEDURAL_GROUND_TRUTH` | Known-open UV packing |
| `plant` | organic | `PROCEDURAL_GROUND_TRUTH` | Known-open UV packing |
| `animal_bust` + groom | fur | `PROCEDURAL_GROUND_TRUTH` | **Synthetic only** |

## Synthetic fur labelling (mandatory)

Every animal_bust / fur artifact must carry:

> Synthetic target with known construction parameters. This is not evidence
> about any real animal; the real-animal lane remains blocked on an authorized
> multiview capture set.

The sealed `fur_animal` contract remains `evidence_status=blocked` until a
rights-cleared multiview capture and measured groom reference are supplied.
See `benchmarks/fur_animal/resumption_contract.json` and
`artifacts/v2/sealed/framework.receipt.json`.

## Measured evidence at this head

From `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` and
its source packet:

| Quantity | Value |
| --- | --- |
| Faces | 5,814 all-quad |
| Watertight | true |
| Genus estimate | 0 |
| UV islands | 34 |
| UV packing (bust) | ~0.62 |
| p99 angle distortion | ~22.3° |
| LOD L3 silhouette IoU | ~0.99137 |
| Guide count | 642 |
| Guard strands | 3,852 |
| Undercoat strands | 6,420 |

## Known-open UV packing failure

Gate `min_uv_packing = 0.35` is **not relaxed**.

| Target | packing_efficiency | Status |
| --- | --- | --- |
| `organic_sculpture` | ~0.290 | fail (known-open) |
| `plant` | ~0.281 | fail (known-open) |
| `animal_bust` | ~0.620 | pass |
| `draped_cloth` | ~0.507 | pass |

Healthy bust packing must never be used to hide branching-form failure.

## How to run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic
.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks
```

Scorecards land under `artifacts/v2/object-benchmarks/organic/<target>/`.
