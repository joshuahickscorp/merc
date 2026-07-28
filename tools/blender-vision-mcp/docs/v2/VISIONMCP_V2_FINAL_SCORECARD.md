# VisionMCP V2 final scorecard

Phase R machine-readable scorecard for the Bible 22.1 facet set on the frozen
0–110 scale. Scores use **only** evidence present at the current head.
Inflated scores are the failure mode this document exists to avoid.

- Schema: `v2.final-scorecard/1`
- Git head: `e1f22d8ca5db327abf8ca09073e28a2d38c00fb7`
- Branch: `goal/visionmcp-v2-all-seeing-eye`
- Facets: **28**
- Mean baseline → final: **59.89 → 80.89** / 110
- Min / max final: **62 / 92**
- Facets ≥ 100: **0** (none — no declared-reference-class proof for 100)
- Exact machine report: `artifacts/v2/final-scorecard.json`
- Verifier: `scripts/verify-final-scorecard.py` → `artifacts/v2/final-scorecard.receipt.json`

## Scoring discipline

- No score reaches 100 without declared-reference-class proof.
- 105 needs three unseen targets; 110 needs adversarial full-runtime repair.
- Never raise a score without current-head runtime evidence (receipt paths + digests).
- Unavailable hardware, credentials, licensed checkpoints, real-world capture, and external review are **blockers**, not passes.
- A derived claim is never stronger than its weakest input (`derive()`).
- Inferred or never-observed structure is never labelled OBSERVED.

## Known-open failures (must not be hidden)

### uv-packing-branching-forms

UV packing ~29% vs 35% gate on organic_sculpture and plant

Values:

- `gate`: `0.35`
- `organic_sculpture_packing_efficiency`: `0.2899297749612835`
- `plant_packing_efficiency`: `0.28122498335153917`

Receipts:

- `artifacts/v2/object-benchmarks/organic/organic_sculpture/scorecard.json`
- `artifacts/v2/object-benchmarks/organic/plant/scorecard.json`

Gate relaxed: **False**

### parity-roughness-blindness

Parity gate near-blind to roughness alone (dE stays ~2.82-3.07 across 0.1→0.9)

Values:

- `delta_e_roughness_0_1`: `2.816369851705388`
- `delta_e_roughness_0_9`: `2.891020980002129`
- `delta_e_sweep_max`: `3.065168361552816`

Receipts:

- `artifacts/v2/appearance/parity-check/discrimination_report.json`

Gate relaxed: **False**

### colmap-dense-mvs-no-cuda

COLMAP dense MVS unavailable (no CUDA build)

Receipts:

- `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`

Blocker requirement: a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run

### no-authorized-real-animal-capture

No authorized real-animal capture set; fur lane is synthetic only

Values:

- `guard_strands`: `3852`
- `guides`: `642`
- `undercoat_strands`: `6420`

Receipts:

