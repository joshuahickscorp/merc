#!/usr/bin/env python3
"""Drive the stable VisionMCP tool surface against the live NOCTURNE/ONE app."""

from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import time
from datetime import UTC, datetime
from pathlib import Path
from typing import Any

from blender_vision.mcp.server import create_server
from blender_vision.perception.query import ObservationQueryService
from blender_vision.projects.store import ProjectStore


def canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":")).encode()


def sha256(value: Any) -> str:
    return hashlib.sha256(canonical(value)).hexdigest()


async def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--artifact-root", type=Path, required=True)
    parser.add_argument("--origin", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--attempt-id", default="pass-001")
    args = parser.parse_args()
    artifact_root = args.artifact_root.expanduser().resolve()
    output = artifact_root / "visionmcp"
    attempt_root = output / args.attempt_id
    calls_root = attempt_root / "calls"
    projects_root = output / "projects"
    calls_root.mkdir(parents=True, exist_ok=True)
    projects_root.mkdir(parents=True, exist_ok=True)
    server = create_server(projects_root)
    calls: list[dict[str, Any]] = []

    async def call(name: str, arguments: dict[str, Any]) -> dict[str, Any]:
        index = len(calls) + 1
        started = datetime.now(UTC)
        started_clock = time.monotonic()
        record: dict[str, Any] = {
            "sequence": index,
            "tool": name,
            "arguments": arguments,
            "started_at": started.isoformat().replace("+00:00", "Z"),
        }
        try:
            content, structured = await server.call_tool(name, arguments)
            record["status"] = "PASS"
            record["result"] = structured
            record["content_item_count"] = len(content)
        except Exception as error:
            record["status"] = "FAIL"
            record["error"] = {
                "type": type(error).__name__,
                "message": str(error),
            }
            structured = {}
        record["ended_at"] = datetime.now(UTC).isoformat().replace("+00:00", "Z")
        record["elapsed_seconds"] = round(time.monotonic() - started_clock, 6)
        record["record_sha256"] = sha256(record)
        calls.append(record)
        (calls_root / f"{index:03d}-{name.replace('.', '-')}.json").write_text(
            json.dumps(record, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        if record["status"] != "PASS":
            raise RuntimeError(f"{name} failed: {record['error']}")
        return structured

    created = await call(
        "project.create",
        {"name": f"{args.run_id}-{args.attempt_id}", "target_fidelity": "L3"},
    )
    project_path = str(
        (projects_root / str(created["project"]["slug"])).resolve()
    )
    declared_routes = ["/", "/technology", "/configurator", "/reserve", "/receipt"]
    declared_states = [
        "initial_loading",
        "poster_fallback",
        "3d_ready",
        "3d_unavailable",
        "reduced_motion",
        "keyboard_navigation",
        "touch_interaction",
        "slow_network",
        "offline_retry",
        "api_validation_error",
        "api_transient_error",
        "successful_reservation",
        "empty_configuration",
        "restored_saved_configuration",
    ]
    resolved_target = await call(
        "vision.resolve_target",
        {
            "project_path": project_path,
            "target": "NOCTURNE/ONE live application and editable 3D asset",
            "requested_tier": "L3",
            "request_class": "AUTONOMOUS_PUBLIC_EVIDENCE",
            "configuration": "fresh live acceptance at fixed localhost origin",
            "market": "synthetic-owned-benchmark",
            "structured_target": {
                "product": "NOCTURNE/ONE",
                "origin": args.origin,
                "routes": declared_routes,
                "declared_states": declared_states,
                "evidence_class": "SYNTHETIC_OWNED",
            },
        },
    )
    common = {
        "project_path": project_path,
        "rights_decision": "SYNTHETIC_OWNED_FIXED_NOCTURNE_CONTRACT",
        "allowed_origins": [args.origin],
        "allow_private_network": True,
        "browser_engine": "chromium",
        "browser_channel": "chrome",
        "color_scheme": "dark",
        "headless": True,
        "full_page": True,
        "timeout_ms": 45_000,
    }
    captures: dict[str, dict[str, Any]] = {}
    for route in declared_routes:
        label = "home" if route == "/" else route.removeprefix("/")
        captures[label] = await call(
            "vision.observe",
            {
                **common,
                "target_url": f"{args.origin}{route}",
                "viewport_width": 1440,
                "viewport_height": 900,
                "device_scale_factor": 2,
            },
        )
    captures["tablet"] = await call(
        "vision.observe",
        {
            **common,
            "target_url": f"{args.origin}/",
            "viewport_width": 768,
            "viewport_height": 1024,
            "device_scale_factor": 2,
            "has_touch": True,
        },
    )
    captures["mobile"] = await call(
        "vision.observe",
        {
            **common,
            "target_url": f"{args.origin}/",
            "viewport_width": 390,
            "viewport_height": 844,
            "device_scale_factor": 3,
            "is_mobile": True,
            "has_touch": True,
            "orientation": "portrait",
        },
    )
    captures["reduced_motion"] = await call(
        "vision.observe",
        {
            **common,
            "target_url": f"{args.origin}/",
            "viewport_width": 1440,
            "viewport_height": 900,
            "device_scale_factor": 2,
            "reduced_motion": "reduce",
        },
    )
    captures["slow_network"] = await call(
        "vision.observe",
        {
            **common,
            "target_url": f"{args.origin}/",
            "viewport_width": 1440,
            "viewport_height": 900,
            "device_scale_factor": 2,
            "network_profile": "slow-3g",
            "wait_until": "domcontentloaded",
        },
    )
    captures["comparison"] = await call(
        "vision.observe",
        {
            **common,
            "target_url": f"{args.origin}/",
            "viewport_width": 1440,
            "viewport_height": 900,
            "device_scale_factor": 2,
            "locale": "en-CA",
            "timezone_id": "America/Toronto",
        },
    )
    experience = await call(
        "vision.discover_states",
        {
            "project_path": project_path,
            "target_url": f"{args.origin}/",
            "rights_decision": "SYNTHETIC_OWNED_FIXED_NOCTURNE_CONTRACT",
            "allowed_origins": [args.origin],
            "viewport_width": 1440,
            "viewport_height": 900,
            "device_scale_factor": 2,
            "color_scheme": "dark",
            "responsive_viewports": [
                {"width": 390, "height": 844},
                {"width": 768, "height": 1024},
                {"width": 1440, "height": 900},
            ],
            "input_modes": ["pointer", "keyboard", "touch"],
            "action_limit": 12,
            "timeline_duration_ms": 1200,
            "timeline_step_ms": 100,
            "scroll_steps": 5,
            "allow_private_network": True,
            "browser_channel": "chrome",
            "headless": True,
        },
    )
    offline_experience = await call(
        "vision.discover_states",
        {
            "project_path": project_path,
            "target_url": f"{args.origin}/",
            "rights_decision": "SYNTHETIC_OWNED_FIXED_NOCTURNE_CONTRACT",
            "allowed_origins": [args.origin],
            "viewport_width": 390,
            "viewport_height": 844,
            "device_scale_factor": 3,
            "is_mobile": True,
            "has_touch": True,
            "orientation": "portrait",
            "color_scheme": "dark",
            "offline": True,
            "network_profile": "offline",
            "responsive_viewports": [
                {"width": 390, "height": 844},
                {"width": 768, "height": 1024},
            ],
            "input_modes": ["touch"],
            "action_limit": 8,
            "timeline_duration_ms": 300,
            "timeline_step_ms": 100,
            "scroll_steps": 2,
            "allow_private_network": True,
            "browser_channel": "chrome",
            "headless": True,
        },
    )
    graphics = await call(
        "vision.inspect_graphics",
        {
            "project_path": project_path,
            "target_url": f"{args.origin}/",
            "rights_decision": "SYNTHETIC_OWNED_FIXED_NOCTURNE_CONTRACT",
            "allowed_origins": [args.origin],
            "frame_timestamps_ms": [0, 500, 1200],
            "require_runtime_scene_hook": False,
            "materialize_gltf": False,
            "allow_private_network": True,
            "browser_channel": "chrome",
            "headless": True,
        },
    )

    home_id = captures["home"]["capture_id"]
    comparison_id = captures["comparison"]["capture_id"]
    query = await call(
        "vision.query",
        {
            "project_path": project_path,
            "capture_id": home_id,
            "query": {"selector": "#enter-3d"},
        },
    )
    match = query["matches"][0]
    bounds = match["bounds"]
    explanation = await call(
        "vision.explain_region",
        {
            "project_path": project_path,
            "capture_id": home_id,
            "x": bounds["x"] + bounds["width"] / 2,
            "y": bounds["y"] + bounds["height"] / 2,
            "graph_type": "LayoutGraph",
        },
    )
    project = ProjectStore.open(Path(project_path))
    state_graph = ObservationQueryService(project).graph(
        experience["capture_id"], "StateGraph"
    )
    observed_state_ids = [item["id"] for item in state_graph["nodes"]]
    state_capture = await call(
        "vision.capture_state",
        {
            "project_path": project_path,
            "capture_id": experience["capture_id"],
            "state_id": observed_state_ids[0],
        },
    )
    behavior = await call(
        "vision.trace_behavior",
        {
            "project_path": project_path,
            "capture_id": experience["capture_id"],
            "input_mode": "pointer",
        },
    )
    motion = await call(
        "vision.analyze_motion",
        {
            "project_path": project_path,
            "capture_id": experience["capture_id"],
        },
    )
    comparison = await call(
        "vision.compare",
        {
            "project_path": project_path,
            "capture_a": home_id,
            "capture_b": comparison_id,
            "selectors": ["#enter-3d", "header", "main", "footer"],
        },
    )
    portfolio = await call(
        "vision.repair",
        {
            "project_path": project_path,
            "action": "portfolio",
            "target_capture_id": home_id,
            "candidates": [
                {
                    "capture_id": comparison_id,
                    "parameters": {
                        "locale": "en-CA",
                        "timezone_id": "America/Toronto",
                    },
                }
            ],
            "selectors": ["#enter-3d", "header", "main", "footer"],
        },
    )
    evaluation = await call(
        "vision.evaluate",
        {
            "project_path": project_path,
            "portfolio_id": portfolio["id"],
        },
    )
    verifications = {}
    for label, capture in {
        "home": captures["home"],
        "mobile": captures["mobile"],
        "experience": experience,
        "offline_experience": offline_experience,
        "graphics": graphics,
    }.items():
        verifications[label] = await call(
            "vision.verify",
            {
                "project_path": project_path,
                "capture_id": capture["capture_id"],
            },
        )
    progress = await call(
        "vision.progress",
        {
            "project_path": project_path,
            "capture_ids": [
                capture["capture_id"] for capture in captures.values()
            ]
            + [experience["capture_id"], offline_experience["capture_id"], graphics["capture_id"]],
            "compute_budget": 8,
        },
    )
    review_queue = await call(
        "vision.review_queue",
        {"project_path": project_path},
    )

    route_coverage = {
        route: captures["home" if route == "/" else route.removeprefix("/")][
            "capture_id"
        ]
        for route in declared_routes
    }
    receipt = {
        "schema_version": "visionmcp.live_self_observation.v1",
        "run_id": args.run_id,
        "origin": args.origin,
        "completed_at": datetime.now(UTC).isoformat().replace("+00:00", "Z"),
        "project_path": project_path,
        "target": resolved_target,
        "route_coverage": route_coverage,
        "declared_states": declared_states,
        "vision_observed_state_ids": observed_state_ids,
        "vision_observed_state_count": len(observed_state_ids),
        "capture_ids": {
            **{label: value["capture_id"] for label, value in captures.items()},
            "experience": experience["capture_id"],
            "offline_experience": offline_experience["capture_id"],
            "graphics": graphics["capture_id"],
        },
        "summaries": {
            label: value["summary"] for label, value in captures.items()
        },
        "experience_summary": experience["summary"],
        "offline_experience_summary": offline_experience["summary"],
        "graphics_summary": graphics["summary"],
        "query": query,
        "explanation": explanation,
        "captured_state": state_capture,
        "behavior": behavior,
        "motion": motion,
        "comparison": comparison,
        "evaluation": evaluation,
        "verifications": verifications,
        "progress": progress,
        "review_queue": review_queue,
        "tool_call_count": len(calls),
        "tool_calls": [
            {
                "sequence": item["sequence"],
                "tool": item["tool"],
                "status": item["status"],
                "elapsed_seconds": item["elapsed_seconds"],
                "record_sha256": item["record_sha256"],
            }
            for item in calls
        ],
        "claim_boundary": [
            "VisionMCP state identities are perceptual DOM/render states, not aliases for the "
            "application's fourteen semantic state labels.",
            "The application semantic-state binding is completed by the fresh journey receipt.",
            "Graphics inspection begins before user activation and therefore does not alone "
            "prove the real GLB path; the live journey receipt provides that proof.",
        ],
    }
    receipt["receipt_sha256"] = sha256(receipt)
    receipt_path = artifact_root / "visionmcp-self-observation-receipt.json"
    receipt_path.write_text(
        json.dumps(receipt, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    (attempt_root / "calls-summary.json").write_text(
        json.dumps(calls, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    print(
        json.dumps(
            {
                "passed": all(item["status"] == "PASS" for item in calls),
                "tool_call_count": len(calls),
                "route_count": len(route_coverage),
                "vision_observed_state_count": len(observed_state_ids),
                "capture_count": len(receipt["capture_ids"]),
                "receipt": str(receipt_path),
                "receipt_sha256": receipt["receipt_sha256"],
            },
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(asyncio.run(main()))
