# Closeout audit — adversarial

Lane K. Judge claims against artifacts and commands, not against the reports
that describe them. Nothing in this file was repaired. HEAD at audit time:
`9e31c65b27860d659d7ce972e2de7052691c0642` (one commit past the context pack's
`645bed17`). All five attack surfaces were driven.

> Every `evidence/…` path named in this audit is cited as the **subject of
> audit**, not as authority for a claim. Each is UNBOUND historical or
> rehearsal evidence and proves nothing here beyond its own contents.


This worktree is a sparse checkout. `python3 scripts/validate-readiness.py`
run here printed `FAIL: decision readiness_score 87 != receipt-derived total 11`
because `evidence/` is not on disk. The same script against the full tree at
the same HEAD (`/Users/scammermike/Downloads/merc`) is the measurement below.

## What holds (and what was run)

| Claim | Result | Command |
|---|---|---|
| Level B 87/100, P1=5, `NO_GO` | Holds | `python3 scripts/validate-readiness.py` on the full tree: `PASS (87/100 derived, P0=0, P1=5, Level B NO_GO)` |
| Backend alpha 85/91, one `ALPHA_BLOCKER` P1 = `P1-STRIPE-TEST` | Holds | Same run: `backend_alpha: 85/91`, `ALPHA_BLOCKER_P1=1`, open receipt `evidence/external/stripe-sandbox-matrix.json` (`status=BLOCKED`, `blocker=connect_platform_not_signed_up`) |
| `validate-evidence-binding.py` exits 0 | Holds on the full tree | `EXIT_B=0` (BOUND 75 / UNBOUND 136 / SUPERSEDED 6 / WITHDRAWN 8). Vacuous here: no `evidence/` dir. |
| `validate-governance.py` exits 0 | Holds on the full tree | `EXIT_G=0` |
| Stripe matrix is test-mode and Connect-blocked | Holds | `git show HEAD:evidence/external/stripe-sandbox-matrix.json` — `provider_mode=test`, `live_mode=PROHIBITED`, `transfer=false`, payout hold/release/failure/reversal all false |
| Alpha soak does not claim the 24h gate | Holds | `qualifying-soak-alpha.json` has `qualifies_for_24h_gate: false`, 3600s, host `mercmerc.net` |
| Live `/readyz` is test mode, no live value | Holds *now* | `curl -sS https://mercmerc.net/readyz` → `payment_mode=test`, `live_value_movement=false`, `status=ready` |
| `MaxConnLifetime` is 30m | Holds | `control/main.go:340` |
| Currency fixture repair did not loosen assertions | Holds | `git show 86a010cb` pins CAD and drops `t.Parallel()` on the six claim tests; fixtures gained an explicit `currency` column. No deleted `Fatal`. |
| `EXTERNAL_ALPHA_PROVEN` cannot flip from synthetics | Holds | Checker in `scripts/validate-readiness.py`; L12 receipts declare `does_not_satisfy=EXTERNAL_ALPHA_PROVEN` |

The control suite was **not** re-run end-to-end in this worktree.
`go test` in `control/` fails at setup (`pattern runtime-profiles/*.json: no matching files found`) because those files are not in the sparse roots. The green-suite attack below is against the diffs and the tests as they stand at HEAD, not against a fresh `ok merc/control`.

Live `/version` at audit time served `9e31c65b` / `go1.26.6`, not the closeout's `a5bca8c0` @ `sha256:2b2f85c9`. Someone redeployed after the closeout was written. That stale pin is noted; it is not scored as a cheat.

---

## Ranked findings

### 1. Public TLS and a buyer website are already shipped. Two gates were moved out of alpha by claiming they are not.

**Severity:** High  
**Surface:** rescoping (1), and it is what makes the security result (3) miss its named surface.

**Claim.** `ops/backend-alpha-gates.json` classifies
`receipt:security:evidence/external/staging-attack-rehearsal.json` as
`POST_ALPHA` with `harm_reachable_in_this_alpha: false`:

> The named-reviewer public-hostname rehearsal assumes an advertised TLS site
> this alpha does not require; requiring it would force exposing a public
> website to satisfy a gate whose harm comes from exposing one.

The same file classifies
`receipt:website_and_buyer_usability:evidence/autonomous/website-validation.json`
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
as `PUBLIC_LAUNCH` with `harm_reachable_in_this_alpha: false`:

> The product surface is terminal-native and no website is required. A
> WCAG/browser gate cannot prevent a harm that only exists if a public site is
> the buyer surface.

`P1-STAGING`'s `smaller_control` repeats the first half: "Public website TLS is
not required."

**Why it does not hold.** The plane this closeout calls READY is an advertised
public TLS website with a buyer surface.

```
curl -sSI https://mercmerc.net/
  HTTP/2 200
  content-type: text/html; charset=utf-8
  strict-transport-security: max-age=31536000
  content-security-policy: … connect-src 'self' https://api.stripe.com;
    frame-src https://js.stripe.com https://hooks.stripe.com;
    script-src 'self' https://js.stripe.com …

curl -sSI https://mercmerc.net/buyer
  HTTP/2 200
  content-type: text/html
  title: merc buyer workspace
```

`evidence/external/staging-alpha-readiness.json` (the receipt that *closed*
`P1-STAGING`) records `public_443_from_operator_mac: OPEN`, SAN `mercmerc.net`,
Let's Encrypt, HSTS. The closeout posture table says "Controlled staging —
READY — public TLS". `Caddyfile` binds `{$SITE_HOST:mercmerc.net}` and reverse
proxies the control plane that serves `/` and `/buyer`.

