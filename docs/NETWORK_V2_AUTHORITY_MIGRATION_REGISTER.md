# Network V2 canonical authority migration register

Baseline: `ae443f69b0a63835a356eb1ed01999325677ad1b`.

This is the deletion register for the eleven V2 concepts. It is not permission
to add eleven wrapper structs. Each row identifies the current mutable authority,
the one canonical target, the compatibility edge, the data copied or lost, and
the condition under which the old representation leaves production authority.

## Register rules

1. A request DTO, an accepted decision, an execution command, an outcome, and a
   receipt may be distinct lifecycle objects. Two mutable objects deciding the
   same fact may not be.
2. Canonical accepted objects are immutable, versioned, digest-bound, and
   validated from their own snapshot. They do not consult later catalogues,
   benchmarks, offers, or policies when replayed.
3. Compatibility conversion happens at one named edge. It may allocate once at
   ingress/egress; it may not allocate or copy repeatedly along the hot path.
4. A legacy row accepted before a canonical object existed stays immutable
   historical evidence. It is never backfilled from current policy.
5. A database index/projection is derived state, not a second authority. The
   canonical digest/epoch from which it was derived must be present.
6. Shadow policy is observation. It cannot reserve capacity, authorize money,
   or overwrite the accepted decision.
7. Removing an old name is not sufficient: its production callers, database
   writers, policy fields, and receipt assembly must move to the canonical
   owner before deletion is credited.

## Summary

| V2 concept | Entry state | Canonical target | Legacy production authority to retire | Deletion milestone |
|---|---|---|---|---|
| Workload | parallel vocabularies | versioned `Workload` graph | `ProjectWorkloadIR` as proposal plus `WorkloadDecision` as separately derived authority | Step 16 |
| Capability | split facts/projections | versioned node `Capability` snapshot with runtime-cell references | `WorkerCapability` as mutable semantic authority and `generatedRuntimeCapability` projections | Step 6 |
| PricingDecision | canonical | existing `PricingDecision` | alternate `EconomicPlan`/float derivations as authority | Step 5 audit; historical edges retained |
| MarketDecision | absent | new immutable `MarketDecision` | realtime and lease clearing receipt shapes; implicit batch book | Steps 7 and 11 |
| PlacementDecision | name exists with wrong scope | expanded canonical `PlacementDecision` | mode-only `PlacementDecision`, `PlacementRequirement`, `RealtimePlacementPlan`, implicit claim SQL | Steps 9 and 11 |
| RuntimeDecision | absent | new immutable `RuntimeDecision` | first `WorkloadRuntimeCandidate` plus post-commit `ShadowSelection` as explanation | Steps 8 and 11 |
| TopologyDecision | planner exists under another name | promoted `TopologyDecision` from `TopologyPlan` | compiler topology, embedded realtime topology, private TP plan as competing decisions | Steps 10 and 11 |
| VerificationContract | absent | new immutable `VerificationContract` | composite strategy string and disconnected policy/class/comparator authority | Step 12 |
| SettlementPlan | absent | new immutable `SettlementPlan` | accepted amounts plus later lane-specific settlement plans | Step 13 |
| EvidenceEnvelope | absent | new immutable `EvidenceEnvelope` chain root | lane-specific receipt roots assembled from current rows | Step 14 |
| ServiceLease | substantive canonical aggregate | existing `ServiceLease`, extended | no alternate aggregate; request/offer/assignment/outcome DTOs remain | Step 28 |

## 1. Workload

### Current representations and callers

- `ProjectWorkloadIR` in `control/project_compiler.go` is the versioned graph
  proposal produced by the compiler.
- `WorkloadDecision` in `control/workload_classification.go` is the executable
  single-job acceptance object. Production admission calls
  `buildWorkloadDecisionForSubmit` from `control/api.go`.
- `JobManifest` in `control/types.go` is the agent dispatch wire object.
- `quoteCompiledProject` in `control/project_quote.go` allocates a new
  `cliJobSubmit` per IR step and asks the legacy quote API to classify it again.
- Project order/materialization tables retain graph identity and dependencies,
  but the accepted per-job WorkloadDecision does not retain the complete graph
  contract.

### Canonical decision

`Workload` becomes the sole accepted, versioned graph. It owns step identity,
dependencies, inputs/outputs, artifacts/models, runtime/resource requirements,
parallelism/checkpoints, privacy/region/egress, verification, deadline/SLO,
result contract and economic constraints.

