# Everything remaining

State at 2026-08-17. HEAD `9e31c65b`. Re-derived from
`python3 scripts/validate-readiness.py` and public `https://mercmerc.net`
probes, not from the 2026-08-10 narrative below.

| Axis | Derived | Decision |
|---|---|---|
| Level A software | — | `GO` |
| Level B private canary | **87/100** (threshold 95, P0=0, P1=5) | `NO_GO` |
| Backend alpha | **85/91** | `ALPHA_ENGINEERING_READY NO_GO`, `EXTERNAL_ALPHA_PROVEN NO_GO` |
| Level C live money / public launch | — | `NO_GO_PROHIBITED` |

Open P1s: `P1-STRIPE-TEST` (`ALPHA_BLOCKER`, Connect not signed up),
`P1-CANARY-REHEARSAL` (`ALPHA_CONTROL`; local L12 PASS, live staging
`BLOCKED`; does not satisfy `EXTERNAL_ALPHA_PROVEN`),
`P1-ALERT-DELIVERY` / `P1-INDEPENDENT-APPROVAL` / `P1-GOVERNANCE`
(`PUBLIC_LAUNCH`). Satisfied on evidence: `P1-STAGING`,
`P1-RECOVERY-SOAK` (3600 s alpha soak; 24 h unearned),
`P1-OFFSITE-RESTORE`. 44 gates classified (28 `ALPHA_BLOCKER`, 7
`ALPHA_CONTROL`, 7 `PUBLIC_LAUNCH`, 2 `POST_ALPHA`). Governance
documents are unapproved drafts.

Public hostname `https://mercmerc.net` (2026-08-17): Let's Encrypt TLS;
`GET /readyz` HTTP 200, `payment_mode=test`, `live_value_movement=false`,
`provider_enabled=true`; `GET /version` HTTP 200, commit
`19fe0b23940c7e3d4da9b45d9cc5689c2c515d07`, `modified: false`,
`go_version: go1.26.6` — 20 commits behind this HEAD, not 733, and
not a live-money plane. A 2026-07-27 cutover of commit `41db85b5` did
put a live Stripe key on this hostname; that event is the unbound
historical receipt `evidence/deploy/live-cutover.json`, not the
2026-08-17 host.

Ordered by consequence. **[auto]** / **[you]** / **[blocked]** as before.

---

## 0. Current money-path wall — Connect signup, not a live stale host

**Current.** Stripe test mode is proven up to the Connect boundary.
`evidence/external/stripe-sandbox-matrix.json` is `BLOCKED` /
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
`connect_platform_not_signed_up` on platform `acct_1TxbzMCwPLrR4vaY`.
That is the single open `ALPHA_BLOCKER` P1. Live value movement on the
public host is `false`.

**G063 — historical (probe 2026-08-10 of a 2026-07-27 cutover), superseded
by the 2026-08-17 public probe above.** On 2026-08-10
`https://mercmerc.net/version` still reported commit `41db85b5`,
`"modified": true`, 733 commits behind then-HEAD.
`evidence/deploy/live-cutover.json` (still `UNBOUND`) is the historical
record of the 2026-07-27 live-key cutover of that commit: it still
records `stripe_mode: "live"` and still carries the contemporaneous
operator warning *"Stripe is LIVE. Charges and payouts move real
money."* Those fields describe 2026-07-27. They are not a description
of the hostname today. `/readyz` on 2026-08-10 returned only
`{"status":"ready"}`. That is **not** the 2026-08-17 host: `/readyz`
now carries `payment_mode` / `live_value_movement`, and both say test /
false.

The 2026-08-10 missing-commit list (`a7bf17a6` prepaid under-hold,
`dd59aa62` advisory lock) is retained as the finding that was true of
`41db85b5`. It is not a claim about `19fe0b23`.

- **[you]** Sign up for Connect on the test account, recreate the
  Connect endpoint with `connect=true`, re-run `make stripe-matrix`.
- Two traps from the runbook still apply on any deploy: `MERC_TOKEN_KEY`
  must be byte-identical or every sealed token in Postgres becomes
  undecryptable; `MERC_VERIFICATION_SAMPLE_SECRET` must be set or
  verification silently uses a **published** default.

