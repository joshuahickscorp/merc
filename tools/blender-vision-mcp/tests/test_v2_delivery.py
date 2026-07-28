"""Tests for the VisionMCP V2 web scene delivery compiler."""

from __future__ import annotations

import os
import struct
from pathlib import Path

import pytest

from blender_vision.cinematic.path import compose_flagship_datacentre_path
from blender_vision.delivery.compress import measure_and_select_compression, write_selected_payload
from blender_vision.delivery.manifest import (
    FROZEN_BUDGETS,
    build_delivery_manifest,
    evaluate_budgets,
)
from blender_vision.delivery.stream import build_streaming_plan
from blender_vision.v2.records import DeliveryAsset
from blender_vision.v2.validation import validate_record


def _write_bytes(path: Path, payload: bytes) -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(payload)
    return path


def test_compression_selection_varies_by_asset(tmp_path: Path) -> None:
    # Highly compressible: zlib/gzip should beat raw on size.
    compressible = (b"SERVER-RACK-PANEL-" * 4000) + b"\x00" * 2000
    # High entropy: transfer codecs expand; raw wins.
    rng = __import__("numpy").random.default_rng(42)
    random_bytes = rng.integers(0, 256, size=40_000, dtype=__import__("numpy").uint8).tobytes()

    path_a = _write_bytes(tmp_path / "compressible.bin", compressible)
    path_b = _write_bytes(tmp_path / "random.bin", random_bytes)

    sel_a = measure_and_select_compression(
        path_a, asset_id="compressible", include_position_quantize=False
    )
    sel_b = measure_and_select_compression(
        path_b, asset_id="random", include_position_quantize=False
    )

    assert sel_a.selected_codec != "raw", sel_a.selection_reason
    assert sel_b.selected_codec == "raw", sel_b.selection_reason
    assert sel_a.selected_codec != sel_b.selected_codec
    # Table must include measured fields for available codecs.
    for selection in (sel_a, sel_b):
        available = [c for c in selection.candidates if c.available]
        assert len(available) >= 2
        for candidate in available:
            assert candidate.bytes >= 0
            assert candidate.decode_ms >= 0.0
            assert candidate.visual_difference >= 0.0


def test_budget_violation_reported_not_suppressed() -> None:
    assets = [
        DeliveryAsset(
            asset_id="shell",
            role="shell",
            path="shell.glb",
            digest="a" * 64,
            bytes=3 * 1024 * 1024,  # over 1.5 MB
        ),
        DeliveryAsset(
            asset_id="mobile",
            role="mobile",
            path="mobile.glb",
            digest="b" * 64,
            bytes=900 * 1024,  # over 650 KB
        ),
        DeliveryAsset(
            asset_id="poster",
            role="poster",
            path="poster.jpg",
            digest="c" * 64,
            bytes=80_000,
        ),
    ]
    graph = compose_flagship_datacentre_path()
    plan = build_streaming_plan(graph)
    violations = evaluate_budgets(
        assets,
        budgets=FROZEN_BUDGETS,
        streaming_plan=plan,
        initial_js_compressed_bytes=500 * 1024,
    )
    names = {item["budget"] for item in violations}
    assert "shell_glb_bytes" in names
    assert "mobile_shell_bytes" in names
    assert "initial_js_compressed_bytes" in names
    # Budgets must remain the frozen values — not raised to hide the breach.
    assert FROZEN_BUDGETS["shell_glb_bytes"] == int(1.5 * 1024 * 1024)
    assert FROZEN_BUDGETS["mobile_shell_bytes"] == 650 * 1024

    manifest = build_delivery_manifest(
        manifest_id="dm-violation",
        source_scene="flagship",
        assets=assets,
        streaming_plan=plan,
        initial_js_compressed_bytes=500 * 1024,
    )
    assert manifest.budget_violations
    assert len(manifest.budget_violations) >= 3
    validate_record(manifest)


def test_manifest_validates_against_frozen_v2_schema(tmp_path: Path) -> None:
    payload = b"x" * 1200
    path = _write_bytes(tmp_path / "shell.raw", payload)
    selection = measure_and_select_compression(
        path, asset_id="shell", include_position_quantize=False
    )
    out = write_selected_payload(selection, tmp_path / "shell.selected")
    assets = [
        DeliveryAsset(
            asset_id="poster",
            role="poster",
            path="poster.jpg",
            digest="d" * 64,
            bytes=12_000,
            compression="jpeg",
        ),
        DeliveryAsset(
            asset_id="shell",
            role="shell",
            path=str(out),
            digest=selection.selected.digest or ("e" * 64),
            bytes=selection.selected.bytes,
            compression=selection.selected_codec,
            decode_ms=selection.selected.decode_ms,
            main_thread_ms=selection.selected.main_thread_ms,
            visual_difference=selection.selected.visual_difference,
            lod="L0",
        ),
        DeliveryAsset(
            asset_id="mobile",
            role="mobile",
            path="mobile.glb",
            digest="f" * 64,
            bytes=100_000,
            compression="gzip",
            lod="L2",
        ),
        DeliveryAsset(
            asset_id="detail",
            role="detail",
            path="detail.glb",
            digest="1" * 64,
            bytes=200_000,
            compression="gzip",
            lod="L1",
            chapter="CAPACITY",
        ),
    ]
    graph = compose_flagship_datacentre_path()
    plan = build_streaming_plan(
        graph,
        asset_ids_by_role={
            "poster": ["poster"],
            "shell": ["shell"],
            "detail": ["detail"],
            "mobile": ["mobile"],
        },
    )
    manifest = build_delivery_manifest(
        manifest_id="dm-ok",
        source_scene="flagship",
        assets=assets,
        compression_selections=[selection],
        streaming_plan=plan,
        initial_js_compressed_bytes=120 * 1024,
    )
    validate_record(manifest)
    assert manifest.budgets["shell_glb_bytes"] == FROZEN_BUDGETS["shell_glb_bytes"]
    assert plan.stages[0].role == "poster"
    assert plan.stages[1].role == "shell"


