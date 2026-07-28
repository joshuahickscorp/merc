"""Phase N — browser and screen eyeball.

Unify pixels, DOM, accessibility tree, computed style, animation state, WebGL
and network into one interface, and implement contradiction detectors:

* visible in DOM but not in pixels
* pixels without semantics
* canvas scroll trap
* focus-order mismatch
* loading-state stall
* source state differing from rendered state
* browser scene differing from the Blender export

Preserves every existing VisionMCP browser tool (``perception.browser``). One
browser at a time via ``scripts/with-one-browser.sh``.
"""

from __future__ import annotations

import hashlib
import json
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import Any

import numpy as np
from numpy.typing import NDArray

from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.ocular.attestation import (
    ExecutionClass,
    RuntimeAttestation,
    attest_blocked,
    attest_substitute,
)
from blender_vision.v2.authority import AuthorityClass

ArrayU8 = NDArray[np.uint8]


class ContradictionKind(StrEnum):
    DOM_VISIBLE_PIXELS_MISSING = "dom_visible_pixels_missing"
    PIXELS_WITHOUT_SEMANTICS = "pixels_without_semantics"
    CANVAS_SCROLL_TRAP = "canvas_scroll_trap"
    FOCUS_ORDER_MISMATCH = "focus_order_mismatch"
    LOADING_STATE_STALL = "loading_state_stall"
    SOURCE_RENDERED_MISMATCH = "source_rendered_mismatch"
    BROWSER_BLENDER_MISMATCH = "browser_blender_mismatch"


@dataclass(slots=True)
class DomNode:
    node_id: str
    tag: str
    role: str | None
    name: str
    bounds_css: tuple[float, float, float, float]  # x, y, w, h
    visible_style: bool
    opacity: float
    display: str
    visibility: str
    z_index: float
    selector: str = ""
    attributes: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {
            "node_id": self.node_id,
            "tag": self.tag,
            "role": self.role,
            "name": self.name,
            "bounds_css": list(self.bounds_css),
            "visible_style": self.visible_style,
            "opacity": self.opacity,
            "display": self.display,
            "visibility": self.visibility,
            "z_index": self.z_index,
            "selector": self.selector,
            "attributes": dict(self.attributes),
        }


@dataclass(slots=True)
class AccessibilityNode:
    role: str
    name: str
    focusable: bool
    focused: bool
    order: int
    bounds_css: tuple[float, float, float, float] | None = None

    def to_dict(self) -> dict[str, Any]:
        return {
            "role": self.role,
            "name": self.name,
            "focusable": self.focusable,
            "focused": self.focused,
            "order": self.order,
            "bounds_css": list(self.bounds_css) if self.bounds_css else None,
        }


@dataclass(slots=True)
class ComputedStyleSnapshot:
    selector: str
    properties: dict[str, str] = field(default_factory=dict)

    def to_dict(self) -> dict[str, Any]:
        return {"selector": self.selector, "properties": dict(self.properties)}


@dataclass(slots=True)
class AnimationState:
    selector: str
    name: str
    play_state: str
    current_time_ms: float
    duration_ms: float

    def to_dict(self) -> dict[str, Any]:
        return {
            "selector": self.selector,
            "name": self.name,
            "play_state": self.play_state,
            "current_time_ms": self.current_time_ms,
            "duration_ms": self.duration_ms,
        }


@dataclass(slots=True)
class WebGLSurface:
    canvas_selector: str
    context: str  # webgl | webgl2 | webgpu | canvas2d | none
    width: int
    height: int
    drawing_buffer_empty: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "canvas_selector": self.canvas_selector,
            "context": self.context,
            "width": self.width,
            "height": self.height,
            "drawing_buffer_empty": self.drawing_buffer_empty,
        }


@dataclass(slots=True)
class NetworkEntry:
    url: str
    method: str
    status: int
    resource_type: str
    failed: bool = False

    def to_dict(self) -> dict[str, Any]:
        return {
            "url": self.url,
            "method": self.method,
            "status": self.status,
            "resource_type": self.resource_type,
            "failed": self.failed,
        }


@dataclass(slots=True)
class Contradiction:
    kind: ContradictionKind
    confidence: float
    summary: str
    evidence: dict[str, Any] = field(default_factory=dict)
    next_action: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "kind": self.kind.value,
            "confidence": self.confidence,
            "summary": self.summary,
            "evidence": dict(self.evidence),
            "next_action": self.next_action,
        }


