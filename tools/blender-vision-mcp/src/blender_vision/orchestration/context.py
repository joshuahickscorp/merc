from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.projects.store import ProjectStore


class ContextPacketStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def create(
        self,
        *,
        target_component: str,
        allowed_operations: list[str],
        desired_gate: str,
        campaign_id: str | None = None,
    ) -> dict[str, Any]:
        with self.project.connection() as connection:
            component = connection.execute(
                "SELECT record_json FROM components WHERE id=?", (target_component,)
            ).fetchone()
            feature_rows = connection.execute(
                "SELECT record_json FROM features WHERE parent_component=? ORDER BY created_at",
                (target_component,),
            ).fetchall()
            references = connection.execute(
                "SELECT id,viewpoint_label,quality_json FROM reference_items ORDER BY created_at"
            ).fetchall()
            measurements = connection.execute(
                "SELECT id,type,value_json,uncertainty_json FROM measurements ORDER BY created_at"
            ).fetchall()
            comparisons = connection.execute(
                "SELECT id,reference_id,residual_digest,metrics_json FROM comparisons "
                "ORDER BY created_at DESC LIMIT 20"
            ).fetchall()
            candidates = connection.execute(
                "SELECT id,lane,status,record_json FROM reconstruction_candidates "
                "ORDER BY created_at DESC LIMIT 20"
            ).fetchall()
            camera_rows = connection.execute(
                "SELECT id,solution_json,diagnostics_json FROM camera_solutions "
                "ORDER BY created_at DESC LIMIT 20"
            ).fetchall()
            mask_rows = connection.execute(
                "SELECT id,reference_id,artifact_digest,roi_json FROM reference_masks "
                "ORDER BY created_at DESC"
            ).fetchall()
        component_record = json.loads(component["record_json"]) if component else None
        feature_records = [json.loads(row["record_json"]) for row in feature_rows]
        evidence_bindings = set((component_record or {}).get("evidence_bindings", []))
        measurement_records = [
            {
                "id": row["id"],
                "type": row["type"],
                "value": json.loads(row["value_json"]),
                "uncertainty": json.loads(row["uncertainty_json"]),
            }
            for row in measurements
        ]
        relevant_measurements = [
            item for item in measurement_records if item["id"] in evidence_bindings
        ]
        relevant_reference_ids = {
            str(reference_id)
            for feature in feature_records
            for reference_id in feature.get("reference_ids", [])
        }
        relevant_reference_ids.update(
            str(reference_id)
            for measurement in relevant_measurements
            for reference_id in measurement["value"].get("reference_ids", [])
        )
        relevant_reference_ids.update(
            str(binding)
            for binding in evidence_bindings
            if any(row["id"] == binding for row in references)
        )
        relevant_comparisons = [
            row for row in comparisons if row["reference_id"] in relevant_reference_ids
        ]
        relevant_cameras = []
        for row in camera_rows:
            solution = json.loads(row["solution_json"])
            cameras = [
                item
                for item in solution.get("cameras", [])
                if item.get("reference_id") in relevant_reference_ids
            ]
            if cameras:
                relevant_cameras.append(
                    {
                        "solution_id": row["id"],
                        "cameras": cameras,
                        "diagnostics": json.loads(row["diagnostics_json"]),
                    }
                )
        packet_id = str(uuid.uuid4())
        packet = {
            "schema_version": 1,
            "id": packet_id,
            "campaign_id": campaign_id,
            "target_component": target_component,
            "current_parameters": component_record,
            "relevant_references": [
                {
                    "id": row["id"],
                    "viewpoint": row["viewpoint_label"],
                    "quality": json.loads(row["quality_json"]),
                }
                for row in references
                if row["id"] in relevant_reference_ids
            ],
            "reference_crops": [
                {
                    "mask_id": row["id"],
                    "reference_id": row["reference_id"],
                    "artifact_digest": row["artifact_digest"],
                    "roi": json.loads(row["roi_json"]),
                }
                for row in mask_rows
                if row["reference_id"] in relevant_reference_ids
            ],
            "cropped_comparisons": [
                {
                    "id": row["id"],
                    "reference_id": row["reference_id"],
                    "residual_digest": row["residual_digest"],
                    "metrics": json.loads(row["metrics_json"]),
                }
                for row in relevant_comparisons
            ],
            "residual_heatmaps": [
                row["residual_digest"]
                for row in relevant_comparisons
                if row["residual_digest"]
            ],
            "measurements": relevant_measurements,
            "camera_metrics": relevant_cameras,
            "component_features": feature_records,
            "candidate_history": [
                record
                for row in candidates
                if target_component
                in json.dumps(record := json.loads(row["record_json"]), sort_keys=True)
            ],
            "allowed_operations": sorted(set(allowed_operations)),
            "desired_gate": desired_gate,
            "created_at": utc_now(),
            "whole_project_included": False,
        }
        relative = f"comparisons/context-packet-{packet_id}.json"
        destination = self.project.root / relative
        atomic_write_json(destination, packet)
        artifact = self.artifacts.ingest_file(
            destination, media_type="application/vnd.bvmcp.context-packet+json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO context_packets(id,campaign_id,component_id,packet_json,"
                "artifact_digest,created_at) VALUES(?,?,?,?,?,?)",
                (
                    packet_id,
                    campaign_id,
                    target_component,
                    json.dumps(packet),
                    artifact.digest,
                    packet["created_at"],
                ),
            )
        return {**packet, "artifact": artifact.to_dict(), "path": relative}
