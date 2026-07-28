from __future__ import annotations

import asyncio
import json
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator

from blender_vision.core.util import canonical_json
from blender_vision.mcp.server import create_server
from blender_vision.perception import (
    AdapterRegistry,
    CaptureBus,
    ExperienceIRCompiler,
    FeatureCapsuleCompiler,
    FeatureCapsuleVerifier,
)
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.projects.store import ProjectStore


class ExperienceFixtureAdapter:
    name = "test.experience-fixture"
    version = "1"

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return {"id": target["id"], "kind": "owned-experience-fixture"}

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target, config
        return {"viewport": {"width": 800, "height": 600}}

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {"runtime": "fixture", "configuration": config}

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        del config
        evidence_node = {
            "id": "layout:hero",
            "selector": "#hero",
            "role": "region",
            "bounds": {"x": 0, "y": 0, "width": 800, "height": 600},
            "styles": {"position": "sticky"},
            "sourceBinding": {"id": "hero"},
        }
        layout = {
            "graph_type": "LayoutGraph",
            "nodes": [evidence_node],
            "edges": [],
        }
        state = {
            "graph_type": "StateGraph",
            "nodes": [
                {
                    "id": "state:idle",
                    "visible_elements": [
                        {
                            "selector": "#hero",
                            "role": "region",
                            "attributes": {"data-state": "idle"},
                        }
                    ],
                    "evidence_references": [{"role": "state.idle", "artifact_digest": "a" * 64}],
                },
                {
                    "id": "state:revealed",
                    "visible_elements": [
                        {
                            "selector": "#hero",
                            "role": "region",
                            "attributes": {"data-state": "revealed"},
                        }
                    ],
                    "evidence_references": [
                        {"role": "state.revealed", "artifact_digest": "b" * 64}
                    ],
                },
            ],
            "edges": [
                {
                    "id": "transition:reveal",
                    "source": "state:idle",
                    "target": "state:revealed",
                    "type": "TRANSITIONS_TO",
                }
            ],
        }
        responsive = {
            "graph_type": "ResponsiveGraph",
            "nodes": [
                {
                    "id": "viewport:360",
                    "viewport": {"width": 360, "height": 700},
                    "elements": [{"selector": "#hero"}],
                    "evidence_references": [{"artifact_digest": "c" * 64}],
                },
                {
                    "id": "viewport:800",
                    "viewport": {"width": 800, "height": 700},
                    "elements": [{"selector": "#hero"}],
                    "evidence_references": [{"artifact_digest": "d" * 64}],
                },
            ],
            "edges": [],
            "input_mode_variants": {"pointer": "observed", "touch": "observed"},
        }
        interaction = {
            "graph_type": "InteractionGraph",
            "nodes": [],
            "edges": [
                {
                    "id": "interaction:reveal",
                    "source": "#hero",
                    "target": "state:revealed",
                    "type": "TRIGGERS",
                    "status": "OBSERVED",
                }
            ],
        }
        motion = {
            "graph_type": "MotionGraph",
            "nodes": [
                {
                    "id": "motion:#hero",
                    "selector": "#hero",
                    "animation": {"duration": 1000, "easing": "linear"},
                    "animation_samples": [{"timestamp": 0}, {"timestamp": 1000}],
                    "scroll_samples": [{"progress": 0}, {"progress": 1}],
                    "evidence_references": ["e" * 64],
                }
            ],
            "edges": [],
            "inference": {"sticky_or_pinned_samples": [{"progress": 0.2}, {"progress": 0.7}]},
            "reduced_motion_variant": {"mode": "crossfade"},
            "replay_contract": {"interpolation": "linear-between-observed-samples"},
        }
        graphics = {
            "graph_type": "GraphicsFrameGraph",
            "nodes": [
                {
                    "id": "graphics:camera:main",
                    "domain_type": "RuntimeCamera",
                    "matrix": [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 2, 1],
                }
            ],
            "edges": [],
            "surface_classification": [{"canvas_id": "hero", "surface": "WebGL2"}],
            "frames": [{"timestamp_ms": 0, "artifact_digest": "f" * 64}],
            "materialized_gltf": {"artifact_digest": "1" * 64, "authority": "DERIVED"},
        }
        design = {
            "graph_type": "DesignSystemGraph",
            "nodes": [
                {
                    "id": "storybook:component:hero",
                    "domain_type": "StorybookComponent",
                    "name": "Hero",
                    "semantic_name": "hero",
                    "source_binding": {"import_path": "./Hero.tsx"},
                }
            ],
            "edges": [],
            "tokens": [
                {
                    "id": "token:action",
                    "name": "color/action",
                    "semantic_name": "coloraction",
                    "value": "#125CD2",
                }
            ],
        }
        graphs = {
            "layout.graph": ("LayoutGraph", layout),
            "state.graph": ("StateGraph", state),
            "responsive.graph": ("ResponsiveGraph", responsive),
            "interaction.graph": ("InteractionGraph", interaction),
            "motion.graph": ("MotionGraph", motion),
            "graphics.graph": ("GraphicsFrameGraph", graphics),
            "design.graph": ("DesignSystemGraph", design),
        }
        descriptors = []
        for role, (graph_type, graph) in graphs.items():
            sink(role, canonical_json(graph), "application/json", None)
            descriptors.append(
                {
                    "graph_type": graph_type,
                    "role": role,
                    "node_count": len(graph["nodes"]),
                    "edge_count": len(graph["edges"]),
                }
            )
        sink(
            "accessibility.tree",
            canonical_json({"nodes": [{"role": "region", "name": "Hero"}]}),
            "application/json",
            None,
        )
        sink(
            "dom.html",
            b"<!doctype html><script>PROTECTED_REFERENCE_IMPLEMENTATION</script>",
            "text/html",
            None,
        )
        return CaptureOutcome(
            summary={"target": target["id"]},
            limitations=["fixture limitation"],
            graphs=descriptors,
        )


