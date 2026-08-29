# merc Privacy Notice — backend alpha

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> **DRAFT — PENDING PRIVACY COUNSEL REVIEW — DO NOT PUBLISH**
>
> Field list derived from `src/control/schema.sql`, `src/control/alpha_request.go`,
> `src/control/accounts.go`, and `src/control/data_governance.go`. It is not a
> completed privacy notice. Writing it does not make the system compliant.

- Version: `draft-0.2-backend-alpha`
- Effective date: **UNSET**
- Surface: controlled backend alpha · named invitees · no public website ·
  Stripe test-mode · CAD · CA
- Controller/operator: **[LEGAL ENTITY, ADDRESS AND REGISTRATION REQUIRED]**
- Privacy contact/DPO: **[CONTACT AND DPO DETERMINATION REQUIRED]**

## What is actually collected

This is not "whatever a template says." It is what the schema and code
store. The short version is:

**Email addresses of named invitees; bcrypt password hashes if a
password is set; hashed session and API keys; device public keys and
fingerprints; webhook URLs; hardware/region claims; Stripe test-mode
customer, payment-method, PaymentIntent, and Connect account
identifiers (not card numbers); and whatever synthetic workload
artifacts the invitee uploads.**

If the public alpha-request path is enabled (it is **refused** while the
private canary is on), the service also stores `alpha_requests.email`,
`role`, free-text `note`, `source_ip`, and `consent_at`.

### Identity and credentials (merc Postgres)

| Table | Personal / identifying fields |
|---|---|
| `buyers` | `email`, `password_hash` (bcrypt; may be null), `created_at` |
| `buyer_identity_tombstones` | `email_sha256` after deletion, operator label |
| `sessions` | `token_hash` (SHA-256 of the session token), `buyer_id` |
| `api_keys` | `key_hash`, optional `name` / `masked`, `buyer_id` |
| `admin_credentials` | `key_hash`, `label` (operator, not a buyer) |
| `suppliers` | `email`, `stripe_acct` (Connect account id), `data_country`, `owner_buyer_id` |
| `alpha_requests` | `email`, `role`, `note`, `source_ip`, `consent_at`, `status` |

### Devices (merc Postgres)

| Table | Fields |
|---|---|
| `workers` | `hardware_identity`, `os_version`, `region`, `region_provenance`, hardware claims, runtime profile ids |
| `worker_tokens` | `device_fingerprint`, `device_public_key`, `label` |
| `worker_enrollment_codes` | `device_fingerprint`, `label`, owning buyer/supplier |

### Payments (merc holds identifiers; Stripe holds the instruments)

| Table | Fields |
|---|---|
| `billing_customers` | `stripe_customer_id`, `default_payment_method` (id, not PAN) |
| `charge_batches` / `buyer_charge_operations` / `buyer_cash_collections` | amounts, currency, `stripe_pi`, `charge_id`, status |
| `ledger_entries` | amounts, `payout_status`, `payout_ref` |
| `suppliers.stripe_acct` | Connect account id |

Card numbers, bank account numbers, and government ID documents are not
columns in this schema. Stripe hosts KYC and tax collection. Supplier
`tax_id` / `tax_country` were dropped.

### Workload content

`jobs` / `tasks` / verification tables store object-storage keys
(`input_ref`, `output_ref`, `result_ref`, artifact keys). The bytes live
in S3-compatible storage. For this alpha those bytes must be synthetic.
The architecture will still send them to the assigned supplier agent.

### Operational

`webhooks.url` and delivery errors; `admin_actions` actor labels;
`disputes.reason`; logs and metrics that may contain IP addresses,
internal ids, and error strings; backups of the above.

The field-level map and proposed retention are in
`docs/PRIVACY_DATA_GOVERNANCE.md`.

## Why this would be used

Subject to counsel approval, intended purposes for the backend alpha are:
let a named invitee in; run the job they submitted; route it to an
operator-controlled worker; verify output; simulate Stripe test-mode
billing; debug; stop abuse; and keep enough ledger history to understand
the simulation.

Data must not be used to train models, build advertising profiles, or
for an unrelated purpose without a separate documented decision. The
alpha does not permit personal data **in workload input**. Account
emails are personal data and are collected anyway.

## Who else can see it

- Operators with an admin key.
- The assigned supplier agent, for the current task's input and output
  objects. In this alpha that agent runs on an operator-controlled
  device. A sandbox does not make that host unable to retain the bytes.
- Stripe, for test-mode customer, payment, and Connect objects the
  operator creates. Stripe's DPA applies to personal data sent to
  Stripe, including in test mode.
- Hosting, object storage, logging, and backup systems the operator
  actually configured. The subprocessor register in
  `ops/legal-review.json` is empty and is not release-ready.

Independently owned supplier machines are out of scope.

## Retention and deletion

Most rows have no enforced TTL. A buyer tombstone and object-deletion
queue exist and have been rehearsed on synthetic data
(`docs/DSAR_RUNBOOK.md`, status **TECHNICAL WORKFLOW REHEARSED**).
Backups can restore deleted rows unless a tombstone is replayed. The
proposed schedule in `docs/PRIVACY_DATA_GOVERNANCE.md` is not
implemented and is not legally approved.

## Individual rights

Depending on location, a person may have rights to access, correct,
delete, restrict, object, port, or complain. No rights-request inbox is
approved. Operators must follow `docs/DSAR_RUNBOOK.md` and must not
improvise SQL against a live database.

This notice does not decide lawful basis, controller/processor roles, or
which statute applies. If any invitee is in Canada, PIPEDA can attach to
the emails. If any invitee is in the EEA or UK, GDPR / UK GDPR can
attach. Those statements are not a counsel opinion.

## Children, automated decisions, sensitive data

The backend alpha is not intended for children, consumer profiling,
eligibility decisions, biometrics, or sensitive data. Do not submit any
of those.

## Changes and contact

Placeholder contacts or this draft must never be presented as a
published notice. A human signature is still required before anyone may
call this approved.
