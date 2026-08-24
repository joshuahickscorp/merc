# Where would Merc's p99 actually come from?

Walked against `HEAD` `4b10b56e0c491c2e9fdb652344d07020c78cd43f` (2026-08-23).
Read-only archaeology. Control-plane sources were read via `git show HEAD:<path>`
(this worktree is a sparse checkout; a missing on-disk file is not absence).
No production traffic was generated. No destructive commands.

This is not a generic distributed-systems tail list. A source is **real** only
if current code can wait there. A number is copied from a persisted
`evidence/perf/*` receipt or from a named constant in source. Everything else
is **UNKNOWN** plus the measurement that would settle it. Lab percentiles
(n≈40–80, single host, often concurrency fixtures) are not production SLOs.

A prior census exists at `evidence/perf/latency-atlas.json` (generated from an
older HEAD). This document re-walks the live request path at this commit and
adds the question a future router actually needs: **which tails can Merc see
before it picks a worker, and which can it only observe after the request has
already suffered them.**

Three products share a control plane and must not be collapsed:

| Lane | Buyer entry | Worker chosen by | Queue before a worker runs |
|---|---|---|---|
| Realtime chat | `POST /v1/chat/completions` | `AuthorizeRealtimeContract` (verified-outcome **cost**, warmth tiebreak) | Arrival join window, **default OFF** |
| Batch jobs | job submit → `tasks` row | `ClaimTasksTx` (`FOR UPDATE SKIP LOCKED`) | **Ask-deferral 20 s** + worker long-poll |
| Service leases | `POST /v1/service-leases` | Ordered `FOR UPDATE` walk of READY offers | **Already filters on measured p99** |

Merc already predicts a worker p99 **before routing** on the service-lease
path. Ordinary realtime ranking does not. That gap is the point of this lane.

---

## 1. End-to-end wait map (places a request can sit)

### 1.1 Realtime chat (`handleChatCompletions`)

Order in `control/realtime.go` after buyer auth:

1. `read_body` / `prepare_json` — in-process.
2. `OperationalControlPaused(controlIntake)` — one DB read.
3. Exact reuse (non-stream only) — miss continues; hit returns.
4. In-flight coalescing (non-stream only) — follower **waits the leader**.
5. `AuthorizeRealtimeContract` — funding lock + offer claim + durable insert.
6. Arrival batcher join — **wired, default disabled**.
7. Upstream dial / TTFB / first SSE — engine time.
8. Settlement intent (stream) overlapping the forward.
9. Finalize on the way out (not on TTFT except the intent drain).

Always-on phase capture (not the env-gated path-timing log) writes
`QueueWaitMS`, `ProviderStartupMS`, `EngineToFirstEventMS` onto execution
evidence. Prefill is deliberately nil: OpenAI-compatible SSE has no
prefill/first-token boundary.

```go
// control/realtime.go:759-784
// realtimeTTFTPhaseCapture is always-on wall-clock capture for the G021 TTFT
// split. Unlike realtimePathTiming it is not gated by an env flag: every live
// execution that reaches finalize writes what it observed.
//
// Boundaries (post-authorize):
//
//	queue_wait           started → dial start (arrival batch + pre_upstream)
//	provider_startup     dial start → response headers (connect + engine accept)
//	engine_to_first_event headers → first upstream SSE/event body
//	prefill              NEVER populated — OpenAI-compatible streaming has no
//	                     prefill/first-token boundary on the wire
```

### 1.2 Batch job (submit → claim → agent → verify → settle)

1. Quote / submit / funding (`buyer_prepaid_balances FOR UPDATE` on prepaid).
2. Task sits `queued` until a worker's `ClaimTasksTx` succeeds.
3. **Ask-deferral**: an expensive-asking worker is refused the row for
   `askDeferralWindow` while a cheaper-asking capable worker is online, unless
   SLA slack cannot absorb the wait.
4. Worker long-poll: empty claims every 5 s up to 25 s.
5. Agent: thermal/memory/schedule/CPU gates **before** poll; then object GET
   (3 retries), model load on pool miss, infer under a per-model mutex
   (Candle), object PUT (3 retries), commit.
6. Verification processor: `pg_try_advisory_lock` on `(job, chunk)` held
   **across object-store seal/read**.
7. Settlement / SLA / optional Stripe charge.

Cold-load stragglers are allowed **six minutes** before the no-peer watchdog
will even consider them wedged:

```go
// control/latency_watchdog.go:13-16
const (
	noPeerWatchdogInterval = 30 * time.Second
	noPeerWatchdogAfter    = 5 * time.Minute
	coldModelLoadAllowance = 6 * time.Minute
)
```

### 1.3 Service lease (the path that already talks p99)

`CreateServiceLease` locks every matching READY offer `FOR UPDATE` in
deterministic ask order, then refuses any offer whose measured p99 (when the
buyer set a bound) does not clear the lease SLO. Heartbeats re-enforce it.

```sql
-- control/service_leases.go:773-785
SELECT worker_id,supplier_id,supplier_nanos_per_replica_hour,
       residency_nanos_per_replica_hour,available_warm_replicas
  FROM service_lease_worker_offers
 WHERE ... AND status='READY'
   AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
   AND latency_window_seconds BETWEEN 1 AND 300
   AND latency_measurement_kind='DATA_PLANE_COMPLETIONS_V1'
   AND p95_latency_milliseconds <= $5
   AND ($7 = 0 OR (p99_latency_milliseconds > 0 AND p99_latency_milliseconds <= $7))
   AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
 ORDER BY (supplier_nanos_per_replica_hour + residency_nanos_per_replica_hour) ASC, ...
 FOR UPDATE
```

