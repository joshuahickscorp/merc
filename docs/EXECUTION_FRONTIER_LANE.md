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

## 2. Work elimination — LARGELY DONE

| item | state |
|---|---|
| exact-result cache | `control/exact_reuse.go` — content-addressed identity, own settlement path |
| in-flight coalescing | **implemented but NOT wired** — `ClaimInflightLeader`/`ReleaseInflight` (`control/exact_reuse.go:256`) have no caller outside their own file and tests. An earlier revision of this table wrongly marked it done. |
| tokenized prefix trie | `control/prefix_routing.go:173` — `ComputePrefixChain` over token ids, `DeepestWarmPrefix`, value-ranked eviction (`EvictPrefixCacheToBudget`) |
| KV-hit-aware routing | prefix warmth feeds the scheduler; `prefixWarmTTL` is deliberately shorter than model warmth |
| tokenization / tool-schema caches | absent |
| image / audio preprocessing caches | not applicable — no image or audio runtime exists (`docs/SHIPPABILITY_STATUS.md`; the image route returns 503) |
| deterministic JSON / tool scaffolding | absent |

Remaining work here is small and unblocked.

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
one. Remaining to reach it:

1. governed DB profile identity — `workers_engine_valid CHECK (engine='candle')`
   is still a string check rather than a foreign key into the registry;
2. the runtime-adapter boundary;
3. `RuntimeSelector` in shadow mode — compute, record, compare, change nothing;
4. a benchmark authority for a second profile (`benchmark_authority` is empty
   for all three non-routable profiles, and the validator refuses routability
   without one);
5. a complete Merc canary chain on that profile.

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
