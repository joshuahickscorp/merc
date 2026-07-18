# Historical Release-Candidate Inventory (non-authoritative)

<!-- CLAIM-SCOPE: historical-non-authoritative -->

> This file preserves an earlier capability inventory and contains stale counts,
> payout assumptions, and “everything local is done” language. It is not current
> release status and must not be used for product, money, runtime, or launch claims.
> Canonical status is [`proof/5x5-gates.json`](proof/5x5-gates.json), rendered by
> `cx prove`; proof ledgers count only when
> `scripts/verify_proof_ledger.py` accepts their terminal source-bound envelope.
> In particular, a local two-process run is not two physical suppliers, transfer
> code is not real money movement, and a generator/build is not a product outcome.

The text below is retained for history. Its old local-versus-external dividing rule
was too coarse: substantial local work remains in attempt-level economics, crash
recovery, runtime authority, enrollment, API contracts, artifact lanes, claim
conformance, and operational drills.

Run the proof yourself:

```bash
make prove-local          # ~3–5 min: native Postgres+MinIO, full matrix, live Metal inference
```

It boots a throwaway stack, runs the deterministic matrix, drives the **real**
Rust agent through **live** embed / batch-infer / whisper / batch-classification
inference, and prints a PROOF LEDGER. Exit code is non-zero on any gap. Last local
run: **82/82 pass** (Computexchange **Turbo** — see [docs/TURBO.md](docs/TURBO.md)).

---

## ✅ Proven locally (one command reproduces all of it)

| Capability | How it's proven | Where |
|---|---|---|
| Infra boots cleanly | native Postgres + MinIO provisioned fresh each run | `scripts/prove-local.sh` |
| DB migrates | `db/schema.sql` applied, idempotent | `db-migrate` ledger row |
| Seed data | demo buyer api_key + worker_token + models + honeypots | `control seed` |
| Control plane healthy | `/healthz` gated startup, fatal-on-misconfig | `control-healthz` |
| MinIO object flow | put/get round-trip + presigned GET/PUT over HTTP | `TestObjectFlow` |
| Worker registration | real agent registers, visible via `/admin/workers` | `worker-register` |
| **Embed job (live)** | real MiniLM on Metal, 384-dim vectors, verified | `job-embed` |
| **Batch-infer job (live)** | real Llama-3.2-1B (Q4 GGUF) on Metal | `job-infer` |
| **Whisper job (live)** | real whisper-tiny transcription on Metal | `job-whisper` |
| Job split → tasks → S3 | per-chunk inputs, presigned dispatch, commit | `TestEmbedHappyPathSimulated` |
| Redundancy verification | within-class cosine ≥ 0.999; mismatch detected | `TestRedundancyVerify` |
| Honeypot verification | known-answer pass; fraud → dock + clawback + requeue | `TestHoneypotVerify` |
| Mismatch / fraud path | divergent results flagged; confirmed fraud clawed back | `TestRedundancyVerify` + `TestHoneypotVerify` |
| Duplicate commit / idempotency | second commit → 409, credited exactly once | `TestDuplicateCommitIdempotent` |
| Stale running tasks requeue | claimed-but-uncommitted → back to queue w/ backoff | `TestStaleRequeue` |
| Failed jobs → partial settle | retries exhausted → task+job fail, settled at completed work (delivered chunks stay charged, no refund row) | `TestFailPathPartialSettle` |
| Webhook retries | backoff retry to a local receiver; exactly-once delivery | `TestWebhookRetry`, `TestWebhookSweepExactlyOnce` |
| Payout hold → ready | held credit past its window → `ready` (queued) | `TestPayoutHoldToReadyAndBlocked` |
| **Payout transfer stays blocked** | stub rail errors; never marks `released` w/o a ref | `payout-blocked` |
| Invalid auth fails | missing/garbage bearer → 401, non-admin → 403 | `TestAuth` |
| Malformed manifests fail cleanly | bad json/tier/hw_class/webhook/input → 4xx, no partial job | `TestMalformedSubmissions` |
| Hostile input never panics | 670k+ fuzz execs on splitter + submit decoder | `FuzzSplitJSONL`, `FuzzJobSubmitDecode` |
| Metrics expose counters | `cx_*_total` advance across a real run | `metrics` ledger row |
| Logs are useful | structured startup/storage/worker lines asserted | `logs` ledger row |
| Tests pass | `go test ./...` + `cargo test` green | `tests-go`, `tests-rust` |
| CI gates all of it | build+vet+fmt+unit+**integration matrix** (PG+MinIO), Rust on macOS/Metal, schema apply | `.github/workflows/ci.yml` |