- `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
- `artifacts/v2/sealed/framework.receipt.json`

Blocker requirement: Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture.

### no-user-supplied-consumer-object-photographs

No user-supplied consumer-object photographs; governed self-captured fixture used and must never be described as the user's object

Receipts:

- `artifacts/v2/object-benchmarks/remote/scorecard.json`
- `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`

Blocker requirement: authorized multiview photographs plus measurements of the user-owned consumer object; a governed self-captured fixture is used meanwhile and is never described as the user's object

### mac-studio-owned-blend-absent

Owned Mac Studio BLEND fixture absent (one Blender test skipped)

Receipts:

- `artifacts/v2/baseline.json`

Blocker requirement: BVMCP_MAC_STUDIO_SCENE pointing at the owned BLEND with SHA-256 22ea2562cc92d44b2df084f0009b3faca6ab37f6ff81e21e55136ac6871e6dae

### flagship-film-not-art-directed

Flagship film composition improved but not art-directed; lighting/framing lack human art-director pass

Values:

- `arc_length_m`: `27.331671650043138`
- `beats`: `9`

Receipts:

- `sandbox/datacenter-film/assets/build-receipt.json`

Blocker requirement: human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed)

## External blockers

| Id | Exact requirement |
| --- | --- |
| `colmap-dense-mvs` | a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run |
| `real-animal-capture` | Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture. |
| `user-supplied-consumer-object-photographs` | authorized multiview photographs plus measurements of the user-owned consumer object; a governed self-captured fixture is used meanwhile and is never described as the user's object |
| `mac-studio-owned-blend-fixture` | BVMCP_MAC_STUDIO_SCENE pointing at the owned BLEND with SHA-256 22ea2562cc92d44b2df084f0009b3faca6ab37f6ff81e21e55136ac6871e6dae |
| `human-art-direction` | human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed) |
| `uv-packing-branching-forms` | UV packing efficiency >= 0.35 on organic_sculpture and plant (currently ~0.29; gate not relaxed) |
| `parity-roughness-sensitivity` | parity gate that discriminates roughness alone with a clearly monotonic, high-dynamic-range dE response across 0.1->0.9 (current sweep stays near dE 2.82-3.07) |
| `three-unseen-real-targets` | three unseen real-world targets with authorized captures, scored under frozen thresholds without manual implementation changes |

## Facet ledger

| Facet | Baseline | Final | Δ | Implementation evidence | Runtime evidence | External / held-out | Failed attempts | Limitations | Reproduction | Remaining blocker |
| --- | ---: | ---: | ---: | --- | --- | --- | --- | --- | --- | --- |
| perception | 72 | 88 | +16 | `src/blender_vision/spatial/coverage.py`<br>`src/blender_vision/active_perception/planner.py`<br>`src/blender_vision/active_perception/uncertainty.py`<br>`src/blender_vision/benchmarks/objects.py` | `artifacts/v2/object-benchmarks/remote/scorecard.json` sha256=`ccf866da267d…` nums=[0.518675, 7]<br>`artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[] | Governed self-captured consumer_remote fixture (PROCEDURAL_GROUND_TRUTH); not a user-owned object. | COLMAP mapper exit 1 on sparse SfM retained as executed=false. | Planner uncertainty_before is receipt-backed; closed-loop post-view reduction is unit-tested without a separate critics receipt on this head.; No user-supplied photographs. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | `user-supplied-consumer-object-photographs`: authorized multiview photographs plus measurements of the user-owned consumer object; a governed self-captured fixture is used meanwhile and is never described as the user's object |
| calibration | 68 | 82 | +14 | `src/blender_vision/spatial/calibration.py`<br>`src/blender_vision/spatial/frames.py`<br>`docs/v2/SPATIAL_EVIDENCE.md` | `artifacts/v2/baseline.json` sha256=`e8dd6df2e73a…` nums=[28, 96]<br>`artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[] | Procedural board calibration accuracy is unit-tested and documented; no external metrology lab receipt. | none listed | Planar checkerboard only.; COLMAP dense MVS blocked without CUDA. | `BVMCP_RUN_BLENDER_TESTS=1 .venv/bin/python -m pytest -q tests/test_v2_spatial.py` | `colmap-dense-mvs`: a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run |
| depth | 66 | 81 | +15 | `src/blender_vision/spatial/depth.py`<br>`src/blender_vision/reconstruction/depth_fusion.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[0.01407329017748828]<br>`artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[] | Depth fusion chamfer vs construction on synthetic animal_bust. | none listed | Dense COLMAP MVS unavailable (no CUDA). | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | `colmap-dense-mvs`: a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run |
| point_clouds | 70 | 84 | +14 | `src/blender_vision/spatial/pointcloud.py`<br>`src/blender_vision/reconstruction/point_representation.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[0.01407329017748828, 0.01986979519450357]<br>`artifacts/v2/object-benchmarks/remote/scorecard.json` sha256=`ccf866da267d…` nums=[0.010019481188662721] | Portfolio point/mesh candidates on synthetic object benchmarks. | point_representation chamfer null by design (not a continuous surface). | No lidar field capture on this head. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | none |
| geometry_reconstruction | 62 | 82 | +20 | `src/blender_vision/reconstruction/portfolio.py`<br>`src/blender_vision/reconstruction/visual_hull.py`<br>`src/blender_vision/reconstruction/depth_fusion.py`<br>`src/blender_vision/reconstruction/parametric.py`<br>…(+1) | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[0.01407329017748828, 0.014722750816704891, 0.01986979519450357, 0.03655627452171458, 0.48028735835666536, False…]<br>`artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[] | Multiple synthetic targets with holdout views; not three unseen real objects. | colmap_sfm executed=false preserved.; retrieval chamfer weak (~0.48 m) retained. | Dimensional \|error\| up to 104.331 mm on animal_bust under envelope scoring.; Dense MVS blocked without CUDA.; Reference class is synthetic construction GT. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | `colmap-dense-mvs`: a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run |
| hidden_geometry | 78 | 91 | +13 | `src/blender_vision/reconstruction/fusion.py`<br>`src/blender_vision/benchmarks/objects.py`<br>`src/blender_vision/v2/authority.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[0, 2]<br>`artifacts/v2/object-benchmarks/remote/scorecard.json` sha256=`ccf866da267d…` nums=[0, 5, 4] | Hidden-surface ledgers; 0 surfaces incorrectly marked observed. | artifacts/v2/repair/failed-attempts/geometry-wrong-hidden-surface/ | Inferred interiors remain INFERRED. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | none |
| procedural_generation | 58 | 88 | +30 | `src/blender_vision/procedural/datacenter.py`<br>`src/blender_vision/procedural/grammar.py`<br>`src/blender_vision/procedural/emit.py`<br>`scripts/run-procedural-engine.py`<br>…(+1) | `sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[20, 464, 249432, 3073572, 1572864]<br>`sandbox/datacenter-film/assets/delivery-manifest.json` sha256=`4bbd2dc651d3…` nums=[249432, 3073572] | Flagship datacentre film is sealed synthetic PROCEDURAL_GROUND_TRUTH. | none listed | Never labelled OBSERVED.; This head cites film receipt (20 modules, 464 instances); separate engine_report.json is not under artifacts/v2/. | `.venv/bin/python scripts/build-datacenter-film.py` | none |
| topology | 55 | 86 | +31 | `src/blender_vision/organic/topology.py`<br>`scripts/run-organic-fur-lane.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[5814, 0, True]<br>`artifacts/v2/object-benchmarks/organic/animal_bust/source_packet.json` sha256=`1fc00e8aa60e…` nums=[5814, 5816] | Synthetic animal_bust all-quad watertight genus 0. | artifacts/v2/repair/failed-attempts/geometry-bad-topology/ | organic_sculpture is not watertight; topology claims are per-target. | `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic` | none |
| uv | 52 | 72 | +20 | `src/blender_vision/organic/topology.py`<br>`src/blender_vision/benchmarks/objects.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[34, 22.316072169507564, 0.6203405223071239]<br>`artifacts/v2/object-benchmarks/organic/organic_sculpture/scorecard.json` sha256=`e3e68c99d5fc…` nums=[0.2899297749612835, 0.35]<br>`artifacts/v2/object-benchmarks/organic/plant/scorecard.json` sha256=`404dae12283d…` nums=[0.28122498335153917, 0.35] | Measured UV packing on organic targets; known-open failures carried forward. | organic_sculpture and plant fail 0.35 packing gate (~0.29). | Gate not relaxed.; Healthy bust packing must not hide branching-form failure. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | `uv-packing-branching-forms`: UV packing efficiency >= 0.35 on organic_sculpture and plant (currently ~0.29; gate not relaxed) |
| textures | 55 | 74 | +19 | `src/blender_vision/materials/textures.py`<br>`src/blender_vision/materials/frequency.py` | `artifacts/v2/appearance/parity-check/discrimination_report.json` sha256=`9ed425d164b1…` nums=[8, 9]<br>`artifacts/v2/object-benchmarks/remote/scorecard.json` sha256=`ccf866da267d…` nums=[] | Synthetic multiview material/texture hypotheses. | artifacts/v2/repair/failed-attempts/material-flat-fake-foam/; artifacts/v2/repair/failed-attempts/material-texture-scale-error/ | Not full path-traced inverse texturing. | `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check` | none |
| materials | 62 | 78 | +16 | `src/blender_vision/materials/inverse.py`<br>`src/blender_vision/materials/parity.py`<br>`scripts/verify-parity-discrimination.py` | `artifacts/v2/appearance/parity-check/discrimination_report.json` sha256=`9ed425d164b1…` nums=[8, 9, 5.370374086816907, 0.278515107607349, 14.295966949747768, 0.4499955560243427…] | Nine-material probe rig; glass named exception; wrong material fails. | glass named exception; roughness near-blindness | KNOWN WEAKNESS: roughness sweep stays near dE 2.82-3.07 — materials capped.; Thresholds unchanged. | `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check` | `parity-roughness-sensitivity`: parity gate that discriminates roughness alone with a clearly monotonic, high-dynamic-range dE response across 0.1->0.9 (current sweep stays near dE 2.82-3.07) |
| lighting | 58 | 76 | +18 | `src/blender_vision/lighting/solve.py`<br>`src/blender_vision/lighting/rigs.py`<br>`src/blender_vision/critics/lighting_artist.py` | `artifacts/v2/repair/repair-corpus.summary.json` sha256=`32d52aa27cc8…` nums=[27, 0]<br>`artifacts/v2/repair/repair-corpus.receipt.json` sha256=`fecc657aee86…` nums=[27, 0] | Five lighting repair drills + inverse lighting modules. | artifacts/v2/repair/failed-attempts/lighting-clipped-hero/; artifacts/v2/repair/failed-attempts/lighting-flat-corridor/ | Flagship film lighting not human art-directed. | `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair` | `human-art-direction`: human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed) |
| reflections | 50 | 68 | +18 | `src/blender_vision/materials/parity.py`<br>`src/blender_vision/lighting/rigs.py` | `artifacts/v2/appearance/parity-check/discrimination_report.json` sha256=`9ed425d164b1…` nums=[5.370374086816907, 0.278515107607349, 14.295966949747768] | Probe-rig specular response; glass transmission named failure. | glass named exception retained | No dedicated environment-map reflection reconstruction benchmark receipt on this head. | `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check` | none |
| organic_geometry | 48 | 84 | +36 | `src/blender_vision/organic/topology.py`<br>`scripts/run-organic-fur-lane.py`<br>`scripts/run-object-benchmarks.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[5814, 0.01407329017748828, 26.967084304242785, 0.4951851808494054]<br>`artifacts/v2/object-benchmarks/organic/draped_cloth/scorecard.json` sha256=`c0ca4321d3af…` nums=[]<br>`artifacts/v2/object-benchmarks/organic/organic_sculpture/scorecard.json` sha256=`e3e68c99d5fc…` nums=[0.2899297749612835]<br>`artifacts/v2/object-benchmarks/organic/plant/scorecard.json` sha256=`404dae12283d…` nums=[0.28122498335153917] | Four synthetic soft/organic targets with construction GT. | UV packing known-open on sculpture and plant | Synthetic only. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | none |
| anatomy | 32 | 62 | +30 | `src/blender_vision/organic/topology.py`<br>`src/blender_vision/grooming/fur.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[5814, 642] | Synthetic animal bust only — not full character anatomy and not a real animal. | none listed | Real-animal lane blocked.; No multi-species anatomy library. | `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic` | `real-animal-capture`: Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture. |
| fur | 28 | 72 | +44 | `src/blender_vision/grooming/fur.py`<br>`scripts/run-organic-fur-lane.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[642, 3852, 6420]<br>`artifacts/v2/sealed/framework.receipt.json` sha256=`f3f01e6573d3…` nums=[6] | Synthetic groom only; fur_animal sealed contract remains blocked for real animals. | none listed | No authorized real-animal capture set.; Synthetic-claim disclaimer on every fur artifact. | `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic` | `real-animal-capture`: Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture. |
| scene_composition | 55 | 74 | +19 | `src/blender_vision/cinematic/path.py`<br>`src/blender_vision/procedural/grammar.py`<br>`scripts/build-datacenter-film.py` | `sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[9, 27.331671650043138, 464]<br>`sandbox/datacenter-film/assets/beats.json` sha256=`6d2559e4bd02…` nums=[9] | Single flagship corridor film; composition reads but is not art-director signed. | artifacts/v2/repair/failed-attempts/cinematic-mobile-crop/; artifacts/v2/repair/failed-attempts/cinematic-text-collision/ | Lighting and framing have not passed a human art director. | `.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic` | `human-art-direction`: human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed) |
| camera_paths | 62 | 88 | +26 | `src/blender_vision/cinematic/spline.py`<br>`src/blender_vision/cinematic/path.py`<br>`src/blender_vision/cinematic/replay.py` | `sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[9, 27.331671650043138]<br>`sandbox/datacenter-film/assets/motion-table.json` sha256=`44facee78b28…` nums=[27.331671650043138] | Deterministic replay on flagship path (9 beats, arc length receipt-backed). | artifacts/v2/repair/failed-attempts/cinematic-delayed-camera/; artifacts/v2/repair/failed-attempts/cinematic-dead-scroll/ | Browser camera match-to-0.06 m is a hardware-supervisor claim not re-encoded as a numeric claim here; motion-table arc length is. | `.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic` | none |
| web_compilation | 66 | 86 | +20 | `src/blender_vision/delivery/manifest.py`<br>`src/blender_vision/delivery/stream.py`<br>`src/blender_vision/delivery/compress.py`<br>`sandbox/datacenter-film/src/film.js` | `sandbox/datacenter-film/assets/delivery-manifest.json` sha256=`4bbd2dc651d3…` nums=[249432, 3073572, 249432, 1572864]<br>`sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[249432, 3073572]<br>`sandbox/datacenter-film/assets/streaming-plan.json` sha256=`bca76422343f…` nums=[] | Sealed delivery manifest with frozen budgets; single scene class. | artifacts/v2/repair/failed-attempts/delivery-oversized-shell/ | Budgets never widened; violations empty on this build. | `.venv/bin/python scripts/build-datacenter-film.py` | none |
| lod | 60 | 88 | +28 | `src/blender_vision/delivery/lods.py`<br>`src/blender_vision/procedural/lod.py` | `artifacts/v2/object-benchmarks/organic/animal_bust/source_packet.json` sha256=`1fc00e8aa60e…` nums=[0.9913717310187514, 0.999892013477775, 0.9982660058267123]<br>`artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[5814] | LOD silhouette IoU on synthetic bust; L3 IoU >= 0.99137. | artifacts/v2/repair/failed-attempts/geometry-lod-identity-mismatch/ | Silhouette IoU identity, not full material LOD audit. | `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic` | none |
| streaming | 58 | 85 | +27 | `src/blender_vision/delivery/stream.py`<br>`sandbox/datacenter-film/assets/streaming-plan.json` | `sandbox/datacenter-film/assets/streaming-plan.json` sha256=`bca76422343f…` nums=[0, 0.0, 1, 0.0]<br>`sandbox/datacenter-film/assets/delivery-manifest.json` sha256=`4bbd2dc651d3…` nums=[249432, 3073572]<br>`sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[3073572, 249432] | Streaming plan poster→shell→detail chapter-gated on flagship film. | none listed | Detail is chapter-gated and must not load as shell. | `.venv/bin/python scripts/build-datacenter-film.py` | none |
| performance | 62 | 82 | +20 | `src/blender_vision/delivery/manifest.py`<br>`src/blender_vision/critics/performance_engineer.py`<br>`src/blender_vision/benchmarks/repair_corpus.py` | `sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[249432, 249432, 1572864, 665600]<br>`sandbox/datacenter-film/assets/delivery-manifest.json` sha256=`4bbd2dc651d3…` nums=[249432]<br>`artifacts/v2/repair/repair-corpus.summary.json` sha256=`32d52aa27cc8…` nums=[27, 0] | Budget compliance + delivery performance repair drills. | artifacts/v2/repair/failed-attempts/delivery-decode-long-task/; artifacts/v2/repair/failed-attempts/delivery-memory-growth/ | Byte budgets receipt-backed on this head; multi-device FPS field study not claimed. | `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair` | none |
| accessibility | 68 | 84 | +16 | `src/blender_vision/critics/accessibility_reviewer.py`<br>`src/blender_vision/cinematic/textsafe.py`<br>`sandbox/datacenter-film/assets/reduced-motion.json` | `sandbox/datacenter-film/assets/reduced-motion.json` sha256=`86e8bfcb7e9e…` nums=[27.331671650043]<br>`artifacts/v2/repair/repair-corpus.summary.json` sha256=`32d52aa27cc8…` nums=[27, 0] | Reduced-motion path + accessibility critic drills. | artifacts/v2/repair/failed-attempts/cinematic-text-collision/; artifacts/v2/repair/failed-attempts/cinematic-reduced-motion-regression/ | No physical screen-reader pairing study on this head. | `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair` | none |
| provenance | 78 | 90 | +12 | `src/blender_vision/v2/records.py`<br>`src/blender_vision/v2/authority.py`<br>`src/blender_vision/v2/validation.py` | `artifacts/v2/sealed/framework.receipt.json` sha256=`f3f01e6573d3…` nums=[6, 0]<br>`artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[]<br>`sandbox/datacenter-film/assets/delivery-manifest.json` sha256=`4bbd2dc651d3…` nums=[249432] | Six sealed contracts with digests; lineage on manifests. | none listed | rights_state unreviewed on many synthetic records until external rights supplied. | `.venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed` | none |
| security | 82 | 92 | +10 | `src/blender_vision/benchmarks/sealed.py`<br>`scripts/run-sealed-framework.py` | `artifacts/v2/sealed/framework.receipt.json` sha256=`f3f01e6573d3…` nums=[6, 0] | Six leakage probes blocked; zero leakage failures. | none listed | Sealed-builder isolation for benchmarks; not a multi-tenant cloud security audit. | `.venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed` | none |
| repair | 72 | 90 | +18 | `src/blender_vision/benchmarks/repair_corpus.py`<br>`scripts/run-repair-corpus.py` | `artifacts/v2/repair/repair-corpus.summary.json` sha256=`32d52aa27cc8…` nums=[27, 0, 27, 0, 0]<br>`artifacts/v2/repair/repair-corpus.receipt.json` sha256=`fecc657aee86…` nums=[27, 0] | 27 full-runtime inject/detect/repair drills; not a 110 adversarial multi-target campaign. | artifacts/v2/repair/failed-attempts/geometry-wrong-dimensions/; artifacts/v2/repair/failed-attempts/material-wrong-roughness/; artifacts/v2/repair/failed-attempts/delivery-oversized-shell/ | Specialist-critic drills, not three-target 110 full-runtime adversarial recovery. | `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair` | none |
| perceptual_quality | 52 | 70 | +18 | `src/blender_vision/critics/__init__.py`<br>`src/blender_vision/critics/editorial_art_director.py`<br>`src/blender_vision/critics/cinematographer.py` | `artifacts/v2/repair/repair-corpus.summary.json` sha256=`32d52aa27cc8…` nums=[27, 0]<br>`sandbox/datacenter-film/assets/build-receipt.json` sha256=`a56162b74038…` nums=[9, 27.331671650043138]<br>`artifacts/v2/appearance/parity-check/discrimination_report.json` sha256=`9ed425d164b1…` nums=[8] | Critics + repair corpus; flagship film not human art-directed. | artifacts/v2/repair/failed-attempts/cinematic-mobile-crop/ | Corridor reads; lighting/framing lack human art-director pass — score capped.; 13/13 critic catch-matrix is unit-tested (tests/test_v2_critics.py); no separate critics receipt under artifacts/v2/ on this head. | `.venv/bin/python -m pytest -q tests/test_v2_critics.py tests/test_v2_repair_corpus.py` | `human-art-direction`: human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed) |
| generalization | 48 | 68 | +20 | `src/blender_vision/benchmarks/objects.py`<br>`src/blender_vision/reconstruction/portfolio.py` | `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json` sha256=`be9b7f44bf13…` nums=[]<br>`artifacts/v2/object-benchmarks/remote/scorecard.json` sha256=`ccf866da267d…` nums=[0.010019481188662721]<br>`artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json` sha256=`22543266d42e…` nums=[0.01407329017748828]<br>`artifacts/v2/object-benchmarks/organic/plant/scorecard.json` sha256=`404dae12283d…` nums=[0.28122498335153917] | Multiple synthetic targets only. No facet claims 105. | none listed | No user-supplied consumer-object photographs.; No authorized real-animal capture.; Cannot redefine reference class to synthetic-only to reach 100/105. | `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks` | `three-unseen-real-targets`: three unseen real-world targets with authorized captures, scored under frozen thresholds without manual implementation changes |

## Per-facet detail

### perception

- **Baseline → final:** 72 → **88** (Δ +16)
- **Implementation evidence:**
  - `src/blender_vision/spatial/coverage.py`
  - `src/blender_vision/active_perception/planner.py`
  - `src/blender_vision/active_perception/uncertainty.py`
  - `src/blender_vision/benchmarks/objects.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/remote/scorecard.json`
    - sha256: `ccf866da267d3d74751dda49f3189e0208db14a9e3315d57bec349b1e0c6938f`
    - numeric claims: `[0.518675, 7]`
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
- **External / held-out:** Governed self-captured consumer_remote fixture (PROCEDURAL_GROUND_TRUTH); not a user-owned object.
- **Failed attempts:**
  - COLMAP mapper exit 1 on sparse SfM retained as executed=false.
- **Limitations:**
  - Planner uncertainty_before is receipt-backed; closed-loop post-view reduction is unit-tested without a separate critics receipt on this head.
  - No user-supplied photographs.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker (`user-supplied-consumer-object-photographs`):** authorized multiview photographs plus measurements of the user-owned consumer object; a governed self-captured fixture is used meanwhile and is never described as the user's object

### calibration

- **Baseline → final:** 68 → **82** (Δ +14)
- **Implementation evidence:**
  - `src/blender_vision/spatial/calibration.py`
  - `src/blender_vision/spatial/frames.py`
  - `docs/v2/SPATIAL_EVIDENCE.md`
- **Runtime evidence:**
  - `artifacts/v2/baseline.json`
    - sha256: `e8dd6df2e73aa2830480b894805601f5bed170a2ef393005c3b7f3440ee774b3`
    - numeric claims: `[28, 96]`
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
- **External / held-out:** Procedural board calibration accuracy is unit-tested and documented; no external metrology lab receipt.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Planar checkerboard only.
  - COLMAP dense MVS blocked without CUDA.
- **Reproduction:** `BVMCP_RUN_BLENDER_TESTS=1 .venv/bin/python -m pytest -q tests/test_v2_spatial.py`
- **Remaining blocker (`colmap-dense-mvs`):** a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run

### depth

- **Baseline → final:** 66 → **81** (Δ +15)
- **Implementation evidence:**
  - `src/blender_vision/spatial/depth.py`
  - `src/blender_vision/reconstruction/depth_fusion.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[0.01407329017748828]`
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
- **External / held-out:** Depth fusion chamfer vs construction on synthetic animal_bust.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Dense COLMAP MVS unavailable (no CUDA).
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker (`colmap-dense-mvs`):** a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run

### point_clouds

- **Baseline → final:** 70 → **84** (Δ +14)
- **Implementation evidence:**
  - `src/blender_vision/spatial/pointcloud.py`
  - `src/blender_vision/reconstruction/point_representation.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[0.01407329017748828, 0.01986979519450357]`
  - `artifacts/v2/object-benchmarks/remote/scorecard.json`
    - sha256: `ccf866da267d3d74751dda49f3189e0208db14a9e3315d57bec349b1e0c6938f`
    - numeric claims: `[0.010019481188662721]`
- **External / held-out:** Portfolio point/mesh candidates on synthetic object benchmarks.
- **Failed attempts:**
  - point_representation chamfer null by design (not a continuous surface).
- **Limitations:**
  - No lidar field capture on this head.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### geometry_reconstruction

- **Baseline → final:** 62 → **82** (Δ +20)
- **Implementation evidence:**
  - `src/blender_vision/reconstruction/portfolio.py`
  - `src/blender_vision/reconstruction/visual_hull.py`
  - `src/blender_vision/reconstruction/depth_fusion.py`
  - `src/blender_vision/reconstruction/parametric.py`
  - `src/blender_vision/reconstruction/colmap_sfm.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[0.01407329017748828, 0.014722750816704891, 0.01986979519450357, 0.03655627452171458, 0.48028735835666536, False, 104.33090925216675, 26.967084304242785, 0.4951851808494054]`
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
- **External / held-out:** Multiple synthetic targets with holdout views; not three unseen real objects.
- **Failed attempts:**
  - colmap_sfm executed=false preserved.
  - retrieval chamfer weak (~0.48 m) retained.
- **Limitations:**
  - Dimensional |error| up to 104.331 mm on animal_bust under envelope scoring.
  - Dense MVS blocked without CUDA.
  - Reference class is synthetic construction GT.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker (`colmap-dense-mvs`):** a CUDA-capable COLMAP build; the installed 4.0.4 is CPU-only so patch_match_stereo cannot run

### hidden_geometry

- **Baseline → final:** 78 → **91** (Δ +13)
- **Implementation evidence:**
  - `src/blender_vision/reconstruction/fusion.py`
  - `src/blender_vision/benchmarks/objects.py`
  - `src/blender_vision/v2/authority.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[0, 2]`
  - `artifacts/v2/object-benchmarks/remote/scorecard.json`
    - sha256: `ccf866da267d3d74751dda49f3189e0208db14a9e3315d57bec349b1e0c6938f`
    - numeric claims: `[0, 5, 4]`
- **External / held-out:** Hidden-surface ledgers; 0 surfaces incorrectly marked observed.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/geometry-wrong-hidden-surface/
- **Limitations:**
  - Inferred interiors remain INFERRED.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### procedural_generation

- **Baseline → final:** 58 → **88** (Δ +30)
- **Implementation evidence:**
  - `src/blender_vision/procedural/datacenter.py`
  - `src/blender_vision/procedural/grammar.py`
  - `src/blender_vision/procedural/emit.py`
  - `scripts/run-procedural-engine.py`
  - `scripts/build-datacenter-film.py`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[20, 464, 249432, 3073572, 1572864]`
  - `sandbox/datacenter-film/assets/delivery-manifest.json`
    - sha256: `4bbd2dc651d3d258b9c333c93fb8edc00c21dfb71156abb10a45088432faa7d1`
    - numeric claims: `[249432, 3073572]`
