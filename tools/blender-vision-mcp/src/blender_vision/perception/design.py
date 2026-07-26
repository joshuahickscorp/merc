from __future__ import annotations

import hashlib
import platform
import tempfile
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import canonical_json, sha256_file, utc_now
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore
from blender_vision.security.adversarial import DesignExportPolicy


class FigmaExportAdapter:
    """Governed adapter for a caller-supplied Figma REST/plugin JSON export."""

    name = "design.figma_export"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        supplied = Path(str(target.get("path", ""))).expanduser().absolute()
        if supplied.is_symlink():
            raise ValueError("Figma export target cannot be a symlink")
        path = supplied.resolve()
        if not path.is_file() or path.suffix.lower() != ".json":
            raise ValueError("Figma export target must be an existing JSON file")
        _payload, security = DesignExportPolicy.load(path)
        return {
            "id": str(target.get("id") or security["sha256"]),
            "kind": "figma-export",
            "path": str(path),
            "digest": security["sha256"],
            "size": security["size"],
            "security": security,
        }

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        rendered = config.get("rendered_image_path")
        image: dict[str, Any] | None = None
        if rendered:
            path = Path(str(rendered)).expanduser().resolve()
            if not path.is_file() or path.suffix.lower() not in {".png", ".jpg", ".jpeg"}:
                raise ValueError("rendered_image_path must be a PNG or JPEG file")
            digest, size = sha256_file(path)
            image = {"path": str(path), "digest": digest, "size": size}
        return {
            "rendered_image": image,
            "include_invisible": bool(config.get("include_invisible", False)),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "adapter": self.name,
            "adapter_version": self.version,
            "source_format": "figma-json-export",
            "rendered_image_digest": (
                config["rendered_image"]["digest"] if config["rendered_image"] else None
            ),
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        source = Path(target["path"])
        payload, security = DesignExportPolicy.load(source)
        if security["sha256"] != target["digest"]:
            raise ValueError("Figma export changed after target normalization")
        source_record = sink(
            "design.source",
            source.read_bytes(),
            "application/json",
            {"source_format": "figma-json-export", "source_digest": target["digest"]},
        )
        rendered_record = None
        if config["rendered_image"]:
            rendered = Path(config["rendered_image"]["path"])
            media_type = "image/png" if rendered.suffix.lower() == ".png" else "image/jpeg"
            rendered_record = sink(
                "design.rendered",
                rendered.read_bytes(),
                media_type,
                {"source": "caller-supplied-figma-render"},
            )
        graph = self._compile_graph(
            payload,
            source_digest=source_record["digest"],
            rendered_digest=rendered_record["digest"] if rendered_record else None,
            include_invisible=config["include_invisible"],
        )
        graph["source_security"] = security
        sink("design.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "design_node_count": len(graph["nodes"]),
                "design_edge_count": len(graph["edges"]),
                "component_count": sum(
                    node["domain_type"] in {"COMPONENT", "COMPONENT_SET", "INSTANCE"}
                    for node in graph["nodes"]
                ),
                "token_count": len(graph["tokens"]),
            },
            limitations=[
                "The adapter observes only fields present in the caller-supplied export.",
                "Prototype behavior and rendered pixels require corresponding exported evidence.",
            ],
            graphs=[
                {
                    "graph_type": "DesignSystemGraph",
                    "role": "design.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "OBSERVED",
                }
            ],
        )

    @classmethod
    def _compile_graph(
        cls,
        payload: dict[str, Any],
        *,
        source_digest: str,
        rendered_digest: str | None,
        include_invisible: bool,
    ) -> dict[str, Any]:
        document = payload.get("document") or payload
        nodes: list[dict[str, Any]] = []
        edges: list[dict[str, Any]] = []

        def walk(item: dict[str, Any], parent: str | None = None) -> None:
            if item.get("visible") is False and not include_invisible:
                return
            raw_id = str(item.get("id") or f"anonymous-{len(nodes)}")
            node_id = f"figma:{raw_id}"
            bounds = item.get("absoluteBoundingBox") or item.get("absoluteRenderBounds") or {}
            evidence = [{"role": "design.source", "artifact_digest": source_digest}]
            if rendered_digest:
                evidence.append(
                    {"role": "design.rendered", "artifact_digest": rendered_digest}
                )
            node = {
                "id": node_id,
                "domain_type": str(item.get("type", "UNKNOWN")),
                "spatial_bounds": {
                    "x": float(bounds.get("x", 0)),
                    "y": float(bounds.get("y", 0)),
                    "width": float(bounds.get("width", 0)),
                    "height": float(bounds.get("height", 0)),
                },
                "temporal_validity": "export-revision",
                "evidence_references": evidence,
                "authority": "OBSERVED",
                "confidence": 1.0,
                "source_restrictions": ["governed-design-export"],
                "uncertainty": [],
                "revision_lineage": [],
                "name": str(item.get("name", "")),
                "semantic_name": cls._semantic_name(str(item.get("name", ""))),
                "visible": item.get("visible", True),
                "opacity": item.get("opacity", 1),
                "layout": {
                    key: item.get(key)
                    for key in (
                        "layoutMode",
                        "primaryAxisAlignItems",
                        "counterAxisAlignItems",
                        "itemSpacing",
                        "paddingLeft",
                        "paddingRight",
                        "paddingTop",
                        "paddingBottom",
                        "constraints",
                    )
                    if key in item
                },
                "appearance": {
                    key: item.get(key)
                    for key in ("fills", "strokes", "effects", "cornerRadius")
                    if key in item
                },
                "typography": item.get("style", {}) if item.get("type") == "TEXT" else {},
                "component_id": item.get("componentId"),
                "component_properties": item.get("componentProperties", {}),
                "variant_properties": item.get("variantProperties", {}),
                "style_bindings": item.get("styles", {}),
                "source_binding": {
                    "figma_id": raw_id,
                    "plugin_data": item.get("sharedPluginData", {}),
                },
            }
            nodes.append(node)
            if parent is not None:
                edges.append(
                    {
                        "source": parent,
                        "target": node_id,
                        "type": "CONTAINS",
                        "authority": "OBSERVED",
                        "evidence_references": evidence,
                    }
                )
            if item.get("componentId"):
                edges.append(
                    {
                        "source": node_id,
                        "target": f"figma:{item['componentId']}",
                        "type": "CORRESPONDS_TO",
                        "relationship": "INSTANCE_OF",
                        "authority": "OBSERVED",
                        "evidence_references": evidence,
                    }
                )
            for child in item.get("children", []):
                walk(child, node_id)

        walk(document)
        tokens = cls._figma_tokens(payload)
        return {
            "schema": "vision.design-system-graph/v1",
            "graph_type": "DesignSystemGraph",
            "authority": "OBSERVED",
            "source_kind": "figma",
            "source_revision": payload.get("version") or payload.get("lastModified"),
            "nodes": nodes,
            "edges": edges,
            "tokens": tokens,
            "components": payload.get("components", {}),
            "component_sets": payload.get("componentSets", {}),
        }

    @classmethod
    def _figma_tokens(cls, payload: dict[str, Any]) -> list[dict[str, Any]]:
        tokens: list[dict[str, Any]] = []
        for token_id, style in sorted((payload.get("styles") or {}).items()):
            tokens.append(
                {
                    "id": f"figma-token:{token_id}",
                    "name": style.get("name", token_id),
                    "semantic_name": cls._semantic_name(style.get("name", token_id)),
                    "kind": style.get("styleType", "STYLE"),
                    "value": style.get("value"),
                    "authority": "OBSERVED",
                }
            )
        for token_id, variable in sorted((payload.get("variables") or {}).items()):
            tokens.append(
                {
                    "id": f"figma-variable:{token_id}",
                    "name": variable.get("name", token_id),
                    "semantic_name": cls._semantic_name(variable.get("name", token_id)),
                    "kind": variable.get("resolvedType", "VARIABLE"),
                    "value": variable.get("value"),
                    "authority": "OBSERVED",
                }
            )
        return tokens

    @staticmethod
    def _semantic_name(value: str) -> str:
        return "".join(character.lower() for character in value if character.isalnum())