`ProjectWorkloadIR` is renamed/recast as a proposal DTO until buyer approval.
Approval creates the canonical Workload. `WorkloadDecision` becomes a lossless,
digest-linked per-step execution projection, not a separately classified
authority. `JobManifest` remains a wire DTO generated once at dispatch.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| project declaration → `ProjectWorkloadIR` | graph allocation at compiler ingress | none expected; proposal only | retain as proposal builder, then rename clearly |
| IR step → `cliJobSubmit` → `WorkloadDecision` | allocates/copies once per step and reclassifies | graph dependency, topology intent, result/economic context is not carried as one object | replace with canonical Workload acceptance and direct step projection |
| `WorkloadDecision` → `JobManifest` | value/JSON copy at dispatch | graph context is reduced to job/task fields | keep one wire conversion containing Workload and step digests |

### Removal sequence and proof

1. Add canonical Workload validation/digest and lossless legacy import tests.
2. Make compiler approval create it once.
3. Bind quote, accepted decisions, project order, tasks, results and envelope to
   the same Workload digest.
4. Stop calling the legacy quote/classification loop for canonical projects.
5. Delete project-only duplicate policy fields after every caller projects from
   Workload.

Completion requires dependency/artifact/topology/result/economic policy to
survive compile → quote → accept → dispatch → verify → settle → receipt without
reconstruction. Roll back if any old project remains unreadable or a projection
loses policy.

## 2. Capability

### Current representations and callers

- Static runtime catalogue facts live in `authorityCell` and
  `authorityRuntimeProfile` in `control/runtime_authority.go`.
- `WorkerCapability` in `control/types.go` and `agent/src/types.rs` is the
  registration wire shape.
- `Store.UpsertWorker` in `control/store_workers.go` flattens registration into
  worker and `worker_authorized_capabilities` rows.
- `generatedRuntimeCapability` in `control/runtime_authority.go` combines
  catalogue and activation projections for admission.
- `advertisedRuntimeCapabilities` and `directedRuntimeCapabilities` in
  `control/activation_policy.go` correctly keep activation policy distinct.
- Realtime/service offers, prefix state, fabric observations, outcome stats and
  agent controls hold additional capability facts outside the registration
  object.

### Canonical decision

`Capability` becomes the immutable, versioned node snapshot for a coherent
epoch. It embeds references to immutable runtime-cell catalogue identities and
owns node hardware/accelerators, memory/storage/network, region/failure domain,
runtime versions, model/artifact/prefix/cache residency with freshness,
availability/limits, thermal/power, benchmark/trust/reliability and interruption
facts. Activation policy remains a separate versioned eligibility overlay.

`WorkerCapability` becomes an ingress DTO only. Runtime catalogue objects remain
immutable catalogues referenced by digest; they are not copied into a competing
node authority. Database rows and indexes are projections carrying the
Capability digest and worker-state epoch.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| agent `WorkerCapability` → store rows | JSON decode plus several DB projections | region/failure domain, network/storage, residency, interruption and governed power/trust facts are absent or elsewhere | one validated conversion into Capability snapshot, then derive rows/indexes |
| runtime catalogue + activation → `generatedRuntimeCapability` | generated slice copy on policy read | node-specific state cannot be expressed | replace selector input with Capability cell references plus activation overlay |
| offers/prefix/fabric/outcomes → selection SQL | repeated joins/copies per decision | no coherent epoch across sources | publish one coherent Capability/market/locality snapshot; retain durable sources |

### Removal sequence and proof

1. Add canonical field bounds, digest and wire compatibility.
2. Persist snapshot identity/epoch; derive indexes incrementally.
3. Move admission/market/placement to one Capability vocabulary.
4. Rename `WorkerCapability` to an explicit registration DTO after clients
   migrate; delete `generatedRuntimeCapability` as selector authority.

Completion requires lossless registration compatibility, fact/policy separation,
stale-epoch refusal and bounded update/allocation metrics. Roll back on changed
routability, stale hard-contract acceptance or lost agent compatibility.

## 3. PricingDecision

### Current representations and callers

- `PricingDecision` in `control/pricing_decision.go` is immutable, fixed-point
  and digest-bound.
- Distributed, exact reuse, realtime, realtime reuse and ServiceLease builders
  all produce it. Quote/job/contract/lease persistence validates exact digests.
