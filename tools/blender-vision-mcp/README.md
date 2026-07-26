# Blender Vision MCP

Blender Vision MCP is an evidence-bound reference-to-3D reconstruction coordinator. It imports
immutable references and existing Blender scenes, records camera authority and uncertainty, runs
Blender headlessly through an allowlisted manifest protocol, compares validation silhouettes, and
exports reproducible acceptance receipts.

The immediate ComputExchange target is an L3 dimensionally constrained visual twin. The software
does not equate a successful workflow with accepted L3 fidelity: the receipt carries explicit gates
and blockers, and the built-in camera initializer is labeled low-confidence and non-metric.

## Install

```bash
cd tools/blender-vision-mcp
uv sync --extra dev
uv run bvmcp doctor
```

No model weights are downloaded by installation or by `doctor`. Blender can be selected explicitly:

```bash
export BVMCP_BLENDER_PATH=/Applications/Blender.app/Contents/MacOS/Blender
```

Safe mode is on unless `BVMCP_UNSAFE=1` is set. Do not use unsafe mode with third-party assets.
The directory is an independently buildable project; clean wheel/source-archive installation and
release gates are documented in [`docs/RELEASE.md`](docs/RELEASE.md).
The capability/benchmark distinction and current Beast Mode evidence gaps are tracked in
[`docs/BEAST_MODE_STATUS.md`](docs/BEAST_MODE_STATUS.md).

The All-Seeing Eye expansion now includes a governed Chromium capture bus for static web evidence.
It records screenshots, DOM/accessibility/style/asset/network/console/performance artifacts,
publishes a content-addressed observation envelope and queryable `LayoutGraph`, and exposes
`vision.observe`, `vision.query`, and `vision.verify` over MCP. See
[`docs/PERCEPTION_CAPTURE.md`](docs/PERCEPTION_CAPTURE.md) for the authority, isolation, query, and
limitation contract.

Governed Figma and Storybook exports now compile into a shared `DesignSystemGraph`; component,
variant, source-symbol, and token drift is evaluated through `vision.compare`. See
[`docs/DESIGN_INTELLIGENCE.md`](docs/DESIGN_INTELLIGENCE.md).

Browser canvas/WebGL/WebGPU runtime evidence, fixed-time frame capture, explicit owned-scene glTF
materialization, and the safe Blender/GLB candidate round trip are documented in
[`docs/GRAPHICS_RUNTIME.md`](docs/GRAPHICS_RUNTIME.md).

ExperienceIR compilation and verified clean-room Feature Capsules are documented in
[`docs/FEATURE_CAPSULES.md`](docs/FEATURE_CAPSULES.md).

Bounded frontend candidate search, named-review CSS patching, and mandatory full-page regression
gates are documented in [`docs/FRONTEND_REPAIR.md`](docs/FRONTEND_REPAIR.md).

Static images, bounded video, authorized camera frames, and synchronized authorized desktop
snapshots now share the same evidence bus and region-explanation surface. See
[`docs/MULTIMODAL_PERCEPTION.md`](docs/MULTIMODAL_PERCEPTION.md).

Repository-to-runtime bindings, visual blast-radius planning, persistent specialist
findings, contradiction handling, compute accounting, and evidence-scored router
refutation are documented in
[`docs/PERCEPTION_WORKSPACE.md`](docs/PERCEPTION_WORKSPACE.md).

Authenticated capability routing for capture, specialist analysis, central
verification, lease recovery, and receipt-equivalent replay is documented in
[`docs/DISTRIBUTED_PERCEPTION.md`](docs/DISTRIBUTED_PERCEPTION.md).

Evidence-ranked correction queues, fixed-benchmark activation, supersession, and
receipt-backed rollback are documented in
[`docs/PERCEPTION_LEARNING.md`](docs/PERCEPTION_LEARNING.md).
The wave-by-wave proof and external benchmark boundaries are recorded in
[`docs/ALL_SEEING_EYE_STATUS.md`](docs/ALL_SEEING_EYE_STATUS.md).

