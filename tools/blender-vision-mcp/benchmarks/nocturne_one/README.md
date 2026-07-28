# NOCTURNE/ONE sealed benchmark

NOCTURNE/ONE is an owned fixed benchmark for reference-to-application and
reference-to-editable-3D capability. The public thresholds live in
`contract.json`; the parametric textual authority lives in
`governed_spec.json`. Both must be committed before any scored builder process
runs.

The oracle-author process runs `oracle_author/generate.py`, which launches
installed Blender with factory startup and auto-execution disabled. It produces
six public renders, two real MP4 motion references, ten textual/contract
documents, a mood board, four evaluator-only holdouts, one evaluator-only mobile
trace, and an editable oracle BLEND. Only `input-packet/` is builder-readable.

The builder runs in a detached worktree under `sandbox-exec`. The OS profile
denies read and write access to both the sealed evaluator output and
`oracle_author/`. A preflight `cat` of a random evaluator canary must fail, and
the completed builder tree is scanned for the same canary. This is filesystem
separation for the recorded process; it is not represented as proof that the
human or model authors have no prior conceptual knowledge of the fictional
product.

## Reproduction sequence

```bash
uv run bvmcp benchmark bootstrap-nocturne-oracle \
  --output /absolute/sealed/nocturne-one

# Run the fixed builder prompt in a detached worktree through
# SealedBuilderRunner, passing only the input packet.

uv run bvmcp benchmark evaluate-nocturne-3d \
  --packet /absolute/sealed/nocturne-one/input-packet \
  --oracle /absolute/sealed/nocturne-one/sealed-evaluator \
  --candidate sandbox/nocturne-one \
  --builder-receipt /absolute/run/sealed-builder.receipt.json \
  --output artifacts/nocturne-one/3d-evaluation

uv run bvmcp benchmark evaluate-nocturne-app \
  --packet /absolute/sealed/nocturne-one/input-packet \
  --candidate sandbox/nocturne-one \
  --builder-receipt /absolute/run/sealed-builder.receipt.json \
  --hidden-mobile-trace \
    /absolute/sealed/nocturne-one/sealed-evaluator/mobile/hidden-interaction-trace.json \
  --output artifacts/nocturne-one/app-evaluation
```

The 3D evaluator reopens the BLEND, reimports both GLBs, fixes all ten public and
hidden cameras without nudge, scores alpha silhouettes, checks dimensions and
part placement, compares mesh signatures against oracle-source substitution,
and audits hierarchy, materials, texture paths, UVs, topology, normals, LOD
identity, and exploded animation.

The application evaluator verifies the exact builder receipt, copies a fresh
source tree, installs from the lockfile, builds/tests it, migrates twice, rolls
back and reapplies, exercises real authenticated/idempotent SQLite-backed API
requests, runs every route and state in installed Chromium, executes the hidden
mobile trace, scans accessibility, measures the real lazy-loaded 3D path, and
runs the fixed five-minute memory loop.

No unsuccessful attempt may be deleted. Candidate source is sealed only through
`bvmcp benchmark seal-nocturne-candidate`, which hashes every source file and
binds every retained attempt receipt.
