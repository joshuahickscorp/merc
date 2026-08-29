# Computexchange website, 3D, and Blender frontier status

**Prepared:** 2026-07-19 EDT
**Repository state inspected:** `release/rc1-go-closure` at `a4d50d9`
**Overall verdict:** the exchange software is a hardened release candidate, the public website is live but deployed from an older visual build, the 3D work is technically recoverable but not release-ready, and the Blender MCP is installed but not currently operational end to end.

## Executive summary

There are two different website states today:

1. `https://computexchange.net/` is online and healthy, and still serves the older interactive Three.js hero with Mac Studio and DGX Spark geometry.
2. The current release branch contains a newer, more conservative, responsive static website built around the private-canary boundary. That build is not what the public domain is serving.

The deployment split is the most urgent issue. The live site still says things such as “a verified spot market for compute” and “the verified catalogue ships today,” while the current release record says the private canary is NO-GO and live/public operation is prohibited. The live visual build and the release truth have drifted apart.

The 3D assets are not lost. A validated Mac Studio `.blend`, render GLB, web LOD GLB, extensive review renders, older Mac/DGX web geometry, and a historical six-GPU rig branch all remain available locally or in Git history. However, the current branch deleted the render builders, serves only a static Mac Studio still, and has unresolved source/provenance review. The latest strict pass-6 correction does not support calling any rebuilt product model accepted or complete.

The Blender stack is close but not “up” in the operational sense. Blender 4.2.1 LTS works in background mode and successfully opened the current `.blend` and GLBs. The Blender MCP server also handshakes and lists 26 tools. But there is no Blender add-on listener on port 9876, the current Codex task has no Blender tools exposed, and the MCP’s CLI path fails because `BLENDER_PATH` is unset and `blender` is not on `PATH`.

## 1. Website status

### What is working

- The public domain, `/healthz`, `/readyz`, and `/admin` all returned HTTP 200 during this review.
- The live desktop hero loaded its self-hosted Three.js code and 1,066,896-byte `oracles.glb` without browser console errors.
- The current branch has a public landing page, an in-memory-key operator page, and a real database-backed `POST /v1/alpha-request` flow.
- `node scripts/site-build.mjs` passed. It verified both HTML surfaces, WCAG AA label contrast at 6.06:1, CSP hashes, security headers, mobile availability, and the canary disclosures. The observability validator also passed with 23 alert rules and 13 dashboard panels.
- The checked-out worktree was clean before this report was added and matched `origin/release/rc1-go-closure` at `a4d50d9`.

### The deployment gap

| Surface | Current state | Meaning |
|---|---|---|
| Public domain | Older interactive Mac Studio + DGX Spark hero | Visually stronger, but stale claims and stale release posture |
| Release branch | Static Mac Studio still and explicit private-canary disclosures | Safer and responsive, but not deployed |
| Historical `site/three-device-hero` branch | Adds a 5.2 MB six-GPU rig GLB | Exists in Git, but the live `/assets/site/rig.glb` currently returns 404 |

This is not a cosmetic difference. It is a release-governance mismatch. The domain should either deploy the exact hardened candidate or the interactive site must be rebased onto the current claims, security headers, responsive behavior, and release evidence.

### Current-branch UX findings

The current branch rendered correctly at 1280 px and 390 px, loaded its main Mac Studio still, and opened/closed the claims ledger successfully. Three polish issues remain:

1. At 390 px, long console labels such as `executors` overflow the 64 px label column and collide with body copy.
2. The reduced-motion media block contains a duplicated nested selector, which should be corrected even though the browser recovered.
3. On desktop, the fixed masthead can visually collide with section copy during scrolling.

### Release posture

The software candidate is GO, but the private canary remains NO-GO at 69/100. Nine P1 gates remain, including persistent private staging, rollback/restart/soak proof, independent backup restore, Stripe test-mode reconciliation, alert delivery, approved participants, independent review, governance approvals, and incident/privacy/provenance exercises. This means the site should continue to present CX as a controlled development canary, not a launched market.

## 2. State of the 3D renders

### What exists and was verified

The local Mac Studio model pack contains:

| Artifact | Size | Verification in this review |
|---|---:|---|
| `models/mac_studio/final_packed.blend` | 3,976,062 bytes | Blender 4.2.1 opened it headlessly; 36 objects, 36 meshes, 37,868 source vertices, 26,327 source polygons, no missing external images |
| `models/mac_studio/render_master.glb` | 3,837,604 bytes | Imported headlessly; 36 meshes, 121,953 vertices, 75,596 polygons |
| `models/mac_studio/web_lod.glb` | 1,845,092 bytes | Imported headlessly; 36 meshes, 59,848 vertices, 26,430 polygons |

The validation receipt also records 418 physical through-holes. A duplicate handoff copy exists under `codex-handoff-3d-rebuild/models/`.

The live site still has the older combined Mac Studio + DGX Spark `oracles.glb`. Historical branches also preserve:

- Mac Studio and DGX Spark front/rear measurement work with silhouette gates reported as passing.
- RTX 5090 FE front/rear measurement sheets with silhouette agreement passing but the overall result explicitly marked `PARTIAL`.
- A six-card rack/rig model and a `site/three-device-hero` build that was never promoted to the current live site.
- Roughly 81 MB of local pass-6 review material, including Mac rear boards and nine DGX foam candidates.

### What is not complete

