from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator
from PIL import Image

from blender_vision.perception import (
    AdapterRegistry,
    CaptureBus,
    CodeRepositoryAdapter,
    ImageFileAdapter,
    PerceptionLearningService,
    PerceptionWorkspace,
    SourceIntelligenceService,
)
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.projects.store import ProjectStore


class LayoutFixtureAdapter:
    name = "fixture.layout"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return {"id": str(target["id"]), "kind": "fixture"}

    def normalize_config(self, target: dict[str, Any], config: dict[str, Any]) -> dict[str, Any]:
        del target, config
        return {}

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        del config
        return {"fixture": True}

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        del target, config
        screenshot = sink("screenshot.viewport", b"owned-layout-pixels", "image/png", None)
        graph = {
            "schema": "vision.layout-graph/v1",
            "graph_type": "LayoutGraph",
            "authority": "OBSERVED",
            "coordinate_space": "CSS viewport pixels",
            "capture": {"fixture": True},
            "nodes": [
                {
                    "id": "layout:hero",
                    "domain_type": "DOMElement",
                    "selector": "#hero",
                    "tag": "section",
                    "bounds": {"x": 0, "y": 0, "width": 300, "height": 180},
                    "spatial_bounds": {
                        "x": 0,
                        "y": 0,
                        "width": 300,
                        "height": 180,
                    },
                    "styles": {"zIndexNumeric": 0},
                    "sourceBinding": {"id": "hero", "path": "src/Hero.tsx"},
                    "surface": "dom",
                    "interactive": False,
                    "depth": 1,
                    "evidence_references": [
                        {
                            "role": "screenshot.viewport",
                            "artifact_digest": screenshot["digest"],
                        }
                    ],
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "source_restrictions": ["owned-fixture"],
                    "uncertainty": [],
                    "revision_lineage": [],
                }
            ],
            "edges": [],
        }
        sink("layout.graph", json.dumps(graph).encode(), "application/json", None)
        interaction = {
            "schema": "vision.interaction-graph/v1",
            "graph_type": "InteractionGraph",
            "authority": "OBSERVED",
            "target": {"fixture": True},
            "nodes": [
                {
                    "id": "#hero",
                    "domain_type": "ActionableElement",
                    "selector": "#hero",
                    "spatial_bounds": {
                        "x": 0,
                        "y": 0,
                        "width": 300,
                        "height": 180,
                    },
                    "evidence_references": [
                        {
                            "role": "screenshot.viewport",
                            "artifact_digest": screenshot["digest"],
                        }
                    ],
                    "authority": "OBSERVED",
                    "confidence": 1.0,
                    "source_restrictions": ["owned-fixture"],
                    "uncertainty": [],
                    "revision_lineage": [],
                }
            ],
            "edges": [
                {
                    "id": "interaction:hero-click",
                    "source": "#hero",
                    "target": "state:active",
                    "type": "TRIGGERS",
                    "input": {"mode": "pointer", "event": "click"},
                    "event_target": "#hero",
                    "authority": "OBSERVED",
                    "evidence_references": [
                        {
                            "role": "screenshot.viewport",
                            "artifact_digest": screenshot["digest"],
                        }
                    ],
                }
            ],
        }
        sink(
            "interaction.graph",
            json.dumps(interaction).encode(),
            "application/json",
            None,
        )
        return CaptureOutcome(
            summary={"node_count": 1},
            graphs=[
                {
                    "graph_type": "LayoutGraph",
                    "role": "layout.graph",
                    "node_count": 1,
                    "edge_count": 0,
                    "authority": "OBSERVED",
                },
                {
                    "graph_type": "InteractionGraph",
                    "role": "interaction.graph",
                    "node_count": 1,
                    "edge_count": 1,
                    "authority": "OBSERVED",
                },
            ],
        )


def _repository(root: Path) -> None:
    (root / "src").mkdir(parents=True)
    (root / "src" / "Hero.tsx").write_text(
        """
export function Hero() {
  return <section id="hero" className="hero">Owned fixture</section>;
}
""".strip(),
        encoding="utf-8",
    )
    (root / "src" / "styles.css").write_text(
        ":root { --color-action: #125cd2; }\n.hero { color: var(--color-action); }\n",
        encoding="utf-8",
    )
    (root / "src" / "Hero.test.tsx").write_text(
        "export const HeroTest = () => 'owned';\n", encoding="utf-8"
    )
    (root / "package.json").write_text(
        '{"name":"owned-fixture","scripts":{"test":"vitest"}}', encoding="utf-8"
    )


