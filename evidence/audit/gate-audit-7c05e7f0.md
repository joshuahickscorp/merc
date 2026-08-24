# Lane C — backend-alpha gate audit

Audit of `ops/backend-alpha-gates.json` against `scripts/validate-readiness.py`
and the receipts under `evidence/`. Gates were not moved. Checkers were not
edited. This file is the only write.

Operator rule applied independently of the gate file's own flags:

- protects a reachable backend-alpha harm → KEEP
- protects a harm only reachable at a later release level → belongs there
- stale or mis-scoped → checker defect
- never lower the bar for convenience, never fake evidence, never preserve
  bureaucracy merely because it already exists

Reachability test used for every row: *can this harm occur during a single
controlled alpha transaction between one known buyer and one known supplier?*
That is the backend-alpha contract (`docs/BACKEND_ALPHA_CONTRACT.md` §1–3),
not Level B private-canary and not public launch.

HEAD at audit: `7c05e7f01fc29db497bee78220f608d1aa4f7746`
(`alpha: Accounts v1 support enabled; connected-account creation PASSES`).

---

## Verify: live-tree scorer

Working tree is a sparse checkout. `ops/` is not materialized. The command
the contract requires, run in this worktree:

```
$ python3 scripts/validate-readiness.py
readiness: FAIL: cannot load readiness ledgers: [Errno 2] No such file or directory: '/Users/scammermike/.claude-grok/worktrees/C-gate-audit-20260823-225703/ops/readiness.json'
```

That is the live-tree output. It does not score anything. `git restore
--source=HEAD -- ops/...` also failed (`pathspec did not match any file(s)
known to git`) because the sparse cone excludes `ops/`. `git sparse-checkout
add` is forbidden in this sandbox.

Scorer results below were produced by importing the live
`scripts/validate-readiness.py`, pointing `ROOT` at a `/tmp` tree whose
`ops/` files were `git show HEAD:ops/...` of this same commit, with
`evidence/` and `scripts/` symlinked from the worktree. Checkers therefore
ran against the live receipts and HEAD gate/matrix/ledger bytes. Reconstructed
`main()` printed:

```
readiness: PASS (88/100 derived, P0=0, P1=5, Level B NO_GO)
  level_b: 88/100 derived (threshold 95/100), P0=0, P1=5, decision NO_GO
  backend_alpha: 88/94 derived, ALPHA_BLOCKER_P1=1, ALPHA_ENGINEERING_READY NO_GO, EXTERNAL_ALPHA_PROVEN NO_GO
  backend_alpha open ALPHA_BLOCKER P1: P1-STRIPE-TEST
  backend_alpha open ALPHA_BLOCKER receipts/soaks: receipt:money_and_reconciliation:evidence/external/stripe-sandbox-matrix.json
  public_launch open named_reviewer:staging-attack-rehearsal: reviewer.name/organization unmet (requirement kept; not an alpha point)
  source_and_ci: derived=10/10
  security: derived=15/15
  money_and_reconciliation: derived=9/15
    - evidence/external/stripe-sandbox-matrix.json: CHECK_FAILED → 0/6 (status=BLOCKED; Connect-complete PASS required (transfer tr_ + payouts))
  lifecycle_and_concurrency: derived=10/10
  artifacts_and_storage: derived=8/8
  agent_and_sandbox: derived=8/8
  database_and_recovery: derived=8/8
  deployment_and_rollback: derived=5/8
    - evidence/external/qualifying-soak-24h.json: CHECK_FAILED → 0/3
  observability_and_alerting: derived=6/6
  privacy_and_data_governance: derived=3/4
    - evidence/external/privacy-qualified-approval.json: MISSING → 0/1
  licensing_and_supply_chain: derived=2/3
    - evidence/external/licensing-provenance-approval.json: MISSING → 0/1
  abuse_and_trust: derived=1/2
    - evidence/external/staffed-abuse-route-or-tabletop.json: MISSING → 0/1
  support_and_incident_response: derived=1/1
  website_and_buyer_usability: derived=2/2
```

Alpha possible is 94 because four DOMAIN_RECEIPTS rows are classified out of
`ALPHA_SCORED` (`POST_ALPHA` 24h soak 3 + three `PUBLIC_LAUNCH` 1-pointers).
The 6 missing alpha points are exactly Stripe Connect-complete. That 6-point
hole is honest. Most of the 88 that *are* awarded are not (see UNEARNED).

`scripts/validate-readiness.py` never reads `binding_status` or
`producer_identity` (`rg binding_status scripts/validate-readiness.py` is
empty). `scripts/validate-evidence-binding.py` exists and is a Makefile
target; it is not an input to this scorer. That is the mechanism that lets
the past earn present-tense readiness.

---

## How scoring works (quoted)

Alpha points come only from DOMAIN_RECEIPTS rows classified `ALPHA_BLOCKER`
or `ALPHA_CONTROL`:

```
# scripts/validate-readiness.py:1610-1635
def derive_backend_alpha_score(...):
    """Score only ALPHA_BLOCKER and ALPHA_CONTROL receipt rows."""
    ...
            if record["classification"] not in ALPHA_SCORED:
                continue
            ...
            if receipt_passes(relative, checker):
                earned += points
```

`receipt_passes` is existence + checker(doc). No HEAD pin, no producer pin,
no provider-account pin:

```
# scripts/validate-readiness.py:1600-1607
def receipt_passes(relative: str, checker: Callable[[Any], bool]) -> bool:
    path = ROOT / relative
    if not path.is_file():
        return False
    doc: Any = load_json(relative) if relative.endswith(".json") else True
    if relative.endswith(".json") and doc is None:
        return False
    return bool(checker(doc))
```