@dataclass(slots=True)
class BrowserEyeball:
    """Unified multi-surface browser observation."""

    page_url: str
    viewport: list[int]
    pixels_digest: str = ""
    pixels_shape: list[int] = field(default_factory=list)
    dom_nodes: list[DomNode] = field(default_factory=list)
    accessibility: list[AccessibilityNode] = field(default_factory=list)
    computed_styles: list[ComputedStyleSnapshot] = field(default_factory=list)
    animations: list[AnimationState] = field(default_factory=list)
    webgl: list[WebGLSurface] = field(default_factory=list)
    network: list[NetworkEntry] = field(default_factory=list)
    source_state: dict[str, Any] = field(default_factory=dict)
    blender_export: dict[str, Any] | None = None
    loading_ready_state: str = "complete"
    scroll: dict[str, float] = field(default_factory=dict)
    contradictions: list[Contradiction] = field(default_factory=list)
    authority: str = AuthorityClass.SENSOR_DERIVED.value
    execution_class: str = ExecutionClass.DIAGNOSTIC_ONLY.value
    notes: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "page_url": self.page_url,
            "viewport": list(self.viewport),
            "pixels_digest": self.pixels_digest,
            "pixels_shape": list(self.pixels_shape),
            "dom_nodes": [n.to_dict() for n in self.dom_nodes],
            "accessibility": [a.to_dict() for a in self.accessibility],
            "computed_styles": [s.to_dict() for s in self.computed_styles],
            "animations": [a.to_dict() for a in self.animations],
            "webgl": [w.to_dict() for w in self.webgl],
            "network": [n.to_dict() for n in self.network],
            "source_state": dict(self.source_state),
            "blender_export": self.blender_export,
            "loading_ready_state": self.loading_ready_state,
            "scroll": dict(self.scroll),
            "contradictions": [c.to_dict() for c in self.contradictions],
            "authority": self.authority,
            "execution_class": self.execution_class,
            "notes": list(self.notes),
        }


def _digest_pixels(pixels: ArrayU8 | None) -> str:
    if pixels is None or pixels.size == 0:
        return ""
    return hashlib.sha256(np.ascontiguousarray(pixels).tobytes()).hexdigest()


def _region_mean_luma(pixels: ArrayU8, bounds: tuple[float, float, float, float]) -> float:
    """Mean luminance in a CSS-pixel bbox mapped 1:1 to the screenshot array."""
    h, w = pixels.shape[:2]
    x, y, bw, bh = bounds
    x0 = max(0, int(x))
    y0 = max(0, int(y))
    x1 = min(w, int(x + max(bw, 1)))
    y1 = min(h, int(y + max(bh, 1)))
    if x1 <= x0 or y1 <= y0:
        return 0.0
    crop = pixels[y0:y1, x0:x1]
    if crop.ndim == 3:
        # PNG screenshots are often RGB; accept either.
        r = crop[..., 0].astype(np.float64)
        g = crop[..., 1].astype(np.float64)
        b = crop[..., 2].astype(np.float64)
        return float(np.mean(0.299 * r + 0.587 * g + 0.114 * b))
    return float(np.mean(crop.astype(np.float64)))


def detect_dom_visible_pixels_missing(
    eyeball: BrowserEyeball, pixels: ArrayU8 | None, *, bg_luma: float = 245.0
) -> list[Contradiction]:
    """DOM says visible, but the pixel region is near-background."""
    if pixels is None:
        return []
    out: list[Contradiction] = []
    for node in eyeball.dom_nodes:
        if not node.visible_style or node.opacity < 0.05 or node.display == "none":
            continue
        if node.visibility == "hidden":
            continue
        x, y, w, h = node.bounds_css
        if w < 4 or h < 4:
            continue
        luma = _region_mean_luma(pixels, node.bounds_css)
        # Near-white (or matching declared bg) with no structure → missing paint.
        empty_paint = abs(luma - bg_luma) < 8.0 or luma < 2.0
        claims_content = bool(node.name) or node.role in {
            "button",
            "img",
            "link",
            "heading",
        }
        if empty_paint and claims_content:
            out.append(
                Contradiction(
                    kind=ContradictionKind.DOM_VISIBLE_PIXELS_MISSING,
                    confidence=0.72,
                    summary=(
                        f"DOM node {node.selector or node.node_id} is style-visible "
                        f"but pixel region is empty/near-bg (luma={luma:.1f})"
                    ),
                    evidence={
                        "node": node.to_dict(),
                        "region_luma": luma,
                        "bg_luma": bg_luma,
                    },
                    next_action="re-capture after fonts/images settle; check opacity stacking",
                )
            )
    return out


