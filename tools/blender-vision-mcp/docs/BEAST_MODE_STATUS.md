# Beast Mode implementation and benchmark status

VisionMCP now implements the Beast Mode control plane across target resolution, governed evidence
acquisition, video intelligence, coverage atlases, camera snapshots, reconstruction portfolios,
semantic graphs, parametric Blender geometry, optimization, transactional candidate lifecycle,
campaign orchestration, resource profiles, delivery, and cryptographically verifiable receipts.

Implementation availability is not benchmark acceptance. The evidence-derived benchmark auditor
records `PASSED` only when every stage check is present in the project database and its artifacts.
It does not accept a declaration or infer lifecycle state from file names.

## Current verification

- Fast suite: 296 passed, 3 opt-in Blender tests skipped.
- Fresh persistent real Blender calibration (`.local/benchmarks/calibration-strict-20260721`):
  passed, including an approved immutable metric camera set, two complete governed render suites,
  lifecycle transaction and promotion, editable BLEND, repeated GLB, and receipt verification. A
  fresh post-migration receipt (`d2926048-2bd7-445f-bc76-62aaa708bc4a`) is accepted with zero
  blockers and verifies its payload and all referenced artifacts.
- Real Blender integration suite: 2 passed, including the governed renderer and the fresh
  18-component Perseverance parametric seed.
- Ruff: passed.

## Maximal visual-fidelity convergence update — July 25, 2026

VisionMCP now freezes immutable visual baselines, binds every visible mesh through a governed
semantic lifecycle, builds manufactured assembly graphs, produces native-resolution component task
packets, and scores primary/secondary/tertiary visual frequencies without allowing whole-object or
area-weighted averages to conceal a failed mandatory detail. Zebra and reflected-line diagnostics
are part of the governed Blender pass suite.

The original three live baselines are receipt-valid but intentionally diagnostic: Mac freeze
`4ecd5d3a-6d7c-4835-857c-f1bad04ba15d`, DGX freeze
`1afe02ba-4f94-4bd6-9e0d-5a2457fe125d`, and RTX freeze
`b58efeec-037f-4686-a18a-7fab81873de8`. Current maximal Mac freeze
`fa2989f9-cf46-4ca3-8b18-945eaa2eb9f5` adds the industrial diagnostics and residual diagnoses
without modifying those original snapshots. No freeze grants acceptance authority.

Visible-mesh proposal coverage is complete with zero unbound meshes: Mac authoritative/candidate
36/37, DGX 39/39, and RTX 252/257. Every binding remains `PROVISIONALLY_BOUND`; none is represented
as reviewed or accepted. The latest Mac repair-candidate run has four replay-valid packets for the
objects actually visible in its front view and blocked visual-frequency scorecard
`0d00e8ca-94b9-455d-ace3-ac0e3051f013`: primary 0/4, secondary 0/16, and tertiary 0/17 components
have acceptance-valid scores because reviewed local masks and accepted bindings do not exist.
Earlier projected-bounds packets remain preserved as diagnostic history.

The latest Mac run is `393814bb-9df7-4020-940c-fcee5b82cc3f`: 27 required maximal passes and 28
output keys including the retained instance-mask alias. The frozen 25-pass governed minimum remains
unchanged for historical receipt replay; new maximal rigs add normal-discontinuity and continuous
highlight-flow diagnostics. The run uses Cycles integer Object Index for
occlusion-preserving mesh identity, converts governed integer values to exact 8-bit PNG palettes,
and retains the projected-bounds comparison as defense in depth. Four visible identities pass that
cross-check (`mac-studio`, `mac-studio-led`, `usbc-tongue`, and `usbc-tongue.001`); the former
enclosure-sized false USB-C assignment is absent. Blender's bundled OpenImageIO now decodes the
shared multilayer EXR into receipt-bound 16-bit depth and signed-normal component crops. All 18
legacy candidate diagnostic crops plus normal-discontinuity and highlight-flow are available in
each latest packet.

The exact integer mask changes the evaluation representation relative to the older hue-derived
silhouette; it is more truthful, not a geometry change. Latest whole-object projection is
0.96355325 IoU and 11.70062923 px boundary RMSE at 2048-pixel reference scale, with all five
projection gates passing diagnostically. The isolated RJ45 repair candidate removes both zero-area
faces and four coincident vertices without changing its envelope. Its replay-valid
manufactured-form audit has zero hard failures, but the candidate is not accepted or promoted. A
fresh Mac acceptance receipt remains blocked by 18 requirements, and fresh Beast Stage 1 audit
`2059ccb2-4b23-40c9-b969-8bdc1dd751f6` remains `INCOMPLETE` at 1/7. DGX and RTX remain blocked by
18 and 21 acceptance requirements respectively.