The weakest checkers are `status_in("PASS")`:

```
# scripts/validate-readiness.py:85-94
def status_in(*allowed: str) -> Callable[[Any], bool]:
    ...
        status = str(doc.get("status", "")).upper()
        return status in allowed_set
```

Wired that way, among others:

```
# scripts/validate-readiness.py:1663-1664
("evidence/autonomous/registry-verification.json", status_in("PASS"), 4),
("evidence/autonomous/supply-chain.json", status_in("PASS"), 3),
```

Dropped P1s are not re-derived. `main()` only requires every known P1 id to
appear in `open_p1` or `dropped_p1` (`scripts/validate-readiness.py:1894-1910`).
`status: SATISFIED` plus an old `candidate_commit` is enough.

---

## Verdict rows

Every gate in `ops/backend-alpha-gates.json` (45). Points are the DOMAIN_RECEIPTS
award (n/a for P1/P0/soak/claim/named_reviewer, which do not add to 94).
"Reachable?" is the known-buyer/known-supplier transaction test, not the gate
file's own boolean.

| id | class | pts | today | verdict | harm (one sentence) | reachable? | smaller sufficient control |
|---|---|---|---|---|---|---|---|
| `receipt:source_and_ci:evidence/autonomous/registry-verification.json` | ALPHA_BLOCKER | 4 | 4/4 | CHECKER-DEFECT | Running an unsigned or unverified registry image, so the binary on the persistent plane is not the candidate the operator thinks it is. | yes | Signature + SPDX + digest pull of the image `/version` on the persistent plane reports, pinned to HEAD. `status: PASS` on a July image is not that. |
| `receipt:source_and_ci:evidence/autonomous/supply-chain.json` | ALPHA_BLOCKER | 3 | 3/3 | CHECKER-DEFECT | Shipping an image with no SBOM, signature, or attestation, so a compromise or license-contaminated binary cannot be reconstructed or refused. | yes | Same as the registry row; this is the same UNBOUND July-20 artifact scored a second time. |
| `receipt:source_and_ci:ops/authorization-matrix.json` | ALPHA_BLOCKER | 3 | 3/3 | KEEP | An unclassified route silently defaults to an unreviewed allow. | yes | none — the 126-route default-deny matrix at HEAD is the control, and `auth_matrix_complete` matches `control/api.go` 126/126 today. |
| `receipt:security:ops/authorization-matrix.json` | ALPHA_BLOCKER | 8 | 8/8 | KEEP | Cross-tenant read/write, anonymous submit, or revoked credentials still accepted. | yes | none — same HEAD matrix, security-weighted. |
| `receipt:security:evidence/autonomous/technical-exercises.json` | ALPHA_BLOCKER | 6 | 6/6 | KEEP | Emergency access that is unaudited, or that cannot recover from operator lockout. | yes | none for the tabletop itself. Checker inspects `break_glass.status` (not status-only) but does not pin HEAD; flag as unbound, not reclassify. |
| `receipt:security:evidence/external/staging-attack-rehearsal.json` | ALPHA_BLOCKER | 1 | 1/1 | KEEP | A hostile internet user finds an authz or cross-tenant hole on an advertised public TLS hostname. | yes | Matrix + local tabletop do not drive `https://mercmerc.net`. Because that hostname is advertised, the executed public-hostname rehearsal stays an alpha start-gate. Named human review is a different gate. |
| `named_reviewer:staging-attack-rehearsal` | PUBLIC_LAUNCH | n/a | unmet | RECLASSIFY-POST_ALPHA | A machine rehearsal is accepted as the public-surface bar because nobody is accountable for having looked. | no | The executed rehearsal (attacks_executed, per-class results, hostname) remains the alpha control. Named review is the Level B private-canary bar in the contract, not Level C. |
| `receipt:money_and_reconciliation:evidence/autonomous/payment-simulator.json` | ALPHA_BLOCKER | 9 | 9/9 | KEEP | Double capture, dropped refund, non-idempotent webhook, or ledger/provider disagreement in money-path *logic*. | yes | none as a logic bar. It is not Stripe; the 6-point matrix is. Do not drop the simulator. Do pin it to HEAD (it is UNBOUND). |
| `receipt:money_and_reconciliation:evidence/external/stripe-sandbox-matrix.json` | ALPHA_BLOCKER | 6 | 0/6 | KEEP | The first Stripe test-mode authorization, capture, refund, dispute, Connect hold, or webhook replay disagrees with the ledger. | yes | none. The simulator is not Stripe. Connect-complete PASS (`tr_` + payout hold/release/failure/reversal) is the right bar. Today's BLOCKED receipt is an honest zero. Do not lower it because Accounts v1 creation now PASSES. |
| `receipt:lifecycle_and_concurrency:evidence/autonomous/technical-exercises.json` | ALPHA_BLOCKER | 5 | 5/5 | KEEP | Lost attempt fencing or unaudited emergency mutation of in-flight jobs. | yes | none. Same technical-exercises break-glass block as the security 6; unbound to HEAD. |
| `receipt:lifecycle_and_concurrency:evidence/autonomous/local-restart-storm.json` | ALPHA_BLOCKER | 5 | 5/5 | CHECKER-DEFECT | A process restart loses in-flight jobs or duplicates financial effects. | yes | Seeded restart-storm of the *deployed* image, pinned to HEAD. `status_in("PASS")` on a July local image is not that. Persistent-plane proof lives in `evidence/external/staging-restart-storm.json`, which this scorer never opens. |
| `receipt:artifacts_and_storage:evidence/autonomous/logical-independent-restore.json` | ALPHA_BLOCKER | 6 | 6/6 | CHECKER-DEFECT | Cannot decrypt and restore the artifact envelope after a local failure. | yes | Checker must inspect `integrity` (ciphertext, sentinels, zero-sum) and pin the producer commit. `status_in("PASS")` would accept a two-key file. |
| `receipt:artifacts_and_storage:evidence/external/offsite-backup-verification.json` | ALPHA_BLOCKER | 2 | 2/2 | KEEP | A single-provider or single-credential wipe destroys the only copy of the ledger and object ciphertext. | yes | Local restore proves the tool, not that bytes left the box. The offsite checker is content-rich (s3 URI, independent download, checksums, encrypted-before-upload). Still not HEAD-pinned. |
| `receipt:agent_and_sandbox:evidence/autonomous/hardware-characterization.json` | ALPHA_BLOCKER | 8 | 8/8 | CHECKER-DEFECT | Admitting work onto hardware that cannot run it, or that produces uncharacterized output. | yes | Characterization of the device that will actually run the known supplier's job, with `runtime_authority_sha256` and source_commit checked. `status_in("PASS")` on an UNBOUND receipt is not that. |
| `receipt:database_and_recovery:evidence/autonomous/logical-independent-restore.json` | ALPHA_BLOCKER | 4 | 4/4 | CHECKER-DEFECT | Cannot restore the ledger database after local failure. | yes | Same receipt and same `status_in("PASS")` defect as the artifacts 6. |
| `receipt:database_and_recovery:evidence/autonomous/local-rollback.json` | ALPHA_BLOCKER | 3 | 3/3 | CHECKER-DEFECT | A bad candidate cannot be rolled back; the plane is stuck on a broken digest. | yes | Rollback/forward of the exact HEAD digest against the persistent data plane (`evidence/external/head-rebuild-redeploy.json` / `staging-rollback-and-forward.json`). Those files are not this checker. |
| `receipt:database_and_recovery:evidence/external/offsite-independent-restore.json` | ALPHA_BLOCKER | 1 | 1/1 | KEEP | The bytes that left the box are truncated, wrong-key, or unrestorable, discovered only after the primary is gone. | yes | Checksumming without restore is false comfort. The restore checker is content-rich (isolated decrypt, new credentials, integrity). Still not HEAD-pinned. |
| `receipt:deployment_and_rollback:evidence/autonomous/staging-validation.json` | ALPHA_BLOCKER | 2 | 2/2 | CHECKER-DEFECT | The compose/image/scaffold cannot actually boot the candidate, so "deployed" is a paper claim. | yes | A boot probe of the pinned image. The receipt says `"deployment_evidence": "NOT EXECUTED"` and the checker only requires `status: PASS`. |
| `receipt:deployment_and_rollback:evidence/autonomous/local-rollback.json` | ALPHA_BLOCKER | 2 | 2/2 | CHECKER-DEFECT | Cannot swap the control image back to the retained prior digest without losing the data plane. | yes | Same July local-rollback receipt as the database 3, status-only. |
| `receipt:deployment_and_rollback:evidence/autonomous/local-restart-storm.json` | ALPHA_BLOCKER | 1 | 1/1 | CHECKER-DEFECT | A multi-component restart on the deployed shape loses work or duplicates money. | yes | Same July local-restart-storm receipt as the lifecycle 5, status-only. |
| `receipt:deployment_and_rollback:evidence/autonomous/local-soak-60s.json` | ALPHA_CONTROL | 0 | 0/0 | KEEP | A process that dies or OOMs in the first minute is treated as a viable alpha host. | yes | The 3600s derived soak is the start-gate. This 0-point witness is correctly ALPHA_CONTROL. `soak_clean` at least inspects restart/oom, not merely status. |
| `receipt:deployment_and_rollback:evidence/external/qualifying-soak-24h.json` | POST_ALPHA | 3 | 0/3 | KEEP | A slow leak or once-per-day timer missed by a short soak, *as an arbitrary 24-hour calendar bar*. | no | 3600s derived from `MaxConnLifetime=30m` (still `control/main.go:340`). Keep the 24h receipt as the Level B bar. Do not award it on the HTTPS observer that currently occupies this path. |
| `receipt:deployment_and_rollback:evidence/autonomous/soak-requirement-derivation.json` | ALPHA_CONTROL | 0 | 0/0 | KEEP | An endurance requirement asserted as a duration nobody derived. | yes | The 0-point derivation row is the right shape. It must not (and does not) substitute for 24h. |
| `receipt:observability_and_alerting:evidence/autonomous/alert-pipeline-simulation.json` | ALPHA_BLOCKER | 3 | 3/3 | CHECKER-DEFECT | A real failure mode never fires an alert, so the supervising operator cannot see it. | yes | The live fire/resolve receipt (`alert-delivery-r1`) is the smaller live control. A harness loopback with `"real_receiver_delivery": "NOT EXECUTED"` plus `status_in("PASS")` does not protect a silent fire on the droplet. |
| `receipt:observability_and_alerting:evidence/autonomous/alert-page-simulation.json` | ALPHA_CONTROL | 2 | 2/2 | CHECKER-DEFECT | The paging path is miswired, so a firing alert never becomes a human-visible page. | yes | Same harness loopback as the pipeline row (`profile: page`). The live delivery receipt is the control the gate file already names. |
| `receipt:observability_and_alerting:evidence/autonomous/alert-delivery-r1.json` | ALPHA_BLOCKER | 1 | 1/1 | KEEP | An alert fires and is never observed at a sink. | yes | This *is* the smaller control that replaces staffed 24/7 paging for a supervised alpha. Checker is content-rich. `host.docker.internal` is what `scripts/test-alert-delivery.sh` writes; that matches "operator supervises in person." Still not HEAD-pinned. |
| `receipt:privacy_and_data_governance:evidence/autonomous/technical-exercises.json` | ALPHA_BLOCKER | 3 | 3/3 | KEEP | Cannot export or delete a known participant's data, or a restore undeletes a tombstone. | yes | none. `technical_privacy` inspects dsar/deletion/tombstone. Unbound to HEAD. |
| `receipt:privacy_and_data_governance:evidence/external/privacy-qualified-approval.json` | PUBLIC_LAUNCH | 1 | 0/1 | KEEP | Processing a public population's personal data without a qualified privacy authority. | no | Technical DSAR/deletion/tombstone (already scored). No public signup. |
| `receipt:licensing_and_supply_chain:evidence/autonomous/supply-chain.json` | ALPHA_BLOCKER | 2 | 2/2 | CHECKER-DEFECT | Running an unsigned or unattested image whose contents cannot be named. | yes | Same UNBOUND July supply-chain receipt as the source_and_ci 3. Third scoring of one historical image. |
| `receipt:licensing_and_supply_chain:evidence/external/licensing-provenance-approval.json` | PUBLIC_LAUNCH | 1 | 0/1 | KEEP | Publishing hashes, weights, or assets to strangers without a named licensing authority. | no | In-repo SBOM and digest-pinned images. This alpha does not distribute to strangers. |
| `receipt:abuse_and_trust:evidence/autonomous/technical-exercises.json` | ALPHA_CONTROL | 1 | 1/1 | KEEP | The operator does not know how to pause intake, revoke a token, or handle a known participant misbehaving. | yes | none at this level. Staffed T&S is the later gate. Unbound to HEAD. |
| `receipt:abuse_and_trust:evidence/external/staffed-abuse-route-or-tabletop.json` | PUBLIC_LAUNCH | 1 | 0/1 | KEEP | Strangers arrive via public signup and abuse the marketplace with no staffed human route. | no | Operator-as-route, kill switch, revocation, technical tabletops. |
| `receipt:support_and_incident_response:evidence/autonomous/technical-exercises.json` | ALPHA_CONTROL | 1 | 1/1 | KEEP | A settlement/display mismatch is "fixed" off-ledger because the operator has never walked the support path. | yes | none. Technical tabletop is the reachable control. Unbound to HEAD. |
| `receipt:website_and_buyer_usability:evidence/autonomous/website-validation.json` | ALPHA_BLOCKER | 2 | 2/2 | RECLASSIFY-PUBLIC_LAUNCH | A public buyer cannot complete the website flow, or the site leaks credentials, or it fails accessibility — against a shipped consumer website. | no | Terminal-native buyer/supplier APIs plus the public-hostname *attack* rehearsal (credential leak / authz). WCAG/browser against a consumer site is PUBLIC_LAUNCH. Level B may keep the 2 points on the 100-point bar. |
| `soak:alpha-derived` | ALPHA_BLOCKER | 0 | pass (clears blocker) | CHECKER-DEFECT | The live process never survives a pgx pool recycle (`MaxConnLifetime=30m`). | yes | ≥3600s soak of the *HEAD* digest on the persistent plane, with expected_commit == HEAD. Today's receipt soaked `a5bca8c0`, not `7c05e7f0`. Zero points, but it clears the ALPHA_BLOCKER open list. |
| `p1:P1-STAGING` | ALPHA_BLOCKER | n/a | dropped SATISFIED | CHECKER-DEFECT | Calling the backend ready when no persistent control+data plane exists. | yes | Health/auth/storage/lifecycle probes against the persistent plane, re-derived every score, pinned to HEAD. `dropped_p1` `SATISFIED` at `a5bca8c0` is not that. |
| `p1:P1-RECOVERY-SOAK` | ALPHA_BLOCKER | n/a | dropped SATISFIED | CHECKER-DEFECT | A bad digest cannot be rolled back, a restart-storm loses work, or the live process has never been watched across a pool-recycle window. | yes | Re-derive rollback + restart-storm + 3600s soak of HEAD. The 24h clause stays POST_ALPHA. Dropped SATISFIED at `a5bca8c0` is a paper close. |
| `p1:P1-OFFSITE-RESTORE` | ALPHA_BLOCKER | n/a | dropped SATISFIED | KEEP | Irrecoverable loss of the experiment's ledger and objects after a one-box wipe. | yes | The two offsite receipts *are* content-checked in DOMAIN_RECEIPTS and currently pass. Disagree with prior Level-C-only scoping: one droplet wipe is reachable. |
| `p1:P1-STRIPE-TEST` | ALPHA_BLOCKER | n/a | open | KEEP | The first test-mode charge, refund, dispute, Connect hold, or webhook disagrees with the ledger. | yes | none. Open for the right reason. Connect-complete remains the exit. |
| `p1:P1-ALERT-DELIVERY` | PUBLIC_LAUNCH | n/a | open | KEEP | A production incident fires at 03:00 and never pages a staffed human on a vendor acknowledgement route. | no | Observed Alertmanager → HTTP sink fire/resolve (`alert-delivery-r1`), already passing. Operator supervises this alpha in person. |
| `p1:P1-CANARY-REHEARSAL` | ALPHA_CONTROL | n/a | open | KEEP | Buyer/supplier/execution/verification/settlement paths have never been driven end to end. | yes (during, not before) | none. This is how the alpha begins, not a paper precondition of beginning. Completing it must not flip EXTERNAL_ALPHA_PROVEN (and the l11 receipts declare `does_not_satisfy=EXTERNAL_ALPHA_PROVEN`). |
| `p1:P1-INDEPENDENT-APPROVAL` | PUBLIC_LAUNCH | n/a | open | KEEP | The author self-approves a closure PR that authorizes irreversible third-party harm. | no | Machine receipts. Live money and public launch stay NO_GO_PROHIBITED regardless of who merges. |
| `p1:P1-GOVERNANCE` | PUBLIC_LAUNCH | n/a | open | KEEP | Operating a public or live-money service without the eight qualified approvals plus named human coverage. | no | Technical DSAR, tabletops, operator supervision, containment. |
| `p0:P0-INDEPENDENT-SUPPLIER` | ALPHA_BLOCKER | n/a | out_of_scope (prohibition in force) | KEEP | Independently owned supplier hardware receives and retains buyer input without contract, location, egress, confidentiality, or attestation. | yes | The prohibition itself. Operator-controlled devices are permitted for ALPHA_ENGINEERING_READY. Expansion exit stays Level C / required before EXTERNAL_ALPHA_PROVEN. |
| `claim:EXTERNAL_ALPHA_PROVEN` | ALPHA_CONTROL | n/a | NO_GO (receipt missing) | KEEP | Reporting that an independent external buyer and supplier have used the system when only synthetics or the operator's devices have. | yes (as a false claim) | none. The checker structurally refuses synthetics. P1-CANARY-REHEARSAL cannot produce a passing document. Keep this cruel. |

