from __future__ import annotations

from typing import Any

from blender_vision.intelligence.active_learning import ActiveLearningStore
from blender_vision.perception.workspace import PerceptionWorkspace
from blender_vision.projects.store import ProjectStore


class PerceptionLearningService:
    """Turn persisted perception uncertainty into governed correction work."""

    def __init__(self, project: ProjectStore):
        self.project = project

    def start_from_workspace(
        self,
        workspace_id: str,
        *,
        model_level: str,
        model_identity: dict[str, Any],
        correction_budget: int = 32,
    ) -> dict[str, Any]:
        workspace = PerceptionWorkspace(self.project).get(workspace_id)
        predictions = []
        for contradiction in workspace["contradictions"]:
            predictions.append(
                {
                    "id": f"contradiction:{contradiction['id']}",
                    "kind": contradiction["kind"],
                    "confidence": min(
                        0.99, max(0.0, float(contradiction["confidence"]) * 0.5)
                    ),
                    "impact": 1.0,
                    "workspace_id": workspace_id,
                    "evidence_references": contradiction["evidence_references"],
                    "proposed_next_action": contradiction["next_action"],
                }
            )
        for finding in workspace["findings"]:
            if not finding["missing_observations"]:
                continue
            predictions.append(
                {
                    "id": f"finding:{finding['id']}",
                    "kind": "MISSING_OBSERVATION",
                    "confidence": min(0.99, max(0.0, float(finding["confidence"]))),
                    "impact": min(
                        1.0,
                        0.25
                        + float(finding["predicted_information_gain"]) / 4.0,
                    ),
                    "workspace_id": workspace_id,
                    "specialist": finding["specialist"],
                    "evidence_references": finding["evidence_references"],
                    "missing_observations": finding["missing_observations"],
                    "proposed_next_action": finding["proposed_next_action"],
                }
            )
        if not predictions:
            predictions.append(
                {
                    "id": f"workspace:{workspace_id}:verified",
                    "kind": "NO_CORRECTION_NEEDED",
                    "confidence": 1.0,
                    "impact": 0.0,
                    "workspace_id": workspace_id,
                    "evidence_references": [
                        {"artifact_digest": workspace["artifact_digest"]}
                    ],
                }
            )
        cycle = ActiveLearningStore(self.project).start(
            model_level=model_level,
            model_identity=model_identity,
            predictions=predictions,
            correction_budget=correction_budget,
        )
        return {
            **cycle,
            "source_workspace_id": workspace_id,
            "source_workspace_digest": workspace["artifact_digest"],
        }
