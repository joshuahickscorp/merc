# Can admission be durable without being synchronous on the first token?

Status: design conclusion with measured floors (2026-08-03).  
Probe: `control/hot_path_free_admit_probe_test.go` → `evidence/perf/hot-path-free-admit-latest.json`.  
Does not change production admit behaviour.

## Plain answer

**No — not if “not synchronous” means zero durable work before dispatch.**

Buyer ceiling under concurrent multi-writer admission requires a **durable**
decrement of available budget before two admits can both observe the same residual.
Supplier liability and crash recovery require a **durable** intent that survives
process death. Removing both from the first-token path reintroduces the shapes
that already produced money P0s (TTL holds that drop in-flight money) or unpaid
supplier work.

**Yes — if “nearly free” means amortized O(1) durable work still before dispatch.**

Pre-authorize funding (execution envelopes, already shipped) and pre-authorize
capacity (capacity leases, designed in `docs/CAPACITY_LEASES.md`, not shipped).
Per request then does:

1. O(1) envelope spend `UPDATE` (or O(1) committed-micros counter)
2. O(1) lease/offer slot `UPDATE`
3. Contract + RESERVED event insert (batched)
4. Commit

That path keeps every disqualifying invariant. It is still synchronous. Its
authorize floor is multi-statement Postgres, measured in the probe — not zero,
and not free enough to assume ≤1 ms Merc-added TTFT without an e2e proof.

**≤1 ms p50 local Merc-added TTFT with durability intact is not achievable by
optimising the legacy multi-aggregate authorize path** (floor ~1.0–1.4 ms
authorize alone; Metal merc-owned was ~3.0 ms). With full amortization it is
**borderline for authorize alone and unproven for merc-added**; renegotiate the
target against the measured O(1) floor rather than chase zero-durable admit.

## What admission does today

### Legacy (no envelope)

`AuthorizeRealtimeContract` one transaction:

| Step | Work | Why synchronous |
| --- | --- | --- |
| Buyer funding | advisory lock + multi-aggregate snapshot + `buyers FOR UPDATE` | ceiling under interleaving |
| Offer claim | ranking CTE + atomic `available_sequences` decrement | capacity truth |
| Pricing / market | CPU + JSON | rate freeze |
| Contract + event | inserts | durable intent + idempotency |
| Commit | WAL | durability |

Lock hierarchy (enforced by `TestAuthorizeSettlementDeadlockRepro`): **buyer
funding before offer capacity**. Do not reverse it.

Measured durable anatomy (segment probe, c=1): funding ~0.15–0.57 ms, offer
~0.22 ms, pricing ~0.04 ms, contract ~0.35 ms, event ~0.11 ms, commit ~0.15 ms
→ **~0.9–1.4 ms** authorize. Prior claimed floor ~1.0 ms (0.7–1.2).

### Envelope ACTIVE

Create already ran `evaluateRealtimeBuyerFunding` for the full cap. Admit:

| Step | Work |
| --- | --- |
| Envelope spend | single `UPDATE` + spend row insert (no buyer advisory lock) |
| Offer claim | **same** ranking CTE as legacy |
| Contract + event | same |
| Commit | same |

Funding multi-aggregate leaves the hot path. Capacity selection and contract
persistence remain. Envelope amortises money check latency, not the whole admit.

## Designs considered

### 1. Bounded pre-authorized execution envelopes (exist)

**Survives** for funding amortization. **Does not** make admit free: offer claim
+ contract insert remain. Does not alone hit ≤1 ms merc-added on the Metal path
where authorize is ~2.6 ms of ~3.0 ms merc-owned.

Rails when ACTIVE:

| Rail | Behaviour |
| --- | --- |
| `evaluateRealtimeBuyerFunding` | Holds `(cap−spent)` on ACTIVE envelopes; EXECUTING under ACTIVE envelope excluded from realtimeReserved; when envelope leaves ACTIVE, in-flight RESERVED spends fall back to EXECUTING hold |
| `prepaidOpenReservationMicros` | Same ACTIVE term + expired-envelope EXECUTING fallback |
| `BuyerFreeCreditRemaining` | **Gap today**: only free credit − charges − open jobs − EXECUTING maxima. Does **not** hold ACTIVE envelope residual. Sibling-rail incompleteness, not introduced by this probe |

