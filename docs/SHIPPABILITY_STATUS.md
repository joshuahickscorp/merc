# Merc shippability status

Audited against the code on 2026-07-28. Statuses use the goal's vocabulary.
Nothing here is inferred from intent; each row was probed against the tree.

**A `CANARY_PROVEN` receipt is capability evidence, not release authorization.**
`public_capability_allowed` remains false, Level B remains `NO_GO`, and Level C
live money/public launch remains prohibited.

## The gate that controls every money lane

The earlier CAD/USD incompatibility was removed by making settlement currency
explicit. Historical canary evidence proves the CAD path, but the release
candidate is intentionally **SEALED**: it rejects live Stripe activation unless
an operator supplies the exact external activation authority. The formal
release ledger separately requires a complete Stripe **test-mode** matrix and
reconciliation receipt.

That distinction is deliberate: old proof is retained, current credentials are
not assumed, and neither a historical live key nor a local test can promote
Level B or Level C.

## Candidate-bound private-canary evidence — 0/21 lanes CANARY_PROVEN

`scripts/private-canary.py` is now a capability inventory, not a canary
authority. Its old `full_path` boolean promoted partial commands — including
unit tests and recorded-response UI tests — after separately probing that a
runtime or credential existed. That did not prove the command used the
capability, and it did not bind an immutable candidate image or exact commit.

The regenerated inventory at `evidence/canary/private-canary.json` therefore
reports zero candidate-bound lanes and keeps
`public_capability_allowed: false`. A passing lane command is capped at
`TESTED`. Two clean, tracked historical receipts preserve
`REAL_RUNTIME_PROVEN` evidence for three lane labels (batch inference,
embeddings, and realtime), but each is explicitly `candidate_bound: false`.
Fabricated, untracked, dirty, malformed, non-ancestor, incomplete-chain, and
non-conserving receipts are rejected. Only
`scripts/go-closure-canary-rehearsal.sh` may create exact-commit,
immutable-image canary authority.

That formal authority is now hardened too. Every scenario receipt is schema-v2
bound to a fresh run ID, exact commit, immutable image, invocation window, and
the operator-reviewed SHA-256 of a canonical non-writable driver whose bytes
must remain unchanged. Observation sources are closed by scenario. Merc independently
queries PostgreSQL for the two approved buyers/workers, completed workload
all-task runtime/reviewed-build/verification/money chains, cancellations, retries, stale recoveries,
stale-attempt fencing, and webhook retry delivery. Provider-only backup,
Stripe, and alert observations retain strict source-specific contracts. Offline
hostile mutations and a disposable-database corroboration suite run in CI. No
external execution is claimed.

Metal-agent restart authority is independently observable now too. Every
cx-agent process registers a fresh session UUID; PostgreSQL preserves the
session start across same-process re-registration and changes it only when a
new process registers. The formal restart storm freezes an operator-reviewed
adapter digest and exact action receipt, but derives its two-agent restart
claim only from both approved reviewed-build session transitions and requires
those sessions to remain current through the rest of the fault storm. Hostile
receipt and disposable-database tests run in CI. No external restart is claimed.

Real historical execution still matters: Apple M3 Ultra/Metal/Candle completed
batch embeddings and llama.cpp completed realtime with verification and money.
Direct RunPod/vLLM runtime evidence also remains, but its receipt does not show a
Merc request-to-settlement chain. None of those facts is promoted into current
release authorization.

The six previously named product gaps remain: image generation lacks a runtime;
LoRA lacks trainer/evaluator dispatch; TP>1 lacks a real multi-GPU receipt;
official-SDK conformance retains a real-engine incompatibility; external-model
onboarding does not walk the money chain; and alert delivery has not reached a
real receiver.

## Lanes

