from __future__ import annotations

import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import EvidenceClass
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.base import GeometryEvidence

ARTIFACT_FIELDS = (
    "depth_artifacts",
    "point_artifacts",
    "normal_artifacts",
    "correspondence_artifacts",
    "visibility_artifacts",
    "confidence_artifacts",
    "mask_artifacts",
    "occupancy_artifacts",
    "silhouette_volume_artifacts",
    "visual_hull_artifacts",
)


class GeometryEvidenceStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def create(
        self,
        backend: str,
        backend_version: str,
        evidence: GeometryEvidence,
        *,
        evidence_class: EvidenceClass,
        configuration: dict[str, Any] | None = None,
        license_record: dict[str, Any] | None = None,
        commercial_eligible: bool,
    ) -> dict[str, Any]:
        if not backend.strip() or not backend_version.strip():
            raise ValueError("geometry evidence requires backend identity and version")
        evidence_value = evidence.to_dict()
        digests = [digest for field in ARTIFACT_FIELDS for digest in evidence_value.get(field, [])]
        with self.project.connection() as connection:
            known = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
        missing = sorted(set(digests) - known)
        if missing:
            raise ValueError(
                "geometry evidence references unknown artifact digests: " + ", ".join(missing)
            )
        run_id = str(uuid.uuid4())
        created_at = utc_now()
        record = {
            "schema_version": 1,
            "id": run_id,
            "backend": backend.strip(),
            "backend_version": backend_version.strip(),
            "evidence_class": evidence_class.value,
            "commercial_eligible": bool(commercial_eligible),
            "license": license_record or {},
            "configuration": configuration or {},
            "evidence": evidence_value,
            "created_at": created_at,
        }
        relative = Path("geometry") / f"run-{run_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.geometry-evidence+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO geometry_runs("
                "id,backend,backend_version,evidence_class,commercial_eligible,config_json,"
                "evidence_json,record_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    run_id,
                    record["backend"],
                    record["backend_version"],
                    record["evidence_class"],
                    int(record["commercial_eligible"]),
                    json.dumps(record["configuration"]),
                    json.dumps({"license": record["license"], **evidence_value}),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**record, "artifact": artifact.to_dict(), "path": str(relative)}

    def get(self, run_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM geometry_runs WHERE id=?", (run_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown geometry run: {run_id}")
        value = dict(row)
        evidence = json.loads(value.pop("evidence_json"))
        value["configuration"] = json.loads(value.pop("config_json"))
        value["commercial_eligible"] = bool(value["commercial_eligible"])
        value["license"] = evidence.pop("license", {})
        value["evidence"] = evidence
        value["artifact_digest"] = value.pop("record_digest")
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM geometry_runs ORDER BY created_at"
                ).fetchall()
            ]
        return [self.get(run_id) for run_id in ids]

    def add_consensus(self, run_ids: list[str], report: dict[str, Any]) -> dict[str, Any]:
        if len(set(run_ids)) < 2:
            raise ValueError("backend comparison requires at least two distinct geometry runs")
        known = {item["id"] for item in self.list()}
        if not set(run_ids).issubset(known):
            raise ValueError("backend comparison references unknown geometry runs")
        consensus_id = str(uuid.uuid4())
        created_at = utc_now()
        record = {
            "schema_version": 1,
            "id": consensus_id,
            "run_ids": run_ids,
            "report": report,
            "created_at": created_at,
        }
        relative = Path("geometry") / f"consensus-{consensus_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.geometry-consensus+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO geometry_consensus("
                "id,run_ids_json,report_json,record_digest,created_at) "
                "VALUES(?,?,?,?,?)",
                (
                    consensus_id,
                    json.dumps(run_ids),
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**record, "artifact": artifact.to_dict(), "path": str(relative)}

    def latest_consensus(self) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM geometry_consensus ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        if row is None:
            return None
        return {
            "id": row["id"],
            "run_ids": json.loads(row["run_ids_json"]),
            "report": json.loads(row["report_json"]),
            "artifact_digest": row["record_digest"],
            "created_at": row["created_at"],
        }