---

## 1. Network V2 obligations still open

> **Historical inventory — state at 2026-08-10, `main` at `353840cb`.**
> Ledger then: 44 verified / 25 open. The G00x/G0xx rows below were not
> re-probed against this HEAD. They are retained so the 2026-08-10 remainder
> is not silently dropped. Current release remainder is §0 plus the five
> open P1s in the header. Do not close a gate from a row in this section
> without a new receipt.

### Decision-chain steps — the spine

| # | What remains | Who |
|---|---|---|
| **G005** Step 5 | Blocked only by G061's fix landing; then a final adversarial pass with no live P0/P1 | [auto] |
| **G007** Step 7 | Realtime `PUSH_ORDER_BOOK` landed. Batch pull-shape and service lease remain; ranking currency stays float USD deliberately | [auto] |
| **G008** Step 8 | RuntimeDecision freeze. **Promotion is struck** — needs its own coverage model first (below) | [auto] |
| **G009** Step 9 | Bind the *actual worker choice*. Three types own "placement" and the one that matters has none | [auto] |
| **G010** Step 10 | TopologyDecision landed; gating | [auto] |
| **G011** Step 11 | Chain root exists (G014). Remaining: batch post-commit shadow tear | [auto] |
| **G012** Step 12 | VerificationContract landed as `PARTIAL_ACCEPTANCE_BOUND`; realtime free-string residual | [auto] |
| **G013** Step 13 | Liability citation landed. Remaining: realtime/lease explicit finality | [auto] |

**Step 8's real blocker.** Promotion is refused on two independent grounds:
`promotionMatchedPairAuthorityRefusal` (no durable matched incumbent/challenger
execution pair) and `activation_policy.go:1326-1333` (evidence covers one exact
scope; cell lifecycle is global). Building one does not unblock the other. A
design report exists for both.

### Locality, scale, learning

| # | What remains | Who |
|---|---|---|
| **G015** Step 15 | Locality is soft belief: stale `worker_prefix_state` can flip a winner with no receipt of the freshness it trusted | [auto] |
| **G016** Step 16 | Workload graph survives compile→receipt; IR currently flattens | [auto] |
| **G017/G019/G020** | Twin harness + fleets + scale curves. **The selector is SQL inside Postgres** — no extracted `Select(epoch, request)` exists, so the twin must drive production's own entry points. A Go stand-in would produce numbers shaped like the targets and meaning nothing | [auto] |
| **G018** Step 18 | Hierarchical indexes. Realtime/service are already profile-scoped; the residual is batch's fleet-relative `EXISTS` per eligible job | [auto] |
| **G021** Step 21 | Capture is **partly already there**: the batch lane's actuals decompose from existing populated timestamps (landed 2026-08-10), realtime needs real new columns for the queue/startup/prefill split inside TTFT, and leases are a continuous metered contract whose "phases" need defining before capture. The prediction side is untouched | [auto] |
| **G022** Step 22 | Network-fault mutants. Work exists but **collided on IDs 105–109** with the open-exposure mutants; needs re-authoring from 110 | [auto] |
| **G025** Step 25 | Adapters/datasets/render/layers/kernels are ABSENT as elimination classes, not near-term | [auto] |
| **G053** | Per-phase predicted-vs-actual. Total-duration prediction already exists (`eta_calibration`). The "no queue-wait, startup or cold-start column exists anywhere" claim was **true of column names and false about the data** — corrected 2026-08-10. Batch decomposes with no migration from timestamps production already writes (`created_at`, `visible_at`, `claimed_at`, `started_at`, `completed_at`, `verified_at`), and `DecomposeTaskPhases` now does it. Realtime genuinely lacks the split: it has only contract `created_at`→`finalized_at` plus `time_to_first_event_ms`, and TTFT conflates queue, startup and prefill. What remains everywhere is the per-phase **prediction** to regret against | [auto] |
| **G062** | 3 stale mutants (28, 29, 34) orphaned by the money-rail rewrite + 3 real survivors | [auto] |
| **G048/G049/G050** | Narrowing ladder; tail-latency law; validation rhythm timings (needs a quiet box — measured under load 60–180 tonight, so not quotable) | [auto] |

