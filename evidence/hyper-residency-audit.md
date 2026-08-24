# HYPER-H — Can Merc even represent that a worker is warm?

Read-only audit of HEAD `4b10b56e0c491c2e9fdb652344d07020c78cd43f`
(`docs: say plainly that the test-mode Connect matrix does not clear live eligibility`).
No schema or control-plane file was modified. Sources were read with
`git show HEAD:<path>` / `git grep --cached` because this worktree is a sparse
checkout; absence of a path on disk is not evidence the path is absent in git.

Three different words named "residency" already live in this tree. They are
not the same fact:

| Word in the tree | What it actually is | Home |
|---|---|---|
| Model / prefix **warmth** | "this worker currently holds weights or a believed KV prefix" | `worker_model_state`, `worker_prefix_state`, heartbeat `loaded_models` / `resident_models`, realtime `warmth` |
| Service-lease **residency nanos** | A *price* for keeping a replica reserved, not a loaded-artifact inventory | `service_lease_worker_offers.residency_nanos_per_replica_hour` |
| Buyer **data residency** / cloud **startup_residency** | Geographic constraint, or a pricing category for pod-startup / idle cloud seconds | `jobs.data_residency`, `JobConstraints.DataResidency`, `suppliers.data_country`; `PricingCostComponent startup_residency` |

This lane is about the first of those. The other two are cited only so they
are not mistaken for it.

---

## 1. Inventory: what worker / device state Merc models today

### 1.1 `workers` — durable identity and live resource stamps

`CREATE TABLE workers` at `control/schema.sql:21` plus later `ALTER TABLE`
columns. Current fields the control plane can persist about a worker:

| Field | Source | What it can say |
|---|---|---|
| `id`, `supplier_id` | `schema.sql:22-23` | Worker and owner identity |
| `hw_class` | `:24` | Coarse class (`apple_silicon_*`, `nvidia_*`) |
| `memory_gb`, `bw_gbps` | `:25-26` | Declared capacity / measured bandwidth |
| `last_seen_at`, `version` | `:28-29` | Liveness, agent version |
| `priority_claim_streak` | `:30`, `:1215` | Fairness debt, not warmth |
| `engine`, `build_hash`, `build_identity_policy`, `hardware_identity` | `:811-814` | Enrolled runtime/build/device identity |
| `agent_session_id`, `agent_session_started_at` | `:815-816` | Process epoch |
| `supported_jobs`, `supported_models` | `:1211-1212` | Enrollment *declaration* of what is runnable / "resident or runnable locally" — not a live loaded set |
| `min_payout_usd_hr`, `thermal_ok` | `:1213-1214` | Ask floor, thermal flag |
| `effective_memory_gb`, `available_memory_gb`, `reserved_headroom_gb`, `throttled` | `:1284-1287` | Live memory / pause flag (heartbeat) |
| `runtime_profile_id`, `runtime_profile_revision`, `runtime_profile_digest` | `:5435`, `:5545-5546` | Governed profile *enrollment* identity (all three or none) |
| `sandboxed`, `unsandboxed_opt_in` | `:7114-7115` | Containment |
| `gpu_count`, `memory_gb_per_gpu`, `interconnect`, `os_version` | `:7722-7725` | Host topology |
| `region`, `region_provenance`, `failure_domain`, `interruption_policy`, `disk_gb` | `:7729-7742` | Geography / preempt / disk |
| `capability_epoch`, `capability_digest` | `:7745-7746` | Pointer at last sealed `NodeCapability` |
| `device_slot` | `:8194` | Internal live-device index; "never an eligibility input" (`store_workers.go` comment on `insertWorkerWithDeviceSlotTx`) |

There is no `workers.warm`, no `loaded_models` column on `workers`, no
shader/CUDA-graph/BVH/container column.

### 1.2 Wire types: heartbeat and enrollment

**Enrollment (capability, not warmth).** `WorkerCapability` (`control/types.go:186`):
`HWClass`, `Engine`, `BuildHash`, `BuildIdentityPolicy`, `HardwareIdentity`,
`MemoryGB`, `MemoryBwGbps`, `GPUCount`, `MemoryGBPerGPU`, `Interconnect`,
`SupportedJobs`, `SupportedModels`, `MinPayoutUsdHr`, `Benchmarks[]`,
`AgentVersion`, `OSVersion`, `Sandboxed`, `UnsandboxedOptIn`, optional
`Region` / `FailureDomain` / `InterruptionPolicy` / `DiskGB`.

`BenchResult` (`types.go:135`) can carry a one-shot `LoadMS` (cold-load
wall-clock) and `TPS`/`EPS`. That is a registration measurement, not a live
"still loaded" bit.

**Canonical snapshot.** `NodeCapability` (`control/node_capability.go:168`)
explicitly *excludes* live warmth:

```
// Model declaration (enrollment). Live warmth is availability/residency
// and is refreshed on heartbeat epochs.
SupportedJobs   []string `json:"supported_jobs"`
SupportedModels []string `json:"supported_models"`
```

