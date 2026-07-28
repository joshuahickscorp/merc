# Performance repair authority

VisionMCP evaluates performance as observed behavior, not as a source-code
optimization claim. The fixed `performance-repair-authority-v1` benchmark runs an
owned application twice in isolated installed-Chromium sessions: first from a
deliberately degraded preimage and then after an exact, digest-bound repair.

Run it with:

```bash
BVMCP_RUN_BROWSER_TESTS=1 uv run bvmcp benchmark bootstrap-performance \
  --output artifacts/performance-run
```

The command requires a new or empty output directory. Its receipt binds the full
Git source revision, benchmark-manifest digest, fixture-tree digest, installed
browser executable digest and version, WebGL instrumentation version, GLB
validator, SQLite version, repair diff, all measurements, and output artifact
digests.

## Observed surfaces

The benchmark measures:

- cold initial response-body transfer bytes from a no-store loopback origin and empty context;
- page-reported boot execution and independent Chromium CDP `ScriptDuration`;
- selected GLB bytes and strict GLB 2.0/named-identity validation;
- actual WebGL texture allocation dimensions and shader compilation duration;
- 24 requestAnimationFrame samples, p95 frame duration, and the dropped-frame ratio;
- buffered long tasks and cumulative layout shift;
- five end-to-end interaction samples;
- exposed Chromium heap growth plus explicit retained application allocations;
- eight API request samples and payload stability;
- 64 indexed SQLite queries plus `EXPLAIN QUERY PLAN`;
- initial-versus-intent resource sequencing, low-LOD selection, and DPR capping;
- independent reduced-motion and forced-no-WebGL sessions.

The raw browser measurements stay distinct. In particular, the fixture-reported
boot duration does not replace CDP duration, explicit retained allocations do not
replace heap observations, and fetched texture bytes must equal the dimensions
observed at the WebGL upload boundary.

## Bounded repair

The repair may change only `fixture/app.js`. It refuses a stale preimage digest,
requires each approved literal to occur exactly once, records a unified diff, and
reports the sole changed path. Its seven permitted changes are:

- eager to intent-driven 3D loading;
- full to adaptive low LOD;
- DPR cap from four to two;
- per-frame and per-interaction work reductions;
- removal of deliberate retained allocations;
- honoring `prefers-reduced-motion`.

There is no arbitrary source rewrite or post-run threshold change.

## Preservation and corruption controls

An accepted repaired result must remain byte-identical at the screenshot gate,
ARIA-identical, behavior-identical, API-payload-identical, and equal in validated
GLB node/mesh identity. A performance win that breaks any of those gates fails.

Four injected controls prove that the evaluator rejects an over-budget transfer,
an eager/non-intent asset sequence, a structurally invalid GLB claim, and a visual
preservation failure. The degraded preimage and every failed assertion remain in
the receipt.

## Claim boundary

This is real installed-Chromium, WebGL2, loopback HTTP, and SQLite execution on the
host that produced the receipt. It proves measured rejection and bounded repair
for the owned fixture. It does not establish universal performance on arbitrary
applications, physical mobile devices, production networks or databases, unseen
3D scenes, or unmeasured GPU/driver combinations. GLB acceptance is structural
and identity-based; it is not a perceptual-equivalence claim.