Ordinary realtime offers have warmth and `available_sequences`. They do **not**
carry a data-plane p99. Ranking is verified-outcome **cost**, then warmth
inside a cost class (`control/realtime_store.go:1107-1111`).

---

## 2. Confirmed-real tail sources

Columns: **frequency** = how often the wait can fire; **added latency** =
what it adds when it does; **predictable** = can Merc know *before* this
request is routed; **mitigation cost**. UNKNOWN means no honest number at
this HEAD — the settling measurement is named, not guessed.

### T1. Per-buyer advisory lock + `buyers` `FOR UPDATE` (money path)

**Real.** Legacy (non-envelope) `AuthorizeRealtimeContract` takes
`pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID)` then
`SELECT … FROM buyers … FOR UPDATE` **before** the offer claim, and holds
both until COMMIT of the EXECUTING insert. Same-buyer concurrent admits
enter the check-and-reserve section one at a time. Envelope spends skip
this lock (atomic envelope `UPDATE`).

```go
// control/realtime_store.go:32-38, 75-98
// Realtime money/capacity lock hierarchy (every path that touches more than
// one of these must acquire in this order; reverse order is a deadlock):
//
//  1. Buyer funding: pg_advisory_xact_lock("realtime-buyer-funding|"+buyerID)
//     then buyers.row FOR UPDATE (see evaluateRealtimeBuyerFunding).
//  2. Offer capacity: UPDATE realtime_worker_offers ...
//
// Serialization is two-step on purpose: FOR UPDATE on the buyer row alone is
// not enough under READ COMMITTED when the reservation itself is written to a
// different table (execution_contracts). Concurrent authorizers that only
// locked buyers could each observe realtimeReserved=0, all pass, then each
// insert an EXECUTING row — overspending a prepaid balance that funds a single
// ceiling.
batch.Queue(`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
	"realtime-buyer-funding|"+buyerID.String())
```

Taking the offer **before** funding was tried (`a8159ac7`) and deadlocked
against `FinalizeRealtimeSuccess` (PostgreSQL `40P01`). Hierarchy restored:
funding before offer. That history is why this is not a leftover.

| | |
|---|---|
| Frequency | Every live legacy authorize for that buyer while another same-buyer authorize/settle holds the lock. Envelope path: does not fire. Production mix UNKNOWN — count `AuthorizeRealtimeContract` with `EnvelopeID == uuid.Nil` vs envelope, plus in-flight same-buyer concurrency. |
| Added latency | **MEASURED** (lab, `evidence/perf/authorize-auth-tails-latest.json`, BOUND, 2026-08-03T17:47:11Z, n=80/cell, does **not** prove production or engine TTFT). Same-buyer 1-offer, `max_conns=20`: c=1 p99 **2.87 ms**; c=8 p99 **56.1 ms**; c=32 p99 **120.1 ms** (p50 56.5, p95 119.2). `max_conns=64` c=32 same-buyer p99 **228.5 ms**. Quiet c=1 is ~1 ms — the tail **is** the serialization. Envelope path at c=32 p50 **3.21 ms** / p95 **33.3 ms** (`evidence/perf/hot-path-free-admit-latest.json`) because the advisory lock leaves the path; remaining c=32 p95 is offer-row serialization on a one-offer book. |
| Predictable before routing | Partially. Known: this buyer already has in-flight EXECUTING contracts (open exposure is computed under the lock). Unknown until the wait: how long the *other* transaction will hold through ranking SQL + insert + commit. |
| Mitigation cost | Envelope-default already avoids the lock (productized). Short-reserve + second TX was measured ~1.33–1.37× same-buyer p95 at c=32 and **died on risk/reward** after a 40P01 (`docs/RUNTIME_AND_PERF.md`). Reversing lock order is disqualified. O(1) `committed_micros` would cut snapshot SQL still **inside** the lock; it does not remove the lock. |

**Ruling:** inherent cost of money correctness on the legacy prepaid path.
Not a bug. Envelope is the product mitigation already in tree.

### T2. Ask-deferral window (`cheaper_ask_online`)

**Real.** Batch claim, not realtime. A 20-second hold so `min_payout_usd_hr`
competes instead of only gating. `cheaper_class_online` is **ORDER BY only**;
`cheaper_ask_online` is a **hard deferral** (claim-narrowing ladder:
`claimStageSoftDeferral`).

```go
// control/scheduler.go:190-193
// askDeferralWindow is how long a task waits for a cheaper-asking worker before
// any eligible worker may claim it.  Short enough that a quiet fleet barely
// notices, long enough that a polling worker gets a turn.
var askDeferralWindow = 20 * time.Second
```

```sql
-- control/scheduler.go:979-1014 (predicate on claimable tasks)
-- First refusal for the cheaper ask. ORDER BY alone cannot express this:
-- ... with a single queued task an expensive worker claims it regardless
-- of who else is online.
AND (NOT ej.cheaper_ask_online
     OR COALESCE(t.visible_at, t.created_at) <= now() - $6::interval
     OR (
       COALESCE(ej.sla_guarantee_secs, 0) > 0
       AND (
         ej.eta_secs IS NULL
         OR (elapsed_since_job_created + window + eta) > sla_guarantee_secs
       )
     ))
