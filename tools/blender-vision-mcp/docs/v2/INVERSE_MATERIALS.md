# Inverse materials (VisionMCP V2)

## Doctrine

Material claims are sealed `v2.material-hypothesis-set` records. Every estimate
carries an authority class derived with `derive()` from observation authorities,
bound evidence view ids, and per-parameter confidence. Underdetermined evidence
emits a portfolio of competing hypotheses (metal vs dielectric), never a single
averaged guess.

## Package surface

| Module | Role |
| --- | --- |
| `materials/inverse.py` | `infer_materials(observations, surfaces)` — base colour, roughness, metalness, IOR, anisotropy, transmission, subsurface |
| `materials/frequency.py` | Six-band Laplacian decomposition with per-band energy |
| `materials/textures.py` | Tileable PBR maps with `world_scale_m` |
| `materials/parity.py` | Cycles / browser WebGL / mobile LOD / poster parity with browser gate |
| `materials/critic.py` | Nine measured material failure detectors |

## Frequency bands

1. `macro_geometry`
2. `medium_displacement`
3. `fine_normal`
4. `micro_roughness`
5. `colour_variation`
6. `backing_occlusion`

Critics use relative band energy to reject flat colour textures that pretend to
be depth (colour energy without medium displacement energy).

## Parity gate

The same material is rendered under the **same** probe geometry and lighting via
a single shared `ProbeRig` consumed by every target:

- Blender Cycles (headless) — `view_transform=Standard` (plain linear→sRGB, not Filmic/AgX)
- Browser raw WebGL (Playwright Chromium; **one browser at a time**) — perspective
  ray–sphere, same camera pose/FOV, same AREA-light conversion, same sRGB encode
- Mobile LOD (roughness bias on the same rig)
- Fixed poster path (offline analytic GGX on the same rig)

`ProbeRig` freezes camera position/target/vertical FOV, AREA light
position/energy/size, world background colour/strength, exposure (EV), sphere
radius/tessellation, and resolution. No target may hardcode its own value for
anything the rig declares.

AREA light → analytic irradiance (documented in `ProbeRig.direct_irradiance_scale`):

```
d  = |light_position|
E0 = energy / (π · d²)     # facing scale at the sphere origin
```

Default arithmetic: `energy=250`, `pos=(2.5,1.8,3.2)` → `d≈4.44185`, `E0≈4.0333`.
Exposure relationship: both targets multiply linear radiance by `2^exposure`
before the Standard IEC 61966-2-1 transfer.

Published browser-gate limits (do not widen to make comparisons pass):

- mean CIEDE2000 ≤ 8.0
- structural `1 - SSIM` ≤ 0.15

A material that passes offline but fails the browser gate is rejected. Prove
discrimination with:

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/verify-parity-discrimination.py \
  --output artifacts/v2/appearance/parity-check
```

That script requires real Blender Cycles and a real Chromium/Chrome via Playwright,
prints per-material dE/structural/MAE, a deliberately wrong browser material, and
a roughness sensitivity sweep, and exits non-zero if fewer than five of nine
benchmark materials pass or if the wrong material passes.

## Critics

| Failure | Measured quantity |
| --- | --- |
| plastic-looking metal | metalness/roughness/IOR plastic score |
| white clipping | fraction of pixels ≥ 0.99 luminance |
| environment smear | horizontal/vertical gradient anisotropy |
| wrong pore scale | pore scale / expected ratio |
| flat texture as depth | medium vs colour relative energy |
| sparkling noise | isolated highlight sparkle fraction |
| mirror-like bead blast | roughness too low for bead-blast label |
| incorrect subsurface | class-inconsistent subsurface weight |
| incorrect fur clump scale | clump scale / expected ratio |

## Benchmark

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-inverse-appearance.py --output artifacts/v2/appearance
```

Nine ground-truth materials live in `benchmarks/appearance_v2/materials.json`.