## Reproducible calibration benchmark

Benchmark 0 is CC0 procedural evidence and needs no private references. It generates a known
120 × 80 × 40 mm technical enclosure, six labeled views, exact measurements, typed components and
features, reviewed appearance profiles, approved metric cameras, independent repeated renders,
editable BLEND and repeated GLB exports, residual comparisons, coverage, and both JSON and
Markdown receipts:

```bash
uv run bvmcp benchmark bootstrap-calibration \
  --project "$PWD/.local-projects/calibration" \
  --reviewer "Named benchmark reviewer" \
  --review-reason "Procedural ground truth and rendered evidence verified"
```

The command fails unless dimension, camera-recovery, scale-recovery, render-repeatability, and
export-consistency gates all pass and the final L3 receipt is valid and accepted. The distributable
benchmark definition and scene generator live under `benchmarks/calibration/`.

## First vertical slice

```bash
PROJECT="$PWD/.local-projects/mac-studio"

uv run bvmcp project create mac-studio --root "$PROJECT" \
  --scene ../../models/mac_studio/final_packed.blend
uv run bvmcp reference import ../../web/assets/site/mac-studio@3x.png \
  --project "$PROJECT" --rights-state INTERNAL --viewpoint-label front
uv run bvmcp blender inspect --project "$PROJECT"
uv run bvmcp project audit --project "$PROJECT"
uv run bvmcp vision solve-cameras --project "$PROJECT"
uv run bvmcp vision refine-camera --project "$PROJECT" \
  --source-solution-id "$CAMERA_SOLUTION_ID" --reference-id "$REFERENCE_ID"
uv run bvmcp validate compare --project "$PROJECT"
uv run bvmcp validate coverage --project "$PROJECT"
uv run bvmcp receipt export --project "$PROJECT"
uv run bvmcp project verify --project "$PROJECT"
```

For the governed repository benchmark, bootstrap the scene, three Apple-published dimensions, and
the legacy strict-audit artifacts without treating the website render as a photograph:

```bash
uv run bvmcp benchmark bootstrap-mac-studio \
  --repository-root ../.. \
  --project "$PROJECT"
```

The benchmark manifest is `benchmarks/mac_studio/benchmark.json`. It records Apple’s specification
source, the 197 × 197 × 95 mm envelope, the revoked legacy grille acceptance, and the missing raw
reference/provenance blockers. `--include-marketing-reference` is explicit because that image is
non-authoritative visual evidence.

The first parametric repair is the main rear hero grille. It has two distinct review gates:

```bash
PROPOSAL_ID=$(uv run bvmcp repair propose-mac-studio-grille --project "$PROJECT" \
  | python -c 'import json,sys; print(json.load(sys.stdin)["result"]["id"])')
uv run bvmcp repair approve "$PROPOSAL_ID" \
  --project "$PROJECT" --approved-by "Named evaluation authorizer"
uv run bvmcp repair apply "$PROPOSAL_ID" --project "$PROJECT"
uv run bvmcp receipt export --project "$PROJECT"
# Only after every blocker except final repair review is resolved:
uv run bvmcp repair review "$PROPOSAL_ID" --project "$PROJECT" \
  --accept --reviewer "Named reviewer" --reason "Reviewed against bound evidence" \
  --receipt-id "$POST_APPLY_RECEIPT_ID"
```

`repair approve` authorizes generation of an isolated evaluation checkpoint; it is not geometry
acceptance. Application never overwrites the imported scene and records mesh-topology,
aperture-ray, rear-render, feature, and component evidence. `repair review --accept` is refused
unless its verified post-apply receipt proves that final repair review is the only remaining L3
blocker. The repository benchmark still cannot satisfy L3 without recovered raw references,
metric cameras, residual comparisons, and human feature review.

Typed measurements can also be added directly:

