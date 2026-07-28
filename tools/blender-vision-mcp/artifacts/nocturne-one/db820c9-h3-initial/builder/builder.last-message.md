Sealed successfully at [sandbox/nocturne-one](/private/tmp/nocturne-one-builder-db820c9/tools/blender-vision-mcp/sandbox/nocturne-one).

- Receipt: [nocturne-build.receipt.json](/private/tmp/nocturne-one-builder-db820c9/tools/blender-vision-mcp/sandbox/nocturne-one/.visionmcp/nocturne-build.receipt.json), SHA-256 `bedd43987df7d1bcca04e084d88e2d5651de3e684ddb4f1da1b4033ba3982975`
- Receipt verification: valid, 35 files, seven attempts; six failures retained and `attempt-007` accepted
- Fresh copy: `npm ci`, `npm run verify`, migrate twice, rollback, reapply, and `npm start` all passed
- Tests: 4 unit, 2 API/integration, five browser routes, zero accessibility findings, WebGL/no-WebGL/reduced-motion flows passed
- 3D: 12/12 parts, 8/8 animated parts, 320×180×360 mm, no UV/topology/normal/scale issues
- GLBs: hero 393,040 bytes; low 201,484 bytes; both VisionMCP-valid
- Public silhouette IoU: `0.959298–0.978155`, all above `0.95`
- Performance: API p95 `2.819 ms`; 300-second memory run, 60 samples, `0` bytes growth; renderer submission p95 approximately `0.7 ms`
- Source tree contains no `node_modules`, `dist`, runtime database, or build cache

Known limitations: VisionMCP emits one advisory because the rear grille uses 23 closed manifold fins rather than aperture evidence. Hidden holdouts and the hidden mobile trace remain evaluator-only and were not accessed.