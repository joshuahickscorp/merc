# Adaptive execution frontier — tranche status

Branch `perf/execution-frontier`, as of 2026-07-30. `release/rc1-go-closure`
untouched.

> Superseded in part by [ADAPTIVE_EXECUTION.md](ADAPTIVE_EXECUTION.md), which
> records the capability/activation split, governed verification classes, the
> checkpoint gate and the llama.cpp failure matrix. In particular, the "Not
> established" note below about the failure matrix is now largely closed and the
> claim that a lifecycle promotion is a coordinated agent deploy is no longer
> true — that was the defect the split removed.

This is what is done, what is not, and what the not-done items are blocked on.
Nothing here is projected: every "done" row has a test or a receipt behind it,
and every "not done" row says so plainly rather than being described as partial.

## Completion checklist

| # | Condition | State |
|---|---|---|
| 1 | PostgreSQL governs immutable runtime profiles | **done** — `(runtime_profile_id, revision)` key, history retained, delete refused by trigger, content drift refused at sync |
| 2 | Every worker binds profile ID, revision and digest | **done** — three-column identity with a NULL-safe CHECK and a composite FK; dispatch capability refused without it |
| 3 | Candle remains active and backward compatible | **done** — full suite green, `candle_metal` still the only routable profile |
| 4 | A second runtime is REAL_RUNTIME_PROVEN through the full Merc chain | **NOT done** — engine proven, routing mechanism built, chain not driven. See below |
| 5 | RuntimeSelector produces shadow decisions and regret measurements | **NOT done** — not started |
| 6 | Routing has not changed without promotion evidence | **done** — `llama_cpp_metal` is still `VALIDATED`; the advertised projection is still exactly the two candle cells, asserted by test |
| 7 | In-flight coalescing works with one payable and independent discounted receipts | **Bound money-path proofs against a double upstream (commit `4ef1922a`).** `evidence/reuse/public-path-coalescing-128-to-1.json` and `evidence/reuse/public-path-128-to-1.json` show 128 deliveries to 1 upstream call through the real public handler; they prove control-plane money/receipt paths and do not measure GPU performance. The 2026-07-31 correction still records earlier store-level gaps; see `evidence/state/correction-2026-07-31-coalescing-and-directed-routing.json` |
| 8 | Tokenization / tool-schema caches with real callers and measured savings | **partial, closed honestly** — tool/schema identity cache is production-wired; control-plane tokenization **DOES_NOT_APPLY** (no tokenizer on control plane). Bound audit: `evidence/perf/five-cache-architecture-audit.json`. Do not build an empty tokenizer cache |
| 9 | Token-budget batching with measured policies per latency class | **NOT done** — not started |
| 10 | No calibration or overhead authority can affect money | **done** — call-graph gate, mutation-verified |
| 11 | Full suite green | **done** — on an isolated database; see the caveat below |
| 12 | Branch pushed | **done** |

## Runtime authority is cell-specific

Added after the profile-level lifecycle was identified as too coarse. The proof
boundary is finer than the profile: llama.cpp's embed cell is proven and its
byte_exact generation cell is measured unsuitable, so one lifecycle would let the
first promote the second.

Cells now carry their own lifecycle, benchmark authority, quality tier, rejection
reason and measured parallelism limits, and the CELL is the routable unit in both
the Go and Rust projections. A profile cannot inflate a cell and a cell cannot
outrank its profile. `REJECTED_FOR_CONTRACT` is a decision with a required stated
reason, not a synonym for unfinished. `REAL_RUNTIME_PROVEN` is evidence rather
than permission — reachable by directed routing, never by ordinary buyer traffic.

Receipts declare which models they measured, so a MiniLM comparison can no longer
be cited as evidence about Llama generation on the same engine.

## Directed routing

> **Corrected 2026-07-31.** `buildWorkloadDecisionDirected` has zero production
> callers. The mechanism is real and is reached only from tests; the operator
> half described below does not exist as an entry point. Every llama.cpp chain
> proof therefore submits through a test-constructed job row rather than through
> `POST /v1/jobs`.

