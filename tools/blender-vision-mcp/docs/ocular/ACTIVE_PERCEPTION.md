# Ocular active perception (next-best-view)

Implementation: `src/blender_vision/ocular/active.py`.

## Goal

Plan views that **demonstrably reduce uncertainty**. A planner whose predicted
information gain does not correlate with realised gain is guessing.

## Flow

```
plan_next_view(world) → NextViewRequest
  → satisfy (render in Blender when available + perception re-observation)
  → measure_information_gain (predicted vs actual)
  → suppress redundant signatures
  → stop when expected gain < min_gain
```

## API

| Symbol | Role |
| --- | --- |
| `NextViewRequest` | Target entity, expected reduction, capture instructions |
| `plan_next_view` | Rank entities by uncertainty; emit or stop |
| `satisfy_next_view` | Feed observation; lower uncertainty; record gain |
| `measure_information_gain` | Confidence rise + sigma drop → [0, 1] |
| `predict_information_gain` | Expected gain from current entity state |
| `gain_correlation` | Pearson predicted vs actual across reports |
| `run_nbv_loop` | Plan → satisfy until stop / budget |

## Stop and suppress

- **Redundant**: same view signature already issued → `suppressed=True`
- **Gain too low**: predicted gain `< min_gain` → `declined=True`
- **Budget exhausted**: `max_requests` reached

## Evidence in the receipt

`scripts/run-ocular-world.py` records per request:

- predicted_gain
- actual_gain
- confidence/sigma before and after
- residual and correlation

Uncertainty after a satisfied request must fall (confidence rises and/or sigma
drops) or the runner exits non-zero.

## Blender

When Blender is available, NBV requests a `BLENDER_EEVEE_NEXT` render of the
requested orbit (materials carry visual signal — not Workbench). If Blender is
blocked on this host, the satisfaction path still re-observes via perception and
attests the render attempt honestly (`CANDIDATE_ONLY` / `BLOCKED`).
