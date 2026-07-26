from __future__ import annotations

import json
import struct
from pathlib import Path
from typing import Any

import pytest

from blender_vision.geometry import GlbValidator


def _glb(document: dict[str, Any], binary: bytes = b"") -> bytes:
    json_chunk = json.dumps(
        document,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    json_chunk += b" " * (-len(json_chunk) % 4)
    chunks = [struct.pack("<II", len(json_chunk), 0x4E4F534A) + json_chunk]
    if binary:
        binary += b"\x00" * (-len(binary) % 4)
        chunks.append(struct.pack("<II", len(binary), 0x004E4942) + binary)
    body = b"".join(chunks)
    return struct.pack("<4sII", b"glTF", 2, 12 + len(body)) + body


def _minimal_document() -> tuple[dict[str, Any], bytes]:
    binary = struct.pack(
        "<9f",
        0.0,
        0.0,
        0.0,
        1.0,
        0.0,
        0.0,
        0.0,
        1.0,
        0.0,
    )
    return (
        {
            "asset": {"version": "2.0", "generator": "owned-test"},
            "scene": 0,
            "scenes": [{"name": "Scene", "nodes": [0]}],
            "nodes": [{"name": "ProductRoot", "mesh": 0}],
            "meshes": [
                {
                    "name": "ProductMesh",
                    "primitives": [{"attributes": {"POSITION": 0}, "mode": 4}],
                }
            ],
            "buffers": [{"byteLength": len(binary)}],
            "bufferViews": [{"buffer": 0, "byteOffset": 0, "byteLength": len(binary)}],
            "accessors": [
                {
                    "bufferView": 0,
                    "componentType": 5126,
                    "count": 3,
                    "type": "VEC3",
                    "min": [0.0, 0.0, 0.0],
                    "max": [1.0, 1.0, 0.0],
                }
            ],
        },
        binary,
    )


def test_glb_validator_accepts_bound_named_editable_structure(tmp_path: Path) -> None:
    document, binary = _minimal_document()
    path = tmp_path / "product.glb"
    path.write_bytes(_glb(document, binary))

    result = GlbValidator().validate(
        path,
        required_node_names=["ProductRoot"],
        required_mesh_names=["ProductMesh"],
    )

    assert result.valid is True
    assert result.errors == []
    assert result.metrics["mesh_count"] == 1
    assert result.metrics["primitive_count"] == 1
    assert result.metrics["binary_chunk_bytes"] == 36
    assert result.named_identity["missing_nodes"] == []


def test_glb_validator_rejects_external_assets_overruns_cycles_and_missing_names(
    tmp_path: Path,
) -> None:
    document, binary = _minimal_document()
    document["buffers"][0]["uri"] = "https://example.test/mesh.bin"
    document["bufferViews"][0]["byteLength"] = 12
    document["nodes"][0]["children"] = [0]
    document["images"] = [{"uri": "https://example.test/texture.png"}]
    path = tmp_path / "hostile.glb"
    path.write_bytes(_glb(document, binary))

    result = GlbValidator().validate(
        path,
        required_node_names=["MissingNode"],
        required_mesh_names=["MissingMesh"],
    )
    codes = {error["code"] for error in result.errors}

    assert result.valid is False
    assert {
        "EXTERNAL_BUFFER_URI",
        "ACCESSOR_OVERRUN",
        "NODE_CYCLE",
        "EXTERNAL_IMAGE_URI",
        "MISSING_REQUIRED_NODE",
        "MISSING_REQUIRED_MESH",
    } <= codes


def test_glb_validator_rejects_header_tamper_and_symlinks(tmp_path: Path) -> None:
    document, binary = _minimal_document()
    data = bytearray(_glb(document, binary))
    struct.pack_into("<I", data, 8, len(data) + 4)
    path = tmp_path / "tampered.glb"
    path.write_bytes(data)

    result = GlbValidator().validate(path)

    assert result.valid is False
    assert any(error["code"] == "LENGTH_MISMATCH" for error in result.errors)
    symlink = tmp_path / "link.glb"
    symlink.symlink_to(path)
    with pytest.raises(ValueError, match="non-symlink"):
        GlbValidator().validate(symlink)


def test_glb_validator_enforces_bounded_input(tmp_path: Path) -> None:
    path = tmp_path / "large.glb"
    path.write_bytes(b"x" * 33)

    with pytest.raises(ValueError, match="bounded validator size"):
        GlbValidator(maximum_bytes=32).validate(path)
