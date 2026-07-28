from __future__ import annotations

import re
from copy import deepcopy
from typing import Any

PACKS: dict[str, dict[str, Any]] = {
    "consumer_electronics": {
        "ontology": ["panel", "port", "button", "LED", "vent", "foot", "fastener"],
        "priors": ["bilateral_symmetry", "planar_panels", "repeated_ports"],
    },
    "computer_hardware": {
        "ontology": [
            "port",
            "connector",
            "fan",
            "blade",
            "grille",
            "vent",
            "fastener",
            "panel",
            "PCB",
            "heat_sink",
            "bracket",
            "button",
            "LED",
            "foot",
        ],
        "priors": ["repeated_patterns", "axis_alignment", "manufactured_symmetry"],
    },
    "vehicles": {
        "ontology": [
            "body_shell",
            "glasshouse",
            "door",
            "hood",
            "deck",
            "bumper",
            "fender",
            "splitter",
            "diffuser",
            "wing",
            "active_aero",
            "intake",
            "vent",
            "wheel",
            "tire",
            "brake_rotor",
            "caliper",
            "headlight",
            "taillight",
            "mirror",
            "exhaust",
            "underbody",
            "panel_seam",
            "fastener",
            "badge",
        ],
        "priors": [
            "bilateral_symmetry",
            "wheel_axis_constraints",
            "wheelbase",
            "track_width",
            "ground_plane",
            "tire_circle_constraints",
            "panel_continuity",
            "glass_continuity",
            "aero_attachment",
            "suspension_stance",
        ],
        "constructs": [
            "spline_body_section",
            "lofted_surface",
            "wheel_arch",
            "panel_cut",
            "panel_gap",
            "surface_crease",
            "aerofoil",
            "duct",
            "vent",
            "light_housing",
            "glass_panel",
            "tire_profile",
            "wheel_spoke_array",
            "brake_assembly",
            "diffuser_channel",
            "underbody_panel",
        ],
    },
    "space_rovers": {
        "ontology": [
            "chassis",
            "equipment_deck",
            "belly_pan",
            "wheel",
            "wheel_grouser",
            "rocker",
            "bogie",
            "differential",
            "steering_actuator",
            "robotic_arm",
            "turret",
            "drill",
            "sample_caching_system",
            "sample_tube",
            "remote_sensing_mast",
            "mastcam",
            "navcam",
            "hazcam_front",
            "hazcam_rear",
            "supercam",
            "high_gain_antenna",
            "low_gain_antenna",
            "uhf_antenna",
            "rtg",
            "meda_sensor",
            "moxie_inlet",
            "calibration_target",
            "cable_harness",
            "fastener",
            "nameplate",
        ],
        "priors": [
            "bilateral_mobility_layout",
            "six_wheel_axis_constraints",
            "rocker_bogie_kinematics",
            "ground_plane",
            "wheel_circle_constraints",
            "mast_axis",
            "robotic_arm_joint_hierarchy",
            "instrument_mount_continuity",
            "flight_configuration_identity",
        ],
        "constructs": [
            "box_section_chassis",
            "equipment_deck",
            "belly_pan",
            "wheel_spoke_array",
            "wheel_grouser_array",
            "rocker_bogie_linkage",
            "instrument_housing",
            "camera_housing",
            "antenna_dish",
            "robotic_arm_linkage",
            "cable_harness",
        ],
    },
    "packaging": {
        "ontology": [
            "fold",
            "flap",
            "seam",
            "cap",
            "spout",
            "label",
            "barcode",
            "print_region",
            "emboss",
            "material_layer",
        ],
        "priors": ["developable_surfaces", "print_registration", "layered_material"],
    },
    "furniture": {
        "ontology": ["frame", "leg", "seat", "back", "joint", "fastener", "upholstery"],
        "priors": ["ground_plane", "bilateral_symmetry", "load_bearing_continuity"],
    },
    "architecture": {
        "ontology": ["wall", "floor", "roof", "door", "window", "column", "fixture"],
        "priors": ["gravity_alignment", "orthogonality", "level_hierarchy"],
    },
    "organic_creatures": {
        "ontology": [
            "skeletal_landmark",
            "joint",
            "surface_anatomy",
            "eye",
            "ear",
            "muzzle",
            "horn",
            "hoof",
            "mane",
            "tail",
            "muscle_group",
        ],
        "priors": ["bilateral_symmetry", "joint_hierarchy", "anatomical_continuity", "pose"],
        "subject_classes": ["generated_creature", "statue_reconstruction", "animal_reconstruction"],
    },
    "general_product": {
        "ontology": ["body", "panel", "opening", "fastener", "label", "foot"],
        "priors": ["manufactured_symmetry", "ground_plane"],
    },
}


def _mentions(text: str, terms: tuple[str, ...]) -> bool:
    return any(
        re.search(rf"(?<![a-z0-9]){re.escape(term)}(?![a-z0-9])", text) is not None
        for term in terms
    )


class CategoryPackRegistry:
    def list(self) -> list[dict[str, Any]]:
        return [{"id": name, **deepcopy(record)} for name, record in sorted(PACKS.items())]

    def get(self, name: str) -> dict[str, Any]:
        if name not in PACKS:
            raise KeyError(f"unknown category pack: {name}")
        return {"id": name, **deepcopy(PACKS[name])}

    def select(self, target: dict[str, Any]) -> dict[str, Any]:
        text = " ".join(str(value) for value in target.values() if value).lower()
        if _mentions(text, ("perseverance", "mars rover", "space rover")):
            selected = "space_rovers"
        elif _mentions(text, ("car", "vehicle", "porsche", "ferrari", "bmw", "audi")):
            selected = "vehicles"
        elif _mentions(
            text,
            (
                "gpu",
                "dgx",
                "computer",
                "graphics card",
                "graphics processing unit",
                "geforce",
                "rtx",
                "founders edition",
                "server",
            ),
        ):
            selected = "computer_hardware"
        elif _mentions(text, ("carton", "bottle", "package", "box")):
            selected = "packaging"
        elif _mentions(text, ("unicorn", "creature", "animal")):
            selected = "organic_creatures"
        elif _mentions(text, ("phone", "tablet", "studio", "electronics")):
            selected = "consumer_electronics"
        else:
            selected = "general_product"
        return self.get(selected)
