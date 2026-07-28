# Release and standalone verification

The `tools/blender-vision-mcp` directory is a complete Python project and can be extracted without
the parent ComputExchange repository. The Mac Studio bootstrap is an optional monorepo integration;
the CC0 calibration benchmark is the standalone acceptance fixture.

## Build and verify

```bash
uv sync --frozen --extra dev
uv run ruff check .
uv run pytest
uv build
uv run python scripts/verify-wheel.py dist
uv run python scripts/verify-v1.py --wheel dist/blender_vision_mcp-0.1.0-py3-none-any.whl
```

For a release candidate, build twice from the same source tree and compare the SHA-256 hashes of
the wheel and source archive. Install the wheel into a new environment, then run:

```bash
bvmcp doctor
bvmcp capabilities
bvmcp project create clean-install-smoke --root /tmp/bvmcp-clean-install
blender-vision --help
```

With Blender installed, run both real integration paths:

```bash
BVMCP_RUN_BLENDER_TESTS=1 uv run pytest tests/test_blender_integration.py
BVMCP_RUN_BLENDER_TESTS=1 uv run pytest tests/test_calibration_benchmark.py
```

The Mac Studio vertical slice is an optional owned external benchmark rather than a distributable
fixture. Set `BVMCP_MAC_STUDIO_SCENE` to a BLEND file with SHA-256
`22ea2562cc92d44b2df084f0009b3faca6ab37f6ff81e21e55136ac6871e6dae` to execute that slice.
Without it, the test reports an exact `BLOCKED_EXTERNAL` skip while the standalone Blender
integration and calibration tests continue to run.

The calibration test must report L3, six approved metric cameras, complete comparison coverage,
silhouette IoU at least 0.95, all five calibration gates, two identical GLB hashes, and a valid JSON
receipt with a Markdown rendering.

## Public-package checklist

- Safe mode is true without environment overrides; arbitrary Blender or shell execution is absent.
- Installation and `doctor` perform no network or model downloads.
- The wheel contains the standalone Blender worker, schemas, review UI, model-license registry,
  security documents, and the CC0 benchmark, but no `.blend`, database, checkpoint, or private
  reference artifact.
- The MCP server discovers every documented high-level tool, resource, workflow, and prompt.
- Linux, macOS, and Windows run the fast suite on Python 3.11 and 3.13 in CI.
- Blender extension validation succeeds on the installed Blender version.
- The extension build output directory is created before passing it to Blender.
- The source archive can build the same wheel without the parent repository.
- `MODEL_LICENSES.json`, `SECURITY.md`, and `docs/SECURITY_REVIEW.md` have been reviewed for the
  exact release date.
- Accepted receipts contain source/runtime revision, hardware/platform, reference and measurement
  hashes, perspective grids, capture/tier decisions, approved cameras, component/constraint records,
  render configuration and pass hashes, residuals, coverage, human approvals, and authoritative
  `.blend` and GLB hashes.
