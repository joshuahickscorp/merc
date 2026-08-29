# Release readiness: scope-separated NO-GO

As of 2026-08-17 (`python3 scripts/validate-readiness.py` at HEAD
`9e31c65b`), the software candidate is Level A **GO**. The supervised
Stripe-test-mode private canary is still Level B **NO-GO** (87/100, threshold
95, P0=0, P1=5). Backend alpha is an additional axis, not a replacement:
**85/91**, `ALPHA_ENGINEERING_READY NO_GO`, `EXTERNAL_ALPHA_PROVEN NO_GO`.
The only open `ALPHA_BLOCKER` P1 is `P1-STRIPE-TEST`. Live money and public
access are separately **NO-GO and prohibited**.

| Level | Decision | Boundary |
|---|---|---|
| A — software candidate | GO | exact-head CI, artifacts, signed registry images, and fresh-clone proof |
| backend alpha | NO-GO | persistent backend, Stripe test-mode money path, local execution/recovery; see `docs/BACKEND_ALPHA_CONTRACT.md` |
| B — private canary | NO-GO | persistent private staging, approved synthetic participants, Stripe test mode, no value |
| C — live pilot/public launch | NO-GO / prohibited | no real charges, transfers, payouts, public signup, or independent suppliers |

The machine-derived readiness score is **87/100**. Local receipts score 84;
the independent offsite backup/restore pair
(`evidence/external/offsite-backup-verification.json` and
`evidence/external/offsite-independent-restore.json`, both `PASS` / `BOUND`)
adds the extra 3. Remaining 13 points are Stripe sandbox matrix (6),
24-hour qualifying soak (3), external staging-attack rehearsal (1),
qualified privacy approval (1), licensing approval (1), and staffed abuse
route (1). They cannot be earned by writing more local code. GO requires at
least 95, zero open P0/P1, all mandatory scenarios, and a passing 24-hour
soak. Recompute with `python3 scripts/validate-readiness.py`. The decision
ledger is `ops/go-no-go.json`; the advisory domain ledger is
`ops/readiness.json` (hand-typed `earned` is ignored). The operator
checklist for the remaining 13 points is
`docs/PROGRAMME.md § "Facet external action pack"`.

## What is proven

- Go format, vet, unit/integration/race tests and schema apply-twice pass.
- Rust format and strict clippy pass. The historical "75 tests pass" count is
  stale (the agent tree now has 175 `#[test]` / `#[tokio::test]` attributes);
  this pass did not re-run `cargo test`.
- Two distinct local Metal agents completed `embed` and `batch_infer` through
  Candle. A late/wrong-attempt commit was rejected without a money effect; the
  ledger remained zero-sum with no duplicate task effects.
- Exact model/tokenizer revisions, byte sizes, SHA-256 values, agent source,
  runtime authority, tuning, and hardware class are bound into admission.
- The 126-route authorization matrix covers eight identity roles with default
  deny. Eighteen routes are explicit public inventory (`public_read` 14,
  `public_bootstrap` 4); the other 108 reject anonymous and wrong-namespace
  credentials before storage access. The readiness pin is 126.
- All privileged mutations are actor-bound and audit-atomic. Disputes freeze
  settlement, and intake, dispatch, payment, and webhook stops are durable.
- Static validators pass for the nine-service immutable staging harness, local
  age backup envelope, 26 alert rules, 14 dashboard panels, and website WCAG AA
  contrast (minimum 6.06:1). Isolated restore, local custom-CA TLS, six-fault
  rollback/forward recovery, a seeded nine-component restart storm, scoped
  DSAR/tombstone replay, technical incident scenarios, and 4,096 generated
  deterministic payment sequences also pass. The longest retained soak receipt is
  `evidence/autonomous/local-soak-300s.json` (300 s; unbound historical soak, not
  a bound qualifying soak); the only other local file is `local-soak-60s.json`,
  which carries a single sample. A derived backend-alpha soak of 3600 s on
  `mercmerc.net` is `PASS` at
  `evidence/external/qualifying-soak-alpha.json` (`qualifies_for_24h_gate:
  false`). No 15-minute, two-hour or 24-hour qualifying soak receipt exists,
  and `.artifacts/local-soak-failures/` records the 15-minute attempts that
  ended in an OOM kill and in a control restart. The Level B/C 24-hour soak
  is therefore NOT EXECUTED. Payment evidence is labelled SIMULATED.
