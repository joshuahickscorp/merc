# NOCTURNE/ONE Live Sandbox Acceptance Report

Run ID: `live-acceptance-20260726T052610Z`
Starting commit: `75fc1c9308e5f346fe74aa6b59595e94af2ac30d`
Accepted live URL: `http://127.0.0.1:4173`
Accepted server PID: `10768`
Server log: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/logs/server.log`

## Acceptance decision

The H3 NOCTURNE/ONE application and editable 3D asset are accepted locally and remain running for operator inspection. The fresh accepted frozen app evaluator passed all 27 assertions, and the fresh accepted frozen 3D evaluator passed all 17 assertions. This run did not change any score: it replaced score-only claims with live evidence.

The fresh isolated H4 attempt is honestly classified **FAIL (mixed)**. Its trusted local gate passed with `LOCAL_PASS_EXTERNAL_UNMEASURED`; the unchanged frozen 3D evaluator passed all 17 assertions; the unchanged frozen app evaluator failed exactly one P0 assertion, `keyboard_journey`, because the first eight Tab targets were `["A", "A", "A", "A", "A", "A", "enter-3d", "A"]` and did not include both `enter-3d` and a navigation target. All other 26 frozen app assertions passed. The evaluator was not weakened and the H3 acceptance remains available as permitted by the contract.

## Fresh execution

- Created a new clean worktree at `/Users/scammermike/Downloads/visionmcp-authority-worktrees/visionmcp-live-sandbox-acceptance-20260726T052610Z` from the exact pushed commit.
- Installed JavaScript and Python dependencies from lockfiles using fresh `npm ci` and `uv sync --all-extras --frozen`; no prior modules, databases, builds, screenshots, traces, or receipts were accepted as evidence.
- Ran typecheck, build, unit, API, integration, browser, GLB/3D, migration, rollback, re-migration, and aggregate verification paths.
- Started the complete Node application/API with fresh SQLite state, exercised reservations and actor/idempotency policy, and verified persisted configuration/reservation rows plus SQLite integrity/WAL state.
- Ran desktop `1440×900 @2x` and mobile `390×844 @3x` journeys through the live application, including the 10-step hidden mobile trace ending at `/receipt`.
- Loaded the real hero and mobile LOD GLBs, switched material/configuration, selected `glass_core`, ran the exploded animation, and verified deterministic Blender frame 120.
- Exercised poster-first loading, reduced motion, no-WebGL, slow network, offline interruption/retry, transient 503 recovery, validation error, successful reservation, and restored persistence.
- Used VisionMCP itself for 30 live tool calls, 13 fresh captures, all five routes, 17 perceptual states, experience/graphics/offline review, and receipt verification.
- Rebuilt the editable BLEND, poster, hero GLB, and low GLB in fresh Blender 4.2.1 LTS executions, then reopened/reimported and evaluated them.
- Ran all twelve repair faults as full-runtime drills; no receipt replay or blocker was counted.
- Ran a fresh, clean, pinned `gpt-5.6-sol` H4 model session with denied oracle access, a trusted local contract preflight, and both unchanged frozen evaluators.

Implementation from the starting commit was reused because it is the system under test. No prior evidence, dependency directory, database, build, screenshot, trace, or receipt was reused as proof. The governed packet and frozen evaluator implementation are the fixed acceptance instruments; fresh packet/oracle copies and manifests were created for this run.

## Coverage and results

Routes: `/`, `/technology`, `/configurator`, `/reserve`, `/receipt`.

Semantic states observed with none missing: `3d_ready`, `3d_unavailable`, `api_transient_error`, `api_validation_error`, `empty_configuration`, `initial_loading`, `keyboard_navigation`, `offline_retry`, `poster_fallback`, `reduced_motion`, `restored_saved_configuration`, `slow_network`, `successful_reservation`, `touch_interaction`.

API statuses: unauthorized `401`, forbidden `403`, validation `400`, transient `503`, first reservation `201`, idempotent replay `200`, payload conflict `409`, same-actor lookup `200`, cross-actor lookup `404`. API p95 was `0.935 ms` over `100` live samples; frozen API p95 was `0.565 ms`.

SQLite: integrity `ok`, journal mode `wal`, migration count `1`, configuration rows `7`, reservation rows `2`; both browser and direct-API reservations were persisted.

Accessibility: zero critical and zero serious violations across all five routes at desktop and mobile widths in the live scan; the frozen evaluator also recorded zero critical and zero serious violations.

Accepted 3D hashes:

- BLEND: `e2e85cacee96498d5d26d226e4d818c230182a6974648a13f75360bd6d872133`
- Hero GLB: `089b73ac09cf752b4e343682a7acae898915dfe514c4e5c0c30a74122b7c8084`
- Mobile LOD GLB: `02c1b1fe8a869d09870c7e6e2375e04a4977fbbb02c26e117208ba2c1c83e6d4`
- Poster WEBP: `68433cbf013b314ec34d8235780c5f4fa512085b2ebf0d15fef4d2db907db495`

The accepted frozen 3D evaluator passed all public and hidden fixed-camera silhouettes. Public IoU range was `0.959298`–`0.978155`; hidden range was `0.956521`–`0.981488`.

Performance with real 3D loaded: desktop frame p95 `0.700 ms` from `120` samples; mobile frame p95 `0.700 ms` from `120` samples; CLS `0.0`; 300-second memory growth `0` bytes. The live journey measured first-real-frame at `150.051 ms`. These deterministic local measurements describe this recorded host and render-submission loop, not population GPU performance. The intentionally induced offline request failure and its expected console entries are preserved, not counted as unexplained production errors.

Repair classification: `12` full runtime, `0` replay, `0` blocked. Every canonical drill demonstrated detection of one isolated injected fault, performed a bounded repair, restored the original manifest, passed its local and global gates, and passed unrelated-route/feature checks. The first browser-drill pass that omitted a build is retained as failed-attempt evidence; the corrected five browser executions are canonical.

## H4 contract correction

The system now binds candidate local metrics to fresh-clone commands, exact API status semantics, public selectors and element types, reopened Blender dimensions/hierarchy/root/part placement/material/animation checks, fixed public evaluator-camera silhouette IoUs, GLB validation/reimports, real WebGL/GLB behavior, reduced motion, and responsive routes. The gate cannot emit `GLOBAL_PASS`; it emits only `LOCAL_PASS_EXTERNAL_UNMEASURED` or `LOCAL_FAIL`, with global acceptance remaining `EXTERNAL_UNMEASURED` until frozen evaluation.

In the fresh H4 run, that gate rejected the first candidate on public silhouette IoUs, the model repaired it, and the second local gate passed. Frozen 3D then passed, including all hidden views. Frozen app subsequently exposed the remaining keyboard-order mismatch. No causal H0–H4 model-uplift claim is made because the same pinned model was not run across H0–H4.

## Failures, blockers, and limitations

- Preserved setup/invocation failures are present in the command ledger and attempt directories: early self-observation passes, sealed-candidate passes, repair pass one browser omission, initial H4 sandbox launch denial, outer disposable `.venv` symlink boundary rejection, and one incorrect app-evaluator CLI invocation. Each is retained rather than erased.
- The outer H4 runner found three Python symlinks only inside its disposable repository-level `.venv`. Scoped recovery removed that exact disposable directory after verifying the candidate tree digest was unchanged (`adab1c1…`), rechecked denied oracle access and boundary rules, and copied the exact candidate for frozen evaluation.
- `mypy` was not installed as an executable in the frozen Python environment; targeted tests and Ruff passed. This is a tooling availability limitation, not reported as a mypy pass.
- Fresh H4 remains failed on one external P0 keyboard journey. There are no blockers to inspecting or operating the accepted H3 application.
- No video file was generated; screenshots and Playwright traces provide the fresh visual and interaction evidence. The required `videos/` evidence directory exists and is intentionally empty.

## Reproduction commands

Run from `tools/blender-vision-mcp` in a new worktree at `75fc1c9308e5f346fe74aa6b59595e94af2ac30d`:

```bash
uv sync --all-extras --frozen
cd sandbox/nocturne-one
npm ci
npm run verify
npm run migrate
npm run migrate:down
npm run migrate
env HOST=127.0.0.1 PORT=4173 DATABASE_PATH=data/nocturne.sqlite3 npm start
```

Fresh accepted frozen checks:

```bash
uv run bvmcp benchmark evaluate-nocturne-3d --packet artifacts/live-sandbox/live-acceptance-20260726T052610Z/oracle-bootstrap/input-packet --oracle artifacts/live-sandbox/live-acceptance-20260726T052610Z/oracle-bootstrap/sealed-evaluator --candidate artifacts/live-sandbox/live-acceptance-20260726T052610Z/sealed-candidate-pass004 --builder-receipt artifacts/live-sandbox/live-acceptance-20260726T052610Z/sealed-builder-pass004/sealed-builder.receipt.json --output artifacts/live-sandbox/live-acceptance-20260726T052610Z/frozen-3d-evaluator --contract benchmarks/nocturne_one/contract.json
uv run bvmcp benchmark evaluate-nocturne-app --packet artifacts/live-sandbox/live-acceptance-20260726T052610Z/oracle-bootstrap/input-packet --candidate artifacts/live-sandbox/live-acceptance-20260726T052610Z/sealed-candidate-pass004 --builder-receipt artifacts/live-sandbox/live-acceptance-20260726T052610Z/sealed-builder-pass004/sealed-builder.receipt.json --hidden-mobile-trace artifacts/live-sandbox/live-acceptance-20260726T052610Z/oracle-bootstrap/sealed-evaluator/mobile/hidden-interaction-trace.json --output artifacts/live-sandbox/live-acceptance-20260726T052610Z/frozen-app-evaluator --contract benchmarks/nocturne_one/contract.json
```

The complete, exact argv/cwd/timing/exit status plus stdout/stderr paths and hashes for every recorded execution are in `artifacts/live-sandbox/live-acceptance-20260726T052610Z/command-ledger.jsonl`.

## Evidence index

- App receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/app-receipt.json`
- 3D receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/3d-receipt.json`
- Performance receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/performance-receipt.json`
- Repair receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/repair-receipt.json`
- VisionMCP self-observation: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/visionmcp-self-observation-receipt.json`
- H4 receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/h4-receipt.json`
- Completion receipt: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/completion-receipt.json`
- Manifest: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/manifest.json`
- Environment: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/environment.json`
- Command ledger: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/command-ledger.jsonl`
- Screenshots: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/screenshots/`
- Traces: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/traces/`
- Logs: `artifacts/live-sandbox/live-acceptance-20260726T052610Z/logs/`

## Production isolation

The production ComputExchange checkout at `/Users/scammermike/Downloads/computexchange` was not modified by this run, and no production website redesign was started. Final production HEAD/status/diff hashes are recorded in the completion receipt and match the pre-run baseline.
