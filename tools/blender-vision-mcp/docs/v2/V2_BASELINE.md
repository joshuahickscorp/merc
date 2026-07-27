# V2 baseline

The frozen starting point for VisionMCP V2. Everything here was measured on this
host before V2 work began; `artifacts/v2/baseline.json` is the machine-readable
form.

## Selected base

```
branch  codex/visionmcp-live-sandbox-acceptance
sha     84d8f9716e6663d970b58fcf2f827c9b0f994407
```

Authoritative because it is a strict superset: `feature/blender-vision-mcp` +37
commits and `goal/visionmcp-100plus-sandbox` +1, with no divergence. Choosing it
discards no accepted capability and requires no merge. See
`docs/v2/V1_RECOVERY_REPORT.md` for the branch survey.

## V2 branch

```
worktree  ~/Downloads/visionmcp-authority-worktrees/visionmcp-v2
branch    goal/visionmcp-v2-all-seeing-eye
base      84d8f971
```

## Environment

| Component | Version |
|---|---|
| Python | 3.13.13 |
| Blender | 4.2.1 LTS |
| Node | 20.17.0 |
| COLMAP | 4.0.4, built **without CUDA** |
| Chromium | 150.0.7871.183 (channel `chrome`) |
| Firefox | 151.0 |
| WebKit | 26.5 |
| Host | macOS arm64, 28 cores, 96 GB |

Added for V2: `scipy`, `scikit-image`, `trimesh`. No model weights are
downloaded and the runtime has no network access.

## Baseline suite results

| Suite | Result |
|---|---|
| Fast | 416 passed, 22 skipped |
| Blender real runtime | 9 passed, 1 skipped |
| Browser real runtime (Chromium + Firefox + WebKit) | 29 passed, 1 skipped |
| Process loss / application / adversarial | 17 passed |

Green, with two external blockers and one defect found and fixed during
recovery — the keyboard journey false pass, described in the recovery report.

## Frozen budgets

The delivery budgets from the master goal are frozen and may not be widened to
make a result pass. A violation is reported as a violation.

```
initial JS compressed   <=   300 KB
shell GLB               <= 1,500 KB
mobile shell            <=   650 KB
poster before GLB       required
detail                  chapter-gated
desktop median          >= 55 FPS      desktop p95  <= 24 ms
mobile median           >= 40 FPS      mobile p95   <= 35 ms
no recurring long task  >  50 ms during scroll
CLS                     <= 0.05
no five-minute memory growth
```

## Evaluation scale

The frozen 0–110 scale from Bible section 22 is used unchanged. No facet reaches
100 without implementation evidence, fresh runtime evidence, held-out or
external evidence, exact reproduction, stated limitations, failed-attempt
history, and a verifier receipt. 105 additionally requires three unseen targets;
110 additionally requires surviving adversarial full-runtime repair.

No score may be raised by redefining a reference class after the result is
known, and no score may be raised without current-head runtime evidence.

## External blockers at baseline

| Blocker | Blocks | Exact requirement |
|---|---|---|
| Owned Mac Studio BLEND fixture | one Blender integration test | `BVMCP_MAC_STUDIO_SCENE` pointing at the owned BLEND with SHA-256 `22ea2562cc92d44b2df084f0009b3faca6ab37f6ff81e21e55136ac6871e6dae` |
| COLMAP without CUDA | dense multi-view stereo | a CUDA-capable COLMAP build; sparse SfM runs on CPU and is used |
| No authorized real-animal capture set | Benchmark E real-subject half | an authorized multiview capture of a real animal, with rights cleared |
| No user-supplied consumer-object photographs | Benchmark B on the user's own object | authorized multiview photographs plus measurements; a governed self-captured fixture is used meanwhile and is never described as the user's object |

Each is recorded as `BLOCKED_EXTERNAL`. None is worked around by substituting a
weaker result and calling it a pass.

## Preserved user state

The production ComputExchange checkout at `~/Downloads/computexchange` holds
uncommitted user-owned work and is never touched by V2. `git add -A` is never
used; every commit stages an explicit path list. The repository's
`.git/info/exclude` contains a user-owned `tools/` entry, which is left in place;
V2 commits use `git add -f` on named paths instead.