def make_experience(tmp_path: Path) -> tuple[ProjectStore, CaptureBus, dict[str, Any]]:
    project = ProjectStore.create(tmp_path / "project", "Feature Capsule")
    registry = AdapterRegistry()
    adapter = ExperienceFixtureAdapter()
    registry.register(adapter)
    bus = CaptureBus(project, registry)
    capture = bus.observe(
        adapter.name,
        {"id": "owned-product-reveal"},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    return project, bus, capture


def test_experience_ir_and_feature_capsule_are_clean_traceable_and_replayable(
    tmp_path: Path,
) -> None:
    project, bus, capture = make_experience(tmp_path)
    ir = ExperienceIRCompiler(project).compile([capture["capture_id"]])
    capsule = FeatureCapsuleCompiler(project).compile(
        ir["id"],
        semantic_purpose="Scroll-linked product reveal",
        kind="SCROLL_PINNED_PRODUCT_REVEAL",
        framework="react",
        performance_budget={"maximum_frame_ms": 20},
        verification_thresholds={"motion_transform_error_px": 2},
        implementation_interface={"props": ["stateId", "renderState"]},
    )
    verification = FeatureCapsuleVerifier(project).verify(capsule["id"])

    assert verification["status"] == "PASS"
    assert verification["clean_room"] is True
    assert all(verification["coverage"].values())
    assert capsule["framework_output"]["contains_reference_source"] is False
    assert capsule["framework_output"]["contains_reference_assets"] is False
    assert "PROTECTED_REFERENCE_IMPLEMENTATION" not in json.dumps(capsule)
    assert "dom.html" not in json.dumps(capsule)
    assert set(capsule["test_fixture"]["state_ids"]) == {
        "state:idle",
        "state:revealed",
    }
    assert capsule["test_fixture"]["reduced_motion_required"] is True
    assert capsule["clean_room_behavior"]["graphics_behavior"]["runtime_nodes"][0][
        "domain_type"
    ] == "RuntimeCamera"
    assert bus.verify(capture["capture_id"])["valid"] is True

    async def exercise_mcp() -> dict[str, Any]:
        server = create_server(tmp_path / "projects")
        _content, result = await server.call_tool(
            "vision.transplant_feature",
            {
                "project_path": str(project.root),
                "capture_ids": [capture["capture_id"]],
                "semantic_purpose": "Scroll-linked product reveal",
                "kind": "SCROLL_PINNED_PRODUCT_REVEAL",
                "framework": "react",
                "performance_budget": {"maximum_frame_ms": 20},
                "verification_thresholds": {"motion_transform_error_px": 2},
                "implementation_interface": {"props": ["stateId", "renderState"]},
            },
        )
        return result

    mcp_result = asyncio.run(exercise_mcp())
    assert mcp_result["capsule"]["id"] == capsule["id"]
    assert mcp_result["evaluation"]["status"] == "PASS"

    repository = Path(__file__).parents[1]
    ir_schema = json.loads((repository / "schemas" / "experience-ir.schema.json").read_text())
    capsule_schema = json.loads(
        (repository / "schemas" / "feature-capsule.schema.json").read_text()
    )
    Draft202012Validator(ir_schema).validate(ir)
    Draft202012Validator(capsule_schema).validate(capsule)


def test_feature_capsule_rejects_unauthorized_reference_assets(tmp_path: Path) -> None:
    project, _bus, capture = make_experience(tmp_path)
    ir = ExperienceIRCompiler(project).compile([capture["capture_id"]])

    with pytest.raises(PermissionError, match="not owned or licensed"):
        FeatureCapsuleCompiler(project).compile(
            ir["id"],
            semantic_purpose="Reveal",
            kind="REVEAL",
            framework="vanilla",
            owned_asset_mappings=[
                {
                    "semantic_role": "product",
                    "replacement_digest": "a" * 64,
                    "rights_state": "PUBLICLY_VISIBLE",
                }
            ],
        )


def test_feature_capsule_tampering_fails_verification(tmp_path: Path) -> None:
    project, _bus, capture = make_experience(tmp_path)
    ir = ExperienceIRCompiler(project).compile([capture["capture_id"]])
    capsule = FeatureCapsuleCompiler(project).compile(
        ir["id"],
        semantic_purpose="Reveal",
        kind="REVEAL",
        framework="vanilla",
    )
    FeatureCapsuleCompiler(project).artifacts.path_for(
        capsule["manifest_digest"]
    ).write_bytes(b"tampered")

    verification = FeatureCapsuleVerifier(project).verify(capsule["id"])

    assert verification["status"] == "FAIL"
    assert verification["integrity"] is False
    assert verification["failures"][0]["role"] == "manifest"
