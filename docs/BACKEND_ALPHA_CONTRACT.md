# Backend alpha contract

This document is the authority for what the backend alpha is, what it
requires, and what it does not require. Later readers should cite this
file, not a release-process habit, when they ask whether a gate applies.

The readiness model already has three levels:

| Level | Meaning | Decision today |
|---|---|---|
| A | software candidate, buildable and locally proven | GO |
| B | private canary (persistent staging, Stripe test mode, no value) | NO_GO |
| C | live money, independent suppliers, or public launch | NO_GO_PROHIBITED |

The thing being shipped is none of those. It is a **backend alpha**.
Because it had no level of its own it inherited every Level B and
Level C requirement by default. That inheritance is the defect this
contract closes.

This contract **adds** a level. It does not replace, weaken, or rescore
Levels A, B, or C. A reader who asks "what is the Level B score" must
still get the same full-bar number the existing validator computes
(today: 84/100, eight open P1s, Level B NO_GO). Backend-alpha
classifications move an obligation to the level that owns it; they
never dissolve one.

Machine authority: `ops/readiness.json` (level + claims),
`ops/backend-alpha-gates.json` (classification of every gate),
`ops/go-no-go.json` (decisions), `ops/scripts/validate-readiness.py`
(derivation). This prose is the citable meaning of those records.

---

## 1. Purpose

The backend alpha exists to exercise, with tightly controlled
participants, the real paths that make merc a marketplace rather than
a demo:

- execution (a job is admitted, placed, run, and completed)
- marketplace (quote, claim, verification, settlement)
- supplier (an agent enrolls, heartbeats, claims, and is paid in
  test-mode ledger terms)
- buyer (a buyer is approved, funds in test mode, submits, and
  retrieves a result)
- verification (an independent check can accept or reject work)
- settlement (the ledger, Stripe test-mode objects, refunds,
  disputes, and holds agree)
- deployment (the exact candidate digest runs on a persistent
  control + data plane)
- recovery (rollback, restart, backup, and restore have been done
  against that plane)

It is backend-first. The product surface is terminal-native and
incomplete. No website is required. No public consumer launch is
authorized. No broad anonymous population is admitted. No unsupported
legal or compliance claim is made. Live money remains prohibited.

Participants are named, allowlisted, and few. The first engineering
pass may use operator-controlled devices and synthetic identities.
That pass must never be reported as proof that an independent
external buyer and supplier have used the system. See §5.

---

## 2. What the alpha requires

These are required before claiming `ALPHA_ENGINEERING_READY`. Each
item is a reachable harm on this alpha, not a ceremony.

1. **A persistent backend.** The exact candidate digest on a durable
   control plane and data plane, probed for health, source identity,
   auth, storage, and lifecycle. A laptop compose that dies when the
   lid closes is not the alpha.
2. **Authorization that defaults to deny.** Every route classified;
   anonymous and cross-tenant callers refused. An allowlisted
   participant does not make an authz hole safe.
3. **A money path that has spoken to Stripe test mode.** The
   deterministic simulator is not Stripe. Settlement is in the
   purpose of this alpha, so the test-mode matrix (auth, capture,
   refund, dispute, Connect hold/release/failure, webhook replay,
   provider reconciliation) is required before the first controlled
   money-path transaction. Live mode stays prohibited.
4. **Execution, supplier, buyer, and verification that actually
   run.** Hardware characterized; sandbox and admission policy in
   force; restart-storm and attempt fencing proven. The counted
   synthetic rehearsal (`P1-CANARY-REHEARSAL`) is how the operator
   learns those paths work. It is not `EXTERNAL_ALPHA_PROVEN`.
5. **Recovery that has left the box.** Local encrypted backup +
   isolated logical restore already prove the tool. A single-box
   wipe of the droplet (or its MinIO/Postgres) is the most likely
   disaster of this alpha. Ciphertext must exist on an independent
   provider/credential and must have been downloaded and restored
   from that copy. Recovery is in the purpose; an unproven restore
   path is an unproven restore path when it matters.
6. **Deploy recovery.** Rollback to the retained prior digest and
   forward again, plus a seeded restart-storm, on the persistent
   plane.
7. **A soak whose duration is derived from a mechanism, not a
   calendar.** See §6. The Level B 24-hour qualifying soak remains
   the Level B bar in full. Backend alpha requires a shorter soak
   derived from live process mechanisms that only elapsed wall time
   can observe.
8. **A working local alert fire → sink → resolve path.** The
   operator supervises this alpha in person. A staffed 24/7 paging
   vendor is not required; a silent fire is.
