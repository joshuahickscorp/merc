# Ocular hard tracking fixtures

Ten conditions plus a permanence sequence for perception-driven multi-object
tracking. Objects share near-identical albedo; procedural textures differ so
colour histograms alone cannot solve identity.

## Layout

```
ocular_hard/
  conditions.py      # catalogue
  create_scene.py    # Blender EEVEE renderer
  synthetic.py       # OpenCV DIAGNOSTIC_ONLY substitute
```

Each rendered condition writes:

- `frames/` — RGB only (builder-visible)
- `sequence_manifest.json` — **no** ground-truth paths
- `sealed_gt/` + `sealed_manifest.json` — sealed evaluator only

## Conditions

visually_similar, crossing_paths, partial_occlusion, full_occlusion,
lighting_change, scale_change, camera_motion, leave_return,
distractor_replacement, unknown_entering, permanence.

## Render

```bash
# Via the runner (attested; falls back to synthetic on Blender crash)
scripts/run-ocular-tracking.py --output artifacts/ocular/tracking

# Direct Blender
blender --background --python benchmarks/ocular_hard/create_scene.py -- --output DIR
```