(`node_capability.go:215-218`.) Snapshots land in
`worker_capability_snapshots` (`schema.sql:7749`: `worker_id`, `epoch`,
`captured_at`, `digest`, `snapshot JSONB`) and are append-only.

**Live heartbeat.** Go `Heartbeat` (`control/types.go:301`) and the matching
Rust `Heartbeat` (`agent/src/types.rs:380`):

| JSON field | Go / Rust type | Meaning |
|---|---|---|
| `loaded_models` | `[]string` | Model ids currently in the agent's pool |
| `resident_models` | `[]ResidentModel` | Measured `rss_delta_bytes` + `load_ms` per warm model |
| `evicted_models` | `[]string` | Models dropped since the previous heartbeat |
| `available_memory_gb` / `effective_memory_gb` / `reserved_headroom_gb` / `throttled` | floats / bool | Live memory / pause |
| `cpu_pct`, `gpu_pct`, `gpu_temp_c`, `active_tasks` | telemetry | Not residency |

`ResidentModel` (`control/types.go:295`, `agent/src/types.rs:368`):

```
type ResidentModel struct {
    ModelID       string `json:"model_id"`
    RSSDeltaBytes int64  `json:"rss_delta_bytes"`
    LoadMS        uint64 `json:"load_ms"`
}
```

The agent actually fills this from an in-process table keyed only by model
id (`agent/src/pool.rs:31-55`, `agent/src/main.rs:3844-3872`). The pool
holds `Warm<Embedder>` and `Warm<LlamaBackend>` (`pool.rs:79-84`).
Tokenizer objects live *inside* those backends (`agent/src/executor.rs:318`,
`:546`); they are not a separate advertised resident.

### 1.3 `worker_model_state` — the live "this model is warm" table

```
CREATE TABLE IF NOT EXISTS worker_model_state (
    worker_id      UUID NOT NULL,
    model_id       TEXT NOT NULL,
    last_seen_warm TIMESTAMPTZ DEFAULT now(),
    PRIMARY KEY (worker_id, model_id)
);
-- plus:
rss_delta_bytes BIGINT   -- measured RSS delta at load; NULL = declaration-only
load_ms         BIGINT   -- measured cold-load ms; NULL = declaration-only
```

(`schema.sql:1702-1714`.) Written by `HeartbeatTx` (`control/store_workers.go:707-790`):

- `resident_models` upserts measured `rss_delta_bytes` + `load_ms`.
- legacy `loaded_models` only refreshes `last_seen_warm` (NULL measurements).
- `evicted_models` `DELETE`s the row immediately.

TTL used by consumers: **60 seconds** (`scheduler.go` `warm_for_task`,
`benchmark.go` CandidateWorkers, `quote.go` WarmEligibleWorkerCount,
`service_leases.go` measuredServiceLeaseWarmCapacityTx,
`latency_watchdog.go` isColdModelStraggler).

### 1.4 `worker_prefix_state` — believed KV prefix warmth

```
CREATE TABLE IF NOT EXISTS worker_prefix_state (
    worker_id      UUID NOT NULL REFERENCES workers(id) ON DELETE CASCADE,
    prefix_id      TEXT NOT NULL CHECK (prefix_id ~ '^pfx_[0-9a-f]{32}$'),
    last_seen_warm TIMESTAMPTZ NOT NULL DEFAULT now(),
    hits           BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (worker_id, prefix_id)
);
-- plus:
depth   INT  NOT NULL DEFAULT 0
cell_id TEXT NOT NULL DEFAULT ''
```

(`schema.sql:3392-3470`.) `prefix_id` is a routing hint hashed from *prompt
text* (`ComputePrefixID`, domain `merc-prompt-prefix-v1`,
`control/prefix_routing.go:111-115`), or from a 4-byte surrogate tokenisation
of leading input bytes because "The control plane has no model tokenizer"
(`prefix_routing_path.go:26-32`). It is not a KV tensor identity.

Belief model, quoted from the file that owns it (`prefix_routing.go:44-51`):

```
Merc does not own the engine's KV cache and cannot see inside it. The table
worker_prefix_state is therefore a *model* of residency, not a mirror of it.

A (worker, prefix_id, depth) row with last_seen_warm within prefixWarmTTL
means "this worker recently served a request that materialised this prefix,
so its engine *probably* still holds the matching KV".
```

TTL: **90 seconds** (`prefixWarmTTL`, `prefix_routing.go:102`). `cell_id` is
explicitly "advisory metadata on the belief row: claim ranking still keys by
`worker_id`" (`prefix_routing.go:600-605`, `schema.sql:3465-3468`).

Correction: `TaskCommit.CachedPromptTokens` (`types.go:270-289`) can
invalidate believed-warm rows when the engine reports
`cached_tokens == 0`. Engines that expose no signal stay on TTL.

### 1.5 Realtime offers — self-declared warmth enum

`realtime_worker_offers` (`schema.sql:3617`):

