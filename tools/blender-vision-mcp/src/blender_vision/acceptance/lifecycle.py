from __future__ import annotations

import json
import math
from typing import Any

from blender_vision.acceptance.transactions import REQUIRED_GATE_CATEGORIES
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import sha256_file
from blender_vision.projects.store import ProjectStore


def _recompute_evaluation_decision(
    gates: Any, metrics: Any, baseline_scene_id: str | None
) -> dict[str, Any] | None:
    if not isinstance(gates, list) or not isinstance(metrics, dict):
        return None
    categories: set[str] = set()
    regressions: list[dict[str, Any]] = []
    improvements: list[dict[str, Any]] = []
    comparable_count = 0
    failed: list[str] = []
    for gate in gates:
        if not isinstance(gate, dict):
            return None
        name = str(gate.get("name", "")).strip()
        category = str(gate.get("category", "")).strip().lower()
        status = str(gate.get("status", "")).strip().upper()
        mandatory = bool(gate.get("mandatory", True))
        if (
            not name
            or category not in REQUIRED_GATE_CATEGORIES
            or status not in {"PASS", "FAIL", "BLOCKED"}
        ):
            return None
        categories.add(category)
        if mandatory and status != "PASS":
            failed.append(name)
        candidate_value = gate.get("candidate_value")
        baseline_value = gate.get("baseline_value")
        if candidate_value is None or baseline_value is None:
            continue
        try:
            candidate_number = float(candidate_value)
            baseline_number = float(baseline_value)
            tolerance = max(0.0, float(gate.get("regression_tolerance", 0.0)))
        except (TypeError, ValueError):
            return None
        finite_values = (candidate_number, baseline_number, tolerance)
        if not all(math.isfinite(value) for value in finite_values):
            return None
        higher_is_better = bool(gate.get("higher_is_better", True))
        regressed = (
            candidate_number < baseline_number - tolerance
            if higher_is_better
            else candidate_number > baseline_number + tolerance
        )
        improved = (
            candidate_number > baseline_number + tolerance
            if higher_is_better
            else candidate_number < baseline_number - tolerance
        )
        if gate.get("regressed") is not regressed:
            return None
        if mandatory:
            comparable_count += 1
            if regressed:
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
            if improved:
                improvements.append(
                    {
                        "gate": name,
                        "category": category,
                        "baseline_value": baseline_number,
                        "candidate_value": candidate_number,
                    }
                )
    missing = sorted(REQUIRED_GATE_CATEGORIES - categories)
    aggregate_improvement = None
    if baseline_scene_id:
        if comparable_count:
            aggregate_improvement = bool(improvements)
        else:
            declared = metrics.get("aggregate_improvement")
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
    return {
        "status": status,
        "missing_gate_categories": missing,
        "failed_gates": failed,
        "regressions": regressions,
        "improvements": improvements,
        "aggregate_improvement": aggregate_improvement,
    }


