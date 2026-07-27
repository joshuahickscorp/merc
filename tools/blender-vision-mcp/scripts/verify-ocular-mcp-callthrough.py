#!/usr/bin/env python3
"""Prove the nineteen Ocular MCP tools execute over the real stdio wire.

A tool is not an MCP tool until the running server lists it AND executes it.
This script starts `.venv/bin/python -m blender_vision.mcp.server` as a
subprocess, performs the real MCP handshake, and calls every ocular tool with
schema-valid arguments in dependency order. Results are written to
artifacts/ocular/mcp-callthrough.json.

Honesty rules:
- Failures are recorded, never swallowed into a pass.
- Structured application errors (status == "error") are FAIL, not PASS.
- Hardware-only paths that cannot run on this host are BLOCKED with a reason.
- BLOCKED is distinct from PASS and FAIL and is not counted as success.
"""

from __future__ import annotations

import asyncio
import json
import os
import sys
import tempfile
import time
import traceback
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import cv2
import numpy as np
from mcp import ClientSession, StdioServerParameters
from mcp.client.stdio import stdio_client

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

# Dependency-respecting call order (close_stream last among stream tools).
CALL_ORDER: tuple[str, ...] = (
    "vision.open_stream",
    "vision.get_stream_state",
    "vision.calibrate_sensor",
    "vision.fixate",
    "vision.saccade",
    "vision.track",
    "vision.reidentify",
    "vision.build_world_model",
    "vision.update_world_model",
    "vision.query_world",
    "vision.explain_belief",
    "vision.list_uncertainties",
    "vision.predict_next",
    "vision.list_surprises",
    "vision.observe_change",
    "vision.plan_capture",
    "vision.ask_next_view",
    "vision.measure_information_gain",
    "vision.close_stream",
)

assert set(CALL_ORDER) == set(OCULAR_TOOLS)
assert len(CALL_ORDER) == 19

STREAM_DEPENDENTS = frozenset(
    {
        "vision.get_stream_state",
        "vision.calibrate_sensor",
        "vision.fixate",
        "vision.saccade",
        "vision.track",
        "vision.reidentify",
        "vision.close_stream",
    }
)
WORLD_DEPENDENTS = frozenset(
    {
        "vision.update_world_model",
        "vision.query_world",
        "vision.explain_belief",
        "vision.list_uncertainties",
        "vision.predict_next",
        "vision.list_surprises",
        "vision.observe_change",
    }
)


@dataclass
class ToolResult:
    tool: str
    status: str  # PASS | FAIL | BLOCKED
    latency_ms: float
    arguments: dict[str, Any] = field(default_factory=dict)
    error: str | None = None
    blocked_reason: str | None = None
    result_summary: dict[str, Any] | None = None
    schema_required: list[str] = field(default_factory=list)


def _package_root() -> Path:
    return Path(__file__).resolve().parents[1]


def _python_bin(root: Path) -> Path:
    candidates = [
        root / ".venv" / "bin" / "python",
        root / ".venv" / "Scripts" / "python.exe",
    ]
    for path in candidates:
        if path.exists():
            return path
    raise FileNotFoundError(f"no .venv python under {root}; expected .venv/bin/python")


