# Everything remaining

State at 2026-08-10. Ledger: 44 verified / 25 open. `main` at `353840cb`.

Since the 2026-08-09 pass: the network-v2 branch is merged into `main` (one line
of work, one writer); the corrected scale rerun turned out to have finished and
its realtime cells were refusals, not measurements; the realtime candidate
ranking is 1.75x faster on a measured paired A/B; the buyer funding rails gained
the ordering test they never had; and mutants 28, 29 and 34 point at live code
again — one of which was hiding a real gap in the legacy pricing branch.

Ordered by consequence, not by step number. Each item says what closes it and
who has to do it — **[auto]** runs unattended here, **[you]** needs a human once,
**[blocked]** names a dependency that does not exist yet.

---

## 0. Urgent — a live payment rail is running stale money code

**G063.** `https://mercmerc.net/version` reports commit `41db85b5`,
`"modified": true`, **733 commits behind** HEAD (re-verified 2026-08-10), and
`evidence/deploy/live-cutover.json` (unbound historical cutover record) records
`stripe_mode: "live"` with its own warning that real money moves.

That host is missing `a7bf17a6` (the P0: realtime funding under-held open prepaid
exposure — no service-lease term at all, and `estimated_usd` where the reserved
residual belonged, which is roughly half) and `dd59aa62` (the P1: prepaid
admission never took the buyer funding advisory lock). Both were reproduced with
failing-before tests today.

There is no remote way to check whether live payments are already sealed:
`GET /readyz` on that host returns `{"status":"ready"}` and nothing else, while
current code returns `payment_mode`, `provider_enabled` and `live_value_movement`
there. The host predates the probe that would answer the question about it.

- **[you]** Seal payments (`MERC_PAYMENT_MODE=sealed`) **or** deploy current code.
  No SSH key to that host exists on this machine — probed, and `docs/RUNBOOKS.md`
  says the same. Steps are prepared in `docs/RELEASE_SIGNING_AND_STAGING.md` §4.
- Two traps from the runbook: `MERC_TOKEN_KEY` must be byte-identical or every
  sealed token in Postgres becomes undecryptable; `MERC_VERIFICATION_SAMPLE_SECRET`
  must be set or verification silently uses a **published** default.

---

## 1. Network V2 obligations still open

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

Final artifact: grades, receipts, validation wall times, network-scale curves,
money reconciliation, and every external blocker named against its Bible §26
residual class. Written last, because it must describe the end state rather than
predict it.
