# NOCTURNE/ONE sealed benchmark report

## Outcome

NOCTURNE/ONE passes every fixed P0/P1 3D and application gate after one bounded
keyboard repair. The result proves a complete application and editable 3D
product for this declared synthetic reference class. It does not prove an
arbitrary universal builder.

The benchmark definition was frozen at
`db820c9dec75cfa73ffd9ff800874cc81c465c2c`; the contract SHA-256 is
`b1ff7fd969075671ab59ffea65757a1fbcc825217fc456d002dd206cc7154fd6`.
The governed packet manifest SHA-256 is
`434a699dfd95e379c7c56d020fc5b0137f099c8c172ecb9690063df13b71d079`.
The sealed evaluator and hidden cameras were not accessible to the builder.

## Builder and repair chain

The H3 child ran for 3,356.67 seconds in a detached worktree with macOS
filesystem denial of the oracle/evaluator roots. It preserved six failed local
attempts before sealing attempt 007. The evaluator then found one failure:
seven anonymous wordmark/navigation anchors displaced the fixed eighth keyboard
target.

The bounded repair changed only the causal focus surface:

- assigned the skip link a stable identity;
- removed the redundant home wordmark self-link from the home Tab order;
- kept the poster canvas out of the Tab order until real 3D readiness;
- added the exact first-eight-target browser regression.

Attempt 007 remains preserved as failed evaluator evidence. Attempt 008 sealed
the exact repair; no threshold, reference, camera, oracle, or unrelated route
changed. The final candidate receipt SHA-256 is
`6c1f361fff01645d0c64f416bed7133b946515042fd946e6003fddf07f57c7f8`.

## 3D acceptance

The final editable BLEND contains all 12 semantic parts, separate transmissive
glass and emissive materials, UVs on all meshes, finite outward normals, no
negative scale, and no reported non-manifold edges. Both GLBs validate and
reimport with named-part identity; the exploded animation is deterministic at
frame 120.

Measured results:

| Gate | Result |
|---|---:|
| Dimensions | 320 × 180 × 360 mm; 0% error |
| Maximum part-placement error | 1.805% of diagonal (≤2%) |
| Public silhouette IoU | 0.9593–0.9782 (all ≥0.95) |
| Hidden silhouette IoU | 0.9565–0.9815 (all ≥0.92) |
| Fixed cameras | 6 public + 4 hidden; no evaluator camera nudge |
| Hero GLB | 393,040 bytes |
| Mobile LOD GLB | 201,484 bytes |

The 3D evaluator receipt SHA-256 is
`8cb9683912465f3a5d5c429c228aa3331bdb567f3271545fdc4863833c1bb0e9`.

## Application acceptance

All five routes and all 14 states are observed. The hidden 10-step mobile trace
finishes on `/receipt` in `successful_reservation`. Configuration and renderer
state remain identical across persistence. Authorization, validation,
transient errors, duplicate requests, actor scope, and idempotency pass against
the real SQLite-backed local API. Migration, reapplication, rollback, second
migration, `npm ci`, and the full verification command all exit zero.

Measured results:

| Gate | Result |
|---|---:|
| Keyboard journey | 8/8 fixed targets |
| Hidden mobile trace | 10/10; successful receipt |
| Accessibility | 0 critical, 0 serious across 5 routes |
| API local p95 | 0.849 ms over 30 samples |
| Initial JavaScript | 145,758 bytes |
| Layout shift | 0 |
| Desktop frame sample | 2,500 median FPS; 0.80 ms p95 |
| Mobile-emulated frame sample | 2,500 median FPS; 0.70 ms p95 |
| Interaction memory | 0-byte growth over 300 seconds / 60 samples |
| Poster/lazy GLB | no initial GLB; poster first; real intent loads GLB |
| No-WebGL | every non-3D route/journey remains available |

The frame samples measure the benchmark renderer’s deterministic request/render
loop on this machine; they are not general GPU/browser population estimates.
The application evaluator receipt SHA-256 is
`eedace5f3d09a32676e4bc6bdeaffa96a0a05d791d07635ec95a57e56c807997`.

## Repair corpus

The fixed twelve-class replay covers:

1. geometry dimension;
2. incorrect material class;
3. fixed camera mismatch;
4. broken mobile composition;
5. animation timing drift;
6. reduced-motion regression;
7. oversized GLB;
8. shader/frame-time regression;
9. API idempotency;
10. database migration;
11. accessibility;
12. unrelated-route regression.

All 12 injected facts were detected. In every drill the authority rejected
threshold relaxation and whole-baseline replacement, selected the exact
one-fact inverse, restored the canonical full receipt, and left zero failed
global assertions. Receipt SHA-256:
`26a97640b262d1e53f85e0827c72f2a482448b207f44cd364605579dab85f651`.

This is deterministic receipt-level repair replay over real runtime evidence.
It is not twelve fresh full Blender/browser/API/database runs; that limitation
is why the repair result does not independently justify a universal 110.

## Reproduction

```bash
cd /Users/scammermike/Downloads/visionmcp-authority-worktrees/visionmcp-100plus-sandbox/tools/blender-vision-mcp

cd sandbox/nocturne-one
npm ci
npm run verify
npm run db:migrate
npm run db:rollback
npm start

cd ../..
uv run bvmcp benchmark run-nocturne-repair-drills \
  --app-receipt artifacts/nocturne-one/3f56653-h4-repair/evaluator-app/nocturne-app.receipt.json \
  --three-d-receipt artifacts/nocturne-one/3f56653-h4-repair/evaluator-3d/nocturne-3d.receipt.json \
  --output artifacts/nocturne-one/3f56653-h4-repair/repair-drills/nocturne-repair-drills.receipt.json
```

Evaluator-only reruns additionally require the sealed oracle/evaluator root and
hidden mobile trace; those paths intentionally remain outside the builder
tree.