The live normal-discontinuity engine measures 274,322 valid pixels, 44,426 pixels above 0.25°,
1.06977° mean, 2.41994° P95, and 167.87007° maximum. It exposes rounded-side tessellation,
top/front transitions, port boundaries, and intended hard edges without treating them as accepted
defects. Receipt-valid diagnosis `ccfd5829-15a6-4237-88a2-39f4283fecab` translates the scorecard
and component packets into nine bounded actions: seven `EVIDENCE_MISSING` and two
`EVIDENCE_CONFLICT`. Every action binds views, image regions, passes, parameters, confidence,
expected visual/gate impact, and rollback scene, while carrying no repair or acceptance authority.
Current maximal freeze `fa2989f9-cf46-4ca3-8b18-945eaa2eb9f5` preserves this state and 458 bound
artifacts without modifying the original Mac, DGX, or RTX freezes.

## Overall accuracy audit

VisionMCP has proven high accuracy only in the controlled synthetic calibration: accepted L3, six
approved metric views, 0.98332 mean and 0.97010 minimum silhouette IoU, all three dimensions within
0.001 mm, a passed seven-category transaction, verified promotion, and BLEND/GLB delivery. This is
strong closed-world proof, not proof of real-object generalization.

The four real Beast projects currently pass 9 of 35 stage gates (25.7%), which measures workflow
completion rather than geometric accuracy. None has an accepted fidelity tier. Mac has no active
authoritative comparison suite, but its scoped 197 × 197 × 95 mm enclosure passes every axis within
0.000004 mm. DGX matches its governed
150 × 150 × 50.5 mm envelope, but its six current authoritative comparisons average only 0.62047
IoU and its cameras/masks are unapproved. RTX numerically matches its envelope but lacks accepted
measurement provenance, has zero active comparisons, resolves zero of 39 atlas cells, and leaves 39
of 41 semantic nodes pending. Perseverance supports 17 of 30 active surface cells but has no active
comparison suite, no metric cameras, 35 of 36 semantic nodes pending, and scene-envelope residuals
of -524.16, -500.16, and -1193.28 mm.

Accordingly, evidence governance and synthetic calibration accuracy are strong; real multiview
silhouette accuracy is weak or absent; semantic, material, and hidden-geometry accuracy remain
unproven; and autonomous real-world L3 delivery has not been demonstrated. Historical comparison
scores are retained for diagnosis but are not reported as current acceptance accuracy.

The benchmark auditor now recomputes immutable camera hashes and verifies scene, transaction,
render-pass, video, mask, and export artifacts against the content-addressed store and materialized
files. Discovered-but-unacquired sources and empty semantic graphs cannot satisfy Stage 3 gates.
Promotion now requires the same verified passed transaction used for acceptance. Authority
replacement is one database transaction, and each displaced scene receives its own hash-verified
`SUPERSEDED` transition receipt naming the replacement scene and governing evaluation. Acceptance
and Beast audits reconstruct the full lifecycle chain and reject direct database state changes,
tampered evaluation receipts, invalid transition receipts, and unreceipted supersessions.
Failed seven-gate decisions are atomic as well: the evaluation record, automatic rejection state,
and linked `CANDIDATE` → `REJECTED` receipt commit together or all roll back. A simulated transition
write failure proves no failed evaluation can be stranded beside a still-active candidate.
Evaluation verification independently recomputes missing categories, failed gates, numerical
regressions, improvements, aggregate-improvement policy, and final status. A freshly registered
but semantically forged receipt cannot turn a failing mandatory gate into a passed transaction.
Camera approval and `camera.freeze` now use the same canonical structural validator as the auditor:
they recompute the snapshot hash and reconcile resolution, intrinsics, extrinsics, coordinate
transform, crop, clipping, source identity, distortion, sensor, and solve-method state. A present or
even attacker-recomputed hash cannot conceal inconsistent camera fields. Approval and rejection
also write a separate immutable decision artifact and ledger row in the same database transaction
as the solution state. Acceptance requires semantic agreement across those records. Simulated
write failure rolls back both, and ledger tampering blocks acceptance. Existing named legacy
decisions may be migrated without a new review only when their reviewer, reason, and timestamp are
present; the schema-v2 migration embeds the old snapshot, deterministically completes missing
derived fields, proves exact pose/intrinsics preservation, and is replayed during verification.