- `EconomicPlan`, float quote fields, offered rates and legacy settlement rows
  coexist as historical or compatibility projections.

### Canonical decision

The existing `PricingDecision` remains canonical. It is extended only with
digests of canonical MarketDecision, RuntimeDecision, PlacementDecision,
TopologyDecision, VerificationContract and SettlementPlan when those concepts
exist. Accepted historical PricingDecisions remain immutable and validate under
their original schema version.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| economic scenario → PricingDecision | value construction/JSON once at acceptance | older lanes omit canonical decision identities and some costs are explicitly unknown | extend schema versions; never encode unknown as zero |
| PricingDecision → `EconomicPlan`/float columns | value projection | nanos may be rounded for display/legacy queries | compatibility read only; money writers use fixed point |
| PricingDecision → settlement | snapshot validation/read | lifecycle rules live elsewhere | SettlementPlan binds lifecycle while replaying exact PricingDecision amounts |

### Removal sequence and proof

Alternative buyer-price derivations stay deleted. New money writers must accept
PricingDecision/SettlementPlan, not `EconomicPlan` or floats. Legacy projections
are removed from production writers after all historical-read tests pass.
Completion remains exact currency/ceiling/floor/conservation/true-net status
across every lane. Any historical repricing or unknown-as-zero cost is rollback.

## 4. MarketDecision

### Current representations and callers

- `RealtimeMarketClearingReceipt` in `control/realtime_store.go` is built during
  atomic offer authorization.
- `serviceLeaseMarketClearingDetail` in
  `control/service_market_liquidity.go` is built during lease creation.
- Realtime, service and aggregate network liquidity receipts are observations,
  not complete accepted order books.
- Batch selection has no frozen MarketDecision; worker supply is implicit in
  scheduler/claim queries.

### Canonical decision

`MarketDecision` is one immutable accepted object containing request/buyer order,
coherent market epoch, eligible offer identities, exclusions with reasons,
provider observations, market depth, selected offer/economic plan, clearing
policy/tie-break, confidence and digest.

Liquidity reports remain observations. Realtime and service clearing receipts
become API projections from MarketDecision. Batch creates a MarketDecision
before/at its first capacity reservation rather than reconstructing the book
from later rows.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| live offer query → realtime receipt | SQL row/JSON copy | complete eligible/excluded set, provider observations and confidence missing | canonical builder consumes indexed snapshot; receipt projects from it |
| lease offer query → private detail | selected row plus counts | exclusions and full economic alternatives missing | canonical builder and lossless legacy JSON projection |
| batch scheduler query → claim | implicit DB result only | no accepted market identity or replay book | freeze MarketDecision candidate identities/epoch, final reservation validates it |

### Removal sequence and proof

1. Build pure canonical validator/digest and legacy projection tests.
2. Wire realtime and lease transactions.
3. Wire batch market snapshot and final reservation.
4. Remove lane-specific builders; retain their JSON/API shapes as projections
   until clients migrate.

Completion requires deterministic price/currency/ceiling/floor clearing,
duplicate-supplier and stale-offer refusal, exact eligible/excluded evidence and
replay. Roll back on changed money or reservation atomicity.

## 5. PlacementDecision

### Current representations and callers

- `PlacementDecision` in `control/execution_mode.go` selects only POOL,
  REPLICA_SERVICE, LOCAL_CLUSTER or CLOUD_BACKSTOP.
- `PlacementRequirement` in `control/quote.go` binds batch eligibility and is
  converted into another scheduler query form.
- `RealtimePlacementPlan` in `control/realtime_placement.go` binds one host and
  tensor-parallel fit.
- Actual batch worker selection is implicit in `control/scheduler.go` claim SQL.
- ServiceLease creation reserves an offer in its own transaction shape.

### Canonical decision

The canonical `PlacementDecision` owns candidate set/epoch and narrowing stages,
hard exclusions, locality, queue/load, predicted latency/cost/reliability,
selected worker(s), execution fabric mode, fallback, regret baseline and digest.

The current mode-only type is renamed `ExecutionModeDecision` before the
canonical name is expanded, avoiding two `PlacementDecision` meanings.
`PlacementRequirement` becomes an input contract. `RealtimePlacementPlan`
becomes the host-plan projection embedded by reference. Claim/reservation SQL
validates the frozen decision rather than deciding silently.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| requirement → scheduler query | allocates/copies supply requirements | considered/excluded candidates and prediction terms vanish | requirement is input; canonical decision retains narrowing trace |
| mode planner → current `PlacementDecision` | small value | no worker/candidate/epoch | rename to ExecutionModeDecision, embed it in canonical placement |
| realtime offer → `RealtimePlacementPlan` | one JSON plan | no alternate candidate/fallback/regret | retain as selected host sub-plan, bind canonical placement digest |
| scheduler SQL → task claim | DB row only | entire decision is implicit | validate selected worker/capacity against frozen decision atomically |

