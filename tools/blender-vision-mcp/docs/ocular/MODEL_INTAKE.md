# Model intake report (Ocular)

**Policy: download nothing.** Every learned family the Bible names is registered
in `blender_vision.ocular.registry` with `ReviewState.REVIEW_PENDING`. A
registry entry without a local checkpoint reports `BackendState.DOWNLOAD_REQUIRED`
(or `UNAVAILABLE`) and can never be selected for a physical claim.

## Selection law

```text
selectable_for_physical ⇔
    review_state == ACCEPTED
    ∧ checkpoint_path is a real file
    ∧ backend_state == AVAILABLE
```

`ModelRegistry.select_for_physical(id)` raises `PhysicalModelSelectionError`
otherwise. Fallbacks may emit `DIAGNOSTIC_ONLY` or `CANDIDATE_ONLY` results —
never a physical PASS.

## Registered families (what *would* be adopted)

| model_id | Family | Version | License | Authority ceiling | Replacement path |
|----------|--------|---------|---------|-------------------|------------------|
| `dino-v2-vitb14` | dense_features | dinov2_vitb14@2023-04 | Apache-2.0 | MODEL_DERIVED | classical histogram + flow |
| `sam2-hiera-large` | promptable_segmentation | sam2_hiera_large@2024-07 | Apache-2.0 | MODEL_DERIVED | ocular.segment classical |
| `sam3-class-placeholder` | promptable_segmentation | sam3-unreleased-intake | unreviewed | MODEL_DERIVED | contour_parts |
| `vggt-geometry` | geometry | vggt@intake | unreviewed | MODEL_DERIVED | COLMAP sparse + visual hull |
| `moge-geometry` | geometry | moge@intake | unreviewed | MODEL_DERIVED | multi-view depth fusion |
| `mast3r-geometry` | geometry | mast3r@intake | CC-BY-NC-4.0 | MODEL_DERIVED | COLMAP matching |
| `cotracker3-point` | point_tracking | cotracker3@intake | CC-BY-NC-4.0 | MODEL_DERIVED | ocular.track Kalman |
| `gaussian-splatting` | radiance | 3dgs@intake | unreviewed | MODEL_DERIVED | mesh + baked textures |
| `vjepa-prediction` | prediction | vjepa@intake | unreviewed | MODEL_DERIVED | constant-velocity + permanence |
| `classical-segment-track` | classical | ocular-classical-1 | Apache-2.0 | SENSOR_DERIVED | (this is the live path) |

Checkpoint digests are placeholders (`sha256:pending-local-checkpoint-not-present`)
until a human pins a real file after license review.

## Privacy

Descriptors, masks, dense matches, and video embeddings can reconstruct or
reveal private scene content. No entry may leave `REVIEW_PENDING` without an
explicit privacy note and a rights state.

## Failure profiles (intake)

- **DINO-class:** domain shift on synthetic CAD; specular collapse; no metric scale.
- **SAM-class:** prompt ambiguity on similar objects; temporal flicker under occlusion.
- **Geometry (VGGT/MoGe/MASt3R):** scale ambiguity; non-commercial licenses (MASt3R).
- **CoTracker-class:** long-horizon occlusion drift; non-commercial license.
- **Gaussian/radiance:** poor editability; external metric scale; VRAM.
- **V-JEPA-class:** latent not geometric; never physical authority alone.

## Classical path (what runs today)

`ocular.segment` + `ocular.track` use OpenCV / NumPy / scikit-image only. They
do not consult this registry for execution. The classical entry exists so intake
coverage is complete and so a future reviewer can accept a learned replacement
without inventing a new process.

## How to promote an entry (human process)

1. Obtain weights offline under the declared license.
2. Compute `sha256` of the checkpoint; set `checkpoint_digest` and `checkpoint_path`.
3. Record hardware + dependency pin that actually ran.
4. Run the named benchmark; attach a sensitivity receipt for any gate metric.
5. Set `review_state = ACCEPTED` with a named reviewer (not `system` / `auto`).
6. Only then may `select_for_physical` succeed.

Until then, every physical claim that needs a learned model must
`attest_blocked(...)` with the exact reason.