| lane | status | evidence |
|---|---|---|
| OpenAI-compatible realtime | `REAL_RUNTIME_PROVEN` historical; candidate canary `OPEN` | `[KILL-RT]` was reversed and `KEEP-RT` executed (`DECISION_ZERO_REVERSAL.md`). A real llama.cpp/Metal engine completed the full contract, verification, debit, supplier-payable, positive-margin, and receipt chain (`evidence/canary/real-runtime-realtime.json`). The retained receipt is clean and provenance-checked but predates the candidate, so it cannot mint `CANARY_PROVEN`. Official Python and JavaScript SDK conformance remains `TESTED`: the fake-upstream suite passes, while the real-engine suite retains a model-specific `parallel_tool_calls` incompatibility. |
| RunPod-backed pinned vLLM | direct runtime `REAL_RUNTIME_PROVEN`; Merc chain `TESTED` | Real NVIDIA hardware served a pinned `vllm/vllm-openai:v0.26.0` image and revision-pinned model with SSE (`evidence/runpod/cuda-first-proof.json`). Measured 7081 tok/s at concurrency 128 for $0.0106 per million tokens (`evidence/perf/cuda-throughput-correction.json`). That provider receipt contains no Merc contract, verification, debit, supplier payable, or Merc contribution, so it is not end-to-end canary authority. |
| Object storage | `TESTED`; candidate canary `OPEN` | Retention, deletion and tenant isolation were exercised against a live store, but the retained inventory has no exact-candidate full-chain receipt. `control/job_object_retention.go` purges 30 days after terminal, holds while any dispute is unresolved, and refuses a period inside the 7-day filing window; mutation-checked 4/4. Workers hold no S3 credential at all — only per-key presigned URLs. |
| Image generation | `IMPLEMENTED` (governance `TESTED`) | `control/image_generation.go` + `POST /v1/images/generations`, 81st route, buyer-owned in the authorization matrix. **Governance is the finished part**: size allowlist, n cap, prompt bound, and refusal of `b64_json` (an inline image never enters object storage, so it would have no retention, erasure or dispute-evidence path). Content policy refuses CSAM, non-consensual intimate imagery, photorealistic real-person likeness and forged documents, checking two normalisations because separator evasion defeats either one alone — my own adversarial test caught that. Refusals name the rule and never echo the prompt. Licence gate is separate from the text one because open image licences (OpenRAIL-M/++) attach use restrictions the licensee must pass downstream, and merc resells generation. Mutation-checked 5/5. **`NOT_IMPLEMENTED` for the lane itself**: there is no image runtime, so an acceptable request returns 503 rather than an invented result. No contract, no supplier, no settlement. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| LoRA training | `IMPLEMENTED` (settlement `TESTED`) | `control/lora_settlement.go`. **Outcome-aware settlement is the finished part.** The price splits: a compute floor the supplier is owed however the run turns out, and an outcome bonus contingent on an independent evaluation. Each of the three alternatives is wrong on its own — supplier-bears-risk means a supplier can lose a day's revenue to someone else's dataset (and merc's pricing governance already refuses prices that don't cover electricity); buyer-bears-risk makes 'outcome-aware' just billing with a nicer name; merc-bears-risk is unbounded. merc's cut comes from the floor, so it never earns more from a failed run than a successful one — asserted. Evaluation must be independent at the **account** level, not just the worker: two worker ids on one account are not two opinions. A held-out set that leaked into training is refused, because the improvement would measure memorisation. Conservation proven over 20,000 randomised settlements; mutation-checked 7/7. **`NOT_IMPLEMENTED` for the lane**: no trainer, no evaluator dispatch, no adapter deployment, no GPU. |
| External-model onboarding | `TESTED` | `control/model_onboarding.go` runs at process start and panics on a model merc cannot resell. Licence is an allowlist (an unrecognised one is refused, not assumed permissive); `remote_code=true` is refused unconditionally because it runs repo-supplied code on third-party supplier hardware; a required attribution must appear in the shipped `NOTICE`, checked against the real file by `TestCatalogueAttributionAppearsInNotice` (mutation-checked: removing "Built with Llama" from NOTICE fails CI). Both catalogue models now declare licence, URL, commercial-use and remote-code posture. `scripts/onboard-model.py` is now the live half: it takes a model through policy, identity, a real smoke completion, determinism at temperature 0, and a MEASURED benchmark against a running runtime, then emits a runtime profile carrying the measured throughput -- so a catalogue price derives from what the model did, not what someone assumed. Proven on Qwen2.5-3B-Instruct: admitted at 142 tok/s, determinism confirmed. `scripts/onboard-model-canary.sh` asserts four refusals too (non-commercial licence, remote_code, an alias the runtime does not serve, an unpinned revision), because a gate that only ever says yes is not a gate. Still `TESTED` not `CANARY_PROVEN`: it does not walk the money chain. |
| Single-host multi-GPU | `IMPLEMENTED` (local authority `TESTED`) | `control/multi_gpu_admission.go`, `control/realtime_placement.go`, and the compiled `cx-agent vllm` path. The vLLM adapter was previously an orphan source file; it is now built, exposed as a CLI command, and registers explicit CUDA class, physical GPU count, per-GPU memory, committed memory, and `nvlink`/`pcie`. The container receives exactly devices `0..TP-1`, never `--gpus all`. The control plane selects the smallest admissible degree, copies the exact placement JSON+digest from offer to contract, exposes it on the receipt, and makes contract placement immutable in PostgreSQL; historical contracts remain readable without invented topology, while legacy offers are drained until they re-register. Per-rank overhead does **not** shrink; PCIe is capped at TP=2; attention heads must divide the degree; undeclared multi-GPU interconnect is refused. 50,000 randomised planner cases plus offer→contract→receipt and tamper tests; mutation-checked 9/9. **Still `EXTERNALLY_BLOCKED` for a sellable TP>1 lane**: the embedded profile is TP=1 and `UNPROVEN`; no TP>1 profile with measured weight/overhead requirements and no real multi-GPU receipt exists. The code does not promote that missing evidence into a claim. |
| Buyer dashboard | `TESTED`; candidate canary `OPEN` | `web/buyer.html`. Its live script signs in to a running Merc and opens the workspace, but it does not emit an exact-candidate receipt and therefore cannot promote itself. |
| Supplier console | `TESTED`; candidate canary `OPEN` | `web/supplier.html` behind `GET /supplier`. The current automated command uses recorded control-plane responses to prove worker-token auth, ledger-granularity money, four payout-rail states, and refusal behavior. Recorded responses are deliberately capped at `TESTED`; mutation-checked 3/3. |
| Public price board | `TESTED`; candidate canary `OPEN` | The published page's arithmetic matches the server's, including confidence-weighted median selection. The board's third-party observations are down-weighted, not decisive. No exact-candidate buyer-to-receipt exercise is retained. |
| Python SDK | historical live exercise; candidate canary `OPEN` | `sdk/python/merc/`, clean-room install verified. The live script submits a job to a running Merc, waits for a worker, and validates the result, but does not yet emit source-bound evidence. |
| TypeScript SDK | historical live exercise; candidate canary `OPEN` | `sdk/typescript/` builds to `dist/`; its live run exposed and locked tests for the idempotency header, JSONL input shape, and cancel route. It does not yet emit source-bound canary evidence. |

