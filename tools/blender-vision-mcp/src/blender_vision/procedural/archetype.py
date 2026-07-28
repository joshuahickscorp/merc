"""Archetype base: semantic parameters, part trees, and provenance."""

from __future__ import annotations

import hashlib
import math
from abc import ABC, abstractmethod
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any, ClassVar

from blender_vision.core.errors import ValidationError
from blender_vision.core.util import canonical_json
from blender_vision.v2.authority import AuthorityClass, Units
from blender_vision.v2.records import Lineage


class PartRole(StrEnum):
    STRUCTURE = "structure"
    PANEL = "panel"
    EQUIPMENT = "equipment"
    VENT = "vent"
    HANDLE = "handle"
    CONNECTOR = "connector"
    BAY = "bay"
    LIGHT = "light"
    INFRASTRUCTURE = "infrastructure"
    FLOOR = "floor"
    CEILING = "ceiling"
    WALL = "wall"
    DOOR = "door"
    CABLE = "cable"
    DECORATIVE = "decorative"
    SHELL = "shell"


class GeometryKind(StrEnum):
    BOX = "box"
    CYLINDER = "cylinder"
    HOLLOW_BOX = "hollow_box"
    VENT_FIELD = "vent_field"
    TUBE = "tube"
    PANEL = "panel"
    LIGHT_CELL = "light_cell"
    BUNDLE = "bundle"


@dataclass(slots=True)
class ParamSpec:
    """Typed parameter declaration with units and inclusive valid range."""

    name: str
    kind: str  # float | int | str | bool
    units: Units = Units.UNITLESS
    minimum: float | int | None = None
    maximum: float | int | None = None
    default: Any = None
    choices: tuple[Any, ...] = ()
    description: str = ""

    def validate(self, value: Any) -> Any:
        if self.kind == "float":
            try:
                resolved = float(value)
            except (TypeError, ValueError) as error:
                raise ValidationError(f"{self.name} must be float, got {value!r}") from error
        elif self.kind == "int":
            try:
                resolved = int(value)
            except (TypeError, ValueError) as error:
                raise ValidationError(f"{self.name} must be int, got {value!r}") from error
            if isinstance(value, float) and not value.is_integer():
                raise ValidationError(f"{self.name} must be int, got {value!r}")
        elif self.kind == "str":
            if not isinstance(value, str):
                raise ValidationError(f"{self.name} must be str, got {value!r}")
            resolved = value
        elif self.kind == "bool":
            if not isinstance(value, bool):
                raise ValidationError(f"{self.name} must be bool, got {value!r}")
            resolved = value
        else:
            raise ValidationError(f"{self.name}: unknown kind {self.kind!r}")

        if self.choices and resolved not in self.choices:
            raise ValidationError(
                f"{self.name}={resolved!r} not in choices {list(self.choices)}"
            )
        if self.minimum is not None and resolved < self.minimum:
            raise ValidationError(
                f"{self.name}={resolved} below minimum {self.minimum}"
            )
        if self.maximum is not None and resolved > self.maximum:
            raise ValidationError(
                f"{self.name}={resolved} above maximum {self.maximum}"
            )
        return resolved

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["units"] = self.units.value
        value["choices"] = list(self.choices)
        return value


@dataclass(slots=True)
class GeometryRecipe:
    """Declarative primitive that emit.py turns into real Blender mesh data."""

    kind: GeometryKind
    size: tuple[float, float, float]
    segments: int = 12
    wall_thickness: float = 0.002
    count_x: int = 1
    count_y: int = 1
    count_z: int = 1
    pitch: tuple[float, float, float] = (0.0, 0.0, 0.0)
    cell_size: tuple[float, float, float] = (0.01, 0.01, 0.002)
    open_face: str = ""  # for hollow_box: +X|-X|+Y|-Y|+Z|-Z
    extras: dict[str, Any] = field(default_factory=dict)

    def local_bounds(self) -> tuple[tuple[float, float, float], tuple[float, float, float]]:
        sx, sy, sz = self.size
        half = (sx * 0.5, sy * 0.5, sz * 0.5)
        return ((-half[0], -half[1], -half[2]), half)

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind.value,
            "size": list(self.size),
            "segments": self.segments,
            "wall_thickness": self.wall_thickness,
            "count_x": self.count_x,
            "count_y": self.count_y,
            "count_z": self.count_z,
            "pitch": list(self.pitch),
            "cell_size": list(self.cell_size),
            "open_face": self.open_face,
            "extras": dict(self.extras),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> GeometryRecipe:
        return cls(
            kind=GeometryKind(payload["kind"]),
            size=tuple(payload["size"]),  # type: ignore[arg-type]
            segments=int(payload.get("segments", 12)),
            wall_thickness=float(payload.get("wall_thickness", 0.002)),
            count_x=int(payload.get("count_x", 1)),
            count_y=int(payload.get("count_y", 1)),
            count_z=int(payload.get("count_z", 1)),
            pitch=tuple(payload.get("pitch", (0.0, 0.0, 0.0))),  # type: ignore[arg-type]
            cell_size=tuple(payload.get("cell_size", (0.01, 0.01, 0.002))),  # type: ignore[arg-type]
            open_face=str(payload.get("open_face", "")),
            extras=dict(payload.get("extras", {})),
        )


