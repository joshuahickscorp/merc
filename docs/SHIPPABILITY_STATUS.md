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

## Lanes

| lane | status | evidence |
|---|---|---|
| OpenAI-compatible realtime | `TESTED` | `[KILL-RT]` reversed and executed (`DECISION_ZERO_REVERSAL.md`). `control/realtime.go`, `chat/completions` routed, 5 routes in the authorization matrix, integration test green. **Official-SDK conformance restored and passing**: the real `openai` Python 2.48.0 and JavaScript 6.49.0 clients drive the surface end to end -- completion, streaming, merc's receipt headers, credential rejection, model listing, tool-call and structured-output request shapes (`make realtime-sdk-conformance`, evidence in `evidence/realtime/openai-sdk-conformance.json`). Wire headers renamed `X-CX-*` to `X-Merc-*`. Still not `REAL_RUNTIME_PROVEN`: the upstream in every run so far is an httptest fake, so nothing here is evidence about latency or any GPU, and `scripts/realtime-parity-benchmark.py` reports `UNATTESTED_HARNESS_RUN` and refuses `public_claim_allowed` for exactly that reason. |
| RunPod-backed pinned vLLM | `IMPLEMENTED` | `control/types.go` now admits `nvidia_24gb/48gb/80gb/180gb` and the `vllm` engine, paired by `EngineAdmissibleFor` so an engine cannot claim hardware that cannot serve it. An NVIDIA worker registers. No RunPod pod has yet served a real job through routing, verification and settlement. |
| Object storage | `TESTED` | Presign/multipart in `control/storage.go`. **Retention**: `control/job_object_retention.go` removes a job's payload objects 30 days after it goes terminal, holds them while any dispute is unresolved, and refuses a period inside the 7-day filing window; proven against live minio + Postgres, mutation-checked 4/4. **Worker credentials**: the agent holds no S3 key at all (`grep -c S3_ACCESS_KEY agent/src` = 0) — workers receive only per-key presigned URLs. **Tenant isolation**: every object key is server-constructed from the job id; no endpoint accepts a caller-supplied key, so there is no ref a caller can point at another tenant. Isolation now has its own gate (`control/tenant_isolation_test.go`): a non-owner gets no job, no 200 and no presigned URL from any buyer-facing job route, and `decodeStrictJSONObject` refuses any body carrying `input_ref`/`output_ref`/`result_key`/`object_key`. Mutation-checked: dropping the buyer scope from `GetJob`, or adding an `input_ref` field to `jobSubmit`, each fails it. |
| Image generation | `IMPLEMENTED` (governance `TESTED`) | `control/image_generation.go` + `POST /v1/images/generations`, 81st route, buyer-owned in the authorization matrix. **Governance is the finished part**: size allowlist, n cap, prompt bound, and refusal of `b64_json` (an inline image never enters object storage, so it would have no retention, erasure or dispute-evidence path). Content policy refuses CSAM, non-consensual intimate imagery, photorealistic real-person likeness and forged documents, checking two normalisations because separator evasion defeats either one alone — my own adversarial test caught that. Refusals name the rule and never echo the prompt. Licence gate is separate from the text one because open image licences (OpenRAIL-M/++) attach use restrictions the licensee must pass downstream, and merc resells generation. Mutation-checked 5/5. **`NOT_IMPLEMENTED` for the lane itself**: there is no image runtime, so an acceptable request returns 503 rather than an invented result. No contract, no supplier, no settlement. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| LoRA training | `IMPLEMENTED` (settlement `TESTED`) | `control/lora_settlement.go`. **Outcome-aware settlement is the finished part.** The price splits: a compute floor the supplier is owed however the run turns out, and an outcome bonus contingent on an independent evaluation. Each of the three alternatives is wrong on its own — supplier-bears-risk means a supplier can lose a day's revenue to someone else's dataset (and merc's pricing governance already refuses prices that don't cover electricity); buyer-bears-risk makes 'outcome-aware' just billing with a nicer name; merc-bears-risk is unbounded. merc's cut comes from the floor, so it never earns more from a failed run than a successful one — asserted. Evaluation must be independent at the **account** level, not just the worker: two worker ids on one account are not two opinions. A held-out set that leaked into training is refused, because the improvement would measure memorisation. Conservation proven over 20,000 randomised settlements; mutation-checked 7/7. **`NOT_IMPLEMENTED` for the lane**: no trainer, no evaluator dispatch, no adapter deployment, no GPU. |
| External-model onboarding | `TESTED` | `control/model_onboarding.go` runs at process start and panics on a model merc cannot resell. Licence is an allowlist (an unrecognised one is refused, not assumed permissive); `remote_code=true` is refused unconditionally because it runs repo-supplied code on third-party supplier hardware; a required attribution must appear in the shipped `NOTICE`, checked against the real file by `TestCatalogueAttributionAppearsInNotice` (mutation-checked: removing "Built with Llama" from NOTICE fails CI). Both catalogue models now declare licence, URL, commercial-use and remote-code posture. No onboarding *smoke path* yet: adding a model is still a code change, not a self-serve flow. |
| Single-host multi-GPU | `IMPLEMENTED` (admission `TESTED`) | `control/multi_gpu_admission.go`. **Admission is the finished part**, and it is a control-plane problem because the decision is made before anything loads: getting it wrong is not a rejection but an OOM kill partway through a run the buyer is already being charged for. Three refusals, each distinguished so a supplier knows whether to add GPUs, upgrade the interconnect, or neither: per-rank overhead (KV cache, activations, CUDA context) does **not** shrink with the degree, so weights-only division admits hosts that die on the first long request; PCIe caps tensor parallelism at 2 because the per-layer all-reduce beyond that costs more than the compute it buys; and a degree must divide the attention heads or the runtime fails at load. An undeclared interconnect is refused rather than guessed. Picks the **smallest** admissible degree — splitting further occupies GPUs another buyer could use. 50,000 randomised placements, none admitted that would not fit; mutation-checked 6/6. **`NOT_IMPLEMENTED` for the lane**: no worker declares a topology, no runtime serves tensor-parallel, no GPU. |
| Buyer dashboard | `IMPLEMENTED` | `web/buyer.html`. |
| Supplier console | `TESTED` | `web/supplier.html`, route `GET /supplier`. Loaded against a live local control plane with a real worker token; shows paid / lifetime / carried at ledger granularity, four distinct payout-rail states and verification standing. Gated by `scripts/test-supplier-console.mjs` (mutation-checked 3/3). Not `REAL_RUNTIME_PROVEN`: the figures it rendered came from a hand-seeded accrual, not from a job a worker actually served. |
| Public price board | `TESTED` | `web/prices.html` behind `GET /prices`. The page promises visitors it recomputes the confidence-weighted median itself; `control/price_board_parity_test.go` runs the page's own script and requires it to agree with the server's `confidenceWeightedMedianUSDPer1K`, so the board cannot publish a price merc does not charge. Every observation must name a provider and a source URL, because the weighting is derived from the source host. Mutation-checked 3/3 — but note the shipped board has too few observations for the weights to move the median, so the parity cases are constructed to sit at the crossing point where they do. The first draft passed against a page that weighted nothing like the server. |
| Python SDK | `TESTED` | `sdk/python/merc/`, clean-room install verified. |
| TypeScript SDK | `TESTED` | `sdk/typescript/`, built to `dist/`, 12 tests covering the binary embeddings decoder, path encoding, bearer auth and `wait()` timeout. Never run against a deployed merc. |

