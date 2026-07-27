# VisionMCP V2.1 Ocular OS scorecard

Phase V machine-readable scorecard for the Bible §27 ocular facet set on the frozen
0–110 scale. This is an **honest scorecard for a subsystem quarantined EXPERIMENTAL
on measured evidence**. It is not a launch document.

- Schema: `ocular.final-scorecard/1`
- Git head: `3342df923901cc17a887b29578f7809072cc04d2`
- Branch: `goal/visionmcp-v2-ocular-os`
- Facets: **35**
- Mean baseline → final: **30.0 → 55.8** / 110
- Min / max final: **28 / 88**
- Facets ≥ 100: **0** (none — no declared-reference-class proof)
- Quarantine: **EXPERIMENTAL**
- Exact machine report: `artifacts/ocular/final-scorecard.json`
- Verifier: `scripts/verify-ocular-scorecard.py` → `artifacts/ocular/final-scorecard.receipt.json`

## Quarantine reason

Measured evidence shows a continuous perception scaffold with honest attestation and sealed isolation, but core loop qualities are weak: detection precision 0.196, surprise precision 0.0079, permanence 1/4, tracking suite fails 3 gates, 5/9 retina events fire on confounders, 8/9 film beats empty. Not a launch document.

## Scoring discipline

- No 100 without declared reference-class proof.
- No 105 without three unseen targets.
- No 110 without adversarial full-runtime repair.
- Never raise a score without current-head runtime evidence (receipt paths + digests).
- Unavailable hardware is BLOCKED, not failed, and must not be fabricated.
- Do not score a facet on capability that exists but does not work.
- Print bad numbers in the body of the scorecard, not only footnotes.

Scale:

```text
0–49   unsupported
50–69  partial
70–84  useful bounded
85–99  strong
100    proven for declared reference class
105    unchanged system passes three unseen targets
110    passes 105 plus adversarial full-runtime repair
```

## Headline measured numbers (not footnotes)

These are the numbers that define the subsystem's real state. They are all on disk.

| Metric | Value | Artifact |
| --- | ---: | --- |
| Detection recall | 0.384 | `artifacts/ocular/detection-quality-verify.json` |
| Detection precision | **0.196** | same |
| Matched IoU | 0.457 | same |
| Large-area blobs / frame | 0.00 (was 1.00) | same (pre-fix recall was 0.011) |
| Surprise precision | **0.0079 (~0.8%)** | `artifacts/ocular/prediction-calibration.json` |
| Surprises fired / true | 3551 / 28 | same |
| Visibility accuracy | 0.945 | same |
| Permanence sealed | **1 of 4** | `artifacts/ocular/permanence.json` |
| Tracking suite | **FAIL (3 gates)** | `artifacts/ocular/tracking/tracking_report.json` |
| Permanence ID switches | 22 (gate ≤12) | same |
| Crossing-paths ID switches | 14 (gate ≤12) | same |
| Retina confounder fires | **5 of 9** events | `artifacts/ocular/event-calibration.json` |
| MCP ocular tools | **19/19** listed and executed | `artifacts/ocular/mcp-callthrough.json` |
| Stream frames / drops | 48 / 0, monotonic | `artifacts/ocular/stream-receipt.json` |
| Webcam | **BLOCKED** (not fabricated) | stream + `retina/webcam_attestation.json` |
| Data-centre beats passed | **9 of 9** | `artifacts/ocular/beats/run-summary.json` |
| Sealed leakage failures | 0 / 8 targets | `artifacts/ocular/sealed/sealed.receipt.json` |
| Repair drills | 23/23 PASS | `artifacts/ocular/repair/repair.receipt.json` |

## Known-open failures (must not be hidden)

### detection-precision-low

Detection precision 0.196 and recall 0.384 on sealed hard suite (improved from recall 0.011 / large-area blobs 1.00/frame).

Values:

- `detection_precision`: `0.19577235422405878`
- `detection_recall`: `0.38375946969696967`
- `mean_matched_iou`: `0.4571001368154998`
- `mean_large_area_dets_per_frame`: `0.0`

Receipts:

- `artifacts/ocular/detection-quality-verify.json` sha256=`0548f8a5cea0…`

Gate relaxed: **False**

### tracking-suite-fails-three-gates

Perception-driven tracking suite FAILS: permanence id_switches=22>12, crossing_paths id_switches=14>12, unknown entrant absorbed.

Values:

- `permanence_id_switches`: `22`
- `crossing_paths_id_switches`: `14`
- `id_switch_threshold`: `12`

Receipts:

- `artifacts/ocular/tracking/tracking_report.json` sha256=`d1119b76d890…`

Gate relaxed: **False**

### permanence-one-of-four

Sealed permanence n_pass=1 of 4 questions; status FAIL.

Values:

- `n_pass`: `1`
- `n_questions`: `4`

Receipts:

- `artifacts/ocular/permanence.json` sha256=`8ab63e208f83…`

Gate relaxed: **False**

### surprise-is-noise

3551 surprises fired, 28 true positives, overall precision 0.007885 (~0.8%); verdict=noise.

Values:

- `total_surprises_fired`: `3551`
- `overall_surprise_tp`: `28`
- `overall_surprise_precision`: `0.007885102787947058`
- `visibility_accuracy`: `0.9448621553884712`

Receipts:

- `artifacts/ocular/prediction-calibration.json` sha256=`96d5d534f833…`

Gate relaxed: **False**

### retina-confounder-fires

5 of 9 retina event types fire on their confounder fixture (confounder_violation=true); unfit_event_types lists 5 events. Deliberate event tests rose from 3 to 6 failures after confounder fixtures were added — new truth, not a regression to hide.

Values:

