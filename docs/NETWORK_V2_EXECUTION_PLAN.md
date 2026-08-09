# Merc Network V2 execution plan

Plan authority: `MERC_ONE_AND_ONLY_NETWORK_V2_10_OF_10_BIBLE.md` and the
attached execution directive. Repository truth was recovered at
`ae443f69b0a63835a356eb1ed01999325677ad1b` before this plan was written.

This plan is deliberately decomposed at authority boundaries. A status of
`RECONCILED_COMPLETE` means the current tree already proves the step and the
work must not be repeated. `PARTIAL` means useful implementation exists but the
Bible's completion condition is not yet true. Simulation and local proof never
promote a physical-fleet or launch claim.

## Entry truth and execution rules

- The source worktree used for implementation is `codex/network-v2`, created
  from `main` at `ae443f69`. The original `main` worktree remains untouched and
  contains a pre-existing, incomplete edit to `control/dev_checkpoint.go`.
- Local `main` is 80 commits ahead of the locally known `origin/main`; no fetch
  was performed, so remote freshness is not claimed.
- `refactor/teardown` is clean at `844ab424` and contains 32 mutation-runner
  commits not present on `main`. Two preserved campaigns corroborate 84 caught,
  0 survived, 0 stale, 0 infrastructure faults in about 259 s and 266 s. Step 4
  now also has an exact clean-candidate campaign at `541c1357`: 84 caught, zero
  survived/stale/infrastructure, 16 isolated workers, 287 s under the 299 s gate.
- Mutation optimization is closed. The already-built runner may be integrated;
  it is not a new optimization tranche. Use change gates for ordinary work,
  authority gates for authority changes, and one full 84 campaign only at a
  coherent candidate boundary.
- Level B remains `NO_GO`; Level C remains `NO_GO_PROHIBITED`. External evidence
  is never synthesized from local tests or the Digital Twin.
- One writer owns a load-bearing authority at a time. Read-only audits, fixture
  generation, benchmark analysis, and proof review may run in parallel.
- Every completed implementation step appends a mini-audit and updates the live
  ledger. A step with a load-bearing correctness score below 100, money safety
  below 100, evidence integrity below 100, or plan fidelity below 95 is repaired
  before the next dependent step.

## Live ledger at plan freeze

| Step | Boundary | Entry status | HEAD | Grade | Key evidence / blocker | Next dependency |
|---:|---|---|---|---|---|---|
| 1 | Exact-state reconciliation | `COMPLETE` | `ae443f69` | 100/100 | frozen reconciliation receipt and exact dirty ownership | step 2 |
| 2 | Canonical migration register | `COMPLETE` | `ae443f69` | 100/100 | eleven-object deletion and authority register | step 3 |
| 3 | Validation plane integration | `COMPLETE` | `2ed771c1` | 100/100 | 19 infra commits; all self-tests pass; 84-entry manifest | steps 1–2 |
| 4 | Runtime-cell economics | `RECONCILED_COMPLETE` | `541c1357` | 100/100 | terminal no-P0/P1 audit; exact 84/84 mutation campaign in 287 s | steps 1–3 |
| 5 | PricingDecision and true net | `REPAIR_IN_PROGRESS` | `541c1357` | pending | canonical settlement landed; cross-currency prepaid/realtime/service replay repairs in progress | step 4 |
| 6 | Canonical Capability | `PARTIAL` | `ae443f69` | pending | facts split across worker/profile/offer tables | steps 2, 4 |
| 7 | Canonical MarketDecision | `PARTIAL` | `ae443f69` | pending | lane-specific clearing receipts only | steps 2, 5–6 |
| 8 | Canonical RuntimeDecision | `PARTIAL_SHADOW_ONLY` | `ae443f69` | pending | measured economics run after batch commit | steps 6–7 |
| 9 | Canonical PlacementDecision | `PARTIAL` | `ae443f69` | pending | mode, requirement, host plan are three meanings | steps 6–8 |
| 10 | Canonical TopologyDecision | `PARTIAL_SHADOW_ONLY` | `ae443f69` | pending | TopologyPlan not bound to accepted batch work | steps 6–9 |
| 11 | Accepted decision-chain transaction | `ABSENT` | `ae443f69` | pending | no atomic Market→Runtime→Placement→Topology chain | steps 7–10 |
| 12 | VerificationContract | `PARTIAL` | `ae443f69` | pending | policy/class/comparator/effects split | steps 2, 11 |
| 13 | SettlementPlan | `PARTIAL` | `ae443f69` | pending | accepted money and later settlement split | steps 5, 11–12 |
| 14 | EvidenceEnvelope | `ABSENT` | `ae443f69` | pending | lane receipts have no common chain root | steps 11–13 |
| 15 | Locality and cache-aware routing | `PARTIAL_PHYSICAL` | `ae443f69` | pending | two-worker prefix proof; other residency classes absent | steps 6, 9, 14 |
| 16 | Canonical Workload and Project Compiler | `PARTIAL` | `ae443f69` | pending | graph becomes per-step legacy submissions | steps 2, 11–14 |
| 17 | Digital Twin production seam | `ABSENT` | `ae443f69` | pending | no twin package/harness | steps 7–16 |
| 18 | Candidate indexes and coherent epochs | `ABSENT` | `ae443f69` | pending | production worker selection remains SQL scan/rank | steps 6–11, 17 |
| 19 | Deterministic synthetic fleets | `ABSENT` | `ae443f69` | pending | no 10→1M governed generator | steps 17–18 |
| 20 | Scale curves and narrowing budgets | `ABSENT` | `ae443f69` | pending | no persisted qualified selector curves | steps 18–19 |
| 21 | Historical shadow replay and regret | `PARTIAL` | `ae443f69` | pending | batch cell regret exists; no network snapshot replay | steps 11, 14, 17–18 |
| 22 | Network mutation and fault contracts | `ABSENT` | `ae443f69` | pending | no manifest for decision faults | steps 11, 17–21 |
| 23 | Direct-engine parity | `MEASURED_NOT_CLOSED` | `ae443f69` | pending | corrected harness; retained failures/refusals | steps 11, 14 |
| 24 | Matched heterogeneous placement | `BLOCKED_BY_MATCHED_EVIDENCE` | `ae443f69` | pending | Metal/CUDA comparison refuses unmatched authority | steps 11, 18, 23 |
| 25 | Work-elimination expansion | `PARTIAL` | `ae443f69` | pending | exact reuse/coalescing/prefix proven; wider assets incomplete | steps 14–18 |
| 26 | Workload expansion | `PARTIAL` | `ae443f69` | pending | render/media/LoRA surfaces mixed proof levels | steps 12–16, 25 |
| 27 | Network fabrics | `PARTIAL` | `ae443f69` | pending | modes/fabric measurement exist; end-to-end choices incomplete | steps 9–11, 17–20 |
| 28 | ServiceLease | `PARTIAL` | `ae443f69` | pending | metering/failover exist; autoscale/scale-zero/region loss absent | steps 11–14, 18, 27 |
| 29 | Regional/self-optimization seams | `PARTIAL` | `ae443f69` | pending | promotion/rollback fragments; canonical epochs absent | steps 18–22, 27–28 |
| 30 | Shippability and supremacy gauntlet | `BLOCKED_EXTERNAL_AND_PENDING_LOCAL` | `ae443f69` | pending | 8 P1, no exact-HEAD candidate checkpoint | all prior steps |

## Numbered implementation plan

### Step 1 — Recover, reconcile, and freeze exact state

1. **Objective:** Produce an exact, non-promotional state receipt and live
   ledger for the implementation worktree, including branch ancestry, dirty
   ownership, active validation, authority inventory, proof inventory, and open
   external blockers.
2. **Why:** Every later decision must start from source truth and must not
   repeat completed work or inherit stale programme prose.
3. **Current authority:** The new reconciliation is derived from current Git,
   worktree, process, receipt and schema probes. The historical
   `evidence/state/program-network-10of10-entry.json` is an **UNBOUND citation
   only** and supplies no authority; old checkpoint receipts are supplemental
   historical records. Extend `scripts/branch-state-receipt.py` only if its
   output can remain fully derived.