9. **Technical privacy machinery.** DSAR export, deletion, and
   restore-time tombstone replay. Controlled participants have
   emails and may have Stripe test-mode customer objects. Qualified
   counsel approval is a later legal program, not this machinery.
10. **Signed, attested, digest-pinned images and a SPDX SBOM.**
    You must know what binary ran.
11. **Containment that stays in force.** No live money. No public
    signup. No independently owned supplier hardware without the
    P0 expansion criteria (contract, location, egress, attestation).
    No payout export.

`ALPHA_ENGINEERING_READY` is the claim that the list above is true
and the operator may begin the alpha. It is not a claim that anyone
outside the operator has used the system.

---

## 3. What the alpha does not require

Each exclusion names the later level that owns the obligation. The
obligation remains; it is not deleted.

| Not required for backend alpha | Why it is not a reachable harm here | Owns the obligation |
|---|---|---|
| Public website | The product surface is terminal-native. No website is shipped or required. A WCAG/browser gate cannot fire against a site that is not the alpha. | `PUBLIC_LAUNCH` (Level C public launch). Level B may still score the existing website receipt on the full 100-point bar. |
| Polished GUI | Incomplete UI is in the definition of this alpha. A buyer who cannot click through a marketing site is not a harm if they are not using a marketing site. | `PUBLIC_LAUNCH` |
| Complete TUI | The versioned BUY/EARN/HEALTH composition surface is explicitly not a TUI. Completing one is a product milestone after the backend works. | `POST_ALPHA` (product surface after backend alpha; still before public launch) |
| Mass-market onboarding | No broad anonymous population. Participants are named and few. | `PUBLIC_LAUNCH` |
| Public documentation site | No public consumer launch, no marketing launch. Operator-facing runbooks already exist in-tree. | `PUBLIC_LAUNCH` |
| Production-scale legal program (eight qualified approvals, published terms, counsel-signed privacy/licensing packs) | No public counterparty, no live charges, no public distribution of hashes, no mass PII. Technical DSAR and the in-repo SBOM/provenance register remain required (§2). | `PUBLIC_LAUNCH` (Level C). Enterprise-scale policy programs sit at `ENTERPRISE`. |
| Enterprise governance (SOC-style, two-person every change, board pack) | This is one operator supervising a backend. | `ENTERPRISE` |
| Large external fleet | Independent uncontrolled supplier hardware is **prohibited** (P0 containment). Expansion is a later decision with its own exit criteria. | `POST_ALPHA` / Level C, after P0 expansion |
| Staffed 24/7 operations organization and vendor paging (PagerDuty/Opsgenie acknowledgement) | The operator supervises this alpha in person. The reachable harm is "a fire happens and nobody sees it." The local Alertmanager → HTTP sink fire/resolve receipt is the control that protects that harm. Unattended 03:00 coverage is a public-launch / live-money problem. | `PUBLIC_LAUNCH` (staffed route); `ENTERPRISE` (an operations organization) |
| Broad public signup / self-serve admission | Explicitly out of scope. Opening signup is a Level C decision. | `PUBLIC_LAUNCH` / Level C |
| Marketing launch | Not a backend property. | `PUBLIC_LAUNCH` |
| Named non-author repository approval of a closure PR that authorizes irreversible third-party harm | Live money and public launch stay `NO_GO_PROHIBITED`. Machine receipts, not a human reviewer, are what the validator scores. Self-merging `ALPHA_ENGINEERING_READY` cannot charge the public or enroll independent suppliers. The two-person exact-head approval remains the Level C bar in full. | `PUBLIC_LAUNCH` / Level C |
| Staffed multi-role abuse desk / qualified human T&S tabletop | No public signup. Participants are known. The operator is the abuse route. Kill switch, revocation, and the technical tabletops protect the reachable abuse (a known participant sends something they should not). A staffed T&S org is what you need when strangers can arrive. | `PUBLIC_LAUNCH` |
| Qualified privacy counsel approval and external subprocessor deletion exercise | Technical DSAR/deletion/tombstone is required. A counsel signature and a subprocessor-deletion legal exercise are the public-population program. | `PUBLIC_LAUNCH` |
| Qualified licensing / asset-provenance approval | SBOM, OIDC signature, attestation, and digest-pinned images are required. A named licensing authority approving public distribution is what you need when hashes and weights ship to strangers. | `PUBLIC_LAUNCH` |
| External hostile rehearsal against a public TLS hostname by a named outside reviewer | This alpha does not require a public website or an advertised hostname. The same authz harm is protected by the default-deny matrix and the local security tabletop. The named-reviewer public-hostname rehearsal is the Level B private-canary bar (that canary *does* advertise a TLS hostname). | `POST_ALPHA` (Level B private canary) |
| Qualifying 24-hour soak as an arbitrary duration | See §6. The 24-hour receipt and the 24-hour clause of `P1-RECOVERY-SOAK` remain the Level B/C bar in full. Backend alpha uses a duration derived from live mechanisms. | `POST_ALPHA` (Level B private canary endurance) |