### Removal sequence and proof

Rename the mode type, introduce the complete canonical object, wire realtime/
lease/batch reservations, then delete independent decision fields from
requirements/plans/receipts. Completion requires hard constraints before scores,
coherent epochs, atomic capacity and deterministic fallback. Roll back on stale
locality, reservation races or claim throughput regressions.

## 6. RuntimeDecision

### Current representations and callers

- `WorkloadRuntimeCandidate` records candidate fields inside WorkloadDecision.
- `rankAndFreezeAdmissionCell` in `control/workload_classification.go` freezes
  the first accepted cell using lifecycle/quality ranking.
- `ShadowSelection` in `control/runtime_shadow_selection.go` and
  `GovernedShadowDecision` in `control/runtime_governed_comparison.go` compare
  additional cells and measured economics after admission.
- Activation/promotion/rollback authority lives in `activation_policy.go`,
  `runtime_cell_promotion.go` and selector rollback code.

### Canonical decision

`RuntimeDecision` freezes engine/cell, model/artifact and precision, quality and
verification contract, hardware class, runtime/profile revision, benchmark
authority, activation revision, selection policy/basis, confidence and rollback
target before physical acceptance.

`WorkloadRuntimeCandidate` remains a candidate input only. ShadowSelection
references the accepted RuntimeDecision and a challenger policy; it never becomes
the accepted object. Measured economics moves into the pre-acceptance canonical
selector once sufficient comparable evidence exists.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| runtime catalogue → candidate slice | slice allocation | activation/rollback and measured basis are not one object | index returns references; decision copies winner identity once |
| first candidate → accepted workload | value copy | why it won and alternatives are missing | accepted RuntimeDecision binds MarketDecision candidates/exclusions |
| accepted job → post-commit ShadowSelection | DB read and candidate copies | too late to veto; not authoritative | retain only challenger observation/replay, linked to accepted digest |

### Removal sequence and proof

Create canonical scorer/validator, persist it in the acceptance transaction,
make dispatch/receipts consume it, and remove accepted runtime duplication from
WorkloadDecision/PlacementRequirement/tasks after compatibility reads migrate.
Completion requires quality-first, measured same-hardware economics, honest tie
bases, promotion and instant rollback. Invalid selection is immediate rollback.

## 7. TopologyDecision

### Current representations and callers

- `TopologyPlan` in `control/topology_planner.go` is the closest generic planner.
- `ProjectIRTopology` in `control/project_compiler.go` carries compiler intent.
- `RealtimePlacementPlan` embeds topology/TP fields.
- `tensorParallelPlan` in `control/multi_gpu_admission.go` is a host calculation.
- `PlanTopologyFromFabricEvaluation` consumes measured fabric evidence.
- Batch calls topology only through post-commit
  `ShadowSelection.withExecutionMode`.

### Canonical decision

Promote/rename `TopologyPlan` to `TopologyDecision` and extend it with candidate
device/failure-domain identities, fabric/locality epoch and evidence digest,
selected/refused strategy, physics rationale, scheduler shape, fallback and
decision digest.

`ProjectIRTopology` is renamed `TopologyRequirement` because it describes buyer/
compiler intent. `tensorParallelPlan` remains a private pure calculation used by
the canonical builder. Realtime host plans reference the canonical decision.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| compiler topology → planner request | value copy | accepted fabric/evidence/worker topology not linked | requirement feeds canonical decision |
| fabric evaluation → `TopologyPlan` | value/slice copy | explicitly non-admissible operator evidence today | canonical builder admits only governed fresh evidence |
| topology plan → shadow row | JSON copy after commit | cannot authorize or refuse accepted batch | persist before acceptance; shadow references digest |
| host TP calculation → realtime plan | value copy | separate decision vocabulary | internal calculation, canonical output only |

### Removal sequence and proof

Rename/promote the generic planner, migrate all callers, bind pre-acceptance
decisions, then remove decision-like fields from compiler/realtime projections.
Completion requires WAN tight-coupling refusal, measured local gang admission,
device/failure-domain bounds and deterministic replay. Unmeasured fabric
admission is rollback.

