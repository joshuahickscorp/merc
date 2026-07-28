# Perceptual critics (VisionMCP V2)

Thirteen specialist roles catch what technical metrics miss. Each critic returns
`CriticFinding` records that **must** bind evidence references and carry at least
one numeric measured quantity.

## Contract

- Interface: `Critic.role`, `applies_to(subject)`, `critique(subject, evidence)`.
- Findings use `blender_vision.v2.records.CriticFinding` (evidence required).
- Workspace seals a `PerceptualCritique` via `CriticWorkspace.run`.
- `passed` is true only when there are no `major` or `critical` findings.
- Authority is derived with `derive()` from input authorities; never hand-promoted.

## Roles

| Role | Primary measurements |
| --- | --- |
| `product_photographer` | silhouette edge strength, background separation, highlight clip fraction |
| `cinematographer` | shot variety, camera lag vs scroll, dead scroll fraction, turn intent |
| `industrial_designer` | relative dimension error, part count, drawer depth variance |
| `environment_artist` | instance variation entropy, occupied volume fraction, depth complexity |
| `material_artist` | plastic-metal score, pore scale ratio, flat-depth pretence |
| `lighting_artist` | luminance variance, shadow floor, clip fraction, contact shadow strength |
| `organic_artist` | curvature variance, left-right symmetry |
| `groom_artist` | clump-to-body ratio, fur density |
| `editorial_art_director` | generic composition score, template similarity, text volume per beat |
| `interaction_designer` | response latency, dead-zone fraction, skip/get-app discoverability |
| `accessibility_reviewer` | WCAG contrast ratio, focus-order completeness, reduced-motion equivalence, text equivalents |
| `performance_engineer` | real frame p50/p95, long tasks, heap growth, CLS (refuses simulated data) |
| `adversarial_acceptance_reviewer` | reference-class mismatch, hidden-view delta, threshold relaxation, detail-for-budget |

## Fixtures

Deterministic fixtures live under `benchmarks/critics/fixtures/` and are produced by:

```bash
.venv/bin/python benchmarks/critics/scripts/generate_fixtures.py
```

Each specialist has a fault fixture that must be caught and shares the clean
`control` fixture that must produce zero findings.

## Repair

`BoundedRepairPlan` names mutable parameters, blast radius, and an acceptance
test. `BoundedRepairRunner` applies a bounded mutation, re-runs critics, and
checks an unrelated control for regression.

## Demo

```bash
.venv/bin/python scripts/run-critics-and-perception.py --output artifacts/v2/critics
```