| Field | Constraint / meaning |
|---|---|
| `(worker_id, runtime_profile_id)` PK | One offer per worker×profile |
| `runtime_profile_sha256` | Digest-bound profile |
| `placement_plan` / `placement_plan_sha256` | Host topology (tensor parallel etc.), not loaded-artifact inventory |
| `warmth` | `CHECK (warmth IN ('HOT','WARM','CACHED','COLD'))` |
| `max_active_sequences`, `available_sequences` | Capacity |
| `supplier_*_usd_per_million_tokens` | Ask |
| `status`, `last_seen_at` | `ACTIVE`/`DRAINING`/`FAILED`/`QUARANTINED`; 45s eligibility |

Wire: `RealtimeOfferRegistration.Warmth` and
`RealtimeOfferHeartbeat.Warmth` (`realtime_store.go:178`, `:187`;
`agent/src/types.rs` `warmth: String`). Validated as the four-enum set
(`realtime.go:129-132`, `:220-223`). Samples table
`realtime_offer_samples` does **not** even store `warmth`
(`schema.sql:3646`).

### 1.6 Service-lease offers — warm *replica counts* and a residency *price*

`service_lease_worker_offers` (`schema.sql:3946`):

| Field | Meaning |
|---|---|
| `maximum_warm_replicas`, `available_warm_replicas` | How many replicas this worker will hold warm |
| `residency_nanos_per_replica_hour` | Price of keeping one replica resident |
| `supplier_nanos_per_replica_hour` | Supplier ask |
| `runtime_profile_id` + `sha256`, `region`, `p95`/`p99`, `status` | Eligibility |

Advertised `available_warm_replicas` is gated on a *measured*
`worker_model_state` row (`service_leases.go:657-673`):
`last_seen_warm` within 60s **and** `rss_delta_bytes IS NOT NULL` **and**
`load_ms IS NOT NULL`. Declaration-only `loaded_models` rows do not count.

### 1.7 Runtime authority / cells / capabilities (identity, not live load)

