# Live money transition

> **NOT AN AUTHORIZATION.** This file is a checklist and a pointer at a
> checker. It does not activate live payments, approve anyone, compute an
> HMAC over real approvals, or change `ops/go-no-go.json`. Live money
> remains `NO_GO_PROHIBITED`. Do not create a real activation record from
> this document. Do not run `payment-activation-sign` because this file
> exists.

The control plane will not move live value until every input below is
present, candidate-bound, and HMAC-verified. Nothing else is an input to
the transition. Filling a row does not authorize live money.

Machine check (schema + running candidate; no live money):

```
python3 ops/scripts/validate-live-activation.py --self-test
python3 ops/scripts/validate-live-activation.py \
  --activation /absolute/private/candidate.json \
  --hmac-key-file /absolute/private/hmac-key \
  --commit "$(git rev-parse HEAD)" \
  --settlement-currency cad
```

Schema: `ops/live-payment-activation.schema.json`.
Signer (not this document): `ops/scripts/cx release payment-activation-sign`.
Deploy pin: `ops/go-closure-inputs.json` → `live_money_transition`.

Constants the checker and the binary share (`src/control/payment_authority.go`):

| Bound | Value |
|---|---|
| Activation window | positive and at most 72 hours |
| Recovery window | 0 to 30 days after `expires_at` |
| Signing lead | at most 7 days before `valid_from` |
| HMAC | HMAC-SHA256 over Go `json.Marshal({schema_version, activation})`, 64 lowercase hex |
| HMAC key | 32..4096 bytes, permission-restricted file, never inline in LIVE |
| Activation file | regular file, mode bits `027` clear, at most 64 KiB |
| Environment | `production` only |
| Currency | `cad`, `usd`, or `jpy`; must equal `MERC_SETTLEMENT_CURRENCY` |
| Approvals | exactly `payments`, `release_manager`, `security` |
| Stripe-Version | `2025-06-30.basil` (compiled `stripeAPIVersion`) |

---

## Inputs

### 1. Clean candidate identity

| Input | Accepted form |
|---|---|
| `candidate_commit` | 40-character lowercase Git SHA-1 embedded in the deployed control binary |
| Binary `modified` | `false`. A dirty build stamp is refused. |
| `MERC_ENV` | `production` |
| `MERC_CANDIDATE_COMMIT` | same 40-character SHA-1 the binary reports |

The activation is bound to that exact commit. A different HEAD, a dirty
binary, or a short SHA is refused.

### 2. Settlement currency

| Input | Accepted form |
|---|---|
| `MERC_SETTLEMENT_CURRENCY` | `cad`, `usd`, or `jpy`; must match the Stripe platform holdable balance |
| `activation.currency` | identical lowercase code |
| `MERC_PRICE_REFERENCE_TO_SETTLEMENT_RATE` | required when settlement is not `usd`: finite positive CAD (or JPY) major units per one USD |
| `MERC_PRICE_FX_REVISION` | required when settlement is not `usd`: immutable reviewed source/revision |

Current reviewed catalogue authority is CAD. Do not silently treat USD
board numbers as CAD.

### 3. Stripe live account eligibility (dashboard, external)

| Input | Accepted form |
|---|---|
| Platform identity | Stripe live business verification complete on the production account |
| Platform bank account | live payout destination on the platform account |
| Connect platform | live Connect signed up; live `ca_*` client id |
| Connected account | live Canadian connected account with payouts enabled in CAD |

Nobody on this machine can complete these. Test-mode Connect on
`acct_1TxbzMCwPLrR4vaY` does not satisfy them.

The whole test-mode Connect remainder is now proven, and none of it clears this
row. `ops/scripts/stripe-sandbox-connect.sh` creates a Canadian connected account,
transfers CAD to it, holds, manually releases, fails and reverses payouts, and
exercises restriction and capability events against a Connect-scoped webhook
endpoint — all on the sandbox platform, all recorded in
`evidence/external/stripe-sandbox-matrix.json` with real provider ids. A
test-mode `acct_` is not a live connected account. Reading the matrix's PASS as
live eligibility is the specific mistake this section exists to prevent.

### 4. Live credentials (files, never git, never chat)

