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
8. **Required proof, per market shape.** Amended 2026-08-09 — see the shape note
   below. Push shapes prove the exact candidate and exclusion set, the
   contention/availability mode, each ceiling stage in the order it is applied, a
   deterministic tie-break under the stated mode, duplicate-supplier refusal,
   stale/forged offer rejection, frozen accepted economics and historical replay.
   Pull shapes prove the claiming worker, the hard filters, the deferral signals,
   the floor and the selected task — and make **no** book-clearing claim.
9. **Mutation/test scope:** Reverse prices, duplicate identity, remove ceiling,
   change currency, stale benchmark, hide provider failure.
10. **Performance metrics:** Book acquisition and clearing p50/p95/p99,
    shortlist size, allocations, verified-outcome cost delta.
11. **Completion:** Every accepted path binds a **shape-validated**
    MarketDecision digest — push paths a ranked book plus availability mode, pull
    paths a claim-time eligibility snapshot — and each legacy receipt projects
    losslessly from its matching shape.
12. **Rollback:** Revert lane wiring on atomicity, money, deterministic-order,
    or latency regression beyond the declared admission budget.

### Step 7 shape note — amended 2026-08-09 after reconnaissance

The original wording assumed one clearing story across all three lanes. The code
does not have one, and writing a single MarketDecision over all of them would
produce an authoritative-looking record for a lane whose semantics it does not
fit. Three corrections, each grounded in the current code:

**Batch is a pull market.** Workers pull work and eligibility uses fleet-relative
deferrals (`cheaper_class_online`, `cheaper_ask_online` in the scheduler); no
market receipt is produced at all. The earlier requirement — that batch create a
MarketDecision before or at its first capacity reservation rather than
reconstructing the book from later rows — is **struck**. Batch freezes a pull
eligibility snapshot at claim and does not synthesise a buyer offer book.
Quote-time supply remains observational unless claim validates a reserved
shortlist, which is out of scope unless the product changes.

**Realtime clearing is partially honest today, so Step 7 is repair, not
greenfield.** `selected_rank` is economic `row_number()` over the full eligible
set, and when `SKIP LOCKED` contention means rank 2 wins, the receipt records
rank 2 rather than rewriting it to 1 — that part is truthful. What is not:
`SelectionReason` still reads "lowest verified-outcome cost" when
`selected_rank > 1`, and no peer list, lock exclusion or contended set is
recorded at all. So the repair is to add the book and the availability mode, and
to stop the prose asserting a clearing that did not happen. Service lease is
different again: it locks every candidate with blocking `FOR UPDATE` and walks
ranks, which is genuine rank-1-under-ceiling clearing bought with serialisation.
That difference is a first-class fact the decision must record, not smooth over.

**Ranking currency is a behaviour change, not part of this step.** Realtime ranks
on float USD in SQL and applies the *buyer-declared* ceiling after selection —
though its profile-rate filter is already pre-rank, so "ceilings come after
selection" is true only of the buyer ceiling. Service lease already ranks in
exact settlement nanos. Moving realtime ranking into nanos and making the buyer
ceiling a pre-rank filter changes money-adjacent admission behaviour and
therefore carries money-proof obligations; it is **demoted** out of Step 7 and
belongs to a later step that can pay for it. Step 7 records the pipeline
truthfully first.

Consequently MarketDecision is one canonical concept with an explicit
`market_shape` discriminator and a per-shape body — `PUSH_ORDER_BOOK` and
`PULL_ELIGIBILITY_SNAPSHOT` — not one nullable-field type spanning both. Build
order: realtime `PUSH_ORDER_BOOK` alone, projecting the legacy receipt losslessly
from it; then service lease onto the same shape; then batch as the pull shape.
Shipping "one MarketDecision for all three lanes" first would freeze a fake batch
book or omit realtime contention, and that is the wide lie this note exists to
prevent.

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

### Step 8 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `c9c69668`, not
carried over from the scout report.

**Promotion is refused by construction, on two independent grounds. Step 8 cannot
complete on "promotion and instant rollback".** Gate v4 refuses *every* promotion
because no durable matched incumbent/challenger execution-pair authority exists:
`promotionMatchedPairAuthorityRefusal` in `control/runtime_cell_promotion.go`
states that shadow consideration plus independently aggregated jobs is not
matched-pair evidence. Separately — and this one survives even if matched pairs
are built — `control/activation_policy.go:1326-1333` refuses because
`CellPromotionEvidence` covers one exact scope (job type, model, tier, hardware
class, hardware identity, latency class) while cell lifecycle is **global across
traffic scopes**, and no global-coverage authority exists. Two different missing
authorities, so building one does not unblock the other.

Consequently the promotion criterion is **struck from Step 8** and becomes its own
obligation: a promotion coverage model (matched-pair execution authority plus a
rule mapping narrow evidence to global lifecycle, or a narrowing of lifecycle to
match evidence scope). Step 8 keeps the part that is reachable: freeze the runtime
choice as accepted authority and make every physical execution cite it.

**There is no single object to rename, so this is a transactional re-home.** The
facts Step 8 wants to freeze are currently owned by five different authorities:
engine/cell freeze on `WorkloadDecision`; benchmark/build/hardware on
`PlacementRequirement.PerformanceAuthority`; money on `PricingDecision.RuntimeCell`;
the activation epoch, which is ephemeral at write; and the selection basis, which
is a post-commit shadow row.

**Correction, 2026-08-09 — this note first claimed the full Step 11 chain was a
load-bearing prerequisite for Step 8. That was wrong, and the error was mine.**
Four of those five facts are *already* frozen atomically: `SubmitJobTx` writes
`workload_decision`, `compute_plan`, `placement_requirement` and
`pricing_decision` with all four digests in a single INSERT
(`store_jobs.go:499-519`). Nothing needs moving for them.

The one fact outside the transaction is the selection basis, and it is outside
**deliberately**. `api.go:1676-1683` places shadow selection after commit and
explains why: "a selector that could refuse a submit would be a router, and this
one is not allowed to route." Dragging the shadow into the accept transaction to
satisfy a wording would convert an observational selector into an admission
authority — a larger architectural change than Step 8, and one nobody has asked
for.

The resolution is that these are two different things and the plan was conflating
them. The **accepted** basis already exists inside the transaction: it is the
lifecycle-ranked freeze `rankAndFreezeAdmissionCell` produced, carried on
`WorkloadDecision`. The **shadow** is a post-commit observation of what a
measured re-ranking would have chosen. So RuntimeDecision names the in-transaction
freeze as the accepted basis, records honestly that the basis was a lifecycle
ladder rather than a measurement, and leaves shadow observational and explicitly
non-authoritative.

Step 8 therefore does **not** block on Step 11. What it needs from Step 11 is
narrower: the accepted RuntimeDecision must be written in the same transaction as
the rest of the chain, which `SubmitJobTx` already demonstrates is achievable on
the batch lane.

**There is no production multi-engine tournament to select from.** Ordinary
routability is empty under honest BIND; the advertised set is a singleton by
design. Step 8 must record the singleton's identity honestly and must not describe
itself as choosing between engines. A tournament is a later step's work, and it
needs supply that does not exist yet.

**One factual correction to the step text.** Point 3 says Step 8 replaces
authoritative use of "the first `WorkloadRuntimeCandidate`". Production does not
take the first candidate: `rankAndFreezeAdmissionCell`
(`control/workload_classification.go:258-293`) freezes a lifecycle-**ranked**
singleton, promoting `chooseShadowCell` onto the freeze path. The replacement
target is that ranked freeze, and the ranking it applies is part of what
RuntimeDecision must record.

**Completion, restated:** every physical execution cites an immutable accepted
RuntimeDecision naming engine, cell, model/artifact, precision/quality, hardware
class, runtime revision, benchmark authority and activation revision; shadow code
cannot become money authority; and the decision states plainly when its selection
basis was a lifecycle ladder rather than a measurement. Promotion and rollback
move to the promotion-coverage obligation.

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

### Step 9 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `c9c69668`.

