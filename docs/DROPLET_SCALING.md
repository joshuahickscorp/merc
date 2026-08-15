# Droplet scaling & expansion plan (G074b / G081)

**Question this answers:** is the DigitalOcean droplet "rated for 10M", and when do we
expand — to what, and why. All numbers below are **measured**, not asserted; every one
traces to an evidence artifact and carries its honesty caveat.

---

## The two scaling axes (they scale on different signals)

1. **Control plane** — the droplet (a small box). Scales with **device count**: liveness
   heartbeats + selection queries. This is the curve in this document.
2. **GPU supply** — RunPod / owned GPUs, **separate hardware**. Scales with **job demand**,
   gated by economics (a lane only runs when its price covers its measured cost). Nothing
   here is about GPU supply; don't conflate the two. Adding devices does not add GPU cost;
   adding *jobs* does.

"10M" means **10M devices**, not users, and it is a **measured curve with a named
saturation point**, never a proven headline.

---

## Measured control-plane device-ceiling (one droplet-class box)

Source: [`evidence/perf/droplet-device-ceiling.json`](../evidence/perf/droplet-device-ceiling.json)
(schema v3). **PROXY**, and an **UPPER bound**: `docker --cpus=1 --memory=961m postgres:17`
with the smallhost knobs, on an Apple **M3 ARM** host — *not* a DigitalOcean x86 vCPU — and
only Postgres is cgroup-limited; the bench client ran unconstrained. A real 1 vCPU / 961 MB
droplet also shares that core and RAM with control + caddy + minio, so the true droplet
number is **proportionally lower**.

| liveness path | sustained hb/s | devices alive `⌊hb/s×45s⌋` | peak devices | selector eligibility p95 |
|---|---|---|---|---|
| baseline (1 UPDATE + 1 INSERT / device) | ~1,980 | **~89,107** | ~133,185 | ~116 ms |
| coalesced **2 ms** flush | ~4,090 | **~184,032** | ~232,992 | ~60 ms |
| coalesced 50 ms flush | ~263 | ~11,820 | — | ~22 ms |

- **2 ms flush is the sweet spot** (~2× baseline). The 50 ms flush is *worse* — the flush
  timer, not the DB, becomes the bottleneck. Batching wins by amortizing WAL/fsync, not by
  waiting.
- **Saturating resource: WAL + fsync on the single vCPU** (~20–23 MB/s WAL at the 2 ms path).
  Not CPU-compute, not locks — durability I/O.
- **Footprint: ~2,185 bytes/device** (offers+workers relations at seed), ~491k devices/GB.
- **Eviction-window invariant holds under batching** (directive IV, proven in G073):
  `last_seen_at` is the device's clamped observation time, never flush-time `now()`; stale
  (>45 s) observations are rejected, not floor-stamped. A dead device leaves the eligible set
  within 45 s regardless of coalescing.
- **Selector ranking is the next superlinear term** at large fleets: the LIMIT-2 branch probe
  stays sub-ms (p50 ~0.2 ms), but the full eligibility scan grows with concurrency
  (p50 ~16 ms → ~64 ms). Walking all eligible rows is what goes non-linear before compute does.

### Why one droplet is not "rated for 10M"

10M × 2,185 B ≈ **21.8 GB** of offer+worker working set. That does not fit in 961 MB. So:

- **10M breaks first on the working set (RAM)**, then on single-vCPU WAL.
- At the sustained coalesced rate, **10M ⇒ ≥ 55 droplet-class boxes** — a host-count *lower
  bound*, valid only while each box's working set fits RAM, WAL isn't already saturated, and
  the selector stays linear. In practice 10M needs **a bigger box + managed Postgres**, not 55
  tiny droplets.

---

## The lever that actually reaches 10M: the live-index FLIP (G082)

Source: [`evidence/perf/liveness-index-bench.json`](../evidence/perf/liveness-index-bench.json)
— the compact in-process authenticated live-device index, **measured** at 100k/1M/10M:

- **~12.4 bytes/device** (176× smaller than the 2,185 B SQL footprint) → 10M hot ≈ **124 MB**,
  which *does* fit a small box.
- **~3.5M authenticated heartbeats/s** in-process, O(1) — vs ~4,090 hb/s through coalesced SQL.
- No per-heartbeat WAL: a heartbeat is an in-process update, not a durable write.

This collapses **both** droplet walls: the 21.8 GB RAM wall → 124 MB, and the WAL/fsync wall →
gone. Postgres stays the **canonical durable authority** for eligibility, identity, money,
capability; the index only answers "is this device live right now", and is **fail-closed**
(losing it can only make devices read DEAD, never fabricate eligibility).

**Status: measured and shadow-wired, NOT yet authoritative.** Selection still uses the SQL
liveness predicate. Making the index authoritative (selection = durable PG eligibility ∩
index-live, retiring the per-heartbeat SQL UPDATE) is **the FLIP** — money-adjacent, so it
lands as a separately-reviewed lane, not folded in silently.

---

## Expansion order — what to do, and the trigger for each

| # | Action | Trigger (the measured signal that says "now") |
|---|---|---|
| **0** | **Seal + redeploy the live droplet control plane** (G063) | It is currently **down** — only PG+MinIO run. Capacity planning is moot until it serves. Gated on the operator's deploy secrets (Stripe **test**, existing token key, FX rate) + Cloudflare DNS. |
| **1** | Bump droplet vCPU / RAM | WAL/fsync saturates the single vCPU, **or** Postgres RSS approaches the RAM budget. Cheapest first step; buys near-linear headroom. |
| **2** | Wire the **live-index FLIP** (G082) | Per-heartbeat SQL WAL is the bottleneck — it already is at the 2 ms coalesced path (~20 MB/s). Biggest single lever: removes liveness from PG, RAM wall 21.8 GB → 124 MB, WAL wall gone. Reviewed money-adjacent lane. |
| **3** | Move Postgres **off the droplet** to managed PG (DO Managed / Neon / Supabase) | Durable working set (money + offer/worker rows) exceeds droplet RAM even after the liveness offload, **or** you need HA / PITR / failover. **Keep Postgres** — a lighter engine (D1/SQLite) breaks the money primitives (191 `FOR UPDATE`, 45 `SKIP LOCKED`, `pg_advisory` locks, `NUMERIC`) and is refused. |
| **4** | Add Hyperdrive / PgBouncer pooling | Connection count from multiple control-plane instances saturates managed-PG connections. |
| **5** | Partition the money/offer tables | Single-table index/vacuum cost goes superlinear. Earned only at very large N — don't pre-partition. |

---

## One-line answer for the operator

A single droplet-class box holds **~89k (baseline) to ~184k (coalesced 2 ms) devices alive**,
saturating on the single vCPU's WAL/fsync, and cannot hold 10M's ~21.8 GB working set in
961 MB — so 10M **today** means ≥55 boxes or (better) a bigger box + managed Postgres. The
**live-index FLIP (G082)** is the real path: measured 12.4 B/device drops 10M to ~124 MB of hot
state with no per-heartbeat WAL, keeping Postgres as the durable money authority. Expand in the
order above, each step triggered by the measured signal, not a guess.

---

*Benchmark half of G074b (a fresh quiet-box re-run of the ingest-ceiling harness) is pending —
the host is currently running the G080 suite-parallelization lane; a competing run now would
contaminate both. The numbers above are the last clean measured curve (G073, proxy). The
scaling-axes + expansion-trigger analysis this document provides needs no re-measurement.*