## REAL_RUNTIME_PROVEN: batch embeddings, 2026-07-27

merc's original supply is Apple Silicon running candle on Metal, and this machine
is an M3 Ultra — an admitted `apple_silicon_ultra` host. A real GPU rental was
never required to prove the batch lane. I had been treating every lane as
GPU-blocked, which under-reported what merc can actually do.

The shipped `cx-agent` binary registered (1,980 embeddings/sec measured on
Metal), claimed a real buyer job, and the whole chain completed:

| step | result |
|---|---|
| buyer request | `POST /v1/jobs`, 3 rows, idempotency-keyed |
| merc contract | job `fd1999ac`, 2 tasks, estimate $0.000250 |
| scheduler | dispatched to worker `…b1` |
| **real runtime** | candle on Metal, Apple M3 Ultra — not a fake, stub or httptest server |
| result | 3 × 384-dim embeddings, fetched through a presigned URL |
| verification | `honeypot-checked`, 1 passed / 0 failed |
| buyer debit | −$0.000250 |
| supplier payable | +$0.000002 |
| merc contribution | +$0.000248 (positive) |
| receipt | `evidence/canary/real-runtime-embed.json` |

### The historical small-job supplier hole is fixed

The 3-row job gave merc **99.2%** of the buyer charge. Measured rather than
assumed: `MERC_CONTROL_PLANE_PER_TASK_USD` is a fixed 100 micro-USD per task
against a 125 micro-USD per-task charge, so fixed cost eats 80% before anything
is split and the supplier's share rounds to 1 micro-USD. A 400-row job on the
same worker splits **49/51**.

