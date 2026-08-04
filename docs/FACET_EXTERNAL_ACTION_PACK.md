# Facet external action pack

**Audience:** a human operator who can hold Stripe, DNS, storage, and approval
authority. You do not need the repository's history — only this file, the
commands it names, and the linked setup docs.

**What this is:** the only remaining work that can move the readiness facet
axis past its current machine-derived score. Code on a development host cannot
close these points. That is intentional.

**What this is not:** a licence to invent receipts, hand-edit
`ops/readiness.json` `earned` fields, or loosen
`scripts/validate-readiness.py`. Hand-typed `earned` values are advisory and
ignored. The score is trustworthy precisely because empty prose cannot raise it.

---

## Current position (recomputed, not asserted)

```bash
python3 scripts/validate-readiness.py
```

Expected on a host with no staging, no offsite storage, and no human approvers:

```text
readiness: PASS (84/100 derived, P0=0, P1=8, Level B NO_GO)
```

| Domain | Derived | Gap | Blocker class |
|---|---:|---:|---|
| `source_and_ci` | 10/10 | 0 | — |
| `security` | 14/15 | 1 | external staging attack rehearsal |
| `money_and_reconciliation` | 9/15 | 6 | Stripe Connect client id, public TLS host, webhook matrix |
| `lifecycle_and_concurrency` | 10/10 | 0 | — |
| `artifacts_and_storage` | 6/8 | 2 | independent offsite copy |
| `agent_and_sandbox` | 8/8 | 0 | — |
| `database_and_recovery` | 7/8 | 1 | external offsite restore |
| `deployment_and_rollback` | 5/8 | 3 | qualifying 24 h soak on persistent staging |
| `observability_and_alerting` | 6/6 | 0 | — (staffed paging remains a release P1, not a facet gap) |
| `privacy_and_data_governance` | 3/4 | 1 | qualified privacy approval / external subprocessor deletion |
| `licensing_and_supply_chain` | 2/3 | 1 | license and asset provenance approval |
| `abuse_and_trust` | 1/2 | 1 | staffed human route or qualified tabletop |
| `support_and_incident_response` | 1/1 | 0 | — |
| `website_and_buyer_usability` | 2/2 | 0 | — |
| **Total** | **84/100** | **16** | all external |

**Machine-reachable ceiling: 84/100.** On a host that lacks persistent staging,
independent offsite storage, and human approvers, 84 is the maximum the receipt
schedule can derive. It is not underachievement and there is no missing local
code path that raises it.

### How the remaining 16 points are represented in the scorer

`DOMAIN_RECEIPTS` in `scripts/validate-readiness.py` fixes each domain's
`possible` total. Earned points are the sum of **wired** receipt rows that
exist on disk and pass their content check. The remaining 16 points each have
a receipt row under `evidence/external/` with a content check as strict as
`alert_delivery_proven`. Those files are absent today, so they contribute
zero; inventing a `status: PASS` stub will not pass the check.

| Domain | Pts | Wired path (absent until real work) |
|---|---:|---|
| `money_and_reconciliation` | 6 | `evidence/external/stripe-sandbox-matrix.json` |
| `deployment_and_rollback` | 3 | `evidence/external/qualifying-soak-24h.json` (+ raw samples JSONL named in the receipt) |
| `artifacts_and_storage` | 2 | `evidence/external/offsite-backup-verification.json` |
| `database_and_recovery` | 1 | `evidence/external/offsite-independent-restore.json` |
| `security` | 1 | `evidence/external/staging-attack-rehearsal.json` |
| `privacy_and_data_governance` | 1 | `evidence/external/privacy-qualified-approval.json` |
| `licensing_and_supply_chain` | 1 | `evidence/external/licensing-provenance-approval.json` |
| `abuse_and_trust` | 1 | `evidence/external/staffed-abuse-route-or-tabletop.json` |

So the operator loop for each gap is:

1. Obtain the credential, host, approval, or rehearsal this pack names.
2. Run the exact command or gate listed.
3. Retain the real evidence artifact at the wired path above (never paste
   secrets into git or chat). Shape must satisfy the content check in
   `scripts/validate-readiness.py`.
