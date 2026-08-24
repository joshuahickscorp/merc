# Lane AUDIT — invariants whose enforcement is narrower than their purpose

HEAD: `83806ccf` (`control: a worker may not verify its own answer`).
This is a read-only audit. No behaviour was changed. The single write is this file.

The defect that prompted the hunt had two parts, now closed on the **pull**
claim path in `control/scheduler.go`:

1. Prior-executor exclusion was gated on `is_redundancy AND hedged_from IS NOT NULL`,
   so it covered only hedged third-opinion rows.
2. The prior set was built from `execution_worker_id`, which is empty until a
   task has started, so a concurrent-permit holder could take both copies
   before either finished.

This report hunts the same two shapes, plus sibling-branch and fail-open
variants, on money, trust, containment, eligibility, and settlement. Findings
are ranked by whether real harm is reachable, not by how odd a predicate looks.
Narrow gates that are correct and deliberate are listed separately and are
**not** defects.

Line numbers are from `git show HEAD:<path>` (sparse checkout; most of
`control/` is not on disk).

---

## Verdict

The pull-path fix in `83806ccf` is real and correctly scoped: both claim
branches (`claimed_by = $1 AND started_at IS NULL` and `claimed_by IS NULL`)
now exclude prior executors/holders on **every** redundancy row, and
`claimed_by` is in the prior set.

It is not the only instance of the shape.

The remaining reachable holes are:

- The **push** pin writers (`InsertTiebreakTask` / `InsertHedgeTask`) still
  exclude only the **anchor** task's execution identity, not every prior
  executor of the chunk. Dispute re-verify feeds that writer with `also=nil`.
- Two machines of the same supplier can still **concurrently** pull-claim
  primary and redundancy: `ClaimTasksTx` serialises on the worker row, not
  the supplier.
- Realtime authorize and service-lease admission never apply the sandbox /
  device-bind / buyer–supplier independence gates that batch claim documents
  as the bar for ordinary buyer work.

Settlement still fails closed on a same-supplier redundancy *match* that
makes it to vote comparison (no money for a faked check). The push-path pin
and the concurrent same-supplier race fail the same way the original bug
did: the job strands or the check is not independent. Realtime/lease is the
one family that can put buyer payload on an uncontained host without a later
independence vote to catch it.

---

## Ranked findings

### F1. Push-side pin eligibility excludes only the anchor, not the chunk
**Rank:** highest remaining batch-verification hole (same harm class as
`83806ccf`, different door).
**Shapes:** 1 (purpose vs predicate), 3 (push vs pull; `InsertTiebreakTask` vs
`insertPlannedTiebreakTx`; dispute vs mismatch dispatch).
**Fail:** closed at pull claim / `StartTask` (the unsafe pin cannot *start*),
open at pin creation (the row is born claimed by a prior executor).
**Confidence:** high. Settled by a test that inserts a complete primary +
complete redundancy, then `InsertTiebreakTask` / dispute re-verify with the
redundancy worker as peer, and asserts `ErrNoSupply` rather than a pin.

**Stated purpose** (`83806ccf` commit, and pull-path comment):

> Prior-executor / current-holder exclusion applies to EVERY redundancy row.
> A worker, or any other machine owned by the same supplier, that already
> executed or is currently holding another task for this (job_id,
> COALESCE(chunk_index,0)) cannot take the check copy.
>
> Scope is the pull path. Push-side peer selection already excludes the
> anchor worker.

**Actual enforcing condition** on the push insert that *creates* the pin
(`control/store_tasks.go` `dynamicPeerClaimEligibleTx`):

```
AND nw.id IS DISTINCT FROM anchor.execution_worker_id
AND nw.supplier_id IS DISTINCT FROM anchor.execution_supplier_id
```

(`control/store_tasks.go:727-728`, used by `InsertTiebreakTask` at
`:815` and `InsertHedgeTask` at `:1279`.)

That is identity of **one** row (`anchor`), keyed on columns that are NULL
until that row has started, compared with `IS DISTINCT FROM` (NULL is
distinct from every worker id, so a not-yet-started anchor excludes
nobody).