def detect_pixels_without_semantics(
    eyeball: BrowserEyeball, pixels: ArrayU8 | None
) -> list[Contradiction]:
    """Non-trivial painted region with no overlapping DOM/a11y node."""
    if pixels is None or pixels.size == 0:
        return []
    h, w = pixels.shape[:2]
    # Downsample residual: high-variance tiles without DOM coverage.
    tile = 32
    covered = np.zeros((h, w), dtype=np.uint8)
    for node in eyeball.dom_nodes:
        x, y, bw, bh = node.bounds_css
        x0, y0 = max(0, int(x)), max(0, int(y))
        x1, y1 = min(w, int(x + max(bw, 1))), min(h, int(y + max(bh, 1)))
        if x1 > x0 and y1 > y0:
            covered[y0:y1, x0:x1] = 1
    gray = (
        (0.299 * pixels[..., 0] + 0.587 * pixels[..., 1] + 0.114 * pixels[..., 2]).astype(
            np.float64
        )
        if pixels.ndim == 3
        else pixels.astype(np.float64)
    )
    hits: list[dict[str, Any]] = []
    # Inclusive end so the last full tile is examined (h=128, tile=32 → 0..96).
    for ty in range(0, max(1, h - tile + 1), tile):
        for tx in range(0, max(1, w - tile + 1), tile):
            patch = gray[ty : ty + tile, tx : tx + tile]
            cov = covered[ty : ty + tile, tx : tx + tile]
            if float(np.std(patch)) > 18.0 and float(np.mean(cov)) < 0.15:
                hits.append({"x": tx, "y": ty, "std": float(np.std(patch))})
    if len(hits) >= 3:
        return [
            Contradiction(
                kind=ContradictionKind.PIXELS_WITHOUT_SEMANTICS,
                confidence=min(0.9, 0.5 + 0.05 * len(hits)),
                summary=f"{len(hits)} high-variance tiles lack DOM/a11y coverage",
                evidence={"tiles": hits[:12], "tile_px": tile},
                next_action="inspect canvas/WebGL/SVG layers; attach accessibility names",
            )
        ]
    return []


def detect_canvas_scroll_trap(eyeball: BrowserEyeball) -> list[Contradiction]:
    """Canvas that steals wheel without document scroll progress."""
    out: list[Contradiction] = []
    scroll_y = float(eyeball.scroll.get("y", 0.0))
    scroll_height = float(eyeball.scroll.get("scroll_height", 0.0))
    client_height = float(eyeball.scroll.get("client_height", 0.0))
    wheel_on_canvas = bool(eyeball.scroll.get("wheel_targeted_canvas", False))
    attempted_delta = float(eyeball.scroll.get("attempted_delta_y", 0.0))
    for surface in eyeball.webgl:
        is_canvas = surface.context in {"webgl", "webgl2", "webgpu", "canvas2d"}
        trapped = (
            is_canvas
            and wheel_on_canvas
            and attempted_delta != 0
            and abs(scroll_y) < 1.0
            and scroll_height > client_height + 8
        )
        if trapped:
            out.append(
                Contradiction(
                    kind=ContradictionKind.CANVAS_SCROLL_TRAP,
                    confidence=0.85,
                    summary=(
                        f"Canvas {surface.canvas_selector} consumed wheel events "
                        "while document scroll did not advance"
                    ),
                    evidence={
                        "surface": surface.to_dict(),
                        "scroll": dict(eyeball.scroll),
                    },
                    next_action="allow wheel passthrough or provide explicit scroll UI",
                )
            )
    return out