- **External / held-out:** Flagship datacentre film is sealed synthetic PROCEDURAL_GROUND_TRUTH.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Never labelled OBSERVED.
  - This head cites film receipt (20 modules, 464 instances); separate engine_report.json is not under artifacts/v2/.
- **Reproduction:** `.venv/bin/python scripts/build-datacenter-film.py`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### topology

- **Baseline → final:** 55 → **86** (Δ +31)
- **Implementation evidence:**
  - `src/blender_vision/organic/topology.py`
  - `scripts/run-organic-fur-lane.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[5814, 0, True]`
  - `artifacts/v2/object-benchmarks/organic/animal_bust/source_packet.json`
    - sha256: `1fc00e8aa60eeaf33eb47c23dd1c68590aa5f9a5db998dc1fd56af86a1c853c7`
    - numeric claims: `[5814, 5816]`
- **External / held-out:** Synthetic animal_bust all-quad watertight genus 0.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/geometry-bad-topology/
- **Limitations:**
  - organic_sculpture is not watertight; topology claims are per-target.
- **Reproduction:** `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### uv

- **Baseline → final:** 52 → **72** (Δ +20)
- **Implementation evidence:**
  - `src/blender_vision/organic/topology.py`
  - `src/blender_vision/benchmarks/objects.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[34, 22.316072169507564, 0.6203405223071239]`
  - `artifacts/v2/object-benchmarks/organic/organic_sculpture/scorecard.json`
    - sha256: `e3e68c99d5fc220f11d4b6e07df4fafa711198c46b68213f1619d55bf66a343a`
    - numeric claims: `[0.2899297749612835, 0.35]`
  - `artifacts/v2/object-benchmarks/organic/plant/scorecard.json`
    - sha256: `404dae12283d08256b427f284d07085129e55a43295487c061909ec7c546f6df`
    - numeric claims: `[0.28122498335153917, 0.35]`
- **External / held-out:** Measured UV packing on organic targets; known-open failures carried forward.
- **Failed attempts:**
  - organic_sculpture and plant fail 0.35 packing gate (~0.29).
- **Limitations:**
  - Gate not relaxed.
  - Healthy bust packing must not hide branching-form failure.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker (`uv-packing-branching-forms`):** UV packing efficiency >= 0.35 on organic_sculpture and plant (currently ~0.29; gate not relaxed)

### textures

- **Baseline → final:** 55 → **74** (Δ +19)
- **Implementation evidence:**
  - `src/blender_vision/materials/textures.py`
  - `src/blender_vision/materials/frequency.py`
- **Runtime evidence:**
  - `artifacts/v2/appearance/parity-check/discrimination_report.json`
    - sha256: `9ed425d164b19e878d6a93aa00e97373c6eb5ba613b6f2542a8817136426841a`
    - numeric claims: `[8, 9]`
  - `artifacts/v2/object-benchmarks/remote/scorecard.json`
    - sha256: `ccf866da267d3d74751dda49f3189e0208db14a9e3315d57bec349b1e0c6938f`
- **External / held-out:** Synthetic multiview material/texture hypotheses.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/material-flat-fake-foam/
  - artifacts/v2/repair/failed-attempts/material-texture-scale-error/
- **Limitations:**
  - Not full path-traced inverse texturing.
- **Reproduction:** `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### materials