The sibling writer `insertPlannedTiebreakTx`
(`control/verification_apply.go:1100`) uses
`tiebreakPeerClaimEligibleTx`, which *does* walk every
`execution_worker_id` / `verification_work` / `task_execution_history` row
for the chunk (`control/verification_lifecycle.go:326-349`). Pull
`claimTaskSQL` now walks those plus `claimed_by` (`control/scheduler.go:1067-1109`).
Push insert does not.

**Who slips through:** the worker (or another machine of the same supplier)
that executed the *redundancy copy*, not the primary. Dispute re-verify is
the clean reachability story:

```
peer, perr := wk.store.SelectRedundancyPeerExcluding(ctx, d.JobID, target.TaskID,
    target.JobType, target.ModelRef, target.MinMemGB, target.AnchorWorker, nil, nil)
...
reverifyID, ierr := wk.store.InsertTiebreakTask(...)
```

(`control/workers.go:652-663`.) `also` and `alsoSuppliers` are **nil**.
`ReverifyTarget` returns only the first complete non-redundancy row's
`worker_id` (`control/store.go:1705-1713`). The redundancy executor is not
in the excluded set. `InsertTiebreakTask` then accepts them because they
are not the anchor.

Mismatch dispatch (`control/verification.go:557-573`) is stricter at
*selection* (it passes every other `ChunkResults` worker/supplier as
`also`), but still writes the pin through the same anchor-only insert.
If selection is ever bypassed, insert will not save it.

After the pin exists, pull claim (`claimTaskSQL` on both branches) and
`StartTask` for `is_redundancy && hedged_from != nil`
(`control/store_tasks.go:233,275-279`) re-check a fuller prior set and
refuse. Result: the check copy sits pinned to a disputant who cannot
start it, until `StalePinnedTiebreaks` (`is_redundancy=true AND
hedged_from IS NOT NULL`, `control/verification_lifecycle.go:192-197`)
reassigns. That is the original stranded-`verifying` failure, reconstructed
on the push door.

Hedges (`is_redundancy=false`) are not verification copies; same-supplier
racing a straggler is mostly a money duplicate that
`FinalizeTaskVerification` / `fenceHedgeSettlement` suppress by skipping a
second `buyer_charge`. Hedge *selection* also passes
`representedChunkSuppliers`, which already includes `claimed_by` holders
(`control/workers.go:791-797`). The insert gate is still the weaker one.

---

### F2. Same-supplier concurrent pull-claim still races
**Rank:** same verification-integrity harm as `83806ccf`, reachable only
with two live workers of one supplier polling at once.
**Shapes:** 2 (prior set is committed state; the other txn has not
committed), residual of the race the pull fix closed for **one** worker.
**Fail:** closed at settlement if both complete (`independentRedundancyMatch`
on two same-supplier votes returns false → `NO_INDEPENDENT_SUPPLIER`).
The job can strand in `verifying` if that path returns an error rather than
an `OutcomeFail` decision (`control/verification.go:214`).
**Confidence:** high on the locking; high that sequential same-supplier is
now refused (tested in `control/redundancy_independence_claim_test.go`);
untested for two concurrent transactions.

**Stated purpose** (`control/scheduler.go:1051-1058`):

> A worker, or any other machine owned by the same supplier, that already
> executed or is currently holding another task for this (job_id,
> COALESCE(chunk_index,0)) cannot take the check copy. claimed_by is part
> of the prior set: execution_worker_id is written only once the task has
> executed, and an agent with concurrent permits can hold both copies
> before either finishes.

**Actual enforcing condition:** the `NOT EXISTS` prior set in
`claimTaskSQL` (`control/scheduler.go:1070-1109`) reads committed
`execution_worker_id`, `claimed_by`, `verification_work`, and
`task_execution_history`. `ClaimTasksTx` takes

```
SELECT ... FROM workers WHERE id = $1 FOR UPDATE
```