def test_streaming_plan_chapter_gates() -> None:
    graph = compose_flagship_datacentre_path()
    plan = build_streaming_plan(graph)
    assert "TURN" in plan.chapter_gates
    detail = next(stage for stage in plan.stages if stage.stage_id == "detail_enrichment")
    assert detail.scroll_trigger > 0.0
    assert detail.chapter == "CAPACITY"
    assert any(item["trigger_id"] == "junction_second_corridor" for item in plan.prefetch_triggers)


def _minimal_glb(triangle_scale: int = 1) -> bytes:
    """Build a tiny valid GLB with one triangle for LOD tests."""
    import json

    # 3 vertices, 1 triangle
    positions = struct.pack(
        "<9f",
        0.0,
        0.0,
        0.0,
        float(triangle_scale),
        0.0,
        0.0,
        0.0,
        float(triangle_scale),
        0.0,
    )
    indices = struct.pack("<3H", 0, 1, 2)
    binary = positions + indices
    # pad binary to 4 bytes
    while len(binary) % 4:
        binary += b"\x00"
    document = {
        "asset": {"version": "2.0"},
        "buffers": [{"byteLength": len(binary)}],
        "bufferViews": [
            {"buffer": 0, "byteOffset": 0, "byteLength": 36, "target": 34962},
            {"buffer": 0, "byteOffset": 36, "byteLength": 6, "target": 34963},
        ],
        "accessors": [
            {
                "bufferView": 0,
                "componentType": 5126,
                "count": 3,
                "type": "VEC3",
                "max": [float(triangle_scale), float(triangle_scale), 0.0],
                "min": [0.0, 0.0, 0.0],
            },
            {"bufferView": 1, "componentType": 5123, "count": 3, "type": "SCALAR"},
        ],
        "meshes": [
            {
                "primitives": [
                    {"attributes": {"POSITION": 0}, "indices": 1, "mode": 4}
                ]
            }
        ],
        "nodes": [{"mesh": 0}],
        "scenes": [{"nodes": [0]}],
        "scene": 0,
    }
    json_bytes = json.dumps(document, separators=(",", ":")).encode("utf-8")
    while len(json_bytes) % 4:
        json_bytes += b" "
    out = bytearray()
    out += b"glTF"
    out += struct.pack("<I", 2)
    total = 12 + 8 + len(json_bytes) + 8 + len(binary)
    out += struct.pack("<I", total)
    out += struct.pack("<I", len(json_bytes))
    out += struct.pack("<I", 0x4E4F534A)
    out += json_bytes
    out += struct.pack("<I", len(binary))
    out += struct.pack("<I", 0x004E4942)
    out += binary
    return bytes(out)


def test_lod_identity_helpers(tmp_path: Path) -> None:
    from blender_vision.delivery.lods import _iou, _read_glb_vertices, _silhouette_from_vertices

    path = tmp_path / "tri.glb"
    path.write_bytes(_minimal_glb())
    verts, tris = _read_glb_vertices(path)
    assert tris == 1
    assert verts.shape == (3, 3)
    sil = _silhouette_from_vertices(verts)
    assert _iou(sil, sil) == 1.0


def test_lod_identity_check_python_fallback(tmp_path: Path) -> None:
    """LOD identity must hold for the measured Python fallback when Blender is blocked."""
    from blender_vision.delivery.lods import LodBudget, build_procedural_rack_glb, generate_lods

    source = build_procedural_rack_glb(tmp_path / "rack.glb", allow_python_fallback=True)
    report = generate_lods(
        source,
        [
            LodBudget(name="L1", max_triangles=120, screen_space_error_px=2.0),
            LodBudget(name="L2", max_triangles=40, screen_space_error_px=4.0),
        ],
        tmp_path / "lods",
        min_silhouette_iou=0.5,
        allow_python_fallback=True,
    )
    assert len(report.levels) == 2
    for level in report.levels:
        assert level.identity_pass is True
        assert level.triangles <= level.max_triangles
        assert Path(level.path).is_file()
    if not report.blender_used:
        assert any("blocked" in note.lower() or "blocker" in note.lower() for note in report.notes)


@pytest.mark.skipif(
    os.environ.get("BVMCP_RUN_BLENDER_TESTS") != "1",
    reason="set BVMCP_RUN_BLENDER_TESTS=1 for real Blender LOD generation",
)
def test_lod_identity_check_blender(tmp_path: Path) -> None:
    from blender_vision.cinematic.blender_probe import probe_blender
    from blender_vision.delivery.lods import LodBudget, build_procedural_rack_glb, generate_lods

    status = probe_blender()
    if not status["available"]:
        pytest.skip(f"BLOCKED_EXTERNAL: {status['reason']}")

    source = build_procedural_rack_glb(tmp_path / "rack.glb", allow_python_fallback=False)
    report = generate_lods(
        source,
        [
            LodBudget(name="L1", max_triangles=500, screen_space_error_px=2.0),
            LodBudget(name="L2", max_triangles=100, screen_space_error_px=4.0),
        ],
        tmp_path / "lods",
        min_silhouette_iou=0.5,
        allow_python_fallback=False,
    )
    assert report.blender_used is True
    assert len(report.levels) == 2
    for level in report.levels:
        assert level.identity_pass is True
        assert Path(level.path).is_file()
