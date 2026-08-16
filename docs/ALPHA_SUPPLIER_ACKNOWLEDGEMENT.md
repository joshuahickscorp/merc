# Backend-alpha supplier acknowledgement

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **DRAFT — PENDING COUNSEL REVIEW — NOT AN ENFORCEABLE CONTRACT**
>
> This is what a supplier is being asked to understand for the controlled
> backend alpha. It does not create employment, a contractor relationship,
> or a right to be paid. Parent terms: `docs/CANARY_TERMS.md`.

- Document version: `draft-0.2-backend-alpha`
- Scope: operator-controlled devices only · Stripe test-mode only · CAD · CA
- Independently owned supplier devices: **not permitted**

## Who a supplier is, in this alpha

A supplier is the operator of a machine that runs `merc-agent` and may
claim work dispatched by the control plane. In this backend alpha that
person is the operator (or someone the operator already knows), the
machine is owned or directly controlled by the operator, and the worker
ID, agent version, and build hash must be on the canary allowlist.

Self-service supplier activation is off. Public signup as a supplier is
off.

## What you are agreeing the machine will do

1. Run only the approved agent binary, version, and build hash.
2. Accept work only for the approved worker identity on the approved
   device, network, runtime, model set, and location.
3. Use the machine's CPU, GPU, memory, disk, and network to execute the
   claimed task and write the result to the presigned output location.
4. Honour the agent's sandbox profile where it is available. The macOS
   sandbox restricts ports and local paths; it does **not** destination-pin
   HTTPS. Do not treat the sandbox as proof that input cannot leave the
   box.
5. Keep the device access-controlled, encrypted at rest as the operator
   requires, updated, and not shared with people who are not on the
   allowlist.

## What you are agreeing you will not do

- Inspect, copy, retain, screenshot, log, disclose, sell, train on,
  reuse, or try to identify task input or output.
- Move the agent onto a machine that is not operator-controlled.
- Point the agent at live Stripe credentials, a non-canary control plane,
  or an unapproved model revision.
- Probe other accounts, workers, object keys, or administrative surfaces.
- Submit or generate prohibited content as defined in
  `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`.

## Payout mechanics (this stage)

The control plane may write ledger rows that look like supplier credits,
holds, releases, and clawbacks. Stripe test-mode Connect objects may be
created against a Canadian connected-account shape. Settlement currency
is CAD.

**Those rows are simulations.** No real money moves. No bank account is
credited. A test-mode transfer ID is not a payment. Test-mode amounts are
not wages, revenue, a price commitment, or evidence that a live payout
path is approved.

See `docs/ALPHA_PAYMENT_PAYOUT_DISCLOSURE.md`.

## Test-mode-only settlement

If you see a Stripe dashboard, it must be the test/sandbox dashboard.
Live keys (`sk_live_`, `rk_live_`, `pk_live_`) are refused by the control
plane without a separate live-payment activation. You must not attempt to
switch the agent or the host environment to live mode.

When live money is later authorised — if it ever is — a different
acknowledgement, tax classification, and identity check will be required.
This document does not survive that change.

## Termination

The operator may revoke the worker token, un-allowlist the worker ID,
pause dispatch, or take the device offline at any time, with or without
cause, including at the end of the evaluation. On instruction you must
stop the agent, support verified deletion of local caches the operator
names, and return or wipe any residual task material.

Revocation is not a layoff, a breach of an employment contract, or a
promise of severance. There is no payout to unwind because no live
payout occurred.

## Reporting

Report immediately, through the operator's incident path in
`docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`:

- suspected exposure of task content
- device compromise or loss of physical control
- unexpected prohibited content
- any use of a live payment credential

## What this document is not

It is not legal advice. It is not an approved supplier agreement. It is
not a DPA. It is not permission to run independently owned hardware. It
does not make the operator your employer.
