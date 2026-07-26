from __future__ import annotations

import contextlib
import json
import os
import threading
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

import pytest
from jsonschema import Draft202012Validator

from blender_vision.mcp.server import create_server
from blender_vision.perception.browser import BrowserAdapter
from blender_vision.perception.bus import AdapterRegistry, CaptureBus
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore

_CROSS_BROWSER_ENGINES = [
    item
    for item in os.environ.get("BVMCP_CROSS_BROWSER_ENGINES", "webkit").split(",")
    if item
]


@contextlib.contextmanager
def owned_fixture_server() -> Any:
    fixture = Path(__file__).parent / "fixtures" / "web" / "static"

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


def test_browser_adapter_requires_explicit_origin_and_private_network_consent() -> None:
    adapter = BrowserAdapter()
    target = adapter.normalize_target({"url": "http://127.0.0.1:8765/"})

    with pytest.raises(PermissionError, match="allowed_origins"):
        adapter.normalize_config(target, {})
    with pytest.raises(PermissionError, match="private or special-use"):
        adapter.normalize_config(
            target,
            {"allowed_origins": ["http://127.0.0.1:8765"]},
        )

    normalized = adapter.normalize_config(
        target,
        {
            "allowed_origins": ["http://127.0.0.1:8765"],
            "allow_private_network": True,
        },
    )
    assert normalized["viewport"] == {"width": 1280, "height": 720}
    assert normalized["allowed_origins"] == ["http://127.0.0.1:8765"]
    assert normalized["engine"] == "chromium"
    assert normalized["channel"] == "chrome"


def test_browser_adapter_normalizes_engine_device_and_network_authority() -> None:
    adapter = BrowserAdapter()
    target = adapter.normalize_target({"url": "http://127.0.0.1:8765/"})
    base = {
        "allowed_origins": ["http://127.0.0.1:8765"],
        "allow_private_network": True,
    }

    firefox = adapter.normalize_config(
        target,
        {
            **base,
            "engine": "firefox",
            "viewport": {"width": 390, "height": 844},
            "device_scale_factor": 3,
            "has_touch": True,
            "orientation": "portrait",
            "color_scheme": "dark",
            "reduced_motion": "reduce",
        },
    )

    assert firefox["engine"] == "firefox"
    assert firefox["channel"] is None
    assert firefox["has_touch"] is True
    assert firefox["orientation"] == "portrait"
    assert firefox["resolved_executable_path"].endswith("/firefox")
    with pytest.raises(ValueError, match="channels"):
        adapter.normalize_config(target, {**base, "engine": "webkit", "channel": "chrome"})
    with pytest.raises(ValueError, match="orientation"):
        adapter.normalize_config(
            target,
            {
                **base,
                "viewport": {"width": 844, "height": 390},
                "orientation": "portrait",
            },
        )
    with pytest.raises(ValueError, match="CDP"):
        adapter.normalize_config(
            target,
            {**base, "engine": "firefox", "network_profile": "slow-3g"},
        )


def test_browser_adapter_redacts_secrets_recursively() -> None:
    value = BrowserAdapter._redact(
        {
            "Authorization": "Bearer exposed",
            "nested": {"api_key": "also-exposed"},
            "message": "request used Bearer third-exposed",
        }
    )
    url = BrowserAdapter._redact_url(
        "https://example.test/path?token=exposed&safe=yes#fragment"
    )

    assert "exposed" not in json.dumps(value)
    assert "exposed" not in url
    assert url == "https://example.test/path?token=%5BREDACTED%5D&safe=yes"