### 2. Capacity leases (`docs/CAPACITY_LEASES.md`, not implemented)

**Survives** as design if Postgres remains authority and cost-class rules hold.
Hot path becomes O(1) slot `UPDATE` instead of ranking CTE. Composes with
envelopes (funding + supply both amortized). Still durable + sync before
dispatch. Probe stand-in: direct `UPDATE realtime_worker_offers … WHERE worker_id=$1`.

### 3. Speculative admit, durable settle before money moves

**Dies in the strict form** (dispatch with no durable hold):

| Attack | Outcome |
| --- | --- |
| Two concurrent admits, same buyer, residual covers one ceiling | Both pass in-memory check → **buyer ceiling broken** |
| Multi-replica control plane | In-memory counters diverge → over-admit |
| Kill after tokens, before durable record | No contract → **supplier unpaid** or invent orphan pay from evidence (operational TTL-hold shape; two prior P0s) |
| Replay without durable idempotency key | Double dispatch |

**Partially survives** only if “speculative” means: durable O(1) holds +
minimal intent **before** dispatch, and heavy enrichment/settlement later.
That is not removing durability from the first-token path; it is the amortized
design above. Exposure window if contract insert is deferred after dispatch:

| Name | Bound |
| --- | --- |
| **dispatch-to-contract gap** | From commit of spend+capacity hold to commit of `execution_contracts` row |
| Money | Covered by durable envelope spend / hold (not trust) |
| Capacity | Covered by durable lease/offer decrement |
| Supplier liability | **Broken** if work completes and process dies before contract unless orphan-evidence payout exists — same family as TTL holds; **disqualified for production without that machinery green** |

Recommendation: do **not** open the dispatch-to-contract gap. Keep contract
insert in the pre-dispatch transaction. Overlap only helps if engine TTFT hides
post-dispatch work; merc-added is dominated by **pre-engine** work.

### 4. Short-reserve + O(1) committed-micros (`3c6c5157`, not merged)

Two-phase legacy admit: TX1 materialises `realtime_funding_holds`, TX2 claims
offer without buyer funding lock. Measured **~1.33–1.37×** same-buyer p95 at
c=32; **c=1 slightly worse** (second begin/commit). Remaining cost is the
multi-aggregate re-aggregate under the lock, not the offer claim.

**Survives** correctness after KEY SHARE-before-offer deadlock fix. **Died on
risk/reward** (~30% one tail vs two money P0s + 40P01 history). Named next step
was O(1) `committed_micros` under TX1 — would cut funding snapshot, still leave
durable sync multi-statement admit, still not “free on first token”.

Worth reviving **only** with the O(1) counter **and** only for legacy
non-envelope buyers; envelope path already avoids the funding lock.

## Surviving design (recommended)

```
offline / create-time:
  CreateExecutionEnvelope(cap)     -- durable full-cap hold
  IssueCapacityLease(worker, slots) -- durable capacity (future)

hot path (still sync, still durable, O(1)+insert):
  BEGIN
    UPDATE envelope SET reserved += need WHERE residual >= need  -- funding token
    SELECT buyers.id FOR KEY SHARE WHERE id = buyer               -- hierarchy
    UPDATE lease/offer SET remaining -= 1 WHERE remaining > 0    -- capacity token
    INSERT execution_contracts … EXECUTING
    INSERT realtime_authorization_events RESERVED
    bind spend/lease → contract
  COMMIT
  → dispatch upstream
```

- **Exposure window:** none beyond today’s EXECUTING reservation (durable before
  first byte to the engine).
- **Not free:** still ~begin + 2 updates + insert batch + commit on local
  Postgres.
- **≤1 ms merc-added:** only if this authorize floor plus intake/admission/proxy
  sum ≤1 ms e2e against the real engine. Authorize-alone measurements in the
  probe are necessary but not sufficient.

## Attacks that survived (on the recommended design)

