"""Ocular MCP surface: registration, dispatch, stream timing, webcam BLOCKED."""

from __future__ import annotations

from pathlib import Path

import cv2
import numpy as np
import pytest

from mcp.server.fastmcp.exceptions import ToolError

from blender_vision.mcp.server import create_server
from blender_vision.ocular.attestation import ExecutionClass
from blender_vision.ocular.stream import (
    close_stream,
    iter_frames,
    open_stream,
    open_stream_or_attest,
)
from blender_vision.projects.store import ProjectStore

OCULAR_TOOLS = (
    "vision.open_stream",
    "vision.close_stream",
    "vision.get_stream_state",
    "vision.calibrate_sensor",
    "vision.fixate",
    "vision.saccade",
    "vision.track",
    "vision.reidentify",
    "vision.observe_change",
    "vision.build_world_model",
    "vision.update_world_model",
    "vision.query_world",
    "vision.explain_belief",
    "vision.list_uncertainties",
    "vision.predict_next",
    "vision.list_surprises",
    "vision.plan_capture",
    "vision.ask_next_view",
    "vision.measure_information_gain",
)

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

# Pre-ocular baseline was 269 tools; +17 new ocular names (two already present).
BASELINE_TOOL_FLOOR = 286


def _write_video(path: Path, n_frames: int = 6) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    w, h = 128, 96
    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(str(path), fourcc, 10.0, (w, h))
    if not writer.isOpened():
        path = path.with_suffix(".avi")
        writer = cv2.VideoWriter(str(path), cv2.VideoWriter_fourcc(*"MJPG"), 10.0, (w, h))
    assert writer.isOpened(), "VideoWriter failed"
    for i in range(n_frames):
        frame = np.zeros((h, w, 3), dtype=np.uint8)
        cv2.rectangle(frame, (10 + i * 8, 30), (30 + i * 8, 50), (0, 180, 255), -1)
        writer.write(frame)
    writer.release()
    return path


def _write_image(path: Path) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    image = np.full((96, 128, 3), 50, dtype=np.uint8)
    cv2.imwrite(str(path), image)
    return path


@pytest.fixture
def projects_root(tmp_path: Path) -> Path:
    root = tmp_path / "projects"
    root.mkdir()
    return root


@pytest.fixture
def server(projects_root: Path):
    return create_server(projects_root)


@pytest.fixture
def video_path(tmp_path: Path) -> Path:
    return _write_video(tmp_path / "clip.mp4")


@pytest.fixture
def calib_path(tmp_path: Path) -> Path:
    return _write_image(tmp_path / "calib.png")


async def _call(server, name: str, arguments: dict) -> dict:
    _content, structured = await server.call_tool(name, arguments)
    assert isinstance(structured, dict), structured
    return structured


@pytest.mark.asyncio
async def test_ocular_tools_registered(server) -> None:
    tools = await server.list_tools()
    by_name = {tool.name: tool for tool in tools}
    for name in OCULAR_TOOLS:
        assert name in by_name, f"missing ocular tool {name}"
        assert by_name[name].description, f"{name} lacks description"
    for name in V1_VISION_TOOLS:
        assert name in by_name, f"V1 vision tool disappeared: {name}"
    assert len(V1_VISION_TOOLS) == 34
    assert len(by_name) >= BASELINE_TOOL_FLOOR


