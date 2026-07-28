"""Parameterised Blender lighting rigs.

Each rig is realised as actual Blender lights and world nodes via generated
``bpy`` script bodies — not a passive parameter dict alone. Calling
``apply_rig_script`` inside Blender (or via headless Blender) creates key, fill,
negative fill, rim, environment, and reflection cards with the declared
exposure, white balance, tone map, and atmosphere.
"""

from __future__ import annotations

from collections.abc import Callable
from dataclasses import asdict, dataclass, field
from enum import StrEnum
from typing import Any


class RigClass(StrEnum):
    NEUTRAL_DOCUMENTATION = "neutral_documentation"
    BLACK_PRODUCT_STUDIO = "black_product_studio"
    DATACENTER_CORRIDOR = "datacenter_corridor"
    SOFT_ORGANIC = "soft_organic"


RIG_NAMES: tuple[str, ...] = tuple(item.value for item in RigClass)


@dataclass(slots=True)
class LightSpec:
    name: str
    light_type: str  # AREA | SUN | POINT | SPOT
    location: tuple[float, float, float]
    rotation_euler_deg: tuple[float, float, float]
    energy: float
    color: tuple[float, float, float] = (1.0, 1.0, 1.0)
    size: float = 1.0
    angle_deg: float = 45.0
    negative: bool = False

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class ReflectionCard:
    name: str
    location: tuple[float, float, float]
    scale: tuple[float, float, float]
    color: tuple[float, float, float] = (1.0, 1.0, 1.0)
    emission: float = 2.0

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class LightingRig:
    """A complete, named lighting rig with Blender realisation."""

    rig_class: RigClass
    key: LightSpec
    fill: LightSpec
    negative_fill: LightSpec
    rim: LightSpec
    environment_color: tuple[float, float, float]
    environment_strength: float
    reflection_cards: list[ReflectionCard] = field(default_factory=list)
    shadow_softness: float = 0.25
    exposure: float = 0.0
    white_balance_k: float = 6500.0
    tone_map: str = "AgX"
    atmosphere: dict[str, Any] = field(default_factory=dict)
    description: str = ""

    def to_hypothesis_fields(self) -> dict[str, Any]:
        return {
            "rig_class": self.rig_class.value,
            "key": self.key.to_dict(),
            "fill": self.fill.to_dict(),
            "negative_fill": self.negative_fill.to_dict(),
            "rim": self.rim.to_dict(),
            "environment": {
                "color": list(self.environment_color),
                "strength": self.environment_strength,
            },
            "reflection_cards": [card.to_dict() for card in self.reflection_cards],
            "shadow_softness": self.shadow_softness,
            "exposure": self.exposure,
            "white_balance_k": self.white_balance_k,
            "tone_map": self.tone_map,
            "atmosphere": dict(self.atmosphere),
        }

    def blender_script_body(self) -> str:
        """Return Python that creates the rig's lights and world inside Blender."""
        lines = [
            "import bpy, math",
            "from mathutils import Euler",
            "",
            "def _clear_lights():",
            "    for obj in list(bpy.data.objects):",
            "        if obj.type == 'LIGHT' or obj.name.startswith('BVMCP_Card_'):",
            "            bpy.data.objects.remove(obj, do_unlink=True)",
            "",
            "def _add_light(spec):",
            "    light_data = bpy.data.lights.new(name=spec['name'], type=spec['light_type'])",
            "    energy = float(spec['energy'])",
            "    if spec.get('negative'):",
            "        energy = -abs(energy)",
            "    light_data.energy = energy",
            "    light_data.color = spec['color']",
            "    if spec['light_type'] == 'AREA':",
            "        light_data.shape = 'RECTANGLE'",
            "        light_data.size = float(spec.get('size', 1.0))",
            "        light_data.size_y = float(spec.get('size', 1.0))",
            "    if spec['light_type'] == 'SUN' and hasattr(light_data, 'angle'):",
            "        light_data.angle = math.radians(float(spec.get('angle_deg', 5.0)))",
            "    if spec['light_type'] == 'SPOT':",
            "        light_data.spot_size = math.radians(float(spec.get('angle_deg', 45.0)))",
            "    obj = bpy.data.objects.new(spec['name'], light_data)",
            "    bpy.context.scene.collection.objects.link(obj)",
            "    obj.location = spec['location']",
            "    rx, ry, rz = spec['rotation_euler_deg']",
            "    obj.rotation_euler = Euler("
            "        (math.radians(rx), math.radians(ry), math.radians(rz))"
            "    )",
            "    obj['bvmcp_rig_role'] = spec.get('role', 'light')",
            "    return obj",
            "",
            "def _add_card(card):",
            "    bpy.ops.mesh.primitive_plane_add(size=1.0, location=card['location'])",
            "    plane = bpy.context.active_object",
            "    plane.name = card['name']",
            "    plane.scale = card['scale']",
            "    mat = bpy.data.materials.new(card['name'] + '_mat')",
            "    mat.use_nodes = True",
            "    nodes = mat.node_tree.nodes",
            "    nodes.clear()",
            "    out = nodes.new('ShaderNodeOutputMaterial')",
            "    emit = nodes.new('ShaderNodeEmission')",
            "    emit.inputs[0].default_value = (*card['color'], 1.0)",
            "    emit.inputs[1].default_value = float(card['emission'])",
            "    mat.node_tree.links.new(emit.outputs[0], out.inputs[0])",
            "    plane.data.materials.append(mat)",
            "    plane['bvmcp_rig_role'] = 'reflection_card'",
            "    return plane",
            "",
            "_clear_lights()",
        ]
        for role, spec in (
            ("key", self.key),
            ("fill", self.fill),
            ("negative_fill", self.negative_fill),
            ("rim", self.rim),
        ):
            payload = spec.to_dict()
            payload["role"] = role
            lines.append(f"_add_light({payload!r})")
        for card in self.reflection_cards:
            lines.append(f"_add_card({card.to_dict()!r})")
        lines.extend(
            [
                "world = bpy.context.scene.world or bpy.data.worlds.new('BVMCP_World')",
                "bpy.context.scene.world = world",
                "world.use_nodes = True",
                "nodes = world.node_tree.nodes",
                "nodes.clear()",
                "out = nodes.new('ShaderNodeOutputWorld')",
                "bg = nodes.new('ShaderNodeBackground')",
                f"bg.inputs[0].default_value = ({self.environment_color[0]}, "
                f"{self.environment_color[1]}, {self.environment_color[2]}, 1.0)",
                f"bg.inputs[1].default_value = {float(self.environment_strength)}",
                "world.node_tree.links.new(bg.outputs[0], out.inputs[0])",
                f"bpy.context.scene.view_settings.exposure = {float(self.exposure)}",
                f"bpy.context.scene.view_settings.view_transform = {self.tone_map!r}",
                f"bpy.context.scene['bvmcp_white_balance_k'] = {float(self.white_balance_k)}",
                f"bpy.context.scene['bvmcp_shadow_softness'] = {float(self.shadow_softness)}",
                f"bpy.context.scene['bvmcp_rig_class'] = {self.rig_class.value!r}",
                f"bpy.context.scene['bvmcp_atmosphere'] = {self.atmosphere!r}",
            ]
        )
        return "\n".join(lines) + "\n"