(`control/scheduler.go:1229-1231`) — **one worker**, not the supplier.
Two agents of the same supplier lock different worker rows, `SKIP LOCKED`
different task rows, and neither `SELECT` sees the other's uncommitted
`claimed_by` / `execution_supplier_id`. Both commit. Same supplier holds
both copies.

The pull fix closed the **sequential** case (second poll sees the first
commit) and the **same-worker** concurrent case (same `FOR UPDATE` row).
It did not close two machines.

`IS DISTINCT FROM NULL` on the push path (F1) is the same empty-prior
shape for an anchor that has not started; pull at least has *some* prior
once the first txn commits.

---

### F3. Realtime authorize and service-lease admission omit containment and buyer–supplier independence
**Rank:** highest *containment* hole if those products are ordinary buyer
work; lower if they are a deliberate uncontained serving class.
**Shapes:** 3 (batch claim / quote vs authorize / lease).
**Fail:** open — missing sandbox, missing device bind, missing linked-supplier
exclusion all yield admission.
**Confidence:** high that the predicates are absent from the live SQL;
medium that this is an oversight rather than a vLLM-serving product split.
Settled by one authorize/lease test with `sandboxed=false` (and one with
`owner_buyer_id = buyer`) asserting refusal.

**Stated purpose** (`control/types.go:213-218` and
`control/buyer_supplier_independence.go:235-243,248-250,267-281`):

> ordinary buyer work requires sandboxed=true and unsandboxed_opt_in=false;
> directed work (workload_decision.directed_cell_id) is the named permit
> for uncontained supply; opt-in remains an absolute exclusion.
>
> Every claim/quote/narrowing reader of ordinary containment must go
> through this helper ... so a future path cannot re-introduce
> self-declared eligibility.
>
> a supplier owned by, enrolled by, or sharing a meaningful email domain
> with the buyer cannot claim that buyer's tasks — including
> redundancy/honeypot verification.

**Actual enforcing condition** on realtime offer reservation
(`control/realtime_supplier_outcome_stats.go:138-143`, same list as the
branch-probe predicate at `:52-58`):

```
WHERE c.runtime_profile_id=$1 AND c.runtime_profile_sha256=$2
  AND c.status='ACTIVE' AND c.available_sequences > 0
  AND c.last_seen_at > now()-interval '45 seconds'
  AND s.status='active' AND s.quarantined_at IS NULL
  AND c.supplier_input_usd_per_million_tokens <= $3
  AND c.supplier_output_usd_per_million_tokens <= $4
```

`UpsertRealtimeOffer` (`control/realtime_store.go:332-391`) writes the
offer after checking worker id + supplier id and the runtime profile. It
does not read `sandboxed`, `unsandboxed_opt_in`, `worker_tokens.device_fingerprint`,
or any buyer-link signal.

Service-lease admission (`control/service_leases.go:777-786`):

```
FROM service_lease_worker_offers
WHERE runtime_profile_id=$1 AND ... AND status='READY'
  AND p95_latency_milliseconds>0 AND latency_measurement_count>=5
  ...
  AND last_seen_at > now()-interval '45 seconds' AND available_warm_replicas >= $4
```

Same omissions.

Quote capacity *does* use `claimableWorkerPredicateSQL`
(`control/quote.go:1316-1332`): sandboxed + device-bound + not
`unsandboxed_opt_in`, and deliberately **without** the directed-cell
permit so uncontained directed supply cannot inflate public quotes.
Batch `ClaimTaskSQL` splices `workerJobContainmentSQL` and
`claimIndependenceSQL` (`control/scheduler.go:923-924`).

**Who slips through:** an unsandboxed (or unbound-token) worker that
registers a vLLM offer serves buyer prompts on a host the batch path would
refuse. A supplier `owner_buyer_id`-linked to the buyer can clear their
own chat traffic and collect supplier rate. There is no later redundancy
vote on this path.

Caveat, marked plainly: the helper comment names "claim/quote/narrowing",
not authorize/lease. Persistent vLLM serving may be unable to run under
the deny-default seatbelt. If that is the product rule, F3 is a **correct
split** and the types.go sentence "ordinary buyer work requires
sandboxed=true" needs to name the serving exception. As written, the
serving paths are a sibling of claim that dropped the bar.