Nothing in that table is a license to skip a control whose harm this
alpha can actually reach. If a later reader wants to drop an
`ALPHA_BLOCKER`, they need a new reachable-harm argument, not a
citation to "it is inconvenient."

---

## 4. Two readiness claims

The model holds two claims. They must never be collapsed.

### `ALPHA_ENGINEERING_READY`

The backend is ready to **begin** the alpha. Synthetic or
operator-controlled participants are permitted as the first
exercise. This claim is blocked by every gate classified
`ALPHA_BLOCKER` and by any breach of containment (live money, public
signup, independent-supplier expansion without the P0 exit
criteria).

This claim may be `GO` while no external human has ever used the
system.

### `EXTERNAL_ALPHA_PROVEN`

A controlled **external** buyer and a controlled **external**
supplier have actually used the deployed backend. "Controlled"
means invite-only, allowlisted, and (for a supplier) contracted /
location-verified — not "the operator typed both parts."

This claim is `NO_GO` until
`evidence/external/external-alpha-participants.json` passes the
checker in `ops/scripts/validate-readiness.py`. That checker makes it
structurally impossible to count a synthetic, disposable, harness,
operator-owned, or operator-controlled identity as an independent
external participant. Completing `P1-CANARY-REHEARSAL` (two
synthetic buyers, two operator Metal workers) must not, and cannot,
flip this claim.

`P0-INDEPENDENT-SUPPLIER` stays in force as a general prohibition
(do not enroll a random GPU). The claim becomes `GO` only when the
named pair's receipt carries the P0 expansion fields (contract,
location, destination-pinned egress, attestation) and the
identities survive the synthetic-refusal checker. A synthetic
canary rehearsal cannot produce that receipt.

---

## 5. Synthetic versus external — structural rule

The following identities are synthetic or operator-side. They may
appear in an engineering rehearsal. They **must not** appear in the
`EXTERNAL_ALPHA_PROVEN` receipt at all. If they do, the checker
returns false and the claim stays `NO_GO`.

- `participant_class` or `identity_kind` in `{synthetic,
  operator_synthetic, operator_controlled, operator_owned, harness,
  test, test_fixture, fixture, disposable, local_simulator,
  simulator, canary_synthetic}`
- `synthetic: true`, `controlled_by_operator: true`,
  `operator_owned: true`
- emails matching synthetic / canary-bot / `test+` /
  `example.com|org|net` / `invalid`
- nil or demo UUIDs (`00000000-0000-…`)
- a supplier `device_id` / `worker_id` listed in the receipt's
  `operator_controlled_device_ids`
- buyer and supplier sharing an id, email, or organization
- missing attestations `not_synthetic`,
  `independent_of_operator`,
  `not_operator_employee_acting_as_fixture`

A passing receipt needs both roles, each with
`participant_class: "independent_external"`, distinct identities,
and those attestations. There is no "treat the operator's second
laptop as the external supplier" override.

---

## 6. Soak re-derivation

The Level B/C qualifying soak is 86 400 seconds because a release
process said 24 hours. The question this contract answers is
different: **what mechanism is 24 hours the only way to observe?**

Examined against the running control plane:

| Mechanism | Duration | Only observable by waiting that long? |
|---|---|---|
| Inflight coalescing lease (`inflightLeaseTTL`) | 30 s | No. Tests expire the row. Live process: minutes. |
| Worker liveness (`last_seen_at` 45–90 s windows) | ≤ 90 s | No. Canary scenario `stale_lease_recovery` plus a short live soak. |
| Verification lease (`verificationLeaseDurationDefault`) | 5 min | No. Tests exist. Live process: one renewal cycle. |
| pgx pool `MaxConnLifetime` | **30 min** (`src/control/main.go`) | **Yes, in the live process.** Unit tests do not recycle the production pool. |
| pgx pool `MaxConnIdleTime` | 5 min | Same class as above; shorter. |
| Charge collect tick | 60 s | No. Many ticks in a one-hour soak. |
| Charge retry step / cap | 30 min / 6 h | Observable by inserting aged attempts; not a general soak. |
| `chargeBatchMaxAge` | 24 h | No. `BuyersDueForBatch` is tested with aged rows. |
| `executionEnvelopeMaxTTL` | 24 h **maximum**; minimum 30 s | No. Expiry is observable at the 30 s floor. |
| `minimumPayoutHold` | 24 h | Live payouts are prohibited. The floor is unit-tested. Stripe test-mode hold/release is the `P1-STRIPE-TEST` matrix. |
| `sessionTTL` | 30 days | Not 24 h. |
| `merc-backup.timer` `OnCalendar=03:15` | ≤ 24 h to the next fire | No. `systemctl start merc-backup.service` / `ops/scripts/backup.sh` fires it. |
| ACME certificate refresh | ~60 days | Not 24 h. Not required (no website). |
| Token / key purge | 30 days | Not 24 h. |
| Log rotation | no 24 h-only control found | — |

