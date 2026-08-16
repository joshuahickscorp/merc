# Support and Incident Runbook

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.

Status: **DRAFT / CONTACTS UNSET / TECHNICAL TABLETOP PASS / QUALIFIED HUMAN TABLETOP NOT EXECUTED**

This runbook covers the controlled backend alpha: named invitees, synthetic
workload input, Stripe test-mode only, CAD, connected-account country CA. It
does not authorize production or live-money response. Populate the contacts,
decision authorities and jurisdiction-specific notification rules before use.

Contacts are **role aliases**, not individuals. During founder-operated canary
every alias forwards to the same mailbox and reaches one person; that is a
staffing fact, not a documentation gap, and the roles stay separate so the
runbook does not have to be rewritten when they diverge.

No personal phone number appears in this repository. Paging escalates through
the alerting system, whose destination lives in the deployment secret
`/run/secrets/cx_alert_receiver_url` and never in version control.

| Role | Alias | Currently reaches |
|---|---|---|
| Incident commander | `ops@[DOMAIN-PENDING-REBRAND]` | founder |
| Security / privacy | `security@[DOMAIN-PENDING-REBRAND]` | founder + counsel when engaged |
| Payments owner | `payments@[DOMAIN-PENDING-REBRAND]` | founder |
| Supplier operations | `suppliers@[DOMAIN-PENDING-REBRAND]` | founder |
| Support intake | `support@[DOMAIN-PENDING-REBRAND]` | founder |
| Status channel | `https://status.[DOMAIN-PENDING-REBRAND]` | not yet published |

`[DOMAIN-PENDING-REBRAND]` is deliberate: the current name collides with an
active company operating a GPU-compute marketplace at `mercmerc.net`, and
publishing aliases under the current domain would have to be redone. See
`docs/REBRAND.md`. The validator keeps failing until a real domain replaces this
placeholder, which is the correct state — a fake-looking address that reaches
nobody is worse than a marked gap.

**Paging is not proven by a webhook alone.** A delivered message is not a woken
human. Before relying on this, run the drill in `docs/RUNBOOKS.md`: fire a
synthetic critical alert, confirm the notification actually reaches the phone,
acknowledge it, confirm resolution, and retain the delivery evidence.

## Severity

| Severity | Definition | Initial action target |
|---|---|---|
| SEV-0 | Credible ongoing physical harm, child exploitation, broad secret compromise, uncontrolled customer-data exposure, or unbounded real-money movement | Immediately stop affected/global paths and page the incident commander, security, privacy/payments and counsel |
| SEV-1 | Cross-account access, supplier exfiltration, compromised signing/payment credential, payout during active dispute, material integrity failure, or sustained critical outage | Contain immediately; establish incident command and evidence log |
| SEV-2 | Bounded single-account/device exposure, repeatable correctness defect, test-money reconciliation failure, or major degraded service | Suspend affected scope and assign owner |
| SEV-3 | Routine support issue, isolated retry, documentation defect or low-risk policy question | Track through support workflow |

Targets are operational design goals, not contractual SLAs. Final targets and
staffing need accountable approval and a delivered-page test.

## Common first actions

1. Open an incident ID; record reporter, UTC time, environment, release commit,
   first symptom and incident commander.
2. Protect people and stop expansion. Prefer reversible, scoped containment;
   use the global dispatch/payment stop when scope is uncertain.
3. Preserve minimal evidence with hashes, timestamps, origin, custodian and
   access log. Never paste tokens, presigned URLs, payment methods or full
   customer content into chat or tickets.
4. Establish a UTC timeline and decision log. Separate facts, hypotheses and
   unanswered questions.
5. Identify affected accounts, suppliers, workers, tasks, objects, providers,
   jurisdictions and model/runtime versions.
6. Engage privacy, security, payment and legal owners according to severity.
7. Communicate only confirmed facts using approved channels. Do not speculate,
   assign blame, promise reimbursement, or assert legal notification duties.

## Scenario: supplier host or egress exposure

- Stop dispatch globally if the affected workload or supplier scope is unknown.
- Revoke worker credentials and presigned access; isolate object credentials.
- Preserve task/worker/object/admin audit facts without duplicating content.
- Determine whether the device was operator-controlled, its verified location,
  network destinations, input categories and affected time window.
- Delete exposed synthetic artifacts after the evidence/hold decision.
- Do not resume until the worker, destination policy and cohort gate are
  reviewed and a negative claim test passes.

## Scenario: prohibited content or safety report

- Stop the task and prevent retries or verification copies.
- Restrict access to the smallest trained team; avoid unnecessary viewing or
  copying.
- Follow `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`.
- Escalate suspected child exploitation or imminent harm immediately to
  approved counsel and the designated safety lead for legally required steps.
- Preserve only what the approved reporting and legal-hold procedure requires.

## Scenario: credential or cross-account compromise

- Revoke affected sessions, API keys, worker credentials and provider secrets.
- Stop dispatch and billing if the compromised scope is unknown.
- Identify access since the last known-good rotation and inspect object/API
  authorization boundaries.
- Rotate secrets through the approved secret store, not chat or shell history.
- Require negative cross-account tests before reopening access.

## Scenario: payment, dispute or payout anomaly

- Keep Stripe in test mode for this canary.
- Stop the collector/payout path and preserve provider event IDs and internal
  operation IDs.
- Reconcile provider cash, charge batch, per-job allocation, ledger liability,
  payout funding, dispute and idempotency state.
- Do not manually mark a payment or payout successful without provider-owned
  evidence and two-person approval.
- Do not permit payout while an active buyer/provider dispute or abuse hold
  exists.

## Scenario: model/license issue

- Disable the affected runtime cell and stop new downloads.
- Record repository, requested revision, observed artifact hash, cached copies,
  deployments and affected tasks.
- Consult the license owner/counsel for notice, deletion, replacement or
  termination requirements.
- Resume only with a pinned, hash-verified, approved artifact and notices.

## Recovery and closure

Recovery requires an identified root cause, reviewed containment, reconciled
state, restored monitoring, verification of participant-visible behavior,
approved communication and explicit incident-commander signoff. Record
follow-ups with owner, severity and due date. A post-incident review must cover
timeline, impact, detection, controls that worked/failed, privacy and payment
decisions, evidence retention and recurrence tests.

## Tabletop requirement

`ops/support-incident-tabletop.json` is the machine-readable record. Its status
is `NOT_EXECUTED`; it must not be changed to `PASS` without named human
participants, UTC timestamps, scenario injects, decisions, observed evidence,
gaps, assigned actions and a second-person review.
