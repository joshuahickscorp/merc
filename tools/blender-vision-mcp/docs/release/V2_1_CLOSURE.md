# V2.1 Ocular closure

Master Goal §3 requires every remaining Ocular lane to be either **FIXED** or
**MOVED TO EXPERIMENTAL**, never silently promoted.

**Decision: MOVE TO EXPERIMENTAL.**

Bible §6.4 states it plainly: if the release-critical Ocular gate does not pass,
ship the Ocular profile as experimental and say so. It does not pass, and the
margin is not close.

## The gate, measured

| Gate | Required | Measured | Pass |
|---|---|---|---|
| proposal recall | ≥ 0.90 | mean **0.648** (0.33–0.97) | no |
| proposal precision | ≥ 0.80 | **0.12–0.39** | no |
| stationary first-frame detection | 3 of 3 | **2 proposals, 0 true positives** | no |
| unknown entrant F1 | ≥ 0.80 | unknown source recall **0.03** | no |
| IDF1 | ≥ 0.85 | **0.09–0.46** | no |
| MOTA | ≥ 0.75 | **0.09–0.46** | no |
| occlusion lifecycle accuracy | ≥ 0.90 | OCCLUDED never entered; uncertainty flat at 0.05 | no |
| leave-and-return re-identification | ≥ 0.90 | false | no |
| distractor rejection | ≥ 0.90 | no tracks formed on that condition | no |
| runtime ground-truth fields | 0 | **0** | **yes** |

The proposal lane's own verdict, issued through `issue_verdict`, is
`DIAGNOSTIC` with the reason recorded. It did not report a pass, which is the
verdict authority working as designed.

Per-source recall shows two sources contributing almost nothing: geometry is
**0.00 across every condition** (no depth pass wired, correctly reported rather
than invented), and unknown-region is **0.03**. An ensemble whose members
contribute nothing is not an ensemble yet.

## What did close

- **Runtime ground truth is gone.** `_detection_from_gt` deleted, `VisualTrack`
  carries no `ground_truth_id`, detections are scanned for banned oracle keys,
  and the tracker asserts no track holds a GT field. Ground truth exists only
  inside the sealed evaluator. This is the one gate that passes, and it is the
  gate that made every other number honest.
- **Physical verdict authority.** `issue_verdict()` is the single path to a
  physical verdict, requiring seven conditions together. Thirteen guards, each
  proven to bite against a reconstructed historical artifact.
- **No substitute physical PASS.** Proven by rebuilding the exact
  `synthetic_sequence` attestation pair that previously reported PASS.
- **No fabricated hardware blame.** The injected `metal_is_supported` /
  `WM_init` text is removed and guarded repository-wide.
- **Merge ownership.** `scripts/merge-ownership.json`; the two merges since have
  clobbered nothing, after two incidents that silently reverted accepted work.
- **Sealed benchmarks** — 0 leakage failures, builder denied a hidden view
  index, canaries clean.
- **Repair corpus** — 23 of 23 drills detected, repaired, verified, no global
  regression.
- **Representation portfolio** — radiance honestly BLOCKED, so
  `photoreal_view_synthesis` resolves to `null` rather than being filled by a
  representation that cannot serve it.

## What is not closed

Perception-derived tracking quality; object permanence through full occlusion;
unknown entrants creating new identities; retina confounder discrimination
(three detectors still fire on their confounders); the data-centre beat metric
correction and the separate art-direction repair; Ocular MCP tool registration
(2 of 19 at last measurement); and continuous recorded-stream end-to-end proof.

## The five failing tests stay failing

```
tests/test_ocular_beats.py::test_unique_mesh_count_stays_bounded_as_instances_rise
tests/test_ocular_beats.py::test_flagship_second_aisle_populated_and_path_clears_solids
tests/test_ocular_events.py::test_confounder_silent[OBJECT_ENTERED]
tests/test_ocular_events.py::test_confounder_silent[OBJECT_LEFT]
tests/test_ocular_events.py::test_confounder_silent[OBJECT_OCCLUDED]
```

They are not skipped, xfailed, deleted, or quarantined out of the suite. They
travel into the public tree still failing, and they are named in `LIMITATIONS`.
"Quarantine" here means the **capability** is labelled experimental — it does
not mean hiding the evidence.

The honest baseline is preserved at tag `ocular-honest-baseline-270ee69`, pushed
to the remote, with 511 artifact files recording the failures. Nothing in this
closure overwrites it.

## Consequence for the public release

The Ocular profile ships **experimental** and is labelled as such in the README,
the capability ledger, the demo output, and the release notes. Suite at this
point: **914 passed, 5 failed, 37 skipped**.
