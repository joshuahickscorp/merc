from __future__ import annotations

import hashlib
import json
import uuid
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.visual_geometry.packets import ComponentTaskPacketStore
from blender_vision.visual_geometry.store import VisualGeometryStore

DEFECT_CLASSES = frozenset(
    {
        "CAMERA_ERROR",
        "GLOBAL_SCALE_ERROR",
        "GLOBAL_PROPORTION_ERROR",
        "COMPONENT_POSITION_ERROR",
        "COMPONENT_SCALE_ERROR",
        "MISSING_COMPONENT",
        "INCORRECT_DEPTH",
        "INCORRECT_CURVATURE",
        "INCORRECT_BEVEL",
        "INCORRECT_EDGE_PROFILE",
        "INCORRECT_ARRAY_DENSITY",
        "INCORRECT_HOLE_GEOMETRY",
        "INCORRECT_SURFACE_CONTINUITY",
        "NORMAL_ERROR",
        "MATERIAL_ERROR",
        "LIGHTING_ERROR",
        "EVIDENCE_CONFLICT",
        "EVIDENCE_MISSING",
    }
)


class VisualDefectDiagnosisStore:
    """Translate replayable visual residuals into bounded, non-accepting diagnoses."""

    SCHEMA_VERSION = 1

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.visual = VisualGeometryStore(project)
        self.packets = ComponentTaskPacketStore(project)

    def create(
        self,
        *,
        scorecard_id: str,
        rollback_scene_id: str,
        packet_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        scorecard_row = self.visual._scorecard(scorecard_id)
        if not self.visual.verify_scorecard(scorecard_id, replay=True)["valid"]:
            raise ValueError("residual diagnosis requires a replay-valid scorecard")
        scorecard = scorecard_row["scorecard"]
        rollback = SceneStore(self.project).get(rollback_scene_id)
        if not self._artifact_valid(rollback["artifact_digest"]):
            raise ValueError("rollback checkpoint scene artifact is missing or corrupt")
        packets = self._packets(
            scorecard=scorecard,
            packet_ids=packet_ids,
        )
        render_output = self._render_output(
            scorecard["render_run_id"], scorecard["reference_id"]
        )
        rollback_checkpoint = {
            "scene_id": rollback["id"],
            "scene_state": rollback["state"],
            "scene_artifact_digest": rollback["artifact_digest"],
            "relative_path": rollback["relative_path"],
        }
        diagnoses = self._derive(
            scorecard=scorecard,
            rig=self.visual._rig(scorecard["rig_id"]),
            render_output=render_output,
            packets=packets,
            rollback_checkpoint=rollback_checkpoint,
        )
        diagnosis_id = str(uuid.uuid4())
        created_at = utc_now()
        inputs = {
            "scorecard_id": scorecard_id,
            "scorecard_receipt_digest": scorecard_row["receipt_digest"],
            "render_run_id": scorecard["render_run_id"],
            "reference_id": scorecard["reference_id"],
            "render_output_sha256": hashlib.sha256(
                canonical_json(render_output)
            ).hexdigest(),
            "packet_ids": sorted(item["id"] for item in packets),
            "packet_receipt_digests": {
                item["id"]: item["receipt_digest"] for item in packets
            },
            "rollback_checkpoint": rollback_checkpoint,
        }
        status = "DIAGNOSED" if diagnoses else "NO_ACTIONABLE_RESIDUAL"
        report = {
            "schema_version": self.SCHEMA_VERSION,
            "id": diagnosis_id,
            "scorecard_id": scorecard_id,
            "scene_id": scorecard["scene_id"],
            "rig_id": scorecard["rig_id"],
            "render_run_id": scorecard["render_run_id"],
            "reference_id": scorecard["reference_id"],
            "status": status,
            "supported_defect_classes": sorted(DEFECT_CLASSES),
            "inputs": inputs,
            "diagnoses": diagnoses,
            "diagnosis_count": len(diagnoses),
            "authority": "DIAGNOSTIC_RESIDUAL_TO_REPAIR_NO_ACCEPTANCE_AUTHORITY",
            "created_at": created_at,
        }
        receipt = self._receipt(report)
        relative = Path("receipts") / f"visual-defect-diagnosis-{diagnosis_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-defect-diagnosis+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_defect_diagnoses("
                "id,scorecard_id,scene_id,rig_id,render_run_id,status,report_json,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    diagnosis_id,
                    scorecard_id,
                    scorecard["scene_id"],
                    scorecard["rig_id"],
                    scorecard["render_run_id"],
                    status,
                    json.dumps(report),
                    artifact.digest,
                    created_at,
                ),
            )
        return {
            **report,
            "receipt": receipt,
            "receipt_digest": artifact.digest,
            "path": str(relative),
        }

    def list(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM visual_defect_diagnoses "
                + ("WHERE scene_id=? " if scene_id else "")
                + "ORDER BY created_at,id",
                (scene_id,) if scene_id else (),
            ).fetchall()
        return [self._normalize(row) for row in rows]

    def verify(self, diagnosis_id: str) -> dict[str, Any]:
        try:
            row = self._get(diagnosis_id)
            report = row["report"]
            receipt_path = self.artifacts.path_for(row["receipt_digest"])
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            scorecard_row = self.visual._scorecard(report["scorecard_id"])
            scorecard = scorecard_row["scorecard"]
            inputs = report["inputs"]
            packets = self._packets(
                scorecard=scorecard,
                packet_ids=inputs["packet_ids"],
            )
            render_output = self._render_output(
                report["render_run_id"], report["reference_id"]
            )
            rollback = SceneStore(self.project).get(
                inputs["rollback_checkpoint"]["scene_id"]
            )
            rollback_checkpoint = {
                "scene_id": rollback["id"],
                "scene_state": rollback["state"],
                "scene_artifact_digest": rollback["artifact_digest"],
                "relative_path": rollback["relative_path"],
            }
            input_valid = bool(
                self.visual.verify_scorecard(report["scorecard_id"], replay=True)[
                    "valid"
                ]
                and scorecard_row["receipt_digest"]
                == inputs["scorecard_receipt_digest"]
                and hashlib.sha256(canonical_json(render_output)).hexdigest()
                == inputs["render_output_sha256"]
                and {
                    item["id"]: item["receipt_digest"] for item in packets
                }
                == inputs["packet_receipt_digests"]
                and all(self.packets.verify(item["id"])["valid"] for item in packets)
                and rollback_checkpoint == inputs["rollback_checkpoint"]
                and self._artifact_valid(rollback["artifact_digest"])
            )
            replayed = self._derive(
                scorecard=scorecard,
                rig=self.visual._rig(report["rig_id"]),
                render_output=render_output,
                packets=packets,
                rollback_checkpoint=rollback_checkpoint,
            )
            replay_valid = canonical_json(replayed) == canonical_json(
                report["diagnoses"]
            )
            receipt_valid = bool(
                self._artifact_valid(row["receipt_digest"])
                and canonical_json(receipt) == canonical_json(self._receipt(report))
                and row["status"] == report["status"]
            )
            return {
                "valid": bool(input_valid and replay_valid and receipt_valid),
                "input_valid": input_valid,
                "replay_valid": replay_valid,
                "receipt_valid": receipt_valid,
                "receipt": receipt,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {
                "valid": False,
                "input_valid": False,
                "replay_valid": False,
                "receipt_valid": False,
            }

    def _derive(
        self,
        *,
        scorecard: dict[str, Any],
        rig: dict[str, Any],
        render_output: dict[str, Any],
        packets: list[dict[str, Any]],
        rollback_checkpoint: dict[str, Any],
    ) -> list[dict[str, Any]]:
        passes = render_output.get("pass_artifact_digests", {})
        width = int(render_output.get("width", 0))
        height = int(render_output.get("height", 0))
        full_region = [0, 0, width, height]
        view = {
            "reference_id": scorecard["reference_id"],
            "camera_solution_id": rig["camera_solution_id"],
        }
        diagnoses: list[dict[str, Any]] = []

        def add(
            *,
            defect_class: str,
            semantic_component: str,
            binding_id: str | None,
            image_region: list[int],
            supporting_passes: list[str],
            candidate_parameters: dict[str, Any],
            confidence: float,
            expected_visual_impact: str,
            expected_gate_impact: dict[str, Any],
            repair_action: str,
            status: str,
            evidence: dict[str, Any],
        ) -> None:
            if defect_class not in DEFECT_CLASSES:
                raise ValueError(f"unsupported residual diagnosis class: {defect_class}")
            payload = {
                "defect_class": defect_class,
                "semantic_component": semantic_component,
                "binding_id": binding_id,
                "views": [view],
                "image_regions_xyxy": [image_region],
                "supporting_diagnostic_passes": sorted(
                    name for name in supporting_passes if name in passes
                ),
                "candidate_parameters": candidate_parameters,
                "confidence": round(float(confidence), 8),
                "expected_visual_impact": expected_visual_impact,
                "expected_gate_impact": expected_gate_impact,
                "repair_action": repair_action,
                "rollback_checkpoint": rollback_checkpoint,
                "status": status,
                "evidence": evidence,
                "authority": "DIAGNOSTIC_HYPOTHESIS_NO_REPAIR_OR_ACCEPTANCE_AUTHORITY",
            }
            payload["id"] = "diagnosis-" + hashlib.sha256(
                canonical_json(payload)
            ).hexdigest()[:20]
            if payload["id"] not in {item["id"] for item in diagnoses}:
                diagnoses.append(payload)

        inputs = scorecard["inputs"]
        if inputs.get("mask_authority") != "REVIEWED_MASK":
            add(
                defect_class="EVIDENCE_MISSING",
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=["silhouette", "object_id"],
                candidate_parameters={},
                confidence=1.0,
                expected_visual_impact=(
                    "A reviewed foreground boundary is required before projection "
                    "residuals can establish geometry."
                ),
                expected_gate_impact={
                    "blocked_gates": ["reviewed_reference_mask"],
                    "expected_direction": "unblocks_projection_authority",
                },
                repair_action="REVIEW_REFERENCE_MASK",
                status="EVIDENCE_REQUEST",
                evidence={
                    "mask_id": inputs.get("mask_id"),
                    "mask_authority": inputs.get("mask_authority"),
                },
            )
        if rig["state"] != "AUTHORITATIVE":
            add(
                defect_class="EVIDENCE_MISSING",
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=["silhouette", "wireframe"],
                candidate_parameters={},
                confidence=1.0,
                expected_visual_impact=(
                    "Camera uncertainty can move every projected edge and must be "
                    "resolved before geometry-specific repair."
                ),
                expected_gate_impact={
                    "blocked_gates": ["approved_fixed_camera"],
                    "expected_direction": "unblocks_camera_authority",
                },
                repair_action="REVIEW_FIXED_CAMERA",
                status="EVIDENCE_REQUEST",
                evidence={"rig_state": rig["state"], "rig_id": rig["id"]},
            )

        failed_projection_gates = sorted(
            name
            for name, passed in scorecard["projection"]["gates"].items()
            if not passed
        )
        if failed_projection_gates:
            camera_hypothesis = rig["state"] != "AUTHORITATIVE"
            add(
                defect_class=(
                    "CAMERA_ERROR" if camera_hypothesis else "GLOBAL_PROPORTION_ERROR"
                ),
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=["silhouette", "wireframe", "neutral_clay"],
                candidate_parameters=(
                    {"camera_state": "review current fixed-camera proposal"}
                    if camera_hypothesis
                    else {"global_form": "derive bounded envelope/proportion parameters"}
                ),
                confidence=0.55 if camera_hypothesis else 0.7,
                expected_visual_impact=(
                    "Correcting the dominant projection cause should align the "
                    "whole-object boundary in every affected view."
                ),
                expected_gate_impact={
                    "failed_gates": failed_projection_gates,
                    "expected_direction": "reduce_projection_residual",
                },
                repair_action=(
                    "REVIEW_OR_REFINE_CAMERA"
                    if camera_hypothesis
                    else "BOUND_GLOBAL_FORM_SEARCH"
                ),
                status="DIAGNOSTIC_HYPOTHESIS",
                evidence={"projection": scorecard["projection"]},
            )

        unavailable_layers = sorted(
            name
            for name, record in scorecard.get("ground_truth_layers", {}).items()
            if record.get("status") != "EVALUATED"
        )
        if unavailable_layers:
            add(
                defect_class="EVIDENCE_MISSING",
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=[
                    "depth",
                    "normal",
                    "world_normal",
                    "geometric_normal",
                    "curvature",
                ],
                candidate_parameters={},
                confidence=1.0,
                expected_visual_impact=(
                    "Depth, normal, curvature, or landmark ground truth is needed "
                    "to distinguish matching silhouettes from incorrect local surfaces."
                ),
                expected_gate_impact={
                    "blocked_gates": [
                        f"reference_{name}" for name in unavailable_layers
                    ],
                    "expected_direction": "enables_local_geometry_metrics",
                },
                repair_action="ACQUIRE_COMPATIBLE_GEOMETRY_EVIDENCE",
                status="EVIDENCE_REQUEST",
                evidence={"unavailable_ground_truth_layers": unavailable_layers},
            )

        edge_record = scorecard.get("edge_structure", {})
        edge_f1 = edge_record.get("edge_f1")
        if isinstance(edge_f1, (int, float)) and float(edge_f1) < 0.5:
            add(
                defect_class="EVIDENCE_CONFLICT",
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=["neutral_clay", "curvature", "highlight_flow"],
                candidate_parameters={},
                confidence=0.9,
                expected_visual_impact=(
                    "Low unclassified edge agreement may come from geometry, material, "
                    "reflection, shadow, or reference texture and cannot safely drive repair."
                ),
                expected_gate_impact={
                    "diagnostic_metric": "edge_f1",
                    "value": float(edge_f1),
                    "expected_direction": "classify_edges_before_parameter_search",
                },
                repair_action="CLASSIFY_REFERENCE_EDGES",
                status="EVIDENCE_REQUEST",
                evidence={"edge_structure": edge_record},
            )

        normal_record = render_output.get("render_diagnostics", {}).get(
            "normal_discontinuity"
        )
        if isinstance(normal_record, dict) and int(
            normal_record.get("nonzero_pixel_count", 0)
        ) > 0:
            add(
                defect_class="EVIDENCE_CONFLICT",
                semantic_component="WHOLE_OBJECT",
                binding_id=None,
                image_region=full_region,
                supporting_passes=[
                    "normal_discontinuity",
                    "highlight_flow",
                    "world_normal",
                    "geometric_normal",
                ],
                candidate_parameters={
                    "surface_continuity": [
                        "smooth_normals",
                        "bevel_profile",
                        "corner_radius",
                        "surface_tessellation",
                    ]
                },
                confidence=0.9,
                expected_visual_impact=(
                    "Classifying intended sharp boundaries separately from sidewall "
                    "faceting and highlight pinching enables bounded surface repair."
                ),
                expected_gate_impact={
                    "diagnostic_metric": "normal_discontinuity",
                    "p95_degrees": normal_record.get("p95_degrees"),
                    "maximum_degrees": normal_record.get("maximum_degrees"),
                    "expected_direction": "classify_before_surface_continuity_gate",
                },
                repair_action="CLASSIFY_SURFACE_CONTINUITY_REGIONS",
                status="DIAGNOSTIC_HYPOTHESIS",
                evidence={"normal_discontinuity": normal_record},
            )

        for row in packets:
            packet = row["packet"]
            region = list(packet.get("crop", {}).get("render_box_xyxy", full_region))
            local_projection = packet.get("metrics", {}).get("projection", {})
            blockers = sorted(set(packet.get("blockers", [])))
            if blockers:
                add(
                    defect_class="EVIDENCE_MISSING",
                    semantic_component=packet["semantic_component"],
                    binding_id=packet["binding_id"],
                    image_region=region,
                    supporting_passes=[
                        "object_id",
                        "silhouette",
                        "depth",
                        "normal",
                        "curvature",
                        "normal_discontinuity",
                        "highlight_flow",
                    ],
                    candidate_parameters=packet.get("current_parameters", {}),
                    confidence=1.0,
                    expected_visual_impact=(
                        "A reviewed local reference mask and accepted semantic binding "
                        "are required before this visible component can be optimized."
                    ),
                    expected_gate_impact={
                        "blocked_gates": blockers,
                        "expected_direction": "enables_component_score",
                    },
                    repair_action="REVIEW_COMPONENT_MASK_AND_BINDING",
                    status="EVIDENCE_REQUEST",
                    evidence={
                        "packet_id": packet["id"],
                        "packet_status": packet["status"],
                        "object_name": packet["object_name"],
                    },
                )
            if local_projection.get("status") == "EVALUATED":
                metrics = local_projection["metrics"]
                if float(metrics["silhouette_iou"]) < 0.9:
                    add(
                        defect_class="COMPONENT_SCALE_ERROR",
                        semantic_component=packet["semantic_component"],
                        binding_id=packet["binding_id"],
                        image_region=region,
                        supporting_passes=[
                            "silhouette",
                            "object_id",
                            "wireframe",
                            "neutral_clay",
                        ],
                        candidate_parameters=packet.get("current_parameters", {}),
                        confidence=0.7,
                        expected_visual_impact=(
                            "A bounded semantic parameter search should reduce the "
                            "component-local silhouette residual."
                        ),
                        expected_gate_impact={
                            "diagnostic_metric": "component_silhouette_iou",
                            "value": float(metrics["silhouette_iou"]),
                            "expected_direction": "increase",
                        },
                        repair_action="BOUND_COMPONENT_PARAMETER_SEARCH",
                        status="DIAGNOSTIC_HYPOTHESIS",
                        evidence={
                            "packet_id": packet["id"],
                            "projection_metrics": metrics,
                        },
                    )

        return sorted(diagnoses, key=lambda item: item["id"])

    def _packets(
        self,
        *,
        scorecard: dict[str, Any],
        packet_ids: list[str] | None,
    ) -> list[dict[str, Any]]:
        available = [
            item
            for item in self.packets.list(rig_id=scorecard["rig_id"])
            if item["render_run_id"] == scorecard["render_run_id"]
            and item["reference_id"] == scorecard["reference_id"]
        ]
        if packet_ids is not None:
            requested = set(packet_ids)
            unknown = sorted(requested - {item["id"] for item in available})
            if unknown:
                raise KeyError(f"diagnosis packet ids do not match the scorecard: {unknown}")
            available = [item for item in available if item["id"] in requested]
        invalid = [
            item["id"] for item in available if not self.packets.verify(item["id"])["valid"]
        ]
        if invalid:
            raise ValueError(f"residual diagnosis packet receipts are invalid: {invalid}")
        return sorted(available, key=lambda item: item["id"])

    def _render_output(self, render_run_id: str, reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT outputs_json FROM render_runs WHERE id=?", (render_run_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown render run: {render_run_id}")
        output = next(
            (
                item
                for item in json.loads(row["outputs_json"])
                if str(item.get("reference_id")) == reference_id
            ),
            None,
        )
        if output is None:
            raise ValueError("diagnosis scorecard reference is absent from its render run")
        return output

    def _receipt(self, report: dict[str, Any]) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_defect_diagnosis",
            "id": report["id"],
            "scorecard_id": report["scorecard_id"],
            "scene_id": report["scene_id"],
            "status": report["status"],
            "report": report,
            "report_sha256": hashlib.sha256(canonical_json(report)).hexdigest(),
            "authority": report["authority"],
            "created_at": report["created_at"],
        }

    def _get(self, diagnosis_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_defect_diagnoses WHERE id=?", (diagnosis_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual-defect diagnosis: {diagnosis_id}")
        return self._normalize(row)

    def _artifact_valid(self, digest: str) -> bool:
        try:
            path = self.artifacts.path_for(digest)
            return path.is_file() and sha256_file(path)[0] == digest
        except (OSError, TypeError, ValueError):
            return False

    @staticmethod
    def _normalize(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["report"] = json.loads(value.pop("report_json"))
        return value