def _write_fixture_video(path: Path, *, n_frames: int = 8, fps: float = 10.0) -> Path:
    """Encode a real multi-frame container OpenCV can decode (no ffmpeg required)."""
    path.parent.mkdir(parents=True, exist_ok=True)
    width, height = 160, 120
    fourcc = cv2.VideoWriter_fourcc(*"mp4v")
    writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
    if not writer.isOpened():
        path = path.with_suffix(".avi")
        fourcc = cv2.VideoWriter_fourcc(*"MJPG")
        writer = cv2.VideoWriter(str(path), fourcc, fps, (width, height))
    if not writer.isOpened():
        raise RuntimeError(f"could not open VideoWriter for {path}")
    for index in range(n_frames):
        frame = np.zeros((height, width, 3), dtype=np.uint8)
        # Moving coloured blob so track has real signal (not an empty detection list).
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
    for y in range(0, 120, 20):
        for x in range(0, 160, 20):
            if ((x // 20) + (y // 20)) % 2 == 0:
                image[y : y + 20, x : x + 20] = (200, 200, 200)
    if not cv2.imwrite(str(path), image):
        raise RuntimeError(f"failed to write calibration image {path}")
    return path


def _summarize_result(payload: Any, *, max_chars: int = 400) -> dict[str, Any]:
    """Compact, JSON-safe summary of a tool result for the artifact record."""
    if not isinstance(payload, dict):
        text = repr(payload)
        return {"type": type(payload).__name__, "repr": text[:max_chars]}
    summary: dict[str, Any] = {}
    for key in (
        "status",
        "state",
        "stream_id",
        "world_id",
        "n_entities",
        "n_tracks",
        "count",
        "execution_class",
        "stop_reason",
        "outcome",
        "inhibited",
        "redundant",
        "reason",
        "expected_reduction",
    ):
        if key in payload:
            summary[key] = payload[key]
    for key in (
        "tracks",
        "tracker_state",
        "decision",
        "world",
        "result",
        "history",
        "uncertainties",
        "predictions",
        "surprises",
        "report",
        "plan",
        "record",
        "requests",
        "estimate",
        "fixation",
        "saccade",
        "calibration",
        "attestation",
        "blocked_reason",
    ):
        if key in payload:
            value = payload[key]
            if isinstance(value, list):
                summary[f"{key}_len"] = len(value)
            elif isinstance(value, dict):
                summary[f"{key}_keys"] = sorted(value.keys())[:20]
            else:
                summary[key] = value
    return summary


def _payload_error_text(payload: dict[str, Any]) -> str:
    for key in ("error", "message", "detail", "blocked_reason", "reason"):
        if payload.get(key):
            return str(payload[key])
    return json.dumps(payload, default=str)[:800]


def _extract_payload(call_result: Any) -> tuple[dict[str, Any] | None, str | None, bool]:
    """Return (structured_payload, error_text, is_protocol_error)."""
    is_error = bool(getattr(call_result, "isError", False))
    structured = getattr(call_result, "structuredContent", None)
    if isinstance(structured, dict):
        return structured, None if not is_error else _payload_error_text(structured), is_error

    content = getattr(call_result, "content", None) or []
    texts: list[str] = []
    for item in content:
        text = getattr(item, "text", None)
        if text is None and isinstance(item, dict):
            text = item.get("text")
        if text:
            texts.append(str(text))
    joined = "\n".join(texts).strip()
    if joined:
        try:
            parsed = json.loads(joined)
        except json.JSONDecodeError:
            return None, joined if is_error else None, is_error
        if isinstance(parsed, dict):
            return parsed, None if not is_error else _payload_error_text(parsed), is_error
        return {"value": parsed}, None if not is_error else joined, is_error
    if is_error:
        return None, "tool returned isError with empty content", True
    return None, "empty tool result", True


def _assert_tool_contract(tool: str, payload: dict[str, Any]) -> None:
    """Require plausible evidence of real work, not an empty shell."""
    if tool == "vision.open_stream":
        if payload.get("status") != "open" and payload.get("state") != "open":
            raise AssertionError(f"open_stream did not open: {payload}")
        if not payload.get("stream_id"):
            raise AssertionError(f"open_stream missing stream_id: {payload}")
    elif tool == "vision.close_stream":
        if (
            payload.get("status") != "closed"
            and payload.get("state") != "closed"
            and "stream_id" not in payload
        ):
            raise AssertionError(f"close_stream unexpected: {payload}")
    elif tool == "vision.get_stream_state":
        if not payload.get("stream_id") and "state" not in payload:
            raise AssertionError(f"get_stream_state empty: {payload}")
    elif tool == "vision.calibrate_sensor":
        if "calibration" not in payload and "id" not in payload:
            raise AssertionError(f"calibrate_sensor missing calibration: {payload}")
    elif tool == "vision.fixate":
        if "fixation" not in payload:
            raise AssertionError(f"fixate missing fixation: {payload}")
    elif tool == "vision.saccade":
        if "saccade" not in payload:
            raise AssertionError(f"saccade missing plan: {payload}")
    elif tool == "vision.track":
        tracks = payload.get("tracks")
        if not isinstance(tracks, list) or len(tracks) < 1:
            raise AssertionError(f"track produced no tracks: {payload}")
        if "tracker_state" not in payload:
            raise AssertionError(f"track missing tracker_state: {payload}")
    elif tool == "vision.reidentify":
        if "decision" not in payload:
            raise AssertionError(f"reidentify missing decision: {payload}")
    elif tool == "vision.build_world_model":
        world = payload.get("world")
        if not isinstance(world, dict) or not world.get("entities"):
            raise AssertionError(f"build_world_model empty world: {payload}")
    elif tool == "vision.update_world_model":
        world = payload.get("world")
        if not isinstance(world, dict):
            raise AssertionError(f"update_world_model missing world: {payload}")
    elif tool == "vision.query_world":
        if "result" not in payload:
            raise AssertionError(f"query_world missing result: {payload}")
    elif tool == "vision.explain_belief":
        if "history" not in payload:
            raise AssertionError(f"explain_belief missing history: {payload}")
    elif tool == "vision.list_uncertainties":
        if "uncertainties" not in payload:
            raise AssertionError(f"list_uncertainties missing rows: {payload}")
    elif tool == "vision.predict_next":
        preds = payload.get("predictions")
        if not isinstance(preds, list) or len(preds) < 1:
            raise AssertionError(f"predict_next empty: {payload}")
    elif tool == "vision.list_surprises":
        if "surprises" not in payload:
            raise AssertionError(f"list_surprises missing: {payload}")
    elif tool == "vision.observe_change":
        if "report" not in payload:
            raise AssertionError(f"observe_change missing report: {payload}")
    elif tool == "vision.plan_capture":
        if "plan" not in payload and "record" not in payload:
            raise AssertionError(f"plan_capture unexpected: {payload}")
    elif tool == "vision.ask_next_view":
        if "requests" not in payload and "stop_reason" not in payload:
            raise AssertionError(f"ask_next_view unexpected: {payload}")
    elif tool == "vision.measure_information_gain":
        if "estimate" not in payload:
            raise AssertionError(f"measure_information_gain missing estimate: {payload}")


def _classify(
    *,
    tool: str,
    payload: dict[str, Any] | None,
    protocol_error: bool,
    error_text: str | None,
) -> tuple[str, str | None, str | None]:
    """Return (status, error, blocked_reason)."""
    if protocol_error:
        return "FAIL", error_text or "JSON-RPC / protocol error", None
    if payload is None:
        return "FAIL", error_text or "no payload", None

    status_field = str(payload.get("status") or "").lower()
    execution_class = str(payload.get("execution_class") or "").upper()

    if status_field == "blocked" or execution_class == "BLOCKED":
        reason = (
            payload.get("blocked_reason")
            or payload.get("reason")
            or payload.get("message")
            or error_text
            or "tool returned BLOCKED"
        )
        return "BLOCKED", None, str(reason)

    if status_field == "error" or payload.get("error"):
        return "FAIL", _payload_error_text(payload), None

    try:
        _assert_tool_contract(tool, payload)
    except AssertionError as exc:
        return "FAIL", str(exc), None

    return "PASS", None, None


def _safe_args(arguments: dict[str, Any]) -> dict[str, Any]:
    """JSON-serialize arguments; replace huge nested worlds with digests."""

    def scrub(value: Any, *, depth: int = 0) -> Any:
        if depth > 6:
            return "<max-depth>"
        if isinstance(value, dict):
            if "entities" in value and ("id" in value or "scene_id" in value):
                return {
                    "_kind": "world_ref",
                    "id": value.get("id"),
                    "scene_id": value.get("scene_id"),
                    "session_id": value.get("session_id"),
                    "n_entities": len(value.get("entities") or []),
                    "current_frame": value.get("current_frame"),
                }
            if "tracks" in value and "frame_index" in value:
                return {
                    "_kind": "tracker_state_ref",
                    "frame_index": value.get("frame_index"),
                    "n_tracks": len(value.get("tracks") or []),
                }
            return {str(k): scrub(v, depth=depth + 1) for k, v in value.items()}
        if isinstance(value, list):
            if len(value) > 40:
                return [scrub(v, depth=depth + 1) for v in value[:20]] + [
                    f"<…{len(value) - 20} more>"
                ]
            return [scrub(v, depth=depth + 1) for v in value]
        if isinstance(value, (str, int, float, bool)) or value is None:
            if isinstance(value, str) and len(value) > 500:
                return value[:500] + "…"
            return value
        return repr(value)

    return scrub(arguments)  # type: ignore[return-value]


def _args_for(tool: str, *, ctx: dict[str, Any]) -> dict[str, Any]:
    """Build a schema-valid, non-trivial argument payload for each tool."""
    stream_id = ctx["stream_id"]
    video_path = ctx["video_path"]
    calib_path = ctx["calib_path"]
    world = ctx.get("world")
    tracker_state = ctx.get("tracker_state")
    project_path = ctx.get("project_path")
    hist16 = [1.0 / 16.0] * 16

    if tool == "vision.open_stream":
        return {
            "source": str(video_path),
            "source_type": "video_file",
            "stream_id": stream_id,
            "buffer_size": 4,
            "frame_rate": 10.0,
        }
    if tool == "vision.get_stream_state":
        return {"stream_id": stream_id}
    if tool == "vision.close_stream":
        return {"stream_id": stream_id}
    if tool == "vision.calibrate_sensor":
        return {
            "image_paths": [str(calib_path)],
            "sensor_id": "callthrough-sensor",
            "stream_id": stream_id,
        }
    if tool == "vision.fixate":
        return {
            "region": [20.0, 30.0, 40.0, 40.0],
            "stream_id": stream_id,
            "reason": "salience",
            "expected_information": 0.6,
        }
    if tool == "vision.saccade":
        return {
            "to_region": [80.0, 50.0, 30.0, 30.0],
            "stream_id": stream_id,
            "reason": "motion",
            "from_region": [20.0, 30.0, 40.0, 40.0],
        }
    if tool == "vision.track":
        return {
            "stream_id": stream_id,
            "frame_index": 0,
            "detections": [
                {
                    "detection_id": "det-0",
                    "kind": "object",
                    "bbox_xywh": [20.0, 40.0, 24.0, 24.0],
                    "centroid_xy": [32.0, 52.0],
                    "appearance_hist": hist16,
                    "area_px": 576.0,
                }
            ],
        }
    if tool == "vision.reidentify":
        return {
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
        }
    if tool == "vision.build_world_model":
        return {
            "scene_id": "callthrough-scene",
            "session_id": "callthrough-session",
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
        }
    if tool == "vision.update_world_model":
        return {
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
        }
    if tool == "vision.query_world":
        return {"world": world, "query": {"type": "scene_summary"}}
    if tool == "vision.explain_belief":
        return {"world": world, "entity_id": "obj-a", "slot": "pose"}
    if tool == "vision.list_uncertainties":
        return {"world": world}
    if tool == "vision.predict_next":
        return {"world": world, "horizon": 1, "store_on_world": True}
    if tool == "vision.list_surprises":
        return {"world": world}
    if tool == "vision.observe_change":
        return {
            "prior_world": world,
            "current_world": ctx["moved_world"],
            "move_tol_m": 0.05,
        }
    if tool == "vision.plan_capture":
        return {
            "project_path": str(project_path),
            "target_id": "callthrough-target",
            "target": {
                "bounds_min": [-0.1, -0.1, -0.1],
                "bounds_max": [0.1, 0.1, 0.1],
            },
            "budget": 3,
            "n_candidates": 8,
            "resolution": 4,
        }
    if tool == "vision.ask_next_view":
        return {
            "project_path": str(project_path),
            "target": ctx["perception_target"],
        }
    if tool == "vision.measure_information_gain":
        return {
            "target": ctx["perception_target"],
            "view": {
                "view_id": "underside-1",
                "kind": "underside",
                "regions": ["underside"],
                "covers_scale_reference": False,
            },
        }
    raise KeyError(f"no argument builder for {tool}")


def _fail(
    tool: str,
    *,
    error: str,
    arguments: dict[str, Any] | None = None,
    latency_ms: float = 0.0,
    schema_required: list[str] | None = None,
) -> ToolResult:
    return ToolResult(
        tool=tool,
        status="FAIL",
        latency_ms=round(latency_ms, 3),
        arguments=_safe_args(arguments or {}),
        error=error,
        schema_required=list(schema_required or []),
    )


async def run_callthrough(
    *,
    artifact_path: Path | None = None,
    work_dir: Path | None = None,
    python_bin: Path | None = None,
) -> dict[str, Any]:
    """Run the full stdio callthrough once. Returns the artifact record.

    The pytest test imports this so the server is started once, not nineteen times.
    """
    root = _package_root()
    python = Path(python_bin) if python_bin else _python_bin(root)
    work = (
        Path(work_dir)
        if work_dir
        else Path(tempfile.mkdtemp(prefix="ocular-mcp-callthrough-"))
    )
    work.mkdir(parents=True, exist_ok=True)
    projects_root = work / "projects"
    projects_root.mkdir(exist_ok=True)
    video_path = _write_fixture_video(work / "stream.mp4")
    calib_path = _write_calib_image(work / "calib.png")

    out_path = (
        Path(artifact_path)
        if artifact_path
        else root / "artifacts" / "ocular" / "mcp-callthrough.json"
    )
    out_path.parent.mkdir(parents=True, exist_ok=True)

    env = os.environ.copy()
    # Prefer this checkout's sources over any other editable install path.
    src = str(root / "src")
    env["PYTHONPATH"] = src + os.pathsep + env.get("PYTHONPATH", "")
    env["BVMCP_PROJECTS_ROOT"] = str(projects_root)
    env.setdefault("BVMCP_DISABLE_BROWSER", "1")

    params = StdioServerParameters(
        command=str(python),
        args=["-m", "blender_vision.mcp.server"],
        env=env,
        cwd=str(root),
    )

    started_at = time.time()
    results: list[ToolResult] = []
    listed: list[str] = []
    schemas: dict[str, Any] = {}
    server_info: dict[str, Any] = {}

    # Drain server stderr (per-tool logging). An undrained PIPE fills and hangs.
    with open(os.devnull, "w", encoding="utf-8") as devnull:
        async with stdio_client(params, errlog=devnull) as (read, write):
            async with ClientSession(read, write) as session:
                # initialize + notifications/initialized (ClientSession handles both).
                init_result = await session.initialize()
                server_info = {
                    "name": getattr(getattr(init_result, "serverInfo", None), "name", None),
                    "version": getattr(
                        getattr(init_result, "serverInfo", None), "version", None
                    ),
                    "protocol_version": str(
                        getattr(init_result, "protocolVersion", "") or ""
                    ),
                }

                tools_response = await session.list_tools()
                tool_objs = list(tools_response.tools)
                listed = sorted(t.name for t in tool_objs)
                by_name = {t.name: t for t in tool_objs}
                for name in OCULAR_TOOLS:
                    tool = by_name.get(name)
                    if tool is None:
                        continue
                    schema = tool.inputSchema or {}
                    schemas[name] = {
                        "required": list(schema.get("required") or []),
                        "properties": sorted((schema.get("properties") or {}).keys()),
                    }

                for name in OCULAR_TOOLS:
                    if name not in by_name:
                        results.append(
                            _fail(
                                name,
                                error=(
                                    "tool not registered in tools/list "
                                    "(unregistered regression)"
                                ),
                            )
                        )

                ctx: dict[str, Any] = {
                    "stream_id": "callthrough-stream",
                    "video_path": video_path,
                    "calib_path": calib_path,
                    "world": None,
                    "moved_world": None,
                    "tracker_state": None,
                    "project_path": None,
                    "perception_target": {
                        "target_id": "callthrough-partial",
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
                    },
                }

                # Disposable project in the temp projects root — not the operator's.
                if "project.create" in by_name:
                    proj = await session.call_tool(
                        "project.create",
                        {"name": "ocular-callthrough-tmp"},
                    )
                    proj_payload, _, proj_proto = _extract_payload(proj)
                    if proj_proto or not isinstance(proj_payload, dict):
                        ctx["project_path"] = str(
                            projects_root / "ocular-callthrough-tmp"
                        )
                    else:
                        slug = (
                            (proj_payload.get("project") or {}).get("slug")
                            or "ocular-callthrough-tmp"
                        )
                        ctx["project_path"] = str(projects_root / slug)
                else:
                    fallback = projects_root / "ocular-callthrough-tmp"
                    fallback.mkdir(parents=True, exist_ok=True)
                    ctx["project_path"] = str(fallback)

                for tool_name in CALL_ORDER:
                    if tool_name not in by_name:
                        continue

                    open_ok = any(
                        r.tool == "vision.open_stream" and r.status == "PASS"
                        for r in results
                    )
                    world_ok = any(
                        r.tool == "vision.build_world_model" and r.status == "PASS"
                        for r in results
                    )

                    if tool_name == "vision.observe_change" and world_ok:
                        moved = await session.call_tool(
                            "vision.build_world_model",
                            {
                                "scene_id": "callthrough-scene",
                                "session_id": "callthrough-session-2",
                                "observations": [
                                    {
                                        "frame_index": 0,
                                        "track_source": "perception",
                                        "lighting": {"mean_luminance": 0.46},
                                        "entities": [
                                            {
                                                "entity_id": "obj-a",
                                                "class_label": "blob",
                                                "pose_m": [
                                                    0.4,
                                                    0.0,
                                                    0.5,
                                                    1.0,
                                                    0.0,
                                                    0.0,
                                                    0.0,
                                                ],
                                                "visible": True,
                                            }
                                        ],
                                    }
                                ],
                            },
                        )
                        moved_payload, moved_err, moved_proto = _extract_payload(moved)
                        if (
                            moved_proto
                            or not isinstance(moved_payload, dict)
                            or "world" not in moved_payload
                        ):
                            results.append(
                                _fail(
                                    tool_name,
                                    error=(
                                        "could not build second world for "
                                        f"observe_change: {moved_err or moved_payload}"
                                    ),
                                )
                            )
                            continue
                        ctx["moved_world"] = moved_payload["world"]

                    if tool_name in STREAM_DEPENDENTS and not open_ok:
                        results.append(
                            _fail(
                                tool_name,
                                error="dependency: vision.open_stream did not PASS",
                            )
                        )
                        continue
                    if tool_name in WORLD_DEPENDENTS and not world_ok:
                        results.append(
                            _fail(
                                tool_name,
                                error=(
                                    "dependency: vision.build_world_model did not PASS"
                                ),
                            )
                        )
                        continue

                    try:
                        arguments = _args_for(tool_name, ctx=ctx)
                    except Exception as exc:  # noqa: BLE001
                        results.append(
                            _fail(
                                tool_name,
                                error=(
                                    f"could not build arguments: "
                                    f"{type(exc).__name__}: {exc}"
                                ),
                            )
                        )
                        continue

                    schema_required = list(
                        (schemas.get(tool_name) or {}).get("required") or []
                    )
                    missing_req = [k for k in schema_required if k not in arguments]
                    if missing_req:
                        results.append(
                            _fail(
                                tool_name,
                                error=(
                                    "arguments missing schema required fields: "
                                    f"{missing_req}"
                                ),
                                arguments=arguments,
                                schema_required=schema_required,
                            )
                        )
                        continue

                    started = time.perf_counter()
                    try:
                        raw = await session.call_tool(tool_name, arguments)
                    except Exception as exc:  # noqa: BLE001 — surface as FAIL
                        latency = (time.perf_counter() - started) * 1000.0
                        results.append(
                            _fail(
                                tool_name,
                                error=(
                                    f"{type(exc).__name__}: {exc}\n"
                                    f"{traceback.format_exc()}"
                                ),
                                arguments=arguments,
                                latency_ms=latency,
                                schema_required=schema_required,
                            )
                        )
                        continue
                    latency = (time.perf_counter() - started) * 1000.0
                    payload, err_text, protocol_error = _extract_payload(raw)
                    status, error, blocked = _classify(
                        tool=tool_name,
                        payload=payload,
                        protocol_error=protocol_error,
                        error_text=err_text,
                    )
                    results.append(
                        ToolResult(
                            tool=tool_name,
                            status=status,
                            latency_ms=round(latency, 3),
                            arguments=_safe_args(arguments),
                            error=error,
                            blocked_reason=blocked,
                            result_summary=(
                                _summarize_result(payload)
                                if payload is not None
                                else None
                            ),
                            schema_required=schema_required,
                        )
                    )

                    if status == "PASS" and isinstance(payload, dict):
                        if tool_name == "vision.track":
                            ctx["tracker_state"] = payload.get("tracker_state")
                        if tool_name in {
                            "vision.build_world_model",
                            "vision.update_world_model",
                        }:
                            ctx["world"] = payload.get("world")
                        if tool_name == "vision.predict_next" and payload.get("world"):
                            ctx["world"] = payload["world"]

    by_tool = {r.tool: r for r in results}
    ordered = [by_tool[name] for name in CALL_ORDER if name in by_tool]
    succeeded = sum(1 for r in ordered if r.status == "PASS")
    failed = sum(1 for r in ordered if r.status == "FAIL")
    blocked_n = sum(1 for r in ordered if r.status == "BLOCKED")
    called = len(ordered)

    record: dict[str, Any] = {
        "schema_version": 1,
        "title": "ocular-mcp-callthrough",
        "generated_at_unix": started_at,
        "duration_s": round(time.time() - started_at, 3),
        "transport": "stdio",
        "server_command": [str(python), "-m", "blender_vision.mcp.server"],
        "server_info": server_info,
        "work_dir": str(work),
        "projects_root": str(projects_root),
        "tools_listed_total": len(listed),
        "ocular_listed": sum(1 for n in OCULAR_TOOLS if n in set(listed)),
        "ocular_tools": list(OCULAR_TOOLS),
        "schemas": schemas,
        "summary": {
            "called": called,
            "succeeded": succeeded,
            "failed": failed,
            "blocked": blocked_n,
        },
        "results": [asdict(r) for r in ordered],
    }

    out_path.write_text(json.dumps(record, indent=2, default=str) + "\n", encoding="utf-8")
    record["artifact_path"] = str(out_path)
    return record


def main() -> int:
    try:
        record = asyncio.run(run_callthrough())
    except Exception as exc:  # noqa: BLE001 — top-level hard failure
        print(f"CALLTHROUGH_HARD_FAIL: {type(exc).__name__}: {exc}", file=sys.stderr)
        traceback.print_exc()
        return 2

    summary = record["summary"]
    print(
        f"ocular_callthrough called={summary['called']} "
        f"succeeded={summary['succeeded']} "
        f"failed={summary['failed']} "
        f"blocked={summary['blocked']}"
    )
    print(f"artifact={record.get('artifact_path')}")
    print(f"tools_listed_total={record.get('tools_listed_total')}")
    print(f"ocular_listed={record.get('ocular_listed')}/19")
    print()
    print(f"{'tool':<36} {'status':<8} {'ms':>10}  detail")
    print("-" * 90)
    for row in record["results"]:
        detail = row.get("error") or row.get("blocked_reason") or ""
        if len(detail) > 60:
            detail = detail[:57] + "..."
        print(
            f"{row['tool']:<36} {row['status']:<8} {row['latency_ms']:>10.1f}  {detail}"
        )

    if summary["failed"] > 0:
        print("\nFAIL: one or more ocular tools failed over stdio")
        return 1
    if summary["called"] != 19:
        print(f"\nFAIL: expected 19 called, got {summary['called']}")
        return 1
    if summary["succeeded"] + summary["blocked"] != 19:
        print("\nFAIL: not every tool is PASS or BLOCKED")
        return 1
    print("\nPASS: all nineteen ocular tools executed (PASS or BLOCKED)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
