"""MCP surface registration and end-to-end calls for VisionMCP V2 tools."""

from __future__ import annotations

from pathlib import Path

import numpy as np
import pytest
from mcp.server.fastmcp.exceptions import ToolError

from blender_vision.mcp.server import create_server
from blender_vision.projects.store import ProjectStore
from blender_vision.v2.authority import AuthorityClass
from blender_vision.v2.records import ObservationBundle
from blender_vision.v2.validation import verify_payload

V2_TOOL_NAMES = (
    "vision.capture_world",
    "vision.import_depth",
    "vision.import_point_cloud",
    "vision.plan_capture",
    "vision.ask_next_view",
    "vision.build_reconstruction_portfolio",
    "vision.compare_reconstruction_backends",
    "vision.fit_parametric_model",
    "vision.generate_procedural_scene",
    "vision.generate_archetype",
    "vision.infer_materials",
    "vision.solve_lighting",
    "vision.optimize_inverse_render",
    "vision.generate_texture_set",
    "vision.retopologize",
    "vision.generate_uvs",
    "vision.generate_lods",
    "vision.generate_fur",
    "vision.compose_camera_path",
    "vision.compile_cinematic_scene",
    "vision.compile_web_scene",
    "vision.stream_scene_assets",
    "vision.run_perceptual_critics",
    "vision.benchmark_target",
    "vision.promote_candidate",
)

# Stable V1 vision.* tools must remain registered unchanged.
V1_VISION_TOOLS = (
    "vision.adapters",
    "vision.analyze_motion",
    "vision.capture_state",
    "vision.compare",
    "vision.compare_backends",
    "vision.compare_camera_solutions",
    "vision.consolidate_camera_solutions",
    "vision.discover_states",
    "vision.evaluate",
    "vision.explain_region",
    "vision.import_camera_solution",
    "vision.import_feature_detections",
    "vision.import_geometry_evidence",
    "vision.inspect_graphics",
    "vision.observe",
    "vision.progress",
    "vision.propose_pnp_landmarks",
    "vision.propose_pnp_landmarks_from_renders",
    "vision.query",
    "vision.reconstruct",
    "vision.refine_camera",
    "vision.repair",
    "vision.resolve_target",
    "vision.review_camera_solution",
    "vision.review_pnp_landmarks",
    "vision.review_queue",
    "vision.run",
    "vision.solve_calibration_board",
    "vision.solve_cameras",
    "vision.solve_pnp_landmarks",
    "vision.solve_vanishing_points",
    "vision.trace_behavior",
    "vision.transplant_feature",
    "vision.verify",
)


@pytest.fixture
def projects_root(tmp_path: Path) -> Path:
    root = tmp_path / "projects"
    root.mkdir()
    return root


@pytest.fixture
def project(projects_root: Path) -> ProjectStore:
    return ProjectStore.create(projects_root / "v2-surface", "V2 Surface")


@pytest.fixture
def server(projects_root: Path):
    return create_server(projects_root)


async def _call(server, name: str, arguments: dict) -> dict:
    _content, structured = await server.call_tool(name, arguments)
    assert isinstance(structured, dict), structured
    return structured


@pytest.mark.asyncio
async def test_v2_tools_registered_with_schemas(server) -> None:
    tools = await server.list_tools()
    by_name = {tool.name: tool for tool in tools}
    for name in V2_TOOL_NAMES:
        assert name in by_name, f"missing V2 tool {name}"
        tool = by_name[name]
        assert tool.description, f"{name} lacks description"
        schema = tool.inputSchema or {}
        assert schema.get("type") == "object", f"{name} schema type"
        assert "properties" in schema, f"{name} schema missing properties"
    for name in V1_VISION_TOOLS:
        assert name in by_name, f"V1 vision tool disappeared: {name}"
    assert len(V1_VISION_TOOLS) == 34


