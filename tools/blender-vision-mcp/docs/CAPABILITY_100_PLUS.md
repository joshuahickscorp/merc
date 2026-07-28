# Evidence-bound capability scoring

VisionMCP uses a 0–110 evidence scale. A capability implementation, adapter, fixture, or plan does
not receive a production score by existing. Scores increase only through receipt-bound execution
against the immutable facet catalog and acceptance registry in `benchmarks/100_plus/`.

## Levels

- **100**: production-complete for the declared reference class on required real runtimes and at
  least one registered external or held-out test, with reproducible receipts and zero unresolved
  P0/P1 defects.
- **105**: level 100 plus at least three unseen targets or variants without manual implementation
  changes.
- **110**: level 105 plus adversarial recovery, bounded automatic repair, and zero global regression
  across the fixed corpus.

Every facet has one of four states:

- `PROVEN_100_PLUS`
- `PROVEN_BELOW_100`
- `BLOCKED_EXTERNAL`, with an exact resumption contract
- `NOT_APPLICABLE`, with a concrete justification

The initial catalog preserves all 139 facets from the supplied grading report: 50 application
facets, 64 3D facets, and 25 system facets. No baseline score was raised during catalog creation.

## Public commands

```bash
bvmcp capability list
bvmcp capability list --domain app
bvmcp capability show app.backend_generation
bvmcp capability evaluate app.backend_generation
bvmcp capability evaluate all --evidence /absolute/path/to/evidence.json
bvmcp capability report \
  --evidence-dir artifacts/100-plus/evidence \
  --output artifacts/100-plus/final-scorecard.json
bvmcp capability verify-report artifacts/100-plus/final-scorecard.json
```

`list` and `show` expose the immutable requirements and baseline. `evaluate` applies evidence
without rewriting the catalog. `report` emits a canonical digest-bound scorecard. `verify-report`
checks the report digest, catalog and registry digests, Git authority, summary, and every referenced
evidence source.

## Evidence bundle

An evidence bundle is a JSON document with:

```json
{
  "schema_version": "1",
  "facet_id": "app.backend_generation",
  "proposed_score": 100,
  "git_head": "0000000000000000000000000000000000000000",
  "catalog_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "registry_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "builder_identity": "isolated-builder",
  "evaluator_identity": "sealed-evaluator",
  "evaluator_had_builder_access": false,
  "manual_edits_receipted": true,
  "thresholds_changed_after_run": false,
  "metrics": {
    "acceptance_gate_pass_rate": 1.0,
    "p0_defects": 0,
    "p1_defects": 0
  },
  "records": [],
  "external_blockers": [],
  "target_variants": [],
  "reproduction_commands": [],
  "unresolved_defects": {
    "P0": 0,
    "P1": 0
  }
}
```

Each evidence record binds an executed artifact path to its SHA-256 and declares one kind:

- `implementation`
- `real_runtime`
- `external_holdout`
- `receipt`
- `adversarial_recovery`
- `bounded_repair`
- `global_regression`

A runtime record names its actual runtime. An external record names the registered holdout test,
target, and reference class. A record also discloses whether it is fixture-only, adapter-only, or
simulated hardware.

## Rejection rules

The evaluator rejects:

- unsupported score increases or missing receipt classes;
- evidence produced for a different facet or Git head;
- evidence produced against stale catalogs or changed thresholds;
- missing or digest-mismatched artifacts;
- an adapter or unexecuted plan presented as successful execution;
- fixture-only evidence presented as an external/held-out run;
- simulated hardware presented as physical runtime evidence;
- missing registered runtime or holdout coverage;
- builder and evaluator identity reuse;
- evaluator access by the builder;
- manual edits outside the receipt chain;
- unresolved external blockers at a 100+ score;
- nonzero P0/P1 defects at 100+;
- fewer than three unseen variants at 105;
- missing adversarial recovery, bounded repair, or zero-regression proof at 110.

An evaluation rejection preserves the registered score and records all causes. It never silently
promotes a partially supported score.

## Authority files

- `original_facets.json`: preserved baseline facets and per-facet requirements
- `score_schema.json`: structural contract for the facet catalog
- `acceptance_registry.json`: fixed level definitions and anti-cheat rules
- `scripts/generate-100-plus-facets.py`: deterministic catalog generator from the preserved grading
  ledger

Evidence files and score reports are append-only benchmark artifacts. Threshold changes require a
new registry digest and invalidate evidence created under the old registry.