## 8. VerificationContract

### Current representations and callers

- `VerificationPolicy` in `control/types.go` is buyer input.
- Runtime cells carry a free verification string.
- `WorkloadDecision.VerificationStrategy` freezes a composite string.
- Governed classes live in `control/verification_class.go`.
- `VerificationWorkPlan`, `VerificationDecision`, verification attempts and
  apply/effect code decide later lifecycle stages.
- `EmbeddingComparison` is rich, but `resultsAgree` reduces it to `.Passed` for
  ordinary task flow, losing comparator detail from the normal receipt.

### Canonical decision

`VerificationContract` is the accepted immutable authority for verifier class,
evaluator/reference/digest, threshold/comparator revision, sampling, failure
consequence, recompute, confidence and quality contract.

Buyer policy and runtime promise are inputs. VerificationWorkPlan is an execution
plan derived from the contract. VerificationDecision/attempt/artifact are
outcomes. None independently changes the accepted contract.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| buyer/runtime/workload inputs → strategy string | string construction | threshold, revision, reference, sampling/consequence not structurally bound | canonical contract builder |
| contract intent → VerificationWorkPlan | value/JSON | currently reconstructed after acceptance | lossless derivation citing contract digest |
| rich comparator → boolean agreement | drops fields | comparator revision/threshold/reference/result detail absent | retain rich outcome and bind it in EvidenceEnvelope |

### Removal sequence and proof

Build contract at acceptance, move every verifier/settlement caller to its
digest, retain rich outcomes, then delete composite strategy as authority.
Completion requires exact threshold/reference/revision/sampling/consequence
replay and money agreement. Any weakening or evidence loss is rollback.

## 9. SettlementPlan

### Current representations and callers

- PricingDecision fixed point carries accepted amounts.
- Economic scenarios/plans describe some task outcomes.
- VerificationWorkPlan includes later per-attempt ledger effects.
- `RealtimeSettlement`, `ServiceLeaseSettlement`, observed-output settlement,
  ledger entries, payout funding, refund and dispute code each own parts of the
  lifecycle.

### Canonical decision

`SettlementPlan` is accepted alongside PricingDecision and binds buyer charge,
supplier/provider/verifier entitlements, holds/reserves, refund/dispute rules,
payout state-machine revision, true-net status and idempotency identities for
every allowed outcome.

Lane settlement structs become outcomes that replay the accepted plan. Ledger
entries remain immutable accounting facts. PricingDecision remains price/cost
authority and is referenced rather than copied into a competing plan.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| PricingDecision/economic scenario → later ledger plan | repeated construction | lifecycle/retry/refund/payout identity not accepted as one object | construct SettlementPlan once at acceptance |
| plan → lane settlement outcome | value/JSON/DB writes | different lane shapes cannot prove one state machine | validate outcome against canonical plan digest |
| outcome → ledger entries | multiple rows | intended idempotency/consequence is implicit in code | each row cites plan/outcome identity |

### Removal sequence and proof

Add canonical plan/schema, migrate each lane one at a time behind exact money
tests, make ledger/refund/dispute/payout writers require it, then remove duplicate
planned fields. Historical lane rows retain old validators. Completion requires
100% nano reconciliation and no lost/double liability. Any money defect rolls
back immediately.

## 10. EvidenceEnvelope

### Current representations and callers

- Batch exposes `ClearingReceipt` in `control/receipt.go`.
- Realtime exposes `RealtimeReceipt` in `control/realtime_store.go`.
- ServiceLease exposes `ServiceLeaseReceipt` in `control/service_leases.go`.
- Project compile/materialization/evaluation use specialist receipts.
- `ReceiptIdentity` in `control/receipt_identity.go` governs generated evidence
  files, not transaction-chain authority.
- Current receipts assemble data from multiple live/current DB rows rather than
  one immutable chain root.

### Canonical decision

`EvidenceEnvelope` is the immutable hash-linked root for request, Workload,
MarketDecision, PricingDecision, RuntimeDecision, PlacementDecision,
TopologyDecision, execution attempts, VerificationContract/outcomes,
SettlementPlan/outcomes and final receipt.