### ✅ Turbo (second pass — [docs/TURBO.md](docs/TURBO.md))
| Capability | How it's proven | Where |
|---|---|---|
| **Scheduler never assigns incompatible work** | an `apple_silicon_ultra`-only job is never claimed by a lower-class (e.g. `apple_silicon_pro`) agent — stays queued; SQL hard-filter joins worker capability | `hard-filter-live`, `TestClaimHardFilter`, `TestMatchHardFilterNeverReturnsIneligible` |
| **Agent completes multiple tasks faster** | warm model pool loads each model **once** across N tasks (baseline reloaded per task); 6-task job through one warm MiniLM | `job-embed-multi`, `pool_loads_once_across_n_runs` |
| **Results merged into a buyer-ready artifact** | per-task results collapsed into one ordered JSONL at `output_ref`; 6 rows in order | `job-embed-multi`, `TestMergeResultObject` |
| **New workload + real verification** | live `batch_classification` (warm Llama, top-1 label) with within-job redundancy compared by the verifier | `job-classify`, `TestResultsAgreeBatchClassification` |
| New workloads: classification / extraction / rerank | three runners + three job-type-aware verifiers (label / canonical-JSON / order equality) | `TestResultsAgree{BatchClassification,JSONExtraction,Rerank}` |
| Verification V2 — 3-way tiebreak | 2 disagreeing results → pinned distinct third worker → real majority vote, loser docked | `TestTiebreakThreeWay` |
| Auto-quarantine | honeypot fail / reputation < 0.2 → supplier `suspended`; scheduler then excludes it | `TestAutoQuarantineOnHoneypotFail` |
| Straggler hedging | running primary past ~2× expected → one pinned hedge to a peer; first commit wins, sibling cancelled | `TestStragglerHedge` |
| Adaptive chunk sizing + ETA | per-job-type lines/task at a ~45s target; queue-depth/throughput ETA persisted | `TestAdaptiveSplitSize` |
| Buyer invoice | per-job invoice from the ledger (charged / supplier-paid / platform-take) | `invoice` ledger row |
| Operator dashboard | served at `/` same-origin (no CORS); jobs + metrics panels | `dashboard` ledger row |
| **Admin panel (jobs/workers/fraud/payouts)** | admin-scoped read views over ALL buyers/suppliers: `/admin/jobs` · `/admin/workers` · `/admin/fraud(-flags)` · `/admin/payouts`, plus `/admin/workers/{id}/suspend` | `admin-views`, `worker-register` |
| **Multi-supplier run (local "two Macs")** | two agent processes on one box register as DISTINCT workers; a shared batch_infer job has ≥2 distinct workers commit its tasks (bounded concurrency guarantees the second agent gets work) — the local stand-in for two physical Macs | `multi-agent` |
| **Manual-export payout (alpha)** | the vendor-neutral alpha rail: owed credits appended to a CSV for out-of-band settlement (`CX_PAYOUT_EXPORT`), with an honest `manual-export` ref — never a faked transfer id | `TestManualExportPayout`, `payment.go` |
| **Operations: load + disaster recovery** | a 12-job burst all completes under concurrent load; `pg_dump` → restore into a fresh DB preserves every job — backups proven *restorable*, not just configured | `load-test`, `disaster-recovery` |
| **Simple install + menu-bar build** | one-command `scripts/install.sh` (build → install → config → LaunchAgent; dry-run verified) and the SwiftUI menu-bar app compiles via `swift build` | `install-check`, `macapp-build` |
| `cx` CLI + Python SDK | standalone Go CLI (submit/status/results/cancel/estimate) + dep-free `urllib` SDK with OpenAI-shaped `embeddings()` | `cli/`, `sdk/python/` |
| **OpenAI-compatible Batch API** | `POST /v1/files` → `POST /v1/batches` → poll `GET /v1/batches/{id}` → `GET /v1/files/{id}/content`; each `custom_id` request maps onto a real embed/infer job (same scheduler+verification), output translated back to OpenAI batch JSONL | `openai-batch`, `openai.go` |
| **Menu-bar status surface** | the agent writes `~/.compute-exchange/status.json` atomically on registration + every heartbeat + task transition (state · earnings · telemetry); a real run produces a valid, fresh doc the macOS app reads | `status-file`, `status.rs` tests |

