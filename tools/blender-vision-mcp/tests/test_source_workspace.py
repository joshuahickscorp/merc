from __future__ import annotations

import json
from pathlib import Path
from typing import Any

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

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
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
        screenshot = sink(
            "screenshot.viewport", b"owned-layout-pixels", "image/png", None
        )
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
        return CaptureOutcome(
            summary={"node_count": 1},
            graphs=[
                {
                    "graph_type": "LayoutGraph",
                    "role": "layout.graph",
                    "node_count": 1,
                    "edge_count": 0,
                    "authority": "OBSERVED",
                }
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
    visual = service.query.query(
        layout["capture_id"], {"point": {"x": 20, "y": 20}}
    )
    source_trace = service.explain_bindings(visual["matches"], code["capture_id"])

    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "code-graph.schema.json").read_text()
    )
    Draft202012Validator(schema).validate(
        {key: value for key, value in graph.items() if key != "citation"}
    )
    assert any(node["domain_type"] == "Component" for node in graph["nodes"])
    assert any(node["domain_type"] == "DesignToken" for node in graph["nodes"])
    assert any(node["domain_type"] == "CSSSelector" for node in graph["nodes"])
    assert graph["runtime_bindings"][0]["authority"] == "OBSERVED"
    assert blast["runtime_node_ids"] == ["#hero"]
    assert blast["affected"]["screenshots"] == [layout["capture_id"]]
    assert blast["final_global_gate_required"] is True
    assert source_trace[0]["runtime_bindings"][0]["source_node_id"].endswith(":Hero")
    assert bus.verify(code["capture_id"])["valid"] is True


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
    reused = workspace.run(
        [image["capture_id"], code["capture_id"]], compute_budget=8
    )
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
    assert any(
        item["kind"] == "UNRESOLVED_RUNTIME_BINDING"
        for item in run["contradictions"]
    )
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
    assert all(
        item["evidence_references"] for item in learning["correction_requests"]
    )
    with project.connection() as connection:
        assert (
            connection.execute(
                "SELECT COUNT(*) FROM perception_specialist_tasks"
            ).fetchone()[0]
            == len(run["tasks"])
        )
        assert (
            connection.execute(
                "SELECT COUNT(*) FROM perception_router_examples"
            ).fetchone()[0]
            == 1
        )
