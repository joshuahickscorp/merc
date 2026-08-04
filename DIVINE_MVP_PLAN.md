# DIVINE_MVP_PLAN.md — what six independent audits found, including one unresolved contradiction

Baseline `refactor/teardown` @ `674e12fc`. Six read-only audits (kernel,
duplication, data-over-code, evidence, tests, performance) ran independently
against the same tree with no sight of each other's output.

**Nothing here is executable yet.** One finding is contradictory and one class of
work is gated. Both are stated below rather than averaged away.

---

## 1. The convergent finding — three audits, one root cause

D1 (kernel), D2 (duplication) and D5 (tests) independently identified the same
blocker, each while hunting something else:

| audit | its words |
|---|---|
| D1 | "Basename guards are a structural smell blocking healthier packages." |
| D2 | "Filename-pinned guards are a real restructuring tax; symbol/package guards should precede money packing." |
| D5 | "The source-filename gates and basename authority lists are the real architectural debt: they freeze file layout and block the teardown targets **more than test LOC does**." |

The mechanism: 93 `control/*.go` basenames are pinned by a `sed`-driven mutation
suite (27), `moneyAndAdmissionAuthorityFiles` (22), seven tests that `os.ReadFile`
a source file and string-match its text, plus scripts and CODEOWNERS.

**This is a guard implemented as text search over filenames.** It is why the
architecture cannot be restructured. Every money-adjacent consolidation is behind
it.

**Replacement** (D5 priced it): a small `go/analysis` pass registering forbidden
identifier sets **by package path, not basename**, plus a mutation suite that
targets symbols. Costs ~0 product LOC. Survives renames. Strictly stronger than
string-matching, because it understands what a symbol *is*.

**This is phase one. It was absent from the first plan entirely.**

---

## 2. The unresolved contradiction — the panel's primary target

D1 and D2 give control-LOC floors an order of magnitude apart.

| audit | floor | method |
|---|---|---|
| **D1** | **125–145k** prod+test (from 166,351) | top-down: derive 14 kernel domains from the 123 routes, estimate each domain's irreducible size |
| **D2** | **~165.5k** near-term; **~162k** after a multi-phase money runtime | bottom-up: enumerate every concrete consolidation with quoted code and sum it |

That is a gap of **20,000–40,000 lines** with no reconciliation.

Both cannot be right. Either:

- **D1 is over-optimistic** — a domain-shaped estimate of "what it would look like
  written once" that no enumerated change list supports, or
- **D2 is under-counting** — it found only what could be proven line-by-line and
  missed structural collapse that is real but not visible to that method.

**Resolving this is the panel's first job.** No LOC target is credible until it
is resolved, and the ≤145k target sits exactly inside the disputed band — D1
touches it, D2 refuses it.

---

## 3. Refusals, each measured

These were sought and rejected with evidence. They are results, not failures.

**Packing buys nothing but file count (D6).** `PHASE_PLAN` called the source
fingerprint file-count-sensitive; it is **byte**-bound.

```
pack 450 small files    →  −1.3 MB (2.5%)  →  fingerprint −4 ms
untrack evidence/perf   →  −21.7 MB        →  0.14s → ~0.08s
untrack .tools          →  −13.2 MB        →  0.14s → ~0.10s
```

Packing 463 → 189 files also barely moves build time — Go's compile unit is the
package. The image is already `-ldflags="-s -w"` and already excludes
`evidence/perf`. **File-count targets are cosmetic; byte targets are real.**

**Data-over-code is mostly a mirage (D3).**

| proposal | verdict |
|---|---|
| generic JSON handler framework | REJECT — decision moves into anonymous closures; misses the real bulk |
| reflection struct-tag validation | REJECT — worse on money/admission |
| ORM / codegen store | REJECT |
| failure/traffic/verification classes → JSON | REJECT — ~0 net LOC, +1 file each, **compiler exhaustiveness lost** |
| extract pure `*_validate.go` | ACCEPT — ~0 LOC, quality only |

**Money rails are different products, not one formula (D1 + D2 agree here).**

> Money is one ledger + one precision type + many priced products. The floor is
> not "one pricing function"; it is "one ledger, one freeze protocol, N product
> meters." — D1
>
> Parallel sale machines are not file bloat — they encode different unit
> economics and trust models. — D1

