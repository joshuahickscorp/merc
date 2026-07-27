# Realtime worker failure runbook

When a vLLM worker exits or a stream ends without final usage:

1. The request context cancels the upstream operation.
2. The contract moves from `EXECUTING` to `FAILED` or `CANCELLED`.
3. A failed `realtime_executions` row records the bounded failure code.
4. Reserved sequence capacity is released.
5. No buyer charge, supplier credit, or platform take is written.
6. The buyer retrieves the receipt and may retry with a new request or the
   original idempotency key for diagnosis; the same key cannot double-settle.

Quarantine repeated engine failures at the offer/worker layer. Do not manually
insert success ledger entries or pay a supplier from worker self-report.

If control exits after admission but before finalization, the contract remains
`EXECUTING` until its deadline plus the recovery grace. The leader-elected
`realtime-contract-recovery` sweep then locks it, records
`control_recovery_timeout`, restores capacity, and leaves money untouched.
Concurrent sweep replicas use `SKIP LOCKED`; only one may write the terminal
outcome. Alert on an executing age over 180 seconds or any finalization error.
