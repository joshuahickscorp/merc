# Backend alpha closeout

Companion to `docs/BACKEND_ALPHA_CONTRACT.md`, which defines the alpha. This
document says where that alpha actually stands, what it took to get here, and
what is genuinely left.

Everything below is derived from `python3 scripts/validate-readiness.py`,
`ops/backend-alpha-gates.json`, `ops/go-no-go.json` and the receipts they cite.
Where a claim came from driving the live plane rather than from a local test,
this document says so, because the difference turned out to matter more than
once.

## Posture

| | |
|---|---|
| **Backend alpha** | not yet READY — see the open blockers below |
| **Stripe test mode** | proven up to the Connect boundary; the remainder is one dashboard action |
| **Controlled staging** | READY — public TLS, current candidate, real Postgres and object storage |
| **Security** | ALPHA SUFFICIENT — 1551 attacks, zero findings |
| **Recovery** | ALPHA SUFFICIENT — 11/11 failure modes, offsite restore proven |
| **CLI/TUI** | next product arc, not an alpha prerequisite |
| **Website** | NOT REQUIRED, classified `PUBLIC_LAUNCH` |
| **Live money** | `NO_GO_PROHIBITED` and unchanged |

Scores: **Level B 87/100** (threshold 95, P1=6, `NO_GO`) ·
**backend alpha 85/91**, `ALPHA_ENGINEERING_READY: NO_GO`,
`EXTERNAL_ALPHA_PROVEN: NO_GO`. Open P1s fell from eight to six when
`P1-STAGING` and `P1-RECOVERY-SOAK` were satisfied by evidence, not by
reclassification.

## What the rescoping did and did not do

44 gates were classified, each with a named reachable harm: 28 `ALPHA_BLOCKER`,
7 `ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 2 `POST_ALPHA`. Only 9 of 44 moved out of
alpha scope.

**Nothing was deleted.** Level B still derives against the full 100-point bar,
`go_threshold` is still 95, and Level C is untouched. A gate rescoped to
`PUBLIC_LAUNCH` keeps its full requirement at that level. The reclassification
by itself closed **zero** P1s: all eight were still open afterwards, and the two
that have since closed did so on evidence.

Two gates were questioned and deliberately **kept** as alpha blockers, against
the easier answer: offsite backup/restore ("a restore path unproven at alpha is
unproven when the droplet is gone"), and the recovery soak — where the arbitrary
24-hour number was replaced not by nothing but by a re-derivation from 16 named
time-dependent mechanisms, the binding one being `pgxpool MaxConnLifetime = 30m`,
giving a 3600-second requirement that samples a live pool recycle on both sides.
The 24-hour receipt remains unearned at Level B and C.

## Defects found by driving the live plane

These are the ones worth recording, because no local test could have caught
them. Every suite on this machine was green while all three were true.

**Every real Stripe webhook was being refused.** The control plane pins
`stripeAPIVersion = "2025-06-30.basil"`; the Stripe account delivers
`2026-06-24.dahlia`; `validateStripeEventContract` refuses any mismatch. The
endpoints were recreated pinned to the compiled version. Real test-mode events
now return 200 over the internet.

**Two cash events were never going to arrive.** The billing endpoint's
subscription omitted `setup_intent.succeeded` and both `charge.dispute.funds_*`
events. Those have a non-zero `EffectRank` in `stripe_cash_events.go` and apply
to the ledger. Stripe would never have sent them, the handler would never have
run, and nothing was red — every local test constructs its own event.
`make stripe-endpoint-subscriptions` now derives the expected set from the Go
handlers and refuses any endpoint that is not subscribed to all of it. It is
proven to fail, not merely to pass.

**Unknown-customer events asked Stripe to retry forever.** A payment-method
event for a customer this deployment has never seen answered 500, which builds a
permanently failing delivery queue for something that can never succeed. Unknown
customer is now acknowledged; a real database fault still answers 500.

One more was found by reading rather than running: the staging-without-Connect
exception in `validateCanaryMoneyMode` returned before the webhook-secret
distinctness check. Not requiring a `ca_*` platform ID there is correct; not
requiring distinct secrets is not, because the Connect route is served whether
or not Connect is onboarded. Fixed, with a regression test.

## Closeout standard

| # | Condition | Status |
|---|---|---|
| 1 | Current candidate boots | **PASS** — `a5bca8c0` @ `sha256:2b2f85c9` serves `/readyz` 200; note the tree has since taken the go1.26.6 bump, so a rebuild is due before the deployed image is byte-current |
| 2 | Deployment reproducible | **PASS** — rollback to `7bf0ab9d` and forward to `2b2f85c9`, both observed with timestamps |
| 3 | Real Postgres and object storage | **PASS** — both healthy behind the live plane |
| 4 | Buyer execution | see "Open" below |
| 5 | Supplier execution | see "Open" below |
| 6 | Verification | see "Open" below |
| 7 | Money state machine, Stripe test mode | **PARTIAL** — everything not gated on Connect proven with real fixture IDs; refusals all fire |
| 8 | Identity/device binding | **PASS** — foreign worker UUID 403, revoked credential stops working |
| 9 | Containment | **PASS** — security suite, containment class |
| 10 | Authority corruption fails closed | **PASS** — security suite, authority class |
| 11 | Rollback/recovery | **PASS** — 11/11 modes, offsite restore across a real provider boundary |
| 12 | Security suite green | **PASS** — 1551 attacks, zero findings |
| 13 | Evidence binds to current candidate | **PASS** for the suites re-run at this commit |
| 14 | No known P0/P1 defect | **PASS** — zero P0, zero P1; the one P2 was closed by go1.26.6 |
| 15 | Remaining blockers outside alpha scope | see "Open" below |

## Open — and why each is real

**Stripe Connect is not signed up.** Stripe itself refuses:
*"You can only create new accounts if you've signed up for Connect."* Reachable
harm: without Connect there is no supplier payout path at all, so the supplier
half of the money state machine cannot be exercised. This is one dashboard
action by the account owner at `https://dashboard.stripe.com/connect` for test
account `acct_1TxbzMCwPLrR4vaY`. It unblocks seven named scenarios: connected
account creation, transfers, payout hold, manual release, payout failure,
Connect restriction/capability events, and `connect=true` webhook delivery. The
Connect endpoint also needs recreating with `connect=true`, which Stripe
currently returns as null.

