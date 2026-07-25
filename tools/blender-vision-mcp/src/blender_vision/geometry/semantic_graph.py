from __future__ import annotations

import json
import re
import uuid
from typing import Any

from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.evidence.targets import TargetResolver
from blender_vision.intelligence.packs import CategoryPackRegistry
from blender_vision.projects.store import ProjectStore

ROVER_COMPONENTS = [
    "chassis",
    "rocker_bogie_suspension",
    "instrument_mast",
    "navigation_camera",
    "hazard_camera",
    "robotic_arm",
    "sample_caching_system",
    "power_system",
    "radioisotope_power_source",
    "high_gain_antenna",
]


def _semantic_id(value: str) -> str:
    normalized = re.sub(r"[^a-z0-9]+", "_", value.lower()).strip("_")
    if not normalized:
        raise ValueError("semantic identifier cannot be blank")
    return normalized


class SemanticTwinGraph:
    def __init__(self, project: ProjectStore):
        self.project = project

    def bootstrap(
        self, *, category: str | None = None, target_id: str | None = None
    ) -> dict[str, Any]:
        target = TargetResolver(self.project).get(target_id)
        pack = (
            CategoryPackRegistry().get(category)
            if category
            else CategoryPackRegistry().select(target["target"])
        )
        root_id = f"target_{target['id'].replace('-', '')[:12]}"
        root = self._create_node(
            root_id,
            None,
            "digital_twin_root",
            parameters={"canonical_target_id": target["id"], "category": pack["id"]},
            constraints=pack.get("priors", []),
        )
        nodes = [root]
        ontology = list(pack["ontology"])
        target_text = json.dumps(target["target"], sort_keys=True).lower()
        if pack["id"] == "vehicles" and "rover" in target_text:
            ontology.extend(ROVER_COMPONENTS)
        for item in dict.fromkeys(ontology):
            nodes.append(
                self._create_node(
                    f"{root_id}_{_semantic_id(item)}",
                    root_id,
                    item,
                    parameters={},
                    constraints=[],
                )
            )
        graph = self.export(root_id)
        atomic_write_json(self.project.root / "geometry" / "semantic-twin-graph.json", graph)
        return graph

    def ensure_component_nodes(
        self, root_id: str, component_types: list[str]
    ) -> dict[str, Any]:
        """Idempotently extend an existing twin with target-specific semantic components."""
        root = self.get(root_id)
        if root["type"] != "digital_twin_root":
            raise ValueError("semantic extension root must be a digital twin root")
        graph = self.export(root_id)
        existing_types = {
            item["type"] for item in graph["nodes"] if item.get("parent_id") == root_id
        }
        created = []
        for component_type in dict.fromkeys(
            str(item).strip() for item in component_types if str(item).strip()
        ):
            if component_type in existing_types:
                continue
            created.append(
                self._create_node(
                    f"{root_id}_{_semantic_id(component_type)}",
                    root_id,
                    component_type,
                    parameters={},
                    constraints=[],
                )
            )
            existing_types.add(component_type)
        value = self.export(root_id)
        atomic_write_json(self.project.root / "geometry" / "semantic-twin-graph.json", value)
        return {**value, "created_node_ids": [item["id"] for item in created]}

    def bind(
        self,
        semantic_id: str,
        *,
        scene_id: str,
        object_names: list[str],
        reference_ids: list[str] | None = None,
        component_ids: list[str] | None = None,
        confidence: float,
    ) -> dict[str, Any]:
        if not object_names:
            raise ValueError("semantic geometry binding requires at least one object")
        if not 0.0 <= confidence <= 1.0:
            raise ValueError("semantic binding confidence must be between zero and one")
        component_ids = sorted(set(component_ids or []))
        if component_ids:
            with self.project.connection() as connection:
                known_components = {
                    row[0]
                    for row in connection.execute("SELECT id FROM components").fetchall()
                }
            unknown_components = sorted(set(component_ids) - known_components)
            if unknown_components:
                raise KeyError(f"unknown component ids: {unknown_components}")
        node = self.get(semantic_id)
        node["geometry"] = {
            "scene_id": scene_id,
            "object_names": sorted(set(object_names)),
            "component_ids": component_ids,
        }
        node["references"] = sorted(set(reference_ids or []))
        node["confidence"] = confidence
        node["acceptance_state"] = "pending"
        node["updated_at"] = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE semantic_nodes SET record_json=?,updated_at=? WHERE id=?",
                (json.dumps(node), node["updated_at"], semantic_id),
            )
        return node

    def review(
        self,
        semantic_id: str,
        *,
        acceptance_state: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        if acceptance_state not in {"accepted", "rejected", "not_applicable"}:
            raise ValueError("semantic review state is invalid")
        if not reviewer.strip() or not reason.strip():
            raise ValueError("semantic review requires a named reviewer and reason")
        node = self.get(semantic_id)
        if node["type"] == "digital_twin_root" and acceptance_state == "not_applicable":
            raise ValueError("digital twin root cannot be marked not applicable")
        if acceptance_state == "accepted" and not node.get("geometry"):
            raise ValueError("semantic geometry must be bound before acceptance")
        node["acceptance_state"] = acceptance_state
        node["review"] = {
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": utc_now(),
        }
        node["updated_at"] = node["review"]["reviewed_at"]
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE semantic_nodes SET record_json=?,updated_at=? WHERE id=?",
                (json.dumps(node), node["updated_at"], semantic_id),
            )
        return node

    def _create_node(
        self,
        node_id: str,
        parent_id: str | None,
        node_type: str,
        *,
        parameters: dict[str, Any],
        constraints: list[str],
    ) -> dict[str, Any]:
        now = utc_now()
        record = {
            "id": node_id,
            "parent_id": parent_id,
            "type": node_type,
            "geometry": None,
            "parameters": parameters,
            "constraints": constraints,
            "materials": [],
            "references": [],
            "observations": [],
            "confidence": 0.0,
            "acceptance_state": "pending",
            "lod_policies": {},
            "created_at": now,
            "updated_at": now,
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO semantic_nodes(id,parent_id,node_type,record_json,created_at,"
                "updated_at) VALUES(?,?,?,?,?,?)",
                (node_id, parent_id, node_type, json.dumps(record), now, now),
            )
            if parent_id:
                edge_id = str(uuid.uuid4())
                edge = {
                    "id": edge_id,
                    "source": parent_id,
                    "target": node_id,
                    "relation": "contains",
                }
                connection.execute(
                    "INSERT INTO semantic_edges(id,source_id,target_id,relation,record_json,"
                    "created_at) VALUES(?,?,?,?,?,?)",
                    (edge_id, parent_id, node_id, "contains", json.dumps(edge), now),
                )
        return record

    def get(self, semantic_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM semantic_nodes WHERE id=?", (semantic_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown semantic node: {semantic_id}")
        return json.loads(row["record_json"])

    def export(self, root_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM semantic_nodes ORDER BY created_at,id"
            ).fetchall()
            edges = connection.execute(
                "SELECT record_json FROM semantic_edges ORDER BY created_at,id"
            ).fetchall()
        nodes = [json.loads(row["record_json"]) for row in rows]
        edge_records = [json.loads(row["record_json"]) for row in edges]
        if root_id is not None:
            if not any(item["id"] == root_id for item in nodes):
                raise KeyError(f"unknown semantic root: {root_id}")
            selected = {root_id}
            changed = True
            while changed:
                before = len(selected)
                selected.update(
                    item["id"] for item in nodes if item.get("parent_id") in selected
                )
                changed = len(selected) != before
            nodes = [item for item in nodes if item["id"] in selected]
            edge_records = [
                item
                for item in edge_records
                if item.get("source") in selected and item.get("target") in selected
            ]
        return {
            "schema_version": 1,
            "root_id": root_id
            or next((item["id"] for item in nodes if not item["parent_id"]), None),
            "nodes": nodes,
            "edges": edge_records,
            "operations_require_semantic_ids": True,
        }