- `n_events`: `9`
- `n_fixtures`: `36`
- `confounder_violations`: `5`

Receipts:

- `artifacts/ocular/event-calibration.json` sha256=`2146543827cb…`

Gate relaxed: **False**

### beats-eight-of-nine-empty

Data-centre beat coverage: 9/9 beats pass under PHYSICAL renders with zero failures. The earlier 1/9 was underexposure, not black renders — two lamps lighting a 19 m aisle, ~14x inverse-square falloff. Fixed by lighting the set; no gate was moved. Critics still fire generic_composition and procedural_sameness, so composition quality remains unproven.

Values:

- `beats_total`: `9`
- `scene_instances`: `843`
- `scene_racks`: `44`

Receipts:

- `artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…`

Gate relaxed: **False**

### no-physical-webcam

Webcam index 0 cannot be opened; execution_class=BLOCKED; fabricated_live_frame=false.

Receipts:

- `artifacts/ocular/retina/webcam_attestation.json` sha256=`54664d13fa76…`
- `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…`

Gate relaxed: **False**

### deliberate-failing-tests-8

Eight currently-failing tests are deliberate: 2 in test_ocular_beats.py and 6 in test_ocular_events.py. Events count rose from 3 to 6 because new confounder fixtures exposed real defects.

Values:

- `beats_deliberate_failures`: `2`
- `events_deliberate_failures`: `6`
- `total_deliberate_failures`: `8`

Receipts:

- `artifacts/ocular/event-calibration.json` sha256=`2146543827cb…`
- `artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…`

Gate relaxed: **False**

## External blockers

| Id | Exact requirement |
| --- | --- |
| `no-physical-webcam` | Host with openable physical webcam/camera so live streaming and live calibration leave BLOCKED under attestation without fabricating frames. |
| `three-unseen-real-targets` | Three unseen real-world targets with authorized captures, scored under frozen thresholds without manual implementation changes. |
| `no-authorized-real-animal-capture` | Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, and ethical-use attestation. |
| `user-supplied-consumer-object-photographs` | Authorized multiview photographs plus measurements of the user-owned consumer object; governed self-captured fixture is never described as the user's object. |
| `colmap-dense-mvs` | CUDA-capable COLMAP build (installed 4.0.4 is CPU-only so patch_match_stereo cannot run). |
| `human-art-direction` | Human art-director sign-off on flagship film lighting and framing. |

## Deliberate failing tests

Eight tests currently fail on purpose:

- **2** in `tests/test_ocular_beats.py` — empty/frustum mismatch beats must fail gates.
- **6** in `tests/test_ocular_events.py` — confounder silence (and related) checks.

The events deliberate-failure count **rose from 3 to 6** this session because new
confounder fixtures were added and exposed real detector defects. That is **new truth**,
not a regression to paper over. Thresholds were not relaxed.

## Facet ledger