```bash
uv run bvmcp measurement add known_overall_dimension \
  --project "$PROJECT" \
  --value '{"axis":"x","millimetres":197}' \
  --evidence-class MANUFACTURER_SPEC \
  --certainty bounded \
  --uncertainty '{"millimetres":0.5}'
```

Perspective-grid records use normalized image coordinates and retain vanishing lines/points,
rulers, calibration targets, symmetry axes, snapping configuration, multi-view links, scale
bindings, and uncertainty. They can initialize a focal length and Manhattan orientation without
claiming metric translation:

```bash
uv run bvmcp measurement grid-create "$REFERENCE_ID" --project "$PROJECT" \
  --created-by "Named perspective reviewer" \
  --definition grid.json
uv run bvmcp vision solve-vanishing-points --project "$PROJECT" --grid-id "$GRID_ID"
```

For a metric calibration-board solve, install the `vision` extra, record the measured board-square
pitch as `array_pitch`, then invoke `vision solve-calibration-board` with its measurement ID. The
OpenCV solution stores reprojection, feature-count, coverage, baseline, scale, principal-point, and
distortion quality fields and remains pending until named camera review. Measurement corrections
never overwrite history; use `measurement correct` to retain the prior value, uncertainty, reviewer,
reason, and timestamp.

Manufacturer specifications require more than a copied URL. `measurement.bind_source_provenance`
creates an immutable receipt binding the unchanged numeric claim and uncertainty to the current
target's rights-reviewed authoritative source, retrieval time, access policy, and claim locator.
Schema-v2 receipts require an acquired reference whose registered artifact bytes reproduce the
source content hash, and both `source_url` and legacy `source` declarations must exactly match the
governed origin. Acceptance rejects discovered-only, missing, corrupt, source-stale, or numerically
tampered provenance receipts. Source review records distinguish humans from automated policy agents;
a policy agent must bind its HTTPS terms sources, retrieval time, scope, and decision, may authorize
only internal use, and must prohibit redistribution.
Deterministic undistorted references likewise carry derivation receipts binding source and output
artifacts, source camera state, transform parameters, and inherited governed-source lineage; an
equivalent duplicate source is recognized only by identical artifact digest and explicit receipt
supersession.

Image-to-model PnP is a three-step governed workflow. `vision.propose_pnp_landmarks` stores
machine-generated coordinates as non-authoritative proposals bound to acquired model/image hashes,
an immutable intrinsics snapshot, measurements, backend identity, confidence, and limitations.
`vision.propose_pnp_landmarks_from_renders` can first compare hash-bound synthetic model views to
governed photographs with deterministic target/render scale pyramids, SIFT ratio matching, and the
strongest threshold-passing RANSAC or USAC-MAGSAC homography. Governed viewpoint labels restrict
which synthetic views may compete; anchors must lie in or near the robust render support hull and
are diversity-capped before review. The diagnostic records every attempted scale, estimator,
inlier, coverage, and reprojection result and refuses weak or tampered evidence; a successful result
is still only a machine proposal. Unreviewed machine proposals can be superseded by a receipt-bound
replacement without manufacturing point-review or camera authority. Render manifests bind the exact
model, anchor manifest, and every rendered pixel by SHA-256.
`vision.review_pnp_landmarks` requires a named decision on every point and independently checks that
each retained view has at least six non-coplanar correspondences. Only that immutable review receipt
can be passed to `vision.solve_pnp_landmarks`; final camera approval is still a separate named action.
Acceptance recomputes the review semantics and rejects missing, stale, corrupt, or newly hash-valid
forged landmark receipts.

`vision refine-camera` performs a bounded coarse-to-fine silhouette sweep over view direction,
framing margin and lens shift. It renders at most 375 candidates in three isolated batches, hashes
every candidate, stores a schema-validated refinement report, and creates a new pending camera
solution. Automatic refinement always emits `approximate_visual_registration`; it cannot promote a
feature-based or metric claim and does not replace named camera or mask review. Use `--stages 1` or
`--stages 2` for cheaper hypothesis pruning, and pass `--solution-id` to `blender render` or
`validate compare` when comparing a specific stored solution.