```

Bounded on purpose: after 20 s anyone who passes the hard filter may claim,
so a cheap worker that advertises and never polls cannot starve the queue.
SLA-aware: fail closed on missing ETA (unknown is not zero). Tests:
`TestClaimTasksTxAskDeferralBoundStillHoldsWithoutSLA`.

| | |
|---|---|
| Frequency | Fires when a cheaper-asking, capable, unthrottled worker was seen within 60 s **and** the task is younger than 20 s **and** (no SLA, or SLA slack demonstrably fits). Production fire-rate UNKNOWN. Settling measurement: fraction of ready tasks with `cheaper_ask_online` and age < 20 s, from `MeasureClaimNarrowing` / `cheaper_ask_online_jobs` (already in the narrowing trace; off the hot path by default). |
| Added latency | **20 s** when it fires (named constant). Not a distribution. Buyer-visible queue age already exported as `merc_queue_age_seconds` p50/p95/p99 by tier×job_type (`control/metrics.go` + `observabilityQueueAge`) — that mixes deferral with ordinary backlog; it does **not** isolate the 20 s term. |
| Predictable before routing | **Yes.** Fleet state (`workers.last_seen_at`, `min_payout_usd_hr`, capabilities) is known at claim time. A router that can see `cheaper_ask_online` knows the expensive worker will be refused for up to 20 s. |
| Mitigation cost | Shortening the window is a market trade (cheap askers get less first look). Disabling it returns to “price list, not exchange” (comment at `scheduler.go:647-656`). SLA already skips the hold when the wait cannot fit. |

**Ruling:** product / market decision, not a bug, not a money-correctness
lock. Money still settles if the expensive worker claims immediately; the
window exists so asks compete.

### T3. Offer-row `FOR UPDATE` (thin realtime book)

**Real.** After funding, authorize claims a sequence with
`UPDATE realtime_worker_offers … WHERE available_sequences > 0`. Multi-offer
books use `FOR UPDATE SKIP LOCKED`; a **single-offer** book uses blocking
`FOR UPDATE` only (SKIP-then-block doubled the ranking CTE and made 1-offer
multi-buyer p95 worse — `realtime_store.go:1087-1091`).

| | |
|---|---|
| Frequency | Concurrent admits onto the same offer row. Thin books (one local agent, canary of two) make this the common lab shape. Production book depth UNKNOWN (`realtimeOfferBookBranchProbeSQL` LIMIT 2 already exists). |
| Added latency | **MEASURED** lab: multi-buyer 1-offer, max_conns=20 c=32 p95 **55.9 ms** p99 **76.8 ms**; N-offer SKIP LOCKED p95 **7.2 ms** p99 **8.1 ms** (same receipt as T1). Receipt synthesis: `MULTI_OFFER_HELPS`. Docs: do **not** treat 1-offer p95 as a standing defect when N-offer recovers (`docs/RUNTIME_AND_PERF.md`). |
| Predictable before routing | **Yes, as book shape.** `candidate_count` / probe > 1 is known before the blocking wait. Which *other* admit holds the row, and for how long, is not. |
| Mitigation cost | Grow supply (SKIP LOCKED already in tree). Splitting `available_sequences` into slot rows was explicitly rejected as a reaction to the thin-book fixture. |

### T4. Arrival-batch join window

**Real in code, default OFF in production construction.**

```go
// control/api.go:63-76
// Arrival batching is OFF by default, deliberately. The join window
// delays every interactive request by up to its class window (2 ms
// today) against a c=1 TTFT overhead budget of 15 ms — so it spends
// real latency that the parity gate measures. The throughput it buys
// back is currently INCONCLUSIVE_NULL: ... evidence/perf/arrival-batching.json
arrivalBatcher: NewArrivalBatcher(ArrivalBatchConfig{Enabled: false}),
```

Class windows (`control/traffic_class.go:231-250`): interactive **2 ms**,
batch-priority 25 ms, batch-standard 50 ms, background 100 ms, clamped by
`HoldWouldMissDeadline`.

| | |
|---|---|
| Frequency | Zero on the default server. Non-zero only if a caller constructs `Enabled: true`. |
| Added latency | Up to the class window when enabled; **0** on the default path. `evidence/perf/merc-latency-gap-accounting-latest.json` still records an `arrival_batch` mark of p99 **0.028 ms** at c=1 — that is the disabled bypass, not a join. |
| Predictable | Yes (config flag + class + deadline). |
| Mitigation cost | Keep off until a real continuous-batching engine measurement shows a positive trade (already the stated policy). |

**Ruling:** product decision, currently spending zero. Not the p99.

### T5. Worker long-poll empty claim loop

**Real.** `/v1/worker/poll?wait_ms=25000` (`agent/src/protocol.rs:25`).
Server re-runs full `ClaimTasksTx` every `longPollInterval = 5s` up to
`longPollCap = 25s` (`control/api.go:3141-3211`).

| | |
|---|---|
| Frequency | Every idle poll. Not on the buyer HTTP path; it delays **when a queued task starts**. |
| Added latency | Up to 5 s between a newly-visible task and the next claim attempt if `taskWake` does not fire. Cap 25 s per poll. SQL duration of one claim is a different term (T6). |
| Predictable | Yes as a bound (5 s / 25 s). Exact wait for a given job UNKNOWN without `taskWake` traces. |
| Mitigation cost | Wake channel already exists. Tightening the tick is extra `ClaimTasksTx` load (fleet-relative EXISTS is the expensive part). |

### T6. `ClaimTasksTx` SQL (fleet-relative EXISTS)

**Real.** Each claim locks `workers WHERE id=$1 FOR UPDATE`, then a large
eligible-jobs CTE (`cheaper_class_online` / `cheaper_ask_online` EXISTS over
live workers), then `FOR UPDATE OF t SKIP LOCKED LIMIT 1`. Histogram:
`merc_claim_duration_ms`.

| | |
|---|---|
| Frequency | Every poll, including empty long-poll ticks. |
| Added latency | Atlas (older HEAD, n small, c=1): claim p50 **2.4 ms** p95 **8.0 ms**, p99 UNMEASURED. Production p99 UNKNOWN — `merc_claim_duration_ms` is the settling histogram (buckets to 5 s). |
| Predictable | Partially (backlog size, fleet size). Not per-request before the SQL runs. |
| Mitigation cost | Already rewritten to per-job EXISTS (comment: 240 jobs vs 12k tasks). Further cuts are query work, not a lock policy. |

### T7. Cold model load (agent `ModelPool`)

**Real.** First `pool.embedder` / `pool.llama` for a ref does blocking weight
load and records `load_ms` + RSS. Heartbeat prefers `resident_models` and
persists `worker_model_state.load_ms`; legacy `loaded_models` only refreshes
`last_seen_warm` (NULL load — cannot authorize service-lease warmth).

```rust
// agent/src/pool.rs:90-105
.get_or_try_init(|| async {
    let e = tokio::task::spawn_blocking(move || {
        let rss_before = read_own_rss_bytes();
        let started = Instant::now();
        note_load();
        let loaded = Embedder::load(&model_ref);
        let load_ms = started.elapsed().as_millis() as u64;
        ...
        record_residency(&measure_key, rss_after as i64 - rss_before as i64, load_ms);
        loaded
    })
```

Claim SQL already prefers a warm worker as a **tiebreak inside a cost class**,
never as a hard filter (`warm_for_task`, 60 s). A cold expensive worker must
not outrank a cold cheap one.

| | |
|---|---|
| Frequency | First task after process start, after idle eviction, after explicit eviction, after a model the worker has never loaded. Idle eviction exists (`evict_idle`). Production cold-rate UNKNOWN. |
| Added latency | **UNKNOWN** in production. Watchdog treats running+cold as legitimate for **6 minutes**. Bench paths log `load_ms`; those benches are not this system's p99. Settling measurement: histogram of `worker_model_state.load_ms` by `(model_id, hw_class)` and the fraction of claims with `warm_for_task = false`. |
| Predictable before routing | **Yes, as a boolean.** `worker_model_state.last_seen_warm` within 60 s is on the claim row. Duration of *this* load is only predictable if `load_ms` was reported for that `(worker, model)` and the weights are still resident. |
| Mitigation cost | Keep models warm (memory). Pin via service-lease warmth. Do not override cost class with warmth (already the rule). |

### T8. GPU / engine contention

**Real, two mechanisms.**

1. Realtime capacity is `available_sequences` on the offer row. A full
   worker refuses or blocks at T3; the engine's own batching queue is
   **inside** TTFB (T12). vLLM adapter advertises exactly one measured warm
   replica (`agent/src/vllm.rs`).
2. In-process Candle embed takes a `Mutex` on the loaded backend
   (`runtime_driver.rs` `embedder.blocking_lock()`), so two embeds on the
   same agent serialize on the model, not the GPU scheduler.
3. Metal init panics if another process owns the GPU; the agent catches and
   **falls back to CPU** (`agent/src/models.rs:30-54`) — a correctness
   fallback that is also a latency cliff, not a wait-and-retry.

| | |
|---|---|
| Frequency | Whenever `available_sequences` is exhausted, or two Candle tasks share a pool entry. Production UNKNOWN. |
| Added latency | Sequence-full: T3 numbers if they wait on the row; otherwise `errRealtimeNoSupply` (not a tail, a 503). Mutex/engine queue: UNKNOWN (inside T12). CPU fallback after Metal panic: UNKNOWN, would look like a different hw_class. |
| Predictable | Sequence count and advertised warmth: yes. In-engine queue depth: no (not on the offer). CPU fallback: only after it has happened. |
| Mitigation cost | Advertise honest `available_sequences`. Don't dual-own Metal (already why llama.cpp is Attach-not-Spawn). |

### T9. Thermal / memory / schedule / CPU claim gates

**Real.** Before `poll_task`, the agent pauses 30 s (thermal/memory) or 60 s
(schedule/battery) or 5 s (CPU ceiling) and **does not claim**.

```rust
// agent/src/main.rs:3960-3967
cfg.refresh_thermal_pressure();
let thermal = cfg.evaluate_thermal_throttle();
ctx.status.set_thermal_throttle(&thermal);
if thermal.throttled {
    ...
    "thermal throttle: pausing new claims"
```

`thermal_throttle` is a retryable failure class (`control/failure.go`).

| | |
|---|---|
| Frequency | When `thermal_pressure.rank() >= thermal_limit` (default Serious/Critical). Production UNKNOWN — heartbeat `Throttled` + `thermal_state` are the signals. |
| Added latency | 30 s pause **of new claims**, not of an in-flight request. Mid-request slowdown from the OS/GPU: UNKNOWN (not a Merc wait point; would show up in T12 / T14). |
| Predictable | **Yes for new routing.** A throttled worker is `workers.throttled` / disappears from 60 s liveness and is excluded from `cheaper_ask_online`. In-flight tokens already on that GPU are not preempted by this gate. |
| Mitigation cost | Supplier sets a more conservative `thermal_limit`. Routing already avoids throttled workers. |

### T10. Prefix-cache miss (warmth belief vs physical KV)

**Real.** `warm_prefix_depth` is ORDER BY preference only (90 s TTL on
`worker_prefix_state`). Cost class still wins. Physical miss is engine
prefill.

| | |
|---|---|
| Frequency | Whenever claim prefers a worker whose engine cache does not actually hold this prefix (stale belief, eviction, salt). |
| Added latency | **MEASURED** on one Metal two-worker corpus (`evidence/perf/prefix-two-worker-latest.json`): warm-minus-cold prompt p50 **−409 ms**, p95 **−433 ms** (negative = warm faster). A forced miss was prompt_ms **436 ms** vs warm cached_tokens 2661. Does **not** prove cross-host, second class, or fleet p99. |
| Predictable | Belief (`warm_prefix_depth`) is predictable. Physical hit is not — Merc ages belief at 90 s and invalidates on observed miss. |
| Mitigation cost | Keep the ORDER BY discipline (already: warmth must not beat cost). Tighter TTL = more false cold (lost wins, not extra tails). |

### T11. Object-store GET/PUT + retries

**Real.** Agent `s3_get` / `s3_put_bytes`: `TRANSFER_RETRIES = 3`, base delay
250 ms doubling (`agent/src/main.rs:3277-3307`). Retryable: connect, timeout,
5xx, 429. Control-side transfers: `merc_transfer_duration_ms`. Verification
reads artifacts under T15.

| | |
|---|---|
| Frequency | Every batch task at least one GET + one PUT. Retry on transient. Realtime chat does **not** pull the buyer prompt through S3 (body is in the HTTP request). |
| Added latency | Happy path UNKNOWN (depends on object size + RTT). Worst designed retry: 250+500+1000 ms **sleep only**, plus four attempts of the transfer itself. Cap 512 MiB download. |
| Predictable | Direction and size class maybe; this attempt's RTT no. |
| Mitigation cost | Object locality / same-region. Don't add retries (already bounded). |

### T12. Upstream / engine TTFB and first SSE (realtime)

**Real.** After authorize, Merc dials `contract.UpstreamBaseURL`. This is
usually the **largest realtime buyer-visible term at c=1**.

| | |
|---|---|
| Frequency | Every live (non-reuse, non-coalesced) realtime request. |
| Added latency | **MEASURED** lab stub/parity, not production GPU: gap-accounting c=1 `upstream_ttfb` p50 **2.86 ms** p95 **3.02 ms**; `upstream_first_sse` p50 **3.03 ms** p95 **3.65 ms**. Merc-owned named sum c=1 p50 **2.99 ms** p95 **4.98 ms**. At c=32 the same receipt's engine_facing p95 **112 ms** and merc-owned p95 **109 ms** — the probe itself says engine-excess can be mis-attributed. **Production Metal/CUDA p99 UNKNOWN.** Settling measurement: already-on `ProviderStartupMS` + `EngineToFirstEventMS` histograms **by worker_id / runtime_profile**, which today sit on per-contract evidence and are not scraped as `merc_*` histograms. |
| Predictable | Warmth + sequence availability: weakly. Worker data-plane p99: **only on service leases** (`DATA_PLANE_COMPLETIONS_V1`, ≥5 samples, 1–300 s window). Ordinary realtime offers: **no**. |
| Mitigation cost | Copy the lease probe onto realtime offers if a router should avoid a cheap-p50 / terrible-p99 worker. That is the smallest honest product change; it is not free (probe load, five-sample floor, fail-closed when p99 missing). |

### T13. In-flight coalescing follower wait

**Real, non-stream only.** Leader lease TTL **30 s**, result hand-off **60 s**,
max 3 elections (`control/inflight_coalescing.go`). Followers poll every 25 ms.
A forgotten release used to wait the full lease — the handler now `defer`s
`ResolveInflightFailure`.

| | |
|---|---|
| Frequency | Identical concurrent non-stream requests from the same tenant. Stream excluded. Production UNKNOWN. |
| Added latency | Up to the leader's execution (the point of coalescing) or 30 s on a dead leader before independent execution. |
| Predictable | Same-identity in-flight row is visible at admit. Duration is the leader's remaining work — not known in advance. |
| Mitigation cost | Already the optimization. Bound is the TTL. |

### T14. Retry after failure (batch)

**Real.** `maxTaskRetries = 3` (`control/workers.go:61`). Retryable classes
include `oom`, `model_load_failed`, `thermal_throttle`, `timeout`,
`transient_io`, `object_store_error`. `immediateFailBackoff = 5s`. Stale
reaper: `staleTaskTimeout = 30m`, then `staleBackoff = 1m` before the row is
visible again. Hedge after **90 s**.

| | |
|---|---|
| Frequency | Per retryable fail. Production UNKNOWN (`task_failures`). |
| Added latency | Another full queue+load+run, plus 5 s backoff, plus possible 20 s ask-deferral again, plus up to 30 min if the worker holds a running claim without committing. |
| Predictable | Failure **rate** is already in realtime ranking (`realtime_supplier_outcome_stats`) and in verified-outcome cost. That *this* attempt will fail: no. |
| Mitigation cost | Ranking already penalizes measured failure/refund rates when sample count ≥ `minRealtimeOutcomeSamples`. |

### T15. Verification lock across object I/O (batch)

**Real.** `pg_try_advisory_lock` on `(job, chunk)` is held while the processor
seals/reads the staged artifact, then `VerifyJobTx`. Busy →
`ErrVerificationChunkBusy`. Commit HTTP can run `ProcessAttempt` synchronously.

```go
// control/verification_processor.go:630-642
if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
    conn.Release()
    return nil, err
}
if !locked {
    conn.Release()
    return nil, ErrVerificationChunkBusy
}
```

| | |
|---|---|
| Frequency | Every verifying chunk. Contention when two processors hit the same chunk. |
| Added latency | Object GET + hash/plan, serialized per chunk. UNKNOWN (no phase clock). Atlas flagged this as lock-across-network **by design**. |
| Predictable | Not as a per-job duration. Existence of the lock: yes. |
| Mitigation cost | Do not "fix" by releasing across I/O without re-reading the contract (comment in the atlas: bounded by design). Async process-after-commit already exists as a drain; making commit always async is a product latency vs "verify before 200" trade. |

### T16. Exact-reuse / API-key cache miss

**Real as a fork, not a long wait.** Stream skips exact reuse (SSE contract).
API-key cache TTL **250 ms**, negatives uncached, admin keys uncached
(`control/api_key_cache.go`). Cold lookup lab p99 **0.15–0.42 ms** (T1
receipt). Auth lookup at c=32 in gap-accounting p95 **49.9 ms** — that cell
is a **measurement/load** term, not the 250 ms TTL.

| | |
|---|---|
| Frequency | Every request. Stream: reuse skipped. Cache hit: skip live authorize+engine. |
| Added latency | Miss: live path (T1+T12). Hit: bill-and-serve, UNMEASURED as its own mark (`latency-atlas` lists exact_reuse UNMEASURED). |
| Predictable | Request identity known; whether a result row exists is a lookup, not a prediction. |
| Mitigation cost | Cache is already there. Don't lengthen TTL (revocation window). |

### T17. Rate limiter (buyer 20 rps / burst 40)

**Real.** `buyerLimiter: newRateLimiter(20, 40)` (`control/api.go:58`). Deny
is 429, not a wait. Not a tail of successful requests.

| | |
|---|---|
| Frequency | Excess of 20 rps per API key. |
| Added latency | None on allow; rejection on deny. |
| Predictable | Yes. |
| Mitigation cost | Raise the bucket (product). |

### T18. Hedge / no-peer / stale claim (batch straggler escape)

**Real.** `hedgeAfter = 90s`, `noPeerWatchdogAfter = 5m` (skipping cold-load
6 min), `staleTaskTimeout = 30m`. These are **how Merc notices** a bad tail,
not the tail itself.

| | |
|---|---|
| Frequency | Tasks still running past the bound with a live heartbeat. |
| Added latency | The bound **is** the added wait before escape. |
| Predictable | The constants are known. Whether this task will need them: no. |
| Mitigation cost | Tightening 30 min stale is operational risk (false requeue of long jobs). |

### T19. Durable contract insert + commit (realtime floor)

**Real.** After locks, authorize inserts `execution_contracts` EXECUTING +
event and commits. Hot-path probe decomp (c=1 p50): envelope spend 0.41 ms,
direct offer 0.22 ms, contract+event 0.39 ms, commit 0.15 ms, total **1.30 ms**.
Zero-durable-before-dispatch **does not survive** (overspend + supplier
liability). This is the floor, not the p99.

---

## 3. Ruling: advisory lock + `FOR UPDATE`

**Inherent cost of money correctness** on the legacy prepaid realtime path.

Argument from code, not taste:

1. The comment at `evaluateRealtimeBuyerFunding` states the READ COMMITTED
   hole: buyer-row `FOR UPDATE` alone does not serialize inserts into
   `execution_contracts`, so two authorizers can both see `reserved=0` and
   both insert EXECUTING — overspending one ceiling.
2. The advisory xact lock is keyed on the buyer and held **through the
   EXECUTING insert and commit**. That is the critical section.
3. Lock order is funding → offer because the reverse produced `40P01`
   against settlement (`FinalizeRealtimeSuccess` locks buyer then offer).
   Restoring hierarchy was a deadlock fix, not a performance regression to
   unwind.
4. Envelope path already removes the advisory lock (atomic residual
   `UPDATE`). The remaining c=32 envelope p95 is offer-row, not funding.

It is not a bug (the serialization is the spec). It is not a product delay
like ask-deferral (removing it without another atomic hold is an overspend).

---

## 4. Ruling: ask-deferral window

**Product / market decision.**

Argument from code:

1. `ORDER BY cheaper_ask_online` cannot refuse a lone queued task; an
   expensive worker would claim it. The WHERE hold is the mechanism that
   makes `min_payout_usd_hr` compete (`scheduler.go:979-986`).
2. The 20 s bound plus 60 s rival-liveness plus SLA slack-gate are explicit
   product knobs, tested (`scheduler_sla_deferral_test.go`,
   `scheduler_ask_claim_integration_test.go`).
3. `cheaper_class_online` was deliberately left as ORDER BY only after a
   prior money bug (rented NVIDIA ranked cheaper than owned Macs). Ask
   deferral is the one economic term that **blocks**, not merely reorders.
4. No ledger invariant requires the wait. An expensive claim is still
   billed at the job's offer. The window is allocation policy.

Not a bug. Not money correctness.

---

## 5. Predictable before routing vs observable only after

A future router can act only on the first column.

### Predictable before this request is dispatched

| Signal | Where it lives | What it lets you avoid |
|---|---|---|
| `worker_model_state.last_seen_warm` (60 s) + `load_ms` | Heartbeat / claim SQL `warm_for_task` | Cold-load cliff (boolean; duration if previously measured) |
| `worker_prefix_state` depth (90 s) | Claim `warm_prefix_depth` | Prefix miss **belief** (not physical hit) |
| `workers.throttled` / thermal pause / 60 s liveness | Heartbeat, `cheaper_ask_online` predicates | Routing to a worker that will not poll |
| `available_sequences` / `available_warm_replicas` | Offer rows | Thin-book serialize / no-supply |
| Book size > 1 | `realtimeOfferBookBranchProbeSQL` | Blocking 1-offer `FOR UPDATE` |
| `min_payout_usd_hr` vs job offer + rival online | `cheaper_ask_online` | Paying the 20 s hold on an expensive worker |
| `hw_class` cost rank | `cheaper_class_online` (order only) | Tying an expensive class to cheap work |
| `worker_tps_cache` | Claim ORDER BY | Picking a slow-throughput worker inside a class |
| Verified-outcome fail/refund rates | `realtime_supplier_outcome_stats` | Cheap p50 / failing p99 **cost**, not latency |
| **Lease p95/p99** (`DATA_PLANE_COMPLETIONS_V1`, n≥5, 1–300 s) | `service_lease_worker_offers` | **The one existing latency-p99-before-route path** |
| Envelope vs legacy funding | Request header | Same-buyer lock (legacy only) |
| Arrival batcher enabled | Server config | 2–100 ms join (currently off) |
| Same-buyer in-flight EXECUTING count | Open exposure SQL | That this admit **will** queue on T1; not how long |

### Observable only after the request has waited

| Signal | Why it cannot be known at route time |
|---|---|
| This admit's wait on `pg_advisory_xact_lock` / offer `FOR UPDATE` | Depends on the *other* transaction's remaining SQL+commit |
| Engine TTFB / first token / in-batch queue | Not on ordinary realtime offers; SSE has no prefill mark |
| This object GET/PUT RTT and retries | Network + store |
| Physical prefix hit vs 90 s belief | Engine eviction is not a Merc clock |
| Mid-request thermal throttling of tokens/s | OS/GPU; agent gate only stops **new** claims |
| Candle `Mutex` wait behind another embed | In-process, not advertised |
| Verification object I/O under chunk advisory lock | After commit |
| Go STW / allocator / page cache | No Merc sensor |
| That this attempt will be retried | Failure classes fire in the agent |
| Coalesce follower's remaining wait | Leader's remaining work |

**Implication for a p99-aware router:** copy the service-lease measurement
kind onto realtime offers (and, if batch dispatch should avoid a
high-p99 worker, onto worker heartbeats as a **preference below cost
class**, same discipline as warmth). Do not invent a p99 from p50. Do not
route on `MERC_REALTIME_PATH_TIMING` logs.

---

## 6. Ruled out as not applicable here

Generic tail sources that are **not** wait points in this tree, with why.
(If a mechanism exists but is not a *designed wait*, it is listed as
uninstrumented noise, not as a Merc source.)

| Candidate | Why not a Merc p99 source here |
|---|---|
| Kafka / Redis / extra queue broker | `README.md`: tasks are claimed from Postgres with `FOR UPDATE SKIP LOCKED`. No Kafka or Redis. |
| Straggler **shard** | Single primary Postgres. No request-path sharding, no scatter-gather of buyer work across shards. (Chunked jobs are tasks, not shards; their straggler is T18.) |
| Runtime **compile** (CUDA graphs / shader JIT as a Merc stage) | No control/agent stage waits on a compile. Metal device init is once-per-process. Model load is weight load (T7). Any engine-internal compile is folded into T12 and is not named. |
| Language GC as a designed wait | Nothing in control or agent waits on GC. Go STW can exist; it is not a routing signal and is not measured. Do not put it on a router. |
| Custom allocator wait | `load_ms` + RSS residency are recorded; there is no allocator lock in the request path. |
| Arrival-join as today's p99 | Default `Enabled: false`. |
| GPU thermal **throttling of an in-flight kernel** as a Merc wait | Agent thermal policy **pauses claims** (T9). It does not insert a wait into an in-flight generate. OS thermal of a running kernel is T12/T14, uninstrumented. |
| Render Cycles GPU contention on the **verify** blender path | `render/verify/blender_service.py` pins `cycles.device = "CPU"` and refuses otherwise. Metal GPU render exists in `render/metal/blender_entry.py` as a harness/entry, not the verify service. |
| Cross-region DNS / Anycast hunting | Not in this codebase's request path. Buyer→control and control→upstream are ordinary HTTP. |
| Multi-tenant noisy-neighbor **disk scheduler** | No code. Would show up as T11 if at all. |
| Exact-reuse as a *wait* | Miss falls through; hit returns. Not a queue. |

---

## 7. Smallest honest instrumentation

Do **not** turn on `MERC_REALTIME_PATH_TIMING=1` in production (one structured
log line per request, residual accounting for lab gap-closure).

Already on, keep:

- `realtimeTTFTPhaseCapture` → evidence `QueueWaitMS` / `ProviderStartupMS` /
  `EngineToFirstEventMS` (prefill stays nil).
- `merc_claim_duration_ms`, `merc_http_request_duration_ms`,
  `merc_transfer_duration_ms`, `merc_task_duration_ms`.
- `merc_queue_age_seconds` p50/p95/p99.
- `merc_latency_phase_ms` queue/dispatch/run **p50/p90** from completed
  tasks (`control/store.go:1860-1879`).
- `worker_model_state.load_ms` when `resident_models` is sent.
- Service-lease `SLO_MEASURED` events with p95/p99.

Add **four** low-cardinality series. No per-request logs. No new broker.

1. **Promote TTFT phases to Prometheus histograms** labelled
   `runtime_profile_id`, `hw_class` (not `worker_id` — cardinality). Source:
   the same ints already written on finalize. This is how a router would
   *observe* worker p99 after the fact, and how one would *validate* a
   pre-route probe.
2. **`merc_authorize_same_buyer_wait`** — optional, sampled: time from
   `pg_advisory_xact_lock` call to lock grant (Postgres `pg_locks` /
   wait-event on the funding key), labelled `path=legacy|envelope`. Sample
   1% or first N per minute. The tail probe already showed this is the
   c=32 legacy p99; production still has no series.
3. **`merc_ask_deferral_holds_total`** — increment when claim SQL would
   have taken a task except `cheaper_ask_online AND age < window AND slack
   ok`. Cheap: a counter next to existing `claimDuration.observe`. Settles
   T2 frequency.
4. **p99 on `merc_latency_phase_ms`** — the SQL already reads completed
   tasks; add `percentile_disc(0.99)` next to 0.5/0.9. Three extra aggregates
   per `job_type` scrape.

**Do not add** a second path-timing log, EXPLAIN on the hot path, or
`claimNarrowingMeasureOnHotPath` in production (it re-evaluates fleet
EXISTS and is off for that reason).

**Service-lease probe as the template for realtime:** if Merc should refuse
a spectacular-p50 / terrible-p99 worker *before* `AuthorizeRealtimeContract`
picks it, the measurement already specified is `DATA_PLANE_COMPLETIONS_V1`
(≥5 samples, 1–300 s window, receipt digest). Putting that column on
`realtime_worker_offers` and ranking **below cost class** (same as warmth)
is the smallest product-shaped change. This report does not make that
change.

### Cheap harness (no new binary, nothing destructive)

Existing opt-in tests already isolate T1/T3 vs pool vs lookup. They write
bound receipts; they need a local Postgres. They do not charge Stripe or
touch live workers:

```bash
# Same-buyer funding lock vs offer-row vs pool (T1, T3)
MERC_AUTHORIZE_TAIL_PROBE=1 \
MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
go test -count=1 -run TestAuthorizeTailCharacterize -timeout 45m ./control

# Per-segment path timing against a stub upstream (not engine p99)
MERC_SEGMENT_LATENCY_MEASURE=1 \
MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
go test -count=1 -run TestMercSegmentLatencyMeasure -timeout 30m ./control
```

Ask-deferral is a constant plus a predicate; its tests do not need a soak
(`TestClaimTasksTxAskDeferralBoundStillHoldsWithoutSLA`). Production
fire-rate wants the counter in (3), not another lab sweep.

No new harness file: write scope is this path only, and the isolation
already exists.

---

## 8. What a Merc p99 would actually be made of

Depends which p99 the router means.

**Buyer realtime TTFT p99 (chat).** At low concurrency the lab says Merc-owned
is ~3–5 ms and the engine is the rest; production engine p99 is UNKNOWN.
At same-buyer concurrency 32 the **legacy funding lock** dominates Merc-owned
(~120–228 ms p99 in the bound tail receipt) unless the request is on an
envelope. Thin books add offer-row waits of tens of ms; SKIP LOCKED on N
equal offers drops that to ~8 ms p99 in the same fixture. Arrival join is
off. Prefill is invisible on the wire.

**Buyer batch time-to-first-claim p99.** The 20 s ask-deferral is larger
than every measured control-plane lock by construction when it fires.
Long-poll adds up to 5 s of detection delay. Ordinary claim SQL is
milliseconds. Production mix of "held for cheaper ask" vs "held for no
workers" is UNKNOWN; `merc_queue_age_seconds` p99 is the current proxy.

**Worker execution p99 (the routing question).** Cold load (UNKNOWN, allowed
for 6 minutes), prefix miss (~0.4 s on one Metal corpus), object-store
retries, Candle mutex, engine queueing, then verification I/O on batch.
Merc can **see** warmth, ask, class, sequences, throttle, and (leases only)
measured p99 **before** pick. It cannot see this attempt's engine queue or
this GET's RTT. Ordinary realtime ranking still optimizes **verified-outcome
cost**, not latency p99 — so a worker with a beautiful p50 and a rotten p99
can still win if its ask/fail-rate looks cheap.

Largest single **designed** tail term: **ask-deferral, 20 s** (batch).
Largest single **suspected worker-execution** tail term: **cold model load**
(UNKNOWN; watchdog 6 min). Largest **measured Merc-owned realtime** tail
term at c=32: **same-buyer funding lock** (p99 120 ms at max_conns=20 in
`authorize-auth-tails-latest.json`).

---

## 9. Verify

Command run in this worktree (sparse checkout: `scripts/lib/` is not on
disk; `PYTHONPATH` pointed at the same git blob of `scripts/lib` from the
sibling checkout of `HEAD`, so the import resolved without
`git sparse-checkout add`):

```
PYTHONPATH="/Users/scammermike/Downloads/merc/scripts" python3 scripts/validate-readiness.py
```

Real output:

```
readiness: candidate 4b10b56e0c491c2e9fdb652344d07020c78cd43f (ops/candidate.json absent; falling back to HEAD)
readiness: code-drift OK (no code changes since candidate)
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '/Users/scammermike/.claude-grok/worktrees/HYPER-M-tail-20260823-234315/ops/readiness.json'
```

`code-drift OK` is the proof this tree has no tracked edits. The FAIL is
sparse-checkout: `ops/readiness.json` (and the rest of `ops/`) is in git at
this HEAD but not materialized. Bare `python3 scripts/validate-readiness.py`
without `PYTHONPATH` fails earlier:

```
ModuleNotFoundError: No module named 'lib'
```

because `scripts/lib/receipt_binding.py` is also not on disk. Neither path
was added to the sparse checkout (the sandbox refuses `git sparse-checkout add`).
This report does not depend on a passing 100-point score.