**Four things own the word "placement", and the one Step 9 most needs has no
object at all.** `PlacementDecision` (`control/execution_mode.go:103`) is supply
*mode*. `PlacementRequirement` (`control/quote.go:363`) is *eligibility*.
`RealtimePlacementPlan` (`control/realtime_placement.go:24`) is a *host topology*
plan. The actual worker choice exists only as claim / authorize / lease SQL and is
not represented by any type. So promoting the mode object to canonical captures
none of the worker assignment, and the claim path never validates a frozen worker
set. Step 9's real work is binding the fourth meaning, not renaming the first.

**"The selected worker belongs to a frozen candidate set and epoch" is struck for
batch,** for the same reason it was struck in Step 7: batch is a pull market over
`SKIP LOCKED`, so the candidate epoch is the live queue × fleet at poll time and
there is no epoch object to bind. Batch binds a claim-time eligibility snapshot
recording what was true when the worker pulled. Realtime and service lease can
carry a genuine frozen candidate set; batch may not pretend to.

**Correction to the earlier reconnaissance: predicted-vs-actual is NOT absent, and
Step 9 must not build a second one.** The scout reported no predicted-vs-actual
surface anywhere. That is wrong, and building on it would have created exactly the
duplicate authority this programme forbids. What exists at HEAD:

- `jobs.eta_secs` (predicted completion at submit) and `jobs.eta_secs_raw` (the
  uncalibrated p50 kept only as the learner's denominator), `schema.sql:773-778`;
- `quotes.eta_p50_secs` / `eta_p90_secs` and their raw counterparts,
  `schema.sql:1312-1323`;
- `eta_calibration(predicted_secs, realized_secs)` scoped by job type, tier,
  model_ref and input depth band, `schema.sql:1714-1735`, written by
  `control/plan_actuals.go`;
- `SelectorLiabilityRegret` (`control/runtime_cell_cost.go:1105`), a working cost
  regret surface with exact-pair scoping, exposed at
  `GET /admin/runtime/selector/regret`.

So a calibrated total-duration predictor and a cost-regret surface already exist.
**What is genuinely absent is the decomposition:** `schema.sql` contains no
queue-wait, startup, or cold-start column anywhere. Predicted-vs-actual therefore
resolves to total duration only, and the addendum's per-phase regret (queue,
startup, execution, transfer) has no substrate.

Step 9's regret work is consequently *decomposition plus placement-plane binding*,
and it must follow the pattern `runtime_cell_cost.go` already established rather
than inventing a parallel one:

1. derive from facts the execution and money paths already persist — a query, not
   a new writer and not a second table, because copies drift;
2. scope exactly and refuse to aggregate across incomparable scopes;
3. name unknown components `unknown_*` rather than defaulting them to zero,
   because a zero reads as a measurement and is not one;
4. require a sample floor before a measurement is treated as measured, and fall
   back to the declared ladder below it.

**Locality is soft belief and must be recorded as such.** Stale
`worker_prefix_state` can change the winner inside a cost class without appearing
on any receipt, and correction is post-serve. A PlacementDecision that records a
locality-driven choice must record the freshness of the state it believed.

**Fallback has no single existing pattern to promote.** Realtime keeps an
immutable worker; service lease keeps the lease and takes a new worker; batch
requeues or hedges. A single canonical `fallback` field would flatten three
different lane semantics into one that fits none. Use a per-lane discriminator,
exactly as Step 7 uses `market_shape`.

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

### Step 10 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**Topology is not a missing planner. The planner already exists; the accept
binding does not.** `TopologyPlan` and `PlanTopology` choose shape and then
delegate supply-lane physics to `ChooseExecutionMode`
(`control/topology_planner.go:44-55`, `:107-120`). WAN-tight and unmeasured-fabric
refusal is already coded (`control/execution_mode.go:170-177`). Treating Step 10
as inventing that refusal would re-author a pure function that already decides
correctly.

**"Promote TopologyPlan as the canonical core" is struck as a rename path.**
Production accept paths do not bind `TopologyPlan` except as post-commit shadow
JSON and as a non-authority fabric projection. Batch stores topology only on
`runtime_shadow_selections.topology_plan` after `SubmitJobTx` commits
(`control/api.go:1676-1711`; `control/runtime_shadow_selection.go:529-561`;
schema at `control/schema.sql:6428-6439`). That write cannot veto admission, and
its error is logged and dropped on purpose. Promoting the type name without
moving the write into the accept transaction recreates a lying authority: a
digest-looking object that money and placement never consulted.

**Batch does not choose among multi-device topologies today.** Accepted batch
work freezes `independent_task_fanout` at tensor-parallel degree 1
(`control/workload_classification.go:408-410`). Shadow then calls `PlanTopology`
with `Fabric: FabricUnknown` (`control/runtime_shadow_selection.go:177-181`). The
comment above that call claims "nothing in this tree measures link bandwidth or
latency between workers" (`:172-176`). That comment is **false as written**: the
tree does measure fabric links and synthetic collectives
(`control/fabric_measurement.go`, `control/fabric_topology.go`). What is true is
that **batch admission does not consume those measurements**. Passing
`FabricUnknown` is honest for the batch path; claiming the tree has no fabric
measurement is not.

**"Measured local gang admission" is struck as a Step 10 completion criterion.**
Fabric evaluations force `LocalClusterAdmissible: false` at construct and persist
`local_cluster_admissible=false` (`control/fabric_topology.go:146`, `:327-332`).
Statuses are mesh/collective measurement strings, never
`LOCAL_CLUSTER_ADMITTED_V1` (`:283-289`; constant at
`control/fabric_topology_planner.go:18`). The planner gate itself documents that
current evaluations always map to `FabricUnknown` and refuse tightly coupled work
(`control/fabric_topology_planner.go:58-62`). The evaluation lists the missing
authorities: gang scheduler, customer collectives, topology pricing
(`control/fabric_topology.go:294-298`). Until those exist, the correct production
outcome remains refuse `LOCAL_CLUSTER`, which the planner already does. Completing
Step 10 by inventing admitted gang placement would be a false record.

**Realtime host topology is already frozen; do not replace it with a second TP
decision.** `RealtimePlacementPlan` freezes interconnect, configured/admitted
tensor parallel, and replica execution mode (`control/realtime_placement.go:24-46`).
It is built at offer registration (`control/realtime_store.go:337-344`) and
copied/revalidated into the authorize transaction (`:1068-1076`, insert
`:1139-1163`). Comments state single-host TP ranks remain one replica, never
`LOCAL_CLUSTER` evidence (`realtime_placement.go:41-43`). Private
`tensorParallelPlan` (`control/multi_gpu_admission.go:89-94`) is pure host
arithmetic feeding that plan — keep it private. Retiring "embedded realtime
topology fields" without preserving the placement-plan digest would destroy the
lane's actual topology authority, not remove duplication.

**`ProjectIRTopology` is already labeled non-placement.** It is compiler shape
evidence with no fabric, worker, price, or capacity fields
(`control/project_compiler.go:78-90`). Later cleanup may rename it to a
requirement; promoting it to accept authority would be the reverse error.

**`TopologyDecision` as a Go type, digest, and accepted-job column is ABSENT.**
What Step 10 still owes, by lane:

1. **Batch:** an immutable accept-time topology record inside the accept
   transaction — which for today's product may honestly be the singleton
   (independent fan-out degree 1 + fabric class + refusal of tight multi-host
   coupling) written as a job field, not only a shadow JSON blob.
2. **Realtime:** cite the existing `RealtimePlacementPlan` digest as host/topology
   authority; do not invent a parallel decision that re-decides TP.
3. **Service lease:** freezes worker and pricing, not topology; record that
   absence honestly rather than synthesising a multi-host plan.

**Performance of topology planning remains UNMEASURED.** Do not invent p50/p95
budgets here.

**Completion, restated:** every accepted execution cites either (i) the frozen
host/topology authority that already exists for that lane, or (ii) an immutable
refuse/shape record written in the accept transaction. Shadow may reference
digests; it must not be the sole record of an accepted topology claim. Measured
local gang admission and "promote TopologyPlan by rename" are not completion.

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