- **Baseline → final:** 62 → **78** (Δ +16)
- **Implementation evidence:**
  - `src/blender_vision/materials/inverse.py`
  - `src/blender_vision/materials/parity.py`
  - `scripts/verify-parity-discrimination.py`
- **Runtime evidence:**
  - `artifacts/v2/appearance/parity-check/discrimination_report.json`
    - sha256: `9ed425d164b19e878d6a93aa00e97373c6eb5ba613b6f2542a8817136426841a`
    - numeric claims: `[8, 9, 5.370374086816907, 0.278515107607349, 14.295966949747768, 0.4499955560243427, 2.816369851705388, 2.891020980002129, 3.065168361552816, 8.0, 0.15]`
- **External / held-out:** Nine-material probe rig; glass named exception; wrong material fails.
- **Failed attempts:**
  - glass named exception
  - roughness near-blindness
- **Limitations:**
  - KNOWN WEAKNESS: roughness sweep stays near dE 2.82-3.07 — materials capped.
  - Thresholds unchanged.
- **Reproduction:** `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check`
- **Remaining blocker (`parity-roughness-sensitivity`):** parity gate that discriminates roughness alone with a clearly monotonic, high-dynamic-range dE response across 0.1->0.9 (current sweep stays near dE 2.82-3.07)

