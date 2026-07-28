# Camera, lighting, and material authority

VisionMCP separates camera, illumination, material, and geometry claims. RGB similarity cannot
silently establish geometry, and an evaluator may not move a camera to improve a render.

## Immutable camera contract

An acceptance camera contains:

- image identity and resolution;
- focal intrinsics and principal point;
- a complete `world_from_camera` transform;
- sensor and pixel-aspect model;
- crop and clipping state;
- distortion model and render policy;
- coordinate-frame semantics;
- solve method, registration class, and evidence bindings;
- an immutable SHA-256 over the complete state.

Nonzero distortion requires an applied distortion pass or an explicitly derived undistorted input.
Rendering records the supplied camera hash, exact matrix, intrinsics, and
`immutable_exact_camera_state` framing authority. `AppearanceAuthority` compares those values before
pixels. A render with a changed matrix, focal state, or principal point fails even if its pixels
look better.

## Material and illumination contract

Material profiles can represent:

- material class;
- base and emission color;
- roughness, metallic, anisotropy, coat, transmission, alpha, and specular IOR level;
- IOR and emission strength;
- thin-wall, volume, normal-detail, procedural-texture, and reflectance constraints;
- color calibration, lighting hypothesis, confidence, and uncertainty.

Reflective and transmissive materials require multi-light evidence or reflective-region masks for
approval. Translucent, transparent, and emissive materials additionally require a reported
illumination estimate. These profiles remain appearance-only and cannot promote geometry.

Blender scene inspection records Principled inputs, material class, node/image structure, explicit
light type/energy/color/pose/size, and world environment color/strength or environment images. Each
record receives a structural digest.

## Fixed benchmark

`appearance-authority-v1` is an owned synthetic corpus with anodized metal, frosted translucent
glass, a separate emissive disk, textile, three explicit lights, three public cameras, and one
held-out camera. Independent Blender processes render beauty and ±2 EV brackets from identical
immutable camera states.

The fixed acceptance manifest checks:

- camera hash, matrix, intrinsics, fixed lighting, and zero output dither;
- independent-render channel MAE, RMSE, p95, maximum error, alpha error, and highlight coverage;
- material classes and Principled parameters;
- separate editable material-bearing objects;
- explicit lights and environment hypothesis;
- missing textures;
- rejection of camera nudge, material substitution, and lighting substitution controls.

Run it into a new directory:

```bash
uv run bvmcp benchmark bootstrap-appearance \
  --output artifacts/appearance-run
```

A pass proves the authority machinery, deterministic real-Blender execution, and its declared
fixture. It does not prove arbitrary-photo material identification, HDR recovery when no HDR was
supplied, or universal appearance reconstruction.