- `control/runtime-authority.json` + `authorityRuntimeProfile`
  (`runtime_authority.go:314`): `engine`, `tokenizer_revision`,
  `chat_template_id`, `device`, hardware platforms, `capabilities.prefix_cache`
  (a boolean *the engine supports prefix cache*, not "a prefix is loaded
  here"), cells with model/runner/wire kind.
- Synced tables: `runtime_engines`, `runtime_profiles`,
  `runtime_profile_models`, `runtime_profile_hardware`,
  `runtime_profile_capabilities` (`schema.sql:5357-5432`).
- Dispatch grant: `worker_authorized_capabilities`
  (`schema.sql:1220`: `worker_id, cell_id, runtime_id, job_type, model_ref,
  model_kind, matrix_sha256, authorized_at, routable`).
- Capability digest includes tokenizer revision and `prefix_cache` *flag*
  (`capability_manifest.go:24`, `:144`, `:182-216`).

These say "this worker is *allowed* to run this cell". They do not say the
weights, tokenizer, KV, shaders, or container are currently loaded.

### 1.8 Other state that is *not* worker warmth

| Object | Table / type | Why it is not "worker is warm" |
|---|---|---|
| Exact result reuse | `exact_result_cache` (`schema.sql:3492`) | Global request-identity → result; not bound to a worker |
| In-flight coalescing | `inflight_executions` | Same-tenant in-flight result, not a loaded model |
| Tokenisation cache | `agent/src/token_cache.rs` `TokenizationCache` | Process-local encodings keyed by tokenizer revision; never heartbeated |
| Candle KV snapshot | `quantized_llama_batched.rs` `snapshot_kv_cache` / `restore_kv_cache` | In-process tensors on *one* LlamaBackend; no control-plane type |
| ETA / TPS | `worker_tps_cache`, `benchmark_results.tps`, `benchmark_results.load_ms` | Speed / historical cold-load, not current residency |
| Memory samples | `worker_memory_samples` | GB free, not what occupies it |
| Fabric | `worker_fabric_identities`, `fabric_*` | Link measurements; `fabric.rs` "residency" is *site/location* authority |
| Geographic residency | `jobs.data_residency`, `suppliers.data_country`, `workers.region` | Buyer constraint / ISO country |
| Cloud startup/residency cost | `StartupResidency PricingCostComponent` | Pod-startup / idle-cloud seconds; UNKNOWN for cloud, N/A for community (`runtime_cell_admission_binding.go:864-877`) |

---

## 2. Verdict per residency kind

The kinds named in the lane brief.

### 2.1 Loaded model — **representable today**

Where: heartbeat `loaded_models` / `resident_models` →
`worker_model_state(worker_id, model_id, last_seen_warm, rss_delta_bytes, load_ms)`.
Agent source of truth: `ModelPool.loaded_model_ids` + `residency_snapshot`
(`pool.rs:191-198`, `:31-55`).

Lost if you only have `loaded_models`: measurements stay NULL, and
service-lease warmth *refuses* those rows (`service_leases.go:660-661`).
Lost even with measurements: no engine, cell, quant, device, or tokenizer
revision on the row — identity is `model_id` text equal to `jobs.model_ref`.

### 2.2 Loaded tokenizer — **partially representable** (what is lost: live load)

Tokenizer *revision* is part of profile meaning
(`runtime_authority.go:332-335`, capability digest
`capability_manifest.go:144`). A worker enrolled against profile r1 "is not
interchangeable with one enrolled against r4 — it may be running a different
tokenizer" (`schema.sql:5540-5541`, `store_workers.go:324-325`).

What is lost: there is no `worker_tokenizer_state`, no heartbeat field, and
the in-process `Tokenizer` is a field of `Embedder` / `LlamaBackend`
(`executor.rs:318-345`, `:546-572`). Merc can say "this worker's *profile*
pins tokenizer commit X". It cannot say "that tokenizer is currently loaded
and warm" independently of the model row.

### 2.3 Compiled shaders — **not representable**

No table, heartbeat field, or capability flag. The only `kernelsWarm` in
control is a **function argument** to `gpuPlacementLicense`
(`render_device_placement.go:67`, `:151-154`), mirrored by
`scripts/lib/device_placement.py`. It is not read from any worker row.
Blender keeps shaders across frames with
`scene.render.use_persistent_data = True`
(`render/verify/blender_service.py:92-94`) — a local Cycles setting, never
reported.

### 2.4 Captured CUDA graphs — **not representable**

No schema, no heartbeat, no offer field. The only mention in-tree is a
measurement comment ("A cold CUDA graph is not the steady state") in
`scripts/cuda-embed-arm-measure.py`, which does not persist state.

### 2.5 Compiled Metal pipelines — **not representable** (same as shaders)

`kernelsWarm` / `use_persistent_data` as above. `gpuPlacementLicense`
refuses GPU dispatch when `kernelsWarm=false` with a hardcoded
"First Metal compile on this host is ~32.75s" (`render_device_placement.go:151-154`).
That number is a planner constant, not a per-worker observation.

### 2.6 KV cache — **partially representable** (what is lost: the cache itself)

Representable: *belief* that a worker recently materialised a prompt-prefix,
as `worker_prefix_state` + optional `cached_prompt_tokens` correction.
Realtime `capabilities.prefix_cache` says the *engine can* cache prefixes.
Realtime `warmth` enum includes `CACHED`.

Lost:

- tensor layout, dtype, paged vs contiguous, rope, quant, layer count
- engine / runtime_id on the prefix PK (only advisory `cell_id`)
- any way to *move* KV; Candle `KvSnapshot` never leaves the process
- a real tokenizer-derived prefix (control uses 4-byte surrogates)
- `kvBytesPerToken` is a Llama-3.2-1B constant applied to every engine
  (`prefix_routing.go:372-374`)

### 2.7 Prefix — **representable today** (as a routing hint, not as KV)

`jobs.prefix_id`, `job_prefix_chain(job_id, depth, prefix_id)`,
`worker_prefix_state`. Explicitly "an opaque routing hint: it never selects
a different model, runtime or price" (`schema.sql` comment above
`jobs.prefix_id`; `prefix_routing.go:40-42`).

### 2.8 Scene assets — **not representable**

No per-worker asset inventory. Render inputs are job `input_ref` object
keys. `gpuPlacementLicense` takes `triangleCount` / `instanceCount` as
planner arguments for a complexity class, not as "this worker holds the
scene".

### 2.9 Textures — **not representable** as resident artifacts

`textureBytes` is a planner argument (`render_device_placement.go:68`,
floor `devicePlacementTextureByteFloor = 1_000_000`). There is no
`worker_texture_state`.

### 2.10 BVH — **not representable**

Local Cycles `use_persistent_data` "keep BVH / shaders / kernels across
frames" (`blender_service.py:92-94`). A scene description in
`render/metal/scenes.py` mentions BVH build cost. Nothing is heartbeated
or stored.

### 2.11 Warm runtime — **partially representable** (what is lost: process-level load)

Partial homes:

- `workers.engine` + `runtime_profile_{id,revision,digest}` = enrolled runtime
- realtime/service offers `status='ACTIVE'/'READY'` + `last_seen_at` = a
  serving process is answering heartbeats
- agent `ModelPool` OnceCells = in-process Candle/llama.cpp backends are
  constructed

Lost: no "vLLM engine ready, weights loaded, CUDA context created" distinct
from the warmth enum / replica counts; no Metal/CUDA context object; no
compiled graph set.

### 2.12 Warm container — **not representable**

No worker-container table, no image digest on `workers`, no "container is
warm" heartbeat. Image pins exist in deploy/evidence (e.g. RunPod spend
receipts) as *ops* facts, not live worker state.

---

## 3. Placement / selection sites — can any prefer a warm worker over a cold one?

### 3.1 Batch claim `ClaimTasksTx` — **YES as a task-menu preference; NO as worker-vs-worker assignment; NO as slower-warm vs faster-cold**

