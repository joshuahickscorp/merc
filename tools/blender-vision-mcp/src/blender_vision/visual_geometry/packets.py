from __future__ import annotations

import hashlib
import json
import subprocess
import tempfile
import uuid
from pathlib import Path
from typing import Any

import numpy as np
from PIL import Image, ImageOps

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.config import discover_blender
from blender_vision.core.util import atomic_write_json, canonical_json, sha256_file, utc_now
from blender_vision.geometry.scenes import SceneStore
from blender_vision.projects.store import ProjectStore
from blender_vision.visual_geometry.bindings import SemanticBindingStore
from blender_vision.visual_geometry.metrics import (
    PROJECTION_ENGINE_V2,
    edge_structure_metrics,
    projection_metrics,
)
from blender_vision.visual_geometry.store import VisualGeometryStore

PACKET_PASSES = (
    "beauty",
    "silhouette",
    "depth",
    "normal",
    "world_normal",
    "geometric_normal",
    "curvature",
    "grazing_left",
    "grazing_right",
    "grazing_top",
    "wireframe",
    "object_id",
    "component_id",
    "feature_id",
    "neutral_clay",
    "material_neutral",
    "zebra",
    "reflected_line",
    "normal_discontinuity",
    "highlight_flow",
)


class ComponentTaskPacketStore:
    """Create native-reference and render-pass crops for one bound visible component."""

    SCHEMA_VERSION = 1

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def create(
        self,
        *,
        binding_id: str,
        rig_id: str,
        render_run_id: str,
        baseline_render_run_id: str | None = None,
        padding_fraction: float = 0.2,
    ) -> dict[str, Any]:
        if not 0.0 <= padding_fraction <= 1.0:
            raise ValueError("component crop padding fraction must be between zero and one")
        bindings = SemanticBindingStore(self.project)
        binding = bindings.get(binding_id)
        if not bindings.verify(binding_id)["valid"]:
            raise ValueError("component task packet requires a valid semantic binding")
        visual = VisualGeometryStore(self.project)
        rig = next((item for item in visual.list_rigs() if item["id"] == rig_id), None)
        if rig is None or not visual.verify_rig(rig_id)["valid"]:
            raise ValueError("component task packet requires a valid fixed rig")
        if binding["scene_id"] != rig["scene_id"]:
            raise ValueError("semantic binding and fixed rig belong to different scenes")
        candidate_run = self._render_run(render_run_id)
        baseline_run = self._render_run(baseline_render_run_id or render_run_id)
        if candidate_run["scene_id"] != binding["scene_id"]:
            raise ValueError("candidate render run is not for the bound scene")
        if candidate_run["camera_solution_id"] != rig["camera_solution_id"]:
            raise ValueError("candidate render run does not use the fixed rig camera")
        if baseline_run["camera_solution_id"] != rig["camera_solution_id"]:
            raise ValueError("baseline render run does not use the fixed rig camera")
        requested_views = set(binding["record"].get("evaluation_views", []))
        outputs = [
            output
            for output in candidate_run["outputs"]
            if not requested_views or str(output.get("reference_id")) in requested_views
        ]
        if not outputs:
            raise ValueError("component binding has no compatible render outputs")
        packets = []
        for output in outputs:
            reference_id = str(output["reference_id"])
            baseline_output = next(
                (
                    item
                    for item in baseline_run["outputs"]
                    if str(item.get("reference_id")) == reference_id
                ),
                None,
            )
            packets.append(
                self._create_one(
                    binding=binding,
                    rig=rig,
                    candidate_run=candidate_run,
                    candidate_output=output,
                    baseline_run=baseline_run,
                    baseline_output=baseline_output,
                    padding_fraction=padding_fraction,
                )
            )
        return {
            "binding_id": binding_id,
            "rig_id": rig_id,
            "packets": packets,
            "packet_count": len(packets),
        }

    def list(
        self,
        *,
        binding_id: str | None = None,
        rig_id: str | None = None,
    ) -> list[dict[str, Any]]:
        clauses = []
        parameters = []
        if binding_id:
            clauses.append("binding_id=?")
            parameters.append(binding_id)
        if rig_id:
            clauses.append("rig_id=?")
            parameters.append(rig_id)
        query = "SELECT * FROM visual_component_packets "
        if clauses:
            query += "WHERE " + " AND ".join(clauses) + " "
        query += "ORDER BY created_at,id"
        with self.project.connection() as connection:
            rows = connection.execute(query, parameters).fetchall()
        return [self._normalize(row) for row in rows]

    def verify(self, packet_id: str) -> dict[str, Any]:
        try:
            packet = self._get(packet_id)
            receipt_path = self.artifacts.path_for(packet["receipt_digest"])
            receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
            expected = self._receipt(packet["packet"])
            digests = self._packet_artifact_digests(packet["packet"])
            receipt_valid = bool(
                self._artifact_valid(packet["receipt_digest"])
                and canonical_json(receipt) == canonical_json(expected)
                and packet["status"] == packet["packet"]["status"]
            )
            artifacts_valid = all(self._artifact_valid(digest) for digest in digests)
            replay_valid = True
            projection = packet["packet"].get("metrics", {}).get("projection")
            if projection and projection.get("status") == "EVALUATED":
                inputs = packet["packet"]["inputs"]
                replayed = projection_metrics(
                    self.artifacts.path_for(inputs["component_reference_mask_digest"]),
                    self.artifacts.path_for(inputs["rendered_object_mask_digest"]),
                    engine=PROJECTION_ENGINE_V2,
                )
                replay_valid = canonical_json(replayed) == canonical_json(projection["metrics"])
            return {
                "valid": bool(receipt_valid and artifacts_valid and replay_valid),
                "receipt_valid": receipt_valid,
                "artifacts_valid": artifacts_valid,
                "replay_valid": replay_valid,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {
                "valid": False,
                "receipt_valid": False,
                "artifacts_valid": False,
                "replay_valid": False,
            }

    def _create_one(
        self,
        *,
        binding: dict[str, Any],
        rig: dict[str, Any],
        candidate_run: dict[str, Any],
        candidate_output: dict[str, Any],
        baseline_run: dict[str, Any],
        baseline_output: dict[str, Any] | None,
        padding_fraction: float,
    ) -> dict[str, Any]:
        reference_id = str(candidate_output["reference_id"])
        existing = self._existing(binding["id"], rig["id"], candidate_run["id"], reference_id)
        if existing is not None:
            return existing
        passes = candidate_output.get("pass_artifact_digests", {})
        object_id_digest = passes.get("object_id")
        mapping = candidate_output.get("object_ids", {}).get(binding["object_name"])
        reference = self._reference(reference_id)
        if not object_id_digest or not mapping:
            return self._persist_unavailable(
                binding=binding,
                rig=rig,
                candidate_run=candidate_run,
                baseline_run=baseline_run,
                reference=reference,
                reason="object-ID pass or object color mapping is absent",
            )
        if not self._artifact_valid(object_id_digest):
            raise ValueError("component object-ID artifact is missing or corrupt")

        with tempfile.TemporaryDirectory(prefix="bvmcp-component-packet-") as temporary:
            temporary_root = Path(temporary)
            mask, bbox = self._object_mask(
                self.artifacts.path_for(object_id_digest), mapping["rgb"]
            )
            palette_bbox = bbox
            projected_bbox = self._projected_bounds_box(
                binding=binding,
                reference_id=reference_id,
                camera_solution_id=candidate_run["camera_solution_id"],
                render_size=(
                    int(candidate_output.get("width", 0)),
                    int(candidate_output.get("height", 0)),
                ),
            )
            object_id_crosscheck = self._object_id_crosscheck(
                palette_bbox=palette_bbox,
                projected_bbox=projected_bbox,
                allow_reviewed_without_projection=binding["state"] == "ACCEPTED_BOUND",
            )
            object_mask_authority = (
                "EXACT_RECORDED_OBJECT_ID_COLOR"
                if object_id_crosscheck["valid"]
                else "PROJECTED_WORLD_BOUNDS_DIAGNOSTIC_ONLY"
            )
            if not object_id_crosscheck["valid"]:
                bbox = None
            if bbox is None:
                bbox = projected_bbox
                if bbox is None:
                    return self._persist_unavailable(
                        binding=binding,
                        rig=rig,
                        candidate_run=candidate_run,
                        baseline_run=baseline_run,
                        reference=reference,
                        reason=(
                            "bound object is absent from the ID palette and its governed "
                            "world bounds do not project into the frame"
                        ),
                    )
                mask = np.zeros(
                    (
                        int(candidate_output["height"]),
                        int(candidate_output["width"]),
                    ),
                    dtype=bool,
                )
                mask[bbox[1] : bbox[3], bbox[0] : bbox[2]] = True
            crop_box = self._padded_box(bbox, mask.shape[1], mask.shape[0], padding_fraction)
            mask_path = temporary_root / "rendered-object-mask.png"
            Image.fromarray(mask.astype(np.uint8) * 255, "L").save(mask_path)
            mask_artifact = self.artifacts.ingest_file(mask_path, media_type="image/png")
            crop_artifacts: dict[str, dict[str, Any]] = {}
            decode_blockers = []
            exr_passes: dict[str, str] = {}
            for pass_name in PACKET_PASSES:
                digest = passes.get(pass_name)
                if digest and self._artifact_media_type(digest) == "image/x-exr":
                    exr_passes[pass_name] = digest
            exr_crops: dict[str, dict[str, Any]] = {}
            for digest in sorted(set(exr_passes.values())):
                pass_names = sorted(
                    name for name, value in exr_passes.items() if value == digest
                )
                exr_crops.update(
                    self._crop_exr_diagnostics(
                        digest=digest,
                        pass_names=pass_names,
                        crop_box=crop_box,
                        temporary_root=temporary_root,
                    )
                )
            for pass_name in PACKET_PASSES:
                digest = passes.get(pass_name)
                if not digest:
                    crop_artifacts[pass_name] = {
                        "status": "UNAVAILABLE",
                        "reason": f"{pass_name} is not present in the governed render suite",
                    }
                    continue
                crop = exr_crops.get(pass_name) or self._crop_artifact(
                    digest, crop_box, temporary_root / f"candidate-{pass_name}.png"
                )
                crop_artifacts[pass_name] = crop
                if crop["status"] != "AVAILABLE":
                    decode_blockers.append(pass_name)

            baseline_crop = {
                "status": "UNAVAILABLE",
                "reason": "compatible baseline render output is absent",
            }
            if baseline_output:
                baseline_beauty = baseline_output.get("pass_artifact_digests", {}).get(
                    "beauty"
                ) or baseline_output.get("artifact_digest")
                if baseline_beauty:
                    baseline_crop = self._crop_artifact(
                        baseline_beauty,
                        crop_box,
                        temporary_root / "baseline-beauty.png",
                    )

            reference_crop = self._crop_reference(
                reference["artifact_digest"],
                crop_box,
                (mask.shape[1], mask.shape[0]),
                temporary_root / "reference-native.png",
            )
            component_region = self._component_region(binding, reference_id)
            projection = self._projection_evaluation(
                binding=binding,
                component_region=component_region,
                rendered_mask_digest=mask_artifact.digest,
                rendered_mask_authority=object_mask_authority,
                temporary_root=temporary_root,
            )
            edge_metrics = self._edge_evaluation(
                reference_crop=reference_crop,
                candidate_crop=crop_artifacts.get("neutral_clay", {}),
                temporary_root=temporary_root,
            )
            generated = {
                "rendered_object_mask": {
                    "status": "AVAILABLE",
                    "artifact_digest": mask_artifact.digest,
                },
                "reference_native": reference_crop,
                "baseline_beauty": baseline_crop,
                "candidate_passes": crop_artifacts,
            }
            if projection.get("residual_artifact_digest"):
                generated["silhouette_overlay"] = {
                    "status": "AVAILABLE",
                    "artifact_digest": projection["residual_artifact_digest"],
                }
            else:
                generated["silhouette_overlay"] = {
                    "status": "UNAVAILABLE",
                    "reason": "reviewed component reference mask is absent",
                }
            if edge_metrics.get("overlay_artifact_digest"):
                generated["edge_overlay"] = {
                    "status": "AVAILABLE",
                    "artifact_digest": edge_metrics["overlay_artifact_digest"],
                }
            else:
                generated["edge_overlay"] = {
                    "status": "UNAVAILABLE",
                    "reason": edge_metrics.get("reason", "edge evidence is unavailable"),
                }

        projection_evaluated = projection["status"] == "EVALUATED"
        status = "COMPONENT_EVALUATED" if projection_evaluated else "DIAGNOSTIC_PACKET_READY"
        blockers = []
        if binding["state"] != "ACCEPTED_BOUND":
            blockers.append("semantic binding is not ACCEPTED_BOUND")
        if object_mask_authority != "EXACT_RECORDED_OBJECT_ID_COLOR":
            blockers.append(
                "object isolation uses projected world bounds; visibility and silhouette are "
                "not component-mask evidence"
            )
        if not projection_evaluated:
            blockers.append("reviewed component reference mask is unavailable")
        for required in ("zebra", "reflected_line"):
            if generated["candidate_passes"][required]["status"] != "AVAILABLE":
                blockers.append(f"{required} diagnostic pass is unavailable")
        if decode_blockers:
            blockers.append(
                "one or more diagnostic pass crops could not be decoded: "
                + ", ".join(sorted(decode_blockers))
            )
        packet_id = str(uuid.uuid4())
        created_at = utc_now()
        packet = {
            "schema_version": self.SCHEMA_VERSION,
            "id": packet_id,
            "binding_id": binding["id"],
            "semantic_id": binding["semantic_id"],
            "semantic_component": binding["record"]["semantic_component"],
            "object_name": binding["object_name"],
            "visual_frequency": self._visual_frequency(binding["record"]["semantic_component"]),
            "visual_importance": binding["record"]["visual_importance"],
            "mandatory_component": binding["record"]["visual_importance"] >= 2.5,
            "rig_id": rig["id"],
            "scene_id": binding["scene_id"],
            "reference_id": reference_id,
            "render_run_id": candidate_run["id"],
            "baseline_render_run_id": baseline_run["id"],
            "status": status,
            "acceptance_status": "PASS" if not blockers else "BLOCKED",
            "crop": {
                "render_box_xyxy": list(crop_box),
                "render_size": [mask.shape[1], mask.shape[0]],
                "visible_area_px": int(mask.sum()),
                "padding_fraction": padding_fraction,
                "reference_native_resolution_preserved": True,
            },
            "inputs": {
                "scene_artifact_digest": SceneStore(self.project).get(binding["scene_id"])[
                    "artifact_digest"
                ],
                "binding_proposal_digest": binding["proposal_digest"],
                "binding_decision_digest": binding["decision_digest"],
                "reference_artifact_digest": reference["artifact_digest"],
                "object_id_artifact_digest": object_id_digest,
                "object_id_policy": candidate_output.get("id_pass_policy", {}),
                "object_id_crosscheck": object_id_crosscheck,
                "rendered_object_mask_digest": mask_artifact.digest,
                "rendered_object_mask_authority": object_mask_authority,
                "component_reference_mask_digest": (
                    component_region.get("mask_artifact_digest") if component_region else None
                ),
            },
            "artifacts": generated,
            "metrics": {"projection": projection, "edge_structure": edge_metrics},
            "current_parameters": binding["record"]["parameters"],
            "evidence_confidence": binding["record"]["confidence"],
            "candidate_history": self._history(binding["id"], reference_id),
            "semantic_landmarks": component_region.get("landmarks", []) if component_region else [],
            "blockers": blockers,
            "authority": (
                "COMPONENT_VISUAL_EVIDENCE"
                if not blockers
                else "DIAGNOSTIC_COMPONENT_PACKET_NO_ACCEPTANCE_AUTHORITY"
            ),
            "created_at": created_at,
        }
        return self._persist(packet)

    def _persist_unavailable(
        self,
        *,
        binding: dict[str, Any],
        rig: dict[str, Any],
        candidate_run: dict[str, Any],
        baseline_run: dict[str, Any],
        reference: dict[str, Any],
        reason: str,
    ) -> dict[str, Any]:
        packet_id = str(uuid.uuid4())
        created_at = utc_now()
        packet = {
            "schema_version": self.SCHEMA_VERSION,
            "id": packet_id,
            "binding_id": binding["id"],
            "semantic_id": binding["semantic_id"],
            "semantic_component": binding["record"]["semantic_component"],
            "object_name": binding["object_name"],
            "visual_frequency": self._visual_frequency(binding["record"]["semantic_component"]),
            "visual_importance": binding["record"]["visual_importance"],
            "mandatory_component": binding["record"]["visual_importance"] >= 2.5,
            "rig_id": rig["id"],
            "scene_id": binding["scene_id"],
            "reference_id": reference["id"],
            "render_run_id": candidate_run["id"],
            "baseline_render_run_id": baseline_run["id"],
            "status": "NOT_VISIBLE_OR_UNAVAILABLE",
            "acceptance_status": "BLOCKED",
            "crop": None,
            "inputs": {
                "scene_artifact_digest": SceneStore(self.project).get(binding["scene_id"])[
                    "artifact_digest"
                ],
                "binding_proposal_digest": binding["proposal_digest"],
                "binding_decision_digest": binding["decision_digest"],
                "reference_artifact_digest": reference["artifact_digest"],
                "object_id_artifact_digest": None,
                "rendered_object_mask_digest": None,
                "rendered_object_mask_authority": "UNAVAILABLE",
                "component_reference_mask_digest": None,
            },
            "artifacts": {},
            "metrics": {
                "projection": {"status": "NOT_EVALUATED", "reason": reason},
                "edge_structure": {"status": "NOT_EVALUATED", "reason": reason},
            },
            "current_parameters": binding["record"]["parameters"],
            "evidence_confidence": binding["record"]["confidence"],
            "candidate_history": self._history(binding["id"], reference["id"]),
            "semantic_landmarks": [],
            "blockers": [reason],
            "authority": "DIAGNOSTIC_COMPONENT_PACKET_NO_ACCEPTANCE_AUTHORITY",
            "created_at": created_at,
        }
        return self._persist(packet)

    def _persist(self, packet: dict[str, Any]) -> dict[str, Any]:
        receipt = self._receipt(packet)
        relative = Path("receipts") / f"visual-component-packet-{packet['id']}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-component-packet+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_component_packets("
                "id,binding_id,rig_id,render_run_id,reference_id,status,packet_json,"
                "receipt_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?)",
                (
                    packet["id"],
                    packet["binding_id"],
                    packet["rig_id"],
                    packet["render_run_id"],
                    packet["reference_id"],
                    packet["status"],
                    json.dumps(packet),
                    artifact.digest,
                    packet["created_at"],
                ),
            )
        return {**packet, "receipt_digest": artifact.digest}

    def _projection_evaluation(
        self,
        *,
        binding: dict[str, Any],
        component_region: dict[str, Any] | None,
        rendered_mask_digest: str,
        rendered_mask_authority: str,
        temporary_root: Path,
    ) -> dict[str, Any]:
        if rendered_mask_authority != "EXACT_RECORDED_OBJECT_ID_COLOR":
            return {
                "status": "NOT_EVALUATED",
                "reason": "component projection requires an exact replayed object-ID mask",
                "authority": "NO_COMPONENT_PROJECTION_AUTHORITY",
            }
        if binding["state"] != "ACCEPTED_BOUND" or not component_region:
            return {
                "status": "NOT_EVALUATED",
                "reason": "accepted binding with reviewed component mask is required",
                "authority": "NO_COMPONENT_PROJECTION_AUTHORITY",
            }
        digest = component_region.get("mask_artifact_digest")
        if not digest or not self._artifact_valid(str(digest)):
            return {
                "status": "NOT_EVALUATED",
                "reason": "reviewed component mask artifact is absent or corrupt",
                "authority": "NO_COMPONENT_PROJECTION_AUTHORITY",
            }
        residual_path = temporary_root / "component-projection-residual.png"
        metrics = projection_metrics(
            self.artifacts.path_for(str(digest)),
            self.artifacts.path_for(rendered_mask_digest),
            residual_path=residual_path,
            engine=PROJECTION_ENGINE_V2,
        )
        residual = self.artifacts.ingest_file(residual_path, media_type="image/png")
        return {
            "status": "EVALUATED",
            "metrics": metrics,
            "residual_artifact_digest": residual.digest,
            "authority": "REVIEWED_COMPONENT_MASK_PROJECTION_EVIDENCE",
        }

    def _edge_evaluation(
        self,
        *,
        reference_crop: dict[str, Any],
        candidate_crop: dict[str, Any],
        temporary_root: Path,
    ) -> dict[str, Any]:
        if (
            reference_crop.get("status") != "AVAILABLE"
            or candidate_crop.get("status") != "AVAILABLE"
        ):
            return {
                "status": "NOT_EVALUATED",
                "reason": "reference and neutral-clay crops are required",
            }
        overlay_path = temporary_root / "component-edge-overlay.png"
        metrics = edge_structure_metrics(
            self.artifacts.path_for(reference_crop["artifact_digest"]),
            self.artifacts.path_for(candidate_crop["artifact_digest"]),
            overlay_path=overlay_path,
        )
        overlay = self.artifacts.ingest_file(overlay_path, media_type="image/png")
        return {
            "status": "DIAGNOSTIC_ONLY",
            "metrics": metrics,
            "overlay_artifact_digest": overlay.digest,
            "authority": "UNCLASSIFIED_COMPONENT_EDGES_DIAGNOSTIC_ONLY",
        }

    def _crop_artifact(
        self,
        digest: str,
        crop_box: tuple[int, int, int, int],
        destination: Path,
    ) -> dict[str, Any]:
        if not self._artifact_valid(digest):
            return {"status": "UNAVAILABLE", "reason": "source artifact is corrupt"}
        try:
            with Image.open(self.artifacts.path_for(digest)) as image:
                normalized = ImageOps.exif_transpose(image)
                normalized.crop(crop_box).save(destination, format="PNG")
        except (OSError, ValueError):
            return {
                "status": "UNAVAILABLE",
                "source_artifact_digest": digest,
                "reason": "source pass format is not crop-decodable by the packet engine",
            }
        artifact = self.artifacts.ingest_file(destination, media_type="image/png")
        return {
            "status": "AVAILABLE",
            "source_artifact_digest": digest,
            "artifact_digest": artifact.digest,
        }

    def _crop_exr_diagnostics(
        self,
        *,
        digest: str,
        pass_names: list[str],
        crop_box: tuple[int, int, int, int],
        temporary_root: Path,
    ) -> dict[str, dict[str, Any]]:
        supported = sorted(set(pass_names) & {"depth", "normal"})
        unsupported = sorted(set(pass_names) - set(supported))
        results = {
            name: {
                "status": "UNAVAILABLE",
                "source_artifact_digest": digest,
                "reason": f"multilayer OpenEXR crop is unsupported for {name}",
            }
            for name in unsupported
        }
        if not supported:
            return results
        capability = discover_blender()
        if not capability.available or capability.path is None:
            for name in supported:
                results[name] = {
                    "status": "UNAVAILABLE",
                    "source_artifact_digest": digest,
                    "reason": "Blender is unavailable for bundled OpenEXR decoding",
                }
            return results
        helper = Path(__file__).resolve().parents[3] / "blender_worker" / "exr_crop.py"
        output_paths = {
            name: temporary_root / f"candidate-{name}.png" for name in supported
        }
        result_path = temporary_root / "exr-crop-result.json"
        manifest_path = temporary_root / "exr-crop-manifest.json"
        atomic_write_json(
            manifest_path,
            {
                "schema_version": 1,
                "project_root": str(self.project.root),
                "source_path": str(self.artifacts.path_for(digest)),
                "crop_box_xyxy": list(crop_box),
                "outputs": {name: str(path) for name, path in output_paths.items()},
                "result_path": str(result_path),
            },
        )
        command = [
            capability.path,
            "--background",
            "--factory-startup",
            "--python",
            str(helper),
            "--",
            str(manifest_path),
        ]
        try:
            completed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                timeout=120,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            detail = f"OpenEXR crop helper failed: {type(error).__name__}"
            for name in supported:
                results[name] = {
                    "status": "UNAVAILABLE",
                    "source_artifact_digest": digest,
                    "reason": detail,
                }
            return results
        if completed.returncode or not result_path.is_file():
            detail = (
                completed.stderr.strip().splitlines()[-1]
                if completed.stderr.strip()
                else "OpenEXR crop helper returned no result"
            )
            for name in supported:
                results[name] = {
                    "status": "UNAVAILABLE",
                    "source_artifact_digest": digest,
                    "reason": detail,
                }
            return results
        derivation = json.loads(result_path.read_text(encoding="utf-8"))
        for name in supported:
            path = output_paths[name]
            if not path.is_file():
                results[name] = {
                    "status": "UNAVAILABLE",
                    "source_artifact_digest": digest,
                    "reason": f"OpenEXR crop helper omitted {name}",
                }
                continue
            artifact = self.artifacts.ingest_file(path, media_type="image/png")
            results[name] = {
                "status": "AVAILABLE",
                "source_artifact_digest": digest,
                "artifact_digest": artifact.digest,
                "derivation": derivation["outputs"][name],
            }
        return results

    def _artifact_media_type(self, digest: str) -> str | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT media_type FROM artifacts WHERE digest=?", (digest,)
            ).fetchone()
        return str(row["media_type"]) if row is not None else None

    def _crop_reference(
        self,
        digest: str,
        render_box: tuple[int, int, int, int],
        render_size: tuple[int, int],
        destination: Path,
    ) -> dict[str, Any]:
        if not self._artifact_valid(digest):
            return {"status": "UNAVAILABLE", "reason": "reference artifact is corrupt"}
        try:
            with Image.open(self.artifacts.path_for(digest)) as image:
                normalized = ImageOps.exif_transpose(image)
                scale_x = normalized.width / render_size[0]
                scale_y = normalized.height / render_size[1]
                native_box = (
                    max(0, int(round(render_box[0] * scale_x))),
                    max(0, int(round(render_box[1] * scale_y))),
                    min(normalized.width, int(round(render_box[2] * scale_x))),
                    min(normalized.height, int(round(render_box[3] * scale_y))),
                )
                normalized.crop(native_box).save(destination, format="PNG")
        except (OSError, ValueError):
            return {"status": "UNAVAILABLE", "reason": "reference is not a decodable image"}
        artifact = self.artifacts.ingest_file(destination, media_type="image/png")
        return {
            "status": "AVAILABLE",
            "source_artifact_digest": digest,
            "artifact_digest": artifact.digest,
            "native_box_xyxy": list(native_box),
            "native_size": [normalized.width, normalized.height],
        }

    @staticmethod
    def _object_mask(
        path: Path, rgb: list[int]
    ) -> tuple[np.ndarray, tuple[int, int, int, int] | None]:
        with Image.open(path) as image:
            pixels = np.asarray(ImageOps.exif_transpose(image).convert("RGB"), dtype=np.int16)
        target = np.asarray(rgb, dtype=np.int16).reshape(1, 1, 3)
        mask = np.max(np.abs(pixels - target), axis=2) <= 4
        ys, xs = np.where(mask)
        if not len(xs):
            return mask, None
        return mask, (int(xs.min()), int(ys.min()), int(xs.max()) + 1, int(ys.max()) + 1)

    def _projected_bounds_box(
        self,
        *,
        binding: dict[str, Any],
        reference_id: str,
        camera_solution_id: str,
        render_size: tuple[int, int],
    ) -> tuple[int, int, int, int] | None:
        width, height = render_size
        bounds = binding["record"].get("parameters", {}).get("world_bounds", {})
        minimum = bounds.get("minimum")
        maximum = bounds.get("maximum")
        if (
            width <= 0
            or height <= 0
            or not isinstance(minimum, list)
            or not isinstance(maximum, list)
            or len(minimum) != 3
            or len(maximum) != 3
        ):
            return None
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT solution_json FROM camera_solutions WHERE id=?",
                (camera_solution_id,),
            ).fetchone()
        if row is None:
            return None
        document = json.loads(row["solution_json"])
        cameras = document if isinstance(document, list) else document.get("cameras", [])
        camera = next(
            (item for item in cameras if str(item.get("reference_id")) == reference_id),
            None,
        )
        if camera is None:
            return None
        matrix = camera.get("world_from_camera") or camera.get("extrinsics", {}).get(
            "world_from_camera"
        )
        intrinsics = camera.get("intrinsics", {})
        source_width = float(camera.get("width", width))
        source_height = float(camera.get("height", height))
        if not isinstance(matrix, list) or source_width <= 0 or source_height <= 0:
            return None
        camera_from_world = np.linalg.inv(np.asarray(matrix, dtype=np.float64))
        scale_x = width / source_width
        scale_y = height / source_height
        fx = float(intrinsics.get("fx", 0.0)) * scale_x
        fy = float(intrinsics.get("fy", 0.0)) * scale_y
        cx = float(intrinsics.get("cx", source_width / 2.0)) * scale_x
        cy = float(intrinsics.get("cy", source_height / 2.0)) * scale_y
        projected = []
        for x_value in (float(minimum[0]), float(maximum[0])):
            for y_value in (float(minimum[1]), float(maximum[1])):
                for z_value in (float(minimum[2]), float(maximum[2])):
                    camera_point = camera_from_world @ np.asarray(
                        [x_value, y_value, z_value, 1.0], dtype=np.float64
                    )
                    depth = -float(camera_point[2])
                    if depth <= 1e-9:
                        continue
                    projected.append(
                        (
                            fx * float(camera_point[0]) / depth + cx,
                            cy - fy * float(camera_point[1]) / depth,
                        )
                    )
        if not projected:
            return None
        left = max(0, min(width, int(np.floor(min(value[0] for value in projected)))))
        right = max(0, min(width, int(np.ceil(max(value[0] for value in projected)))))
        top = max(0, min(height, int(np.floor(min(value[1] for value in projected)))))
        bottom = max(0, min(height, int(np.ceil(max(value[1] for value in projected)))))
        if right <= left or bottom <= top:
            return None
        return left, top, right, bottom

    @staticmethod
    def _object_id_crosscheck(
        *,
        palette_bbox: tuple[int, int, int, int] | None,
        projected_bbox: tuple[int, int, int, int] | None,
        allow_reviewed_without_projection: bool,
    ) -> dict[str, Any]:
        if palette_bbox is None:
            return {
                "valid": False,
                "reason": "recorded palette color is not visible",
                "palette_bbox_xyxy": None,
                "projected_bbox_xyxy": (
                    list(projected_bbox) if projected_bbox is not None else None
                ),
            }
        if projected_bbox is None:
            return {
                "valid": allow_reviewed_without_projection,
                "reason": (
                    "named accepted binding permits the exact recorded palette mask"
                    if allow_reviewed_without_projection
                    else "governed object bounds cannot be projected for identity cross-check"
                ),
                "palette_bbox_xyxy": list(palette_bbox),
                "projected_bbox_xyxy": None,
            }
        px0, py0, px1, py1 = palette_bbox
        gx0, gy0, gx1, gy1 = projected_bbox
        intersection_width = max(0, min(px1, gx1) - max(px0, gx0))
        intersection_height = max(0, min(py1, gy1) - max(py0, gy0))
        intersection_area = intersection_width * intersection_height
        palette_area = max(1, (px1 - px0) * (py1 - py0))
        projected_area = max(1, (gx1 - gx0) * (gy1 - gy0))
        palette_covered_fraction = intersection_area / palette_area
        area_ratio = palette_area / projected_area
        center_x = (px0 + px1) / 2.0
        center_y = (py0 + py1) / 2.0
        margin_x = max(4.0, (gx1 - gx0) * 0.15)
        margin_y = max(4.0, (gy1 - gy0) * 0.15)
        center_consistent = bool(
            gx0 - margin_x <= center_x <= gx1 + margin_x
            and gy0 - margin_y <= center_y <= gy1 + margin_y
        )
        valid = bool(
            palette_covered_fraction >= 0.5
            and area_ratio <= 1.5
            and center_consistent
        )
        return {
            "valid": valid,
            "reason": (
                "palette mask and projected governed bounds agree"
                if valid
                else "palette mask conflicts with projected governed object bounds"
            ),
            "palette_bbox_xyxy": list(palette_bbox),
            "projected_bbox_xyxy": list(projected_bbox),
            "palette_covered_fraction": round(palette_covered_fraction, 8),
            "palette_to_projected_area_ratio": round(area_ratio, 8),
            "center_consistent": center_consistent,
        }

    @staticmethod
    def _padded_box(
        bbox: tuple[int, int, int, int],
        width: int,
        height: int,
        padding_fraction: float,
    ) -> tuple[int, int, int, int]:
        left, top, right, bottom = bbox
        padding = max(4, round(max(right - left, bottom - top) * padding_fraction))
        return (
            max(0, left - padding),
            max(0, top - padding),
            min(width, right + padding),
            min(height, bottom + padding),
        )

    @staticmethod
    def _visual_frequency(semantic_component: str) -> str:
        value = semantic_component.lower()
        if any(token in value for token in ("enclosure", "base_component", "body", "shell")):
            return "PRIMARY_FORM"
        if any(
            token in value
            for token in (
                "connector",
                "fastener",
                "status_led",
                "heat_sink_fin",
                "card_slot",
                "power_button",
            )
        ):
            return "TERTIARY_FORM"
        return "SECONDARY_FORM"

    @staticmethod
    def _component_region(binding: dict[str, Any], reference_id: str) -> dict[str, Any] | None:
        return next(
            (
                item
                for item in binding["record"].get("reference_regions", [])
                if str(item.get("reference_id")) == reference_id
            ),
            None,
        )

    def _history(self, binding_id: str, reference_id: str) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT id,status,render_run_id,created_at FROM visual_component_packets "
                "WHERE binding_id=? AND reference_id=? ORDER BY created_at,id",
                (binding_id, reference_id),
            ).fetchall()
        return [dict(row) for row in rows]

    def _packet_artifact_digests(self, packet: dict[str, Any]) -> set[str]:
        with self.project.connection() as connection:
            known = {
                str(row["digest"])
                for row in connection.execute("SELECT digest FROM artifacts").fetchall()
            }
        found: set[str] = set()

        def visit(value: Any) -> None:
            if isinstance(value, dict):
                for item in value.values():
                    visit(item)
            elif isinstance(value, list):
                for item in value:
                    visit(item)
            elif isinstance(value, str) and value in known:
                found.add(value)

        visit(packet)
        return found

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

    def _reference(self, reference_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM reference_items WHERE id=?", (reference_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown reference: {reference_id}")
        return dict(row)

    def _existing(
        self, binding_id: str, rig_id: str, render_run_id: str, reference_id: str
    ) -> dict[str, Any] | None:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_component_packets WHERE binding_id=? AND rig_id=? "
                "AND render_run_id=? AND reference_id=? ORDER BY created_at DESC,id DESC LIMIT 1",
                (binding_id, rig_id, render_run_id, reference_id),
            ).fetchone()
        return self._normalize(row) if row is not None else None

    def _get(self, packet_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_component_packets WHERE id=?", (packet_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual component packet: {packet_id}")
        return self._normalize(row)

    def _artifact_valid(self, digest: str) -> bool:
        try:
            path = self.artifacts.path_for(digest)
            return path.is_file() and sha256_file(path)[0] == digest
        except (OSError, TypeError, ValueError):
            return False

    def _receipt(self, packet: dict[str, Any]) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_component_task_packet",
            "id": packet["id"],
            "binding_id": packet["binding_id"],
            "rig_id": packet["rig_id"],
            "render_run_id": packet["render_run_id"],
            "reference_id": packet["reference_id"],
            "status": packet["status"],
            "packet": packet,
            "packet_sha256": hashlib.sha256(canonical_json(packet)).hexdigest(),
            "authority": packet["authority"],
            "created_at": packet["created_at"],
        }

    @staticmethod
    def _normalize(raw: Any) -> dict[str, Any]:
        value = dict(raw)
        value["packet"] = json.loads(value.pop("packet_json"))
        return value