class StorybookExportAdapter:
    """Governed adapter for Storybook index/manifest JSON."""

    name = "design.storybook_export"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        supplied = Path(str(target.get("path", ""))).expanduser().absolute()
        if supplied.is_symlink():
            raise ValueError("Storybook export target cannot be a symlink")
        path = supplied.resolve()
        if not path.is_file() or path.suffix.lower() != ".json":
            raise ValueError("Storybook target must be an existing index JSON file")
        _payload, security = DesignExportPolicy.load(path)
        return {
            "id": str(target.get("id") or security["sha256"]),
            "kind": "storybook-export",
            "path": str(path),
            "digest": security["sha256"],
            "size": security["size"],
            "security": security,
        }

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        return {
            "framework": str(config.get("framework", "unknown")),
            "builder": str(config.get("builder", "unknown")),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {
            "platform": platform.platform(),
            "python": platform.python_version(),
            "adapter": self.name,
            "adapter_version": self.version,
            "framework": config["framework"],
            "builder": config["builder"],
        }

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        source = Path(target["path"])
        payload, security = DesignExportPolicy.load(source)
        if security["sha256"] != target["digest"]:
            raise ValueError("Storybook export changed after target normalization")
        source_record = sink(
            "design.source",
            source.read_bytes(),
            "application/json",
            {"source_format": "storybook-index", "source_digest": target["digest"]},
        )
        graph = self._compile_graph(payload, source_record["digest"], config)
        graph["source_security"] = security
        sink("design.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={
                "design_node_count": len(graph["nodes"]),
                "design_edge_count": len(graph["edges"]),
                "component_count": sum(
                    node["domain_type"] == "StorybookComponent" for node in graph["nodes"]
                ),
                "story_count": sum(
                    node["domain_type"] == "StorybookStory" for node in graph["nodes"]
                ),
                "token_count": len(graph["tokens"]),
            },
            limitations=[
                "Only exported Storybook metadata is observed; play-function execution "
                "is separate.",
                "Visual states without an exported story remain unobserved.",
            ],
            graphs=[
                {
                    "graph_type": "DesignSystemGraph",
                    "role": "design.graph",
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                    "authority": "OBSERVED",
                }
            ],
        )

    @classmethod
    def _compile_graph(
        cls,
        payload: dict[str, Any],
        source_digest: str,
        config: dict[str, Any],
    ) -> dict[str, Any]:
        entries = payload.get("entries") or payload.get("stories") or {}
        grouped: dict[str, list[tuple[str, dict[str, Any]]]] = {}
        for story_id, entry in sorted(entries.items()):
            title = str(entry.get("title") or "Untitled")
            grouped.setdefault(title, []).append((story_id, entry))
        nodes: list[dict[str, Any]] = []
        edges: list[dict[str, Any]] = []
        evidence = [{"role": "design.source", "artifact_digest": source_digest}]
        for title, stories in grouped.items():
            component_id = f"storybook:component:{cls._semantic_name(title)}"
            nodes.append(
                cls._node(
                    component_id,
                    "StorybookComponent",
                    title,
                    evidence,
                    {
                        "semantic_name": cls._semantic_name(title),
                        "import_path": stories[0][1].get("importPath"),
                        "framework": config["framework"],
                    },
                )
            )
            for story_id, entry in stories:
                story_node_id = f"storybook:story:{story_id}"
                name = str(entry.get("name") or entry.get("story") or story_id)
                nodes.append(
                    cls._node(
                        story_node_id,
                        "StorybookStory",
                        name,
                        evidence,
                        {
                            "semantic_name": cls._semantic_name(name),
                            "component_semantic_name": cls._semantic_name(title),
                            "args": entry.get("args", {}),
                            "arg_types": entry.get("argTypes", {}),
                            "parameters": entry.get("parameters", {}),
                            "tags": entry.get("tags", []),
                            "source_binding": {
                                "import_path": entry.get("importPath"),
                                "export_name": entry.get("exportName"),
                            },
                        },
                    )
                )
                edges.append(
                    {
                        "source": component_id,
                        "target": story_node_id,
                        "type": "CONTAINS",
                        "relationship": "HAS_VARIANT_OR_STATE",
                        "authority": "OBSERVED",
                        "evidence_references": evidence,
                    }
                )
        tokens = [
            {
                "id": f"storybook-token:{name}",
                "name": name,
                "semantic_name": cls._semantic_name(name),
                "kind": token.get("kind", "TOKEN") if isinstance(token, dict) else "TOKEN",
                "value": token.get("value") if isinstance(token, dict) else token,
                "authority": "OBSERVED",
            }
            for name, token in sorted((payload.get("tokens") or {}).items())
        ]
        return {
            "schema": "vision.design-system-graph/v1",
            "graph_type": "DesignSystemGraph",
            "authority": "OBSERVED",
            "source_kind": "storybook",
            "source_revision": payload.get("v") or payload.get("version"),
            "framework": config["framework"],
            "builder": config["builder"],
            "nodes": nodes,
            "edges": edges,
            "tokens": tokens,
        }

    @staticmethod
    def _node(
        node_id: str,
        domain_type: str,
        name: str,
        evidence: list[dict[str, str]],
        extra: dict[str, Any],
    ) -> dict[str, Any]:
        return {
            "id": node_id,
            "domain_type": domain_type,
            "spatial_bounds": None,
            "temporal_validity": "export-revision",
            "evidence_references": evidence,
            "authority": "OBSERVED",
            "confidence": 1.0,
            "source_restrictions": ["governed-component-export"],
            "uncertainty": ["no-spatial-render-exported"],
            "revision_lineage": [],
            "name": name,
            **extra,
        }

    @staticmethod
    def _semantic_name(value: str) -> str:
        return "".join(character.lower() for character in value if character.isalnum())


