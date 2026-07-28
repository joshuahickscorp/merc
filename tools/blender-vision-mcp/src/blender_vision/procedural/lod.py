"""LOD generation and identity checks for procedural archetypes."""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.procedural.archetype import (
    Dimensions,
    PartSpec,
    measure_parts_bounds,
)


class LodLevel(StrEnum):
    NEAR = "near"
    MID = "mid"
    FAR = "far"


#: Triangle budgets are soft targets used by emit for mesh density selection.
DEFAULT_TRIANGLE_BUDGETS: dict[str, int] = {
    LodLevel.NEAR.value: 50_000,
    LodLevel.MID.value: 8_000,
    LodLevel.FAR.value: 1_200,
}

_LOD_RANK = {LodLevel.NEAR.value: 3, LodLevel.MID.value: 2, LodLevel.FAR.value: 1}


def _kept_for_lod(part: PartSpec, lod: str) -> bool:
    """Keep part if its detail ceiling is at least as coarse as the requested LOD.

    ``lod_keep_until="near"`` → near only; ``"mid"`` → near+mid; ``"far"`` → all.
    Rank is higher for finer LODs, so keep when want_rank >= keep_rank.
    """
    keep_rank = _LOD_RANK.get(part.lod_keep_until, 1)
    want_rank = _LOD_RANK.get(lod, 1)
    return want_rank >= keep_rank


def filter_parts_for_lod(parts: list[PartSpec], lod: str) -> list[PartSpec]:
    """Drop parts whose ``lod_keep_until`` is below the requested level."""

    def clone_filtered(part: PartSpec) -> PartSpec | None:
        if not _kept_for_lod(part, lod):
            return None
        children = []
        for child in part.children:
            kept = clone_filtered(child)
            if kept is not None:
                children.append(kept)
        return PartSpec(
            name=part.name,
            role=part.role,
            transform=part.transform,
            geometry=part.geometry,
            material_slot=part.material_slot,
            state_keys=part.state_keys,
            children=children,
            lod_keep_until=part.lod_keep_until,
        )

    result: list[PartSpec] = []
    for part in parts:
        kept = clone_filtered(part)
        if kept is not None:
            result.append(kept)
    return result


def part_name_set(parts: list[PartSpec]) -> set[str]:
    names: set[str] = set()
    for part in parts:
        for item in part.walk():
            names.add(item.name)
    return names


def silhouette_extents(dims: Dimensions) -> dict[str, float]:
    """Orthographic silhouette extents (proxy for true projected silhouette)."""
    return {
        "xy": max(dims.width_m, dims.depth_m),
        "xz": max(dims.width_m, dims.height_m),
        "yz": max(dims.depth_m, dims.height_m),
        "width_m": dims.width_m,
        "depth_m": dims.depth_m,
        "height_m": dims.height_m,
    }


@dataclass(slots=True)
class LodVariant:
    level: str
    parts: list[PartSpec]
    triangle_budget: int
    dimensions: Dimensions
    part_names: set[str] = field(default_factory=set)

    def to_dict(self) -> dict[str, Any]:
        return {
            "level": self.level,
            "triangle_budget": self.triangle_budget,
            "dimensions": self.dimensions.to_dict(),
            "part_names": sorted(self.part_names),
            "parts": [part.to_dict() for part in self.parts],
        }


@dataclass(slots=True)
class LodIdentityReport:
    archetype: str
    passed: bool
    bbox_ok: bool
    silhouette_ok: bool
    part_set_ok: bool
    lost_parts: list[str] = field(default_factory=list)
    bbox_deltas_m: dict[str, dict[str, float]] = field(default_factory=dict)
    silhouette_deltas: dict[str, dict[str, float]] = field(default_factory=dict)
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "archetype": self.archetype,
            "passed": self.passed,
            "bbox_ok": self.bbox_ok,
            "silhouette_ok": self.silhouette_ok,
            "part_set_ok": self.part_set_ok,
            "lost_parts": list(self.lost_parts),
            "bbox_deltas_m": dict(self.bbox_deltas_m),
            "silhouette_deltas": dict(self.silhouette_deltas),
            "notes": list(self.notes),
        }


