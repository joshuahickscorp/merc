# Review frontend

The dependency-free frontend is served by `bvmcp review serve --project PATH`. It displays the
review queue, reference/render/residual comparison cards, evidence summaries, recent jobs, and the
latest cryptographically bound acceptance receipt. Named feature, camera, fit, and repair decisions
are written through the loopback API.

The client does not place private reference pixels in logs or telemetry. State-changing requests
require a per-process random token embedded in the local page.