The hostile-rehearsal harm ("a hostile internet user finds an authz hole on an
advertised public TLS staging hostname") is reachable the moment that hostname
answers on :443. The website harm is reachable the moment `/buyer` is a public
HTML workspace with Stripe.js in the CSP. Both gates were moved by describing
a product that is not the one that is deployed.

The same public hostname is then used as *proof* that staging exists, and
denied as a reason to attack it. That is the backwards construction the
contract names as cheating.

**Command / file.**

- `curl -sSI https://mercmerc.net/` and `curl -sSI https://mercmerc.net/buyer`
- `ops/backend-alpha-gates.json` ids
  `receipt:security:evidence/external/staging-attack-rehearsal.json`,
  `receipt:website_and_buyer_usability:evidence/autonomous/website-validation.json`,
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
  `p1:P1-STAGING`
- `evidence/external/staging-alpha-readiness.json` `criteria.tls`
- `docs/BACKEND_ALPHA_CLOSEOUT.md` posture table ("public TLS")

---

### 2. "1551 attacks, 0 findings" counts 1257 file reads as attacks and never reaches the surface it is filed under.

**Severity:** High  
**Surface:** security result (3), receipts (4).

**Claim.** Closeout posture and condition 12: "Security — ALPHA SUFFICIENT —
1551 attacks, zero findings." The producer is
`evidence/external/staging-attack-rehearsal.json`, `binding_status: BOUND`.

**Why it does not hold.**

The receipt is honest about what it is, and the closeout is not.

| Field | Value |
|---|---|
| `kind` | `local_alpha_security_rehearsal` |
| `surface` | `local_control_plane_routes` |
| `qualification` | `LOCAL` |
| `target` | `http://127.0.0.1/` via `httptest` (`control/alpha_security_suite_test.go`) |
| `honesty.staging_droplet_touched` | `false` |
| `reviewer.name` | `alpha-security-suite local lane` |
| wall clock | `2026-08-17T00:42:30Z` → `00:43:29Z` (59 s) |
| `attack_rows` | 279 |
| `observations.request_count` / `attacks_executed` | 1551 |

`request_count` is `sum(attack.executed)`, not HTTP requests.

```
supply_chain: attempted=9  executed=1265
  secrets-working-tree-live-keys  executed=1257  alpha_reachable=false
    title: "tracked working tree has no live Stripe key values"
    reproduction: "scan git ls-files for sk_live_/rk_live_ values"
containment: attempted=10  executed=26
  containment-seatbelt-profile    executed=17
```

1257 of the 1551 "attacks" are "this tracked file was opened and did not
contain a live Stripe key." That class is `alpha_reachable: false` and does
not touch a control-plane route, let alone `https://mercmerc.net`. The
remaining HTTP work is `httptest` against `Server.Routes()` on `127.0.0.1`.
Several `authority` rows are file reads (`ops/authorization-matrix.json`
default is deny; the embed parses).

`scripts/validate-readiness.py:external_staging_attack_proven` correctly
refuses this receipt (`CHECK_FAILED → 0/1`). The receipt's own `honesty`
says so: it cannot claim the readiness point because it is not
`persistent_staging_tls`. The closeout still cites 1551/0 as the security
closeout.

`binding_status: BOUND` on a path named `evidence/external/staging-attack-rehearsal.json`
overstates what the producer recorded. BOUND here means "producer_identity
slots are filled", not "this is the external staging rehearsal." A reader of
the path + BOUND + closeout prose will take it as the named-reviewer public
TLS rehearsal. It is not.

**Command / file.**

```
git show HEAD:evidence/external/staging-attack-rehearsal.json
# kind, surface, honesty, attack_classes, observations
python3 scripts/validate-readiness.py   # security: CHECK_FAILED → 0/1
```

`scripts/alpha-security-suite.py` writes this path on purpose
(`EVIDENCE_OUT = …/staging-attack-rehearsal.json`) and states it does not
touch staging.

---

### 3. `P1-CANARY-REHEARSAL` was taken off the start-gate list so `ALPHA_ENGINEERING_READY` can flip without the loop the contract requires.

**Severity:** High  
**Surface:** rescoping (1).

**Claim.** `ops/backend-alpha-gates.json` `p1:P1-CANARY-REHEARSAL` is
`ALPHA_CONTROL`, `necessary_before_first_controlled_alpha_transaction: false`:

> The counted matrix is the engineering rehearsal — it is how the alpha
> begins, not a paper precondition of beginning.

Closeout posture after `645bed17`: "ENGINEERING-READY except the money path —
conditions 4-7 proven locally, 14 PASS; the one open blocker is Stripe
Connect." Conditions 4, 5, 6 flipped from FAIL to **PASS locally**.

**Why it does not hold.** `docs/BACKEND_ALPHA_CONTRACT.md` §2, required
*before* `ALPHA_ENGINEERING_READY`:

> 4. Execution, supplier, buyer, and verification that actually run.

Item 4 is not optional ceremony. Classifying the only rehearsal that would
prove it as `ALPHA_CONTROL` means the claim can become `GO` the moment
Connect is signed, with the live loop still closed. That is rescoping the
obligation off the start-gate while leaving the words in the contract.

The live plane does not have the loop. `evidence/canary/l12-p1-canary-rehearsal-live-staging.json`
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
(`status: BLOCKED`, `plane: https://mercmerc.net`):

- buyer / supplier / verification / settlement: **not run**
- canary allowlist still names superseded r5 hash `f4303a751ca2b2af`
- sealed identity wants `7cc01c442c7f6dbe`; host emits `2939a8e26ffe6fd2`
- quote 400 when nothing is advertised; if infer is advertised, quote 503s
  because canary honeypot economics fail `validateCurrentUniformTaskCounts`

So: the contract says the loop is a start-gate; the gate file says it is not;
the only live-plane receipt says it is blocked; the closeout says conditions
4–6 PASS and the remaining blocker is one dashboard action.

**Command / file.**

- `docs/BACKEND_ALPHA_CONTRACT.md` §2.4
- `ops/backend-alpha-gates.json` `p1:P1-CANARY-REHEARSAL`
- `git show HEAD:evidence/canary/l12-p1-canary-rehearsal-live-staging.json`
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
- `git show 645bed17 -- docs/BACKEND_ALPHA_CLOSEOUT.md`

---

### 4. The 24-hour soak harm was rewritten as the number 86400, and the derivation checker cannot record an honest wall-clock mechanism.

**Severity:** High  
**Surface:** rescoping (1), receipts (4).

**Claim.** `receipt:deployment_and_rollback:evidence/external/qualifying-soak-24h.json`
is `POST_ALPHA`, `harm_reachable_in_this_alpha: false`. Harm text:

> A slow leak, a once-per-day timer, or an unattended overnight failure that
> a short soak would miss — as an *arbitrary 24-hour calendar bar*.

The 0-point row `soak-requirement-derivation.json` is the "we derived this"
witness. Closeout: "No soak assertion is 24-hour-only."

**Why it does not hold.**

The real harms in that sentence — slow leak, unattended overnight failure —
are reachable on a public persistent plane that answers 24/7. Appending
"as an *arbitrary 24-hour calendar bar*" converts those harms into a
definition so they can be declared unreachable. That is constructing the
harm from the desired classification.

The derivation receipt is not a free inventory. `soak_derivation_recorded`
(`scripts/validate-readiness.py:164–178`) requires

```
conclusion == "deterministic_coverage_supersedes_arbitrary_24h"
```

and **fails the receipt if any mechanism has `requires_wall_clock: true`**.
An honest derivation that said "pgx `MaxConnLifetime` is only observable by
waiting" cannot pass. Every one of the 16 mechanisms therefore has
`requires_wall_clock: false`, including:

| Mechanism | Period | Exercise recorded |
|---|---|---|
| pgx pool recycle | MaxConnLifetime 30m | "pool closed and reopened (process restart)" |
| memory growth under sustained full-stack load | "no named period (continuous)" | "historical local 15m compose soaks OOM'd on the agent" |

The first is the *same* mechanism the contract and `soak:alpha-derived` use
to justify a 3600 s live soak, because "unit tests do not recycle the
production pool." The derivation says it does not need wall clock; the 3600 s
gate exists because it does. The second records that short soaks have already
OOM'd, then sets `requires_wall_clock: false` anyway.

`what_a_long_soak_would_still_add` admits the 24 h window still buys
"uninterrupted RSS/FD/heap samples on the full compose+agent stack against
persistent staging and two distinct external devices," then files that under
"staging/device boundary, not a soak duration."

The 3600 s receipt itself is honest (`qualifies_for_24h_gate: false`). The
cheat is the classification of the 24 h gate and the checker that will only
accept a derivation that concludes the 24 h gate is unnecessary.

**Command / file.**

- `scripts/validate-readiness.py` `soak_derivation_recorded`
- `git show HEAD:evidence/autonomous/soak-requirement-derivation.json`
- `ops/backend-alpha-gates.json` `receipt:…qualifying-soak-24h.json` and `soak:alpha-derived`
- `docs/BACKEND_ALPHA_CONTRACT.md` §6

---

### 5. `TestL2StripeWebhookMatrixAgainstRealHandlers` passes because it stopped requiring dashboard secrets.

**Severity:** High  
**Surface:** green suite (2), money path (5).

**Claim.** `8142e6e5`: "the L2 Stripe matrix now proves handler behaviour
without needing dashboard secrets — which is a narrower claim, and the
receipt says so. Closeout condition 14 moves to PASS." Condition 14 then
reads "No known P0/P1 defect — `ok merc/control`, 0 failures."

**Why it does not hold.** Before `8142e6e5` the test was a fail-closed
precondition on the configured webhook contract:

```
if !strings.HasPrefix(billingSecret, "whsec_") || !strings.HasPrefix(connectSecret, "whsec_") {
    t.Fatal("dashboard webhook secrets required")
}
if billingSecret == connectSecret {
    t.Fatal("billing and connect secrets must be distinct")
}
```

After `8142e6e5` (`control/stripe_l2_webhook_matrix_test.go:43–73`) missing
or identical secrets install

```
whsec_l2_matrix_billing_bbbbbbbbbbbbbbbb
whsec_l2_matrix_connect_cccccccccccccccc
sk_test_l2_webhook_matrix_not_a_live_secret
```

and the test continues. The suite no longer goes red when this process does
not hold the dashboard pair that staging actually verifies against. That
pair is exactly what `P1-STRIPE-TEST` and
`stripe_sandbox_matrix_proven` still demand (`endpoint_secrets_verified`,
distinct endpoint ids, compiled `stripeAPIVersion`).

The commit that invented the synthetic secrets is the same commit that
moved condition 14 to PASS. The nine-failure list in the closeout *named*
"the L2 Stripe matrix, which wants secrets this process does not hold" as
the reason 14 was FAIL. The test was retargeted so that sentence would stop
being true, not so the requirement would be met.

This is not a deleted test. It is a test that stopped asserting the
environment condition the money-path gate still requires. Handler HMAC
logic is still exercised. The production webhook contract is not.

(This worktree cannot execute the test: `runtime-profiles/*.json` is outside
the sparse roots. The disproof is the diff.)

**Command / file.**

```
git show 8142e6e5 -- control/stripe_l2_webhook_matrix_test.go
# current lines 43–73 of that file
```

---

### 6. Cell-authority tests that used to assert the buyer-visible catalogue now assert the embedded document.

**Severity:** Medium  
**Surface:** green suite (2).

**Claim.** `066f9189`: "No assertion was deleted or loosened; the product was
correct and the tests were mid-edit." The five tests that the full suite was
failing now pass (`TestOnlyBindableAuthorityCellsAreRoutable`,
`TestAdvertisedSurfaceIsTheBindableSet`,
`TestInvalidatingAuthorityDemotesDependentCell`,
`TestSupersededIncompleteAgentContentRootCannotAuthorizeCurrentAdmission`,
`TestDirectedSetIsASupersetThatDoesNotWidenTheCatalogue`).

**Why it does not hold as stated.** Those tests stopped calling
`advertisedRuntimeCapabilities()` and started calling
`documentAdvertisedCells()`.

```
advertisedRuntimeCapabilities()  = currentActivation().advertised
                                   // "the buyer-visible catalogue under the current policy"
                                   control/activation_policy.go:286–289

documentAdvertisedCells()        = projectCells(runtimeAuthority,
                                   ACTIVE && Routable)
                                   // embedded document only
                                   control/runtime_authority.go:815–818
```

Production still sells through `advertisedRuntimeCapabilities()`
(`control/runtime_matrix.go`, `control/workload_classification.go`). The
live activation can diverge from the document (`adoptActivationIfNewer`,
stored policy, quarantine). A cell that is document-ACTIVE+BOUND but
QUARANTINED on the live overlay — which is exactly the L12 live-staging
refusal: "a live overlay can QUARANTINE it via
`storedRoutableEntryHasCurrentGlobalAuthority`" — will not fail
`TestAdvertisedSurfaceIsTheBindableSet` or
`TestCellLifecyclesDidNotWidenTheAdvertisedSurface`. Those tests no longer
look at the overlay.

The invalidation test was also retargeted from r5 to r6. That half is a
pin update, not a loosening. The helper swap is.

`activation_policy_test.go` and `runtime_matrix_test.go` still go through
the live helper. The tests that were red, and that the commit exists to
turn green, do not.

**Command / file.**

```
git show 066f9189 -- control/cell_authority_binding_test.go \
  control/runtime_authority_v2_test.go \
  control/runtime_cell_authority_test.go \
  control/directed_routing_test.go
rg -n "advertisedRuntimeCapabilities|documentAdvertisedCells" control --type go
```

---

### 7. UNBOUND July 20 registry/SBOM receipts score 9 ALPHA_BLOCKER points for a different image than the one that runs.

**Severity:** Medium  
**Surface:** receipts (4).

**Claim.** `source_and_ci` 10/10 and `licensing_and_supply_chain` 2/3 rest on
`evidence/autonomous/registry-verification.json` (4) and
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
`evidence/autonomous/supply-chain.json` (3+2). The matching gates' harm is
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
"the binary on the persistent plane is not the candidate the operator thinks
it is" / "you cannot say what executed."

**Why it does not hold.** Both receipts are `binding_status: UNBOUND`, dated
**2026-07-20**, candidate image

```
ghcr.io/joshuahickscorp/computexchange-control@sha256:f848a8048af250f7135f54b15d8bf4455bd24af6d42fd4d380dd99e0c1b91563
```

registry `source_commit: a4d50d93d0d8e44742fabe9d6b06380e3191b2e5`.

The plane the closeout describes ran `a5bca8c0` @ `sha256:2b2f85c9…` from
the local docker store ("not pushed to a remote registry"). Live `/version`
now serves `9e31c65b`. Neither is `f848a804` / `a4d50d93`.

`validate-readiness.py` awards the points on `status_in("PASS")` only. It
does not check `binding_status` and does not check that the verified digest
is the digest that is running. The score 85/91 therefore includes 9 start-gate
points for a signed SBOM of a different binary.

The receipts do not overstate their own `binding_status` (they say UNBOUND).
The score and the gate harms cite them for more than they recorded.

**Command / file.**

```
git show HEAD:evidence/autonomous/registry-verification.json
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
git show HEAD:evidence/autonomous/supply-chain.json
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
curl -sS https://mercmerc.net/version
python3 scripts/validate-readiness.py   # source_and_ci 10/10
```

---

### 8. `alert-delivery-r1` is the start-gate that replaced staffed paging. It is UNBOUND, 15 days old, and pointed at `host.docker.internal`.

**Severity:** Medium  
**Surface:** receipts (4), rescoping (1).

**Claim.** `ops/backend-alpha-gates.json` makes
`evidence/autonomous/alert-delivery-r1.json` an `ALPHA_BLOCKER` (1 point) and
the `smaller_control` for `P1-ALERT-DELIVERY` (`PUBLIC_LAUNCH`):

> Observed Alertmanager → HTTP sink fire/resolve receipt (alert-delivery-r1),
> already passing. The operator supervises this alpha in person.

Readiness awards the point (`observability_and_alerting: 6/6`).

**Why it does not hold.** The receipt:

- `binding_status: UNBOUND`
- `completed_at: 2026-08-02T15:03:37Z`
- `receiver.url_host: http://host.docker.internal`
- `delivery.sink_log:` `/Users/scammermike/Downloads/merc-wt-a1/.artifacts/alert-delivery/…`
  (a deleted worktree path)
- no `producer_identity`

`alert_delivery_proven` accepts any `url_host` that is not empty and does
not contain `harness` or `example`. `host.docker.internal` passes. That is a
local compose sink from August 2, not a sink the supervising operator of
`mercmerc.net` will see at 03:00.

`P1-ALERT-DELIVERY` was moved to `PUBLIC_LAUNCH` on "this alpha is
supervised." The plane is a public TLS hostname that runs unattended. The
replacement control does not observe that plane.

**Command / file.**

```
git show HEAD:evidence/autonomous/alert-delivery-r1.json
# scripts/validate-readiness.py:alert_delivery_proven (host check)
ops/backend-alpha-gates.json  p1:P1-ALERT-DELIVERY
```

---

### 9. The closeout document contradicts itself on the score, on condition 14, and on whether the suite is green.

**Severity:** Medium  
**Surface:** receipts / documents (4). The green-suite *claim* in the table
is what `8142e6e5` asserts; the same file still argues the opposite.

**Claim.** Closeout scores line: "Level B 87/100 (threshold 95, **P1=6**,
`NO_GO`)." Condition 14: "**PASS** — `ok merc/control`, 0 failures. Zero P0,
**zero P1**."