4. **Files/packages:** `evidence/state/`, `ops/go-no-go.json`,
   `ops/readiness.json`, `docs/NETWORK_V2_EXECUTION_PLAN.md`; read-only Git,
   process, worktree, receipt, and schema probes.
5. **Dependencies:** None.
6. **Safe parallelism:** Read-only state, authority, and proof audits.
7. **Serial work:** Freeze one reconciled receipt and ledger after all audits
   agree; do not mutate either source worktree while measuring it.
8. **Required proof:** Exact commit, branch/upstream divergence, worktree list,
   dirty diff ownership, process/lock census, canonical-symbol census, mutation
   campaign provenance, readiness P0/P1 list.
9. **Mutation/test scope:** No mutation campaign. Run receipt-schema/unit checks
   only if the receipt writer changes.
10. **Performance metrics:** Audit wall time; no product latency claim.
11. **Completion:** Receipt and ledger name all disagreements and every locally
    actionable or external blocker without treating simulation as fleet proof.
12. **Rollback:** Delete the new receipt/ledger if any field is hand-authored as
    measured truth, points at the wrong HEAD, or leaks secret-shaped data.

### Step 2 — Freeze the canonical migration and deletion register

1. **Objective:** Map Workload, Capability, PricingDecision, MarketDecision,
   PlacementDecision, RuntimeDecision, TopologyDecision, VerificationContract,
   SettlementPlan, EvidenceEnvelope, and ServiceLease to current symbols,
   conversions, allocation/copy behavior, lost policy, canonical winner, and
   deletion milestone.
2. **Why:** V2 forbids decorative schemas and parallel authority.
3. **Current authority:** Reconciles `ProjectWorkloadIR`/`WorkloadDecision`/
   `JobManifest`; worker/profile/generated capability objects; lane clearing
   receipts; execution mode/placement requirement/realtime plan; shadow runtime
   selection; topology plans; verification fragments; settlement fragments; and
   lane-specific receipts.
4. **Files/packages:** A versioned register under `docs/`; source references in
   `control/project_*`, `workload_classification.go`, `types.go`,
   `runtime_authority.go`, `realtime_*`, `execution_mode.go`,
   `topology_planner.go`, `verification_*`, `pricing_decision.go`,
   `receipt*.go`, `service_lease*.go`.
5. **Dependencies:** Step 1.
6. **Safe parallelism:** Independent caller/copy census and test census.
7. **Serial work:** Choose one canonical owner and deletion order per concept;
   later code changes must follow this register.
8. **Required proof:** Symbol and production-caller references for every row;
   explicit lossless/lossy/allocating conversion classification.
9. **Mutation/test scope:** Static authority/callgraph tests only.
10. **Performance metrics:** Conversion allocations/copies and hot-path impact.
11. **Completion:** Every legacy representation has a retirement, immutable
    historical, or single compatibility-edge disposition.
12. **Rollback:** Revert a mapping if a real production caller or policy field
    was missed, or if it would create two mutable sources of truth.

### Step 3 — Integrate, do not re-optimize, the closed validation plane

1. **Objective:** Bring the already-proven fast/authority/full/deep mutation
   gates from `refactor/teardown` into the implementation line, resolving only
   integration conflicts.
2. **Why:** V2 development needs bounded feedback, and the proven work currently
   lives on a divergent branch rather than the candidate line.
3. **Current authority:** Extends `scripts/mutation-test.sh`, `Makefile`, and
   checkpoint wiring with the manifest, sharding, isolated DB, preflight cache,
   and gate selection already present at `844ab424`.
4. **Files/packages:** Only the 32 unique `refactor/teardown` commits and any
   narrowly necessary merge resolution; never the dirty root checkpoint edit.
5. **Dependencies:** Steps 1–2.
6. **Safe parallelism:** Read-only merge review and manifest count verification.
7. **Serial work:** One merge owner; one targeted runner self-test.
8. **Required proof:** 84-entry manifest, gate budgets 120/300/299 seconds,
   restoration semantics, isolated worktrees/DBs, and no dropped main change.
9. **Mutation/test scope:** Runner self-tests/change gate only; defer the next
   full 84 run to a coherent candidate checkpoint.
10. **Performance metrics:** Retain the corroborated ~259 s/~266 s full-run
    evidence as historical, not exact-new-HEAD evidence.
11. **Completion:** Implementation branch exposes all four gates and preserves
    the exact 84-case inventory with no new optimization work.
12. **Rollback:** Abort/revert the merge if it drops a main authority change,
    changes the mutant inventory, or cannot prove source restoration.

### Step 4 — Reconcile runtime-cell-specific economics

1. **Objective:** Confirm runtime cell, hardware, physical duration/cost,
   retries, verification, and supplier liability drive economics wherever the
   tree already claims they do.
2. **Why:** Engines are cells; model-level or generic percentages cannot choose
   sustainable heterogeneous supply.
3. **Current authority:** `runtime_cell_economics.go`,
   `runtime_cell_admission_binding.go`, `runtime_cell_entitlement.go`,
   `runtime_cell_performance.go`, and workload-specific supplier-share policy.
4. **Files/packages:** Those files, pricing catalogue authority, scheduler, and
   settlement tests.
5. **Dependencies:** Step 1.
6. **Safe parallelism:** Evidence review and arithmetic audit.
7. **Serial work:** Any repair to money authority.
8. **Required proof:** Per-cell/per-hardware identity, conservative benchmark
   binding, exact supplier floor/entitlement, retry and verification accounting.
9. **Mutation/test scope:** Existing runtime-cell economics and money mutations.
10. **Performance metrics:** Verified-outcome cost/unit and supplier gross/net
    per active hour where measured; absolute values first.
11. **Completion:** Mark `RECONCILED_COMPLETE` if current proofs remain valid;
    implement only defects measurement exposes.
12. **Rollback:** Revert any repair that permits unbound benchmarks, model-only
    pricing, negative unsponsored contribution, or historical money rewrite.

### Step 5 — Reconcile canonical PricingDecision and true-net closure

1. **Objective:** Preserve one immutable fixed-point PricingDecision across
   distributed, reuse, realtime, and lease lanes, and keep true net unavailable
   rather than wrong where accepted costs are unmeasured.
2. **Why:** Accepted money must replay exactly and later measurements may not
   rewrite history.
3. **Current authority:** `pricing_decision.go`, lane builders, frozen digests,
   `modeled_cost_settlement.go`, exact entitlements, and ledger writers.
4. **Files/packages:** Pricing, quotes/jobs/contracts/leases schema, receipts,
   settlement, disputes/refunds, money guards.
5. **Dependencies:** Steps 1 and 4.
6. **Safe parallelism:** Read-only reconciliation across lanes.
7. **Serial work:** Any fixed-point or ledger change.
8. **Required proof:** Currency/unit agreement, buyer ceiling, supplier floor,
   provider/verification/storage/egress state, reserves, exact conservation,
   immutable digest replay, true-net status.
9. **Mutation/test scope:** Pricing/money authority mutants and targeted
   adversarial/race tests.
10. **Performance metrics:** Buyer gross, every cost class, gross spread, true
    net, subsidy, and cost-to-verified-outcome; never gross as profit.
11. **Completion:** Current authority passes without duplicated price derivation
    or hidden unknown-as-zero cost.
12. **Rollback:** Any ceiling/floor/currency/conservation failure or historical
    repricing.

### Step 6 — Canonicalize node Capability facts and epochs

1. **Objective:** Establish one versioned node Capability containing immutable
   identity plus hardware, runtime cells, region/failure domain, network,
   residency/cache, availability/limits, interruption, thermal/power,
   benchmark, trust, and reliability facts while keeping activation policy
   separate.
2. **Why:** Market and placement cannot narrow honestly over scattered or
   supplier-declared facts.
3. **Current authority:** Extends `WorkerCapability`, `authorityCell`,
   `authorityRuntimeProfile`, `generatedRuntimeCapability`, worker-authorized
   DB projections, realtime/service offers, prefix and fabric observations.
4. **Files/packages:** `types.go`, agent wire types, capability manifest/runtime
   authority, worker registration/store/schema, offer and locality/fabric code.
