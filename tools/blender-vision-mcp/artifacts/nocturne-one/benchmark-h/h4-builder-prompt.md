# Fixed NOCTURNE/ONE H4 builder prompt

You are the builder in a sealed benchmark. Work only in the current detached
worktree. The governed input packet is the absolute directory in
`NOCTURNE_PACKET_ROOT`. The fixed public contract is
`tools/blender-vision-mcp/benchmarks/nocturne_one/contract.json`.

The OS denies the oracle author, oracle BLEND, hidden cameras, hidden holdouts,
hidden mobile trace, prior benchmark conditions, prior Codex state, and every
other NOCTURNE/ONE candidate. Do not attempt to bypass that denial. Do not
search outside the worktree or governed packet for NOCTURNE/ONE data.

Build a new standalone project from scratch at:

`tools/blender-vision-mcp/sandbox/nocturne-one/`

Use VisionMCP's governed packet, application authority, Blender/GLB validators,
performance contracts, candidate portfolios, evidence ranking, and bounded
repair facilities. The output must be an original clean-room implementation
from the supplied synthetic owned references and text. Do not copy an existing
site template, embed reference meshes, replace interactive 3D with video, or
fake the reservation API in the client.

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
declared in the product brief. Its values must report real application and
renderer state. `setTestCondition` may select the public slow-network and
transient-error drills, but it may not directly manufacture a passing result.

Model the product from the dimensions, part/material specification, six
renders, and two motion references. Use independent mesh construction. Include
the 12 exact semantic part names in the editable scene and both GLBs. Keep the
transparent glass core and emissive disk as separate editable materials. UV
every mesh, preserve outward finite normals, avoid non-manifold edges and
negative scale, provide named LOD identity, and keyframe the declared exploded
parts from frames 1 to 120.

This is H4. Before choosing the accepted implementation:

1. Create a deterministic candidate portfolio with at least three genuinely
   distinct bounded hypotheses for one high-information 3D choice and at least
   three bounded implementation hypotheses for one high-information app choice.
2. Evaluate and rank every candidate with fixed public metrics. Preserve every
   candidate definition, metric record, rejection reason, and ranking under
   `portfolios/`; do not keep only the winner.
3. For every failed build or test, write a structured failure receipt before
   changing source. Rank the failure by severity and information gain.
4. Generate a bounded repair plan naming the smallest causal file/region,
   permitted invariant changes, and global gates that must remain green.
5. Apply only the selected bounded repair, rerun its local gate, then rerun the
   full application and 3D regression suite. Reject and retain local wins that
   break another route, state, API, accessibility, performance, or 3D gate.
6. Preserve the machine-readable repair ledger under `.visionmcp/`.

Write the final accepted-attempt receipt under `.visionmcp/`. Seal the exact
source tree with:

```bash
uv run bvmcp benchmark seal-nocturne-candidate \
  --packet "$NOCTURNE_PACKET_ROOT" \
  --candidate tools/blender-vision-mcp/sandbox/nocturne-one \
  --condition H4-codex-sealed-portfolio \
  --attempt attempt-001:ACCEPTED:.visionmcp/attempt-001.json
```

If earlier attempts fail, include each as
`--attempt ID:FAILED:relative/path.json` in chronological order before the
accepted attempt.

Do not stop at a scaffold. Run real Blender generation, GLB validation, fresh
npm verification, API/database tests, installed-Chrome browser tests,
accessibility checks, visual captures, performance checks, portfolio ranking,
and bounded repair. Finish only when the candidate receipt exists and a concise
terminal summary states exact tests, candidate/repair counts, and limitations.