**Evidence bulk is substrate, not duplication (D4).** 826,488 lines = 63.4% of
the repo, but true executable authority is **~15 files / ~2.7k lines**. The
gateway-parity monsters are **~96% `raw_samples`** — re-derivation substrate.
D4 refuses off-tree archival unless a retrievable content-addressed store exists:
*"otherwise keep bodies and accept a higher clone floor. That is a proven higher
floor, and that is a correct answer."*

**Test mass is proof, not ceremony (D5).** Reducible: ~3–4k LOC, all scaffolding
(1,351 `if err != nil { t.Fatal }` blocks = 4,089 LOC → `must`/`mustf` ≈ −2,700,
plus store openers and seeders). Assertion count unchanged. Floor **~70k test
LOC, ~255 files** without packing.

**`quiet.json` is not a duplicate** — uniquely records `missing_identity_fields`,
a negative result. Confirmed independently by D4.

---

## 4. Verdict on the aggressive targets

| target | measured | verdict |
|---|---|---|
| dirs ≤8 (7) | 9 today; D1's 14 domains do not fold `docs`/`pricing` for free | **likely refused** |
| files ≤550 (450) | periphery has **zero** proof-safe deletes; packing floor 623 | **refused without guard migration** |
| control Go files ≤120 (90) | D1/D2 both say **~120–160**, only after guard migration | **120 reachable, 90 refused** |
| markdown ≤15 (12) | 14 paths are externally frozen + 1 generated census | **hard floor 15** |
| control LOC ≤145k (125k) | **D1 125–145k vs D2 165.5k — unresolved** | **blocked on §2** |
| tracked lines ≤750k (600k) | bytes route is real; 987k proven, below needs the raw/summary split | **plausible, needs D4's CAS story** |

---

## 5. Architecture-level wins D1 identified (beyond packing)

> First-pass file packing (463→189) does not change the domain floor. Domain work
> is how you beat 189 files.

- **Project composition** and **receipt multiplicity** are the genuine
  architecture-level merges.
- Twelve-plus "authority" nouns are **three patterns** (freeze, gate, catalog).
  Renaming is cheap, safe, and does not yield large LOC.
- `schema.sql` (6,581 lines) stays multi-thousand: *"accretion is history;
  rewrite ≠ delete state."*

---

## 6. Proposed phase order

| # | Phase | Gate |
|---|---|---|
| **G0** | **Guard migration** — symbol/package analyzer replaces basename pins; mutation suite targets symbols | mutation suite green on symbols; all 7 `os.ReadFile` gates replaced |
| G1 | Bytes: alias dedupe, `control/evidence` orphan, `.tools` bootstrap, evidence raw/summary split per D4 | fingerprint improves; zero cited receipts touched |
| G2 | Test scaffolding: `must`/`mustf`, shared openers | **assertion count unchanged**; wall time ≤ 19.70 s |
| G3 | Docs 68 → 19 with the no-loss checker + contradiction ledger | checker exits 0 |
| G4 | Naming: authority vocabulary → freeze/gate/catalog | no behaviour change |
| G5 | Control packing, now unblocked by G0 | build/binary/latency unchanged |
| G6 | Money runtime consolidation | **REQUIRES AUDIT** — only after §2 is resolved |

Anything touching money, admission, pricing, settlement or cell authority stays
behind G0 **and** an adversarial audit. No exceptions.

---

## 7. Open, and not to be resolved by averaging

1. **The D1/D2 LOC contradiction** (§2) — panel's first job.
2. **Whether ≤8 directories is reachable** without a fold that harms clarity.
3. **D4's archive**: is there a retrievable CAS, or do we accept the clone floor?
4. **RunPod key**: user rotates post-refactor; tracked copy stays until then.
5. **History rewrite**: authorized, conditional on teardown finished and tested.
   It invalidates every checkpoint receipt and fingerprint bound to a rewritten
   SHA — affected receipts need **superseding evidence**, never in-place edits.
6. **`control/dev_checkpoint.go`**: 100 uncommitted lines in main; excluded from
   every merge group; reintegrate before merge.

**No writer runs until the panel finds no surviving P0/P1.**
