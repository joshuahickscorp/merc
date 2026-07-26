from __future__ import annotations

import asyncio
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

from blender_vision.mcp.server import create_server
from blender_vision.perception import (
    AdapterRegistry,
    BrowserExperienceAdapter,
    CaptureBus,
    MotionGraphReplay,
    ObservationQueryService,
)
from blender_vision.projects.store import ProjectStore

_CROSS_BROWSER_ENGINES = [
    item
    for item in os.environ.get("BVMCP_CROSS_BROWSER_ENGINES", "webkit").split(",")
    if item
]


@contextlib.contextmanager
def experience_server() -> Iterator[str]:
    fixture = Path(__file__).parent / "fixtures" / "web" / "experience"

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


def test_experience_configuration_is_bounded_and_environment_identified() -> None:
    adapter = BrowserExperienceAdapter()
    target = adapter.normalize_target({"url": "http://127.0.0.1:8123/"})
    config = adapter.normalize_config(
        target,
        {
            "allowed_origins": ["http://127.0.0.1:8123"],
            "allow_private_network": True,
            "responsive_viewports": [
                {"width": 1200, "height": 700},
                {"width": 360, "height": 700},
            ],
            "action_limit": 1000,
            "input_modes": ["touch", "keyboard", "unsupported"],
        },
    )

    assert config["responsive_viewports"] == [
        {"width": 360, "height": 700},
        {"width": 1200, "height": 700},
    ]
    assert config["action_limit"] == 64
    assert config["input_modes"] == ["keyboard", "touch"]
    assert config["engine"] == "chromium"
    environment = adapter.environment(config)
    assert environment["capture_mode"] == "experience"
    assert environment["responsive_viewports"] == config["responsive_viewports"]


