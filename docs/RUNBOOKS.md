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

## Control plane or database outage

Stop admission at the proxy if `/readyz` fails. Record `/version`, control logs,
PostgreSQL health and saturation, disk availability, and the first failed ticker;
do not restart repeatedly before preserving evidence. If PostgreSQL is unhealthy,
keep control stopped until the database is consistent. Restore into a new target
with `scripts/restore.sh <timestamp> --to cx_restore_YYYYMMDD`, compare table and
ledger counts, then move traffic. Never accept jobs while lifecycle authority is
ambiguous.

## Agent offline or task stall

Check `cx_active_workers`, typed task failures, thermal/memory throttle state, and
the device's `status.json`. An active task renews its lease with both task id and
attempt; a stale attempt cannot renew or commit. Let automatic dead-claim and
stale-task recovery act first. If manual action is required, suspend the worker,
inspect the task attempt and artifact state, then use the authenticated requeue
endpoint once. Reinstatement is a separate audited action.

## Queue stall and safe requeue

Break queue depth down by tier and workload, inspect `/admin/scheduler/explain`,
and verify an exact runtime/model/hardware match exists. Do not requeue a running
task merely because it is slow: confirm its worker heartbeat/lease is absent or
the execution deadline has passed. The requeue action increments the attempt
epoch, so delayed start/fail/commit calls from the previous execution are inert.

## Verification failure or bad-result dispute

Freeze payout release for the affected task and preserve primary, redundancy,
honeypot, tiebreak, result-hash, and object metadata. Confirm output cardinality,
shape, checksum, and runtime provenance before blaming the supplier. Use the
buyer dispute endpoint once; allow the verification worker to resolve it. Ban or
reputation changes require an attributed admin action. Repair buyer money only
with an append-only compensating effect.

## Money incident or payout hold

Disable payout release, preserve Stripe event ids and idempotency keys, and query
the admin drift/payout views. Distinguish provider `outcome_unknown` from a failed
operation: resolve it by exact provider id before retrying. Compare buyer charge,
supplier liability, platform take, processor fee, subsidy, refund, and cash-moved
records. Never edit or delete settled ledger rows. Apply a reviewed compensating
entry with a stable source id, re-run reconciliation, then unfreeze only the
bounded affected scope.

## Webhook failure

Confirm the registered URL is still public HTTPS, DNS has not moved to a private
address, and the signing secret matches. Inspect attempts, lease expiry, response
class, redirect rejection, and dead-letter time. Re-registering the same job/URL
is idempotent and returns the same encrypted secret. Do not bypass SSRF controls;
have the buyer provide a compliant endpoint and replay from the durable outbox.

## Object-store failure or artifact corruption

Pause claims while inputs or result PUTs are unavailable. Compare database keys,
object size, content SHA-256, expected record count, and ownership prefix. Restore
objects from the backup at the same boundary as PostgreSQL. Never mark a task
complete based on a database row when the authoritative result object is absent
or fails verification.

## Insufficient capacity

Check exact runtime cell, model kind/reference, memory headroom, hardware class,
minimum reputation, geographic constraint, reservation price, and worker age.
Return a capacity error or leave work queued; do not silently route to an
unsupported runtime or weaken redundancy. Invite a compatible supplier or ask
the buyer to select a supported model/tier.

## Emergency secret rotation

Treat any committed or logged production secret as compromised. Revoke at the
provider first, rotate endpoint-specific webhook secrets independently, mint new
buyer/admin/worker credentials, and verify the new credential before revoking the
old one. Encryption-key rotation requires a dual-read/single-write rollout.
Verification-sampling key rotation requires a documented task boundary. Re-run
the history secret scan after remediation without printing recovered values.

## Backup and restore drill

`make restore-drill` creates isolated PostgreSQL and MinIO instances, inserts a
representative buyer/job/task plus an artifact, takes a checksummed custom-format
dump and object mirror, restores both, and compares content hashes. This proves
the mechanism locally; production readiness additionally requires a successful
`scripts/backup.sh` upload to independent offsite storage followed by
`scripts/restore.sh` from that uploaded copy.

## Rollback rehearsal

Keep at least the current and prior content-addressed control images on the
staging/production host. Run `scripts/rollback.sh <full-commit>` in staging, verify
public `/version`, `/readyz`, quote, idempotent submit, cancel, both workloads, and
ledger reconciliation, then redeploy the candidate. Schema rollback is forbidden;
all schema changes are additive and old binaries must be compatibility-tested.

## Alert mapping

`monitoring/alerts.yml` maps each alert to these sections. Before release, load
the rules in the actual monitoring system, route `severity=page` to the on-call
receiver, fire one synthetic alert, acknowledge it, and record the delivery and
resolution timestamps. A checked-in rule that has never paged is not a proven
alert.
