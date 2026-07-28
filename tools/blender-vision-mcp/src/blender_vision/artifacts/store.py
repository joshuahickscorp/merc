from __future__ import annotations

import mimetypes
import os
import shutil
import tempfile
from pathlib import Path
from typing import Any

from blender_vision.core.errors import SecurityError
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
        supplied = source.expanduser().absolute()
        if supplied.is_symlink():
            raise SecurityError("artifact source cannot be a symlink")
        source = supplied.resolve()
        if not source.is_file():
            raise FileNotFoundError(source)
        digest, size = sha256_file(source)
        destination = self.path_for(digest)
        if not destination.resolve().is_relative_to(self.root.resolve()):
            raise SecurityError("artifact destination escaped the content-addressed root")
        destination.parent.mkdir(parents=True, exist_ok=True)
        if destination.is_symlink():
            raise SecurityError("artifact destination cannot be a symlink")
        if not destination.exists():
            descriptor, temporary = tempfile.mkstemp(prefix=f".{digest}.", dir=destination.parent)
            os.close(descriptor)
            try:
                shutil.copyfile(source, temporary)
                os.replace(temporary, destination)
            finally:
                if os.path.exists(temporary):
                    os.unlink(temporary)
        else:
            observed_digest, observed_size = sha256_file(destination)
            if observed_digest != digest or observed_size != size:
                raise SecurityError("existing content-addressed artifact was substituted")
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

    def verify(self, digest: str) -> dict[str, Any]:
        path = self.path_for(digest)
        if not path.resolve().is_relative_to(self.root.resolve()):
            raise SecurityError("content-addressed artifact escaped its root")
        if path.is_symlink() or not path.is_file():
            raise SecurityError("content-addressed artifact is missing or linked")
        observed_digest, observed_size = sha256_file(path)
        if observed_digest != digest:
            raise SecurityError("content-addressed artifact digest mismatch")
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT size,relative_path FROM artifacts WHERE digest=?",
                (digest,),
            ).fetchone()
        if row is None:
            raise SecurityError("content-addressed artifact is not registered")
        expected_relative = str(path.relative_to(self.project.root))
        if int(row["size"]) != observed_size or row["relative_path"] != expected_relative:
            raise SecurityError("content-addressed artifact metadata mismatch")
        return {
            "valid": True,
            "digest": digest,
            "size": observed_size,
            "relative_path": expected_relative,
        }

    def materialize(self, digest: str, destination: Path) -> Path:
        source = self.path_for(digest)
        self.verify(digest)
        if destination.expanduser().absolute().is_symlink():
            raise SecurityError("artifact materialization target cannot be a symlink")
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
