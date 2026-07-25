from __future__ import annotations

import json
import uuid
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.projects.store import ProjectStore

CHANGE_POLICIES: dict[str, dict[str, list[str]]] = {
    "geometry": {
        "passes": ["beauty", "silhouette", "depth", "normal", "object_id", "feature_id"],
        "metrics": ["silhouette_iou", "dice", "edge_residual", "depth", "normal"],
    },
    "topology": {
        "passes": ["silhouette", "depth", "normal", "object_id", "feature_id"],
        "metrics": ["silhouette_iou", "edge_residual", "depth", "normal", "topology"],
    },
    "appearance": {
        "passes": [
            "beauty",
            "appearance",
            "exposure_minus_2",
            "exposure_0",
            "exposure_plus_2",
            "material_neutral",
        ],
        "metrics": ["appearance", "material", "highlight", "color", "texture"],
    },
    "camera": {
        "passes": ["beauty", "silhouette", "object_id"],
        "metrics": ["reprojection", "silhouette_iou", "edge_residual"],
    },
}


class LocalityPlanner:
    """Create evidence-bound minimal recomputation plans for semantic changes."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def plan(
        self,
        semantic_ids: list[str],
        *,
        change_kind: str,
        camera_solution_id: str | None = None,
    ) -> dict[str, Any]:
        if not semantic_ids or len(set(semantic_ids)) != len(semantic_ids):
            raise ValueError("locality planning requires unique semantic ids")
        if change_kind not in CHANGE_POLICIES:
            raise ValueError(f"unsupported locality change kind: {change_kind}")
        with self.project.connection() as connection:
            node_rows = connection.execute(
                "SELECT id,record_json FROM semantic_nodes WHERE id IN ({})".format(
                    ",".join("?" for _ in semantic_ids)
                ),
                semantic_ids,
            ).fetchall()
            eligible_references = {
                row[0]
                for row in connection.execute(
                    "SELECT id FROM reference_items WHERE media_type LIKE 'image/%' "
                    "AND acceptance_eligible=1"
                ).fetchall()
            }
            if camera_solution_id:
                camera_row = connection.execute(
                    "SELECT id,solution_json FROM camera_solutions WHERE id=?",
                    (camera_solution_id,),
                ).fetchone()
            else:
                camera_row = connection.execute(
                    "SELECT id,solution_json FROM camera_solutions ORDER BY created_at DESC LIMIT 1"
                ).fetchone()
            mask_rows = connection.execute(
                "SELECT reference_id,roi_json,created_at FROM reference_masks "
                "ORDER BY created_at DESC"
            ).fetchall()
        nodes_by_id = {row["id"]: json.loads(row["record_json"]) for row in node_rows}
        missing = sorted(set(semantic_ids) - set(nodes_by_id))
        if missing:
            raise KeyError(f"unknown semantic ids: {missing}")
        object_names: set[str] = set()
        component_ids: set[str] = set()
        bound_references: set[str] = set()
        scene_ids: set[str] = set()
        for semantic_id in semantic_ids:
            node = nodes_by_id[semantic_id]
            geometry = node.get("geometry") or {}
            object_names.update(str(item) for item in geometry.get("object_names", []))
            component_ids.update(str(item) for item in geometry.get("component_ids", []))
            if geometry.get("scene_id"):
                scene_ids.add(str(geometry["scene_id"]))
            bound_references.update(str(item) for item in node.get("references", []))
        if not object_names:
            raise ValueError("affected semantic nodes have no bound geometry")
        reference_reason = "semantic_node_reference_bindings"
        affected_references = bound_references & eligible_references
        if not affected_references:
            affected_references = set(eligible_references)
            reference_reason = "eligible_reference_fallback_no_component_specific_bindings"
        if camera_row is None:
            raise ValueError("locality planning requires a stored camera solution")
        camera_document = json.loads(camera_row["solution_json"])
        camera_references = {
            str(camera["reference_id"]) for camera in camera_document.get("cameras", [])
        }
        affected_references &= camera_references
        if not affected_references:
            raise ValueError("camera solution does not cover any affected eligible reference")
        regions: dict[str, dict[str, int]] = {}
        for row in mask_rows:
            reference_id = str(row["reference_id"])
            if reference_id in affected_references and reference_id not in regions:
                roi = json.loads(row["roi_json"])
                if all(int(roi.get(key, 0)) >= 0 for key in ("x", "y", "width", "height")):
                    regions[reference_id] = {
                        key: int(roi[key]) for key in ("x", "y", "width", "height")
                    }
        policy = CHANGE_POLICIES[change_kind]
        plan_id = str(uuid.uuid4())
        record = {
            "schema_version": 1,
            "id": plan_id,
            "created_at": utc_now(),
            "change_kind": change_kind,
            "semantic_ids": sorted(semantic_ids),
            "scene_ids": sorted(scene_ids),
            "object_names": sorted(object_names),
            "component_ids": sorted(component_ids),
            "camera_solution_id": camera_row["id"],
            "reference_ids": sorted(affected_references),
            "reference_selection_reason": reference_reason,
            "requested_passes": list(policy["passes"]),
            "requested_metrics": list(policy["metrics"]),
            "regions_by_reference": regions,
            "full_project_recompute": False,
            "execution_parameters": {
                "solution_id": camera_row["id"],
                "reference_ids": sorted(affected_references),
                "requested_passes": list(policy["passes"]),
                "regions_by_reference": regions,
            },
        }
        relative = f"comparisons/locality-plan-{plan_id}.json"
        destination = self.project.root / relative
        atomic_write_json(destination, record)
        artifact = self.artifacts.ingest_file(
            destination, media_type="application/vnd.bvmcp.locality-plan+json"
        )
        return {**record, "artifact": artifact.to_dict(), "path": relative}
