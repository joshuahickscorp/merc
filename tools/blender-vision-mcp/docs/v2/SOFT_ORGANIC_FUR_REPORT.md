# Phase P — Soft / organic / fur benchmarks

## Targets

| Target | Phase | Ground truth | Notes |
|--------|-------|--------------|-------|
| `draped_cloth` | soft | Cloth sim construction | Open sheet topology |
| `organic_sculpture` | organic | Branching procedural form | Known-open UV packing |
| `plant` | organic | Stem + leaves | Known-open UV packing |
| `animal_bust` + groom | fur | **Synthetic only** | Never evidence about a real animal |

## Synthetic fur labelling

Every animal_bust / fur artifact carries:

> Synthetic target with known construction parameters. This is not evidence
> about any real animal; the real-animal lane remains blocked on an authorized
> multiview capture set.

Resumption for a real animal requires authorization, multiview capture, scale
reference, and ethics/rights fields — see
`benchmarks/fur_animal/resumption_contract.json`.

## Known-open UV packing failure

Pinned by `tests/test_v2_organic.py` and reasserted here:

- Gate: `min_uv_packing = 0.35`
- `organic_sculpture` and `plant` pack to ~29% with the current unwrap
- **The gate is not relaxed.** Either packing is genuinely improved and
  remeasured, or the failure is carried forward unchanged.

## Scorecard contents (per target)

- Source packet
- Sealed builder status (Blender organic lane or explicit blocker)
- Geometry portfolio with backend chamfer vs construction / receipt truth
- Dimensional error (mm)
- Unseen-view PSNR / SSIM
- Hidden-surface ledger
- Materials hypothesis set
- Topology / UV (from organic receipt when present)
- Fur groom metrics (when receipt present)
- Editable asset + web LOD + offline proxy
- Failures and blockers preserved

## Coupling to organic lane

Full retopo / UV / LOD / groom execution lives in
`scripts/run-organic-fur-lane.py`. When its receipt exists at
`artifacts/v2/organic/organic-fur-receipt.json`, Phase P loads measured
topology/UV/fur into the scorecard. When Blender is blocked, construction
envelope meshes are used for geometry scoring and Blender stages are
`blocked` with the probe reason.

## How to run

```bash
# Preferred: organic lane first (real Blender hardware)
.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic

# Then object benchmarks (Phases O + P)
.venv/bin/python scripts/run-object-benchmarks.py \
  --output artifacts/v2/object-benchmarks
```

## Sealed framework note

`benchmarks/sealed.py` is not present on this branch. The local sealed contract
is `blender_vision.benchmarks.objects` (source packet → builder → hidden
evaluator → scorecard). If a shared sealed harness lands later, this module
should adopt it rather than diverge.