### lighting

- **Baseline → final:** 58 → **76** (Δ +18)
- **Implementation evidence:**
  - `src/blender_vision/lighting/solve.py`
  - `src/blender_vision/lighting/rigs.py`
  - `src/blender_vision/critics/lighting_artist.py`
- **Runtime evidence:**
  - `artifacts/v2/repair/repair-corpus.summary.json`
    - sha256: `32d52aa27cc8cd6bf094915ee0e7bdc8865ac8f28c7e5f6346cf7596cd8cc02b`
    - numeric claims: `[27, 0]`
  - `artifacts/v2/repair/repair-corpus.receipt.json`
    - sha256: `fecc657aee8622feddb17bdc5b3d29f799ff8ae95dc6f32f09c99af8ea2093ef`
    - numeric claims: `[27, 0]`
- **External / held-out:** Five lighting repair drills + inverse lighting modules.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/lighting-clipped-hero/
  - artifacts/v2/repair/failed-attempts/lighting-flat-corridor/
- **Limitations:**
  - Flagship film lighting not human art-directed.
- **Reproduction:** `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair`
- **Remaining blocker (`human-art-direction`):** human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed)

### reflections

- **Baseline → final:** 50 → **68** (Δ +18)
- **Implementation evidence:**
  - `src/blender_vision/materials/parity.py`
  - `src/blender_vision/lighting/rigs.py`