Counts: 45 rows. KEEP 27. RECLASSIFY 2. CHECKER-DEFECT 16.

---

## 1. STALE CHECKERS

Assertions that name a field, path, schema version, or producer that is not
what occupies that path at HEAD. Proven by quote vs on-disk/HEAD reality.

### 1.1 `qualifying_24h_soak_proven` vs the file at its wired path

Checker assertion (`scripts/validate-readiness.py:511-516`):

```
if doc.get("schema_version") != 2:
    return False
if str(doc.get("kind", "")) != "go_closure_soak":
    return False
```

It further requires `control_image` matching `_IMMUTABLE_IMAGE`,
`runtime.container_id` as 64 hex, `assertions.two_agents_continuously_present`,
`samples.sha256`, and a subprocess of `scripts/validate-go-closure-soak-receipt.py`.

On-disk occupant of `evidence/external/qualifying-soak-24h.json` (HEAD producer
`scripts/soak/soak24.py`, `KIND = "qualifying_24h_https_observer"`,
`SCHEMA_VERSION = 1`):

```
"schema_version": 1,
"kind": "qualifying_24h_https_observer",
"status": "IN_PROGRESS",
"expected_commit": "9e31c65b27860d659d7ce972e2de7052691c0642",
"duration": { "requested_seconds": 86400, "elapsed_seconds": 182, ... }
```