4. After the score moves, update `ops/go-no-go.json` `readiness_score` to the
   new derived total (the validator fails closed if they disagree).

Until genuine evidence lands at those paths and passes the content checks,
`validate-readiness.py` will still print `84/100`. The P1 exit criteria in
`ops/go-no-go.json` and `make release-doctor` still close on real evidence;
the facet score moves only when a content-checked external receipt is present.

Never run live Stripe keys. Live mode is refused by contract and prohibited for
Level C.

---

## Recommended order (points per unit of human effort)

Do work in this order. Rationale is effort and sequencing, not ceremony.

| Order | Domain(s) | Pts | Why first / next |
|---:|---|---:|---|
| **1** | `money_and_reconciliation` | **6** | Highest absolute return. Once a public HTTPS staging hostname exists, the remaining inputs are two dashboard values (Connect client id + recreated webhooks) and a scripted matrix. Roughly minutes of dashboard work plus one command pass. |
| **2** | `licensing_and_supply_chain` | **1** | Pure paper. No staging host. Can run in parallel with infra work. Provenance register and named reviewer only. |
| **3** | `privacy_and_data_governance` | **1** | Also paper / counsel. Can run in parallel with (2). |
| **4** | `artifacts_and_storage` + `database_and_recovery` | **2+1=3** | Same offsite bucket exercise closes both domains. One rehearsal, three facet points. |
| **5** | `deployment_and_rollback` | **3** | Persistent staging already required for (1). The extra cost is mostly wall-clock: a qualifying ≥86400 s soak on the uninterrupted candidate container. Low active effort after deploy. |
| **6** | `security` | **1** | Needs the staging surface from (1)/(5). External attack rehearsal against that host; local tabletops already scored. |
| **7** | `abuse_and_trust` | **1** | Lowest points per coordination cost: needs a staffed human route or qualified multi-role tabletop on calendars you do not control. |

**Do first if you can only do one thing:** stand up `STAGING_TLS_HOSTNAME` and
close the Stripe block. Six points, shared prerequisite for soak and attack
rehearsal, and the payments P1 (`P1-STRIPE-TEST`) the release ledger already
names.

**Do not start with** the abuse tabletop or a lone security drill if staging and
Stripe are still open — one staffed meeting buys 1 point; the same afternoon on
Connect + webhooks buys 6 and unblocks more.

Shared prerequisite note: items 1, 5, and 6 all need a project-controlled
persistent TLS staging host. Item 4 needs a **different** storage provider from
that host. Items 2 and 3 need neither.

---

## 1. `money_and_reconciliation` — 6 points (do first)

### What is required

| Input | Form | Who provides it |
|---|---|---|
| `STAGING_TLS_HOSTNAME` | DNS name with valid public HTTPS that Stripe can POST to | DNS / infra owner for the private-canary zone |
| `MERC_CONNECT_CLIENT_ID` | Sandbox Connect client id `ca_…` | Payments owner in **Stripe Dashboard → Connect → Settings** (dashboard-only; not fetchable from the API) |
| Billing webhook endpoint | Enabled `we_…` at `https://<host>/v1/stripe/webhook` | Payments owner; must pin API version `2025-06-30.basil` (null / mismatched versions fail closed; Stripe cannot update `api_version` in place — recreate) |
| Connect webhook endpoint | Distinct enabled `we_…` at `https://<host>/v1/stripe/connect-webhook` | Same; distinct `whsec_…` secret from billing |
| `STRIPE_SECRET_KEY` | `sk_test_*` or scoped `rk_test_*` only | Payments owner; live prefixes are refused before any network call |
| `STRIPE_TEST_CONNECTED_ACCOUNT_ID` | Disposable Canadian `acct_…` with payouts enabled | Payments owner |
| Endpoint ids + secrets | `STRIPE_BILLING_WEBHOOK_ENDPOINT_ID`, `STRIPE_CONNECT_WEBHOOK_ENDPOINT_ID`, `STRIPE_WEBHOOK_SECRET`, `MERC_CONNECT_WEBHOOK_SECRET` | From the recreated endpoints |

