# Backend-alpha risk disclosure

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **DRAFT — PENDING COUNSEL REVIEW — NOT A WARRANTY DISCLAIMER APPROVED FOR USE**
>
> Honest risks of the controlled backend alpha. This is pre-production
> software. Parent terms: `docs/CANARY_TERMS.md`.

- Document version: `draft-0.2-backend-alpha`

## This is pre-production software

The control plane, the agent, the catalogue, the payment path, and the
deletion path are under active change. Builds can be wrong. Tests can
pass and the running system can still fail. A green `/healthz` is not a
promise the next job will complete.

There is no production-scale public signup, no public website in this
scope, and no claim that the system is ready for strangers or for live
money.

## Work can be wrong, late, repeated, or lost

Model output can be inaccurate, incomplete, unsafe, or identical to
output someone else received. Verification can reject good work and
accept bad work. Jobs can be cancelled, retried, or left incomplete when
a worker disappears.

Do not rely on a result for a decision that matters. Do not ship an
artifact from this alpha to a customer.

## There is no SLA

No uptime, latency, throughput, or quality figure in a catalogue,
dashboard, or commit message is a contractual commitment. Service
credits do not exist. If the operator stops the evaluation, in-flight
work dies with it.

## Input can be seen on a worker

Task input is sent to a supplier agent over presigned object URLs. In
this alpha the worker is operator-controlled, which reduces — it does
not eliminate — the chance that input is retained on disk, in logs, in
swap, or in a crash dump.

The macOS sandbox does not pin outbound HTTPS destinations. A
compromised or misconfigured host can send data somewhere the operator
did not intend. Independently owned supplier machines are out of scope
precisely because this risk is not contractually or technically closed.

## Personal data is collected even though workloads must be synthetic

Workload **content** must be synthetic. Account **identity** is not.
Named invitees use real emails. The database stores those emails,
password hashes, device public keys and fingerprints, webhook URLs, and
Stripe test-mode customer and payment-method identifiers. Source IP is
stored on `alpha_requests` when that public path is enabled (it is
refused while the private canary is on).

If you are in Canada, that identity data is personal information. A
privacy regime can attach to it. See `docs/PRIVACY_NOTICE_DRAFT.md`.
Writing that notice does not make the system compliant.

## Money you see is not money

Stripe test-mode objects, ledger rows, and CAD quotes do not move
cardholder funds or pay a supplier. Treating a test PaymentIntent as a
real sale, or a test transfer as income, is a mistake. Live keys are a
separate, still-prohibited activation.

See `docs/ALPHA_PAYMENT_PAYOUT_DISCLOSURE.md`.

## Security residuals that are still true

- Presigned URLs are time-limited but are still capabilities.
- Admin keys and break-glass API keys exist. They are powerful.
- Backups, if taken, can resurrect deleted rows until a tombstone is
  replayed. That replay is rehearsed on synthetic data, not proven on
  this alpha's live backups.
- Logs and metrics may contain identifiers, IPs, and error strings.
- The public site, if someone later turns it on, is outside this alpha's
  described surface.

`docs/SECURITY.md` is a boundary register, not a certification.

## Models and licenses

Llama 3.2 is used on the batch-inference path and is subject to the
Llama 3.2 Community License and Acceptable Use Policy. MiniLM is used on
the embedding path. Neither model row is cleared in
`docs/THIRD_PARTY_LICENSES.md`. Using a blocked model in a closed test
does not clear it for a public service.

## Operator discretion

The operator can revoke you, wipe artifacts, pause intake, pause
dispatch, pause payments, or end the evaluation, without a hearing and
without a refund of simulated balances. There is no approved appeal
process.

## What this disclosure is not

It is not an approved limitation of liability. Counsel has not supplied
the warranty, indemnity, or damages language. It is not insurance. It is
not a reason to put real customer data or real money on the system.
