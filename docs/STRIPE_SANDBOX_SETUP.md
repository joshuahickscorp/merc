# Stripe Sandbox setup

Do not add real money.
Do not use a real card.
Do not use sk_live.
Do not use live connected accounts.

This procedure is only for merc’s supervised no-value private canary.
It does not authorize production Stripe mode, real charges, transfers, payouts,
or public enrollment.

## Least-privilege inputs

Create or select a Stripe Sandbox and place the following values in the
gitignored mode-0600 `.env.go-closure` or an approved secret manager. Never put
them in chat, Git, CI output, screenshots, or evidence:

- `STRIPE_SECRET_KEY`: `sk_test_*`, or a sufficiently scoped `rk_test_*` key.
- `STRIPE_WEBHOOK_SECRET`: the `whsec_*` secret for the Sandbox cash-event endpoint.
- `MERC_CONNECT_WEBHOOK_SECRET`: a distinct `whsec_*` secret for the Sandbox Connect endpoint.
- `MERC_CONNECT_CLIENT_ID`: the Sandbox Connect client identifier.
- `STRIPE_TEST_CONNECTED_ACCOUNT_ID`: a disposable `acct_*` Sandbox connected account.
  It must be a project-controlled US test account with payouts enabled; the
  bundled matrix temporarily installs Stripe's documented success/failure test
  bank fixtures, switches the payout schedule to manual, and restores it.
- `STRIPE_BILLING_WEBHOOK_ENDPOINT_ID`: the enabled `we_*` Sandbox endpoint for
  `/v1/stripe/webhook`.
- `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`: the enabled `we_*` Sandbox endpoint for
  `/v1/stripe/connect-webhook` and connected-account events.
- Stripe CLI: installed locally; the command pins `STRIPE_API_KEY` to the already
  validated test-only key and never uses a saved live profile.

The API key needs only the Sandbox permissions used by customer, PaymentIntent,
refund, transfer, connected-account, event, and reconciliation checks. The
provider scenario driver is bundled at
`scripts/stripe-sandbox-scenarios.sh`; no operator-supplied code is required. Its
JSON receipt contains only identifier classes and no credential-shaped value.

## Run

```sh
chmod 600 .env.go-closure
make stripe-check
make stripe-matrix
```

`stripe-check` validates credential classes and confirms both the platform and
connected account are Sandbox objects. Every live-key check occurs before the
first network request. `stripe-matrix` creates disposable objects, exercises the
matrix, emits sanitized JSON, and attempts supported cleanup. It forces an
impossible client deadline and retries the exact idempotency key, proving safe
recovery when persistence at the provider is unknown. The deterministic
simulator separately covers both pre-persistence timeout and provider success
followed by a lost response. As part of the matrix, each configured HTTPS
webhook endpoint receives one inert signed probe and one invalid-signature
probe; it must accept only the event signed by its supplied Sandbox `whsec_*`
secret.

Until both commands pass with provider-owned receipts, readiness must remain:

```text
internal payment model: PASS
deterministic provider simulator: SIMULATED PASS
Stripe sandbox integration: NOT EXECUTED
Stripe live integration: PROHIBITED
```