def _neutral_documentation() -> LightingRig:
    return LightingRig(
        rig_class=RigClass.NEUTRAL_DOCUMENTATION,
        key=LightSpec(
            "BVMCP_Key",
            "AREA",
            (2.4, -1.2, 3.0),
            (50.0, 0.0, 35.0),
            energy=220.0,
            size=1.4,
            color=(1.0, 0.98, 0.95),
        ),
        fill=LightSpec(
            "BVMCP_Fill",
            "AREA",
            (-2.0, -1.5, 2.0),
            (55.0, 0.0, -40.0),
            energy=60.0,
            size=2.0,
            color=(0.95, 0.97, 1.0),
        ),
        negative_fill=LightSpec(
            "BVMCP_NegFill",
            "AREA",
            (0.0, 2.5, 1.2),
            (90.0, 0.0, 180.0),
            energy=25.0,
            size=2.5,
            color=(0.9, 0.92, 1.0),
            negative=True,
        ),
        rim=LightSpec(
            "BVMCP_Rim",
            "AREA",
            (0.5, 2.0, 2.5),
            (65.0, 0.0, 180.0),
            energy=90.0,
            size=0.8,
            color=(0.9, 0.95, 1.0),
        ),
        environment_color=(0.55, 0.58, 0.62),
        environment_strength=0.35,
        reflection_cards=[
            ReflectionCard("BVMCP_Card_Key", (2.8, 0.0, 1.5), (0.8, 1.0, 1.2), emission=1.5)
        ],
        shadow_softness=0.35,
        exposure=0.0,
        white_balance_k=6500.0,
        tone_map="AgX",
        atmosphere={"fog_density": 0.0, "haze": 0.0},
        description="Neutral documentation: even product lighting, soft shadows.",
    )


def _black_product_studio() -> LightingRig:
    return LightingRig(
        rig_class=RigClass.BLACK_PRODUCT_STUDIO,
        key=LightSpec(
            "BVMCP_Key",
            "AREA",
            (1.8, -2.2, 2.8),
            (48.0, 0.0, 20.0),
            energy=380.0,
            size=0.7,
            color=(1.0, 0.97, 0.93),
        ),
        fill=LightSpec(
            "BVMCP_Fill",
            "AREA",
            (-1.5, -1.8, 1.5),
            (60.0, 0.0, -30.0),
            energy=28.0,
            size=1.5,
            color=(0.85, 0.9, 1.0),
        ),
        negative_fill=LightSpec(
            "BVMCP_NegFill",
            "AREA",
            (-0.2, 2.0, 0.8),
            (85.0, 0.0, 180.0),
            energy=40.0,
            size=3.0,
            color=(0.7, 0.75, 0.85),
            negative=True,
        ),
        rim=LightSpec(
            "BVMCP_Rim",
            "AREA",
            (0.0, 2.4, 2.2),
            (70.0, 0.0, 180.0),
            energy=160.0,
            size=0.5,
            color=(0.95, 0.98, 1.0),
        ),
        environment_color=(0.02, 0.02, 0.025),
        environment_strength=0.05,
        reflection_cards=[
            ReflectionCard(
                "BVMCP_Card_Long", (2.2, 0.5, 1.4), (0.3, 1.6, 0.8), emission=4.0
            ),
            ReflectionCard(
                "BVMCP_Card_Soft", (-2.0, 0.0, 1.2), (0.5, 1.0, 0.9), emission=1.2
            ),
        ],
        shadow_softness=0.15,
        exposure=-0.3,
        white_balance_k=5600.0,
        tone_map="AgX",
        atmosphere={"fog_density": 0.0, "haze": 0.0},
        description="Black product studio: high contrast, tight key, dark env.",
    )


