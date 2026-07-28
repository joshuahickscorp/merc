from __future__ import annotations

import json
from typing import Any

from blender_vision.backends.generative3d import GenerativeProposalStore
from blender_vision.core.errors import BackendUnavailable, EvidenceUnavailable, ProjectError
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.pipeline import GeometryPipeline
from blender_vision.visual.oracle import VisualOracleStore


class PortfolioExecutor:
    """Execute cheap local lanes and truthfully retain unavailable hypotheses."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.store = ReconstructionPortfolioStore(project)

    def execute_initial(self, portfolio_id: str) -> dict[str, Any]:
        candidates = {item["lane"]: item for item in self.store.list_candidates(portfolio_id)}
        results: list[dict[str, Any]] = []
        pipeline = GeometryPipeline(self.project)
        with self.project.connection() as connection:
            portfolio_row = connection.execute(
                "SELECT configuration_json FROM reconstruction_portfolios WHERE id=?",
                (portfolio_id,),
            ).fetchone()
            if portfolio_row is None:
                raise KeyError(f"unknown reconstruction portfolio: {portfolio_id}")
            portfolio_configuration = json.loads(portfolio_row["configuration_json"])
            colmap = connection.execute(
                "SELECT id FROM camera_solutions WHERE backend='colmap' "
                "ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
            semantic_rows = connection.execute(
                "SELECT record_json FROM semantic_nodes"
            ).fetchall()
        classical = candidates.get("classical_photogrammetry")
        if classical and classical["status"] != "EVALUATED":
            if colmap:
                run = pipeline.import_colmap_solution(
                    colmap["id"], configuration={"portfolio_id": portfolio_id}
                )
                results.append(
                    self.store.record_result(
                        classical["id"],
                        metrics={
                            "camera": 0.75,
                            "coverage": min(
                                1.0,
                                len(run["evidence"].get("camera_extrinsics", [])) / 12.0,
                            ),
                            "silhouette": 0.0,
                        },
                        artifacts=self._artifact_digests(run),
                        geometry_run_id=run["id"],
                    )
                )
            else:
                results.append(
                    self.store.record_unavailable(
                        classical["id"],
                        status="BLOCKED_BACKEND",
                        reason="no governed COLMAP solution is available",
                        requirements=["two or more overlapping decodable views", "COLMAP"],
                    )
                )
        learned = candidates.get("learned_multiview_geometry")
        learned_configuration = portfolio_configuration.get("backends", {}).get(
            "learned_multiview_geometry"
        )
        if learned and learned["status"] == "PROPOSED" and learned_configuration:
            backend = str(learned_configuration.get("backend", "vggt-commercial"))
            if backend not in {"vggt-commercial", "vggt-original-research"}:
                raise ValueError("learned portfolio lane requires an explicit VGGT backend")
            try:
                run = pipeline.run(backend, learned_configuration)
            except BackendUnavailable as error:
                results.append(
                    self.store.record_unavailable(
                        learned["id"],
                        status="BLOCKED_BACKEND",
                        reason=str(error),
                        requirements=[
                            "operator-approved local VGGT checkpoint",
                            "compatible offline VGGT runtime",
                        ],
                    )
                )
            else:
                camera_count = len(run["evidence"].get("camera_extrinsics", []))
                results.append(
                    self.store.record_result(
                        learned["id"],
                        metrics={
                            "camera": 0.5 if camera_count else 0.0,
                            "coverage": min(1.0, camera_count / 12.0),
                            "silhouette": 0.0,
                            "learned_confidence": float(
                                run["evidence"].get("uncertainty", {}).get(
                                    "depth_confidence_mean", 0.0
                                )
                            ),
                        },
                        artifacts=self._artifact_digests(run),
                        geometry_run_id=run["id"],
                        evidence_authority="observed_initialization",
                        acceptance_eligible=bool(run["commercial_eligible"]),
                    )
                )
        generative = candidates.get("generative_image_to_3d")
        generative_configuration = portfolio_configuration.get("backends", {}).get(
            "generative_image_to_3d"
        )
        if generative and generative["status"] == "PROPOSED" and generative_configuration:
            request_id = str(generative_configuration.get("request_id", ""))
            try:
                proposal = GenerativeProposalStore(self.project).get_result(request_id)
            except (KeyError, ProjectError) as error:
                results.append(
                    self.store.record_unavailable(
                        generative["id"],
                        status="BLOCKED_BACKEND",
                        reason=str(error),
                        requirements=["completed hash-bound generative request result"],
                    )
                )
            else:
                artifacts = sorted(
                    {
                        *proposal.get("mesh_digests", []),
                        *proposal.get("texture_digests", []),
                        *proposal.get("image_digests", []),
                        *proposal.get("pbr_channels", {}).values(),
                        proposal["record_digest"],
                    }
                )
                results.append(
                    self.store.record_result(
                        generative["id"],
                        metrics={
                            "camera": 0.0,
                            "coverage": 0.0,
                            "silhouette": 0.0,
                            "appearance": float(bool(proposal.get("texture_digests"))),
                            "generative_confidence": float(proposal["confidence"]),
                        },
                        artifacts=artifacts,
                        evidence_authority="synthetic_hypothesis",
                        acceptance_eligible=False,
                    )
                )
        gaussian = candidates.get("gaussian_visual_oracle")
        gaussian_configuration = portfolio_configuration.get("backends", {}).get(
            "gaussian_visual_oracle"
        )
        if gaussian and gaussian["status"] == "PROPOSED" and gaussian_configuration:
            oracle_id = str(gaussian_configuration.get("oracle_id", ""))
            try:
                oracle = VisualOracleStore(self.project).get(oracle_id)
            except KeyError as error:
                results.append(
                    self.store.record_unavailable(
                        gaussian["id"],
                        status="BLOCKED_BACKEND",
                        reason=str(error),
                        requirements=["registered camera-bound Gaussian visual oracle"],
                    )
                )
            else:
                results.append(
                    self.store.record_result(
                        gaussian["id"],
                        metrics={
                            "camera": 0.0,
                            "coverage": 0.0,
                            "silhouette": 0.0,
                            "appearance": 1.0,
                        },
                        artifacts=[oracle["artifact_digest"]],
                        evidence_authority="appearance_oracle_only",
                        acceptance_eligible=False,
                    )
                )
        visual = candidates.get("visual_hull")
        if visual and visual["status"] != "EVALUATED":
            try:
                run = pipeline.run(
                    "visual_hull", {"portfolio_id": portfolio_id}
                )
            except EvidenceUnavailable as error:
                run = pipeline.run(
                    "silhouette", {"solve_cameras": False, "portfolio_id": portfolio_id}
                )
                mask_count = len(run["evidence"].get("mask_artifacts", []))
                results.append(
                    self.store.record_progress(
                        visual["id"],
                        status="EVIDENCE_READY",
                        reason=(
                            f"silhouette hypotheses are ready but governed visual-hull carving "
                            f"cannot run: {error}"
                        ),
                        metrics={
                            "camera": 0.5 if run["evidence"].get("camera_extrinsics") else 0.0,
                            "coverage": min(1.0, mask_count / 12.0),
                            "silhouette": 0.0,
                        },
                        artifacts=self._artifact_digests(run),
                        geometry_run_id=run["id"],
                    )
                )
            else:
                mask_count = len(run["evidence"].get("mask_artifacts", []))
                results.append(
                    self.store.record_result(
                        visual["id"],
                        metrics={
                            "camera": (
                                0.75
                                if run["evidence"]["diagnostics"].get(
                                    "camera_solution_approved"
                                )
                                else 0.5
                            ),
                            "coverage": min(1.0, mask_count / 6.0),
                            "silhouette": 0.0,
                        },
                        artifacts=self._artifact_digests(run),
                        geometry_run_id=run["id"],
                    )
                )
        parametric = candidates.get("parametric_category_model")
        hybrid = candidates.get("hybrid_semantic_reconstruction")
        if hybrid and parametric and (
            hybrid["status"] != "EVALUATED"
            or hybrid.get("scene_id") != parametric.get("scene_id")
        ):
            if parametric["status"] == "EVALUATED" and parametric.get("scene_id"):
                nodes = [json.loads(row["record_json"]) for row in semantic_rows]
                component_nodes = [item for item in nodes if item["type"] != "digital_twin_root"]
                bound = [item for item in component_nodes if item.get("geometry")]
                results.append(
                    self.store.record_result(
                        hybrid["id"],
                        metrics={
                            **parametric.get("metrics", {}),
                            "semantic_editability": 1.0,
                            "semantic_coverage": len(bound) / max(1, len(component_nodes)),
                        },
                        artifacts=parametric.get("artifacts", []),
                        scene_id=parametric["scene_id"],
                    )
                )
            else:
                results.append(
                    self.store.record_unavailable(
                        hybrid["id"],
                        status="BLOCKED_BACKEND",
                        reason="hybrid lane awaits an evaluated editable parametric candidate",
                        requirements=["parametric_category_model result"],
                    )
                )
        metadata = self.project.project().get("metadata", {})
        existing = candidates.get("existing_model_repair")
        if existing and existing["status"] == "PROPOSED" and not metadata.get(
            "private_starting_model", True
        ):
            results.append(
                self.store.record_unavailable(
                    existing["id"],
                    status="NOT_APPLICABLE",
                    reason="benchmark explicitly prohibits a private starting model",
                )
            )
        for lane, requirements in {
            "learned_multiview_geometry": ["configured licensed learned-geometry worker"],
            "generative_image_to_3d": ["configured licensed generative 3D worker"],
            "gaussian_visual_oracle": ["configured Gaussian reconstruction worker"],
        }.items():
            candidate = candidates.get(lane)
            if candidate:
                candidate = self.store.get_candidate(candidate["id"])
            if candidate and candidate["status"] == "PROPOSED":
                results.append(
                    self.store.record_unavailable(
                        candidate["id"],
                        status="BLOCKED_BACKEND",
                        reason="optional worker is not configured in this project",
                        requirements=requirements,
                    )
                )
        return {
            "portfolio_id": portfolio_id,
            "results": results,
            "ranking": self.store.rank(portfolio_id),
            "fusion_plan": self.store.fusion_plan(portfolio_id),
            "acceptance_performed": False,
        }

    @staticmethod
    def _artifact_digests(run: dict[str, Any]) -> list[str]:
        evidence = run.get("evidence", {})
        return sorted(
            {
                item
                for key, values in evidence.items()
                if key.endswith("_artifacts") and isinstance(values, list)
                for item in values
                if isinstance(item, str)
            }
        )
