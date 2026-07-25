# VisionMCP 100+ Baseline

## Authority

- Goal branch: `goal/visionmcp-100plus-sandbox`
- Isolated worktree:
  `/Users/scammermike/Downloads/visionmcp-authority-worktrees/visionmcp-100plus-sandbox`
- Expected and observed starting SHA:
  `e0f8df6708cb3fdebec9298f33804e5d3def9260`
- Starting authority branch: `feature/blender-vision-mcp`
- Baseline captured: 2026-07-25 UTC
- Machine: Apple M3 Ultra, 60-core integrated GPU, 96 GiB memory, macOS 27.0

The original authority worktree remained clean. The separate ComputExchange production worktree was
already two commits ahead and contained extensive modified and untracked files before this goal.
Its exact starting status is preserved in
`artifacts/100-plus/baseline/raw/worktree-preservation-boundary.log`; this goal does not modify it.

## Reproduction results

| Gate | Result | Runtime | Peak resident memory |
|---|---:|---:|---:|
| Frozen dependency sync | PASS | 0.65 s | not measured |
| Fast Python suite | 328 passed, 7 gated skips | 65.85 s pytest / 66.99 s wall | 472,170,496 bytes |
| Real browser + Blender, clean isolated checkout | **FAIL**: 1 failed, 12 passed | 158.19 s pytest | 734,396,416 bytes |
| Real browser + Blender, digest-pinned external Mac Studio fixture mounted | 13 passed | 220.10 s pytest / 220.56 s wall | 873,545,728 bytes |
| Ruff | PASS | not separately timed | not measured |
| Wheel verification | PASS, 225 members | not separately timed | not measured |
| V1 release verifier | PASS | not separately timed | not measured |
| Blender extension validation | PASS | not separately timed | not measured |
| Blender extension build | PASS after creating the output directory | not separately timed | not measured |

Raw logs, including negative results, are under `artifacts/100-plus/baseline/raw/`.

## Baseline defects found

### External Mac Studio fixture was implicit

`tests/test_blender_integration.py::test_mac_studio_vertical_slice` assumes
`models/mac_studio/final_packed.blend` exists three directory levels above the test. That file is
ignored and is not present in a new Git worktree. The initially reported 13-pass real-runtime result
therefore depended on an undeclared symlink in the original authority checkout.

The result was reproduced only after mounting the existing owned fixture read-only with SHA-256:

`22ea2562cc92d44b2df084f0009b3faca6ab37f6ff81e21e55136ac6871e6dae`

The corresponding tracked reference image has SHA-256:

`353fe0e05430d681d08ccd765315da5ffcc49288da3b69028d6e15694976f0a3`

This dependency must become an explicit external benchmark contract. Adapter or test presence does
not make the external fixture portable.

### In-tree benchmark artifacts contaminated an sdist experiment

Two builds made from a pristine detached worktree produced byte-identical wheels and source
archives:

- wheel:
  `77b2c951621166bd1ddcbb7a06399a63e4fb280ea32c0ec1669a434dd8eebc1a`
- source archive:
  `7568d4df25affedefffc947c2fee418bf47b5a54b4c8d58ab943d9bc190796c8`

An earlier experiment emitted benchmark evidence under the source directory. Hatch included that
untracked evidence in the source archive, so sequential archives differed as the evidence directory
changed. The differing archives and analysis were preserved. The final release pipeline needs an
explicit packaging exclusion for benchmark evidence and sandbox runtime artifacts.

### Blender extension output directory is caller-created

Extension validation passed. The first extension build failed because Blender does not create a
missing `--output-dir`; after the directory was created, the build passed. Reproduction scripts must
create and bind their output directories before invoking Blender.

## Runtime and lock inventory

- Python used by the project: 3.12.13
- `uv`: 0.11.7
- Blender: 4.2.1 LTS
- Chrome: 150.0.7871.183
- Playwright package: 1.61.0
- Node: 20.17.0
- ffmpeg/ffprobe: 8.1.1
- COLMAP: 4.0.4
- `uv.lock` SHA-256:
  `34d11668ddb3f00c2346a62b1ea9d99162a17dcef1d75a982621903d6ee16b2d`
- `pyproject.toml` SHA-256:
  `612de967d5fac63b517ee95894161f821a859a32e42c0360695144553d158d52`

The environment did not contain a Playwright-managed additional browser. Safari is installed, but
installed Safari is not equivalent to a Playwright WebKit runtime. Firefox application bundle
metadata was absent, and invoking the expected Firefox CLI path hung; the failed probe was stopped
and preserved.

## Public surface inventory

- MCP tools: 242
- MCP resource templates: 28
- MCP prompts: 7
- registered geometry/camera backends: 7
- registered schemas: 34

Exact names and digests are in `mcp-inventory.json`,
`schema-artifact-store-digests.log`, and `artifact-store-source-digests.log`.

## Baseline capability grade

The supplied grading report is retained without inflation:

| Declared capability | Baseline score | Baseline conclusion |
|---|---:|---|
| Frontend reconstruction/repair from visual and runtime references | 84/100 | strong controlled capability |
| Complete production application from visual and textual references | 71/100 | incomplete full-stack authority |
| Calibrated hard-surface multiview reconstruction | 82/100 | strong controlled capability |
| Arbitrary photo/video and text to editable 3D | 67/100 | below production-complete |
| Browser 3D scene to editable Blender/GLB | 91/100 | strongest proven 3D lane |
| Evidence, governance, reproducibility, verification | 94/100 | mature but missing adversarial 100+ corpus |
| Universal autonomous app and 3D builder | 69/100 | not yet achieved |

Phase 1 preserves every individual facet from the supplied report in the executable 0–110 score
authority. No baseline number is converted to 100 merely because its implementation exists.

## Exact baseline commands

Run from `tools/blender-vision-mcp`:

```bash
uv sync --frozen --extra dev
uv run ruff check .
/usr/bin/time -l uv run pytest
BVMCP_RUN_BROWSER_TESTS=1 BVMCP_RUN_BLENDER_TESTS=1 \
  /usr/bin/time -l uv run pytest \
  tests/test_browser_perception.py \
  tests/test_web_experience.py \
  tests/test_graphics_perception.py \
  tests/test_blender_integration.py \
  tests/test_calibration_benchmark.py
uv build
uv run python scripts/verify-wheel.py dist
uv run python scripts/verify-v1.py \
  --wheel dist/blender_vision_mcp-0.1.0-py3-none-any.whl
/Applications/Blender.app/Contents/MacOS/Blender \
  --command extension validate blender_extension
mkdir -p dist/blender-extension
/Applications/Blender.app/Contents/MacOS/Blender \
  --command extension build \
  --source-dir blender_extension \
  --output-dir dist/blender-extension
```

The real-runtime command requires the external Mac Studio fixture contract until that defect is
repaired. The standalone calibration and fresh parametric seed tests do not require that fixture.