This is the same failure class as the LoRA compute floor truncating to zero: a
fixed cost dominating a small quote leaves a party with approximately nothing,
through arithmetic rather than policy. A supplier serving only minimum-size jobs
would have worked for almost nothing. `BuildEconomicPlan` now derives and
freezes a minimum billable base-compute floor so every executable physical-work
plan that charges the buyer reserves at least one micro-USD of supplier
liability. `estimateJobUSD` also floors positive sub-micro work before the plan
is built. The figures above remain historical measurements, not a claim about
current quotes.

### Two bugs this run surfaced

- A stale control binary dispatched under an older runtime-authority matrix than
  the worker had attested to, and the agent refused every task. That gate works,
  and it is exactly the re-attestation consequence recorded when `matrix_version`
  was bumped — but nothing had exercised it until a real worker did.
- The control plane refuses a job it cannot verify (`no usable honeypot is
  seeded for this workload`) rather than running it unverified. Fail-closed,
  confirmed by hitting it.

## REAL_RUNTIME_PROVEN: OpenAI-compatible realtime, 2026-07-27

I had reported this lane blocked on CUDA. It was not. **"Real runtime" and
"CUDA" are different capabilities**, and conflating them is what made me report
a lane blocked that a locally installed engine could serve.

`llama-server` (llama.cpp, already installed) loaded the exact GGUF merc's
catalogue pins — `Llama-3.2-1B-Instruct-Q4_K_M`, already in the HF cache from
the agent's own benchmark — and served merc's realtime lane on Metal.

| step | result |
|---|---|
| worker offer | registered `ACTIVE`, profile sha256 matched |
| buyer request | `POST /v1/chat/completions`, idempotency-keyed, `X-Merc-Max-USD` |
| merc contract | `b59d6380`, authorized $0.000035 |
| **real runtime** | llama.cpp on Metal, real weights, real tokens |
| result | a real 31-token completion |
| verification | `PASSED`, receipt state `VERIFIED` |
| authorization | `CAPTURED` — $0.000019 captured, $0.000016 released |
| buyer debit | $0.000019 |
| supplier payable | $0.000002 |
| merc contribution | **$0.000017 (positive)** |
| receipt | `rcp_559d4988…`, with `stream_root_sha256` and `output_commitment` |

### Measured overhead: merc adds 3.9 ms

merc median 58.4 ms vs 54.4 ms straight at the engine, over 5 samples.

**Not publishable, and the harness says so itself.**
`scripts/realtime-parity-benchmark.py` reports `UNATTESTED_HARNESS_RUN` and
`public_claim_allowed: false`, because llama-server presents no runtime
attestation header. The number is real; the gate that stops it becoming a
marketing claim is also real, and it fired without being asked to.

### What is still genuinely CUDA-blocked

The `runpod_vllm` lane — NVIDIA hardware and a pinned, digest-addressed vLLM
image — is now tracked as its own lane rather than hidden inside "realtime". No
Apple Silicon engine substitutes for it. Same for image generation and LoRA.
Multi-GPU now has a compiled adapter and frozen local placement authority, but
remains externally blocked on a real TP>1 profile, host, benchmark and receipt.

## Money defect found by running the SDK against a live merc

Running the shipped Python SDK against a running control plane — something no
test had ever done — surfaced a defect in merc's pricing that no unit test could
have, because it only appears after the catalogue is repriced from a **real**
supplier's measured throughput.

### Historical failure: the supplier was paid zero while the buyer was charged

| units | buyer charged | supplier paid |
|---|---|---|
| 1–4 | *rejected*: `base_compute_usd must be finite and positive` |
| 10 | $0.000124 | **$0.000000** |
| 100 | $0.000125 | $0.000001 |
| 1,000,000 | $0.018000 | $0.014400 |

Between roughly 5 and 99 units the old plan was executable, the buyer was
charged, and `SupplierPayoutPerTaskUSD` was exactly zero. This was not the
sub-cent carry the accrual path handles — the payout was 0, so nothing was
accrued at all and Merc recorded no obligation to whoever performed the work.
`roundEconomicUSD(computePerTask * SupplierShare)` rounds 0.0000000144 to zero.