Residual comparison authority now has the same transaction and replay boundary. Each comparison
receipt binds the exact reference input, optional reviewed mask and locality crop, governed render,
residual artifact, engine, metrics, and timestamp. Producers refuse non-reproducing values before
insertion; acceptance and autonomous continuation independently reproduce both the metrics and
residual digest. Database score edits, corrupt bound artifacts, forged bounded metrics, and
interrupted inserts are covered by tests. Legacy rows are wrapped only when deterministic replay is
exact. OpenCV GrabCut is serialized and seeded at the call boundary so a newly produced comparison
replays byte-for-byte. The versioned v3 engine bounds automatic segmentation to 512 pixels while
v2 retains its original 1024-pixel replay contract. Verification caches each immutable reference
segmentation per engine but still recomputes every render-specific metric and residual. Older
unreproducible rows can be replaced only by deterministic recomputation from their immutable
reference/render inputs and an immutable supersession receipt that preserves the authority boundary;
replacement insertion and source supersession now commit in one transaction. A simulated failure
proves no interrupted replacement row can acquire authority. Replay-identical duplicates can be
collapsed only through a separate immutable receipt and remain preserved in history.

Reference segmentation now has its own proposal/review authority boundary. VisionMCP can derive a
bounded deterministic mask from an immutable acceptance image, but records it only as a replayable
machine proposal with `MACHINE_PROPOSAL_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY`. A separate named
accept/reject action writes an immutable decision receipt; proposal state and any approved,
high-confidence mask commit in one database transaction. Acceptance verifies both receipts and the
approved mask linkage, rejects invalid proposal ledgers, and excludes pending, low-confidence, or
wrong-use masks. A simulated decision-write failure proves an approval row cannot be stranded.
Schema-v1 verification now also requires the complete governed proposal fields, canonical component
sets, an in-bounds integer ROI, the v3 bounded-segmentation configuration, coherent ledger state,
and a complete approved-mask snapshot in the named decision. A newly registered hash-valid receipt
with an empty approval snapshot or invalid ROI cannot acquire authority. Idempotent reuse keys the
creator, backend, intended use, components, and ROI, so a changed review scope creates a new immutable
proposal instead of returning stale evidence.

The Beast stage auditor now consumes the same lifecycle-recomputed evaluation authority, accepts
only complete every-reference governed render suites belonging to the authoritative scene, replays
every linked comparison receipt, and counts BLEND/GLB delivery only for that scene. Fresh persistent
audits keep Mac Studio at 1/7, DGX at 1/7, RTX 5090 at 2/11, and Perseverance at 5/10. The five
formerly invalid Mac comparisons now have deterministic, replayable replacements and verified
supersession receipts, removing the invalid-history blocker without creating an authoritative
comparison suite or changing the 1/7 score. All 38 formerly invalid DGX rows now have deterministic
replacements and verified supersession receipts as well. One replay-identical replacement created
during an interrupted pre-transaction run is preserved under a duplicate-collapse receipt. Fresh
DGX audit has zero invalid comparison IDs, 39 governed supersessions, and 42 verified comparisons,
but no approved metric camera/render suite; Stage 2 therefore remains correctly at 1/7.

Mac now has a receipt-valid canonical target, one artifact-bound legacy-reference adoption proposal,
one replay-valid full-object mask proposal, and a complete unapproved known-scale camera hypothesis.
The camera binds all three manufacturer dimensions and records 0.96699 silhouette IoU, 0.98322 Dice,
and 10.642 px full-resolution boundary RMSE. Blender-evaluated body scope also removes the false
1.85001 mm depth failure: all three 197 × 197 × 95 mm enclosure axes are within 0.000004 mm. None of
these proposals creates review authority. The current fixed diagnostic repair-candidate rig renders
the 27-pass maximal visual-geometry suite while preserving the historic 25-pass governed minimum;
its integer-object-index scorecard reports 0.96355 IoU, 0.98144 Dice, and 11.701 px boundary RMSE
at the 2048 px reference scale. All 37 rendered objects have valid provisional semantic bindings.
The four objects actually visible in this front view have exact object-index masks, governed
projected-bounds agreement, and complete 20-pass component crops including directly decoded depth
and normals, normal-discontinuity, and highlight-flow. The separate candidate manufactured-form audit
removes the former two exact `mac-rj45-tab` degeneracies and has zero hard failures, but it cannot
alter authoritative state without named review and the complete promotion transaction. Fresh Stage
1 audit `2059ccb2-4b23-40c9-b969-8bdc1dd751f6` remains 1/7 because
rights, mask, camera, repair acceptance, semantic features, authoritative comparisons, transaction,
promotion, and BLEND/GLB delivery are still incomplete.

