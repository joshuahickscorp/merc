"""Semantic scene grammar: declarative placement and layout operations."""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.core.errors import ValidationError
from blender_vision.procedural.archetype import Transform3D
from blender_vision.procedural.standards import u_to_z


class OpKind(StrEnum):
    PLACE = "place"
    REPEAT_ALONG = "repeat_along"
    MIRROR = "mirror"
    LEAVE_GAP = "leave_gap"
    VARY_STATE = "vary_state"
    JUNCTION = "junction"
    TURN = "turn"
    POPULATE_RACK = "populate_rack"


@dataclass(slots=True)
class GrammarOp:
    kind: OpKind
    payload: dict[str, Any] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"kind": self.kind.value, **self.payload}

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> GrammarOp:
        payload = dict(data)
        kind = OpKind(payload.pop("kind"))
        return cls(kind=kind, payload=payload)


@dataclass(slots=True)
class InstanceRef:
    """Resolved instance produced by evaluating a scene program."""

    instance_id: str
    archetype: str
    params: dict[str, Any]
    transform: Transform3D
    state: dict[str, Any] = field(default_factory=dict)
    parent_id: str | None = None
    tags: list[str] = field(default_factory=list)
    # For rack equipment: occupied U ranges (inclusive 1-based)
    u_start: int | None = None
    u_end: int | None = None
    mesh_key: str = ""  # archetype+params fingerprint for instancing (excludes state)

    def to_dict(self) -> dict[str, Any]:
        return {
            "instance_id": self.instance_id,
            "archetype": self.archetype,
            "params": dict(self.params),
            "transform": self.transform.to_dict(),
            "state": dict(self.state),
            "parent_id": self.parent_id,
            "tags": list(self.tags),
            "u_start": self.u_start,
            "u_end": self.u_end,
            "mesh_key": self.mesh_key,
            "count": 1,
        }


@dataclass(slots=True)
class SceneProgram:
    """Declarative semantic program (Python + JSON-serialisable)."""

    name: str
    operations: list[GrammarOp] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "name": self.name,
            "operations": [op.to_dict() for op in self.operations],
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> SceneProgram:
        return cls(
            name=str(data["name"]),
            operations=[GrammarOp.from_dict(item) for item in data.get("operations", [])],
        )

    def place(
        self,
        archetype: str,
        instance_id: str,
        *,
        location: tuple[float, float, float] = (0.0, 0.0, 0.0),
        rotation_euler: tuple[float, float, float] = (0.0, 0.0, 0.0),
        params: dict[str, Any] | None = None,
        tags: list[str] | None = None,
        state: dict[str, Any] | None = None,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.PLACE,
                {
                    "archetype": archetype,
                    "instance_id": instance_id,
                    "location": list(location),
                    "rotation_euler": list(rotation_euler),
                    "params": dict(params or {}),
                    "tags": list(tags or []),
                    "state": dict(state or {}),
                },
            )
        )
        return self

    def repeat_along(
        self,
        source_id: str,
        *,
        axis: str,
        count: int,
        pitch_m: float,
        id_prefix: str | None = None,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.REPEAT_ALONG,
                {
                    "source_id": source_id,
                    "axis": axis,
                    "count": count,
                    "pitch_m": pitch_m,
                    "id_prefix": id_prefix or source_id,
                },
            )
        )
        return self

    def mirror(self, source_id: str, *, plane: str = "XZ", id_suffix: str = "_mir") -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.MIRROR,
                {"source_id": source_id, "plane": plane, "id_suffix": id_suffix},
            )
        )
        return self

    def leave_gap(self, rack_id: str, u_ranges: list[tuple[int, int]]) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.LEAVE_GAP,
                {
                    "rack_id": rack_id,
                    "u_ranges": [list(item) for item in u_ranges],
                },
            )
        )
        return self

    def vary_state(
        self,
        selector: str,
        state: dict[str, Any],
        *,
        match_tag: str | None = None,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.VARY_STATE,
                {
                    "selector": selector,
                    "state": dict(state),
                    "match_tag": match_tag,
                },
            )
        )
        return self

    def junction(
        self,
        instance_id: str,
        *,
        location: tuple[float, float, float],
        turn: str = "left",
        size_m: float = 2.4,
        height_m: float = 3.0,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.JUNCTION,
                {
                    "instance_id": instance_id,
                    "location": list(location),
                    "turn": turn,
                    "size_m": size_m,
                    "height_m": height_m,
                },
            )
        )
        return self

    def turn(
        self,
        *,
        at: tuple[float, float, float],
        direction: str,
        radius_m: float = 1.2,
        heading_deg: float = 0.0,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.TURN,
                {
                    "at": list(at),
                    "direction": direction,
                    "radius_m": radius_m,
                    "heading_deg": heading_deg,
                },
            )
        )
        return self

    def populate_rack(
        self,
        rack_id: str,
        *,
        u_start: int,
        u_end: int,
        archetype: str,
        u_height: int,
        params: dict[str, Any] | None = None,
        gap_ranges: list[tuple[int, int]] | None = None,
        id_prefix: str | None = None,
    ) -> SceneProgram:
        self.operations.append(
            GrammarOp(
                OpKind.POPULATE_RACK,
                {
                    "rack_id": rack_id,
                    "u_start": u_start,
                    "u_end": u_end,
                    "archetype": archetype,
                    "u_height": u_height,
                    "params": dict(params or {}),
                    "gap_ranges": [list(g) for g in (gap_ranges or [])],
                    "id_prefix": id_prefix or f"{rack_id}_{archetype}",
                },
            )
        )
        return self


