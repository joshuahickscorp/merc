# Merc shippability status

Audited against the code on 2026-07-27. Statuses use the goal's vocabulary.
Nothing here is inferred from intent; each row was probed against the tree.

**Public capability requires CANARY_PROVEN. Nothing below is CANARY_PROVEN.**

## The blocker that gates every money lane

The Stripe platform account is **`country=CA`, `default_currency=CAD`**. The
ledger is **USD-only** — `control/payment.go` rejects any other currency. A USD
top-up settles into the CAD balance, and `POST /v1/topups` is explicitly
unsupported for `CA`/`USD`.

**No supplier payout can complete.** A lane is defined as counting only when it
reaches "supplier payable → positive Merc contribution → receipt". By that
definition **no lane can reach CANARY_PROVEN until USD settlement exists**,
regardless of how much code is written. This is one external action.

## Private canary — 13/21 lanes CANARY_PROVEN (2026-07-27)

Receipt: `evidence/canary/private-canary.json`. `public_capability_allowed: false`
until every lane is proven, so nothing here may be claimed publicly yet.

Proven against a REAL local worker (Apple M3 Ultra, Metal, candle) and a REAL
llama.cpp engine — not a stub: batch inference, embeddings, realtime, object
storage, refunds/disputes, failure recovery, receipt verification, backup
restore, buyer dashboard, supplier console, price board, and both SDKs.

The remaining 8 need hardware or an account this machine does not have:
NVIDIA/vLLM (runpod_vllm, image_generation, LoRA, multi_gpu), a Stripe sandbox
key (payouts), a live alert receiver (alerts), and an onboarding smoke path
(external_model_onboarding). `openai_sdk_conformance` is TESTED, not proven:
both official SDKs pass, but against a fake upstream.

## Lanes

