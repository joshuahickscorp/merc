# Synthetic calibration benchmark

This CC0 benchmark generates a known 120 × 80 × 40 mm technical object with a rounded body, three
ports, a fan ring, 35 grille markers, four screws, and exact component metadata. The safe synthetic
dataset worker then produces beauty, instance-mask, depth/normal, keypoint, lighting, and camera
records without network access.

The governed path creates owned six-view references, imports exact metric cameras and dimensions,
runs the scene audit and residual comparisons, repeats the full validation render and GLB export,
and fails unless all five calibration gates and the L3 receipt are accepted and verified. The five
gates cover known dimensions, metric-camera round trip, recovered canonical scale, bounded decoded
pixel repeatability, and byte-identical GLB exports. It still requires an explicit named reviewer:

```bash
uv run bvmcp benchmark bootstrap-calibration \
  --project /absolute/path/to/calibration-project \
  --reviewer "Named calibration reviewer" \
  --review-reason "Procedural geometry, cameras, features, and residuals reviewed"
```

The standalone Blender scene generator remains available for inspecting the fixture without
creating a governed project:

```bash
BLENDER=/Applications/Blender.app/Contents/MacOS/Blender
"$BLENDER" --background --factory-startup --python create_scene.py -- calibration.blend
```

Ground truth and acceptance tolerances are in `benchmark.json`. Both files are included in release
wheels so the public package has a distributable reference benchmark with no third-party pixels.
