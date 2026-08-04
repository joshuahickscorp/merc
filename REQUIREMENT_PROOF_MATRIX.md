# Requirement proof matrix

> **Superseded on 2026-07-29. Do not read this table as current state.**
> `docs/SHIPPABILITY_STATUS.md` carries the maintained rung for every lane, and
> `ops/go-no-go.json` plus `RELEASE_READINESS.md` carry the release decision.
>
> The rows below were accurate on 2026-07-21 and the tree has moved past several
> of them. Five are now flatly contradicted by the code: the public price board
> (`pricing/board.json`, `control/pricing_governance.go`, `web/prices.html`),
> image generation (`control/image_generation.go`), LoRA eval payment
> (`control/lora_settlement.go`), the TypeScript SDK (`clients/sdk/typescript/`, builds
> to `dist/`), and single-host multi-GPU (`control/multi_gpu_admission.go`) are
> all marked "Not implemented" or validation-only here while the tree has
> `IMPLEMENTED`-or-better evidence for each.
>
> This file is retained as an audit trail of what was claimed on 2026-07-21, not
> as an operator checklist. A row here may not be used to promote or demote any
> lane.

Status as of 2026-07-21. `Tested` means automated software evidence. It does not
mean a physical CUDA/vLLM runtime ran.

| Requirement | Code | Tests | Real runtime | Canary | Production | Blocker |
|---|---|---|---|---|---|---|
| Chat completions | Implemented | PostgreSQL + fake-upstream integration | Not proven | No | No | CUDA host |
| SSE streaming | Implemented, one-event bound | Unit + integration | Not proven | No | No | CUDA host |
| OpenAI client migration | Base-URL compatible | Python 2.46.0 + JavaScript 6.48.0 | Not proven | No | No | Repeat against real vLLM |
| OpenAI model discovery | List envelope + realtime alias | Python + JavaScript `models.list()` | N/A | No | No | Canary proof |
| Parallel tool-call shape | Transparent pass-through | Python + JavaScript two-call integration | No | No | No | Real model/runtime support |
| Structured-output shape | JSON-schema pass-through | Python + JavaScript integration | No | No | No | Real model/runtime support |
| vLLM adapter | Pinned supervisor implemented | Command/profile tests | Not executed | No | No | Linux CUDA + Docker runtime |
| Usage reconciliation | Implemented | Exact final-usage tests | Not proven | No | No | Real tokenizer/runtime |
| Stream receipt | Implemented | Hash-chain receipt integration | Not proven | No | No | Real runtime |
| Buyer authorization | Explicit reserve/capture/release/void/refund facts | PostgreSQL binding + restore invariants | Internal only | No | No | Stripe test-mode account aggregation |
| Buyer charge | Immutable settlement + internal ledger | Exact capture and zero-sum integration | Internal only | No | No | Stripe test-mode aggregation |
| Supplier payable | Held liability, payout state, pre-transfer clawback | Integration + concurrent refund/payout race | Internal only | No | No | Stripe Connect test mode |
| Refund/no-charge | Failure voids full reservation; operator-confirmed platform fault creates full internal credit | Worker-death/recovery/disconnect + audited idempotent refund integration | Internal only | No | No | Stripe cash refund, reversal, partial-credit paths |
| Client disconnect | Upstream cancellation + no-charge receipt | PostgreSQL integration | Not proven | No | No | Real vLLM failure injection |
| Control crash recovery | Deadline/grace sweep + capacity restore | Concurrent PostgreSQL recovery | Not proven | No | No | Restart rehearsal with real runtime |
| Realtime observability | Metrics, alerts, dashboard, runbook | Static validator + endpoint integration | Local only | No | No | Real receiver/canary |
| Realtime backup/restore | New tables and relationships included | Isolated custom-format restore + invariants | Local only | No | No | Offsite restore boundary |
| Model cache | Adapter mounts governed local path | Command test | Not proven | No | No | CUDA host and model acquisition |
| Autoscaling | Not implemented | No | No | No | No | Later stage |
| Object upload | Existing batch S3 path only | Existing tests | Batch only | No | No | Realtime artifact plane |
| Public price board | Not implemented | No | No | No | No | Benchmark evidence |
| Direct-vLLM parity benchmark | Harness + pinned fixture implemented | Self-test + fake-upstream end-to-end, un-attested | No | No | No | Linux CUDA worker |
| Image generation | Not implemented | No | No | No | No | Later stage |
| LoRA eval payment | Not implemented | No | No | No | No | Later stage |
| Multi-GPU | Profile fields present | Validation only | No | No | No | Multi-GPU hardware |
| TypeScript SDK | Not implemented | No | No | No | No | Later stage |
| Organizations | Not implemented | No | No | No | No | Later stage |
