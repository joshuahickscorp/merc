# Execution frontier lane

Branch: `perf/execution-frontier`. `release/rc1-go-closure` stays frozen.

This maps the ten-step faster/cheaper order onto what the tree actually
contains, so no step is started against a wall that was already visible.
Everything below was probed against the code on 2026-07-30, not inferred from
intent. `docs/FRONTIER_300X.md` and `docs/SPEED_LANE_2026-07-27.md` hold the
measured throughput authorities and are not restated here.

No global refactor. The boundaries the Execution Brain needs mostly exist
already under different names, listed per step.

## 1. Calibrated compute planning — MEASURING

Existing: `control/compute_plan.go` freezes the geometry; `eta_calibration` plus
`Store.ETABiasFactor` (`control/store.go:609`) close the loop for duration only,
one-sided and clamped so a fast window cannot shrink an SLA promise.

Added this lane: `control/plan_actuals.go` records predicted-versus-realized for
`output_tokens`, `task_count`, `task_attempts` and `compute_usd` at finalize,
scoped by (job_type, tier, model_ref, input_depth_band) plus the runtime and
hardware class that executed the job. `GET /admin/plan-accuracy` reports median
and p90 ratio and MAPE per bucket, untrusted below `driftMinSamples`.

Observed-only by construction. No money, reserve, pricing or admission path
reads it. Promoting an estimator from this evidence is a separate change that
must state its own quality and money effect.

Not recorded, and why:

| quantity | blocker |
|---|---|
| peak VRAM / KV growth | `worker_memory_samples` is host available/effective GB per worker. `minimum_memory_gb` is an admission floor, not a predicted usage. Needs worker-side per-job device telemetry. |
| storage / transfer bytes | `jobs.economic_output_bytes` is realized, but no predicted byte count is frozen in the plan to compare against. |
| duration | already owned by `eta_calibration`. Two learners over one quantity is a bug. |

Truth boundary (`control/plan_actuals.go`, `control/plan_calibration.go`):

- `observation_class` is `PRIMARY_EXECUTION`, `CACHE_HIT` or `SYNTHETIC_TEST`.
  Only the first trains ordinary planning; the others are labelled rather than
  dropped so reuse and fixture coverage stay visible.
- A hedged chunk counts once. Summing an original and its dynamic copy inflated
  realized output by the hedge rate, which made the ceiling estimator look most
  accurate exactly when the fleet was struggling enough to hedge.
- Only terminal `complete` jobs are recorded.
- `ResolvePlanCalibration` walks exact → runtime+model+depth → model+depth →
  model → workload_class → global and names the level and sample count it landed
  on. An empty scope field is skipped, never matched as a wildcard.
- `CalibrationPromotable` gates any use: 100-sample floor, MAPE ≤ 35%,
  p90/median ≤ 2.0, no active drift alarm, named revision, shadow window that
  measurably improved, receipt digest. `AffectsMoney` is refused outright.
- `TestCalibrationIsUnreachableFromMoneyAndAdmissionPaths` enforces the
  separation in the build: 22 money/pricing/settlement/admission files may not
  reference calibration, and nothing outside a 6-file allowlist may either.

Next: enough finalized jobs to fill a trusted bucket, then argue the first
estimator change from the measured band through the promotion gate.

The project compiler now has a durable buyer-scoped evidence boundary in
addition to the upload response: `POST /v1/projects/compile` writes an
append-only proposal or bounded-probe receipt, and
`GET /v1/projects/compile/{id}` returns the same IR after revalidating the
project/artifact and IR digests. Probe receipts require a prior proposal
receipt from the same buyer; neither state creates a quote, reserves capacity,
or executes work. Calibration and execution remain explicitly refused.

Render decomposition is now a buyer-scoped production read rather than a
test-only helper: `GET /v1/projects/compile/{id}/render/{step}/units/{ordinal}`
expands one bounded frame/camera/tile/sample ordinal from that immutable receipt.
The response is `DECOMPOSITION_ONLY_NOT_EXECUTABLE` and carries the unresolved
asset-locality, runtime, worker, assembly, verification, and settlement refusal;
it does not create a task, reserve capacity, or move money. A real render runtime
and deterministic assembly/settlement path are still required before this lane
can claim execution capability.

