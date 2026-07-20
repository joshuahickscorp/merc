# Privacy Data Inventory, Retention and Minimization

- Status: **DRAFT / CONTROL DESIGN INCOMPLETE / EXTERNAL CANARY BLOCKED**
- Review basis: source tree at `533788d69c0fa06863d8fbcf5b2fd793955c3bbd`
- Owner: **[PRIVACY OWNER REQUIRED]**

This is the working data map for the controlled synthetic-data canary. It is
not a representation that retention, export, deletion, backup tombstones,
residency, or legal bases are implemented. “Proposed” periods are conservative
engineering targets for counsel and accountable owners to approve or replace.

## Processing boundary

```text
buyer/participant -> control API -> PostgreSQL + object storage
                              |-> approved supplier agent -> object storage
                              |-> Stripe test mode
                              |-> deployment logs/metrics/backups
```

Supplier agents receive presigned input and output URLs. The macOS sandbox does
not destination-pin outbound HTTPS and the host is not remotely attested.
Operator-controlled suppliers plus synthetic input are therefore mandatory for
this canary.

## Inventory and proposed schedule

| Data set | Representative storage | Purpose/recipients | Current behavior | Proposed engineering target | Canary rule |
|---|---|---|---|---|---|
| Alpha leads | `alpha_requests`: email, role, note, source IP, timestamps | Access triage; operators | No enforced TTL | Minimize IP at collection; reject unrestricted note; delete denied/stale leads within 30 days and accepted lead record within 90 days after onboarding | Use disposable test addresses only until TTL exists |
| Buyer identity | `buyers`: email, password hash, creation time | Authentication/support; operators | Retained indefinitely | Delete or irreversibly pseudonymize within 30 days of verified closure unless a documented exception applies | Named invitees only |
| Sessions and keys | `sessions`, `api_keys`: token/key hashes, labels, expiry/revocation | Authentication/security; operators | Expiry/revocation exists, physical purge not comprehensive | Purge expired/revoked rows after 30 days; retain minimal security event separately only when justified | Revoke at canary exit |
| Supplier identity/payment | `suppliers`, billing/Connect identifiers and status | Onboarding, routing, payouts; Stripe/operators | Retained indefinitely; activation and verified location are incomplete | Keep only approved identity/payment fields; retention period pending tax, payment and employment counsel | Operator-controlled test supplier only; no real KYC or payout claim |
| Worker/device identity | `workers`, `worker_tokens`, enrollment and credential-audit tables: public keys, fingerprints, label, runtime/hardware claims, network/health state | Authentication, routing, fraud and support; operator/supplier | Audit and device records persist | Purge secrets promptly; pseudonymize device identifiers 30 days after offboarding unless incident hold applies | Pre-approved devices only |
| Input and output content | Object storage keys referenced by jobs/tasks, including partial results | Requested compute; control plane and assigned supplier | No general lifecycle deletion path found | Delete input, output and partial objects no later than 7 days after terminal completion; allow participant-requested earlier deletion where compatible with integrity checks | Synthetic, non-personal, non-confidential data only |
| Job and task metadata | `jobs`, `tasks`, execution history, failures, events, duration and memory samples | Delivery, verification, support, reliability and pricing | Core history persists; only telemetry subsets have sweep periods | Separate content from operational facts; delete/pseudonymize linkable non-financial metadata after 90 days; preserve only aggregated metrics | No production identifiers in free text |
| Verification and fraud evidence | honeypots, verdicts, resolution, verification work, reputation and fraud flags | Integrity/abuse; operators and assigned workers as needed | Retained indefinitely in several tables | 180-day proposed maximum unless an active incident/dispute/legal hold requires more; document access | Synthetic evidence only |
| Quotes and economic plans | `quotes`, job economic plans/reserves, catalog and schedule facts | Price binding and reconciliation; operators/participants | Retained indefinitely | Keep with related transaction for approved financial retention; otherwise pseudonymize after 90 days | Test amounts only |
| Ledger/payment/payout | ledger entries, charge batches/operations, cash collections, provider events, disputes, payout funding/settlements/operations | Accounting, fraud, disputes, tax; Stripe/operators | Retained indefinitely | Exact fields and period **UNSET pending payments/tax counsel**; retain no workload content; use restricted immutable archive where required | Stripe test mode only |
| Webhooks | URL, secret material/hash as implemented, attempts and errors | Participant notification/support | Registration and error history persist | Revoke at closure; purge endpoint and detailed errors within 30 days; retain aggregate delivery metrics only | Test endpoints with no embedded credentials |
| Admin/support records | admin actions, dispute reasons, operational notes and tickets | Security, support, legal defense | Database and external-ticket retention not unified | Structured reason codes; prohibit unnecessary personal/content copies; 180-day proposed period unless hold applies | Use scenario identifiers, not real-person narratives |
| Logs/metrics | process, proxy, container, host and alerting systems | Operations/security | Deployment-dependent; may contain IP, IDs, URLs and errors | Redact tokens/query data; 30-day online target and 90-day restricted security target, subject to counsel | Synthetic identifiers; no request bodies |
| Backups | database dumps and object mirrors | Recovery | Local/raw and offsite lifecycle not comprehensively evidenced | Encrypt, restrict, rotate on a documented schedule no longer than 35 days for canary, and apply deletion tombstones after restore | No canary until encrypted restore/tombstone drill passes |

## Minimization rules

1. Do not accept personal, confidential, regulated, production or credential
   data as workload input during the controlled canary.
2. Do not put request bodies, presigned URLs, tokens, passwords, payment method
   data, provider payloads or free-form workload content in logs or tickets.
3. Remove source IP from alpha capture unless counsel approves a documented
   security need; if needed, truncate or keyed-hash it and enforce a short TTL.
4. Replace free-text operational reasons with bounded reason codes plus a
   separately access-controlled note only where necessary.
5. Store exact country/residency only from reviewed evidence. A worker or
   supplier self-declaration is not proof of physical processing location.
6. Separate immutable accounting facts from content and direct identifiers so
   legal retention does not force indefinite content retention.
7. Give object-storage, database, support, analytics and backup actors separate
   least-privilege credentials and auditable access.

## Legal-basis and recipient matrix

No lawful basis is approved. Before processing personal data, counsel must map
each inventory row to purpose, affected person, controller/processor role,
lawful basis, notice text, recipients, location, transfer mechanism,
contract/DPA, retention rule and rights handling. Consent must not be used as a
default where it is not freely given or cannot be withdrawn.

The subprocessor register is currently empty and therefore not release-ready.
It must include hosting, storage, Stripe, monitoring, support and every external
supplier organization before those parties receive data.

## Lifecycle control requirements

- Account closure must atomically block login, key use, worker claims, charges
  and payouts while a reviewed export/deletion workflow proceeds.
- Job deletion must distinguish cancellation from erasure and cover input,
  output, partial, retry, verification and derived artifacts.
- Every purge must be idempotent, observable and recorded without retaining the
  erased content.
- Legal holds require named approver, scope, reason, start/review/end dates and
  access control. A hold must not silently apply to unrelated data.
- Backup restores must replay deletion tombstones before restored service is
  opened to traffic.
- Retention jobs must emit counts and oldest-record age without leaking record
  contents.

## Exit criteria

This control is not complete until tests prove inventory coverage, export,
correction, account closure, artifact purge, expired-credential purge,
retention sweeps, legal holds, and backup-restore tombstones. Counsel must then
approve the final schedule and notice, and an accountable privacy owner must
sign `ops/legal-review.json` for an exact commit.
