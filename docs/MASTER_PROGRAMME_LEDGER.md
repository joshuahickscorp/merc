# Master Programme Ledger

The 22-step execution order this session is grinding through, and the evidence
that closes each one. Steps 1–20 are `merc.md` §19 *Immediate execution order*
verbatim. Steps 21 and 22 close the two sections that follow it: §20 (what Merc
must not do) and §21 (the definition of the first complete loop). A step is
`DONE` only when a receipt in `evidence/` or a test in `control/` proves it, not
when code exists.

Status vocabulary is the one Phase 0 mandates:

```text
ABSENT
SCHEMA_ONLY
IMPLEMENTED_UNWIRED
PRODUCTION_WIRED
REAL_RUNTIME_PROVEN
ECONOMICALLY_PROVEN
CANARY_PROVEN
PUBLICLY_USABLE
```

## Step 2 — entry classification

Read from `evidence/state/branch-state-step1.json` (HEAD `58384221`, live
database projection), `ops/go-no-go.json`, and the tree. Not from prose.

| # | Step | Entry status | Evidence read |
| -: | ---- | ------ | -------- |
| 1 | Generate current HEAD/state receipt | `DONE` | `evidence/state/branch-state-step1.json`, HEAD-bound, live DB projection |
| 2 | Skip every item already proven by that receipt | `DONE` | this table |
| 3 | Boot the production image from a clean clone | `PRODUCTION_WIRED` | `scripts/test-release-image-boots.sh` runs in `make ci`; clean-clone boot not re-verified this session |
| 4 | Canary-decision packaging and readiness | `PRODUCTION_WIRED` | `control/canary_decision.go`, `control/canary_policy.go` + tests |
| 5 | Buyer top-up and governed refund routes | `PRODUCTION_WIRED` | `POST /v1/billing/{setup,status,topup}`; refunds only behind `/admin/…` authority |
| 6 | Prepaid and realtime charge-to-payable | `PRODUCTION_WIRED` locally, provider matrix `OPEN` | `control/prepaid.go`, realtime settlement; `P1-STRIPE-TEST` open |
| 7 | Supplier throughput from benchmark authority | needs probe | `control/benchmark.go`, `runtime_profiles.benchmark_authority` |
| 8 | The complete stranger transaction | `REAL_RUNTIME_PROVEN` historical, not candidate-bound | `evidence/canary/real-runtime-embed.json`, `real-runtime-realtime.json` |
| 9 | RuntimeSelector shadow mode | `PRODUCTION_WIRED`, eligibility only | `control/runtime_shadow_selection.go`, policy `eligibility-only-v1`. No cost or latency scoring, no outcome table |
| 10 | Paired cohorts and regret | `ABSENT` through the chain | `embed-cell-candle-vs-llama-cpp-r1.json` is a bench harness, not a Merc-chain cohort; nothing computes regret |
| 11 | Narrow selector promotion authority | `ABSENT` | no promotion receipt, no rollback target |
| 12 | 128-request coalescing through money | `PRODUCTION_WIRED`, economics unproven | `control/inflight_coalescing.go`, caller `control/realtime.go` |
| 13 | Tokenization and tool-schema caches | `ABSENT` | zero callers in the receipt's `capability_wiring` |
| 14 | Token-budget batching by traffic class | `PARTIAL` | `control/batch_policy.go` declares `INTERACTIVE` and `BATCH` only |
| 15 | Governed vLLM CUDA cell through RunPod | direct runtime `REAL_RUNTIME_PROVEN`, Merc chain `ABSENT` | `evidence/runpod/cuda-first-proof.json`; `vllm_cuda` r3 cell is `DRAFT`. **The stored RunPod key returns HTTP 401** |
| 16 | Rendering IR and distributed render proof | `ABSENT` | no render adapter, no rendering IR, no Blender path in the tree |
| 17 | Merc Agent one-click experience | `PARTIAL` | `agent/` builds and benchmarks, `macapp/` exists; limit/schedule surface unverified |
| 18 | Workload IR and project detectors | `ABSENT` | `WorkloadDecision` is an admission artefact, not a project graph |
| 19 | Pool, replica and local-cluster modes | `ABSENT` as declared modes | placement authority exists; no mode taxonomy, no refusal rule |
| 20 | Level-B security and operational closure | `EXTERNALLY_BLOCKED` | `ops/go-no-go.json`: 0 P0, 8 open P1, 7 of them external |
| 21 | Audit against §20 "what Merc must not do" | pending | — |
| 22 | Prove the §21 first complete loop | pending | — |