@pytest.mark.asyncio
async def test_generate_archetype_end_to_end(server, project: ProjectStore) -> None:
    result = await _call(
        server,
        "vision.generate_archetype",
        {
            "project_path": str(project.root),
            "name": "rack_shell",
            "params": {"u_count": 42, "frame_width_m": 0.6},
        },
    )
    assert result["authority"] == AuthorityClass.PROCEDURAL_GROUND_TRUTH.value
    assert result["archetype"]["name"] == "rack_shell"
    assert abs(result["declared_dimensions"]["height_m"] - 1.8669) < 1e-9
    assert result["fingerprint"]
    assert "rack_shell" in result["known_archetypes"]


@pytest.mark.asyncio
async def test_compose_camera_path_end_to_end(server, project: ProjectStore) -> None:
    result = await _call(
        server,
        "vision.compose_camera_path",
        {
            "project_path": str(project.root),
            "path_id": "mcp-flagship",
            "flagship": True,
        },
    )
    record = result["record"]
    verified = verify_payload(record)
    assert verified.RECORD_KIND == "v2.camera-path-graph"
    assert verified.digest == record["digest"]
    assert len(verified.beats) == 9
    assert verified.arc_length_m > 0.0


@pytest.mark.asyncio
async def test_run_perceptual_critics_end_to_end(server, project: ProjectStore) -> None:
    from blender_vision.critics import CriticRole
    from blender_vision.critics.fixtures import evidence_for, load_control_subject

    control_subject = load_control_subject(CriticRole.PRODUCT_PHOTOGRAPHER)
    evidence = evidence_for(control_subject.subject_id)
    result = await _call(
        server,
        "vision.run_perceptual_critics",
        {
            "project_path": str(project.root),
            "subject": {
                "subject_id": control_subject.subject_id,
                "kind": control_subject.kind,
                "metrics": dict(control_subject.metrics),
                "media": dict(control_subject.media),
                "tags": sorted(control_subject.tags),
            },
            "evidence_references": list(evidence.references),
            "evidence_payloads": dict(evidence.payloads),
            "roles": ["product_photographer"],
            "input_authorities": [AuthorityClass.OBSERVED.value],
        },
    )
    record = result["record"]
    verified = verify_payload(record)
    assert verified.RECORD_KIND == "v2.perceptual-critique"
    assert verified.passed is True
    assert "product_photographer" in verified.critics_run


@pytest.mark.asyncio
async def test_ask_next_view_end_to_end(server, project: ProjectStore) -> None:
    target = {
        "target_id": "mug-partial",
        "cells": [
            {
                "region": "front",
                "area_m2": 1.0,
                "covered": True,
                "occlusion_fraction": 0.0,
                "resolution_px": 1600,
                "candidate_predictions": [0.1, 0.12],
            },
            {
                "region": "underside",
                "area_m2": 1.0,
                "covered": False,
                "candidate_predictions": [0.0, 0.8, 0.9],
            },
            {
                "region": "left",
                "area_m2": 0.8,
                "covered": False,
                "candidate_predictions": [0.2, 0.7],
            },
            {
                "region": "right",
                "area_m2": 0.8,
                "covered": True,
                "occlusion_fraction": 0.0,
                "resolution_px": 1600,
            },
        ],
        "scale_authority": "UNRESOLVED",
        "material_confidences": [0.4, 0.35, 0.3],
        "has_scale_reference": False,
        "has_diffuse_light_view": False,
        "has_grazing_light_view": False,
        "has_lens_metadata": False,
        "has_calibration_target": False,
    }
    result = await _call(
        server,
        "vision.ask_next_view",
        {"project_path": str(project.root), "target": target},
    )
    assert result["requests"], "planner should request missing views"
    first = result["requests"][0]
    verified = verify_payload(first)
    assert verified.RECORD_KIND == "v2.next-view-request"
    assert 0 <= verified.priority <= 10
    assert verified.expected_reduction >= 0
    assert verified.capture_instructions
    assert verified.human_instructions
    assert verified.reason


