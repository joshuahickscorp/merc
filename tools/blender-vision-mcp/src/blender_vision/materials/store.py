from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import utc_now
from blender_vision.projects.store import ProjectStore

SCALAR_PROPERTIES = ("roughness", "metallic", "anisotropy", "clearcoat")


class MaterialStore:
    """Keep appearance evidence separate from geometry authority and review."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def create(
        self,
        region_id: str,
        properties: dict[str, Any],
        *,
        evidence_class: EvidenceClass,
        confidence: float,
        reference_ids: list[str] | None = None,
        artifact_digests: list[str] | None = None,
        component_id: str | None = None,
        material_slot: str | None = None,
        uncertainty: dict[str, Any] | None = None,
        color_calibration: dict[str, Any] | None = None,
        lighting_estimate: dict[str, Any] | None = None,
        reflective_region_masks: list[str] | None = None,
        multi_light_reference_ids: list[str] | None = None,
        supersedes_id: str | None = None,
        notes: str | None = None,
    ) -> dict[str, Any]:
        if not region_id.strip():
            raise ValueError("material profile requires a region identifier")
        if not 0.0 <= confidence <= 1.0:
            raise ValueError("material confidence must be between zero and one")
        normalized = self._validate_properties(properties)
        reference_ids = sorted(set(reference_ids or []))
        artifact_digests = sorted(set(artifact_digests or []))
        reflective_region_masks = sorted(set(reflective_region_masks or []))
        multi_light_reference_ids = sorted(set(multi_light_reference_ids or []))
        all_references = sorted(set(reference_ids) | set(multi_light_reference_ids))
        all_artifacts = sorted(set(artifact_digests) | set(reflective_region_masks))
        with self.project.connection() as connection:
            known_references = {
                row[0] for row in connection.execute("SELECT id FROM reference_items")
            }
            known_artifacts = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
            known_components = {row[0] for row in connection.execute("SELECT id FROM components")}
            known_profiles = {
                row[0] for row in connection.execute("SELECT id FROM material_profiles")
            }
        if not set(all_references).issubset(known_references):
            raise ValueError("material profile references unknown image evidence")
        if not set(all_artifacts).issubset(known_artifacts):
            raise ValueError("material profile references unknown artifacts")
        if component_id and component_id not in known_components:
            raise ValueError("material profile references an unknown component")
        if supersedes_id and supersedes_id not in known_profiles:
            raise ValueError("material profile supersedes an unknown profile")
        profile_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "schema_version": 1,
            "id": profile_id,
            "region_id": region_id.strip(),
            "component_id": component_id,
            "material_slot": material_slot,
            "properties": normalized,
            "evidence": {
                "reference_ids": reference_ids,
                "artifact_digests": artifact_digests,
                "reflective_region_mask_digests": reflective_region_masks,
                "multi_light_reference_ids": multi_light_reference_ids,
                "evidence_class": evidence_class.value,
            },
            "confidence": float(confidence),
            "uncertainty": uncertainty or {},
            "color_calibration": color_calibration or {"state": "unreported"},
            "lighting_estimate": lighting_estimate or {"state": "unreported"},
            "authority": {
                "appearance_only": True,
                "may_establish_geometry": False,
                "rgb_excluded_from_geometry_gates": True,
                "geometry_passes": ["mask", "depth", "normal", "feature"],
            },
            "status": "proposed",
            "approval": {"state": "pending"},
            "supersedes_id": supersedes_id,
            "notes": notes,
            "created_at": now,
            "updated_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO material_profiles("
                "id,region_id,component_id,material_slot,status,record_json,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?)",
                (
                    profile_id,
                    record["region_id"],
                    component_id,
                    material_slot,
                    "proposed",
                    json.dumps(record),
                    now,
                    now,
                ),
            )
        return record

    def review(
        self, profile_id: str, *, approved: bool, reviewer: str, reason: str
    ) -> dict[str, Any]:
        if not reviewer.strip() or not reason.strip():
            raise ValueError("material review requires a named reviewer and reason")
        record = self.get(profile_id)
        if record["status"] != "proposed":
            raise ValueError("only a proposed material profile can be reviewed")
        evidence = record["evidence"]
        if approved and not (
            evidence["reference_ids"]
            or evidence["artifact_digests"]
            or evidence["multi_light_reference_ids"]
        ):
            raise ValueError("material approval requires bound evidence")
        now = utc_now()
        status = "approved" if approved else "rejected"
        record["status"] = status
        record["approval"] = {
            "state": status,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": now,
        }
        record["updated_at"] = now
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE material_profiles SET status=?,record_json=?,updated_at=? WHERE id=?",
                (status, json.dumps(record), now, profile_id),
            )
        return record

    def get(self, profile_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM material_profiles WHERE id=?", (profile_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown material profile: {profile_id}")
        return json.loads(row["record_json"])

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM material_profiles ORDER BY created_at"
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]

    @staticmethod
    def _validate_properties(properties: dict[str, Any]) -> dict[str, Any]:
        if not isinstance(properties, dict):
            raise ValueError("material properties must be a JSON object")
        allowed = {
            "base_color",
            *SCALAR_PROPERTIES,
            "normal_detail",
            "procedural_texture",
        }
        unknown = set(properties) - allowed
        if unknown:
            raise ValueError("unknown material properties: " + ", ".join(sorted(unknown)))
        if "base_color" not in properties:
            raise ValueError("material properties require base_color")
        base_color = properties["base_color"]
        if (
            not isinstance(base_color, list)
            or len(base_color) not in {3, 4}
            or not all(
                isinstance(value, (int, float)) and 0.0 <= value <= 1.0 for value in base_color
            )
        ):
            raise ValueError("base_color must contain three or four normalized channels")
        normalized: dict[str, Any] = {"base_color": [float(value) for value in base_color]}
        for name in SCALAR_PROPERTIES:
            value = properties.get(name, 0.0 if name != "roughness" else 0.5)
            if not isinstance(value, (int, float)) or not 0.0 <= value <= 1.0:
                raise ValueError(f"{name} must be between zero and one")
            normalized[name] = float(value)
        for name in ("normal_detail", "procedural_texture"):
            value = properties.get(name, {})
            if not isinstance(value, dict):
                raise ValueError(f"{name} must be a JSON object")
            normalized[name] = value
        return normalized
