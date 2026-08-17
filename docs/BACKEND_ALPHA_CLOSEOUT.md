# Backend alpha closeout

Companion to `docs/BACKEND_ALPHA_CONTRACT.md`, which defines the alpha. This
document says where that alpha actually stands, what it took to get here, and
what is genuinely left.

Everything below is derived from `python3 scripts/validate-readiness.py`,
`ops/backend-alpha-gates.json`, `ops/go-no-go.json` and the receipts they cite.
Where a claim came from driving the live plane rather than from a local test,
this document says so, because the difference turned out to matter more than
once.

Numbers in this file were taken at tree HEAD `b6dd531e` against the live plane
that answers `https://mercmerc.net`. That plane serves commit `9e31c65b`, which
is an ancestor of HEAD, not HEAD itself. If a later sentence disagrees with the
validator or with `/version`, the validator and `/version` win.

## Posture

| | |
|---|---|
| **Backend alpha** | NOT READY — `ALPHA_ENGINEERING_READY: NO_GO`. Conditions 4–6 PASS locally and are unproven on live staging; 7 PARTIAL; 14 FAIL. The open `ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`. |
| **Stripe test mode** | proven up to the Connect boundary. The remainder is Connect signup, a Canadian test connected account, a `connect=true` webhook, and a passing matrix — not one dashboard click. |
| **Controlled staging** | READY — public TLS, live commit `9e31c65b`, `/readyz` 200, `payment_mode=test`, real Postgres and object storage |
| **Security** | local suite PASS — 1551 executed, 0 findings, `qualification: LOCAL`. The validator still scores security 14/15 (`staging-attack-rehearsal` `CHECK_FAILED`). |
| **Recovery** | 11/11 failure modes PASS; offsite restore proven |
| **CLI/TUI** | next product arc, not an alpha prerequisite |
| **Website** | NOT REQUIRED for this alpha, classified `PUBLIC_LAUNCH` |
| **Live money** | `NO_GO_PROHIBITED` and unchanged |

Scores: **Level B 87/100** (threshold 95, P0=0, P1=5, `NO_GO`) ·
**backend alpha 85/91**, `ALPHA_BLOCKER_P1=1`,
`ALPHA_ENGINEERING_READY: NO_GO`, `EXTERNAL_ALPHA_PROVEN: NO_GO`.

Open P1s fell from eight to five when `P1-STAGING`, `P1-RECOVERY-SOAK` and
`P1-OFFSITE-RESTORE` were satisfied by evidence, not by reclassification. The
five that remain:

| id | classification | role |
|---|---|---|
| `P1-STRIPE-TEST` | `ALPHA_BLOCKER` | Connect-complete Stripe test-mode matrix |
| `P1-CANARY-REHEARSAL` | `ALPHA_CONTROL` | counted buyer/supplier/verification loop |
| `P1-ALERT-DELIVERY` | `PUBLIC_LAUNCH` | staffed paging receiver |
| `P1-INDEPENDENT-APPROVAL` | `PUBLIC_LAUNCH` | named non-author reviewer |
| `P1-GOVERNANCE` | `PUBLIC_LAUNCH` | eight qualified approvals |

An earlier revision of this file said `P1=6` and "fell from eight to six" after
only two of those three closes. That was an off-by-one. The validator prints
`P1=5`; `ops/go-no-go.json` `open_p1` has five ids; `ops/readiness.json`
`target_scope_open_p1` is 5.

## What the rescoping did and did not do

