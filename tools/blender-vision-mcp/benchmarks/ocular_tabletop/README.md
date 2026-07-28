# Ocular tabletop tracking fixture

Procedural Blender scene for Phases E/F: segmentation, multi-object tracking,
and object permanence.

## Scenario

Three **genuinely confusable** objects (same mesh, near-identical base colour,
subtle seed-only roughness difference). Roles across the sequence:

| GT id | Role |
|-------|------|
| `obj_move` | Translates across the table |
| `obj_occlude` | Stays put; a dark slab slides over it mid-sequence |
| `obj_leave` | Leaves the frame and later returns (same id) |

Negative case (second pass / trailing frames):

| GT id | Role |
|-------|------|
| `obj_depart` | Leaves permanently |
| `obj_replace` | Different similar object enters — must **not** re-id as `obj_depart` |

## Ground truth

Per-frame JSON lists object id, world position (Blender +Z up), and 2D
projected centroid/bbox in the render. Coordinate frame is declared as
`blender-world` / `OPENCV_CAMERA` for image space.

## Run

```bash
scripts/run-ocular-tracking.py --output artifacts/ocular/tracking
```

Renders with a real Blender binary, attests the run, segments/tracks every
frame, and prints ID switches, MOTA, occlusion survival, re-id precision/recall,
the 3-id confusion matrix, and the replaced-object negative result.
