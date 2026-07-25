from __future__ import annotations

from typing import Any

from blender_vision.evidence.measurements import MeasurementStore
from blender_vision.projects.store import ProjectStore
from blender_vision.repairs.store import RepairStore

ROLE_TO_PARAMETER = {
    "rear_hero_grille_width": "field_width_mm",
    "rear_hero_grille_height": "field_height_mm",
    "rear_hero_grille_z_center": "z_center_mm",
    "rear_hero_grille_pitch": "pitch_mm",
    "rear_hero_grille_diameter": "diameter_mm",
}


def propose_mac_studio_grille(project: ProjectStore) -> dict[str, Any]:
    measurements = {item["value"].get("role"): item for item in MeasurementStore(project).list()}
    missing = sorted(role for role in ROLE_TO_PARAMETER if role not in measurements)
    if missing:
        raise ValueError(
            "Mac Studio grille proposal requires benchmark measurements: " + ", ".join(missing)
        )
    config = {
        parameter: float(measurements[role]["value"]["millimetres"])
        for role, parameter in ROLE_TO_PARAMETER.items()
    }
    config.update(
        {
            "body_object": "mac-studio",
            "existing_panel_object": "mac-vent-mesh",
            "target_hole_count": 2349,
            "panel_thickness_mm": 1.2,
            "recess_depth_mm": 15.0,
            "corner_radius_mm": 28.0,
            "minimum_open_fraction": 0.9,
        }
    )
    evidence = [
        {
            "kind": "measurement",
            "id": measurements[role]["id"],
            "role": role,
            "evidence_class": measurements[role]["evidence_class"],
            "uncertainty": measurements[role]["uncertainty"],
        }
        for role in ROLE_TO_PARAMETER
    ]
    with project.connection() as connection:
        legacy = connection.execute(
            "SELECT digest,source_name FROM artifacts WHERE source_name IN "
            "('B1_STATUS.md','diagnosis.json','grille_metrics.json') ORDER BY source_name"
        ).fetchall()
    evidence.extend(
        {"kind": "legacy_audit_artifact", "digest": row["digest"], "name": row["source_name"]}
        for row in legacy
    )
    return RepairStore(project).propose(
        "mac_studio_rear_hero_grille",
        config,
        evidence_bindings=evidence,
        expected_improvement={
            "through_holes": {"before": 0, "expected": 2349},
            "field_z_center_mm": {"before": 56.0, "expected": config["z_center_mm"]},
            "pitch_mm": {"before": 4.2, "expected": config["pitch_mm"]},
            "hole_diameter_mm": {"before": 2.8, "expected": config["diameter_mm"]},
            "placement": "+Y rear face in mac-studio body-local coordinates",
            "acceptance_effect": (
                "proposal remains non-authoritative until topology, rays, render, and human "
                "review pass"
            ),
        },
    )
