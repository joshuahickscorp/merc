from __future__ import annotations

import contextlib
import json
import os
import threading
from collections.abc import Iterator
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator

from blender_vision.perception import (
    AdapterRegistry,
    CaptureBus,
    GraphicsRoundTripService,
    GraphicsRuntimeAdapter,
    ObservationQueryService,
    RuntimeGltfCompiler,
)
from blender_vision.projects.store import ProjectStore


@contextlib.contextmanager
def graphics_server() -> Iterator[str]:
    fixture = Path(__file__).parent / "fixtures" / "web" / "graphics"

    class QuietHandler(SimpleHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:
            del format, args

    server = ThreadingHTTPServer(
        ("127.0.0.1", 0),
        lambda *args, **kwargs: QuietHandler(*args, directory=fixture, **kwargs),
    )
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


def runtime_scene() -> dict[str, Any]:
    return {
        "revision": "unit-scene-v1",
        "camera": {
            "id": "camera",
            "matrix": [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 2, 1],
            "perspective": {"yfov": 0.7, "znear": 0.1, "zfar": 100, "aspectRatio": 1.0},
        },
        "objects": [
            {
                "id": "triangle",
                "name": "Triangle",
                "positions": [-0.5, -0.5, 0, 0.5, -0.5, 0, 0, 0.5, 0],
                "indices": [0, 1, 2],
                "matrix": [1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1],
                "material": {
                    "name": "Blue",
                    "baseColorFactor": [0.1, 0.3, 0.8, 1],
                },
            }
        ],
    }


def test_runtime_scene_compiles_to_self_contained_gltf() -> None:
    gltf = RuntimeGltfCompiler.compile(runtime_scene())

    assert gltf["asset"]["version"] == "2.0"
    assert gltf["asset"]["generator"] == "VisionMCP RuntimeGltfCompiler/1"
    assert len(gltf["meshes"]) == 1
    assert len(gltf["cameras"]) == 1
    assert gltf["nodes"][0]["extras"]["authority"] == (
        "DERIVED_FROM_EXPLICIT_RUNTIME_HOOK"
    )
    assert gltf["buffers"][0]["uri"].startswith("data:application/octet-stream;base64,")
    assert gltf["accessors"][0]["min"] == [-0.5, -0.5, 0.0]
    assert gltf["accessors"][0]["max"] == [0.5, 0.5, 0.0]


def test_runtime_scene_refuses_to_invent_unobserved_geometry() -> None:
    with pytest.raises(ValueError, match="no materializable mesh geometry"):
        RuntimeGltfCompiler.compile({"revision": "empty", "objects": []})


def capture_graphics(project: ProjectStore) -> tuple[CaptureBus, dict[str, Any]]:
    adapter = GraphicsRuntimeAdapter()
    registry = AdapterRegistry()
    registry.register(adapter)
    bus = CaptureBus(project, registry)
    with graphics_server() as origin:
        capture = bus.observe(
            adapter.name,
            {"url": f"{origin}/index.html"},
            {
                "allowed_origins": [origin],
                "allow_private_network": True,
                "channel": "chrome",
                "viewport": {"width": 800, "height": 600},
                "frame_timestamps_ms": [0, 500, 1000],
                "require_runtime_scene_hook": True,
                "materialize_gltf": True,
            },
            rights_decision="SYNTHETIC_OWNED",
        )
    return bus, capture


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_BROWSER_TESTS=1 to launch the installed Chrome browser",
)
def test_real_webgl_capture_records_runtime_frames_scene_and_performance(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Graphics perception")
    bus, capture = capture_graphics(project)

    assert capture["summary"]["canvas_count"] == 1
    assert capture["summary"]["webgl_canvas_count"] == 1
    assert capture["summary"]["webgpu_canvas_count"] == 0
    assert capture["summary"]["draw_call_count"] >= 4
    assert capture["summary"]["runtime_scene_exposed"] is True
    assert capture["summary"]["gltf_materialized"] is True
    assert bus.verify(capture["capture_id"])["valid"] is True

    graph = ObservationQueryService(project).graph(
        capture["capture_id"], "GraphicsFrameGraph"
    )
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "graphics-frame-graph.schema.json").read_text()
    )
    Draft202012Validator(schema).validate(
        {key: value for key, value in graph.items() if key != "citation"}
    )
    assert graph["surface_classification"] == [
        {"canvas_id": "scene", "surface": "WebGL2"}
    ]
    assert [frame["timestamp_ms"] for frame in graph["frames"]] == [0, 500, 1000]
    final_matrix = graph["frames"][-1]["runtime_scene"]["objects"][0]["matrix"]
    assert final_matrix[0] == pytest.approx(0, abs=1e-6)
    assert final_matrix[1] == pytest.approx(1, abs=1e-6)
    assert graph["runtime"]["performance"]["medianFrameMs"] > 0
    gltf_digest = graph["materialized_gltf"]["artifact_digest"]
    gltf = json.loads(bus.artifacts.path_for(gltf_digest).read_text())
    assert gltf["extras"]["sourceRevision"] == "owned-webgl-triangle-v1"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1"
    or os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set browser and Blender integration gates for the real graphics round trip",
)
def test_real_webgl_to_blender_to_glb_round_trip_is_structurally_verified(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Graphics round trip")
    _bus, capture = capture_graphics(project)

    report = GraphicsRoundTripService(project).round_trip(capture["capture_id"])

    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "graphics-roundtrip.schema.json").read_text()
    )
    Draft202012Validator(schema).validate(report)
    assert report["validation"]["valid"] is True
    assert report["validation"]["mesh_count"] >= 1
    assert report["validation"]["node_count"] >= 1
    assert report["reimport"]["imported_objects"]
    assert report["accepted"] is False
    assert report["fixed_frame_residual"]["status"] == "NOT_EVALUATED"
    assert len(report["acceptance_blockers"]) == 3
    assert (project.root / report["export"]["export_path"]).is_file()
    with project.connection() as connection:
        row = connection.execute(
            "SELECT validation_status,report_digest FROM graphics_roundtrips"
        ).fetchone()
    assert row["validation_status"] == "PASS"
    assert _bus.artifacts.path_for(row["report_digest"]).is_file()
