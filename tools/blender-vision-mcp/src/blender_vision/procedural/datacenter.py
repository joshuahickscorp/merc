"""Data-centre archetype library with real internal structure.

Dimensions follow EIA-310 / IEC 60297 rack standards (1U = 44.45 mm,
19" mounting width = 482.6 mm). Server and GPU drawers expose chassis,
bezel, vent field, handles, bays and rear connectors — not featureless boxes.
"""

from __future__ import annotations

import math
from typing import Any, ClassVar

from blender_vision.procedural.archetype import (
    Archetype,
    Dimensions,
    GeometryKind,
    GeometryRecipe,
    ParamSpec,
    PartRole,
    PartSpec,
    Transform3D,
    box_part,
)
from blender_vision.procedural.standards import (
    DEFAULT_DEPTH_M,
    DEFAULT_FRAME_WIDTH_M,
    DEFAULT_U_COUNT,
    FLOOR_TILE_M,
    FRAME_WIDTH_600_M,
    FRAME_WIDTH_800_M,
    MOUNTING_WIDTH_M,
    U_HEIGHT_M,
    drawer_height_m,
    rack_height_m,
    u_to_z,
)
from blender_vision.v2.authority import Units


def _vent_field(
    name: str,
    width: float,
    height: float,
    location: tuple[float, float, float],
    *,
    cols: int = 12,
    rows: int = 4,
    depth: float = 0.003,
    lod: str = "mid",
) -> PartSpec:
    pitch_x = width / max(cols, 1)
    pitch_z = height / max(rows, 1)
    cell_w = pitch_x * 0.55
    cell_h = pitch_z * 0.45
    return PartSpec(
        name=name,
        role=PartRole.VENT,
        transform=Transform3D(location=location),
        geometry=GeometryRecipe(
            kind=GeometryKind.VENT_FIELD,
            size=(width, depth, height),
            count_x=cols,
            count_z=rows,
            pitch=(pitch_x, 0.0, pitch_z),
            cell_size=(cell_w, depth, cell_h),
        ),
        material_slot="vent",
        lod_keep_until=lod,
    )


