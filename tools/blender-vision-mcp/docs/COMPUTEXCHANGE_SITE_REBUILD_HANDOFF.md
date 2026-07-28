# ComputExchange production-site rebuild handoff

This is a read-only production plan. The existing ComputExchange checkout and
live site were not edited, deployed, or reconfigured.

## Current authority and staleness

The production checkout was inspected at
`fbe02ce7ff8e60d6be8b32745a95179bd425a700` on
`release/rc1-go-closure`. It already contained 63 modified or untracked paths;
they were treated as user-owned and left untouched. The checked-in
`web/index.html` last changed on July 20, 2026.

The stronger staleness signal is deployment drift, not calendar age:

- checked-out `web/index.html` SHA-256:
  `5db3ebcaa0a496a3503520c7ed054a8150eb20f8fb6109fcdb63288a9d5bb767`;
- live `https://computexchange.net/` HTML SHA-256:
  `4aaa3b8bb2cca20c87ea7735ecfc241e64c2d1a8992016ed843430c688fbc9e0`;
- the two pages are different implementations;
- the live page refers to `docs/SITE-CLAIMS.md`,
  `docs/SITE-REBUILD-T0.md`, `render/build_scene.py`, and deployed 3D assets
  that are absent from the captured production web tree.

Before design work, establish which source revision produced the live bytes.
No later acceptance receipt should tolerate source/deployment divergence.

## Current site and 3D inventory

The live desktop experience is one 2,714 px narrative page:

1. an interactive Mac Studio/DGX Spark WebGL hero;
2. three “how it works” rows and a receipt dialog;
3. one price monument;
4. an earn narrative;
5. a closed-alpha download narrative.

At 390×844 the complete site is hidden. The only visible journey says “this
page is built for a desktop” and offers AirDrop, email, or link copying. This
has no horizontal overflow, but it is not a responsive version of the product.

The live 3D path contains:

- `hero.js`: 12,662 bytes;
- unbundled `three.module.js`: 1,272,972 bytes;
- `oracles.glb`: 1,066,896 bytes;
- the GLB is structurally valid with 7 nodes, 7 meshes, 9 materials, 3
  textures, 0 animations, and `KHR_materials_clearcoat`;
- the current source `web/` tree contains no BLEND, GLB, glTF, FBX, or OBJ.

The GLB is a sound starting asset, not yet a production authority: its editable
source, generation recipe, rights record, visual acceptance views, and
deployment binding must be recovered or rebuilt.

## Runtime, responsive, accessibility, motion, and source binding

A single Toronto-machine network observation returned HTTP 200 with 58 ms TTFB
and 74 ms total HTML time for 19,275 bytes. This is not a percentile or Web
Vitals claim. The known Three.js + hero controller + GLB path is at least
2,352,530 bytes before loader add-ons, fonts, and images.

Positive current properties:

- one H1, one main landmark, restrained monochrome tokens, self-hosted fonts;
- pointer and touch orbit, hover labels, request-on-demand rendering;
- a still-image WebGL fallback;
- explicit reduced-motion handling in the renderer and status animation;
- basic `nosniff`, `SAMEORIGIN`, and no-referrer headers.

Closure defects:

- mobile receives no product, pricing, proof, buyer, or supplier journey;
- the canvas has no independent accessible equivalent in the live DOM;
- source, deployed HTML, renderer source, GLB, and claimed Blender generator
  are not one receipt chain;
- optional scroll choreography exists in `hero.js` but is disabled;
- the 1.27 MB unbundled Three.js module is an avoidable initial cost;
- the live claims ledger cites an unbound proof count and absent source paths;
- the single-page information architecture exposes only one ordinary link.

Evidence is preserved in `artifacts/site-rebuild-handoff/`, including desktop
and mobile captures, live HTML/headers/network timings, renderer source, GLB,
GLB validation, and exact SHA-256 digests.

## What NOCTURNE/ONE proved

Safe, directly transferable lanes:

- source-first responsive TypeScript application construction;
- poster-first WebGL with real intent-gated GLB loading;
- named editable Blender parts and validated named-identity LODs;
- reduced-motion and no-WebGL completion paths;
- keyboard journeys, mobile interaction traces, semantic route/state probes,
  and zero critical/serious automated accessibility findings;
- SQLite-backed validation, authorization, idempotency, duplicate handling,
  migrations, rollback, and retry behavior;
- frozen performance budgets measured on the complete real path;
- sealed builder/evaluator separation, failed-attempt preservation, and
  smallest-surface repair with global regression reruns.

Lanes that remain blocked or risky:

- arbitrary brand/site generalization beyond one held-out product;
- real Safari/Firefox parity on this macOS 27 environment (Firefox SWGL remains
  externally blocked);
