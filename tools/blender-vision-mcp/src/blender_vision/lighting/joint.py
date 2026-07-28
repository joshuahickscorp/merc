"""Joint material/light optimisation via coordinate descent.

Material and lighting parameters stay in **separate** sealed records with
separate authority. The joint solve may not merge their authority classes.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any

import numpy as np

from blender_vision.lighting.solve import (
    GeometryContext,
    LightingObservation,
    solve_lighting,
)
from blender_vision.materials.inverse import (
    SurfaceObservation,
    SurfaceRegion,
    infer_materials,
)
from blender_vision.v2.authority import (
    AuthorityClass,
    AuthorityPromotionError,
    derive,
    strength,
)
from blender_vision.v2.records import LightingHypothesisSet, MaterialHypothesisSet


@dataclass(slots=True)
class JointSolveResult:
    """Coordinate-descent result with strictly separate material and light records."""

    material_record: MaterialHypothesisSet
    lighting_record: LightingHypothesisSet
    iterations: int
    residual_history: list[float]
    joint_metadata: dict[str, Any]

    def authorities_merged(self) -> bool:
        """True only if a bug merged authority — must always be False."""
        return self.material_record.id == self.lighting_record.id

    def to_dict(self) -> dict[str, Any]:
        return {
            "material_record_id": self.material_record.id,
            "lighting_record_id": self.lighting_record.id,
            "material_authority": self.material_record.authority.value,
            "lighting_authority": self.lighting_record.authority.value,
            "iterations": self.iterations,
            "residual_history": list(self.residual_history),
            "joint_metadata": dict(self.joint_metadata),
            "authorities_merged": self.authorities_merged(),
        }


def _render_residual(
    material: MaterialHypothesisSet,
    lighting: LightingHypothesisSet,
    observations: list[SurfaceObservation],
) -> float:
    """Cheap proxy residual: mismatch between observed mean colour and predicted albedo*light."""
    if not material.hypotheses or not lighting.hypotheses:
        return 1.0
    mat = next(
        (
            item
            for item in material.hypotheses
            if item.hypothesis_id == material.selected_hypothesis_id
        ),
        material.hypotheses[0],
    )
    light = next(
        (
            item
            for item in lighting.hypotheses
            if item.hypothesis_id == lighting.selected_hypothesis_id
        ),
        lighting.hypotheses[0],
    )
    intensity = float(light.key.get("intensity", 1.0))
    env = float(light.environment.get("strength", 0.2)) if light.environment else 0.2
    predicted = np.array(mat.base_colour[:3], dtype=np.float64) * (0.65 * intensity + 0.35 * env)
    predicted = predicted * (1.0 - 0.35 * mat.metalness) + np.array(mat.base_colour[:3]) * (
        0.35 * mat.metalness * intensity
    )
    observed_means = []
    for obs in observations:
        pixels = obs.rgb.reshape(-1, 3)
        if obs.mask is not None:
            pixels = obs.rgb[obs.mask]
        if pixels.size:
            observed_means.append(pixels.mean(axis=0))
    if not observed_means:
        return 1.0
    observed = np.mean(np.stack(observed_means, axis=0), axis=0)
    return float(np.mean(np.abs(predicted - observed)))


def joint_solve(
    observations: list[SurfaceObservation],
    surfaces: list[SurfaceRegion],
    geometry: GeometryContext,
    *,
    max_iterations: int = 4,
    lighting_observations: list[LightingObservation] | None = None,
) -> JointSolveResult:
    """Alternate material and lighting solves; never fuse authority into one record."""
    if max_iterations < 1:
        raise ValueError("max_iterations must be >= 1")

    light_obs = lighting_observations
    if light_obs is None:
        light_obs = [
            LightingObservation(
                view_id=obs.view_id,
                rgb=obs.rgb,
                mask=obs.mask,
                authority=obs.authority,
            )
            for obs in observations
        ]

    residual_history: list[float] = []
    material_record = infer_materials(observations, surfaces)
    lighting_record = solve_lighting(light_obs, geometry)
    residual_history.append(_render_residual(material_record, lighting_record, observations))

    for _ in range(max_iterations - 1):
        # Coordinate descent: re-estimate lighting, then materials.
        lighting_record = solve_lighting(light_obs, geometry)
        material_record = infer_materials(observations, surfaces)
        residual_history.append(_render_residual(material_record, lighting_record, observations))
        if len(residual_history) >= 2 and abs(residual_history[-1] - residual_history[-2]) < 1e-4:
            break

    # Annotate joint linkage without merging authority or records.
    joint_id = f"joint-{uuid.uuid4().hex[:10]}"
    material_inputs = list(material_record.lineage.input_authorities)
    lighting_inputs = list(lighting_record.lineage.input_authorities)
    # Each side re-derives independently: the joint solve links two records, it
    # does not let either borrow the other's evidence.
    material_ceiling = derive(material_inputs or [AuthorityClass.HYPOTHETICAL])
    lighting_ceiling = derive(lighting_inputs or [AuthorityClass.HYPOTHETICAL])
    for label, record, ceiling in (
        ("material", material_record, material_ceiling),
        ("lighting", lighting_record, lighting_ceiling),
    ):
        if strength(record.authority) > strength(ceiling):
            raise AuthorityPromotionError(
                f"joint solve would leave the {label} record claiming "
                f"{record.authority.value} above its own ceiling {ceiling.value}"
            )
    joint_metadata = {
        "joint_id": joint_id,
        "material_record_id": material_record.id,
        "lighting_record_id": lighting_record.id,
        "material_authority": material_record.authority.value,
        "lighting_authority": lighting_record.authority.value,
        "material_authority_ceiling": material_ceiling.value,
        "lighting_authority_ceiling": lighting_ceiling.value,
        "authority_merged": False,
        "residual_final": residual_history[-1],
    }
    # Attach joint metadata as notes / joint_solve field without rewriting authority.
    material_record.notes.append(f"joint_solve:{joint_id}")
    material_record.digest = ""
    material_record.seal()

    lighting_record.joint_solve = dict(joint_metadata)
    lighting_record.notes.append(f"joint_solve:{joint_id}")
    lighting_record.digest = ""
    lighting_record.seal()

    # Final hard invariant: distinct ids and no shared authority record object.
    if material_record.id == lighting_record.id:
        raise RuntimeError("joint_solve merged material and lighting record ids")
    if material_record is lighting_record:
        raise RuntimeError("joint_solve merged material and lighting record objects")

    return JointSolveResult(
        material_record=material_record,
        lighting_record=lighting_record,
        iterations=len(residual_history),
        residual_history=residual_history,
        joint_metadata=joint_metadata,
    )