## Session preconditions

* `GROK_STATUS=DEFERRED_USAGE_LIMIT` — no Grok delegation this session; every
  adversarial review is labelled `NOT_GROK_INDEPENDENT`.
* Stripe **test mode** only, CAD only. No live keys, no real payouts. The
  credential is live: `GET /v1/balance` returns `livemode:false` with a CAD
  bucket.
* RunPod is authorised but **the stored credential is dead** (HTTP 401 against
  `rest.runpod.io/v1/pods`), so step 15's paid half cannot execute.
* `release/rc1-go-closure` stays frozen; work lands on `perf/execution-frontier`.

## Step 3 — exit status of this session

What moved, with the evidence. Nothing here is marked done on the strength of code
existing.

| # | Step | Exit status | What landed |
| -: | ---- | ----------- | ----------- |
| 1 | HEAD/state receipt | `DONE` | `evidence/state/branch-state-step1.json` with a working live-database projection. Getting there required fixing `control/schema.sql`, which could not migrate any database that had ever served work |
| 2 | Skip proven work | `DONE` | the entry table above |
| 7 | Supplier throughput from benchmark authority | `PARTIAL` | `control/repricing_benchmark_authority_test.go` binds every `repricingBenchmarks` constant to the receipt it cites, and enforces a CONSERVATIVE bound: a constant above the measurement fails (it would price undemonstrated supply), one more than 1% below fails (it overcharges the buyer). Found the `batch_infer` constant at 138.7 against a measured 138.71389521 — conservative, so it stands. Still not the per-cell, per-hardware derivation the step asks for |
| 9 | RuntimeSelector shadow mode | `DONE` | the scoring half arrived. `control/runtime_cell_cost.go` reads measured per-cell cost out of the money path — the earlier comment claiming no such source existed was wrong, since every committed task already carries its cell, units, duration, frozen supplier liability, retries and verification outcome. Cost is a query, not a new table. Policy is now `eligibility-and-measured-cost-v2` and every row records which arm decided it |
| 10 | Paired cohorts and regret | `PARTIAL` | `SelectorRegretForScope` computes regret against the cheapest measured eligible cell, counts unmeasured decisions rather than scoring them zero, and refuses to compare cost across hardware classes. Tested against seeded completed tasks. **No cohort was driven through live agents this session**, so the regret figure has no production cohort behind it yet |
| 11 | Selector promotion authority | `DONE` | `control/runtime_cell_promotion.go`: exact scope, both sides measured above 20 samples, zero challenger verification failures, a 10% cost margin, production decisions required (not a benchmark), incumbent must be the routable cell, rollback target resolved before promotion, SHA-256 receipt reference. Refusals are returned as evidence with reasons, not as errors |
| 13 | Tokenization and tool-schema caches | `DONE as a measured decision not to build` | `BenchmarkCanonicalToolsAndSchema` measures 10.8µs per request for a large tool set against 3.9ms of Merc overhead and a 58ms median. A perfect cache removes 0.3% of Merc's cost and 0.02% of buyer latency. This control plane never tokenizes — input depth is accumulated during the upload stream that has to happen anyway. Both caches would have been decoration; the benchmark stays so the decision can be re-tested |
| 14 | Batching by traffic class | `DONE, declared unwired` | `control/traffic_class.go`: the four classes with promoted policy, and `ValidateTrafficClassPolicies` proving the ordering invariants — every lower class declares it never delays every higher one, only BACKGROUND is preemptible, INTERACTIVE stays below the throughput knee. Budgets come from the two MEASURED points on the existing curve; classes sharing one say so rather than interpolating a third. Registered in `knownUnwired` because no production path asks for the class yet |
| 19 | Execution fabric modes | `DONE, declared unwired` | `control/execution_mode.go`: POOL, REPLICA_SERVICE, LOCAL_CLUSTER, CLOUD_BACKSTOP, each placement carrying its reason and every refused mode carrying its own. Tightly coupled work is refused on a WAN fabric and on an unmeasured one; unknown parallelism fails closed to tight, so a new upstream mode cannot be silently placed on the public internet |
| 21 | §20 must-not-do audit | `DONE` | `scripts/must-not-do-audit.py` → `evidence/state/must-not-do-audit.json`. **12 PASS, 1 FAIL, 1 not mechanically checkable.** The failure is prohibition 10: `control/payment.go` has one process-wide `platformTakeRate`, so one percentage applies to embeddings, generation, rendering and every future lane alike, and the published schedule carries a single supplier share of 0.97 |

