# Money audit, 2026-07-27

Ten agents across five dimensions (loss paths, fee efficiency, margin, ledger
integrity, supplier profitability), each finding attacked by an independent
verifier instructed to refute it. What survived is below, worst first.

I re-verified findings 1 and 2 against the code myself before writing this.

---

## 1. Suppliers are paid exactly $0.00, and always will be

**Confirmed independently by three of the five audit dimensions.**

`ClaimPayout` floors each ledger entry to whole cents:

```go
cashCents = liabilityMicros / microUSDPerCent   // control/payment.go:57, integer floor
```

When the entry is worth less than a cent, `RequestedCents == 0` and the entry is
set to `payout_status='carried'` (`control/store_payouts.go:693-707`). Then:

- `PayoutCarried` has exactly **one writer** in the whole tree — that line.
- **No code anywhere moves `carried` back to `held`.**
- `DuePayouts` selects `payout_status='held'` only (`store_payouts.go:577`).

So `carried` is terminal. And at catalogue prices essentially every credit is
sub-cent: a 1000-record job bills $0.00231, the supplier's ~97% share lands at
roughly 2,244 micro-USD — **0.22 of one cent**. It takes about four such jobs to
reach a single payable cent, and the entry is carried and abandoned long before.

This is the mechanical reason supplier net is −$0.00014/hr. It is not thin
margin. **The cash never leaves the platform.**

The design intended otherwise, which is what makes this a bug rather than a
policy: the settlement policy constant is literally named
`supplierSettlementPolicyFloorCentCarryV1`, the schema stores
`remainder_microusd` per settlement (`schema.sql:1822`), and
`('payout','carried','held')` is an explicitly whitelisted transition
(`schema.sql:2188`). The carry was specified and never implemented.

**Fix (not yet written — this is a money state machine and deserves its own
change):** accumulate the supplier's outstanding remainder and add it to the
next entry's liability before flooring, then settle the folded entries. The
column and the transition already exist to support exactly this.

Until it lands, `/v1/worker/earnings` reports money to suppliers that the payout
path is structurally incapable of sending.

## 2. The 24-hour age trigger charges at $0.50, where Stripe keeps 62.9%

`BuyersDueForBatch` fires when the balance reaches `chargeMinUSD` **or** the
oldest deferred job passes 24h, gated only by `SUM >= stripeMinChargeUSD`
($0.50, `collect.go:34`).

Stripe on a $0.50 charge: `0.029 × 0.50 + 0.30 = $0.3145` — **62.9%**.

But the buyer price floor was computed assuming the fixed fee is amortised over
a $5.00 batch: `processorRate = 0.029 + 0.30/5.00 = 8.9%`
(`economic_plan.go:39-44`). The plan reports Executable with positive headroom
while the charge loses about **$0.27**.

This is not an edge case, it is the only path that fires. At board prices a
buyer accrues ~$0.0045/GPU-hour, so reaching the $5.00 batch floor takes ~1,113
GPU-hours; the $0.50 age-out is what actually happens.

`economic_plan.go:261` asserts the fee "is amortised over a minimum-size charge
batch, matching how chargeOrDeferJob and FormChargeBatch actually settle". For
this path that assertion is false.

**Fix:** require `SUM >= chargeMinUSD` on the age branch too and let sub-floor
balances roll forward — nothing is lost, the receivable is already durable in the
ledger. Or make `processorFloorTerms` use the real worst-case batch, in which
case the rate becomes 62.9% and `BuildEconomicPlan` correctly refuses.

## 3. Failed charges retry forever with no attempt cap

`ReflipNoCardJobs` returns `no_payment_method` jobs to `deferred` so they rejoin
batching. There is **no equivalent for `failed`**. `retryFailedSingle` re-charges
each failed job alone — paying a solo $0.30 instead of a shared one — and
`chargeRetryBackoff` caps the *interval* at 6h but nothing caps the *attempts*.
`IncrementChargeAttempts` is recorded and logged, never compared to a limit.

A permanently dead card produces four Stripe PaymentIntent attempts per day, per
job, forever.

## 4. The advertised 3% platform take is really ~33.5%