def test_code_graph_runtime_binding_blast_radius_and_schema(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Code perception")
    repository = tmp_path / "repository"
    _repository(repository)
    registry = AdapterRegistry()
    registry.register(CodeRepositoryAdapter())
    registry.register(LayoutFixtureAdapter())
    bus = CaptureBus(project, registry)
    layout = bus.observe(
        "fixture.layout",
        {"id": "owned-layout"},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    code = bus.observe(
        "code.repository",
        {"path": str(repository)},
        {
            "linked_capture_ids": [layout["capture_id"]],
            "runtime_bindings": [
                {
                    "runtime_node_id": "#hero",
                    "source_path": "src/Hero.tsx",
                    "symbol": "Hero",
                    "binding_kind": "owned-fixture-instrumentation",
                    "capture_id": layout["capture_id"],
                }
            ],
        },
        rights_decision="SYNTHETIC_OWNED",
    )
    service = SourceIntelligenceService(project)
    graph = service.query.graph(code["capture_id"], "CodeGraph")
    blast = service.visual_blast_radius(
        code["capture_id"], ["src/Hero.tsx"], [layout["capture_id"]]
    )
    visual = service.query.query(layout["capture_id"], {"point": {"x": 20, "y": 20}})
    source_trace = service.explain_bindings(visual["matches"], code["capture_id"])
    pixel_trace = service.source_to_pixel_trace(
        code["capture_id"],
        source_path="src/Hero.tsx",
        symbol="Hero",
    )
    event_trace = service.event_to_source_trace(
        code["capture_id"],
        event_capture_id=layout["capture_id"],
        event_edge_id="interaction:hero-click",
    )

    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "code-graph.schema.json").read_text()
    )
    Draft202012Validator(schema).validate(
        {key: value for key, value in graph.items() if key != "citation"}
    )
    assert any(node["domain_type"] == "Component" for node in graph["nodes"])
    assert any(node["domain_type"] == "DesignToken" for node in graph["nodes"])
    assert any(node["domain_type"] == "CSSSelector" for node in graph["nodes"])
    assert graph["semantic_index"]["enabled"] is False
    assert graph["semantic_index"]["engine"] == "static-pattern-fallback"
    assert graph["runtime_bindings"][0]["authority"] == "OBSERVED"
    assert blast["runtime_node_ids"] == ["#hero"]
    assert blast["affected"]["screenshots"] == [layout["capture_id"]]
    assert blast["final_global_gate_required"] is True
    assert source_trace[0]["runtime_bindings"][0]["source_node_id"].endswith(":Hero")
    assert pixel_trace["authority"] == "OBSERVED"
    assert pixel_trace["pixel_regions"][0]["bounds"]["width"] == 300
    assert event_trace["authority"] == "OBSERVED"
    assert event_trace["source_nodes"][0]["name"] == "Hero"
    assert bus.verify(code["capture_id"])["valid"] is True


def test_typescript_compiler_sdk_must_be_confined_to_repository(tmp_path: Path) -> None:
    repository = tmp_path / "repository"
    _repository(repository)
    adapter = CodeRepositoryAdapter()
    target = adapter.normalize_target({"path": str(repository)})

    with pytest.raises(ValueError, match="repository-relative confined path"):
        adapter.normalize_config(
            target,
            {"typescript_package_path": "/tmp/untrusted-typescript"},
        )