## Second pass

| # | Step | Status | What changed |
| -: | ---- | ------ | ------------ |
| 3 | Boot the production image | `DONE`, verified this session | `scripts/test-release-image-boots.sh` built the final image from the tree and served every probe with **no host files**: `/healthz`, `/version`, `/prices`, `/pricing/board.json`, `/.well-known/security.txt`, and the price board loaded from the release at digest `0e4a70dc40f8`. Receipt: `evidence/state/release-image-boot.json` |
| 4 | Canary packaging and readiness | `DONE`, already tested at entry | `/readyz` returns 503 with `reason_code: canary_policy_unconfigured` — a direct configuration error, not an unexplained buyer-facing 403 — and it reads BOTH the boot copy and a live re-read, so a decision that stops resolving on a running process cannot leave the probe green while payouts halt. `TestReadyzGoesRedWhenTheDisableDecisionStopsResolving` and `TestReadyzNamesCanaryMisconfigurationAndProbesStayReachable` |
| 10 | Paired cohorts and regret | `DONE`, and it found something | `control/paired_cohort_test.go` drives 40 real executions through two enrolled agents across both embed cells, records the shadow decisions the way `createJob` does, then reads measured cost, regret and the promotion gate off the result and writes `evidence/perf/selector/paired-cohort-embed.json`. Opt-in behind `MERC_PAIRED_COHORT=1` and registered in `allowed-test-skips.txt` |
| 12 | 128-request coalescing | `ECONOMICALLY_PROVEN` for the follower half | `control/coalesced_cluster_money_test.go`: 128 distinct delivery authorities, 128 receipts, **zero** supplier credits, every follower charged strictly less than a fresh execution, buyer debit equal to platform take across the cluster, and a second tenant unable to read any of it. The leader's single payable is explicitly not claimed — no test drives the realtime finalise path |
| 15 | Governed vLLM CUDA cell | harness ready, credential still dead | `scripts/runpod-spend-guard.py` converts a dollar cap into a pod lifetime with a self-test, holds back 20% for teardown delay, rounds spend up, and marks a receipt **inadmissible** on unverified teardown, an overrun lifetime, a floating image tag or a leftover pod. `runpod-vllm.sh experiment` wires that around the existing provisioner and refuses to start while anything else is billing. The self-test runs in `make ci` |
| 19 | Execution fabric modes | `PRODUCTION_WIRED` | `createJob` now asks `ChooseExecutionMode` and stores the mode plus the full explanation, refusals included, beside the shadow selection. Batch work reaches POOL by construction, which is the reason to record it: once a second mode is reachable, "by construction" and "by decision" are indistinguishable afterwards. A tightly coupled degree-4 workload records **no** mode rather than being defaulted onto the public internet, and the database refuses a mode with no reason |

### What the first real cohort found

Forty real executions through two enrolled agents measured **0.194 USD per unit on
both embed cells** and a mean regret of **exactly 0.000**. That is not two engines
happening to tie. It is structural, and it is the most important finding of the
session:

> The supplier payout is priced per **model** — catalogue price × units × supplier
> share — so any two cells serving one model at one price have identical
> verified-outcome cost per unit however fast either one is, and the regret between
> them is zero by construction.

A cost-only gate would have reported that as a failed cost argument — *"saves
0.00%, below the required 10% margin"* — which is true and useless, because no
same-price promotion can ever clear a cost margin. The gate now names which
argument it is making:

| Basis | Margin | What it claims |
| --- | --- | --- |
| `CHEAPER_VERIFIED_UNIT` | 10% | a real saving |
| `MORE_THROUGHPUT_AT_EQUAL_PRICE` | 25% | the same unit produced faster — a capacity gain, **not** a saving |

The wider margin on the second is because it protects against noise in a duration
measured on a shared host rather than noise in a dollar figure.

The same run measured 3.0000 ms per unit on both cells, identical to four decimals
— a task too small to separate two engines rather than two engines agreeing. The
retained bench receipt puts llama.cpp at 2,179 texts/sec against candle's 326 at
batch 8, so the cohort now embeds batches of 32.

