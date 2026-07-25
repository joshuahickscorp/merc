# RTX 5090 Founders Edition benchmark

`benchmark.json` governs the distinct 137 × 40 × 304 mm cooler-body reconstruction target. Its
dimensional gate excludes the I/O bracket, PCIe edge connector, and installation clearance while
tracking those parts as required technical features. Private reference images are never packaged.

Bootstrap an existing candidate without accepting it:

```bash
uv run bvmcp benchmark bootstrap-rtx-5090-fe \
  --project "$PROJECT" --scene "$CANDIDATE_BLEND" \
  --repository-root "$REPOSITORY_ROOT" --reference-root "$REFERENCE_ROOT" \
  --source-revision "$SOURCE_REVISION" \
  --source-artifact "$BUILDER_SCRIPT"
```

The command imports immutable source and reference evidence, audits Blender geometry, measures the
cooler-body envelope, and creates unreviewed feature candidates. A historical candidate remains a
baseline only; it is not silently promoted as the required strict reconstruction revision.