### ✅ Closed-alpha pass (supplier safety + skeleton — [docs/ALPHA_READINESS.md](docs/ALPHA_READINESS.md))
| Capability | How it's proven | Where |
|---|---|---|
| **Dynamic provider throttling** | agent takes a REAL available-memory reading each cycle, computes effective allocatable memory (`available − headroom`), and a pure eligibility function pauses new claims on headroom breach / utilization ceiling / next-task estimate — enforced before every claim | `agent/src/config.rs` + `hardware.rs` unit tests, `status-file` |
| **Effective memory surfaced** | `status.json` carries total/available/reserved/effective memory + `throttled` + `throttle_reason` + `current_task_id`; `prove-local` asserts the block is present and coherent (`effective = available − headroom`) | `status-file`, `status.rs` tests |
| **Scheduler safety contract** | the SKIP-LOCKED claim filters on the worker's live **effective memory** and **throttled** state (falling back to total memory only pre-heartbeat) — a throttled or under-provisioned worker can never claim unsafe work | `TestClaimHardFilter` (throttled + effective-memory cases), `TestMatchExcludesThrottledWorker` |
| **Operator throttle visibility** | `GET /admin/workers` exposes `effective_memory_gb` + `throttled` per worker | `store.go ListWorkers`, `admin-views` |
| **Operator Control Room** | passkey-gated console at `/admin` (`/` redirects there) · birds-eye money split, fleet, runs, self-healing watchdog · the role-tab skeleton was retired into this one console | `admin-console`, `web/admin.html` |

### 🟡 Design-deferred surfaces
Not faked, not finished: **functional structure wired to real APIs**, final design
pending product input. The menu-bar app (`macapp/`, buildable) and the
`Workflows`/"IDE" placeholder (no code editor, no arbitrary execution; workflows
are expressed through jobs — see [docs/SUPPLIER-SURFACE.md](docs/SUPPLIER-SURFACE.md)).
See [docs/PRODUCT_SHAPE.md](docs/PRODUCT_SHAPE.md) and
[docs/ALPHA_READINESS.md](docs/ALPHA_READINESS.md).

### The single intentional local stub
- **Payout transfer rail** (`stubPayout`). The ledger math, the 90/10 split, the
  hold→ready state machine, and the clawback are all real and tested. Only the
  actual money movement is stubbed — and the system **refuses to fake it**: a
  credit is marked `ready` (owed, queued), never `released`, until a real rail
  exists. Proven by `payout-blocked` + `TestPayoutHoldToReadyAndBlocked`.

---

## ⛔ Remaining work — necessarily external

None of these can be honestly completed on one developer machine. They are the
real release checklist.

### Real accounts & production credentials
- [ ] **Stripe Connect _or_ Trolley** account + production credentials. `Payout.Send`
      is now **implemented** (`StripePayout` in `control/payment.go`: a real,
      idempotent Stripe `/v1/transfers` call to the supplier's `stripe_acct`,
      selected automatically when `STRIPE_SECRET_KEY` is set). This is now a
      credentials + supplier-onboarding task with **no code left** — without a key
      the honest stub keeps credits at `ready` (owed), never `released`.
- [ ] Production Postgres + object storage (S3/R2) with real credentials, backups, TLS.
- [ ] Hosted supplier tax/KYC onboarding through the approved payout provider;
      verify full tax identifiers never enter CX fields, logs, stores, or backups.

### Legal & compliance (require a professional)
- [ ] **FINTRAC** opinion: does buyer→rail→supplier flow trigger MSB registration?
- [ ] **CRA Part XX** digital-platform reporting (in effect since Jan 2024).
- [ ] GST/HST registration timing; Quebec Law 25 / PIPEDA review for job PII.
- [ ] Terms of service: independent-contractor classification, no-arbitrary-code, disputes.

### Signing & distribution (require an identity / platform account)
- [ ] **Apple Developer ID** code-signing + notarization. The LOCAL build path is
      done + proven: `scripts/install.sh` builds + installs the agent with a
      login-time LaunchAgent (`install-check`), and the menu-bar app compiles via
      `swift build` (`macapp/Package.swift`, `macapp-build`). What remains external:
      a Developer ID to sign + notarize a distributable `.app`, plus a Sparkle (or
      equivalent) signed auto-update channel.
- [ ] App entitlements review (deny mic/cam/location; App Sandbox). The agent
      already writes `~/.compute-exchange/status.json` (`agent/src/status.rs`,
      proven by the `status-file` check); the signed `.app` that reads it is the
      external part left.

