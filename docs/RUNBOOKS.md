# Operator runbooks

All examples assume an explicit environment file, an explicit database URL, and
a current backup. Never operate on a database whose target you have not resolved.

## Deploy

1. Verify `make ci` and `make prove-local` at the exact source revision.
2. Back up PostgreSQL and the artifact bucket.
3. Build the control image and native signed agent from that revision.
4. Apply `control/schema.sql` in one transaction. Applying it twice must succeed.
5. Roll out the control plane, check `/readyz`, then roll out agents gradually.
6. Inspect queue age, task failures, verification, ledger drift, and payout holds.

The control binary also applies its embedded schema under an advisory migration
lock. The checked-in SQL file and embedded bytes are the same authority.

## Backup and restore

Create a database dump with `make backup` or an equivalent `pg_dump` invocation.
Record the source revision, schema hash, bucket version, and encryption-key version
beside the backup. Do not store secrets in the repository.

Restore into a new database first:

```bash
createdb cx_restore
pg_restore --exit-on-error --clean --if-exists \
  --dbname=postgres://cx@localhost/cx_restore backup.dump
psql postgres://cx@localhost/cx_restore -v ON_ERROR_STOP=1 \
  -f control/schema.sql
```

Start one control instance against the restored target, check readiness and
ledger reconciliation, then switch traffic. Artifact objects must be restored or
version-rewound to the matching backup boundary.

## Roll back

Schema changes are additive and idempotent. To roll back application behavior:

1. stop new submissions or put the service in maintenance mode;
2. allow active commits to drain or expire leases;
3. deploy the previous control and agent artifacts built from the recorded source;
4. leave additive schema objects in place unless a separately reviewed data
   migration proves removal safe;
5. re-run readiness, quote, submit, cancellation, verification, and ledger checks.

Do not reverse settled ledger rows. Use compensating entries with stable
idempotency keys. Do not delete artifacts referenced by completed jobs.

## Queue recovery

- A dead agent is recovered by lease expiry and transactional requeue.
- Before manual requeue, inspect task state, lease owner, attempt count, result
  object, commit record, and ledger effects.
- Use the authenticated requeue endpoint once. Repeated action is audited and must
  remain idempotent.
- Suspend a suspect worker before releasing its work. Reinstatement is a separate
  audited operator action.

## Money incidents

Freeze payout release before investigation. Compare job charge, supplier earning,
platform fee, subsidy, refund, and dispute effects by their source IDs. Search for
duplicate `(task_id, kind)` effects and verify the global ledger sum. Repair with a
new compensating entry; never mutate history. Resume payouts only after the drift
query is clean and the incident source is bounded.

## Secret rotation

- Buyer/admin keys and worker credentials: mint a replacement, verify it, revoke
  the old credential, then inspect the credential audit.
- Worker enrollment: revoke unused codes and active credentials independently.
- Webhook secrets: exact re-registration creates a new sealed secret.
- Token-encryption key: deploy dual-read/single-write rotation before removing the
  prior key.
- Verification-sampling secret: rotate only at a documented task boundary because
  it changes deterministic sampling decisions.

## Storage or database outage

On database loss, stop admission because lifecycle and money authority are
unavailable. On storage loss, keep tasks from being claimed and do not fabricate
completion from database state alone. After recovery, reconcile missing input and
result objects against task rows before resuming dispatch.

## Evidence

`scripts/prove-local.sh` writes source-bound JSONL receipts to
`.artifacts/prove-local/ledger.jsonl`. Preserve the ledger, source revision,
binary hashes, census, and service logs together. `KEEP=1` retains disposable
services for inspection; shut them down explicitly after investigation.