def _mesh_key(archetype: str, params: dict[str, Any]) -> str:
    items = ",".join(f"{k}={params[k]!r}" for k in sorted(params))
    return f"{archetype}|{items}"


def _axis_index(axis: str) -> int:
    mapping = {"x": 0, "y": 1, "z": 2, "X": 0, "Y": 1, "Z": 2}
    if axis not in mapping:
        raise ValidationError(f"axis must be x|y|z, got {axis!r}")
    return mapping[axis]


def evaluate_program(program: SceneProgram) -> list[InstanceRef]:
    """Expand a scene program into concrete instance references."""
    by_id: dict[str, InstanceRef] = {}
    order: list[str] = []
    gaps: dict[str, list[tuple[int, int]]] = {}
    heading_rad = 0.0

    def add(inst: InstanceRef) -> None:
        if inst.instance_id in by_id:
            raise ValidationError(f"duplicate instance_id {inst.instance_id!r}")
        inst.mesh_key = _mesh_key(inst.archetype, inst.params)
        by_id[inst.instance_id] = inst
        order.append(inst.instance_id)

    for op in program.operations:
        kind = op.kind
        p = op.payload
        if kind is OpKind.PLACE:
            add(
                InstanceRef(
                    instance_id=str(p["instance_id"]),
                    archetype=str(p["archetype"]),
                    params=dict(p.get("params") or {}),
                    transform=Transform3D(
                        location=tuple(p.get("location", (0.0, 0.0, 0.0))),  # type: ignore[arg-type]
                        rotation_euler=tuple(p.get("rotation_euler", (0.0, 0.0, 0.0))),  # type: ignore[arg-type]
                    ),
                    state=dict(p.get("state") or {}),
                    tags=list(p.get("tags") or []),
                )
            )
        elif kind is OpKind.REPEAT_ALONG:
            source = by_id.get(str(p["source_id"]))
            if source is None:
                raise ValidationError(f"repeat_along source {p['source_id']!r} not found")
            count = int(p["count"])
            if count < 1:
                raise ValidationError("repeat_along count must be >= 1")
            pitch = float(p["pitch_m"])
            axis_i = _axis_index(str(p["axis"]))
            prefix = str(p.get("id_prefix") or source.instance_id)
            # First instance is the source; create count-1 additional copies.
            for i in range(1, count):
                loc = list(source.transform.location)
                loc[axis_i] = loc[axis_i] + pitch * i
                add(
                    InstanceRef(
                        instance_id=f"{prefix}_{i:02d}",
                        archetype=source.archetype,
                        params=dict(source.params),
                        transform=Transform3D(
                            location=(loc[0], loc[1], loc[2]),
                            rotation_euler=source.transform.rotation_euler,
                            scale=source.transform.scale,
                        ),
                        state=dict(source.state),
                        tags=list(source.tags) + ["repeated"],
                        parent_id=source.instance_id,
                    )
                )
            source.tags = list(source.tags) + ["repeated", "repeat_source"]
        elif kind is OpKind.MIRROR:
            source = by_id.get(str(p["source_id"]))
            if source is None:
                raise ValidationError(f"mirror source {p['source_id']!r} not found")
            plane = str(p.get("plane", "XZ")).upper()
            loc = list(source.transform.location)
            rot = list(source.transform.rotation_euler)
            if plane == "XZ":
                loc[1] = -loc[1]
                rot[2] = -rot[2]
            elif plane == "YZ":
                loc[0] = -loc[0]
                rot[2] = math.pi - rot[2]
            elif plane == "XY":
                loc[2] = -loc[2]
                rot[0] = -rot[0]
            else:
                raise ValidationError(f"unsupported mirror plane {plane!r}")
            suffix = str(p.get("id_suffix", "_mir"))
            add(
                InstanceRef(
                    instance_id=f"{source.instance_id}{suffix}",
                    archetype=source.archetype,
                    params=dict(source.params),
                    transform=Transform3D(
                        location=(loc[0], loc[1], loc[2]),
                        rotation_euler=(rot[0], rot[1], rot[2]),
                        scale=source.transform.scale,
                    ),
                    state=dict(source.state),
                    tags=list(source.tags) + ["mirrored"],
                    parent_id=source.instance_id,
                )
            )
        elif kind is OpKind.LEAVE_GAP:
            rack_id = str(p["rack_id"])
            ranges = [(int(a), int(b)) for a, b in p.get("u_ranges", [])]
            for a, b in ranges:
                if a < 1 or b < a:
                    raise ValidationError(f"invalid gap range {(a, b)}")
            gaps.setdefault(rack_id, []).extend(ranges)
        elif kind is OpKind.VARY_STATE:
            selector = str(p["selector"])
            state = dict(p.get("state") or {})
            match_tag = p.get("match_tag")
            matched = 0
            for inst in by_id.values():
                if inst.instance_id == selector or selector in inst.tags:
                    if match_tag and match_tag not in inst.tags:
                        continue
                    # Geometry-affecting params must not change.
                    inst.state = {**inst.state, **state}
                    # mesh_key intentionally unchanged — state is non-geometric.
                    matched += 1
                elif selector == "*" or selector == "all":
                    if match_tag and match_tag not in inst.tags:
                        continue
                    inst.state = {**inst.state, **state}
                    matched += 1
            if matched == 0:
                raise ValidationError(f"vary_state selector {selector!r} matched nothing")
        elif kind is OpKind.JUNCTION:
            add(
                InstanceRef(
                    instance_id=str(p["instance_id"]),
                    archetype="junction",
                    params={
                        "turn": p.get("turn", "left"),
                        "size_m": float(p.get("size_m", 2.4)),
                        "height_m": float(p.get("height_m", 3.0)),
                    },
                    transform=Transform3D(
                        location=tuple(p["location"]),  # type: ignore[arg-type]
                        rotation_euler=(0.0, 0.0, heading_rad),
                    ),
                    tags=["junction", str(p.get("turn", "left"))],
                )
            )
        elif kind is OpKind.TURN:
            direction = str(p["direction"]).lower()
            if direction not in {"left", "right"}:
                raise ValidationError("turn direction must be left|right")
            delta = math.pi * 0.5 if direction == "left" else -math.pi * 0.5
            heading_rad += delta
            # Turn is a heading update; geometry is placed by subsequent ops.
        elif kind is OpKind.POPULATE_RACK:
            rack = by_id.get(str(p["rack_id"]))
            if rack is None:
                raise ValidationError(f"populate_rack rack {p['rack_id']!r} not found")
            u_start = int(p["u_start"])
            u_end = int(p["u_end"])
            u_height = int(p["u_height"])
            if u_height < 1 or u_start < 1 or u_end < u_start:
                raise ValidationError("invalid populate_rack U range")
            gap_ranges = [(int(a), int(b)) for a, b in p.get("gap_ranges", [])]
            gap_ranges.extend(gaps.get(rack.instance_id, []))
            params = dict(p.get("params") or {})
            params.setdefault("u_height", u_height)
            prefix = str(p.get("id_prefix") or f"{rack.instance_id}_eq")
            cursor = u_start
            slot = 0
            while cursor + u_height - 1 <= u_end:
                end_u = cursor + u_height - 1
                if _overlaps_gaps(cursor, end_u, gap_ranges):
                    cursor += 1
                    continue
                z = u_to_z(cursor)
                rx, ry, rz = rack.transform.location
                # Equipment sits inside rack, front-aligned, Z at U bottom.
                add(
                    InstanceRef(
                        instance_id=f"{prefix}_{slot:02d}",
                        archetype=str(p["archetype"]),
                        params=dict(params),
                        transform=Transform3D(
                            location=(rx, ry, rz + z),
                            rotation_euler=rack.transform.rotation_euler,
                        ),
                        parent_id=rack.instance_id,
                        tags=["equipment", "rack_populated"],
                        u_start=cursor,
                        u_end=end_u,
                    )
                )
                slot += 1
                cursor += u_height
        else:
            raise ValidationError(f"unsupported grammar op {kind}")

    return [by_id[i] for i in order]