44 gates were classified, each with a named reachable harm: 28 `ALPHA_BLOCKER`,
7 `ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 2 `POST_ALPHA`. Only 9 of 44 moved out of
alpha scope.

**Nothing was deleted.** Level B still derives against the full 100-point bar,
`go_threshold` is still 95, and Level C is untouched. A gate rescoped to
`PUBLIC_LAUNCH` keeps its full requirement at that level. The reclassification
by itself closed **zero** P1s: all eight were still open afterwards, and the
three that have since closed did so on evidence.

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
now return 200 over the internet. Live `/readyz` still reports
`stripe_api_version=2025-06-30.basil`.

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

## Defects found by making `make ci` complete

`make ci` had never run to completion during this closeout. Every earlier
attempt ran in a sparse worktree, and its failures were artifacts of the
checkout rather than facts about the product — one lane correctly refused to
report against such a tree at all. Running it on a full tree found three more.

**Fixtures were lying about currency, and it looked like two defects.** Eleven
currency tests and six scheduler tests were failing. The job and economic-plan
fixtures insert rows without a currency column, falling through to the schema's
`DEFAULT 'usd'`, while the platform settles CAD — the Stripe account is CA/CAD.
So `job_economic_plans_json_currency_bound` fires correctly, and the claim
currency filter correctly refuses to let a CAD worker claim a usd-default job.
The "SLA deferral starvation" is that filter working, not a broken deferral
bound. The product is right both times; the fixtures are wrong.

A fix for this was written and **reverted**, because it was a net loss and the
numbers say so plainly:

| tree | control-suite failures |
|---|---|
| baseline | 25 |
| with the currency fix | 80 — fixed 17, broke 73 |
| reverting only its `usd`→`cad` test default | 77 |
| reverting the whole change | 27 |
| **the fix at the right radius** | **9** |

The obvious hypothesis — that the damage was the package-wide default flip — read
as wrong when tested alone: reverting that line recovered three tests, not
seventy-three, which pointed the blame at the fixture writes. That reading was
incomplete. `installSettlementCurrencyForTest` is **process-global**, so pinning
CAD inside the six `TestClaimTasksTx*` also requires dropping `t.Parallel()` on
them — otherwise they flip settlement under whatever else is running, and the
default revert cannot show its true effect.

With that correction the change is a clear win: the package default stays `usd`,
the six claim tests pin CAD and run serially, the three USD-platform Stripe
preflight tests pin USD, and the fixtures keep writing the settlement currency
explicitly. That landing (`86a010cb`) took the suite from 25 failures to 9, and
none of the nine was the currency defect.

Those nine were later closed at `8142e6e5`, which reported `ok merc/control`, 0
failures, against the compose Postgres. `git log 8142e6e5..HEAD -- control/` is
empty, so that is still the last measurement of the package. The same commit
narrowed the L2 Stripe matrix so missing dashboard secrets install synthetic
`whsec_` values and the test continues — a smaller claim than "this process
holds the staging webhook pair", and not a close of `P1-STRIPE-TEST`. That
commit then marked closeout condition 14 PASS and wrote "zero P1". The suite
going green does not make `P1=5` into `P1=0`. Condition 14 stays FAIL.

**26 test failures were the wrong database.** The dev Postgres ran with the
image default `max_locks_per_transaction = 64`, which cannot hold a schema apply
per isolated database, so 26 tests failed with `out of shared memory` for
reasons unrelated to the code — and hid the real failures behind them. Raising
it to 512 takes that count to zero. Worth recording separately: this machine has
**two** Postgres on 5432 — the compose container via OrbStack, and a Homebrew
`postgresql@17` on `[::1]` — and both `localhost` and `127.0.0.1` reach the
Homebrew one. Every test run in this closeout until that was noticed had been
pointed at the wrong database.

## Closeout standard

| # | Condition | Status |
|---|---|---|
| 1 | Current candidate boots | **PASS** — live `GET https://mercmerc.net/version` serves `9e31c65b27860d659d7ce972e2de7052691c0642` (`go1.26.6`, `modified=false`); `GET /readyz` is 200, `status=ready`, `payment_mode=test`, `live_value_movement=false`. Tree HEAD `b6dd531e` is two commits later and is not what the plane serves. The previous closeout pin `a5bca8c0` @ `sha256:2b2f85c9` is a prior candidate, not the live one. The staging-readiness receipt that records this same live commit also records image `sha256:e0b642220dcd195c84290466cdaf90c2083c8740a87e6c3166d5817683f59fd3`. |
| 2 | Deployment reproducible | **PASS** — this live candidate was rolled back to `19fe0b23` @ `sha256:245dc92a` (`2026-08-17T15:35:05Z`) and forwarded to `9e31c65b` @ `sha256:e0b64222` (`15:35:19Z`); both `/readyz` 200, data-plane markers kept. An earlier rollback/forward of `8283ae58` @ `sha256:7bf0ab9d` → `a5bca8c0` @ `sha256:2b2f85c9` remains on disk as a previous cycle. |
| 3 | Real Postgres and object storage | **PASS** — both healthy behind the live plane (`/readyz` `status=ready`; markers survived the rollback/forward above) |
| 4 | Buyer execution | **PASS locally / unproven on live** — `TestL12RoutableInferLoopThroughThePublicAPI` submits, prices and claims through `Routes()`, 104s (`8142e6e5`). Local L12 buyer receipt `status=PASS`. Live staging rehearsal `status=BLOCKED` (observed against then-deployed `19fe0b23`): quote 400 when nothing is advertised. |
| 5 | Supplier execution | **PASS locally / unproven on live** — worker registers advertising `candle-metal-llama1-infer`, claims and executes. Same live-staging BLOCKED: canary allowlist still pins build hash `f4303a751ca2b2af` while the sealed identity is `7cc01c442c7f6dbe`; the host binary emits `2939a8e26ffe6fd2`. |
| 6 | Verification | **PASS locally / unproven on live** — accept AND reject both proven locally; a corrupted result is refused. Live rehearsal did not run verification. |
| 7 | Money state machine, Stripe test mode | **PARTIAL** — `evidence/external/stripe-sandbox-matrix.json` is `status=BLOCKED`, `blocker=connect_platform_not_signed_up`, `provider_mode=test`, `live_mode=PROHIBITED`. Authorization, capture, refunds and the refusal set are proven with real fixture IDs. Transfer, payout hold/release/failure/reversal and `connect=true` delivery are not. |
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority)
| 8 | Identity/device binding | **PASS** — foreign worker UUID 403, revoked credential stops working |
| 9 | Containment | **PASS** — local security suite, containment class |
| 10 | Authority corruption fails closed | **PASS** — local security suite, authority class |
| 11 | Rollback/recovery | **PASS** — `evidence/recovery/suite.json` 11/11 modes PASS (`go_test_exit_code=0`); offsite restore across a real provider boundary |
| 12 | Security suite green | **PASS locally** — `evidence/external/staging-attack-rehearsal.json` is `kind=local_alpha_security_rehearsal`, `qualification=LOCAL`, `attacks_executed=1551`, `findings=[]`. It does not touch the droplet (`honesty.staging_droplet_touched=false`). `validate-readiness.py` therefore prints `security: 14/15` and `staging-attack-rehearsal.json: CHECK_FAILED → 0/1`. |
| 13 | Evidence binds to current candidate | **PASS** of the binding validator — `python3 scripts/validate-evidence-binding.py` exits 0 (BOUND 79 / UNBOUND 131 / SUPERSEDED 6 / WITHDRAWN 8). That is not the same as "every receipt measured the live digest": the 3600s soak is bound to `a5bca8c0`, the local security rehearsal recorded `73bc49da`, the recovery suite completed `2026-08-17T00:44:10Z`. `staging-alpha-readiness.json` is BOUND to live `9e31c65b`. |
| 14 | No known P0/P1 defect | **FAIL** — `validate-readiness.py` prints `P0=0, P1=5`. The open `ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`. Four further P1s remain open at other classifications (table above). A green `control` suite, even if still true, does not close those P1s. An earlier sentence in this file that said "zero P1" was false. |
| 15 | Remaining blockers outside alpha scope | **PASS** — `ops/backend-alpha-gates.json`, 44 gates classified with a named reachable harm each (28 `ALPHA_BLOCKER`, 7 `ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 2 `POST_ALPHA`) |

## Open — and why each is real

**Stripe Connect is not signed up.** Stripe itself refuses:
*"You can only create new accounts if you've signed up for Connect."* Reachable
harm: without Connect there is no supplier payout path at all, so the supplier
half of the money state machine cannot be exercised. `P1-STRIPE-TEST` stays
open until all of this is done on test account `acct_1TxbzMCwPLrR4vaY`:

1. Sign up for Connect at `https://dashboard.stripe.com/connect`.
2. Create a Canadian test connected account.
3. Recreate the Connect webhook with `connect=true` (currently `connect=null`).
4. Re-run `make stripe-matrix` until `stripe_sandbox_matrix_proven` accepts a
   PASS with a real `tr_` and payout hold / release / failure / reversal.

That is the exit criterion in `ops/go-no-go.json`. It is more than one
dashboard action. It unblocks seven named scenarios: connected account
creation, transfers, payout hold, manual release, payout failure, Connect
restriction/capability events, and `connect=true` webhook delivery.

**The execution loop is not closed on the live plane.** The last live rehearsal
(`evidence/canary/l12-p1-canary-rehearsal-live-staging.json`, `status=BLOCKED`,
(unbound rehearsal record — cited as subject, not authority; it does not prove the loop ran on staging)
then-deployed `19fe0b23`) did not run buyer, supplier, verification or
settlement. `POST /v1/quote` and `POST /v1/jobs` 400 when no runtime cell is
advertised by a registered worker. `candle-metal-llama1-infer` does bind
locally; three sibling candle cells do not, for concrete evidence defects (an
empty `engine_build_hash`, a `merc_source_commit` of
`working-tree-before-media-authority`, and a missing `merc_source_commit`).
Live also still pins canary hash `f4303a751ca2b2af` against sealed identity
`7cc01c442c7f6dbe`. Reachable harm: without this, buyer execution, supplier
execution, verification and settlement are unproven on the plane this closeout
calls READY — the four conditions that say the network works. Local L12 PASS
does not flip `P1-CANARY-REHEARSAL` or `EXTERNAL_ALPHA_PROVEN`. This document
does not claim the later live commit `9e31c65b` closed that loop.

The derived soak is **done on a previous candidate**, not on the live digest:
3600 seconds against the persistent plane at `a5bca8c0`,
2026-08-16T23:58:47Z to 2026-08-17T00:58:47Z, 121 samples at 30-second
intervals, every sample carrying that commit and `payment_mode=test`. Wall
clock equals requested equals actual, and the receipt sets
`qualifies_for_24h_gate: false` — it does not pretend to be the 24-hour gate,
which stays unearned at Level B and C. It has not been re-run against
`9e31c65b`.

Condition 14 is FAIL because five P1s are open, one of them the money-path
start-gate. It is not FAIL because nine control tests are red: that remainder
was the state at `86a010cb`, and `8142e6e5` reported it closed. The older
sentence that used those nine tests to hold 14 at FAIL, and the later sentence
that used the green suite to declare 14 PASS with zero P1, cannot both stand.
The validator is the tie-break: `P1=5`, so 14 is FAIL.

## Terminal posture

```
MERC BACKEND ALPHA        NOT READY — ALPHA_ENGINEERING_READY NO_GO
                          4-6 PASS locally / unproven on live
                          7 PARTIAL (Connect)
                          14 FAIL (P0=0, P1=5)
STRIPE TEST MODE          PROVEN to the Connect boundary
CONTROLLED STAGING        READY — 9e31c65b, payment_mode=test
SECURITY                  LOCAL SUITE PASS (1551 executed, 0 findings)
                          validator security 14/15; external rehearsal CHECK_FAILED
RECOVERY                  11/11 modes PASS; offsite restore proven
CLI/TUI                   NEXT PRODUCT ARC
WEBSITE                   NOT REQUIRED FOR ALPHA (PUBLIC_LAUNCH)
LIVE MONEY                HELD — NO_GO_PROHIBITED, unchanged
```

The single remaining **alpha-blocker P1** in the readiness model is
`P1-STRIPE-TEST`. That is not the same as "the alpha is ready except for one
dashboard action." The live execution loop is still BLOCKED (`P1-CANARY-REHEARSAL`,
`ALPHA_CONTROL`), and condition 14 is FAIL for as long as any of the five P1s
is open.

## Live money — the actual remaining inputs

Do not perform any of this without explicit operator authorization. Live money
is `NO_GO_PROHIBITED` and every check above enforces test mode. Live `/readyz`
is `payment_mode=test`, `live_value_movement=false`. A historical UNBOUND
cutover receipt in the tree that describes this hostname as Stripe live is not
the plane that answers today.

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
release-blocking, and `python3 scripts/validate-license-register.py` still
prints FAIL on the priced Llama row (`llama-3.2-1b-instruct-q4` / register row
`Llama 3.2 1B Instruct GGUF` is `BLOCKED`) rather than being silenced.
