# Backend alpha closeout

Companion to `docs/BACKEND_ALPHA_CONTRACT.md`, which defines the alpha. This
document says where that alpha actually stands, what it took to get here, and
what is genuinely left.

Everything below is derived from `python3 scripts/validate-readiness.py`,
`ops/backend-alpha-gates.json`, `ops/go-no-go.json` and the receipts they cite.
Where a claim came from driving the live plane rather than from a local test,
this document says so, because the difference turned out to matter more than
once.

Numbers in this file were taken at tree HEAD `ca6a6d3a` against the live plane
that answers `https://mercmerc.net`. That plane serves commit `0ffbd52d`, which
is an ancestor of HEAD, not HEAD itself (seven commits behind). If a later
sentence disagrees with the validator or with `/version`, the validator and
`/version` win.

## Posture

| | |
|---|---|
| **Backend alpha** | NOT READY — `ALPHA_ENGINEERING_READY: NO_GO`. Conditions 4–6 PASS locally and are unproven on live staging; 7 PARTIAL; 14 FAIL. The open `ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`. |
| **Stripe test mode** | proven up to the Connect platform-profile wall. Connect is signed up; the remainder is the Connect platform profile, a Canadian test connected account, a `connect=true` webhook, and a passing matrix — not one dashboard click. |
| **Controlled staging** | READY — public TLS, live commit `0ffbd52d`, `/readyz` 200, `payment_mode=test`, real Postgres and object storage |
| **Security** | public-hostname rehearsal 265 executed against `https://mercmerc.net` / 0 findings, `kind=external_staging_attack_rehearsal`, `qualification=EXTERNAL`, `status=PASS`. Validator `security: 15/15`. Named human reviewer is honestly unmet and is a `PUBLIC_LAUNCH` obligation, not an alpha point. |
| **Recovery** | 11/11 failure modes PASS; offsite restore proven |
| **CLI/TUI** | next product arc, not an alpha prerequisite |
| **Website** | port 443 is public and serving, so the two gates rescoped on "no public website" came back into alpha scope — the denominator moved 91 → 94; `website_and_buyer_usability` is 2/2 |
| **Live money** | `NO_GO_PROHIBITED` and unchanged |

Scores: **Level B 88/100** (threshold 95, P0=0, P1=5, `NO_GO`) ·
**backend alpha 88/94**, `ALPHA_BLOCKER_P1=1`,
`ALPHA_ENGINEERING_READY: NO_GO`, `EXTERNAL_ALPHA_PROVEN: NO_GO`.
The 87 → 88 step is the named-reviewer requirement moving to `PUBLIC_LAUNCH`
while the executed public rehearsal stayed the alpha security point. P1 count
did not change.

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