## 2. Work elimination — LARGELY DONE

| item | state |
|---|---|
| exact-result cache | `control/exact_reuse.go` — content-addressed identity, own settlement path. **Now tenant-scoped**: it was shared across buyers, which is a cache-existence side channel. |
| in-flight coalescing | **WIRED** — `control/inflight_coalescing.go`, governed `inflight_executions` with lease, state machine, bounded re-election and expiry; called from the realtime lane after the exact-cache miss. The old unwired `ClaimInflightLeader`/`ReleaseInflight` and `inflight_requests` are deleted. |
| tokenized prefix trie | `control/prefix_routing.go:173` — `ComputePrefixChain` over token ids, `DeepestWarmPrefix`, value-ranked eviction (`EvictPrefixCacheToBudget`) |
| KV-hit-aware routing | prefix warmth feeds the scheduler; `prefixWarmTTL` is deliberately shorter than model warmth |
| prepared tools/schema identity cache | **WIRED** — `control/realtime_identity_cache.go`; bounded tenant/profile/policy-scoped cache is called by exact reuse, coalescing, and cache population. It caches semantic request identity only; no tokenizer exists. |
| image / audio preprocessing caches | not applicable — no image or audio runtime exists (`docs/SHIPPABILITY_STATUS.md`; the image route returns 503) |
| deterministic JSON / tool scaffolding | absent |

Coalescing proves the shape the milestone asks for: 128 concurrent callers elect
exactly one leader, the followers reconcile, one supplier payable, an independent
discounted receipt each, positive Merc contribution, and physical tokens that do
not grow with the followers. Cross-tenant sharing is deliberately not attempted —
`RequestIdentity` carries a tenant scope, so two tenants issuing byte-identical
requests never meet.

The control plane still has no model tokenizer, so token-ID caching remains absent by
design. Prepared request identity now has a bounded production cache; the benchmark
records the hit path separately from the measured canonicalisation baseline so this
small optimisation cannot be mistaken for a tokenizer or pricing authority.

Realtime demand now also leaves a receipt-bound market observation. The atomic
offer reservation records candidate depth, selected rank, supplier ask in fixed
point nanos, and the selected worker/supplier identities beside the immutable
`PricingDecision`; `/v1/realtime/requests/{id}/receipt` returns the same
`market_clearing` object. The order-book crossing is production-wired and
tested through two live offers, including the database immutability refusal.
This raises the realtime liquidity evidence, not the local-fabric claim: region
authority, calibrated net costs, and a multi-site market remain absent.

The operator surface now also exposes `GET /admin/market/liquidity/network`, a
single bounded receipt that composes the realtime and warm-service lane reports
for the same requested retention window. It preserves each lane's own offer,
demand, fill, utilization, churn, and fixed-point price-depth evidence and
states that the scope is retained Merc lanes only. It does not infer a global
market, legal region, or a new price.

Fabric topology evaluations are also replayable through the authenticated
worker-scoped `GET /v1/worker/fabric/topologies/{id}` read. The response
reconstructs the exact persisted links, synthetic collective evidence, freshness
and planner refusal. It remains evidence only: the stored non-admissible status
cannot create a `LOCAL_CLUSTER` placement or a gang scheduler grant.

## 3. Continuous batching — PARTIAL

`agent/src/quantized_llama_batched.rs` has the primitives: padded-mask batched
prefill, `compact_kv_cache`, `truncate_kv_cache`, per-row KV byte accounting.
`agent/src/executor.rs:595` `generate_batch` sizes a batch against a memory
budget (`batch_width_cap`).

That is memory-budgeted **static** batching inside one task. Token-budget
batching across arriving requests, length bucketing, adaptive queue delay and
latency classes do not exist. Unblocked.

## 4. Runtime tournament — SCHEMA UNBLOCKED, PRODUCT STILL SINGLE-RUNTIME

**Update 2026-07-30.** The schema blocker below is resolved. `runtime-authority.json`
is now v2 with a `runtimes[]` registry, governed invariants replacing the
two-model/two-cell count check, and lifecycle states that decide routability.
Four profiles are registered:

