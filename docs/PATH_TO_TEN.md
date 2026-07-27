# Path to 10 — what an agent can finish alone, and what only you can do

**Method:** Grok planned the machine half, Claude planned the product half, both
under one constraint — *only work needing no human decision, credential, or
approval goes in the plan body*. Everything else is quarantined at the bottom.

**Read this first: 10 is not reachable, and no plan should promise it.** Three of
these facets are capped by having no users, and one is capped by not owning
hardware. The honest target is **≈7 autonomously, ≈8.5 once your list is done**,
and the last stretch is demand, which is not a commit. What follows is the most
that can truthfully be built.

---

## Ceiling analysis

| Facet | Now | Max alone | Max after your list | What caps it |
|---|---:|---:|---:|---|
| Claims integrity | 8 | **9** | 9.5 | External attestation, real page receipt |
| Money safety | 7 | **9** | 9.5 | Stripe test-mode matrix needs a full sandbox secret set |
| Architecture | 6.5 | **9** | 9 | Only product-surface decisions cap it; nothing human-blocked |
| Security | 7 | **8** | 9 | External pen-test; production IAM and real CIDRs |
| Operability | 6 | **8** | 9 | A real paging destination and a real on-call human |
| Legal & governance | 6 | **7** | 9 | Repo visibility, LICENSE choice, counsel |
| DX & support | 5 | **7** | 8.5 | PyPI publish is blocked on the licence; support contact is a person |
| Inference performance | 3.5 | **6** | 8 | vLLM-on-CUDA parity needs a working GPU credential |
| Unit economics | 5 | **6.5** | 7 | A price is only validated by someone paying it |
| Competitive position | 1.5 | **2.5** | 3 | Demand is not a commit |
| **Overall** | **5.5** | **≈7** | **≈8.5** | |

---

## The GPU question, answered

You asked whether we can connect to RunPod for hardening. Three findings:

**1. The stored RunPod credential is dead.** `.secrets/runpod.env` holds a
50-character `RUNPOD_API_KEY`. It returns **HTTP 401** against
`rest.runpod.io/v1/pods` and an error envelope from the GraphQL endpoint. The
integration is writable — their API is plain HTTPS and needs no CLI — but it
cannot authenticate today. A replacement key is a 30-second action and it is on
your list.

**2. Most of what the GPU run was *for* can be done without one, today.** The
audit's sharpest performance finding was that the realtime lane had *never
executed against a real engine* — its only evidence file records
`upstream: "httptest fake SSE server"`. That is fixable locally right now:

- `llama-server` is installed, and **the exact GGUF the catalogue prices**
  (`Llama-3.2-1B-Instruct-Q4_K_M`) is already cached on this machine.
- I started it and drove it: real SSE streaming, correct `data: [DONE]`
  terminator, and **usage in the final chunk** — which is precisely what
  `control/realtime.go` forces via `stream_options.include_usage` and settles
  on. It reported 358 tok/s predicted on Metal.

So the gateway, the contract lifecycle, settlement from real usage, and SDK
conformance can all be proven against a genuine third-party OpenAI-compatible
server serving the production model. That removes the "fake upstream" finding
entirely. It does **not** produce a vLLM-on-CUDA parity number.

**3. Docker is now available.** `colima` was installed but not running; I started
it, so `docker` works. That unlocks the local Prometheus/Alertmanager fire →
receive → resolve loop and a production-shaped restore drill.

**Net:** performance can reach ~6 alone. Reaching 8 needs one working GPU
credential — and with one, the whole run is scriptable end to end (provision,
serve the pinned `vllm/vllm-openai` image, run
`scripts/realtime-parity-benchmark.py --attest-real-vllm`, tear down), because
the failure mode that matters is forgetting to destroy the pod, and that is
automatable.

---

## Plan — autonomous phases, ordered by (score gained) ÷ effort

### Phase 1 — Prove the money design (~4 d) · money 7→9

The largest single gap anywhere in the project. **14 of 15** money and scheduling
entry points still have zero test callers; only `ClaimTasksTx` is covered. The
design is now good and almost none of it is proven.

1. **One shared fixture** (~0.5 d) — a single seeding helper for buyer, active
   supplier + worker with authorized capability, job with a valid economic-plan
   snapshot, held ledger credit, charged job. Reuse the two existing dialects in
   `scheduler_ask_claim_integration_test.go` and `dispute_payout_integration_test.go`;
   do not invent a third.
2. **`SubmitJobTx`** — commits job + tasks + plan; plan/task-count mismatch fails
   closed with no job row; **submit must mint no money**.
3. **`CompleteTaskTx`** — wrong worker, wrong attempt, already-terminal all fail
   closed; concurrent double-complete yields exactly one success.
4. **`FinalizeJobTx` / `completeJobEconomics`** — `actual_usd` equals the sum of
   buyer-charge rows; a second finalize does not double-insert the SLA premium.
