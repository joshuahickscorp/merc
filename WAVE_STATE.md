# WAVE_STATE.md — where the superwave landed, and what to re-arm

Written after the 5-hour session limit killed 216 of 515 workflow agents mid-flight.
Everything below marked **verified** was re-checked by hand after the fact.

**Teardown HEAD `114135a1`. Main untouched at `ff441d1e`** with the 100 uncommitted
lines in `control/dev_checkpoint.go` intact.

---

## 1. The refactor landed. All six phases.

```
114135a1  L6: docs 68 to 19 with no-loss checker and contradiction ledger
ae94832e  L5: LFS runpodctl, untrack operator key, checksum-pinned bootstrap
0c721952  L4: migrate 84 evidence/perf files to git-lfs at tip
5ffce714  L3: delete uncited control/evidence perf orphan
0688b1db  L2: resolve LFS pointers in binding gates; dual-form raw_samples
f1f518f0  L1: install git-lfs substrate without converting files
```

### Verified result

| | before | after | target |
|---|---:|---:|---:|
| **committed lines** | 1,303,077 | **295,239** | ≤300,000 ✓ |
| tracked files | 1009 | **962** | — |
| tracked markdown | 68 | **19** | ≤19 ✓ |
| tracked dirs | 9 | **9** | ≤9 ✓ |

Integrity, re-verified by hand:

```
git lfs fsck                    → Git LFS fsck OK   (SUPPLEMENTARY ONLY)
oid == sha256(body)             → MATCH on all 70 unique oids (independent)
scripts/verify-lfs-corpus.py    → 85 pointers / 70 oids / 0 missing / 0 corrupt
validate-claim-surfaces         → PASS (10 surfaces)
validate-governance             → PASS, fail-closed
rename-residue-audit            → RESIDUE=0
validate-repo-boundary          → 962 files, owned LOC 207,407 (unchanged)
gofmt/vet/build                 → clean
go test ./...                   → ok merc/control 20.382s
```

**2026-08-04 incident:** two object-store bodies
(`0684258d…` arrival-batching, `dfb3f133…` gateway-parity with `schemX_version`)
failed independent `sha256==oid` while `git lfs fsck` still said OK. Worktree
payloads were intact; store restored from them. Root cause remains **UNPROVEN**
(mutation/LFS-hardlink hypothesis only). Durable receipt:
`evidence/state/lfs-corruption-incident-20260804.json`. Corpus authority:
`scripts/verify-lfs-corpus.py` + `TestLFSCorpusIntegrity`.

Test wall 19.70 s → 20.38 s (+3.5%, at the edge of run-to-run noise — re-measure
before treating it as a regression).

---

## 2. The one real defect: the metric was measuring the wrong thing

**`PLAN_300K.md` §7 pins `git ls-files | xargs wc -l` as the gate. That command is
wrong for this refactor**, and it reports failure on the two phases carrying 96%
of the reduction.

Reproduced at this exact commit:

```
working tree     1,179,347 lines
committed blobs    295,239 lines
```

Same commit. Two answers. Cause: git-lfs's **clean filter rewrites the index blob
on commit and never touches the file on disk**. `wc -l` reads disk.

```
evidence/perf/arrival-batching.json
  on disk    511 lines
  in commit    3 lines   (version …/oid sha256:0684258d…/size)
git status --porcelain → 0     ← git reports the tree CLEAN
```

Git actively hides it: the clean filter normalizes on read, so `git status` and
`git diff` both show nothing while the committed blob is 508 lines shorter.

**The commits are correct. The verification instruction was wrong.** This is a
false negative, and the live risk during execution was a writer reverting correct
work because the plan's own gate said it did nothing.

**Fix — replace the metric everywhere:**

```bash
git ls-files -s | awk '{print $2}' | git cat-file --batch --buffer | wc -l
```

`PLAN_300K.md` §1 and §7 both need re-baselining against that command. Note the
same property applies to `.tools/runpodctl`: 59,799 on disk, 3 in the index.

---

## 3. Reduction hunt: there is essentially nothing left

4 rounds, 86 distinct candidates, 17 accepted through safety/honesty/legibility
judging. **Total honest lines: 326.**

The honesty lens is why that number is small and trustworthy. Example — the
`docs/screenshots` candidate claimed 1,451 lines:

> Those "lines" are 0x0A bytes in a DEFLATE stream. 2 of 1405 are even printable.
> revisedLines = 0.

It also caught that the same convention inflates the *baseline* by **77,033
phantom lines** across all tracked binaries, and named the pattern: harvesting
those is *"metric laundering, not teardown."* Real saving there is 395 KB of
clone weight and 2 orphaned files — worth doing as hygiene, worth zero as lines.

**Practical conclusion: the tracked-line frontier is closed.** 295,239 is at or
near the floor.

---

## 4. Killed by the session limit — re-arm these

### 4a. Un-adjudicated pre-mortem findings (raised, never verified)