Store values only in gitignored mode-0600 `.env.go-closure` (or the approved
secret manager). Never commit them. Full input list:
`docs/STRIPE_SANDBOX_SETUP.md`, `ops/go-closure-inputs.json`.

### Commands / gate

```bash
cp ops/staging/env.go-closure.example .env.go-closure   # if not already
chmod 600 .env.go-closure
# fill STAGING_TLS_HOSTNAME, Stripe test inputs, MERC_CONNECT_CLIENT_ID, …

# Recreate both webhook endpoints against the live hostname with API version
# 2025-06-30.basil (see scripts/stripe-webhooks.sh and docs/STRIPE_SANDBOX_SETUP.md).
# Stale tunnel hostnames and api_version:null endpoints fail the contract.

set -a; . ./.env.go-closure; set +a
make stripe-check     # scripts/stripe-sandbox.sh check
make stripe-matrix    # scripts/stripe-sandbox.sh matrix
```

`stripe-check` must report test key class, distinct endpoint ids, Connect test
account present, `staging_hostname_valid: true`, `live_mode: PROHIBITED`.
`stripe-matrix` runs the bundled CAD provider scenarios (success, decline,
refund, dispute ordering, Connect restriction, payout hold/release/failure,
webhook signature and replay) and prints a sanitized
`kind:"stripe_sandbox_matrix"` receipt with `status:"PASS"`.

Related release gate: `P1-STRIPE-TEST` in `ops/go-no-go.json`.

### How to verify the facet moved

After real matrix evidence is written to
`evidence/external/stripe-sandbox-matrix.json` and passes
`stripe_sandbox_matrix_proven`:

```bash
python3 scripts/validate-readiness.py
# expect: money_and_reconciliation: derived=15/15
# expect: readiness: PASS (90/100 …)   # 84 + 6
```

Until that file lands with the full matrix shape, the matrix can still pass and
close the payments P1 while the facet stays at 9/15. Do not hand-type
`earned: 15`.

---

## 2. `licensing_and_supply_chain` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| License / asset provenance approval | Named reviewer resolves every `BLOCKED_*` row in `ops/asset-provenance.json` and model provenance | Licensing / brand / counsel authority listed in the governance packets |
| Model provenance | Same bundle exercise `asset_and_model_provenance` | Same |
| Project license clarity | Already present as `LICENSE`; provenance gaps are the open item | Repository owner + counsel |

Register path: `ops/asset-provenance.json` (status today
`BLOCKED_INCOMPLETE_PROVENANCE`). Review packet:
`ops/governance-review-packets.json`. Bundle shape:
`ops/governance-approval-bundle.schema.json` → `approvals.licensing` and
`exercises.asset_and_model_provenance`.

### Commands / gate

```bash
# After the restricted governance bundle is completed outside git:
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# equivalent: cd control && go run . release approvals-check --bundle <path>
make release-doctor
```

Related release gate: portion of `P1-GOVERNANCE`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: licensing_and_supply_chain: derived=3/3
# total +1 from the previous derived score
```

---

## 3. `privacy_and_data_governance` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Qualified privacy approval | Named privacy counsel / DPO approval of roles, purposes, retention, transfers | Privacy authority |
| External subprocessor deletion | Evidence that a real subprocessor deletion (or approved absence) was exercised — not only the local technical DSAR/tombstone path | Privacy + engineering |

Technical DSAR/deletion/tombstone already scores 3/4 via
`evidence/autonomous/technical-exercises.json` (unbound local technical receipt;
not qualified external evidence). The remaining point is the **qualified** half.

Bundle: `approvals.privacy` and `exercises.dsar_export_deletion` (qualified) in
the governance approval bundle. Context: `docs/PRIVACY_DATA_GOVERNANCE.md`,
`docs/DSAR_RUNBOOK.md`, `ops/legal-review.json` (`PRIV-001` still open).

### Commands / gate

```bash
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
make release-doctor
```

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: privacy_and_data_governance: derived=4/4
# total +1
```

---

## 4. `artifacts_and_storage` (2) + `database_and_recovery` (1) — 3 points together

Do these as one rehearsal. Local logical restore already earned the in-host
points (`logical-independent-restore.json`). What remains is an **independent
provider boundary**.