### Step 11 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**"Atomic decision chain wholly ABSENT" overstates the gap.** Money and substantial
plans already share accept transactions per lane. What is ABSENT is a single
canonical Market → Runtime → Placement → Topology chain object with cross-linked
digests of those four types — and those four types are not all present as
production authorities (Step 7/8/9/10 notes). Claiming total absence of atomic
accept writes would produce a second "accept" writer beside the ones that already
reserve money and freeze digests.

**What each lane already freezes in one transaction:**

- **Batch `SubmitJobTx`** (`control/store_jobs.go:361+`, insert digests
  `:499-519`): activation guard, prepaid/quote checks, `workload_decision`,
  `compute_plan`, `placement_requirement`, `pricing_decision`, economic plan,
  tasks. **Not** in that TX: shadow topology/selection
  (`control/api.go:1676-1711`), MarketDecision, TopologyDecision, or the worker
  (worker is pull claim — `control/scheduler.go:1140+`).
- **Realtime `AuthorizeRealtimeContract`** (lock order documented at
  `control/realtime_store.go:892-906`; contract insert `:1139-1163`): funding,
  offer capacity, worker, placement plan + sha, PricingDecision + sha,
  `market_clearing`. Host placement was frozen earlier at offer upsert
  (`:337-344`); authorize revalidates, it does not re-plan. Mid-TX failure rolls
  funding and capacity back together.
- **Service lease `CreateServiceLease`** (`control/service_leases.go:664-691+`):
  `FOR UPDATE` on the profile+region offer book, PricingDecision, prepaid
  ceiling, worker/capacity, market detail on the activation event. Single TX;
  rollback restores capacity and prepaid.

**The real tear is post-commit authoritative explanation on batch.** After
`SubmitJobTx` commits, shadow selection may fail and the job still lives
(`control/api.go:1679-1682`; `RecordShadowSelection` uses `ON CONFLICT DO NOTHING`
at `control/runtime_shadow_selection.go:554`). Shadow cannot undo money. If a
fact is described as an accepted decision, it must be written inside the accept
transaction or demoted to observational. Leaving it post-commit-only and calling
it accepted is the false record this step exists to prevent.

**"Worker choice is implicit later" is true for batch only.** Realtime and service
lease freeze the worker in the accept transaction. Batch freezes eligibility at
claim time under pull semantics (`ClaimTasksTx`); that is a separate boundary, not
a chain bug by itself. Extending claim as if it were the accept chain freezes the
wrong market shape (see Step 7).

**Full Step 11 is not a hard prerequisite for Step 8.** The Step 8 shape note
already corrected the earlier claim that RuntimeDecision required the four-node
chain. Four of the five runtime-relevant facts are already inside `SubmitJobTx`;
the fifth — measured shadow selection basis — is post-commit **deliberately** and
must not be dragged into accept to satisfy wording without turning an
observational selector into a router. Step 8 freezes the in-transaction lifecycle
ranked cell. Step 11 still owns: moving any remaining *accepted* decision digests
into the lane accept TX, linking digests that **exist for that lane**, and proving
crash/retry leaves no orphan prepaid, capacity, or accepted job without its
declared digests.

**Do not invent MarketDecision or TopologyDecision solely to fill a four-name
diagram.** Batch MarketDecision remains a pull eligibility snapshot at claim (Step
7). Topology binding follows Step 10's amended absences. Cross-link what is real;
refuse decorative chain rows that restate digests under a second algorithm.

**Admission/claim lock-wait and chain write budgets for this step remain
UNMEASURED** as a Step 11 deliverable. Existing claim duration instrumentation is
not a substitute for a measured chain gate.

**Completion, restated:** every accepted execution is replayable from digests
written in its lane accept TX; shadow is observational or references those
digests; claim-time batch assignment remains a documented pull boundary with its
own eligibility receipt, not a silent rewrite of accept-time decisions. The
canonical four-decision chain root is still ABSENT and is completed only when the
amended Steps 7–10 objects that actually apply per lane exist and link.

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

### Step 12 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**`VerificationContract` as a Go type is ABSENT.** Authority is split across
buyer knobs, free strings, governed classes, sampling pins, work plans, decision
effects, and a rich comparator whose full result is dropped on the ordinary path.
Building a greenfield contract that also becomes outcome/money authority would
duplicate objects that already hold those roles under other names.

**What already binds (do not re-author as a second digest authority):**

- **Governed class + policy revision** — `SAMPLED|REQUIRED|HONEYPOT|REDUNDANT|REPLAY`
  and `verify-class-v1` (`control/verification_class.go:23-50`), frozen on the
  compute plan (`control/compute_plan.go:76-87`) and disclosed on task receipts
  (`control/receipt.go:53-59`).
- **Sampling pin** — policy `hmac-reputation-v1`, probability and selection on
  `VerificationWorkPlan` (`control/verification_work_plan.go:17-32`).
- **Attempt outcome + ledger plan** — `VerificationWorkPlan` carries
  `Decision`, `Settlement`, and `DecisionSHA256` (`:22-32`, `:69-75`); digest
  function at `control/verification_apply.go:844`.
- **Failure consequences** — `VerificationDecision.Effects` kinds
  (`control/verification_plan.go:29-58`), applied after plan.
- **Embed equivalence** — `embed-cosine-v2` thresholds and full
  `EmbeddingComparison` (`control/embedding_comparator.go:29-45`, `:145-173`).

**What is partial or misleading today:**

- **`VerificationPolicy` is not the verification contract.** It is three buyer
  knobs: redundancy frac, honeypot frac, payout hold secs
  (`control/types.go:140-144`).
- **Cell `Verification` and `WorkloadDecision.VerificationStrategy` are free
  composite strings**, not frozen check proofs (`control/runtime_authority.go:134`;
  strategy builder `control/workload_classification.go:316-327`). They can name a
  method while SAMPLED selection is false. That is the buyer-overrun risk: the
  strategy/cost line, not the aggregate `fully-verified` label alone.
  `fully-verified` requires every delivered chunk to have been verified under the
  aggregate rules (`control/types.go:421-424`); task receipts can disclose
  `verification_selected` (`control/receipt.go:53-59`).
- **Rich comparator output is not retained on the ordinary path.**
  `resultsAgree` keeps only `.Passed` (`control/verification.go:581-584`) while
  `EmbeddingComparison` was designed to persist full diagnosis
  (`embedding_comparator.go:145-149`). Threshold/revision/reference are therefore
  **not** acceptance-bound on one ordinary-path object today.
- **Named recompute policy is ABSENT.** Only effect kinds like requeue/tiebreak
  exist (`control/verification_plan.go:36-37`); there is no pre-acceptance policy
  for how many times / when to re-execute.
- **Cross-lane:** realtime receipt carries a free `verification` string
  (`control/realtime_store.go:2482`), not the batch work-plan machinery.

**Step 12 is therefore bind acceptance-time verification identity, not invent a
second outcome authority.** Freeze and cite: governed class + class policy
revision, sampling policy identity, evaluator kind (byte-exact vs embed
comparator revision + frozen thresholds/reference digest when applicable), and
the failure-consequence **vocabulary** apply is allowed to emit. Keep
`VerificationWorkPlan` / `VerificationDecision` / `decision_sha256` as attempt
outcome authority. Retain rich comparator outcomes where money or buyer claims
depend on them. Project legacy strategy strings from the bound identity without
policy loss. Refuse settlement against a class/threshold other than the pinned
work plan.

**Verification latency and sampling-ratio production budgets remain UNMEASURED**
as programme gates for this step.

**Completion, restated:** no buyer-visible "verified under X" claim without either
a check selection record or an explicit SAMPLED-not-selected status; no second
digest that competes with `decision_sha256` for outcomes; recompute remains
ABSENT-as-policy until product requires a named policy. Ledger status PARTIAL is
correct: split authorities exist; the single pre-acceptance contract object does
not.

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

### Step 13 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**`SettlementPlan` as a Go type is ABSENT. Batch money authority is not.** Building
a SettlementPlan that re-states buyer/supplier/provider nanos already fixed-pointed
on `PricingDecision` would create a second money truth. Renaming
`PricingDecision` or `ContributionSettlement` to `SettlementPlan` is likewise
refused — that is duplicate canonical authority under a new label, not completion.

**What already binds money (extend; do not replace):**

