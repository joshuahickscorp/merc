# Capacity leases (design only)

Status: design, not implemented. Implement only after execution envelopes are
complete and green. This document matches the envelope rigour: where authority
lives, what is safe to over-issue, crash behaviour, and interaction with the
cost-rank ordering in `control/scheduler.go` / realtime offer selection.

## Problem

The supply-side mirror of per-request market clearing: every realtime
authorization re-ranks the offer book, decrements `available_sequences`, and
binds worker + rates into an `execution_contract`. At high concurrency that is
correct but expensive, and it couples admission latency to book depth.

A **capacity lease** is a bounded grant a *verified worker* holds so the hot
scheduler can assign work *within* the lease without re-clearing the full market
per request.

## Lease contents (minimum)

| Field | Purpose |
| --- | --- |
| Worker id + supplier id | Who is bound |
| Runtime profile id + sha256 | Model / runtime scope (must match envelope scope when both exist) |
| Sequence slots | Max concurrent sequences covered by the lease |
| Interval / expires_at | Wall-clock bound |
| Supplier floor rates | Input/output nanos per million (frozen at lease issue) |
| Buyer price ceiling class | Optional: max buyer rates this lease may serve |
| Cost class key | The verified-outcome cost class at issue time |
| Monotonic version | Optimistic concurrency on remaining slots |

## Authoritative state

**Postgres is authority**, same argument as envelopes:

- In-memory slot counters without durable issue are a free credit line for
  capacity: restart, second replica, or clock skew over-admits sequences.
- Safe shape: one row per lease; remaining slots updated by a **single**
  `UPDATE ... WHERE remaining_slots >= $n AND expires_at > now() RETURNING ...`.
- No transaction-scoped lock held across the upstream vLLM call. Bind lease
  slot → run upstream → release/confirm slot on finalize, analogous to envelope
  RESERVED → CAPTURED/VOIDED.

## What makes a lease safe to over-issue (or not)

**Not safe to over-issue** relative to the worker's advertised
`available_sequences` *at issue time*: issuing leases whose total remaining
slots exceed the worker's true free sequences recreates the SKIP LOCKED
false-503 / silent overbook failures the atomic decrement path already fixed.

**Safe to over-issue relative to global demand**: leases are worker-local. Two
workers can each hold leases; the market is not a single global sequence pool.

**Not safe to issue across cost classes**: see ranking interaction below.

Issue-time rules:

1. Worker must be READY, heartbeat-fresh, profile hash match.
2. Lease slots ≤ current `available_sequences` (atomic decrement of both offer
   row and lease issue in one transaction).
3. Supplier floors frozen into the lease; a later cheaper offer does not rewrite
   in-flight leases (same immutability as contract rates today).
4. Expiry returns unused slots to the offer row (or to zero if the worker is
   gone — see death).

## Worker death mid-lease

| Situation | Action |
| --- | --- |
| Heartbeat lost, lease still ACTIVE | Mark lease `FAILOVER_REQUIRED` / `DEAD`; do not assign new work |
| In-flight contracts on that worker | Existing realtime recovery voids or settles by evidence; lease slots for those contracts stay reserved until contract terminal |
| Unused remaining slots | Release to zero on the dead worker's offer (offer is not READY); do **not** silently transfer to another worker — that would skip market clearing |
| Replacement capacity | New work clears the market (or a new lease is issued to a live worker). A lease is not a transferable bearer instrument |

Crash-safety direction (same as envelopes): **err toward holding the slot
reservation** until the bound contract is terminal, so a restart cannot
double-assign the same sequence. Orphan-slot recovery after grace voids holds
with no live contract (no phantom permanent capacity lock).

## Interaction with cost-rank ordering

Batch claim (`control/scheduler.go`) and realtime authorization share a
discipline:

> **Cost rank wins; warmth only breaks ties within a cost class.**

A capacity lease must not become a way to route to a more expensive class.

Concrete rules:

1. **Cost class is frozen at lease issue** from the same verified-outcome cost
   inputs used in `AuthorizeRealtimeContract` (base ask adjusted by measured
   fail/refund rates when sample thresholds are met).
2. The hot path may assign a request to a worker's lease **only if** that
   lease's cost class is still in the minimum cost class among *currently
   eligible* READY offers for the profile. If a cheaper class appears, either:
   - stop assigning from more expensive leases (preferred), or
   - require re-clearing for that request.
3. Warmth (HOT/WARM/COLD) may only choose among leases **inside** the same cost
   class. A HOT expensive lease must never beat a COLD cheaper class.
4. Self-declared warmth must not influence the cost class key.

Pseudo-policy for the assigner:

```
eligible = READY offers for profile with heartbeat freshness
min_class = min(cost_class(o) for o in eligible)
assignable_leases = leases where lease.cost_class == min_class
                    and lease.remaining_slots > 0
                    and lease.worker in eligible
pick = min_by (warmth_rank, worker_id) among assignable_leases
atomic_decrement(pick.remaining_slots)
```

If `assignable_leases` is empty, fall back to full market clear (today's path).

## Interaction with execution envelopes

Envelopes amortize **buyer funding**. Leases amortize **supply selection**.
They compose:

1. Spend envelope (buyer cap) — one UPDATE.
2. Assign lease slot (supply) — one UPDATE.
3. Insert contract binding both.
4. Upstream call (no durable locks held).
5. Finalize: capture envelope spend + release lease slot accounting; supplier
   entitlement still from the per-request PricingDecision, unchanged.

Supplier liability remains the settlement path that exists today. Leases change
*when* capacity is reserved, not *how much* a supplier earns for delivered work.

## Over-issue summary

| Axis | Over-issue? |
| --- | --- |
| Worker's free sequences at issue | No |
| Buyer funding (use envelope) | No — separate authority |
| Cost class vs live book | No — lease unusable if not in min class |
| Warmth preference | Yes, only as tie-break within class |

## Implementation sketch (future)

Tables: `capacity_leases`, `capacity_lease_assignments` (idempotent per request),
events. Ticker: expire leases, recover orphan assignments, reconcile offer
`available_sequences` with sum of active lease remainders + unleased free.

Do not implement until envelope tests are green and the funding amortization is
measured as a real win under concurrent load.
