from __future__ import annotations

import json
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.perception.bus import CaptureBus
from blender_vision.projects.store import ProjectStore


class ObservationQueryService:
    def __init__(self, project: ProjectStore, bus: CaptureBus | None = None):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.bus = bus

    def overview(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,adapter,status,authority,manifest_digest,summary_json,"
                "attempt_count,created_at,updated_at FROM observation_captures "
                "ORDER BY created_at DESC,id"
            ).fetchall()
        return {
            "project_id": self.project.project()["id"],
            "captures": [
                {
                    "capture_id": row["id"],
                    "adapter": row["adapter"],
                    "status": row["status"],
                    "authority": row["authority"],
                    "manifest_digest": row["manifest_digest"],
                    "summary": json.loads(row["summary_json"]),
                    "attempt_count": row["attempt_count"],
                    "created_at": row["created_at"],
                    "updated_at": row["updated_at"],
                }
                for row in rows
            ],
        }

    def latest_capture_id(self) -> str:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT id FROM observation_captures WHERE status='COMPLETE' "
                "ORDER BY updated_at DESC,id DESC LIMIT 1"
            ).fetchone()
        if row is None:
            raise KeyError("project has no complete observations")
        return str(row["id"])

    def graph(self, capture_id: str, graph_type: str = "LayoutGraph") -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT artifact_digest,authority,node_count,edge_count FROM perceptual_graphs "
                "WHERE capture_id=? AND graph_type=?",
                (capture_id, graph_type),
            ).fetchone()
        if row is None:
            raise KeyError(f"{graph_type} not found for capture {capture_id}")
        path = self.artifacts.path_for(row["artifact_digest"])
        graph = json.loads(path.read_text(encoding="utf-8"))
        graph["citation"] = {
            "capture_id": capture_id,
            "artifact_digest": row["artifact_digest"],
            "authority": row["authority"],
        }
        return graph

    def graph_types(self, capture_id: str) -> list[str]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT graph_type FROM perceptual_graphs "
                "WHERE capture_id=? ORDER BY graph_type",
                (capture_id,),
            ).fetchall()
        return [str(row["graph_type"]) for row in rows]

    def query(self, capture_id: str, query: dict[str, Any]) -> dict[str, Any]:
        requested_type = query.get("graph_type")
        if requested_type is None:
            graph_types = self.graph_types(capture_id)
            if "LayoutGraph" in graph_types:
                requested_type = "LayoutGraph"
            elif len(graph_types) == 1:
                requested_type = graph_types[0]
            elif graph_types:
                raise ValueError(
                    "capture contains multiple graph types; query.graph_type is required"
                )
            else:
                raise KeyError(f"capture {capture_id} has no perceptual graph")
        graph = self.graph(capture_id, str(requested_type))
        nodes = list(graph.get("nodes", []))
        node_id = query.get("id")
        domain_type = query.get("domain_type")
        selector = query.get("selector")
        role = query.get("role")
        text = query.get("text")
        asset_url = query.get("asset_url")
        surface = query.get("surface")
        styles = query.get("styles")
        source_binding = query.get("source_binding")
        interactive = query.get("interactive")
        point = query.get("point")
        if node_id:
            nodes = [node for node in nodes if node.get("id") == node_id]
        if domain_type:
            nodes = [node for node in nodes if node.get("domain_type") == domain_type]
        if selector:
            nodes = [node for node in nodes if node.get("selector") == selector]
        if role:
            nodes = [node for node in nodes if node.get("role") == role]
        if text:
            needle = str(text).casefold()
            nodes = [
                node
                for node in nodes
                if needle
                in str(node.get("text") or node.get("name") or "").casefold()
            ]
        if asset_url:
            needle = str(asset_url).casefold()
            nodes = [
                node
                for node in nodes
                if any(needle in str(url).casefold() for url in node.get("assetUrls", []))
            ]
        if surface:
            nodes = [node for node in nodes if node.get("surface") == surface]
        if styles:
            nodes = [
                node
                for node in nodes
                if all(node.get("styles", {}).get(key) == value for key, value in styles.items())
            ]
        if source_binding:
            nodes = [
                node
                for node in nodes
                if all(
                    node.get("sourceBinding", {}).get(key) == value
                    for key, value in source_binding.items()
                )
            ]
        if interactive is not None:
            if not isinstance(interactive, bool):
                raise ValueError("interactive query must be a boolean")
            nodes = [node for node in nodes if node.get("interactive") is interactive]
        if point:
            x, y = float(point["x"]), float(point["y"])
            nodes = [
                node
                for node in nodes
                if (bounds := node.get("bounds"))
                and bounds["x"] <= x <= bounds["x"] + bounds["width"]
                and bounds["y"] <= y <= bounds["y"] + bounds["height"]
            ]
            nodes.sort(
                key=lambda node: (
                    float(node.get("styles", {}).get("zIndexNumeric", 0)),
                    node.get("depth", 0),
                ),
                reverse=True,
            )
        limit = max(1, min(int(query.get("limit", 50)), 500))
        return {
            "capture_id": capture_id,
            "graph_type": graph["graph_type"],
            "query": query,
            "matches": nodes[:limit],
            "match_count": len(nodes),
            "citation": graph["citation"],
        }

    def verify(self, capture_id: str) -> dict[str, Any]:
        if self.bus is None:
            raise RuntimeError("verification requires a CaptureBus")
        return self.bus.verify(capture_id)
