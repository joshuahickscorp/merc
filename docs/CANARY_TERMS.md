# ComputExchange Controlled Canary Terms

> **DRAFT — PENDING COUNSEL REVIEW — NOT APPROVED FOR ACCEPTANCE OR PUBLICATION**
>
> This draft describes a synthetic-data, Stripe-test-mode evaluation only. It is
> not an authorization for production data, personal data, confidential data,
> real charges, real payouts, or public supplier participation. The operating
> legal entity, address, governing law, privacy contact, abuse contact, support
> contact, and effective date remain unset. No canary may begin until those
> fields and every approval in `ops/legal-review.json` are completed by an
> accountable human.

- Document version: `draft-0.1`
- Effective date: **UNSET**
- Operator: **[LEGAL ENTITY REQUIRED]**
- Privacy contact: **[PRIVACY CONTACT REQUIRED]**
- Abuse contact: **[ABUSE CONTACT REQUIRED]**
- Support contact: **[SUPPORT CONTACT REQUIRED]**

## 1. Controlled scope

The canary is an invite-only technical evaluation. Only named participants
approved by the operator may use it. Participant accounts, operator-controlled
supplier devices, workload types, models, daily task counts, and test budgets
must be allowlisted before use.

The canary permits only:

- data generated solely for testing and containing no information about a real
  person, customer, organization, credential, system, or confidential matter;
- operator-controlled supplier devices in an approved location;
- Stripe test-mode payment objects with no real cardholder funds or supplier
  payouts; and
- the exact workload and model versions recorded in the approved canary
  manifest.

Production, customer, personal, regulated, secret, export-controlled, or
confidential data is prohibited. Public signup, self-service supplier
activation, real-money operation, and independently owned supplier devices are
outside this draft's scope.

## 2. Eligibility and authority

A participant must be invited, be legally capable of accepting the final
terms, and have authority to act for the organization named during enrollment.
The operator may reject or remove a participant or device at any time. The
final terms must identify permitted jurisdictions, minimum age, business-only
or consumer eligibility, sanctions screening, and export-control requirements.

## 3. Experimental service

ComputExchange is a development preview. Work may fail, be delayed, be
repeated for verification, produce nondeterministic or incorrect output, or be
stopped without notice to protect participants or the service. No availability,
performance, output-quality, or earnings commitment is made in this draft.

Any speed objective or service credit must be separately stated in the final
terms. Silence is not an SLA. Test-mode amounts are simulations and are not an
offer, price commitment, wage, revenue forecast, or promise of payment.

## 4. Distributed processing boundary

The architecture can send task input to a supplier agent and receive output
from it. A software sandbox cannot prove that an independently controlled host
did not retain input. For this controlled canary, every supplier device must be
owned or directly controlled by the operator, reviewed before activation, and
restricted to synthetic data.

Before any broader pilot, the final documents must identify the parties'
privacy roles, approved subprocessors and locations, transfer mechanism,
security measures, retention/deletion duties, incident duties, audit rights,
and a data-processing agreement where required.

## 5. Participant data and instructions

Participants retain their rights in their submitted material. They grant the
operator only the limited rights needed to host, transmit, process, verify,
return, secure, and delete permitted canary material. Participants must have
all rights needed to provide their material and instructions.

Participants must not submit personal data, credentials, proprietary source
code, production prompts, trade secrets, security-sensitive system details, or
third-party confidential information. The operator may inspect synthetic
canary records for reliability, abuse prevention, support, and incident
response as described in the final privacy notice.

## 6. Acceptable use

Use must comply with `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`, applicable
law, third-party rights, and applicable model-use policies. Prohibited use
includes unlawful or harmful activity; child exploitation; malware; intrusion
or credential abuse; weapons or bodily-harm enablement; fraud, impersonation,
spam, or deceptive engagement; unauthorized professional advice; unlawful
discrimination; privacy invasion; rights infringement; safety-control
circumvention; and prohibited military or critical-infrastructure use.

