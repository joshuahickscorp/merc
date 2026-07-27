"""Greedy next-view planning over a coverage atlas (pure geometry)."""

from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from typing import Any

import numpy as np

from blender_vision.core.errors import ValidationError
from blender_vision.spatial.coverage import CoverageAtlas, SurfacePatch
from blender_vision.spatial.trajectory import look_at_matrix
from blender_vision.v2.authority import (
    AuthorityClass,
    CoordinateFrame,
    Uncertainty,
    Units,
    VisibilityState,
)
from blender_vision.v2.records import Lineage, ObservationBundle


@dataclass(slots=True)
class ProposedView:
    """One candidate camera placement with its marginal coverage gain."""

    view_id: str
    position: np.ndarray
    target: np.ndarray
    world_from_camera: np.ndarray
    marginal_coverage: float
    newly_covered_patch_ids: list[str]
    cumulative_covered_fraction: float
    rank: int

    def to_dict(self) -> dict[str, Any]:
        return {
            "view_id": self.view_id,
            "position": self.position.tolist(),
            "target": self.target.tolist(),
            "world_from_camera": self.world_from_camera.tolist(),
            "marginal_coverage": self.marginal_coverage,
            "newly_covered_patch_ids": list(self.newly_covered_patch_ids),
            "cumulative_covered_fraction": self.cumulative_covered_fraction,
            "rank": self.rank,
        }


@dataclass(slots=True)
class CapturePlan:
    """Ordered viewpoints maximising marginal coverage under a budget."""

    proposed_views: list[ProposedView]
    existing_covered_fraction: float
    final_covered_fraction: float
    never_observed_fraction: float
    patch_count: int
    budget: int
    frame: CoordinateFrame
    coverage_deltas: list[dict[str, Any]] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "proposed_views": [view.to_dict() for view in self.proposed_views],
            "existing_covered_fraction": self.existing_covered_fraction,
            "final_covered_fraction": self.final_covered_fraction,
            "never_observed_fraction": self.never_observed_fraction,
            "patch_count": self.patch_count,
            "budget": self.budget,
            "frame": self.frame.to_dict(),
            "coverage_deltas": list(self.coverage_deltas),
        }

    def seal_observation_bundle(
        self,
        *,
        target_id: str,
        operation: str = "spatial.capture_plan",
    ) -> ObservationBundle:
        return ObservationBundle(
            id=f"plan-{uuid.uuid4().hex[:12]}",
            target_id=target_id,
            authority=AuthorityClass.HYPOTHETICAL,
            lineage=Lineage(
                operation=operation,
                inputs=[],
                input_authorities=[],
                parameters={
                    "budget": self.budget,
                    "existing_covered_fraction": self.existing_covered_fraction,
                    "final_covered_fraction": self.final_covered_fraction,
                    "proposed_count": len(self.proposed_views),
                    "claimed_authority": AuthorityClass.HYPOTHETICAL.value,
                },
                environment={"frame": self.frame.to_dict()},
                limitations=[
                    "pure geometry greedy set cover; not information-gain scoring",
                    "NextViewRequest emission is a separate subsystem",
                ],
            ),
            uncertainty=Uncertainty(
                kind="coverage-plan",
                units=Units.NORMALIZED,
                basis="greedy marginal coverage over discrete patches",
                samples=self.patch_count,
            ),
            modalities=["capture-plan"],
            coverage={
                "existing_covered_fraction": self.existing_covered_fraction,
                "final_covered_fraction": self.final_covered_fraction,
                "never_observed_fraction": self.never_observed_fraction,
                "deltas": self.coverage_deltas,
            },
        ).seal()


