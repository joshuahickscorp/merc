# V2 recovery and freeze

Phase A of the Ocular OS master goal. The previous run's report is not taken on
trust; every number below was re-executed on this host in a fresh worktree.

## Authority verified

| Claim | Verified |
|---|---|
| Accepted SHA `247431e2c0d70174b7b043caafee6edaf55c5809` | exists, and **is** the branch head exactly |
| Branch `goal/visionmcp-v2-all-seeing-eye` | head equals the accepted SHA |
| Commits after the accepted SHA | none |

```
worktree  ~/Downloads/visionmcp-authority-worktrees/visionmcp-v2-ocular
branch    goal/visionmcp-v2-ocular-os   (from 247431e)
```

Production checkout `~/Downloads/computexchange` remains on
`release/rc1-go-closure` and is not written to. It carries 203 user-owned dirty
paths, and a separate session of the user's was observed running `make` and
`rsync` there during this work; none of that is ours.

## Re-executed results

| Suite | Reported by V2 | Reproduced here |
|---|---|---|
| Fast | 720 | **720 passed, 33 skipped, 0 failed** |
| Real runtime (Blender + COLMAP) | 323 | **322 passed, 1 failed** from a clean checkout; **323 passed** after the step below |
| Release verifier | valid | `"valid": true` |
| Scorecard verifier | PASS | **VERIFY PASS**, 28 facets, 128 numeric claims, 62 receipts |

Blender 4.2.1 LTS, COLMAP 4.0.4 (no CUDA), Chrome 150, Firefox 151, WebKit 26.5,
Python 3.13.13, macOS arm64, 28 cores, 96 GB.

## Reproducibility defect found in the accepted baseline

`tests/test_v2_organic.py::test_real_organic_lane_receipt_is_present_and_passed`
asserts on `artifacts/v2/organic/organic-fur-receipt.json`. That receipt is
**not tracked in git** — 390 files under `artifacts/v2/` are committed, and this
one is not.

The consequence is that the accepted SHA does not produce 323 real-runtime
passes from a clean checkout. It produces 322 passes and one failure, and only
reaches 323 after `scripts/run-organic-fur-lane.py` has been run locally to
generate the receipt.

This is not a fabricated number: the test fails loudly with the exact command to
run, rather than skipping or passing vacuously, which is the correct behaviour.
But the headline "323 real-runtime tests" carried an unstated precondition.
Recorded here so the ocular baseline starts from the reproducible figure.

The lane was executed physically in this worktree to close it:

```
guides=642 guard=3852 undercoat=6420   critique passed: True
exit=1 — two known-open uv_packing gates (see below)
```

After that run the organic test passes 13/13 and the physical suite reaches 323.

## Open failures carried forward, unchanged

These are inherited from V2 and are **not** reset by this recovery.

1. **UV packing ~29% against a 35% gate** on the branching sculpture and the
   plant. The lane exits non-zero on exactly these two gates, and the test pins
   the open set so any new failure is caught. The Ocular goal forbids relaxing
   the 35% target to pass.
2. **Material parity is near-blind to roughness.** Sweeping browser roughness
   0.1 → 0.9 moves dE2000 only 2.82 → 3.07 and structural 0.045 → 0.051; every
   step passes. The gate catches gross material error and cannot be cited as
   evidence a roughness estimate is right. Phase L must fix or honestly
   downgrade it.
3. **Data-centre second aisle holds 8 of 464 instances.** Beats 05–08 play
   against a bare corridor. A high global instance count is not beat coverage.
4. **COLMAP dense MVS unavailable** — the installed 4.0.4 has no CUDA build.
   Sparse SfM runs; dense is BLOCKED.
5. **No authorized real-animal capture.** The fur lane is synthetic only.
6. **No user-supplied consumer-object photographs.** The fixture is
   self-captured and governed, and is never described as the user's object.
7. **Owned Mac Studio BLEND fixture absent** — one Blender test skips with a
   documented SHA-256 requirement.

## Fallback audit (Phase B)

Three modules contain substitution paths:
`procedural/emit.py`, `procedural/mesh_offline.py`, `benchmarks/objects.py`.

`scripts/run-procedural-engine.py` already appends a failure when the Blender
backend is blocked, so the offline path cannot reach `ALL ASSERTIONS PASSED`.
That property was previously incidental. It is now structural:

`src/blender_vision/ocular/attestation.py` adds `RuntimeAttestation` and
`ExecutionClass`. A substitute is `DIAGNOSTIC_ONLY` or `CANDIDATE_ONLY`; an
unavailable runtime is `BLOCKED`; only a real execution is `PHYSICAL`. The three
physical verdicts — `PHYSICAL_PASS`, `HARDWARE_VERIFIED`,
`REAL_RUNTIME_VERIFIED` — are unreachable from anything else, and a
`DIAGNOSTIC_ONLY` attestation cannot carry `RUNTIME_OBSERVED` authority.

`classify_failure()` closes the second half of the V2 defect. Path, dependency,
API and script signatures are matched **before** hardware, so the relative
`--python` path that V2 published as "Metal SIGSEGV during WM_init" now
classifies as `PATH_ERROR`, while a genuine `MTLBackend::metal_is_supported`
crash classifies as `HARDWARE_ERROR`. A bare crash with no other signal stays
`UNCLASSIFIED` rather than becoming a hardware accusation.

## Process discipline

`scripts/with-one-browser.sh` serializes all Playwright work behind one lock
directory and reaps helper processes on `EXIT`, `INT`, `TERM` and `HUP`. It
fails the run when more than three engines survive. The V2 session leaked ten
engines from interrupted runs; the trap now covers that path.
