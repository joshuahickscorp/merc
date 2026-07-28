# Datacenter film report (VisionMCP V2)

Flagship scroll-bound film produced by the procedural world engine, cinematic
compiler, and delivery stack. Authority is `PROCEDURAL_GROUND_TRUTH` throughout.

## What was built

A data-centre corridor film with:

- twenty rack/facility archetypes,
- 464 instances with mesh de-duplication,
- nine narrative beats on a continuous camera path,
- frozen delivery budgets,
- poster → shell → chapter-gated detail streaming.

Sandbox: `sandbox/datacenter-film/`.

## Receipt-backed measurements

From `sandbox/datacenter-film/assets/build-receipt.json` and
`delivery-manifest.json`:

| Quantity | Value |
| --- | --- |
| Modules / archetypes | 20 |
| Instances | 464 |
| Shell GLB bytes | 249,432 (budget 1,572,864) |
| Mobile shell bytes | 249,432 (budget 665,600) |
| Detail GLB bytes | 3,073,572 (chapter-gated) |
| Budget violations | 0 |
| Beats | 9 |
| Camera arc length | 27.331671650043138 m |
| Replay | deterministic (`replay_digest` present) |

Budgets are frozen and never widened to force a pass.

## Narrative beats

`00 THRESHOLD → 01 CAPACITY → 02 INFERENCE → 03 DISPATCH → 04 EXECUTION → 05 TURN → 06 VERIFY → 07 RECEIPT → 08 ACCESS`

The turn is a continuous spline event (not a hard cut). Prefetch of the second
corridor is planned before TURN scroll.

## Composition honesty

The corridor **reads** as a coherent path through capacity and verification.
Lighting and framing have **not** passed a human art director. Perceptual
quality scoring treats this as an open limitation, not a production art claim.

## How to run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-procedural-engine.py --output artifacts/v2/procedural
.venv/bin/python scripts/build-datacenter-film.py
.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic
```

Browser gates must use `scripts/with-one-browser.sh` and
`scripts/reap-browsers.sh` — at most one Playwright browser alive at a time.

## Related docs

- `PROCEDURAL_WORLD_ENGINE.md`
- `CINEMATIC_COMPILER.md`
- `WEB_SCENE_COMPILER.md`
- `VISIONMCP_V2_FINAL_SCORECARD.md`
