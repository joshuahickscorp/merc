# ComputExchange execution network bible

This file is the repository control record for the authoritative expansion
specification supplied on 2026-07-21. The received 3,416-line source has SHA-256
`60962a66291553cdc6a88d4e2cbf0979e80f4fd731ff8b42c48740e11d0368e1`.

The governing milestone is a production-grade OpenAI-compatible streaming
endpoint backed by pinned upstream vLLM at near-native performance, with
ComputExchange admission, routing, verification, transparent pricing,
settlement, automatic failure remedies, supplier payouts, and inspectable
receipts. Commodity runtime work belongs upstream; ComputExchange owns the
execution contract and execution-to-money authority.

Every cycle must distinguish implementation, automated tests, real-runtime
proof, private-canary proof, production proof, and external blockers. A route,
mock, unit test, dashboard, or unmeasured claim is never promoted to a higher
proof level. The current detailed state is maintained in
`docs/SHIPPABILITY_STATUS.md` (per-lane rung) and `RELEASE_READINESS.md` with
`ops/go-no-go.json` (release decision). `REQUIREMENT_PROOF_MATRIX.md` is a
superseded 2026-07-21 snapshot kept only as an audit trail. `VLLM_LANE_STATUS.md`
was named here but has never existed in this repository; the vLLM lane's current
rung lives in `docs/SHIPPABILITY_STATUS.md` alongside every other lane.

Until the complete source text is imported through the normal documentation
review path, the source with the digest above remains authoritative if this
control record is incomplete. This record may not be used to weaken or replace
any requirement in that source.