Geometry backends use one normalized, artifact-bound contract. The built-in safe lane can generate
silhouette evidence immediately; COLMAP is selected when installed and enough views register:

```bash
uv run bvmcp vision run --project "$PROJECT" --backend auto
uv run bvmcp vision list-evidence --project "$PROJECT"
uv run bvmcp vision compare-backends --project "$PROJECT"
```

Editable production candidates can be prepared through a bounded Blender transaction:

```bash
uv run bvmcp blender prepare-asset \
  --project "$PROJECT" \
  --targets preparation-targets.json
uv run bvmcp benchmark bootstrap-asset-preparation \
  --output artifacts/asset-preparation-run
```

This lane executes named retopology candidates, UVs, explicit PBR materials, real texture baking,
character-lite rigging/animation, object animation, LODs, collision hulls, and mesh repair. It then
audits the candidate BLEND, structurally validates its GLB, and audits a fresh GLB reimport.
Outputs remain review candidates; a successful operation receipt is not perceptual acceptance.

Camera/material/lighting acceptance uses immutable no-nudge camera states, structural PBR and light
inspection, independent real-Blender renders, exposure brackets, held-out views, pixel thresholds,
and mandatory corruption controls:

```bash
uv run bvmcp benchmark bootstrap-appearance \
  --output artifacts/appearance-run
```

The complete contract and its inference limits are in
[`docs/APPEARANCE_AUTHORITY.md`](docs/APPEARANCE_AUTHORITY.md).

Opaque references use automatic segmentation only as diagnostic evidence. To make a silhouette
authoritative for L3, import a same-size binary mask after a named human boundary review:

```bash
uv run bvmcp reference import-mask reviewed-front-mask.png \
  --project "$PROJECT" --reference-id "$REFERENCE_ID" \
  --reviewer "Named silhouette reviewer" \
  --reason "Boundary traced and checked against the immutable source image"
uv run bvmcp reference list-masks --project "$PROJECT" --reference-id "$REFERENCE_ID"
```

The normalized mask, original mask file, reviewer, reason, source reference, and SHA-256 bindings
are immutable receipt evidence. Automatic GrabCut and background estimates never satisfy this gate.

VGGT is an executable optional backend, but never a downloader. First approve and manually import
an exact checkpoint with `bvmcp model approve-source` / `bvmcp model import-checkpoint`; then install the
operator-reviewed upstream `vggt`, PyTorch, and NumPy runtime on a vision worker and invoke:

```bash
uv run bvmcp vision run --project "$PROJECT" --backend vggt-commercial \
  --configuration '{"model_installation_id":"<installation-uuid>","device":"auto"}'
```

The adapter constructs `VGGT()` directly, loads only the governed local checkpoint with PyTorch's
`weights_only=True`, forces Hugging Face/Transformers offline mode, writes depth/point/confidence
NPZ artifacts, converts OpenCV camera-from-world outputs to stored camera-to-world matrices, and
keeps scale/cameras non-metric until classical alignment and review. The original checkpoint is
stored as research-only evidence and cannot become commercial authority.

External GPU workers import depth, point, normal, visibility, and confidence outputs by existing
SHA-256 artifact digest using `vision import-evidence`. The import document must record backend
version, license/commercial-use state, checkpoint hash when applicable, evidence class, source frame,
canonical transform, scale, and uncertainty. Research-only results remain excluded from commercial
authority, and backend comparison retains scale-incompatible hypotheses instead of averaging them.

The complete path can be submitted as one recorded workflow:

```bash
uv run bvmcp workflow audit-reference-fidelity \
  --project "$PROJECT" \
  --maximum-dimension 1024
```

Add `--async` to queue supported commands, then run the single-user coordinator in another shell:

```bash
uv run bvmcp daemon --project "$PROJECT"
uv run bvmcp jobs --project "$PROJECT"
uv run bvmcp job cancel JOB_ID --project "$PROJECT"
```

## Review UI and Blender extension

The reviewer is loopback-only, serves only registered artifacts, and uses a random per-process
mutation token. It provides overlay/wipe and residual views, geometry evidence, feature/component
trees, perspective grids and measurements, camera and coverage maps, acceptance gates, named review
actions, workers, and jobs:

```bash
uv run bvmcp review serve --project "$PROJECT" --open
```

The unified queue also surfaces advisory role handoffs, optimization proposals, and governed
2D/3D landmark proposals. Existing image bytes that predate the source ledger can be wrapped in an
artifact-bound legacy-reference adoption proposal without changing or duplicating the image. The
proposal has no rights authority: a named reviewer must either exclude it or explicitly supply its
origin, publisher, target variant, viewpoint, quality, terms/privacy reviews, and internal-use and
redistribution decisions. Adoption writes an immutable decision receipt and links the exact prior
reference to the source and rights ledgers; exclusion removes that image from acceptance evidence.
DGX projects also surface their required three-tier foam-LOD policy as an editable named review;
the queue clears it only when the content-addressed receipt remains semantically valid.
Landmark review overlays every proposed image point and requires an
explicit accept, reject, or five-coordinate correction for every point before submission; it never
approves the resulting camera. Camera cards display registration authority and warn when a
relative-scale or approximate solution can freeze comparison framing but cannot satisfy L3 or an
external Beast camera gate. The queue preserves every stored proposal but groups review choices by
Pareto dominance: a card can summarize another only with equal-or-better authority, reference
coverage, per-reference confidence, and stored silhouette fit. Surviving cards show source images,
fit scores, and acceptance-reference coverage. Browser actions additionally cover evidence
approval, applied-repair validation renders with topology/ray/dimensional diagnostics, revisioned
measurement correction, cross-view feature linking, camera rejection, component review, capture
requests, and model-tier decisions. Capture and tier decisions are stored in receipts; a human tier
decision cannot accept fidelity beyond the latest valid accepted receipt.

`blender_extension/` is a thin Blender 4.2+ client. It selects a project, submits allowlisted CLI
workflows, polls/cancels jobs, and opens the reviewer; it does not run a listener, database, model,
or reconstruction engine inside Blender. See `blender_extension/README.md` for validation and
packaging commands.

## Synthetic data, models, and appearance

Synthetic plans are immutable, seed-bound records. Local Blender generation emits beauty and
instance-label PNGs, multilayer depth/normal EXR, camera matrices, keypoints, object IDs, lighting,
and material metadata in batches; distributed generation uses the same governed completion path:

```bash
DATASET_ID=$(uv run bvmcp dataset plan-synthetic calibration-labels \
  --project "$PROJECT" --sample-count 128 --seed 17 | jq -r .id)
uv run bvmcp dataset generate "$DATASET_ID" --project "$PROJECT"
```

Feature detections and fitted parameters remain proposals until named review. Gaussian splats and
NeRFs may be registered as appearance-only visual oracles and can never establish dimensions or
hidden geometry. Analytical, black-box, differentiable, and learned optimizers preserve evidence
authority and produce reviewable proposals.

Material profiles record base color, roughness, metallic, anisotropy, clearcoat, normal detail,
procedural texture, calibration/lighting evidence, reflective masks, uncertainty, and confidence.
They are explicitly appearance-only; geometry gates use mask, depth, normal, and feature passes
independently of RGB. Use `bvmcp material create`, `material review`, and `material list`, or the
equivalent MCP tools.

The coordinator never downloads weights. A model checkpoint must have an approved HTTPS or `hf://`
source, license record, named reason, expected SHA-256, manual acquisition, digest-matched import,
and immutable revision before it can be used.

## Distributed workers