The soak assertions themselves (`two_agents_continuously_present`,
`no_page_alerts`, `no_webhook_dead_letters`,
`no_control_restarts_or_recreates`, `no_stuck_terminal_jobs`,
`bounded_resource_growth`) are hold-still checks. None of them
names a 24-hour-only mechanism.

**Backend-alpha soak.** Minimum **3 600 seconds (1 hour)**: twice
`MaxConnLifetime` so a live pool recycle is observed with samples
on both sides, which also covers many verification-lease renewals
and many worker-liveness windows. Receipt path:
`evidence/external/qualifying-soak-alpha.json`. Classification:
`ALPHA_BLOCKER`. This does not replace the 24-hour receipt.

**Level B/C soak.** `evidence/external/qualifying-soak-24h.json`
and the 24-hour clause of `P1-RECOVERY-SOAK` stay in full at those
levels (`POST_ALPHA` from this alpha's point of view). Nothing is
deleted.

---

## 7. Classification vocabulary

Every gate in the readiness model, and every open P1, carries one
of:

| Class | Meaning |
|---|---|
| `ALPHA_BLOCKER` | The named harm is reachable on this alpha and the control is required before the first controlled alpha transaction / before `ALPHA_ENGINEERING_READY`. |
| `ALPHA_CONTROL` | The named harm is reachable and the control applies during this alpha, but it is not a start-gate (or it is the smaller substitute that protects the same harm). |
| `POST_ALPHA` | Required after this alpha, typically by the Level B private canary, before live money or public launch. |
| `PUBLIC_LAUNCH` | Required for a public / consumer / live-money launch (Level C). Kept in full at that level. |
| `ENTERPRISE` | Required for an enterprise governance / staffed-org posture beyond a public launch. |
| `OBSOLETE` | The obligation no longer exists in any form. Unused in this calibration; prefer moving an obligation to a later class over declaring it dead. |

The five answers recorded on every gate (data, not prose) are:

1. What specific harm does this prevent?
2. Can that harm occur in this alpha?
3. Is it necessary before the first controlled alpha transaction?
4. Is there a smaller control that protects the same harm?
5. Is it actually a later production / public-launch requirement?

A gate may not be rescoped because a release process said so,
because it is best practice, because legal might care, or because
we will eventually need it. Name the harm, then say whether this
alpha can reach it.

---

## 8. Relationship to Levels A, B, and C

- Level A remains GO on the software candidate.
- Level B remains the private canary, scored on the **full
  100-point bar**. Missing `evidence/external/*` receipts still
  cost the same 16 points. All eight P1s still block Level B.
  `python3 ops/scripts/validate-readiness.py` must still print that
  84/100 number.
- Level C remains `NO_GO_PROHIBITED`. Live money and public access
  stay false.
- Backend alpha is an additional decision axis. Its score is
  computed only from receipts classified `ALPHA_BLOCKER` or
  `ALPHA_CONTROL`. Its GO/NO_GO is computed only from
  `ALPHA_BLOCKER` gates plus the two claims in §4.

A previous calibration scoped some receipts to "Level C only" so
that Level B would look closer to GO. That is a different question
from the one this contract answers, and it is not applied here.

---

## 9. How to cite this

- "Is a website required for the thing we are shipping?" → No.
  §3, owned by `PUBLIC_LAUNCH`.
- "Must we have Stripe test-mode before the first money-path
  transaction?" → Yes. §2.3, `ALPHA_BLOCKER`.
- "Does a passing synthetic canary prove external use?" → No. §4–5.
- "Why is the alpha soak one hour, not 24?" → §6, `MaxConnLifetime`.
- "Did we delete the 24-hour soak?" → No. It remains the Level B/C
  bar. §6, §8.
