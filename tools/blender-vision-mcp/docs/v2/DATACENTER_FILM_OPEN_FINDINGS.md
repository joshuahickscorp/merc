# Data-centre film — open findings

Measured at head, after the rack-population pass. These are real defects in the
flagship, recorded rather than smoothed over, and none of them is a reason to
relax a gate.

## 1. The second aisle is empty — 8 of 464 instances

```
MAIN AISLE   (y < 11.5)   456 instances
  gpu_drawer 144, server_drawer 120, blanking_panel 72,
  cable_tray 25, rack_shell 24, switch 24, ...

SECOND AISLE (y > 11.5)     8 instances
  floor_tile 1, junction 1, aisle 1, cable_tray 1,
  containment_door 1, terminal_wall 1, cooling_face 1, column 1
```

The grammar populates the main aisle and stops at the junction. Beats
`05 TURN`, `06 VERIFY`, `07 RECEIPT` and `08 ACCESS` — the second half of the
film — therefore play against a bare corridor. A screenshot at scroll 0.80,
camera `[1.8, 13.3, 1.5]`, shows a flat wall and nothing else.

This is the Bible's "scene never reads as an empty box" failure, confined to the
post-turn half. The main aisle does not have this problem: perforated rack
faces, drawer fronts, hot/cold aisle floor colouring and status LEDs are all
visible and correct there.

**Fix:** extend the grammar's population pass across the junction into the
second aisle — a second rack row plus the verification terminal's own fixtures.
The archetypes already exist; only the placement program stops early.

## 2. Composition and lighting are not art-directed

The corridor now reads with correct scale, upright orientation and real depth,
but framing and lighting have had no art director pass. Walls blow out to olive
under the warm practical, and the shot is not composed against the text.

The perceptual-quality facet must be scored on this state, not on the intent.

## 3. Frame conversion was a runtime-only fix

The glTF Y-up to Blender Z-up conversion is applied at load in
`sandbox/datacenter-film/src/film.js`, and `camera.up` is set for the Z-up world
in the same file. `blender_vision.v2.authority` declares `BLENDER_WORLD` and
`GLTF_WORLD` and `spatial/frames.py` implements the conversion, but the web
delivery path does not yet route through them — the runtime hard-codes the
rotation instead.

Two implementations of one conversion is exactly the arrangement that produced
the train/holdout leak elsewhere in this codebase. The delivery compiler should
emit the frame declaration into the manifest and the runtime should consume it.

## 4. Prior state, for comparison

Before the population pass the racks were 144-triangle open frames with 42U of
air, and before the frame fix the camera flew through a wall it read as a floor.
Both are fixed and verified; this file records only what is still open.