### What is required

| Input | Form | Who provides it |
|---|---|---|
| `MERC_BACKUP_OFFSITE` | `s3://…` bucket on infrastructure **different** from the deployment host | Storage / backup administrator |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | Restricted to that bucket; **not** the artifact-store keys | Same |
| `MERC_BACKUP_ENCRYPTION_RECIPIENT` | `age1…` public recipient | Backup owner |
| `MERC_BACKUP_DECRYPTION_IDENTITY_FILE` | Path to mode-0600 age identity **not** stored on the deployment host | Backup owner |

### Commands / gate

```bash
set -a; . ./.env.go-closure; set +a
make release-doctor CHECK=backup
# On persistent staging (preferred) or an authorized host with offsite creds:
bash scripts/backup.sh
# Independent download, decrypt, restore in isolation — the rollback rehearsal
# binds backup + restore + prior/candidate image recovery:
bash scripts/go-closure-rollback-rehearsal.sh --target ssh --execute
```

Expect a schema-v2 encrypted offsite backup manifest bound to the exact offsite
URI, a verification receipt with independently downloaded checksums, and a
restore that matches database/object invariants. Local-only restore is not
enough; the validator and P1 text require the independent download.

Related release gate: `P1-OFFSITE-RESTORE`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: artifacts_and_storage: derived=8/8   (+2)
# expect: database_and_recovery: derived=8/8   (+1)
# total +3 once both receipt paths are wired to the offsite evidence
```

---

## 5. `deployment_and_rollback` — 3 points

Local staging validation, rollback, and restart-storm receipts already score
5/8. The remaining 3 require a **qualifying 24-hour soak on persistent staging**
with the exact candidate image.

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Persistent staging stack | Host + TLS + compose state from `docs/STAGING_DEPLOYMENT_PLAN.md` | Operations |
| `MERC_CANDIDATE_CONTROL_IMAGE` / `MERC_CANDIDATE_COMMIT` | Immutable `@sha256:` image and matching full commit | Release engineering |
| Wall-clock window | ≥ 86400 s uninterrupted candidate container | Operations |
| Soak bounds | `MERC_SOAK_MAX_*` growth limits as in go-closure env | Operations |

A local `make soak-24h` on a laptop is not a substitute. Qualifying mode in
`scripts/go-closure-soak.sh` refuses durations under 86400 s and requires the
persistent candidate control container. The wired 60 s local soak receipt is
intentionally worth **0** points so short local runs cannot inflate the domain.

### Commands / gate

```bash
# After candidate is live on staging (see docs/STAGING_DEPLOYMENT_PLAN.md):
scripts/go-closure-deploy.sh --target ssh --activate candidate --execute
curl -sf "https://${STAGING_TLS_HOSTNAME}/healthz"
curl -s  "https://${STAGING_TLS_HOSTNAME}/readyz" \
  | jq '{status,payment_mode,live_value_movement}'
# require: status ready, payment_mode test, live_value_movement false

scripts/go-closure-soak.sh --target ssh --duration 86400 --interval 60 --execute
# retain the schema-v2 qualifying soak receipt the script emits
```

Related release gates: `P1-STAGING`, `P1-RECOVERY-SOAK`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: deployment_and_rollback: derived=8/8
# total +3 once the qualifying soak receipt is wired
```

---

## 6. `security` — 1 point

### What is required

| Input | Form | Who provides it |
|---|---|---|
| External staging attack rehearsal | Hostile exercise against the real staging TLS surface (authz, cross-tenant, break-glass under staging conditions), with a written receipt | Security owner / external reviewer |
| Staging host | Same `STAGING_TLS_HOSTNAME` boundary as above | Operations |

Local technical break-glass and the authorization matrix already score 14/15.
The missing point is explicitly the **external** rehearsal (see comment on
`security` in `DOMAIN_RECEIPTS`).

### Commands / gate

Run the approved external rehearsal against staging; preserve a redacted receipt
outside secret material. Include the security exercise in the governance bundle:

```bash
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# exercises.security_tabletop must be the qualified/external half, not only
# the local technical tabletop already reflected in technical-exercises.json
```

