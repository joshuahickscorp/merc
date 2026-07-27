# Capture protocol — user's physical remote

This protocol is for a **rights-cleared capture of a remote you own**.
It is separate from the governed self-captured procedural fixture used in CI.

## Requirements

1. **≥ 24 orbit views**, short side ≥ 1600 px, sharp focus, even diffuse light.
2. **Underside** — tip carefully or shoot through glass; required or leave NEVER_OBSERVED.
3. **Scale reference** — metric ruler or credit card (85.60 × 53.98 mm) in ≥ 2 views.
4. **Lens metadata** — focal length (mm) and sensor width or 35 mm equivalent.
5. **Optional** ChArUco / checkerboard for intrinsics.
6. **Holdout set** — reserve ~8 views for the evaluator only; builder must not read them.

## Layout after capture

```
artifacts/v2/object-benchmarks/remote/real_capture/
  source_packet.json
  images/train/
  images/holdout/
  scale_reference.json
  camera_notes.json
```

## Command

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-remote.py \
  --train-dir artifacts/v2/object-benchmarks/remote/real_capture/images/train \
  --output artifacts/ocular/remote
```

## Claims

- Until real photographs are supplied, scored physical-remote claims stay BLOCKED.
- Dense COLMAP MVS stays BLOCKED without CUDA.
- Never seed detections from ground-truth boxes.
