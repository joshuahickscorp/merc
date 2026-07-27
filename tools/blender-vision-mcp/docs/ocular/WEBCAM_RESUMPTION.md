# Webcam resumption contract

This document is the ready-to-run protocol for a host that **has** a working
camera. It is also the honesty contract for hosts that do not.

## Authority law

- Live frames are **never fabricated**.
- A missing camera, denied permission, or busy device returns
  `ExecutionClass.BLOCKED` via `RuntimeAttestation`, not a synthetic image.
- Only `ExecutionClass.PHYSICAL` may claim a live-camera PASS, and only after
  `RuntimeAttestation.require_physical(...)`.
- Recorded video streams are real input and are the default on this development
  host (no webcam).

## This host (verified development machine)

| Check | Result |
| --- | --- |
| Webcam present | No reliable device |
| Protocol | `vision.open_stream(..., source_type="webcam", allow_webcam=True)` |
| Expected | `status=blocked`, `execution_class=BLOCKED` |
| Fabricated frame | Forbidden |

Probe used by the recorded-stream runner:

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-stream.py
# prints: webcam_probe=BLOCKED fabricated=False
```

## Ready-to-run protocol (host with a camera)

### 1. Device enumeration

```bash
# macOS: list AVFoundation devices (indices map to OpenCV indices)
system_profiler SPCameraDataType

# OpenCV index probe (0, 1, …)
.venv/bin/python - <<'PY'
import cv2
for index in range(0, 4):
    cap = cv2.VideoCapture(index)
    ok = cap.isOpened()
    w = int(cap.get(cv2.CAP_PROP_FRAME_WIDTH) or 0) if ok else 0
    h = int(cap.get(cv2.CAP_PROP_FRAME_HEIGHT) or 0) if ok else 0
    print(f"index={index} open={ok} resolution={w}x{h}")
    cap.release()
PY
```

Record the first index that opens with non-zero resolution.

### 2. Permission

- **macOS**: grant Camera access to the terminal / Python / IDE running the
  process (System Settings → Privacy & Security → Camera).
- **Linux**: ensure the user is in the `video` group and `/dev/videoN` is
  readable.
- If permission is denied, OpenCV fails to open → **BLOCKED** (correct).

### 3. Calibration target

Use a printed checkerboard (default `9×6` inner corners) with a **known square
size in metres** if MEASURED authority is required.

Without `square_m` / `physical_scale_m`, calibration stays
`SENSOR_DERIVED` / `INFERRED` even when corners resolve — pixels alone are not
a metric measurement.

Capture at least three board views at different poses (or accept
principal-point fallback for a quick smoke test).

### 4. Capture sequence

1. Open the stream with opt-in webcam:

   ```python
   from blender_vision.ocular.stream import open_stream, read_frame, close_stream
   handle = open_stream(
       "0",
       source_type="webcam",
       allow_webcam=True,
       webcam_index=0,          # from enumeration
       stream_id="live-0",
       buffer_size=16,
       frame_rate=30.0,
   )
   ```

2. If `isinstance(handle, RuntimeAttestation)` → stop. Status is BLOCKED; do
   not invent frames.
3. Otherwise read frames incrementally:

   ```python
   frame, image = read_frame(handle)
   # process; never load an unbounded backlog into RAM as a "batch live file"
   ```

4. Calibrate from saved board stills:

   ```text
   vision.calibrate_sensor(
     image_paths=[...],
     sensor_id=...,
     board_cols=9,
     board_rows=6,
     square_m=0.025,
     stream_id="live-0",
   )
   ```

5. Run the ocular loop tools: `fixate` → `track` → `build_world_model` /
   `update_world_model` → `predict_next` → `list_surprises` →
   `ask_next_view` / `measure_information_gain`.
6. Close with `vision.close_stream`.

### 5. Expected receipts

| Step | Expected fields |
| --- | --- |
| Open (success) | `status=open`, `execution_class=PHYSICAL`, non-zero resolution |
| Open (failure) | `status=blocked`, `execution_class=BLOCKED`, `blocked_reason` set, **no image** |
| Calibration | sealed `ocular.sensor-calibration`; MEASURED only with physical scale |
| Stream state | monotonic `last_timestamp`, `frames_emitted`, `frames_dropped >= 0` |
| Close | `state=closed`, `closed_at` set |

### 6. Single command to run

On a camera-equipped host, after granting permission and placing a board in
view:

```bash
cd tools/blender-vision-mcp
.venv/bin/python - <<'PY'
from blender_vision.ocular.stream import open_stream_or_attest, read_frame, close_stream
from blender_vision.ocular.attestation import ExecutionClass

handle, attestation, status = open_stream_or_attest(
    "0",
    source_type="webcam",
    allow_webcam=True,
    webcam_index=0,
    stream_id="webcam-live",
    buffer_size=8,
)
print(status)
if status["status"] != "open":
    raise SystemExit(f"BLOCKED: {status.get('blocked_reason')}")
assert handle is not None
assert handle.execution_class is ExecutionClass.PHYSICAL
item = read_frame(handle)
assert item is not None, "PHYSICAL open must yield a real frame"
frame, image = item
print({
    "frame_id": frame.frame_id,
    "timestamp": frame.timestamp,
    "resolution": frame.resolution,
    "image_digest": frame.image_digest,
    "shape": list(image.shape),
})
print(close_stream(handle))
PY
```

## MCP form

```text
vision.open_stream(
  source="0",
  source_type="webcam",
  allow_webcam=true,
  webcam_index=0,
  stream_id="webcam-live"
)
```

Same semantics: BLOCKED attestation on failure, PHYSICAL stream handle on
success, never a fake frame either way.

## Non-goals

- Do not download model weights to "improve" webcam frames.
- Do not report HARDWARE_ERROR for a path or permission bug
  (`classify_failure` order: script/path/dependency/API before hardware).
- Do not treat a recorded file as proof of live capture.