An operator or a test can force a governed job onto a named cell. The name is a
server-side argument, never read from the buyer wire, and is frozen into the
decision so the choice is auditable and the stored decision reconstructs as
itself. The reachable set is VALIDATED and above with terminal states excluded —
the floor is VALIDATED because a cell reaches REAL_RUNTIME_PROVEN *by* being
driven through the chain, so requiring it first would make the state unreachable.

Building it surfaced a coupling that would have blocked any second runtime:
worker matching used the BUYER's declared model kind, so a request naming
`all-minilm-l6-v2` as `hf` could never reach llama.cpp's GGUF cell whatever the
evidence said. The frozen cell now supplies the kind and the scheduler compares
against that.

## What item 4 is actually missing

The engine half is proven and measured:

- llama.cpp executes the embed cell through `RuntimeDriver` and agrees with
  candle at **0.999998 minimum cosine** against the 0.999 gate the control plane
  applies to a `cosine`-verified cell — reproduced here, not cited;
- `evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json` binds
  source commit, both profiles' id and revision, the exact artifacts per wire
  kind, hardware, engine configuration, and the quality result;
- `EmbedRunner` dispatches by driver, so a llama.cpp worker can serve the cell it
  is registered for.

What has not been driven is those bytes through submit → claim → start → commit →
verify → settle → payable → receipt on `llama_cpp_metal`. That is the whole of
what stands between `VALIDATED` and `REAL_RUNTIME_PROVEN`, and the lifecycle has
deliberately not been moved without it.

The concrete blocker is a fixture, not a design gap. Verification settles against
a stored result artifact, and every verification test in this tree constructs its
processor with a `nil` Storage — they exercise leasing and drain mechanics, never
an artifact round trip. Driving the chain end to end needs an object-storage
fixture (the compose MinIO is available to `make dev-up` but no test uses it).
That fixture is the next piece of work, and it unblocks the chain for both cells
at once.

## Autonomous two-agent product chain

Two real `merc-agent` processes, two suppliers, two engines, against a control
plane running the production `Routes()`. Every assertion reads what the control
plane stored.

| Stop condition | State |
|---|---|
| 1. Two enrolled agents claim autonomously | **done** |
| 2. Both valid jobs verify and finalize | **done** — `task=complete`, `verification=pass` |
| 3. Both buyer debits reconcile | **done** — ledger conserves |
| 4. Both supplier payables correctly attributed | **done** — one row each, distinct suppliers |
| 5. Both Merc contributions positive | **done** — 0.393166 USD each |
| 6. v2 rejection creates no success payment | **done** — outcome `fail`, 0.000000000 USD net, reputation 0.95→0.801 |
| 7. Both final receipts verify | **done** — authority names the executing cell; ten tamper mutations refused |
| 8. llama.cpp embed REAL_RUNTIME_PROVEN | **done** — profile r7→r8, cell promoted, routable=false |
| 9. Candle remains ACTIVE | **done** |
| 10. llama.cpp generation remains rejected | **done** |
| 11. Full suite green | **done** |
| 12. Branch pushed | **done** |

Measured, identically on both cells:

```
candle    task=complete verification=pass  cell=candle-metal-minilm-embed    kind=hf
llama_cpp task=complete verification=pass  cell=llama-cpp-metal-minilm-embed kind=gguf

buyer_charge    -0.587166000
platform_take   +0.393166000
supplier_credit +0.194000000
                ------------
residual                   0

cross-cell equivalence: mean=0.999999 min_row=0.999999 revision=embed-cosine-v2
```

Six server-side defects were found by running real agents, none of which code
reading had surfaced: the advertised engine was hardcoded to candle; the agent's
capability projection was routable-only; worker profile resolution was
routable-only; `ValidateWorkerAgainstProfile` required a routable lifecycle;
`validEngines` was a hand-written `{candle, vllm}` literal that refused
`llama_cpp` at the door; and `agent.toml` requires `power_only` with no serde
default.

### Rejection economics

A honeypot submitted beside a primary, with a compute plan declaring both, graded
against the approved answer for DIFFERENT text:

```
honeypot verification outcome: "fail"
0 supplier rows netting 0.000000000 USD
reputation 0.95 -> 0.801, honeypot_fail event recorded
task requeued (status=retrying)
```