The gate also caught its own caller: the cohort's scope claimed verification
contract `cosine_similarity` where the cell sells `cosine`, and the authority check
refused the promotion for it.

### And then the batch-32 rerun contradicted the benchmark

With batches of 32 the cells finally separated — and not in the direction the
retained benchmark implies:

| cell | measured through the chain | supplier USD/unit |
| --- | --- | --- |
| `candle-metal-minilm-embed` | 0.2188 ms/unit | 0.006062500 |
| `llama-cpp-metal-minilm-embed` | 0.2812 ms/unit | 0.006062500 |

`evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json` puts
llama.cpp at 2,179 texts/sec against candle's 326 at batch 8 — about 6.7× faster.
Through the full Merc chain at batch 32 it is **28.6% slower per unit**, and the
gate refused the promotion for exactly that reason.

The two numbers are not the same quantity: one is raw engine throughput on a warm
in-process harness, the other is what an enrolled agent reports for a task that
also downloads its input, executes, hashes and uploads its result. That is the
whole of §20's fourth prohibition — *do not treat standalone benchmarks as product
proof* — stated in numbers rather than as advice, and it is the chain measurement a
promotion has to be argued from.

### Two holes found by reviewing the cost model against itself

Both were in the first version of the scorer, and both would have promoted the
wrong cell:

1. **Price without latency.** The gate compared verified-outcome cost and stopped,
   so a cell at half the price and four times slower per unit cleared it — for
   realtime traffic, where latency is what the buyer is paying for. The ratio is
   now always computed and always reported; it is a refusal only where latency is
   the product, because batch work has a deadline that absorbs a slower cell and
   refusing there would decline a real saving for nothing.
2. **Blind to a crashing cell.** The sample counted completed tasks, so a cell
   that failed a third of what it claimed looked exactly as clean as one that
   failed none. Terminal outcomes are now read over every primary task that
   reached `complete` or `failed` on the cell, the cost divides by the delivery
   rate as well as the verification pass rate, and the gate refuses a challenger
   with any outright failure.

### Steps that did not move, and why

* **16, 17, 18, 22** — not attempted. Each is a multi-day build: a rendering IR with
  asset digests and frame decomposition, a packaged agent installer with limits and
  schedule, a project compiler with detectors and a sandboxed probe, and the full
  loop that depends on all three. *(Steps 3, 4 and 12 were in this list and have
  since moved — see the second pass above.)*
* **5, 6** — already `PRODUCTION_WIRED` at entry. The provider half of 6 is
  `P1-STRIPE-TEST`, and the Stripe **test** credential in `.env` is live and
  working (`GET /v1/balance` → `livemode:false`, CAD bucket). The matrix is still
  blocked: `scripts/stripe-sandbox.sh check` reports
  `staging_hostname_valid: false`, because Stripe has to reach a public webhook
  endpoint and there is no staging host. That is `P1-STAGING`, not a payments gap.
* **8** — the historical `REAL_RUNTIME_PROVEN` chains stand but predate the
  candidate. A candidate-bound stranger run needs the canary rehearsal driver.
* **15** — **the stored RunPod credential returns HTTP 401.** Everything up to the
  paid experiment can be built; the experiment itself cannot run until the key is
  replaced.
* **20** — `EXTERNALLY_BLOCKED` by construction: 7 of the 8 open P1 gates need a
  staging host, an offsite provider, a real paging receiver, two external Metal
  devices, a non-author reviewer, or eight named governance approvals.

## Third pass — the credential arrived

| # | Step | Status | What happened |
| -: | ---- | ------ | ------------- |
| 15 | Governed vLLM CUDA cell | harness `ECONOMICALLY_PROVEN`, Merc chain still `ABSENT` | A live RunPod key was supplied. **First governed paid experiment ran end to end**: RTX A5000 on SECURE, pinned `vllm/vllm-openai:v0.26.0-cu129`, vLLM served, teardown **verified**, zero orphan pods, receipt **admissible** — 105s against an 18,000s budget from a $1.00 cap at $0.16/hr, **$0.01 spent**. `evidence/runpod/spend-rr7b6uwmivaolh.json`. The chain did not run: no quote, selector decision, verification, charge, payable or receipt went through the pod, and `vllm_cuda` r3 stays `DRAFT` |
| 6 | Stripe provider matrix | still `OPEN`, and now for two *named* reasons | A public HTTPS control plane was stood up and reached from the internet at `status: ready, payment_mode: test, live_value_movement: false` — the exact `/readyz` gate the staging plan names — through a `cloudflared` quick tunnel, then torn down. So webhook delivery is reachable in principle. What blocks the matrix is below |

