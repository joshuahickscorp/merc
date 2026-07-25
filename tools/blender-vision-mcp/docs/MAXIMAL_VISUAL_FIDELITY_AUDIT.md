# Maximal visual-fidelity directive audit

Current as of July 25, 2026. This audit treats implementation, live execution, and named
acceptance as separate claims. `PROVEN` requires current replayable evidence; `PARTIAL` means a
real implementation exists but the full directive is not satisfied; `BLOCKED` identifies an
external evidence or named-review dependency; `ABSENT` means the required capability is not yet
implemented.

| # | Directive requirement | Status | Current evidence | What still prevents completion |
| ---: | --- | --- | --- | --- |
| 1 | Preserve current baseline | PROVEN | Receipt-valid freezes: original Mac `4ecd5d3a-6d7c-4835-857c-f1bad04ba15d`, current maximal Mac `fa2989f9-cf46-4ca3-8b18-945eaa2eb9f5`, DGX `1afe02ba-4f94-4bd6-9e0d-5a2457fe125d`, RTX `b58efeec-037f-4686-a18a-7fab81873de8`. The new industrial passes are a separate maximal superset; the frozen 25-pass governed minimum is unchanged. | No baseline overwrite is permitted. |
| 2 | Semantically bind all visible geometry | PARTIAL | Mac candidate 37/37, DGX 39/39, and RTX candidate 257/257 visible meshes have receipt-valid machine proposals and `PART_OF` assembly edges. All four lifecycle states are implemented. | Every live binding remains `PROVISIONALLY_BOUND`; evidence regions, corrections, and named acceptance are incomplete. |
| 3 | Visual-frequency pyramid | IMPLEMENTED / LIVE BLOCKED | Primary, secondary, and tertiary component groups and independent blockers are persisted in replay-valid frequency scorecards. Latest Mac scorecard: `0d00e8ca-94b9-455d-ace3-ac0e3051f013`. | No component has an acceptance-valid score because local reviewed masks and accepted bindings are absent. |
| 4 | Replace area-dominated scoring | PARTIAL | Whole-object, semantic mean, visible-area, visual-importance, worst-five, and minimum-mandatory fields exist. Component packets replay silhouette and edge evidence. | Landmark, reference-depth, reference-normal, reference-curvature, material, and manufactured-form residuals are not all evaluable per component. Missing ground truth must stay explicit. |
| 5 | Native-resolution component crops | PARTIAL | Latest exact visible Mac packets contain native reference/baseline/candidate crops, masks, overlays, landmarks, parameters, confidence/history, and all 20 requested diagnostic crops, including decoded EXR depth/normals. | Only four objects are actually visible in the single compatible front reference. Important rear and occluded components lack compatible views and reviewed local masks. |
| 6 | Industrial-surface analysis | PARTIAL | Zebra, reflected-line, curvature, world/geometric normals, wireframe, three grazing lights, new continuous highlight-flow, and `screen_space_normal_discontinuity_v1` now execute in real Blender. The live map measures 274,322 valid pixels, 44,426 pixels above 0.25°, mean 1.06977°, P95 2.41994°, maximum 167.87007°. | G0/G1/G2 classification, planarity, symmetry, radius fitting, curvature combs, and governed waviness/pinching maps remain incomplete. Sharp intended boundaries are not yet classified separately from defects. |
| 7 | Manufactured-component library | PARTIAL | Governed generic ports, panels, buttons, screws, feet, fans, blade/vent/hole arrays, grilles, brackets, heat sinks, seams, lofts, sweeps, patches, and vehicle forms exist. | Connector-specific USB-A/USB-C/HDMI/DisplayPort/Ethernet/audio/power/PCIe primitives with full dimensional variants, internals, attachments, materials, LODs, and landmarks are not complete. |
| 8 | Assembly-graph reasoning | PARTIAL | All required relation names are schema-valid; `PART_OF` relations are generated and acceptance checks them. | Live Mac/DGX graphs contain only `PART_OF`; contact, clearance, intersection, wall-thickness, recess, fastening, and support diagnostics are not yet proven. |
| 9 | Specialist vision ensemble | BLOCKED / INTERFACE ONLY | Pluggable segmentation, landmark, learned reconstruction, generative, Gaussian, oracle, and external-runtime contracts exist. | The directive explicitly rejects interface-only progress. The live Mac project has zero registered visual oracles and no approved/configured specialist model ensemble execution. |
| 10 | Visual-defect critic | ABSENT | Generic synthetic-data, training-run, evaluation, and active-learning infrastructure exists. | No controlled multi-class defect corpus and no trained critic/adapter have been proven on the requested perturbation taxonomy. |
| 11 | Residual-to-repair translation | IMPLEMENTED / LIVE DIAGNOSTIC | `visual_geometry.diagnose_residuals` supports all 18 required defect classes and binds each result to semantic component, views, regions, passes, candidate parameters, confidence, expected visual/gate impact, and a hash-verified rollback scene. Live Mac diagnosis `ccfd5829-15a6-4237-88a2-39f4283fecab` replays nine diagnoses: seven `EVIDENCE_MISSING` and two `EVIDENCE_CONFLICT`. | Current evidence cannot safely localize a geometry repair. Edge and surface-continuity regions need classification; masks, camera, local reference geometry, and accepted bindings remain missing. |
| 12 | Component tournaments | PARTIAL / LIVE BLOCKED | Snapshot-bound multiview bounded parameter search creates isolated candidates, fixed-camera crops/comparisons, rejects nonselected scenes, and records a review-only winner. | It requires approved multiview cameras, which Mac lacks; broad-to-fine search, component packet scoring, explicit Pareto retention, critic ranking, and full all-view regression are not complete together. |
| 13 | Defect-revealing diagnostic lighting | PROVEN FOR NEW MAXIMAL RUNS | New rigs require 27 immutable passes: the frozen 25-pass suite plus normal-discontinuity and highlight-flow. Latest run `393814bb-9df7-4020-940c-fcee5b82cc3f` contains 28 output keys including the instance-mask alias. | Diagnostic images do not create reference or acceptance authority. |
| 14 | Mac Studio convergence campaign | BLOCKED / INCOMPLETE | Candidate topology repair, exact object IDs, all five diagnostic projection gates, maximal pass suite, and replay-valid component/frequency evidence exist. | Fresh L3 has 18 blockers: rights, reviewed masks, approved multiview cameras, accepted bindings/features/repair, rear evidence, authoritative comparisons/audit, passed transaction, promotion, and BLEND/GLB delivery. |
| 15 | DGX convergence campaign | BLOCKED / INCOMPLETE | Envelope and provisional binding coverage exist; an immutable baseline is frozen. | Porous structure, multiview camera/mask authority, coherent assembly validation, component scoring, promotion, and delivery remain incomplete. |
| 16 | RTX 5090 convergence campaign | BLOCKED / INCOMPLETE | Broad model/binding coverage, frozen baseline, and existing candidate/history suites exist. | Fan/heat-sink primitives, approved six-direction and close-up evidence, component scoring, coherent assembly validation, promotion, and delivery remain incomplete. |
| 17 | Dual delivery modes | ABSENT | Evidence-authority labels distinguish inferred/proposal/accepted records internally. | No explicit receipt-bound `EVIDENCE_TWIN` and `BEST_VISUAL_REPLICA` delivery transaction/artifact pair exists. |
| 18 | Visual-first progress reporting | PARTIAL | The Desktop report leads with visual change, target components, measured effects, current state, and highest-impact gaps. | Automated campaign reports do not yet uniformly emit before/after component images and per-view regression summaries. |
| 19 | Stop condition | NOT MET | Mac Beast Stage 1 remains `INCOMPLETE` at 1/7; latest acceptance is false. | Mac is not accepted L3. The alternative stop condition is also not met because every supportable region is not accepted and the evidence ceiling has not been independently verified. |

