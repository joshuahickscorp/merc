# Governed multimodal perception

VisionMCP's capture bus accepts static images, bounded video files, explicitly
authorized camera frames, and explicitly authorized desktop snapshots. Every
capture is content-addressed, carries the caller's rights decision, records the
tool environment, and stores immutable source and derived artifacts.

## Adapters

- `image.file` emits an `ImageGraph` with observed pixels and derived contour/OCR
  regions.
- `camera.frame` uses the image contract but requires
  `configuration.user_authorized=true` and records the device label and optional
  calibration metadata.
- `camera.live` opens an explicitly selected OpenCV device only after
  `configuration.user_authorized=true`, requires a caller-supplied session ID,
  and captures a bounded frame count into a replayable `CameraSequenceGraph`.
- `video.file` uses `ffprobe` and `ffmpeg` to extract a deterministic, bounded
  timestamp set. It emits a `VideoNarrativeGraph` containing frame evidence,
  derived region tracks and scene cuts, and explicitly uncalibrated 2D global
  motion. It never presents that motion as a 3D camera trajectory.
- `desktop.authorized_snapshot` requires
  `configuration.user_authorized=true`. It synchronizes screenshot regions with
  an optional caller-provided accessibility JSON snapshot and marks spatial
  correspondences as derived.

Live ambient surveillance is deliberately absent. Device acquisition must happen
through an explicit authorized request. Monocular depth is reported as
unavailable unless a governed depth artifact is supplied. Sensor depth requires
calibration metadata and remains limited to the supplied record; model depth
requires model identity and license metadata and is labeled `DERIVED`, never
direct metric observation.

`vision.query` selects the capture's sole graph automatically and
`vision.explain_region` returns all nodes at a pixel with evidence references,
authority, confidence, uncertainty, and source restrictions.

`vision.reconstruct` with `mode=media_to_interface` compiles observed regions,
OCR symbols, and temporal tracks into an editable clean-room
`MediaInterfaceIR`. Its layout constraints remain hypotheses and require rendered
pixel comparison, accessibility review, and the global regression gate.