DGX now also has an exact resolved target and one live governed manufacturer source acquired through
VisionMCP's robots-, redirect-, address-, size-, and timeout-checked downloader. NVIDIA's official
hardware HTML is stored under digest
`940968f69e4ad325a21bd88673fd9a6a15967ce0ad9fa8f4697e835c8adfb07b`; its access record explicitly
identifies an automated policy agent, binds the official terms URLs and retrieval time, limits use to
the user's personal non-commercial internal review, and prohibits redistribution. Policy-agent
reviews cannot enable redistribution. Schema-v2 measurement receipts now require acquired,
hash-verified source bytes and check both modern `source_url` and legacy `source` declarations.
The unchanged 150 × 150 × 50.5 mm claims verify under provenance digests `0cc4733b...`,
`ecf279e0...`, and `01381cb0...`, removing the manufacturer-provenance blocker. Six artifact-bound
legacy-image adoption proposals remain deliberately unreviewed, so stricter acceptance now exposes
their missing source-ledger governance rather than trusting old rights labels.
General source authority now has two append-only, content-addressed ledgers: one for the named
terms/privacy/rights decision and one for acquired bytes bound to that current decision. Local and
URL acquisition, progress, autonomous continuation, coverage, derived-reference lineage,
manufacturer measurements, landmark inputs, and acceptance all replay these ledgers rather than
trusting mutable `ACQUIRED` or reviewer fields. Review and acquisition writes use compare-and-swap
and transactional ledger commits; hash-valid semantic forgeries, skipped supersession links, and
simulated write failures cannot create authority. Legacy named reviews migrate only through a
schema-v2 wrapper embedding the untouched prior source snapshot and `new_review_performed: false`;
acquired sources additionally rehash both artifact-store and materialized bytes. DGX's policy-agent
review now verifies under governance digest `5b15dc3f...` and acquisition digest `dbe6e432...`.
Perseverance has seven valid governance rows and five verified acquisitions; its two discovered-only
sources remain deliberately non-acquired. No source review or permission was invented.
Canonical target identity now has its own append-only, content-addressed resolution ledger.
Deterministic resolutions and clarification proposals bind the exact request, target snapshot,
alternatives, ambiguity replay, revision, time, authority, and immediate predecessor. Compare-and-swap
prevents concurrent rebinding; target/event insertion is atomic; acceptance, progress, autonomy,
source governance, parametric seeds, and Beast audits reject an unreceipted or semantically forged
target. Schema-v2 migration embeds the untouched legacy target record and explicitly states
`new_resolution_performed: false`. Maintained DGX, both RTX revisions, and Perseverance now verify
under migrated target authority without changing identity; Mac and strict calibration had no target
rows. Hash-valid ambiguity forgeries, skipped supersession, migration falsification, and interrupted
writes are covered by adversarial tests.
Those same six images now also have six replay-verified automatic mask proposals. All remain
`PROPOSED` at medium confidence: none is an approved reference mask and the high-confidence-mask
acceptance blocker remains active. Fresh acceptance artifact
`e428375d735ff385f13723e1feca873f6c05ca7d7b48907d6771c57d976bddeb` reports six pending, zero
approved, zero rejected, and zero invalid proposal receipts. Fresh Stage 2 audit
`6ca7d89f1a5bc722f885f61492a45b04ce1b3174a33bb9bd3a55fcab073688ed` therefore remains 1/7.

DGX foam-LOD policy authority now lives in an append-only named-review ledger rather than mutable
project metadata. Each decision is schema-strict, content-addressed, linked to the prior decision,
and committed with compare-and-swap protection so concurrent reviewers cannot silently overwrite
one another. Verification replays the exact receipt, reviewer, reason, authority, strategy, and
immediate-predecessor supersession chain and requires coverage of every current acceptance image view. Boolean coercion,
non-finite thresholds, tier extensions, metadata-only receipts, hash-valid semantic forgeries, and
interrupted ledger writes cannot create authority. The live DGX ledger remains empty and its single
six-view policy task remains deliberately pending; no approval was manufactured.