The operator may reject input, suspend accounts or workers, preserve evidence,
notify affected parties, and make legally required reports. Final notice,
appeal, preservation, and reporting rules require counsel approval.

## 7. Supplier obligations

An approved canary supplier operator must:

- process tasks only through the approved agent and only on the approved
  device, network, runtime, model and location;
- not inspect, copy, retain, disclose, sell, train on, reuse, or attempt to
  identify task content;
- maintain device access control, encryption, updates, sandboxing and network
  restrictions required by the canary manifest;
- report suspected exposure, device compromise, policy breach, unexpected
  content, or loss of control immediately;
- stop work on instruction and support verified deletion; and
- permit technical evidence collection needed for the canary.

No employment, contractor, agency, tax, reimbursement, power-cost, equipment,
or payout terms are established by this draft. Counsel and tax review are
required before independently owned suppliers or real payouts are permitted.

## 8. Test pricing, disputes and payouts

All amounts in this canary are Stripe test-mode simulations. The canary
manifest must bind the currency, catalog version, quote rules, platform take,
processor assumptions, rounding policy, dispute window, refund behavior,
payout hold, carry-forward treatment, and maximum simulated exposure.

No real charge or payout is authorized. Before live money, final terms must
cover authorization and capture, invoices, taxes, processor and payout fees,
minimum charges, cent rounding, refunds, disputes, chargebacks, payout holds,
reversals, clawbacks, provider failure, unclaimed balances, and account
termination.

## 9. Models, output and intellectual property

Model outputs may be inaccurate, incomplete, unsafe, non-unique, or subject to
third-party rights. Participants are responsible for evaluating output before
any use. The final terms must allocate ownership and licenses without promising
non-infringement or fitness.

The batch-inference path uses Llama 3.2 materials. **Built with Llama.** Llama
3.2 is subject to the Llama 3.2 Community License and Acceptable Use Policy.
The release must ship the required agreement, attribution notice, and
applicable third-party notices before that model is made available.

The embedding path uses `sentence-transformers/all-MiniLM-L6-v2`, whose model
page declares Apache-2.0. The current closure worktree introduces exact model
revisions and artifact-hash enforcement, but that work and its test evidence
are not yet bound to a final candidate; model licensing also remains blocked.

## 10. Privacy, retention and deletion

The draft privacy notice and inventory are in
`docs/PRIVACY_NOTICE_DRAFT.md` and `docs/PRIVACY_DATA_GOVERNANCE.md`. Current
code does not implement a complete account export, erasure workflow, object
lifecycle, or backup-deletion tombstone. Until those controls are verified,
the canary may use only synthetic data and disposable test identities.

Final terms must state request channels, identity verification, response
periods, deletion exceptions, legal holds, financial-record retention and
backup behavior.

## 11. Security and incidents

Participants must promptly report suspected security, privacy, abuse, payment,
or safety events using the final published contacts. The operator may isolate
accounts, workers, models, storage, or payment processing while investigating.
The operator runbook is `docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`.

## 12. Suspension and termination

The operator may suspend or terminate canary access for risk, abuse, policy
breach, legal requirements, non-cooperation, or the end of the evaluation.
Termination must revoke credentials and trigger the documented export,
retention, deletion and legal-hold workflow.

## 13. Clauses counsel must supply

This draft deliberately does not invent legal language. Counsel must approve
or supply warranties and disclaimers, liability limitations, indemnities,
confidentiality, intellectual-property allocation, governing law, venue or
arbitration, consumer rights, tax responsibility, notices, assignment,
severability, force majeure, order of precedence, amendment and reacceptance,
survival, and any jurisdiction-specific disclosures.

## 14. Acceptance evidence

The final service must refuse access without affirmative acceptance of the
exact approved document version. Evidence must record the document SHA-256,
version, principal, timestamp, acceptance action, and only the minimum network
evidence approved by privacy counsel. Material changes require reacceptance.
An operator-written database row without the participant's affirmative action
is not acceptance.