No `control_image`, no `runtime.container_id`, no `samples.sha256`.
`soak24.py` *refuses* to write `status=PASS` or `qualifies_for_24h_gate=true`.

The producer that *does* emit `schema_version: 2, kind: go_closure_soak` is
`scripts/go-closure-soak.sh`, and it writes `GC_EVIDENCE_FILE` under
`evidence/go-closure/${timestamp}-${kind}.json` (`scripts/lib/go-closure-common.sh`),
a directory that is not in DOMAIN_RECEIPTS and is not at HEAD.

This currently fails closed (0/3, POST_ALPHA, not in the 94). It is still a
stale path/schema/producer wiring: the checker names a schema the wired path
no longer produces, and the producer that still emits that schema no longer
writes to the wired path.

### 1.2 Advisory ledger still names a 110-route world

Not a DOMAIN_RECEIPTS checker, but it is what `ops/readiness.json` claims
while `auth_matrix_complete` pins 126:

```
# ops/readiness.json census_at_latest_full_proof
"routes": 110,
...
# ops/readiness.json local_evidence.authorization
"authorization": "PASS: 110 routes x 8 roles, default deny; ..."
```

HEAD reality: `ops/authorization-matrix.json` has 126 routes;
`control/api.go` registers 126; the two sets are equal (0 in api-not-matrix,
0 in matrix-not-api). The tripwire at 126 is *not* stale. The hand-typed
census is.

