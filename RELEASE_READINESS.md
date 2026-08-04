# Release readiness: scope-separated NO-GO

As of 2026-07-20, the hardened software candidate passes its full local
two-agent proof, but the requested supervised Stripe-test-mode private canary is
still **NO-GO**. Live money and public access are separately **NO-GO and
prohibited**.

| Level | Decision | Boundary |
|---|---|---|
| A — software candidate | GO | exact-head CI, artifacts, signed registry images, and fresh-clone proof |
| B — private canary | NO-GO | persistent private staging, approved synthetic participants, Stripe test mode, no value |
| C — live pilot/public launch | NO-GO / prohibited | no real charges, transfers, payouts, public signup, or independent suppliers |

The machine-derived readiness score is **84/100**. That figure is the
**machine-reachable ceiling** on a host with no persistent staging, no
independent offsite storage, and no human approvers: every remaining point is
reserved for external credentials, hosts, soak time, or qualified approvals, and
cannot be earned by writing more local code. It is not underachievement. GO
requires at least 95, zero open P0/P1, all mandatory scenarios, and a passing
24-hour soak. Recompute with `python3 scripts/validate-readiness.py`. The
decision ledger is `ops/go-no-go.json`; the advisory domain ledger is
`ops/readiness.json` (hand-typed `earned` is ignored). The operator checklist
for the remaining 16 points is `docs/FACET_EXTERNAL_ACTION_PACK.md`.

## What is proven

- Go format, vet, unit/integration/race tests and schema apply-twice pass.
- Rust format and strict clippy pass; all 75 tests pass.
- Two distinct local Metal agents completed `embed` and `batch_infer` through
  Candle. A late/wrong-attempt commit was rejected without a money effect; the
  ledger remained zero-sum with no duplicate task effects.
- Exact model/tokenizer revisions, byte sizes, SHA-256 values, agent source,
  runtime authority, tuning, and hardware class are bound into admission.
- The 76-route authorization matrix covers eight identity roles with default
  deny; all 61 credential-protected routes reject anonymous and wrong-namespace
  credentials before storage access.
- All privileged mutations are actor-bound and audit-atomic. Disputes freeze
  settlement, and intake, dispatch, payment, and webhook stops are durable.
- Static validators pass for the nine-service immutable staging harness, local
  age backup envelope, 26 alert rules, 14 dashboard panels, and website WCAG AA
  contrast (minimum 6.06:1). Isolated restore, local custom-CA TLS, six-fault
  rollback/forward recovery, a seeded nine-component restart storm, scoped
  DSAR/tombstone replay, technical incident scenarios, and 4,096 generated
  deterministic payment sequences also pass. The longest retained soak receipt is
  `evidence/autonomous/local-soak-300s.json` (300 s; unbound historical soak, not
  a bound qualifying soak); the only other is `local-soak-60s.json`, which
  carries a single sample. No 15-minute, two-hour or 24-hour soak receipt exists,
  and `.artifacts/local-soak-failures/` records the 15-minute attempts that ended
  in an OOM kill and in a control restart. Soak duration is therefore NOT
  EXECUTED beyond 300 s. Payment evidence is labelled SIMULATED; Stripe test mode
  remains NOT EXECUTED.
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

Eight P1 gates remain. They require resources or authority not available in this
workspace: persistent TLS staging and published-image external
rollback/restart/24-hour soak (no local soak window beyond 300 s has produced a
passing receipt);
independently uploaded and restored encrypted backup; Stripe test-mode fixtures
and reconciliation; a real alert receiver; two approved buyers/two
operator-controlled Metal agents and scenario adapters; an independent
repository reviewer; qualified governance approvals; and qualified human
incident/provenance closure. Local technical versions of the incident and
privacy exercises pass but do not constitute those approvals.

Exact-head run `29711174514` passed all five CI jobs at `7502d1d`; all four
artifact bundles and their embedded checksums verified. Registry run
`29711173217` published and verified candidate digest
`sha256:e6f8e7e6208119454567f174241e34efea89d499a85d9612af976ccbc0578e8f`
and prior digest
`sha256:9a2cf1aa8366bf9f476816ad5288851fea48322296a64c0af82a774b9dccccd2`
with SPDX SBOMs, GitHub-OIDC signatures, and attestations. A clean fresh clone
passed `make ci` and the full isolated two-agent proof; its ledger SHA-256 is
`3d5331bc56f042212dbc61427b73237b9bb8eac3228354d4b6ea1dc38fb4f23f`.
Receipt-ledger-only successor commits use the PR current-head check rollup as
the normative exact-head remote receipt.

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