class RackShell(Archetype):
    name: ClassVar[str] = "rack_shell"
    description: ClassVar[str] = "19-inch EIA rack frame with posts, rails and top/base."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_count", "int", Units.UNITLESS, 1, 48, DEFAULT_U_COUNT),
        ParamSpec(
            "frame_width_m",
            "float",
            Units.METRE,
            FRAME_WIDTH_600_M,
            FRAME_WIDTH_800_M,
            DEFAULT_FRAME_WIDTH_M,
            choices=(FRAME_WIDTH_600_M, FRAME_WIDTH_800_M),
        ),
        ParamSpec("depth_m", "float", Units.METRE, 0.6, 1.4, DEFAULT_DEPTH_M),
        ParamSpec("post_section_m", "float", Units.METRE, 0.03, 0.08, 0.05),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["frame_width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=rack_height_m(int(self.params["u_count"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["frame_width_m"])
        d = float(self.params["depth_m"])
        h = rack_height_m(int(self.params["u_count"]))
        post = float(self.params["post_section_m"])
        rail_t = 0.012
        base_h = 0.04
        top_h = 0.03
        parts: list[PartSpec] = []

        # Four vertical posts at corners of the outer envelope.
        for sx, sy, label in (
            (-1, -1, "post_fl"),
            (1, -1, "post_fr"),
            (-1, 1, "post_rl"),
            (1, 1, "post_rr"),
        ):
            parts.append(
                box_part(
                    label,
                    PartRole.STRUCTURE,
                    (post, post, h),
                    (sx * (w - post) * 0.5, sy * (d - post) * 0.5, h * 0.5),
                    material_slot="steel",
                )
            )

        parts.append(
            box_part(
                "base_plate",
                PartRole.STRUCTURE,
                (w, d, base_h),
                (0.0, 0.0, base_h * 0.5),
                material_slot="steel",
            )
        )
        parts.append(
            box_part(
                "top_plate",
                PartRole.STRUCTURE,
                (w, d, top_h),
                (0.0, 0.0, h - top_h * 0.5),
                material_slot="steel",
            )
        )

        # Mounting rails at EIA width, full bay height.
        rail_h = h - base_h - top_h
        rail_z = base_h + rail_h * 0.5
        rail_inset = (w - MOUNTING_WIDTH_M) * 0.5
        for sx, label in ((-1, "rail_left"), (1, "rail_right")):
            parts.append(
                box_part(
                    label,
                    PartRole.STRUCTURE,
                    (rail_t, d - 2 * post, rail_h),
                    (sx * (w * 0.5 - rail_inset), 0.0, rail_z),
                    material_slot="steel",
                    lod_keep_until="mid",
                )
            )

        # Front/rear horizontal members every 10U for structural read.
        u_count = int(self.params["u_count"])
        for u in range(10, u_count, 10):
            z = u_to_z(u) + U_HEIGHT_M * 0.5
            parts.append(
                box_part(
                    f"cross_front_u{u}",
                    PartRole.STRUCTURE,
                    (w - 2 * post, 0.02, 0.02),
                    (0.0, -(d * 0.5 - post), z),
                    material_slot="steel",
                    lod_keep_until="mid",
                )
            )
        return parts


class RackDoor(Archetype):
    name: ClassVar[str] = "rack_door"
    description: ClassVar[str] = "Perforated front/rear rack door with handle and hinges."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_count", "int", Units.UNITLESS, 1, 48, DEFAULT_U_COUNT),
        ParamSpec(
            "frame_width_m",
            "float",
            Units.METRE,
            FRAME_WIDTH_600_M,
            FRAME_WIDTH_800_M,
            DEFAULT_FRAME_WIDTH_M,
            choices=(FRAME_WIDTH_600_M, FRAME_WIDTH_800_M),
        ),
        ParamSpec("thickness_m", "float", Units.METRE, 0.01, 0.05, 0.025),
        ParamSpec("side", "str", Units.UNITLESS, default="front", choices=("front", "rear")),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["frame_width_m"]),
            depth_m=float(self.params["thickness_m"]),
            height_m=rack_height_m(int(self.params["u_count"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["frame_width_m"])
        t = float(self.params["thickness_m"])
        h = rack_height_m(int(self.params["u_count"]))
        frame = 0.04
        panel_w = w - 2 * frame
        panel_h = h - 2 * frame
        parts = [
            box_part(
                "door_frame",
                PartRole.DOOR,
                (w, t, h),
                (0.0, 0.0, h * 0.5),
                material_slot="steel",
            ),
            _vent_field(
                "door_perforation",
                panel_w,
                panel_h,
                (0.0, 0.0, h * 0.5),
                cols=16,
                rows=max(8, int(self.params["u_count"]) // 2),
                depth=min(0.003, t * 0.4),
            ),
            box_part(
                "door_handle",
                PartRole.HANDLE,
                (0.02, min(0.012, t * 0.5), 0.12),
                (w * 0.5 - 0.06, 0.0, h * 0.5),
                material_slot="metal",
                lod_keep_until="mid",
            ),
            box_part(
                "hinge_top",
                PartRole.STRUCTURE,
                (0.02, min(0.02, t * 0.7), 0.04),
                (-w * 0.5 + 0.02, 0.0, h - 0.1),
                material_slot="metal",
                lod_keep_until="near",
            ),
            box_part(
                "hinge_bottom",
                PartRole.STRUCTURE,
                (0.02, min(0.02, t * 0.7), 0.04),
                (-w * 0.5 + 0.02, 0.0, 0.1),
                material_slot="metal",
                lod_keep_until="near",
            ),
        ]
        return parts


class ServerDrawer(Archetype):
    name: ClassVar[str] = "server_drawer"
    description: ClassVar[str] = "1–4U server chassis with bezel, vents, drives and rear IO."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_height", "int", Units.UNITLESS, 1, 4, 2),
        ParamSpec("depth_m", "float", Units.METRE, 0.4, 0.9, 0.7),
        ParamSpec("drive_bays", "int", Units.UNITLESS, 0, 24, 8),
        ParamSpec(
            "width_m",
            "float",
            Units.METRE,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=drawer_height_m(int(self.params["u_height"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = drawer_height_m(int(self.params["u_height"]))
        wall = 0.0015
        bezel_t = 0.012
        parts: list[PartSpec] = [
            PartSpec(
                name="chassis",
                role=PartRole.SHELL,
                transform=Transform3D(location=(0.0, 0.0, h * 0.5)),
                geometry=GeometryRecipe(
                    kind=GeometryKind.HOLLOW_BOX,
                    size=(w, d, h),
                    wall_thickness=wall,
                    open_face="-Y",
                ),
                material_slot="chassis",
            ),
            box_part(
                "front_bezel",
                PartRole.PANEL,
                (w, bezel_t, h),
                (0.0, -(d * 0.5 - bezel_t * 0.5), h * 0.5),
                material_slot="bezel",
            ),
            _vent_field(
                "front_vent_field",
                w * 0.55,
                h * 0.55,
                (-w * 0.12, -(d * 0.5 - 0.002), h * 0.5),
                cols=14,
                rows=max(2, int(self.params["u_height"]) * 2),
                depth=0.004,
            ),
            box_part(
                "handle_left",
                PartRole.HANDLE,
                (0.018, 0.02, h * 0.55),
                (-w * 0.5 + 0.02, -(d * 0.5 - 0.02), h * 0.5),
                material_slot="metal",
                lod_keep_until="mid",
            ),
            box_part(
                "handle_right",
                PartRole.HANDLE,
                (0.018, 0.02, h * 0.55),
                (w * 0.5 - 0.02, -(d * 0.5 - 0.02), h * 0.5),
                material_slot="metal",
                lod_keep_until="mid",
            ),
        ]

        bays = int(self.params["drive_bays"])
        if bays > 0:
            bay_w = min(0.1, (w * 0.4) / max(bays, 1))
            bay_h = h * 0.7
            bay_d = 0.03
            start_x = w * 0.15
            for i in range(bays):
                x = start_x + i * (bay_w + 0.004)
                if x + bay_w * 0.5 > w * 0.48:
                    break
                parts.append(
                    box_part(
                        f"drive_bay_{i + 1:02d}",
                        PartRole.BAY,
                        (bay_w, bay_d, bay_h),
                        (x, -(d * 0.5 - bezel_t - bay_d * 0.5), h * 0.5),
                        material_slot="drive",
                        lod_keep_until="mid",
                    )
                )

        # Rear connectors
        for i, label in enumerate(("psu_1", "psu_2", "nic_1", "nic_2", "mgmt")):
            parts.append(
                box_part(
                    f"rear_{label}",
                    PartRole.CONNECTOR,
                    (0.035, 0.02, 0.015),
                    (-w * 0.28 + i * 0.07, d * 0.5 - 0.015, h * 0.35),
                    material_slot="connector",
                    lod_keep_until="near",
                )
            )
        parts.append(
            box_part(
                "status_led_strip",
                PartRole.LIGHT,
                (w * 0.12, 0.004, 0.006),
                (w * 0.32, -(d * 0.5 - 0.002), h * 0.75),
                material_slot="led",
                lod_keep_until="mid",
            )
        )
        return parts


class GpuDrawer(Archetype):
    name: ClassVar[str] = "gpu_drawer"
    description: ClassVar[str] = "2–8U GPU server drawer with GPU bays and heavy front vents."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_height", "int", Units.UNITLESS, 2, 8, 4),
        ParamSpec("depth_m", "float", Units.METRE, 0.5, 1.0, 0.85),
        ParamSpec("gpu_count", "int", Units.UNITLESS, 1, 16, 8),
        ParamSpec(
            "width_m",
            "float",
            Units.METRE,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=drawer_height_m(int(self.params["u_height"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = drawer_height_m(int(self.params["u_height"]))
        wall = 0.002
        bezel_t = 0.015
        parts: list[PartSpec] = [
            PartSpec(
                name="chassis",
                role=PartRole.SHELL,
                transform=Transform3D(location=(0.0, 0.0, h * 0.5)),
                geometry=GeometryRecipe(
                    kind=GeometryKind.HOLLOW_BOX,
                    size=(w, d, h),
                    wall_thickness=wall,
                    open_face="-Y",
                ),
                material_slot="chassis",
            ),
            box_part(
                "front_bezel",
                PartRole.PANEL,
                (w, bezel_t, h),
                (0.0, -(d * 0.5 - bezel_t * 0.5), h * 0.5),
                material_slot="bezel",
            ),
            _vent_field(
                "front_vent_field",
                w * 0.88,
                h * 0.78,
                (0.0, -(d * 0.5 - 0.003), h * 0.5),
                cols=20,
                rows=max(4, int(self.params["u_height"]) * 2),
                depth=0.005,
            ),
            box_part(
                "handle_left",
                PartRole.HANDLE,
                (0.022, 0.025, h * 0.6),
                (-w * 0.5 + 0.025, -(d * 0.5 - 0.02), h * 0.5),
                material_slot="metal",
                lod_keep_until="mid",
            ),
            box_part(
                "handle_right",
                PartRole.HANDLE,
                (0.022, 0.025, h * 0.6),
                (w * 0.5 - 0.025, -(d * 0.5 - 0.02), h * 0.5),
                material_slot="metal",
                lod_keep_until="mid",
            ),
        ]

        gpu_count = int(self.params["gpu_count"])
        bay_w = 0.04
        bay_h = h * 0.72
        bay_d = d * 0.55
        span = min(w * 0.85, gpu_count * (bay_w + 0.008))
        start = -span * 0.5 + bay_w * 0.5
        for i in range(gpu_count):
            x = start + i * (bay_w + 0.008)
            parts.append(
                box_part(
                    f"gpu_bay_{i + 1:02d}",
                    PartRole.BAY,
                    (bay_w, bay_d, bay_h),
                    (x, 0.05, h * 0.5),
                    material_slot="gpu",
                    lod_keep_until="mid",
                )
            )
            parts.append(
                box_part(
                    f"gpu_heatsink_{i + 1:02d}",
                    PartRole.STRUCTURE,
                    (bay_w * 0.85, bay_d * 0.4, bay_h * 0.35),
                    (x, -0.05, h * 0.65),
                    material_slot="heatsink",
                    lod_keep_until="near",
                )
            )

        for i, label in enumerate(("psu_1", "psu_2", "psu_3", "ib_1", "ib_2", "mgmt")):
            parts.append(
                box_part(
                    f"rear_{label}",
                    PartRole.CONNECTOR,
                    (0.035, 0.02, 0.02),
                    (-w * 0.32 + i * 0.065, d * 0.5 - 0.015, h * 0.4),
                    material_slot="connector",
                    lod_keep_until="near",
                )
            )
        parts.append(
            box_part(
                "status_led_matrix",
                PartRole.LIGHT,
                (w * 0.2, 0.004, 0.01),
                (0.0, -(d * 0.5 - 0.002), h * 0.88),
                material_slot="led",
                lod_keep_until="mid",
            )
        )
        return parts


class Switch(Archetype):
    name: ClassVar[str] = "switch"
    description: ClassVar[str] = "1–2U network switch with port face and rear fans."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_height", "int", Units.UNITLESS, 1, 2, 1),
        ParamSpec("depth_m", "float", Units.METRE, 0.25, 0.6, 0.4),
        ParamSpec("port_count", "int", Units.UNITLESS, 8, 128, 48),
        ParamSpec(
            "width_m",
            "float",
            Units.METRE,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=drawer_height_m(int(self.params["u_height"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = drawer_height_m(int(self.params["u_height"]))
        ports = int(self.params["port_count"])
        parts = [
            box_part(
                "chassis",
                PartRole.SHELL,
                (w, d, h),
                (0.0, 0.0, h * 0.5),
                material_slot="chassis",
            ),
            box_part(
                "front_panel",
                PartRole.PANEL,
                (w, 0.008, h),
                (0.0, -(d * 0.5 - 0.004), h * 0.5),
                material_slot="bezel",
            ),
        ]
        cols = min(ports, 24)
        rows = max(1, math.ceil(ports / cols))
        port_w, port_h, port_d = 0.014, 0.01, 0.02
        usable_w = w * 0.9
        pitch_x = usable_w / cols
        pitch_z = (h * 0.7) / rows
        for i in range(ports):
            col = i % cols
            row = i // cols
            x = -usable_w * 0.5 + pitch_x * (col + 0.5)
            z = h * 0.2 + pitch_z * (row + 0.5)
            parts.append(
                box_part(
                    f"port_{i + 1:03d}",
                    PartRole.CONNECTOR,
                    (port_w, port_d, port_h),
                    (x, -(d * 0.5 - 0.01), z),
                    material_slot="port",
                    lod_keep_until="mid",
                )
            )
        fan_h = min(0.03, h * 0.7)
        for i in range(3):
            parts.append(
                box_part(
                    f"rear_fan_{i + 1}",
                    PartRole.VENT,
                    (0.04, 0.015, fan_h),
                    (-0.08 + i * 0.08, d * 0.5 - 0.012, h * 0.5),
                    material_slot="fan",
                    lod_keep_until="mid",
                )
            )
        return parts


class BlankingPanel(Archetype):
    name: ClassVar[str] = "blanking_panel"
    description: ClassVar[str] = "1–6U blanking / filler panel."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_height", "int", Units.UNITLESS, 1, 6, 1),
        ParamSpec("thickness_m", "float", Units.METRE, 0.002, 0.02, 0.006),
        ParamSpec(
            "width_m",
            "float",
            Units.METRE,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
            MOUNTING_WIDTH_M,
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["thickness_m"]),
            height_m=drawer_height_m(int(self.params["u_height"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        t = float(self.params["thickness_m"])
        h = drawer_height_m(int(self.params["u_height"]))
        return [
            box_part(
                "panel",
                PartRole.PANEL,
                (w, t, h),
                (0.0, 0.0, h * 0.5),
                material_slot="blanking",
            ),
            box_part(
                "finger_grip",
                PartRole.HANDLE,
                (0.04, min(0.008, t * 0.6), 0.01),
                (0.0, 0.0, h * 0.5),
                material_slot="metal",
                lod_keep_until="near",
            ),
        ]


class Pdu(Archetype):
    name: ClassVar[str] = "pdu"
    description: ClassVar[str] = "Vertical rack PDU with outlet banks."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("u_count", "int", Units.UNITLESS, 20, 48, DEFAULT_U_COUNT),
        ParamSpec("outlet_count", "int", Units.UNITLESS, 8, 48, 24),
        ParamSpec("width_m", "float", Units.METRE, 0.04, 0.08, 0.055),
        ParamSpec("depth_m", "float", Units.METRE, 0.04, 0.1, 0.06),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=rack_height_m(int(self.params["u_count"])),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = rack_height_m(int(self.params["u_count"]))
        outlets = int(self.params["outlet_count"])
        parts = [
            box_part(
                "body",
                PartRole.INFRASTRUCTURE,
                (w, d, h),
                (0.0, 0.0, h * 0.5),
                material_slot="pdu",
            )
        ]
        pitch = (h * 0.9) / outlets
        for i in range(outlets):
            z = h * 0.05 + pitch * (i + 0.5)
            parts.append(
                box_part(
                    f"outlet_{i + 1:02d}",
                    PartRole.CONNECTOR,
                    (w * 0.7, 0.012, 0.03),
                    (0.0, d * 0.5 - 0.01, z),
                    material_slot="outlet",
                    lod_keep_until="mid",
                )
            )
        parts.append(
            box_part(
                "breaker_bank",
                PartRole.STRUCTURE,
                (w * 0.85, d * 0.5, 0.08),
                (0.0, 0.0, h - 0.08),
                material_slot="breaker",
                lod_keep_until="mid",
            )
        )
        return parts


class CableTray(Archetype):
    name: ClassVar[str] = "cable_tray"
    description: ClassVar[str] = "Overhead ladder cable tray segment."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("length_m", "float", Units.METRE, 0.3, 40.0, 2.4),
        ParamSpec("width_m", "float", Units.METRE, 0.15, 0.6, 0.3),
        ParamSpec("height_m", "float", Units.METRE, 0.04, 0.15, 0.08),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["length_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        length = float(self.params["length_m"])
        h = float(self.params["height_m"])
        rail = 0.015
        parts = [
            box_part(
                "rail_left",
                PartRole.INFRASTRUCTURE,
                (rail, length, h),
                (-(w - rail) * 0.5, 0.0, h * 0.5),
                material_slot="tray",
            ),
            box_part(
                "rail_right",
                PartRole.INFRASTRUCTURE,
                (rail, length, h),
                ((w - rail) * 0.5, 0.0, h * 0.5),
                material_slot="tray",
            ),
        ]
        rungs = max(2, int(length / 0.3))
        # Keep rungs fully inside the length envelope (half-rung inset at ends).
        usable = length - rail
        pitch = usable / rungs
        for i in range(rungs + 1):
            y = -usable * 0.5 + i * pitch
            parts.append(
                box_part(
                    f"rung_{i:02d}",
                    PartRole.INFRASTRUCTURE,
                    (w - 2 * rail, rail, rail * 0.6),
                    (0.0, y, rail * 0.3),
                    material_slot="tray",
                    lod_keep_until="mid",
                )
            )
        return parts


class CableBundle(Archetype):
    name: ClassVar[str] = "cable_bundle"
    description: ClassVar[str] = "Grouped cable run as a tapered bundle volume."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("length_m", "float", Units.METRE, 0.2, 40.0, 1.2),
        ParamSpec("diameter_m", "float", Units.METRE, 0.02, 0.2, 0.06),
        ParamSpec("strand_count", "int", Units.UNITLESS, 3, 48, 12),
    )

    def declared_dimensions(self) -> Dimensions:
        dia = float(self.params["diameter_m"])
        return Dimensions(
            width_m=dia,
            depth_m=float(self.params["length_m"]),
            height_m=dia,
        )

    def build(self) -> list[PartSpec]:
        length = float(self.params["length_m"])
        dia = float(self.params["diameter_m"])
        strands = int(self.params["strand_count"])
        parts = [
            PartSpec(
                name="bundle_core",
                role=PartRole.CABLE,
                transform=Transform3D(
                    location=(0.0, 0.0, 0.0),
                    rotation_euler=(math.pi * 0.5, 0.0, 0.0),
                ),
                geometry=GeometryRecipe(
                    kind=GeometryKind.BUNDLE,
                    size=(dia, dia, length),
                    segments=12,
                    count_x=strands,
                    extras={"strand_count": strands},
                ),
                material_slot="cable",
            )
        ]
        # Outer sheath volume for far LOD silhouette.
        parts.append(
            PartSpec(
                name="sheath",
                role=PartRole.CABLE,
                transform=Transform3D(
                    location=(0.0, 0.0, 0.0),
                    rotation_euler=(math.pi * 0.5, 0.0, 0.0),
                ),
                geometry=GeometryRecipe(
                    kind=GeometryKind.CYLINDER,
                    size=(dia, dia, length),
                    segments=16,
                ),
                material_slot="cable",
                lod_keep_until="far",
            )
        )
        return parts


class CoolingFace(Archetype):
    name: ClassVar[str] = "cooling_face"
    description: ClassVar[str] = "Cold-aisle containment / CRAC face with louvres."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("width_m", "float", Units.METRE, 0.4, 2.4, 1.2),
        ParamSpec("height_m", "float", Units.METRE, 1.5, 3.0, 2.2),
        ParamSpec("depth_m", "float", Units.METRE, 0.1, 0.5, 0.2),
        ParamSpec("louvre_count", "int", Units.UNITLESS, 4, 40, 16),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = float(self.params["height_m"])
        n = int(self.params["louvre_count"])
        parts = [
            box_part(
                "frame",
                PartRole.INFRASTRUCTURE,
                (w, d, h),
                (0.0, 0.0, h * 0.5),
                material_slot="cooling",
            )
        ]
        pitch = (h * 0.85) / n
        for i in range(n):
            z = h * 0.08 + pitch * (i + 0.5)
            parts.append(
                box_part(
                    f"louvre_{i + 1:02d}",
                    PartRole.VENT,
                    (w * 0.9, d * 0.4, pitch * 0.35),
                    (0.0, d * 0.15, z),
                    material_slot="louvre",
                    lod_keep_until="mid",
                )
            )
        return parts


class FloorTile(Archetype):
    name: ClassVar[str] = "floor_tile"
    description: ClassVar[str] = "Raised-floor tile (default 600 mm module)."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("size_m", "float", Units.METRE, 0.3, 1.2, FLOOR_TILE_M),
        ParamSpec("thickness_m", "float", Units.METRE, 0.02, 0.08, 0.035),
        ParamSpec("perforated", "bool", Units.UNITLESS, default=False),
    )

    def declared_dimensions(self) -> Dimensions:
        s = float(self.params["size_m"])
        return Dimensions(
            width_m=s,
            depth_m=s,
            height_m=float(self.params["thickness_m"]),
        )

    def build(self) -> list[PartSpec]:
        s = float(self.params["size_m"])
        t = float(self.params["thickness_m"])
        parts = [
            box_part(
                "tile",
                PartRole.FLOOR,
                (s, s, t),
                (0.0, 0.0, t * 0.5),
                material_slot="floor",
            )
        ]
        if self.params["perforated"]:
            parts.append(
                _vent_field(
                    "perforation",
                    s * 0.7,
                    s * 0.7,
                    (0.0, 0.0, t * 0.75),
                    cols=8,
                    rows=8,
                    depth=t * 0.4,
                )
            )
        return parts


class CeilingPanel(Archetype):
    name: ClassVar[str] = "ceiling_panel"
    description: ClassVar[str] = "Suspended ceiling panel module."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("size_m", "float", Units.METRE, 0.3, 1.2, FLOOR_TILE_M),
        ParamSpec("thickness_m", "float", Units.METRE, 0.01, 0.05, 0.02),
    )

    def declared_dimensions(self) -> Dimensions:
        s = float(self.params["size_m"])
        return Dimensions(
            width_m=s,
            depth_m=s,
            height_m=float(self.params["thickness_m"]),
        )

    def build(self) -> list[PartSpec]:
        s = float(self.params["size_m"])
        t = float(self.params["thickness_m"])
        return [
            box_part(
                "panel",
                PartRole.CEILING,
                (s, s, t),
                (0.0, 0.0, -t * 0.5),
                material_slot="ceiling",
            ),
            box_part(
                "grid_frame",
                PartRole.STRUCTURE,
                (s, s, t * 0.3),
                (0.0, 0.0, -t * 0.15),
                material_slot="grid",
                lod_keep_until="mid",
            ),
        ]


class WallRib(Archetype):
    name: ClassVar[str] = "wall_rib"
    description: ClassVar[str] = "Architectural wall rib / mullion."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("height_m", "float", Units.METRE, 1.0, 6.0, 3.0),
        ParamSpec("width_m", "float", Units.METRE, 0.05, 0.4, 0.15),
        ParamSpec("depth_m", "float", Units.METRE, 0.05, 0.4, 0.12),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = float(self.params["height_m"])
        return [
            box_part(
                "rib",
                PartRole.WALL,
                (w, d, h),
                (0.0, 0.0, h * 0.5),
                material_slot="wall",
            ),
            box_part(
                "cap",
                PartRole.STRUCTURE,
                (w, d, 0.03),
                (0.0, 0.0, h - 0.015),
                material_slot="wall",
                lod_keep_until="mid",
            ),
        ]


class Column(Archetype):
    name: ClassVar[str] = "column"
    description: ClassVar[str] = "Structural square column."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("height_m", "float", Units.METRE, 2.0, 8.0, 3.6),
        ParamSpec("section_m", "float", Units.METRE, 0.2, 0.8, 0.4),
    )

    def declared_dimensions(self) -> Dimensions:
        s = float(self.params["section_m"])
        return Dimensions(width_m=s, depth_m=s, height_m=float(self.params["height_m"]))

    def build(self) -> list[PartSpec]:
        s = float(self.params["section_m"])
        h = float(self.params["height_m"])
        return [
            box_part(
                "shaft",
                PartRole.STRUCTURE,
                (s, s, h),
                (0.0, 0.0, h * 0.5),
                material_slot="concrete",
            ),
            box_part(
                "base",
                PartRole.STRUCTURE,
                (s, s, 0.08),
                (0.0, 0.0, 0.04),
                material_slot="concrete",
                lod_keep_until="mid",
            ),
            box_part(
                "capital",
                PartRole.STRUCTURE,
                (s, s, 0.08),
                (0.0, 0.0, h - 0.04),
                material_slot="concrete",
                lod_keep_until="mid",
            ),
        ]


class Threshold(Archetype):
    name: ClassVar[str] = "threshold"
    description: ClassVar[str] = "Corridor entry threshold with door frame and floor band."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("width_m", "float", Units.METRE, 1.0, 4.0, 2.4),
        ParamSpec("height_m", "float", Units.METRE, 2.0, 4.0, 2.8),
        ParamSpec("depth_m", "float", Units.METRE, 0.15, 0.8, 0.35),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["depth_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        d = float(self.params["depth_m"])
        h = float(self.params["height_m"])
        post = 0.12
        return [
            box_part(
                "floor_band",
                PartRole.FLOOR,
                (w, d, 0.04),
                (0.0, 0.0, 0.02),
                material_slot="threshold",
            ),
            box_part(
                "jamb_left",
                PartRole.WALL,
                (post, d, h),
                (-(w - post) * 0.5, 0.0, h * 0.5),
                material_slot="wall",
            ),
            box_part(
                "jamb_right",
                PartRole.WALL,
                (post, d, h),
                ((w - post) * 0.5, 0.0, h * 0.5),
                material_slot="wall",
            ),
            box_part(
                "header",
                PartRole.WALL,
                (w, d, post),
                (0.0, 0.0, h - post * 0.5),
                material_slot="wall",
            ),
            box_part(
                "door_leaf_left",
                PartRole.DOOR,
                ((w - 2 * post) * 0.48, 0.05, h - post - 0.04),
                (-(w - 2 * post) * 0.25, 0.0, (h - post) * 0.5 + 0.02),
                material_slot="door",
                lod_keep_until="mid",
            ),
            box_part(
                "door_leaf_right",
                PartRole.DOOR,
                ((w - 2 * post) * 0.48, 0.05, h - post - 0.04),
                ((w - 2 * post) * 0.25, 0.0, (h - post) * 0.5 + 0.02),
                material_slot="door",
                lod_keep_until="mid",
            ),
        ]


class Aisle(Archetype):
    name: ClassVar[str] = "aisle"
    description: ClassVar[str] = "Aisle volume marker with floor strip and overhead guides."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("length_m", "float", Units.METRE, 1.0, 40.0, 12.0),
        ParamSpec("width_m", "float", Units.METRE, 0.8, 3.0, 1.2),
        ParamSpec("height_m", "float", Units.METRE, 2.0, 5.0, 3.0),
        ParamSpec("kind", "str", Units.UNITLESS, default="cold", choices=("cold", "hot")),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["length_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        length = float(self.params["length_m"])
        h = float(self.params["height_m"])
        return [
            box_part(
                "floor_strip",
                PartRole.FLOOR,
                (w, length, 0.01),
                (0.0, 0.0, 0.005),
                material_slot=f"aisle_{self.params['kind']}",
            ),
            box_part(
                "guide_left",
                PartRole.INFRASTRUCTURE,
                (0.02, length, h),
                (-(w * 0.5 - 0.01), 0.0, h * 0.5),
                material_slot="guide",
                lod_keep_until="far",
            ),
            box_part(
                "guide_right",
                PartRole.INFRASTRUCTURE,
                (0.02, length, h),
                (w * 0.5 - 0.01, 0.0, h * 0.5),
                material_slot="guide",
                lod_keep_until="far",
            ),
            box_part(
                "volume_marker",
                PartRole.DECORATIVE,
                (w * 0.98, length * 0.98, 0.02),
                (0.0, 0.0, h - 0.01),
                material_slot="volume",
                lod_keep_until="near",
            ),
        ]


class Junction(Archetype):
    name: ClassVar[str] = "junction"
    description: ClassVar[str] = "Corridor junction / turn module with floor plate."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("size_m", "float", Units.METRE, 1.5, 6.0, 2.4),
        ParamSpec("height_m", "float", Units.METRE, 2.0, 5.0, 3.0),
        ParamSpec(
            "turn",
            "str",
            Units.UNITLESS,
            default="left",
            choices=("left", "right", "tee", "cross"),
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        s = float(self.params["size_m"])
        return Dimensions(width_m=s, depth_m=s, height_m=float(self.params["height_m"]))

    def build(self) -> list[PartSpec]:
        s = float(self.params["size_m"])
        h = float(self.params["height_m"])
        parts = [
            box_part(
                "floor_plate",
                PartRole.FLOOR,
                (s, s, 0.015),
                (0.0, 0.0, 0.0075),
                material_slot="junction",
            ),
            box_part(
                "column_core",
                PartRole.STRUCTURE,
                (0.15, 0.15, h),
                (0.0, 0.0, h * 0.5),
                material_slot="structure",
                lod_keep_until="far",
            ),
        ]
        turn = self.params["turn"]
        if turn in {"left", "tee", "cross"}:
            parts.append(
                box_part(
                    "arm_left",
                    PartRole.FLOOR,
                    (s * 0.35, s * 0.2, 0.012),
                    (-s * 0.3, 0.0, 0.01),
                    material_slot="junction",
                )
            )
        if turn in {"right", "tee", "cross"}:
            parts.append(
                box_part(
                    "arm_right",
                    PartRole.FLOOR,
                    (s * 0.35, s * 0.2, 0.012),
                    (s * 0.3, 0.0, 0.01),
                    material_slot="junction",
                )
            )
        if turn == "cross":
            parts.append(
                box_part(
                    "arm_forward",
                    PartRole.FLOOR,
                    (s * 0.2, s * 0.35, 0.012),
                    (0.0, s * 0.3, 0.01),
                    material_slot="junction",
                )
            )
        return parts


class ContainmentDoor(Archetype):
    name: ClassVar[str] = "containment_door"
    description: ClassVar[str] = "Aisle containment sliding / swing door."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("width_m", "float", Units.METRE, 0.8, 2.0, 1.2),
        ParamSpec("height_m", "float", Units.METRE, 1.8, 3.0, 2.2),
        ParamSpec("thickness_m", "float", Units.METRE, 0.02, 0.08, 0.04),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["thickness_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        t = float(self.params["thickness_m"])
        h = float(self.params["height_m"])
        return [
            box_part(
                "leaf",
                PartRole.DOOR,
                (w, t, h),
                (0.0, 0.0, h * 0.5),
                material_slot="containment",
            ),
            box_part(
                "vision_panel",
                PartRole.PANEL,
                (w * 0.35, t * 0.4, h * 0.35),
                (0.0, 0.0, h * 0.55),
                material_slot="glass",
                lod_keep_until="mid",
            ),
            box_part(
                "handle",
                PartRole.HANDLE,
                (0.03, min(0.04, t * 0.7), 0.12),
                (w * 0.35, 0.0, h * 0.45),
                material_slot="metal",
                lod_keep_until="mid",
            ),
            box_part(
                "seal_bottom",
                PartRole.STRUCTURE,
                (w, t * 0.8, 0.02),
                (0.0, 0.0, 0.01),
                material_slot="seal",
                lod_keep_until="near",
            ),
        ]


class TerminalWall(Archetype):
    name: ClassVar[str] = "terminal_wall"
    description: ClassVar[str] = "Corridor terminal wall with service panel."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("width_m", "float", Units.METRE, 1.5, 6.0, 2.4),
        ParamSpec("height_m", "float", Units.METRE, 2.0, 5.0, 3.0),
        ParamSpec("thickness_m", "float", Units.METRE, 0.1, 0.5, 0.2),
    )

    def declared_dimensions(self) -> Dimensions:
        return Dimensions(
            width_m=float(self.params["width_m"]),
            depth_m=float(self.params["thickness_m"]),
            height_m=float(self.params["height_m"]),
        )

    def build(self) -> list[PartSpec]:
        w = float(self.params["width_m"])
        t = float(self.params["thickness_m"])
        h = float(self.params["height_m"])
        return [
            box_part(
                "wall",
                PartRole.WALL,
                (w, t, h),
                (0.0, 0.0, h * 0.5),
                material_slot="wall",
            ),
            box_part(
                "service_panel",
                PartRole.PANEL,
                (w * 0.4, min(0.03, t * 0.4), h * 0.35),
                (0.0, t * 0.15, h * 0.45),
                material_slot="panel",
                lod_keep_until="mid",
            ),
            box_part(
                "baseboard",
                PartRole.STRUCTURE,
                (w, t, 0.1),
                (0.0, 0.0, 0.05),
                material_slot="baseboard",
                lod_keep_until="mid",
            ),
            box_part(
                "cornice",
                PartRole.STRUCTURE,
                (w, t, 0.08),
                (0.0, 0.0, h - 0.04),
                material_slot="cornice",
                lod_keep_until="mid",
            ),
        ]


class StatusLightMatrix(Archetype):
    name: ClassVar[str] = "status_light_matrix"
    description: ClassVar[str] = "Per-instance status light grid; state is non-geometric."
    parameter_specs: ClassVar[tuple[ParamSpec, ...]] = (
        ParamSpec("cols", "int", Units.UNITLESS, 1, 64, 8),
        ParamSpec("rows", "int", Units.UNITLESS, 1, 64, 4),
        ParamSpec("pitch_m", "float", Units.METRE, 0.02, 0.2, 0.05),
        ParamSpec("cell_m", "float", Units.METRE, 0.008, 0.05, 0.02),
        ParamSpec(
            "status",
            "str",
            Units.UNITLESS,
            default="ok",
            choices=("ok", "warn", "fault", "off", "maintenance"),
        ),
    )

    def declared_dimensions(self) -> Dimensions:
        cols = int(self.params["cols"])
        rows = int(self.params["rows"])
        pitch = float(self.params["pitch_m"])
        cell = float(self.params["cell_m"])
        return Dimensions(
            width_m=(cols - 1) * pitch + cell,
            depth_m=cell,
            height_m=(rows - 1) * pitch + cell,
        )

    def build(self) -> list[PartSpec]:
        cols = int(self.params["cols"])
        rows = int(self.params["rows"])
        pitch = float(self.params["pitch_m"])
        cell = float(self.params["cell_m"])
        # Status is stored on the matrix root only — geometry is identical for all states.
        dims = self.declared_dimensions()
        root = PartSpec(
            name="matrix_root",
            role=PartRole.LIGHT,
            transform=Transform3D(location=(0.0, 0.0, dims.height_m * 0.5)),
            geometry=GeometryRecipe(
                kind=GeometryKind.LIGHT_CELL,
                size=(dims.width_m, cell, dims.height_m),
                count_x=cols,
                count_z=rows,
                pitch=(pitch, 0.0, pitch),
                cell_size=(cell, cell * 0.4, cell),
                extras={"status": self.params["status"]},
            ),
            material_slot=f"status_{self.params['status']}",
            state_keys=("status", "material_slot"),
        )
        # Individual cells for near LOD detail (same mesh layout regardless of status).
        cells: list[PartSpec] = []
        for r in range(rows):
            for c in range(cols):
                x = (c - (cols - 1) * 0.5) * pitch
                z = (r - (rows - 1) * 0.5) * pitch
                cells.append(
                    PartSpec(
                        name=f"cell_r{r:02d}_c{c:02d}",
                        role=PartRole.LIGHT,
                        transform=Transform3D(location=(x, 0.0, z)),
                        geometry=GeometryRecipe(
                            kind=GeometryKind.LIGHT_CELL,
                            size=(cell, cell * 0.4, cell),
                        ),
                        material_slot="status_cell",
                        state_keys=("status",),
                        lod_keep_until="near",
                    )
                )
        root.children = cells
        return [root]


DATACENTER_ARCHETYPES: dict[str, type[Archetype]] = {
    cls.name: cls
    for cls in (
        RackShell,
        RackDoor,
        ServerDrawer,
        GpuDrawer,
        Switch,
        BlankingPanel,
        Pdu,
        CableTray,
        CableBundle,
        CoolingFace,
        FloorTile,
        CeilingPanel,
        WallRib,
        Column,
        Threshold,
        Aisle,
        Junction,
        ContainmentDoor,
        TerminalWall,
        StatusLightMatrix,
    )
}


def make_archetype(name: str, params: dict[str, Any] | None = None) -> Archetype:
    if name not in DATACENTER_ARCHETYPES:
        from blender_vision.core.errors import ValidationError

        raise ValidationError(f"unknown archetype: {name!r}")
    return DATACENTER_ARCHETYPES[name](params)