### 1.3 What is not stale (checked, not assumed)

- Stripe `payload_api_version` substring `"2025-06-30"` still matches
  `const stripeAPIVersion = "2025-06-30.basil"` in
  `control/stripe_api_contract.go`.
- `MaxConnLifetime = 30 * time.Minute` still at `control/main.go:340`.
- `auth_matrix_complete` `routes == 126` matches HEAD `api.go` exactly.
  The comment that it *went* stale at 118 and again at 123 is history, not
  a current miss.

---

## 2. UNBOUND-SATISFIABLE CHECKERS

These are the dangerous ones: a historical artifact not tied to the current
commit / producer / provider account can satisfy them. `validate-readiness.py`
does not read `binding_status` at all.

HEAD = `7c05e7f01fc29db497bee78220f608d1aa4f7746`.

### Structural: every `status_in("PASS")` row

A two-key document `{"status":"PASS"}` is enough. Wired today on:

| receipt | pts | receipt identity (not checked) |
|---|---|---|
| `evidence/autonomous/registry-verification.json` | 4 | `binding_status: UNBOUND`; `honesty.does_not_describe_running_plane: true`; candidate `a4d50d93` / `sha256:f848a804…` (2026-07-20) |
| `evidence/autonomous/supply-chain.json` | 3+2 | `binding_status: UNBOUND`; `workflow_commit: a4d50d93` (same July image) |
| `evidence/autonomous/local-restart-storm.json` | 5+1 | `binding_status: UNBOUND`; `source_commit: a4d50d93`; local image `sha256:bb6cb052…` |
| `evidence/autonomous/logical-independent-restore.json` | 6+4 | `binding_status: BOUND` to `73bc49da`, not HEAD; checker ignores identity and `integrity` |
| `evidence/autonomous/hardware-characterization.json` | 8 | `binding_status: UNBOUND`; no source_commit |
| `evidence/autonomous/local-rollback.json` | 3+2 | `binding_status: UNBOUND`; candidate `a4d50d93` |
| `evidence/autonomous/staging-validation.json` | 2 | `binding_status: UNBOUND`; `"deployment_evidence": "NOT EXECUTED"` |
| `evidence/autonomous/alert-pipeline-simulation.json` | 3 | `binding_status: UNBOUND`; `"receiver": "harness-controlled loopback receiver"` |
| `evidence/autonomous/alert-page-simulation.json` | 2 | same harness document, `profile: page` |
| `evidence/autonomous/website-validation.json` | 2 | `binding_status: UNBOUND`; see UNEARNED |