---

### F4. `independentRedundancyMatch` treats a single vote as a match
**Rank:** fail-open verification label and payout *if* the peer lookup and
the vote gather disagree; otherwise the default branch is only reached
with two agreeing votes.
**Shapes:** 1 (comment vs predicate), 4 (too few votes grant permission),
3 (peer lookup by `input_ref` vs gather by `chunk_index` and
`is_honeypot=false`).
**Fail:** open (records `redundancy_match`, `OutcomePass`).
**Confidence:** high that the predicate contradicts its comment; medium
that a live job can present exactly one `ChunkResults` row while
`redundancyBytes != nil`. Settled by a unit test: `redundancyBytes` set,
`ChunkResults` returning only the committer, assert not `redundancy_match`.

**Stated purpose** (`control/verification.go:206-207,230-234`):

> Same-supplier (or linked) redundancy is not independent verification.
> Fail closed rather than settle as if verified.
>
> minIndependentSuppliersForRedundancy is the smallest set of distinct
> suppliers that independentRedundancyMatch accepts as a real vote.
> verification.go fails closed with NO_INDEPENDENT_SUPPLIER when
> len(independentSupplierVotes(all)) is below this.

**Actual enforcing condition** (`control/verification.go:242-246`):

```
func independentRedundancyMatch(all []chunkVote) bool {
    if len(all) <= 1 {
        return true
    }
    return independentSupplierCountSatisfiesRedundancy(len(independentSupplierVotes(all)))
}
```

Zero or one vote is a match. Two same-supplier votes correctly fail
(the `default` branch uses **raw** votes, so same-supplier pairs still
hit the `else` at `:205`).

The `redundancyBytes != nil` arm (`:163-173`) only synthesises a two-vote
fallback when `gatherChunkResults` returns **empty**. A one-row gather is
kept, collapsed by `independentSupplierVotes` to length 1, and passed as
`redundancy_match` **without comparing `redundancyBytes`**.

Peer bytes are loaded by `PeerSealedResult` / `PeerResultKey`, which join
on `job_id` + `input_ref` and do **not** exclude honeypots
(`control/store.go:946-948,972-973`). Votes are gathered by
`ChunkResults`, which joins on `job_id` + `chunk_index`, requires
`is_honeypot = false`, and requires execution identity
(`control/store_tasks.go:603-607`). A complete honeypot (or any complete
sibling) that shares `input_ref` but not `chunk_index` can populate
`redundancyBytes` and then vanish from the gather.

Two same-supplier completed copies of the *same* chunk do **not** hit this
hole: gather returns two rows, `independentRedundancyMatch` returns false,
settlement fails closed.

---

### F5. `tiebreakPeerClaimEligibleTx` prior set still omits `claimed_by`
**Rank:** residual of shape 2 on the start/reassign path; lower than F1
because third-opinion runs after the first two copies have executed.
**Shapes:** 2, 3 (pull claim vs start/reassign).
**Fail:** closed if the sibling has `execution_worker_id` (usual at
third-opinion time); open if the only prior is a claimed-but-unstarted
holder.
**Confidence:** high that the UNION is missing; low that a live
third-opinion has a claimed-unstarted sibling (one tiebreak per chunk).

**Stated purpose:** same as F1 — claimed-but-not-yet-executed siblings
must count (`control/scheduler.go:1082-1083`).

**Actual enforcing condition** (`control/verification_lifecycle.go:326-349`):

```
SELECT prior.execution_worker_id AS worker_id,
       prior.execution_supplier_id AS supplier_id
  FROM tasks prior
 WHERE ... AND prior.execution_worker_id IS NOT NULL
UNION ALL
SELECT work.worker_id, work.supplier_id FROM verification_work ...
UNION ALL
SELECT history.worker_id, history.supplier_id FROM task_execution_history ...
```

No `claimed_by` UNION. `PinnedTiebreakExclusions` (`:367-378`) is the
same three-source set. Pull `claimTaskSQL` added the fourth source in
`83806ccf`; this sibling was not updated.

