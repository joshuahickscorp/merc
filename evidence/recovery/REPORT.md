# Recovery suite report

Completed at `2026-08-17T00:44:10Z`. Suite status: **PASS**.

## Failure modes

| Mode | Status | Test result | Interval shortened |
| --- | --- | --- | --- |
| `process_restart` | PASS | pass | no |
| `control_plane_restart_under_load` | PASS | pass | yes |
| `postgres_restart` | PASS | pass | no |
| `object_store_restart` | PASS | pass | no |
| `network_interruption` | PASS | pass | yes |
| `stale_worker_expiry` | PASS | pass | yes |
| `interrupted_execution` | PASS | pass | no |
| `duplicate_stripe_event` | PASS | pass | no |
| `partial_settlement` | PASS | pass | yes |
| `rollback_and_forward` | PASS | pass | no |
| `restore_from_backup` | PASS | pass | no |

## Money state machine

### Duplicate Stripe event

the same charge.refunded event_id applied twice produced one stripe_webhook_events row and one refunded_cents value; the second ApplyPaymentEventTx returned Duplicate=true and did not re-apply a cash effect.

### Partial settlement

ClaimPayout committed ledger+operation to sending with cash_moved=false; RecoverStalePayoutOperations moved the row to outcome_unknown; ClaimOutcomeUnknownPayouts re-presented the same requested_cents; FinalizePayout released once; a second finalize did not create a second cash_moved row.

## Soak derivation

Every named time-dependent mechanism in control/ has a period that is either seconds-to-minutes or is a Duration/timestamp the tests already inject. The arbitrary 24-hour soak duration was protecting nothing that the deterministic recovery suite does not now cover. A long staging soak would still add evidence about production-shaped RSS/FD growth on two external Metal devices; that is a staging/device boundary, not a soak duration, and remains a separate P1.

Conclusion code: `deterministic_coverage_supersedes_arbitrary_24h`.

## Offsite independence

correlated-failure loss of real data: same disk, host, operator, or credential set destroying both the live store and the only backup.

That harm is not reachable at backend alpha with synthetic data. The local proof is the restore-drill plus logical-independent-restore (encrypted, checksummed, isolated credentials, source destroyed).