@pytest.mark.asyncio
async def test_all_ocular_tools_callable_through_dispatch(
    server, projects_root: Path, video_path: Path, calib_path: Path
) -> None:
    stream_id = "test-ocular-stream"
    opened = await _call(
        server,
        "vision.open_stream",
        {
            "source": str(video_path),
            "source_type": "video_file",
            "stream_id": stream_id,
            "buffer_size": 3,
        },
    )
    assert opened["status"] == "open"
    assert opened["stream_id"] == stream_id

    state = await _call(server, "vision.get_stream_state", {"stream_id": stream_id})
    assert state["stream_id"] == stream_id

    calib = await _call(
        server,
        "vision.calibrate_sensor",
        {
            "image_paths": [str(calib_path)],
            "sensor_id": "test-sensor",
            "stream_id": stream_id,
        },
    )
    assert "calibration" in calib

    fix = await _call(
        server,
        "vision.fixate",
        {"region": [10.0, 10.0, 20.0, 20.0], "stream_id": stream_id},
    )
    assert "fixation" in fix

    sac = await _call(
        server,
        "vision.saccade",
        {
            "to_region": [40.0, 40.0, 20.0, 20.0],
            "stream_id": stream_id,
            "from_region": [10.0, 10.0, 20.0, 20.0],
        },
    )
    assert "saccade" in sac

    hist = [1.0 / 8.0] * 8
    tracked = await _call(
        server,
        "vision.track",
        {
            "stream_id": stream_id,
            "frame_index": 0,
            "detections": [
                {
                    "detection_id": "d0",
                    "kind": "object",
                    "bbox_xywh": [10.0, 30.0, 20.0, 20.0],
                    "centroid_xy": [20.0, 40.0],
                    "appearance_hist": hist,
                }
            ],
        },
    )
    assert tracked["n_tracks"] >= 1
    tracker_state = tracked["tracker_state"]

    reid = await _call(
        server,
        "vision.reidentify",
        {
            "stream_id": stream_id,
            "tracker_state": tracker_state,
            "detection": {
                "detection_id": "d1",
                "kind": "object",
                "bbox_xywh": [90.0, 70.0, 8.0, 8.0],
                "centroid_xy": [94.0, 74.0],
                "appearance_hist": [0.0] * 7 + [1.0],
            },
            "require_lost_or_occluded": False,
        },
    )
    assert "decision" in reid

    built = await _call(
        server,
        "vision.build_world_model",
        {
            "scene_id": "test-scene",
            "observations": [
                {
                    "frame_index": 0,
                    "track_source": "perception",
                    "lighting": {"mean_luminance": 0.4},
                    "entities": [
                        {
                            "entity_id": "e1",
                            "class_label": "blob",
                            "pose_m": [0.0, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                        }
                    ],
                }
            ],
        },
    )
    world = built["world"]
    assert built["n_entities"] >= 1

    updated = await _call(
        server,
        "vision.update_world_model",
        {
            "world": world,
            "observation": {
                "frame_index": 1,
                "track_source": "perception",
                "entities": [
                    {
                        "entity_id": "e1",
                        "class_label": "blob",
                        "pose_m": [0.05, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                    }
                ],
            },
        },
    )
    world = updated["world"]

    query = await _call(
        server,
        "vision.query_world",
        {"world": world, "query": {"type": "scene_summary"}},
    )
    assert query["result"]["n_entities"] >= 1

    explain = await _call(
        server,
        "vision.explain_belief",
        {"world": world, "entity_id": "e1", "slot": "pose"},
    )
    assert isinstance(explain["history"], list)

    unc = await _call(server, "vision.list_uncertainties", {"world": world})
    assert unc["count"] >= 1

    pred = await _call(
        server,
        "vision.predict_next",
        {"world": world, "horizon": 1, "store_on_world": True},
    )
    assert pred["count"] >= 1
    if pred.get("world"):
        world = pred["world"]

    surprises = await _call(server, "vision.list_surprises", {"world": world})
    assert "surprises" in surprises

    other = await _call(
        server,
        "vision.build_world_model",
        {
            "scene_id": "test-scene",
            "session_id": "s2",
            "observations": [
                {
                    "frame_index": 0,
                    "track_source": "perception",
                    "entities": [
                        {
                            "entity_id": "e1",
                            "class_label": "blob",
                            "pose_m": [0.5, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                        }
                    ],
                }
            ],
        },
    )
    change = await _call(
        server,
        "vision.observe_change",
        {"prior_world": world, "current_world": other["world"]},
    )
    assert "report" in change

    project = ProjectStore.create(projects_root / "ocular-mcp", "Ocular MCP")
    plan = await _call(
        server,
        "vision.plan_capture",
        {
            "project_path": str(project.root),
            "target_id": "t1",
            "target": {
                "bounds_min": [-0.1, -0.1, -0.1],
                "bounds_max": [0.1, 0.1, 0.1],
            },
            "budget": 2,
            "n_candidates": 6,
            "resolution": 4,
        },
    )
    assert "plan" in plan or "record" in plan

    target = {
        "target_id": "partial",
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
                "candidate_predictions": [0.0, 0.9],
            },
        ],
        "scale_authority": "UNRESOLVED",
        "has_scale_reference": False,
    }
    ask = await _call(
        server,
        "vision.ask_next_view",
        {"project_path": str(project.root), "target": target},
    )
    assert "stop_reason" in ask or "requests" in ask

    gain = await _call(
        server,
        "vision.measure_information_gain",
        {
            "target": target,
            "view": {
                "view_id": "u1",
                "kind": "underside",
                "regions": ["underside"],
            },
        },
    )
    assert "estimate" in gain

    closed = await _call(server, "vision.close_stream", {"stream_id": stream_id})
    assert closed.get("status") == "closed" or closed.get("state") == "closed"


def test_stream_timestamps_monotonic_and_drops_accounted(
    tmp_path: Path, video_path: Path
) -> None:
    handle = open_stream(
        video_path,
        source_type="video_file",
        stream_id="mono-test",
        buffer_size=2,  # small buffer forces drop accounting under push pressure
    )
    assert hasattr(handle, "stream_id")
    timestamps: list[float] = []
    dropped_before: list[int] = []
    for frame, _image in iter_frames(handle):
        timestamps.append(frame.timestamp)
        dropped_before.append(frame.dropped_before)
    state = close_stream(handle)
    assert len(timestamps) >= 2
    assert all(timestamps[i] > timestamps[i - 1] for i in range(1, len(timestamps)))
    drops = int(state["stats"]["frames_dropped"])
    assert drops >= 0
    # dropped_before is stamped before push; a drop on the final push can leave
    # stats one ahead of the last frame marker. Both must still be accounted.
    assert all(d >= 0 for d in dropped_before)
    peak = max(dropped_before) if dropped_before else 0
    assert abs(drops - peak) <= 1
    assert state["stats"]["frames_emitted"] == len(timestamps)


def test_webcam_attested_blocked_not_fabricated() -> None:
    """This host has no webcam guarantee; BLOCKED is correct, fabrications are not."""
    handle, attestation, status = open_stream_or_attest(
        "0",
        source_type="webcam",
        allow_webcam=True,
        webcam_index=0,
        stream_id="webcam-block-test",
    )
    if status["status"] == "open":
        # Real camera present: still not a fabrication; close cleanly.
        assert handle is not None
        assert handle.execution_class is ExecutionClass.PHYSICAL
        close_stream(handle)
        pytest.skip("webcam present on this host; BLOCKED path not exercised")
    assert status["status"] == "blocked"
    assert status["execution_class"] == ExecutionClass.BLOCKED.value
    assert attestation is not None
    assert attestation.execution_class is ExecutionClass.BLOCKED
    assert "fabricat" not in (attestation.blocked_reason or "").lower()
    # Opt-in false must also refuse without inventing frames.
    refused = open_stream(
        "0",
        source_type="webcam",
        allow_webcam=False,
        webcam_index=0,
    )
    assert not hasattr(refused, "stream_id") or getattr(refused, "execution_class", None) is (
        ExecutionClass.BLOCKED
    )
    from blender_vision.ocular.attestation import RuntimeAttestation

    assert isinstance(refused, RuntimeAttestation)
    assert refused.execution_class is ExecutionClass.BLOCKED


@pytest.mark.asyncio
async def test_mcp_build_world_model_rejects_ground_truth_observation(server) -> None:
    """MCP never exposes allow_ground_truth; ground-truth observations are rejected."""
    with pytest.raises(ToolError, match="ground-truth identity is forbidden"):
        await server.call_tool(
            "vision.build_world_model",
            {
                "scene_id": "mcp-gt-reject",
                "observations": [
                    {
                        "frame_index": 0,
                        "track_source": "ground_truth",
                        "entities": [
                            {
                                "entity_id": "cup",
                                "class_label": "cup",
                                "pose_m": [0.0, 0.0, 0.1, 1.0, 0.0, 0.0, 0.0],
                            }
                        ],
                    }
                ],
            },
        )