Legacy-reference adoption now receives the same semantic verification boundary. Proposal replay
checks the complete schema, current canonical target, immutable reference snapshot, artifact-store
bytes, materialized bytes, limitations, and suggestion record. An adoption review requires the
resolved manufacturer/model, a complete source and access-policy snapshot, canonical rights with
explicit internal use, coherent review times, and exact reconciliation across the proposal,
evidence-source row, rights ledger, and reference rights state. Exclusions must carry no hidden
source identity. Hash-valid target-variant and exclusion forgeries become acceptance blockers;
malformed hash-valid documents cannot crash receipt export. A simulated review-write failure proves
source, rights, reference state, and proposal decision all roll back together.

Autonomous continuation now uses verified runtime facts instead of raw row counts. A camera must
have a valid decision receipt and complete metric coverage; a render suite must bind a valid scene,
the approved camera ensemble, every eligible reference, every governed pass, content-addressed
bytes, and its materialized output; comparisons must bind those render bytes and valid residuals;
passed evaluations and promotion must survive the lifecycle receipt audit. Forged approval,
evaluation, promotion, missing render materialization, incomplete pass coverage, and corrupt
comparison evidence cannot advance the controller. Candidate evaluation and promotion now create
explicit persistent role tasks and pause the campaign. A tested named-review resume proceeds from
verified promotion through delivery and closes the campaign only after accepted receipt evidence.

Landmark PnP no longer accepts coordinates and a reviewer name in the same call. Machine proposals
are bound to acquired image/model hashes, an immutable intrinsics snapshot, authoritative dimension
IDs, backend identity, point confidence, methods, and declared limitations. A separate named review
must decide every point and retain six non-coplanar correspondences per view. Acceptance independently
recomputes that decision and blocks PnP camera authority for a missing, stale, corrupt, or semantically
forged receipt; PnP output remains pending until the ordinary separate camera approval.
The render matcher now uses deterministic target/render scale pyramids, governed viewpoint filtering,
RANSAC/USAC-MAGSAC selection under unchanged thresholds, robust render-support filtering, and bounded
2D/3D diversity. Replaced unreviewed proposals move to `SUPERSEDED` only through an immutable receipt
that explicitly carries no review or camera authority; only the replacement remains actionable.

The user-facing workflow now returns a compact, digest-bound progress report at launch and after
every autonomous continuation, with an independently callable read-only `workflow.progress` tool.
It recomputes target resolution, sources surviving conflict/duplicate filtering, rights review,
authoritative dimensions, video/keyframes, surface coverage, portfolio state/current best, gates,
missing-evidence requests, promotion, delivery, and the next action. Campaign-authored status text
cannot alter acceptance. Regional `EVIDENCE_SUPPORTED` status is deliberately distinct from an
accepted fidelity scope, so exterior coverage alone cannot become an L3 claim.

Evidence compatibility is now a governed prerequisite rather than a best-effort note. Target-field,
market, option-package, prototype, modification, aftermarket, mirroring, editing, and partial-crop
conflicts produce content-addressed audits and cannot contribute to canonical coverage or acceptance
while unresolved. Explicit named decisions may confirm a false positive, exclude or limit a source,
or place it in a separately described configuration branch that is never silently merged. Decisions
bind the exact finding hash and become stale if source metadata changes. Lens, perspective, and crop
inconsistencies remain diagnostic warnings and never establish camera or geometry authority.
Exact, resized/recompressed, and mirrored image copies are also fingerprinted across acquired
target evidence. They collapse into one authority/quality/resolution-ranked canonical observation,
cannot inflate directional coverage or benchmark acquisition counts, and produce a
content-addressed duplicate-audit receipt without deleting the source or altering its rights.

A built-in CPU visual-hull backend now performs bounded voxel carving from two or more reviewed
full-object masks matched to complete immutable cameras. It writes a content-addressed editable PLY
mesh and occupancy/governance report, remains non-metric and non-concavity authority unless
independent evidence says otherwise, propagates source redistribution limits, and invalidates
queued results when camera documents or mask revisions change. Receipt verification now traverses
every normalized geometry artifact field and rejects a missing or corrupt hull mesh or occupancy
record.

## Beast benchmark stages