- physical second-host and WebGPU evidence;
- live Figma/Storybook ingestion;
- remote production deployment and rollback proof;
- claiming 105/110 generalization from a single product;
- importing NOCTURNE visual language or geometry into ComputExchange.

## Recommended information architecture

Use the ComputExchange product and proof model—not the sandbox composition:

- Home: verified compute exchange thesis, live supply/capacity facts, primary
  buyer and supplier actions;
- How verification works: quote, dispatch, attempt, verification, receipt,
  dispute, settlement;
- Buyers: supported workload/model cells, API/SDK quick start, pricing and
  budget controls;
- Suppliers: eligibility, device runtime, quiet hours, safety, expected
  economics with evidence status;
- Network: current admitted devices and runtimes, availability, status;
- Pricing: catalogue, fee model, examples, limitations;
- Trust: security, privacy, artifact handling, receipts, governance;
- Docs/status: versioned API, SDKs, runbooks, service status;
- Alpha access: one authenticated, validated request journey.

Keep the dark “technical instrument” identity if desired, but make the visual
system about exchange topology, bounded work, verification, and receipts.
Do not reuse NOCTURNE’s product shell, eclipse motif, typography composition,
or interaction choreography.

For 3D, rebuild or recover the Mac Studio/DGX scene as governed editable source:
semantic device groups, one hero GLB, a mobile LOD, validated materials,
fixed-camera poster views, accessible textual equivalents, and an explicit
asset/right/source receipt. Use 3D to explain the network, not as decorative
proof.

## Production budgets and acceptance gates

These are proposed ComputExchange-specific budgets:

- critical HTML + CSS ≤ 180 KB transferred;
- compressed initial JavaScript ≤ 250 KB;
- poster visible before any GLB request;
- desktop hero GLB ≤ 2 MB; mobile LOD ≤ 750 KB;
- no full-resolution texture before 3D intent or idle gate;
- cumulative layout shift ≤ 0.05 in the fixed harness;
- desktop median ≥ 55 FPS and p95 frame time ≤ 24 ms;
- mobile-emulated median ≥ 40 FPS and p95 ≤ 35 ms;
- API p95 ≤ 150 ms under the declared local profile;
- no unbounded five-minute interaction memory growth.

Release must also prove:

- deployed source/tree digest exactly matches the release receipt;
- desktop, tablet, and mobile complete the same core content and alpha journey;
- keyboard and screen-reader-oriented journeys pass every public route;
- zero critical/serious automated accessibility findings;
- reduced-motion and no-WebGL users retain all non-3D meaning/actions;
- 3D BLEND reopens, both GLBs validate/reimport, LOD identity is stable;
- all product claims link to current source and runtime evidence;
- fresh-clone build/test, canary deployment, rollback, and post-rollback smoke
  pass from immutable artifacts.

## Migration and rollback sequence

1. Resolve the dirty production checkout with its owner; create a clean,
   immutable baseline without discarding local changes.
2. Identify the exact deployed revision and import only provenance-verified
   live assets/source into that baseline.
3. Create branch `codex/computexchange-site-rebuild` in worktree
   `/Users/scammermike/Downloads/computexchange-worktrees/site-rebuild`.
4. Freeze desktop/mobile/browser/3D/network/source-binding baseline receipts.
5. Build the new information architecture and design system with poster-only
   assets first.
6. Rebuild or recover the editable 3D scene, then add intent-gated hero and LOD.
7. Bind claims, routes, source symbols, runtime probes, API, accessibility, and
   performance gates.
8. Deploy one immutable canary version, exercise real journeys, then promote
   only the exact accepted digest.
9. Roll back by redeploying the previously accepted immutable version; do not
   mutate or rebuild the old version during rollback.

## Ready-to-run next goal

```text
/goal Rebuild the ComputExchange production site from the governed handoff at
tools/blender-vision-mcp/docs/COMPUTEXCHANGE_SITE_REBUILD_HANDOFF.md. First
resolve the pre-existing dirty checkout with me without deleting or overwriting
any changes. Identify and receipt the exact source of the live deployment.
Create the isolated worktree
/Users/scammermike/Downloads/computexchange-worktrees/site-rebuild on branch
codex/computexchange-site-rebuild. Implement the new ComputExchange-specific
information architecture, responsive mobile/tablet/desktop experience,
source-bound claims, governed editable device scene, poster-first WebGL and LOD,
alpha access journey, accessibility, performance, canary deployment, and
immutable rollback gates. Do not copy NOCTURNE/ONE visuals or geometry. Stop
before production promotion unless I explicitly authorize it.
```
