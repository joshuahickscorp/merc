# Phase R — Representation portfolio

Per reconstruction target the portfolio produces:

| Kind | Role |
| --- | --- |
| Mesh | Editable geometry, measurement (with scale), animation proxy |
| Point cloud | Sparse measurement, web preview |
| Procedural | Parameter edits, animation |
| Retrieved | Only when license allows; else BLOCKED |
| Gaussian / radiance | **BLOCKED** on this host — no weights, no network |

## Purpose evaluation

Photoreal view synthesis is **not** satisfied by mesh/points. Those remain
honestly unsuitable for that purpose when radiance is blocked.

```bash
.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio
```
