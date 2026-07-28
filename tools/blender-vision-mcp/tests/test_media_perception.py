from __future__ import annotations

import json
import shutil
import subprocess
from pathlib import Path

import numpy as np
import pytest
from jsonschema import Draft202012Validator
from PIL import Image, ImageDraw

from blender_vision.mcp.server import create_server
from blender_vision.perception import (
    AdapterRegistry,
    CameraFrameAdapter,
    CaptureBus,
    DesktopSnapshotAdapter,
    ImageFileAdapter,
    LiveCameraAdapter,
    MediaReconstructionService,
    ObservationQueryService,
    VideoFileAdapter,
)
from blender_vision.projects.store import ProjectStore


def _fixture_image(path: Path, *, offset: int = 0) -> None:
    image = Image.new("RGB", (320, 200), "white")
    draw = ImageDraw.Draw(image)
    draw.rectangle((40 + offset, 45, 180 + offset, 130), fill="#125cd2")
    draw.rectangle((205, 50, 290, 120), outline="#111111", width=4)
    draw.text((55 + offset, 75), "VISION", fill="white", stroke_width=1)
    image.save(path)


def _bus(tmp_path: Path) -> tuple[ProjectStore, CaptureBus]:
    project = ProjectStore.create(tmp_path / "project", "Media perception")
    registry = AdapterRegistry()
    registry.register(ImageFileAdapter())
    registry.register(CameraFrameAdapter())
    registry.register(LiveCameraAdapter())
    registry.register(VideoFileAdapter())
    registry.register(DesktopSnapshotAdapter())
    return project, CaptureBus(project, registry)


def _validate_graph(graph: dict[str, object], schema_name: str) -> None:
    schema = json.loads(
        (Path(__file__).parents[1] / "schemas" / schema_name).read_text(encoding="utf-8")
    )
    Draft202012Validator(schema).validate(
        {key: value for key, value in graph.items() if key != "citation"}
    )


def test_image_and_authorized_camera_frames_are_traceable_and_queryable(
    tmp_path: Path,
) -> None:
    project, bus = _bus(tmp_path)
    source = tmp_path / "owned.png"
    _fixture_image(source)
    depth = tmp_path / "owned-depth.png"
    Image.new("I;16", (320, 200), 1024).save(depth)

    image = bus.observe(
        "image.file",
        {"path": str(source)},
        {
            "ocr": True,
            "depth_path": str(depth),
            "depth_kind": "sensor",
            "depth_calibration": {"unit": "millimetres", "scale": 1.0},
            "depth_encoding": "uint16-millimetres",
        },
        rights_decision="SYNTHETIC_OWNED",
    )
    with pytest.raises(PermissionError, match="user_authorized"):
        bus.observe(
            "camera.frame",
            {"path": str(source)},
            {},
            rights_decision="USER_CAPTURE",
        )
    camera = bus.observe(
        "camera.frame",
        {"path": str(source)},
        {"user_authorized": True, "device_label": "owned-test-camera"},
        rights_decision="USER_CAPTURE",
    )

    query = ObservationQueryService(project, bus)
    graph = query.graph(image["capture_id"], "ImageGraph")
    matches = query.query(
        image["capture_id"],
        {"point": {"x": 80, "y": 80}},
    )
    _validate_graph(graph, "image-graph.schema.json")
    assert image["summary"]["region_count"] >= 1
    assert matches["graph_type"] == "ImageGraph"
    assert any(node["domain_type"] == "VisualRegion" for node in matches["matches"])
    depth_node = next(
        node for node in graph["nodes"] if node["domain_type"] == "DepthMap"
    )
    assert depth_node["authority"] == "OBSERVED"
    assert graph["depth"]["status"] == "AVAILABLE"
    assert all(node["evidence_references"] for node in graph["nodes"])
    assert camera["summary"]["width"] == 320
    assert bus.verify(camera["capture_id"])["valid"] is True
    interface = MediaReconstructionService(project).reconstruct_interface(
        image["capture_id"]
    )
    assert interface["status"] == "CANDIDATE"
    assert interface["authority"] == "HYPOTHESIS"
    assert interface["implementation_contract"]["copy_reference_source"] is False
    assert MediaReconstructionService(project).reconstruct_interface(
        image["capture_id"]
    )["reused"] is True