### Content checkers that still do not pin HEAD / account / plane

| checker | pts today | what it checks | what it does not |
|---|---|---|---|
| `technical_break_glass` / `technical_privacy` / `technical_tabletops` | 6+5+3+1+1 | subsection PASS flags | producer `87a81857` (2026-07-19), not HEAD |
| `payment_simulated` | 9 | `"SIMULATED"` in status or label | HEAD; money-path code has moved (Accounts v1 at HEAD) |
| `external_staging_attack_proven` | 1 | hostname, HTTPS, executed classes | producer `7fe48960`, not HEAD; 112 routes driven vs 126 now |
| `independent_offsite_backup_proven` | 2 | s3 URI, checksums, policy | producer `d8d2e65a`; backup `20260817T011802Z`; not HEAD |
| `external_offsite_restore_proven` | 1 | isolated decrypt + integrity | same backup id / commit as the backup row |
| `alert_delivery_proven` | 1 | fingerprints, firing/resolved | producer `9e31c65b`; `url_host` is `http://host.docker.internal`; `sink_log` is another worktree (`…/F-alert-delivery-20260817-112925/.artifacts/…`). `_LOCAL_HOST_TOKEN` includes `docker.internal` but is **not applied** here (only `"harness"` / `"example"`). |
| `soak_clean` | 0 | restart_max==0 and oom==0 | `source_commit: a4d50d93` |
| `soak_derivation_recorded` | 0 | kind/status/mechanisms/conclusion string | producer `73bc49da` |
| `qualifying_alpha_soak_proven` | 0 pts, **clears ALPHA_BLOCKER** | duration ≥3600, not 24h, restart/oom | `expected_commit` is never compared to HEAD. Receipt soaked `a5bca8c0` on mercmerc.net. |
| `stripe_sandbox_matrix_proven` | 0 today | Connect-complete objects, `2025-06-30` | no source_commit; an old PASS from another Stripe account would score. Today it fails, so it is not earning. |
| `auth_matrix_complete` | 3+8 | 126 + default deny | not unbound in the historical-receipt sense: it scores the file at HEAD. It does not probe the running server. |

### Dropped P1s (honor-system SATISFIED)

`go-no-go.json` `dropped_p1`:

```
P1-STAGING        SATISFIED  candidate_commit a5bca8c0  2026-08-16T23:58:47Z
P1-RECOVERY-SOAK  SATISFIED  candidate_commit a5bca8c0  2026-08-17T00:58:47Z
P1-OFFSITE-RESTORE SATISFIED candidate_commit d8d2e65a  2026-08-17T01:18:42Z
```

The scorer does not reopen those receipts. Latest plane observations in-tree
are not HEAD either: `staging-alpha-readiness.json` deployed `9e31c65b`;
`head-rebuild-redeploy.json` candidate `0ffbd52d`. Neither is
`7c05e7f0`. A historical SATISFIED row is enough to keep these P1s off the
open list.

### What is actually bound to present-tense HEAD

`ops/authorization-matrix.json` via `auth_matrix_complete` (11 points).
That is the only ALPHA_SCORED award whose artifact is the current commit's
file and whose checker looks at that file's content rather than a historical
`status` field.

---

## 3. UNEARNED POINTS TODAY

Rows that currently score (`receipt_passes` true) without having earned the
named harm against HEAD / the running plane / the live provider account.

Alpha derived 88/94. Of those 88:

