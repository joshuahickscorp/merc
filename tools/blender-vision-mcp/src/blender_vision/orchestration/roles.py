from __future__ import annotations

import json
import math
import uuid
from typing import Any

from blender_vision.core.util import utc_now
from blender_vision.orchestration.campaigns import AGENT_ROLES, CampaignStore
from blender_vision.projects.store import ProjectStore

ROLE_KEYWORDS = {
    "Evidence Researcher": {"source", "evidence", "rights", "search", "provenance"},
    "Variant Resolver": {"variant", "identity", "configuration", "target"},
    "Capture Planner": {"capture", "view", "coverage", "calibration", "landmark"},
    "Camera Analyst": {"camera", "pose", "intrinsics", "pnp", "colmap", "reprojection"},
    "Geometry Analyst": {"geometry", "silhouette", "depth", "topology", "mesh"},
    "Component Modeler": {"component", "parametric", "semantic", "edit"},
    "Material Analyst": {"material", "appearance", "texture", "lighting"},
    "Optimization Planner": {"optimize", "fit", "candidate", "improvement", "cost"},
    "Adversarial Reviewer": {"adversarial", "regression", "contradiction", "overclaim"},
    "Acceptance Auditor": {"acceptance", "receipt", "gate", "promote", "delivery"},
}