Batch is a pull market (`worker_placement.go`: `BATCH_CLAIM_PULL`,
`PENDING_CLAIM` at accept). The SQL answers "which *task* should worker `$1`
take?", not "which *worker* should this job get".

Warmth terms, computed per job for the claiming worker
(`scheduler.go:750-796`):

```
(COALESCE(j.model_ref,'') <> '' AND EXISTS (
  SELECT 1 FROM worker_model_state wms
    WHERE wms.worker_id = $1 AND wms.model_id = j.model_ref
      AND wms.last_seen_warm > now() - interval '60 seconds'
)) AS warm_for_task,
...
GREATEST( ... MAX(c.depth) ... last_seen_warm > now() - interval '90 seconds'
         ... ) AS warm_prefix_depth
```

Comment immediately above (`scheduler.go:770-781`):

```
-- (warmth is a PREFERENCE in ORDER BY, never a WHERE predicate).
...
-- ORDER BY places this AFTER cheaper_class_online / cheaper_ask_online
-- on purpose: a warm expensive worker must not outrank a cold cheap one
-- without arithmetic that quantifies prefill saved vs class cost delta.
-- We do not have that arithmetic measured per (depth, class) pair, so
-- cost rank wins and warmth only breaks ties within a cost class.
```

Actual `ORDER BY` (`scheduler.go:1128-1143`):

```
ORDER BY (t.claimed_by = $1) DESC,
         ... priority fairness ...,
         cheaper_class_online ASC, cheaper_ask_online ASC,
         <shapeOrderExpr> ASC,
         worker_tps DESC,
         warm_prefix_depth DESC, warm_for_task DESC,
         job_dispatched_count ASC, t.created_at ASC
```

`worker_tps DESC` sits **above** both warmth terms. A slower warm worker
cannot beat a faster cold one even *within* a cost class, on this path.
Across classes, cheaper-class deferral also sits above warmth — confirmed
by the live-proof receipt:

`evidence/prefix/live-proof.json`:
`"cheaper_class_online ASC and cheaper_ask_online ASC sort ABOVE warm_prefix_depth DESC in the claim ORDER BY, so a warm expensive worker never displaces a cold cheap one."`

`bindBatchClaimWorkerPlacement` (`scheduler.go:1353-1355`, `:1425-1428`)
records locality belief for the receipt and states the limit: "Batch pull
cannot prove multi-worker affinity moved the global pick."

**Prefer warm worker over cold for the same job? No.** Two workers racing
with `SKIP LOCKED` are not ranked against each other by warmth; each
reorders its own menu.

### 3.2 `RankByCostThenPrefixAffinity` — **YES within cost+ask; not live assignment**

`control/prefix_placement.go:42-51`: CostRank ASC, AskUSDHr ASC,
WarmPrefixDepth DESC, WarmModel DESC. "Prefix depth never moves a candidate
across a cost-rank boundary." Pure function for tests; live claim SQL is
the production twin (and even that twin still puts `worker_tps` above
warmth, which this helper does not model).

### 3.3 Realtime authorize (push offer book) — **YES as a cost-class tiebreak**

Ranking CTE (`realtime_supplier_outcome_stats.go:148-154`):

```
row_number() OVER (ORDER BY
  e.verified_outcome_cost ASC,
  CASE e.warmth WHEN 'HOT' THEN 0 WHEN 'WARM' THEN 1 WHEN 'CACHED' THEN 2 ELSE 3 END,
  e.available_sequences DESC, e.last_seen_at DESC, e.worker_id ASC)
```

`realtime_clearing.go:9-35`, `:91-105`, `:253-267`: "Warmth is a tiebreak
only inside the same verified-outcome cost class"; `HOT < WARM < CACHED <
COLD`. Self-declared HOT cannot outrank a cheaper measured cost.

Hardware TPS / latency are **not** ranking terms on this path (offers have
no p95 column). At equal verified-outcome cost, a slower HOT worker **does**
beat a faster COLD worker. There is still no arithmetic of "cold-load ms
saved versus tokens/sec delta". Policy used to be
`lowest_warmth_then_supplier_rate_v1`; current receipts must use
`verified_outcome_cost_v1` (`realtime_clearing.go:40`,
`realtime_store.go:607`).

### 3.4 Service-lease clearing — **YES as an admission filter; not as warm-vs-cold ranking**

`CreateServiceLease` (`service_leases.go:768-787`):

```
AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
ORDER BY (supplier_nanos_per_replica_hour + residency_nanos_per_replica_hour) ASC,
         supplier_nanos_per_replica_hour ASC, worker_id ASC