def audit_scene_lifecycle(project: ProjectStore) -> dict[str, Any]:
    """Verify candidate evaluations and every lifecycle edge from stored receipts."""
    with project.connection() as connection:
        scenes = [
            dict(row)
            for row in connection.execute(
                "SELECT id,state,is_authoritative,artifact_digest,created_at "
                "FROM scene_assets ORDER BY created_at,id"
            )
        ]
        transitions = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM scene_transitions ORDER BY created_at,id"
            )
        ]
        evaluations = [
            dict(row)
            for row in connection.execute(
                "SELECT * FROM candidate_evaluations ORDER BY created_at,id"
            )
        ]
        artifacts = {
            row["digest"]: dict(row)
            for row in connection.execute("SELECT digest,size FROM artifacts")
        }

    artifact_store = ArtifactStore(project)
    scene_by_id = {scene["id"]: scene for scene in scenes}

    def read_verified_artifact(digest: Any) -> dict[str, Any] | None:
        if not isinstance(digest, str) or digest not in artifacts:
            return None
        try:
            path = artifact_store.path_for(digest)
            actual_digest, actual_size = sha256_file(path)
            if actual_digest != digest or actual_size != int(artifacts[digest]["size"]):
                return None
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (FileNotFoundError, json.JSONDecodeError, OSError, TypeError, ValueError):
            return None
        return payload if isinstance(payload, dict) else None

    verified_evaluations: dict[str, dict[str, Any]] = {}
    invalid_evaluation_ids: list[str] = []
    for evaluation in evaluations:
        scene = scene_by_id.get(evaluation["scene_id"])
        payload = read_verified_artifact(evaluation["receipt_digest"])
        try:
            gates = json.loads(evaluation["gates_json"])
            metrics = json.loads(evaluation["metrics_json"])
            regressions = json.loads(evaluation["regressions_json"])
        except (json.JSONDecodeError, TypeError):
            gates, metrics, regressions = None, None, None
        decision = _recompute_evaluation_decision(
            gates, metrics, evaluation["baseline_scene_id"]
        )
        valid = bool(
            scene
            and payload
            and decision
            and payload.get("receipt_type") == "candidate_evaluation_transaction"
            and payload.get("id") == evaluation["id"]
            and payload.get("scene_id") == evaluation["scene_id"]
            and payload.get("scene_artifact_digest") == scene["artifact_digest"]
            and payload.get("baseline_scene_id") == evaluation["baseline_scene_id"]
            and payload.get("status") == evaluation["status"]
            and evaluation["status"] == decision["status"]
            and payload.get("required_gate_categories")
            == sorted(REQUIRED_GATE_CATEGORIES)
            and payload.get("missing_gate_categories")
            == decision["missing_gate_categories"]
            and payload.get("failed_gates") == decision["failed_gates"]
            and payload.get("gates") == gates
            and payload.get("metrics") == metrics
            and payload.get("regressions") == regressions == decision["regressions"]
            and payload.get("improvements") == decision["improvements"]
            and payload.get("aggregate_improvement")
            == decision["aggregate_improvement"]
            and payload.get("created_at") == evaluation["created_at"]
        )
        if valid:
            verified_evaluations[evaluation["id"]] = evaluation
        else:
            invalid_evaluation_ids.append(evaluation["id"])

    verified_transition_ids: set[str] = set()
    invalid_transition_ids: list[str] = []
    for transition in transitions:
        scene = scene_by_id.get(transition["scene_id"])
        payload = read_verified_artifact(transition["receipt_digest"])
        valid = bool(
            scene
            and payload
            and payload.get("receipt_type") == "scene_lifecycle_transition"
            and payload.get("id") == transition["id"]
            and payload.get("scene_id") == transition["scene_id"]
            and payload.get("scene_artifact_digest") == scene["artifact_digest"]
            and payload.get("from_state") == transition["from_state"]
            and payload.get("to_state") == transition["to_state"]
            and payload.get("reviewer") == transition["reviewer"]
            and payload.get("reason") == transition["reason"]
            and payload.get("evaluation_id") == transition["evaluation_id"]
            and payload.get("created_at") == transition["created_at"]
        )
        evaluation = verified_evaluations.get(transition["evaluation_id"])
        if valid and transition["to_state"] in {"ACCEPTED", "PROMOTED"}:
            valid = bool(
                evaluation
                and evaluation["scene_id"] == transition["scene_id"]
                and evaluation["status"] == "PASSED"
            )
        elif valid and transition["to_state"] == "REJECTED" and transition["evaluation_id"]:
            valid = bool(
                evaluation
                and evaluation["scene_id"] == transition["scene_id"]
                and evaluation["status"] == "FAILED"
            )
        elif valid and transition["to_state"] == "SUPERSEDED":
            superseding_scene_id = payload.get("superseded_by_scene_id") if payload else None
            valid = bool(
                superseding_scene_id
                and superseding_scene_id != transition["scene_id"]
                and evaluation
                and evaluation["scene_id"] == superseding_scene_id
                and evaluation["status"] == "PASSED"
            )
        if valid:
            verified_transition_ids.add(transition["id"])
        else:
            invalid_transition_ids.append(transition["id"])

    transitions_by_scene: dict[str, list[dict[str, Any]]] = {}
    for transition in transitions:
        transitions_by_scene.setdefault(transition["scene_id"], []).append(transition)

    def complete_chain(scene: dict[str, Any]) -> bool:
        chain = transitions_by_scene.get(scene["id"], [])
        if not chain or any(item["id"] not in verified_transition_ids for item in chain):
            return False
        state = chain[0]["from_state"]
        if state not in {"DRAFT", "CANDIDATE"}:
            return False
        for transition in chain:
            if transition["from_state"] != state:
                return False
            state = transition["to_state"]
        if state != scene["state"]:
            return False
        accepted = next((item for item in chain if item["to_state"] == "ACCEPTED"), None)
        promoted = next((item for item in chain if item["to_state"] == "PROMOTED"), None)
        return bool(
            accepted
            and promoted
            and accepted["evaluation_id"] == promoted["evaluation_id"]
            and accepted["evaluation_id"] in verified_evaluations
        )

    authoritative = [scene for scene in scenes if scene["is_authoritative"]]
    authoritative_scene = authoritative[0] if len(authoritative) == 1 else None
    unreceipted_superseded_scene_ids = [
        scene["id"]
        for scene in scenes
        if scene["state"] == "SUPERSEDED"
        and not any(
            transition["to_state"] == "SUPERSEDED"
            and transition["id"] in verified_transition_ids
            for transition in transitions_by_scene.get(scene["id"], [])
        )
    ]
    return {
        "authoritative_scene_count": len(authoritative),
        "authoritative_scene_id": authoritative_scene["id"] if authoritative_scene else None,
        "authoritative_promotion_chain_valid": bool(
            authoritative_scene
            and authoritative_scene["state"] == "PROMOTED"
            and complete_chain(authoritative_scene)
        ),
        "verified_evaluation_count": len(verified_evaluations),
        "verified_passed_evaluation_ids": sorted(
            evaluation_id
            for evaluation_id, evaluation in verified_evaluations.items()
            if evaluation["status"] == "PASSED"
        ),
        "invalid_evaluation_ids": invalid_evaluation_ids,
        "verified_transition_count": len(verified_transition_ids),
        "invalid_transition_ids": invalid_transition_ids,
        "unreceipted_superseded_scene_ids": unreceipted_superseded_scene_ids,
        "valid": bool(
            len(authoritative) <= 1
            and not invalid_evaluation_ids
            and not invalid_transition_ids
            and not unreceipted_superseded_scene_ids
        ),
    }