5. **Dependencies:** Steps 2, 4, and 5.
6. **Safe parallelism:** Schema/field census and fixture generation.
7. **Serial work:** Canonical type, digest, persistence migration, and production
   registration conversion.
8. **Required proof:** Lossless legacy conversion, fact/policy separation,
   freshness/epoch binding, tenant/privacy/failure-domain invariants, digest
   tamper rejection, unchanged eligible worker behavior.
9. **Mutation/test scope:** Capability/activation/identity/region/freshness
   authority mutations; targeted agent compatibility tests.
10. **Performance metrics:** Registration/update time, allocation count, index
    update latency, serialized size, memory per worker.
11. **Completion:** Every selector reads one coherent Capability vocabulary;
    legacy projections are read-only compatibility edges with deletion dates.
12. **Rollback:** Restore the prior projection if compatibility loses a fact,
    changes routability, or a stale epoch can satisfy a hard contract.

### Step 7 — Introduce and wire canonical MarketDecision

1. **Objective:** Freeze eligible supply, exclusions, buyer order, offers,
   provider observations, depth, economic plan, clearing policy, confidence,
   epochs, and a digest for every accepted lane.
2. **Why:** A chosen offer without the considered/excluded book cannot prove
   market clearing or support replay/regret.
3. **Current authority:** Replaces lane authority in
   `RealtimeMarketClearingReceipt` and `serviceLeaseMarketClearingDetail`;
   extends batch scheduler/economic plan, while liquidity receipts remain
   observations rather than accepted decisions.
4. **Files/packages:** New canonical decision file, realtime authorization/store,
   service lease creation, batch quote/admission/scheduler, schema and receipts.
5. **Dependencies:** Steps 2, 5, and 6.
6. **Safe parallelism:** Compatibility converters and fixture construction.
7. **Serial work:** One canonical builder/validator/digest and each lane's
   transactional persistence.
8. **Required proof:** Exact candidate/exclusion set, price/currency/ceiling,
   duplicate supplier refusal, stale/forged offer rejection, deterministic
   tie-break, frozen accepted economics, historical replay.
9. **Mutation/test scope:** Reverse prices, duplicate identity, remove ceiling,
   change currency, stale benchmark, hide provider failure.
10. **Performance metrics:** Book acquisition and clearing p50/p95/p99,
    shortlist size, allocations, verified-outcome cost delta.
11. **Completion:** All accepted batch/realtime/lease paths bind one validated
    MarketDecision digest; legacy receipts project losslessly from it.
12. **Rollback:** Revert lane wiring on atomicity, money, deterministic-order,
    or latency regression beyond the declared admission budget.

### Step 8 — Introduce and wire canonical RuntimeDecision

1. **Objective:** Freeze engine, cell, model/artifact, precision/quality,
   hardware class, runtime revision, benchmark authority, activation revision,
   selection basis, challenger state, and rollback target before execution.
2. **Why:** The current batch path ranks measured economics only after commit in
   shadow mode, so runtime choice is not accepted authority.
3. **Current authority:** Replaces authoritative use of the first
   `WorkloadRuntimeCandidate`; absorbs the production subset of
   `ShadowSelection`/`GovernedShadowDecision`; activation/promotion remains the
   policy authority, not a duplicated selector.
4. **Files/packages:** Workload admission, runtime authority/activation,
   runtime performance/cost tie authority, quote/job schema, receipts.
5. **Dependencies:** Steps 6–7.
6. **Safe parallelism:** Deterministic scorer tests and converter review.
7. **Serial work:** Move measured, admissible selection before acceptance and
   persist it atomically; shadow remains observational for unpromoted policies.
8. **Required proof:** Quality/verification first, same-hardware economics,
   honest cost-tie throughput basis, unmeasured refusal, rollback resolution,
   no benchmark-only promotion.
9. **Mutation/test scope:** Inflate throughput, stale benchmark, ignore quality,
   remove rollback, forge lifecycle/activation.
10. **Performance metrics:** Selection p50/p95/p99, cost/latency regret,
    prediction error, changed choice rate, verified cost/throughput delta.
11. **Completion:** Every physical execution cites an immutable accepted
    RuntimeDecision; shadow code cannot silently become money authority.
12. **Rollback:** Restore last activation revision and accepted runtime builder
    if any invalid selection, quality failure, or promotion-gate bypass occurs.

### Step 9 — Canonicalize PlacementDecision

1. **Objective:** Make PlacementDecision bind narrowing stages, coherent
   candidate epoch, locality/load/queue, predicted latency/cost/reliability,
   selected worker(s), execution fabric mode, fallback, and regret baseline.
2. **Why:** Current `PlacementDecision` means only supply lane while actual host
   and claim-time choices use separate authorities.
3. **Current authority:** Extends/replaces `PlacementDecision` in
   `execution_mode.go`, `PlacementRequirement`, `RealtimePlacementPlan`,
   claim-time scheduler SQL, and service lease offer reservation.
4. **Files/packages:** Placement/mode, quote/admission, scheduler/store,
   realtime placement/store, leases, locality/fabric, schema/receipts.
5. **Dependencies:** Steps 6–8.
6. **Safe parallelism:** Predictor fixtures and compatibility adapters.
7. **Serial work:** Canonical semantic rename/migration and transactional worker
   selection/reservation; avoid wrapper-on-wrapper aliases.
8. **Required proof:** Hard constraints precede scores; no region/privacy/floor
   violation; capacity reservation atomicity; deterministic fallback; selected
   worker belongs to frozen candidate set and epoch.
9. **Mutation/test scope:** Forge warmth/locality, remove region, hide failure,
   ignore reliability/capacity, duplicate supplier, deadline removal.
10. **Performance metrics:** Narrowing counts, placement p50/p95/p99, queue and
    startup prediction error, cost/latency/reliability regret.
11. **Completion:** One PlacementDecision vocabulary is frozen for every
    accepted placement and the old three meanings are deleted or immutable
    compatibility projections.
12. **Rollback:** Revert if reservation atomicity, locality freshness, hard
    contract filtering, or claim throughput regresses.

### Step 10 — Canonicalize TopologyDecision

1. **Objective:** Freeze selected/refused topology, degree, fabric evidence,
   device/failure domains, physics rationale, scheduler shape, fallback, and
   digest before acceptance.
2. **Why:** V2 must explicitly choose or refuse topology; batch currently stores
   it only on a post-commit shadow row.
3. **Current authority:** Promotes `TopologyPlan` as the canonical core and
   retires overlapping `ProjectIRTopology`, embedded realtime topology fields,
   and private tensor-parallel plan as compatibility/input projections.
4. **Files/packages:** Topology/fabric/multi-GPU, project compiler, realtime
   placement, workload admission, schema and receipts.
5. **Dependencies:** Steps 6–9.
6. **Safe parallelism:** Physics/refusal scenario tables and converter tests.
7. **Serial work:** Canonical type rename/extension and pre-acceptance wiring.
8. **Required proof:** WAN-tight coupling refusal, measured local gang admission,
   candidate device/failure-domain bounds, deterministic topology, fabric
   freshness and evidence digest.
9. **Mutation/test scope:** Forge bandwidth/latency, corrupt freshness, remove
   degree/deadline/failure domain, hide region loss.
10. **Performance metrics:** Planning p50/p95/p99, predicted/actual transfer and
    startup deltas, topology cost/latency/energy where measured.
11. **Completion:** Every accepted workload binds a TopologyDecision or an
    explicit refusal; no physical topology derives from shadow-only state.
12. **Rollback:** Revert any topology path that admits unmeasured tight fabric,
    oversubscribes devices, or changes legacy single-device behavior.

### Step 11 — Persist the accepted production decision chain atomically

1. **Objective:** Bind MarketDecision → RuntimeDecision → PlacementDecision →
   TopologyDecision, with Workload and PricingDecision identities, in the same
   transaction that accepts/reserves the physical plan.
2. **Why:** This is the highest-priority real gap: today shadow decisions cannot
   veto admission and actual worker choice is implicit later.
3. **Current authority:** Extends `SubmitJobTx`, realtime authorization, service
   lease creation, claim/reservation transactions, and their immutable JSON/
   digest constraints; removes post-commit shadow as the only batch explanation.