- **`PricingDecision` / `FixedPointPricingDecision`** — accepted price/cost
  forecast authority, multi-mode, with explicit `ContributionStage`
  (`control/pricing_decision.go:86-110`, components including verification cost
  at `:183-191`). Comments state pricing never becomes true net by completion
  alone (`:105-109`).
- **`ContributionSettlement`** — **only** batch-job authority allowed to call
  contribution "true net" (`control/contribution_settlement.go:16-19`, stages
  `:22-25`, struct `:54-59`). Keyed by subject + `PricingDecisionSHA256` +
  currency (`:35-42`). True net is structurally absent outside FINAL
  (`:54-55`). Loaded for jobs via `ContributionSettlementForJob` (`:1112-1138`).
  Invoice projection treats it as canonical; legacy contribution view projects
  only from it (`control/store_billing.go:157-159`).
- **Verification attempt ledger plan** — `VerificationWorkPlan.Settlement` digested
  with the decision (`control/verification_work_plan.go:29-31`).
- **Holds** — supplier credit `PayoutHeld` + `ReleaseAt`, minimum hold 24h
  (`control/payment.go:92-99`, `:115-128`); buyer policy hold secs on
  `VerificationPolicy` (`control/types.go:143`).
- **Lane terminals** — `RealtimeSettlement` (`control/realtime_store.go:1267-1276`)
  and receipt money fields; `ServiceLeaseSettlement`
  (`control/service_leases.go:231-245`) with true-net status often blocked
  (`UNKNOWN_ECONOMIC_FINALITY_BLOCKERS` at `:220`).
- **Ledger idempotency** — per write path (e.g. lease
  `insertServiceLeaseLedgerEntryTx` / `insertLedgerEntryIfAbsentByRefTx` at
  `control/service_leases.go:288-305`; refund/reversal keys required in
  `control/payment.go:385`, `:456`). Do not invent a second write-idempotency
  authority.

**Critical lane split:** `ContributionSettlement` is batch-job only. Realtime and
service lease have their own settlement structs and do **not** share that true-net
reducer. One SettlementPlan body spanning all three lanes would freeze a fake
shared lifecycle the code does not run.

**What is genuinely absent:**

- One accepted-before-execution object that freezes refund/dispute/payout
  **state-machine rules** (as opposed to amounts + post-facto reduction from
  ledger/dispute rows).
- Cross-lane settlement identity that makes batch / realtime / lease prove the
  same liability state machine.
- Verifier **entitlement** as a first-class settled party (verification appears as
  a **cost component** on pricing — `pricing_decision.go:184` — not a separate
  verifier payout plan).
- Pre-execution "true net" — correctly refused today; true net only at FINAL.

**"Every liability transition authorized by one pre-execution SettlementPlan" is
struck as written.** Transitions are authorized by pricing digest + lane writers +
ledger code paths + post-hoc `ContributionSettlement` reduction. Pre-binding true
net is unreachable by construction of the forecast/settlement split.

**Work that remains real (if product still requires it):** (a) document and, only
if needed, freeze a refund/dispute/payout state-machine **revision identity** cited
by writers; (b) make realtime/lease finality status as explicit as batch FINAL
blockers; (c) ensure every liability transition cites an existing authority digest
(pricing and/or contribution settlement and/or lane settlement id) — not a parallel
plan body that copies amounts. If audit finds no lifecycle gap after that census,
mark the step reconciled documentation rather than implement a type.

**Settlement tail and time-to-payable budgets remain UNMEASURED** as new Step 13
gates unless measured under existing authorities.

**Completion, restated:** 100% nano reconciliation under **existing** authorities;
zero second fixed-point tables; no true-net promotion from forecast. Status
language: PARTIAL — amounts canonical for batch; lifecycle plan object absent;
not a greenfield SettlementPlan build.

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

### Step 14 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**`EvidenceEnvelope` as a transaction chain root is ABSENT. File evidence binding
is not a substitute, and calling it one would produce a false partial.**

What exists today:

- **`ReceiptIdentity` + `scripts/lib/evidence_binding.py`** — eight-field
  **producer identity** for files under `evidence/` (`control/receipt_identity.go:75-86`;
  Python mirror `scripts/lib/evidence_binding.py:1-39`). BOUND/UNBOUND/WITHDRAWN.
  This is file provenance, not request-to-money chain authority. The plan body
  already says so; recon agrees.
- **LFS corpus integrity** — separate from transaction envelopes.
- **Per-object decision digests** — workload/compute/placement/pricing on
  `ReceiptAuthority` (`control/receipt.go:21-26`); realtime placement and pricing
  digests on contracts; verification `decision_sha256`. Digests exist **per
  object**. There is no append-only envelope node list linking
  market → runtime → placement → topology → execution → verification → settlement
  as one root.
- **Lane receipts** — `ClearingReceipt`, `RealtimeReceipt`, `ServiceLeaseReceipt`,
  project receipts as separate roots. Batch buyer receipt is **live multi-query
  assembly** then `assembleClearingReceipt` (`control/api.go:3940-3987`;
  `control/receipt.go:112-139`). Re-read re-aggregates verification/dispute/invoice
  state; historical immutability of the assembled receipt is not guaranteed by a
  stored root.
- **Funding `execution_envelopes`** — prepaid/funding holds
  (`control/accounts.go`, `control/store_prepaid.go`), **not** EvidenceEnvelope.
  Name collision risk only; unrelated authority.

**Therefore Step 14 is a genuine greenfield for the transaction chain, not a
rename of evidence binding or of per-decision SHAs.** Closest cousins must be
**cited**, not reimplemented:

1. Do not reimplement file binding as "EvidenceEnvelope" for files.
2. Envelope nodes cite existing authority digests; they do not recompute alternate
   digests of the same facts.
3. Realtime already freezes some clearing money identity on the contract
   (`RealtimeMarketClearingReceipt` at `control/realtime_store.go:249-277`); cite
   it.
4. Keep funding envelopes out of this type namespace in prose and code.

**Completion criterion "no lane assembles authoritative identity from mutable
current rows" is not met today** for batch ClearingReceipt. The work is: define
node kinds and digest linkage; write nodes at state transitions (not only at GET);
project existing lane receipts from a stored root when present; refuse
authoritative identity built only from mutable current rows for **new** outcomes;
leave producer-identity fields on the file layer.

**Envelope write/read latency budgets remain UNMEASURED.** Do not invent them
without measurement.

**Ledger: ABSENT is correct for the transaction chain.** Do not mark partial solely
because evidence binding or per-object digests exist.

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

### Step 15 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `ed1549ca`.

**Batch cache-aware routing already exists. Treating Step 15 as "expand
two-worker locality into routing" rebuilds a second scheduler over soft belief.**
Production `ClaimTasksTx` already ranks on believed prefix depth and model warm
**below** cost/ask: `cheaper_class_online ASC, cheaper_ask_online ASC, …,
warm_prefix_depth DESC, warm_for_task DESC` (`control/scheduler.go:1093-1102`).
Prefix depth is computed from `worker_prefix_state` joined to `job_prefix_chain`
with a 90s TTL (`:758-786`, `last_seen_warm > now() - interval '90 seconds'` at
`:779`). Model warm is a 60s `EXISTS` on `worker_model_state` (`:751-755`). The
pure twin is `RankByCostThenPrefixAffinity` (`control/prefix_placement.go:56`);
wiring tests pin cost-before-prefix and forbid warm-as-hard-filter
(`control/prefix_placement_test.go`, `control/prefix_routing_wiring_test.go`).

**Locality is a belief, not a mirror of the engine.**
`control/prefix_routing.go:49-53` states `worker_prefix_state` is a *model* of
residency: Merc cannot see the engine KV. Tables exist
(`control/schema.sql:3364` `worker_prefix_state`, `:3444` `job_prefix_chain`;
model table at `:1689`). Warmth is written **after** durable commit:
`completeTaskTx` → `observeAndMarkPrefixForCommit` → optional
`CorrectPrefixBeliefFromObservation` then `markWorkerWarmForJob`
(`control/store_tasks.go:516-517`; path `control/prefix_routing_path.go:144-166`,
`:87-94`). TTL is `prefixWarmTTL = 90s` (`control/prefix_routing.go:104`);
stale rows are swept at 20×TTL (`:203`). Model residency is heartbeat-written
(`control/store_workers.go:515-554`).