## Current visual convergence headline

The candidate geometry and whole-object projection are unchanged: IoU `0.96355325`, Dice
`0.98143837`, precision `0.99193675`, recall `0.97115989`, and boundary RMSE `11.70062923` px at
the 2048-pixel reference scale. The new diagnostic products do not claim an accuracy improvement;
they remove an observability gap.

The continuous highlight-flow view exposes the top-to-side radius and front-shell transition
without dark material concealment. The new normal-discontinuity heatmap exposes dense geometric
normal changes along the rounded sidewalls, top/front transition, port boundaries, and silhouette.
Those signals are diagnostic only: intended sharp edges and silhouette boundaries still need
semantic classification before any automated repair.

## Highest-impact next autonomous work

1. Extend industrial analysis with intended-edge classification, G0/G1/G2 continuity, local
   planarity/radius fitting, and curvature combs.
2. Use the replayable residual diagnoses to drive component tournaments once reviewed cameras and
   local masks exist; do not search geometry while the dominant diagnosis is evidence missing.
3. Build connector-specific and fan/heat-sink manufactured primitives.
4. Add contact, clearance, wall-thickness, recess, and intersection checks to the assembly audit.
5. Configure and execute real specialist backends where operator-approved runtimes and model
   weights are available.

Named rights, mask, camera, semantic, repair, and promotion decisions must remain independent human
actions. VisionMCP may prepare exact review packets but must not perform those decisions itself.