class VisualFrequencyScoreStore:
    """Aggregate component-local evidence without letting large surfaces hide failures."""

    SCHEMA_VERSION = 1
    COMPONENT_PASS_THRESHOLD = 0.9

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)
        self.packets = ComponentTaskPacketStore(project)

    def create(
        self,
        *,
        scene_id: str,
        rig_id: str,
        packet_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        SceneStore(self.project).get(scene_id)
        bindings = SemanticBindingStore(self.project).list(scene_id)
        available_packets = self.packets.list(rig_id=rig_id)
        if packet_ids is not None:
            selected = set(packet_ids)
            unknown = sorted(selected - {item["id"] for item in available_packets})
            if unknown:
                raise KeyError(f"unknown or wrong-rig component packet ids: {unknown}")
            available_packets = [item for item in available_packets if item["id"] in selected]
        invalid_packet_ids = sorted(
            item["id"] for item in available_packets if not self.packets.verify(item["id"])["valid"]
        )
        valid_packets = [item for item in available_packets if item["id"] not in invalid_packet_ids]
        scorecard_id = str(uuid.uuid4())
        created_at = utc_now()
        scorecard = self._aggregate(
            scorecard_id=scorecard_id,
            scene_id=scene_id,
            rig_id=rig_id,
            bindings=bindings,
            packets=valid_packets,
            invalid_packet_ids=invalid_packet_ids,
            created_at=created_at,
        )
        receipt = self._receipt(scorecard)
        relative = Path("receipts") / f"visual-frequency-scorecard-{scorecard_id}.json"
        atomic_write_json(self.project.root / relative, receipt)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative,
            media_type="application/vnd.bvmcp.visual-frequency-scorecard+json",
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO visual_frequency_scorecards("
                "id,scene_id,rig_id,status,scorecard_json,receipt_digest,created_at) "
                "VALUES(?,?,?,?,?,?,?)",
                (
                    scorecard_id,
                    scene_id,
                    rig_id,
                    scorecard["status"],
                    json.dumps(scorecard),
                    artifact.digest,
                    created_at,
                ),
            )
        return {**scorecard, "receipt_digest": artifact.digest}

    def list(self, scene_id: str | None = None) -> list[dict[str, Any]]:
        with self.project.connection() as connection:
            rows = connection.execute(
                "SELECT * FROM visual_frequency_scorecards "
                + ("WHERE scene_id=? " if scene_id else "")
                + "ORDER BY created_at,id",
                (scene_id,) if scene_id else (),
            ).fetchall()
        return [self._normalize(row) for row in rows]

    def verify(self, scorecard_id: str) -> dict[str, Any]:
        try:
            scorecard = self._get(scorecard_id)
            receipt = json.loads(
                self.artifacts.path_for(scorecard["receipt_digest"]).read_text(encoding="utf-8")
            )
            packet_ids = scorecard["scorecard"]["inputs"]["packet_ids"]
            packets = [self.packets._get(packet_id) for packet_id in packet_ids]
            packet_valid = all(self.packets.verify(item["id"])["valid"] for item in packets)
            receipt_valid = bool(
                self._artifact_valid(scorecard["receipt_digest"])
                and canonical_json(receipt) == canonical_json(self._receipt(scorecard["scorecard"]))
                and scorecard["status"] == scorecard["scorecard"]["status"]
            )
            return {
                "valid": bool(receipt_valid and packet_valid),
                "receipt_valid": receipt_valid,
                "packet_valid": packet_valid,
            }
        except (KeyError, OSError, TypeError, ValueError, json.JSONDecodeError):
            return {"valid": False, "receipt_valid": False, "packet_valid": False}

    def _aggregate(
        self,
        *,
        scorecard_id: str,
        scene_id: str,
        rig_id: str,
        bindings: list[dict[str, Any]],
        packets: list[dict[str, Any]],
        invalid_packet_ids: list[str],
        created_at: str,
    ) -> dict[str, Any]:
        latest: dict[tuple[str, str], dict[str, Any]] = {}
        for row in packets:
            key = (row["binding_id"], row["reference_id"])
            previous = latest.get(key)
            if previous is None or (row["created_at"], row["id"]) > (
                previous["created_at"],
                previous["id"],
            ):
                latest[key] = row
        components = []
        for binding in bindings:
            component_packets = [
                item["packet"]
                for (binding_id, _reference_id), item in latest.items()
                if binding_id == binding["id"]
            ]
            scores = [
                self._quality_score(item["metrics"]["projection"]["metrics"])
                for item in component_packets
                if item["metrics"]["projection"].get("status") == "EVALUATED"
            ]
            packet_blockers = sorted(
                {blocker for item in component_packets for blocker in item.get("blockers", [])}
            )
            packets_acceptance_ready = bool(component_packets) and all(
                item.get("acceptance_status") == "PASS" for item in component_packets
            )
            score = min(scores) if scores else None
            frequency = ComponentTaskPacketStore._visual_frequency(
                binding["record"]["semantic_component"]
            )
            importance = float(binding["record"]["visual_importance"])
            components.append(
                {
                    "binding_id": binding["id"],
                    "object_name": binding["object_name"],
                    "semantic_component": binding["record"]["semantic_component"],
                    "binding_state": binding["state"],
                    "visual_frequency": frequency,
                    "visual_importance": importance,
                    "mandatory": importance >= 2.5,
                    "view_count": len(component_packets),
                    "evaluated_view_count": len(scores),
                    "score": round(score, 8) if score is not None else None,
                    "status": (
                        "PASS"
                        if score is not None
                        and score >= self.COMPONENT_PASS_THRESHOLD
                        and binding["state"] == "ACCEPTED_BOUND"
                        and packets_acceptance_ready
                        else "FAIL"
                        if score is not None and score < self.COMPONENT_PASS_THRESHOLD
                        else "BLOCKED"
                    ),
                    "blockers": [
                        *(
                            []
                            if binding["state"] == "ACCEPTED_BOUND"
                            else ["semantic binding is not ACCEPTED_BOUND"]
                        ),
                        *([] if scores else ["no reviewed component-mask evaluation exists"]),
                        *packet_blockers,
                    ],
                    "visible_area_px": sum(
                        int((item.get("crop") or {}).get("visible_area_px", 0))
                        for item in component_packets
                    ),
                }
            )
        scored = [item for item in components if item["score"] is not None]
        mandatory = [item for item in components if item["mandatory"]]
        frequencies = {}
        for level in ("PRIMARY_FORM", "SECONDARY_FORM", "TERTIARY_FORM"):
            level_components = [item for item in components if item["visual_frequency"] == level]
            level_scores = [item["score"] for item in level_components if item["score"] is not None]
            frequencies[level] = {
                "component_count": len(level_components),
                "scored_component_count": len(level_scores),
                "score": round(sum(level_scores) / len(level_scores), 8) if level_scores else None,
                "status": (
                    "PASS"
                    if level_components
                    and all(item["status"] == "PASS" for item in level_components)
                    else "BLOCKED"
                    if any(item["status"] == "BLOCKED" for item in level_components)
                    or not level_components
                    else "FAIL"
                ),
                "blockers": sorted(
                    {blocker for item in level_components for blocker in item["blockers"]}
                ),
            }
        scores = [float(item["score"]) for item in scored]
        semantic_mean = sum(scores) / len(scores) if scores else None
        worst_five = sorted(scored, key=lambda item: float(item["score"]))[:5]
        mandatory_scores = [float(item["score"]) for item in mandatory if item["score"] is not None]
        visible_area_score = self._weighted(scored, "visible_area_px")
        visual_importance_score = self._weighted(scored, "visual_importance")
        whole_object_score = self._whole_object_score(scene_id, rig_id)
        status = (
            "PASS"
            if components
            and not invalid_packet_ids
            and all(item["status"] == "PASS" for item in components)
            and all(item["status"] == "PASS" for item in frequencies.values())
            else "FAIL"
            if any(item["status"] == "FAIL" for item in components)
            else "BLOCKED"
        )
        return {
            "schema_version": self.SCHEMA_VERSION,
            "id": scorecard_id,
            "scene_id": scene_id,
            "rig_id": rig_id,
            "status": status,
            "component_pass_threshold": self.COMPONENT_PASS_THRESHOLD,
            "inputs": {
                "packet_ids": sorted(item["id"] for item in packets),
                "binding_ids": sorted(item["id"] for item in bindings),
                "invalid_packet_ids": invalid_packet_ids,
            },
            "scores": {
                "whole_object_score": whole_object_score,
                "semantic_component_weighted_mean": round(semantic_mean, 8)
                if semantic_mean is not None
                else None,
                "worst_five_components": [
                    {"binding_id": item["binding_id"], "score": item["score"]}
                    for item in worst_five
                ],
                "minimum_mandatory_component_score": round(min(mandatory_scores), 8)
                if mandatory_scores
                else None,
                "visible_area_score": visible_area_score,
                "visual_importance_weighted_score": visual_importance_score,
            },
            "visual_frequencies": frequencies,
            "components": components,
            "blockers": sorted(
                {blocker for item in components for blocker in item["blockers"]}
                | ({"one or more packet receipts are invalid"} if invalid_packet_ids else set())
            ),
            "authority": (
                "COMPONENT_WEIGHTED_VISUAL_ACCEPTANCE_EVIDENCE"
                if status == "PASS"
                else "INCOMPLETE_COMPONENT_VISUAL_EVIDENCE"
            ),
            "created_at": created_at,
        }

    def _whole_object_score(self, scene_id: str, rig_id: str) -> float | None:
        scorecards = VisualGeometryStore(self.project).list_scorecards(scene_id)
        values = [
            float(item["scorecard"]["projection"]["metrics"]["silhouette_iou"])
            for item in scorecards
            if item["rig_id"] == rig_id
            and VisualGeometryStore(self.project).verify_scorecard(item["id"])["valid"]
        ]
        return round(min(values), 8) if values else None

    @staticmethod
    def _quality_score(metrics: dict[str, Any]) -> float:
        boundary_quality = max(
            0.0,
            1.0 - float(metrics["boundary_rmse_fraction_of_diagonal"]) / 0.015,
        )
        return (
            0.5 * float(metrics["silhouette_iou"])
            + 0.1 * float(metrics["silhouette_dice"])
            + 0.1 * float(metrics["foreground_precision"])
            + 0.1 * float(metrics["foreground_recall"])
            + 0.2 * boundary_quality
        )

    @staticmethod
    def _weighted(items: list[dict[str, Any]], weight_key: str) -> float | None:
        weighted = [
            (float(item["score"]), float(item[weight_key]))
            for item in items
            if item["score"] is not None and float(item[weight_key]) > 0.0
        ]
        total = sum(weight for _score, weight in weighted)
        return (
            round(sum(score * weight for score, weight in weighted) / total, 8)
            if total > 0.0
            else None
        )

    def _receipt(self, scorecard: dict[str, Any]) -> dict[str, Any]:
        return {
            "schema_version": self.SCHEMA_VERSION,
            "receipt_type": "visual_frequency_scorecard",
            "id": scorecard["id"],
            "scene_id": scorecard["scene_id"],
            "rig_id": scorecard["rig_id"],
            "status": scorecard["status"],
            "scorecard": scorecard,
            "scorecard_sha256": hashlib.sha256(canonical_json(scorecard)).hexdigest(),
            "authority": scorecard["authority"],
            "created_at": scorecard["created_at"],
        }

    def _get(self, scorecard_id: str) -> dict[str, Any]:
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT * FROM visual_frequency_scorecards WHERE id=?", (scorecard_id,)
            ).fetchone()
        if row is None:
            raise KeyError(f"unknown visual-frequency scorecard: {scorecard_id}")
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
        value["scorecard"] = json.loads(value.pop("scorecard_json"))
        return value
