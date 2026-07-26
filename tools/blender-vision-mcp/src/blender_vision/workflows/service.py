from __future__ import annotations

import json
import uuid
from contextlib import suppress
from dataclasses import replace
from pathlib import Path
from typing import Any

from PIL import Image, ImageOps, ImageStat

from blender_vision.acceptance.receipts import export_receipt, verify_receipt
from blender_vision.artifacts.store import ArtifactStore
from blender_vision.blender.passes import MAXIMAL_VISUAL_RENDER_PASSES
from blender_vision.blender.runner import BlenderRunner
from blender_vision.cameras.solver import CameraSolver
from blender_vision.cameras.state import scaled_camera_state
from blender_vision.comparison.metrics import (
    BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION,
    compare_silhouettes,
)
from blender_vision.comparison.store import ComparisonStore
from blender_vision.core.errors import ProjectError
from blender_vision.core.models import EvidenceClass, RegistrationClass
from blender_vision.core.util import atomic_write_json, utc_now
from blender_vision.datasets.store import DatasetStore
from blender_vision.evidence.masks import ReferenceMaskStore
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.evidence.targets import TargetResolver
from blender_vision.features.ontology import FeatureType, TechnicalFeature
from blender_vision.features.store import FeatureStore
from blender_vision.geometry.gltf_validator import GlbValidator
from blender_vision.geometry.portfolio import ReconstructionPortfolioStore
from blender_vision.geometry.scenes import SceneStore
from blender_vision.geometry.semantic_graph import ROVER_COMPONENTS, SemanticTwinGraph
from blender_vision.parametric.components import ComponentSpec, ComponentType
from blender_vision.parametric.store import ComponentStore
from blender_vision.projects.store import ProjectStore
from blender_vision.repairs.mac_studio import propose_mac_studio_grille
from blender_vision.repairs.store import RepairStore
from blender_vision.security.paths import safe_filename