The pass-6 files contain optimistic intermediate summaries followed by a stricter final correction. The correction governs:

- The claimed Mac perforation accept was revoked because the 418 holes are on the secondary lower band while the main rear hero grille is still absent.
- The camera results are body-bounding-box registrations, not full semantic camera solves.
- The strict project gate report remains at zero accepted geometry, zero completed hard gates, and 74 of 75 tasks pending.
- Milestone A remains false, with six missing checks: Mac evaluated/accepted/group coverage and DGX evaluated/accepted/foam application.
- DGX’s tractable foam candidates were independently scored 42/100 and are three to five times too coarse. Correct-scale whole-product strut geometry would require millions of elements, so the practical answer is a signed-off LOD/shader hybrid.
- No pass-6 RTX v2 deliverable exists in the current local review tree.

The current site render is attractive as a hero image, but the rear-board evidence makes clear that the rebuilt Mac is not a complete reference-faithful product model yet.

### Reproducibility and legal state

The current branch deleted the marketing render builders in commit `8f5fb57`. The `render/` directory is empty, while `.gitignore` still says builders are committed and can regenerate the pixels. That comment is now stale. Heavy artifacts remain in ignored local packs and Git history, but a fresh clone of the current branch cannot reproduce the 3D system.

`ops/asset-provenance.json` also blocks distribution of the device renders pending the reference list and usage rights, creator/AI declaration, copyright assignment, source-file receipt, and product-shape/trademark review. The website pixels therefore have stronger technical continuity than legal provenance.

## 3. Is the headless Blender MCP still up?

**Short answer: partially installed and running, but not usable end to end right now.**

### Verified healthy layers

- Blender 4.2.1 LTS is installed at `/Applications/Blender.app/Contents/MacOS/Blender`.
- Direct background execution works and successfully inspected the current `.blend` and both GLBs.
- Blender’s Claude extension version 1.0.1 is installed.
- Two Claude-owned `blender-mcp` process chains have been alive since July 16.
- A fresh MCP handshake succeeded. The server identified itself as `blender-mcp` 1.27.0 and registered 26 tools.
- The last recorded full headless smoke test on July 15 passed: it created, measured, and removed a 10 x 20 x 30 mm cube without saving the scene.

### Broken or unavailable layers

- Nothing is listening on `localhost:9876`, so tools that execute inside a running Blender/add-on session cannot connect.
- No Blender process is currently running.
- A direct call to the MCP’s `_for_cli` blend-file tool failed with: Blender executable not found at `blender`; set `BLENDER_PATH`.
- The current Codex task exposes no Blender MCP tools. The installed server belongs to the Claude extension, not this runtime.
- Two duplicate MCP process chains are idle, which is unnecessary process duplication and not proof of a functioning Blender worker.

Operationally, the control plane is warm but the worker is disconnected. This should be treated as **amber/down**, not green/up.

## 4. Why this is the next CX frontier

Blender is a strong next workload for CX because it is bounded, artifact-producing, easy to verify visually and cryptographically, and naturally decomposes into preview, final, and repair passes. It also exercises the part of the exchange that inference alone does not: long-running heterogeneous compute with large inputs and outputs.

The MCP should be the operator and development interface, not the production execution protocol. Production workers should run a pinned headless Blender CLI with a narrow manifest, deterministic settings where possible, strict resource limits, isolated filesystems, denied-by-default networking, and source-bound output receipts. MCP can then inspect scenes, prepare jobs, and diagnose failures without becoming the security boundary.

## 5. Recommended sequence

### Immediate: reconcile truth and restore control

1. Decide which website is canonical and deploy one exact commit to the domain.
2. Remove the live overclaims or rebase the interactive hero onto the private-canary content and current CSP.
3. Fix the 390 px label collision and reduced-motion CSS defect.
4. Set `BLENDER_PATH=/Applications/Blender.app/Contents/MacOS/Blender` for the MCP server, keep one process chain, and add a read-only headless health check that opens a known `.blend` and reports object counts.
5. Expose the same pinned Blender MCP deliberately to the Codex/CX environment if it is meant to be part of the operating workflow.

### Next: make the render lab reproducible

1. Restore the authoritative builders and reference registry into a dedicated `computexchange-render-lab` repository or versioned artifact pack.
2. Bind every `.blend`, GLB, texture, reference, render, and review board to hashes and provenance receipts.
3. Resolve the visual-asset legal blockers before calling any model production-ready.
4. Rebuild the Mac hero grille, adopt an approved DGX foam LOD/shader strategy, and decide whether RTX/rig work is a marketing asset or the first production render workload.

### Product frontier: add rendering as a first-class CX workload

Define a narrow `blender_render` job cell with:

- a pinned Blender/Cycles version and engine/device declaration;
- scene bundle, frame/camera, samples, resolution, seed, time, memory, and output limits;
- preview, final, and optional repair/verification phases;
- sandboxed Python policy and no network by default;
- image/EXR/video artifacts with checksums, render configuration, hardware identity, timings, and failure receipts;
- benchmark-derived quotes and `max_usd` enforcement at dispatch;
- duplicate-frame or crop-based verification before settlement.

The first proof should be CX rendering its own Mac Studio hero through the exchange: submit the scene, quote it, render a deterministic preview and final, verify the output, and return a source-bound receipt. That would turn the visual system from marketing baggage into the clearest end-to-end demonstration of what Computexchange can become.
