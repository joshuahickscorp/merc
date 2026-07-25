from __future__ import annotations

import hashlib
import json
import os
from contextlib import nullcontext
from importlib import metadata
from pathlib import Path
from typing import Any

from blender_vision.artifacts.store import ArtifactStore
from blender_vision.core.errors import BackendUnavailable
from blender_vision.core.models import EvidenceClass, RegistrationClass
from blender_vision.core.util import atomic_write_json
from blender_vision.evidence.references import ReferenceIngestor
from blender_vision.projects.store import ProjectStore
from blender_vision.vision.base import GeometryEvidence
from blender_vision.vision.store import GeometryEvidenceStore


def _source_tree_digest(module_file: str) -> str:
    root = Path(module_file).resolve().parent
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*.py")):
        digest.update(str(path.relative_to(root)).encode())
        digest.update(b"\0")
        digest.update(path.read_bytes())
        digest.update(b"\0")
    return digest.hexdigest()


class VGGTAdapter:
    """Optional, local-checkpoint-only adapter for the upstream VGGT package."""

    def __init__(self, project: ProjectStore):
        self.project = project
        self.artifacts = ArtifactStore(project)

    def _installation(self, installation_id: str) -> dict[str, Any]:
        if not installation_id:
            raise BackendUnavailable(
                "VGGT requires an explicit governed model_installation_id; approve, manually "
                "acquire, digest-verify, and import the checkpoint first"
            )
        with self.project.connection() as connection:
            row = connection.execute(
                "SELECT i.id,i.artifact_digest,i.revision,i.installed_at,a.name,a.source_url,"
                "a.license_json,a.approved_by,a.reason,a.status "
                "FROM model_installations i JOIN model_approvals a ON a.id=i.approval_id "
                "WHERE i.id=?",
                (installation_id,),
            ).fetchone()
        if row is None:
            raise BackendUnavailable(f"unknown VGGT model installation: {installation_id}")
        value = dict(row)
        value["license"] = json.loads(value.pop("license_json"))
        return value

    @staticmethod
    def _runtime() -> tuple[Any, Any, Any, Any, Any]:
        os.environ.setdefault("HF_HUB_OFFLINE", "1")
        os.environ.setdefault("TRANSFORMERS_OFFLINE", "1")
        try:
            import numpy as np
            import torch
            import vggt
            from vggt.models.vggt import VGGT
            from vggt.utils.load_fn import load_and_preprocess_images
            from vggt.utils.pose_enc import pose_encoding_to_extri_intri
        except ImportError as error:
            raise BackendUnavailable(
                "VGGT optional runtime is unavailable; install the operator-reviewed upstream "
                "vggt package plus torch/numpy on a vision worker"
            ) from error
        helpers = (load_and_preprocess_images, pose_encoding_to_extri_intri)
        return np, torch, vggt, VGGT, helpers

    @staticmethod
    def _device(torch: Any, requested: str) -> str:
        if requested not in {"auto", "cuda", "mps", "cpu"}:
            raise ValueError("VGGT device must be auto, cuda, mps, or cpu")
        if requested == "auto":
            if torch.cuda.is_available():
                return "cuda"
            if getattr(torch.backends, "mps", None) and torch.backends.mps.is_available():
                return "mps"
            return "cpu"
        if requested == "cuda" and not torch.cuda.is_available():
            raise BackendUnavailable("VGGT CUDA execution was requested but CUDA is unavailable")
        if requested == "mps" and not (
            getattr(torch.backends, "mps", None) and torch.backends.mps.is_available()
        ):
            raise BackendUnavailable("VGGT MPS execution was requested but MPS is unavailable")
        return requested

    def run(self, backend: str, configuration: dict[str, Any]) -> dict[str, Any]:
        installation = self._installation(str(configuration.get("model_installation_id", "")))
        license_record = installation["license"]
        commercial_eligible = bool(
            license_record.get("commercial_use") is True
            and not license_record.get("research_only", False)
        )
        if backend == "vggt-commercial" and not commercial_eligible:
            raise BackendUnavailable(
                "the selected VGGT checkpoint is not approved for commercial authority"
            )
        references = [
            item
            for item in ReferenceIngestor(self.project).list()
            if item["media_type"].startswith("image/") and item["quality"].get("decode_ok")
        ]
        if not references:
            raise ValueError("VGGT requires at least one decodable image reference")
        maximum_images = int(configuration.get("maximum_images", 32))
        if not 1 <= maximum_images <= 200:
            raise ValueError("VGGT maximum_images must be between 1 and 200")
        references = references[:maximum_images]
        np, torch, vggt_module, VGGT, helpers = self._runtime()
        load_and_preprocess_images, pose_encoding_to_extri_intri = helpers
        device = self._device(torch, str(configuration.get("device", "auto")))
        checkpoint = self.artifacts.path_for(installation["artifact_digest"])
        if not checkpoint.is_file():
            raise FileNotFoundError(checkpoint)
        try:
            state = torch.load(checkpoint, map_location="cpu", weights_only=True)
        except TypeError as error:
            raise BackendUnavailable(
                "the installed torch runtime lacks safe weights_only checkpoint loading"
            ) from error
        if isinstance(state, dict) and "state_dict" in state:
            state = state["state_dict"]
        model = VGGT()
        model.load_state_dict(state)
        model.eval().to(device)
        image_paths = [str(self.project.root / item["relative_path"]) for item in references]
        images = load_and_preprocess_images(image_paths).to(device)
        if device == "cuda":
            dtype = (
                torch.bfloat16
                if torch.cuda.get_device_capability()[0] >= 8
                else torch.float16
            )
            autocast = torch.autocast(device_type="cuda", dtype=dtype)
            precision = str(dtype).split(".")[-1]
        elif device == "mps":
            autocast = torch.autocast(device_type="mps", dtype=torch.float16)
            precision = "float16"
        else:
            autocast = nullcontext()
            precision = "float32"
        with torch.inference_mode(), autocast:
            predictions = model(images)
            extrinsic, intrinsic = pose_encoding_to_extri_intri(
                predictions["pose_enc"], predictions["images"].shape[-2:]
            )

        def array(value: Any) -> Any:
            result = value.detach().float().cpu().numpy()
            return result[0] if result.ndim >= 1 and result.shape[0] == 1 else result

        extrinsic_array = array(extrinsic)
        intrinsic_array = array(intrinsic)
        depth = array(predictions["depth"])
        depth_confidence = array(predictions["depth_conf"])
        points = array(predictions["world_points"])
        point_confidence = array(predictions["world_points_conf"])
        processed_height, processed_width = predictions["images"].shape[-2:]
        output = self.project.root / "geometry" / "vggt" / installation["id"]
        output.mkdir(parents=True, exist_ok=True)
        depth_path = output / "depth.npz"
        point_path = output / "world-points.npz"
        confidence_path = output / "confidence.npz"
        reference_ids = np.asarray([item["id"] for item in references], dtype="U64")
        np.savez_compressed(depth_path, depth=depth.astype(np.float32), reference_ids=reference_ids)
        np.savez_compressed(
            point_path, world_points=points.astype(np.float32), reference_ids=reference_ids
        )
        np.savez_compressed(
            confidence_path,
            depth_confidence=depth_confidence.astype(np.float32),
            point_confidence=point_confidence.astype(np.float32),
            reference_ids=reference_ids,
        )
        depth_artifact = self.artifacts.ingest_file(
            depth_path, media_type="application/vnd.bvmcp.depth+npz"
        )
        point_artifact = self.artifacts.ingest_file(
            point_path, media_type="application/vnd.bvmcp.point-map+npz"
        )
        confidence_artifact = self.artifacts.ingest_file(
            confidence_path, media_type="application/vnd.bvmcp.confidence+npz"
        )
        camera_intrinsics = []
        camera_extrinsics = []
        for index, reference in enumerate(references):
            world_to_camera = np.eye(4, dtype=np.float64)
            world_to_camera[:3, :4] = extrinsic_array[index]
            world_from_camera = np.linalg.inv(world_to_camera)
            calibration = intrinsic_array[index]
            camera_intrinsics.append(
                {
                    "reference_id": reference["id"],
                    "model": "PINHOLE",
                    "width": int(processed_width),
                    "height": int(processed_height),
                    "intrinsics": {
                        "fx": float(calibration[0, 0]),
                        "fy": float(calibration[1, 1]),
                        "cx": float(calibration[0, 2]),
                        "cy": float(calibration[1, 2]),
                    },
                }
            )
            camera_extrinsics.append(
                {
                    "reference_id": reference["id"],
                    "world_from_camera": world_from_camera.tolist(),
                    "registration_class": RegistrationClass.APPROXIMATE_VISUAL.value,
                    "confidence": float(np.mean(depth_confidence[index])),
                }
            )
        try:
            package_version = metadata.version("vggt")
        except metadata.PackageNotFoundError:
            package_version = str(getattr(vggt_module, "__version__", "source-tree"))
        code_digest = _source_tree_digest(vggt_module.__file__)
        camera_path = output / "cameras.json"
        atomic_write_json(
            camera_path,
            {
                "schema_version": 1,
                "camera_intrinsics": camera_intrinsics,
                "camera_extrinsics": camera_extrinsics,
                "source_convention": "VGGT OpenCV camera-from-world",
            },
        )
        camera_artifact = self.artifacts.ingest_file(
            camera_path, media_type="application/vnd.bvmcp.camera-evidence+json"
        )
        evidence = GeometryEvidence(
            camera_intrinsics=camera_intrinsics,
            camera_extrinsics=camera_extrinsics,
            depth_artifacts=[depth_artifact.digest],
            point_artifacts=[point_artifact.digest],
            confidence_artifacts=[confidence_artifact.digest],
            diagnostics={
                "model_installation_id": installation["id"],
                "checkpoint_digest": installation["artifact_digest"],
                "checkpoint_revision": installation["revision"],
                "backend_package_version": package_version,
                "backend_source_tree_sha256": code_digest,
                "camera_artifact_digest": camera_artifact.digest,
                "reference_ids": [item["id"] for item in references],
                "device": device,
                "precision": precision,
                "network_used": False,
            },
            source_frame="vggt_world_opencv_camera_from_world",
            transform_to_canonical=None,
            scale_factor=None,
            uncertainty={
                "scale": "unresolved_without_authoritative_alignment",
                "camera": "learned_geometry_initialization_not_metric_authority",
                "depth_confidence_mean": float(np.mean(depth_confidence)),
                "point_confidence_mean": float(np.mean(point_confidence)),
            },
        )
        run_backend = "vggt-commercial" if commercial_eligible else "vggt-original-research"
        evidence_class = (
            EvidenceClass.MULTI_VIEW_OBSERVED
            if len(references) > 1
            else EvidenceClass.SINGLE_VIEW_OBSERVED
        )
        return GeometryEvidenceStore(self.project).create(
            run_backend,
            f"{package_version}+{installation['revision']}",
            evidence,
            evidence_class=evidence_class,
            configuration={
                **configuration,
                "model_installation_id": installation["id"],
                "network_used": False,
            },
            license_record={
                **license_record,
                "checkpoint_digest": installation["artifact_digest"],
                "checkpoint_revision": installation["revision"],
                "source_url": installation["source_url"],
                "approved_by": installation["approved_by"],
                "approval_reason": installation["reason"],
            },
            commercial_eligible=commercial_eligible,
        )
