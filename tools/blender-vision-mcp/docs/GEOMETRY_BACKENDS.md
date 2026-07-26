# Governed geometry backends

`BackendRegistry` is the discovery authority for classical, learned, parametric, repair, and
validation lanes. Every entry now exposes:

- implementation version and revision;
- license, commercial-use state, redistribution policy, and checkpoint digest when applicable;
- runtime state and hardware requirements;
- operations, input modalities, outputs, and input bounds;
- output coordinate frame and metric-scale authority;
- known limitations and confidence semantics.

Registry presence is never execution evidence. `AVAILABLE` means the implementation or required
local executable was discovered. A capability score still requires a source-head-bound runtime
receipt and the facet's fixed external or holdout evidence. `LICENSE_REVIEW_REQUIRED`,
`DOWNLOAD_REQUIRED`, `RESEARCH_ONLY`, and `UNAVAILABLE` cannot be promoted by an adapter or
manifest alone.

Current real lanes include EXIF and heuristic camera initialization, COLMAP classical multiview,
reviewed-mask visual hull, dimension-governed Blender parametric modeling, bounded Blender LOD,
bounded causal degenerate repair, bounded Blender production preparation, and exact GLB structural
validation. VGGT remains checkpoint governed. The generic external-candidate contract accepts
operator-supplied single-image, multi-image, text-conditioned, and organic reconstruction backends
only after license and checkpoint review; it does not claim that any such backend is installed.

## Blender production preparation

`blender-asset-preparation` is a real, locally discovered Blender lane. One transaction accepts
explicit named meshes and can execute:

- bounded decimate-based retopology candidates with polygon and envelope deltas;
- smart-project UV generation with finite coordinate and unit-square checks;
- explicit Principled BSDF material construction;
- real Cycles diffuse-color texture baking into a project-confined PNG packed into the BLEND;
- a two-bone weighted character-lite rig and deterministic pose animation;
- deterministic object animation;
- descending decimated LODs;
- named `UCX_*` convex collision hulls;
- duplicate/degenerate cleanup and normal recalculation with topology before/after facts.

The operation saves an isolated candidate BLEND, exports an embedded-resource GLB, validates that
GLB, reimports it into a fresh Blender scene, and audits both Blender scenes. It does not promote
the source or candidate. Decimation is not represented as hand-authored all-quad topology, and
successful baking does not prove reference-texture fidelity.

```bash
uv run bvmcp blender prepare-asset \
  --project "$PROJECT" \
  --targets preparation-targets.json

uv run bvmcp benchmark bootstrap-asset-preparation \
  --output artifacts/asset-preparation-run
```

The fixed `asset-preparation-v1` benchmark is a six-object synthetic-owned corpus covering
dimensioned hard surface, curved reflective/translucent material, organic form, character-lite
rigging, UV/baking, LOD/collision, and deliberate mesh damage. A pass requires real Blender,
candidate and reimport audits, byte-bound GLB validation, every requested stage receipt, and
source-head-bound output digests.

## GLB structural authority

`GlbValidator` parses GLB 2.0 bytes without executing extensions or fetching external resources. It
checks:

- header, declared length, chunk order, alignment, and JSON decoding;
- embedded buffer and buffer-view bounds;
- accessor types, component types, strides, finite bounds, and byte ranges;
- mesh primitive attributes, indices, material references, and modes;
- node, child, scene, camera, skin, and default-scene references plus graph cycles;
- material texture indices, samplers, embedded image sources, and MIME types;
- animation sampler/accessor/channel/node/path references;
- skin joints and inverse bind matrices;
- required-vs-used extensions;
- caller-required node and mesh names.

The result binds the file SHA-256 and exact metrics. It is structural evidence, not a material
render, topology-quality, animation-quality, or visual-equivalence claim.

```bash
bvmcp glb validate candidate.glb \
  --required-node ProductRoot \
  --required-mesh ProductMesh
```

GLB input paths must be regular files rather than symlinks, and validation is bounded to 512 MiB by
default. Embedded data images are accepted; external buffer and image URIs are rejected for the
sealed/offline asset contract.