| profile | lifecycle | routable | evidence |
|---|---|---|---|
| `candle_metal` | ACTIVE | yes | `evidence/canary/real-runtime-realtime.json` |
| `mlx_metal` | VALIDATED | no | `docs/SPEED_LANE_2026-07-27.md` |
| `llama_cpp_metal` | VALIDATED | no | `docs/SPEED_LANE_2026-07-27.md` |
| `vllm_cuda` | DRAFT | no | `evidence/runpod/cuda-first-proof.json` |

Only CANARY and ACTIVE profiles project into the advertised capability set, so
registration widened nothing that is sellable. A benchmark that ran outside the
product carries a profile to VALIDATED and no further.

**Merc has a multi-runtime schema, not a multi-runtime product.** The milestone
is one governed workload executed through two independently registered profiles
at the same declared quality tier, with complete execution and money receipts,
comparable benchmark evidence, and a shadow selector that predicted the better
one.

**Done since.** Governed identity now lives in PostgreSQL: `runtime_engines`,
`runtime_profiles`, `runtime_profile_models`, `runtime_profile_hardware`,
`runtime_profile_capabilities`, synced under the migration lock. Routability is
a derived column with a CHECK, not an assertable flag, and a partial unique
index refuses two routable profiles claiming one cell. Content immutability is
enforced at sync — an existing `(runtime_profile_id, revision)` whose digest
moved is a hard refusal. Worker admission validates engine, platform,
per-claimed-cell memory and device count against the profile; `device_count`
was pure documentation before this.

The migration is deliberately mid-transition: `runtime_profile_id` is nullable,
`workers.engine` and `workers_engine_valid` both remain, and a trigger
dual-validates. `ReconcileWorkerRuntimeProfiles` is the gate for the rest.

**Update 2026-07-30, later.** The second runtime is chosen, driven and measured.
`docs/runtime/SECOND_RUNTIME_CENSUS.md` records why it is `llama_cpp_metal` on
the **embed** cell and not MLX and not the infer cell: MLX is not installed and
has no agent code path at all, and llama.cpp's `batch_infer` cell is `byte_exact`
in the one configuration where llama.cpp is byte-deterministic on Metal and runs
at 1.02× serial there, so proving the chain on it would prove it on a
configuration Merc would never route to.

Done since:

- `RuntimeDriver` (`agent/src/runtime_driver.rs`) — validate, launch, health,
  embed, cancel, drain, metrics — implemented by `CandleDriver` and
  `LlamaCppDriver`, with `EmbedRunner` as a real caller rather than a parallel
  abstraction. `LlamaCppDriver::validate` refuses a `byte_exact` cell outright.
- Artifact format now reaches the artifact list, not just the cell:
  `all-minilm-l6-v2` declares the GGUF llama.cpp loads beside the safetensors
  candle loads, and a cell declaring a format with no bytes behind it is refused.
- The profile content digest binds each cell's **resolved artifacts**, so
  repointing which GGUF backs `gguf` can no longer change every executed byte
  while the revision and digest stand still.
- A receipt-bound benchmark authority exists for the embed cell:
  `evidence/perf/runtime-benchmarks/embed-cell-candle-vs-llama-cpp-r1.json`,
  produced by `merc-agent bench-embed` — one harness, one corpus, both drivers,
  the quality gate evaluated before any timing. llama.cpp wins at every batch
  size measured (1.5×, 6.7×, 2.7×, 1.2× at batch 1/8/32/128) and clears the 0.999
  cosine gate at 0.999998 minimum.

Remaining to reach the milestone:

1. migration steps 6–7 — `NOT NULL`, then drop `workers_engine_valid`, gated on
   a clean reconciliation in a real deployment. Steps 6–8 are currently satisfied
   by the stronger dispatch-time invariant (`worker_capability_requires_profile`)
   rather than by `NOT NULL`, because enrollment legitimately creates a worker row
   before any profile is known;
