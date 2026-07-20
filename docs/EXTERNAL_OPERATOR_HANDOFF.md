# External-only release handoff

This handoff contains no secret values. Never paste a credential into an issue,
pull request, CI log, evidence receipt, or chat. Store credentials only in the
named external secret boundary and run the verification command from a clean
operator shell.

| Requirement | Where the operator obtains it | Minimum scope | Secure storage | Verification command | Gate closed |
|---|---|---|---|---|---|
| Persistent staging provider credentials | Approved infrastructure provider or organization cloud administrator | Create/update only the private ComputExchange staging service, persistent volumes, firewall rules, and secret references | Provider secret manager; expose only as `.env.go-closure` references on the deployment host | `scripts/cx release validate-staging && scripts/go-closure-deploy.sh --target ssh --activate candidate --check` | `P1-STAGING` |
| DNS hostname or DNS authorization | Organization DNS administrator for the approved private-canary zone | Create/update the single staging A/AAAA/CNAME record and authorize certificate issuance for that hostname only | DNS provider secret manager; certificate material in the staging secret store | `make release-doctor CHECK=staging` followed by the documented deployment probe | `P1-STAGING` |
| Independent offsite backup destination | Approved storage provider and a separate backup administrator | Write encrypted backup objects and read them back in the dedicated backup prefix; no plaintext permission | Backup-provider secret manager under a principal separate from staging | `scripts/go-closure-rollback-rehearsal.sh --target ssh --execute` | `P1-OFFSITE-RESTORE` |
| Stripe sandbox `sk_test` key | Stripe Dashboard in test mode from the authorized payments administrator | Test-mode customers, PaymentIntents, refunds, disputes, and required read-only reconciliation; no live resources | Organization secret manager mapped only to `STRIPE_SECRET_KEY` in `.env.go-closure` | `scripts/cx release stripe-check` | `P1-STRIPE-TEST` configuration half |
| Stripe sandbox webhook `whsec` secret | Stripe Dashboard test-mode webhook endpoint settings | Sign only the dedicated account webhook endpoint; use a distinct Connect endpoint secret | Organization secret manager mapped to the bounded webhook variable; never source a live environment | `scripts/cx release stripe-check` | `P1-STRIPE-TEST` webhook half |
| Stripe test Connect configuration | Stripe Dashboard test-mode Connect settings and disposable test connected accounts | Test accounts, transfers, payout simulation, restrictions, and account status only | Organization secret manager and `.env.go-closure` variable names documented in `docs/STRIPE_SANDBOX_SETUP.md` | `scripts/cx release stripe-matrix` | `P1-STRIPE-TEST` |
| Real alert receiver credential | Approved paging provider/on-call administrator | Post only to the private-canary service/route with firing, acknowledgement, and resolution visibility | Paging-provider secret manager referenced by staging Alertmanager | `scripts/cx release alert-page --real-receiver-env CX_ALERT_RECEIVER_<APPROVED_NAME>` | `P1-ALERT-DELIVERY` |
| Qualified governance approvals | Named security, privacy, legal, licensing, payments, operations, supplier-policy, and release authorities | Review and approve or reject the exact clean commit and bounded test-only canary scope, including qualified human tabletop evidence | Signed approval bundle in the restricted governance evidence store; pass only its path | `scripts/cx release approvals-check --bundle <secure-bundle-path>` | `P1-INDEPENDENT-APPROVAL` and `P1-GOVERNANCE` |
| Additional physical Metal devices | Authorized supplier-operations owner | One receipt per distinct approved Apple Silicon device; synthetic data and private-canary workloads only | Device enrollment credential in the device keychain; characterization receipts in the restricted evidence store | `make agent-characterize` on each device, then the supervised canary command | Physical-device portion of `P1-RECOVERY-SOAK` and `P1-CANARY-REHEARSAL` |

The current decision remains:

```text
Level A software candidate: GO
Level B supervised Stripe-test-mode canary: NO-GO
Level C live money/public launch: prohibited
```
