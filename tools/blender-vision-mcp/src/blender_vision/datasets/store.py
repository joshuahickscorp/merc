from __future__ import annotations

import json
import math
import random
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.projects.store import ProjectStore
from blender_vision.security.paths import confined_path


class DatasetStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def plan_synthetic(
        self,
        name: str,
        *,
        sample_count: int,
        seed: int,
        scene_id: str | None = None,
        component_ids: list[str] | None = None,
        domain_randomization: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        if not 1 <= sample_count <= 100_000:
            raise ValueError("synthetic dataset sample_count must be between 1 and 100,000")
        with self.project.connection() as connection:
            scene_rows = connection.execute(
                "SELECT id,artifact_digest FROM scene_assets ORDER BY created_at"
            ).fetchall()
            known_components = {
                row[0] for row in connection.execute("SELECT id FROM components").fetchall()
            }
        if not scene_rows:
            raise ProjectError("synthetic dataset planning requires an imported Blender scene")
        selected_scene = next(
            (row for row in scene_rows if row["id"] == scene_id),
            scene_rows[-1] if scene_id is None else None,
        )
        if selected_scene is None:
            raise ProjectError(f"unknown scene for synthetic dataset: {scene_id}")
        component_ids = component_ids or []
        if not set(component_ids).issubset(known_components):
            raise ValueError("synthetic dataset references unknown components")
        randomization = {
            "lighting": {"energy_range": [0.3, 3.0], "temperature_kelvin": [3200, 7500]},
            "roughness": [0.15, 0.85],
            "exposure_ev": [-2.0, 2.0],
            "camera_azimuth_degrees": [-180.0, 180.0],
            "camera_elevation_degrees": [-35.0, 55.0],
            "camera_fov_degrees": [28.0, 72.0],
            "blur_sigma_px": [0.0, 1.5],
            "noise_sigma": [0.0, 0.035],
            "background_hsv": [[0.0, 0.0, 0.03], [1.0, 0.35, 0.9]],
            "occlusion_fraction": [0.0, 0.25],
            "manufacturing_variation_fraction": [-0.01, 0.01],
            **(domain_randomization or {}),
        }
        generator = random.Random(seed)
        preview = [
            {
                "sample_index": index,
                "azimuth_degrees": generator.uniform(*randomization["camera_azimuth_degrees"]),
                "elevation_degrees": generator.uniform(*randomization["camera_elevation_degrees"]),
                "fov_degrees": generator.uniform(*randomization["camera_fov_degrees"]),
                "exposure_ev": generator.uniform(*randomization["exposure_ev"]),
            }
            for index in range(min(sample_count, 16))
        ]
        manifest = {
            "schema_version": 1,
            "generator": "blender_vision.synthetic.v1",
            "sample_count": sample_count,
            "seed": seed,
            "scene_id": selected_scene["id"],
            "scene_artifact_digest": selected_scene["artifact_digest"],
            "component_ids": component_ids,
            "outputs": [
                "beauty",
                "instance_mask",
                "feature_mask",
                "keypoints",
                "object_ids",
                "feature_ids",
                "dimensions_mm",
                "pose",
                "orientation",
                "visible_fraction",
                "occlusion_fraction",
                "cross_view_identity",
                "camera_matrix",
                "depth",
                "normals",
                "materials",
                "lighting",
                "occlusion",
            ],
            "domain_randomization": randomization,
            "deterministic_preview": preview,
            "execution": {
                "state": "planned",
                "worker_operation": "generate_synthetic_dataset",
                "no_network": True,
            },
        }
        return self.register(
            name,
            "synthetic_technical_product",
            manifest,
            rights_state="SYNTHETIC_OWNED",
            status="planned",
        )

    def register(
        self,
        name: str,
        kind: str,
        manifest: dict[str, Any],
        *,
        rights_state: str,
        status: str = "registered",
    ) -> dict[str, Any]:
        if not name.strip() or not kind.strip() or not rights_state.strip():
            raise ValueError("dataset requires name, kind, and rights state")
        if rights_state == "UNKNOWN":
            raise ValueError("dataset rights must be reviewed before registration")
        digests = self._manifest_digests(manifest)
        with self.project.connection() as connection:
            known = {
                row[0] for row in connection.execute("SELECT digest FROM artifacts").fetchall()
            }
        if not digests.issubset(known):
            raise ValueError("dataset manifest references unregistered artifacts")
        dataset_id = str(uuid.uuid4())
        now = utc_now()
        record = {
            "schema_version": 1,
            "id": dataset_id,
            "name": name.strip(),
            "kind": kind.strip(),
            "status": status,
            "rights_state": rights_state,
            "manifest": manifest,
            "created_at": now,
        }
        relative = Path("training") / "datasets" / f"dataset-{dataset_id}.json"
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative, media_type="application/vnd.bvmcp.dataset+json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO datasets("
                "id,name,kind,status,manifest_json,record_digest,rights_state,"
                "created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    dataset_id,
                    record["name"],
                    record["kind"],
                    status,
                    json.dumps(manifest),
                    artifact.digest,
                    rights_state,
                    now,
                    now,
                ),
            )
        return {**record, "artifact": artifact.to_dict(), "path": str(relative)}

    def mark_generated(
        self, dataset_id: str, *, artifact_digests: list[str], sample_count: int
    ) -> dict[str, Any]:
        dataset = self.get(dataset_id)
        if dataset["status"] != "planned":
            raise ProjectError(f"dataset is not awaiting generation: {dataset_id}")
        if not artifact_digests or sample_count <= 0:
            raise ValueError("generated dataset requires artifacts and positive sample count")
        if sample_count != int(dataset["manifest"]["sample_count"]):
            raise ValueError("generated dataset sample count must match its immutable plan")
        with self.project.connection() as connection:
            known = {
                row[0] for row in connection.execute("SELECT digest FROM artifacts").fetchall()
            }
        if not set(artifact_digests).issubset(known):
            raise ValueError("generated dataset references unregistered artifacts")
        manifest = dataset["manifest"]
        manifest["execution"] = {
            "state": "generated",
            "artifact_digests": artifact_digests,
            "generated_sample_count": sample_count,
            "plan_record_digest": dataset["artifact_digest"],
        }
        now = utc_now()
        record = {
            "schema_version": 1,
            "id": dataset_id,
            "name": dataset["name"],
            "kind": dataset["kind"],
            "status": "generated",
            "rights_state": dataset["rights_state"],
            "manifest": manifest,
            "created_at": dataset["created_at"],
            "updated_at": now,
        }
        relative = Path("training") / "datasets" / f"dataset-{dataset_id}-generated.json"
        atomic_write_json(self.project.root / relative, record)
        generated_record = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.dataset+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE datasets SET status='generated',manifest_json=?,record_digest=?,"
                "updated_at=? WHERE id=?",
                (json.dumps(manifest), generated_record.digest, now, dataset_id),
            )
        return self.get(dataset_id)

    def get(self, dataset_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM datasets WHERE id=?", (dataset_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown dataset: {dataset_id}")
        value = dict(row)
        value["manifest"] = json.loads(value.pop("manifest_json"))
        value["artifact_digest"] = value.pop("record_digest")
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0] for row in connection.execute("SELECT id FROM datasets ORDER BY created_at")
            ]
        return [self.get(dataset_id) for dataset_id in ids]

    @staticmethod
    def _manifest_digests(value: Any) -> set[str]:
        found: set[str] = set()
        if isinstance(value, dict):
            for key, item in value.items():
                if key.endswith("digest") and isinstance(item, str) and len(item) == 64:
                    found.add(item)
                elif key.endswith("digests") and isinstance(item, list):
                    found.update(
                        digest for digest in item if isinstance(digest, str) and len(digest) == 64
                    )
                else:
                    found.update(DatasetStore._manifest_digests(item))
        elif isinstance(value, list):
            for item in value:
                found.update(DatasetStore._manifest_digests(item))
        return found


