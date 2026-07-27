# V2 full-runtime repair corpus (Phase Q)

The repair corpus injects each named visual/delivery failure into a **concrete
artifact**, measures it with a live specialist critic, applies a **bounded
repair**, and re-measures. It is not frozen-receipt replay
(`nocturne_repair_drills` authority).

## Layout

| Path | Role |
| --- | --- |
| `src/blender_vision/benchmarks/repair_corpus.py` | Drill registry + `run_repair_drill` / `run_repair_corpus` |
| `scripts/run-repair-corpus.py` | CLI matrix runner |
| `tests/test_v2_repair_corpus.py` | Inject/detect/repair proofs |
| `artifacts/v2/repair/` | Receipts, matrix, `failed-attempts/` |

## Per-drill contract

Each drill is a dataclass declaring:

1. **id / category / failure class**
2. **injection** — reversible mutation of a real artifact (mesh GLB, material
   parameters, shaded image, camera-path metrics, delivery shell bytes,
   process timings)
3. **expected detector** — which of the thirteen critics and which finding key
4. **bounded repair** — named parameters + blast radius only
5. **acceptance test** — the critic finding must clear after repair

`run_repair_drill()` always:

1. captures a clean baseline measurement,
2. injects,
3. re-measures and **requires the detector to fire** (a miss is a drill failure),
4. applies the bounded repair,
5. re-measures and requires acceptance,
6. runs a global re-check on an unrelated control subject,
7. preserves the injected artifact + failing measurement under
   `failed-attempts/<drill_id>/`.

## Named failures

### Geometry (5)

| Drill id | Critic | Finding key |
| --- | --- | --- |
| `geometry-wrong-dimensions` | industrial_designer | proportion-scale |
| `geometry-wrong-hidden-surface` | adversarial_acceptance_reviewer | hidden-view |
| `geometry-missing-semantic-part` | industrial_designer | part-count |
| `geometry-bad-topology` | industrial_designer | fake-drawer |
| `geometry-lod-identity-mismatch` | adversarial_acceptance_reviewer | detail-removed |

### Material (5)

| Drill id | Critic | Finding key |
| --- | --- | --- |
| `material-plastic-metal` | material_artist | plastic-metal |
| `material-wrong-roughness` | material_artist | plastic-metal |
| `material-flat-fake-foam` | material_artist | flat-texture-depth |
| `material-texture-scale-error` | material_artist | wrong-pore-scale |
| `material-offline-browser-mismatch` | material_artist | plastic-metal (+ ΔE gate) |

### Lighting (5)

| Drill id | Critic | Finding key |
| --- | --- | --- |
| `lighting-clipped-hero` | lighting_artist | clipped-hero |
| `lighting-floating-contact` | lighting_artist | floating-object |
| `lighting-wrong-exposure` | lighting_artist | flat-corridor |
| `lighting-flat-corridor` | lighting_artist | flat-corridor |
| `lighting-excessive-glow` | lighting_artist | overfilled-shadows |

### Cinematic (6)

| Drill id | Critic | Finding key |
| --- | --- | --- |
| `cinematic-delayed-camera` | cinematographer | camera-lag |
| `cinematic-dead-scroll` | cinematographer | dead-scroll |
| `cinematic-text-collision` | accessibility_reviewer | contrast-ratio |
| `cinematic-left-turn-overshoot` | cinematographer | turn-intent |
| `cinematic-mobile-crop` | editorial_art_director | generic-composition |
| `cinematic-reduced-motion-regression` | accessibility_reviewer | reduced-motion |

### Delivery (6)

| Drill id | Critic | Finding key |
| --- | --- | --- |
| `delivery-oversized-shell` | performance_engineer | long-tasks (+ shell budget) |
| `delivery-decode-long-task` | performance_engineer | long-tasks |
| `delivery-memory-growth` | performance_engineer | memory-growth |
| `delivery-blank-first-frame` | performance_engineer | cls |
| `delivery-shader-flash` | performance_engineer | frame-p95 |
| `delivery-no-webgl-content-loss` | accessibility_reviewer | textual-equivalent |

The contract text says “25 failures” while the semicolon-separated list names
**27** distinct classes. The corpus implements every named class (27 rows).

## External runtimes

Geometry / material / lighting drills declare **Blender** as the preferred
external re-check. Cinematic mobile/reduced-motion, material offline/browser
parity, and several delivery drills declare a **real browser**.

When an external runtime cannot start (Metal SIGSEGV during Blender
`WM_init`, Chrome Crashpad sandbox denial, missing Chromium), the drill is
`BLOCKED_EXTERNAL` with the **exact reason**. The injected artifact and failing
measurement are still written under `failed-attempts/` so a supervisor can
re-run on real hardware.

`scripts/run-repair-corpus.py --force-measure` proves the inject→detect→repair
loop on the live artifact+critic path without claiming Blender/browser success.

**Browser rule:** at most one Playwright browser at a time. Launch, use, close.
Between runs use `scripts/reap-browsers.sh`. There is no
`with-one-browser.sh` in this tree yet; the corpus probe serialises a single
launch/close cycle itself.

## Run

```bash
cd tools/blender-vision-mcp
.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair
.venv/bin/python scripts/run-repair-corpus.py --output artifacts/v2/repair --force-measure
.venv/bin/python -m pytest -q tests/test_v2_repair_corpus.py
```

The matrix columns are: drill id, detector fired, repaired, acceptance passed,
global regression, runtime used, status, measured before/after.

Exit non-zero only when a runnable drill fails detection or acceptance.
`BLOCKED_EXTERNAL` rows do not fail the process.

## Doctrine

- A derived claim is never stronger than its weakest input.
- Detectors are the existing thirteen critics — no new critics, no relaxed
  thresholds, no frozen-budget changes.
- Failed attempts are preserved.
- A Python error is never reported as a hardware blocker; hardware blockers are
  only those actually diagnosed (probe return codes, Crashpad denials, etc.).