| Input | Accepted form |
|---|---|
| `STRIPE_SECRET_KEY_SOURCE` | absolute path to a regular `0600` file containing `sk_live_*` or scoped `rk_live_*` |
| `STRIPE_SECRET_KEY` | **unset**. LIVE forbids an inline secret |
| `STRIPE_PUBLISHABLE_KEY` | `pk_live_*` for the buyer browser surface |
| `STRIPE_WEBHOOK_SECRET` | live `whsec_*` for the cash-event endpoint |
| `MERC_CONNECT_WEBHOOK_SECRET` | distinct live `whsec_*` for the Connect endpoint |
| `MERC_CONNECT_CLIENT_ID` | live `ca_*` |
| `MERC_PAYOUT_EXPORT` | **unset**. LIVE refuses it |

### 5. Live webhook endpoints

| Input | Accepted form |
|---|---|
| Billing endpoint | live `we_*`, `livemode=true`, `api_version=2025-06-30.basil` |
| Connect endpoint | distinct live `we_*`, `connect=true`, same API version |
| Billing events | every type the billing handler switches on (below) |
| Connect events | every type the Connect handler switches on (below) |

Compiled billing handlers:

- `setup_intent.succeeded`
- `payment_method.attached`
- `payment_intent.succeeded`
- `payment_intent.payment_failed`
- `charge.refunded`
- `charge.dispute.created`
- `charge.dispute.funds_withdrawn`
- `charge.dispute.funds_reinstated`
- `charge.dispute.closed`
- `radar.early_fraud_warning.created`
- `radar.early_fraud_warning.updated`

Compiled Connect handlers:

- `account.updated`
- `account.external_account.created`
- `account.external_account.updated`
- `account.external_account.deleted`
- `capability.updated`
- `payout.created`
- `payout.updated`
- `payout.paid`
- `payout.failed`
- `payout.canceled`
- `payout.reconciliation_completed`

`account.updated` is the only Connect event that changes the supplier's bank-
payout readiness, and its `payouts_enabled` fact is applied only in event-time
order. The three `account.external_account.*` events are retained as strictly
bound bank-account/card observations; they do not change readiness or move
money. `capability.updated` records the exact provider capability and status;
the payout worker refuses a new Merc transfer when the newest durable
`transfers` observation is not `active`. A missing historical observation is
reported as `unknown` and remains compatible with older enrolled accounts;
Stripe's transfer response remains authoritative for that request. The
`transfers` capability and a connected account's bank-payout readiness are
separate Stripe functions, so status surfaces expose both facts instead of
collapsing them. The `payout.*` events are retained as append-only observations
of Stripe's connected-account bank payout; they do not settle or reverse
Merc's separate supplier-credit transfer.

A live endpoint that omits a handled cash event will never deliver it.
`ops/scripts/validate-stripe-endpoint-subscriptions.py` checks the compiled
set against test-mode endpoints and **refuses a live key**; it does not
clear this row.

### 6. Host and payment mode (last)

| Input | Accepted form |
|---|---|
| Production TLS hostname | the host the live binary serves |
| `MERC_CONNECT_RETURN_URL` | `https://<that-host>/…` |
| `MERC_CONNECT_REFRESH_URL` | `https://<that-host>/…` |
| `MERC_PAYMENT_PROVIDER` | `stripe` |
| `MERC_PAYMENT_MODE` | `live` — set only after inputs 1–9 exist. Default production mode is `sealed`. |

### 7. HMAC key

| Input | Accepted form |
|---|---|
| `MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_SOURCE` | absolute path to a regular file, mode bits `027` clear, 32..4096 bytes |
| `MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY` | **unset**. LIVE forbids an inline HMAC key |
| `MERC_LIVE_PAYMENT_ACTIVATION_HMAC_KEY_FILE` | container path mounted from the source (`/run/secrets/merc-live-payment-activation-hmac-key`) |

The key is never printed. The example key inside
`ops/scripts/validate-live-activation.py` is not this input and must never be
installed.

### 8. Activation envelope

One JSON object conforming to `ops/live-payment-activation.schema.json`.
No extra keys. Unsigned input to the signer must have empty
`hmac_sha256`; the signed file must have a matching 64-hex digest.

