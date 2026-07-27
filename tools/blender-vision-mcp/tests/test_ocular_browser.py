"""Phase N — browser eyeball and contradiction detectors."""

from __future__ import annotations

from pathlib import Path

import numpy as np

from blender_vision.ocular.browser_eyeball import (
    ContradictionKind,
    build_eyeball_from_snapshot,
    demonstrate_detectors,
    run_browser_eyeball,
    run_contradiction_detectors,
    synthetic_contradiction_fixture,
)
from blender_vision.perception.browser import BrowserAdapter


def test_all_contradiction_kinds_fire() -> None:
    demo = demonstrate_detectors()
    assert demo["all_kinds_fired"] is True
    required = {k.value for k in ContradictionKind}
    assert required.issubset(set(demo["kinds_fired"]))


def test_focus_order_and_scroll_and_loading() -> None:
    snap = synthetic_contradiction_fixture()
    pixels = snap.pop("pixels")
    eyeball = build_eyeball_from_snapshot(snap, pixels=pixels)
    kinds = {c.kind for c in eyeball.contradictions}
    assert ContradictionKind.FOCUS_ORDER_MISMATCH in kinds
    assert ContradictionKind.CANVAS_SCROLL_TRAP in kinds
    assert ContradictionKind.LOADING_STATE_STALL in kinds
    assert ContradictionKind.SOURCE_RENDERED_MISMATCH in kinds
    assert ContradictionKind.BROWSER_BLENDER_MISMATCH in kinds


def test_unified_interface_fields() -> None:
    snap = synthetic_contradiction_fixture()
    pixels = snap.pop("pixels")
    eyeball = build_eyeball_from_snapshot(snap, pixels=pixels)
    payload = eyeball.to_dict()
    for key in (
        "dom_nodes",
        "accessibility",
        "computed_styles",
        "animations",
        "webgl",
        "network",
        "pixels_digest",
        "contradictions",
    ):
        assert key in payload


def test_existing_browser_adapter_preserved() -> None:
    # Do not break the VisionMCP browser tool surface.
    assert hasattr(BrowserAdapter, "capture")
    assert hasattr(BrowserAdapter, "normalize_target")


def test_run_browser_eyeball_receipt(tmp_path: Path) -> None:
    receipt = run_browser_eyeball(tmp_path)
    assert receipt["status"] == "PASS"
    assert (tmp_path / "browser_eyeball.receipt.json").is_file()
    assert (tmp_path / "contradiction_demo.json").is_file()


def test_detectors_idempotent_on_clean_page() -> None:
    # Minimal clean snapshot: no contradictions expected for most classes.
    pixels = np.zeros((32, 32, 3), dtype=np.uint8)
    pixels[:] = (40, 40, 40)
    pixels[5:15, 5:15] = (200, 50, 50)
    snap = {
        "page_url": "about:blank",
        "viewport": [32, 32],
        "loading_ready_state": "complete",
        "dom_nodes": [
            {
                "node_id": "n0",
                "tag": "div",
                "role": "main",
                "name": "Content",
                "bounds_css": [5, 5, 10, 10],
                "visible_style": True,
                "opacity": 1.0,
                "display": "block",
                "visibility": "visible",
                "selector": "#main",
            }
        ],
        "accessibility": [
            {
                "role": "main",
                "name": "Content",
                "focusable": False,
                "focused": False,
                "order": 0,
                "bounds_css": [5, 5, 10, 10],
            }
        ],
        "webgl": [],
        "network": [],
        "source_state": {"expected_visible_text": "Content"},
        "scroll": {
            "y": 0,
            "scroll_height": 32,
            "client_height": 32,
            "wheel_targeted_canvas": False,
            "attempted_delta_y": 0,
        },
        "computed_styles": [],
        "animations": [],
    }
    eyeball = build_eyeball_from_snapshot(snap, pixels=pixels)
    run_contradiction_detectors(eyeball, pixels)
    # Clean page should not fire scroll trap / focus / blender mismatch.
    kinds = {c.kind for c in eyeball.contradictions}
    assert ContradictionKind.CANVAS_SCROLL_TRAP not in kinds
    assert ContradictionKind.BROWSER_BLENDER_MISMATCH not in kinds
