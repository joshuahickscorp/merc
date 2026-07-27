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
| Image generation | `NOT_IMPLEMENTED` | No reference in `control/`. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| LoRA training | `NOT_IMPLEMENTED` | Only match is an unrelated test string. |
| External-model onboarding | `TESTED` | `control/model_onboarding.go` runs at process start and panics on a model merc cannot resell. Licence is an allowlist (an unrecognised one is refused, not assumed permissive); `remote_code=true` is refused unconditionally because it runs repo-supplied code on third-party supplier hardware; a required attribution must appear in the shipped `NOTICE`, checked against the real file by `TestCatalogueAttributionAppearsInNotice` (mutation-checked: removing "Built with Llama" from NOTICE fails CI). Both catalogue models now declare licence, URL, commercial-use and remote-code posture. No onboarding *smoke path* yet: adding a model is still a code change, not a self-serve flow. |
| Single-host multi-GPU | `NOT_IMPLEMENTED` | No tensor-parallel or multi-GPU reference. |
| Buyer dashboard | `IMPLEMENTED` | `web/buyer.html`. |
| Supplier console | `TESTED` | `web/supplier.html`, route `GET /supplier`. Loaded against a live local control plane with a real worker token; shows paid / lifetime / carried at ledger granularity, four distinct payout-rail states and verification standing. Gated by `scripts/test-supplier-console.mjs` (mutation-checked 3/3). Not `REAL_RUNTIME_PROVEN`: the figures it rendered came from a hand-seeded accrual, not from a job a worker actually served. |
| Public price board | `IMPLEMENTED` | `web/prices.html` behind `GET /prices`, recomputing the confidence-weighted median client-side and naming each observation's provider and source. Not `TESTED`: it has no gate of its own. |
| Python SDK | `TESTED` | `sdk/python/merc/`, clean-room install verified. |
| TypeScript SDK | `TESTED` | `sdk/typescript/`, built to `dist/`, 12 tests covering the binary embeddings decoder, path encoding, bearer auth and `wait()` timeout. Never run against a deployed merc. |

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