| Input | Accepted form |
|---|---|
| `schema_version` | `1` |
| `activation.activation_id` | `^[A-Za-z0-9][A-Za-z0-9._:-]{7,127}$` |
| `activation.candidate_commit` | input 1 |
| `activation.environment` | `production` |
| `activation.currency` | input 2 |
| `activation.valid_from` | RFC3339 date-time |
| `activation.expires_at` | after `valid_from`, at most 72 hours later |
| `activation.recovery_expires_at` | from `expires_at` to 30 days after it |
| `activation.max_single_charge_minor` | integer ≥ 1 |
| `activation.max_single_payout_minor` | integer ≥ 1 |
| `activation.max_single_refund_minor` | integer ≥ 1 |
| `activation.max_single_reversal_minor` | integer ≥ 1 |
| `activation.external_aggregate_cap_ref` | 1..512 characters naming an **external** Stripe/account aggregate cap. Per-operation limits are not an aggregate cap. |
| `activation.approvals` | exactly three objects; input 9 |
| `hmac_sha256` | HMAC-SHA256 of the Go-canonical `{schema_version, activation}` body, 64 lowercase hex |
| `MERC_LIVE_PAYMENT_ACTIVATION_SOURCE` | absolute `0600` path to that file |
| `MERC_LIVE_PAYMENT_ACTIVATION_SHA256` | SHA-256 of the file bytes (not of the HMAC body) |
| `MERC_LIVE_PAYMENT_ACTIVATION_FILE` | container path mounted from the source |

HMAC body field order is the Go struct order in
`src/control/payment_authority.go`: `schema_version`, then `activation` with
`activation_id`, `candidate_commit`, `environment`, `currency`,
`valid_from`, `expires_at`, `recovery_expires_at`,
`max_single_charge_minor`, `max_single_payout_minor`,
`max_single_refund_minor`, `max_single_reversal_minor`,
`external_aggregate_cap_ref`, `approvals[]` (`role`, `approver`,
`reference`). Times remashal as Go `RFC3339Nano`.

After `expires_at`, new charges, payouts, and provider setup are refused.
Refunds, reversals, provider reads, and webhooks remain available until
`recovery_expires_at`. After that the process is not operationally ready.

### 9. Three named approvals

Exactly three, unique roles, no others:

| Role | Fields |
|---|---|
| `payments` | `approver` (1..256), `reference` (1..512) |
| `release_manager` | `approver` (1..256), `reference` (1..512) |
| `security` | `approver` (1..256), `reference` (1..512) |

Missing a role, a duplicate role, an empty approver, or an empty
reference is refused. An example approver is not an approval.

### 10. Controlled first live transaction

One human-executed charge at or below `max_single_charge_minor` after the
signed activation is installed and `/readyz` reports
`payment_mode=live` and `live_value_movement=true`. This is not a file
and is not performed by this checklist.

---

## Example (not an authorization)

The only signed example this repository produces is generated in memory
by `python3 ops/scripts/validate-live-activation.py --self-test`. It uses
`EXAMPLE-NOT-AN-AUTHORIZATION-*` identifiers, `example.invalid`
approvers, and an example HMAC key that must never be installed. A
self-test PASS is not an activation.

Unsigned shape, for field names only. Do not sign this block. Do not
install it.

```json
{
  "schema_version": 1,
  "hmac_sha256": "",
  "activation": {
    "activation_id": "EXAMPLE-NOT-AN-AUTHORIZATION-0001",
    "candidate_commit": "0000000000000000000000000000000000000000",
    "environment": "production",
    "currency": "cad",
    "valid_from": "2026-01-01T00:00:00Z",
    "expires_at": "2026-01-02T00:00:00Z",
    "recovery_expires_at": "2026-01-03T00:00:00Z",
    "max_single_charge_minor": 1,
    "max_single_payout_minor": 1,
    "max_single_refund_minor": 1,
    "max_single_reversal_minor": 1,
    "external_aggregate_cap_ref": "example-only/not-an-authorization/aggregate-cap",
    "approvals": [
      {"role": "payments", "approver": "payments@example.invalid", "reference": "EXAMPLE-NOT-AN-AUTHORIZATION-PAYMENTS"},
      {"role": "release_manager", "approver": "release@example.invalid", "reference": "EXAMPLE-NOT-AN-AUTHORIZATION-RELEASE"},
      {"role": "security", "approver": "security@example.invalid", "reference": "EXAMPLE-NOT-AN-AUTHORIZATION-SECURITY"}
    ]
  }
}
```

`candidate_commit` of forty zeros is not a running candidate. The
self-test replaces it with `git rev-parse HEAD` and signs only with the
example key.

---

## What the checker refuses

`ops/scripts/validate-live-activation.py` accepts a well-formed example and
refuses at least:

- wrong `candidate_commit` (not the running candidate)
- expired activation and recovery windows
- missing an approval role
- absent per-transaction caps
- HMAC missing, malformed, or not matching the example/candidate key
