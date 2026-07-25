from __future__ import annotations

import mimetypes
import os
import shutil
import tempfile
from pathlib import Path

from blender_vision.core.models import ArtifactRecord
from blender_vision.core.util import sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class ArtifactStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.root = project.root / "artifacts" / "sha256"

    def path_for(self, digest: str) -> Path:
        if len(digest) != 64 or any(character not in "0123456789abcdef" for character in digest):
            raise ValueError("invalid SHA-256 digest")
        return self.root / digest[:2] / digest[2:4] / digest

    def ingest_file(self, source: Path, *, media_type: str | None = None) -> ArtifactRecord:
        source = source.expanduser().resolve()
        if not source.is_file():
            raise FileNotFoundError(source)
        digest, size = sha256_file(source)
        destination = self.path_for(digest)
        destination.parent.mkdir(parents=True, exist_ok=True)
        if not destination.exists():
            descriptor, temporary = tempfile.mkstemp(prefix=f".{digest}.", dir=destination.parent)
            os.close(descriptor)
            try:
                shutil.copyfile(source, temporary)
                os.replace(temporary, destination)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        detected_type = (
            media_type or mimetypes.guess_type(source.name)[0] or "application/octet-stream"
        )
        record = ArtifactRecord(
            digest=digest,
            size=size,
            media_type=detected_type,
            path=str(destination.relative_to(self.project.root)),
            source_name=source.name,
            created_at=utc_now(),
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT OR IGNORE INTO artifacts"
                "(digest,size,media_type,relative_path,source_name,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (digest, size, detected_type, record.path, source.name, record.created_at),
            )
        return record

    def materialize(self, digest: str, destination: Path) -> Path:
        source = self.path_for(digest)
        if not source.is_file():
            raise FileNotFoundError(source)
        destination.parent.mkdir(parents=True, exist_ok=True)
        if destination.exists():
            existing_digest, _ = sha256_file(destination)
            if existing_digest != digest:
                raise FileExistsError(destination)
            return destination
        try:
            os.link(source, destination)
        except OSError:
            shutil.copy2(source, destination)
        return destination