4. **Files/packages:** Job/quote/store/scheduler, realtime store, service leases,
   canonical decision files, schema migration, receipt assemblers.
5. **Dependencies:** Steps 7–10.
6. **Safe parallelism:** Read-only transaction and failure-injection review.
7. **Serial work:** Schema migration, transaction ordering, money/reservation
   locks, and production callers.
8. **Required proof:** Crash/retry/idempotency, stale epoch, concurrent claim,
   fallback, tamper, exact digest cross-links, and no orphan money/capacity.
9. **Mutation/test scope:** Authority-domain mutations plus targeted race,
   deadlock, stale-attempt, and failure-matrix tests.
10. **Performance metrics:** Admission/authorization/claim p50/p95/p99,
    lock-wait p95, allocations, DB rows/bytes per decision, throughput.
11. **Completion:** Every accepted execution can be replayed from its frozen
    chain and no production path selects outside it.
12. **Rollback:** Roll back the migration/caller switch if deadlocks, double
    reservation, lost capacity/money, or tail budget breach appears.

### Step 12 — Canonicalize VerificationContract

1. **Objective:** Freeze verifier class/evaluator/reference, threshold and
   comparator revision, sampling, failure consequence, recompute, quality and
   confidence at acceptance.
2. **Why:** Verification is a buyer contract and settlement input, not a free
   string reconstructed after execution.
3. **Current authority:** Reconciles `VerificationPolicy`, cell verification
   strings, workload strategy, verification classes, `VerificationWorkPlan`,
   `VerificationDecision`, and rich comparator output.
4. **Files/packages:** Workload/admission, verification class/plan/work/apply,
   embedding comparator, pricing/settlement, schema/receipts.
5. **Dependencies:** Steps 2 and 11.
6. **Safe parallelism:** Comparator/reference and consequence census.
7. **Serial work:** Canonical contract builder/validator and execution/receipt
   wiring.
8. **Required proof:** Threshold/reference/revision binding, sampling
   determinism, independence, failure/recompute consequences, rich result
   retention, tamper rejection.
9. **Mutation/test scope:** Comparator threshold/revision/reference, sampling,
   independence, failure consequence, result digest mutations.
10. **Performance metrics:** Verification p50/p95/p99, sampling ratio, retry
    burden, quality failures, verified-outcome cost.
11. **Completion:** Every accepted plan and result cite the same immutable
    VerificationContract; legacy fragments project from it without policy loss.
12. **Rollback:** Revert if verification weakens, receipt evidence is lost, or
    money can settle against a different contract.

### Step 13 — Canonicalize SettlementPlan

1. **Objective:** Freeze buyer charge, supplier/provider/verifier entitlements,
   holds, reserves, refund/dispute rules, payout state machine identity, true
   net, and idempotency before execution.
2. **Why:** Pricing amounts alone do not bind the lifecycle and consequences of
   settlement.
3. **Current authority:** Extends PricingDecision fixed point and reconciles
   economic plan/scenarios, verification work settlement, realtime/lease
   settlements, ledger entries, payout funding, refunds and disputes.
4. **Files/packages:** PricingDecision, payment/ledger/payouts, verification
   settlement, realtime/lease settlement, jobs/contracts schema/receipts.
5. **Dependencies:** Steps 5, 11, and 12.
6. **Safe parallelism:** State-machine and legacy-history audit.
7. **Serial work:** Money schema/builder/writers and exact replay.
8. **Required proof:** Nano conservation, currency/ceiling/floor, collected cash
   funding, holds/refunds/disputes, retry/idempotency, unknown provider outcome,
   immutable historical replay.
9. **Mutation/test scope:** Full affected money authority mutations, targeted
   concurrency/race/adversarial tests.
10. **Performance metrics:** Settlement-critical tail, time-to-payable,
    reconciliation rate, provider/payment/risk cost and true net.
11. **Completion:** Every liability transition is authorized by one accepted
    SettlementPlan and 100% reconciles.
12. **Rollback:** Immediate rollback on any lost/double money, hidden subsidy,
    historical rewrite, or unknown outcome misclassification.

### Step 14 — Introduce the immutable EvidenceEnvelope chain root

1. **Objective:** Create one hash-linked envelope for request → workload →
   market → pricing → runtime → placement → topology → execution → verification
   → settlement → receipt across batch, realtime, lease, and project lanes.
2. **Why:** V2 needs one explainable chain, not four incomparable receipt roots.
3. **Current authority:** Replaces lane-specific receipt roots while preserving
   `ClearingReceipt`, `RealtimeReceipt`, `ServiceLeaseReceipt`, and compiler
   receipts as lossless API projections; `ReceiptIdentity` remains file-evidence
   provenance rather than transaction authority.
4. **Files/packages:** New envelope type/validator/digest, decision and execution
   schema, receipt assemblers/APIs, tamper and retention code.
5. **Dependencies:** Steps 11–13.
6. **Safe parallelism:** Projection and historical compatibility tests.
7. **Serial work:** Root persistence and each lane's writer.
8. **Required proof:** Complete cross-digest chain, immutable append semantics,
   missing/wrong-order/tamper rejection, historical receipt readability, tenant
   authorization and retention/deletion policy.
9. **Mutation/test scope:** Evidence authority mutations, chain truncation/
   reorder/tamper, wrong tenant, stale decision, receipt replay.
10. **Performance metrics:** Envelope bytes, hashing/write/read p50/p95/p99,
    receipt critical-path delta.
11. **Completion:** Every new outcome exposes one validated envelope root and
    no lane assembles authoritative identity from mutable current rows.
12. **Rollback:** Revert if write latency breaches budget, history becomes
    unreadable, or any chain mutation is accepted.

### Step 15 — Expand two-worker locality into canonical cache-aware routing

1. **Objective:** Feed governed model, prefix/KV, adapter, artifact, dataset,
   render asset, container layer, kernel, and preprocessing residency/freshness
   into Capability and PlacementDecision without outranking hard economics.
2. **Why:** Reusing expensive state ranks above better placement and engine
   micro-optimization.
3. **Current authority:** Extends prefix routing/placement, two-worker physical
   receipt, model cache policy, artifact/project inputs, and lease residency.
4. **Files/packages:** Prefix/locality, capability, placement/index, agent
   telemetry, project/materialization, evidence and benchmarks.
5. **Dependencies:** Steps 6, 9, and 14.
6. **Safe parallelism:** Residency-class schemas and deterministic fixtures.
7. **Serial work:** Production telemetry ingestion and placement scoring.
8. **Required proof:** Worker/runtime/model/artifact/prefix identity, freshness,
   miss/restart fallback, stale/forged warmth refusal, physical work avoided,
   buyer/tenant isolation.
9. **Mutation/test scope:** Forge warmth, corrupt freshness, wrong tenant/model/
   artifact, cache disappearance, worker loss.
10. **Performance metrics:** Cached tokens/work avoided, TTFT p50/p95/p99,
    startup/prefill delta, hit/miss, verified cost and measured joules.
11. **Completion:** Canonical placement consumes governed locality for all
    implemented residency classes and receipts distinguish physical savings
    from logical throughput.
12. **Rollback:** Disable the affected locality class if stale hits, isolation
    failure, false savings, or worse absolute outcome latency appears.

### Step 16 — Make one canonical Workload graph survive compile to receipt

1. **Objective:** Version a single Workload graph that retains dependencies,
   inputs/outputs/artifacts/models, requirements/estimates, parallelism,
   checkpoints, privacy/region/egress, verification, deadline/SLO, result and
   economic contracts through execution.
2. **Why:** `ProjectWorkloadIR`, `WorkloadDecision`, and `JobManifest` currently
   lose graph policy through allocating step conversions.
3. **Current authority:** Promote `ProjectWorkloadIR` semantics into canonical
   Workload; make `WorkloadDecision` the accepted per-step projection and
   `JobManifest` the wire projection until deleted.
4. **Files/packages:** Project compiler/declaration/contracts/topology/quote/
   submit/order/materialize, workload classification, agent manifest, schema,
   SDK/CLI and receipts.