**Stale `worker_prefix_state` can change the winner inside a cost class without
appearing on any receipt.** `warm_prefix_depth` is ORDER BY only; it flips which
eligible worker wins when cost/ask tie. Claim `RETURNING` deliberately does **not**
expose prefix warmth to the worker client (`control/prefix_routing_wiring_test.go:434-442`;
production `RETURNING` at `control/scheduler.go:1125`). No batch placement or
settlement field freezes "I chose worker W because believed depth D at freshness
T." Because warmth is post-commit, concurrent claimants can pile onto a stale warm
row until observation correction or TTL. Realtime freezes **offer-declared**
warmth on the selected rank
(`RealtimeClearingRankingInputs.Warmth` at `control/realtime_clearing.go:56-74`;
receipt `control/realtime_store.go:258-279`) — that is a different warmth channel,
not `worker_prefix_state`.

**The two-worker bound receipt is same-host process placement, not network
supremacy.** `evidence/perf/prefix-two-worker-latest.json` labels "two workers
(same host)" and lists `not_exercised`: two suppliers on two machines, cross-supplier
cache-aware routing, multi-region, paid cloud. Re-proving KV reuse on one Mac does
not complete a multi-supplier locality claim. Programme ledger status
`PARTIAL_PHYSICAL` ("two-worker prefix proof; other residency classes absent") is
the honest ceiling of what that receipt authorizes.

**What is already done (do not re-author):**

- Prefix belief + claim ORDER BY + observation correction + cost-before-warmth
  tests.
- Model warm tiebreak via `worker_model_state` / `warm_for_task`.
- Bound physical cold/warm prefix measurements on this host class (two-worker +
  single-worker metal evidence under `evidence/perf/`).

**What is genuinely ABSENT (name absences; do not invent tables):**

- PlacementDecision / accepted receipt field that records locality belief, depth,
  freshness (`last_seen_warm` age or epoch), and whether affinity moved the winner.
- Governed Capability residency epoch for locality (Step 6 ambition; not live).
- Control-plane residency / routing substrate for **adapter, dataset, render
  asset, container layer, compiled kernel, preprocessing**. Render assembly
  **explicitly refuses** asset locality
  (`control/render_assembly_receipts.go:70`, `:186`). LoRA evaluation is
  non-executable for adapter deployment
  (`control/lora_evaluation_receipts.go:19`).
- Production money path attributing batch prefix hits as `prefix_reused_input`:
  the billing class exists (`control/billing_classes.go:23`) and
  `RecordTokenAccounting` exists (`:126-127`), but **no non-test production
  caller** writes it.
- Realtime prefix-trie routing (absent; offer warmth only).

**"Canonical placement consumes governed locality for all implemented residency
classes" is misleading as written.** Implemented for batch routing today: **prefix
belief + model warm**. Everything else named in the step objective is absent or
agent-local. Feeding non-existent classes into Capability/PlacementDecision would
mint empty authorities — the Step 9-class defect.

**Work that remains real:**

1. When affinity moves the winner inside a cost class, freeze on the accepted
   placement/claim path: worker id, believed depth and/or model-warm bit,
   freshness (`last_seen_warm` age or equivalent), and observation-correction
   status. **Required proof, not optional polish:** "when affinity moves the
   winner, the receipt shows the freshness it believed." Without that, soft
   belief is an invisible second scheduler.
2. Keep cost outranking warmth (already tested); stale/forged warmth must not
   cross cost class.
3. Treat other residency classes as **named absences** until a real writer
   exists. Do not scaffold adapter/dataset/render/container/kernel/preprocess
   tables for this step.
4. Do not promote the two-worker receipt into a multi-supplier or network claim.

**Completion, restated:** Batch placement either records locality belief when it
decided, or refuses to claim "cache-aware" on the receipt. Cost-before-warmth
holds. Other residency classes remain named absences. Status language: PARTIAL —
batch prefix/model routing live; belief accountability open; multi-class governed
locality absent.

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

### Step 16 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**There is no canonical accepted `Workload` type. Promoting `ProjectWorkloadIR`
into one while leaving the admission freeze path alone would create a second
classifier beside production.**

What exists:

- **`ProjectWorkloadIR` / `ProjectIRStep`** — versioned **proposal** graph, not an
  executable JobManifest (`control/project_compiler.go:29-31`, steps with
  `depends_on`, estimates, topology, checkpoint, verification at `:51-68`).
- **`project_compile_receipts`** — buyer-scoped immutable IR JSONB evidence
  (`control/schema.sql:6736-6748`). Schema comments say the control plane keeps
  IR identity and ceiling only on the order (`:6730-6735`); compile receipts **do**
  store the graph body for that buyer (`ir JSONB` at `:6748`).
- **`ProjectOrder`** — server ceiling + `ir_sha256`, no source graph body on the
  order (`control/project_order.go:18-31`).
- **`WorkloadDecision`** — **single-job** admission authority, frozen on jobs and
  exposed on receipts (`control/workload_classification.go:67-71`, type `:72-97`).
  Ordinary admission freezes exactly one runtime cell (`:198-206`, `:387-392`).
  Production freeze path is `buildWorkloadDecisionForSubmit`
  (`:432-439`; caller `control/api.go:1102-1105`). Persisted as
  `jobs.workload_decision` + digest (insert `control/store_jobs.go:499-519`; load
  validates digest `:671-702`).
- **`JobManifest`** — thin agent wire DTO at dispatch (`control/types.go:147-156`;
  built `control/api.go:3247-3256`); inputs travel via presigned URL, not the
  manifest.

**Graph policy does not survive compile → receipt as one identity.** Quote path
allocates a fresh `cliJobSubmit` per IR step and reclassifies independently
(`control/project_quote.go:67-91`). Independent submit refuses any step with
dependencies (`control/project_submit.go:74-76`) and creates one ordinary job per
step (`:177-216`). Dependent graphs submit roots first
(`control/project_dependency.go:129-134`), then materialize
(`control/project_materialize.go:26-41`) and re-quote/submit later steps — still
one job each time. Jobs link project only via `project_order_id` +
`project_step_id` (`control/schema.sql:6887-6896`), not the full graph.
`ClearingReceipt` exposes per-job `WorkloadDecision` digests
(`control/receipt.go:5-26`), not `ir_sha256`.

**"`WorkloadDecision` is the accepted per-step projection of the graph" is
FALSE.** It is an independently classified single-job authority
(`buildWorkloadDecisionFromBindingDirected` path), not a lossless projection of
IR dependencies, result contract, checkpoint/egress policy, or project economic
context. Dependencies, result contracts, and graph-level economics do **not**
appear on the frozen decision. Two identities run in parallel: `ir_sha256` on
order/compile, `workload_decision_sha256` on the job.

**Highest-value duplication traps:**

1. A second "canonical Workload" writer beside `buildWorkloadDecisionForSubmit` /
   `jobs.workload_decision`.
2. Rebuilding project compile/quote/order/materialize — they already exist as
   proposal, ceiling, and hand-off authorities.
3. Thickening `JobManifest` to re-encode graph policy the agent path intentionally
   does not carry.

**What is genuinely absent:** a single accepted graph object (post-buyer approval)
that quote, job, placement, verification, settlement, and receipt all cite;
lossless projection of IR graph fields into each step's frozen decision; server-side
execution of dependent DAGs as one accepted unit; graph digests inside
decision-chain / envelope objects (envelope itself ABSENT — Step 14).

**Step 16 work, restated:** keep `ProjectWorkloadIR` as proposal and compile
receipts as durable IR evidence. On buyer approval, freeze one immutable graph
digest (`ir_sha256` plus any acceptance stamp) that every subsequent project job,
quote, order step, materialization, and job receipt must cite. Make each step's
accepted `WorkloadDecision` a **digest-linked projection** of that graph — not a
re-run of catalogue classification that forgets deps/result/economic context —
while ordinary non-project jobs keep today's single-job freeze path. If a
projection is lossy relative to the approved IR, refuse that project class rather
than ship a second Workload writer.

