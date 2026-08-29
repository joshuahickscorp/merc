# Backend-alpha payment and payout disclosure

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **DRAFT — PENDING COUNSEL AND PAYMENTS REVIEW — NOT A COMMERCIAL OFFER**
>
> Describes how money is *simulated* on the controlled backend alpha, and
> what would have to change before live money. Parent terms:
> `docs/CANARY_TERMS.md`.

- Document version: `draft-0.2-backend-alpha`
- Stripe mode: **test only**
- Settlement currency: **CAD**
- Connected-account country: **CA**
- Live money: **prohibited**

## The one sentence that matters

**No live money moves at backend alpha.** Test cards, test Customers,
test PaymentIntents, test Charges, test refunds, test disputes, and test
Connect transfers are Stripe sandbox objects. Card networks do not
process them. No supplier bank account is credited. A CAD number in the
ledger is not cash.

## What the software actually does today

From `src/control/payment_authority.go`, `src/control/schema.sql`, and the
billing tables:

1. The process classifies the Stripe secret. `sk_test_` / `rk_test_` is
   test mode. A live-looking key is refused unless a separate, HMAC-signed
   live-payment activation file is present. That file is not part of this
   alpha.
2. Settlement currency must match the Stripe account. This alpha is CAD.
   A USD quote against a CA/CAD test account is a misconfiguration, not a
   multi-currency product.
3. Buyers may have a `billing_customers.stripe_customer_id` and a
   `default_payment_method` that is a Stripe PaymentMethod id. Card PAN,
   CVC, and expiry are not stored in merc.
4. Suppliers may have `suppliers.stripe_acct` (a Connect account id).
   Stripe hosts KYC and tax collection. `tax_id` / `tax_country` columns
   were dropped from `suppliers`.
5. `ledger_entries` records buyer_charge, supplier_credit, platform_take,
   clawback, and stripe_fee rows, with payout_status values that include
   held / released / clawed_back. In this alpha those statuses are
   exercised against test-mode provider objects, or against the local
   simulator.
6. Payout release is blocked while an active dispute or abuse hold
   exists. That guard is real code. It still does not move live funds.

## What a buyer sees

- Quotes and job estimates in CAD (or in a test unit labelled as such).
- Optional sandbox free-credit (`buyers.free_credit_usd`) that admits
  work without a real top-up.
- If the Stripe test matrix is run: test PaymentIntents, captures,
  declines, refunds, and webhook application outcomes.

None of that is an invoice for a real supply. None of it is a consumer
credit product.

## What a supplier sees

- Ledger credits that look like earnings.
- Hold / release / reversal state.
- Possibly a test Connect account and test transfers.

None of that is wages. None of that is a promise that live payouts will
use the same split, the same hold, or the same Connect configuration.
Independently owned suppliers are not permitted, so there is no
third-party payee in this alpha.

## What test mode does **not** trigger

Moving money in Stripe test mode does not, by itself:

- transfer cardholder funds
- pay a supplier
- create a tax-remittance obligation on the simulated amount
- enrol anyone as an employee or independent contractor
- satisfy Stripe's live-mode identity, bank-account, or Connect
  verification checks (Identity does not verify in a sandbox; Connect
  test accounts omit sensitive fields)
- require a live-mode access policy or a go-live checklist

Using Stripe at all — including test keys — means the operator has
accepted the Stripe Services Agreement for the account. That click-through
is not a counsel review, and it is not live-mode onboarding.

## What changes when live money is allowed

A later, separate decision. It is not this document. At minimum the
operator would need, and this list is not a clearance:

| Change | Why it appears only then |
|---|---|
| Live API keys (`sk_live_` / `rk_live_` / `pk_live_`) | Test keys cannot accept real payment methods |
| HMAC-signed live-payment activation (`ops/live-payment-activation.schema.json`) | The binary refuses new live value movement without it |
| Stripe account identity / business verification | Live mode, not sandbox |
| Bank account (or other payout destination) on the platform and on connected accounts | Required to settle real CAD |
| Connect live onboarding, KYC, and the connected-account agreement | Test accounts skip identity |
| Canadian card-industry Code of Conduct disclosures | Live card acceptance in Canada |
| Tax analysis (GST/HST, supplier classification, information slips) | Real consideration |
| Counsel-approved buyer terms, refund/dispute language, and invoices | Real charges create real rights |
| Chargeback and reserve handling with actual funds at risk | Test disputes have no cash |
| A different supplier acknowledgement | Real payouts are not this alpha |

Until every one of those exists, treat every CAD figure as a prop.

## Prohibited in this alpha

- Configuring `MERC_PAYMENT_MODE=live`
- Exporting payouts
- Using a real card "just to see"
- Telling a participant they will be paid
- Describing a test transfer as income

## What this disclosure is not

It is not an approved pricing schedule. It is not tax advice. It is not
Stripe's own terms. It does not authorise live money.
