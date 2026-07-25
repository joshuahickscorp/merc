from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import canonical_json, utc_now
from blender_vision.datasets.store import TrainingStore
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path


class VisualOracleStore:
    """Register hash-bound Gaussian or neural visual oracles without making mesh claims."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def register(
        self,
        source: Path,
        *,
        kind: str,
        camera_solution_ids: list[str],
        training_configuration: dict[str, Any],
        license_record: dict[str, Any],
    ) -> dict[str, Any]:
        if kind not in {"gaussian_splat", "neural_radiance_field", "reference_render_set"}:
            raise ValueError("unsupported visual oracle kind")
        source = confined_path(self.project.root, source, must_exist=True)
        if not camera_solution_ids:
            raise ValueError("visual oracle requires bound camera solutions")
        with self.project.connection() as connection:
            known = {
                row[0] for row in connection.execute("SELECT id FROM camera_solutions").fetchall()
            }
        if not set(camera_solution_ids).issubset(known):
            raise ValueError("visual oracle references unknown camera solutions")
        if not license_record.get("license"):
            raise ValueError("visual oracle requires a license identifier")
        artifact = self.artifacts.ingest_file(
            source, media_type="application/vnd.bvmcp.visual-oracle"
        )
        configuration = {
            "camera_solution_ids": camera_solution_ids,
            "training": training_configuration,
            "configuration_sha256": hashlib.sha256(
                canonical_json(
                    {
                        "camera_solution_ids": camera_solution_ids,
                        "training": training_configuration,
                    }
                )
            ).hexdigest(),
            "authority": ("appearance oracle only; cannot establish dimensions or hidden geometry"),
        }
        commercial = bool(
            license_record.get("commercial_use") is True
            and not license_record.get("research_only", False)
        )
        oracle_id = str(uuid.uuid4())
        created_at = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_oracles("
                "id,kind,artifact_digest,config_json,license_json,commercial_eligible,created_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (
                    oracle_id,
                    kind,
                    artifact.digest,
                    json.dumps(configuration),
                    json.dumps(license_record),
                    int(commercial),
                    created_at,
                ),
            )
        return self.get(oracle_id)

    def plan_training(
        self,
        dataset_id: str,
        *,
        kind: str,
        camera_solution_ids: list[str],
        backend: str,
        training_configuration: dict[str, Any],
    ) -> dict[str, Any]:
        """Plan licensed offline oracle training while binding the exact camera set."""
        if kind not in {"gaussian_splat", "neural_radiance_field"}:
            raise ValueError(
                "trainable visual oracle kind must be gaussian_splat or neural_radiance_field"
            )
        if not camera_solution_ids:
            raise ValueError("visual oracle training requires bound camera solutions")
        with self.project.connection() as connection:
            known = {
                row[0] for row in connection.execute("SELECT id FROM camera_solutions").fetchall()
            }
        if not set(camera_solution_ids).issubset(known):
            raise ValueError("visual oracle training references unknown camera solutions")
        configuration = {
            **training_configuration,
            "purpose": "visual_oracle",
            "oracle_kind": kind,
            "camera_solution_ids": camera_solution_ids,
            "authority": "appearance oracle only; cannot establish dimensions or hidden geometry",
        }
        return TrainingStore(self.project).plan(
            dataset_id,
            backend=backend,
            configuration=configuration,
        )

    def get(self, oracle_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_oracles WHERE id=?", (oracle_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual oracle: {oracle_id}")
        value = dict(row)
        value["configuration"] = json.loads(value.pop("config_json"))
        value["license"] = json.loads(value.pop("license_json"))
        value["commercial_eligible"] = bool(value["commercial_eligible"])
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute("SELECT id FROM visual_oracles ORDER BY created_at")
            ]
        return [self.get(oracle_id) for oracle_id in ids]