@dataclass(slots=True)
class Transform3D:
    """TRS in Blender world frame (+Z up, -Y forward)."""

    location: tuple[float, float, float] = (0.0, 0.0, 0.0)
    rotation_euler: tuple[float, float, float] = (0.0, 0.0, 0.0)
    scale: tuple[float, float, float] = (1.0, 1.0, 1.0)

    def to_dict(self) -> dict[str, Any]:
        return {
            "location": list(self.location),
            "rotation_euler": list(self.rotation_euler),
            "scale": list(self.scale),
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> Transform3D:
        return cls(
            location=tuple(payload.get("location", (0.0, 0.0, 0.0))),  # type: ignore[arg-type]
            rotation_euler=tuple(payload.get("rotation_euler", (0.0, 0.0, 0.0))),  # type: ignore[arg-type]
            scale=tuple(payload.get("scale", (1.0, 1.0, 1.0))),  # type: ignore[arg-type]
        )

    def translated(self, dx: float, dy: float, dz: float) -> Transform3D:
        x, y, z = self.location
        return Transform3D(
            location=(x + dx, y + dy, z + dz),
            rotation_euler=self.rotation_euler,
            scale=self.scale,
        )


@dataclass(slots=True)
class PartSpec:
    """One named semantic part with a geometry recipe and optional children."""

    name: str
    role: PartRole
    transform: Transform3D
    geometry: GeometryRecipe
    material_slot: str = "neutral"
    state_keys: tuple[str, ...] = ()  # keys that may vary without remeshing
    children: list[PartSpec] = field(default_factory=list)
    lod_keep_until: str = "far"  # near | mid | far — dropped below this

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "role": self.role.value,
            "transform": self.transform.to_dict(),
            "geometry": self.geometry.to_dict(),
            "material_slot": self.material_slot,
            "state_keys": list(self.state_keys),
            "children": [child.to_dict() for child in self.children],
            "lod_keep_until": self.lod_keep_until,
        }

    @classmethod
    def from_dict(cls, payload: dict[str, Any]) -> PartSpec:
        return cls(
            name=payload["name"],
            role=PartRole(payload["role"]),
            transform=Transform3D.from_dict(payload["transform"]),
            geometry=GeometryRecipe.from_dict(payload["geometry"]),
            material_slot=str(payload.get("material_slot", "neutral")),
            state_keys=tuple(payload.get("state_keys", ())),
            children=[cls.from_dict(item) for item in payload.get("children", [])],
            lod_keep_until=str(payload.get("lod_keep_until", "far")),
        )

    def walk(self) -> list[PartSpec]:
        items = [self]
        for child in self.children:
            items.extend(child.walk())
        return items


@dataclass(slots=True)
class Dimensions:
    width_m: float
    depth_m: float
    height_m: float

    def as_tuple(self) -> tuple[float, float, float]:
        return (self.width_m, self.depth_m, self.height_m)

    def to_dict(self) -> dict[str, float]:
        return {
            "width_m": self.width_m,
            "depth_m": self.depth_m,
            "height_m": self.height_m,
        }


def _rotate_point(
    point: tuple[float, float, float],
    euler: tuple[float, float, float],
) -> tuple[float, float, float]:
    """Apply XYZ Euler rotation (Blender order) to a point."""
    x, y, z = point
    rx, ry, rz = euler
    # X
    cy, cz = math.cos(rx), math.sin(rx)
    y, z = y * cy - z * cz, y * cz + z * cy
    # Y
    cx, cz = math.cos(ry), math.sin(ry)
    x, z = x * cx + z * cz, -x * cz + z * cx
    # Z
    cx, cy = math.cos(rz), math.sin(rz)
    x, y = x * cx - y * cy, x * cy + y * cx
    return (x, y, z)


