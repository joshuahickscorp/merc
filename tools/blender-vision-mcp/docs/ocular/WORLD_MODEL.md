# Ocular world model (Phases H)

The persistent world is the memory-bearing middle of the perception loop:

```
sense → calibrate → attend → segment → track → geometry → identity
→ world model → predict → surprise → next-best-view → act → verify → remember
```

Implementation: `src/blender_vision/ocular/world.py`.

## Records

| Record | Kind | Role |
| --- | --- | --- |
| `WorldState` | `ocular.world-state` | Scene container, append-only belief history, lighting fingerprint |
| `Entity` | `ocular.entity` | Identity, pose, trajectory, belief sets, surfaces, confidence |
| `Surface` | `ocular.surface` | Classifiable surface with Bible §12.5 provenance |
| `Relation` | `ocular.relation` | Bible §12.2 links (`same_as` vs `candidate_same_as`) |
| `BeliefUpdate` | `ocular.belief-update` | One irreversible step: prior, evidence, model, posterior |

All subclass `V2Record` and seal with content digests. Authority is capped by
`derive()` and visibility ceilings. Coordinate frame is always declared
(default: Blender Z-up).

## Laws

1. **Belief history is append-only.** Updates never rewrite earlier entries.
2. **Contradiction adds a competing belief.** A disagreeing observation does not
   silently replace the prior posterior; both remain in `entity.belief_sets`.
3. **Occlusion and absent frames do not drop identity.** Confidence decays and
   pose sigma grows; the entity stays in the world.
4. **`same_as` is never inferred from `candidate_same_as` alone.** Promotion
   requires a non-empty recorded evidence list.
5. **Surfaces are classifiable** (`directly_observed`, `symmetry_inferred`, …).
   Inferred provenance cannot claim `OBSERVED` authority.
6. **Persistence is byte-identical across process restart** for the beliefs
   slice (`beliefs_digest` / `beliefs_bytes`).

## Operations

- `build_world_model(observations, scene_id=…)` — ordered frames → sealed world
- `update_world_model(world, observation)` — one frame (or `absent=True`)
- `query_world(world, query)` — entity / class / relations / scene summary /
  `compare_sessions`
- `explain_belief(world, entity_id, slot)` — full history for one slot
- `list_uncertainties(world)` — confidence / sigma ranking
- `compare_worlds(prior, current)` — Phase M change classes
- `save_world` / `load_world` — disk checkpoint

## Observation schema

```json
{
  "frame_index": 0,
  "track_source": "ground_truth",
  "absent": false,
  "lighting": {"mean_luminance": 0.5},
  "entities": [
    {
      "entity_id": "cup",
      "class_label": "cup",
      "pose_m": [x, y, z, qw, qx, qy, qz],
      "visible": true,
      "appearance": {},
      "surfaces": []
    }
  ]
}
```

When a live tracker is unavailable, set `track_source` to `ground_truth` and
state that in the receipt. This task does not implement segmentation/tracking.

## Dynamic-room change classes

`compare_worlds` reports, each with confidence and evidence:

| Class | Meaning |
| --- | --- |
| `same_scene` | Sufficient identity overlap |
| `moved_object` | Shared entity pose delta above tolerance |
| `removed_object` | Present in prior, absent in current |
| `new_object` | Present in current, absent in prior |
| `lighting_only` | Luminance/lighting channel change; **never** a geometry change |

## Verification

```bash
.venv/bin/python -m pytest -q tests/test_ocular_world.py
.venv/bin/python scripts/run-ocular-world.py --output artifacts/ocular/world
```
