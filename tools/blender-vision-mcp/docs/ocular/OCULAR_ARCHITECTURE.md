# Ocular OS architecture (V2.1)

VisionMCP V2.1 Ocular OS is the continuous perception loop, not a one-shot
reconstruction job. Identity is derived from image evidence alone.

## Loop

```
recorded stream → calibrated OcularFrames → segmentation from pixels
→ dense appearance features → perception-derived identities → temporal
association → occlusion → object permanence → prediction → surprise
→ re-identification → world update → next-best-view → MCP query
```

## MCP surface (19 tools)

Registered on the real FastMCP server in `src/blender_vision/mcp/server.py`:

| Tool | Role |
| --- | --- |
| `vision.open_stream` | Open video / sequence / screen / render / opt-in webcam |
| `vision.close_stream` | Release capture and ring buffer |
| `vision.get_stream_state` | Buffer, timestamps, drops, execution class |
| `vision.calibrate_sensor` | Lens / principal-point / scale calibration receipt |
| `vision.fixate` | Gaze fixation with inhibition-of-return |
| `vision.saccade` | Planned gaze jump |
| `vision.track` | Multi-object association (IoU + appearance + Kalman) |
| `vision.reidentify` | Appearance re-id for lost/occluded tracks |
| `vision.observe_change` | Session-to-session change classes |
| `vision.build_world_model` | Ordered observations → sealed world |
| `vision.update_world_model` | Append-only one-frame update |
| `vision.query_world` | Entity / class / relations / summary |
| `vision.explain_belief` | Belief history for one slot |
| `vision.list_uncertainties` | Ranked uncertainty list |
| `vision.predict_next` | Pose / visibility / existence predictions |
| `vision.list_surprises` | Recorded surprise events |
| `vision.plan_capture` | Coverage-maximising view plan (shared V2 tool) |
| `vision.ask_next_view` | Information-gain ranked next views (shared V2 tool) |
| `vision.measure_information_gain` | Expected reduction for a proposed view |

`vision.plan_capture` and `vision.ask_next_view` pre-existed as V2 tools and
are part of the ocular nineteen by name. The other seventeen are additive.

## Modules

| Module | Responsibility |
| --- | --- |
| `ocular/stream.py` | Stream bus, ring buffer, drop accounting, webcam BLOCKED |
| `ocular/calibration.py` | Sensor calibration receipts |
| `ocular/gaze.py` | Fixation, saccade, IOR, attention budget |
| `ocular/retina.py` | Pyramid, flow, foveation |
| `ocular/segment.py` | Classical segmentation (no learned weights) |
| `ocular/track.py` | Association, occlusion, re-id |
| `ocular/world.py` | Persistent world + beliefs |
| `ocular/predict.py` | Prediction and surprise |
| `ocular/attestation.py` | Physical authority law |

## Laws (non-negotiable)

1. **No ground truth in the builder path.** GT exists only in the sealed evaluator.
2. **No fallback emits a physical PASS.** Substitute → `DIAGNOSTIC_ONLY` /
   `CANDIDATE_ONLY`; unavailable → `BLOCKED`.
3. **Never blame hardware you did not observe.** Use `classify_failure()`.
4. **Webcam frames are never fabricated.** Missing camera → `BLOCKED`.
5. **Streams are consumed incrementally.** Timestamps are monotonic; drops are
   counted in stream stats and per-frame `dropped_before`.

## Proof scripts

```bash
.venv/bin/python scripts/verify-ocular-mcp.py
.venv/bin/python scripts/run-ocular-stream.py --receipt artifacts/ocular/stream-receipt.json
```

`verify-ocular-mcp.py` starts the real server object, lists tools, and calls
all nineteen through server dispatch. Listing alone is not enough.

## Webcam resumption

See [WEBCAM_RESUMPTION.md](./WEBCAM_RESUMPTION.md). This development host has
no camera; live capture is attested BLOCKED until a host with hardware runs the
resumption protocol.
