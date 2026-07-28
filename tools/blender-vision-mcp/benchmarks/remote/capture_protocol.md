# Capture protocol — consumer remote (real photographs)

This protocol lets a human supply real photographs of a physical remote control
so VisionMCP can resume Phase O against observed evidence. Nothing in the
synthetic self-capture lane is evidence about any physical remote.

## Goal

≥ 24 sharp, well-lit views of one remote, plus optional holdout views the
builder must never see, plus the metric and calibration material the solver
needs for scale authority.

## Equipment

- Camera or phone capable of ≥ 1600 px on the short side
- Soft diffuse light (overcast window or softbox); avoid hard specular spikes
- Metric scale reference: ruler or credit card (85.60 × 53.98 mm) in-frame
- Optional: Charuco / checkerboard calibration board
- Turntable or marked positions every 15°

## Setup

1. Place the remote on a non-reflective neutral surface.
2. Put the scale reference on the same plane as the remote base, fully visible
   in at least three views.
3. Light from two soft sources so buttons and seams are readable without
   blowing highlights on glossy plastic.
4. Disable beauty filters / AI scene enhance on the phone.

## View set (minimum)

| ID | Description | Notes |
|----|-------------|-------|
| orbit_00 … orbit_23 | 24 azimuths × elev ≈ 25–40° | Primary multiview |
| top | Near-nadir | Button layout |
| three_quarter_front / rear | 45° elevations | Branding, ports |
| **underside** | Object inverted or glass-table shot | **Required for hatch** |
| **battery_open** (optional) | Hatch open, compartment visible | Only if authorized |
| grazing_01 | Low raking light on top face | Microgeometry |
| diffuse_01 | Soft diffuse re-shoot of hero | Base colour |

Do **not** invent underside geometry from top views. If underside shots are
missing, the ledger must keep those regions as `NEVER_OBSERVED`.

## File naming

```
remote/<session_id>/
  images/
    orbit_00.jpg
    ...
    underside_00.jpg
  scale_reference.json      # known lengths in millimetres
  camera_notes.json         # EXIF / focal length / sensor if known
  holdout/                  # optional; builder must not read this
```

## Scale reference JSON

```json
{
  "objects": [
    {"label": "credit_card", "length_mm": 85.60, "width_mm": 53.98}
  ],
  "units": "mm"
}
```

## Quality gates before submission

- [ ] ≥ 24 distinct orbit views, object fills ≥ 40% of the frame
- [ ] Sharp focus on buttons and seams
- [ ] Scale reference readable in ≥ 3 frames
- [ ] No motion blur
- [ ] EXIF focal length present, or manual focal length + sensor width recorded
- [ ] Underside captured **or** explicitly declared absent (ledger stays NEVER_OBSERVED)
- [ ] Holdout set (if any) stored outside the builder’s readable paths

## What not to do

- Do not crop the object out of frame.
- Do not use generative fill / object erase.
- Do not claim manufacturer dimensions as MEASURED without a caliper reading.
- Do not open the battery hatch unless the capture is authorized and labelled.