**Compile/quote latency budgets remain UNMEASURED** as new programme gates unless
measured.

**Ledger PARTIAL is correct:** substantial compiler + decision machinery exists;
canonical graph-through-receipt does not.

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

### Step 18 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `0deb8b6b`.

**"A linear full-fleet scan cannot remain the large-scale hot path" is overstated
for two of three lanes and misstates the third.** Treating Step 18 as replacing a
universal full-fleet worker scan would reimplement filters that already exist
inside production SQL and miss the costs that still scale.

**What production selection actually does:**

- **Realtime** — profile-scoped offer book, not every worker.
  `AuthorizeRealtimeContract` reserves via
  `realtimeAuthorizeSelectOfferSQL*` with hard filters on
  `runtime_profile_id` + `runtime_profile_sha256`, active status, capacity,
  freshness, supplier quarantine, and profile rate ceilings
  (`control/realtime_supplier_outcome_stats.go:120-125`; call site
  `control/realtime_store.go:999-1044`). Ranking is verified-outcome cost then
  warmth/capacity/recency (`:89-114`). Multi-offer uses `SKIP LOCKED` by rank;
  single-offer blocks (`realtime_store.go:1002-1007`). Deliberately allows
  non-rank-1 under SKIP contention (comments `:32-36`).
- **Service lease** — profile + **region** scoped offers, ordered by total
  supplier+residency nanos, all candidates locked `FOR UPDATE`, first ask that
  clears `PricingDecision` wins (`control/service_leases.go:681-691+`). Not a
  full-fleet worker scan.
- **Batch** — pull claim, not push select of a worker for a job.
  `ClaimTasksTx` (`control/scheduler.go:1140+`) locks the claiming worker, then
  finds a task (`FOR UPDATE OF t SKIP LOCKED LIMIT 1` at `:1104-1105`). Hard
  filters (currency, frozen runtime cell vs `worker_authorized_capabilities`,
  memory, hw_classes, residency, reputation, rate, placement v3, containment,
  ready task) live inside one large CTE (`:842+`). **Fleet-relative work remains:**
  for each eligible job, `cheaper_class_online` / `cheaper_ask_online` run
  `EXISTS` over live workers (`:558+`). Comments record the former
  O(queue × fleet) shape reduced to once per candidate job via `MATERIALIZED`
  (`:516-531`). That is still live-fleet work per eligible job, not "visit every
  worker to pick supply for one request," and it is the batch cost Step 18 must
  actually target. Warmth is ORDER BY preference only, never a hard filter
  (`:756-760`). Side path `Match` + `CandidateWorkers` (`:97-146`) is peer /
  redundancy ranking in Go — **not** the poll hot path; do not treat it as the
  production selector.

**Postgres indexes and capability projections exist** (e.g.
`worker_authorized_capabilities`, ready-task partial indexes). **Absent as the
product this step names:** versioned hierarchical candidate indexes, coherent
network epoch snapshots shared by production and twin, and a multi-stage shortlist
pipeline with published stage cardinalities. **`failure_domain` is ABSENT** in
control Go and schema (repo search under `control/` returns no symbol). Region
appears as a hard filter on service lease; batch uses residency lists; realtime
offer SQL cited above has no region filter.

**Bible-style ladder stages that already exist are implicit inside SQL** (hard
contract, trust/sandbox pieces on batch, runtime capability projection, economic
rank). Separate expensive scoring stage is ABSENT; preferences are ORDER BY terms,
not first-class stage metrics. Reservation is against **live rows under locks**,
not against an epoch snapshot.

**Duplication traps:** reimplementing lane SQL filters in a Go index that claims
to admit while `ClaimTasksTx` / authorize offer SQL / `CreateServiceLease` remain
the writers of capacity; building a second scorer while those paths still decide;
treating `Match` as production selection.

**Step 18 work, restated:** define versioned capability / market / locality
**epochs** derived from durable tables. Production selection remains SQL
reservation against durable counters **until** an epoch-backed shortlist proves
parity and fail-closed behaviour. Target the real costs: (a) batch
`cheaper_class_online` / `cheaper_ask_online` EXISTS over live workers per eligible
job; (b) any future push-select that would unscoped-scan workers. Do not replace
DB capacity rows and SKIP LOCKED / FOR UPDATE reservation with index-only admit.
False positives/negatives fail closed to current SQL. Twin (when built) must
consume the same epoch reader — Step 20's note already forbids a Go stand-in
selector.

**Index update/snapshot latency and stage-cardinality production gates remain
UNMEASURED** until published.

**Completion, restated:** hot-path claim/authorize reports stage cardinalities;
large fleets do not re-evaluate full live worker tables per job for preference
signals without an epoch index; RT/service profile scoping is preserved, not
"fixed" as if it were a full-fleet scan. Ledger ABSENT remains correct for the
hierarchical index **product**; do not claim partial solely because SQL already
narrows.

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

### Step 20 shape note — amended 2026-08-09 after reconnaissance

**The production selector is SQL inside PostgreSQL, and that single fact
invalidates the way this step was written.**

Batch selection is `ClaimTasksTx` — one large CTE with fleet-relative `EXISTS`
subqueries per eligible job. Realtime selection is the offer-claim CTE. Service
lease selection is the ordered `FOR UPDATE` book walk. There is **no** extracted
pure function `Select(epoch, request) -> decision` that a twin and production
could both call. Any Go-side ranker that exists is a component, not the selector.

Three consequences, and none of them are optional:

**1. The scale targets are currently unmeasurable as qualified claims.** 1k ≤1 ms,
10k ≤3 ms, 100k ≤10 ms, 1M ≤25 ms cannot be honestly reported until the
measurement runs the SQL production runs. Benchmarking a Go matcher and labelling
the result "candidate selection p95" would be the anti-gaming law against treating
simulation as physical proof, committed in the most quotable place in the
programme. The targets stay as targets and are marked UNMEASURED until a harness
exists.

**2. "1M workers" is not one number, because the three lanes do not scale on the
same variable.** Batch claim cost grows with eligible jobs/tasks *and* with the
fleet-relative `EXISTS` shape per eligible job, so total fleet size does bite.
Realtime and service-lease cost grows with the **offer book for one runtime
profile and model sha**, not with total fleet size — a parallel census confirmed
their scans are already profile-scoped. So a million workers that publish no
offers on the profile under test do not stress those lanes at all, and a curve
that reports one number across all three lanes is measuring different things and
averaging them. Each lane states its own scaling variable, or the curve is
meaningless.

**3. The harness must execute production's SQL, not a model of it.** The
acceptable shapes are (a) a governed harness that materialises deterministic
synthetic fleets in PostgreSQL and calls the same entry points production calls —
`ClaimTasksTx`, the realtime authorize path, `CreateServiceLease` — with fixed
build, config, seed and a locked planner shape; or (b) extraction of a decision
core that production genuinely calls, after which the twin and production share
it by construction. Option (b) is a larger change than Step 20 and must not be
smuggled in as a measurement task.

Under either shape the curve records the SQL text digest and the planner shape it
ran against, because a curve measured on a different plan than production uses is
not evidence about production.

**Step 17's "Digital Twin on production decision functions" inherits all of this.**
The twin is not blocked on being written; it is blocked on being written against
the real entry points. A twin built on a Go stand-in would produce numbers that
look like the Bible's targets and mean nothing, which is worse than having no
twin.

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

### Step 21 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `ed1549ca`.

**Faithful new-policy replay of network decisions requires state that was never
captured. The step's first obligation is capture (or honest refusal of Mode B),
not a replay engine over missing inputs.**

What a faithful counterfactual would need and **does not have**:

