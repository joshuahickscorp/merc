from __future__ import annotations

import hashlib
import json
import re
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore

BINDING_STATES = {
    "UNBOUND",
    "PROVISIONALLY_BOUND",
    "REVIEWED_BOUND",
    "ACCEPTED_BOUND",
}
ASSEMBLY_RELATIONS = {
    "PART_OF",
    "ATTACHED_TO",
    "RECESSED_IN",
    "CUTS_THROUGH",
    "CONTAINED_BY",
    "FASTENED_TO",
    "OVERLAPS",
    "ALIGNED_WITH",
    "ARRAYED_WITH",
}


def _slug(value: str) -> str:
    normalized = re.sub(r"[^a-z0-9]+", "_", value.lower()).strip("_")
    return normalized or "unnamed"


class SemanticBindingStore:
    """Object-level semantic binding and assembly hierarchy for visible scene geometry."""

    SCHEMA_VERSION = 1

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def propose_scene(
        self,
        scene_id: str,
        *,
        reference_ids: list[str] | None = None,
        classifications: dict[str, dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        inventory = scene.get("inventory")
        if not isinstance(inventory, dict):
            raise ValueError("semantic binding requires a governed scene inventory")
        objects = [
            item
            for item in inventory.get("objects", [])
            if item.get("type") == "MESH" and not item.get("hidden_render", False)
        ]
        if not objects:
            raise ValueError("semantic binding requires at least one visible mesh object")
        classifications = classifications or {}
        unknown = sorted(set(classifications) - {str(item.get("name")) for item in objects})
        if unknown:
            raise KeyError(f"classifications reference unknown visible objects: {unknown}")
        evaluation_views = self._evaluation_views(scene_id, reference_ids)
        root_id = self._ensure_node(
            f"scene_{scene_id.replace('-', '')[:12]}_visual_twin",
            parent_id=None,
            node_type="digital_twin_root",
            relation=None,
            parameters={"scene_id": scene_id, "scene_artifact_digest": scene["artifact_digest"]},
        )
        results = []
        for obj in sorted(objects, key=lambda item: str(item.get("name", ""))):
            name = str(obj.get("name", "")).strip()
            if not name:
                raise ValueError("visible mesh object has no stable name")
            override = classifications.get(name, {})
            diagnosis = self._classify(name, override)
            assembly_id = self._ensure_node(
                f"{root_id}_{_slug(diagnosis['parent_assembly'])}",
                parent_id=root_id,
                node_type=diagnosis["parent_assembly"],
                relation="PART_OF",
                parameters={"assembly_role": diagnosis["parent_assembly"]},
            )
            semantic_id = self._ensure_node(
                f"{root_id}_{_slug(name)}",
                parent_id=assembly_id,
                node_type=diagnosis["semantic_component"],
                relation="PART_OF",
                parameters={"object_name": name},
            )
            results.append(
                self._propose_binding(
                    scene=scene,
                    obj=obj,
                    semantic_id=semantic_id,
                    parent_assembly_id=assembly_id,
                    diagnosis=diagnosis,
                    evaluation_views=evaluation_views,
                    override=override,
                )
            )
        coverage = self.coverage(scene_id)
        return {
            "scene_id": scene_id,
            "root_id": root_id,
            "bindings": results,
            "coverage": coverage,
            "authority": "MACHINE_SEMANTIC_PROPOSALS_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY",
        }

    def review(
        self,
        binding_id: str,
        *,
        state: str,
        reviewer: str,
        reason: str,
    ) -> dict[str, Any]:
        if state not in {"UNBOUND", "REVIEWED_BOUND", "ACCEPTED_BOUND"}:
            raise ValueError(
                "binding review state must be UNBOUND, REVIEWED_BOUND, or ACCEPTED_BOUND"
            )
        if not reviewer.strip() or not reason.strip():
            raise ValueError("semantic binding review requires a named reviewer and reason")
        binding = self.get(binding_id)
        verification = self.verify(binding_id)
        if not verification["valid"]:
            raise ValueError("semantic binding proposal is invalid or stale")
        current = binding["state"]
        allowed = {
            "UNBOUND": {"REVIEWED_BOUND"},
            "PROVISIONALLY_BOUND": {"UNBOUND", "REVIEWED_BOUND"},
            "REVIEWED_BOUND": {"UNBOUND", "ACCEPTED_BOUND"},
            "ACCEPTED_BOUND": set(),
        }
        if state not in allowed[current]:
            raise ValueError(f"invalid semantic binding transition: {current} -> {state}")
        reviewed_at = utc_now()
        decision = {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "semantic_geometry_binding_decision",
            "binding_id": binding_id,
            "proposal_digest": binding["proposal_digest"],
            "scene_id": binding["scene_id"],
            "scene_artifact_digest": binding["record"]["scene_artifact_digest"],
            "object_name": binding["object_name"],
            "semantic_id": binding["semantic_id"],
            "from_state": current,
            "to_state": state,
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": reviewed_at,
            "authority": (
                "NAMED_SEMANTIC_BINDING_ACCEPTANCE"
                if state == "ACCEPTED_BOUND"
                else "NAMED_SEMANTIC_BINDING_REVIEW"
            ),
        }
        relative = Path("receipts") / f"semantic-binding-decision-{binding_id}-{state}.json"
        atomic_write_json(self.project.root / relative, decision)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.semantic-binding-decision+json",
        )
        record = dict(binding["record"])
        record["state"] = state
        record["review"] = {
            "reviewer": reviewer.strip(),
            "reason": reason.strip(),
            "reviewed_at": reviewed_at,
            "decision_digest": artifact.digest,
        }
        record["blockers"] = self._binding_blockers(record)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT state,proposal_digest FROM semantic_geometry_bindings WHERE id=?",
                (binding_id,),
            ).fetchone()
            if (
                row is None
                or row["state"] != current
                or row["proposal_digest"] != binding["proposal_digest"]
            ):
                raise RuntimeError("semantic binding changed during review")
            connection.execute(
                "UPDATE semantic_geometry_bindings SET state=?,record_json=?,decision_digest=?,"
                "updated_at=? WHERE id=?",
                (state, json.dumps(record), artifact.digest, reviewed_at, binding_id),
            )
            node_state = "accepted" if state == "ACCEPTED_BOUND" else "pending"
            node_row = connection.execute(
                "SELECT record_json FROM semantic_nodes WHERE id=?", (binding["semantic_id"],)
            ).fetchone()
            if node_row is None:
                raise RuntimeError("semantic binding node disappeared during review")
            node = json.loads(node_row["record_json"])
            node["binding_state"] = state
            node["acceptance_state"] = node_state
            node["updated_at"] = reviewed_at
            connection.execute(
                "UPDATE semantic_nodes SET record_json=?,updated_at=? WHERE id=?",
                (json.dumps(node), reviewed_at, binding["semantic_id"]),
            )
        return self.get(binding_id)

    def repropose(
        self,
        binding_id: str,
        *,
        reason: str,
        classification: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Supersede an unreviewed machine proposal after a classifier correction."""
        if not reason.strip():
            raise ValueError("semantic binding re-proposal requires a reason")
        binding = self.get(binding_id)
        if binding["state"] != "PROVISIONALLY_BOUND" or binding["decision_digest"] is not None:
            raise ValueError("only an unreviewed provisional binding can be re-proposed")
        if not self.verify(binding_id)["valid"]:
            raise ValueError("semantic binding proposal is invalid or stale")
        diagnosis = self._classify(binding["object_name"], classification or {})
        root_id = f"scene_{binding['scene_id'].replace('-', '')[:12]}_visual_twin"
        assembly_id = self._ensure_node(
            f"{root_id}_{_slug(diagnosis['parent_assembly'])}",
            parent_id=root_id,
            node_type=diagnosis["parent_assembly"],
            relation="PART_OF",
            parameters={"assembly_role": diagnosis["parent_assembly"]},
        )
        record = dict(binding["record"])
        record.update(
            {
                "semantic_component": diagnosis["semantic_component"],
                "parent_assembly_id": assembly_id,
                "parent_assembly": diagnosis["parent_assembly"],
                "confidence": float(diagnosis["confidence"]),
                "visual_importance": float(diagnosis["visual_importance"]),
                "classification_signals": diagnosis["signals"],
                "proposal_revision": int(record.get("proposal_revision", 1)) + 1,
                "previous_proposal_digest": binding["proposal_digest"],
                "reproposal_reason": reason.strip(),
                "state": "PROVISIONALLY_BOUND",
                "review": None,
            }
        )
        record["blockers"] = self._binding_blockers(record)
        revised_at = utc_now()
        proposal = self._proposal_receipt(
            binding_id=binding_id,
            record=record,
            created_at=revised_at,
        )
        relative = Path("receipts") / (
            f"semantic-binding-proposal-{binding_id}-r{record['proposal_revision']}.json"
        )
        atomic_write_json(self.project.root / relative, proposal)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.semantic-binding-proposal+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT state,proposal_digest,decision_digest FROM semantic_geometry_bindings "
                "WHERE id=?",
                (binding_id,),
            ).fetchone()
            if (
                current is None
                or current["state"] != "PROVISIONALLY_BOUND"
                or current["proposal_digest"] != binding["proposal_digest"]
                or current["decision_digest"] is not None
            ):
                raise RuntimeError("semantic binding changed during machine re-proposal")
            connection.execute(
                "UPDATE semantic_geometry_bindings SET parent_assembly_id=?,record_json=?,"
                "proposal_digest=?,updated_at=? WHERE id=?",
                (
                    assembly_id,
                    json.dumps(record),
                    artifact.digest,
                    revised_at,
                    binding_id,
                ),
            )
            node_row = connection.execute(
                "SELECT record_json FROM semantic_nodes WHERE id=?", (binding["semantic_id"],)
            ).fetchone()
            if node_row is None:
                raise RuntimeError("semantic node disappeared during machine re-proposal")
            node = json.loads(node_row["record_json"])
            node.update(
                {
                    "parent_id": assembly_id,
                    "type": diagnosis["semantic_component"],
                    "confidence": float(diagnosis["confidence"]),
                    "updated_at": revised_at,
                }
            )
            connection.execute(
                "UPDATE semantic_nodes SET parent_id=?,node_type=?,record_json=?,updated_at=? "
                "WHERE id=?",
                (
                    assembly_id,
                    diagnosis["semantic_component"],
                    json.dumps(node),
                    revised_at,
                    binding["semantic_id"],
                ),
            )
            for edge_row in connection.execute(
                "SELECT id,record_json FROM semantic_edges WHERE source_id=? "
                "AND relation='PART_OF'",
                (binding["semantic_id"],),
            ).fetchall():
                edge_record = json.loads(edge_row["record_json"])
                edge_record["state"] = "SUPERSEDED_BY_MACHINE_REPROPOSAL"
                connection.execute(
                    "UPDATE semantic_edges SET record_json=? WHERE id=?",
                    (json.dumps(edge_record), edge_row["id"]),
                )
            edge_id = str(uuid.uuid4())
            edge_record = {
                "id": edge_id,
                "source": binding["semantic_id"],
                "target": assembly_id,
                "relation": "PART_OF",
                "state": "STRUCTURAL_HIERARCHY",
                "proposal_digest": artifact.digest,
                "created_at": revised_at,
            }
            connection.execute(
                "INSERT INTO semantic_edges("
                "id,source_id,target_id,relation,record_json,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    edge_id,
                    binding["semantic_id"],
                    assembly_id,
                    "PART_OF",
                    json.dumps(edge_record),
                    revised_at,
                ),
            )
        return self.get(binding_id)

    def relate(
        self,
        source_binding_id: str,
        target_binding_id: str,
        *,
        relation: str,
        confidence: float,
        evidence: list[dict[str, Any]] | None = None,
    ) -> dict[str, Any]:
        if relation not in ASSEMBLY_RELATIONS - {"PART_OF"}:
            raise ValueError(f"unsupported assembly relation: {relation}")
        if not 0.0 <= confidence <= 1.0:
            raise ValueError("assembly relationship confidence must be between zero and one")
        source = self.get(source_binding_id)
        target = self.get(target_binding_id)
        if source["scene_id"] != target["scene_id"]:
            raise ValueError("assembly relationship cannot cross scenes")
        created_at = utc_now()
        relationship = {
            "id": str(uuid.uuid4()),
            "source": source["semantic_id"],
            "target": target["semantic_id"],
            "source_binding_id": source_binding_id,
            "target_binding_id": target_binding_id,
            "scene_id": source["scene_id"],
            "relation": relation,
            "confidence": float(confidence),
            "evidence": evidence or [],
            "state": "PROVISIONAL",
            "authority": "ASSEMBLY_RELATION_PROPOSAL_NO_ACCEPTANCE_AUTHORITY",
            "created_at": created_at,
        }
        relative = Path("receipts") / f"assembly-relation-{relationship['id']}.json"
        atomic_write_json(self.project.root / relative, relationship)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.assembly-relation+json",
        )
        relationship["receipt_digest"] = artifact.digest
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO semantic_edges("
                "id,source_id,target_id,relation,record_json,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    relationship["id"],
                    relationship["source"],
                    relationship["target"],
                    relation,
                    json.dumps(relationship),
                    created_at,
                ),
            )
        return relationship

    def list(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM semantic_geometry_bindings "
                + ("WHERE scene_id=? " if scene_id else "")
                + "ORDER BY scene_id,object_name,id",
                (scene_id,) if scene_id else (),
            ).fetchall()
        return [self._normalize(row) for row in rows]

    def get(self, binding_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM semantic_geometry_bindings WHERE id=?", (binding_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown semantic geometry binding: {binding_id}")
        return self._normalize(row)

    def verify(self, binding_id: str) -> dict[str, Any]:
        try:
            binding = self.get(binding_id)
            scene = SceneStore(self.project).get(binding["scene_id"])
            proposal = json.loads(
                self.artifacts.path_for(binding["proposal_digest"]).read_text(encoding="utf-8")
            )
            proposal_record = proposal["record"]
            expected = self._proposal_receipt(
                binding_id=binding["id"],
                record=proposal_record,
                created_at=proposal["created_at"],
            )
            current_proposal_record = {
                **binding["record"],
                "state": "PROVISIONALLY_BOUND",
                "review": None,
                "blockers": self._binding_blockers(
                    {
                        **binding["record"],
                        "state": "PROVISIONALLY_BOUND",
                        "review": None,
                    }
                ),
            }
            inventory_object = self._inventory_object(
                scene.get("inventory"), binding["object_name"]
            )
            proposal_valid = bool(
                self._artifact_valid(binding["proposal_digest"])
                and canonical_json(proposal) == canonical_json(expected)
                and canonical_json(proposal_record) == canonical_json(current_proposal_record)
                and scene["artifact_digest"] == binding["record"]["scene_artifact_digest"]
                and hashlib.sha256(canonical_json(inventory_object)).hexdigest()
                == binding["record"]["inventory_object_sha256"]
                and (
                    not proposal_record.get("previous_proposal_digest")
                    or self._artifact_valid(proposal_record["previous_proposal_digest"])
                )
            )
            decision_valid = binding["decision_digest"] is None
            if binding["decision_digest"] is not None:
                decision_valid = self._verify_decision(binding)
            return {
                "valid": bool(proposal_valid and decision_valid),
                "proposal_valid": proposal_valid,
                "decision_valid": decision_valid,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "proposal_valid": False, "decision_valid": False}

    def coverage(self, scene_id: str) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        inventory = scene.get("inventory")
        visible = sorted(
            str(item.get("name"))
            for item in (inventory or {}).get("objects", [])
            if item.get("type") == "MESH" and not item.get("hidden_render", False)
        )
        bindings = {item["object_name"]: item for item in self.list(scene_id)}
        invalid = sorted(
            name for name, item in bindings.items() if not self.verify(item["id"])["valid"]
        )
        state_names = {
            state: sorted(
                name
                for name in visible
                if name in bindings and bindings[name]["state"] == state and name not in invalid
            )
            for state in BINDING_STATES
        }
        missing = sorted(set(visible) - set(bindings))
        unbound = sorted(set(missing + state_names["UNBOUND"] + invalid))
        accepted = state_names["ACCEPTED_BOUND"]
        return {
            "scene_id": scene_id,
            "visible_mesh_count": len(visible),
            "binding_count": len(bindings),
            "valid_binding_count": len(bindings) - len(invalid),
            "state_counts": {state: len(names) for state, names in sorted(state_names.items())},
            "unbound_objects": unbound,
            "invalid_binding_objects": invalid,
            "all_visible_resolved": not unbound,
            "all_visible_reviewed": not unbound
            and all(name in state_names["REVIEWED_BOUND"] or name in accepted for name in visible),
            "all_visible_accepted": len(accepted) == len(visible) and not invalid,
            "acceptance_authority": (
                "NAMED_ACCEPTED_OBJECT_BINDINGS"
                if len(accepted) == len(visible) and not invalid
                else "SEMANTIC_BINDING_INCOMPLETE_OR_UNREVIEWED"
            ),
        }

    def assembly_audit(self, scene_id: str) -> dict[str, Any]:
        bindings = self.list(scene_id)
        with self.project.connection() as connection:
            edges = [
                json.loads(row["record_json"])
                for row in connection.execute(
                    "SELECT e.record_json FROM semantic_edges e "
                    "JOIN semantic_nodes s ON s.id=e.source_id "
                    "WHERE json_extract(s.record_json,'$.geometry.scene_id')=? "
                    "OR json_extract(s.record_json,'$.parameters.scene_id')=?",
                    (scene_id, scene_id),
                ).fetchall()
            ]
        part_of_sources = {
            str(item.get("source")) for item in edges if item.get("relation") == "PART_OF"
        }
        missing_parent = sorted(
            item["object_name"] for item in bindings if item["semantic_id"] not in part_of_sources
        )
        coverage = self.coverage(scene_id)
        return {
            "scene_id": scene_id,
            "status": (
                "BLOCKED"
                if coverage["unbound_objects"] or missing_parent
                else "GRAPH_READY_PHYSICAL_RELATIONS_UNREVIEWED"
            ),
            "binding_coverage": coverage,
            "relationship_count": len(edges),
            "missing_part_of_objects": missing_parent,
            "physical_relation_types_present": sorted(
                {
                    str(item.get("relation"))
                    for item in edges
                    if item.get("relation") in ASSEMBLY_RELATIONS - {"PART_OF"}
                }
            ),
            "blind_spots": [
                "contact, clearance, wall thickness, and intersection need geometric checks",
                "provisional relationships have no named acceptance authority",
            ],
        }

    def _propose_binding(
        self,
        *,
        scene: dict[str, Any],
        obj: dict[str, Any],
        semantic_id: str,
        parent_assembly_id: str,
        diagnosis: dict[str, Any],
        evaluation_views: list[str],
        override: dict[str, Any],
    ) -> dict[str, Any]:
        name = str(obj["name"])
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT * FROM semantic_geometry_bindings WHERE scene_id=? AND object_name=?",
                (scene["id"], name),
            ).fetchone()
        if existing is not None:
            return self._normalize(existing)
        signals = dict(diagnosis["signals"])
        signals["geometry_dimensions"] = bool(obj.get("world_bounds", {}).get("dimensions"))
        signals["spatial_position"] = bool(obj.get("world_bounds"))
        signals["material"] = bool(obj.get("materials"))
        signals["object_id_render"] = bool(signals["object_id_render"] or obj.get("component_id"))
        record = {
            "schema_version": self.SCHEMA_VERSION,
            "scene_id": scene["id"],
            "scene_artifact_digest": scene["artifact_digest"],
            "object_name": name,
            "semantic_id": semantic_id,
            "semantic_component": diagnosis["semantic_component"],
            "parent_assembly_id": parent_assembly_id,
            "parent_assembly": diagnosis["parent_assembly"],
            "evidence_sources": [
                {
                    "kind": "GOVERNED_SCENE_INVENTORY",
                    "scene_artifact_digest": scene["artifact_digest"],
                },
                *list(override.get("evidence_sources", [])),
            ],
            "parameters": self._object_parameters(obj),
            "reference_regions": list(override.get("reference_regions", [])),
            "evaluation_views": list(override.get("evaluation_views", evaluation_views)),
            "confidence": float(diagnosis["confidence"]),
            "visual_importance": float(diagnosis["visual_importance"]),
            "classification_signals": signals,
            "inventory_object_sha256": hashlib.sha256(canonical_json(obj)).hexdigest(),
            "state": "PROVISIONALLY_BOUND",
            "review": None,
            "authority": "MACHINE_SEMANTIC_PROPOSAL_NO_ACCEPTANCE_AUTHORITY",
        }
        record["blockers"] = self._binding_blockers(record)
        binding_id = str(uuid.uuid4())
        created_at = utc_now()
        proposal = self._proposal_receipt(
            binding_id=binding_id, record=record, created_at=created_at
        )
        relative = Path("receipts") / f"semantic-binding-proposal-{binding_id}.json"
        atomic_write_json(self.project.root / relative, proposal)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.semantic-binding-proposal+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            connection.execute(
                "INSERT INTO semantic_geometry_bindings"
                "(id,scene_id,object_name,semantic_id,parent_assembly_id,state,record_json,"
                "proposal_digest,decision_digest,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                (
                    binding_id,
                    scene["id"],
                    name,
                    semantic_id,
                    parent_assembly_id,
                    "PROVISIONALLY_BOUND",
                    json.dumps(record),
                    artifact.digest,
                    None,
                    created_at,
                    created_at,
                ),
            )
            row = connection.execute(
                "SELECT record_json FROM semantic_nodes WHERE id=?", (semantic_id,)
            ).fetchone()
            if row is None:
                raise RuntimeError("semantic node disappeared during binding proposal")
            node = json.loads(row["record_json"])
            node.update(
                {
                    "geometry": {
                        "scene_id": scene["id"],
                        "object_names": [name],
                        "component_ids": [obj["component_id"]] if obj.get("component_id") else [],
                    },
                    "parameters": record["parameters"],
                    "references": record["evaluation_views"],
                    "confidence": record["confidence"],
                    "binding_state": "PROVISIONALLY_BOUND",
                    "acceptance_state": "pending",
                    "updated_at": created_at,
                }
            )
            connection.execute(
                "UPDATE semantic_nodes SET record_json=?,updated_at=? WHERE id=?",
                (json.dumps(node), created_at, semantic_id),
            )
        return self.get(binding_id)

    def _ensure_node(
        self,
        node_id: str,
        *,
        parent_id: str | None,
        node_type: str,
        relation: str | None,
        parameters: dict[str, Any],
    ) -> str:
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT id FROM semantic_nodes WHERE id=?", (node_id,)
            ).fetchone()
            if existing is not None:
                return node_id
            now = utc_now()
            record = {
                "id": node_id,
                "parent_id": parent_id,
                "type": node_type,
                "geometry": None,
                "parameters": parameters,
                "constraints": [],
                "materials": [],
                "references": [],
                "observations": [],
                "confidence": 0.0,
                "acceptance_state": "pending",
                "lod_policies": {},
                "created_at": now,
                "updated_at": now,
            }
            connection.execute(
                "INSERT INTO semantic_nodes("
                "id,parent_id,node_type,record_json,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?)",
                (node_id, parent_id, node_type, json.dumps(record), now, now),
            )
            if parent_id and relation:
                edge = {
                    "id": str(uuid.uuid4()),
                    "source": node_id,
                    "target": parent_id,
                    "relation": relation,
                    "state": "STRUCTURAL_HIERARCHY",
                    "created_at": now,
                }
                connection.execute(
                    "INSERT INTO semantic_edges("
                    "id,source_id,target_id,relation,record_json,created_at) "
                    "VALUES(?,?,?,?,?,?)",
                    (
                        edge["id"],
                        node_id,
                        parent_id,
                        relation,
                        json.dumps(edge),
                        now,
                    ),
                )
        return node_id

    @staticmethod
    def _classify(name: str, override: dict[str, Any]) -> dict[str, Any]:
        lowered = name.lower()
        rules = [
            (("rj45", "ethernet"), "ethernet_connector", "rear_io_assembly", 3.0),
            (("usbc", "usb-c"), "usb_c_connector", "io_assembly", 3.0),
            (("hdmi",), "hdmi_connector", "rear_io_assembly", 3.0),
            (("displayport", "dp-"), "displayport_connector", "rear_io_assembly", 3.0),
            (("headphone", "audio"), "audio_connector", "io_assembly", 2.5),
            (("power-button", "power_button", "pwr-button"), "power_button", "power_assembly", 3.0),
            (("power", "pwr", "mac-ac"), "power_component", "power_assembly", 2.5),
            (("fan", "rotor", "blade"), "fan_component", "cooling_assembly", 3.0),
            (("fin", "heatsink", "heat-sink"), "heat_sink_fin", "cooling_assembly", 2.5),
            (
                ("vent", "grille", "mesh", "cellfield", "intake", "foam"),
                "ventilation_component",
                "cooling_assembly",
                3.0,
            ),
            (("brk", "bracket", "ioplate"), "io_bracket_component", "structural_assembly", 2.0),
            (("base", "bottom", "foot", "undercone"), "base_component", "base_assembly", 2.0),
            (("led",), "status_led", "io_assembly", 2.0),
            (("sd-", "sd_"), "sd_card_slot", "io_assembly", 2.5),
            (("screw", "fastener"), "fastener", "structural_assembly", 1.5),
            (
                (
                    "enclosure",
                    "body",
                    "shell",
                    "shroud",
                    "studio",
                    "dgx-spark",
                    "backplate",
                ),
                "enclosure_component",
                "enclosure_assembly",
                1.5,
            ),
        ]
        semantic = "technical_component"
        parent = "enclosure_assembly"
        importance = 1.0
        matched = []
        for tokens, candidate, assembly, weight in rules:
            if any(token in lowered for token in tokens):
                semantic, parent, importance = candidate, assembly, weight
                matched = [token for token in tokens if token in lowered]
                break
        semantic = str(override.get("semantic_component", semantic)).strip()
        parent = str(override.get("parent_assembly", parent)).strip()
        confidence = float(override.get("confidence", 0.82 if matched else 0.55))
        if not semantic or not parent or not 0.0 <= confidence <= 1.0:
            raise ValueError(f"invalid semantic classification for object {name}")
        return {
            "semantic_component": semantic,
            "parent_assembly": parent,
            "confidence": confidence,
            "visual_importance": float(override.get("visual_importance", importance)),
            "signals": {
                "object_name": True,
                "name_tokens": matched,
                "geometry_dimensions": True,
                "spatial_position": True,
                "material": True,
                "object_id_render": bool(override.get("object_id_render", False)),
                "feature_id_render": bool(override.get("feature_id_render", False)),
                "reference_segmentation": bool(override.get("reference_segmentation", False)),
                "vlm_classification": bool(override.get("vlm_classification", False)),
                "category_ontology": True,
                "human_correction": False,
            },
        }

    @staticmethod
    def _object_parameters(obj: dict[str, Any]) -> dict[str, Any]:
        bounds = obj.get("world_bounds", {})
        return {
            "world_bounds": bounds,
            "dimensions": bounds.get("dimensions"),
            "component_id": obj.get("component_id"),
            "component_type": obj.get("component_type"),
            "materials": obj.get("materials", []),
            "modifiers": [
                {"name": item.get("name"), "type": item.get("type")}
                for item in obj.get("modifiers", [])
            ],
            "mesh_summary": {
                key: obj.get("mesh", {}).get(key) for key in ("vertices", "edges", "polygons")
            },
        }

    def _evaluation_views(
        self, scene_id: str, explicit_reference_ids: list[str] | None
    ) -> list[str]:
        if explicit_reference_ids is not None:
            requested = sorted(set(str(value) for value in explicit_reference_ids))
        else:
            with self.project.connection() as connection:
                rows = connection.execute(
                    "SELECT outputs_json FROM render_runs WHERE scene_id=? ORDER BY created_at,id",
                    (scene_id,),
                ).fetchall()
            requested = sorted(
                {
                    str(output["reference_id"])
                    for row in rows
                    for output in json.loads(row["outputs_json"])
                    if output.get("reference_id")
                }
            )
        if requested:
            with self.project.connection() as connection:
                placeholders = ",".join("?" for _ in requested)
                known = {
                    str(row["id"])
                    for row in connection.execute(
                        f"SELECT id FROM reference_items WHERE id IN ({placeholders})", requested
                    ).fetchall()
                }
            unknown = sorted(set(requested) - known)
            if unknown:
                raise KeyError(f"unknown evaluation reference ids: {unknown}")
        return requested

    @staticmethod
    def _binding_blockers(record: dict[str, Any]) -> list[str]:
        blockers = []
        if not record.get("reference_regions"):
            blockers.append("component reference regions are not localized or reviewed")
        if not record.get("evaluation_views"):
            blockers.append("component has no governed evaluation views")
        if record.get("state") == "PROVISIONALLY_BOUND":
            blockers.append("machine semantic proposal requires named review")
        elif record.get("state") == "REVIEWED_BOUND":
            blockers.append("reviewed semantic binding lacks named acceptance")
        elif record.get("state") == "UNBOUND":
            blockers.append("visible geometry is semantically unbound")
        return blockers

    def _proposal_receipt(
        self, *, binding_id: str, record: dict[str, Any], created_at: str
    ) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "semantic_geometry_binding_proposal",
            "id": binding_id,
            "scene_id": record["scene_id"],
            "scene_artifact_digest": record["scene_artifact_digest"],
            "object_name": record["object_name"],
            "semantic_id": record["semantic_id"],
            "parent_assembly_id": record["parent_assembly_id"],
            "record": record,
            "record_sha256": hashlib.sha256(canonical_json(record)).hexdigest(),
            "authority": "MACHINE_SEMANTIC_PROPOSAL_NO_REVIEW_OR_ACCEPTANCE_AUTHORITY",
            "created_at": created_at,
        }

    def _verify_decision(self, binding: dict[str, Any]) -> bool:
        digest = binding["decision_digest"]
        if not digest or not self._artifact_valid(digest):
            return False
        decision = json.loads(self.artifacts.path_for(digest).read_text(encoding="utf-8"))
        review = binding["record"].get("review") or {}
        expected = {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "semantic_geometry_binding_decision",
            "binding_id": binding["id"],
            "proposal_digest": binding["proposal_digest"],
            "scene_id": binding["scene_id"],
            "scene_artifact_digest": binding["record"]["scene_artifact_digest"],
            "object_name": binding["object_name"],
            "semantic_id": binding["semantic_id"],
            "from_state": decision.get("from_state"),
            "to_state": binding["state"],
            "reviewer": review.get("reviewer"),
            "reason": review.get("reason"),
            "reviewed_at": review.get("reviewed_at"),
            "authority": (
                "NAMED_SEMANTIC_BINDING_ACCEPTANCE"
                if binding["state"] == "ACCEPTED_BOUND"
                else "NAMED_SEMANTIC_BINDING_REVIEW"
            ),
        }
        return bool(
            decision.get("from_state") in BINDING_STATES
            and canonical_json(decision) == canonical_json(expected)
            and review.get("decision_digest") == digest
        )

    @staticmethod
    def _inventory_object(inventory: Any, object_name: str) -> dict[str, Any]:
        if not isinstance(inventory, dict):
            raise ValueError("scene inventory is absent")
        matches = [item for item in inventory.get("objects", []) if item.get("name") == object_name]
        if len(matches) != 1:
            raise ValueError("bound object is missing or no longer uniquely named")
        return matches[0]

    def _artifact_valid(self, digest: str) -> bool:
        try:
            path = self.artifacts.path_for(digest)
            return path.is_file() and sha256_file(path)[0] == digest
        except (OSError, TypeError, ValueError):
            return False

    @staticmethod
    def _normalize(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["record"] = json.loads(value.pop("record_json"))
        return value
