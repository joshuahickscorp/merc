# Perception capture bus

VisionMCP's browser sensor captures an explicitly allowlisted web target in an isolated Chromium,
Firefox, or WebKit context, writes every observation into the content-addressed artifact store,
publishes an immutable observation envelope, and exposes a queryable `LayoutGraph`. The historical
adapter identifier remains `browser.chromium` for API compatibility; `configuration.engine` is the
authoritative runtime selector.

## Authority and identity

Every capture is `OBSERVED` evidence. It is not a claim about author intent, hidden state, or
pixels outside the captured environment. The SHA-256 capture identity covers:

- adapter name and version;
- normalized target;
- engine, executable path and SHA-256, viewport, screen, device scale, orientation, touch/mobile
  emulation, locale, timezone, color scheme, contrast, forced colors, reduced motion, offline or
  throttled-network profile, wait policy, and browser launch settings;
- concrete platform, Python, Playwright, executable, and browser version;
- source identity and the caller's rights decision.

An identical request reuses the verified envelope without launching the sensor. A changed
environment creates a new identity. Artifacts are committed as the adapter emits them. If a
capture is interrupted, retry resumes the same identity and refuses any role whose new digest
would mix a different sensor state into the earlier observation.

The envelope, each artifact, and every lifecycle event receipt are content-addressed.
`vision.verify` re-hashes them and checks that the envelope's request and artifact index exactly
match the project database.

## Cross-browser sensor

`browser.chromium` records across all three engines:

- viewport and full-page PNG screenshots;
- serialized HTML and either a Chrome DevTools Protocol DOM snapshot or an explicitly labeled
  portable layout snapshot;
- Playwright ARIA output, a portable semantic accessibility snapshot, deterministic keyboard Tab
  traversal, and the full CDP accessibility tree on Chromium;
- computed layout/style/source bindings in `LayoutGraph`;
- stylesheets, loaded fonts, asset URLs, and non-DOM surface inventory;
- network metadata, console messages, document metadata, and CDP or Web Performance API metrics.

Chromium alone supplies CDP DOM/AX/performance evidence and real fast-3G/slow-3G bandwidth
throttling. Firefox and WebKit do not silently receive Chromium-shaped claims: their portable
artifacts name their evidence format, and unsupported bandwidth profiles are rejected. Offline
capture bootstraps the owned application online, transitions the loaded context offline, dispatches
the runtime event, and captures the resulting state with `navigator.onLine=false`.

Mobile evidence binds viewport, DPR, orientation, touch, `is_mobile` where Playwright supports it,
and the observed media-query state. Playwright does not support `is_mobile` for Firefox; requesting
that combination is rejected rather than degraded without notice.

`vision.query` supports selector, semantic role, text, point, asset URL, surface, computed-style,
source-binding, and interactivity filters. Every response cites the exact graph artifact digest
and capture identity.

## Isolation and network policy

The browser uses a new empty context for every capture. It never opens a personal browser profile,
blocks service workers, disables cache, and launches with background networking and synchronization
disabled. The caller must provide exact HTTP(S) origins. Private, loopback, link-local, and other
special-use addresses are rejected unless `allow_private_network` is explicitly enabled. Every
subresource request is checked against the same origin and address policy.

Request/response metadata and console messages recursively redact authentication, cookie, password,
secret, token, API-key, credential, and signature fields plus bearer tokens. Response bodies are
not collected. Target credentials and URL fragments are rejected or discarded.

## MCP surface

- `vision.adapters` lists installed sensors without starting them.
- `vision.observe` captures or reuses an observation.
- `vision.discover_states` performs bounded pointer, keyboard, and touch exploration plus a
  viewport sweep and deterministic motion/scroll sampling.
- `vision.capture_state`, `vision.trace_behavior`, and `vision.analyze_motion` progressively
  disclose observed state, causality, and replayable tracks.
- `vision.query` queries the capture's observed graph.
- `vision.verify` verifies its immutable evidence chain.
- `vision://project/{project_id}/overview` lists project observations.
- `vision://project/{project_id}/graph/{graph_type}` returns the latest complete graph.
- `vision://project/{project_id}/node/{node_id}` and
  `vision://project/{project_id}/timeline/{timeline_id}` disclose one graph entity at a time.

Cross-origin iframe internals and closed shadow roots are reported as limitations rather than
guessed. The bounded crawler does not treat undiscovered states as absent. Design-authoring
adapters, WebGL/WebGPU scene reconstruction, Feature Capsules, repair loops, desktop sensors, and
active learning remain later expansion waves.

## Fixed runtime matrix

The immutable corpus is `benchmarks/100_plus/browser/manifest.json`. It binds the owned fixture,
Chromium/Firefox/WebKit engine registry, keyboard/touch responsive and motion gates, and four
environment profiles:

- portrait mobile, DPR 3, touch, dark color scheme, and reduced motion;
- landscape mobile, DPR 2, and touch;
- a loaded application transitioned offline;
- Chromium slow-3G network emulation.

Run it with:

```bash
bvmcp browser benchmark \
  --output artifacts/100-plus/browser-matrix/<source-sha>
```

The receipt is functionally passing only when Chromium, at least one additional engine, every
environment profile, content-addressed capture verification, keyboard traversal, and the fixed
accessibility threshold pass. Each unavailable engine retains `BLOCKED_EXTERNAL` and an exact
installation/retry contract; successful engines are not discarded because another runtime is
blocked.
