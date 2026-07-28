from __future__ import annotations

import json
from pathlib import Path

from pydantic import BaseModel, ConfigDict

from blender_vision.app_build.specification import ApplicationReferencePacket
from blender_vision.core.util import sha256_file


class ReferencePacketLoadError(ValueError):
    pass


class LoadedApplicationReferencePacket(BaseModel):
    model_config = ConfigDict(arbitrary_types_allowed=True, extra="forbid")

    packet: ApplicationReferencePacket
    packet_path: Path
    source_root: Path
    verified_source_ids: set[str]
    source_paths: dict[str, Path]


class ReferencePacketLoader:
    def load(self, packet_path: Path) -> LoadedApplicationReferencePacket:
        expanded = packet_path.expanduser().absolute()
        if expanded.is_symlink():
            raise ReferencePacketLoadError("application reference packet cannot be a symlink")
        path = expanded.resolve()
        if not path.is_file():
            raise ReferencePacketLoadError(f"application reference packet is missing: {path}")
        try:
            document = json.loads(path.read_text(encoding="utf-8"))
        except json.JSONDecodeError as error:
            raise ReferencePacketLoadError(
                f"invalid application reference packet JSON: {error}"
            ) from error
        packet = ApplicationReferencePacket.model_validate(document)
        root = path.parent.resolve()
        verified: set[str] = set()
        source_paths: dict[str, Path] = {}
        for source in packet.sources:
            locator = Path(source.locator)
            if locator.is_absolute():
                raise ReferencePacketLoadError(
                    f"source {source.id} must use a packet-relative locator"
                )
            if ".." in locator.parts:
                raise ReferencePacketLoadError(
                    f"source {source.id} escaped the packet root: {source.locator}"
                )
            unresolved = root / locator
            component = root
            for part in locator.parts:
                if part in {"", "."}:
                    continue
                component /= part
                if component.is_symlink():
                    raise ReferencePacketLoadError(
                        f"source {source.id} cannot use symlinks: {source.locator}"
                    )
            candidate = unresolved.resolve()
            if not candidate.is_relative_to(root):
                raise ReferencePacketLoadError(
                    f"source {source.id} escaped the packet root: {source.locator}"
                )
            if not candidate.is_file():
                raise ReferencePacketLoadError(f"source {source.id} is missing: {source.locator}")
            digest, _size = sha256_file(candidate)
            if digest != source.digest:
                raise ReferencePacketLoadError(
                    f"source {source.id} digest mismatch: "
                    f"expected {source.digest}, observed {digest}"
                )
            if source.rights_state == "UNKNOWN":
                raise ReferencePacketLoadError(f"source {source.id} has no usable rights decision")
            verified.add(source.id)
            source_paths[source.id] = candidate
        return LoadedApplicationReferencePacket(
            packet=packet,
            packet_path=path,
            source_root=root,
            verified_source_ids=verified,
            source_paths=source_paths,
        )
