# Maximal V1 compliance matrix

This document maps the Maximal V1 Development Plan to shipped implementation and verification
evidence. “Implemented” means the public package contains the executable code path or governed
data-only boundary. It does not mean that an operator has supplied third-party weights, proprietary
reference evidence, hardware, or human approval for every project.

## Milestones A–J

| Milestone | Status | Implementation and evidence |
| --- | --- | --- |
| A — complete scaffold | Implemented | Installable package, `bvmcp` CLI, stdio/HTTP MCP server, SQLite-WAL project store, content-addressed artifacts, jobs/cache/provenance, backend registry, schemas, doctor, Docker and CI. |
| B — headless authority | Implemented | Blender discovery and a hash-bound, allowlisted, `--disable-autoexec` worker provide inspect, audit, render, import, component mutation, constraints, cameras, checkpoints, LOD, Blend/GLB export, repair, synthetic generation and calibration generation. |
| C — evidence platform | Implemented | Immutable references, image/video/PDF capture, rights state, typed/correctable measurements, perspective grids, reviewed masks, feature graph, uncertainty, coverage and next-best-view records. |
| D — geometry ensemble | Implemented | Silhouette, COLMAP and governed offline VGGT adapters normalize cameras/depth/points/confidence. Bounded coarse-to-fine silhouette camera refinement records every evaluated render hash and always remains non-metric. External high-resolution backends use the artifact-bound import contract. Camera and geometry consensus retain incompatible hypotheses and commercial/research authority separately. |
| E — parametric reconstruction | Implemented | Component DSL/store, constraint validation, Blender generators, Geometry Nodes arrays, robust component fitting, revision checks, scene audit and evidence-bound Mac Studio repair. |
| F — validation system | Implemented | Receipt-bound fixed rigs render beauty/exposure, neutral clay, material-neutral, three grazing directions, silhouette, depth, smoothed/geometric normals, curvature, wireframe and object/component/feature IDs. Replayable visual-geometry scorecards add projection precision/recall and boundary RMSE/Chamfer/P95, supplemental edge/perceptual diagnostics, explicit unavailable-ground-truth states, cause attribution, and residual overlays. A separate versioned manufactured-form audit detects objective mesh failures and review risks without turning priors into evidence. Comparison supersession, uncertainty, coverage/NBV, canonical signed receipts and human-readable receipt rendering remain enforced. |
| G — full intelligence | Implemented with governed external runtimes | Gradient/GrabCut/manual segmentation, feature-label masks and detection import, deterministic domain-randomized synthetic data, training/evaluation import, visual-oracle registry, high-resolution external evidence boundary and analytical/black-box/differentiable/learned optimization tiers. Third-party models and trainers are never silently downloaded or bundled. |
| H — operator experience | Implemented | Loopback/token review app, evidence and residual views, named review actions, workflow tools, resources/prompts, job monitoring, capture requests, tier review and Blender extension package. |
| I — distributed execution | Implemented | Authenticated capability registry, hardware/model/load/locality routing, renewable leases, bounded retries, fault recovery, verified chunk transfer, stale-part cleanup, MCP worker protocol and packaged mounted-project worker loop. Mac/5090/DGX are capability profiles, not hard-coded hosts. |
| J — public product | Implemented | Independently extractable wheel, install/doctor flow, manual model-governance workflow, license registry, documentation, owned calibration benchmark, security review, Docker assets, extension ZIP and release verification. |

## Authority boundaries

- The coordinator performs no silent checkpoint or reference download.
- VGGT uses an explicit governed installation id, a local checkpoint, offline environment flags and
  PyTorch `weights_only=True`; learned output remains non-metric until aligned and reviewed.
- Research-only evidence is retained for comparison but cannot become commercial authority.
- Gaussian splats and NeRFs are appearance oracles only. They cannot establish dimensions, hidden
  geometry or the accepted editable model.
- Automatic opaque-image segmentation is diagnostic evidence. L3 requires embedded alpha or a
  hash-bound, named, reviewed reference mask.
- Automatic camera refinement may improve residuals and composition but always produces a pending
  approximate-visual solution; it never inherits or promotes metric authority from its source.
- Historical comparisons remain in receipts; acceptance evaluates the newest comparison for each
  reference and records exactly which attempts were superseded.
- Human decisions require a non-empty reviewer and reason. No automated benchmark path may approve
  proprietary project evidence on a person’s behalf.

## Verification evidence

The release gate is `scripts/verify-v1.py`. It checks required package surfaces, schemas, restricted
Blender operations, model-license policy, public benchmark assets, distributable contents and,
optionally, a project/receipt. The normal development gate additionally runs Ruff, the unit suite,
the real Blender vertical slice, the real calibration benchmark, wheel verification, a clean wheel
install, MCP initialize/list-tools and Blender extension validation/build.

The owned calibration benchmark is the reproducible positive L3 fixture. It proves known dimensions,
camera recovery, scale recovery, repeatability, export consistency, multi-pass receipt coverage and
receipt integrity without redistributing proprietary references.

## Benchmark 1 — Mac Studio

The current persistent Mac Studio project contains a repaired authoritative Blend, a candidate GLB,
rear-grille ray/topology/dimension evidence, render passes, a coverage report and a cryptographically
valid receipt. Its bounded automatic camera refinement passes the numerical silhouette threshold,
but that comparison still uses a medium-confidence automatic mask and a non-metric camera. It is
intentionally not accepted at L3 yet. The remaining project-evidence decisions are:

1. replace the single non-authoritative marketing render with adequate owned/licensed multi-view
   photos or explicitly reviewed masks;
2. recover metric cameras covering every accepted reference and obtain named camera review;
3. review the proposed technical feature graph;
4. review the applied repair against the new evidence and a valid receipt;
5. rerun comparison, coverage and receipt export after those external decisions.

Those are external evidence and human-review requirements, not conditions the software may forge or
silently waive.
