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
bounded causal degenerate repair, and exact GLB structural validation. VGGT remains checkpoint
governed. The generic external-candidate contract accepts operator-supplied single-image,
multi-image, text-conditioned, organic, retopology, UV/PBR/bake, rig/animation, and collision
backends only after license and checkpoint review; it does not claim that any such backend is
installed.

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
