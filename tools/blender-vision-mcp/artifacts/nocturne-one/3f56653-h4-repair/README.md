# NOCTURNE/ONE bounded repair result

This directory preserves the complete post-repair evaluator evidence for the
candidate at Git head `3f56653cb208f930cbf4d677dc20a65fc15a9605`.

## Causal repair

The immutable initial H3 run passed the full 3D evaluator but failed one of 27
application assertions: P0 `keyboard_journey`. The first eight Tab presses
visited only anonymous anchors and did not reach the `enter-3d` control.

The bounded repair changed two source files:

- `src/client/main.ts` gives the skip link a stable identity, removes the
  redundant home wordmark self-link from the home-route Tab order, keeps the
  inactive poster canvas out of the Tab order, and makes it focusable only
  after a real 3D scene is ready.
- `tests/browser.test.ts` reproduces the evaluator's exact eight-Tab bound and
  verifies that the canvas becomes keyboard-focusable after activation.

Attempt 007 and the initial failed receipt remain preserved under
`artifacts/nocturne-one/db820c9-h3-initial/`. Attempt 008 binds the repair and
the candidate was resealed as `H4-codex-bounded-repair`.

## Application result

- Status: `PASS`
- Assertions: `27 / 27`
- Keyboard targets:
  `skip-to-main, A, A, A, A, A, A, enter-3d`
- Hidden mobile trace: `10 / 10` steps, final route `/receipt`, final state
  `successful_reservation`
- Accessibility: `0` critical and `0` serious findings across five routes
- API p95: `0.8487500018 ms` over 30 samples
- Five-minute memory growth: `0 bytes`
- Cumulative layout shift: `0.0`
- Initial JavaScript transfer: `145758 bytes`
- App evaluator receipt SHA-256:
  `eedace5f3d09a32676e4bc6bdeaffa96a0a05d791d07635ec95a57e56c807997`

## 3D result

- Status: `PASS`
- Assertions: all passed
- Public silhouette IoU: `0.9592975674` to `0.9781549393`
- Hidden silhouette IoU: `0.9565207319` to `0.9814884414`
- 3D evaluator receipt SHA-256:
  `8cb9683912465f3a5d5c429c228aa3331bdb567f3271545fdc4863833c1bb0e9`

## Authority

- Repaired candidate receipt SHA-256:
  `6c1f361fff01645d0c64f416bed7133b946515042fd946e6003fddf07f57c7f8`
- Original sealed-builder receipt SHA-256:
  `8e95cd6b2e6655372c874398fbb3200a7d963b1af9460e3db19fe5bdee19b4a6`
- Contract SHA-256:
  `b1ff7fd969075671ab59ffea65757a1fbcc825217fc456d002dd206cc7154fd6`
- Packet manifest SHA-256:
  `434a699dfd95e379c7c56d020fc5b0137f099c8c172ecb9690063df13b71d079`

The evaluator definition remained fixed at the pre-builder Git head
`db820c9dec75cfa73ffd9ff800874cc81c465c2c`.