5. **Dependencies:** Steps 2 and 11–14.
6. **Safe parallelism:** Detector fixtures and legacy converter tests.
7. **Serial work:** Canonical graph, compatibility edge, server-side acceptance
   and dependent-step materialization.
8. **Required proof:** Bounded probe/approval, lossless digests, dependency and
   artifact flow, ceiling across steps, safe refusal, result contracts,
   restart/idempotency.
9. **Mutation/test scope:** Dependency/artifact/ceiling/privacy/topology/
   verification/result-contract mutations.
10. **Performance metrics:** Compile/quote p50/p95/p99, allocations, estimate
    error median/p90, below-ceiling rate, execution critical path.
11. **Completion:** One Workload identity appears in every decision and envelope;
    no graph policy is reconstructed from separate submissions.
12. **Rollback:** Revert a workload class if conversion is lossy, estimates
    breach gates, or dependent execution escapes contracts/budget.

### Step 17 — Build the Digital Twin on production decision functions

1. **Objective:** Add a deterministic harness that invokes the same canonical
   market, pricing, runtime, placement, topology, and decision-chain validators
   used by production, with injected state/clock only at explicit seams.
2. **Why:** Merc must develop network intelligence before the physical fleet
   exists without creating a toy second implementation.
3. **Current authority:** Extends the pure production decision core extracted in
   steps 7–11; no separate simplified scorer or economics formula is allowed.
4. **Files/packages:** New bounded lab package/files inside the modular monolith,
   production decision interfaces, deterministic fixtures, CLI/bench harness.
5. **Dependencies:** Steps 7–16.
6. **Safe parallelism:** Scenario fixture construction and proof review.
7. **Serial work:** Production seam extraction and harness integration.
8. **Required proof:** Same inputs produce byte-identical decisions/digests in
   production and twin paths; deterministic seed/clock; explicit simulation
   label; no DB/money side effect.
9. **Mutation/test scope:** Production/twin parity, alternate-implementation
   guard, determinism and simulation-label tests.
10. **Performance metrics:** Scenario throughput, allocations, memory and
    decision p50/p95/p99; no physical latency/energy claim.
11. **Completion:** Every twin scenario exercises production code and any
    semantic divergence fails the test.
12. **Rollback:** Remove/refuse the twin seam if it forks scoring/economics,
    mutates production state, or can mint admissible physical evidence.

### Step 18 — Add hierarchical candidate indexes and coherent epochs

1. **Objective:** Maintain incremental indexes for hard contract, region/privacy/
   failure domain, runtime/workload, artifact/locality, economics, and final
   scoring over coherent worker/market/locality/provider epochs.
2. **Why:** A linear full-fleet scan cannot remain the large-scale hot path.
3. **Current authority:** Replaces broad scheduler/offer scans only after parity;
   DB remains durability authority while the index is a versioned derived state
   used by production and twin.
4. **Files/packages:** Capability store, selector/placement, worker/offer update
   paths, schema epochs, snapshot/index structures and metrics.
5. **Dependencies:** Steps 6–11 and 17.
6. **Safe parallelism:** Index data-structure benchmarks and fixture generation.
7. **Serial work:** Update ingestion, snapshot publication, production read path
   and atomic final reservation.
8. **Required proof:** Incremental single-worker updates, coherent snapshot,
   no hard-constraint false positive/negative, deterministic shortlist, DB final
   reservation, index rebuild/parity and stale-epoch refusal.
9. **Mutation/test scope:** Dropped update, stale epoch, wrong region/runtime/
   locality bucket, duplicate supplier, hidden failure/capacity.
10. **Performance metrics:** Update and snapshot p50/p95/p99, memory/worker,
    stage cardinalities, final shortlist, index/full-scan parity and speed.
11. **Completion:** Production and twin narrow hierarchically; large-scale hot
    path does not synchronously visit every worker.
12. **Rollback:** Fall back to bounded fail-closed DB selection if index parity,
    freshness, memory, or reservation correctness fails.

### Step 19 — Generate deterministic governed fleets and canonical scenarios

1. **Objective:** Generate fleets of 10, 100, 1k, 10k, 100k, and 1M workers
   varying engine, hardware, price, region, failure domain, network, queue,
   reliability/failure, residency/cache, energy/thermal and interruption.
2. **Why:** The execution brain needs broad, reproducible state before real
   market liquidity exists.
3. **Current authority:** Feeds canonical Capability, market offers and epoch
   updates; never invents a second worker schema.
4. **Files/packages:** Digital Twin generators, canonical scenario manifests,
   deterministic seeds and compact fixture/receipt output.
5. **Dependencies:** Steps 17–18.
6. **Safe parallelism:** Independent scenario family construction.
7. **Serial work:** Canonical generator validation and seed/version freeze.
8. **Required proof:** All Bible scenarios plus stress sizes; bounded values,
   unique identities, correlated failures, expected hard-contract outcomes and
   byte-identical regeneration.
9. **Mutation/test scope:** Generator determinism/bounds/identity, scenario
   expectation and schema-compatibility tests.
10. **Performance metrics:** Generation wall time, bytes/worker, peak memory,
    update ingestion rate.
11. **Completion:** Every declared size/scenario is reproducible and invokes the
    production decision seam with an explicit `SIMULATION_ONLY` evidence class.
12. **Rollback:** Reject a generator revision if distributions are unbounded,
    expected answers are encoded by a second selector, or memory is impractical.

### Step 20 — Persist qualified scale curves and enforce narrowing budgets

1. **Objective:** Measure and persist selection curves for all fleet sizes,
   stage cardinalities, memory, updates, decisions, and correctness.
2. **Why:** Scale targets are qualified goals and must not become unmeasured
   marketing claims.
3. **Current authority:** Extends Digital Twin/selector metrics and evidence
   binding; does not modify physical performance authority.
4. **Files/packages:** Benchmark command/tests, bound evidence under
   `evidence/perf/network/`, claim validation and programme ledger.
5. **Dependencies:** Steps 18–19.
6. **Safe parallelism:** Scale runs by size when host resource budgets permit.
7. **Serial work:** Evidence aggregation, correctness validation and claim gate.
8. **Required proof:** 1k/10k/100k/1M qualified p95 against 1/3/10/25 ms targets,
   no invalid decision, no full scan, exact build/config/seed/raw samples.
9. **Mutation/test scope:** Scale receipt binding and claim-surface tests; no
   full product mutation campaign.
10. **Performance metrics:** Absolute p50/p95/p99, throughput, peak memory,
    allocations, stage counts, update latency and ratios.
11. **Completion:** Curves are reproducible and either pass or name the precise
    scale ceiling; simulation is never presented as fleet performance.
12. **Rollback:** Withdraw a curve if binding, samples, host contention,
    correctness, or qualification is invalid.

### Step 21 — Persist historical decision snapshots and replay regret

1. **Objective:** Store request, canonical decision inputs/network epochs,
   eligible cells, prediction, selected chain, actual outcome, and envelope root;
   replay new policy versions before promotion.
2. **Why:** Synthetic success cannot justify production promotion.
3. **Current authority:** Extends batch `runtime_shadow_selections`, measured
   cell regret and promotion receipts to the complete decision chain and all
   lanes; avoids duplicating facts already immutable in envelopes.
4. **Files/packages:** Decision/envelope schema, replay corpus/query, selector
   promotion/rollback, operator APIs and evidence.
5. **Dependencies:** Steps 11, 14, and 17–18.
6. **Safe parallelism:** Historical corpus extraction and report design.
7. **Serial work:** Snapshot identity, replay engine and promotion gate.
8. **Required proof:** Historical immutability, missing-actual handling, policy
   version/rollback, latency/cost/quality/SLA/energy/fallback metrics, unchanged
   production authority during shadow replay.
9. **Mutation/test scope:** Snapshot tamper/truncation, policy mismatch, currency/
   epoch change, actual-outcome omission, promotion bypass.
10. **Performance metrics:** Replay throughput, latency/cost regret p50/p90/p95,
    prediction error, SLA/quality/fallback/changed-choice rates, energy only when
    authoritative.
11. **Completion:** Every selector promotion requires passing historical replay
    plus existing paired/shadow gates and has an instant rollback target.