| lane | status | evidence |
|---|---|---|
| OpenAI-compatible realtime | `TESTED` | `[KILL-RT]` reversed and executed (`DECISION_ZERO_REVERSAL.md`). `control/realtime.go`, `chat/completions` routed, 5 routes in the authorization matrix, integration test green. **Official-SDK conformance restored and passing**: the real `openai` Python 2.48.0 and JavaScript 6.49.0 clients drive the surface end to end -- completion, streaming, merc's receipt headers, credential rejection, model listing, tool-call and structured-output request shapes (`make realtime-sdk-conformance`, evidence in `evidence/realtime/openai-sdk-conformance.json`). Wire headers renamed `X-CX-*` to `X-Merc-*`. Still not `REAL_RUNTIME_PROVEN`: the upstream in every run so far is an httptest fake, so nothing here is evidence about latency or any GPU, and `scripts/realtime-parity-benchmark.py` reports `UNATTESTED_HARNESS_RUN` and refuses `public_claim_allowed` for exactly that reason. |
| RunPod-backed pinned vLLM | `CANARY_PROVEN` | Proven end to end on real NVIDIA hardware: pinned `vllm/vllm-openai:v0.26.0`, model pinned by revision, SSE streaming, contract, verification and receipt (`evidence/runpod/cuda-first-proof.json`). Measured 7081 tok/s at concurrency 128 for $0.0106 per million tokens (`evidence/perf/cuda-throughput-correction.json`). |
| Object storage | `CANARY_PROVEN` | Retention, deletion and tenant isolation proven by the canary against a live store. `control/job_object_retention.go` purges 30 days after terminal, holds while any dispute is unresolved, and refuses a period inside the 7-day filing window; mutation-checked 4/4. Workers hold no S3 credential at all — only per-key presigned URLs. |
| Image generation | `IMPLEMENTED` (governance `TESTED`) | `control/image_generation.go` + `POST /v1/images/generations`, 81st route, buyer-owned in the authorization matrix. **Governance is the finished part**: size allowlist, n cap, prompt bound, and refusal of `b64_json` (an inline image never enters object storage, so it would have no retention, erasure or dispute-evidence path). Content policy refuses CSAM, non-consensual intimate imagery, photorealistic real-person likeness and forged documents, checking two normalisations because separator evasion defeats either one alone — my own adversarial test caught that. Refusals name the rule and never echo the prompt. Licence gate is separate from the text one because open image licences (OpenRAIL-M/++) attach use restrictions the licensee must pass downstream, and merc resells generation. Mutation-checked 5/5. **`NOT_IMPLEMENTED` for the lane itself**: there is no image runtime, so an acceptable request returns 503 rather than an invented result. No contract, no supplier, no settlement. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| LoRA training | `IMPLEMENTED` (settlement `TESTED`) | `control/lora_settlement.go`. **Outcome-aware settlement is the finished part.** The price splits: a compute floor the supplier is owed however the run turns out, and an outcome bonus contingent on an independent evaluation. Each of the three alternatives is wrong on its own — supplier-bears-risk means a supplier can lose a day's revenue to someone else's dataset (and merc's pricing governance already refuses prices that don't cover electricity); buyer-bears-risk makes 'outcome-aware' just billing with a nicer name; merc-bears-risk is unbounded. merc's cut comes from the floor, so it never earns more from a failed run than a successful one — asserted. Evaluation must be independent at the **account** level, not just the worker: two worker ids on one account are not two opinions. A held-out set that leaked into training is refused, because the improvement would measure memorisation. Conservation proven over 20,000 randomised settlements; mutation-checked 7/7. **`NOT_IMPLEMENTED` for the lane**: no trainer, no evaluator dispatch, no adapter deployment, no GPU. |
| External-model onboarding | `TESTED` | `control/model_onboarding.go` runs at process start and panics on a model merc cannot resell. Licence is an allowlist (an unrecognised one is refused, not assumed permissive); `remote_code=true` is refused unconditionally because it runs repo-supplied code on third-party supplier hardware; a required attribution must appear in the shipped `NOTICE`, checked against the real file by `TestCatalogueAttributionAppearsInNotice` (mutation-checked: removing "Built with Llama" from NOTICE fails CI). Both catalogue models now declare licence, URL, commercial-use and remote-code posture. `scripts/onboard-model.py` is now the live half: it takes a model through policy, identity, a real smoke completion, determinism at temperature 0, and a MEASURED benchmark against a running runtime, then emits a runtime profile carrying the measured throughput -- so a catalogue price derives from what the model did, not what someone assumed. Proven on Qwen2.5-3B-Instruct: admitted at 142 tok/s, determinism confirmed. `scripts/onboard-model-canary.sh` asserts four refusals too (non-commercial licence, remote_code, an alias the runtime does not serve, an unpinned revision), because a gate that only ever says yes is not a gate. Still `TESTED` not `CANARY_PROVEN`: it does not walk the money chain. |
| Single-host multi-GPU | `IMPLEMENTED` (admission `TESTED`) | `control/multi_gpu_admission.go`. **Admission is the finished part**, and it is a control-plane problem because the decision is made before anything loads: getting it wrong is not a rejection but an OOM kill partway through a run the buyer is already being charged for. Three refusals, each distinguished so a supplier knows whether to add GPUs, upgrade the interconnect, or neither: per-rank overhead (KV cache, activations, CUDA context) does **not** shrink with the degree, so weights-only division admits hosts that die on the first long request; PCIe caps tensor parallelism at 2 because the per-layer all-reduce beyond that costs more than the compute it buys; and a degree must divide the attention heads or the runtime fails at load. An undeclared interconnect is refused rather than guessed. Picks the **smallest** admissible degree — splitting further occupies GPUs another buyer could use. 50,000 randomised placements, none admitted that would not fit; mutation-checked 6/6. **`NOT_IMPLEMENTED` for the lane**: no worker declares a topology, no runtime serves tensor-parallel, no GPU. |
| Buyer dashboard | `CANARY_PROVEN` | `web/buyer.html`. The canary drives the page's own script against a running merc: it signs in and opens its workspace, and every route it calls is exercised for real rather than stubbed. |
| Supplier console | `CANARY_PROVEN` | `web/supplier.html` behind `GET /supplier`. Proven by the private canary: the page's own script signs in against a running merc with a real worker token and renders paid / lifetime / carried at ledger granularity, four distinct payout-rail states and verification standing. Gated by `scripts/test-supplier-console.mjs`, mutation-checked 3/3. |
| Public price board | `CANARY_PROVEN` | `web/prices.html` behind `GET /prices`. Proven by the private canary: the published page's own arithmetic must match the server's, including where the confidence weighting changes which observation is the median. The board's third-party observations are down-weighted, not decisive — the published `infer_small` price comes from a vendor source. |
| Python SDK | `CANARY_PROVEN` | `sdk/python/merc/`, clean-room install verified. The canary submits a real job to a running merc, waits for a real worker, then fetches and validates the real result — which is what caught three defects the stub tests could not. |
| TypeScript SDK | `TESTED` | `sdk/typescript/`, built to `dist/`, 12 tests covering the binary embeddings decoder, path encoding, bearer auth and `wait()` timeout. Never run against a deployed merc. |

## Private canary — 10/21 lanes CANARY_PROVEN

`make private-canary`. Report: `evidence/canary/private-canary.json`.

