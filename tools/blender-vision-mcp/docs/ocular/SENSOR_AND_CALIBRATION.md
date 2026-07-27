# Sensor registry and calibration

Part of VisionMCP V2.1 — the Ocular Operating System. This document describes
the stream-facing sensor book (Bible 5.1) and the calibration path that produces
a `SensorCalibration` receipt.

## Sensor descriptor (Bible 5.1)

Every stream binds a `SensorDescriptor` before frames are emitted:

| Field | Role |
| --- | --- |
| `sensor_id` | Stable id within the process registry |
| `source_type` | `video_file`, `image_sequence`, `screen_capture`, `blender_render`, `webcam` |
| `hardware` | Path, device index, or render backend label |
| `colour_profile` | Default `srgb` |
| `lens` | Free-text lens identity when known |
| `resolution` | `[width, height]` |
| `frame_rate` | Nominal Hz |
| `timestamp_domain` | `monotonic`, `wall_utc`, `media_pts`, or `frame_index` |
| `rights_state` | `owned` / `licensed` / `synthetic` / … |
| `privacy_state` | `cleared` / `contains_pii` / `masked` / `synthetic` |
| `last_calibration` | Id of the most recent `SensorCalibration` |
| `known_limitations` | Free-text residual risks |

The registry is process-local (`SensorRegistry` / `DEFAULT_REGISTRY`). Lookups
fail closed: an unknown `sensor_id` raises `ValidationError` rather than
inventing hardware.

## Stream sources

`open_stream` / `close_stream` / `get_stream_state` in
`blender_vision.ocular.stream`:

| Source | Notes |
| --- | --- |
| Video file | OpenCV `VideoCapture`; PHYSICAL when the file opens |
| Image sequence | Sorted directory of PNG/JPEG/…; deterministic frame-index timebase |
| Screen capture | Pillow `ImageGrab`; BLOCKED if grab fails |
| Blender render | Image sequence produced by an attested Blender run |
| Webcam | **Opt-in only** (`allow_webcam=True`). Missing camera → `ExecutionClass.BLOCKED`, never a fabricated frame |

Frames live in a bounded ring buffer. Overflow increments `frames_dropped` and
discards the oldest sample. Timestamps are forced strictly monotonic even when
the source stalls.

## `OcularFrame`

An immutable sealed `V2Record` binding:

`frame_id`, `stream_id`, `timestamp`, `sensor_state`, `image_digest`,
`resolution`, `colour_space`, `exposure`, `camera_intrinsics`,
`camera_pose_if_known`, `depth_digest`, `motion_digest`,
`privacy_mask_digest`, `calibration_receipt`, and an explicit
`CoordinateFrame`.

After `seal()`, field assignment raises `AttributeError`. Downstream consumers
treat a sealed frame as a fixed sample.

## Calibration

`calibrate_sensor(image_paths, sensor_id=…, board_size=…, square_m=…)`:

1. Detect checkerboard corners (OpenCV `findChessboardCorners` + `cornerSubPix`).
2. Fit `calibrateCamera` when ≥ 3 views resolve; otherwise fall back to a
   principal-point estimate from the board centre of mass, or the image centre.
3. Measure timestamp-skew proxy, exposure-pumping (alternating mean luminance),
   and colour-temperature drift across the set.
4. Emit `SensorCalibration` with an explicit `CoordinateFrame`.

### Authority law

- **MEASURED** only when a physical scale is supplied (`square_m` or
  `physical_scale_m`) **and** at least one board detection succeeded.
- Without a physical scale, authority is capped at **SENSOR_DERIVED** even if
  the board is perfect. Pixels without metres are not a measurement.
- The default coordinate frame is OpenCV camera (`up=-Y`, `forward=+Z`). Blender
  world is `+Z` up / `-Y` forward — never mix them without an explicit transform.

## Webcam honesty

```python
open_stream(0, source_type=SourceType.WEBCAM)  # BLOCKED: opt-in required
open_stream(0, source_type=SourceType.WEBCAM, allow_webcam=True, webcam_index=99999)
# → RuntimeAttestation(execution_class=BLOCKED, blocked_reason=…)
```

No path fabricates frames when the device is absent.
