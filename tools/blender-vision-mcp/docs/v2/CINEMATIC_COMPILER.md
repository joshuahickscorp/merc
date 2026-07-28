# Cinematic Compiler

Scroll-bound camera paths for VisionMCP V2. The compiler produces a sealed
`CameraPathGraph` (`v2.camera-path-graph`) and supports deterministic replay.

## Modules

| Module | Role |
| --- | --- |
| `blender_vision.cinematic.spline` | Centripetal Catmull-Rom position spline; squad/slerp orientation; arc-length LUT |
| `blender_vision.cinematic.path` | `compose_camera_path`, flagship datacentre path, geometry intersection checks |
| `blender_vision.cinematic.replay` | Pure, cross-process deterministic camera state at a scroll value |
| `blender_vision.cinematic.textsafe` | Contrast + luminance-variance text-safe zone solver |
| `blender_vision.cinematic.emit` | Browser motion table + headless Blender camera bake |

## Contracts

- Narrative beats must cover scroll `[0, 1]` with no gaps and no overlaps.
- Arc-length parameterisation: equal scroll delta ≈ equal world distance.
  Resampling at 1000 points recovers table arc length to relative error `< 1e-6`.
- Orientation curve forbids quaternion flips (consecutive sample dots stay positive).
- Camera samples must not enter declared solid geometry.
- Authority is derived with `derive()`; look-ahead orientation is not OBSERVED.
- Replay uses only sealed graph fields and fixed rounding so digests match across processes.

## Flagship path

Nine beats:

`00 THRESHOLD → 01 CAPACITY → 02 INFERENCE → 03 DISPATCH → 04 EXECUTION → 05 TURN → 06 VERIFY → 07 RECEIPT → 08 ACCESS`

The turn is a single continuous spline event (not a hard cut). Prefetch of the
second corridor is planned just before TURN scroll.

## Evidence

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic
BVMCP_RUN_BLENDER_TESTS=1 .venv/bin/python -m pytest -q tests/test_v2_cinematic.py
```
