# Active perception / next-best-view (VisionMCP V2)

The planner answers: **which missing evidence would reduce uncertainty most**,
and asks for it with exact capture instructions.

## Modules

- `uncertainty.py` — uncovered surface fraction, scale-authority state, material
  confidence spread, hypothesis disagreement, lighting/calibration/lens gaps.
- `information_gain.py` — expected newly covered area from the coverage atlas
  plus expected disagreement resolution (variance of candidate predictions on
  newly visible surface), blended with a simulated post-view uncertainty total.
- `planner.py` — `NextBestViewPlanner.ask_next_view(target) -> list[NextViewRequest]`.

## Consumer-object request kinds

When applicable, the planner emits sealed `NextViewRequest` records for:

- underside
- side
- scale reference
- diffuse light
- grazing light
- lens metadata
- calibration target

Each request includes missing uncertainty, expected reduction, machine-readable
capture instructions, human instructions, required calibration, acceptable
alternatives, reason, and priority.

## Redundancy and stop conditions

A proposed view is **not emitted** when:

- its signature already exists in coverage, or
- expected information gain is below the configured threshold and it adds no
  new measurement modality.

Stop reasons:

1. `gates_satisfied` — acceptance gates already met; no further capture.
2. `gain_too_low` — no non-redundant positive-gain candidate remains.
3. `user_declined` — the operator declined further capture.

## Demo loop

```bash
.venv/bin/python scripts/run-critics-and-perception.py --output artifacts/v2/critics
```

The script plans on a partially covered synthetic target, satisfies the top
request, shows measured uncertainty decrease, confirms the satisfied view is
not re-requested, then shows zero requests on a fully covered target.