Mac Studio, RTX 5090, and DGX-class workers use the same pull protocol. Each worker advertises a
class, hardware labels, memory, render devices, warm models, and an allowlist of operation
capabilities. The coordinator filters by hard requirements, then scores cache locality, preferred
hardware/models, VRAM, and load. Jobs use renewable 15–3600 second leases; expired or explicitly
retryable attempts are requeued up to a bounded attempt count.

Enroll from a trusted machine with local project access. The returned worker token is shown once:

```bash
uv run bvmcp worker register "Mac Studio" --project "$PROJECT" --worker-class blender \
  --capabilities '{
    "hardware":["apple-silicon","metal","mps"],
    "vram_gb":64,"system_memory_gb":64,"supported_models":[],
    "render_devices":["METAL"],"capabilities":["blender.*","dataset.*"]
  }'
uv run bvmcp worker list --project "$PROJECT"
uv run bvmcp worker reap-expired --project "$PROJECT"
```

On a trusted machine with the portable project mounted locally, start the packaged pull loop with
the one-time worker id and token returned by registration. The token is read from the environment
so it does not need to appear in the process arguments:

```bash
export BVMCP_WORKER_TOKEN='the-one-time-secret'
uv run bvmcp worker run --project "$PROJECT" --worker-id "$WORKER_ID"
# smoke-test exactly one claim, including the idle case
uv run bvmcp worker run --project "$PROJECT" --worker-id "$WORKER_ID" --once
```

Typical NVIDIA labels are `['nvidia-rtx-5090','cuda','optix']` for a vision/render worker and
`['nvidia-dgx','cuda']` for a training worker. Remote workers call the `worker.heartbeat`,
`worker.claim`, `worker.renew`, `worker.complete`, and `worker.fail` MCP tools. Input and output
artifacts move through authenticated, sequential 1 MiB chunks and are accepted only when their
declared size and SHA-256 digest match. Dataset and training completions additionally update their
governed records; arbitrary remote commands are never accepted. Workers can explicitly abort their
own partial uploads, and stale unregistered parts can be reaped without touching accepted artifacts.

Remote MCP enrollment is disabled unless the coordinator has a strong
`BVMCP_WORKER_ENROLLMENT_TOKEN`. Put TLS and client authentication in front of any non-loopback MCP
transport, keep worker and lease tokens out of logs, and rotate the enrollment token after adding
workers. Queue synthetic generation with `dataset.generate(execution="distributed")`; planned
feature training is queued for a compatible training worker automatically.

## MCP configuration

The distribution exposes both `bvmcp serve` and `blender-vision` stdio entry points. Example host
configuration:

```json
{
  "mcpServers": {
    "blender-vision": {
      "command": "uv",
      "args": [
        "--directory",
        "/absolute/path/to/tools/blender-vision-mcp",
        "run",
        "blender-vision"
      ],
      "env": {
        "BVMCP_PROJECTS_ROOT": "/absolute/path/to/projects"
      }
    }
  }
}
```

Preferred MCP operation: `workflow.audit_reference_fidelity`. It returns a job ID immediately;
poll `job.status` until the state is terminal. Lower-level tools remain available for inspection and
recovery.

For autonomous target work, use `workflow.reconstruct_from_public_evidence` (or the user-capture
variant), then `workflow.continue_autonomous`. Both return a compact recomputed progress envelope;
`workflow.progress` retrieves the same read-only view on demand. Evidence-supported surface regions
remain distinct from accepted fidelity, and only verified transactional acceptance can complete
delivery. Autonomous continuation advances only from receipt-valid camera, render, comparison,
evaluation, and lifecycle evidence. Candidate evaluation and promotion are explicit resumable
authority pauses with persistent role tasks; after named review, `resume_paused=true` continues
through verified BLEND/GLB delivery rather than trusting raw database flags or row counts.

## Project format

Each project is portable and owns `project.json`, SQLite in WAL mode, immutable original references,
scene copies, camera evidence, renders, comparisons, exports, receipts, and a SHA-256 artifact tree.
The canonical frame is right-handed, Z-up, in millimetres.