| Facet | Baseline | Final | Δ | score_state | Implementation | Runtime (receipts) | External / held-out | Failures | Limitations | Reproduction | Remaining blocker |
| --- | ---: | ---: | ---: | --- | --- | --- | --- | --- | --- | --- | --- |
| sensor_calibration | 35 | 62 | +27 | scored | `src/blender_vision/ocular/calibration.py`<br>`src/blender_vision/ocular/sensors.py`<br>`docs/ocular/SENSOR_AND_CALIBRATION.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | Recorded-stream calibration uses image_centre_fallback with authority INFERRED; no metrology lab board on a live sensor. Checkerboard path is unit-exercised via vision.calibrate_sensor MCP tool. | Live webcam calibrate_sensor cannot run: webcam BLOCKED on this host. | Stream calibration method is image_centre_fallback, not a measured lens model.; No physical scale anchor without a ruler/credit-card reference (remote measurements mark scale unknown). | `.venv/bin/python scripts/run-ocular-stream.py --frames 48 --receipt artifacts/ocular/stream-receipt.json` | `no-physical-webcam`: Host with openable webcam (or authorized capture device) so calibrate_sensor can run on live frames rather than image-centre fallback. |
| live_streaming | 20 | 74 | +54 | scored | `src/blender_vision/ocular/stream.py`<br>`scripts/run-ocular-stream.py`<br>`docs/ocular/OCULAR_ARCHITECTURE.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48, 0, True]<br>`artifacts/ocular/retina/webcam_attestation.json` sha256=`54664d13fa76…`<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19, 19] | Physical recorded video_file stream: 48 frames emitted, 0 drops, timestamps monotonic, incremental consumption. Webcam probe attested BLOCKED without fabricating frames. | Earlier stream accounting bug reported 44 dropped frames that had actually been delivered (fixed; receipt now 0 drops). | Live camera path is BLOCKED on this host (no camera); recorded stream only.; fabricated_live_frame is false — absence is attested, not invented. | `.venv/bin/python scripts/run-ocular-stream.py --frames 48 --receipt artifacts/ocular/stream-receipt.json` | `no-physical-webcam`: Physical webcam present and openable so vision.open_stream(allow_webcam=true) can leave BLOCKED and emit live frames under PHYSICAL authority. |
| foveation | 15 | 58 | +43 | scored | `src/blender_vision/ocular/retina.py`<br>`src/blender_vision/ocular/gaze.py`<br>`docs/ocular/RETINA_AND_ATTENTION.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/retina/receipt.json` sha256=`9e4f6ca3a4c4…`<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | Stream receipt records fixation_count=48 on the recorded sequence; retina fixtures for camera_motion and object_motion were Blender-generated under RUNTIME_OBSERVED. | none listed | No sealed metric that foveal crops improve recognition vs full-frame compute.; Foveation machinery exists; quality of the foveal policy is not separately proven. | `.venv/bin/python scripts/run-ocular-retina.py --output artifacts/ocular/retina` | `foveation-quality-benchmark`: Sealed task showing information gain or accuracy lift from foveated compute vs uniform full-frame on held-out sequences. |
| attention | 15 | 60 | +45 | scored | `src/blender_vision/ocular/gaze.py`<br>`src/blender_vision/mcp/server.py`<br>`docs/ocular/RETINA_AND_ATTENTION.md` | `artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19, 19, 0]<br>`artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48] | vision.fixate and vision.saccade execute over stdio MCP (19/19 ocular tools). Stream processes 48 fixations with inhibition-of-return plumbing in gaze.py. | none listed | No sealed saliency-vs-task ranking benchmark with human or oracle labels.; Attention budget is implemented; optimality is not measured. | `.venv/bin/python scripts/verify-ocular-mcp-callthrough.py --output artifacts/ocular/mcp-callthrough.json` | `attention-task-ranking-benchmark`: Held-out sequences with task goals where fixation order is scored against an information-gain oracle or human gaze. |
| segmentation | 22 | 52 | +30 | scored | `src/blender_vision/ocular/segment.py`<br>`src/blender_vision/ocular/detect.py`<br>`src/blender_vision/ocular/proposals.py`<br>`scripts/measure-detection-quality.py` | `artifacts/ocular/detection-quality-verify.json` sha256=`0548f8a5cea0…` nums=[0.383759, 0.195772, 0.4571, 0, 11] | Proposal-fusion detection scored against sealed GT in evaluator role only, across 11 hard conditions at IoU 0.5. Detector never opens sealed_gt/. | Pre-fix detector returned full-frame table/shadow blobs: recall 0.011, matched IoU 0.020, large-area blobs 1.00/frame.; Containment-in-support-plane was treated as identity; fusion ranked by area. | detection_recall=0.38375946969696967 — still well under useful operating levels.; detection_precision=0.19577235422405878 (~0.196) — majority of detections are false.; mean_matched_iou=0.4571001368154998; mean_large_area | `.venv/bin/python scripts/measure-detection-quality.py --output artifacts/ocular/detection-quality-verify.json` | `detection-precision-low`: Raise sealed detection precision well above 0.196 and recall above ~0.38 on the 11-condition hard suite without GT leakage into the detector. |
| tracking | 25 | 55 | +30 | scored | `src/blender_vision/ocular/track.py`<br>`scripts/run-ocular-tracking.py`<br>`docs/ocular/TRACKING_AND_MEMORY.md` | `artifacts/ocular/tracking/tracking_report.json` sha256=`d1119b76d890…` nums=[0.909953, 0.735632, 22, 14]<br>`artifacts/ocular/detection-quality-verify.json` sha256=`0548f8a5cea0…` nums=[0.383759] | Perception-driven tracking on Blender Eevee hard suite (11 conditions). No GT-seeded boxes. IDF1 examples: visually_similar 0.910, crossing_paths 0.736, lighting_change 0.985, scale_change 0.481. | Suite FAILS three gates: permanence id_switches=22>12; crossing_paths id_switches=14>12; unknown entrant absorbed into existing identity.; Historical GT-seeded baseline looked better; that path is forbidden. | MOTA remains low on several conditions (e.g. full_occlusion ~0.198, scale_change ~0.200).; Fragmentation after detector fix raises ID switches — recorded, not retuned away. | `.venv/bin/python scripts/run-ocular-tracking.py --output artifacts/ocular/tracking` | `tracking-suite-fails-three-gates`: Perception-driven hard suite must pass ID-switch gates (permanence and crossing_paths <=12), refuse unknown absorption, and keep distractor false-reid false without |
| reidentification | 20 | 48 | +28 | scored | `src/blender_vision/ocular/track.py`<br>`docs/ocular/TRACKING_AND_MEMORY.md` | `artifacts/ocular/tracking/tracking_report.json` sha256=`d1119b76d890…` nums=[0.643357, 1]<br>`artifacts/ocular/permanence.json` sha256=`8ab63e208f83…` nums=[1, 4] | Distractor replacement condition id_switches=1; permanence distractor_refused passes (appearance-only re-id correctly refused). leave_return same-id recovery fails. | leave_return_same_id is false with multiple FP re-id events on return frames.; Permanence identity_preserved_on_return is false (0 same-id return frames of 7). | Appearance histogram re-id without strong kinematics fails under occlusion+return.; No learned re-id embedding; classical appearance only. | `.venv/bin/python scripts/run-ocular-tracking.py --output artifacts/ocular/tracking` | `reid-leave-return-fail`: Evidence-backed re-id that preserves identity across leave/return and full occlusion on the sealed permanence and leave_return conditions. |
| object_permanence | 15 | 38 | +23 | scored | `src/blender_vision/ocular/world.py`<br>`src/blender_vision/ocular/track.py`<br>`docs/ocular/WORLD_MODEL.md` | `artifacts/ocular/permanence.json` sha256=`8ab63e208f83…` nums=[1, 4, 14, 32]<br>`artifacts/ocular/tracking/tracking_report.json` sha256=`d1119b76d890…` nums=[22, 0.744681] | Sealed permanence evaluation on perception-derived tracks: n_pass=1 of n_questions=4, status FAIL. entity_count=14 over 32 frames. identity_provenance=perception (allow_ground_truth=false). | Only distractor_refused passes; identity_preserved_on_return, uncertainty_fell_after_reid, uncertainty_grew_while_occluded all fail.; Permanence tracking id_switches=22 against threshold 12. | Object permanence is implemented as a belief/track state machine but does not hold under sealed scoring.; Do not score this facet on capability that exists but does not work. | `PYTHONPATH=src .venv/bin/python -c "# see permanence runner used to emit artifacts/ocular/permanence.json"` | `permanence-one-of-four`: Sealed permanence questions must pass at least identity preservation on return and uncertainty growth while occluded without GT-seeded tracks. |
| dense_features | 20 | 50 | +30 | scored | `src/blender_vision/ocular/retina.py`<br>`src/blender_vision/ocular/detect.py`<br>`docs/ocular/RETINA_AND_ATTENTION.md` | `artifacts/ocular/detection-quality-verify.json` sha256=`0548f8a5cea0…` nums=[0.383759, 5.73864]<br>`artifacts/ocular/retina/receipt.json` sha256=`9e4f6ca3a4c4…` | Classical multiscale / appearance features drive proposal fusion (mean 5.74 dets/frame). No licensed dense foundation-model weights selected for PHYSICAL use. | Model intake leaves learned dense backends DOWNLOAD_REQUIRED / non-selectable without checkpoints. | Dense features are classical and bounded; not a universal embedding cortex.; Feature quality is only indirectly evidenced via detection/tracking metrics. | `.venv/bin/python scripts/measure-detection-quality.py --output artifacts/ocular/detection-quality-verify.json` | `no-licensed-dense-checkpoint`: Rights-cleared dense visual backbone with checkpoint on disk, intake receipt, and sealed uplift vs classical features. |
| depth | 30 | 46 | +16 | scored | `src/blender_vision/ocular/remote_loop.py`<br>`src/blender_vision/ocular/representation.py`<br>`docs/ocular/REMOTE_EYEBALL_REPORT.md` | `artifacts/ocular/remote/remote_loop.receipt.json` sha256=`90eac9cde9b3…` nums=[25, 6]<br>`artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…` | Remote loop builds perception-derived entities (entity_count=6) from 25 train images; portfolio emits mesh/point_cloud candidates. Depth is model/sensor-derived at best. | none listed | No dedicated ocular depth accuracy receipt (AbsRel/δ) against sealed depth GT.; COLMAP dense MVS remains blocked without CUDA (inherited V2 blocker). | `.venv/bin/python scripts/run-ocular-remote.py --output artifacts/ocular/remote` | `ocular-depth-accuracy-receipt`: Sealed depth evaluation (e.g. AbsRel, δ1) on ocular streams with declared sensor or multiview authority; CUDA COLMAP or other dense path if claimed. |
| camera | 35 | 55 | +20 | scored | `src/blender_vision/ocular/calibration.py`<br>`src/blender_vision/ocular/retina.py`<br>`docs/ocular/SENSOR_AND_CALIBRATION.md` | `artifacts/ocular/event-calibration.json` sha256=`2146543827cb…` nums=[1, 0]<br>`artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/retina/webcam_attestation.json` sha256=`54664d13fa76…` | CAMERA_MOVED event calibration: precision=1.0, recall=1.0, fpr=0.0 on sealed fixtures. Stream camera path is video_file with inferred intrinsics. | none listed | Live camera blocked; no physical extrinsics recovery from a moving handheld sensor.; Camera model in stream is image_centre_fallback. | `.venv/bin/python scripts/run-ocular-event-calibration.py --output artifacts/ocular/event-calibration.json` | `no-physical-webcam`: Live calibrated camera with measured intrinsics/extrinsics receipts under PHYSICAL authority. |
| geometry | 48 | 68 | +20 | scored | `src/blender_vision/ocular/representation.py`<br>`src/blender_vision/ocular/remote_loop.py`<br>`scripts/run-ocular-portfolio.py`<br>`docs/ocular/REPRESENTATION_PORTFOLIO.md` | `artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…`<br>`artifacts/ocular/remote/remote_loop.receipt.json` sha256=`90eac9cde9b3…` nums=[25, 6] | Portfolio PASS on 5 targets (datacenter, organic_fur, remote, soft_object, tabletop) with mesh/point_cloud/procedural executed; gaussian_radiance blocked without weights. Remote loop emits next-view p | Radiance/Gaussian blocked: no trained weights; network download forbidden. | Geometry is multi-representation scaffolding plus procedural/mesh candidates, not a measured reconstruction RMSE leaderboard for ocular streams.; User-owned remote photographs are still absent (governed fixture only). | `.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio` | `user-supplied-consumer-object-photographs`: Authorized multiview photographs of a user-owned object with scale reference; do not describe the governed fixture as the user's object. |
| temporal_prediction | 12 | 28 | +16 | scored | `src/blender_vision/ocular/predict.py`<br>`scripts/run-ocular-prediction-calibration.py`<br>`docs/ocular/PREDICTIVE_LOOP.md` | `artifacts/ocular/prediction-calibration.json` sha256=`96d5d534f833…` nums=[3551, 28, 0.0078851, 0.944862, 14142] | Hard-suite prediction calibration: total_surprises_fired=3551, overall_surprise_tp=28, overall_surprise_precision=0.007885102787947058 (~0.8%), visibility_accuracy=0.9448621553884712, total_prediction | Stream headline previously reported 441 predictions / 128 surprises with no correctness evidence; this artifact supplies that evidence. | Surprise precision ~0.8% means the predictive loop does not work as a truth-telling system.; Visibility prediction is relatively strong (0.945) but does not redeem surprise noise.; Do not score this facet as if the predi | `.venv/bin/python scripts/run-ocular-prediction-calibration.py --output artifacts/ocular/prediction-calibration.json` | `surprise-is-noise`: Overall surprise precision far above 0.0079 on sealed hard conditions with thresholds not fit to sealed labels. |
| world_memory | 28 | 58 | +30 | scored | `src/blender_vision/ocular/world.py`<br>`docs/ocular/WORLD_MODEL.md`<br>`docs/ocular/DYNAMIC_MEMORY_REPORT.md` | `artifacts/ocular/world/receipt.json` sha256=`62d07ef20294…` nums=[2, 18]<br>`artifacts/ocular/permanence.json` sha256=`8ab63e208f83…` nums=[1, 4]<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | World receipt: n_entities=2, n_belief_updates=18; dynamic_room change classes detected (new/removed/moved/lighting/same). MCP world tools execute. Permanence sealed 1/4. | Permanence FAIL dominates long-horizon trust.; World sequence_meta notes benchmarks/ocular_tabletop absent; driven from grouped fixtures. | Cross-session continuity is partially demonstrated on synthetic dynamic_room, not on live tabletop camera.; Entity beliefs are perception-derived; quality tracks detection/tracking weakness. | `.venv/bin/python scripts/run-ocular-world.py --output artifacts/ocular/world` | `permanence-one-of-four`: World memory that survives occlusion and leave/return under sealed permanence questions. |
| active_perception | 32 | 66 | +34 | scored | `src/blender_vision/ocular/active.py`<br>`src/blender_vision/ocular/remote_loop.py`<br>`docs/ocular/ACTIVE_PERCEPTION.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/remote/remote_loop.receipt.json` sha256=`90eac9cde9b3…` nums=[25, 6]<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | Stream next_best_view emits request_count=10 with uncertainty_before and information_gain fields; remote loop emits 6 next-view requests with expected_reduction values. vision.plan_capture / ask_next_ | none listed | Requests are ranked and emitted; closed-loop proof that following them reduces sealed uncertainty is limited.; Live guided capture requires a human+camera not present on this host. | `.venv/bin/python scripts/run-ocular-stream.py --frames 48 --receipt artifacts/ocular/stream-receipt.json` | `closed-loop-nbv-efficiency`: Measure views-to-target-uncertainty on sealed targets when acting on ask_next_view vs random/fixed baselines. |
| next_best_view_efficiency | 25 | 54 | +29 | scored | `src/blender_vision/ocular/active.py`<br>`docs/ocular/ACTIVE_PERCEPTION.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/remote/remote_loop.receipt.json` sha256=`90eac9cde9b3…` nums=[25] | Stream information_gain.expected_reduction=0.221665 with disagreement_reduction=0.2025; remote next_view_plan.request_count=6. Machinery for efficiency measurement exists. | none listed | No sealed views-to-threshold curve comparing active vs passive capture policies.; Efficiency is therefore partial: ranked requests, not proven sample-efficiency gains. | `.venv/bin/python scripts/run-ocular-remote.py --output artifacts/ocular/remote` | `closed-loop-nbv-efficiency`: Benchmark 10 (Bible §26): measure how many requested views are required to reach target uncertainty on sealed objects. |
| material_decomposition | 38 | 58 | +20 | scored | `src/blender_vision/ocular/sensitivity.py`<br>`docs/ocular/MATERIAL_LIGHT_SENSITIVITY.md` | `artifacts/ocular/sensitivity/summary.json` sha256=`496632d3920e…` nums=[41, 94, 135]<br>`artifacts/ocular/remote/remote_loop.receipt.json` sha256=`90eac9cde9b3…` nums=[25] | Sensitivity summary: n_authoritative=41, n_diagnostic=94, n_receipts=135, false_authoritative=false, critic_all_passed=true. Remote material hypotheses remain INFERRED. | V2 parity was near-blind to roughness (dE ~2.82–3.07 across 0.1→0.9); ocular sensitivity lane widens roughness response (authoritative delta_e2000 spans recorded in summary). | Material labels on remote are pixel hypotheses, not measured BRDFs.; Decomposition is sensitivity-proven more than inverse-render-solved. | `.venv/bin/python scripts/run-ocular-sensitivity.py --output artifacts/ocular/sensitivity` | `measured-brdf-or-inverse-render`: Sealed inverse-render or goniometric evidence separating material parameters on real objects, not only synthetic probe sensitivity. |
| lighting_separation | 36 | 56 | +20 | scored | `src/blender_vision/ocular/sensitivity.py`<br>`docs/ocular/MATERIAL_LIGHT_SENSITIVITY.md` | `artifacts/ocular/sensitivity/summary.json` sha256=`496632d3920e…` nums=[41, 135]<br>`artifacts/ocular/event-calibration.json` sha256=`2146543827cb…` nums=[1] | Authoritative sensitivity includes light_direction and exposure parameters; LIGHT_CHANGED event calibration precision=1.0, recall=1.0, fpr=0.0 on sealed fixtures. | none listed | Lighting separation is probe/event level, not full environment map recovery.; No adversarial relighting of a held-out real capture. | `.venv/bin/python scripts/run-ocular-sensitivity.py --output artifacts/ocular/sensitivity` | `environment-lighting-recovery`: Recover lighting that re-renders a held-out view within sealed error under PHYSICAL capture. |
| reflection_transparency | 28 | 42 | +14 | scored | `src/blender_vision/ocular/sealed.py`<br>`docs/ocular/SEALED_OCULAR.md` | `artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8, 0]<br>`artifacts/ocular/sensitivity/summary.json` sha256=`496632d3920e…` nums=[41] | Sealed framework includes reflective_transparent target with leakage probes PASS (failures=0, target_count=8). Sensitivity apparatus can stress specular metrics. | none listed | No dedicated ocular scorecard numbers for geometry/material/light separation on reflective/transparent objects (RMSE, confusion rates).; Sealed infrastructure is not the same as passing a quality gate for this facet. | `.venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed` | `reflective-transparent-quality-metrics`: Sealed quality metrics for reflection/transparency separation (not only split isolation) on Benchmark 3. |
| hard_surface | 42 | 58 | +16 | scored | `src/blender_vision/ocular/beat_coverage.py`<br>`src/blender_vision/ocular/representation.py`<br>`docs/ocular/DATACENTER_ART_DIRECTION_REPORT.md` | `artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…` nums=[843, 660]<br>`artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…`<br>`artifacts/ocular/beats/beat-00-receipt.json` sha256=`61fa8fcf16bf…` nums=[0.726192, 11.8618] | Data-centre scene has 843 instances / 44 racks / 660 drawers in beat 00, which passes with non_background_pixel_fraction=0.7262. Portfolio datacenter target executes mesh/procedural. | Beats 01-08 previously failed beat_minimums on near-empty renders; all nine now pass. | Hard-surface procedural world exists; art-direction and camera/lighting for most beats do not.; Critics flag generic composition and procedural sameness. | `See artifacts/ocular/beats/run-summary.json (physical beat coverage run)` | `beats-eight-of-nine-empty`: All nine data-centre beats pass visible-instance and non-background gates under PHYSICAL renders with art-direction review. |
| soft_object | 30 | 48 | +18 | scored | `src/blender_vision/ocular/representation.py`<br>`src/blender_vision/ocular/sealed.py`<br>`docs/ocular/REPRESENTATION_PORTFOLIO.md` | `artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…`<br>`artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8] | Portfolio includes soft_object target; sealed soft_object split isolation PASS. Mesh/point/procedural executed; radiance blocked. | none listed | No fold/leather topology quality metrics beyond portfolio scaffolding.; Soft-object perception is not a measured success — infrastructure only. | `.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio` | `soft-object-quality-metrics`: Sealed soft-object reconstruction/perception metrics (folds, topology, material) on Benchmark 7. |
| organic | 34 | 52 | +18 | scored | `src/blender_vision/ocular/representation.py`<br>`docs/ocular/REPRESENTATION_PORTFOLIO.md` | `artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…`<br>`artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8]<br>`artifacts/ocular/baseline.json` sha256=`94af6757a704…` nums=[28] | Portfolio organic_fur target PASS scaffolding; sealed organic_fur isolation PASS. Inherited V2 organic UV packing open failure (~29% vs 35% gate) remains. | uv-packing-branching-forms carried forward from V2 (~0.29 vs 0.35). | Organic geometry path is synthetic construction/portfolio, not a wild plant/animal capture.; Ocular loop does not add a new organic quality leaderboard. | `.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio` | `uv-packing-branching-forms`: UV packing efficiency >= 0.35 on organic_sculpture and plant (currently ~0.29; gate not relaxed). |
| hair_fur | 28 | 46 | +18 | scored | `src/blender_vision/ocular/representation.py`<br>`docs/ocular/MODEL_INTAKE.md` | `artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…`<br>`artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8]<br>`artifacts/ocular/baseline.json` sha256=`94af6757a704…` nums=[28] | organic_fur portfolio and sealed target exist. V2 baseline records synthetic fur lane (guides/guards) — no authorized real-animal capture. | none listed | Fur lane is synthetic only; real-animal capture is an external blocker.; No temporal groom/occlusion quality score for ocular streams. | `.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio` | `no-authorized-real-animal-capture`: Rights-cleared multiview stills/video of a real animal with OWNED or LICENSED rights_state and ethical-use attestation. |
| browser_perception | 40 | 55 | +15 | scored | `src/blender_vision/ocular/browser_eyeball.py`<br>`src/blender_vision/ocular/repair.py`<br>`docs/ocular/BROWSER_EYEBALL.md` | `artifacts/ocular/repair/repair.receipt.json` sha256=`342d7f990f1f…` nums=[23, 0]<br>`artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8]<br>`artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…` nums=[9] | Repair corpus: 23/23 drills PASS (12 PHYSICAL, 11 DIAGNOSTIC_ONLY), blocked_count=0, including browser-blank-first-frame, browser-dom-pixel-contradiction, browser-focus-trap. Sealed browser_page leaka | Beat-run browser probe failed: Playwright chromium_headless_shell executable missing at run time. | Browser eyeball works as a diagnostic/repair runtime more than as a continuous ocular stream source in the beat film run.; Pixel/DOM dual evidence is demonstrated in drills, not a long live session. | `.venv/bin/python scripts/run-ocular-repair.py --output artifacts/ocular/repair` | `browser-continuous-stream-session`: Continuous browser/screen stream under PHYSICAL playwright/chrome with sealed page tasks, not only diagnostic drills. |
| source_to_pixel | 36 | 58 | +22 | scored | `src/blender_vision/ocular/browser_eyeball.py`<br>`src/blender_vision/ocular/repair.py`<br>`docs/ocular/BROWSER_EYEBALL.md` | `artifacts/ocular/repair/repair.receipt.json` sha256=`342d7f990f1f…` nums=[23]<br>`artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8, 0] | Repair drills cover source_rendered_mismatch and DOM/pixel contradictions; sealed browser_page isolation prevents oracle leakage into builder. | none listed | Source-to-pixel is proven mainly via injected-fault repair drills, not production SPA coverage. | `.venv/bin/python scripts/run-ocular-repair.py --output artifacts/ocular/repair` | `production-spa-source-to-pixel`: Source-to-pixel chain on a real multi-route SPA with sealed expected render digests. |
| cinematic_composition | 22 | 58 | +36 | scored | `src/blender_vision/ocular/beat_coverage.py`<br>`src/blender_vision/ocular/critics_calibration.py`<br>`docs/ocular/DATACENTER_ART_DIRECTION_REPORT.md` | `artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…` nums=[843, 660]<br>`artifacts/ocular/beats/beat-00-receipt.json` sha256=`61fa8fcf16bf…` nums=[0.726192, 11.8618] | Beat coverage run-summary passed=true: 9 of 9 beats meet declared minimums, zero failures, execution_class PHYSICAL, non_background_pixel_fraction 0.611-0.958. Critics fire generic_composition on all beats and procedural_sameness on 01–08. | Originally 8 of 9 beats read empty/near-empty while frustum_instance_count ran into the hundreds; root-caused to lighting falloff and fixed. | Cinematic path is not art-directed; composition scores remain template-like.; Do not treat instance counts as beat coverage. | `Physical beat coverage → artifacts/ocular/beats/run-summary.json` | `beats-eight-of-nine-empty`: Every data-centre beat populated and art-directed; human art-director sign-off still required for 100-class claims. |
| aesthetic_criticism | 30 | 64 | +34 | scored | `src/blender_vision/ocular/critics_calibration.py`<br>`docs/ocular/MATERIAL_LIGHT_SENSITIVITY.md` | `artifacts/ocular/sensitivity/critic_calibration.json` sha256=`14642a103465…`<br>`artifacts/ocular/sensitivity/summary.json` sha256=`496632d3920e…` nums=[41]<br>`artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…` nums=[9] | critic_calibration.json all_passed=true on positive/near_threshold/negative/confounder/false_positive_check cells; sensitivity.critic_all_passed=true. Critics also fire on beat film. | none listed | Critic calibration is on synthetic matrix cells, not a human taste panel.; Critics correctly flag bad beats; they do not fix composition. | `.venv/bin/python scripts/run-ocular-sensitivity.py --output artifacts/ocular/sensitivity` | `human-art-direction`: Human art-director adjudication paired with critic scores on flagship film beats. |
| real_time_latency | 18 | 48 | +30 | scored | `src/blender_vision/ocular/stream.py`<br>`scripts/run-ocular-stream.py` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48, 0]<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | Stream processes 48 frames incrementally with 0 drops and monotonic timestamps; MCP callthrough duration_s≈1.733 for 19 tools. Incremental=true, loaded_wholesale=false. | none listed | No hard real-time SLA (ms/frame budget, p95 latency) receipt for the full ocular loop.; Latency is only weakly evidenced by successful incremental processing. | `.venv/bin/python scripts/run-ocular-stream.py --frames 48 --receipt artifacts/ocular/stream-receipt.json` | `latency-sla-receipt`: Publish per-stage p50/p95 latency on a declared frame rate with drop policy under PHYSICAL load. |
| physical_run_authority | 45 | 84 | +39 | scored | `src/blender_vision/ocular/attestation.py`<br>`src/blender_vision/ocular/verdict.py`<br>`docs/ocular/OCULAR_ARCHITECTURE.md` | `artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8, 0]<br>`artifacts/ocular/repair/repair.receipt.json` sha256=`342d7f990f1f…` nums=[23, 0]<br>`artifacts/ocular/retina/webcam_attestation.json` sha256=`54664d13fa76…`<br>`artifacts/ocular/baseline.json` sha256=`94af6757a704…` nums=[720] | Attestation law: webcam BLOCKED without fabricated frames; sealed and repair lanes run under PHYSICAL/DIAGNOSTIC classes without fallback physical PASS. Baseline records fallback audit: physical_pass_ | none listed | Authority discipline is strong; many perceptual qualities remain weak.; DIAGNOSTIC_ONLY drills must never be narrated as PHYSICAL product proof. | `.venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed` | none |
| leakage_resistance | 50 | 88 | +38 | scored | `src/blender_vision/ocular/sealed.py`<br>`docs/ocular/SEALED_OCULAR.md` | `artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8, 0]<br>`artifacts/ocular/prediction-calibration.json` sha256=`96d5d534f833…` nums=[3551]<br>`artifacts/ocular/detection-quality-verify.json` sha256=`0548f8a5cea0…` nums=[11] | Sealed receipt status=PASS, failures=0, target_count=8; leakage_matrix probes all blocked as required (builder cannot read oracle/canary; single split authority). Detection and prediction paths refuse | none listed | Leakage resistance is about split isolation, not about high perception scores.; Canary tests pass; quality tests may still fail honestly. | `.venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed` | none |
| metric_sensitivity | 34 | 72 | +38 | scored | `src/blender_vision/ocular/sensitivity.py`<br>`docs/ocular/MATERIAL_LIGHT_SENSITIVITY.md` | `artifacts/ocular/sensitivity/summary.json` sha256=`496632d3920e…` nums=[41, 94, 135]<br>`artifacts/ocular/sensitivity/critic_calibration.json` sha256=`14642a103465…` | n_authoritative=41 sensitivity receipts, n_diagnostic=94, n_receipts=135, physical_blocked=[], false_authoritative=false. Gates that always pass are worthless; this lane records spans against threshol | Inherited V2 roughness-blindness in browser parity is a separate known-open; ocular sensitivity probes improve discrimination on Cycles/physical probes. | Sensitivity is measured on controlled probes, not arbitrary in-the-wild photos. | `.venv/bin/python scripts/run-ocular-sensitivity.py --output artifacts/ocular/sensitivity` | `parity-roughness-sensitivity`: Browser/WebGL parity path must also discriminate roughness with high dynamic range (V2 dE sweep near 2.82–3.07 remains open where applicable). |
| editability | 40 | 60 | +20 | scored | `src/blender_vision/ocular/representation.py`<br>`docs/ocular/REPRESENTATION_PORTFOLIO.md` | `artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…` | Portfolio purpose_selections choose procedural for editable_geometry/animation and mesh for measurement/web on datacenter; radiance blocked rather than silently substituted. | none listed | Editability is purpose routing among representations, not a DCC round-trip quality suite. | `.venv/bin/python scripts/run-ocular-portfolio.py --output artifacts/ocular/portfolio` | `dcc-roundtrip-editability`: Round-trip edit in Blender/DCC with sealed geometric/material diff gates after export/import. |
| web_delivery | 40 | 56 | +16 | scored | `src/blender_vision/ocular/beat_coverage.py`<br>`docs/ocular/DATACENTER_ART_DIRECTION_REPORT.md` | `artifacts/ocular/beats/run-summary.json` sha256=`9ea5af826ceb…` nums=[843, 660]<br>`artifacts/ocular/portfolio/portfolio.receipt.json` sha256=`3b85a94df729…` | Beat run budgets.violations=[] against frozen shell/mobile/js budgets; portfolio selects mesh for web purpose. Browser launch for beat film failed (Playwright binary missing). | Browser gate failure in beat summary (playwright chromium missing). | Budget compliance is not the same as a beautiful, complete film delivery.; 8/9 beats fail visual density — web shell budgets can pass while content fails. | `artifacts/ocular/beats/run-summary.json budgets section` | `beats-eight-of-nine-empty`: Web delivery of a fully populated, reviewed data-centre film with browser runtime green. |
| uncertainty | 42 | 68 | +26 | scored | `src/blender_vision/ocular/world.py`<br>`src/blender_vision/ocular/active.py`<br>`docs/ocular/WORLD_MODEL.md` | `artifacts/ocular/stream-receipt.json` sha256=`3e3832a5f597…` nums=[48]<br>`artifacts/ocular/world/receipt.json` sha256=`62d07ef20294…` nums=[2, 18]<br>`artifacts/ocular/mcp-callthrough.json` sha256=`d6679266a78b…` nums=[19] | Stream lists per-entity uncertainties; world belief updates=18; vision.list_uncertainties and measure_information_gain execute. Permanence correctly refuses to claim uncertainty behaviour it does not  | uncertainty_grew_while_occluded and uncertainty_fell_after_reid fail sealed checks. | Uncertainty is reported; its calibration against outcomes is weak (see surprise ECE/noise). | `.venv/bin/python scripts/run-ocular-stream.py --frames 48 --receipt artifacts/ocular/stream-receipt.json` | `uncertainty-calibration`: Calibrated uncertainty (ECE/reliability) tying reported sigmas to sealed error rates across conditions. |
| generalization | 20 | 34 | +14 | scored | `src/blender_vision/ocular/sealed.py`<br>`docs/ocular/SEALED_OCULAR.md`<br>`docs/ocular/MODEL_INTAKE.md` | `artifacts/ocular/sealed/sealed.receipt.json` sha256=`e69a64712ebc…` nums=[8]<br>`artifacts/ocular/tracking/tracking_report.json` sha256=`d1119b76d890…` nums=[11]<br>`artifacts/ocular/baseline.json` sha256=`94af6757a704…` nums=[28] | Eight sealed synthetic/procedural targets and eleven hard tracking conditions exist. No three unseen real-world authorized targets. No score reaches 100/105/110. | none listed | Generalization is unsupported at 105-class: lacks three unseen real targets.; All strong numbers are on procedural/Blender fixtures or governed self-captured remote. | `.venv/bin/python scripts/run-ocular-sealed.py --output artifacts/ocular/sealed` | `three-unseen-real-targets`: Three unseen real-world targets with authorized captures, scored under frozen thresholds without manual implementation changes. |