**Why it does not hold.**

| Location | What it says |
|---|---|
| `python3 scripts/validate-readiness.py` | P1=**5** |
| `ops/go-no-go.json` `open_p1` | 5 ids |
| `ops/readiness.json` `target_scope_open_p1` | 5 |
| Closeout scores line | P1=**6** |
| Condition 14 | "Zero P0, **zero P1**" |
| Same file, "Open — and why each is real" | "**Nine control-suite tests still fail.** … These keep closeout condition 14 at **FAIL**." |
| Terminal posture | `NOT READY — conditions 4-7 and 14 open` |

`645bed17` flipped the table (4, 5, 6, 14 → PASS; posture → ENGINEERING-READY)
and left the narrative and the terminal block on the previous state. "Zero
P1" next to five open P1s (one of them the closeout's own remaining
blocker) is not a wording quibble.

8 original − 3 dropped (`P1-STAGING`, `P1-RECOVERY-SOAK`, `P1-OFFSITE-RESTORE`)
= 5. The "fell from eight to six" sentence in the same section is the same
off-by-one.

**Command / file.**

```
python3 scripts/validate-readiness.py
rg -n "P1=6|Nine control|zero P1|14 open|ENGINEERING-READY" docs/BACKEND_ALPHA_CLOSEOUT.md
```