| Attack | Why it fails to break the design |
| --- | --- |
| Concurrent same-buyer overspend | Envelope `UPDATE … reserved+need ≤ cap` is atomic |
| Concurrent capacity overbook | Lease/offer `UPDATE … remaining > 0` is atomic |
| Envelope expiry mid-flight | Existing ACTIVE-conditional exclusion: in-flight falls back to EXECUTING hold on all complete rails (fix `BuyerFreeCreditRemaining` if used for cash) |
| Kill after commit before stream | EXECUTING + RESERVED spend reconcilable; finalize/void paths exist |
| Kill before commit | Rollback restores envelope residual and capacity |
| Replay | Spend idempotency key + contract idempotency key |
| Settlement vs admit deadlock | KEY SHARE (or funding lock) on buyer **before** offer; never reverse |
| Reverse lock hierarchy for speed | **Disqualified** — 40P01 already proven |

## Three committed-money rails under the recommended design

| Rail | Envelope residual | OPEN short-reserve hold | EXECUTING contract | Expired envelope in-flight |
| --- | --- | --- | --- | --- |
| `evaluateRealtimeBuyerFunding` | ACTIVE `(cap−spent)` | must sum OPEN holds if short-reserve ships | sum maxima; exclude only if spend’s envelope still ACTIVE | hold via EXECUTING (exclusion off) |
| `prepaidOpenReservationMicros` | same ACTIVE term | must include OPEN holds if shipped | expired-envelope RESERVED spend fallback | same |
| `BuyerFreeCreditRemaining` | **must add ACTIVE residual** (gap today) | **must add OPEN holds** if shipped | EXECUTING maxima (present) | covered if EXECUTING still counted |

Any design that drops a term on one rail while another rail still spends is a
P0. Envelopes already taught that lesson twice.

## What to renegotiate

| Target | Honest position after this analysis |
| --- | --- |
| ≤1 ms p50 local Merc-added TTFT | Not reachable on legacy path. Borderline under full amortization; **require e2e Metal remeasure** after envelope-default + lease, not authorize-only hope |
| Authorize 10× at c=1 | Floor branch already MET: ~1 ms durable work is irreducible without removing durability |
| “Architecture nearly free in the hot path” | Achievable as **amortized O(1) durable**, not as **async durable** |

## Measured floors (this worktree probe)

Machine: Mac Studio, 28 CPUs, load ~8 / 28 (quiet by probe heuristic).  
n=80 samples/cell, same buyer, one offer unless noted. Unbound probe receipt.

### Authorize p50 / p95 (ms)

| Path | c=1 p50 | c=1 p95 | c=8 p50 | c=8 p95 | c=32 p50 | c=32 p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Legacy multi-aggregate | 1.13 | 1.45 | 13.4 | 38.8 | 59.1 | 117 |
| Envelope ACTIVE (prod path) | 1.25 | 1.61 | 3.19 | 5.12 | 3.21 | 33.3 |
| Envelope + direct offer claim (lease stand-in) | **0.96** | 1.33 | **2.51** | 5.71 | **2.88** | 24.0 |

### Direct-claim serial decomp (c=1, p50 ms)

| begin | envelope spend | direct offer | pricing | contract+event batch | commit | total |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| (in total) | 0.41 | 0.22 | (in total) | 0.39 | 0.15 | **1.30** |

### Reading

1. **Envelope does not win at c=1.** Prod envelope path was *slightly slower* than
   legacy at c=1 (1.25 vs 1.13): spend UPDATE+row insert costs more than a quiet
   funding snapshot. The envelope win is **concurrency**: same-buyer c=32 drops
   from 59 ms → 3.2 ms p50 because the buyer funding advisory lock leaves the path.
2. **Lease stand-in helps a little more** (0.96 ms c=1; 2.88 ms c=32 p50) by
   replacing the ranking CTE with a PK update. Contract insert + commit remain
   ~0.5 ms combined — the durable floor of “intent must exist before dispatch”.
3. **≤1 ms merc-added TTFT is still not shown.** Best authorize p50 is 0.96 ms
   *before* intake/admission/proxy. Prior mock merc-added had ~0.4 ms of those;
   Metal merc-owned was ~3.0 ms. Authorize-alone under 1 ms ≠ target met.
4. **c=32 p95** on envelope paths (24–33 ms) is still offer-row serialisation on
   a one-offer book — multiplicity / leases address the tail, not zero-durable.

## Probe how-to

```bash
MERC_HOT_PATH_FREE_PROBE=1 \
MERC_TEST_DATABASE_URL=postgres://cx:cx@localhost:5432/cx?sslmode=disable \
go test -count=1 -run TestHotPathFreeAdmitProbe -timeout 30m ./control
```