2. a complete Merc chain — task, verification, money, payable, receipt — on
   `llama_cpp_metal`. The engine executes real work through the driver and clears
   the gate; what has not been driven is that output through submit → claim →
   commit → verify → settle → receipt. Until that runs, `llama_cpp_metal` stays
   `VALIDATED`: a benchmark is a prerequisite for `REAL_RUNTIME_PROVEN`, not a
   substitute for the chain;
3. `RuntimeSelector` in shadow mode plus regret measurement.

A governance note that falls out of the measurements and will matter at step 2:
`validateBenchmarkAuthorityBinding` refuses a **routable** profile that serves a
`byte_exact` cell on a benchmark authority recording non-byte-determinism.
`llama_cpp_metal` declares such a cell, so it cannot become routable while it
does — correctly. Reaching CANARY on the embed cell therefore means dropping the
infer cell from that profile, not arguing with the rule.

### The original blocker, for the record

MLX versus llama.cpp is already **measured** on this hardware
(`docs/SPEED_LANE_2026-07-27.md`). What does not exist is the authority to
select among runtimes:

- `control/runtime-authority.json` describes exactly one runtime
  (`candle_metal`), two models, two cells.
- `loadRuntimeAuthority` (`control/runtime_authority.go:82`) panics unless
  `len(Models) == 2 && len(Cells) == 2`, and `runtimeAuthorityDocument.Runtime`
  is a single object, not a list.
- `workers_engine_valid` (`control/schema.sql:1162`) constrains
  `engine = 'candle'`.
- `agent/src/vllm.rs` exists but the control plane cannot advertise it.

So there is nothing to hold a tournament between. This is the `RuntimeSelector`
boundary from the authorized targeted-refactor list, and it is a capability
blocker rather than a profiling result: a schema that admits one runtime cannot
be measured into admitting two. Doing it means making the authority document
multi-runtime while keeping every existing guarantee — per-cell minimum memory,
onboarding policy, matrix digest binding into task provenance, and the
`tasks_runtime_provenance_complete` invariant.

Order note: this is the first step in the list whose prerequisite is structural,
so it should be scheduled after steps 1–3 produce measurements, not before.

## 5. Quantization and kernels — PARTIAL

A governed quality suite already exists and already rejected a quantization:
`evidence/bench/quality-suite.jsonl` approved 8-bit as OUTCOME-EQUIVALENT and
refused 4-bit (`docs/FRONTIER_300X.md`). Mixed-bit profiles, dequant/GEMM
profiling, weight prepacking and graph/shape compilation do not exist.

## 6. Speculation portfolio — PRIMITIVE ONLY

`KvCacheSlot::truncate` is implemented and tested for the accept-k-of-n rollback
shape (`agent/src/quantized_llama_batched.rs:912`). No proposer of any kind —
no suffix, n-gram, MTP, EAGLE or draft model. Unblocked, and suffix/n-gram is
the cheapest entry.

## 7. Hardware and energy arbitrage — HONESTLY UNMODELED

`hwClassCostRank` (`control/scheduler.go:161`) is a static ordinal by marginal
cost to Merc, used to prefer cheaper hardware on claim. Energy is explicitly
declared unknown in both the quote and the pricing decision: "power draw and
provider-specific energy cost are not yet modeled" (`control/quote.go:660`,
`control/api.go:1162`). Selecting by *verified outcome cost* needs the step-1
actuals plus per-device power, neither of which is metered today.

## 8. Outcome cascades — ABSENT

No small-model-then-evaluator-then-large-model path exists. The
`OUTCOME_EQUIVALENT` label is already defined and defended in
`docs/FRONTIER_300X.md`, so a cascade must report under it and never as
model-exact.

## Promotion rule

Every promoted profile states quality, latency, throughput, energy, provider
cost, supplier contribution, Merc contribution and buyer-price effect, and must
improve at least one without degrading another beyond policy. The four
authorities stay separate: PHYSICAL EXACT-MODEL, DELIVERED REUSE, EXACT CACHE,
OUTCOME-EQUIVALENT.

No complete replacement for an upstream engine is built unless measurement shows
a stable, profitable bottleneck that upstream and a narrow extension cannot
solve.