---

### 10. A tracked receipt and a remaining-work doc still describe `mercmerc.net` as live-money. The live probe is test mode.

**Severity:** Medium  
**Surface:** money path (5).

**Claim.** Closeout: "Live money — `NO_GO_PROHIBITED` and unchanged."
`/readyz` `payment_mode=test`, `live_value_movement=false`. That part is
true of the process answering today.

**Why a reader can take the opposite.**

`evidence/deploy/live-cutover.json` (still at HEAD, `binding_status: UNBOUND`):

```
"host": "192.241.134.31",
"site": "mercmerc.net",
"commit": "41db85b5",
"stripe_mode": "live",
"warning": "Stripe is LIVE. Charges and payouts move real money."
```

`docs/REMAINING_WORK.md` §0 (dated 2026-08-10, still the current
"everything remaining" doc) treats that as the live host:

> G063. `https://mercmerc.net/version` reports commit `41db85b5` …
> `evidence/deploy/live-cutover.json` records `stripe_mode: "live"` with its
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
> own warning that real money moves.
> That host is missing `a7bf17a6` (the P0: realtime funding under-held …)

`docs/RELEASE_SIGNING_AND_STAGING.md` correctly labels the cutover UNBOUND
and historical. `REMAINING_WORK.md` does not: it is written as an urgent
present-tense defect of the hostname this closeout calls the alpha plane.

