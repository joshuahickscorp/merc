from __future__ import annotations

import json
import re
import uuid
from importlib.resources import files
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import sha256_file, utc_now
from blender_vision.projects.store import ProjectStore


class ModelStore:
    """Govern externally acquired checkpoints; this class never performs network I/O."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def approve_source(
        self,
        name: str,
        source_url: str,
        expected_sha256: str,
        *,
        license_record: dict[str, Any],
        approved_by: str,
        reason: str,
    ) -> dict[str, Any]:
        parsed = urlparse(source_url)
        if parsed.scheme not in {"https", "hf"} or not parsed.netloc:
            raise ValueError("model source must be an explicit HTTPS or hf:// URL")
        if not re.fullmatch(r"[0-9a-f]{64}", expected_sha256):
            raise ValueError("model approval requires an expected SHA-256 digest")
        if not name.strip() or not approved_by.strip() or not reason.strip():
            raise ValueError("model approval requires name, reviewer, and reason")
        if not license_record.get("license"):
            raise ValueError("model approval requires a license identifier")
        approval_id = str(uuid.uuid4())
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO model_approvals(id,name,source_url,expected_digest,license_json,"
                "approved_by,reason,status,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)",
                (
                    approval_id,
                    name.strip(),
                    source_url,
                    expected_sha256,
                    json.dumps(license_record),
                    approved_by.strip(),
                    reason.strip(),
                    "approved_for_manual_download",
                    now,
                    now,
                ),
            )
        return self.get_approval(approval_id)

    def import_checkpoint(
        self, approval_id: str, source: Path, *, revision: str
    ) -> dict[str, Any]:
        approval = self.get_approval(approval_id)
        if approval["status"] != "approved_for_manual_download":
            raise ProjectError("model source approval is not awaiting an import")
        if not revision.strip():
            raise ValueError("model checkpoint requires an immutable revision")
        source = source.expanduser().resolve()
        actual_digest, _size = sha256_file(source)
        if actual_digest != approval["expected_digest"]:
            raise ValueError("model checkpoint does not match its approved SHA-256 digest")
        artifact = self.artifacts.ingest_file(
            source, media_type="application/vnd.bvmcp.model-checkpoint"
        )
        installation_id = str(uuid.uuid4())
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO model_installations(id,approval_id,artifact_digest,revision,"
                "installed_at) VALUES(?,?,?,?,?)",
                (installation_id, approval_id, artifact.digest, revision.strip(), now),
            )
            connection.execute(
                "UPDATE model_approvals SET status='installed',updated_at=? WHERE id=?",
                (now, approval_id),
            )
        return {
            "id": installation_id,
            "approval": self.get_approval(approval_id),
            "artifact": artifact.to_dict(),
            "revision": revision.strip(),
            "installed_at": now,
            "commercial_eligible": bool(
                approval["license"].get("commercial_use") is True
                and not approval["license"].get("research_only", False)
            ),
        }

    def get_approval(self, approval_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM model_approvals WHERE id=?", (approval_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown model approval: {approval_id}")
        value = dict(row)
        value["license"] = json.loads(value.pop("license_json"))
        return value

    def list(self) -> dict[str, Any]:
        with self.project.connection() as connection:
            approvals = [
                self.get_approval(row[0])
                for row in connection.execute(
                    "SELECT id FROM model_approvals ORDER BY created_at"
                ).fetchall()
            ]
            installations = [
                dict(row)
                for row in connection.execute(
                    "SELECT * FROM model_installations ORDER BY installed_at"
                ).fetchall()
            ]
        resource = files("blender_vision").joinpath("MODEL_LICENSES.json")
        registry_path = Path(__file__).resolve().parents[3] / "MODEL_LICENSES.json"
        registry = json.loads(
            resource.read_text() if resource.is_file() else registry_path.read_text(encoding="utf-8")
        )
        return {
            "registry": registry,
            "approvals": approvals,
            "installations": installations,
            "policy": {
                "silent_downloads": False,
                "network_performed_by_coordinator": False,
                "digest_verification_required": True,
            },
        }
