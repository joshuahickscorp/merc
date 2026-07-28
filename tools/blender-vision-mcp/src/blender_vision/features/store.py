from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import utc_now
from blender_vision.features.ontology import FeatureType, TechnicalFeature
from blender_vision.projects.store import ProjectStore


class FeatureStore:
    def __init__(self, project: ProjectStore):
        self.project = project

    def add(
        self,
        feature: TechnicalFeature | str,
        *,
        feature_id: str | None = None,
        parent_component: str | None = None,
        dimensions: dict[str, Any] | None = None,
        coordinate_frame: str = "canonical_mm",
        observations: list[dict[str, Any]] | None = None,
        reference_ids: list[str] | None = None,
        confidence: float = 0.0,
        uncertainty: dict[str, Any] | None = None,
        evidence_class: EvidenceClass = EvidenceClass.INFERRED_LOW_CONFIDENCE,
        model_revision: str | None = None,
        human_approval: bool = False,
        coverage_group: str | None = None,
        hero_surface: bool = False,
        provenance: list[dict[str, Any]] | None = None,
        lifecycle_state: str = "active",
    ) -> dict[str, Any]:
        if isinstance(feature, TechnicalFeature):
            technical_feature = feature
        else:
            technical_feature = TechnicalFeature(
                id=feature_id or uuid.uuid4().hex,
                type=FeatureType(feature),
                parent_component=parent_component,
                coordinate_frame=coordinate_frame,
                observations=observations or [],
                reference_ids=reference_ids or [],
                confidence=confidence,
                evidence_class=evidence_class,
                uncertainty=uncertainty or {},
                dimensions=dimensions or {},
                model_revision=model_revision,
                human_approval=human_approval,
                coverage_group=coverage_group,
                hero_surface=hero_surface,
                provenance=provenance or [],
                lifecycle_state=lifecycle_state,
            )
        created_at = utc_now()
        record = {**technical_feature.to_dict(), "created_at": created_at}
        record["feature_type"] = record["type"]
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO features(id,type,parent_component,record_json,created_at) "
                "VALUES(?,?,?,?,?)",
                (
                    technical_feature.id,
                    technical_feature.type.value,
                    technical_feature.parent_component,
                    json.dumps(record),
                    created_at,
                ),
            )
        return record

    def supersede(self, feature_id: str, *, replacement_id: str, reason: str) -> dict[str, Any]:
        if not reason.strip():
            raise ValueError("feature supersession requires a reason")
        self.get(replacement_id)
        record = self.get(feature_id)
        record["lifecycle_state"] = "superseded"
        record["superseded_by"] = replacement_id
        record["supersession"] = {"reason": reason.strip(), "superseded_at": utc_now()}
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE features SET record_json=? WHERE id=?",
                (json.dumps(record), feature_id),
            )
        return record

    def get(self, feature_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM features WHERE id=?", (feature_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown feature: {feature_id}")
        return json.loads(row["record_json"])

    def review(
        self,
        feature_id: str,
        *,
        approved: bool,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        """Persist a named human decision; approval is never inferred from confidence."""
        if not reviewer.strip():
            raise ValueError("feature review requires a named reviewer")
        if not reason.strip():
            raise ValueError("feature review requires a reason")
        record = self.get(feature_id)
        if approved and not (record.get("observations") or record.get("reference_ids")):
            raise ValueError("feature approval requires at least one evidence binding")
        record["human_approval"] = approved
        record["approval"] = {
            "state": "approved" if approved else "rejected",
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": utc_now(),
        }
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE features SET record_json=? WHERE id=?",
                (json.dumps(record), feature_id),
            )
        return record

    def link_observation(
        self,
        feature_id: str,
        reference_id: str,
        observation: dict[str, Any],
        *,
        linked_by: str,
        reason: str,
    ) -> dict[str, Any]:
        """Link another view to a feature while preserving named cross-view provenance."""
        if not linked_by.strip() or not reason.strip():
            raise ValueError("feature linking requires a named reviewer and reason")
        with self.project.connection() as connection:
            reference = connection.execute(
                "SELECT id FROM reference_items WHERE id=? AND media_type LIKE 'image/%'",
                (reference_id,),
            ).fetchone()
        if reference is None:
            raise ValueError("feature link requires an image reference")
        if not isinstance(observation, dict) or not observation:
            raise ValueError("feature link requires a non-empty observation")
        record = self.get(feature_id)
        record["reference_ids"] = sorted(set(record.get("reference_ids", [])) | {reference_id})
        record.setdefault("observations", []).append(
            {**observation, "reference_id": reference_id, "linked_at": utc_now()}
        )
        record.setdefault("cross_view_link_history", []).append(
            {
                "reference_id": reference_id,
                "linked_by": linked_by.strip(),
                "reason": reason.strip(),
                "linked_at": utc_now(),
            }
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE features SET record_json=? WHERE id=?",
                (json.dumps(record), feature_id),
            )
        return record

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM features ORDER BY created_at"
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]
