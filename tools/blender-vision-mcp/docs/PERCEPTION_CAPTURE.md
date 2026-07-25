# Perception capture bus

VisionMCP's perception expansion starts with one governed vertical slice: an isolated Chromium
sensor captures an explicitly allowlisted web target, writes every observation into the existing
content-addressed artifact store, publishes an immutable observation envelope, and exposes a
queryable `LayoutGraph`.

## Authority and identity

Every capture is `OBSERVED` evidence. It is not a claim about author intent, hidden state, or
pixels outside the captured environment. The SHA-256 capture identity covers:

- adapter name and version;
- normalized target;
- viewport, device scale, locale, timezone, theme, reduced-motion preference, wait policy, and
  browser launch settings;
- concrete platform, Python, Playwright, and browser version;
- source identity and the caller's rights decision.

An identical request reuses the verified envelope without launching the sensor. A changed
environment creates a new identity. Artifacts are committed as the adapter emits them. If a
capture is interrupted, retry resumes the same identity and refuses any role whose new digest
would mix a different sensor state into the earlier observation.

The envelope, each artifact, and every lifecycle event receipt are content-addressed.
`vision.verify` re-hashes them and checks that the envelope's request and artifact index exactly
match the project database.

## Chromium sensor

`browser.chromium` currently records:

- viewport and full-page PNG screenshots;
- serialized HTML and a Chrome DevTools Protocol DOM snapshot;
- the full accessibility tree;
- computed layout/style/source bindings in `LayoutGraph`;
- stylesheets, loaded fonts, asset URLs, and non-DOM surface inventory;
- network metadata, console messages, document metadata, and browser performance metrics.

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