def test_motion_replay_interpolates_only_inside_observed_interval() -> None:
    graph = {
        "graph_type": "MotionGraph",
        "nodes": [
            {
                "selector": "#card",
                "animation_samples": [
                    {
                        "timestamp": 0,
                        "opacity": 0.0,
                        "transformMatrix2d": [1, 0, 0, 1, 0, 0],
                        "bounds": {"x": 0, "y": 0, "width": 100, "height": 50},
                    },
                    {
                        "timestamp": 1000,
                        "opacity": 1.0,
                        "transformMatrix2d": [1, 0, 0, 1, 120, 0],
                        "bounds": {"x": 120, "y": 0, "width": 100, "height": 50},
                    },
                ],
            }
        ],
    }
    replay = MotionGraphReplay(graph)

    midpoint = replay.sample("#card", 500)

    assert midpoint["authority"] == "DERIVED"
    assert midpoint["opacity"] == pytest.approx(0.5)
    assert midpoint["transformMatrix2d"][4] == pytest.approx(60)
    assert midpoint["evidence_interval"] == [0, 1000]
    with pytest.raises(ValueError, match="extrapolation"):
        replay.sample("#card", 1001)


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_BROWSER_TESTS=1 to launch the installed Chrome browser",
)
def test_real_experience_capture_discovers_state_responsive_interaction_and_motion(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Experience perception")
    adapter = BrowserExperienceAdapter()
    registry = AdapterRegistry()
    registry.register(adapter)
    bus = CaptureBus(project, registry)
    query = ObservationQueryService(project, bus)

    with experience_server() as origin:
        arguments = {
            "project_path": str(project.root),
            "target_url": f"{origin}/index.html",
            "rights_decision": "SYNTHETIC_OWNED",
            "allowed_origins": [origin],
            "viewport_width": 800,
            "viewport_height": 600,
            "responsive_viewports": [
                {"width": 360, "height": 700},
                {"width": 800, "height": 700},
            ],
            "action_limit": 12,
            "input_modes": ["pointer", "keyboard", "touch"],
            "timeline_duration_ms": 1200,
            "timeline_step_ms": 300,
            "scroll_steps": 3,
            "allow_private_network": True,
            "browser_channel": "chrome",
        }
        capture = bus.observe(
            adapter.name,
            {"url": arguments["target_url"]},
            {
                "allowed_origins": arguments["allowed_origins"],
                "allow_private_network": arguments["allow_private_network"],
                "channel": arguments["browser_channel"],
                "viewport": {"width": 800, "height": 600},
                "responsive_viewports": arguments["responsive_viewports"],
                "action_limit": arguments["action_limit"],
                "input_modes": arguments["input_modes"],
                "timeline_duration_ms": arguments["timeline_duration_ms"],
                "timeline_step_ms": arguments["timeline_step_ms"],
                "scroll_steps": arguments["scroll_steps"],
            },
            rights_decision="SYNTHETIC_OWNED",
        )

        async def exercise_mcp() -> tuple[dict[str, Any], dict[str, Any]]:
            server = create_server(tmp_path / "projects")
            _content, observed = await server.call_tool("vision.discover_states", arguments)
            _content, analyzed = await server.call_tool(
                "vision.analyze_motion",
                {
                    "project_path": str(project.root),
                    "capture_id": capture["capture_id"],
                    "selector": "#animated-card",
                },
            )
            return observed, analyzed

        mcp_observed, mcp_motion = asyncio.run(exercise_mcp())

    assert capture["summary"]["state_count"] >= 8
    assert capture["summary"]["interaction_count"] >= 12
    assert capture["summary"]["responsive_observation_count"] == 2
    assert capture["summary"]["motion_timeline_count"] == 2
    assert bus.verify(capture["capture_id"])["valid"] is True
    assert mcp_observed["reused"] is True
    assert mcp_motion["tracks"][0]["selector"] == "#animated-card"

    states = query.graph(capture["capture_id"], "StateGraph")
    interactions = query.graph(capture["capture_id"], "InteractionGraph")
    responsive = query.graph(capture["capture_id"], "ResponsiveGraph")
    motion = query.graph(capture["capture_id"], "MotionGraph")
    repository = Path(__file__).parents[1]
    for graph, schema_name in (
        (states, "state-graph.schema.json"),
        (interactions, "interaction-graph.schema.json"),
        (responsive, "responsive-graph.schema.json"),
        (motion, "motion-graph.schema.json"),
    ):
        schema = json.loads((repository / "schemas" / schema_name).read_text())
        without_citation = {key: value for key, value in graph.items() if key != "citation"}
        Draft202012Validator(schema).validate(without_citation)

    visible_sets = [
        {element["selector"] for element in state["visible_elements"]}
        for state in states["nodes"]
    ]
    assert any("#site-menu" in visible for visible in visible_sets)
    assert any("#fixture-dialog" in visible for visible in visible_sets)
    assert any("#panel-two" in visible for visible in visible_sets)
    assert any("#form-error" in visible for visible in visible_sets)
    assert all(state["evidence_references"] for state in states["nodes"])

    observed_modes = {
        edge["input"]["mode"]
        for edge in interactions["edges"]
        if edge.get("status") == "OBSERVED"
    }
    assert observed_modes == {"pointer", "keyboard", "touch"}

    mobile, desktop = responsive["nodes"]
    mobile_selectors = {element["selector"] for element in mobile["elements"]}
    desktop_selectors = {element["selector"] for element in desktop["elements"]}
    assert "#mobile-nav" in mobile_selectors
    assert "#desktop-nav" not in mobile_selectors
    assert "#desktop-nav" in desktop_selectors
    assert "#mobile-nav" not in desktop_selectors
    assert responsive["edges"][0]["reflow"]["added"]
    assert responsive["edges"][0]["reflow"]["removed"]

    animation = next(
        item for item in motion["animations"] if item["selector"] == "#animated-card"
    )
    assert animation["timing"]["duration"] == 1000
    assert animation["timing"]["delay"] == 100
    replayed = MotionGraphReplay(motion).sample("#animated-card", 600)
    assert replayed["transformMatrix2d"][4] == pytest.approx(60, abs=2)
    parallax = next(
        item
        for item in motion["inference"]["parallax"]
        if item["selector"] == "#parallax-card"
    )
    assert parallax["transform_y_per_scroll_y"] == pytest.approx(0.25, abs=0.02)
    assert any(
        sample["selector"] == "#sticky-note"
        for sample in motion["inference"]["sticky_or_pinned_samples"]
    )
    assert motion["reduced_motion_variant"]["animations"] == []
    assert motion["inference"]["movement_classification"]["camera_motion"] == "not_observed"


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_CROSS_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_CROSS_BROWSER_TESTS=1 to launch managed Firefox and WebKit",
)
@pytest.mark.parametrize("engine", _CROSS_BROWSER_ENGINES)
def test_real_additional_engine_replays_keyboard_touch_responsive_and_motion(
    tmp_path: Path,
    engine: str,
) -> None:
    project = ProjectStore.create(tmp_path / engine, f"{engine} experience")
    adapter = BrowserExperienceAdapter()
    registry = AdapterRegistry()
    registry.register(adapter)
    bus = CaptureBus(project, registry)

    with experience_server() as origin:
        capture = bus.observe(
            adapter.name,
            {"url": f"{origin}/index.html"},
            {
                "engine": engine,
                "allowed_origins": [origin],
                "allow_private_network": True,
                "viewport": {"width": 390, "height": 844},
                "device_scale_factor": 2,
                "orientation": "portrait",
                "has_touch": True,
                "responsive_viewports": [
                    {"width": 390, "height": 844},
                    {"width": 900, "height": 700},
                ],
                "input_modes": ["keyboard", "touch"],
                "action_limit": 4,
                "timeline_duration_ms": 100,
                "timeline_step_ms": 100,
                "scroll_steps": 2,
            },
            rights_decision="SYNTHETIC_OWNED",
        )

    assert capture["summary"]["browser_engine"] == engine
    assert capture["summary"]["state_count"] >= 2
    assert capture["summary"]["interaction_count"] >= 2
    assert capture["summary"]["responsive_observation_count"] == 2
    assert capture["summary"]["motion_timeline_count"] == 2
    assert capture["summary"]["accessibility_critical_or_serious_count"] == 0
    assert capture["summary"]["keyboard_journey_status"] in {
        "COMPLETE_CYCLE",
        "COMPLETE_DOCUMENT",
    }
    assert bus.verify(capture["capture_id"])["valid"] is True
