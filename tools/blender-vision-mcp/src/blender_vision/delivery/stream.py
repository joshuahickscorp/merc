"""Streaming plan: poster → shell → first frame → detail → prefetch → terminal."""

from __future__ import annotations

from dataclasses import asdict, dataclass, field
from typing import Any

from blender_vision.v2.records import CameraPathGraph


@dataclass(slots=True)
class StreamStage:
    stage_id: str
    role: str
    order: int
    scroll_trigger: float
    chapter: str | None
    asset_ids: list[str] = field(default_factory=list)
    prefetch: bool = False
    description: str = ""

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)


@dataclass(slots=True)
class StreamingPlan:
    path_id: str
    stages: list[StreamStage]
    chapter_gates: dict[str, float]
    prefetch_triggers: list[dict[str, Any]]
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "path_id": self.path_id,
            "stages": [item.to_dict() for item in self.stages],
            "chapter_gates": dict(self.chapter_gates),
            "prefetch_triggers": list(self.prefetch_triggers),
            "notes": list(self.notes),
        }


def build_streaming_plan(
    graph: CameraPathGraph,
    *,
    asset_ids_by_role: dict[str, list[str]] | None = None,
) -> StreamingPlan:
    """Emit the runtime streaming manifest keyed to CameraPathGraph scroll beats."""
    roles = asset_ids_by_role or {}
    beats = sorted(graph.beats, key=lambda item: item.scroll_start)
    chapter_gates = {beat.label: float(beat.scroll_start) for beat in beats}

    # Prefetch second corridor / junction assets just before the TURN beat.
    turn = next((beat for beat in beats if beat.label == "TURN"), None)
    terminal = next((beat for beat in beats if beat.label in {"RECEIPT", "ACCESS"}), None)
    verify = next((beat for beat in beats if beat.label == "VERIFY"), None)

    junction_scroll = float(turn.scroll_start - 0.02) if turn else 0.5
    junction_scroll = max(0.0, junction_scroll)
    if verify is not None:
        terminal_scroll = float(verify.scroll_start)
    elif terminal is not None:
        terminal_scroll = float(terminal.scroll_start)
    else:
        terminal_scroll = 0.8

    stages = [
        StreamStage(
            stage_id="poster",
            role="poster",
            order=0,
            scroll_trigger=0.0,
            chapter=beats[0].label if beats else "THRESHOLD",
            asset_ids=list(roles.get("poster", ["poster"])),
            description="Poster image before any GLB download",
        ),
        StreamStage(
            stage_id="shell",
            role="shell",
            order=1,
            scroll_trigger=0.0,
            chapter=beats[0].label if beats else "THRESHOLD",
            asset_ids=list(roles.get("shell", ["shell"])),
            description="Shell GLB after poster; stable first frame base",
        ),
        StreamStage(
            stage_id="stable_first_frame",
            role="shell",
            order=2,
            scroll_trigger=0.0,
            chapter=beats[0].label if beats else "THRESHOLD",
            asset_ids=list(roles.get("shell", ["shell"])),
            description="Hold first frame until shell decode completes",
        ),
        StreamStage(
            stage_id="detail_enrichment",
            role="detail",
            order=3,
            scroll_trigger=chapter_gates.get("CAPACITY", 0.08),
            chapter="CAPACITY",
            asset_ids=list(roles.get("detail", ["detail"])),
            description="Detail LODs chapter-gated from CAPACITY onward",
        ),
        StreamStage(
            stage_id="junction_prefetch",
            role="network",
            order=4,
            scroll_trigger=junction_scroll,
            chapter="TURN",
            asset_ids=list(roles.get("network", ["junction", "second_corridor"])),
            prefetch=True,
            description="Prefetch second-corridor assets before the continuous left turn",
        ),
        StreamStage(
            stage_id="terminal_assets",
            role="terminal",
            order=5,
            scroll_trigger=terminal_scroll,
            chapter="VERIFY",
            asset_ids=list(roles.get("terminal", ["terminal"])),
            description="Verification terminal assets",
        ),
        StreamStage(
            stage_id="mobile_shell",
            role="mobile",
            order=6,
            scroll_trigger=0.0,
            chapter=beats[0].label if beats else "THRESHOLD",
            asset_ids=list(roles.get("mobile", ["mobile_shell"])),
            description="Mobile shell variant (budgeted separately)",
        ),
    ]

    prefetch_triggers = [
        {
            "trigger_id": "junction_second_corridor",
            "scroll": junction_scroll,
            "asset_ids": list(roles.get("network", ["junction", "second_corridor"])),
            "reason": "prefetch before TURN so the continuous turn never waits on network",
        },
        {
            "trigger_id": "terminal",
            "scroll": terminal_scroll,
            "asset_ids": list(roles.get("terminal", ["terminal"])),
            "reason": "terminal assets before VERIFY/RECEIPT",
        },
    ]

    return StreamingPlan(
        path_id=graph.id,
        stages=stages,
        chapter_gates=chapter_gates,
        prefetch_triggers=prefetch_triggers,
        notes=[
            "poster must complete before shell GLB is requested",
            "detail enrichment is chapter-gated; never full download at threshold",
            "native scroll only; no custom scroll driver",
        ],
    )
