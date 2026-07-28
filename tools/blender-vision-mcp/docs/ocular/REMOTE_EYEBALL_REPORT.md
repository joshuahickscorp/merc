# Phase K — Remote eyeball report

## Claim boundary

This report describes a **governed self-captured procedural fixture** driven
through the Ocular perception loop. It is **not** a claim about any physical
remote control, including any remote the user may own.

Real-photograph resumption lives in:

- `artifacts/ocular/remote/USER_REMOTE_CAPTURE_PROTOCOL.md` (written by the runner)
- `benchmarks/remote/capture_protocol.md` (legacy V2 protocol)
- `benchmarks/remote/resumption_contract.json`

## Pipeline (builder path — no ground truth)

```
train image sequence
  → open_stream(IMAGE_SEQUENCE)
  → classical watershed segmentation (pixels only)
  → dense appearance (colour hist + gradient energy)
  → perception-derived track ids (IoU + appearance + Kalman)
  → world model update (track_source=perception_derived)
  → per-view: observed / inferred / next-view
  → geometry portfolio (mesh, points, procedural, retrieval blocked, radiance BLOCKED)
  → hidden-surface ledger
  → material hypotheses + lighting frequency split
  → next-best-view planner
```

Ground-truth boxes never seed detections. Holdout images are not read by the
builder path.

## Per-view answers

For each train view the receipt records:

| Field | Meaning |
| --- | --- |
| `observed` | Segments with bbox, appearance, SENSOR_DERIVED authority |
| `inferred` | Occluded/lost permanence, temporal association |
| `next_view` | Capture requests (underside, closer framing, orbit) |

## Geometry portfolio

| Backend | Status |
| --- | --- |
| mesh visual-hull box | executed (relative scale) |
| point cloud | executed |
| procedural parametric | executed (INFERRED) |
| retrieval | BLOCKED unless rights-cleared license |
| Gaussian / radiance | **BLOCKED** — no weights, no network |

## Blockers (honest)

- Dense COLMAP MVS without CUDA
- Gaussian/radiance without trained weights
- User remote photographs not supplied
- Metric scale without ruler/credit-card anchor

## How to run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-remote.py \
  --output artifacts/ocular/remote
```

Receipt: `artifacts/ocular/remote/remote_loop.receipt.json`.

## User remote capture

See the protocol file written next to the receipt. Minimum: ≥24 orbit views,
underside, scale reference, lens metadata; holdout reserved for evaluator only.
