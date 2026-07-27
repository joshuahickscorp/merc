# V2 Spatial Evidence Lane

Governed ingestion and normalisation of depth maps, normal maps, point clouds,
camera trajectories, calibration targets, measured anchors, and capture coverage.

Package: `blender_vision.spatial`

## Doctrine

1. Every claim carries an `AuthorityClass`, evidence references, and `Uncertainty`.
2. Derived claims use `v2.authority.derive()` — never stronger than the weakest input.
3. Unobserved structure is `NEVER_OBSERVED` / `INFERRED`, never `OBSERVED`.
4. Truthful uncertainty: blocked operations name the blocker; nothing is silently stubbed.
5. Frame conversion lives only in `spatial.frames`.

## Modules

### `frames.py`

| Operation | Authority | Notes |
| --- | --- | --- |
| `transform_matrix(src, dst)` | n/a (pure geometry) | 3×3 maps among `BLENDER_WORLD`, `GLTF_WORLD`, `OPENCV_CAMERA` |
| `convert_points` / `convert_transform` | preserves caller authority | Round-trip exact to 1e-9 |

Known maps:

- Blender `(x,y,z)` → glTF `(x, z, -y)`
- Blender `(x,y,z)` → OpenCV `(x, -z, y)`
- glTF `(x,y,z)` → OpenCV `(x, -y, -z)`

**Limitation:** Custom frames fall back to basis construction; only the three
named V2 frames are guaranteed exact.

### `depth.py` — `DepthMap`

| Operation | Input authority | Output authority | Limitation |
| --- | --- | --- | --- |
| `from_npy` / `from_16bit_png` / `from_pfm` | caller-supplied | same | EXR not loaded here (OpenEXR optional in lane script) |
| `to_metric(scale)` | `derive(depth, scale)` | capped by scale source | Refuses `UNRESOLVED` scale; never invents MEASURED |
| `back_project()` | requires metric | points inherit map authority | Refuses `RELATIVE` / raw `DISPARITY` |
| `to_normals()` | metric or z-buffer | same | Edge-aware finite differences; discontinuity guard |
| `validate_against(camera)` | n/a | issue list | Dimension + intrinsic comparison |
| `seal_observation_bundle()` | map authority | sealed `ObservationBundle` | Blocks MEASURED on unscaled maps |

**Measured accuracy (unit tests):** analytic plane Z=2 m back-projects with
sub-micrometre error at the image centre.

**Hard rule:** an unscaled relative depth map cannot be promoted to `MEASURED`.

### `pointcloud.py` — `PointCloud`

| Operation | Authority | Measured accuracy |
| --- | --- | --- |
| `write_ply` / `read_ply` (ASCII + binary LE) | preserved | Positions exact within float32; colours 8-bit quantised |
| `voxel_downsample` | preserved | Deterministic first-hit per voxel |
| `estimate_normals` (PCA + cKDTree) | `derive` → ≤ SENSOR_DERIVED | Orientation toward centroid |
| `chamfer_distance` | n/a | Symmetric mean NN distance |
| `umeyama_align` | n/a | Recovers known similarity to 1e-9 RMSE on noise-free data |
| `transform` | preserved | 3×3 / 4×4 |

**Limitation:** Umeyama requires index-wise correspondence. Unordered clouds need
a separate matcher. PLY list-properties are rejected.

### `trajectory.py` — `CameraTrajectory`

| Check / op | Behaviour |
| --- | --- |
| `validate()` | Orthonormal right-handed rotations; strict monotonic timestamps; no duplicate poses |
| `arc_length()` | Polyline of camera centres |
| `resample(n)` | Arc-length position + SVD-reorthonormalised rotation lerp |
| `relative_pose(i,j)` | `inv(W_j) @ W_i` |
| `to_blender` / `from_blender` | Via `frames.convert_transform` |

**Limitation:** Resample is not quaternion SLERP; adequate for short paths only.

### `calibration.py` — `calibrate_planar_board`

OpenCV `findChessboardCorners` / `findChessboardCornersSB` + `calibrateCamera`.

| Condition | Authority |
| --- | --- |
| `square_size_m` supplied (physical board) | **MEASURED** |
| no physical size (unit squares) | **SENSOR_DERIVED** |

Returns intrinsics, distortion, per-view reprojection error, RMS.

**Measured accuracy (procedural orbit, 640×480, 12 detected views, square-pixel
fix + zero distortion):**

| Quantity | Measured |
| --- | --- |
| Mean reprojection | ~1.53 px |
| fx / fy relative error | ~2.97% |
| cx error | ~10.3 px |
| cy error | ~3.5 px |

**Limitations:**

- Planar checkerboard only (no Charuco).
- Requires ≥ 3 detected views.
- Dense MVS / COLMAP patch-match is **out of scope** (and CUDA-blocked on this host).
- On this agent host Blender 4.2.1 SIGSEGVs in `MTLBackend::metal_is_supported`
  during `WM_init` (exit -11) before any Python runs. The lane records that as
  `BLOCKED_EXTERNAL` and falls back to a procedural fixture with authority
  `PROCEDURAL_GROUND_TRUTH`. When Blender starts, `generate_fixture.py` is used.

### `coverage.py` — `CoverageAtlas`

Ray-casts surface patches (box faces or sphere samples) against cameras.

| Visibility | Meaning | Authority ceiling |
| --- | --- | --- |
| `DIRECTLY_VISIBLE` | ≥ covered_hit_threshold front-facing hits | OBSERVED |
| `PARTIALLY_VISIBLE` | ≥ partial threshold | SENSOR_DERIVED |
| `NEVER_OBSERVED` | zero hits | INFERRED (not observed) |

**Measured behaviour:** with cameras only above a box/sphere, the underside
hemisphere / −Z face is reported `NEVER_OBSERVED` — never inferred as seen.

**Limitation:** Without a proxy mesh, self-occlusion is not modelled; only
incidence and front-face tests apply. Optional triangle mesh enables Möller–Trumbore occlusion.

### `capture_plan.py` — `plan_capture`

Greedy set cover over the coverage atlas. Pure geometry.

| Output | Role |
| --- | --- |
| ordered `ProposedView` list | maximise marginal coverage under budget |
| `coverage_deltas` | for a separate NextViewRequest / info-gain subsystem |

Authority of the sealed plan: **HYPOTHETICAL** (proposed, not yet executed).

**Limitation:** Does not emit `NextViewRequest` records; does not score
information gain beyond discrete patch coverage.

## End-to-end lane

```
.venv/bin/python scripts/run-spatial-lane.py --output artifacts/v2/spatial
```

Steps: Blender fixture attempt (checkerboard + metric box, ≥12 views + Z; falls
back to procedural GT when Metal init crashes) → calibration → depth
back-project / chamfer → PLY round-trip → trajectory → coverage.

**Measured lane numbers (procedural fallback on this host):**

| Step | Result |
| --- | --- |
| Calibration | reproj 1.53 px, fx_err 2.97% |
| Depth chamfer vs box surface | 0.0113 m |
| Trajectory arc length | 4.181 m (exact match to analytic) |
| Coverage never-observed | 0.167; underside patches all `NEVER_OBSERVED` |

Receipt: `artifacts/v2/spatial/receipt.json`.

## Explicit non-goals

- Learned / monocular depth (no weights).
- Photogrammetry / SfM / COLMAP dense MVS.
- NeRF, Gaussian splatting.
- MCP tool registration (wired separately).