## Evidence and acceptance

Camera records distinguish body-box alignment, approximate visual registration, feature-based
solutions, and metric solutions. The `auto` camera lane attempts installed COLMAP first and records
the reason for any fallback; the deterministic turntable fallback and automatic silhouette
refinement remain non-metric. Refinement runs, candidate hashes, segmentation authority and framing
parameters are included in receipts. Named camera approval and rejection are separate immutable
decision artifacts committed atomically with the camera solution pointer. Acceptance reconciles the
artifact, decision ledger, complete camera snapshots, reviewer, reason, time, and state; an
interrupted or tampered decision cannot create camera authority. Legacy named decisions can be
wrapped without a new approval only through a replayable migration receipt that preserves the exact
pose and intrinsics and records any deterministically populated state. An L3 receipt
requires approved metric cameras, x/y/z authoritative dimensions, a human-approved technical
feature graph, a registered GLB export, complete reference comparisons, silhouette IoU at or above
the stored threshold, a clean scene audit, reviewed reference provenance, and any project-declared
material/calibration gates. A cryptographically valid receipt may still be rejected by its fidelity
gates; these are separate claims by design. JSON receipts are paired with human-readable Markdown
summaries and keep authoritative Blender and export artifact hashes. Every authoritative residual
comparison also has an immutable receipt binding the reference input, any reviewed mask or locality
crop, the governed render, residual image, comparison engine, metric values, and timestamp.
Acceptance independently replays the comparison and requires both the metrics and residual SHA-256
to match, so editing a plausible score directly in SQLite cannot create evidence. Beast benchmark
audits use the same verified lifecycle decisions and require an authoritative-scene render suite
covering every eligible image, a replayable comparison for each view, and authoritative-scene-only
BLEND/GLB delivery; evidence belonging to another candidate cannot satisfy a stage.

## Visual-geometry convergence

Real benchmark projects now require receipt-bound fixed visual-geometry rigs. A rig freezes the
scene artifact, complete camera snapshots, resolution, framing, lighting policy, color state, and
the required beauty/exposure, neutral-clay, material-neutral, left/right/top grazing, silhouette,
depth, smoothed/geometric normal, curvature, wireframe, object/component/feature-ID, zebra, and
reflected-line passes plus neutral-grey, white, and black validation backgrounds. Governed ID
passes use Cycles integer Object Index, reject fractional or ungoverned IDs, and encode the result
as an exact 8-bit palette PNG. Component masks must also agree with governed projected object bounds
before they receive exact-rendered-mask authority. Unapproved cameras create a
`DIAGNOSTIC_PROPOSAL` rig and can never satisfy L3. Each benchmark can also freeze an immutable
comparison baseline that binds its scenes, lifecycle, evaluations, renders, rigs, scorecards,
audits, exports, cameras, references, masks, and artifact manifest without claiming that a
diagnostic baseline is accepted authority.

New maximal rigs preserve that frozen 25-pass replay contract and add real Blender
`normal_discontinuity` and continuous `highlight_flow` diagnostics. The former measures
screen-space smooth/geometric-normal disagreement and adjacent geometric-normal changes; the
latter exposes radius flow, waviness, and pinching under a normal-driven band field. Both remain
diagnostic until intended edges and comparable reference evidence are classified.

Every visible mesh now receives a stable semantic proposal in an explicit `UNBOUND`,
`PROVISIONALLY_BOUND`, `REVIEWED_BOUND`, or `ACCEPTED_BOUND` lifecycle. Machine classification can
propose or supersede an unreviewed proposal, but a separately receipted named review and acceptance
are required before the binding can support L3. Parent assemblies and component relations form a
manufactured assembly graph rather than relying on Blender object names.