class TrainingStore:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def plan(
        self,
        dataset_id: str,
        *,
        backend: str,
        configuration: dict[str, Any],
    ) -> dict[str, Any]:
        dataset = DatasetStore(self.project).get(dataset_id)
        if dataset["status"] != "generated":
            raise ProjectError("feature-model training requires a generated dataset")
        if not backend.strip():
            raise ValueError("training backend identity is required")
        run_id = str(uuid.uuid4())
        now = utc_now()
        config = {
            **configuration,
            "network_allowed": False,
            "silent_weight_downloads": False,
            "dataset_manifest_digest": dataset["artifact_digest"],
        }
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO training_runs("
                "id,dataset_id,backend,status,config_json,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (run_id, dataset_id, backend.strip(), "planned", json.dumps(config), now, now),
            )
        return self.get(run_id)

    def import_result(
        self,
        run_id: str,
        checkpoint_path: Path,
        *,
        metrics: dict[str, float],
        license_record: dict[str, Any],
        model_revision: str,
    ) -> dict[str, Any]:
        run = self.get(run_id)
        if run["status"] != "planned":
            raise ProjectError(f"training run is not awaiting a result: {run_id}")
        checkpoint = confined_path(self.project.root, checkpoint_path, must_exist=True)
        if not license_record.get("license"):
            raise ValueError("trained model requires a license identifier")
        if license_record.get("commercial_use") is not True:
            raise ValueError("default-lane trained models require commercial-use clearance")
        if not model_revision.strip():
            raise ValueError("trained model requires a model revision")
        if any(
            not isinstance(value, (int, float)) or not math.isfinite(float(value))
            for value in metrics.values()
        ):
            raise ValueError("training metrics must be finite numbers")
        artifact = self.artifacts.ingest_file(
            checkpoint, media_type="application/vnd.bvmcp.model-checkpoint"
        )
        actual_hash, _size = sha256_file(checkpoint)
        result = {
            "model_revision": model_revision.strip(),
            "checkpoint_sha256": actual_hash,
            "metrics": metrics,
            "license": license_record,
            "commercial_eligible": True,
        }
        now = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE training_runs SET status='completed',result_json=?,checkpoint_digest=?,"
                "updated_at=? WHERE id=? AND status='planned'",
                (json.dumps(result), artifact.digest, now, run_id),
            )
        return self.get(run_id)

    def evaluate(
        self,
        dataset_id: str,
        predictions_path: Path,
        *,
        training_run_id: str | None = None,
    ) -> dict[str, Any]:
        dataset = DatasetStore(self.project).get(dataset_id)
        predictions = confined_path(self.project.root, predictions_path, must_exist=True)
        document = json.loads(predictions.read_text(encoding="utf-8"))
        counts = document.get("counts", {})
        true_positive = int(counts.get("true_positive", 0))
        false_positive = int(counts.get("false_positive", 0))
        false_negative = int(counts.get("false_negative", 0))
        intersection = float(counts.get("mask_intersection", 0.0))
        union = float(counts.get("mask_union", 0.0))
        precision = true_positive / max(1, true_positive + false_positive)
        recall = true_positive / max(1, true_positive + false_negative)
        metrics = {
            "precision": precision,
            "recall": recall,
            "f1": 2.0 * precision * recall / max(1e-12, precision + recall),
            "mask_iou": intersection / max(1.0, union),
            "evaluation_sample_count": int(document.get("sample_count", 0)),
            "dataset_generated_sample_count": int(
                dataset["manifest"].get("execution", {}).get("generated_sample_count", 0)
            ),
        }
        artifact = self.artifacts.ingest_file(
            predictions, media_type="application/vnd.bvmcp.feature-predictions+json"
        )
        evaluation_id = str(uuid.uuid4())
        created_at = utc_now()
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO model_evaluations("
                "id,training_run_id,dataset_id,metrics_json,predictions_digest,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    evaluation_id,
                    training_run_id,
                    dataset_id,
                    json.dumps(metrics),
                    artifact.digest,
                    created_at,
                ),
            )
        return {
            "id": evaluation_id,
            "training_run_id": training_run_id,
            "dataset_id": dataset_id,
            "metrics": metrics,
            "predictions_artifact": artifact.to_dict(),
            "created_at": created_at,
        }

    def get(self, run_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM training_runs WHERE id=?", (run_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown training run: {run_id}")
        value = dict(row)
        value["configuration"] = json.loads(value.pop("config_json"))
        raw = value.pop("result_json")
        value["result"] = json.loads(raw) if raw else None
        return value

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute("SELECT id FROM training_runs ORDER BY created_at")
            ]
        return [self.get(run_id) for run_id in ids]
