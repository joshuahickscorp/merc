# BUCKETS.md — four audits reconciled. Every target refused, with the arithmetic.

Governed by `TRUE_REDUCTION_SYNTHESIS.md`. Three buckets, no MOVE.

> Current inventory supersession (2026-08-08 working candidate): the historic
> **85 pointers / 70 OIDs** statement in §5 is retained below as a snapshot, not
> current authority. The independently verified live corpus is **88 pointers /
> 73 OIDs / 15 aliases**, zero missing/corrupt/hydrated mismatches. The candidate
> is still uncommitted and unpushed, so this does not assert remote durability.

Each audit partitioned its ledger to the exact line and produced an enumerated
change list rather than an estimate — which is what P1 demanded after D1's
top-down 65–85k claim collapsed for being unlisted.

---

## 1. The verdict

| ledger | now | target | **evidenced floor** | verdict |
|---|---:|---:|---:|---|
| production source | 119,649 | 100–110k | **~119,000** | **refused** |
| tests / proof | 75,345 | 65–75k | **~72,600** | **refused** |
| tooling / ops | 47,159 | 20–30k | **~41,400** | **refused decisively** |
| evidence tooling | — | — | **0 authored lines** | **refused** |

**Total honest authored-line reduction available: ~3,400–4,600 of 242,153 — under 2%.**

Not one audit found a way to reach its target. Four independent partitions, each
summing exactly to its ledger, each concluding the floor is essentially where the
code already is.

## 2. DELETE — authored lines that stop existing

| item | lines | source |
|---|---:|---|
| `scripts/gateway-parity.py` — **INVALIDATED**, superseded | **1,460** | T2 |
| `must`/`mustf` test scaffolding (1,351 `if err != nil { t.Fatal }` blocks) | **1,500–2,700** | T3 |
| control dead code + enumerated consolidations | **224–280** (stretch 400) | T1 |
| exact-duplicate validator helpers | **150–170** | T2 |
| `SubmitJobTx` fail-closed cases → one table-driven test | **50–80** | T3 |

Everything above deletes authored lines. Nothing above is a move.

## 3. CONSOLIDATE — accepted

Almost nothing survived this bucket, and the reasons are the finding:

- **Control**: `api.go` "ceremony" is **domain glue, not boilerplate**. `store*.go`
  has **no cross-file clones**. `schema.sql` is **not** a Go mirror. The three
  hypotheses that motivated the whole programme are individually refuted.
- **Scripts**: rehearsal and RunPod families **already share libs**. Further
  multi-mode merges **refused** — a merged script with six modes is worse than six
  scripts.
- **Tests**: dispute-resolve paths share a walk but assert **different** money and
  lifecycle properties. Mergeable only if A∪B assertions all survive.

## 4. KEEP — with the reason, because these are the answer

- **Same scenario is almost never proven twice with the same invariants.** The
  large test files are transition matrices, progressive multi-agent stages, or
  distinct realtime products (stream contract vs prepaid funding vs coalesce vs
  exact reuse). *"No multi-10k scenario deletion exists without cutting proof."*
- **Dual-language SDK/install paths and live/test credential splits are
  intentional.**
- **Money logic outside the 22-file allowlist is a guard gap, not free deletes** —
  a real safety finding, and not a reduction opportunity.
- **~5.5k of opt-in measure harnesses** inflate Ledger 2 without costing unit-suite
  time. Ledger inflation, not waste.
- **Evidence: zero authored-line reduction** after the blockers are fixed. 22.2 MiB
  and ~70k logical records, of which active production authority is **~37 paths /
  0.32 MiB**. The rest is archive, and archive is the product.

## 5. Merge blockers — designed, not yet built

**B1 — CI hydration.** `git lfs pull --exclude=""` after checkout; minimum the
`control` and `interfaces` jobs. Must fail closed on absent payloads and run from
a fresh clone with an empty LFS cache.

**B2 — fingerprint binding.** Bind LFS pointer **oid + size**, resolve and verify
the payload, fail closed when absent, refuse citation when unverified. This
changes historical `source_sha256` → **supersede, never edit**. Affected receipts
must be enumerated first.

**B3 — origin was clobbered mid-wave.** `origin` had been rewritten to
`/nonexistent-remote-xyz` (config mtime 16:10, during the execution run).
**Restored** to `git@github.com:joshuahickscorp/merc.git` and verified against the
live repo. LFS objects have never been pushed anywhere — `git lfs push` is
required before this branch is usable by anyone else, and until then the LFS
payloads exist **only on this machine**.

**LFS integrity is sound**: 85 tracked files → **70 unique oids** (15 are
byte-identical duplicates, correctly deduplicated by content addressing), **zero
missing objects**, `fsck` OK.

## 6. What this programme actually produced

Genuine, and worth keeping:

- 25 → 9 top-level directories
- 68 → 19 markdown, with a no-loss checker and a contradiction ledger
- 122,368-line uncited orphan deleted
- ~77% Git storage footprint reduction via LFS
- a private SSH key removed from the index
- **a live money-guard gap**: ~6,038 LOC of money logic outside the allowlist

Not produced: meaningful authored-code reduction. **Merc was already near its
floor when this started**, and four audits each partitioning to the exact line is
the proof.

> The transformation comes from reducing the number of mechanisms required to
> build, test, prove and operate it — and the audits found those mechanisms are
> already close to minimal. The remaining work is the guard gap and the two merge
> blockers, not further compression.