def part_world_bounds(
    part: PartSpec,
    parent: Transform3D | None = None,
) -> tuple[tuple[float, float, float], tuple[float, float, float]]:
    """Axis-aligned world bounds of a part tree (local geometry only, no children merge)."""
    loc = part.transform.location
    rot = part.transform.rotation_euler
    scale = part.transform.scale
    if parent is not None:
        # Compose: parent then local (approximate TRS, sufficient for AABB checks)
        pl = parent.location
        pr = parent.rotation_euler
        ps = parent.scale
        scaled = (loc[0] * ps[0], loc[1] * ps[1], loc[2] * ps[2])
        rotated = _rotate_point(scaled, pr)
        loc = (pl[0] + rotated[0], pl[1] + rotated[1], pl[2] + rotated[2])
        rot = (pr[0] + rot[0], pr[1] + rot[1], pr[2] + rot[2])
        scale = (ps[0] * scale[0], ps[1] * scale[1], ps[2] * scale[2])

    local_min, local_max = part.geometry.local_bounds()
    corners = [
        (x, y, z)
        for x in (local_min[0], local_max[0])
        for y in (local_min[1], local_max[1])
        for z in (local_min[2], local_max[2])
    ]
    world_pts: list[tuple[float, float, float]] = []
    for corner in corners:
        scaled = (corner[0] * scale[0], corner[1] * scale[1], corner[2] * scale[2])
        rotated = _rotate_point(scaled, rot)
        world_pts.append((loc[0] + rotated[0], loc[1] + rotated[1], loc[2] + rotated[2]))

    mins = (
        min(p[0] for p in world_pts),
        min(p[1] for p in world_pts),
        min(p[2] for p in world_pts),
    )
    maxs = (
        max(p[0] for p in world_pts),
        max(p[1] for p in world_pts),
        max(p[2] for p in world_pts),
    )
    return mins, maxs


def union_bounds(
    bounds: list[tuple[tuple[float, float, float], tuple[float, float, float]]],
) -> tuple[tuple[float, float, float], tuple[float, float, float]]:
    if not bounds:
        return (0.0, 0.0, 0.0), (0.0, 0.0, 0.0)
    mins = (
        min(b[0][0] for b in bounds),
        min(b[0][1] for b in bounds),
        min(b[0][2] for b in bounds),
    )
    maxs = (
        max(b[1][0] for b in bounds),
        max(b[1][1] for b in bounds),
        max(b[1][2] for b in bounds),
    )
    return mins, maxs


def measure_parts_bounds(parts: list[PartSpec]) -> Dimensions:
    """Union AABB of every leaf and parent part, expressed as width/depth/height."""
    all_bounds: list[tuple[tuple[float, float, float], tuple[float, float, float]]] = []

    def collect(part: PartSpec, parent: Transform3D | None) -> None:
        all_bounds.append(part_world_bounds(part, parent))
        composed = part.transform
        if parent is not None:
            pl = parent.location
            loc = part.transform.location
            composed = Transform3D(
                location=(pl[0] + loc[0], pl[1] + loc[1], pl[2] + loc[2]),
                rotation_euler=part.transform.rotation_euler,
                scale=part.transform.scale,
            )
        for child in part.children:
            collect(child, composed)

    for part in parts:
        collect(part, None)
    mins, maxs = union_bounds(all_bounds)
    return Dimensions(
        width_m=maxs[0] - mins[0],
        depth_m=maxs[1] - mins[1],
        height_m=maxs[2] - mins[2],
    )


def mesh_fingerprint(parts: list[PartSpec], *, include_state: bool = False) -> str:
    """Stable hash of geometry that should be identical under ``vary_state``."""

    def scrub_geometry(geom: dict[str, Any]) -> dict[str, Any]:
        if include_state:
            return geom
        extras = dict(geom.get("extras") or {})
        # Status / appearance keys ride in extras but must not remesh.
        for key in ("status", "appearance", "colour", "color"):
            extras.pop(key, None)
        cleaned = dict(geom)
        cleaned["extras"] = extras
        return cleaned

    def node(part: PartSpec) -> dict[str, Any]:
        payload = {
            "name": part.name,
            "role": part.role.value,
            "transform": part.transform.to_dict(),
            "geometry": scrub_geometry(part.geometry.to_dict()),
            "material_slot": part.material_slot if include_state else "",
            "children": [node(child) for child in part.children],
            "lod_keep_until": part.lod_keep_until,
        }
        if include_state:
            payload["state_keys"] = list(part.state_keys)
        return payload

    return hashlib.sha256(canonical_json([node(part) for part in parts])).hexdigest()