```

Unmeasured workers advertise 0 replicas (`measuredServiceLeaseWarmCapacityTx`)
and cannot enter the book. Every candidate is already "warm-advertising".
Ranking is total *price* (supplier ask + residency nanos), not a warmth
enum, and not hardware speed. Failover uses the same sum
(`service_leases.go:1752`).

`TestServiceLeaseClearingRanksByTotalSupplierPlusResidency` pins that
sum, not "prefer the currently loaded worker".

### 3.5 `SelectRedundancyPeer` / `SelectEndgameRacePeer` — **YES, including slower-warm over faster-cold**

`CandidateWorkers` sets `MatchWorker.Warm` from `worker_model_state` 60s
TTL (`benchmark.go:22-28`).

`matchScore` (`scheduler.go:166-176`): `Reputation * TPS * 1.05` if Warm
— a 5% bonus, not a full override.

Then `rankPeersBySpeed` (`planner.go:211-218`):

```
if out[i].Warm != out[j].Warm {
    return out[i].Warm
}
return out[i].TPS[jobType] > out[j].TPS[jobType]
```

Warm is ordered **before** TPS. `SelectRedundancyPeerExcluding` returns
`rankPeersBySpeed(ranked, jobType)[0].ID` (`scheduler.go:367`).
`SelectEndgameRacePeer` additionally **filters out cold**
("never cold-race: a minutes-long load cannot cut a seconds tail",
`scheduler.go:406-408`) and then applies the same ranker (`:414`).

Callers: verification hedge (`verification.go:565`), watchdog
(`latency_watchdog.go:94`), worker hedge paths (`workers.go:652`, `:719`).
This is real placement of a *peer* worker, and it is the one site that
implements the economic slogan without qualification.

### 3.6 `PlanFanout` — **models the slogan; does not assign a worker to a task**

`PlannerWorker.startCostSecs` adds `ColdLoadSecs` when `!Warm`
(`planner.go:37-42`); default cold load 120s, or `benchmark_results.load_ms`
when present (`planner.go:238-251`). Sort is cheapest-start first, then
rate (`planner.go:82-91`). Used from `control/api.go:4527-4530` to size
chunks / log a modeled width. Comment in the log line: `[MODELED]`. Not a
claim-time worker pick.

### 3.7 Quote `WarmEligibleWorkers` — **count only, not a decision**

`quote.go:246`, `:890`, `:1415-1425`: `COUNT(*)` of claimable workers with
a fresh `worker_model_state` row. Displayed on the quote; does not select
a worker.

### 3.8 `gpuPlacementLicense` — **not live worker state**

`kernelsWarm` and `resident` are parameters
(`render_device_placement.go:67`). Resident floors move the CPU/GPU
crossover earlier; `kernelsWarm=false` refuses GPU. Nothing loads those
booleans from `worker_model_state` or any render-residency table.

### 3.9 Runtime-cell selector / shadow selection — **no**

`runtime_shadow_selection.go` / `runtime_cell_cost.go` rank *cells* by
measured supplier liability. They do not read `worker_model_state` or
offer `warmth`.

### 3.10 Latency watchdog — **consumes warmth, does not place**

`isColdModelStraggler` (`latency_watchdog.go:19-38`) is the inverse of the
60s `worker_model_state` EXISTS. It delays treating a cold-load as a
wedged task (`coldModelLoadAllowance = 6m`). Not a selector.

### 3.11 Verdict table

| Site | Prefer warm over cold? | Slower warm over faster cold? |
|---|---|---|
| `ClaimTasksTx` SQL | Task-menu only; not worker-vs-worker | **No** (`worker_tps` and cost-class sit above warmth) |
| `RankByCostThenPrefixAffinity` | Within cost+ask | **No** (no TPS; cost first) |
| Realtime authorize SQL | **Yes**, tiebreak after verified-outcome cost | **Yes at equal cost** (speed not ranked); **no vs cheaper ask** |
| Service-lease clearing | **Yes** as filter (cold/unmeasured cannot be selected) | Not ranked; all remaining are warm-advertising |
| `rankPeersBySpeed` (redundancy + endgame) | **Yes** | **Yes** (Warm before TPS) |
| Endgame extra filter | **Yes** (cold ineligible) | Among remaining warm, TPS |
| `matchScore` | Weak (×1.05) | Only if cold is <5% faster |
| `PlanFanout` | Modeled start-cost | Modeled only; not dispatch |
| Quote warm count | No | No |
| `gpuPlacementLicense` | Args, not fleet state | n/a |
| Cell/shadow selector | No | No |

---

## 4. What a trustworthy residency record would have to bind

| Required field | Has a home today? |
|---|---|
| **Identity of the resident thing** | **Partial.** Models: `worker_model_state.model_id` (catalogue / heartbeat id, not artifact SHA). Prefix: `prefix_id` = hash of prompt text or 4-byte surrogate, not KV bytes. Tokenizer: profile `tokenizer_revision` only. Shaders / CUDA graphs / Metal pipelines / textures / BVH / container: **no**. Runtime: `runtime_profile_id`+revision+digest is enrollment, not "this binary is loaded". |
| **Which worker holds it** | **Yes** for models and prefixes (`worker_id` PK). Realtime/service offers are also per `worker_id`. |
| **Runtime compatibility** | **Partial, wrong layer.** `workers.engine`, `runtime_profile_*`, `worker_authorized_capabilities.(runtime_id, cell_id, model_kind, matrix_sha256)` bind *permission* to run. `worker_model_state` and `worker_prefix_state` PK do **not** include engine/runtime/cell (prefix `cell_id` is advisory, default `''`, not in PK, not in claim JOIN). Realtime offers bind `runtime_profile_id`+`sha256` (strong for that lane). |
| **Hardware compatibility** | **Partial, wrong layer.** `workers.hw_class`, `hardware_identity`, `gpu_count`, `runtime_profile_hardware.platform`. Not on the warmth row. A warm-model row on an M3 Ultra is indistinguishable from one on a different SKU of the same `hw_class`. |
| **Freshness** | **Yes.** `last_seen_warm` + hard-coded TTLs (60s model, 90s prefix, 45s realtime/service offer). No per-row TTL, no expiry column. |
| **Confidence** | **No.** Prefix has `hits` (reuse count) and optional engine `cached_prompt_tokens` correction. No confidence/probability column. Model rows are boolean EXISTS. Realtime warmth is an enum with no evidence payload. |
| **How it was learned** | **Partial, implicit.** Model: heartbeat (measured vs declared distinguished by NULL rss/load). Prefix: inferred from task completion (`MarkPrefixWarm` / `MarkPrefixChainWarm`), optionally contradicted by observation. Realtime warmth: worker self-declaration. No `source` / `provenance` column. |
| **Privacy policy** | **No on the warmth row.** Geographic `jobs.data_residency` / `suppliers.data_country` is a different fact. Prefix hashing avoids storing prompt text; it is not a policy binding (tenant, retention, "do not route this prefix across workers"). |
| **Movement cost** | **No.** Nothing describes the cost of shipping KV, weights, BVH, or a container to another worker. `kvBytesPerToken` is a constant for *belief-table eviction*, and that eviction "never deletes anything a worker needs" (`prefix_routing.go:375-378`). |
| **Recompute cost** | **Partial.** `worker_model_state.load_ms` and `benchmark_results.load_ms` are measured cold-load ms; claim SQL does not use them. `ResidentModel.rss_delta_bytes` is occupancy, used to *authorize* service-lease capacity, not to rank. Prefix eviction uses `depth^1.01` (`prefillCostExponent`) as a stand-in for recompute. Planner default `plannerDefaultColdLoadSecs = 120`. Service-lease `residency_nanos_per_replica_hour` is a *price*, not a physical recompute cost. Cloud `startup_residency` is UNKNOWN for cloud supply. |

---

## 5. KV cross-worker compatibility trap

**Direct answer: nothing in the current schema would stop Merc from wrongly
assuming two workers holding "the same model" (or the same `prefix_id`) hold
interchangeable KV.**

Facts that create the trap:

1. **`prefix_id` is prompt-text identity, not KV identity.**
   `ComputePrefixID` hashes raw prefix bytes
   (`prefix_routing.go:111-115`). The live path uses 4-byte surrogate
   "tokens" because the control plane has no model tokenizer
   (`prefix_routing_path.go:26-32`). Two engines that tokenise the same
   UTF-8 differently still share a `prefix_id` if the leading *bytes* match.

2. **The prefix row does not name a KV representation.**
   PK is `(worker_id, prefix_id)`. Added `cell_id` is `DEFAULT ''`, not in
   the PK, and claim ranking "still keys by `worker_id` because the claiming
   worker is the unit of placement" (`prefix_routing.go:600-605`). No
   dtype, paged-vs-contiguous, rope scale, block size, or layer shape.

3. **Claim warmth JOIN does not include engine, runtime, or cell.**
   `warm_prefix_depth` joins `worker_prefix_state` to `job_prefix_chain` on
   `prefix_id` and `worker_id` only (`scheduler.go:789`). A multi-cell
   worker that served the prefix on llama.cpp can look warm for a later
   claim that freezes a different cell (`prefix_routing.go:57-59` already
   names this failure mode).

4. **`worker_model_state.model_id` is equally untyped.**
   Warm-for-task is `wms.model_id = j.model_ref` (`scheduler.go:764`).
   Candle GGUF and vLLM HF for names that collide as `model_ref` would both
   satisfy the EXISTS. Dispatch still freezes *this claiming worker's*
   `worker_authorized_capabilities` row onto the task, so pull-claim cannot
   *execute* on the wrong engine — but a future push "send this request to
   whoever is warm for model X" would treat those rows as interchangeable.

5. **Byte-size accounting assumes one layout.**
   `kvBytesPerToken = 16 * 2 * 8 * 64 * 2` "for Llama-3.2-1B"
   (`prefix_routing.go:372-374`) is applied to every `worker_prefix_state`
   row. vLLM paged KV, MLX, and Candle tensors are not that size; the
   budget eviction will mis-estimate all of them the same way.

6. **Realtime is tighter, still not a KV layout.**
   Offers must match `runtime_profile_id` **and** `runtime_profile_sha256`
   (`realtimeAuthorizeCandidatesCTE` WHERE). That binds tokenizer revision,
   engine, device, and `capabilities.prefix_cache` *as a flag*
   (`capability_manifest.go:182-216`). It does not bind KV block size,
   prefix-cache implementation, or tensor-parallel shard layout. Two vLLM
   workers on the same profile digest are treated as interchangeable
   sequence capacity. That is correct for *routing a new request to a
   process that already loaded the model*; it would be wrong as a license
   to copy paged KV between those processes.

7. **Merc does not currently transport KV.**
   Candle `restore_kv_cache` is in-process
   (`quantized_llama_batched.rs:604`). There is no control-plane "move KV
   from worker A to worker B" API. The trap is therefore latent for today's
   pull/push routing (local reuse only) and load-bearing the moment residency
   is treated as a *movable* economic object.

What *does* stop some wrong mixes, but not this one:

- `EngineAdmissibleFor` / CUDA vs Apple (`types.go:64-86`) at enrollment.
- Task freeze of `runtime_cell_id` + `runtime_id` + `model_kind` +
  `runtime_matrix_sha256` at claim.
- Realtime profile digest match.
- Prefix comment: a forged `prefix_id` "can only cost a cache miss"
  (`prefix_routing.go:40-42`) — safe for *routing hints*, silent if someone
  later treats `prefix_id` as a transferable KV key.

Nothing stores a `kv_compat_digest`. Nothing compares two workers' KV
representations. The schema will not refuse a row that says worker A (Candle
f16) and worker B (vLLM paged KV) both hold `pfx_<same>`.

---

## 6. Smallest schema addition that would unlock warm-preferring placement

Merc can already *say* "this worker has model M warm" and "this worker is
believed to hold prefix P". The primary batch claim path still will not let
a slower warm worker beat a faster cold one, and that gap is **ranking**,
not missing columns (`load_ms` is already on `worker_model_state` and is
unused in `ORDER BY`).

What the schema cannot say — and what the KV trap shows would be unsound to
imply — is *which compatible resident object* is warm. The smallest addition
that unlocks *trustworthy* warm-preferring placement (including a future
"slower warm beats faster cold" comparator that is allowed to look at
prefix/KV, not just weights) is one compatibility-bound residency key on
the existing warmth tables, not a third overloaded boolean.

Concretely: replace the prefix PK `(worker_id, prefix_id)` and the model PK
`(worker_id, model_id)` with

```
(worker_id, kind, artifact_id, compat_digest)
```

or, equivalently, add `compat_digest TEXT NOT NULL` (sha256 of
`runtime_id \x1f cell_id \x1f model_kind \x1f engine \x1f kv_layout_id`)
to `worker_prefix_state` **and** `worker_model_state`, put it in the primary
key, and require claim/authorize/lease JOINs to match it against the frozen
cell. `kind` is what later lets shaders, CUDA graphs, Metal pipelines, BVH,
and containers share the same freshness/confidence/provenance columns
without pretending they are `model_id`s.

Until that digest exists, ranking can prefer warmth only as "this worker
probably will not pay a load/prefill on *its own* engine" — which is all
today's pull claim and local KV reuse actually need — and must not treat
two warm rows for the same `model_ref` / `prefix_id` as interchangeable
economic inventory.

---

## 7. VERIFY: `python3 scripts/validate-readiness.py`

Run against this sparse worktree (only listed roots materialized). Direct
invocation:

```
$ python3 scripts/validate-readiness.py
Traceback (most recent call last):
  File ".../scripts/validate-readiness.py", line 45, in <module>
    from lib.receipt_binding import bound_to, head_commit, receipt_commit  # noqa: E402