### The causal chain is perverse

```
a real supplier benchmarks at 1,980 embeddings/sec on an M3 Ultra
  → merc reprices the catalogue from measured supplier throughput
  → the per-1k price falls to $0.000018
  → small jobs' base compute rounds to zero at micro-USD granularity
  → the supplier is paid nothing, or the job is rejected outright
```

**A faster supplier makes small jobs unpayable, then unbuyable.** Nobody chose
that; it falls out of rounding.

### This is the third instance of one failure class

1. LoRA compute floor truncating to zero at small quotes — **fixed**, with a
   minimum quote derived from the share constants.
2. Supplier share collapsing to 0.8% on a 3-row job — fixed per-task
   control-plane cost dominating. **Recorded.**
3. Supplier payout rounding to exactly zero between 5 and 99 units —
   **fixed by the derived minimum-billable floor**.

`control/small_job_economics_test.go` now asserts the corrected behavior:
positive buyer charge for physical work implies strictly positive supplier
liability, and buyer charge covers supplier plus control-plane cost across the
former zero-payable window, multi-task shapes, and multiple supplier shares.

## Three TypeScript SDK defects, found by running it against a live merc

The TS SDK had 12 passing unit tests. Every one used a stub `fetch` that
accepted whatever it was sent, so none could catch a client speaking a shape
merc rejects. **The shipped client could not submit a job at all.**

| defect | what merc did | why the tests missed it |
|---|---|---|
| No `Idempotency-Key` header | `400` on every `submitJob` | the stub never inspected headers |
| `input` sent as an array | `400 input must be a JSONL string` | a test asserted the array shape as correct, calling it "matching the Python SDK" — it never matched; Python has always serialised to JSONL |
| `cancelJob` → `POST /v1/jobs/{id}/cancel` | route not served | the stub answered any URL |

All three fixed, the test that pinned the wrong shape rewritten, and three new
tests added that assert the header, the JSONL serialisation and the cancel
route. 12 tests → 15.

Both SDKs now have a **live** lane: `scripts/sdk-live-python.py` and
`scripts/sdk-live-typescript.mjs` submit a real job to a running merc, wait for
a real Metal worker, and validate the real result. Those runs found defects and
remain valuable historical compatibility evidence, but they do not emit an
immutable-image, exact-commit receipt. They therefore remain below
candidate-bound `CANARY_PROVEN`.

## Repository boundary and rename

| item | status | evidence |
|---|---|---|
| VisionMCP extracted | `TESTED` | VisionMCP remains a separate repository, with **zero** files tracked by merc. `scripts/validate-repo-boundary.py` runs in `make ci` and fails if any VisionMCP path enters merc's tree. merc currently has 529 tracked files and **113,478** owned LOC; none is VisionMCP. The untracked `live-instrument` design archive and its VisionMCP-linking `.mcp.json` remain preserved in their separate worktree and are intentionally excluded from this candidate. |
| Rename zero-residue audit | `TESTED` | `scripts/rename-residue-audit.py`, in `make ci`. FROZEN 256 / BLOCKED 380 / **RESIDUE 0**. Frozen and blocked classes are itemised with a per-identifier reason in `docs/RENAME_REGISTER.md` §5. |

## Money and operations