Existing lane receipts become authorized projections of one envelope version.
Specialist project/evidence receipts may be child evidence nodes, not alternate
transaction roots. File evidence binding remains a separate provenance layer
that can bind an envelope receipt artifact.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| DB rows → lane receipt | multiple scans/JSON allocations | missing canonical stages; later mutable rows can affect assembly | append envelope nodes at state transitions, project once at read |
| lane receipt → evidence file | JSON copy and file binding | transaction and producer provenance use unrelated roots | file binding cites envelope root; authorities remain distinct |
| specialist project receipt → final outcome | separate roots | full request-to-money chain cannot be traversed | attach as child evidence node by digest |

### Removal sequence and proof

Add envelope schema/validator, write chain nodes transactionally per lane, switch
receipt readers, prove historical compatibility, then stop authoritative receipt
assembly from current mutable rows. Completion requires tamper/truncation/order/
tenant rejection and bounded receipt latency. Any accepted chain mutation or
history loss is rollback.

## 11. ServiceLease

### Current representations and callers

- `ServiceLease` in `control/service_leases.go` is a substantive aggregate with
  creation, pricing, prepaid reservation, cumulative metering, heartbeat,
  rolling upgrade, recovery/failover, terminal settlement and receipt paths.
- `ServiceLeaseRequest`, offer registration, assignment, heartbeat, pricing
  authority, market detail, events and terminal settlement are distinct
  lifecycle DTOs/records.
- The reserved data plane is in `control/service_lease_data_plane.go`.

### Canonical decision

The existing `ServiceLease` remains the canonical service lifecycle aggregate.
It is extended to reference canonical Workload/service contract, MarketDecision,
RuntimeDecision, PlacementDecision, TopologyDecision, VerificationContract,
PricingDecision, SettlementPlan and EvidenceEnvelope identities.

Request/offer/assignment/heartbeat remain DTOs or events; they may not carry a
second mutable lifecycle truth. Autoscaling and scale-to-zero become explicit
lease policy/decision/events rather than inference from worker-reported warm
replicas.

### Conversion, allocation, and loss

| Edge | Allocation/copy | Current loss | V2 disposition |
|---|---|---|---|
| request + offer → ServiceLease | transaction/JSON copies | canonical decision chain and failure domain absent | acceptance binds all decision digests and region/failure epoch |
| heartbeat → mutable lease aggregate/events | DB row update plus append event | worker WarmReplicas acts as scaling observation, not governed decision | controller emits explicit scale decision/event; heartbeat is evidence |
| lease → receipt | multiple queries/JSON | per-request execution/verification chain absent | continuous envelope plus request child envelopes |

### Removal sequence and proof

Bind canonical decisions first, then add governed scaling/region loss/per-request
evidence. Remove any duplicate state fields after event/aggregate replay parity.
Completion requires worker/region loss, upgrade, scale events, meter retry/
duplicate safety, 100% money reconciliation and SLO receipts. Lost/double money,
overbooking or unrecoverable state is rollback.

## Cross-authority serial order

The register fixes the implementation dependency; it is intentionally not an
invitation to land all types at once:

```text
Capability snapshot
  → MarketDecision
  → RuntimeDecision
  → PlacementDecision
  → TopologyDecision
  → atomic accepted decision chain
  → VerificationContract
  → SettlementPlan
  → EvidenceEnvelope
  → canonical Workload migration
  → Digital Twin / indexes / replay
  → ServiceLease full lifecycle
```

PricingDecision remains canonical throughout. Each migration must delete or
demote the old production writer before the next dependent authority treats the
new object as truth.

## Static proof census at baseline

The following baseline facts were verified before the register was frozen:

- Only `PricingDecision`, `PlacementDecision`, and `ServiceLease` exist by exact
  V2 type name.
- `PlacementDecision` currently has mode-only semantics and therefore cannot be
  accepted as the V2 object without migration.
- Batch admission calls `buildWorkloadDecisionForSubmit`; measured runtime cost,
  placement and topology are recorded after `SubmitJobTx` as shadow evidence.
- Actual batch worker selection remains in claim-time scheduler SQL.
- Realtime and lease paths persist lane-specific clearing/pricing/placement
  evidence, but neither produces the complete cross-lane decision chain.
- No `EvidenceEnvelope`, historical network snapshot corpus, coherent network
  epochs, Digital Twin, or 10-to-1M candidate index exists.

These facts are entry-state assertions, not permanent invariants. Later mini-
audits must update the register as types migrate and remove entry-state guards
rather than teaching tests that V2 must remain absent.