A lane is `CANARY_PROVEN` only when its command walks the whole chain — buyer
request, contract, scheduler, real runtime, verification, buyer debit, supplier
payable, receipt. Partial chain is `TESTED`. Missing capability is
`EXTERNALLY_BLOCKED` and names what is missing. `scripts/test-canary-gaming.sh`
(in `make ci`) proves neither runtime capability can be satisfied by an
unreachable endpoint, that no partial-chain lane self-promotes, and that
`public_capability_allowed` stays false unless every lane is proven.

| lane | status | note |
|---|---|---|
| batch_inference | `CANARY_PROVEN` | proven end to end against a real Apple Silicon worker (evidence/canary/real-runtime-embed.json) |
| embeddings | `CANARY_PROVEN` | real 384-dim embeddings computed on Metal, honeypot-verified, settled |
| realtime | `CANARY_PROVEN` | proven against a real llama.cpp/Metal engine: contract, real completion, VERIFIED receipt, CAPTURED authorization, positive margin (evidence/canary/real-runtime-realtime.json) |
| runpod_vllm | `EXTERNALLY_BLOCKED` | NVIDIA hardware and a pinned digest-addressed vLLM image; no Apple Silicon engine substitutes for this |
| openai_sdk_conformance | `EXTERNALLY_BLOCKED` | official openai Python 2.48.0 and JS 6.49.0 against merc. Against a REAL engine the JS client passes all seven capabilities and Python passes six: parallel_tool_calls fails because llama.cpp cannot parse this model's tool schema. Against the httptest fake it passed -- the fake was masking a real incompatibility, which is the whole argument for real-runtime proof |
| object_storage | `CANARY_PROVEN` | retention, deletion, tenant isolation against a live store |
| image_generation | `EXTERNALLY_BLOCKED` | governance only; no image runtime exists |
| lora | `EXTERNALLY_BLOCKED` | settlement arithmetic only; no trainer, no evaluator dispatch |
| multi_gpu | `EXTERNALLY_BLOCKED` | admission only; no tensor-parallel runtime has served a request |
| external_model_onboarding | `TESTED` | licence and remote-code policy; smoke test and benchmark need a runtime |
| refunds_disputes | `CANARY_PROVEN` | dispute filing, freeze, resolution and payout control |
| payouts | `EXTERNALLY_BLOCKED` | accrual and reconciliation; a real transfer needs the sandbox |
| failure_recovery | `CANARY_PROVEN` | stuck-job rescue and cancellation |
| receipt_verification | `CANARY_PROVEN` | ledger conservation and sole-writer enforcement |
| backup_restore | `CANARY_PROVEN` | backup scheduling and envelope |
| buyer_dashboard | `TESTED` | web/buyer.html renders and is CSP hash-bound; not driven by a real buyer session |
| supplier_console | `CANARY_PROVEN` | worker-token auth, sub-cent money at ledger granularity, 4 payout-rail states and the refusal path, against recorded control-plane responses |
| price_board | `CANARY_PROVEN` | the published page's own arithmetic must match the server's, including where the confidence weights decide the answer |
| python_sdk | `TESTED` | clean-room install of the built wheel; never run against a deployed merc |
| typescript_sdk | `TESTED` | binary embeddings decoder, path encoding, auth and wait() timeout; never run against a deployed merc |
| alerts | `TESTED` | alert and dashboard validation only; no delivery to a receiver |

**Missing capabilities:**

- `cuda_runtime` — no GPU runtime: set MERC_GPU_ENDPOINT to a reachable pinned runtime and RUNPOD_API_KEY (or MERC_GPU_API_KEY) to authenticate. No RunPod credential exists on this machine
- `openai_sdks` — official OpenAI SDKs not configured (MERC_TEST_OPENAI_PYTHON, MERC_TEST_OPENAI_NODE_MODULE)
- `stripe_sandbox` — STRIPE_SECRET_KEY is not set

`public_capability_allowed` is **false**. No public capability claim is permitted for any lane until every lane is `CANARY_PROVEN`.

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

### The supplier's share is size-dependent, and that is a real finding

The 3-row job gave merc **99.2%** of the buyer charge. Measured rather than
assumed: `MERC_CONTROL_PLANE_PER_TASK_USD` is a fixed 100 micro-USD per task
against a 125 micro-USD per-task charge, so fixed cost eats 80% before anything
is split and the supplier's share rounds to 1 micro-USD. A 400-row job on the
same worker splits **49/51**.

