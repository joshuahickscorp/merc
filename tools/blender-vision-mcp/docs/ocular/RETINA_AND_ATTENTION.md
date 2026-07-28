# Retina pipeline and gaze control

Part of VisionMCP V2.1 — the Ocular Operating System. The retina is the front
end of the eye: continuous calibrated frames in, Bible 6.5 retinal events and
foveated fixations out. Everything downstream consumes `OcularFrame`.

## Pipeline

```
OcularFrame + image
    → reflex lane (reduced resolution, latency only)
    → attentive lane:
         Gaussian / Laplacian pyramid
         frame differencing
         dense optical flow (Farneback)
         camera / object motion separation
         coarse saliency + local contrast
         exposure-change detection
         track birth / death / occlusion
    → RetinalEvent[] + latencies
```

### Reflex vs attentive

| Lane | May | Must not |
| --- | --- | --- |
| Reflex | Reduce resolution; report latency | Rewrite a correctness label |
| Attentive | Write `correctness_label`; full-res analysis | Claim PHYSICAL without attestation |

`assert_reflex_cannot_relabel` enforces the law in code and in tests.

### Camera vs object motion

Dense flow is fit with a partial affine (`estimateAffinePartial2D` / RANSAC).
The affine residual is object motion; the affine translation magnitude is the
camera-motion score. A pure global pan must emit `CAMERA_MOVED` and must not
emit `OBJECT_MOVED`.

## Bible 6.5 event vocabulary

| Event | Detector sketch |
| --- | --- |
| `OBJECT_ENTERED` | New motion blob unmatched to prior tracks |
| `OBJECT_LEFT` | Track missing for two frames |
| `OBJECT_MOVED` | Residual flow after global fit |
| `OBJECT_OCCLUDED` | Area collapse or single-frame disappearance |
| `OBJECT_REAPPEARED` | Blob near a recently lost track |
| `SURFACE_CHANGED` | Mean residual without bulk motion or light shift |
| `TEXT_CHANGED` | High Canny-edge delta, low bulk motion |
| `LIGHT_CHANGED` | Global mean luminance jump |
| `CAMERA_MOVED` | Global affine dominates residual |
| `NEW_UNKNOWN_REGION` | Companion to first enter of a track |
| `EXPECTED_EVENT_MISSING` | Declared expectation absent at its frame |

## Gaze

`GazeController` implements `fixate`, `saccade`, and `zoom`:

- **Fixation** binds target, region, reason, expected information, duration,
  resolution, models requested, and outcome.
- **Inhibition of return**: a region is not re-fixated unless uncertainty stayed
  high, evidence changed, or a critic asked.
- **Attention budget** records compute cost, latency, expected gain, actual
  gain, and redundant observations.
- **Zoom** is a foveal crop resampled to the requested resolution.

## Physical execution

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-retina.py --output artifacts/ocular/retina
```

The script:

1. Renders a Blender object-motion sequence (attested) or a procedural fallback
   labelled `PROCEDURAL_GROUND_TRUTH`.
2. Opens it as a stream and runs every frame through the retina.
3. Reports reflex / attentive latency p50/p95, peak RSS, dropped frames.
4. Prints per-event precision/recall and confusion counts.
5. Renders a camera-pan sequence and proves `CAMERA_MOVED` without `OBJECT_MOVED`.
6. Attempts a real webcam open (≥ 30 frames → PHYSICAL; else BLOCKED with reason).

Declared recall floor for `OBJECT_ENTERED` / `OBJECT_LEFT` / `OBJECT_OCCLUDED`
is **0.50**. Exit is non-zero when that floor is missed or camera motion is
misreported as object motion.

## Coordinate frames

Blender fixtures declare `blender-world` (`+Z` up, `-Y` forward). Stream frames
default to OpenCV camera (`-Y` up, `+Z` forward). Pose fields always carry their
frame; silent mixing is a contract violation (see the wall-as-floor incident).
