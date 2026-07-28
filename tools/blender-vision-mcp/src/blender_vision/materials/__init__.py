"""Evidence-bound material and appearance profiles."""

from blender_vision.materials.critic import (
    MATERIAL_CRITICS,
    MaterialCriticContext,
    inject_material_failure,
    run_material_critics,
)
from blender_vision.materials.frequency import (
    BAND_ORDER,
    FrequencyBand,
    FrequencyDecomposition,
    decompose_surface,
    lacks_medium_band,
)
from blender_vision.materials.inverse import (
    SurfaceObservation,
    SurfaceRegion,
    infer_materials,
)
from blender_vision.materials.parity import (
    ParityReport,
    ParityTarget,
    compare_images,
    delta_e2000,
    run_parity,
    structural_difference,
)
from blender_vision.materials.store import MaterialStore
from blender_vision.materials.textures import TextureSet, generate_texture_set

__all__ = [
    "BAND_ORDER",
    "MATERIAL_CRITICS",
    "FrequencyBand",
    "FrequencyDecomposition",
    "MaterialCriticContext",
    "MaterialStore",
    "ParityReport",
    "ParityTarget",
    "SurfaceObservation",
    "SurfaceRegion",
    "TextureSet",
    "compare_images",
    "decompose_surface",
    "delta_e2000",
    "generate_texture_set",
    "infer_materials",
    "inject_material_failure",
    "lacks_medium_band",
    "run_material_critics",
    "run_parity",
    "structural_difference",
]