| Missing substrate | Why it blocks faithful replay |
|---|---|
| Full live fleet / claim eligibility at batch claim time | Batch is pull + `SKIP LOCKED`; worker choice is claim-time SQL (`ClaimTasksTx`). No snapshot freezes who was online, asks, cheaper-class deferral, warmth depth, etc. at claim. |
| Full realtime/service **offer book** at clear | `RealtimeMarketClearingReceipt` freezes selected rank signals + `candidate_count` and selected-offer ranking inputs (`control/realtime_store.go:249-279`, `control/realtime_clearing.go:56-74`) — **not** every candidate's full ranking vector. "Would rank-2 have won under new policy?" cannot be re-derived from the receipt alone. |
| Coherent network epochs / candidate index | Steps 18–20 product surfaces ABSENT; no epoch identity to pin snapshots to. |
| Accepted atomic Market→Runtime→Placement→Topology chain | Step 11 chain root ABSENT; there is no single "selected chain" digest. |
| Matched incumbent/challenger **execution** pair + shared input/cohort digest | Required for causal outcome counterfactuals and for any promotion gate v4 would accept. |
| Common EvidenceEnvelope root | Step 14 ABSENT (ledger / recon). |
| Region-health / "failed region healthy" authority | **ABSENT** in control Go (no target to snapshot). |
| Per-phase duration decomposition | Queue/startup/transfer actuals still absent. |

Reconciliation already records this ceiling:
`historical_network_replay: ABSENT_BATCH_CELL_REGRET_ONLY`
(`evidence/state/network-v2-reconciliation.json`).

**What already exists (extend; do not rebuild as a second history authority):**