def test_live_camera_is_explicit_bounded_and_emits_replayable_sequence(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    project, bus = _bus(tmp_path)

    class FakeCamera:
        def __init__(self) -> None:
            self.index = 0
            self.released = False

        def isOpened(self) -> bool:  # noqa: N802 - OpenCV protocol
            return True

        def set(self, _key: int, _value: int) -> bool:
            return True

        def read(self) -> tuple[bool, np.ndarray]:
            frame = np.full((48, 64, 3), 20 + self.index * 30, dtype=np.uint8)
            self.index += 1
            return True, frame

        def release(self) -> None:
            self.released = True

    camera = FakeCamera()
    monkeypatch.setattr(
        "blender_vision.perception.media.cv2.VideoCapture",
        lambda _index: camera,
    )
    with pytest.raises(PermissionError, match="user_authorized"):
        bus.observe(
            "camera.live",
            {"device_index": 0, "label": "owned", "session_id": "session-1"},
            {"frame_count": 2},
            rights_decision="USER_CAPTURE",
        )
    capture = bus.observe(
        "camera.live",
        {"device_index": 0, "label": "owned", "session_id": "session-1"},
        {
            "user_authorized": True,
            "frame_count": 2,
            "interval_ms": 0,
            "calibration": {"fx": 1000, "fy": 1000},
        },
        rights_decision="USER_CAPTURE",
    )
    graph = ObservationQueryService(project).graph(
        capture["capture_id"], "CameraSequenceGraph"
    )
    schema = json.loads(
        (
            Path(__file__).parents[1]
            / "schemas"
            / "camera-sequence-graph.schema.json"
        ).read_text()
    )
    Draft202012Validator(schema).validate(
        {key: value for key, value in graph.items() if key != "citation"}
    )
    assert capture["summary"]["frame_count"] == 2
    assert graph["metadata"]["live_camera"]["calibration"]["fx"] == 1000
    assert camera.released is True
    assert bus.verify(capture["capture_id"])["valid"] is True


@pytest.mark.skipif(
    not shutil.which("ffmpeg") or not shutil.which("ffprobe"),
    reason="ffmpeg and ffprobe are required for the governed video adapter",
)
def test_video_builds_sampled_frames_tracks_scenes_and_uncalibrated_motion(
    tmp_path: Path,
) -> None:
    project, bus = _bus(tmp_path)
    frames = tmp_path / "frames"
    frames.mkdir()
    for index, offset in enumerate((0, 18, 36), start=1):
        _fixture_image(frames / f"frame-{index:03d}.png", offset=offset)
    video = tmp_path / "owned.mp4"
    subprocess.run(
        [
            shutil.which("ffmpeg") or "ffmpeg",
            "-loglevel",
            "error",
            "-framerate",
            "2",
            "-i",
            str(frames / "frame-%03d.png"),
            "-c:v",
            "mpeg4",
            "-pix_fmt",
            "yuv420p",
            "-y",
            str(video),
        ],
        check=True,
        capture_output=True,
        timeout=30,
    )

    capture = bus.observe(
        "video.file",
        {"path": str(video)},
        {"sample_count": 3, "maximum_dimension": 320},
        rights_decision="SYNTHETIC_OWNED",
    )
    graph = ObservationQueryService(project).graph(
        capture["capture_id"], "VideoNarrativeGraph"
    )

    _validate_graph(graph, "video-narrative-graph.schema.json")
    assert capture["summary"]["sample_count"] == 3
    assert len(graph["nodes"]) == 3
    assert len(graph["camera_motion"]) == 2
    assert graph["depth"]["status"] == "UNAVAILABLE"
    assert all(
        item["classification"] == "camera_or_global_image_motion_2d"
        for item in graph["camera_motion"]
    )
    assert bus.verify(capture["capture_id"])["valid"] is True


def test_authorized_desktop_snapshot_synchronizes_accessibility_and_pixels(
    tmp_path: Path,
) -> None:
    project, bus = _bus(tmp_path)
    screenshot = tmp_path / "desktop.png"
    _fixture_image(screenshot)
    accessibility = tmp_path / "desktop-ax.json"
    accessibility.write_text(
        json.dumps(
            {
                "windows": [{"id": "main", "title": "Owned Fixture"}],
                "nodes": [
                    {
                        "id": "primary",
                        "role": "button",
                        "name": "Primary action",
                        "bounds": {"x": 35, "y": 40, "width": 155, "height": 100},
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    target = {
        "path": str(screenshot),
        "application": "Owned Fixture",
        "window_title": "Test Window",
    }
    with pytest.raises(PermissionError, match="user_authorized"):
        bus.observe(
            "desktop.authorized_snapshot",
            target,
            {"accessibility_path": str(accessibility)},
            rights_decision="USER_CAPTURE",
        )
    capture = bus.observe(
        "desktop.authorized_snapshot",
        target,
        {
            "user_authorized": True,
            "accessibility_path": str(accessibility),
            "ocr": False,
        },
        rights_decision="USER_CAPTURE",
    )
    graph = ObservationQueryService(project).graph(
        capture["capture_id"], "DesktopExperienceGraph"
    )

    _validate_graph(graph, "desktop-experience-graph.schema.json")
    assert any(node["domain_type"] == "AccessibilityNode" for node in graph["nodes"])
    assert any(edge["type"] == "CORRESPONDS_TO" for edge in graph["edges"])
    assert capture["summary"]["correspondence_count"] >= 1
    assert bus.verify(capture["capture_id"])["valid"] is True


@pytest.mark.asyncio
async def test_media_adapters_and_region_explanation_are_public_mcp_tools(
    tmp_path: Path,
) -> None:
    project = ProjectStore.create(tmp_path / "project", "Media MCP")
    source = tmp_path / "owned.png"
    _fixture_image(source)
    server = create_server(tmp_path / "projects")
    _content, adapters = await server.call_tool("vision.adapters", {})
    names = {item["name"] for item in adapters["adapters"]}
    assert {
        "image.file",
        "camera.frame",
        "camera.live",
        "video.file",
        "desktop.authorized_snapshot",
    } <= names
    _content, capture = await server.call_tool(
        "vision.observe",
        {
            "project_path": str(project.root),
            "adapter": "image.file",
            "target": {"path": str(source)},
            "configuration": {"ocr": False},
            "rights_decision": "SYNTHETIC_OWNED",
        },
    )
    _content, explanation = await server.call_tool(
        "vision.explain_region",
        {
            "project_path": str(project.root),
            "capture_id": capture["capture_id"],
            "x": 80,
            "y": 80,
        },
    )
    assert explanation["graph_type"] == "ImageGraph"
    assert any(item["authority"] == "DERIVED" for item in explanation["explanations"])
