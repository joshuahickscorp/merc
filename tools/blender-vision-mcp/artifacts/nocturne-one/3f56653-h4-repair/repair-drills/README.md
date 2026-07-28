# NOCTURNE/ONE repair-drill replay

`nocturne-repair-drills.receipt.json` records the fixed twelve-class repair
corpus executed against the final real application and 3D acceptance receipts.
Every drill:

1. injects one deterministic failure into one observed evaluator fact;
2. proves the fixed detector rejects that fact;
3. rejects threshold relaxation and whole-baseline replacement;
4. selects the exact one-fact inverse repair; and
5. proves canonical restoration of the complete input receipt with no failed
   global assertion.

Reproduce from `tools/blender-vision-mcp`:

```bash
uv run bvmcp benchmark run-nocturne-repair-drills \
  --app-receipt artifacts/nocturne-one/3f56653-h4-repair/evaluator-app/nocturne-app.receipt.json \
  --three-d-receipt artifacts/nocturne-one/3f56653-h4-repair/evaluator-3d/nocturne-3d.receipt.json \
  --output artifacts/nocturne-one/3f56653-h4-repair/repair-drills/nocturne-repair-drills.receipt.json
```

Claim boundary: this is executable receipt-level repair replay over frozen real
runtime evidence. It is not twelve newly launched Blender, browser, API,
database, accessibility, and performance evaluator runs.