### Fabric, services, parity

| # | What remains | Who |
|---|---|---|
| **G026** Step 26 | Workload classes where prerequisites hold | [auto] |
| **G027** Step 27 | POOL/REPLICA_SERVICE likely already exist under other names; **LOCAL_CLUSTER is refused by construction** (`LocalClusterAdmissible=false`) and **CLOUD_BACKSTOP is decision-only** (`CloudBackstopPermitted` is never true in production). Two of four close as exact refusals | [auto] |
| **G028** Step 28 | ServiceLease: prove no lost liability or double charge under worker loss, upgrade, scale, meter retry, duplicate event, region loss | [auto] |
| **G029** Step 29 | Regional seams — explicitly *not* to be built before measured triggers | [auto] |
| **G023/G051** | Direct-engine parity, and Merc-overhead vs network-advantage as **separate** benchmarks | [you] one RunPod session |
| **G024** Step 24 | **Not a hardware problem.** `heterogeneous-placement-principal-latest.json` is BOUND and refuses on software grounds: ordinary admission freezes a Metal-only singleton, CUDA cells are DRAFT, no CUDA embed cell exists, and Metal q4 vs CUDA bf16 is not one quality contract. GPU time cannot flip it — the engine tournament is the unblock | [auto] |

---

## 2. Shippability (G030) — the 24-item gauntlet

> **Historical (2026-08-10).** Offsite restore, persistent TLS staging, and
> the alpha recovery soak have since closed on evidence. Signed
> distribution, licence counsel, 24 h soak, staffed paging, and the Stripe
> Connect remainder remain.

**[auto], already done or reachable:** source/evidence reproducibility, validation
inside budgets, receipts rejecting tampering, money reconciliation, worker/provider
loss recovery, POOL/fabric refusals, exact-HEAD checkpoint.

**[auto], now unblocked:**
- **SBOM** — generator exists. `evidence/state/sbom.json` is an unbound
  historical snapshot at `8e6b1024` (CycloneDX 1.5, 639 components there),
  generated from `go list -m -json all` + `cargo metadata`. No AGPL/SSPL/GPL; the
  single copyleft appearance (`r-efi`) is a disjunction that resolves to MIT.
  236 Go components are recorded as *undeclared* rather than guessed permissive.

**[you], one-time credentials:**
- **Signed distribution** — `scripts/notarize-macos-release.sh` is written and
  fails closed. Needs a **Developer ID Application** certificate (`security
  find-identity` shows 0) and an App Store Connect API key. After that,
  notarization is two commands, not a manual step per release.
- **Licences** — three concrete items: no owner-approved root project licence
  (`agent/Cargo.toml` says MIT with no text tracked); Llama 3.2 1B Q4 needs the
  agreement vendored plus "Built with Llama" attribution; all-MiniLM-L6-v2 needs
  notice preservation. Both models are BLOCKED and the register forbids editing
  BLOCKED rows to silence it — merc **prices and serves** them, so it is a real
  legal question.

**[you], infrastructure and people:**
- External strict-TLS staging (see §3), 24h+ mixed soak,
  **staffed paging with a human acknowledgement**. The live-droplet offsite
  restore drill is recorded at `docs/OFFSITE_BACKUP_RESTORE.md`. Bible §19:
  no LLM may invent external approval.

**[blocked] permanently local:** real supplier fleet (Level B is NO_GO), so
anything needing fleet evidence stays a §26 residual.

---

## 3. Cloudflare consolidation — the honest migration plan

> **Historical plan (2026-08-10).** `mercmerc.app` still serves the black
> landing page (HTTP 200, re-verified 2026-08-17). `mercmerc.net` now
> terminates public Let's Encrypt TLS on the droplet and answers `/readyz`
> 200 in test mode. DNS-proxy and Containers steps were not re-probed.

### Already done

- `mercmerc.app` + `www` serve the black landing page from Worker `merc-landing`
  (`web/landing/`). HTTP 200, TLS verified, Cloudflare-proxied at `172.64.80.1`.
