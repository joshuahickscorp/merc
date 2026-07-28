from __future__ import annotations

import hashlib
import json
from pathlib import Path

import pytest
from test_application_specification import complete_packet_document

from blender_vision.app_build import ReferencePacketLoader, ReferencePacketLoadError


def _write_packet(root: Path) -> Path:
    document = complete_packet_document()
    for source in document["sources"]:
        payload = f"authority:{source['id']}\n".encode()
        locator = Path("sources") / f"{source['id']}.txt"
        source_path = root / locator
        source_path.parent.mkdir(parents=True, exist_ok=True)
        source_path.write_bytes(payload)
        source["locator"] = locator.as_posix()
        source["digest"] = hashlib.sha256(payload).hexdigest()
    packet_path = root / "packet.json"
    packet_path.write_text(json.dumps(document), encoding="utf-8")
    return packet_path


def test_loader_verifies_every_declared_source_byte(tmp_path: Path) -> None:
    loaded = ReferencePacketLoader().load(_write_packet(tmp_path))

    assert loaded.verified_source_ids == {source.id for source in loaded.packet.sources}
    assert all(path.is_file() for path in loaded.source_paths.values())


def test_loader_rejects_source_digest_mismatch(tmp_path: Path) -> None:
    packet_path = _write_packet(tmp_path)
    (tmp_path / "sources" / "brief.txt").write_text("changed\n", encoding="utf-8")

    with pytest.raises(ReferencePacketLoadError, match="brief digest mismatch"):
        ReferencePacketLoader().load(packet_path)


def test_loader_rejects_locator_escape(tmp_path: Path) -> None:
    packet_path = _write_packet(tmp_path)
    document = json.loads(packet_path.read_text(encoding="utf-8"))
    document["sources"][0]["locator"] = "../outside.txt"
    packet_path.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ReferencePacketLoadError, match="escaped the packet root"):
        ReferencePacketLoader().load(packet_path)


def test_loader_rejects_source_symlink(tmp_path: Path) -> None:
    packet_path = _write_packet(tmp_path)
    target = tmp_path / "sources" / "brief.txt"
    target.unlink()
    target.symlink_to(tmp_path / "sources" / "stories.txt")

    with pytest.raises(ReferencePacketLoadError, match="cannot use symlinks"):
        ReferencePacketLoader().load(packet_path)


def test_loader_rejects_unknown_rights(tmp_path: Path) -> None:
    packet_path = _write_packet(tmp_path)
    document = json.loads(packet_path.read_text(encoding="utf-8"))
    document["sources"][0]["rights_state"] = "UNKNOWN"
    packet_path.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ReferencePacketLoadError, match="no usable rights decision"):
        ReferencePacketLoader().load(packet_path)
