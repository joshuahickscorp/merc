# Merc runbooks

Start with [release readiness](RELEASE_READINESS.md). The current candidate
is not approved for live money or public production. Preserve receipts and
authorities during every incident; do not repair a gate by editing evidence or
loosening a validator.

## Control plane or database outage

Pause intake, preserve the exact error and current authority digests, and keep
settlement and payout paths fail-closed. Follow the detailed
[support and incident runbook](SUPPORT_AND_INCIDENT_RUNBOOK.md) and do not
delete or rewrite database, object-store, or ledger state during diagnosis.

## Deploy

Deploy only the exact reviewed image/source digest through the release
procedure in [release signing and staging](RELEASE_SIGNING_AND_STAGING.md).
The deployment wrapper must not invent an approval, bypass the readiness
ledger, or carry live credentials in arguments.

## Agent offline or task stall

Stop new assignment to the affected worker, preserve the worker and task
receipts, and let the control-plane recovery path distinguish expiry,
requeue, verification failure, and provider uncertainty. Do not transfer a
lease or payout authority by hand.

## Queue stall and safe requeue

Use the bounded requeue and attempt-epoch checks. A retry must retain its
original job identity and must not create a second settlement path. Escalate
when the durable state and receipt disagree.

## Storage or database outage

Fail closed before writes whose accounting or verification context cannot be
read. Follow [offsite backup and restore](OFFSITE_BACKUP_RESTORE.md) for
recovery; keep the source, encryption, and independent-restore evidence
separate.

## Backup and restore

Use the documented backup/restore procedure and verify hashes, encryption
envelopes, object sentinels, and ledger zero-sum before reopening any lane.
Never print credentials or restore over the only surviving copy.

## Webhook delivery failure

Treat provider responses as untrusted until signature, endpoint identity,
replay, and durable idempotency checks pass. Keep payment, Connect, and
internal webhook secrets distinct and hold settlement when the provider state
is unknown.

## Verification failure or bad result dispute

Freeze the affected outcome, retain the attempt and verification artifacts, and
recompute through the independent verifier. A dispute cannot be resolved by
changing the answer, workload, threshold, or historical receipt in place.

## Money incident or payout hold

Hold payout and preserve the provider event, ledger entries, reconciliation
state, and exact pricing authority. Follow [live-money transition](LIVE_MONEY_TRANSITION.md);
the current readiness state remains prohibited for live value movement.

## Insufficient capacity

Return a bounded capacity failure or wait for newly admitted capacity. Never
over-issue relative to a worker's declared safe envelope, and never represent a
quote as a reserved slot without the corresponding durable authority.

## Alert mapping

Alert annotations point back to the sections above. Synthetic alert delivery
proves routing behavior only; it is not external staffed paging evidence.

## Related controls

- [security](SECURITY.md)
- [backend alpha contract](BACKEND_ALPHA_CONTRACT.md)
- [DSAR runbook](DSAR_RUNBOOK.md)
- [support and incident runbook](SUPPORT_AND_INCIDENT_RUNBOOK.md)
- [offsite backup and restore](OFFSITE_BACKUP_RESTORE.md)

This page is the canonical operator runbook; historical procedures are not
release instructions.
