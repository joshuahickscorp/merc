from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.datasets.store import DatasetStore, TrainingStore
from blender_vision.projects.store import ProjectStore

MODEL_LEVELS = {
    "universal_technical_backbone",
    "category_head",
    "product_family_adapter",
    "project_few_shot_adapter",
}
QUALITY_METRICS = {"precision", "recall", "f1", "mask_iou", "keypoint_ap", "pose_ap"}
ALLOWED_TRANSITIONS = {
    None: {"AWAITING_CORRECTIONS", "NO_CORRECTIONS_NEEDED"},
    "AWAITING_CORRECTIONS": {"READY_TO_RETRAIN"},
    "READY_TO_RETRAIN": {"RETRAINING_PLANNED"},
    "RETRAINING_PLANNED": {"IMPROVED", "REJECTED"},
    "IMPROVED": {"PROMOTED"},
}


class ActiveLearningStore:
    """Artifact-bound correction, retraining, benchmark, and model-activation cycles."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def start(
        self,
        *,
        model_level: str,
        model_identity: dict[str, Any],
        predictions: list[dict[str, Any]],
        correction_budget: int = 32,
    ) -> dict[str, Any]:
        if model_level not in MODEL_LEVELS:
            raise ValueError("unsupported model hierarchy level")
        if not model_identity.get("name") or not model_identity.get("revision"):
            raise ValueError("active learning requires model name and revision")
        if not 1 <= correction_budget <= 10_000:
            raise ValueError("correction budget must be between 1 and 10,000")
        ranked = []
        prediction_ids: set[str] = set()
        for prediction in predictions:
            prediction_id = str(prediction.get("id", "")).strip()
            if not prediction_id or prediction_id in prediction_ids:
                raise ValueError("active-learning predictions require unique non-empty ids")
            prediction_ids.add(prediction_id)
            confidence = float(prediction.get("confidence", -1.0))
            impact = float(prediction.get("impact", -1.0))
            if not all(
                math.isfinite(value) and 0.0 <= value <= 1.0
                for value in (confidence, impact)
            ):
                raise ValueError("prediction confidence and impact must be within [0, 1]")
            ranked.append(
                {
                    **prediction,
                    "id": prediction_id,
                    "priority": (1.0 - confidence) * impact,
                    "correction_state": "requested",
                }
            )
        ranked.sort(key=lambda item: (-item["priority"], item["id"]))
        requests = [item for item in ranked[:correction_budget] if item["priority"] > 0.0]
        cycle_id = str(uuid.uuid4())
        prediction_document = {
            "schema_version": 1,
            "record_type": "active_learning_predictions",
            "cycle_id": cycle_id,
            "model_level": model_level,
            "model_identity": model_identity,
            "predictions": predictions,
        }
        prediction_relative = (
            Path("training") / "active-learning" / f"predictions-{cycle_id}.json"
        )
        atomic_write_json(self.project.root / prediction_relative, prediction_document)
        prediction_artifact = self.artifacts.ingest_file(
            self.project.root / prediction_relative,
            media_type="application/vnd.bvmcp.active-learning-predictions+json",
        )
        now = utc_now()
        record = {
            "schema_version": 2,
            "id": cycle_id,
            "revision": 1,
            "model_level": model_level,
            "model_identity": model_identity,
            "status": "AWAITING_CORRECTIONS" if requests else "NO_CORRECTIONS_NEEDED",
            "prediction_count": len(predictions),
            "predictions_digest": prediction_artifact.digest,
            "correction_budget": correction_budget,
            "correction_requests": requests,
            "corrected_samples": [],
            "retraining_plan": None,
            "benchmark_comparison": None,
            "activation": None,
            "created_at": now,
            "updated_at": now,
        }
        return self._persist(record)

    def record_corrections(
        self, cycle_id: str, corrections: list[dict[str, Any]], *, corrected_by: str
    ) -> dict[str, Any]:
        if not corrected_by.strip() or not corrections:
            raise ValueError("corrections require samples and a named corrector")
        record = self.get(cycle_id)
        if record["status"] != "AWAITING_CORRECTIONS":
            raise ValueError("active-learning cycle is not awaiting corrections")
        requested = {str(item.get("id")) for item in record["correction_requests"]}
        corrected_ids = [str(item.get("prediction_id", "")).strip() for item in corrections]
        if any(not item for item in corrected_ids) or not set(corrected_ids).issubset(requested):
            raise ValueError("correction references a prediction that was not requested")
        if len(corrected_ids) != len(set(corrected_ids)):
            raise ValueError("each requested prediction may be corrected only once per cycle")
        now = utc_now()
        record["corrected_samples"] = [
            {**item, "corrected_by": corrected_by.strip(), "corrected_at": now}
            for item in corrections
        ]
        self._advance(record, "READY_TO_RETRAIN", now)
        return self._persist(record, replace=True)

    def plan_retraining(
        self,
        cycle_id: str,
        *,
        backend: str,
        benchmark_dataset_id: str,
        required_metrics: list[str] | None = None,
        training_configuration: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        record = self.get(cycle_id)
        if record["status"] == "RETRAINING_PLANNED":
            return record
        if record["status"] != "READY_TO_RETRAIN":
            raise ValueError("corrections must be recorded before retraining")
        if not backend.strip():
            raise ValueError("retraining requires a backend identity")
        benchmark = DatasetStore(self.project).get(benchmark_dataset_id)
        if benchmark["rights_state"] == "UNKNOWN":
            raise ValueError("benchmark dataset rights must be reviewed")
        selected_metrics = list(
            dict.fromkeys(required_metrics or ["precision", "recall", "f1", "mask_iou"])
        )
        if not selected_metrics or not set(selected_metrics).issubset(QUALITY_METRICS):
            raise ValueError("active-learning benchmark metrics are unsupported")
        source_digest = self._snapshot_digest(cycle_id)
        correction_dataset = self._correction_dataset(record, source_digest)
        training_run = self._training_run(
            record,
            correction_dataset["id"],
            backend.strip(),
            benchmark,
            source_digest,
            training_configuration or {},
        )
        record["retraining_plan"] = {
            "backend": backend.strip(),
            "correction_dataset_id": correction_dataset["id"],
            "correction_dataset_digest": correction_dataset["artifact_digest"],
            "training_run_id": training_run["id"],
            "benchmark_dataset_id": benchmark_dataset_id,
            "benchmark_dataset_digest": benchmark["artifact_digest"],
            "required_metrics": selected_metrics,
            "corrected_sample_count": len(record["corrected_samples"]),
            "network_allowed": False,
            "requires_benchmark_comparison": True,
            "worker_operation": "training.execute",
        }
        self._advance(record, "RETRAINING_PLANNED")
        return self._persist(record, replace=True)

    def compare_evaluations(
        self,
        cycle_id: str,
        *,
        baseline_evaluation_id: str,
        candidate_evaluation_id: str,
    ) -> dict[str, Any]:
        record = self.get(cycle_id)
        if record["status"] in {"IMPROVED", "REJECTED"}:
            comparison = record["benchmark_comparison"] or {}
            if (
                comparison.get("baseline_evaluation_id") == baseline_evaluation_id
                and comparison.get("candidate_evaluation_id") == candidate_evaluation_id
            ):
                return record
        if record["status"] != "RETRAINING_PLANNED":
            raise ValueError("benchmark comparison requires a retraining plan")
        if baseline_evaluation_id == candidate_evaluation_id:
            raise ValueError("baseline and candidate evaluations must be distinct")
        baseline = self._evaluation(baseline_evaluation_id)
        candidate = self._evaluation(candidate_evaluation_id)
        plan = record["retraining_plan"]
        benchmark_id = plan["benchmark_dataset_id"]
        if baseline["dataset_id"] != benchmark_id or candidate["dataset_id"] != benchmark_id:
            raise ValueError("both evaluations must use the fixed benchmark dataset")
        if candidate["training_run_id"] != plan["training_run_id"]:
            raise ValueError("candidate evaluation is not bound to the planned retraining run")
        training = TrainingStore(self.project).get(plan["training_run_id"])
        if training["status"] != "completed" or not training.get("checkpoint_digest"):
            raise ValueError("planned retraining must complete before benchmark comparison")
        required = plan["required_metrics"]
        if not set(required).issubset(baseline["metrics"]):
            raise ValueError("baseline evaluation is missing required benchmark metrics")
        if not set(required).issubset(candidate["metrics"]):
            raise ValueError("candidate evaluation is missing required benchmark metrics")
        baseline_count = int(baseline["metrics"].get("evaluation_sample_count", 0))
        candidate_count = int(candidate["metrics"].get("evaluation_sample_count", 0))
        benchmark = DatasetStore(self.project).get(benchmark_id)
        benchmark_count = int(benchmark["manifest"].get("sample_count", 0))
        if (
            baseline_count <= 0
            or candidate_count != baseline_count
            or baseline_count != benchmark_count
        ):
            raise ValueError(
                "fixed benchmark evaluations must cover the complete matching sample set"
            )
        before = {key: float(baseline["metrics"][key]) for key in required}
        after = {key: float(candidate["metrics"][key]) for key in required}
        if any(not math.isfinite(value) for value in [*before.values(), *after.values()]):
            raise ValueError("benchmark metrics must be finite")
        deltas = {key: after[key] - before[key] for key in required}
        tolerance = 1e-12
        regressed = sorted(key for key, value in deltas.items() if value < -tolerance)
        improved = sorted(key for key, value in deltas.items() if value > tolerance)
        record["benchmark_comparison"] = {
            "benchmark_dataset_id": benchmark_id,
            "benchmark_dataset_digest": plan["benchmark_dataset_digest"],
            "baseline_evaluation_id": baseline_evaluation_id,
            "baseline_predictions_digest": baseline["predictions_digest"],
            "candidate_evaluation_id": candidate_evaluation_id,
            "candidate_predictions_digest": candidate["predictions_digest"],
            "candidate_training_run_id": training["id"],
            "candidate_checkpoint_digest": training["checkpoint_digest"],
            "before": before,
            "after": after,
            "deltas": deltas,
            "improved_metrics": improved,
            "regressed_metrics": regressed,
            "evaluation_sample_count": baseline_count,
            "policy": {
                "caller_asserted_metrics": False,
                "fixed_dataset_required": True,
                "no_regression_required": True,
                "at_least_one_improvement_required": True,
            },
        }
        self._advance(record, "IMPROVED" if not regressed and improved else "REJECTED")
        return self._persist(record, replace=True)

    def compare(
        self,
        cycle_id: str,
        *,
        before: dict[str, float],
        after: dict[str, float],
    ) -> dict[str, Any]:
        del cycle_id, before, after
        raise ValueError(
            "caller-supplied benchmark metrics are not authoritative; "
            "use compare_evaluations with stored evaluation ids"
        )

    def promote(self, cycle_id: str, *, reviewed_by: str, reason: str) -> dict[str, Any]:
        if not reviewed_by.strip() or not reason.strip():
            raise ValueError("model activation requires a named reviewer and reason")
        record = self.get(cycle_id)
        if record["status"] == "PROMOTED":
            activation = record.get("activation") or {}
            if activation.get("reviewed_by") == reviewed_by and activation.get("reason") == reason:
                return record
        if record["status"] != "IMPROVED":
            raise ValueError("only a non-regressing improved cycle may activate a model")
        comparison = record["benchmark_comparison"]
        training = TrainingStore(self.project).get(comparison["candidate_training_run_id"])
        result = training.get("result") or {}
        if training["status"] != "completed" or not result.get("commercial_eligible"):
            raise ValueError("activated model must be completed and commercially eligible")
        prior_snapshot_digest = self._snapshot_digest(cycle_id)
        with self.project.connection() as connection:
            previous = connection.execute(
                "SELECT id FROM active_model_revisions WHERE model_level=? AND model_name=? "
                "AND status='ACTIVE'",
                (record["model_level"], record["model_identity"]["name"]),
            ).fetchone()
        activation_id = str(uuid.uuid4())
        now = utc_now()
        activation = {
            "schema_version": 1,
            "record_type": "active_model_activation",
            "id": activation_id,
            "cycle_id": cycle_id,
            "cycle_snapshot_digest": prior_snapshot_digest,
            "model_level": record["model_level"],
            "model_name": record["model_identity"]["name"],
            "model_revision": result["model_revision"],
            "training_run_id": training["id"],
            "checkpoint_digest": training["checkpoint_digest"],
            "benchmark_evaluation_id": comparison["candidate_evaluation_id"],
            "benchmark_predictions_digest": comparison["candidate_predictions_digest"],
            "supersedes_active_revision_id": previous["id"] if previous else None,
            "reviewed_by": reviewed_by.strip(),
            "reason": reason.strip(),
            "created_at": now,
        }
        activation_relative = (
            Path("training") / "active-learning" / f"activation-{activation_id}.json"
        )
        atomic_write_json(self.project.root / activation_relative, activation)
        activation_artifact = self.artifacts.ingest_file(
            self.project.root / activation_relative,
            media_type="application/vnd.bvmcp.active-model-activation+json",
        )
        old_status = record["status"]
        record["activation"] = {
            **activation,
            "activation_digest": activation_artifact.digest,
        }
        self._advance(record, "PROMOTED", now)
        cycle_relative, cycle_artifact = self._write_snapshot(record)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT status,artifact_digest FROM active_learning_cycles WHERE id=?",
                (cycle_id,),
            ).fetchone()
            if (
                current is None
                or current["status"] != old_status
                or current["artifact_digest"] != prior_snapshot_digest
            ):
                raise RuntimeError("active-learning cycle changed during model activation")
            connection.execute(
                "UPDATE active_model_revisions SET status='SUPERSEDED',updated_at=? "
                "WHERE model_level=? AND model_name=? AND status='ACTIVE'",
                (now, record["model_level"], record["model_identity"]["name"]),
            )
            connection.execute(
                "INSERT INTO active_model_revisions("
                "id,model_level,model_name,model_revision,training_run_id,cycle_id,"
                "checkpoint_digest,benchmark_evaluation_id,status,reviewed_by,reason,"
                "activation_digest,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    activation_id,
                    record["model_level"],
                    record["model_identity"]["name"],
                    result["model_revision"],
                    training["id"],
                    cycle_id,
                    training["checkpoint_digest"],
                    comparison["candidate_evaluation_id"],
                    "ACTIVE",
                    reviewed_by.strip(),
                    reason.strip(),
                    activation_artifact.digest,
                    now,
                    now,
                ),
            )
            connection.execute(
                "UPDATE active_learning_cycles SET status=?,record_json=?,artifact_digest=?,"
                "updated_at=? WHERE id=?",
                (record["status"], json.dumps(record), cycle_artifact.digest, now, cycle_id),
            )
            self._insert_event(
                connection,
                record,
                from_status=old_status,
                snapshot_digest=cycle_artifact.digest,
            )
        return {
            **record,
            "artifact": cycle_artifact.to_dict(),
            "path": str(cycle_relative),
            "activation_artifact": activation_artifact.to_dict(),
            "activation_path": str(activation_relative),
        }

    def get(self, cycle_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT record_json FROM active_learning_cycles WHERE id=?", (cycle_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown active-learning cycle: {cycle_id}")
        return json.loads(row["record_json"])

    def list(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM active_learning_cycles ORDER BY created_at,id"
                )
            ]
        return [self.get(cycle_id) for cycle_id in ids]

    def active_models(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            return [
                dict(row)
                for row in connection.execute(
                    "SELECT * FROM active_model_revisions ORDER BY created_at,id"
                )
            ]

    def rollback(
        self,
        active_revision_id: str,
        *,
        reviewed_by: str,
        reason: str,
    ) -> dict[str, Any]:
        if not reviewed_by.strip() or not reason.strip():
            raise ValueError("model rollback requires a named reviewer and reason")
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT * FROM active_model_rollbacks WHERE rolled_back_revision_id=?",
                (active_revision_id,),
            ).fetchone()
            active = connection.execute(
                "SELECT * FROM active_model_revisions WHERE id=?",
                (active_revision_id,),
            ).fetchone()
        if existing:
            record = dict(existing)
            if (
                record["reviewed_by"] != reviewed_by.strip()
                or record["reason"] != reason.strip()
            ):
                raise ValueError("model revision already has a different rollback receipt")
            receipt = json.loads(
                self.artifacts.path_for(record["receipt_digest"]).read_text(
                    encoding="utf-8"
                )
            )
            return {**receipt, "receipt_digest": record["receipt_digest"], "reused": True}
        if active is None or active["status"] != "ACTIVE":
            raise ValueError("only the currently active model revision can be rolled back")
        cycle = self.get(active["cycle_id"])
        activation = cycle.get("activation") or {}
        restored_id = activation.get("supersedes_active_revision_id")
        restored = None
        if restored_id:
            with self.project.connection() as connection:
                restored = connection.execute(
                    "SELECT * FROM active_model_revisions WHERE id=?",
                    (restored_id,),
                ).fetchone()
            if (
                restored is None
                or restored["status"] != "SUPERSEDED"
                or restored["model_level"] != active["model_level"]
                or restored["model_name"] != active["model_name"]
            ):
                raise ValueError("superseded model revision is not eligible for restoration")
        rollback_id = str(uuid.uuid4())
        now = utc_now()
        receipt = {
            "schema_version": 1,
            "record_type": "active_model_rollback",
            "id": rollback_id,
            "rolled_back_revision": {
                "id": active["id"],
                "model_level": active["model_level"],
                "model_name": active["model_name"],
                "model_revision": active["model_revision"],
                "checkpoint_digest": active["checkpoint_digest"],
                "activation_digest": active["activation_digest"],
            },
            "restored_revision": (
                {
                    "id": restored["id"],
                    "model_level": restored["model_level"],
                    "model_name": restored["model_name"],
                    "model_revision": restored["model_revision"],
                    "checkpoint_digest": restored["checkpoint_digest"],
                    "activation_digest": restored["activation_digest"],
                }
                if restored
                else None
            ),
            "reviewed_by": reviewed_by.strip(),
            "reason": reason.strip(),
            "created_at": now,
        }
        relative = (
            Path("training") / "active-learning" / f"rollback-{rollback_id}.json"
        )
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.active-model-rollback+json",
        )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT status FROM active_model_revisions WHERE id=?",
                (active_revision_id,),
            ).fetchone()
            if current is None or current["status"] != "ACTIVE":
                raise RuntimeError("active model changed during rollback")
            if restored_id:
                previous = connection.execute(
                    "SELECT status FROM active_model_revisions WHERE id=?",
                    (restored_id,),
                ).fetchone()
                if previous is None or previous["status"] != "SUPERSEDED":
                    raise RuntimeError("restored model changed during rollback")
            connection.execute(
                "UPDATE active_model_revisions SET status='ROLLED_BACK',updated_at=? "
                "WHERE id=?",
                (now, active_revision_id),
            )
            if restored_id:
                connection.execute(
                    "UPDATE active_model_revisions SET status='ACTIVE',updated_at=? "
                    "WHERE id=?",
                    (now, restored_id),
                )
            connection.execute(
                "INSERT INTO active_model_rollbacks("
                "id,rolled_back_revision_id,restored_revision_id,reviewed_by,reason,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?)",
                (
                    rollback_id,
                    active_revision_id,
                    restored_id,
                    reviewed_by.strip(),
                    reason.strip(),
                    artifact.digest,
                    now,
                ),
            )
        return {
            **receipt,
            "receipt_digest": artifact.digest,
            "path": str(relative),
            "reused": False,
        }

    def _correction_dataset(
        self, record: dict[str, Any], source_digest: str
    ) -> dict[str, Any]:
        for dataset in DatasetStore(self.project).list():
            manifest = dataset["manifest"]
            if (
                manifest.get("active_learning_cycle_id") == record["id"]
                and manifest.get("source_cycle_snapshot_digest") == source_digest
            ):
                return dataset
        manifest = {
            "schema_version": 1,
            "active_learning_cycle_id": record["id"],
            "source_cycle_snapshot_digest": source_digest,
            "source_predictions_digest": record["predictions_digest"],
            "sample_count": len(record["corrected_samples"]),
            "corrected_samples": record["corrected_samples"],
            "artifact_digests": [source_digest, record["predictions_digest"]],
            "execution": {
                "state": "generated",
                "generated_sample_count": len(record["corrected_samples"]),
                "source": "named_human_corrections",
            },
        }
        created = DatasetStore(self.project).register(
            f"active-learning-corrections-{record['id']}",
            "corrected_feature_samples",
            manifest,
            rights_state="DERIVED_CORRECTIONS_INTERNAL",
            status="generated",
        )
        return DatasetStore(self.project).get(created["id"])

    def _training_run(
        self,
        record: dict[str, Any],
        dataset_id: str,
        backend: str,
        benchmark: dict[str, Any],
        source_digest: str,
        configuration: dict[str, Any],
    ) -> dict[str, Any]:
        for run in TrainingStore(self.project).list():
            config = run["configuration"]
            if (
                run["dataset_id"] == dataset_id
                and run["backend"] == backend
                and config.get("active_learning_cycle_id") == record["id"]
                and config.get("source_cycle_snapshot_digest") == source_digest
            ):
                return run
        return TrainingStore(self.project).plan(
            dataset_id,
            backend=backend,
            configuration={
                **configuration,
                "active_learning_cycle_id": record["id"],
                "source_cycle_snapshot_digest": source_digest,
                "benchmark_dataset_id": benchmark["id"],
                "benchmark_dataset_digest": benchmark["artifact_digest"],
                "model_level": record["model_level"],
                "base_model_identity": record["model_identity"],
            },
        )

    def _evaluation(self, evaluation_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM model_evaluations WHERE id=?", (evaluation_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown model evaluation: {evaluation_id}")
        value = dict(row)
        value["metrics"] = json.loads(value.pop("metrics_json"))
        value["predictions_digest"] = value.pop("predictions_digest")
        return value

    def _snapshot_digest(self, cycle_id: str) -> str:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT artifact_digest FROM active_learning_cycles WHERE id=?", (cycle_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown active-learning cycle: {cycle_id}")
        return str(row["artifact_digest"])

    @staticmethod
    def _advance(record: dict[str, Any], status: str, now: str | None = None) -> None:
        if status not in ALLOWED_TRANSITIONS.get(record.get("status"), set()):
            raise ValueError(f"invalid active-learning transition to {status}")
        record["status"] = status
        record["revision"] = int(record.get("revision", 0)) + 1
        record["updated_at"] = now or utc_now()

    def _write_snapshot(self, record: dict[str, Any]):
        relative = (
            Path("training")
            / "active-learning"
            / f"cycle-{record['id']}-r{record['revision']}.json"
        )
        atomic_write_json(self.project.root / relative, record)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.active-learning-cycle+json",
        )
        return relative, artifact

    def _persist(self, record: dict[str, Any], *, replace: bool = False) -> dict[str, Any]:
        relative, artifact = self._write_snapshot(record)
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            if replace:
                current = connection.execute(
                    "SELECT status,record_json FROM active_learning_cycles WHERE id=?",
                    (record["id"],),
                ).fetchone()
                if current is None:
                    raise KeyError(f"unknown active-learning cycle: {record['id']}")
                previous = json.loads(current["record_json"])
                if int(record["revision"]) != int(previous.get("revision", 0)) + 1:
                    raise RuntimeError("active-learning revision changed during persistence")
                if record["status"] not in ALLOWED_TRANSITIONS.get(current["status"], set()):
                    raise RuntimeError("active-learning status changed during persistence")
                connection.execute(
                    "UPDATE active_learning_cycles SET status=?,record_json=?,artifact_digest=?,"
                    "updated_at=? WHERE id=?",
                    (
                        record["status"],
                        json.dumps(record),
                        artifact.digest,
                        record["updated_at"],
                        record["id"],
                    ),
                )
                from_status = current["status"]
            else:
                if int(record.get("revision", 0)) != 1:
                    raise ValueError("new active-learning cycles must start at revision one")
                connection.execute(
                    "INSERT INTO active_learning_cycles(id,model_level,status,record_json,"
                    "artifact_digest,created_at,updated_at) VALUES(?,?,?,?,?,?,?)",
                    (
                        record["id"],
                        record["model_level"],
                        record["status"],
                        json.dumps(record),
                        artifact.digest,
                        record["created_at"],
                        record["updated_at"],
                    ),
                )
                from_status = None
            self._insert_event(
                connection,
                record,
                from_status=from_status,
                snapshot_digest=artifact.digest,
            )
        return {**record, "artifact": artifact.to_dict(), "path": str(relative)}

    @staticmethod
    def _insert_event(
        connection: Any,
        record: dict[str, Any],
        *,
        from_status: str | None,
        snapshot_digest: str,
    ) -> None:
        connection.execute(
            "INSERT INTO active_learning_events(id,cycle_id,revision,from_status,to_status,"
            "snapshot_digest,created_at) VALUES(?,?,?,?,?,?,?)",
            (
                str(uuid.uuid4()),
                record["id"],
                record["revision"],
                from_status,
                record["status"],
                snapshot_digest,
                record["updated_at"],
            ),
        )


def audit_active_learning(project: ProjectStore) -> dict[str, Any]:
    """Verify immutable cycle snapshots, transition order, and active model activation records."""
    with project.connection() as connection:
        cycle_rows = connection.execute(
            "SELECT * FROM active_learning_cycles ORDER BY created_at,id"
        ).fetchall()
        event_rows = connection.execute(
            "SELECT * FROM active_learning_events ORDER BY cycle_id,revision"
        ).fetchall()
        model_rows = connection.execute(
            "SELECT * FROM active_model_revisions ORDER BY created_at,id"
        ).fetchall()
        rollback_rows = connection.execute(
            "SELECT * FROM active_model_rollbacks ORDER BY created_at,id"
        ).fetchall()
        dataset_rows = connection.execute("SELECT * FROM datasets").fetchall()
        training_rows = connection.execute("SELECT * FROM training_runs").fetchall()
        evaluation_rows = connection.execute("SELECT * FROM model_evaluations").fetchall()
        artifacts = {
            row["digest"]: row["relative_path"]
            for row in connection.execute("SELECT digest,relative_path FROM artifacts")
        }
    events_by_cycle: dict[str, list[dict[str, Any]]] = {}
    for row in event_rows:
        events_by_cycle.setdefault(row["cycle_id"], []).append(dict(row))
    datasets = {
        row["id"]: {
            **dict(row),
            "manifest": json.loads(row["manifest_json"]),
        }
        for row in dataset_rows
    }
    training_runs = {
        row["id"]: {
            **dict(row),
            "configuration": json.loads(row["config_json"]),
            "result": json.loads(row["result_json"]) if row["result_json"] else None,
        }
        for row in training_rows
    }
    evaluations = {
        row["id"]: {**dict(row), "metrics": json.loads(row["metrics_json"])}
        for row in evaluation_rows
    }
    models_by_cycle = {row["cycle_id"]: dict(row) for row in model_rows}
    invalid_cycles = []
    cycle_records = []
    for row in cycle_rows:
        record = json.loads(row["record_json"])
        events = events_by_cycle.get(row["id"], [])
        valid = (
            record.get("id") == row["id"]
            and record.get("model_level") == row["model_level"]
            and record.get("status") == row["status"]
            and _artifact_matches(project, artifacts, row["artifact_digest"], record)
            and len(events) == int(record.get("revision", 0))
        )
        previous = None
        for index, event in enumerate(events, start=1):
            snapshot = _artifact_json(project, artifacts, event["snapshot_digest"])
            valid = bool(
                valid
                and event["revision"] == index
                and event["from_status"] == previous
                and event["to_status"] in ALLOWED_TRANSITIONS.get(previous, set())
                and isinstance(snapshot, dict)
                and snapshot.get("id") == row["id"]
                and snapshot.get("revision") == index
                and snapshot.get("status") == event["to_status"]
            )
            previous = event["to_status"]
        valid = bool(
            valid
            and _cycle_semantics_valid(
                project,
                record,
                events,
                artifacts,
                datasets,
                training_runs,
                evaluations,
                models_by_cycle,
            )
        )
        if not valid:
            invalid_cycles.append(row["id"])
        cycle_records.append({**record, "artifact_digest": row["artifact_digest"], "valid": valid})
    invalid_models = []
    active_keys = set()
    for row in model_rows:
        activation = _artifact_json(project, artifacts, row["activation_digest"])
        key = (row["model_level"], row["model_name"])
        valid = bool(
            isinstance(activation, dict)
            and activation.get("id") == row["id"]
            and activation.get("cycle_id") == row["cycle_id"]
            and activation.get("training_run_id") == row["training_run_id"]
            and activation.get("checkpoint_digest") == row["checkpoint_digest"]
            and activation.get("benchmark_evaluation_id") == row["benchmark_evaluation_id"]
            and activation.get("reviewed_by") == row["reviewed_by"]
            and activation.get("reason") == row["reason"]
        )
        if row["status"] == "ACTIVE":
            if key in active_keys:
                valid = False
            active_keys.add(key)
        if not valid:
            invalid_models.append(row["id"])
    models_by_id = {row["id"]: dict(row) for row in model_rows}
    invalid_rollbacks = []
    for row in rollback_rows:
        record = dict(row)
        receipt = _artifact_json(project, artifacts, row["receipt_digest"])
        rolled = models_by_id.get(row["rolled_back_revision_id"])
        restored = (
            models_by_id.get(row["restored_revision_id"])
            if row["restored_revision_id"]
            else None
        )
        valid = bool(
            isinstance(receipt, dict)
            and receipt.get("record_type") == "active_model_rollback"
            and receipt.get("id") == row["id"]
            and receipt.get("rolled_back_revision", {}).get("id")
            == row["rolled_back_revision_id"]
            and (
                receipt.get("restored_revision", {}).get("id")
                if receipt.get("restored_revision")
                else None
            )
            == row["restored_revision_id"]
            and receipt.get("reviewed_by") == row["reviewed_by"]
            and receipt.get("reason") == row["reason"]
            and rolled is not None
            and rolled["status"] == "ROLLED_BACK"
            and (
                restored is None
                or (
                    restored["model_level"] == rolled["model_level"]
                    and restored["model_name"] == rolled["model_name"]
                    and restored["status"] in {"ACTIVE", "SUPERSEDED", "ROLLED_BACK"}
                )
            )
        )
        if not valid:
            invalid_rollbacks.append(row["id"])
    return {
        "cycles": cycle_records,
        "events": [dict(row) for row in event_rows],
        "active_model_revisions": [dict(row) for row in model_rows],
        "model_rollbacks": [dict(row) for row in rollback_rows],
        "invalid_cycle_ids": invalid_cycles,
        "invalid_model_revision_ids": invalid_models,
        "invalid_rollback_ids": invalid_rollbacks,
    }


def _artifact_json(
    project: ProjectStore, artifacts: dict[str, str], digest: str
) -> dict[str, Any] | None:
    relative = artifacts.get(digest)
    if not relative:
        return None
    path = project.root / relative
    if not path.is_file() or sha256_file(path)[0] != digest:
        return None
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return None
    return value if isinstance(value, dict) else None


def _artifact_matches(
    project: ProjectStore,
    artifacts: dict[str, str],
    digest: str,
    expected: dict[str, Any],
) -> bool:
    return _artifact_json(project, artifacts, digest) == expected


def _cycle_semantics_valid(
    project: ProjectStore,
    record: dict[str, Any],
    events: list[dict[str, Any]],
    artifacts: dict[str, str],
    datasets: dict[str, dict[str, Any]],
    training_runs: dict[str, dict[str, Any]],
    evaluations: dict[str, dict[str, Any]],
    models_by_cycle: dict[str, dict[str, Any]],
) -> bool:
    prediction_document = _artifact_json(project, artifacts, record.get("predictions_digest", ""))
    if not isinstance(prediction_document, dict):
        return False
    predictions = prediction_document.get("predictions")
    if (
        prediction_document.get("cycle_id") != record.get("id")
        or prediction_document.get("model_level") != record.get("model_level")
        or prediction_document.get("model_identity") != record.get("model_identity")
        or not isinstance(predictions, list)
        or len(predictions) != record.get("prediction_count")
    ):
        return False
    ranked = []
    seen = set()
    try:
        for prediction in predictions:
            prediction_id = str(prediction.get("id", "")).strip()
            confidence = float(prediction.get("confidence", -1.0))
            impact = float(prediction.get("impact", -1.0))
            if (
                not prediction_id
                or prediction_id in seen
                or not all(
                    math.isfinite(value) and 0.0 <= value <= 1.0
                    for value in (confidence, impact)
                )
            ):
                return False
            seen.add(prediction_id)
            ranked.append(
                {
                    **prediction,
                    "id": prediction_id,
                    "priority": (1.0 - confidence) * impact,
                    "correction_state": "requested",
                }
            )
    except (TypeError, ValueError):
        return False
    ranked.sort(key=lambda item: (-item["priority"], item["id"]))
    expected_requests = [
        item
        for item in ranked[: int(record.get("correction_budget", 0))]
        if item["priority"] > 0.0
    ]
    if record.get("correction_requests") != expected_requests:
        return False
    status = record.get("status")
    corrections = record.get("corrected_samples")
    if status == "NO_CORRECTIONS_NEEDED":
        return not expected_requests and corrections == []
    if status == "AWAITING_CORRECTIONS":
        return bool(expected_requests) and corrections == []
    if not isinstance(corrections, list) or not corrections:
        return False
    requested_ids = {item["id"] for item in expected_requests}
    correction_ids = [str(item.get("prediction_id", "")) for item in corrections]
    if (
        len(correction_ids) != len(set(correction_ids))
        or not set(correction_ids).issubset(requested_ids)
        or any(not item.get("corrected_by") or not item.get("corrected_at") for item in corrections)
    ):
        return False
    if status == "READY_TO_RETRAIN":
        return record.get("retraining_plan") is None
    plan = record.get("retraining_plan")
    if not isinstance(plan, dict) or len(events) < 3:
        return False
    correction_dataset = datasets.get(plan.get("correction_dataset_id"))
    benchmark = datasets.get(plan.get("benchmark_dataset_id"))
    training = training_runs.get(plan.get("training_run_id"))
    source_snapshot = events[1]["snapshot_digest"]
    if not correction_dataset or not benchmark or not training:
        return False
    correction_manifest = correction_dataset["manifest"]
    if (
        correction_dataset["status"] != "generated"
        or correction_dataset["record_digest"] != plan.get("correction_dataset_digest")
        or correction_manifest.get("active_learning_cycle_id") != record.get("id")
        or correction_manifest.get("source_cycle_snapshot_digest") != source_snapshot
        or correction_manifest.get("source_predictions_digest")
        != record.get("predictions_digest")
        or correction_manifest.get("corrected_samples") != corrections
        or int(correction_manifest.get("sample_count", 0)) != len(corrections)
        or benchmark["record_digest"] != plan.get("benchmark_dataset_digest")
        or training["dataset_id"] != correction_dataset["id"]
        or training["backend"] != plan.get("backend")
        or training["configuration"].get("active_learning_cycle_id") != record.get("id")
        or training["configuration"].get("source_cycle_snapshot_digest") != source_snapshot
        or training["configuration"].get("benchmark_dataset_id") != benchmark["id"]
        or training["configuration"].get("benchmark_dataset_digest")
        != benchmark["record_digest"]
        or plan.get("network_allowed") is not False
        or plan.get("requires_benchmark_comparison") is not True
    ):
        return False
    if status == "RETRAINING_PLANNED":
        return record.get("benchmark_comparison") is None
    comparison = record.get("benchmark_comparison")
    if not isinstance(comparison, dict):
        return False
    baseline = evaluations.get(comparison.get("baseline_evaluation_id"))
    candidate = evaluations.get(comparison.get("candidate_evaluation_id"))
    required = plan.get("required_metrics")
    if not baseline or not candidate or not isinstance(required, list) or not required:
        return False
    if (
        baseline["dataset_id"] != benchmark["id"]
        or candidate["dataset_id"] != benchmark["id"]
        or candidate["training_run_id"] != training["id"]
        or training["status"] != "completed"
        or not training.get("checkpoint_digest")
        or not _artifact_exists(project, artifacts, training["checkpoint_digest"])
        or comparison.get("candidate_checkpoint_digest") != training["checkpoint_digest"]
        or comparison.get("baseline_predictions_digest") != baseline["predictions_digest"]
        or comparison.get("candidate_predictions_digest") != candidate["predictions_digest"]
        or not _evaluation_metrics_valid(project, artifacts, baseline)
        or not _evaluation_metrics_valid(project, artifacts, candidate)
        or int(baseline["metrics"].get("evaluation_sample_count", 0))
        != int(benchmark["manifest"].get("sample_count", 0))
        or int(candidate["metrics"].get("evaluation_sample_count", 0))
        != int(benchmark["manifest"].get("sample_count", 0))
    ):
        return False
    try:
        before = {key: float(baseline["metrics"][key]) for key in required}
        after = {key: float(candidate["metrics"][key]) for key in required}
    except (KeyError, TypeError, ValueError):
        return False
    deltas = {key: after[key] - before[key] for key in required}
    improved = sorted(key for key, value in deltas.items() if value > 1e-12)
    regressed = sorted(key for key, value in deltas.items() if value < -1e-12)
    expected_status = "IMPROVED" if not regressed and improved else "REJECTED"
    if (
        comparison.get("before") != before
        or comparison.get("after") != after
        or comparison.get("deltas") != deltas
        or comparison.get("improved_metrics") != improved
        or comparison.get("regressed_metrics") != regressed
        or status not in {expected_status, "PROMOTED"}
        or (status == "PROMOTED" and expected_status != "IMPROVED")
    ):
        return False
    if status != "PROMOTED":
        return record.get("activation") is None
    model = models_by_cycle.get(record["id"])
    activation = record.get("activation")
    return bool(
        model
        and training.get("result", {}).get("commercial_eligible") is True
        and isinstance(activation, dict)
        and activation.get("id") == model["id"]
        and activation.get("activation_digest") == model["activation_digest"]
        and activation.get("checkpoint_digest") == model["checkpoint_digest"]
        and activation.get("benchmark_evaluation_id") == model["benchmark_evaluation_id"]
        and activation.get("reviewed_by") == model["reviewed_by"]
        and activation.get("reason") == model["reason"]
    )


def _evaluation_metrics_valid(
    project: ProjectStore, artifacts: dict[str, str], evaluation: dict[str, Any]
) -> bool:
    document = _artifact_json(project, artifacts, evaluation.get("predictions_digest", ""))
    if not isinstance(document, dict):
        return False
    counts = document.get("counts")
    if not isinstance(counts, dict):
        return False
    try:
        true_positive = int(counts.get("true_positive", 0))
        false_positive = int(counts.get("false_positive", 0))
        false_negative = int(counts.get("false_negative", 0))
        intersection = float(counts.get("mask_intersection", 0.0))
        union = float(counts.get("mask_union", 0.0))
        precision = true_positive / max(1, true_positive + false_positive)
        recall = true_positive / max(1, true_positive + false_negative)
        expected = {
            "precision": precision,
            "recall": recall,
            "f1": 2.0 * precision * recall / max(1e-12, precision + recall),
            "mask_iou": intersection / max(1.0, union),
            "evaluation_sample_count": int(document.get("sample_count", 0)),
        }
    except (TypeError, ValueError, ZeroDivisionError):
        return False
    return all(evaluation["metrics"].get(key) == value for key, value in expected.items())


def _artifact_exists(
    project: ProjectStore, artifacts: dict[str, str], digest: str
) -> bool:
    relative = artifacts.get(digest)
    if not relative:
        return False
    path = project.root / relative
    return path.is_file() and sha256_file(path)[0] == digest
