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

## Current inventory supersession — 2026-08-08 working candidate

The historical snapshot below is retained as evidence of its own measurement;
it is **not** the current-candidate inventory. The independent corpus validator
now reports **88 tracked LFS pointer paths / 73 unique OIDs / 15 aliases**, with
zero missing, corrupt, or hydrated-body mismatches. Use
`scripts/verify-lfs-corpus.py` and `evidence/state/lfs-corpus-ledger.json` for
the live expected counts. The exact candidate remains uncommitted and unpushed,
so this is a working-tree ledger rather than an off-machine durability claim.

---

### Working-tree four-ledger snapshot — 2026-08-08 (not an exact-commit receipt)

This additive snapshot uses `git ls-files -co --exclude-standard`: it includes
the intended candidate files, excludes ignored caches/build outputs, and must be
recomputed after commit. It records **999 candidate inputs**: 984 indexed paths
and 15 untracked candidate artifacts.

| ledger | current working-tree result |
|---|---:|
| production source | **123,105 LOC** (`control` non-test Go 93,531; agent Rust 20,936; schema 6,580; web 1,321; clients 737) |
| test and proof | **78,687 LOC** (268 `control/*_test.go` files) |
| tooling and operations | **49,973 LOC** (scripts 45,851; ops 4,122) |
| evidence | **194 files**; **88 LFS pointers / 73 OIDs / 15 aliases** |

This is a measurement, not a claimed authored-code reduction. The current work
removes guard exemptions and stale declarations, adds proof, and formats five
Rust lines; it does not claim a true production-code consolidation.

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

185 files, 84 LFS-backed under `evidence/perf/**` plus `.tools/runpodctl`
(85 tracked LFS pointer files total). Payload integrity is now a fail-closed
ledger, not a mass:

| derived count | value | authority |
|---|---:|---|
| tracked LFS pointer files | **85** | `scripts/verify-lfs-corpus.py` |
| unique payload OIDs | **70** | same (15 content-addressed aliases) |
| missing objects | **0** | independent `sha256(object)==oid` |
| corrupt objects | **0** | independent `sha256(object)==oid` |
| resolved-payload mismatches | **0** | worktree vs pointer oid |

Expected counts live in `evidence/state/lfs-corpus-ledger.json`. If the tree
legitimately gains a pointer, the gate fails with
`N pointers, expected 85 — update the ledger` rather than silently passing.
`git lfs fsck` is **supplementary only** — on 2026-08-04 it returned OK while
two object-store bodies were corrupt (see
`evidence/state/lfs-corruption-incident-20260804.json` and the section below).

#### LFS corruption incident (2026-08-04) — receipt, not root cause

Two local LFS object-store bodies failed an independent hash check while
`git lfs fsck` reported OK:

| path | oid | corrupt sha256 (was) | repair |
|---|---|---|---|
| `evidence/perf/arrival-batching.json` | `0684258d…246236` | `348f6339…bcc52` | restored from intact worktree |
| `evidence/perf/gateway-parity.json` | `dfb3f133…bcdb7d` | `b306d9fe…f21385` (`schemX_version`) | restored from intact worktree |

**Root cause: UNPROVEN.** The mutation-suite / LFS-hardlink interaction is a
hypothesis (single-byte `a→X` on `schema_version`, shared object store across
worktrees, intact worktrees). Disposable reproduction in A2: hardlinking a
worktree LFS path onto the object-store path and applying the same one-byte
edit yields the **exact** incident corrupt digest (`b306d9fe…`) — so the
*coupling* is real. `mutation-test.sh` only mutates `control/*.go` and does not
create that hardlink, so the agent of the original write remains unproven.
Details: `evidence/state/lfs-corruption-incident-20260804.json`.

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
