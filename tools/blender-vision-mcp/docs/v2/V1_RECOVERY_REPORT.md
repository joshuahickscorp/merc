# V1 recovery report

Phase A of the VisionMCP V2 master goal. What existed before V2 work began,
which branch is authoritative, and what state the recovered baseline is in.

## Branch survey

Three branches carry VisionMCP work. All remotes were fetched before the survey.

| Branch | Head | Committed |
|---|---|---|
| `feature/blender-vision-mcp` | `e0f8df67` | 2026-07-25 19:18 |
| `goal/visionmcp-100plus-sandbox` | `75fc1c93` | 2026-07-26 01:10 |
| `codex/visionmcp-live-sandbox-acceptance` | `84d8f971` | 2026-07-26 03:43 |

The history is **linear**, not divergent:

```
git rev-list --left-right --count feature...100plus   ->  0  36
git rev-list --left-right --count feature...live      ->  0  37
git rev-list --left-right --count 100plus...live      ->  0   1
```

`git merge-base` confirms `feature` is the common ancestor of both, and
`100plus` is the ancestor of `live`.

### Selected base

**`codex/visionmcp-live-sandbox-acceptance` @ `84d8f971`.**

It is authoritative because it is a strict superset. Every commit on the other
two branches is reachable from it, so choosing it discards no accepted
capability and requires no merge. The goal's instruction not to discard accepted
capability merely because it lives on another branch is satisfied by
construction rather than by judgement.

The 37 recovered commits cover, in order: the 100-plus baseline freeze,
executable capability scoring, application authority graphs and completeness
gates, governed application compilation with source-byte verification, the
five-case application benchmark, TypeScript compiler indexing and frontend
typecheck repair, cross-engine browser verification including the WebKit focus
work, GLB structural validation and asset production preparation, fixed-camera
appearance authority, measured bounded performance repair, real process-loss
recovery, adversarial acceptance enforcement, the sealed NOCTURNE ONE app+3D
benchmark with its H3 pipeline and H4 negative result, the fixed failure-corpus
repair run, and the published 100-plus scorecard.

### V2 branch

```
worktree  /Users/scammermike/Downloads/visionmcp-authority-worktrees/visionmcp-v2
branch    goal/visionmcp-v2-all-seeing-eye   (from 84d8f971)
```

## Dirty checkouts preserved

| Checkout | Branch | Dirty paths | Action |
|---|---|---|---|
| `~/Downloads/computexchange` | `release/rc1-go-closure` | 213 | untouched |
| `~/Downloads/computexchange-worktrees/live-instrument` | `design/computexchange-live-instrument` | 2 | untouched |
| `~/Downloads/visionmcp-authority-worktrees/visionmcp-100plus-sandbox` | `goal/visionmcp-100plus-sandbox` | 0 | untouched |
| `~/Downloads/visionmcp-authority-worktrees/visionmcp-live-sandbox-acceptance-*` | `codex/…-acceptance` | 0 | untouched |

The production ComputExchange checkout holds 213 uncommitted user-owned paths.
Nothing in V2 touches it. `git add -A` was never used; every commit stages an
explicit path list.

One repository-local detail worth recording: `.git/info/exclude` contains a
`tools/` entry, which hides new files under `tools/` from `git status`. That
exclude is user-owned and was left in place; V2 commits use `git add -f` on
explicit paths instead.

## Recovered baseline state

All suites were executed on the V2 worktree at the recovered head.

| Suite | Result |
|---|---|
| Fast suite | **416 passed, 22 skipped, 0 failed** |
| Blender real runtime | **9 passed, 1 skipped** |
| Browser real runtime (Chromium + Firefox + WebKit) | **29 passed, 1 skipped** |
| Process loss / application / adversarial | **17 passed** |

Environment: Python 3.13.13, Blender 4.2.1 LTS, Node 20.17, COLMAP 4.0.4
(CPU-only), Chrome 150 / Firefox 151 / WebKit 26.5, macOS arm64, 28 cores, 96 GB.

## The one baseline failure, and what it actually was

The first browser-benchmark run failed on the Firefox engine:

```
assert statuses["firefox"] in {"PASS", "BLOCKED_EXTERNAL"}
AssertionError: assert 'FAIL' in {'BLOCKED_EXTERNAL', 'PASS'}
```

The tempting reading is "macOS Firefox does not tab to buttons by default", which
would make it an environment quirk. Instrumenting the real engines showed
something worse.

