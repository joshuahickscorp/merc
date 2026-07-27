# VisionMCP V2 — MCP surface architecture

This document describes how the V2 subsystems are exposed through the FastMCP
server in `src/blender_vision/mcp/server.py`. It does not redefine authority
doctrine; see `v2/authority.py`, `v2/records.py`, and the per-subsystem docs
under `docs/v2/`.

## Goal

Register the Bible §18.2 tools so an LLM can reach V2 capability without
reimplementing spatial, reconstruction, procedural, material, lighting, organic,
grooming, cinematic, delivery, critic, or active-perception logic.

## Placement

All tools live inside the existing `create_server()` builder. They use the same
idioms as V1:

| Concern | Pattern |
| --- | --- |
| Project resolution | `open_project(project_path)` / `by_id(project_id)` |
| Background jobs | `enqueue_background` (V1 Blender ops only; V2 asset scripts call `V2BlenderExecutor` and report `blocked` on failure) |
| Return type | JSON-serialisable `dict` |
| V2 records | sealed `to_dict()` so digest + authority travel with the payload |
| Docstrings | One imperative line stating the authority the result carries |

V1's thirty-four `vision.*` tools are untouched.

## Tool map

| Tool | Subsystem | Primary API | Record / payload |
| --- | --- | --- | --- |
| `vision.capture_world` | `spatial` | `CoverageAtlas` + `CoverageReport.seal_scene_evidence` | `v2.scene-evidence-graph` |
| `vision.import_depth` | `spatial.depth` | `DepthMap.from_*` + `seal_observation_bundle` | `v2.observation-bundle` |
| `vision.import_point_cloud` | `spatial.pointcloud` | `PointCloud.read_ply` + seal | `v2.observation-bundle` |
| `vision.plan_capture` | `spatial.capture_plan` | `plan_capture` | plan dict + HYPOTHETICAL bundle |
| `vision.ask_next_view` | `active_perception` | `NextBestViewPlanner.plan` | list of `v2.next-view-request` |
| `vision.build_reconstruction_portfolio` | `reconstruction` | `build_portfolio` | `v2.reconstruction-portfolio` |
| `vision.compare_reconstruction_backends` | `reconstruction.compare` | `compare_candidates` / `compare_all` | metric table (no fused score) |
| `vision.fit_parametric_model` | `reconstruction.parametric` | `fit_primitive` | RANSAC fit, MODEL_DERIVED |
| `vision.generate_procedural_scene` | `procedural.scene` | `build_flagship_scene` / `compile_scene` | `v2.procedural-scene-graph` |
| `vision.generate_archetype` | `procedural.library` | `default_library().create` | manifest + fingerprint |
| `vision.infer_materials` | `materials.inverse` | `infer_materials` | `v2.material-hypothesis-set` |
| `vision.solve_lighting` | `lighting.solve` | `solve_lighting` | `v2.lighting-hypothesis-set` |
| `vision.optimize_inverse_render` | `lighting.joint` | `joint_solve` | linked material + lighting records |
| `vision.generate_texture_set` | `materials.textures` | `generate_texture_set` | texture paths + metadata |
| `vision.retopologize` | `organic.topology` | `TopologyService.process` | measured topology or `blocked` |
| `vision.generate_uvs` | `organic.topology` | `TopologyService.process` (unwrap) | UV report or `blocked` |
| `vision.generate_lods` | `delivery.lods` | `generate_lods` | `LodReport` (`blender_used` honest) |
| `vision.generate_fur` | `grooming.fur` | `FurGroomer.groom` | groom report or `blocked` |
| `vision.compose_camera_path` | `cinematic.path` | `compose_camera_path` / flagship | `v2.camera-path-graph` |
| `vision.compile_cinematic_scene` | `cinematic.emit` | `export_motion_table` (+ optional bake) | path + motion table |
| `vision.compile_web_scene` | `delivery.manifest` | `build_delivery_manifest` | `v2.delivery-manifest` |
| `vision.stream_scene_assets` | `delivery.stream` | `build_streaming_plan` | streaming plan dict |
| `vision.run_perceptual_critics` | `critics` | `CriticWorkspace.run` | `v2.perceptual-critique` |
| `vision.benchmark_target` | `app_build.benchmark` | `ApplicationBenchmarkRunner.run` | receipt or `blocked` |
| `vision.promote_candidate` | `v2.records` | `V2Record.promote` | promoted record or `refused` |

## Authority rules enforced at the surface

1. **No silent promotion.** Derived claims go through subsystem `derive()` paths.
2. **Review-only classes.** `vision.promote_candidate` calls `V2Record.promote()`,
   which refuses empty, `system`, `auto`, `automatic`, or `visionmcp` reviewers
   when the target is review-only (OBSERVED, MEASURED, HUMAN_REVIEWED, …).
3. **Blocked, not faked.** Blender/COLMAP/codec unavailability returns
   `{"status": "blocked", "reason": "..."}`. Python exceptions are reported as
   runtime failures, never reclassified as hardware blockers.
4. **Portfolios under uncertainty.** Reconstruction and material/lighting tools
   return hypothesis sets, not single confident truths.
5. **Internal adapters stay internal.** COLMAP, Blender workers, and codec
   backends are not separate MCP tools.

## Project layout for V2 artifacts

Tools write under `<project>/v2/...` (spatial, reconstruction, procedural,
materials, lighting, organic, grooming, cinematic, delivery, critics,
promotions). Records are JSON with digests; callers should re-validate through
`blender_vision.v2.validation.verify_payload` before trusting external payloads.

## Verification

```bash
cd tools/blender-vision-mcp
.venv/bin/ruff check src/blender_vision/mcp/server.py tests/test_v2_mcp_surface.py
.venv/bin/python -m pytest -q tests/test_v2_mcp_surface.py
.venv/bin/python -m pytest -q tests/test_jobs_and_mcp.py
.venv/bin/python -m pytest -q
```

## Non-goals

- Changing subsystem behaviour or signatures under `v2/`, `schemas/v2/`, or the
  domain packages.
- Exposing low-level backends as first-class MCP tools.
- CLI wiring (`cli/main.py`).
- Weakening V1 tools or tests.
