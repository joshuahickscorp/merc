"""Quantify what is still unknown for a reconstruction target."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

import numpy as np

from blender_vision.v2.authority import AuthorityClass, strength


class UncertaintyKind(StrEnum):
    SURFACE_COVERAGE = "surface_coverage"
    SCALE_AUTHORITY = "scale_authority"
    MATERIAL_CONFIDENCE = "material_confidence"
    HYPOTHESIS_DISAGREEMENT = "hypothesis_disagreement"
    LIGHTING = "lighting"
    CALIBRATION = "calibration"
    LENS_METADATA = "lens_metadata"


@dataclass(slots=True)
class SurfaceCell:
    region: str
    area_m2: float = 1.0
    covered: bool = False
    incidence_angle_deg: float | None = None
    resolution_px: int = 0
    occlusion_fraction: float = 1.0
    view_ids: list[str] = field(default_factory=list)
    candidate_predictions: list[float] = field(default_factory=list)

    @property
    def uncovered_area_m2(self) -> float:
        if self.covered and self.occlusion_fraction <= 0.5 and self.resolution_px >= 512:
            return 0.0
        return float(self.area_m2)


@dataclass(slots=True)
class PerceptionTarget:
    """Synthetic or measured target for next-best-view planning."""

    target_id: str
    cells: list[SurfaceCell] = field(default_factory=list)
    scale_authority: AuthorityClass = AuthorityClass.UNRESOLVED
    material_confidences: list[float] = field(default_factory=list)
    portfolio_predictions: dict[str, list[float]] = field(default_factory=dict)
    has_scale_reference: bool = False
    has_diffuse_light_view: bool = False
    has_grazing_light_view: bool = False
    has_lens_metadata: bool = False
    has_calibration_target: bool = False
    gates_satisfied: bool = False
    user_declined: bool = False
    existing_view_signatures: set[str] = field(default_factory=set)

    def total_area(self) -> float:
        return float(sum(cell.area_m2 for cell in self.cells)) or 1.0

    def uncovered_area(self) -> float:
        return float(sum(cell.uncovered_area_m2 for cell in self.cells))

    def uncovered_fraction(self) -> float:
        return self.uncovered_area() / self.total_area()

    def mark_covered(self, regions: list[str], *, view_id: str, resolution_px: int = 1600) -> None:
        for cell in self.cells:
            if cell.region in regions:
                cell.covered = True
                cell.occlusion_fraction = 0.0
                cell.resolution_px = resolution_px
                cell.incidence_angle_deg = 25.0
                if view_id not in cell.view_ids:
                    cell.view_ids.append(view_id)
                # Observing reduces candidate disagreement on that surface.
                if cell.candidate_predictions:
                    mean = float(np.mean(cell.candidate_predictions))
                    cell.candidate_predictions = [mean] * len(cell.candidate_predictions)
        self.existing_view_signatures.add(view_id)


@dataclass(slots=True)
class UncertaintyReport:
    target_id: str
    total: float
    components: dict[str, float]
    uncovered_surface_fraction: float
    scale_authority: str
    material_confidence_spread: float
    hypothesis_disagreement: float
    details: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "target_id": self.target_id,
            "total": self.total,
            "components": dict(self.components),
            "uncovered_surface_fraction": self.uncovered_surface_fraction,
            "scale_authority": self.scale_authority,
            "material_confidence_spread": self.material_confidence_spread,
            "hypothesis_disagreement": self.hypothesis_disagreement,
            "details": dict(self.details),
        }


def material_confidence_spread(confidences: list[float]) -> float:
    if not confidences:
        return 1.0
    arr = np.asarray(confidences, dtype=np.float64)
    # Spread: 1 - max confidence, plus variance among hypotheses.
    return float(min(1.0, (1.0 - float(arr.max())) + float(arr.var())))


def hypothesis_disagreement(portfolio: dict[str, list[float]] | list[SurfaceCell]) -> float:
    """Mean variance of candidate predictions (portfolio or per-cell)."""
    if isinstance(portfolio, dict):
        variances = []
        for values in portfolio.values():
            if len(values) >= 2:
                variances.append(float(np.var(np.asarray(values, dtype=np.float64))))
        return float(np.mean(variances)) if variances else 0.0
    variances = []
    for cell in portfolio:
        if len(cell.candidate_predictions) >= 2:
            preds = np.asarray(cell.candidate_predictions, dtype=np.float64)
            variances.append(float(np.var(preds)))
    return float(np.mean(variances)) if variances else 0.0


def scale_authority_uncertainty(authority: AuthorityClass | str) -> float:
    """1.0 when unresolved; 0.0 when measured/manufacturer/human-reviewed."""
    auth = AuthorityClass(authority)
    if auth in {
        AuthorityClass.MEASURED,
        AuthorityClass.MANUFACTURER_SPEC,
        AuthorityClass.HUMAN_REVIEWED,
    }:
        return 0.0
    if auth is AuthorityClass.OBSERVED:
        return 0.15
    if strength(auth) >= strength(AuthorityClass.SENSOR_DERIVED):
        return 0.35
    if auth is AuthorityClass.INFERRED:
        return 0.7
    return 1.0


def quantify_uncertainty(target: PerceptionTarget) -> UncertaintyReport:
    uncovered = target.uncovered_fraction()
    scale_u = scale_authority_uncertainty(target.scale_authority)
    if not target.has_scale_reference and scale_u < 1.0:
        # Without a scale reference, scale cannot be stronger than unresolved in practice.
        scale_u = max(scale_u, 0.85)
    mat_spread = material_confidence_spread(target.material_confidences)
    disagree = hypothesis_disagreement(target.cells) or hypothesis_disagreement(
        target.portfolio_predictions
    )
    lighting_u = 0.0
    if not target.has_diffuse_light_view:
        lighting_u += 0.35
    if not target.has_grazing_light_view:
        lighting_u += 0.35
    calib_u = 0.0 if target.has_calibration_target else 0.4
    lens_u = 0.0 if target.has_lens_metadata else 0.25

    components = {
        UncertaintyKind.SURFACE_COVERAGE.value: float(uncovered),
        UncertaintyKind.SCALE_AUTHORITY.value: float(scale_u),
        UncertaintyKind.MATERIAL_CONFIDENCE.value: float(mat_spread),
        UncertaintyKind.HYPOTHESIS_DISAGREEMENT.value: float(min(1.0, disagree)),
        UncertaintyKind.LIGHTING.value: float(min(1.0, lighting_u)),
        UncertaintyKind.CALIBRATION.value: float(calib_u),
        UncertaintyKind.LENS_METADATA.value: float(lens_u),
    }
    # Weighted total in [0, 1].
    weights = {
        UncertaintyKind.SURFACE_COVERAGE.value: 0.35,
        UncertaintyKind.SCALE_AUTHORITY.value: 0.15,
        UncertaintyKind.MATERIAL_CONFIDENCE.value: 0.12,
        UncertaintyKind.HYPOTHESIS_DISAGREEMENT.value: 0.15,
        UncertaintyKind.LIGHTING.value: 0.10,
        UncertaintyKind.CALIBRATION.value: 0.08,
        UncertaintyKind.LENS_METADATA.value: 0.05,
    }
    total = float(sum(components[k] * weights[k] for k in components))
    return UncertaintyReport(
        target_id=target.target_id,
        total=total,
        components=components,
        uncovered_surface_fraction=float(uncovered),
        scale_authority=target.scale_authority.value,
        material_confidence_spread=float(mat_spread),
        hypothesis_disagreement=float(min(1.0, disagree)),
        details={
            "uncovered_regions": [c.region for c in target.cells if c.uncovered_area_m2 > 0],
            "has_scale_reference": target.has_scale_reference,
            "has_diffuse_light_view": target.has_diffuse_light_view,
            "has_grazing_light_view": target.has_grazing_light_view,
            "has_lens_metadata": target.has_lens_metadata,
            "has_calibration_target": target.has_calibration_target,
        },
    )
