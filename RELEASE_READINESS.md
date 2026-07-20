# Release readiness: scope-separated NO-GO

As of 2026-07-20, the hardened software candidate passes its full local
two-agent proof, but the requested supervised Stripe-test-mode private canary is
still **NO-GO**. Live money and public access are separately **NO-GO and
prohibited**.

| Level | Decision | Boundary |
|---|---|---|
| A — software candidate | GO locally, pending exact-head remote publication proof | build/test artifacts only |
| B — private canary | NO-GO | persistent private staging, approved synthetic participants, Stripe test mode, no value |
| C — live pilot/public launch | NO-GO / prohibited | no real charges, transfers, payouts, public signup, or independent suppliers |

The weighted readiness score is **65/100**. GO requires at least 95, zero open
P0/P1, all mandatory scenarios, and a passing 24-hour soak. The machine decision
and scoring evidence are in `ops/go-no-go.json` and `ops/readiness.json`.

## What is proven

- Go format, vet, unit/integration/race tests and schema apply-twice pass.
- Rust format and strict clippy pass; all 72 tests pass.
- Two distinct local Metal agents completed `embed` and `batch_infer` through
  Candle. A late/wrong-attempt commit was rejected without a money effect; the
  ledger remained zero-sum with no duplicate task effects.
- Exact model/tokenizer revisions, byte sizes, SHA-256 values, agent source,
  runtime authority, tuning, and hardware class are bound into admission.
- The 70-route authorization matrix covers eight identity roles with default
  deny; all 56 credential-protected routes reject anonymous and wrong-namespace
  credentials before storage access.
- All privileged mutations are actor-bound and audit-atomic. Disputes freeze
  settlement, and intake, dispatch, payment, and webhook stops are durable.
- Static validators pass for the nine-service immutable staging harness, local
  age backup envelope, 23 alert rules, 13 dashboard panels, and website WCAG AA
  contrast (minimum 6.06:1).
- Fourteen independent review domains contain the required scope, failure model,
  findings, severity, evidence, repair, verification, and residual risk fields in
  `ops/independent-reviews.json`.

The latest complete precommit proof used a unique disposable Compose project
and volumes, so it could not replay prior jobs or idempotency keys. It is bound
to source SHA-256
`05627d75e28fe07815f7026c5e81093e329be168cc99b701fe96971c7cac5eab`
and ledger SHA-256
`249215b4dd7822ef0ee5f5e7b8b21dc14ebbb54e8f591c9a5fc18ffcbad9e824`.
The source remained unchanged through the run. Because the source fingerprint
also binds the Git commit ID, the authoritative clean committed-candidate proof
is generated after commit and reported as an external exact-HEAD receipt; it
cannot be embedded inside the commit it identifies without self-reference.

## Why Level B is still NO-GO

Ten P1 gates remain. They require resources or authority not available in this
workspace: exact-head remote publication/fresh-clone proof; persistent TLS
staging; rollback, restart storm and 24-hour soak; independently uploaded and
restored encrypted backup; Stripe test-mode fixtures and reconciliation; a real
alert receiver; two approved buyers/two operator-controlled Metal agents and
scenario adapters; an independent repository reviewer; qualified governance
approvals; and the named incident/privacy/provenance exercises.

The workstation contains a Stripe **live-mode** credential. It was classified
`live_refused`, never printed, and never used. It cannot satisfy any gate.

## Single exact operator request

`ops/go-closure-inputs.json` is the sole machine-readable request. It specifies
each missing name, accepted form, least scope, private destination, verification
command, and expected receipt. Supply values only in the gitignored
`.env.go-closure`; supply the approval JSON outside git and set only
`GOVERNANCE_APPROVAL_BUNDLE_PATH`. Run:

```sh
make release-doctor
```

The doctor prints only booleans and credential classes. It returns success only
when staging, backup, Stripe test mode, alerting, participants, independent
review, and governance are all ready.

No RC tag may be created and the GO-closure PR must remain draft/unmerged until
every Level B P1 is closed on the exact candidate.