- **Runtime evidence:**
  - `artifacts/v2/appearance/parity-check/discrimination_report.json`
    - sha256: `9ed425d164b19e878d6a93aa00e97373c6eb5ba613b6f2542a8817136426841a`
    - numeric claims: `[5.370374086816907, 0.278515107607349, 14.295966949747768]`
- **External / held-out:** Probe-rig specular response; glass transmission named failure.
- **Failed attempts:**
  - glass named exception retained
- **Limitations:**
  - No dedicated environment-map reflection reconstruction benchmark receipt on this head.
- **Reproduction:** `.venv/bin/python scripts/verify-parity-discrimination.py --output artifacts/v2/appearance/parity-check`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### organic_geometry

- **Baseline → final:** 48 → **84** (Δ +36)
- **Implementation evidence:**
  - `src/blender_vision/organic/topology.py`
  - `scripts/run-organic-fur-lane.py`
  - `scripts/run-object-benchmarks.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[5814, 0.01407329017748828, 26.967084304242785, 0.4951851808494054]`
  - `artifacts/v2/object-benchmarks/organic/draped_cloth/scorecard.json`
    - sha256: `c0ca4321d3afb60458ee72cb93eb7bb024475b7008357721563f70cc0292e7e8`
  - `artifacts/v2/object-benchmarks/organic/organic_sculpture/scorecard.json`
    - sha256: `e3e68c99d5fc220f11d4b6e07df4fafa711198c46b68213f1619d55bf66a343a`
    - numeric claims: `[0.2899297749612835]`
  - `artifacts/v2/object-benchmarks/organic/plant/scorecard.json`
    - sha256: `404dae12283d08256b427f284d07085129e55a43295487c061909ec7c546f6df`
    - numeric claims: `[0.28122498335153917]`
- **External / held-out:** Four synthetic soft/organic targets with construction GT.
- **Failed attempts:**
  - UV packing known-open on sculpture and plant
- **Limitations:**
  - Synthetic only.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### anatomy

- **Baseline → final:** 32 → **62** (Δ +30)
- **Implementation evidence:**
  - `src/blender_vision/organic/topology.py`
  - `src/blender_vision/grooming/fur.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[5814, 642]`
- **External / held-out:** Synthetic animal bust only — not full character anatomy and not a real animal.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Real-animal lane blocked.
  - No multi-species anatomy library.
- **Reproduction:** `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic`
- **Remaining blocker (`real-animal-capture`):** Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture.

### fur

- **Baseline → final:** 28 → **72** (Δ +44)
- **Implementation evidence:**
  - `src/blender_vision/grooming/fur.py`
  - `scripts/run-organic-fur-lane.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[642, 3852, 6420]`
  - `artifacts/v2/sealed/framework.receipt.json`
    - sha256: `f3f01e6573d30b0e6585336cbe43c1b8c0222d34ce90295d8a3ffcba4205a5ea`
    - numeric claims: `[6]`
- **External / held-out:** Synthetic groom only; fur_animal sealed contract remains blocked for real animals.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - No authorized real-animal capture set.
  - Synthetic-claim disclaimer on every fur artifact.
- **Reproduction:** `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic`
- **Remaining blocker (`real-animal-capture`):** Rights-cleared multiview stills and/or video of a real animal, explicit rights_state OWNED or LICENSED, capture environment receipt, and an ethical-use attestation. Synthetic busts may only appear as a separate non-scored development fixture.

### scene_composition

- **Baseline → final:** 55 → **74** (Δ +19)
- **Implementation evidence:**
  - `src/blender_vision/cinematic/path.py`
  - `src/blender_vision/procedural/grammar.py`
  - `scripts/build-datacenter-film.py`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[9, 27.331671650043138, 464]`
  - `sandbox/datacenter-film/assets/beats.json`
    - sha256: `6d2559e4bd0222ce8507239b2eb65f915b10d45f6e4ac56d41f3a3c4f410dc3d`
    - numeric claims: `[9]`
- **External / held-out:** Single flagship corridor film; composition reads but is not art-director signed.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/cinematic-mobile-crop/
  - artifacts/v2/repair/failed-attempts/cinematic-text-collision/
- **Limitations:**
  - Lighting and framing have not passed a human art director.
- **Reproduction:** `.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic`
- **Remaining blocker (`human-art-direction`):** human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed)

### camera_paths

- **Baseline → final:** 62 → **88** (Δ +26)
- **Implementation evidence:**
  - `src/blender_vision/cinematic/spline.py`
  - `src/blender_vision/cinematic/path.py`
  - `src/blender_vision/cinematic/replay.py`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[9, 27.331671650043138]`
  - `sandbox/datacenter-film/assets/motion-table.json`
    - sha256: `44facee78b28a0201a6b88ed05ae6429070ce93b469d2b738e5b2bca121d6d42`
    - numeric claims: `[27.331671650043138]`
