# Alpha governance drafts — what was written, what was found, what remains

> **DRAFT · INTERNAL · NOT LEGAL ADVICE**
>
> Not reviewed by counsel. Does not constitute legal approval or compliance.
>
> This report is the honest residue of lane L4. It is not a qualified
> approval. It does not close `privacy-qualified-approval`,
> `licensing-provenance-approval`, or `staffed-abuse-route-or-tabletop`.
> Those receipts require a named human signature against evidence that
> does not exist yet. A draft plus this sentence is the honest output.

- Version: `draft-0.2-backend-alpha`
- Surface described by every document in this lane: controlled backend
  alpha · limited operator-known participants · no public website · no
  broad marketing · no production-scale public signup · Stripe test-mode
  only · settlement CAD · connected-account country CA

## Documents created vs updated

| Item | Path | Action |
|---|---|---|
| 1. Alpha terms | `docs/CANARY_TERMS.md` | **Updated** — re-scoped from a generic canary to this backend alpha |
| 2. Supplier acknowledgement | `docs/ALPHA_SUPPLIER_ACKNOWLEDGEMENT.md` | **Created** |
| 3. Buyer acknowledgement | `docs/ALPHA_BUYER_ACKNOWLEDGEMENT.md` | **Created** |
| 4. Privacy summary | `docs/PRIVACY_NOTICE_DRAFT.md` | **Updated** — inventory taken from schema and code |
| 5. Acceptable-use language | `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md` | **Updated** — same prohibitions, alpha surface restated |
| 6. Risk disclosure | `docs/ALPHA_RISK_DISCLOSURE.md` | **Created** |
| 7. Payment / payout disclosure | `docs/ALPHA_PAYMENT_PAYOUT_DISCLOSURE.md` | **Created** |
| 8. Data handling summary | `docs/PRIVACY_DATA_GOVERNANCE.md` | **Updated** — schema-derived table |
| 9. Open-source license inventory | `docs/LICENSE_INVENTORY.md` + `docs/generated/license-inventory.json` | **Generated** from the lockfiles |
| 10. Dependency / license report | `docs/LICENSE_INVENTORY.md` + `docs/THIRD_PARTY_LICENSES.md` | **Generated/register-backed** from the same run |
| DSAR runbook | `docs/DSAR_RUNBOOK.md` | **Updated** header and scope only |
| Support / incident runbook | `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md` | **Updated** header and scope only |
| Third-party register | `docs/THIRD_PARTY_LICENSES.md` | **Updated** — points at the generated graph; model rows stay BLOCKED |
| Supply-chain policy | `docs/SUPPLY_CHAIN_POLICY.md` | **Updated** header only |

No parallel duplicate was introduced where a live document already
covered the topic. New files exist only where the existing set had no
supplier ack, buyer ack, risk disclosure, or payment disclosure.

Every document above carries the mandatory header. None of them asserts
approval or compliance.

## Personal-data inventory found in the schema and code

Sources: `src/control/schema.sql`, `src/control/alpha_request.go`,
`src/control/accounts.go`, `src/control/data_governance.go`.

**Held by merc**

- `buyers.email`, `buyers.password_hash` (bcrypt; nullable)
- `sessions.token_hash`, `api_keys.key_hash` / `name` / `masked`
- `admin_credentials.key_hash` / `label`
- `suppliers.email`, `suppliers.stripe_acct`, `suppliers.data_country`
- `alpha_requests.email`, `role`, `note`, `source_ip`, `consent_at`
  (public path refused while the private canary is enabled)
- `worker_tokens.device_fingerprint`, `device_public_key`, `label`
- `workers.hardware_identity`, `os_version`, `region`, hardware claims
- `billing_customers.stripe_customer_id`, `default_payment_method` (an id)
- charge / cash / ledger / dispute rows: amounts, CAD, Stripe object ids
- `webhooks.url` and delivery errors
- object-storage keys for job/task/verification artifacts
- `buyer_identity_tombstones.email_sha256` after deletion

**Not held by merc**