Scorecards replay silhouette IoU/Dice/precision/recall and boundary RMSE/Chamfer/P95 from stored
artifacts. Edge structure and perceptual values are explicitly supplemental because unclassified
reference edges may come from geometry, materials, reflections, shadows, or texture. Missing depth,
normal, curvature, landmark, component-mask, or rear-view ground truth is reported as unavailable;
it is never inferred from a rendered pass. The manufactured-form auditor independently checks
degeneracy, duplicates, normals, manifold topology, near-zero detail, repetition consistency, and
semantic binding coverage while recording blind spots that still require human grazing, curvature,
wireframe, contact, or measurement review. Both scorecards and audits are versioned and replayable,
so evaluator upgrades do not rewrite earlier evidence. Native-resolution component packets bind the
reference crop, candidate and baseline crops, diagnostic pass crops, semantic binding, current
parameters, landmarks, confidence, history, and local metrics. Blender's bundled OpenImageIO
decoder extracts receipt-bound component depth and signed-normal PNG crops from governed multilayer
OpenEXR while retaining channel names, valid-pixel counts, depth range, crop coordinates, and source
artifact digest. A visual-frequency scorecard separately reports primary, secondary, and tertiary
form plus whole-object, semantic-weighted, visible-area, visual-importance-weighted, worst-five,
and minimum-mandatory-component results. A small mandatory component cannot disappear inside an
enclosure average.

Residual diagnosis is also replayable. It classifies discrepancies through the governed
18-category defect taxonomy and binds each diagnosis to a component, views, exact pixel regions,
supporting passes, bounded candidate parameters, confidence, expected visual and gate impact, and
a hash-verified rollback scene. Diagnoses can request evidence or propose a bounded repair action,
but cannot mutate geometry, accept evidence, or promote a candidate.

MCP clients use `visual_geometry.freeze_baseline`, `visual_geometry.bind_scene`,
`visual_geometry.review_binding`, `visual_geometry.repropose_binding`,
`visual_geometry.relate_components`, `visual_geometry.create_component_packet`,
`visual_geometry.score_frequencies`, `visual_geometry.create_rig`, `visual_geometry.evaluate`,
`visual_geometry.diagnose_residuals`,
`visual_geometry.audit_manufactured_form`, `visual_geometry.repair_degenerate_candidate`,
`visual_geometry.list`, and `visual_geometry.verify`.
A real L3 benchmark requires an approved-camera rig, accepted bindings for every visible mesh, a
replay-valid passing component/frequency scorecard for every acceptance reference, and a clean
manufactured-form audit on the authoritative promoted scene in addition to the existing lifecycle,
transaction, provenance, measurement, export, and calibration gates.

Comparison production seeds and serializes OpenCV GrabCut at its exact call boundary so output is
byte-deterministic under concurrent execution. Engine v2 retains its 1024-pixel replay contract;
v3 bounds new automatic segmentation to 512 pixels and caches a mask only by immutable reference
digest and engine during an audit. Every render-specific metric and residual is still independently
recomputed. An older comparison that cannot replay is never re-signed in place: VisionMCP recomputes
it from the immutable bound reference and render, then inserts the replacement and links the retained
legacy row through a separately verified supersession receipt in one database transaction. A failed
link rolls back the replacement row. Replay-identical duplicates are retained but excluded only by
their own verified collapse receipt. Supersession repairs history only; it grants no camera, scene,
evaluation, or promotion authority.

## Development

```bash
uv run ruff check .
uv run pytest
uv build
uv run python scripts/verify-wheel.py dist
/Applications/Blender.app/Contents/MacOS/Blender --command extension validate blender_extension
/Applications/Blender.app/Contents/MacOS/Blender --command extension build \
  --source-dir blender_extension --output-dir dist
```

Blender integration tests are opt-in with `BVMCP_RUN_BLENDER_TESTS=1`. The local Mac Studio smoke
script is `scripts/mac-studio-smoke.sh`. The reviewed trust boundaries and residual risks are in
[`docs/SECURITY_REVIEW.md`](docs/SECURITY_REVIEW.md).