def test_keyboard_journey_skips_webkit_body_sentinel_before_controls() -> None:
    class Keyboard:
        def __init__(self) -> None:
            self.keys: list[str] = []

        def press(self, key: str) -> None:
            self.keys.append(key)

    class Page:
        def __init__(self) -> None:
            self.keyboard = Keyboard()
            self.focus = iter(
                [
                    {"selector": "body", "tag": "body", "role": None, "name": ""},
                    {"selector": "body", "tag": "body", "role": None, "name": ""},
                    {
                        "selector": "#action",
                        "tag": "button",
                        "role": None,
                        "name": "Action",
                    },
                    {"selector": "body", "tag": "body", "role": None, "name": ""},
                ]
            )

        def evaluate(self, script: str) -> dict[str, Any] | None:
            if "blur()" in script:
                return None
            return next(self.focus)

    page = Page()
    journey = BrowserAdapter._keyboard_journey(
        page,
        {"keyboard_step_limit": 8, "engine": "webkit"},
    )

    assert journey["status"] == "COMPLETE_DOCUMENT"
    assert journey["navigation_keys"] == ["Tab", "Alt+Tab"]
    assert page.keyboard.keys == ["Tab", "Tab", "Alt+Tab", "Alt+Tab"]
    assert journey["unique_focus_targets"] == ["#action"]


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_BROWSER_TESTS=1 to launch the installed Chrome browser",
)
@pytest.mark.asyncio
async def test_real_chrome_capture_produces_queryable_observed_evidence(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Browser perception")
    adapter = BrowserAdapter()
    registry = AdapterRegistry()
    registry.register(adapter)
    bus = CaptureBus(project, registry)
    query = ObservationQueryService(project, bus)
    server = create_server(tmp_path / "projects")

    with owned_fixture_server() as origin:
        arguments = {
            "project_path": str(project.root),
            "target_url": f"{origin}/index.html",
            "rights_decision": "SYNTHETIC_OWNED",
            "allowed_origins": [origin],
            "viewport_width": 800,
            "viewport_height": 600,
            "device_scale_factor": 1,
            "color_scheme": "light",
            "reduced_motion": "reduce",
            "allow_private_network": True,
            "browser_channel": "chrome",
        }
        _content, capture = await server.call_tool(
            "vision.observe",
            arguments,
        )
        _content, reused = await server.call_tool(
            "vision.observe",
            arguments,
        )

    roles = {artifact["role"] for artifact in capture["artifacts"]}
    assert {
        "screenshot.viewport",
        "screenshot.full",
        "dom.html",
        "dom.snapshot",
        "accessibility.tree",
        "accessibility.journey",
        "layout.graph",
        "stylesheets",
        "fonts",
        "assets",
        "network",
        "console",
        "performance",
        "document.metadata",
        "surfaces",
    } <= roles
    assert capture["authority"] == "OBSERVED"
    assert capture["summary"]["http_status"] == 200
    assert capture["summary"]["browser_engine"] == "chromium"
    assert capture["summary"]["accessibility_critical_or_serious_count"] == 0
    assert capture["summary"]["keyboard_journey_status"] in {
        "COMPLETE_CYCLE",
        "COMPLETE_DOCUMENT",
    }
    assert capture["summary"]["node_count"] >= 6
    assert reused["reused"] is True
    assert reused["capture_id"] == capture["capture_id"]
    assert bus.verify(capture["capture_id"])["valid"] is True

    button = query.query(
        capture["capture_id"],
        {"selector": "#observe-button"},
    )
    role = query.query(capture["capture_id"], {"role": "button"})
    asset = query.query(capture["capture_id"], {"asset_url": "/icon.svg"})
    point = query.query(
        capture["capture_id"],
        {
            "point": {
                "x": button["matches"][0]["bounds"]["x"] + 5,
                "y": button["matches"][0]["bounds"]["y"] + 5,
            }
        },
    )
    assert button["matches"][0]["styles"]["backgroundColor"] == "rgb(18, 92, 210)"
    assert button["matches"][0]["sourceBinding"]["id"] == "observe-button"
    assert role["matches"][0]["accessibleName"] == "Observe fixture"
    assert asset["matches"][0]["sourceBinding"]["id"] == "mark"
    assert any(match["selector"] == "#observe-button" for match in point["matches"])
    assert button["citation"]["authority"] == "OBSERVED"
    _content, mcp_button = await server.call_tool(
        "vision.query",
        {
            "project_path": str(project.root),
            "capture_id": capture["capture_id"],
            "query": {"selector": "#observe-button"},
        },
    )
    _content, mcp_verification = await server.call_tool(
        "vision.verify",
        {
            "project_path": str(project.root),
            "capture_id": capture["capture_id"],
        },
    )
    assert mcp_button["matches"][0]["sourceBinding"]["id"] == "observe-button"
    assert mcp_verification["valid"] is True

    graph = query.graph(capture["capture_id"])
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / "layout-graph.schema.json").read_text()
    )
    graph_without_citation = {key: value for key, value in graph.items() if key != "citation"}
    Draft202012Validator(schema).validate(graph_without_citation)

    network_role = next(
        artifact for artifact in capture["artifacts"] if artifact["role"] == "network"
    )
    network_bytes = bus.artifacts.path_for(network_role["digest"]).read_bytes()
    assert b"owned-fixture" not in network_bytes
    assert b"REDACTED" in network_bytes


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_CROSS_BROWSER_TESTS") != "1",
    reason="set BVMCP_RUN_CROSS_BROWSER_TESTS=1 to launch managed Firefox and WebKit",
)
@pytest.mark.parametrize("engine", _CROSS_BROWSER_ENGINES)
def test_real_additional_engine_capture_is_accessible_and_evidence_bound(
    tmp_path: Path,
    engine: str,
) -> None:
    project = ProjectStore.create(tmp_path / engine, f"{engine} perception")
    adapter = BrowserAdapter()
    registry = AdapterRegistry()
    registry.register(adapter)
    bus = CaptureBus(project, registry)

    with owned_fixture_server() as origin:
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
                "color_scheme": "dark",
                "reduced_motion": "reduce",
            },
            rights_decision="SYNTHETIC_OWNED",
        )

    roles = {artifact["role"] for artifact in capture["artifacts"]}
    assert capture["summary"]["browser_engine"] == engine
    assert capture["summary"]["http_status"] == 200
    assert capture["summary"]["accessibility_critical_or_serious_count"] == 0
    assert capture["summary"]["keyboard_journey_status"] in {
        "COMPLETE_CYCLE",
        "COMPLETE_DOCUMENT",
    }
    assert capture["environment"]["browser_engine"] == engine
    assert capture["environment"]["browser_executable_sha256"]
    assert {"accessibility.tree", "accessibility.journey", "layout.graph"} <= roles
    assert bus.verify(capture["capture_id"])["valid"] is True
