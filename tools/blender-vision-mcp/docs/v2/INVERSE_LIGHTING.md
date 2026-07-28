# Inverse lighting (VisionMCP V2)

## Doctrine

Lighting claims are sealed `v2.lighting-hypothesis-set` records, separate from
material records. Joint material/light optimisation uses coordinate descent and
**must not merge authority** — the two records keep distinct ids, digests, and
authority ceilings derived only from their own inputs.

## Package surface

| Module | Role |
| --- | --- |
| `lighting/rigs.py` | Four real Blender rigs as `bpy` light/world node scripts |
| `lighting/solve.py` | `solve_lighting(observations, geometry)` |
| `lighting/joint.py` | Coordinate descent; separate sealed records |
| `lighting/critic.py` | Seven measured lighting failure detectors |

## Rigs

Each rig declares key, fill, negative fill, rim, environment, reflection cards,
shadow softness, exposure, white balance, tone map, and atmosphere. Realisation
is a generated Blender script that creates actual lights and world background
nodes (not a passive dict alone):

- `neutral_documentation`
- `black_product_studio`
- `datacenter_corridor`
- `soft_organic`

```python
from blender_vision.lighting import get_rig, apply_rig_script

rig = get_rig("black_product_studio")
script = apply_rig_script("black_product_studio")  # execute inside Blender
```

## Solve

Key direction and size come from shading gradients and specular highlight
position on known normals (analytic sphere or supplied normal maps). Environment
intensity/colour come from ambient regions; exposure and white balance from
mid-tone and R/B ratio. Multi-view disagreement emits competing hypotheses.

## Critics

| Failure | Measured quantity |
| --- | --- |
| clipped metal | hero-surface fraction ≥ 0.99 luminance |
| fake plastic metal | fill intensity × key size × softness score |
| floating objects | contact-shadow gradient magnitude |
| flat black | mean luminance and std |
| overfilled shadows | fill-to-key ratio |
| arbitrary glow | bloom / halo score |
| material error from lighting | cast × exposure corruption score |

## Joint solve

```python
from blender_vision.lighting import joint_solve

result = joint_solve(observations, surfaces, geometry)
assert result.material_record.id != result.lighting_record.id
assert result.joint_metadata["authority_merged"] is False
```

## Benchmark

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-inverse-appearance.py --output artifacts/v2/appearance
```
