# Material and light sensitivity receipts

Phases L and O of the Ocular Operating System: every comparison metric must
prove it can discriminate, and every perceptual critic must be calibrated
against near-threshold cases and confounders.

## Law

A comparison metric is **DIAGNOSTIC** until a sealed `ProbeSensitivityReceipt`
(`ocular.probe-sensitivity-receipt`, Bible 25.14) proves both halves:

1. **Discrimination** — the metric separates a declared meaningful parameter
   delta with a stated margin.
2. **Confounder rejection** — the metric does **not** move beyond allowance
   under resolution change (within a stated range), PNG re-encode, a one-pixel
   crop, or sample count above a stated floor.

A metric that responds to everything is as useless as one that responds to
nothing. Claiming `AUTHORITATIVE` without both halves is a seal-time error.

## Probe fix (roughness observability)

The previous parity gate passed every roughness step: sweeping browser
roughness 0.1→0.9 moved whole-image dE2000 only 2.82→3.07. The residual was
cross-renderer noise, not roughness signal. Causes:

- Whole-image means dilute the specular lobe into diffuse body + background.
- Matte dielectric base material has little specular structure.
- Default area light is large; low-roughness lobes were under-resolved.

**Fix (extend, do not replace `ProbeRig`):**

| Piece | Change |
| --- | --- |
| `SENSITIVITY_PROBE_RIG` | Higher resolution (256), tighter area light (0.35), slightly closer FOV |
| `measure_highlight` | Specular peak energy + lobe FWHM on the sphere |
| `highlight_delta_e2000` | CIEDE2000 restricted to the specular lobe union |
| Sweep base | Polished metal for roughness / anisotropy / light sweeps |

Published parity gate thresholds are **unchanged**. Sensitivity metrics are
additional evidence channels; they do not widen dE≤8 / structural≤0.15.

### Before / after (offline self-sweep)

| Config | Metric | Expected behaviour |
| --- | --- | --- |
| BEFORE: `DEFAULT_PROBE_RIG`, matte dielectric | whole-image dE2000 | Weak / non-monotonic span |
| AFTER: `SENSITIVITY_PROBE_RIG`, metal | `specular_lobe_width`, peak energy | Monotonic, human-scale span |

If roughness still cannot be discriminated after an honest attempt, the metric
stays `DIAGNOSTIC` — sensitivity is never manufactured.

## Parameters swept (Bible 15.3)

`roughness`, `metalness`, `ior_specular`, `normal_strength`,
`displacement_scale`, `anisotropy`, `light_size`, `light_direction`,
`exposure`.

## Critic calibration (Phase O)

Roles: product, material, light, environment, composition, typography, motion,
organic, groom, performance.

Each role has a five-case (+ repair) fixture set:

| Case | Must |
| --- | --- |
| positive | fire |
| near-threshold | fire |
| negative | stay silent |
| confounder | stay silent |
| false-positive check | stay silent |
| repair verification | stay silent after bounded repair |

A prior “13/13 caught, 0 false positives” without near-threshold cases is a
contract failure under this regime.

## Running

```bash
cd tools/blender-vision-mcp

# Unit / offline suite
.venv/bin/python -m pytest -q tests/test_ocular_sensitivity.py

# Physical: real Cycles + one browser (serialized)
scripts/with-one-browser.sh \
  .venv/bin/python scripts/run-ocular-sensitivity.py \
  --output artifacts/ocular/sensitivity
```

The physical runner prints the full response curve per metric, the
AUTHORITATIVE/DIAGNOSTIC verdict and measured threshold per parameter, the
roughness before/after table, confounder results, and the critic matrix. Exit
non-zero if any receipt claims AUTHORITATIVE without both halves or if the
critic matrix fails.

## Modules

| Path | Role |
| --- | --- |
| `src/blender_vision/ocular/sensitivity.py` | Receipt record + classification machinery |
| `src/blender_vision/ocular/critics_calibration.py` | Critic fixtures, matrix, per-role receipts |
| `src/blender_vision/materials/parity.py` | `ProbeRig` / highlight metrics (extended) |
| `scripts/run-ocular-sensitivity.py` | Physical + offline driver |
| `tests/test_ocular_sensitivity.py` | Synthetic AUTHORITATIVE/DIAGNOSTIC + critic matrix |
