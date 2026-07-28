# Fixed NOCTURNE/ONE H3 builder prompt

You are the builder in a sealed benchmark. Work only in the current detached
worktree. The governed input packet is the absolute directory in
`NOCTURNE_PACKET_ROOT`. The fixed public contract is
`tools/blender-vision-mcp/benchmarks/nocturne_one/contract.json`.

The OS denies oracle-author source, oracle BLEND, hidden cameras, hidden
holdouts, mesh statistics, material node values, and the hidden mobile trace.
Do not attempt to bypass that denial. Do not search outside the worktree or
input packet for NOCTURNE/ONE data.

Build a new standalone project at:

`tools/blender-vision-mcp/sandbox/nocturne-one/`

Use VisionMCP's governed packet, application authority, Blender/GLB validators,
performance contracts, and receipt tools. The output must be an original
clean-room implementation from the supplied synthetic owned references and
text. Do not copy an existing site template, embed any source reference mesh,
replace interactive 3D with video, or fake the reservation API in the client.

Required deliverables:

- `3d/nocturne-one.blend`, editable and cleanly named;
- `3d/build_candidate.py`, the independent parametric source used to create it;
- `public/assets/nocturne-one-hero.glb` and `nocturne-one-low.glb`;
- a TypeScript application with the five fixed routes and all fixed states;
- a real lazy-loaded GLB/WebGL renderer with poster-first and no-WebGL paths;
- pointer, touch, keyboard, scroll, reduced-motion, and responsive behavior;
- configuration persistence with identical application and 3D state;
- an Express or equivalent local API backed by SQLite;
- authorization, validation, transient, idempotency, duplicate, and
  cross-actor handling;
- up/down migrations, deterministic seed, unit/integration/API/browser/
  accessibility/visual/3D tests, and a pinned lockfile;
- `npm ci`, `npm run verify`, `npm run db:migrate`, `npm run db:rollback`, and
  `npm start` must work from a fresh source copy;
- no `node_modules`, build cache, or runtime database in the source tree.

Expose the public evaluator protocol as `window.__NOCTURNE__`, exactly as
declared in the packet's product brief. Its values must report real application
and renderer state. `setTestCondition` may select the public slow-network and
transient-error drills, but it may not directly manufacture a passing result.

Model the fictional product from the supplied dimensions, part/material
specification, six renders, and two motion references. Use an independent
candidate mesh construction, not oracle-source geometry. Include the 12 exact
semantic part node names in the editable scene and both GLBs. Keep the
transparent glass core and emissive eclipse disk as separate editable
materials. UV every mesh, preserve outward finite normals, avoid non-manifold
edges and negative scale, provide named LOD identity, and keyframe the declared
exploded parts from frames 1 to 120.

Preserve each unsuccessful build or repair under `failed-attempts/` with a JSON
receipt. Write the accepted attempt receipt under `.visionmcp/`. When all local
checks pass, seal the exact source tree with:

```bash
uv run bvmcp benchmark seal-nocturne-candidate \
  --packet "$NOCTURNE_PACKET_ROOT" \
  --candidate tools/blender-vision-mcp/sandbox/nocturne-one \
  --condition H3-codex-sealed \
  --attempt attempt-001:ACCEPTED:.visionmcp/attempt-001.json
```

If earlier attempts failed, include every one as an additional
`--attempt ID:FAILED:relative/path.json` argument in chronological order before
the final accepted attempt.

Do not stop at a scaffold. Run the real Blender generation, GLB validation,
fresh npm verification, API/database tests, and browser tests. Finish only when
the candidate receipt exists and a concise terminal summary states the exact
test results and remaining known limitations.