class ReconstructionService:
    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def import_scene(self, source: str) -> dict[str, Any]:
        return SceneStore(self.project).import_blend(Path(source))

    def import_reference(
        self, source: str, *, rights_state: str = "UNKNOWN", viewpoint_label: str | None = None
    ) -> dict[str, Any]:
        return ReferenceIngestor(self.project).import_file(
            Path(source), rights_state=rights_state, viewpoint_label=viewpoint_label
        )

    def inspect_scene(
        self, scene_id: str | None = None, *, job_id: str | None = None
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        result = BlenderRunner(self.project).run(
            "inspect_scene",
            Path(scene["absolute_path"]),
            {},
            job_id=job_id,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        SceneStore(self.project).set_inventory(scene["id"], result)
        relative = Path("scene") / f"{scene['id']}.inventory.json"
        atomic_write_json(self.project.root / relative, result)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative, media_type="application/vnd.bvmcp.scene-inventory+json"
        )
        return {"scene_id": scene["id"], "inventory": result, "artifact": artifact.to_dict()}

    def audit_scene(
        self, scene_id: str | None = None, *, job_id: str | None = None
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        result = BlenderRunner(self.project).run(
            "validate_scene",
            Path(scene["absolute_path"]),
            {},
            job_id=job_id,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        SceneStore(self.project).set_inventory(scene["id"], result["inventory"])
        relative = Path("scene") / f"{scene['id']}.audit.json"
        atomic_write_json(self.project.root / relative, result)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative, media_type="application/vnd.bvmcp.scene-audit+json"
        )
        return {
            "scene_id": scene["id"],
            "audit": result,
            "artifact": artifact.to_dict(),
            "path": str(relative),
        }

    def revise_rtx_5090_fe_candidate(
        self,
        source_revision: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a measured strict-v2 checkpoint without accepting the reconstruction."""
        source_revision = source_revision.strip()
        if not source_revision:
            raise ValueError("RTX 5090 FE candidate revision requires a source revision")
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "rtx_5090_fe":
            raise ProjectError("strict RTX revision requires an RTX 5090 FE benchmark project")
        governed_revision = str(metadata.get("source_revision", "")).strip()
        if governed_revision and governed_revision != source_revision:
            raise ProjectError(
                "requested source revision does not match the benchmark bootstrap provenance"
            )
        scene = SceneStore(self.project).get(scene_id)
        revision_id = str(uuid.uuid4())
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"rtx-5090-fe-strict-v2-{revision_id}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "revise_rtx_5090_fe_candidate",
            Path(scene["absolute_path"]),
            {
                "output_path": str(output),
                "source_revision": source_revision,
                "tolerance_mm": 0.25,
            },
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"rtx-5090-fe-strict-v2-{revision_id}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": (
                "Strict-v2 is a dimensional candidate; visual residuals, feature/material review, "
                "and named human acceptance remain pending."
            ),
        }

    def refine_rtx_5090_fe_visual_candidate(
        self,
        source_revision: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create an audit-clean power-frame RTX checkpoint without accepting it."""
        source_revision = source_revision.strip()
        if not source_revision:
            raise ValueError("RTX visual candidate refinement requires a source revision")
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "rtx_5090_fe":
            raise ProjectError("RTX visual refinement requires an RTX 5090 FE benchmark project")
        governed_revision = str(metadata.get("source_revision", "")).strip()
        if governed_revision and governed_revision != source_revision:
            raise ProjectError(
                "requested source revision does not match the benchmark bootstrap provenance"
            )
        scene = SceneStore(self.project).get(scene_id)
        revision_id = str(uuid.uuid4())
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"rtx-5090-fe-strict-v16-audit-clean-power-frame-{revision_id}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "refine_rtx_5090_fe_visual_candidate",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "source_revision": source_revision},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output,
            original_name=f"rtx-5090-fe-strict-v16-audit-clean-power-frame-{revision_id}.blend",
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": (
                "Strict-v16 makes the recessed power frame watertight; residual and named "
                "human review gates remain pending."
            ),
        }

    def refine_rtx_5090_fe_front_frame_candidate(
        self,
        source_revision: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a dimension-locked RTX front X-frame checkpoint without accepting it."""
        source_revision = source_revision.strip()
        if not source_revision:
            raise ValueError("RTX front-frame refinement requires a source revision")
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "rtx_5090_fe":
            raise ProjectError("RTX front-frame refinement requires an RTX benchmark project")
        governed_revision = str(metadata.get("source_revision", "")).strip()
        if governed_revision and governed_revision != source_revision:
            raise ProjectError(
                "requested source revision does not match the benchmark bootstrap provenance"
            )
        scene = SceneStore(self.project).get(scene_id)
        revision_id = str(uuid.uuid4())
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"rtx-5090-fe-strict-v17-front-x-frame-{revision_id}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "refine_rtx_5090_fe_front_frame_candidate",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "source_revision": source_revision},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output,
            original_name=f"rtx-5090-fe-strict-v17-front-x-frame-{revision_id}.blend",
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": (
                "Strict-v17 reconstructs the evidence-visible front X frame; fresh residual and "
                "named human review gates remain pending."
            ),
        }

    def refine_dgx_spark_visual_candidate(
        self,
        source_revision: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a dimension-locked DGX appearance and rear-I/O checkpoint."""
        source_revision = source_revision.strip()
        if not source_revision:
            raise ValueError("DGX Spark visual refinement requires a source revision")
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "dgx_spark":
            raise ProjectError("DGX visual refinement requires a DGX Spark benchmark project")
        governed_revision = str(metadata.get("source_revision", "")).strip()
        if governed_revision and governed_revision != source_revision:
            raise ProjectError(
                "requested source revision does not match the benchmark bootstrap provenance"
            )
        scene = SceneStore(self.project).get(scene_id)
        revision_id = str(uuid.uuid4())
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"dgx-spark-visual-v16-front-foam-response-{revision_id}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "refine_dgx_spark_visual_candidate",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "source_revision": source_revision},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output,
            original_name=f"dgx-spark-visual-v16-front-foam-response-{revision_id}.blend",
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": (
                "Visual-v16 preserves the inset and black-response corrections, calibrates the "
                "layered foam field toward the dark front reference, and "
                "retains clean proxy topology, governed aperture evidence, rear-I/O detail, "
                "and the "
                "measured DGX body envelope; residual and named human review gates remain pending."
            ),
        }

    def refine_dgx_spark_base_foot_candidate(
        self,
        source_revision: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create a body-locked DGX checkpoint with an evidence-sized recessed foot."""
        source_revision = source_revision.strip()
        if not source_revision:
            raise ValueError("DGX Spark base-foot refinement requires a source revision")
        metadata = self.project.project().get("metadata", {})
        if metadata.get("benchmark") != "dgx_spark":
            raise ProjectError("DGX base-foot refinement requires a DGX Spark benchmark project")
        governed_revision = str(metadata.get("source_revision", "")).strip()
        if governed_revision and governed_revision != source_revision:
            raise ProjectError(
                "requested source revision does not match the benchmark bootstrap provenance"
            )
        scene = SceneStore(self.project).get(scene_id)
        revision_id = str(uuid.uuid4())
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"dgx-spark-visual-v17-recessed-base-foot-{revision_id}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "refine_dgx_spark_base_foot_candidate",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "source_revision": source_revision},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output,
            original_name=f"dgx-spark-visual-v17-recessed-base-foot-{revision_id}.blend",
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": (
                "Visual-v17 changes only the recessed base foot below the measured body; fresh "
                "multi-view residuals and named human review gates remain pending."
            ),
        }

    def propose_mac_studio_grille(self) -> dict[str, Any]:
        return propose_mac_studio_grille(self.project)

    def approve_repair(self, proposal_id: str, approved_by: str) -> dict[str, Any]:
        return RepairStore(self.project).approve(proposal_id, approved_by)

    def review_repair(
        self,
        proposal_id: str,
        *,
        accepted: bool,
        reviewer: str,
        reason: str,
        receipt_id: str | None = None,
    ) -> dict[str, Any]:
        """Accept only checkpoint geometry whose receipt has no blocker beyond review itself."""
        evidence: dict[str, Any] = {}
        if accepted:
            if not receipt_id:
                raise ValueError("accepted repair reviews require a post-apply receipt id")
            proposal = RepairStore(self.project).get(proposal_id)
            if proposal["status"] != "applied":
                raise ProjectError(f"repair proposal is not awaiting review: {proposal_id}")
            result = proposal.get("result") or {}
            worker = result.get("worker") or {}
            topology = worker.get("topology") or {}
            dimensional_checks = worker.get("dimensional_checks") or {}
            ray_validation = worker.get("ray_validation") or {}
            validation_gates = {
                "checkpoint_audit_valid": bool(
                    (result.get("audit") or {}).get("audit", {}).get("valid")
                ),
                "single_manifold_component": (
                    topology.get("connected_components") == 1
                    and topology.get("non_manifold_edges") == 0
                ),
                "one_tunnel_per_aperture": (
                    topology.get("closed_surface_genus") == worker.get("generated_hole_count")
                ),
                "ray_open_fraction": float(ray_validation.get("open_fraction", 0.0))
                >= float(proposal["config"].get("minimum_open_fraction", 1.0)),
                "dimensions_within_tolerance": bool(dimensional_checks)
                and all(bool(value) for value in dimensional_checks.values()),
                "review_render_usable": (
                    (result.get("rear_render") or {})
                    .get("pixel_evidence", {})
                    .get("distinct_values", 0)
                    >= 32
                ),
            }
            failed_gates = sorted(name for name, passed in validation_gates.items() if not passed)
            if failed_gates:
                raise ProjectError(
                    "repair checkpoint fails final validation gates: " + ", ".join(failed_gates)
                )
            with self.project.connection() as connection:
                receipt_row = connection.execute(
                    "SELECT digest FROM receipts WHERE id=?", (receipt_id,)
                ).fetchone()
            if receipt_row is None:
                raise ProjectError(f"unknown acceptance receipt: {receipt_id}")
            receipt_path = self.artifacts.path_for(receipt_row["digest"])
            verification = verify_receipt(receipt_path, project=self.project)
            if not verification["valid"]:
                raise ProjectError(f"acceptance receipt failed verification: {receipt_id}")
            envelope = json.loads(receipt_path.read_text(encoding="utf-8"))
            receipt_acceptance = envelope["payload"]["acceptance"]
            blockers = set(receipt_acceptance.get("blockers", []))
            review_blocker = "L3+ applied repairs still require acceptance evidence"
            unaccepted_ids = (
                receipt_acceptance.get("metrics", {})
                .get("repairs", {})
                .get("unaccepted_applied_ids", [])
            )
            receipt_ready = bool(receipt_acceptance.get("accepted")) or (
                blockers == {review_blocker} and unaccepted_ids == [proposal_id]
            )
            if not receipt_ready:
                detail = "; ".join(receipt_acceptance.get("blockers", [])) or "unknown blockers"
                raise ProjectError(
                    "receipt does not isolate final repair review as its only blocker: " + detail
                )
            evidence = {
                "receipt_id": receipt_id,
                "receipt_artifact_digest": receipt_row["digest"],
                "receipt_payload_sha256": envelope["payload_sha256"],
                "receipt_verification": verification,
                "validation_gates": validation_gates,
            }
        return RepairStore(self.project).review_applied(
            proposal_id,
            accepted=accepted,
            reviewer=reviewer,
            reason=reason,
            receipt_id=receipt_id,
            evidence=evidence,
        )

    def apply_repair(
        self,
        proposal_id: str,
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        proposal = RepairStore(self.project).get(proposal_id)
        if proposal["status"] != "approved":
            raise ValueError(f"repair proposal is not approved: {proposal_id}")
        if proposal["kind"] != "mac_studio_rear_hero_grille":
            raise ValueError(f"unsupported repair kind: {proposal['kind']}")
        scene = SceneStore(self.project).get(scene_id)
        output = self.project.root / "scene" / "checkpoints" / f"{proposal_id}.blend"
        parameters = {**proposal["config"], "output_path": str(output)}
        worker_result = BlenderRunner(self.project).run(
            "repair_mac_studio_grille",
            Path(scene["absolute_path"]),
            parameters,
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"mac-studio-grille-repair-{proposal_id}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        render_path = self.project.root / "renders" / f"repair-{proposal_id}-rear.png"
        render = BlenderRunner(self.project).run(
            "render_passes",
            output,
            {
                "output_path": str(render_path),
                "width": 1024,
                "height": 1024,
                "horizontal_fov_degrees": 42.0,
                "view_direction": [0.0, -1.0, -0.03],
                "review_lighting": True,
                "review_exposure": -2.5,
            },
            job_id=f"{job_id}-render" if job_id else None,
            timeout_seconds=900,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        render_artifact = self.artifacts.ingest_file(render_path, media_type="image/png")
        with Image.open(render_path) as image:
            rgba = image.convert("RGBA")
            grayscale = rgba.convert("L")
            alpha = rgba.getchannel("A")
            values = [
                value
                for value, visible in zip(grayscale.tobytes(), alpha.tobytes(), strict=True)
                if visible
            ]
        pixel_evidence = {
            "visible_pixels": len(values),
            "minimum": min(values) if values else None,
            "maximum": max(values) if values else None,
            "mean": ImageStat.Stat(grayscale, mask=alpha).mean[0] if values else None,
            "distinct_values": len(set(values)),
            "clipped_black_fraction": (
                sum(value == 0 for value in values) / len(values) if values else None
            ),
            "clipped_white_fraction": (
                sum(value == 255 for value in values) / len(values) if values else None
            ),
        }
        if not values or pixel_evidence["distinct_values"] < 32:
            raise RuntimeError("repair review render has no usable tonal range")
        if (
            pixel_evidence["clipped_black_fraction"] > 0.25
            or pixel_evidence["clipped_white_fraction"] > 0.25
        ):
            raise RuntimeError(
                "repair review render is exposure-clipped and cannot serve as visual evidence"
            )
        evidence_ids = [
            binding["id"]
            for binding in proposal["evidence"]
            if binding.get("kind") == "measurement"
        ]
        feature = TechnicalFeature(
            id=f"mac-hero-grille-{proposal_id}",
            type=FeatureType.GRILLE,
            parent_component=f"mac-hero-grille-component-{proposal_id}",
            coordinate_frame="mac-studio body-local; canonical millimetres",
            observations=[
                {
                    "kind": "generated_topology",
                    "topology": worker_result["topology"],
                    "ray_validation": worker_result["ray_validation"],
                    "render_digest": render_artifact.digest,
                }
            ],
            reference_ids=[],
            confidence=0.75,
            evidence_class=EvidenceClass.SINGLE_VIEW_OBSERVED,
            uncertainty={
                "source_reference_available": False,
                "human_review_required": True,
            },
            dimensions={
                "width_mm": proposal["config"]["field_width_mm"],
                "height_mm": proposal["config"]["field_height_mm"],
                "pitch_mm": proposal["config"]["pitch_mm"],
                "hole_diameter_mm": proposal["config"]["diameter_mm"],
            },
            model_revision=generated_scene["artifact"]["digest"],
            human_approval=False,
            coverage_group="rear_grille",
            hero_surface=True,
            provenance=proposal["evidence"],
        )
        feature_store = FeatureStore(self.project)
        stored_feature = feature_store.add(feature)
        with suppress(KeyError):
            feature_store.supersede(
                "mac-studio-rear-grille",
                replacement_id=feature.id,
                reason="Revisioned upper hero-grille candidate replaces the lower-band proxy.",
            )
        component = ComponentStore(self.project).create(
            ComponentSpec(
                id=f"mac-hero-grille-component-{proposal_id}",
                type=ComponentType.GRILLE,
                parameters=proposal["config"],
                evidence_bindings=evidence_ids,
                material_slots=["mac-rear-perf-alu", "BVMCP_rear-interior-dark"],
                lod_rules={"hero": "physical", "web": "geometry_nodes_or_normal_map"},
            )
        )
        result = {
            "proposal_id": proposal_id,
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker_result,
            "audit": audit,
            "rear_render": {
                **render,
                "artifact": render_artifact.to_dict(),
                "pixel_evidence": pixel_evidence,
            },
            "feature": stored_feature,
            "component": component,
            "acceptance": {
                "accepted": False,
                "state": "awaiting_final_review",
                "reason": (
                    "development repair requires recovered raw reference, metric camera, residual "
                    "comparison, and human approval"
                ),
                "evaluation_authorized_by": proposal["approved_by"],
                "evaluation_authorized_at": proposal["approved_at"],
            },
        }
        RepairStore(self.project).mark_applied(proposal_id, result)
        return result

    def generate_components(
        self,
        component_ids: list[str],
        *,
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        if not component_ids or len(set(component_ids)) != len(component_ids):
            raise ValueError("component generation requires unique component ids")
        store = ComponentStore(self.project)
        components = [store.get(component_id) for component_id in component_ids]
        scene = SceneStore(self.project).get(scene_id)
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = self.project.root / "scene" / "checkpoints" / f"components-{token}.blend"
        worker = BlenderRunner(self.project).run(
            "generate_components",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "components": components},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        if worker.get("failed_constraints"):
            raise RuntimeError("generated components fail hard parametric constraints")
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"component-checkpoint-{token}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        if not audit["audit"]["valid"]:
            raise RuntimeError(
                "generated component checkpoint failed the authoritative scene audit"
            )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "component_ids": component_ids,
            "accepted": False,
            "reason": "generated checkpoint requires visual and evidence review",
        }

    def generate_component_variant(
        self,
        component: dict[str, Any],
        *,
        scene_id: str,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Generate one isolated component checkpoint without mutating component authority."""
        component_id = str(component.get("id", "")).strip()
        if not component_id or not isinstance(component.get("parameters"), dict):
            raise ValueError("component variant requires an identified parameter record")
        scene = SceneStore(self.project).get(scene_id)
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"component-variant-{component_id}-{token}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "update_component",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "component": component},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        if worker.get("failed_constraints"):
            raise RuntimeError("component variant fails hard parametric constraints")
        generated_scene = SceneStore(self.project).register_generated(
            output,
            original_name=f"component-variant-{component_id}-{token}.blend",
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        if not audit["audit"]["valid"]:
            raise RuntimeError("component variant failed the authoritative scene audit")
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "component": component,
            "worker": worker,
            "audit": audit,
            "accepted": False,
            "reason": "isolated search candidate; transactional evaluation remains required",
        }

    def generate_parametric_seed(
        self,
        *,
        portfolio_id: str,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Generate an editable, dimension-bound seed without a private starting model."""
        with self.project.connection() as connection:
            measurement_rows = connection.execute(
                "SELECT id,value_json FROM measurements "
                "WHERE type='known_overall_dimension' "
                "AND evidence_class IN ('MEASURED','MANUFACTURER_SPEC') ORDER BY created_at"
            ).fetchall()
            reference_ids = [
                row[0]
                for row in connection.execute(
                    "SELECT id FROM reference_items WHERE media_type LIKE 'image/%'"
                )
            ]
            target_row = connection.execute(
                "SELECT id FROM target_resolutions ORDER BY rowid DESC LIMIT 1"
            ).fetchone()
            semantic_root_row = connection.execute(
                "SELECT record_json FROM semantic_nodes "
                "WHERE node_type='digital_twin_root' ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
            semantic_rows = connection.execute(
                "SELECT id,node_type FROM semantic_nodes ORDER BY created_at,id"
            ).fetchall()
        dimensions: dict[str, float] = {}
        dimension_ids: list[str] = []
        for row in measurement_rows:
            value = json.loads(row["value_json"])
            if value.get("axis") in {"x", "y", "z"}:
                dimensions[value["axis"]] = float(value["millimetres"])
                dimension_ids.append(row["id"])
        if set(dimensions) != {"x", "y", "z"}:
            raise ProjectError("parametric seed requires governed x, y, and z overall dimensions")
        if target_row is None:
            raise ProjectError("parametric seed requires a resolved target")
        try:
            target = TargetResolver(self.project).get(target_row["id"])["target"]
        except ValueError as error:
            raise ProjectError(
                "parametric seed requires valid canonical target authority"
            ) from error
        target_text = json.dumps(target, sort_keys=True).lower()
        category = (
            json.loads(semantic_root_row["record_json"])["parameters"].get("category")
            if semantic_root_row
            else "general_product"
        )
        if (
            semantic_root_row
            and category in {"vehicles", "space_rovers"}
            and "rover" in target_text
        ):
            semantic_root = json.loads(semantic_root_row["record_json"])
            SemanticTwinGraph(self.project).ensure_component_nodes(
                semantic_root["id"], ROVER_COMPONENTS
            )
            with self.project.connection() as connection:
                semantic_rows = connection.execute(
                    "SELECT id,node_type FROM semantic_nodes ORDER BY created_at,id"
                ).fetchall()
        component_specs = self._seed_component_specs(
            dimensions,
            category=category,
            target=target,
            evidence_bindings=dimension_ids,
        )
        with self.project.connection() as connection:
            existing_component_ids = {
                row[0] for row in connection.execute("SELECT id FROM components").fetchall()
            }
        if existing_component_ids.intersection(spec.id for spec in component_specs):
            revision = f"rev_{uuid.uuid4().hex[:8]}"
            component_specs = [
                replace(spec, id=f"{spec.id}_{revision}") for spec in component_specs
            ]
        component_store = ComponentStore(self.project)
        stored_components = [component_store.create(spec) for spec in component_specs]
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = self.project.root / "scene" / "checkpoints" / f"semantic-seed-{token}.blend"
        worker = BlenderRunner(self.project).run(
            "generate_semantic_seed",
            self.project.project_file,
            {"output_path": str(output), "components": stored_components},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"semantic-parametric-seed-{token}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        if not audit["audit"]["valid"]:
            raise RuntimeError("parametric semantic seed failed the authoritative scene audit")
        candidate = next(
            (
                item
                for item in ReconstructionPortfolioStore(self.project).list_candidates(
                    portfolio_id
                )
                if item["lane"] == "parametric_category_model"
            ),
            None,
        )
        if candidate is None:
            raise ProjectError("portfolio has no parametric category candidate")
        candidate = ReconstructionPortfolioStore(self.project).record_result(
            candidate["id"],
            metrics={
                "dimensional_support": 1.0,
                "semantic_editability": 1.0,
                "coverage": min(1.0, len(reference_ids) / 24.0),
                "camera": 0.0,
                "silhouette": 0.0,
            },
            artifacts=[generated_scene["artifact"]["digest"]],
            scene_id=generated_scene["id"],
        )
        body_ids = [
            spec.id
            for spec in component_specs
            if spec.id == "seed_body" or spec.id.startswith("seed_body_rev_")
        ]
        mast_ids = [spec.id for spec in component_specs if "mast" in spec.id]
        object_names_by_kind = {
            "body_shell": body_ids,
            "chassis": body_ids,
            "instrument_mast": mast_ids,
            "rocker_bogie_suspension": [
                spec.id for spec in component_specs if "rocker_bogie" in spec.id
            ],
            "robotic_arm": [spec.id for spec in component_specs if "robotic_arm" in spec.id],
            "navigation_camera": [
                spec.id for spec in component_specs if "navigation_camera" in spec.id
            ],
            "hazard_camera": [
                spec.id for spec in component_specs if "hazard_camera" in spec.id
            ],
            "sample_caching_system": [
                spec.id for spec in component_specs if "sample_caching" in spec.id
            ],
            "power_system": [
                spec.id for spec in component_specs if "power_source" in spec.id
            ],
            "radioisotope_power_source": [
                spec.id for spec in component_specs if "power_source" in spec.id
            ],
            "high_gain_antenna": [
                spec.id for spec in component_specs if "high_gain_antenna" in spec.id
            ],
            "underbody": [
                spec.id for spec in component_specs if spec.type == ComponentType.PANEL
            ],
            "wheel": [
                spec.id
                for spec in component_specs
                if spec.type == ComponentType.TIRE_PROFILE
            ],
        }
        graph = SemanticTwinGraph(self.project)
        semantic_bindings = []
        for row in semantic_rows:
            names = object_names_by_kind.get(row["node_type"], [])
            if names:
                semantic_bindings.append(
                    graph.bind(
                        row["id"],
                        scene_id=generated_scene["id"],
                        object_names=names,
                        reference_ids=reference_ids,
                        component_ids=names,
                        confidence=0.35,
                    )
                )
        return {
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "candidate": candidate,
            "components": stored_components,
            "semantic_bindings": semantic_bindings,
            "accepted": False,
            "evidence_authority": "INFERRED_PARAMETRIC_HYPOTHESIS",
            "reason": "dimension-bound editable seed requires multiview fitting and review",
        }

    @staticmethod
    def _seed_component_specs(
        dimensions: dict[str, float],
        *,
        category: str,
        target: dict[str, Any],
        evidence_bindings: list[str],
    ) -> list[ComponentSpec]:
        x, y, z = (dimensions[axis] for axis in ("x", "y", "z"))
        vehicle_like = category in {"vehicles", "space_rovers"}
        body_height = z * (0.28 if vehicle_like else 0.85)
        wheel_radius = z * 0.12
        body_z = wheel_radius * 1.2 + body_height / 2.0
        specs = [
            ComponentSpec(
                id="seed_body",
                type=ComponentType.BODY,
                parameters={
                    "dimensions_mm": [x * 0.68, y * 0.68, body_height],
                    "location_mm": [0.0, 0.0, body_z],
                    "bevel_mm": min(x, y, z) * 0.025,
                    "metallic": 0.45,
                    "roughness": 0.38,
                },
                evidence_bindings=evidence_bindings,
                lod_rules={"authority": "overall-dimension seed only"},
            )
        ]
        if not vehicle_like:
            return specs
        specs.append(
            ComponentSpec(
                id="seed_underbody",
                type=ComponentType.PANEL,
                parameters={
                    "dimensions_mm": [x * 0.72, y * 0.6, max(10.0, z * 0.015)],
                    "location_mm": [0.0, 0.0, wheel_radius * 1.05],
                    "rows": 2,
                    "columns": 4,
                },
                evidence_bindings=evidence_bindings,
            )
        )
        wheel_count = 6 if "rover" in json.dumps(target).lower() else 4
        wheel_outer_radius = wheel_radius * 1.28
        x_positions = (
            [-x / 2.0 + wheel_outer_radius, 0.0, x / 2.0 - wheel_outer_radius]
            if wheel_count == 6
            else [-x / 2.0 + wheel_outer_radius, x / 2.0 - wheel_outer_radius]
        )
        section_radius = wheel_radius * 0.28
        for side, y_position in (
            ("left", -y / 2.0 + section_radius),
            ("right", y / 2.0 - section_radius),
        ):
            for index, x_position in enumerate(x_positions, start=1):
                specs.append(
                    ComponentSpec(
                        id=f"seed_wheel_{side}_{index}",
                        type=ComponentType.TIRE_PROFILE,
                        parameters={
                            "radius_mm": wheel_radius,
                            "section_radius_mm": section_radius,
                            "location_mm": [x_position, y_position, wheel_outer_radius],
                            "axis": "y",
                            "metallic": 0.65,
                            "roughness": 0.42,
                        },
                        evidence_bindings=evidence_bindings,
                    )
                )
        specs.append(
            ComponentSpec(
                id="seed_mast",
                type=ComponentType.BODY,
                parameters={
                    "dimensions_mm": [x * 0.06, y * 0.06, z * 0.58],
                    "location_mm": [0.0, 0.0, z * 0.71],
                    "bevel_mm": min(x, y, z) * 0.006,
                    "metallic": 0.55,
                    "roughness": 0.35,
                },
                evidence_bindings=evidence_bindings,
                lod_rules={"authority": "overall-height seed only"},
            )
        )
        if "rover" in json.dumps(target).lower():
            for side, y_position in (("left", -y * 0.31), ("right", y * 0.31)):
                specs.append(
                    ComponentSpec(
                        id=f"seed_rocker_bogie_{side}",
                        type=ComponentType.SWEEP,
                        parameters={
                            "control_points_mm": [
                                [-x * 0.36, 0.0, 0.0],
                                [0.0, 0.0, z * 0.08],
                                [x * 0.36, 0.0, 0.0],
                            ],
                            "location_mm": [0.0, y_position, z * 0.19],
                            "profile_radius_mm": max(12.0, z * 0.012),
                            "metallic": 0.7,
                            "roughness": 0.4,
                        },
                        evidence_bindings=evidence_bindings,
                    )
                )
            specs.extend(
                [
                    ComponentSpec(
                        id="seed_robotic_arm",
                        type=ComponentType.SWEEP,
                        parameters={
                            "control_points_mm": [
                                [0.0, 0.0, 0.0],
                                [x * 0.2, -y * 0.08, z * 0.1],
                                [x * 0.32, -y * 0.12, -z * 0.04],
                            ],
                            "location_mm": [-x * 0.18, -y * 0.12, z * 0.42],
                            "profile_radius_mm": max(15.0, z * 0.015),
                            "metallic": 0.7,
                            "roughness": 0.35,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_navigation_camera",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.09, y * 0.08, z * 0.05],
                            "location_mm": [0.0, 0.0, z * 0.9],
                            "bevel_mm": min(x, y, z) * 0.004,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_hazard_camera_front",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.05, y * 0.035, z * 0.035],
                            "location_mm": [x * 0.3, -y * 0.28, z * 0.37],
                            "bevel_mm": min(x, y, z) * 0.002,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_hazard_camera_rear",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.05, y * 0.035, z * 0.035],
                            "location_mm": [-x * 0.3, y * 0.28, z * 0.37],
                            "bevel_mm": min(x, y, z) * 0.002,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_sample_caching",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.22, y * 0.16, z * 0.12],
                            "location_mm": [x * 0.08, 0.0, z * 0.45],
                            "bevel_mm": min(x, y, z) * 0.008,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_radioisotope_power_source",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.18, y * 0.14, z * 0.12],
                            "location_mm": [-x * 0.16, y * 0.1, z * 0.5],
                            "bevel_mm": min(x, y, z) * 0.01,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                    ComponentSpec(
                        id="seed_high_gain_antenna",
                        type=ComponentType.BODY,
                        parameters={
                            "dimensions_mm": [x * 0.2, y * 0.025, z * 0.14],
                            "location_mm": [x * 0.14, 0.0, z * 0.74],
                            "bevel_mm": min(x, y, z) * 0.004,
                        },
                        evidence_bindings=evidence_bindings,
                    ),
                ]
            )
        return specs

    def generate_synthetic_dataset(
        self, dataset_id: str, *, job_id: str | None = None
    ) -> dict[str, Any]:
        store = DatasetStore(self.project)
        dataset = store.get(dataset_id)
        if dataset["status"] != "planned":
            raise ValueError(f"dataset is not awaiting generation: {dataset_id}")
        manifest = dataset["manifest"]
        scene = SceneStore(self.project).get(manifest["scene_id"])
        output_dir = self.project.root / "training" / "datasets" / dataset_id / "rendered"
        total_samples = int(manifest["sample_count"])
        batches = []
        for sample_start in range(0, total_samples, 64):
            batch_count = min(64, total_samples - sample_start)
            worker = BlenderRunner(self.project).run(
                "generate_synthetic_dataset",
                Path(scene["absolute_path"]),
                {
                    "output_dir": str(output_dir),
                    "sample_count": batch_count,
                    "sample_start": sample_start,
                    "seed": int(manifest["seed"]),
                    "domain_randomization": manifest["domain_randomization"],
                    "width": 256,
                    "height": 256,
                },
                job_id=f"{job_id}-{sample_start}" if job_id else None,
                timeout_seconds=max(900, batch_count * 120),
                cancelled=(
                    (lambda: self.project.cancellation_requested(job_id)) if job_id else None
                ),
            )
            batches.append(worker)
        worker_files = {item["path"]: item for batch in batches for item in batch["files"]}
        artifacts = [
            self.artifacts.ingest_file(self.project.root / item["path"]).to_dict()
            for item in worker_files.values()
        ]
        generated = store.mark_generated(
            dataset_id,
            artifact_digests=[item["digest"] for item in artifacts],
            sample_count=total_samples,
        )
        return {
            "dataset": generated,
            "worker": {
                "sample_count": total_samples,
                "batch_count": len(batches),
                "batches": batches,
                "files": list(worker_files.values()),
                "outputs": [
                    "beauty",
                    "instance_mask",
                    "feature_mask",
                    "depth",
                    "normals",
                    "keypoints",
                    "dimensions_mm",
                    "feature_ids",
                    "materials",
                    "lighting",
                    "occlusion",
                    "pose",
                    "orientation",
                    "visible_fraction",
                    "cross_view_identity",
                ],
                "network_used": False,
            },
            "artifacts": artifacts,
            "commercial_eligible": True,
        }

    def solve_cameras(
        self,
        *,
        backend: str = "heuristic-pinhole",
        reference_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        selected = "turntable" if backend == "heuristic-pinhole" else backend
        document = CameraSolver(self.project).solve(selected, reference_ids=reference_ids)
        relative = Path(document["path"])
        artifact = self.artifacts.ingest_file(
            self.project.root / relative, media_type="application/vnd.bvmcp.camera-solution+json"
        )
        return {**document, "artifact": artifact.to_dict()}

    def _camera_solution(self, solution_id: str | None = None) -> dict[str, Any]:
        with self.project.connection() as connection:
            if solution_id:
                row = connection.execute(
                    "SELECT * FROM camera_solutions WHERE id=?", (solution_id,)
                ).fetchone()
            else:
                row = connection.execute(
                    "SELECT * FROM camera_solutions ORDER BY created_at DESC LIMIT 1"
                ).fetchone()
        if row is None:
            raise FileNotFoundError("project has no camera solution")
        document = json.loads(row["solution_json"])
        if isinstance(document, list):
            document = {"id": row["id"], "cameras": document}
        return {**dict(row), **document}

    def render_views(
        self,
        *,
        scene_id: str | None = None,
        solution_id: str | None = None,
        job_id: str | None = None,
        maximum_dimension: int = 1024,
        reference_ids: list[str] | None = None,
        requested_passes: list[str] | None = None,
        regions_by_reference: dict[str, dict[str, int]] | None = None,
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        camera_record = self._camera_solution(solution_id)
        requested = set(reference_ids or [])
        cameras = [
            camera
            for camera in camera_record["cameras"]
            if not requested or camera["reference_id"] in requested
        ]
        if requested - {camera["reference_id"] for camera in cameras}:
            raise ValueError("local render references are not covered by the camera solution")
        if not cameras:
            raise ValueError("render requires at least one covered camera")
        allowed_passes = MAXIMAL_VISUAL_RENDER_PASSES
        selected_passes = list(dict.fromkeys(requested_passes or sorted(allowed_passes)))
        if not selected_passes or set(selected_passes) - allowed_passes:
            raise ValueError("render requested unsupported or empty evidence passes")
        if "beauty" not in selected_passes:
            selected_passes.insert(0, "beauty")
        regions_by_reference = regions_by_reference or {}
        render_run_id = str(uuid.uuid4())
        renders: list[dict[str, Any]] = []
        for index, solution in enumerate(cameras):
            camera_state = scaled_camera_state(solution, maximum_dimension)
            width = int(camera_state["width"])
            height = int(camera_state["height"])
            intrinsics = camera_state["intrinsics"]
            legacy_shift_x = 0.5 - float(intrinsics.get("cx", width / 2.0)) / width
            legacy_shift_y = (float(intrinsics.get("cy", height / 2.0)) - height / 2.0) / width
            output = (
                self.project.root
                / "renders"
                / (
                    f"{solution['reference_id']}_{camera_record['id']}_"
                    f"{scene['id']}_{render_run_id}.png"
                )
            )
            result = BlenderRunner(self.project).run(
                "render_passes",
                Path(scene["absolute_path"]),
                {
                    "output_path": str(output),
                    "width": width,
                    "height": height,
                    "camera_state": camera_state,
                    # Redundant diagnostics retained for older workers. Exact
                    # workers ignore them whenever camera_state is present.
                    "lens_shift_x": legacy_shift_x,
                    "lens_shift_y": legacy_shift_y,
                    "camera_roll_degrees": float(
                        solution.get("diagnostics", {}).get("camera_roll_degrees", 0.0)
                    ),
                    "review_lighting": True,
                    "review_exposure": -0.5,
                    "evidence_passes": True,
                    "governed_validation": True,
                    "requested_passes": selected_passes,
                    "crop_roi_px": regions_by_reference.get(solution["reference_id"]),
                },
                job_id=f"{job_id or uuid.uuid4()}-{index}",
                cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
            )
            render_path = self.project.root / result["render_path"]
            artifact = self.artifacts.ingest_file(render_path, media_type="image/png")
            pass_artifacts = {"beauty": artifact.to_dict()}
            for pass_name, relative_path in result.get("passes", {}).items():
                if pass_name == "beauty":
                    continue
                media_type = (
                    "image/x-exr" if str(relative_path).lower().endswith(".exr") else "image/png"
                )
                pass_artifacts[pass_name] = self.artifacts.ingest_file(
                    self.project.root / relative_path, media_type=media_type
                ).to_dict()
            renders.append(
                {
                    "reference_id": solution["reference_id"],
                    "camera_solution_id": camera_record["id"],
                    "relative_path": result["render_path"],
                    "artifact": artifact.to_dict(),
                    "pass_artifacts": pass_artifacts,
                    "render": result,
                    "crop_roi_px": result.get("crop_roi_px"),
                }
            )
        render_config = {
            "maximum_dimension": maximum_dimension,
            "review_lighting": True,
            "review_exposure": -0.5,
            "passes": selected_passes,
            "camera_count": len(cameras),
            "camera_solution_id": camera_record["id"],
            "framing_authority": "immutable exact camera state; scene bounds prohibited",
            "locality": {
                "reference_ids": sorted(requested),
                "regions_by_reference": regions_by_reference,
                "full_project_recompute": not bool(requested or regions_by_reference),
            },
        }
        render_outputs = [
            {
                "reference_id": item["reference_id"],
                "artifact_digest": item["artifact"]["digest"],
                "pass_artifact_digests": {
                    name: artifact["digest"] for name, artifact in item["pass_artifacts"].items()
                },
                "relative_path": item["relative_path"],
                "width": item["render"]["width"],
                "height": item["render"]["height"],
                "crop_roi_px": item.get("crop_roi_px"),
                "object_ids": item["render"].get("object_ids", {}),
                "component_ids": item["render"].get("component_ids", {}),
                "feature_ids": item["render"].get("feature_ids", {}),
                "id_pass_policy": item["render"].get("id_pass_policy", {}),
                "render_diagnostics": item["render"].get("render_diagnostics", {}),
                "validation_policy": item["render"].get("validation_policy", {}),
            }
            for item in renders
        ]
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO render_runs"
                "(id,scene_id,camera_solution_id,config_json,outputs_json,created_at) "
                "VALUES(?,?,?,?,?,?)",
                (
                    render_run_id,
                    scene["id"],
                    camera_record["id"],
                    json.dumps(render_config),
                    json.dumps(render_outputs),
                    utc_now(),
                ),
            )
        return {
            "id": render_run_id,
            "scene_id": scene["id"],
            "camera_solution_id": camera_record["id"],
            "configuration": render_config,
            "renders": renders,
        }

    def export_scene(
        self,
        *,
        scene_id: str | None = None,
        output_name: str = "model.glb",
        job_id: str | None = None,
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        name = safe_filename(output_name)
        if not name.lower().endswith(".glb"):
            name += ".glb"
        output = self.project.root / "exports" / name
        result = BlenderRunner(self.project).run(
            "export_glb",
            Path(scene["absolute_path"]),
            {"output_path": str(output)},
            job_id=job_id,
            timeout_seconds=900,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        artifact = self.artifacts.ingest_file(output, media_type="model/gltf-binary")
        export_id = str(uuid.uuid4())
        export_config = {"output_name": name, "format": "glb"}
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO exports"
                "(id,scene_id,artifact_digest,format,relative_path,config_json,worker_json,"
                "created_at) "
                "VALUES(?,?,?,?,?,?,?,?)",
                (
                    export_id,
                    scene["id"],
                    artifact.digest,
                    "glb",
                    result["export_path"],
                    json.dumps(export_config),
                    json.dumps(result.get("worker", {})),
                    utc_now(),
                ),
            )
        return {
            **result,
            "id": export_id,
            "scene_id": scene["id"],
            "configuration": export_config,
            "artifact": artifact.to_dict(),
        }

    def export_blend(
        self,
        *,
        scene_id: str | None = None,
        output_name: str = "model.blend",
        job_id: str | None = None,
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        name = safe_filename(output_name)
        if not name.lower().endswith(".blend"):
            name += ".blend"
        output = self.project.root / "exports" / name
        result = BlenderRunner(self.project).run(
            "export_blend",
            Path(scene["absolute_path"]),
            {"output_path": str(output)},
            job_id=job_id,
            timeout_seconds=900,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        artifact = self.artifacts.ingest_file(output, media_type="application/x-blender")
        export_id = str(uuid.uuid4())
        export_config = {"output_name": name, "format": "blend"}
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO exports"
                "(id,scene_id,artifact_digest,format,relative_path,config_json,worker_json,"
                "created_at) VALUES(?,?,?,?,?,?,?,?)",
                (
                    export_id,
                    scene["id"],
                    artifact.digest,
                    "blend",
                    str(output.relative_to(self.project.root)),
                    json.dumps(export_config),
                    json.dumps(result.get("worker", {})),
                    utc_now(),
                ),
            )
        return {
            **result,
            "id": export_id,
            "scene_id": scene["id"],
            "export_path": str(output.relative_to(self.project.root)),
            "configuration": export_config,
            "artifact": artifact.to_dict(),
        }

    def generate_lod(
        self,
        *,
        scene_id: str | None = None,
        ratio: float = 0.5,
        objects: list[str] | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        scene = SceneStore(self.project).get(scene_id)
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = self.project.root / "scene" / "checkpoints" / f"lod-{token}.blend"
        worker = BlenderRunner(self.project).run(
            "generate_lod",
            Path(scene["absolute_path"]),
            {"output_path": str(output), "ratio": ratio, "objects": objects or []},
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"lod-{ratio:.3f}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        if not audit["audit"]["valid"]:
            raise RuntimeError("LOD checkpoint failed the authoritative scene audit")
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "ratio": ratio,
            "objects": objects or [],
            "worker": worker,
            "audit": audit,
            "acceptance": {
                "accepted": False,
                "reason": "LOD checkpoint requires visual review before becoming authoritative",
            },
        }

    def prepare_asset(
        self,
        *,
        targets: list[dict[str, Any]],
        scene_id: str | None = None,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create, structurally validate, and reimport one bounded production candidate."""

        if not isinstance(targets, list):
            raise TypeError("asset preparation targets must be a list")
        scene = SceneStore(self.project).get(scene_id)
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"asset-preparation-{token}.blend"
        )
        glb_output = self.project.root / "exports" / f"asset-preparation-{token}.glb"
        worker = BlenderRunner(self.project).run(
            "prepare_asset",
            Path(scene["absolute_path"]),
            {
                "output_path": str(output),
                "glb_output_path": str(glb_output),
                "targets": targets,
            },
            job_id=job_id,
            timeout_seconds=1800,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"asset-preparation-{token}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        structural_validation = GlbValidator().validate(
            glb_output,
            required_node_names=worker["required_prepared_nodes"],
        ).to_dict()
        if not structural_validation["valid"]:
            raise RuntimeError("prepared GLB failed structural validation")

        reimport_output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"asset-preparation-{token}-glb-reimport.blend"
        )
        reimport_worker = BlenderRunner(self.project).run(
            "import_asset",
            self.project.root,
            {
                "source_path": str(glb_output),
                "output_path": str(reimport_output),
            },
            job_id=f"{job_id}-glb-reimport" if job_id else None,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        reimported_scene = SceneStore(self.project).register_generated(
            reimport_output,
            original_name=f"asset-preparation-{token}-glb-reimport.blend",
        )
        reimport_audit = self.audit_scene(
            reimported_scene["id"],
            job_id=f"{job_id}-glb-reimport-audit" if job_id else None,
        )
        if not audit["audit"]["valid"] or not reimport_audit["audit"]["valid"]:
            raise RuntimeError("prepared BLEND or GLB reimport failed scene audit")
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "worker": worker,
            "audit": audit,
            "glb": {
                "relative_path": str(glb_output.relative_to(self.project.root)),
                "artifact": self.artifacts.ingest_file(
                    glb_output, media_type="model/gltf-binary"
                ).to_dict(),
                "validation": structural_validation,
            },
            "glb_reimport": {
                "scene": reimported_scene,
                "worker": reimport_worker,
                "audit": reimport_audit,
            },
            "acceptance": {
                "accepted": False,
                "reason": (
                    "Execution, editability, structural GLB, and reimport gates passed; "
                    "reference-specific visual and deformation review remain required."
                ),
            },
        }

    def repair_degenerate_geometry_candidate(
        self,
        *,
        scene_id: str,
        object_name: str,
        expected_degenerate_faces: int,
        area_epsilon: float = 1e-14,
        merge_distance: float = 1e-10,
        job_id: str | None = None,
    ) -> dict[str, Any]:
        """Create an isolated, unaccepted candidate with guarded topology repair."""

        scene = SceneStore(self.project).get(scene_id)
        token = safe_filename(job_id or str(uuid.uuid4()))
        output = (
            self.project.root
            / "scene"
            / "checkpoints"
            / f"degenerate-repair-{token}.blend"
        )
        worker = BlenderRunner(self.project).run(
            "repair_degenerate_geometry_candidate",
            Path(scene["absolute_path"]),
            {
                "output_path": str(output),
                "object_name": object_name,
                "expected_degenerate_faces": expected_degenerate_faces,
                "area_epsilon": area_epsilon,
                "merge_distance": merge_distance,
            },
            job_id=job_id,
            timeout_seconds=1200,
            cancelled=(lambda: self.project.cancellation_requested(job_id)) if job_id else None,
        )
        generated_scene = SceneStore(self.project).register_generated(
            output, original_name=f"degenerate-repair-{object_name}.blend"
        )
        audit = self.audit_scene(
            generated_scene["id"], job_id=f"{job_id}-audit" if job_id else None
        )
        return {
            "source_scene_id": scene["id"],
            "generated_scene": generated_scene,
            "object_name": object_name,
            "worker": worker,
            "audit": audit,
            "acceptance": {
                "accepted": False,
                "reason": (
                    "isolated topology candidate requires named review and all-view "
                    "visual regression evidence before promotion"
                ),
            },
        }

    def compare_views(self, renders: list[dict[str, Any]]) -> dict[str, Any]:
        references = {item["id"]: item for item in ReferenceIngestor(self.project).list()}
        masks = ReferenceMaskStore(self.project)
        comparisons: list[dict[str, Any]] = []
        for render in renders:
            reference = references[render["reference_id"]]
            reference_path = self.project.root / reference["relative_path"]
            render_path = self.project.root / render["relative_path"]
            comparison_id = str(uuid.uuid4())
            residual_path = self.project.root / "comparisons" / f"{comparison_id}.png"
            reviewed_mask = masks.latest(reference["id"])
            comparison_reference_path = reference_path
            reviewed_mask_path = (
                masks.artifacts.path_for(reviewed_mask["artifact_digest"])
                if reviewed_mask
                else None
            )
            crop_artifacts: dict[str, dict[str, Any]] = {}
            crop_roi = render.get("crop_roi_px")
            if crop_roi:
                render_record = render.get("render", {})
                full_width = int(render_record.get("full_frame_width", 0))
                full_height = int(render_record.get("full_frame_height", 0))
                if full_width <= 0 or full_height <= 0:
                    raise ValueError("local render comparison requires full-frame dimensions")
                box = (
                    int(crop_roi["x"]),
                    int(crop_roi["y"]),
                    int(crop_roi["x"]) + int(crop_roi["width"]),
                    int(crop_roi["y"]) + int(crop_roi["height"]),
                )
                comparison_reference_path = (
                    self.project.root
                    / "comparisons"
                    / f"{comparison_id}.reference-crop.png"
                )
                with Image.open(reference_path) as source_image:
                    prepared = ImageOps.exif_transpose(source_image).convert("RGBA").resize(
                        (full_width, full_height), Image.Resampling.LANCZOS
                    )
                    prepared.crop(box).save(comparison_reference_path, format="PNG")
                crop_artifacts["reference"] = self.artifacts.ingest_file(
                    comparison_reference_path, media_type="image/png"
                ).to_dict()
                if reviewed_mask_path is not None:
                    cropped_mask_path = (
                        self.project.root
                        / "comparisons"
                        / f"{comparison_id}.reviewed-mask-crop.png"
                    )
                    with Image.open(reviewed_mask_path) as source_mask:
                        prepared_mask = ImageOps.exif_transpose(source_mask).convert("L").resize(
                            (full_width, full_height), Image.Resampling.NEAREST
                        )
                        prepared_mask.crop(box).save(cropped_mask_path, format="PNG")
                    reviewed_mask_path = cropped_mask_path
                    crop_artifacts["reviewed_mask"] = self.artifacts.ingest_file(
                        cropped_mask_path, media_type="image/png"
                    ).to_dict()
            metrics = compare_silhouettes(
                comparison_reference_path,
                render_path,
                residual_path,
                reviewed_mask_path=reviewed_mask_path,
                reviewed_mask_record=reviewed_mask,
                automatic_segmentation_maximum_dimension=(
                    BOUNDED_AUTOMATIC_SEGMENTATION_MAXIMUM_DIMENSION
                ),
            )
            if crop_roi:
                metrics["locality"] = {
                    "crop_roi_px": crop_roi,
                    "full_frame_size": [full_width, full_height],
                    "crop_artifact_digests": {
                        name: artifact["digest"] for name, artifact in crop_artifacts.items()
                    },
                    "full_frame_metric_recomputed": False,
                }
            residual = self.artifacts.ingest_file(residual_path, media_type="image/png")
            stored = ComparisonStore(self.project).record(
                comparison_id,
                reference_id=reference["id"],
                render_digest=render["artifact"]["digest"],
                residual_digest=residual.digest,
                metrics=metrics,
                engine="compare_silhouettes_v3",
            )
            comparisons.append(
                {
                    "id": comparison_id,
                    "reference_id": reference["id"],
                    "render_digest": render["artifact"]["digest"],
                    "residual": residual.to_dict(),
                    "metrics": metrics,
                    "receipt": stored["receipt_artifact"],
                }
            )
        return {"comparisons": comparisons}

    def coverage_report(self) -> dict[str, Any]:
        references = [
            item
            for item in ReferenceIngestor(self.project).list()
            if item["media_type"].startswith("image/")
        ]
        with self.project.connection() as connection:
            compared = {
                row[0]
                for row in connection.execute(
                    "SELECT DISTINCT reference_id FROM comparisons"
                ).fetchall()
            }
            feature_count = connection.execute("SELECT COUNT(*) FROM features").fetchone()[0]
            camera_rows = connection.execute(
                "SELECT solution_json FROM camera_solutions ORDER BY created_at DESC LIMIT 1"
            ).fetchone()
        camera_document = json.loads(camera_rows[0]) if camera_rows else {}
        cameras = (
            camera_document.get("cameras", [])
            if isinstance(camera_document, dict)
            else camera_document
        )
        classes: dict[str, int] = {}
        for camera in cameras:
            key = camera["registration_class"]
            classes[key] = classes.get(key, 0) + 1
        report_id = str(uuid.uuid4())
        report = {
            "schema_version": 1,
            "id": report_id,
            "created_at": utc_now(),
            "reference_count": len(references),
            "compared_reference_count": len(compared),
            "comparison_coverage": round(len(compared) / len(references), 8) if references else 0.0,
            "uncompared_reference_ids": [
                item["id"] for item in references if item["id"] not in compared
            ],
            "viewpoints": sorted({item["viewpoint_label"] or "unlabeled" for item in references}),
            "camera_registration_classes": classes,
            "technical_feature_count": feature_count,
            "uncertainty": {
                "camera": "high"
                if classes.get(RegistrationClass.APPROXIMATE_VISUAL.value)
                else "unclassified",
                "geometry": "unclassified",
                "feature_coverage": "unseen" if feature_count == 0 else "partial",
            },
            "next_best_view": (
                "label and capture front, rear, left, right, and top views with overlap"
            )
            if len({item["viewpoint_label"] for item in references if item["viewpoint_label"]}) < 5
            else None,
        }
        relative = Path("comparisons") / f"coverage-{report_id}.json"
        atomic_write_json(self.project.root / relative, report)
        artifact = self.artifacts.ingest_file(
            self.project.root / relative, media_type="application/vnd.bvmcp.coverage+json"
        )
        with self.project.connection() as connection:
            connection.execute(
                "INSERT INTO coverage_reports(id,digest,report_json,created_at) VALUES(?,?,?,?)",
                (report_id, artifact.digest, json.dumps(report), report["created_at"]),
            )
        return {**report, "artifact": artifact.to_dict(), "path": str(relative)}

    def export_receipt(self) -> dict[str, Any]:
        return export_receipt(self.project)