| Stage | Project | Current status | Principal remaining evidence |
| --- | --- | --- | --- |
| 1 Mac Studio | `.local/benchmarks/mac-studio-v1` | INCOMPLETE | named rights/mask/camera review, independent rear evidence, accepted features and repair, governed comparison suite, passed transaction, promoted scene, BLEND/GLB delivery |
| 2 DGX | `.local/benchmarks/dgx-spark-v3` | INCOMPLETE | approved immutable cameras, full governed suite, promoted improvement, editable BLEND, foam LOD approval |
| 3 RTX 5090 | `.local/benchmarks/rtx-5090-fe-v6` | INCOMPLETE | governed acquisition/video, reviewed teardown ROI, semantic acceptance, fixed cameras, all-gate promotion, editable BLEND |
| 4 external object | `.local/benchmarks/perseverance-stage4-v1` | INCOMPLETE | metric cameras, governed renders, all-gate promotion, BLEND and GLB |

Stage 4 uses NASA's Mars 2020 Perseverance rover. It starts with no private model, binds NASA's
official rounded 3000 × 2700 × 2200 mm dimensions, registers named source and media-policy review,
and has ingested and ranked keyframes from an official NASA/JPL-Caltech Earth-twin video. A
governed official NASA/JPL GLB is retained as a public landmark/topology reference, never as an
independent acceptance model. Three exact-flight-rover NASA originals add a port-side close view
and two complementary inverted underbody views. Their visible regions, assembly-state limits, and
wrapped-wheel exclusions are source-bound in the atlas. A rover-specific category pack replaced
the misleading automotive door/bumper/exhaust cells: 17 of 30 active rover regions are currently
evidence-supported (56.67%), while 13 unresolved regions have precise capture requests. Its
revisionable editable 18-component parametric seed matches the official envelope and binds body,
chassis, suspension, mast, navigation and hazard cameras, robotic arm, sample caching, power, RTG,
antenna, wheels, and underbody semantics. COLMAP has
recovered five feature-based views, and immutable pinhole derivatives remove the render-distortion
blocker without upgrading authority. Five content-addressed derivation receipts bind those rasters
through their source frames to the rights-reviewed NASA video; no missing or invalid lineage remains.
The three official rounded dimensions now have separate immutable source-provenance receipts that
bind the unchanged values and uncertainty to the governed NASA specifications page. These changes
removed two acceptance blockers without changing target compatibility, geometry, or camera authority.
A reviewed landmark-PnP lane now exists, but the live cameras
remain relative-scale, non-metric, and unapproved. Stage 4 now also has a content-addressed schema-v2
review packet binding the public model, 19 selected model-object anchors, the three exact NASA images
at native resolution, the authoritative dimensions, and an approximate intrinsics snapshot. The
matcher now has one review-required replacement proposal with 17 port-side, 24 oblique-underbody,
and 24 near-orthogonal-underbody points. Its governed synthetic views are `left_roll_000`,
`bottom_rear_roll_000`, and `bottom_roll_270`; proposal digest
`7ec50d1fed0eb5b81d259a37cdaf7c1a7559f1e4fa1d2a2f49bbba90d2c28fed` remains machine-only.
The earlier unreviewed output is receipt-validly `SUPERSEDED`, not reviewed. The packet and proposal
explicitly record axis-scale disagreement, wrapped wheels, partial assembly views, absent EXIF focal
metadata, and model-center uncertainty, and wait for exhaustive independent named review rather than
creating a metric claim. The
benchmark release policy excludes reference media unless each asset passes a final per-asset rights
review; default deliverables are derived geometry, citations, receipts, and project-owned renders.
Surface observations require an acquired, named-reviewed source and are idempotent, so replaying a
source cannot inflate observation counts.

Component-local validation now plans affected semantic views, evidence passes, reference-mask
regions, and metrics. Real Blender proof rendered one immutable-camera view with only beauty and
silhouette passes and a separate 128 x 128 ROI rather than recomputing the full suite.

Pre-reviewed direct-media URLs can now be acquired inside the executor. The downloader verifies
robots policy, redirects, network address class, named governance, rights, size and timeout limits,
records selected HTTP provenance, and advances acquired video into governed shot/keyframe analysis.
A reviewed search-provider adapter now executes bounded ranked queries inside VisionMCP. Provider
registration requires named terms/privacy review, HTTPS or explicitly authorized private access,
domain allowlists, response/query budgets, and environment-only credentials. Discovery receipts
bind the provider, target, queries, skipped unsafe or duplicate URLs, and registered source IDs.
Results are never auto-downloaded or auto-licensed: they enter the ledger with unresolved rights,
and an autonomous campaign stops for independent source review before acquisition. No live
provider is configured in the four benchmark projects yet.