`evidence/external/staging-money-ingress.json` is `provider_mode: test`,
`live_mode: PROHIBITED`, `binding_status: BOUND` — honest on mode — but
every signed probe is tagged `"source": "hmac_signed_live_post"`. "Live"
there means "posted over the internet to staging", not live Stripe. The
token is load-bearing next to `live-cutover.json`.

Today's `curl https://mercmerc.net/readyz` is test mode on `9e31c65b`. The
finding is not "the plane is live now." The finding is that the tree still
carries a live-money description of this hostname that a closeout reader
can take as current.

**Command / file.**

```
git show HEAD:evidence/deploy/live-cutover.json
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
rg -n "stripe_mode|Stripe is LIVE" docs/REMAINING_WORK.md docs/RELEASE_SIGNING_AND_STAGING.md
curl -sS https://mercmerc.net/readyz
```

---

### 11. Level B still cashes 2/2 website points on an UNBOUND 2026-07-20 loopback Playwright run.

**Severity:** Medium (Level B score honesty; the alpha classification is finding 1).

**Claim.** `website_and_buyer_usability: derived=2/2`. Advisory reason:
"automated desktop/mobile browser interaction checks and AA 6.06:1 static
validation pass."

**Why it does not hold as a statement about the site that is up.**
`evidence/autonomous/website-validation.json` is `UNBOUND`,
`completed_at: 2026-07-20T05:41:18Z`,
`verification_commands: Playwright against a loopback static server`.
It is not bound to `mercmerc.net`, not bound to HEAD, and not the `/buyer`
workspace served today. The readiness checker is `status_in("PASS",
"PASS_AUTOMATED_BROWSER")` — status only.

