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
