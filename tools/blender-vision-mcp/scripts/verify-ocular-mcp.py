#!/usr/bin/env python3
"""Prove the Ocular MCP surface is real: list tools and call each through the server.

A Python-callable function is not an MCP tool until the running server lists it
and executes it. This script starts the actual FastMCP server object, enumerates
the tool list, asserts all nineteen ocular names are present, and dispatches
each one with valid arguments. Exit non-zero on any missing tool or error.
"""

from __future__ import annotations

import asyncio
import sys
import tempfile
from pathlib import Path

import cv2
import numpy as np

# Package is installed editable via the project venv.
from blender_vision.mcp.server import create_server

OCULAR_TOOLS: tuple[str, ...] = (
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

# Baseline V1 vision tools that must remain registered (contract: surface intact).
V1_VISION_TOOLS: tuple[str, ...] = (
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


def _write_fixture_video(path: Path, *, n_frames: int = 8, fps: float = 10.0) -> Path:
    """Encode a real multi-frame container OpenCV can decode (no ffmpeg required)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    width, height = 160, 120
    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
    if not writer.isOpened():
        # Fall back to AVI/MJPG if mp4v is unavailable on this host.
        path = path.with_suffix(".avi")
        fourcc = cv2.VideoWriter_fourcc(*"MJPG")
        writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
    if not writer.isOpened():
        raise RuntimeError(f"could not open VideoWriter for {path}")
    for index in range(n_frames):
        frame = np.zeros((height, width, 3), dtype=np.uint8)
        # Moving coloured blob so segment/track have signal.
        cx = 20 + index * 12
        cy = 40 + (index % 3) * 8
        cv2.rectangle(frame, (cx, cy), (cx + 24, cy + 24), (40, 180, 255), -1)
        cv2.putText(
            frame,
            f"f{index}",
            (8, 16),
            cv2.FONT_HERSHEY_SIMPLEX,
            0.4,
            (220, 220, 220),
            1,
            cv2.LINE_AA,
        )
        writer.write(frame)
    writer.release()
    return path


def _write_calib_image(path: Path) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    image = np.full((120, 160, 3), 40, dtype=np.uint8)
    # Soft checker pattern (no corners required for image_centre_fallback).
    for y in range(0, 120, 20):
        for x in range(0, 160, 20):
            if ((x // 20) + (y // 20)) % 2 == 0:
                image[y : y + 20, x : x + 20] = (200, 200, 200)
    cv2.imwrite(str(path), image)
    return path


async def _call(server, name: str, arguments: dict) -> dict:
    _content, structured = await server.call_tool(name, arguments)
    if not isinstance(structured, dict):
        raise AssertionError(f"{name} returned non-dict: {type(structured)!r} {structured!r}")
    if structured.get("status") == "error":
        raise AssertionError(f"{name} returned error status: {structured}")
    return structured


async def main() -> int:
    root = Path(tempfile.mkdtemp(prefix="ocular-mcp-verify-"))
    video_path = _write_fixture_video(root / "stream.mp4")
    calib_path = _write_calib_image(root / "calib.png")
    projects_root = root / "projects"
    projects_root.mkdir()

    server = create_server(projects_root)
    tools = await server.list_tools()
    by_name = {tool.name: tool for tool in tools}
    total = len(by_name)
    print(f"server_tool_count={total}")

    missing = [name for name in OCULAR_TOOLS if name not in by_name]
    if missing:
        print(f"MISSING ocular tools ({len(missing)}): {missing}")
        return 1
    print("ocular_listed=19/19")

    missing_v1 = [name for name in V1_VISION_TOOLS if name not in by_name]
    if missing_v1:
        print(f"MISSING V1 vision tools: {missing_v1}")
        return 1
    print(f"v1_vision_intact={len(V1_VISION_TOOLS)}")

    # Baseline total before this change was 269 (34 V1 vision + 25 V2 + rest).
    # After adding 17 new ocular tools (plan_capture / ask_next_view already
    # existed) the floor is 269 + 17 = 286.
    if total < 286:
        print(f"tool surface regressed: total={total} expected>=286")
        return 1
    print(f"total_tool_floor_ok total={total} >= 286")

    statuses: list[tuple[str, str]] = []
    stream_id = "verify-ocular-stream"
    world: dict | None = None
    tracker_state: dict | None = None

    try:
        opened = await _call(
            server,
            "vision.open_stream",
            {
                "source": str(video_path),
                "source_type": "video_file",
                "stream_id": stream_id,
                "buffer_size": 4,
                "frame_rate": 10.0,
            },
        )
        if opened.get("status") != "open":
            raise AssertionError(f"open_stream failed: {opened}")
        statuses.append(("vision.open_stream", "ok"))

        state = await _call(server, "vision.get_stream_state", {"stream_id": stream_id})
        if state.get("stream_id") != stream_id:
            raise AssertionError(f"get_stream_state mismatch: {state}")
        statuses.append(("vision.get_stream_state", "ok"))

        calib = await _call(
            server,
            "vision.calibrate_sensor",
            {
                "image_paths": [str(calib_path)],
                "sensor_id": "verify-sensor",
                "stream_id": stream_id,
            },
        )
        if "calibration" not in calib:
            raise AssertionError(f"calibrate_sensor missing calibration: {calib}")
        statuses.append(("vision.calibrate_sensor", "ok"))

        fix = await _call(
            server,
            "vision.fixate",
            {
                "region": [20.0, 30.0, 40.0, 40.0],
                "stream_id": stream_id,
                "reason": "salience",
                "expected_information": 0.6,
            },
        )
        if "fixation" not in fix:
            raise AssertionError(f"fixate missing fixation: {fix}")
        statuses.append(("vision.fixate", "ok"))

        sac = await _call(
            server,
            "vision.saccade",
            {
                "to_region": [80.0, 50.0, 30.0, 30.0],
                "stream_id": stream_id,
                "reason": "motion",
                "from_region": [20.0, 30.0, 40.0, 40.0],
            },
        )
        if "saccade" not in sac:
            raise AssertionError(f"saccade missing plan: {sac}")
        statuses.append(("vision.saccade", "ok"))

        hist = [1.0 / 16.0] * 16
        track = await _call(
            server,
            "vision.track",
            {
                "stream_id": stream_id,
                "frame_index": 0,
                "detections": [
                    {
                        "detection_id": "det-0",
                        "kind": "object",
                        "bbox_xywh": [20.0, 40.0, 24.0, 24.0],
                        "centroid_xy": [32.0, 52.0],
                        "appearance_hist": hist,
                        "area_px": 576.0,
                    }
                ],
            },
        )
        tracker_state = track.get("tracker_state")
        if not track.get("tracks"):
            raise AssertionError(f"track produced no tracks: {track}")
        statuses.append(("vision.track", "ok"))

        reid = await _call(
            server,
            "vision.reidentify",
            {
                "stream_id": stream_id,
                "tracker_state": tracker_state,
                "detection": {
                    "detection_id": "det-1",
                    "kind": "object",
                    "bbox_xywh": [100.0, 80.0, 10.0, 10.0],
                    "centroid_xy": [105.0, 85.0],
                    "appearance_hist": [0.0] * 15 + [1.0],
                },
                "require_lost_or_occluded": False,
            },
        )
        if "decision" not in reid:
            raise AssertionError(f"reidentify missing decision: {reid}")
        statuses.append(("vision.reidentify", "ok"))

        built = await _call(
            server,
            "vision.build_world_model",
            {
                "scene_id": "verify-scene",
                "session_id": "verify-session",
                "observations": [
                    {
                        "frame_index": 0,
                        "track_source": "perception",
                        "lighting": {"mean_luminance": 0.45},
                        "entities": [
                            {
                                "entity_id": "obj-a",
                                "track_id": "obj-a",
                                "class_label": "blob",
                                "pose_m": [0.1, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                                "visible": True,
                                "appearance": {"mean_bgr": [40, 180, 255]},
                            }
                        ],
                    }
                ],
            },
        )
        world = built["world"]
        if not world.get("entities"):
            raise AssertionError(f"build_world_model empty: {built}")
        statuses.append(("vision.build_world_model", "ok"))

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
                            "entity_id": "obj-a",
                            "class_label": "blob",
                            "pose_m": [0.12, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                            "visible": True,
                        }
                    ],
                },
            },
        )
        world = updated["world"]
        statuses.append(("vision.update_world_model", "ok"))

        query = await _call(
            server,
            "vision.query_world",
            {"world": world, "query": {"type": "scene_summary"}},
        )
        if "result" not in query:
            raise AssertionError(f"query_world missing result: {query}")
        statuses.append(("vision.query_world", "ok"))

        explain = await _call(
            server,
            "vision.explain_belief",
            {"world": world, "entity_id": "obj-a", "slot": "pose"},
        )
        if "history" not in explain:
            raise AssertionError(f"explain_belief missing history: {explain}")
        statuses.append(("vision.explain_belief", "ok"))

        unc = await _call(server, "vision.list_uncertainties", {"world": world})
        if "uncertainties" not in unc:
            raise AssertionError(f"list_uncertainties missing rows: {unc}")
        statuses.append(("vision.list_uncertainties", "ok"))

        pred = await _call(
            server,
            "vision.predict_next",
            {"world": world, "horizon": 1, "store_on_world": True},
        )
        if not pred.get("predictions"):
            raise AssertionError(f"predict_next empty: {pred}")
        if pred.get("world"):
            world = pred["world"]
        statuses.append(("vision.predict_next", "ok"))

        surprises = await _call(server, "vision.list_surprises", {"world": world})
        if "surprises" not in surprises:
            raise AssertionError(f"list_surprises missing: {surprises}")
        statuses.append(("vision.list_surprises", "ok"))

        # Second world for change observation (object moved).
        moved = await _call(
            server,
            "vision.build_world_model",
            {
                "scene_id": "verify-scene",
                "session_id": "verify-session-2",
                "observations": [
                    {
                        "frame_index": 0,
                        "track_source": "perception",
                        "lighting": {"mean_luminance": 0.46},
                        "entities": [
                            {
                                "entity_id": "obj-a",
                                "class_label": "blob",
                                "pose_m": [0.4, 0.0, 0.5, 1.0, 0.0, 0.0, 0.0],
                                "visible": True,
                            }
                        ],
                    }
                ],
            },
        )
        change = await _call(
            server,
            "vision.observe_change",
            {
                "prior_world": world,
                "current_world": moved["world"],
                "move_tol_m": 0.05,
            },
        )
        if "report" not in change:
            raise AssertionError(f"observe_change missing report: {change}")
        statuses.append(("vision.observe_change", "ok"))

        # Existing V2 tools that are part of the ocular nineteen.
        from blender_vision.projects.store import ProjectStore

        store = ProjectStore.create(projects_root / "ocular-verify", "Ocular Verify")
        project_path = str(store.root)

        target = {
            "target_id": "verify-target",
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
            ],
            "scale_authority": "UNRESOLVED",
            "has_scale_reference": False,
            "has_diffuse_light_view": False,
            "has_grazing_light_view": False,
            "has_lens_metadata": False,
            "has_calibration_target": False,
        }

        plan = await _call(
            server,
            "vision.plan_capture",
            {
                "project_path": str(project_path),
                "target_id": "verify-target",
                "target": {
                    "bounds_min": [-0.1, -0.1, -0.1],
                    "bounds_max": [0.1, 0.1, 0.1],
                },
                "budget": 3,
                "n_candidates": 8,
                "resolution": 4,
            },
        )
        if "plan" not in plan and "record" not in plan:
            raise AssertionError(f"plan_capture unexpected: {plan}")
        statuses.append(("vision.plan_capture", "ok"))

        ask = await _call(
            server,
            "vision.ask_next_view",
            {"project_path": str(project_path), "target": target},
        )
        if "requests" not in ask and "stop_reason" not in ask:
            raise AssertionError(f"ask_next_view unexpected: {ask}")
        statuses.append(("vision.ask_next_view", "ok"))

        gain = await _call(
            server,
            "vision.measure_information_gain",
            {
                "target": target,
                "view": {
                    "view_id": "underside-1",
                    "kind": "underside",
                    "regions": ["underside"],
                    "covers_scale_reference": False,
                },
            },
        )
        if "estimate" not in gain:
            raise AssertionError(f"measure_information_gain missing estimate: {gain}")
        statuses.append(("vision.measure_information_gain", "ok"))

        closed = await _call(server, "vision.close_stream", {"stream_id": stream_id})
        # close_stream returns status closed merged with snapshot.
        if (
            closed.get("status") != "closed"
            and closed.get("state") != "closed"
            and "stream_id" not in closed
        ):
            raise AssertionError(f"close_stream unexpected: {closed}")
        statuses.append(("vision.close_stream", "ok"))

    except Exception as exc:  # noqa: BLE001 — report and fail the run
        print(f"EXECUTION_ERROR: {type(exc).__name__}: {exc}")
        for name, status in statuses:
            print(f"  {name}: {status}")
        remaining = [n for n in OCULAR_TOOLS if n not in {s[0] for s in statuses}]
        for name in remaining:
            print(f"  {name}: not_run")
        return 1

    executed = {name for name, _ in statuses}
    missing_exec = [name for name in OCULAR_TOOLS if name not in executed]
    if missing_exec:
        print(f"not executed: {missing_exec}")
        return 1

    print("ocular_executed=19/19")
    for name, status in statuses:
        print(f"  {name}: {status}")
    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