**Command / file.**

```
git show HEAD:evidence/autonomous/website-validation.json
(unbound historical receipt — cited here as the subject of audit, not as authority; it does not prove the claim under discussion)
python3 scripts/validate-readiness.py   # website_and_buyer_usability 2/2
```

---

### 12. "One dashboard action" undersells the remaining money-path gate.

**Severity:** Low  
**Surface:** money path (5).

**Claim.** Closeout: "the remainder is one dashboard action" / "the single
remaining alpha blocker … is one dashboard action."

**Why it is short.** `ops/go-no-go.json` `P1-STRIPE-TEST` exit criterion,
and the matrix receipt, require: Connect signup on `acct_1TxbzMCwPLrR4vaY`,
a Canadian test connected account, recreate the Connect webhook with
`connect=true` (currently `connect=null`), then `make stripe-matrix` until
`stripe_sandbox_matrix_proven` accepts a PASS with a real `tr_` and payout
hold/release/failure/reversal. That is a dashboard click plus account
setup plus webhook recreation plus a passing matrix, not one action.

The receipt is otherwise honest (`BLOCKED`, `UNBOUND`, real `pi_`/`ch_`
for the non-Connect half, `live_mode: PROHIBITED`).

**Command / file.** `ops/go-no-go.json` `P1-STRIPE-TEST`;
`git show HEAD:evidence/external/stripe-sandbox-matrix.json` `blocker`,
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
`connect_gated_remainder`, `notes`.