## Facets unknown or blocked

No facet is scored `unknown` or fully `blocked` as a score_state.
Live-camera work is **BLOCKED as hardware** and recorded on the facets that need it
(`sensor_calibration`, `live_streaming`, `camera`) via `remaining_blocker: no-physical-webcam`,
while recorded-stream evidence remains scoreable. That is deliberate: absence of a webcam
is not a software failure and must not be fabricated into a pass or a fake fail.

## What works vs what does not

**Scoreable and real:**

- Nineteen ocular MCP tools list **and execute** over stdio (not list-only).
- Recorded stream: 48 frames, monotonic timestamps, 0 drops, incremental.
- Webcam absence attested BLOCKED without fabricated live frames.
- Sealed split isolation: 8 targets, 0 leakage failures.
- Repair corpus: 23/23 drills pass under PHYSICAL or DIAGNOSTIC_ONLY.
- Detection no longer emits full-frame background blobs (0.00/frame; was 1.00).
- Material/light sensitivity lane emits authoritative receipts; critic calibration matrix passes.

**Implemented but not working well enough to celebrate:**

- Predictive surprise is **noise** (precision 0.8%).
- Object permanence sealed score is **1 of 4**.
- Tracking suite still **FAIL**s three gates after perception-driven scoring.
- Detection precision **0.196**.
- Five of nine retina events fire on their own confounder.
- Eight of nine data-centre beats are empty in the render despite full frustums.

## Verifier

```text
.venv/bin/python scripts/verify-ocular-scorecard.py
```

Last receipt: passed=True facets=35 numeric_claims=114 receipts=82 scorecard_sha256=98681a36487e4987…

The verifier **fails** if any numeric claim is absent from its receipt (poison test required
whenever this document is regenerated).

## Honest judgement

Ocular OS is a real continuous-perception **scaffold** with strong process hygiene (attestation, sealed splits, MCP call-through, honest BLOCKED webcam) and weak perceptual competence on the qualities that would make it an eyeball: identity, permanence, calibrated surprise, and film/art direction. The right publication state is EXPERIMENTAL quarantine on measured evidence — not a capability launch, and not a claim that nineteen tools imply a working predictive world model.

