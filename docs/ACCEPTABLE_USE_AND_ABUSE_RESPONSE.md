# Acceptable Use and Abuse Response

> **DRAFT — PENDING COUNSEL AND TRUST/SAFETY APPROVAL**
>
> The user-facing policy, reporting contacts, appeal rules, sanctions process,
> mandatory-reporting duties and response targets are not approved. Until they
> are, only allowlisted synthetic workloads on operator-controlled devices may
> run.

- Policy version: `draft-0.1`
- Abuse contact: **[ABUSE CONTACT REQUIRED]**
- Emergency escalation: **[ON-CALL ROUTE REQUIRED]**

## Allowed canary use

The controlled canary is limited to benign, synthetic test cases designed to
exercise the two documented workloads, system reliability, verification and
Stripe test-mode reconciliation. An approved canary manifest must bind the
participants, models, devices, locations, data generators, rate limits and
maximum simulated spend.

## Prohibited use

Do not use the service to submit, generate, transform, classify, embed, store,
route or assist with:

- unlawful conduct or violation of another person's rights;
- exploitation or sexualization of children, human trafficking, non-consensual
  sexual material or failure to make a legally required report;
- violence, terrorism, self-harm, weapons, warfare, espionage, controlled
  substances, or operation of critical infrastructure or dangerous machinery;
- malware, credential theft, phishing, intrusion, denial of service,
  vulnerability exploitation without authorization, or evasion of safeguards;
- fraud, impersonation, spam, fake reviews, deceptive engagement,
  disinformation or claims that AI output is human-generated when disclosure is
  required;
- harassment, threats, unlawful discrimination, or decisions concerning
  employment, housing, credit, insurance, education, health, legal rights or
  other essential opportunities;
- unauthorized medical, legal, financial or other regulated professional
  practice;
- collection, inference or disclosure of personal, biometric, health,
  financial, precise-location, credential, private or confidential information;
- copyright, trademark, privacy, publicity, contractual or other rights
  infringement;
- evasion of geographic, model, account, rate, verification, payment or safety
  controls; or
- activity prohibited by an applicable model license or use policy, including
  the Llama 3.2 Acceptable Use Policy.

The controlled canary additionally prohibits all real-person data, production
data, secrets, proprietary customer material and externally owned supplier
devices even when a use would otherwise be lawful.

## Participant duties

Participants must use only their assigned identity and credentials; keep
credentials and worker devices secure; follow model, data and jurisdiction
restrictions; cooperate with a safety investigation; stop on instruction; and
report suspected abuse, unexpected prohibited content, data exposure, device
compromise, payment anomaly or control bypass promptly.

Participants must not probe another account, worker, task, object key or
administrative surface. Security research requires prior written scope and a
separate approved disclosure policy.

## Operator enforcement workflow

### 1. Receive and preserve minimally

Assign a case ID and record source, received time, affected identifiers and a
short allegation. Do not copy full workload content or credentials into a
ticket. Preserve only necessary evidence under restricted access, record its
hash and source, and apply a time-bounded hold approved by the incident lead.

### 2. Triage

Classify the report using the support severities in
`docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`. Immediately escalate credible risk of
ongoing physical harm, child exploitation, large-scale compromise, exposed
credentials, cross-account data or material payment loss. Counsel must define
mandatory-reporting triggers and jurisdictions before public operation.

### 3. Contain

Use the narrowest effective reversible control: block a job, revoke a token,
suspend a worker or account, disable a model/runtime cell, stop dispatch,
disable signup, pause test billing, restrict storage access, or isolate the
environment. For credible cross-account or supplier-host exposure, stop all
dispatch until scope is known.

No operator may release real funds, erase material evidence, contact law
enforcement, or notify external parties solely from this draft. Follow approved
payments, legal and incident authority.

### 4. Investigate and decide

Correlate immutable IDs and timestamps across account, worker, job, task,
object, verification, admin, provider and infrastructure records. Distinguish
policy violation, security defect, false positive and compromised account.
Record evidence, uncertainty, decision-maker, policy version and rationale.

Available outcomes are no action, warning, workload rejection, rate or scope
restriction, credential rotation, temporary suspension, permanent removal,
payment hold, model disablement, required remediation, or legally required
report. The final policy must define notice and appeal rights.

### 5. Recover and learn

Verify revoked access, purge prohibited content under the approved retention
and legal-hold process, restore only reviewed capabilities, notify affected
parties where required, and create corrective actions with owners and dates.
Do not train models or build unrelated profiles from abuse evidence.

## Minimum enforcement controls before canary

- Account, supplier, worker and model allowlists that default deny.
- A tested global dispatch and billing kill switch.
- One active dispute/abuse case per underlying event with idempotent intake.
- Bounded input sizes, structured reason codes and rate limits.
- Audited admin actions and two-person review for high-impact actions.
- Published reporting and privacy contacts with a staffed escalation route.
- A synthetic tabletop covering supplier exfiltration, prohibited content,
  payment dispute, credential compromise and model-license takedown.
- Evidence that active disputes and abuse holds prevent payout release.

## Model policy sources

- Llama 3.2 Acceptable Use Policy:
  <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/USE_POLICY.md>
- Llama 3.2 Community License:
  <https://github.com/meta-llama/llama-models/blob/main/models/llama3_2/LICENSE>

Links are references, not evidence of counsel approval or technical
enforcement. `ops/legal-review.json` remains authoritative for release status.