**The execution loop is not closed.** `POST /v1/quote` and `POST /v1/jobs`
return 400 because no runtime cell is advertised by a registered worker.
`candle-metal-llama1-infer` does bind; three sibling candle cells do not, for
concrete evidence defects (an empty `engine_build_hash`, a `merc_source_commit`
of `working-tree-before-media-authority`, and a missing `merc_source_commit`).
Reachable harm: without this, buyer execution, supplier execution, verification
and settlement are unproven — the four conditions that say the network works.

The derived soak is **done**: 3600 seconds against the persistent plane on the
deployed candidate, 2026-08-16T23:58:47Z to 2026-08-17T00:58:47Z, 121 samples at
30-second intervals, every sample carrying the deployed commit and
`payment_mode=test`. Wall clock equals requested equals actual, and the receipt
sets `qualifies_for_24h_gate: false` — it does not pretend to be the 24-hour
gate, which stays unearned at Level B and C.

## Live money — the actual remaining inputs

Do not perform any of this without explicit operator authorization. Live money
is `NO_GO_PROHIBITED` and every check above enforces test mode.

The transition is not vague. It is:

1. Stripe live account eligibility — identity, bank account, Connect KYC.
2. Live keys and live Connect configuration.
3. Live webhook endpoints and their secrets, **pinned to the compiled
   `stripeAPIVersion`**, and subscribed to every handled cash event. Both of
   those are now enforced by checks rather than by memory, because both were
   wrong in test mode first.
4. A signed live-payment activation conforming to
   `ops/live-payment-activation.schema.json`: HMAC-SHA256, bound to a
   40-character candidate commit, `environment: production`, per-transaction
   caps for charge, payout, refund and reversal, an aggregate cap reference, an
   expiry and a recovery expiry, and exactly three named approvals
   (`payments`, `release_manager`, `security`).
5. A controlled first transaction.

## What this alpha still does not claim

`EXTERNAL_ALPHA_PROVEN` is false and no receipt implies otherwise. Every
participant exercised so far is operator-controlled or synthetic. Governance
documents are drafts marked *DRAFT · INTERNAL · NOT LEGAL ADVICE*; no human has
approved anything and no approval receipt was minted. Invitee emails are
personal information and PIPEDA or GDPR can attach — that is a real obligation,
not a cleared one. Catalogue models, fonts and visual assets remain
release-blocking, and `make license-register` still fails on the priced Llama
row rather than being silenced.
