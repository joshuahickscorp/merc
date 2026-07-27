# Dynamic-room memory report (Phase M)

## Scenario

| Session | Content |
| --- | --- |
| Session 1 | Observes room: sofa, lamp, book, table under cool soft lighting |
| Session 2 | Moves lamp, removes book, adds plant, changes lighting only (warmer, brighter) |

Fixture: `benchmarks/ocular_room/`.

## Required outputs

For each change class the system must report **detected**, **confidence**, and
**evidence**:

1. **same_scene** — shared entity identity (sofa, table, lamp still known)
2. **moved_object** — lamp pose delta above move tolerance
3. **removed_object** — book absent in session 2
4. **new_object** — plant present only in session 2
5. **lighting_only** — mean luminance / temperature shift; **`geometry_change: false`**

The lighting-only confounder is the point of the benchmark. A system that
treats a lighting change as a geometry change fails the gate.

## How classification works

`compare_worlds(prior, current)`:

- Geometry moves: Euclidean pose distance on shared entity ids
- Removals / additions: set difference on entity ids
- Lighting channel: mean luminance (or structured lighting dict diff)
- Lighting class always sets `geometry_change: false` even when concurrent
  geometry moves exist elsewhere in the scene

## Gates (exit non-zero)

- Lighting-only reported as geometry change
- Any of the five classes missing
- Belief overwrite instead of competing belief (checked in survival section)
- Session restart digest mismatch

## Run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-ocular-world.py --output artifacts/ocular/world
```

Artifacts:

- `dynamic_session1.json` / `dynamic_session2.json` — sealed worlds
- `dynamic_room_report.json` — full change report
- `receipt.json` — overall PASS/FAIL summary

## Track source

This phase drives the world from **ground-truth tracks**. Live
segmentation/tracking is out of scope for the world-model task and is stated
explicitly in the receipt (`track_source: ground_truth`).