def detect_focus_order_mismatch(eyeball: BrowserEyeball) -> list[Contradiction]:
    """Tab order disagrees with visual top-to-bottom / left-to-right order."""
    focusable = [a for a in eyeball.accessibility if a.focusable and a.bounds_css]
    if len(focusable) < 2:
        return []
    visual = sorted(
        focusable,
        key=lambda a: (
            int((a.bounds_css or (0, 0, 0, 0))[1] // 20),
            (a.bounds_css or (0, 0, 0, 0))[0],
        ),
    )
    by_tab = sorted(focusable, key=lambda a: a.order)
    visual_ids = [(a.role, a.name) for a in visual]
    tab_ids = [(a.role, a.name) for a in by_tab]
    if visual_ids != tab_ids:
        return [
            Contradiction(
                kind=ContradictionKind.FOCUS_ORDER_MISMATCH,
                confidence=0.8,
                summary="Accessibility focus order mismatches visual reading order",
                evidence={"visual_order": visual_ids, "tab_order": tab_ids},
                next_action="adjust tabindex / DOM order to match layout",
            )
        ]
    return []


def detect_loading_state_stall(eyeball: BrowserEyeball) -> list[Contradiction]:
    """readyState stuck or loading indicator still present after settle budget."""
    out: list[Contradiction] = []
    if eyeball.loading_ready_state not in {"complete", "interactive"}:
        out.append(
            Contradiction(
                kind=ContradictionKind.LOADING_STATE_STALL,
                confidence=0.9,
                summary=f"document.readyState stuck at {eyeball.loading_ready_state!r}",
                evidence={"ready_state": eyeball.loading_ready_state},
                next_action="wait longer or diagnose blocked network request",
            )
        )
    # Source flags a loading spinner that still claims busy.
    if eyeball.source_state.get("loading") is True and eyeball.loading_ready_state == "complete":
        out.append(
            Contradiction(
                kind=ContradictionKind.LOADING_STATE_STALL,
                confidence=0.75,
                summary="Source loading flag still true after document complete",
                evidence={"source_state": dict(eyeball.source_state)},
                next_action="clear app loading gate or fix hung promise",
            )
        )
    failed = [n for n in eyeball.network if n.failed or (n.status >= 400)]
    if failed and eyeball.source_state.get("loading") is True:
        out.append(
            Contradiction(
                kind=ContradictionKind.LOADING_STATE_STALL,
                confidence=0.88,
                summary="Loading stall coincides with failed network entries",
                evidence={"failed": [n.to_dict() for n in failed[:8]]},
                next_action="retry or surface the network failure to the user",
            )
        )
    return out


def detect_source_rendered_mismatch(eyeball: BrowserEyeball) -> list[Contradiction]:
    """App source state (IR / store) disagrees with rendered DOM text/attrs."""
    source = eyeball.source_state
    if not source:
        return []
    expected_text = source.get("expected_visible_text")
    if not expected_text:
        return []
    names = " ".join(n.name for n in eyeball.dom_nodes if n.name)
    if str(expected_text) not in names:
        return [
            Contradiction(
                kind=ContradictionKind.SOURCE_RENDERED_MISMATCH,
                confidence=0.78,
                summary="Source expected_visible_text not present in rendered DOM names",
                evidence={
                    "expected_visible_text": expected_text,
                    "dom_names_sample": names[:500],
                },
                next_action="diff source state vs renderer binding",
            )
        ]
    return []


def detect_browser_blender_mismatch(eyeball: BrowserEyeball) -> list[Contradiction]:
    """Browser scene fingerprint differs from Blender export fingerprint."""
    export = eyeball.blender_export
    if not export:
        return []
    browser_fp = export.get("browser_scene_fingerprint") or eyeball.pixels_digest
    blender_fp = export.get("blender_export_fingerprint")
    if not blender_fp:
        return []
    if browser_fp != blender_fp:
        return [
            Contradiction(
                kind=ContradictionKind.BROWSER_BLENDER_MISMATCH,
                confidence=0.9,
                summary="Browser scene fingerprint differs from Blender export",
                evidence={
                    "browser_scene_fingerprint": browser_fp,
                    "blender_export_fingerprint": blender_fp,
                    "mesh_count_browser": export.get("mesh_count_browser"),
                    "mesh_count_blender": export.get("mesh_count_blender"),
                },
                next_action="re-export GLB and re-capture browser round-trip",
            )
        ]
    return []


def run_contradiction_detectors(
    eyeball: BrowserEyeball, pixels: ArrayU8 | None = None
) -> list[Contradiction]:
    """Run the full detector set and attach results to the eyeball."""
    found: list[Contradiction] = []
    found.extend(detect_dom_visible_pixels_missing(eyeball, pixels))
    found.extend(detect_pixels_without_semantics(eyeball, pixels))
    found.extend(detect_canvas_scroll_trap(eyeball))
    found.extend(detect_focus_order_mismatch(eyeball))
    found.extend(detect_loading_state_stall(eyeball))
    found.extend(detect_source_rendered_mismatch(eyeball))
    found.extend(detect_browser_blender_mismatch(eyeball))
    eyeball.contradictions = found
    return found


def build_eyeball_from_snapshot(
    snapshot: dict[str, Any],
    *,
    pixels: ArrayU8 | None = None,
) -> BrowserEyeball:
    """Construct a BrowserEyeball from a JSON snapshot (builder/fixture path)."""
    dom_nodes = [
        DomNode(
            node_id=str(n.get("node_id", f"n-{i}")),
            tag=str(n.get("tag", "div")),
            role=n.get("role"),
            name=str(n.get("name", "")),
            bounds_css=tuple(n.get("bounds_css", [0, 0, 0, 0]))[:4],  # type: ignore[arg-type]
            visible_style=bool(n.get("visible_style", True)),
            opacity=float(n.get("opacity", 1.0)),
            display=str(n.get("display", "block")),
            visibility=str(n.get("visibility", "visible")),
            z_index=float(n.get("z_index", 0)),
            selector=str(n.get("selector", "")),
            attributes={str(k): str(v) for k, v in (n.get("attributes") or {}).items()},
        )
        for i, n in enumerate(snapshot.get("dom_nodes") or [])
    ]
    # Normalise bounds to 4-tuple of floats.
    fixed_dom: list[DomNode] = []
    for node in dom_nodes:
        b = node.bounds_css
        if len(b) < 4:
            b = (0.0, 0.0, 0.0, 0.0)
        else:
            b = (float(b[0]), float(b[1]), float(b[2]), float(b[3]))
        fixed_dom.append(
            DomNode(
                node_id=node.node_id,
                tag=node.tag,
                role=node.role,
                name=node.name,
                bounds_css=b,
                visible_style=node.visible_style,
                opacity=node.opacity,
                display=node.display,
                visibility=node.visibility,
                z_index=node.z_index,
                selector=node.selector,
                attributes=node.attributes,
            )
        )

    a11y: list[AccessibilityNode] = []
    for i, a in enumerate(snapshot.get("accessibility") or []):
        bounds = a.get("bounds_css")
        bt = None
        if bounds and len(bounds) >= 4:
            bt = (float(bounds[0]), float(bounds[1]), float(bounds[2]), float(bounds[3]))
        a11y.append(
            AccessibilityNode(
                role=str(a.get("role", "generic")),
                name=str(a.get("name", "")),
                focusable=bool(a.get("focusable", False)),
                focused=bool(a.get("focused", False)),
                order=int(a.get("order", i)),
                bounds_css=bt,
            )
        )

    styles = [
        ComputedStyleSnapshot(
            selector=str(s.get("selector", "")),
            properties={str(k): str(v) for k, v in (s.get("properties") or {}).items()},
        )
        for s in snapshot.get("computed_styles") or []
    ]
    animations = [
        AnimationState(
            selector=str(a.get("selector", "")),
            name=str(a.get("name", "")),
            play_state=str(a.get("play_state", "running")),
            current_time_ms=float(a.get("current_time_ms", 0.0)),
            duration_ms=float(a.get("duration_ms", 0.0)),
        )
        for a in snapshot.get("animations") or []
    ]
    webgl = [
        WebGLSurface(
            canvas_selector=str(w.get("canvas_selector", "canvas")),
            context=str(w.get("context", "none")),
            width=int(w.get("width", 0)),
            height=int(w.get("height", 0)),
            drawing_buffer_empty=bool(w.get("drawing_buffer_empty", False)),
        )
        for w in snapshot.get("webgl") or []
    ]
    network = [
        NetworkEntry(
            url=str(n.get("url", "")),
            method=str(n.get("method", "GET")),
            status=int(n.get("status", 0)),
            resource_type=str(n.get("resource_type", "other")),
            failed=bool(n.get("failed", False)),
        )
        for n in snapshot.get("network") or []
    ]

    if pixels is None and snapshot.get("pixels_png_b64"):
        # Avoid hard dependency on decoding unless present; leave pixels None.
        pixels = None

    eyeball = BrowserEyeball(
        page_url=str(snapshot.get("page_url", "about:blank")),
        viewport=list(snapshot.get("viewport") or [1280, 720]),
        pixels_digest=(
            _digest_pixels(pixels)
            if pixels is not None
            else str(snapshot.get("pixels_digest", ""))
        ),
        pixels_shape=(
            list(pixels.shape)
            if pixels is not None
            else list(snapshot.get("pixels_shape") or [])
        ),
        dom_nodes=fixed_dom,
        accessibility=a11y,
        computed_styles=styles,
        animations=animations,
        webgl=webgl,
        network=network,
        source_state=dict(snapshot.get("source_state") or {}),
        blender_export=snapshot.get("blender_export"),
        loading_ready_state=str(snapshot.get("loading_ready_state", "complete")),
        scroll=dict(snapshot.get("scroll") or {}),
        execution_class=str(
            snapshot.get("execution_class") or ExecutionClass.DIAGNOSTIC_ONLY.value
        ),
        notes=list(snapshot.get("notes") or []),
    )
    run_contradiction_detectors(eyeball, pixels)
    return eyeball


def synthetic_contradiction_fixture() -> dict[str, Any]:
    """Self-contained snapshot that fires every contradiction class."""
    # 128x128 near-white image with several high-variance tiles outside DOM coverage.
    rng = np.random.default_rng(0)
    pixels = np.full((128, 128, 3), 250, dtype=np.uint8)
    for y0, x0 in ((64, 64), (64, 96), (96, 64), (96, 96), (32, 96)):
        pixels[y0 : y0 + 28, x0 : x0 + 28] = rng.integers(0, 255, size=(28, 28, 3), dtype=np.uint8)
    return {
        "page_url": "file://synthetic-contradiction-fixture",
        "viewport": [128, 128],
        "pixels": pixels,
        "dom_nodes": [
            {
                "node_id": "btn-1",
                "tag": "button",
                "role": "button",
                "name": "Submit",
                "bounds_css": [2, 2, 20, 12],
                "visible_style": True,
                "opacity": 1.0,
                "display": "block",
                "visibility": "visible",
                "selector": "#submit",
            },
            {
                "node_id": "ghost",
                "tag": "div",
                "role": "button",
                "name": "Ghost CTA",
                "bounds_css": [4, 20, 24, 12],
                "visible_style": True,
                "opacity": 1.0,
                "display": "block",
                "visibility": "visible",
                "selector": "#ghost",
            },
        ],
        "accessibility": [
            {
                "role": "button",
                "name": "Second",
                "focusable": True,
                "focused": False,
                "order": 0,
                "bounds_css": [30, 40, 10, 10],
            },
            {
                "role": "button",
                "name": "First",
                "focusable": True,
                "focused": False,
                "order": 1,
                "bounds_css": [2, 2, 10, 10],
            },
        ],
        "webgl": [
            {
                "canvas_selector": "#gl",
                "context": "webgl2",
                "width": 128,
                "height": 128,
                "drawing_buffer_empty": False,
            }
        ],
        "scroll": {
            "y": 0.0,
            "scroll_height": 400.0,
            "client_height": 128.0,
            "wheel_targeted_canvas": True,
            "attempted_delta_y": 120.0,
        },
        "loading_ready_state": "loading",
        "source_state": {
            "loading": True,
            "expected_visible_text": "WelcomeHero",
        },
        "network": [
            {
                "url": "https://example.invalid/app.js",
                "method": "GET",
                "status": 0,
                "resource_type": "script",
                "failed": True,
            }
        ],
        "blender_export": {
            "browser_scene_fingerprint": "aaa",
            "blender_export_fingerprint": "bbb",
            "mesh_count_browser": 2,
            "mesh_count_blender": 3,
        },
        "computed_styles": [
            {"selector": "#submit", "properties": {"color": "rgb(0,0,0)"}}
        ],
        "animations": [
            {
                "selector": ".spinner",
                "name": "spin",
                "play_state": "running",
                "current_time_ms": 9999,
                "duration_ms": 1000,
            }
        ],
    }


def demonstrate_detectors() -> dict[str, Any]:
    """Run detectors on the synthetic fixture; return a demonstration receipt."""
    snap = synthetic_contradiction_fixture()
    pixels = snap.pop("pixels")
    eyeball = build_eyeball_from_snapshot(snap, pixels=pixels)
    kinds = sorted({c.kind.value for c in eyeball.contradictions})
    return {
        "schema": "ocular.browser-eyeball-demo/1",
        "completed_at": utc_now(),
        "contradiction_count": len(eyeball.contradictions),
        "kinds_fired": kinds,
        "all_kinds_required": sorted(k.value for k in ContradictionKind),
        "all_kinds_fired": all(
            k.value in kinds for k in ContradictionKind
        ),
        "eyeball": eyeball.to_dict(),
    }


def capture_page_physical(
    url: str,
    *,
    output: Path,
    channel: str = "chrome",
    viewport: tuple[int, int] = (1280, 720),
) -> tuple[BrowserEyeball | None, RuntimeAttestation]:
    """Attempt a physical Playwright capture. Never fabricates success.

    Callers should serialize with ``scripts/with-one-browser.sh``.
    """
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        att = attest_blocked("playwright", f"playwright not importable: {exc}")
        return None, att

    screenshot_path = output / "browser_screenshot.png"
    snapshot_path = output / "browser_snapshot.json"
    try:
        with sync_playwright() as p:
            try:
                browser = p.chromium.launch(channel=channel, headless=True)
            except Exception as exc:  # noqa: BLE001
                att = attest_blocked(
                    "chrome",
                    f"chromium.launch(channel={channel!r}) failed: {exc}",
                )
                return None, att
            try:
                context = browser.new_context(
                    viewport={"width": viewport[0], "height": viewport[1]}
                )
                page = context.new_page()
                page.goto(url, wait_until="domcontentloaded", timeout=30_000)
                page.screenshot(path=str(screenshot_path), full_page=False)
                # Long JS is intentional; keep lines short for ruff E501.
                payload = page.evaluate(
                    """() => {
                      const els = [...document.querySelectorAll('body *')];
                      const nodes = els.slice(0, 200).map((el, i) => {
                        const r = el.getBoundingClientRect();
                        const s = getComputedStyle(el);
                        const label = el.innerText
                          || el.getAttribute('aria-label') || '';
                        return {
                          node_id: 'n-'+i,
                          tag: el.tagName.toLowerCase(),
                          role: el.getAttribute('role'),
                          name: label.slice(0, 120),
                          bounds_css: [r.x, r.y, r.width, r.height],
                          visible_style:
                            s.display !== 'none' && s.visibility !== 'hidden',
                          opacity: parseFloat(s.opacity || '1'),
                          display: s.display,
                          visibility: s.visibility,
                          z_index: parseFloat(s.zIndex) || 0,
                          selector: el.id
                            ? ('#'+el.id) : el.tagName.toLowerCase(),
                        };
                      });
                      const a11y = nodes.filter(
                        n => n.role || n.tag === 'button' || n.tag === 'a'
                      ).slice(0, 40).map((n, i) => ({
                        role: n.role || n.tag,
                        name: n.name,
                        focusable: true,
                        focused: false,
                        order: i,
                        bounds_css: n.bounds_css,
                      }));
                      const webgl = [...document.querySelectorAll('canvas')]
                        .map((c, i) => {
                          let ctx = 'none';
                          if (c.getContext('webgl2')) ctx = 'webgl2';
                          else if (c.getContext('webgl')) ctx = 'webgl';
                          else if (c.getContext('2d')) ctx = 'canvas2d';
                          return {
                            canvas_selector: c.id
                              ? ('#'+c.id)
                              : ('canvas:nth-of-type('+(i+1)+')'),
                            context: ctx,
                            width: c.width,
                            height: c.height,
                            drawing_buffer_empty: false,
                          };
                        });
                      return {
                        page_url: location.href,
                        viewport: [window.innerWidth, window.innerHeight],
                        loading_ready_state: document.readyState,
                        scroll: {
                          y: window.scrollY,
                          scroll_height:
                            document.documentElement.scrollHeight,
                          client_height:
                            document.documentElement.clientHeight,
                          wheel_targeted_canvas: false,
                          attempted_delta_y: 0,
                        },
                        dom_nodes: nodes,
                        accessibility: a11y,
                        webgl: webgl,
                        network: [],
                        source_state: {},
                        computed_styles: [],
                        animations: [],
                      };
                    }"""
                )
                atomic_write_json(snapshot_path, payload)
            finally:
                browser.close()
    except Exception as exc:  # noqa: BLE001
        att = attest_substitute(
            "browser-capture",
            execution_class=ExecutionClass.DIAGNOSTIC_ONLY,
            reason=f"capture raised: {exc}",
            substitute="none",
        )
        return None, att

    import cv2

    pixels = cv2.imread(str(screenshot_path), cv2.IMREAD_COLOR)
    if pixels is not None:
        pixels = cv2.cvtColor(pixels, cv2.COLOR_BGR2RGB)
    eyeball = build_eyeball_from_snapshot(payload, pixels=pixels)
    eyeball.execution_class = ExecutionClass.PHYSICAL.value
    eyeball.authority = AuthorityClass.RUNTIME_OBSERVED.value
    physical = RuntimeAttestation(
        id="attest-chrome-physical",
        runtime="chrome",
        execution_class=ExecutionClass.PHYSICAL,
        executable=channel,
        command=["playwright", "channel=chrome", url],
        returncode=0,
        started_at=utc_now(),
        ended_at=utc_now(),
        host={"channel": channel},
        output_digests={
            "screenshot": (
                hashlib.sha256(screenshot_path.read_bytes()).hexdigest()
                if screenshot_path.is_file()
                else ""
            ),
        },
        authority=AuthorityClass.RUNTIME_OBSERVED,
    ).seal()
    atomic_write_json(output / "browser_eyeball.json", eyeball.to_dict())
    return eyeball, physical


def run_browser_eyeball(
    output: Path,
    *,
    url: str | None = None,
    snapshot_path: Path | None = None,
    physical: bool = False,
) -> dict[str, Any]:
    """Phase N runner core: demo detectors + optional physical capture."""
    output = output.expanduser().resolve()
    output.mkdir(parents=True, exist_ok=True)
    demo = demonstrate_detectors()
    atomic_write_json(output / "contradiction_demo.json", demo)

    receipt: dict[str, Any] = {
        "schema": "ocular.browser-eyeball-receipt/1",
        "completed_at": utc_now(),
        "demo": {
            "contradiction_count": demo["contradiction_count"],
            "kinds_fired": demo["kinds_fired"],
            "all_kinds_fired": demo["all_kinds_fired"],
        },
        "physical": None,
        "snapshot": None,
        "existing_browser_tools": "preserved: blender_vision.perception.browser.BrowserAdapter",
        "serialization": "scripts/with-one-browser.sh",
        "status": "PASS" if demo["all_kinds_fired"] else "FAIL",
    }

    if snapshot_path is not None:
        data = json.loads(Path(snapshot_path).read_text(encoding="utf-8"))
        eyeball = build_eyeball_from_snapshot(data)
        atomic_write_json(output / "snapshot_eyeball.json", eyeball.to_dict())
        receipt["snapshot"] = {
            "path": str(snapshot_path),
            "contradiction_count": len(eyeball.contradictions),
            "kinds": sorted({c.kind.value for c in eyeball.contradictions}),
        }

    if physical and url:
        eyeball, att = capture_page_physical(url, output=output / "physical")
        receipt["physical"] = {
            "url": url,
            "attestation": att.to_dict(),
            "eyeball": None if eyeball is None else {
                "contradiction_count": len(eyeball.contradictions),
                "kinds": sorted({c.kind.value for c in eyeball.contradictions}),
                "execution_class": eyeball.execution_class,
            },
        }
        if att.execution_class is ExecutionClass.BLOCKED:
            receipt.setdefault("blockers", []).append(
                {"id": "browser_physical", "reason": att.blocked_reason}
            )
    elif physical and not url:
        att = attest_blocked(
            "browser-physical",
            "physical=True requires a url (file:// or http://127.0.0.1 local only)",
        )
        receipt["physical"] = {"attestation": att.to_dict()}

    atomic_write_json(output / "browser_eyeball.receipt.json", receipt)
    return receipt
