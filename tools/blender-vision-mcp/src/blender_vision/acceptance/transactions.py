from __future__ import annotations

import json
import math
import uuid
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import SceneLifecycleState
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore

REQUIRED_GATE_CATEGORIES = {
    "camera",
    "measurement",
    "component",
    "topology",
    "material",
    "appearance",
    "provenance",
}


class CandidateTransactionStore:
    """Atomic policy decision over an already-computed mandatory evidence bundle."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.scenes = SceneStore(project)

    def evaluate(
        self,
        scene_id: str,
        *,
        gates: list[dict[str, Any]],
        baseline_scene_id: str | None = None,
        metrics: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        scene = self.scenes.get(scene_id)
        if scene["state"] != SceneLifecycleState.CANDIDATE.value:
            raise ValueError("only a CANDIDATE scene can enter transactional evaluation")
        if baseline_scene_id:
            baseline = self.scenes.get(baseline_scene_id)
            if baseline["state"] not in {
                SceneLifecycleState.ACCEPTED.value,
                SceneLifecycleState.PROMOTED.value,
            }:
                raise ValueError("evaluation baseline must be ACCEPTED or PROMOTED")
        normalized: list[dict[str, Any]] = []
        categories: set[str] = set()
        regressions: list[dict[str, Any]] = []
        improvements: list[dict[str, Any]] = []
        for index, gate in enumerate(gates):
            if not isinstance(gate, dict):
                raise ValueError("candidate gates must be objects")
            name = str(gate.get("name", "")).strip()
            category = str(gate.get("category", "")).strip().lower()
            status = str(gate.get("status", "")).strip().upper()
            if not name or category not in REQUIRED_GATE_CATEGORIES:
                raise ValueError(f"gate {index} requires a name and supported category")
            if status not in {"PASS", "FAIL", "BLOCKED"}:
                raise ValueError(f"gate {name} requires PASS, FAIL, or BLOCKED status")
            mandatory = bool(gate.get("mandatory", True))
            record = {
                **gate,
                "name": name,
                "category": category,
                "status": status,
                "mandatory": mandatory,
            }
            categories.add(category)
            candidate_value = gate.get("candidate_value")
            baseline_value = gate.get("baseline_value")
            if candidate_value is not None and baseline_value is not None:
                candidate_number, baseline_number = float(candidate_value), float(baseline_value)
                if not math.isfinite(candidate_number) or not math.isfinite(baseline_number):
                    raise ValueError(f"gate {name} metric values must be finite")
                tolerance = max(0.0, float(gate.get("regression_tolerance", 0.0)))
                higher_is_better = bool(gate.get("higher_is_better", True))
                regressed = (
                    candidate_number < baseline_number - tolerance
                    if higher_is_better
                    else candidate_number > baseline_number + tolerance
                )
                record["regressed"] = regressed
                if mandatory and regressed:
                    regressions.append(
                        {
                            "gate": name,
                            "category": category,
                            "baseline_value": baseline_number,
                            "candidate_value": candidate_number,
                            "regression_tolerance": tolerance,
                            "higher_is_better": higher_is_better,
                        }
                    )
                improved = (
                    candidate_number > baseline_number + tolerance
                    if higher_is_better
                    else candidate_number < baseline_number - tolerance
                )
                if mandatory and improved:
                    improvements.append(
                        {
                            "gate": name,
                            "category": category,
                            "baseline_value": baseline_number,
                            "candidate_value": candidate_number,
                        }
                    )
            normalized.append(record)
        missing = sorted(REQUIRED_GATE_CATEGORIES - categories)
        failed = [
            gate["name"] for gate in normalized if gate["mandatory"] and gate["status"] != "PASS"
        ]
        aggregate_improvement = None
        if baseline_scene_id:
            comparable = [
                gate
                for gate in normalized
                if gate["mandatory"]
                and gate.get("candidate_value") is not None
                and gate.get("baseline_value") is not None
            ]
            if comparable:
                aggregate_improvement = bool(improvements)
            else:
                declared = (metrics or {}).get("aggregate_improvement")
                aggregate_improvement = bool(
                    isinstance(declared, (int, float))
                    and math.isfinite(float(declared))
                    and float(declared) > 0.0
                )
        status = (
            "PASSED"
            if not missing
            and not failed
            and not regressions
            and aggregate_improvement is not False
            else "FAILED"
        )
        evaluation_id = str(uuid.uuid4())
        created_at = utc_now()
        receipt = {
            "schema_version": 1,
            "receipt_type": "candidate_evaluation_transaction",
            "id": evaluation_id,
            "scene_id": scene_id,
            "scene_artifact_digest": scene["artifact_digest"],
            "baseline_scene_id": baseline_scene_id,
            "status": status,
            "required_gate_categories": sorted(REQUIRED_GATE_CATEGORIES),
            "missing_gate_categories": missing,
            "failed_gates": failed,
            "regressions": regressions,
            "improvements": improvements,
            "aggregate_improvement": aggregate_improvement,
            "gates": normalized,
            "metrics": metrics or {},
            "created_at": created_at,
        }
        receipt_path = self.project.root / "receipts" / f"candidate-evaluation-{evaluation_id}.json"
        atomic_write_json(receipt_path, receipt)
        artifact = self.artifacts.ingest_file(
            receipt_path, media_type="application/vnd.bvmcp.candidate-evaluation+json"
        )
        rejection_result = None
        if status == "FAILED":
            reasons = []
            if missing:
                reasons.append(f"missing mandatory categories: {', '.join(missing)}")
            if failed:
                reasons.append(f"failed mandatory gates: {', '.join(failed)}")
            if regressions:
                reasons.append("one or more mandatory metrics regressed")
            if aggregate_improvement is False:
                reasons.append("candidate provides no aggregate improvement over baseline")
            rejection_result = self.scenes._create_transition_receipt(
                scene=scene,
                source=SceneLifecycleState.CANDIDATE,
                destination=SceneLifecycleState.REJECTED,
                reviewer="VisionMCP transactional policy",
                reason="; ".join(reasons),
                evaluation_id=evaluation_id,
            )
        with self.project.connection() as connection:
            connection.execute("BEGIN IMMEDIATE")
            current = connection.execute(
                "SELECT state FROM scene_assets WHERE id=?", (scene_id,)
            ).fetchone()
            if current is None or current["state"] != SceneLifecycleState.CANDIDATE.value:
                raise RuntimeError("candidate state changed during evaluation preparation")
            connection.execute(
                "INSERT INTO candidate_evaluations(id,scene_id,baseline_scene_id,status,gates_json,"
                "metrics_json,regressions_json,receipt_digest,created_at) "
                "VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    evaluation_id,
                    scene_id,
                    baseline_scene_id,
                    status,
                    json.dumps(normalized),
                    json.dumps(metrics or {}),
                    json.dumps(regressions),
                    artifact.digest,
                    created_at,
                ),
            )
            if rejection_result:
                connection.execute(
                    "UPDATE scene_assets SET state=? WHERE id=?",
                    (SceneLifecycleState.REJECTED.value, scene_id),
                )
                self.scenes._insert_transition(connection, rejection_result)
        rejection = None
        if rejection_result:
            rejection = {
                **rejection_result["receipt_payload"],
                "receipt": rejection_result["artifact"].to_dict(),
                "path": rejection_result["path"],
                "superseded_transitions": [],
            }
        return {
            **receipt,
            "receipt": artifact.to_dict(),
            "path": str(receipt_path.relative_to(self.project.root)),
            "automatic_rejection": rejection,
        }

    def get(self, evaluation_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM candidate_evaluations WHERE id=?", (evaluation_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown candidate evaluation: {evaluation_id}")
        value = dict(row)
        value["gates"] = json.loads(value.pop("gates_json"))
        value["metrics"] = json.loads(value.pop("metrics_json"))
        value["regressions"] = json.loads(value.pop("regressions_json"))
        return value

    def list(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            if scene_id:
                rows = connection.execute(
                    "SELECT id FROM candidate_evaluations WHERE scene_id=? ORDER BY created_at,id",
                    (scene_id,),
                ).fetchall()
            else:
                rows = connection.execute(
                    "SELECT id FROM candidate_evaluations ORDER BY created_at,id"
                ).fetchall()
        return [self.get(row["id"]) for row in rows]
