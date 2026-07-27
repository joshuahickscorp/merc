# Execution-network release gates

The realtime lane is **NO-GO** for private canary and production.

Passed software gates: schema apply, auth inventory, pinned-profile validation,
SSE ordering and bounds, V0 commitments, usage reconciliation, idempotent
settlement, receipt ownership, no-charge worker-death and client-disconnect
behavior, exact reserve/capture/release/void accounting, audited full internal
credit and supplier clawback before transfer, concurrent stale-contract
recovery, official Python/JavaScript SDK protocol conformance, and static
realtime observability validation.

Open launch gates: physical CUDA/vLLM execution, direct parity benchmark,
real-vLLM rerun of client conformance, tools and structured output, real-engine
cancellation and disconnect races, restart/fallback/load/soak, real metric and
alert delivery, cache acquisition, Stripe test mode, Connect payable
reconciliation, cash refund/transfer reversal/partial-credit proof, an offsite
restore of the locally proven new tables,
privacy/legal review, and private-canary approval.