12. **Rollback:** Withdraw a policy if replay corpus integrity, regret, quality,
    or rollback resolution fails.

### Step 22 — Extend mutation contracts into network decisions

1. **Objective:** Add a manifest mapping each network fault to the cheapest
   exact invariant, with stable IDs/classes/expected failure signatures.
2. **Why:** Network validation should inherit targeted mutation discipline
   without reintroducing thousands of unrelated tests per fault.
3. **Current authority:** Extends the closed mutation infrastructure and
   canonical decision validators; does not optimize the runner.
4. **Files/packages:** Network mutation manifest/fixtures, decision tests and
   gate classification.
5. **Dependencies:** Steps 11 and 17–21.
6. **Safe parallelism:** Independent fault-contract authoring.
7. **Serial work:** Manifest registration and exact test ladder integration.
8. **Required proof:** Inflate throughput, forge warmth, remove region/deadline/
   ceiling, hide worker/provider/region failure, reverse prices, stale benchmark,
   duplicate supplier, ignore reliability, corrupt locality, change currency;
   each caught for the intended reason.
9. **Mutation/test scope:** Cheapest specific test first; authority gate for
   transitive decision/money faults; full campaign only at candidate boundary.
10. **Performance metrics:** Per-fault and gate wall time, false catches,
    infrastructure failures, restoration and critical path.
11. **Completion:** 100% registered network faults caught, 0 false catches,
    gaps, duplicates, stale results, or restoration faults within budgets.
12. **Rollback:** Remove/quarantine any mutant whose failure is setup, timeout,
    unrelated red, or resource exhaustion rather than the invariant.

### Step 23 — Close corrected direct-engine boundary parity

1. **Objective:** Compare Merc-wrapped engines with direct engines using matched
   request, model/artifact, engine/runtime, concurrency, cache state, transport,
   quality and measurement authority.
2. **Why:** Direct parity is the boundary condition before network advantages
   are credited, though it is not the product endpoint.
3. **Current authority:** Existing gateway parity harness/matrix, withdrawn and
   retained receipts, latency-gap accounting, real CUDA/Metal probes.
4. **Files/packages:** Parity harness/CLI/matrix, runtime adapters, transport,
   evidence binding and claim gates.
5. **Dependencies:** Steps 11 and 14.
6. **Safe parallelism:** Harness review and locally available engine runs.
7. **Serial work:** Matched physical run per engine/hardware and evidence seal.
8. **Required proof:** Exact environment and raw samples, quality identity,
   power analysis, no synthetic upstream, corrected absolute deltas and honest
   refusal when hardware/credentials are unavailable.
9. **Mutation/test scope:** Harness provenance/request equality/sample/gate
   mutations and targeted adapter tests.
10. **Performance metrics:** Merc/direct p50/p95/p99 absolute delta and ratio,
    throughput, verified cost, measured joules.
11. **Completion:** Supported physical cells pass the declared parity budget or
    are explicitly withdrawn/refused with reproducible evidence.
12. **Rollback:** Withdraw any receipt with unmatched arms, underpowered sample,
    fabricated direct path, stale artifact, or material regression.

### Step 24 — Prove matched Metal/CUDA heterogeneous placement

1. **Objective:** Run matched-weight, matched-contract Metal and CUDA cells
   through the canonical selector/decision/envelope and show the choice or
   precise refusal.
2. **Why:** Heterogeneous hardware choice is a network advantage only when both
   arms sell the same outcome.
3. **Current authority:** Existing Metal/CUDA verdict and placement harness that
   correctly refuses unmatched evidence; runtime authority and placement gate.
4. **Files/packages:** Runtime profiles/cells, heterogeneous drive, selector/
   placement, parity/economics/energy evidence and receipts.
5. **Dependencies:** Steps 11, 18, and 23.
6. **Safe parallelism:** Artifact/contract matching and evidence review.
7. **Serial work:** Authorized physical CUDA spend/run and matched selection.
8. **Required proof:** Same weights/quality/request, per-cell physical costs,
   queue/startup/locality, measured power where available, decision/envelope,
   no cross-hardware cost inference when rates are absent.
9. **Mutation/test scope:** Hardware/artifact/quality/cost/energy identity and
   selector basis mutations.
10. **Performance metrics:** Absolute outcome latency/cost/energy, selection
    regret, supplier attractiveness and break-even.
11. **Completion:** Receipt-backed heterogeneous choice or honest
    `EXTERNAL_HARDWARE_OR_FLEET_EVIDENCE` stop.
12. **Rollback:** Withdraw on unmatched model/quality, unauthorized spend,
    unmeasured energy claim, or policy promotion from benchmark alone.

### Step 25 — Expand work elimination beyond current reuse/coalescing/prefix

1. **Objective:** Generalize safe reuse/coalescing/locality to preprocessing,
   tokenization/tool schemas, adapters, datasets, render/media assets, container
   layers and compiled kernels where identity permits.
2. **Why:** Avoiding work produces larger absolute system gains than control
   micro-optimization.
3. **Current authority:** Exact result reuse, 128→1 realtime coalescing,
   prefix/KV routing, prepared request/tool-schema caches, project artifacts.
4. **Files/packages:** Reuse identity/path/batch, inflight coalescing, prefix,
   realtime caches, project/materialization/media/render, agent caches, pricing/
   settlement/receipts.
5. **Dependencies:** Steps 14–18.
6. **Safe parallelism:** Per-class identity and benchmark fixtures.
7. **Serial work:** Production cache/coalescing writer and money semantics per
   class.
8. **Required proof:** Full output identity, tenant/shareability, one physical
   payable, independent buyer receipts, miss/failure fallback, no reuse counted
   as physical throughput.
9. **Mutation/test scope:** Identity omission, tenant crossing, stale result,
   duplicate payable, leader loss, underflow/refund and false-hit mutations.
10. **Performance metrics:** Physical work avoided, hit rate, absolute latency/
    cost/energy delta, cache memory/storage and fallback cost.
11. **Completion:** Each enabled class has production callers and receipt-backed
    savings; inapplicable caches remain explicitly inapplicable.
12. **Rollback:** Disable a class on false equivalence, isolation/money defect,
    or negative absolute outcome.

### Step 26 — Expand governed workload classes only where prerequisites hold

1. **Objective:** Carry rendering, media, image/video generation, evaluation,
   extraction, LoRA/training, bounded containers, and services through canonical
   Workload, decisions, verification, settlement and envelope.
2. **Why:** Merc optimizes verified outcomes across workload classes, not only
   inference.
3. **Current authority:** Existing render work/assembly, media segment/transcode,
   image generation, LoRA dataset/evaluation/settlement, runtime adapters and
   project compiler declarations.
4. **Files/packages:** Workload/project/runtime adapters, agent runners, topology,
   verification, pricing/settlement, buyer/SDK surfaces and canary evidence.
5. **Dependencies:** Steps 12–16 and 25.
6. **Safe parallelism:** Independent workload fixture/proof lanes.
7. **Serial work:** One production authority path per workload; do not promote
   multiple unproven lanes together.
8. **Required proof:** Bounded sandbox/input/artifacts, deterministic work units,
   quality/result contract, failure/reassembly, exact money, physical canary or
   explicit refusal.
9. **Mutation/test scope:** Workload-specific contract/artifact/quality/money/
   failure mutations and targeted agents.
10. **Performance metrics:** Time/cost-to-verified-outcome p50/p95/p99, estimate
    error, throughput, work avoided, measured energy where available.
11. **Completion:** Each promoted class reaches its declared proof rung end to
    end; prerequisites absent means `BLOCKED_BY_PREREQUISITE`, not scaffolding.
12. **Rollback:** Withdraw the class on sandbox, quality, money, artifact,
    estimate, or physical-canary failure.

### Step 27 — Complete POOL, REPLICA_SERVICE, LOCAL_CLUSTER, CLOUD_BACKSTOP

1. **Objective:** Prove or refuse each fabric through canonical decisions:
   independent pool, warm replica service, measured local gang, and economically
   justified provider backstop.
2. **Why:** The network must choose supply topology from physics, deadline,
   locality, reliability and economics.
