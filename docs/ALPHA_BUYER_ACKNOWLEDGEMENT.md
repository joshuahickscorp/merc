# Backend-alpha buyer acknowledgement

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **DRAFT — PENDING COUNSEL REVIEW — NOT AN ENFORCEABLE CONTRACT**
>
> This is what a buyer is being asked to understand for the controlled
> backend alpha. Parent terms: `docs/CANARY_TERMS.md`.

- Document version: `draft-0.2-backend-alpha`
- Scope: named invitees only · no public website · Stripe test-mode only · CAD
- Public self-serve signup: **off** unless a signed canary-disable decision exists

## Who a buyer is, in this alpha

A buyer is a named person the operator already knows, whose email is on
`MERC_CANARY_APPROVED_BUYER_EMAILS`. Signup of any other email is
refused. There is no public marketing site and no production-scale
signup funnel.

You must not invite others, share credentials, or treat a sandbox API
key as a production secret to hand to a customer.

## No availability guarantee, no SLA

This is pre-production software. The control plane, the agent, object
storage, and Stripe test-mode webhooks may fail, stall, restart, drop
work, or be halted without notice.

There is no uptime commitment, no latency commitment, no output-quality
commitment, and no service credit. Silence in these drafts is not an
SLA. Quotes, ETAs, and catalogue prices are test-mode figures for the
evaluation, not an offer to the public.

See `docs/ALPHA_RISK_DISCLOSURE.md`.

## Test-mode billing

If the alpha creates a Stripe Customer, PaymentIntent, Charge, refund,
dispute, or transfer, it does so in **test mode**. Test cards are the
only cards that work. Real cards are declined. No live money leaves your
bank or a test card network.

A CAD amount you see on a quote, a ledger row, or a Stripe test object
is a simulation. It is not an invoice you must pay in real currency, and
it is not a receipt you can take to tax authorities as evidence of a
real supply.

Sandbox free-credit grants (`buyers.free_credit_usd`) are an admission
gate for the evaluation. They are not a stored-value product and are not
redeemable for cash.

See `docs/ALPHA_PAYMENT_PAYOUT_DISCLOSURE.md`.

## Data the service actually holds about you

Derived from `control/schema.sql` and the account/billing paths, not from
a template:

| Held by merc | What it is |
|---|---|
| `buyers.email` | the allowlisted email you signed up with |
| `buyers.password_hash` | bcrypt of the password, if you set one |
| `sessions.token_hash` / `api_keys.key_hash` | hashes of session tokens and API keys |
| `billing_customers.stripe_customer_id` | Stripe test Customer id, if billing was exercised |
| `billing_customers.default_payment_method` | Stripe PaymentMethod **id**, not a card number |
| `webhooks.url` | callback URLs you registered |
| job / task / quote / ledger / dispute rows | operational and test-money history |
| object-storage keys for inputs and outputs | the artifacts themselves |

Card numbers, bank accounts, and government ID documents are **not**
stored in merc's database. Stripe hosts payment-method data. Tax-id
columns were removed from `suppliers`.

See `docs/PRIVACY_NOTICE_DRAFT.md` and `docs/PRIVACY_DATA_GOVERNANCE.md`.

## What happens to your artifacts

Workload input and output live in object storage under keys referenced
by `jobs` and `tasks`. Assigned supplier agents receive time-limited
presigned URLs for the current task only.

There is no general lifecycle deletion of objects today. A reviewed
buyer-tombstone path exists (`TombstoneBuyer`) and has been rehearsed on
synthetic data; it is not an approved production DSAR. Until a retention
schedule is approved and enforced, treat every artifact as something the
operator can still read, and do not upload anything you cannot afford to
lose or expose on an operator-controlled worker.

On account closure the operator intends to revoke sessions and keys,
stop new charges, tombstone the buyer, and enqueue object deletions.
Backups may still hold copies until a tombstone replay is applied.
That workflow is **not** a promise that every copy is gone.

## Your duties

- Submit only synthetic, non-confidential, non-personal workload input.
- Keep credentials to yourself.
- Follow `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`.
- Do not treat model output as advice, a fact, or a unique work you can
  ship without review.
- Report abuse, unexpected content, or credential loss through
  `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`.

## What this document is not

It is not legal advice. It is not an approved customer contract. It is
not a privacy notice you may publish. It does not make the alpha
generally available.