ModuleNotFoundError: No module named 'lib'
```

`scripts/lib/receipt_binding.py` and `ops/readiness.json` are in git at HEAD
and are not on disk (`ls ops` / `ls scripts/lib` → No such file or
directory). This sandbox forbids `git sparse-checkout add`.

Second run, with `scripts/lib/{__init__,receipt_binding}.py` extracted to
`/tmp/hyper-h-val/lib` and `PYTHONPATH=/tmp/hyper-h-val` (no repo writes):

```
$ PYTHONPATH=/tmp/hyper-h-val python3 scripts/validate-readiness.py
readiness: candidate 4b10b56e0c491c2e9fdb652344d07020c78cd43f (ops/candidate.json absent; falling back to HEAD)
readiness: code-drift OK (no code changes since candidate)
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '.../ops/readiness.json'
```

Both outputs are from the live tree at `4b10b56e`. The audit itself is
`evidence/hyper-residency-audit.md` only.

---

## 8. Acceptance checklist

- [x] Current-state inventory with exact types/fields (§1)
- [x] Representable / partial / absent per residency kind (§2)
- [x] Quoted decision sites with yes/no on warm preference (§3)
- [x] Trustworthy-record field list with has-a-home verdict (§4)
- [x] Direct answer on the KV-compatibility trap (§5)
- [x] Every claim quotes real schema or real code
- [x] Single write path: this file
