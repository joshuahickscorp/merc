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

The same material is rendered under the same probe geometry and lighting in:

- Blender Cycles (headless)
- Browser WebGL (Playwright Chromium; **one browser at a time**)
- Mobile LOD (roughness bias)
- Fixed poster path (analytic GGX)

Metrics: mean CIEDE2000 and `1 - SSIM` structural term. A material that passes
offline but fails the browser gate is rejected.

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