class DesignIntelligenceService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.query = ObservationQueryService(project)

    def analyze_drift(
        self,
        figma_capture_id: str,
        storybook_capture_id: str,
        *,
        bindings: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        figma = self.query.graph(figma_capture_id, "DesignSystemGraph")
        storybook = self.query.graph(storybook_capture_id, "DesignSystemGraph")
        if figma.get("source_kind") != "figma" or storybook.get("source_kind") != "storybook":
            raise ValueError("design drift requires a Figma capture and a Storybook capture")
        binding_map = dict(bindings or {})
        figma_components = {
            node["semantic_name"]: node
            for node in figma["nodes"]
            if node["domain_type"] in {"COMPONENT", "COMPONENT_SET"}
        }
        story_components = {
            node["semantic_name"]: node
            for node in storybook["nodes"]
            if node["domain_type"] == "StorybookComponent"
        }
        for name in sorted(figma_components.keys() & story_components.keys()):
            binding_map.setdefault(name, name)
        mapped = []
        for figma_name, story_name in sorted(binding_map.items()):
            if figma_name in figma_components and story_name in story_components:
                mapped.append(
                    {
                        "figma_node": figma_components[figma_name]["id"],
                        "storybook_node": story_components[story_name]["id"],
                        "type": "CORRESPONDS_TO",
                        "traceable": True,
                    }
                )
        mapped_figma_names = {
            figma_name
            for figma_name, story_name in binding_map.items()
            if figma_name in figma_components and story_name in story_components
        }
        missing_components = sorted(set(figma_components) - mapped_figma_names)
        figma_tokens = {token["semantic_name"]: token for token in figma.get("tokens", [])}
        story_tokens = {token["semantic_name"]: token for token in storybook.get("tokens", [])}
        token_drift = []
        for name in sorted(figma_tokens.keys() | story_tokens.keys()):
            left, right = figma_tokens.get(name), story_tokens.get(name)
            if left is None or right is None or left.get("value") != right.get("value"):
                token_drift.append(
                    {
                        "token": name,
                        "figma": left.get("value") if left else None,
                        "storybook": right.get("value") if right else None,
                    }
                )
        missing_variants = self._variant_drift(figma, storybook, binding_map)
        status = (
            "PASS"
            if not missing_components and not token_drift and not missing_variants
            else "DRIFT_DETECTED"
        )
        report = {
            "schema": "vision.design-drift/v1",
            "id": hashlib.sha256(
                canonical_json(
                    {
                        "figma_capture_id": figma_capture_id,
                        "storybook_capture_id": storybook_capture_id,
                        "bindings": binding_map,
                    }
                )
            ).hexdigest(),
            "figma_capture_id": figma_capture_id,
            "storybook_capture_id": storybook_capture_id,
            "authority": "DERIVED",
            "status": status,
            "component_bindings": mapped,
            "missing_components": missing_components,
            "missing_variants": missing_variants,
            "token_drift": token_drift,
            "component_policy": {
                "existing_component_reuse_required": True,
                "one_off_component_count": len(missing_components),
                "traceable_binding_count": len(mapped),
            },
            "citations": [figma["citation"], storybook["citation"]],
            "created_at": utc_now(),
        }
        record = self._ingest_json(report)
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR IGNORE INTO design_drift_runs("
                "id,figma_capture_id,storybook_capture_id,bindings_json,status,"
                "report_digest,created_at) VALUES(?,?,?,?,?,?,?)",
                (
                    report["id"],
                    figma_capture_id,
                    storybook_capture_id,
                    canonical_json(binding_map).decode(),
                    status,
                    record["digest"],
                    report["created_at"],
                ),
            )
        return {**report, "report_digest": record["digest"]}

    @staticmethod
    def _variant_drift(
        figma: dict[str, Any],
        storybook: dict[str, Any],
        bindings: dict[str, str],
    ) -> list[dict[str, Any]]:
        figma_variants: dict[str, set[str]] = {}
        for node in figma["nodes"]:
            if node["domain_type"] == "COMPONENT" and node.get("variant_properties"):
                parent = node.get("semantic_name", "")
                variant = ",".join(
                    f"{key}={value}"
                    for key, value in sorted(node["variant_properties"].items())
                )
                figma_variants.setdefault(parent, set()).add(
                    FigmaExportAdapter._semantic_name(variant)
                )
        story_variants: dict[str, set[str]] = {}
        for node in storybook["nodes"]:
            if node["domain_type"] == "StorybookStory":
                story_variants.setdefault(node["component_semantic_name"], set()).add(
                    node["semantic_name"]
                )
        missing = []
        for figma_name, story_name in bindings.items():
            absent = sorted(
                figma_variants.get(figma_name, set()) - story_variants.get(story_name, set())
            )
            if absent:
                missing.append({"component": figma_name, "variants": absent})
        return missing

    def _ingest_json(self, value: dict[str, Any]) -> dict[str, Any]:
        staging = self.project.root / "observations" / ".staging"
        staging.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(
            prefix="design-drift-", suffix=".json", dir=staging
        ) as file:
            file.write(canonical_json(value))
            file.flush()
            return self.artifacts.ingest_file(
                Path(file.name), media_type="application/json"
            ).to_dict()