- **`runtime_shadow_selections`** — immutable batch cell-plane shadow
  (`control/schema.sql:6300+`; rewrite refused by
  `cx_refuse_shadow_selection_rewrite` at `:6464-6474`). Writer
  `Store.RecordShadowSelection` (`control/runtime_shadow_selection.go:532`);
  policy `eligibility-and-measured-supplier-liability-v3` (`:54`). Caller is
  **post-commit** on submit: errors logged and dropped; shadow **cannot veto**
  admission (`control/api.go:1676-1710` — "a selector that could refuse a submit
  would be a router, and this one is not allowed to route"). Captures considered/
  excluded cells, routed vs shadow cell, basis, topology plan, execution mode.
  Ordinary admission freezes a **singleton** cell; multi-candidate scoring is
  over the directed set, shadow-only — regret on ordinary traffic is often
  identically zero by construction for routed vs considered singleton.
- **`SelectorLiabilityRegret` / `SelectorLiabilityRegretForScope`** — query over
  shadow rows + windowed measured supplier-liability proxies
  (`control/runtime_cell_cost.go:1097-1154+`); admin GET at `:1282-1332`. **Not**
  a second table; **not** total-cost regret; platform components named unknown.
- **Duration and plan predicted-vs-actual** — `eta_calibration`
  (`control/schema.sql:1714+`; writers e.g. `control/store.go:1733`);
  `plan_actuals` (`schema.sql:5133+`; `control/plan_actuals.go`). Duration is
  **owned by `eta_calibration`**; recording it again on plan_actuals is refused
  because it would create two learners over one quantity
  (`control/plan_actuals.go:14-38`). `plan_actuals` is observation-only — money/
  admission paths do not read it (`:22-25`).
- **Lane accepted freezes** — `jobs.workload_decision` / `pricing_decision` (and
  digests); realtime/service `market_clearing` + pricing/placement freezes.
  Historical performance/catalogue snapshot validation on accepted jobs is
  "replay accepted freeze," **not** re-rank under a new policy binary.
- **`runtime_cell_promotion_evaluations`** — append-only evaluation receipts.
  Gate `cell-promotion-gate-v4` always hits
  `promotionMatchedPairAuthorityRefusal`
  (`control/runtime_cell_promotion.go:41`, `:78`, refuse at `:321`).

**Promotion as Step 21 completion is struck — same class as Step 8.** "Every
selector promotion requires historical replay…" is **unreachable** while gate v4
refuses every promotion for missing matched-pair authority, and while
narrow-scope evidence cannot authorize global lifecycle (Step 8 note). Promotion
coverage remains a separate obligation. Instant rollback of a never-promoted
policy is not a completion criterion.

**Duplication traps:** a new "decision snapshot corpus" that re-stores
predictions, cell consideration, duration actuals, or liability totals as
canonical writers would fork authority already owned by shadow / eta_calibration /
plan_actuals / SelectorLiabilityRegret / lane receipts.

**Replay modes, stated honestly:**

- **Mode A (available now):** re-score stored cell consideration sets under named
  `selection_policy` / matrix / `policy_revision`; report liability and duration
  regret from existing queries; preserve immutability; handle missing-actual the
  way SelectorLiabilityRegret already scores unrelated/unmeasured counts.
- **Mode B (blocked without new capture):** new-policy worker or full order-book
  ranking. **Refuse** with the named missing fields above. Do not invent fleet/
  book/epoch facts. If product needs Mode B, the **first** serial work is
  append-only claim-time eligibility snapshots (batch) and full offer-book freezes
  (push markets) — capture before replay.

**Completion, restated:** Corpus immutability preserved; Mode A reports from
existing authorities without a second history writer; Mode B refuses rather than
fabricating; **promotion is not a completion criterion of this step**. Status:
PARTIAL — batch cell regret exists; network snapshot replay does not.

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

### Step 22 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `ed1549ca`.

**Most of the Bible's network fault list has no mutation target, because the
authority it would attack does not exist yet.** A catalogue claiming 100% of that
list would be claiming coverage of absent authorities. Ledger `ABSENT` — "no
manifest for decision faults" — is true for a dedicated **network-fault
catalogue**; it is **false** if read as "no mutants touch anything
network-adjacent."

**Mutation infrastructure today:** `scripts/mutation-manifest.json` holds **104**
mutants (ids 1–104). Runner is `scripts/mutation-test.sh` + contracts/preflight.
Domains covered are production-adjacent money, pricing, realtime settlement,
currency, admission, economics, true-net, prefix locality (1), runtime admission
(1), runtime selection (3), etc. — **not** a network-decision fault catalogue.

**Live adjacent mutants (do not re-count as completing the Bible list):**

| Id | Domain | Target | What it hits |
|---:|---|---|---|
| 41 | `prefix_locality` | `control/prefix_routing.go` | prefix warmth ignores TTL |
| 49 | `runtime_admission` | `control/scheduler.go` | claim ignores frozen runtime candidates (**only** scheduler mutant — not cheapest-worker ORDER BY) |
| 70–72 | `runtime_selection` | `control/runtime_shadow_selection.go` | shadow cost-tie / manufactured winner / latency noise floor honesty |
| 46 | `quote_admission` | `control/quote.go` | quote supply ignores buyer data residency (claim-time residency has **no** dedicated mutant) |
| 20, 23, 94–99, 103–104 | currency / pricing / reuse | various | change-currency family already dense |

**No mutant today on:** market-clearing ranker (`control/realtime_clearing.go` —
grep of manifest has no `realtime_clearing` / `topology_planner` targets),
topology planner, batch cheapest-class / warmth-over-cost claim ORDER BY
economics, network epochs, candidate index, Digital Twin, EvidenceEnvelope,
region-health, promotion gate as a network-fault class.

**Bible network faults — register-now vs defer (authorities named):**

| Fault | Status | Target / wait |
|---|---|---|
| Drop cheapest eligible worker | **Could exist — no mutant** | Live claim SQL cheapest-sufficient-class / ask rank (`control/scheduler.go` ORDER BY at `:1093+`). |
| Forge warm-cache state | **Partial** (id 41 TTL only) | Not every forge path (self-declared realtime warmth, host cache lies). |
| Reverse supplier ranking | **Could exist — no mutant** | Live `realtimeOfferBeats` / verified-outcome cost (`control/realtime_clearing.go`). |
| Erase region restriction | **Partial** (id 46 quote) | Claim-time `data_country` / residency filters lack a dedicated mutant. |
| Inflate throughput / stale benchmark as current | **Related economics mutants, not exact product-ranking mutant** | Stale-benchmark refusal exists as tests; no manifest mutant that makes stale benchmark current authority. |
| Remove deadline | **Could exist — no mutant** | Deadlines exist on jobs/realtime paths; no "remove deadline" mutant. |
| Duplicate supplier identity | **No exact mutant** | Money paths refuse duplicate payables; enrollment identity forge is a different surface. |
| Hide provider failure / drop reliability penalty | **Could exist — no mutant** | Verified-outcome cost multipliers and liability path exist; no dedicated "zero failure rate in ranking" mutant. |
| Change currency | **Mutants exist** | Dense currency_authority / pricing / realtime mismatch coverage — do **not** re-badge as new network coverage without new decision wiring. |
| Make failed region healthy | **No target — ABSENT authority** | Control has no region-health / failed-region surface. Inventing a mutant invents an authority. |
| Coherent-epoch corruption, twin-only faults, full decision-chain envelope truncation, candidate-index poisoning | **No target** | Wait on Steps 11 / 14 / 17–20 (and Step 18 epoch product) before registration. |

**Fault registration rule for this step:**

1. **Register now** only faults with a live `source_target` and a red invariant
   under the mutant for the **intended reason** (cheapest-worker claim discipline,
   reverse clearing rank, hide failure / drop reliability, forge warmth beyond
   existing TTL coverage, claim-time region erase, stale benchmark-as-current,
   remove deadline on paths that enforce it).
2. **Defer with named ABSENT target** every fault whose authority does not exist
   yet — list the authority each waits on; do not stub targets.
3. **Do not** mark Step 22 complete because currency/money mutants exist.
4. **Do not optimize the mutation runner.** Its performance target is already
   exceeded; re-optimizing wall-time is **out of scope**. Completion is coverage
   of registered network faults with zero false catches / infrastructure scored
   as caught — the oracle bar already built — not runner speed.

**"100% registered network faults" completion, restated:** 100% of faults
**registered against live targets** are caught for the intended reason; the
catalogue **explicitly lists deferred faults with no target** rather than
claiming 100% of the Bible list. Status: ABSENT as network-fault catalogue;
PARTIAL adjacent mutants exist.

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

### Step 25 shape note — amended 2026-08-09 after reconnaissance

Every claim below was re-verified against `codex/network-v2` at `ed1549ca`.

**Step 25 reads as "add more caches." Three core mechanisms are already
production; two named expansions are closed non-applicable; most asset classes
are ABSENT. Scaffolding empty cache classes would invent authority.**

Distinguish sharply **MEASURED** vs merely **WIRED** vs **ABSENT** /
**DOES_NOT_APPLY**. Never invent a latency number; unmeasured is UNMEASURED.

**Already production (maintain only — do not reimplement as a "work-elimination
framework"):**

| Class | Authority | What is proven |
|---|---|---|
| Exact-result reuse | `control/exact_reuse.go` (`RequestIdentity` incl. adapter string at `:54-62`); realtime tries cache before schedule (`control/realtime.go:925-955`); `SettleRealtimeExactReuse` (`control/realtime_store.go:1393-1396`) with `ClassExactResultReuse` (`control/billing_classes.go:25`), no supplier credit, PricingDecision authority | **MEASURED** on money/receipt path (public-path reuse proofs under `evidence/reuse/`). Control-plane money and isolation — not a multi-tenant GPU throughput proof. |
| In-flight coalescing | `control/inflight_coalescing.go` (`ClaimInflightExecution` at `:73-80`); wired after exact-cache miss (`control/realtime.go` coalesce path); followers settle `ClassCoalescedDelivery` (`billing_classes.go:31`) | **MEASURED** leader election, money, isolation; 128→1 is money-path + double-upstream proof, not fleet GPU multi-tenant physics. Streaming excluded (SSE). Cross-tenant never merges. |
| Prefix / KV | Routing preference at control plane (`warm_prefix_depth` in `ClaimTasksTx`); engine physical savings in bound evidence | Work elimination at the **engine**; routing at control plane. Does **not** skip scheduling a supplier for batch; does **not** write `prefix_reused_input` on settlement today. Physical cold/warm **MEASURED** on Metal evidence; belief hit rate measured on claim path. |
| Tool/schema identity cache | `control/realtime_identity_cache.go` | **WIRED_MEASURED** (hit ~0.4 µs / miss ~12 µs on host microbench per `docs/RUNTIME_AND_PERF.md` and five-cache audit). Avoids re-canonicalising identity only — not model work. |

**Closed non-applicable (bound audit — do not build empty caches):**

- Control-plane **tokenization** cache — **DOES_NOT_APPLY** (`docs/RUNTIME_AND_PERF.md:798`;
  `evidence/perf/five-cache-architecture-audit.json` status `DOES_NOT_APPLY`).
  No model tokenizer on the control plane; `estimateTokens` is a byte heuristic.
- **Image/audio preprocessing** caches — **DOES_NOT_APPLY**
  (`docs/RUNTIME_AND_PERF.md:799`; same audit). Image gen 503; media rendering is
  closed-scene byte-exact work; exact full-result reuse is the correct shape
  where applicable, not a preprocess tier.

**Wired but UNMEASURED as a cold-vs-warm load receipt:**

- **Model-weight residency** — agent disk pin + `ModelPool` (`agent/src/models.rs`,
  `agent/src/pool.rs`); control plane sees heartbeat `worker_model_state`
  (`control/store_workers.go:515-554`; claim `warm_for_task` 60s). Five-cache
  audit status **`WIRED_UNMEASURED`**: substrate exists; bound saving =
  cold_path_ms − warm_path_ms for the same governed model id is **not** a sealed
  receipt. Do not fork ModelPool; measure or leave UNMEASURED.

**Mostly ABSENT as work-elimination classes (not near-term under this step):**

- **Adapters** — string on `RequestIdentity` only (`exact_reuse.go:62`); no
  residency/routing; LoRA evaluation **non-executable** for adapter deployment
  (`control/lora_evaluation_receipts.go:19`).
- **Datasets** — project pin digests at compile; no locality-aware placement or
  transfer-elimination authority.
- **Render assets** — assembly receipt **refuses** asset locality
  (`control/render_assembly_receipts.go:70`, `:186`).
- **Container layers** — no control-plane cache authority.
- **Compiled kernels** — engine-internal only; no Merc authority.
- **Preprocessed inputs** — absent as a class (full-result reuse is the intended
  shape where identity permits).

The step must **not** present these absent classes as near-term deliverables.
They wait on real substrate (often Step 26 workload prerequisites), not on a
generic cache framework.

**Receipt gap that is real (optional next, not a second elimination surface):**
`prefix_reused_input` is billing **vocabulary** (`control/billing_classes.go:23`)
with `RecordTokenAccounting` (`:126-127`) and **no non-test production caller**.
Ordinary batch jobs do not settle prefix savings through that class. Wiring
observation → token class (or an explicit "unattributed engine hit") is honest
attribution work; inventing a parallel savings ledger would duplicate
token-accounting authority.

**Duplication hazards:** reimplementing exact reuse settlement, inflight
coalescing, billing token classes, realtime identity cache, or prefix belief as a
"Step 25 work-elimination framework" creates parallel money or identity
authorities — the programme's most expensive defect class.

**Completion, restated:** Every **enabled** class has one production caller and
one receipt path (already true for exact reuse and coalescing; prefix routing
done, money attribution optional). Every **inapplicable** class has a bound
refusal (`DOES_NOT_APPLY`), not a stub table. Every **absent** class is named
absent until substrate exists — not scheduled as expansion theatre. Status:
maintain wired elimination; close receipt gaps where engine already reports
savings; refuse empty cache classes.

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

Correction recorded 2026-08-09, during the Step 5 baseline measurement:
The proofs above are exactly what was run, and they hold. They are not a
whole-suite green, and this audit originally read as though they were. Measured
at 53c804f1, the Step 4 boundary commit, with a disposable PostgreSQL and object
store: `go test ./...` over the complete package reports 70 failures in 452.9 s.
Every one of them is a stale test premise, not a production bypass: making the
checked-in benchmark authority honestly non-routable was the substance of Step
4, and dozens of fixtures still assumed it routed. Eight such fixtures were
reconciled before this audit was written; the remaining group was not, and
saying so is the difference between a proof and a claim. The mutation preflight
selector, which is what the 287 s campaign binds, is a deliberate subset and was
green. Closing the remaining fixtures is tracked inside Step 5 so that one
boundary owns one whole-suite verdict.

Verdict: RECONCILED_COMPLETE for the runtime-cell economics authority named
above, on the proofs named above. NOT a whole-suite verdict; see the correction.
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