### What the Stripe matrix actually needs now

1. **Both webhook endpoints must be recreated.** `we_1Txf…jyO3LpJ` and
   `we_1Txf…fW72ynZ` carry `api_version: null`, and the contract fails closed on a
   null or mismatched endpoint version because **Stripe cannot update that field in
   place**. They also still point at `exams-payday-sol-outline.trycloudflare.com`,
   a tunnel from the concurrent session that no longer resolves.
2. **`MERC_CONNECT_CLIENT_ID` (`ca_…`) is a dashboard value**, not an API one, so it
   cannot be fetched autonomously.

A finding on the second: the parent readiness gate requires that client id, and
`scripts/stripe-sandbox-scenarios.sh` — the thing that actually produces provider
evidence — **never reads it**. The gate is stricter than what the scenarios consume.
That is worth fixing at the gate, deliberately, by someone who decides what the
Connect scenarios ought to exercise. It is **not** worth working around, and the
shared test account's webhook configuration was left alone rather than recreated
under a second agent that is live on the same account.

## Step 22 — the loop reaches admission and stops there

`control/first_complete_loop_test.go` drives the whole loop through the **public
API**, which no existing chain proof does: every one of them submits a
test-constructed job row through `store.SubmitJobTx` and so skips signup, the API
key, admission, the quote, the compute plan and the pricing decision.

It gets a stranger signed up, funded, holding an API key, and all the way to the
pricing authority. Admission then refuses:

```text
modeled supplier gross 0.102978 USD/hr is below the admission ceiling
0.104733 USD/hr, so a worker admitted at that ceiling could not earn it
```

### The diagnosis

Two derivations of the same rate, in two different domains:

| | derivation |
| --- | --- |
| **ceiling** | `unitsPerSec × 3600/1000 × referencePricePer1K × supplierShare × tier` — continuous dollars, straight from the catalogue |
| **gross** | `(primarySupplier + verification) / fxRate / expectedSeconds × 3600` — the **frozen** per-task payout, which has already been through `roundEconomicUSD` and the minimum-billable floor, divided back out |

The frozen payout is stored at micro-USD granularity. Rounding a small payout loses
up to one micro per task, and on a three-record embed that is the 1.7%. So the
ceiling is an ideal computed in continuous dollars and the gross is a realized
amount computed from rounded micro-USD — and **admission refuses every job small
enough for the rounding to matter.**

This is the **fourth** instance of the failure class `SHIPPABILITY_STATUS.md`
already records three of: the LoRA compute floor truncating to zero, the supplier
share collapsing to 0.8% on a three-row job, and the supplier payout rounding to
exactly zero between 5 and 99 units. Same cause every time — a fixed granularity
dominating a small quote — arriving through arithmetic rather than through policy.

### Why it was not fixed here

Not by loosening the check: admitting work at a ceiling the modeled throughput
cannot earn is the thing the check exists to prevent, and the check is behaving
correctly. And not by widening the tolerance: re-expressing one micro-USD as an
hourly rate over sub-second work gives **$1.44/hr** against a $0.10/hr ceiling, so
any tolerance large enough to pass would swallow the entire comparison.

The fix is structural — compare in the domain where the rounding lives, micro-USD
per task, rather than in USD/hr — and it touches admission pricing for every job.
That is not a change to make at the end of a session on a remaining context budget.

What did land: the refusal used to be six conditions collapsed into one sentence
with no numbers, so a 409 could not tell you whether the throughput was missing, the
ceiling was zero, or the gross had fallen under it. It now reports every failing
condition with its figures, and that is what made the diagnosis above possible at
all.

### The six deployment inputs the stranger path needs

Driving it surfaced these, none of which was written down anywhere. A deployment
missing any one of them returns 503 or 409 to a stranger:

1. `MERC_SANDBOX_CREDIT_USD` — defaults to **zero**, so a stranger signs up unfunded.
2. `MERC_ECON_SCHEDULE_VERSION`, `MERC_PROCESSOR_PERCENT_BPS`,
   `MERC_PROCESSOR_FIXED_USD`, `MERC_CONTROL_PLANE_PER_TASK_USD`,
   `MERC_TARGET_MARGIN_BPS` — all required, no defaults.
3. `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` + `MERC_PRICE_FX_REVISION` — the
   USD-reference-to-CAD operator input.
4. A published catalogue price schedule — only `main()`'s boot publishes it.
5. A seeded verification honeypot — Merc refuses work it cannot verify.
6. `MERC_CANARY_MODE=false` with a decision reference — one decision gates both
   signup and the payout ceiling.

## The exact economic authority

`control/money_nanos.go` lands the unit the fourth defect made unavoidable:
integer nano-major-units bound to a currency, overflow-safe `big.Int`
intermediates, no `float64` in the arithmetic. Eleven tests, all green, including
the real 0.102978/0.104733 case reproduced as arithmetic.

Rounding directions are opposite **by design**: a supplier floor rounds UP (a
positive-but-tiny entitlement must never become zero), a buyer charge rounds DOWN
(rounding up charges for work not delivered). Rates, durations, units and per-task
amounts are separate types, because the defect compared a USD/hour against a
USD/task and `float64` permitted it. `RemainderCarry` extends the sub-cent fix one
layer down — 10,000 accruals of 17 nanos post 170 micros and lose nothing, where
micro-only arithmetic rounded every one to zero.

There is **no function** that reconstructs an hourly rate from a rounded task
payout. That is the bug; it is not offered.

### Why admission is not switched over in the same change

The check needs the frozen supplier entitlement to be **exact**, and today
`BuildEconomicPlan` freezes it through `roundEconomicUSD` at micro-USD. Comparing
an exactly-derived floor against a micro-quantised frozen amount still fails by up
to one micro — the same defect wearing a different hat. Comparing both *rounded*
would pass and leave the supplier ~1.7% short, which is the fix the directive
correctly forbids.

So the next change is the additive one: nano fields on the frozen plan, computed
exactly, dual-written beside the existing micro projections, with legacy rows
keeping their own policy revision. Then admission compares
`supplier_gross_nanos >= supplier_required_nanos` per task and the reverse-hourly
path is deleted once no caller needs it.

That is money-path surgery across `BuildEconomicPlan` and its test suite, and it
was not started on a thin context budget. The authority it depends on is in place
and proven.

## Three layers, each revealed by fixing the one above it

The exact authority did not "fix admission". It removed the fog, and under the fog
were two more defects. Each number below is measured, from the public API path.

| layer | reported | what it actually was |
| --- | --- | --- |
| 1 | `0.102978` vs `0.104733` USD/hr, a **1.676%** gap | one lost micro-USD, amplified by dividing a rounded payout by a sub-second duration |
| 2 | `970` vs `2,035` nanos/task, a **2.1×** gap | the floor derived from the compute plan's modeled duration, the ceiling from the catalogue's governed throughput — **two authorities for one quantity** |
| 3 | `970` vs `29,093` nanos/task, a **30×** gap | the compute plan quotes three records at `$0.000001` while the catalogue prices the same three units at `$0.0000291` before the share |

Layers 1 and 2 are fixed. Layer 3 is not, and it is not a precision defect at all:

> The buyer is quoted roughly **thirty times below** the catalogue price the
> supplier was admitted against, so no share of that quote can meet the supplier's
> rate. Quote and settlement do not reconcile.

That is a pricing-authority disagreement — `merc.md` §8's "quote and settlement
reconcile" — and it was invisible while everything was rounded to micro-USD,
because at that granularity all three of these amounts collapse to roughly the
same tiny number. Admission was refusing for the wrong stated reason the whole
time.

Three layers deep in money code is where to stop and report the figure rather than
guess at a fourth change.

## A hazard worth naming

**Another Claude Code session was editing this same working tree during the second
pass.** `scripts/merc-credentials.sh` gained a `--runpod` flag at 19:54 and
`evidence/canary/private-canary.json` was rewritten at 19:57, neither by this
session. Those changes are deliberately left uncommitted and untouched here, and
no commit in this session's range contains either file.

It also explains an earlier puzzle: a background test suite that appeared to
restart itself twice. Two agents in one tree race on the same test databases, the
same `go build` cache and the same tracked evidence files, and the failure mode is
quiet — a suite that reports someone else's half-applied edit. Worktrees, not one
shared tree.