- Stripe test mode has been exercised, but not in a form that closes its gate.
  The formal path `evidence/external/stripe-sandbox-matrix.json` exists:
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
  `status: BLOCKED`, `provider_mode: test`, `live_mode: PROHIBITED`,
  `blocker.id: connect_platform_not_signed_up` on `acct_1TxbzMCwPLrR4vaY`.
  `stripe_sandbox_matrix_proven` therefore awards 0/6. An earlier unbound
  canary file `evidence/canary/stripe-cad-supplier-matrix-0a4b66cb.json`
  (`status: PASS`, test-mode provider objects) still does not close the
  gate. Corrected 2026-08-09: this bullet previously read "Stripe test mode
  remains NOT EXECUTED". Corrected 2026-08-17: the wall is Connect signup,
  not a missing formal path.
- Fourteen **agent** review domains contain the required scope, failure model,
  findings, severity, evidence, repair, verification, and residual risk fields in
  `ops/agent-review-notes.json`. Corrected 2026-08-09: this bullet previously
  described them as "independent review domains" and pointed at
  `ops/independent-reviews.json`, a path that does not exist. The artifact it
  was renamed to states its own limits — `artifact_kind: agent_review_notes`,
  `independence_claim: NOT_CLAIMED`, `method: parallel-agent source and receipt
  inspection` — and `scripts/validate-independent-reviews.py` says the same in
  its header. Independent human review has **not** happened. Section 19 forbids
  an LLM inventing external approval, and calling agent inspection "independent
  review" in the release-readiness document is that, whatever the underlying
  artifact says about itself: a later reader closes the gate on the summary, not
  on the JSON.

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

Five P1 gates remain (`ops/go-no-go.json` `open_p1`). Three others are in
`dropped_p1` as `SATISFIED` on evidence: `P1-STAGING` (public TLS at
`mercmerc.net`, `/readyz` 200, `payment_mode=test`), `P1-RECOVERY-SOAK`
(rollback/forward + restart-storm + 3600 s derived soak; the 24-hour clause
of this same P1 stays the Level B/C bar), and `P1-OFFSITE-RESTORE`
(independent R2 download and isolated restore from the live droplet
volumes).

Still open:

- `P1-STRIPE-TEST` (`ALPHA_BLOCKER`) — non-Connect Stripe test-mode scenarios
  have been driven; Connect signup is the remaining wall
  (`evidence/external/stripe-sandbox-matrix.json` status `BLOCKED`,
(unbound receipt, status BLOCKED — a test-mode snapshot cited as subject, not authority; it does not prove the Connect half of the money path)
  `blocker.id=connect_platform_not_signed_up`).
- `P1-CANARY-REHEARSAL` (`ALPHA_CONTROL`) — two approved buyers, two
  operator-controlled Metal agents, and the counted scenario matrix. Local
  L12 receipts PASS and must not be read as `EXTERNAL_ALPHA_PROVEN`.
- `P1-ALERT-DELIVERY` (`PUBLIC_LAUNCH`) — a real staffed paging receiver.
- `P1-INDEPENDENT-APPROVAL` (`PUBLIC_LAUNCH`) — a named non-author reviewer.
- `P1-GOVERNANCE` (`PUBLIC_LAUNCH`) — eight qualified approvals. Governance
  documents are drafts marked *DRAFT · INTERNAL · NOT LEGAL ADVICE*.

Local technical versions of the incident and privacy exercises pass but do
not constitute those approvals. `EXTERNAL_ALPHA_PROVEN` is false. Live money
stays `NO_GO_PROHIBITED`.

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