@pytest.mark.skipif(
    not os.environ.get("BVMCP_TYPESCRIPT_SEMANTIC_REPOSITORY"),
    reason="set BVMCP_TYPESCRIPT_SEMANTIC_REPOSITORY to a prepared owned npm repository",
)
def test_real_typescript_compiler_semantic_graph(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "TypeScript semantics")
    registry = AdapterRegistry()
    registry.register(CodeRepositoryAdapter())
    bus = CaptureBus(project, registry)
    capture = bus.observe(
        "code.repository",
        {"path": os.environ["BVMCP_TYPESCRIPT_SEMANTIC_REPOSITORY"]},
        {
            "typescript_package_path": "node_modules/typescript",
            "typescript_tsconfig": "tsconfig.json",
        },
        rights_decision="SYNTHETIC_OWNED",
    )
    graph = SourceIntelligenceService(project).query.graph(capture["capture_id"], "CodeGraph")
    semantic_artifact = next(
        item for item in capture["artifacts"] if item["role"] == "source.typescript-semantic-index"
    )
    semantic_document = json.loads(
        bus.artifacts.path_for(semantic_artifact["digest"]).read_text(encoding="utf-8")
    )
    semantic_schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "typescript-semantic-index.schema.json").read_text(
            encoding="utf-8"
        )
    )
    Draft202012Validator(semantic_schema).validate(semantic_document)

    assert graph["semantic_index"]["enabled"] is True
    assert graph["semantic_index"]["engine"] in {
        "typescript-compiler-api",
        "typescript-native-compiler-api",
    }
    assert graph["semantic_index"]["symbol_count"] > 0
    assert graph["semantic_index"]["resolved_import_count"] > 0
    assert graph["semantic_index"]["reference_count"] > 0
    assert any("compiler_type" in node for node in graph["nodes"])
    assert any(edge["type"] == "RESOLVES_IMPORT" for edge in graph["edges"])
    assert any(edge["type"] == "REFERENCES_SYMBOL" for edge in graph["edges"])
    assert bus.verify(capture["capture_id"])["valid"] is True


def test_workspace_persists_findings_contradictions_compute_and_router_refutation(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Perception workspace")
    repository = tmp_path / "repository"
    _repository(repository)
    image_path = tmp_path / "owned.png"
    Image.new("RGB", (160, 100), "#125cd2").save(image_path)
    registry = AdapterRegistry()
    registry.register(CodeRepositoryAdapter())
    registry.register(ImageFileAdapter())
    bus = CaptureBus(project, registry)
    code = bus.observe(
        "code.repository",
        {"path": str(repository)},
        {
            "runtime_bindings": [
                {
                    "runtime_node_id": "#missing",
                    "source_path": "src/Missing.tsx",
                    "symbol": "Missing",
                }
            ]
        },
        rights_decision="SYNTHETIC_OWNED",
    )
    image = bus.observe(
        "image.file",
        {"path": str(image_path)},
        {"ocr": False},
        rights_decision="SYNTHETIC_OWNED",
    )
    workspace = PerceptionWorkspace(project)
    run = workspace.run([code["capture_id"], image["capture_id"]], compute_budget=8)
    reused = workspace.run([image["capture_id"], code["capture_id"]], compute_budget=8)
    benchmark = workspace.benchmark_router(
        [
            {
                "id": "static",
                "graph_types": ["ImageGraph", "CodeGraph"],
                "required_specialists": [
                    "Pixel Analyst",
                    "Code-Binding Analyst",
                    "Source/Rights Analyst",
                ],
            },
            {
                "id": "motion",
                "graph_types": ["MotionGraph"],
                "required_specialists": [
                    "Motion Analyst",
                    "Adversarial Reviewer",
                ],
            },
        ],
        maximum_specialists=4,
    )

    assert run["status"] == "COMPLETE"
    assert run["tasks"]
    assert all(finding["evidence_references"] for finding in run["findings"])
    assert run["compute"]["used_units"] <= run["compute"]["budget_units"]
    assert any(item["kind"] == "UNRESOLVED_RUNTIME_BINDING" for item in run["contradictions"])
    assert reused["reused"] is True
    assert benchmark["status"] == "REFUTED"
    assert benchmark["active_router"] == "deterministic-v1"
    assert benchmark["matched_compute"]["caller_supplied_scores_trusted"] is False
    learning = PerceptionLearningService(project).start_from_workspace(
        run["id"],
        model_level="project_few_shot_adapter",
        model_identity={"name": "workspace-specialist", "revision": "baseline-v1"},
        correction_budget=2,
    )
    assert learning["status"] == "AWAITING_CORRECTIONS"
    assert learning["source_workspace_digest"] == run["artifact_digest"]
    assert all(item["evidence_references"] for item in learning["correction_requests"])
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM perception_specialist_tasks").fetchone()[
            0
        ] == len(run["tasks"])
        assert (
            connection.execute("SELECT COUNT(*) FROM perception_router_examples").fetchone()[0] == 1
        )
