# Phase O — Consumer object (remote) benchmark

## Claim boundary

This report describes a **procedurally constructed, self-captured fixture**. It
is **not** a claim about any physical remote control, including any remote the
user may own. Real-photograph resumption is documented in
`benchmarks/remote/capture_protocol.md` and
`benchmarks/remote/resumption_contract.json`.

## What was built

| Item | Value |
|------|--------|
| Target id | `consumer_remote` |
| Body dimensions (construction) | 180 × 60 × 25 mm |
| Features | 4×2 buttons, side seams, underside battery hatch |
| Train views | ≥ 24 |
| Holdout views | ≥ 8 (builder-denied) |
| Authority | `PROCEDURAL_GROUND_TRUTH` for construction; reconstruction candidates use `derive()` |

## Pipeline

1. **Source packet** — construction parameters, train view list, holdout ids.
2. **Sealed builder** — reconstruction portfolio from train images only
   (visual hull, depth fusion, parametric, points, COLMAP sparse SfM, retrieval,
   browser runtime). Dense COLMAP MVS remains blocked without CUDA.
3. **Hidden-surface ledger** — underside, hatch outer, battery compartment
   interior, internals: `NEVER_OBSERVED` with authority ceiling `INFERRED`.
   Button sidewalls: `PARTIALLY_VISIBLE`.
4. **Materials** — body vs button separation from train pixels; hatch material
   not estimated from observation.
5. **Unseen-view evaluation** — holdout renders vs builder mesh (PSNR / SSIM).
6. **Dimensional evaluation** — bounding box error in millimetres vs construction.
7. **Next-view planner** — requests underside, grazing light, lens metadata, etc.
8. **Delivery** — editable OBJ/PLY/GLB, offline proxy frames, web LODs, manifest.

## Environment notes

- Blender 4.2.1 may SIGSEGV during Metal GPU init in restricted sandboxes. When
  that happens the receipt records `blender_blocker` and uses a labelled
  software raycast path. That is not a silent fallback.
- COLMAP sparse SfM runs when available; dense MVS is blocked:
  `COLMAP built without CUDA; patch_match_stereo unavailable`.

## How to run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-object-benchmarks.py \
  --output artifacts/v2/object-benchmarks
```

Scorecard: `artifacts/v2/object-benchmarks/remote/scorecard.json`.

## Resumption (real photographs)

See `benchmarks/remote/capture_protocol.md`. Minimum: 24 orbit views, scale
reference, lens metadata; underside required or left `NEVER_OBSERVED`.
