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

### Steps that did not move, and why

* **3, 4, 12, 16, 17, 18, 22** — not attempted this session. 12 needs the 128-caller
  chain driven through the ledger and 128 receipts; the arithmetic and the leader
  election are already tested, the end-to-end settlement is not. 16, 17, 18 and 22
  are each multi-day builds.
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
4. **Three suite failures were harness artefacts, not defects.** Exporting
   `STRIPE_SECRET_KEY` into `go test` trips a deliberate hardening panic;
   `payoutAgainst` compared a stub secret against the ambient real one (fixed in
   the test helper); and the shared `cx` database had 51 still-claimable tasks
   from earlier runs, so the currency-fence test claimed a leftover job rather
   than proving the fence broken. Tests now run against a dedicated database.
