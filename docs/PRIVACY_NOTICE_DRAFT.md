# ComputExchange Privacy Notice

> **DRAFT — PENDING PRIVACY COUNSEL REVIEW — DO NOT PUBLISH**
>
> This document contains unresolved identity, lawful-basis, jurisdiction,
> transfer, retention, rights, and contact fields. It is not a completed privacy
> notice. The controlled canary remains limited to synthetic data and disposable
> test identities until the notice and underlying controls are approved.

- Version: `draft-0.1`
- Effective date: **UNSET**
- Controller/operator: **[LEGAL ENTITY, ADDRESS AND REGISTRATION REQUIRED]**
- Privacy contact/DPO: **[CONTACT AND DPO DETERMINATION REQUIRED]**

## What the service currently records

The service can record account email and password hash; sessions and API-key
hashes; supplier and payment-provider identifiers; worker device keys,
fingerprints, labels, hardware and runtime claims; alpha-access email, role,
note and source IP; job input and output object references and content; job,
task, verification, dispute and support history; webhook URLs and delivery
errors; prices, quotes, ledger, charge, refund, dispute and payout records; and
operational telemetry and logs.

The authoritative, field-level inventory and current implementation gaps are
documented in `docs/PRIVACY_DATA_GOVERNANCE.md`.

## Why information would be used

Subject to counsel approval and final product configuration, intended purposes
are account access, requested compute processing, supplier routing, output
verification, fraud and abuse prevention, billing and payout administration,
support, security, incident response, reliability measurement, legal
compliance, and establishment or defense of claims. The final notice must map
each purpose and data category to a lawful basis and must distinguish optional
from required processing.

Data must not be used to train models, build advertising profiles, or for an
unrelated purpose without a separate documented decision, notice and lawful
basis. The controlled canary does not permit personal data in workload input.

## Distributed workers and recipients

The architecture can transmit task input to a supplier agent. On an
independently controlled supplier machine, the supplier may be able to see or
retain that input despite application-level sandboxing. The controlled canary
therefore permits only operator-controlled devices and synthetic data.

Before broader use, the final notice must name or categorize suppliers,
infrastructure, object storage, payment, monitoring, support and other
subprocessors; state their locations; explain international transfers; link a
current subprocessor list; and explain whether each supplier acts as a
processor, subprocessor, independent controller, or other role. Contracts and
technical routing must match that description.

## Retention and deletion

Current records and object artifacts do not all have enforced deletion
schedules. The proposed schedule in `docs/PRIVACY_DATA_GOVERNANCE.md` is not
implemented or legally approved. Financial, tax, fraud, dispute and security
records may require longer retention, but the exact period and fields remain
unset pending counsel.

Backups require a tested deletion-tombstone process so that a restore does not
silently reactivate erased data. Until lifecycle and backup controls are
verified, production personal data is prohibited.

## Individual rights

Depending on location and role, an individual may have rights to access,
correct, delete, restrict or object to processing, receive a portable copy,
withdraw consent, or complain to a regulator. The final notice must identify
which rights apply, how to exercise them, the identity-verification process,
response periods, appeal rights and applicable regulator.

No rights-request inbox is currently approved. Operators must follow
`docs/DSAR_RUNBOOK.md`; they must not improvise deletion directly in production.

## Security and incidents

The service uses access controls and application safeguards described in
`docs/SECURITY.md`, but those safeguards do not eliminate supplier-host,
destination-egress, storage, backup, credential, or human-access risk. The
final notice must describe security accurately and avoid guarantees.

Incident response follows `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`. Notification
thresholds, recipients and deadlines require jurisdiction-specific counsel
review.

## Children, automated decisions and sensitive data

The controlled canary is not intended for children, consumer profiling,
eligibility decisions, high-impact decisions, biometric processing, or
sensitive data. The final service must implement approved age/eligibility and
high-risk-use restrictions before public use.

## Changes and contact

The final notice must state how changes are communicated, when reacceptance is
required, and how to contact the operator and applicable regulator. Placeholder
contacts or this draft must never be presented as a published notice.