Two assumptions were corrected against what the policy actually does. Quarantine
is NOT the consequence of a wrong answer — it is reserved for an answer-class
mismatch, where the supplier's engine identity does not match what it claimed,
which is a different accusation. And "pays nobody" means the credit does not
STAND: the supplier is credited on commit and the grade arrives afterwards, so
the assertion is on the NET across credit and clawback.

A claim was also retired along the way. The settlement commit said the llama.cpp
task was graded against candle's approved output; it was not. The seeding ran an
`UPDATE` before `SubmitJobTx` inserted the row, so it matched nothing and the task
stayed ordinary. Both artifacts genuinely agree, so a passing comparison and an
unrun one were indistinguishable there — a check that could not fail. Withdrawn
in the tree and in the history.

### Promotion

`llama_cpp_metal` r7 → r8, profile and embed cell VALIDATED →
REAL_RUNTIME_PROVEN, receipt at `evidence/chain/two-agent-product-chain.json`.

REAL_RUNTIME_PROVEN is reachable by directed operator or test routing only. The
cell is **not** routable, Candle remains ACTIVE with both cells routable, and
`llama-cpp-metal-llama1-infer` remains REJECTED_FOR_CONTRACT.

The lifecycle guard that gated this used to hardcode "only candle_metal has a
Merc-chain receipt". That was true when written and became false the moment
llama.cpp completed the chain; naming a profile made the rule un-passable by
design. It now requires an existing chain or canary receipt, so the next runtime
can satisfy it and a profile claiming the state with nothing behind it still
fails.

### Not established

The failure matrix — agent death before and after claim, runtime unavailability,
input download failure, upload interruption, verifier restart, finalizer restart,
settlement retry — is not exercised. No CANARY or ACTIVE promotion follows from
this receipt.

## Caveats that are not caveats about this branch

**The shared development database is polluted.** `postgres://cx@localhost/cx`,
which `make ci` defaults to, holds 82 orphaned test-fixture jobs dating to
2026-07-28 with `workload_decision IS NULL`. The scheduler's frozen-runtime
filter has a legacy `workload_decision IS NULL OR …` escape hatch, so those
orphans are claimable by any worker, and two claim-ordering tests fail there
while passing against a freshly applied database. Every result in this tranche
was taken on an isolated database, as the tranche requires. The leak predates
this branch.

## Two findings worth carrying forward

**A revision bump had never been exercised against a populated database.** The
first real one — forced by widening the content digest — failed three separate
ways, each invisible against an empty table: insert-before-demote against a
partial unique index, a superseded revision still holding its cells routable, and
a child-row backfill that collapsed two revisions onto one key.
`TestRevisionBumpSucceedsAgainstAPopulatedRegistry` now drives it the way an
upgrade does.

**The exact-result cache was shared across tenants.** Not a bug in coalescing —
a pre-existing side channel that wiring coalescing forced into view. The bytes
were identical either way, so no correctness test could see it; the leak was that
buyer B could learn buyer A had run a request by watching it return at the reuse
price. `RequestIdentity` is tenant-scoped now and refuses an empty scope rather
than hashing it as the empty string.

## Grok

`DEFERRED_USAGE_LIMIT`. Not auth-blocked, not awaiting an external action. The
queued audits keep their contracts and are re-runnable when usage returns:

| audit | contract |
|---|---|
| runtime authority | is `(id, revision)` immutability actually enforced end to end, including the digest's coverage of resolved artifacts |
| plan actuals | can any money or admission path consume a calibration read — adversarially, against the new call-graph gate |
| selector promotion | not yet applicable; no selector exists to audit |
| coalescing privacy/money | can any caller obtain another tenant's result, or a second supplier payable, through the in-flight path |
| batching benchmark integrity | not yet applicable; no batching sweep exists to audit |

No acceptance condition in this tranche depended on Grok-specific evidence. The
direct adversarial work was done inline instead: the call-graph gate was
mutation-tested rather than inspected, and the coalescing money and tenant
properties are asserted by tests that fail if either is broken.