- **External / held-out:** Deterministic replay on flagship path (9 beats, arc length receipt-backed).
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/cinematic-delayed-camera/
  - artifacts/v2/repair/failed-attempts/cinematic-dead-scroll/
- **Limitations:**
  - Browser camera match-to-0.06 m is a hardware-supervisor claim not re-encoded as a numeric claim here; motion-table arc length is.
- **Reproduction:** `.venv/bin/python scripts/run-cinematic-delivery.py --output artifacts/v2/cinematic`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### web_compilation

- **Baseline → final:** 66 → **86** (Δ +20)
- **Implementation evidence:**
  - `src/blender_vision/delivery/manifest.py`
  - `src/blender_vision/delivery/stream.py`
  - `src/blender_vision/delivery/compress.py`
  - `sandbox/datacenter-film/src/film.js`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/delivery-manifest.json`
    - sha256: `4bbd2dc651d3d258b9c333c93fb8edc00c21dfb71156abb10a45088432faa7d1`
    - numeric claims: `[249432, 3073572, 249432, 1572864]`
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[249432, 3073572]`
  - `sandbox/datacenter-film/assets/streaming-plan.json`
    - sha256: `bca76422343f6f9a3a88fc2387b422eda64ffd2ecf3c491b77bc3ff258590ad5`
- **External / held-out:** Sealed delivery manifest with frozen budgets; single scene class.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/delivery-oversized-shell/
- **Limitations:**
  - Budgets never widened; violations empty on this build.
- **Reproduction:** `.venv/bin/python scripts/build-datacenter-film.py`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### lod

- **Baseline → final:** 60 → **88** (Δ +28)
- **Implementation evidence:**
  - `src/blender_vision/delivery/lods.py`
  - `src/blender_vision/procedural/lod.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/organic/animal_bust/source_packet.json`
    - sha256: `1fc00e8aa60eeaf33eb47c23dd1c68590aa5f9a5db998dc1fd56af86a1c853c7`
    - numeric claims: `[0.9913717310187514, 0.999892013477775, 0.9982660058267123]`
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[5814]`
- **External / held-out:** LOD silhouette IoU on synthetic bust; L3 IoU >= 0.99137.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/geometry-lod-identity-mismatch/
- **Limitations:**
  - Silhouette IoU identity, not full material LOD audit.
- **Reproduction:** `.venv/bin/python scripts/run-organic-fur-lane.py --output artifacts/v2/organic`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### streaming

- **Baseline → final:** 58 → **85** (Δ +27)
- **Implementation evidence:**
  - `src/blender_vision/delivery/stream.py`
  - `sandbox/datacenter-film/assets/streaming-plan.json`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/streaming-plan.json`
    - sha256: `bca76422343f6f9a3a88fc2387b422eda64ffd2ecf3c491b77bc3ff258590ad5`
    - numeric claims: `[0, 0.0, 1, 0.0]`
  - `sandbox/datacenter-film/assets/delivery-manifest.json`
    - sha256: `4bbd2dc651d3d258b9c333c93fb8edc00c21dfb71156abb10a45088432faa7d1`
    - numeric claims: `[249432, 3073572]`
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[3073572, 249432]`
- **External / held-out:** Streaming plan poster→shell→detail chapter-gated on flagship film.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Detail is chapter-gated and must not load as shell.
- **Reproduction:** `.venv/bin/python scripts/build-datacenter-film.py`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### performance

- **Baseline → final:** 62 → **82** (Δ +20)
- **Implementation evidence:**
  - `src/blender_vision/delivery/manifest.py`
  - `src/blender_vision/critics/performance_engineer.py`
  - `src/blender_vision/benchmarks/repair_corpus.py`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[249432, 249432, 1572864, 665600]`
  - `sandbox/datacenter-film/assets/delivery-manifest.json`
    - sha256: `4bbd2dc651d3d258b9c333c93fb8edc00c21dfb71156abb10a45088432faa7d1`
    - numeric claims: `[249432]`
  - `artifacts/v2/repair/repair-corpus.summary.json`
    - sha256: `32d52aa27cc8cd6bf094915ee0e7bdc8865ac8f28c7e5f6346cf7596cd8cc02b`
    - numeric claims: `[27, 0]`
- **External / held-out:** Budget compliance + delivery performance repair drills.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/delivery-decode-long-task/
  - artifacts/v2/repair/failed-attempts/delivery-memory-growth/
- **Limitations:**
  - Byte budgets receipt-backed on this head; multi-device FPS field study not claimed.
- **Reproduction:** `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### accessibility

- **Baseline → final:** 68 → **84** (Δ +16)
- **Implementation evidence:**
  - `src/blender_vision/critics/accessibility_reviewer.py`
  - `src/blender_vision/cinematic/textsafe.py`
  - `sandbox/datacenter-film/assets/reduced-motion.json`
- **Runtime evidence:**
  - `sandbox/datacenter-film/assets/reduced-motion.json`
    - sha256: `86e8bfcb7e9e9c9208a904f76ea3a4845e0f757653fc6f90294c7c8237b1f077`
    - numeric claims: `[27.331671650043]`
  - `artifacts/v2/repair/repair-corpus.summary.json`
    - sha256: `32d52aa27cc8cd6bf094915ee0e7bdc8865ac8f28c7e5f6346cf7596cd8cc02b`
    - numeric claims: `[27, 0]`
- **External / held-out:** Reduced-motion path + accessibility critic drills.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/cinematic-text-collision/
  - artifacts/v2/repair/failed-attempts/cinematic-reduced-motion-regression/
- **Limitations:**
  - No physical screen-reader pairing study on this head.
- **Reproduction:** `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### provenance

- **Baseline → final:** 78 → **90** (Δ +12)
- **Implementation evidence:**
  - `src/blender_vision/v2/records.py`
  - `src/blender_vision/v2/authority.py`
  - `src/blender_vision/v2/validation.py`
- **Runtime evidence:**
  - `artifacts/v2/sealed/framework.receipt.json`
    - sha256: `f3f01e6573d30b0e6585336cbe43c1b8c0222d34ce90295d8a3ffcba4205a5ea`
    - numeric claims: `[6, 0]`
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
  - `sandbox/datacenter-film/assets/delivery-manifest.json`
    - sha256: `4bbd2dc651d3d258b9c333c93fb8edc00c21dfb71156abb10a45088432faa7d1`
    - numeric claims: `[249432]`
