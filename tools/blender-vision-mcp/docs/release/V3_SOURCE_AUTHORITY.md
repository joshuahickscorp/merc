# V3 source authority

Which commit the public product is extracted from, and why.

## Branches and tags surveyed

| Ref | Head | Role |
|---|---|---|
| `goal/visionmcp-v2-ocular-os` | see below | V2.1 Ocular work; the extraction source |
| `goal/visionmcp-v2-all-seeing-eye` | `247431e` | V2 authority, ancestor |
| `codex/visionmcp-live-sandbox-acceptance` | `84d8f971` | V1 superset, ancestor of V2 |
| tag `ocular-honest-baseline-270ee69` | `270ee69` | frozen honest baseline, pushed to remote |

No unresolved conflicts: the ocular branch is linear from `247431e`, which is
itself linear from `84d8f971`. Nothing accepted lives on an unmerged branch.

`270ee69` is **not** the final source. The Master Goal warned against assuming
it, correctly — verdict authority, merge ownership, Phases K/N/R–U, and proposal
fusion all landed after it.

## Selected extraction SHA

The head of `goal/visionmcp-v2-ocular-os` at export time, recorded in
`artifacts/release/parent-export.json` together with the annotated tag
`visionmcp-parent-export-v2.1`.

## State at selection

- Suite: **914 passed, 5 failed, 37 skipped**. The five failures are deliberate
  and preserved; see `V2_1_CLOSURE.md`.
- Physical receipts: Blender 4.2.1 LTS, COLMAP 4.0.4 (no CUDA, dense MVS
  BLOCKED), Chrome 150 / Firefox 151 / WebKit 26.5.
- Sealed benchmarks 0 leakage failures; repair corpus 23/23; portfolio with
  radiance BLOCKED; remote with two named blockers.
- Ocular capability: **EXPERIMENTAL** per Bible §6.4.

## Missing pushes and untracked files

The parent export step verifies that no **required** artifact is untracked. One
such defect was already found and recorded during Phase A: the organic-fur
receipt was untracked while 390 sibling artifacts were committed, which meant a
clean checkout produced 322 real-runtime passes rather than the reported 323.
That class of defect is exactly what the export check exists to catch.