Missing-evidence analysis is now executable rather than advisory. When a failed candidate has
mandatory `BLOCKED` gates, the autonomous executor maps their categories to bounded evidence terms,
combines them with current directional and surface-atlas gaps, and runs a persistent focused
provider pursuit. A pursuit is lease-protected, resumable, idempotent, and content-addressed. New
leads remain unreviewed and are never downloaded or licensed automatically. If reviewed public
discovery yields nothing, VisionMCP records an `EVIDENCE_CEILING`, creates typed photograph,
measurement, document-upload, calibration, or teardown requests for the unresolved gaps, and stops
the campaign instead of repeating a generic search. New
evidence makes the old blocked-gate diagnosis stale so the candidate transaction is recomputed.
Acceptance verifies the pursuit, discovery, source, and capture-request lineage and rejects
post-receipt request tampering.

The Stage 4 portfolio now has evaluated parametric, classical COLMAP, and hybrid semantic lanes.
The visual-hull worker is implemented, but Stage 4 has zero reviewed full-object masks; its lane
therefore remains `EVIDENCE_READY` with the precise requirement for at least two governed masks
rather than being mislabeled as completed geometry. Existing-model repair is not applicable to the
no-private-model benchmark, and learned, licensed generative, and Gaussian workers remain
explicitly blocked until configured.

Portfolio backend configuration now activates, rather than merely describes, governed learned,
generative, and Gaussian lanes. An explicit local VGGT installation can execute offline and bind
its depth, point, confidence, and approximate-camera evidence to the learned candidate; research
checkpoints remain non-acceptance evidence. Receipt-verified distributed generative results and registered
camera-bound Gaussian oracles can populate their lanes, but stay labeled synthetic hypothesis or
appearance-only and cannot independently establish acceptance. Candidate records reject unknown
geometry-run IDs. None of these optional backends is configured in the four live benchmark
projects, so their blocker status remains truthful.

The generative 3D proposal interface is now executable rather than import-only. All six roadmap
operations—shape, shape plus material, multiview images, texture, retopology, and candidate
export—create idempotent artifact-bound requests with an explicit backend checkpoint and license.
Requests persist no credentials and route only to a dedicated generative worker advertising the
exact approved model. Worker completion must declare registered model/image outputs matching the
operation contract, exact input references, backend, checkpoint, seed, confidence, and limitations.
Results remain `SYNTHETIC_HYPOTHESIS`, cannot establish hidden geometry or acceptance, and bring
their verified result receipt into the portfolio candidate lineage. Terminal worker failure remains
failed rather than manufacturing a result. Acceptance recomputes request identity, licenses,
commercial eligibility, media types, operation outputs, artifact bytes, authority labels, and
request/result lineage, rejecting a separately registered hash-valid semantic forgery. The
interface is implemented and distributed-worker tested, but no live Beast benchmark has executed
an installed generative model.

Active learning is now an executable governed lifecycle instead of a caller-asserted comparison.
Low-confidence/high-impact predictions are artifact-bound and ranked into named correction tasks.
Corrections create a rights-labeled generated dataset and an actual offline training run routed to
the training worker class. Candidate and baseline metrics are recomputed from stored prediction
artifacts on the same immutable benchmark dataset; equal or regressed checkpoints are rejected.
Only a commercially eligible checkpoint with at least one improvement and no required-metric
regression can enter named activation. Activation, prior-model supersession, and the final cycle
event commit atomically. Acceptance reconstructs every revision and independently reconciles the
prediction, correction-dataset, training, benchmark, checkpoint, comparison, and activation
lineage, including rejection of a newly registered hash-valid but semantically forged comparison.
The workflow is implemented and tested; no live Beast benchmark has executed a real retraining
worker, so this is not claimed as runtime proof.

Component fitting now has the same transactional discipline as scene evaluation. Each proposal
binds the exact component snapshot and immutable proposal artifact. Named acceptance or rejection
writes a separate content-addressed decision receipt, and an accepted parameter revision plus its
fit decision commit in one database transaction or both roll back. Acceptance recomputes the
decision receipt against the stored fit, reviewer, reason, parameters, and applied revision;
post-review database edits, corrupt proposals, stale component snapshots, and injected write
failures cannot produce an authoritative fit.

The component-local multiview optimizer now refuses caller-asserted residual scores. It requires
an approved structurally valid immutable camera set, an explicit semantic-node binding to the
parametric component, at least two affected eligible views, artifact-valid renders and residuals,
and exact locality-plan coverage. Silhouette loss is recomputed from the stored comparison IoUs
for every candidate before a proposal is recorded. Optimization acceptance/rejection is also
snapshot-bound, atomic with any component revision, independently receipted, and semantically
reverified by acceptance; stale, forged, or interrupted optimization decisions fail.