## Findings the entry pass turned up

1. **`control/schema.sql` could not migrate a database that had ever served
   work.** `runtime_profile_models.lifecycle` was added with `DEFAULT 'DRAFT'`
   and a derived-routability constraint was then applied in the same
   transaction, so any database with a routable cell aborted the migration.
   Fixed by inheriting the parent profile's authority for exactly those rows.
2. **A mutation-testing run from an earlier session was still live**, holding
   `if false` mutants in `control/compute_plan.go`, `control/store_jobs.go`,
   `control/exact_reuse_batch.go` and `control/realtime_placement.go`, plus four
   orphaned `merc-agent` processes from tests that had been killed. Stopped and
   the tree restored.
3. **`TestFailureMatrixAgentDeathAfterClaim` was a timing flake on a fast host.**
   It polls for `running`, kills the agent, then asserts the task returned to the
   queue. The embed cell measures ~1,950 embeddings/sec here, so a three-record
   task can be claimed, executed, uploaded and committed inside one 100ms poll
   interval — and the assertion then fired against a task that legitimately
   finished. The existing skip guard closed the window up to the last poll but not
   the window between that poll and the signal landing. It now re-checks after the
   kill and skips with the same stated reason, which is already an allowed skip.
4. **`make ci` was already failing on an unreviewed route.**
   `GET /v1/worker/viability` had been registered through `authWorker` since
   before the programme baseline, and `ops/authorization-matrix.json` did not list
   it, so `scripts/validate-authorization-matrix.py` failed on a coverage
   mismatch. Added as `worker_owned` with the pinned surface size moved 84 → 85.
5. **Three suite failures were harness artefacts, not defects.** Exporting
   `STRIPE_SECRET_KEY` into `go test` trips a deliberate hardening panic;
   `payoutAgainst` compared a stub secret against the ambient real one (fixed in
   the test helper); and the shared `cx` database had 51 still-claimable tasks
   from earlier runs, so the currency-fence test claimed a leftover job rather
   than proving the fence broken. Tests now run against a dedicated database.
## The pricing authority, resolved: one derivation, three defects under it

The programme ledger's last entry reported layer 3 as a pricing disagreement —
`970` nanos quoted against `29,093` required, a 30x gap between the compute plan
and the catalogue — and stopped there rather than guessing at a fourth change.

It was not one disagreement. It was three defects stacked, and only the smallest
of them was about pricing policy at all. Each number below is measured from the
public API path on the three-record embed fixture.

### D1 — a sub-second task rounded up to a whole second

`RequiredTaskNanosFromThroughput` computed the task duration in two steps:

```go
secondsNanos, _ := mulDiv(int64(units), NanosPerMajorUnit, int64(throughput), true)
durationNanos, _ := mulDiv(secondsNanos, NanosPerMajorUnit, 1, true)
```

The first step divides, and `mulDiv` returns an **int64**. So the intermediate is
an integer count of seconds, and 59 units at 1,666 units/sec — 35 milliseconds —
rounds up to `1`. The second step then scales that 1 to a full second.

Every task shorter than a second produced the same floor: the entire hourly
ceiling over 3,600, independent of how little work the task held. That is the
29,093. The true figure is 1,031. **The 30x gap was an integer division**, and it
had nothing to do with the catalogue.

Fixed by forming the whole product in one `mulDiv`, whose intermediate is
`big.Int`, so `units x 1e9 x 1e9 / throughput` never passes through an int64.

### D2 — the floor and the entitlement in different currencies

The floor was derived from `catalogue.ReferencePricePer1K` — USD, the market board
— and then labelled with the settlement currency and compared against an
entitlement denominated in CAD. Both sides were `float64` named USD, so nothing in
the type system could object.

Under USD settlement the FX rate is 1.0 and the defect is invisible. Under the
CAD settlement this programme mandates it is a flat 1.37x error in the supplier's
disfavour. The floor now comes from `SettlementPricePer1K`, and the stranger
admission proof runs in **both** currencies for exactly this reason — the mandated
money mode has to be exercised, not assumed.

### D3 — the buyer's gross quantised before the supplier's share