---

## Surfaces driven

| # | Surface | Driven? | Outcome |
|---|---|---|---|
| 1 | Rescoping / named reachable harm | Yes. All 9 out-of-alpha gates read. Public hostname and `/buyer` probed. | Findings 1, 3, 4, 8 |
| 2 | Green suite / retargeted tests | Yes. `8142e6e5`, `066f9189`, `86a010cb` diffs. Currency holds. L2 and cell-authority do not. Full suite not re-run (sparse). | Findings 5, 6 |
| 3 | Security 1551/0 | Yes. Receipt broken down by class and by `executed`. Producer is local httptest. | Finding 2 |
| 4 | Receipts / binding overclaim | Yes. Attack-rehearsal, soak derivation, registry, supply-chain, alert-delivery, website-validation, L12 live staging, closeout prose. Binding validator on the full tree. | Findings 2, 4, 7, 8, 9, 11 |
| 5 | Money path described as live | Yes. Live `/readyz` is test. `live-cutover.json` + `REMAINING_WORK.md` still describe this host as live. L2 synthetic `sk_test_`. `hmac_signed_live_post`. | Findings 5, 10, 12 |

## Not found, after looking

- No receipt claims `qualifies_for_24h_gate: true` on the alpha soak.
- No closeout sentence says the Stripe matrix is a Connect-complete PASS.
- `EXTERNAL_ALPHA_PROVEN` is not being flipped by L11/L12 synthetics.
- `86a010cb` currency repair writes the settlement currency explicitly and
  serialises the six claim tests; it does not delete assertions.
- Level B's 100-point bar and `go_threshold=95` were not rewritten. The 9
  rescoped gates still cost Level B the same 9 points (1+3+1+1+1+2). 100−9=91
  is the alpha bar; 91−6 (blocked Stripe matrix) = 85. The arithmetic holds.
- `validate-readiness.py` / `validate-evidence-binding.py` /
  `validate-governance.py` exit 0 on the full tree at this HEAD.

---

## What this audit did not run

- `ok merc/control` against `192.168.148.2`. Sparse tree is missing
  `control/runtime-profiles/*.json`; `go test` fails at package setup.
- `make alpha-security` (would rewrite the receipt this lane is forbidden
  to edit).
- `scripts/recovery-suite.sh`.
- `git lfs prune` was not run.
- `.merc-secrets.env` was not read.
- No live Stripe call, no live money.