def plan_capture(
    target: dict[str, Any],
    existing_views: list[dict[str, Any]],
    budget: int,
    *,
    candidate_views: list[dict[str, Any]] | None = None,
    atlas: CoverageAtlas | None = None,
    resolution: int = 6,
    n_candidates: int = 24,
) -> CapturePlan:
    """Greedy set cover: pick up to `budget` candidates maximising new coverage.

    `target` keys:
      - bounds_min, bounds_max: box target, or
      - center, radius: sphere target
      - look_at (optional): point cameras aim at (defaults to centre)

    Returns ordered ProposedViews and the coverage deltas a NextViewRequest
    subsystem can consume. Does not emit NextViewRequest records itself.
    """
    if budget < 0:
        raise ValidationError("budget must be non-negative")
    atlas = atlas or CoverageAtlas()
    patches = _target_patches(target, atlas, resolution=resolution)
    look_at = np.asarray(
        target.get(
            "look_at",
            target.get(
                "center",
                (
                    (
                        np.asarray(target["bounds_min"], dtype=np.float64)
                        + np.asarray(target["bounds_max"], dtype=np.float64)
                    )
                    * 0.5
                )
                if "bounds_min" in target
                else (0.0, 0.0, 0.0),
            ),
        ),
        dtype=np.float64,
    )

    # Baseline coverage from existing views.
    baseline = atlas.evaluate(patches, existing_views)
    observed: set[str] = {
        p.patch_id
        for p in baseline.patches
        if p.visibility
        in {VisibilityState.DIRECTLY_VISIBLE, VisibilityState.PARTIALLY_VISIBLE}
    }
    existing_fraction = len(observed) / max(1, len(patches))

    candidates = candidate_views or _default_orbit_candidates(
        look_at, target, n=n_candidates
    )
    if not candidates and budget > 0:
        raise ValidationError("no candidate views available for planning")

    selected: list[ProposedView] = []
    selected_cameras: list[dict[str, Any]] = list(existing_views)
    coverage_deltas: list[dict[str, Any]] = []
    remaining = list(enumerate(candidates))

    for rank in range(budget):
        best_index: int | None = None
        best_gain = -1.0
        best_new: set[str] = set()
        best_cam: dict[str, Any] | None = None
        for list_index, (orig_index, cand) in enumerate(remaining):
            cam = _ensure_camera_dict(cand, look_at, index=orig_index)
            report = atlas.evaluate(patches, [cam])
            newly = {
                p.patch_id
                for p in report.patches
                if p.visibility
                in {
                    VisibilityState.DIRECTLY_VISIBLE,
                    VisibilityState.PARTIALLY_VISIBLE,
                }
                and p.patch_id not in observed
            }
            gain = len(newly) / max(1, len(patches))
            if gain > best_gain:
                best_gain = gain
                best_index = list_index
                best_new = newly
                best_cam = cam
        if best_index is None or best_cam is None or best_gain <= 0.0:
            break
        remaining.pop(best_index)
        observed |= best_new
        selected_cameras.append(best_cam)
        cumulative = len(observed) / max(1, len(patches))
        matrix = np.asarray(best_cam["world_from_camera"], dtype=np.float64)
        view = ProposedView(
            view_id=str(best_cam.get("label", f"proposed-{rank}")),
            position=matrix[:3, 3].copy(),
            target=look_at.copy(),
            world_from_camera=matrix,
            marginal_coverage=best_gain,
            newly_covered_patch_ids=sorted(best_new),
            cumulative_covered_fraction=cumulative,
            rank=rank,
        )
        selected.append(view)
        coverage_deltas.append(
            {
                "rank": rank,
                "view_id": view.view_id,
                "marginal_coverage": best_gain,
                "cumulative_covered_fraction": cumulative,
                "newly_covered_count": len(best_new),
            }
        )

    final_fraction = len(observed) / max(1, len(patches))
    return CapturePlan(
        proposed_views=selected,
        existing_covered_fraction=existing_fraction,
        final_covered_fraction=final_fraction,
        never_observed_fraction=1.0 - final_fraction,
        patch_count=len(patches),
        budget=budget,
        frame=atlas.frame,
        coverage_deltas=coverage_deltas,
    )


def _target_patches(
    target: dict[str, Any], atlas: CoverageAtlas, *, resolution: int
) -> list[SurfacePatch]:
    if "bounds_min" in target and "bounds_max" in target:
        return atlas.sample_box_patches(
            np.asarray(target["bounds_min"], dtype=np.float64),
            np.asarray(target["bounds_max"], dtype=np.float64),
            resolution=resolution,
        )
    if "center" in target and "radius" in target:
        return atlas.sample_sphere_patches(
            np.asarray(target["center"], dtype=np.float64),
            float(target["radius"]),
            n_lat=max(4, resolution),
            n_lon=max(8, resolution * 2),
        )
    raise ValidationError(
        "target must provide bounds_min/bounds_max or center/radius"
    )


def _default_orbit_candidates(
    look_at: np.ndarray, target: dict[str, Any], *, n: int
) -> list[dict[str, Any]]:
    if "radius" in target:
        radius = float(target["radius"]) * 2.5
    elif "bounds_min" in target:
        lo = np.asarray(target["bounds_min"], dtype=np.float64)
        hi = np.asarray(target["bounds_max"], dtype=np.float64)
        radius = float(np.linalg.norm(hi - lo)) * 1.5
    else:
        radius = 2.0
    # Several elevation rings so the underside can be proposed when budget allows.
    elevations = [-0.6, -0.15, 0.25, 0.65]
    candidates: list[dict[str, Any]] = []
    per_ring = max(1, n // len(elevations))
    index = 0
    for elev in elevations:
        for step in range(per_ring):
            theta = 2 * np.pi * step / per_ring
            height = radius * elev
            horizontal = radius * np.sqrt(max(0.0, 1.0 - elev**2 * 0.5))
            position = look_at + np.array(
                [
                    horizontal * np.cos(theta),
                    horizontal * np.sin(theta),
                    height,
                ]
            )
            candidates.append(
                {
                    "label": f"orbit-{index}",
                    "position": position.tolist(),
                    "target": look_at.tolist(),
                }
            )
            index += 1
            if index >= n:
                return candidates
    return candidates


def _ensure_camera_dict(
    cand: dict[str, Any], look_at: np.ndarray, *, index: int
) -> dict[str, Any]:
    if "world_from_camera" in cand:
        matrix = np.asarray(cand["world_from_camera"], dtype=np.float64)
        result = dict(cand)
        result["world_from_camera"] = matrix
        result.setdefault("label", f"cand-{index}")
        return result
    position = np.asarray(cand.get("position", (0.0, 0.0, 1.0)), dtype=np.float64)
    target = np.asarray(cand.get("target", look_at), dtype=np.float64)
    matrix = look_at_matrix(position, target)
    return {
        "label": str(cand.get("label", f"cand-{index}")),
        "position": position.tolist(),
        "target": target.tolist(),
        "world_from_camera": matrix,
    }
