# TRUE_REDUCTION_SYNTHESIS.md — the rules the four audits get reconciled under

Governs the synthesis of `true-control-91k`, `true-scripts-43k`,
`true-tests-scenario`, `true-evidence-ledger`. **No audit suggestion is
implemented individually.** Everything is reconciled first, then bucketed.

---

## 1. Three buckets. There is no fourth.

| bucket | meaning |
|---|---|
| **DELETE** | the authored logic, script, fixture or payload is no longer required |
| **CONSOLIDATE** | N implementations become one canonical mechanism **and authored LOC truly disappears** |
| **KEEP** | protects a unique capability, proof, migration, historical truth, or operational path |

**There is no MOVE bucket.** Moving affects layout, not complexity.

Zero credit for: migrating to LFS or object storage, merging files without
deleting logic, generating code, relocating complexity into a registry, a data
file, a mode switch, or a script.

This rule exists because a prior wave reported "1,303,077 → 295,456 lines" for a
change that deleted **zero** authored lines. Under these buckets that change
scores nothing — correctly.

## 2. Every accepted reduction records

```
authored LOC deleted            files deleted
concepts / state machines removed
capabilities preserved          public contracts preserved
proof / mutation coverage preserved
runtime and CI performance delta
rollback
```

An entry that cannot fill `authored LOC deleted` with lines that **stop
existing** is not an entry.

## 3. Honest targets

| ledger | now | strong target | stretch |
|---|---:|---:|---|
| production source | 119,649 | **100k–110k** | 85–95k only if architecture genuinely collapses |
| tests / proof | 75,345 | **65k–75k** | do not force lower if mutation coverage weakens |
| tooling / ops | 47,159 | **20k–30k** | 15–20k via major script consolidation |
| active evidence | 185 files | **manifests + release-critical bodies only** | payloads content-addressed |
| tracked files | 962 | **450–600** | lower only if clarity improves |
| top-level dirs | 9 | **7–9** | essentially solved |

Stretch below these **only** when architectural reduction proves it safe — never
by metric manipulation.

**Reject any proposal that reduces a metric while increasing conceptual,
operational, generation, or runtime complexity.**

## 4. Priority order

1. **one reusable experiment/evidence framework** replacing one-off scripts
2. **one canonical execution lifecycle** replacing workload-specific copies
3. **shared test scenarios with entry-path adapters**
4. **active evidence manifests + verified content-addressed payloads**
5. **duplicated validators and mirrored Go/SQL authority**

The largest credible opportunity is **tooling (47,159)**, not Go production
source — which is already inside its target band.

## 5. Merge blockers — nothing lands before these

### 5a. CI LFS hydration

All 10 `actions/checkout` set `lfs: true`; `.lfsconfig` sets
`fetchexclude = evidence/perf/**`; checkout runs a plain `git lfs pull` which
honours it. Verified: `--include` alone does **not** override an exclude; only
`--exclude=""` does (84 resolve failures → 2).

CI must: fetch every release-critical object explicitly, **fail on absent
payloads**, run from a **fresh clone with an empty LFS cache**, verify payload
OIDs, and boot the final image.

> A local hydrated worktree is not proof that a clean CI runner can reproduce the
> product.

### 5b. Fingerprint binding

Today `sourceFingerprint` hashes what is on disk — for an LFS-backed file, the
pointer. Faster, and weaker.

Bind: `relative logical evidence identity + OID + payload size + producer/source commit`

Validator must: resolve the payload → hash its bytes → compare against the OID →
**fail if missing** → **refuse citation if unverified**.

**Do not silently change old source fingerprints.** Enumerate every affected
historical receipt and create superseding evidence where needed.

### 5c. Proof of reproduction

Fresh clone, empty LFS cache, full gate suite, release-image boot. Re-run the
performance baseline **repeatedly** — one sample (19.70 s → 20.38 s) cannot
distinguish regression from variance.

## 6. Per-lane decision standard

**Control (91,006).** The question is not "can handlers be merged" but "can
several mechanisms become one canonical mechanism." No credit for moving 2,000
lines into a larger file. Hunt: duplicated request lifecycles, parallel
quote/submit/settle paths, duplicated runtime-cell validation, repeated
buyer/worker authorization ceremony, equivalent state machines, repeated receipt
construction, mirrored Go/SQL decisions, one-implementation interfaces,
workload-specific copies of one generic pipeline.

**Scripts (43,044).** The hard question per script:

> Does reproducing the receipt require this exact driver, or does the receipt
> already carry enough identity, configuration and raw evidence to make the
> driver unnecessary?

An experiment must not permanently leave behind a launcher, a wrapper, a parser,
a summarizer, a validator and a status updater **per run**. One reusable
framework replaces dozens of one-off drivers.

**Tests (75,345).** Build the matrix — scenario × entry path × invariant ×
failure mode × mutation-protected × receipt produced. Some duplication is
deliberate: each entry point proves something different. Target shape is *shared
scenario builder + shared assertions + distinct entry-path adapters*, never
deleting test paths. Invariants: assertion coverage does not fall, mutation
survivors do not increase, every money/admission/security invariant stays
directly exercised, public API paths are not replaced by inner-transaction tests,
runtime ideally falls.

**Evidence (185 files).** Report logical records, unique payload hashes,
duplicate aliases, superseded, withdrawn, actively cited, release-critical,
bytes, hydration cost. A 134,000-line JSON may hold millions of raw samples, or
one result with duplicated formatting, or a historical negative result — those
are **different objects** and must be counted differently.

---

## The governing insight

> Merc's production source is already reasonably compact. The transformation
> comes from reducing the number of **mechanisms** required to build, test, prove
> and operate it — not from deleting half the core.
