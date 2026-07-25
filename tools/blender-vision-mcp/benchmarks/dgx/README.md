# DGX Spark benchmark

`benchmark.json` governs the 150 × 150 × 50.5 mm DGX Spark reconstruction target. It identifies
the body-only dimensional envelope, required views, feature groups, material regions, and known
acceptance blockers. Private reference images are never packaged.

Bootstrap an existing candidate without accepting it:

```bash
uv run bvmcp benchmark bootstrap-dgx-spark \
  --project "$PROJECT" --scene "$CANDIDATE_BLEND" \
  --repository-root "$REPOSITORY_ROOT" --reference-root "$REFERENCE_ROOT" \
  --source-revision "$SOURCE_REVISION" \
  --source-artifact "$BUILDER_SCRIPT"
```

The command imports immutable source and reference evidence, audits Blender geometry, checks the
manufacturer envelope, and creates unreviewed feature candidates. It never grants camera, feature,
material, repair, or final L3 acceptance.