## Private canary — 5/15 lanes CANARY_PROVEN

`make private-canary` (A lane is CANARY_PROVEN only when its command walks the full chain). Report: `evidence/canary/private-canary.json`.

A lane is `CANARY_PROVEN` only when its command walks the whole chain — buyer
request, contract, scheduler, real runtime, verification, buyer debit, supplier
payable, receipt. A lane that passed a partial test is `TESTED`. A lane whose
capability is missing is `EXTERNALLY_BLOCKED` and names what is missing. There
is no override, and `scripts/test-canary-gaming.sh` (in `make ci`) proves it:
an unreachable GPU endpoint does not satisfy the runtime capability, no
partial-chain lane can promote itself, and `public_capability_allowed` stays
false unless every lane is proven.

| lane | status | note |
|---|---|---|
| batch_inference | `TESTED` | money path proven against the local scheduler; no real GPU worker served it |
| embeddings | `TESTED` | no real runtime produced the embeddings |
| realtime | `EXTERNALLY_BLOCKED` | with a real runtime this walks contract, stream, verification, settlement and receipt |
| openai_sdk_conformance | `EXTERNALLY_BLOCKED` | wire-surface conformance; says nothing about any GPU |
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
| alerts | `TESTED` | alert and dashboard validation only; no delivery to a receiver |

**Missing capabilities blocking the rest:**

- `gpu_runtime` — no GPU runtime: set MERC_GPU_ENDPOINT to a reachable pinned runtime and RUNPOD_API_KEY (or MERC_GPU_API_KEY) to authenticate. No RunPod credential exists on this machine
- `openai_sdks` — official OpenAI SDKs not configured (MERC_TEST_OPENAI_PYTHON, MERC_TEST_OPENAI_NODE_MODULE)
- `stripe_sandbox` — STRIPE_SECRET_KEY is not set

`public_capability_allowed` is **false**. merc may not make a public capability claim for any lane until every lane is `CANARY_PROVEN`.

## Repository boundary and rename

| item | status | evidence |
|---|---|---|
| VisionMCP extracted | `TESTED` | Already a standalone git repository at `~/Downloads/visionmcp` (3,297 tracked files, own history), with **zero** files tracked by merc. Its internals were not modified. `scripts/validate-repo-boundary.py` runs in `make ci` and fails if any VisionMCP path enters merc's tree, so it cannot be re-absorbed by a `git add -A` from the wrong directory. merc's owned LOC is **70,708**, and none of it is VisionMCP. **`EXTERNALLY_BLOCKED` on one step**: the repository has no remote — creating and pushing to a GitHub repository is an account action. |
| Rename zero-residue audit | `TESTED` | `scripts/rename-residue-audit.py`, in `make ci`. FROZEN 158 / BLOCKED 346 / **RESIDUE 0**. Frozen and blocked classes are itemised with a per-identifier reason in `docs/RENAME_REGISTER.md` §5. |

## Money and operations

| item | status | evidence |
|---|---|---|
| Supplier accrual | `TESTED` | `control/supplier_accrual.go`. Micro-USD conservation proven under 24 concurrent claims, 240 randomized orderings, and 9/9 mutation detection. |
| Payout reconciliation | `IMPLEMENTED` | Blocked from proof by the CAD/USD mismatch. |
| Aggregated billing / prepaid | `IMPLEMENTED` | 4 references in `control/accounts.go`; charge batching reworked so the age trigger no longer fires at Stripe's $0.50 floor. |
| Refunds / disputes | `IMPLEMENTED` | 21 files reference disputes. Transfer reversal has never met real Stripe. |
| Stripe sandbox end to end | `EXTERNALLY_BLOCKED` | Connect enabled, two distinct webhook secrets wired, `stripe-check` passes on everything except settlement currency. |
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