- `wrangler` 4.120.0 authenticated by OAuth. **The Cloudflare MCP connectors are
  redundant** — wrangler covers deploy, DNS, routes and Workers.

### Known broken

- `CLOUDFLARE_API_TOKEN` in `.merc-secrets.env` is **dead** (API returns
  `9109 Invalid access token`). Anything reading that env var —
  `cloudflare-purge.sh`, `cloudflare-teardown.sh` — fails until it is replaced.
- `mercmerc.net` is **grey-cloud** (DNS-only, A `192.241.134.31`), so Cloudflare
  is not in its path at all. TLS is terminated by the droplet's Let's Encrypt cert.

### What full consolidation actually requires

**Do not migrate the database to D1.** D1 is SQLite. The control plane depends on
191 `FOR UPDATE`, 45 `SKIP LOCKED`, 25 `pg_advisory_*` and 55 `NUMERIC(12,6)`
columns. Those are the money-correctness backbone — the same code that yielded a
P0 and a P1 today. SQLite has no row locks, no `SKIP LOCKED`, and no exact
decimal, so the money path would fall back to float, which it explicitly refuses.
That is a rewrite of the concurrency core, not a port.

*(One genuine fit worth noting: a Durable Object per buyer would be a clean —
arguably better — replacement for the per-buyer advisory lock. It replaces 25 of
261 sites. The pull-market claim path has no equivalent.)*

**The version that works, in order:**

1. **[you]** Stand up real Postgres off the droplet — Neon, Supabase, or DO
   Managed. All are real Postgres, so every lock primitive and `NUMERIC` survives
   untouched.
2. **[auto]** Migrate schema and data; re-run the isolated-DB suite against it.
   That suite is the proof the move preserved money semantics.
3. **[auto]** Put **Hyperdrive** in front. This is a real latency win, not a
   nicety: it removes seven round trips per connection (TCP, TLS ×3, auth ×3) and
   pools globally. Free plan. Private databases connect via Workers VPC over a
   Cloudflare Tunnel.
4. **[you → then auto]** Run the control plane on **Cloudflare Containers**, not
   Workers. There are 23 background loops in production — verification processor,
   lease sweeps, rate limiter, latency watchdog — and Workers are request-scoped.
   **Containers needs the Workers Paid plan**; on Free, `wrangler containers list`
   returns `Unauthorized: You do not have access to Cloudflare Containers`. Config
   is written and waiting at `deploy/cloudflare/` — `wrangler.jsonc`,
   `container-entry.js`, and `migrate-to-cloudflare.sh`. The existing
   `Dockerfile.control` (distroless, digest-pinned) is the image, so nothing new
   has to be built.
5. **[auto]** Move `mercmerc.net` to proxied, put the Worker in front, and set
   SSL/TLS to **Full (strict)** with Authenticated Origin Pulls. Flexible leaves
   the origin hop unencrypted; plain Full does not validate the origin cert;
   neither is "strict TLS" for the gauntlet.
6. **[auto]** Retire the droplet. Subscription gone.

### What consolidation does *not* buy

Edge proximity saves roughly 50–100 ms of network RTT. The latency atlas measures
the dominant terms as a **20-second** queue deferral (`askDeferralWindow`) and
same-buyer lock serialization at **59 ms p50 / 133 ms p99 at concurrency 32** —
with physical work in seconds. Migrating for speed optimises a term that is not
the bottleneck, which is exactly what the addendum's "absolute effect first" rule
exists to prevent.

Migrate to consolidate and to drop the subscription. Not for latency. And note
that GPU execution can never live on Cloudflare — suppliers are separate machines
by design, which is the whole product.

---

## 4. Handoff (G031)

> Still last. Current machine remainder is the five open P1s and
> `EXTERNAL_ALPHA_PROVEN=false`. Live money stays `NO_GO_PROHIBITED`.

Final artifact: grades, receipts, validation wall times, network-scale curves,
money reconciliation, and every external blocker named against its Bible §26
residual class. Written last, because it must describe the end state rather than
predict it.