class RoleTaskStore:
    """Persistent advisory handoffs for specialized reasoning roles."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def assign(
        self,
        campaign_id: str,
        objective: str,
        *,
        confidence: float,
        estimated_cost: float,
        inputs: dict[str, Any],
        role: str | None = None,
    ) -> dict[str, Any]:
        CampaignStore(self.project).get(campaign_id)
        if not objective.strip():
            raise ValueError("role task requires an objective")
        if not math.isfinite(confidence) or not 0.0 <= confidence <= 1.0:
            raise ValueError("role task confidence must be between zero and one")
        if not math.isfinite(estimated_cost) or estimated_cost < 0:
            raise ValueError("role task cost must be finite and non-negative")
        selected_role = role or self._select_role(objective)
        if selected_role not in AGENT_ROLES:
            raise ValueError("role task uses an unsupported role")
        with self.project.connection() as connection:
            existing = connection.execute(
                "SELECT id FROM role_tasks WHERE campaign_id=? AND role=? AND objective=? "
                "AND status IN ('ASSIGNED','WAITING_INPUT','COMPLETED') "
                "ORDER BY created_at LIMIT 1",
                (campaign_id, selected_role, objective.strip()),
            ).fetchone()
        if existing:
            return self.get(existing["id"])
        task_id = str(uuid.uuid4())
        now = utc_now()
        priority = (1.0 - confidence) / (1.0 + estimated_cost)
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO role_tasks(id,campaign_id,role,objective,status,priority,"
                "estimated_cost,confidence,inputs_json,output_json,created_at,updated_at) "
                "VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
                (
                    task_id,
                    campaign_id,
                    selected_role,
                    objective.strip(),
                    "ASSIGNED",
                    priority,
                    estimated_cost,
                    confidence,
                    json.dumps(inputs),
                    None,
                    now,
                    now,
                ),
            )
        return self.get(task_id)

    def set_waiting(self, task_id: str, *, reason: str) -> dict[str, Any]:
        if not reason.strip():
            raise ValueError("waiting role task requires a reason")
        task = self.get(task_id)
        output = {"waiting_reason": reason.strip(), "authority": "advisory_only"}
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE role_tasks SET status='WAITING_INPUT',output_json=?,updated_at=? "
                "WHERE id=?",
                (json.dumps(output), utc_now(), task["id"]),
            )
        return self.get(task_id)

    def complete(
        self,
        task_id: str,
        *,
        output: dict[str, Any],
        artifact_digests: list[str],
        completed_by: str,
    ) -> dict[str, Any]:
        if not completed_by.strip():
            raise ValueError("role task completion requires a named actor")
        task = self.get(task_id)
        if task["status"] not in {"ASSIGNED", "WAITING_INPUT"}:
            raise ValueError("role task is not open")
        with self.project.connection() as connection:
            artifacts = {row[0] for row in connection.execute("SELECT digest FROM artifacts")}
        if not set(artifact_digests).issubset(artifacts):
            raise ValueError("role task output references unknown artifacts")
        record = {
            **output,
            "artifact_digests": sorted(set(artifact_digests)),
            "completed_by": completed_by.strip(),
            "completed_at": utc_now(),
            "authority": "advisory_only; cannot accept or promote a scene",
        }
        with self.project.connection() as connection:
            connection.execute(
                "UPDATE role_tasks SET status='COMPLETED',output_json=?,updated_at=? WHERE id=?",
                (json.dumps(record), record["completed_at"], task_id),
            )
        return self.get(task_id)

    def ensure_metric_camera_boundary(
        self, campaign_id: str, *, evidence: dict[str, Any]
    ) -> list[dict[str, Any]]:
        specifications = [
            (
                "Camera Analyst",
                "Recover and independently review metric camera poses for all eligible views",
                0.35,
                1.0,
            ),
            (
                "Capture Planner",
                "Plan reviewed non-coplanar landmarks or a calibration-board capture",
                0.2,
                0.35,
            ),
            (
                "Adversarial Reviewer",
                "Verify that relative COLMAP scale is not mislabeled as metric authority",
                0.8,
                0.15,
            ),
            (
                "Acceptance Auditor",
                "Audit that L3 acceptance remains blocked until metric camera review passes",
                0.9,
                0.1,
            ),
        ]
        return [
            self.assign(
                campaign_id,
                objective,
                role=role,
                confidence=confidence,
                estimated_cost=cost,
                inputs={"boundary": "metric_camera", "evidence": evidence},
            )
            for role, objective, confidence, cost in specifications
        ]

    def ensure_multiview_fit_boundary(
        self, campaign_id: str, *, evidence: dict[str, Any]
    ) -> list[dict[str, Any]]:
        specifications = [
            (
                "Geometry Analyst",
                "Diagnose component-local fixed-camera residuals across all affected views",
                0.55,
                0.35,
            ),
            (
                "Component Modeler",
                "Generate bounded semantic parameter candidates for multiview evaluation",
                0.45,
                0.6,
            ),
            (
                "Optimization Planner",
                "Bind candidate losses to locality-scoped comparison artifacts and rank them",
                0.65,
                0.4,
            ),
            (
                "Adversarial Reviewer",
                "Reject any claimed multiview improvement not reproduced from stored residuals",
                0.9,
                0.15,
            ),
        ]
        return [
            self.assign(
                campaign_id,
                objective,
                role=role,
                confidence=confidence,
                estimated_cost=cost,
                inputs={"boundary": "multiview_component_fit", "evidence": evidence},
            )
            for role, objective, confidence, cost in specifications
        ]

    def ensure_candidate_evaluation_boundary(
        self, campaign_id: str, *, evidence: dict[str, Any]
    ) -> list[dict[str, Any]]:
        specifications = [
            (
                "Geometry Analyst",
                "Reconcile the candidate geometry, dimensions, components, and topology gates",
                0.75,
                0.25,
            ),
            (
                "Material Analyst",
                "Reconcile governed material and appearance evidence for candidate evaluation",
                0.7,
                0.2,
            ),
            (
                "Adversarial Reviewer",
                "Challenge every mandatory candidate gate and identify aggregate regressions",
                0.85,
                0.2,
            ),
            (
                "Acceptance Auditor",
                "Authorize one seven-category atomic candidate transaction from verified evidence",
                0.9,
                0.15,
            ),
        ]
        return [
            self.assign(
                campaign_id,
                objective,
                role=role,
                confidence=confidence,
                estimated_cost=cost,
                inputs={"boundary": "candidate_evaluation", "evidence": evidence},
            )
            for role, objective, confidence, cost in specifications
        ]

    def ensure_safe_promotion_boundary(
        self, campaign_id: str, *, evidence: dict[str, Any]
    ) -> list[dict[str, Any]]:
        specifications = [
            (
                "Adversarial Reviewer",
                "Verify the passed transaction, lifecycle chain, and absence of regressions",
                0.9,
                0.1,
            ),
            (
                "Acceptance Auditor",
                "Authorize acceptance and promotion using the verified transaction receipt",
                0.95,
                0.1,
            ),
        ]
        return [
            self.assign(
                campaign_id,
                objective,
                role=role,
                confidence=confidence,
                estimated_cost=cost,
                inputs={"boundary": "safe_promotion", "evidence": evidence},
            )
            for role, objective, confidence, cost in specifications
        ]

    def get(self, task_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute("SELECT * FROM role_tasks WHERE id=?", (task_id,)).fetchone()
        if row is None:
            raise KeyError(f"unknown role task: {task_id}")
        value = dict(row)
        value["inputs"] = json.loads(value.pop("inputs_json"))
        output_json = value.pop("output_json")
        value["output"] = json.loads(output_json) if output_json else None
        value["authority"] = "advisory_only"
        return value

    def list(self, campaign_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id FROM role_tasks "
                + ("WHERE campaign_id=? " if campaign_id else "")
                + "ORDER BY priority DESC,created_at,id",
                (campaign_id,) if campaign_id else (),
            ).fetchall()
        return [self.get(row["id"]) for row in rows]

    @staticmethod
    def _select_role(objective: str) -> str:
        words = set(objective.lower().replace("-", " ").split())
        return max(
            ROLE_KEYWORDS,
            key=lambda role: (len(words & ROLE_KEYWORDS[role]), role == "Optimization Planner"),
        )