| pts | row | why unearned today |
|---|---|---|
| 4 | registry-verification | Receipt *says so*: `"does_not_describe_running_plane": true`, candidate `a4d50d93` / `f848a804…`. Later `/version` observations listed in `honesty` are `a5bca8c0`, `19fe0b23`, `9e31c65b` — "None of these are f848a804 / a4d50d93." Checker awards 4 anyway. |
| 3 | supply-chain (source_and_ci) | Same July image, `binding_status: UNBOUND`. |
| 2 | supply-chain (licensing) | Third award of the same file. |
| 8 | hardware-characterization | UNBOUND, no source_commit, `status_in("PASS")`. |
| 9 | payment-simulator | UNBOUND; `generated_sequences.count=4096` against seed 20260719. Makefile even wants `binding_status == "BOUND"`; the file is UNBOUND. Accounts v1 landed at HEAD after this receipt. |
| 6 | technical-exercises break_glass (security) | Producer `87a81857` (2026-07-19). |
| 5 | technical-exercises break_glass (lifecycle) | Same file, second award. |
| 5 | local-restart-storm (lifecycle) | Local `a4d50d93` image, not the droplet. |
| 1 | local-restart-storm (deployment) | Same file, second award. |
| 6 | logical-independent-restore (artifacts) | Producer `73bc49da`; checker does not read integrity. |
| 4 | logical-independent-restore (database) | Same file, second award. |
| 3 | local-rollback (database) | `a4d50d93` local native image; limitation text: amd64 published candidate "is not claimed on this arm64 host". |
| 2 | local-rollback (deployment) | Same file, second award. |
| 2 | staging-validation | `"deployment_evidence": "NOT EXECUTED"` while the harm is boot. |
| 2 | offsite-backup-verification | Content-true for backup `20260817T011802Z` at `d8d2e65a`, not HEAD. |
| 1 | offsite-independent-restore | Same backup. |
| 1 | staging-attack-rehearsal | Drove mercmerc.net at `7fe48960` for 30s on 2026-08-17, not HEAD; 112 routes vs 126 now. |
| 3 | alert-pipeline-simulation | Harness loopback, `real_receiver_delivery: NOT EXECUTED`. |
| 2 | alert-page-simulation | Duplicate harness document. |
| 1 | alert-delivery-r1 | Content-true for `9e31c65b` + `host.docker.internal`; sink_log is another worktree. |
| 3 | technical-exercises privacy | Same July-19 file as break-glass. |
| 1 | technical-exercises abuse tabletops | Same file. |
| 1 | technical-exercises support tabletops | Same file. |
| 2 | website-validation | Receipt *says so* (quoted below). |

**Earned today (present-tense):** 11 points, both from `ops/authorization-matrix.json`
(`auth_matrix_complete`, 3+8), which matches HEAD `control/api.go` 126/126
default deny.

**Honest zeros (not unearned):** Stripe matrix 0/6 BLOCKED; 24h soak 0/3
POST_ALPHA; three PUBLIC_LAUNCH receipts missing 0/1 each.

**Zero-point blocker falsely cleared:** `soak:alpha-derived` is 0 points but
`qualifying_alpha_soak_proven` returns true on a 3600s soak of `a5bca8c0`,
so `backend_alpha_blocker_receipts_open` does not list it. HEAD was not
soaked. That is unearned *containment*, not unearned points.

Website quote (the cleanest single exhibit):

```
# evidence/autonomous/website-validation.json
"status": "PASS_AUTOMATED_BROWSER",
"target": {
  "kind": "loopback_static_server",
  "url_not_tested": "https://mercmerc.net/buyer"
},
"honesty": {
  "record_class": "historical_loopback_run",
  "event_date": "2026-07-20",
  "public_hostname_touched": false,
  "does_not_describe": "the public /buyer workspace on mercmerc.net, HEAD, or any later site build"
}
```

Checker (`DOMAIN_RECEIPTS` website row):

```
("evidence/autonomous/website-validation.json", status_in("PASS", "PASS_AUTOMATED_BROWSER"), 2),
```

That is 2 points for a document that names the URL it did not test.

---

## 4. GENUINE RECLASSIFICATION CANDIDATES

Argued from the harm model and the backend-alpha contract. Not from
difficulty. Not a proposal to delete the obligation.

### 4.1 `receipt:website_and_buyer_usability:evidence/autonomous/website-validation.json`

**Current:** ALPHA_BLOCKER, 2 points, `necessary_before_first_controlled_alpha_transaction: true`.

**Candidate:** PUBLIC_LAUNCH (Level C public/consumer launch). Leave the
row on the Level B 100-point bar.

**Harm named by the gate:** "A public buyer cannot complete the website
flow, or the site leaks credentials, or it fails accessibility — against a
shipped consumer website."

**Why that harm is not reachable on this alpha:**

- Contract §1: "It is backend-first. The product surface is terminal-native
  and incomplete. No website is required."
- Contract §3 table, row "Public website": "A WCAG/browser gate cannot fire
  against a site that is not the alpha." Owner: `PUBLIC_LAUNCH`. "Level B
  may still score the existing website receipt on the full 100-point bar."
- Contract §9: "Is a website required for the thing we are shipping? → No.
  §3, owned by PUBLIC_LAUNCH."
- `ops/readiness.json` `level_backend_alpha.website_required: false`.
- The known buyer and known supplier transact via allowlisted APIs
  (jobs, quote, worker poll/commit, Stripe test-mode). A WCAG miss on a
  marketing/buyer HTML workspace is not that transaction.

**Why this is not a convenience cut:** the public-TLS *authz* harm on
`mercmerc.net` is real because the hostname is advertised. That harm is
already owned by `staging-attack-rehearsal` (KEEP as ALPHA_BLOCKER). Moving
the *website usability/WCAG* row does not move the attack rehearsal.

**Checker note (not the reclass argument):** even if the row stayed
ALPHA_BLOCKER, today's 2 points are unearned (loopback, `url_not_tested`).
Fixing the checker without reclassifying would require a live-hostname
browser pass — which the contract says is not an alpha start-gate.

