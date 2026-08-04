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
git lfs fsck                    → Git LFS fsck OK
oid == sha256(body)             → MATCH on spot-checked files
validate-claim-surfaces         → PASS (10 surfaces)
validate-governance             → PASS, fail-closed
rename-residue-audit            → RESIDUE=0
validate-repo-boundary          → 962 files, owned LOC 207,407 (unchanged)
gofmt/vet/build                 → clean
go test ./...                   → ok merc/control 20.382s
```

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

| finding | why it matters |
|---|---|
| `PAYLOAD-GUARD-GOES-VACUOUS-SILENTLY` | the exact failure this wave was designed to catch — a gate that passes on a pointer |
| `PRICING-QUARANTINE-GATE-HOLLOWED` | pricing authority resolution against LFS-backed receipts |
| `SOURCE-FINGERPRINT-NOT-A-FUNCTION-OF-COMMIT` | fingerprint now hashes pointers; does the oid transitively attest? |
| `RAW-SAMPLES-SHA256-UNVERIFIED` | the L2 dual-form branch — can a tampered old receipt validate through it? |
| `CI-LFS-TRUE-IS-A-NO-OP` | does CI actually fetch LFS, or validate pointers? |
| `IMMOVABLES-COST-THE-TARGET` | do the protected paths eat the margin? |
| `L4-GOTESTS-FAIL` | **REFUTED by hand** — `go test ./...` passes, 20.382s |
| G1–G10, F3, F4, F7, F8 | docs-phase findings, all verifiers killed |

**`PAYLOAD-GUARD-GOES-VACUOUS-SILENTLY` and `SOURCE-FINGERPRINT-NOT-A-FUNCTION-OF-COMMIT`
are the two to settle first.** Both ask whether a gate now passes on a 3-line
pointer while believing it validated a receipt. `validate-evidence-binding.py`
still reports `total_files: 166`, which is consistent with pre-split behaviour —
but consistent is not the same as verified.

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