- **Firefox** traversed `#menu-trigger → #tab-one → #tab-two → #network-probe`
  and then parked on the last control, because headless Firefox has no browser
  chrome to hand focus back to. The journey never terminated, so it timed out as
  `BOUNDED`.
- **WebKit**, on the richer experience fixture, reached `#email` — one text field
  — and stopped. The next Tab returned `#email` again, which matched
  `first_selector`, so the journey reported **`COMPLETE_CYCLE` after covering 1
  of 7 focusable controls**. That was a false pass in the accepted baseline.

The root cause is shared: the journey had no notion of *document coverage*. It
terminated on repeat-of-first or on a body sentinel, and the existing WebKit
Option-Tab escalation fired only when the engine emitted body sentinels first.

### Fix

`src/blender_vision/perception/browser.py`:

1. Enumerate the document's focusable set before traversing.
2. Escalate WebKit to Option-Tab whenever plain Tab stalls **and** controls remain
   unreached, not only on body sentinels.
3. Add a terminal state for focus parked on the last control.
4. Downgrade any traversal that terminates with unreached controls to a new
   **`FOCUS_TRAPPED`** status.
5. Launch Firefox with `accessibility.tabfocus=7`, the documented all-elements
   mode that mirrors macOS Full Keyboard Access.

This is a strengthening, not a relaxation: it *adds* a failure state that did not
previously exist and no threshold was widened. A focus trap — a modal pinning
focus so the rest of the page is unreachable — was previously indistinguishable
from a completed journey; it now fails loudly.

### Verification

`tests/fixtures/web/focus_trap/` is a deliberate trap. Measured across all three
real engines:

| Fixture | Chromium | Firefox | WebKit |
|---|---|---|---|
| `experience` | COMPLETE_DOCUMENT 7/7 | COMPLETE_DOCUMENT 7/7 | COMPLETE_DOCUMENT 7/7 (Tab→Alt+Tab) |
| `static` | COMPLETE_DOCUMENT 1/1 | COMPLETE_CYCLE 1/1 | COMPLETE_DOCUMENT 1/1 |
| `focus_trap` | **FOCUS_TRAPPED** 1/3 | **FOCUS_TRAPPED** 1/3 | **FOCUS_TRAPPED** 1/3 |

Browser benchmark after the fix: chromium PASS, firefox PASS, webkit PASS, all
four profiles PASS, no external blockers.

## Current tool surface (V1, recovered)

34 `vision.*` tools plus the `system.*`, `campaign.*`, `role.*`, `context.*` and
`workflow.*` families, registered in `src/blender_vision/mcp/server.py`:

```
resolve_target observe capture_state discover_states query explain_region
trace_behavior analyze_motion inspect_graphics compare reconstruct
transplant_feature repair evaluate verify progress review_queue run adapters
compare_backends solve_cameras refine_camera compare_camera_solutions
consolidate_camera_solutions import_camera_solution review_camera_solution
solve_calibration_board solve_vanishing_points solve_pnp_landmarks
propose_pnp_landmarks propose_pnp_landmarks_from_renders review_pnp_landmarks
import_feature_detections import_geometry_evidence
```

48 V1 JSON schemas under `schemas/`. 182 source modules, 92 test files.

## Current limitations carried into V2

From the recovered docs and confirmed by inspection, the weak areas are:
single-image complete geometry, hidden-geometry recovery, arbitrary-photo
reconstruction, organic and anatomical modelling, hair and fur, retopology and
UV authority, texture reconstruction, material separation from lighting, lighting
and environment estimation, reflection matching, autonomous finished-asset
delivery, and general app generation from visual evidence alone.

These are exactly the V2 target areas.

## External blockers at baseline

| Blocker | Blocks | Requirement |
|---|---|---|
| Owned Mac Studio BLEND fixture | one Blender integration test | `BVMCP_MAC_STUDIO_SCENE` with SHA-256 `22ea2562…e6dae` |
| COLMAP built without CUDA | dense multi-view stereo | a CUDA-capable COLMAP build; sparse SfM runs fine on CPU |

Both are recorded as `BLOCKED_EXTERNAL`. Neither was introduced by V2 and neither
is worked around by substituting a weaker result.

## Baseline verdict

The recovered baseline is green on every suite, with two explicit external
blockers and one real defect found and fixed. V2 work proceeds from
`4bbb975d` on `goal/visionmcp-v2-all-seeing-eye`.