### Multi-machine & field testing (require more than one box)
> The multi-supplier path is **proven locally** — two agent processes on one box
> register as distinct workers and a shared job has ≥2 distinct workers commit its
> tasks (`multi-agent`). What remains genuinely needs separate physical hardware:
- [ ] Cross-machine within-class redundancy on real heterogeneous Macs
      (M-series ↔ M-series byte/cosine agreement at scale).
- [ ] Availability / churn measurement on real enrolled devices over weeks.
- [ ] Scheduler behavior under real supply thinness + intermittency.

### Real users (require demand)
- [ ] First external buyer runs a paid job at a cost they confirm beats incumbent.
- [ ] First external supplier receives a real payout. The **alpha manual-export
      rail is built + proven** (`CX_PAYOUT_EXPORT` → owed credits appended to a CSV
      for out-of-band settlement, `TestManualExportPayout`); a real *licensed* rail
      (Stripe Connect/Trolley) moving real money is the external step that remains.

### Later product surface (deliberately out of V1 scope)
- [x] **Plane B buildable seam — LANDED** ([`docs/PLANE_B.md`](docs/PLANE_B.md) §7).
      The `apple_silicon_cluster` horizon is in all three contract files in lockstep;
      summed-memory routing is proven against the unchanged claim filter
      (`TestClusterSummedMemoryRouting`); the topology/re-shard math is pure +
      unit-tested (`agent/src/cluster.rs`) and runnable (`cx-agent cluster-plan`);
      and `ClusterRunner` gates giant models to clusters, surfacing the
      external-substrate boundary (no fake distributed forward pass).
  - [ ] **External/field (Phase 6):** real multi-Mac execution over a Thunderbolt-5
        fabric, the measured all-pairs probe, macOS-26.2 + JACCL, and cross-cluster
        determinism — the substrate the `ClusterRunner` seam plugs into.
- [ ] Optimistic fraud-proof verification (Verde-style bisection) — Phase 6.
- [ ] **Plane C: Compute Autopilot + Exchange Brain** ([`docs/PLANE_C.md`](docs/PLANE_C.md)).
      Pre-canon design for the next expansion: quote/preflight/budget/failure
      prevention on the buyer side, plus learned routing/capacity/pricing
      intelligence on the exchange side. Intentionally bakes two candidate planes
      into one until the quote system and scheduler brain prove they should split.
- [x] **Plane C errata — D0 failure-prevention slice LANDED + proven**
      ([`docs/PLANE_C_ERRATA.md`](docs/PLANE_C_ERRATA.md) §6). The structural fix for
      silent OOM / money drain: `POST /v1/worker/task/{id}/fail` (immediate typed
      failure), `task_failures` + `job_events`, a shared agent⇄control failure
      taxonomy, retryable→requeue-in-seconds / buyer-bad-input→terminal+partial-settle,
      buyer-visible `GET /v1/jobs/{id}/events` + `/failures` (+ `cx`/SDK), and the
      `tasks_ready_unclaimed_idx` claim index proven load-bearing. Proof rows:
      `matrix:TestFailEndpoint{RequeuesImmediately,BadInputTerminal,OnlyClaimingWorker}`,
      `matrix:TestFailureTaxonomyClassification`, live `job-events`,
      `claim-index`. Remaining errata items (budget enforcement, quote
      binding, drift, warm-state, memory estimator) carry forward into Plane D.
- [ ] **Plane D: Local Advantage Engine** ([`docs/PLANE_D.md`](docs/PLANE_D.md)).
      The next frontier before RunPod or a second Apple device: local queue
      acceleration, long-poll wakeups, warm-model routing, memory telemetry,
      streaming/binary artifacts, budget governance, local benchmark lab, and a
      larger proof ledger that keeps pushing speed and efficiency on the hardware
      already available.
- [ ] More workloads (eval, image_gen, LoRA fine-tune, OCR, vision-embeddings,
      synthetic-data) — each needs its model + a verifier strategy; the three
      shipped (`batch_classification`/`json_extraction`/`rerank`) are the pattern.

---

## Where this sits against the action plan
The action plan's **Phase 0–2** engineering (benchmark agent, control plane,
job lifecycle, V1 verification, dogfood network) is **built and locally proven**.
**Phase 3** (paid closed alpha) is unblocked on code — it is now gated only on the
external items above (a payment rail + a legal green-light + one real buyer).
