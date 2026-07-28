# Tracking and memory (Phases E/F)

Identity must survive occlusion, similar objects, and temporary disappearance.
This document describes the classical segment → track loop that runs with **no
downloaded weights**, and how object permanence is recorded.

## Loop position

```
sense -> calibrate -> attend -> segment -> track -> geometry -> identity
-> world model -> predict -> surprise -> next-best-view -> act -> verify -> remember
```

Phases E/F own **segment** and **track**. Geometry and world-model work live
elsewhere.

## Segmentation (`ocular.segment`)

Classical methods only; every result is labelled exactly:

| Method | Needs | Notes |
|--------|-------|-------|
| `region_grow` | seed points | Lab colour radius flood-fill |
| `watershed` | — | Gradient surface + distance markers |
| `grabcut` | box | OpenCV GrabCut from a rectangle |
| `motion_components` | previous frame | Connected components on residual |
| `contour_parts` | — | Contour / hole decomposition |

Authority ceiling: **SENSOR_DERIVED**. Never MODEL_DERIVED without a governed
accepted checkpoint.

`segment_concept(prompt)` resolves text through a **local concept table**
(`red`, `green`, `foreground`, …). Unknown prompts return
`ConceptResolution.UNRESOLVED` with `AuthorityClass.UNRESOLVED`. No open-
vocabulary claim is made.

**Identity stability.** When a previous `SegmentationResult` is supplied,
current regions are matched by IoU + appearance so segment IDs do not renumber
on a static frame pair.

## Tracking (`ocular.track`)

`VisualTrack` is a sealed V2-style record with:

- state ∈ {`ACTIVE`, `OCCLUDED`, `LOST`, `REAPPEARED`}
- **identity_uncertainty** ∈ [0, 1] — always reported, never hidden
- appearance histogram, bbox, Kalman state, optional `occluder_track_id`
- predicted reappearance position while unseen

Association score (declared weights):

```
score = 0.45·IoU + 0.35·appearance + 0.20·position
```

Thresholds (declared; a gate that always passes is worthless):

| Name | Value | Use |
|------|------:|-----|
| `ASSOCIATION_THRESHOLD` | 0.28 | Frame-to-frame match |
| `REID_THRESHOLD_OCCLUDED` | 0.70 | Reappear after occlusion |
| `REID_THRESHOLD_LOST` | 0.92 | Reappear after departure |
| `UNCERTAINTY_GROWTH_PER_FRAME` | 0.04 | Monotonic growth while unseen |
| `MAX_OCCLUDED_FRAMES` | 45 | Then → LOST |
| `MAX_LOST_FRAMES` | 90 | Then reaped |

### Object permanence

When a track has no detection:

1. Kalman constant-velocity prediction continues.
2. State becomes `OCCLUDED` (or `LOST` after the occluded window).
3. If another active track covers the predicted centre, it is recorded as
   `occluder_track_id`.
4. `identity_uncertainty` grows by `UNCERTAINTY_GROWTH_PER_FRAME` each frame
   (monotone, capped at 1.0).
5. Re-identification requires appearance ≥ the state-appropriate threshold.

### Negative case (replacement)

An object that **leaves** (`LOST`) and is replaced by a *different* similar
object must not re-id as the original. Lost-state re-id uses
`REID_THRESHOLD_LOST = 0.92`. Mid-range histogram similarity is refused.

## Fixture

`benchmarks/ocular_tabletop/` builds three confusable spheres in Blender:

- `obj_move` — translates
- `obj_occlude` — stationary; dark slab slides over it
- `obj_leave` — exits and returns (same id)
- `obj_depart` / `obj_replace` — leave + similar replacement (must refuse)

World frame: Blender (+Z up). Image space: OpenCV (+Y down). Both declared in
the sequence manifest.

## Runner

```bash
scripts/run-ocular-tracking.py --output artifacts/ocular/tracking
```

Prints ID switches, MOTA, fragmentation, occlusion survival, re-id
precision/recall, the 3-id confusion matrix, permanence uncertainty samples,
and the negative-case decision. Exit non-zero on false re-id or ID switches
above the declared threshold.

## What this is not

- Not SAM/DINO/CoTracker execution (see `MODEL_INTAKE.md`).
- Not open-vocabulary segmentation.
- Not a claim that colour alone separates the three similar objects — position
  and motion prediction are required; that is intentional.