73 findings probed, 52 survived refutation, but **`[synthesis]` died**, so there
is no ranked blocker list. These were raised and their three verifiers all failed
— they are neither confirmed nor refuted:

| finding | status |
|---|---|
| `PAYLOAD-GUARD-GOES-VACUOUS-SILENTLY` | **REFUTED** — see §4a-i |
| `L4-GOTESTS-FAIL` | **REFUTED** — `go test ./...` passes, 20.382 s |
| `CI-LFS-TRUE-IS-A-NO-OP` | **CONFIRMED, P0** — see §4a-ii |
| `PRICING-QUARANTINE-GATE-HOLLOWED` | open — pricing authority resolution against LFS-backed receipts |
| `SOURCE-FINGERPRINT-NOT-A-FUNCTION-OF-COMMIT` | open — fingerprint now hashes pointers; does the oid transitively attest? |
| `RAW-SAMPLES-SHA256-UNVERIFIED` | open — can a tampered old receipt validate through the L2 dual-form branch? |
| `IMMOVABLES-COST-THE-TARGET` | open — moot, target met at 295,239 |
| G1–G10, F3, F4, F7, F8 | open — docs-phase findings, all verifiers killed |

#### §4a-i — `PAYLOAD-GUARD-GOES-VACUOUS-SILENTLY` is REFUTED

The design is sound. Cloned with `GIT_LFS_SKIP_SMUDGE=1` so every tier-2 body was
absent, then ran the gate:

```
REAL EXIT (no-LFS clone): 1
  - evidence/perf/arrival-batching.json: unreadable JSON after LFS resolve
    (oid sha256:0684258dc2d0ff…): cannot resolve LFS pointer
```

It fails **loud**, and names the payload by digest. It does not pass on a pointer.

One real artefact, worth knowing but not a defect: the census classification
drifts by environment — with bodies `BOUND 48 / UNBOUND 108 / OTHER 0`, without
them `BOUND 48 / UNBOUND 107 / OTHER 2`. Both exit 1 because 2 `MISSING_STATUS`
failures are pre-existing and were red on `main` before any of this.

#### §4a-ii — `CI-LFS-TRUE-IS-A-NO-OP` is CONFIRMED. **CI will go red on the next push.**

All 10 `actions/checkout` steps across the three workflows set `lfs: true`. But
`.lfsconfig` sets:

```
fetchexclude = evidence/perf/**
```

`actions/checkout` runs a plain `git lfs pull`, which **honours fetchexclude**. So
CI dutifully fetches LFS and skips precisely the tier-2 files the gates need.
Reproduced:

```
git lfs pull                                  → arrival-batching.json still a pointer
git lfs pull --include="evidence/perf/**"     → still a pointer (include does NOT override exclude)
git lfs pull --exclude="" --include="…"       → resolves; LFS failures 84 → 2
```

The comment inside `.lfsconfig` — *"CI sets lfs: true on actions/checkout so gates
see full content"* — is false, and the file it sits in is what makes it false.

**Fix**: add a step after every checkout that clears the exclude explicitly —
`git lfs pull --exclude="" --include="evidence/perf/**"` — or drop `fetchexclude`
and accept full-fetch clones. `--include` alone is not sufficient.

*(The 2 residual failures in the probe are objects absent from the local source
repo, not a config problem — but confirm every object exists on `origin` before
pushing, or CI fails for that reason instead.)*

### 4b. Reduction-loop gaps

- `[floor-critic]` died — no formal floor statement
- **Rounds 3 and 4 hunts never ran** (all 7 lenses × 2 rounds killed), so
  loop-until-dry never actually reached dry
- All 19 `SCHEMA-IDX-*` candidates lost their judges — `schema.sql` index findings
  are unadjudicated
- ~10 `G*` control-file merge candidates lost judges

### 4c. Grok audits — all five completed, all unread

```
money-surface   330 lines   ← the unblocker: true money surface vs the 22-file list
sql-schema      339 lines   ← first-ever audit of schema.sql
web-agent-ops   380 lines
next-frontier   388 lines
exec-300k       154 lines   ← the writer's own report
```

**`money-surface` is the highest-value unread artifact.** It re-derives money
ownership from the code rather than the allowlist, and the live gap it measures
(~6,038 LOC of money logic outside the guard) is a real defect in the repo
independent of any refactor.

---

## 5. Still open, unchanged

- **RunPod key**: removed from the index in L5, **still in git history**. Rotation
  is the user's action, deferred by their decision. Do not remove from history
  without it.
- **History rewrite**: authorized, conditional on teardown being finished and
  tested. It invalidates every checkpoint receipt and source fingerprint bound to
  a rewritten SHA — affected receipts need *superseding evidence*, never in-place
  edits. Enumerate them before executing.
- **`control/dev_checkpoint.go`**: 100 uncommitted lines in main, excluded from
  every merge group. Reintegrate before merging teardown.
- **Not merged to main.** No push. No history rewritten.
