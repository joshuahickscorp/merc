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
| OpenAI-compatible realtime | `NOT_IMPLEMENTED` | Deliberately deleted by `[KILL-RT]`. No `chat/completions` route, no SSE anywhere in `control/`. Reversal proposed in `DECISION_ZERO_REVERSAL.md`, **not** executed. |
| RunPod-backed pinned vLLM | `NOT_IMPLEMENTED` | `control/types.go` admits no CUDA class; an NVIDIA worker is refused at registration. Same lane as realtime. |
| Object storage | `IMPLEMENTED` | 4 presign/multipart references in `control/storage.go`. Retention, tenant isolation and worker credentials not separately proven. |
| Image generation | `NOT_IMPLEMENTED` | No reference in `control/`. |
| Video | `NOT_IMPLEMENTED` | Goal correctly gates this behind image. |
| LoRA training | `NOT_IMPLEMENTED` | Only match is an unrelated test string. |
| External-model onboarding | `PARTIAL` | `control/runtime-authority.json` pins revisions and sha256 for the catalogue; no licence/remote-code policy or onboarding smoke path. |
| Single-host multi-GPU | `NOT_IMPLEMENTED` | No tensor-parallel or multi-GPU reference. |
| Buyer dashboard | `IMPLEMENTED` | `web/buyer.html`. |
| Supplier console | `TESTED` | `web/supplier.html`, route `GET /supplier`. Loaded against a live local control plane with a real worker token; shows paid / lifetime / carried at ledger granularity, four distinct payout-rail states and verification standing. Gated by `scripts/test-supplier-console.mjs` (mutation-checked 3/3). Not `REAL_RUNTIME_PROVEN`: the figures it rendered came from a hand-seeded accrual, not from a job a worker actually served. |
| Public price board | `NOT_IMPLEMENTED` | `pricing/board.json` exists and is governed, but nothing publishes it. |
| Python SDK | `TESTED` | `sdk/python/merc/`, clean-room install verified. |
| TypeScript SDK | `NOT_IMPLEMENTED` | No directory. |

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
