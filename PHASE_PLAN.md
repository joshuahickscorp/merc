# PHASE_PLAN.md — measured verdict on every target, and the executable phases

Baseline: `refactor/teardown` @ `21af29c8`. Stage one (19→9 directories) landed.
Six read-only censuses measured everything below. Companion documents:
`REFACTOR_PLAN.md` (immovables and why).

---

## 1. Verdict on each target

| Target | Measured | Verdict |
|---|---|---|
| tracked dirs ≤9 | **9** | **MET** |
| tracked markdown ≤18 | 19 without codegen, 18 with | **MET at 19** — see §2 |
| control Go LOC ≤165,600 | ~163,000 reachable | **MET**, 2.6k margin |
| all tracked lines ≤1.0M | 987,098 reachable | **MET** |
| **control Go files ≤160** | **189 free-only floor** | **NOT MET in Wave A** |
| **tracked files ≤650** | **668 after Wave A** | **NOT MET in Wave A** |

Four of six clear on safe work alone. Two require Wave B — merging files whose
**basenames are a load-bearing interface** — which is permitted by the directive
only with the guard re-derived from domain membership and an audit.

### Why 160 and 650 are not reachable safely

463 `control/*.go` = 133 free prod + 237 free test + 93 named. Packing only the
free files, inside the 300–1500 LOC band, with money merged only with money:

```
free prod  133 → 55
free test  237 → 41
named       93 → 93   (untouched)
                 ───
                 189      <- free-only hard floor, 0 LOC deleted
```

That propagates:

```
1009  current
 −50  docs 68 → 18
−274  control 463 → 189
 −15  byte-identical evidence aliases
  −1  control/evidence orphan (uncited, 122,368 lines)
  −1  untrack runpodctl
─────
 668  <- Wave A floor. Target is 650. Short by 18.
```

**Nowhere cheap is left.** The periphery census examined `scripts/` (153),
`agent/` (30) and `clients/` (17) — the floor audit's declared blind spot — and
found **zero proof-safe deletes**. The 22 scripts with no inbound reference are
evidence producers named in committed receipts, operator entry points, or unwired
gates. Rust has no unused crates and no dead-code noise. Clients are public
contract, seatbelt profile, and legal text.

So the last 18 files can only come from the 93 named. Wave B tiers:

| Scenario | control files | tracked files | Touches money guards? |
|---|---:|---:|---|
| Wave A only | 189 | 668 | no |
| + named **non-money** + named test packs | ~144 | ~623 | **no** |
| + named money packs | ~134 | ~613 | **yes — audit required** |

**Recommendation: Wave A, then Wave B-non-money only.** That reaches ~623 files
and ~144 control files — clearing ≤650 *and* the 625–630 stretch — **without
touching a money or admission guard.** The money packs buy 10 further files and
are not worth re-deriving `moneyAndAdmissionAuthorityFiles` for.

---

## 2. The documentation floor is 19, not 18

14 paths are frozen (8 legal instruments + `SECURITY`/`RUNBOOKS`/`RENAME_REGISTER`
+ `README`/`CHANGELOG`/`RELEASE_READINESS`), plus the generated census, plus 3
merged homes = **18** — but only if `scripts/build-cli-release.sh` *generates*
`clients/cli/README.md` from a slice of `ARCHITECTURE.md`.

The directive forbids codegen "unless tracked LOC/files fall **and complexity does
not rise**." Files fall by one; complexity rises. **Keep `clients/cli/README.md`;
the honest floor is 19.**

---

## 3. Where the LOC actually is, and the one real lever

`Routes()` in `api.go` is already one line per route — table-driving it costs
**−3 LOC**, i.e. it is worse. Skipped.

| lever | LOC | risk | guards |
|---|---:|---|---|
| `must(t, err)` replacing `if err != nil { t.Fatal }` | **2,656** | LOW | none (tests) |
| known dead code | ~180 | LOW | varies |
| store list / admin table / receipt helpers | ~400–600 | MED | `api.go`, `store.go` |

166,351 → **~163,000**. Reachable **without** touching money, admission,
`money_nanos` (a deliberate exactness surface) or `RuntimeProfileAdapter` (a
documented authority boundary). If any proposal claims the LOC target *only* via
money-path merges, reject it — the test ceremony alone is sufficient.

---

## 4. Where the mass is

```
evidence/perf/   826,488 lines = 63.4% of the entire repository
```

Route to ≤1.0M, touching **zero cited receipts**:

```
1,302,701
 −133,436  byte-identical aliases (verified by hash, not by name)
 −122,368  control/evidence/perf orphan — all citations resolve to the
           top-level evidence/perf copy; nothing cites the control/ one
 − 59,799  untrack runpodctl (13 MB binary)
──────────
   987,098   ✓
```

**Do not collapse `quiet.json` into `quiet.bound.json`.** They are not
byte-identical: 29 keys match, but `quiet.json` uniquely records
`missing_identity_fields` — a negative result about the original run. The
directive protects refusals. The −134,062 that collapse would buy is not needed.

Immovable regardless of the number: `quiet.bound.json` (BOUND authority),
`local-metal.json` (cited by two tests), `local-metal.bound.json` (SUPERSEDED
historical measurement, content differs from quiet), `runpod-vllm-latest.json`
(live interface).

---

## 5. No-degradation gate, armed at `21af29c8`

| metric | baseline | class |
|---|---:|---|
| go build warm / cold | 0.36 s / 4.80 s | code-shape |
| unit test wall | 19.70 s (1574 pass, 439 skip) | code-shape |
| control binary | 25,085,954 B | code-shape |
| agent binary | 12,309,136 B | code-shape |
| image content | 11,252,151 B | code-shape |
| startup → `/readyz` | 0.466 s | code-shape |
| authorize p50 c=1 | 1.717 ms | **money path** |
| RealtimeIdentityCacheHit | 445 ns/op | code-shape |
| CanonicalToolsAndSchema | 13,118 ns/op, 193 allocs | code-shape |
| source fingerprint | 0.14 s / 1004 files | **file-count** |

File-count metrics **should improve** as tracked files fall 1009 → ~623.
Code-shape metrics **must not regress**. Any phase touching realtime, store,
pricing or API re-runs the authorize and gateway harnesses.

Blocked, with fill-in commands recorded: checkpoint digest, `prove --full`, E2E
segment latency (~30 min, writes into `evidence/perf/`), peak RSS (Seatbelt
denies `ps`).

---

## 6. Phases

One writer in `.worktrees/teardown`. One phase per commit, each independently
revertible, each leaving gates no worse than baseline.

| # | Phase | Δfiles | ΔLOC | Risk | Guards touched |
|---|---|---:|---:|---|---|
| P1 | Mass: delete `control/evidence` orphan, collapse 15 verified aliases, untrack `runpodctl` + checksum-pinned bootstrap | −17 | −315,603 | LOW | binding census re-stamp |
| P2 | Docs 68 → 19 + `scripts/docs_noloss_check.py` + contradiction ledger | −49 | — | **HIGH** | validators, claim-policy, CITE_ROOTS, `FROZEN_PATHS` |
| P3 | Test ceremony: `must`/`mustf` | 0 | −2,656 | LOW | none |
| P4 | Dead code + prod helpers (store list, admin table, receipt) | 0 | −600 | MED | `api.go`, `store.go` |
| P5 | Control Wave A: free packs only | −274 | 0 | MED | none (free by definition) |
| P6 | Control Wave B **non-money only** + named test packs | −45 | 0 | **HIGH** | MUTATIONS, `os.ReadFile`, scripts, CODEOWNERS |
| — | Wave B money packs | −10 | 0 | **REJECTED** | not worth re-deriving the money allowlist |

Landing: **~623 files, ~163,000 control Go LOC, ~987k tracked lines, 19 markdown,
9 directories.**

`control/dev_checkpoint.go` is excluded from every group — the main tree holds
100 uncommitted lines in it.

### Per phase

```
Grok read-only census (done) → writer in teardown → Claude diff review
→ targeted tests → Grok adversarial audit of the exact commit
→ count/LOC/performance receipt → commit
```

P2 is the phase most likely to go wrong: highest volume of irreversible human
meaning, no compiler to catch a dropped caveat, and the ten known contradictions
will read as merge errors unless the ledger ships in the same commit.

---

## 7. Still open

- **RunPod key** (`.tools/rp-key/id_ed25519`, on `origin/main`): user handles
  rotation post-refactor. Not on the critical path.
- **History rewrite**: authorized, conditional on teardown being finished, merged
  and tested. It invalidates every checkpoint receipt and source fingerprint
  bound to a rewritten SHA — receipts citing an old SHA need **superseding
  evidence**, never edited in place. Enumerate the affected receipts before
  executing.
- **`dev_checkpoint.go`**: reintegrate the 100 uncommitted lines before merge.
- **Do not merge to main during this tranche.**