5. **`reservePayoutFunding` / `AuthorizePayoutSubsidy` / `FinalizePayout`** —
   under-fund and double-pay are the failure modes; test them under concurrency.
6. **`resolveDispute`** — freeze blocks payout; terminal resolution controls it.
7. **Property test on the ledger writer** — random micro amounts round-trip
   through `NUMERIC(12,6)` exactly.

*Needs: local Postgres only.*

### Phase 2 — Kill the fake upstream (~1.5 d) · performance 3.5→6, claims 8→8.5

8. Stand `llama-server` up on the cached GGUF as a managed test fixture.
9. Repoint `scripts/realtime-openai-{node,python}-conformance` and the realtime
   integration tests at it; emit evidence with `real_runtime_executed: true` and
   the engine's real identity.
10. Measure and record **actual** gateway overhead p50 against a real engine —
    the `<10 ms` figure in `PRICE_BOARD_METHOD.md` has never been measured.
11. Fix `realtime.go`'s `maxInputTokens := int64(len(upstreamBody))` — byte count
    used as token count, so a 32,768-token context is really ~7,000.
12. `temperature` is accepted and silently discarded by the executor
    (`argmax`). Implement sampling or reject the parameter; silent acceptance is
    the worst of the three.

*Needs: the local engine. No network, no credential.*

### Phase 3 — Operability proven, not just configured (~2 d) · operability 6→8

13. Bring up Prometheus + Alertmanager under the now-working docker, fire a
    synthetic alert, receive it at a local sink, resolve it, and store delivery
    IDs. That converts "alerting configured" into "alerting demonstrated".
14. Restore drill against ≥10k jobs / ≥1k ledger rows / ≥1k objects with a
    **measured** RTO in the receipt; fail the drill on toy row counts.
15. Refuse a corrupted backup envelope, and prove it.

*Needs: docker (now available) and local Postgres/MinIO.*

### Phase 4 — Architecture (~3 d) · architecture 6.5→9

16. `createJob` is still 477 lines — extract the remaining store-dependent
    stages behind narrow interfaces.
17. Put `Server` behind a narrow store port so the 45 handlers are testable
    without live Postgres and MinIO.
18. Split `package main` (142 files) into domain packages. Do this **after**
    Phase 1, so the money paths are protected by tests before they move.

### Phase 5 — DX and economics (~2.5 d) · DX 5→7, economics 5→6.5

19. SDK: retries with backoff, typed errors surfacing the new `code`, `py.typed`,
    the missing `example.py`. Build and verify the wheel locally — publishing is
    blocked on the licence.
20. Complete the error-code enum across all 264 error sites, not just the
    touched ones.
21. Build `pricing/board.json` from public competitor prices with fetch dates,
    and drive the catalogue from it instead of the cost-plus laptop formula.
22. A quickstart that a stranger can follow to a successful call against a local
    stack, verified by running it in a clean shell.

---

## YOUR LIST — the only things blocking the rest

Ranked by what they unblock per second of your time.

| # | Action | Time | Unblocks |
|---|---|---|---|
| 1 | `gh repo edit joshuahickscorp/merc --visibility private` | 5 s | The only open legal exposure |
| 2 | **A working RunPod API key** (or any GPU provider) into `.secrets/runpod.env` | 30 s | Performance 6→8; real vLLM parity; the CUDA lane's entire evidence base |
| 3 | **Decision Zero** — `[KILL-RT]` or `[KEEP-RT]`, see `docs/DECISION_ZERO.md` | one call | ~Half the remaining roadmap; ends dual-tracking |
| 4 | Choose a LICENSE | minutes | PyPI publish → DX 7→8.5; resolves `NOTICE` contradiction |
| 5 | Real support + incident contacts in `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md` | minutes | `make release-gates`; claims 9→9.5 |
| 6 | A real paging destination (PagerDuty/Opsgenie/Slack webhook) | minutes | Operability 8→9 |
| 7 | Stripe sandbox secret set — you said later, so it is sequenced last | ~1 h | Money 9→9.5 |
| 8 | Counsel on `THIRD_PARTY_LICENSES.md` (both catalogue models are BLOCKED) | external | Legal GO; serving claims |
| 9 | External pen-test | external | Security 8→9 |
| 10 | Trademark call on the `mercmerc.net` collision | external | Brand, before acquisition spend |

**Two things are irreducible and no list closes them:** you have no users, and
you do not own GPUs. Competitive position stays ~3 until someone pays, and
performance stays capped until the hardware the control plane admits is hardware
buyers want. Every item above is worth doing; none of them is a substitute for
those two.

## Not doing, deliberately

**Git history purge.** 5.66 GiB of Blender blobs against a ~8 MB tracked tree.
`git filter-repo` is executable locally but rewrites every SHA on a branch
already published as `origin/release/rc1-go-closure`, and only pays off after a
force-push that breaks every clone and CI cache. Force-push is irreversible
shared state. `.gitignore` already blocks re-entry. Use shallow clones; if you
want this done, it is a human-scheduled operation, not an agent one.
