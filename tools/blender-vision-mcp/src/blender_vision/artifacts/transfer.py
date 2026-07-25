from __future__ import annotations

import base64
import binascii
import re
import uuid
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import sha256_file, utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.scheduling.distributed import DistributedScheduler
from blender_vision.security.paths import confined_path

MAX_CHUNK_BYTES = 1024 * 1024
MAX_TRANSFER_BYTES = 4 * 1024 * 1024 * 1024
MEDIA_TYPE = re.compile(r"^[A-Za-z0-9!#$&^_.+\-/]{1,127}$")


class ArtifactTransfer:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.workers = DistributedScheduler(project)

    def describe(self, digest: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT digest,size,media_type,source_name,created_at "
                "FROM artifacts WHERE digest=?",
                (digest,),
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown artifact: {digest}")
        return dict(row)

    def read_chunk(
        self,
        worker_id: str,
        worker_token: str,
        digest: str,
        *,
        offset: int,
        maximum_bytes: int = MAX_CHUNK_BYTES,
    ) -> dict[str, Any]:
        self.workers.authenticate(worker_id, worker_token)
        if offset < 0 or not 1 <= maximum_bytes <= MAX_CHUNK_BYTES:
            raise ValueError("invalid artifact chunk range")
        path = self.artifacts.path_for(digest)
        size = path.stat().st_size
        if offset > size:
            raise ValueError("artifact chunk offset exceeds artifact size")
        with path.open("rb") as handle:
            handle.seek(offset)
            data = handle.read(maximum_bytes)
        return {
            "digest": digest,
            "offset": offset,
            "next_offset": offset + len(data),
            "size": size,
            "eof": offset + len(data) == size,
            "data_base64": base64.b64encode(data).decode(),
        }

    def begin_upload(
        self,
        worker_id: str,
        worker_token: str,
        *,
        expected_digest: str,
        expected_size: int,
        media_type: str,
        source_name: str,
    ) -> dict[str, Any]:
        self.workers.authenticate(worker_id, worker_token)
        if not re.fullmatch(r"[0-9a-f]{64}", expected_digest):
            raise ValueError("artifact upload requires a SHA-256 digest")
        if not 0 <= expected_size <= MAX_TRANSFER_BYTES:
            raise ValueError("artifact upload exceeds the 4 GiB limit")
        if not MEDIA_TYPE.fullmatch(media_type):
            raise ValueError("artifact upload has an invalid media type")
        transfer_id = str(uuid.uuid4())
        relative = Path("jobs") / "transfers" / f"{transfer_id}.part"
        path = confined_path(self.project.root, self.project.root / relative)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.touch(exist_ok=False)
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO artifact_transfers(id,worker_id,expected_digest,expected_size,"
                "media_type,source_name,relative_path,next_offset,status,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?)",
                (
                    transfer_id,
                    worker_id,
                    expected_digest,
                    expected_size,
                    media_type,
                    Path(source_name).name or "worker-output",
                    str(relative),
                    0,
                    "uploading",
                    now,
                    now,
                ),
            )
        return {
            "transfer_id": transfer_id,
            "next_offset": 0,
            "maximum_chunk_bytes": MAX_CHUNK_BYTES,
        }

    def upload_chunk(
        self,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
        *,
        offset: int,
        data_base64: str,
    ) -> dict[str, Any]:
        self.workers.authenticate(worker_id, worker_token)
        try:
            data = base64.b64decode(data_base64, validate=True)
        except (binascii.Error, ValueError) as error:
            raise ValueError("artifact chunk is not valid base64") from error
        if len(data) > MAX_CHUNK_BYTES:
            raise ValueError("artifact chunk exceeds the 1 MiB limit")
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT * FROM artifact_transfers WHERE id=? AND worker_id=?",
                (transfer_id, worker_id),
            ).fetchone()
            if row is None or row["status"] != "uploading":
                raise KeyError("unknown active artifact transfer")
            if offset != row["next_offset"]:
                raise ValueError("artifact chunks must be uploaded sequentially")
            next_offset = offset + len(data)
            if next_offset > row["expected_size"]:
                raise ValueError("artifact chunk exceeds declared upload size")
            path = confined_path(self.project.root, self.project.root / row["relative_path"])
            with path.open("ab") as handle:
                handle.write(data)
            connection.execute(
                "UPDATE artifact_transfers SET next_offset=?,updated_at=? WHERE id=?",
                (next_offset, utc_now(), transfer_id),
            )
        return {"transfer_id": transfer_id, "next_offset": next_offset}

    def complete_upload(
        self,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
    ) -> dict[str, Any]:
        self.workers.authenticate(worker_id, worker_token)
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM artifact_transfers WHERE id=? AND worker_id=?",
                (transfer_id, worker_id),
            ).fetchone()
        if row is None or row["status"] != "uploading":
            raise KeyError("unknown active artifact transfer")
        path = confined_path(self.project.root, self.project.root / row["relative_path"])
        actual_digest, actual_size = sha256_file(path)
        if actual_size != row["expected_size"] or actual_digest != row["expected_digest"]:
            with self.project.connection() as connection:
                connection.execute(
                    "UPDATE artifact_transfers SET status='rejected',updated_at=? WHERE id=?",
                    (utc_now(), transfer_id),
                )
            path.unlink(missing_ok=True)
            raise ValueError("artifact upload does not match its declared size and digest")
        artifact = self.artifacts.ingest_file(path, media_type=row["media_type"])
        with self.project.connection() as connection:
            now = utc_now()
            connection.execute(
                "UPDATE artifact_transfers SET status='completed',updated_at=? WHERE id=?",
                (now, transfer_id),
            )
            connection.execute(
                "INSERT OR REPLACE INTO artifact_locations(digest,worker_id,updated_at) "
                "VALUES(?,?,?)",
                (artifact.digest, worker_id, now),
            )
        path.unlink(missing_ok=True)
        return {"transfer_id": transfer_id, "artifact": artifact.to_dict(), "verified": True}

    def abort_upload(
        self,
        worker_id: str,
        worker_token: str,
        transfer_id: str,
    ) -> dict[str, Any]:
        """Abort only the authenticated worker's active upload and remove its partial file."""
        self.workers.authenticate(worker_id, worker_token)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            row = connection.execute(
                "SELECT * FROM artifact_transfers WHERE id=? AND worker_id=?",
                (transfer_id, worker_id),
            ).fetchone()
            if row is None or row["status"] != "uploading":
                raise KeyError("unknown active artifact transfer")
            connection.execute(
                "UPDATE artifact_transfers SET status='aborted',updated_at=? WHERE id=?",
                (utc_now(), transfer_id),
            )
        path = confined_path(self.project.root, self.project.root / row["relative_path"])
        path.unlink(missing_ok=True)
        return {"transfer_id": transfer_id, "status": "aborted", "recoverable": False}

    def reap_stale(self, *, maximum_age_seconds: int = 3600) -> dict[str, Any]:
        """Expire abandoned partial uploads without touching registered artifacts."""
        if not 60 <= maximum_age_seconds <= 7 * 24 * 3600:
            raise ValueError("stale upload age must be between one minute and seven days")
        cutoff = (datetime.now(UTC) - timedelta(seconds=maximum_age_seconds)).isoformat()
        expired = []
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            rows = connection.execute(
                "SELECT id,relative_path FROM artifact_transfers "
                "WHERE status='uploading' AND updated_at<?",
                (cutoff,),
            ).fetchall()
            now = utc_now()
            for row in rows:
                connection.execute(
                    "UPDATE artifact_transfers SET status='expired',updated_at=? WHERE id=?",
                    (now, row["id"]),
                )
                expired.append(dict(row))
        for row in expired:
            path = confined_path(self.project.root, self.project.root / row["relative_path"])
            path.unlink(missing_ok=True)
        return {"expired": len(expired), "transfer_ids": [row["id"] for row in expired]}