@pytest.mark.asyncio
async def test_promote_candidate_refuses_system_reviewer(
    server, project: ProjectStore
) -> None:
    draft = ObservationBundle(
        id="ob-promote-1",
        target_id="rack",
        authority=AuthorityClass.INFERRED,
    ).seal()
    refused = await _call(
        server,
        "vision.promote_candidate",
        {
            "project_path": str(project.root),
            "record": draft.to_dict(),
            "target_authority": AuthorityClass.OBSERVED.value,
            "reviewer": "system",
            "reason": "automatic promotion attempt",
        },
    )
    assert refused["status"] == "refused"
    assert "review-only" in refused["reason"] or "cannot" in refused["reason"].lower()

    also_refused = await _call(
        server,
        "vision.promote_candidate",
        {
            "project_path": str(project.root),
            "record": draft.to_dict(),
            "target_authority": AuthorityClass.HUMAN_REVIEWED.value,
            "reviewer": "auto",
            "reason": "auto",
        },
    )
    assert also_refused["status"] == "refused"

    missing_reviewer = await _call(
        server,
        "vision.promote_candidate",
        {
            "project_path": str(project.root),
            "record": draft.to_dict(),
            "target_authority": AuthorityClass.MODEL_DERIVED.value,
            "reviewer": "",
            "reason": "empty reviewer",
        },
    )
    assert missing_reviewer["status"] == "refused"


@pytest.mark.asyncio
async def test_promote_candidate_accepted_path(server, project: ProjectStore) -> None:
    draft = ObservationBundle(
        id="ob-promote-2",
        target_id="rack",
        authority=AuthorityClass.INFERRED,
    ).seal()
    accepted = await _call(
        server,
        "vision.promote_candidate",
        {
            "project_path": str(project.root),
            "record": draft.to_dict(),
            "target_authority": AuthorityClass.MODEL_DERIVED.value,
            "reviewer": "alice.reviewer",
            "reason": "multi-view residual within budget after named review",
        },
    )
    assert accepted["status"] == "promoted"
    verified = verify_payload(accepted["record"])
    assert verified.authority is AuthorityClass.MODEL_DERIVED
    assert any("alice.reviewer" in note for note in verified.notes)


@pytest.mark.asyncio
async def test_plan_capture_and_fit_parametric(server, project: ProjectStore) -> None:
    plan = await _call(
        server,
        "vision.plan_capture",
        {
            "project_path": str(project.root),
            "target_id": "box",
            "target": {
                "bounds_min": [-0.5, -0.5, 0.0],
                "bounds_max": [0.5, 0.5, 1.0],
            },
            "budget": 4,
        },
    )
    assert plan["plan"]["budget"] == 4
    assert plan["record"]["record_kind"] == "v2.observation-bundle"
    verify_payload(plan["record"])

    rng = np.random.default_rng(0)
    centre = np.array([0.1, -0.2, 0.3])
    radius = 0.35
    directions = rng.normal(size=(400, 3))
    directions /= np.linalg.norm(directions, axis=1, keepdims=True)
    points = (centre + radius * directions + rng.normal(scale=0.002, size=(400, 3))).tolist()
    fit = await _call(
        server,
        "vision.fit_parametric_model",
        {
            "project_path": str(project.root),
            "points": points,
            "kind": "sphere",
            "iterations": 200,
            "seed": 1,
        },
    )
    assert fit["fit"]["kind"] == "sphere"
    assert fit["fit"]["inlier_ratio"] > 0.8
    assert fit["authority"] == AuthorityClass.MODEL_DERIVED.value


@pytest.mark.asyncio
async def test_tool_error_on_unknown_archetype(server, project: ProjectStore) -> None:
    with pytest.raises(ToolError, match="unknown archetype"):
        await server.call_tool(
            "vision.generate_archetype",
            {
                "project_path": str(project.root),
                "name": "not_a_real_archetype",
            },
        )