45 gates are classified, each with a named reachable harm: 30 `ALPHA_BLOCKER`,
7 `ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 1 `POST_ALPHA`. 8 of 45 sit outside alpha
scope. The 45th gate is `named_reviewer:staging-attack-rehearsal`, added as
`PUBLIC_LAUNCH` rather than as a deletion of the reviewer requirement. The
24-hour soak remains the one `POST_ALPHA` duration bar.

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
no longer empty — four later commits touched `control/` (`9ba9884e`,
`e37a25c1`, `0ffbd52d`, `77b36ea9`) — so that landing is not the last
measurement of the package. The same commit
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
| 1 | Current candidate boots | **PASS** — live `GET https://mercmerc.net/version` serves `0ffbd52dd7ace54e5ac620c38d724af4fb2e7c10` (`go1.26.6`, `modified=false`, `build_date=2026-08-17T20:24:26Z`); `GET /readyz` is 200, `status=ready`, `payment_mode=test`, `live_value_movement=false`. Tree HEAD `ca6a6d3aa13d05e429c66c271645e9ee11ea95d7` is seven commits later and is not what the plane serves. The previous closeout pin `9e31c65b` @ `sha256:e0b64222` is a prior candidate, not the live one. `evidence/external/head-rebuild-redeploy.json` records this live commit with image `sha256:5c33d078c71a8e42a9a2c2cbaa5bf4722195423c65a1f13647684f8e9fa50253`. `evidence/external/staging-alpha-readiness.json` still records `9e31c65b` and is not a measurement of the digest that answers today. |
| 2 | Deployment reproducible | **PASS** — this live candidate was rolled back to `aa1f189e` @ `sha256:b6563659` (`2026-08-17T20:27:45Z`) and forwarded to `0ffbd52d` @ `sha256:5c33d078` (`20:27:56Z`); both `/readyz` 200, data-plane markers kept (`evidence/external/head-rebuild-redeploy.json`). An earlier rollback/forward of `19fe0b23` @ `sha256:245dc92a` → `9e31c65b` @ `sha256:e0b64222` remains on disk as a previous cycle. |
| 3 | Real Postgres and object storage | **PASS** — both healthy behind the live plane (`/readyz` `status=ready`; markers survived the rollback/forward above) |
| 4 | Buyer execution | **PASS locally / unproven on live** — `TestL12RoutableInferLoopThroughThePublicAPI` submits, prices and claims through `Routes()`, 104s (`8142e6e5`). Local L12 buyer receipt `status=PASS`. Live staging rehearsal `status=BLOCKED` (observed against then-deployed `19fe0b23`): quote 400 when nothing is advertised. |
| 5 | Supplier execution | **PASS locally / unproven on live** — worker registers advertising `candle-metal-llama1-infer`, claims and executes. Same live-staging BLOCKED: canary allowlist still pins build hash `f4303a751ca2b2af` while the sealed identity is `7cc01c442c7f6dbe`; the host binary emits `2939a8e26ffe6fd2`. |
| 6 | Verification | **PASS locally / unproven on live** — accept AND reject both proven locally; a corrupted result is refused. Live rehearsal did not run verification. |
| 7 | Money state machine, Stripe test mode | **PARTIAL** — `evidence/external/stripe-sandbox-matrix.json` is `status=BLOCKED`, `provider_mode=test`, `live_mode=PROHIBITED`. The validator still prints `blocker=connect_platform_not_signed_up` because the Connect writer hardcodes that id. The live Stripe error on `POST /v1/accounts` is no longer signup: `connected_account_creation` is `http=400 Please review the responsibilities of collecting requirements for connected accounts at https://dashboard.stripe.com/settings/connect/platform-profile.` Authorization, capture, refunds and the refusal set are proven with real fixture IDs. Transfer, payout hold/release/failure/reversal and `connect=true` delivery are not. |
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority)
| 8 | Identity/device binding | **PASS** — foreign worker UUID 403, revoked credential stops working |
| 9 | Containment | **PASS** — local security suite, containment class |
| 10 | Authority corruption fails closed | **PASS** — local security suite, authority class |
| 11 | Rollback/recovery | **PASS** — `evidence/recovery/suite.json` 11/11 modes PASS (`go_test_exit_code=0`); offsite restore across a real provider boundary |
| 12 | Security suite green | **PASS** — `evidence/external/staging-attack-rehearsal.json` is `kind=external_staging_attack_rehearsal`, `qualification=EXTERNAL`, `status=PASS`, `surface=persistent_staging_tls`, `observations.attacks_executed=265`, `finding_rows=[]`. `honesty.staging_droplet_http_client=true`. `validate-readiness.py` prints `security: 15/15`. Named review stays unmet (`reviewer.name` and `reviewer.organization` empty) and is printed as `public_launch open named_reviewer:staging-attack-rehearsal` — "requirement kept; not an alpha point". An earlier revision of this file described that same path as `kind=local_alpha_security_rehearsal` / 1551 executed / `CHECK_FAILED → 0/1`. That was the previous contents of the file, not the current one. The 1551 local Routes() count is not what scores the external point. |
| 13 | Evidence binds to current candidate | **PASS** of the binding validator — `python3 scripts/validate-evidence-binding.py` exits 0 (BOUND 88 / UNBOUND 132 / SUPERSEDED 6 / WITHDRAWN 8). That is not the same as "every receipt measured the live digest": the 3600s soak is bound to `a5bca8c0`, the public rehearsal recorded `7fe48960`, the recovery suite completed `2026-08-17T00:44:10Z`. `staging-alpha-readiness.json` is BOUND to `9e31c65b`, not to live `0ffbd52d`. `head-rebuild-redeploy.json` is BOUND to live `0ffbd52d`. |
| 14 | No known P0/P1 defect | **FAIL** — `validate-readiness.py` prints `P0=0, P1=5`. The open `ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`. Four further P1s remain open at other classifications (table above). A green `control` suite, even if still true, does not close those P1s. An earlier sentence in this file that said "zero P1" was false. |
| 15 | Remaining blockers outside alpha scope | **PASS** — `ops/backend-alpha-gates.json`, 45 gates classified with a named reachable harm each (30 `ALPHA_BLOCKER`, 7 `ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 1 `POST_ALPHA`) |

## Open — and why each is real

**Stripe Connect is signed up; the wall is the Connect platform profile.**
`POST /v1/accounts` on test account `acct_1TxbzMCwPLrR4vaY` no longer returns
the signup refusal. It returns: *"Please review the responsibilities of
collecting requirements for connected accounts at
https://dashboard.stripe.com/settings/connect/platform-profile."* The
Connect writer still stamps `blocker.id=connect_platform_not_signed_up`, and
the validator still prints that string; the scenario detail is the live error.
`GET /v1/accounts` is 200 with zero connected accounts (none synthesized).
Platform `payouts_enabled` is true. The Connect webhook still has
`connect=null`; a `connect=true` recreate probe still comes back
`connect=None`. Reachable harm: without a connected account there is no
supplier payout path at all, so the supplier half of the money state machine
cannot be exercised. `P1-STRIPE-TEST` stays open until all of this is done:

1. Complete the Connect platform profile at
   `https://dashboard.stripe.com/settings/connect/platform-profile`.
2. Create a Canadian test connected account.
3. Recreate the Connect webhook with `connect=true` (currently `connect=null`).
4. Re-run `make stripe-matrix` until `stripe_sandbox_matrix_proven` accepts a
   PASS with a real `tr_` and payout hold / release / failure / reversal.

`ops/go-no-go.json` still describes the remainder as Connect signup. That
exit_criterion text is stale against the receipt; the validator is the
tie-break and still awards 0/6 until the matrix is PASS. It is more than one
dashboard action. It unblocks seven named scenarios: connected account
creation, transfers, payout hold, manual release, payout failure, Connect
restriction/capability events, and `connect=true` webhook delivery.

**The execution loop is not closed on the live plane.** The L12 live rehearsal
(`evidence/canary/l12-p1-canary-rehearsal-live-staging.json`, `status=BLOCKED`,
(unbound rehearsal record — cited as subject, not authority; it does not prove the loop ran on staging)
then-deployed `19fe0b23`) did not run buyer, supplier, verification or
settlement. A later close-loop drive against live `0ffbd52d`
(`evidence/canary/s4-staging-close-loop-summary.json`, `status=PARTIAL`,
`external_alpha_proven=false`, operator-controlled) still refused every
stage; quote stopped at HTTP 401 `missing or malformed Authorization bearer
token`. `candle-metal-llama1-infer` does bind locally; three sibling candle
cells do not, for concrete evidence defects (an empty `engine_build_hash`, a
`merc_source_commit` of `working-tree-before-media-authority`, and a missing
`merc_source_commit`). Live still pins canary hash `f4303a751ca2b2af` against
sealed identity `7cc01c442c7f6dbe` (`head-rebuild-redeploy.json`
`host_env_build_hash` vs compose overlay). Reachable harm: without this,
buyer execution, supplier execution, verification and settlement are unproven
on the plane this closeout calls READY — the four conditions that say the
network works. Local L12 PASS does not flip `P1-CANARY-REHEARSAL` or
`EXTERNAL_ALPHA_PROVEN`. This document does not claim live `0ffbd52d` closed
that loop.

The derived soak is **done on a previous candidate**, not on the live digest:
3600 seconds against the persistent plane at `a5bca8c0`,
2026-08-16T23:58:47Z to 2026-08-17T00:58:47Z, 121 samples at 30-second
intervals, every sample carrying that commit and `payment_mode=test`. Wall
clock equals requested equals actual, and the receipt sets
`qualifies_for_24h_gate: false` — it does not pretend to be the 24-hour gate,
which stays unearned at Level B and C. It has not been re-run against
`0ffbd52d`.

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
                          7 PARTIAL (Connect platform profile)
                          14 FAIL (P0=0, P1=5)
STRIPE TEST MODE          PROVEN to the Connect platform-profile wall
CONTROLLED STAGING        READY — 0ffbd52d, payment_mode=test
SECURITY                  EXTERNAL REHEARSAL PASS (265 executed, 0 findings)
                          validator security 15/15
                          named reviewer PUBLIC_LAUNCH unmet
RECOVERY                  11/11 modes PASS; offsite restore proven
CLI/TUI                   NEXT PRODUCT ARC
WEBSITE                   IN ALPHA (2/2; public TLS brought the gates back)
LIVE MONEY                HELD — NO_GO_PROHIBITED, unchanged
```

The single remaining **alpha-blocker P1** in the readiness model is
`P1-STRIPE-TEST`. That is not the same as "the alpha is ready except for one
dashboard action." The live execution loop is still BLOCKED (`P1-CANARY-REHEARSAL`,
`ALPHA_CONTROL`), and condition 14 is FAIL for as long as any of the five P1s
is open.

## What the audit changed

An adversarial lane was pointed at this document's own claims and returned twelve
findings (`docs/CLOSEOUT_AUDIT.md`). Three mattered enough to move the bar:

**Two gates were rescoped on a premise that stopped being true.** They were moved
out of alpha because "this alpha has no public website". Then port 443 was opened
and `mercmerc.net` began serving public TLS on buyer routes. The gates are back;
backend alpha went 85/91 → 87/94. The named-reviewer requirement then moved to
`PUBLIC_LAUNCH` and the executed public rehearsal scored, so 87/94 → 88/94 and
`security: 15/15`. `staging-attack-rehearsal` is no longer an open
`ALPHA_BLOCKER` receipt. The only open alpha-blocker receipt is Connect
(`stripe-sandbox-matrix.json`). An earlier revision of this file that called
the rehearsal a second open `ALPHA_BLOCKER` alongside Connect was describing
`security: 14/15`, not the current validator print.

**A test was made to pass by removing its requirement.** The L2 Stripe matrix had
its `t.Fatal("dashboard webhook secrets required")` replaced with synthetic
secrets, and the same commit moved condition 14 to PASS. The requirement is
restored, and then actually met: `make test-money-contract` supplies the real
dashboard pair and a real `sk_test_` key and passes. The plain suite keeps two
expected failures, because without credentials the production webhook contract
genuinely is not proven.

**The tree described this hostname as live-money.** `live-cutover.json` recorded
`stripe_mode: "live"` in the present tense while `/readyz` answers
`payment_mode=test`. Reframed as historical; the measurement was not altered.

## Why the loop still does not run on staging

Both refusals that stood between local success and staging success are fixed and
deployed. `https://mercmerc.net` serves `0ffbd52d` (an ancestor of tree HEAD
`ca6a6d3a`, not HEAD itself), `/readyz` is 200, `/pricing/board.json` is 200
(it was 503 — the pricing document seed named the superseded r4 promotion
receipt while the cell is resealed to r6), and real Stripe test-mode events
return 200 with zero 500s. An earlier sentence in this file that said the
hostname "serves current HEAD" was true only while HEAD was `0ffbd52d`.