- Card PAN, CVC, expiry
- Bank account numbers
- Government ID / KYC documents
- Supplier `tax_id` / `tax_country` (columns dropped)

**Held by Stripe when the test matrix is exercised**

- Test customer, payment-method, PaymentIntent, charge, refund, dispute,
  Connect account, and transfer objects. Test mode: card networks do not
  process them; Identity does not verify; Connect test accounts omit
  sensitive fields.

So the honest one-line answer is **not** only "email addresses and
payment metadata held by Stripe." It is:

> named-invitee emails and password hashes in Postgres; device public
> keys and fingerprints; webhook URLs; hardware/region claims; Stripe
> **test-mode** customer / payment-method / Connect **identifiers** (not
> PANs); and synthetic workload artifacts in object storage.

If participants use real emails — they will — that is personal
information.

## Generated license / dependency result

Run: `python3 ops/scripts/generate-license-inventory.py`

| Graph | Count | License source |
|---|---|---|
| `src/control/go.mod` modules | 25 | LICENSE file inside the exact `proxy.golang.org` module zip |
| `src/control/go.sum` versions not in `go.mod` | 8 | listed, not licensed (checksum-only) |
| `src/agent/Cargo.lock` packages | 403 | declared `license` in the matching crate `Cargo.toml` |
| Python SDK | 1 first-party, 0 runtime deps | `pyproject.toml` Apache-2.0 |
| TypeScript SDK lock | 2 (root + `typescript` devDep) | lockfile Apache-2.0 |

- Incompatible copyleft in this software graph: **0**
- Undeclared third-party rows: **0**
- Catalogue models, Geist fonts, visual assets: **excluded** and still
  **BLOCKED** / **INCOMPLETE / RELEASE BLOCKING** in
  `docs/THIRD_PARTY_LICENSES.md`

A generated "compatible" verdict is not a clearance. Apache-2.0 notice
reproduction still applies if the agent binary is given to a supplier.

## The three readiness receipts this lane does not mint

`ops/scripts/validate-readiness.py` will award a point for each of:

- `evidence/external/privacy-qualified-approval.json`
- `evidence/external/licensing-provenance-approval.json`
- `evidence/external/staffed-abuse-route-or-tabletop.json`

only if a named human, a real organisation, an RFC3339 timestamp, and
cross-checks against live ledgers all pass. Those files are **absent**.
That is intentional. Drafts were the missing work. A human signature is still required.
Fabricating `status: PASS` would be a lie.

`ops/legal-review.json` stays `NO_GO`. Every approval row stays
`PENDING`. `ops/governance-review-packets.json` stays
`PREPARED_PENDING_QUALIFIED_REVIEW`.

## Honest external residue

A requirement is listed below only if an actual law, an actual payment
provider rule, an actual contractual obligation, or an actual regulated
activity forces it. Project taste and the readiness scorecard are called
out separately so they are not smuggled in as law.

### Before backend alpha

**Nothing external requires a lawyer, a payment-counsel opinion, a
staffed 24/7 abuse desk, or Stripe live-mode onboarding before a
Stripe-test-mode, operator-known, no-public-website backend alpha.**

What *does* attach, and is easy to over- or under-state:

1. **Stripe Services Agreement.** The SSA is effective when the operator
   first accesses or uses Stripe, including test/sandbox keys
   ([Stripe SSA](https://stripe.com/legal/ssa);
   [API keys / sandbox vs live](https://docs.stripe.com/keys)). If test
   keys already exist, that acceptance has already happened. The SSA
   does **not** require KYC, a bank account, Connect live verification,
   or a go-live checklist to use sandbox keys. Sandbox: card networks do
   not process payments; Identity does not verify; Connect test accounts
   omit sensitive fields. Using a real card on live infrastructure to
   "test" is a separate SSA violation and is not this alpha.

2. **Personal information.** Invitee emails, password hashes, device
   keys, and Stripe customer ids are personal information. A Canadian
   commercial organisation collecting them can fall under PIPEDA. That
   is not "no privacy law applies because it is an alpha." It is also
   not "you must have a signed counsel opinion before two named
   colleagues may log in." PIPEDA requires a purpose, consent
   proportionate to the collection, and reasonable safeguards. The
   account paths already require a password and, on the public
   alpha-request path, `consent_at`. The drafts in this lane are the
   notice. A DPO, a published website privacy policy, and a
   counsel-signed lawful-basis matrix are not statutory preconditions
   for this closed test. If an invitee is in the EEA or UK, GDPR / UK
   GDPR can attach the same way — notice and a lawful basis, not a
   mandatory DPO for a two-person test.

3. **License obligations on what actually ships.** Nothing ships
   publicly. The Apache-2.0 agent may be installed on operator-known
   machines; that is limited distribution, and the `src/agent/LICENSE` /
   `NOTICE` files already exist. Llama's AUP and "Built with Llama"
   apply if that model is actually run; they are already in `NOTICE`.
   Public-site font OFL obligations do not attach while there is no
   public website. The generated software graph found no strong
   copyleft. Model/font/asset rows remain blocked for any **public**
   or **live-money** distribution.

4. **Test-mode money.** Simulated CAD charges and Connect transfers do
   not move funds, do not create a tax remittance on the simulated
   amount, do not classify anyone as an employee, and do not trigger
   FINTRAC / MSB activity. They also do not satisfy live-mode Stripe
   checks.

5. **The project's own readiness model** still wants three human
   signatures for three points. That is a gate this repository invented.
   It is not a statute.

Operator duties that are real and cheap, and are not "hire counsel":

- do not flip live keys
- give invitees the draft notice (these documents)
- keep workload input synthetic
- ship `src/agent/LICENSE` and `NOTICE` with any agent binary that leaves
  the operator's hands
- follow the Llama AUP if Llama is served
- treat invitee emails as personal information

### Before live money / public launch

Do not lose this list just because the alpha list is short.

- Stripe live keys, live webhook secrets, and the HMAC-signed live
  payment activation (`ops/live-payment-activation.schema.json`)
- Stripe live identity / business verification and a payout destination
- Connect live onboarding, KYC, and the connected-account agreement
- Canadian card-industry Code of Conduct / FCAC disclosures for live
  card acceptance
- Tax: GST/HST, supplier classification, information slips, invoices
- Counsel-approved terms, privacy notice, AUP, acceptance/reacceptance
  design, governing law, liability, consumer-rights analysis
- Lawful-basis / roles / subprocessor / transfer / retention matrix
  actually approved, not merely drafted
- Independently-owned supplier contracts, location evidence, DPA, and
  destination-pinned egress **if** that expansion is even attempted
- Staffed abuse/incident route and mandatory-reporting rules
- Owner-approved project license plus counsel-approved third-party
  register, including Llama, MiniLM, fonts, and visual assets
- Public-site license notices and any distribution NOTICE bundle
- Production DSAR channel, identity verification, and two-person
  deletion
- PCI posture via Stripe (no PAN in merc, but live acceptance still has
  SAQ obligations)
- The eight named governance approvals in
  `ops/governance-approval-bundle.schema.json`

Live money and public launch remain `NO_GO_PROHIBITED` in
`ops/go-no-go.json`.

## What a reviewer should sign, if they sign anything

Not this report. A reviewer who later wants to mint the three external
receipts still has to:

1. Read the drafts against the exact candidate commit.
2. Decide whether the closed alpha, as described, is acceptable.
3. Put their name, organisation, scope, evidence URI, and timestamp on
   a bundle that is **not** this file.
4. For privacy: also execute an external subprocessor deletion, and
   update `evidence/autonomous/technical-exercises.json` so it no longer
   says that exercise is `NOT EXECUTED`.
5. For licensing: clear every BLOCKED row in `ops/asset-provenance.json`
   and the model register — which this lane deliberately did not do.
6. For abuse: staff a real contact or run a timed multi-role tabletop,
   and update the technical ledger so the qualified human tabletop is no
   longer `NOT EXECUTED`.

Until then the honest state is: **drafts exist; approvals do not.**
