# Backend Alpha — Freeze Record

The backend-alpha readiness program is **frozen** at the state below. This is an
immutable reference point, not a finish line: readiness stops being the project
here, and development continues past it (see the ascension campaign).

## The frozen state

| | |
|---|---|
| **Candidate commit** | `c0c9e3fce0fd8d376575d4770de517aba9c42816` |
| **Signed image** | `ghcr.io/joshuahickscorp/computexchange-control@sha256:ba4e047ffb22ae5dc6314955fdba948cfdaffebabea66d5de07662161a81898e` |
| **Deployment** | `mercmerc.net`, `readyz: ready`, `payment_mode: test`, `live_value_movement: false` |
| **Checker result** | `PASS (83/100 derived, P0=0, P1=4, Level B NO_GO)` — exit 0 |
| **Backend alpha** | **81/92** |
| **Open ALPHA_BLOCKER P1** | **0** |
| **Code drift** | none since the candidate |
| **Frozen** | 2026-08-24 |

Reproduce with `python3 scripts/validate-readiness.py` at the candidate commit.

## The number is 81/92, not 92/92

The remaining 11 points are `local-restart-storm` (5+1) and `local-rollback`
(3+2). They are **not** missing receipts — they are blocked by one defect, stated
here rather than papered over:

**The agent identity is bound to a binary file, not to a reproducible source
identity.** The policy is `merc_agent_running_executable_sha256_v1`:
`engine_build_hash` digests the built binary's sha, and Rust embeds build paths,
so the same source compiled into a different `CARGO_TARGET_DIR` produces different
bytes and therefore a different identity. Measured on this tree — both binaries
current source, both 12,804,992 bytes, `cargo build` a confirmed 0.52s no-op:

- `agent/target/release/merc-agent` presents `15d161336337742b` (deterministic across runs)
- `.artifacts/local-production-cargo-target/release/merc-agent` presents `ba8c6c6398df887f`
- the sealed r7 cell advertises `f4210c0ef62e4490`

No agent buildable on this tree can enrol against the advertised cell. This is the
same defect r6 had; the r7 re-seal moved it rather than fixing it. It also blocks
the first real buyer-to-supplier loop.

**Fix path for the next candidate line:** (a) have the rehearsal build into — or
reuse — the same target dir as the sealed build, so its binary *is* the sealed
binary; and (b) re-measure the cell in place into the existing receipt so
`control/runtime-authority.json`, which the agent embeds via `include_str!`, does
not change. Keeping the authority path frozen across the build is what makes the
fixed point terminate. Not attempted at freeze time because re-sealing is a
pricing-authority change and the previous one moved the rate 2.2x.

## What is genuinely closed

`P1-STRIPE-TEST`, the blocker this program opened against, is **SATISFIED**: the
Stripe matrix PASSES bound to the candidate with a real `tr_` transfer and payout
hold/release/failure/reversal under one fresh `run_id`, `live_mode` PROHIBITED,
no synthesized identifier. `money_and_reconciliation` scores 15/15.

Also closed at the candidate: authorization matrix (126 routes, default deny),
registry + supply-chain from real CI attestations, offsite backup and independent
restore, logical independent restore, technical exercises, alert pipeline/page/
delivery, website validation, hardware characterization, payment simulator,
staging validation, and the external staging attack rehearsal (attacks executed,
zero findings, all five critical negatives NO).

## Why the earlier 88/100 was not real

The score this program started from rested on receipts that named no current
commit: an audit found **0 of 26 required receipts bound to HEAD**, and a broader
sweep found **0 of 246** evidence files bound. The checker never asked when a
receipt was made, so July artifacts counted as present-tense readiness. The
largest single row — the authorization matrix, 11 points — carried no commit at
all. Binding is now enforced: a scored receipt must name the commit in
`ops/candidate.json`, and a separate assertion refuses to score at all if tracked
code changed after it.

## Level B remainder (not backend alpha)

`qualifying-soak-24h` (3, and its gate is currently unearnable by construction —
documented in `qualifying_24h_soak_proven`), plus three named-human approvals:
privacy, licensing-provenance, and staffed abuse route/tabletop.
`P1-INDEPENDENT-APPROVAL` requires a non-author reviewer and cannot be closed by
the author under any amount of work.

## The freeze rule

No further readiness-point optimization is project work. The candidate above is
not modified again. Performance and capability development proceed on the next
candidate line.