What remains is not those fixes:

**No allowlisted buyer bearer is available to this process.** One unrevoked
session exists, but Caddy redacts `Authorization`, so it cannot be recovered from
access logs, and reading `.merc-secrets.env` is prohibited. This is an operator
credential, not an engineering gap.

**Only one supplier can be admitted.** `max_active_workers=1`, and that worker's
supplier is owned by the buyer, so a claim refuses `BUYER_SUPPLIER_LINKED` and
redundancy refuses `NO_INDEPENDENT_SUPPLIER`.

The second one exposed a hole in a decision made during this closeout. Canary was
allowed to skip the heterogeneous honeypot for operator-controlled participants,
on the argument that redundancy detects the same defect and fails only to
collusion. It also fails to having nobody to be redundant with — and with one
admissible supplier the honeypot was being skipped while redundancy could never
produce an independent vote, which trades a real control for nothing and only
notices at verification, after the work is done.

The skip now additionally requires enough admissible independent suppliers for
redundancy to be independent, with the floor derived from what
`control/verification.go` actually enforces. One supplier refuses at quote,
before any write, naming the reason. So staging will 503 on quote until a second
operator-reserved worker is admitted — the intended refusal, and an ops decision
rather than something to fix by editing the allowlist.

## Control suite

Exactly two expected money-contract failures:
`TestL2StripeWebhookMatrixRequiresDashboardSecrets` and
`TestL2StripeWebhookMatrixAgainstRealHandlers`. Both exist because an audit
found the production webhook contract had been made to pass with synthetic
secrets, and the requirement was restored. They pass under
`make test-money-contract`, which supplies the real dashboard pair and a real
`sk_test_` key and refuses a non-test key or identical secrets. Supplying those
credentials suite-wide is wrong and the suite proves it: one test carries its
own configured credential and rejects a different one, and a guard panics
rather than run unhardened verification sampling with money rails present.

`make ci` also runs `TestFirstCompleteLoopThroughThePublicAPI` once
`cargo build --release` has produced `merc-agent`. On this host that test
failed enrolment: the agent is a real M3 Ultra and the advertised cell is the
TEST fixture (`apple_silicon_pro` / `feedfacefeedface`). That is not a close
of conditions 4–6, and it is not a reason to delete the test.

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
