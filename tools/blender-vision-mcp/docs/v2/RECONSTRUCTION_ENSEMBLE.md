# VisionMCP V2 — Reconstruction Ensemble

The reconstruction ensemble never relies on a single method. Every claim carries
an authority class, evidence references, and uncertainty. Derived claims use
`blender_vision.v2.authority.derive()` and cannot outrank their weakest input.
Inferred or retrieved structure is never labelled OBSERVED.

Package: `blender_vision.reconstruction`.

## Common contract

Each backend implements:

| Member | Meaning |
|--------|---------|
| `name` | Stable backend id |
| `availability()` | `BackendAvailability` with real environment state |
| `run(inputs)` | Returns a `ReconstructionCandidate` |

`executed=True` is set only after real work produces artifacts or topology
evidence. Unavailable backends appear in the portfolio with an explicit reason.

Portfolio assembly: `portfolio.build_portfolio` → sealed
`v2.reconstruction-portfolio` record, schema-validated via
`v2.validation.verify_payload`.

Comparison: `compare.compare_candidates` reports chamfer, volume ratio, surface
area ratio, coverage overlap, topology, and per-metric winners. There is **no**
single scalar score that can hide disagreement.

Fusion: `fusion.fuse_candidates` refuses incompatible coordinate frames, units,
scale authority, or target identity with typed `FusionError`. Permitted modes:

1. `observed_plus_procedural_shell`
2. `depth_plus_measured_dimensions`
3. `retrieved_plus_observed_face`

Fusion output carries `derive()` authority and a hidden-surface ledger entry for
every non-observed region introduced.

## Backends

### 1. `colmap_sfm` — classical sparse SfM

| | |
|--|--|
| **What it does** | Runs real COLMAP `feature_extractor`, `exhaustive_matcher`, `mapper`. Parses `cameras.bin` / `images.bin` / `points3D.bin` (text fallback). Emits sparse point cloud + camera JSON. |
| **Authority** | `SENSOR_DERIVED`. `scale_authority` is `UNRESOLVED` unless a metric anchor is supplied. |
| **Measured accuracy** | Target-dependent. Ensemble reports registered image count and mean reprojection error from COLMAP’s own point errors. Chamfer vs truth is only meaningful after metric alignment. |
| **Limitations** | Sparse points only — not a surface. Textureless / reflective scenes fail registration. **Dense MVS (`patch_match_stereo`) is UNAVAILABLE when COLMAP is built without CUDA**, with exact reason: `COLMAP built without CUDA; patch_match_stereo unavailable`. Never silently substituted. |

### 2. `visual_hull` — silhouette carving

| | |
|--|--|
| **What it does** | Voxel grid; project each voxel into every mask; carve; `skimage.measure.marching_cubes` surface. |
| **Authority** | `SENSOR_DERIVED` (capped by inputs). |
| **Measured accuracy** | On synthetic sphere/box with known cameras, coarse grids (≈36–40³) recover volume within ~40% relative error (hull overestimates concavity-free solids). |
| **Limitations** | Fills unobserved concavities. Metric only with metric cameras and bounds. Existing project-governed path remains in `vision.visual_hull.VisualHullReconstructor`; this backend is the pure array API for V2 portfolios. |

### 3. `depth_fusion` — TSDF volumetric fusion

| | |
|--|--|
| **What it does** | Truncated signed-distance accumulation of posed depth maps with weights, then marching cubes. Reports truncation distance and voxel size. |
| **Authority** | `SENSOR_DERIVED`. |
| **Measured accuracy** | Analytic plane depth from a single OpenCV camera reconstructs a surface near the plane (mean \|z\| ≲ 0.08 m at 2 cm voxels). |
| **Limitations** | No `spatial` package on this branch — local depth frames in `ReconstructionInputs.depth_frames`. Missing views leave free-space holes. Not a replacement for dense MVS. |

### 4. `parametric` — RANSAC primitives

| | |
|--|--|
| **What it does** | RANSAC fit of plane / box / cylinder / sphere to a point cloud; returns parameters, inlier ratio, residual RMSE, and a mesh export. |
| **Authority** | `MODEL_DERIVED`. |
| **Measured accuracy** | Synthetic sphere (r=0.35, σ=0.002 noise) recovers centre within 3 cm and radius within 2 cm at inlier ratio > 0.85. |
| **Limitations** | Single primitive. Multi-body scenes need pre-segmentation. Scalar measurement-to-component fitting remains in `parametric.fitting.ComponentFitter` (project store); geometric RANSAC lives here without duplicating that path. |

### 5. `retrieval` — local archetype library

| | |
|--|--|
| **What it does** | Loads owned/procedural meshes from a local library manifest. Adaptation is limited to anisotropic scale + landmark-aligned affine. |
| **Authority** | Capped by `VisibilityState.RETRIEVED_MODEL` → `MODEL_DERIVED`. Surfaces tagged `RETRIEVED_MODEL` (never OBSERVED). |
| **Measured accuracy** | Exact under the declared scale/affine; not a measurement of the physical object. |
| **Limitations** | Prior, not evidence. **Unreviewed licensing is refused.** Non-rigid free-form warp is forbidden. |

### 6. `browser_runtime` — WebGL/runtime scene extraction

| | |
|--|--|
| **What it does** | Thin adapter over `perception.graphics.RuntimeGltfCompiler`. Materializes an owned browser scene hook into glTF + mesh candidate. |
| **Authority** | `RUNTIME_OBSERVED`. |
| **Measured accuracy** | Structural fidelity to the runtime hook only; no pixel-to-Blender residual is claimed. |
| **Limitations** | Requires explicit scene hook data (`objects` with positions/indices or bounds). Does not solve photogrammetric registration of browser pixels. |

### 7. `point_representation` — oriented-point archive

| | |
|--|--|
| **What it does** | Back-projects fused depth (or accepts points) into an oriented-point archive: position, normal, colour, radius, confidence. Binary `.osplat` + PLY view. |
| **Authority** | `SENSOR_DERIVED`. |
| **Measured accuracy** | Point set matches input depth samples; not a continuous surface metric. |
| **Limitations** | **Not a trained radiance field. Not a trained Gaussian splat.** Claiming NeRF/3DGS capability is a contract violation. Format magic `BVMCPSPLT` v1. |

## Running the ensemble

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-reconstruction-ensemble.py \
  --output artifacts/v2/reconstruction
```

Targets:

1. **Calibration object** — sphere with ground truth; per-backend chamfer.
2. **Consumer multiview** — remote-like body, ≥24 views; COLMAP sparse when images exist.
3. **Data-centre rack module** — parametric + retrieval candidates; fusion between them accepted; fusion with calibration **refused**.

### Rendering note

The preferred image source is Blender 4.2 headless
(`/Applications/Blender.app/Contents/MacOS/Blender`). In environments where
Blender crashes during Metal GPU backend detection (observed on restricted
macOS sandboxes), the ensemble falls back to a software multiview rasterizer
that still produces ≥24 geometrically consistent textured views of each target
so COLMAP sparse SfM can execute. The receipt records which renderer was used;
Blender failure is never silently ignored.

## Doctrine checklist

- [x] Authority + evidence + uncertainty on every claim
- [x] `derive()` for derived authority
- [x] No OBSERVED label on retrieved/hidden structure
- [x] Portfolio of candidates, not one guess
- [x] Truthful blocked states (especially COLMAP dense without CUDA)
- [x] Adapters that import also execute or report exact blockers
