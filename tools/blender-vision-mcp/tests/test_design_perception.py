from __future__ import annotations

import json
from pathlib import Path

import pytest
from jsonschema import Draft202012Validator
from PIL import Image

from blender_vision.mcp.server import create_server
from blender_vision.perception import (
    AdapterRegistry,
    CaptureBus,
    DesignIntelligenceService,
    FigmaExportAdapter,
    ObservationQueryService,
    StorybookExportAdapter,
)
from blender_vision.projects.store import ProjectStore


def make_design_bus(tmp_path: Path) -> tuple[ProjectStore, CaptureBus]:
    project = ProjectStore.create(tmp_path / "project", "Design perception")
    registry = AdapterRegistry()
    registry.register(FigmaExportAdapter())
    registry.register(StorybookExportAdapter())
    return project, CaptureBus(project, registry)


def test_figma_and_storybook_exports_compile_to_traceable_design_graphs(
    tmp_path: Path,
) -> None:
    project, bus = make_design_bus(tmp_path)
    fixtures = Path(__file__).parent / "fixtures" / "design"
    rendered = tmp_path / "figma-screen.png"
    Image.new("RGB", (480, 640), "#ffffff").save(rendered)

    figma = bus.observe(
        "design.figma_export",
        {"path": str(fixtures / "figma-owned.json")},
        {"rendered_image_path": str(rendered)},
        rights_decision="SYNTHETIC_OWNED",
    )
    storybook = bus.observe(
        "design.storybook_export",
        {"path": str(fixtures / "storybook-owned.json")},
        {"framework": "react", "builder": "vite"},
        rights_decision="SYNTHETIC_OWNED",
    )

    assert bus.verify(figma["capture_id"])["valid"] is True
    assert bus.verify(storybook["capture_id"])["valid"] is True
    assert figma["summary"]["component_count"] == 4
    assert storybook["summary"]["component_count"] == 2
    assert storybook["summary"]["story_count"] == 3
    assert {artifact["role"] for artifact in figma["artifacts"]} == {
        "design.graph",
        "design.rendered",
        "design.source",
    }

    query = ObservationQueryService(project)
    figma_graph = query.graph(figma["capture_id"], "DesignSystemGraph")
    story_graph = query.graph(storybook["capture_id"], "DesignSystemGraph")
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "design-system-graph.schema.json").read_text()
    )
    for graph in (figma_graph, story_graph):
        Draft202012Validator(schema).validate(
            {key: value for key, value in graph.items() if key != "citation"}
        )
        assert all(node["evidence_references"] for node in graph["nodes"])
    assert next(
        node for node in story_graph["nodes"] if node["id"] == "storybook:component:button"
    )["import_path"] == "./src/Button.stories.tsx"

    report = DesignIntelligenceService(project).analyze_drift(
        figma["capture_id"],
        storybook["capture_id"],
    )

    assert report["status"] == "PASS"
    assert report["missing_components"] == []
    assert report["missing_variants"] == []
    assert report["token_drift"] == []
    assert report["component_policy"]["one_off_component_count"] == 0
    assert report["component_policy"]["traceable_binding_count"] == 2
    assert all(binding["traceable"] for binding in report["component_bindings"])
    drift_schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "design-drift.schema.json").read_text()
    )
    Draft202012Validator(drift_schema).validate(report)


def test_design_drift_detects_intentional_token_and_variant_regressions(
    tmp_path: Path,
) -> None:
    project, bus = make_design_bus(tmp_path)
    fixtures = Path(__file__).parent / "fixtures" / "design"
    figma = bus.observe(
        "design.figma_export",
        {"path": str(fixtures / "figma-owned.json")},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    drifted = bus.observe(
        "design.storybook_export",
        {"path": str(fixtures / "storybook-drift.json")},
        {"framework": "react", "builder": "vite"},
        rights_decision="SYNTHETIC_OWNED",
    )

    report = DesignIntelligenceService(project).analyze_drift(
        figma["capture_id"],
        drifted["capture_id"],
    )

    assert report["status"] == "DRIFT_DETECTED"
    assert report["token_drift"] == [
        {"token": "coloraction", "figma": "#125CD2", "storybook": "#EE3300"}
    ]
    assert report["missing_variants"] == [
        {"component": "button", "variants": ["statesecondary"]}
    ]
    with project.connection() as connection:
        row = connection.execute(
            "SELECT status,report_digest FROM design_drift_runs"
        ).fetchone()
    assert row["status"] == "DRIFT_DETECTED"
    assert bus.artifacts.path_for(row["report_digest"]).is_file()


@pytest.mark.asyncio
async def test_generic_mcp_observe_and_compare_cover_design_sources(tmp_path: Path) -> None:
    project = ProjectStore.create(tmp_path / "project", "Design MCP")
    fixtures = Path(__file__).parent / "fixtures" / "design"
    server = create_server(tmp_path / "projects")

    _content, figma = await server.call_tool(
        "vision.observe",
        {
            "project_path": str(project.root),
            "rights_decision": "SYNTHETIC_OWNED",
            "adapter": "design.figma_export",
            "target": {"path": str(fixtures / "figma-owned.json")},
            "configuration": {},
        },
    )
    _content, storybook = await server.call_tool(
        "vision.observe",
        {
            "project_path": str(project.root),
            "rights_decision": "SYNTHETIC_OWNED",
            "adapter": "design.storybook_export",
            "target": {"path": str(fixtures / "storybook-owned.json")},
            "configuration": {"framework": "react", "builder": "vite"},
        },
    )
    _content, comparison = await server.call_tool(
        "vision.compare",
        {
            "project_path": str(project.root),
            "capture_a": figma["capture_id"],
            "capture_b": storybook["capture_id"],
        },
    )

    assert comparison["status"] == "PASS"
    assert comparison["component_policy"]["traceable_binding_count"] == 2
