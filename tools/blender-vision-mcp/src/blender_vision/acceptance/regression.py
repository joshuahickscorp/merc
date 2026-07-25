from __future__ import annotations

import json
import math
import uuid
from pathlib import Path
from typing import Any

from blender_vision.acceptance.transactions import CandidateTransactionStore
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.models import SceneLifecycleState
from blender_vision.core.util import atomic_write_json, sha256_file, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore


class FixedCameraRegressionEvaluator:
    """Reject a regressing candidate from paired, artifact-bound fixed-view evidence.

    This lane is deliberately rejection-only. Exact camera reuse can prove that a candidate
    became worse without implying that the legacy camera set is approved, metric, or sufficient
    for acceptance.
    """

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.scenes = SceneStore(project)

    def evaluate(
        self,
        *,
        baseline_scene_id: str,
        candidate_scene_id: str,
        metric: str = "silhouette_iou",
        minimum_views: int = 2,
        regression_tolerance: float = 0.0,
    ) -> dict[str, Any]:
        if metric != "silhouette_iou":
            raise ValueError("fixed-camera regression currently supports silhouette_iou")
        if minimum_views < 2:
            raise ValueError("fixed-camera regression requires at least two views")
        if not math.isfinite(regression_tolerance) or regression_tolerance < 0.0:
            raise ValueError("regression tolerance must be finite and non-negative")
        baseline = self.scenes.get(baseline_scene_id)
        candidate = self.scenes.get(candidate_scene_id)
        if candidate["state"] not in {
            SceneLifecycleState.DRAFT.value,
            SceneLifecycleState.CANDIDATE.value,
        }:
            raise ValueError("regression evaluator requires a DRAFT or CANDIDATE scene")
        self._verify_artifact(baseline["artifact_digest"], baseline["relative_path"])
        self._verify_artifact(candidate["artifact_digest"], candidate["relative_path"])

        baseline_views = self._latest_views(baseline_scene_id, metric)
        candidate_views = self._latest_views(candidate_scene_id, metric)
        if set(baseline_views) != set(candidate_views):
            raise ValueError("baseline and candidate must cover the exact same references")
        if len(baseline_views) < minimum_views:
            raise ValueError("fixed-camera regression has insufficient paired views")

        pairs = []
        for reference_id in sorted(baseline_views):
            before, after = baseline_views[reference_id], candidate_views[reference_id]
            if before["camera_solution_id"] != after["camera_solution_id"]:
                raise ValueError(
                    f"camera solution changed for paired reference: {reference_id}"
                )
            if before["partial_object"] or after["partial_object"]:
                raise ValueError("partial-object crops cannot enter aggregate regression")
            pairs.append(
                {
                    "reference_id": reference_id,
                    "camera_solution_id": before["camera_solution_id"],
                    "baseline_comparison_id": before["comparison_id"],
                    "candidate_comparison_id": after["comparison_id"],
                    "baseline_render_digest": before["render_digest"],
                    "candidate_render_digest": after["render_digest"],
                    "baseline_residual_digest": before["residual_digest"],
                    "candidate_residual_digest": after["residual_digest"],
                    "baseline_value": before["value"],
                    "candidate_value": after["value"],
                    "delta": after["value"] - before["value"],
                }
            )
        baseline_mean = sum(item["baseline_value"] for item in pairs) / len(pairs)
        candidate_mean = sum(item["candidate_value"] for item in pairs) / len(pairs)
        delta = candidate_mean - baseline_mean
        if candidate_mean >= baseline_mean - regression_tolerance:
            raise ValueError("candidate does not regress beyond the configured tolerance")

        report_id = str(uuid.uuid4())
        report = {
            "schema_version": 1,
            "id": report_id,
            "kind": "fixed_camera_regression_evidence",
            "baseline_scene_id": baseline_scene_id,
            "baseline_scene_state": baseline["state"],
            "baseline_scene_digest": baseline["artifact_digest"],
            "candidate_scene_id": candidate_scene_id,
            "candidate_scene_digest": candidate["artifact_digest"],
            "metric": metric,
            "view_count": len(pairs),
            "baseline_mean": baseline_mean,
            "candidate_mean": candidate_mean,
            "aggregate_delta": delta,
            "regression_tolerance": regression_tolerance,
            "regressed_view_count": sum(item["delta"] < 0.0 for item in pairs),
            "paired_views": pairs,
            "authority": (
                "rejection_only; exact camera reuse does not approve or metrically upgrade "
                "the camera solutions"
            ),
            "created_at": utc_now(),
        }
        relative = Path("comparisons") / f"fixed-camera-regression-{report_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.fixed-camera-regression+json",
        )
        if candidate["state"] == SceneLifecycleState.DRAFT.value:
            self.scenes.transition(
                candidate_scene_id,
                SceneLifecycleState.CANDIDATE,
                reviewer="VisionMCP fixed-camera regression policy",
                reason=(
                    "Artifact-bound paired-view evidence is ready for rejection-only "
                    "transactional evaluation"
                ),
            )
        baseline_for_transaction = (
            baseline_scene_id
            if baseline["state"]
            in {SceneLifecycleState.ACCEPTED.value, SceneLifecycleState.PROMOTED.value}
            else None
        )
        transaction = CandidateTransactionStore(self.project).evaluate(
            candidate_scene_id,
            baseline_scene_id=baseline_for_transaction,
            gates=[
                self._blocked_gate(
                    "camera",
                    "fixed camera identity; approval independent",
                    "Exact solution IDs were reused, but camera approval is not inferred.",
                    artifact.digest,
                ),
                self._blocked_gate(
                    "measurement",
                    "dimensional acceptance not evaluated",
                    "Regression-only evaluation does not approve dimensions.",
                    artifact.digest,
                ),
                self._blocked_gate(
                    "component",
                    "component acceptance not evaluated",
                    "Regression-only evaluation does not approve component completeness.",
                    artifact.digest,
                ),
                self._blocked_gate(
                    "topology",
                    "topology acceptance not evaluated",
                    "Regression-only evaluation does not approve topology.",
                    artifact.digest,
                ),
                self._blocked_gate(
                    "material",
                    "material acceptance not evaluated",
                    "Regression-only evaluation does not approve materials.",
                    artifact.digest,
                ),
                {
                    "category": "appearance",
                    "name": "aggregate fixed-camera silhouette regression",
                    "status": "FAIL",
                    "mandatory": True,
                    "candidate_value": candidate_mean,
                    "baseline_value": baseline_mean,
                    "higher_is_better": True,
                    "regression_tolerance": regression_tolerance,
                    "evidence": {
                        "report_digest": artifact.digest,
                        "report_path": str(relative),
                        "paired_comparison_ids": [
                            item["candidate_comparison_id"] for item in pairs
                        ],
                    },
                },
                {
                    "category": "provenance",
                    "name": "artifact-bound regression provenance",
                    "status": "PASS",
                    "mandatory": True,
                    "evidence": {
                        "report_digest": artifact.digest,
                        "scene_digests": [
                            baseline["artifact_digest"],
                            candidate["artifact_digest"],
                        ],
                    },
                },
            ],
            metrics={
                "evaluation_authority": "rejection_only",
                "diagnostic_baseline_scene_id": baseline_scene_id,
                "diagnostic_baseline_state": baseline["state"],
                "aggregate_regression": delta,
                "regressed_view_count": report["regressed_view_count"],
                "view_count": len(pairs),
                "evidence_report_digest": artifact.digest,
            },
        )
        if transaction["status"] != "FAILED" or not transaction["automatic_rejection"]:
            raise RuntimeError("regression-only transaction did not reject the candidate")
        return {
            "report": report,
            "artifact": artifact.to_dict(),
            "path": str(relative),
            "transaction": transaction,
            "accepted": False,
            "candidate_state": self.scenes.get(candidate_scene_id)["state"],
        }

    def _latest_views(self, scene_id: str, metric: str) -> dict[str, dict[str, Any]]:
        with self.project.connection() as connection:
            render_rows = connection.execute(
                "SELECT id,camera_solution_id,outputs_json FROM render_runs WHERE scene_id=?",
                (scene_id,),
            ).fetchall()
            comparison_rows = connection.execute(
                "SELECT c.*,r.acceptance_eligible FROM comparisons c "
                "JOIN reference_items r ON r.id=c.reference_id ORDER BY c.created_at,c.id"
            ).fetchall()
        render_by_digest: dict[str, dict[str, Any]] = {}
        for row in render_rows:
            for output in json.loads(row["outputs_json"]):
                digests = set(output.get("pass_artifact_digests", {}).values())
                artifact = output.get("artifact") or {}
                if artifact.get("digest"):
                    digests.add(artifact["digest"])
                for digest in digests:
                    if digest:
                        render_by_digest[digest] = {
                            "render_run_id": row["id"],
                            "camera_solution_id": row["camera_solution_id"],
                        }
        latest: dict[str, dict[str, Any]] = {}
        for row in comparison_rows:
            render = render_by_digest.get(row["render_digest"])
            if render is None:
                continue
            metrics = json.loads(row["metrics_json"])
            value = metrics.get(metric)
            if not isinstance(value, (int, float)) or not math.isfinite(float(value)):
                continue
            if not bool(row["acceptance_eligible"]):
                continue
            self._verify_artifact(row["render_digest"])
            self._verify_artifact(row["residual_digest"])
            latest[row["reference_id"]] = {
                **render,
                "comparison_id": row["id"],
                "render_digest": row["render_digest"],
                "residual_digest": row["residual_digest"],
                "value": float(value),
                "partial_object": bool(metrics.get("reference_partial_object_crop", False)),
            }
        return latest

    def _verify_artifact(self, digest: str, relative_path: str | None = None) -> None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT size,relative_path FROM artifacts WHERE digest=?", (digest,)
            ).fetchone()
        if row is None:
            raise ValueError(f"regression evidence references an unknown artifact: {digest}")
        paths = [row["relative_path"]]
        if relative_path is not None:
            paths.append(relative_path)
        root = self.project.root.resolve()
        for raw in paths:
            path = (root / raw).resolve()
            try:
                path.relative_to(root)
            except ValueError as error:
                raise ValueError("regression evidence path escapes the project") from error
            if not path.is_file():
                raise ValueError(f"regression evidence artifact is missing: {digest}")
            actual, size = sha256_file(path)
            if actual != digest or (raw == row["relative_path"] and size != row["size"]):
                raise ValueError(f"regression evidence artifact failed hash verification: {digest}")

    @staticmethod
    def _blocked_gate(
        category: str, name: str, reason: str, report_digest: str
    ) -> dict[str, Any]:
        return {
            "category": category,
            "name": name,
            "status": "BLOCKED",
            "mandatory": True,
            "reason": reason,
            "evidence": {"regression_report_digest": report_digest},
        }