def _datacenter_corridor() -> LightingRig:
    return LightingRig(
        rig_class=RigClass.DATACENTER_CORRIDOR,
        key=LightSpec(
            "BVMCP_Key",
            "AREA",
            (0.0, 0.0, 4.5),
            (0.0, 0.0, 0.0),
            energy=180.0,
            size=6.0,
            color=(0.92, 0.95, 1.0),
        ),
        fill=LightSpec(
            "BVMCP_Fill",
            "AREA",
            (0.0, -3.0, 2.5),
            (70.0, 0.0, 0.0),
            energy=40.0,
            size=3.0,
            color=(0.85, 0.9, 1.0),
        ),
        negative_fill=LightSpec(
            "BVMCP_NegFill",
            "AREA",
            (0.0, 4.0, 1.0),
            (90.0, 0.0, 180.0),
            energy=15.0,
            size=4.0,
            color=(0.6, 0.65, 0.8),
            negative=True,
        ),
        rim=LightSpec(
            "BVMCP_Rim",
            "SPOT",
            (3.0, 2.0, 3.5),
            (55.0, 0.0, -40.0),
            energy=120.0,
            size=1.0,
            angle_deg=55.0,
            color=(0.7, 0.85, 1.0),
        ),
        environment_color=(0.08, 0.1, 0.14),
        environment_strength=0.2,
        reflection_cards=[],
        shadow_softness=0.4,
        exposure=0.15,
        white_balance_k=7200.0,
        tone_map="AgX",
        atmosphere={"fog_density": 0.02, "haze": 0.08, "depth_falloff": 0.12},
        description="Datacenter corridor: overhead cool wash, long falloff, slight haze.",
    )


def _soft_organic() -> LightingRig:
    return LightingRig(
        rig_class=RigClass.SOFT_ORGANIC,
        key=LightSpec(
            "BVMCP_Key",
            "AREA",
            (2.0, -2.5, 2.2),
            (42.0, 0.0, 25.0),
            energy=140.0,
            size=3.5,
            color=(1.0, 0.94, 0.88),
        ),
        fill=LightSpec(
            "BVMCP_Fill",
            "AREA",
            (-2.5, -1.0, 1.8),
            (50.0, 0.0, -50.0),
            energy=90.0,
            size=4.0,
            color=(0.95, 0.96, 1.0),
        ),
        negative_fill=LightSpec(
            "BVMCP_NegFill",
            "AREA",
            (0.5, 2.8, 1.0),
            (85.0, 0.0, 180.0),
            energy=10.0,
            size=3.0,
            color=(0.9, 0.9, 0.95),
            negative=True,
        ),
        rim=LightSpec(
            "BVMCP_Rim",
            "AREA",
            (-1.0, 2.0, 2.8),
            (60.0, 0.0, 160.0),
            energy=50.0,
            size=2.0,
            color=(1.0, 0.96, 0.9),
        ),
        environment_color=(0.75, 0.72, 0.68),
        environment_strength=0.55,
        reflection_cards=[
            ReflectionCard(
                "BVMCP_Card_Bounce", (0.0, -3.0, 0.2), (3.0, 0.1, 2.0),
                color=(1.0, 0.95, 0.9),
                emission=0.6,
            )
        ],
        shadow_softness=0.65,
        exposure=0.25,
        white_balance_k=5200.0,
        tone_map="AgX",
        atmosphere={"fog_density": 0.01, "haze": 0.04},
        description="Soft organic: large sources, warm key, gentle falloff.",
    )


_REGISTRY: dict[str, Callable[[], LightingRig]] = {
    RigClass.NEUTRAL_DOCUMENTATION.value: _neutral_documentation,
    RigClass.BLACK_PRODUCT_STUDIO.value: _black_product_studio,
    RigClass.DATACENTER_CORRIDOR.value: _datacenter_corridor,
    RigClass.SOFT_ORGANIC.value: _soft_organic,
}


def list_rigs() -> list[str]:
    return list(RIG_NAMES)


def get_rig(name: str) -> LightingRig:
    key = str(name).strip()
    if key not in _REGISTRY:
        raise KeyError(f"unknown lighting rig: {name!r}; known={list(RIG_NAMES)}")
    return _REGISTRY[key]()


def apply_rig_script(name: str) -> str:
    """Full Blender script text that realises the named rig."""
    return get_rig(name).blender_script_body()
