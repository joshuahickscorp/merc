from __future__ import annotations

from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any

from blender_vision.constraints.models import Constraint


class ComponentType(StrEnum):
    BODY = "Body"
    PANEL = "Panel"
    SHELL = "Shell"
    CUTOUT = "Cutout"
    PORT = "Port"
    BUTTON = "Button"
    SCREW = "Screw"
    FOOT = "Foot"
    FAN = "Fan"
    BLADE_ARRAY = "BladeArray"
    VENT_ARRAY = "VentArray"
    HOLE_ARRAY = "HoleArray"
    GRILLE = "Grille"
    BRACKET = "Bracket"
    HEAT_SINK = "HeatSink"
    LOGO = "Logo"
    PCB = "PCB"
    SPLINE_BODY_SECTION = "SplineBodySection"
    LOFTED_SURFACE = "LoftedSurface"
    WHEEL_ARCH = "WheelArch"
    PANEL_CUT = "PanelCut"
    PANEL_GAP = "PanelGap"
    SURFACE_CREASE = "SurfaceCrease"
    AEROFOIL = "Aerofoil"
    DUCT = "Duct"
    VENT = "Vent"
    LIGHT_HOUSING = "LightHousing"
    GLASS_PANEL = "GlassPanel"
    TIRE_PROFILE = "TireProfile"
    WHEEL_SPOKE_ARRAY = "WheelSpokeArray"
    BRAKE_ASSEMBLY = "BrakeAssembly"
    DIFFUSER_CHANNEL = "DiffuserChannel"
    UNDERBODY_PANEL = "UnderbodyPanel"
    BEZIER = "Bezier"
    NURBS = "NURBS"
    CURVE_NETWORK = "CurveNetwork"
    SWEEP = "Sweep"
    PATCH_SURFACE = "PatchSurface"
    CONTROLLED_SHRINKWRAP = "ControlledShrinkwrap"
    RETOPOLOGY_CAGE = "RetopologyCage"


@dataclass(slots=True)
class ComponentSpec:
    id: str
    type: ComponentType
    parameters: dict[str, Any]
    constraints: list[Constraint] = field(default_factory=list)
    evidence_bindings: list[str] = field(default_factory=list)
    material_slots: list[str] = field(default_factory=list)
    lod_rules: dict[str, Any] = field(default_factory=dict)
    generator_version: str = "1"

    def to_dict(self) -> dict[str, Any]:
        value = asdict(self)
        value["type"] = self.type.value
        value["constraints"] = [constraint.to_dict() for constraint in self.constraints]
        return value