The optimizer is now fed by a persistent bounded search rather than an advisory-only candidate
contract. Search planning snapshots the component, semantic bindings, authoritative baseline,
approved camera solution, locality plan, and scalar bounds. Execution generates isolated component
variants, renders only affected views and passes, records artifact-backed residuals, retries
interrupted candidates within a fixed budget, and resumes idempotently. Its immutable completion
receipt binds every candidate scene, render run, comparison, and optimization proposal; acceptance
independently verifies that lineage and rejects database or render-association tampering. Search
completion never mutates a component or claims acceptance.
Failed attempt scenes and every nonselected completed variant are automatically rejected with
their own policy-bound lifecycle receipts, leaving only the lowest-loss candidate eligible for the
seven-category transaction.

Once an autonomous campaign has approved metric cameras, stored comparisons, and explicit
semantic-to-component bindings, the executor derives bounded scalar variants, runs or resumes the
fixed-camera search through the coordinator, and persists Geometry Analyst, Component Modeler,
Optimization Planner, and Adversarial Reviewer tasks for named review. It pauses before parameter
mutation and never auto-accepts or promotes the selected candidate.

Post-promotion delivery is now executable rather than advisory. The coordinator first reconstructs
the authoritative scene's receipt-verified promotion chain, then produces or reuses byte- and
hash-valid materialized BLEND and GLB exports, creates a final machine receipt, and verifies it
against the project artifact store. Receipt reuse also recomputes current acceptance, so a newly
registered, hash-valid but semantically forged `accepted` envelope cannot close the workflow. The
autonomous executor stops its campaign as `DELIVERY_COMPLETE` only after this workflow returns an
accepted fidelity; a valid receipt with remaining blockers pauses as
`DELIVERY_ACCEPTANCE_BLOCKED` instead.

Specialized reasoning now uses persistent role-task records. Stage 4 has Camera Analyst and Capture
Planner tasks waiting for reviewed metric evidence, plus completed automated Adversarial Reviewer
and Acceptance Auditor findings that keep the L3 camera gate closed. All role outputs are advisory
and are structurally unable to accept or promote a scene.

Stage 3 now has an exact resolved GeForce RTX 5090 Founders Edition target, a target-scoped
15-node computer-hardware semantic graph, a 14-cell surface atlas, an eight-lane portfolio, and a
resumable campaign. Its six fallback cameras and silhouette masks remain unapproved hypotheses.
The campaign is paused at `EVIDENCE_ACQUISITION_REQUIRED` because the legacy images have no
governed source-ledger entries; they cannot silently advance camera or reconstruction authority.

Stage 2 now records v17 as `REJECTED` through a rejection-only seven-category transaction. Six
paired v16/v17 views reused the exact same camera-solution IDs; five regressed and mean silhouette
IoU fell from 0.95831 to 0.94679. Camera approval, dimensions, components, topology, and materials
remain blocked rather than inferred. The transaction cannot accept or promote either scene.
The same six legacy view hypotheses are now consolidated into one current immutable camera
solution with exact acceptance-reference coverage and no authority upgrade. A real v16 review run
produced six complete 512 px governed suites (72 hash-verified pass artifacts). Both remain pending:
the benchmark auditor excludes the camera and renders until a named camera review approves the
solution. Foam LOD approval likewise requires a structured screen-space tier strategy and a
content-addressed named-review receipt; a metadata boolean can no longer pass that gate.

## Truth boundary

The calibration proves the production machinery works end to end. It does not substitute for the
four Beast demonstrations. VisionMCP Beast Mode must not be described as benchmark-complete until
all four stage reports are `PASSED`. Missing, inferred, synthetic, and rights-restricted evidence
remain explicit and cannot be promoted by a visual score alone.

The persistent real calibration also passes the strengthened lifecycle audit: one authoritative
scene, three verified transition receipts (`DRAFT` → `CANDIDATE` → `ACCEPTED` → `PROMOTED`), one
verified passed evaluation, no invalid or missing supersession receipts, L3 accepted, and a valid
machine-readable acceptance receipt.

The requirement-by-requirement implementation/proof matrix is in
[`BEAST_MODE_ROADMAP_AUDIT.md`](BEAST_MODE_ROADMAP_AUDIT.md).