- **External / held-out:** Six sealed contracts with digests; lineage on manifests.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - rights_state unreviewed on many synthetic records until external rights supplied.
- **Reproduction:** `.venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### security

- **Baseline → final:** 82 → **92** (Δ +10)
- **Implementation evidence:**
  - `src/blender_vision/benchmarks/sealed.py`
  - `scripts/run-sealed-framework.py`
- **Runtime evidence:**
  - `artifacts/v2/sealed/framework.receipt.json`
    - sha256: `f3f01e6573d30b0e6585336cbe43c1b8c0222d34ce90295d8a3ffcba4205a5ea`
    - numeric claims: `[6, 0]`
- **External / held-out:** Six leakage probes blocked; zero leakage failures.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - Sealed-builder isolation for benchmarks; not a multi-tenant cloud security audit.
- **Reproduction:** `.venv/bin/python scripts/run-sealed-framework.py --output artifacts/v2/sealed`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### repair

- **Baseline → final:** 72 → **90** (Δ +18)
- **Implementation evidence:**
  - `src/blender_vision/benchmarks/repair_corpus.py`
  - `scripts/run-repair-corpus.py`
- **Runtime evidence:**
  - `artifacts/v2/repair/repair-corpus.summary.json`
    - sha256: `32d52aa27cc8cd6bf094915ee0e7bdc8865ac8f28c7e5f6346cf7596cd8cc02b`
    - numeric claims: `[27, 0, 27, 0, 0]`
  - `artifacts/v2/repair/repair-corpus.receipt.json`
    - sha256: `fecc657aee8622feddb17bdc5b3d29f799ff8ae95dc6f32f09c99af8ea2093ef`
    - numeric claims: `[27, 0]`
- **External / held-out:** 27 full-runtime inject/detect/repair drills; not a 110 adversarial multi-target campaign.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/geometry-wrong-dimensions/
  - artifacts/v2/repair/failed-attempts/material-wrong-roughness/
  - artifacts/v2/repair/failed-attempts/delivery-oversized-shell/
- **Limitations:**
  - Specialist-critic drills, not three-target 110 full-runtime adversarial recovery.
- **Reproduction:** `.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair`
- **Remaining blocker:** none declared for score increase path beyond generalisation to 100+

### perceptual_quality

- **Baseline → final:** 52 → **70** (Δ +18)
- **Implementation evidence:**
  - `src/blender_vision/critics/__init__.py`
  - `src/blender_vision/critics/editorial_art_director.py`
  - `src/blender_vision/critics/cinematographer.py`
- **Runtime evidence:**
  - `artifacts/v2/repair/repair-corpus.summary.json`
    - sha256: `32d52aa27cc8cd6bf094915ee0e7bdc8865ac8f28c7e5f6346cf7596cd8cc02b`
    - numeric claims: `[27, 0]`
  - `sandbox/datacenter-film/assets/build-receipt.json`
    - sha256: `a56162b74038e4d3b3632dbafd1134df8b1d7002baa3cdd744afcc6814bcf523`
    - numeric claims: `[9, 27.331671650043138]`
  - `artifacts/v2/appearance/parity-check/discrimination_report.json`
    - sha256: `9ed425d164b19e878d6a93aa00e97373c6eb5ba613b6f2542a8817136426841a`
    - numeric claims: `[8]`
- **External / held-out:** Critics + repair corpus; flagship film not human art-directed.
- **Failed attempts:**
  - artifacts/v2/repair/failed-attempts/cinematic-mobile-crop/
- **Limitations:**
  - Corridor reads; lighting/framing lack human art-director pass — score capped.
  - 13/13 critic catch-matrix is unit-tested (tests/test_v2_critics.py); no separate critics receipt under artifacts/v2/ on this head.
- **Reproduction:** `.venv/bin/python -m pytest -q tests/test_v2_critics.py tests/test_v2_repair_corpus.py`
- **Remaining blocker (`human-art-direction`):** human art-director sign-off on flagship film lighting and framing (corridor composition is improved but not art-directed)

### generalization

- **Baseline → final:** 48 → **68** (Δ +20)
- **Implementation evidence:**
  - `src/blender_vision/benchmarks/objects.py`
  - `src/blender_vision/reconstruction/portfolio.py`
- **Runtime evidence:**
  - `artifacts/v2/object-benchmarks/object_benchmarks_receipt.json`
    - sha256: `be9b7f44bf130ee4a5a92b0ff6d13ca551d3e2c6a382ad54b2a287708850ad79`
  - `artifacts/v2/object-benchmarks/remote/scorecard.json`
    - sha256: `ccf866da267d3d74751dda49f3189e0208db14a9e3315d57bec349b1e0c6938f`
    - numeric claims: `[0.010019481188662721]`
  - `artifacts/v2/object-benchmarks/organic/animal_bust/scorecard.json`
    - sha256: `22543266d42ee38e413a24c4e36ddb6939bb6b3e83c96ebb9ba16920bb33873d`
    - numeric claims: `[0.01407329017748828]`
  - `artifacts/v2/object-benchmarks/organic/plant/scorecard.json`
    - sha256: `404dae12283d08256b427f284d07085129e55a43295487c061909ec7c546f6df`
    - numeric claims: `[0.28122498335153917]`
- **External / held-out:** Multiple synthetic targets only. No facet claims 105.
- **Failed attempts:**
  - (none recorded beyond retained suite blockers)
- **Limitations:**
  - No user-supplied consumer-object photographs.
  - No authorized real-animal capture.
  - Cannot redefine reference class to synthetic-only to reach 100/105.
- **Reproduction:** `.venv/bin/python scripts/run-object-benchmarks.py --output artifacts/v2/object-benchmarks`
- **Remaining blocker (`three-unseen-real-targets`):** three unseen real-world targets with authorized captures, scored under frozen thresholds without manual implementation changes

## Verifier

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/verify-final-scorecard.py
```

The verifier loads `final-scorecard.json` without trusting this markdown,
checks every cited receipt path and digest, confirms every numeric claim
appears in the cited receipt, rejects scores ≥ 100 without proof fields,
requires exact requirement strings on blockers, and exits non-zero on any
unbacked claim.

## Related reports

- `V2_ARCHITECTURE.md`, `V2_BASELINE.md`, `V1_RECOVERY_REPORT.md`
- `RECONSTRUCTION_ENSEMBLE.md`, `PROCEDURAL_WORLD_ENGINE.md`
- `INVERSE_MATERIALS.md`, `INVERSE_LIGHTING.md`
- `ORGANIC_AND_FUR.md`, `SOFT_ORGANIC_FUR_REPORT.md`
- `CINEMATIC_COMPILER.md`, `WEB_SCENE_COMPILER.md`, `DATACENTER_FILM_REPORT.md`
- `PERCEPTUAL_CRITICS.md`, `ACTIVE_PERCEPTION.md`
- `REMOTE_BENCHMARK_REPORT.md`, `REPAIR_CORPUS.md`, `SEALED_BENCHMARKS.md`