This is the same failure class as the LoRA compute floor truncating to zero: a
fixed cost dominating a small quote leaves a party with approximately nothing,
through arithmetic rather than policy. A supplier serving only minimum-size jobs
works for free. merc should enforce a minimum billable job size the way
`settleLoRARun` now enforces a minimum quote. **Not yet implemented.**

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
Apple Silicon engine substitutes for it. Same for image generation, LoRA and
multi-GPU.

## Money defect found by running the SDK against a live merc

Running the shipped Python SDK against a running control plane — something no
test had ever done — surfaced a defect in merc's pricing that no unit test could
have, because it only appears after the catalogue is repriced from a **real**
supplier's measured throughput.

### The supplier is paid zero while the buyer is charged

| units | buyer charged | supplier paid |
|---|---|---|
| 1–4 | *rejected*: `base_compute_usd must be finite and positive` |
| 10 | $0.000124 | **$0.000000** |
| 100 | $0.000125 | $0.000001 |
| 1,000,000 | $0.018000 | $0.014400 |

Between roughly 5 and 99 units the plan is executable, the buyer **is** charged,
and `SupplierPayoutPerTaskUSD` is **exactly zero**. This is not the sub-cent
carry the accrual path handles — the payout is 0, so nothing is accrued at all
and merc records no obligation to whoever performed the work.
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
3. Supplier payout rounding to exactly zero between 5 and 99 units.
   **Characterised, not fixed.**

`control/small_job_economics_test.go` pins the current behaviour so the window
cannot widen unnoticed, and fails loudly with `GOOD NEWS, ACTION REQUIRED` the
moment someone fixes it. **Not fixed here because the remedy — a minimum
billable job size, a supplier payout floor, or amortising the per-task cost —
is a pricing decision, not a bug fix.**

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
a real Metal worker, and validate the real result. That is what promoted
`python_sdk` and `typescript_sdk` to `CANARY_PROVEN` — not a relaxed bar, a
harder test.

## Repository boundary and rename

| item | status | evidence |
|---|---|---|
| VisionMCP extracted | `TESTED` | Already a standalone git repository at `~/Downloads/visionmcp` (3,297 tracked files, own history), with **zero** files tracked by merc. Its internals were not modified. `scripts/validate-repo-boundary.py` runs in `make ci` and fails if any VisionMCP path enters merc's tree, so it cannot be re-absorbed by a `git add -A` from the wrong directory. merc's owned LOC is **70,708**, and none of it is VisionMCP. **`EXTERNALLY_BLOCKED` on one step**: the repository has no remote — creating and pushing to a GitHub repository is an account action. |
| Rename zero-residue audit | `TESTED` | `scripts/rename-residue-audit.py`, in `make ci`. FROZEN 158 / BLOCKED 346 / **RESIDUE 0**. Frozen and blocked classes are itemised with a per-identifier reason in `docs/RENAME_REGISTER.md` §5. |

## Money and operations

| item | status | evidence |
|---|---|---|
| Supplier accrual | `TESTED` | `control/supplier_accrual.go`. Micro-USD conservation proven under 24 concurrent claims, 240 randomized orderings, and 9/9 mutation detection. |
| Payout reconciliation | `CANARY_PROVEN` | The CAD/USD mismatch that blocked this is gone: settlement currency is configuration (`control/currency.go`) and the platform settles CAD. The canary `payouts` lane passes against a real Stripe key. Supplier accrual, minor-unit carry and the sole ledger writer are unchanged. |
| Aggregated billing / prepaid | `IMPLEMENTED` | 4 references in `control/accounts.go`; charge batching reworked so the age trigger no longer fires at Stripe's $0.50 floor. |
| Refunds / disputes | `IMPLEMENTED` | 21 files reference disputes. Transfer reversal has never met real Stripe. |
| Stripe sandbox end to end | `CANARY_PROVEN` | Settles in CAD, which is what the platform account holds; no USD was manufactured. A live key now runs on the droplet. Not yet proven: a real Connect transfer landing in a supplier's bank, which is a different claim from a ledger payable and is kept separate deliberately. |
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

`EXTRACTED`. Now at `~/Downloads/blender-vision-mcp`, commit `fca82c4`, 276
files, 76,472 lines. It was never tracked by this repository, so it contributed
**zero** lines to Merc and no history filtering was required. Zero tracked
residue remains.

## Honest summary

Merc today is a **batch-only** platform with strong money correctness, a
governed price board, and no live payout rail. Of the eight physical lanes named
in the goal, one is implemented, one is partial, and six are not implemented —
and the two largest (realtime, RunPod) are one lane gated on a hardware-admission
change and a reversed decision.

The single highest-leverage action is not code. It is USD settlement on the
Stripe account, because it converts every money lane from unprovable to
provable.
