from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.projects.store import ProjectStore

SOURCE_KINDS = {"generative_multiview", "gaussian_visual_oracle", "current_candidate"}


class SyntheticViewStore:
    """Govern hypothetical views so they can guide work but never become evidence."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def register(
        self,
        artifact_digest: str,
        *,
        source_kind: str,
        generator: dict[str, Any],
        input_reference_ids: list[str],
        view_identity: dict[str, Any],
        consistency: dict[str, float],
    ) -> dict[str, Any]:
        if source_kind not in SOURCE_KINDS:
            raise ValueError("unsupported synthetic view source")
        if not generator.get("backend") or not generator.get("revision"):
            raise ValueError("synthetic view requires generator backend and revision")
        with self.project.connection() as connection:
            artifact = connection.execute(
                "SELECT digest FROM artifacts WHERE digest=?", (artifact_digest,)
            ).fetchone()
            known_references = {
                row[0] for row in connection.execute("SELECT id FROM reference_items")
            }
        if artifact is None:
            raise ValueError("synthetic view artifact is not registered")
        if not set(input_reference_ids).issubset(known_references):
            raise ValueError("synthetic view references unknown source evidence")
        if any(
            not isinstance(value, (int, float)) or not math.isfinite(float(value))
            for value in consistency.values()
        ):
            raise ValueError("synthetic view consistency metrics must be finite")
        coherent = bool(consistency) and all(
            0.0 <= float(value) <= 1.0 for value in consistency.values()
        ) and min(float(value) for value in consistency.values()) >= 0.7
        view_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "schema_version": 1,
            "id": view_id,
            "source_kind": source_kind,
            "artifact_digest": artifact_digest,
            "generator": generator,
            "input_reference_ids": input_reference_ids,
            "view_identity": view_identity,
            "consistency": consistency,
            "coherent": coherent,
            "evidence_class": "SYNTHETIC_HYPOTHESIS",
            "acceptance_eligible": False,
            "permitted_uses": [
                "topology_initialization",
                "hidden_surface_hypothesis",
                "proposal_comparison",
            ],
            "created_at": now,
        }
        relative = Path("geometry") / "synthetic-views" / f"synthetic-view-{view_id}.json"
        atomic_write_json(self.project.root / relative, record)
        receipt = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.synthetic-view+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO synthetic_views"
                "(id,source_kind,artifact_digest,record_json,created_at) "
                "VALUES(?,?,?,?,?)",
                (view_id, source_kind, artifact_digest, json.dumps(record), now),
            )
        return {**record, "record_artifact": receipt.to_dict(), "path": str(relative)}

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT record_json FROM synthetic_views ORDER BY created_at,id"
            ).fetchall()
        return [json.loads(row["record_json"]) for row in rows]