def generate_lods(
    parts: list[PartSpec],
    *,
    budgets: dict[str, int] | None = None,
) -> dict[str, LodVariant]:
    budgets = budgets or DEFAULT_TRIANGLE_BUDGETS
    variants: dict[str, LodVariant] = {}
    for level in (LodLevel.NEAR.value, LodLevel.MID.value, LodLevel.FAR.value):
        filtered = filter_parts_for_lod(parts, level)
        dims = measure_parts_bounds(filtered) if filtered else Dimensions(0.0, 0.0, 0.0)
        variants[level] = LodVariant(
            level=level,
            parts=filtered,
            triangle_budget=int(budgets.get(level, DEFAULT_TRIANGLE_BUDGETS[level])),
            dimensions=dims,
            part_names=part_name_set(filtered),
        )
    return variants


def check_lod_identity(
    archetype_name: str,
    variants: dict[str, LodVariant],
    *,
    bbox_tolerance_m: float = 0.02,
    silhouette_tolerance: float = 0.05,
    require_all_parts: bool = False,
) -> LodIdentityReport:
    """Compare mid/far LODs against near.

    Bounding box and silhouette must remain within tolerance. A far LOD that
    drops semantic parts is reported (``part_set_ok=False``) and fails when
    ``require_all_parts`` is True; by default part loss is recorded as a
    finding without forcing failure, so intentional far simplification is
    allowed but never silent.
    """
    if LodLevel.NEAR.value not in variants:
        raise ValidationError("LOD identity requires a near variant")
    near = variants[LodLevel.NEAR.value]
    lost: list[str] = []
    bbox_deltas: dict[str, dict[str, float]] = {}
    sil_deltas: dict[str, dict[str, float]] = {}
    bbox_ok = True
    sil_ok = True
    notes: list[str] = []

    near_sil = silhouette_extents(near.dimensions)
    for level in (LodLevel.MID.value, LodLevel.FAR.value):
        if level not in variants:
            notes.append(f"missing LOD level {level}")
            continue
        variant = variants[level]
        missing = sorted(near.part_names - variant.part_names)
        if missing:
            lost.extend(f"{level}:{name}" for name in missing)
            notes.append(f"{level} loses semantic parts: {missing}")

        deltas = {
            "width_m": variant.dimensions.width_m - near.dimensions.width_m,
            "depth_m": variant.dimensions.depth_m - near.dimensions.depth_m,
            "height_m": variant.dimensions.height_m - near.dimensions.height_m,
        }
        bbox_deltas[level] = deltas
        if any(abs(v) > bbox_tolerance_m for v in deltas.values()):
            bbox_ok = False
            notes.append(f"{level} bbox delta exceeds {bbox_tolerance_m} m: {deltas}")

        sil = silhouette_extents(variant.dimensions)
        s_delta = {key: sil[key] - near_sil[key] for key in near_sil}
        sil_deltas[level] = s_delta
        # Relative tolerance on silhouette extents.
        for key, delta in s_delta.items():
            base = max(abs(near_sil[key]), 1e-6)
            if abs(delta) / base > silhouette_tolerance and abs(delta) > bbox_tolerance_m:
                sil_ok = False
                notes.append(f"{level} silhouette {key} delta {delta:.4f} exceeds tolerance")

    part_set_ok = not lost
    # Identity hard-fails on bbox/silhouette; part loss is always reported.
    passed = bbox_ok and sil_ok and (part_set_ok if require_all_parts else True)
    if lost and not require_all_parts:
        notes.append(
            "semantic part loss recorded (allowed for far LOD simplification; "
            "not silent)"
        )
    return LodIdentityReport(
        archetype=archetype_name,
        passed=passed,
        bbox_ok=bbox_ok,
        silhouette_ok=sil_ok,
        part_set_ok=part_set_ok,
        lost_parts=lost,
        bbox_deltas_m=bbox_deltas,
        silhouette_deltas=sil_deltas,
        notes=notes,
    )


def intentionally_broken_far_lod(parts: list[PartSpec]) -> list[PartSpec]:
    """Test helper: far LOD that drops every non-shell part and shrinks the shell."""
    from blender_vision.procedural.archetype import (
        GeometryKind,
        GeometryRecipe,
        PartRole,
        Transform3D,
    )

    shells = [p for p in parts if p.role in {PartRole.SHELL, PartRole.STRUCTURE, PartRole.FLOOR}]
    if not shells:
        shells = parts[:1]
    broken: list[PartSpec] = []
    for part in shells[:1]:
        sx, sy, sz = part.geometry.size
        broken.append(
            PartSpec(
                name=part.name,
                role=part.role,
                transform=Transform3D(),
                geometry=GeometryRecipe(
                    kind=GeometryKind.BOX,
                    size=(sx * 0.5, sy * 0.5, sz * 0.5),
                ),
                material_slot=part.material_slot,
                lod_keep_until="far",
            )
        )
    return broken