| item | status | evidence |
|---|---|---|
| Supplier accrual | `TESTED` | `control/supplier_accrual.go`. Micro-USD conservation proven under 24 concurrent claims, 240 randomized orderings, and 9/9 mutation detection. |
| Payout reconciliation | historical provider exercise; candidate `TESTED` / formal gate `OPEN` | The CAD/USD mismatch that blocked this is gone: settlement currency is configuration (`control/currency.go`) and the platform settles CAD. Supplier accrual, minor-unit carry and the sole ledger writer are unchanged. The current candidate still requires the formal test-mode payout and reconciliation matrix. |
| Stripe API contract | `TESTED` | Every product and operator-script Stripe request pins `2025-06-30.basil` immediately before network I/O. Billing and Connect endpoint creation additionally pins webhook payload rendering; existing null/mismatched endpoint versions fail closed because Stripe cannot update that field in place. Signed events must carry that exact version and the expected test/live mode before any effect runs. Charges, setup, reads, Connect, payouts, refunds, reversals, probes, webhook management, and later signed event shapes cannot drift with the account default. Static bypass guards, operator-script self-tests, and transport/operation-shape tests cover the boundary; mutation-checked 5/5. |
| Stripe Sandbox authority | `TESTED` scaffold; provider execution `OPEN` | The Level B manifest now explicitly selects `test` payment mode, the Stripe provider, and the mandatory CAD settlement currency; production defaults can no longer leave the configured canary structurally SEALED. Preflight and matrix share one authority for API version, CAD provider objects, a distinct payout-enabled Canadian connected account, Stripe's Canadian success/failure payout fixtures, distinct endpoint IDs/secrets, exact staging host paths, complete event inventories, and sanitized receipts. Signed no-value probes require the real handler/database to classify terminal-first application as `applied`, the older opening fact as `stale_ignored` behind rank 30, and an exact replay as `duplicate`; the matrix no longer self-asserts those outcomes. Offline adversarial tests reject URL/version/event/country/currency/ID drift before network access. No claim of provider execution is made. |
| Aggregated billing / prepaid | `IMPLEMENTED` | 4 references in `control/accounts.go`; charge batching reworked so the age trigger no longer fires at Stripe's $0.50 floor. |
| Refunds / disputes | `IMPLEMENTED` | 21 files reference disputes. Transfer reversal has never met real Stripe. |
| Stripe sandbox end to end | historical capability evidence; formal gate `OPEN` (`NO_GO`) | Historical CAD-settlement provider evidence is retained without promotion. The current formal candidate still lacks the complete test-mode matrix and provider-reconciliation receipt required by `P1-STRIPE-TEST`; live activation is sealed and Level C remains prohibited. |
| Production deployment / TLS | `EXTERNALLY_BLOCKED` | No SSH key in session. Droplet still serves the pre-session build (`/version` 404s). |
| Backup / restore | `TESTED` | Gates present and passing. |
| Alerts / status / rollback | `IMPLEMENTED` | 24 alerts validated; delivery to a real receiver unproven. |
| Licence scope | `PARTIAL` | Split done; `validate-license-register` deliberately red pending counsel. |
| Buyer/supplier/privacy/refund terms | `EXTERNALLY_BLOCKED` | Drafts marked DO NOT PUBLISH pending counsel. |
| Internal security review | `PARTIAL` | Adversarial input, fuzzing, tenant-isolation and mutation testing done this session; found 3 real defects. |
| Independent pentest | `EXTERNALLY_BLOCKED` | Requires an external firm. |

## Rename

Tier 1 landed (brand prose, Go module `merc/control`, Python distribution
`merc`, website copy). Tier 2 and the FREEZE list are recorded in
`docs/RENAME_REGISTER.md` and are unchanged: env vars, registry paths, the
GitHub repo slug and production directories are **EXTERNAL**, and hash domains,
4-byte binary magics, live credential prefixes and signed receipts are
**FREEZE — never rename**. Renaming those would falsify receipts or silently
invalidate every previously computed digest.

## VisionMCP

`EXTRACTED` and deliberately separate. VisionMCP has its own repository and
history. It is not tracked, built, counted, or shipped by Merc; the repository
boundary validator reports zero foreign paths. The committed
`design/computexchange-live-instrument` branch is already an ancestor of this
candidate. Its remaining untracked 1.3 GB internal-design archive and
VisionMCP-linking `.mcp.json` are preserved in that worktree, not merged here.

## Honest summary

Merc is a buildable, locally proven Level A software candidate with batch,
realtime, and historical single-GPU CUDA/vLLM canary evidence. Quote-bound
compute and realtime placement authority are now frozen into immutable
contracts and receipts. TP>1, image generation, LoRA execution, real alert
delivery, and the formal external release exercises remain unproven.

The machine-derived score remains **83/100**, with **P0=0** and eight external
P1 gates. Level B is `NO_GO`; Level C live money/public launch is prohibited.
No historical credential, deployment, or canary receipt overrides those gates.