`WorkUnits` was an integer, so 58.25 settlement units had to be ceiled to 59
before a floor could be derived, and `BaseComputeUSD` was a micro-USD float, so a
gross of 1,436 nanos froze as 1,000. The supplier's 97% was then taken of the
smaller number: 970 nanos against a 1,393 nano floor.

`NanoWorkUnits` carries units at 1e9 so a fraction of a unit stays exact, and
`EconomicPlanInput.BaseComputeNanos` carries the unrounded catalogue gross beside
the micro-USD projection rather than instead of it.

### What replaced all three

One derivation, in `exactTaskEconomics`:

```text
catalogue settlement unit price
  x exact fractional units in the task
  -> exact buyer gross          (rounds DOWN, the buyer's direction)
  x explicit supplier share
  -> exact supplier entitlement AND the floor it must clear (rounds UP)
```

The throughput and the modeled duration are **gone from the money path**. They
cancel:

```text
ceiling  = unitsPerSec x 3600/1000 x price x share
seconds  = units / unitsPerSec
required = ceiling x seconds / 3600 = units/1000 x price x share
```

Carrying them through the arithmetic anyway is what let D1 exist, and it also made
a frozen money figure depend on a dated benchmark that can be revalidated out from
under an already-accepted receipt.

Because the floor and the entitlement are now the same expression evaluated once,
admission is an **identity**, not a tolerance. The measured headroom is exactly
zero in both currencies — not "within epsilon", zero.

## The loop closed

`TestFirstCompleteLoopThroughThePublicAPI` is no longer gated, and it runs green
end to end on a host with a built agent and object storage:

```text
LOOP CLOSED: buyer 11564 micros = supplier 2 + Merc 11562,
  on candle_metal/candle-metal-minilm-embed (apple_silicon_ultra),
  mode POOL, basis LIFECYCLE_LADDER, verification pass
```

A stranger signed up, was funded, received an API key, submitted a three-record
project, Merc admitted and priced it, a real `merc-agent` process claimed it,
executed it on Candle/Metal, the result verified against a seeded honeypot, the
buyer was charged inside the ceiling they accepted, the supplier was credited once
per executed task, and money conserved exactly. The receipt is
`evidence/canary/first-complete-loop.json`.

Two things had to be fixed before it would close, and neither was the pricing
authority.

### The job settled nowhere, because nothing was running the sweeps

The loop reached `status=verifying` and stayed there until the deadline. The cause
is not a race and not a defect in production: finalization is attempted **inline on
the last task commit**, and two tasks committing at once contend for the
verification process capacity, so one returns `202 Pending`. The other then calls
`finalizeJobIfDone`, finds not-all-tasks-done, and returns. Nothing inline ever
comes back for the pending one.

What comes back for it is `Workers.Run` — `verification-recovery` on a 2s tick and
`job-finalize` on a 20s tick. `main()` starts those under leader election; a
`httptest` server does not. So the driver was proving a deployment while running
half of one. It now starts the same workers with the same `stubPayout{}` default
`main()` uses.

### "The supplier is owed exactly once" was the wrong invariant

The assertion failed on a correctly-settled loop. Merc buys verification by
**executing extra tasks**, so a three-record embed with one honeypot is two
executions, and the supplier who performed both is owed for both — which is
exactly what the pricing decision quoted the buyer for
(`SupplierPayoutPerTaskUSD x (primary + redundancy + honeypot)`).

The invariant that matters is that no task is paid twice. That is what is asserted
now: one credit row per credited task, one credited task per completed task, and
exactly one supplier in the credited set — compared as a set, because a
single-row scan would have passed while a second, wrong supplier was also paid.

### One honest number to sit with

The supplier earned **2 micros of a 11,564 micro charge**. That is not a defect
and not a rounding failure — it is the shape of a three-record job under this
schedule. `MERC_CONTROL_PLANE_PER_TASK_USD` is $0.005 and there are two tasks, so
$0.010 of the charge is control-plane cost Merc actually incurs, and the target
margin is taken on top. The physical compute genuinely is worth about a
micro-USD.

It is still worth naming, because a 99.98% platform share is not a defensible
market position, and nothing in `§18 Competitive proof` can be claimed from a job
whose price is entirely fixed cost. Small jobs need either amortised control-plane
cost (batching, coalescing) or a minimum job size the buyer sees before they
submit. That is Phase 5 and Phase 7 work, and it now has a measured number
attached to it rather than an intuition.