3. **Current authority:** `ExecutionMode`, `TopologyPlan`, realtime replica
   placement, fabric measurements/evaluations, service offers, cloud-backstop
   flags and project topology.
4. **Files/packages:** Decisions, scheduler, realtime/leases, fabric/topology,
   provider abstraction, Digital Twin scenarios and physical evidence.
5. **Dependencies:** Steps 9–11 and 17–20.
6. **Safe parallelism:** Twin scenarios and independent physical fabric review.
7. **Serial work:** Production mode activation/reservation and provider money.
8. **Required proof:** Pool independence; replica queue/residency/failover;
   mutual measured local links/gang failure; cloud premium vs deadline/reliability;
   WAN-tight refusal.
9. **Mutation/test scope:** Region/fabric/provider/queue/residency/failure/
   deadline/cost faults.
10. **Performance metrics:** Per-mode outcome latency/cost/reliability, transfer,
    queue/startup, failover and measured energy.
11. **Completion:** All four modes have real decision paths and receipts or an
    explicit physics/evidence refusal.
12. **Rollback:** Disable a mode on unmeasured topology, provider-cost omission,
    capacity loss, or worse contract outcome.

### Step 28 — Complete ServiceLease lifecycle authority

1. **Objective:** Add governed autoscaling, scale-to-zero where contract allows,
   residency, health, regional failover, rolling upgrades, continuous metering/
   settlement and per-request service evidence.
2. **Why:** Persistent serving is a distinct buyer contract with continuing
   money and reliability obligations.
3. **Current authority:** Existing `ServiceLease`, offers, prepaid reservation,
   cumulative metering, heartbeat/upgrades, failover, data plane and receipts.
4. **Files/packages:** Service lease/pricing/market/data plane/API/recovery,
   canonical decisions/envelope, provider/region epochs and agent runtime.
5. **Dependencies:** Steps 11–14, 18, and 27.
6. **Safe parallelism:** Autoscaling policy simulations and failure fixtures.
7. **Serial work:** Mutable lifecycle state, capacity and continuous money.
8. **Required proof:** Worker/region loss, scale up/down/zero, upgrade, duplicate/
   retry meter events, idempotent settlement, no lost liability/double charge,
   SLO and per-request evidence.
9. **Mutation/test scope:** Lease authority/money/race/failover/region/upgrade/
   metering mutations.
10. **Performance metrics:** Scale/failover/restart time, warm capacity, SLO p95/
    p99, meter/settlement tail, cost/utilization and reconciliation.
11. **Completion:** Lease lifecycle implements every Bible state or explicitly
    refuses unsupported contracts; eventual money reconciliation is 100%.
12. **Rollback:** Stop new leases and revert policy on lost/double money,
    overbooking, SLO breach, or unrecoverable region/upgrade state.

### Step 29 — Add regional scale seams and governed self-optimization

1. **Objective:** Version region/failure-domain/worker/market/locality/provider
   epochs, regional summaries and hierarchical APIs; learn only through
   replayable shadow policies with promotion/rollback.
2. **Why:** Merc should prepare seams without pre-paying microservices and must
   never let an opaque learner become money authority.
3. **Current authority:** Extends activation/promotion/rollback, performance and
   regret data, capability/index epochs, provider/fabric observations.
4. **Files/packages:** Canonical schemas/index/snapshots, replay/promotion,
   metrics/operator APIs and Digital Twin region scenarios.
5. **Dependencies:** Steps 18–22 and 27–28.
6. **Safe parallelism:** Regional summary and policy replay experiments.
7. **Serial work:** Epoch authority and production policy promotion.
8. **Required proof:** Region-loss isolation, coherent cross-epoch decisions,
   shadow/paired/replay gates, receipt-bound policy version, instant rollback,
   no distributed service split before measured trigger.
9. **Mutation/test scope:** Epoch skew, region/provider loss, stale summary,
   promotion/rollback and opaque-policy bypass faults.
10. **Performance metrics:** State backlog/freshness, lock wait, memory, regional
    decision latency, policy regret and rollback time.
11. **Completion:** Seams are versioned and production policies are replayable;
    actual service distribution occurs only after a Bible trigger is measured.
12. **Rollback:** Revert policy/summary use on stale decisions, regret/SLA
    regression, rollback failure, or unjustified distribution.

### Step 30 — Close shippability and run the exact-HEAD supremacy gauntlet

1. **Objective:** Run all locally achievable gauntlet items from clean frozen
   candidates, collect physical/external receipts where authorized, retain
   honest blockers, and issue final Level A/B/C verdicts.
2. **Why:** Technical network development does not erase launch, money,
   security, privacy, operations, or evidence obligations.
3. **Current authority:** `ops/go-no-go.json`, readiness validators, Level-B
   release plan/doctor/launch/evidence chain, candidate checkpoint, runtime/
   network/receipt claims.
4. **Files/packages:** Release/readiness/ops/governance, full product and agent,
   bound evidence, final ledger and handoff.
5. **Dependencies:** Steps 1–29, except external actions may be prepared in
   parallel but cannot be fabricated.
6. **Safe parallelism:** Clean-clone source/evidence reproduction, independent
   review, external operator work, physical fleet/engine runs.
7. **Serial work:** Freeze exact candidate; candidate gate; external ordered
   evidence chain; governance approvals; release decision/promotion.
8. **Required proof:** All 24 supremacy items; 0 P0/unadjudicated P1 for target
   scope; external stranger buyer/supplier, Stripe CAD, signed release/update,
   staging, offsite restore, paging, rollback, 24h soak and qualified approvals.
9. **Mutation/test scope:** Change/authority during repairs; one full 84 campaign
   plus CI/race/evidence/checkpoint at coherent candidate; deep gate only at
   release boundary.
10. **Performance metrics:** Gate wall times, runtime/network curves, regret,
    verified outcome latency/cost/energy, money reconciliation, RPO/RTO,
    shippability score.
11. **Completion:** Exact-HEAD handoff answers whether V2 itself or adjacent
    machinery was built, lists every grade/receipt/blocker, and promotes only if
    the formal gates authorize it. Allowed residuals are only the Bible's four
    honest stop classes.
12. **Rollback:** Any P0/P1, evidence/money/security failure, performance claim
    without authority, mixed candidate, or external approval gap keeps Level B
    `NO_GO` and Level C prohibited; roll back activated policy/release.

## Mandatory mini-audit template

After each numbered step append the following to the progress ledger and this
plan's audit appendix (or a linked immutable receipt):

```text
STEP N MINI-AUDIT

Plan fidelity: __ / 100
Correctness: __ / 100
Evidence quality: __ / 100
Architectural simplicity: __ / 100
Performance impact: __ / 100
Security / money safety where applicable: __ / 100
Future-network compatibility: __ / 100

What the plan required:
What I actually implemented:
Exact proof:
What remains incomplete:
New defects discovered:
New concepts added:
Old concepts removed:
LOC / files: +__ / -__
Performance:
Verdict: COMPLETE | REPAIR_REQUIRED | BLOCKED_EXTERNAL | BLOCKED_BY_PREREQUISITE
```

## Audit appendix

### Step 1 mini-audit — exact-state reconciliation

```text
STEP 1 MINI-AUDIT

Plan fidelity: 100 / 100
Correctness: 100 / 100
Evidence quality: 100 / 100
Architectural simplicity: 100 / 100
Performance impact: 100 / 100
Security / money safety where applicable: 100 / 100
Future-network compatibility: 100 / 100

What the plan required:
Recover branch, HEAD, upstream, worktrees, dirty ownership, validation ownership,
canonical authority, performance/economics proof, readiness and external P1s.

What I actually implemented:
Froze an explicitly unbound worktree inventory at ae443f69, isolated network V2
on codex/network-v2, recorded the unrelated compile-breaking root edit, proved no
active validation owner, reconciled the divergent validation branch, classified
all eleven V2 authorities, proof gaps and eight external P1s, and preserved the
84/84 campaigns as historical ephemeral evidence rather than exact-HEAD proof.

Exact local inventory (UNBOUND citation only; not release or claim authority):
evidence/state/network-v2-reconciliation.json
docs/NETWORK_V2_EXECUTION_PLAN.md

What remains incomplete:
No exact-HEAD checkpoint; refactor/teardown is not integrated; readiness sources
disagree 84 vs 83; every open local architecture step and all eight external P1s.

New defects discovered:
The original main worktree's checkpoint cache edit does not compile; batch
measured selection/topology is post-commit shadow-only; seven canonical V2 type
names are absent; no Digital Twin/index/epochs/historical network replay exists.

New concepts added:
Network V2 progress ledger and explicit unbound reconciliation inventory.

Old concepts removed:
None.

LOC / files:
+1201 / -0 across plan, receipt and ledger (documentation/evidence only).

Performance:
No product performance changed. Two prior complete mutation campaigns remain
corroborated at about 259 s and 266 s; they do not bind current HEAD.

Verdict: COMPLETE
```

