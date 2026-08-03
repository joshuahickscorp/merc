# One capacity row is one capacity row

**Verdict (2026-08-03): the multi-buyer single-offer authorize tail
(~50–75 ms p95 at c=32) is a thin-book / fixture characterisation, not a
production defect to re-architect capacity around.**

Do not reopen slot-row splitting, counter redesign, or lock-hierarchy
reordering against this number without first measuring a live multi-supplier
book for the same profile. The multi-offer path is already fixed.

## What one offer row is

| Fact | Authority |
| --- | --- |
| One row = one `(worker_id, runtime_profile_id)` | `control/schema.sql` `PRIMARY KEY (worker_id,runtime_profile_id)` on `realtime_worker_offers` |
| Upsert replaces that worker's live capacity for that profile | `Store.UpsertRealtimeOffer` in `control/realtime_store.go` |
| Agent registers **one** offer per vLLM session/profile | `agent/src/vllm.rs` (`register_realtime` after healthy pin) |
| `max_active_sequences` / `available_sequences` is a **counter of concurrent sequence slots on that worker**, default 128 | `agent/vllm.example.toml`, heartbeat clamp in `HeartbeatRealtimeOffer` |
| Not one GPU, not one supplier, not one slot row | Placement (`gpu_count`, `hw_class`) is metadata on the same offer row |

So "more rows" means **more workers advertising the same runtime profile**, not
finer-grained accounting of one worker's sequences. Multiplicity is a **supply
fact** (how many independent workers registered), not a modelling bug in the
counter.

Heartbeat does not invent free capacity: it clamps the advertised value by
in-flight `EXECUTING` contracts. Admit decrements atomically; finalize /
failure / stale recovery release via `releaseRealtimeCapacity`.

## Offers per (model, job type) in every catalogue examined

Realtime routing keys on **runtime profile** (and its model alias), not batch
job type. Counts below are **active offer rows that can clear a chat request
for that profile**.

| Environment | Offers per realtime profile | Source |
| --- | ---: | --- |
| Seeded dev (`make seed` / `control/seed.go`) | **0** until an agent registers | Seed inserts 2 batch workers (`demoWorkerID`, `demoWorkerID2`) for embed/batch_infer only. **No** `realtime_worker_offers` rows. |
| Local agent after register | **1** per agent process × profile | One `register_realtime` per pinned profile (`vllm.rs`). Typical laptop: one worker → one offer for `cx-chat-1b`. |
| VLLM profile catalogue in-repo | **1 profile** today | `control/runtime-profiles/vllm-llama-3.2-1b-instruct-bf16.json` → `cx-chat-1b` |
| Level-B / go-closure canary design | **2** distinct approved workers | `docs/LEVEL_B_OPERATOR_HANDOFF.md`, `ops/go-closure-inputs.json` ("two distinct operator-controlled Metal worker identities") — still a thin book, not a marketplace. |
| Tail probe 1-offer cell | **1** (deliberate) | `TestAuthorizeTailCharacterize` drains all other offers. |
| Tail probe N-offer cell | **N = concurrency** equal-rank offers | Same test; SKIP LOCKED fans out. |
| Intended production marketplace | **N independent suppliers/workers** | Order book ranking, liquidity slices (`current_active_offers`), multi-offer SKIP LOCKED path. |

There is **no production multi-tenant offer census** in-repo (no live fleet
receipt with offer multiplicity). What the repo knows is: seed and local
single-agent paths are thin by construction; canary is two workers; the product
code and probes assume a multi-offer book is the healthy shape.

## Why the ~50–75 ms number appears

Bound evidence: `evidence/perf/authorize-auth-tails-latest.json`
(phase `after_skip_gated_by_offer_count`).

At max_conns=64, c=32 (multi-buyer):

| Cell | p50 ms | p95 ms |
| --- | ---: | ---: |
| 1 offer | ~22 | **~76** |
| N=32 equal-rank offers | ~5 | **~10** |

Same-buyer 1-offer is higher still (~100–200 ms p95) and is owned by the
**buyer funding lock** lane, not this one.

Mechanism (unchanged hierarchy: buyer funding → offer capacity → insert →
commit):

1. Admit takes `FOR UPDATE` on the chosen `realtime_worker_offers` row.
2. PostgreSQL holds that row lock until **transaction commit**, not just the
   UPDATE statement.
3. `available_sequences > 0` decrement remains atomic and correct.
4. With one candidate, every concurrent admit queues on that one row.
5. With many candidates, `FOR UPDATE SKIP LOCKED` (gated by
   `activeOfferCount > 1` in `AuthorizeRealtimeContract`) lets contenders take
   other free offers — already measured ~3–8× better on multi-offer cells.

**One capacity row is one capacity row.** That is physical lock semantics, not
an accidental missing index.

## Is `available_sequences` the wrong granularity?

| Option | Effect on single-offer multi-buyer | Capacity truth | Status |
| --- | --- | --- | --- |
| Keep one counter row per worker×profile | Serialises concurrent admits on that worker for the TX duration | Strong: decrement + contract insert in one TX; release on terminal | **Current** |
| Row-per-slot | Parallel claims on distinct free slots of the same worker | Feasible but multiplies bookkeeping; orphan-slot recovery must match finalize/void/stale paths | Not justified while multi-offer is the production shape |
| Decrement without holding lock through insert | Would shrink hold time only if claim and contract insert split | Splitting risks oversubscribe or stranded capacity across commit boundaries | Rejected for capacity truth |
| Capacity leases (many slots pre-claimed) | Amortises re-ranking; does not remove concurrent multi-buyer wait on one worker's remaining slots | Design only — `docs/CAPACITY_LEASES.md` | Future, after envelopes prove out |

Capacity truth is non-negotiable: a supplier must never be oversubscribed, and
an abandoned claim must not strand sequences. The current counter + lock
through insert satisfies that. Weakening it for a thin-book tail is the wrong
trade.

## What not to do

- Do **not** reverse buyer-funding-before-offer-capacity (deadlock 40P01;
  `TestAuthorizeSettlementDeadlockRepro`).
- Do **not** treat the 1-offer p95 as a standing Merc defect when N-offer p95
  is already ~10 ms at c=32 under the same probe.
- Do **not** invent slot rows to "fix" local `make seed` + one agent.
- Do re-open only if a **bound production or staging liquidity receipt** shows
  sustained single-offer books for a hot profile under multi-buyer load *and*
  that shape is intentional product reality rather than under-supply.

## Operational read

| Observation | Meaning |
| --- | --- |
| Multi-buyer 1-offer p95 high, N-offer low | Thin book / fixture. Grow supply or accept single-worker serialization. |
| Multi-buyer N-offer still high | Real defect (pool, funding, ranking CTE, etc.) — investigate. |
| Same-buyer ≫ multi-buyer 1-offer | Buyer funding lock lane (separate). |

## Related

- Tail factorial: `control/authorize_tail_characterize_test.go`
- Claim SQL + SKIP LOCKED notes: `control/realtime_supplier_outcome_stats.go`,
  `control/realtime_store.go` (`AuthorizeRealtimeContract`)
- Bound numbers: `evidence/perf/authorize-auth-tails-latest.json`
- Lease design (not implemented): `docs/CAPACITY_LEASES.md`
