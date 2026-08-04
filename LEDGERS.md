# LEDGERS.md — four numbers, measured, replacing one misleading one

## The correction

`295,456 committed blob lines` is a **Git-storage** figure. LFS pointers are not
deleted payload. Reporting it as a reduction from 1.3M was metric laundering —
the same failure this programme's own honesty lens caught when a candidate
claimed 1,451 lines of screenshot that were newline bytes in a DEFLATE stream.

**The number that did not move: authored source. Not by one line.**

Correct claim: *Git's committed blob footprint fell ~77% through LFS and evidence
deduplication, while authored source LOC was unchanged.*

From here, four independent ledgers. **A pointer never counts as removal of its
payload.**

---

## Measured at `7784413a` (direct measurement; the census is stale — it reports
## 776 tracked files against an actual 962)

### Ledger 1 — Production source: **119,649**

| | LOC |
|---|---:|
| `control/*.go` (non-test) | 91,006 |
| `agent/**/*.rs` | 20,127 |
| `control/schema.sql` | 6,580 |
| `web/**` (html/js/css) | 1,303 |
| `clients/**` (py/ts/swift) | 633 |

### Ledger 2 — Test and proof: **75,345**

`control/*_test.go`. 259 files. Two independent audits found the assertion mass
non-redundant and ~3–4k of removable scaffolding.

### Ledger 3 — Tooling and operations: **47,159**

| | LOC |
|---|---:|
| `scripts/**` | 43,044 |
| `ops/**` | 4,115 |

### Ledger 4 — Evidence

185 files, 84 LFS-backed. Payload bytes, logical records and alias counts are
**not yet quantified** — nobody has audited this as a ledger rather than as mass.

---

## What this reveals

**Production source is already 119,649 — inside the 100k–125k moonshot band.**

The programme has been measuring the wrong thing in both directions. The bulk of
authored lines is not the product:

```
production   119,649   ← already at target
tests         75,345
tooling       47,159   ← scripts alone (43,044) exceed the entire Rust agent
```

`scripts/` is larger than `agent/` by more than 2×. It has never been audited for
*necessity*, only for dead code — where it returned zero deletable files because
every script is a gate, an evidence producer, or an operator entry point.

So the real question is no longer "can production source reach 100k". It is:

1. Can `control` production Go go 91,006 → 65–85k? D1 claimed so top-down; P1
   refuted the claim as **unlisted** — the enumerated consolidations summed only
   −4.5–7.5k. That gap is unresolved and needs a change list, not an estimate.
2. Is 43,044 LOC of `scripts/` proportionate to what it gates?
3. Is 75,345 LOC of test genuinely irreducible at the *scenario* level, as
   opposed to the fixture level already examined?

---

## Merge-blocking, before any further compression

1. **CI does not fetch tier-2 evidence.** All 10 checkouts set `lfs: true`;
   `.lfsconfig` sets `fetchexclude = evidence/perf/**`; `actions/checkout` runs a
   plain `git lfs pull` which honours it. Verified: `--include` alone does not
   override an exclude; only `--exclude=""` does (84 failures → 2).
2. **Source fingerprint must bind pointer OID + size + verified payload**, and
   **fail closed when an object is absent.** Today it hashes whatever is on disk —
   faster, and weaker.
3. **Prove a fresh clone resolves every citable receipt**, and that every LFS
   object exists on `origin`.
4. **Re-run the full performance baseline repeatedly**, not once. The single
   sample 19.70 s → 20.38 s cannot distinguish regression from variance.

---

## Standing rule for every future proposal

Each must name:

```
capability preserved
public contract preserved
proof preserved
LOC actually deleted        ← authored lines, not pointers
complexity actually removed
runtime/performance result
```

Rejected outright: moving content to LFS, moving code into generation, god files,
merging files without deleting logic, relocating complexity into a registry or a
mode switch.

**Report storage footprint and authored-code reduction as separate results.
Never as one.**