Related release gate: portion of `P1-GOVERNANCE` / independent security review.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: security: derived=15/15
# total +1
```

---

## 7. `abuse_and_trust` — 1 point (do last among equals)

### What is required

| Input | Form | Who provides it |
|---|---|---|
| Staffed human abuse route **or** qualified multi-role tabletop | Named humans on a real escalation path, or a timed tabletop with trust/safety + support + security roles | Trust & safety + support owners |
| Contacts | Non-placeholder contacts in the support/incident runbook where the release gates require them | Same |

Local technical tabletops already score 1/2 via
`evidence/autonomous/technical-exercises.json` (unbound local technical receipt;
`qualified_human_tabletop: NOT EXECUTED` in the derived technical receipt).

### Commands / gate

```bash
# After the staffed route or qualified tabletop receipt exists:
make approvals-check BUNDLE=/path/to/governance-approval-bundle.json
# exercises.support_tabletop / security_tabletop qualified halves as required
# by the bundle schema, plus supplier_policy approval for abuse scope
make release-doctor
```

Related docs: `docs/ACCEPTABLE_USE_AND_ABUSE_RESPONSE.md`,
`docs/SUPPORT_AND_INCIDENT_RUNBOOK.md`.

### How to verify the facet moved

```bash
python3 scripts/validate-readiness.py
# expect: abuse_and_trust: derived=2/2
# total +1
```

---

## Score arithmetic checklist (operator)

After each wired receipt lands, recompute — do not trust a remembered total:

```bash
python3 scripts/validate-readiness.py
# compare domain lines to the table at the top of this file
# ops/go-no-go.json readiness_score must equal the new derived total
# or the validator fails closed
```

Additive path if every gap closes in the order above:

| After closing | Running total |
|---|---:|
| Start (machine ceiling) | 84 |
| + money (6) | 90 |
| + licensing (1) | 91 |
| + privacy (1) | 92 |
| + artifacts (2) + database (1) | 95 |
| + deployment soak (3) | 98 |
| + security (1) | 99 |
| + abuse (1) | **100** |

GO for Level B still requires the separate P1 list (canary participants,
staffed alert receiver, independent PR reviewer, full governance bundle, etc.).
Hitting 95 on the facet is necessary but not sufficient while open P1s remain.
`go_threshold` is 95 and any open target-scope P0/P1 forces Level B `NO_GO`.

---

## What not to do

- Do not set `earned` by hand in `ops/readiness.json` and expect the score to
  rise. `scripts/test-readiness-gaming.sh` proves the validator rejects that.
- Do not use `sk_live`, real cards, or live connected accounts.
- Do not treat a Cloudflare quick tunnel, a laptop Compose stack, or a 60 s /
  300 s local soak as offsite, persistent staging, or a qualifying 24 h soak.
- Do not wire a receipt path for missing evidence "to prepare the score."
  Wire only after the real artifact exists, with a content check that refuses
  empty or simulated stand-ins.
- Do not invent governance approver names. An unsigned or self-approved bundle
  is not a qualified approval.

---

## Related surfaces (read-only context)

| Surface | Role |
|---|---|
| `scripts/validate-readiness.py` | Authoritative facet derivation |
| `ops/go-no-go.json` | Level A/B/C decisions and P1 exit criteria |
| `ops/readiness.json` | Advisory ledger; `earned` ignored by the scorer |
| `ops/go-closure-inputs.json` | Exact operator input names |
| `docs/EXTERNAL_OPERATOR_HANDOFF.md` | Broader external-only release handoff |
| `docs/STAGING_DEPLOYMENT_PLAN.md` | Ordered staging deploy commands |
| `docs/STRIPE_SANDBOX_SETUP.md` | Stripe test inputs and matrix |
| `RELEASE_READINESS.md` | Scope-separated release narrative |
| `docs/SHIPPABILITY_STATUS.md` | Capability vs release authority |

---

*Operator pack. External receipt rows are wired in `scripts/validate-readiness.py`
so real artifacts can earn the reserved 16 points; domain `possible` totals and
existing content checks are unchanged. Empty or fabricated files still score 0.*
