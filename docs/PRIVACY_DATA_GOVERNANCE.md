# Privacy data inventory, retention and minimization — backend alpha

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.

- Status: **DRAFT / CONTROL DESIGN INCOMPLETE / NOT AN APPROVED SCHEDULE**
- Review basis: `src/control/schema.sql` and the account / billing / DSAR code
  at the commit that last regenerated this draft
- Owner: **[PRIVACY OWNER REQUIRED]**
- Surface: controlled backend alpha · named invitees · Stripe test-mode ·
  CAD · CA · operator-controlled workers · synthetic workload input

This is the working data map. It is not a representation that retention,
export, deletion, backup tombstones, residency, or legal bases are
implemented or approved. “Proposed” periods are conservative engineering
targets for counsel to approve or replace.

## Processing boundary

```text
named invitee -> control API -> PostgreSQL + object storage
                            |-> operator-controlled supplier agent -> object storage
                            |-> Stripe test mode (CAD, CA)
                            |-> operator logs / metrics / backups
```

Public alpha-request capture is refused while the private canary is
enabled. Signup is refused unless the email is on
`MERC_CANARY_APPROVED_BUYER_EMAILS`.

Supplier agents receive presigned input and output URLs. The macOS
sandbox does not destination-pin outbound HTTPS. Operator-controlled
suppliers plus synthetic input are therefore mandatory for this alpha.

## Inventory (from the schema, not a template)

| Data set | Where it actually lives | What is in it | Who can reach it | Current behaviour | Proposed engineering target | Alpha rule |
|---|---|---|---|---|---|---|
| Buyer identity | `buyers` | email, bcrypt password hash, sandbox free-credit, created_at | operators; the buyer, via their session | retained until tombstone | delete or irreversibly pseudonymize within 30 days of verified closure unless a documented exception applies | named invitees only |
| Buyer tombstone | `buyer_identity_tombstones` | email SHA-256, reason, operator label, artifact-key digests | operators | append-only | keep as the deletion receipt; do not store the raw email | hash only |
| Sessions / API keys | `sessions`, `api_keys` | token/key hashes, labels, expiry/revocation | operators | expiry/revocation exist; physical purge is incomplete | purge expired/revoked rows after 30 days | revoke at alpha exit |
| Admin credentials | `admin_credentials`, `admin_actions` | key hashes, labels, audited actions | operators | retained | rotate; do not put personal narratives in action notes | operator-only |
| Alpha leads | `alpha_requests` | email, role, note, source IP, consent_at, status | operators | no enforced TTL; path refused while canary is on | delete denied/stale leads within 30 days | disposable test addresses if this path is ever re-enabled |
| Supplier identity | `suppliers` | email, Stripe Connect account id, data_country, owner_buyer_id | operators; Stripe (the Connect id) | retained; tax_id columns dropped | keep only approved identity/payment fields; period pending tax counsel | operator-controlled test supplier only |
| Worker / device | `workers`, `worker_tokens`, `worker_enrollment_codes`, `worker_credential_audit` | public keys, fingerprints, labels, hardware/runtime/region claims | operators; the owning supplier | persist | purge secrets promptly; pseudonymize device identifiers 30 days after offboarding unless hold | pre-approved devices only |
| Stripe identifiers (merc) | `billing_customers`, charge/cash/ledger tables | customer id, payment-method id, PaymentIntent id, charge id, amounts, CAD currency | operators; Stripe | retained | retain as financial simulation records; no PAN | Stripe test-mode only |
| Payment instruments | Stripe, not merc | cards, bank accounts, KYC documents | Stripe | Stripe-hosted | none in merc | test cards only |
| Input / output content | object storage keys on jobs/tasks/verification tables | the bytes of the job | control plane; assigned worker for the current task | no general lifecycle deletion | delete no later than 7 days after terminal completion | synthetic, non-personal, non-confidential only |
| Job / task metadata | `jobs`, `tasks`, execution history, failures | status, timings, worker/supplier ids, hashes | operators; buyer sees their own | persists | separate content from operational facts; 90-day proposed for non-financial metadata | no production identifiers in free text |
| Quotes / ledger / disputes | `quotes`, `ledger_entries`, `disputes`, payout funding/operations | CAD amounts, test provider refs, reasons | operators; buyer sees their own | retained indefinitely | period **UNSET pending payments/tax counsel** | test amounts only |
| Webhooks | `webhooks` | URL, sealed secret, attempts, last_error | operators; the buyer's endpoint | persist | revoke at closure; purge URL and errors within 30 days | test endpoints, no embedded credentials |
| Logs / metrics | process, proxy, container, host, alerting | may contain IP, ids, URLs, errors | operators of those systems | deployment-dependent | redact tokens; 30-day online / 90-day restricted security target | no request bodies |
| Backups | database dumps and object mirrors | whatever was in Postgres and the bucket | operators with backup credentials | local/offsite lifecycle not comprehensively evidenced | encrypt; canary rotation no longer than 35 days; apply deletion tombstones after restore | no production personal data in backups that cannot honour a tombstone |

## Minimization rules

1. Do not accept personal, confidential, regulated, production or
   credential data as **workload input** during the backend alpha.
2. Do treat invitee emails, password hashes, device keys, source IPs, and
   Stripe customer ids as personal information. They are.
3. Do not put request bodies, presigned URLs, tokens, passwords, payment
   method data, provider payloads or free-form workload content in logs
   or tickets.
4. Remove source IP from alpha capture unless counsel approves a
   documented security need.
5. Store exact country/residency only from reviewed evidence. A worker
   self-declaration is not proof of physical location.
6. Separate immutable accounting facts from content so legal retention
   does not force indefinite content retention.
7. Give object-storage, database, support, analytics and backup actors
   separate least-privilege credentials.

## Legal-basis and recipient matrix

No lawful basis is approved. Before processing personal data at any
scale beyond this closed invitee set, counsel must map each inventory
row to purpose, affected person, controller/processor role, lawful
basis, notice text, recipients, location, transfer mechanism,
contract/DPA, retention rule and rights handling.

The subprocessor register is currently empty. It must include hosting,
storage, Stripe, monitoring, support and every external supplier
organization before those parties receive data in a public or live-money
service.

For this backend alpha the only intended external recipient of payment
identifiers is Stripe, in test mode.

## Lifecycle control requirements

- Account closure must atomically block login, key use, worker claims,
  charges and payouts while a reviewed export/deletion workflow proceeds.
- Job deletion must cover input, output, partial, retry, verification
  and derived artifacts.
- Every purge must be idempotent, observable and recorded without
  retaining the erased content.
- Legal holds require named approver, scope, reason, dates and access
  control.
- Backup restores must replay deletion tombstones before restored
  service is opened to traffic.

A synthetic export / tombstone / object-sweep path exists and is
described in `docs/DSAR_RUNBOOK.md`. It is a technical rehearsal, not a
production rights procedure.

## Exit criteria

This control is not complete until tests prove inventory coverage,
export, correction, account closure, artifact purge, expired-credential
purge, retention sweeps, legal holds, and backup-restore tombstones.
Counsel must then approve the final schedule and notice. An accountable
privacy owner must sign. **No such signature exists.**