### Step 4 mini-audit — closed runtime-cell economics authority

```text
STEP 4 MINI-AUDIT

Plan fidelity: 100 / 100
Correctness: 100 / 100
Evidence quality: 100 / 100
Architectural simplicity: 100 / 100
Performance impact: 100 / 100
Security / money safety where applicable: 100 / 100
Future-network compatibility: 100 / 100

What the plan required:
Make physical duration, supplier entitlement, active-hour floor and admission
authority exact for the selected runtime cell, hardware, artifact and execution
identity; retain historical replay without letting stale evidence authorize new
money.

What I actually implemented:
Commit 0450da06 closes publication, placement, claim, lifecycle and settlement
authority as one chain. It binds exact cell/wire artifact, benchmark bytes,
engine build-policy/executable identity, hardware fingerprint, Unit/UnitScope,
conservative sustained throughput, whole-package workload-scoped power, current
catalogue pointer and activation epoch. Durable quote/job ingress rechecks
physical authority and maps refusal to 503; bound quotes retain frozen price
only while the current physical snapshot remains identical. Current distributed
task economics fail closed to one primary plus exact redundancy clones, preserve
and verify immutable input digests, and keep dynamic peer pins claim-eligible.
Settled supplier-liability evidence is read under one repeatable snapshot with
exact device/build/policy and canonical ledger shape. Checked-in superseded
benchmark identity remains honestly non-authorizing; TEST_ONLY authorities are
scoped to mechanics tests and never relabel physical evidence.

Exact proof:
clean detached candidate: 541c13575aa1f21356c128d0beceb888babb2c7c
full mutation gate: PASS 84 caught, 0 survived, 0 stale, 0 infrastructure
workers: 16 isolated worktrees and PostgreSQL clusters
elapsed: 287 s (hard budget: 299 s)
timing records: 84 unique; result set={caught}; pathways={PURE,DB}
terminal Step-4 audit: no remaining P0/P1
focused physical/publication/quote/activation/dynamic-pin/liability suites: PASS
schema reapplication and source restoration proofs: PASS

What remains incomplete:
No production routable cell is manufactured from superseded evidence. Level B
remains NO_GO and Level C remains NO_GO_PROHIBITED. Pre-refusal orphan object
cleanup and non-authorizing evidence/metrics hygiene remain lower-severity debt
for their owning later steps; they do not authorize runtime-cell money.

New concepts added:
Exact execution build-policy and device fingerprints; schedule physical
snapshots; current-pointer physical equivalence for bound quotes; uniform-task
economic authority; canonical input-digest dispatch; immutable activation
epochs and dynamic-pin eligibility locks.

Old concepts removed:
Model-level throughput/power as sufficient publication authority; generic
artifact intersection; unversioned short build tokens; raw/stale public price
surfaces; mutable/global activation-cache trust; heterogeneous average-task
economics presented as an exact supplier floor.

Performance:
The full exact candidate campaign completed in 287 s under the 299 s release
gate. The clean preflight is split into two deterministic complete DB lanes plus
independent unit/baseline proofs; mutation count and per-mutant isolation are
unchanged.

Verdict: RECONCILED_COMPLETE
```

### Step 2 mini-audit — canonical migration and deletion register

```text
STEP 2 MINI-AUDIT

Plan fidelity: 100 / 100
Correctness: 100 / 100
Evidence quality: 100 / 100
Architectural simplicity: 100 / 100
Performance impact: 100 / 100
Security / money safety where applicable: 100 / 100
Future-network compatibility: 100 / 100

What the plan required:
Map all eleven V2 concepts to actual symbols and callers; choose one canonical
owner; classify conversion allocation/copy and information loss; give every old
representation a retirement, compatibility or historical disposition.

What I actually implemented:
Created the eleven-concept deletion register. It keeps PricingDecision and
ServiceLease canonical, promotes Workload/Capability/Market/Runtime/Topology/
Verification/Settlement/Evidence authorities in serial order, renames the
current mode-only PlacementDecision before using that name canonically, and
records the exact conversion and deletion boundary for every legacy object.

Exact proof:
docs/NETWORK_V2_AUTHORITY_MIGRATION_REGISTER.md
The baseline symbol/caller census in
evidence/state/network-v2-reconciliation.json is an UNBOUND citation-only
inventory and supplies no release or performance authority.

What remains incomplete:
Every code migration in steps 6-16; this step is the governing deletion map, not
a claim that absent canonical authorities already exist.

New defects discovered:
No additional defect beyond the step-1 census. The register makes one concrete
loss explicit: ordinary verification reduces rich EmbeddingComparison evidence
to a boolean before normal receipt assembly.

New concepts added:
Canonical migration/deletion register; explicit ExecutionModeDecision rename.

Old concepts removed:
None yet; removal milestones are now fixed.

LOC / files:
+628 / -0 across the 552-line register, ledger and audit updates.

Performance:
No runtime change. The register identifies repeated project conversions, runtime
candidate slice copies, repeated DB joins and receipt assembly scans as metrics
that each migration must measure.

Verdict: COMPLETE
```

### Step 3 mini-audit — closed validation-plane integration

```text
STEP 3 MINI-AUDIT

Plan fidelity: 100 / 100
Correctness: 100 / 100
Evidence quality: 100 / 100
Architectural simplicity: 100 / 100
Performance impact: 100 / 100
Security / money safety where applicable: 100 / 100
Future-network compatibility: 100 / 100

What the plan required:
Bring the already-proven fast/authority/full/deep mutation gates onto the
implementation line without a new optimization tranche, inventory change,
parallel architecture, or full campaign.

What I actually implemented:
Cherry-picked the 19 mutation-infrastructure commits from 082985b7 through
844ab424 as b7ea98f6 through 2ed771c1. I deliberately excluded the preceding
13 commits, including the additive control/protocol DTO layer that declares
itself non-authoritative and would violate the V2 parallel-architecture rule.
One LFS-test conflict was resolved by applying only the commit's shared-ledger
fixture change to main's existing proof sequence.

Exact proof:
mutation-manifest: PASS 84 mutations
test-mutation-manifest: PASS 84 exact mutations and timing guards
mutation contracts: PASS 27 source contracts / 322 named invariant tests
test-mutation-contract-observer: PASS
test-mutation-preflight-cache: PASS
test-mutation-gate: PASS tier selection explicit and fail-closed
test-mutation-test-parallel: PASS all 84 cases uniquely sharded
test-with-isolated-test-db: PASS
go test -run 'Test(DevCheckpoint|MutationTemplate)': PASS in 0.262 s
parallel-protocol-import: ABSENT

What remains incomplete:
No new full 84 campaign or exact-HEAD checkpoint was run; both are intentionally
reserved for a coherent authority candidate. The two prior 259 s/266 s campaigns
remain historical evidence.

New defects discovered:
The source validation branch also carried decorative protocol DTOs beside legacy
authority. They were excluded rather than normalized into the programme.

New concepts added:
Fast, authority, full and deep gates; 84-entry manifest; weighted isolated
sharding; exact preflight/template caches; mutation contract observer.

Old concepts removed:
Sequential-only candidate validation as the normal checkpoint path.

LOC / files:
+6398 / -68 across 27 infrastructure/test files.

Performance:
Declared hard budgets: fast 120 s, authority 300 s, full 299 s, deep explicit
asynchronous. No new full measurement was made. Developer blocking remains zero
by isolated worktree/database design.

Verdict: COMPLETE
```
