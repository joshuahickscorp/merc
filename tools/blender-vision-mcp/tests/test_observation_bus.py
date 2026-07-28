from __future__ import annotations

import json
from pathlib import Path
from typing import Any

import pytest

from blender_vision.core.util import canonical_json
from blender_vision.perception.bus import AdapterRegistry, CaptureBus
from blender_vision.perception.contracts import ArtifactSink, CaptureOutcome
from blender_vision.projects.store import ProjectStore


class FakeAdapter:
    name = "test.fake"
    version = "7"

    def __init__(self, *, interrupt_once: bool = False):
        self.calls = 0
        self.interrupt_once = interrupt_once

    def normalize_target(self, target: dict[str, Any]) -> dict[str, Any]:
        return {"id": str(target["id"]), "kind": "fixture"}

    def normalize_config(
        self, target: dict[str, Any], config: dict[str, Any]
    ) -> dict[str, Any]:
        del target
        return {
            "viewport": {
                "width": int(config.get("width", 1280)),
                "height": int(config.get("height", 720)),
            },
            "device_scale_factor": float(config.get("device_scale_factor", 1)),
            "color_scheme": str(config.get("color_scheme", "light")),
            "reduced_motion": str(config.get("reduced_motion", "no-preference")),
        }

    def environment(self, config: dict[str, Any]) -> dict[str, Any]:
        return {"sensor": "deterministic-fixture", "configuration": config}

    def capture(
        self,
        target: dict[str, Any],
        config: dict[str, Any],
        sink: ArtifactSink,
    ) -> CaptureOutcome:
        self.calls += 1
        sink("screenshot.viewport", b"stable-pixels", "image/png", {"target": target["id"]})
        if self.interrupt_once and self.calls == 1:
            raise RuntimeError("simulated power loss")
        graph = {
            "schema": "vision.layout-graph/v1",
            "graph_type": "LayoutGraph",
            "authority": "OBSERVED",
            "coordinate_space": "CSS viewport pixels",
            "capture": {"target": target},
            "nodes": [
                {
                    "id": "node:0",
                    "selector": "#fixture",
                    "tag": "button",
                    "role": "button",
                    "text": "Observe",
                    "bounds": {"x": 10, "y": 20, "width": 100, "height": 30},
                    "styles": {"color": "rgb(0, 0, 0)"},
                    "sourceBinding": {"id": "fixture"},
                    "surface": "DOM",
                    "interactive": True,
                    "depth": 1,
                }
            ],
            "edges": [],
        }
        sink("layout.graph", canonical_json(graph), "application/json", None)
        return CaptureOutcome(
            summary={"node_count": 1},
            limitations=["fixture evidence"],
            graphs=[
                {
                    "graph_type": "LayoutGraph",
                    "role": "layout.graph",
                    "node_count": 1,
                    "edge_count": 0,
                }
            ],
        )


def make_bus(tmp_path: Path, adapter: FakeAdapter) -> tuple[ProjectStore, CaptureBus]:
    project = ProjectStore.create(tmp_path / "project", "Observation bus")
    registry = AdapterRegistry()
    registry.register(adapter)
    return project, CaptureBus(project, registry)


def test_capture_identity_is_idempotent_and_environment_sensitive(tmp_path: Path) -> None:
    adapter = FakeAdapter()
    project, bus = make_bus(tmp_path, adapter)
    target = {"id": "owned-fixture"}
    first = bus.observe(
        adapter.name,
        target,
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    second = bus.observe(
        adapter.name,
        target,
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    changed = bus.observe(
        adapter.name,
        target,
        {"width": 1440},
        rights_decision="SYNTHETIC_OWNED",
    )

    assert first["capture_id"] == second["capture_id"]
    assert first["manifest_digest"] == second["manifest_digest"]
    assert second["reused"] is True
    assert changed["capture_id"] != first["capture_id"]
    assert adapter.calls == 2
    assert bus.verify(first["capture_id"])["valid"] is True
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM observation_captures").fetchone()[0] == 2


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("device_scale_factor", 2),
        ("color_scheme", "dark"),
        ("reduced_motion", "reduce"),
    ],
)
def test_capture_identity_tracks_each_visual_environment_input(
    tmp_path: Path, field: str, value: Any
) -> None:
    adapter = FakeAdapter()
    _, bus = make_bus(tmp_path, adapter)
    baseline = bus.observe(
        adapter.name,
        {"id": "target"},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    changed = bus.observe(
        adapter.name,
        {"id": "target"},
        {field: value},
        rights_decision="SYNTHETIC_OWNED",
    )
    assert changed["capture_id"] != baseline["capture_id"]


def test_interrupted_capture_resumes_without_duplicate_authority(tmp_path: Path) -> None:
    adapter = FakeAdapter(interrupt_once=True)
    project, bus = make_bus(tmp_path, adapter)
    arguments = (adapter.name, {"id": "resumable"}, {})
    with pytest.raises(RuntimeError, match="simulated power loss"):
        bus.observe(*arguments, rights_decision="SYNTHETIC_OWNED")

    resumed = bus.observe(*arguments, rights_decision="SYNTHETIC_OWNED")

    assert resumed["status"] == "COMPLETE"
    assert resumed["attempt_count"] == 2
    assert bus.verify(resumed["capture_id"])["valid"] is True
    with project.connection() as connection:
        assert connection.execute("SELECT COUNT(*) FROM observation_captures").fetchone()[0] == 1
        roles = connection.execute(
            "SELECT role,COUNT(*) AS count FROM observation_capture_artifacts "
            "GROUP BY role ORDER BY role"
        ).fetchall()
        events = connection.execute(
            "SELECT event_type FROM observation_events ORDER BY sequence"
        ).fetchall()
    assert [(row["role"], row["count"]) for row in roles] == [
        ("layout.graph", 1),
        ("screenshot.viewport", 1),
    ]
    assert [row["event_type"] for row in events] == [
        "capture.started",
        "capture.interrupted",
        "capture.started",
        "capture.completed",
    ]


def test_tampering_is_reported_with_the_exact_artifact_role(tmp_path: Path) -> None:
    adapter = FakeAdapter()
    _, bus = make_bus(tmp_path, adapter)
    capture = bus.observe(
        adapter.name,
        {"id": "tamper-test"},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    screenshot = next(
        artifact
        for artifact in capture["artifacts"]
        if artifact["role"] == "screenshot.viewport"
    )
    bus.artifacts.path_for(screenshot["digest"]).write_bytes(b"tampered")

    verification = bus.verify(capture["capture_id"])

    assert verification["valid"] is False
    assert verification["failures"][0]["role"] == "screenshot.viewport"


def test_manifest_is_canonical_json_and_indexes_exact_artifacts(tmp_path: Path) -> None:
    adapter = FakeAdapter()
    _, bus = make_bus(tmp_path, adapter)
    capture = bus.observe(
        adapter.name,
        {"id": "manifest-test"},
        {},
        rights_decision="SYNTHETIC_OWNED",
    )
    manifest_path = bus.artifacts.path_for(capture["manifest_digest"])
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))

    assert manifest_path.read_bytes() == canonical_json(manifest)
    assert [item["role"] for item in manifest["artifacts"]] == [
        "layout.graph",
        "screenshot.viewport",
    ]
