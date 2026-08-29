# Programme

Merged programme surface (PLAN_300K L6).

> **Historical record — selector economics withdrawn 2026-08-09.** The cohort
> and derived selector/economics/shadow receipts named below used a per-1,000-unit
> catalogue price as though it were per-unit, overstating supplier liability by
> 1,000×, and labelled that incomplete supplier-liability proxy as complete
> cost. They are retained with `validity: WITHDRAWN` as diagnostics only; none
> authorizes a cost winner, regret, promotion, gross-margin, or true-net claim.
> Current code may report a separately named equal-liability throughput
> diagnostic under a narrow contract/epoch scope, but `cell-promotion-gate-v4`
> cannot authorize it: no durable incumbent/challenger execution pair binds both
> cells to the same input/cohort digest.

> **Current admission truth — historical positive loops are not production
> authority.** `TestFirstCompleteLoopThroughThePublicAPI` now exercises mechanics
> only by installing explicit `TEST_ONLY` performance and catalogue-publication
> authorities; it cannot write candidate evidence or establish a sellable lane.
> The former positive CAD Project Compiler admission/execution test was removed.
> Its current public tests prove the opposite boundary: embed compilation reaches
> the quote handler, then returns 503 with zero quote, project, job, task, or
> ledger writes because no production `BOUND` performance authority has a
> `Unit`/`UnitScope` compatible with settlement. Fresh production catalogue
> publication independently refuses the checked-in throughput/power evidence.


<!-- source: docs/MASTER_PROGRAMME_LEDGER.md -->

> **Current readiness (re-derived 2026-08-17, HEAD `9e31c65b`).**
> `python3 scripts/validate-readiness.py`: Level B **87/100** (threshold 95,
> P0=0, P1=5, `NO_GO`); backend alpha **85/91**,
> `ALPHA_ENGINEERING_READY NO_GO`, `EXTERNAL_ALPHA_PROVEN NO_GO`. Open P1s:
> `P1-STRIPE-TEST` (`ALPHA_BLOCKER`), `P1-CANARY-REHEARSAL` (`ALPHA_CONTROL`),
> `P1-ALERT-DELIVERY` / `P1-INDEPENDENT-APPROVAL` / `P1-GOVERNANCE`
> (`PUBLIC_LAUNCH`). Satisfied on evidence: `P1-STAGING`, `P1-RECOVERY-SOAK`
> (3600 s alpha soak; 24 h still unearned), `P1-OFFSITE-RESTORE`.
> `live_money_or_public_launch` is `NO_GO_PROHIBITED`. Live
> `https://mercmerc.net/readyz` is HTTP 200, `payment_mode=test`,
> `live_value_movement=false`. Session tables below that still say eight P1s,
> 84/100, no staging host, `/readyz` 503, or `/version` 404 are
> **historical** unless restated here.

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

Read from `evidence/state/branch-state-step1.json` (unbound session-state
inventory; HEAD `58384221`, live database projection), `ops/go-no-go.json`, and
the tree. Not from prose.

| # | Step | Entry status | Evidence read |
| -: | ---- | ------ | -------- |
| 1 | Generate current HEAD/state receipt | `DONE` | `evidence/state/branch-state-step1.json` (unbound session inventory), HEAD-bound, live DB projection |
| 2 | Skip every item already proven by that receipt | `DONE` | this table |
| 3 | Boot the production image from a clean clone | `CURRENTLY_REFUSED_BY_PRICING_AUTHORITY` | The boot harness remains wired in `make ci`, but the stronger catalogue gate now refuses the shipped UNBOUND throughput receipt and ASSUMED watts. A clean production image cannot mint a fresh price schedule until BOUND throughput and exact MEASURED power authority exist. |
| 4 | Canary-decision packaging and readiness | `PRODUCTION_WIRED` | `control/canary_decision.go`, `control/canary_policy.go` + tests |
| 5 | Buyer top-up and governed refund routes | `PRODUCTION_WIRED` | `POST /v1/billing/{setup,status,topup}`; refunds only behind `/admin/…` authority |
| 6 | Prepaid and realtime charge-to-payable | `PRODUCTION_WIRED` locally, provider matrix `OPEN` | `control/prepaid.go`, realtime settlement; `P1-STRIPE-TEST` open |
| 7 | Supplier throughput from benchmark authority | needs probe | `control/benchmark.go`, `runtime_profiles.benchmark_authority` |
| 8 | The complete stranger transaction | `REAL_RUNTIME_PROVEN` historical, not candidate-bound | `evidence/canary/real-runtime-embed.json`, `real-runtime-realtime.json` |
| 9 | RuntimeSelector shadow mode | `PRODUCTION_WIRED`, eligibility only | `control/runtime_shadow_selection.go`, policy `eligibility-only-v1`. No cost or latency scoring, no outcome table |
| 10 | Paired cohorts and regret | `ABSENT` through the chain | `embed-cell-candle-vs-llama-cpp-r1.json` is a bench harness, not a Merc-chain cohort; nothing computes regret |
| 11 | Narrow selector promotion authority | `ABSENT` | no promotion receipt, no rollback target |
| 12 | 128-request coalescing through money | `PRODUCTION_WIRED`; bound money-path proofs against a double upstream | `control/inflight_coalescing.go`, caller `control/realtime.go`; `evidence/reuse/public-path-coalescing-128-to-1.json` (commit `4ef1922a`) |
| 13 | Tokenization and tool-schema caches | `PARTIAL` → identity **DONE**; tokenization **DOES_NOT_APPLY** | prepared request identity cache is production-wired and micro-measured; control-plane tokenization cache does not apply (no model tokenizer on the control plane). Bound audit: `evidence/perf/five-cache-architecture-audit.json` |
| 14 | Token-budget batching by traffic class | `PARTIAL` | `control/batch_policy.go` declares `INTERACTIVE` and `BATCH` only |
| 15 | Governed vLLM CUDA cell through RunPod | direct runtime `REAL_RUNTIME_PROVEN`, Merc chain `ABSENT` | `evidence/runpod/cuda-first-proof.json` (unbound provider proof; not Merc-chain canary); `vllm_cuda` r3 cell is `DRAFT`. **The stored RunPod key returns HTTP 401** |
| 16 | Rendering IR and distributed render proof | `ABSENT` | no render adapter, no rendering IR, no Blender path in the tree |
| 17 | Merc Agent one-click experience | `PARTIAL` | `agent/` builds and benchmarks, `clients/macapp/` exists; limit/schedule surface unverified |
| 18 | Workload IR and project detectors | `ABSENT` | `WorkloadDecision` is an admission artefact, not a project graph |
| 19 | Pool, replica and local-cluster modes | `ABSENT` as declared modes | placement authority exists; no mode taxonomy, no refusal rule |
| 20 | Level-B security and operational closure | `EXTERNALLY_BLOCKED` | `ops/go-no-go.json` at this HEAD: 0 P0, 5 open P1. Only `P1-STRIPE-TEST` is an `ALPHA_BLOCKER`. `P1-STAGING`, `P1-RECOVERY-SOAK` (alpha exit) and `P1-OFFSITE-RESTORE` are `SATISFIED`. Historical session tables that said 8 open P1s are superseded. |
| 21 | Audit against §20 "what Merc must not do" | pending | — |
| 22 | Prove the §21 first complete loop | `TEST_ONLY_MECHANICS`; production physical admission `REFUSED` | `TestFirstCompleteLoopThroughThePublicAPI` uses explicit synthetic performance/publication authority and writes no candidate evidence. Historical real-runtime receipts remain non-current. |

## Session preconditions

* `EXTERNAL_REVIEW_STATUS=DEFERRED` — no delegated review this session; every
  adversarial review is labelled `NOT_EXTERNALLY_INDEPENDENT`.
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
| 1 | HEAD/state receipt | `DONE` | `evidence/state/branch-state-step1.json` (unbound session inventory) with a working live-database projection. Getting there required fixing `control/schema.sql`, which could not migrate any database that had ever served work |
| 2 | Skip proven work | `DONE` | the entry table above |
| 7 | Supplier throughput from benchmark authority | `PARTIAL` | `control/repricing_benchmark_authority_test.go` binds every `repricingBenchmarks` constant to the receipt it cites, and enforces a CONSERVATIVE bound: a constant above the measurement fails (it would price undemonstrated supply), one more than 1% below fails (it overcharges the buyer). Found the `batch_infer` constant at 138.7 against a measured 138.71389521 — conservative, so it stands. Still not the per-cell, per-hardware derivation the step asks for |
| 9 | RuntimeSelector shadow mode | `DONE` | the scoring half arrived. `control/runtime_cell_cost.go` reads the eventual accepted supplier payout plus separate reliability evidence out of the money path — every committed task already carries its cell, accepted outputs, duration, frozen payout, retries and verification outcome. Retries and failed attempts are unpaid and fail closed rather than being capitalized into liability. Policy is now `eligibility-and-measured-supplier-liability-v3` and every row records which arm decided it |
| 10 | Paired cohorts and regret | `HISTORICAL_ECONOMICS_WITHDRAWN` | The old `SelectorRegretForScope` result used a 1,000×-wrong unit conversion and an incomplete cost vocabulary. The retained latency rows are diagnostics only. A future liability-regret authority requires a durable incumbent/challenger execution pair bound to one shared input/cohort digest; the current independently aggregated jobs do not provide it. |
| 11 | Selector promotion authority | `NON_AUTHORIZING_V4_REFUSAL_GATE` | `cell-promotion-gate-v4` refuses every promotion because the durable matched-execution authority above does not exist. Shadow consideration plus independently aggregated jobs is not exact-pair evidence. Unequal supplier liability also cannot authorize a total-cost promotion while platform costs remain unknown. |
| 13 | Tokenization and tool-schema caches | `PARTIAL` closed honestly: identity **DONE**; tokenization **DOES_NOT_APPLY** | `control/realtime_identity_cache.go` is production-wired (tenant/profile/policy-scoped). Host microbench (M3 Ultra): hit ~0.4µs, miss ~12µs. Control-plane tokenization does not exist — admission/pricing use byte heuristics; settlement tokens come from the engine. Agent embed already has `token_cache.rs`. Bound audit: `evidence/perf/five-cache-architecture-audit.json`. Do not build an empty tokenizer cache to tick the box |
| 14 | Batching by traffic class | `DONE, declared unwired` | `control/traffic_class.go`: the four classes with promoted policy, and `ValidateTrafficClassPolicies` proving the ordering invariants — every lower class declares it never delays every higher one, only BACKGROUND is preemptible, INTERACTIVE stays below the throughput knee. Budgets come from the two MEASURED points on the existing curve; classes sharing one say so rather than interpolating a third. Registered in `knownUnwired` because no production path asks for the class yet |
| 19 | Execution fabric modes | `DONE, declared unwired` | `control/execution_mode.go`: POOL, REPLICA_SERVICE, LOCAL_CLUSTER, CLOUD_BACKSTOP, each placement carrying its reason and every refused mode carrying its own. Tightly coupled work is refused on a WAN fabric and on an unmeasured one; unknown parallelism fails closed to tight, so a new upstream mode cannot be silently placed on the public internet |
| 21 | §20 must-not-do audit | `DONE` | `scripts/must-not-do-audit.py` → `evidence/state/must-not-do-audit.json` (unbound audit inventory). **12 PASS, 1 FAIL, 1 not mechanically checkable.** The failure is prohibition 10: `control/payment.go` has one process-wide `platformTakeRate`, so one percentage applies to embeddings, generation, rendering and every future lane alike, and the published schedule carries a single supplier share of 0.97 |

## Second pass

| # | Step | Status | What changed |
| -: | ---- | ------ | ------------ |
| 3 | Boot the production image | historical boot proof retained; current candidate `REFUSED` | The earlier tree served every probe with no host files, recorded by `evidence/state/release-image-boot.json` (an UNBOUND inventory, not release authority). That result does not carry forward: current startup rejects the shipped UNBOUND throughput receipt and ASSUMED watts before publication. The image-boot gate must remain red until a BOUND benchmark plus exact MEASURED power row can authorize a fresh catalogue schedule. |
| 4 | Canary packaging and readiness | `DONE`, already tested at entry | **Historical local observation (unconfigured process):** `/readyz` returned 503 with `reason_code: canary_policy_unconfigured` — a direct configuration error, not an unexplained buyer-facing 403 — and it reads BOTH the boot copy and a live re-read. `TestReadyzGoesRedWhenTheDisableDecisionStopsResolving` and `TestReadyzNamesCanaryMisconfigurationAndProbesStayReachable`. **Current live staging (2026-08-17):** `https://mercmerc.net/readyz` is HTTP 200, `status=ready`, `payment_mode=test`, `live_value_movement=false`. |
| 10 | Paired cohorts and regret | `WITHDRAWN AS ECONOMIC AUTHORITY` | The harness drove 40 executions, but its mixed receipt used the wrong price unit and pooled an insufficient scope. `evidence/perf/selector/paired-cohort-embed.json` and the derived cell-economics, economic-selector, and governed-shadow receipts are retained as `WITHDRAWN`; their latency rows are historical diagnostics, not matched-pair, economic, or promotion proof. |
| 12 | 128-request coalescing | bound money-path proofs (double upstream) | Bound proofs at commit `4ef1922a` (`evidence/reuse/public-path-128-to-1.json`, `evidence/reuse/public-path-coalescing-128-to-1.json`) show 128 deliveries to 1 upstream call through the real public handler against an HTTP upstream double. They prove control-plane reuse/coalescing money and receipt paths; they do not measure GPU performance. Store-level follower money/isolation remains in `control/coalesced_cluster_money_test.go` |
| 15 | Governed vLLM CUDA cell | harness ready, credential still dead | `scripts/runpod-spend-guard.py` converts a dollar cap into a pod lifetime with a self-test, holds back 20% for teardown delay, rounds spend up, and marks a receipt **inadmissible** on unverified teardown, an overrun lifetime, a floating image tag or a leftover pod. `runpod-vllm.sh experiment` wires that around the existing provisioner and refuses to start while anything else is billing. The self-test runs in `make ci` |
| 19 | Execution fabric modes | `PRODUCTION_WIRED` | `createJob` now asks `ChooseExecutionMode` and stores the mode plus the full explanation, refusals included, beside the shadow selection. Batch work reaches POOL by construction, which is the reason to record it: once a second mode is reachable, "by construction" and "by decision" are indistinguishable afterwards. A tightly coupled degree-4 workload records **no** mode rather than being defaulted onto the public internet, and the database refuses a mode with no reason |

### Current Step 4 activation and performance boundary

`cell-promotion-gate-v4` is deliberately non-authorizing. The observation model
stores a shadow decision naming cells that were considered and separately
aggregates jobs completed on each cell; it does not persist a matched
incumbent/challenger execution pair or a shared input/cohort digest. Therefore
the gate always refuses, even when the aggregate liability and throughput
figures would otherwise satisfy a margin.

A narrow comparison receipt cannot change a cell-global lifecycle. Scoped
receipts are rejected as authority for global `CANARY` or `ACTIVE` writes.
`CANARY` remains directed-only: its allowlist and traffic-percentage fields are
stored, but no ordinary-routing admission path consumes them, so they cannot be
used to expose unrestricted buyer traffic.

Production also has zero `BOUND` performance lane whose `Unit` and `UnitScope`
match the corresponding settlement authority. Batch measures
`tokens/decode_output_tokens` while settlement uses
`tokens/token_like_input_plus_max_output_tokens`; embeddings measure
`embeddings/completed_embedding_records` while settlement uses
`token_like_input_units/token_like_input_geometry`. New production admission
therefore refuses rather than converting
incommensurate units. Previously accepted jobs remain readable and replayable
from their frozen performance snapshots; historical replay does not consult or
reinterpret today's performance authority.

### Historical cohort diagnostics (withdrawn as authority)

The original paragraph claimed **0.194 USD per unit** and zero complete-cost
regret. That economic interpretation is withdrawn: the fixture applied a price
quoted per 1,000 units as though it were per unit, and the scorer omitted
platform costs. The later records-only “correction” to **0.0000060625 USD per
output** was also not canonical: text settlement units are token-like
`max(records, raw_input_bytes/4)`, not output-record count. The repaired
test harness freezes its actual 2,380-byte/32-output corpus as 595 settlement
units; at its explicitly synthetic historical $0.00625/1K fixture rate that is
$0.003607/task and $0.00011271875/output. Those are regression-test figures,
not a reinterpretation or revalidation of the withdrawn physical receipt.

> Supplier payout is frozen per accepted job from catalogue price × exact
> settlement-input geometry × supplier share. Rejected verification attempts,
> terminal failures and retries are unpaid reliability evidence: they make a row
> ineligible for comparison; they do not multiply ledger liability. Two cells
> may be compared only under matched frozen geometry and contract authority.
> Provider, energy, storage, egress, utilization, model-load and refund-risk
> costs remain separate unknowns, so neither an economic winner nor total-cost
> regret follows.

A historical cost-only diagnostic would have reported that as a failed cost
argument — *"saves 0.00%, below the required 10% margin"* — which is true and
useless, because no
same-price comparison can ever clear a cost margin. The diagnostic scorer names
which argument it is reporting:

| Basis | Margin | What it claims |
| --- | --- | --- |
| `SUPPLIER_LIABILITY_PROXY_ONLY_COST_REFUSED` | none | unequal liability is reported but cannot authorize a total-cost choice while platform costs are unknown |
| `MORE_THROUGHPUT_AT_EQUAL_SUPPLIER_LIABILITY` | 25% | the same verified unit produced faster at equal supplier liability — a capacity gain, **not** a saving |

The wider margin on the second is because it protects against noise in a duration
measured on a shared host rather than noise in a dollar figure.

Neither basis is promotion authority today. Gate v4 refuses both until a durable
matched execution pair with one shared input/cohort digest exists.

The same run measured 3.0000 ms per unit on both cells, identical to four decimals
— a task too small to separate two engines rather than two engines agreeing. An
unbound in-process harness had once suggested a large batch-8 gap, so the cohort
now embeds batches of 32 to give the chain a chance to separate the cells.

The historical gate also caught its own caller: the cohort's scope claimed
verification contract `cosine_similarity` where the cell sells `cosine`, and
the authority check refused the attempted promotion. That refusal is retained
as narrative, not as current promotion evidence.

### And then the batch-32 rerun contradicted the benchmark

With batches of 32 the cells finally separated — and not in the direction the
retained benchmark implies:

| cell | measured through the chain | supplier USD/unit |
| --- | --- | --- |
| `candle-metal-minilm-embed` | 0.2188 ms/output | **WITHDRAWN — no valid economic value recoverable from this receipt** |
| `llama-cpp-metal-minilm-embed` | 0.2812 ms/output | **WITHDRAWN — no valid economic value recoverable from this receipt** |