Used by `StartTask` when `dynamicTiebreak` (`control/store_tasks.go:233,279`)
and by `ReassignPinnedTiebreak` (`control/verification_lifecycle.go:440`).
`StartTask` does **not** call it for ordinary redundancy
(`hedged_from IS NULL`) or for hedges (`is_redundancy=false`). Ordinary
redundancy is not born pinned, so that narrowness is currently correct
(see C4).

---

### F6. LoRA account independence fails open on a nil account id
**Rank:** money, but the production receipt writer already refuses nil
ids. Residual on the settlement helper.
**Shapes:** 1, 4.
**Fail:** open if `settleLoRARun` is called with two suppliers and
`TrainerAccountID == uuid.Nil`.
**Confidence:** high on the helper; high that
`validateProjectLoRAEvaluationReceipt` (`control/lora_evaluation_receipts.go:267`)
refuses nil accounts on the project path. Settled by calling
`validateLoRAIndependence` with distinct suppliers and nil accounts and
expecting `errLoRANotIndependent`.

**Stated purpose** (`control/lora_settlement.go:192-196`):

> the rule is stricter: not the same worker, and not the same account
> behind two workers.

**Actual enforcing condition** (`control/lora_settlement.go:204-207`):

```
if eval.TrainerAccountID != uuid.Nil && eval.TrainerAccountID == eval.EvaluatorAccountID {
    return fmt.Errorf("%w: trainer and evaluator are two workers on one account",
        errLoRANotIndependent)
}
```

Nil trainer account skips the account check. Supplier-id equality still
refuses (`:201-202`). Two workers, one account, only fails when the
account ids are **populated and equal**.

---

### F7. `buyerSupplierLinkSignals` treats a missing buyer/supplier row as unlinked
**Rank:** low. Not the claim SQL gate. Used for exclusion *recording* and
the Go helper `SupplierLinkedToBuyer`.
**Shapes:** 4.
**Fail:** open (`ErrNoRows` → `nil, nil` → `len(signals)==0` → unlinked).
**Confidence:** high on the helper; high that live claim SQL does not go
through it (claimer without a `suppliers` row never reaches `eligible_jobs`).

**Stated purpose** (`control/buyer_supplier_independence.go:74-75`):

> returns the non-empty set of link signal names that connect this
> supplier to this buyer. Empty means unlinked.

**Actual enforcing condition** (`control/buyer_supplier_independence.go:96-97`):

```
if errors.Is(err, pgx.ErrNoRows) {
    return nil, nil
}
```

A JOIN miss is reported as "no link", which is permission. Claim
enforcement is the SQL in `claimIndependenceSQL` / `supplierNotLinkedToBuyerSQL`,
which fail closed by requiring a positive link to exclude (a missing
supplier simply does not match `me.supplier_id_s`). Do not "fix" claim
SQL from this helper; if anything, the helper should fail closed so
receipts cannot say "unlinked" when the rows were missing.

---

## Deliberate narrow gates (not defects)

These are narrow on purpose. Flagging them as bugs would hide F1–F3.

### C1. Frozen verification-class pinning stays on hedged third-opinion rows only
`control/scheduler.go:1111-1120`: `t.hedged_from IS NULL OR (verification_hw_class pinned ...)`.
Ordinary redundancy has no frozen class; requiring one would make those
rows unclaimable. This is the split `83806ccf` made. **Correct.**

Empty `verification_engine` / `build_hash` / `policy` skip those columns
(`COALESCE(...,'')='' OR match`). Hardware class is still required for
hedged rows. Pre-v3 placements grandfather the rest. **Correct** as
back-compat; not the original bug's shape.

### C2. Honeypots are not covered by prior-executor exclusion
`83806ccf`: "Honeypots are deliberately untouched: a known-answer probe is
aimed at one worker, so same-worker execution is the point." Pull SQL
gates independence on `COALESCE(t.is_redundancy,false)` only.
**Correct.**

