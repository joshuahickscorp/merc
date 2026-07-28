# Beat render diagnosis — physical

The beat-coverage gate failed nine of nine beats with
`non_background_fraction 0.0000` and `frustum_render_mismatch: geometry in
frustum but render is empty/black`.

Diagnosed physically against the rendered PNGs. **The renders are not black.**
There are two separate defects and the gate's verdict conflated them.

## 1. The gate's background threshold is wrong for the content

`beat_coverage.py:387` sets `background_luminance_ceiling = 0.08` in **linear**
Rec. 709 luminance, and `:403` classifies a pixel as non-background when
`lum > 0.08`.

Measured on `renders/beat_04.png`:

```
luminance  min=0.00063  p50=0.01359  p90=0.01812  p99=0.02833  max=0.04634

ceiling=0.020 -> non_background_fraction = 0.0419
ceiling=0.010 -> non_background_fraction = 0.7635
ceiling=0.005 -> non_background_fraction = 0.7958
```

The frame's **maximum** linear luminance is 0.046, well under the 0.08 ceiling,
so every pixel in every beat classifies as background and the fraction is
identically zero no matter what is on screen. Linear 0.08 is roughly sRGB 0.31 —
a mid-grey threshold applied to a deliberately dark cinematic corridor.

At a defensible ceiling, roughly three quarters of the frame is real content.
The `frustum_render_mismatch` finding follows from the same zero and is
therefore also spurious: geometry is in frustum *and* in the render.

This is a metric defect. It must be fixed by choosing a background rule
appropriate to the content — a percentile-relative or scene-referred rule rather
than a fixed absolute in linear space — and **not** by lowering the number until
the beats pass. The rule must still fail a genuinely empty beat, so the fix has
to be validated against a deliberately emptied control.

## 2. The scene is genuinely underlit and badly framed

Separately, and truthfully: max linear luminance 0.046 (~sRGB 0.24) means the
corridor is very dark. Inspecting `beat_04.png` directly, the camera sits hard
against a wall and the corridor reduces to a narrow vertical slit; the visible
rack geometry — perforated faces and the overhead cable tray — occupies a small
bottom-centre sliver while two large flat olive walls dominate the frame.

So the art-direction failure the Ocular goal describes is real, and the critics
were right to flag repeated sameness and generic dead-centre framing on beats 01
through 05. Fixing the metric will not fix the composition; both are needed.

## Sequencing

Per the resumption instruction, the beat pipeline is fixed **after** the
perception-driven world loop passes. This diagnosis is recorded now so that work
starts from measurements rather than from the misleading "black render" symptom.

When it is taken up, the required per-beat evidence is unchanged: non-black
rendered pixels, visible semantic geometry, foreground/midground/background,
text-safe region, focal subject, lighting hierarchy, camera/world coordinate
agreement, and an exact artifact digest.