An unbound in-process harness once reported roughly 6.7× at batch 8
(`evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json`:
llama.cpp 2,179 texts/sec against candle's 326). That is not product throughput
and it is not chain cost. Through the full Merc chain at batch 32 llama.cpp was
**28.6% slower per unit**, and the historical gate refused the attempted
promotion for that reason before the underlying receipts were withdrawn.

The two numbers are not the same quantity: one is raw engine throughput on a warm
in-process harness, the other is what an enrolled agent reports for a task that
also downloads its input, executes, hashes and uploads its result. That is the
whole of §20's fourth prohibition — *do not treat standalone benchmarks as product
proof* — stated in numbers rather than as advice. A future promotion would need
a durable matched-input execution pair in addition to valid chain measurements;
this independently aggregated cohort is not that authority.

### Two holes found by reviewing the cost model against itself

Both were in the first version of the historical scorer, and both would have
promoted the wrong cell. Gate v4 now refuses independently because no durable
matched-execution pair exists:

1. **Price without latency.** The gate compared verified-outcome cost and stopped,
   so a cell at half the price and four times slower per unit cleared it — for
   realtime traffic, where latency is what the buyer is paying for. The ratio is
   now always computed and always reported; it is a refusal only where latency is
   the product, because batch work has a deadline that absorbs a slower cell and
   refusing there would decline a real saving for nothing.
2. **Blind to a crashing cell.** The sample counted completed tasks, so a cell
   that failed a third of what it claimed looked exactly as clean as one that
   failed none. Terminal outcomes are now read over every primary task that
   reached `complete` or `failed` on the cell. A verification or terminal failure
   makes the cohort ineligible; it is unpaid reliability evidence and is never
   divided into or capitalized into supplier liability. Eligible liability is
   the exact settled supplier credit for the accepted work.

### Steps that did not move, and why

* **16, 17, 18, 22** — not attempted. Each is a multi-day build: a rendering IR with
  asset digests and frame decomposition, a packaged agent installer with limits and
  schedule, a project compiler with detectors and a sandboxed probe, and the full
  loop that depends on all three. *(Steps 3, 4 and 12 were in this list and have
  since moved — see the second pass above.)*
* **5, 6** — already `PRODUCTION_WIRED` at entry. The provider half of 6 is
  `P1-STRIPE-TEST`. **Historical (this session's entry):** the matrix was
  blocked on `staging_hostname_valid: false` / `P1-STAGING`. **Current:**
  `P1-STAGING` is `SATISFIED`; public TLS webhook delivery is reachable at
  `mercmerc.net`. The remaining wall is Connect signup
  (`evidence/external/stripe-sandbox-matrix.json` status `BLOCKED`,
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
  `blocker.id=connect_platform_not_signed_up`). That is `P1-STRIPE-TEST`,
  not a missing host.
* **8** — the historical `REAL_RUNTIME_PROVEN` chains stand but predate the
  candidate. A candidate-bound stranger run needs the canary rehearsal driver.
* **15** — **the stored RunPod credential returns HTTP 401.** Everything up to the
  paid experiment can be built; the experiment itself cannot run until the key is
  replaced.
* **20** — `EXTERNALLY_BLOCKED` by construction. **Historical:** 7 of 8 open
  P1s needed a staging host, offsite provider, paging receiver, two Metal
  devices, a non-author reviewer, or eight named approvals. **Current:** 5
  open P1s. Staging, alpha recovery-soak, and offsite restore closed on
  evidence. Still external: Connect signup, the counted canary rehearsal,
  staffed paging, independent review, and qualified governance.

## Third pass — the credential arrived

| # | Step | Status | What happened |
| -: | ---- | ------ | ------------- |
| 15 | Governed vLLM CUDA cell | harness `ECONOMICALLY_PROVEN`, Merc chain still `ABSENT` | A live RunPod key was supplied. **First governed paid experiment ran end to end**: RTX A5000 on SECURE, pinned `vllm/vllm-openai:v0.26.0-cu129`, vLLM served, teardown **verified**, zero orphan pods — 105s against an 18,000s budget from a $1.00 cap at $0.16/hr, **$0.01 spent**. `evidence/runpod/spend-rr7b6uwmivaolh.json` is now **WITHDRAWN** (mutable image tag; runtime unidentifiable) and citable by nothing; the paid run happened, but the receipt backs no cost or performance claim. The chain did not run: no quote, selector decision, verification, charge, payable or receipt went through the pod, and `vllm_cuda` r3 stays `DRAFT` |
| 6 | Stripe provider matrix | still `OPEN`, Connect signup | **Historical:** a `cloudflared` quick tunnel reached `/readyz` ready / test / no live value, then was torn down. **Current:** persistent public TLS at `mercmerc.net` answers the same `/readyz` shape. What still blocks the matrix is Connect signup, below. |

### What the Stripe matrix actually needs now

> **Historical (tunnel-era).** The two `we_1Txf…` endpoints with
> `api_version: null` pointing at a dead `trycloudflare.com` host were a
> session finding. They are not the current wall.

**Current wall (2026-08-17):**
`evidence/external/stripe-sandbox-matrix.json` is `BLOCKED` /
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
`connect_platform_not_signed_up`. Stripe itself refuses:
*"You can only create new accounts if you've signed up for Connect."*
Non-Connect test-mode scenarios have been driven against
`acct_1TxbzMCwPLrR4vaY`. Persistent staging is public HTTPS at
`mercmerc.net`. `MERC_CONNECT_CLIENT_ID` (`ca_…`) remains a dashboard
value, not an API one.

Historical tunnel-era remainder, retained as the finding that was true
then:

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

## Historical Step 22 investigation — not current admission authority

The investigation below records how the old positive loop was debugged. Its
arithmetic findings remain useful, but every admission or “loop closed” statement
in this subsection is historical. Current truth is the `TEST_ONLY` mechanics seam
and production physical-authority refusal summarized after it.

The historical `control/first_complete_loop_test.go` path drove the whole loop
through the **public API**, which the earlier chain proofs did not: every one of
them submitted a test-constructed job row through `store.SubmitJobTx` and so
skipped signup, the API key, admission, the quote, the compute plan and the
pricing decision.

It got a stranger signed up, funded, holding an API key, and all the way to the
pricing authority. Admission then refused:

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
   `MERC_PROCESSOR_FIXED_USD`, `MERC_CONTROL_PLANE_PER_BATCH_USD`,
   `MERC_MIN_CONTRIBUTION_PER_BATCH_USD`,
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

That last sentence describes the **type**, and it is proven by
`money_nanos_test.go`. It is not yet a property of the running system, and the
distinction matters enough to state here rather than let a reader assume it:
**production settlement does not post through `RemainderCarry`.** Task and
realtime settlement project each leg independently with `projectNanosToMicros` /
`LedgerMicrosFromNanos` (`payment.go:135-171`, `realtime_store.go:1745-1753`), so
sub-micro remainders are rounded per post rather than carried. `RemainderCarry`
has no non-test caller.

This is precision debt, not lost or created money: the cash domain is a micro
ledger (`NUMERIC(12,6)`), and exact conservation at acceptance is enforced
separately by `FixedPoint` (`validateFixedPointPricing`). What it means is that
nobody may claim the ledger conserves exact nanos per task without residual loss
until the accrual is actually wired. `TestRemainderCarryHasNoProductionCaller`
fails the moment that changes, so this paragraph cannot quietly go stale.

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

**Another implementation session was editing this same working tree during the second
pass.** `scripts/merc-credentials.sh` gained a `--runpod` flag at 19:54 and
`evidence/canary/private-canary.json` (unbound capability inventory) was rewritten
at 19:57, neither by this session. Those changes are deliberately left uncommitted
and untouched here, and no commit in this session's range contains either file.

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
historically mandated CAD settlement it is a flat 1.37x error in the supplier's
disfavour. The floor now comes from `SettlementPricePer1K`, and the exact-money
fixture exercises **both** currencies for this reason. That is arithmetic
coverage, not current physical-admission evidence.

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
the `TEST_ONLY` exact-money fixture proves an **identity**, not a tolerance. The
arithmetic headroom is exactly zero in both currencies — not "within epsilon",
zero. Current production admission still separately requires physical authority
whose unit and unit scope match settlement.

## Historical loop; current mechanics seam

The old `TestFirstCompleteLoopThroughThePublicAPI` result remains historical:

```text
LOOP CLOSED: buyer 11564 micros = supplier 2 + Merc 11562,
  on candle_metal/candle-metal-minilm-embed (apple_silicon_ultra),
  mode POOL, basis LIFECYCLE_LADDER, verification pass
```

A stranger signed up, was funded, received an API key, submitted a three-record
project, and a real `merc-agent` completed the historical Candle/Metal path. The
receipt at `evidence/canary/first-complete-loop.json` is UNBOUND and is not
candidate or current-admission authority.

The current test keeps the buyer/agent/settlement mechanics but installs explicit
`TEST_ONLY` combined-token performance and synthetic catalogue-publication
authorities. It refuses to write candidate evidence and proves no production
lane. Checked-in batch throughput is decode-output-only while settlement is
combined input-plus-output; checked-in embed throughput is
completed-output-records while settlement is token-like input geometry. Until a
BOUND conversion or matching receipt exists, the two current CAD Project
Compiler regressions prove that public embed quoting returns 503 and writes no
quote, project, job, task, or ledger row:

- `TestProjectCompilerCADEmbedAdmissionRefusesWithoutScopeCompatibleAuthority`
- `TestProjectCompilerCADEmbedExecutionRefusesBeforeDurableMutation`

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

The retained receipt paid the supplier **2 micros of an 11,564 micro charge**.
That is the measured defect baseline, not an acceptable current policy. The
replacement schedule allocates declared account/invoice overhead across the
collector's economic charge batch, rounds the supplier's ledger projection up
from its exact nano-unit entitlement, and requires positive absolute
contribution. Receipts now distinguish the gross platform ledger row from true
net contribution; the latter remains unavailable while any named cost category
is unknown rather than being presented as profit.
attached to it rather than an intuition.


<!-- source: docs/SHIPPABILITY_STATUS.md -->

# Merc shippability status

Audited against the code on 2026-08-02. Statuses use the goal's vocabulary.
Nothing here is inferred from intent; each row was probed against the tree.

> Newer than this file for the runtime selector, batching, execution modes and
> the §20 prohibitions: `docs/PROGRAMME.md § "Master Programme Ledger"`, with machine-readable
> receipts at `evidence/state/branch-state-step1.json` and
> `evidence/state/must-not-do-audit.json` (both unbound; session-state inventory,
> not bound identity). Where the two disagree, the receipts are the authority —
> this file is prose and prose drifts.

**A `CANARY_PROVEN` receipt is capability evidence, not release authorization.**
`public_capability_allowed` remains false, Level B remains `NO_GO`, and Level C
live money/public launch remains prohibited.

## The gate that controls every money lane

The earlier CAD/USD incompatibility was removed by making settlement currency
explicit and binding it into every new quote, accepted job, and deterministic
economic plan. Quote, job, task-ledger, SLA, and charge-batch authority is
immutable; dispatch, settlement, and collection refuse obligations from a
different deployment currency. Historical canary evidence proves the CAD path,
and catalogue publication now binds the USD reference board to CAD through an
operator-supplied rate and immutable FX revision. The release candidate is
intentionally **SEALED**: it rejects live Stripe activation unless
an operator supplies the exact external activation authority. The formal
release ledger separately requires a complete Stripe **test-mode** matrix and
reconciliation receipt.

That distinction is deliberate: old proof is retained, current credentials are
not assumed, and neither a historical live key nor a local test can promote
Level B or Level C.

## Candidate-bound private-canary evidence — 0/21 lanes CANARY_PROVEN

`scripts/private-canary.py` is now a capability inventory, not a canary
authority. Its old `full_path` boolean promoted partial commands — including
unit tests and recorded-response UI tests — after separately probing that a
runtime or credential existed. That did not prove the command used the
capability, and it did not bind an immutable candidate image or exact commit.

The regenerated inventory at `evidence/canary/private-canary.json` (unbound
capability inventory, not a bound canary receipt) therefore reports zero
candidate-bound lanes and keeps `public_capability_allowed: false`. A passing
lane command is capped at `TESTED`. Two clean, tracked historical receipts
preserve `REAL_RUNTIME_PROVEN` evidence for three lane labels (batch inference,
embeddings, and realtime), but each is explicitly `candidate_bound: false`.
Fabricated, untracked, dirty, malformed, non-ancestor, incomplete-chain, and
non-conserving receipts are rejected. Only
`scripts/go-closure-canary-rehearsal.sh` may create exact-commit,
immutable-image canary authority.

That formal authority is now hardened too. Every scenario receipt is schema-v2
bound to a fresh run ID, exact commit, immutable image, invocation window, and
the operator-reviewed SHA-256 of a canonical non-writable driver whose bytes
must remain unchanged. Observation sources are closed by scenario. Merc independently
queries PostgreSQL for the two approved buyers/workers, completed workload
all-task runtime/reviewed-build/verification/money chains, cancellations, retries, stale recoveries,
stale-attempt fencing, and webhook retry delivery. Provider-only backup,
Stripe, and alert observations retain strict source-specific contracts. Offline
hostile mutations and a disposable-database corroboration suite run in CI. No
external execution is claimed.

Metal-agent restart authority is independently observable now too. Every
merc-agent process registers a fresh session UUID; PostgreSQL preserves the
session start across same-process re-registration and changes it only when a
new process registers. The formal restart storm freezes an operator-reviewed
adapter digest and exact action receipt, but derives its two-agent restart
claim only from both approved reviewed-build session transitions and requires
those sessions to remain current through the rest of the fault storm. Hostile
receipt and disposable-database tests run in CI. No external restart is claimed.

Formal soak authority is fail-closed around one uninterrupted candidate
container. Every raw sample carries the full container ID, configured immutable
image, and content-addressed image ID; any recreation, restart, or substitution
aborts the run. A separate hostile validator verifies the JSONL digest and
re-derives sample coverage, agent presence, alert/dead-letter/task health, and
all resource-growth bounds before the runner retains a schema-v2 PASS receipt.
The required external 24-hour soak remains NOT EXECUTED.

Formal rollback backup authority is now receipt-derived as well. `backup.sh`
binds the encrypted bytes, database/object scope, and exact offsite URI in a
schema-v2 manifest; independently downloads both ciphertext and manifest,
compares both hashes, and emits a closed verification receipt. The rollback
rehearsal resolves artifacts only through the exact atomic invocation result,
accepts only a fresh receipt inside its own window, and embeds both receipts in
the rollback/forward evidence instead of asserting a success boolean. No
external offsite backup or rollback run is claimed.

The supplier restart authority contract was also re-run against a disposable
PostgreSQL 16 instance and retained at
`evidence/autonomous/agent-restart-authority-r1.json` (unbound local exercise
receipt; not a bound identity proof). It verifies exact-run, candidate/driver/
worker bindings, durable process-session transitions, out-of-window and replay
refusal, and release-doctor refusal when the reviewed restart-driver digest is
absent. This remains a local authority exercise, not external staging evidence.

The separate final acceptance validator now prevents individually plausible
receipts from being spliced into false release authority. It requires one
fresh, ordered deploy → rollback/forward → restart → canary → qualifying soak
chain; the same exact commit, immutable image, and observed image ID; retained
backup ciphertext and raw soak samples; all nested scenario/action receipts;
and the canonical eight-domain governance bundle with release approval after
the completed soak. Hostile fixtures cover substitution, reordering, replay,
truncation, raw-artifact tampering, incomplete identities, duplicate keys, and
secret-shaped content. A validator PASS is explicitly limited to eligibility
for supervised Level-B private-canary review. No external chain or governance
approval is claimed, and Level C remains prohibited.

Real historical execution still matters: Apple M3 Ultra/Metal/Candle completed
batch embeddings and llama.cpp completed realtime with verification and money.
Direct RunPod/vLLM runtime evidence also remains, but its receipt does not show a
Merc request-to-settlement chain. None of those facts is promoted into current
release authorization.

The bounded scene-rendering gap is now closed as a private deterministic lane;
the remaining product gaps are full prompt-to-image generation (no governed
model runtime), LoRA trainer/evaluator dispatch, TP>1's real multi-GPU receipt,
external-model onboarding's money-chain exercise, and alert delivery's lack of
an external staffed paging receiver. A private Alertmanager→HTTP sink
fire/resolve receipt is retained, but it is not an external staffed paging
claim. The scene renderer is deliberately not
counted as prompt-to-image generation.

## Lanes

| lane | status | evidence |
|---|---|---|
| OpenAI-compatible realtime | `REAL_RUNTIME_PROVEN` historical; candidate canary `OPEN` | `[KILL-RT]` was reversed and `KEEP-RT` executed (`DECISION_ZERO_REVERSAL.md`). A real llama.cpp/Metal engine completed the full contract, verification, debit, supplier-payable, positive-margin, and receipt chain (`evidence/canary/real-runtime-realtime.json`). The retained receipt is clean and provenance-checked but predates the candidate, so it cannot mint `CANARY_PROVEN`. New physical, exact-result-cache, and coalesced-delivery contracts now freeze currency-bound fixed-point `PricingDecision` authority through settlement and receipt. Reuse records zero physical supplier liability, its explicit ledger-minimum delivery charge, and gross contribution separately from unknown processor/control/storage/egress/risk costs; it does not claim true net. Bound money-path proofs at commit `4ef1922a` (`evidence/reuse/public-path-128-to-1.json`, `evidence/reuse/public-path-coalescing-128-to-1.json`) show 128 deliveries to 1 upstream call through the real public handler against an HTTP upstream double. They prove control-plane exact-reuse and coalescing money/receipt paths; they do not measure GPU performance and are not candidate-bound canary authority. Official Python and JavaScript SDK conformance passes all seven capabilities against the attested Qwen2.5-3B real engine (`evidence/canary/sdk-conformance-real-engine.json`); the older Llama-3.2 tool-template caveat remains model-specific, not a Merc surface defect. |
| RunPod-backed pinned vLLM | direct runtime `REAL_RUNTIME_PROVEN`; Merc chain `TESTED` | Real NVIDIA hardware served a pinned `vllm/vllm-openai:v0.26.0` image and revision-pinned model with SSE (`evidence/runpod/cuda-first-proof.json`). We do not know CUDA aggregate tok/s or $/MTok at this commit; no bound receipt with digests and raw samples exists. Historical unbound figures in `evidence/perf/cuda-throughput-correction.json` and `evidence/perf/cuda-a5000-ceiling.json` must not be quoted as today's numbers. That provider receipt contains no Merc contract, verification, debit, supplier payable, or Merc contribution, so it is not end-to-end canary authority. |
| Object storage | `TESTED`; candidate canary `OPEN` | Retention, deletion and tenant isolation were exercised against a live store, but the retained inventory has no exact-candidate full-chain receipt. `control/job_object_retention.go` purges 30 days after terminal, holds while any dispute is unresolved, and refuses a period inside the 7-day filing window; mutation-checked 4/4. Workers hold no S3 credential at all — only per-key presigned URLs. |
| Image generation | `IMPLEMENTED` (governance `TESTED`) | `control/image_generation.go` + `POST /v1/images/generations`, 81st route, buyer-owned in the authorization matrix. **Governance is the finished part**: size allowlist, n cap, prompt bound, and refusal of `b64_json` (an inline image never enters object storage, so it would have no retention, erasure or dispute-evidence path). Content policy refuses CSAM, non-consensual intimate imagery, photorealistic real-person likeness and forged documents, checking two normalisations because separator evasion defeats either one alone — my own adversarial test caught that. Refusals name the rule and never echo the prompt. Licence gate is separate from the text one because open image licences (OpenRAIL-M/++) attach use restrictions the licensee must pass downstream, and merc resells generation. Mutation-checked 5/5. **`NOT_IMPLEMENTED` for the lane itself**: there is no image runtime, so an acceptable request returns 503 rather than an invented result. No contract, no supplier, no settlement. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| Bounded media transcode | `REAL_RUNTIME_PROVEN` private canary; public activation `BLOCKED` | The governed `ffmpeg-transcode-v1` `builtin` cell now completed the real buyer quote → firm submit → two sandboxed `merc-agent` suppliers → FFmpeg execution → byte-exact MP4 merge → primary/redundancy receipt path in `evidence/canary/media-transcode-full-loop-r1.json`. Input/output hashes, H.264 320×180 probe, 4.228s end-to-end elapsed time, zero-sum ledger, and isolated Compose teardown are retained. This is deterministic media processing, not image generation or a public payment claim. |
| Deterministic scene rendering | `REAL_RUNTIME_PROVEN` private shadow-ledger proof; public activation `BLOCKED` | The bounded `svg-scene-render-v1` builtin cell now completes a real CAD buyer quote → firm submit → two sandboxed agents → deterministic PPM rasterisation → byte-exact merge → receipt path. The latest quote-bound receipt is retained in `evidence/canary/cad-local-proof-aefa51df.json`: its 64×64 quote explicitly records 4,096 declared output-pixel billable/settlement units and CAD currency, alongside the 1.37 FX revision, independent redundancy, teardown and zero-sum invariants. This is governed rendering/media IR, not prompt-to-image generation; the image route remains fail-closed at 503 until a licensed image model runtime exists. |
| LoRA training | `IMPLEMENTED` (settlement `TESTED`) | `control/lora_settlement.go`. **Outcome-aware settlement is the finished part.** The price splits: a compute floor the supplier is owed however the run turns out, and an outcome bonus contingent on an independent evaluation. Each of the three alternatives is wrong on its own — supplier-bears-risk means a supplier can lose a day's revenue to someone else's dataset (and merc's pricing governance already refuses prices that don't cover electricity); buyer-bears-risk makes 'outcome-aware' just billing with a nicer name; merc-bears-risk is unbounded. merc's cut comes from the floor, so it never earns more from a failed run than a successful one — asserted. Evaluation must be independent at the **account** level, not just the worker: two worker ids on one account are not two opinions. A held-out set that leaked into training is refused, because the improvement would measure memorisation. Conservation proven over 20,000 randomised settlements; mutation-checked 7/7. **`NOT_IMPLEMENTED` for the lane**: no trainer, no evaluator dispatch, no adapter deployment, no GPU. |
| External-model onboarding | `TESTED` | `control/model_onboarding.go` runs at process start and panics on a model merc cannot resell. Licence is an allowlist (an unrecognised one is refused, not assumed permissive); `remote_code=true` is refused unconditionally because it runs repo-supplied code on third-party supplier hardware; a required attribution must appear in the shipped `NOTICE`, checked against the real file by `TestCatalogueAttributionAppearsInNotice` (mutation-checked: removing "Built with Llama" from NOTICE fails CI). Both catalogue models now declare licence, URL, commercial-use and remote-code posture. `scripts/onboard-model.py` is now the live half: it takes a model through policy, identity, a real smoke completion, determinism at temperature 0, and a MEASURED benchmark against a running runtime, then emits a runtime profile carrying the measured throughput -- so a catalogue price derives from what the model did, not what someone assumed. External-model policy gates are tested; measured onboarding tok/s at this commit is unknown. The live half still exercises policy, identity, smoke completion and temperature-0 determinism against a running runtime when one is available, but any historical harness tok/s figure is unbound and is not claimed here. `scripts/onboard-model-canary.sh` asserts four refusals too (non-commercial licence, remote_code, an alias the runtime does not serve, an unpinned revision), because a gate that only ever says yes is not a gate. Still `TESTED` not `CANARY_PROVEN`: it does not walk the money chain. |
| Single-host multi-GPU | `IMPLEMENTED` (local authority `TESTED`) | `control/multi_gpu_admission.go`, `control/realtime_placement.go`, and the compiled `merc-agent vllm` path. The vLLM adapter was previously an orphan source file; it is now built, exposed as a CLI command, and registers explicit CUDA class, physical GPU count, per-GPU memory, committed memory, and `nvlink`/`pcie`. The container receives exactly devices `0..TP-1`, never `--gpus all`. The control plane selects the smallest admissible degree, copies the exact placement JSON+digest from offer to contract, exposes it on the receipt, and makes contract placement immutable in PostgreSQL; historical contracts remain readable without invented topology, while legacy offers are drained until they re-register. Per-rank overhead does **not** shrink; PCIe is capped at TP=2; attention heads must divide the degree; undeclared multi-GPU interconnect is refused. 50,000 randomised planner cases plus offer→contract→receipt and tamper tests; mutation-checked 9/9. **Still `EXTERNALLY_BLOCKED` for a sellable TP>1 lane**: the embedded profile is TP=1 and `UNPROVEN`; no TP>1 profile with measured weight/overhead requirements and no real multi-GPU receipt exists. The code does not promote that missing evidence into a claim. |
| Buyer dashboard | `TESTED`; candidate canary `OPEN` | `web/buyer.html`. Its live script signs in to a running Merc and opens the workspace, but it does not emit an exact-candidate receipt and therefore cannot promote itself. |
| Supplier console | `TESTED`; candidate canary `OPEN` | `web/supplier.html` behind `GET /supplier`. The current automated command uses recorded control-plane responses to prove worker-token auth, ledger-granularity money, four payout-rail states, and refusal behavior. Recorded responses are deliberately capped at `TESTED`; mutation-checked 3/3. |
| Public price board | schedule mechanics `TESTED`; fresh production publication `REFUSED` | The page/schedule arithmetic, fixed-point currency fields, append-only persistence, rollback and replay mechanics are covered through one explicit in-memory `TEST_ONLY` BOUND-throughput/MEASURED-power seam. Production construction now requires every throughput receipt to be BOUND with complete producer identity and a real source commit, plus an exact MEASURED sustained-power row. The shipped M3 Pro throughput receipt is UNBOUND and all checked-in watts are ASSUMED, so a fresh process and release image refuse before publishing instead of treating diagnostics as buyer-price authority. Previously persisted schedules remain self-contained and replayable. No candidate buyer-to-receipt claim exists. |
| Python SDK | historical live exercise; candidate canary `OPEN` | `clients/sdk/python/merc/`, clean-room install verified. The live script submits a job to a running Merc, waits for a worker, and validates the result, but does not yet emit source-bound evidence. |
| TypeScript SDK | historical live exercise; candidate canary `OPEN` | `clients/sdk/typescript/` builds to `dist/`; its live run exposed and locked tests for the idempotency header, JSONL input shape, and cancel route. It does not yet emit source-bound canary evidence. |

## Historical REAL_RUNTIME_PROVEN: batch embeddings, 2026-07-27

This section is retained execution history, not current admission authority.
Production physical admission now refuses before a quote or job write for the
unit-scope reasons above.

merc's original supply is Apple Silicon running candle on Metal, and this machine
is an M3 Ultra — an admitted `apple_silicon_ultra` host. A real GPU rental was
never required to prove the batch lane. I had been treating every lane as
GPU-blocked, which under-reported what merc can actually do.

The shipped `merc-agent` binary registered (1,980 embeddings/sec measured on
Metal), claimed a real buyer job, and the whole chain completed:

| step | result |
|---|---|
| buyer request | `POST /v1/jobs`, 3 rows, idempotency-keyed |
| merc contract | job `fd1999ac`, 2 tasks, estimate $0.000250 |
| scheduler | dispatched to worker `…b1` |
| **real runtime** | candle on Metal, Apple M3 Ultra — not a fake, stub or httptest server |
| result | 3 × 384-dim embeddings, fetched through a presigned URL |
| verification | `honeypot-checked`, 1 passed / 0 failed |
| buyer debit | −$0.000250 |
| supplier payable | +$0.000002 |
| merc contribution | +$0.000248 (positive) |
| receipt | `evidence/canary/real-runtime-embed.json` |

### The historical small-job supplier hole is fixed

The retained 3-row receipt gave merc **99.2%** of the buyer charge. That remains
historical evidence of the former per-task fixed-cost defect, not the current
policy. New schedules declare `MERC_CONTROL_PLANE_PER_BATCH_USD`; the same
economic batch that amortises the processor fixed fee allocates this overhead
pro rata. `MERC_MIN_CONTRIBUTION_PER_BATCH_USD` prevents a percentage target
from rounding to zero on a micro-job.

This is the same failure class as the LoRA compute floor truncating to zero: a
fixed cost dominating a small quote leaves a party with approximately nothing,
through arithmetic rather than policy. A supplier serving only minimum-size jobs
would have worked for almost nothing. `BuildEconomicPlan` now both derives the
minimum billable supplier floor and solves the discrete micro-ledger scenarios
against the configured absolute contribution. Supplier settlement projection
rounds upward and may never fall below the exact nano-unit floor. The figures
above remain historical measurements, not a claim about
current quotes.

### Two bugs this run surfaced

- A stale control binary dispatched under an older runtime-authority matrix than
  the worker had attested to, and the agent refused every task. That gate works,
  and it is exactly the re-attestation consequence recorded when `matrix_version`
  was bumped — but nothing had exercised it until a real worker did.
- The control plane refuses a job it cannot verify (`no usable honeypot is
  seeded for this workload`) rather than running it unverified. Fail-closed,
  confirmed by hitting it.

## REAL_RUNTIME_PROVEN: OpenAI-compatible realtime, 2026-07-27

I had reported this lane blocked on CUDA. It was not. **"Real runtime" and
"CUDA" are different capabilities**, and conflating them is what made me report
a lane blocked that a locally installed engine could serve.

`llama-server` (llama.cpp, already installed) loaded the exact GGUF merc's
catalogue pins — `Llama-3.2-1B-Instruct-Q4_K_M`, already in the HF cache from
the agent's own benchmark — and served merc's realtime lane on Metal.

| step | result |
|---|---|
| worker offer | registered `ACTIVE`, profile sha256 matched |
| buyer request | `POST /v1/chat/completions`, idempotency-keyed, `X-Merc-Max-USD` |
| merc contract | `b59d6380`, authorized $0.000035 |
| **real runtime** | llama.cpp on Metal, real weights, real tokens |
| result | a real 31-token completion |
| verification | `PASSED`, receipt state `VERIFIED` |
| authorization | `CAPTURED` — $0.000019 captured, $0.000016 released |
| buyer debit | $0.000019 |
| supplier payable | $0.000002 |
| merc contribution | **$0.000017 (positive)** |
| receipt | `rcp_559d4988…`, with `stream_root_sha256` and `output_commitment` |

### Gateway overhead: unproven at this commit

Gateway overhead is unproven at this commit. Self-test and unattested harness
numbers must not be quoted. An older `scripts/realtime-parity-benchmark.py` run
once reported a ~3.9 ms median delta (merc 58.4 ms vs engine 54.4 ms over 5
samples) but marked itself `UNATTESTED_HARNESS_RUN` with
`public_claim_allowed: false` because llama-server presents no runtime
attestation header; `evidence/perf/gateway-parity.json` is withdrawn and the
remaining parity artifacts are harness self-tests with `comparable: false`.
None of those figures is a bound measurement at this commit.

### What is still genuinely CUDA-blocked

The `runpod_vllm` lane — NVIDIA hardware and a pinned, digest-addressed vLLM
image — is now tracked as its own lane rather than hidden inside "realtime". No
Apple Silicon engine substitutes for it. Same for image generation and LoRA.
Multi-GPU now has a compiled adapter and frozen local placement authority, but
remains externally blocked on a real TP>1 profile, host, benchmark and receipt.

## Money defect found by running the SDK against a live merc

Running the shipped Python SDK against a running control plane — something no
test had ever done — surfaced a defect in merc's pricing that no unit test could
have, because it only appears after the catalogue is repriced from a **real**
supplier's measured throughput.

### Historical failure: the supplier was paid zero while the buyer was charged

| units | buyer charged | supplier paid |
|---|---|---|
| 1–4 | *rejected*: `base_compute_usd must be finite and positive` |
| 10 | $0.000124 | **$0.000000** |
| 100 | $0.000125 | $0.000001 |
| 1,000,000 | $0.018000 | $0.014400 |

Between roughly 5 and 99 units the old plan was executable, the buyer was
charged, and `SupplierPayoutPerTaskUSD` was exactly zero. This was not the
sub-cent carry the accrual path handles — the payout was 0, so nothing was
accrued at all and Merc recorded no obligation to whoever performed the work.
`roundEconomicUSD(computePerTask * SupplierShare)` rounds 0.0000000144 to zero.

### The causal chain is perverse

```
a real supplier benchmarks at 1,980 embeddings/sec on an M3 Ultra
  → merc reprices the catalogue from measured supplier throughput
  → the per-1k price falls to $0.000018
  → small jobs' base compute rounds to zero at micro-USD granularity
  → the supplier is paid nothing, or the job is rejected outright
```

**A faster supplier makes small jobs unpayable, then unbuyable.** Nobody chose
that; it falls out of rounding.

### This is the third instance of one failure class

1. LoRA compute floor truncating to zero at small quotes — **fixed**, with a
   minimum quote derived from the share constants.
2. Supplier share collapsing to 0.8% on a 3-row job — fixed per-task
   control-plane cost dominating. **Recorded.**
3. Supplier payout rounding to exactly zero between 5 and 99 units —
   **fixed by the derived minimum-billable floor**.

`control/small_job_economics_test.go` now asserts the corrected behavior:
positive buyer charge for physical work implies strictly positive supplier
liability, and buyer charge covers supplier plus control-plane cost across the
former zero-payable window, multi-task shapes, and multiple supplier shares.

## Three TypeScript SDK defects, found by running it against a live merc

The TS SDK had 12 passing unit tests. Every one used a stub `fetch` that
accepted whatever it was sent, so none could catch a client speaking a shape
merc rejects. **The shipped client could not submit a job at all.**

| defect | what merc did | why the tests missed it |
|---|---|---|
| No `Idempotency-Key` header | `400` on every `submitJob` | the stub never inspected headers |
| `input` sent as an array | `400 input must be a JSONL string` | a test asserted the array shape as correct, calling it "matching the Python SDK" — it never matched; Python has always serialised to JSONL |
| `cancelJob` → `POST /v1/jobs/{id}/cancel` | route not served | the stub answered any URL |

All three fixed, the test that pinned the wrong shape rewritten, and three new
tests added that assert the header, the JSONL serialisation and the cancel
route. 12 tests → 15.

Both SDKs now have a **live** lane: `scripts/sdk-live-python.py` and
`scripts/sdk-live-typescript.mjs` submit a real job to a running merc, wait for
a real Metal worker, and validate the real result. Those runs found defects and
remain valuable historical compatibility evidence, but they do not emit an
immutable-image, exact-commit receipt. They therefore remain below
candidate-bound `CANARY_PROVEN`.

## Repository boundary and rename

| item | status | evidence |
|---|---|---|
| VisionMCP extracted | `TESTED` | VisionMCP remains a separate repository, with **zero** files tracked by merc. `scripts/validate-repo-boundary.py` runs in `make ci` and fails if any VisionMCP path enters merc's tree. merc currently has 543 tracked files and **121,325** owned LOC; none is VisionMCP. The untracked `live-instrument` design archive and its VisionMCP-linking `.mcp.json` remain preserved in their separate worktree and are intentionally excluded from this candidate. |
| Rename zero-residue audit | `TESTED` | `scripts/rename-residue-audit.py`, in `make ci`. FROZEN 260 / BLOCKED 407 / **RESIDUE 0**. Frozen and blocked classes are itemised with a per-identifier reason in `docs/RENAME_REGISTER.md` §5. |

## Money and operations

| item | status | evidence |
|---|---|---|
| Supplier accrual | `TESTED` | `control/supplier_accrual.go`. Micro-USD conservation proven under 24 concurrent claims, 240 randomized orderings, and 9/9 mutation detection. |
| Payout reconciliation | historical provider exercise; candidate `TESTED` / formal gate `OPEN` | The CAD/USD mismatch that blocked this is gone: settlement currency is configuration (`control/currency.go`) and the platform settles CAD. Supplier accrual, minor-unit carry and the sole ledger writer are unchanged. The current candidate still requires the formal test-mode payout and reconciliation matrix. |
| Stripe API contract | `TESTED` | Every product and operator-script Stripe request pins `2025-06-30.basil` immediately before network I/O. Billing and Connect endpoint creation additionally pins webhook payload rendering; existing null/mismatched endpoint versions fail closed because Stripe cannot update that field in place. Signed events must carry that exact version and the expected test/live mode before any effect runs. Charges, setup, reads, Connect, payouts, refunds, reversals, probes, webhook management, and later signed event shapes cannot drift with the account default. Static bypass guards, operator-script self-tests, and transport/operation-shape tests cover the boundary; mutation-checked 5/5. |
| Stripe Sandbox authority | `TESTED` scaffold; provider execution `OPEN` | The Level B manifest now explicitly selects `test` payment mode, the Stripe provider, and the mandatory CAD settlement currency; production defaults can no longer leave the configured canary structurally SEALED. Preflight and matrix share one authority for API version, CAD provider objects, a distinct payout-enabled Canadian connected account, Stripe's Canadian success/failure payout fixtures, distinct endpoint IDs/secrets, exact staging host paths, complete event inventories, and sanitized receipts. Signed no-value probes require the real handler/database to classify terminal-first application as `applied`, the older opening fact as `stale_ignored` behind rank 30, and an exact replay as `duplicate`; the matrix no longer self-asserts those outcomes. Offline adversarial tests reject URL/version/event/country/currency/ID drift before network access. No claim of provider execution is made. |
| Aggregated billing / prepaid | `IMPLEMENTED` | 4 references in `control/accounts.go`; charge batching reworked so the age trigger no longer fires at Stripe's $0.50 floor. |
| Quote/job currency authority | local database `TESTED`; provider reconciliation `OPEN` | Every new quote, accepted job, economic plan, verification settlement, task ledger row, SLA adjustment, and charge batch carries one explicit ISO currency. Database constraints bind full quote/economic JSON, quote-to-job acceptance, job-to-plan authority, and task-to-ledger writes; authority columns and targets are immutable. A currency cutover cannot reinterpret numeric amounts: the wrong deployment cannot bind the quote, claim or start the job, settle it, expose it for single/batched collection, or confirm provider cash. Exact-result reuse follows the same fence. Legacy persisted verification plans remain readable but derive and validate the immutable job currency before writing money. Fresh-PostgreSQL tests cover CAD visibility and success, CAD/USD cutover refusal at every lifecycle boundary, atomic rollback, hostile mutation/direct inserts, and schema reapply. Historical `_usd` field names are legacy storage/API names, not currency authority. |
| Catalogue price authority | schedule mechanics/database `TESTED`; current production publication `REFUSED` | `pricing/board.json` is explicitly a USD reference. A non-USD deployment must supply a finite positive reference-to-settlement rate and immutable revision; neither application code nor staging scaffolding invents one. Fresh publication additionally requires every throughput input to be `BOUND` with complete producer identity and a real source commit, plus an exact `MEASURED` sustained-power row for its hardware. The checked-in M3 Pro receipt is UNBOUND and every current watts row is ASSUMED, so startup now refuses instead of letting those diagnostics authorize buyer prices. One explicitly synthetic in-memory/file test seam proves schedule construction, FX conversion, append-only persistence and rollback mechanics only; it is not production evidence. Previously stored schedules remain self-contained and replayable. The release-image boot gate remains blocked until a BOUND throughput receipt and measured power authority exist. |
| Quote/planner/claim placement authority | local database `TESTED`; current production performance ingress `REFUSED` | A physical quote that passes current ingress carries a versioned placement requirement freezing the exact server-authorized model kind, runtime cell, runtime ID, engine, runtime-matrix digest, frozen performance snapshot, effective-memory floor, hardware and residency sets, reputation/trusted gates, and modeled supplier ask admission ceiling. Historical accepted placements validate and replay from that frozen snapshot. New ingress separately requires today's `BOUND` performance `Unit`/`UnitScope` to match settlement; no production lane currently does, so production physical admission refuses before a quote or job write. The USD/hour ceiling is derived from the frozen USD reference price, supplier share, tier and modeled throughput; it is not advertised as realized hourly pay, because settlement remains per accepted task. Quote capacity, warm capacity, pool reputation, adaptive split sizing, and the throughput planner execute one shared predicate after this authority gate; trusted status is derived from live reputation plus completed tasks rather than a stale label. Bound submission refuses placement, ceiling, or current-authority drift. PostgreSQL makes every scheduler-facing projection immutable after acceptance. Fresh-database hostile and tamper tests cover predicate agreement, historical replay, current unit-scope refusal, the quote authority, job projection, database trigger, missing placement JSON, and schema reapply. No current capacity, fleet, or SLA canary claim is made from this local proof. |
| Composite pricing decision authority | local database `TESTED`; provider/fleet reconciliation `OPEN` | Any physical quote admitted by current ingress and every accepted job binds workload, compute, placement, economic-plan and economic-schedule digests to the exact append-only catalogue schedule, market-board digest, model formula, USD reference price, settlement price, FX rate/revision, supplier share, tier, billable units, modeled time basis, buyer price, maximum price and component costs. Storage, egress, provider energy/depreciation and risk remain explicitly `unknown`, never modeled zero. PostgreSQL stores immutable placement/pricing JSON plus canonical SHA-256 values and cross-checks their workload, compute, placement and currency projections. A bound submission keeps the quote-pinned price, FX and supplier-share terms; durable ingress reads the current model schedule pointer only to require byte-exactly unchanged physical authority. A price-only reprice therefore preserves an unexpired quote, while a throughput, power, runtime, build, device or receipt supersession refuses before writes. Unchanged terms retain the exact quote pricing SHA, while a declined SLA or firm-cap variant carries the immutable origin quote SHA. Exact-result reuse has no physical placement and marks supplier and verification cost `not_applicable`; its firm cap is checked before durable writes. Clearing receipts expose the complete decision, exact digests, catalogue/FX provenance and modeled-versus-settled reconciliation. Fresh-PostgreSQL tests cover quote→job→scheduler→receipt digest continuity, arbitrary positive-rate co-mutation, every authority-family digest, exact-reuse cap/zero-work semantics, nested database tamper rejection, historical currency readability and legacy NULL authority. No external provider or fleet observation is promoted by this local proof. |
| Verification recovery capacity | local database `TESTED`; fleet canary `OPEN` | Capacity is derived from a worst-case three PostgreSQL connections per verifier, with two connections preserved for API/background work and one for worker-leader election. The default 20-connection pool admits exactly five concurrent processors; unsafe worker pools below six refuse startup. Recovery claims and hydrates leases in one transaction, drains bounded parallel waves, serializes only the same job chunk, reports forward progress independently of sweep duration, and returns every owned lease promptly to `pending` on cancellation or error. Backlog, expired-lease, oldest-open, retry, terminal-outcome, and no-progress metrics feed three page rules and a runbook. Fresh-PostgreSQL and race tests prove a 100-row backlog is processed exactly once at the safe cap, same-chunk exclusion does not block other chunks, pool headroom remains live, cancellation leaves no leased rows, and a progressing sweep stays healthy while 30 seconds without progress becomes stale. No fleet-load claim is made. |
| Processor-fee allocation | local database `TESTED`; provider reconciliation `OPEN` | New batch fees use deterministic Hamilton/largest-remainder allocation at micro-USD precision with immutable job-ID tie-breaking. Pre-upgrade allocations are preserved and explicitly marked `legacy_order_residual_v0`, never silently rewritten. The database binds every allocation to its exact batch, job, provider reference, and method; serializes concurrent allocators; preserves append-only rows; rejects partial/mismatched replays; and requires exact fee conservation. Invoices and clearing receipts expose the per-job allocation, versioned method, and platform net after that fee without exposing Stripe identifiers; batch invoices fail closed on an incomplete allocation. Ten thousand randomized order/permutation/quota cases and fresh-PostgreSQL concurrency/mutation tests pass. No Stripe object or provider cash evidence is claimed. |
| Refunds / disputes | `IMPLEMENTED` | 21 files reference disputes. Transfer reversal has never met real Stripe. |
| Stripe sandbox end to end | historical capability evidence; formal gate `OPEN` (`NO_GO`) | Historical CAD-settlement provider evidence is retained without promotion. The current formal candidate still lacks the complete test-mode matrix and provider-reconciliation receipt required by `P1-STRIPE-TEST`; live activation is sealed and Level C remains prohibited. |
| Production deployment / TLS | persistent staging `READY`; live money `NO_GO_PROHIBITED` | **Historical (this shippability pass):** no SSH key in session; droplet `/version` 404s. **Current (2026-08-17):** `https://mercmerc.net/version` HTTP 200, commit `19fe0b23`, `modified: false`, `go1.26.6`; `/readyz` 200, `payment_mode=test`, `live_value_movement=false`; Let's Encrypt public TLS. That commit is 20 behind HEAD. |
| Backup / restore | `TESTED` | Gates present and passing. |
| Alerts / status / rollback | `REAL_RUNTIME_PROVEN` private | 27 alerts validated; Alertmanager fire→HTTP sink→resolve was observed through the real local webhook path in `evidence/autonomous/alert-delivery-r1.json` (unbound local path receipt; not external paging authority); external staffed paging remains open. |
| Licence scope | `PARTIAL` | Split done; `validate-license-register` deliberately red pending counsel. |
| Buyer/supplier/privacy/refund terms | `EXTERNALLY_BLOCKED` | Drafts marked DO NOT PUBLISH pending counsel. |
| Internal security review | `PARTIAL` | Adversarial input, fuzzing, tenant-isolation and mutation testing done this session; found 3 real defects. |
| Independent pentest | `EXTERNALLY_BLOCKED` | Requires an external firm. |

## Rename

Tier 1 landed (brand prose, Go module `merc/control`, Python distribution
`merc`, website copy). Tier 2 and the FREEZE list are recorded in
`docs/RENAME_REGISTER.md` and are unchanged: env vars, registry paths, the
GitHub repo slug and production directories are **EXTERNAL**, and hash domains,
4-byte binary magics, live credential prefixes and signed receipts are
**FREEZE — never rename**. Renaming those would falsify receipts or silently
invalidate every previously computed digest.

## VisionMCP

`EXTRACTED` and deliberately separate. VisionMCP has its own repository and
history. It is not tracked, built, counted, or shipped by Merc; the repository
boundary validator reports zero foreign paths. The committed
`design/computexchange-live-instrument` branch is already an ancestor of this
candidate. Its remaining untracked 1.3 GB internal-design archive and
VisionMCP-linking `.mcp.json` are preserved in that worktree, not merged here.

## Honest summary

Merc is a buildable, locally proven Level A software candidate with historical
batch, realtime, deterministic scene-rendering, and single-GPU CUDA/vLLM
execution evidence. Accepted historical contracts and receipts retain their
frozen compute, rendering, realtime-placement, and capacity authority for
self-contained replay. They are not current admission authority: production has
zero `BOUND` performance lane whose unit and unit scope match settlement, so new
physical admission refuses. TP>1, full prompt-to-image generation, LoRA
execution, external staffed alert delivery, and the formal external release
exercises remain unproven. The private local alert receiver path is proven, but
it is not staffed paging authority.

The last machine-derived readiness tool prints **87/100**, **P0=0** and
**five** open P1s (`python3 scripts/validate-readiness.py` at this HEAD).
84/100 is the local-receipt ceiling before the offsite pair; that pair is now
present and the derived total is 87. The remaining 13 points are Stripe (6),
24-hour soak (3), external staging-attack rehearsal (1), and three qualified
human approvals (privacy, licensing, staffed abuse). Staging, the 3600 s
alpha soak, and offsite restore are no longer independently open.
`P1-STRIPE-TEST` is the remaining `ALPHA_BLOCKER`. Level B is `NO_GO`;
`EXTERNAL_ALPHA_PROVEN` is false; Level C live money/public launch is
prohibited. No historical credential, deployment, boot, or canary receipt
overrides any of those gates. New admission still needs a BOUND performance
receipt whose unit scope matches settlement, and fresh catalogue publication
still needs BOUND throughput plus exact MEASURED sustained power.


<!-- source: docs/MERC_SHIPPABILITY_DIRECTIVE.md -->

<!-- Preserved from a repository planning attachment before that scratch copy was deleted.
     This is the governing Merc directive; it belongs in the repository, not
     in a transient attachments directory. -->

````text
MERC FINAL SHIPPABILITY, PIPELINE INTELLIGENCE, PERFORMANCE, PRICING, AND MARKET-DOMINANCE DIRECTIVE

ROLE

You are the primary implementation agent for Merc.

An external review channel is available for independent analysis, adversarial review, benchmarking, architecture, and audit.

Use external review continuously as a feedback loop, not as a substitute for implementation.

The objective is to bring Merc from its current partially canary-proven state to a complete, production-grade managed AI execution platform whose routing, pricing, execution, verification, settlement, developer experience, and operational reliability are all independently defensible.

Do not optimize for activity, commit count, route count, test count, or documentation volume.

Optimize for real product proof.

Do not stop after one audit or one implementation cycle.

Continue the constructive loop until:

1. every launch-critical lane is at least CANARY_PROVEN;
2. the complete pipeline has been independently graded and materially improved;
3. the pricing system is demonstrably sustainable and competitive;
4. Merc has repeatable workload-placement superiority;
5. no known high-leverage architectural, economic, security, or reliability issue remains unaddressed;
6. remaining blockers are genuinely external and returned as one consolidated action queue.

CURRENT VERIFIED BASELINE

Preserve and verify this current truth before changing it:

- Local Metal execution works.
- Realtime, batch inference, and embeddings have real local execution evidence.
- Gateway overhead is NOT currently measured. The artifact this line used to rest
  on, `evidence/perf/gateway-parity.json`, is `INVALIDATED_PENDING_RERUN`, and the
  only parity artifacts that exist at this commit are `HARNESS_SELF_TEST` with
  `comparable: false`. Until a bound parity receipt exists, treat gateway overhead
  as unproven rather than small.
- Supplier microunit accrual conserves money.
- Finite charge retries exist.
- Reuse billing classes distinguish physical and nonphysical work.
- Exact-result cache primitives exist.
- In-flight request coalescing has eliminated duplicate inference in synthetic concurrent traffic.
- Prefix-aware routing exists.
- Fuzzing, concurrency stress, mutation testing, and randomized money state-machine testing are present.
- Stripe credentials authenticate.
- Stripe sandbox currently functions in CAD and should remain CAD for test authority.
- RunPod credentials authenticate.
- RunPod worker startup and durable CUDA enrollment remain incomplete or only partially proven.
- Merc currently admits only the job classes proven by runtime authority.
- TensorParallelSize and PipelineParallelSize fields alone do not prove multi-device scheduling or execution.
- VisionMCP is outside the Merc product boundary and must not be modified or counted.
- Historical inflated throughput claims are retired.
- Physical inference, delivered-token efficiency, reuse, and outcome efficiency must remain separate authorities.

Revalidate the baseline from code, receipts, tests, runtime, and current infrastructure before relying on it.

MISSION

Build Merc into the strongest practical bounded AI execution platform.

The complete pipeline must be able to:

1. understand what the buyer is asking to execute;
2. classify the workload accurately;
3. estimate compute, memory, storage, network, latency, and verification requirements;
4. determine whether one device is sufficient;
5. determine whether tensor, pipeline, data, expert, frame, sample, or task parallelism is appropriate;
6. select a compatible runtime;
7. select an appropriate hardware topology;
8. produce a truthful quote and price ceiling;
9. schedule the workload;
10. acquire and cache inputs and models;
11. execute reliably;
12. verify the output or outcome;
13. charge the buyer;
14. create supplier payables;
15. preserve positive Merc contribution;
16. recover or refund failures;
17. expose an inspectable receipt;
18. improve from every benchmark and failure.

The user should not need to understand GPU rental, runtime configuration, model memory, tensor parallelism, container operations, batching, verification, supplier trust, or settlement.

Merc must convert a requested outcome into a correctly planned, priced, routed, executed, verified, and settled result.

PRIMARY CONSTRUCTIVE LOOP

Repeat this loop continuously:

1. INSPECT
   - Read the current repository, schemas, routes, SDKs, agents, runtime profiles, schedulers, pricing rules, receipts, operations, and benchmark evidence.
   - Distinguish:
     IMPLEMENTED
     WIRED
     TESTED
     REAL_RUNTIME_PROVEN
     CANARY_PROVEN
     PRODUCTION_PROVEN
     DOCUMENTED_ONLY
     DEAD
     EXTERNALLY_BLOCKED

2. AUDIT WITH EXTERNAL REVIEW
   - Give the reviewer a bounded audit contract.
   - Require citations to exact code, schema, runtime evidence, receipts, or measurements.
   - Require a score from 0–10 for each category and subcategory.
   - Require explicit missing evidence.
   - Require a concrete solution set.
   - Require the reviewer to distinguish fact, inference, proposal, and unknown.
   - Require at least one adversarial challenge to every major conclusion.

3. VERIFY THE REVIEW
   - Independently verify the reviewer’s claims.
   - Reject false positives.
   - Correct weak framing.
   - Do not implement unverified recommendations.

4. PRIORITIZE
   Rank work by:

   expected buyer value
   × expected revenue or savings
   × probability of success
   × blocker reduction
   ÷ implementation cost
   ÷ operational risk

5. SPLIT IMPLEMENTATION
   - Assign the external reviewer an independent, nonconflicting implementation or audit tranche where useful.
   - Keep the highest-risk money, authority, routing, and lifecycle work under direct review.
   - Use isolated worktrees and isolated databases.
   - Never allow parallel agents to mutate the same authoritative state or test database.

6. IMPLEMENT
   - Complete a coherent vertical slice.
   - Remove obsolete mocks and stubs when replaced.
   - Preserve migrations, receipt verification, and rollback.
   - Do not leave interface-only backends.

7. PROVE
   - Unit tests.
   - Integration tests.
   - Race tests.
   - Mutation tests where authority matters.
   - Fuzzing where parsers, billing, scheduling, identity, and state machines matter.
   - Real runtime.
   - Real hardware where applicable.
   - Failure injection.
   - Economic reconciliation.
   - Canary evidence.

8. BENCHMARK
   - Compare direct runtime versus Merc-wrapped runtime.
   - Record workload, hardware, runtime, model, revision, precision, batch, latency, throughput, quality, cost, supplier contribution, Merc contribution, and failure rate.
   - Never merge physical, delivered, cached, or outcome-equivalent measurements.

9. RE-GRADE
   - Ask the external reviewer to independently re-grade only the affected categories.
   - Require evidence for every score increase.
   - Reject score inflation from documentation or test-only progress.

10. CONTINUE
   - Select the next highest-value blocker.
   - Do not pause for ordinary engineering decisions.
   - Pause only for genuinely external approvals, secrets, paid infrastructure authorization, legal decisions, or destructive production actions.

COMPLETE PIPELINE AUDIT

Create and maintain a scored pipeline matrix.

Use at least the following categories and subcategories.

CATEGORY 1 — REQUEST INTAKE AND WORKLOAD CLASSIFICATION

1. Natural-language or structured request interpretation.
2. API route and manifest validation.
3. Model-specific versus outcome-specific intent.
4. Workload classification:
   - realtime generation;
   - batch generation;
   - embeddings;
   - reranking;
   - classification;
   - structured extraction;
   - image generation;
   - video generation;
   - fine-tuning;
   - evaluation;
   - rendering;
   - multimodal preprocessing;
   - model onboarding.
5. Determinism detection.
6. Exact-result cache eligibility.
7. Prefix-reuse eligibility.
8. In-flight coalescing eligibility.
9. Verification-policy selection.
10. Latency-class selection.
11. Data-rights and privacy classification.
12. Invalid or ambiguous request handling.

Target: 10/10.

Merc must not route based only on a caller-supplied job_type string without independently validating workload shape and requirements.

CATEGORY 2 — COMPUTE-WEIGHT ESTIMATION

1. Input token estimation.
2. Output token estimation.
3. Context-length requirements.
4. Model-weight memory.
5. KV-cache memory.
6. Adapter memory.
7. Image/video tensor memory.
8. Training memory.
9. Optimizer state.
10. Activation memory.
11. Render memory.
12. Temporary storage.
13. Network transfer.
14. Expected duration.
15. Expected power or provider cost.
16. Verification cost.
17. Failure/retry reserve.
18. Confidence range.
19. Historical estimate calibration.
20. Estimate versus actual reconciliation.

Create a versioned Compute Plan:

```json
{
  "workload_class": "...",
  "model_revision": "...",
  "runtime_candidates": [],
  "minimum_vram_bytes": 0,
  "preferred_vram_bytes": 0,
  "host_ram_bytes": 0,
  "storage_bytes": 0,
  "network_bytes": 0,
  "expected_input_units": 0,
  "expected_output_units": 0,
  "expected_seconds": 0,
  "expected_cost": 0,
  "maximum_cost": 0,
  "parallelism": {},
  "verification_cost": 0,
  "confidence": 0
}
````

Target: estimate errors low enough that OOM, underquoting, and unnecessary expensive placement become exceptional rather than ordinary.

CATEGORY 3 — RUNTIME AND HARDWARE COMPATIBILITY

1. Model architecture support.
2. Runtime support.
3. CUDA version.
4. Driver.
5. Metal support.
6. GPU architecture.
7. GPU memory.
8. CPU architecture.
9. Host memory.
10. Storage.
11. Quantization.
12. Context.
13. LoRA compatibility.
14. Multimodal compatibility.
15. Tensor-parallel support.
16. Pipeline-parallel support.
17. Data-parallel support.
18. Expert-parallel support.
19. Network-topology requirement.
20. Runtime-version pinning.
21. Model and tokenizer revision pinning.
22. Health and readiness.
23. Startup and warm-state estimates.

No scheduler route may rely on unsupported profile fields alone.

Compatibility must be physically proven.

CATEGORY 4 — PLACEMENT AND DELEGATION

1. Single-device fit.
2. Single-host multi-GPU.
3. Multi-host eligibility.
4. Tensor-parallel selection.
5. Pipeline-parallel selection.
6. Data-parallel selection.
7. Expert-parallel selection.
8. Frame or sample parallelism.
9. Batch partitioning.
10. Artifact locality.
11. Prefix locality.
12. Adapter locality.
13. Region.
14. Buyer privacy.
15. Supplier trust.
16. Price ceiling.
17. Deadline.
18. Queue depth.
19. Runtime startup.
20. Fault domain.
21. Fallback.
22. Explainability.
23. Rebalancing.
24. Drain and maintenance.
25. Preemption.

Build a generic Placement Plan rather than hardcoding RunPod.

Example:

```json
{
  "topology": "SINGLE_HOST_TENSOR_PARALLEL",
  "runtime": "vllm",
  "device_count": 4,
  "required_vram_per_device": 80000000000,
  "interconnect": "NVLINK_PREFERRED",
  "model_sharding": "TENSOR_PARALLEL",
  "artifact_strategy": "LOCAL_CACHE",
  "verification_strategy": "V1",
  "fallbacks": []
}
```

Target: Merc chooses the least expensive topology that satisfies quality, latency, reliability, and verification requirements.

CATEGORY 5 — PRICING

Audit and revise every layer.

1. Supplier cost.
2. Provider cost.
3. Electricity where supplier-owned.
4. Hardware depreciation where applicable.
5. Runtime efficiency.
6. Startup cost.
7. Model-cache residency.
8. Storage.
9. Egress.
10. Verification.
11. Retry reserve.
12. Refund reserve.
13. Payment-processing allocation.
14. Support reserve.
15. Fraud reserve.
16. Merc contribution.
17. Supplier contribution.
18. Buyer savings.
19. Uncached input.
20. Reused input.
21. Exact cache.
22. Generated output.
23. Reused result.
24. Training attempt fee.
25. Training success fee.
26. SLA premium.
27. Spot discount.
28. Reserved capacity.
29. Multi-GPU premium.
30. Market reference quality.
31. Quote confidence.
32. Actual-versus-estimated reconciliation.

Pricing must satisfy:

```text
supplier_or_provider_cost covered
Merc contribution positive
buyer price competitive where sustainably possible
no hidden subsidy
currency explicit
```

Do not set prices from the minimum of unqualified market observations.

Use governed, normalized, confidence-weighted evidence.

Require a 10/10 pricing audit before launch.

A 10/10 pricing score means:

* no known conservation bug;
* no orphaned accrual;
* no unbounded retry;
* no cross-currency arithmetic;
* no negative unsponsored profile;
* no misleading take-rate claim;
* no stale unsupported competitor input;
* quote error measured;
* margins measured per workload;
* refund and transfer behavior tested;
* pricing decisions explainable and receipted.

CATEGORY 6 — EXECUTION

1. Admission.
2. Queueing.
3. Leasing.
4. Runtime launch.
5. Artifact acquisition.
6. Model loading.
7. Warm state.
8. Streaming.
9. Cancellation.
10. Timeout.
11. Retry.
12. Fallback.
13. Partial result.
14. Checkpoint.
15. Idempotency.
16. Worker failure.
17. Runtime failure.
18. OOM.
19. Multi-GPU failure.
20. Output finalization.
21. Artifact upload.
22. Resource teardown.
23. Orphan detection.
24. Cost control.

Every execution lane must use real runtime evidence.

CATEGORY 7 — PARALLEL EXECUTION

Create separate proof for:

1. Realtime request concurrency.
2. Continuous batching.
3. Independent batch splitting.
4. Shared-prefix batch.
5. Embedding sharding.
6. Image sample parallelism.
7. Video frame/segment parallelism.
8. Blender frame/tile parallelism.
9. LoRA data parallelism where supported.
10. Tensor parallelism.
11. Pipeline parallelism.
12. Expert parallelism.
13. Multi-host coordination.
14. Result aggregation.
15. Failure of one shard.
16. Partial retry.
17. Settlement across multiple suppliers.
18. Verification of aggregated output.

Do not treat tensor_parallel_size stored in a profile as implementation proof.

Prove at least single-host multi-GPU physically through RunPod.

Treat public-internet cross-owner model sharding as experimental unless latency, reliability, trust, and economics pass.

Prefer splitting independent tasks over splitting one tightly coupled model across unrelated suppliers.

CATEGORY 8 — VERIFICATION

1. Runtime identity.
2. Model identity.
3. Input commitment.
4. Stream commitment.
5. Output commitment.
6. Usage reconciliation.
7. Artifact validity.
8. Schema validation.
9. Deterministic replay.
10. Sample replay.
11. Redundant execution.
12. Buyer evaluator.
13. Fine-tune evaluation.
14. Render verification.
15. Multi-shard aggregation.
16. Supplier fraud.
17. Honeypots.
18. Verification cost.
19. Dispute authority.
20. Receipt verification.

Target: a supplier cannot create settlement authority by reporting success.

CATEGORY 9 — SETTLEMENT

1. Buyer authorization.
2. Buyer ledger debit.
3. CAD currency authority.
4. Supplier payable.
5. Provider cost.
6. Platform contribution.
7. Verification charge.
8. Storage/egress.
9. Refund.
10. Credit.
11. Transfer.
12. Payout.
13. Reversal.
14. Dispute reserve.
15. Chargeback.
16. Retry.
17. Manual review.
18. Idempotency.
19. Reconciliation.
20. Multi-supplier allocation.
21. Rounding.
22. Microunit conservation.

Use CAD for sandbox.

Do not block test authority on USD.

Keep currency explicit so live currency can change later.

CATEGORY 10 — DEVELOPER EXPERIENCE

1. OpenAI compatibility.
2. Python SDK.
3. TypeScript SDK.
4. Streaming.
5. Idempotency.
6. Typed errors.
7. Artifact upload.
8. Endpoint management.
9. Model onboarding.
10. Fine-tune workflow.
11. Receipts.
12. Webhooks.
13. Examples.
14. CLI.
15. API stability.
16. Migration path.
17. Documentation accuracy.

CATEGORY 11 — BUYER APPLICATION

1. Sign-up and organization.
2. Projects.
3. API keys.
4. Models.
5. Endpoints.
6. Jobs.
7. Artifacts.
8. Fine-tunes.
9. Media generation.
10. Spend.
11. Budgets.
12. Receipts.
13. Refunds.
14. Disputes.
15. Webhooks.
16. Usage and performance.

CATEGORY 12 — SUPPLIER APPLICATION

1. Enrollment.
2. Hardware.
3. Agent installation.
4. Benchmark.
5. Profiles.
6. Cache.
7. Jobs.
8. Utilization.
9. Earnings.
10. Payables.
11. Payouts.
12. Reputation.
13. Failures.
14. Drain.
15. Maintenance.
16. Economics calculator.

CATEGORY 13 — OPERABILITY

1. Production deployment.
2. DNS.
3. TLS.
4. Health.
5. Readiness.
6. Version.
7. Database.
8. Object storage.
9. Backups.
10. Restore.
11. Alerts.
12. Paging.
13. Runbooks.
14. Status.
15. Rollback.
16. Soak.
17. Incident exercise.
18. Cost monitoring.
19. Orphan cleanup.
20. Capacity control.

CATEGORY 14 — SECURITY

1. Authentication.
2. Authorization.
3. Default deny.
4. Tenant isolation.
5. Worker isolation.
6. Artifact isolation.
7. Secret handling.
8. Webhook security.
9. SSRF.
10. Egress.
11. Model safety.
12. Remote code.
13. Supply chain.
14. Dependency scanning.
15. Fuzzing.
16. Mutation tests.
17. Fraud defense.
18. Adversarial review.
19. Independent penetration test.
20. Retest.

CATEGORY 15 — LEGAL AND GOVERNANCE

1. Merc naming.
2. Repository licensing.
3. SDK/agent licensing.
4. Model licensing.
5. Dataset rights.
6. Privacy.
7. Buyer terms.
8. Supplier terms.
9. Refund policy.
10. Stripe structure.
11. Tax.
12. Payouts.
13. Attribution.
14. Retention.
15. Incident notification.

SCORING

For every category and subcategory, the reviewer must give:

* score 0–10;
* evidence;
* status;
* defect;
* proposed solution;
* implementation cost;
* expected score after implementation;
* dependency;
* external blocker if any.

Score meanings:

0: absent
1–2: conceptual or broken
3–4: partial implementation
5: usable in controlled development
6: tested but incomplete
7: real-runtime proven
8: canary-worthy
9: production-grade
10: no known material change remains under current scope

A score of 10 requires independent adversarial review and evidence that further obvious changes are not available.

Do not assign 10 because the checklist is long.

COMPETITIVE PERFORMANCE PROGRAM

The objective is not to manufacture a universal “50× better” claim.

The objective is to discover and prove specific workload classes where Merc delivers order-of-magnitude advantages.

Create competitor-normalized benchmarks for:

1. open chat;
2. high-concurrency chat;
3. batch generation;
4. shared-prefix batch;
5. repeated eval sweeps;
6. retry storms;
7. structured extraction;
8. corpus embedding;
9. exact duplicate bursts;
10. LoRA outcome contracts;
11. image batches;
12. rendering.

Compare:

* buyer price;
* physical compute;
* delivered work;
* latency;
* success;
* verification;
* refunds;
* supplier economics;
* total operating burden.

Target ladders:

* 2× better where commodity execution dominates;
* 5× better through routing, batching, and reuse;
* 10× better on eligible repeated-work workloads;
* 50× better only where exact coalescing, cache reuse, outcome settlement, or task specialization genuinely supports it.

Every claim must state:

* workload;
* model;
* hardware;
* runtime;
* quality tier;
* cache/reuse;
* physical work;
* delivered work;
* competitor basis;
* benchmark date;
* receipt.

A verified 10× cost advantage on one valuable workload is better than a false 50× platform-wide claim.

RUNPOD EXPERIMENT PROGRAM

Use RunPod aggressively but with budget limits.

Experiments:

1. Minimal CUDA diagnostic pod.
2. Merc agent enrollment.
3. Single-GPU vLLM.
4. Direct-vLLM versus Merc parity.
5. Embeddings.
6. Batch.
7. Image generation.
8. LoRA training.
9. Tensor-parallel multi-GPU.
10. GPU cost/performance tournament.
11. Worker failure.
12. Pod startup latency.
13. Model-cache effect.
14. Artifact transfer.
15. Orphan cleanup.

For every pod:

* immutable image;
* exact configuration;
* maximum budget;
* startup timeout;
* idle timeout;
* automatic teardown;
* logs;
* cost;
* result.

Do not create unlimited pods.

RUNTIME TOURNAMENT

For each high-value profile, compare where available:

* MLX;
* llama.cpp;
* upstream vLLM;
* TensorRT-LLM;
* owned Merc runtime.

Select based on:

* quality;
* TTFT;
* ITL;
* throughput;
* energy;
* provider cost;
* startup;
* model breadth;
* reliability;
* maintenance.

Do not replace vLLM globally merely because another engine wins one profile.

PRICING ALGORITHM FINALIZATION

Build a Pricing Decision object that binds:

```json
{
  "compute_plan": "...",
  "placement_plan": "...",
  "supplier_cost": 0,
  "provider_cost": 0,
  "verification_cost": 0,
  "storage_cost": 0,
  "egress_cost": 0,
  "payment_cost": 0,
  "risk_reserve": 0,
  "Merc_contribution": 0,
  "buyer_price": 0,
  "market_reference": [],
  "currency": "CAD",
  "confidence": 0,
  "policy_revision": "..."
}
```

Require invariant tests:

* price never below sustainable floor without subsidy;
* reuse never costs more than uncached;
* exact cache is cheaper than fresh execution;
* supplier/provider cost covered;
* Merc contribution positive;
* currency never mixed;
* rounding cannot overcharge;
* quote and settlement reconcile;
* stale market observations cannot dominate;
* one low-quality source cannot set the market;
* refunds cannot exceed original charge;
* supplier payable cannot exceed verified authority.

Ask the external reviewer to perform a final independent pricing audit and attempt to find any remaining lever or defect.

Do not declare 10/10 until it fails to find a material unaddressed change and the findings are independently checked.

FINAL SHIPPABILITY REQUIREMENTS

Merc is shippable only when the following are at least CANARY_PROVEN:

* realtime OpenAI-compatible inference;
* batch generation;
* embeddings;
* object storage;
* model cache;
* image generation;
* LoRA training and independent evaluation;
* adapter deployment;
* external-model validation;
* single-host multi-GPU;
* buyer dashboard;
* supplier console;
* Python SDK;
* TypeScript SDK;
* receipt-backed price board;
* CAD Stripe sandbox;
* supplier payable;
* refund;
* dispute;
* production deployment;
* backup restore;
* alert delivery;
* incident handling;
* security review.

Independent pentest and legal approvals may remain EXTERNALLY_BLOCKED but must have a complete package ready.

FINAL CANARY

Run one comprehensive canary:

1. OpenAI client sends realtime request.
2. Merc classifies workload.
3. Compute plan generated.
4. Price generated.
5. RunPod worker selected.
6. vLLM executes.
7. Stream delivered.
8. Usage reconciled.
9. CAD buyer debit recorded.
10. Supplier payable recorded.
11. Merc contribution positive.
12. Receipt verifies.
13. Batch runs.
14. Embeddings run.
15. Image generation runs.
16. LoRA trains and evaluates.
17. Adapter deploys.
18. Multi-GPU model runs.
19. Failure refunds.
20. Worker crashes and recovers.
21. Backup restores.
22. Alert reaches paging destination.
23. Dispute packet generates.
24. No orphan pod remains.
25. Every cost reconciles.

Run a sustained mixed-workload soak.

AGGRESSIVE RULES

* Prefer real execution over more documentation.
* Prefer fixing the current highest-value blocker over adding breadth.
* Use external review for continuous independent review.
* Verify the review.
* Split parallel work safely.
* Use isolated databases and worktrees.
* Do not let agents interfere with one another.
* Delete dead code.
* Replace mocks with real paths.
* Keep CI and race tests green.
* Preserve money conservation.
* Preserve receipt verification.
* Record failed experiments.
* Tear down paid infrastructure immediately after use.
* Do not ask permission for ordinary work.
* Do not inflate scores.
* Do not inflate benchmarks.
* Do not claim implementation from fields, schemas, or docs alone.
* Do not start the minimum-LOC refactor until shipping behavior is frozen.

REPORTING FORMAT

After every constructive cycle:

1. Category attacked.
2. Previous grade.
3. New grade.
4. External-review findings.
5. Independent verification.
6. Implementation.
7. Real resource used.
8. Product path completed.
9. Performance.
10. Quality.
11. Buyer price.
12. Supplier/provider cost.
13. Merc contribution.
14. Receipt.
15. Failures.
16. Remaining defects.
17. Next tranche.

FINAL STOP CONDITION

Stop only when:

* all launch-critical categories are 8 or higher;
* pricing, money correctness, verification integrity, and execution authority are 9 or higher;
* any category scored 10 has survived independent external challenge and direct verification;
* all product lanes are canary-proven;
* at least one real workload demonstrates a sustainable 10× advantage over a normalized alternative;
* any 50× claim is tied to a specific qualified workload and genuine work elimination or outcome efficiency;
* no known material high-leverage change remains;
* all remaining actions require external human, legal, account, or production approval;
* one consolidated external-action queue is produced;
* the complete behavioral truth is frozen for the later deep minimum-LOC refactor.

The objective is not to finish a checklist.

The objective is to make Merc a complete, aggressively verified, economically superior execution platform whose pipeline makes correct decisions from request intake through settlement and whose strongest claims survive independent attack.

```
```
```

<!-- auto-closed unbalanced fence from docs/PROGRAMME.md -->


<!-- source: docs/PATH_TO_TEN.md -->

# Path to 10 — what an agent can finish alone, and what only you can do

> **Historical planning snapshot — superseded on 2026-07-28.** The RunPod
> credential/proof and Decision Zero statements below no longer describe the
> repository: real CUDA/vLLM evidence exists and `KEEP-RT` was executed. Use
> `EXTERNAL_OPERATOR_HANDOFF.md`, `SHIPPABILITY_STATUS.md`, and
> `ops/go-no-go.json` for the current gates. This file is retained as an audit
> trail, not an operator checklist.

**Method:** The machine and product halves were planned in separate passes under
one constraint — *only work needing no human decision, credential, or
approval goes in the plan body*. Everything else is quarantined at the bottom.

**Read this first: 10 is not reachable, and no plan should promise it.** Three of
these facets are capped by having no users, and one is capped by not owning
hardware. The honest target is **≈7 autonomously, ≈8.5 once your list is done**,
and the last stretch is demand, which is not a commit. What follows is the most
that can truthfully be built.

---

## Ceiling analysis

| Facet | Now | Max alone | Max after your list | What caps it |
|---|---:|---:|---:|---|
| Claims integrity | 8 | **9** | 9.5 | External attestation, real page receipt |
| Money safety | 7 | **9** | 9.5 | Stripe test-mode matrix needs a full sandbox secret set |
| Architecture | 6.5 | **9** | 9 | Only product-surface decisions cap it; nothing human-blocked |
| Security | 7 | **8** | 9 | External pen-test; production IAM and real CIDRs |
| Operability | 6 | **8** | 9 | A real paging destination and a real on-call human |
| Legal & governance | 6 | **7** | 9 | Repo visibility, LICENSE choice, counsel |
| DX & support | 5 | **7** | 8.5 | PyPI publish is blocked on the licence; support contact is a person |
| Inference performance | 3.5 | **6** | 8 | vLLM-on-CUDA parity needs a working GPU credential |
| Unit economics | 5 | **6.5** | 7 | A price is only validated by someone paying it |
| Competitive position | 1.5 | **2.5** | 3 | Demand is not a commit |
| **Overall** | **5.5** | **≈7** | **≈8.5** | |

---

## The GPU question, answered

You asked whether we can connect to RunPod for hardening. Three findings:

**1. The stored RunPod credential is dead.** `.secrets/runpod.env` holds a
50-character `RUNPOD_API_KEY`. It returns **HTTP 401** against
`rest.runpod.io/v1/pods` and an error envelope from the GraphQL endpoint. The
integration is writable — their API is plain HTTPS and needs no CLI — but it
cannot authenticate today. A replacement key is a 30-second action and it is on
your list.

**2. Most of what the GPU run was *for* can be done without one, today.** The
audit's sharpest performance finding was that the realtime lane had *never
executed against a real engine* — its only evidence file records
`upstream: "httptest fake SSE server"`. That is fixable locally right now:

- `llama-server` is installed, and **the exact GGUF the catalogue prices**
  (`Llama-3.2-1B-Instruct-Q4_K_M`) is already cached on this machine.
- I started it and drove it: real SSE streaming, correct `data: [DONE]`
  terminator, and **usage in the final chunk** — which is precisely what
  `control/realtime.go` forces via `stream_options.include_usage` and settles
  on. It reported 358 tok/s predicted on Metal.

So the gateway, the contract lifecycle, settlement from real usage, and SDK
conformance can all be proven against a genuine third-party OpenAI-compatible
server serving the production model. That removes the "fake upstream" finding
entirely. It does **not** produce a vLLM-on-CUDA parity number.

**3. Docker is now available.** `colima` was installed but not running; I started
it, so `docker` works. That unlocks the local Prometheus/Alertmanager fire →
receive → resolve loop and a production-shaped restore drill.

**Net:** performance can reach ~6 alone. Reaching 8 needs one working GPU
credential — and with one, the whole run is scriptable end to end (provision,
serve the pinned `vllm/vllm-openai` image, run
`scripts/realtime-parity-benchmark.py --attest-real-vllm`, tear down), because
the failure mode that matters is forgetting to destroy the pod, and that is
automatable.

---

## Plan — autonomous phases, ordered by (score gained) ÷ effort

### Phase 1 — Prove the money design (~4 d) · money 7→9

The largest single gap anywhere in the project. **14 of 15** money and scheduling
entry points still have zero test callers; only `ClaimTasksTx` is covered. The
design is now good and almost none of it is proven.

1. **One shared fixture** (~0.5 d) — a single seeding helper for buyer, active
   supplier + worker with authorized capability, job with a valid economic-plan
   snapshot, held ledger credit, charged job. Reuse the two existing dialects in
   `scheduler_ask_claim_integration_test.go` and `dispute_payout_integration_test.go`;
   do not invent a third.
2. **`SubmitJobTx`** — commits job + tasks + plan; plan/task-count mismatch fails
   closed with no job row; **submit must mint no money**.
3. **`CompleteTaskTx`** — wrong worker, wrong attempt, already-terminal all fail
   closed; concurrent double-complete yields exactly one success.
4. **`FinalizeJobTx` / `completeJobEconomics`** — `actual_usd` equals the sum of
   buyer-charge rows; a second finalize does not double-insert the SLA premium.
5. **`reservePayoutFunding` / `AuthorizePayoutSubsidy` / `FinalizePayout`** —
   under-fund and double-pay are the failure modes; test them under concurrency.
6. **`resolveDispute`** — freeze blocks payout; terminal resolution controls it.
7. **Property test on the ledger writer** — random micro amounts round-trip
   through `NUMERIC(12,6)` exactly.

*Needs: local Postgres only.*

### Phase 2 — Kill the fake upstream (~1.5 d) · performance 3.5→6, claims 8→8.5

8. Stand `llama-server` up on the cached GGUF as a managed test fixture.
9. Repoint `scripts/realtime-openai-{node,python}-conformance` and the realtime
   integration tests at it; emit evidence with `real_runtime_executed: true` and
   the engine's real identity.
10. Measure and record **actual** gateway overhead p50 against a real engine —
    the `<10 ms` figure in `PRICE_BOARD_METHOD.md` has never been measured.
11. Fix `realtime.go`'s `maxInputTokens := int64(len(upstreamBody))` — byte count
    used as token count, so a 32,768-token context is really ~7,000.
12. `temperature` is accepted and silently discarded by the executor
    (`argmax`). Implement sampling or reject the parameter; silent acceptance is
    the worst of the three.

*Needs: the local engine. No network, no credential.*

### Phase 3 — Operability proven, not just configured (~2 d) · operability 6→8

13. Bring up Prometheus + Alertmanager under the now-working docker, fire a
    synthetic alert, receive it at a local sink, resolve it, and store delivery
    IDs. That converts "alerting configured" into "alerting demonstrated".
14. Restore drill against ≥10k jobs / ≥1k ledger rows / ≥1k objects with a
    **measured** RTO in the receipt; fail the drill on toy row counts.
15. Refuse a corrupted backup envelope, and prove it.

*Needs: docker (now available) and local Postgres/MinIO.*

### Phase 4 — Architecture (~3 d) · architecture 6.5→9

16. `createJob` is still 477 lines — extract the remaining store-dependent
    stages behind narrow interfaces.
17. Put `Server` behind a narrow store port so the 45 handlers are testable
    without live Postgres and MinIO.
18. Split `package main` (142 files) into domain packages. Do this **after**
    Phase 1, so the money paths are protected by tests before they move.

### Phase 5 — DX and economics (~2.5 d) · DX 5→7, economics 5→6.5

19. SDK: retries with backoff, typed errors surfacing the new `code`, `py.typed`,
    the missing `example.py`. Build and verify the wheel locally — publishing is
    blocked on the licence.
20. Complete the error-code enum across all 264 error sites, not just the
    touched ones.
21. Build `pricing/board.json` from public competitor prices with fetch dates,
    and drive the catalogue from it instead of the cost-plus laptop formula.
22. A quickstart that a stranger can follow to a successful call against a local
    stack, verified by running it in a clean shell.

---

## YOUR LIST — the only things blocking the rest

Ranked by what they unblock per second of your time.

| # | Action | Time | Unblocks |
|---|---|---|---|
| 1 | `gh repo edit joshuahickscorp/merc --visibility private` | 5 s | The only open legal exposure |
| 2 | **A working RunPod API key** (or any GPU provider) into `.secrets/runpod.env` | 30 s | Performance 6→8; real vLLM parity; the CUDA lane's entire evidence base |
| 3 | **Decision Zero** — `[KILL-RT]` or `[KEEP-RT]`, see `docs/ARCHITECTURE.md § "Decision Zero — realtime lane: keep or kill"` | one call | ~Half the remaining roadmap; ends dual-tracking |
| 4 | Choose a LICENSE | minutes | PyPI publish → DX 7→8.5; resolves `NOTICE` contradiction |
| 5 | Real support + incident contacts in `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md` | minutes | `make release-gates`; claims 9→9.5 |
| 6 | A real paging destination (PagerDuty/Opsgenie/Slack webhook) | minutes | Operability 8→9 |
| 7 | Stripe sandbox secret set — you said later, so it is sequenced last | ~1 h | Money 9→9.5 |
| 8 | Counsel on `THIRD_PARTY_LICENSES.md` (both catalogue models are BLOCKED) | external | Legal GO; serving claims |
| 9 | External pen-test | external | Security 8→9 |
| 10 | Trademark call on the `mercmerc.net` collision | external | Brand, before acquisition spend |

**Two things are irreducible and no list closes them:** you have no users, and
you do not own GPUs. Competitive position stays ~3 until someone pays, and
performance stays capped until the hardware the control plane admits is hardware
buyers want. Every item above is worth doing; none of them is a substitute for
those two.

## Not doing, deliberately

**Git history purge.** 5.66 GiB of Blender blobs against a ~8 MB tracked tree.
`git filter-repo` is executable locally but rewrites every SHA on a branch
already published as `origin/release/rc1-go-closure`, and only pays off after a
force-push that breaks every clone and CI cache. Force-push is irreversible
shared state. `.gitignore` already blocks re-entry. Use shallow clones; if you
want this done, it is a human-scheduled operation, not an agent one.


<!-- source: docs/YOUR_10_ACTIONS.md -->

# Getting to 8.5 — a tutorial for the ten things only you can do

> **Historical planning snapshot — superseded on 2026-07-28.** Do not use this
> file as the current release checklist. The RunPod proof was completed and
> Decision Zero was resolved as `KEEP-RT`; see `DECISION_ZERO_REVERSAL.md`.
> Current external actions and release gates are authoritative in
> `EXTERNAL_OPERATOR_HANDOFF.md`, `SHIPPABILITY_STATUS.md`, and
> `ops/go-no-go.json`. This snapshot remains only to preserve decision history.

Everything else is being built for you. This document is the complete list of
what an agent cannot do on your behalf, why, and exactly how to do each one.

**Total hands-on time for items 1–6: about 25 minutes.** Items 7–10 involve
other people and run on their calendars, not yours.

Each item says what it unblocks, so you can stop wherever the return stops being
worth it.

---

## 1. Make the repository private — 5 seconds

**Why an agent won't:** changing the visibility of your GitHub account's
repository is an account setting on an outward-facing service. It is also the
single item where doing nothing has an ongoing cost.

**Why it matters:** the repo is public, has no `LICENSE`, and your own
`NOTICE:11-13` says *"Public source or binary distribution remains blocked
pending an owner-approved project license."* Anyone who has cloned it holds no
grant, and you hold no defensive terms. It also blocks item 4, which blocks PyPI,
which caps developer experience.

```bash
gh repo edit joshuahickscorp/merc --visibility private
```

Check first if you care about forks:

```bash
gh repo view joshuahickscorp/merc --json forkCount,stargazerCount
```

**Unblocks:** the only open legal exposure. Prerequisite for item 4.

---

## 2. A working GPU credential — 30 seconds

**Why an agent won't:** provisioning GPUs spends your money, and the key on disk
is dead so there is nothing to spend.

**What I found:** `.secrets/runpod.env` holds a 50-character `RUNPOD_API_KEY`. It
returns **HTTP 401** from `rest.runpod.io/v1/pods`. Stale or revoked.

**What to do:** create a fresh key at
`runpod.io → Settings → API Keys` (read+write), then:

```bash
printf 'RUNPOD_API_KEY=%s\n' 'YOUR_NEW_KEY' > .secrets/runpod.env
chmod 600 .secrets/runpod.env
```

Any provider works — Lambda, Vast, Modal — the harness is provider-agnostic HTTPS.
Make sure the account has a few dollars of credit; a parity run is roughly one
hour of one A100, about **$2–3**.

**Verify it took:**

```bash
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $(cut -d= -f2- < .secrets/runpod.env)" \
  https://rest.runpod.io/v1/pods
```

`200` means good. `401` means the key is wrong.

**Unblocks:** inference performance 6 → 8. This is the only route to a real
vLLM-on-CUDA parity number, and every performance and price claim in the repo
currently traces back to one hand-typed laptop benchmark.

**Note:** protocol conformance is already proven against a *real* engine without
any GPU — `make real-engine-conformance` runs llama.cpp on the exact model the
catalogue prices and produces
`evidence/realtime/real-engine-conformance.json`. What the GPU adds is the
competitive throughput number, not basic correctness.

---

## 3. Decision Zero — one call, no typing

**Why an agent won't:** it decides what the company sells. Two independent
analyses landed on opposite sides (78% vs 72%) — that overlap means the evidence
genuinely underdetermines it.

**Read:** `docs/ARCHITECTURE.md § "Decision Zero — realtime lane: keep or kill"`. It costs both branches.

**The question that resolves it:** *within 90 days, can you get a Linux/CUDA
supplier to register and serve real traffic?*

- **Yes** → `[KEEP-RT]`. Realtime is the only path to that supply.
- **No** → `[KILL-RT]`. `control/types.go` admits only four Apple hardware
  classes, so an 8×H100 supplier gets HTTP 400 — you cannot sell a latency SLA
  on machines the control plane refuses.

Reply with the branch and I will execute it. The lane is snapshotted with
checksums outside the repo, so either direction is reversible.

**Unblocks:** roughly half the remaining roadmap, and ends paying to maintain
two products.

---

## 4. Choose a licence — 2 minutes

**Why an agent won't:** it grants or withholds rights to your work in perpetuity.

**Current state:** no `LICENSE` file. `agent/Cargo.toml:6` declares `license =
"MIT"` with no MIT text present, so the package metadata and the repository
disagree.

**If MIT is what you meant** (which `Cargo.toml` implies):

```bash
curl -sL https://raw.githubusercontent.com/licenses/license-templates/master/templates/mit.txt \
  | sed "s/{{ year }}/2026/; s/{{ organization }}/Joshua Hicks/" > LICENSE
```

Then delete the "no project-level LICENSE" paragraph from `NOTICE`.

If you want something else — Apache-2.0 for the patent grant, or a source-available
licence to stop a competitor reselling it — say which and I will write it.

**Unblocks:** PyPI publishing → DX 7 → 8.5. Resolves the `NOTICE` contradiction.

---

## 5. Real incident contacts — 5 minutes

**Why an agent won't:** inventing a phone number that pages nobody is worse than
an empty field, because it looks handled.

**Current state:** `make release-gates` fails on this by design:

```
runbook-contacts: FAIL: 6 placeholder contact line(s)
```

Edit `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md:9-14`. Six roles: incident commander,
security/privacy counsel, payments owner, supplier ops, support intake, status
channel. **At this stage they can all be you** — one email and one phone number
that actually reaches you beats six `[CONTACT REQUIRED]` markers.

```bash
make release-gates     # should stop reporting the contacts failure
```

**Unblocks:** the readiness GO gate; claims integrity 9 → 9.5.

---

## 6. A paging destination — 5 minutes

**Why an agent won't:** it is an account on a third-party service, and an on-call
routing policy is a commitment about who wakes up.

Simplest version — a Slack incoming webhook:

```bash
mkdir -p /run/secrets 2>/dev/null || true
printf '%s' 'https://hooks.slack.com/services/XXX/YYY/ZZZ' \
  | sudo tee /run/secrets/cx_alert_receiver_url > /dev/null
```

`ops/monitoring/alertmanager.yml:21` already reads that path. PagerDuty or Opsgenie
work the same way.

**Unblocks:** operability 8 → 9. Turns "alerting configured" into "alerting
delivered", which is the difference between a dashboard and an on-call system.

---

## 7. Stripe sandbox — about 1 hour, deferred as you asked

Sequenced last deliberately. In the Stripe Dashboard, in **test mode**:

1. Create a webhook endpoint → copy `whsec_...` into `STRIPE_WEBHOOK_SECRET`.
2. Create a **second, distinct** endpoint for Connect → `STRIPE_CONNECT_WEBHOOK_SECRET`.
   The validator refuses if both secrets are the same, on purpose.
3. Enable Connect, note the `ca_...` client id.
4. Copy the `sk_test_...` key.

Put them in `.env`. **Never a `sk_live_` key** — the code refuses to start and
the scripts refuse before any network call.

```bash
make stripe-check && make stripe-matrix
```

**Unblocks:** money safety 9 → 9.5. The reversal path is implemented and
simulator-tested but has never met real Stripe.

---

## 8. Counsel on the model licences — external

`docs/THIRD_PARTY_LICENSES.md:31-32` marks **both** catalogue models BLOCKED —
Llama 3.2 1B and all-MiniLM-L6-v2 — while the binary prices and serves them.
Every model you sell is marked blocked in your own register.

`make license-register` fails on this deliberately. **Do not resolve it by
editing the register**; that is the one move that converts a real legal question
into a fake green check.

**Unblocks:** legal GO; the right to make public serving claims.

---

## 9. External penetration test — external

Security is at 7 and reaches 8 autonomously. The last point needs someone who
does not work for you attacking it. The codebase is unusually ready for this: the
webhook egress path has a DNS-rebinding-safe pinned dial, CI runs `govulncheck`,
`gitleaks` and `cargo-audit`, and admin authority is now a separate principal.

**Unblocks:** security 8 → 9.

---

## 10. The name — external, and the clock is running

`mercmerc.net` is a live, funded GPU-compute marketplace. Press, SEO and
enterprise legal review all reach them first. This costs more the longer you
build brand on the current name.

Get a trademark search. The answer may be "proceed anyway" — but make it a
decision rather than a default.

---

## What this buys you

| After | Score |
|---|---|
| Today | 5.5 |
| Autonomous work completing now | ≈7 |
| Items 1–6 (about 25 minutes) | ≈8 |
| Items 7–10 | ≈8.5 |

## What no list closes

**You have no users, and you do not own GPUs.** Competitive position stays around
3 until someone pays you, and inference performance stays capped while the only
hardware the control plane admits is Apple Silicon. Every item above is worth
doing. None of them substitutes for a first paying buyer.

The cheapest honest test of the whole thesis is item 3 plus three design
partners. If three people who are not you will run real jobs, the rest of this
matters. If none will, that is the most valuable thing you could learn, and it
costs a week rather than another quarter of engineering.


<!-- source: docs/EXTERNAL_ACTION_QUEUE.md -->

# External action queue

Everything below needs a human, an account, paid infrastructure, or a decision
that is not the code's to make. Nothing here can be closed by writing software.

For the **readiness facet** specifically (current machine-derived **87/100**,
P1=5; 84/100 is the local-only ceiling before the offsite pair, which has
landed), use the ordered operator checklist in
`docs/PROGRAMME.md § "Facet external action pack"`. This queue remains the
broader external inventory for canary inputs and rename cutovers.

Generated from measured probes, not from intent. Re-derive with:

```bash
set -a; . ./.env; set +a
bash scripts/stripe-sandbox.sh check
# private-canary.json is an unbound capability inventory, not a bound canary receipt
python3 -c "import json;d=json.load(open('evidence/canary/private-canary.json'));print(d['lanes_canary_proven'],'/',d['lanes_total'])"
```

## The headline

**A live Stripe key is not on this list, and cannot be.**
`scripts/lib/go-closure-common.sh:155` *refuses* anything but `sk_test_*` /
`rk_test_*`, and the payment authority reports `live_mode: PROHIBITED`. The
formal canary is test-mode by policy. Supplying a live key today would be
rejected by the very gate that mints release authority.

Measured Stripe state: `api_key: test`, `billing_webhook: webhook`,
`connect_webhook: webhook`, connect test account present, endpoint IDs distinct,
settlement currency `cad`. Every credential the sandbox contract asks for is
present.

Candidate-bound canary lanes: **0 of 21**.

## 1. Staging deployment — **historical blocker; now SATISFIED**

`P1-STAGING` is `SATISFIED` (2026-08-16). Persistent staging serves
`https://mercmerc.net` over public TLS. The go-closure canary rehearsal
still needs the remaining participant/adapter inputs in the table; it is
no longer blocked on the absence of a host.

| Input | What it is |
|---|---|
| `STAGING_TLS_HOSTNAME` | A DNS name with TLS. The only failing field in the Stripe contract check (`staging_hostname_valid: false`), because Stripe webhook endpoints must reach a public HTTPS host. |
| `STAGING_DEPLOYMENT_ROOT` | Where the candidate is deployed and run from. |
| `MERC_CANDIDATE_CONTROL_IMAGE`, `MERC_PRIOR_CONTROL_IMAGE` | Immutable `@sha256:` image references. The rehearsal binds receipts to exact image identity, so a floating tag will not do. |
| `MERC_CANDIDATE_COMMIT`, `MERC_PRIOR_COMMIT` | The exact commits those images were built from. |
| `MERC_PROMETHEUS_IMAGE`, `MERC_ALERTMANAGER_IMAGE`, `MERC_GRAFANA_IMAGE`, `MERC_NODE_EXPORTER_IMAGE` | Pinned observability images. |
| `MERC_GO_CLOSURE_ENV_FILE` | The `.env.go-closure` holding the above. Deliberately gitignored. |

## 2. Off-host backup — blocks the restore lane

Backup and independent restore is a launch-critical lane, and it is the one the
driver refuses hardest, because a local-only restore is not evidence that the
data survived leaving the host.

| Input | What it is |
|---|---|
| `MERC_BACKUP_OFFSITE` | An S3-compatible bucket on different infrastructure from the deployment. |
| `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` | Restricted credentials for that bucket. Do not reuse the artifact-store keys. |
| `MERC_BACKUP_ENCRYPTION_RECIPIENT`, `MERC_BACKUP_DECRYPTION_IDENTITY_FILE` | An `age` keypair. The private identity must not live on the deployment host. |

## 3. Alerting — blocks the paging lane

| Input | What it is |
|---|---|
| `ALERT_RECEIVER_WEBHOOK_URL` | An HTTPS receiver that actually pages a human. |
| `ALERT_RECEIVER_NAME` | Which receiver, for the Alertmanager route. |

The canary fires a real alert and requires the receiver to record both firing and
resolution. A sink that swallows the alert proves nothing.

## 4. Approved canary participants — an operator decision

| Input | What it is |
|---|---|
| `MERC_CANARY_APPROVED_BUYER_EMAILS` | Exactly two buyers who consented to being canary participants. |
| `MERC_CANARY_APPROVED_WORKER_IDS` | Exactly two supplier workers. **These must be real v4 UUIDs.** The demo seed IDs (`00000000-…-b1`) are version-nibble `0`, and the receipt validator correctly refuses them, so `distinct_metal_agent` cannot pass on seeded workers. |
| `MERC_CANARY_APPROVED_AGENT_VERSIONS`, `MERC_CANARY_APPROVED_BUILD_HASHES` | The reviewed agent build the canary is allowed to attribute work to. |
| `MERC_CANARY_APPROVED_DRIVER_SHA256` | The operator-reviewed digest of the scenario driver. Review the driver, then pin its hash — the rehearsal re-checks the bytes before and after every scenario. |

## 5. Secrets that must be minted, not defaulted

| Input | Note |
|---|---|
| `MERC_TOKEN_KEY` | At least 32 unpredictable bytes. **Copy byte-identically at cutover**: `control/crypto.go` derives the AES key as `sha256(value)`, so regenerating it makes every sealed OAuth token and webhook secret in Postgres permanently undecryptable. |
| `MERC_VERIFICATION_SAMPLE_SECRET` | At least 32 unpredictable bytes. Determines which tasks get sampled; a predictable value lets a supplier know when it is unobserved. |

## 6. Rename cutovers that need the environment moved with the code

`scripts/rename-residue-audit.py` reports `RESIDUE=0` — nothing renameable is
left in the repository. What remains is 408 occurrences that each need an
external change first:

- the `ghcr.io/…/computexchange-control` registry package, and the
  `computexchange/control` image tags in CI;
- `CX_*` environment variables on the droplet and in GitHub Actions secrets —
  the code already reads `MERC_*`, so code and environment must move together
  (one such gap was found and closed locally: `CX_CONNECT_WEBHOOK_SECRET`);
- `/opt/computexchange`, `/etc/computexchange`, `/var/lib/computexchange` —
  real directories on the droplet (supplier agent state now uses `~/.merc`,
  with an in-repo migration from the pre-rebrand home directory);
- Prometheus `job="computexchange-control"` and the `ComputeExchange*` alert
  names — Alertmanager fingerprints the label set, so the receiver's filters
  must be updated in the same change or pages vanish silently.

Recorded receipts under `evidence/` keep the old names permanently. They record
what was actually verified, and rewriting them would make the repository claim
it verified an artifact that was never built.

## 7. Human and legal

- Independent penetration test, and the retest after remediation.
- Legal review of buyer and supplier terms, refund policy, and the Stripe
  account structure.
- A named on-call human for the incident runbook — the support contact is still
  a placeholder.
- Non-author review of the release, which the release doctor checks for.

## Not blocked on you

These are code items and are being worked in-repo, listed so the queue is not
mistaken for the whole picture:

- the `409` compute-versus-economic authority disagreement at job submit;
- a `batch_infer` honeypot, which needs a known answer generated on the exact
  engine build — seeding a generic one would disarm the probe;
- image generation, LoRA train/eval, adapter deploy and tensor-parallel
  execution, which are unimplemented rather than blocked.


<!-- source: docs/FACET_EXTERNAL_ACTION_PACK.md -->

# Facet external action pack

**Audience:** a human operator who can hold Stripe, DNS, storage, and approval
authority. You do not need the repository's history — only this file, the
commands it names, and the linked setup docs.

**What this is:** the only remaining work that can move the readiness facet
axis past its current machine-derived score. Code on a development host cannot
close these points. That is intentional.

**What this is not:** a licence to invent receipts, hand-edit
`ops/readiness.json` `earned` fields, or loosen
`scripts/validate-readiness.py`. Hand-typed `earned` values are advisory and
ignored. The score is trustworthy precisely because empty prose cannot raise it.

---

## Current position (recomputed, not asserted)

```bash
python3 scripts/validate-readiness.py
```

Expected at this HEAD (offsite pair present, 24 h soak and Stripe matrix
not, no qualified human approvals):

```text
readiness: PASS (87/100 derived, P0=0, P1=5, Level B NO_GO)
```

On a host with no offsite credential the derived total is 84/100. The P1
count is 5, not 8: `P1-STAGING`, `P1-RECOVERY-SOAK` (alpha exit) and
`P1-OFFSITE-RESTORE` are `SATISFIED`.

| Domain | Derived | Gap | Blocker class |
|---|---:|---:|---|
| `source_and_ci` | 10/10 | 0 | — |
| `security` | 14/15 | 1 | external staging attack rehearsal |
| `money_and_reconciliation` | 9/15 | 6 | Stripe Connect signup + Connect-complete matrix (`tr_` / payout hold/release/failure/reversal). Public TLS host is no longer the gap. |
| `lifecycle_and_concurrency` | 10/10 | 0 | — |
| `artifacts_and_storage` | 8/8 | 0 | independent offsite copy (Cloudflare R2 rehearsal) |
| `agent_and_sandbox` | 8/8 | 0 | — |
| `database_and_recovery` | 8/8 | 0 | isolated offsite restore (Cloudflare R2 rehearsal) |
| `deployment_and_rollback` | 5/8 | 3 | qualifying 24 h soak on persistent staging |
| `observability_and_alerting` | 6/6 | 0 | — (staffed paging remains a release P1, not a facet gap) |
| `privacy_and_data_governance` | 3/4 | 1 | qualified privacy approval / external subprocessor deletion |
| `licensing_and_supply_chain` | 2/3 | 1 | license and asset provenance approval |
| `abuse_and_trust` | 1/2 | 1 | staffed human route or qualified tabletop |
| `support_and_incident_response` | 1/1 | 0 | — |
| `website_and_buyer_usability` | 2/2 | 0 | — |
| **Total** | **87/100** | **13** | remaining external |

**Machine-reachable ceiling without staging/approvers: 87/100** once the
already-configured Cloudflare R2 rehearsal (`make offsite-independent-restore`)
passes. On a host that also lacks that offsite credential, the ceiling is
84/100. Neither figure is underachievement; remaining gaps are Stripe,
soak, public-hostname rehearsal, and human approvals.

### How the remaining 13 points are represented in the scorer

`DOMAIN_RECEIPTS` in `scripts/validate-readiness.py` fixes each domain's
`possible` total. Earned points are the sum of **wired** receipt rows that
exist on disk and pass their content check. The remaining 13 points each have
a receipt row under `evidence/external/` with a content check as strict as
`alert_delivery_proven`. Inventing a `status: PASS` stub will not pass the
check. The offsite pair is present and passing after
`make offsite-independent-restore`.

| Domain | Pts | Wired path |
|---|---:|---|
| `money_and_reconciliation` | 6 | `evidence/external/stripe-sandbox-matrix.json` (unbound historical snapshot; status BLOCKED on Connect signup; does not prove the 6 points) |
| `deployment_and_rollback` | 3 | `evidence/external/qualifying-soak-24h.json` (+ raw samples JSONL named in the receipt) |
| `artifacts_and_storage` | 2 | `evidence/external/offsite-backup-verification.json` (present; R2 rehearsal) |
| `database_and_recovery` | 1 | `evidence/external/offsite-independent-restore.json` (present; R2 rehearsal) |
| `security` | 1 | `evidence/external/staging-attack-rehearsal.json` |
| `privacy_and_data_governance` | 1 | `evidence/external/privacy-qualified-approval.json` |
| `licensing_and_supply_chain` | 1 | `evidence/external/licensing-provenance-approval.json` |
| `abuse_and_trust` | 1 | `evidence/external/staffed-abuse-route-or-tabletop.json` |

So the operator loop for each gap is:

1. Obtain the credential, host, approval, or rehearsal this pack names.
2. Run the exact command or gate listed.
3. Retain the real evidence artifact at the wired path above (never paste
   secrets into git or chat). Shape must satisfy the content check in
   `scripts/validate-readiness.py`.
4. After the score moves, update `ops/go-no-go.json` `readiness_score` to the
   new derived total (the validator fails closed if they disagree).

Until genuine evidence lands at those paths and passes the content checks,
`validate-readiness.py` prints the derived total (87/100 with the R2
offsite pair; 84/100 without it). The P1 exit criteria in
`ops/go-no-go.json` and `make release-doctor` still close on real evidence;
the facet score moves only when a content-checked external receipt is present.

Never run live Stripe keys. Live mode is refused by contract and prohibited for
Level C.

---

## Recommended order (points per unit of human effort)

Do work in this order. Rationale is effort and sequencing, not ceremony.

| Order | Domain(s) | Pts | Why first / next |
|---:|---|---:|---|
| **1** | `money_and_reconciliation` | **6** | Highest absolute return. Public HTTPS staging exists. Remaining wall is Connect signup on `acct_1TxbzMCwPLrR4vaY`, then `make stripe-matrix` until `stripe_sandbox_matrix_proven` accepts a PASS. |
| **2** | `licensing_and_supply_chain` | **1** | Pure paper. Provenance register and named reviewer only. |
| **3** | `privacy_and_data_governance` | **1** | Also paper / counsel. Can run in parallel with (2). |
| **4** | `artifacts_and_storage` + `database_and_recovery` | **0 remaining** | **Done.** Offsite pair is `PASS` / `BOUND` and already in the 87. Listed so this table is not mistaken for a to-do. |
| **5** | `deployment_and_rollback` | **3** | Persistent staging exists. Remaining is the qualifying ≥86400 s soak. The 3600 s alpha soak does not award these 3 points. |
| **6** | `security` | **1** | External named-reviewer attack rehearsal against the public TLS host. The local 1551-attack rehearsal is `qualification: LOCAL` and does not score this point. |
| **7** | `abuse_and_trust` | **1** | Staffed human route or qualified multi-role tabletop. |

**Do first if you can only do one thing:** Connect signup, then the Stripe
matrix. Six points, and the remaining `ALPHA_BLOCKER` P1.

**Do not start with** the abuse tabletop or a lone security drill if Stripe
is still open — one staffed meeting buys 1 point; the same afternoon on
Connect + webhooks buys 6.

Shared prerequisite note: items 1, 5, and 6 share the already-standing
persistent TLS host. Item 4 is closed. Items 2 and 3 need neither.

---

## 1. `money_and_reconciliation` — 6 points (do first)

### What is required

| Input | Form | Who provides it |
|---|---|---|
| `STAGING_TLS_HOSTNAME` | DNS name with valid public HTTPS that Stripe can POST to | DNS / infra owner for the private-canary zone |
| `MERC_CONNECT_CLIENT_ID` | Sandbox Connect client id `ca_…` | Payments owner in **Stripe Dashboard → Connect → Settings** (dashboard-only; not fetchable from the API) |
| Billing webhook endpoint | Enabled `we_…` at `https://<host>/v1/stripe/webhook` | Payments owner; must pin API version `2025-06-30.basil` (null / mismatched versions fail closed; Stripe cannot update `api_version` in place — recreate) |
| Connect webhook endpoint | Distinct enabled `we_…` at `https://<host>/v1/stripe/connect-webhook` | Same; distinct `whsec_…` secret from billing |
| `STRIPE_SECRET_KEY` | `sk_test_*` or scoped `rk_test_*` only | Payments owner; live prefixes are refused before any network call |
| `STRIPE_TEST_CONNECTED_ACCOUNT_ID` | Disposable Canadian `acct_…` with payouts enabled | Payments owner |
| Endpoint ids + secrets | `STRIPE_BILLING_WEBHOOK_ENDPOINT_ID`, `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`, `STRIPE_WEBHOOK_SECRET`, `MERC_CONNECT_WEBHOOK_SECRET` | From the recreated endpoints |

Store values only in gitignored mode-0600 `.env.go-closure` (or the approved
secret manager). Never commit them. Full input list:
`docs/ARCHITECTURE.md § "Stripe Sandbox setup"`, `ops/go-closure-inputs.json`.

### Commands / gate

```bash
cp ops/staging/env.go-closure.example .env.go-closure   # if not already
chmod 600 .env.go-closure
# fill STAGING_TLS_HOSTNAME, Stripe test inputs, MERC_CONNECT_CLIENT_ID, …

# Recreate both webhook endpoints against the live hostname with API version
# 2025-06-30.basil (see scripts/stripe-webhooks.sh and docs/ARCHITECTURE.md § "Stripe Sandbox setup").
# Stale tunnel hostnames and api_version:null endpoints fail the contract.

set -a; . ./.env.go-closure; set +a
make stripe-check     # scripts/stripe-sandbox.sh check
make stripe-matrix    # scripts/stripe-sandbox.sh matrix
```

`stripe-check` must report test key class, distinct endpoint ids, Connect test
account present, `staging_hostname_valid: true`, `live_mode: PROHIBITED`.
`stripe-matrix` runs the bundled CAD provider scenarios (success, decline,
refund, dispute ordering, Connect restriction, payout hold/release/failure,
webhook signature and replay) and prints a sanitized
`kind:"stripe_sandbox_matrix"` receipt with `status:"PASS"`.

Related release gate: `P1-STRIPE-TEST` in `ops/go-no-go.json`.

### How to verify the facet moved

After a Connect-complete PASS is written (the current
`evidence/external/stripe-sandbox-matrix.json` is an unbound
historical snapshot, status BLOCKED, and does not prove the facet)
and that file then passes `stripe_sandbox_matrix_proven`:

```bash
python3 scripts/validate-readiness.py
# expect: money_and_reconciliation: derived=15/15
# expect: readiness: PASS (93/100 …)   # current 87 + 6, not 84 + 6
```

Until that file lands with the full matrix shape, the matrix can still pass and
close the payments P1 while the facet stays at 9/15. Do not hand-type
`earned: 15`.

---

## 2. `licensing_and_supply_chain` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| License / asset provenance approval | Named reviewer resolves every `BLOCKED_*` row in `ops/asset-provenance.json` and model provenance | Licensing / brand / counsel authority listed in the governance packets |
| Model provenance | Same bundle exercise `asset_and_model_provenance` | Same |
| Project license clarity | Already present as `LICENSE`; provenance gaps are the open item | Repository owner + counsel |

Register path: `ops/asset-provenance.json` (status today
`BLOCKED_INCOMPLETE_PROVENANCE`). Review packet:
`ops/governance-review-packets.json`. Bundle shape:
`ops/governance-approval-bundle.schema.json` → `approvals.licensing` and
`exercises.asset_and_model_provenance`.

### Commands / gate

```bash
# After the restricted governance bundle is completed outside git:
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# equivalent: cd control && go run . release approvals-check --bundle <path>
make release-doctor
```

Related release gate: portion of `P1-GOVERNANCE`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: licensing_and_supply_chain: derived=3/3
# total +1 from the previous derived score
```

---

## 3. `privacy_and_data_governance` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Qualified privacy approval | Named privacy counsel / DPO approval of roles, purposes, retention, transfers | Privacy authority |
| External subprocessor deletion | Evidence that a real subprocessor deletion (or approved absence) was exercised — not only the local technical DSAR/tombstone path | Privacy + engineering |

Technical DSAR/deletion/tombstone already scores 3/4 via
`evidence/autonomous/technical-exercises.json` (unbound local technical receipt;
not qualified external evidence). The remaining point is the **qualified** half.

Bundle: `approvals.privacy` and `exercises.dsar_export_deletion` (qualified) in
the governance approval bundle. Context: `docs/PRIVACY_DATA_GOVERNANCE.md`,
`docs/DSAR_RUNBOOK.md`, `ops/legal-review.json` (`PRIV-001` still open).

### Commands / gate

```bash
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
make release-doctor
```

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: privacy_and_data_governance: derived=4/4
# total +1
```

---

## 4. `artifacts_and_storage` (2) + `database_and_recovery` (1) — 3 points together

Do these as one rehearsal. Local logical restore already earned the in-host
points (`logical-independent-restore.json`). What remains is an **independent
provider boundary**.

### What is required

| Input | Form | Who provides it |
|---|---|---|
| `MERC_BACKUP_OFFSITE` | `s3://…` bucket on infrastructure **different** from the deployment host | Storage / backup administrator |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Restricted to that bucket; **not** the artifact-store keys | Same |
| `MERC_BACKUP_ENCRYPTION_RECIPIENT` | `age1…` public recipient | Backup owner |
| `MERC_BACKUP_DECRYPTION_IDENTITY_FILE` | Path to mode-0600 age identity **not** stored on the deployment host | Backup owner |

### Commands / gate

```bash
make offsite-independent-restore-check
make offsite-independent-restore
# Maps already-configured .merc-secrets.env R2_* keys onto MERC_BACKUP_*.
# Uploads only age ciphertext to s3://<R2_BUCKET>/offsite-alpha/<backup_id>,
# independently re-downloads and hashes, restores into a new isolated
# Postgres/MinIO, and writes:
#   evidence/external/offsite-backup-verification.json
#   evidence/external/offsite-independent-restore.json
# Does not dump or destroy the live droplet volume.
```

Expect a schema-v2 encrypted offsite backup manifest bound to the exact offsite
URI, a verification receipt with independently downloaded checksums, and a
restore that matches database/object invariants. Local-only restore is not
enough; the validator and P1 text require the independent download.

Related release gate: `P1-OFFSITE-RESTORE`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: artifacts_and_storage: derived=8/8   (+2)
# expect: database_and_recovery: derived=8/8   (+1)
# total +3 once both receipt paths are wired to the offsite evidence
```

---

## 5. `deployment_and_rollback` — 3 points

Local staging validation, rollback, and restart-storm receipts already score
5/8. The remaining 3 require a **qualifying 24-hour soak on persistent staging**
with the exact candidate image.

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Persistent staging stack | Host + TLS + compose state from `docs/RUNBOOKS.md § "Staging deployment command plan"` | Operations |
| `MERC_CANDIDATE_CONTROL_IMAGE` / `MERC_CANDIDATE_COMMIT` | Immutable `@sha256:` image and matching full commit | Release engineering |
| Wall-clock window | ≥ 86400 s uninterrupted candidate container | Operations |
| Soak bounds | `MERC_SOAK_MAX_*` growth limits as in go-closure env | Operations |

A local `make soak-24h` on a laptop is not a substitute. Qualifying mode in
`scripts/go-closure-soak.sh` refuses durations under 86400 s and requires the
persistent candidate control container. The wired 60 s local soak receipt is
intentionally worth **0** points so short local runs cannot inflate the domain.

### Commands / gate

```bash
# After candidate is live on staging (see docs/RUNBOOKS.md § "Staging deployment command plan"):
scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
curl -sf "https://${STAGING_TLS_HOSTNAME}/healthz"
curl -s  "https://${STAGING_TLS_HOSTNAME}/readyz" \
  | jq '{status,payment_mode,live_value_movement}'
# require: status ready, payment_mode test, live_value_movement false

scripts/go-closure-soak.sh --target ssh --duration 86400 --interval 60 --execute
# retain the schema-v2 qualifying soak receipt the script emits
```

Related release gates: `P1-STAGING` and the alpha exit of `P1-RECOVERY-SOAK`
are `SATISFIED`. The 24-hour clause of `P1-RECOVERY-SOAK` remains the
Level B/C bar.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: deployment_and_rollback: derived=8/8
# total +3 once the qualifying soak receipt is wired
```

---

## 6. `security` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| External staging attack rehearsal | Hostile exercise against the real staging TLS surface (authz, cross-tenant, break-glass under staging conditions), with a written receipt | Security owner / external reviewer |
| Staging host | Same `STAGING_TLS_HOSTNAME` boundary as above | Operations |

Local technical break-glass and the authorization matrix already score 14/15.
The missing point is explicitly the **external** rehearsal (see comment on
`security` in `DOMAIN_RECEIPTS`).

### Commands / gate

Run the approved external rehearsal against staging; preserve a redacted receipt
outside secret material. Include the security exercise in the governance bundle:

```bash
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# exercises.security_tabletop must be the qualified/external half, not only
# the local technical tabletop already reflected in technical-exercises.json
```

Related release gate: portion of `P1-GOVERNANCE` / independent security review.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: security: derived=15/15
# total +1
```

---

## 7. `abuse_and_trust` — 1 point (do last among equals)

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Staffed human abuse route **or** qualified multi-role tabletop | Named humans on a real escalation path, or a timed tabletop with trust/safety + support + security roles | Trust & safety + support owners |
| Contacts | Non-placeholder contacts in the support/incident runbook where the release gates require them | Same |

Local technical tabletops already score 1/2 via
`evidence/autonomous/technical-exercises.json` (unbound local technical receipt;
`qualified_human_tabletop: NOT EXECUTED` in the derived technical receipt).

### Commands / gate

```bash
# After the staffed route or qualified tabletop receipt exists:
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# exercises.support_tabletop / security_tabletop qualified halves as required
# by the bundle schema, plus supplier_policy approval for abuse scope
make release-doctor
```

Related docs: `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`,
`docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: abuse_and_trust: derived=2/2
# total +1
```

---

## Score arithmetic checklist (operator)

After each wired receipt lands, recompute — do not trust a remembered total:

```bash
python3 scripts/validate-readiness.py
# compare domain lines to the table at the top of this file
# ops/go-no-go.json readiness_score must equal the new derived total
# or the validator fails closed
```

Additive path if every gap closes in the order above:

| After closing | Running total |
|---|---:|
| Start (machine ceiling) | 84 |
| + money (6) | 90 |
| + licensing (1) | 91 |
| + privacy (1) | 92 |
| + artifacts (2) + database (1) | 95 |
| + deployment soak (3) | 98 |
| + security (1) | 99 |
| + abuse (1) | **100** |

GO for Level B still requires the separate P1 list (canary participants,
staffed alert receiver, independent PR reviewer, full governance bundle, etc.).
Hitting 95 on the facet is necessary but not sufficient while open P1s remain.
`go_threshold` is 95 and any open target-scope P0/P1 forces Level B `NO_GO`.

---

## What not to do

- Do not set `earned` by hand in `ops/readiness.json` and expect the score to
  rise. `scripts/test-readiness-gaming.sh` proves the validator rejects that.
- Do not use `sk_live`, real cards, or live connected accounts.
- Do not treat a Cloudflare quick tunnel, a laptop Compose stack, or a 60 s /
  300 s local soak as offsite, persistent staging, or a qualifying 24 h soak.
- Do not wire a receipt path for missing evidence "to prepare the score."
  Wire only after the real artifact exists, with a content check that refuses
  empty or simulated stand-ins.
- Do not invent governance approver names. An unsigned or self-approved bundle
  is not a qualified approval.

---

## Related surfaces (read-only context)

| Surface | Role |
|---|---|
| `scripts/validate-readiness.py` | Authoritative facet derivation |
| `ops/go-no-go.json` | Level A/B/C decisions and P1 exit criteria |
| `ops/readiness.json` | Advisory ledger; `earned` ignored by the scorer |
| `ops/go-closure-inputs.json` | Exact operator input names |
| `docs/PROGRAMME.md § "External-only release handoff"` | Broader external-only release handoff |
| `docs/RUNBOOKS.md § "Staging deployment command plan"` | Ordered staging deploy commands |
| `docs/ARCHITECTURE.md § "Stripe Sandbox setup"` | Stripe test inputs and matrix |
| `RELEASE_READINESS.md` | Scope-separated release narrative |
| `docs/PROGRAMME.md § "Merc shippability status"` | Capability vs release authority |

---

*Operator pack. External receipt rows are wired in `scripts/validate-readiness.py`
so real artifacts can earn the reserved 16 points; domain `possible` totals and
existing content checks are unchanged. Empty or fabricated files still score 0.*


<!-- source: docs/LEVEL_B_OPERATOR_HANDOFF.md -->

# Level-B operator handoff

Current committed candidate: reseal to exact clean `HEAD` (this tree is
`9e31c65b`; live staging serves `19fe0b23`). Readiness is **87/100**, P0 is
**0**, P1 is **5**, Level B is **NO-GO**, backend alpha is **85/91**
`ALPHA_ENGINEERING_READY NO_GO` / `EXTERNAL_ALPHA_PROVEN NO_GO`, and Level C
is **NO-GO_PROHIBITED**. **87/100 is the current derived total** (84 local +
3 offsite). The remaining 13 points are external-only (see
`docs/PROGRAMME.md § "Facet external action pack"`). A Stripe live
credential is prohibited and is not an input to this procedure.

The five open P1s are: complete Stripe sandbox CAD matrix including Connect
(`P1-STRIPE-TEST`); approved buyer/worker canary (`P1-CANARY-REHEARSAL`);
external staffed alert fire and resolution (`P1-ALERT-DELIVERY`);
independent accountable review/retest (`P1-INDEPENDENT-APPROVAL`); and
candidate-bound governance approvals (`P1-GOVERNANCE`). Staging, alpha
rollback/restart/3600 s soak, and offsite restore are `SATISFIED`. The
24-hour soak remains unearned at Level B/C. No local or simulated receipt
closes an open P1.

## External bundles

| Bundle | Obtain from / required scope | Secure location | Non-secret verification | Gate |
| --- | --- | --- | --- | --- |
| Staging | Project staging host, SSH authority, base hostname and Cloudflare authority; isolated persistent test stack only | topology in `ops/launch/level-b.yaml`, credentials in `.merc-launch.env` mode 0600 | `merc release doctor --config ops/launch/level-b.yaml --secrets-file .merc-launch.env` | TLS staging, rollback, storm, soak |
| Artifact storage | Persistent staging object-store endpoint/bucket and least-privilege credentials | YAML and mode-0600 env file | same doctor command | durable artifacts |
| Offsite backup | Independent provider/bucket, restricted credentials, age recipient and separate age identity | mode-0600 env file; identity in separate mode-0600 file | `make release-doctor CHECK=backup` | backup and isolated restore |
| Stripe sandbox | `sk_test_`/approved `rk_test_`, both webhook secrets, test Connect account and endpoint IDs | mode-0600 env file | `make release-doctor CHECK=stripe` | CAD matrix |
| Alerting | Real on-call receiver credential/URL | mode-0600 env file | `make release-doctor CHECK=alert` | firing and resolution |
| Participants | Two approved synthetic buyers, two approved workers, enrollment authority and reviewed drivers | restricted completed participant record | `make release-doctor CHECK=canary` | approved canary |
| Independent review | Named non-author reviewer and completed report/retest | restricted review record | `make release-doctor CHECK=review` | review/retest |
| Governance | Exact-candidate named approvals and exercises | restricted governance evidence store | `merc release approvals-check --bundle <restricted-bundle>` | governance |

`merc release inputs --minimal --json` reports these eight bundles and does not
treat the 24-hour soak as an input. `merc release inputs --explain` classifies
the detailed adapter fields. Copy `.merc-launch.env.template` before adding any
secret; use the two JSON templates as non-secret record shapes.

Advisory external-review status: **NO_USABLE_VERDICT**. This workspace has no
authenticated external-review adapter; prior unavailable-adapter logs remain
under local tool state. External review is not treated as an approval or as
evidence for any gate.

## Resume

1. Fill `ops/launch/level-b.yaml` (the object hostname may be derived as
   `objects.<staging hostname>`).
2. Store secrets in mode-0600 `.merc-launch.env`.
3. Complete participant and approval records outside git.
4. Run:

```sh
merc release doctor --config ops/launch/level-b.yaml --secrets-file .merc-launch.env
merc release plan --config ops/launch/level-b.yaml --secrets-file .merc-launch.env --out .artifacts/level-b-plan.json
merc release launch --environment staging --config ops/launch/level-b.yaml --secrets-file .merc-launch.env --apply --approve-plan <PLAN_SHA256>
```

The last command remains fail-closed: it either produces candidate-bound
evidence and a decision, or reports the exact missing receipt. Expected direct
costs are staging compute/object storage, offsite storage/egress, and the
chosen alerting service; no paid resource is created by this repository without
the supplied authorized credentials.


<!-- source: docs/EXTERNAL_OPERATOR_HANDOFF.md -->

# External-only release handoff

This handoff contains no secret values. Never paste a credential into an issue,
pull request, CI log, evidence receipt, or chat. Store credentials only in the
named external secret boundary and run the verification command from a clean
operator shell.

| Requirement | Where the operator obtains it | Minimum scope | Secure storage | Verification command | Gate closed |
|---|---|---|---|---|---|
| Persistent staging provider credentials | Approved infrastructure provider or organization cloud administrator | Create/update only the private merc staging service, persistent volumes, firewall rules, and secret references | Provider secret manager; expose only as `.env.go-closure` references on the deployment host | `scripts/cx release validate-staging && scripts/go-closure-deploy.sh --target ssh --activate candidate --check` | `P1-STAGING` |
| DNS hostname or DNS authorization | Organization DNS administrator for the approved private-canary zone | Create/update the single staging A/AAAA/CNAME record and authorize certificate issuance for that hostname only | DNS provider secret manager; certificate material in the staging secret store | `make release-doctor CHECK=staging` followed by the documented deployment probe | `P1-STAGING` |
| Independent offsite backup destination | Approved storage provider and a separate backup administrator | Write encrypted backup objects and read them back in the dedicated backup prefix; no plaintext permission | Backup-provider secret manager under a principal separate from staging | `scripts/go-closure-rollback-rehearsal.sh --target ssh --execute` | `P1-OFFSITE-RESTORE` |
| Stripe sandbox `sk_test` key | Stripe Dashboard in test mode from the authorized payments administrator | Test-mode customers, PaymentIntents, refunds, disputes, and required read-only reconciliation; no live resources | Organization secret manager mapped only to `STRIPE_SECRET_KEY` in `.env.go-closure` | `scripts/cx release stripe-check` | `P1-STRIPE-TEST` configuration half |
| Stripe sandbox webhook `whsec` secret | Stripe Dashboard test-mode webhook endpoint settings | Sign only the dedicated account webhook endpoint; use a distinct Connect endpoint secret | Organization secret manager mapped to the bounded webhook variable; never source a live environment | `scripts/cx release stripe-check` | `P1-STRIPE-TEST` webhook half |
| Stripe test Connect configuration | Stripe Dashboard test-mode Connect settings and a disposable Canadian test connected account | CAD test accounts, transfers, payout simulation, restrictions, and account status only | Organization secret manager and `.env.go-closure` variable names documented in `docs/ARCHITECTURE.md § "Stripe Sandbox setup"` | `scripts/cx release stripe-matrix` | `P1-STRIPE-TEST` |
| Real alert receiver credential | Approved paging provider/on-call administrator | Post only to the private-canary service/route with firing, acknowledgement, and resolution visibility | Paging-provider secret manager referenced by staging Alertmanager | `scripts/cx release alert-page --real-receiver-env MERC_ALERT_RECEIVER_<APPROVED_NAME>` | `P1-ALERT-DELIVERY` |
| Reviewed staging action adapters | Release engineering owner for the staging host and both Metal devices | Implement only the documented schema-v2 canary scenarios and two-agent supervisor restart action; independently record each reviewed executable SHA-256 in `.env.go-closure` | Root-owned/non-group-writable staging path; non-secret approved digests in `.env.go-closure` | `scripts/go-closure-restart-storm.sh --target ssh --check && scripts/go-closure-canary-rehearsal.sh --target ssh --check` | Execution half of `P1-RECOVERY-SOAK` and `P1-CANARY-REHEARSAL` |
| Qualified governance approvals | Named security, privacy, legal, licensing, payments, operations, supplier-policy, and release authorities | Review and approve or reject the exact clean commit and bounded test-only canary scope, including qualified human tabletop evidence | Signed approval bundle in the restricted governance evidence store; pass only its path | `scripts/cx release approvals-check --bundle <secure-bundle-path>` | `P1-INDEPENDENT-APPROVAL` and `P1-GOVERNANCE` |
| Additional physical Metal devices | Authorized supplier-operations owner | One receipt per distinct approved Apple Silicon device; synthetic data and private-canary workloads only | Device enrollment credential in the device keychain; characterization receipts in the restricted evidence store | `make agent-characterize` on each device, then the supervised canary command | Physical-device portion of `P1-RECOVERY-SOAK` and `P1-CANARY-REHEARSAL` |

After every row has produced its real evidence, the release operator must run
the exact-path command documented under “Evidence and recovery rules” in
`ops/staging/README.md`. `validate-go-closure-evidence-chain.py` is the sole
cross-receipt acceptance gate: it rejects mixed candidates, images, stale or
reordered operations, unqualified soak data, backup-byte substitution, and a
release approval that predates the operational chain. Its output is sanitized
and authorizes only Level-B review; it cannot activate live Stripe or convert
the decision below into Level-C GO.

The current decision remains:

```text
Level A software candidate: GO
Backend alpha: 85/91, ALPHA_ENGINEERING_READY NO_GO, EXTERNAL_ALPHA_PROVEN NO_GO
Level B supervised Stripe-test-mode canary: NO-GO (87/100, P1=5)
Level C live money/public launch: NO_GO_PROHIBITED
```

`P1-STAGING` and `P1-OFFSITE-RESTORE` in the table above are operator
runbook rows for a repeat; they are not currently open P1s.


<!-- source: ROADMAP.md -->

# Roadmap

What Merc is working on next. Every item here comes from something already
recorded in the repository: the open blockers in `ops/go-no-go.json`, the
follow-ups in `ops/readiness.json`, and the known limitations in `docs/`.

No dates. Items marked *(uncertain)* are named as problems but have no committed
plan yet.

## Near term

> **Updated 2026-08-17.** A private pilot that moves **real** money remains
> `NO_GO_PROHIBITED` even after every item below. These were the five
> workstation-impossible proofs on the path to a test-mode private canary.

- ~~Upload a backup to independent offsite storage and restore from that
  uploaded copy.~~ **Done.** `P1-OFFSITE-RESTORE` `SATISFIED`
  (`evidence/external/offsite-backup-verification.json` and
  `evidence/external/offsite-independent-restore.json`, both `PASS` /
  `BOUND`).
- ~~Deploy to a real staging host with TLS.~~ **Done as a host.**
  `P1-STAGING` `SATISFIED`. `https://mercmerc.net/readyz` is 200,
  `payment_mode=test`. The full buyer/supplier/operator path on that host is
  **not** done (`l12-p1-canary-rehearsal-live-staging.json` is `BLOCKED`).
- ~~Rehearse a rollback to the previous container image in staging.~~
  **Done (alpha).** `evidence/external/staging-rollback-and-forward.json`
  `PASS`. The 24-hour soak is still unearned.
- Run the full Stripe test-mode money matrix end to end, including Connect
  transfers and payout hold/release/failure. **Still open**
  (`P1-STRIPE-TEST`; matrix `BLOCKED` on Connect signup).
- Wire the alert rules to a real on-call receiver and prove that one
  synthetic page is delivered, acknowledged, and resolved. **Still open**
  (`P1-ALERT-DELIVERY`, classified `PUBLIC_LAUNCH`).

## Later

- Run sustained concurrent multi-job and restart soak tests. The current proof is
  two agents running a fixed script; cold-start time has been seen to vary from
  about 3.3 seconds warm to a 34 second cold outlier.
- Sign container images and add a container vulnerability scanner, once registry
  credentials exist.
- Drop the unmaintained `paste` macro once Candle's dependency graph allows it.
  It is a warning, not a known vulnerability.
- Build the buyer account features that are still missing: password recovery,
  data export, account deletion, and self-serve activation for new users.
- Pin the agent's outbound traffic to known destinations with a forced egress
  proxy. The macOS sandbox controls direction and ports but not which host the
  agent can reach. *(uncertain)*
- Remotely attest supplier hardware. Today a machine's identity is self-declared
  and only the advertised configuration is checked. *(uncertain)*
- Grow the minimal website into a real buyer dashboard, without inventing
  optimistic status: queued, running, verifying, complete, failed, and cancelled
  must stay distinct, and an unknown payment outcome must show as awaiting
  operator resolution rather than success or failure. *(uncertain)*
- Publish the Python SDK to PyPI. It currently installs from a checkout.
  *(uncertain)*


<!-- source: RELEASE_GATES.md -->

# Execution-network release gates

> **Historical snapshot from the absorbed `RELEASE_GATES.md`.** Not the
> 2026-08-17 readiness ledger. Current scores and open P1s are in the
> banner at the top of this file and in `ops/go-no-go.json`.

The realtime lane is **NO-GO** for private canary and production.

Passed software gates: schema apply, auth inventory, pinned-profile validation,
SSE ordering and bounds, V0 commitments, usage reconciliation, idempotent
settlement, receipt ownership, no-charge worker-death and client-disconnect
behavior, exact reserve/capture/release/void accounting, audited full internal
credit and supplier clawback before transfer, concurrent stale-contract
recovery, official Python/JavaScript SDK protocol conformance, and static
realtime observability validation.

Open launch gates: physical CUDA/vLLM execution, direct parity benchmark,
real-vLLM rerun of client conformance, tools and structured output, real-engine
cancellation and disconnect races, restart/fallback/load/soak, real metric and
alert delivery, cache acquisition, Stripe test mode, Connect payable
reconciliation, cash refund/transfer reversal/partial-credit proof, an offsite
restore of the locally proven new tables,
privacy/legal review, and private-canary approval.


<!-- source: REQUIREMENT_PROOF_MATRIX.md -->

# Requirement proof matrix

> **Superseded on 2026-07-29. Do not read this table as current state.**
> `docs/PROGRAMME.md § "Merc shippability status"` carries the maintained rung for every lane, and
> `ops/go-no-go.json` plus `RELEASE_READINESS.md` carry the release decision.
>
> The rows below were accurate on 2026-07-21 and the tree has moved past several
> of them. Five are now flatly contradicted by the code: the public price board
> (`pricing/board.json`, `control/pricing_governance.go`, `web/prices.html`),
> image generation (`control/image_generation.go`), LoRA eval payment
> (`control/lora_settlement.go`), the TypeScript SDK (`clients/sdk/typescript/`, builds
> to `dist/`), and single-host multi-GPU (`control/multi_gpu_admission.go`) are
> all marked "Not implemented" or validation-only here while the tree has
> `IMPLEMENTED`-or-better evidence for each.
>
> This file is retained as an audit trail of what was claimed on 2026-07-21, not
> as an operator checklist. A row here may not be used to promote or demote any
> lane.

Status as of 2026-07-21. `Tested` means automated software evidence. It does not
mean a physical CUDA/vLLM runtime ran.

| Requirement | Code | Tests | Real runtime | Canary | Production | Blocker |
|---|---|---|---|---|---|---|
| Chat completions | Implemented | PostgreSQL + fake-upstream integration | Not proven | No | No | CUDA host |
| SSE streaming | Implemented, one-event bound | Unit + integration | Not proven | No | No | CUDA host |
| OpenAI client migration | Base-URL compatible | Python 2.46.0 + JavaScript 6.48.0 | Not proven | No | No | Repeat against real vLLM |
| OpenAI model discovery | List envelope + realtime alias | Python + JavaScript `models.list()` | N/A | No | No | Canary proof |
| Parallel tool-call shape | Transparent pass-through | Python + JavaScript two-call integration | No | No | No | Real model/runtime support |
| Structured-output shape | JSON-schema pass-through | Python + JavaScript integration | No | No | No | Real model/runtime support |
| vLLM adapter | Pinned supervisor implemented | Command/profile tests | Not executed | No | No | Linux CUDA + Docker runtime |
| Usage reconciliation | Implemented | Exact final-usage tests | Not proven | No | No | Real tokenizer/runtime |
| Stream receipt | Implemented | Hash-chain receipt integration | Not proven | No | No | Real runtime |
| Buyer authorization | Explicit reserve/capture/release/void/refund facts | PostgreSQL binding + restore invariants | Internal only | No | No | Stripe test-mode account aggregation |
| Buyer charge | Immutable settlement + internal ledger | Exact capture and zero-sum integration | Internal only | No | No | Stripe test-mode aggregation |
| Supplier payable | Held liability, payout state, pre-transfer clawback | Integration + concurrent refund/payout race | Internal only | No | No | Stripe Connect test mode |
| Refund/no-charge | Failure voids full reservation; operator-confirmed platform fault creates full internal credit | Worker-death/recovery/disconnect + audited idempotent refund integration | Internal only | No | No | Stripe cash refund, reversal, partial-credit paths |
| Client disconnect | Upstream cancellation + no-charge receipt | PostgreSQL integration | Not proven | No | No | Real vLLM failure injection |
| Control crash recovery | Deadline/grace sweep + capacity restore | Concurrent PostgreSQL recovery | Not proven | No | No | Restart rehearsal with real runtime |
| Realtime observability | Metrics, alerts, dashboard, runbook | Static validator + endpoint integration | Local only | No | No | Real receiver/canary |
| Realtime backup/restore | New tables and relationships included | Isolated custom-format restore + invariants | Local only | No | No | Offsite restore boundary |
| Model cache | Adapter mounts governed local path | Command test | Not proven | No | No | CUDA host and model acquisition |
| Autoscaling | Not implemented | No | No | No | No | Later stage |
| Object upload | Existing batch S3 path only | Existing tests | Batch only | No | No | Realtime artifact plane |
| Public price board | Not implemented | No | No | No | No | Benchmark evidence |
| Direct-vLLM parity benchmark | Harness + pinned fixture implemented | Self-test + fake-upstream end-to-end, un-attested | No | No | No | Linux CUDA worker |
| Image generation | Not implemented | No | No | No | No | Later stage |
| LoRA eval payment | Not implemented | No | No | No | No | Later stage |
| Multi-GPU | Profile fields present | Validation only | No | No | No | Multi-GPU hardware |
| TypeScript SDK | Not implemented | No | No | No | No | Later stage |
| Organizations | Not implemented | No | No | No | No | Later stage |


<!-- source: docs/LANE_RESEARCH.md -->

# Which lane can actually pay a supplier

Three independent web-enabled studies, commissioned 2026-07-26, on pricing
conventions, the throughput/watt frontier, and demand for verified compute. All
three converged, which is the reason to take the conclusion seriously.

## The arithmetic that forces the answer

Moving the catalogue from cost-plus to a market board fixed a price that was
~460× above market — and cut supplier gross by the same factor. On the hardware
the control plane admits:

| | Cost-plus | Market board |
|---|---:|---:|
| Price, Llama-3.2-1B | $4.1386 / 1M | $0.0090 / 1M |
| Supplier gross @138.7 tok/s | $2.004 / hr | $0.00436 / hr |
| Electricity (30 W @ $0.15/kWh) | $0.0045 / hr | $0.0045 / hr |
| **Supplier net** | **+$2.00 / hr** | **−$0.00014 / hr** |

Required throughput at the market price, 97% supplier share:

| Supplier net | Aggregate tok/s needed | Reality |
|---|---:|---|
| electricity only | 143 | M3 Pro measures **138.7** — underwater |
| $0.25 / hr | 8,098 | one fully-loaded H100 at SOTA serving |
| $1.00 / hr | 31,962 | multi-GPU continuous-batching cluster |
| $2.00 / hr | 63,781 | multi-node datacenter |

TensorRT-LLM on an H100 at FP8 peaks near ~11k aggregate output tok/s for a
6B-class model — about **$0.25–0.35/hr** of supplier gross, and only if the card
is sold 100% of the time. That is not consumer-fleet economics; it is selling a
whole H100 at near-theoretical decode density.

## What the studies concluded

**Pricing.** There is no stable price point at which a heterogeneous consumer
fleet, priced per token, both pays owners above electricity and undercuts
RunPod/Vast — because market token prices already sit near datacenter marginal
cost with continuous batching. GPU-*hour* rental (the Salad/Vast shape) does work
for consumer NVIDIA hosts. **That is not a token market.**

**Speed.** Closing the engine gap (Candle static batching → vLLM/SGLang/TRT
continuous batching) is real and large — roughly **5–50×** — but it does not close
the *economic* gap at $0.009/1M for small models. It makes you a better commodity
seller of already-raced tokens.

**Speculative decoding is a clear no.** It helps latency at low batch and
generally *hurts* throughput at high batch, which is the wrong direction
entirely: payout economics are driven by aggregate throughput, not per-request
speed. It cannot deliver 8k–64k tok/s.

**A generic "we are faster" story is not a moat.** Cerebras (~1,800 tok/s on
Llama 3.1 8B) and Groq (~600 tok/s on 8B-class) already sell per-request speed
that a marketplace of spare machines will not match.

**Demand.** "Verified compute" as defined here — receipts, hash-chained stream
evidence, honeypots, an immutable ledger — is **not a standalone market**. No
public buyer RFPs or procurement language name marketplace-style cryptographic
receipts as the purchased good. It is a *feature* that lets strangers settle,
analogous to escrow. What buyers do pay for is **confidential computing / TEE
isolation** (data-in-use privacy, ~20–30% instance premium in regulated settings,
figure still being traced to SKU pricing) and **compliance posture** as a
procurement gate. Those are different products.

## The lane that does work

**Deadline-tolerant GPU rendering.**

| | Value |
|---|---|
| RTX 4090 OctaneBench | ~1,300 OB |
| Farm clearing rate (GarageFarm low GPU tier) | $0.004 / OB·hr |
| **Gross capacity value** | **~$5.20 / hr** |
| Electricity @ 400–450 W | ~$0.06–0.07 / hr |
| After a 20–40% take and partial utilisation | still **dollars per hour** |

Against **$0.0044/hr, underwater** for AI inference. Roughly three orders of
magnitude better per supplier-hour.

And the shape matches what this codebase already is: embarrassingly parallel by
frame, deadline-tolerant, output-verifiable by pixel hash, retries cheap. The
scheduler was built for exactly this — 45-second task sizing, `FOR UPDATE SKIP
LOCKED` claiming, hedge tasks, straggler requeue, per-chunk settlement. The
verification machinery that has no buyer in AI has an obvious use here: frames
are the artifact and a reference frame is a natural honeypot.

Secondary lane: **Mac/iOS CI**, where Apple hardware is a licensing requirement
rather than a cost disadvantage — the one place this project's Apple-only
supply constraint is an asset instead of a liability.

Risks the research names honestly: DCC licences and plugins, VRAM floors, NDA and
content security (TPN), colour management, and support burden. **Render farms
sell operations, not raw FLOPs.**

## What this does not say

It does not say to pivot today. It says the AI-inference token lane cannot pay
suppliers at market prices on any hardware this project can realistically
aggregate, and that a lane exists which can. The batch machinery, the settlement
ledger and the verification apparatus all carry over — the workload changes, not
the platform.

Sources, fetch dates and the full arithmetic are in the three commissioned
reports under the local external-review task archive.

---

<!-- historical-doc-names:begin — this ledger names documents by their
     pre-consolidation identity on purpose; the names are the record of which
     sources contradicted each other, not links to live files. -->
# Documentation contradiction ledger (L6)

Surfaced, not resolved. Merging placed contradictory claims side by side; this
ledger stops that reading as a merge error.

## Ten known cross-document contradictions

1. **Route counts 76 / 77 / 110** — `docs/SECURITY.md` (110 method/path
   registrations) vs `docs/DECISION_ZERO_REVERSAL.md` (72→77 discussion) vs other
   surfaces citing 76. Authority at execution time: `ops/authorization-matrix.json`.
2. **MLX throughput 6,828 t/s vs 310.7 t/s** — `docs/SPEED_LANE_2026-07-27.md`
   vs `docs/RUNTIME_CROSS_TEST_2026-07-30.md` (different harnesses).
3. **Webhook runbook anchor mismatch** — alerts request
   `docs/RUNBOOKS.md#webhook-delivery-failure` but heading slug is
   `#webhook-failure` (`## Webhook failure`). Pre-existing; not fixed in L6.
4. **Board-power twin citations** — `board-power-a40-latest` vs timestamped twin
   are independently cited (not a numeric contradiction; alias non-collapse rule).
5. **Quiet vs quiet.bound** — not byte-identical; `quiet.json` uniquely records
   `missing_identity_fields` (negative result). Not collapsed.
6. **Shippability / programme status language** — `docs/SHIPPABILITY_STATUS.md`
   vs `docs/MERC_SHIPPABILITY_DIRECTIVE.md` vs root `RELEASE_READINESS.md` use
   different readiness vocabularies for overlapping claims.
7. **Canary driver findings vs scenario driver** — `docs/CANARY_DRIVER_FINDINGS.md`
   vs `docs/CANARY_SCENARIO_DRIVER.md` differ on which scenarios are closed.
8. **Frontier / path-to-ten targets** — `docs/FRONTIER_300X.md` vs
   `docs/PATH_TO_TEN.md` vs `docs/YOUR_10_ACTIONS.md` disagree on sequencing.
9. **Decision Zero outcome vs reversal** — `docs/DECISION_ZERO_OUTCOME.md` vs
   `docs/DECISION_ZERO_REVERSAL.md` record opposing programme turns; both retained.
10. **Runtime matrix vs cross-test ceilings** — `docs/RUNTIME_MATRIX.md` vs
    `docs/RUNTIME_CROSS_TEST_2026-07-30.md` cite different peak figures for
    overlapping cells.

## Three dangling references (do not create files)

1. `docs/REBRAND.md` — cited from `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`
2. `docs/GPU_CAPABILITY.md` — cited from `ops/economics-readiness.json`
3. `docs/internal/CREED_AND_PATH_TO_TEN.md` / `docs/CREED_AND_PATH_TO_TEN.md` —
   cited from `control/scheduler.go` comments

## Policy

- Do not drop either side of a contradiction to make the checker green.
- Do not rewrite historical receipt paths or SHAs while documenting them.
- Legal/frozen paths were not merged into these homes.


<!-- historical-doc-names:end -->
