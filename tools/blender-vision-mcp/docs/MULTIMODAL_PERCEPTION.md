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
- `video.file` uses `ffprobe` and `ffmpeg` to extract a deterministic, bounded
  timestamp set. It emits a `VideoNarrativeGraph` containing frame evidence,
  derived region tracks and scene cuts, and explicitly uncalibrated 2D global
  motion. It never presents that motion as a 3D camera trajectory.
- `desktop.authorized_snapshot` requires
  `configuration.user_authorized=true`. It synchronizes screenshot regions with
  an optional caller-provided accessibility JSON snapshot and marks spatial
  correspondences as derived.

Live ambient surveillance is deliberately absent. Device acquisition must happen
through an authorized host integration that produces the governed snapshot or
frame input. Monocular depth is reported as unavailable unless a separately
governed depth backend is configured.

`vision.query` selects the capture's sole graph automatically and
`vision.explain_region` returns all nodes at a pixel with evidence references,
authority, confidence, uncertainty, and source restrictions.
