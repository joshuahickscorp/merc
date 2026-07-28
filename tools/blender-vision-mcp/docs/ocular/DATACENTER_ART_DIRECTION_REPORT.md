# Data-centre flagship — art direction close (Phase P)

## Defect

The second aisle held **8 of 464 instances**. Beats `05 TURN`, `06 VERIFY`,
`07 RECEIPT` and `08 ACCESS` played against a bare corridor. A screenshot at
scroll 0.80, camera `[1.8, 13.3, 1.5]`, showed a flat wall.

A high global instance count must never again stand in for per-beat coverage.

## What changed

### Grammar (`procedural/grammar.py`)

Population continues across the junction into the second aisle:

- Eight equipped racks per flank (N/S) along +X past the containment door
- Floor tiles, cable tray, cable bundle on the hot corridor
- Terminal wall treatment: paired wall ribs, restrained status matrix (4×2)
- Second cooling face opposite the existing unit
- Terminal column for depth

Reduced scenes (`aisle_length_m < 10`) leave the second aisle unpopulated so
main-aisle unit tests stay exact. Full flagship defaults populate it.

### Beat coverage (`ocular/beat_coverage.py`)

`BeatCoverageReceipt` (Bible 25.15) declares and **measures** per beat:

- Frustum cull at the beat camera (Blender Z-up frame, explicit)
- Pixel statistics from the real render (non-background fraction, depth spread,
  luminance histogram, light key/fill proxy)
- Text-safe contrast from pixels via `evaluate_text_safe` — never assumed
- Confirmed visible racks/drawers: frustum geometry with an empty render fails

Declared minimums are per-beat. Global scene counts are not a gate input.

### Camera framing (`cinematic/path.py`)

Path architecture unchanged (threshold → turn → terminal). Framing tweaks:

- Control points sit in the clear volume of the now-populated second aisle
- Solids updated for second-aisle rack rows
- Focus targets include second-aisle racks and verification terminal
- Light hierarchy states key/fill through terminal verify

### Shell / mobile

Terminal-wall text zone gets a soft scrim so RECEIPT/ACCESS copy keeps contrast
against the wall. Native scroll only; no hijacking.

## Instance distribution (full flagship, measured)

| Region | Instances | Racks | Drawers |
| --- | ---: | ---: | ---: |
| Main aisle | 458 | 24 | 360 |
| Second aisle | 313 | 16 | 240 |
| **Total** | **771** | **40** | **600** |

Unique mesh count: **26** (bounded; was 20 before status/tile/cooling param variants).

## Per-beat coverage table (diagnostic run on this host)

Blender 4.2.1 headless is **BLOCKED** on this session (Metal SIGSEGV during
`WM_init` / `metal_is_supported`). Frames below are CPU diagnostic AABB
rasters (`ExecutionClass.DIAGNOSTIC_ONLY`), not EEVEE. They still fail the
physical gate, and they still enforce per-beat minimums — a high global count
does not pass an empty beat.

| beat | racks | drawers | non_bg | depth | contrast | pass |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| 00 THRESHOLD | 28 | 424 | 0.247 | 12.17 | 19.37 | PASS |
| 01 CAPACITY | 40 | 600 | 0.564 | 12.18 | 19.78 | PASS |
| 02 INFERENCE | 40 | 590 | 0.817 | 11.90 | 19.43 | PASS |
| 03 DISPATCH | 32 | 435 | 0.696 | 9.50 | 19.13 | PASS |
| 04 EXECUTION | 14 | 158 | 0.634 | 7.09 | 19.53 | PASS |
| 05 TURN | 12 | 135 | 0.652 | 3.22 | 19.46 | PASS |
| 06 VERIFY | 16 | 236 | 0.720 | 3.92 | 19.50 | PASS |
| 07 RECEIPT | 12 | 142 | 0.673 | 2.43 | 19.13 | PASS |
| 08 ACCESS | 1 | 10 | 0.213 | 0.98 | 19.48 | PASS |

Budgets (frozen, unchanged): shell 249 432 B / mobile 249 432 B / detail
chapter-gated 3 073 572 B — all within limits.

## How to verify

```bash
cd tools/blender-vision-mcp
.venv/bin/python -m pytest -q tests/test_ocular_beats.py
.venv/bin/python scripts/run-beat-coverage.py --output artifacts/ocular/beats
```

The runner:

1. Builds the flagship scene and emits through real Blender (attested)
2. Renders each of the nine beat cameras
3. Seals a `BeatCoverageReceipt` per beat
4. Prints the per-beat table and fails on any minimum breach
5. Checks shell/detail/mobile bytes against frozen budgets
6. Runs environment / editorial / lighting critics on the renders
7. Verifies the sandbox in one Playwright browser via `with-one-browser.sh`

## Laws held

- No fallback emits a physical PASS; Blender absence is `BLOCKED`
- Failures classified from observed output, not invented hardware blame
- Coordinate frame is Blender Z-up on every receipt
- `slots=True` records call `V2Record.method(self)`, never zero-arg `super()`
- Browsers serialised through `with-one-browser.sh`
