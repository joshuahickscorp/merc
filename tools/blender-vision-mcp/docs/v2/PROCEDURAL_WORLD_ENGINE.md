# Procedural World Engine (VisionMCP V2)

Semantic instructions → real, editable Blender geometry with named parts, LODs,
and browser-ready GLB exports.

## Package

`blender_vision.procedural`:

| Module | Role |
| --- | --- |
| `standards` | EIA-310 / IEC 60297 constants (1U = 44.45 mm, 19″ = 482.6 mm, …) |
| `archetype` | `Archetype`, `PartSpec`, `GeometryRecipe`, dimension checks, mesh fingerprints |
| `datacenter` | Twenty data-centre archetypes with real internal structure |
| `library` | Registry + licensing/provenance manifest |
| `grammar` | Declarative scene program (`place`, `repeat_along`, `leave_gap`, `vary_state`, …) |
| `instancing` | Mesh de-duplication → Blender collection instances |
| `lod` | near/mid/far filtering + identity check (bbox, silhouette, part set) |
| `scene` | Sealed `ProceduralSceneGraph` V2 record |
| `emit` | **Only** Blender speaker: headless script → `.blend` + GLB + EEVEE renders |

## Rack standards

| Quantity | Value |
| --- | --- |
| 1U | 44.45 mm |
| 19″ mounting width | 482.6 mm |
| Common frame widths | 600 mm, 800 mm |
| 42U height | 1.8669 m |
| Raised-floor tile | 600 mm |

## Archetypes (20)

`rack_shell`, `rack_door`, `server_drawer`, `gpu_drawer`, `switch`,
`blanking_panel`, `pdu`, `cable_tray`, `cable_bundle`, `cooling_face`,
`floor_tile`, `ceiling_panel`, `wall_rib`, `column`, `threshold`, `aisle`,
`junction`, `containment_door`, `terminal_wall`, `status_light_matrix`.

`server_drawer` / `gpu_drawer` include chassis, front bezel, vent field,
handles, drive/GPU bays, heatsinks (GPU), and rear connectors.

## Grammar target

```
threshold → main aisle → left-turn junction → second aisle → terminal wall
```

with racks both sides, overhead trays/cables, floor tile grid, containment door,
cooling face, and a status-light matrix driven by per-instance **state** (not
geometry — `vary_state` keeps mesh identity for instancing).

## Authority

Generated records use `PROCEDURAL_GROUND_TRUTH`. Geometry is synthetic and
editable; dimensional parameters follow manufacturer rack standards. Nothing is
labelled `OBSERVED`.

## Execution

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-procedural-engine.py --output artifacts/v2/procedural
```

Outputs under the given directory:

- `archetypes/<name>/{name}.glb`, LOD GLBs, `metrics.json`, `emit_blender.py`
- `flagship_scene/scene.glb`, four preview PNGs
- `procedural_scene_graph.json` (sealed V2 record)
- `archetype_manifest.json`, `engine_report.json`

### Blender vs offline emission

`emit.py` always generates a headless Blender script (`emit_blender.py`). When
Blender starts successfully, that script builds editable `.blend` files, GLB
exports, and EEVEE renders via collection instances.

If Blender headless crashes during Metal GPU init (observed on some host
sessions as SIGSEGV in `MTLBackend::metal_is_supported` before any Python
runs), emission falls back to **trimesh-offline**: real triangle meshes, real
GLB files, measured bboxes, and CPU orthographic PNG previews. That path is
labelled explicitly (`backend: trimesh-offline`, `blender_status: BLOCKED`);
it does not fake a `.blend` or EEVEE render. The engine exits non-zero while
Blender remains blocked so the failure is never silent.

## Tests

```bash
.venv/bin/python -m pytest -q tests/test_v2_procedural.py
BVMCP_RUN_BLENDER_TESTS=1 .venv/bin/python -m pytest -q tests/test_v2_procedural.py
```