def _overlaps_gaps(start: int, end: int, gaps: list[tuple[int, int]]) -> bool:
    return any(not (end < g0 or start > g1) for g0, g1 in gaps)


def occupied_u_ranges(instances: list[InstanceRef], rack_id: str) -> list[tuple[int, int]]:
    return sorted(
        (inst.u_start, inst.u_end)
        for inst in instances
        if inst.parent_id == rack_id and inst.u_start is not None and inst.u_end is not None
    )


def empty_u_slots(
    instances: list[InstanceRef],
    rack_id: str,
    *,
    u_count: int,
) -> list[int]:
    occupied: set[int] = set()
    for start, end in occupied_u_ranges(instances, rack_id):
        occupied.update(range(start, end + 1))
    return [u for u in range(1, u_count + 1) if u not in occupied]


def datacenter_flagship_program(
    *,
    aisle_length_m: float = 12.0,
    rack_count_per_side: int = 12,
    rack_pitch_m: float = 0.6,
    aisle_width_m: float = 1.2,
) -> SceneProgram:
    """threshold -> main aisle -> left-turn junction -> second aisle -> terminal wall."""
    program = SceneProgram(name="datacenter_flagship")
    rack_depth = 1.0
    half_aisle = aisle_width_m * 0.5
    rack_y = half_aisle + rack_depth * 0.5

    program.place(
        "threshold",
        "threshold_entry",
        location=(0.0, -1.0, 0.0),
        params={"width_m": aisle_width_m + 2 * rack_depth, "height_m": 3.0, "depth_m": 0.35},
        tags=["path", "threshold"],
    )
    program.place(
        "aisle",
        "aisle_main",
        location=(0.0, aisle_length_m * 0.5, 0.0),
        params={
            "length_m": aisle_length_m,
            "width_m": aisle_width_m,
            "height_m": 3.0,
            "kind": "cold",
        },
        tags=["path", "aisle", "main"],
    )

    # Left-side seed rack, then repeat along +Y.
    program.place(
        "rack_shell",
        "rack_L_00",
        location=(-rack_y, 1.2, 0.0),
        params={"u_count": 42, "frame_width_m": 0.6, "depth_m": rack_depth},
        tags=["rack", "left"],
    )
    program.repeat_along(
        "rack_L_00",
        axis="y",
        count=rack_count_per_side,
        pitch_m=rack_pitch_m,
        id_prefix="rack_L",
    )
    program.place(
        "rack_shell",
        "rack_R_00",
        location=(rack_y, 1.2, 0.0),
        rotation_euler=(0.0, 0.0, math.pi),
        params={"u_count": 42, "frame_width_m": 0.6, "depth_m": rack_depth},
        tags=["rack", "right"],
    )
    program.repeat_along(
        "rack_R_00",
        axis="y",
        count=rack_count_per_side,
        pitch_m=rack_pitch_m,
        id_prefix="rack_R",
    )

    # Populate first left rack: U3–U28 with 4U GPU drawers, three blanking gaps.
    program.leave_gap("rack_L_00", [(9, 10), (17, 18), (25, 26)])
    program.populate_rack(
        "rack_L_00",
        u_start=3,
        u_end=28,
        archetype="gpu_drawer",
        u_height=4,
        params={"u_height": 4, "gpu_count": 8, "depth_m": 0.85},
        id_prefix="rack_L_00_gpu",
    )

    # Overhead infrastructure along main aisle.
    program.place(
        "cable_tray",
        "tray_main",
        location=(0.0, aisle_length_m * 0.5, 2.7),
        params={"length_m": aisle_length_m, "width_m": 0.3, "height_m": 0.08},
        tags=["overhead", "tray"],
    )
    program.place(
        "cable_bundle",
        "bundle_main",
        location=(0.05, aisle_length_m * 0.5, 2.65),
        params={"length_m": aisle_length_m * 0.9, "diameter_m": 0.06, "strand_count": 12},
        tags=["overhead", "cable"],
    )

    # Floor tile grid under main aisle.
    program.place(
        "floor_tile",
        "tile_00",
        location=(-aisle_width_m * 0.25, 0.3, 0.0),
        params={"size_m": 0.6, "thickness_m": 0.035, "perforated": True},
        tags=["floor"],
    )
    program.repeat_along(
        "tile_00",
        axis="y",
        count=int(aisle_length_m / 0.6),
        pitch_m=0.6,
        id_prefix="tile",
    )

    # Status light matrix driven by per-instance state.
    program.place(
        "status_light_matrix",
        "status_entry",
        location=(0.0, 0.2, 2.4),
        params={"cols": 8, "rows": 4, "pitch_m": 0.05, "cell_m": 0.02, "status": "ok"},
        tags=["status", "signage"],
        state={"status": "ok"},
    )
    program.vary_state("status_entry", {"status": "warn"})

    # Left-turn junction at end of main aisle.
    junction_y = aisle_length_m + 1.2
    program.junction(
        "junction_left",
        location=(0.0, junction_y, 0.0),
        turn="left",
        size_m=aisle_width_m + 2 * rack_depth,
        height_m=3.0,
    )
    program.turn(at=(0.0, junction_y, 0.0), direction="left")

    # Second aisle along +X after left turn.
    second_len = 8.0
    program.place(
        "aisle",
        "aisle_second",
        location=(second_len * 0.5, junction_y, 0.0),
        rotation_euler=(0.0, 0.0, math.pi * 0.5),
        params={
            "length_m": second_len,
            "width_m": aisle_width_m,
            "height_m": 3.0,
            "kind": "hot",
        },
        tags=["path", "aisle", "second"],
    )
    program.place(
        "cable_tray",
        "tray_second",
        location=(second_len * 0.5, junction_y, 2.7),
        rotation_euler=(0.0, 0.0, math.pi * 0.5),
        params={"length_m": second_len, "width_m": 0.3, "height_m": 0.08},
        tags=["overhead", "tray"],
    )
    program.place(
        "containment_door",
        "containment_mid",
        location=(second_len * 0.35, junction_y, 0.0),
        params={"width_m": aisle_width_m, "height_m": 2.2, "thickness_m": 0.04},
        tags=["containment"],
    )
    program.place(
        "terminal_wall",
        "terminal",
        location=(second_len + 0.5, junction_y, 0.0),
        params={
            "width_m": aisle_width_m + 2 * rack_depth,
            "height_m": 3.0,
            "thickness_m": 0.2,
        },
        tags=["path", "terminal"],
    )
    program.place(
        "cooling_face",
        "cooling_terminal",
        location=(second_len * 0.7, junction_y + aisle_width_m * 0.5 + 0.15, 0.0),
        params={"width_m": 1.2, "height_m": 2.2, "depth_m": 0.2, "louvre_count": 16},
        tags=["cooling"],
    )
    program.place(
        "column",
        "col_junction",
        location=(-1.5, junction_y - 1.5, 0.0),
        params={"height_m": 3.6, "section_m": 0.4},
        tags=["structure"],
    )
    return program


def program_to_jsonable(program: SceneProgram) -> dict[str, Any]:
    return program.to_dict()
