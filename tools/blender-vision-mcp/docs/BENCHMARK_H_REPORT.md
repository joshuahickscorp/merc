# Benchmark H report

## Result boundary

Benchmark H is incomplete as the requested same-Grok uplift study. No exact
Grok model/version or noninteractive Grok credential was available. H0–H2 are
therefore `BLOCKED_EXTERNAL` with fixed prompts, packet projections,
prohibitions, limits, and a resumption contract in
`artifacts/nocturne-one/benchmark-h/resume-manifest.json`.

Two useful Codex engineering conditions were run:

- H3: Codex CLI default model (not pinned in its command receipt), VisionMCP
  governed app/3D pipeline;
- H4: explicit `gpt-5.6-sol`, H3 facilities plus fixed candidate portfolios and
  bounded repair requirements.

Because model identity was not held fixed and there is one run per condition,
the results do not establish causal model/tool uplift. They expose a valuable
failure mode: a faster, locally green portfolio/repair process can still be
worse under a frozen external evaluator.

## Isolation

Both conditions used fresh detached worktrees at the sealed benchmark commit
and the same governed packet/contract. The builder could not read the oracle,
hidden views, or hidden mobile trace.

H3’s OS sandbox proved oracle filesystem separation, but its process could read
global Codex state; it is not a strong cognitive-isolation claim. H4 used a
fresh isolated Codex home and a single macOS sandbox profile that also denied
global Codex state, the H3 worktree/output/evaluator paths, repaired evidence,
and the first failed H4 launcher output. The H4 canary scan passed.

The first H4 launcher attempt failed in 0.24 seconds because nested
`sandbox-exec` is prohibited. It produced no child stdout and is classified as
infrastructure failure, not a model result. A combined-profile smoke receipt
passed before the fixed prompt was relaunched unchanged. Both receipts are
preserved.

## Quantitative condition ledger

| Measure | H3 | H4 |
|---|---:|---:|
| Builder wall clock | 3,356.67 s | 977.81 s |
| Builder exit / sealed boundary | 0 / PASS | 0 / PASS |
| Input tokens | 22,610,144 | 3,878,080 |
| Cached input tokens | 22,094,080 | 3,743,744 |
| Output tokens | 96,985 | 34,556 |
| Reasoning output tokens | 32,214 | 2,860 |
| Command executions | 74 | 33 |
| MCP calls in child transcript | 4 | 0 |
| File-change items | 22 | 18 |
| Local failed attempts/failures | 6 | 13 |
| Portfolio candidates | not required | 6 across 2 portfolios |
| Bounded repair plans | not structured | 13 |
| Locally accepted repairs | not structured | 7 |
| Human interventions | 0 | 0 |
| Frozen 3D evaluator | PASS after parent bounded repair | FAIL |
| Frozen application evaluator | PASS after parent bounded repair | FAIL |
| Final global regression count | 0 for accepted H3 | not assignable; evaluator failed |

H4’s builder phase was 70.9% faster and used materially fewer tokens/commands.
That is efficiency evidence only. It did not improve externally measured
quality.

The transcript does not contain reliable peak-memory, per-subsystem compute,
or exact builder-side render/capture counters. Those fields remain
`NOT_CAPTURED`; they are not backfilled from guesses. Both frozen 3D evaluators
rendered the fixed six public and four hidden views.

## H3 outcome

The H3 child created an editable BLEND, two named-identity GLBs, a complete
five-route TypeScript application, SQLite API, migrations, tests, fallbacks,
and seven sealed attempts. The initial frozen evaluation passed every 3D gate
but failed one of 27 application assertions: the eighth keyboard target.

The parent’s receipt-bound smallest-surface repair changed focus order only,
preserved attempt 007, sealed attempt 008, and reran all local and frozen
global gates:

- 3D: 17/17 assertions passed;
- application: 27/27 assertions passed;
- hidden silhouette IoU: 0.9565–0.9815;
- hidden mobile: 10/10 to successful `/receipt`;
- accessibility: 0 critical/serious;
- performance budgets: all pass;
- unresolved P0/P1 and global regressions: 0.

H3 is the accepted VisionMCP condition for this benchmark.

## H4 local result and frozen rejection

H4 preserved two genuine three-candidate portfolios:

- 3D outer-shell cross-section/curve resolution;
- poster-to-renderer loading boundary.

It recorded 13 failures (2 P0, 9 P1, 2 P2), 13 selected repair plans, seven
locally accepted repairs, and six rejected or incomplete local wins. Its ledger
claimed a final global pass and sealed one accepted attempt.

The external evaluator disproved that local result.

3D failures:

- dimensions 325.300 × 180 × 380.047 mm, with 1.66% X and 5.57% Z error;
- outer-shell placement error 3.80% (>2%);
- required scene root absent and all 12 parts unparented;
- public silhouettes 0.7867–0.8532 (<0.95);
- hidden silhouettes 0.8498–0.8892 (<0.92).

Twelve of 17 structural/material/output assertions passed, including part
identity, material classes, UV/topology/normals, animation, GLB validation and
reimport, LOD identity, and BLEND reopening. The P0 silhouette/dimension failures
still make the 3D result a hard fail.

Application failures:

- fresh-clone `npm run verify` found no test files because the configured test
  glob did not match the copied project;
- transient-error mode returned 201 instead of 503;
- first reservation returned 200 instead of 201 and conflict returned 200
  instead of 409;
- `#part-selector` was a `div`, not the public-contract `select`, aborting the
  evaluator journey.

Authentication denial, actor scope, and local API latency passed before the
journey aborted. Later browser/accessibility/performance assertions were not
reached and cannot be scored as passes.

The H4 source, portfolios, repair ledger, complete transcript, local receipt,
archive, and negative evaluator receipts are preserved under
`artifacts/nocturne-one/db820c9-h4-independent/`.

## Qualitative conclusion

Candidate portfolios and repair ledgers improved process observability and H4
efficiency, but the chosen public metrics were not sufficiently coupled to the
frozen acceptance authority. In particular, the 3D portfolio recorded a
0.1% dimension error while the external inspector measured up to 5.57%; the
local “global pass” also missed the public API element type and status-code
contract.

The next closure is not “more repair loops.” It is to bind portfolio metrics
and every global gate to the same external inspection contract used for
acceptance, then rerun three or more unseen targets with a truly fixed model
identity. Until that experiment exists, this report makes no causal uplift or
universal-builder claim.