`BuildEconomicPlan` derives the supplier's share from *compute cost*, then
derives the buyer's price from an unrelated floor, and books the difference as
`BuyerSafetyFeePerTaskUSD` — 100% to the platform.

With the shipped schedule the buyer pays $0.00068715/task while the supplier
receives $0.00045728. Effective take: **33.5%**, not the 3% `MERC_PLATFORM_TAKE_PCT`
advertises. The quote surfaces the spread to the *buyer*; the supplier has no
equivalent view.

Passing ~10% of that safety fee to the supplier would clear electricity plus a
50% margin and cost the platform ~$0.0000216/task.

## 5. One blog-sourced price sets the entire catalogue

`repriceFromMarketBoard` prices a class as **`min()`** of its observations
(`pricing.go:167-186`). For `infer_small` the three observations are $0.01, $0.04
and $0.06 per 1M — and the $0.01 row that sets the price for every supplier cites
a competitor's marketing blog, not a vendor pricing page.

`min()` is the most fragile possible selector: one stale, promotional or
mistyped row drags the whole supply side underwater, and the only validation is
that the positioning multiplier is finite and positive.

Median instead of min moves supplier net from −$0.00014/hr to **+$0.0129/hr** —
3.87× electricity. A one-line selector choice is worth 4–6× the supplier's
entire income.

## 6. Rounding at the Stripe boundary is never reconciled

Inside the ledger the money is clean — exact micro-USD, bound as
`($micros::numeric / 1000000)` so no float touches the money column. The drift is
at the rail: `chargeBuyer` converts with `int64(math.Round(usd*100))` and
`FormChargeBatch` accumulates in `float64`. Nothing ever compares
`SUM(kind='buyer_charge')` against `buyer_cash_collections.received_cents`.

Separately, `stripe_fee` is booked negative against the buyer and never
subtracted from `platform_take`, so ledger margin overstates reality by the full
processor fee: on a $5 batch `platform_take` reads $0.15 while Stripe took $0.445.

**Local resolution, 2026-07-28:** new recorded processor fees are now allocated
to batch jobs with Hamilton's largest-remainder method at micro-USD precision.
Immutable job IDs break equal-remainder ties, so permuting the same economic
facts cannot change which job receives a rounding micro-unit. Allocation is
serialized, append-only, idempotent, and rejected if the row set is partial or
does not conserve the fee exactly. Buyer invoices and clearing receipts expose
both `processor_fee_allocated_usd` and
`platform_net_after_processor_usd`, plus the versioned allocation method; a
batch invoice fails closed if a recorded fee has not been completely allocated.
Pre-upgrade rows retain and expose `legacy_order_residual_v0` rather than being
silently rewritten. Ten thousand randomized quota,
conservation, and permutation cases plus fresh-PostgreSQL concurrent mutation
tests cover the local boundary.

This does **not** close provider reconciliation. No Stripe test object, balance
transaction, refund, dispute, payout, or real cash evidence was created in this
change, and the formal Stripe test-mode matrix remains a release blocker.

## What cannot be fixed by configuration

`MERC_PLATFORM_TAKE_PCT` is clamped to [1%, 5%]. Running the whole range: at 5%
supplier net is −$0.00023/hr, at 1% it is −$0.00005/hr. **At a literal 0% take it
is still −$0.0000061/hr.** Break-even needs 143.2 tok/s at 97% share and 138.9
even at 100%; the M3 Pro measured 138.7.

The platform could take nothing at all and the supplier would still miss their
power bill by 0.13%. This knob closes 78% of the gap and cannot close the rest,
because the rest is a hardware fact. See `docs/SPEED_LANE_2026-07-27.md` — the
M3 Ultra measurement changes this arithmetic materially.

## Landed today

The one report that detects this condition, `SupplierViabilityReport`, existed
but was called **only from a test**. It now runs at boot (`control/main.go`) and
prints, against the real catalogue:

```
WARNING: supplier economics underwater: model=llama-3.2-1b-instruct-q4
job=batch_infer hw=apple_silicon_pro gross=$0.004359/hr electricity=$0.004500/hr
net=$-0.000141/hr; break-even needs 143.2 units/s, measured 138.7
```
