from __future__ import annotations

import hashlib
import json
import tempfile
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.passes import MAXIMAL_VISUAL_RENDER_PASSES
from blender_vision.cameras.decisions import CameraDecisionStore
from blender_vision.cameras.state import validate_complete_camera_state
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.visual_geometry.metrics import (
    PROJECTION_ENGINE_V1,
    PROJECTION_ENGINE_V2,
    edge_structure_metrics,
    perceptual_diagnostic,
    projection_metrics,
)


class VisualGeometryStore:
    """Receipt-bound fixed rigs and replayable visual-geometry scorecards."""

    SCHEMA_VERSION = 1
    PROJECTION_THRESHOLDS = {
        "silhouette_iou_minimum": 0.95,
        "silhouette_dice_minimum": 0.97,
        "foreground_precision_minimum": 0.95,
        "foreground_recall_minimum": 0.95,
        "boundary_rmse_fraction_of_diagonal_maximum": 0.015,
    }

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def create_rig(
        self,
        *,
        scene_id: str,
        camera_solution_id: str,
        maximum_dimension: int = 1024,
    ) -> dict[str, Any]:
        if isinstance(maximum_dimension, bool) or maximum_dimension < 64:
            raise ValueError("fixed rig maximum_dimension must be at least 64")
        scene = SceneStore(self.project).get(scene_id)
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM camera_solutions WHERE id=?", (camera_solution_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown camera solution: {camera_solution_id}")
        camera_document = json.loads(row["solution_json"])
        cameras = (
            camera_document
            if isinstance(camera_document, list)
            else camera_document.get("cameras", [])
        )
        if not cameras:
            raise ValueError("fixed visual-geometry rig requires at least one camera")
        for camera in cameras:
            validate_complete_camera_state(camera)
        decision = CameraDecisionStore(self.project).verify(camera_solution_id)
        authoritative = bool(
            decision["valid"] and decision["state"] == "approved" and row["approved"]
        )
        config = {
            "schema_version": self.SCHEMA_VERSION,
            "camera_solution_id": camera_solution_id,
            "camera_solution_snapshot_sha256": hashlib.sha256(
                canonical_json(camera_document)
            ).hexdigest(),
            "camera_immutable_sha256s": [str(camera["immutable_sha256"]) for camera in cameras],
            "reference_ids": sorted(str(camera["reference_id"]) for camera in cameras),
            "scene_id": scene_id,
            "scene_artifact_digest": scene["artifact_digest"],
            "maximum_dimension": int(maximum_dimension),
            "object_transform_policy": "SCENE_AUTHORED_IMMUTABLE",
            "ground_plane_policy": "DISABLED_UNLESS_PRESENT_IN_SCENE",
            "validation_lighting": {
                "review_lighting": True,
                "review_exposure": -0.5,
                "grazing_directions": ["left", "right", "top"],
            },
            "render_state": {
                "framing_authority": "IMMUTABLE_EXACT_CAMERA_STATE",
                "scene_bounds_framing": False,
                "color_management": "STANDARD_GOVERNED_WORKER_STATE",
                "crop_policy": "FULL_REFERENCE_FRAME",
            },
            "required_passes": sorted(MAXIMAL_VISUAL_RENDER_PASSES),
            "projection_thresholds": self.PROJECTION_THRESHOLDS,
        }
        config_digest = hashlib.sha256(canonical_json(config)).hexdigest()
        rig_id = str(uuid.uuid4())
        created_at = utc_now()
        state = "AUTHORITATIVE" if authoritative else "DIAGNOSTIC_PROPOSAL"
        receipt = {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_geometry_fixed_rig",
            "id": rig_id,
            "scene_id": scene_id,
            "scene_artifact_digest": scene["artifact_digest"],
            "camera_solution_id": camera_solution_id,
            "camera_solution_snapshot_sha256": config["camera_solution_snapshot_sha256"],
            "camera_decision_valid": bool(decision["valid"]),
            "camera_decision_state": decision["state"],
            "state": state,
            "config": config,
            "config_digest": config_digest,
            "authority": (
                "FIXED_REVIEWED_CAMERA_RIG"
                if authoritative
                else "FIXED_DIAGNOSTIC_RIG_NO_APPROVAL_AUTHORITY"
            ),
            "created_at": created_at,
        }
        relative = Path("receipts") / f"visual-geometry-rig-{rig_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-geometry-rig+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_geometry_rigs"
                "(id,scene_id,camera_solution_id,state,config_json,config_digest,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    rig_id,
                    scene_id,
                    camera_solution_id,
                    state,
                    json.dumps(config),
                    config_digest,
                    artifact.digest,
                    created_at,
                ),
            )
        return {
            "id": rig_id,
            "state": state,
            "configuration": config,
            "receipt": receipt,
            "receipt_digest": artifact.digest,
        }

    def evaluate(
        self,
        *,
        rig_id: str,
        reference_id: str,
        render_run_id: str,
        mask_id: str | None = None,
        mask_proposal_id: str | None = None,
    ) -> dict[str, Any]:
        if mask_id and mask_proposal_id:
            raise ValueError("choose an approved mask or a proposal, not both")
        rig = self._rig(rig_id)
        rig_verification = self.verify_rig(rig_id)
        if not rig_verification["valid"]:
            raise ValueError("visual-geometry rig receipt is invalid")
        render_run = self._render_run(render_run_id)
        if (
            render_run["scene_id"] != rig["scene_id"]
            or render_run["camera_solution_id"] != rig["camera_solution_id"]
        ):
            raise ValueError("render run is not bound to the fixed rig scene and camera")
        output = next(
            (value for value in render_run["outputs"] if value["reference_id"] == reference_id),
            None,
        )
        if output is None:
            raise ValueError("render run does not contain the requested reference")
        if reference_id not in rig["config"]["reference_ids"]:
            raise ValueError("reference is not covered by the fixed rig")
        passes = output.get("pass_artifact_digests", {})
        if not isinstance(passes, dict):
            raise ValueError("render output pass ledger is malformed")
        invalid_passes = sorted(
            name for name, digest in passes.items() if not self._artifact_valid(digest)
        )
        if invalid_passes:
            raise ValueError(f"render passes are missing or corrupt: {invalid_passes}")
        reference = self._reference(reference_id)
        if not self._artifact_valid(reference["artifact_digest"]):
            raise ValueError("reference artifact is missing or corrupt")
        mask = self._resolve_mask(
            reference_id,
            mask_id=mask_id,
            mask_proposal_id=mask_proposal_id,
        )
        silhouette_digest = passes.get("silhouette")
        if not silhouette_digest:
            raise ValueError("visual-geometry evaluation requires a silhouette pass")

        with tempfile.TemporaryDirectory(prefix="bvmcp-visual-geometry-") as temporary:
            temporary_root = Path(temporary)
            residual_path = temporary_root / "projection-residual.png"
            edge_overlay_path = temporary_root / "edge-overlay.png"
            projection = projection_metrics(
                self.artifacts.path_for(mask["artifact_digest"]),
                self.artifacts.path_for(silhouette_digest),
                residual_path=residual_path,
                engine=PROJECTION_ENGINE_V2,
            )
            neutral_digest = passes.get("neutral_clay") or passes.get("material_neutral")
            edges = (
                edge_structure_metrics(
                    self.artifacts.path_for(reference["artifact_digest"]),
                    self.artifacts.path_for(neutral_digest),
                    object_mask_path=self.artifacts.path_for(mask["artifact_digest"]),
                    overlay_path=edge_overlay_path,
                )
                if neutral_digest
                else {
                    "status": "NOT_EVALUATED",
                    "reason": "neutral-clay pass is absent",
                    "authority": "NO_EDGE_EVIDENCE",
                }
            )
            perceptual = (
                perceptual_diagnostic(
                    self.artifacts.path_for(reference["artifact_digest"]),
                    self.artifacts.path_for(passes["beauty"]),
                )
                if passes.get("beauty")
                else {
                    "status": "NOT_EVALUATED",
                    "reason": "beauty pass is absent",
                    "authority": "NO_PERCEPTUAL_EVIDENCE",
                }
            )
            residual_artifact = self.artifacts.ingest_file(residual_path, media_type="image/png")
            edge_artifact = (
                self.artifacts.ingest_file(edge_overlay_path, media_type="image/png")
                if edge_overlay_path.is_file()
                else None
            )

        required_passes = set(rig["config"]["required_passes"])
        missing_passes = sorted(required_passes - set(passes))
        projection_gates = self._projection_gates(projection)
        component_records = output.get("component_ids", {})
        component_values = {
            str(name): (
                value.get("component_id") if isinstance(value, dict) else None
            )
            for name, value in component_records.items()
        }
        assigned_component_ids = sorted(
            {
                str(value)
                for value in component_values.values()
                if value not in {None, "", "UNASSIGNED"}
            }
        )
        unassigned_component_objects = sorted(
            str(name)
            for name, value in component_values.items()
            if value in {None, "", "UNASSIGNED"}
        )
        local_geometry = {
            "status": "NOT_EVALUATED",
            "component_id_pass_present": "component_id" in passes,
            "rendered_object_count": len(component_records),
            "assigned_component_count": len(assigned_component_ids),
            "assigned_component_ids": assigned_component_ids,
            "unassigned_object_count": len(unassigned_component_objects),
            "unassigned_objects": unassigned_component_objects,
            "reviewed_component_mask_count": 0,
            "reason": ("no reviewed per-component reference masks are bound to this scorecard"),
            "authority": "GEOMETRY_UNVERIFIED",
        }
        unavailable_ground_truth = {
            name: {
                "status": "NOT_EVALUATED",
                "render_pass_present": pass_name in passes,
                "reason": f"no governed {name} reference evidence is registered",
                "authority": "REFERENCE_EVIDENCE_UNAVAILABLE",
            }
            for name, pass_name in {
                "depth": "depth",
                "world_normals": "world_normal",
                "geometric_normals": "geometric_normal",
                "curvature": "curvature",
                "landmarks": "feature_id",
            }.items()
        }
        causes = self._attribute_causes(
            rig_state=rig["state"],
            mask_authority=mask["authority"],
            projection_gates=projection_gates,
            missing_passes=missing_passes,
            local_status=local_geometry["status"],
        )
        authoritative_inputs = bool(
            rig["state"] == "AUTHORITATIVE" and mask["authority"] == "REVIEWED_MASK"
        )
        if not authoritative_inputs:
            status = "DIAGNOSTIC_ONLY"
        elif missing_passes or not all(projection_gates.values()):
            status = "FAIL"
        elif local_geometry["status"] != "PASS":
            status = "BLOCKED"
        else:
            status = "PASS"
        scorecard_id = str(uuid.uuid4())
        created_at = utc_now()
        scorecard = {
            "schema_version": self.SCHEMA_VERSION,
            "id": scorecard_id,
            "rig_id": rig_id,
            "scene_id": rig["scene_id"],
            "reference_id": reference_id,
            "render_run_id": render_run_id,
            "status": status,
            "projection_metric_engine": PROJECTION_ENGINE_V2,
            "authority": (
                "VISUAL_GEOMETRY_ACCEPTANCE_EVIDENCE"
                if status == "PASS"
                else "DIAGNOSTIC_OR_INCOMPLETE_VISUAL_GEOMETRY_EVIDENCE"
            ),
            "inputs": {
                "reference_artifact_digest": reference["artifact_digest"],
                "mask_id": mask["id"],
                "mask_artifact_digest": mask["artifact_digest"],
                "mask_authority": mask["authority"],
                "silhouette_artifact_digest": silhouette_digest,
                "neutral_artifact_digest": neutral_digest,
                "beauty_artifact_digest": passes.get("beauty"),
                "pass_artifact_digests": dict(sorted(passes.items())),
            },
            "pass_coverage": {
                "required": sorted(required_passes),
                "present": sorted(passes),
                "missing": missing_passes,
                "complete": not missing_passes,
            },
            "projection": {"metrics": projection, "gates": projection_gates},
            "edge_structure": edges,
            "local_geometry": local_geometry,
            "ground_truth_layers": unavailable_ground_truth,
            "perceptual": perceptual,
            "cause_attribution": causes,
            "artifacts": {
                "projection_residual_digest": residual_artifact.digest,
                "edge_overlay_digest": edge_artifact.digest if edge_artifact else None,
            },
            "acceptance_notes": [
                "visual truth leads; test counts are not a fidelity metric",
                "unclassified image edges and perceptual metrics are supplemental only",
                "missing rear or local evidence is reported, never inferred",
            ],
            "created_at": created_at,
        }
        receipt = self._scorecard_receipt(scorecard)
        relative = Path("receipts") / f"visual-geometry-scorecard-{scorecard_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        receipt_artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-geometry-scorecard+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_geometry_scorecards"
                "(id,rig_id,scene_id,reference_id,render_run_id,status,scorecard_json,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    scorecard_id,
                    rig_id,
                    rig["scene_id"],
                    reference_id,
                    render_run_id,
                    status,
                    json.dumps(scorecard),
                    receipt_artifact.digest,
                    created_at,
                ),
            )
        return {
            **scorecard,
            "receipt": receipt,
            "receipt_digest": receipt_artifact.digest,
        }

    def list_rigs(self) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM visual_geometry_rigs ORDER BY created_at,id"
            ).fetchall()
        return [self._normalize_rig(row) for row in rows]

    def list_scorecards(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM visual_geometry_scorecards "
                + ("WHERE scene_id=? " if scene_id else "")
                + "ORDER BY created_at,id",
                (scene_id,) if scene_id else (),
            ).fetchall()
        return [self._normalize_scorecard(row) for row in rows]

    def verify_rig(self, rig_id: str) -> dict[str, Any]:
        try:
            rig = self._rig(rig_id)
            if not self._artifact_valid(rig["receipt_digest"]):
                return {"valid": False}
            receipt = json.loads(
                self.artifacts.path_for(rig["receipt_digest"]).read_text(encoding="utf-8")
            )
            scene = SceneStore(self.project).get(rig["scene_id"])
            config_digest = hashlib.sha256(canonical_json(rig["config"])).hexdigest()
            expected = {
                "schema_version": self.SCHEMA_VERSION,
                "receipt_type": "visual_geometry_fixed_rig",
                "id": rig["id"],
                "scene_id": rig["scene_id"],
                "scene_artifact_digest": scene["artifact_digest"],
                "camera_solution_id": rig["camera_solution_id"],
                "camera_solution_snapshot_sha256": rig["config"]["camera_solution_snapshot_sha256"],
                "camera_decision_valid": receipt.get("camera_decision_valid"),
                "camera_decision_state": receipt.get("camera_decision_state"),
                "state": rig["state"],
                "config": rig["config"],
                "config_digest": config_digest,
                "authority": (
                    "FIXED_REVIEWED_CAMERA_RIG"
                    if rig["state"] == "AUTHORITATIVE"
                    else "FIXED_DIAGNOSTIC_RIG_NO_APPROVAL_AUTHORITY"
                ),
                "created_at": rig["created_at"],
            }
            valid = bool(
                canonical_json(receipt) == canonical_json(expected)
                and rig["config_digest"] == config_digest
                and scene["artifact_digest"] == rig["config"]["scene_artifact_digest"]
                and self._camera_snapshot_valid(rig)
            )
            if rig["state"] == "AUTHORITATIVE":
                decision = CameraDecisionStore(self.project).verify(rig["camera_solution_id"])
                valid = bool(valid and decision["valid"] and decision["state"] == "approved")
            return {"valid": valid, "receipt": receipt}
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False}

    def verify_scorecard(self, scorecard_id: str, *, replay: bool = True) -> dict[str, Any]:
        try:
            row = self._scorecard(scorecard_id)
            if not self._artifact_valid(row["receipt_digest"]):
                return {"valid": False, "receipt_valid": False, "replay_valid": False}
            receipt = json.loads(
                self.artifacts.path_for(row["receipt_digest"]).read_text(encoding="utf-8")
            )
            receipt_valid = canonical_json(receipt) == canonical_json(
                self._scorecard_receipt(row["scorecard"])
            )
            inputs = row["scorecard"]["inputs"]
            artifacts = row["scorecard"]["artifacts"]
            digests = [
                inputs["reference_artifact_digest"],
                inputs["mask_artifact_digest"],
                inputs["silhouette_artifact_digest"],
                artifacts["projection_residual_digest"],
                *inputs["pass_artifact_digests"].values(),
            ]
            if artifacts.get("edge_overlay_digest"):
                digests.append(artifacts["edge_overlay_digest"])
            receipt_valid = bool(
                receipt_valid
                and row["status"] == row["scorecard"]["status"]
                and all(self._artifact_valid(digest) for digest in digests)
                and self.verify_rig(row["rig_id"])["valid"]
            )
            replay_valid = receipt_valid
            if receipt_valid and replay:
                replayed = projection_metrics(
                    self.artifacts.path_for(inputs["mask_artifact_digest"]),
                    self.artifacts.path_for(inputs["silhouette_artifact_digest"]),
                    engine=row["scorecard"].get(
                        "projection_metric_engine", PROJECTION_ENGINE_V1
                    ),
                )
                replay_valid = canonical_json(replayed) == canonical_json(
                    row["scorecard"]["projection"]["metrics"]
                )
            return {
                "valid": bool(receipt_valid and replay_valid),
                "receipt_valid": receipt_valid,
                "replay_valid": replay_valid,
                "receipt": receipt,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "receipt_valid": False, "replay_valid": False}

    def _resolve_mask(
        self,
        reference_id: str,
        *,
        mask_id: str | None,
        mask_proposal_id: str | None,
    ) -> dict[str, Any]:
        masks = ReferenceMaskStore(self.project)
        if mask_proposal_id:
            proposal = masks.get_proposal(mask_proposal_id)
            if proposal["reference_id"] != reference_id:
                raise ValueError("mask proposal belongs to a different reference")
            verification = masks.verify_proposal(mask_proposal_id)
            if not verification["valid"]:
                raise ValueError("mask proposal is invalid")
            return {
                "id": mask_proposal_id,
                "artifact_digest": proposal["mask_artifact_digest"],
                "authority": "MACHINE_PROPOSAL_DIAGNOSTIC_ONLY",
            }
        mask = masks.get(mask_id) if mask_id else masks.latest(reference_id)
        if mask is None:
            raise ValueError("visual-geometry evaluation requires a reviewed mask or proposal")
        if mask["reference_id"] != reference_id:
            raise ValueError("reference mask belongs to a different reference")
        if mask.get("approval_state") != "approved" or not masks.verify_approved_mask(mask):
            raise ValueError("reference mask is not governed approved evidence")
        if not self._artifact_valid(mask["artifact_digest"]):
            raise ValueError("reference mask artifact is missing or corrupt")
        return {
            "id": mask["id"],
            "artifact_digest": mask["artifact_digest"],
            "authority": "REVIEWED_MASK",
        }

    @classmethod
    def _projection_gates(cls, metrics: dict[str, Any]) -> dict[str, bool]:
        thresholds = cls.PROJECTION_THRESHOLDS
        return {
            "silhouette_iou": metrics["silhouette_iou"] >= thresholds["silhouette_iou_minimum"],
            "silhouette_dice": metrics["silhouette_dice"] >= thresholds["silhouette_dice_minimum"],
            "foreground_precision": metrics["foreground_precision"]
            >= thresholds["foreground_precision_minimum"],
            "foreground_recall": metrics["foreground_recall"]
            >= thresholds["foreground_recall_minimum"],
            "boundary_rmse": metrics["boundary_rmse_fraction_of_diagonal"]
            <= thresholds["boundary_rmse_fraction_of_diagonal_maximum"],
        }

    @staticmethod
    def _attribute_causes(
        *,
        rig_state: str,
        mask_authority: str,
        projection_gates: dict[str, bool],
        missing_passes: list[str],
        local_status: str,
    ) -> list[dict[str, str]]:
        findings: list[dict[str, str]] = []
        if mask_authority != "REVIEWED_MASK":
            findings.append(
                {
                    "cause": "REFERENCE",
                    "finding": "projection uses an unreviewed mask proposal",
                }
            )
        if rig_state != "AUTHORITATIVE":
            findings.append({"cause": "CAMERA", "finding": "fixed rig camera is not approved"})
        if not all(projection_gates.values()):
            findings.append(
                {
                    "cause": "CAMERA" if rig_state != "AUTHORITATIVE" else "GEOMETRY",
                    "finding": "projection gate failure",
                }
            )
        if missing_passes:
            findings.append(
                {"cause": "UNKNOWN", "finding": "required diagnostic passes are absent"}
            )
        if local_status != "PASS":
            findings.append(
                {
                    "cause": "REFERENCE",
                    "finding": "reviewed local component evidence is unavailable",
                }
            )
        return findings or [{"cause": "UNKNOWN", "finding": "no scored failure"}]

    def _scorecard_receipt(self, scorecard: dict[str, Any]) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_geometry_scorecard",
            "id": scorecard["id"],
            "rig_id": scorecard["rig_id"],
            "scene_id": scorecard["scene_id"],
            "reference_id": scorecard["reference_id"],
            "render_run_id": scorecard["render_run_id"],
            "status": scorecard["status"],
            "scorecard": scorecard,
            "scorecard_sha256": hashlib.sha256(canonical_json(scorecard)).hexdigest(),
            "authority": scorecard["authority"],
            "created_at": scorecard["created_at"],
        }

    def _camera_snapshot_valid(self, rig: dict[str, Any]) -> bool:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?",
                (rig["camera_solution_id"],),
            ).fetchone()
        return bool(
            row
            and hashlib.sha256(canonical_json(json.loads(row["solution_json"]))).hexdigest()
            == rig["config"]["camera_solution_snapshot_sha256"]
        )

    def _artifact_valid(self, digest: Any) -> bool:
        try:
            path = self.artifacts.path_for(str(digest))
            return path.is_file() and sha256_file(path)[0] == digest
        except (OSError, TypeError, ValueError):
            return False

    def _reference(self, reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_items WHERE id=?", (reference_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown reference: {reference_id}")
        return dict(row)

    def _render_run(self, render_run_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM render_runs WHERE id=?", (render_run_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown render run: {render_run_id}")
        value = dict(row)
        value["config"] = json.loads(value.pop("config_json"))
        value["outputs"] = json.loads(value.pop("outputs_json"))
        return value

    def _rig(self, rig_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_geometry_rigs WHERE id=?", (rig_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual-geometry rig: {rig_id}")
        return self._normalize_rig(row)

    @staticmethod
    def _normalize_rig(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["config"] = json.loads(value.pop("config_json"))
        return value

    def _scorecard(self, scorecard_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_geometry_scorecards WHERE id=?", (scorecard_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual-geometry scorecard: {scorecard_id}")
        return self._normalize_scorecard(row)

    @staticmethod
    def _normalize_scorecard(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["scorecard"] = json.loads(value.pop("scorecard_json"))
        return value
