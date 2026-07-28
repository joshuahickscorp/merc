# Ocular predictive loop and surprise (Phase I)

Implementation: `src/blender_vision/ocular/predict.py`.

## Contract

A prediction is not a soft guess. It carries:

- **expected** value (pose, visibility, frame features, …)
- **tolerance** band in explicit units
- **horizon** (frames the prediction is valid for)
- **source_belief_id** linking back to the belief that licensed it

When observation falls **outside** tolerance a `SurpriseEvent` fires:

- records prediction, observation, magnitude, contradicted belief
- **raises uncertainty** on the entity (confidence down, pose sigma up)
- appends a competing / contradicting belief update

When observation falls **inside** tolerance for a pose prediction, confirming
evidence **lowers uncertainty**.

## API

| Function | Role |
| --- | --- |
| `predict_next(world, horizon=1)` | Constant-velocity pose + visibility + existence + frame features |
| `evaluate_prediction` / `evaluate_prediction_detailed` | Compare one prediction to observation |
| `evaluate_observations` | Batch evaluate a frame |
| `list_surprises(world)` | Recorded surprise events |
| `make_prediction(...)` | Manual sealed prediction for benchmarks |
| `uncertainty_trajectory(world, entity_id)` | Confidence time series from belief history |

## Prediction kinds

- `pose` — constant velocity over tracked trajectory
- `visibility` — persistence prior
- `existence` — entity still present
- `frame_features` — mean luminance persistence
- `camera_path` — authored camera position
- `browser_animation` — scroll / phase timeline
- `material_response` — specular peak / BRDF check

## Six benchmarks (runner)

1. **expected_motion** — observation matches constant-velocity prediction (no surprise)
2. **unexpected_moved_object** — large pose jump → surprise + confidence drop
3. **missing_object** — existence prediction fails
4. **wrong_browser_animation** — animation phase outside tolerance
5. **camera_path_mismatch** — camera position disagree
6. **material_response_mismatch** — specular peak disagree

Each prints predicted vs observed vs magnitude.

## Uncertainty trajectory

```
confirming evidence → confidence ↑, sigma ↓
surprise           → confidence ↓, sigma ↑
occlusion / absent → confidence ↓, sigma ↑
```

Printed by `scripts/run-ocular-world.py` section 5.

## Authority

Predictions are `MODEL_DERIVED`. Surprises that bind sensor disagreement are
`SENSOR_DERIVED`. Uncertainty updates that only reflect absence are `INFERRED`.
No fallback may claim a physical PASS; use `RuntimeAttestation` when hardware
actually ran.