def box_part(
    name: str,
    role: PartRole,
    size: tuple[float, float, float],
    location: tuple[float, float, float],
    *,
    material_slot: str = "neutral",
    lod_keep_until: str = "far",
    rotation: tuple[float, float, float] = (0.0, 0.0, 0.0),
    children: list[PartSpec] | None = None,
) -> PartSpec:
    return PartSpec(
        name=name,
        role=role,
        transform=Transform3D(location=location, rotation_euler=rotation),
        geometry=GeometryRecipe(kind=GeometryKind.BOX, size=size),
        material_slot=material_slot,
        lod_keep_until=lod_keep_until,
        children=list(children or []),
    )


class Archetype(ABC):
    """Base for all procedural modules."""

    name: ClassVar[str]
    version: ClassVar[str] = "1.0.0"
    description: ClassVar[str] = ""
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = ()
    licensing: ClassVar[str] = "generated-procedural"
    rights_state: ClassVar[str] = "internal-synthetic"

    def __init__(self, params: dict[str, Any] | None = None) -> None:
        self.params = self.validate_params(params or {})

    @classmethod
    def defaults(cls) -> dict[str, Any]:
        return {
            spec.name: spec.default
            for spec in cls.parameter_specs
            if spec.default is not None
        }

    @classmethod
    def validate_params(cls, params: dict[str, Any]) -> dict[str, Any]:
        known = {spec.name: spec for spec in cls.parameter_specs}
        unknown = sorted(set(params) - set(known))
        if unknown:
            raise ValidationError(f"{cls.name}: unknown parameters {unknown}")
        resolved = dict(cls.defaults())
        for key, value in params.items():
            resolved[key] = known[key].validate(value)
        missing = [
            spec.name
            for spec in cls.parameter_specs
            if spec.name not in resolved
        ]
        if missing:
            raise ValidationError(f"{cls.name}: missing required parameters {missing}")
        return resolved

    @abstractmethod
    def declared_dimensions(self) -> Dimensions:
        """Authoritative exterior dimensions in metres."""

    @abstractmethod
    def build(self) -> list[PartSpec]:
        """Return the semantic part tree for current parameters."""

    def provenance(self) -> Lineage:
        return Lineage(
            tool="blender-vision-mcp",
            tool_version="0.1.0",
            operation=f"procedural.archetype.{self.name}",
            inputs=[],
            input_authorities=[AuthorityClass.MANUFACTURER_SPEC.value],
            parameters={"archetype": self.name, "version": self.version, **self.params},
            rights_state=self.rights_state,
            limitations=[
                "Geometry is procedural synthetic ground truth, not observed scan data.",
            ],
        )

    def part_names(self) -> list[str]:
        names: list[str] = []
        for part in self.build():
            names.extend(item.name for item in part.walk())
        return names

    def measured_dimensions(self) -> Dimensions:
        return measure_parts_bounds(self.build())

    def dimension_deltas_m(self) -> dict[str, float]:
        declared = self.declared_dimensions()
        measured = self.measured_dimensions()
        return {
            "width_m": measured.width_m - declared.width_m,
            "depth_m": measured.depth_m - declared.depth_m,
            "height_m": measured.height_m - declared.height_m,
        }

    def assert_dimensions(self, *, tolerance_m: float = 0.001) -> None:
        deltas = self.dimension_deltas_m()
        bad = {axis: delta for axis, delta in deltas.items() if abs(delta) > tolerance_m}
        if bad:
            raise ValidationError(
                f"{self.name}: declared/measured dimension delta exceeds "
                f"{tolerance_m * 1000:.1f} mm: {bad}"
            )

    def to_manifest_entry(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "version": self.version,
            "description": self.description,
            "licensing": self.licensing,
            "rights_state": self.rights_state,
            "parameters": [spec.to_dict() for spec in self.parameter_specs],
            "dimensions_m": self.declared_dimensions().to_dict(),
            "part_names": self.part_names(),
            "lineage": self.provenance().to_dict(),
        }
