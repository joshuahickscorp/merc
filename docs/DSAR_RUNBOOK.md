# Data Subject and Account Data Request Runbook

Status: **DRAFT / MANUAL PROCEDURE NOT YET REHEARSED**

This runbook covers access, correction, portability, deletion, restriction and
objection requests. It does not substitute for counsel and does not authorize
ad hoc production SQL. The product lacks a complete export/erasure workflow;
that is a blocker for processing production personal data.

## Preconditions

- Publish an approved privacy contact and authenticated request channel.
- Name a privacy decision-maker and backup.
- Approve identity-verification rules proportionate to the request; never ask
  for more identity data than the service originally needed without a
  documented reason.
- Establish the applicable jurisdiction, deadline, extension and appeal rules.
- Create an access-controlled case identifier. Do not place secrets, request
  bodies, payment method data, or unnecessary identity documents in the case.

## Intake and triage

1. Record receipt time, request type, asserted account, jurisdiction, channel
   and case owner.
2. Acknowledge receipt using approved language without promising an outcome.
3. Verify control of the account or approved alternative identity evidence.
   Record the verification result, not copies of evidence unless required.
4. Search for duplicate, abusive or conflicting requests and any active legal
   hold. Escalate uncertainty to privacy counsel.
5. Freeze automatic purges only for the narrow records necessary to complete
   the request; do not freeze unrelated data.

## Inventory search

Search by stable internal buyer/supplier/worker IDs after resolving the
verified account. Cover every row in `docs/PRIVACY_DATA_GOVERNANCE.md`, object
storage, backups/tombstones, logs, support systems and subprocessors. Record
query version, environment, operator, result counts and exceptions. Never copy
raw presigned URLs or authentication secrets into the case.

## Access or portability package

1. Export data in a structured, commonly readable format with a human-readable
   index of sources, purposes, timestamps and code meanings.
2. Exclude other persons' data, live credentials, anti-fraud secrets and
   privileged material only under an approved exception.
3. Scan the package for secrets and cross-account data using a second operator.
4. Encrypt the package and deliver it through an approved authenticated
   channel. Transmit any decryption secret separately.
5. Set and enforce a short download expiration, then delete the package and
   record only its hash, size and delivery result.

## Correction, restriction or objection

Apply corrections at the authoritative source and propagate them to derived
stores and subprocessors. Restriction must prevent processing, routing,
charging and payout actions as applicable without destroying evidence needed
to resolve the request. Counsel decides whether an objection applies and which
lawful basis controls.

## Deletion or account closure

1. Revoke sessions, API keys, worker credentials and webhook delivery.
2. Stop new job claims, charges and payouts for the account while preserving
   reconciliable financial state.
3. Delete workload input, output, partial, retry and verification artifacts.
4. Delete or irreversibly pseudonymize account, supplier, worker, job,
   verification, support and telemetry identifiers not under an approved
   exception.
5. Separate and minimize any retained accounting, tax, dispute, fraud,
   security or legal-hold record. Record category, fields, authority, approver,
   review date and scheduled deletion date.
6. Send deletion instructions to every applicable subprocessor and record its
   completion evidence.
7. Create a non-content deletion tombstone consumed by every restore process.
8. Have a second operator verify negative reads across databases, objects,
   caches, search indexes, logs and participant-visible APIs.

Because these operations are not yet implemented as one safe workflow,
operators must not run improvised `DELETE`, object-removal or pseudonymization
commands against production. Build, review and rehearse an idempotent tool
first.

## Completion record

Record the request type, received/completed dates, verified principal, systems
searched, record counts, exceptions and authority, subprocessor responses,
export package hash or deletion receipt, two-person review, response channel,
appeal information and next retention review. Do not retain the exported data
inside the case.

## Test protocol

Before broader canary use, seed a synthetic identity across every inventory
category and prove:

- a complete deterministic export with no other account's data;
- correction propagation;
- immediate access/processing restriction;
- idempotent deletion and negative reads;
- preservation of only approved minimal ledger facts;
- subprocessor deletion evidence; and
- restore of a pre-deletion backup followed by tombstone replay, with the data
  still unavailable.