If accepted, alpha possible becomes 92 and today's earned becomes 86, all
else equal.

### 4.2 `named_reviewer:staging-attack-rehearsal`

**Current:** PUBLIC_LAUNCH, `owned_by_level: level_c_live_money`.

**Candidate:** POST_ALPHA (Level B private canary).

**Harm named by the gate:** a machine rehearsal accepted as the
public-surface bar because nobody looked.

**Why it is not an alpha start-gate (already true in both files):**
participants are operator-controlled, no public signup, no live money;
the machine checker already refuses a paper pass that did not drive the
hostname.

**Why PUBLIC_LAUNCH is the wrong later bucket:** contract §3 table, row
"External hostile rehearsal against a public TLS hostname by a named
outside reviewer" assigns that obligation to **POST_ALPHA (Level B
private canary)** because "that canary *does* advertise a TLS hostname."
The gate file put the named reviewer at Level C instead. That is a
later-level mis-bucket, not an alpha dispute.

**What must not move:** the executed machine rehearsal
(`receipt:security:evidence/external/staging-attack-rehearsal.json`)
stays ALPHA_BLOCKER. The hostname *is* advertised (`https://mercmerc.net`
serves `/buyer`, `/supplier`, `/admin`, `/prices` over public TLS).
Contract §3's "this alpha does not require an advertised hostname" is
overtaken by the empirical surface; contract §3 also says "Nothing in
that table is a license to skip a control whose harm this alpha can
actually reach." Hostile internet users can already hit the box during
the known-pair transaction. KEEP the machine row.

### 4.3 Explicit non-candidates (harm model says stay)

These were examined and are **not** reclassified:

| id | why KEEP |
|---|---|
| Stripe matrix 6 + P1-STRIPE-TEST | Settlement is in the purpose. Simulator is not Stripe. Connect-complete (`tr_` + payouts) stays. Accounts v1 connected-account *creation* PASSing at HEAD does not mint a `tr_` or payout hold/release/failure/reversal. Today's remainder still FAILED on capabilities and `connect=null` webhook. |
| Offsite backup 2 + restore 1 + P1-OFFSITE-RESTORE | Prior art scoped these to Level C because synthetics can be re-seeded. Recovery is in this alpha's purpose; one droplet wipe is reachable. Stay ALPHA_BLOCKER. |
| 24h soak | Stay POST_ALPHA. `MaxConnLifetime` is still 30m; 3600s is the alpha observation. Do not delete 24h; do not award it on the HTTPS observer. |
| P0-INDEPENDENT-SUPPLIER | Containment. The harm is reachable the moment an uncontrolled GPU is enrolled. |
| claim:EXTERNAL_ALPHA_PROVEN | False-claim harm is reachable. Checker must stay cruel. |
| staging-attack machine rehearsal | Advertised hostname (see 4.2). |
| Auth matrix 3+8 | Unclassified route on a public IP is reachable for a known pair (and for strangers already). |
| Privacy technical 3, licensing SBOM 2 (the *classification*), tabletops, alert-delivery, payment simulator | Reachable harms, later-level *approvals* already split out. |

---

## Stripe remainder (honest hole, do not touch the bar)

`evidence/external/stripe-sandbox-matrix.json`:

```
"status": "BLOCKED",
"provider_mode": "test",
"live_mode": "PROHIBITED",
"payment_objects": { ..., "transfer": false, ... }
"harness": { "stripe_matrix": "BLOCKED-ON-CONNECT", "connect_test_account_present": false, ... }
```

`connect_gated_remainder` at HEAD: `connected_account_creation` PASS
(`acct_1U7npECeWJZCwOUN`, type=custom country=CA); `transfer_to_connected_account`
FAILED (`http=400` destination needs transfers/crypto_transfers/legacy_payments);
payout hold/release/failure FAILED; connect-true webhook delivery FAILED
(`connect=None`).

`stripe_sandbox_matrix_failure_reason` output used by the scorer:

```
status=BLOCKED; Connect-complete PASS required (transfer tr_ + payouts)
```

That is a real unmet ALPHA_BLOCKER, not a checker defect. Recent commits
(`9d064ea9`, `7c05e7f0`) moved connected-account *creation*. They did not
land a `tr_` or a Connect webhook with `connect=true`. Lowering
`stripe_sandbox_matrix_proven` to accept creation-without-transfer would
fake the money path this alpha exists to exercise.

---

## Summary for the supervising session

- **Do not lower Stripe.** 0/6 is honest. P1-STRIPE-TEST stays open.
- **Do not treat 88/94 as present-tense.** 11/94 is the only award whose
  artifact is HEAD (`authorization-matrix.json`). The rest of the 88 is
  historical `status: PASS` (or a content-rich receipt of another commit).
- **Two reclassifications, both later-level, neither a deletion:** website
  ALPHA_BLOCKER → PUBLIC_LAUNCH; named_reviewer PUBLIC_LAUNCH → POST_ALPHA.
- **The recurring checker defect:** `status_in("PASS")` plus no
  `binding_status` / HEAD pin. `validate-evidence-binding.py` already
  exists and is unused by this scorer. Wiring it, or requiring
  `producer_identity.source_commit.value == HEAD` on ALPHA_BLOCKER
  receipts, is the control that stops the past from earning the present.
- **Dropped P1 SATISFIED is the same bug at P1 grain.** P1-STAGING and
  P1-RECOVERY-SOAK closed on `a5bca8c0`. HEAD is `7c05e7f0`.
)