### C3. Submit does not refuse when no independent supplier is online
`control/store_jobs.go:251-254`: claim filters linked suppliers; settlement
fails closed; "the queue may wait." `RefuseIfNoIndependentSupplier`
(`control/buyer_supplier_independence.go:354-358`) exists and is called
from `control/containment_identity_test.go` only. The comment on
`CountIndependentSuppliersForBuyer` (`:308-310`, "Used at submit and
settlement") is **stale**. Live policy is C3, not that comment.
**Correct**, with a stale-comment residue.

### C4. `StartTask` re-checks peer eligibility only for `is_redundancy && hedged_from`
Ordinary redundancy is claimed via pull, which now has the full prior set.
Hedges are latency races, not check copies, and are born through
`dynamicPeerClaimEligibleTx` (F1 is about *who* that function excludes,
not about this StartTask split). **Correct** given current writers.

### C5. `excluded_worker` is a time window, not a permanent ban
`control/scheduler.go:1045-1048`: `excluded_worker IS NULL OR <> me OR
excluded_until IS NULL OR <= now()`. Schema comment: `NULL = no exclusion`.
Purpose is first-crack after a honeypot fail, then anyone. **Correct.**
A thin fleet must not be starved.

### C6. Cheaper-ask deferral fails closed on a missing ETA
`control/scheduler.go` comment at the ask-deferral predicate: if
`sla_guarantee_secs > 0` and `eta_secs IS NULL`, do not defer. Unknown is
not zero. **Correct** (fail closed = do not hold the cheap-ask window).

### C7. `max_usd IS NULL` skips the budget governor
No cap means no cap. Extra tiebreaks are still bounded by
`job_economic_reserves.reserved_tasks`. **Correct.**

### C8. Placement v1/v2 skip v3 identity pin; v4 skips it for a different reason
`control/scheduler.go:904-916`: v4 is multi-family under an
`AcceptableQualityContract`; pinning `hardware_identity` would freeze one
family. Cell membership is the `runtime_candidates` / WAC join above.
Historical v1/v2 keep pre-identity semantics. New quotes mint v3 or v4
(`control/quote.go:454-459`). **Correct.**

### C9. Directed `directed_cell_id` is the named uncontained permit
`workerJobContainmentSQL` (`control/buyer_supplier_independence.go:257-264`):
ordinary work needs `sandboxed` AND device bind; non-empty
`workload_decision.directed_cell_id` is the operator/lab/canary permit;
`unsandboxed_opt_in` remains an absolute exclusion even then. Quote
capacity does **not** honour that permit (`control/quote.go:1320-1321`).
**Correct.**

### C10. `payout_instrument_owner` is not an independent link signal
Recorded only when `stripe_acct <> ''` **and** `owner_buyer_id` already
matches (`control/buyer_supplier_independence.go:113-114`). SQL helpers
do not test Stripe alone. **Correct** — it is a receipt label on an
already-linked owner, not a fourth exclusion.

### C11. Hedge settlement skips a second `buyer_charge`; redundancy does not
`fenceHedgeSettlementTx` / `FinalizeTaskVerification`: `is_redundancy`
always carries settlement entries; non-redundancy hedges share one charge
across `hedged_from` siblings. Redundancy is priced extra check work.
Hedge is a race. **Correct.**

### C12. Mismatch without a third worker is provisional pay, not a silent pass
`dispatchTiebreak` returns `nil` on `ErrNoSupply`
(`control/verification.go:567-568`). The caller still returns
`OutcomePassWithPenalty`. That is logged provisional trust when no
same-class third worker is online, not a missing independence check on a
same-supplier pair (that pair never reaches this branch). **Correct.**

### C13. `COALESCE(sandboxed,false)` on claim
Missing/false sandbox does **not** grant ordinary work; it requires the
directed permit. Device bind `EXISTS` fails closed when the fingerprint is
NULL or blank. **Correct.** Opposite of fail-open.

### C14. Pinned vs general pull claim
Both branches substitute into the same `claimTaskSQL`
(`control/scheduler.go:1278-1281`). Independence is not on one branch
only. **Correct** (this was an explicit goal of `83806ccf`).

---

## Where each shape was searched (including empty results)

### Shape 1 — enforcing condition narrower than stated purpose

| Area | Looked | Result |
|---|---|---|
| Pull claim independence | `control/scheduler.go` `claimTaskSQL` | Fixed in `83806ccf`. Remaining: F2 (supplier-granularity race). |
| Push pin eligibility | `control/store_tasks.go` `dynamicPeerClaimEligibleTx`, `InsertTiebreakTask`, `InsertHedgeTask` | **F1** |
| Start / reassign | `control/store_tasks.go` `StartTask`; `control/verification_lifecycle.go` | **F5**; C4 |
| Settlement votes | `control/verification.go` `independentRedundancyMatch`, `verifyTaskResult` | **F4** |
| LoRA | `control/lora_settlement.go`, `control/lora_evaluation_receipts.go` | **F6** on the helper; receipt path refuses nil |
| Submit vs settlement independence | `control/store_jobs.go`, `RefuseIfNoIndependentSupplier` | C3; stale comment on `CountIndependentSuppliersForBuyer` |
| Canary honeypot substitution | `control/canary_policy.go` `requiresHeterogeneousHoneypot` | Floor uses `admissibleSupplierCount`, not online liveness. Correct for admission. |
| Money / contribution | `control/contribution_settlement.go`, `control/observed_output_settlement.go`, `validateVerificationSettlementTx` | No purpose/predicate mismatch of this shape. Missing risk-reserve row leaves `RiskCanonical` false (TrueNet withheld), not a pay-out. |
| Prepaid / payout | `control/prepaid.go`, `control/store_payouts.go`, `control/payment_authority.go` | No analogous "gated on the hedged branch only" money rule found. |

### Shape 2 — guard keyed on state populated later than the decision

| Area | Looked | Result |
|---|---|---|
| `execution_worker_id` at claim | `claimTaskSQL` prior set; `ClaimTasksTx` UPDATE writes it on pull claim; trigger `cx_protect_task_execution_identity` only allows write on queued/retrying→running | Pull adds `claimed_by`. Same-worker concurrent polls serialise on worker `FOR UPDATE`. **F2** remains across two workers. |
| Push eligibility vs NULL anchor identity | `dynamicPeerClaimEligibleTx` `IS DISTINCT FROM anchor.execution_worker_id` | **F1**. NULL excludes nobody. |
| `frozenDynamicPeerAnchor` | `control/scheduler.go:278+` `COALESCE(execution_worker_id, '0000…')` then `WorkerID == uuid.Nil` refuses `Current` | Fail closed for v3 freeze; falls back to `GetWorkerProfile(anchor)`. Not a grant. |
| `tiebreakPeerClaimEligibleTx` / `PinnedTiebreakExclusions` | still `execution_worker_id IS NOT NULL` only | **F5** |
| `DeadClaimedTasks` | requires `execution_worker_id IS NOT NULL` (`control/store_tasks.go:1386`) | Running tasks that reached Start/Claim have it. Pinned-unstarted are not `status='running'`. No grant. |
| Prefix routing | `execution_worker_id = $2 OR worker_id = $2` | Affinity, not an exclusion gate. |
| ReverifyTarget | `tk.worker_id` on complete rows (`worker_id IS NOT NULL`) | Complete rows keep `worker_id`. Feeds F1 by excluding only that one worker. |

### Shape 3 — exclusion on one branch, not its sibling

| Pair | Looked | Result |
|---|---|---|
| Pinned vs general pull claim | `ClaimTasksTx` both predicates into `claimTaskSQL` | C14, covered |
| Push insert vs pull claim | `dynamicPeerClaimEligibleTx` vs `claimTaskSQL` | **F1** |
| `InsertTiebreakTask` vs `insertPlannedTiebreakTx` | anchor-only vs full prior set (minus `claimed_by`) | **F1** |
| Mismatch dispatch vs dispute re-verify | `also` from `ChunkResults` vs `nil, nil` | **F1** (dispute) |
| Hedge select vs hedge insert | `representedChunkSuppliers` includes `claimed_by`; insert does not | Select is stricter; insert is the last word. **F1** |
| Peer artifact vs vote gather | `input_ref` / no honeypot filter vs `chunk_index` + `is_honeypot=false` | **F4** |
| Batch claim vs quote vs `CandidateWorkers` | quote uses `claimableWorkerPredicateSQL`; `CandidateWorkers` (`control/benchmark.go:44-58`) does **not** (no sandbox, no device bind). Push *selection* is advisory; insert applies containment. | Selection noise, not a grant, except F1's weaker independence. |
| Batch claim vs realtime vs lease | **F3** | |
| `StartTask` ordinary redundancy vs hedged | C4 | |
| Measurement `claim_narrowing.go` vs production claim | measurement `pass_ready` does not include redundancy independence; production SQL does. Observability only. | Not a grant. |
| Placement v4 in measurement vs production | production lists `'','1','2','4'`; measurement omits `'4'`. Cardinality undercount, not a grant. | |

### Shape 4 — missing / NULL / unparseable yields permission

| Area | Looked | Result |
|---|---|---|
| `independentRedundancyMatch` len≤1 | **F4** | |
| LoRA nil account | **F6** | |
| `buyerSupplierLinkSignals` `ErrNoRows` | **F7** | |
| `COALESCE(t.is_redundancy,false)` | NULL flag skips independence. Column is nullable (`BOOLEAN DEFAULT false`, no `NOT NULL`). A mis-flagged check copy would skip the gate. Writers set the flag; class trigger derives `REDUNDANT` from it. Not a live grant without a bad insert. | |
| Empty frozen engine/build on hedged pin | skip that column; hw_class still required. C1 | |
| `excluded_until IS NULL` | C5 | |
| `max_usd IS NULL` | C7 | |
| `placement version < 3` | C8 | |
| `eta_secs IS NULL` on cheaper-ask | C6, fail **closed** (do not defer) | |
| `COALESCE(sandboxed,false)` | C13, fail **closed** for ordinary work | |
| Device fingerprint NULL/blank | `workerDeviceBoundSQL` EXISTS fails; ordinary work refused. Fail closed | |
| `dispatchTiebreak` `ErrNoSupply` | C12 | |
| `CandidateWorkers` missing containment | cannot by itself pin: insert still runs `workerJobContainmentSQL` | |
| Money: `validateVerificationSettlementTx` | refuses unless planned entries match recomputed observed-output amounts. Fail closed | |
| Money: `clawbackTaskCreditTx` `ErrNoRows` | no credit row → no clawback (nothing to reverse). Not a grant of a new credit | |
| Contribution missing `job_risk_reserves` | `RiskCanonical` stays false; TrueNet not published. Fail closed on the "true net" claim | |

---

## Already closed (template, not a remaining defect)

Pull `claimTaskSQL` (`control/scheduler.go:1049-1120`) now:

- applies prior-executor / current-holder exclusion to every
  `COALESCE(is_redundancy,false)` row, not only `hedged_from IS NOT NULL`;
- includes `claimed_by` joined to `workers.supplier_id`;
- keeps frozen-class pinning on hedged rows only;
- is spliced into **both** claim branches.

`control/redundancy_independence_claim_test.go` locks that SQL text and
the sequential same-worker / same-supplier refusals. It does not lock F1
(push insert), F2 (two concurrent workers), or F3 (realtime/lease).

---

## Verify

### Behaviour unchanged

This audit wrote `evidence/invariant-gate-audit.md` only. No tracked file
was edited. `control/scheduler.go` was not touched.

### `cd control && go build ./...`

Working tree is a sparse checkout. `go build ./...` exits **1**:

```
realtime_profiles.go:62:12: pattern runtime-profiles/*.json: no matching files found
```

That is a missing sparse root (`control/runtime-profiles/*.json`), not a
code change. `git sparse-checkout add` is forbidden in this sandbox. The
build is a blocker for a compile proof, not evidence of a behaviour
change.

### `git status --short`

See the completion report for the live `git status --short` paste after
this file is written.
